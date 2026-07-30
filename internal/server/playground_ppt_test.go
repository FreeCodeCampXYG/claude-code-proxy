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

const validPPTJSON = `{"schema_version":"ppt-outline/v1","title":"测试报告","audience":"团队","slides":[{"id":"slide-1","layout":"bullets","title":"概览","bullets":["要点"]}]}`

func TestPlaygroundPPTExportsValidatedDeck(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":`+strconv.Quote(validPPTJSON)+`}}]}`)
	}))
	defer upstream.Close()
	app := fiber.New()
	setupPlaygroundEndpoints(app, &config.Config{OpenAIBaseURL: upstream.URL, OpenAIAPIKey: "test-key", SonnetModel: "ppt-test"})
	baseURL := startLoopbackTestServer(t, app)
	token := playgroundToken(t, baseURL)

	generate, _ := http.NewRequest(http.MethodPost, baseURL+"/playground/ppt", strings.NewReader(`{"prompt":"生成报告"}`))
	generate.Header.Set("Content-Type", "application/json")
	generate.Header.Set("X-Playground-Token", token)
	generated, err := http.DefaultClient.Do(generate)
	if err != nil { t.Fatal(err) }
	body, _ := io.ReadAll(generated.Body)
	generated.Body.Close()
	if generated.StatusCode != http.StatusOK || !strings.Contains(string(body), "测试报告") { t.Fatalf("unexpected PPT generation %d %s", generated.StatusCode, body) }

	for _, format := range []struct { query, contentType, filename string }{{"markdown", "text/markdown", "ppt-outline.md"}, {"html", "text/html", "ppt-outline.html"}} {
		t.Run(format.query, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, baseURL+"/playground/ppt/export?format="+format.query, bytes.NewBufferString(`{"deck":`+validPPTJSON+`}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Playground-Token", token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil { t.Fatal(err) }
			content, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), format.contentType) || !strings.Contains(resp.Header.Get("Content-Disposition"), format.filename) || !strings.Contains(string(content), "测试报告") { t.Fatalf("unexpected export status=%d headers=%v body=%s", resp.StatusCode, resp.Header, content) }
		})
	}
}
