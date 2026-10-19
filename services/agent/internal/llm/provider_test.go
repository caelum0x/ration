package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewAnthropicProviderFromEnv_GatewayVarsUnset(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("RATION_AGENT_MODEL", "")
	t.Setenv(gatewayURLEnvVar, "")
	t.Setenv(gatewayModelEnvVar, "")
	t.Setenv(gatewayAPIKeyEnvVar, "")

	p := NewAnthropicProviderFromEnv()

	if p.GatewayURL != "" {
		t.Errorf("GatewayURL = %q, want empty so the existing direct-Anthropic path is unaffected", p.GatewayURL)
	}
	if p.Model != defaultModel {
		t.Errorf("Model = %q, want default %q", p.Model, defaultModel)
	}
	if p.APIKey != "sk-ant-test" {
		t.Errorf("APIKey = %q, want value from ANTHROPIC_API_KEY", p.APIKey)
	}
}

func TestNewAnthropicProviderFromEnv_GatewayVarsSet(t *testing.T) {
	t.Setenv(gatewayURLEnvVar, "http://localhost:3000/openai/v1")
	t.Setenv(gatewayModelEnvVar, "tensorzero::function_name::chat")
	t.Setenv(gatewayAPIKeyEnvVar, "relay-quickstart")

	p := NewAnthropicProviderFromEnv()

	if p.GatewayURL != "http://localhost:3000/openai/v1" {
		t.Errorf("GatewayURL = %q, want value from %s", p.GatewayURL, gatewayURLEnvVar)
	}
	if p.GatewayModel != "tensorzero::function_name::chat" {
		t.Errorf("GatewayModel = %q, want value from %s", p.GatewayModel, gatewayModelEnvVar)
	}
	if p.GatewayAPIKey != "relay-quickstart" {
		t.Errorf("GatewayAPIKey = %q, want value from %s", p.GatewayAPIKey, gatewayAPIKeyEnvVar)
	}
}

func TestCreateMessage_DirectWhenGatewayURLUnset(t *testing.T) {
	var hitPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		if got := r.Header.Get("x-api-key"); got != "sk-ant-test" {
			t.Errorf("x-api-key = %q, want sk-ant-test", got)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	p := &AnthropicProvider{
		HTTPClient: srv.Client(),
		APIKey:     "sk-ant-test",
		Model:      "claude-sonnet-5",
		BaseURL:    srv.URL,
	}

	resp, err := p.createMessage(context.Background(), messagesRequest{Model: "claude-sonnet-5", MaxTokens: 100})
	if err != nil {
		t.Fatalf("createMessage() error = %v", err)
	}
	if hitPath != "/v1/messages" {
		t.Errorf("request path = %q, want /v1/messages (direct Anthropic transport, unchanged)", hitPath)
	}
	if extractText(resp.Content) != "ok" {
		t.Errorf("extractText() = %q, want ok", extractText(resp.Content))
	}
}

func TestCreateMessage_ViaGatewayWhenGatewayURLSet(t *testing.T) {
	var hitPath string
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok via relay"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	p := &AnthropicProvider{
		HTTPClient:    srv.Client(),
		Model:         "claude-sonnet-5",
		GatewayURL:    srv.URL,
		GatewayAPIKey: "relay-quickstart",
	}

	resp, err := p.createMessage(context.Background(), messagesRequest{Model: "claude-sonnet-5", MaxTokens: 100})
	if err != nil {
		t.Fatalf("createMessage() error = %v", err)
	}
	if hitPath != "/chat/completions" {
		t.Errorf("request path = %q, want /chat/completions (Relay's OpenAI-compatible transport)", hitPath)
	}
	if gotAuth != "Bearer relay-quickstart" {
		t.Errorf("authorization header = %q, want Bearer relay-quickstart", gotAuth)
	}
	if extractText(resp.Content) != "ok via relay" {
		t.Errorf("extractText() = %q, want ok via relay", extractText(resp.Content))
	}
}

func TestCreateMessage_ViaGatewayOmitsAuthHeaderWhenAPIKeyEmpty(t *testing.T) {
	var authHeaderSet bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaderSet = r.Header.Get("authorization") != ""
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	p := &AnthropicProvider{HTTPClient: srv.Client(), Model: "claude-sonnet-5", GatewayURL: srv.URL}

	if _, err := p.createMessage(context.Background(), messagesRequest{Model: "claude-sonnet-5"}); err != nil {
		t.Fatalf("createMessage() error = %v", err)
	}
	if authHeaderSet {
		t.Error("authorization header was set, want none when GatewayAPIKey is empty (matches an auth-disabled Relay quickstart deployment)")
	}
}

func TestCreateMessageDirect_ErrorsWithoutAPIKey(t *testing.T) {
	p := &AnthropicProvider{Model: "claude-sonnet-5"}

	if _, err := p.createMessage(context.Background(), messagesRequest{Model: "claude-sonnet-5"}); err == nil {
		t.Fatal("createMessage() error = nil, want error when ANTHROPIC_API_KEY/APIKey is unset and GatewayURL is also unset")
	}
}

