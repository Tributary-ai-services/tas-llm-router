package openai

import (
	"context"
	"fmt"

	"github.com/sashabaranov/go-openai"

	"github.com/tributary-ai/llm-router-waf/internal/instrumentation"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// Embeddings serves POST /v1/embeddings. It forwards to OpenAI's embeddings
// endpoint via the per-request (BYOK-aware) client and returns the OpenAI-shaped
// response. go-openai transparently decodes a base64-encoded vendor response
// into float32 vectors, so the emitted `embedding` is always a float array — the
// OpenAI SDK accepts that regardless of the encoding_format it requested.
func (p *OpenAIProvider) Embeddings(ctx context.Context, req *types.EmbeddingRequest) (*types.EmbeddingResponse, error) {
	instrumentation.StampForwarded(ctx)
	ctx = instrumentation.Attach(ctx)

	oreq := openai.EmbeddingRequest{
		Model: openai.EmbeddingModel(req.Model),
		Input: req.Input,
		User:  req.User,
	}
	if req.EncodingFormat != "" {
		oreq.EncodingFormat = openai.EmbeddingEncodingFormat(req.EncodingFormat)
	}
	if req.Dimensions != nil {
		oreq.Dimensions = *req.Dimensions
	}

	resp, err := p.clientFor(ctx).CreateEmbeddings(ctx, oreq)
	if err != nil {
		p.logger.WithError(err).Error("OpenAI embeddings call failed")
		return nil, fmt.Errorf("openai embeddings call failed: %w", err)
	}

	out := &types.EmbeddingResponse{Object: "list", Model: string(resp.Model)}
	for _, d := range resp.Data {
		out.Data = append(out.Data, types.EmbeddingData{
			Object:    "embedding",
			Index:     d.Index,
			Embedding: d.Embedding,
		})
	}
	out.Usage = &types.EmbeddingUsage{
		PromptTokens: resp.Usage.PromptTokens,
		TotalTokens:  resp.Usage.TotalTokens,
	}
	return out, nil
}
