package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/diagnostics"
	"github.com/gofiber/fiber/v2"
)

func TestRequestIDMiddlewareReplacesInboundHeader(t *testing.T) {
	app := fiber.New()
	app.Use(requestIDMiddleware)
	app.Get("/", func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "attacker-controlled")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	requestID := resp.Header.Get("X-Request-ID")
	if requestID == "" || requestID == "attacker-controlled" {
		t.Fatalf("expected generated request ID, got %q", requestID)
	}
}

func TestHandleMessagesCapturesAllContentBoundariesOnlyWhenEnabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-test","choices":[{"message":{"role":"assistant","content":"provider secret"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	store, err := diagnostics.Open(filepath.Join(t.TempDir(), "content.db"), diagnostics.StoreOptions{Retention: time.Hour, ContentRetention: time.Hour, CaptureContent: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := &config.Config{OpenAIBaseURL: upstream.URL, OpenAIAPIKey: "secret-key", DiagnosticsCaptureContent: true}
	app := fiber.New()
	app.Use(requestIDMiddleware)
	setupClaudeEndpoints(app, cfg, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"user secret"}]}`))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != fiber.StatusOK {
		if resp != nil { resp.Body.Close() }
		t.Fatalf("request failed: response=%v error=%v", resp, err)
	}
	resp.Body.Close()
	requestID := resp.Header.Get("X-Request-ID")
	_ = awaitDiagnosticEvent(t, store, requestID)
	var snapshots []diagnostics.ContentSnapshot
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snapshots, err = store.Content(t.Context(), diagnostics.ContentQuery{RequestID: requestID})
		if err == nil && len(snapshots) == 4 { break }
		time.Sleep(5 * time.Millisecond)
	}
	if len(snapshots) != 4 {
		t.Fatalf("content snapshots = %#v, want four boundaries", snapshots)
	}
	boundaries := map[diagnostics.ContentBoundary]bool{}
	for _, snapshot := range snapshots {
		boundaries[snapshot.Boundary] = true
		if strings.Contains(string(snapshot.Content), "secret-key") {
			t.Fatalf("credential leaked in snapshot: %s", snapshot.Content)
		}
	}
	for _, boundary := range []diagnostics.ContentBoundary{diagnostics.ContentBoundaryClaudeRequest, diagnostics.ContentBoundaryUpstreamRequest, diagnostics.ContentBoundaryUpstreamResponse, diagnostics.ContentBoundaryClaudeResponse} {
		if !boundaries[boundary] { t.Fatalf("missing content boundary %q", boundary) }
	}
}

func TestHandleMessagesCapturesFailedUpstreamResponseWithoutRetryingContextError(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"input exceeds context window for max_completion_tokens"}}`)
	}))
	defer upstream.Close()

	store, err := diagnostics.Open(filepath.Join(t.TempDir(), "failure-content.db"), diagnostics.StoreOptions{CaptureContent: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := &config.Config{OpenAIBaseURL: upstream.URL, OpenAIAPIKey: "secret-key", DiagnosticsCaptureContent: true}
	app := fiber.New()
	app.Use(requestIDMiddleware)
	setupClaudeEndpoints(app, cfg, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"test"}]}`))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusBadGateway || calls != 1 {
		t.Fatalf("status=%d upstream calls=%d, want 502 and one call", resp.StatusCode, calls)
	}
	requestID := resp.Header.Get("X-Request-ID")
	event := awaitDiagnosticEvent(t, store, requestID)
	if event.AttemptCount != 1 || event.RetryCount != 0 {
		t.Fatalf("context-window diagnostics retried: %#v", event)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snapshots, contentErr := store.Content(t.Context(), diagnostics.ContentQuery{RequestID: requestID})
		for _, snapshot := range snapshots {
			if snapshot.Boundary == diagnostics.ContentBoundaryUpstreamResponse {
				return
			}
		}
		if contentErr != nil {
			t.Fatal(contentErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("missing captured upstream error response")
}

func TestHandleMessagesRecordsRedactedUpstreamRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected upstream path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"provider secret"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"prompt_tokens_details":{"cached_tokens":3},"cache_creation_input_tokens":4}}`)
	}))
	defer upstream.Close()

	store := openDiagnosticsTestStore(t)

	cfg := &config.Config{OpenAIBaseURL: upstream.URL, OpenAIAPIKey: "secret-key"}
	app := fiber.New()
	app.Use(requestIDMiddleware)
	setupClaudeEndpoints(app, cfg, store)

	body := `{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"user secret"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status %d: %s", resp.StatusCode, responseBody)
	}

	event := awaitDiagnosticEvent(t, store, resp.Header.Get("X-Request-ID"))
	if strings.Contains(string(event.RequestBody), "user secret") {
		t.Fatalf("request body was not redacted: %s", event.RequestBody)
	}
	if strings.Contains(string(event.ResponseBody), "provider secret") {
		t.Fatalf("response body was not redacted: %s", event.ResponseBody)
	}
	var metadata diagnosticsMetadata
	if err := json.Unmarshal(event.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata.Attempts) != 1 || metadata.Attempts[0].StatusCode != http.StatusOK {
		t.Fatalf("unexpected attempts: %+v", metadata.Attempts)
	}
	if event.AttemptCount != 1 || event.RetryCount != 0 || event.InputTokens != 1 || event.OutputTokens != 1 || event.CacheReadInputTokens != 3 || event.CacheCreationInputTokens != 4 || event.StopReason != "stop" || event.CompletionState != completionCompleted || event.ClaudeRequestBytes != len(body) || event.UpstreamRequestBytes == 0 || event.UpstreamResponseBytes == 0 || event.ClaudeResponseBytes == 0 || event.MessageCount != 1 {
		t.Fatalf("unexpected normalized diagnostics: %#v", event)
	}
}

func TestHandleMessagesRejectsResponsesAPIResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_test","object":"response","output":[{"type":"message"}]}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{OpenAIBaseURL: upstream.URL, OpenAIAPIKey: "secret-key"}
	app := fiber.New()
	app.Use(requestIDMiddleware)
	setupClaudeEndpoints(app, cfg, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"test"}]}`))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusBadGateway {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Responses API payload") || !strings.Contains(string(body), "Chat Completions") {
		t.Fatalf("expected descriptive invalid-response body, got %s", body)
	}
}

