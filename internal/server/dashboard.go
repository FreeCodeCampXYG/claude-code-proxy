package server

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/diagnostics"
	"github.com/gofiber/fiber/v2"
)

//go:embed ui/dashboard/page.html
var dashboardPageHTML string

func setupDashboardEndpoints(app *fiber.App, cfg *config.Config, store *diagnostics.Store) {
	options := newLocalPageOptions("/", "X-Local-Page-Token", "dashboard token required")
	app.Get("/", requireLoopback, localPageSecurity(options), func(c *fiber.Ctx) error {
		return embeddedHTMLWithToken(c, dashboardHTML(cfg, store != nil), options.Token, localPageTokenPlaceholder)
	})
	app.Get("/status", func(c *fiber.Ctx) error {
		return c.JSON(rootStatusPayload(cfg, store != nil))
	})
}

func rootStatusPayload(cfg *config.Config, diagnosticsEnabled bool) fiber.Map {
	if cfg == nil {
		cfg = &config.Config{}
	}
	endpoints := fiber.Map{
		"health":         "/health",
		"status":         "/status",
		"dashboard":      "/",
		"messages":       "/v1/messages",
		"count_tokens":   "/v1/messages/count_tokens",
		"monitor":        "/monitor",
		"proxy_settings": "/settings/proxy",
		"prompts":        "/prompts",
		"playground":     "/playground",
	}
	if diagnosticsEnabled {
		endpoints["diagnostics"] = "/debug/logs"
	}
	return fiber.Map{
		"message": "Claude Code Proxy",
		"version": ProxyVersion,
		"status":  "running",
		"config": fiber.Map{
			"openai_base_url": safeBaseURL(cfg.OpenAIBaseURL),
			"routing_mode":    getRoutingMode(cfg),
			"opus_model":      getOpusModel(cfg),
			"sonnet_model":    getSonnetModel(cfg),
			"haiku_model":     getHaikuModel(cfg),
		},
		"diagnostics_enabled": diagnosticsEnabled,
		"endpoints":           endpoints,
	}
}

func dashboardHTML(cfg *config.Config, diagnosticsEnabled bool) string {
	cards := []struct {
		Title string
		Href  string
		Desc  string
	}{
		{"代理设置", "/settings/proxy", "配置上游代理、运行时覆盖与连通性测试"},
		{"Prompt 存档", "/prompts", "查看、检索、导出经过代理的提示词归档"},
		{"流量监控", "/monitor", "实时观察请求、连接与模型消耗"},
		{"API 调用台", "/playground", "一次性任务调用、OCR、图片与模板工作台"},
	}
	if diagnosticsEnabled {
		cards = append(cards, struct {
			Title string
			Href  string
			Desc  string
		}{"诊断日志", "/debug/logs", "查看诊断事件、分析图表与隐私保护内容快照"})
	}
	cardsHTML := ""
	for _, card := range cards {
		cardsHTML += fmt.Sprintf(`<a class="card" href="%s" target="_blank" rel="noopener noreferrer"><strong>%s</strong><span>%s</span></a>`, card.Href, card.Title, card.Desc)
	}
	statusJSON := rootStatusPayload(cfg, diagnosticsEnabled)
	statusText := fmt.Sprintf("版本：%s · 上游：%s · 路由：%s", ProxyVersion, safeBaseURL(cfg.OpenAIBaseURL), getRoutingMode(cfg))
	page := dashboardPageHTML
	page = strings.ReplaceAll(page, "__DASHBOARD_TITLE__", "Claude Code Proxy 控制台")
	page = strings.ReplaceAll(page, "__DASHBOARD_STATUS__", statusText)
	page = strings.ReplaceAll(page, "__DASHBOARD_CARDS__", cardsHTML)
	page = strings.ReplaceAll(page, "__DASHBOARD_JSON__", mustJSON(statusJSON))
	return page
}

func mustJSON(value interface{}) string {
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded)
}
