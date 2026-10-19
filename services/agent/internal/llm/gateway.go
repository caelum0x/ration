package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// This file implements the Railyard Relay gateway transport: translating
// Anthropic Messages wire shapes (ContentBlock/Message/Tool,
// tool_use/tool_result blocks, the top-level "system" field) to and from
// Relay's client-facing OpenAI-compatible chat.completions wire shape.
//
// Confirmed from the sibling Railyard repo
// (~/products/railyard/apps/relay/README.md,
// apps/relay/examples/relay/openai-client/README.md, and
// apps/relay/docs/gateway/configuration-reference.mdx — Relay is a
// maintained fork of TensorZero, not an Anthropic-API-shaped proxy):
//
//   - Endpoint: POST <base>/chat/completions, where <base> is something
//     like "http://localhost:3000/openai/v1" for a local
//     docker-compose.quickstart.yml run.
//   - Auth: "Authorization: Bearer <api-key>" (OpenAI-SDK-style), only
//     enforced when the deployment has `[gateway.auth] enabled = true`;
//     the quickstart config runs with auth off.
//   - Request/response body: OpenAI's chat.completions shape — messages
//     with string content, a "tool" role for tool results keyed by
//     tool_call_id, and assistant tool_calls with stringified JSON
//     arguments — NOT Anthropic's content-block array.
//   - Model selection: a `model` field using TensorZero selector syntax
//     (e.g. "tensorzero::model_name::claude-sonnet" or
//     "tensorzero::function_name::chat") that is only meaningful against
//     the operator's own deployed tensorzero.toml. This package cannot
//     know that mapping, so it is the required RATION_LLM_GATEWAY_MODEL
//     env var (see gatewayModel below), not a guess.
//
// What this does NOT do yet (documented TODO, not silently skipped):
// Railyard's Relay has no confirmed-reachable, owner-provided production
// URL from this sandbox, and its budget/metering features (Mint billing,
// RATION_LLM_GATEWAY_API_KEY auth, per-key spend caps via Relay's Abacus
// integration) are opt-in server-side configuration this change does not
// provision. Wiring an actual production Relay deployment, minting a real
// RATION_LLM_GATEWAY_API_KEY, and setting RATION_LLM_GATEWAY_MODEL to a
// model/function that operator's tensorzero.toml actually serves are owner
// actions tracked in docs/AGENT-DESIGN.md, not implemented here.

type openAIFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openAITool struct {
	Type     string            `json:"type"`
	Function openAIFunctionDef `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    *string          `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIChatRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	Messages  []openAIMessage `json:"messages"`
	Tools     []openAITool    `json:"tools,omitempty"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Role      string           `json:"role"`
			Content   *string          `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// gatewayModel resolves which Relay model/function selector to request.
// RATION_LLM_GATEWAY_MODEL (p.GatewayModel) is the correct, explicit way to
// set this — Relay/TensorZero selectors only exist in the operator's own
// deployed tensorzero.toml, which this package has no way to introspect.
// Falling back to p.Model (the Anthropic model name, e.g.
// "claude-sonnet-5") is a best-effort default that only works if the
// operator's Relay config happens to expose a model or function by that
// exact name; it is not a verified mapping.
func (p *AnthropicProvider) gatewayModel() string {
	if p.GatewayModel != "" {
		return p.GatewayModel
	}
	return p.Model
}

// toGatewayRequest translates an Anthropic Messages API request into
// Relay's OpenAI-compatible chat.completions request.
func toGatewayRequest(p *AnthropicProvider, req messagesRequest) (*openAIChatRequest, error) {
	out := &openAIChatRequest{
		Model:     p.gatewayModel(),
		MaxTokens: req.MaxTokens,
	}

	if req.System != "" {
		system := req.System
		out.Messages = append(out.Messages, openAIMessage{Role: "system", Content: &system})
	}

	for _, msg := range req.Messages {
		converted, err := toGatewayMessages(msg)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, converted...)
	}

	for _, tool := range req.Tools {
		out.Tools = append(out.Tools, openAITool{
			Type: "function",
			Function: openAIFunctionDef{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			},
		})
	}

	return out, nil
}