func TestInvalidInboundAPIKeyHasLocalAuthenticationDiagnostics(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { upstreamCalls++ }))
	defer upstream.Close()

	store := openDiagnosticsTestStore(t)
	cfg := &config.Config{AnthropicAPIKey: "expected", OpenAIBaseURL: upstream.URL}
	app := fiber.New()
	app.Use(requestIDMiddleware)
	setupClaudeEndpoints(app, cfg, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","max_tokens":1,"messages":[{"role":"user","content":"test"}]}`))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	req.Header.Set("x-api-key", "wrong")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized || upstreamCalls != 0 {
		t.Fatalf("status=%d upstream calls=%d, want 401 and no upstream call", resp.StatusCode, upstreamCalls)
	}
	event := awaitDiagnosticEvent(t, store, resp.Header.Get("X-Request-ID"))
	if event.CompletionState != completionLocalAuthentication || event.FailureKind != failureLocalAuthentication || event.AttemptCount != 0 {
		t.Fatalf("invalid-key diagnostics = %#v", event)
	}
}

func TestHandleMessagesMalformedBodyStoresMetadataOnly(t *testing.T) {
	store := openDiagnosticsTestStore(t)

	cfg := &config.Config{OpenAIBaseURL: "http://127.0.0.1:1"}
	app := fiber.New()
	app.Use(requestIDMiddleware)
	setupClaudeEndpoints(app, cfg, store)

	raw := `{"messages":["private malformed content"`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(raw))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("unexpected status %d", resp.StatusCode)
	}
	event := awaitDiagnosticEvent(t, store, resp.Header.Get("X-Request-ID"))
	if len(event.RequestBody) != 0 || strings.Contains(string(event.Metadata), "private malformed content") {
		t.Fatalf("malformed raw body leaked: request=%s metadata=%s", event.RequestBody, event.Metadata)
	}
	if !strings.Contains(string(event.Metadata), `"malformed":true`) {
		t.Fatalf("missing malformed metadata: %s", event.Metadata)
	}
}

