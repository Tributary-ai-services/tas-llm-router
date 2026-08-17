package semcache

import (
	"context"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"
)

// Live threshold calibration against the deployed embedder.
//
// Guarded like redis_store_integration_test.go — set AIQG_CALIBRATION_OLLAMA_URL
// (e.g. http://localhost:11434 behind a port-forward to svc/ollama in tas-shared)
// to run it. Skipped by default so CI stays hermetic.
//
// Motivation: the Plan #16 demo flows measured hit rates well under their models
// on security-questionnaire (25% vs 60%) and incident-burst (38% vs 85%), and
// running those prompts through Verify() showed L2 PASSING — so the misses were
// L1 similarity, not the guards. This answers the follow-on question the guards
// cannot: is the deployed threshold wrong, or is all-minilm simply unable to
// separate these classes at any threshold? FPRFloor is the discriminator
// (§9.2 step 5): if it stays above budget even at the strictest threshold, no
// threshold rescues the model.
//
// The pair set is drawn from the live flow prompts plus the near-miss classes a
// cache actually has to survive. Per calibrate.go's own warning, the NEGATIVE
// class is the one that matters — calibrating on positives alone tunes straight
// to threshold 0.

// livePairs: Match=true means the two genuinely share an answer.
var livePairs = []LabeledPair{
	// ---- POSITIVES: real paraphrases that should share an answer ----
	// helpdesk (these DO hit live)
	{"What's the process for resetting my VPN cert?", "How do I reset my VPN certificate?", true},
	{"How can I reset the VPN certificate on my laptop?", "How do I reset my VPN certificate?", true},
	// RFP (p1 MISSED live, p2 hit)
	{"Is customer data encrypted while stored?", "Do you encrypt customer data at rest?", true},
	{"Confirm whether customer data is encrypted at rest.", "Do you encrypt customer data at rest?", true},
	// triage (both MISSED before the L2 fix; hit after)
	{`Classify this support ticket into one category: "Unable to log in to my account"`,
		`Classify this support ticket into one category: "I can't log in to my account"`, true},
	{`Classify this support ticket into one category: "Login is not working for my account"`,
		`Classify this support ticket into one category: "I can't log in to my account"`, true},
	// burst — 3 of 7 hit live; all 7 are genuine paraphrases
	{"Are we having an outage with the payments API?", "Is the payments API down right now?", true},
	{"Payments API seems to be failing — is there a known issue?", "Is the payments API down right now?", true},
	{"Is there an incident affecting the payments API?", "Is the payments API down right now?", true},
	{"Why is the payments API returning errors?", "Is the payments API down right now?", true},
	{"Is the payment service currently unavailable?", "Is the payments API down right now?", true},
	{"Are payments broken at the moment?", "Is the payments API down right now?", true},
	{"Known problem with the payments API?", "Is the payments API down right now?", true},

	// ---- NEGATIVES: look similar, must NOT share an answer ----
	// the live probes
	{"How do I reset my VPN certificate under the 2024 access policy?", "How do I reset my VPN certificate?", false},
	{"How do I make sure I do not reset my VPN certificate?", "How do I reset my VPN certificate?", false},
	{"Is your recovery point objective 4 hours or less?", "What is your recovery point objective, in hours?", false},
	{"Do you comply with SOC 2 Type II?", "Do you encrypt customer data at rest?", false},
	{`Classify this support ticket into one category: "I can't log in to the VPN"`,
		`Classify this support ticket into one category: "I can't log in to my account"`, false},
	{`Classify this support ticket into one category: "I can log in, but billing is wrong"`,
		`Classify this support ticket into one category: "I can't log in to my account"`, false},
	{"Is the payments API down in the EU region specifically?", "Is the payments API down right now?", false},
	// classic near-miss classes a cache must survive
	{"Do you encrypt customer data in transit?", "Do you encrypt customer data at rest?", false},
	{"How do I disable MFA for my account?", "How do I enable MFA for my account?", false},
	{"What is the renewal notice period?", "What is the termination notice period?", false},
	{"How do I reset my password?", "How do I reset my VPN certificate?", false},
	{"Is the search API down right now?", "Is the payments API down right now?", false},
	{"What are the fees on the Chase Sapphire card?", "What are the fees on the Chase Sapphire Reserve card?", false},

	// ---- NUANCE-ONLY NEGATIVES: the class L2 structurally CANNOT catch ----
	// These differ by a single lowercase content word — no capitalized entity,
	// no number/date, no negation, so scope/freshness/negation/entity all pass
	// and the L1 threshold is the ONLY thing standing between them and a wrong
	// answer. Without these the pair set would be biased toward L2's strengths
	// (the probes above were designed to exercise L2) and would overstate how
	// safe a low threshold is.
	{"How do I renew my VPN certificate?", "How do I reset my VPN certificate?", false},
	{"Do you encrypt employee data at rest?", "Do you encrypt customer data at rest?", false},
	{"How do I import my data?", "How do I export my data?", false},
	{"Is the payment charged automatically?", "Is the payment refunded automatically?", false},
	{"What is the termination notice address?", "What is the termination notice period?", false},
}

