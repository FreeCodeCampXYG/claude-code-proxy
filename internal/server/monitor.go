package server

import (
	_ "embed"
	"encoding/json"
	"strings"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/monitor"
	"github.com/gofiber/fiber/v2"
)

//go:embed ui/monitor/page.html
var monitorPageHTML string

func setupMonitorEndpoints(app *fiber.App, cfg *config.Config, stats *monitor.Stats) {
	options := newLocalPageOptions("/monitor", "X-Monitor-Token", "monitor token required")
	group := setupLocalPageGroup(app, options)
	group.Get("", func(c *fiber.Ctx) error { return monitorHTML(c, options.Token, cfg, stats) })
	group.Get("/", func(c *fiber.Ctx) error { return monitorHTML(c, options.Token, cfg, stats) })
	group.Get("/snapshot", func(c *fiber.Ctx) error {
		if stats == nil {
			return c.JSON(monitor.Snapshot{})
		}
		return c.JSON(stats.Snapshot())
	})
}

func monitorHTML(c *fiber.Ctx, token string, cfg *config.Config, stats *monitor.Stats) error {
	if cfg == nil {
		cfg = &config.Config{}
	}
	snapshot := monitor.Snapshot{}
	if stats != nil {
		snapshot = stats.Snapshot()
	}
	page := monitorPageHTML
	page = strings.Replace(page, localPageTokenPlaceholder, "window.__localPageToken="+string(mustMarshalJSON(token))+";", 1)
	page = strings.ReplaceAll(page, "__MONITOR_BOOTSTRAP__", mustMonitorJSON(map[string]interface{}{
		"provider": string(cfg.DetectProvider()),
		"upstream": safeBaseURL(cfg.OpenAIBaseURL),
		"snapshot": snapshot,
	}))
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.SendString(page)
}

func mustMonitorJSON(value interface{}) string {
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded)
}
