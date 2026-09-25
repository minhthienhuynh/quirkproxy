package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/minhthienhuynh/quirkproxy/model"
	"github.com/minhthienhuynh/quirkproxy/provider"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

type fakeLister struct {
	id      string
	models  []model.Model
	err     error
	listErr error
}

func (f *fakeLister) ID() string {
	if f.id != "" {
		return f.id
	}
	return "fake"
}

func (f *fakeLister) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityListModels}
}

func (f *fakeLister) ListModels(_ context.Context) (provider.ListResult, error) {
	if f.listErr != nil {
		return provider.ListResult{}, f.listErr
	}
	if f.err != nil {
		return provider.ListResult{}, f.err
	}
	return provider.ListResult{Models: f.models}, nil
}

func testRegistry(t *testing.T, p provider.ModelLister) *provider.Registry {
	t.Helper()
	reg := provider.NewRegistry()
	if err := reg.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

func TestHealthz(t *testing.T) {
	app := New(testRegistry(t, &fakeLister{}), testLogger())

	req, err := http.NewRequest(http.MethodGet, "/healthz", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["status"] != "ok" {
		t.Fatalf("status field = %q, want %q", got["status"], "ok")
	}
}

func TestModelsOpenAICompatible(t *testing.T) {
	fake := &fakeLister{models: []model.Model{{
		ID:     "mimo-v2.6-flash-free",
		Name:   "MiMo-V2.6-Flash Free",
		Family: "mimo",
		Status: "active",
		Owner:  "fake",
		Cost:   model.Cost{},
		Limit:  model.Limit{Context: 200000, Output: 32000},
		Capabilities: model.Capabilities{
			ToolCall:         true,
			Reasoning:        true,
			Modalities:       model.Modalities{Input: []string{"text"}, Output: []string{"text"}},
			InterleavedField: "reasoning_content",
		},
		Variants:  map[string]model.Variant{},
		Transport: "@ai-sdk/openai-compatible",
		Endpoint:  "chat/completions",
		Created:   1790331791,
	}}}
	app := New(testRegistry(t, fake), testLogger())

	req, err := http.NewRequest(http.MethodGet, "/v1/models", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// A standard OpenAI client only reads the core shape.
	var core struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &core); err != nil {
		t.Fatalf("core unmarshal: %v", err)
	}
	if core.Object != "list" || len(core.Data) != 1 {
		t.Fatalf("core = %+v, want list with 1 item", core)
	}
	if core.Data[0].ID != "mimo-v2.6-flash-free" || core.Data[0].OwnedBy != "fake" {
		t.Fatalf("core item = %+v", core.Data[0])
	}
	// The enriched fields must be present too.
	var full struct {
		Data []struct {
			Name                string `json:"name"`
			ContextLength       int    `json:"context_length"`
			MaxCompletionTokens int    `json:"max_completion_tokens"`
			Capabilities        struct {
				Interleaved *struct {
					Field string `json:"field"`
				} `json:"interleaved"`
			} `json:"capabilities"`
			Quirk struct {
				Endpoint string `json:"endpoint"`
			} `json:"quirk"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &full); err != nil {
		t.Fatalf("full unmarshal: %v", err)
	}
	item := full.Data[0]
	if item.ContextLength != 200000 || item.MaxCompletionTokens != 32000 {
		t.Fatalf("limits = %d/%d, want 200000/32000", item.ContextLength, item.MaxCompletionTokens)
	}
	if item.Quirk.Endpoint != "chat/completions" {
		t.Fatalf("endpoint = %q", item.Quirk.Endpoint)
	}
	if item.Capabilities.Interleaved == nil || item.Capabilities.Interleaved.Field != "reasoning_content" {
		t.Fatalf("interleaved = %+v, want reasoning_content", item.Capabilities.Interleaved)
	}
	if age := resp.Header.Get("X-Quirk-Cache-Age"); age == "" {
		t.Fatal("missing X-Quirk-Cache-Age header")
	}
}

func TestModelsUpstreamDownNoCache(t *testing.T) {
	app := New(testRegistry(t, &fakeLister{err: provider.ErrNoCache}), testLogger())

	req, err := http.NewRequest(http.MethodGet, "/v1/models", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Error.Message == "" {
		t.Fatal("empty error message, want a clear message")
	}
}

func TestModelsUnexpectedError(t *testing.T) {
	app := New(testRegistry(t, &fakeLister{err: errors.New("boom")}), testLogger())

	req, err := http.NewRequest(http.MethodGet, "/v1/models", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestModelsDuplicateIDFirstWins(t *testing.T) {
	mkModel := func(owner string) model.Model {
		return model.Model{ID: "same-id", Name: owner, Status: "active", Owner: owner}
	}
	reg := provider.NewRegistry()
	if err := reg.Register(&fakeLister{id: "a", models: []model.Model{mkModel("a")}}); err != nil {
		t.Fatalf("Register a: %v", err)
	}
	if err := reg.Register(&fakeLister{id: "b", models: []model.Model{mkModel("b")}}); err != nil {
		t.Fatalf("Register b: %v", err)
	}
	app := New(reg, testLogger())

	req, err := http.NewRequest(http.MethodGet, "/v1/models", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Data) != 1 {
		t.Fatalf("items = %d, want 1 (duplicate dropped)", len(got.Data))
	}
	if got.Data[0].OwnedBy != "a" {
		t.Fatalf("kept = %q, want first provider a", got.Data[0].OwnedBy)
	}
}

func TestModelsScopedToProvider(t *testing.T) {
	reg := provider.NewRegistry()
	if err := reg.Register(&fakeLister{id: "a", models: []model.Model{{ID: "a-1", Owner: "a"}}}); err != nil {
		t.Fatalf("Register a: %v", err)
	}
	if err := reg.Register(&fakeLister{id: "b", models: []model.Model{{ID: "b-1", Owner: "b"}}}); err != nil {
		t.Fatalf("Register b: %v", err)
	}
	app := New(reg, testLogger())

	get := func(path string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, path, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return resp.StatusCode, got
	}

	code, got := get("/b/v1/models")
	if code != http.StatusOK {
		t.Fatalf("scoped status = %d, want 200", code)
	}
	data, _ := got["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "b-1" {
		t.Fatalf("scoped data = %v, want only b-1", data)
	}
	if code, _ := get("/nope/v1/models"); code != http.StatusNotFound {
		t.Fatalf("unknown provider status = %d, want 404", code)
	}

	reg.SetEnabled("b", false)
	if code, _ := get("/b/v1/models"); code != http.StatusNotFound {
		t.Fatalf("disabled provider status = %d, want 404", code)
	}
	if code, got := get("/v1/models"); code != http.StatusOK {
		t.Fatalf("aggregate status = %d, want 200", code)
	} else if data, _ := got["data"].([]any); len(data) != 1 {
		t.Fatalf("aggregate data = %v, want only a-1 after b disabled", data)
	}
}

func TestModelsScopedUpstreamDown(t *testing.T) {
	reg := provider.NewRegistry()
	if err := reg.Register(&fakeLister{id: "a", err: provider.ErrNoCache}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	app := New(reg, testLogger())

	req, err := http.NewRequest(http.MethodGet, "/a/v1/models", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
