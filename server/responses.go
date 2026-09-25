package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/minhthienhuynh/quirkproxy/provider"
)

// responsesRequest is the OpenAI Responses API subset this change supports.
// Input passes through verbatim (string or items array).
type responsesRequest struct {
	Model           string              `json:"model"`
	Input           any                 `json:"input"`
	Tools           []responsesWireTool `json:"tools"`
	Reasoning       any                 `json:"reasoning"`
	Stream          bool                `json:"stream"`
	MaxOutputTokens *int                `json:"max_output_tokens"`
}

type responsesWireTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// findResponsesProvider resolves the enabled Responses provider serving
// modelID via the cached allowlist. Unknown, disabled, non-responses, or
// model outside allowlist is 404 without touching upstream.
func findResponsesProvider(ctx context.Context, reg *provider.Registry, logger *slog.Logger, modelID, filter string) (provider.ResponsesCompleter, error) {
	if filter != "" {
		p, ok := reg.Lookup(filter)
		if !ok || !reg.Enabled(filter) || !hasCapability(p, provider.CapabilityResponses) {
			return nil, &provider.UpstreamError{Status: fiber.StatusNotFound, Type: "ProviderNotFound", Message: "unknown or disabled provider " + filter}
		}
		if err := checkModel(ctx, p, modelID); err != nil {
			return nil, err
		}
		rc, ok := p.(provider.ResponsesCompleter)
		if !ok {
			return nil, &provider.UpstreamError{Status: fiber.StatusBadGateway, Type: "MisconfiguredProvider", Message: "provider advertises responses but does not implement it"}
		}
		return rc, nil
	}
	var rc provider.ResponsesCompleter
	keptID := ""
	for _, p := range reg.SortedEnabled() {
		if !hasCapability(p, provider.CapabilityResponses) {
			continue
		}
		hit, err := modelAllowed(ctx, p, modelID)
		if err != nil {
			return nil, err
		}
		if !hit {
			continue
		}
		candidate, ok := p.(provider.ResponsesCompleter)
		if !ok {
			return nil, &provider.UpstreamError{Status: fiber.StatusBadGateway, Type: "MisconfiguredProvider", Message: "provider advertises responses but does not implement it"}
		}
		if rc != nil {
			logger.Warn("duplicate model id, keeping first",
				"model", modelID, "kept_provider", keptID, "skipped_provider", p.ID())
			continue
		}
		rc = candidate
		keptID = p.ID()
	}
	if rc == nil {
		return nil, &provider.UpstreamError{Status: fiber.StatusNotFound, Type: "ModelNotFound", Message: "unknown or non-free model " + modelID}
	}
	return rc, nil
}

// responsesHandler serves POST /v1/responses and
// POST /:provider/v1/responses (non-stream and SSE stream).
func responsesHandler(reg *provider.Registry, logger *slog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req responsesRequest
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid responses request: " + err.Error()})
		}
		if req.Model == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "model is required"})
		}
		if req.Input == nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "input is required"})
		}
		rc, err := findResponsesProvider(c.Context(), reg, logger, req.Model, c.Params("provider", ""))
		if err != nil {
			return chatUpstreamError(c, logger, req.Model, err)
		}
		respReq := provider.ResponsesRequest{
			Model:           req.Model,
			Input:           req.Input,
			Reasoning:       req.Reasoning,
			MaxOutputTokens: req.MaxOutputTokens,
		}
		for _, t := range req.Tools {
			respReq.Tools = append(respReq.Tools, provider.ResponsesTool{
				Name: t.Name, Description: t.Description, Parameters: t.Parameters,
			})
		}
		if req.Stream {
			return relayResponsesStream(c, logger, rc, respReq, req.Model)
		}
		res, err := rc.Responses(c.Context(), respReq)
		if err != nil {
			return chatUpstreamError(c, logger, req.Model, err)
		}
		c.Set("Content-Type", "application/json")
		return c.Send(res.Payload)
	}
}

// relayResponsesStream relays the upstream SSE stream, preserving event:
// lines and rewriting only the envelope (id/model/created_at) inside data
// JSON response objects. The upstream response.completed is swallowed and
// exactly one proxy-emitted response.completed ends the stream. A scanner
// failure closes the stream without a fake completed.
func relayResponsesStream(c fiber.Ctx, logger *slog.Logger, rc provider.ResponsesCompleter, req provider.ResponsesRequest, model string) error {
	raw, err := rc.ResponsesRaw(c.Context(), req)
	if err != nil {
		return chatUpstreamError(c, logger, model, err)
	}
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	id, created := newResponsesID(), time.Now().Unix()
	c.Response().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer raw.Close()
		bad := 0
		sc := bufio.NewScanner(raw)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		var completedData []byte
		for sc.Scan() {
			line := sc.Text()
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "data:") {
				if strings.HasPrefix(trimmed, "event:") && strings.Contains(trimmed, "response.completed") {
					continue
				}
				fmt.Fprintln(w, line)
				_ = w.Flush()
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				continue
			}
			var ev map[string]json.RawMessage
			if json.Unmarshal([]byte(payload), &ev) != nil {
				bad++
				continue
			}
			if respRaw, ok := ev["response"]; ok {
				var resp map[string]json.RawMessage
				if json.Unmarshal(respRaw, &resp) == nil {
					resp["id"], _ = json.Marshal(id)
					resp["model"], _ = json.Marshal(model)
					resp["created_at"], _ = json.Marshal(created)
					ev["response"], _ = json.Marshal(resp)
				}
			}
			evType := ""
			if t, ok := ev["type"]; ok {
				json.Unmarshal(t, &evType)
			}
			if evType == "response.completed" {
				completedData, _ = json.Marshal(ev)
				continue
			}
			rewritten, err := json.Marshal(ev)
			if err != nil {
				bad++
				continue
			}
			fmt.Fprintln(w, "data: "+string(rewritten))
			_ = w.Flush()
		}
		if err := sc.Err(); err != nil {
			logger.Error("responses sse relay aborted", "model", model, "error", err)
			_ = w.Flush()
			return
		}
		if bad > 0 {
			logger.Warn("responses sse relay skipped malformed chunks", "model", model, "count", bad)
		}
		if completedData != nil {
			fmt.Fprintln(w, "event: response.completed")
			fmt.Fprintln(w, "data: "+string(completedData))
			_ = w.Flush()
		}
	})
	return nil
}

// newResponsesID generates a proxy-side response id matching the
// provider-side format (resp_ + 26 base62).
func newResponsesID() string {
	const b62 = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var b [26]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("resp_quirk_%d", time.Now().UnixNano())
	}
	out := make([]byte, 26)
	for i := range b {
		out[i] = b62[int(b[i])%len(b62)]
	}
	return "resp_" + string(out)
}
