package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
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
		trace.finish(fiber.StatusBadRequest, diagnosticFailure(completionInvalidResponse, failureInvalidResponse, err))
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
		trace.finish(fiber.StatusUnauthorized, diagnosticFailure(completionLocalAuthentication, failureLocalAuthentication, err))
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
		trace.finish(fiber.StatusBadRequest, diagnosticFailure(completionConversionError, failureConversionError, err))
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
		return handleStreamingMessages(c, openaiReq, claudeReq.Model, cfg, trace)
	}

	startTime := time.Now()
	openaiResp, err := callOpenAI(c.UserContext(), openaiReq, cfg, trace)
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
		trace.finish(fiber.StatusInternalServerError, diagnosticFailure(completionConversionError, failureConversionError, err))
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
	stopReason := ""
	if len(openaiResp.Choices) > 0 && openaiResp.Choices[0].FinishReason != nil {
		stopReason = *openaiResp.Choices[0].FinishReason
	}
	trace.setOutcome(openaiResp.Usage.PromptTokens, openaiResp.Usage.CompletionTokens,
		openaiResp.Usage.PromptTokensDetails.CachedTokens, openaiResp.Usage.CacheCreationInputTokens,
		0, stopReason, completionCompleted, "", false, false)
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
func handleStreamingMessages(c *fiber.Ctx, openaiReq *models.OpenAIRequest, claudeModel string, cfg *config.Config, trace *diagnosticsTrace) error {
	startTime := time.Now()
	requestContext := c.UserContext()

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		// Fiber/fasthttp does not expose a reliable client-disconnect context here.
		// The dependable boundary is a failed stream write/conversion; cancel the
		// upstream request at that point and always when this writer lifetime ends.
		streamContext, cancel := context.WithCancel(requestContext)
		defer cancel()

		trace.stage("stream_started", "")
		resp, err := callOpenAIStream(streamContext, openaiReq, cfg, trace)
		if err != nil {
			_ = writeSSEError(w, fmt.Sprintf("streaming request failed: %v", err))
			trace.setResponseBody([]byte(errorJSON(err)))
			trace.finish(fiber.StatusBadGateway, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		outcome := streamOpenAIToClaude(w, resp.Body, claudeModel, openaiReq.Model, cfg, startTime, cancel)
		responseMetadata, _ := json.Marshal(map[string]interface{}{
			"streaming": true,
			"chunks": outcome.Chunks,
			"stop_reason": outcome.StopReason,
			"usage": outcome.Usage,
		})
		trace.setResponseBody(responseMetadata)
		inputTokens := usageInt(outcome.Usage, "input_tokens")
		outputTokens := usageInt(outcome.Usage, "output_tokens")
		cacheReadInputTokens := usageInt(outcome.Usage, "cache_read_input_tokens")
		cacheCreationInputTokens := usageInt(outcome.Usage, "cache_creation_input_tokens")
		if outcome.Err != nil {
			completionState, failureKind := classifyFailure(outcome.Err)
			trace.setOutcome(inputTokens, outputTokens, cacheReadInputTokens, cacheCreationInputTokens,
				outcome.Chunks, outcome.StopReason, completionState, failureKind,
				failureKind == failureCanceled, failureKind == failureUpstreamTruncated)
			trace.finish(fiber.StatusBadGateway, outcome.Err)
			return
		}
		trace.setOutcome(inputTokens, outputTokens, cacheReadInputTokens, cacheCreationInputTokens,
			outcome.Chunks, outcome.StopReason, completionCompleted, "", false, false)
		trace.finish(fiber.StatusOK, nil)
	})

	return nil
}

