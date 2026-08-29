package app

import (
	"context"
	"errors"
	"strings"
	"testing"

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
