// Package config handles configuration loading from environment variables and .env files.
//
// It supports multiple config file locations (./.env, ~/.claude/proxy.env, ~/.claude-code-proxy)
// and detects the provider type (OpenRouter, OpenAI, Ollama) based on the OPENAI_BASE_URL.
// The package also handles model overrides for routing Claude model names to alternative providers.
package config

import (
	"fmt"
	"os"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
)

// ProviderType represents the backend provider type
type ProviderType string

const (
	ProviderOpenRouter ProviderType = "openrouter"
	ProviderOpenAI     ProviderType = "openai"
	ProviderOllama     ProviderType = "ollama"
	ProviderNewAPI     ProviderType = "newapi"
	ProviderGeneric    ProviderType = "generic"
	ProviderUnknown    ProviderType = "unknown"
)

const (
	DefaultDiagnosticsRetention        = 72 * time.Hour
	DefaultDiagnosticsContentRetention = time.Hour
	DefaultDiagnosticsBusyTimeout      = 5 * time.Second
	DefaultDiagnosticsCloseTimeout     = 10 * time.Second
	DefaultContextWindowRewriteStatus  = 413
	DefaultContextWindowPatternsMode   = "append"
)

var DefaultContextWindowErrorPatterns = []string{
	"exceeds the context window",
	"exceeds context window",
	"context window exceeded",
	"context_window_exceeded",
	"maximum context length",
	"context length exceeded",
	"input is too long",
	"too many tokens",
	"tokens exceed",
}

// CacheKey uniquely identifies a (provider, model) combination for capability caching
// Using a struct as map key provides type safety and zero collision risk
type CacheKey struct {
	BaseURL string // Provider base URL (e.g., "https://openrouter.ai/api/v1")
	Model   string // Model name (e.g., "gpt-5", "openai/gpt-5")
}

// ModelCapabilities tracks which parameters a specific model supports
// This is learned dynamically through adaptive retry mechanism
type ModelCapabilities struct {
	UsesMaxCompletionTokens bool      // Does this model use max_completion_tokens?
	LastChecked             time.Time // When was this last verified?
}

// Global capability cache ((baseURL, model) -> capabilities)
// Protected by mutex for thread-safe access across concurrent requests
var (
	modelCapabilityCache = make(map[CacheKey]*ModelCapabilities)
	capabilityCacheMutex sync.RWMutex
)

// Config holds all proxy configuration
type Config struct {
	// Required
	OpenAIAPIKey      string
	OpenAIAPIKeyIndex int
	OpenAIAPIKeyLabel string

	// Optional
	OpenAIBaseURL   string
	OpenAIProvider  ProviderType
	AnthropicAPIKey string

	// Upstream proxy settings
	UpstreamProxyEnabled  bool
	UpstreamProxyType     string
	UpstreamProxyAddr     string
	UpstreamProxyUser     string
	UpstreamProxyPass     string
	ProxyConfigPath       string
	ProxyRuntime          *ProxyManager

	// Diagnostics storage
	DiagnosticsEnabled          bool
	DiagnosticsCaptureContent   bool
	DiagnosticsDBPath           string
	DiagnosticsRetention        time.Duration
	DiagnosticsContentRetention time.Duration
	DiagnosticsBusyTimeout      time.Duration
	DiagnosticsCloseTimeout     time.Duration

	// Prompt archive storage
	PromptArchiveEnabled   bool
	PromptArchiveDBPath    string
	PromptArchiveRetention time.Duration

	// Playground feature configuration. Image generation uses a deliberately
	// independent endpoint and credential; it never falls back to the chat key.
	ImageAPIURL string
	ImageAPIKey string
	ImageModel  string
	OCRModel    string
	PPTModel    string

	// Model routing (pattern-based if not set)
	OpusModel   string
	SonnetModel string
	HaikuModel  string

	// GPT/NewAPI tool calling compatibility
	DisableParallelToolCalls bool

	// Context-window error rewriting
	ContextWindowRewriteEnabled bool
	ContextWindowRewriteStatus  int
	ContextWindowErrorPatterns  []string
	ContextWindowPatternsMode   string

	// Router settings
	RouterConfigPath string
	Router           *RouterManager

	// Server settings
	Host string
	Port string

	// Debug logging
	Debug bool

	// Simple logging - one-line summary per request
	SimpleLog bool

	// Passthrough mode - directly proxy to Anthropic without conversion
	PassthroughMode bool

	// OpenRouter-specific (optional, improves rate limits)
	OpenRouterAppName string
	OpenRouterAppURL  string
}

