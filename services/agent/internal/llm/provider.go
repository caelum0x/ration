// Package llm is a thin, dependency-free client for the Anthropic Messages
// API plus the agentic tool-calling loop built on top of it. There is no
// Anthropic SDK dependency here by design — this talks to
// https://api.anthropic.com/v1/messages directly over net/http.
//
// Optionally, calls can instead be routed through Railyard's Relay gateway
// (a sibling product, ~/products/railyard/apps/relay) by setting
// RATION_LLM_GATEWAY_URL. Relay's client-facing surface is OpenAI-compatible
// chat.completions, not the Anthropic Messages wire format, so that path
// translates at the transport boundary — see gateway.go. See
// docs/AGENT-DESIGN.md for why (metering/budget enforcement via Railyard's
// Abacus) and exactly what is wired versus a documented TODO.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	defaultBaseURL           = "https://api.anthropic.com"
	defaultAnthropicVersion  = "2023-06-01"
	defaultModel             = "claude-sonnet-5"
	defaultMaxTokens         = 4096
	defaultMaxToolIterations = 8
	defaultHTTPTimeout       = 60 * time.Second

	// gatewayURLEnvVar, when set, routes every LLM call through Railyard's
	// Relay gateway instead of directly to Anthropic. It is deliberately
	// NOT defaulted to a non-empty value in code: Railyard's Relay has no
	// confirmed-reachable production URL from this sandbox (see gateway.go
	// package doc and docs/AGENT-DESIGN.md), and defaulting to any
	// non-empty URL would make the existing, working, tested direct-
	// Anthropic path silently start trying to call a gateway that may not
	// exist in a given environment. Leaving it unset preserves the
	// existing behavior exactly.
	//
	// For local development against a Railyard Relay quickstart (confirmed
	// from ~/products/railyard/apps/relay/README.md and
	// docker-compose.quickstart.yml: `docker compose -f
	// docker-compose.quickstart.yml up --build`, OpenAI-compatible API
	// served on port 3000 under /openai/v1), set this to:
	//   http://localhost:3000/openai/v1
	gatewayURLEnvVar = "RATION_LLM_GATEWAY_URL"

	// gatewayModelEnvVar selects which Relay/TensorZero model or function
	// selector to request (e.g. "tensorzero::model_name::claude-sonnet" or
	// "tensorzero::function_name::chat") — see gateway.go's gatewayModel
	// doc comment for why this can't be defaulted sensibly either.
	gatewayModelEnvVar = "RATION_LLM_GATEWAY_MODEL"

	// gatewayAPIKeyEnvVar carries the bearer token Relay expects as
	// `Authorization: Bearer <token>` (confirmed from
	// apps/relay/docs/gateway/configuration-reference.mdx and
	// apps/relay/examples/relay/openai-client/README.md). Left unset, no
	// Authorization header is sent, which only works against a Relay
	// instance running with `[gateway.auth] enabled = false` (e.g. the
	// quickstart config).
	gatewayAPIKeyEnvVar = "RATION_LLM_GATEWAY_API_KEY"
)

