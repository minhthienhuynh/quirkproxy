package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadSample(t *testing.T) {
	cfg, err := Load("testdata/sample.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8080" {
		t.Fatalf("Listen = %q, want %q", cfg.Listen, "127.0.0.1:8080")
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("Providers = %d, want 1", len(cfg.Providers))
	}
	p := cfg.Providers[0]
	if p.ID != "opencode" || !p.Enabled {
		t.Fatalf("provider = %+v, want id=opencode enabled=true", p)
	}
	if got := p.RefreshFor("zen"); got != time.Hour {
		t.Fatalf("RefreshFor(zen) = %v, want 1h", got)
	}
	if got := p.RefreshFor("models_dev"); got != 12*time.Hour {
		t.Fatalf("RefreshFor(models_dev) = %v, want 12h", got)
	}
	if got := p.RefreshFor("unknown"); got != time.Hour {
		t.Fatalf("RefreshFor(unknown) = %v, want fallback 1h", got)
	}
	if p.MaxCacheAge.Std() != 24*time.Hour {
		t.Fatalf("MaxCacheAge = %v, want 24h", p.MaxCacheAge.Std())
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := Load("testdata/does-not-exist.json"); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadDefaultsAndValidation(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()
		path := t.TempDir() + "/config.json"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		return path
	}
	cfg, err := Load(write(t, `{"listen":":8080","providers":[{"id":"x","type":"x"}]}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Providers[0].RefreshTTL.Std() != time.Hour {
		t.Fatalf("default refresh_ttl = %v, want 1h", cfg.Providers[0].RefreshTTL.Std())
	}
	if cfg.Providers[0].MaxCacheAge.Std() != 24*time.Hour {
		t.Fatalf("default max_cache_age = %v, want 24h", cfg.Providers[0].MaxCacheAge.Std())
	}
	for _, content := range []string{
		`{"providers":[{"id":"x","type":"x"}]}`,
		`{"listen":":8080","providers":[{"type":"x"}]}`,
		`{"listen":":8080","providers":[{"id":"x"}]}`,
	} {
		if _, err := Load(write(t, content)); err == nil {
			t.Fatalf("expected error for %s, got nil", content)
		}
	}
}

// writeConfig writes a minimal valid config with the given listen value.
func writeConfig(t *testing.T, listen string) string {
	t.Helper()
	path := t.TempDir() + "/config.json"
	body := `{"listen":"` + listen + `","providers":[{"id":"x","type":"x"}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// Task 1.2: QUIRKPROXY_HOST / QUIRKPROXY_PORT override the file's listen,
// each part independently; unset parts fall back to the file.
func TestLoadListenEnvOverride(t *testing.T) {
	cases := []struct {
		name             string
		file             string
		host, port, want string
	}{
		{"both set", "127.0.0.1:8080", "0.0.0.0", "9090", "0.0.0.0:9090"},
		{"only port", "127.0.0.1:8080", "", "9090", "127.0.0.1:9090"},
		{"only host", "127.0.0.1:8080", "0.0.0.0", "", "0.0.0.0:8080"},
		{"neither set", "127.0.0.1:8080", "", "", "127.0.0.1:8080"},
		{"ipv6 host preserved on port override", "[::1]:8080", "", "9090", "[::1]:9090"},
		{"ipv6 host overridden", "[::1]:8080", "0.0.0.0", "", "0.0.0.0:8080"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("QUIRKPROXY_HOST", c.host)
			t.Setenv("QUIRKPROXY_PORT", c.port)
			cfg, err := Load(writeConfig(t, c.file))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Listen != c.want {
				t.Fatalf("Listen = %q, want %q", cfg.Listen, c.want)
			}
		})
	}
}

// Task 1.2: an invalid QUIRKPROXY_PORT is a fatal config error naming the var.
func TestLoadListenEnvInvalidPort(t *testing.T) {
	for _, bad := range []string{"abc", "0", "70000", "-1", "8080x"} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv("QUIRKPROXY_HOST", "")
			t.Setenv("QUIRKPROXY_PORT", bad)
			_, err := Load(writeConfig(t, "127.0.0.1:8080"))
			if err == nil {
				t.Fatalf("expected error for port %q, got nil", bad)
			}
			if !strings.Contains(err.Error(), "QUIRKPROXY_PORT") {
				t.Fatalf("error = %v, want it to name QUIRKPROXY_PORT", err)
			}
		})
	}
}
