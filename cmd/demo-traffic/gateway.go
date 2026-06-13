// Gateway target for the demo-traffic generator.
//
// Unlike the default Loki target (which synthesizes response-event log
// lines and pushes them straight to Loki), the gateway target sends real
// chat-completion requests through the AIQG strict gateway
// (llm-router-aiqg). The gateway then emits genuine response events down
// the full Kafka→Spark→TimescaleDB path (and Loki), so the per-agent /
// per-flow TimescaleDB rollups behind /metrics/agents and /flows light up
// with attributed traffic — which the Loki-synthesis path can't do (it
// never touches Kafka).
//
// Attribution headers (TAS-Agent-*, TAS-Flow-Id / TAS-Conversation-Id,
// baggage user.id) are derived from the same agent personas the synth
// path uses, so the two targets tell the same demo story.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// gatewayClient posts chat completions to {URL}/v1/chat/completions.
type gatewayClient struct {
	URL       string
	Token     string // TAS-Auth gateway token (tas_qg_live_...)
	Model     string
	MaxTokens int
	HTTP      *http.Client
}

func newGatewayClient(url, token, model string, maxTokens int, insecure bool) *gatewayClient {
	hc := &http.Client{Timeout: 60 * time.Second}
	if insecure {
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return &gatewayClient{URL: strings.TrimRight(url, "/"), Token: token, Model: model, MaxTokens: maxTokens, HTTP: hc}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
}

// send posts one chat completion with the given attribution headers and
// returns the HTTP status code (0 on transport error). The response body
// is drained and discarded — we only care that the gateway emitted an event.
func (c *gatewayClient) send(ctx context.Context, headers map[string]string, prompt string) (int, error) {
	body, err := json.Marshal(chatRequest{
		Model:     c.Model,
		Messages:  []chatMessage{{Role: "user", Content: prompt}},
		MaxTokens: c.MaxTokens,
	})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("TAS-Auth", c.Token)
	// Path A (strict) requires an Authorization header to be present; the
	// gateway uses its own server-side upstream creds, so the dummy
	// upstream key still yields a real (tiny) vendor call.
	req.Header.Set("Authorization", "Bearer demo-traffic")
	req.Header.Set("TAS-Upstream-Authorization", "Bearer demo-traffic")
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// prompts per CLEAR workflow type. Kept short (max-tokens is tiny) — the
// point is attribution + workflow classification, not content.
var workflowPrompts = map[string][]string{
	"single_turn_qa":            {"What is the capital of France?", "Define idempotency in one line.", "What is a vector database?"},
	"rag":                       {"Using the docs, what is TAS?", "Summarize the retrieved context.", "Cite a source for that claim."},
	"agentic":                   {"Plan the steps to refactor this module.", "Decide which tool to call next.", "Orchestrate retrieval then summarize."},
	"summarization":             {"Summarize: the quick brown fox jumps over the lazy dog.", "Give a one-line TL;DR of this paragraph."},
	"code_generation":           {"Write a Go function to reverse a string.", "Fix this off-by-one loop.", "Generate a SQL query for the top 5 users."},
	"classification_extraction": {"Classify the sentiment of 'I love this'.", "Extract the date from 'meet on 2026-06-13'.", "Label this ticket's intent."},
}

// compliancePrompts carry fake PII so the gateway's inbound scanners fire,
// giving the Findings panel real flagged traffic.
var compliancePrompts = []string{
	"My email is jane@example.com and my SSN is 123-45-6789 — summarize my account.",
	"Call me at 555-867-5309 about the overdue invoice for card 4111 1111 1111 1111.",
}

func pickPrompt(g rng, workflow string, complianceRate float64) string {
	if g.chance(complianceRate) {
		return compliancePrompts[g.r.Intn(len(compliancePrompts))]
	}
	ps := workflowPrompts[workflow]
	if len(ps) == 0 {
		ps = workflowPrompts["single_turn_qa"]
	}
	return ps[g.r.Intn(len(ps))]
}

func splitUsers(csv string) []string {
	var out []string
	for _, u := range strings.Split(csv, ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		out = []string{"u_demo"}
	}
	return out
}

// runGatewayPass walks every persona's flows and sends one request per
// step with attribution headers derived from the persona. Multi-turn
// personas share a conversation_id; every flow shares a flow_id.
func runGatewayPass(ctx context.Context, g rng, c *gatewayClient, users []string, flowsPerAgent int, complianceRate float64, dryRun bool) (sent, failed int) {
	for _, persona := range personas {
		agentID := agentIDFor(persona.Name)
		for f := 0; f < flowsPerAgent; f++ {
			user := users[g.r.Intn(len(users))]
			headers := map[string]string{
				"TAS-Agent-Id":      agentID,
				"TAS-Agent-Name":    persona.Name,
				"TAS-Agent-Version": persona.Version,
				"TAS-Flow-Id":       g.uuid(),
				"baggage":           "user.id=" + user,
			}
			if persona.TurnsHi > 1 {
				headers["TAS-Conversation-Id"] = g.uuid()
			}
			for _, s := range persona.BuildFlow(g) {
				select {
				case <-ctx.Done():
					return
				default:
				}
				prompt := pickPrompt(g, profileByKey[s.Profile].Workflow, complianceRate)
				if dryRun {
					fmt.Printf("POST %s/v1/chat/completions  agent=%q user=%s flow=%s prompt=%q\n",
						c.URL, persona.Name, user, headers["TAS-Flow-Id"], prompt)
					sent++
					continue
				}
				code, err := c.send(ctx, headers, prompt)
				if err != nil || code < 200 || code >= 300 {
					failed++
				} else {
					sent++
				}
			}
		}
	}
	return sent, failed
}

// runGatewayTarget handles the gateway target end-to-end: single pass, or
// loop on --interval.
func runGatewayTarget(ctx context.Context, g rng, r rates, gc *gatewayClient, users []string, flowsPerAgent int, interval time.Duration, dryRun bool) {
	fmt.Printf("demo-traffic: target=gateway url=%s model=%s agents=%d flows-per-agent=%d users=%v dry-run=%v\n",
		gc.URL, gc.Model, len(personas), flowsPerAgent, users, dryRun)
	pass := func() {
		sent, failed := runGatewayPass(ctx, g, gc, users, flowsPerAgent, r.Compliance, dryRun)
		fmt.Printf("gateway pass: sent=%d failed=%d\n", sent, failed)
	}
	pass()
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nstopped.")
			return
		case <-ticker.C:
			pass()
		}
	}
}
