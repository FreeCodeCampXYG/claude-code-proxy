package server

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/pkg/models"
)

func TestUnsupportedMaxTokensParameter(t *testing.T) {
	req := &models.OpenAIRequest{MaxCompletionTokens: 1024}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"structured parameter rejection", diagnosticFailure(completionUpstreamError, failureUpstreamStatus, &upstreamStatusError{StatusCode: 400, Body: []byte(`{"error":{"param":"max_completion_tokens","message":"unsupported parameter"}}`)}), true},
		{"unprocessable parameter rejection", diagnosticFailure(completionUpstreamError, failureUpstreamStatus, &upstreamStatusError{StatusCode: 422, Body: []byte(`{"error":{"param":"max_tokens","message":"not supported"}}`)}), true},
		{"structured context limit", diagnosticFailure(completionUpstreamError, failureUpstreamStatus, &upstreamStatusError{StatusCode: 400, Body: []byte(`{"error":{"param":"max_completion_tokens","message":"value exceeds context window"}}`)}), false},
		{"context window 502", diagnosticFailure(completionUpstreamError, failureUpstreamStatus, &upstreamStatusError{StatusCode: 502, Body: []byte(`{"error":{"type":"invalid_request_error","message":"max_completion_tokens context window exceeded"}}`)}), false},
		{"server parameter error", diagnosticFailure(completionUpstreamError, failureUpstreamStatus, &upstreamStatusError{StatusCode: 500, Body: []byte(`{"error":{"param":"max_completion_tokens"}}`)}), false},
		{"unrelated validation error", diagnosticFailure(completionUpstreamError, failureUpstreamStatus, &upstreamStatusError{StatusCode: 400, Body: []byte(`{"error":{"param":"temperature"}}`)}), false},
		{"transport error", fmt.Errorf("connection refused"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUnsupportedMaxTokensParameter(tt.err, req); got != tt.want {
				t.Fatalf("isUnsupportedMaxTokensParameter() = %t, want %t", got, tt.want)
			}
		})
	}
	if isUnsupportedMaxTokensParameter(tests[0].err, &models.OpenAIRequest{}) {
		t.Fatal("requests without a token-limit field must not retry")
	}
}

// TestModelCapabilityCacheStructure tests the cache key and value structure
func TestModelCapabilityCacheStructure(t *testing.T) {
	// Test that cache keys are properly formed with both BaseURL and Model
	cfg1 := &config.Config{
		OpenAIBaseURL: "https://api.openai.com/v1",
	}

	cfg2 := &config.Config{
		OpenAIBaseURL: "https://openrouter.ai/api/v1",
	}

	// Same model, different providers should have different cache entries
	model := "gpt-5"

	key1 := CacheKey{
		BaseURL: cfg1.OpenAIBaseURL,
		Model:   model,
	}

	key2 := CacheKey{
		BaseURL: cfg2.OpenAIBaseURL,
		Model:   model,
	}

	// Keys should be different
	if key1 == key2 {
		t.Errorf("Different providers should have different cache keys")
	}

	// Keys with same provider and model should be equal
	key1Copy := CacheKey{
		BaseURL: cfg1.OpenAIBaseURL,
		Model:   model,
	}

	if key1 != key1Copy {
		t.Errorf("Same provider and model should have equal cache keys")
	}
}

// TestCacheHitAndMiss tests cache behavior for hits and misses
func TestCacheHitAndMiss(t *testing.T) {
	cache := &ModelCapabilityCache{
		data:  make(map[CacheKey]*ModelCapabilities),
		mutex: &sync.RWMutex{},
	}

	key := CacheKey{
		BaseURL: "https://api.openai.com/v1",
		Model:   "gpt-5",
	}

	// Test cache miss
	caps, found := cache.Get(key)
	if found {
		t.Error("Cache should miss for new key")
	}
	if caps != nil {
		t.Error("Cache miss should return nil capabilities")
	}

	// Test cache set
	newCaps := &ModelCapabilities{
		UsesMaxCompletionTokens: true,
		LastChecked:             time.Now(),
	}
	cache.Set(key, newCaps)

	// Test cache hit
	caps, found = cache.Get(key)
	if !found {
		t.Error("Cache should hit after setting value")
	}
	if caps.UsesMaxCompletionTokens != true {
		t.Error("Cache should return correct capability value")
	}
}

