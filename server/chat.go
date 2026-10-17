package server

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/minhthienhuynh/quirkproxy/provider"
)

// chatRequest is the OpenAI chat completions subset this change supports.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []chatTool    `json:"tools"`
	ToolChoice  any           `json:"tool_choice"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature"`
	MaxTokens   *int          `json:"max_tokens"`
	User        string        `json:"user,omitempty"`
}

type chatMessage struct {
	Role    string         `json:"role"`
	Content messageContent `json:"content"`
	// ToolCalls are the calls an assistant turn requested; ToolCallID links
	// a role:"tool" result to the call it answers. Parsed so the fallback can
	// preserve the multi-turn tool loop.
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// messageContent accepts a plain string or an array of content parts
// (opencode sends [{type:"text",text:...}] even for text-only prompts).
type messageContent struct {
	Text string
}

// UnmarshalJSON implements json.Unmarshaler.
func (m *messageContent) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		m.Text = s
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &parts); err != nil {
		return err
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		}
	}
	m.Text = strings.Join(texts, "\n")
	return nil
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type chatChoice struct {
	Index        int       `json:"index"`
	Message      chatReply `json:"message"`
	FinishReason string    `json:"finish_reason"`
}

type chatReply struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

// toolCall is the OpenAI function tool-call wire shape, shared by request
// history (assistant turns) and replies.
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
}

