// Package workflow heuristically classifies an incoming chat request
// into one of the AIQG workflow types so the CLEAR latency scorer
// uses a sensible SLA. Customer-supplied TAS-Workflow headers always
// win; this code only fires when no header was set.
//
// Classifier philosophy:
//   - Be quick (called on the hot path of every request).
//   - Be CONSERVATIVE about over-claiming code_generation /
//     summarization / agentic — when in doubt, return "" and let the
//     event carry an unknown workflow (3s SLA per pkg/clear). False
//     negatives are far cheaper than false positives because the
//     "wrong" SLA distorts the dashboard chart.
//   - Don't read message body content twice; the caller passes the
//     already-decoded ChatRequest.
//
// Heuristic priority (first match wins):
//   1. tools / functions present → agentic
//   2. ANY message looks like a summarization prompt → summarization
//   3. ANY message looks like a code-generation prompt → code_generation
//   4. ANY message looks like classification / extraction → classification_extraction
//   5. prompt contains >=3 RAG-style separators → rag
//   6. exactly one user message under 500 chars → single_turn_qa
//   7. otherwise → "" (unknown, falls through to default SLA)
package workflow

import (
	"regexp"
	"strings"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// Workflow enum values. Must match validWorkflows in
// internal/middleware/aiqg_headers.go and slaMsForWorkflow in
// pkg/clear/clear.go. We keep the strings here as constants rather
// than re-importing to avoid a middleware → workflow → middleware
// import cycle.
const (
	WorkflowSingleTurnQA            = "single_turn_qa"
	WorkflowRAG                     = "rag"
	WorkflowAgentic                 = "agentic"
	WorkflowSummarization           = "summarization"
	WorkflowCodeGeneration          = "code_generation"
	WorkflowClassificationExtract   = "classification_extraction"

	// Single-turn QA upper bound on user-message length. Anything
	// longer than this is "not a quick question" and falls through to
	// later rules or unknown.
	singleTurnQAMaxChars = 500
	// RAG requires at least N separator hits to be confident this is
	// retrieval-augmented context rather than incidental document
	// references.
	ragMinSeparators = 3
)

// Separator pattern for RAG detection — same shape as the
// aiqg-bloated-context Gatekeeper matcher. Reused-by-similarity, not
// reused-by-import, because Gatekeeper imports the scan engine which
// adds compile-time bulk this hot path doesn't need.
var ragSeparatorRe = regexp.MustCompile(`(?i)\b(document|context|source|chunk|passage|reference)(\s*#?\s*\d+)?\s*:`)

// summarizationCues fire when ANY non-system message hints at a
// summarization task. "Summarize" / "summary" / "tl;dr" / "in X
// bullet points" / "key takeaways".
var summarizationCues = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(summari[sz]e|summary|tl;?dr)\b`),
	regexp.MustCompile(`(?i)\bin\s+\d+\s+(bullet|sentence|paragraph|point)s?\b`),
	regexp.MustCompile(`(?i)\bkey\s+(takeaway|point|finding)s?\b`),
}

// codeGenCues fire when a message asks for code. The presence of a
// fenced code block alone isn't enough — that can be context for a
// non-code question. We require an explicit ask. The .*? between
// verb and noun tolerates intervening modifiers (e.g. "write a
// Python function").
var codeGenCues = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(write|implement|generate|refactor|fix|debug)\b[^.!?\n]{0,40}\b(code|function|class|method|module|script|program|implementation|unit\s+test|test)\b`),
	regexp.MustCompile(`(?i)\b(?:in|using)\s+(?:python|javascript|typescript|java|go|golang|rust|c\+\+|c#|ruby|kotlin|swift)\b.*\b(?:function|class|method|implementation|code)\b`),
	regexp.MustCompile(`(?i)\bcomplete\s+(?:the|this)\s+(?:function|method|class|implementation)\b`),
}

// classificationCues fire when the request looks like a labeling /
// extraction task — discrete output expected.
var classificationCues = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:classify|categor(?:i[sz]e|y)|label|tag)\s+(?:the|this|each|every|following)\b`),
	// "Is this X a Y or Z (sentiment)?" — tolerate an optional
	// trailing noun before the question mark / sentence end.
	regexp.MustCompile(`(?i)\bis\s+(?:the|this)\s+\w+\s+(?:a|an)\s+\w+\s+or\s+\w+(?:\s+\w+)?[?.!]`),
	regexp.MustCompile(`(?i)\bextract\s+(?:the|all|every)?\s*\w+\s+from\b`),
	regexp.MustCompile(`(?i)\bwhich\s+(?:category|class|label|type)\b`),
}

// Classify returns one of the WorkflowXxx constants or "" when no
// confident classification was found. Pure heuristic, no I/O.
func Classify(req *types.ChatRequest) string {
	if req == nil {
		return ""
	}

	// Rule 1: tool / function calling → agentic.
	if len(req.Tools) > 0 || len(req.Functions) > 0 {
		return WorkflowAgentic
	}

	// Concatenate user-role message text once so each cue scans the
	// same buffer. System prompts are excluded because long system
	// prompts shouldn't dominate the classification.
	var userText strings.Builder
	var userMsgCount int
	var longestUserMsg int
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		s := messageText(m)
		if s == "" {
			continue
		}
		userMsgCount++
		if len(s) > longestUserMsg {
			longestUserMsg = len(s)
		}
		if userText.Len() > 0 {
			userText.WriteString("\n")
		}
		userText.WriteString(s)
	}
	body := userText.String()
	if body == "" {
		return ""
	}

	// Rules 2-4: cue-pattern matches. Order chosen so the most
	// specific intent wins — summarization implies bulk text input
	// but doesn't always co-occur with the code-gen cues, etc.
	if anyMatch(body, summarizationCues) {
		return WorkflowSummarization
	}
	if anyMatch(body, codeGenCues) {
		return WorkflowCodeGeneration
	}
	if anyMatch(body, classificationCues) {
		return WorkflowClassificationExtract
	}

	// Rule 5: RAG-style multi-document context.
	if len(ragSeparatorRe.FindAllStringIndex(body, -1)) >= ragMinSeparators {
		return WorkflowRAG
	}

	// Rule 6: short single-turn QA.
	if userMsgCount == 1 && longestUserMsg <= singleTurnQAMaxChars {
		return WorkflowSingleTurnQA
	}

	// Fallthrough — unknown. Better to let the default SLA apply than
	// guess wrong.
	return ""
}

func anyMatch(s string, res []*regexp.Regexp) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// messageText extracts the string body from a Message.Content union
// (string OR []ContentPart for multimodal). For multimodal messages
// we concatenate every text part — image parts contribute nothing
// to text-cue matching.
func messageText(m types.Message) string {
	switch c := m.Content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, part := range c {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := pm["text"].(string); ok {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(t)
			}
		}
		return sb.String()
	default:
		return ""
	}
}
