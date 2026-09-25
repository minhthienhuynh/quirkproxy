package provider

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestRetrySucceedsAfterFailures(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), 4, time.Millisecond, func(_ context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("flaky")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestRetryExhausts(t *testing.T) {
	boom := errors.New("boom")
	calls := 0
	err := Retry(context.Background(), 2, time.Millisecond, func(_ context.Context) error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestLogDropsOneLinePerReason(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	drops := []Drop{
		{ModelID: "m-price", Reason: DropPrice, Detail: "cost.input=20"},
		{ModelID: "m-status", Reason: DropStatus, Detail: "deprecated"},
		{ModelID: "m-catalog", Reason: DropMissingCatalog, Detail: "absent from zen"},
		{ModelID: "m-list", Reason: DropListFilter, Detail: "blacklisted"},
		{ModelID: "m-meta", Reason: DropMissingMetadata, Detail: "no cost"},
	}
	LogDrops(logger, "opencode", drops)
	out := buf.String()
	for _, d := range drops {
		if !strings.Contains(out, d.ModelID) || !strings.Contains(out, string(d.Reason)) {
			t.Fatalf("log missing entry for %s/%s:\n%s", d.ModelID, d.Reason, out)
		}
	}
	if got := strings.Count(out, "model filtered out"); got != len(drops) {
		t.Fatalf("log lines = %d, want %d", got, len(drops))
	}
}
