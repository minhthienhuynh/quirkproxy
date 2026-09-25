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
}

// UpstreamError preserves the upstream API rejection status and message.
type UpstreamError struct {
	Status  int
	Type    string
	Message string
	Hint    string
}

// Error implements error.
func (e *UpstreamError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("upstream: status %d: %s (%s)", e.Status, e.Message, e.Hint)
	}
	return fmt.Sprintf("upstream: status %d: %s", e.Status, e.Message)
}
