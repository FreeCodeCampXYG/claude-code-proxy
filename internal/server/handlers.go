package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/converter"
	"github.com/claude-code-proxy/proxy/internal/diagnostics"
	"github.com/claude-code-proxy/proxy/pkg/models"
	"github.com/gofiber/fiber/v2"
)

// addOpenRouterHeaders adds OpenRouter-specific HTTP headers for better rate limits.
// Sets HTTP-Referer and X-Title headers when configured, which helps with OpenRouter's
// rate limiting and usage tracking.
func addOpenRouterHeaders(req *http.Request, cfg *config.Config) {
	if cfg.OpenRouterAppURL != "" {
		req.Header.Set("HTTP-Referer", cfg.OpenRouterAppURL)
	}
	if cfg.OpenRouterAppName != "" {
		req.Header.Set("X-Title", cfg.OpenRouterAppName)
	}
}

// handleMessages is the main handler for /v1/messages endpoint.
// It parses Claude requests, converts them to OpenAI format, and routes to either
// streaming or non-streaming handlers based on the request's stream parameter.
func handleMessages(c *fiber.Ctx, cfg *config.Config, store *diagnostics.Store) error {
	trace := newDiagnosticsTrace(c, cfg, store)

	var claudeReq models.ClaudeRequest
	if err := c.BodyParser(&claudeReq); err != nil {
		fmt.Printf("[ERROR] Failed to parse request body request_id=%s: %v\n", c.GetRespHeader("X-Request-ID"), err)
		trace.setMalformedBody(c.Body(), c.Get(fiber.HeaderContentType), err)
		trace.finish(fiber.StatusBadRequest, err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"type": "error",
			"error": fiber.Map{
				"type":    "invalid_request_error",
				"message": fmt.Sprintf("Invalid request body: %v", err),
			},
		})
	}

	if cfg.AnthropicAPIKey != "" && c.Get("x-api-key") != cfg.AnthropicAPIKey {
		err := fmt.Errorf("invalid API key")
		trace.finish(fiber.StatusUnauthorized, err)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"type": "error",
			"error": fiber.Map{
				"type":    "authentication_error",
				"message": "Invalid API key",
			},
		})
	}

	openaiReq, err := converter.ConvertRequest(claudeReq, cfg)
	if err != nil {
		trace.finish(fiber.StatusBadRequest, err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"type": "error",
			"error": fiber.Map{
				"type":    "invalid_request_error",
				"message": err.Error(),
			},
		})
	}

	streaming := openaiReq.Stream != nil && *openaiReq.Stream
	trace.setModel(openaiReq.Model, streaming)
	if cfg.Debug {
		fmt.Printf("[DEBUG] request_id=%s provider=%s model=%s streaming=%t tools=%d\n",
			c.GetRespHeader("X-Request-ID"), cfg.DetectProvider(), openaiReq.Model, streaming, len(openaiReq.Tools))
	}

	if streaming {
		return handleStreamingMessages(c, openaiReq, cfg, trace)
	}

	startTime := time.Now()
	openaiResp, err := callOpenAI(openaiReq, cfg, trace)
	if err != nil {
		trace.setResponseBody([]byte(errorJSON(err)))
		trace.finish(fiber.StatusBadGateway, err)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"type": "error",
			"error": fiber.Map{
				"type":    "api_error",
				"message": fmt.Sprintf("OpenAI API error: %v", err),
			},
		})
	}

	claudeResp, err := converter.ConvertResponse(openaiResp, claudeReq.Model)
	if err != nil {
		trace.finish(fiber.StatusInternalServerError, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"type": "error",
			"error": fiber.Map{
				"type":    "api_error",
				"message": fmt.Sprintf("Response conversion error: %v", err),
			},
		})
	}
	if encoded, marshalErr := json.Marshal(openaiResp); marshalErr == nil {
		trace.setResponseBody(encoded)
	}
	trace.finish(fiber.StatusOK, nil)

	if cfg.Debug {
		fmt.Printf("[DEBUG] request_id=%s completed status=%d content_blocks=%d\n",
			c.GetRespHeader("X-Request-ID"), fiber.StatusOK, len(claudeResp.Content))
	}
	if cfg.SimpleLog {
		duration := time.Since(startTime).Seconds()
		tokensPerSec := 0.0
		if duration > 0 && claudeResp.Usage.OutputTokens > 0 {
			tokensPerSec = float64(claudeResp.Usage.OutputTokens) / duration
		}
		timestamp := time.Now().Format("15:04:05")
		fmt.Printf("[%s] [REQ] %s model=%s in=%d out=%d tok/s=%.1f\n",
			timestamp, cfg.OpenAIBaseURL, openaiReq.Model, claudeResp.Usage.InputTokens,
			claudeResp.Usage.OutputTokens, tokensPerSec)
	}

	return c.JSON(claudeResp)
}

