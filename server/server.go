package server

import (
	"log/slog"

	"github.com/gofiber/fiber/v3"

	"github.com/minhthienhuynh/quirkproxy/provider"
)

// New builds the HTTP application with all routes registered.
func New(reg *provider.Registry, logger *slog.Logger) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName: "quirkproxy",
	})

	app.Get("/healthz", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Get("/v1/models", modelsHandler(reg, logger))
	// Provider-scoped listing. Registered after the aggregate route
	// because Fiber matches routes in registration order.
	app.Get("/:provider/v1/models", modelsHandler(reg, logger))

	return app
}
