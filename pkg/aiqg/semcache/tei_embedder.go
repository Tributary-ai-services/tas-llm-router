package semcache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// TEIEmbedder is the Embedder implementation for HuggingFace
// text-embeddings-inference, the serving path for redis/langcache-embed-v3-small
// (docs/AIQG-SEMANTIC-CACHING.md §6).
//
// Why a second implementation rather than reusing OllamaEmbedder: Ollama cannot
// serve this model at all (not GGUF / llama.cpp-compatible — both pull forms
// were tried and rejected), and the wire shapes differ anyway. TEI takes
// {"inputs": …} on /embed and returns a bare [[float…]]; Ollama takes
// {"model","input"} on /api/embed and returns {"embeddings":[[…]]}. TEI is also
// pinned to ONE model by its --model-id flag, so there is no per-request model
// field to send.
//
// Why the model matters more than the threshold: general-purpose retrieval
// encoders are the false-hit root cause, not the cure. Measured on a 31-pair
// labeled set (calibrate_live_test.go), all-minilm and nomic-embed-text both
// leave the match/near-miss classes interleaved (separation -0.33 and -0.12);
// langcache-embed-v3-small is purpose-trained for cache matching with negation
// in its eval set.
//
// ⚠️ Dimensionality is 384 — the same as all-minilm — which makes this a
// drop-in for the RediSearch FLAT index but ALSO means the store will silently
// compare vectors from the two models if entries outlive a switch. The cache
// key and Scope do not identify the embedder. Flush aiqg:scache:* on cutover.
type TEIEmbedder struct {
	baseURL string
	dim     int
	hc      *http.Client
}

// NewTEIEmbedder targets baseURL (e.g. http://tei.tas-shared:8080). dim must
// match the vector index dimensionality; the served model is fixed by TEI's
// own --model-id, so it is not a parameter here.
func NewTEIEmbedder(baseURL string, dim int) *TEIEmbedder {
	return &TEIEmbedder{
		baseURL: strings.TrimRight(baseURL, "/"),
		dim:     dim,
		hc:      &http.Client{Timeout: 10 * time.Second},
	}
}

func (t *TEIEmbedder) Dim() int { return t.dim }

// Embed returns the prompt's embedding via TEI's /embed. A non-2xx or a
// dimension mismatch is an error so the caller treats it as a cache miss
// rather than indexing a bad vector — the cascade fails open to a miss.
func (t *TEIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	// truncate lets TEI clamp inputs past the model's max sequence (128 for
	// langcache-embed-v3-small) instead of erroring the request. A truncated
	// embedding is a weaker match signal; a failed embed is no cache at all.
	body, _ := json.Marshal(map[string]any{"inputs": text, "truncate": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("tei embed: status %d", resp.StatusCode)
	}
	// TEI returns a bare array-of-arrays, not an object.
	var out [][]float32
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out) == 0 || len(out[0]) == 0 {
		return nil, fmt.Errorf("tei embed: empty embedding")
	}
	v := out[0]
	if len(v) != t.dim {
		return nil, fmt.Errorf("tei embed: dim %d != index dim %d (wrong model?)", len(v), t.dim)
	}
	return v, nil
}
