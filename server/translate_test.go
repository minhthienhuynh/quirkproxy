package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/minhthienhuynh/quirkproxy/provider"
)

func TestTranslateChatToResponsesSingleMessage(t *testing.T) {
	temp := 0.7
	req := provider.ChatRequest{
		Model:       "muse-spark",
		Messages:    []provider.ChatMessage{{Role: "user", Content: "hi"}},
		Tools:       []provider.ChatTool{{Name: "read", Description: "d", Parameters: map[string]any{"type": "object"}}},
		ToolChoice:  "auto",
		Temperature: &temp,
		MaxTokens:   intPtr(100),
	}
	got := chatToResponses(req)
	if got.Model != "muse-spark" {
		t.Fatalf("model = %q", got.Model)
	}
	if s, _ := got.Input.(string); s != "hi" {
		t.Fatalf("input = %v, want plain text", got.Input)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "read" {
		t.Fatalf("tools = %+v", got.Tools)
	}
	if got.MaxOutputTokens == nil || *got.MaxOutputTokens != 100 {
		t.Fatalf("max_output_tokens = %v", got.MaxOutputTokens)
	}
	if got.Reasoning != nil {
		t.Fatalf("reasoning must be nil (temperature dropped), got %v", got.Reasoning)
	}
}

func TestTranslateChatToResponsesMultiMessage(t *testing.T) {
	req := provider.ChatRequest{
		Model: "muse-spark",
		Messages: []provider.ChatMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
		},
	}
	got := chatToResponses(req)
	s, _ := got.Input.(string)
	if !strings.Contains(s, "system: sys") || !strings.Contains(s, "user: hi") {
		t.Fatalf("input = %q, want role-prefixed lines", s)
	}
}

func TestTranslateResponsesToChat(t *testing.T) {
	req := provider.ResponsesRequest{
		Model:           "m",
		Input:           "hello",
		Tools:           []provider.ResponsesTool{{Name: "bash"}},
		Reasoning:       map[string]any{"effort": "high"},
		MaxOutputTokens: intPtr(50),
	}
	got := responsesToChat(req)
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" || got.Messages[0].Content != "hello" {
		t.Fatalf("messages = %+v", got.Messages)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "bash" {
		t.Fatalf("tools = %+v", got.Tools)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 50 {
		t.Fatalf("max_tokens = %v", got.MaxTokens)
	}
	if got.Temperature != nil {
		t.Fatalf("temperature must be nil (reasoning dropped)")
	}
}

func TestTranslateResponsesPayloadToChat(t *testing.T) {
	payload := []byte(`{"id":"resp-x","model":"m","output":[{"type":"message","content":[{"type":"output_text","text":"Hello"}]},{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},{"type":"reasoning","id":"rs_1"}]}`)
	res, err := responsesPayloadToChatResult(payload)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Content != "Hello" {
		t.Fatalf("content = %q", res.Content)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "c1" || res.ToolCalls[0].Name != "read" {
		t.Fatalf("toolcalls = %+v", res.ToolCalls)
	}
}

func TestTranslateResponsesPayloadEmptyOutputErrors(t *testing.T) {
	for _, payload := range []string{
		`{"id":"resp-x","model":"m","output":[]}`,
		`{"id":"resp-x","model":"m","output":[{"type":"reasoning","id":"rs_1"}]}`,
	} {
		if _, err := responsesPayloadToChatResult([]byte(payload)); err == nil {
			t.Fatalf("want error for payload without usable output: %s", payload)
		}
	}
}

func TestTranslateChatResultToResponsesPayload(t *testing.T) {
	res := provider.ChatResult{
		Content:   "OK",
		ToolCalls: []provider.ToolCall{{ID: "c1", Name: "read", Arguments: "{}"}},
	}
	raw := chatResultToResponsesPayload(res, "m", "resp-test", 123)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["id"] != "resp-test" || got["model"] != "m" {
		t.Fatalf("envelope = %v", got)
	}
	out, _ := got["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("output len = %d, want message + function_call", len(out))
	}
}

func TestSynthesizeChatSSE(t *testing.T) {
	sse := synthesizeChatSSE("Hi", nil, "m", "chatcmpl-x", 99)
	if !strings.Contains(sse, `"content":"Hi"`) {
		t.Fatalf("missing content: %q", sse)
	}
	if strings.Count(sse, "data: [DONE]") != 1 {
		t.Fatalf("want exactly one [DONE]: %q", sse)
	}
	if !strings.Contains(sse, `"model":"m"`) {
		t.Fatalf("model not rewritten: %q", sse)
	}
}

func TestSynthesizeResponsesSSE(t *testing.T) {
	payload := []byte(`{"id":"resp-up","model":"m","created_at":1,"status":"completed","output":[]}`)
	sse := synthesizeResponsesSSE(payload, "m", "resp-new", 99)
	if !strings.Contains(sse, "event: response.created") {
		t.Fatalf("missing created: %q", sse)
	}
	if strings.Count(sse, "event: response.completed") != 1 {
		t.Fatalf("want exactly one completed: %q", sse)
	}
	if !strings.Contains(sse, `"model":"m"`) {
		t.Fatalf("model not rewritten: %q", sse)
	}
}

func intPtr(v int) *int { return &v }
