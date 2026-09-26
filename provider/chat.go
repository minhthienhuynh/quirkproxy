package provider

import (
	"fmt"
)

// ChatMessage is one OpenAI-style chat message.
type ChatMessage struct {
	Role    string
	Content string
}

// ChatTool is one function tool in provider-neutral form.
type ChatTool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ChatRequest is the normalized input for one chat inference call.
type ChatRequest struct {
	Model       string
	Messages    []ChatMessage
	Tools       []ChatTool
	ToolChoice  any
	Temperature *float64
	MaxTokens   *int
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
