package workflow

import (
	"strings"
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func msg(role, content string) types.Message {
	return types.Message{Role: role, Content: content}
}

func TestClassify_Agentic_Tools(t *testing.T) {
	req := &types.ChatRequest{
		Messages: []types.Message{msg("user", "help me with my tasks")},
		Tools:    []types.Tool{{}},
	}
	if got := Classify(req); got != WorkflowAgentic {
		t.Errorf("expected agentic, got %q", got)
	}
}

func TestClassify_Summarization(t *testing.T) {
	for _, prompt := range []string{
		"Please summarize this report.",
		"In 5 bullet points, recap the meeting.",
		"What are the key takeaways from this article?",
		"tl;dr the discussion above.",
	} {
		t.Run(prompt[:20], func(t *testing.T) {
			req := &types.ChatRequest{Messages: []types.Message{msg("user", prompt)}}
			if got := Classify(req); got != WorkflowSummarization {
				t.Errorf("prompt %q → %q, want summarization", prompt, got)
			}
		})
	}
}

func TestClassify_CodeGeneration(t *testing.T) {
	for _, prompt := range []string{
		"Write a Python function that reverses a string.",
		"Implement the binary search method in Go.",
		"Complete this function: def hello(...)",
		"Refactor this code to remove the duplication.",
	} {
		t.Run(prompt[:20], func(t *testing.T) {
			req := &types.ChatRequest{Messages: []types.Message{msg("user", prompt)}}
			if got := Classify(req); got != WorkflowCodeGeneration {
				t.Errorf("prompt %q → %q, want code_generation", prompt, got)
			}
		})
	}
}

func TestClassify_Classification(t *testing.T) {
	for _, prompt := range []string{
		"Classify the following email as spam or not spam.",
		"Is this review a positive or negative sentiment?",
		"Extract the dates from this paragraph.",
		"Which category does this product belong to?",
	} {
		t.Run(prompt[:20], func(t *testing.T) {
			req := &types.ChatRequest{Messages: []types.Message{msg("user", prompt)}}
			if got := Classify(req); got != WorkflowClassificationExtract {
				t.Errorf("prompt %q → %q, want classification_extraction", prompt, got)
			}
		})
	}
}

func TestClassify_RAG(t *testing.T) {
	body := "Question: what's mentioned?\n\n" +
		"Document 1: foo\nDocument 2: bar\nDocument 3: baz\nDocument 4: qux"
	req := &types.ChatRequest{Messages: []types.Message{msg("user", body)}}
	if got := Classify(req); got != WorkflowRAG {
		t.Errorf("expected rag, got %q", got)
	}
}

func TestClassify_SingleTurnQA(t *testing.T) {
	req := &types.ChatRequest{Messages: []types.Message{msg("user", "What's the capital of France?")}}
	if got := Classify(req); got != WorkflowSingleTurnQA {
		t.Errorf("expected single_turn_qa, got %q", got)
	}
}

func TestClassify_Empty(t *testing.T) {
	if got := Classify(nil); got != "" {
		t.Errorf("nil request should classify to empty, got %q", got)
	}
	if got := Classify(&types.ChatRequest{}); got != "" {
		t.Errorf("empty request should classify to empty, got %q", got)
	}
}

// A long single-turn user message shouldn't trigger single_turn_qa
// (>500 chars). Should fall through to "" — better unknown than wrong.
func TestClassify_LongSingleMessage_FallsThrough(t *testing.T) {
	long := strings.Repeat("filler text ", 80) // ~960 chars
	req := &types.ChatRequest{Messages: []types.Message{msg("user", long)}}
	if got := Classify(req); got != "" {
		t.Errorf("long single message should fall through to unknown, got %q", got)
	}
}

// Multiple user turns + short messages → not single_turn_qa.
func TestClassify_MultiTurn_NotSingleTurnQA(t *testing.T) {
	req := &types.ChatRequest{Messages: []types.Message{
		msg("user", "hi"),
		msg("assistant", "hello"),
		msg("user", "how are you"),
	}}
	if got := Classify(req); got != "" {
		t.Errorf("multi-turn → %q, want \"\"", got)
	}
}

// Priority test — when tools are present, ALL other cues are ignored.
func TestClassify_Priority_AgenticOverSummarize(t *testing.T) {
	req := &types.ChatRequest{
		Messages: []types.Message{msg("user", "summarize this for me")},
		Tools:    []types.Tool{{}},
	}
	if got := Classify(req); got != WorkflowAgentic {
		t.Errorf("agentic must win over summarization, got %q", got)
	}
}
