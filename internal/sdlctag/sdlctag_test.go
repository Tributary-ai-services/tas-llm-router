package sdlctag

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workload "github.com/Tributary-ai-services/aether-shared/go-aiqg-workload"
	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
)

// All prompts here are synthetic. Real prompts never enter this repo.

func quietLog() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

func on(tenants ...string) Config {
	return Config{Enabled: true, Tenants: tenants}
}

// countingExtract wraps the real extractor and counts calls, so a test can
// prove the extractor was never reached.
func countingExtract(n *atomic.Int32) func(workload.TurnStartInput) workload.TurnStartFlags {
	return func(in workload.TurnStartInput) workload.TurnStartFlags {
		n.Add(1)
		return workload.ExtractTurnStart(in)
	}
}

func approvalPair() []types.Message {
	return []types.Message{
		{Role: "system", Content: "You are a coding agent."},
		{Role: "user", Content: "Bump the chart version."},
		{Role: "assistant", Content: "Chart bumped to 1.4.2. Want me to roll it out to staging?"},
		{Role: "user", Content: "yes"},
	}
}

func TestNew_OffByDefault(t *testing.T) {
	cases := map[string]Config{
		"zero value":             {},
		"tenants without global": {Tenants: []string{"t1"}},
		"global without tenants": {Enabled: true},
		"blank tenant ids":       {Enabled: true, Tenants: []string{"", "  "}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			tg := New(cfg, quietLog())
			if tg != nil {
				t.Fatalf("New(%+v) = non-nil, want nil (off)", cfg)
			}
			// A nil Tagger is valid and off.
			if tg.Active("t1") || tg.Tag("t1", approvalPair()) != nil || tg.Tripped() {
				t.Fatal("nil Tagger must be inert")
			}
		})
	}
}

func TestTag_TenantNotEnabled_NeverCallsExtractor(t *testing.T) {
	var calls atomic.Int32
	tg := New(on("tenant-on"), quietLog())
	tg.extract = countingExtract(&calls)

	for _, tenant := range []string{"tenant-off", ""} {
		if got := tg.Tag(tenant, approvalPair()); got != nil {
			t.Errorf("Tag(%q) = %+v, want nil", tenant, got)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("extractor called %d times for a tenant that is off", calls.Load())
	}
}

func TestTag_On_TagsTurnStart(t *testing.T) {
	var calls atomic.Int32
	tg := New(on("tenant-on"), quietLog())
	tg.extract = countingExtract(&calls)

	got := tg.Tag("tenant-on", approvalPair())
	if got == nil {
		t.Fatal("Tag = nil on an enabled tenant's turn start")
	}
	if calls.Load() != 1 {
		t.Fatalf("extractor calls = %d, want 1", calls.Load())
	}
	if got.ExtractorVersion != workload.TurnStartExtractorVersion {
		t.Errorf("ExtractorVersion = %q", got.ExtractorVersion)
	}
	if got.PromptLen != string(workload.LenTiny) || !got.HasPrev || !got.StartsApproval ||
		!got.PrevExplicitAsk || !got.PrevEndsQuestion || got.Truncated {
		t.Errorf("flags = %+v", got)
	}
}

func TestTag_NotATurnStart(t *testing.T) {
	tg := New(on("t"), quietLog())
	cases := map[string][]types.Message{
		"empty": nil,
		"tool result last": {
			{Role: "user", Content: "run the tests"},
			{Role: "assistant", ToolCalls: []types.ToolCall{{ID: "c1"}}},
			{Role: "tool", ToolCallID: "c1", Content: "ok 42 passed"},
		},
		"assistant prefill last": {
			{Role: "user", Content: "write a haiku"},
			{Role: "assistant", Content: "Autumn"},
		},
		"reminder-only user message": {
			{Role: "assistant", Content: "Done."},
			{Role: "user", Content: "<system-reminder>The date changed.</system-reminder>"},
		},
		"image-only user message": {
			{Role: "user", Content: []interface{}{map[string]interface{}{"type": "image_url"}}},
		},
	}
	for name, msgs := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tg.Tag("t", msgs); got != nil {
				t.Fatalf("Tag = %+v, want nil", got)
			}
		})
	}
}

