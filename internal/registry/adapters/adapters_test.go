package adapters

import (
	"context"
	"errors"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func quiet() *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.FatalLevel)
	return l
}

// --- stubs -----------------------------------------------------------------

type stubLister struct {
	ids   []string
	err   error
	calls int
}

func (s *stubLister) ListModelIDs(_ context.Context) ([]string, error) {
	s.calls++
	return s.ids, s.err
}

type stubProber struct {
	result map[string]bool
	errs   map[string]error
}

func (s *stubProber) ProbeModel(_ context.Context, model string) (bool, error) {
	if e := s.errs[model]; e != nil {
		return false, e
	}
	return s.result[model], nil
}

// --- OpenAI ----------------------------------------------------------------

func TestOpenAI_DiscoverFiltersAndEnriches(t *testing.T) {
	lister := &stubLister{ids: []string{
		"gpt-4o", "gpt-3.5-turbo", "o1-mini", // chat
		"text-embedding-3-small", "whisper-1", "dall-e-3", "gpt-4o-realtime-preview", "tts-1", // not chat
	}}
	static := []types.ModelInfo{{Name: "gpt-4o", InputCostPer1K: 0.005, OutputCostPer1K: 0.015}}
	a := NewOpenAIAdapter(lister, static, quiet())

	models, err := a.DiscoverModels(context.Background())
	if err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}
	got := map[string]types.ModelInfo{}
	for _, m := range models {
		got[m.Name] = m
	}
	if len(got) != 3 {
		t.Fatalf("discovered %d models, want 3 (gpt-4o, gpt-3.5-turbo, o1-mini); got %v", len(got), keys(got))
	}
	for _, id := range []string{"gpt-4o", "gpt-3.5-turbo", "o1-mini"} {
		m, ok := got[id]
		if !ok {
			t.Errorf("missing chat model %q", id)
			continue
		}
		if m.Status != types.ModelStatusActive {
			t.Errorf("%q status = %q, want active", id, m.Status)
		}
		if m.LastValidated.IsZero() {
			t.Errorf("%q LastValidated not stamped", id)
		}
	}
	// Enrichment: gpt-4o carries the static pricing; the synthesized ones don't.
	if got["gpt-4o"].InputCostPer1K != 0.005 {
		t.Errorf("gpt-4o not enriched from static config: cost=%v", got["gpt-4o"].InputCostPer1K)
	}
	for _, bad := range []string{"text-embedding-3-small", "whisper-1", "dall-e-3", "gpt-4o-realtime-preview", "tts-1"} {
		if _, ok := got[bad]; ok {
			t.Errorf("non-chat model %q leaked into discovery", bad)
		}
	}
}

func TestOpenAI_ValidateModel(t *testing.T) {
	a := NewOpenAIAdapter(&stubLister{ids: []string{"gpt-4o", "o1-mini"}}, nil, quiet())
	ctx := context.Background()
	if ok, _ := a.ValidateModel(ctx, "gpt-4o"); !ok {
		t.Error("gpt-4o should validate")
	}
	if ok, _ := a.ValidateModel(ctx, "gpt-9-imaginary"); ok {
		t.Error("unknown model should not validate")
	}
}

func TestOpenAI_ResolveAliasLatestAndIdentity(t *testing.T) {
	a := NewOpenAIAdapter(&stubLister{ids: []string{"gpt-4o", "gpt-4o-mini", "gpt-4-turbo", "gpt-3.5-turbo"}}, nil, quiet())
	ctx := context.Background()

	got, err := a.ResolveAlias(ctx, "gpt-4-latest")
	if err != nil {
		t.Fatal(err)
	}
	if got == "gpt-4-latest" || !matchesFamily(got, "gpt-4") {
		t.Errorf("gpt-4-latest resolved to %q; want a concrete gpt-4 family model", got)
	}
	// A concrete id is returned unchanged.
	if got, _ := a.ResolveAlias(ctx, "gpt-4o"); got != "gpt-4o" {
		t.Errorf("concrete id changed: %q", got)
	}
	// A -latest with no family match falls back to identity.
	if got, _ := a.ResolveAlias(ctx, "zzz-latest"); got != "zzz-latest" {
		t.Errorf("unmatched -latest changed: %q", got)
	}
}

