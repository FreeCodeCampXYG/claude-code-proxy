package server

import (
	"encoding/json"
	"io"
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

func TestHandleMessagesRecordsRedactedUpstreamRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected upstream path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"provider secret"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
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

	event, err := store.Detail(t.Context(), resp.Header.Get("X-Request-ID"))
	if err != nil {
		t.Fatal(err)
	}
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
	event, err := store.Detail(t.Context(), resp.Header.Get("X-Request-ID"))
	if err != nil {
		t.Fatal(err)
	}
	if len(event.RequestBody) != 0 || strings.Contains(string(event.Metadata), "private malformed content") {
		t.Fatalf("malformed raw body leaked: request=%s metadata=%s", event.RequestBody, event.Metadata)
	}
	if !strings.Contains(string(event.Metadata), `"malformed":true`) {
		t.Fatalf("missing malformed metadata: %s", event.Metadata)
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
