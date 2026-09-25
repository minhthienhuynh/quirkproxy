package server

import (
	"bufio"
	"context"
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
}

type chatMessage struct {
	Role    string         `json:"role"`
	Content messageContent `json:"content"`
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
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	ToolCalls []replyToolCall `json:"tool_calls,omitempty"`
}

type replyToolCall struct {
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

// findChatProvider resolves the enabled chat provider serving modelID via
// the cached allowlist. With a non-empty filter the lookup scopes to that
// single provider: unknown, disabled, non-chat, or model outside its
// allowlist is a 404 without touching upstream. Without a filter, duplicate
// model ids resolve first-wins deterministically with a warning, mirroring
// modelsHandler. A missing model is a 404 UpstreamError; a ListModels
// failure is 503/502: the model may exist behind a dead upstream.
func findChatProvider(ctx context.Context, reg *provider.Registry, logger *slog.Logger, modelID, filter string) (provider.ChatCompleter, error) {
	if filter != "" {
		p, ok := reg.Lookup(filter)
		if !ok || !reg.Enabled(filter) || !hasCapability(p, provider.CapabilityChat) {
			return nil, &provider.UpstreamError{Status: fiber.StatusNotFound, Type: "ProviderNotFound", Message: "unknown or disabled provider " + filter}
		}
		if err := checkModel(ctx, p, modelID); err != nil {
			return nil, err
		}
		cc, ok := p.(provider.ChatCompleter)
		if !ok {
			return nil, &provider.UpstreamError{Status: fiber.StatusBadGateway, Type: "MisconfiguredProvider", Message: "provider advertises chat but does not implement it"}
		}
		return cc, nil
	}
	var cc provider.ChatCompleter
	keptID := ""
	for _, p := range reg.SortedEnabled() {
		if !hasCapability(p, provider.CapabilityChat) {
			continue
		}
		hit, err := modelAllowed(ctx, p, modelID)
		if err != nil {
			return nil, err
		}
		if !hit {
			continue
		}
		candidate, ok := p.(provider.ChatCompleter)
		if !ok {
			return nil, &provider.UpstreamError{Status: fiber.StatusBadGateway, Type: "MisconfiguredProvider", Message: "provider advertises chat but does not implement it"}
		}
		if cc != nil {
			logger.Warn("duplicate model id, keeping first",
				"model", modelID, "kept_provider", keptID, "skipped_provider", p.ID())
			continue
		}
		cc = candidate
		keptID = p.ID()
	}
	if cc == nil {
		return nil, &provider.UpstreamError{Status: fiber.StatusNotFound, Type: "ModelNotFound", Message: "unknown or non-free model " + modelID}
	}
	return cc, nil
}

// modelAllowed reports whether modelID is in the provider's cached allowlist.
func modelAllowed(ctx context.Context, p provider.ModelLister, modelID string) (bool, error) {
	res, err := p.ListModels(ctx)
	if err != nil {
		return false, catalogUnavailable(err)
	}
	for _, m := range res.Models {
		if m.ID == modelID {
			return true, nil
		}
	}
	return false, nil
}

// checkModel enforces allowlist membership for scoped routes.
func checkModel(ctx context.Context, p provider.ModelLister, modelID string) error {
	hit, err := modelAllowed(ctx, p, modelID)
	if err != nil {
		return err
	}
	if !hit {
		return &provider.UpstreamError{Status: fiber.StatusNotFound, Type: "ModelNotFound", Message: "model " + modelID + " not served by provider " + p.ID()}
	}
	return nil
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
		cc, err := findChatProvider(c.Context(), reg, logger, req.Model, c.Params("provider", ""))
		if err != nil {
			return chatUpstreamError(c, logger, req.Model, err)
		}
		chatReq := provider.ChatRequest{Model: req.Model, ToolChoice: req.ToolChoice, Temperature: req.Temperature, MaxTokens: req.MaxTokens}
		for _, m := range req.Messages {
			chatReq.Messages = append(chatReq.Messages, provider.ChatMessage{Role: m.Role, Content: m.Content.Text})
		}
		for _, t := range req.Tools {
			chatReq.Tools = append(chatReq.Tools, provider.ChatTool{Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters})
		}
		if req.Stream {
			return relayChatStream(c, logger, cc, chatReq, req.Model)
		}
		res, err := cc.Chat(c.Context(), chatReq)
		if err != nil {
			return chatUpstreamError(c, logger, req.Model, err)
		}
		reply := chatReply{Role: "assistant", Content: res.Content}
		finish := "stop"
		for _, tc := range res.ToolCalls {
			var rtc replyToolCall
			rtc.ID, rtc.Type = tc.ID, "function"
			rtc.Function.Name, rtc.Function.Arguments = tc.Name, tc.Arguments
			reply.ToolCalls = append(reply.ToolCalls, rtc)
		}
		if len(reply.ToolCalls) > 0 {
			finish = "tool_calls"
		}
		return c.JSON(chatResponse{
			ID: newResponseID(), Object: "chat.completion", Created: time.Now().Unix(),
			Model: req.Model, Choices: []chatChoice{{Index: 0, Message: reply, FinishReason: finish}},
		})
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
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
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
func chatUpstreamError(c fiber.Ctx, logger *slog.Logger, model string, err error) error {
	var uerr *provider.UpstreamError
	if errors.As(err, &uerr) {
		body := fiber.Map{"message": uerr.Message, "type": uerr.Type, "code": uerr.Status}
		if uerr.Hint != "" {
			body["hint"] = uerr.Hint
		}
		return c.Status(uerr.Status).JSON(fiber.Map{"error": body})
	}
	logger.Error("chat failed", "model", model, "error", err)
	return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "chat upstream unavailable"})
}
