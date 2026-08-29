// Package config 从 YAML 文件和环境变量加载网关启动配置。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultAddress               = ":8080"
	configFileEnvironment        = "AI_GATEWAY_CONFIG_FILE"
	ProviderTypeOpenAICompatible = "openai-compatible"
)

// Config 表示应用启动所需的全部配置。
type Config struct {
	Address   string
	Providers []ProviderConfig
	Models    []ModelConfig
}

// ProviderConfig 表示一个上游服务连接配置。
type ProviderConfig struct {
	Name      string
	Type      string
	BaseURL   string
	APIKeyEnv string
	APIKey    string
	Timeout   time.Duration
}

// ModelConfig 表示一个对外逻辑模型到上游模型的映射。
type ModelConfig struct {
	Name          string `yaml:"name"`
	Provider      string `yaml:"provider"`
	UpstreamModel string `yaml:"upstream_model"`
}

type fileConfig struct {
	Address   string               `yaml:"address"`
	Providers []fileProviderConfig `yaml:"providers"`
	Models    []ModelConfig        `yaml:"models"`
}

type fileProviderConfig struct {
	Name      string `yaml:"name"`
	Type      string `yaml:"type"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	Timeout   string `yaml:"timeout"`
}

// Load 从 AI_GATEWAY_CONFIG_FILE 指定的 YAML 文件加载并校验配置。
func Load() (Config, error) {
	path := strings.TrimSpace(os.Getenv(configFileEnvironment))
	if path == "" {
		return Config{}, fmt.Errorf("%s is required", configFileEnvironment)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read gateway config: %w", err)
	}
	return parse(contents, os.LookupEnv)
}

func parse(contents []byte, lookupEnv func(string) (string, bool)) (Config, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)

	var loaded fileConfig
	if err := decoder.Decode(&loaded); err != nil {
		return Config{}, fmt.Errorf("decode gateway config: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("decode gateway config: %w", err)
	} else if err == nil {
		return Config{}, fmt.Errorf("gateway config must contain one YAML document")
	}

	config := Config{
		Address: strings.TrimSpace(loaded.Address),
		Models:  make([]ModelConfig, 0, len(loaded.Models)),
	}
	if config.Address == "" {
		config.Address = defaultAddress
	}
	if address, exists := lookupEnv("AI_GATEWAY_ADDR"); exists && strings.TrimSpace(address) != "" {
		config.Address = strings.TrimSpace(address)
	}

	config.Providers = make([]ProviderConfig, 0, len(loaded.Providers))
	providerNames := make(map[string]struct{}, len(loaded.Providers))
	for index, provider := range loaded.Providers {
		validated, err := validateProvider(index, provider, lookupEnv)
		if err != nil {
			return Config{}, err
		}
		if _, exists := providerNames[validated.Name]; exists {
			return Config{}, fmt.Errorf("provider name %q is duplicated", validated.Name)
		}
		providerNames[validated.Name] = struct{}{}
		config.Providers = append(config.Providers, validated)
	}
	if len(config.Providers) == 0 {
		return Config{}, fmt.Errorf("at least one provider is required")
	}

	modelNames := make(map[string]struct{}, len(loaded.Models))
	for index, model := range loaded.Models {
		model.Name = strings.TrimSpace(model.Name)
		model.Provider = strings.TrimSpace(model.Provider)
		model.UpstreamModel = strings.TrimSpace(model.UpstreamModel)
		if model.Name == "" {
			return Config{}, fmt.Errorf("models[%d].name is required", index)
		}
		if _, exists := modelNames[model.Name]; exists {
			return Config{}, fmt.Errorf("model name %q is duplicated", model.Name)
		}
		if _, exists := providerNames[model.Provider]; !exists {
			return Config{}, fmt.Errorf("model %q references unknown provider %q", model.Name, model.Provider)
		}
		if model.UpstreamModel == "" {
			return Config{}, fmt.Errorf("models[%d].upstream_model is required", index)
		}
		modelNames[model.Name] = struct{}{}
		config.Models = append(config.Models, model)
	}
	if len(config.Models) == 0 {
		return Config{}, fmt.Errorf("at least one model is required")
	}
	return config, nil
}

func validateProvider(
	index int,
	provider fileProviderConfig,
	lookupEnv func(string) (string, bool),
) (ProviderConfig, error) {
	provider.Name = strings.TrimSpace(provider.Name)
	provider.Type = strings.TrimSpace(provider.Type)
	provider.BaseURL = strings.TrimSpace(provider.BaseURL)
	provider.APIKeyEnv = strings.TrimSpace(provider.APIKeyEnv)
	provider.Timeout = strings.TrimSpace(provider.Timeout)

	if provider.Name == "" {
		return ProviderConfig{}, fmt.Errorf("providers[%d].name is required", index)
	}
	if provider.Type != ProviderTypeOpenAICompatible {
		return ProviderConfig{}, fmt.Errorf("provider %q has unsupported type %q", provider.Name, provider.Type)
	}
	if err := validateBaseURL(provider.BaseURL); err != nil {
		return ProviderConfig{}, fmt.Errorf("provider %q base_url: %w", provider.Name, err)
	}
	if provider.APIKeyEnv == "" {
		return ProviderConfig{}, fmt.Errorf("provider %q api_key_env is required", provider.Name)
	}
	apiKey, exists := lookupEnv(provider.APIKeyEnv)
	if !exists || strings.TrimSpace(apiKey) == "" {
		return ProviderConfig{}, fmt.Errorf(
			"provider %q API key environment variable %s is required",
			provider.Name,
			provider.APIKeyEnv,
		)
	}
	timeout, err := time.ParseDuration(provider.Timeout)
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("provider %q timeout is invalid: %w", provider.Name, err)
	}
	if timeout <= 0 {
		return ProviderConfig{}, fmt.Errorf("provider %q timeout must be greater than zero", provider.Name)
	}
	return ProviderConfig{
		Name:      provider.Name,
		Type:      provider.Type,
		BaseURL:   provider.BaseURL,
		APIKeyEnv: provider.APIKeyEnv,
		APIKey:    strings.TrimSpace(apiKey),
		Timeout:   timeout,
	}, nil
}

func validateBaseURL(value string) error {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("must be a valid HTTP or HTTPS URL")
	}
	return nil
}
