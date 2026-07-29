package server

import (
	_ "embed"
	"strings"

	"github.com/gofiber/fiber/v2"
)

//go:embed ui/placeholder/page.html
var placeholderPageHTML string

type placeholderPage struct {
	Path        string
	TokenHeader string
	Title       string
	Desc        string
}

func setupPlaceholderUIEndpoints(app *fiber.App) {
	pages := []placeholderPage{
		{Path: "/prompts", TokenHeader: "X-Prompts-Token", Title: "Prompt 存档", Desc: "提示词归档、检索与导出页面正在建设中。当前版本先提供本机占位页，避免控制台入口返回 Cannot GET。"},
		{Path: "/playground", TokenHeader: "X-Playground-Token", Title: "API 调用台", Desc: "一次性任务调用、OCR、图片与模板工作台正在建设中。当前版本先提供本机占位页，避免控制台入口返回 Cannot GET。"},
	}
	for _, page := range pages {
		setupPlaceholderPage(app, page)
	}
}

func setupPlaceholderPage(app *fiber.App, page placeholderPage) {
	options := newLocalPageOptions(page.Path, page.TokenHeader, strings.TrimPrefix(page.Path, "/")+" token required")
	group := setupLocalPageGroup(app, options)
	handler := func(c *fiber.Ctx) error {
		return embeddedHTMLWithToken(c, placeholderHTML(page), options.Token, localPageTokenPlaceholder)
	}
	group.Get("", handler)
	group.Get("/", handler)
}

func placeholderHTML(page placeholderPage) string {
	html := placeholderPageHTML
	html = strings.ReplaceAll(html, "__LOCAL_PAGE_TITLE__", page.Title)
	html = strings.ReplaceAll(html, "__LOCAL_PAGE_DESC__", page.Desc)
	return html
}
