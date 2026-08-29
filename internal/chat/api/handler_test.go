package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	openaiapi "github.com/zhaohaip/ai-api-gateway-go/api/openai"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/service"
)

type handlerFakeProvider struct {
	generate func(context.Context, domain.ChatRequest) (domain.ChatResponse, error)
	stream   func(context.Context, domain.ChatRequest) (domain.ChatStream, error)
}

func (f handlerFakeProvider) Generate(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
	return f.generate(ctx, req)
}

func (f handlerFakeProvider) Stream(ctx context.Context, req domain.ChatRequest) (domain.ChatStream, error) {
	return f.stream(ctx, req)
}

func TestHandlerValidRequestAndOpenAIResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	provider := handlerFakeProvider{
		generate: func(_ context.Context, request domain.ChatRequest) (domain.ChatResponse, error) {
			if request.Model != "private-upstream-model" || len(request.Messages) != 1 {
				t.Fatalf("service request = %#v", request)
			}
			if request.Temperature == nil || *request.Temperature != 0.7 {
				t.Fatalf("temperature = %v", request.Temperature)
			}
			if request.MaxCompletionTokens == nil || *request.MaxCompletionTokens != 1024 {
				t.Fatalf("max completion tokens = %v", request.MaxCompletionTokens)
			}
			finishReason := "stop"
			return domain.ChatResponse{
				Model:        "upstream-model-must-not-leak",
				Message:      domain.Message{Role: domain.RoleAssistant, Content: "你好，有什么可以帮助你？"},
				FinishReason: &finishReason,
				Usage: &domain.Usage{
					PromptTokens:     10,
					CompletionTokens: 8,
					TotalTokens:      18,
				},
			}, nil
		},
		stream: func(_ context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
			t.Fatal("stream=false request called Provider.Stream")
			return nil, nil
		},
	}
	registry, err := service.NewModelRegistry([]service.ModelRoute{
		{
			ExposedModel:  "default-chat",
			UpstreamModel: "private-upstream-model",
			ProviderName:  "test-provider",
			Provider:      provider,
		},
	})
	if err != nil {
		t.Fatalf("NewModelRegistry() error = %v", err)
	}
	handler := NewHandler(service.NewChatService(registry))
	handler.newID = func() (string, error) { return "chatcmpl-test", nil }
	handler.now = func() time.Time { return time.Unix(1787880000, 0) }

	recorder := performRequest(NewRouter(handler), context.Background(), `{
		"model":"default-chat",
		"messages":[{"role":"user","content":"你好"}],
		"temperature":0.7,
		"max_completion_tokens":1024,
		"stream":false
	}`, "application/json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response openaiapi.ChatCompletionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.ID != "chatcmpl-test" || response.Object != "chat.completion" || response.Created != 1787880000 {
		t.Fatalf("response metadata = %#v", response)
	}
	if response.Model != "default-chat" || len(response.Choices) != 1 {
		t.Fatalf("response model/choices = %#v", response)
	}
	if response.Choices[0].Message.Role != "assistant" || response.Choices[0].Message.Content == "" {
		t.Fatalf("choice = %#v", response.Choices[0])
	}
	if response.Usage == nil || response.Usage.TotalTokens != 18 {
		t.Fatalf("usage = %#v", response.Usage)
	}
}

