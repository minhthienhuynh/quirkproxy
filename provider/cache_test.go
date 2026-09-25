package provider

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRefresherFresh(t *testing.T) {
	var calls atomic.Int32
	r := NewRefresher(func(_ context.Context) (string, error) {
		calls.Add(1)
		return "v1", nil
	}, time.Minute, time.Hour)

	v, age, err := r.Get(context.Background())
	if err != nil || v != "v1" {
		t.Fatalf("Get = %q, %v; want v1, nil", v, err)
	}
	if age != 0 {
		t.Fatalf("age = %v, want 0 on first fetch", age)
	}
	if _, _, err := r.Get(context.Background()); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fetch calls = %d, want 1 (served from cache)", got)
	}
}

func TestRefresherStaleServesAndRefreshes(t *testing.T) {
	var calls atomic.Int32
	r := NewRefresher(func(_ context.Context) (string, error) {
		n := calls.Add(1)
		if n == 1 {
			return "v1", nil
		}
		return "v2", nil
	}, 30*time.Millisecond, time.Hour)

	if _, _, err := r.Get(context.Background()); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	v, age, err := r.Get(context.Background())
	if err != nil {
		t.Fatalf("stale Get: %v", err)
	}
	if v != "v1" {
		t.Fatalf("stale Get = %q, want cached v1", v)
	}
	if age <= 0 {
		t.Fatalf("age = %v, want > 0 for stale value", age)
	}
	waitFor(t, "background refresh", func() bool { return calls.Load() >= 2 })
	v, _, err = r.Get(context.Background())
	if err != nil || v != "v2" {
		t.Fatalf("after refresh Get = %q, %v; want v2, nil", v, err)
	}
}

func TestRefresherNoCacheError(t *testing.T) {
	boom := errors.New("boom")
	r := NewRefresher(func(_ context.Context) (string, error) {
		return "", boom
	}, time.Minute, time.Hour)
	if _, _, err := r.Get(context.Background()); !errors.Is(err, ErrNoCache) {
		t.Fatalf("err = %v, want ErrNoCache", err)
	}
}

func TestRefresherExpiredCacheError(t *testing.T) {
	var calls atomic.Int32
	r := NewRefresher(func(_ context.Context) (string, error) {
		if calls.Add(1) == 1 {
			return "v1", nil
		}
		return "", errors.New("boom")
	}, 20*time.Millisecond, 40*time.Millisecond)

	if _, _, err := r.Get(context.Background()); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, _, err := r.Get(context.Background()); !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("err = %v, want ErrUpstreamUnavailable", err)
	}
}
