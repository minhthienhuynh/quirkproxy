package server

import (
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/minhthienhuynh/quirkproxy/model"
	"github.com/minhthienhuynh/quirkproxy/provider"
)

// modelItem is one entry of the GET /v1/models response: the OpenAI core,
// familiar extension fields, and the quirk debug namespace.
type modelItem struct {
	ID                  string             `json:"id"`
	Object              string             `json:"object"`
	Created             int64              `json:"created"`
	OwnedBy             string             `json:"owned_by"`
	Name                string             `json:"name"`
	ContextLength       int                `json:"context_length"`
	MaxCompletionTokens int                `json:"max_completion_tokens"`
	Pricing             pricing            `json:"pricing"`
	Capabilities        capabilities       `json:"capabilities"`
	Variants            map[string]variant `json:"variants"`
	Quirk               quirk              `json:"quirk"`
}

type pricing struct {
	Prompt     float64 `json:"prompt"`
	Completion float64 `json:"completion"`
	CacheRead  float64 `json:"cache_read"`
}

type capabilities struct {
	ToolCall    bool         `json:"tool_call"`
	Reasoning   bool         `json:"reasoning"`
	Attachment  bool         `json:"attachment"`
	Temperature bool         `json:"temperature"`
	Modalities  modalities   `json:"modalities"`
	Interleaved *interleaved `json:"interleaved,omitempty"`
}

type interleaved struct {
	Field string `json:"field"`
}

type modalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type variant struct {
	ReasoningEffort string `json:"reasoning_effort"`
}

type quirk struct {
	Status    string `json:"status"`
	Family    string `json:"family"`
	Transport string `json:"transport"`
	Endpoint  string `json:"endpoint"`
}

type modelList struct {
	Object string      `json:"object"`
	Data   []modelItem `json:"data"`
}

func catalogError(c fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{"error": fiber.Map{"message": msg, "type": "invalid_request_error", "code": status}})
}

func toItem(m model.Model) modelItem {
	variants := make(map[string]variant, len(m.Variants))
	for name, v := range m.Variants {
		variants[name] = variant{ReasoningEffort: v.ReasoningEffort}
	}
	caps := capabilities{
		ToolCall:    m.Capabilities.ToolCall,
		Reasoning:   m.Capabilities.Reasoning,
		Attachment:  m.Capabilities.Attachment,
		Temperature: m.Capabilities.Temperature,
		Modalities: modalities{
			Input:  append([]string(nil), m.Capabilities.Modalities.Input...),
			Output: append([]string(nil), m.Capabilities.Modalities.Output...),
		},
	}
	if m.Capabilities.InterleavedField != "" {
		caps.Interleaved = &interleaved{Field: m.Capabilities.InterleavedField}
	}
	return modelItem{
		ID:                  m.ID,
		Object:              "model",
		Created:             m.Created,
		OwnedBy:             m.Owner,
		Name:                m.Name,
		ContextLength:       m.Limit.Context,
		MaxCompletionTokens: m.Limit.Output,
		Pricing: pricing{
			Prompt:     m.Cost.Input,
			Completion: m.Cost.Output,
			CacheRead:  m.Cost.CacheRead,
		},
		Capabilities: caps,
		Variants:     variants,
		Quirk: quirk{
			Status:    m.Status,
			Family:    m.Family,
			Transport: m.Transport,
			Endpoint:  m.Endpoint,
		},
	}
}

// modelsHandler serves GET /v1/models aggregated over enabled providers,
// or scoped to one provider when the route carries a :provider param.
// Duplicate ids across providers resolve first-wins with a warning.
func modelsHandler(reg *provider.Registry, logger *slog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		filter := c.Params("provider", "")
		items := []modelItem{}
		seen := make(map[string]string)
		var maxAge time.Duration
		provs := reg.SortedEnabled()
		if filter != "" {
			p, ok := reg.Lookup(filter)
			if !ok || !reg.Enabled(filter) {
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
					"error": "unknown or disabled provider " + strconv.Quote(filter),
				})
			}
			provs = []provider.ModelLister{p}
		}
		for _, p := range provs {
			res, err := p.ListModels(c.Context())
			if err != nil {
				if errors.Is(err, provider.ErrNoCache) || errors.Is(err, provider.ErrUpstreamUnavailable) {
					return catalogError(c, fiber.StatusServiceUnavailable,
						"model catalog unavailable: upstream unreachable and no usable cache")
				}
				return catalogError(c, fiber.StatusBadGateway, "model catalog unavailable")
			}
			if res.Age > maxAge {
				maxAge = res.Age
			}
			for _, m := range res.Models {
				if first, dup := seen[m.ID]; dup {
					logger.Warn("duplicate model id, keeping first",
						"model", m.ID, "kept_provider", first, "skipped_provider", p.ID())
					continue
				}
				seen[m.ID] = p.ID()
				items = append(items, toItem(m))
			}
		}
		c.Set("X-Quirk-Cache-Age", strconv.FormatInt(int64(maxAge/time.Second), 10)+"s")
		return c.JSON(modelList{Object: "list", Data: items})
	}
}
