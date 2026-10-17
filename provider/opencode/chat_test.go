package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minhthienhuynh/quirkproxy/config"
	"github.com/minhthienhuynh/quirkproxy/provider"
)

var (
	msgPat = regexp.MustCompile(`^msg_[0-9a-f]{12}[A-Za-z0-9]{14}$`)
	sesPat = regexp.MustCompile(`^ses_[0-9a-f]{12}[A-Za-z0-9]{14}$`)
)

func TestChatRecipeHeaders(t *testing.T) {
	var gotUA, gotClient, gotProject, gotReq, gotSes, gotAuth, gotCT string
	var gotSesID, gotAffinity, gotSesIDHeader string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotClient, gotProject = r.Header.Get("User-Agent"), r.Header.Get("x-opencode-client"), r.Header.Get("x-opencode-project")
		gotReq, gotSes = r.Header.Get("x-opencode-request"), r.Header.Get("x-opencode-session")
		gotSesID = r.Header.Get("x-opencode-session-id")
		gotAffinity = r.Header.Get("x-session-affinity")
		gotSesIDHeader = r.Header.Get("x-session-id")
		gotAuth, gotCT = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := &zenChatClient{client: srv.Client(), recipe: recipeFromOptions(map[string]any{
		"zen_api_key": "test-key",
	}, srv.URL)}
	res, err := c.chat(context.Background(), provider.ChatRequest{
		Model:    "mimo-v2.6-flash-free",
		Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
	}, "mimo-v2.6-flash-free")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_ = res

	if gotUA != defaultUserAgent {
		t.Fatalf("UA = %q, want pinned %q", gotUA, defaultUserAgent)
	}
	if gotClient != "cli" || gotProject != "global" {
		t.Fatalf("client/project = %q/%q", gotClient, gotProject)
	}
	if gotReq != "" {
		t.Fatalf("x-opencode-request = %q, want omitted in v2", gotReq)
	}
	if !sesPat.MatchString(gotSes) {
		t.Fatalf("id formats: ses=%q", gotSes)
	}
	if gotSesID != gotSes || gotAffinity != gotSes || gotSesIDHeader != gotSes {
		t.Fatalf("session headers mismatch: ses=%q sesID=%q affinity=%q sesIDHeader=%q", gotSes, gotSesID, gotAffinity, gotSesIDHeader)
	}
	if gotAuth != "Bearer test-key" || gotCT != "application/json" {
		t.Fatalf("auth/ct = %q/%q", gotAuth, gotCT)
	}
	if gotBody["stream"] != true {
		t.Fatalf("stream = %v, want true (gate requirement)", gotBody["stream"])
	}
	names := []string{}
	for _, tool := range gotBody["tools"].([]any) {
		names = append(names, tool.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	has := func(n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
	if !has("shell") || !has("read") || !has("glob") || !has("grep") {
		t.Fatalf("tools = %v, want shell+read+glob+grep ensured", names)
	}
	msgs := gotBody["messages"].([]any)
	if len(msgs) < 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v, want system prompt prepended", msgs)
	}
}

func TestChatSSEAggregation(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"content":"Hel"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"lo"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read","arguments":"{\"pa"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\"}"}}]}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	c := &zenChatClient{client: srv.Client(), recipe: recipeFromOptions(nil, srv.URL)}
	res, err := c.chat(context.Background(), provider.ChatRequest{Model: "m", Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}}}, "m")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if res.Content != "Hello" {
		t.Fatalf("content = %q, want Hello", res.Content)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "read" || res.ToolCalls[0].Arguments != "{\"path\"}" || res.ToolCalls[0].ID != "c1" {
		t.Fatalf("toolcalls = %+v", res.ToolCalls)
	}
}