// handleStreamingMessages handles streaming SSE responses from the provider.
// It forwards the OpenAI request, receives streaming chunks, and converts them to
// Claude's SSE event format in real-time using streamOpenAIToClaude.
func handleStreamingMessages(c *fiber.Ctx, openaiReq *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace) error {
	startTime := time.Now()

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		trace.stage("stream_started", "")
		resp, err := callOpenAIStream(openaiReq, cfg, trace)
		if err != nil {
			writeSSEError(w, fmt.Sprintf("streaming request failed: %v", err))
			trace.setResponseBody([]byte(errorJSON(err)))
			trace.finish(fiber.StatusBadGateway, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		outcome := streamOpenAIToClaude(w, resp.Body, openaiReq.Model, cfg, startTime)
		responseMetadata, _ := json.Marshal(map[string]interface{}{
			"streaming": true,
			"chunks": outcome.Chunks,
			"stop_reason": outcome.StopReason,
			"usage": outcome.Usage,
		})
		trace.setResponseBody(responseMetadata)
		if outcome.Err != nil {
			trace.finish(fiber.StatusBadGateway, outcome.Err)
			return
		}
		trace.finish(fiber.StatusOK, nil)
	})

	return nil
}

// ToolCallState tracks the state of a tool call during streaming
type ToolCallState struct {
	ID          string // Tool call ID from OpenAI
	Name        string // Function name
	ArgsBuffer  string // Accumulated JSON arguments
	JSONSent    bool   // Flag if we sent the JSON delta
	ClaudeIndex int    // The content block index for Claude
	Started     bool   // Flag if content_block_start was sent
}

// streamOpenAIToClaude converts OpenAI streaming responses to Claude's SSE event format.
//
// It processes the OpenAI SSE stream chunk-by-chunk, generating the proper sequence of
// Claude events: message_start, content_block_start, content_block_delta, content_block_stop,
// message_delta, and message_stop.
//
// Handles:
//   - Thinking blocks from reasoning models (OpenRouter's reasoning_details, OpenAI's reasoning_content)
//   - Text content deltas
//   - Tool call deltas (accumulates JSON arguments across chunks)
//   - Token usage tracking and throughput calculation for simple log mode
//
// The function maintains state to track content block indices, tool call accumulation,
// and ensures proper event ordering for Claude Code compatibility.
type streamOutcome struct {
	Chunks     int
	StopReason string
	Usage      map[string]interface{}
	Err        error
}

