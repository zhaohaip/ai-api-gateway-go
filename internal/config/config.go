// Package config 从环境变量加载网关启动配置。
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	defaultAddress         = ":8080"
	defaultBaseURL         = "https://api.deepseek.com/v1"
	defaultPublicModel     = "deepseek-v4-pro"
	defaultUpstreamTimeout = 30 * time.Second
)

// Config 表示应用启动所需的全部配置。
type Config struct {
	Address  string
	Upstream UpstreamConfig
}

// UpstreamConfig 表示单个 OpenAI 兼容上游模型配置。
type UpstreamConfig struct {
	BaseURL     string
	APIKey      string
	PublicModel string
	Model       string
	Timeout     time.Duration
}

// Load 从环境变量加载并校验配置。
func Load() (Config, error) {
	timeout, err := loadTimeout()
	if err != nil {
		return Config{}, err
	}
	config := Config{
		Address: envOrDefault("AI_GATEWAY_ADDR", defaultAddress),
		Upstream: UpstreamConfig{
			BaseURL:     envOrDefault("AI_GATEWAY_UPSTREAM_BASE_URL", defaultBaseURL),
			APIKey:      strings.TrimSpace(os.Getenv("AI_GATEWAY_UPSTREAM_API_KEY")),
			PublicModel: envOrDefault("AI_GATEWAY_PUBLIC_MODEL", defaultPublicModel),
			Model:       strings.TrimSpace(os.Getenv("AI_GATEWAY_UPSTREAM_MODEL")),
			Timeout:     timeout,
		},
	}
	if config.Upstream.APIKey == "" {
		return Config{}, fmt.Errorf("AI_GATEWAY_UPSTREAM_API_KEY is required")
	}
	if config.Upstream.Model == "" {
		return Config{}, fmt.Errorf("AI_GATEWAY_UPSTREAM_MODEL is required")
	}
	return config, nil
}

func loadTimeout() (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv("AI_GATEWAY_UPSTREAM_TIMEOUT"))
	if value == "" {
		return defaultUpstreamTimeout, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse AI_GATEWAY_UPSTREAM_TIMEOUT: %w", err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("AI_GATEWAY_UPSTREAM_TIMEOUT must be greater than zero")
	}
	return timeout, nil
}

func envOrDefault(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}
