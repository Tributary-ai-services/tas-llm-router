// Command clear-score wraps the shipping CLEAR scorer (pkg/clear) as a thin
// JSONL filter so external tools — notably the AIQG validation harness — can
// validate the ACTUAL Go implementation instead of a re-implementation that
// could silently drift from it.
//
// It reads one JSON object per line on stdin and writes one JSON object per line
// on stdout, in the same order. Input fields mirror the subset of clear.Input a
// caller can supply from an observed response:
//
//	{"finish_reason":"stop","http_status":200,"workflow":"code_generation",
//	 "vendor":"openai","model":"gpt-4o-mini"}
//
// Output is the pkg/clear.Scores JSON (fields omitted when the scorer returns
// nil), e.g. {"efficacy":100,"composite":100,"weights_applied":"..."}.
//
// http_status is passed through verbatim: 0 means "gateway never wrote a
// response" (clear treats that as no Efficacy signal), so a caller scoring a
// real completion must send the real status (typically 200).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/tributary-ai/llm-router-waf/pkg/clear"
)

type input struct {
	FinishReason string `json:"finish_reason"`
	HTTPStatus   int    `json:"http_status"`
	Workflow     string `json:"workflow,omitempty"`
	Vendor       string `json:"vendor,omitempty"`
	Model        string `json:"model,omitempty"`
}

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var in input
		if err := json.Unmarshal(line, &in); err != nil {
			fmt.Fprintf(os.Stderr, "clear-score: bad input line: %v\n", err)
			os.Exit(1)
		}
		scores := clear.Compute(clear.Input{
			FinishReason: in.FinishReason,
			HTTPStatus:   in.HTTPStatus,
			Workflow:     in.Workflow,
			Vendor:       in.Vendor,
			Model:        in.Model,
		})
		b, err := json.Marshal(scores)
		if err != nil {
			fmt.Fprintf(os.Stderr, "clear-score: marshal: %v\n", err)
			os.Exit(1)
		}
		out.Write(b)
		out.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "clear-score: read error: %v\n", err)
		os.Exit(1)
	}
}
