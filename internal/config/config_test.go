package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfigYAML = `
address: 127.0.0.1:9090
providers:
  - name: deepseek
    type: openai-compatible
    base_url: https://api.deepseek.example/v1
    api_key_env: DEEPSEEK_API_KEY
    timeout: 60s
  - name: qwen
    type: openai-compatible
    base_url: https://qwen.example/compatible-mode/v1
    api_key_env: QWEN_API_KEY
    timeout: 30s
models:
  - name: default-chat
    provider: deepseek
    upstream_model: deepseek-chat
  - name: reasoning-chat
    provider: deepseek
    upstream_model: deepseek-reasoner
  - name: fast-chat
    provider: qwen
    upstream_model: qwen-plus
`

func TestLoadMultipleProvidersAndModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(validConfigYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(configFileEnvironment, path)
	t.Setenv("DEEPSEEK_API_KEY", "deepseek-secret")
	t.Setenv("QWEN_API_KEY", "qwen-secret")

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.Address != "127.0.0.1:9090" || len(config.Providers) != 2 || len(config.Models) != 3 {
		t.Fatalf("Load() config = %#v", config)
	}
	if config.Providers[0].Timeout != time.Minute || config.Providers[0].APIKey != "deepseek-secret" {
		t.Fatalf("first provider = %#v", config.Providers[0])
	}
	if config.Models[0].Name != "default-chat" || config.Models[1].Provider != "deepseek" {
		t.Fatalf("models = %#v", config.Models)
	}
}

func TestLoadRequiresConfigFile(t *testing.T) {
	t.Setenv(configFileEnvironment, "")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), configFileEnvironment) {
		t.Fatalf("Load() error = %v, want missing config file", err)
	}
}

func TestParseRejectsInvalidProviderAndModelConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		keys    map[string]string
		wantErr string
	}{
		{
			name: "no providers",
			config: `models:
  - name: chat
    provider: missing
    upstream_model: upstream`,
			wantErr: "at least one provider",
		},
		{
			name: "duplicate provider name",
			config: `providers:
  - &provider
    name: same
    type: openai-compatible
    base_url: https://one.example/v1
    api_key_env: KEY
    timeout: 1s
  - <<: *provider
models:
  - name: chat
    provider: same
    upstream_model: upstream`,
			keys:    map[string]string{"KEY": "secret"},
			wantErr: "provider name \"same\" is duplicated",
		},
		{
			name: "duplicate model name",
			config: singleProviderYAML() + `models:
  - name: chat
    provider: provider
    upstream_model: first
  - name: chat
    provider: provider
    upstream_model: second`,
			keys:    map[string]string{"KEY": "secret"},
			wantErr: "model name \"chat\" is duplicated",
		},
		{
			name: "unknown provider reference",
			config: singleProviderYAML() + `models:
  - name: chat
    provider: missing
    upstream_model: upstream`,
			keys:    map[string]string{"KEY": "secret"},
			wantErr: "references unknown provider",
		},
		{
			name:    "missing API key environment variable",
			config:  singleProviderYAML() + validSingleModelYAML(),
			wantErr: "API key environment variable KEY is required",
		},
		{
			name: "invalid base URL",
			config: strings.Replace(singleProviderYAML(), "https://provider.example/v1", "file:///tmp/provider", 1) +
				validSingleModelYAML(),
			keys:    map[string]string{"KEY": "secret"},
			wantErr: "valid HTTP or HTTPS URL",
		},
		{
			name:    "invalid timeout",
			config:  strings.Replace(singleProviderYAML(), "timeout: 1s", "timeout: zero", 1) + validSingleModelYAML(),
			keys:    map[string]string{"KEY": "secret"},
			wantErr: "timeout is invalid",
		},
		{
			name:    "non-positive timeout",
			config:  strings.Replace(singleProviderYAML(), "timeout: 1s", "timeout: 0s", 1) + validSingleModelYAML(),
			keys:    map[string]string{"KEY": "secret"},
			wantErr: "greater than zero",
		},
		{
			name:    "unsupported provider type",
			config:  strings.Replace(singleProviderYAML(), "openai-compatible", "unsupported", 1) + validSingleModelYAML(),
			keys:    map[string]string{"KEY": "secret"},
			wantErr: "unsupported type",
		},
		{
			name:    "no models",
			config:  singleProviderYAML(),
			keys:    map[string]string{"KEY": "secret"},
			wantErr: "at least one model",
		},
		{
			name: "empty upstream model",
			config: singleProviderYAML() + `models:
  - name: chat
    provider: provider
    upstream_model: ""`,
			keys:    map[string]string{"KEY": "secret"},
			wantErr: "upstream_model is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parse([]byte(test.config), mapLookup(test.keys))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("parse() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func singleProviderYAML() string {
	return `providers:
  - name: provider
    type: openai-compatible
    base_url: https://provider.example/v1
    api_key_env: KEY
    timeout: 1s
`
}

func validSingleModelYAML() string {
	return `models:
  - name: chat
    provider: provider
    upstream_model: upstream
`
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, exists := values[name]
		return value, exists
	}
}
