package opencode

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/minhthienhuynh/quirkproxy/config"
	"github.com/minhthienhuynh/quirkproxy/model"
	"github.com/minhthienhuynh/quirkproxy/provider"
)

const (
	defaultZenModelsURL  = "https://opencode.ai/zen/v1/models"
	defaultModelsDevURL  = "https://models.dev/api.json"
	openaiCompatibleNPM  = "@ai-sdk/openai-compatible"
	openaiNPM            = "@ai-sdk/openai"
	responsesZenEndpoint = "responses"
	chatZenEndpoint      = "chat/completions"
)

// Provider implements provider.ModelLister for OpenCode Zen free models.
type Provider struct {
	id       string
	cfg      config.Provider
	client   *http.Client
	logger   *slog.Logger
	zenURL   string
	devURL   string
	zenCache *provider.Refresher[map[string]zenEntry]
	devCache *provider.Refresher[devProvider]
	chat     *zenChatClient
}

// New returns an opencode provider bound to the given config envelope.
// The provider id comes from the envelope (default "opencode") so routing
// (/ :provider/v1/models) and enable/disable stay consistent.
func New(cfg config.Provider, logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	id := cfg.ID
	if id == "" {
		id = "opencode"
	}
	client := httpClient()
	p := &Provider{
		id:     id,
		cfg:    cfg,
		client: client,
		logger: logger,
		zenURL: stringOption(cfg.Options, "zen_models_url", defaultZenModelsURL),
		devURL: stringOption(cfg.Options, "models_dev_url", defaultModelsDevURL),
	}
	p.zenCache = provider.NewRefresher(
		withRetry(func(ctx context.Context) (map[string]zenEntry, error) {
			return fetchZenCatalog(ctx, client, p.zenURL)
		}),
		cfg.RefreshFor("zen"), cfg.MaxCacheAge.Std(),
	)
	p.devCache = provider.NewRefresher(
		withRetry(func(ctx context.Context) (devProvider, error) {
			return fetchModelsDev(ctx, client, p.devURL)
		}),
		cfg.RefreshFor("models_dev"), cfg.MaxCacheAge.Std(),
	)
	p.chat = &zenChatClient{
		client:     inferenceClient(durationOption(cfg.Options, "inference_header_timeout", defaultInferenceHeaderTimeout)),
		logger:     logger,
		genTimeout: durationOption(cfg.Options, "inference_generation_timeout", defaultInferenceGenerationTimeout),
		recipe: recipeFromOptions(cfg.Options,
			stringOption(cfg.Options, "zen_api_url", "https://opencode.ai/zen/v1")),
	}
	return p
}

// withRetry wraps a fetch with fixed retry policy shared by all sources.
func withRetry[T any](fetch provider.FetchFunc[T]) provider.FetchFunc[T] {
	return func(ctx context.Context) (T, error) {
		var out T
		err := provider.Retry(ctx, 3, 500*time.Millisecond, func(ctx context.Context) error {
			var ferr error
			out, ferr = fetch(ctx)
			return ferr
		})
		return out, err
	}
}

func stringOption(opts map[string]any, key, fallback string) string {
	if v, ok := opts[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

// durationOption reads a duration string (e.g. "10m", "30s") from options.
// Absent, empty, malformed, or non-positive values fall back to the default so
// a bad config value never disables the bound entirely.
func durationOption(opts map[string]any, key string, fallback time.Duration) time.Duration {
	v, ok := opts[key]
	if !ok {
		return fallback
	}
	s, ok := v.(string)
	if !ok {
		return fallback
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

var (
	_ provider.ModelLister        = (*Provider)(nil)
	_ provider.ChatCompleter      = (*Provider)(nil)
	_ provider.ResponsesCompleter = (*Provider)(nil)
)

// ID implements provider.ModelLister.
func (p *Provider) ID() string { return p.id }

// Capabilities implements provider.ModelLister.
func (p *Provider) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityListModels, provider.CapabilityChat, provider.CapabilityResponses}
}

// Chat implements provider.ChatCompleter.
func (p *Provider) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResult, error) {
	return p.chat.chat(ctx, req, req.Model)
}

// ChatRaw implements provider.ChatCompleter.
func (p *Provider) ChatRaw(ctx context.Context, req provider.ChatRequest) (io.ReadCloser, error) {
	return p.chat.postRaw(ctx, buildBody(req, req.Model))
}

// Responses implements provider.ResponsesCompleter.
func (p *Provider) Responses(ctx context.Context, req provider.ResponsesRequest) (provider.ResponsesResult, error) {
	raw, err := p.chat.postResponsesRaw(ctx, buildRespBody(req, req.Model))
	if err != nil {
		return provider.ResponsesResult{}, err
	}
	defer raw.Close()
	id, err := newRespID()
	if err != nil {
		return provider.ResponsesResult{}, err
	}
	payload, err := aggregateResponses(raw, id, req.Model, time.Now().Unix())
	if err != nil {
		return provider.ResponsesResult{}, err
	}
	return provider.ResponsesResult{Payload: payload}, nil
}

// ResponsesRaw implements provider.ResponsesCompleter.
func (p *Provider) ResponsesRaw(ctx context.Context, req provider.ResponsesRequest) (io.ReadCloser, error) {
	return p.chat.postResponsesRaw(ctx, buildRespBody(req, req.Model))
}

// ListModels implements provider.ModelLister.
func (p *Provider) ListModels(ctx context.Context) (provider.ListResult, error) {
	zen, zenAge, err := p.zenCache.Get(ctx)
	if err != nil {
		return provider.ListResult{}, err
	}
	dev, devAge, err := p.devCache.Get(ctx)
	if err != nil {
		return provider.ListResult{}, err
	}
	models, drops := merge(zen, dev, p.cfg, p.id)
	provider.LogDrops(p.logger, p.ID(), drops)
	age := zenAge
	if devAge > age {
		age = devAge
	}
	return provider.ListResult{Models: models, Drops: drops, Age: age}, nil
}

// merge joins the Zen catalog with models.dev metadata and applies the CLI
// filter chain: cost, status, catalog intersection (fail-closed), lists.
func merge(zen map[string]zenEntry, dev devProvider, cfg config.Provider, owner string) ([]model.Model, []provider.Drop) {
	var models []model.Model
	var drops []provider.Drop
	drop := func(id string, reason provider.DropReason, detail string) {
		drops = append(drops, provider.Drop{ModelID: id, Reason: reason, Detail: detail})
	}

	ids := make([]string, 0, len(dev.Models))
	for id := range dev.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		m := dev.Models[id]
		if m.Cost != nil && (m.Cost.Input != 0 || (m.Cost.ContextOver200k != nil && m.Cost.ContextOver200k.Input != 0)) {
			drop(id, provider.DropPrice, "cost.input != 0")
			continue
		}
		status := m.Status
		if status == "" {
			status = "active"
		}
		if status == "deprecated" {
			drop(id, provider.DropStatus, "status="+status)
			continue
		}
		ze, ok := zen[id]
		if !ok {
			drop(id, provider.DropMissingCatalog, "absent from zen catalog")
			continue
		}
		if !passLists(id, cfg) {
			drop(id, provider.DropListFilter, "blacklist/whitelist")
			continue
		}
		models = append(models, normalize(id, m, ze, dev, owner))
	}
	return models, drops
}

