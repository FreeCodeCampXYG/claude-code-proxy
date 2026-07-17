package converter

import (
	"encoding/json"
	"testing"

	"github.com/claude-code-proxy/proxy/pkg/models"
)

func TestOpenAIResponseDecodesFlexibleReasoningFields(t *testing.T) {
	tests := []struct {
		name          string
		message       string
		wantThinking  string
		wantSignature string
	}{
		{
			name:          "reasoning content object wins over other fields",
			message:       `{"reasoning_content":{"text":"content object"},"reasoning":"secondary","reasoning_details":[{"type":"reasoning.text","text":"detail","signature":"detail-signature"}]}`,
			wantThinking:  "content object",
			wantSignature: "",
		},
		{
			name:          "reasoning array accepts signed object",
			message:       `{"reasoning_content":[],"reasoning":[{"summary":"reasoning array","signature":"reasoning-signature"}],"reasoning_details":{"type":"reasoning.text","text":"detail"}}`,
			wantThinking:  "reasoning array",
			wantSignature: "reasoning-signature",
		},
		{
			name:          "details object skips encrypted fragment",
			message:       `{"reasoning_details":[{"type":"reasoning.encrypted","data":"opaque"},{"type":"reasoning.summary","summary":"visible summary","signature":"summary-signature"}]}`,
			wantThinking:  "visible summary",
			wantSignature: "summary-signature",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var response models.OpenAIResponse
			raw := `{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":` + tt.message + `,"finish_reason":"stop"}]}`
			if err := json.Unmarshal([]byte(raw), &response); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}

			converted, err := ConvertResponse(&response, "claude-test")
			if err != nil {
				t.Fatalf("ConvertResponse() error = %v", err)
			}
			if len(converted.Content) != 1 {
				t.Fatalf("content = %#v, want one thinking block", converted.Content)
			}
			block := converted.Content[0]
			if block.Type != "thinking" || block.Thinking != tt.wantThinking || block.Signature != tt.wantSignature {
				t.Fatalf("thinking block = %#v, want thinking=%q signature=%q", block, tt.wantThinking, tt.wantSignature)
			}
		})
	}
}

func TestNormalizeReasoningFallsBackOnlyWhenHigherPriorityIsUnusable(t *testing.T) {
	blocks := NormalizeReasoning(
		map[string]interface{}{"type": "reasoning.opaque", "text": "not visible", "signature": "not-forwarded"},
		map[string]interface{}{"summary": "fallback reasoning", "signature": "reasoning-signature"},
		[]interface{}{map[string]interface{}{"type": "reasoning.text", "text": "detail reasoning", "signature": "detail-signature"}},
	)
	if len(blocks) != 1 {
		t.Fatalf("NormalizeReasoning() = %#v, want one fallback block", blocks)
	}
	if blocks[0].Text != "fallback reasoning" || blocks[0].Signature != "reasoning-signature" || blocks[0].Source != "reasoning" {
		t.Fatalf("fallback block = %#v", blocks[0])
	}
}
