package server

import (
	"fmt"
	"strings"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/ppt"
	"github.com/gofiber/fiber/v2"
)

const playgroundPPTSchemaVersion = ppt.SchemaVersion
const playgroundMaxPPTResponseBytes = 48 * 1024

type playgroundPPTRequest struct { Model string `json:"model"`; Prompt string `json:"prompt"`; System string `json:"system"`; MaxTokens int `json:"max_tokens"` }
type playgroundPPTExportRequest struct { Deck ppt.Deck `json:"deck"` }

func playgroundPPT(c *fiber.Ctx, cfg *config.Config) error {
	var input playgroundPPTRequest
	if err:=c.BodyParser(&input); err!=nil{return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error":err.Error()})}
	input.Prompt=strings.TrimSpace(input.Prompt)
	if input.Prompt=="" { return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error":"prompt is required"}) }
	if len([]byte(input.Prompt))>8192{return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error":"prompt is too long"})}
	model := ""
	if cfg != nil { model = strings.TrimSpace(cfg.PPTModel) }
	if model == "" { model=strings.TrimSpace(input.Model) }; if model=="" {model=playgroundDefaultModel(cfg)}
	system:=pptSystemPrompt(); if strings.TrimSpace(input.System)!="" {system += "\n\n附加创作要求（不能改变 JSON 格式）：\n"+strings.TrimSpace(input.System)}
	content,usage,err:=playgroundPPTCompletion(c, cfg, model, system, input.Prompt, input.MaxTokens)
	if err!=nil{return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error":err.Error()})}
	deck,validationErr:=ppt.Decode([]byte(content)); repaired:=false
	if validationErr!=nil {
		repairPrompt:=fmt.Sprintf("请修复以下 PPT JSON，使其符合固定 schema。只返回一个 JSON 对象。验证错误：%s\n原输出：%s",validationErr,boundPPTText(content))
		content,usage,err=playgroundPPTCompletion(c,cfg,model,pptSystemPrompt(),repairPrompt,input.MaxTokens)
		if err!=nil{return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error":err.Error()})}
		deck,validationErr=ppt.Decode([]byte(content)); repaired=true
	}
	if validationErr!=nil{return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error":"上游返回的 PPT 结构无效："+validationErr.Error(),"code":"ppt_schema_invalid"})}
	return c.JSON(fiber.Map{"deck":deck,"usage":usage,"repaired":repaired})
}

func playgroundPPTCompletion(c *fiber.Ctx,cfg *config.Config,model,system,prompt string,maxTokens int)(string,interface{},error){
	req:=playgroundOpenAIRequest(playgroundChatRequest{Model:model,System:system,Prompt:prompt,MaxTokens:maxTokens},cfg); stream:=false; req.Stream=&stream
	resp,err:=callOpenAI(c.UserContext(),req,cfg,nil);if err!=nil{return "",nil,err};if len(resp.Choices)==0{return "",nil,fmt.Errorf("PPT 上游未返回结果")};content,ok:=resp.Choices[0].Message.Content.(string);if !ok{return "",nil,fmt.Errorf("PPT 上游返回非文本结果")};if len(content)>playgroundMaxPPTResponseBytes{return "",nil,fmt.Errorf("PPT 上游结果过大")};return content,resp.Usage,nil
}

func pptSystemPrompt()string{return `你是 PPT 结构生成器。必须只输出一个 JSON 对象，不能使用 Markdown code fence 或任何额外说明。schema_version 必须是 "ppt-outline/v1"。对象字段仅允许 schema_version,title,subtitle,audience,language,theme,slides；theme 仅允许 name,tone,aspect_ratio；每个 slide 仅允许 id,layout,title,subtitle,bullets,left_column,right_column,speaker_notes。layout 只能是 title,bullets,two-column,quote,timeline,comparison,closing。two-column/comparison 必须同时有 left_column 和 right_column。`}
func boundPPTText(value string)string{if len(value)>playgroundMaxPPTResponseBytes{return value[:playgroundMaxPPTResponseBytes]};return value}

func playgroundPPTExport(c *fiber.Ctx)error{var input playgroundPPTExportRequest;if err:=c.BodyParser(&input);err!=nil{return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error":err.Error()})};if err:=ppt.Validate(input.Deck);err!=nil{return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error":"PPT schema invalid: "+err.Error()})};format:=strings.ToLower(strings.TrimSpace(c.Query("format")));switch format{case "markdown","md":c.Set(fiber.HeaderContentType,"text/markdown; charset=utf-8");c.Set("Content-Disposition","attachment; filename=ppt-outline.md");return c.SendString(ppt.Markdown(input.Deck));case "html":content,err:=ppt.HTML(input.Deck);if err!=nil{return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error":"render PPT HTML: "+err.Error()})};c.Set(fiber.HeaderContentType,"text/html; charset=utf-8");c.Set("Content-Disposition","attachment; filename=ppt-outline.html");return c.SendString(content);default:return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error":"format must be markdown or html"})}}
