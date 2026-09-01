// Package app 负责应用依赖装配和 HTTP Server 生命周期。
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	einopenai "github.com/cloudwego/eino-ext/components/model/openai"

	"github.com/zhaohaip/ai-api-gateway-go/internal/auth"
	chateino "github.com/zhaohaip/ai-api-gateway-go/internal/chat/adapter/eino"
	chatapi "github.com/zhaohaip/ai-api-gateway-go/internal/chat/api"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/service"
	"github.com/zhaohaip/ai-api-gateway-go/internal/concurrencylimit"
	"github.com/zhaohaip/ai-api-gateway-go/internal/config"
	"github.com/zhaohaip/ai-api-gateway-go/internal/ratelimit"
)

const shutdownTimeout = 10 * time.Second

// App 表示已完成依赖装配的网关应用。
type App struct {
	server *http.Server
}

type providerFactory func(context.Context, config.ProviderConfig, string) (service.ChatProvider, error)

// New 在启动阶段初始化并复用各 Provider 的 Eino ChatModel。
func New(ctx context.Context, appConfig config.Config) (*App, error) {
	return newApp(ctx, appConfig, newEinoProvider)
}

func newApp(ctx context.Context, appConfig config.Config, factory providerFactory) (*App, error) {
	authenticator, err := auth.NewMemoryAuthenticator(appConfig.Auth.APIKeys)
	if err != nil {
		return nil, fmt.Errorf("initialize API key authenticator: %w", err)
	}
	limiter, err := ratelimit.NewMemoryLimiter(appConfig.Limits.Global.Rate, appConfig.Limits.DefaultAPIKey.Rate)
	if err != nil {
		return nil, fmt.Errorf("initialize request limiter: %w", err)
	}
	concurrencyController, err := concurrencylimit.NewMemoryController(
		appConfig.Limits.Global.MaxConcurrency,
		appConfig.Limits.DefaultAPIKey.MaxConcurrency,
	)
	if err != nil {
		return nil, fmt.Errorf("initialize concurrency controller: %w", err)
	}
	registry, err := buildModelRegistry(ctx, appConfig, factory)
	if err != nil {
		return nil, err
	}
	chatService := service.NewChatService(registry)
	handler := chatapi.NewHandlerWithRequestControls(chatService, limiter, concurrencyController)

	return &App{
		server: &http.Server{
			Addr:              appConfig.Address,
			Handler:           chatapi.NewRouter(handler, authenticator),
			ReadHeaderTimeout: 5 * time.Second,
		},
	}, nil
}

func newEinoProvider(
	ctx context.Context,
	providerConfig config.ProviderConfig,
	defaultModel string,
) (service.ChatProvider, error) {
	if providerConfig.Type != config.ProviderTypeOpenAICompatible {
		return nil, fmt.Errorf("provider %q has unsupported type %q", providerConfig.Name, providerConfig.Type)
	}
	chatModel, err := einopenai.NewChatModel(ctx, &einopenai.ChatModelConfig{
		BaseURL: providerConfig.BaseURL,
		APIKey:  providerConfig.APIKey,
		Model:   defaultModel,
		Timeout: providerConfig.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize Eino OpenAI ChatModel: %w", err)
	}
	return chateino.NewProvider(chatModel), nil
}

func buildModelRegistry(
	ctx context.Context,
	appConfig config.Config,
	factory providerFactory,
) (*service.Registry, error) {
	defaultModels := make(map[string]string, len(appConfig.Providers))
	for _, modelConfig := range appConfig.Models {
		if _, exists := defaultModels[modelConfig.Provider]; !exists {
			defaultModels[modelConfig.Provider] = modelConfig.UpstreamModel
		}
	}

	providers := make(map[string]service.ChatProvider, len(appConfig.Providers))
	for _, providerConfig := range appConfig.Providers {
		if _, exists := providers[providerConfig.Name]; exists {
			return nil, fmt.Errorf("provider name %q is duplicated", providerConfig.Name)
		}
		provider, err := factory(ctx, providerConfig, defaultModels[providerConfig.Name])
		if err != nil {
			return nil, fmt.Errorf("initialize provider %q: %w", providerConfig.Name, err)
		}
		providers[providerConfig.Name] = provider
	}

	routes := make([]service.ModelRoute, 0, len(appConfig.Models))
	for _, modelConfig := range appConfig.Models {
		provider, exists := providers[modelConfig.Provider]
		if !exists {
			return nil, fmt.Errorf("model %q references unknown provider %q", modelConfig.Name, modelConfig.Provider)
		}
		routes = append(routes, service.ModelRoute{
			ExposedModel:  modelConfig.Name,
			UpstreamModel: modelConfig.UpstreamModel,
			ProviderName:  modelConfig.Provider,
			Provider:      provider,
		})
	}
	registry, err := service.NewModelRegistry(routes)
	if err != nil {
		return nil, fmt.Errorf("initialize model registry: %w", err)
	}
	return registry, nil
}

// Run 启动 HTTP Server，并在 Context 取消时优雅关闭。
func (a *App) Run(ctx context.Context) error {
	serverError := make(chan error, 1)
	go func() {
		serverError <- a.server.ListenAndServe()
	}()

	select {
	case err := <-serverError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := a.server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		err := <-serverError
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP during shutdown: %w", err)
		}
		return nil
	}
}
