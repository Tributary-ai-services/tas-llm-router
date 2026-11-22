package gatekeeper

// Scan-once (Plan #22 GK-5a).
//
// The gateway used to scan the whole conversation on every turn. A chat
// client resends the full history each time, so a card number pasted in
// turn 1 was scanned again on turns 2..N and counted as a new finding every
// time, inflating Data safety by the length of the conversation. A tool
// result was scanned once at the MCP proxy and again here.
//
// Scan-once remembers, per tenant, what each message's content produced the
// last time it was scanned:
//
//   - remembered clean        -> not scanned again
//   - remembered with findings -> scanned again, so blocking and redaction
//     still act on it, but its findings are not COUNTED again
//   - not remembered          -> scanned, counted, remembered
//
// Fail-safe in one direction only: if the store is slow (over its budget),
// unreachable or returns garbage, everything is scanned and counted as
// before. A missing record can only cost time, never a missed finding.
//
// The key carries everything that changes the answer: tenant (another
// tenant with the same content never skips), the content hash after
// normalisation, profile, trust tier, the Gatekeeper rule-set digest and the
// gateway build. A rule change or a deploy therefore re-scans everything once.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/redis/go-redis/v9"
	"golang.org/x/text/unicode/norm"

	"github.com/Tributary-ai-services/Gatekeeper/pkg/pipeline"
	"github.com/Tributary-ai-services/Gatekeeper/pkg/scan"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// ScanOnceConfig configures the scan-once store. Off by default: turning it
// on lowers Data-safety finding counts (each finding is counted once per
// conversation instead of once per turn), which must be announced first.
type ScanOnceConfig struct {
	Enabled  bool          `yaml:"enabled"`
	RedisURL string        `yaml:"redis_url"`
	TTL      time.Duration `yaml:"ttl"`     // refreshed on every hit; default 24h
	Timeout  time.Duration `yaml:"timeout"` // lookup budget; default 20ms
	// BuildID is the gateway build, folded into every key so a deploy
	// (which may change matcher code the rule digest cannot see) re-scans.
	BuildID string `yaml:"-"`
}

const (
	defaultScanOnceTTL     = 24 * time.Hour
	defaultScanOnceTimeout = 20 * time.Millisecond
	scanOnceKeyPrefix      = "gk:scan1:"
)

// ScanRecord is what is remembered about one message's content.
type ScanRecord struct {
	// Patterns are the pattern ids found, sorted; empty means clean.
	Patterns []string `json:"p,omitempty"`
}

// Clean reports whether the content produced no findings.
func (r ScanRecord) Clean() bool { return len(r.Patterns) == 0 }

// ScanOnceStore remembers scan outcomes. Implementations must treat every
// error as "not remembered".
type ScanOnceStore interface {
	// GetMany returns one entry per key, nil where nothing is remembered.
	GetMany(ctx context.Context, keys []string) ([]*ScanRecord, error)
	// SetMany remembers records and refreshes the TTL of keys in touch.
	SetMany(ctx context.Context, recs map[string]ScanRecord, touch []string, ttl time.Duration) error
}

// redisScanOnceStore is the shared store: every gateway replica sees the
// same records, so a conversation that moves between pods still skips.
type redisScanOnceStore struct{ rdb redis.UniversalClient }

// NewRedisScanOnceStore connects to url (redis://...).
func NewRedisScanOnceStore(url string) (ScanOnceStore, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	return &redisScanOnceStore{rdb: redis.NewClient(opt)}, nil
}

