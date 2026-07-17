package diagnostics

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDescribeMalformedBody(t *testing.T) {
	body := []byte(`{"messages":[`)
	metadata := DescribeMalformedBody(body, "application/json", errors.New("unexpected EOF"))
	if !metadata.Malformed || metadata.ContentType != "application/json" || metadata.Length != len(body) {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
	if metadata.Error != "unexpected EOF" || metadata.Preview != "" {
		t.Fatalf("malformed metadata missing safe details: %#v", metadata)
	}
	encoded := MalformedBodyJSON(body, "application/json", errors.New("unexpected EOF"))
	if !json.Valid(encoded) || strings.Contains(string(encoded), string(body)) {
		t.Fatalf("MalformedBodyJSON leaked body or returned invalid JSON: %s", encoded)
	}
}
