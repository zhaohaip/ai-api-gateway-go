package api

import (
	"fmt"
	"strings"

	openaiapi "github.com/zhaohaip/ai-api-gateway-go/api/openai"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

func validateRequest(request openaiapi.ChatCompletionRequest) error {
	if strings.TrimSpace(request.Model) == "" {
		return domain.NewInvalidRequestError("model is required", "model", "required")
	}
	if len(request.Messages) == 0 {
		return domain.NewInvalidRequestError("messages must not be empty", "messages", "invalid_value")
	}
	for index, message := range request.Messages {
		paramPrefix := fmt.Sprintf("messages[%d]", index)
		switch message.Role {
		case string(domain.RoleSystem), string(domain.RoleUser), string(domain.RoleAssistant):
		default:
			return domain.NewInvalidRequestError(
				fmt.Sprintf("%s.role must be one of system, user, assistant", paramPrefix),
				paramPrefix+".role",
				"invalid_value",
			)
		}
		if strings.TrimSpace(message.Content) == "" {
			return domain.NewInvalidRequestError(
				paramPrefix+".content must not be empty",
				paramPrefix+".content",
				"invalid_value",
			)
		}
	}
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 2) {
		return domain.NewInvalidRequestError(
			"temperature must be between 0 and 2",
			"temperature",
			"invalid_value",
		)
	}
	if request.MaxCompletionTokens != nil && *request.MaxCompletionTokens <= 0 {
		return domain.NewInvalidRequestError(
			"max_completion_tokens must be greater than 0",
			"max_completion_tokens",
			"invalid_value",
		)
	}
	return nil
}
