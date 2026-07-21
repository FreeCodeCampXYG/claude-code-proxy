package converter

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/pkg/models"
)

// TestExtractSystemText tests system message extraction from various formats
func TestExtractSystemText(t *testing.T) {
	tests := []struct {
		name     string
		system   interface{}
		expected string
	}{
		{
			name:     "nil system",
			system:   nil,
			expected: "",
		},
		{
			name:     "string system",
			system:   "You are a helpful assistant",
			expected: "You are a helpful assistant",
		},
		{
			name: "array system with single block",
			system: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": "You are Claude Code",
				},
			},
			expected: "You are Claude Code",
		},
		{
			name: "array system with multiple blocks",
			system: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": "You are Claude Code",
				},
				map[string]interface{}{
					"type": "text",
					"text": "Be helpful and concise",
				},
			},
			expected: "You are Claude Code\nBe helpful and concise",
		},
		{
			name: "array system with non-text blocks (should skip)",
			system: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": "First part",
				},
				map[string]interface{}{
					"type": "image",
					"data": "base64...",
				},
				map[string]interface{}{
					"type": "text",
					"text": "Second part",
				},
			},
			expected: "First part\nSecond part",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractSystemText(tt.system)
			if result != tt.expected {
				t.Errorf("extractSystemText() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// TestMapModel tests model routing logic
func TestMapModel(t *testing.T) {
	cfg := &config.Config{
		OpusModel:   "",
		SonnetModel: "",
		HaikuModel:  "",
	}

	tests := []struct {
		name        string
		claudeModel string
		expected    string
	}{
		{
			name:        "haiku model",
			claudeModel: "claude-haiku-3-5-20241022",
			expected:    "gpt-5.6-luna",
		},
		{
			name:        "sonnet-4 model",
			claudeModel: "claude-sonnet-4-20250514",
			expected:    "gpt-5.6-terra",
		},
		{
			name:        "sonnet-5 model",
			claudeModel: "claude-sonnet-5-20250101",
			expected:    "gpt-5.6-terra",
		},
		{
			name:        "sonnet-3 model",
			claudeModel: "claude-3-5-sonnet-20241022",
			expected:    "gpt-5.6-terra", // All sonnets now map to gpt-5.6-terra
		},
		{
			name:        "opus model",
			claudeModel: "claude-opus-4-20250514",
			expected:    "gpt-5.6-sol",
		},
		{
			name:        "non-claude model (passthrough)",
			claudeModel: "gpt-4o",
			expected:    "gpt-4o",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mapModel(tt.claudeModel, cfg)
			if result != tt.expected {
				t.Errorf("mapModel(%q) = %q, want %q", tt.claudeModel, result, tt.expected)
			}
		})
	}
}

// TestMapModelWithOverrides tests model routing with env overrides
func TestMapModelWithOverrides(t *testing.T) {
	cfg := &config.Config{
		OpusModel:   "custom-opus-model",
		SonnetModel: "custom-sonnet-model",
		HaikuModel:  "custom-haiku-model",
	}

	tests := []struct {
		name        string
		claudeModel string
		expected    string
	}{
		{
			name:        "haiku with override",
			claudeModel: "claude-haiku-3-5-20241022",
			expected:    "custom-haiku-model",
		},
		{
			name:        "sonnet with override",
			claudeModel: "claude-sonnet-4-20250514",
			expected:    "custom-sonnet-model",
		},
		{
			name:        "opus with override",
			claudeModel: "claude-opus-4-20250514",
			expected:    "custom-opus-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mapModel(tt.claudeModel, cfg)
			if result != tt.expected {
				t.Errorf("mapModel(%q) = %q, want %q", tt.claudeModel, result, tt.expected)
			}
		})
	}
}

