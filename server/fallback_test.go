package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/minhthienhuynh/quirkproxy/model"
	"github.com/minhthienhuynh/quirkproxy/provider"
)

// fakeDual serves both chat and responses with endpoint-aware models.
type fakeDual struct {
	fakeLister
	chatRes   provider.ChatResult
	chatErr   error
	respRes   provider.ResponsesResult
	respErr   error
	chats     int
	responses int
	lastChat  provider.ChatRequest
	lastResp  provider.ResponsesRequest
}

func (f *fakeDual) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityListModels, provider.CapabilityChat, provider.CapabilityResponses}
}

func (f *fakeDual) Chat(_ context.Context, req provider.ChatRequest) (provider.ChatResult, error) {
	f.chats++
	f.lastChat = req
	if f.chatErr != nil {
		return provider.ChatResult{}, f.chatErr
	}
	return f.chatRes, nil
}

func (f *fakeDual) ChatRaw(_ context.Context, _ provider.ChatRequest) (io.ReadCloser, error) {
	return nil, &provider.UpstreamError{Status: 500, Message: "raw not used in fallback tests"}
}

func (f *fakeDual) Responses(_ context.Context, req provider.ResponsesRequest) (provider.ResponsesResult, error) {
	f.responses++
	f.lastResp = req
	if f.respErr != nil {
		return provider.ResponsesResult{}, f.respErr
	}
	return f.respRes, nil
}

func (f *fakeDual) ResponsesRaw(_ context.Context, _ provider.ResponsesRequest) (io.ReadCloser, error) {
	return nil, &provider.UpstreamError{Status: 500, Message: "raw not used in fallback tests"}
}

func dualFixture() *fakeDual {
	return &fakeDual{
		fakeLister: fakeLister{id: "opencode", models: []model.Model{
			{ID: "chat-m", Owner: "opencode", Endpoint: "chat/completions"},
			{ID: "resp-m", Owner: "opencode", Endpoint: "responses"},
		}},
		chatRes: provider.ChatResult{Content: "from-chat"},
		respRes: provider.ResponsesResult{Payload: []byte(
			`{"id":"resp-up","object":"response","model":"resp-m","created_at":1,"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"from-responses"}]}]}`)},
	}
}

func TestChatFallbackToResponses(t *testing.T) {
	fake := dualFixture()
	app := New(chatReg(t, fake), testLogger())

	code, got := postChat(t, app, map[string]any{
		"model":    "resp-m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, got %v", code, got)
	}
	choices, _ := got["choices"].([]any)
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "from-responses" {
		t.Fatalf("content = %v, want from-responses via fallback", msg["content"])
	}
	if fake.responses != 1 || fake.chats != 0 {
		t.Fatalf("calls chat=%d responses=%d, want 0/1", fake.chats, fake.responses)
	}
}

func TestChatFallbackStreamBuffered(t *testing.T) {
	fake := dualFixture()
	app := New(chatReg(t, fake), testLogger())

	raw, _ := json.Marshal(map[string]any{
		"model": "resp-m", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, "from-responses") {
		t.Fatalf("missing content: %q", text)
	}
	if strings.Count(text, "data: [DONE]") != 1 {
		t.Fatalf("want exactly one [DONE]: %q", text)
	}
}

func TestResponsesFallbackToChat(t *testing.T) {
	fake := dualFixture()
	app := New(responsesReg(t, fake), testLogger())

	code, got := postResponses(t, app, map[string]any{
		"model": "chat-m", "input": "hi",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, got %v", code, got)
	}
	out, _ := got["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output len = %d, want 1 message via fallback", len(out))
	}
	raw, _ := json.Marshal(out[0])
	if !strings.Contains(string(raw), "from-chat") {
		t.Fatalf("missing content via fallback: %v", out[0])
	}
	if fake.chats != 1 || fake.responses != 0 {
		t.Fatalf("calls chat=%d responses=%d, want 1/0", fake.chats, fake.responses)
	}
}

func TestResponsesFallbackStreamBuffered(t *testing.T) {
	fake := dualFixture()
	app := New(responsesReg(t, fake), testLogger())

	raw, _ := json.Marshal(map[string]any{
		"model": "chat-m", "stream": true, "input": "hi",
	})
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, "from-chat") {
		t.Fatalf("missing content: %q", text)
	}
	if strings.Count(text, "event: response.completed") != 1 {
		t.Fatalf("want exactly one completed: %q", text)
	}
}

