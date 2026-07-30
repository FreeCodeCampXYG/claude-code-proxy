package server

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/pkg/models"
	"github.com/gofiber/fiber/v2"
)

const playgroundMaxOCRImageBytes = 10 * 1024 * 1024

func playgroundOCRUpload(c *fiber.Ctx, cfg *config.Config) error {
	file, err := c.FormFile("image")
	if err != nil { return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "image file is required", "code": "image_required"}) }
	if file.Size <= 0 || file.Size > playgroundMaxOCRImageBytes { return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "image size must be between 1 byte and 10 MiB", "code": "image_size_invalid"}) }
	opened, err := file.Open()
	if err != nil { return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "cannot read image", "code": "image_unreadable"}) }
	defer opened.Close()
	body, err := io.ReadAll(io.LimitReader(opened, playgroundMaxOCRImageBytes+1))
	if err != nil || len(body) == 0 || len(body) > playgroundMaxOCRImageBytes { return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "image size must be between 1 byte and 10 MiB", "code": "image_size_invalid"}) }
	mime := http.DetectContentType(body)
	if mime != "image/jpeg" && mime != "image/png" && mime != "image/webp" { return c.Status(fiber.StatusUnsupportedMediaType).JSON(fiber.Map{"error": "only JPEG, PNG, and WebP images are supported", "code": "image_type_unsupported"}) }
	prompt := strings.TrimSpace(c.FormValue("prompt"))
	if prompt == "" { prompt = "请识别图片中的所有文字，保持原始格式输出。" }
	model := ""
	if cfg != nil { model = strings.TrimSpace(cfg.OCRModel) }
	if model == "" { model = strings.TrimSpace(c.FormValue("model")) }
	if model == "" { model = playgroundDefaultModel(cfg) }
	maxTokens := 2048
	if raw := strings.TrimSpace(c.FormValue("max_tokens")); raw != "" { _, _ = fmt.Sscanf(raw, "%d", &maxTokens) }
	if maxTokens < 1 { maxTokens = 2048 }
	stream := false
	content := []map[string]interface{}{{"type":"text", "text":prompt}, {"type":"image_url", "image_url":map[string]string{"url":"data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(body)}}}
	req := &models.OpenAIRequest{Model:model, Messages:[]models.OpenAIMessage{{Role:"user", Content:content}}, Stream:&stream}
	if cfg != nil && cfg.ShouldUseMaxCompletionTokens(model) { req.MaxCompletionTokens=maxTokens } else { req.MaxTokens=maxTokens }
	applyPlaygroundProviderOptions(req, cfg)
	resp, err := callOpenAI(c.UserContext(), req, cfg, nil)
	if err != nil {
		status := fiber.StatusBadGateway; code := "ocr_upstream_failed"
		message := err.Error()
		if strings.Contains(message, "status 400") || strings.Contains(message, "status 415") || strings.Contains(message, "status 422") { status=fiber.StatusUnprocessableEntity; code="vision_input_unsupported"; message="当前 OCR 上游不接受该视觉输入或模型" }
		return c.Status(status).JSON(fiber.Map{"error":message,"code":code})
	}
	result := ""
	if len(resp.Choices)>0 { result, _ = resp.Choices[0].Message.Content.(string) }
	return c.JSON(fiber.Map{"task":"ocr", "content":result, "usage":resp.Usage, "model":model})
}

func playgroundDefaultModel(cfg *config.Config) string {
	if cfg != nil && strings.TrimSpace(cfg.SonnetModel) != "" { return strings.TrimSpace(cfg.SonnetModel) }
	return "gpt-5"
}
