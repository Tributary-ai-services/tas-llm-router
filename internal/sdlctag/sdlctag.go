// Package sdlctag is SDLC mode A: inline turn-start tagging in the gateway's
// request path (tas-session-corpus docs/DESIGN-continuous-labeling.md §3,
// "Mode A: permission and guardrails").
//
// When a request starts a human turn, the tagger reads the human's prompt and
// the agent message it replies to, runs go-aiqg-workload's ExtractTurnStart,
// and returns booleans, counts and a length bucket for the AIQG event. It
// never returns text, and it never returns a turn act: that axis has not
// graduated (P1 measured 50% exact agreement on held-out control turns against
// an 85% lower-bound bar), so the provisional act stays out of the event.
//
// The rules it is built under, each enforced here or by a test:
//   - Off by default, at two levels. Config.Enabled is the global switch and
//     Config.Tenants the per-tenant one; both must allow a request. While off,
//     the extractor is never called and the event carries no mode A field.
//   - Tag-only and fail-open. Tag returns a value or nil, never an error, and
//     nothing it returns can block, alter or delay the request beyond the
//     extractor's own wall time.
//   - Panic recovery and a latency budget (default 1 ms). A panic or an
//     overrun is a strike; MaxStrikes consecutive strikes switch the tagger
//     off for the life of the process (aiqg_sdlc_tag_tripped=1).
//   - No policy hooks. Nothing in the gateway reads the tag back.
package sdlctag

import (
	"fmt"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	workload "github.com/Tributary-ai-services/aether-shared/go-aiqg-workload"
	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
)

const (
	// DefaultBudget is the design target for the extractor's wall time.
	DefaultBudget = time.Millisecond
	// DefaultMaxStrikes is how many CONSECUTIVE panics or overruns trip the
	// tagger off. Consecutive, so one GC pause cannot trip it; a success
	// resets the count.
	DefaultMaxStrikes = 3

	// Read caps. The extractor scans its inputs once per cue list and per
	// regexp, so its cost is linear in input length: about 30 µs per KiB on
	// the dev laptop (BenchmarkTag), which put 32 KiB caps at ~1.9 ms, over
	// budget. Pasted logs make prompts of hundreds of KiB, so a cap is what
	// keeps the worst case inside the budget. The prompt keeps its head
	// (openers, the first question: what the human typed, before any paste)
	// and the previous message its tail (the closing ask or question), which
	// is where the flags look. A capped input sets Truncated on the event, so
	// the offline harness can exclude or re-run those turns.
	maxPromptBytes = 4 << 10
	maxPrevBytes   = 4 << 10

	// maxWalkBack bounds the search for the agent message a prompt replies
	// to. An agent loop can leave hundreds of tool-only messages between the
	// last assistant text and the prompt; past this many, HasPrev is false.
	maxWalkBack = 64
)

// Outcome label values for metrics.SDLCTagTotal.
const (
	OutcomeTagged       = "tagged"
	OutcomeNotTurnStart = "not_turn_start"
	OutcomePanic        = "panic"
	OutcomeOverBudget   = "over_budget"
)

// Config is the mode A switch set. The zero value is off.
type Config struct {
	// Enabled is the global switch (env SDLC_TAG_ENABLED). Default false.
	Enabled bool `yaml:"enabled"`
	// Tenants is the per-tenant switch: tenant ids mode A may tag (env
	// SDLC_TAG_TENANTS, comma-separated). Empty means no tenant, so enabling
	// the global switch alone tags nothing. There is deliberately no
	// wildcard: each tenant is turned on by name once its axis graduates.
	Tenants []string `yaml:"tenants"`
	// Budget is the extractor's wall-time budget. Zero means DefaultBudget.
	Budget time.Duration `yaml:"budget"`
	// MaxStrikes is the consecutive panic/overrun count that trips the
	// tagger off. Zero means DefaultMaxStrikes.
	MaxStrikes int `yaml:"max_strikes"`
}

// Tagger applies Config to requests. A nil *Tagger is valid and always off.
type Tagger struct {
	tenants    map[string]struct{}
	budget     time.Duration
	maxStrikes int32
	log        *logrus.Logger

	strikes atomic.Int32
	tripped atomic.Bool

	// extract is ExtractTurnStart; a seam so tests can inject a panic or a
	// slow extractor.
	extract func(workload.TurnStartInput) workload.TurnStartFlags
}