// ToolCallState tracks the state of a tool call during streaming
type ToolCallState struct {
	ID            string   // Tool call ID from OpenAI
	Name          string   // Function name
	ArgsFragments []string // Argument fragments retained only for this response stream
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

func streamOpenAIToClaude(w *bufio.Writer, reader io.Reader, claudeModel, providerModel string, cfg *config.Config, startTime time.Time, cancelUpstream context.CancelFunc) streamOutcome {
	if cfg.Debug {
		fmt.Printf("[DEBUG] streamOpenAIToClaude: Starting conversion\n")
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	messageID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	nextBlockIndex := 0
	openBlockIndex := -1
	openBlockType := ""
	currentToolCalls := make(map[int]*ToolCallState)
	finalStopReason := "end_turn"
	finishSeen, doneSeen := false, false
	chunkCount := 0
	usageData := map[string]interface{}{
		"input_tokens": 0, "output_tokens": 0,
		"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		"cache_creation": map[string]interface{}{"ephemeral_5m_input_tokens": 0, "ephemeral_1h_input_tokens": 0},
	}

	emit := func(event string, data interface{}) error {
		err := writeSSEEvent(w, event, data)
		if err != nil {
			if cancelUpstream != nil {
				cancelUpstream()
			}
			return diagnosticFailure(completionDownstreamWrite, failureDownstreamWrite, err)
		}
		return nil
	}
	fail := func(err error) streamOutcome {
		if cancelUpstream != nil {
			cancelUpstream()
		}
		return streamOutcome{Chunks: chunkCount, StopReason: finalStopReason, Usage: usageData, Err: err}
	}
	stopOpenBlock := func() error {
		if openBlockIndex < 0 {
			return nil
		}
		if err := emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": openBlockIndex}); err != nil {
			return err
		}
		openBlockIndex = -1
		openBlockType = ""
		return nil
	}
	startBlock := func(blockType string, contentBlock map[string]interface{}) (int, error) {
		if openBlockType == blockType {
			return openBlockIndex, nil
		}
		if err := stopOpenBlock(); err != nil {
			return -1, err
		}
		index := nextBlockIndex
		nextBlockIndex++
		if err := emit("content_block_start", map[string]interface{}{
			"type": "content_block_start", "index": index, "content_block": contentBlock,
		}); err != nil {
			return -1, err
		}
		openBlockIndex = index
		openBlockType = blockType
		return index, nil
	}

	if err := emit("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": messageID, "type": "message", "role": "assistant", "model": claudeModel,
			"content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil, "usage": usageData,
		},
	}); err != nil {
		return fail(err)
	}
	if err := emit("ping", map[string]interface{}{"type": "ping"}); err != nil {
		return fail(err)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(line, "data:")) == "[DONE]" {
			doneSeen = true
			break
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			streamErr := diagnosticFailure(completionInvalidResponse, failureInvalidResponse,
				fmt.Errorf("invalid upstream stream JSON: %w", err))
			_ = writeSSEError(w, streamErr.Error())
			return fail(streamErr)
		}
		chunkCount++

		if usage, ok := chunk["usage"].(map[string]interface{}); ok {
			inputTokens, outputTokens := 0, 0
			if val, ok := usage["prompt_tokens"].(float64); ok {
				inputTokens = int(val)
			}
			if val, ok := usage["completion_tokens"].(float64); ok {
				outputTokens = int(val)
			}
			usageData = map[string]interface{}{
				"input_tokens": inputTokens, "output_tokens": outputTokens,
				"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
			}
			if details, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
				if cached, ok := details["cached_tokens"].(float64); ok {
					usageData["cache_read_input_tokens"] = int(cached)
				}
			}
			if created, ok := usage["cache_creation_input_tokens"].(float64); ok {
				usageData["cache_creation_input_tokens"] = int(created)
			}
		}

		choices, ok := chunk["choices"].([]interface{})
		if !ok || len(choices) == 0 { continue }
		choice, ok := choices[0].(map[string]interface{})
		if !ok { continue }
		delta, _ := choice["delta"].(map[string]interface{})

		thinkingText, signature := "", ""
		if details, ok := delta["reasoning_details"].([]interface{}); ok {
			var detailsText strings.Builder
			for _, raw := range details {
				detail, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				if signature == "" {
					signature, _ = detail["signature"].(string)
				}
				var value string
				switch detail["type"] {
				case "reasoning.text":
					value, _ = detail["text"].(string)
				case "reasoning.summary":
					value, _ = detail["summary"].(string)
				}
				detailsText.WriteString(value)
			}
			thinkingText = detailsText.String()
		}
		if value, ok := delta["reasoning_content"].(string); ok && value != "" {
			thinkingText = value
		} else if value, ok := delta["reasoning"].(string); ok && value != "" {
			thinkingText = value
		}
		if thinkingText != "" || signature != "" {
			thinkingIndex, err := startBlock("thinking", map[string]interface{}{"type": "thinking", "thinking": ""})
			if err != nil {
				return fail(err)
			}
			if thinkingText != "" {
				if err := emit("content_block_delta", map[string]interface{}{
					"type": "content_block_delta", "index": thinkingIndex,
					"delta": map[string]interface{}{"type": "thinking_delta", "thinking": thinkingText},
				}); err != nil {
					return fail(err)
				}
			}
			if signature != "" {
				if err := emit("content_block_delta", map[string]interface{}{
					"type": "content_block_delta", "index": thinkingIndex,
					"delta": map[string]interface{}{"type": "signature_delta", "signature": signature},
				}); err != nil {
					return fail(err)
				}
			}
		}

		if content, ok := delta["content"].(string); ok && content != "" {
			textIndex, err := startBlock("text", map[string]interface{}{"type": "text", "text": ""})
			if err != nil {
				return fail(err)
			}
			if err := emit("content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": textIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": content},
			}); err != nil {
				return fail(err)
			}
		}

		if rawCalls, ok := delta["tool_calls"].([]interface{}); ok {
			for _, raw := range rawCalls {
				tc, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				idx := 0
				if value, ok := tc["index"].(float64); ok {
					idx = int(value)
				}
				state := currentToolCalls[idx]
				if state == nil {
					state = &ToolCallState{}
					currentToolCalls[idx] = state
				}
				if id, ok := tc["id"].(string); ok && id != "" {
					state.ID = id
				}
				functionData, _ := tc["function"].(map[string]interface{})
				if name, ok := functionData["name"].(string); ok && name != "" {
					state.Name = name
				}
				if args, ok := functionData["arguments"].(string); ok && args != "" {
					state.ArgsFragments = append(state.ArgsFragments, args)
				}
			}
		}

		if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" {
			finishSeen = true
			switch finishReason {
			case "length": finalStopReason = "max_tokens"
			case "tool_calls", "function_call": finalStopReason = "tool_use"
			default: finalStopReason = "end_turn"
			}
		}
	}

	if err := scanner.Err(); err != nil {
		streamErr := diagnosticFailure(completionUpstreamError, failureUpstreamRead,
			fmt.Errorf("stream read error: %w", err))
		_ = writeSSEError(w, streamErr.Error())
		return fail(streamErr)
	}
	if !doneSeen && !finishSeen {
		streamErr := diagnosticFailure(completionUpstreamError, failureUpstreamTruncated,
			fmt.Errorf("upstream stream truncated before completion"))
		_ = writeSSEError(w, streamErr.Error())
		return fail(streamErr)
	}
	if err := stopOpenBlock(); err != nil {
		return fail(err)
	}
	toolIndices := make([]int, 0, len(currentToolCalls))
	for upstreamIndex := range currentToolCalls {
		toolIndices = append(toolIndices, upstreamIndex)
	}
	sort.Ints(toolIndices)
	for _, upstreamIndex := range toolIndices {
		state := currentToolCalls[upstreamIndex]
		if state.ID == "" || state.Name == "" {
			streamErr := diagnosticFailure(completionInvalidResponse, failureInvalidResponse,
				fmt.Errorf("incomplete upstream tool call at index %d", upstreamIndex))
			_ = writeSSEError(w, streamErr.Error())
			return fail(streamErr)
		}
		toolIndex, err := startBlock("tool", map[string]interface{}{
			"type": "tool_use", "id": state.ID, "name": state.Name, "input": map[string]interface{}{},
		})
		if err != nil {
			return fail(err)
		}
		for _, fragment := range state.ArgsFragments {
			if err := emit("content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": toolIndex,
				"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": fragment},
			}); err != nil {
				return fail(err)
			}
		}
		if err := stopOpenBlock(); err != nil {
			return fail(err)
		}
	}
	if err := emit("message_delta", map[string]interface{}{
		"type": "message_delta", "delta": map[string]interface{}{"stop_reason": finalStopReason, "stop_sequence": nil}, "usage": usageData,
	}); err != nil { return fail(err) }
	if err := emit("message_stop", map[string]interface{}{"type": "message_stop"}); err != nil { return fail(err) }

	if cfg.SimpleLog {
		inputTokens, _ := usageData["input_tokens"].(int); outputTokens, _ := usageData["output_tokens"].(int)
		duration := time.Since(startTime).Seconds(); tokensPerSec := 0.0
		if duration > 0 && outputTokens > 0 { tokensPerSec = float64(outputTokens) / duration }
		fmt.Printf("[%s] [REQ] %s model=%s in=%d out=%d tok/s=%.1f\n", time.Now().Format("15:04:05"), cfg.OpenAIBaseURL, providerModel, inputTokens, outputTokens, tokensPerSec)
	}
	if cfg.Debug { fmt.Printf("[DEBUG] stream completed model=%s chunks=%d stop_reason=%s\n", providerModel, chunkCount, finalStopReason) }
	return streamOutcome{Chunks: chunkCount, StopReason: finalStopReason, Usage: usageData}
}

