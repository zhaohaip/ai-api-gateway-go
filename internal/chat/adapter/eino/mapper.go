package eino

import (
	"fmt"

	"github.com/cloudwego/eino/schema"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

func toEinoMessages(messages []domain.Message) ([]*schema.Message, error) {
	result := make([]*schema.Message, 0, len(messages))
	for index, message := range messages {
		role, err := toEinoRole(message.Role)
		if err != nil {
			return nil, fmt.Errorf("map message %d: %w", index, err)
		}
		result = append(result, &schema.Message{
			Role:    role,
			Content: message.Content,
		})
	}
	return result, nil
}

func toEinoRole(role domain.MessageRole) (schema.RoleType, error) {
	switch role {
	case domain.RoleSystem:
		return schema.System, nil
	case domain.RoleUser:
		return schema.User, nil
	case domain.RoleAssistant:
		return schema.Assistant, nil
	default:
		return "", fmt.Errorf("unsupported internal message role %q", role)
	}
}

func toDomainResponse(model string, message *schema.Message) (domain.ChatResponse, error) {
	if message == nil {
		return domain.ChatResponse{}, fmt.Errorf("upstream returned no message")
	}
	if message.Role != schema.Assistant {
		return domain.ChatResponse{}, fmt.Errorf("upstream returned unsupported role %q", message.Role)
	}
	if len(message.ToolCalls) > 0 || len(message.AssistantGenMultiContent) > 0 || len(message.MultiContent) > 0 {
		return domain.ChatResponse{}, fmt.Errorf("upstream returned unsupported non-text content")
	}

	response := domain.ChatResponse{
		Model: model,
		Message: domain.Message{
			Role:    domain.RoleAssistant,
			Content: message.Content,
		},
	}
	if message.ResponseMeta == nil {
		return response, nil
	}
	if message.ResponseMeta.FinishReason != "" {
		finishReason := message.ResponseMeta.FinishReason
		response.FinishReason = &finishReason
	}
	if message.ResponseMeta.Usage != nil {
		response.Usage = &domain.Usage{
			PromptTokens:     message.ResponseMeta.Usage.PromptTokens,
			CompletionTokens: message.ResponseMeta.Usage.CompletionTokens,
			TotalTokens:      message.ResponseMeta.Usage.TotalTokens,
		}
	}
	return response, nil
}

func toDomainChunk(message *schema.Message) (domain.ChatChunk, error) {
	if message == nil {
		return domain.ChatChunk{}, fmt.Errorf("upstream returned no stream message")
	}
	if message.Role != "" && message.Role != schema.Assistant {
		return domain.ChatChunk{}, fmt.Errorf("upstream returned unsupported stream role %q", message.Role)
	}
	if len(message.ToolCalls) > 0 || len(message.AssistantGenMultiContent) > 0 || len(message.MultiContent) > 0 {
		return domain.ChatChunk{}, fmt.Errorf("upstream returned unsupported non-text stream content")
	}

	chunk := domain.ChatChunk{}
	if message.Role == schema.Assistant {
		role := domain.RoleAssistant
		chunk.Delta.Role = &role
	}
	if message.Content != "" {
		content := message.Content
		chunk.Delta.Content = &content
	}
	if message.ReasoningContent != "" {
		reasoningContent := message.ReasoningContent
		chunk.Delta.ReasoningContent = &reasoningContent
	}
	if message.ResponseMeta == nil {
		return chunk, nil
	}
	if message.ResponseMeta.FinishReason != "" {
		finishReason := message.ResponseMeta.FinishReason
		chunk.FinishReason = &finishReason
	}
	if message.ResponseMeta.Usage != nil {
		chunk.Usage = &domain.Usage{
			PromptTokens:     message.ResponseMeta.Usage.PromptTokens,
			CompletionTokens: message.ResponseMeta.Usage.CompletionTokens,
			TotalTokens:      message.ResponseMeta.Usage.TotalTokens,
		}
	}
	return chunk, nil
}
