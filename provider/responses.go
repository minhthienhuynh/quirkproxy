package provider

import (
	"context"
	"io"
)

// ResponsesRequest is the normalized input for one Responses API call.
// Input/Tools/Reasoning pass through verbatim (strings, items, or objects).
type ResponsesRequest struct {
	Model           string
	Input           any
	Tools           []ResponsesTool
	Reasoning       any
	MaxOutputTokens *int
	SessionID       string
	ClientIdentity  string
}

// ResponsesTool is one function tool in provider-neutral form.
type ResponsesTool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ResponsesResult is the full aggregated Responses API response.
// Payload is the completed response object with the envelope rewritten.
type ResponsesResult struct {
	Payload []byte
}

// ResponsesCompleter is implemented by providers that serve the Responses API.
type ResponsesCompleter interface {
	// Responses sends one request and returns the full aggregated result.
	Responses(ctx context.Context, req ResponsesRequest) (ResponsesResult, error)
	// ResponsesRaw sends one request and returns the raw upstream SSE
	// stream. The caller must close the body. Used to relay streams.
	ResponsesRaw(ctx context.Context, req ResponsesRequest) (io.ReadCloser, error)
}