// Load reads configuration from environment variables
// Tries multiple locations: ./.env, ~/.claude/proxy.env, ~/.claude-code-proxy
func Load() (*Config, error) {
	// Try loading .env files in priority order
	locations := []string{".env"}
	if homeDir, err := os.UserHomeDir(); err == nil && homeDir != "" {
		locations = append(locations,
			filepath.Join(homeDir, ".claude", "proxy.env"),
			filepath.Join(homeDir, ".claude-code-proxy"),
		)
	}

	for _, loc := range locations {
		if _, err := os.Stat(loc); err == nil {
			// The first existing config file is authoritative. Do not silently skip a
			// malformed dotenv file, because that makes startup failures misleading.
			if err := godotenv.Overload(loc); err != nil {
				return nil, fmt.Errorf("load config from %s: %w (for Windows paths, prefer ROUTER_CONFIG_PATH=D:/data/ClaudeWork/proxy-router.json)", loc, err)
			}
			fmt.Printf("📁 Loaded config from: %s\n", loc)
			break
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect config file %s: %w", loc, err)
		}
	}

	// Build config from environment
	apiKey, apiKeyIndex, apiKeyLabel, err := selectedOpenAIAPIKey(os.Getenv("OPENAI_API_KEY"), os.Getenv("OPENAI_API_KEYS"), os.Getenv("OPENAI_API_KEY_INDEX"))
	if err != nil {
		return nil, err
	}
	routerPath := getEnvOrDefault("ROUTER_CONFIG_PATH", DefaultRouterConfigPath())
	router, err := NewRouterManager(routerPath)
	if err != nil {
		return nil, fmt.Errorf("load router config from ROUTER_CONFIG_PATH=%q: %w", routerPath, err)
	}
	proxyPath := getEnvOrDefault("UPSTREAM_PROXY_CONFIG_PATH", DefaultProxyConfigPath())
	proxyRuntime, err := NewProxyManager(proxyPath)
	if err != nil {
		return nil, fmt.Errorf("load proxy config from UPSTREAM_PROXY_CONFIG_PATH=%q: %w", proxyPath, err)
	}
	cfg := &Config{
		OpenAIAPIKey:      apiKey,
		OpenAIAPIKeyIndex: apiKeyIndex,
		OpenAIAPIKeyLabel: apiKeyLabel,
		OpenAIBaseURL:   strings.TrimRight(getEnvOrDefault("OPENAI_BASE_URL", "https://api.openai.com/v1"), "/"),
		OpenAIProvider:  ProviderType(strings.ToLower(strings.TrimSpace(os.Getenv("OPENAI_PROVIDER")))),
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),

		// Upstream proxy settings
		UpstreamProxyEnabled: getEnvAsBoolOrDefault("UPSTREAM_PROXY_ENABLED", false),
		UpstreamProxyType:    strings.ToLower(strings.TrimSpace(os.Getenv("UPSTREAM_PROXY_TYPE"))),
		UpstreamProxyAddr:    strings.TrimSpace(os.Getenv("UPSTREAM_PROXY_ADDR")),
		UpstreamProxyUser:    strings.TrimSpace(os.Getenv("UPSTREAM_PROXY_USER")),
		UpstreamProxyPass:    strings.TrimSpace(os.Getenv("UPSTREAM_PROXY_PASS")),
		ProxyConfigPath:      proxyPath,
		ProxyRuntime:         proxyRuntime,

		// Diagnostics storage
		DiagnosticsEnabled:          getEnvAsBoolOrDefault("DIAGNOSTICS_ENABLED", false),
		DiagnosticsCaptureContent:   getEnvAsBoolOrDefault("DIAGNOSTICS_CAPTURE_CONTENT", false),
		DiagnosticsDBPath:           os.Getenv("DIAGNOSTICS_DB_PATH"),
		DiagnosticsRetention:        getEnvAsDurationOrDefault("DIAGNOSTICS_RETENTION", DefaultDiagnosticsRetention),
		DiagnosticsContentRetention: getEnvAsDurationOrDefault("DIAGNOSTICS_CONTENT_RETENTION", DefaultDiagnosticsContentRetention),
		DiagnosticsBusyTimeout:      getEnvAsDurationOrDefault("DIAGNOSTICS_BUSY_TIMEOUT", DefaultDiagnosticsBusyTimeout),
		DiagnosticsCloseTimeout:     getEnvAsDurationOrDefault("DIAGNOSTICS_CLOSE_TIMEOUT", DefaultDiagnosticsCloseTimeout),

		// Prompt archive storage
		PromptArchiveEnabled:   getEnvAsBoolOrDefault("PROMPT_ARCHIVE_ENABLED", false),
		PromptArchiveDBPath:    os.Getenv("PROMPT_ARCHIVE_DB_PATH"),
		PromptArchiveRetention: getEnvAsDurationOrDefault("PROMPT_ARCHIVE_RETENTION", 168*time.Hour),

		// Playground feature configuration
		ImageAPIURL: strings.TrimRight(strings.TrimSpace(os.Getenv("IMAGE_API_URL")), "/"),
		ImageAPIKey: strings.TrimSpace(os.Getenv("IMAGE_API_KEY")),
		ImageModel:  strings.TrimSpace(os.Getenv("IMAGE_MODEL")),
		OCRModel:    strings.TrimSpace(os.Getenv("OCR_MODEL")),
		PPTModel:    strings.TrimSpace(os.Getenv("PPT_MODEL")),

		// Pattern-based routing (optional overrides)
		OpusModel:   getEnvOrDefault("ANTHROPIC_DEFAULT_OPUS_MODEL", "gpt-5.6-sol"),
		SonnetModel: getEnvOrDefault("ANTHROPIC_DEFAULT_SONNET_MODEL", "gpt-5.6-terra"),
		HaikuModel:  getEnvOrDefault("ANTHROPIC_DEFAULT_HAIKU_MODEL", "gpt-5.6-luna"),

		// GPT/NewAPI tool calling compatibility
		DisableParallelToolCalls: getEnvAsBoolOrDefault("OPENAI_DISABLE_PARALLEL_TOOL_CALLS", false),

		// Context-window error rewriting
		ContextWindowRewriteEnabled: getEnvAsBoolOrDefault("CONTEXT_WINDOW_REWRITE_ENABLED", true),
		ContextWindowRewriteStatus:  getEnvAsIntOrDefault("CONTEXT_WINDOW_REWRITE_STATUS", DefaultContextWindowRewriteStatus),
		ContextWindowErrorPatterns:  loadContextWindowErrorPatterns(),
		ContextWindowPatternsMode:   strings.ToLower(strings.TrimSpace(getEnvOrDefault("CONTEXT_WINDOW_ERROR_PATTERNS_MODE", DefaultContextWindowPatternsMode))),

		// Router settings
		RouterConfigPath: routerPath,
		Router:           router,

		// Server settings
		Host: getEnvOrDefault("HOST", "0.0.0.0"),
		Port: getEnvOrDefault("PORT", "8082"),

		// Passthrough mode
		PassthroughMode: getEnvAsBoolOrDefault("PASSTHROUGH_MODE", false),

		// OpenRouter-specific (optional)
		OpenRouterAppName: os.Getenv("OPENROUTER_APP_NAME"),
		OpenRouterAppURL:  os.Getenv("OPENROUTER_APP_URL"),
	}

	if cfg.OpenAIBaseURL == "" {
		return nil, fmt.Errorf("OPENAI_BASE_URL must not be empty")
	}
	if cfg.OpenAIProvider != "" && !isValidProvider(cfg.OpenAIProvider) {
		return nil, fmt.Errorf("OPENAI_PROVIDER must be one of openrouter, openai, ollama, newapi, generic")
	}
	if err := ValidateProxyConfig(ProxyConfig{Enabled: cfg.UpstreamProxyEnabled, Type: cfg.UpstreamProxyType, Address: cfg.UpstreamProxyAddr, Username: cfg.UpstreamProxyUser, Password: cfg.UpstreamProxyPass}); err != nil {
		return nil, err
	}
	if cfg.DiagnosticsRetention <= 0 {
		return nil, fmt.Errorf("DIAGNOSTICS_RETENTION must be a positive Go duration")
	}
	if cfg.DiagnosticsCaptureContent && !cfg.DiagnosticsEnabled {
		return nil, fmt.Errorf("DIAGNOSTICS_CAPTURE_CONTENT requires DIAGNOSTICS_ENABLED=true")
	}
	if cfg.DiagnosticsCaptureContent && cfg.DiagnosticsContentRetention <= 0 {
		return nil, fmt.Errorf("DIAGNOSTICS_CONTENT_RETENTION must be a positive Go duration when content capture is enabled")
	}
	if cfg.DiagnosticsCaptureContent && cfg.DiagnosticsContentRetention > cfg.DiagnosticsRetention {
		return nil, fmt.Errorf("DIAGNOSTICS_CONTENT_RETENTION must not exceed DIAGNOSTICS_RETENTION")
	}
	if cfg.DiagnosticsBusyTimeout <= 0 {
		return nil, fmt.Errorf("DIAGNOSTICS_BUSY_TIMEOUT must be a positive Go duration")
	}
	if cfg.PromptArchiveEnabled && cfg.PromptArchiveRetention <= 0 {
		return nil, fmt.Errorf("PROMPT_ARCHIVE_RETENTION must be a positive Go duration")
	}
	if err := validateImageConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.ContextWindowPatternsMode != "append" && cfg.ContextWindowPatternsMode != "override" {
		return nil, fmt.Errorf("CONTEXT_WINDOW_ERROR_PATTERNS_MODE must be append or override")
	}
	if !isAllowedContextWindowRewriteStatus(cfg.ContextWindowRewriteStatus) {
		return nil, fmt.Errorf("CONTEXT_WINDOW_REWRITE_STATUS must be one of 400, 413, 422")
	}
	if cfg.ContextWindowPatternsMode == "override" && len(cfg.ContextWindowErrorPatterns) == 0 {
		return nil, fmt.Errorf("CONTEXT_WINDOW_ERROR_PATTERNS must not be empty when CONTEXT_WINDOW_ERROR_PATTERNS_MODE=override")
	}
	if cfg.DiagnosticsEnabled && strings.TrimSpace(cfg.DiagnosticsDBPath) == "" {
		if cacheDir, err := os.UserCacheDir(); err == nil && cacheDir != "" {
			cfg.DiagnosticsDBPath = filepath.Join(cacheDir, "claude-code-proxy", "diagnostics.db")
		} else if homeDir, err := os.UserHomeDir(); err == nil && homeDir != "" {
			cfg.DiagnosticsDBPath = filepath.Join(homeDir, ".claude", "proxy-diagnostics.db")
		} else {
			return nil, fmt.Errorf("DIAGNOSTICS_DB_PATH is required when diagnostics are enabled and no user directory is available")
		}
	}
	if cfg.PromptArchiveEnabled && strings.TrimSpace(cfg.PromptArchiveDBPath) == "" {
		if cacheDir, err := os.UserCacheDir(); err == nil && cacheDir != "" {
			cfg.PromptArchiveDBPath = filepath.Join(cacheDir, "claude-code-proxy", "prompt_archive.db")
		} else if homeDir, err := os.UserHomeDir(); err == nil && homeDir != "" {
			cfg.PromptArchiveDBPath = filepath.Join(homeDir, ".claude", "proxy-prompt-archive.db")
		} else {
			return nil, fmt.Errorf("PROMPT_ARCHIVE_DB_PATH is required when prompt archive is enabled and no user directory is available")
		}
	}

	// Validate required fields
	// Allow missing API key for Ollama (localhost endpoints)
	if cfg.OpenAIAPIKey == "" {
		if !strings.Contains(cfg.OpenAIBaseURL, "localhost") &&
			!strings.Contains(cfg.OpenAIBaseURL, "127.0.0.1") {
			return nil, fmt.Errorf("OPENAI_API_KEY is required (unless using localhost/Ollama)")
		}
		// Set dummy key for Ollama
		cfg.OpenAIAPIKey = "ollama"
		cfg.OpenAIAPIKeyIndex = 0
		cfg.OpenAIAPIKeyLabel = "local"
	}

	return cfg, nil
}