// TestConvertRequest tests full request conversion
func TestConvertRequest(t *testing.T) {
	cfg := &config.Config{
		OpusModel:   "",
		SonnetModel: "",
		HaikuModel:  "",
	}

	t.Run("simple request with string system", func(t *testing.T) {
		temp := 0.7
		stream := false
		claudeReq := models.ClaudeRequest{
			Model:     "claude-sonnet-4-20250514",
			MaxTokens: 1000,
			System:    "You are helpful",
			Messages: []models.ClaudeMessage{
				{
					Role:    "user",
					Content: "Hello",
				},
			},
			Temperature: &temp,
			Stream:      &stream,
		}

		openaiReq, err := ConvertRequest(claudeReq, cfg)
		if err != nil {
			t.Fatalf("ConvertRequest() error = %v", err)
		}

		if openaiReq.Model != "gpt-5.6-terra" {
			t.Errorf("Model = %q, want %q", openaiReq.Model, "gpt-5.6-terra")
		}

		if len(openaiReq.Messages) != 2 {
			t.Errorf("Messages length = %d, want 2", len(openaiReq.Messages))
		}

		if openaiReq.Messages[0].Role != "system" {
			t.Errorf("First message role = %q, want %q", openaiReq.Messages[0].Role, "system")
		}

		if openaiReq.MaxCompletionTokens != 1000 {
			t.Errorf("MaxCompletionTokens = %d, want 1000", openaiReq.MaxCompletionTokens)
		}

		if *openaiReq.Temperature != temp {
			t.Errorf("Temperature = %f, want %f", *openaiReq.Temperature, temp)
		}
	})

	t.Run("request with array system", func(t *testing.T) {
		claudeReq := models.ClaudeRequest{
			Model:     "claude-sonnet-4-20250514",
			MaxTokens: 1000,
			System: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": "Part 1",
				},
				map[string]interface{}{
					"type": "text",
					"text": "Part 2",
				},
			},
			Messages: []models.ClaudeMessage{
				{
					Role:    "user",
					Content: "Hello",
				},
			},
		}

		openaiReq, err := ConvertRequest(claudeReq, cfg)
		if err != nil {
			t.Fatalf("ConvertRequest() error = %v", err)
		}

		systemContent, ok := openaiReq.Messages[0].Content.(string)
		if !ok {
			t.Fatal("System message content is not a string")
		}

		expected := "Part 1\nPart 2"
		if systemContent != expected {
			t.Errorf("System content = %q, want %q", systemContent, expected)
		}
	})

	t.Run("request with tools", func(t *testing.T) {
		claudeReq := models.ClaudeRequest{
			Model:     "claude-sonnet-4-20250514",
			MaxTokens: 1000,
			Messages: []models.ClaudeMessage{
				{
					Role:    "user",
					Content: "Hello",
				},
			},
			Tools: []models.Tool{
				{
					Name:        "get_weather",
					Description: "Get weather information",
					InputSchema: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"location": map[string]interface{}{
								"type": "string",
							},
						},
					},
				},
			},
		}

		openaiReq, err := ConvertRequest(claudeReq, cfg)
		if err != nil {
			t.Fatalf("ConvertRequest() error = %v", err)
		}

		if len(openaiReq.Tools) != 1 {
			t.Fatalf("Tools length = %d, want 1", len(openaiReq.Tools))
		}

		if openaiReq.Tools[0].Type != "function" {
			t.Errorf("Tool type = %q, want %q", openaiReq.Tools[0].Type, "function")
		}

		if openaiReq.Tools[0].Function.Name != "get_weather" {
			t.Errorf("Tool name = %q, want %q", openaiReq.Tools[0].Function.Name, "get_weather")
		}
	})

	t.Run("request with GPT-compatible serial tools", func(t *testing.T) {
		claudeReq := models.ClaudeRequest{
			Model:     "claude-sonnet-4-20250514",
			MaxTokens: 1024,
			Messages:  []models.ClaudeMessage{{Role: "user", Content: "Hello"}},
			Tools: []models.Tool{{
				Name:        "get_weather",
				Description: "Get weather information",
				InputSchema: map[string]interface{}{"type": "object"},
			}},
		}
		openaiReq, err := ConvertRequest(claudeReq, &config.Config{DisableParallelToolCalls: true})
		if err != nil {
			t.Fatalf("ConvertRequest() error = %v", err)
		}
		if openaiReq.ParallelToolCalls == nil || *openaiReq.ParallelToolCalls {
			t.Fatalf("ParallelToolCalls = %#v, want false", openaiReq.ParallelToolCalls)
		}
	})
}

