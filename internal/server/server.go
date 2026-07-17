// Package server implements the HTTP proxy server that translates between
// Claude API format and OpenAI-compatible providers (OpenRouter, OpenAI Direct, Ollama).
//
// The server receives Claude API requests on /v1/messages, converts them to OpenAI format,
// forwards them to the configured provider, and converts responses back to Claude format.
// It handles both streaming (SSE) and non-streaming responses, including tool calls and
// thinking blocks from reasoning models.
package server

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/converter"
	"github.com/claude-code-proxy/proxy/internal/daemon"
	"github.com/claude-code-proxy/proxy/internal/diagnostics"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

// ProxyVersion is set by release builds with -ldflags. Development builds use this fallback.
var ProxyVersion = "dev"

// Start initializes and starts the HTTP server
func Start(cfg *config.Config) error {
	var diagnosticsStore *diagnostics.Store
	if cfg.DiagnosticsEnabled {
		var err error
		diagnosticsStore, err = diagnostics.Open(cfg.DiagnosticsDBPath, diagnostics.StoreOptions{
			Retention:        cfg.DiagnosticsRetention,
			ContentRetention: cfg.DiagnosticsContentRetention,
			CaptureContent:   cfg.DiagnosticsCaptureContent,
			BusyTimeout:      cfg.DiagnosticsBusyTimeout,
		})
		if err != nil {
			return fmt.Errorf("initialize diagnostics store: %w", err)
		}
		defer func() {
			if err := diagnosticsStore.Close(); err != nil {
				fmt.Printf("[WARN] Failed to close diagnostics store: %v\n", err)
			}
		}()
		if _, err := diagnosticsStore.Cleanup(context.Background()); err != nil {
			fmt.Printf("[WARN] Diagnostics cleanup failed: %v\n", err)
		}
	}

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ServerHeader:          "Claude-Code-Proxy",
		AppName:               "Claude Code Proxy v" + ProxyVersion,
	})

	// Middleware
	app.Use(recover.New())
	app.Use(requestIDMiddleware)
	proxyCORS := cors.New(cors.Config{
		AllowOrigins: "*",
		AllowMethods: "GET,POST,PUT,DELETE,OPTIONS",
		AllowHeaders: "*",
	})
	app.Use(func(c *fiber.Ctx) error {
		if strings.HasPrefix(c.Path(), "/debug/logs") {
			return c.Next()
		}
		return proxyCORS(c)
	})

	// Enable HTTP logging only when simple log mode is enabled
	if cfg.SimpleLog {
		app.Use(logger.New(logger.Config{
			Format: "[${time}] ${status} - ${latency} ${method} ${path}\n",
		}))
	}

	// Health check endpoint
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"status":              "ok",
			"version":             ProxyVersion,
			"diagnostics_enabled": diagnosticsStore != nil,
		})
	})

	// Root endpoint - proxy info
	app.Get("/", func(c *fiber.Ctx) error {
		endpoints := fiber.Map{
			"health":       "/health",
			"messages":     "/v1/messages",
			"count_tokens": "/v1/messages/count_tokens",
		}
		if diagnosticsStore != nil {
			endpoints["diagnostics"] = "/debug/logs"
		}
		return c.JSON(fiber.Map{
			"message": "Claude Code Proxy",
			"version": ProxyVersion,
			"status":  "running",
			"config": fiber.Map{
				"openai_base_url": cfg.OpenAIBaseURL,
				"routing_mode":    getRoutingMode(cfg),
				"opus_model":      getOpusModel(cfg),
				"sonnet_model":    getSonnetModel(cfg),
				"haiku_model":     getHaikuModel(cfg),
			},
			"endpoints": endpoints,
		})
	})

	// Claude API endpoints
	setupClaudeEndpoints(app, cfg, diagnosticsStore)
	setupDiagnosticsEndpoints(app, diagnosticsStore)

	// Graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		fmt.Println("\n🛑 Shutting down...")
		daemon.Cleanup()
		_ = app.Shutdown()
	}()

	// Start server
	addr := fmt.Sprintf("%s:%s", cfg.Host, cfg.Port)
	fmt.Printf("✅ Proxy running at http://localhost:%s\n", cfg.Port)

	if cfg.DiagnosticsEnabled {
		fmt.Printf("   Diagnostics: http://127.0.0.1:%s/debug/logs\n", cfg.Port)
		fmt.Printf("   Diagnostics DB: %s (retention %s)\n", cfg.DiagnosticsDBPath, cfg.DiagnosticsRetention)
		if cfg.DiagnosticsCaptureContent {
			fmt.Printf("   Diagnostics content capture: enabled (retention %s; local sensitive data)\n", cfg.DiagnosticsContentRetention)
		} else {
			fmt.Println("   Diagnostics content capture: disabled (redacted diagnostics only)")
		}
	}

	if cfg.PassthroughMode {
		fmt.Printf("   Mode: PASSTHROUGH (direct to Anthropic API)\n")
	} else {
		fmt.Printf("   Mode: Conversion (via %s)\n", cfg.OpenAIBaseURL)
		fmt.Printf("   Model Routing: %s\n", getRoutingMode(cfg))

		// Show actual model mappings
		if cfg.OpusModel != "" || cfg.SonnetModel != "" || cfg.HaikuModel != "" {
			fmt.Printf("   Models:\n")
			if cfg.OpusModel != "" {
				fmt.Printf("     - Opus   → %s\n", cfg.OpusModel)
			}
			if cfg.SonnetModel != "" {
				fmt.Printf("     - Sonnet → %s\n", cfg.SonnetModel)
			}
			if cfg.HaikuModel != "" {
				fmt.Printf("     - Haiku  → %s\n", cfg.HaikuModel)
			}
		}
	}

	return app.Listen(addr)
}

func getRoutingMode(cfg *config.Config) string {
	if cfg.OpusModel != "" || cfg.SonnetModel != "" || cfg.HaikuModel != "" {
		return "custom (env overrides)"
	}
	return "pattern-based"
}

func getOpusModel(cfg *config.Config) string {
	if cfg.OpusModel != "" {
		return cfg.OpusModel
	}
	return converter.DefaultOpusModel + " (pattern-based)"
}

func getSonnetModel(cfg *config.Config) string {
	if cfg.SonnetModel != "" {
		return cfg.SonnetModel
	}
	return "version-aware (pattern-based)"
}

func getHaikuModel(cfg *config.Config) string {
	if cfg.HaikuModel != "" {
		return cfg.HaikuModel
	}
	return converter.DefaultHaikuModel + " (pattern-based)"
}

func setupClaudeEndpoints(app *fiber.App, cfg *config.Config, store *diagnostics.Store) {
	// Messages endpoint - main Claude API
	app.Post("/v1/messages", func(c *fiber.Ctx) error {
		return handleMessages(c, cfg, store)
	})

	// Token counting endpoint
	app.Post("/v1/messages/count_tokens", func(c *fiber.Ctx) error {
		return handleCountTokens(c, cfg)
	})
}