func passLists(id string, cfg config.Provider) bool {
	for _, b := range cfg.Blacklist {
		if b == id {
			return false
		}
	}
	if len(cfg.Whitelist) == 0 {
		return true
	}
	for _, w := range cfg.Whitelist {
		if w == id {
			return true
		}
	}
	return false
}

// normalize maps one models.dev entry to the internal representation,
// mirroring fromModelsDevModel in the CLI.
func normalize(id string, m devModel, ze zenEntry, dev devProvider, owner string) model.Model {
	npm := openaiCompatibleNPM
	switch {
	case m.Provider != nil && m.Provider.NPM != "":
		npm = m.Provider.NPM
	case dev.NPM != "":
		npm = dev.NPM
	}
	endpoint := chatZenEndpoint
	if npm == openaiNPM {
		endpoint = responsesZenEndpoint
	}

	toolCall := true
	if m.ToolCall != nil {
		toolCall = *m.ToolCall
	}
	modalities := model.Modalities{}
	if m.Modalities != nil {
		modalities.Input = append([]string(nil), m.Modalities.Input...)
		modalities.Output = append([]string(nil), m.Modalities.Output...)
	}
	out := model.Model{
		ID:          id,
		Name:        m.Name,
		Description: m.Description,
		Family:      m.Family,
		Status:      "active",
		Owner:       owner,
		Capabilities: model.Capabilities{
			ToolCall:         toolCall,
			Reasoning:        bool(m.Reasoning),
			Attachment:       bool(m.Attachment),
			Temperature:      bool(m.Temperature),
			Modalities:       modalities,
			InterleavedField: string(m.Interleaved),
		},
		Transport: npm,
		Endpoint:  endpoint,
		Created:   ze.Created,
		Variants:  variants(m.ReasoningOptions),
	}
	if m.Cost != nil {
		out.Cost = model.Cost{
			Input:      m.Cost.Input,
			Output:     m.Cost.Output,
			CacheRead:  m.Cost.CacheRead,
			CacheWrite: m.Cost.CacheWrite,
		}
	}
	if m.Limit != nil {
		out.Limit = model.Limit{Context: m.Limit.Context, Input: m.Limit.Input, Output: m.Limit.Output}
	}
	return out
}

// variants maps models.dev reasoning_options to named presets, mirroring
// reasoningVariants in the CLI for the transports Zen uses. Absent or empty
// options yield no variants (this intentionally short-circuits the CLI's
// family matrix, which is out of scope).
func variants(opts []devReasoningOption) map[string]model.Variant {
	out := map[string]model.Variant{}
	if len(opts) == 0 {
		return out
	}
	for _, o := range opts {
		switch o.Type {
		case "effort":
			for _, v := range o.Values {
				out[v] = model.Variant{ReasoningEffort: v}
			}
		case "toggle":
			for _, v := range []string{"low", "medium", "high"} {
				out[v] = model.Variant{ReasoningEffort: v}
			}
		case "budget_tokens":
			for _, v := range []string{"high", "max"} {
				out[v] = model.Variant{ReasoningEffort: v}
			}
		}
	}
	return out
}
