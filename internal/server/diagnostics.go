package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/diagnostics"
	"github.com/gofiber/fiber/v2"
)

const diagnosticsBodyLimit = 64 * 1024

const (
	completionCompleted          = "completed"
	completionUpstreamError      = "upstream_error"
	completionDownstreamWrite    = "downstream_write"
	completionCanceled           = "canceled"
	completionConversionError    = "conversion_error"
	completionInvalidResponse    = "invalid_response"
	completionLocalAuthentication = "local_authentication"

	failureUpstreamError        = "upstream_error"
	failureUpstreamStatus       = "upstream_status"
	failureUpstreamRead         = "upstream_read"
	failureUpstreamTruncated    = "upstream_truncated"
	failureDownstreamWrite      = "downstream_write"
	failureCanceled             = "canceled"
	failureConversionError      = "conversion_error"
	failureInvalidResponse      = "invalid_response"
	failureLocalAuthentication  = "local_authentication"
)

type diagnosticsFailure struct {
	completionState string
	failureKind     string
	err             error
}

func (failure *diagnosticsFailure) Error() string { return failure.err.Error() }
func (failure *diagnosticsFailure) Unwrap() error { return failure.err }

func diagnosticFailure(completionState, failureKind string, err error) error {
	if err == nil {
		return nil
	}
	return &diagnosticsFailure{completionState: completionState, failureKind: failureKind, err: err}
}

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
	Timeline         []diagnosticsTimeline `json:"timeline,omitempty"`
	Attempts         []diagnosticsAttempt  `json:"attempts,omitempty"`
	UpstreamURL      string                `json:"upstream_url,omitempty"`
	Metadata         json.RawMessage       `json:"metadata,omitempty"`
	CorrelationProbe correlationProbe      `json:"correlation_probe"`
}

type correlationProbe struct {
	HeaderCandidates []correlationCandidate `json:"header_candidates"`
	BillingKeys      []correlationCandidate `json:"billing_keys"`
}

type correlationCandidate struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
}

var correlationHeaderCandidates = []string{
	"x-anthropic-billing-header",
	"x-anthropic-client-sha",
	"x-anthropic-parent-request-id",
	"x-anthropic-session-id",
}

type diagnosticsTimeline struct {
	At    time.Time `json:"at"`
	Stage string    `json:"stage"`
	Detail string   `json:"detail,omitempty"`
}

type diagnosticsTrace struct {
	mu             sync.Mutex
	store          *diagnostics.Store
	event          diagnostics.Event
	metadata       diagnosticsMetadata
	captures       []diagnostics.ContentCapture
	captureContent bool
	start          time.Time
	finished       bool
}

