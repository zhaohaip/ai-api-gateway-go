// Package app 负责应用依赖装配和 HTTP Server 生命周期。
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	einopenai "github.com/cloudwego/eino-ext/components/model/openai"

	chateino "github.com/zhaohaip/ai-api-gateway-go/internal/chat/adapter/eino"
	chatapi "github.com/zhaohaip/ai-api-gateway-go/internal/chat/api"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/service"
	"github.com/zhaohaip/ai-api-gateway-go/internal/config"
)

const shutdownTimeout = 10 * time.Second

// App 表示已完成依赖装配的网关应用。
type App struct {
	server *http.Server
}

// New 在启动阶段初始化并复用 Eino ChatModel。
func New(ctx context.Context, config config.Config) (*App, error) {
	chatModel, err := einopenai.NewChatModel(ctx, &einopenai.ChatModelConfig{
		BaseURL: config.Upstream.BaseURL,
		APIKey:  config.Upstream.APIKey,
		Model:   config.Upstream.Model,
		Timeout: config.Upstream.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize Eino OpenAI ChatModel: %w", err)
	}
	provider := chateino.NewProvider(chatModel, chateino.ProviderConfig{
		PublicModel:   config.Upstream.PublicModel,
		UpstreamModel: config.Upstream.Model,
	})
	chatService := service.NewChatService(provider)
	handler := chatapi.NewHandler(chatService)

	return &App{
		server: &http.Server{
			Addr:              config.Address,
			Handler:           chatapi.NewRouter(handler),
			ReadHeaderTimeout: 5 * time.Second,
		},
	}, nil
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
