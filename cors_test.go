package main

import (
	"net/http"
	"os"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
)

// setupAppForTests creates a minimal Fiber app with the same CORS configuration
// that is used in main.go.
func setupAppForTests() *fiber.App {
	// Ensure the env var is set for the test.
	os.Setenv("CORS_ALLOWED_ORIGINS", "http://localhost:3000")

	app := fiber.New()

	// Apply the same CORS middleware as in production.
	app.Use(cors.New(cors.Config{
		AllowOrigins:     getAllowedOrigins(),
		AllowMethods:     "GET,HEAD,OPTIONS",
		AllowHeaders:     "X-API-KEY,Content-Type",
		AllowCredentials: false,
	}))

	// Stub endpoint required for the test.
	app.Get("/api/events", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	return app
}

func TestCORS(t *testing.T) {
	app := setupAppForTests()

	// 1. Origin NOT allowed -> no CORS headers
	req, _ := http.NewRequest(http.MethodOptions, "/api/events", nil)
	req.Header.Set("Origin", "http://evil.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	res, err := app.Test(req)
	if err != nil {
		t.Fatalf("failed to perform request: %v", err)
	}
	if h := res.Header.Get("Access-Control-Allow-Origin"); h != "" {
		t.Fatalf("unexpected Access-Control-Allow-Origin for disallowed origin, got %q", h)
	}

	// 2. Origin allowed -> headers present and status 204
	req2, _ := http.NewRequest(http.MethodOptions, "/api/events", nil)
	req2.Header.Set("Origin", "http://localhost:3000")
	req2.Header.Set("Access-Control-Request-Method", "GET")
	res2, err := app.Test(req2)
	if err != nil {
		t.Fatalf("failed to perform request: %v", err)
	}
	if res2.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content, got %d", res2.StatusCode)
	}
	if h := res2.Header.Get("Access-Control-Allow-Origin"); h != "http://localhost:3000" {
		t.Fatalf("missing or wrong Access-Control-Allow-Origin, got %q", h)
	}
}
