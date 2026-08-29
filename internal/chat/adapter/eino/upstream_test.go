package eino

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	einopenai "github.com/cloudwego/eino-ext/components/model/openai"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

func TestProviderWithLocalOpenAICompatibleUpstream(t *testing.T) {
	type upstreamMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type upstreamRequest struct {
		Model               string            `json:"model"`
		Messages            []upstreamMessage `json:"messages"`
		Temperature         *float32          `json:"temperature"`
		MaxCompletionTokens *int              `json:"max_completion_tokens"`
	}
	requestReceived := make(chan upstreamRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer local-test-key" {
			t.Errorf("Authorization header was not set")
		}
		var body upstreamRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		requestReceived <- body
		writer.Header().Set("Content-Type", "application/json")
		if _, err := writer.Write([]byte(`{
			"id":"chatcmpl-upstream-secret",
			"object":"chat.completion",
			"created":1787880000,
			"model":"upstream-secret-name",
			"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18}
		}`)); err != nil {
			t.Errorf("write upstream response: %v", err)
		}
	}))
	defer server.Close()

	chatModel, err := einopenai.NewChatModel(context.Background(), &einopenai.ChatModelConfig{
		BaseURL: server.URL + "/v1",
		APIKey:  "local-test-key",
		Model:   "upstream-secret-name",
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewChatModel() error = %v", err)
	}
	provider := NewProvider(chatModel, ProviderConfig{
		PublicModel:   "default-chat",
		UpstreamModel: "upstream-secret-name",
	})
	temperature := float32(0.7)
	maxCompletionTokens := 1024
	response, err := provider.Generate(context.Background(), domain.ChatRequest{
		Model:               "default-chat",
		Messages:            []domain.Message{{Role: domain.RoleUser, Content: "ping"}},
		Temperature:         &temperature,
		MaxCompletionTokens: &maxCompletionTokens,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	upstream := <-requestReceived
	if upstream.Model != "upstream-secret-name" {
		t.Errorf("upstream model = %q", upstream.Model)
	}
	if upstream.Temperature == nil || *upstream.Temperature != 0.7 {
		t.Errorf("upstream temperature = %v", upstream.Temperature)
	}
	if upstream.MaxCompletionTokens == nil || *upstream.MaxCompletionTokens != 1024 {
		t.Errorf("upstream max_completion_tokens = %v", upstream.MaxCompletionTokens)
	}
	if len(upstream.Messages) != 1 || upstream.Messages[0].Content != "ping" {
		t.Errorf("upstream messages = %#v", upstream.Messages)
	}
	if response.Model != "default-chat" || response.Message.Content != "pong" {
		t.Fatalf("response = %#v", response)
	}
	if response.Usage == nil || response.Usage.TotalTokens != 18 {
		t.Fatalf("response usage = %#v", response.Usage)
	}
}

func TestProviderStreamWithLocalOpenAICompatibleUpstream(t *testing.T) {
	type upstreamRequest struct {
		Model               string   `json:"model"`
		Stream              bool     `json:"stream"`
		Temperature         *float32 `json:"temperature"`
		MaxCompletionTokens *int     `json:"max_completion_tokens"`
	}
	requestReceived := make(chan upstreamRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body upstreamRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode streaming upstream request: %v", err)
		}
		requestReceived <- body
		writer.Header().Set("Content-Type", "text/event-stream")
		payload := "" +
			"data: {\"id\":\"upstream-id\",\"object\":\"chat.completion.chunk\",\"model\":\"private-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"AI\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"upstream-id\",\"object\":\"chat.completion.chunk\",\"model\":\"private-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" gateway\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"upstream-id\",\"object\":\"chat.completion.chunk\",\"model\":\"private-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"id\":\"upstream-id\",\"object\":\"chat.completion.chunk\",\"model\":\"private-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n" +
			"data: [DONE]\n\n"
		if _, err := io.WriteString(writer, payload); err != nil {
			t.Errorf("write streaming upstream response: %v", err)
		}
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer server.Close()

	chatModel, err := einopenai.NewChatModel(context.Background(), &einopenai.ChatModelConfig{
		BaseURL: server.URL + "/v1",
		APIKey:  "local-test-key",
		Model:   "private-model",
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewChatModel() error = %v", err)
	}
	provider := NewProvider(chatModel, ProviderConfig{
		PublicModel:   "default-chat",
		UpstreamModel: "private-model",
	})
	temperature := float32(0.2)
	maxCompletionTokens := 64
	stream, err := provider.Stream(context.Background(), domain.ChatRequest{
		Model:               "default-chat",
		Messages:            []domain.Message{{Role: domain.RoleUser, Content: "hello"}},
		Temperature:         &temperature,
		MaxCompletionTokens: &maxCompletionTokens,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()

	var chunks []domain.ChatChunk
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv() error = %v", recvErr)
		}
		chunks = append(chunks, chunk)
	}
	upstream := <-requestReceived
	if !upstream.Stream || upstream.Model != "private-model" {
		t.Fatalf("streaming upstream request = %#v", upstream)
	}
	if upstream.Temperature == nil || *upstream.Temperature != 0.2 {
		t.Fatalf("streaming upstream temperature = %v", upstream.Temperature)
	}
	if upstream.MaxCompletionTokens == nil || *upstream.MaxCompletionTokens != 64 {
		t.Fatalf("streaming upstream max completion tokens = %v", upstream.MaxCompletionTokens)
	}
	if len(chunks) != 3 {
		t.Fatalf("stream chunk count = %d, chunks = %#v", len(chunks), chunks)
	}
	if chunks[0].Delta.Content == nil || *chunks[0].Delta.Content != "AI" {
		t.Fatalf("first chunk = %#v", chunks[0])
	}
	final := chunks[len(chunks)-1]
	if final.FinishReason == nil || *final.FinishReason != "stop" || final.Usage == nil || final.Usage.TotalTokens != 5 {
		t.Fatalf("final chunk = %#v", final)
	}
}
