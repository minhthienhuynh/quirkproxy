package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func fixtureServer(t *testing.T, file, contentType string) *httptest.Server {
	t.Helper()
	data, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatalf("read fixture %s: %v", file, err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(data)
	}))
}

func TestFetchZenCatalog(t *testing.T) {
	srv := fixtureServer(t, "zen-models.json", "application/json")
	defer srv.Close()

	got, err := fetchZenCatalog(context.Background(), httpClient(), srv.URL)
	if err != nil {
		t.Fatalf("fetchZenCatalog: %v", err)
	}
	if len(got) != 80 {
		t.Fatalf("models = %d, want 80", len(got))
	}
	for _, id := range []string{"muse-spark-1.3-contributor-free", "big-pickle", "mimo-v2.6-flash-free"} {
		if _, ok := got[id]; !ok {
			t.Fatalf("missing %s in catalog", id)
		}
	}
}

func TestFetchZenCatalogSkipsIDLess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"a"},{"created":1}]}`))
	}))
	defer srv.Close()

	got, err := fetchZenCatalog(context.Background(), httpClient(), srv.URL)
	if err != nil {
		t.Fatalf("fetchZenCatalog: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("models = %d, want 1 (id-less skipped)", len(got))
	}
}

func TestFetchModelsDev(t *testing.T) {
	// The fixture is a trimmed provider entry; wrap it like api.json does.
	inner, err := os.ReadFile("testdata/models-dev-opencode.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"opencode":` + string(inner) + `}`))
	}))
	defer wrapped.Close()

	got, err := fetchModelsDev(context.Background(), httpClient(), wrapped.URL)
	if err != nil {
		t.Fatalf("fetchModelsDev: %v", err)
	}
	if len(got.Models) != 14 {
		t.Fatalf("models = %d, want 14", len(got.Models))
	}
	mimo := got.Models["mimo-v2.6-flash-free"]
	if mimo.Cost == nil || mimo.Cost.Input != 0 {
		t.Fatalf("mimo cost = %+v, want input 0", mimo.Cost)
	}
	paid := got.Models["gpt-5.5"]
	if paid.Cost == nil || paid.Cost.Input == 0 {
		t.Fatalf("gpt-5.5 cost = %+v, want input > 0", paid.Cost)
	}
	spark := got.Models["muse-spark-1.3-contributor-free"]
	if spark.Provider == nil || spark.Provider.NPM != "@ai-sdk/openai" {
		t.Fatalf("muse-spark provider = %+v, want npm @ai-sdk/openai", spark.Provider)
	}
}