func TestHandlerRequestValidation(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		contentType string
		param       *string
		code        string
	}{
		{name: "missing model", body: `{"messages":[{"role":"user","content":"hello"}]}`, contentType: "application/json", param: stringPointer("model"), code: "required"},
		{name: "empty messages", body: `{"model":"default-chat","messages":[]}`, contentType: "application/json", param: stringPointer("messages"), code: "invalid_value"},
		{name: "invalid role", body: `{"model":"default-chat","messages":[{"role":"tool","content":"hello"}]}`, contentType: "application/json", param: stringPointer("messages[0].role"), code: "invalid_value"},
		{name: "empty content", body: `{"model":"default-chat","messages":[{"role":"user","content":"  "}]}`, contentType: "application/json", param: stringPointer("messages[0].content"), code: "invalid_value"},
		{name: "non-text content", body: `{"model":"default-chat","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`, contentType: "application/json", param: stringPointer("messages.content"), code: "invalid_type"},
		{name: "unsupported parameter", body: `{"model":"default-chat","messages":[{"role":"user","content":"hello"}],"top_p":0.5}`, contentType: "application/json", param: stringPointer("top_p"), code: "unsupported_parameter"},
		{name: "unsupported message parameter", body: `{"model":"default-chat","messages":[{"role":"user","content":"hello","name":"caller"}]}`, contentType: "application/json", param: stringPointer("name"), code: "unsupported_parameter"},
		{name: "invalid content type", body: `{}`, contentType: "text/plain", param: nil, code: "invalid_content_type"},
		{name: "invalid JSON", body: `{`, contentType: "application/json", param: nil, code: "invalid_json"},
		{name: "multiple JSON values", body: `{"model":"default-chat"} {}`, contentType: "application/json", param: nil, code: "invalid_json"},
		{name: "temperature out of range", body: `{"model":"default-chat","messages":[{"role":"user","content":"hello"}],"temperature":2.1}`, contentType: "application/json", param: stringPointer("temperature"), code: "invalid_value"},
		{name: "zero max completion tokens", body: `{"model":"default-chat","messages":[{"role":"user","content":"hello"}],"max_completion_tokens":0}`, contentType: "application/json", param: stringPointer("max_completion_tokens"), code: "invalid_value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			handler := NewHandler(newTestChatService(t, handlerFakeProvider{generate: func(
				_ context.Context,
				_ domain.ChatRequest,
			) (domain.ChatResponse, error) {
				called = true
				return domain.ChatResponse{}, nil
			}}))

			recorder := performRequest(NewRouter(handler), context.Background(), test.body, test.contentType)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			if called {
				t.Fatal("invalid request called provider")
			}
			response := decodeErrorResponse(t, recorder)
			if response.Error.Type != "invalid_request_error" || response.Error.Code != test.code {
				t.Fatalf("error = %#v", response.Error)
			}
			if !equalStringPointers(response.Error.Param, test.param) {
				t.Fatalf("param = %v, want %v", response.Error.Param, test.param)
			}
		})
	}
}

func TestHandlerDomainErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		kind   string
		code   string
	}{
		{name: "unknown model", err: domain.NewError(domain.ErrorKindModelNotFound, "model unavailable", "model", "model_not_found", nil), status: 404, kind: "invalid_request_error", code: "model_not_found"},
		{name: "rate limit", err: domain.NewError(domain.ErrorKindRateLimited, "rate limited", "", "upstream_rate_limited", nil), status: 429, kind: "rate_limit_error", code: "upstream_rate_limited"},
		{name: "timeout", err: domain.NewError(domain.ErrorKindTimeout, "timed out", "", "upstream_timeout", nil), status: 504, kind: "upstream_timeout_error", code: "upstream_timeout"},
		{name: "ordinary upstream", err: domain.NewError(domain.ErrorKindUpstream, "upstream unavailable", "", "upstream_error", nil), status: 502, kind: "upstream_error", code: "upstream_error"},
		{name: "internal", err: domain.NewError(domain.ErrorKindInternal, "internal", "", "internal_error", nil), status: 500, kind: "server_error", code: "internal_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(newTestChatService(t, handlerFakeProvider{generate: func(
				_ context.Context,
				_ domain.ChatRequest,
			) (domain.ChatResponse, error) {
				return domain.ChatResponse{}, test.err
			}}))
			recorder := performRequest(NewRouter(handler), context.Background(), validRequestJSON(), "application/json")
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d", recorder.Code, test.status)
			}
			response := decodeErrorResponse(t, recorder)
			if response.Error.Type != test.kind || response.Error.Code != test.code {
				t.Fatalf("error = %#v", response.Error)
			}
		})
	}
}

func TestHandlerPropagatesClientCancellation(t *testing.T) {
	contextObserved := false
	handler := NewHandler(newTestChatService(t, handlerFakeProvider{generate: func(
		ctx context.Context,
		_ domain.ChatRequest,
	) (domain.ChatResponse, error) {
		select {
		case <-ctx.Done():
			contextObserved = true
			return domain.ChatResponse{}, ctx.Err()
		default:
			return domain.ChatResponse{}, errors.New("context was not canceled")
		}
	}}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	recorder := performRequest(NewRouter(handler), ctx, validRequestJSON(), "application/json")
	if !contextObserved {
		t.Fatal("provider did not observe canceled request context")
	}
	if recorder.Code != statusClientClosedRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, statusClientClosedRequest)
	}
}

