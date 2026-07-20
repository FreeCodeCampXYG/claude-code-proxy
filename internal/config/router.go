package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

type RouterRule struct {
	Enabled  bool   `json:"enabled"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	MaxChars int    `json:"max_chars,omitempty"`
	MinChars int    `json:"min_chars,omitempty"`
}

type RouterKeywordGroup struct {
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Keywords []string `json:"keywords"`
	Model    string   `json:"model,omitempty"`
	Effort   string   `json:"effort,omitempty"`
}

type RouterConfig struct {
	Enabled       bool                 `json:"enabled"`
	Defaults      RouterRule           `json:"defaults"`
	Simple        RouterRule           `json:"simple"`
	ToolUse       RouterRule           `json:"tool_use"`
	ToolResult    RouterRule           `json:"tool_result"`
	LongContext   RouterRule           `json:"long_context"`
	KeywordGroups []RouterKeywordGroup `json:"keyword_groups"`
}

type RouterManager struct {
	mu     sync.RWMutex
	path   string
	config RouterConfig
}

var routerGroupNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func DefaultRouterConfig() RouterConfig {
	return RouterConfig{
		Enabled:       false,
		Defaults:      RouterRule{Enabled: true, Model: "gpt-5.5", Effort: "medium"},
		Simple:        RouterRule{Enabled: true, MaxChars: 4000, Model: "gpt-5.4", Effort: "low"},
		ToolUse:       RouterRule{Enabled: true, Model: "gpt-5.5", Effort: "medium"},
		ToolResult:    RouterRule{Enabled: true, Model: "gpt-5.5", Effort: "low"},
		LongContext:   RouterRule{Enabled: true, MinChars: 120000, Model: "gpt-5.6-terra", Effort: "medium"},
		KeywordGroups: []RouterKeywordGroup{},
	}
}

func DefaultRouterConfigPath() string {
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".claude", "proxy-router.json")
	}
	return "proxy-router.json"
}

func NewRouterManager(path string) (*RouterManager, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultRouterConfigPath()
	}
	cfg := DefaultRouterConfig()
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse router config %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read router config %s: %w", path, err)
	}
	cfg = NormalizeRouterConfig(cfg)
	if err := ValidateRouterConfig(cfg); err != nil {
		return nil, err
	}
	return &RouterManager{path: path, config: cfg}, nil
}

func (m *RouterManager) Path() string {
	if m == nil {
		return ""
	}
	return m.path
}

func (m *RouterManager) Snapshot() RouterConfig {
	if m == nil {
		return DefaultRouterConfig()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneRouterConfig(m.config)
}

func (m *RouterManager) Save(cfg RouterConfig) error {
	if m == nil {
		return fmt.Errorf("router manager is not initialized")
	}
	cfg = NormalizeRouterConfig(cfg)
	if err := ValidateRouterConfig(cfg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode router config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return fmt.Errorf("create router config directory: %w", err)
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write router config: %w", err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace router config: %w", err)
	}
	m.mu.Lock()
	m.config = cloneRouterConfig(cfg)
	m.mu.Unlock()
	return nil
}

func NormalizeRouterConfig(cfg RouterConfig) RouterConfig {
	cfg.Defaults = normalizeRouterRule(cfg.Defaults)
	cfg.Simple = normalizeRouterRule(cfg.Simple)
	cfg.ToolUse = normalizeRouterRule(cfg.ToolUse)
	cfg.ToolResult = normalizeRouterRule(cfg.ToolResult)
	cfg.LongContext = normalizeRouterRule(cfg.LongContext)
	if cfg.KeywordGroups == nil {
		cfg.KeywordGroups = []RouterKeywordGroup{}
	}
	for i := range cfg.KeywordGroups {
		cfg.KeywordGroups[i].Name = strings.TrimSpace(cfg.KeywordGroups[i].Name)
		cfg.KeywordGroups[i].Model = strings.TrimSpace(cfg.KeywordGroups[i].Model)
		cfg.KeywordGroups[i].Effort = NormalizeRouterEffort(cfg.KeywordGroups[i].Effort)
		if cfg.KeywordGroups[i].Keywords == nil {
			cfg.KeywordGroups[i].Keywords = []string{}
		}
		for j := range cfg.KeywordGroups[i].Keywords {
			cfg.KeywordGroups[i].Keywords[j] = strings.TrimSpace(cfg.KeywordGroups[i].Keywords[j])
		}
	}
	return cfg
}

func normalizeRouterRule(rule RouterRule) RouterRule {
	rule.Model = strings.TrimSpace(rule.Model)
	rule.Effort = NormalizeRouterEffort(rule.Effort)
	return rule
}

func ValidateRouterConfig(cfg RouterConfig) error {
	if err := validateRouterRule("simple", cfg.Simple); err != nil {
		return err
	}
	if err := validateRouterRule("tool_use", cfg.ToolUse); err != nil {
		return err
	}
	if err := validateRouterRule("tool_result", cfg.ToolResult); err != nil {
		return err
	}
	if err := validateRouterRule("long_context", cfg.LongContext); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, group := range cfg.KeywordGroups {
		name := strings.TrimSpace(group.Name)
		if name == "" || !routerGroupNamePattern.MatchString(name) {
			return fmt.Errorf("router keyword group name %q is invalid", group.Name)
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("router keyword group %q is duplicated", name)
		}
		seen[key] = true
		for _, keyword := range group.Keywords {
			if strings.TrimSpace(keyword) == "" {
				return fmt.Errorf("router keyword group %q contains an empty keyword", name)
			}
		}
	}
	return nil
}

func validateRouterRule(name string, rule RouterRule) error {
	if rule.MaxChars < 0 || rule.MinChars < 0 {
		return fmt.Errorf("router %s thresholds must be non-negative", name)
	}
	return nil
}

func NormalizeRouterEffort(effort string) string { return strings.ToLower(strings.TrimSpace(effort)) }

func cloneRouterConfig(cfg RouterConfig) RouterConfig {
	if cfg.KeywordGroups == nil {
		cfg.KeywordGroups = []RouterKeywordGroup{}
	} else {
		cfg.KeywordGroups = append([]RouterKeywordGroup{}, cfg.KeywordGroups...)
	}
	for i := range cfg.KeywordGroups {
		if cfg.KeywordGroups[i].Keywords == nil {
			cfg.KeywordGroups[i].Keywords = []string{}
		} else {
			cfg.KeywordGroups[i].Keywords = append([]string{}, cfg.KeywordGroups[i].Keywords...)
		}
	}
	return cfg
}