func newResponseID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("chatcmpl-quirk-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

// catalogUnavailable maps allowlist fetch failures to 503/502.
func catalogUnavailable(err error) error {
	if errors.Is(err, provider.ErrNoCache) || errors.Is(err, provider.ErrUpstreamUnavailable) {
		return &provider.UpstreamError{Status: fiber.StatusServiceUnavailable, Type: "UpstreamUnavailable", Message: "model catalog unavailable: upstream unreachable and no usable cache"}
	}
	return &provider.UpstreamError{Status: fiber.StatusBadGateway, Type: "UpstreamError", Message: "model catalog unavailable"}
}

func hasCapability(p provider.ModelLister, want provider.Capability) bool {
	for _, c := range p.Capabilities() {
		if c == want {
			return true
		}
	}
	return false
}

// serveChatDirect serves a chat-endpoint model straight through the chat
// backend, relaying streams envelope-rewritten.
func serveChatDirect(c fiber.Ctx, logger *slog.Logger, p provider.ModelLister, filter string, req chatRequest, chatReq provider.ChatRequest) error {
	cc, err := completerOf(p, filter, provider.CapabilityChat, "chat",
		func(p provider.ModelLister) (provider.ChatCompleter, bool) {
			cc, ok := p.(provider.ChatCompleter)
			return cc, ok
		})
	if err != nil {
		return chatUpstreamError(c, logger, req.Model, err)
	}
	if req.Stream {
		return relayChatStream(c, logger, cc, chatReq, req.Model)
	}
	res, err := cc.Chat(c.Context(), chatReq)
	if err != nil {
		return chatUpstreamError(c, logger, req.Model, err)
	}
	return writeChatResponse(c, req.Model, res)
}

// serveChatFallback serves a responses-endpoint model through the chat API:
// it calls the Responses backend (buffered non-stream) and translates the
// result back to the chat shape.
func serveChatFallback(c fiber.Ctx, logger *slog.Logger, p provider.ModelLister, filter string, req chatRequest, chatReq provider.ChatRequest) error {
	rc, err := completerOf(p, filter, provider.CapabilityResponses, "responses",
		func(p provider.ModelLister) (provider.ResponsesCompleter, bool) {
			rc, ok := p.(provider.ResponsesCompleter)
			return rc, ok
		})
	if err != nil {
		return chatUpstreamError(c, logger, req.Model, err)
	}
	logger.Info("protocol fallback", "model", req.Model,
		"from_endpoint", "chat/completions", "to_endpoint", "responses", "buffered", req.Stream)
	if dropped := droppedChatFields(req); len(dropped) > 0 {
		logger.Warn("protocol fallback dropping unsupported fields", "model", req.Model, "dropped_fields", strings.Join(dropped, ","))
	}
	respReq := chatToResponses(chatReq)
	res, err := rc.Responses(c.Context(), respReq)
	if err != nil {
		return chatUpstreamError(c, logger, req.Model, err)
	}
	chatRes, err := responsesPayloadToChatResult(res.Payload)
	if err != nil {
		var uerr *provider.UpstreamError
		if errors.As(err, &uerr) {
			return chatUpstreamError(c, logger, req.Model, uerr)
		}
		return chatUpstreamError(c, logger, req.Model, &provider.UpstreamError{
			Status: fiber.StatusBadGateway, Type: "TranslateError",
			Message: "fallback translate failed: " + err.Error(),
		})
	}
	if req.Stream {
		setSSEHeaders(c)
		return c.SendString(synthesizeChatSSE(chatRes, req.Model, newResponseID(), time.Now().Unix()))
	}
	return writeChatResponse(c, req.Model, chatRes)
}

// droppedChatFields lists chat request fields with values that the Responses
// wire cannot carry and the fallback therefore drops.
func droppedChatFields(req chatRequest) []string {
	var out []string
	if req.Temperature != nil {
		out = append(out, "temperature")
	}
	if req.ToolChoice != nil {
		out = append(out, "tool_choice")
	}
	return out
}

// chatFinishReason derives the chat finish_reason: a transport-derived
// override (e.g. "length" from a truncated responses payload) wins over
// the derived stop/tool_calls default.
func chatFinishReason(res provider.ChatResult) string {
	if res.FinishReason != "" {
		return res.FinishReason
	}
	if len(res.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

// writeChatResponse renders a chat.completion JSON response.
func writeChatResponse(c fiber.Ctx, model string, res provider.ChatResult) error {
	reply := chatReply{Role: "assistant", Content: res.Content}
	for _, tc := range res.ToolCalls {
		var rtc toolCall
		rtc.ID, rtc.Type = tc.ID, "function"
		rtc.Function.Name, rtc.Function.Arguments = tc.Name, tc.Arguments
		reply.ToolCalls = append(reply.ToolCalls, rtc)
	}
	finish := chatFinishReason(res)
	return c.JSON(chatResponse{
		ID: newResponseID(), Object: "chat.completion", Created: time.Now().Unix(),
		Model: model, Choices: []chatChoice{{Index: 0, Message: reply, FinishReason: finish}},
	})
}

// chatHandler serves POST /v1/chat/completions and
// POST /:provider/v1/chat/completions (non-stream and SSE stream).
func chatHandler(reg *provider.Registry, logger *slog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req chatRequest
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid chat request: " + err.Error()})
		}
		if req.Model == "" || len(req.Messages) == 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "model and messages are required"})
		}
		chatReq := provider.ChatRequest{
			Model:          req.Model,
			ToolChoice:     req.ToolChoice,
			Temperature:    req.Temperature,
			MaxTokens:      req.MaxTokens,
			SessionID:      extractSessionHint(c, req.User),
			ClientIdentity: extractClientIdentity(c),
		}
		for _, m := range req.Messages {
			cm := provider.ChatMessage{Role: m.Role, Content: m.Content.Text, ToolCallID: m.ToolCallID}
			for _, tc := range m.ToolCalls {
				cm.ToolCalls = append(cm.ToolCalls, provider.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
			chatReq.Messages = append(chatReq.Messages, cm)
		}
		for _, t := range req.Tools {
			chatReq.Tools = append(chatReq.Tools, provider.ChatTool{Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters})
		}
		filter := c.Params("provider", "")
		p, m, err := resolveModel(c.Context(), reg, logger, req.Model, filter)
		if err != nil {
			return chatUpstreamError(c, logger, req.Model, err)
		}
		endpoint := m.Endpoint
		if endpoint == "" {
			endpoint = "chat/completions"
		}
		switch endpoint {
		case "chat/completions":
			return serveChatDirect(c, logger, p, filter, req, chatReq)
		case "responses":
			return serveChatFallback(c, logger, p, filter, req, chatReq)
		default:
			return chatUpstreamError(c, logger, req.Model, &provider.UpstreamError{
				Status: fiber.StatusBadGateway, Type: "MisconfiguredProvider",
				Message: "model " + req.Model + " serves unknown endpoint " + m.Endpoint,
			})
		}
	}
}

