// Package config 从 YAML 文件和环境变量加载网关启动配置。
package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zhaohaip/ai-api-gateway-go/internal/auth"
	"github.com/zhaohaip/ai-api-gateway-go/internal/concurrencylimit"
	"github.com/zhaohaip/ai-api-gateway-go/internal/ratelimit"
)

const (
	defaultAddress               = ":8080"
	configFileEnvironment        = "AI_GATEWAY_CONFIG_FILE"
	ProviderTypeOpenAICompatible = "openai-compatible"
)

// Config 表示应用启动所需的全部配置。
type Config struct {
	Address   string
	Auth      AuthConfig
	Limits    LimitsConfig
	Timeouts  TimeoutConfig
	Providers []ProviderConfig
	Models    []ModelConfig
}

// AuthConfig 表示客户端访问网关所需的认证配置。
type AuthConfig struct {
	APIKeys []auth.APIKey
}

// LimitsConfig 表示网关的全局和默认 API Key 请求限制。
type LimitsConfig struct {
	Global        RequestLimits
	DefaultAPIKey RequestLimits
}

// RequestLimits 表示一个范围内的频率和并发限制。
type RequestLimits struct {
	Rate           ratelimit.Limit
	MaxConcurrency int
}

// TimeoutConfig 表示非流式和 SSE 请求的业务超时。
type TimeoutConfig struct {
	NonStream time.Duration
	Stream    StreamTimeoutConfig
}

// StreamTimeoutConfig 表示 SSE 首包、空闲和总时长超时。
type StreamTimeoutConfig struct {
	FirstChunk time.Duration
	Idle       time.Duration
	Total      time.Duration
}

// Validate 校验业务超时值及其组合关系；0 表示禁用对应超时。
func (c TimeoutConfig) Validate() error {
	values := []struct {
		name     string
		duration time.Duration
	}{
		{name: "timeouts.non_stream", duration: c.NonStream},
		{name: "timeouts.stream.first_chunk", duration: c.Stream.FirstChunk},
		{name: "timeouts.stream.idle", duration: c.Stream.Idle},
		{name: "timeouts.stream.total", duration: c.Stream.Total},
	}
	for _, value := range values {
		if value.duration < 0 {
			return fmt.Errorf("%s must be non-negative", value.name)
		}
	}
	if c.Stream.Total > 0 && c.Stream.FirstChunk > 0 && c.Stream.Total < c.Stream.FirstChunk {
		return fmt.Errorf("timeouts.stream.total must not be less than first_chunk")
	}
	if c.Stream.Total > 0 && c.Stream.Idle > 0 && c.Stream.Total < c.Stream.Idle {
		return fmt.Errorf("timeouts.stream.total must not be less than idle")
	}
	return nil
}

// ProviderConfig 表示一个上游服务连接配置。
type ProviderConfig struct {
	Name                  string
	Type                  string
	BaseURL               string
	APIKeyEnv             string
	APIKey                string
	Timeout               time.Duration
	ConnectTimeout        time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
}

// ModelConfig 表示一个对外逻辑模型到上游模型的映射。
type ModelConfig struct {
	Name          string `yaml:"name"`
	Provider      string `yaml:"provider"`
	UpstreamModel string `yaml:"upstream_model"`
}

type fileConfig struct {
	Address   string               `yaml:"address"`
	Auth      fileAuthConfig       `yaml:"auth"`
	Limits    fileLimitsConfig     `yaml:"limits"`
	Timeouts  fileTimeoutConfig    `yaml:"timeouts"`
	Providers []fileProviderConfig `yaml:"providers"`
	Models    []ModelConfig        `yaml:"models"`
}

type fileAuthConfig struct {
	APIKeys []fileAPIKeyConfig `yaml:"api_keys"`
}

type fileLimitsConfig struct {
	Global        fileLimitConfig `yaml:"global"`
	DefaultAPIKey fileLimitConfig `yaml:"default_api_key"`
}

type fileLimitConfig struct {
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	Burst             int     `yaml:"burst"`
	MaxConcurrency    int     `yaml:"max_concurrency"`
}

type fileTimeoutConfig struct {
	NonStream string                  `yaml:"non_stream"`
	Stream    fileStreamTimeoutConfig `yaml:"stream"`
}

type fileStreamTimeoutConfig struct {
	FirstChunk string `yaml:"first_chunk"`
	Idle       string `yaml:"idle"`
	Total      string `yaml:"total"`
}

