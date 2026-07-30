package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/promptarchive"
	"github.com/gofiber/fiber/v2"
)

func TestPromptsExportUsesFiltersWithoutPaginationOffset(t *testing.T) {
	archive := openPromptArchiveForTest(t)
	now := time.Now().UTC()
	for _, record := range []promptarchive.Record{
		{RequestID: "keep-new", CreatedAt: now, Model: "keep", Summary: "new"},
		{RequestID: "keep-old", CreatedAt: now.Add(-time.Minute), Model: "keep", Summary: "old"},
		{RequestID: "other", CreatedAt: now.Add(-2 * time.Minute), Model: "other", Summary: "other"},
	} {
		if err := archive.Insert(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}

	app := fiber.New()
	setupPromptsEndpoints(app, &config.Config{}, nil, archive)
	baseURL := startLoopbackTestServer(t, app)
	token := promptsTokenForTest(t, baseURL)

	req, err := http.NewRequest(http.MethodGet, baseURL+"/prompts/export?format=json&model=keep&limit=1&offset=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Prompts-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status=%d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("unexpected content type %q", got)
	}
	if got := resp.Header.Get("Content-Disposition"); got != `attachment; filename="prompts-archive.json"` {
		t.Fatalf("unexpected Content-Disposition %q", got)
	}
	var result struct{ Records []promptarchive.Record `json:"records"` }
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 2 || result.Records[0].RequestID != "keep-new" || result.Records[1].RequestID != "keep-old" {
		t.Fatalf("export did not return all filtered records from first page: %+v", result.Records)
	}
}

func TestPromptsExportCapsAtOneThousand(t *testing.T) {
	archive := openPromptArchiveForTest(t)
	now := time.Now().UTC()
	for i := 0; i < promptExportMaxRecords+1; i++ {
		record := promptarchive.Record{RequestID: fmt.Sprintf("record-%04d", i), CreatedAt: now.Add(-time.Duration(i) * time.Second), Model: "test"}
		if err := archive.Insert(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}

	app := fiber.New()
	setupPromptsEndpoints(app, &config.Config{}, nil, archive)
	baseURL := startLoopbackTestServer(t, app)
	req, err := http.NewRequest(http.MethodGet, baseURL+"/prompts/export?format=json&limit=1&offset=500", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Prompts-Token", promptsTokenForTest(t, baseURL))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct{ Records []promptarchive.Record `json:"records"` }
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	first := ""
	if len(result.Records) > 0 {
		first = result.Records[0].RequestID
	}
	if resp.StatusCode != http.StatusOK || len(result.Records) != promptExportMaxRecords || first != "record-0000" {
		t.Fatalf("unexpected bounded export: status=%d count=%d first=%q", resp.StatusCode, len(result.Records), first)
	}
}

func TestPromptsExportValidationAndMarkdownHeaders(t *testing.T) {
	archive := openPromptArchiveForTest(t)
	if err := archive.Insert(context.Background(), promptarchive.Record{RequestID: "one", CreatedAt: time.Now(), Model: "test", Summary: "summary"}); err != nil {
		t.Fatal(err)
	}
	app := fiber.New()
	setupPromptsEndpoints(app, &config.Config{}, nil, archive)
	baseURL := startLoopbackTestServer(t, app)
	token := promptsTokenForTest(t, baseURL)

	for _, format := range []string{"", "md", "csv"} {
		t.Run("reject "+format, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, baseURL+"/prompts/export?format="+url.QueryEscape(format), nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Prompts-Token", token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("unexpected status=%d", resp.StatusCode)
			}
		})
	}

	req, err := http.NewRequest(http.MethodGet, baseURL+"/prompts/export?format=markdown", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Prompts-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") || resp.Header.Get("Content-Disposition") != `attachment; filename="prompts-archive.md"` {
		t.Fatalf("unexpected Markdown response status=%d content-type=%q disposition=%q", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"))
	}
}

func openPromptArchiveForTest(t *testing.T) *promptarchive.Store {
	t.Helper()
	archive, err := promptarchive.Open(t.TempDir()+"/prompts.db", promptarchive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	return archive
}

func promptsTokenForTest(t *testing.T, baseURL string) string {
	t.Helper()
	page := getLoopbackTest(t, baseURL, "/prompts")
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	return extractLocalPageToken(t, string(body))
}
