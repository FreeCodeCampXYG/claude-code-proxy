package server

import (
	_ "embed"
	"encoding/json"
	"strings"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/gofiber/fiber/v2"
)

//go:embed ui/prompts/page.html
var promptsPageHTML string

func setupPromptsEndpoints(app *fiber.App, cfg *config.Config) {
	options := newLocalPageOptions("/prompts", "X-Prompts-Token", "prompts token required")
	group := setupLocalPageGroup(app, options)
	handler := func(c *fiber.Ctx) error { return promptsHTML(c, options.Token, cfg) }
	group.Get("", handler)
	group.Get("/", handler)
	group.Get("/status", func(c *fiber.Ctx) error { return c.JSON(promptsStatus(cfg)) })
}

func promptsHTML(c *fiber.Ctx, token string, cfg *config.Config) error {
	page := promptsPageHTML
	page = strings.Replace(page, localPageTokenPlaceholder, "window.__localPageToken="+string(mustMarshalJSON(token))+";", 1)
	page = strings.ReplaceAll(page, "__PROMPTS_BOOTSTRAP__", mustPromptsJSON(promptsStatus(cfg)))
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.SendString(page)
}

func promptsStatus(cfg *config.Config) fiber.Map {
	return fiber.Map{
		"enabled": false,
		"retention": "168h",
		"storage": "diagnostics sqlite schema pending",
		"routes": []string{"GET /prompts", "GET /prompts/:id", "DELETE /prompts/:id", "POST /prompts/export"},
	}
}

func mustPromptsJSON(value interface{}) string {
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded)
}
