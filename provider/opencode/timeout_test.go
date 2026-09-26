package opencode

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Task 1.1: the inference client must carry no total Client.Timeout (the bug
// vector that cut long generations) while still bounding time-to-headers; the
// catalog client keeps its short total timeout.
func TestInferenceAndCatalogClientBounds(t *testing.T) {
	inf := inferenceClient(defaultInferenceHeaderTimeout)
	if inf.Timeout != 0 {
		t.Fatalf("inference client Timeout = %v, want 0 (no total cutoff)", inf.Timeout)
	}
	tr, ok := inf.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("inference transport type = %T, want *http.Transport", inf.Transport)
	}
	if tr.ResponseHeaderTimeout != defaultInferenceHeaderTimeout {
		t.Fatalf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, defaultInferenceHeaderTimeout)
	}

	cat := httpClient()
	if cat.Timeout != 15*time.Second {
		t.Fatalf("catalog client Timeout = %v, want 15s", cat.Timeout)
	}
}

// Task 1.2: duration options parse duration strings and fall back on absent,
// empty, malformed, zero, negative, or non-string values.
func TestDurationOption(t *testing.T) {
	def := 10 * time.Minute
	cases := []struct {
		opts map[string]any
		want time.Duration
	}{
		{nil, def},
		{map[string]any{}, def},
		{map[string]any{"k": ""}, def},
		{map[string]any{"k": "   "}, def},
		{map[string]any{"k": "0s"}, def},
		{map[string]any{"k": "-5s"}, def},
		{map[string]any{"k": "bogus"}, def},
		{map[string]any{"k": 5}, def},
		{map[string]any{"k": "30s"}, 30 * time.Second},
		{map[string]any{"k": "2m"}, 2 * time.Minute},
		{map[string]any{"k": "1h30m"}, 90 * time.Minute},
	}
	for _, c := range cases {
		if got := durationOption(c.opts, "k", def); got != c.want {
			t.Fatalf("durationOption(%v) = %v, want %v", c.opts, got, c.want)
		}
	}
}

// Task 1.3: the call is bounded by genTimeout as a safety net. A body that
// stalls beyond genTimeout fails (mirrors the original mid-stream cut); a body
// that completes within genTimeout succeeds even though the old fixed total
// would not have been the bound. Timescales are compressed; the absence of the
// old 15s total cutoff is asserted on the client in
// TestInferenceAndCatalogClientBounds.
func TestPostToPathGenerationBound(t *testing.T) {
	t.Run("body read bounded by genTimeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
		}))
		defer srv.Close()

		c := &zenChatClient{
			client:     inferenceClient(2 * time.Second),
			genTimeout: 50 * time.Millisecond,
			recipe:     recipeFromOptions(nil, srv.URL),
		}
		raw, err := c.postToPath(context.Background(), "/responses", []byte(`{}`), "responses")
		if err != nil {
			t.Fatalf("postToPath: %v", err)
		}
		defer raw.Close()
		_, rerr := io.ReadAll(raw)
		if rerr == nil {
			t.Fatal("want body read to fail once genTimeout elapses")
		}
		if !errors.Is(rerr, context.DeadlineExceeded) {
			t.Fatalf("body read err = %v, want context deadline exceeded", rerr)
		}
	})

	t.Run("completes within genTimeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond)
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
		}))
		defer srv.Close()

		c := &zenChatClient{
			client:     inferenceClient(2 * time.Second),
			genTimeout: 2 * time.Second,
			recipe:     recipeFromOptions(nil, srv.URL),
		}
		raw, err := c.postToPath(context.Background(), "/responses", []byte(`{}`), "responses")
		if err != nil {
			t.Fatalf("postToPath: %v", err)
		}
		defer raw.Close()
		if _, err := io.ReadAll(raw); err != nil {
			t.Fatalf("body read: %v", err)
		}
	})

	t.Run("client disconnect cancels call", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(500 * time.Millisecond)
			_, _ = w.Write([]byte("data: {}\n\n"))
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		c := &zenChatClient{
			client:     inferenceClient(2 * time.Second),
			genTimeout: 5 * time.Second,
			recipe:     recipeFromOptions(nil, srv.URL),
		}
		raw, err := c.postToPath(ctx, "/responses", []byte(`{}`), "responses")
		if err != nil {
			t.Fatalf("postToPath: %v", err)
		}
		defer raw.Close()
		cancel()
		if _, rerr := io.ReadAll(raw); rerr == nil {
			t.Fatal("want body read to fail after client disconnect")
		}
	})
}
