package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v2"
)

const localPageTokenPlaceholder = "window.__localPageToken=window.__localPageToken||'';"

type localPageOptions struct {
	Path       string
	IndexPaths map[string]bool
	Token      string
	TokenHeader string
	TokenError string
}

func newLocalPageOptions(path, tokenHeader, tokenError string) localPageOptions {
	base := strings.TrimRight(path, "/")
	return localPageOptions{
		Path:        base,
		IndexPaths:  map[string]bool{base: true, base + "/": true},
		Token:       newRequestID(),
		TokenHeader: tokenHeader,
		TokenError:  tokenError,
	}
}

func setupLocalPageGroup(app *fiber.App, options localPageOptions) fiber.Router {
	return app.Group(options.Path, requireLoopback, localPageSecurity(options))
}

func localPageSecurity(options localPageOptions) fiber.Handler {
	return func(c *fiber.Ctx) error {
		c.Set("Cache-Control", "no-store")
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("Referrer-Policy", "no-referrer")
		c.Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
		origin := strings.TrimSpace(c.Get("Origin"))
		if origin != "" && !isLoopbackOrigin(origin) {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": fmt.Sprintf("%s origin must be loopback", strings.TrimPrefix(options.Path, "/"))})
		}
		if !options.IndexPaths[c.Path()] && c.Get(options.TokenHeader) != options.Token {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": options.TokenError})
		}
		return c.Next()
	}
}

func embeddedHTMLWithToken(c *fiber.Ctx, page, token, placeholder string) error {
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	if token == "" || placeholder == "" {
		return c.SendString(page)
	}
	target := string(mustMarshalJSON(token))
	if index := strings.Index(placeholder, "="); index >= 0 {
		target = placeholder[:index+1] + string(mustMarshalJSON(token)) + ";"
	}
	page = strings.Replace(page, placeholder, target, 1)
	return c.SendString(page)
}

func mustMarshalJSON(value string) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

func localUIRoutePrefix(path string) bool {
	for _, prefix := range []string{"/debug/logs", "/settings", "/prompts", "/monitor", "/playground"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return path == "/" || strings.HasPrefix(path, "/ui") || strings.HasPrefix(path, "/dashboard") || strings.HasPrefix(path, "/status")
}
