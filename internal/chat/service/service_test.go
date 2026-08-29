package service

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

type fakeProvider struct {
	generate func(context.Context, domain.ChatRequest) (domain.ChatResponse, error)
	stream   func(context.Context, domain.ChatRequest) (domain.ChatStream, error)
}

func (f fakeProvider) Generate(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
	return f.generate(ctx, req)
}

func (f fakeProvider) Stream(ctx context.Context, req domain.ChatRequest) (domain.ChatStream, error) {
	return f.stream(ctx, req)
}

func TestChatServiceGenerate(t *testing.T) {
	chatService := newServiceForProvider(t, fakeProvider{
		generate: func(_ context.Context, request domain.ChatRequest) (domain.ChatResponse, error) {
			if request.Model != "upstream-model" {
				t.Fatalf("provider request model = %q", request.Model)
			}
			return domain.ChatResponse{Model: "upstream-model"}, nil
		},
	})

	actual, err := chatService.Generate(context.Background(), domain.ChatRequest{Model: "default-chat"})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if actual.Model != "default-chat" {
		t.Fatalf("Generate() model = %q, want logical model", actual.Model)
	}
}

func TestChatServiceGenerateAndStreamUseRegisteredRoutes(t *testing.T) {
	firstGenerateCalls, firstStreamCalls, secondStreamCalls := 0, 0, 0
	firstProvider := fakeProvider{
		generate: func(_ context.Context, request domain.ChatRequest) (domain.ChatResponse, error) {
			firstGenerateCalls++
			if request.Model != "first-upstream" {
				t.Fatalf("first Generate() model = %q", request.Model)
			}
			return domain.ChatResponse{Model: request.Model}, nil
		},
		stream: func(_ context.Context, request domain.ChatRequest) (domain.ChatStream, error) {
			firstStreamCalls++
			if request.Model != "first-upstream" {
				t.Fatalf("first Stream() model = %q", request.Model)
			}
			return &serviceFakeStream{recvError: io.EOF}, nil
		},
	}
	secondProvider := fakeProvider{
		stream: func(_ context.Context, request domain.ChatRequest) (domain.ChatStream, error) {
			secondStreamCalls++
			if request.Model != "second-upstream" {
				t.Fatalf("second Stream() model = %q", request.Model)
			}
			return &serviceFakeStream{recvError: io.EOF}, nil
		},
	}
	registry, err := NewModelRegistry([]ModelRoute{
		{ExposedModel: "first-chat", UpstreamModel: "first-upstream", ProviderName: "first", Provider: firstProvider},
		{ExposedModel: "second-chat", UpstreamModel: "second-upstream", ProviderName: "second", Provider: secondProvider},
	})
	if err != nil {
		t.Fatalf("NewModelRegistry() error = %v", err)
	}
	chatService := NewChatService(registry)

	response, err := chatService.Generate(context.Background(), domain.ChatRequest{Model: "first-chat"})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if response.Model != "first-chat" {
		t.Fatalf("Generate() response model = %q", response.Model)
	}
	firstStream, err := chatService.Stream(context.Background(), domain.ChatRequest{Model: "first-chat"})
	if err != nil {
		t.Fatalf("first Stream() error = %v", err)
	}
	if err := firstStream.Close(); err != nil {
		t.Fatalf("first stream Close() error = %v", err)
	}
	secondStream, err := chatService.Stream(context.Background(), domain.ChatRequest{Model: "second-chat"})
	if err != nil {
		t.Fatalf("second Stream() error = %v", err)
	}
	if err := secondStream.Close(); err != nil {
		t.Fatalf("second stream Close() error = %v", err)
	}
	if firstGenerateCalls != 1 || firstStreamCalls != 1 || secondStreamCalls != 1 {
		t.Fatalf(
			"provider calls = generate %d, first stream %d, second stream %d",
			firstGenerateCalls,
			firstStreamCalls,
			secondStreamCalls,
		)
	}
}

