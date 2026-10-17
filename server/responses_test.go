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

type fakeResponses struct {
	fakeLister
	respRes provider.ResponsesResult
	respErr error
	rawBody io.ReadCloser
	rawErr  error
	calls   int
	lastReq provider.ResponsesRequest
}

func (f *fakeResponses) ID() string { return f.id }

func (f *fakeResponses) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityListModels, provider.CapabilityResponses}
}

func (f *fakeResponses) Responses(_ context.Context, req provider.ResponsesRequest) (provider.ResponsesResult, error) {
	f.calls++
	f.lastReq = req
	if f.respErr != nil {
		return provider.ResponsesResult{}, f.respErr
	}
	return f.respRes, nil
}

func (f *fakeResponses) ResponsesRaw(_ context.Context, _ provider.ResponsesRequest) (io.ReadCloser, error) {
	f.calls++
	if f.rawErr != nil {
		return nil, f.rawErr
	}
	return f.rawBody, nil
}

func responsesReg(t *testing.T, p provider.ModelLister) *provider.Registry {
	t.Helper()
	reg := provider.NewRegistry()
	if err := reg.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

func postResponses(t *testing.T, app *fiber.App, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
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

func TestResponsesNonStream(t *testing.T) {
	payload := []byte(`{"id":"resp-x","object":"response","model":"m","created_at":1,"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}]}`)
	fake := &fakeResponses{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}},
		respRes: provider.ResponsesResult{Payload: payload}}
	app := New(responsesReg(t, fake), testLogger())

	code, got := postResponses(t, app, map[string]any{
		"model": "m", "input": "hi",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, got %v", code, got)
	}
	if got["object"] != "response" {
		t.Fatalf("object = %v", got["object"])
	}
	if fake.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", fake.calls)
	}
}

func TestResponsesStreamRelayed(t *testing.T) {
	sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_up\",\"model\":\"m\",\"created_at\":1,\"output\":[]}}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"ENC\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_up\",\"model\":\"m\",\"created_at\":1,\"status\":\"completed\",\"output\":[{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"ENC\"}]}}\n\n"
	fake := &fakeResponses{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}},
		rawBody: io.NopCloser(strings.NewReader(sse))}
	app := New(responsesReg(t, fake), testLogger())

	raw, err := json.Marshal(map[string]any{
		"model": "m", "stream": true, "input": "hi",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
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
	if !strings.Contains(text, "event: response.created") {
		t.Fatalf("event line lost: %q", text)
	}
	if !strings.Contains(text, `"encrypted_content":"ENC"`) {
		t.Fatalf("reasoning item not pass-through: %q", text)
	}
	completedCount := strings.Count(text, "event: response.completed")
	if completedCount != 1 {
		t.Fatalf("response.completed count = %d, want 1 (proxy-emitted)", completedCount)
	}
	if !strings.Contains(text, `"model":"m"`) {
		t.Fatalf("model not rewritten: %q", text)
	}
	if fake.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", fake.calls)
	}
}

func TestResponsesUnknownModel(t *testing.T) {
	fake := &fakeResponses{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}}}
	app := New(responsesReg(t, fake), testLogger())

	code, _ := postResponses(t, app, map[string]any{
		"model": "paid-model", "input": "hi",
	})
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if fake.calls != 0 {
		t.Fatal("upstream must not be touched when model not in allowlist")
	}
}

func TestResponsesUpstream403(t *testing.T) {
	fake := &fakeResponses{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}},
		respErr: &provider.UpstreamError{Status: 403, Type: "FreeTierError", Message: "nope", Hint: "UA pin"}}
	app := New(responsesReg(t, fake), testLogger())

	code, got := postResponses(t, app, map[string]any{
		"model": "m", "input": "hi",
	})
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
	errObj, _ := got["error"].(map[string]any)
	if errObj["message"] != "nope" || errObj["hint"] != "UA pin" {
		t.Fatalf("error = %v", got["error"])
	}
}

func TestResponsesDeadUpstream503(t *testing.T) {
	fake := &fakeResponses{fakeLister: fakeLister{id: "opencode", listErr: provider.ErrNoCache}}
	app := New(responsesReg(t, fake), testLogger())

	code, _ := postResponses(t, app, map[string]any{
		"model": "m", "input": "hi",
	})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if fake.calls != 0 {
		t.Fatal("upstream must not be touched when allowlist is down")
	}
}

func TestResponsesScoped(t *testing.T) {
	fake := &fakeResponses{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}},
		respRes: provider.ResponsesResult{Payload: []byte(`{"id":"resp-x","object":"response","model":"m"}`)}}
	reg := provider.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatalf("Register: %v", err)
	}
	app := New(reg, testLogger())

	code, got := postScoped(t, app, "/opencode/v1/responses", map[string]any{
		"model": "m", "input": "hi",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, got %v", code, got)
	}
	if code, _ := postScoped(t, app, "/nope/v1/responses", map[string]any{
		"model": "m", "input": "hi",
	}); code != http.StatusNotFound {
		t.Fatalf("unknown provider status = %d, want 404", code)
	}
	if code, _ := postScoped(t, app, "/opencode/v1/responses", map[string]any{
		"model": "other", "input": "hi",
	}); code != http.StatusNotFound {
		t.Fatalf("model outside allowlist status = %d, want 404", code)
	}
}

func TestResponsesSessionAndIdentityExtraction(t *testing.T) {
	fake := &fakeResponses{fakeLister: fakeLister{id: "opencode",
		models: []model.Model{{ID: "m", Owner: "opencode"}}},
		respRes: provider.ResponsesResult{Payload: []byte(`{"id":"resp-x","object":"response","model":"m"}`)}}
	app := New(responsesReg(t, fake), testLogger())

	raw, _ := json.Marshal(map[string]any{
		"model": "m",
		"input": "hi",
	})
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-opencode-session", "ses_responses_456")
	req.Header.Set("Authorization", "Bearer test-auth")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()

	if fake.lastReq.SessionID != "ses_responses_456" {
		t.Fatalf("SessionID = %q, want ses_responses_456", fake.lastReq.SessionID)
	}
	if fake.lastReq.ClientIdentity == "" {
		t.Fatalf("ClientIdentity is empty")
	}
}
