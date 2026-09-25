package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Duration is a time.Duration that unmarshals from strings like "1h30m".
type Duration time.Duration

// UnmarshalJSON parses a duration string.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Std returns the standard time.Duration.
func (d Duration) Std() time.Duration {
	return time.Duration(d)
}

// Provider is the shared configuration envelope every provider uses.
type Provider struct {
	ID           string              `json:"id"`
	Type         string              `json:"type"`
	Enabled      bool                `json:"enabled"`
	Blacklist    []string            `json:"blacklist"`
	Whitelist    []string            `json:"whitelist"`
	RefreshTTL   Duration            `json:"refresh_ttl"`
	Refresh      map[string]Duration `json:"refresh,omitempty"`
	MaxCacheAge  Duration            `json:"max_cache_age"`
	Experimental bool                `json:"experimental,omitempty"`
	Options      map[string]any      `json:"options,omitempty"`
}

// RefreshFor returns the TTL for a named source, falling back to RefreshTTL.
func (p Provider) RefreshFor(source string) time.Duration {
	if v, ok := p.Refresh[source]; ok {
		return v.Std()
	}
	return p.RefreshTTL.Std()
}

// Config is the top-level quirkproxy configuration.
type Config struct {
	Listen    string     `json:"listen"`
	Providers []Provider `json:"providers"`
}

// Load reads and parses the configuration file at path, applying defaults
// (refresh_ttl 1h, max_cache_age 24h) and rejecting envelopes that would
// silently misbehave (empty listen, empty provider id/type).
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Listen == "" {
		return Config{}, fmt.Errorf("config: listen must not be empty")
	}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.ID == "" {
			return Config{}, fmt.Errorf("config: provider %d has empty id", i)
		}
		if p.Type == "" {
			return Config{}, fmt.Errorf("config: provider %q has empty type", p.ID)
		}
		if p.RefreshTTL.Std() <= 0 {
			p.RefreshTTL = Duration(time.Hour)
		}
		for src, ttl := range p.Refresh {
			if ttl.Std() <= 0 {
				return Config{}, fmt.Errorf("config: provider %q refresh %q must be positive", p.ID, src)
			}
		}
		if p.MaxCacheAge.Std() <= 0 {
			p.MaxCacheAge = Duration(24 * time.Hour)
		}
	}
	return cfg, nil
}
