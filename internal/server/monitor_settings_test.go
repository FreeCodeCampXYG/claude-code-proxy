package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
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
	pageText := string(pageBody)
	for _, fragment := range []string{"流量监控", "window.__localPageToken=", "/monitor/snapshot", "X-Monitor-Token", "setInterval", "lastUpdated"} {
		if !strings.Contains(pageText, fragment) {
			t.Fatalf("monitor page missing %q: %s", fragment, pageBody)
		}
	}
	if pageResp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected monitor page status=%d body=%s", pageResp.StatusCode, pageBody)
	}

	tokenMatch := regexp.MustCompile(`window\.__localPageToken="([^"]+)";`).FindStringSubmatch(pageText)
	if len(tokenMatch) != 2 {
		t.Fatalf("monitor page did not inject token: %s", pageBody)
	}
	snapshotReq, err := http.NewRequest(http.MethodGet, baseURL+"/monitor/snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshotReq.Header.Set("X-Monitor-Token", tokenMatch[1])
	snapshotResp, err := http.DefaultClient.Do(snapshotReq)
	if err != nil {
		t.Fatal(err)
	}
	snapshotResp.Body.Close()
	if snapshotResp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated snapshot status=%d, want 200", snapshotResp.StatusCode)
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
