package eino

import (
	"context"
	"errors"
	"fmt"
	"testing"

	einopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

type fakeChatModel struct {
	generate func(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error)
	stream   func(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error)
}

func (f fakeChatModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	return f.stream(ctx, messages, options...)
}

func (f fakeChatModel) Generate(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.Message, error) {
	return f.generate(ctx, messages, options...)
}

func TestToEinoMessages(t *testing.T) {
	messages, err := toEinoMessages([]domain.Message{
		{Role: domain.RoleSystem, Content: "rules"},
		{Role: domain.RoleUser, Content: "question"},
		{Role: domain.RoleAssistant, Content: "answer"},
	})
	if err != nil {
		t.Fatalf("toEinoMessages() error = %v", err)
	}
	wantRoles := []schema.RoleType{schema.System, schema.User, schema.Assistant}
	for index, wantRole := range wantRoles {
		if messages[index].Role != wantRole {
			t.Errorf("message %d role = %q, want %q", index, messages[index].Role, wantRole)
		}
	}
	if messages[1].Content != "question" {
		t.Fatalf("user content = %q, want question", messages[1].Content)
	}
}

func TestProviderGenerateMapsResponseMetadata(t *testing.T) {
	provider := NewProvider(fakeChatModel{generate: func(
		_ context.Context,
		messages []*schema.Message,
		options ...model.Option,
	) (*schema.Message, error) {
		if len(messages) != 1 || messages[0].Role != schema.User {
			t.Fatalf("Generate() messages = %#v", messages)
		}
		commonOptions := model.GetCommonOptions(nil, options...)
		if commonOptions.Model == nil || *commonOptions.Model != "upstream-secret-name" {
			t.Fatalf("Generate() model option = %v", commonOptions.Model)
		}
		if commonOptions.Temperature == nil || *commonOptions.Temperature != 0 {
			t.Fatalf("Generate() temperature option = %v, want pointer to zero", commonOptions.Temperature)
		}
		return &schema.Message{
			Role:    schema.Assistant,
			Content: "hello",
			ResponseMeta: &schema.ResponseMeta{
				FinishReason: "stop",
				Usage: &schema.TokenUsage{
					PromptTokens:     3,
					CompletionTokens: 2,
					TotalTokens:      5,
				},
			},
		}, nil
	}}, ProviderConfig{PublicModel: "default-chat", UpstreamModel: "upstream-secret-name"})
	temperature := float32(0)

	response, err := provider.Generate(context.Background(), domain.ChatRequest{
		Model:       "default-chat",
		Messages:    []domain.Message{{Role: domain.RoleUser, Content: "hi"}},
		Temperature: &temperature,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if response.Model != "default-chat" {
		t.Fatalf("response model = %q, want logical model", response.Model)
	}
	if response.FinishReason == nil || *response.FinishReason != "stop" {
		t.Fatalf("finish reason = %v", response.FinishReason)
	}
	if response.Usage == nil || response.Usage.TotalTokens != 5 {
		t.Fatalf("usage = %#v", response.Usage)
	}
}

func TestProviderDoesNotInventMissingMetadata(t *testing.T) {
	provider := NewProvider(fakeChatModel{generate: func(
		_ context.Context,
		_ []*schema.Message,
		_ ...model.Option,
	) (*schema.Message, error) {
		return &schema.Message{Role: schema.Assistant, Content: "hello"}, nil
	}}, ProviderConfig{PublicModel: "default-chat", UpstreamModel: "upstream"})

	response, err := provider.Generate(context.Background(), validDomainRequest())
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if response.Usage != nil || response.FinishReason != nil {
		t.Fatalf("Generate() metadata = usage %#v, finish reason %#v; want nil", response.Usage, response.FinishReason)
	}
}

func TestProviderUnknownModel(t *testing.T) {
	called := false
	provider := NewProvider(fakeChatModel{generate: func(
		_ context.Context,
		_ []*schema.Message,
		_ ...model.Option,
	) (*schema.Message, error) {
		called = true
		return nil, nil
	}}, ProviderConfig{PublicModel: "default-chat", UpstreamModel: "upstream"})

	_, err := provider.Generate(context.Background(), domain.ChatRequest{Model: "unknown"})
	assertDomainErrorKind(t, err, domain.ErrorKindModelNotFound)
	if called {
		t.Fatal("unknown model called Eino ChatModel")
	}
}

func TestProviderErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind domain.ErrorKind
	}{
		{name: "rate limited", err: &einopenai.APIError{HTTPStatusCode: 429, Message: "secret raw error"}, kind: domain.ErrorKindRateLimited},
		{name: "gateway timeout", err: &einopenai.APIError{HTTPStatusCode: 504, Message: "secret raw error"}, kind: domain.ErrorKindTimeout},
		{name: "context deadline", err: context.DeadlineExceeded, kind: domain.ErrorKindTimeout},
		{name: "ordinary upstream", err: errors.New("secret raw error"), kind: domain.ErrorKindUpstream},
		{name: "upstream unauthorized", err: &einopenai.APIError{HTTPStatusCode: 401, Message: "bad key"}, kind: domain.ErrorKindUpstream},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := NewProvider(fakeChatModel{generate: func(
				_ context.Context,
				_ []*schema.Message,
				_ ...model.Option,
			) (*schema.Message, error) {
				return nil, test.err
			}}, ProviderConfig{PublicModel: "default-chat", UpstreamModel: "upstream"})

			_, err := provider.Generate(context.Background(), validDomainRequest())
			assertDomainErrorKind(t, err, test.kind)
			if err.Error() == test.err.Error() {
				t.Fatal("provider exposed raw upstream error")
			}
		})
	}
}

func TestProviderPropagatesCancellation(t *testing.T) {
	contextKey := struct{}{}
	provider := NewProvider(fakeChatModel{generate: func(
		ctx context.Context,
		_ []*schema.Message,
		_ ...model.Option,
	) (*schema.Message, error) {
		if ctx.Value(contextKey) != "request-value" {
			return nil, fmt.Errorf("request context value was not propagated")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}, ProviderConfig{PublicModel: "default-chat", UpstreamModel: "upstream"})
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey, "request-value"))
	cancel()

	_, err := provider.Generate(ctx, validDomainRequest())
	assertDomainErrorKind(t, err, domain.ErrorKindCanceled)
}

func validDomainRequest() domain.ChatRequest {
	return domain.ChatRequest{
		Model:    "default-chat",
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hello"}},
	}
}

func assertDomainErrorKind(t *testing.T, err error, want domain.ErrorKind) {
	t.Helper()
	var domainErr *domain.Error
	if !errors.As(err, &domainErr) {
		t.Fatalf("error = %T, want *domain.Error", err)
	}
	if domainErr.Kind != want {
		t.Fatalf("error kind = %q, want %q", domainErr.Kind, want)
	}
}