func TestChatFallbackMisconfigured502(t *testing.T) {
	fake := &fakeChat{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "resp-only", Owner: "opencode", Endpoint: "responses"}}},
		chatRes: provider.ChatResult{Content: "unreachable"}}
	app := New(chatReg(t, fake), testLogger())

	code, _ := postChat(t, app, map[string]any{
		"model":    "resp-only",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (provider owns model but lacks responses completer)", code)
	}
	if fake.chats != 0 {
		t.Fatal("upstream must not be touched when no capable completer exists")
	}
}

func TestChatScopedMisconfigured502(t *testing.T) {
	fake := &fakeChat{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "resp-only", Owner: "opencode", Endpoint: "responses"}}},
		chatRes: provider.ChatResult{Content: "unreachable"}}
	reg := provider.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatalf("Register: %v", err)
	}
	app := New(reg, testLogger())

	code, _ := postScoped(t, app, "/opencode/v1/chat/completions", map[string]any{
		"model":    "resp-only",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 like the aggregate route", code)
	}
}

func TestResponsesFallbackMisconfigured502(t *testing.T) {
	fake := &fakeResponses{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "chat-only", Owner: "opencode", Endpoint: "chat/completions"}}},
		respRes: provider.ResponsesResult{Payload: []byte(`{"id":"x"}`)}}
	app := New(responsesReg(t, fake), testLogger())

	code, _ := postResponses(t, app, map[string]any{
		"model": "chat-only", "input": "hi",
	})
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (provider owns model but lacks chat completer)", code)
	}
	if fake.calls != 0 {
		t.Fatal("upstream must not be touched when no capable completer exists")
	}
}

func TestChatScopedFallbackToResponses(t *testing.T) {
	fake := dualFixture()
	reg := provider.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatalf("Register: %v", err)
	}
	app := New(reg, testLogger())

	code, got := postScoped(t, app, "/opencode/v1/chat/completions", map[string]any{
		"model":    "resp-m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, got %v", code, got)
	}
	choices, _ := got["choices"].([]any)
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "from-responses" {
		t.Fatalf("content = %v, want from-responses via scoped fallback", msg["content"])
	}
}

func TestResponsesScopedFallbackToChat(t *testing.T) {
	fake := dualFixture()
	reg := provider.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatalf("Register: %v", err)
	}
	app := New(reg, testLogger())

	code, got := postScoped(t, app, "/opencode/v1/responses", map[string]any{
		"model": "chat-m", "input": "hi",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, got %v", code, got)
	}
	out, _ := got["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output len = %d, want 1 message via scoped fallback", len(out))
	}
}

