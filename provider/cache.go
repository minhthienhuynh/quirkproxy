package provider

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNoCache is returned when a fetch fails and no cached value exists.
var ErrNoCache = errors.New("provider: no cached value and upstream fetch failed")

// ErrUpstreamUnavailable is returned when a fetch fails and the cached
// value is older than the maximum age.
var ErrUpstreamUnavailable = errors.New("provider: upstream unavailable and cache expired")

// FetchFunc fetches a fresh value from upstream.
type FetchFunc[T any] func(ctx context.Context) (T, error)

// Refresher is a shared stale-while-revalidate cache. Reads never block on
// upstream once a value exists: stale values are served immediately while a
// background refresh runs.
type Refresher[T any] struct {
	fetch   FetchFunc[T]
	ttl     time.Duration
	maxAge  time.Duration
	mu      sync.Mutex
	value   T
	fetched time.Time
	has     bool
	ongoing atomic.Bool
}

// NewRefresher returns a Refresher with the given TTL and maximum cache age.
func NewRefresher[T any](fetch FetchFunc[T], ttl, maxAge time.Duration) *Refresher[T] {
	return &Refresher[T]{fetch: fetch, ttl: ttl, maxAge: maxAge}
}

// Get returns the cached value and its age. When no usable cache exists it
// fetches synchronously.
func (r *Refresher[T]) Get(ctx context.Context) (T, time.Duration, error) {
	r.mu.Lock()
	has, value, fetched := r.has, r.value, r.fetched
	r.mu.Unlock()

	now := time.Now()
	if has {
		age := now.Sub(fetched)
		if age <= r.ttl {
			return value, age, nil
		}
		if age <= r.maxAge {
			r.refreshAsync()
			return value, age, nil
		}
		if v, age, err := r.fetchSync(ctx); err == nil {
			return v, age, nil
		} else {
			var zero T
			return zero, age, errors.Join(ErrUpstreamUnavailable, err)
		}
	}
	return r.fetchSync(ctx)
}

func (r *Refresher[T]) fetchSync(ctx context.Context) (T, time.Duration, error) {
	v, err := r.fetch(ctx)
	if err != nil {
		var zero T
		return zero, 0, errors.Join(ErrNoCache, err)
	}
	r.mu.Lock()
	r.value, r.fetched, r.has = v, time.Now(), true
	r.mu.Unlock()
	return v, 0, nil
}

func (r *Refresher[T]) refreshAsync() {
	if !r.ongoing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer r.ongoing.Store(false)
		v, err := r.fetch(context.Background())
		if err != nil {
			return
		}
		r.mu.Lock()
		r.value, r.fetched, r.has = v, time.Now(), true
		r.mu.Unlock()
	}()
}
