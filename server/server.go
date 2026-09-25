package server

import (
	"log/slog"

	"github.com/gofiber/fiber/v3"
	fiberlogger "github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/gofiber/fiber/v3/middleware/recover"

	"github.com/minhthienhuynh/quirkproxy/provider"
)

// New builds the HTTP application with all routes registered.
func New(reg *provider.Registry, logger *slog.Logger) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName: "quirkproxy",
	})
	app.Use(recover.New())
	app.Use(fiberlogger.New(fiberlogger.Config{
		Format: "${method} ${path} -> ${status} (${latency})\n",
	}))

	app.Get("/healthz", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Get("/v1/models", modelsHandler(reg, logger))
	// Provider-scoped listing. Registered after the aggregate route
	// because Fiber matches routes in registration order.
	app.Get("/:provider/v1/models", modelsHandler(reg, logger))
	app.Post("/v1/chat/completions", chatHandler(reg, logger))
	// Provider-scoped chat, registered after the aggregate route
	// because Fiber matches routes in registration order.
	app.Post("/:provider/v1/chat/completions", chatHandler(reg, logger))
	app.Post("/v1/responses", responsesHandler(reg, logger))
	// Provider-scoped responses, registered after the aggregate route
	// because Fiber matches routes in registration order.
	app.Post("/:provider/v1/responses", responsesHandler(reg, logger))

	return app
}
