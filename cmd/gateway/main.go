package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/zhaohaip/ai-api-gateway-go/internal/app"
	"github.com/zhaohaip/ai-api-gateway-go/internal/config"
)

func main() {
	applicationConfig, err := config.Load()
	if err != nil {
		slog.Error("加载网关配置失败", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application, err := app.New(ctx, applicationConfig)
	if err != nil {
		slog.Error("初始化网关失败", "error", err)
		os.Exit(1)
	}
	slog.Info("AI API 网关开始监听", "address", applicationConfig.Address)
	if err := application.Run(ctx); err != nil {
		slog.Error("AI API 网关退出", "error", err)
		os.Exit(1)
	}
}
