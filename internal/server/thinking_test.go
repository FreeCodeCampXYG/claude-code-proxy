package server

import (
	"bufio"
	"bytes"
	"encoding/json"
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
			outcome := streamOpenAIToClaude(bufio.NewWriter(&output), strings.NewReader(input), "gpt-test", &config.Config{}, time.Now())
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
