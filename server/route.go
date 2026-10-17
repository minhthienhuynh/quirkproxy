package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/minhthienhuynh/quirkproxy/model"
	"github.com/minhthienhuynh/quirkproxy/provider"
)

// resolveModel finds the provider owning modelID and returns its record with
// exactly one allowlist lookup per request. Scoped routes require the
// provider to exist and be enabled with the model in its allowlist;
// aggregate routes resolve first-wins by lowest provider id (sorted) with a
// WARN on duplicates. Catalog failures map to 503/502; unknown models to
// 404. No capability filtering happens here so fallback-capable providers
// are never excluded before the endpoint branch.
func resolveModel(ctx context.Context, reg *provider.Registry, logger *slog.Logger, modelID, filter string) (provider.ModelLister, model.Model, error) {
	if filter != "" {
		p, ok := reg.Lookup(filter)
		if !ok || !reg.Enabled(filter) {
			return nil, model.Model{}, &provider.UpstreamError{Status: fiber.StatusNotFound, Type: "ProviderNotFound", Message: "unknown or disabled provider " + filter}
		}
		res, err := p.ListModels(ctx)
		if err != nil {
			return nil, model.Model{}, catalogUnavailable(err)
		}
		for _, m := range res.Models {
			if m.ID == modelID {
				return p, m, nil
			}
		}
		return nil, model.Model{}, &provider.UpstreamError{Status: fiber.StatusNotFound, Type: "ModelNotFound", Message: "model " + modelID + " not served by provider " + p.ID()}
	}
	var owner provider.ModelLister
	var owned model.Model
	for _, p := range reg.SortedEnabled() {
		res, err := p.ListModels(ctx)
		if err != nil {
			return nil, model.Model{}, catalogUnavailable(err)
		}
		for _, m := range res.Models {
			if m.ID != modelID {
				continue
			}
			if owner == nil {
				owner, owned = p, m
			} else {
				logger.Warn("duplicate model id, keeping first",
					"model", modelID, "kept_provider", owner.ID(), "skipped_provider", p.ID())
			}
			break
		}
	}
	if owner == nil {
		return nil, model.Model{}, &provider.UpstreamError{Status: fiber.StatusNotFound, Type: "ModelNotFound", Message: "unknown or non-free model " + modelID}
	}
	return owner, owned, nil
}

// requireServingCapability ensures the owning provider can serve want for a
// model it owns. A provider serving the other protocol maps to 502
// MisconfiguredProvider. A provider serving no protocol maps to 404
// (scoped keeps the documented ProviderNotFound; aggregate reports
// ModelNotFound) to preserve the long-standing non-inference-provider
// behavior.
func requireServingCapability(p provider.ModelLister, filter string, want provider.Capability) error {
	if hasCapability(p, want) {
		return nil
	}
	other := provider.CapabilityChat
	if want == provider.CapabilityChat {
		other = provider.CapabilityResponses
	}
	if hasCapability(p, other) {
		return &provider.UpstreamError{Status: fiber.StatusBadGateway, Type: "MisconfiguredProvider",
			Message: "provider " + p.ID() + " serves " + string(other) + ", not " + string(want)}
	}
	if filter != "" {
		return &provider.UpstreamError{Status: fiber.StatusNotFound, Type: "ProviderNotFound", Message: "unknown or disabled provider " + filter}
	}
	return &provider.UpstreamError{Status: fiber.StatusNotFound, Type: "ModelNotFound", Message: "no enabled provider serves " + string(want)}
}

// completerOf extracts the typed completer after the capability gate, mapping
// a dishonest advertisement (capability without implementation) to 502.
func completerOf[T any](p provider.ModelLister, filter string, want provider.Capability, kind string, extract func(provider.ModelLister) (T, bool)) (T, error) {
	var zero T
	if err := requireServingCapability(p, filter, want); err != nil {
		return zero, err
	}
	t, ok := extract(p)
	if !ok {
		return zero, &provider.UpstreamError{Status: fiber.StatusBadGateway, Type: "MisconfiguredProvider", Message: "provider advertises " + kind + " but does not implement it"}
	}
	return t, nil
}

// setSSEHeaders marks a response as a server-sent-events stream.
func setSSEHeaders(c fiber.Ctx) {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
}

// extractSessionHint returns the first non-empty session identifier from headers
// or the OpenAI user field.
func extractSessionHint(c fiber.Ctx, user string) string {
	for _, h := range []string{"x-opencode-session", "x-session-id", "x-conversation-id", "session-id"} {
		if v := strings.TrimSpace(c.Get(h)); v != "" {
			return v
		}
	}
	return strings.TrimSpace(user)
}

// extractClientIdentity computes a stable identifier for the downstream client.
func extractClientIdentity(c fiber.Ctx) string {
	if auth := strings.TrimSpace(c.Get("Authorization")); auth != "" {
		h := sha256.Sum256([]byte(auth))
		return hex.EncodeToString(h[:16])
	}
	return c.IP()
}
