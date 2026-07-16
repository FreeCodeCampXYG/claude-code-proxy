package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
)

func TestStreamOpenAIToClaudeUsesThinkingField(t *testing.T) {
	tests := []struct {
		name              string
		delta             string
		wantThinking      string
		wantSignature     string
	}{
		{name: "reasoning content", delta: `{"reasoning_content":"newapi reasoning"}`, wantThinking: "newapi reasoning"},
		{name: "reasoning", delta: `{"reasoning":"provider reasoning"}`, wantThinking: "provider reasoning"},
		{name: "reasoning details", delta: `{"reasoning_details":[{"type":"reasoning.text","text":"openrouter reasoning"}]}`, wantThinking: "openrouter reasoning"},
		{name: "reasoning details signature", delta: `{"reasoning_details":[{"type":"reasoning.text","text":"openrouter reasoning","signature":"provider-signature"}]}`, wantThinking: "openrouter reasoning", wantSignature: "provider-signature"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := "data: {\"choices\":[{\"delta\":" + tt.delta + ",\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n"
			var output bytes.Buffer
			outcome := streamOpenAIToClaude(bufio.NewWriter(&output), strings.NewReader(input), "claude-sonnet-4", "gpt-test", &config.Config{}, time.Now(), nil)
			if outcome.Err != nil {
				t.Fatalf("streamOpenAIToClaude() error = %v", outcome.Err)
			}

			foundThinkingDelta := false
			foundSignatureDelta := false
			for _, event := range decodeSSEEvents(t, output.String()) {
				if event["event"] != "content_block_delta" {
					continue
				}
				var payload map[string]interface{}
				if err := json.Unmarshal([]byte(event["data"]), &payload); err != nil {
					t.Fatal(err)
				}
				delta, _ := payload["delta"].(map[string]interface{})
				if delta["type"] == "signature_delta" {
					if delta["signature"] != tt.wantSignature {
						t.Fatalf("signature delta = %#v, want %q", delta, tt.wantSignature)
					}
					foundSignatureDelta = true
					continue
				}
				if delta["type"] != "thinking_delta" {
					continue
				}
				foundThinkingDelta = true
				if got, ok := delta["thinking"].(string); !ok || got != tt.wantThinking {
					t.Fatalf("thinking delta = %#v, want thinking %q", delta, tt.wantThinking)
				}
				if _, exists := delta["text"]; exists {
					t.Fatalf("thinking delta must not use text field: %#v", delta)
				}
			}
			if !foundThinkingDelta {
				t.Fatalf("no thinking delta in output: %s", output.String())
			}
			if tt.wantSignature != "" && !foundSignatureDelta {
				t.Fatalf("no signature delta in output: %s", output.String())
			}
		})
	}
}