func streamOpenAIToClaude(w *bufio.Writer, reader io.Reader, providerModel string, cfg *config.Config, startTime time.Time) streamOutcome {
	if cfg.Debug {
		fmt.Printf("[DEBUG] streamOpenAIToClaude: Starting conversion\n")
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // Increase buffer size

	// State variables
	messageID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	textBlockIndex := 1                              // Text block is index 1 (thinking is 0)
	toolBlockCounter := 2                            // Tool calls start at index 2
	currentToolCalls := make(map[int]*ToolCallState)
	finalStopReason := "end_turn"
	chunkCount := 0
	usageData := map[string]interface{}{
		"input_tokens":                0,
		"output_tokens":               0,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
		"cache_creation": map[string]interface{}{
			"ephemeral_5m_input_tokens": 0,
			"ephemeral_1h_input_tokens": 0,
		},
	}

	// Thinking block tracking (to show thinking indicator in Claude Code)
	thinkingBlockIndex := 0 // Thinking block is always index 0
	thinkingBlockStarted := false
	thinkingBlockHasContent := false
	textBlockStarted := false // Track if we've sent text block_start

	// Send initial SSE events
	writeSSEEvent(w, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         providerModel,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":                0,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
				"cache_creation": map[string]interface{}{
					"ephemeral_5m_input_tokens": 0,
					"ephemeral_1h_input_tokens": 0,
				},
			},
		},
	})

	writeSSEEvent(w, "ping", map[string]interface{}{
		"type": "ping",
	})

	_ = w.Flush()

	// Process streaming chunks
	for scanner.Scan() {
		line := scanner.Text()

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		// Check for [DONE] marker
		if strings.Contains(line, "[DONE]") {
			break
		}

		// Parse data line
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		dataJSON := strings.TrimPrefix(line, "data: ")

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(dataJSON), &chunk); err != nil {
			continue
		}
		chunkCount++

		// Handle usage data
		if usage, ok := chunk["usage"].(map[string]interface{}); ok {
			// Convert float64 to int for token counts (JSON unmarshals numbers as float64)
			inputTokens := 0
			outputTokens := 0
			if val, ok := usage["prompt_tokens"].(float64); ok {
				inputTokens = int(val)
			}
			if val, ok := usage["completion_tokens"].(float64); ok {
				outputTokens = int(val)
			}

			usageData = map[string]interface{}{
				"input_tokens":  inputTokens,
				"output_tokens": outputTokens,
			}

			// Add cache metrics if present
			if promptTokensDetails, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
				if cachedTokens, ok := promptTokensDetails["cached_tokens"].(float64); ok && cachedTokens > 0 {
					usageData["cache_read_input_tokens"] = int(cachedTokens)
				}
			}
		}

		// Extract delta from choices
		choices, ok := chunk["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			continue
		}

		choice := choices[0].(map[string]interface{})
		delta, ok := choice["delta"].(map[string]interface{})
		if !ok {
			continue
		}

		// Handle reasoning delta (thinking blocks)
		// Support both OpenRouter and OpenAI formats:
		// - OpenRouter: delta.reasoning_details (array)
		// - OpenAI o1/o3: delta.reasoning_content (string)

		// First, check for OpenAI's reasoning_content format (o1/o3 models)
		if reasoningContent, ok := delta["reasoning_content"].(string); ok && reasoningContent != "" {
			// Send content_block_start for thinking block on first thinking delta
			if !thinkingBlockStarted {
				writeSSEEvent(w, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": thinkingBlockIndex,
					"content_block": map[string]interface{}{
						"type": "thinking",
					},
				})
				thinkingBlockStarted = true
				thinkingBlockHasContent = true
			}

			// Send thinking delta
			writeSSEEvent(w, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": thinkingBlockIndex,
				"delta": map[string]interface{}{
					"type": "thinking_delta",
					"text": reasoningContent,
				},
			})
		}

		// Then, check for OpenRouter's reasoning_details format
		// Only process reasoning_details if we haven't already processed reasoning field
		if reasoningDetailsRaw, ok := delta["reasoning_details"]; ok && delta["reasoning"] == nil {
			if reasoningDetails, ok := reasoningDetailsRaw.([]interface{}); ok && len(reasoningDetails) > 0 {
				for _, detailRaw := range reasoningDetails {
					if detail, ok := detailRaw.(map[string]interface{}); ok {
						// Extract reasoning text from the detail
						thinkingText := ""
						detailType, _ := detail["type"].(string)

						switch detailType {
						case "reasoning.text":
							if text, ok := detail["text"].(string); ok {
								thinkingText = text
							}
						case "reasoning.summary":
							if summary, ok := detail["summary"].(string); ok {
								thinkingText = summary
							}
						case "reasoning.encrypted":
							// Skip encrypted/redacted reasoning in streaming
							continue
						}

						if thinkingText != "" {
							// Send content_block_start for thinking block on first thinking delta
							if !thinkingBlockStarted {
								writeSSEEvent(w, "content_block_start", map[string]interface{}{
									"type":  "content_block_start",
									"index": thinkingBlockIndex,
									"content_block": map[string]interface{}{
										"type":     "thinking",
										"thinking": "",
									},
								})
								thinkingBlockStarted = true
								_ = w.Flush()
							}

							// Send thinking block delta
							writeSSEEvent(w, "content_block_delta", map[string]interface{}{
								"type":  "content_block_delta",
								"index": thinkingBlockIndex,
								"delta": map[string]interface{}{
									"type":     "thinking_delta",
									"thinking": thinkingText,
								},
							})
							thinkingBlockHasContent = true
							_ = w.Flush()
						}
					}
				}
			}
		}

		// Handle reasoning field directly (simpler format from some models)
		if reasoning, ok := delta["reasoning"].(string); ok && reasoning != "" {
			// Send content_block_start for thinking block on first thinking delta
			if !thinkingBlockStarted {
				writeSSEEvent(w, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": thinkingBlockIndex,
					"content_block": map[string]interface{}{
						"type":     "thinking",
						"thinking": "",
					},
				})
				thinkingBlockStarted = true
				_ = w.Flush()
			}

			// Send thinking block delta
			writeSSEEvent(w, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": thinkingBlockIndex,
				"delta": map[string]interface{}{
					"type":     "thinking_delta",
					"thinking": reasoning,
				},
			})
			thinkingBlockHasContent = true
			_ = w.Flush()
		}

		// Handle text delta
		if content, ok := delta["content"].(string); ok && content != "" {
			// Send content_block_start for text block on first text delta
			if !textBlockStarted {
				writeSSEEvent(w, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": textBlockIndex,
					"content_block": map[string]interface{}{
						"type": "text",
						"text": "",
					},
				})
				textBlockStarted = true
				_ = w.Flush()
			}

			writeSSEEvent(w, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": textBlockIndex,
				"delta": map[string]interface{}{
					"type": "text_delta",
					"text": content,
				},
			})
			_ = w.Flush()
		}

		// Handle tool call deltas
		if toolCallsRaw, ok := delta["tool_calls"]; ok {
			toolCalls, ok := toolCallsRaw.([]interface{})
			if ok && len(toolCalls) > 0 {
				for _, tcRaw := range toolCalls {
					tcDelta, ok := tcRaw.(map[string]interface{})
					if !ok {
						continue
					}

					// Get tool call index
					tcIndex := 0
					if idx, ok := tcDelta["index"].(float64); ok {
						tcIndex = int(idx)
					}

					// Initialize tool call tracking if not exists
					if _, exists := currentToolCalls[tcIndex]; !exists {
						currentToolCalls[tcIndex] = &ToolCallState{
							ID:          "",
							Name:        "",
							ArgsBuffer:  "",
							JSONSent:    false,
							ClaudeIndex: 0,
							Started:     false,
						}
					}

					toolCall := currentToolCalls[tcIndex]

					// Update tool call ID if provided
					if id, ok := tcDelta["id"].(string); ok {
						toolCall.ID = id
					}

					// Update function name
					if functionData, ok := tcDelta["function"].(map[string]interface{}); ok {
						if name, ok := functionData["name"].(string); ok {
							toolCall.Name = name
						}

						// Start content block when we have complete initial data
						if toolCall.ID != "" && toolCall.Name != "" && !toolCall.Started {
							toolBlockCounter++
							claudeIndex := textBlockIndex + toolBlockCounter
							toolCall.ClaudeIndex = claudeIndex
							toolCall.Started = true

							writeSSEEvent(w, "content_block_start", map[string]interface{}{
								"type":  "content_block_start",
								"index": claudeIndex,
								"content_block": map[string]interface{}{
									"type":  "tool_use",
									"id":    toolCall.ID,
									"name":  toolCall.Name,
									"input": map[string]interface{}{},
								},
							})
							_ = w.Flush()
						}

						// Handle function arguments
						// Type assertion handles nil check, Started flag, and we process even empty strings
						if args, ok := functionData["arguments"].(string); ok && toolCall.Started {
							// Only accumulate if args is not empty
							if args != "" {
								toolCall.ArgsBuffer += args
							}

							// Try to parse complete JSON and send delta when we have valid JSON
							if toolCall.ArgsBuffer != "" {
								var jsonTest interface{}
								if err := json.Unmarshal([]byte(toolCall.ArgsBuffer), &jsonTest); err == nil {
									// If parsing succeeds and we haven't sent this JSON yet
									if !toolCall.JSONSent {
										writeSSEEvent(w, "content_block_delta", map[string]interface{}{
											"type":  "content_block_delta",
											"index": toolCall.ClaudeIndex,
											"delta": map[string]interface{}{
												"type":         "input_json_delta",
												"partial_json": toolCall.ArgsBuffer,
											},
										})
										_ = w.Flush()
										toolCall.JSONSent = true
									}
								}
							}
							// If JSON is incomplete, continue accumulating (no action needed)
						}
					}
				}
			}
		}

		// Handle finish reason
		// NOTE: Don't break here - with stream_options.include_usage, OpenAI sends usage in a chunk AFTER finish_reason
		if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" {
			switch finishReason {
			case "length":
				finalStopReason = "max_tokens"
			case "tool_calls", "function_call":
				finalStopReason = "tool_use"
			case "stop":
				finalStopReason = "end_turn"
			default:
				finalStopReason = "end_turn"
			}
			// Continue processing to capture usage chunk (don't break)
		}
	}

	// Send final SSE events

	// Send content_block_stop for text block if it was started
	if textBlockStarted {
		writeSSEEvent(w, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": textBlockIndex,
		})
		_ = w.Flush()
	}

	// Send content_block_stop for each tool call
	for _, toolData := range currentToolCalls {
		// Check both Started AND claude_index is not None
		if toolData.Started && toolData.ClaudeIndex != 0 {
			writeSSEEvent(w, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": toolData.ClaudeIndex,
			})
			_ = w.Flush()
		}
	}

	// Send content_block_stop for thinking block if it had content
	if thinkingBlockStarted && thinkingBlockHasContent {
		writeSSEEvent(w, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": thinkingBlockIndex,
		})
		_ = w.Flush()
	}

	// Debug: Check if usage data was received
	if cfg.Debug {
		inputTokens, _ := usageData["input_tokens"].(int)
		outputTokens, _ := usageData["output_tokens"].(int)
		if inputTokens == 0 && outputTokens == 0 {
			fmt.Printf("[DEBUG] OpenRouter streaming: Usage data unavailable (expected limitation of streaming API)\n")
		}
	}

	// Send message_delta with stop_reason and accumulated usage data
	writeSSEEvent(w, "message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   finalStopReason,
			"stop_sequence": nil,
		},
		"usage": usageData,
	})
	_ = w.Flush()

	// Send message_stop
	writeSSEEvent(w, "message_stop", map[string]interface{}{
		"type": "message_stop",
	})
	_ = w.Flush()

	// Simple log: one-line summary
	if cfg.SimpleLog {
		inputTokens := 0
		outputTokens := 0

		// Try to extract tokens from various possible formats
		if val, ok := usageData["input_tokens"].(int); ok {
			inputTokens = val
		} else if val, ok := usageData["input_tokens"].(float64); ok {
			inputTokens = int(val)
		}

		if val, ok := usageData["output_tokens"].(int); ok {
			outputTokens = val
		} else if val, ok := usageData["output_tokens"].(float64); ok {
			outputTokens = int(val)
		}

		// Calculate tokens per second
		duration := time.Since(startTime).Seconds()
		tokensPerSec := 0.0
		if duration > 0 && outputTokens > 0 {
			tokensPerSec = float64(outputTokens) / duration
		}

		timestamp := time.Now().Format("15:04:05")
		fmt.Printf("[%s] [REQ] %s model=%s in=%d out=%d tok/s=%.1f\n",
			timestamp,
			cfg.OpenAIBaseURL,
			providerModel,
			inputTokens,
			outputTokens,
			tokensPerSec)
	}

	// Check for scanner errors
	if err := scanner.Err(); err != nil {
		writeSSEError(w, fmt.Sprintf("stream read error: %v", err))
		return streamOutcome{Chunks: chunkCount, StopReason: finalStopReason, Usage: usageData, Err: err}
	}
	if cfg.Debug {
		fmt.Printf("[DEBUG] stream completed model=%s chunks=%d stop_reason=%s\n", providerModel, chunkCount, finalStopReason)
	}
	return streamOutcome{Chunks: chunkCount, StopReason: finalStopReason, Usage: usageData}
}

