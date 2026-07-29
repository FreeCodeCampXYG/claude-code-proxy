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

	pageReq := httptest.NewRequest(http.MethodGet, "/monitor", nil)
	pageReq.RemoteAddr = "127.0.0.1:1234"
	pageResp, err := app.Test(pageReq)
	if err != nil {
		t.Fatal(err)
	}
	pageBody, _ := io.ReadAll(pageResp.Body)
	pageResp.Body.Close()
	if pageResp.StatusCode != http.StatusOK || !strings.Contains(string(pageBody), "流量监控") || !strings.Contains(string(pageBody), "window.__localPageToken=") {
		t.Fatalf("unexpected monitor page: status=%d body=%s", pageResp.StatusCode, pageBody)
	}

	tokenReq := httptest.NewRequest(http.MethodGet, "/monitor/snapshot", nil)
	tokenReq.RemoteAddr = "127.0.0.1:1234"
	tokenResp, err := app.Test(tokenReq)
	if err != nil {
		t.Fatal(err)
	}
	defer tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected token protection, got %d", tokenResp.StatusCode)
	}
}

func TestProxySettingsConfigRoute(t *testing.T) {
	app := fiber.New()
	cfg := &config.Config{OpenAIBaseURL: "https://api.openai.com/v1", UpstreamProxyEnabled: true, UpstreamProxyType: "http", UpstreamProxyAddr: "127.0.0.1:7890"}
	setupProxySettingsEndpoints(app, cfg)

	pageReq := httptest.NewRequest(http.MethodGet, "/settings/proxy", nil)
	pageReq.RemoteAddr = "127.0.0.1:1234"
	pageResp, err := app.Test(pageReq)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(pageResp.Body)
	pageResp.Body.Close()
	if pageResp.StatusCode != http.StatusOK || !strings.Contains(string(body), "代理设置") {
		t.Fatalf("unexpected settings page: status=%d body=%s", pageResp.StatusCode, body)
	}

	configReq := httptest.NewRequest(http.MethodGet, "/settings/proxy/config", nil)
	configReq.RemoteAddr = "127.0.0.1:1234"
	configResp, err := app.Test(configReq)
	if err != nil {
		t.Fatal(err)
	}
	defer configResp.Body.Close()
	if configResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected protected config route, got %d", configResp.StatusCode)
	}
}
