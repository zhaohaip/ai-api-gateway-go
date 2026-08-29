package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	t.Setenv("AI_GATEWAY_ADDR", "127.0.0.1:9090")
	t.Setenv("AI_GATEWAY_UPSTREAM_BASE_URL", "https://example.com/v1")
	t.Setenv("AI_GATEWAY_UPSTREAM_API_KEY", "test-key")
	t.Setenv("AI_GATEWAY_PUBLIC_MODEL", "public-chat")
	t.Setenv("AI_GATEWAY_UPSTREAM_MODEL", "private-model")
	t.Setenv("AI_GATEWAY_UPSTREAM_TIMEOUT", "2s")

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.Address != "127.0.0.1:9090" || config.Upstream.BaseURL != "https://example.com/v1" {
		t.Fatalf("Load() config = %#v", config)
	}
	if config.Upstream.PublicModel != "public-chat" || config.Upstream.Model != "private-model" {
		t.Fatalf("Load() models = %#v", config.Upstream)
	}
	if config.Upstream.Timeout != 2*time.Second {
		t.Fatalf("Load() timeout = %s", config.Upstream.Timeout)
	}
}

func TestLoadRequiresSecretsAndUpstreamModel(t *testing.T) {
	t.Setenv("AI_GATEWAY_UPSTREAM_API_KEY", "")
	t.Setenv("AI_GATEWAY_UPSTREAM_MODEL", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "AI_GATEWAY_UPSTREAM_API_KEY") {
		t.Fatalf("Load() error = %v, want missing API key", err)
	}
}

func TestLoadRejectsInvalidTimeout(t *testing.T) {
	t.Setenv("AI_GATEWAY_UPSTREAM_API_KEY", "test-key")
	t.Setenv("AI_GATEWAY_UPSTREAM_MODEL", "upstream")
	t.Setenv("AI_GATEWAY_UPSTREAM_TIMEOUT", "0s")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "greater than zero") {
		t.Fatalf("Load() error = %v, want invalid timeout", err)
	}
}