// writeSSEEvent writes a Server-Sent Event
func writeSSEEvent(w *bufio.Writer, event string, data interface{}) {
	dataJSON, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\n", event)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", string(dataJSON))
}

// writeSSEError writes an error event
func writeSSEError(w *bufio.Writer, message string) {
	writeSSEEvent(w, "error", map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "api_error",
			"message": message,
		},
	})
	_ = w.Flush()
}

// callOpenAI makes an HTTP request to the OpenAI API with automatic retry logic
// for max_completion_tokens parameter errors. Uses per-model capability caching.
func callOpenAI(req *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace) (*models.OpenAIResponse, error) {
	resp, err := callOpenAIInternal(req, cfg, trace, false)
	if err != nil {
		// Check if this is a max_tokens parameter error
		if isMaxTokensParameterError(err.Error()) {
			if cfg.Debug {
				fmt.Printf("[DEBUG] Detected max_completion_tokens parameter error for model %s, retrying without it\n", req.Model)
			}
			// Retry without max_completion_tokens and cache the capability per model
			return retryWithoutMaxCompletionTokens(req, cfg, trace)
		}
		// Other errors - return as-is
		return nil, err
	}

	// Success on first try - cache that this (provider, model) supports max_completion_tokens
	// Only cache if we actually sent max_completion_tokens
	if req.MaxCompletionTokens > 0 {
		cacheKey := config.CacheKey{
			BaseURL: cfg.OpenAIBaseURL,
			Model:   req.Model,
		}
		config.SetModelCapabilities(cacheKey, &config.ModelCapabilities{
			UsesMaxCompletionTokens: true,
		})
		if cfg.Debug {
			fmt.Printf("[DEBUG] Cached: model %s supports max_completion_tokens\n", req.Model)
		}
	}

	return resp, nil
}

