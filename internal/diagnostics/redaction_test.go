package diagnostics

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactJSONPreservesStructureAndRedactsValues(t *testing.T) {
	input := []byte(`{
		"model":"gpt-5",
		"authorization":"Bearer top-secret",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],
		"tools":[{"name":"read_file","description":"reads a file","input_schema":{"type":"object"}}],
		"reasoning_details":[{"type":"reasoning.text","text":"private thought"}],
		"count":2,
		"stream":true
	}`)

	encoded, err := RedactJSON(input, RedactionOptions{})
	if err != nil {
		t.Fatalf("RedactJSON() error = %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatalf("unmarshal redacted JSON: %v", err)
	}
	if result["model"] != "gpt-5" || result["count"] != float64(2) || result["stream"] != true {
		t.Fatalf("safe scalar values were not preserved: %#v", result)
	}

	authorization := result["authorization"].(map[string]any)
	assertDescriptor(t, authorization, "string", len("Bearer top-secret"))

	messages := result["messages"].([]any)
	message := messages[0].(map[string]any)
	if message["role"] != "user" {
		t.Fatalf("message structure not preserved: %#v", message)
	}
	content := message["content"].([]any)
	block := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("content block type not preserved: %#v", block)
	}
	assertDescriptor(t, block["text"].(map[string]any), "string", len("hello"))

	tools := result["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "read_file" {
		t.Fatalf("tool structure not preserved: %#v", tool)
	}
	assertDescriptor(t, tool["description"].(map[string]any), "string", len("reads a file"))
	if _, ok := tool["input_schema"].(map[string]any); !ok {
		t.Fatalf("tool input schema was not represented safely: %#v", tool["input_schema"])
	}

	if strings.Contains(string(encoded), "top-secret") || strings.Contains(string(encoded), "hello") || strings.Contains(string(encoded), "private thought") {
		t.Fatalf("redacted JSON leaked a protected value: %s", encoded)
	}
}

func TestRedactJSONBoundsRecursion(t *testing.T) {
	input := []byte(`{"a":{"b":{"c":{"d":"secret"}}}}`)
	encoded, err := RedactJSON(input, RedactionOptions{MaxDepth: 2, MaxNodes: 100})
	if err != nil {
		t.Fatalf("RedactJSON() error = %v", err)
	}
	if !strings.Contains(string(encoded), `"reason":"max_depth"`) {
		t.Fatalf("expected max-depth descriptor, got %s", encoded)
	}

	encoded, err = RedactJSON([]byte(`{"a":1,"b":2,"c":3}`), RedactionOptions{MaxDepth: 10, MaxNodes: 2})
	if err != nil {
		t.Fatalf("RedactJSON() error = %v", err)
	}
	if !strings.Contains(string(encoded), `"reason":"max_nodes"`) {
		t.Fatalf("expected max-nodes descriptor, got %s", encoded)
	}
}

func TestRedactJSONRejectsMalformedAndTrailingData(t *testing.T) {
	for _, input := range []string{`{"messages":[`, `{} {}`} {
		if _, err := RedactJSON([]byte(input), RedactionOptions{}); err == nil {
			t.Fatalf("RedactJSON(%q) expected error", input)
		}
	}
}

func assertDescriptor(t *testing.T, descriptor map[string]any, wantType string, wantLength int) {
	t.Helper()
	if descriptor["redacted"] != true || descriptor["type"] != wantType || descriptor["length"] != float64(wantLength) {
		t.Fatalf("unexpected redaction descriptor: %#v", descriptor)
	}
	hash, _ := descriptor["sha256"].(string)
	if len(hash) != 64 {
		t.Fatalf("expected SHA-256 hash, got %q", hash)
	}
}
