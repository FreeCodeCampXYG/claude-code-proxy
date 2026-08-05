package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/diagnostics"
	"github.com/claude-code-proxy/proxy/internal/promptarchive"
	"github.com/gofiber/fiber/v2"
)

//go:embed ui/prompts/page.html
var promptsPageHTML string

func setupPromptsEndpoints(app *fiber.App, cfg *config.Config, store *diagnostics.Store, archive *promptarchive.Store) {
	options := newLocalPageOptions("/prompts", "X-Prompts-Token", "prompts token required")
	group := setupLocalPageGroup(app, options)
	handler := func(c *fiber.Ctx) error { return promptsHTML(c, options.Token, cfg, store, archive) }
	group.Get("", handler)
	group.Get("/", handler)
	group.Get("/status", func(c *fiber.Ctx) error { return c.JSON(promptsStatus(cfg, store, archive)) })
	group.Get("/records", func(c *fiber.Ctx) error { return promptsRecords(c, store, archive) })
	group.Get("/export", func(c *fiber.Ctx) error { return promptsExport(c, archive) })
	group.Delete("/:id", func(c *fiber.Ctx) error { return promptsDelete(c, archive) })
	group.Get("/:id", func(c *fiber.Ctx) error { return promptsDetail(c, store, archive) })
}

func promptsHTML(c *fiber.Ctx, token string, cfg *config.Config, store *diagnostics.Store, archive *promptarchive.Store) error {
	page := promptsPageHTML
	page = strings.Replace(page, localPageTokenPlaceholder, "window.__localPageToken="+string(mustMarshalJSON(token))+";", 1)
	page = strings.ReplaceAll(page, "__PROMPTS_BOOTSTRAP__", mustPromptsJSON(promptsStatus(cfg, store, archive)))
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.SendString(page)
}

func promptsStatus(cfg *config.Config, store *diagnostics.Store, archive *promptarchive.Store) fiber.Map {
	retention := "72h"
	if cfg != nil && archive != nil && cfg.PromptArchiveRetention > 0 {
		retention = cfg.PromptArchiveRetention.String()
	} else if cfg != nil && cfg.DiagnosticsRetention > 0 {
		retention = cfg.DiagnosticsRetention.String()
	}
	stats := promptarchive.Statistics{}
	if archive != nil {
		stats, _ = archive.Statistics(context.Background())
	}
	return fiber.Map{
		"enabled":         archive != nil || store != nil,
		"archive_enabled": archive != nil,
		"diagnostics_enabled": store != nil,
		"capture_content": cfg != nil && cfg.DiagnosticsCaptureContent,
		"retention":       retention,
		"storage":         "prompt_archive sqlite when PROMPT_ARCHIVE_ENABLED=true; diagnostics metadata fallback otherwise",
		"statistics":      stats,
		"note":            "独立 Prompt 存档开启后写入本机脱敏数据；仅显式会话标识的连续请求会安全增量压缩，详情和导出仍返回完整可读内容。",
		"routes":          []string{"GET /prompts/records", "GET /prompts/:id", "DELETE /prompts/:id", "GET /prompts/export"},
	}
}

func mustPromptsJSON(value interface{}) string {
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded)
}

type promptRecord struct {
	RequestID       string    `json:"request_id"`
	CreatedAt       time.Time `json:"created_at"`
	Model           string    `json:"model"`
	IncomingModel   string    `json:"incoming_model,omitempty"`
	MessageCount    int       `json:"message_count"`
	ToolCount       int       `json:"tool_count"`
	Summary         string    `json:"summary"`
	Status          string    `json:"status"`
	Streaming       bool      `json:"streaming"`
	InputTokens     int       `json:"input_tokens"`
	OutputTokens    int       `json:"output_tokens"`
	RequestBytes    int       `json:"request_bytes"`
	ContentCaptured bool      `json:"content_captured"`
}

func promptsRecords(c *fiber.Ctx, store *diagnostics.Store, archive *promptarchive.Store) error {
	if archive != nil {
		query, err := promptArchiveQuery(c)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		records, err := archive.Query(c.Context(), query)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		total, err := archive.Count(c.Context(), query)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"records": records, "total": total, "enabled": true, "source": "prompt_archive"})
	}
	if store == nil {
		return c.JSON(fiber.Map{"records": []promptRecord{}, "total": 0, "enabled": false, "source": "unavailable", "message": "Prompt 存档未开启；请设置 PROMPT_ARCHIVE_ENABLED=true 后重启。临时排障可用 -d 开启诊断元数据回退。"})
	}
	query, keyword, err := promptsQuery(c)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	query.Keyword = keyword
	events, err := store.Query(c.Context(), query)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	records := make([]promptRecord, 0, len(events))
	for _, event := range events {
		records = append(records, promptRecordFromSummary(event, false))
	}
	total, err := store.Count(c.Context(), query)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"records": records, "total": total, "enabled": true, "source": "diagnostics"})
}

