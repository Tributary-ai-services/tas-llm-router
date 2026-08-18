package semcache

import (
	"testing"
	"time"
)

func envEntry(prompt string) *Entry {
	return &Entry{Prompt: prompt, TenantID: "t1", Model: "m1", ScoringVersion: "v1",
		CreatedAtUnix: time.Now().Unix()}
}

var envScope = Scope{TenantID: "t1", Model: "m1", ScoringVersion: "v1"}

// The case this guard was written for. Both embedders score this near-miss
// ABOVE a genuine paraphrase (all-minilm 0.8904, langcache 0.8971), so no
// threshold rejects it and L2 is the only thing standing between the question
// and the wrong answer.
func TestVerifyRejectsEnvironmentSwap(t *testing.T) {
	const seed = "What is our policy for rotating production database credentials?"
	got := Verify("What is our policy for rotating staging database credentials?",
		envEntry(seed), envScope, time.Now(), time.Hour)
	if got.Pass {
		t.Fatal("staging question served the production answer — the exact failure this guard exists to stop")
	}
	if got.Reason != "environment" {
		t.Fatalf("Reason = %q, want \"environment\"", got.Reason)
	}
}

// Canonicalisation is load-bearing in the opposite direction: prod/production
// must collapse, or the guard converts a correct cache hit into a miss.
func TestVerifyAllowsEnvironmentSynonyms(t *testing.T) {
	cases := []struct{ a, b string }{
		{"How do I restart the production API?", "How do I restart the prod API?"},
		{"staging deploy steps?", "stage deploy steps?"},
		{"dev database seeding", "development database seeding"},
	}
	for _, c := range cases {
		if !environmentsMatch(c.a, c.b) {
			t.Errorf("environmentsMatch(%q, %q) = false; synonyms must collapse or real hits are suppressed", c.a, c.b)
		}
	}
}

// Substring matching would invent environments out of ordinary prose:
// "production" sits inside "reproduction", "prod" inside "product"/"produce".
// Any of these firing would reject unrelated paraphrases for no reason.
func TestEnvironmentsOfIgnoresSubstrings(t *testing.T) {
	for _, s := range []string{
		"explain the reproduction steps",
		"what is our product roadmap",
		"how do we produce the report",
		"productivity tips",
		"the introduction section",
	} {
		if envs := environmentsOf(s); len(envs) != 0 {
			t.Errorf("environmentsOf(%q) = %v, want none — substring match would break unrelated prose", s, envs)
		}
	}
}

// "pre-prod" must not read as bare "prod". Reading it as production is exactly
// the confusion the guard prevents, just one level down.
func TestEnvironmentsOfQualifiedForms(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"deploy to pre-prod first", "preproduction"},
		{"deploy to preprod first", "preproduction"},
		{"deploy to pre prod first", "preproduction"},
		{"non-prod accounts only", "nonproduction"},
		{"nonprod accounts only", "nonproduction"},
		{"deploy to prod", "production"},
	}
	for _, c := range cases {
		envs := environmentsOf(c.in)
		if _, ok := envs[c.want]; !ok || len(envs) != 1 {
			t.Errorf("environmentsOf(%q) = %v, want exactly {%s}", c.in, envs, c.want)
		}
	}
	// The distinction has to survive comparison, not just extraction.
	if environmentsMatch("deploy to pre-prod", "deploy to prod") {
		t.Error("pre-prod matched prod — a qualified environment is a different environment")
	}
}

// An environment named on one side only is a mismatch. The unqualified question
// usually means production, so treating absence as a wildcard would let a
// general question serve the staging answer.
func TestVerifyRejectsAsymmetricEnvironment(t *testing.T) {
	got := Verify("How do I rotate database credentials?",
		envEntry("How do I rotate staging database credentials?"), envScope, time.Now(), time.Hour)
	if got.Pass {
		t.Fatal("unqualified question matched a staging entry; absence must not act as a wildcard")
	}
	if got.Reason != "environment" {
		t.Fatalf("Reason = %q, want \"environment\"", got.Reason)
	}
}

// The guard must be inert on the overwhelming majority of traffic, which names
// no environment at all.
func TestVerifyUnaffectedWhenNoEnvironmentMentioned(t *testing.T) {
	got := Verify("How do I reset my VPN certificate?",
		envEntry("How do I reset my VPN certificate?"), envScope, time.Now(), time.Hour)
	if !got.Pass {
		t.Fatalf("identical prompts rejected with %q; the guard must be inert when no environment is named", got.Reason)
	}
}

func TestEnvironmentsMatchBothAbsent(t *testing.T) {
	if !environmentsMatch("what is the refund window", "how long is the refund window") {
		t.Error("two prompts naming no environment must match")
	}
}

// Distinct environments must stay distinct rather than collapsing into one
// "non-production" bucket -- a staging answer is not a dev answer.
func TestEnvironmentsAreNotConflated(t *testing.T) {
	pairs := [][2]string{
		{"staging db url", "dev db url"},
		{"uat sign-off process", "staging sign-off process"},
		{"sandbox api limits", "canary api limits"},
	}
	for _, p := range pairs {
		if environmentsMatch(p[0], p[1]) {
			t.Errorf("environmentsMatch(%q, %q) = true; distinct environments must not collapse", p[0], p[1])
		}
	}
}

// Polysemous words are deliberately NOT environments. Each of these reads as an
// environment only occasionally, so admitting them would reject ordinary
// paraphrases far more often than it would prevent a wrong answer.
func TestEnvironmentsOfExcludesPolysemousWords(t *testing.T) {
	for _, s := range []string{
		"can you show me a live demo",
		"how do I test this locally",
		"ask Dr Smith about the int field",
		"what is the local time",
		"who is on the qa team",
	} {
		if envs := environmentsOf(s); len(envs) != 0 {
			t.Errorf("environmentsOf(%q) = %v, want none — polysemous words must not read as environments", s, envs)
		}
	}
}