func usageInt(usage map[string]interface{}, name string) int {
	switch value := usage[name].(type) {
	case int:
		return value
	case float64:
		return int(value)
	default:
		return 0
	}
}

// writeSSEEvent writes and flushes a Server-Sent Event.
func writeSSEEvent(w *bufio.Writer, event string, data interface{}) error {
	dataJSON, err := json.Marshal(data)
	if err != nil { return fmt.Errorf("marshal SSE event %s: %w", event, err) }
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, dataJSON); err != nil { return fmt.Errorf("write SSE event %s: %w", event, err) }
	if err := w.Flush(); err != nil { return fmt.Errorf("flush SSE event %s: %w", event, err) }
	return nil
}

// writeSSEError writes an error event.
func writeSSEError(w *bufio.Writer, message string) error {
	return writeSSEEvent(w, "error", map[string]interface{}{
		"type": "error", "error": map[string]interface{}{"type": "api_error", "message": message},
	})
}

// callOpenAI makes an HTTP request to the OpenAI API with automatic retry logic
// for max_completion_tokens parameter errors. Uses per-model capability caching.
func callOpenAI(ctx context.Context, req *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace) (*models.OpenAIResponse, error) {
	resp, err := callOpenAIInternal(ctx, req, cfg, trace, false)
	if err != nil {
		// Check if this is a max_tokens parameter error
		if isMaxTokensParameterError(err.Error()) {
			if cfg.Debug {
				fmt.Printf("[DEBUG] Detected max_completion_tokens parameter error for model %s, retrying without it\n", req.Model)
			}
			// Retry without max_completion_tokens and cache the capability per model
			return retryWithoutMaxCompletionTokens(ctx, req, cfg, trace)
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
func callOpenAIStream(ctx context.Context, req *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace) (*http.Response, error) {
	resp, err := callOpenAIStreamInternal(ctx, req, cfg, trace, false)
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

			return callOpenAIStreamInternal(ctx, &retryReq, cfg, trace, true)
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
func callOpenAIStreamInternal(ctx context.Context, req *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace, retry bool) (result *http.Response, err error) {
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
	httpReq, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(reqBody))
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

	// Use transport phase timeouts without imposing a total streaming deadline.
	client := &http.Client{Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 15 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}}

	// Make request
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, diagnosticFailure(completionUpstreamError, failureUpstreamError, fmt.Errorf("request failed: %w", err))
	}
	statusCode = resp.StatusCode

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, diagnosticsBodyLimit+1))
		_ = resp.Body.Close()
		return nil, diagnosticFailure(completionUpstreamError, failureUpstreamStatus,
			fmt.Errorf("OpenAI API returned status %d: %s", resp.StatusCode, boundedUpstreamText(body)))
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
func retryWithoutMaxCompletionTokens(ctx context.Context, req *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace) (*models.OpenAIResponse, error) {
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
	return callOpenAIInternal(ctx, &retryReq, cfg, trace, true)
}

