package llm

import (
	"encoding/json"
	"testing"
)

func TestGatewayModel(t *testing.T) {
	t.Run("uses GatewayModel when set", func(t *testing.T) {
		p := &AnthropicProvider{Model: "claude-sonnet-5", GatewayModel: "tensorzero::function_name::chat"}
		if got := p.gatewayModel(); got != "tensorzero::function_name::chat" {
			t.Fatalf("gatewayModel() = %q, want tensorzero selector", got)
		}
	})

	t.Run("falls back to Model when GatewayModel is empty", func(t *testing.T) {
		p := &AnthropicProvider{Model: "claude-sonnet-5"}
		if got := p.gatewayModel(); got != "claude-sonnet-5" {
			t.Fatalf("gatewayModel() = %q, want fallback to Model", got)
		}
	})
}

func TestToGatewayRequest(t *testing.T) {
	p := &AnthropicProvider{Model: "claude-sonnet-5", GatewayModel: "tensorzero::function_name::chat"}

	req := messagesRequest{
		Model:     "claude-sonnet-5",
		MaxTokens: 4096,
		System:    "You may grant up to exactly 10000.",
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "deposit is late"}}},
		},
		Tools: []Tool{
			{Name: "ration_bridge_funding", Description: "grant a bridge", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}

	out, err := toGatewayRequest(p, req)
	if err != nil {
		t.Fatalf("toGatewayRequest() error = %v", err)
	}

	if out.Model != "tensorzero::function_name::chat" {
		t.Errorf("Model = %q, want the configured gateway selector", out.Model)
	}
	if out.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %d, want 4096", out.MaxTokens)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want 2 (system + user)", len(out.Messages))
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content == nil || *out.Messages[0].Content != req.System {
		t.Errorf("Messages[0] = %+v, want the system prompt as a system-role message", out.Messages[0])
	}
	if out.Messages[1].Role != "user" || out.Messages[1].Content == nil || *out.Messages[1].Content != "deposit is late" {
		t.Errorf("Messages[1] = %+v, want the user text", out.Messages[1])
	}
	if len(out.Tools) != 1 || out.Tools[0].Type != "function" || out.Tools[0].Function.Name != "ration_bridge_funding" {
		t.Errorf("Tools = %+v, want one function tool named ration_bridge_funding", out.Tools)
	}
}