func promptsDetail(c *fiber.Ctx, store *diagnostics.Store, archive *promptarchive.Store) error {
	id := strings.TrimSpace(c.Params("id"))
	if id == "" || id == "records" || id == "status" || id == "export" {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "prompt record not found"})
	}
	if archive != nil {
		record, err := archive.Detail(c.Context(), id)
		if err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "prompt record not found"})
		}
		return c.JSON(fiber.Map{"record": record, "source": "prompt_archive"})
	}
	if store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Prompt 存档未开启；请先开启 PROMPT_ARCHIVE_ENABLED 或 -d"})
	}
	event, err := store.Detail(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "prompt record not found"})
	}
	captures, _ := store.Content(c.Context(), diagnostics.ContentQuery{RequestID: id, Boundary: diagnostics.ContentBoundaryClaudeRequest})
	return c.JSON(fiber.Map{"record": promptRecordFromEvent(event, len(captures) > 0), "content": captures})
}

func promptArchiveQuery(c *fiber.Ctx) (promptarchive.Query, error) {
	limit, err := strconv.Atoi(c.Query("limit", "50"))
	if err != nil || limit < 1 || limit > 200 {
		return promptarchive.Query{}, fmt.Errorf("limit must be an integer between 1 and 200")
	}
	offset, err := strconv.Atoi(c.Query("offset", "0"))
	if err != nil || offset < 0 {
		return promptarchive.Query{}, fmt.Errorf("offset must be a non-negative integer")
	}
	since, err := strictRFC3339(c.Query("since"))
	if err != nil {
		return promptarchive.Query{}, fmt.Errorf("invalid since: %w", err)
	}
	until, err := strictRFC3339(c.Query("until"))
	if err != nil {
		return promptarchive.Query{}, fmt.Errorf("invalid until: %w", err)
	}
	if since.IsZero() && strings.TrimSpace(c.Query("range")) != "all" {
		since = time.Now().UTC().Add(-24 * time.Hour)
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return promptarchive.Query{}, fmt.Errorf("since must not be after until")
	}
	return promptarchive.Query{Model: strings.TrimSpace(c.Query("model")), Keyword: strings.TrimSpace(c.Query("q")), Since: since, Until: until, Limit: limit, Offset: offset}, nil
}

const promptExportMaxRecords = 1000

func promptsExport(c *fiber.Ctx, archive *promptarchive.Store) error {
	if archive == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "PROMPT_ARCHIVE_ENABLED is required for export"})
	}

	format := strings.ToLower(strings.TrimSpace(c.Query("format")))
	if format != "json" && format != "markdown" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "format must be json or markdown"})
	}
	query, err := promptArchiveQuery(c)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	records, err := promptArchiveExportRecords(c, archive, query)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	switch format {
	case "markdown":
		var b strings.Builder
		for _, record := range records {
			fmt.Fprintf(&b, "## %s\n- 时间：%s\n- 模型：%s\n- 摘要：%s\n- 消息数：%d\n- 工具数：%d\n\n", record.RequestID, record.CreatedAt.Format(time.RFC3339), record.Model, record.Summary, record.MessageCount, record.ToolCount)
			if record.PayloadState != "unavailable" && len(record.Payload) > 0 {
				fence := markdownCodeFence(record.Payload)
				fmt.Fprintf(&b, "%sjson\n%s\n%s\n\n", fence, record.Payload, fence)
			} else {
				b.WriteString("内容无法还原。\n\n")
			}
		}
		c.Set(fiber.HeaderContentType, "text/markdown; charset=utf-8")
		c.Set(fiber.HeaderContentDisposition, `attachment; filename="prompts-archive.md"`)
		return c.SendString(b.String())
	default:
		encoded, err := json.Marshal(fiber.Map{"records": records})
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "encode prompt archive export"})
		}
		c.Set(fiber.HeaderContentType, "application/json; charset=utf-8")
		c.Set(fiber.HeaderContentDisposition, `attachment; filename="prompts-archive.json"`)
		return c.Send(encoded)
	}
}

func markdownCodeFence(payload []byte) string {
	longest, current := 0, 0
	for _, char := range payload {
		if char == '`' {
			current++
			if current > longest { longest = current }
		} else { current = 0 }
	}
	return strings.Repeat("`", max(3, longest+1))
}