func selectedOpenAIAPIKey(singleKey, keysValue, indexValue string) (string, int, string, error) {
	keysValue = strings.TrimSpace(keysValue)
	if keysValue == "" {
		if strings.TrimSpace(indexValue) != "" && strings.TrimSpace(indexValue) != "1" {
			return "", 0, "", fmt.Errorf("OPENAI_API_KEY_INDEX requires OPENAI_API_KEYS")
		}
		if strings.TrimSpace(singleKey) == "" {
			return "", 0, "", nil
		}
		return singleKey, 1, "key-1", nil
	}
	if strings.TrimSpace(singleKey) != "" {
		return "", 0, "", fmt.Errorf("OPENAI_API_KEY and OPENAI_API_KEYS cannot both be set")
	}
	entries := strings.Split(keysValue, ",")
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		key := strings.TrimSpace(entry)
		if key == "" {
			return "", 0, "", fmt.Errorf("OPENAI_API_KEYS must not contain empty entries")
		}
		keys = append(keys, key)
	}
	index := 1
	if strings.TrimSpace(indexValue) != "" {
		parsed, err := strconv.Atoi(strings.TrimSpace(indexValue))
		if err != nil || parsed < 1 {
			return "", 0, "", fmt.Errorf("OPENAI_API_KEY_INDEX must be a positive integer")
		}
		index = parsed
	}
	if index > len(keys) {
		return "", 0, "", fmt.Errorf("OPENAI_API_KEY_INDEX must be between 1 and %d", len(keys))
	}
	return keys[index-1], index, fmt.Sprintf("key-%d", index), nil
}