// TestConvertResponse tests OpenAI → Claude response conversion
func TestConvertResponse(t *testing.T) {
	t.Run("simple text response", func(t *testing.T) {
		finishReason := "stop"
		openaiResp := &models.OpenAIResponse{
			ID:      "chatcmpl-123",
			Object:  "chat.completion",
			Created: 1234567890,
			Model:   "gpt-5",
			Choices: []models.OpenAIChoice{
				{
					Index: 0,
					Message: models.OpenAIMessage{
						Role:    "assistant",
						Content: "Hello! How can I help you?",
					},
					FinishReason: &finishReason,
				},
			},
			Usage: models.OpenAIUsage{
				PromptTokens:     10,
				CompletionTokens: 20,
				TotalTokens:      30,
			},
		}

		claudeResp, err := ConvertResponse(openaiResp, "claude-sonnet-4-20250514")
		if err != nil {
			t.Fatalf("ConvertResponse() error = %v", err)
		}

		if claudeResp.ID != "chatcmpl-123" {
			t.Errorf("ID = %q, want %q", claudeResp.ID, "chatcmpl-123")
		}

		if claudeResp.Model != "claude-sonnet-4-20250514" {
			t.Errorf("Model = %q, want %q", claudeResp.Model, "claude-sonnet-4-20250514")
		}

		if len(claudeResp.Content) != 1 {
			t.Fatalf("Content length = %d, want 1", len(claudeResp.Content))
		}

		if claudeResp.Content[0].Type != "text" {
			t.Errorf("Content type = %q, want %q", claudeResp.Content[0].Type, "text")
		}

		if claudeResp.Content[0].Text != "Hello! How can I help you?" {
			t.Errorf("Content text = %q, want %q", claudeResp.Content[0].Text, "Hello! How can I help you?")
		}

		if *claudeResp.StopReason != "end_turn" {
			t.Errorf("StopReason = %q, want %q", *claudeResp.StopReason, "end_turn")
		}

		if claudeResp.Usage.InputTokens != 10 {
			t.Errorf("InputTokens = %d, want 10", claudeResp.Usage.InputTokens)
		}

		if claudeResp.Usage.OutputTokens != 20 {
			t.Errorf("OutputTokens = %d, want 20", claudeResp.Usage.OutputTokens)
		}
	})

	t.Run("response with tool calls", func(t *testing.T) {
		finishReason := "tool_calls"
		openaiResp := &models.OpenAIResponse{
			ID:      "chatcmpl-456",
			Object:  "chat.completion",
			Created: 1234567890,
			Model:   "gpt-5",
			Choices: []models.OpenAIChoice{
				{
					Index: 0,
					Message: models.OpenAIMessage{
						Role:    "assistant",
						Content: "",
						ToolCalls: []models.OpenAIToolCall{
							{
								ID:   "call_123",
								Type: "function",
								Function: struct {
									Name      string `json:"name"`
									Arguments string `json:"arguments"`
								}{
									Name:      "get_weather",
									Arguments: `{"location":"San Francisco"}`,
								},
							},
						},
					},
					FinishReason: &finishReason,
				},
			},
			Usage: models.OpenAIUsage{
				PromptTokens:     15,
				CompletionTokens: 25,
				TotalTokens:      40,
			},
		}

		claudeResp, err := ConvertResponse(openaiResp, "claude-sonnet-4-20250514")
		if err != nil {
			t.Fatalf("ConvertResponse() error = %v", err)
		}

		if len(claudeResp.Content) != 1 {
			t.Fatalf("Content length = %d, want 1", len(claudeResp.Content))
		}

		if claudeResp.Content[0].Type != "tool_use" {
			t.Errorf("Content type = %q, want %q", claudeResp.Content[0].Type, "tool_use")
		}

		if claudeResp.Content[0].ID != "call_123" {
			t.Errorf("Tool call ID = %q, want %q", claudeResp.Content[0].ID, "call_123")
		}

		if claudeResp.Content[0].Name != "get_weather" {
			t.Errorf("Tool name = %q, want %q", claudeResp.Content[0].Name, "get_weather")
		}

		if *claudeResp.StopReason != "tool_use" {
			t.Errorf("StopReason = %q, want %q", *claudeResp.StopReason, "tool_use")
		}
	})
}