func TestTurnStartInput_PairSelection(t *testing.T) {
	t.Run("skips tool steps back to the last agent text", func(t *testing.T) {
		msgs := []types.Message{
			{Role: "user", Content: "fix the flaky test"},
			{Role: "assistant", Content: "Found it. Shall I also add a retry?"},
			{Role: "assistant", ToolCalls: []types.ToolCall{{ID: "c1"}}},
			{Role: "tool", ToolCallID: "c1", Content: "edited"},
			// an interrupted loop: the human's text right after a tool result
			{Role: "user", Content: "stop, don't add a retry"},
		}
		in, _, ok := turnStartInput(msgs)
		if !ok || in.Prompt != "stop, don't add a retry" || in.PrevAssistant != "Found it. Shall I also add a retry?" {
			t.Fatalf("ok=%v in=%+v", ok, in)
		}
	})
	t.Run("an earlier human prompt ends the walk", func(t *testing.T) {
		msgs := []types.Message{
			{Role: "assistant", Content: "Want me to deploy?"},
			{Role: "user", Content: "not yet"},
			{Role: "assistant", ToolCalls: []types.ToolCall{{ID: "c1"}}},
			{Role: "tool", ToolCallID: "c1", Content: "x"},
			{Role: "user", Content: "ok now"},
		}
		in, _, ok := turnStartInput(msgs)
		if !ok || in.PrevAssistant != "" {
			t.Fatalf("PrevAssistant = %q, want empty (no agent text since the last human prompt)", in.PrevAssistant)
		}
	})
	t.Run("strips reminders from block content", func(t *testing.T) {
		msgs := []types.Message{
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "<system-reminder>project rules here</system-reminder>"},
				map[string]interface{}{"type": "text", "text": "what is the status of OPS-12?"},
			}},
		}
		in, _, ok := turnStartInput(msgs)
		if !ok || strings.Contains(in.Prompt, "project rules") || !strings.Contains(in.Prompt, "OPS-12") {
			t.Fatalf("ok=%v prompt=%q", ok, in.Prompt)
		}
	})
	t.Run("typed content parts", func(t *testing.T) {
		msgs := []types.Message{
			{Role: "assistant", Content: []types.ContentPart{{Type: "text", Text: "Which one?"}}},
			{Role: "user", Content: []types.ContentPart{{Type: "text", Text: "do 1"}}},
		}
		in, _, ok := turnStartInput(msgs)
		if !ok || in.Prompt != "do 1" || in.PrevAssistant != "Which one?" {
			t.Fatalf("ok=%v in=%+v", ok, in)
		}
	})
	t.Run("caps long inputs on rune boundaries", func(t *testing.T) {
		long := strings.Repeat("é", maxPromptBytes) // 2 bytes per rune
		msgs := []types.Message{
			{Role: "assistant", Content: long + " Want me to?"},
			{Role: "user", Content: "yes " + long},
		}
		in, truncated, ok := turnStartInput(msgs)
		if !ok || !truncated {
			t.Fatalf("ok=%v truncated=%v", ok, truncated)
		}
		if len(in.Prompt) > maxPromptBytes || len(in.PrevAssistant) > maxPrevBytes {
			t.Fatalf("caps not applied: %d, %d", len(in.Prompt), len(in.PrevAssistant))
		}
		if !strings.HasPrefix(in.Prompt, "yes ") || !strings.HasSuffix(in.PrevAssistant, "Want me to?") {
			t.Fatal("prompt must keep its head and the previous message its tail")
		}
		if !utf8Valid(in.Prompt) || !utf8Valid(in.PrevAssistant) {
			t.Fatal("cut split a rune")
		}
	})
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

func TestTag_PanicIsRecoveredAndTrips(t *testing.T) {
	tg := New(Config{Enabled: true, Tenants: []string{"t"}, MaxStrikes: 2}, quietLog())
	tg.extract = func(workload.TurnStartInput) workload.TurnStartFlags { panic("boom: secret prompt text") }

	for i := 0; i < 2; i++ {
		var got *events.SDLCTurnStart
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic escaped Tag: %v", r)
				}
			}()
			got = tg.Tag("t", approvalPair())
		}()
		if got != nil {
			t.Fatalf("call %d: Tag = %+v after a panic, want nil", i, got)
		}
	}
	if !tg.Tripped() || tg.Active("t") {
		t.Fatal("two consecutive panics must trip the tagger off")
	}
}

func TestTag_OverBudgetAutoOff(t *testing.T) {
	var calls atomic.Int32
	tg := New(Config{Enabled: true, Tenants: []string{"t"}, Budget: 50 * time.Microsecond, MaxStrikes: 3}, quietLog())
	tg.extract = func(in workload.TurnStartInput) workload.TurnStartFlags {
		calls.Add(1)
		time.Sleep(2 * time.Millisecond)
		return workload.ExtractTurnStart(in)
	}

	for i := 1; i <= 3; i++ {
		// An overrun still returns its result: the time is already spent.
		if got := tg.Tag("t", approvalPair()); got == nil {
			t.Fatalf("call %d: over-budget result dropped", i)
		}
		if wantTripped := i == 3; tg.Tripped() != wantTripped {
			t.Fatalf("after %d overruns Tripped=%v, want %v", i, tg.Tripped(), wantTripped)
		}
	}
	if got := tg.Tag("t", approvalPair()); got != nil {
		t.Fatalf("tripped tagger returned %+v", got)
	}
	if calls.Load() != 3 {
		t.Fatalf("extractor calls = %d, want 3 (none after tripping)", calls.Load())
	}
}

func TestTag_SuccessResetsStrikes(t *testing.T) {
	slow := true
	tg := New(Config{Enabled: true, Tenants: []string{"t"}, Budget: 50 * time.Microsecond, MaxStrikes: 2}, quietLog())
	tg.extract = func(in workload.TurnStartInput) workload.TurnStartFlags {
		if slow {
			time.Sleep(2 * time.Millisecond)
		}
		return workload.ExtractTurnStart(in)
	}
	for _, s := range []bool{true, false, true, false, true} {
		slow = s
		tg.budget = 50 * time.Microsecond
		if !s {
			tg.budget = time.Second // a guaranteed success
		}
		tg.Tag("t", approvalPair())
	}
	if tg.Tripped() {
		t.Fatal("non-consecutive overruns must not trip the tagger")
	}
}