func LoadWithDebug(debug bool) (*Config, error) {
	cfg, err := Load()
	if err != nil {
		return nil, err
	}
	cfg.Debug = debug
	if debug {
		cfg.DiagnosticsEnabled = true
		if strings.TrimSpace(cfg.DiagnosticsDBPath) == "" {
			if cacheDir, cacheErr := os.UserCacheDir(); cacheErr == nil && cacheDir != "" {
				cfg.DiagnosticsDBPath = filepath.Join(cacheDir, "claude-code-proxy", "diagnostics.db")
			} else if homeDir, homeErr := os.UserHomeDir(); homeErr == nil && homeDir != "" {
				cfg.DiagnosticsDBPath = filepath.Join(homeDir, ".claude", "proxy-diagnostics.db")
			} else {
				return nil, fmt.Errorf("DIAGNOSTICS_DB_PATH is required when debug diagnostics are enabled")
			}
		}
	}
	return cfg, nil
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvAsBoolOrDefault(key string, defaultValue bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return defaultValue
	}
	return value == "true" || value == "1" || value == "yes"
}

func getEnvAsIntOrDefault(key string, defaultValue int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

func loadContextWindowErrorPatterns() []string {
	value := strings.TrimSpace(os.Getenv("CONTEXT_WINDOW_ERROR_PATTERNS"))
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	patterns := make([]string, 0, len(parts))
	for _, part := range parts {
		pattern := strings.TrimSpace(part)
		if pattern != "" {
			patterns = append(patterns, pattern)
		}
	}
	return patterns
}

func isAllowedContextWindowRewriteStatus(status int) bool {
	switch status {
	case 400, 413, 422:
		return true
	default:
		return false
	}
}

func (c *Config) EffectiveContextWindowErrorPatterns() []string {
	custom := append([]string(nil), c.ContextWindowErrorPatterns...)
	if c.ContextWindowPatternsMode == "override" {
		return custom
	}
	patterns := append([]string(nil), DefaultContextWindowErrorPatterns...)
	return append(patterns, custom...)
}

func getEnvAsDurationOrDefault(key string, defaultValue time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0
	}
	return duration
}

