package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// fingerprint-eval target: validation harness for the inferred attribution
// tiers (docs/AIQG-AGENT-FLOW-ATTRIBUTION.md §B / AIQG-EXPERIMENTS-RUNNER §6).
// It sends UNTAGGED traffic (no TAS-Agent/Flow/Conversation headers, no
// baggage) from a set of personas with DISTINCT toolsets + config, so the only
// thing the gateway can attribute on is the fingerprinted tier. It prints one
// `event_id,persona` ground-truth line per request to stdout — join those
// against aiqg.event_metrics to score how cleanly the inferred
// agent_surrogate_id recovers the known personas (purity / no collision).

type fpTool struct {
	Type     string   `json:"type"`
	Function fpToolFn `json:"function"`
}
type fpToolFn struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// mkTool builds a function tool with an object schema over the named string
// params — distinct names/params give each persona a distinct toolset hash.
func mkTool(name, desc string, params ...string) fpTool {
	props := map[string]interface{}{}
	for _, p := range params {
		props[p] = map[string]interface{}{"type": "string"}
	}
	return fpTool{Type: "function", Function: fpToolFn{
		Name: name, Description: desc,
		Parameters: map[string]interface{}{"type": "object", "properties": props, "required": params},
	}}
}

// fpPersona is a synthetic programmatic agent with a stable signature. The
// toolset is the dominant fingerprint signal; temperature is a config
// tie-breaker. All use one model so the toolset (not the model) is what must
// separate them — the harder, more honest test.
type fpPersona struct {
	Name        string
	Model       string
	Temperature *float64
	Tools       []fpTool
}

func tempPtr(f float64) *float64 { return &f }

var fpPersonas = []fpPersona{
	{Name: "weather-bot", Model: "gpt-4o-mini", Tools: []fpTool{
		mkTool("get_weather", "current weather", "location"),
		mkTool("get_forecast", "n-day forecast", "location", "days"),
	}},
	{Name: "invoice-agent", Model: "gpt-4o-mini", Tools: []fpTool{
		mkTool("lookup_invoice", "fetch an invoice", "invoice_id"),
		mkTool("create_invoice", "create an invoice", "customer", "amount"),
	}},
	{Name: "search-assistant", Model: "gpt-4o-mini", Tools: []fpTool{
		mkTool("web_search", "search the web", "query"),
		mkTool("fetch_url", "fetch a page", "url"),
	}},
	{Name: "code-helper", Model: "gpt-4o-mini", Temperature: tempPtr(0.2), Tools: []fpTool{
		mkTool("run_tests", "run the test suite", "path"),
	}},
	{Name: "triage-bot", Model: "gpt-4o-mini", Tools: []fpTool{
		mkTool("classify_ticket", "label a ticket", "text"),
		mkTool("assign_owner", "route to a team", "team"),
	}},
}

type fpRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature *float64      `json:"temperature,omitempty"`
	Tools       []fpTool      `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
}

// sendFP posts one UNTAGGED tool-bearing request and returns the
// TAS-Response-Event-Id from the response header.
func (c *gatewayClient) sendFP(ctx context.Context, p fpPersona) (string, int, error) {
	body, err := json.Marshal(fpRequest{
		Model:       p.Model,
		Messages:    []chatMessage{{Role: "user", Content: "Say hello in one short sentence."}},
		MaxTokens:   12,
		Temperature: p.Temperature,
		Tools:       p.Tools,
		ToolChoice:  "auto", // fingerprint is on the DECLARED tools, not whether one fires
	})
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("TAS-Auth", c.Token)
	req.Header.Set("Authorization", "Bearer demo-traffic")
	req.Header.Set("TAS-Upstream-Authorization", "Bearer demo-traffic")
	// NO attribution headers — the gateway must infer identity.
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.Header.Get("TAS-Response-Event-Id"), resp.StatusCode, nil
}

// runFingerprintEval sends perPersona requests per persona, untagged, and
// prints `event_id,persona` ground-truth lines to stdout for offline scoring.
func runFingerprintEval(ctx context.Context, gc *gatewayClient, perPersona int, dryRun bool) {
	fmt.Printf("# fingerprint-eval: %d personas × %d requests untagged → %s\n", len(fpPersonas), perPersona, gc.URL)
	fmt.Println("event_id,persona")
	sent, failed := 0, 0
	for _, p := range fpPersonas {
		for i := 0; i < perPersona; i++ {
			if dryRun {
				fmt.Printf("(dry-run) %s tools=%d model=%s\n", p.Name, len(p.Tools), p.Model)
				continue
			}
			ev, code, err := gc.sendFP(ctx, p)
			if err != nil || code/100 != 2 || ev == "" {
				failed++
				continue
			}
			sent++
			fmt.Printf("%s,%s\n", ev, p.Name)
			time.Sleep(50 * time.Millisecond)
		}
	}
	fmt.Printf("# done: sent=%d failed=%d\n", sent, failed)
}