func TestChatUpstreamErrors(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"FreeTierError","message":"nope"}}`))
	}))
	defer srv.Close()

	c := &zenChatClient{client: srv.Client(), recipe: recipeFromOptions(nil, srv.URL)}
	_, err := c.chat(context.Background(), provider.ChatRequest{Model: "m"}, "m")
	var uerr *provider.UpstreamError
	if !errors.As(err, &uerr) {
		t.Fatalf("err type = %T, want *UpstreamError", err)
	}
	if uerr.Status != 403 || uerr.Type != "FreeTierError" || uerr.Message != "nope" {
		t.Fatalf("uerr = %+v", uerr)
	}
	if uerr.Hint == "" {
		t.Fatal("403 must carry UA-pin hint")
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1 (no 4xx retry)", hits.Load())
	}
}

func TestRecipeDefaultsAndNoKeyLeak(t *testing.T) {
	r := recipeFromOptions(map[string]any{}, "https://example.invalid")
	if r.UserAgent != defaultUserAgent || r.Client != "cli" || r.Project != "global" || r.APIKey != "public" {
		t.Fatalf("defaults = %+v", r)
	}
	r = recipeFromOptions(map[string]any{
		"user_agent": "custom/1.0", "zen_api_key": "SECRET-KEY-123",
	}, "https://example.invalid")
	if r.UserAgent != "custom/1.0" || r.APIKey != "SECRET-KEY-123" {
		t.Fatalf("overrides = %+v", r)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"AuthError","message":"bad"}}`))
	}))
	defer srv.Close()
	c := &zenChatClient{client: srv.Client(), recipe: r}
	_, err := c.chat(context.Background(), provider.ChatRequest{Model: "m"}, "m")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), "SECRET-KEY-123") {
		t.Fatalf("error leaks key: %v", err)
	}
}

func TestChatRawConcurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := New(config.Provider{ID: "opencode", Type: "opencode", Enabled: true,
		Options: map[string]any{"zen_api_url": srv.URL}}, nil)

	const n = 50
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, err := p.ChatRaw(context.Background(), provider.ChatRequest{
				Model: "m", Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				errs <- err
				return
			}
			_, copyErr := io.Copy(io.Discard, body)
			body.Close()
			errs <- copyErr
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent chat: %v", err)
		}
	}
}

func TestUpstreamErrorCapturesRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		header map[string]string
		want   string
	}{
		{"retry-after", map[string]string{"Retry-After": "120"}, "120"},
		{"retry-after-ms", map[string]string{"retry-after-ms": "2500"}, "2500"},
		{"absent", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"type":"RateLimit","message":"slow"}}`))
			}))
			defer srv.Close()
			c := &zenChatClient{client: srv.Client(), recipe: recipeFromOptions(nil, srv.URL)}
			_, err := c.chat(context.Background(), provider.ChatRequest{Model: "m"}, "m")
			var uerr *provider.UpstreamError
			if !errors.As(err, &uerr) {
				t.Fatalf("err type = %T, want *UpstreamError", err)
			}
			if uerr.RetryAfter != tc.want {
				t.Fatalf("RetryAfter = %q, want %q", uerr.RetryAfter, tc.want)
			}
		})
	}
}

func TestChatRecipeGenuineAuthHeader(t *testing.T) {
	var gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	// Default: the genuine marker is always sent — never omitted, never keyless.
	c1 := &zenChatClient{client: srv.Client(), recipe: recipeFromOptions(nil, srv.URL)}
	if _, err := c1.chat(context.Background(), provider.ChatRequest{Model: "m"}, "m"); err != nil {
		t.Fatalf("chat default: %v", err)
	}
	if gotAuth != "Bearer public" {
		t.Fatalf("default Authorization = %q, want Bearer public", gotAuth)
	}
	if gotUA != "opencode/latest/2.0.32/cli" {
		t.Fatalf("default User-Agent = %q, want pinned opencode/latest/2.0.32/cli", gotUA)
	}

	// Env vars are not a marker source: the default stays the genuine marker.
	t.Setenv("OPENCODE_API_KEY", "env_token_456")
	t.Setenv("ZEN_API_KEY", "env_token_789")
	c2 := &zenChatClient{client: srv.Client(), recipe: recipeFromOptions(nil, srv.URL)}
	if _, err := c2.chat(context.Background(), provider.ChatRequest{Model: "m"}, "m"); err != nil {
		t.Fatalf("chat with env keys: %v", err)
	}
	if gotAuth != "Bearer public" {
		t.Fatalf("Authorization with env keys = %q, want Bearer public (env is not a marker source)", gotAuth)
	}

	// Explicit key: sent verbatim.
	c3 := &zenChatClient{client: srv.Client(), recipe: recipeFromOptions(map[string]any{
		"zen_api_key": "st_token_123",
	}, srv.URL)}
	if _, err := c3.chat(context.Background(), provider.ChatRequest{Model: "m"}, "m"); err != nil {
		t.Fatalf("chat with key: %v", err)
	}
	if gotAuth != "Bearer st_token_123" {
		t.Fatalf("Authorization with key = %q, want Bearer st_token_123", gotAuth)
	}
}