func TestChatServiceUnknownModelDoesNotUseDefault(t *testing.T) {
	providerCalled := false
	provider := fakeProvider{
		generate: func(_ context.Context, _ domain.ChatRequest) (domain.ChatResponse, error) {
			providerCalled = true
			return domain.ChatResponse{}, nil
		},
		stream: func(_ context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
			providerCalled = true
			return nil, nil
		},
	}
	chatService := newServiceForProvider(t, provider)

	_, err := chatService.Generate(context.Background(), domain.ChatRequest{Model: "unknown-model"})
	if !errors.Is(err, domain.ErrModelNotFound) {
		t.Fatalf("Generate() error = %v, want ErrModelNotFound", err)
	}
	assertServiceErrorKind(t, err, domain.ErrorKindModelNotFound)
	if providerCalled {
		t.Fatal("unknown model silently used the registered provider")
	}
	_, err = chatService.Stream(context.Background(), domain.ChatRequest{Model: "unknown-model"})
	if !errors.Is(err, domain.ErrModelNotFound) {
		t.Fatalf("Stream() error = %v, want ErrModelNotFound", err)
	}
	if providerCalled {
		t.Fatal("unknown streaming model silently used the registered provider")
	}
}

func TestChatServiceClassifiesUnexpectedProviderErrorAsInternal(t *testing.T) {
	chatService := newServiceForProvider(t, fakeProvider{
		generate: func(_ context.Context, _ domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{}, errors.New("unexpected provider detail")
		},
	})

	_, err := chatService.Generate(context.Background(), domain.ChatRequest{Model: "default-chat"})
	var domainErr *domain.Error
	if !errors.As(err, &domainErr) {
		t.Fatalf("Generate() error = %T, want *domain.Error", err)
	}
	if domainErr.Kind != domain.ErrorKindInternal {
		t.Fatalf("Generate() kind = %q, want %q", domainErr.Kind, domain.ErrorKindInternal)
	}
	if domainErr.Message == "unexpected provider detail" {
		t.Fatal("Generate() exposed provider error detail")
	}
}

type serviceFakeStream struct {
	recvError  error
	closeError error
}

func (s *serviceFakeStream) Recv() (domain.ChatChunk, error) {
	return domain.ChatChunk{}, s.recvError
}

func (s *serviceFakeStream) Close() error {
	return s.closeError
}

func TestChatServiceStreamNormalizesDeferredErrors(t *testing.T) {
	providerStream := &serviceFakeStream{
		recvError:  errors.New("raw receive detail"),
		closeError: errors.New("raw close detail"),
	}
	chatService := newServiceForProvider(t, fakeProvider{
		stream: func(_ context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
			return providerStream, nil
		},
	})

	stream, err := chatService.Stream(context.Background(), domain.ChatRequest{Model: "default-chat"})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	_, recvErr := stream.Recv()
	assertServiceErrorKind(t, recvErr, domain.ErrorKindInternal)
	assertServiceErrorKind(t, stream.Close(), domain.ErrorKindInternal)
}

func TestChatServiceStreamPreservesEOF(t *testing.T) {
	providerStream := &serviceFakeStream{recvError: io.EOF}
	chatService := newServiceForProvider(t, fakeProvider{
		stream: func(_ context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
			return providerStream, nil
		},
	})

	stream, err := chatService.Stream(context.Background(), domain.ChatRequest{Model: "default-chat"})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	_, recvErr := stream.Recv()
	if !errors.Is(recvErr, io.EOF) {
		t.Fatalf("Recv() error = %v, want io.EOF", recvErr)
	}
}

func newServiceForProvider(t *testing.T, provider ChatProvider) *ChatService {
	t.Helper()
	registry, err := NewModelRegistry([]ModelRoute{
		{
			ExposedModel:  "default-chat",
			UpstreamModel: "upstream-model",
			ProviderName:  "test",
			Provider:      provider,
		},
	})
	if err != nil {
		t.Fatalf("NewModelRegistry() error = %v", err)
	}
	return NewChatService(registry)
}

func assertServiceErrorKind(t *testing.T, err error, want domain.ErrorKind) {
	t.Helper()
	var domainErr *domain.Error
	if !errors.As(err, &domainErr) {
		t.Fatalf("error = %T, want *domain.Error", err)
	}
	if domainErr.Kind != want {
		t.Fatalf("error kind = %q, want %q", domainErr.Kind, want)
	}
}