// TestRunConversation_ViaGateway_ToolCallRoundTrip drives a full two-turn
// conversation through a fake Relay server: first turn returns a tool call,
// second turn (after the tool_result is sent back) returns final text. This
// is the exact loop in RunConversation; it must behave identically whether
// createMessage dispatches to Anthropic directly or to Relay.
func TestRunConversation_ViaGateway_ToolCallRoundTrip(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var req openAIChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		w.Header().Set("content-type", "application/json")
		switch callCount {
		case 1:
			// First turn: the model wants to call list_entries before
			// deciding on tightened spend controls.
			w.Write([]byte(`{
				"choices": [{
					"message": {
						"role": "assistant",
						"tool_calls": [{
							"id": "call_1",
							"type": "function",
							"function": {"name": "list_entries", "arguments": "{\"tenant_id\":\"t1\"}"}
						}]
					},
					"finish_reason": "tool_calls"
				}]
			}`))
		case 2:
			// Second turn must carry the tool result back as a
			// "tool"-role message keyed by tool_call_id.
			found := false
			for _, m := range req.Messages {
				if m.Role == "tool" && m.ToolCallID == "call_1" {
					found = true
					if m.Content == nil || *m.Content != "headroom: 340" {
						t.Errorf("tool result content = %v, want headroom: 340", m.Content)
					}
				}
			}
			if !found {
				t.Errorf("request messages = %+v, want a tool-role message for call_1", req.Messages)
			}
			w.Write([]byte(`{
				"choices": [{
					"message": {"role": "assistant", "content": "granted $340 of $600 requested"},
					"finish_reason": "stop"
				}]
			}`))
		default:
			t.Fatalf("unexpected third call to gateway")
		}
	}))
	defer srv.Close()

	p := &AnthropicProvider{
		HTTPClient: srv.Client(),
		Model:      "claude-sonnet-5",
		GatewayURL: srv.URL,
		MaxTokens:  100,
	}

	handler := func(ctx context.Context, toolName string, input json.RawMessage) (string, bool) {
		if toolName != "list_entries" {
			t.Errorf("toolName = %q, want list_entries", toolName)
		}
		return "headroom: 340", false
	}

	out, err := p.RunConversation(context.Background(), "system prompt", "deposit is late", nil, handler)
	if err != nil {
		t.Fatalf("RunConversation() error = %v", err)
	}
	if out != "granted $340 of $600 requested" {
		t.Errorf("RunConversation() = %q, want final text after the tool round-trip", out)
	}
	if callCount != 2 {
		t.Errorf("callCount = %d, want exactly 2 (one tool-call turn, one final turn)", callCount)
	}
}

// TestSendWithRetry_RetriesOnRetryableStatusThenSucceeds exercises
// sendWithRetry's retry loop (extracted in this change so both the direct
// Anthropic transport and the Relay gateway transport share identical
// retry/backoff behavior): a 429 on the first attempt must not be returned
// to the caller as a terminal error if a later attempt succeeds.
func TestSendWithRetry_RetriesOnRetryableStatusThenSucceeds(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok after retry"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	p := &AnthropicProvider{HTTPClient: srv.Client(), Model: "claude-sonnet-5", GatewayURL: srv.URL}

	resp, err := p.createMessage(context.Background(), messagesRequest{Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("createMessage() error = %v, want the 429 to be retried transparently", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one 429, one success)", attempts)
	}
	if extractText(resp.Content) != "ok after retry" {
		t.Errorf("extractText() = %q, want ok after retry", extractText(resp.Content))
	}
}

// TestSendWithRetry_NonRetryableStatusFailsImmediately verifies a
// non-retryable 4xx (e.g. a bad request to Relay) is not retried and is
// surfaced as an APIError callers can inspect.
func TestSendWithRetry_NonRetryableStatusFailsImmediately(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer srv.Close()

	p := &AnthropicProvider{HTTPClient: srv.Client(), Model: "claude-sonnet-5", GatewayURL: srv.URL}

	_, err := p.createMessage(context.Background(), messagesRequest{Model: "claude-sonnet-5"})
	if err == nil {
		t.Fatal("createMessage() error = nil, want an error on a non-retryable 400")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1 (no retry on a non-retryable status)", attempts)
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("err = %T, want *APIError", err)
	}
	if apiErr.Retryable() {
		t.Error("Retryable() = true, want false for a 400")
	}
}

func TestCreateMessageViaGateway_PropagatesNon2xxAsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid key"}`))
	}))
	defer srv.Close()

	p := &AnthropicProvider{HTTPClient: srv.Client(), Model: "claude-sonnet-5", GatewayURL: srv.URL}

	_, err := p.createMessage(context.Background(), messagesRequest{Model: "claude-sonnet-5"})
	if err == nil {
		t.Fatal("createMessage() error = nil, want an error on a 401 from the gateway")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want it to mention the 401 status", err)
	}
}
