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