// New returns a Tagger for cfg, or nil when cfg is off globally or names no
// tenant. Callers keep the nil: every method treats it as off.
func New(cfg Config, log *logrus.Logger) *Tagger {
	if !cfg.Enabled || len(cfg.Tenants) == 0 {
		return nil
	}
	t := &Tagger{
		tenants:    make(map[string]struct{}, len(cfg.Tenants)),
		budget:     cfg.Budget,
		maxStrikes: int32(cfg.MaxStrikes),
		log:        log,
		extract:    workload.ExtractTurnStart,
	}
	for _, id := range cfg.Tenants {
		if id = strings.TrimSpace(id); id != "" {
			t.tenants[id] = struct{}{}
		}
	}
	if len(t.tenants) == 0 {
		return nil
	}
	if t.budget <= 0 {
		t.budget = DefaultBudget
	}
	if t.maxStrikes <= 0 {
		t.maxStrikes = DefaultMaxStrikes
	}
	metrics.SDLCTagTripped.Set(0)
	return t
}

// Active reports whether a request for tenantID may be tagged. It is the
// whole gate: a false here means the messages are never read.
func (t *Tagger) Active(tenantID string) bool {
	if t == nil || tenantID == "" || t.tripped.Load() {
		return false
	}
	_, ok := t.tenants[tenantID]
	return ok
}

// Tripped reports whether the tagger has switched itself off.
func (t *Tagger) Tripped() bool { return t != nil && t.tripped.Load() }

// Tag returns the turn-start flags for a request, or nil when mode A is off
// for tenantID, the request does not start a human turn, or the extractor
// panicked. It never returns an error and never modifies msgs.
func (t *Tagger) Tag(tenantID string, msgs []types.Message) (out *events.SDLCTurnStart) {
	if !t.Active(tenantID) {
		return nil
	}
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			out = nil
			// The panic value is not logged: it could carry prompt text.
			// The type and the stack locate the fault without it.
			if t.log != nil {
				t.log.WithFields(logrus.Fields{
					"event":      "aiqg.sdlc_tag_panic",
					"tenant_id":  tenantID,
					"panic_type": fmt.Sprintf("%T", r),
					"stack":      string(debug.Stack()),
				}).Error("SDLC mode A tagger panicked; request unaffected")
			}
			t.strike(tenantID, OutcomePanic)
		}
	}()

	in, truncated, ok := turnStartInput(msgs)
	if !ok {
		metrics.SDLCTagTotal.WithLabelValues(OutcomeNotTurnStart).Inc()
		return nil
	}
	f := t.extract(in)
	elapsed := time.Since(start)
	metrics.SDLCTagDurationSeconds.Observe(elapsed.Seconds())
	if elapsed > t.budget {
		// The time is already spent, so the result is kept; the strike is
		// what protects the next requests.
		t.strike(tenantID, OutcomeOverBudget)
	} else {
		t.strikes.Store(0)
		metrics.SDLCTagTotal.WithLabelValues(OutcomeTagged).Inc()
	}
	return toEvent(f, truncated)
}

func (t *Tagger) strike(tenantID, outcome string) {
	metrics.SDLCTagTotal.WithLabelValues(outcome).Inc()
	if t.strikes.Add(1) < t.maxStrikes {
		return
	}
	if t.tripped.CompareAndSwap(false, true) {
		metrics.SDLCTagTripped.Set(1)
		if t.log != nil {
			t.log.WithFields(logrus.Fields{
				"event":       "aiqg.sdlc_tag_tripped",
				"tenant_id":   tenantID,
				"last_cause":  outcome,
				"max_strikes": t.maxStrikes,
				"budget":      t.budget.String(),
			}).Warn("SDLC mode A tagger switched itself off on this pod until restart")
		}
	}
}

