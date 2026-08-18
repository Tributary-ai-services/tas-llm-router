package main

import (
	"testing"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/semcache"
)

func TestClassCounts(t *testing.T) {
	pairs := []semcache.LabeledPair{
		{Query: "a", Candidate: "b", Match: true},
		{Query: "c", Candidate: "d", Match: false},
		{Query: "e", Candidate: "f", Match: false},
	}
	pos, neg := classCounts(pairs)
	if pos != 1 || neg != 2 {
		t.Fatalf("classCounts = (%d,%d), want (1,2)", pos, neg)
	}
}

// A single-class corpus must be reported as such rather than swept into a
// confident-looking threshold: with no near-misses every threshold shows FPR 0,
// which reads as "perfectly safe" and is how an earlier calibration reached the
// wrong conclusion.
func TestClassCountsSingleClass(t *testing.T) {
	pos, neg := classCounts([]semcache.LabeledPair{
		{Query: "a", Candidate: "b", Match: true},
		{Query: "c", Candidate: "d", Match: true},
	})
	if pos == 0 || neg != 0 {
		t.Fatalf("classCounts = (%d,%d), want (2,0)", pos, neg)
	}
}

func TestDominantScope(t *testing.T) {
	s1 := semcache.Scope{TenantID: "t1", Model: "m1", ScoringVersion: "v1"}
	s2 := semcache.Scope{TenantID: "t2", Model: "m2", ScoringVersion: "v1"}
	judged := []semcache.JudgedPair{{Scope: s1}, {Scope: s2}, {Scope: s1}}

	got, mixed := dominantScope(judged)
	if got != s1 {
		t.Fatalf("dominantScope = %+v, want %+v", got, s1)
	}
	if !mixed {
		t.Fatal("mixed = false, want true — a corpus spanning scopes must be flagged")
	}
}

func TestDominantScopeSingle(t *testing.T) {
	s := semcache.Scope{TenantID: "t", Model: "m", ScoringVersion: "v"}
	got, mixed := dominantScope([]semcache.JudgedPair{{Scope: s}, {Scope: s}})
	if got != s || mixed {
		t.Fatalf("dominantScope = (%+v,%v), want (%+v,false)", got, mixed, s)
	}
}

func TestDominantScopeEmpty(t *testing.T) {
	if _, mixed := dominantScope(nil); mixed {
		t.Fatal("empty corpus must not report mixed scopes")
	}
}

func TestNewEmbedder(t *testing.T) {
	for _, p := range []string{"ollama", "OLLAMA", "tei", "TEI"} {
		if _, err := newEmbedder(p, "http://o", "m", "http://t", 384); err != nil {
			t.Fatalf("newEmbedder(%q) = %v, want nil", p, err)
		}
	}
	if _, err := newEmbedder("word2vec", "", "", "", 384); err == nil {
		t.Fatal("newEmbedder with an unknown provider must error, not silently pick a default")
	}
}
