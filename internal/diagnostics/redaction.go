package diagnostics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
)

const (
	DefaultMaxRedactionDepth = 32
	DefaultMaxRedactionNodes = 10000
)

type RedactionOptions struct {
	MaxDepth int
	MaxNodes int
}

type RedactedValue struct {
	Redacted bool   `json:"redacted"`
	Type     string `json:"type"`
	Length   int    `json:"length"`
}

type RedactionLimit struct {
	Redacted bool   `json:"redacted"`
	Reason   string `json:"reason"`
	Type     string `json:"type"`
	Length   int    `json:"length"`
}

var secretKeys = map[string]struct{}{
	"access_token": {}, "api_key": {}, "apikey": {}, "authorization": {},
	"bearer_token": {}, "client_secret": {}, "cookie": {}, "openai_api_key": {},
	"password": {}, "private_key": {}, "proxy_authorization": {},
	"refresh_token": {}, "secret": {}, "set_cookie": {}, "token": {},
	"x_api_key": {}, "x_auth_token": {},
}

var conversationalKeys = map[string]struct{}{
	"arguments": {}, "content": {}, "description": {}, "input": {},
	"instructions": {}, "messages": {}, "output": {}, "prompt": {},
	"reasoning": {}, "reasoning_content": {}, "reasoning_details": {},
	"signature": {}, "system": {}, "text": {}, "thinking": {},
	"tool_calls": {}, "tool_result": {}, "tool_results": {}, "tools": {},
}

var pathKeys = map[string]struct{}{
	"path": {}, "file": {}, "filename": {}, "file_path": {}, "cwd": {},
	"directory": {}, "workdir": {}, "workspace": {},
}

func RedactJSON(body []byte, options RedactionOptions) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON for redaction: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode JSON for redaction: multiple JSON values")
		}
		return nil, fmt.Errorf("decode trailing JSON for redaction: %w", err)
	}

	redacted := Redact(value, options)
	result, err := json.Marshal(redacted)
	if err != nil {
		return nil, fmt.Errorf("encode redacted JSON: %w", err)
	}
	return result, nil
}

// RedactSecretsJSON preserves diagnostic content while removing credential values. It is
// intentionally narrower than RedactJSON, which also removes conversational content.
func RedactSecretsJSON(body []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON for secret redaction: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode JSON for secret redaction: multiple JSON values")
		}
		return nil, fmt.Errorf("decode trailing JSON for secret redaction: %w", err)
	}
	return json.Marshal(redactSecrets(value))
}

func redactSecrets(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			if _, secret := secretKeys[normalizeKey(key)]; secret {
				result[key] = describeRedacted(child)
				continue
			}
			result[key] = redactSecrets(child)
			if _, isPath := pathKeys[normalizeKey(key)]; isPath {
				if value, ok := result[key].(string); ok {
					result[key] = pathBaseName(value)
				}
			}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = redactSecrets(child)
		}
		return result
	case string:
		var nested any
		if json.Unmarshal([]byte(typed), &nested) == nil {
			if encoded, err := json.Marshal(redactSecrets(nested)); err == nil {
				return string(encoded)
			}
		}
		return typed
	default:
		return value
	}
}

func Redact(value any, options RedactionOptions) any {
	if options.MaxDepth <= 0 {
		options.MaxDepth = DefaultMaxRedactionDepth
	}
	if options.MaxNodes <= 0 {
		options.MaxNodes = DefaultMaxRedactionNodes
	}

	state := redactionState{options: options}
	return state.walk(value, "", 0, false)
}

type redactionState struct {
	options RedactionOptions
	nodes   int
}

func (state *redactionState) walk(value any, key string, depth int, force bool) any {
	force = force || shouldRedactKey(key)
	if depth >= state.options.MaxDepth {
		return describeLimit(value, "max_depth")
	}
	state.nodes++
	if state.nodes > state.options.MaxNodes {
		return describeLimit(value, "max_nodes")
	}

	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			childForce := force && !isStructuralKey(childKey)
			result[childKey] = state.walk(childValue, childKey, depth+1, childForce)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for i, childValue := range typed {
			result[i] = state.walk(childValue, key, depth+1, force)
		}
		return result
	default:
		if force {
			return describeRedacted(value)
		}
		if _, isPath := pathKeys[normalizeKey(key)]; isPath {
			if value, ok := typed.(string); ok {
				return pathBaseName(value)
			}
		}
		return value
	}
}

func shouldRedactKey(key string) bool {
	normalized := normalizeKey(key)
	if _, ok := secretKeys[normalized]; ok {
		return true
	}
	_, ok := conversationalKeys[normalized]
	return ok
}

func isStructuralKey(key string) bool {
	switch normalizeKey(key) {
	case "id", "index", "name", "role", "type":
		return true
	default:
		return false
	}
}

func normalizeKey(key string) string {
	key = strings.TrimSpace(strings.ToLower(key))
	key = strings.NewReplacer("-", "_", " ", "_", ".", "_").Replace(key)
	return key
}

func describeRedacted(value any) RedactedValue {
	typeName, length := describeValue(value)
	return RedactedValue{Redacted: true, Type: typeName, Length: length}
}

func describeLimit(value any, reason string) RedactionLimit {
	typeName, length := describeValue(value)
	return RedactionLimit{Redacted: true, Reason: reason, Type: typeName, Length: length}
}

func describeValue(value any) (string, int) {
	return jsonType(value), valueLength(value)
}

func pathBaseName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	value = strings.TrimRight(strings.ReplaceAll(value, "\\", "/"), "/")
	if value == "" {
		return ""
	}
	return path.Base(value)
}

func jsonType(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number, float32, float64, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func valueLength(value any) int {
	switch typed := value.(type) {
	case nil:
		return 0
	case string:
		return len([]byte(typed))
	case []any:
		return len(typed)
	case map[string]any:
		return len(typed)
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return len(fmt.Sprintf("%v", value))
		}
		return len(encoded)
	}
}
