// Package eino 使用 Eino ChatModel 实现聊天 Provider Port。
package eino

import (
	"context"
	"fmt"

	einopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

type chatModel interface {
	Generate(ctx context.Context, messages []*schema.Message, options ...model.Option) (*schema.Message, error)
	Stream(ctx context.Context, messages []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error)
}

// ProviderConfig 定义单模型的逻辑名到上游名映射。
type ProviderConfig struct {
	PublicModel   string
	UpstreamModel string
}

// Provider 将内部聊天模型转换为 Eino 调用。
type Provider struct {
	chatModel     chatModel
	publicModel   string
	upstreamModel string
}

// NewProvider 创建复用已初始化 ChatModel 的 Eino Provider。
func NewProvider(chatModel chatModel, config ProviderConfig) *Provider {
	return &Provider{
		chatModel:     chatModel,
		publicModel:   config.PublicModel,
		upstreamModel: config.UpstreamModel,
	}
}

// Generate 执行一次 Eino 非流式生成。
func (p *Provider) Generate(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
	messages, options, err := p.prepareRequest(req)
	if err != nil {
		return domain.ChatResponse{}, err
	}

	message, err := p.chatModel.Generate(ctx, messages, options...)
	if err != nil {
		return domain.ChatResponse{}, classifyProviderError(err)
	}
	response, err := toDomainResponse(req.Model, message)
	if err != nil {
		return domain.ChatResponse{}, upstreamServiceError(fmt.Errorf("map Eino response: %w", err))
	}
	return response, nil
}

// Stream 创建一次 Eino 流式生成，并适配为内部 ChatStream。
func (p *Provider) Stream(ctx context.Context, req domain.ChatRequest) (domain.ChatStream, error) {
	messages, options, err := p.prepareRequest(req)
	if err != nil {
		return nil, err
	}
	reader, err := p.chatModel.Stream(ctx, messages, options...)
	if err != nil {
		return nil, classifyProviderError(err)
	}
	return newChatStream(reader), nil
}

func (p *Provider) prepareRequest(req domain.ChatRequest) ([]*schema.Message, []model.Option, error) {
	if req.Model != p.publicModel {
		return nil, nil, domain.NewError(
			domain.ErrorKindModelNotFound,
			fmt.Sprintf("the model %q does not exist", req.Model),
			"model",
			"model_not_found",
			nil,
		)
	}

	messages, err := toEinoMessages(req.Messages)
	if err != nil {
		return nil, nil, domain.NewInternalError(fmt.Errorf("map messages to Eino: %w", err))
	}
	options := []model.Option{model.WithModel(p.upstreamModel)}
	if req.Temperature != nil {
		options = append(options, model.WithTemperature(*req.Temperature))
	}
	if req.MaxCompletionTokens != nil {
		options = append(options, einopenai.WithMaxCompletionTokens(*req.MaxCompletionTokens))
	}
	return messages, options, nil
}
