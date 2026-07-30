package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/gofiber/fiber/v2"
)

const (
	playgroundMaxImagePromptBytes = 4096
	playgroundMaxImageArtifactBytes = 8 * 1024 * 1024
	playgroundMaxImageResponseBytes = 18 * 1024 * 1024
)

type playgroundImageGenerationRequest struct {
	Prompt string `json:"prompt"`
	Size string `json:"size"`
	Count int `json:"count"`
}

type playgroundImageArtifact struct {
	B64 string `json:"b64"`
	MIMEType string `json:"mime_type"`
	Bytes int `json:"bytes"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

type imageGenerationUpstreamResponse struct {
	Data []struct { B64JSON string `json:"b64_json"`; RevisedPrompt string `json:"revised_prompt"` } `json:"data"`
}

func playgroundImageGeneration(c *fiber.Ctx, cfg *config.Config) error {
	if cfg == nil || cfg.ImageAPIURL == "" || cfg.ImageAPIKey == "" || cfg.ImageModel == "" { return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error":"图片生成未配置；需同时设置 IMAGE_API_URL、IMAGE_API_KEY、IMAGE_MODEL","code":"image_not_configured"}) }
	var req playgroundImageGenerationRequest
	if err := c.BodyParser(&req); err != nil { return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error":err.Error(),"code":"invalid_request"}) }
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" || len([]byte(req.Prompt)) > playgroundMaxImagePromptBytes { return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error":"prompt is required and must not exceed 4096 bytes","code":"prompt_invalid"}) }
	if req.Size == "" { req.Size="1024x1024" }
	if req.Size!="1024x1024" && req.Size!="1792x1024" && req.Size!="1024x1792" { return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error":"unsupported image size","code":"size_invalid"}) }
	if req.Count == 0 { req.Count=1 }
	if req.Count<1 || req.Count>2 { return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error":"count must be between 1 and 2","code":"count_invalid"}) }
	payload := fiber.Map{"model":cfg.ImageModel,"prompt":req.Prompt,"size":req.Size,"n":req.Count,"response_format":"b64_json"}
	body, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(c.UserContext(), http.MethodPost, cfg.ImageGenerationsURL(), bytes.NewReader(body))
	if err != nil { return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error":"invalid image endpoint","code":"image_endpoint_invalid"}) }
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+cfg.ImageAPIKey)
	request.Header.Set("X-Request-ID", newRequestID())
	client, err := playgroundImageClient(cfg)
	if err != nil { return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error":"image transport unavailable","code":"image_transport_failed"}) }
	response, err := client.Do(request)
	if err != nil { return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error":"image generation request failed","code":"image_upstream_failed"}) }
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, playgroundMaxImageResponseBytes+1))
	if err != nil || len(responseBody)>playgroundMaxImageResponseBytes { return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error":"image response is invalid or too large","code":"image_response_invalid"}) }
	if response.StatusCode < 200 || response.StatusCode >= 300 { return c.Status(imageStatus(response.StatusCode)).JSON(fiber.Map{"error":"image provider rejected the request","code":"image_upstream_rejected","retryable":response.StatusCode==429 || response.StatusCode>=500}) }
	var upstream imageGenerationUpstreamResponse
	if err := json.Unmarshal(responseBody, &upstream); err != nil || len(upstream.Data)==0 { return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error":"image provider returned no image artifacts","code":"image_response_invalid"}) }
	artifacts := make([]playgroundImageArtifact,0,len(upstream.Data))
	totalArtifactBytes := 0
	for _, item := range upstream.Data {
		decoded, err := base64.StdEncoding.DecodeString(item.B64JSON)
		if err != nil || len(decoded)==0 || len(decoded)>playgroundMaxImageArtifactBytes { return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error":"image artifact is invalid or too large","code":"image_artifact_invalid"}) }
		totalArtifactBytes += len(decoded)
		if totalArtifactBytes > playgroundMaxImageResponseBytes { return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error":"total image artifacts are too large","code":"image_artifact_invalid"}) }
		mime:=http.DetectContentType(decoded)
		if !strings.HasPrefix(mime,"image/") { return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error":"image artifact has unsupported type","code":"image_artifact_invalid"}) }
		artifacts=append(artifacts, playgroundImageArtifact{B64:item.B64JSON,MIMEType:mime,Bytes:len(decoded),RevisedPrompt:item.RevisedPrompt})
	}
	return c.JSON(fiber.Map{"state":"completed","model":cfg.ImageModel,"artifacts":artifacts,"message":"图片生成完成"})
}

func playgroundImageClient(cfg *config.Config) (*http.Client,error) { transport,err:=upstreamTransport(cfg); if err!=nil{return nil,err}; return &http.Client{Transport:transport,Timeout:120*time.Second},nil }
func imageStatus(status int) int { if status==400||status==401||status==403||status==413||status==415||status==422{return fiber.StatusUnprocessableEntity}; if status==429{return fiber.StatusTooManyRequests}; return fiber.StatusBadGateway }