// turnStartInput picks the pair ExtractTurnStart reads: the human's prompt
// (the last message, role=user, with text once client-injected reminders are
// removed) and the most recent agent text before it. ok=false when the
// request does not start a human turn: the last message is a tool result, an
// assistant prefill, or a user message that carries only injected context.
//
// This is a local, conservative reading of "turn start", not the G3 turn
// boundary (DESIGN §8.6), which will own the definition and its index.
func turnStartInput(msgs []types.Message) (in workload.TurnStartInput, truncated, ok bool) {
	if len(msgs) == 0 {
		return in, false, false
	}
	last := msgs[len(msgs)-1]
	if last.Role != "user" {
		return in, false, false
	}
	prompt := humanText(last.Content)
	if strings.TrimSpace(prompt) == "" {
		return in, false, false
	}

	// Walk back to the latest assistant message with text. Tool results and
	// tool-only assistant steps in between are skipped (an interrupted agent
	// loop puts the human's text right after tool results); another human
	// prompt ends the walk, because the agent never replied to it in text.
	var prev string
	stop := len(msgs) - 1 - maxWalkBack
walk:
	for i := len(msgs) - 2; i >= 0 && i >= stop; i-- {
		m := msgs[i]
		switch m.Role {
		case "assistant":
			if txt := humanText(m.Content); strings.TrimSpace(txt) != "" {
				prev = txt
				break walk
			}
		case "user":
			if strings.TrimSpace(humanText(m.Content)) != "" {
				break walk
			}
		case "system", "developer":
			break walk
		}
	}

	var tp, tv bool
	in.Prompt, tp = head(prompt, maxPromptBytes)
	in.PrevAssistant, tv = tail(prev, maxPrevBytes)
	return in, tp || tv, true
}

// humanText returns a message's text parts, joined, with client-injected
// <system-reminder> spans removed. Claude Code wraps CLAUDE.md, hook output
// and tool reminders that way inside user messages; left in, they would set
// the flags from text no human typed.
func humanText(content interface{}) string {
	var s string
	switch c := content.(type) {
	case string:
		s = c
	case []types.ContentPart:
		var b strings.Builder
		for _, p := range c {
			if p.Type == "text" && p.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(p.Text)
			}
		}
		s = b.String()
	case []interface{}:
		var b strings.Builder
		for _, item := range c {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if typ, _ := m["type"].(string); typ != "" && typ != "text" {
				continue
			}
			if txt, _ := m["text"].(string); txt != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(txt)
			}
		}
		s = b.String()
	}
	return stripReminders(s)
}

const (
	reminderOpen  = "<system-reminder>"
	reminderClose = "</system-reminder>"
)

func stripReminders(s string) string {
	if !strings.Contains(s, reminderOpen) {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, reminderOpen)
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		j := strings.Index(s[i:], reminderClose)
		if j < 0 {
			break // unterminated: drop the rest rather than read it as human text
		}
		s = s[i+j+len(reminderClose):]
	}
	return b.String()
}

// head returns at most n bytes from the start of s, cut on a rune boundary.
func head(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// tail returns at most n bytes from the end of s, cut on a rune boundary.
func tail(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:], true
}

// toEvent copies the flags onto the event shape. One line per flag, so a flag
// added upstream fails TestEventMirrorsFlags until it is mapped here.
func toEvent(f workload.TurnStartFlags, truncated bool) *events.SDLCTurnStart {
	return &events.SDLCTurnStart{
		ExtractorVersion: f.Version,
		PromptLen:        string(f.PromptLen),
		HasPrev:          f.HasPrev,
		Truncated:        truncated,

		StartsApproval: f.StartsApproval,
		StartsNegation: f.StartsNegation,
		SelectionReply: f.SelectionReply,
		HasQuestion:    f.HasQuestion,
		Imperative:     f.Imperative,
		CorrectionCue:  f.CorrectionCue,
		VerifyCue:      f.VerifyCue,
		StatusCue:      f.StatusCue,
		InfoCue:        f.InfoCue,

		PrevEndsQuestion: f.PrevEndsQuestion,
		PrevExplicitAsk:  f.PrevExplicitAsk,
		PrevOffersChoice: f.PrevOffersChoice,
		PrevStatusList:   f.PrevStatusList,

		TicketIDsPrompt: f.TicketIDsPrompt,
		TicketIDsPrev:   f.TicketIDsPrev,
	}
}
