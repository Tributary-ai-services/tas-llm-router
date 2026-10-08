package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// AIQG-50 was shipped patched into ONE of three handler error paths, and the
// live probe caught it: a /v1/messages request still returned 500 because that
// route goes through handleNonStreamingCompletionWithRetry, not
// handleNonStreamingCompletion. The unit tests all passed, because they covered
// the classifier rather than its call sites.
//
// This is the guard for that class of miss. It fails if any handler answers a
// provider failure with a hardcoded 500 instead of classifying it through
// upstreamFailure, so adding a fourth route cannot quietly reintroduce the
// defect.
//
// Source-level, deliberately. The alternative -- exercising every route against
// a vendor that refuses -- needs a live upstream and would not run in CI.
func TestNoUnclassifiedProviderFailures(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	// A provider failure answered with a literal 500 and the raw error text.
	// upstreamFailure is the only sanctioned way to turn a provider error into
	// an HTTP status, because it is what relays the vendor's own verdict.
	bad := regexp.MustCompile(`writeErrorCtx\([^)]*http\.StatusInternalServerError,\s*fmt\.Sprintf\("(Completion|Streaming|Routing) failed`)
	for i, line := range strings.Split(string(src), "\n") {
		if bad.MatchString(line) {
			t.Errorf("server.go:%d answers a provider failure with a hardcoded 500 "+
				"instead of s.upstreamFailure(...): %s", i+1, strings.TrimSpace(line))
		}
	}
}

// The classifier must be reached from every handler that can receive a provider
// error. Counting call sites is crude, but it is what catches "a new route was
// added and nobody wired it" -- the exact miss this item recorded.
func TestUpstreamFailureIsUsedByEveryCompletionPath(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	const want = 3 // non-streaming, non-streaming-with-retry, streaming
	if got := strings.Count(string(src), "s.upstreamFailure(r, err,"); got != want {
		t.Errorf("found %d upstreamFailure call sites, want %d -- a completion path was "+
			"added or removed; wire it (or update this count with the reason)", got, want)
	}
}
