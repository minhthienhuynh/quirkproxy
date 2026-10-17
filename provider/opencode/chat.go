package opencode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/minhthienhuynh/quirkproxy/provider"
)

// Pinned genuine client recipe (verified live; see design spike findings).
// The UA triple must track the release the gate currently accepts.
const (
	defaultUserAgent = "opencode/latest/2.0.32/cli"
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
	apiKey := stringOption(opts, "zen_api_key", "")
	if apiKey == "" {
		if envKey := os.Getenv("OPENCODE_API_KEY"); envKey != "" {
			apiKey = envKey
		} else if envKey := os.Getenv("ZEN_API_KEY"); envKey != "" {
			apiKey = envKey
		} else {
			apiKey = defaultAPIKey
		}
	}
	return Recipe{
		UserAgent: stringOption(opts, "user_agent", defaultUserAgent),
		Client:    stringOption(opts, "client", defaultClient),
		Project:   stringOption(opts, "project", defaultProject),
		APIKey:    apiKey,
		BaseURL:   strings.TrimRight(baseURL, "/"),
	}
}

const (
	hexChars = "0123456789abcdef"
	b62Chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

var (
	idCounter uint32
	idLastMs  int64
	idMu      sync.Mutex
)

// newSessionID generates ses_<12hex><14base62> using OpenCode v2's descending timestamp format.
func newSessionID() (string, error) {
	return newSessionIDAt(time.Now().UnixMilli())
}

// newSessionIDAt is newSessionID with an injectable clock. It mirrors the
// genuine client's SMn: the counter counts mints within one millisecond and
// resets when the timestamp advances, keeping the packed counter inside the
// gate's accepted range (< 64) for the whole process lifetime instead of
// climbing without bound.
func newSessionIDAt(ms int64) (string, error) {
	idMu.Lock()
	if ms != idLastMs {
		idLastMs = ms
		idCounter = 0
	}
	idCounter++
	cnt := idCounter
	idMu.Unlock()

	current := uint64(ms)*4096 + uint64(cnt%4096)
	val := ^current

	timeBytes := make([]byte, 6)
	for i := 0; i < 6; i++ {
		timeBytes[i] = byte((val >> (40 - 8*uint(i))) & 0xff)
	}
	timeHex := hex.EncodeToString(timeBytes)

	b62 := make([]byte, 14)
	randBytes := make([]byte, 14)
	if _, err := rand.Read(randBytes); err != nil {
		return "", fmt.Errorf("crypto rand: %w", err)
	}
	for i, b := range randBytes {
		b62[i] = b62Chars[int(b)%len(b62Chars)]
	}
	return "ses_" + timeHex + string(b62), nil
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
	// genTimeout bounds one inference call (headers + body/stream) as a
	// safety net; the body lifetime is otherwise bounded by the caller's
	// context (client disconnect). Zero means no bound (tests only).
	genTimeout time.Duration
	sessions   *SessionManager
}

func (c *zenChatClient) resolveSession(hint, identity string) string {
	if c.sessions != nil {
		return c.sessions.Resolve(hint, identity)
	}
	if hint != "" {
		return translateSessionID(hint)
	}
	sesID, err := newSessionID()
	if err != nil {
		return translateSessionID(identity)
	}
	return sesID
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

// ensureGateTools appends genuine v2 tool entries when absent.
func ensureGateTools(tools []wireTool) []wireTool {
	existing := make(map[string]bool, len(tools))
	for _, t := range tools {
		existing[t.Function.Name] = true
	}
	for _, reqTool := range v2GateWireTools() {
		if !existing[reqTool.Function.Name] {
			tools = append(tools, reqTool)
		}
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
	var hasSystem bool
	for _, m := range req.Messages {
		if m.Role == "system" {
			hasSystem = true
			break
		}
	}

	msgs := make([]wireMessage, 0, len(req.Messages)+1)
	if !hasSystem {
		msgs = append(msgs, wireMessage{Role: "system", Content: defaultHarnessPrompt})
	}
	for _, m := range req.Messages {
		if m.Role == "system" {
			if !strings.Contains(m.Content, "OpenCode") {
				msgs = append(msgs, wireMessage{
					Role:    "system",
					Content: defaultHarnessPrompt + "\n\n# User Instructions\n" + m.Content,
				})
			} else {
				msgs = append(msgs, wireMessage{Role: m.Role, Content: m.Content})
			}
		} else {
			msgs = append(msgs, wireMessage{Role: m.Role, Content: m.Content})
		}
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
// The call is bounded by genTimeout (headers + body) as a safety net on top
// of the caller's context; the bound is cancelled when the body is closed.
func (c *zenChatClient) postToPath(ctx context.Context, path string, payload []byte, kind string, hints ...string) (io.ReadCloser, error) {
	ctx, cancel := c.boundContext(ctx)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.recipe.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s request: %w", kind, err)
	}
	if c.recipe.APIKey != "" && c.recipe.APIKey != "public" && c.recipe.APIKey != "none" {
		httpReq.Header.Set("Authorization", "Bearer "+c.recipe.APIKey)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", c.recipe.UserAgent)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-opencode-client", c.recipe.Client)
	httpReq.Header.Set("x-opencode-project", c.recipe.Project)

	var hint, identity string
	if len(hints) > 0 {
		hint = hints[0]
	}
	if len(hints) > 1 {
		identity = hints[1]
	}
	sesID := c.resolveSession(hint, identity)
	httpReq.Header.Set("x-opencode-session", sesID)
	httpReq.Header.Set("x-opencode-session-id", sesID)
	httpReq.Header.Set("x-session-affinity", sesID)
	httpReq.Header.Set("x-session-id", sesID)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s fetch: %w", kind, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		// Read the error body before cancelling: cancelling the request
		// context first could abort a still-in-flight body read and drop
		// the upstream error message.
		uerr := parseUpstreamError(resp)
		cancel()
		return nil, uerr
	}
	return &cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}, nil
}

// boundContext applies the generation safety timeout when set, returning a
// cancel that must be called to release the timer (wired to body Close).
func (c *zenChatClient) boundContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.genTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.genTimeout)
}

// cancelReadCloser ties the bound-context cancel to the body's Close so the
// safety-net timer does not outlive a successful short call.
type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

// Close closes the body and releases the bound-context timer.
func (r *cancelReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.cancel()
	return err
}

func (c *zenChatClient) postRaw(ctx context.Context, body chatBody, hints ...string) (io.ReadCloser, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode chat: %w", err)
	}
	return c.postToPath(ctx, "/chat/completions", payload, "chat", hints...)
}

// chat posts one request with stream forced on (gate requirement) and
// returns the aggregated result.
func (c *zenChatClient) chat(ctx context.Context, req provider.ChatRequest, model string) (provider.ChatResult, error) {
	body, err := c.postRaw(ctx, buildBody(req, model), req.SessionID, req.ClientIdentity)
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
