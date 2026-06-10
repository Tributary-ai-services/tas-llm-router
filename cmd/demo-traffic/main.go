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
		perProfile = flag.Int("per-profile", 12, "events per agent profile per pass")
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

	fmt.Printf("demo-traffic: tenant=%s account=%s per-profile=%d profiles=%d seed=%d dry-run=%v\n",
		*tenantID, *accountID, *perProfile, len(profiles), s, *dryRun)
	if *interval <= 0 {
		fmt.Println("mode: single pass")
	} else {
		fmt.Printf("mode: loop every %s (Ctrl-C to stop)\n", *interval)
	}

	pass := func(now time.Time) error {
		entries, results := buildPass(g, r, *tenantID, *accountID, *perProfile, *spreadSec, now)
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

// buildPass synthesizes one full pass: per-profile events with the first
// of each profile forced to carry its signature matcher. Timestamps are
// spread across the last spreadSec seconds so the time-series panels
// render a curve rather than a single spike.
func buildPass(g rng, r rates, tenantID, accountID string, perProfile, spreadSec int, now time.Time) ([]lokiEntry, []synthResult) {
	var entries []lokiEntry
	var results []synthResult
	window := time.Duration(spreadSec) * time.Second

	for _, p := range profiles {
		for i := range perProfile {
			// Offset back from now by a random fraction of the window.
			ts := now.Add(-time.Duration(g.r.Int63n(int64(window) + 1)))
			fields, res := synthEvent(g, p, i == 0, r, tenantID, accountID, ts)
			line, err := marshalLine(fields)
			if err != nil {
				continue
			}
			entries = append(entries, lokiEntry{TS: ts, Line: line})
			results = append(results, res)
		}
	}
	return entries, results
}

// printSummary reports the run's headline numbers, including the
// potential savings (avoidable spend) the dashboard's avoidable-cost
// panel will surface.
func printSummary(results []synthResult, dryRun bool) {
	type agg struct {
		count        int
		cost         float64
		avoidableUSD float64
		compSum      int
		compN        int
	}
	byProfile := map[string]*agg{}
	var total agg
	for _, res := range results {
		a := byProfile[res.Profile]
		if a == nil {
			a = &agg{}
			byProfile[res.Profile] = a
		}
		a.count++
		a.cost += res.CostUSD
		a.avoidableUSD += res.AvoidableUSD
		total.count++
		total.cost += res.CostUSD
		total.avoidableUSD += res.AvoidableUSD
		if res.HasComposite {
			a.compSum += res.Composite
			a.compN++
			total.compSum += res.Composite
			total.compN++
		}
	}

	keys := make([]string, 0, len(byProfile))
	for k := range byProfile {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	verb := "pushed"
	if dryRun {
		verb = "generated (dry-run)"
	}
	fmt.Printf("\n%s %d events  |  total cost $%.4f  |  potential savings $%.4f (%.1f%%)\n",
		verb, total.count, total.cost, total.avoidableUSD, pct(total.avoidableUSD, total.cost))
	fmt.Printf("%-28s %6s  %12s  %12s  %9s\n", "profile", "events", "cost USD", "avoidable", "avg CLEAR")
	for _, k := range keys {
		a := byProfile[k]
		avg := 0
		if a.compN > 0 {
			avg = a.compSum / a.compN
		}
		fmt.Printf("%-28s %6d  %12.4f  %12.4f  %9d\n", k, a.count, a.cost, a.avoidableUSD, avg)
	}
	fmt.Println()
}

func pct(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return part / whole * 100
}
