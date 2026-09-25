package provider

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// WatchRecipe periodically sends the cheapest possible chat probe to detect
// a rotted genuine recipe (gate or auth change). Failures log at ERROR;
// successes stay silent. A 404 means the probe model left the allowlist
// (free lists rotate) — logged distinctly from gate/auth failures.
func WatchRecipe(ctx context.Context, logger *slog.Logger, lister ModelLister, cc ChatCompleter, model string, interval time.Duration) {
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
			_, err := cc.Chat(ctx, ChatRequest{
				Model:    probeModel,
				Messages: []ChatMessage{{Role: "user", Content: "ok"}},
			})
			if err == nil {
				continue
			}
			var uerr *UpstreamError
			if errors.As(err, &uerr) && uerr.Status == 404 {
				logger.Error("recipe check failed: probe model not in allowlist (free list may have rotated)", "model", probeModel)
				continue
			}
			logger.Error("recipe check failed", "model", probeModel, "error", err)
		}
	}
}

// firstModel returns the first model id in the cached allowlist, or "".
func firstModel(ctx context.Context, lister ModelLister) string {
	res, err := lister.ListModels(ctx)
	if err != nil || len(res.Models) == 0 {
		return ""
	}
	return res.Models[0].ID
}