type fileAPIKeyConfig struct {
	ID            string   `yaml:"id"`
	KeyEnv        string   `yaml:"key_env"`
	Enabled       bool     `yaml:"enabled"`
	AllowedModels []string `yaml:"allowed_models"`
}

type fileProviderConfig struct {
	Name                  string `yaml:"name"`
	Type                  string `yaml:"type"`
	BaseURL               string `yaml:"base_url"`
	APIKeyEnv             string `yaml:"api_key_env"`
	Timeout               string `yaml:"timeout"`
	ConnectTimeout        string `yaml:"connect_timeout"`
	TLSHandshakeTimeout   string `yaml:"tls_handshake_timeout"`
	ResponseHeaderTimeout string `yaml:"response_header_timeout"`
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

	timeouts, err := parseTimeouts(loaded.Timeouts)
	if err != nil {
		return Config{}, err
	}
	config := Config{
		Address:  strings.TrimSpace(loaded.Address),
		Timeouts: timeouts,
		Limits: LimitsConfig{
			Global: RequestLimits{
				Rate: ratelimit.Limit{
					RequestsPerSecond: loaded.Limits.Global.RequestsPerSecond,
					Burst:             loaded.Limits.Global.Burst,
				},
				MaxConcurrency: loaded.Limits.Global.MaxConcurrency,
			},
			DefaultAPIKey: RequestLimits{
				Rate: ratelimit.Limit{
					RequestsPerSecond: loaded.Limits.DefaultAPIKey.RequestsPerSecond,
					Burst:             loaded.Limits.DefaultAPIKey.Burst,
				},
				MaxConcurrency: loaded.Limits.DefaultAPIKey.MaxConcurrency,
			},
		},
		Models: make([]ModelConfig, 0, len(loaded.Models)),
	}
	if config.Address == "" {
		config.Address = defaultAddress
	}
	if address, exists := lookupEnv("AI_GATEWAY_ADDR"); exists && strings.TrimSpace(address) != "" {
		config.Address = strings.TrimSpace(address)
	}
	if err := validateLimits(config.Limits); err != nil {
		return Config{}, err
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
	apiKeys, err := validateClientAPIKeys(loaded.Auth.APIKeys, modelNames, lookupEnv)
	if err != nil {
		return Config{}, err
	}
	config.Auth.APIKeys = apiKeys
	return config, nil
}

func parseTimeouts(loaded fileTimeoutConfig) (TimeoutConfig, error) {
	nonStream, err := parseOptionalDuration("timeouts.non_stream", loaded.NonStream)
	if err != nil {
		return TimeoutConfig{}, err
	}
	firstChunk, err := parseOptionalDuration("timeouts.stream.first_chunk", loaded.Stream.FirstChunk)
	if err != nil {
		return TimeoutConfig{}, err
	}
	idle, err := parseOptionalDuration("timeouts.stream.idle", loaded.Stream.Idle)
	if err != nil {
		return TimeoutConfig{}, err
	}
	total, err := parseOptionalDuration("timeouts.stream.total", loaded.Stream.Total)
	if err != nil {
		return TimeoutConfig{}, err
	}
	timeouts := TimeoutConfig{
		NonStream: nonStream,
		Stream: StreamTimeoutConfig{
			FirstChunk: firstChunk,
			Idle:       idle,
			Total:      total,
		},
	}
	if err := timeouts.Validate(); err != nil {
		return TimeoutConfig{}, err
	}
	return timeouts, nil
}

func parseOptionalDuration(name, value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid: %w", name, err)
	}
	if duration < 0 {
		return 0, fmt.Errorf("%s must be non-negative", name)
	}
	return duration, nil
}

func validateLimits(limits LimitsConfig) error {
	if _, err := ratelimit.NewMemoryLimiter(limits.Global.Rate, limits.DefaultAPIKey.Rate); err != nil {
		return fmt.Errorf("limits configuration is invalid: %w", err)
	}
	if _, err := concurrencylimit.NewMemoryController(
		limits.Global.MaxConcurrency,
		limits.DefaultAPIKey.MaxConcurrency,
	); err != nil {
		return fmt.Errorf("limits configuration is invalid: %w", err)
	}
	return nil
}

