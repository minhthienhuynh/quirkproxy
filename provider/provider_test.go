package provider

import (
	"context"
	"testing"

	"github.com/minhthienhuynh/quirkproxy/model"
)

// fake is a test-only ModelLister implementation.
type fake struct {
	id     string
	models []model.Model
}

func (f *fake) ID() string { return f.id }

func (f *fake) Capabilities() []Capability { return []Capability{CapabilityListModels} }

func (f *fake) ListModels(_ context.Context) (ListResult, error) {
	return ListResult{Models: f.models}, nil
}

var _ ModelLister = (*fake)(nil)

func TestRegisterFake(t *testing.T) {
	r := NewRegistry()
	f := &fake{id: "fake", models: []model.Model{{ID: "m"}}}
	if err := r.Register(f); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Register(f); err == nil {
		t.Fatal("expected duplicate registration error, got nil")
	}
}

func TestLookup(t *testing.T) {
	r := NewRegistry()
	f := &fake{id: "fake"}
	if err := r.Register(f); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := r.Lookup("fake")
	if !ok || got.ID() != "fake" {
		t.Fatalf("Lookup(fake) = %v, %v; want provider, true", got, ok)
	}
	if _, ok := r.Lookup("missing"); ok {
		t.Fatal("Lookup(missing) = true, want false")
	}
}

func TestEnableDisable(t *testing.T) {
	r := NewRegistry()
	a, b := &fake{id: "a"}, &fake{id: "b"}
	if err := r.Register(a); err != nil {
		t.Fatalf("Register a: %v", err)
	}
	if err := r.Register(b); err != nil {
		t.Fatalf("Register b: %v", err)
	}
	if got := len(r.EnabledProviders()); got != 2 {
		t.Fatalf("EnabledProviders = %d, want 2", got)
	}
	r.SetEnabled("b", false)
	if r.Enabled("b") {
		t.Fatal("Enabled(b) = true after disable, want false")
	}
	if !r.Enabled("a") {
		t.Fatal("Enabled(a) = false, want true")
	}
	if got := len(r.EnabledProviders()); got != 1 {
		t.Fatalf("EnabledProviders = %d, want 1", got)
	}
	r.SetEnabled("b", true)
	if !r.Enabled("b") {
		t.Fatal("Enabled(b) = false after re-enable, want true")
	}
	if r.Enabled("missing") {
		t.Fatal("Enabled(missing) = true, want false")
	}
}
