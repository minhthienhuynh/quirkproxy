package opencode

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/minhthienhuynh/quirkproxy/config"
	"github.com/minhthienhuynh/quirkproxy/provider"
)

func loadFixtures(t *testing.T) (map[string]zenEntry, devProvider) {
	t.Helper()

	zenData, err := os.ReadFile("testdata/zen-models.json")
	if err != nil {
		t.Fatalf("read zen fixture: %v", err)
	}
	var zenRaw struct {
		Data []zenEntry `json:"data"`
	}
	if err := json.Unmarshal(zenData, &zenRaw); err != nil {
		t.Fatalf("parse zen fixture: %v", err)
	}
	zen := make(map[string]zenEntry, len(zenRaw.Data))
	for _, e := range zenRaw.Data {
		zen[e.ID] = e
	}

	devData, err := os.ReadFile("testdata/models-dev-opencode.json")
	if err != nil {
		t.Fatalf("read dev fixture: %v", err)
	}
	var dev devProvider
	if err := json.Unmarshal(devData, &dev); err != nil {
		t.Fatalf("parse dev fixture: %v", err)
	}
	return zen, dev
}

func testConfig() config.Provider {
	return config.Provider{ID: "opencode", Type: "opencode", Enabled: true}
}

func idsOf(t *testing.T, zen map[string]zenEntry, dev devProvider, cfg config.Provider) []string {
	t.Helper()
	models, _ := merge(zen, dev, cfg, "opencode")
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids
}

func TestMergeYieldsKnownFreeSeven(t *testing.T) {
	zen, dev := loadFixtures(t)
	want := []string{
		"big-pickle",
		"ling-3.0-flash-fin-free",
		"mimo-v2.6-flash-free",
		"muse-spark-1.3-contributor-free",
		"nemotron-3-ultra-free",
		"nemotron-3.5-lightning-free",
		"space-bunny-free",
	}
	got := idsOf(t, zen, dev, testConfig())
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("models = %v, want %v", got, want)
		}
	}
}

func TestMergeDrops(t *testing.T) {
	zen, dev := loadFixtures(t)
	_, drops := merge(zen, dev, testConfig(), "opencode")
	byID := make(map[string]provider.DropReason)
	for _, d := range drops {
		byID[d.ModelID] = d.Reason
	}
	cases := map[string]provider.DropReason{
		"gpt-5.5":                         provider.DropPrice,
		"mimo-v2.5-free":                  provider.DropStatus,
		"muse-spark-1.2-contributor-free": provider.DropStatus,
		"deepseek-v4-flash-free":          provider.DropStatus,
		"qwen3.6-plus-free":               provider.DropStatus,
		"test-cost-null":                  provider.DropMissingMetadata,
		"test-absent-zen":                 provider.DropMissingCatalog,
	}
	for id, want := range cases {
		if byID[id] != want {
			t.Fatalf("drop[%s] = %q, want %q (all drops: %v)", id, byID[id], want, byID)
		}
	}
}

func TestMergeBlacklist(t *testing.T) {
	zen, dev := loadFixtures(t)
	cfg := testConfig()
	cfg.Blacklist = []string{"big-pickle"}
	models, drops := merge(zen, dev, cfg, "opencode")
	for _, m := range models {
		if m.ID == "big-pickle" {
			t.Fatal("big-pickle present despite blacklist")
		}
	}
	found := false
	for _, d := range drops {
		if d.ModelID == "big-pickle" && d.Reason == provider.DropListFilter {
			found = true
		}
	}
	if !found {
		t.Fatal("no list_filter drop recorded for big-pickle")
	}
}

func TestMergeVariantsMatchCLI(t *testing.T) {
	zen, dev := loadFixtures(t)
	models, _ := merge(zen, dev, testConfig(), "opencode")
	got := make(map[string][]string)
	for _, m := range models {
		names := make([]string, 0, len(m.Variants))
		for n := range m.Variants {
			names = append(names, n)
		}
		sort.Strings(names)
		got[m.ID] = names
	}
	want := map[string][]string{
		"big-pickle":                      {},
		"ling-3.0-flash-fin-free":         {"high", "low", "medium"},
		"mimo-v2.6-flash-free":            {},
		"muse-spark-1.3-contributor-free": {"high", "low", "medium", "minimal", "xhigh"},
		"nemotron-3-ultra-free":           {},
		"nemotron-3.5-lightning-free":     {},
		"space-bunny-free":                {"high", "low", "max", "medium", "xhigh"},
	}
	for id, names := range want {
		if len(got[id]) != len(names) {
			t.Fatalf("variants[%s] = %v, want %v", id, got[id], names)
		}
		for i := range names {
			if got[id][i] != names[i] {
				t.Fatalf("variants[%s] = %v, want %v", id, got[id], names)
			}
		}
	}
	// Payload check: effort variants carry their own name.
	for _, m := range models {
		for name, v := range m.Variants {
			if v.ReasoningEffort != name {
				t.Fatalf("variant %s/%s payload = %q, want name", m.ID, name, v.ReasoningEffort)
			}
		}
	}
}

func TestMergeTransportAndEndpoint(t *testing.T) {
	zen, dev := loadFixtures(t)
	models, _ := merge(zen, dev, testConfig(), "opencode")
	byID := make(map[string]string)
	endpoint := make(map[string]string)
	for _, m := range models {
		byID[m.ID] = m.Transport
		endpoint[m.ID] = m.Endpoint
	}
	if byID["muse-spark-1.3-contributor-free"] != "@ai-sdk/openai" {
		t.Fatalf("muse-spark transport = %q", byID["muse-spark-1.3-contributor-free"])
	}
	if endpoint["muse-spark-1.3-contributor-free"] != "responses" {
		t.Fatalf("muse-spark endpoint = %q", endpoint["muse-spark-1.3-contributor-free"])
	}
	if endpoint["mimo-v2.6-flash-free"] != "chat/completions" {
		t.Fatalf("mimo endpoint = %q", endpoint["mimo-v2.6-flash-free"])
	}
}

func TestVariantsBudgetTokens(t *testing.T) {
	got := variants([]devReasoningOption{{Type: "budget_tokens", Min: 100, Max: 1000}})
	if len(got) != 2 || got["high"].ReasoningEffort != "high" || got["max"].ReasoningEffort != "max" {
		t.Fatalf("budget variants = %+v, want high+max", got)
	}
}

func TestMergeAlphaAndWhitelist(t *testing.T) {
	cost0 := &struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
	}{}
	mkDev := func() devProvider {
		return devProvider{NPM: "@ai-sdk/openai-compatible", Models: map[string]devModel{
			"alpha-free": {ID: "alpha-free", Status: "alpha", Cost: cost0},
			"beta-free":  {ID: "beta-free", Cost: cost0},
		}}
	}
	zen := map[string]zenEntry{"alpha-free": {ID: "alpha-free"}, "beta-free": {ID: "beta-free"}}

	got, _ := merge(zen, mkDev(), testConfig(), "opencode")
	if len(got) != 1 || got[0].ID != "beta-free" {
		t.Fatalf("without experimental = %v, want only beta-free", got)
	}
	cfg := testConfig()
	cfg.Experimental = true
	got, _ = merge(zen, mkDev(), cfg, "opencode")
	if len(got) != 2 {
		t.Fatalf("with experimental = %v, want both", got)
	}
	cfg.Whitelist = []string{"alpha-free"}
	got, _ = merge(zen, mkDev(), cfg, "opencode")
	if len(got) != 1 || got[0].ID != "alpha-free" {
		t.Fatalf("with whitelist = %v, want only alpha-free", got)
	}
}