// calibrationThresholds extends below DefaultThresholds()'s 0.80 floor. The
// question is where L2's coverage ends — i.e. how far the threshold can drop
// before the nuance-only negatives start getting served.
func calibrationThresholds() []float64 {
	var out []float64
	for v := 0.50; v <= 0.99001; v += 0.01 {
		out = append(out, round3(v))
	}
	return out
}

func TestLiveThresholdCalibration(t *testing.T) {
	base := os.Getenv("AIQG_CALIBRATION_OLLAMA_URL")
	if base == "" {
		t.Skip("set AIQG_CALIBRATION_OLLAMA_URL to run live calibration")
	}
	model := os.Getenv("AIQG_CALIBRATION_EMBED_MODEL")
	if model == "" {
		model = "all-minilm"
	}
	dim := 384
	if s := os.Getenv("AIQG_CALIBRATION_EMBED_DIM"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("bad AIQG_CALIBRATION_EMBED_DIM %q: %v", s, err)
		}
		dim = v
	}
	// Some encoders (nomic-embed-text, bge-*) are trained with a task prefix
	// and degrade materially without it. OllamaEmbedder sends RAW text, so
	// measuring such a model unprefixed understates it — test both before
	// concluding a model is unsuitable, because "add prefix support to the
	// embedder" and "this model is no good" are very different conclusions.
	// Cache matching is prompt-vs-prompt (symmetric), so the SAME prefix goes
	// on both sides rather than a query/document split.
	prefix := os.Getenv("AIQG_CALIBRATION_EMBED_PREFIX")

	emb := NewOllamaEmbedder(base, model, dim)

	// Embed every unique text once.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	vecs := map[string][]float32{}
	for _, p := range livePairs {
		for _, s := range []string{p.Query, p.Candidate} {
			if _, ok := vecs[s]; ok {
				continue
			}
			v, err := emb.Embed(ctx, prefix+s)
			if err != nil {
				t.Fatalf("embed %q: %v", truncateForLog(s), err)
			}
			if len(v) != dim {
				t.Fatalf("embed %q: got dim %d, want %d — set AIQG_CALIBRATION_EMBED_DIM", truncateForLog(s), len(v), dim)
			}
			vecs[s] = v
		}
	}
	t.Logf("model=%s dim=%d prefix=%q unique_texts=%d pairs=%d", model, dim, prefix, len(vecs), len(livePairs))

	// Per-pair similarity, sorted — the separation picture.
	type row struct {
		sim   float64
		match bool
		label string
	}
	rows := make([]row, 0, len(livePairs))
	for _, p := range livePairs {
		rows = append(rows, row{cosineSimilarity(vecs[p.Query], vecs[p.Candidate]), p.Match, truncateForLog(p.Query)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].sim > rows[j].sim })

	t.Log("--- pairs by similarity (MATCH should be high, NEAR-MISS should be low) ---")
	var minPos, maxNeg = 1.0, 0.0
	for _, r := range rows {
		kind := "NEAR-MISS"
		if r.match {
			kind = "MATCH    "
			if r.sim < minPos {
				minPos = r.sim
			}
		} else if r.sim > maxNeg {
			maxNeg = r.sim
		}
		t.Logf("  %.4f  %s  %s", r.sim, kind, r.label)
	}
	t.Logf("lowest MATCH = %.4f | highest NEAR-MISS = %.4f | separation = %+.4f",
		minPos, maxNeg, minPos-maxNeg)

	sc := Scope{TenantID: "t1", Model: "m1", ScoringVersion: "v1"}
	for _, useL2 := range []bool{false, true} {
		mode := "L1 only (embedding quality alone)"
		if useL2 {
			mode = "L1 + L2 cascade (deployed behaviour)"
		}
		res := Sweep(livePairs, vecs, calibrationThresholds(), useL2, sc)
		t.Logf("--- %s ---", mode)
		t.Logf("  %-9s %5s %5s %5s %5s %9s %8s", "thresh", "TP", "FP", "FN", "TN", "hit_rate", "FPR")
		for _, r := range res {
			t.Logf("  %-9.2f %5d %5d %5d %5d %8.1f%% %7.1f%%",
				r.Threshold, r.TP, r.FP, r.FN, r.TN, r.HitRate*100, r.FPR*100)
		}
		t.Logf("  FPR floor = %.1f%%", FPRFloor(res)*100)
		for _, budget := range []float64{0.0, 0.05, 0.10} {
			if best, ok := Recommend(res, budget); ok {
				t.Logf("  budget FPR<=%3.0f%% -> threshold %.2f (hit_rate %.1f%%, FPR %.1f%%)",
					budget*100, best.Threshold, best.HitRate*100, best.FPR*100)
			} else {
				t.Logf("  budget FPR<=%3.0f%% -> UNREACHABLE at any threshold", budget*100)
			}
		}
	}
}

func truncateForLog(s string) string {
	const n = 52
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