// callOpenAIInternal is the internal implementation without retry logic
func callOpenAIInternal(ctx context.Context, req *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace, retry bool) (result *models.OpenAIResponse, err error) {
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
	httpReq, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(reqBody))
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
		return nil, diagnosticFailure(completionUpstreamError, failureUpstreamError, fmt.Errorf("request failed: %w", err))
	}
	statusCode = resp.StatusCode
	defer func() { _ = resp.Body.Close() }()

	// Read response body
	// Read response body. Error payloads are bounded before being surfaced or recorded.
	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, diagnosticsBodyLimit+1))
		if readErr != nil {
			return nil, diagnosticFailure(completionUpstreamError, failureUpstreamRead,
				fmt.Errorf("failed to read error response: %w", readErr))
		}
		return nil, diagnosticFailure(completionUpstreamError, failureUpstreamStatus,
			fmt.Errorf("OpenAI API returned status %d: %s", resp.StatusCode, boundedUpstreamText(respBody)))
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, diagnosticFailure(completionUpstreamError, failureUpstreamRead,
			fmt.Errorf("failed to read response: %w", err))
	}

	// Parse response
	var openaiResp models.OpenAIResponse
	if err := json.Unmarshal(respBody, &openaiResp); err != nil {
		return nil, diagnosticFailure(completionInvalidResponse, failureInvalidResponse,
			fmt.Errorf("failed to parse response: %w", err))
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