// toGatewayMessages converts one Anthropic-shaped Message into zero or more
// OpenAI chat messages. A single Anthropic user message carrying several
// tool_result blocks (RunConversation always sends all of one turn's
// results together — see its comment in provider.go on why splitting them
// across messages is avoided) must expand into that many separate OpenAI
// "tool"-role messages, since OpenAI's chat.completions has no
// multi-result message shape.
func toGatewayMessages(msg Message) ([]openAIMessage, error) {
	var text string
	var toolCalls []openAIToolCall
	var toolResults []openAIMessage

	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			text += block.Text
		case "tool_use":
			toolCalls = append(toolCalls, openAIToolCall{
				ID:   block.ID,
				Type: "function",
				Function: openAIFunctionCall{
					Name:      block.Name,
					Arguments: string(block.Input),
				},
			})
		case "tool_result":
			content := block.Content
			toolResults = append(toolResults, openAIMessage{
				Role:       "tool",
				Content:    &content,
				ToolCallID: block.ToolUseID,
			})
		default:
			return nil, fmt.Errorf("llm: gateway translation: unsupported content block type %q", block.Type)
		}
	}

	var out []openAIMessage
	if text != "" || len(toolCalls) > 0 {
		m := openAIMessage{Role: msg.Role}
		if text != "" {
			m.Content = &text
		}
		if len(toolCalls) > 0 {
			m.ToolCalls = toolCalls
		}
		out = append(out, m)
	}
	out = append(out, toolResults...)
	return out, nil
}

// fromGatewayResponse translates Relay's OpenAI-compatible chat.completions
// response back into the Anthropic-shaped messagesResponse that
// RunConversation already knows how to consume.
func fromGatewayResponse(resp *openAIChatResponse) (*messagesResponse, error) {
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("llm: gateway response had no choices")
	}
	choice := resp.Choices[0]

	var blocks []ContentBlock
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		blocks = append(blocks, ContentBlock{Type: "text", Text: *choice.Message.Content})
	}
	for _, call := range choice.Message.ToolCalls {
		blocks = append(blocks, ContentBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: json.RawMessage(call.Function.Arguments),
		})
	}

	// OpenAI's "tool_calls" finish reason is this package's "tool_use" —
	// RunConversation branches on that exact string. Any other finish
	// reason (e.g. "stop", "length") is passed through unchanged; all that
	// matters to the caller is that it is not "tool_use".
	stopReason := choice.FinishReason
	if stopReason == "tool_calls" {
		stopReason = "tool_use"
	}

	out := &messagesResponse{
		Role:       "assistant",
		Content:    blocks,
		StopReason: stopReason,
	}
	out.Usage.InputTokens = resp.Usage.PromptTokens
	out.Usage.OutputTokens = resp.Usage.CompletionTokens
	return out, nil
}

// createMessageViaGateway sends req to Railyard's Relay gateway at
// p.GatewayURL instead of the Anthropic API, translating to and from
// Relay's OpenAI-compatible chat.completions wire format. See this file's
// package-level comment for the confirmed wire shape and what is still a
// documented TODO (a real production Relay URL/key and the operator's
// tensorzero.toml model mapping).
func (p *AnthropicProvider) createMessageViaGateway(ctx context.Context, req messagesRequest) (*messagesResponse, error) {
	gatewayReq, err := toGatewayRequest(p, req)
	if err != nil {
		return nil, fmt.Errorf("llm: translate request for gateway: %w", err)
	}

	body, err := json.Marshal(gatewayReq)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal gateway request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.GatewayURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build gateway request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	if p.GatewayAPIKey != "" {
		httpReq.Header.Set("authorization", "Bearer "+p.GatewayAPIKey)
	}

	respBody, err := p.sendWithRetry(ctx, httpReq)
	if err != nil {
		return nil, err
	}

	var parsed openAIChatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("llm: decode gateway response: %w", err)
	}
	return fromGatewayResponse(&parsed)
}