// ContentBlock is one block of a message's content array. The Anthropic
// Messages API uses the same block shape for text, tool_use, and
// tool_result; which fields are populated depends on Type.
type ContentBlock struct {
	Type string `json:"type"`

	// type == "text"
	Text string `json:"text,omitempty"`

	// type == "tool_use"
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type == "tool_result"
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Message is one turn in the conversation.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// Tool describes one tool the model may call. InputSchema must be a valid
// JSON Schema object, e.g. produced with json.Marshal of a plain Go map or
// struct.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolHandler executes a single tool call and returns the text to report
// back to the model as the tool_result content, plus whether it represents
// an error (reported to the model via tool_result.is_error).
type ToolHandler func(ctx context.Context, toolName string, input json.RawMessage) (resultContent string, isError bool)

// Provider is the interface the rest of the agent depends on. It is
// satisfied by *AnthropicProvider for real calls and by a hand-rolled fake
// in tests, so internal/loop can be tested without any network access.
type Provider interface {
	// RunConversation drives a single-user-turn, multi-tool-call
	// conversation to completion: it sends systemPrompt + userMessage with
	// tools attached, executes any tool_use blocks the model returns via
	// handler, feeds the tool_result blocks back, and repeats until the
	// model stops calling tools (or the iteration budget is exhausted). It
	// returns the model's final text response.
	RunConversation(ctx context.Context, systemPrompt, userMessage string, tools []Tool, handler ToolHandler) (string, error)
}

// AnthropicProvider is the real Provider backed by the Anthropic Messages
// API over plain net/http, or (when GatewayURL is set) by Railyard's Relay
// gateway — see gateway.go.
type AnthropicProvider struct {
	HTTPClient        *http.Client
	APIKey            string
	Model             string
	BaseURL           string
	AnthropicVersion  string
	MaxTokens         int
	MaxToolIterations int

	// GatewayURL, when non-empty, routes calls through Railyard's Relay
	// gateway instead of directly to Anthropic (see gateway.go). It is the
	// base URL up to and including the OpenAI-compatible path prefix (e.g.
	// "http://localhost:3000/openai/v1"); "/chat/completions" is appended.
	GatewayURL string
	// GatewayModel is the Relay/TensorZero model or function selector to
	// request. Falls back to Model if empty (see gatewayModel in
	// gateway.go for why that fallback is best-effort, not guaranteed).
	GatewayModel string
	// GatewayAPIKey, when non-empty, is sent as `Authorization: Bearer
	// <GatewayAPIKey>` to Relay. Unused when GatewayURL is empty.
	GatewayAPIKey string
}

// NewAnthropicProviderFromEnv builds an AnthropicProvider from environment
// variables: ANTHROPIC_API_KEY (required for real direct-Anthropic calls —
// an empty key is allowed to construct the provider but every call will
// fail with a clear error), RATION_AGENT_MODEL (default
// "claude-sonnet-5"), and the optional Railyard Relay gateway settings
// RATION_LLM_GATEWAY_URL, RATION_LLM_GATEWAY_MODEL, and
// RATION_LLM_GATEWAY_API_KEY (see the gatewayURLEnvVar doc comment above —
// leaving RATION_LLM_GATEWAY_URL unset preserves the original
// direct-to-Anthropic behavior exactly).
func NewAnthropicProviderFromEnv() *AnthropicProvider {
	model := os.Getenv("RATION_AGENT_MODEL")
	if model == "" {
		model = defaultModel
	}

	return &AnthropicProvider{
		HTTPClient:        &http.Client{Timeout: defaultHTTPTimeout},
		APIKey:            os.Getenv("ANTHROPIC_API_KEY"),
		Model:             model,
		BaseURL:           defaultBaseURL,
		AnthropicVersion:  defaultAnthropicVersion,
		MaxTokens:         defaultMaxTokens,
		MaxToolIterations: defaultMaxToolIterations,
		GatewayURL:        os.Getenv(gatewayURLEnvVar),
		GatewayModel:      os.Getenv(gatewayModelEnvVar),
		GatewayAPIKey:     os.Getenv(gatewayAPIKeyEnvVar),
	}
}

type messagesRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	Tools     []Tool    `json:"tools,omitempty"`
}

type messagesResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// APIError is returned when the Anthropic API responds with a non-2xx
// status. Callers can inspect StatusCode to distinguish retryable
// (429, 5xx) from non-retryable (4xx other than 429) failures.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic API error: status %d: %s", e.StatusCode, e.Body)
}

// Retryable reports whether the request is worth retrying as-is (rate
// limited or a server-side failure).
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// createMessage dispatches to Railyard's Relay gateway when GatewayURL is
// configured, falling back to a direct Anthropic Messages API call
// otherwise. This is the only branch point between the two transports —
// RunConversation and everything above it is identical either way.
func (p *AnthropicProvider) createMessage(ctx context.Context, req messagesRequest) (*messagesResponse, error) {
	if p.GatewayURL != "" {
		return p.createMessageViaGateway(ctx, req)
	}
	return p.createMessageDirect(ctx, req)
}