func TestHandlerOmitsUnknownUsageAndReturnsNullFinishReason(t *testing.T) {
	handler := NewHandler(newTestChatService(t, handlerFakeProvider{generate: func(
		_ context.Context,
		request domain.ChatRequest,
	) (domain.ChatResponse, error) {
		return domain.ChatResponse{
			Model:   request.Model,
			Message: domain.Message{Role: domain.RoleAssistant, Content: "hello"},
		}, nil
	}}))
	handler.newID = func() (string, error) { return "chatcmpl-test", nil }
	recorder := performRequest(NewRouter(handler), context.Background(), validRequestJSON(), "application/json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), `"usage"`) {
		t.Fatalf("response invented usage: %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"finish_reason":null`) {
		t.Fatalf("response finish_reason is not null: %s", recorder.Body.String())
	}
}

func TestCompletionIDsAreUnique(t *testing.T) {
	provider := handlerFakeProvider{generate: func(_ context.Context, request domain.ChatRequest) (domain.ChatResponse, error) {
		return domain.ChatResponse{Model: request.Model, Message: domain.Message{Role: domain.RoleAssistant, Content: "hello"}}, nil
	}}
	handler := NewHandler(newTestChatService(t, provider))
	router := NewRouter(handler)
	first := performRequest(router, context.Background(), validRequestJSON(), "application/json")
	second := performRequest(router, context.Background(), validRequestJSON(), "application/json")
	var firstResponse, secondResponse openaiapi.ChatCompletionResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstResponse); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondResponse); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if firstResponse.ID == secondResponse.ID || !strings.HasPrefix(firstResponse.ID, "chatcmpl-") {
		t.Fatalf("completion IDs are not unique OpenAI-style IDs: %q, %q", firstResponse.ID, secondResponse.ID)
	}
}

func TestHandlerUnknownModelReturnsOpenAIModelNotFound(t *testing.T) {
	providerCalled := false
	handler := NewHandler(newTestChatService(t, handlerFakeProvider{generate: func(
		_ context.Context,
		_ domain.ChatRequest,
	) (domain.ChatResponse, error) {
		providerCalled = true
		return domain.ChatResponse{}, nil
	}}))

	recorder := performRequest(
		NewRouter(handler),
		context.Background(),
		`{"model":"unknown-model","messages":[{"role":"user","content":"hello"}]}`,
		"application/json",
	)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	response := decodeErrorResponse(t, recorder)
	if response.Error.Message != "The model 'unknown-model' does not exist." ||
		response.Error.Type != "invalid_request_error" || response.Error.Code != "model_not_found" ||
		response.Error.Param == nil || *response.Error.Param != "model" {
		t.Fatalf("error = %#v", response.Error)
	}
	if providerCalled {
		t.Fatal("unknown model called a provider")
	}
}

func TestHandlerListsOnlyLogicalModelsInRegistrationOrder(t *testing.T) {
	provider := handlerFakeProvider{}
	registry, err := service.NewModelRegistry([]service.ModelRoute{
		{
			ExposedModel:  "default-chat",
			UpstreamModel: "private-deepseek-model",
			ProviderName:  "secret-deepseek-provider",
			Provider:      provider,
		},
		{
			ExposedModel:  "fast-chat",
			UpstreamModel: "private-qwen-model",
			ProviderName:  "secret-qwen-provider",
			Provider:      provider,
		},
	})
	if err != nil {
		t.Fatalf("NewModelRegistry() error = %v", err)
	}
	handler := NewHandler(service.NewChatService(registry))
	handler.now = func() time.Time { return time.Unix(1787912515, 0) }
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder := httptest.NewRecorder()

	NewRouter(handler).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response openaiapi.ModelListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode model list: %v", err)
	}
	if response.Object != "list" || len(response.Data) != 2 {
		t.Fatalf("response = %#v", response)
	}
	wantModels := []string{"default-chat", "fast-chat"}
	for index, want := range wantModels {
		model := response.Data[index]
		if model.ID != want || model.Object != "model" || model.Created != 1787912515 || model.OwnedBy != "gateway" {
			t.Fatalf("model %d = %#v", index, model)
		}
	}
	if strings.Contains(recorder.Body.String(), "private-") || strings.Contains(recorder.Body.String(), "secret-") {
		t.Fatalf("model list leaked routing details: %s", recorder.Body.String())
	}
}

func newTestChatService(t testing.TB, provider service.ChatProvider) *service.ChatService {
	t.Helper()
	registry, err := service.NewModelRegistry([]service.ModelRoute{
		{
			ExposedModel:  "default-chat",
			UpstreamModel: "default-chat",
			ProviderName:  "test",
			Provider:      provider,
		},
	})
	if err != nil {
		t.Fatalf("NewModelRegistry() error = %v", err)
	}
	return service.NewChatService(registry)
}

func performRequest(handler http.Handler, ctx context.Context, body, contentType string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body)).WithContext(ctx)
	request.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeErrorResponse(t *testing.T, recorder *httptest.ResponseRecorder) openaiapi.ErrorResponse {
	t.Helper()
	var response openaiapi.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error response: %v; body = %s", err, recorder.Body.String())
	}
	return response
}

func validRequestJSON() string {
	return `{"model":"default-chat","messages":[{"role":"user","content":"hello"}]}`
}

func stringPointer(value string) *string {
	return &value
}

func equalStringPointers(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