func validateClientAPIKeys(
	apiKeys []fileAPIKeyConfig,
	modelNames map[string]struct{},
	lookupEnv func(string) (string, bool),
) ([]auth.APIKey, error) {
	if len(apiKeys) == 0 {
		return nil, fmt.Errorf("at least one client API key is required")
	}
	result := make([]auth.APIKey, 0, len(apiKeys))
	keyIDs := make(map[string]struct{}, len(apiKeys))
	keyHashes := make(map[[sha256.Size]byte]struct{}, len(apiKeys))
	for index, apiKey := range apiKeys {
		apiKey.ID = strings.TrimSpace(apiKey.ID)
		apiKey.KeyEnv = strings.TrimSpace(apiKey.KeyEnv)
		if apiKey.ID == "" {
			return nil, fmt.Errorf("auth.api_keys[%d].id is required", index)
		}
		if _, exists := keyIDs[apiKey.ID]; exists {
			return nil, fmt.Errorf("client API key ID %q is duplicated", apiKey.ID)
		}
		if apiKey.KeyEnv == "" {
			return nil, fmt.Errorf("client API key %q key_env is required", apiKey.ID)
		}
		rawKey, exists := lookupEnv(apiKey.KeyEnv)
		rawKey = strings.TrimSpace(rawKey)
		if !exists || rawKey == "" {
			return nil, fmt.Errorf(
				"client API key %q environment variable %s is required",
				apiKey.ID,
				apiKey.KeyEnv,
			)
		}
		keyHash := sha256.Sum256([]byte(rawKey))
		if _, exists := keyHashes[keyHash]; exists {
			return nil, fmt.Errorf("client API key value is duplicated")
		}
		allowedModels := make([]string, 0, len(apiKey.AllowedModels))
		for modelIndex, allowedModel := range apiKey.AllowedModels {
			allowedModel = strings.TrimSpace(allowedModel)
			if allowedModel != "*" {
				if _, exists := modelNames[allowedModel]; !exists {
					return nil, fmt.Errorf(
						"auth.api_keys[%d].allowed_models[%d] references unknown model %q",
						index,
						modelIndex,
						allowedModel,
					)
				}
			}
			allowedModels = append(allowedModels, allowedModel)
		}
		keyIDs[apiKey.ID] = struct{}{}
		keyHashes[keyHash] = struct{}{}
		result = append(result, auth.APIKey{
			ID:            apiKey.ID,
			KeyHash:       keyHash,
			Enabled:       apiKey.Enabled,
			AllowedModels: allowedModels,
		})
	}
	return result, nil
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
	provider.ConnectTimeout = strings.TrimSpace(provider.ConnectTimeout)
	provider.TLSHandshakeTimeout = strings.TrimSpace(provider.TLSHandshakeTimeout)
	provider.ResponseHeaderTimeout = strings.TrimSpace(provider.ResponseHeaderTimeout)

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
	var legacyTimeout time.Duration
	var err error
	if provider.Timeout != "" {
		legacyTimeout, err = parseProviderTimeout(provider.Name, "timeout", provider.Timeout, 0)
		if err != nil {
			return ProviderConfig{}, err
		}
	}
	connectTimeout, err := parseProviderTimeout(
		provider.Name,
		"connect_timeout",
		provider.ConnectTimeout,
		legacyTimeout,
	)
	if err != nil {
		return ProviderConfig{}, err
	}
	tlsHandshakeTimeout, err := parseProviderTimeout(
		provider.Name,
		"tls_handshake_timeout",
		provider.TLSHandshakeTimeout,
		legacyTimeout,
	)
	if err != nil {
		return ProviderConfig{}, err
	}
	responseHeaderTimeout, err := parseProviderTimeout(
		provider.Name,
		"response_header_timeout",
		provider.ResponseHeaderTimeout,
		legacyTimeout,
	)
	if err != nil {
		return ProviderConfig{}, err
	}
	return ProviderConfig{
		Name:                  provider.Name,
		Type:                  provider.Type,
		BaseURL:               provider.BaseURL,
		APIKeyEnv:             provider.APIKeyEnv,
		APIKey:                strings.TrimSpace(apiKey),
		Timeout:               legacyTimeout,
		ConnectTimeout:        connectTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}, nil
}

func parseProviderTimeout(
	providerName string,
	fieldName string,
	value string,
	fallback time.Duration,
) (time.Duration, error) {
	if value == "" {
		if fallback > 0 {
			return fallback, nil
		}
		return 0, fmt.Errorf("provider %q %s is required", providerName, fieldName)
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("provider %q %s is invalid: %w", providerName, fieldName, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("provider %q %s must be greater than zero", providerName, fieldName)
	}
	return duration, nil
}

func validateBaseURL(value string) error {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("must be a valid HTTP or HTTPS URL")
	}
	return nil
}
