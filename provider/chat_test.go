package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
)

// fakeTimeout is a net.Error whose Timeout() reports true (e.g. a socket
// read deadline), used to exercise the net-timeout branch.
type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "i/o timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

func TestClassifyTransportError(t *testing.T) {
	upstream := &UpstreamError{Status: 403, Type: "FreeTierError", Message: "gate"}

	cases := []struct {
		name     string
		err      error
		wantType string
		sameErr  bool
	}{
		{"nil stays nil", nil, "", true},
		{"already typed unchanged", upstream, "", true},
		{"plain deadline exceeded", context.DeadlineExceeded, "UpstreamTimeout", false},
		{"wrapped deadline exceeded", fmt.Errorf("read sse: %w", context.DeadlineExceeded), "UpstreamTimeout", false},
		{"client timeout deadline", fmt.Errorf("responses fetch: %w", context.DeadlineExceeded), "UpstreamTimeout", false},
		{"net timeout", fakeTimeout{}, "UpstreamTimeout", false},
		{"wrapped net timeout", fmt.Errorf("read: %w", fakeTimeout{}), "UpstreamTimeout", false},
		{"dial refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "UpstreamTransport", false},
		{"url transport error", &url.Error{Op: "Post", URL: "http://x", Err: errors.New("connection reset")}, "UpstreamTransport", false},
		{"syscall reset", fmt.Errorf("fetch: %w", syscall.ECONNRESET), "UpstreamTransport", false},
		{"os syscall error", &os.SyscallError{Syscall: "read", Err: syscall.EPIPE}, "UpstreamTransport", false},
		{"ordinary error unchanged", errors.New("encode responses: boom"), "", true},
		{"unknown error unchanged", fmt.Errorf("responses: terminal event not found"), "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyTransportError(c.err)
			if c.err == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if c.sameErr {
				if !errors.Is(got, c.err) && got != c.err {
					t.Fatalf("got %v, want unchanged %v", got, c.err)
				}
				return
			}
			var uerr *UpstreamError
			if !errors.As(got, &uerr) {
				t.Fatalf("got %T %v, want *UpstreamError", got, got)
			}
			if uerr.Type != c.wantType {
				t.Fatalf("type = %q, want %q", uerr.Type, c.wantType)
			}
			if uerr.Status != 502 {
				t.Fatalf("status = %d, want 502", uerr.Status)
			}
			// Message preserves the original error text.
			if uerr.Message == "" {
				t.Fatal("message should carry the original error text")
			}
		})
	}
}

// The classification must find a timeout/transport cause through several wrap
// layers, since providers wrap with context like "responses fetch: ...".
func TestClassifyTransportErrorThroughWraps(t *testing.T) {
	base := &url.Error{Op: "Post", URL: "http://x", Err: context.DeadlineExceeded}
	wrapped := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", base))
	got := ClassifyTransportError(wrapped)
	var uerr *UpstreamError
	if !errors.As(got, &uerr) || uerr.Type != "UpstreamTimeout" {
		t.Fatalf("got %v (%T), want UpstreamTimeout", got, got)
	}
}
