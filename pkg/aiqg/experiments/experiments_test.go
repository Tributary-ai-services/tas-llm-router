package experiments

import (
	"context"
	"encoding/json"
	matcher "github.com/Tributary-ai-services/aether-shared/go-aiqg-matcher"
	"testing"
	"time"
)

func ptr(i int) *int { return &i }

func twoArm(maxPct int) Experiment {
	e := Experiment{
		ID:       "exp-1",
		Status:   StatusRunning,
		Variants: []Variant{{Key: "control", Weight: 50}, {Key: "b", Weight: 50, Override: json.RawMessage(`{"model":"gpt-4o-mini"}`)}},
	}
	if maxPct > 0 {
		e.Guardrails.MaxTrafficPct = ptr(maxPct)
	}
	return e
}

func TestAssign_DeterministicAndSticky(t *testing.T) {
	e := twoArm(100)
	id := Identity{ConversationID: "conv-42"}
	d1 := e.assign(id)
	d2 := e.assign(id)
	if d1 == nil || d2 == nil {
		t.Fatal("expected assignment at 100% traffic")
	}
	if d1.Variant != d2.Variant {
		t.Errorf("same identity must be sticky: %s vs %s", d1.Variant, d2.Variant)
	}
	if d1.ExperimentID != "exp-1" {
		t.Errorf("experiment id not set: %+v", d1)
	}
}

func TestAssign_SplitIsRoughly5050(t *testing.T) {
	e := twoArm(100)
	control, variant := 0, 0
	for i := 0; i < 10000; i++ {
		d := e.assign(Identity{ConversationID: "user-" + itoa(i)})
		if d == nil {
			t.Fatal("no assignment at 100%")
		}
		if d.Variant == "control" {
			control++
		} else {
			variant++
		}
	}
	// Allow generous slack; just prove it's a real split, not all-one-arm.
	if control < 4000 || control > 6000 {
		t.Errorf("control share off: control=%d variant=%d", control, variant)
	}
}

func TestAssign_MaxTrafficPctUntouched(t *testing.T) {
	e := twoArm(10) // 10% eligible
	touched := 0
	for i := 0; i < 10000; i++ {
		if d := e.assign(Identity{ConversationID: "u-" + itoa(i)}); d != nil {
			touched++
		}
	}
	// ~10% eligible (allow slack); the rest untouched (nil).
	if touched < 700 || touched > 1300 {
		t.Errorf("eligible share off: touched=%d (want ~1000)", touched)
	}
}

func TestAssign_ZeroPctNeverAssigns(t *testing.T) {
	e := twoArm(100)
	e.Guardrails.MaxTrafficPct = ptr(0)
	if d := e.assign(Identity{ConversationID: "x"}); d != nil {
		t.Errorf("0%% traffic must never assign, got %+v", d)
	}
}

func TestAssignmentKey_LadderFallback(t *testing.T) {
	a := Assignment{} // default = conversation-first
	if got := a.key(Identity{ConversationID: "c", UserID: "u"}); got != "c" {
		t.Errorf("default key = %q, want conversation", got)
	}
	if got := a.key(Identity{UserID: "u", RequestID: "r"}); got != "u" {
		t.Errorf("ladder fallback to user = %q", got)
	}
	if got := a.key(Identity{RequestID: "r"}); got != "r" {
		t.Errorf("final fallback to request = %q", got)
	}
	au := Assignment{KeySource: "user"}
	if got := au.key(Identity{ConversationID: "c", UserID: "u"}); got != "u" {
		t.Errorf("key_source=user = %q, want u", got)
	}
}

func TestCohortMatches(t *testing.T) {
	c := Cohort{SourceApp: []string{"checkout"}, WorkflowType: []string{"rag"}}
	if !c.Matches(ReqAttrs{SourceApp: "checkout", WorkflowType: "rag"}.toMatcherAttrs()) {
		t.Error("should match")
	}
	if c.Matches(ReqAttrs{SourceApp: "other", WorkflowType: "rag"}.toMatcherAttrs()) {
		t.Error("source_app mismatch should fail")
	}
	if c.Matches(ReqAttrs{SourceApp: "checkout", WorkflowType: "code_generation"}.toMatcherAttrs()) {
		t.Error("workflow mismatch should fail")
	}
	// Empty cohort matches anything.
	if !(Cohort{}).Matches(ReqAttrs{SourceApp: "anything"}.toMatcherAttrs()) {
		t.Error("empty cohort must match all")
	}
}

