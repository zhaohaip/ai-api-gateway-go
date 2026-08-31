package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	openaiapi "github.com/zhaohaip/ai-api-gateway-go/api/openai"
	gatewayauth "github.com/zhaohaip/ai-api-gateway-go/internal/auth"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/service"
	"github.com/zhaohaip/ai-api-gateway-go/internal/config"
)

type appFakeProvider struct {
	name string
}

func (*appFakeProvider) Generate(context.Context, domain.ChatRequest) (domain.ChatResponse, error) {
	return domain.ChatResponse{}, nil
}

func (*appFakeProvider) Stream(context.Context, domain.ChatRequest) (domain.ChatStream, error) {
	return nil, nil
}

func TestBuildModelRegistryInitializesAndReusesProviderOnce(t *testing.T) {
	appConfig := config.Config{
		Providers: []config.ProviderConfig{
			{Name: "deepseek", Type: config.ProviderTypeOpenAICompatible},
			{Name: "qwen", Type: config.ProviderTypeOpenAICompatible},
		},
		Models: []config.ModelConfig{
			{Name: "default-chat", Provider: "deepseek", UpstreamModel: "deepseek-chat"},
			{Name: "reasoning-chat", Provider: "deepseek", UpstreamModel: "deepseek-reasoner"},
			{Name: "fast-chat", Provider: "qwen", UpstreamModel: "qwen-plus"},
		},
	}
	initializations := make(map[string]int)
	defaultModels := make(map[string]string)
	registry, err := buildModelRegistry(context.Background(), appConfig, func(
		_ context.Context,
		providerConfig config.ProviderConfig,
		defaultModel string,
	) (service.ChatProvider, error) {
		initializations[providerConfig.Name]++
		defaultModels[providerConfig.Name] = defaultModel
		return &appFakeProvider{name: providerConfig.Name}, nil
	})
	if err != nil {
		t.Fatalf("buildModelRegistry() error = %v", err)
	}
	if initializations["deepseek"] != 1 || initializations["qwen"] != 1 {
		t.Fatalf("provider initializations = %#v", initializations)
	}
	if defaultModels["deepseek"] != "deepseek-chat" || defaultModels["qwen"] != "qwen-plus" {
		t.Fatalf("provider default models = %#v", defaultModels)
	}

	defaultRoute, err := registry.Resolve("default-chat")
	if err != nil {
		t.Fatalf("resolve default-chat: %v", err)
	}
	reasoningRoute, err := registry.Resolve("reasoning-chat")
	if err != nil {
		t.Fatalf("resolve reasoning-chat: %v", err)
	}
	fastRoute, err := registry.Resolve("fast-chat")
	if err != nil {
		t.Fatalf("resolve fast-chat: %v", err)
	}
	if defaultRoute.Provider != reasoningRoute.Provider {
		t.Fatal("models on the same provider did not reuse the provider instance")
	}
	if defaultRoute.Provider == fastRoute.Provider {
		t.Fatal("different provider configurations shared a provider instance")
	}
	chatService := service.NewChatService(registry)
	if _, err := chatService.Generate(context.Background(), domain.ChatRequest{Model: "default-chat"}); err != nil {
		t.Fatalf("Generate(default-chat) error = %v", err)
	}
	if _, err := chatService.Generate(context.Background(), domain.ChatRequest{Model: "reasoning-chat"}); err != nil {
		t.Fatalf("Generate(reasoning-chat) error = %v", err)
	}
	if initializations["deepseek"] != 1 {
		t.Fatalf("provider was initialized during requests: %#v", initializations)
	}
}

func TestBuildModelRegistryFailsWhenProviderInitializationFails(t *testing.T) {
	appConfig := config.Config{
		Providers: []config.ProviderConfig{{Name: "provider", Type: config.ProviderTypeOpenAICompatible}},
		Models:    []config.ModelConfig{{Name: "chat", Provider: "provider", UpstreamModel: "upstream"}},
	}
	_, err := buildModelRegistry(context.Background(), appConfig, func(
		context.Context,
		config.ProviderConfig,
		string,
	) (service.ChatProvider, error) {
		return nil, errors.New("initialization failed")
	})
	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("buildModelRegistry() error = %v", err)
	}
}

func TestAppUsesProviderAPIKeyInsteadOfGatewayAPIKeyUpstream(t *testing.T) {
	const (
		gatewayAPIKey  = "sk-gw-client-only"
		providerAPIKey = "provider-upstream-only"
	)
	type upstreamRequest struct {
		Authorization string
		Model         string
	}
	upstreamRequests := make(chan upstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		upstreamRequests <- upstreamRequest{
			Authorization: request.Header.Get("Authorization"),
			Model:         body.Model,
		}
		writer.Header().Set("Content-Type", "application/json")
		if _, err := writer.Write([]byte(`{
            "id":"upstream-id",
            "object":"chat.completion",
            "created":1,
            "model":"private-upstream",
            "choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
        }`)); err != nil {
			t.Errorf("write upstream response: %v", err)
		}
	}))
	defer upstream.Close()

	application, err := New(context.Background(), config.Config{
		Address: ":0",
		Auth: config.AuthConfig{APIKeys: []gatewayauth.APIKey{
			{
				ID:            "client",
				KeyHash:       sha256.Sum256([]byte(gatewayAPIKey)),
				Enabled:       true,
				AllowedModels: []string{"default-chat"},
			},
		}},
		Providers: []config.ProviderConfig{
			{
				Name:    "upstream",
				Type:    config.ProviderTypeOpenAICompatible,
				BaseURL: upstream.URL + "/v1",
				APIKey:  providerAPIKey,
				Timeout: time.Second,
			},
		},
		Models: []config.ModelConfig{
			{Name: "default-chat", Provider: "upstream", UpstreamModel: "private-upstream"},
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"default-chat","messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+gatewayAPIKey)
	recorder := httptest.NewRecorder()

	application.server.Handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	receivedUpstreamRequest := <-upstreamRequests
	if receivedUpstreamRequest.Authorization != "Bearer "+providerAPIKey ||
		receivedUpstreamRequest.Model != "private-upstream" {
		t.Fatalf("upstream request = %#v", receivedUpstreamRequest)
	}
	if strings.Contains(receivedUpstreamRequest.Authorization, gatewayAPIKey) {
		t.Fatal("gateway API key was forwarded upstream")
	}
	var response openaiapi.ChatCompletionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode gateway response: %v", err)
	}
	if response.Model != "default-chat" {
		t.Fatalf("response model = %q", response.Model)
	}
}
