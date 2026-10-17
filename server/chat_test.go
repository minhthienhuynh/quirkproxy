package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/minhthienhuynh/quirkproxy/model"
	"github.com/minhthienhuynh/quirkproxy/provider"
)

type fakeChat struct {
	fakeLister
	chatRes provider.ChatResult
	chatErr error
	rawBody io.ReadCloser
	rawErr  error
	chats   int
	lastReq provider.ChatRequest
}

func (f *fakeChat) ID() string { return f.id }

func (f *fakeChat) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityListModels, provider.CapabilityChat}
}

func (f *fakeChat) Chat(_ context.Context, req provider.ChatRequest) (provider.ChatResult, error) {
	f.chats++
	f.lastReq = req
	if f.chatErr != nil {
		return provider.ChatResult{}, f.chatErr
	}
	return f.chatRes, nil
}

func (f *fakeChat) ChatRaw(_ context.Context, _ provider.ChatRequest) (io.ReadCloser, error) {
	f.chats++
	if f.rawErr != nil {
		return nil, f.rawErr
	}
	return f.rawBody, nil
}

func chatReg(t *testing.T, p provider.ModelLister) *provider.Registry {
	t.Helper()
	reg := provider.NewRegistry()
	if err := reg.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

func postChat(t *testing.T, app *fiber.App, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", data, err)
	}
	return resp.StatusCode, got
}

func TestChatOK(t *testing.T) {
	fake := &fakeChat{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "mimo-v2.6-flash-free", Owner: "opencode"}}},
		chatRes: provider.ChatResult{Content: "OK"}}
	app := New(chatReg(t, fake), testLogger())

	code, got := postChat(t, app, map[string]any{
		"model":    "mimo-v2.6-flash-free",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, got %v", code, got)
	}
	if got["object"] != "chat.completion" {
		t.Fatalf("object = %v", got["object"])
	}
	choices, _ := got["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", got["choices"])
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["role"] != "assistant" || msg["content"] != "OK" {
		t.Fatalf("message = %v", msg)
	}
	if fake.chats != 1 {
		t.Fatalf("upstream chats = %d, want 1", fake.chats)
	}
}

func TestChatStreamRelayed(t *testing.T) {
	fake := &fakeChat{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}},
		rawBody: io.NopCloser(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"))}
	app := New(chatReg(t, fake), testLogger())

	raw, err := json.Marshal(map[string]any{
		"model": "m", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, `"content":"Hi"`) || !strings.HasSuffix(strings.TrimSpace(text), "data: [DONE]") {
		t.Fatalf("sse body = %q", text)
	}
	if !strings.Contains(text, `"model":"m"`) {
		t.Fatalf("model not rewritten: %q", text)
	}
	if fake.chats != 1 {
		t.Fatalf("upstream chats = %d, want 1", fake.chats)
	}
}

func TestChatUnknownModel(t *testing.T) {
	fake := &fakeChat{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}}}
	app := New(chatReg(t, fake), testLogger())

	code, _ := postChat(t, app, map[string]any{
		"model":    "gpt-paid",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if fake.chats != 0 {
		t.Fatal("upstream must not be touched on 404")
	}
}

func TestChatUpstream403(t *testing.T) {
	fake := &fakeChat{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}},
		chatErr: &provider.UpstreamError{Status: 403, Type: "FreeTierError", Message: "nope", Hint: "h"}}
	app := New(chatReg(t, fake), testLogger())

	code, got := postChat(t, app, map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != 403 {
		t.Fatalf("status = %d, want 403", code)
	}
	errObj, _ := got["error"].(map[string]any)
	if errObj["message"] != "nope" || errObj["hint"] != "h" {
		t.Fatalf("error = %v, want verbatim message + hint", got["error"])
	}
}

func TestChatToolCallsMapped(t *testing.T) {
	fake := &fakeChat{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}},
		chatRes: provider.ChatResult{ToolCalls: []provider.ToolCall{{ID: "c1", Name: "read", Arguments: "{}"}}}}
	app := New(chatReg(t, fake), testLogger())

	code, got := postChat(t, app, map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	choices, _ := got["choices"].([]any)
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %v", msg["tool_calls"])
	}
	if choices[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choices[0].(map[string]any)["finish_reason"])
	}
}

