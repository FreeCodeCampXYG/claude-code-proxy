package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultRouterConfigUsesSupportedEffortAndEmptyKeywordGroups(t *testing.T) {
	cfg := DefaultRouterConfig()
	if cfg.Defaults.Model != "gpt-5.5" || cfg.Defaults.Effort != "medium" || !cfg.Defaults.Enabled {
		t.Fatalf("defaults = %#v, want enabled gpt-5.5 medium", cfg.Defaults)
	}
	if cfg.Simple.Model != "gpt-5.4" || cfg.Simple.Effort != "low" || cfg.Simple.MaxChars != 4000 {
		t.Fatalf("simple = %#v, want gpt-5.4 low max_chars=4000", cfg.Simple)
	}
	if cfg.ToolUse.Model != "gpt-5.5" || cfg.ToolUse.Effort != "medium" {
		t.Fatalf("tool_use = %#v, want gpt-5.5 medium", cfg.ToolUse)
	}
	if cfg.ToolResult.Model != "gpt-5.5" || cfg.ToolResult.Effort != "low" {
		t.Fatalf("tool_result = %#v, want gpt-5.5 low", cfg.ToolResult)
	}
	if cfg.LongContext.Model != "gpt-5.6-terra" || cfg.LongContext.Effort != "medium" || cfg.LongContext.MinChars != 120000 {
		t.Fatalf("long_context = %#v, want gpt-5.6-terra medium min_chars=120000", cfg.LongContext)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || !json.Valid(data) {
		t.Fatalf("invalid JSON: %s", data)
	}
	if !strings.Contains(string(data), `"keyword_groups":[]`) {
		t.Fatalf("default config should encode empty keyword_groups array, got %s", data)
	}
}

func TestRouterManagerNormalizesLoadedAndSavedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.json")
	if err := os.WriteFile(path, []byte(`{"enabled":true,"simple":{"enabled":true,"model":" gpt-5.4 ","effort":" Medium ","max_chars":2000},"keyword_groups":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewRouterManager(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded := manager.Snapshot()
	if loaded.Simple.Model != "gpt-5.4" || loaded.Simple.Effort != "medium" || loaded.KeywordGroups == nil {
		t.Fatalf("loaded config was not normalized: %#v", loaded)
	}
	if err := manager.Save(loaded); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"keyword_groups": []`) && !strings.Contains(string(data), `"keyword_groups":[]`) {
		t.Fatalf("saved config should encode empty keyword_groups array, got %s", data)
	}
}

func TestRouterManagerSaveCreatesMissingParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "ClaudeWork", "proxy-router.json")
	manager, err := NewRouterManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Save(DefaultRouterConfig()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("saved router config stat error = %v", err)
	}
}


func TestNormalizeRouterEffortTreatsMissingUIValuesAsEmpty(t *testing.T) {
	for _, value := range []string{" undefined ", "null", ""} {
		if got := NormalizeRouterEffort(value); got != "" {
			t.Fatalf("NormalizeRouterEffort(%q) = %q, want empty", value, got)
		}
	}
}

func TestRouterManagerReportsInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy-router.json")
	if err := os.WriteFile(path, []byte(`{"enabled":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRouterManager(path); err == nil || !strings.Contains(err.Error(), "parse router config") {
		t.Fatalf("NewRouterManager() error = %v, want router parse error", err)
	}
}

func TestRouterManagerRejectsSQLiteDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.db")
	if err := os.WriteFile(path, append([]byte("SQLite format 3\x00"), make([]byte, 32)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRouterManager(path); err == nil || !strings.Contains(err.Error(), "SQLite database") {
		t.Fatalf("NewRouterManager() error = %v, want SQLite database error", err)
	}
}