func TestStreamOpenAIToClaudeEventOrderingAndUsageAfterFinish(t *testing.T) {
	input := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"think"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"answer"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call-b","function":{"name":"second","arguments":"{\"b\":"}},{"index":0,"id":"call-a","function":{"name":"first","arguments":"{\"a\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}},{"index":1,"function":{"arguments":"2}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":5}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n"

	var output bytes.Buffer
	outcome := streamOpenAIToClaude(bufio.NewWriter(&output), strings.NewReader(input), "claude-sonnet-4", "gpt-test", &config.Config{}, time.Now(), nil)
	if outcome.Err != nil {
		t.Fatalf("streamOpenAIToClaude() error = %v", outcome.Err)
	}
	if got := outcome.Usage["output_tokens"]; got != 5 {
		t.Fatalf("output tokens = %#v, want 5", got)
	}

	events := decodeSSEEvents(t, output.String())
	var starts, stops []int
	var toolNames []string
	var partials []string
	toolStartPosition := -1
	thinkingStopPosition, textStartPosition, textStopPosition := -1, -1, -1
	for position, event := range events {
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(event["data"]), &payload); err != nil {
			t.Fatal(err)
		}
		index, _ := payload["index"].(float64)
		switch event["event"] {
		case "message_start":
			message := payload["message"].(map[string]interface{})
			if message["model"] != "claude-sonnet-4" {
				t.Fatalf("message_start model = %#v", message["model"])
			}
		case "content_block_start":
			starts = append(starts, int(index))
			block := payload["content_block"].(map[string]interface{})
			if block["type"] == "text" {
				textStartPosition = position
			}
			if block["type"] == "tool_use" {
				if toolStartPosition < 0 {
					toolStartPosition = position
				}
				toolNames = append(toolNames, block["name"].(string))
			}
		case "content_block_stop":
			stops = append(stops, int(index))
			if int(index) == 0 {
				thinkingStopPosition = position
			}
			if int(index) == 1 {
				textStopPosition = position
			}
		case "content_block_delta":
			delta := payload["delta"].(map[string]interface{})
			if delta["type"] == "input_json_delta" {
				partials = append(partials, delta["partial_json"].(string))
			}
		}
	}
	if strings.Trim(strings.Join([]string{formatInts(starts), formatInts(stops)}, ";"), ";") != "0,1,2,3;0,1,2,3" {
		t.Fatalf("starts/stops = %v/%v", starts, stops)
	}
	if thinkingStopPosition < 0 || textStartPosition < 0 || thinkingStopPosition > textStartPosition {
		t.Fatalf("thinking stop position=%d text start position=%d", thinkingStopPosition, textStartPosition)
	}
	if textStopPosition < 0 || toolStartPosition < 0 || textStopPosition > toolStartPosition {
		t.Fatalf("text stop position=%d tool start position=%d", textStopPosition, toolStartPosition)
	}
	if strings.Join(toolNames, ",") != "first,second" {
		t.Fatalf("tool order = %v", toolNames)
	}
	if strings.Join(partials, "|") != `{"a":|1}|{"b":|2}` {
		t.Fatalf("tool fragments = %q", partials)
	}
	assertSequentialContentBlockLifecycle(t, events)
	if events[len(events)-2]["event"] != "message_delta" || events[len(events)-1]["event"] != "message_stop" {
		t.Fatalf("unexpected terminal events: %#v", events[len(events)-2:])
	}
}

func TestStreamOpenAIToClaudePreservesToolArgumentFragmentsUntilFinalization(t *testing.T) {
	input := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"read_file"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n"

	var output bytes.Buffer
	outcome := streamOpenAIToClaude(bufio.NewWriter(&output), strings.NewReader(input), "claude", "gpt", &config.Config{}, time.Now(), nil)
	if outcome.Err != nil {
		t.Fatal(outcome.Err)
	}

	events := decodeSSEEvents(t, output.String())
	toolStart := -1
	var partialPositions []int
	var partials []string
	for position, event := range events {
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(event["data"]), &payload); err != nil {
			t.Fatal(err)
		}
		if event["event"] == "content_block_start" {
			block, _ := payload["content_block"].(map[string]interface{})
			if block["type"] == "tool_use" {
				toolStart = position
			}
		}
		if event["event"] == "content_block_delta" {
			delta, _ := payload["delta"].(map[string]interface{})
			if delta["type"] == "input_json_delta" {
				partialPositions = append(partialPositions, position)
				partials = append(partials, delta["partial_json"].(string))
			}
		}
	}
	if toolStart < 0 || len(partialPositions) != 2 || partialPositions[0] != toolStart+1 {
		t.Fatalf("tool start=%d partial positions=%v events=%#v", toolStart, partialPositions, events)
	}
	if len(partials) != 2 || partials[0] != `{"path":` || partials[1] != `"a.txt"}` {
		t.Fatalf("tool argument fragments = %q", partials)
	}
	assertSequentialContentBlockLifecycle(t, events)
}

func TestStreamOpenAIToClaudeReopenedTypesUseNewContiguousIndices(t *testing.T) {
	input := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"think-1"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"text-1"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"think-2"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"text-2"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n"

	var output bytes.Buffer
	outcome := streamOpenAIToClaude(bufio.NewWriter(&output), strings.NewReader(input), "claude", "gpt", &config.Config{}, time.Now(), nil)
	if outcome.Err != nil {
		t.Fatal(outcome.Err)
	}

	events := decodeSSEEvents(t, output.String())
	var starts, stops []int
	var types []string
	for _, event := range events {
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(event["data"]), &payload); err != nil {
			t.Fatal(err)
		}
		index, _ := payload["index"].(float64)
		switch event["event"] {
		case "content_block_start":
			starts = append(starts, int(index))
			block := payload["content_block"].(map[string]interface{})
			types = append(types, block["type"].(string))
		case "content_block_stop":
			stops = append(stops, int(index))
		}
	}
	if formatInts(starts) != "0,1,2,3" || formatInts(stops) != "0,1,2,3" {
		t.Fatalf("starts/stops = %v/%v", starts, stops)
	}
	if strings.Join(types, ",") != "thinking,text,thinking,text" {
		t.Fatalf("block types = %v", types)
	}
	assertSequentialContentBlockLifecycle(t, events)
}

