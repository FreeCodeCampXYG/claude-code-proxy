package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/gofiber/fiber/v2"
)

func TestPlaygroundChatStreamProxiesUpstreamSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	app := fiber.New()
	setupPlaygroundEndpoints(app, &config.Config{OpenAIBaseURL: upstream.URL, OpenAIAPIKey: "test-key", SonnetModel: "gpt-test"})
	baseURL := startLoopbackTestServer(t, app)

	page := getLoopbackTest(t, baseURL, "/playground")
	pageBytes, _ := io.ReadAll(page.Body)
	page.Body.Close()
	token := extractLocalPageToken(t, string(pageBytes))

	req, err := http.NewRequest(http.MethodPost, baseURL+"/playground/chat/stream", bytes.NewBufferString(`{"prompt":"hello","model":"gpt-test","max_tokens":32}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Playground-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"content":"hi"`) || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("unexpected stream status=%d body=%s", resp.StatusCode, body)
	}
}

func TestPlaygroundTextTaskEndpoints(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"task ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	}))
	defer upstream.Close()

	app := fiber.New()
	setupPlaygroundEndpoints(app, &config.Config{OpenAIBaseURL: upstream.URL, OpenAIAPIKey: "test-key", SonnetModel: "gpt-test"})
	baseURL := startLoopbackTestServer(t, app)
	page := getLoopbackTest(t, baseURL, "/playground")
	pageBytes, _ := io.ReadAll(page.Body)
	page.Body.Close()
	token := extractLocalPageToken(t, string(pageBytes))

	for _, path := range []string{"/playground/ocr", "/playground/ppt", "/playground/image"} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewBufferString(`{"prompt":"hello","model":"gpt-test","max_tokens":32}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Playground-Token", token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "task ok") {
				t.Fatalf("unexpected task response status=%d body=%s", resp.StatusCode, body)
			}
		})
	}
}

func extractLocalPageToken(t *testing.T, page string) string {
	t.Helper()
	prefix := "window.__localPageToken=\""
	start := strings.Index(page, prefix)
	if start < 0 {
		t.Fatalf("missing local page token in %s", page)
	}
	start += len(prefix)
	end := strings.Index(page[start:], "\"")
	if end < 0 {
		t.Fatalf("unterminated local page token in %s", page)
	}
	return page[start : start+end]
}
