package provider

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// WatchRecipe periodically sends the cheapest possible chat and responses
// probes to detect a rotted genuine recipe (gate or auth change). Failures
// log at ERROR; successes stay silent. A 404 means the probe model left the
// allowlist (free lists rotate) — logged distinctly from gate/auth failures.
// Either completer may be nil to skip that probe kind.
func WatchRecipe(ctx context.Context, logger *slog.Logger, lister ModelLister, cc ChatCompleter, rc ResponsesCompleter, model string, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			probeModel := model
			if probeModel == "" {
				probeModel = firstModel(ctx, lister)
			}
			if probeModel == "" {
				logger.Error("recipe check skipped: allowlist empty or unavailable")
				continue
			}
			if cc != nil {
				_, err := cc.Chat(ctx, ChatRequest{
					Model:    probeModel,
					Messages: []ChatMessage{{Role: "user", Content: "ok"}},
				})
				logProbeErr(logger, "chat", probeModel, err)
			}
			if rc != nil {
				_, err := rc.Responses(ctx, ResponsesRequest{
					Model: probeModel,
					Input: "ok",
				})
				logProbeErr(logger, "responses", probeModel, err)
			}
		}
	}
}

// logProbeErr logs a probe failure at ERROR, distinguishing allowlist
// rotation (404) from gate/auth rot. Nil err is silent.
func logProbeErr(logger *slog.Logger, kind, model string, err error) {
	if err == nil {
		return
	}
	var uerr *UpstreamError
	if errors.As(err, &uerr) && uerr.Status == 404 {
		logger.Error("recipe check failed: probe model not in allowlist (free list may have rotated)",
			"kind", kind, "model", model)
		return
	}
	logger.Error("recipe check failed", "kind", kind, "model", model, "error", err)
}

// firstModel returns the first model id in the cached allowlist, or "".
func firstModel(ctx context.Context, lister ModelLister) string {
	res, err := lister.ListModels(ctx)
	if err != nil || len(res.Models) == 0 {
		return ""
	}
	return res.Models[0].ID
}