// relayChatStream relays the upstream SSE stream, rewriting only the
// envelope (id/model/created) while passing choices through byte-identical.
// An upstream [DONE] is swallowed; exactly one proxy-emitted [DONE] ends
// the stream. A scanner failure closes the stream without a fake [DONE].
func relayChatStream(c fiber.Ctx, logger *slog.Logger, cc provider.ChatCompleter, chatReq provider.ChatRequest, model string) error {
	raw, err := cc.ChatRaw(c.Context(), chatReq)
	if err != nil {
		return chatUpstreamError(c, logger, model, err)
	}
	// raw is closed inside the stream writer: the handler returns before
	// the stream finishes, so a defer here would kill the stream early.
	setSSEHeaders(c)
	id, created := newResponseID(), time.Now().Unix()
	c.Response().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer raw.Close()
		bad := 0
		sc := bufio.NewScanner(raw)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			payload, ok := cutDataLine(line)
			if !ok {
				fmt.Fprintln(w, line)
				_ = w.Flush()
				continue
			}
			if payload == "[DONE]" {
				continue
			}
			var chunk sseEnvelope
			if json.Unmarshal([]byte(payload), &chunk) != nil {
				bad++
				continue
			}
			chunk.ID, chunk.Created, chunk.Model = id, created, model
			if rewritten, err := json.Marshal(chunk); err == nil {
				line = "data: " + string(rewritten)
			}
			fmt.Fprintln(w, line)
			_ = w.Flush()
		}
		if err := sc.Err(); err != nil {
			logger.Error("sse relay aborted", "model", model, "error", err)
			_ = w.Flush()
			return
		}
		if bad > 0 {
			logger.Warn("sse relay skipped malformed chunks", "model", model, "count", bad)
		}
		fmt.Fprintln(w, "data: [DONE]")
		_ = w.Flush()
	})
	return nil
}

// sseEnvelope rewrites only the envelope; choices stay byte-identical.
type sseEnvelope struct {
	ID      string            `json:"id"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Object  string            `json:"object"`
	Choices []json.RawMessage `json:"choices"`
}

func cutDataLine(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")), true
}

// chatUpstreamError passes the upstream status and message through verbatim,
// keeping any hint in a separate field so the original message is intact.
// A carried retry-after value is forwarded as a Retry-After response header
// so downstream clients back off on the server's schedule. Transport/timeout
// failures are upgraded to a typed envelope (UpstreamTimeout/UpstreamTransport)
// instead of the generic catch-all, which is reserved for unclassifiable errors.
func chatUpstreamError(c fiber.Ctx, logger *slog.Logger, model string, err error) error {
	if uerr := asUpstreamError(err); uerr != nil {
		body := fiber.Map{"message": uerr.Message, "type": uerr.Type, "code": uerr.Status}
		if uerr.Hint != "" {
			body["hint"] = uerr.Hint
		}
		if uerr.RetryAfter != "" {
			c.Set("Retry-After", uerr.RetryAfter)
		}
		return c.Status(uerr.Status).JSON(fiber.Map{"error": body})
	}
	logger.Error("chat failed", "model", model, "error", err)
	return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "chat upstream unavailable"})
}

// asUpstreamError resolves err to a typed UpstreamError: one already typed, or
// a transport/timeout failure classified by provider.ClassifyTransportError.
// Returns nil when err cannot be classified, leaving the generic fallback.
func asUpstreamError(err error) *provider.UpstreamError {
	if err == nil {
		return nil
	}
	var uerr *provider.UpstreamError
	if errors.As(err, &uerr) {
		return uerr
	}
	if errors.As(provider.ClassifyTransportError(err), &uerr) {
		return uerr
	}
	return nil
}