// createMessageDirect calls the Anthropic Messages API directly. This is
// the original, unmodified code path (pre-gateway-routing) — see
// createMessageViaGateway in gateway.go for the Railyard Relay path.
func (p *AnthropicProvider) createMessageDirect(ctx context.Context, req messagesRequest) (*messagesResponse, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("llm: ANTHROPIC_API_KEY is not set")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	baseURL := p.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	version := p.AnthropicVersion
	if version == "" {
		version = defaultAnthropicVersion
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", p.APIKey)
	httpReq.Header.Set("anthropic-version", version)

	respBody, err := p.sendWithRetry(ctx, httpReq)
	if err != nil {
		return nil, err
	}

	var parsed messagesResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}
	return &parsed, nil
}

// sendWithRetry executes httpReq (cloned per attempt so retries get a fresh
// body reader) up to 3 attempts with linear backoff, retrying only on
// retryable API errors (429, 5xx) or transport-level failures. It returns
// the raw 2xx response body. Shared by both the direct-Anthropic and
// Relay-gateway transports so the retry/backoff behavior is identical and
// defined in exactly one place.
func (p *AnthropicProvider) sendWithRetry(ctx context.Context, httpReq *http.Request) ([]byte, error) {
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}

		httpResp, err := client.Do(httpReq.Clone(ctx))
		if err != nil {
			lastErr = fmt.Errorf("llm: do request: %w", err)
			continue
		}

		respBody, readErr := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("llm: read response: %w", readErr)
			continue
		}

		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			apiErr := &APIError{StatusCode: httpResp.StatusCode, Body: string(respBody)}
			if apiErr.Retryable() {
				lastErr = apiErr
				continue
			}
			return nil, apiErr
		}

		return respBody, nil
	}

	return nil, fmt.Errorf("llm: request failed after retries: %w", lastErr)
}

// RunConversation implements Provider.
func (p *AnthropicProvider) RunConversation(ctx context.Context, systemPrompt, userMessage string, tools []Tool, handler ToolHandler) (string, error) {
	messages := []Message{
		{Role: "user", Content: []ContentBlock{{Type: "text", Text: userMessage}}},
	}

	maxIterations := p.MaxToolIterations
	if maxIterations <= 0 {
		maxIterations = defaultMaxToolIterations
	}
	maxTokens := p.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	for i := 0; i < maxIterations; i++ {
		resp, err := p.createMessage(ctx, messagesRequest{
			Model:     p.Model,
			MaxTokens: maxTokens,
			System:    systemPrompt,
			Messages:  messages,
			Tools:     tools,
		})
		if err != nil {
			return "", err
		}

		messages = append(messages, Message{Role: "assistant", Content: resp.Content})

		if resp.StopReason != "tool_use" {
			return extractText(resp.Content), nil
		}

		toolUseBlocks := make([]ContentBlock, 0, len(resp.Content))
		for _, block := range resp.Content {
			if block.Type == "tool_use" {
				toolUseBlocks = append(toolUseBlocks, block)
			}
		}
		if len(toolUseBlocks) == 0 {
			// stop_reason said tool_use but there's nothing to execute;
			// avoid spinning forever.
			return extractText(resp.Content), nil
		}

		resultBlocks := make([]ContentBlock, 0, len(toolUseBlocks))
		for _, use := range toolUseBlocks {
			if handler == nil {
				resultBlocks = append(resultBlocks, ContentBlock{
					Type:      "tool_result",
					ToolUseID: use.ID,
					Content:   "no tool handler configured",
					IsError:   true,
				})
				continue
			}
			result, isErr := handler(ctx, use.Name, use.Input)
			resultBlocks = append(resultBlocks, ContentBlock{
				Type:      "tool_result",
				ToolUseID: use.ID,
				Content:   result,
				IsError:   isErr,
			})
		}

		// All tool_result blocks for one turn must go back in a single
		// user message — splitting them across messages trains the model
		// to stop issuing parallel tool calls.
		messages = append(messages, Message{Role: "user", Content: resultBlocks})
	}

	return "", fmt.Errorf("llm: exceeded max tool iterations (%d) without reaching a final answer", maxIterations)
}

func extractText(blocks []ContentBlock) string {
	var out string
	for _, b := range blocks {
		if b.Type == "text" {
			out += b.Text
		}
	}
	return out
}