// TestConvertFinishReason tests finish reason mapping
func TestConvertFinishReason(t *testing.T) {
	tests := []struct {
		openaiReason string
		claudeReason string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"content_filter", "end_turn"},
		{"unknown", "end_turn"},
	}

	for _, tt := range tests {
		t.Run(tt.openaiReason, func(t *testing.T) {
			// Create a mock response to test the conversion
			openaiResp := &models.OpenAIResponse{
				ID: "test",
				Choices: []models.OpenAIChoice{
					{
						Index: 0,
						Message: models.OpenAIMessage{
							Role:    "assistant",
							Content: "test",
						},
						FinishReason: &tt.openaiReason,
					},
				},
				Usage: models.OpenAIUsage{},
			}

			claudeResp, err := ConvertResponse(openaiResp, "test-model")
			if err != nil {
				t.Fatalf("ConvertResponse() error = %v", err)
			}

			if *claudeResp.StopReason != tt.claudeReason {
				t.Errorf("finish reason %q mapped to %q, want %q",
					tt.openaiReason, *claudeResp.StopReason, tt.claudeReason)
			}
		})
	}
}

// TestConvertMessagesWithComplexContent tests message conversion with arrays
func TestConvertMessagesWithComplexContent(t *testing.T) {
	t.Run("message with array content", func(t *testing.T) {
		messages := []models.ClaudeMessage{
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{
						"type": "text",
						"text": "Hello",
					},
					map[string]interface{}{
						"type": "text",
						"text": "How are you?",
					},
				},
			},
		}

		result := convertMessages(messages, "")

		if len(result) != 1 {
			t.Fatalf("Expected 1 message, got %d", len(result))
		}

		contentStr, ok := result[0].Content.(string)
		if !ok {
			t.Fatal("Content is not a string")
		}

		expected := "Hello\nHow are you?"
		if contentStr != expected {
			t.Errorf("Content = %q, want %q", contentStr, expected)
		}
	})

	t.Run("message with tool_result", func(t *testing.T) {
		messages := []models.ClaudeMessage{
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "call_123",
						"content":     "Weather is sunny",
					},
				},
			},
		}

		result := convertMessages(messages, "")

		if len(result) != 1 {
			t.Fatalf("Expected 1 message, got %d", len(result))
		}

		if result[0].Role != "tool" {
			t.Errorf("Role = %q, want %q", result[0].Role, "tool")
		}

		if result[0].ToolCallID != "call_123" {
			t.Errorf("ToolCallID = %q, want %q", result[0].ToolCallID, "call_123")
		}

		contentStr, ok := result[0].Content.(string)
		if !ok {
			t.Fatal("Content is not a string")
		}

		if contentStr != "Weather is sunny" {
			t.Errorf("Content = %q, want %q", contentStr, "Weather is sunny")
		}
	})

	t.Run("message with tool_use blocks", func(t *testing.T) {
		messages := []models.ClaudeMessage{
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{
						"type": "tool_use",
						"id":   "call_456",
						"name": "get_weather",
						"input": map[string]interface{}{
							"location": "San Francisco",
						},
					},
				},
			},
		}

		result := convertMessages(messages, "")

		if len(result) != 1 {
			t.Fatalf("Expected 1 message, got %d", len(result))
		}

		if result[0].Role != "assistant" {
			t.Errorf("Role = %q, want %q", result[0].Role, "assistant")
		}

		if len(result[0].ToolCalls) != 1 {
			t.Fatalf("Expected 1 tool call, got %d", len(result[0].ToolCalls))
		}

		toolCall := result[0].ToolCalls[0]
		if toolCall.ID != "call_456" {
			t.Errorf("ToolCall ID = %q, want %q", toolCall.ID, "call_456")
		}

		if toolCall.Type != "function" {
			t.Errorf("ToolCall Type = %q, want %q", toolCall.Type, "function")
		}

		if toolCall.Function.Name != "get_weather" {
			t.Errorf("Function Name = %q, want %q", toolCall.Function.Name, "get_weather")
		}

		if toolCall.Function.Arguments != `{"location":"San Francisco"}` {
			t.Errorf("Function Arguments = %q, want %q", toolCall.Function.Arguments, `{"location":"San Francisco"}`)
		}
	})

	t.Run("message with mixed text and tool_use blocks", func(t *testing.T) {
		messages := []models.ClaudeMessage{
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{
						"type": "text",
						"text": "Let me check the weather for you.",
					},
					map[string]interface{}{
						"type": "tool_use",
						"id":   "call_789",
						"name": "get_weather",
						"input": map[string]interface{}{
							"location": "New York",
						},
					},
				},
			},
		}

		result := convertMessages(messages, "")

		if len(result) != 1 {
			t.Fatalf("Expected 1 message, got %d", len(result))
		}

		contentStr, ok := result[0].Content.(string)
		if !ok {
			t.Fatal("Content is not a string")
		}

		if contentStr != "Let me check the weather for you." {
			t.Errorf("Content = %q, want %q", contentStr, "Let me check the weather for you.")
		}

		if len(result[0].ToolCalls) != 1 {
			t.Fatalf("Expected 1 tool call, got %d", len(result[0].ToolCalls))
		}

		if result[0].ToolCalls[0].ID != "call_789" {
			t.Errorf("ToolCall ID = %q, want %q", result[0].ToolCalls[0].ID, "call_789")
		}
	})

	t.Run("complete tool call cycle", func(t *testing.T) {
		// Simulates: assistant calls tool → user provides result
		messages := []models.ClaudeMessage{
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{
						"type": "text",
						"text": "I'll get the weather for you.",
					},
					map[string]interface{}{
						"type": "tool_use",
						"id":   "call_abc",
						"name": "get_weather",
						"input": map[string]interface{}{
							"location": "London",
						},
					},
				},
			},
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "call_abc",
						"content":     "Temperature: 15°C, Cloudy",
					},
				},
			},
		}

		result := convertMessages(messages, "")

		if len(result) != 2 {
			t.Fatalf("Expected 2 messages, got %d", len(result))
		}

		// First message: assistant with tool call
		if result[0].Role != "assistant" {
			t.Errorf("First message role = %q, want %q", result[0].Role, "assistant")
		}

		if len(result[0].ToolCalls) != 1 {
			t.Fatalf("Expected 1 tool call, got %d", len(result[0].ToolCalls))
		}

		if result[0].ToolCalls[0].ID != "call_abc" {
			t.Errorf("ToolCall ID = %q, want %q", result[0].ToolCalls[0].ID, "call_abc")
		}

		// Second message: tool result
		if result[1].Role != "tool" {
			t.Errorf("Second message role = %q, want %q", result[1].Role, "tool")
		}

		if result[1].ToolCallID != "call_abc" {
			t.Errorf("ToolCallID = %q, want %q", result[1].ToolCallID, "call_abc")
		}

		contentStr, ok := result[1].Content.(string)
		if !ok {
			t.Fatal("Tool result content is not a string")
		}

		if contentStr != "Temperature: 15°C, Cloudy" {
			t.Errorf("Tool result content = %q, want %q", contentStr, "Temperature: 15°C, Cloudy")
		}
	})

	t.Run("multiple tool_use blocks in one message", func(t *testing.T) {
		messages := []models.ClaudeMessage{
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{
						"type": "tool_use",
						"id":   "call_1",
						"name": "get_weather",
						"input": map[string]interface{}{
							"location": "Tokyo",
						},
					},
					map[string]interface{}{
						"type": "tool_use",
						"id":   "call_2",
						"name": "get_time",
						"input": map[string]interface{}{
							"timezone": "Asia/Tokyo",
						},
					},
				},
			},
		}

		result := convertMessages(messages, "")

		if len(result) != 1 {
			t.Fatalf("Expected 1 message, got %d", len(result))
		}

		if len(result[0].ToolCalls) != 2 {
			t.Fatalf("Expected 2 tool calls, got %d", len(result[0].ToolCalls))
		}

		if result[0].ToolCalls[0].Function.Name != "get_weather" {
			t.Errorf("First tool name = %q, want %q", result[0].ToolCalls[0].Function.Name, "get_weather")
		}

		if result[0].ToolCalls[1].Function.Name != "get_time" {
			t.Errorf("Second tool name = %q, want %q", result[0].ToolCalls[1].Function.Name, "get_time")
		}
	})
}

