package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type ProxyConfig struct {
	Enabled  bool   `json:"enabled"`
	Type     string `json:"type,omitempty"`
	Address  string `json:"address,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type ProxyManager struct {
	mu     sync.RWMutex
	path   string
	config ProxyConfig
}

func DefaultProxyConfigPath() string {
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".claude", "proxy-settings.json")
	}
	return "proxy-settings.json"
}

func DefaultProxyConfig() ProxyConfig {
	return ProxyConfig{}
}

func NewProxyManager(path string) (*ProxyManager, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultProxyConfigPath()
	}
	cfg := DefaultProxyConfig()
	if data, err := os.ReadFile(path); err == nil {
		if bytes.HasPrefix(data, []byte("SQLite format 3\x00")) {
			return nil, fmt.Errorf("proxy config %s is a SQLite database, not a JSON proxy config; choose a separate .json path for UPSTREAM_PROXY_CONFIG_PATH", path)
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse proxy config %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read proxy config %s: %w", path, err)
	}
	cfg = NormalizeProxyConfig(cfg)
	if err := ValidateProxyConfig(cfg); err != nil {
		return nil, err
	}
	return &ProxyManager{path: path, config: cfg}, nil
}

func (m *ProxyManager) Path() string {
	if m == nil {
		return ""
	}
	return m.path
}

func (m *ProxyManager) Snapshot() ProxyConfig {
	if m == nil {
		return DefaultProxyConfig()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

func (m *ProxyManager) Save(cfg ProxyConfig) error {
	if m == nil {
		return fmt.Errorf("proxy manager is not initialized")
	}
	cfg = NormalizeProxyConfig(cfg)
	if err := ValidateProxyConfig(cfg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode proxy config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return fmt.Errorf("create proxy config directory: %w", err)
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write proxy config: %w", err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace proxy config: %w", err)
	}
	m.mu.Lock()
	m.config = cfg
	m.mu.Unlock()
	return nil
}

func NormalizeProxyConfig(cfg ProxyConfig) ProxyConfig {
	cfg.Type = strings.ToLower(strings.TrimSpace(cfg.Type))
	cfg.Address = strings.TrimSpace(cfg.Address)
	cfg.Username = strings.TrimSpace(cfg.Username)
	cfg.Password = strings.TrimSpace(cfg.Password)
	if !cfg.Enabled {
		cfg.Type = ""
		cfg.Address = ""
		cfg.Username = ""
		cfg.Password = ""
	}
	return cfg
}

func ValidateProxyConfig(cfg ProxyConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if cfg.Type != "http" && cfg.Type != "socks5" {
		return fmt.Errorf("proxy type must be http or socks5")
	}
	if cfg.Type == "socks5" {
		return fmt.Errorf("socks5 proxy support is not enabled yet")
	}
	if cfg.Address == "" {
		return fmt.Errorf("proxy address is required when proxy is enabled")
	}
	if !strings.Contains(cfg.Address, ":") {
		return fmt.Errorf("proxy address must use host:port format")
	}
	return nil
}

func MaskedProxyConfig(cfg ProxyConfig) ProxyConfig {
	cfg = NormalizeProxyConfig(cfg)
	if cfg.Password != "" {
		cfg.Password = "********"
	}
	return cfg
}
