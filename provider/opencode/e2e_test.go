package opencode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/minhthienhuynh/quirkproxy/config"
	"github.com/minhthienhuynh/quirkproxy/provider"
)

// slowResponsesStub serves POST /responses with a SSE stream whose terminal
// event is delayed by d, simulating a slow generation.
func slowResponsesStub(d time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		time.Sleep(d)
		_, _ = fmt.Fprint(w, "event: response.completed\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_up\",\"object\":\"response\",\"model\":\"m\",\"created_at\":1,\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"slow-ok\"}]}]}}\n\n")
		fl.Flush()
	}))
}

// newStubProvider builds a real opencode Provider whose inference endpoint is
// the given stub, with the generation bound set to genTimeout.
func newStubProvider(stubURL string, genTimeout time.Duration) *Provider {
	return New(config.Provider{
		ID:   "opencode",
		Type: "opencode",
		Options: map[string]any{
			"zen_api_url":                  stubURL,
			"inference_generation_timeout": genTimeout.String(),
			"inference_header_timeout":     "5s",
		},
	}, nil)
}

// Task 3.1 (regression): a responses-endpoint model whose generation outlasts
// what a fixed total request timeout would allow must still return the full
// folded payload through the real client — not get cut mid-stream. The absence
// of the old 15s total cutoff is asserted on the client in
// TestInferenceAndCatalogClientBounds; here we prove the whole real-client path
// (postToPath -> aggregateResponses) completes for a slow stream.
func TestResponsesSlowStreamCompletes(t *testing.T) {
	stub := slowResponsesStub(300 * time.Millisecond)
	defer stub.Close()

	p := newStubProvider(stub.URL, 2*time.Second)
	res, err := p.Responses(context.Background(), provider.ResponsesRequest{Model: "m", Input: "hi"})
	if err != nil {
		t.Fatalf("Responses: %v", err)
	}
	if !strings.Contains(string(res.Payload), "slow-ok") {
		t.Fatalf("payload missing folded content: %s", res.Payload)
	}
}

// A generation that outlasts the configured generation bound fails, and the
// failure classifies as a typed UpstreamTimeout (not a raw error).
func TestResponsesGenerationTimeoutTyped(t *testing.T) {
	stub := slowResponsesStub(300 * time.Millisecond)
	defer stub.Close()

	p := newStubProvider(stub.URL, 50*time.Millisecond)
	_, err := p.Responses(context.Background(), provider.ResponsesRequest{Model: "m", Input: "hi"})
	if err == nil {
		t.Fatal("want error when generation outlasts the bound")
	}
	up := provider.ClassifyTransportError(err)
	var uerr *provider.UpstreamError
	if !errors.As(up, &uerr) || uerr.Type != "UpstreamTimeout" {
		t.Fatalf("classified = %v (%T), want UpstreamTimeout", up, up)
	}
}
