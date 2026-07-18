package semcache

import (
	"testing"
	"time"
)

func scopeOf(t, m string) Scope { return Scope{TenantID: t, Model: m, ScoringVersion: "v1"} }

func entryOf(prompt, tenant, model string) *Entry {
	return &Entry{Prompt: prompt, TenantID: tenant, Model: model, ScoringVersion: "v1", CreatedAtUnix: time.Now().Unix()}
}

func TestVerify_EntityGuard_SapphireReserve(t *testing.T) {
	sc := scopeOf("t1", "m1")
	now := time.Now()
	// The canonical counterexample: cosine 0.99, different answers. L2 must reject.
	cand := entryOf("What are the fees on the Chase Sapphire Reserve card?", "t1", "m1")
	incoming := "What are the fees on the Chase Sapphire card?"
	if r := Verify(incoming, cand, sc, now, time.Hour); r.Pass {
		t.Fatal("must reject Sapphire vs Sapphire Reserve (entity guard)")
	} else if r.Reason != "entity_number_date" {
		t.Errorf("expected entity_number_date rejection, got %q", r.Reason)
	}
	// The exact same question must pass.
	if r := Verify(cand.Prompt, cand, sc, now, time.Hour); !r.Pass {
		t.Errorf("identical prompt must pass: %+v", r)
	}
}

func TestVerify_NumbersAndDatesAndVersions(t *testing.T) {
	sc := scopeOf("t1", "m1")
	now := time.Now()
	cases := []struct{ inc, cand string }{
		{"invoice total for 4 seats", "invoice total for 5 seats"},                   // number
		{"what changed in release 2026-07-01", "what changed in release 2026-08-01"}, // date
		{"how do I use GPT-4 tools", "how do I use GPT-5 tools"},                     // version/SKU
	}
	for _, c := range cases {
		r := Verify(c.inc, entryOf(c.cand, "t1", "m1"), sc, now, time.Hour)
		if r.Pass {
			t.Errorf("must reject differing number/date/version: %q vs %q", c.inc, c.cand)
		}
	}
	// Same numbers/dates → the entity guard doesn't block (other guards apply).
	if r := Verify("invoice total for 5 seats", entryOf("invoice total for 5 seats", "t1", "m1"), sc, now, time.Hour); !r.Pass {
		t.Errorf("identical number prompt should pass: %+v", r)
	}
}

func TestVerify_NegationGuard(t *testing.T) {
	sc := scopeOf("t1", "m1")
	now := time.Now()
	if r := Verify("is aspirin safe for kids", entryOf("is aspirin not safe for kids", "t1", "m1"), sc, now, time.Hour); r.Pass {
		t.Fatal("polarity disagreement must reject (negation guard)")
	} else if r.Reason != "negation" {
		t.Errorf("expected negation rejection, got %q", r.Reason)
	}
	// Contraction counts as negation.
	if r := Verify("can I return this", entryOf("can't I return this", "t1", "m1"), sc, now, time.Hour); r.Pass {
		t.Error("contraction n't must flip polarity")
	}
	// Double negation has the same parity as none → not blocked by negation.
	r := Verify("this is not unusual", entryOf("this is not unusual", "t1", "m1"), sc, now, time.Hour)
	if !r.Pass {
		t.Errorf("identical double-negation prompt should pass: %+v", r)
	}
}

func TestVerify_ScopeGuard(t *testing.T) {
	now := time.Now()
	cand := entryOf("hello world", "t1", "m1")
	// Cross-tenant — the cardinal failure. Must reject even with identical text.
	if r := Verify("hello world", cand, scopeOf("t2", "m1"), now, time.Hour); r.Pass || r.Reason != "scope" {
		t.Errorf("cross-tenant must reject with scope reason: %+v", r)
	}
	// Cross-model.
	if r := Verify("hello world", cand, scopeOf("t1", "m2"), now, time.Hour); r.Pass || r.Reason != "scope" {
		t.Errorf("cross-model must reject with scope reason: %+v", r)
	}
}

func TestVerify_Freshness(t *testing.T) {
	sc := scopeOf("t1", "m1")
	now := time.Now()
	stale := entryOf("hello world", "t1", "m1")
	stale.CreatedAtUnix = now.Add(-2 * time.Hour).Unix()
	if r := Verify("hello world", stale, sc, now, time.Hour); r.Pass || r.Reason != "freshness" {
		t.Errorf("stale entry must reject with freshness reason: %+v", r)
	}
	fresh := entryOf("hello world", "t1", "m1")
	fresh.CreatedAtUnix = now.Add(-1 * time.Minute).Unix()
	if r := Verify("hello world", fresh, sc, now, time.Hour); !r.Pass {
		t.Errorf("fresh entry should pass: %+v", r)
	}
}

func TestVerify_ParaphrasePasses(t *testing.T) {
	// A genuine paraphrase with the SAME entities/numbers/polarity should pass L2
	// (L1 similarity is what gates whether it even reaches here).
	sc := scopeOf("t1", "m1")
	now := time.Now()
	r := Verify("how do I reset my password", entryOf("how can I reset my password", "t1", "m1"), sc, now, time.Hour)
	if !r.Pass {
		t.Errorf("clean paraphrase should pass L2: %+v", r)
	}
}