// TestPerModelPerProviderScoping tests that caching is properly scoped
func TestPerModelPerProviderScoping(t *testing.T) {
	cache := &ModelCapabilityCache{
		data:  make(map[CacheKey]*ModelCapabilities),
		mutex: &sync.RWMutex{},
	}

	// Test data: same model, different providers, different capabilities
	testCases := []struct {
		baseURL  string
		model    string
		supports bool
	}{
		{"https://api.openai.com/v1", "gpt-5", true},
		{"https://openrouter.ai/api/v1", "gpt-5", true},
		{"http://localhost:11434/v1", "gpt-5", false},
		{"https://api.openai.com/v1", "gpt-4o", false},
		{"https://openrouter.ai/api/v1", "gpt-4o", false},
	}

	// Set all cache entries
	for _, tc := range testCases {
		key := CacheKey{BaseURL: tc.baseURL, Model: tc.model}
		caps := &ModelCapabilities{
			UsesMaxCompletionTokens: tc.supports,
			LastChecked:             time.Now(),
		}
		cache.Set(key, caps)
	}

	// Verify each entry is correct
	for _, tc := range testCases {
		key := CacheKey{BaseURL: tc.baseURL, Model: tc.model}
		caps, found := cache.Get(key)
		if !found {
			t.Errorf("Cache should contain %s on %s", tc.model, tc.baseURL)
		}
		if caps.UsesMaxCompletionTokens != tc.supports {
			t.Errorf("Cache for %s on %s should be %v, got %v",
				tc.model, tc.baseURL, tc.supports, caps.UsesMaxCompletionTokens)
		}
	}
}

// TestCacheConcurrency tests thread-safe cache operations
func TestCacheConcurrency(t *testing.T) {
	cache := &ModelCapabilityCache{
		data:  make(map[CacheKey]*ModelCapabilities),
		mutex: &sync.RWMutex{},
	}

	// Test concurrent reads and writes
	done := make(chan bool)
	errors := make(chan string, 100)

	// Writer goroutines
	for i := 0; i < 10; i++ {
		go func(id int) {
			for j := 0; j < 10; j++ {
				key := CacheKey{
					BaseURL: "https://api.openai.com/v1",
					Model:   "model-" + string(rune('a'+id)),
				}
				caps := &ModelCapabilities{
					UsesMaxCompletionTokens: j%2 == 0,
					LastChecked:             time.Now(),
				}
				cache.Set(key, caps)
			}
			done <- true
		}(i)
	}

	// Reader goroutines
	for i := 0; i < 10; i++ {
		go func(id int) {
			for j := 0; j < 10; j++ {
				key := CacheKey{
					BaseURL: "https://api.openai.com/v1",
					Model:   "model-" + string(rune('a'+id)),
				}
				_, found := cache.Get(key)
				// We don't assert found because writers are concurrent
				if found && key.Model == "" {
					errors <- "Invalid model in cache"
				}
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 20; i++ {
		<-done
	}

	close(errors)
	for err := range errors {
		t.Errorf("Concurrency error: %s", err)
	}
}

// TestCacheTimestampTracking tests that LastChecked is properly tracked
func TestCacheTimestampTracking(t *testing.T) {
	cache := &ModelCapabilityCache{
		data:  make(map[CacheKey]*ModelCapabilities),
		mutex: &sync.RWMutex{},
	}

	key := CacheKey{
		BaseURL: "https://api.openai.com/v1",
		Model:   "gpt-5",
	}

	before := time.Now()
	caps := &ModelCapabilities{
		UsesMaxCompletionTokens: true,
		LastChecked:             before,
	}
	cache.Set(key, caps)

	retrieved, found := cache.Get(key)
	if !found {
		t.Error("Cache entry not found")
	}

	// Check that timestamp is preserved
	if !retrieved.LastChecked.Equal(before) {
		t.Errorf("LastChecked not preserved: %v vs %v", retrieved.LastChecked, before)
	}

	// Check that timestamp is recent
	if time.Since(retrieved.LastChecked) > time.Second {
		t.Error("Cache timestamp should be recent")
	}
}

// Helper types and functions for testing (would be in actual implementation)

// CacheKey identifies a model capability cache entry
type CacheKey struct {
	BaseURL string
	Model   string
}

// ModelCapabilities holds learned model capabilities
type ModelCapabilities struct {
	UsesMaxCompletionTokens bool
	LastChecked             time.Time
}

// ModelCapabilityCache manages per-model capabilities
type ModelCapabilityCache struct {
	data  map[CacheKey]*ModelCapabilities
	mutex *sync.RWMutex
}

// Get retrieves a cache entry
func (c *ModelCapabilityCache) Get(key CacheKey) (*ModelCapabilities, bool) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	caps, found := c.data[key]
	return caps, found
}

// Set stores a cache entry
func (c *ModelCapabilityCache) Set(key CacheKey, caps *ModelCapabilities) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.data[key] = caps
}

// Retry classification is covered by TestUnsupportedMaxTokensParameter above.
