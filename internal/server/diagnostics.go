package server

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/diagnostics"
	"github.com/claude-code-proxy/proxy/pkg/models"
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

	failureUpstreamError          = "upstream_error"
	failureUpstreamStatus         = "upstream_status"
	failureContextWindowExceeded = "context_window_exceeded"
	failureUpstreamRead           = "upstream_read"
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

func (trace *diagnosticsTrace) setRouting(req *models.OpenAIRequest) {
	if trace == nil || req == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.event.IncomingEffort = req.IncomingEffort
	trace.event.RoutedEffort = req.RoutedEffort
	trace.event.RouteRule = req.RouteRule
	trace.event.RouteModelOverridden = req.RouteModelOverridden
	trace.event.RouteEffortOverridden = req.RouteEffortOverridden
}

func (trace *diagnosticsTrace) setMalformedBody(body []byte, contentType string, parseErr error) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.event.ClaudeRequestBytes = len(body)
	trace.metadata.Metadata = diagnostics.MalformedBodyJSON(body, contentType, parseErr)
}

func (trace *diagnosticsTrace) setClaudeRequestMetrics(body []byte, request models.ClaudeRequest) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.event.ClaudeRequestBytes = len(body)
	trace.event.MessageCount = len(request.Messages)
}

func (trace *diagnosticsTrace) setUpstreamRequestMetrics(body []byte, toolCount int) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.event.UpstreamRequestBytes = len(body)
	trace.event.ToolCount = toolCount
}

func (trace *diagnosticsTrace) setUpstreamResponseBytes(size int) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.event.UpstreamResponseBytes = size
}

func (trace *diagnosticsTrace) setClaudeResponseBytes(size int) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.event.ClaudeResponseBytes = size
}

func (trace *diagnosticsTrace) addClaudeResponseBytes(size int) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.event.ClaudeResponseBytes += size
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
		fmt.Printf("[WARN] Diagnostics record not queued; queue is full or diagnostics is shutting down; request_id=%s\n", event.RequestID)
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

func setupDiagnosticsEndpoints(app *fiber.App, store *diagnostics.Store, cfg *config.Config) {
	if store == nil {
		return
	}
	token := newRequestID()
	group := app.Group("/debug/logs", requireLoopback, diagnosticsSecurity(token))
	group.Get("", func(c *fiber.Ctx) error { return diagnosticsHTMLWithToken(c, token) })
	group.Get("/", func(c *fiber.Ctx) error { return diagnosticsHTMLWithToken(c, token) })
	group.Get("/events", func(c *fiber.Ctx) error { return diagnosticsList(c, store) })
	group.Get("/analytics", func(c *fiber.Ctx) error { return diagnosticsAnalytics(c, store) })
	group.Get("/router-config", func(c *fiber.Ctx) error { return diagnosticsRouterConfig(c, cfg) })
	group.Put("/router-config", func(c *fiber.Ctx) error { return diagnosticsSaveRouterConfig(c, cfg) })
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
	apiKeyLabel := c.Query("api_key_label")
	apiKeyLabelNot := c.Query("api_key_label_not")
	if strings.HasPrefix(apiKeyLabel, "!") {
		apiKeyLabelNot = strings.TrimPrefix(apiKeyLabel, "!")
		apiKeyLabel = ""
	}
	return diagnostics.Query{
		RequestID: c.Query("request_id"), Model: c.Query("model"), Provider: c.Query("provider"),
		APIKeyLabel: apiKeyLabel, APIKeyLabelNot: apiKeyLabelNot, CompletionState: c.Query("completion_state"), Streaming: streaming,
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
	RequestID                 string              `json:"request_id"`
	CreatedAt                 time.Time           `json:"created_at"`
	Method                    string              `json:"method,omitempty"`
	Path                      string              `json:"path,omitempty"`
	Provider                  string              `json:"provider,omitempty"`
	APIKeyLabel               string              `json:"api_key_label,omitempty"`
	Model                     string              `json:"model,omitempty"`
	StatusCode                int                 `json:"status_code,omitempty"`
	Duration                  time.Duration       `json:"duration,omitempty"`
	Streaming                 bool                `json:"streaming"`
	Error                     string              `json:"error,omitempty"`
	AttemptCount              int                 `json:"attempt_count"`
	RetryCount                int                 `json:"retry_count"`
	InputTokens               int                 `json:"input_tokens"`
	OutputTokens              int                 `json:"output_tokens"`
	CacheReadInputTokens      int                 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens  int                 `json:"cache_creation_input_tokens"`
	ChunkCount                int                 `json:"chunk_count"`
	StopReason                string              `json:"stop_reason,omitempty"`
	CompletionState           string              `json:"completion_state,omitempty"`
	FailureKind               string              `json:"failure_kind,omitempty"`
	IncomingEffort            string              `json:"incoming_effort,omitempty"`
	RoutedEffort              string              `json:"routed_effort,omitempty"`
	RouteRule                 string              `json:"route_rule,omitempty"`
	RouteModelOverridden      bool                `json:"route_model_overridden"`
	RouteEffortOverridden     bool                `json:"route_effort_overridden"`
	Metadata                  diagnosticsMetadata `json:"metadata"`
}

func diagnosticsDetailViewFor(event diagnostics.Event) diagnosticsDetailView {
	view := diagnosticsDetailView{
		RequestID: event.RequestID, CreatedAt: event.CreatedAt, Method: event.Method, Path: event.Path,
		Provider: event.Provider, APIKeyLabel: event.APIKeyLabel, Model: event.Model, StatusCode: event.StatusCode, Duration: event.Duration,
		Streaming: event.Streaming, Error: event.Error, AttemptCount: event.AttemptCount, RetryCount: event.RetryCount,
		InputTokens: event.InputTokens, OutputTokens: event.OutputTokens, CacheReadInputTokens: event.CacheReadInputTokens,
		CacheCreationInputTokens: event.CacheCreationInputTokens, ChunkCount: event.ChunkCount, StopReason: event.StopReason,
		CompletionState: event.CompletionState, FailureKind: event.FailureKind,
		IncomingEffort: event.IncomingEffort, RoutedEffort: event.RoutedEffort, RouteRule: event.RouteRule,
		RouteModelOverridden: event.RouteModelOverridden, RouteEffortOverridden: event.RouteEffortOverridden,
	}
	_ = json.Unmarshal(event.Metadata, &view.Metadata)
	return view
}

func diagnosticsRouterConfig(c *fiber.Ctx, cfg *config.Config) error {
	if cfg == nil || cfg.Router == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "router manager is not available"})
	}
	return c.JSON(routerConfigResponse(cfg.Router))
}

func routerConfigResponse(router *config.RouterManager) fiber.Map {
	path := router.Path()
	_, err := os.Stat(path)
	exists := err == nil
	status := "loaded"
	if os.IsNotExist(err) {
		status = "using_defaults_file_missing"
	} else if err != nil {
		status = "stat_error"
	}
	return fiber.Map{"path": path, "exists": exists, "status": status, "config": router.Snapshot()}
}

func diagnosticsSaveRouterConfig(c *fiber.Ctx, cfg *config.Config) error {
	if cfg == nil || cfg.Router == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "router manager is not available"})
	}
	var routerConfig config.RouterConfig
	if err := c.BodyParser(&routerConfig); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	if err := cfg.Router.Save(routerConfig); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(routerConfigResponse(cfg.Router))
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

//go:embed diagnostics.html
var diagnosticsPageHTML string
