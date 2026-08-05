package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestPlaygroundChatStreamEmitsUpstreamErrors(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	upstreamURL := upstream.URL
	upstream.Close()

	app := fiber.New()
	setupPlaygroundEndpoints(app, &config.Config{OpenAIBaseURL: upstreamURL, OpenAIAPIKey: "test-key", SonnetModel: "gpt-test"})
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
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "event: error") || !strings.Contains(string(body), "upstream request failed") {
		t.Fatalf("unexpected error stream status=%d body=%s", resp.StatusCode, body)
	}
}

func TestPlaygroundPageReflectsImplementedCapabilities(t *testing.T) {
	app := fiber.New()
	setupPlaygroundEndpoints(app, &config.Config{OpenAIBaseURL: "https://api.openai.com/v1", SonnetModel: "gpt-test"})
	baseURL := startLoopbackTestServer(t, app)

	resp := getLoopbackTest(t, baseURL, "/playground")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected page status=%d body=%s", resp.StatusCode, body)
	}
	for _, fragment := range []string{"window.__localPageToken=", "function escapeHtml", "function processFrame", "XMLHttpRequest", "pasteOcrImage", "ocrPreviewURL", "图片工作流", "OCR 识图", "PPT 创作", "ppt-outline/v1", "IMAGE_API_URL、IMAGE_API_KEY、IMAGE_MODEL"} {
		if !strings.Contains(page, fragment) {
			t.Fatalf("playground page missing %q", fragment)
		}
	}
	for _, fragment := range []string{"追加下一步", "data-image-edit", "模型与 Key 配置蓝图", "自定义任务模板"} {
		if strings.Contains(page, fragment) {
			t.Fatalf("playground page still contains unavailable control %q", fragment)
		}
	}
}

func TestPlaygroundTextTaskEndpoints(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		content := "task ok"
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "ppt-outline/v1") {
				content = `{"schema_version":"ppt-outline/v1","title":"测试标题","slides":[{"id":"slide-1","layout":"bullets","title":"第一页","bullets":["要点"]}]}`
			}
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+strconv.Quote(content)+`},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	}))
	defer upstream.Close()

	app := fiber.New()
	setupPlaygroundEndpoints(app, &config.Config{OpenAIBaseURL: upstream.URL, OpenAIAPIKey: "test-key", SonnetModel: "gpt-test"})
	baseURL := startLoopbackTestServer(t, app)
	page := getLoopbackTest(t, baseURL, "/playground")
	pageBytes, _ := io.ReadAll(page.Body)
	page.Body.Close()
	token := extractLocalPageToken(t, string(pageBytes))

	for _, tt := range []struct {
		path string
		body string
	}{
		{path: "/playground/ocr", body: `{"prompt":"hello","model":"gpt-test","max_tokens":32}`},
		{path: "/playground/ppt", body: `{"prompt":"hello","model":"gpt-test","max_tokens":32}`},
		{path: "/playground/image", body: `{"prompt":"hello","model":"gpt-test","max_tokens":32}`},
	} {
		t.Run(tt.path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, baseURL+tt.path, bytes.NewBufferString(tt.body))
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
			if resp.StatusCode != http.StatusOK || (tt.path != "/playground/ppt" && !strings.Contains(string(body), "task ok")) || (tt.path == "/playground/ppt" && !strings.Contains(string(body), "测试标题")) {
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