// DetectProvider identifies the provider type using OPENAI_PROVIDER when set,
// otherwise falling back to base URL detection.
func (c *Config) DetectProvider() ProviderType {
	if c.OpenAIProvider != "" {
		return c.OpenAIProvider
	}

	baseURL := strings.ToLower(c.OpenAIBaseURL)

	if strings.Contains(baseURL, "openrouter.ai") {
		return ProviderOpenRouter
	}
	if strings.Contains(baseURL, "api.openai.com") {
		return ProviderOpenAI
	}
	if strings.Contains(baseURL, "localhost") || strings.Contains(baseURL, "127.0.0.1") {
		return ProviderOllama
	}
	return ProviderUnknown
}

func isValidProvider(provider ProviderType) bool {
	switch provider {
	case ProviderOpenRouter, ProviderOpenAI, ProviderOllama, ProviderNewAPI, ProviderGeneric:
		return true
	default:
		return false
	}
}

// ChatCompletionsURL returns the configured OpenAI-compatible chat completions endpoint.
func (c *Config) ChatCompletionsURL() string {
	baseURL := strings.TrimRight(strings.TrimSpace(c.OpenAIBaseURL), "/")
	if strings.HasSuffix(strings.ToLower(baseURL), "/chat/completions") {
		return baseURL
	}
	return baseURL + "/chat/completions"
}

