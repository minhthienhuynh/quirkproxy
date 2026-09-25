// Command quirkproxy serves free AI models through an OpenAI-compatible API.
package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/minhthienhuynh/quirkproxy/config"
	"github.com/minhthienhuynh/quirkproxy/provider"
	"github.com/minhthienhuynh/quirkproxy/provider/opencode"
	"github.com/minhthienhuynh/quirkproxy/server"
)

func main() {
	configPath := flag.String("config", "config.sample.json", "path to configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	reg := provider.NewRegistry()
	for _, pc := range cfg.Providers {
		var p provider.ModelLister
		switch pc.Type {
		case "opencode":
			p = opencode.New(pc, logger)
		default:
			logger.Warn("unknown provider type, skipping", "id", pc.ID, "type", pc.Type)
			continue
		}
		if err := reg.Register(p); err != nil {
			log.Fatalf("register provider: %v", err)
		}
		if !pc.Enabled {
			logger.Info("provider disabled, skipping", "id", pc.ID)
		}
		reg.SetEnabled(pc.ID, pc.Enabled)
		cc, _ := p.(provider.ChatCompleter)
		rc, _ := p.(provider.ResponsesCompleter)
		if (cc != nil || rc != nil) && pc.Enabled {
			startWatchdog(pc, p, cc, rc, logger)
		}
	}

	app := server.New(reg, logger)
	if err := app.Listen(cfg.Listen); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// startWatchdog launches the recipe healthcheck when the provider opts in
// via options.recipe_check_interval (e.g. "1h"). Absent or invalid means off.
// An empty recipe_check_model probes the first allowlist model, so free-list
// rotation never looks like gate rot.
func startWatchdog(pc config.Provider, p provider.ModelLister, cc provider.ChatCompleter, rc provider.ResponsesCompleter, logger *slog.Logger) {
	raw, _ := pc.Options["recipe_check_interval"].(string)
	if raw == "" {
		return
	}
	interval, err := time.ParseDuration(raw)
	if err != nil || interval <= 0 {
		logger.Warn("invalid recipe_check_interval, watchdog off", "id", pc.ID)
		return
	}
	model, _ := pc.Options["recipe_check_model"].(string)
	logger.Info("recipe watchdog on", "id", pc.ID, "interval", interval)
	go provider.WatchRecipe(context.Background(), logger, p, cc, rc, model, interval)
}