func newDiagnosticsTrace(c *fiber.Ctx, cfg *config.Config, store *diagnostics.Store) *diagnosticsTrace {
	if store == nil {
		return nil
	}
	now := time.Now().UTC()
	trace := &diagnosticsTrace{
		store:          store,
		captureContent: cfg.DiagnosticsCaptureContent,
		start:          now,
		event: diagnostics.Event{
			RequestID: c.GetRespHeader("X-Request-ID"),
			CreatedAt: now,
			Method: c.Method(),
			Path: c.Path(),
			Provider: string(cfg.DetectProvider()),
			APIKeyLabel: cfg.OpenAIAPIKeyLabel,
		},
		metadata: diagnosticsMetadata{
			UpstreamURL:      cfg.ChatCompletionsURL(),
			CorrelationProbe: probeCorrelationCapabilities(c),
		},
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

func (trace *diagnosticsTrace) capture(boundary diagnostics.ContentBoundary, attempt int, body []byte) {
	if trace == nil || !trace.captureContent || len(body) == 0 {
		return
	}
	capture := diagnostics.ContentCapture{
		RequestID: trace.event.RequestID, AttemptNumber: attempt, Boundary: boundary,
		CreatedAt: time.Now().UTC(), SourceBytes: len(body),
	}
	if trace.store.ReserveCapture(len(body)) {
		capture.Body = append([]byte(nil), body...)
		capture.ReservedBytes = int64(len(body))
	} else {
		capture.CaptureMode = diagnostics.ContentCaptureSummary
		if len(body) > diagnostics.DefaultCaptureBytes {
			capture.Reason = "source_cap"
			capture.SourceTruncated = true
		} else {
			capture.Reason = "in_flight_budget"
		}
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.captures = append(trace.captures, capture)
}

func (trace *diagnosticsTrace) beginAttempt(streaming, retry bool) int {
	if trace == nil {
		return -1
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	now := time.Now().UTC()
	trace.metadata.Attempts = append(trace.metadata.Attempts, diagnosticsAttempt{
		Number: len(trace.metadata.Attempts) + 1,
		StartedAt: now,
		Streaming: streaming,
		Retry: retry,
	})
	trace.metadata.Timeline = append(trace.metadata.Timeline, diagnosticsTimeline{At: now, Stage: "upstream_attempt_started"})
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
	trace.metadata.Timeline = append(trace.metadata.Timeline, diagnosticsTimeline{At: attempt.CompletedAt, Stage: "upstream_attempt_completed"})
	attempt.StatusCode = statusCode
	if err != nil {
		attempt.Error = err.Error()
	}
}

func (trace *diagnosticsTrace) setOutcome(inputTokens, outputTokens, cacheReadInputTokens, cacheCreationInputTokens, chunks int, stopReason, completionState, failureKind string, canceled, truncated bool) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.event.InputTokens = inputTokens
	trace.event.OutputTokens = outputTokens
	trace.event.CacheReadInputTokens = cacheReadInputTokens
	trace.event.CacheCreationInputTokens = cacheCreationInputTokens
	trace.event.ChunkCount = chunks
	trace.event.StopReason = stopReason
	trace.event.CompletionState = completionState
	trace.event.FailureKind = failureKind
	trace.event.Canceled = canceled
	trace.event.Truncated = truncated
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
	trace.event.AttemptCount = len(trace.metadata.Attempts)
	for _, attempt := range trace.metadata.Attempts {
		if attempt.Retry {
			trace.event.RetryCount++
		}
	}
	if err != nil {
		trace.event.Error = err.Error()
		if trace.event.FailureKind == "" || trace.event.CompletionState == "" {
			completionState, failureKind := classifyFailure(err)
			if trace.event.FailureKind == "" {
				trace.event.FailureKind = failureKind
			}
			if trace.event.CompletionState == "" {
				trace.event.CompletionState = completionState
			}
		}
		if trace.event.FailureKind == failureCanceled {
			trace.event.Canceled = true
		}
		if trace.event.FailureKind == failureUpstreamTruncated {
			trace.event.Truncated = true
		}
	} else if trace.event.CompletionState == "" {
		trace.event.CompletionState = completionCompleted
	}
	trace.metadata.Timeline = append(trace.metadata.Timeline, diagnosticsTimeline{At: now, Stage: "completed"})
	trace.event.Metadata, _ = json.Marshal(trace.metadata)
	event := trace.event
	captures := append([]diagnostics.ContentCapture(nil), trace.captures...)
	trace.mu.Unlock()

	if !trace.store.Enqueue(event, captures) {
		fmt.Printf("[WARN] Diagnostics queue full; dropped request_id=%s\n", event.RequestID)
	}
}

func classifyFailure(err error) (string, string) {
	if err == nil {
		return completionCompleted, ""
	}
	if errors.Is(err, context.Canceled) {
		return completionCanceled, failureCanceled
	}
	var typed *diagnosticsFailure
	if errors.As(err, &typed) {
		return typed.completionState, typed.failureKind
	}
	return completionUpstreamError, failureUpstreamError
}

func redactDiagnosticJSON(body []byte) json.RawMessage {
	if len(body) == 0 {
		return nil
	}
	if len(body) > diagnosticsBodyLimit {
		encoded, _ := json.Marshal(map[string]interface{}{
			"redacted": true,
			"reason": "body_limit",
			"length": len(body),
		})
		return encoded
	}
	redacted, err := diagnostics.RedactJSON(body, diagnostics.RedactionOptions{})
	if err == nil {
		return redacted
	}
	return diagnostics.MalformedBodyJSON(body, "application/json", err)
}

func probeCorrelationCapabilities(c *fiber.Ctx) correlationProbe {
	probe := correlationProbe{}
	for _, name := range correlationHeaderCandidates {
		hasValue := strings.TrimSpace(c.Get(name)) != ""
		probe.HeaderCandidates = append(probe.HeaderCandidates, correlationCandidate{Name: name, Present: hasValue})
	}
	for _, name := range billingHeaderKeyNames(c.Get("x-anthropic-billing-header")) {
		probe.BillingKeys = append(probe.BillingKeys, correlationCandidate{Name: name, Present: true})
	}
	return probe
}

func billingHeaderKeyNames(value string) []string {
	seen := map[string]bool{}
	var names []string
	for _, field := range strings.Split(value, ";") {
		name, _, ok := strings.Cut(strings.TrimSpace(field), "=")
		name = strings.TrimSpace(name)
		if !ok || !safeBillingKeyName(name) || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

func safeBillingKeyName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
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
	token := newRequestID()
	group := app.Group("/debug/logs", requireLoopback, diagnosticsSecurity(token))
	group.Get("", func(c *fiber.Ctx) error { return diagnosticsHTMLWithToken(c, token) })
	group.Get("/", func(c *fiber.Ctx) error { return diagnosticsHTMLWithToken(c, token) })
	group.Get("/events", func(c *fiber.Ctx) error { return diagnosticsList(c, store) })
	group.Get("/analytics", func(c *fiber.Ctx) error { return diagnosticsAnalytics(c, store) })
	group.Get("/export", func(c *fiber.Ctx) error { return diagnosticsExport(c, store) })
	group.Delete("", func(c *fiber.Ctx) error { return diagnosticsClear(c, store) })
	group.Delete("/", func(c *fiber.Ctx) error { return diagnosticsClear(c, store) })
	group.Get("/:id/content", func(c *fiber.Ctx) error { return diagnosticsContent(c, store) })
	group.Get("/:id/raw", func(c *fiber.Ctx) error { return diagnosticsRaw(c, store) })
	group.Get("/:id", func(c *fiber.Ctx) error { return diagnosticsDetail(c, store) })
	group.Delete("/:id", func(c *fiber.Ctx) error { return diagnosticsDelete(c, store) })
}

func diagnosticsSecurity(token string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		c.Set("Cache-Control", "no-store")
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("Referrer-Policy", "no-referrer")
		c.Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
		origin := strings.TrimSpace(c.Get("Origin"))
		if origin != "" && !isLoopbackOrigin(origin) {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "diagnostics origin must be loopback"})
		}
		if c.Path() != "/debug/logs" && c.Path() != "/debug/logs/" && c.Get("X-Diagnostics-Token") != token {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "diagnostics token required"})
		}
		return c.Next()
	}
}

func isLoopbackOrigin(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func requireLoopback(c *fiber.Ctx) error {
	remoteIP := c.Context().RemoteIP()
	if remoteIP == nil || !remoteIP.IsLoopback() {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "debug logs are available only from loopback"})
	}
	return c.Next()
}

