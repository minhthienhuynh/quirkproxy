package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// zenEntry is one model in the Zen catalog.
type zenEntry struct {
	ID      string
	Created int64
}

func (z *zenEntry) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	z.ID, z.Created = raw.ID, raw.Created
	return nil
}

// fetchZenCatalog GETs the public Zen model catalog. Items without an id
// are skipped instead of failing the whole listing.
func fetchZenCatalog(ctx context.Context, client *http.Client, url string) (map[string]zenEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("zen catalog request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zen catalog fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zen catalog status: %d", resp.StatusCode)
	}
	var raw struct {
		Data []zenEntry `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("zen catalog decode: %w", err)
	}
	out := make(map[string]zenEntry, len(raw.Data))
	for _, e := range raw.Data {
		if e.ID == "" {
			continue
		}
		out[e.ID] = e
	}
	return out, nil
}

// flexBool is true for JSON true or any non-null object (models.dev uses
// both shapes for capability flags like reasoning).
type flexBool bool

// UnmarshalJSON implements json.Unmarshaler.
func (b *flexBool) UnmarshalJSON(data []byte) error {
	var v bool
	if err := json.Unmarshal(data, &v); err == nil {
		*b = flexBool(v)
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err == nil {
		*b = flexBool(m != nil)
		return nil
	}
	*b = false
	return nil
}

// flexField is a string that also accepts {"field": "..."} objects
// (models.dev uses both shapes for interleaved).
type flexField string

// UnmarshalJSON implements json.Unmarshaler.
func (f *flexField) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*f = flexField(s)
		return nil
	}
	var m struct {
		Field string `json:"field"`
	}
	if err := json.Unmarshal(data, &m); err == nil {
		*f = flexField(m.Field)
		return nil
	}
	*f = ""
	return nil
}

// devReasoningOption mirrors one entry of models.dev reasoning_options.
type devReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
	Min    int      `json:"min"`
	Max    int      `json:"max"`
}

// devModel is the subset of models.dev model metadata quirkproxy needs.
type devModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Family      string `json:"family"`
	Status      string `json:"status"`
	Cost        *struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
	} `json:"cost"`
	Limit *struct {
		Context int `json:"context"`
		Input   int `json:"input"`
		Output  int `json:"output"`
	} `json:"limit"`
	Attachment       flexBool             `json:"attachment"`
	Reasoning        flexBool             `json:"reasoning"`
	ReasoningOptions []devReasoningOption `json:"reasoning_options"`
	ToolCall         *bool                `json:"tool_call"`
	Temperature      flexBool             `json:"temperature"`
	Modalities       *struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	ReleaseDate string    `json:"release_date"`
	LastUpdated string    `json:"last_updated"`
	Interleaved flexField `json:"interleaved"`
	Provider    *struct {
		NPM string `json:"npm"`
		API string `json:"api"`
	} `json:"provider"`
}

// devProvider is the subset of one models.dev provider entry.
type devProvider struct {
	NPM    string              `json:"npm"`
	API    string              `json:"api"`
	Models map[string]devModel `json:"models"`
}

func httpClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Keep a warm pool for concurrent upstream calls: the default keeps
	// only 2 idle conns per host, forcing a fresh TCP+TLS handshake
	// for every burst beyond that.
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 32
	transport.IdleConnTimeout = 90 * time.Second
	return &http.Client{Timeout: 15 * time.Second, Transport: transport}
}

// fetchModelsDev GETs models.dev and extracts the opencode provider entry.
func fetchModelsDev(ctx context.Context, client *http.Client, url string) (devProvider, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return devProvider{}, fmt.Errorf("models.dev request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return devProvider{}, fmt.Errorf("models.dev fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return devProvider{}, fmt.Errorf("models.dev status: %d", resp.StatusCode)
	}
	var top map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&top); err != nil {
		return devProvider{}, fmt.Errorf("models.dev decode: %w", err)
	}
	raw, ok := top["opencode"]
	if !ok {
		return devProvider{}, fmt.Errorf("models.dev: no opencode provider entry")
	}
	var prov devProvider
	if err := json.Unmarshal(raw, &prov); err != nil {
		return devProvider{}, fmt.Errorf("models.dev opencode decode: %w", err)
	}
	return prov, nil
}