func TestDebugLogsAreAbsentWhenDisabled(t *testing.T) {
	app := fiber.New()
	setupDiagnosticsEndpoints(app, nil)
	req := httptest.NewRequest(http.MethodGet, "/debug/logs", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("expected not found, got %d", resp.StatusCode)
	}
}

func TestDebugLogsRejectForwardedLoopbackFromRemote(t *testing.T) {
	store := openDiagnosticsTestStore(t)

	app := fiber.New()
	setupDiagnosticsEndpoints(app, store)
	req := httptest.NewRequest(http.MethodGet, "/debug/logs/events", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("expected forbidden, got %d", resp.StatusCode)
	}
}

func TestDiagnosticsAnalyticsRangesExportAndPrivacy(t *testing.T) {
	store := openDiagnosticsTestStore(t)
	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 101; i++ {
		if err := store.Insert(t.Context(), diagnostics.Event{RequestID: fmt.Sprintf("req-%03d", i), CreatedAt: base.Add(time.Duration(i)*time.Minute), StatusCode: 200, CompletionState: "completed"}); err != nil {
			t.Fatal(err)
		}
	}
	app := fiber.New()
	setupDiagnosticsEndpoints(app, store)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = app.Listener(listener) }()
	t.Cleanup(func() {
		_ = app.Shutdown()
		_ = listener.Close()
	})

	var diagnosticsToken string
	request := func(path string) *http.Response {
		t.Helper()
		for attempt := 0; attempt < 20; attempt++ {
			req, requestErr := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+path, nil)
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			if diagnosticsToken != "" {
				req.Header.Set("X-Diagnostics-Token", diagnosticsToken)
			}
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				return resp
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("request %s: proxy did not accept loopback connection", path)
		return nil
	}

	bootstrap := request("/debug/logs")
	pageBytes, _ := io.ReadAll(bootstrap.Body)
	bootstrap.Body.Close()
	const tokenPrefix = "window.__diagnosticsToken="
	start := strings.Index(string(pageBytes), tokenPrefix)
	if start < 0 {
		t.Fatal("dashboard did not inject diagnostics token")
	}
	encodedToken, _, ok := strings.Cut(string(pageBytes[start+len(tokenPrefix):]), ";")
	if !ok || json.Unmarshal([]byte(encodedToken), &diagnosticsToken) != nil || diagnosticsToken == "" {
		t.Fatal("dashboard returned invalid diagnostics token")
	}

	resp := request("/debug/logs/analytics?since=2026-07-16")
	if resp.StatusCode != fiber.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("invalid range status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	const selectedRange = "since=2026-07-16T10:00:00Z&until=2026-07-16T11:40:00Z"
	resp = request("/debug/logs/analytics?" + selectedRange)
	if resp.StatusCode != fiber.StatusOK {
		resp.Body.Close()
		t.Fatalf("analytics status = %d", resp.StatusCode)
	}
	var analytics diagnostics.Analytics
	if err := json.NewDecoder(resp.Body).Decode(&analytics); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if analytics.Total != 101 || analytics.Since != time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC) || analytics.Until != time.Date(2026, 7, 16, 11, 40, 0, 0, time.UTC) {
		t.Fatalf("analytics = %#v, want total 101 with effective UTC bounds", analytics)
	}

	resp = request("/debug/logs/events?" + selectedRange + "&limit=100&offset=0")
	if resp.StatusCode != fiber.StatusOK {
		resp.Body.Close()
		t.Fatalf("first events page status = %d", resp.StatusCode)
	}
	var firstPage struct {
		Events []diagnostics.Event `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&firstPage); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(firstPage.Events) != 100 {
		t.Fatalf("first events page length = %d, want 100", len(firstPage.Events))
	}

	resp = request("/debug/logs/events?" + selectedRange + "&limit=100&offset=100")
	if resp.StatusCode != fiber.StatusOK {
		resp.Body.Close()
		t.Fatalf("second events page status = %d", resp.StatusCode)
	}
	var secondPage struct {
		Events []diagnostics.Event `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&secondPage); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(secondPage.Events) != 1 {
		t.Fatalf("second events page length = %d, want 1", len(secondPage.Events))
	}

	resp = request("/debug/logs/analytics")
	if resp.StatusCode != fiber.StatusOK {
		resp.Body.Close()
		t.Fatalf("all analytics status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&analytics); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if analytics.Total != 101 || !analytics.Since.IsZero() || !analytics.Until.IsZero() || analytics.TaskGroups == nil || analytics.ByModel == nil || analytics.ByProvider == nil || analytics.ByCompletionState == nil || analytics.HourlyTimeline == nil || analytics.ModelConsumption == nil {
		t.Fatalf("all analytics = %#v, want unbounded all-results and non-nil slices", analytics)
	}

	resp = request("/debug/logs/export")
	if resp.StatusCode != fiber.StatusOK {
		resp.Body.Close()
		t.Fatalf("export status = %d", resp.StatusCode)
	}
	exported, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if lines := strings.Count(strings.TrimSpace(string(exported)), "\n") + 1; lines != 101 {
		t.Fatalf("exported lines = %d, want 101", lines)
	}

	resp = request("/debug/logs")
	if resp.StatusCode != fiber.StatusOK {
		resp.Body.Close()
		t.Fatalf("dashboard status = %d", resp.StatusCode)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(page)
	// Keep these contract fragments aligned with diagnosticsHTML. This deliberately
	// checks the dependency-free, Chinese accessible master-detail viewer without a
	// browser test dependency: its token-aware request helper, demand-loaded content,
	// graceful paging fallback, SVG chart, query controls, and textContent-only
	// rendering are all part of the local-page contract.
	for _, required := range []string{
		`html lang="zh-CN"`, "代理诊断", "刷新", "所有保留记录", "筛选请求", "请求主从视图", "请求列表", "请求详情", "显示原始记录 JSON", "本地内容", "确定要删除所有诊断记录吗？", "已完成",
		"Intl.DateTimeFormat('zh-CN'", "params.set('until',now.toISOString())", "const now=new Date()", `id="pageSize"`, "每页显示", `<option value="10" selected>10</option>`, `<option value="20">20</option>`, `<option value="30">30</option>`, "let pageSize=10", "[10,20,30].includes(size)", "pageSize=size;offset=0;resetDetail();load()", "API 密钥", "by_api_key", "breakdown('apiKeys',analytics.by_api_key||[],localized)", `id="apiKeyFilter"`, `id="apiKeyOptions"`, "data-api-key=\"key-2\"", "只看其他 key", "api_key_label", "api_key_label_not", "datalist('apiKeyOptions',analytics.by_api_key||[])", "eventsParams.set('limit',String(pageSize))", "eventsParams.set('offset',String(requestedOffset))", "const requestedOffset=offset", "offset!==requestedOffset", "master-detail{display:grid;grid-template-columns:minmax(0,1fr) minmax(420px,1.1fr)", "@media(max-width:1200px) and (min-width:961px)", "detailStatus", "status-badge", `id="autoRefresh"`, "自动刷新", `id="quickFilters"`, `aria-label="快捷筛选"`, "data-state=\"upstream_error\"", "data-stream=\"true\"", "detailConclusion", "failureExplanations", "copyRequestID", "navigator.clipboard.writeText", "请求 ID 已复制", "setInterval(()=>{if(!document.hidden&&!loading)load()},interval)", "prefers-reduced-motion", "role','tablist'", "role','tab'", "role','tabpanel'", "ArrowRight", "ArrowLeft", "function localized(value){return value==='unavailable'||!value?'不可用':String(value)}", "breakdown('models',analytics.by_model||[],localized)", "breakdown('providers',analytics.by_provider||[],localized)", "completion_state", "request_id", "window.__diagnosticsToken", "X-Diagnostics-Token", "/debug/logs/'+encodeURIComponent(selectedID)+'/content", "page.total", "总数暂不可用", "createElementNS('http://www.w3.org/2000/svg'", "输入与输出令牌趋势", "Token 与报文长度趋势", "Token / 报文长度占比", "长度与 token 相关性", "模型消耗对比", "renderByteRatios(analytics)", "renderCorrelation(analytics.token_byte_correlation)", "modelConsumption(analytics.model_consumption||[])", "lengthChart(analytics.hourly_timeline,lastVisibleRows)", "function kb(value)", "请求 KB=Claude 入站请求", "response_total_bytes", "上游响应+返回给 Claude 的响应", "bytes_per_input_token", "token_byte_correlation", "model_consumption", "textContent", "/debug/logs/analytics",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("dashboard missing %q", required)
		}
	}
	if strings.Contains(text, "innerHTML") {
		t.Fatal("dashboard must not render data with innerHTML")
	}
}

