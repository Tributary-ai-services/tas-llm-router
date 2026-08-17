package semcache

import (
	"testing"
	"time"
)

// These cover the two L2 over-rejection bugs found by the Plan #16 demo-flow
// live run (2026-08-16), where ticket-triage measured a 0% semantic hit rate
// against a modeled 65%. Both guards failed in the SAFE direction — no wrong
// answers — but they zeroed the hit rate on quoted user content, which is
// exactly the classification workload semantic caching should be best at.
//
// The tests are paired: each "must now match" case has a sibling asserting the
// discriminative behaviour the guard exists for is still intact, because the
// fix loosens matching and the dangerous direction is loosening too far.

// ---- Bug 1: capitals forced by position were read as proper nouns ----------

func TestPositionalCapital_QuotedStringOpenerIsNotAnEntity(t *testing.T) {
	sc := scopeOf("t1", "m1")
	now := time.Now()

	// The exact triage prompts that measured 0% live.
	seed := `Classify this support ticket into one category: "I can't log in to my account"`
	for _, incoming := range []string{
		`Classify this support ticket into one category: "Unable to log in to my account"`,
		`Classify this support ticket into one category: "Login is not working for my account"`,
	} {
		if r := Verify(incoming, entryOf(seed, "t1", "m1"), sc, now, time.Hour); !r.Pass {
			t.Errorf("must accept paraphrase with a capital opening the quote\n  seed:     %s\n  incoming: %s\n  rejected on: %s", seed, incoming, r.Reason)
		}
	}
}

func TestPositionalCapital_SkippedAfterEveryBoundary(t *testing.T) {
	// A capital is positional after sentence enders, clause introducers, and
	// quote/bracket openers. None of these should yield an entity token.
	for _, s := range []string{
		`Ready to go.`,
		`Done. Ready to go.`,
		`Is it ready? Ready to go.`,
		`Status: Ready to go.`,
		`He said "Ready to go."`,
		`(Ready to go.)`,
		"Line one\nReady to go.",
	} {
		if toks := discriminativeTokens(s); len(toks) != 0 {
			t.Errorf("discriminativeTokens(%q) = %v, want empty — every capital here is positional", s, toks)
		}
	}
}

func TestPositionalCapital_CaseOnlyDifferenceNowMatches(t *testing.T) {
	a := `Classify: "Unable to log in"`
	b := `Classify: "unable to log in"`
	if !discriminativeTokensMatch(a, b) {
		t.Errorf("case-only difference at a quote opener must not be discriminative\n  a tokens=%v\n  b tokens=%v",
			discriminativeTokens(a), discriminativeTokens(b))
	}
}

// The guard still has to do its job: a genuine mid-sentence proper noun is not
// positional and must stay discriminative.
func TestPositionalCapital_MidSentenceProperNounsStillDiscriminate(t *testing.T) {
	a := "What are the fees on the Chase Sapphire Reserve card?"
	b := "What are the fees on the Chase Sapphire card?"
	if discriminativeTokensMatch(a, b) {
		t.Error("Sapphire vs Sapphire Reserve must still be discriminative")
	}
	toks := discriminativeTokens(a)
	for _, want := range []string{"@chase", "@sapphire", "@reserve"} {
		if _, ok := toks[want]; !ok {
			t.Errorf("mid-sentence proper noun %s missing from %v", want, toks)
		}
	}
}

// Acronyms carry high discriminative signal and are exempt from the positional
// skip — including when they open a quote.
func TestPositionalCapital_AcronymAtBoundaryStillKept(t *testing.T) {
	toks := discriminativeTokens(`Classify this ticket: "VPN is down"`)
	if _, ok := toks["@vpn"]; !ok {
		t.Errorf("acronym opening a quote must stay discriminative, got %v", toks)
	}

	sc := scopeOf("t1", "m1")
	seed := `Classify this ticket: "I can't log in to my account"`
	incoming := `Classify this ticket: "VPN is down"`
	if r := Verify(incoming, entryOf(seed, "t1", "m1"), sc, time.Now(), time.Hour); r.Pass {
		t.Error("a ticket naming VPN must not match one that does not (acronym is discriminative)")
	}
}

