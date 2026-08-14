package anthropic

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// CountTokens returns the exact number of input tokens Anthropic would bill for
// the request (system + messages + tools), via the vendor's count_tokens
// endpoint. Serves POST /v1/messages/count_tokens. Reuses convertToAnthropicRequest
// for the message/system conversion; tools use the count_tokens-specific union.
func (p *AnthropicProvider) CountTokens(ctx context.Context, req *types.ChatRequest) (int, error) {
	mp, err := p.convertToAnthropicRequest(req)
	if err != nil {
		return 0, err
	}

	params := anthropic.MessageCountTokensParams{
		Model:    mp.Model,
		Messages: mp.Messages,
	}
	if len(mp.System) > 0 {
		params.System = anthropic.MessageCountTokensParamsSystemUnion{OfTextBlockArray: mp.System}
	}
	if len(req.Tools) > 0 {
		var tools []anthropic.MessageCountTokensToolUnionParam
		for _, tool := range req.Tools {
			if tool.Type == "function" || tool.Type == "" {
				tools = append(tools, anthropic.MessageCountTokensToolParamOfTool(
					toAnthropicInputSchema(tool.Function.Parameters), tool.Function.Name))
			}
		}
		params.Tools = tools
	}

	res, err := p.clientFor(ctx).Messages.CountTokens(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("anthropic count_tokens failed: %w", err)
	}
	return int(res.InputTokens), nil
}