func TestChatArrayContent(t *testing.T) {
	fake := &fakeChat{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}},
		chatRes: provider.ChatResult{Content: "ok"}}
	app := New(chatReg(t, fake), testLogger())

	code, _ := postChat(t, app, map[string]any{
		"model": "m",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "Hello"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}},
			map[string]any{"type": "text", "text": "World"},
		}}},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(fake.lastReq.Messages) != 1 || fake.lastReq.Messages[0].Content != "Hello\nWorld" {
		t.Fatalf("mapped content = %+v", fake.lastReq.Messages)
	}
}

func TestChatDeadUpstream503(t *testing.T) {
	fake := &fakeChat{fakeLister: fakeLister{id: "opencode", listErr: provider.ErrNoCache}}
	app := New(chatReg(t, fake), testLogger())

	code, _ := postChat(t, app, map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if fake.chats != 0 {
		t.Fatal("upstream chat must not be touched when allowlist is down")
	}
}

func TestChatDuplicateFirstWins(t *testing.T) {
	mk := func(id, owner string) *fakeChat {
		return &fakeChat{fakeLister: fakeLister{id: id,
			models: []model.Model{{ID: "same", Owner: owner}}},
			chatRes: provider.ChatResult{Content: "from-" + id}}
	}
	reg := provider.NewRegistry()
	if err := reg.Register(mk("b", "b")); err != nil {
		t.Fatalf("Register b: %v", err)
	}
	if err := reg.Register(mk("a", "a")); err != nil {
		t.Fatalf("Register a: %v", err)
	}
	app := New(reg, testLogger())

	code, got := postChat(t, app, map[string]any{
		"model":    "same",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	choices, _ := got["choices"].([]any)
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "from-a" {
		t.Fatalf("content = %v, want lowest provider id first", msg["content"])
	}
}

func TestChatRelaySkipsUpstreamDoneAndBad(t *testing.T) {
	fake := &fakeChat{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}},
		rawBody: io.NopCloser(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n" +
				"data: {not json}\n\n" +
				"data: [DONE]\n\n"))}
	app := New(chatReg(t, fake), testLogger())

	raw, _ := json.Marshal(map[string]any{
		"model": "m", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if strings.Count(text, "data: [DONE]") != 1 {
		t.Fatalf("expected exactly one [DONE], got:\n%s", text)
	}
	if !strings.Contains(text, `"content":"Hi"`) {
		t.Fatalf("missing content chunk:\n%s", text)
	}
}

func postScoped(t *testing.T, app *fiber.App, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", data, err)
	}
	return resp.StatusCode, got
}

func scopedReg(t *testing.T) (*provider.Registry, *fakeChat) {
	t.Helper()
	fake := &fakeChat{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}},
		chatRes: provider.ChatResult{Content: "scoped-ok"}}
	reg := provider.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg, fake
}

func scopedBody() map[string]any {
	return map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
}

func TestChatScopedOK(t *testing.T) {
	reg, fake := scopedReg(t)
	app := New(reg, testLogger())
	code, got := postScoped(t, app, "/opencode/v1/chat/completions", scopedBody())
	if code != http.StatusOK {
		t.Fatalf("status = %d, got %v", code, got)
	}
	choices, _ := got["choices"].([]any)
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "scoped-ok" {
		t.Fatalf("message = %v", msg)
	}
	if fake.chats != 1 {
		t.Fatalf("upstream chats = %d, want 1", fake.chats)
	}
}

func TestChatScopedRejects(t *testing.T) {
	reg, fake := scopedReg(t)
	app := New(reg, testLogger())

	if code, _ := postScoped(t, app, "/nope/v1/chat/completions", scopedBody()); code != http.StatusNotFound {
		t.Fatalf("unknown provider status = %d, want 404", code)
	}
	other := dict(scopedBody(), "model", "other")
	if code, _ := postScoped(t, app, "/opencode/v1/chat/completions", other); code != http.StatusNotFound {
		t.Fatalf("foreign model status = %d, want 404", code)
	}
	reg.SetEnabled("opencode", false)
	if code, _ := postScoped(t, app, "/opencode/v1/chat/completions", scopedBody()); code != http.StatusNotFound {
		t.Fatalf("disabled provider status = %d, want 404", code)
	}
	if fake.chats != 0 {
		t.Fatalf("upstream chats = %d, want 0 (never touched)", fake.chats)
	}
}

