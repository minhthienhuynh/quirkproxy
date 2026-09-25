package provider

import (
	"log/slog"
)

// LogDrops logs one line per filtered-out model so a missing model can be
// traced to the exact filter step without guessing.
func LogDrops(logger *slog.Logger, providerID string, drops []Drop) {
	for _, d := range drops {
		logger.Info("model filtered out",
			"provider", providerID,
			"model", d.ModelID,
			"reason", string(d.Reason),
			"detail", d.Detail,
		)
	}
}
