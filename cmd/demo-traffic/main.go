// Command demo-traffic generates synthetic AIQG response-event log
// lines and pushes them to Loki so the AIQG dashboard's CLEAR, cost,
// latency, tag, and avoidable-cost panels light up with believable
// multi-agent traffic.
//
// It does NOT call any vendor LLM. Inputs are sampled from eight agent
// profiles (see profiles.go) and scored by the REAL pkg/clear scorer,
// so demo scores and dollar costs match what the live gateway would
// produce for the same inputs.
//
// One pass exercises all eight profiles plus the compliance / vague /
// hedging cross-cutting attributes, with each profile's signature
// matcher guaranteed to fire once so every panel has data after a
// single run. Pass --interval to keep emitting on a schedule.
//
// Usage:
//
//	go run ./cmd/demo-traffic                 # one pass to the default Loki, default aiqg-demo tenant
//	go run ./cmd/demo-traffic --dry-run       # print lines, don't push
//	go run ./cmd/demo-traffic --interval 30s  # loop forever, one pass every 30s
//	go run ./cmd/demo-traffic --per-profile 25
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"
)

// Defaults resolved from the aiqg-demo account Settings page
// (Account ID + Tenant ID). The dashboard injects tenant_id from the
// JWT and filters every Loki query on it, so these must match the
// account you view the dashboard as.
const (
	defaultLokiURL   = "https://loki.tas.scharber.com"
	defaultTenantID  = "a689c0b2-02ca-46d1-9916-f9a30c00222a" // aiqg-demo
	defaultAccountID = "9088f68b-1fe5-427f-bb7b-8f16fa37a23a" // aiqg-demo
)