// The event must carry flags only: booleans, counts and closed enums. A
// string field is a place text could leak, so the allowed ones are named.
func TestSDLCTurnStartCarriesNoText(t *testing.T) {
	allowedStrings := map[string]bool{"ExtractorVersion": true, "PromptLen": true}
	typ := reflect.TypeOf(events.SDLCTurnStart{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		switch f.Type.Kind() {
		case reflect.Bool, reflect.Int:
		case reflect.String:
			if !allowedStrings[f.Name] {
				t.Errorf("field %s is a free string; only enums and versions may be strings", f.Name)
			}
		default:
			t.Errorf("field %s has kind %s; only bool, int and enum strings are allowed", f.Name, f.Type.Kind())
		}
		lower := strings.ToLower(f.Name)
		if strings.Contains(lower, "act") && f.Name != "ExtractorVersion" || strings.Contains(lower, "confidence") {
			t.Errorf("field %s looks like a turn act; the axis has not graduated", f.Name)
		}
	}

	// And end to end: no word of the prompt or previous message survives.
	tg := New(on("t"), quietLog())
	msgs := []types.Message{
		{Role: "assistant", Content: "Zanzibarquux report ready. Want me to file it under PLUGH-77?"},
		{Role: "user", Content: "yes, and xyzzy-frobnicate the wombatlicious config"},
	}
	ev := tg.Tag("t", msgs)
	if ev == nil {
		t.Fatal("Tag = nil")
	}
	b, _ := json.Marshal(ev)
	for _, word := range []string{"zanzibarquux", "plugh", "xyzzy", "frobnicate", "wombatlicious", "config", "report"} {
		if strings.Contains(strings.ToLower(string(b)), word) {
			t.Errorf("event JSON contains %q: %s", word, b)
		}
	}
	if ev.TicketIDsPrev != 1 {
		t.Errorf("TicketIDsPrev = %d, want 1 (counted, not copied)", ev.TicketIDsPrev)
	}
}

// Every TurnStartFlags field must reach the event under the same name and
// kind. Fails when go-aiqg-workload adds a flag that toEvent does not map.
func TestEventMirrorsFlags(t *testing.T) {
	src := reflect.TypeOf(workload.TurnStartFlags{})
	dst := reflect.TypeOf(events.SDLCTurnStart{})
	mapped := 0
	for i := 0; i < src.NumField(); i++ {
		f := src.Field(i)
		if f.Name == "Version" {
			continue // carried as ExtractorVersion
		}
		g, ok := dst.FieldByName(f.Name)
		if !ok {
			t.Errorf("TurnStartFlags.%s has no field on events.SDLCTurnStart", f.Name)
			continue
		}
		if g.Type.Kind() != f.Type.Kind() {
			t.Errorf("%s: kind %s on the event, %s on the flags", f.Name, g.Type.Kind(), f.Type.Kind())
		}
		mapped++
	}
	// ExtractorVersion and Truncated are the gateway's own two fields.
	if want := mapped + 2; dst.NumField() != want {
		t.Errorf("events.SDLCTurnStart has %d fields, want %d (flags + ExtractorVersion + Truncated)", dst.NumField(), want)
	}

	// toEvent must copy values, not just have matching names: set every
	// bool and int on the source and check the event carries each one.
	var f workload.TurnStartFlags
	fv := reflect.ValueOf(&f).Elem()
	for i := 0; i < fv.NumField(); i++ {
		switch fv.Field(i).Kind() {
		case reflect.Bool:
			fv.Field(i).SetBool(true)
		case reflect.Int:
			fv.Field(i).SetInt(int64(i + 1))
		}
	}
	ev := reflect.ValueOf(*toEvent(f, false))
	for i := 0; i < fv.NumField(); i++ {
		name := src.Field(i).Name
		if name == "Version" || name == "PromptLen" {
			continue
		}
		if got, want := ev.FieldByName(name).Interface(), fv.Field(i).Interface(); got != want {
			t.Errorf("toEvent dropped %s: got %v want %v", name, got, want)
		}
	}
}

func BenchmarkTag(b *testing.B) {
	tg := New(Config{Enabled: true, Tenants: []string{"t"}, Budget: time.Hour}, quietLog())
	typical := approvalPair()
	bigPaste := strings.Repeat("panic: runtime error at handler.go:88 OPS-1 ", 4000) // ~180 KiB
	worst := []types.Message{
		{Role: "assistant", Content: strings.Repeat("1. step done\n", 5000) + "Want me to continue?"},
		{Role: "user", Content: "why did you do that? " + bigPaste},
	}
	b.Run("typical", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			tg.Tag("t", typical)
		}
	})
	b.Run("capped_worst_case", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			tg.Tag("t", worst)
		}
	})
}
