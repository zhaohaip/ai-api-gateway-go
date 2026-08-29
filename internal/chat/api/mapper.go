package api

import (
	openaiapi "github.com/zhaohaip/ai-api-gateway-go/api/openai"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

func toDomainRequest(request openaiapi.ChatCompletionRequest) domain.ChatRequest {
	messages := make([]domain.Message, 0, len(request.Messages))
	for _, message := range request.Messages {
		messages = append(messages, domain.Message{
			Role:    domain.MessageRole(message.Role),
			Content: message.Content,
		})
	}
	return domain.ChatRequest{
		Model:               request.Model,
		Messages:            messages,
		Temperature:         request.Temperature,
		MaxCompletionTokens: request.MaxCompletionTokens,
	}
}

func toOpenAIResponse(id string, created int64, requestedModel string, response domain.ChatResponse) openaiapi.ChatCompletionResponse {
	result := openaiapi.ChatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   requestedModel,
		Choices: []openaiapi.Choice{
			{
				Index: 0,
				Message: openaiapi.Message{
					Role:    string(response.Message.Role),
					Content: response.Message.Content,
				},
				FinishReason: response.FinishReason,
			},
		},
	}
	if response.Usage != nil {
		result.Usage = &openaiapi.Usage{
			PromptTokens:     response.Usage.PromptTokens,
			CompletionTokens: response.Usage.CompletionTokens,
			TotalTokens:      response.Usage.TotalTokens,
		}
	}
	return result
}

func toOpenAIChunk(
	id string,
	created int64,
	requestedModel string,
	chunk domain.ChatChunk,
	includeAssistantRole bool,
) openaiapi.ChatCompletionChunk {
	result := openaiapi.ChatCompletionChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   requestedModel,
		Choices: []openaiapi.StreamChoice{},
	}

	if includeAssistantRole || chunk.Delta.Content != nil || chunk.Delta.ReasoningContent != nil || chunk.FinishReason != nil {
		delta := openaiapi.MessageDelta{
			Content:          chunk.Delta.Content,
			ReasoningContent: chunk.Delta.ReasoningContent,
		}
		if includeAssistantRole {
			role := string(domain.RoleAssistant)
			delta.Role = &role
		}
		result.Choices = append(result.Choices, openaiapi.StreamChoice{
			Index:        0,
			Delta:        delta,
			FinishReason: chunk.FinishReason,
		})
	}
	if chunk.Usage != nil {
		result.Usage = &openaiapi.Usage{
			PromptTokens:     chunk.Usage.PromptTokens,
			CompletionTokens: chunk.Usage.CompletionTokens,
			TotalTokens:      chunk.Usage.TotalTokens,
		}
	}
	return result
}

func hasOpenAIChunkData(chunk domain.ChatChunk, includeAssistantRole bool) bool {
	return includeAssistantRole || chunk.Delta.Content != nil || chunk.Delta.ReasoningContent != nil ||
		chunk.FinishReason != nil || chunk.Usage != nil
}
