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
	expected := domain.ChatResponse{Model: "default-chat"}
	service := NewChatService(fakeProvider{
		generate: func(_ context.Context, _ domain.ChatRequest) (domain.ChatResponse, error) {
			return expected, nil
		},
	})

	actual, err := service.Generate(context.Background(), domain.ChatRequest{})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if actual.Model != expected.Model {
		t.Fatalf("Generate() model = %q, want %q", actual.Model, expected.Model)
	}
}

func TestChatServiceClassifiesUnexpectedProviderErrorAsInternal(t *testing.T) {
	service := NewChatService(fakeProvider{
		generate: func(_ context.Context, _ domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{}, errors.New("unexpected provider detail")
		},
	})

	_, err := service.Generate(context.Background(), domain.ChatRequest{})
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
	chatService := NewChatService(fakeProvider{
		stream: func(_ context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
			return providerStream, nil
		},
	})

	stream, err := chatService.Stream(context.Background(), domain.ChatRequest{})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	_, recvErr := stream.Recv()
	assertServiceErrorKind(t, recvErr, domain.ErrorKindInternal)
	assertServiceErrorKind(t, stream.Close(), domain.ErrorKindInternal)
}

func TestChatServiceStreamPreservesEOF(t *testing.T) {
	providerStream := &serviceFakeStream{recvError: io.EOF}
	chatService := NewChatService(fakeProvider{
		stream: func(_ context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
			return providerStream, nil
		},
	})

	stream, err := chatService.Stream(context.Background(), domain.ChatRequest{})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	_, recvErr := stream.Recv()
	if !errors.Is(recvErr, io.EOF) {
		t.Fatalf("Recv() error = %v, want io.EOF", recvErr)
	}
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
