package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/monitor"
	"github.com/gofiber/fiber/v2"
)

func TestMonitorRoutesRequireLoopbackAndExposeSnapshot(t *testing.T) {
	app := fiber.New()
	stats := monitor.NewStats(4)
	stats.Record(monitor.Event{Success: true, Model: "gpt-test"})
	setupMonitorEndpoints(app, &config.Config{OpenAIBaseURL: "https://api.openai.com/v1"}, stats)

	remoteReq := httptest.NewRequest(http.MethodGet, "/monitor", nil)
	remoteReq.RemoteAddr = "203.0.113.7:1234"
	remoteResp, err := app.Test(remoteReq)
	if err != nil {
		t.Fatal(err)
	}
	remoteResp.Body.Close()
	if remoteResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected forbidden, got %d", remoteResp.StatusCode)
	}

	baseURL := startLoopbackTestServer(t, app)
	pageResp := getLoopbackTest(t, baseURL, "/monitor")
	pageBody, _ := io.ReadAll(pageResp.Body)
	pageResp.Body.Close()
	if pageResp.StatusCode != http.StatusOK || !strings.Contains(string(pageBody), "流量监控") || !strings.Contains(string(pageBody), "window.__localPageToken=") {
		t.Fatalf("unexpected monitor page: status=%d body=%s", pageResp.StatusCode, pageBody)
	}

	tokenResp := getLoopbackTest(t, baseURL, "/monitor/snapshot")
	defer tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected token protection, got %d", tokenResp.StatusCode)
	}
}

func TestProxySettingsConfigRoute(t *testing.T) {
	app := fiber.New()
	cfg := &config.Config{OpenAIBaseURL: "https://api.openai.com/v1", UpstreamProxyEnabled: true, UpstreamProxyType: "http", UpstreamProxyAddr: "127.0.0.1:7890"}
	setupProxySettingsEndpoints(app, cfg)
	baseURL := startLoopbackTestServer(t, app)

	pageResp := getLoopbackTest(t, baseURL, "/settings/proxy")
	body, _ := io.ReadAll(pageResp.Body)
	pageResp.Body.Close()
	if pageResp.StatusCode != http.StatusOK || !strings.Contains(string(body), "代理设置") {
		t.Fatalf("unexpected settings page: status=%d body=%s", pageResp.StatusCode, body)
	}

	configResp := getLoopbackTest(t, baseURL, "/settings/proxy/config")
	defer configResp.Body.Close()
	if configResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected protected config route, got %d", configResp.StatusCode)
	}
}