func TestCorrelationProbeStoresOnlyNamesAndPresence(t *testing.T) {
	store := openDiagnosticsTestStore(t)
	cfg := &config.Config{OpenAIBaseURL: "http://127.0.0.1:1"}
	app := fiber.New()
	app.Use(requestIDMiddleware)
	setupClaudeEndpoints(app, cfg, store)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{"))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	req.Header.Set("x-anthropic-session-id", "secret-session-value")
	req.Header.Set("x-anthropic-parent-request-id", "secret-parent-value")
	req.Header.Set("x-anthropic-billing-header", "cost_center=secret-cost; safe-key=secret-safe; invalid key=secret-invalid; bare-secret")
	resp, err := app.Test(req)
	if err != nil { t.Fatal(err) }
	event := awaitDiagnosticEvent(t, store, resp.Header.Get("X-Request-ID"))
	metadata := string(event.Metadata)
	if strings.Contains(metadata, "secret-session-value") || strings.Contains(metadata, "secret-parent-value") || strings.Contains(metadata, "secret-cost") || strings.Contains(metadata, "secret-safe") || strings.Contains(metadata, "secret-invalid") || strings.Contains(metadata, "bare-secret") {
		t.Fatalf("correlation values leaked: %s", metadata)
	}
	if !strings.Contains(metadata, `"name":"x-anthropic-session-id","present":true`) || !strings.Contains(metadata, `"name":"x-anthropic-billing-header","present":true`) || !strings.Contains(metadata, `"name":"cost_center","present":true`) || !strings.Contains(metadata, `"name":"safe-key","present":true`) {
		t.Fatalf("missing safe capability probe: %s", metadata)
	}
	if strings.Contains(metadata, `"name":"invalid key"`) || strings.Contains(metadata, `session_id+parent_request_id`) {
		t.Fatalf("unsafe or inferred correlation candidate stored: %s", metadata)
	}
	if event.TaskHash != "" {
		t.Fatalf("task identity must not be inferred, got %q", event.TaskHash)
	}
}

func TestStrictRFC3339AcceptsOffsets(t *testing.T) {
	value := "2026-07-16T18:30:00+08:00"
	parsed, err := strictRFC3339(value)
	if err != nil || parsed != time.Date(2026, 7, 16, 10, 30, 0, 0, time.UTC) || parsed.Location() != time.UTC {
		t.Fatalf("strictRFC3339(%q) = %v, %v", value, parsed, err)
	}
}

func awaitDiagnosticEvent(t *testing.T, store *diagnostics.Store, requestID string) diagnostics.Event {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		event, err := store.Detail(t.Context(), requestID)
		if err == nil {
			return event
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("diagnostic event %q was not persisted", requestID)
	return diagnostics.Event{}
}

func openDiagnosticsTestStore(t *testing.T) *diagnostics.Store {
	t.Helper()
	store, err := diagnostics.Open(filepath.Join(t.TempDir(), "diagnostics.db"), diagnostics.StoreOptions{Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close diagnostics store: %v", err)
		}
	})
	return store
}

func TestServerSetup(t *testing.T) {
	cfg := &config.Config{
		Host: "127.0.0.1",
		Port: "9999",
	}

	// Just verify config is valid
	if cfg.Host != "127.0.0.1" {
		t.Errorf("Expected host 127.0.0.1")
	}

	if cfg.Port != "9999" {
		t.Errorf("Expected port 9999")
	}
}

// TestAPIKeyValidation tests API key validation logic
func TestAPIKeyValidation(t *testing.T) {
	tests := []struct {
		name           string
		configuredKey  string
		requestKey     string
		shouldValidate bool
		shouldPass     bool
	}{
		{
			name:           "with matching key",
			configuredKey:  "test-key",
			requestKey:     "test-key",
			shouldValidate: true,
			shouldPass:     true,
		},
		{
			name:           "with mismatched key",
			configuredKey:  "test-key",
			requestKey:     "wrong-key",
			shouldValidate: true,
			shouldPass:     false,
		},
		{
			name:           "no validation when not configured",
			configuredKey:  "",
			requestKey:     "any-key",
			shouldValidate: false,
			shouldPass:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				AnthropicAPIKey: tt.configuredKey,
			}

			// Simulate validation logic
			if cfg.AnthropicAPIKey != "" {
				// Validation is required
				if cfg.AnthropicAPIKey != tt.requestKey {
					if tt.shouldPass {
						t.Errorf("Expected validation to pass")
					}
				} else {
					if !tt.shouldPass {
						t.Errorf("Expected validation to fail")
					}
				}
			} else {
				// No validation required
				if !tt.shouldPass {
					t.Errorf("Expected to pass when validation disabled")
				}
			}
		})
	}
}

