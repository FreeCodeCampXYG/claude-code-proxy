package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProxyConfigValidation(t *testing.T) {
	if err := ValidateProxyConfig(ProxyConfig{Enabled: true, Type: "http", Address: "127.0.0.1:7890"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProxyConfig(ProxyConfig{Enabled: true, Type: "bad", Address: "127.0.0.1:7890"}); err == nil {
		t.Fatal("expected invalid proxy type error")
	}
	if err := ValidateProxyConfig(ProxyConfig{Enabled: true, Type: "http"}); err == nil {
		t.Fatal("expected missing address error")
	}
}

func TestProxyManagerSaveAndSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy-settings.json")
	manager, err := NewProxyManager(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ProxyConfig{Enabled: true, Type: "http", Address: "127.0.0.1:7890", Username: "u", Password: "p"}
	if err := manager.Save(cfg); err != nil {
		t.Fatal(err)
	}
	loaded := manager.Snapshot()
	if !loaded.Enabled || loaded.Type != "http" || loaded.Address != "127.0.0.1:7890" {
		t.Fatalf("unexpected snapshot: %#v", loaded)
	}
	if data, err := os.ReadFile(path); err != nil || len(data) == 0 {
		t.Fatalf("saved file invalid: %v %q", err, data)
	}
}
