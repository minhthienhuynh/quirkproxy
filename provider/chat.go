package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
)

// ChatMessage is one OpenAI-style chat message. ToolCalls carries the calls
// an assistant turn requested (request history) and ToolCallID links a
// role:"tool" result to the call it answers, so a protocol fallback can
// preserve the multi-turn tool loop instead of flattening it.
type ChatMessage struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
}

// ChatTool is one function tool in provider-neutral form.
type ChatTool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ChatRequest is the normalized input for one chat inference call.
type ChatRequest struct {
	Model          string
	Messages       []ChatMessage
	Tools          []ChatTool
	ToolChoice     any
	Temperature    *float64
	MaxTokens      *int
	SessionID      string
	ClientIdentity string
}

// ToolCall is one assistant-requested tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ChatResult is the full aggregated inference result.
type ChatResult struct {
	Content   string
	ToolCalls []ToolCall
	// FinishReason, when set, overrides the handler-derived finish_reason
	// ("stop"/"tool_calls") with a transport-derived value: "length" when
	// the result was cut by the output-token limit, "content_filter" when
	// it was cut by a content filter.
	FinishReason string
}

// UpstreamError preserves the upstream API rejection status and message.
type UpstreamError struct {
	Status  int
	Type    string
	Message string
	Hint    string
	// RetryAfter carries an upstream retry-after (or retry-after-ms) value
	// so handlers can forward it as a Retry-After response header.
	RetryAfter string
}

// Error implements error.
func (e *UpstreamError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("upstream: status %d: %s (%s)", e.Status, e.Message, e.Hint)
	}
	return fmt.Sprintf("upstream: status %d: %s", e.Status, e.Message)
}

// ClassifyTransportError inspects err (even deeply wrapped) and, when it is a
// transport or timeout failure rather than an upstream API rejection, wraps it
// in a typed UpstreamError so handlers can surface a clear cause instead of a
// generic catch-all. Timeout covers context deadlines and net timeouts;
// transport covers dial/reset/connection failures. Anything else — including an
// error already typed as UpstreamError — passes through unchanged.
func ClassifyTransportError(err error) error {
	if err == nil {
		return nil
	}
	var uerr *UpstreamError
	if errors.As(err, &uerr) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return &UpstreamError{Status: http.StatusBadGateway, Type: "UpstreamTimeout", Message: err.Error()}
	}
	if isTransport(err) {
		return &UpstreamError{Status: http.StatusBadGateway, Type: "UpstreamTransport", Message: err.Error()}
	}
	return err
}

// isTimeout reports whether err unwraps to a network timeout (a context
// deadline is handled separately before this is consulted).
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// isTransport reports whether err is a network/connection-level failure (dial
// refused, reset, broken pipe, syscalls) as opposed to an application error.
func isTransport(err error) bool {
	var oe *net.OpError
	if errors.As(err, &oe) {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return true
	}
	var se *os.SyscallError
	if errors.As(err, &se) {
		return true
	}
	return errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNABORTED)
}
