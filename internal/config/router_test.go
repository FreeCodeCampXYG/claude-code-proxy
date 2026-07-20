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
	if cfg.Simple.Effort != "low" {
		t.Fatalf("simple effort = %q, want low", cfg.Simple.Effort)
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