func diagnosticsQuery(c *fiber.Ctx, defaultLimit int) (diagnostics.Query, error) {
	limit, err := strconv.Atoi(c.Query("limit", strconv.Itoa(defaultLimit)))
	if err != nil || limit < 0 || limit > 1000 {
		return diagnostics.Query{}, fmt.Errorf("limit must be an integer between 0 and 1000")
	}
	offset, err := strconv.Atoi(c.Query("offset", "0"))
	if err != nil || offset < 0 {
		return diagnostics.Query{}, fmt.Errorf("offset must be a non-negative integer")
	}
	since, err := strictRFC3339(c.Query("since"))
	if err != nil {
		return diagnostics.Query{}, fmt.Errorf("invalid since: %w", err)
	}
	until, err := strictRFC3339(c.Query("until"))
	if err != nil {
		return diagnostics.Query{}, fmt.Errorf("invalid until: %w", err)
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return diagnostics.Query{}, fmt.Errorf("since must not be after until")
	}
	streamingValue := strings.TrimSpace(c.Query("stream"))
	var streaming *bool
	if streamingValue != "" {
		parsed, parseErr := strconv.ParseBool(streamingValue)
		if parseErr != nil {
			return diagnostics.Query{}, fmt.Errorf("stream must be true or false")
		}
		streaming = &parsed
	}
	return diagnostics.Query{
		RequestID: c.Query("request_id"), Model: c.Query("model"), Provider: c.Query("provider"),
		CompletionState: c.Query("completion_state"), Streaming: streaming,
		Since: since, Until: until, Limit: limit, Offset: offset,
	}, nil
}

func strictRFC3339(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("must use RFC3339 format")
	}
	return parsed.UTC(), nil
}

func diagnosticsList(c *fiber.Ctx, store *diagnostics.Store) error {
	query, err := diagnosticsQuery(c, 50)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	events, err := store.Query(c.Context(), query)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	total, err := store.Count(c.Context(), query)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"events": events, "total": total})
}

func diagnosticsAnalytics(c *fiber.Ctx, store *diagnostics.Store) error {
	query, err := diagnosticsQuery(c, 0)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	query.Limit, query.Offset = 0, 0
	analytics, err := store.Analytics(c.Context(), query)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(analytics)
}

