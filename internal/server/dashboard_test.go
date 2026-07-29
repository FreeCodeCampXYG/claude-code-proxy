package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/gofiber/fiber/v2"
)

func TestRootDashboardAndStatusRoutes(t *testing.T) {
	app := fiber.New()
	setupDashboardEndpoints(app, &config.Config{OpenAIBaseURL: "https://api.openai.com/v1"}, nil)

	baseURL := startLoopbackTestServer(t, app)
	resp := getLoopbackTest(t, baseURL, "/")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("dashboard status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(body)
	for _, fragment := range []string{"Claude Code Proxy 控制台", "功能入口", "/settings/proxy", "/monitor", "/playground", "/prompts", `target="_blank"`, `rel="noopener noreferrer"`, "ccp-dashboard-theme", "themeSelect", "window.__localPageToken="} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("dashboard missing %q", fragment)
		}
	}

	statusResp := getLoopbackTest(t, baseURL, "/status")
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status route status = %d", statusResp.StatusCode)
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(statusResp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["message"] != "Claude Code Proxy" || payload["status"] != "running" {
		t.Fatalf("unexpected status payload: %#v", payload)
	}
}

func TestRootDashboardRejectsRemoteClients(t *testing.T) {
	app := fiber.New()
	setupDashboardEndpoints(app, &config.Config{OpenAIBaseURL: "https://api.openai.com/v1"}, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected forbidden, got %d", resp.StatusCode)
	}
}

func TestPlaceholderLocalPages(t *testing.T) {
	app := fiber.New()
	setupPlaceholderUIEndpoints(app)
	baseURL := startLoopbackTestServer(t, app)

	for _, tt := range []struct {
		path  string
		title string
	}{
		{path: "/prompts", title: "Prompt 存档"},
		{path: "/playground", title: "API 调用台"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			resp := getLoopbackTest(t, baseURL, tt.path)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), tt.title) || !strings.Contains(string(body), "window.__localPageToken=") {
				t.Fatalf("placeholder %s status=%d body=%s", tt.path, resp.StatusCode, body)
			}
		})
	}
}

func TestPlaceholderLocalPagesRejectRemoteClients(t *testing.T) {
	for _, path := range []string{"/prompts", "/playground"} {
		t.Run(path, func(t *testing.T) {
			app := fiber.New()
			setupPlaceholderUIEndpoints(app)
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = "203.0.113.7:1234"
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("expected forbidden, got %d", resp.StatusCode)
			}
		})
	}
}
