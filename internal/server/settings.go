package server

import (
	_ "embed"
	"encoding/json"
	"os"
	"strings"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/gofiber/fiber/v2"
)

//go:embed ui/settings/page.html
var settingsPageHTML string

func setupProxySettingsEndpoints(app *fiber.App, cfg *config.Config) {
	options := newLocalPageOptions("/settings/proxy", "X-Proxy-Settings-Token", "proxy settings token required")
	group := setupLocalPageGroup(app, options)
	group.Get("", func(c *fiber.Ctx) error { return proxySettingsHTML(c, options.Token, cfg) })
	group.Get("/", func(c *fiber.Ctx) error { return proxySettingsHTML(c, options.Token, cfg) })
	group.Get("/config", func(c *fiber.Ctx) error { return c.JSON(proxySettingsResponse(cfg)) })
}

func proxySettingsResponse(cfg *config.Config) fiber.Map {
	if cfg == nil {
		cfg = &config.Config{}
	}
	runtimePath := ""
	runtimeStatus := "unavailable"
	runtimeExists := false
	runtimeConfig := config.ProxyConfig{}
	if cfg != nil && cfg.ProxyRuntime != nil {
		runtimePath = cfg.ProxyRuntime.Path()
		runtimeConfig = config.MaskedProxyConfig(cfg.ProxyRuntime.Snapshot())
		_, err := os.Stat(runtimePath)
		runtimeExists = err == nil
		if os.IsNotExist(err) {
			runtimeStatus = "using_defaults_file_missing"
		} else if err != nil {
			runtimeStatus = "stat_error"
		} else {
			runtimeStatus = "loaded"
		}
	}
	return fiber.Map{
		"env": fiber.Map{
			"enabled": cfg.UpstreamProxyEnabled,
			"type":    cfg.UpstreamProxyType,
			"address": cfg.UpstreamProxyAddr,
			"user":    cfg.UpstreamProxyUser,
			"password": maskedValue(cfg.UpstreamProxyPass),
		},
		"runtime": fiber.Map{
			"path":   runtimePath,
			"exists": runtimeExists,
			"status": runtimeStatus,
			"config": runtimeConfig,
		},
	}
}

func proxySettingsHTML(c *fiber.Ctx, token string, cfg *config.Config) error {
	page := settingsPageHTML
	page = strings.Replace(page, localPageTokenPlaceholder, "window.__localPageToken="+string(mustMarshalJSON(token))+";", 1)
	page = strings.ReplaceAll(page, "__SETTINGS_BOOTSTRAP__", mustSettingsJSON(proxySettingsResponse(cfg)))
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.SendString(page)
}

func mustSettingsJSON(value interface{}) string {
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded)
}

func maskedValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return "********"
}
