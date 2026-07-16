package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/diagnostics"
	"github.com/gofiber/fiber/v2"
)

const diagnosticsBodyLimit = 64 * 1024

type diagnosticsAttempt struct {
	Number      int       `json:"number"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	Streaming  bool      `json:"streaming"`
	Retry      bool      `json:"retry"`
	StatusCode int       `json:"status_code,omitempty"`
	Error      string    `json:"error,omitempty"`
}

type diagnosticsMetadata struct {
	Timeline      []diagnosticsTimeline `json:"timeline,omitempty"`
	Attempts      []diagnosticsAttempt  `json:"attempts,omitempty"`
	UpstreamURL   string                `json:"upstream_url,omitempty"`
	Metadata      json.RawMessage       `json:"metadata,omitempty"`
}

type diagnosticsTimeline struct {
	At    time.Time `json:"at"`
	Stage string    `json:"stage"`
	Detail string   `json:"detail,omitempty"`
}

type diagnosticsTrace struct {
	mu       sync.Mutex
	store    *diagnostics.Store
	event    diagnostics.Event
	metadata diagnosticsMetadata
	start    time.Time
	finished bool
}

func newDiagnosticsTrace(c *fiber.Ctx, cfg *config.Config, store *diagnostics.Store) *diagnosticsTrace {
	if store == nil {
		return nil
	}
	now := time.Now().UTC()
	trace := &diagnosticsTrace{
		store: store,
		start: now,
		event: diagnostics.Event{
			RequestID: c.GetRespHeader("X-Request-ID"),
			CreatedAt: now,
			Method: c.Method(),
			Path: c.Path(),
			Provider: string(cfg.DetectProvider()),
		},
		metadata: diagnosticsMetadata{UpstreamURL: cfg.ChatCompletionsURL()},
	}
	trace.stage("request_received", "")
	return trace
}

func (trace *diagnosticsTrace) stage(stage, detail string) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.metadata.Timeline = append(trace.metadata.Timeline, diagnosticsTimeline{
		At: time.Now().UTC(), Stage: stage, Detail: detail,
	})
}

func (trace *diagnosticsTrace) setModel(model string, streaming bool) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.event.Model = model
	trace.event.Streaming = streaming
}

func (trace *diagnosticsTrace) setMalformedBody(body []byte, contentType string, parseErr error) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.metadata.Metadata = diagnostics.MalformedBodyJSON(body, contentType, parseErr)
}

func (trace *diagnosticsTrace) setRequestBody(body []byte) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.event.RequestBody = redactDiagnosticJSON(body)
}

func (trace *diagnosticsTrace) setResponseBody(body []byte) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.event.ResponseBody = redactDiagnosticJSON(body)
}

func (trace *diagnosticsTrace) beginAttempt(streaming, retry bool) int {
	if trace == nil {
		return -1
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.metadata.Attempts = append(trace.metadata.Attempts, diagnosticsAttempt{
		Number: len(trace.metadata.Attempts) + 1,
		StartedAt: time.Now().UTC(),
		Streaming: streaming,
		Retry: retry,
	})
	return len(trace.metadata.Attempts) - 1
}

func (trace *diagnosticsTrace) endAttempt(index, statusCode int, err error) {
	if trace == nil || index < 0 {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if index >= len(trace.metadata.Attempts) {
		return
	}
	attempt := &trace.metadata.Attempts[index]
	attempt.CompletedAt = time.Now().UTC()
	attempt.StatusCode = statusCode
	if err != nil {
		attempt.Error = err.Error()
	}
}

func (trace *diagnosticsTrace) finish(statusCode int, err error) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	if trace.finished {
		trace.mu.Unlock()
		return
	}
	trace.finished = true
	now := time.Now().UTC()
	trace.event.UpdatedAt = now
	trace.event.StatusCode = statusCode
	trace.event.Duration = now.Sub(trace.start)
	if err != nil {
		trace.event.Error = err.Error()
	}
	trace.metadata.Timeline = append(trace.metadata.Timeline, diagnosticsTimeline{At: now, Stage: "completed"})
	trace.event.Metadata, _ = json.Marshal(trace.metadata)
	event := trace.event
	trace.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if insertErr := trace.store.Insert(ctx, event); insertErr != nil {
		fmt.Printf("[WARN] Diagnostics insert failed request_id=%s: %v\n", event.RequestID, insertErr)
	}
}

func redactDiagnosticJSON(body []byte) json.RawMessage {
	if len(body) == 0 {
		return nil
	}
	if len(body) > diagnosticsBodyLimit {
		sum := sha256.Sum256(body)
		encoded, _ := json.Marshal(map[string]interface{}{
			"redacted": true,
			"reason": "body_limit",
			"length": len(body),
			"sha256": hex.EncodeToString(sum[:]),
		})
		return encoded
	}
	redacted, err := diagnostics.RedactJSON(body, diagnostics.RedactionOptions{})
	if err == nil {
		return redacted
	}
	return diagnostics.MalformedBodyJSON(body, "application/json", err)
}

func requestIDMiddleware(c *fiber.Ctx) error {
	requestID := newRequestID()
	c.Set("X-Request-ID", requestID)
	c.Locals("request_id", requestID)
	return c.Next()
}

func newRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

func setupDiagnosticsEndpoints(app *fiber.App, store *diagnostics.Store) {
	if store == nil {
		return
	}
	group := app.Group("/debug/logs", requireLoopback)
	group.Get("", diagnosticsHTML)
	group.Get("/", diagnosticsHTML)
	group.Get("/events", func(c *fiber.Ctx) error { return diagnosticsList(c, store) })
	group.Get("/export", func(c *fiber.Ctx) error { return diagnosticsExport(c, store) })
	group.Delete("", func(c *fiber.Ctx) error { return diagnosticsClear(c, store) })
	group.Delete("/", func(c *fiber.Ctx) error { return diagnosticsClear(c, store) })
	group.Get("/:id", func(c *fiber.Ctx) error { return diagnosticsDetail(c, store) })
	group.Delete("/:id", func(c *fiber.Ctx) error { return diagnosticsDelete(c, store) })
}

func requireLoopback(c *fiber.Ctx) error {
	remoteIP := c.Context().RemoteIP()
	if remoteIP == nil || !remoteIP.IsLoopback() {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "debug logs are available only from loopback"})
	}
	return c.Next()
}

func diagnosticsQuery(c *fiber.Ctx) diagnostics.Query {
	limit, _ := strconv.Atoi(c.Query("limit", "100"))
	offset, _ := strconv.Atoi(c.Query("offset", "0"))
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return diagnostics.Query{
		RequestID: c.Query("request_id"),
		Model: c.Query("model"),
		Provider: c.Query("provider"),
		Limit: limit,
		Offset: offset,
	}
}

func diagnosticsList(c *fiber.Ctx, store *diagnostics.Store) error {
	events, err := store.Query(c.Context(), diagnosticsQuery(c))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"events": events})
}

func diagnosticsDetail(c *fiber.Ctx, store *diagnostics.Store) error {
	event, err := store.Detail(c.Context(), c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "diagnostic event not found"})
	}
	return c.JSON(event)
}

func diagnosticsDelete(c *fiber.Ctx, store *diagnostics.Store) error {
	deleted, err := store.Delete(c.Context(), c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	if !deleted {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "diagnostic event not found"})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func diagnosticsClear(c *fiber.Ctx, store *diagnostics.Store) error {
	count, err := store.Clear(c.Context())
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"deleted": count})
}

func diagnosticsExport(c *fiber.Ctx, store *diagnostics.Store) error {
	var output strings.Builder
	err := store.Export(c.Context(), diagnosticsQuery(c), func(event diagnostics.Event) error {
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		output.Write(encoded)
		output.WriteByte('\n')
		return nil
	})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	c.Set(fiber.HeaderContentType, "application/x-ndjson")
	c.Set(fiber.HeaderContentDisposition, `attachment; filename="proxy-diagnostics.ndjson"`)
	return c.SendString(output.String())
}

func diagnosticsHTML(c *fiber.Ctx) error {
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.SendString(`<!doctype html><html><head><meta charset="utf-8"><title>Proxy diagnostics</title><style>body{font:14px system-ui;margin:2rem;color:#222}button{margin:.25rem}table{border-collapse:collapse;width:100%}th,td{border-bottom:1px solid #ddd;padding:.5rem;text-align:left}pre{white-space:pre-wrap;background:#f5f5f5;padding:1rem}</style></head><body><h1>Proxy diagnostics</h1><button onclick="load()">Refresh</button><button onclick="clearLogs()">Clear</button><a href="/debug/logs/export">Export</a><table><thead><tr><th>Time</th><th>Request</th><th>Model</th><th>Status</th><th>Duration</th></tr></thead><tbody id="rows"></tbody></table><pre id="detail">Select a request.</pre><script>async function load(){const r=await fetch('/debug/logs/events');const j=await r.json();rows.innerHTML='';for(const e of j.events||[]){const tr=document.createElement('tr');tr.innerHTML='<td>'+e.created_at+'</td><td><button data-id="'+e.request_id+'">'+e.request_id+'</button></td><td>'+esc(e.model||'')+'</td><td>'+e.status_code+'</td><td>'+e.duration+'</td>';tr.querySelector('button').onclick=()=>show(e.request_id);rows.appendChild(tr)}}async function show(id){const r=await fetch('/debug/logs/'+encodeURIComponent(id));detail.textContent=JSON.stringify(await r.json(),null,2)}async function clearLogs(){if(confirm('Delete all diagnostics?')){await fetch('/debug/logs/',{method:'DELETE'});load()}}function esc(s){const d=document.createElement('div');d.textContent=s;return d.innerHTML}load()</script></body></html>`)
}