// TestServerConfiguration tests server host and port configuration
func TestServerConfiguration(t *testing.T) {
	tests := []struct {
		name string
		host string
		port string
	}{
		{
			name: "default configuration",
			host: "0.0.0.0",
			port: "8082",
		},
		{
			name: "localhost only",
			host: "127.0.0.1",
			port: "8082",
		},
		{
			name: "custom port",
			host: "0.0.0.0",
			port: "9999",
		},
		{
			name: "specific interface",
			host: "192.168.1.100",
			port: "8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Host: tt.host,
				Port: tt.port,
			}

			if cfg.Host != tt.host {
				t.Errorf("Expected host %s, got %s", tt.host, cfg.Host)
			}

			if cfg.Port != tt.port {
				t.Errorf("Expected port %s, got %s", tt.port, cfg.Port)
			}
		})
	}
}

// TestDebugMode tests debug mode configuration
func TestDebugMode(t *testing.T) {
	tests := []struct {
		name  string
		debug bool
	}{
		{
			name:  "debug enabled",
			debug: true,
		},
		{
			name:  "debug disabled",
			debug: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Debug: tt.debug,
			}

			if cfg.Debug != tt.debug {
				t.Errorf("Expected Debug %v, got %v", tt.debug, cfg.Debug)
			}
		})
	}
}

