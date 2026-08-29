package api

import (
	"testing"

	openaiapi "github.com/zhaohaip/ai-api-gateway-go/api/openai"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

func TestToDomainRequest(t *testing.T) {
	temperature := float32(0)
	maxCompletionTokens := 128
	request := openaiapi.ChatCompletionRequest{
		Model: "default-chat",
		Messages: []openaiapi.Message{
			{Role: "system", Content: "rules"},
			{Role: "user", Content: "hello"},
		},
		Temperature:         &temperature,
		MaxCompletionTokens: &maxCompletionTokens,
	}

	result := toDomainRequest(request)
	if result.Model != request.Model {
		t.Fatalf("model = %q, want %q", result.Model, request.Model)
	}
	if len(result.Messages) != 2 || result.Messages[0].Role != domain.RoleSystem || result.Messages[1].Content != "hello" {
		t.Fatalf("messages = %#v", result.Messages)
	}
	if result.Temperature == nil || *result.Temperature != 0 {
		t.Fatalf("temperature = %v, want pointer to zero", result.Temperature)
	}
	if result.MaxCompletionTokens == nil || *result.MaxCompletionTokens != 128 {
		t.Fatalf("max completion tokens = %v", result.MaxCompletionTokens)
	}
}
