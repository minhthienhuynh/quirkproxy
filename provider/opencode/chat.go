package opencode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"

	"github.com/minhthienhuynh/quirkproxy/provider"
)

// Pinned genuine client recipe (verified live; see design spike findings).
// The UA triple must track the release the gate currently accepts.
const (
	defaultUserAgent = "opencode/1.18.30 ai-sdk/provider-utils/4.0.23 runtime/bun/1.4.2"
	defaultClient    = "cli"
	defaultProject   = "global"
	defaultAPIKey    = "public"
)

// Recipe carries the versioned genuine-client markers.
type Recipe struct {
	UserAgent string
	Client    string
	Project   string
	APIKey    string
	BaseURL   string
}

// recipeFromOptions builds the recipe from provider options with verified defaults.
func recipeFromOptions(opts map[string]any, baseURL string) Recipe {
	return Recipe{
		UserAgent: stringOption(opts, "user_agent", defaultUserAgent),
		Client:    stringOption(opts, "client", defaultClient),
		Project:   stringOption(opts, "project", defaultProject),
		APIKey:    stringOption(opts, "zen_api_key", defaultAPIKey),
		BaseURL:   baseURL,
	}
}

const (
	hexChars = "0123456789abcdef"
	b62Chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// newRequestID generates msg_<12hex><14base62>; newSessionID ses_....
func newRequestID() (string, error) {
	hex, err := randStr(hexChars, 12)
	if err != nil {
		return "", err
	}
	b62, err := randStr(b62Chars, 14)
	if err != nil {
		return "", err
	}
	return "msg_" + hex + b62, nil
}

func newSessionID() (string, error) {
	hex, err := randStr(hexChars, 12)
	if err != nil {
		return "", err
	}
	b62, err := randStr(b62Chars, 14)
	if err != nil {
		return "", err
	}
	return "ses_" + hex + b62, nil
}

func randStr(chars string, n int) (string, error) {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			return "", fmt.Errorf("crypto rand: %w", err)
		}
		sb.WriteByte(chars[j.Int64()])
	}
	return sb.String(), nil
}

// zenChatClient posts chat requests with the genuine recipe.
type zenChatClient struct {
	client *http.Client
	recipe Recipe
	logger *slog.Logger
}

// chatBody is the Zen wire shape (OpenAI-compatible subset).
type chatBody struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Tools       []wireTool    `json:"tools,omitempty"`
	ToolChoice  any           `json:"tool_choice,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
}

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters,omitempty"`
	} `json:"function"`
}

// skeletalTool returns the minimal tool entry the gate requires alongside
// any client tools (verified: names matter, schema content does not).
func skeletalTool(name string) wireTool {
	var t wireTool
	t.Type = "function"
	t.Function.Name = name
	t.Function.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
	return t
}

// ensureGateTools appends skeletal bash/read entries when absent.
func ensureGateTools(tools []wireTool) []wireTool {
	var bash, read bool
	for _, t := range tools {
		switch t.Function.Name {
		case "bash":
			bash = true
		case "read":
			read = true
		}
	}
	if !bash {
		tools = append(tools, skeletalTool("bash"))
	}
	if !read {
		tools = append(tools, skeletalTool("read"))
	}
	return tools
}

// sseChunk is the subset of OpenAI chat SSE we aggregate.
type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

// aggregateSSE folds an OpenAI chat event stream into content + tool calls.
// Malformed data lines are skipped and counted, never failing the stream.
func aggregateSSE(r io.Reader) (string, []provider.ToolCall, int, error) {
	var content strings.Builder
	calls := map[int]*provider.ToolCall{}
	order := []int{}
	bad := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ch sseChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			bad++
			continue
		}
		for _, c := range ch.Choices {
			content.WriteString(c.Delta.Content)
			for _, tc := range c.Delta.ToolCalls {
				call, ok := calls[tc.Index]
				if !ok {
					call = &provider.ToolCall{}
					calls[tc.Index] = call
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					call.ID = tc.ID
				}
				if tc.Function.Name != "" {
					call.Name = tc.Function.Name
				}
				call.Arguments += tc.Function.Arguments
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", nil, bad, fmt.Errorf("read sse: %w", err)
	}
	out := make([]provider.ToolCall, 0, len(order))
	for _, i := range order {
		out = append(out, *calls[i])
	}
	return content.String(), out, bad, nil
}

// buildBody maps a normalized request to the Zen wire shape.
func buildBody(req provider.ChatRequest, model string) chatBody {
	msgs := make([]wireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, wireMessage{Role: m.Role, Content: m.Content})
	}
	tools := make([]wireTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		var w wireTool
		w.Type = "function"
		w.Function.Name = t.Name
		w.Function.Description = t.Description
		w.Function.Parameters = t.Parameters
		tools = append(tools, w)
	}
	return chatBody{
		Model: model, Messages: msgs, Stream: true,
		Tools: ensureGateTools(tools), ToolChoice: req.ToolChoice,
		Temperature: req.Temperature, MaxTokens: req.MaxTokens,
	}
}

// postRaw sends the body with the genuine recipe and returns the raw SSE
// stream on 200. No retries: a 4xx is a decision, and every retry burns quota.
// postToPath sends the payload to the given path with the genuine recipe
// and returns the raw SSE stream on 200. Shared by chat and responses.
func (c *zenChatClient) postToPath(ctx context.Context, path string, payload []byte, kind string) (io.ReadCloser, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.recipe.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%s request: %w", kind, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.recipe.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", c.recipe.UserAgent)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-opencode-client", c.recipe.Client)
	httpReq.Header.Set("x-opencode-project", c.recipe.Project)
	reqID, err := newRequestID()
	if err != nil {
		return nil, err
	}
	sesID, err := newSessionID()
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-opencode-request", reqID)
	httpReq.Header.Set("x-opencode-session", sesID)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s fetch: %w", kind, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, parseUpstreamError(resp)
	}
	return resp.Body, nil
}

func (c *zenChatClient) postRaw(ctx context.Context, body chatBody) (io.ReadCloser, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode chat: %w", err)
	}
	return c.postToPath(ctx, "/chat/completions", payload, "chat")
}

// chat posts one request with stream forced on (gate requirement) and
// returns the aggregated result.
func (c *zenChatClient) chat(ctx context.Context, req provider.ChatRequest, model string) (provider.ChatResult, error) {
	body, err := c.postRaw(ctx, buildBody(req, model))
	if err != nil {
		return provider.ChatResult{}, err
	}
	defer body.Close()
	content, calls, bad, err := aggregateSSE(body)
	if err != nil {
		return provider.ChatResult{}, err
	}
	if bad > 0 {
		logger := c.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("sse aggregation skipped malformed chunks", "count", bad)
	}
	return provider.ChatResult{Content: content, ToolCalls: calls}, nil
}

// parseUpstreamError preserves status and message from a Zen error body.
func parseUpstreamError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Message != "" {
		msg = envelope.Error.Message
	}
	uerr := &provider.UpstreamError{Status: resp.StatusCode, Type: envelope.Error.Type, Message: msg}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		uerr.RetryAfter = ra
	} else if ms := resp.Header.Get("retry-after-ms"); ms != "" {
		uerr.RetryAfter = ms
	}
	if resp.StatusCode == 403 {
		uerr.Hint = "check pinned User-Agent recipe"
	}
	return uerr
}
