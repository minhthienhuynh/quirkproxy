package provider

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minhthienhuynh/quirkproxy/model"
)

type watchFake struct {
	err    error
	models []string
}

func (f *watchFake) ID() string { return "watch-fake" }

func (f *watchFake) Capabilities() []Capability { return []Capability{CapabilityChat} }

func (f *watchFake) ListModels(_ context.Context) (ListResult, error) {
	if f.err != nil {
		return ListResult{}, f.err
	}
	out := ListResult{}
	for _, id := range f.models {
		out.Models = append(out.Models, model.Model{ID: id})
	}
	return out, nil
}

func (f *watchFake) Chat(_ context.Context, _ ChatRequest) (ChatResult, error) {
	return ChatResult{}, f.err
}

func (f *watchFake) ChatRaw(_ context.Context, _ ChatRequest) (io.ReadCloser, error) {
	return nil, f.err
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestWatchRecipeLogsFailure(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	safe := &lockedWriter{mu: &mu, buf: &buf}
	logger := slog.New(slog.NewTextHandler(safe, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		WatchRecipe(ctx, logger, &watchFake{}, &watchFake{err: &UpstreamError{Status: 403, Message: "nope"}}, "m", 10*time.Millisecond)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if strings.Contains(safe.String(), "recipe check failed") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no recipe failure logged")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if !strings.Contains(safe.String(), "403") {
		t.Fatalf("log missing status: %s", safe.String())
	}
}

func TestWatchRecipeSilentOnSuccess(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	WatchRecipe(ctx, logger, &watchFake{}, &watchFake{}, "m", 10*time.Millisecond)
	if buf.Len() != 0 {
		t.Fatalf("unexpected log output: %s", buf.String())
	}
}

func TestWatchRecipeDynamicModel(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	safe := &lockedWriter{mu: &mu, buf: &buf}
	logger := slog.New(slog.NewTextHandler(safe, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	// Empty model probes the first allowlist entry; a 404 there means the
	// list rotated, logged distinctly from gate failures.
	cc := &watchFake{err: &UpstreamError{Status: 404, Message: "gone"}}
	WatchRecipe(ctx, logger, &watchFake{models: []string{"m"}}, cc, "", 10*time.Millisecond)
	if !strings.Contains(safe.String(), "not in allowlist") {
		t.Fatalf("want rotation log, got: %s", safe.String())
	}
}