func main() {
	var (
		lokiURL    = flag.String("loki-url", defaultLokiURL, "Loki base URL (push API at /loki/api/v1/push)")
		tenantID   = flag.String("tenant-id", defaultTenantID, "AIQG account tenant_id stamped on every event")
		accountID  = flag.String("account-id", defaultAccountID, "AIQG account id (aiqg_account_id)")
		flowsPer   = flag.Int("flows-per-agent", 4, "flows generated per demo agent per pass (each flow emits several step-events)")
		inferredFl = flag.Int("inferred-flows", 3, "unattributed flows per pass (identity_source=inferred, no agent id)")
		interval   = flag.Duration("interval", 0, "if >0, loop forever emitting one pass per interval (e.g. 30s)")
		seed       = flag.Int64("seed", 0, "RNG seed for reproducible runs (0 = time-based)")
		dryRun     = flag.Bool("dry-run", false, "print log lines to stdout instead of pushing to Loki")
		orgID      = flag.String("org-id", "", "optional X-Scope-OrgID header for multi-tenant Loki")
		spreadSec  = flag.Int("spread", 90, "seconds to spread a pass's events over, ending at now")
		insecure   = flag.Bool("insecure", true, "skip TLS verification (TAS Loki uses the internal tas-ca-issuer CA)")

		complianceRate = flag.Float64("compliance-rate", 0.08, "fraction of events carrying a compliance (PII/cred/injection/safety) finding")
		vagueRate      = flag.Float64("vague-rate", 0.12, "fraction of events carrying a vague-input finding")
		hedgingRate    = flag.Float64("hedging-rate", 0.12, "fraction of events carrying a hedging finding")
		errorRate      = flag.Float64("error-rate", 0.02, "fraction of events that failed upstream (vendor_error)")
	)
	flag.Parse()

	s := *seed
	if s == 0 {
		s = time.Now().UnixNano()
	}
	g := rng{r: rand.New(rand.NewSource(s))}
	r := rates{
		Compliance: *complianceRate,
		Vague:      *vagueRate,
		Hedging:    *hedgingRate,
		Error:      *errorRate,
	}

	client := newLokiClient(*lokiURL, *orgID, *insecure)
	streamLabels := map[string]string{
		"namespace": "tas-llm-router", // the only label the dashboard filters on
		"container": "tas-llm-router",
		"source":    "demo-traffic-generator",
		"level":     "info",
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("demo-traffic: tenant=%s account=%s agents=%d flows-per-agent=%d inferred-flows=%d seed=%d dry-run=%v\n",
		*tenantID, *accountID, len(personas), *flowsPer, *inferredFl, s, *dryRun)
	if *interval <= 0 {
		fmt.Println("mode: single pass")
	} else {
		fmt.Printf("mode: loop every %s (Ctrl-C to stop)\n", *interval)
	}

	pass := func(now time.Time) error {
		entries, results := buildPass(g, r, *tenantID, *accountID, *flowsPer, *inferredFl, *spreadSec, now)
		if *dryRun {
			for _, e := range entries {
				fmt.Println(e.Line)
			}
		} else if err := client.push(ctx, streamLabels, entries); err != nil {
			return err
		}
		printSummary(results, *dryRun)
		return nil
	}

	if *interval <= 0 {
		if err := pass(time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	if err := pass(time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nstopped.")
			return
		case t := <-ticker.C:
			if err := pass(t); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}
		}
	}
}

// buildPass synthesizes one full pass as agent flows: each demo agent
// runs `flowsPerAgent` flows (grouped into multi-turn conversations),
// plus `inferredFlows` unattributed flows with no agent identity. Every
// flow is a tree of step-events sharing flow_id/conversation_id and
// linked via step_id/parent_step_id. The first event of each workflow
// profile is forced to carry its signature matcher so every dashboard
// panel still lights up in one pass. Timestamps are spread across the
// last spreadSec seconds so the time-series panels render a curve.
func buildPass(g rng, r rates, tenantID, accountID string, flowsPerAgent, inferredFlows, spreadSec int, now time.Time) ([]lokiEntry, []synthResult) {
	var entries []lokiEntry
	var results []synthResult
	window := time.Duration(spreadSec) * time.Second
	seenSig := map[string]bool{} // profile key → signature already forced this pass

	// emitFlow renders one flow's step tree. meta carries the flow-level
	// identity (agent/conversation/flow/source); mkStepID mints each
	// step's id (uuid for header flows, 16-hex span id for trace flows).
	// base is the (≤ now) anchor; steps are laid out just before it so
	// none land in the future.
	emitFlow := func(steps []stepSpec, meta agentContext, mkStepID func() string) {
		stepIDs := make([]string, len(steps))
		for i := range steps {
			stepIDs[i] = mkStepID()
		}
		base := now.Add(-time.Duration(g.r.Int63n(int64(window) + 1)))
		for i, s := range steps {
			p := profileByKey[s.Profile]
			actx := meta
			actx.StepID = stepIDs[i]
			actx.FlowStepSeq = i + 1
			if s.Parent >= 0 {
				actx.ParentStepID = stepIDs[s.Parent]
			}
			ts := base.Add(-time.Duration(len(steps)-1-i) * 50 * time.Millisecond)

			sig := !seenSig[p.Key]
			seenSig[p.Key] = true
			fields, res := synthEvent(g, p, sig, r, tenantID, accountID, ts)
			actx.stamp(fields)
			res.Agent = actx.AgentName
			res.FlowID = actx.FlowID
			line, err := marshalLine(fields)
			if err != nil {
				continue
			}
			entries = append(entries, lokiEntry{TS: ts, Line: line})
			results = append(results, res)
		}
	}

	// Named agent flows.
	for _, persona := range personas {
		agentID := agentIDFor(persona.Name)
		for remaining := flowsPerAgent; remaining > 0; {
			turns := min(g.intIn(persona.TurnsLo, persona.TurnsHi), remaining)
			convoID := g.uuid() // one conversation shared across this run's turns
			for range turns {
				remaining--
				trace := g.chance(persona.TraceProb)
				flowID, idSrc := g.uuid(), "header"
				if trace {
					flowID, idSrc = g.hex(16), "trace" // 32-hex W3C trace id
				}
				meta := agentContext{
					AgentID: agentID, AgentName: persona.Name, AgentVersion: persona.Version,
					ConversationID: convoID, FlowID: flowID, IdentitySource: idSrc,
				}
				mkStepID := g.uuid
				if trace {
					mkStepID = func() string { return g.hex(8) } // 16-hex W3C span id
				}
				emitFlow(persona.BuildFlow(g), meta, mkStepID)
			}
		}
	}

	// Unattributed (inferred) flows — flow_id present (reconstructed from
	// session), but no agent_id/agent_name.
	for range inferredFlows {
		persona := personas[g.r.Intn(len(personas))] // reuse a flow shape
		meta := agentContext{ConversationID: g.uuid(), FlowID: g.uuid(), IdentitySource: "inferred"}
		emitFlow(persona.BuildFlow(g), meta, g.uuid)
	}

	return entries, results
}

// printSummary reports the run's headline numbers rolled up per agent —
// flows, step-events, cost, potential savings (avoidable spend), and
// average CLEAR — exactly the shape the per-agent dashboard view shows.
func printSummary(results []synthResult, dryRun bool) {
	type agg struct {
		flows        map[string]bool
		steps        int
		cost         float64
		avoidableUSD float64
		compSum      int
		compN        int
	}
	byAgent := map[string]*agg{}
	var total agg
	total.flows = map[string]bool{}
	for _, res := range results {
		name := res.Agent
		if name == "" {
			name = "(inferred / unattributed)"
		}
		a := byAgent[name]
		if a == nil {
			a = &agg{flows: map[string]bool{}}
			byAgent[name] = a
		}
		a.steps++
		a.cost += res.CostUSD
		a.avoidableUSD += res.AvoidableUSD
		if res.FlowID != "" {
			a.flows[res.FlowID] = true
			total.flows[res.FlowID] = true
		}
		total.steps++
		total.cost += res.CostUSD
		total.avoidableUSD += res.AvoidableUSD
		if res.HasComposite {
			a.compSum += res.Composite
			a.compN++
			total.compSum += res.Composite
			total.compN++
		}
	}

	keys := make([]string, 0, len(byAgent))
	for k := range byAgent {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	verb := "pushed"
	if dryRun {
		verb = "generated (dry-run)"
	}
	fmt.Printf("\n%s %d flows / %d step-events  |  total cost $%.4f  |  potential savings $%.4f (%.1f%%)\n",
		verb, len(total.flows), total.steps, total.cost, total.avoidableUSD, pct(total.avoidableUSD, total.cost))
	fmt.Printf("%-30s %6s %6s  %12s  %12s  %9s\n", "agent", "flows", "steps", "cost USD", "avoidable", "avg CLEAR")
	for _, k := range keys {
		a := byAgent[k]
		avg := 0
		if a.compN > 0 {
			avg = a.compSum / a.compN
		}
		fmt.Printf("%-30s %6d %6d  %12.4f  %12.4f  %9d\n", k, len(a.flows), a.steps, a.cost, a.avoidableUSD, avg)
	}
	fmt.Println()
}

func pct(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return part / whole * 100
}
