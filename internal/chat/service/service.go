// Package service 组织聊天用例，不依赖 HTTP 或模型 SDK。
package service

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

// ChatProvider 定义可替换的聊天模型能力。
type ChatProvider interface {
	Generate(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error)
	Stream(ctx context.Context, req domain.ChatRequest) (domain.ChatStream, error)
}

// ChatService 组织聊天模型调用。
type ChatService struct {
	registry ModelRegistry
}

// NewChatService 创建聊天服务。
func NewChatService(registry ModelRegistry) *ChatService {
	return &ChatService{registry: registry}
}

// Generate 执行一次非流式生成。
func (s *ChatService) Generate(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
	route, providerRequest, err := s.resolveRequest(req)
	if err != nil {
		return domain.ChatResponse{}, err
	}
	response, err := route.Provider.Generate(ctx, providerRequest)
	if err != nil {
		return domain.ChatResponse{}, normalizeProviderError(err, "generate chat completion")
	}
	response.Model = route.ExposedModel
	return response, nil
}

// Stream 创建一次流式聊天调用。
func (s *ChatService) Stream(ctx context.Context, req domain.ChatRequest) (domain.ChatStream, error) {
	route, providerRequest, err := s.resolveRequest(req)
	if err != nil {
		return nil, err
	}
	stream, err := route.Provider.Stream(ctx, providerRequest)
	if err != nil {
		if stream != nil {
			_ = stream.Close()
		}
		return nil, normalizeProviderError(err, "create chat completion stream")
	}
	return &chatStream{stream: stream}, nil
}

// ListModels 按稳定顺序返回已注册的逻辑模型。
func (s *ChatService) ListModels() []ModelInfo {
	return s.registry.List()
}

// ProviderName 返回逻辑模型对应的非敏感 Provider 名称，仅用于运行日志。
func (s *ChatService) ProviderName(model string) string {
	route, err := s.registry.Resolve(model)
	if err != nil {
		return ""
	}
	return route.ProviderName
}

func (s *ChatService) resolveRequest(req domain.ChatRequest) (ModelRoute, domain.ChatRequest, error) {
	route, err := s.registry.Resolve(req.Model)
	if err != nil {
		return ModelRoute{}, domain.ChatRequest{}, normalizeProviderError(err, "resolve chat model")
	}
	providerRequest := req
	providerRequest.Model = route.UpstreamModel
	return route, providerRequest, nil
}

func normalizeProviderError(err error, operation string) error {
	var domainErr *domain.Error
	if errors.As(err, &domainErr) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return domain.NewError(
			domain.ErrorKindCanceled,
			"the request was canceled by the client",
			"",
			"request_canceled",
			err,
		)
	}

	return domain.NewInternalError(fmt.Errorf("%s: %w", operation, err))
}

type chatStream struct {
	stream domain.ChatStream
}

func (s *chatStream) Recv() (domain.ChatChunk, error) {
	chunk, err := s.stream.Recv()
	if err == nil || errors.Is(err, io.EOF) {
		return chunk, err
	}
	return domain.ChatChunk{}, normalizeProviderError(err, "receive chat completion stream")
}

func (s *chatStream) Close() error {
	if err := s.stream.Close(); err != nil {
		return normalizeProviderError(err, "close chat completion stream")
	}
	return nil
}