func TestOpenAI_ListerErrorPropagates(t *testing.T) {
	boom := errors.New("api down")
	a := NewOpenAIAdapter(&stubLister{err: boom}, nil, quiet())
	ctx := context.Background()
	if _, err := a.DiscoverModels(ctx); !errors.Is(err, boom) {
		t.Errorf("DiscoverModels err = %v, want wrapped api down", err)
	}
	if _, err := a.ValidateModel(ctx, "gpt-4o"); !errors.Is(err, boom) {
		t.Errorf("ValidateModel err = %v, want wrapped api down", err)
	}
	// ResolveAlias degrades to identity but surfaces the error.
	got, err := a.ResolveAlias(ctx, "gpt-4-latest")
	if got != "gpt-4-latest" || !errors.Is(err, boom) {
		t.Errorf("ResolveAlias on error = (%q, %v), want identity + wrapped error", got, err)
	}
}

// --- Anthropic -------------------------------------------------------------

func TestAnthropic_DiscoverStampsStatusAndSpareTransient(t *testing.T) {
	known := []types.ModelInfo{
		{Name: "claude-sonnet-4-5"},
		{Name: "claude-old"},
		{Name: "claude-flaky"},
	}
	prober := &stubProber{
		result: map[string]bool{"claude-sonnet-4-5": true, "claude-old": false},
		errs:   map[string]error{"claude-flaky": errors.New("timeout")},
	}
	a := NewAnthropicAdapter(prober, known, quiet())

	models, err := a.DiscoverModels(context.Background())
	if err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}
	got := map[string]types.ModelInfo{}
	for _, m := range models {
		got[m.Name] = m
	}
	if got["claude-sonnet-4-5"].Status != types.ModelStatusActive {
		t.Errorf("active model status = %q", got["claude-sonnet-4-5"].Status)
	}
	if got["claude-old"].Status != types.ModelStatusUnavailable {
		t.Errorf("rejected model status = %q, want unavailable", got["claude-old"].Status)
	}
	// Transient probe error must NOT downgrade: status untouched, not stamped.
	if got["claude-flaky"].Status == types.ModelStatusUnavailable {
		t.Error("a transient probe error wrongly marked the model unavailable")
	}
	if !got["claude-flaky"].LastValidated.IsZero() {
		t.Error("a transient probe error should not stamp LastValidated")
	}
}

func TestAnthropic_ValidateDelegatesToProbe(t *testing.T) {
	prober := &stubProber{result: map[string]bool{"claude-sonnet-4-5": true}}
	a := NewAnthropicAdapter(prober, nil, quiet())
	ctx := context.Background()
	if ok, _ := a.ValidateModel(ctx, "claude-sonnet-4-5"); !ok {
		t.Error("known-good model should validate")
	}
	if ok, _ := a.ValidateModel(ctx, "claude-gone"); ok {
		t.Error("unprobed model should not validate")
	}
}

func TestAnthropic_ResolveLatestNewestOfFamily(t *testing.T) {
	known := []types.ModelInfo{
		{Name: "claude-sonnet-4-5"},
		{Name: "claude-3-5-sonnet-20241022"}, // older naming order
		{Name: "claude-haiku-4-5-20251001"},
		{Name: "claude-opus-4-8"},
	}
	a := NewAnthropicAdapter(&stubProber{}, known, quiet())
	ctx := context.Background()

	if got, _ := a.ResolveAlias(ctx, "claude-sonnet-latest"); got != "claude-sonnet-4-5" {
		t.Errorf("claude-sonnet-latest = %q, want claude-sonnet-4-5 (newest of family, both naming orders considered)", got)
	}
	if got, _ := a.ResolveAlias(ctx, "claude-haiku-latest"); got != "claude-haiku-4-5-20251001" {
		t.Errorf("claude-haiku-latest = %q, want claude-haiku-4-5-20251001", got)
	}
	// Non-alias returned unchanged.
	if got, _ := a.ResolveAlias(ctx, "claude-opus-4-8"); got != "claude-opus-4-8" {
		t.Errorf("concrete id changed: %q", got)
	}
}

func TestProviderNames(t *testing.T) {
	if NewOpenAIAdapter(&stubLister{}, nil, quiet()).GetProviderName() != "openai" {
		t.Error("openai name")
	}
	if NewAnthropicAdapter(&stubProber{}, nil, quiet()).GetProviderName() != "anthropic" {
		t.Error("anthropic name")
	}
}

// Both adapters satisfy the ProviderAdapter interface.
var (
	_ ProviderAdapter = (*OpenAIAdapter)(nil)
	_ ProviderAdapter = (*AnthropicAdapter)(nil)
)

func keys(m map[string]types.ModelInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