// url_path changed meaning when cohorts adopted the shared matcher: it was a
// SUBSTRING match and is now an RE2 regex. The old behaviour was untested, so
// these lock in both the new semantics and the migration that preserves the old
// ones.
func TestCohortURLPathIsRegexAfterSharedMatcher(t *testing.T) {
	// Anchoring is now expressible — it was not under substring matching.
	anchored := Cohort{URLPath: "^/v1/chat$"}
	if anchored.Matches(ReqAttrs{Path: "/v1/chat_admin"}.toMatcherAttrs()) {
		t.Error("anchored pattern must not match /v1/chat_admin")
	}
	if !anchored.Matches(ReqAttrs{Path: "/v1/chat"}.toMatcherAttrs()) {
		t.Error("anchored pattern must match its exact path")
	}

	// An unanchored pattern still behaves like the old substring match, which is
	// why the vast majority of stored cohorts are unaffected by the change.
	loose := Cohort{URLPath: "/v1/chat"}
	if !loose.Matches(ReqAttrs{Path: "/api/v1/chat/completions"}.toMatcherAttrs()) {
		t.Error("unanchored pattern should still match anywhere in the path")
	}

	// A stored substring containing regex metacharacters is migrated with
	// QuoteMeta, so it keeps meaning the literal text rather than becoming a
	// wildcard. This is the case the migration exists for.
	migrated := Cohort{URLPath: matcher.SubstringToRegex("v1.chat")}
	if migrated.Matches(ReqAttrs{Path: "v1Xchat"}.toMatcherAttrs()) {
		t.Error("migrated substring must keep '.' literal, not match any character")
	}
	if !migrated.Matches(ReqAttrs{Path: "/api/v1.chat"}.toMatcherAttrs()) {
		t.Error("migrated substring must still match the literal text")
	}
}

// fakeLoader returns a fixed list (and counts loads to prove caching).
type fakeLoader struct {
	exps  []Experiment
	loads int
}

func (f *fakeLoader) ActiveForTenant(_ context.Context, _ string) ([]Experiment, error) {
	f.loads++
	return f.exps, nil
}

func TestResolver_ClaimFirstMatchAndCache(t *testing.T) {
	// Two overlapping experiments; the first (by list order) claims.
	expA := twoArm(100)
	expA.ID = "A"
	expB := twoArm(100)
	expB.ID = "B"
	fl := &fakeLoader{exps: []Experiment{expA, expB}}
	r := NewResolver(fl, time.Minute)

	d := r.Resolve(context.Background(), "t1", Identity{ConversationID: "c1"}, ReqAttrs{})
	if d == nil || d.ExperimentID != "A" {
		t.Fatalf("first experiment should claim, got %+v", d)
	}
	if !d.Apply {
		t.Error("running experiment should have Apply=true")
	}
	// Second resolve hits cache (no extra load).
	r.Resolve(context.Background(), "t1", Identity{ConversationID: "c2"}, ReqAttrs{})
	if fl.loads != 1 {
		t.Errorf("expected 1 load (cached), got %d", fl.loads)
	}
}

func TestResolver_DryRunNoApply(t *testing.T) {
	e := twoArm(100)
	e.Status = StatusDryRun
	r := NewResolver(&fakeLoader{exps: []Experiment{e}}, time.Minute)
	d := r.Resolve(context.Background(), "t1", Identity{ConversationID: "c"}, ReqAttrs{})
	if d == nil {
		t.Fatal("dry_run should still assign + stamp")
	}
	if d.Apply {
		t.Error("dry_run must NOT apply the override")
	}
}

func TestResolver_NilSafe(t *testing.T) {
	var r *Resolver
	if d := r.Resolve(context.Background(), "t", Identity{}, ReqAttrs{}); d != nil {
		t.Error("nil resolver must return nil")
	}
}

// tiny int→string to avoid strconv import churn in the table tests
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