func (c *Config) ImageGenerationsURL() string {
	parsed, err := url.Parse(strings.TrimSpace(c.ImageAPIURL))
	if err != nil {
		return ""
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(strings.ToLower(parsed.Path), "/images/generations") {
		parsed.Path += "/images/generations"
	}
	return parsed.String()
}

func validateImageConfig(cfg *Config) error {
	configured := 0
	for _, value := range []string{cfg.ImageAPIURL, cfg.ImageAPIKey, cfg.ImageModel} {
		if strings.TrimSpace(value) != "" {
			configured++
		}
	}
	if configured == 0 {
		return nil
	}
	if configured != 3 {
		return fmt.Errorf("IMAGE_API_URL, IMAGE_API_KEY, and IMAGE_MODEL must be configured together")
	}
	parsed, err := url.Parse(cfg.ImageAPIURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("IMAGE_API_URL must be an absolute http or https URL without credentials or fragment")
	}
	return nil
}

// IsLocalhost returns true if the base URL points to localhost
func (c *Config) IsLocalhost() bool {
	baseURL := strings.ToLower(c.OpenAIBaseURL)
	return strings.Contains(baseURL, "localhost") || strings.Contains(baseURL, "127.0.0.1")
}


// GetModelCapabilities retrieves cached capabilities for a (provider, model) combination.
// Returns nil if no capabilities are cached yet (first request for this model).
// Thread-safe with read lock.
func GetModelCapabilities(key CacheKey) *ModelCapabilities {
	capabilityCacheMutex.RLock()
	defer capabilityCacheMutex.RUnlock()
	return modelCapabilityCache[key]
}

// SetModelCapabilities caches the capabilities for a (provider, model) combination.
// This is called after detecting what parameters a specific model supports through adaptive retry.
// Thread-safe with write lock.
func SetModelCapabilities(key CacheKey, capabilities *ModelCapabilities) {
	capabilityCacheMutex.Lock()
	defer capabilityCacheMutex.Unlock()
	capabilities.LastChecked = time.Now()
	modelCapabilityCache[key] = capabilities
}

// ShouldUseMaxCompletionTokens determines if we should send max_completion_tokens
// based on cached model capabilities learned through adaptive detection.
// No hardcoded model patterns - tries max_completion_tokens for ALL models on first request.
func (c *Config) ShouldUseMaxCompletionTokens(modelName string) bool {
	// Build cache key for this (provider, model) combination
	key := CacheKey{
		BaseURL: c.OpenAIBaseURL,
		Model:   modelName,
	}

	// Check if we have cached knowledge about this specific model
	caps := GetModelCapabilities(key)
	if caps != nil {
		// Cache hit - use learned capability
		if c.Debug {
			fmt.Printf("[DEBUG] Cache HIT: %s → max_completion_tokens=%v\n",
				modelName, caps.UsesMaxCompletionTokens)
		}
		return caps.UsesMaxCompletionTokens
	}

	// Cache miss - default to trying max_completion_tokens first
	// The retry mechanism in handlers.go will detect if it's not supported
	// and automatically fall back to max_tokens, then cache the result
	if c.Debug {
		fmt.Printf("[DEBUG] Cache MISS: %s → will auto-detect (try max_completion_tokens)\n", modelName)
	}
	return true
}