func TestStreamOpenAIToClaudeNormalizesCacheUsage(t *testing.T) {
	input := strings.Join([]string{
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":6},"cache_creation_input_tokens":2}}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n"
	var output bytes.Buffer
	outcome := streamOpenAIToClaude(bufio.NewWriter(&output), strings.NewReader(input), "claude", "gpt", &config.Config{}, time.Now(), nil)
	if outcome.Err != nil {
		t.Fatal(outcome.Err)
	}
	if usageInt(outcome.Usage, "cache_read_input_tokens") != 6 || usageInt(outcome.Usage, "cache_creation_input_tokens") != 2 {
		t.Fatalf("usage = %#v", outcome.Usage)
	}
}

func TestStreamOpenAIToClaudeMalformedJSONIsInvalidResponse(t *testing.T) {
	input := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`,
		`data: {not-json}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n"

	var output bytes.Buffer
	canceled := false
	outcome := streamOpenAIToClaude(bufio.NewWriter(&output), strings.NewReader(input), "claude", "gpt", &config.Config{}, time.Now(), func() { canceled = true })
	completionState, failureKind := classifyFailure(outcome.Err)
	if outcome.Err == nil || completionState != completionInvalidResponse || failureKind != failureInvalidResponse {
		t.Fatalf("error = %v, completion state = %q, failure kind = %q", outcome.Err, completionState, failureKind)
	}
	if outcome.Chunks != 1 {
		t.Fatalf("chunks = %d, want only the valid parsed chunk", outcome.Chunks)
	}
	if !canceled {
		t.Fatal("upstream context was not canceled on conversion failure")
	}
	if strings.Contains(output.String(), "event: message_delta") || strings.Contains(output.String(), "event: message_stop") {
		t.Fatalf("malformed stream emitted success terminal events: %s", output.String())
	}
	if !strings.Contains(output.String(), "event: error") {
		t.Fatalf("malformed stream missing error event: %s", output.String())
	}
}

func TestStreamOpenAIToClaudeCancelsUpstreamOnWriterFailure(t *testing.T) {
	writer := bufio.NewWriterSize(failingWriter{}, 1)
	canceled := make(chan struct{})
	cancel := func() {
		select {
		case <-canceled:
		default:
			close(canceled)
		}
	}

	outcome := streamOpenAIToClaude(writer, strings.NewReader("data: [DONE]\n\n"), "claude", "gpt", &config.Config{}, time.Now(), cancel)
	completionState, failureKind := classifyFailure(outcome.Err)
	if outcome.Err == nil || completionState != completionDownstreamWrite || failureKind != failureDownstreamWrite {
		t.Fatalf("writer failure = %v, state=%q kind=%q", outcome.Err, completionState, failureKind)
	}
	select {
	case <-canceled:
	default:
		t.Fatal("upstream context was not canceled at the reliable writer-failure boundary")
	}
}

func TestStreamOpenAIToClaudeReasoningPrecedence(t *testing.T) {
	input := `data: {"choices":[{"delta":{"reasoning_content":"preferred","reasoning":"duplicate","reasoning_details":[{"type":"reasoning.text","text":"also duplicate","signature":"real-signature"}]},"finish_reason":"stop"}]}` + "\n\n"
	var output bytes.Buffer
	outcome := streamOpenAIToClaude(bufio.NewWriter(&output), strings.NewReader(input), "claude", "gpt", &config.Config{}, time.Now(), nil)
	if outcome.Err != nil {
		t.Fatal(outcome.Err)
	}
	if strings.Count(output.String(), "preferred") != 1 || strings.Contains(output.String(), "duplicate") || strings.Count(output.String(), "real-signature") != 1 {
		t.Fatalf("unexpected reasoning output: %s", output.String())
	}
	if !strings.Contains(output.String(), `"type":"signature_delta"`) {
		t.Fatalf("compatible signature was not emitted: %s", output.String())
	}
}

func TestStreamOpenAIToClaudeTruncatedStreamHasNoSuccessTerminalEvents(t *testing.T) {
	input := `data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n"
	var output bytes.Buffer
	outcome := streamOpenAIToClaude(bufio.NewWriter(&output), strings.NewReader(input), "claude", "gpt", &config.Config{}, time.Now(), nil)
	completionState, failureKind := classifyFailure(outcome.Err)
	if outcome.Err == nil || completionState != completionUpstreamError || failureKind != failureUpstreamTruncated {
		t.Fatalf("error = %v, state=%q kind=%q", outcome.Err, completionState, failureKind)
	}
	if strings.Contains(output.String(), "event: message_delta") || strings.Contains(output.String(), "event: message_stop") {
		t.Fatalf("truncated stream emitted success terminal events: %s", output.String())
	}
	if !strings.Contains(output.String(), "event: error") {
		t.Fatalf("truncated stream missing error event: %s", output.String())
	}
}

func TestStreamOpenAIToClaudeReadFailureHasNoSuccessTerminalEvents(t *testing.T) {
	reader := io.MultiReader(strings.NewReader(`data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n"), failingReader{})
	var output bytes.Buffer
	outcome := streamOpenAIToClaude(bufio.NewWriter(&output), reader, "claude", "gpt", &config.Config{}, time.Now(), nil)
	completionState, failureKind := classifyFailure(outcome.Err)
	if outcome.Err == nil || completionState != completionUpstreamError || failureKind != failureUpstreamRead {
		t.Fatalf("error = %v, state=%q kind=%q", outcome.Err, completionState, failureKind)
	}
	if strings.Contains(output.String(), "event: message_delta") || strings.Contains(output.String(), "event: message_stop") {
		t.Fatalf("read failure emitted success terminal events: %s", output.String())
	}
}

func TestStreamOpenAIToClaudeReturnsWriterFailure(t *testing.T) {
	writer := bufio.NewWriterSize(failingWriter{}, 1)
	outcome := streamOpenAIToClaude(writer, strings.NewReader("data: [DONE]\n\n"), "claude", "gpt", &config.Config{}, time.Now(), nil)
	if outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "write SSE event") {
		t.Fatalf("error = %v", outcome.Err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("reader failed") }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("writer failed") }

func assertSequentialContentBlockLifecycle(t *testing.T, events []map[string]string) {
	t.Helper()
	openIndex := -1
	nextIndex := 0
	for position, event := range events {
		if event["event"] != "content_block_start" && event["event"] != "content_block_delta" && event["event"] != "content_block_stop" {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(event["data"]), &payload); err != nil {
			t.Fatal(err)
		}
		index := int(payload["index"].(float64))
		switch event["event"] {
		case "content_block_start":
			if openIndex >= 0 {
				t.Fatalf("block %d still open when block %d starts at event %d", openIndex, index, position)
			}
			if index != nextIndex {
				t.Fatalf("block start index = %d, want contiguous %d at event %d", index, nextIndex, position)
			}
			openIndex = index
			nextIndex++
		case "content_block_delta":
			if index != openIndex {
				t.Fatalf("delta index = %d while open block = %d at event %d", index, openIndex, position)
			}
		case "content_block_stop":
			if index != openIndex {
				t.Fatalf("stop index = %d while open block = %d at event %d", index, openIndex, position)
			}
			openIndex = -1
		}
	}
	if openIndex >= 0 {
		t.Fatalf("block %d remains open", openIndex)
	}
}

func formatInts(values []int) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = fmt.Sprint(value)
	}
	return strings.Join(parts, ",")
}

func decodeSSEEvents(t *testing.T, output string) []map[string]string {
	t.Helper()
	var events []map[string]string
	for _, block := range strings.Split(strings.TrimSpace(output), "\n\n") {
		event := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			if value, ok := strings.CutPrefix(line, "event: "); ok {
				event["event"] = value
			}
			if value, ok := strings.CutPrefix(line, "data: "); ok {
				event["data"] = value
			}
		}
		if event["event"] != "" {
			events = append(events, event)
		}
	}
	return events
}