func dict(base map[string]any, k string, v any) map[string]any {
	out := map[string]any{}
	for key, val := range base {
		out[key] = val
	}
	out[k] = v
	return out
}

func TestChatBadRequests(t *testing.T) {
	fake := &fakeChat{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}}}
	app := New(chatReg(t, fake), testLogger())

	for _, body := range []any{
		map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		map[string]any{"model": "m"},
	} {
		code, _ := postChat(t, app, body)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for %v", code, body)
		}
	}
	if fake.chats != 0 {
		t.Fatal("upstream must not be touched on 400")
	}
}

func TestChatScopedNonChatProvider(t *testing.T) {
	reg := provider.NewRegistry()
	plain := &fakeLister{id: "plain", models: []model.Model{{ID: "m", Owner: "plain"}}}
	if err := reg.Register(plain); err != nil {
		t.Fatalf("Register: %v", err)
	}
	app := New(reg, testLogger())

	code, _ := postScoped(t, app, "/plain/v1/chat/completions", scopedBody())
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestChatStreamUpstreamError(t *testing.T) {
	fake := &fakeChat{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}},
		rawErr: &provider.UpstreamError{Status: 429, Type: "RateLimit", Message: "slow down"}}
	app := New(chatReg(t, fake), testLogger())

	raw, _ := json.Marshal(map[string]any{
		"model": "m", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429 passthrough", resp.StatusCode)
	}
}

// Task 1.2: the handler parses tool_calls and tool_call_id from the request
// wire into provider.ChatMessage so a fallback can preserve the tool loop.
func TestChatRequestParsesToolHistory(t *testing.T) {
	fake := dualFixture()
	app := New(chatReg(t, fake), testLogger())

	postChat(t, app, map[string]any{
		"model": "chat-m",
		"messages": []any{
			map[string]any{"role": "user", "content": "add gemini"},
			map[string]any{"role": "assistant", "content": "checking", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"filePath":"c.json"}`}},
				map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "bash", "arguments": `{"command":"curl"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "{cfg}"},
			map[string]any{"role": "tool", "tool_call_id": "call_2", "content": "{out}"},
		},
	})

	msgs := fake.lastChat.Messages
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4", len(msgs))
	}
	if len(msgs[1].ToolCalls) != 2 || msgs[1].ToolCalls[0].Name != "read" || msgs[1].ToolCalls[1].ID != "call_2" {
		t.Fatalf("assistant ToolCalls not parsed: %+v", msgs[1].ToolCalls)
	}
	if msgs[2].ToolCallID != "call_1" || msgs[3].ToolCallID != "call_2" {
		t.Fatalf("ToolCallID not parsed: %q / %q", msgs[2].ToolCallID, msgs[3].ToolCallID)
	}
}

func TestChatSessionAndIdentityExtraction(t *testing.T) {
	fake := &fakeChat{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "mimo-v2.6-flash-free", Owner: "opencode"}}},
		chatRes: provider.ChatResult{Content: "OK"}}
	app := New(chatReg(t, fake), testLogger())

	raw, _ := json.Marshal(map[string]any{
		"model":    "mimo-v2.6-flash-free",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-id", "ses_test_123")
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()

	if fake.lastReq.SessionID != "ses_test_123" {
		t.Fatalf("SessionID = %q, want ses_test_123", fake.lastReq.SessionID)
	}
	if fake.lastReq.ClientIdentity == "" {
		t.Fatalf("ClientIdentity is empty")
	}

	raw2, _ := json.Marshal(map[string]any{
		"model":    "mimo-v2.6-flash-free",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"user":     "user_conversation_999",
	})
	req2, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw2))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := app.Test(req2)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp2.Body.Close()

	if fake.lastReq.SessionID != "user_conversation_999" {
		t.Fatalf("SessionID from user field = %q, want user_conversation_999", fake.lastReq.SessionID)
	}

	// Test 3: x-reset-session forces "new"
	req3, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("x-reset-session", "true")
	resp3, err := app.Test(req3)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp3.Body.Close()

	if fake.lastReq.SessionID != "new" {
		t.Fatalf("SessionID from x-reset-session = %q, want new", fake.lastReq.SessionID)
	}
}