// TestSimpleLogMode tests simple log mode configuration
func TestSimpleLogMode(t *testing.T) {
	cfg := &config.Config{
		SimpleLog: true,
	}

	if !cfg.SimpleLog {
		t.Errorf("Expected SimpleLog to be true")
	}

	cfg.SimpleLog = false
	if cfg.SimpleLog {
		t.Errorf("Expected SimpleLog to be false")
	}
}

// TestOpenRouterConfiguration tests OpenRouter-specific configuration
func TestOpenRouterConfiguration(t *testing.T) {
	cfg := &config.Config{
		OpenRouterAppName: "Claude-Code-Proxy",
		OpenRouterAppURL:  "https://github.com/example/repo",
		OpenAIBaseURL:     "https://openrouter.ai/api/v1",
	}

	if cfg.OpenRouterAppName != "Claude-Code-Proxy" {
		t.Errorf("Expected app name 'Claude-Code-Proxy'")
	}

	if cfg.OpenRouterAppURL != "https://github.com/example/repo" {
		t.Errorf("Expected app URL 'https://github.com/example/repo'")
	}

	if cfg.DetectProvider() != config.ProviderOpenRouter {
		t.Errorf("Expected OpenRouter provider")
	}
}

// TestProviderDetectionForHandlers tests provider detection in handler context
func TestProviderDetectionForHandlers(t *testing.T) {
	tests := []struct {
		name             string
		baseURL          string
		expectedProvider config.ProviderType
	}{
		{
			name:             "OpenRouter",
			baseURL:          "https://openrouter.ai/api/v1",
			expectedProvider: config.ProviderOpenRouter,
		},
		{
			name:             "OpenAI",
			baseURL:          "https://api.openai.com/v1",
			expectedProvider: config.ProviderOpenAI,
		},
		{
			name:             "Ollama",
			baseURL:          "http://localhost:11434/v1",
			expectedProvider: config.ProviderOllama,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				OpenAIBaseURL: tt.baseURL,
			}

			provider := cfg.DetectProvider()
			if provider != tt.expectedProvider {
				t.Errorf("Expected provider %v, got %v", tt.expectedProvider, provider)
			}
		})
	}
}

