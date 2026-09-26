package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
)

// errField pulls the "error" value out of a response body and reports whether
// it is the typed object form (vs the legacy catch-all string).
func errField(t *testing.T, got map[string]any) (string, map[string]any) {
	t.Helper()
	e, ok := got["error"]
	if !ok {
		t.Fatalf("no error field in %v", got)
	}
	if s, ok := e.(string); ok {
		return s, nil
	}
	m, ok := e.(map[string]any)
	if !ok {
		t.Fatalf("error field type = %T, want string or object", e)
	}
	typ, _ := m["type"].(string)
	return typ, m
}

// Task 2.2: a transport/timeout failure must surface as a typed envelope, not
// the generic "chat upstream unavailable" catch-all. These use the
// responses-endpoint fallback path (resp-m → Responses), matching muse-spark.
func TestChatTimeoutSurfacesTyped(t *testing.T) {
	fake := dualFixture()
	fake.respErr = fmt.Errorf("responses fetch: %w", context.DeadlineExceeded)
	app := New(chatReg(t, fake), testLogger())

	code, got := postChat(t, app, map[string]any{
		"model":    "resp-m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, got %v", code, got)
	}
	typ, body := errField(t, got)
	if typ != "UpstreamTimeout" {
		t.Fatalf("error.type = %q, want UpstreamTimeout (body %v)", typ, got)
	}
	if body == nil {
		t.Fatal("want typed error object")
	}
}

func TestChatTransportSurfacesTyped(t *testing.T) {
	fake := dualFixture()
	fake.respErr = fmt.Errorf("responses fetch: %w", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED})
	app := New(chatReg(t, fake), testLogger())

	code, got := postChat(t, app, map[string]any{
		"model":    "resp-m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, got %v", code, got)
	}
	typ, _ := errField(t, got)
	if typ != "UpstreamTransport" {
		t.Fatalf("error.type = %q, want UpstreamTransport (body %v)", typ, got)
	}
}

// The direct chat path (chat-m → Chat) shares the same boundary.
func TestChatDirectTimeoutSurfacesTyped(t *testing.T) {
	fake := dualFixture()
	fake.chatErr = fmt.Errorf("chat fetch: %w", context.DeadlineExceeded)
	app := New(chatReg(t, fake), testLogger())

	code, got := postChat(t, app, map[string]any{
		"model":    "chat-m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, got %v", code, got)
	}
	typ, _ := errField(t, got)
	if typ != "UpstreamTimeout" {
		t.Fatalf("error.type = %q, want UpstreamTimeout (body %v)", typ, got)
	}
}

// An unclassifiable error still falls back to the catch-all (regression: the
// generic path is preserved for non-transport failures).
func TestChatUnknownErrorFallsToCatchAll(t *testing.T) {
	fake := dualFixture()
	fake.respErr = errors.New("boom")
	app := New(chatReg(t, fake), testLogger())

	code, got := postChat(t, app, map[string]any{
		"model":    "resp-m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, got %v", code, got)
	}
	s, _ := errField(t, got)
	if s != "chat upstream unavailable" {
		t.Fatalf("error = %q, want catch-all string", s)
	}
}

// The responses handler shares the same error boundary.
func TestResponsesTimeoutSurfacesTyped(t *testing.T) {
	fake := dualFixture()
	fake.respErr = fmt.Errorf("responses fetch: %w", context.DeadlineExceeded)
	app := New(chatReg(t, fake), testLogger())

	code, got := postResponses(t, app, map[string]any{
		"model": "resp-m",
		"input": "hi",
	})
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, got %v", code, got)
	}
	typ, _ := errField(t, got)
	if typ != "UpstreamTimeout" {
		t.Fatalf("error.type = %q, want UpstreamTimeout (body %v)", typ, got)
	}
}

// The typed message keeps the original cause text for diagnosis.
func TestChatTimeoutKeepsOriginalMessage(t *testing.T) {
	fake := dualFixture()
	fake.respErr = fmt.Errorf("read sse: %w", context.DeadlineExceeded)
	app := New(chatReg(t, fake), testLogger())

	_, got := postChat(t, app, map[string]any{
		"model":    "resp-m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	_, body := errField(t, got)
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "read sse") || !strings.Contains(msg, "deadline exceeded") {
		t.Fatalf("message = %q, want original cause preserved", msg)
	}
}

// Task 3.1 (server framing): a responses-endpoint model served via the chat
// buffer returns the full folded content with 200 — never the 502 catch-all —
// for a successful slow-ish generation. The timeout/transport failures are the
// typed envelopes covered above; success must not be mistaken for either.
func TestChatFallbackBufferedNot502Regression(t *testing.T) {
	fake := dualFixture()
	app := New(chatReg(t, fake), testLogger())

	code, got := postChat(t, app, map[string]any{
		"model":    "resp-m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (got %v)", code, got)
	}
	choices, _ := got["choices"].([]any)
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "from-responses" {
		t.Fatalf("content = %v, want from-responses via buffer", msg["content"])
	}
	if fake.responses != 1 {
		t.Fatalf("responses calls = %d, want 1", fake.responses)
	}
}
