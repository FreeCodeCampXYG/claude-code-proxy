package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/converter"
	"github.com/claude-code-proxy/proxy/internal/diagnostics"
	"github.com/claude-code-proxy/proxy/internal/monitor"
	"github.com/claude-code-proxy/proxy/internal/promptarchive"
	"github.com/claude-code-proxy/proxy/pkg/models"
	"github.com/gofiber/fiber/v2"
)

const upstreamMaxAttempts = 3

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
func handleMessages(c *fiber.Ctx, cfg *config.Config, store *diagnostics.Store, stats *monitor.Stats, promptStore *promptarchive.Store) error {
	trace := newDiagnosticsTrace(c, cfg, store)
	if stats != nil {
		stats.IncActive()
		defer stats.DecActive()
	}

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

	trace.capture(diagnostics.ContentBoundaryClaudeRequest, 0, c.Body())
	trace.setClaudeRequestMetrics(c.Body(), claudeReq)
	if promptStore != nil {
		// Only this explicit client session identifier may opt a record into archive compaction.
		// Do not infer a session from request IDs, parent IDs, timestamps, or request content.
		promptStore.Enqueue(promptarchive.BuildRecord(c.GetRespHeader("X-Request-ID"), c.Body(), claudeReq, time.Now(), strings.TrimSpace(c.Get("x-anthropic-session-id"))))
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
	trace.setRouting(openaiReq)
	if cfg.Debug {
		fmt.Printf("[DEBUG] request_id=%s provider=%s model=%s key_label=%s streaming=%t tools=%d\n",
			c.GetRespHeader("X-Request-ID"), cfg.DetectProvider(), openaiReq.Model, cfg.OpenAIAPIKeyLabel, streaming, len(openaiReq.Tools))
	}

	if streaming {
		return handleStreamingMessages(c, openaiReq, claudeReq.Model, cfg, trace, stats)
	}

	startTime := time.Now()
	openaiResp, err := callOpenAI(c.UserContext(), openaiReq, cfg, trace)
	if err != nil {
		mapped := mapUpstreamErrorForDownstream(err, cfg)
		trace.setResponseBody([]byte(errorJSON(err)))
		trace.finish(mapped.StatusCode, mapped.DiagnosticsError)
		logUpstreamFailure(trace, cfg, openaiReq.Model, false, mapped.StatusCode, mapped.ContextWindowExceeded, mapped.Retryable, err)
		return c.Status(mapped.StatusCode).JSON(mapped.Body)
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
	if encoded, marshalErr := json.Marshal(claudeResp); marshalErr == nil {
		trace.capture(diagnostics.ContentBoundaryClaudeResponse, 0, encoded)
		trace.setClaudeResponseBytes(len(encoded))
	}
	if encoded, marshalErr := json.Marshal(openaiResp); marshalErr == nil {
		trace.setResponseBody(encoded)
		trace.setUpstreamResponseBytes(len(encoded))
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
	if stats != nil {
		stats.Record(monitor.Event{RequestID: c.GetRespHeader("X-Request-ID"), Model: openaiReq.Model, Provider: string(cfg.DetectProvider()), Success: true, StatusCode: fiber.StatusOK, InputTokens: claudeResp.Usage.InputTokens, OutputTokens: claudeResp.Usage.OutputTokens, CacheTokens: openaiResp.Usage.PromptTokensDetails.CachedTokens + openaiResp.Usage.CacheCreationInputTokens, Duration: time.Since(startTime)})
	}
	if cfg.SimpleLog {
		duration := time.Since(startTime).Seconds()
		tokensPerSec := 0.0
		if duration > 0 && claudeResp.Usage.OutputTokens > 0 {
			tokensPerSec = float64(claudeResp.Usage.OutputTokens) / duration
		}
		timestamp := time.Now().Format("15:04:05")
		fmt.Printf("[%s] [REQ] %s model=%s key_label=%s in=%d out=%d tok/s=%.1f\n",
			timestamp, safeBaseURL(cfg.OpenAIBaseURL), openaiReq.Model, cfg.OpenAIAPIKeyLabel, claudeResp.Usage.InputTokens,
			claudeResp.Usage.OutputTokens, tokensPerSec)
	}

	return c.JSON(claudeResp)
}

// handleStreamingMessages handles streaming SSE responses from the provider.
// It forwards the OpenAI request, receives streaming chunks, and converts them to
// Claude's SSE event format in real-time using streamOpenAIToClaude.
func handleStreamingMessages(c *fiber.Ctx, openaiReq *models.OpenAIRequest, claudeModel string, cfg *config.Config, trace *diagnosticsTrace, stats *monitor.Stats) error {
	startTime := time.Now()
	requestContext := c.UserContext()

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	requestID := ""
	if trace != nil {
		requestID = trace.event.RequestID
	}
	provider := string(cfg.DetectProvider())

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		// Fiber/fasthttp does not expose a reliable client-disconnect context here.
		// The dependable boundary is a failed stream write/conversion; cancel the
		// upstream request at that point and always when this writer lifetime ends.
		streamContext, cancel := context.WithCancel(requestContext)
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				err := diagnosticFailure(completionUpstreamError, failureUpstreamError,
					fmt.Errorf("streaming response panic: %v", recovered))
				_ = writeSSEError(w, err.Error(), "api_error", false)
				trace.setResponseBody([]byte(errorJSON(err)))
				trace.finish(fiber.StatusOK, err)
				if stats != nil {
					stats.Record(monitor.Event{RequestID: requestID, Model: openaiReq.Model, Provider: provider, Streaming: true, Success: false, StatusCode: fiber.StatusOK, Duration: time.Since(startTime)})
				}
				fmt.Printf("[ERROR] Streaming response panic request_id=%s provider=%s model=%s panic=%v\n", requestID, provider, openaiReq.Model, recovered)
			}
		}()

		trace.stage("stream_started", "")
		resp, err := callOpenAIStream(streamContext, openaiReq, cfg, trace)
		if err != nil {
			mapped := mapUpstreamErrorForDownstream(err, cfg)
			if writeErr := writeSSEError(w, mapped.StreamMessage, mapped.ErrorType, mapped.Retryable); writeErr != nil {
				mapped.DiagnosticsError = diagnosticFailure(completionDownstreamWrite, failureDownstreamWrite, writeErr)
			}
			mapped.StatusCode = fiber.StatusOK
			trace.setResponseBody([]byte(errorJSON(err)))
			trace.finish(mapped.StatusCode, mapped.DiagnosticsError)
			if stats != nil {
				stats.Record(monitor.Event{RequestID: requestID, Model: openaiReq.Model, Provider: provider, Streaming: true, Success: false, StatusCode: mapped.StatusCode, Duration: time.Since(startTime)})
			}
			logUpstreamFailure(trace, cfg, openaiReq.Model, true, mapped.StatusCode, mapped.ContextWindowExceeded, mapped.Retryable, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		outcome := streamOpenAIToClaude(w, resp.Body, claudeModel, openaiReq.Model, cfg, startTime, cancel)
		responseMetadata, _ := json.Marshal(map[string]interface{}{
			"streaming":   true,
			"chunks":      outcome.Chunks,
			"stop_reason": outcome.StopReason,
			"usage":       outcome.Usage,
		})
		trace.setResponseBody(responseMetadata)
		trace.setUpstreamResponseBytes(len(outcome.Semantic))
		trace.setClaudeResponseBytes(len(outcome.Semantic))
		trace.capture(diagnostics.ContentBoundaryUpstreamResponse, 1, outcome.Semantic)
		trace.capture(diagnostics.ContentBoundaryClaudeResponse, 0, outcome.Semantic)
		inputTokens := usageInt(outcome.Usage, "input_tokens")
		outputTokens := usageInt(outcome.Usage, "output_tokens")
		cacheReadInputTokens := usageInt(outcome.Usage, "cache_read_input_tokens")
		cacheCreationInputTokens := usageInt(outcome.Usage, "cache_creation_input_tokens")
		cacheTokens := cacheReadInputTokens + cacheCreationInputTokens
		if outcome.Err != nil {
			completionState, failureKind := classifyFailure(outcome.Err)
			trace.setOutcome(inputTokens, outputTokens, cacheReadInputTokens, cacheCreationInputTokens,
				outcome.Chunks, outcome.StopReason, completionState, failureKind,
				failureKind == failureCanceled, failureKind == failureUpstreamTruncated)
			trace.finish(fiber.StatusBadGateway, outcome.Err)
			if stats != nil {
				stats.Record(monitor.Event{RequestID: requestID, Model: openaiReq.Model, Provider: provider, Streaming: true, Success: false, StatusCode: fiber.StatusBadGateway, InputTokens: inputTokens, OutputTokens: outputTokens, CacheTokens: cacheTokens, Duration: time.Since(startTime)})
			}
			return
		}
		trace.setOutcome(inputTokens, outputTokens, cacheReadInputTokens, cacheCreationInputTokens,
			outcome.Chunks, outcome.StopReason, completionCompleted, "", false, false)
		trace.finish(fiber.StatusOK, nil)
		if stats != nil {
			stats.Record(monitor.Event{RequestID: requestID, Model: openaiReq.Model, Provider: provider, Streaming: true, Success: true, StatusCode: fiber.StatusOK, InputTokens: inputTokens, OutputTokens: outputTokens, CacheTokens: cacheTokens, Duration: time.Since(startTime)})
		}
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
	Semantic   json.RawMessage
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
	openBlockSource := ""
	currentToolCalls := make(map[int]*ToolCallState)
	finalStopReason := "end_turn"
	finishSeen, doneSeen := false, false
	chunkCount := 0
	var thinkingParts, textParts []string
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
	failWithSSEError := func(original error, message string, errorType ...string) streamOutcome {
		kind := "api_error"
		if len(errorType) > 0 {
			kind = errorType[0]
		}
		if writeErr := writeSSEError(w, message, kind, true); writeErr != nil {
			return fail(diagnosticFailure(completionDownstreamWrite, failureDownstreamWrite, writeErr))
		}
		return fail(original)
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
		openBlockSource = ""
		return nil
	}
	startBlock := func(blockType, source string, contentBlock map[string]interface{}) (int, error) {
		if openBlockType == blockType && openBlockSource == source {
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
		openBlockSource = source
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
			return failWithSSEError(streamErr, streamErr.Error())
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
		if !ok || len(choices) == 0 {
			if isResponsesPayload(chunk) {
				streamErr := diagnosticFailure(completionInvalidResponse, failureInvalidResponse,
					fmt.Errorf("invalid upstream stream response: received Responses API payload; expected Chat Completions choices[].delta"))
				return failWithSSEError(streamErr, streamErr.Error())
			}
			continue // usage-only Chat Completions chunk
		}
		choice, ok := choices[0].(map[string]interface{})
		if !ok {
			streamErr := diagnosticFailure(completionInvalidResponse, failureInvalidResponse,
				fmt.Errorf("invalid upstream stream response: choices[0] is not an object"))
			return failWithSSEError(streamErr, streamErr.Error())
		}
		delta, ok := choice["delta"].(map[string]interface{})
		if !ok {
			// Azure asynchronous content-filter annotations are valid Chat
			// Completions chunks without a delta. They carry metadata only and
			// must not terminate an otherwise healthy stream.
			continue
		}

		for _, reasoningBlock := range converter.NormalizeReasoning(delta["reasoning_content"], delta["reasoning"], delta["reasoning_details"]) {
			thinkingParts = append(thinkingParts, reasoningBlock.Text)
			thinkingIndex, err := startBlock("thinking", reasoningBlock.Source, map[string]interface{}{"type": "thinking", "thinking": ""})
			if err != nil {
				return fail(err)
			}
			if err := emit("content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": thinkingIndex,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": reasoningBlock.Text},
			}); err != nil {
				return fail(err)
			}
			if reasoningBlock.Signature != "" {
				if err := emit("content_block_delta", map[string]interface{}{
					"type": "content_block_delta", "index": thinkingIndex,
					"delta": map[string]interface{}{"type": "signature_delta", "signature": reasoningBlock.Signature},
				}); err != nil {
					return fail(err)
				}
			}
		}

		if content, ok := delta["content"].(string); ok && content != "" {
			textParts = append(textParts, content)
			textIndex, err := startBlock("text", "text", map[string]interface{}{"type": "text", "text": ""})
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
		return failWithSSEError(streamErr, streamErr.Error())
	}
	if !doneSeen && !finishSeen {
		streamErr := diagnosticFailure(completionUpstreamError, failureUpstreamTruncated,
			fmt.Errorf("upstream stream truncated before completion"))
		return failWithSSEError(streamErr, streamErr.Error())
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
			return failWithSSEError(streamErr, streamErr.Error())
		}
		toolIndex, err := startBlock("tool", "tool", map[string]interface{}{
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
		fmt.Printf("[%s] [REQ] %s model=%s key_label=%s in=%d out=%d tok/s=%.1f\n", time.Now().Format("15:04:05"), safeBaseURL(cfg.OpenAIBaseURL), providerModel, cfg.OpenAIAPIKeyLabel, inputTokens, outputTokens, tokensPerSec)
	}
	if cfg.Debug { fmt.Printf("[DEBUG] stream completed model=%s chunks=%d stop_reason=%s\n", providerModel, chunkCount, finalStopReason) }
	semanticTools := make([]map[string]string, 0, len(toolIndices))
	for _, upstreamIndex := range toolIndices {
		state := currentToolCalls[upstreamIndex]
		semanticTools = append(semanticTools, map[string]string{
			"id": state.ID, "name": state.Name, "arguments": strings.Join(state.ArgsFragments, ""),
		})
	}
	semantic, _ := json.Marshal(map[string]interface{}{
		"format":      "chat-completions-stream-normalized-v1",
		"thinking":    strings.Join(thinkingParts, ""),
		"content":     strings.Join(textParts, ""),
		"tool_calls":  semanticTools,
		"stop_reason": finalStopReason,
		"usage":       usageData,
	})
	return streamOutcome{Chunks: chunkCount, StopReason: finalStopReason, Usage: usageData, Semantic: semantic}
}

func safeBaseURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return raw
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
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
func writeSSEError(w *bufio.Writer, message string, errorType string, retryable bool) error {
	kind := "api_error"
	if strings.TrimSpace(errorType) != "" {
		kind = errorType
	}
	return writeSSEEvent(w, "error", map[string]interface{}{
		"type": "error", "error": map[string]interface{}{"type": kind, "message": message, "retryable": retryable},
	})
}

// callOpenAI makes an HTTP request to the OpenAI API with bounded retry logic
// for transient upstream failures and adaptive max token parameter errors.
func callOpenAI(ctx context.Context, req *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace) (*models.OpenAIResponse, error) {
	currentReq := req
	adaptedMaxTokens := false
	var lastErr error
	attempts := 0

	for attempts < upstreamMaxAttempts {
		resp, err := callOpenAIInternal(ctx, currentReq, cfg, trace, attempts > 0)
		attempts++
		if err == nil {
			cacheMaxCompletionTokensSupport(currentReq, cfg, false)
			return resp, nil
		}
		lastErr = err

		if !adaptedMaxTokens && isUnsupportedMaxTokensParameter(err, currentReq) {
			if cfg.Debug {
				fmt.Printf("[DEBUG] Detected rejected max_completion_tokens parameter for model %s, retrying without it\n", currentReq.Model)
			}
			currentReq = requestWithoutMaxTokens(currentReq, cfg)
			adaptedMaxTokens = true
			continue
		}
		if isContextWindowExceededError(err, cfg) || !isRetryableUpstreamError(err) {
			return nil, err
		}
	}

	return nil, &upstreamRetriesExhaustedError{Attempts: attempts, Err: lastErr}
}

// callOpenAIStream makes a streaming HTTP request with bounded retry logic before
// any downstream SSE response body is written.
func callOpenAIStream(ctx context.Context, req *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace) (*http.Response, error) {
	currentReq := req
	adaptedMaxTokens := false
	var lastErr error
	attempts := 0

	for attempts < upstreamMaxAttempts {
		resp, err := callOpenAIStreamInternal(ctx, currentReq, cfg, trace, attempts > 0)
		attempts++
		if err == nil {
			cacheMaxCompletionTokensSupport(currentReq, cfg, true)
			return resp, nil
		}
		lastErr = err

		if !adaptedMaxTokens && isUnsupportedMaxTokensParameter(err, currentReq) {
			if cfg.Debug {
				fmt.Printf("[DEBUG] Detected rejected max_completion_tokens parameter in stream for model %s, retrying without it\n", currentReq.Model)
			}
			currentReq = requestWithoutMaxTokens(currentReq, cfg)
			adaptedMaxTokens = true
			continue
		}
		if isContextWindowExceededError(err, cfg) || !isRetryableUpstreamError(err) {
			return nil, err
		}
	}

	return nil, &upstreamRetriesExhaustedError{Attempts: attempts, Err: lastErr}
}

// callOpenAIStreamInternal makes a streaming HTTP request without retry logic
func callOpenAIStreamInternal(ctx context.Context, req *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace, retry bool) (result *http.Response, err error) {
	attempt := 0
	if trace != nil {
		attempt = trace.beginAttempt(true, retry)
	}
	statusCode := 0
	defer func() {
		if trace != nil {
			trace.endAttempt(attempt, statusCode, err)
		}
	}()

	// Marshal request to JSON
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	if trace != nil {
		trace.setRequestBody(reqBody)
		trace.setUpstreamRequestMetrics(reqBody, len(req.Tools))
		trace.capture(diagnostics.ContentBoundaryUpstreamRequest, attempt+1, reqBody)
	}

	// Build API URL
	apiURL := cfg.ChatCompletionsURL()

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	requestID := ""
	if trace != nil {
		requestID = trace.event.RequestID
	}
	buildUpstreamRequest(httpReq, cfg, requestID)

	// Use transport phase timeouts without imposing a total streaming deadline.
	client, err := newStreamingUpstreamClient(cfg)
	if err != nil {
		return nil, diagnosticFailure(completionUpstreamError, failureUpstreamError, err)
	}

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
		if trace != nil {
			trace.capture(diagnostics.ContentBoundaryUpstreamResponse, attempt+1, body)
			trace.setUpstreamResponseBytes(len(body))
		}
		return nil, diagnosticFailure(completionUpstreamError, failureUpstreamStatus,
			newUpstreamStatusError(resp.StatusCode, body))
	}

	return resp, nil
}

type upstreamRetriesExhaustedError struct {
	Attempts int
	Err      error
}

func (err *upstreamRetriesExhaustedError) Error() string {
	return fmt.Sprintf("upstream request failed after %d attempts: %v", err.Attempts, err.Err)
}

func (err *upstreamRetriesExhaustedError) Unwrap() error {
	return err.Err
}

type upstreamStatusError struct {
	StatusCode int
	Body       []byte
	Message    string
}

func (err *upstreamStatusError) Error() string {
	return fmt.Sprintf("OpenAI API returned status %d: %s", err.StatusCode, err.Message)
}

func newUpstreamStatusError(statusCode int, body []byte) *upstreamStatusError {
	return &upstreamStatusError{
		StatusCode: statusCode,
		Body:       append([]byte(nil), body...),
		Message:    boundedUpstreamText(body),
	}
}

type downstreamErrorMapping struct {
	StatusCode            int
	Body                  fiber.Map
	ErrorType             string
	StreamMessage         string
	DiagnosticsError      error
	ContextWindowExceeded bool
	Retryable             bool
}

func mapUpstreamErrorForDownstream(err error, cfg *config.Config) downstreamErrorMapping {
	if isContextWindowExceededError(err, cfg) {
		message := fmt.Sprintf("context_window_exceeded: upstream rejected the request because it exceeds the target model context window. Original upstream error: %v", err)
		return downstreamErrorMapping{
			StatusCode: cfg.ContextWindowRewriteStatus,
			ErrorType:  "invalid_request_error",
			Body: fiber.Map{
				"type": "error",
				"error": fiber.Map{
					"type":      "invalid_request_error",
					"message":   message,
					"retryable": false,
				},
			},
			StreamMessage:         message,
			DiagnosticsError:      diagnosticFailure(completionUpstreamError, failureContextWindowExceeded, err),
			ContextWindowExceeded: true,
			Retryable:             false,
		}
	}
	retryable := true
	var exhausted *upstreamRetriesExhaustedError
	if errors.As(err, &exhausted) {
		retryable = false
	}
	return downstreamErrorMapping{
		StatusCode: fiber.StatusBadGateway,
		ErrorType:  "api_error",
		Body: fiber.Map{
			"type": "error",
			"error": fiber.Map{
				"type":      "api_error",
				"message":   fmt.Sprintf("OpenAI API error: %v", err),
				"retryable": retryable,
			},
		},
		StreamMessage:    fmt.Sprintf("streaming request failed: %v", err),
		DiagnosticsError: err,
		Retryable:        retryable,
	}
}

func isContextWindowExceededError(err error, cfg *config.Config) bool {
	if cfg == nil || !cfg.ContextWindowRewriteEnabled {
		return false
	}
	var statusErr *upstreamStatusError
	if !errors.As(err, &statusErr) || len(statusErr.Body) == 0 {
		return false
	}
	body := strings.ToLower(contextWindowMatchText(statusErr.Body))
	for _, pattern := range cfg.EffectiveContextWindowErrorPatterns() {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern != "" && strings.Contains(body, pattern) {
			return true
		}
	}
	return false
}

func contextWindowMatchText(body []byte) string {
	var fields []string
	var payload map[string]interface{}
	if json.Unmarshal(body, &payload) == nil {
		collectJSONStrings(payload, &fields)
	}
	if len(fields) > 0 {
		fields = append(fields, string(body))
		return strings.Join(fields, " ")
	}
	return string(body)
}

func collectJSONStrings(value interface{}, fields *[]string) {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			if key == "message" || key == "code" || key == "type" || key == "param" || key == "error" {
				collectJSONStrings(child, fields)
			}
		}
	case []interface{}:
		for _, child := range typed {
			collectJSONStrings(child, fields)
		}
	case string:
		*fields = append(*fields, typed)
	}
}

func requestWithoutMaxTokens(req *models.OpenAIRequest, cfg *config.Config) *models.OpenAIRequest {
	retryReq := *req
	retryReq.MaxCompletionTokens = 0
	retryReq.MaxTokens = 0
	cacheKey := config.CacheKey{BaseURL: cfg.OpenAIBaseURL, Model: req.Model}
	config.SetModelCapabilities(cacheKey, &config.ModelCapabilities{UsesMaxCompletionTokens: false})
	return &retryReq
}

func cacheMaxCompletionTokensSupport(req *models.OpenAIRequest, cfg *config.Config, streaming bool) {
	if req.MaxCompletionTokens <= 0 {
		return
	}
	cacheKey := config.CacheKey{BaseURL: cfg.OpenAIBaseURL, Model: req.Model}
	config.SetModelCapabilities(cacheKey, &config.ModelCapabilities{UsesMaxCompletionTokens: true})
	if cfg.Debug {
		if streaming {
			fmt.Printf("[DEBUG] Cached: model %s supports max_completion_tokens (streaming)\n", req.Model)
			return
		}
		fmt.Printf("[DEBUG] Cached: model %s supports max_completion_tokens\n", req.Model)
	}
}

func isRetryableUpstreamError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var statusErr *upstreamStatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return false
}

// isUnsupportedMaxTokensParameter only adapts a model capability after a provider
// explicitly rejects one of the token-limit fields in a validation response.
func isUnsupportedMaxTokensParameter(err error, req *models.OpenAIRequest) bool {
	if req == nil || (req.MaxCompletionTokens <= 0 && req.MaxTokens <= 0) {
		return false
	}
	var statusErr *upstreamStatusError
	if !errors.As(err, &statusErr) || (statusErr.StatusCode != http.StatusBadRequest && statusErr.StatusCode != http.StatusUnprocessableEntity) {
		return false
	}

	var payload struct {
		Error struct {
			Param   string `json:"param"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(statusErr.Body, &payload) == nil && isMaxTokensParameterName(payload.Error.Param) {
		return explicitlyUnsupportedParameter(payload.Error.Message, payload.Error.Code)
	}

	body := strings.ToLower(string(statusErr.Body))
	return (strings.Contains(body, "parameter max_completion_tokens") ||
		strings.Contains(body, "parameter 'max_completion_tokens'") ||
		strings.Contains(body, "parameter \"max_completion_tokens\"") ||
		strings.Contains(body, "parameter max_tokens") ||
		strings.Contains(body, "parameter 'max_tokens'") ||
		strings.Contains(body, "parameter \"max_tokens\"")) &&
		(strings.Contains(body, "unsupported") || strings.Contains(body, "not supported") || strings.Contains(body, "not a valid"))
}

func isMaxTokensParameterName(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "max_tokens" || value == "max_completion_tokens"
}

func explicitlyUnsupportedParameter(message, code string) bool {
	value := strings.ToLower(message + " " + code)
	return strings.Contains(value, "unsupported") || strings.Contains(value, "not supported") || strings.Contains(value, "not a valid") || strings.Contains(value, "unknown parameter")
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

func isResponsesPayload(payload map[string]interface{}) bool {
	if object, _ := payload["object"].(string); object == "response" {
		return true
	}
	if eventType, _ := payload["type"].(string); strings.HasPrefix(eventType, "response.") {
		return true
	}
	_, hasOutput := payload["output"]
	return hasOutput && payload["choices"] == nil
}

// callOpenAIInternal is the internal implementation without retry logic
func callOpenAIInternal(ctx context.Context, req *models.OpenAIRequest, cfg *config.Config, trace *diagnosticsTrace, retry bool) (result *models.OpenAIResponse, err error) {
	attempt := 0
	if trace != nil {
		attempt = trace.beginAttempt(false, retry)
	}
	statusCode := 0
	defer func() {
		if trace != nil {
			trace.endAttempt(attempt, statusCode, err)
		}
	}()

	// Marshal request to JSON
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	if trace != nil {
		trace.setRequestBody(reqBody)
		trace.setUpstreamRequestMetrics(reqBody, len(req.Tools))
		trace.capture(diagnostics.ContentBoundaryUpstreamRequest, attempt+1, reqBody)
	}

	// Build API URL
	apiURL := cfg.ChatCompletionsURL()

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	requestID := ""
	if trace != nil {
		requestID = trace.event.RequestID
	}
	buildUpstreamRequest(httpReq, cfg, requestID)

	// Create HTTP client with timeout
	client, err := newUnaryUpstreamClient(cfg)
	if err != nil {
		return nil, diagnosticFailure(completionUpstreamError, failureUpstreamError, err)
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
		if trace != nil {
			trace.capture(diagnostics.ContentBoundaryUpstreamResponse, attempt+1, respBody)
			trace.setUpstreamResponseBytes(len(respBody))
		}
		return nil, diagnosticFailure(completionUpstreamError, failureUpstreamStatus,
			newUpstreamStatusError(resp.StatusCode, respBody))
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, diagnosticFailure(completionUpstreamError, failureUpstreamRead,
			fmt.Errorf("failed to read response: %w", err))
	}
	if trace != nil {
		trace.capture(diagnostics.ContentBoundaryUpstreamResponse, attempt+1, respBody)
		trace.setUpstreamResponseBytes(len(respBody))
	}

	// Reject native Responses API payloads before decoding them as an empty Chat
	// Completions response. Compatible providers must use /chat/completions.
	var responsePayload map[string]interface{}
	if err := json.Unmarshal(respBody, &responsePayload); err != nil {
		return nil, diagnosticFailure(completionInvalidResponse, failureInvalidResponse,
			fmt.Errorf("failed to parse response: %w", err))
	}
	if isResponsesPayload(responsePayload) {
		return nil, diagnosticFailure(completionInvalidResponse, failureInvalidResponse,
			fmt.Errorf("invalid upstream response: received Responses API payload; expected Chat Completions response with choices"))
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

func logUpstreamFailure(trace *diagnosticsTrace, cfg *config.Config, model string, streaming bool, proxyStatus int, contextWindowExceeded bool, retryable bool, err error) {
	requestID := ""
	if trace != nil {
		requestID = trace.event.RequestID
	}
	upstreamStatus := 0
	var statusErr *upstreamStatusError
	if errors.As(err, &statusErr) {
		upstreamStatus = statusErr.StatusCode
	}
	completionState, failureKind := classifyFailure(err)
	if contextWindowExceeded {
		completionState, failureKind = completionUpstreamError, failureContextWindowExceeded
	}
	fmt.Printf("[ERROR] Upstream request failed request_id=%s provider=%s model=%s streaming=%t proxy_status=%d upstream_status=%d completion=%s failure=%s context_window_exceeded=%t retryable=%t\n",
		requestID, cfg.DetectProvider(), model, streaming, proxyStatus, upstreamStatus, completionState, failureKind, contextWindowExceeded, retryable)
}

func handleCountTokens(c *fiber.Ctx, cfg *config.Config) error {
	// Simple token counting endpoint
	return c.JSON(fiber.Map{
		"input_tokens": 100,
	})
}