type diagnosticsDetailView struct {
	RequestID       string                 `json:"request_id"`
	CreatedAt       time.Time              `json:"created_at"`
	Method          string                 `json:"method,omitempty"`
	Path            string                 `json:"path,omitempty"`
	Provider        string                 `json:"provider,omitempty"`
	APIKeyLabel     string                 `json:"api_key_label,omitempty"`
	Model           string                 `json:"model,omitempty"`
	StatusCode      int                    `json:"status_code,omitempty"`
	Duration        time.Duration          `json:"duration,omitempty"`
	Streaming       bool                   `json:"streaming"`
	Error           string                 `json:"error,omitempty"`
	AttemptCount    int                    `json:"attempt_count"`
	RetryCount      int                    `json:"retry_count"`
	InputTokens     int                    `json:"input_tokens"`
	OutputTokens    int                    `json:"output_tokens"`
	CacheReadInputTokens int                `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int            `json:"cache_creation_input_tokens"`
	ChunkCount      int                    `json:"chunk_count"`
	StopReason      string                 `json:"stop_reason,omitempty"`
	CompletionState string                 `json:"completion_state,omitempty"`
	FailureKind     string                 `json:"failure_kind,omitempty"`
	Metadata        diagnosticsMetadata    `json:"metadata"`
}

func diagnosticsDetailViewFor(event diagnostics.Event) diagnosticsDetailView {
	view := diagnosticsDetailView{
		RequestID: event.RequestID, CreatedAt: event.CreatedAt, Method: event.Method, Path: event.Path,
		Provider: event.Provider, APIKeyLabel: event.APIKeyLabel, Model: event.Model, StatusCode: event.StatusCode, Duration: event.Duration,
		Streaming: event.Streaming, Error: event.Error, AttemptCount: event.AttemptCount, RetryCount: event.RetryCount,
		InputTokens: event.InputTokens, OutputTokens: event.OutputTokens, CacheReadInputTokens: event.CacheReadInputTokens,
		CacheCreationInputTokens: event.CacheCreationInputTokens, ChunkCount: event.ChunkCount, StopReason: event.StopReason,
		CompletionState: event.CompletionState, FailureKind: event.FailureKind,
	}
	_ = json.Unmarshal(event.Metadata, &view.Metadata)
	return view
}

func diagnosticsDetail(c *fiber.Ctx, store *diagnostics.Store) error {
	event, err := store.Detail(c.Context(), c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "diagnostic event not found"})
	}
	return c.JSON(diagnosticsDetailViewFor(event))
}

func diagnosticsRaw(c *fiber.Ctx, store *diagnostics.Store) error {
	event, err := store.Detail(c.Context(), c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "diagnostic event not found"})
	}
	return c.JSON(event)
}

func diagnosticsContent(c *fiber.Ctx, store *diagnostics.Store) error {
	snapshots, err := store.Content(c.Context(), diagnostics.ContentQuery{RequestID: c.Params("id")})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"snapshots": snapshots})
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
	query, queryErr := diagnosticsQuery(c, 0)
	if queryErr != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": queryErr.Error()})
	}
	var output strings.Builder
	err := store.Export(c.Context(), query, func(event diagnostics.Event) error {
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
	return diagnosticsHTMLWithToken(c, "")
}

func diagnosticsHTMLWithToken(c *fiber.Ctx, token string) error {
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	page := diagnosticsPageHTML
	if token != "" {
		encoded, _ := json.Marshal(token)
		page = strings.Replace(page, "window.__diagnosticsToken=window.__diagnosticsToken||'';", "window.__diagnosticsToken="+string(encoded)+";", 1)
	}
	return c.SendString(page)
}

const diagnosticsPageHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>代理诊断</title><style>:root{color-scheme:light dark;--bg:#f5f7fa;--panel:#fff;--text:#17202a;--muted:#687386;--line:#dce2ea;--accent:#2563eb;--output:#b45309;--danger:#b42318;--selected:#e8f0ff}*{box-sizing:border-box}body{font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;margin:0;background:var(--bg);color:var(--text)}main{max-width:1600px;margin:auto;padding:24px}button,input,select{font:inherit}button{cursor:pointer;border:1px solid var(--line);border-radius:7px;background:var(--panel);color:var(--text);padding:7px 11px}button.primary{color:#fff;background:var(--accent);border-color:var(--accent)}button.danger{color:var(--danger)}button:disabled{cursor:not-allowed;opacity:.55}button:focus-visible,input:focus-visible,select:focus-visible{outline:3px solid color-mix(in srgb,var(--accent) 35%,transparent);outline-offset:2px}.page-header,.actions,.filters,.filter-set,.pagination,.legend{display:flex;gap:10px;align-items:center;flex-wrap:wrap}.page-header h1{margin:0 auto 0 0;font-size:1.65rem}.muted{color:var(--muted)}.notice{min-height:1.5em;margin:10px 0}.notice[role="alert"]{color:var(--danger)}.filters,.panel,.card{background:var(--panel);padding:12px;border:1px solid var(--line);border-radius:10px}.filter-set{border:0;margin:0;padding:0;align-items:end}.filter-set legend{font-weight:700;padding:0 4px}.field{display:grid;gap:4px;min-width:140px}.field input,.field select{padding:7px;border:1px solid var(--line);border-radius:6px;background:var(--panel);color:var(--text)}.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(135px,1fr));gap:10px;margin:16px 0}.card b{display:block;font-size:1.3rem;margin-top:3px}.analytics{display:grid;grid-template-columns:repeat(auto-fit,minmax(260px,1fr));gap:12px}.panel h2,.panel h3{margin:0 0 10px}.bar{display:grid;grid-template-columns:minmax(86px,1fr) 3fr 42px;gap:8px;align-items:center;margin:7px 0}.bar span:first-child{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.track{height:9px;background:var(--line);border-radius:9px;overflow:hidden}.fill{height:100%;background:var(--accent)}.master-detail{display:grid;grid-template-columns:minmax(0,1.35fr) minmax(350px,.85fr);gap:12px;margin-top:12px;align-items:start}.scroll{overflow:auto}table{border-collapse:collapse;width:100%}.request-table{min-width:760px}th,td{border-bottom:1px solid var(--line);padding:8px;text-align:left;vertical-align:top}th{color:var(--muted)}.request-table tbody tr.selected{background:var(--selected)}.request-button{border:0;background:none;color:var(--accent);padding:0;text-decoration:underline;text-align:left}.summary{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:8px;margin:0}.summary div{border-bottom:1px solid var(--line);padding:4px 0}.summary dt{font-size:.85rem;color:var(--muted)}.summary dd{margin:1px 0;overflow-wrap:anywhere}.timeline{list-style:none;padding:0;margin:0}.timeline li{border-left:3px solid var(--accent);padding:0 0 2px 9px;margin:0 0 7px}.json{white-space:pre-wrap;overflow-wrap:anywhere;max-height:320px;overflow:auto;background:var(--bg);padding:10px;border-radius:6px}.legend{list-style:none;padding:0;margin:0 0 8px}.legend-key{display:inline-block;width:20px;border-top:3px solid var(--accent);margin-right:5px;vertical-align:middle}.legend-key.output{border-color:var(--output);border-top-style:dashed}.chart-wrap{overflow:auto}.token-chart{display:block;min-width:600px;width:100%;height:auto}.chart-grid{stroke:var(--line);stroke-width:1}.chart-axis{fill:var(--muted);font-size:12px}.input-line{fill:none;stroke:var(--accent);stroke-width:3}.output-line{fill:none;stroke:var(--output);stroke-width:3;stroke-dasharray:7 4}.input-point{fill:var(--accent)}.output-point{fill:var(--output)}.sr-only{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0,0,0,0);white-space:nowrap;border:0}@media(max-width:960px){.master-detail{grid-template-columns:1fr}.detail-panel{order:-1}}@media(prefers-color-scheme:dark){:root{--bg:#11151b;--panel:#1a2029;--text:#e7edf5;--muted:#a6b0bf;--line:#35404f;--accent:#70a0ff;--output:#f6ad55;--danger:#ff9b93;--selected:#1d355f}}</style></head><body><main><header class="page-header"><h1>代理诊断</h1><span id="timezone" class="muted"></span><div class="actions"><button id="refresh" type="button">刷新</button><button id="export" type="button">导出</button><button id="clear" class="danger" type="button">清空</button></div></header><p id="notice" class="notice" aria-live="polite"></p><form id="filters" class="filters"><fieldset class="filter-set"><legend>筛选请求</legend><label class="field">时间范围<select id="range"><option value="1">最近 1 小时</option><option value="6">最近 6 小时</option><option value="24" selected>最近 24 小时</option><option value="72">最近 72 小时</option><option value="all">所有保留记录</option></select></label><label class="field">提供商<input id="providerFilter" list="providerOptions" placeholder="全部"></label><datalist id="providerOptions"></datalist><label class="field">模型<input id="modelFilter" list="modelOptions" placeholder="全部"></label><datalist id="modelOptions"></datalist><label class="field">完成状态<select id="stateFilter"><option value="">全部</option><option value="completed">已完成</option><option value="upstream_error">上游错误</option><option value="downstream_write">下游写入失败</option><option value="canceled">已取消</option><option value="conversion_error">转换错误</option><option value="invalid_response">响应无效</option><option value="local_authentication">本地认证失败</option></select></label><label class="field">流式<select id="streamFilter"><option value="">全部</option><option value="true">流式</option><option value="false">非流式</option></select></label><label class="field">请求 ID<input id="requestIDFilter" placeholder="完整请求 ID"></label><button class="primary" type="submit">应用筛选</button><button id="resetFilters" type="button">重置</button></fieldset></form><p id="rangeLabel" class="muted"></p><section id="cards" class="cards" aria-label="汇总指标"></section><section class="analytics" aria-label="分组分析"><section class="panel"><h2>模型</h2><div id="models"></div></section><section class="panel"><h2>提供商</h2><div id="providers"></div></section><section class="panel"><h2>API 密钥</h2><div id="apiKeys"></div></section><section class="panel"><h2>完成状态</h2><div id="states"></div></section></section><section class="panel" style="margin-top:12px" aria-labelledby="trendTitle"><h2 id="trendTitle">输入与输出令牌趋势</h2><p id="chartNote" class="muted">按当前页请求的本地小时聚合。</p><ul class="legend" aria-label="令牌趋势图例"><li><span class="legend-key" aria-hidden="true"></span>输入令牌（实线）</li><li><span class="legend-key output" aria-hidden="true"></span>输出令牌（虚线）</li></ul><div id="tokenChart" class="chart-wrap"></div><div class="scroll"><table><caption>输入与输出令牌趋势数据表</caption><thead><tr><th>小时</th><th>输入令牌</th><th>输出令牌</th></tr></thead><tbody id="chartTable"></tbody></table></div></section><div class="pagination" style="margin:12px 0"><button id="previous" type="button">上一页</button><span id="pageLabel" class="muted" aria-live="polite"></span><button id="next" type="button">下一页</button></div><section class="master-detail" aria-label="请求主从视图"><section class="panel scroll" aria-labelledby="listTitle"><h2 id="listTitle">请求列表</h2><table class="request-table"><caption class="sr-only">选择一条请求以查看结构化详情</caption><thead><tr><th>本地时间</th><th>请求</th><th>模型 / 提供商</th><th>状态</th><th>尝试</th><th>令牌</th><th>延迟</th></tr></thead><tbody id="rows"></tbody></table></section><aside class="panel detail-panel" aria-labelledby="detailTitle"><h2 id="detailTitle">请求详情</h2><p id="detailEmpty" class="muted">请选择一个请求。</p><div id="detailContent" hidden><h3>摘要</h3><dl id="summary" class="summary"></dl><h3>令牌</h3><table><caption class="sr-only">令牌用量</caption><tbody id="tokens"></tbody></table><h3>上游尝试</h3><div class="scroll"><table><caption class="sr-only">上游尝试记录</caption><thead><tr><th>#</th><th>开始</th><th>完成</th><th>模式</th><th>状态 / 错误</th></tr></thead><tbody id="attempts"></tbody></table></div><h3>时间线</h3><ol id="detailTimeline" class="timeline"></ol><button id="loadRaw" type="button" aria-expanded="false" aria-controls="json">显示原始记录 JSON</button><pre id="json" class="json" hidden></pre><section aria-labelledby="contentTitle"><h3 id="contentTitle">本地内容</h3><p class="muted">只有启用“内容抓取”后才可在此按需查看本机保存的实际内容。</p><button id="loadContent" type="button">加载内容</button><span id="contentStatus" class="muted" aria-live="polite"></span><div id="contentCards"></div><pre id="content" class="json" hidden></pre></section></div></aside></section></main><script>window.__diagnosticsToken=window.__diagnosticsToken||'';</script><script>'use strict';const $=id=>document.getElementById(id),pageSize=50;let offset=0,selectedID='',selectedEvent=null,lastVisibleRows=[];const localDateFormat=new Intl.DateTimeFormat('zh-CN',{dateStyle:'medium',timeStyle:'medium'}),numberFormat=new Intl.NumberFormat('zh-CN'),states={completed:'已完成',upstream_error:'上游错误',downstream_write:'下游写入失败',canceled:'已取消',conversion_error:'转换错误',invalid_response:'响应无效',local_authentication:'本地认证失败'};$('timezone').textContent='浏览器时区：'+(Intl.DateTimeFormat().resolvedOptions().timeZone||'本地时区');function node(tag,text,cls){const e=document.createElement(tag);if(text!==undefined)e.textContent=String(text);if(cls)e.className=cls;return e}function clear(id){$(id).replaceChildren()}function date(value){const parsed=new Date(value);return Number.isNaN(parsed.getTime())?'不可用':localDateFormat.format(parsed)}function integer(value){const parsed=Number(value);return Number.isFinite(parsed)?numberFormat.format(parsed):'不可用'}function duration(value){const parsed=Number(value);return Number.isFinite(parsed)?integer(Math.round(parsed/1000000))+' 毫秒':'不可用'}function localized(value){return value==='unavailable'||!value?'不可用':String(value)}function state(value){return states[value]||localized(value)}function endpoint(path,params){const query=params.toString();return query?path+'?'+query:path}function token(){return typeof window.__diagnosticsToken==='string'?window.__diagnosticsToken.trim():''}function headers(existing){const result=new Headers(existing||{}),value=token();if(value)result.set('X-Diagnostics-Token',value);return result}async function api(path,options){const init=options||{};return fetch(path,Object.assign({},init,{headers:headers(init.headers)}))}async function readJSON(response){const text=await response.text();if(!text)return{};try{return JSON.parse(text)}catch(error){throw new Error('服务器返回的不是 JSON 响应')}}async function getJSON(path,options){const response=await api(path,options),data=await readJSON(response);if(!response.ok)throw new Error(data.error||'请求失败（HTTP '+response.status+'）');return data}function notice(message,error){$('notice').textContent=message||'';$('notice').setAttribute('role',error?'alert':'status')}function rangeQuery(){const params=new URLSearchParams(),value=$('range').value;if(value==='all')return params;const now=new Date(),since=new Date(now.getTime()-Number(value)*3600000);params.set('since',since.toISOString());params.set('until',now.toISOString());return params}function filters(){return{provider:$('providerFilter').value.trim(),model:$('modelFilter').value.trim(),state:$('stateFilter').value,stream:$('streamFilter').value,requestID:$('requestIDFilter').value.trim()}}function query(){const params=rangeQuery(),f=filters();for(const [key,value] of [['provider',f.provider],['model',f.model],['completion_state',f.state],['stream',f.stream],['request_id',f.requestID]])if(value)params.set(key,value);return params}function matches(item,f){return(!f.provider||item.provider===f.provider)&&(!f.model||item.model===f.model)&&(!f.state||item.completion_state===f.state)&&(!f.stream||String(Boolean(item.streaming))===f.stream)&&(!f.requestID||item.request_id===f.requestID)}function cards(analytics){clear('cards');for(const item of [['总数',analytics.total],['成功',analytics.success],['失败',analytics.failure],['截断',analytics.truncated],['取消',analytics.canceled],['流式',analytics.streaming],['重试',analytics.retries],['尝试',analytics.attempts],['平均尝试次数',Number(analytics.average_attempts||0).toFixed(2)],['输入令牌',analytics.input_tokens],['输出令牌',analytics.output_tokens],['缓存读取',analytics.cache_read_input_tokens],['缓存创建',analytics.cache_creation_input_tokens],['平均延迟',Math.round(analytics.average_latency_ms||0)+' 毫秒'],['P95 延迟',(analytics.p95_latency_ms||0)+' 毫秒']]){const card=node('div',undefined,'card'),value=typeof item[1]==='string'?item[1]:integer(item[1]);card.append(node('span',item[0],'muted'),node('b',value));$('cards').append(card)}}function breakdown(id,items,translate){clear(id);if(!items.length){$(id).append(node('p','没有可显示的数据。','muted'));return}const max=Math.max(1,...items.map(item=>Number(item.count)||0));for(const item of items){const name=translate?translate(item.name):item.name,row=node('div',undefined,'bar'),label=node('span',name),track=node('div',undefined,'track'),fill=node('div',undefined,'fill');label.title=name;fill.style.width=(Number(item.count||0)/max*100)+'%';track.append(fill);row.append(label,track,node('span',integer(item.count)));$(id).append(row)}}function datalist(id,items){clear(id);for(const item of items||[]){if(item&&item.name){const option=document.createElement('option');option.value=item.name;$(id).append(option)}}}function tasks(analytics){clear('tasks');const groups=Array.isArray(analytics.task_groups)?analytics.task_groups:[];$('tasks').append(node('p','无法分组的记录：'+integer(analytics.task_grouping_unavailable||0),'muted'));if(!groups.length)$('tasks').append(node('p','未观察到已批准的任务关联键。'));for(const group of groups)$('tasks').append(node('p',localized(group.task_hash)+'：'+integer(group.count)))}function bucket(events){const buckets=new Map();for(const event of events){const when=new Date(event.created_at);if(Number.isNaN(when.getTime()))continue;when.setMinutes(0,0,0);const key=when.toISOString(),item=buckets.get(key)||{hour:key,input_tokens:0,output_tokens:0};item.input_tokens+=Number(event.input_tokens)||0;item.output_tokens+=Number(event.output_tokens)||0;buckets.set(key,item)}return Array.from(buckets.values()).sort((a,b)=>new Date(a.hour)-new Date(b.hour))}function svg(name){return document.createElementNS('http://www.w3.org/2000/svg',name)}function chart(hourly,events){clear('tokenChart');clear('chartTable');const supplied=Array.isArray(hourly)?hourly:[],hasTokens=supplied.some(item=>Object.prototype.hasOwnProperty.call(item,'input_tokens')||Object.prototype.hasOwnProperty.call(item,'output_tokens')),points=(hasTokens?supplied:bucket(events)).slice().sort((a,b)=>new Date(a.hour)-new Date(b.hour));if(!points.length){$('chartNote').textContent='当前筛选没有可用于趋势图的令牌数据。';const row=node('tr'),cell=node('td','没有可显示的数据。','muted');cell.colSpan=3;row.append(cell);$('chartTable').append(row);return}$('chartNote').textContent=hasTokens?'按当前筛选的每小时令牌数据聚合。':'按当前页请求的本地小时聚合。';const width=720,height=250,left=54,right=16,top=24,bottom=38,usableWidth=width-left-right,usableHeight=height-top-bottom,max=Math.max(1,...points.flatMap(item=>[Number(item.input_tokens)||0,Number(item.output_tokens)||0])),image=svg('svg');image.setAttribute('class','token-chart');image.setAttribute('viewBox','0 0 '+width+' '+height);image.setAttribute('role','img');image.setAttribute('aria-labelledby','chartSVGTitle chartSVGDesc');const title=svg('title');title.id='chartSVGTitle';title.textContent='输入与输出令牌折线图';const desc=svg('desc');desc.id='chartSVGDesc';desc.textContent='实线表示输入令牌，虚线表示输出令牌。下方数据表提供准确数值。';image.append(title,desc);for(const ratio of [0,.5,1]){const y=top+usableHeight*(1-ratio),grid=svg('line'),label=svg('text');grid.setAttribute('x1',left);grid.setAttribute('x2',width-right);grid.setAttribute('y1',y);grid.setAttribute('y2',y);grid.setAttribute('class','chart-grid');label.setAttribute('x',left-8);label.setAttribute('y',y+4);label.setAttribute('text-anchor','end');label.setAttribute('class','chart-axis');label.textContent=integer(Math.round(max*ratio));image.append(grid,label)}function point(index,value){return{x:points.length===1?left+usableWidth/2:left+index*usableWidth/(points.length-1),y:top+usableHeight-(Number(value)||0)/max*usableHeight}}function draw(values,kind){const path=svg('path');path.setAttribute('class',kind);path.setAttribute('d',values.map((value,index)=>{const p=point(index,value);return(index?'L':'M')+p.x.toFixed(2)+' '+p.y.toFixed(2)}).join(' '));image.append(path);values.forEach((value,index)=>{const p=point(index,value),circle=svg('circle'),tip=svg('title');circle.setAttribute('class',kind==='input-line'?'input-point':'output-point');circle.setAttribute('cx',p.x);circle.setAttribute('cy',p.y);circle.setAttribute('r','3');tip.textContent=date(points[index].hour)+'：'+(kind==='input-line'?'输入 ':'输出 ')+integer(value);circle.append(tip);image.append(circle)})}draw(points.map(item=>item.input_tokens),'input-line');draw(points.map(item=>item.output_tokens),'output-line');points.forEach((item,index)=>{if(points.length>8&&index%Math.ceil(points.length/8)!==0&&index!==points.length-1)return;const p=point(index,0),label=svg('text');label.setAttribute('x',p.x);label.setAttribute('y',height-12);label.setAttribute('text-anchor','middle');label.setAttribute('class','chart-axis');label.textContent=new Intl.DateTimeFormat('zh-CN',{hour:'2-digit',minute:'2-digit'}).format(new Date(item.hour));image.append(label)});$('tokenChart').append(image);for(const item of points){const row=node('tr');row.append(node('td',date(item.hour)),node('td',integer(item.input_tokens)),node('td',integer(item.output_tokens)));$('chartTable').append(row)}}function pages(total,visibleCount,rawCount,requestedOffset){const known=Number.isFinite(total)&&total>=0,start=rawCount?requestedOffset+1:0,end=requestedOffset+visibleCount,page=Math.floor(requestedOffset/pageSize)+1;if(known){const count=Math.max(1,Math.ceil(total/pageSize));$('pageLabel').textContent='第 '+page+' / '+count+' 页，显示 '+start+'–'+end+' / '+total+' 条';$('next').disabled=requestedOffset+pageSize>=total}else{$('pageLabel').textContent='第 '+page+' 页，显示 '+start+'–'+end+' 条（总数暂不可用）';$('next').disabled=rawCount<pageSize}$('previous').disabled=requestedOffset===0}function eventMetadata(event){if(!event.metadata)return{};if(typeof event.metadata==='object')return event.metadata;try{return JSON.parse(event.metadata)}catch(error){return{}}}function messageRow(id,message,columns){clear(id);const row=node('tr'),cell=node('td',message,'muted');cell.colSpan=columns;row.append(cell);$(id).append(row)}function resetDetail(){selectedID='';selectedEvent=null;$('detailEmpty').hidden=false;$('detailContent').hidden=true;$('content').hidden=true;$('content').textContent='';$('contentStatus').textContent='';$('loadContent').disabled=true;clear('summary');clear('tokens');messageRow('attempts','没有可显示的上游尝试。',5);clear('detailTimeline');$('json').textContent='';$('json').hidden=true;$('loadRaw').disabled=true;$('loadRaw').setAttribute('aria-expanded','false')}function definitions(items){clear('summary');for(const item of items){const box=node('div'),term=node('dt',item[0]),value=node('dd',item[1]);box.append(term,value);$('summary').append(box)}}function detail(event){const meta=eventMetadata(event),attempts=Array.isArray(meta.attempts)?meta.attempts:[],timeline=Array.isArray(meta.timeline)?meta.timeline:[];$('detailEmpty').hidden=true;$('detailContent').hidden=false;definitions([['请求 ID',localized(event.request_id)],['时间',date(event.created_at)],['模型',localized(event.model)],['提供商',localized(event.provider)],['API 密钥',localized(event.api_key_label)],['完成状态',state(event.completion_state)],['HTTP 状态',event.status_code||'不可用'],['流式',event.streaming?'是':'否'],['耗时',duration(event.duration)],['错误',localized(event.error)]]);clear('tokens');for(const item of [['输入令牌',event.input_tokens],['输出令牌',event.output_tokens],['缓存读取令牌',event.cache_read_input_tokens],['缓存创建令牌',event.cache_creation_input_tokens],['流分块',event.chunk_count],['停止原因',localized(event.stop_reason)]]){const row=node('tr'),value=typeof item[1]==='string'?item[1]:integer(item[1]);row.append(node('th',item[0]),node('td',value));$('tokens').append(row)}clear('attempts');if(!attempts.length)messageRow('attempts','没有可显示的上游尝试。',5);for(const attempt of attempts){const row=node('tr');row.append(node('td',attempt.number||'—'),node('td',date(attempt.started_at)),node('td',attempt.completed_at?date(attempt.completed_at):'进行中'),node('td',attempt.streaming?'流式':'非流式'),node('td',attempt.error||attempt.status_code||'不可用'));$('attempts').append(row)}clear('detailTimeline');if(!timeline.length)$('detailTimeline').append(node('li','没有可显示的时间线。','muted'));for(const entry of timeline){const item=node('li');item.append(node('strong',localized(entry.stage)),node('div',date(entry.at)),node('div',localized(entry.detail)));$('detailTimeline').append(item)}$('json').textContent='';$('json').hidden=true;$('loadRaw').disabled=!selectedID;$('loadRaw').setAttribute('aria-expanded','false');$('content').hidden=true;$('content').textContent='';$('contentStatus').textContent='尚未加载。';$('loadContent').disabled=!selectedID}function rows(items){clear('rows');if(!items.length){const row=node('tr'),cell=node('td','没有符合筛选条件的请求。','muted');cell.colSpan=7;row.append(cell);$('rows').append(row);return}for(const item of items){const row=node('tr'),request=node('td'),button=node('button',item.request_id||'不可用','request-button');row.classList.toggle('selected',item.request_id===selectedID);button.setAttribute('aria-pressed',String(item.request_id===selectedID));button.type='button';button.addEventListener('click',()=>select(item));request.append(button);row.append(node('td',date(item.created_at)),request,node('td',localized(item.model)+' / '+localized(item.provider)+' / '+localized(item.api_key_label)),node('td',state(item.completion_state)),node('td',(item.attempt_count||0)+'（重试 '+(item.retry_count||0)+' 次）'),node('td',integer(item.input_tokens)+' / '+integer(item.output_tokens)),node('td',duration(item.duration)));$('rows').append(row)}}async function select(summary){if(!summary.request_id)return;selectedID=summary.request_id;selectedEvent=summary;rows(lastVisibleRows);detail(summary);try{const event=await getJSON('/debug/logs/'+encodeURIComponent(selectedID));if(selectedID!==summary.request_id)return;selectedEvent=event;detail(event)}catch(error){if(selectedID===summary.request_id){$('contentStatus').textContent='详情加载失败：'+error.message;notice('详情加载失败：'+error.message,true)}}}async function loadRaw(){if(!selectedID)return;$('loadRaw').disabled=true;try{const event=await getJSON('/debug/logs/'+encodeURIComponent(selectedID)+'/raw');if(event.request_id!==selectedID)return;$('json').textContent=JSON.stringify(event,null,2);$('json').hidden=false;$('loadRaw').setAttribute('aria-expanded','true')}catch(error){notice('原始记录加载失败：'+error.message,true)}finally{$('loadRaw').disabled=!selectedID}}function contentLabel(boundary){return{claude_request:'Claude request',upstream_request:'Upstream request',upstream_response:'Upstream response',claude_response:'Claude response'}[boundary]||localized(boundary)}function renderContentSnapshots(payload){clear('contentCards');const snapshots=Array.isArray(payload.snapshots)?payload.snapshots:[];if(!snapshots.length){$('contentCards').append(node('p','没有可显示的已保存内容。','muted'));return}const renderLimit=256*1024;for(const snapshot of snapshots){const card=node('section',undefined,'card'),title=node('strong',contentLabel(snapshot.boundary)+' · 尝试 '+integer(snapshot.attempt_number));card.append(title,node('div',snapshot.capture_mode==='partial_summary'?'已保存首尾摘录':'已保存完整内容','muted'));const value=snapshot.content||{};if(snapshot.capture_mode==='partial_summary'&&value.partial){const excerpt=node('pre',undefined,'json');excerpt.textContent=String(value.head||'')+'\n\n已省略 '+integer(value.omitted_characters||0)+' 个字符\n\n'+String(value.tail||'');card.append(excerpt);if(value.source_truncated||value.reason==='in_flight_budget'||value.reason==='source_cap')card.append(node('p','内容抓取受限：'+localized(value.reason),'muted'))}else{const text=JSON.stringify(value,null,2),output=node('pre',text.length>renderLimit?text.slice(0,renderLimit)+'\n…浏览器显示已截断…':text,'json');card.append(output)}$('contentCards').append(card)}}async function loadContent(){if(!selectedID)return;$('loadContent').disabled=true;$('contentStatus').textContent='正在加载…';try{const payload=await getJSON('/debug/logs/'+encodeURIComponent(selectedID)+'/content');renderContentSnapshots(payload);$('content').hidden=true;$('contentStatus').textContent='已加载。'}catch(error){clear('contentCards');$('content').hidden=true;$('contentStatus').textContent=error.message}finally{$('loadContent').disabled=!selectedID}}function total(page,analytics,clientFilter){const fromPage=Number(page.total);if(Number.isFinite(fromPage)&&fromPage>=0)return fromPage;if(!clientFilter){const fromAnalytics=Number(analytics.total);if(Number.isFinite(fromAnalytics)&&fromAnalytics>=0)return fromAnalytics}return null}async function load(){const requestedOffset=offset,params=query(),eventsParams=new URLSearchParams(params),f=filters();eventsParams.set('limit',String(pageSize));eventsParams.set('offset',String(requestedOffset));$('previous').disabled=true;$('next').disabled=true;notice('正在加载…');let done=false;try{const results=await Promise.all([getJSON(endpoint('/debug/logs/analytics',params)),getJSON(endpoint('/debug/logs/events',eventsParams))]),analytics=results[0],page=results[1],raw=Array.isArray(page.events)?page.events:[];if(offset!==requestedOffset)return;$('rangeLabel').textContent=params.has('since')?'时间范围：'+date(params.get('since'))+' 至 '+date(params.get('until')):'显示所有保留记录';cards(analytics);breakdown('models',analytics.by_model||[],localized);breakdown('providers',analytics.by_provider||[],localized);breakdown('apiKeys',analytics.by_api_key||[],localized);breakdown('states',analytics.by_completion_state||[],state);datalist('modelOptions',analytics.by_model||[]);datalist('providerOptions',analytics.by_provider||[]);lastVisibleRows=raw.filter(item=>matches(item,f));rows(lastVisibleRows);chart(analytics.hourly_token_timeline||analytics.token_timeline||analytics.hourly_timeline,lastVisibleRows);const clientFilter=Boolean(f.state||f.stream);pages(total(page,analytics,clientFilter),lastVisibleRows.length,raw.length,requestedOffset);notice(clientFilter?'完成状态和流式筛选同时在当前页应用，以兼容旧版服务。':'');done=true}catch(error){if(offset===requestedOffset){notice('加载失败：'+error.message,true);rows([]);chart([])}}finally{if(offset===requestedOffset&&!done){$('previous').disabled=offset===0;$('next').disabled=true}}}async function exportLogs(){const button=$('export');button.disabled=true;try{const response=await api(endpoint('/debug/logs/export',query()));if(!response.ok){const data=await readJSON(response);throw new Error(data.error||'导出失败（HTTP '+response.status+'）')}const blob=await response.blob(),url=URL.createObjectURL(blob),link=document.createElement('a');link.href=url;link.download='proxy-diagnostics.ndjson';document.body.append(link);link.click();link.remove();setTimeout(()=>URL.revokeObjectURL(url),0)}catch(error){notice('导出失败：'+error.message,true)}finally{button.disabled=false}}async function clearLogs(){if(!confirm('确定要删除所有诊断记录吗？'))return;await getJSON('/debug/logs/',{method:'DELETE'});offset=0;resetDetail();await load()}$('refresh').addEventListener('click',load);$('export').addEventListener('click',exportLogs);$('clear').addEventListener('click',()=>clearLogs().catch(error=>notice('清空失败：'+error.message,true)));$('filters').addEventListener('submit',event=>{event.preventDefault();offset=0;resetDetail();load()});$('resetFilters').addEventListener('click',()=>{$('range').value='24';$('providerFilter').value='';$('modelFilter').value='';$('stateFilter').value='';$('streamFilter').value='';$('requestIDFilter').value='';offset=0;resetDetail();load()});$('previous').addEventListener('click',()=>{offset=Math.max(0,offset-pageSize);load()});$('next').addEventListener('click',()=>{offset+=pageSize;load()});$('loadRaw').addEventListener('click',loadRaw);$('loadContent').addEventListener('click',loadContent);resetDetail();load()</script></body></html>`