func promptArchiveExportRecords(c *fiber.Ctx, archive *promptarchive.Store, query promptarchive.Query) ([]promptarchive.Record, error) {
	query.Offset = 0 // Exports always start at the first matching record.
	query.Limit = 200
	records := make([]promptarchive.Record, 0, promptExportMaxRecords)
	for len(records) < promptExportMaxRecords {
		page, err := archive.Query(c.Context(), query)
		if err != nil {
			return nil, err
		}
		remaining := promptExportMaxRecords - len(records)
		if len(page) > remaining {
			page = page[:remaining]
		}
		for index, record := range page {
			detail, detailErr := archive.Detail(c.Context(), record.RequestID)
			if detailErr != nil {
				return nil, detailErr
			}
			page[index] = detail
		}
		records = append(records, page...)
		if len(page) < query.Limit || len(records) == promptExportMaxRecords {
			break
		}
		query.Offset += len(page)
	}
	return records, nil
}

func promptsDelete(c *fiber.Ctx, archive *promptarchive.Store) error {
	if archive == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "PROMPT_ARCHIVE_ENABLED is required for delete"})
	}
	deleted, err := archive.Delete(c.Context(), strings.TrimSpace(c.Params("id")))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	if !deleted {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "prompt record not found"})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func promptsQuery(c *fiber.Ctx) (diagnostics.Query, string, error) {
	limit, err := strconv.Atoi(c.Query("limit", "50"))
	if err != nil || limit < 1 || limit > 200 {
		return diagnostics.Query{}, "", fmt.Errorf("limit must be an integer between 1 and 200")
	}
	offset, err := strconv.Atoi(c.Query("offset", "0"))
	if err != nil || offset < 0 {
		return diagnostics.Query{}, "", fmt.Errorf("offset must be a non-negative integer")
	}
	since, err := strictRFC3339(c.Query("since"))
	if err != nil {
		return diagnostics.Query{}, "", fmt.Errorf("invalid since: %w", err)
	}
	until, err := strictRFC3339(c.Query("until"))
	if err != nil {
		return diagnostics.Query{}, "", fmt.Errorf("invalid until: %w", err)
	}
	if since.IsZero() && strings.TrimSpace(c.Query("range")) != "all" {
		since = time.Now().UTC().Add(-24 * time.Hour)
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return diagnostics.Query{}, "", fmt.Errorf("since must not be after until")
	}
	return diagnostics.Query{Model: strings.TrimSpace(c.Query("model")), Since: since, Until: until, Limit: limit, Offset: offset}, strings.ToLower(strings.TrimSpace(c.Query("q"))), nil
}

func promptRecordFromSummary(event diagnostics.EventSummary, captured bool) promptRecord {
	status := event.CompletionState
	if status == "" {
		status = "unknown"
	}
	return promptRecord{
		RequestID: event.RequestID, CreatedAt: event.CreatedAt, Model: event.Model, IncomingModel: event.IncomingModel,
		MessageCount: event.MessageCount, ToolCount: event.ToolCount, Summary: promptSummary(event.MessageCount, event.ToolCount, event.ClaudeRequestBytes),
		Status: status, Streaming: event.Streaming, InputTokens: event.InputTokens, OutputTokens: event.OutputTokens,
		RequestBytes: event.ClaudeRequestBytes, ContentCaptured: captured,
	}
}

func promptRecordFromEvent(event diagnostics.Event, captured bool) promptRecord {
	status := event.CompletionState
	if status == "" {
		status = "unknown"
	}
	return promptRecord{
		RequestID: event.RequestID, CreatedAt: event.CreatedAt, Model: event.Model, IncomingModel: event.IncomingModel,
		MessageCount: event.MessageCount, ToolCount: event.ToolCount, Summary: promptSummary(event.MessageCount, event.ToolCount, event.ClaudeRequestBytes),
		Status: status, Streaming: event.Streaming, InputTokens: event.InputTokens, OutputTokens: event.OutputTokens,
		RequestBytes: event.ClaudeRequestBytes, ContentCaptured: captured,
	}
}

func promptSummary(messages, tools, bytes int) string {
	parts := []string{fmt.Sprintf("%d 条消息", messages)}
	if tools > 0 {
		parts = append(parts, fmt.Sprintf("%d 个工具", tools))
	}
	if bytes > 0 {
		parts = append(parts, fmt.Sprintf("请求 %d bytes", bytes))
	}
	return strings.Join(parts, " · ")
}

func promptRecordMatches(record promptRecord, keyword string) bool {
	text := strings.ToLower(strings.Join([]string{record.RequestID, record.Model, record.IncomingModel, record.Summary, record.Status}, " "))
	return strings.Contains(text, keyword)
}
