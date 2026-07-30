package server

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/converter"
	"github.com/claude-code-proxy/proxy/pkg/models"
	"github.com/gofiber/fiber/v2"
)

//go:embed ui/playground/page.html
var playgroundPageHTML string

func setupPlaygroundEndpoints(app *fiber.App, cfg *config.Config) {
	options := newLocalPageOptions("/playground", "X-Playground-Token", "playground token required")
	group := setupLocalPageGroup(app, options)
	handler := func(c *fiber.Ctx) error { return playgroundHTML(c, options.Token, cfg) }
	group.Get("", handler)
	group.Get("/", handler)
	group.Get("/config", func(c *fiber.Ctx) error { return c.JSON(playgroundConfig(cfg)) })
	group.Post("/chat/stream", func(c *fiber.Ctx) error { return playgroundChatStream(c, cfg) })
	group.Post("/ocr", func(c *fiber.Ctx) error { return playgroundTextTask(c, cfg, "ocr") })
	group.Post("/ppt", func(c *fiber.Ctx) error { return playgroundTextTask(c, cfg, "ppt") })
	group.Post("/image", func(c *fiber.Ctx) error { return playgroundTextTask(c, cfg, "image") })
}

func playgroundHTML(c *fiber.Ctx, token string, cfg *config.Config) error {
	page := playgroundPageHTML
	page = strings.Replace(page, localPageTokenPlaceholder, "window.__localPageToken="+string(mustMarshalJSON(token))+";", 1)
	page = strings.ReplaceAll(page, "__PLAYGROUND_BOOTSTRAP__", mustPlaygroundJSON(playgroundConfig(cfg)))
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.SendString(page)
}

func playgroundConfig(cfg *config.Config) fiber.Map {
	if cfg == nil {
		cfg = &config.Config{}
	}
	models := []string{}
	for _, model := range []string{cfg.SonnetModel, cfg.OpusModel, cfg.HaikuModel} {
		model = strings.TrimSpace(model)
		if model != "" && !containsString(models, model) {
			models = append(models, model)
		}
	}
	if len(models) == 0 {
		models = append(models, converter.DefaultSonnetModel, converter.DefaultOpusModel, converter.DefaultHaikuModel)
	}
	return fiber.Map{
		"provider": string(cfg.DetectProvider()),
		"upstream": safeBaseURL(cfg.OpenAIBaseURL),
		"models":   models,
		"limits": fiber.Map{
			"max_rounds": 2,
		},
		"image": fiber.Map{
			"configured": strings.TrimSpace(cfg.OpenAIBaseURL) != "",
		},
	}
}

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func mustPlaygroundJSON(value interface{}) string {
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded)
}

type playgroundChatRequest struct {
	Model       string   `json:"model"`
	System      string   `json:"system"`
	Prompt      string   `json:"prompt"`
	Image       string   `json:"image,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
}

func playgroundChatStream(c *fiber.Ctx, cfg *config.Config) error {
	var req playgroundChatRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "prompt is required"})
	}
	openAIReq := playgroundOpenAIRequest(req, cfg)
	c.Set(fiber.HeaderContentType, "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")
	requestContext := c.UserContext()
	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		streamContext, cancel := context.WithCancel(requestContext)
		defer cancel()
		resp, err := callOpenAIStream(streamContext, openAIReq, cfg, nil)
		if err != nil {
			_ = writeSSEEvent(w, "error", fiber.Map{"error": fmt.Sprintf("upstream request failed: %v", err)})
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(w, resp.Body)
		_ = w.Flush()
	})
	return nil
}

func playgroundTextTask(c *fiber.Ctx, cfg *config.Config, task string) error {
	var req playgroundChatRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "prompt is required"})
	}
	switch task {
	case "ocr":
		if strings.TrimSpace(req.System) == "" {
			req.System = "你是 OCR 识图助手。识别图片或用户输入中的文字，保持原始格式，无法识别时说明原因。"
		}
		if strings.TrimSpace(req.Image) != "" {
			req.Prompt = req.Prompt + "\n\n[图片已由页面上传为 data URL；当前上游若支持多模态，可在后续版本直接传图。]"
		}
	case "ppt":
		if strings.TrimSpace(req.System) == "" {
			req.System = "你是专业 PPT 策划助手。请输出 JSON，包含 title、audience、style、slides 数组；每页包含 title、bullets、speaker_notes。"
		}
	case "image":
		if strings.TrimSpace(req.System) == "" {
			req.System = "你是图片创意提示词助手。请把用户需求整理成可直接用于图片生成模型的中英双语 prompt，并给出尺寸、风格、负面提示词建议。"
		}
	}
	openAIReq := playgroundOpenAIRequest(req, cfg)
	stream := false
	openAIReq.Stream = &stream
	resp, err := callOpenAI(c.UserContext(), openAIReq, cfg, nil)
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
	}
	content := ""
	if len(resp.Choices) > 0 {
		if value, ok := resp.Choices[0].Message.Content.(string); ok {
			content = value
		}
	}
	return c.JSON(fiber.Map{"task": task, "content": content, "usage": resp.Usage})
}

func playgroundOpenAIRequest(req playgroundChatRequest, cfg *config.Config) *models.OpenAIRequest {
	stream := true
	model := strings.TrimSpace(req.Model)
	if model == "" && cfg != nil {
		model = strings.TrimSpace(cfg.SonnetModel)
	}
	if model == "" {
		model = converter.DefaultSonnetModel
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 2048
	}
	messages := []models.OpenAIMessage{}
	if strings.TrimSpace(req.System) != "" {
		messages = append(messages, models.OpenAIMessage{Role: "system", Content: strings.TrimSpace(req.System)})
	}
	messages = append(messages, models.OpenAIMessage{Role: "user", Content: req.Prompt})
	openAIReq := &models.OpenAIRequest{Model: model, Messages: messages, Temperature: req.Temperature, Stream: &stream}
	if cfg != nil && cfg.ShouldUseMaxCompletionTokens(model) {
		openAIReq.MaxCompletionTokens = maxTokens
	} else {
		openAIReq.MaxTokens = maxTokens
	}
	applyPlaygroundProviderOptions(openAIReq, cfg)
	return openAIReq
}

func applyPlaygroundProviderOptions(req *models.OpenAIRequest, cfg *config.Config) {
	if cfg == nil || req == nil {
		return
	}
	switch cfg.DetectProvider() {
	case config.ProviderOpenRouter:
		req.StreamOptions = map[string]interface{}{"include_usage": true}
		req.Usage = map[string]interface{}{"include": true}
		req.Reasoning = map[string]interface{}{"enabled": true}
	case config.ProviderOpenAI:
		req.StreamOptions = map[string]interface{}{"include_usage": true}
		req.ReasoningEffort = converter.DefaultNewAPIEffort
	case config.ProviderNewAPI:
		req.StreamOptions = map[string]interface{}{"include_usage": true}
		req.ReasoningEffort = converter.DefaultNewAPIEffort
	}
}