// callOpenAIStream makes a streaming HTTP request with retry logic for parameter errors.
// Uses per-model capability caching.
func callOpenAIStream(req *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace) (*http.Response, error) {
	resp, err := callOpenAIStreamInternal(req, cfg, trace, false)
	if err != nil {
		// Check if this is a max_tokens parameter error
		if isMaxTokensParameterError(err.Error()) {
			if cfg.Debug {
				fmt.Printf("[DEBUG] Detected max_completion_tokens parameter error in stream for model %s, retrying without it\n", req.Model)
			}
			// Create retry request without max tokens
			retryReq := *req
			retryReq.MaxCompletionTokens = 0
			retryReq.MaxTokens = 0

			// Cache that this (provider, model) doesn't support max_completion_tokens
			cacheKey := config.CacheKey{
				BaseURL: cfg.OpenAIBaseURL,
				Model:   req.Model,
			}
			config.SetModelCapabilities(cacheKey, &config.ModelCapabilities{
				UsesMaxCompletionTokens: false,
			})

			return callOpenAIStreamInternal(&retryReq, cfg, trace, true)
		}
		return nil, err
	}

	// Success - cache capability if we sent max_completion_tokens
	if req.MaxCompletionTokens > 0 {
		cacheKey := config.CacheKey{
			BaseURL: cfg.OpenAIBaseURL,
			Model:   req.Model,
		}
		config.SetModelCapabilities(cacheKey, &config.ModelCapabilities{
			UsesMaxCompletionTokens: true,
		})
		if cfg.Debug {
			fmt.Printf("[DEBUG] Cached: model %s supports max_completion_tokens (streaming)\n", req.Model)
		}
	}

	return resp, nil
}

