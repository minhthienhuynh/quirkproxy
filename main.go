// Command quirkproxy serves free AI models through an OpenAI-compatible API.
package main

import (
	"flag"
	"log"
	"log/slog"
	"os"

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
	}

	app := server.New(reg, logger)
	if err := app.Listen(cfg.Listen); err != nil {
		log.Fatalf("listen: %v", err)
	}
}