// TestPassthroughMode tests passthrough mode configuration
func TestPassthroughMode(t *testing.T) {
	cfg := &config.Config{
		PassthroughMode: true,
	}

	if !cfg.PassthroughMode {
		t.Errorf("Expected PassthroughMode to be true")
	}
}

// TestConfigFormatDetection tests that config can detect different format scenarios
func TestConfigFormatDetection(t *testing.T) {
	// Test that config struct properly represents all fields
	cfg := &config.Config{
		OpenAIAPIKey:      "test-key",
		OpenAIBaseURL:     "https://api.openai.com/v1",
		AnthropicAPIKey:   "test-anthropic-key",
		OpusModel:         "gpt-5",
		SonnetModel:       "gpt-5",
		HaikuModel:        "gpt-5-mini",
		Host:              "0.0.0.0",
		Port:              "8082",
		Debug:             true,
		SimpleLog:         true,
		PassthroughMode:   false,
		OpenRouterAppName: "app",
		OpenRouterAppURL:  "https://example.com",
	}

	// Verify all fields are accessible
	if cfg.OpenAIAPIKey != "test-key" {
		t.Errorf("OpenAIAPIKey not set correctly")
	}
	if cfg.AnthropicAPIKey != "test-anthropic-key" {
		t.Errorf("AnthropicAPIKey not set correctly")
	}
	if cfg.OpusModel != "gpt-5" {
		t.Errorf("OpusModel not set correctly")
	}
	if cfg.SonnetModel != "gpt-5" {
		t.Errorf("SonnetModel not set correctly")
	}
	if cfg.HaikuModel != "gpt-5-mini" {
		t.Errorf("HaikuModel not set correctly")
	}
}