// callOpenAIStreamInternal makes a streaming HTTP request without retry logic
func callOpenAIStreamInternal(req *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace, retry bool) (result *http.Response, err error) {
	attempt := trace.beginAttempt(true, retry)
	statusCode := 0
	defer func() { trace.endAttempt(attempt, statusCode, err) }()

	// Marshal request to JSON
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	trace.setRequestBody(reqBody)

	// Build API URL
	apiURL := cfg.ChatCompletionsURL()

	// Create HTTP request
	httpReq, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")
	if trace != nil {
		httpReq.Header.Set("X-Request-ID", trace.event.RequestID)
	}

	// Skip auth for Ollama (localhost)
	if !cfg.IsLocalhost() {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
	}

	// OpenRouter-specific headers
	if cfg.DetectProvider() == config.ProviderOpenRouter {
		addOpenRouterHeaders(httpReq, cfg)
	}

	// Create HTTP client with longer timeout for streaming
	client := &http.Client{
		Timeout: 300 * time.Second,
	}

	// Make request
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	statusCode = resp.StatusCode

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, diagnosticsBodyLimit+1))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("OpenAI API returned status %d: %s", resp.StatusCode, boundedUpstreamText(body))
	}

	return resp, nil
}

// isMaxTokensParameterError checks if the error message indicates an unsupported
// max_tokens or max_completion_tokens parameter issue.
// Uses broad keyword matching to handle different error message formats across providers.
// No status code checking - relies on message content alone.
func isMaxTokensParameterError(errorMessage string) bool {
	errorLower := strings.ToLower(errorMessage)

	// Check for parameter error indicators
	hasParamIndicator := strings.Contains(errorLower, "parameter") ||
		strings.Contains(errorLower, "unsupported") ||
		strings.Contains(errorLower, "invalid")

	// Check for our specific parameter names
	hasOurParam := strings.Contains(errorLower, "max_tokens") ||
		strings.Contains(errorLower, "max_completion_tokens")

	// Require both indicators to reduce false positives
	return hasParamIndicator && hasOurParam
}