func (s *redisScanOnceStore) GetMany(ctx context.Context, keys []string) ([]*ScanRecord, error) {
	out := make([]*ScanRecord, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	vals, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	for i, v := range vals {
		str, ok := v.(string)
		if !ok {
			continue
		}
		var rec ScanRecord
		if json.Unmarshal([]byte(str), &rec) != nil {
			continue // unreadable = not remembered = scan
		}
		out[i] = &rec
	}
	return out, nil
}

func (s *redisScanOnceStore) SetMany(ctx context.Context, recs map[string]ScanRecord, touch []string, ttl time.Duration) error {
	if len(recs) == 0 && len(touch) == 0 {
		return nil
	}
	pipe := s.rdb.Pipeline()
	for k, rec := range recs {
		b, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		pipe.Set(ctx, k, b, ttl)
	}
	for _, k := range touch {
		pipe.Expire(ctx, k, ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// normalizeForScanKey is the only normalisation applied before hashing:
// Unicode NFC and trailing whitespace trimmed. Anything more aggressive
// (case folding, collapsing spaces) could make two contents share a key
// while scanning differently.
func normalizeForScanKey(s string) string {
	return strings.TrimRightFunc(norm.NFC.String(s), unicode.IsSpace)
}

func scanOnceKey(tenant string, content []byte, profile scan.ScanProfile, tier scan.TrustTier, ruleset, build string) string {
	sum := sha256.Sum256([]byte(normalizeForScanKey(string(content))))
	return scanOnceKeyPrefix + tenant + ":" + hex.EncodeToString(sum[:]) + ":" +
		string(profile) + ":" + strconv.Itoa(int(tier)) + ":" + ruleset + ":" + build
}

// InboundScan is the result of scanning a request's messages.
type InboundScan struct {
	// Result is the merged scan over every message that was scanned. It
	// drives blocking and enforcement, so it includes findings that were
	// already counted on an earlier turn. Never nil when err is nil.
	Result *pipeline.ProcessResult
	// Countable are the findings to count toward Data safety: all of
	// Result's findings, minus those in content remembered as already
	// having them.
	Countable []scan.Finding
	// Messages is how many messages were eligible for scanning.
	Messages int
	// Skipped is how many were not scanned because they were remembered
	// clean. Hits counts every remembered message, clean or not.
	Skipped, Hits int
	// ScanMs is wall time spent scanning (excluding the store lookup).
	ScanMs float64
	// StoreUsed is true when scan-once consulted the store successfully.
	StoreUsed bool
	// ToolResults attributes the scan to tool results (role=tool messages,
	// which Anthropic tool_result blocks become), keyed by tool_call_id.
	// Filled only on the per-message path (scan-once on), the only path that
	// scans each message on its own; nil otherwise. Plan #22 T.5 credits
	// these to the tool call the result answers.
	ToolResults map[string]ToolResultScan
}

// ToolResultScan is what the inbound scan found in one tool result.
type ToolResultScan struct {
	// Findings are the COUNTABLE findings by pattern id: the same set that
	// reaches the response event's tag_findings, so a finding credited to a
	// tool call is never counted a second time there.
	Findings map[string]int
	// Skipped is true when the content was remembered clean and not scanned.
	Skipped bool
}

func (in *InboundScan) recordToolResult(toolCallID string, findings []scan.Finding, skipped bool) {
	if toolCallID == "" {
		return
	}
	if in.ToolResults == nil {
		in.ToolResults = map[string]ToolResultScan{}
	}
	tr, ok := in.ToolResults[toolCallID]
	if !ok {
		tr = ToolResultScan{Findings: map[string]int{}, Skipped: skipped}
	} else {
		tr.Skipped = tr.Skipped && skipped
	}
	for _, f := range findings {
		tr.Findings[f.PatternID]++
	}
	in.ToolResults[toolCallID] = tr
}

// SetScanOnceStore enables scan-once with the given store. A nil store
// disables it.
func (c *Client) SetScanOnceStore(store ScanOnceStore, cfg ScanOnceConfig) {
	if cfg.TTL <= 0 {
		cfg.TTL = defaultScanOnceTTL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultScanOnceTimeout
	}
	c.scanOnce = store
	c.scanOnceCfg = cfg
	if v, ok := c.scanner.(scan.RulesetVersioner); ok {
		c.rulesetVersion = v.RulesetVersion()
	} else {
		c.rulesetVersion = "unknown"
	}
}

// ScanOnceEnabled reports whether a scan-once store is configured.
func (c *Client) ScanOnceEnabled() bool { return c != nil && c.scanOnce != nil }

// ScanInbound scans the request's messages. Without a scan-once store it is
// exactly ScanMessages, with every finding countable. With one, each
// eligible message is looked up and scanned on its own, as described at the
// top of this file.
func (c *Client) ScanInbound(ctx context.Context, messages []types.Message, meta ScanMeta) (*InboundScan, error) {
	if !c.ScanOnceEnabled() || meta.TenantID == "" {
		// No tenant means no key we could safely share: scan everything.
		start := time.Now()
		res, err := c.ScanMessages(ctx, messages, meta)
		if err != nil {
			return nil, err
		}
		out := &InboundScan{Result: res, ScanMs: msSince(start)}
		if res != nil && res.ScanResult != nil {
			out.Countable = res.ScanResult.Findings
		}
		return out, nil
	}

	policy := c.config.Inbound.ScanPolicy
	if len(policy.AlwaysScanRoles) == 0 && len(policy.NeverScanRoles) == 0 {
		policy = DefaultScanPolicy()
	}
	profile := parseScanProfile(c.config.Inbound.ScanProfile)
	tier := parseTrustTier(c.config.Inbound.TrustTier)

	type item struct {
		content    []byte
		key        string
		toolCallID string
	}
	var items []item
	for _, msg := range messages {
		if !shouldScanMessage(msg, policy) {
			continue
		}
		content := extractMessagesText([]types.Message{msg})
		if len(content) == 0 {
			continue
		}
		items = append(items, item{content: content, toolCallID: msg.ToolCallID,
			key: scanOnceKey(meta.TenantID, content, profile, tier, c.rulesetVersion, c.scanOnceCfg.BuildID)})
	}

	out := &InboundScan{Messages: len(items)}
	if len(items) == 0 {
		out.Result = &pipeline.ProcessResult{Skipped: true, SkipReason: "all_messages_pre_scanned_or_empty"}
		return out, nil
	}

	keys := make([]string, len(items))
	for i, it := range items {
		keys[i] = it.key
	}
	lctx, cancel := context.WithTimeout(ctx, c.scanOnceCfg.Timeout)
	recs, lerr := c.scanOnce.GetMany(lctx, keys)
	cancel()
	if lerr != nil || len(recs) != len(items) {
		recs = make([]*ScanRecord, len(items)) // fail safe: nothing remembered
		if lerr != nil && c.logger != nil {
			c.logger.WithError(lerr).Debug("scan-once lookup failed; scanning everything")
		}
	} else {
		out.StoreUsed = true
	}

	merged := &scan.ScanResult{}
	newRecs := map[string]ScanRecord{}
	var touch []string
	start := time.Now()
	for i, it := range items {
		prev := recs[i]
		if prev != nil {
			out.Hits++
			touch = append(touch, it.key)
			if prev.Clean() {
				out.Skipped++
				out.recordToolResult(it.toolCallID, nil, true)
				continue
			}
		}

		req := pipeline.ProcessRequest{
			Content:        it.content,
			ContentType:    "chat",
			TrustTier:      tier,
			ScanProfile:    profile,
			TenantID:       meta.TenantID,
			RequestID:      meta.RequestID,
			UserID:         meta.UserID,
			Source:         meta.Source,
			SkipExtraction: true,
			SkipStreaming:  true,
		}
		res, err := c.processor.Process(ctx, req)
		if err != nil {
			return nil, err
		}
		var findings []scan.Finding
		if res != nil && res.ScanResult != nil {
			findings = res.ScanResult.Findings
			mergeScanResult(merged, res.ScanResult)
		}
		if res != nil && res.ActionResult != nil && res.ActionResult.Blocked {
			out.Result = res // keep an action-level block visible to ShouldBlock
		}

		if prev == nil {
			out.Countable = append(out.Countable, findings...)
			out.recordToolResult(it.toolCallID, findings, false)
			newRecs[it.key] = ScanRecord{Patterns: patternIDs(findings)}
		} else {
			// Content we have seen with findings: count only patterns that
			// were not there before (they can appear after a rule change
			// only via a new key, so in practice this adds nothing; it keeps
			// the rule "once per (tenant, content, pattern)" exact).
			seen := map[string]bool{}
			for _, p := range prev.Patterns {
				seen[p] = true
			}
			var fresh []scan.Finding
			for _, f := range findings {
				if !seen[f.PatternID] {
					fresh = append(fresh, f)
				}
			}
			out.Countable = append(out.Countable, fresh...)
			out.recordToolResult(it.toolCallID, fresh, false)
		}
	}
	out.ScanMs = msSince(start)

	if out.Result == nil {
		out.Result = &pipeline.ProcessResult{}
	}
	// A request whose every message was remembered clean still "ran" a scan
	// in the sense that matters: its content is known clean. Report an empty
	// ScanResult rather than Skipped, so Assurance scores 100, not nil.
	out.Result.ScanResult = merged
	out.Result.Skipped = false

	if len(newRecs) > 0 || len(touch) > 0 {
		// Best effort, off the request path's critical section but bounded.
		sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 250*time.Millisecond)
		if err := c.scanOnce.SetMany(sctx, newRecs, touch, c.scanOnceCfg.TTL); err != nil && c.logger != nil {
			c.logger.WithError(err).Debug("scan-once record failed; next turn will scan again")
		}
		scancel()
	}
	return out, nil
}

// RememberedClean reports, for each message, whether scan-once remembers it
// as clean. Redaction uses it to skip re-scanning content it knows holds
// nothing to redact. Any store problem returns all false (scan everything).
func (c *Client) RememberedClean(ctx context.Context, messages []types.Message, meta ScanMeta) []bool {
	out := make([]bool, len(messages))
	if !c.ScanOnceEnabled() || meta.TenantID == "" {
		return out
	}
	profile := parseScanProfile(c.config.Inbound.ScanProfile)
	tier := parseTrustTier(c.config.Inbound.TrustTier)
	idx := []int{}
	keys := []string{}
	for i, msg := range messages {
		content := extractMessagesText([]types.Message{msg})
		if len(content) == 0 {
			continue
		}
		idx = append(idx, i)
		keys = append(keys, scanOnceKey(meta.TenantID, content, profile, tier, c.rulesetVersion, c.scanOnceCfg.BuildID))
	}
	lctx, cancel := context.WithTimeout(ctx, c.scanOnceCfg.Timeout)
	defer cancel()
	recs, err := c.scanOnce.GetMany(lctx, keys)
	if err != nil || len(recs) != len(keys) {
		return out
	}
	for j, rec := range recs {
		if rec != nil && rec.Clean() {
			out[idx[j]] = true
		}
	}
	return out
}

func patternIDs(fs []scan.Finding) []string {
	if len(fs) == 0 {
		return nil
	}
	set := map[string]bool{}
	for _, f := range fs {
		set[f.PatternID] = true
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// mergeScanResult folds one message's result into the request's result.
// Only the fields the gateway reads are merged: findings (blocking,
// enforcement, counts) and the highest risk score.
func mergeScanResult(dst, src *scan.ScanResult) {
	dst.Findings = append(dst.Findings, src.Findings...)
	if src.RiskScore > dst.RiskScore {
		dst.RiskScore = src.RiskScore
	}
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}