// Numbers/dates/versions are discriminative wherever they appear — position
// never exempts them.
func TestPositionalCapital_NumbersUnaffected(t *testing.T) {
	sc := scopeOf("t1", "m1")
	seed := `Reset the certificate.`
	incoming := `Reset the certificate under the 2024 policy.`
	if r := Verify(incoming, entryOf(seed, "t1", "m1"), sc, time.Now(), time.Hour); r.Pass {
		t.Error("a date must still be discriminative")
	} else if r.Reason != "entity_number_date" {
		t.Errorf("expected entity_number_date rejection, got %q", r.Reason)
	}
}

// ---- Bug 2: "can't" was a negation but "unable" was not -------------------

func TestNegation_UnableIsSuppletiveCannot(t *testing.T) {
	for _, s := range []string{"I can't log in", "I cannot log in", "I am unable to log in"} {
		if got := negationParity(s); got != 1 {
			t.Errorf("negationParity(%q) = %d, want 1 — these are the same polarity", s, got)
		}
	}
}

func TestNegation_ActionNegatingVerbsFlipPolarity(t *testing.T) {
	// The dangerous pairs: lexically close enough to clear any workable
	// similarity threshold, but opposite in meaning.
	for _, tc := range []struct{ base, negated string }{
		{"How do I enable MFA?", "How do I disable MFA?"},
		{"How do I reset my certificate?", "How do I avoid resetting my certificate?"},
		{"How do I allow this domain?", "How do I prevent this domain?"},
		{"Which rows are included?", "Which rows are excluded?"},
		// Every inflection must count, not just the bare stem.
		{"How do I reset it?", "How do I avoided resetting it?"},
		{"How do I reset it?", "How do I keep preventing it?"},
		{"How do I enable it?", "How do I keep it disabled?"},
	} {
		if negationParity(tc.base) == negationParity(tc.negated) {
			t.Errorf("polarity must differ:\n  %q (parity %d)\n  %q (parity %d)",
				tc.base, negationParity(tc.base), tc.negated, negationParity(tc.negated))
		}
	}
}

func TestNegation_DisableVsEnableRejectedByVerify(t *testing.T) {
	sc := scopeOf("t1", "m1")
	cand := entryOf("How do I enable MFA for my account?", "t1", "m1")
	incoming := "How do I disable MFA for my account?"
	if r := Verify(incoming, cand, sc, time.Now(), time.Hour); r.Pass {
		t.Fatal("enable vs disable must be rejected — serving one for the other inverts the instruction")
	} else if r.Reason != "negation" {
		t.Errorf("expected negation rejection, got %q", r.Reason)
	}
}

// Polysemous ops verbs are deliberately NOT treated as polarity markers —
// "how do I stop nginx" is an ordinary request, not a negation.
func TestNegation_PolysemousOpsVerbsAreNotNegations(t *testing.T) {
	for _, s := range []string{"How do I stop nginx?", "How do I skip the migration?", "How do I block this IP?"} {
		if got := negationParity(s); got != 0 {
			t.Errorf("negationParity(%q) = %d, want 0 — this is a content verb, not a polarity marker", s, got)
		}
	}
}

// Double negation still reads as no negation, so the parity arithmetic did not
// change meaning with the added words.
func TestNegation_DoubleNegationParityUnchanged(t *testing.T) {
	if got := negationParity("It is not unable to run without help"); got != 1 {
		// not(1) + unable(1) + without(1) = 3 -> parity 1
		t.Errorf("negationParity = %d, want 1", got)
	}
	if got := negationParity("cannot avoid"); got != 0 {
		// cannot(1) + avoid(1) = 2 -> parity 0
		t.Errorf("negationParity(%q) = %d, want 0", "cannot avoid", got)
	}
}