// retryWithoutMaxCompletionTokens attempts the request again without max_completion_tokens.
// Caches the result per (provider, model) combination for future requests.
func retryWithoutMaxCompletionTokens(req *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace) (*models.OpenAIResponse, error) {
	// Create a copy of the request without max_completion_tokens
	retryReq := *req
	retryReq.MaxCompletionTokens = 0
	retryReq.MaxTokens = 0 // Also clear max_tokens to avoid issues

	if cfg.Debug {
		fmt.Printf("[DEBUG] Retrying without max_completion_tokens/max_tokens for model: %s\n", req.Model)
	}

	// Cache that this specific (provider, model) doesn't support max_completion_tokens
	cacheKey := config.CacheKey{
		BaseURL: cfg.OpenAIBaseURL,
		Model:   req.Model,
	}
	config.SetModelCapabilities(cacheKey, &config.ModelCapabilities{
		UsesMaxCompletionTokens: false,
	})

	// Make the retry request
	return callOpenAIInternal(&retryReq, cfg, trace, true)
}

// callOpenAIInternal is the internal implementation without retry logic
func callOpenAIInternal(req *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace, retry bool) (result *models.OpenAIResponse, err error) {
	attempt := trace.beginAttempt(false, retry)
	statusCode := 0
	defer func() { trace.endAttempt(attempt, statusCode, err) }()

	// Marshal request to JSON
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	trace.setRequestBody(reqBody)

	// Build API URL
	apiURL := cfg.ChatCompletionsURL()

	// Create HTTP request
	httpReq, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")
	if trace != nil {
		httpReq.Header.Set("X-Request-ID", trace.event.RequestID)
	}

	// Skip auth for Ollama (localhost) - Ollama doesn't require authentication
	if !cfg.IsLocalhost() {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
	}

	// OpenRouter-specific headers for better rate limits
	if cfg.DetectProvider() == config.ProviderOpenRouter {
		addOpenRouterHeaders(httpReq, cfg)
	}

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 90 * time.Second,
	}

	// Make request
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	statusCode = resp.StatusCode
	defer func() { _ = resp.Body.Close() }()

	// Read response body
	// Read response body. Error payloads are bounded before being surfaced or recorded.
	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, diagnosticsBodyLimit+1))
		if readErr != nil {
			return nil, fmt.Errorf("failed to read error response: %w", readErr)
		}
		return nil, fmt.Errorf("OpenAI API returned status %d: %s", resp.StatusCode, boundedUpstreamText(respBody))
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse response
	var openaiResp models.OpenAIResponse
	if err := json.Unmarshal(respBody, &openaiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &openaiResp, nil
}

func boundedUpstreamText(body []byte) string {
	originalLength := len(body)
	truncated := originalLength > diagnosticsBodyLimit
	if truncated {
		body = body[:diagnosticsBodyLimit]
	}
	if !truncated {
		if redacted, err := diagnostics.RedactJSON(body, diagnostics.RedactionOptions{}); err == nil {
			return string(redacted)
		}
	}
	metadata := diagnostics.DescribeMalformedBody(body, "application/json", fmt.Errorf("upstream body was not valid complete JSON"))
	metadata.Length = originalLength
	if truncated {
		metadata.Preview = "truncated"
	}
	encoded, _ := json.Marshal(metadata)
	return string(encoded)
}

func errorJSON(err error) string {
	encoded, _ := json.Marshal(map[string]string{"error": err.Error()})
	return string(encoded)
}

func handleCountTokens(c *fiber.Ctx, cfg *config.Config) error {
	// Simple token counting endpoint
	return c.JSON(fiber.Map{
		"input_tokens": 100,
	})
}
