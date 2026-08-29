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
	provider ChatProvider
}

// NewChatService 创建聊天服务。
func NewChatService(provider ChatProvider) *ChatService {
	return &ChatService{provider: provider}
}

// Generate 执行一次非流式生成。
func (s *ChatService) Generate(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
	response, err := s.provider.Generate(ctx, req)
	if err == nil {
		return response, nil
	}
	return domain.ChatResponse{}, normalizeProviderError(err, "generate chat completion")
}

// Stream 创建一次流式聊天调用。
func (s *ChatService) Stream(ctx context.Context, req domain.ChatRequest) (domain.ChatStream, error) {
	stream, err := s.provider.Stream(ctx, req)
	if err != nil {
		return nil, normalizeProviderError(err, "create chat completion stream")
	}
	return &chatStream{stream: stream}, nil
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
