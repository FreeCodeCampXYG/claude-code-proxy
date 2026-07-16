package converter

import (
	"testing"

	"github.com/claude-code-proxy/proxy/pkg/models"
)

func TestConvertResponsePreservesReasoningContent(t *testing.T) {
	finishReason := "stop"
	response, err := ConvertResponse(&models.OpenAIResponse{
		ID: "response-id",
		Choices: []models.OpenAIChoice{{
			Message: models.OpenAIMessage{
				Role:             "assistant",
				ReasoningContent: "provider reasoning",
				Content:          "final answer",
			},
			FinishReason: &finishReason,
		}},
	}, "claude-sonnet-test")
	if err != nil {
		t.Fatalf("ConvertResponse() error = %v", err)
	}
	if len(response.Content) != 2 {
		t.Fatalf("content blocks = %#v, want thinking then text", response.Content)
	}
	if response.Content[0].Type != "thinking" || response.Content[0].Thinking != "provider reasoning" {
		t.Fatalf("first block = %#v, want reasoning thinking block", response.Content[0])
	}
	if response.Content[0].Signature != "" {
		t.Fatalf("unexpected fabricated signature: %#v", response.Content[0])
	}
	if response.Content[1].Type != "text" || response.Content[1].Text != "final answer" {
		t.Fatalf("second block = %#v, want text block", response.Content[1])
	}
}

func TestConvertResponsePreservesReasoningDetailSignature(t *testing.T) {
	response, err := ConvertResponse(&models.OpenAIResponse{
		Choices: []models.OpenAIChoice{{
			Message: models.OpenAIMessage{ReasoningDetails: []interface{}{map[string]interface{}{
				"type":      "reasoning.text",
				"text":      "provider reasoning",
				"signature": "provider-signature",
			}}},
		}},
	}, "claude-sonnet-test")
	if err != nil {
		t.Fatalf("ConvertResponse() error = %v", err)
	}
	if len(response.Content) != 1 || response.Content[0].Signature != "provider-signature" {
		t.Fatalf("content blocks = %#v, want provider signature", response.Content)
	}
}
