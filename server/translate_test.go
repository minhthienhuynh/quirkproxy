package server

import (
	"encoding/json"
	"errors"
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
	items, ok := got.Input.([]any)
	if !ok {
		t.Fatalf("input = %T, want []any structured items", got.Input)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	// Genuine shape: no explicit "message" type; system content is a string.
	m0 := itemMap(t, items[0])
	if m0["role"] != "system" {
		t.Fatalf("items[0].role = %v, want system", m0["role"])
	}
	if _, hasType := m0["type"]; hasType {
		t.Fatalf("message item must not carry a type field: %+v", m0)
	}
	if s, ok := m0["content"].(string); !ok || s != "sys" {
		t.Fatalf("system content = %#v, want string \"sys\"", m0["content"])
	}
	// User content is an input_text array.
	m1 := itemMap(t, items[1])
	if m1["role"] != "user" {
		t.Fatalf("items[1].role = %v, want user", m1["role"])
	}
	parts, ok := m1["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("user content = %#v, want one-part array", m1["content"])
	}
	if p := itemMap(t, parts[0]); p["type"] != "input_text" || p["text"] != "hi" {
		t.Fatalf("user part = %+v, want input_text/hi", p)
	}
}

// itemMap asserts it is a JSON object and returns it.
func itemMap(t *testing.T, it any) map[string]any {
	t.Helper()
	m, ok := it.(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want map", it)
	}
	return m
}

// Task 2.2/2.3: multi-turn tool history maps to structured input items
// (function_call / function_call_output) with call_id linkage preserved, not a
// flat "role: content" string. function_call carries call_id only (no item id).
func TestChatToResponsesPreservesToolLoop(t *testing.T) {
	req := provider.ChatRequest{
		Model: "muse-spark",
		Messages: []provider.ChatMessage{
			{Role: "user", Content: "add gemini"},
			{Role: "assistant", Content: "checking", ToolCalls: []provider.ToolCall{
				{ID: "call_1", Name: "read", Arguments: `{"filePath":"c.json"}`},
				{ID: "call_2", Name: "bash", Arguments: `{"command":"curl"}`},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "{cfg}"},
			{Role: "tool", ToolCallID: "call_2", Content: "{out}"},
		},
	}
	got := chatToResponses(req)
	items, ok := got.Input.([]any)
	if !ok {
		t.Fatalf("input = %T, want []any", got.Input)
	}
	// user msg, assistant msg, call_1, call_2, out_1, out_2 = 6 items
	if len(items) != 6 {
		t.Fatalf("items = %d, want 6 (msg,msg,call,call,out,out): %+v", len(items), items)
	}
	var calls, outputs []map[string]any
	for _, it := range items {
		m := itemMap(t, it)
		switch m["type"] {
		case "function_call":
			calls = append(calls, m)
		case "function_call_output":
			outputs = append(outputs, m)
		}
	}
	if len(calls) != 2 || len(outputs) != 2 {
		t.Fatalf("calls=%d outputs=%d, want 2/2", len(calls), len(outputs))
	}
	callIDs := map[string]bool{}
	for _, c := range calls {
		callIDs[c["call_id"].(string)] = true
		if c["name"].(string) == "" || c["arguments"].(string) == "" {
			t.Fatalf("call missing name/arguments: %+v", c)
		}
		if _, hasID := c["id"]; hasID {
			t.Fatalf("function_call must not set an item id (only call_id): %+v", c)
		}
	}
	for _, o := range outputs {
		id, _ := o["call_id"].(string)
		if !callIDs[id] {
			t.Fatalf("output call_id %v not linked to any call", o["call_id"])
		}
		if s, _ := o["output"].(string); s == "" {
			t.Fatalf("output missing text: %+v", o)
		}
	}
}

// Task 2.2 back-compat: a lone text message still maps to a plain string.
func TestChatToResponsesSingleTextBackCompat(t *testing.T) {
	got := chatToResponses(provider.ChatRequest{
		Model: "m", Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if s, ok := got.Input.(string); !ok || s != "hi" {
		t.Fatalf("input = %#v, want string \"hi\"", got.Input)
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
	sse := synthesizeChatSSE(provider.ChatResult{Content: "Hi"}, "m", "chatcmpl-x", 99)
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

func TestTranslateResponsesPayloadIncompleteFinishReason(t *testing.T) {
	tests := []struct {
		name    string
		details string
		want    string
	}{
		{"max_output_tokens truncation", `"incomplete_details":{"reason":"max_output_tokens"}`, "length"},
		{"content_filter cut", `"incomplete_details":{"reason":"content_filter"}`, "content_filter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte(`{"id":"resp-x","model":"m","status":"incomplete",` + tt.details + `,"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`)
			res, err := responsesPayloadToChatResult(payload)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if res.Content != "partial" {
				t.Fatalf("content = %q, want partial output kept", res.Content)
			}
			if res.FinishReason != tt.want {
				t.Fatalf("finish = %q, want %q", res.FinishReason, tt.want)
			}
		})
	}
}

func TestTranslateResponsesPayloadIncompleteEmptyOutput(t *testing.T) {
	for _, payload := range []string{
		`{"id":"resp-x","model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`,
		`{"id":"resp-x","model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"reasoning","id":"rs_1"}]}`,
	} {
		res, err := responsesPayloadToChatResult([]byte(payload))
		if err != nil {
			t.Fatalf("truncation without usable output must not error: %s (%v)", payload, err)
		}
		if res.Content != "" || len(res.ToolCalls) != 0 {
			t.Fatalf("result = %+v, want empty content and no tool calls", res)
		}
		if res.FinishReason != "length" {
			t.Fatalf("finish = %q, want length", res.FinishReason)
		}
	}
}

func TestTranslateResponsesPayloadFailedErrors(t *testing.T) {
	payload := []byte(`{"id":"resp-x","model":"m","status":"failed","error":{"message":"quota exhausted"},"output":[]}`)
	_, err := responsesPayloadToChatResult(payload)
	var ue *provider.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want UpstreamError", err)
	}
	if ue.Message != "quota exhausted" {
		t.Fatalf("message = %q, want upstream message", ue.Message)
	}
}

func TestSynthesizeChatSSETruncationFinishReason(t *testing.T) {
	sse := synthesizeChatSSE(provider.ChatResult{Content: "cut", FinishReason: "length"}, "m", "chatcmpl-y", 99)
	if !strings.Contains(sse, `"finish_reason":"length"`) {
		t.Fatalf("missing finish_reason length: %q", sse)
	}
}
