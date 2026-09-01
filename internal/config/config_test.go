package config

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfigYAML = `
address: 127.0.0.1:9090
auth:
  api_keys:
    - id: demo-client
      key_env: GATEWAY_DEMO_API_KEY
      enabled: true
      allowed_models: [default-chat, fast-chat]
    - id: internal-client
      key_env: GATEWAY_INTERNAL_API_KEY
      enabled: true
      allowed_models: ["*"]
limits:
  global:
    requests_per_second: 100
    burst: 200
    max_concurrency: 50
  default_api_key:
    requests_per_second: 5
    burst: 10
    max_concurrency: 3
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
	t.Setenv("GATEWAY_DEMO_API_KEY", "sk-gw-demo")
	t.Setenv("GATEWAY_INTERNAL_API_KEY", "sk-gw-internal")

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
	if len(config.Auth.APIKeys) != 2 || config.Auth.APIKeys[0].KeyHash != sha256.Sum256([]byte("sk-gw-demo")) {
		t.Fatalf("client API keys = %#v", config.Auth.APIKeys)
	}
	if config.Limits.Global.Rate.RequestsPerSecond != 100 || config.Limits.Global.Rate.Burst != 200 ||
		config.Limits.Global.MaxConcurrency != 50 ||
		config.Limits.DefaultAPIKey.Rate.RequestsPerSecond != 5 ||
		config.Limits.DefaultAPIKey.Rate.Burst != 10 || config.Limits.DefaultAPIKey.MaxConcurrency != 3 {
		t.Fatalf("limits = %#v", config.Limits)
	}
}

func TestParseValidatesRequestLimits(t *testing.T) {
	baseConfig := `auth:
  api_keys:
    - {id: client, key_env: CLIENT_KEY, enabled: true, allowed_models: [chat]}
` + singleProviderYAML() + validSingleModelYAML()
	tests := []struct {
		name    string
		limits  string
		wantErr string
	}{
		{
			name: "negative global rate",
			limits: `limits:
  global: {requests_per_second: -1, burst: 1}
`,
			wantErr: "global requests_per_second",
		},
		{
			name: "negative API key burst",
			limits: `limits:
  default_api_key: {requests_per_second: 1, burst: -1}
`,
			wantErr: "default_api_key burst",
		},
		{
			name: "rate without burst",
			limits: `limits:
  global: {requests_per_second: 1, burst: 0}
`,
			wantErr: "must both be zero or greater than zero",
		},
		{
			name: "burst without rate",
			limits: `limits:
  default_api_key: {requests_per_second: 0, burst: 1}
`,
			wantErr: "must both be zero or greater than zero",
		},
		{
			name: "negative global concurrency",
			limits: `limits:
  global: {max_concurrency: -1}
`,
			wantErr: "global max_concurrency must be non-negative",
		},
		{
			name: "negative API key concurrency",
			limits: `limits:
  default_api_key: {max_concurrency: -1}
`,
			wantErr: "default_api_key max_concurrency must be non-negative",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parse([]byte(test.limits+baseConfig), mapLookup(map[string]string{
				"KEY":        "provider",
				"CLIENT_KEY": "client",
			}))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("parse() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestParseTreatsZeroRequestLimitsAsDisabled(t *testing.T) {
	contents := `limits:
  global: {requests_per_second: 0, burst: 0}
  default_api_key: {requests_per_second: 0, burst: 0, max_concurrency: 0}
auth:
  api_keys:
    - {id: client, key_env: CLIENT_KEY, enabled: true, allowed_models: [chat]}
` + singleProviderYAML() + validSingleModelYAML()
	config, err := parse([]byte(contents), mapLookup(map[string]string{
		"KEY":        "provider",
		"CLIENT_KEY": "client",
	}))
	if err != nil {
		t.Fatalf("parse() error = %v", err)
	}
	if config.Limits.Global != (RequestLimits{}) || config.Limits.DefaultAPIKey != (RequestLimits{}) {
		t.Fatalf("limits = %#v", config.Limits)
	}
}

func TestParseRejectsInvalidClientAPIKeyConfiguration(t *testing.T) {
	baseConfig := singleProviderYAML() + validSingleModelYAML()
	tests := []struct {
		name    string
		auth    string
		keys    map[string]string
		wantErr string
	}{
		{
			name:    "no client keys",
			wantErr: "at least one client API key",
		},
		{
			name: "duplicate key ID",
			auth: `auth:
  api_keys:
    - {id: same, key_env: CLIENT_ONE, enabled: true, allowed_models: [chat]}
    - {id: same, key_env: CLIENT_TWO, enabled: true, allowed_models: [chat]}
`,
			keys:    map[string]string{"KEY": "provider", "CLIENT_ONE": "first", "CLIENT_TWO": "second"},
			wantErr: "client API key ID \"same\" is duplicated",
		},
		{
			name: "duplicate key value",
			auth: `auth:
  api_keys:
    - {id: first, key_env: CLIENT_ONE, enabled: true, allowed_models: [chat]}
    - {id: second, key_env: CLIENT_TWO, enabled: true, allowed_models: [chat]}
`,
			keys:    map[string]string{"KEY": "provider", "CLIENT_ONE": "same", "CLIENT_TWO": "same"},
			wantErr: "client API key value is duplicated",
		},
		{
			name: "missing key environment variable",
			auth: `auth:
  api_keys:
    - {id: client, key_env: MISSING_CLIENT_KEY, enabled: true, allowed_models: [chat]}
`,
			keys:    map[string]string{"KEY": "provider"},
			wantErr: "environment variable MISSING_CLIENT_KEY is required",
		},
		{
			name: "disabled key still requires environment variable",
			auth: `auth:
  api_keys:
    - {id: disabled, key_env: MISSING_DISABLED_KEY, enabled: false, allowed_models: [chat]}
`,
			keys:    map[string]string{"KEY": "provider"},
			wantErr: "environment variable MISSING_DISABLED_KEY is required",
		},
		{
			name: "unknown allowed model",
			auth: `auth:
  api_keys:
    - {id: client, key_env: CLIENT_KEY, enabled: true, allowed_models: [unknown]}
`,
			keys:    map[string]string{"KEY": "provider", "CLIENT_KEY": "client"},
			wantErr: "references unknown model \"unknown\"",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			keys := test.keys
			if keys == nil {
				keys = map[string]string{"KEY": "provider"}
			}
			_, err := parse([]byte(test.auth+baseConfig), mapLookup(keys))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("parse() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestParseAcceptsWildcardModelPermission(t *testing.T) {
	contents := `auth:
  api_keys:
    - {id: client, key_env: CLIENT_KEY, enabled: true, allowed_models: ["*"]}
` + singleProviderYAML() + validSingleModelYAML()
	config, err := parse([]byte(contents), mapLookup(map[string]string{
		"KEY":        "provider",
		"CLIENT_KEY": "client",
	}))
	if err != nil {
		t.Fatalf("parse() error = %v", err)
	}
	if len(config.Auth.APIKeys) != 1 || config.Auth.APIKeys[0].AllowedModels[0] != "*" {
		t.Fatalf("auth config = %#v", config.Auth)
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
