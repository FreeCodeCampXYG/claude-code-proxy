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
	"syscall"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/converter"
	"github.com/claude-code-proxy/proxy/internal/daemon"
	"github.com/claude-code-proxy/proxy/internal/diagnostics"
	"github.com/claude-code-proxy/proxy/internal/monitor"
	"github.com/claude-code-proxy/proxy/internal/promptarchive"
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
	var promptStore *promptarchive.Store
	stats := monitor.NewStats(200)
	if cfg.DiagnosticsEnabled {
		var err error
		diagnosticsStore, err = diagnostics.Open(cfg.DiagnosticsDBPath, diagnostics.StoreOptions{
			Retention:        cfg.DiagnosticsRetention,
			ContentRetention: cfg.DiagnosticsContentRetention,
			CaptureContent:   cfg.DiagnosticsCaptureContent,
			BusyTimeout:      cfg.DiagnosticsBusyTimeout,
			CloseTimeout:     cfg.DiagnosticsCloseTimeout,
		})
		if err != nil {
			return fmt.Errorf("initialize diagnostics store at %s: %w; preserve this database, then inspect its schema/foreign-key integrity or configure a new DIAGNOSTICS_DB_PATH", cfg.DiagnosticsDBPath, err)
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
	if cfg.PromptArchiveEnabled {
		var err error
		promptStore, err = promptarchive.Open(cfg.PromptArchiveDBPath, promptarchive.Options{Retention: cfg.PromptArchiveRetention, BusyTimeout: cfg.DiagnosticsBusyTimeout})
		if err != nil {
			return fmt.Errorf("initialize prompt archive store: %w", err)
		}
		defer func() {
			if err := promptStore.Close(); err != nil {
				fmt.Printf("[WARN] Failed to close prompt archive store: %v\n", err)
			}
		}()
	}

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ServerHeader:          "Claude-Code-Proxy",
		AppName:               "Claude Code Proxy v" + ProxyVersion,
		// Playground OCR accepts a 10 MiB image plus multipart framing.
		BodyLimit: 11 * 1024 * 1024,
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
		if localUIRoutePrefix(c.Path()) {
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
	app.Get("/status", func(c *fiber.Ctx) error {
		return c.JSON(rootStatusPayload(cfg, diagnosticsStore != nil))
	})

	setupDashboardEndpoints(app, cfg, diagnosticsStore)
	setupMonitorEndpoints(app, cfg, stats)
	setupProxySettingsEndpoints(app, cfg)
	setupPromptsEndpoints(app, cfg, diagnosticsStore, promptStore)
	setupPlaygroundEndpoints(app, cfg)

	// Claude API endpoints
	setupClaudeEndpointsWithMonitor(app, cfg, diagnosticsStore, stats, promptStore)
	setupDiagnosticsEndpoints(app, diagnosticsStore, cfg)

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

	if cfg.PromptArchiveEnabled {
		fmt.Printf("   Prompt archive: http://127.0.0.1:%s/prompts\n", cfg.Port)
		fmt.Printf("   Prompt archive DB: %s (retention %s)\n", cfg.PromptArchiveDBPath, cfg.PromptArchiveRetention)
	}

	if cfg.PassthroughMode {
		fmt.Printf("   Mode: PASSTHROUGH (direct to Anthropic API)\n")
	} else {
		fmt.Printf("   Mode: Conversion (via %s)\n", safeBaseURL(cfg.OpenAIBaseURL))
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
	setupClaudeEndpointsWithMonitor(app, cfg, store, nil, nil)
}

func setupClaudeEndpointsWithMonitor(app *fiber.App, cfg *config.Config, store *diagnostics.Store, stats *monitor.Stats, promptStore *promptarchive.Store) {
	// Messages endpoint - main Claude API
	app.Post("/v1/messages", func(c *fiber.Ctx) error {
		return handleMessages(c, cfg, store, stats, promptStore)
	})

	// Token counting endpoint
	app.Post("/v1/messages/count_tokens", func(c *fiber.Ctx) error {
		return handleCountTokens(c, cfg)
	})
}
