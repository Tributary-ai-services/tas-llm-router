package semcache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// OllamaEmbedder is the production Embedder — it calls an Ollama server's
// embeddings endpoint. We reuse the all-minilm model already deployed for the
// payload-reduction extractor (384-dim, matching the FLAT index's DIM 384). It
// is general-purpose, not cache-match-tuned (the design prefers langcache-embed);
// adequate for S1 shadow-mode data collection, upgradeable before serving.
type OllamaEmbedder struct {
	baseURL string
	model   string
	dim     int
	hc      *http.Client
}

// NewOllamaEmbedder targets baseURL (e.g. http://ollama.tas-shared:11434) with
// model (e.g. all-minilm). dim must match the vector index dimensionality.
func NewOllamaEmbedder(baseURL, model string, dim int) *OllamaEmbedder {
	return &OllamaEmbedder{
		baseURL: baseURL, model: model, dim: dim,
		hc: &http.Client{Timeout: 10 * time.Second},
	}
}

func (o *OllamaEmbedder) Dim() int { return o.dim }

// Embed returns the prompt's embedding. Uses Ollama's /api/embed (batch shape);
// a non-2xx or a dimension mismatch is an error so the caller treats it as a
// cache miss rather than indexing a bad vector.
func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": o.model, "input": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("ollama embed: status %d", resp.StatusCode)
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Embeddings) == 0 || len(out.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("ollama embed: empty embedding")
	}
	v := out.Embeddings[0]
	if len(v) != o.dim {
		return nil, fmt.Errorf("ollama embed: dim %d != index dim %d (wrong model?)", len(v), o.dim)
	}
	return v, nil
}
