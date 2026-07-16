package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	trace.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if insertErr := trace.store.Insert(ctx, event); insertErr != nil {
		fmt.Printf("[WARN] Diagnostics insert failed request_id=%s: %v\n", event.RequestID, insertErr)
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
	group := app.Group("/debug/logs", requireLoopback)
	group.Get("", diagnosticsHTML)
	group.Get("/", diagnosticsHTML)
	group.Get("/events", func(c *fiber.Ctx) error { return diagnosticsList(c, store) })
	group.Get("/analytics", func(c *fiber.Ctx) error { return diagnosticsAnalytics(c, store) })
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
	return diagnostics.Query{
		RequestID: c.Query("request_id"), Model: c.Query("model"), Provider: c.Query("provider"),
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
	query, err := diagnosticsQuery(c, 100)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	events, err := store.Query(c.Context(), query)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"events": events})
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
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.SendString(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Proxy diagnostics analytics</title><style>:root{color-scheme:light dark;--bg:#f5f7fa;--panel:#fff;--text:#17202a;--muted:#687386;--line:#dce2ea;--accent:#316fea;--good:#16835b;--bad:#c33d3d}*{box-sizing:border-box}body{font:14px system-ui,sans-serif;margin:0;background:var(--bg);color:var(--text)}main{max-width:1400px;margin:auto;padding:24px}header,.controls{display:flex;gap:12px;align-items:center;flex-wrap:wrap}h1{margin-right:auto}.muted{color:var(--muted)}button,a,select{font:inherit}.controls{background:var(--panel);padding:12px;border:1px solid var(--line);border-radius:10px}.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(135px,1fr));gap:10px;margin:16px 0}.card,.panel{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px}.card b{display:block;font-size:24px;margin-top:5px}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:12px}.bar{display:grid;grid-template-columns:minmax(90px,1fr) 3fr 45px;gap:8px;align-items:center;margin:7px 0}.track{height:9px;background:var(--line);border-radius:9px;overflow:hidden}.fill{height:100%;background:var(--accent)}table{border-collapse:collapse;width:100%}th,td{border-bottom:1px solid var(--line);padding:8px;text-align:left;vertical-align:top}tbody button{border:0;background:none;color:var(--accent);cursor:pointer;padding:0}.timeline{display:flex;align-items:flex-end;gap:3px;height:150px}.hour{flex:1;min-width:4px;background:var(--accent);border-radius:3px 3px 0 0}.scroll{overflow:auto}pre{white-space:pre-wrap;overflow-wrap:anywhere;max-height:420px;overflow:auto;background:var(--bg);padding:12px}@media(prefers-color-scheme:dark){:root{--bg:#11151b;--panel:#1a2029;--text:#e7edf5;--muted:#a6b0bf;--line:#35404f;--accent:#70a0ff;--good:#51c797;--bad:#ff7777}}</style></head><body><main><header><h1>Proxy diagnostics</h1><span id="timezone" class="muted"></span><button id="refresh">Refresh</button><button id="clear">Clear</button><a id="export" href="/debug/logs/export">Export</a></header><div class="controls"><label>Range <select id="range"><option value="1">Last hour</option><option value="6">Last 6 hours</option><option value="24" selected>Last 24 hours</option><option value="72">Last 72 hours</option><option value="all">All retained</option></select></label><span id="rangeLabel" class="muted"></span></div><section id="cards" class="cards"></section><section class="grid"><div class="panel"><h2>Models</h2><div id="models"></div></div><div class="panel"><h2>Providers</h2><div id="providers"></div></div><div class="panel"><h2>Completion states</h2><div id="states"></div></div><div class="panel"><h2>Task grouping</h2><div id="tasks"></div></div></section><section class="panel" style="margin-top:12px"><h2>Hourly timeline</h2><div id="timeline" class="timeline"></div></section><section class="panel scroll" style="margin-top:12px"><h2>Recent requests</h2><table><thead><tr><th>Local time</th><th>Request</th><th>Model / provider</th><th>State</th><th>Attempts</th><th>Tokens</th><th>Latency</th></tr></thead><tbody id="rows"></tbody></table></section><section class="panel" style="margin-top:12px"><h2>Request detail</h2><pre id="detail">Select a request.</pre></section></main><script>'use strict';const $=id=>document.getElementById(id);const zone=Intl.DateTimeFormat().resolvedOptions().timeZone||'local';$('timezone').textContent='Browser timezone: '+zone;function rangeQuery(){const value=$('range').value;if(value==='all'){return ''}const since=new Date(Date.now()-Number(value)*3600000).toISOString().replace('.000Z','Z');return '?since='+encodeURIComponent(since)}function node(tag,text,cls){const e=document.createElement(tag);if(text!==undefined)e.textContent=String(text);if(cls)e.className=cls;return e}function clear(id){$(id).replaceChildren()}function cards(a){clear('cards');const items=[['Total',a.total],['Success',a.success],['Failure',a.failure],['Truncated',a.truncated],['Canceled',a.canceled],['Streaming',a.streaming],['Retries',a.retries],['Attempts',a.attempts],['Avg attempts',Number(a.average_attempts||0).toFixed(2)],['Input tokens',a.input_tokens],['Output tokens',a.output_tokens],['Cache read',a.cache_read_input_tokens],['Cache creation',a.cache_creation_input_tokens],['Avg latency',Math.round(a.average_latency_ms)+' ms'],['P95 latency',a.p95_latency_ms+' ms']];for(const item of items){const c=node('div',undefined,'card');c.append(node('span',item[0],'muted'),node('b',item[1]));$('cards').append(c)}}function breakdown(id,items){clear(id);const max=Math.max(1,...items.map(x=>x.count));for(const item of items){const row=node('div',undefined,'bar');const label=node('span',item.name);label.title=item.name;const track=node('div',undefined,'track');const fill=node('div',undefined,'fill');fill.style.width=(item.count/max*100)+'%';track.append(fill);row.append(label,track,node('span',item.count));$(id).append(row)}}function timeline(items){clear('timeline');const max=Math.max(1,...items.map(x=>x.total));for(const item of items){const bar=node('div',undefined,'hour');bar.style.height=Math.max(3,item.total/max*100)+'%';bar.title=new Date(item.hour).toLocaleString()+' — total '+item.total+', success '+item.success+', failure '+item.failure;$('timeline').append(bar)}}function tasks(a){clear('tasks');const groups=Array.isArray(a.task_groups)?a.task_groups:[];$('tasks').append(node('p','Grouping unavailable: '+a.task_grouping_unavailable,'muted'));if(!groups.length){$('tasks').append(node('p','No approved task correlation key was observed.'))}for(const group of groups){$('tasks').append(node('p',group.task_hash+': '+group.count))}}async function load(){const q=rangeQuery();$('rangeLabel').textContent=q?'Filtered by selected browser-relative range':'Showing all retained events';$('export').href='/debug/logs/export'+q;const [ar,er]=await Promise.all([fetch('/debug/logs/analytics'+q),fetch('/debug/logs/events'+(q?q+'&limit=100':'?limit=100'))]);const a=await ar.json(),e=await er.json();if(!ar.ok||!er.ok)throw new Error(a.error||e.error||'Request failed');$('rangeLabel').textContent=q?'UTC range: '+new Date(a.since).toISOString()+' — '+new Date(a.until).toISOString():'Showing all retained events';cards(a);breakdown('models',a.by_model||[]);breakdown('providers',a.by_provider||[]);breakdown('states',a.by_completion_state||[]);timeline(a.hourly_timeline||[]);tasks(a);clear('rows');for(const item of e.events||[]){const tr=node('tr');const btn=node('button',item.request_id);btn.onclick=()=>show(item.request_id);const req=node('td');req.append(btn);tr.append(node('td',new Date(item.created_at).toLocaleString()),req,node('td',(item.model||'unavailable')+' / '+(item.provider||'unavailable')),node('td',item.completion_state||'unavailable'),node('td',item.attempt_count+' ('+item.retry_count+' retries)'),node('td',item.input_tokens+' / '+item.output_tokens),node('td',item.duration/1000000+' ms'));$('rows').append(tr)}}async function show(id){const r=await fetch('/debug/logs/'+encodeURIComponent(id));$('detail').textContent=JSON.stringify(await r.json(),null,2)}async function clearLogs(){if(confirm('Delete all diagnostics?')){await fetch('/debug/logs/',{method:'DELETE'});await load()}}$('refresh').onclick=()=>load().catch(showError);$('clear').onclick=()=>clearLogs().catch(showError);$('range').onchange=()=>load().catch(showError);function showError(err){$('detail').textContent=String(err)}load().catch(showError)</script></body></html>`)
}