func TestRouterCostAwareRouting(t *testing.T) {
	cfg := routerTestConfig(t, config.RouterConfig{
		Enabled:     true,
		Defaults:    config.RouterRule{Enabled: true, Model: "gpt-5.5", Effort: "medium"},
		Simple:      config.RouterRule{Enabled: true, Model: "gpt-5.4", Effort: "low", MaxChars: 4000},
		ToolUse:     config.RouterRule{Enabled: true, Model: "gpt-5.5", Effort: "medium"},
		ToolResult:  config.RouterRule{Enabled: true, Model: "gpt-5.5", Effort: "low"},
		LongContext: config.RouterRule{Enabled: true, Model: "gpt-5.6-terra", Effort: "medium", MinChars: 10000},
	})

	t.Run("historical tool_result does not poison later simple turn", func(t *testing.T) {
		req, err := ConvertRequest(models.ClaudeRequest{
			Model: "claude-sonnet-4",
			MaxTokens: 1024,
			Messages: []models.ClaudeMessage{
				{Role: "user", Content: "read a file"},
				{Role: "assistant", Content: []interface{}{map[string]interface{}{"type": "tool_use", "id": "call-1", "name": "read", "input": map[string]interface{}{"path": "a.txt"}}}}},
				{Role: "user", Content: []interface{}{map[string]interface{}{"type": "tool_result", "tool_use_id": "call-1", "content": "file content"}}},
				{Role: "assistant", Content: "done"},
				{Role: "user", Content: "2+2?"},
			},
		}, cfg)
		if err != nil {
			t.Fatalf("ConvertRequest() error = %v", err)
		}
		assertRoute(t, req, "simple", "gpt-5.4")
	})

	t.Run("immediate tool_result routes to tool_result", func(t *testing.T) {
		req, err := ConvertRequest(models.ClaudeRequest{
			Model: "claude-sonnet-4",
			MaxTokens: 1024,
			Messages: []models.ClaudeMessage{{Role: "user", Content: []interface{}{map[string]interface{}{"type": "tool_result", "tool_use_id": "call-1", "content": "test output"}}}},
		}, cfg)
		if err != nil {
			t.Fatalf("ConvertRequest() error = %v", err)
		}
		assertRoute(t, req, "tool_result", "gpt-5.5")
	})

	t.Run("available tool schemas alone do not route to tool_use", func(t *testing.T) {
		req, err := ConvertRequest(models.ClaudeRequest{
			Model:     "claude-sonnet-4",
			MaxTokens: 1024,
			Messages:  []models.ClaudeMessage{{Role: "user", Content: "hello"}},
			Tools: []models.Tool{{
				Name:        "read_file",
				Description: "Read a file",
				InputSchema: map[string]interface{}{"type": "object"},
			}},
		}, cfg)
		if err != nil {
			t.Fatalf("ConvertRequest() error = %v", err)
		}
		assertRoute(t, req, "simple", "gpt-5.4")
	})

	t.Run("long context beats simple and tool_result", func(t *testing.T) {
		longCfg := config.RouterConfig{
			Enabled:     true,
			Defaults:    config.RouterRule{Enabled: true, Model: "gpt-5.5", Effort: "medium"},
			Simple:      config.RouterRule{Enabled: true, Model: "gpt-5.4", Effort: "low", MaxChars: 4000},
			ToolUse:     config.RouterRule{Enabled: true, Model: "gpt-5.5", Effort: "medium"},
			ToolResult:  config.RouterRule{Enabled: true, Model: "gpt-5.5", Effort: "low"},
			LongContext: config.RouterRule{Enabled: true, Model: "gpt-5.6-terra", Effort: "medium", MinChars: 120},
		}
		longText := strings.Repeat("x", 140)
		req, err := ConvertRequest(models.ClaudeRequest{
			Model: "claude-sonnet-4",
			MaxTokens: 1024,
			Messages: []models.ClaudeMessage{{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": longText},
				map[string]interface{}{"type": "tool_result", "tool_use_id": "call-1", "content": "short output"},
			}}},
		}, routerTestConfig(t, longCfg))
		if err != nil {
			t.Fatalf("ConvertRequest() error = %v", err)
		}
		assertRoute(t, req, "long_context", "gpt-5.6-terra")
	})

	t.Run("default fallback uses main workhorse", func(t *testing.T) {
		req, err := ConvertRequest(models.ClaudeRequest{
			Model:     "claude-sonnet-4",
			MaxTokens: 1024,
			Messages:  []models.ClaudeMessage{{Role: "user", Content: strings.Repeat("normal task ", 500)}},
		}, cfg)
		if err != nil {
			t.Fatalf("ConvertRequest() error = %v", err)
		}
		assertRoute(t, req, "default", "gpt-5.5")
	})

	t.Run("incoming sol model is preserved without inferred effort", func(t *testing.T) {
		cases := []struct {
			name    string
			content string
		}{
			{name: "small", content: "hello"},
			{name: "medium", content: strings.Repeat("x", 5000)},
			{name: "high", content: strings.Repeat("x", 130000)},
			{name: "xhigh", content: strings.Repeat("x", 310000)},
			{name: "max", content: strings.Repeat("x", 810000)},
		}
		for _, tt := range cases {
			t.Run(tt.name, func(t *testing.T) {
				req, err := ConvertRequest(models.ClaudeRequest{
					Model:     "gpt-5.6-sol",
					MaxTokens: 1024,
					Messages:  []models.ClaudeMessage{{Role: "user", Content: tt.content}},
				}, cfg)
				if err != nil {
					t.Fatalf("ConvertRequest() error = %v", err)
				}
				if req.Model != "gpt-5.6-sol" {
					t.Fatalf("Model = %q, want incoming sol model preserved", req.Model)
				}
				if req.RoutedEffort != "" || req.ReasoningEffort != "" {
					t.Fatalf("effort = routed %q reasoning %q, want omitted", req.RoutedEffort, req.ReasoningEffort)
				}
				if req.RouteModelOverridden {
					t.Fatalf("RouteModelOverridden = true, want false when sol model is preserved")
				}
				if req.RouteEffortOverridden {
					t.Fatalf("RouteEffortOverridden = true, want false for transparent NewAPI effort")
				}
			})
		}
	})

	t.Run("incoming sol explicit effort is preserved for huge requests", func(t *testing.T) {
		for _, effort := range []string{"high", "max"} {
			t.Run(effort, func(t *testing.T) {
				req, err := ConvertRequest(models.ClaudeRequest{
					Model:        "gpt-5.6-sol",
					MaxTokens:    1024,
					Messages:     []models.ClaudeMessage{{Role: "user", Content: strings.Repeat("x", 810000)}},
					OutputConfig: &models.ClaudeOutputConfig{Effort: effort},
				}, cfg)
				if err != nil {
					t.Fatalf("ConvertRequest() error = %v", err)
				}
				if req.Model != "gpt-5.6-sol" {
					t.Fatalf("Model = %q, want incoming sol model preserved", req.Model)
				}
				if req.ReasoningEffort != effort || req.RoutedEffort != effort {
					t.Fatalf("effort = routed %q reasoning %q, want explicit %q", req.RoutedEffort, req.ReasoningEffort, effort)
				}
				if req.RouteEffortOverridden {
					t.Fatalf("RouteEffortOverridden = true, want false for explicit transparent effort")
				}
			})
		}
	})

	t.Run("router disabled preserves incoming sol model and explicit effort", func(t *testing.T) {
		req, err := ConvertRequest(models.ClaudeRequest{
			Model:        "gpt-5.6-sol",
			MaxTokens:    1024,
			Messages:     []models.ClaudeMessage{{Role: "user", Content: strings.Repeat("x", 810000)}},
			OutputConfig: &models.ClaudeOutputConfig{Effort: "xhigh"},
		}, &config.Config{OpenAIBaseURL: "https://newapi.example.com/v1", OpenAIProvider: config.ProviderNewAPI})
		if err != nil {
			t.Fatalf("ConvertRequest() error = %v", err)
		}
		if req.Model != "gpt-5.6-sol" {
			t.Fatalf("Model = %q, want incoming sol model preserved", req.Model)
		}
		if req.ReasoningEffort != "xhigh" || req.RoutedEffort != "xhigh" {
			t.Fatalf("effort = routed %q reasoning %q, want explicit xhigh", req.RoutedEffort, req.ReasoningEffort)
		}
		if req.RouteModelOverridden || req.RouteEffortOverridden {
			t.Fatalf("route override flags = model:%v effort:%v, want both false when router is disabled", req.RouteModelOverridden, req.RouteEffortOverridden)
		}
	})
}

