package eino

import (
	"context"
	"errors"
	"io"
	"testing"

	einopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

func TestProviderStreamMapsRequestAndChunks(t *testing.T) {
	temperature := float32(0)
	provider := NewProvider(fakeChatModel{stream: func(
		ctx context.Context,
		messages []*schema.Message,
		options ...model.Option,
	) (*schema.StreamReader[*schema.Message], error) {
		if ctx.Value(streamContextKey{}) != "request-value" {
			t.Fatal("Stream() did not propagate request context")
		}
		if len(messages) != 1 || messages[0].Role != schema.User || messages[0].Content != "hello" {
			t.Fatalf("Stream() messages = %#v", messages)
		}
		commonOptions := model.GetCommonOptions(nil, options...)
		if commonOptions.Model == nil || *commonOptions.Model != "upstream-secret-name" {
			t.Fatalf("Stream() model option = %v", commonOptions.Model)
		}
		if commonOptions.Temperature == nil || *commonOptions.Temperature != 0 {
			t.Fatalf("Stream() temperature option = %v", commonOptions.Temperature)
		}
		return schema.StreamReaderFromArray([]*schema.Message{
			{Role: schema.Assistant, Content: "AI"},
			{Content: " gateway"},
			{
				ResponseMeta: &schema.ResponseMeta{
					FinishReason: "stop",
					Usage: &schema.TokenUsage{
						PromptTokens:     2,
						CompletionTokens: 2,
						TotalTokens:      4,
					},
				},
			},
		}), nil
	}})
	ctx := context.WithValue(context.Background(), streamContextKey{}, "request-value")

	stream, err := provider.Stream(ctx, domain.ChatRequest{
		Model:       "upstream-secret-name",
		Messages:    []domain.Message{{Role: domain.RoleUser, Content: "hello"}},
		Temperature: &temperature,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()

	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv() error = %v", err)
	}
	if first.Delta.Role == nil || *first.Delta.Role != domain.RoleAssistant || first.Delta.Content == nil || *first.Delta.Content != "AI" {
		t.Fatalf("first chunk = %#v", first)
	}
	second, err := stream.Recv()
	if err != nil || second.Delta.Content == nil || *second.Delta.Content != " gateway" {
		t.Fatalf("second chunk = %#v, error = %v", second, err)
	}
	final, err := stream.Recv()
	if err != nil {
		t.Fatalf("final Recv() error = %v", err)
	}
	if final.FinishReason == nil || *final.FinishReason != "stop" || final.Usage == nil || final.Usage.TotalTokens != 4 {
		t.Fatalf("final chunk = %#v", final)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("last Recv() error = %v, want io.EOF", err)
	}
}

type streamContextKey struct{}

type fakeEinoStreamReader struct {
	recvError  error
	closeCount int
}

func (r *fakeEinoStreamReader) Recv() (*schema.Message, error) {
	return nil, r.recvError
}

func (r *fakeEinoStreamReader) Close() {
	r.closeCount++
}

func TestChatStreamClassifiesRecvErrorAndClosesOnce(t *testing.T) {
	reader := &fakeEinoStreamReader{recvError: context.Canceled}
	stream := newChatStream(reader)

	_, err := stream.Recv()
	assertDomainErrorKind(t, err, domain.ErrorKindCanceled)
	if err := stream.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if reader.closeCount != 1 {
		t.Fatalf("underlying Close() count = %d, want 1", reader.closeCount)
	}
}

func TestChatStreamClassifiesDeferredErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind domain.ErrorKind
	}{
		{name: "canceled", err: context.Canceled, kind: domain.ErrorKindCanceled},
		{name: "timeout", err: context.DeadlineExceeded, kind: domain.ErrorKindTimeout},
		{name: "rate limited", err: &einopenai.APIError{HTTPStatusCode: 429}, kind: domain.ErrorKindRateLimited},
		{name: "ordinary upstream", err: errors.New("raw upstream detail"), kind: domain.ErrorKindUpstream},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream := newChatStream(&fakeEinoStreamReader{recvError: test.err})
			_, err := stream.Recv()
			assertDomainErrorKind(t, err, test.kind)
		})
	}
}

func TestChatStreamPreservesEOF(t *testing.T) {
	stream := newChatStream(&fakeEinoStreamReader{recvError: io.EOF})
	_, err := stream.Recv()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Recv() error = %v, want io.EOF", err)
	}
}

func TestToDomainChunkMapsReasoningAndPreservesSpecialChunks(t *testing.T) {
	reasoningMessages := []*schema.Message{
		{Role: schema.Assistant, ReasoningContent: "需要先分析"},
		{Role: schema.Assistant, ReasoningContent: "再组织答案"},
	}
	for index, message := range reasoningMessages {
		chunk, err := toDomainChunk(message)
		if err != nil {
			t.Fatalf("reasoning chunk %d error = %v", index, err)
		}
		if chunk.Delta.ReasoningContent == nil || *chunk.Delta.ReasoningContent != message.ReasoningContent {
			t.Fatalf("reasoning chunk %d = %#v", index, chunk)
		}
		if chunk.Delta.Content != nil {
			t.Fatalf("reasoning chunk %d was mapped as normal content: %#v", index, chunk)
		}
		if chunk.Empty() {
			t.Fatalf("reasoning chunk %d was considered empty", index)
		}
	}

	finishReason := "stop"
	tests := []struct {
		name    string
		message *schema.Message
		empty   bool
	}{
		{name: "meaningless empty", message: &schema.Message{}, empty: true},
		{name: "assistant role", message: &schema.Message{Role: schema.Assistant}},
		{name: "finish reason", message: &schema.Message{ResponseMeta: &schema.ResponseMeta{FinishReason: finishReason}}},
		{name: "usage only", message: &schema.Message{ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{TotalTokens: 3}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chunk, err := toDomainChunk(test.message)
			if err != nil {
				t.Fatalf("toDomainChunk() error = %v", err)
			}
			if chunk.Empty() != test.empty {
				t.Fatalf("chunk.Empty() = %v, want %v; chunk = %#v", chunk.Empty(), test.empty, chunk)
			}
		})
	}
}

func TestToDomainChunkRejectsToolCalls(t *testing.T) {
	_, err := toDomainChunk(&schema.Message{
		Role:      schema.Assistant,
		ToolCalls: []schema.ToolCall{{}},
	})
	if err == nil {
		t.Fatal("toDomainChunk() accepted tool call delta")
	}
}