func TestToGatewayRequest_NoSystemPrompt(t *testing.T) {
	p := &AnthropicProvider{Model: "claude-sonnet-5"}
	req := messagesRequest{
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}

	out, err := toGatewayRequest(p, req)
	if err != nil {
		t.Fatalf("toGatewayRequest() error = %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("len(Messages) = %d, want 1 (no system message when System is empty)", len(out.Messages))
	}
}

func TestToGatewayMessages_TextOnly(t *testing.T) {
	msg := Message{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hello"}}}

	out, err := toGatewayMessages(msg)
	if err != nil {
		t.Fatalf("toGatewayMessages() error = %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if out[0].Role != "user" || out[0].Content == nil || *out[0].Content != "hello" {
		t.Errorf("out[0] = %+v, want {Role: user, Content: hello}", out[0])
	}
	if len(out[0].ToolCalls) != 0 {
		t.Errorf("ToolCalls = %+v, want none for a text-only message", out[0].ToolCalls)
	}
}

func TestToGatewayMessages_ToolUse(t *testing.T) {
	msg := Message{
		Role: "assistant",
		Content: []ContentBlock{
			{Type: "text", Text: "checking headroom"},
			{Type: "tool_use", ID: "toolu_1", Name: "list_entries", Input: json.RawMessage(`{"tenant_id":"t1"}`)},
		},
	}

	out, err := toGatewayMessages(msg)
	if err != nil {
		t.Fatalf("toGatewayMessages() error = %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1 (text + tool_use collapse into one assistant message)", len(out))
	}
	m := out[0]
	if m.Role != "assistant" || m.Content == nil || *m.Content != "checking headroom" {
		t.Errorf("message content = %+v, want the text block preserved", m)
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "toolu_1" || m.ToolCalls[0].Function.Name != "list_entries" {
		t.Errorf("ToolCalls = %+v, want one call named list_entries with id toolu_1", m.ToolCalls)
	}
	if m.ToolCalls[0].Function.Arguments != `{"tenant_id":"t1"}` {
		t.Errorf("Arguments = %q, want the raw JSON input string", m.ToolCalls[0].Function.Arguments)
	}
}

// TestToGatewayMessages_MultipleToolResults verifies the case
// RunConversation always produces: one user message carrying several
// tool_result blocks for one turn's parallel tool calls. OpenAI's
// chat.completions has no multi-result message shape, so each block must
// become its own "tool"-role message, in order, with no accompanying
// user-role message added.
func TestToGatewayMessages_MultipleToolResults(t *testing.T) {
	msg := Message{
		Role: "user",
		Content: []ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_1", Content: "headroom: 340"},
			{Type: "tool_result", ToolUseID: "toolu_2", Content: "deposit: late", IsError: false},
		},
	}

	out, err := toGatewayMessages(msg)
	if err != nil {
		t.Fatalf("toGatewayMessages() error = %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (one per tool_result block, no wrapping user message)", len(out))
	}
	for i, want := range []struct {
		id      string
		content string
	}{
		{"toolu_1", "headroom: 340"},
		{"toolu_2", "deposit: late"},
	} {
		if out[i].Role != "tool" {
			t.Errorf("out[%d].Role = %q, want tool", i, out[i].Role)
		}
		if out[i].ToolCallID != want.id {
			t.Errorf("out[%d].ToolCallID = %q, want %q", i, out[i].ToolCallID, want.id)
		}
		if out[i].Content == nil || *out[i].Content != want.content {
			t.Errorf("out[%d].Content = %v, want %q", i, out[i].Content, want.content)
		}
	}
}

func TestToGatewayMessages_UnsupportedBlockType(t *testing.T) {
	msg := Message{Role: "user", Content: []ContentBlock{{Type: "image"}}}

	if _, err := toGatewayMessages(msg); err == nil {
		t.Fatal("toGatewayMessages() error = nil, want an error for an unsupported block type")
	}
}

// unmarshalGatewayResponse decodes raw JSON the way the real HTTP transport
// does, so these tests exercise the actual wire shape rather than
// constructing the (unexported, anonymous-struct-typed) response value by
// hand.
func unmarshalGatewayResponse(t *testing.T, raw string) *openAIChatResponse {
	t.Helper()
	var resp openAIChatResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("json.Unmarshal(openAIChatResponse) error = %v", err)
	}
	return &resp
}

func TestFromGatewayResponse_Text(t *testing.T) {
	resp := unmarshalGatewayResponse(t, `{
		"choices": [{
			"message": {"role": "assistant", "content": "granted $340 of $600 requested"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 120, "completion_tokens": 40}
	}`)

	out, err := fromGatewayResponse(resp)
	if err != nil {
		t.Fatalf("fromGatewayResponse() error = %v", err)
	}
	const wantText = "granted $340 of $600 requested"
	if extractText(out.Content) != wantText {
		t.Errorf("extractText() = %q, want %q", extractText(out.Content), wantText)
	}
	if out.StopReason != "stop" {
		t.Errorf("StopReason = %q, want stop (passed through unchanged)", out.StopReason)
	}
	if out.Usage.InputTokens != 120 || out.Usage.OutputTokens != 40 {
		t.Errorf("Usage = %+v, want InputTokens=120, OutputTokens=40", out.Usage)
	}
}

func TestFromGatewayResponse_ToolCalls(t *testing.T) {
	resp := unmarshalGatewayResponse(t, `{
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
	}`)

	out, err := fromGatewayResponse(resp)
	if err != nil {
		t.Fatalf("fromGatewayResponse() error = %v", err)
	}
	// RunConversation branches on stop_reason == "tool_use" to decide
	// whether to execute tool_use blocks; OpenAI's "tool_calls" finish
	// reason must be translated to that exact string or the loop would
	// silently stop instead of executing the call.
	if out.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use (translated from OpenAI's tool_calls)", out.StopReason)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "tool_use" || out.Content[0].Name != "list_entries" || out.Content[0].ID != "call_1" {
		t.Errorf("Content = %+v, want one tool_use block named list_entries with id call_1", out.Content)
	}
	if string(out.Content[0].Input) != `{"tenant_id":"t1"}` {
		t.Errorf("Input = %s, want the raw arguments string", out.Content[0].Input)
	}
}

func TestFromGatewayResponse_NoChoices(t *testing.T) {
	if _, err := fromGatewayResponse(&openAIChatResponse{}); err == nil {
		t.Fatal("fromGatewayResponse() error = nil, want an error when there are no choices")
	}
}
