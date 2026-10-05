// Command judge-score runs the SHIPPING LLM-as-judge rubric over
// newline-delimited (prompt, response) pairs and writes the resulting scores as
// NDJSON.
//
// Why it exists: `pkg/aiqg/judge` only runs inside the gateway, on sampled live
// traffic, so there was no way to point the rubric at a labeled corpus and ask
// whether it agrees with anyone. That left the judged Efficacy floor shipped and
// untested — AIQG-30 says it gates nothing today, and nothing measured whether
// it *should*. Plan #19 W1 needs exactly this: score both sides of a human
// preference pair and see if the judge orders them the way people did.
//
// It takes the same shape as cmd/clear-score, which exists so the validation
// harness can bind to the real Go CLEAR scorer instead of its Python
// re-implementation (see validation/aiqg_validate/goscorer.py). The reason is
// the same: a second copy of a scoring rule drifts from the first, and then the
// measurement is of the copy.
//
// Nothing in pkg/aiqg/judge changed to make this possible. judge.Completion is
// an exported interface precisely so the judge is "unit-testable and so the
// judge model is explicit", so this command supplies a transport and gets the
// real rubric, the real parsing, and the real abstain semantics for free.
//
// Usage:
//
//	judge-score -gateway http://localhost:18086 -model gpt-4o-mini < pairs.ndjson
//
// Input, one JSON object per line:
//
//	{"id":"p1","workflow":"single_turn_qa","prompt":"…","response":"…"}
//
// Output, one per line, in input order:
//
//	{"id":"p1","overall":0.87,"dimensions":{…},"abstain":false,
//	 "rubric_version":"v1","workflow":"single_turn_qa"}
//
// A failed or abstaining judge is reported, never silently dropped: `error` is
// set on failure and `abstain` is true when the judge declined. Both are
// outcomes the caller must be able to count — an abstain rate that cannot be
// seen is AIQG-33.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/judge"
)

type request struct {
	ID       string `json:"id"`
	Workflow string `json:"workflow"`
	Prompt   string `json:"prompt"`
	Response string `json:"response"`
}

type result struct {
	ID            string             `json:"id"`
	Overall       float64            `json:"overall"`
	Dimensions    map[string]float64 `json:"dimensions,omitempty"`
	Abstain       bool               `json:"abstain"`
	RubricVersion string             `json:"rubric_version,omitempty"`
	Workflow      string             `json:"workflow,omitempty"`
	JudgeModel    string             `json:"judge_model,omitempty"`
	Err           string             `json:"error,omitempty"`
}

// gatewayCompletion is the transport half of judge.Completion: an AIQG
// chat-completion call. The judge package supplies everything else.
type gatewayCompletion struct {
	base       string
	token      string
	sourceApp  string
	httpClient *http.Client
}

func (g *gatewayCompletion) Complete(ctx context.Context, model, system, user string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model": model,
		// The judge's rubric asks for strict JSON; max_tokens is generous
		// rather than tight because a truncated reply parses as an abstain and
		// would quietly understate the judge rather than fail loudly.
		"max_tokens": 1024,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(g.base, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("TAS-Auth", g.token)
	// Declared, not inferred. This traffic IS a probe, so it says so rather
	// than relying on a source-app denylist to guess — the W3 half of Plan #19.
	req.Header.Set("TAS-Source-App", g.sourceApp)
	req.Header.Set("TAS-Synthetic", "true")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gateway %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}
	return parsed.Choices[0].Message.Content, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func main() {
	var (
		gateway     = flag.String("gateway", "", "AIQG gateway base URL (required)")
		model       = flag.String("model", "", "judge model (required)")
		token       = flag.String("token", os.Getenv("AIQG_TAS_AUTH_TOKEN"), "TAS-Auth token; defaults to $AIQG_TAS_AUTH_TOKEN")
		sourceApp   = flag.String("source-app", "aiqg-judge-eval", "TAS-Source-App for the judge calls")
		concurrency = flag.Int("concurrency", 4, "in-flight judge calls")
		timeout     = flag.Duration("timeout", 90*time.Second, "per-call timeout")
	)
	flag.Parse()

	if *gateway == "" || *model == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "judge-score: -gateway, -model and a token are all required")
		flag.Usage()
		os.Exit(2)
	}
	if *concurrency < 1 {
		*concurrency = 1
	}

	j := &judge.Judge{
		LLM: &gatewayCompletion{
			base:       *gateway,
			token:      *token,
			sourceApp:  *sourceApp,
			httpClient: &http.Client{Timeout: *timeout},
		},
		Model: *model,
	}

	// Read everything first so output can be written in input order: an
	// agreement measurement pairs rows by index, and a reordered stream is a
	// silently wrong answer rather than an error.
	var reqs []request
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var r request
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			fmt.Fprintf(os.Stderr, "judge-score: bad input line: %v\n", err)
			os.Exit(1)
		}
		reqs = append(reqs, r)
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "judge-score: read stdin: %v\n", err)
		os.Exit(1)
	}

	results := make([]result, len(reqs))
	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			r := reqs[i]
			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			defer cancel()

			out := result{ID: r.ID, Workflow: r.Workflow, JudgeModel: *model}
			score, err := j.Score(ctx, r.Workflow, r.Prompt, r.Response)
			if err != nil {
				out.Err = err.Error()
				results[i] = out
				return
			}
			out.Overall = score.Overall
			out.Dimensions = score.Dimensions
			out.Abstain = score.Abstain
			out.RubricVersion = score.RubricVersion
			if score.Workflow != "" {
				out.Workflow = score.Workflow
			}
			results[i] = out
		}(i)
	}
	wg.Wait()

	enc := json.NewEncoder(os.Stdout)
	for _, r := range results {
		if err := enc.Encode(r); err != nil {
			fmt.Fprintf(os.Stderr, "judge-score: write: %v\n", err)
			os.Exit(1)
		}
	}
}