func routerTestConfig(t *testing.T, routerCfg config.RouterConfig) *config.Config {
	t.Helper()
	manager, err := config.NewRouterManager(filepath.Join(t.TempDir(), "router.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Save(routerCfg); err != nil {
		t.Fatal(err)
	}
	return &config.Config{OpenAIBaseURL: "https://newapi.example.com/v1", OpenAIProvider: config.ProviderNewAPI, Router: manager}
}

// assertRoute verifies that the request was routed to the expected rule and model.
// Router rules now enforce minimum effort; effort assertions are done in dedicated tests.
func assertRoute(t *testing.T, req *models.OpenAIRequest, rule, model string) {
	t.Helper()
	if req.RouteRule != rule {
		t.Fatalf("RouteRule = %q, want %q", req.RouteRule, rule)
	}
	if req.Model != model {
		t.Fatalf("Model = %q, want %q", req.Model, model)
	}
}

// Benchmark tests
func BenchmarkExtractSystemText(b *testing.B) {
	system := []interface{}{
		map[string]interface{}{
			"type": "text",
			"text": "You are Claude Code",
		},
		map[string]interface{}{
			"type": "text",
			"text": "Be helpful and concise",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		extractSystemText(system)
	}
}

func BenchmarkConvertRequest(b *testing.B) {
	cfg := &config.Config{}
	claudeReq := models.ClaudeRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1000,
		System:    "You are helpful",
		Messages: []models.ClaudeMessage{
			{
				Role:    "user",
				Content: "Hello",
			},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ConvertRequest(claudeReq, cfg)
	}
}

func BenchmarkConvertResponse(b *testing.B) {
	finishReason := "stop"
	openaiResp := &models.OpenAIResponse{
		ID: "test",
		Choices: []models.OpenAIChoice{
			{
				Index: 0,
				Message: models.OpenAIMessage{
					Role:    "assistant",
					Content: "Hello! How can I help you?",
				},
				FinishReason: &finishReason,
			},
		},
		Usage: models.OpenAIUsage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ConvertResponse(openaiResp, "claude-sonnet-4-20250514")
	}
}