func TestChatFallbackUpstream403Passthrough(t *testing.T) {
	fake := dualFixture()
	fake.respErr = &provider.UpstreamError{Status: 403, Type: "FreeTierError", Message: "gate", Hint: "UA pin"}
	app := New(chatReg(t, fake), testLogger())

	code, got := postChat(t, app, map[string]any{
		"model":    "resp-m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != 403 {
		t.Fatalf("status = %d, want 403 passthrough", code)
	}
	errObj, _ := got["error"].(map[string]any)
	if errObj["message"] != "gate" || errObj["hint"] != "UA pin" {
		t.Fatalf("error = %v, want verbatim message + hint", got["error"])
	}
}

func TestResponsesFallbackUpstream403Passthrough(t *testing.T) {
	fake := dualFixture()
	fake.chatErr = &provider.UpstreamError{Status: 403, Type: "FreeTierError", Message: "gate", Hint: "UA pin"}
	app := New(responsesReg(t, fake), testLogger())

	code, got := postResponses(t, app, map[string]any{
		"model": "chat-m", "input": "hi",
	})
	if code != 403 {
		t.Fatalf("status = %d, want 403 passthrough", code)
	}
	errObj, _ := got["error"].(map[string]any)
	if errObj["message"] != "gate" {
		t.Fatalf("error = %v, want verbatim message", got["error"])
	}
}

func TestChatFallbackFirstWins(t *testing.T) {
	mk := func(id string) *fakeDual {
		d := dualFixture()
		d.fakeLister.id = id
		d.chatRes = provider.ChatResult{Content: "from-" + id}
		d.respRes = provider.ResponsesResult{Payload: []byte(
			`{"id":"r","object":"response","model":"resp-m","created_at":1,"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"resp-from-` + id + `"}]}]}`)}
		return d
	}
	reg := provider.NewRegistry()
	if err := reg.Register(mk("b")); err != nil {
		t.Fatalf("Register b: %v", err)
	}
	if err := reg.Register(mk("a")); err != nil {
		t.Fatalf("Register a: %v", err)
	}
	app := New(reg, testLogger())

	code, got := postChat(t, app, map[string]any{
		"model":    "resp-m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	choices, _ := got["choices"].([]any)
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "resp-from-a" {
		t.Fatalf("content = %v, want lowest provider id first", msg["content"])
	}
}

func TestChatFallbackRetryAfterHeader(t *testing.T) {
	fake := dualFixture()
	fake.respErr = &provider.UpstreamError{Status: 429, Type: "RateLimit", Message: "slow", RetryAfter: "120"}
	app := New(chatReg(t, fake), testLogger())

	raw, _ := json.Marshal(map[string]any{
		"model":    "resp-m",
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
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "120" {
		t.Fatalf("Retry-After = %q, want 120", got)
	}
}

func TestResponsesFallbackEmptyInput400(t *testing.T) {
	fake := dualFixture()
	app := New(responsesReg(t, fake), testLogger())

	code, _ := postResponses(t, app, map[string]any{
		"model": "chat-m", "input": []any{},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (empty input cannot map to chat messages)", code)
	}
	if fake.chats != 0 {
		t.Fatal("upstream chat must not be touched on 400")
	}
}

func TestChatFallbackIncompleteKeepsTruncatedOutput(t *testing.T) {
	fake := dualFixture()
	fake.respRes = provider.ResponsesResult{Payload: []byte(
		`{"id":"resp-up","object":"response","model":"resp-m","created_at":1,"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial answer"}]}]}`)}
	app := New(chatReg(t, fake), testLogger())

	code, got := postChat(t, app, map[string]any{
		"model":    "resp-m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (incomplete truncation must not 502), got %v", code, got)
	}
	choices, _ := got["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", got["choices"])
	}
	ch, _ := choices[0].(map[string]any)
	msg, _ := ch["message"].(map[string]any)
	if msg["content"] != "partial answer" {
		t.Fatalf("content = %v, want partial answer", msg["content"])
	}
	if ch["finish_reason"] != "length" {
		t.Fatalf("finish_reason = %v, want length", ch["finish_reason"])
	}
}

func TestChatFallbackIncompleteStreamFinishReason(t *testing.T) {
	fake := dualFixture()
	fake.respRes = provider.ResponsesResult{Payload: []byte(
		`{"id":"resp-up","object":"response","model":"resp-m","created_at":1,"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial answer"}]}]}`)}
	app := New(chatReg(t, fake), testLogger())

	raw, _ := json.Marshal(map[string]any{
		"model": "resp-m", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"finish_reason":"length"`) {
		t.Fatalf("missing finish_reason length in stream: %q", body)
	}
	if !strings.Contains(string(body), `"partial answer"`) {
		t.Fatalf("missing partial content in stream: %q", body)
	}
}

func TestChatFallbackFailedPayloadSurfacesUpstreamError(t *testing.T) {
	fake := dualFixture()
	fake.respRes = provider.ResponsesResult{Payload: []byte(
		`{"id":"resp-up","object":"response","model":"resp-m","created_at":1,"status":"failed","error":{"message":"quota exhausted"},"output":[]}`)}
	app := New(chatReg(t, fake), testLogger())

	code, got := postChat(t, app, map[string]any{
		"model":    "resp-m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, got %v", code, got)
	}
	errObj, _ := got["error"].(map[string]any)
	if errObj["type"] != "UpstreamResponseFailed" {
		t.Fatalf("type = %v, want UpstreamResponseFailed (not wrapped as TranslateError)", errObj["type"])
	}
	if errObj["message"] != "quota exhausted" {
		t.Fatalf("message = %v, want verbatim upstream message", errObj["message"])
	}
}
