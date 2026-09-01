package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	openaiapi "github.com/zhaohaip/ai-api-gateway-go/api/openai"
	gatewayauth "github.com/zhaohaip/ai-api-gateway-go/internal/auth"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
	"github.com/zhaohaip/ai-api-gateway-go/internal/ratelimit"
)

type recordingLimiter struct {
	mu   sync.Mutex
	err  error
	keys []string
}

func (l *recordingLimiter) Allow(keyID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, keyID)
	return l.err
}

func (l *recordingLimiter) Calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.keys...)
}

func TestRateLimitReturnsOpenAIErrorRetryAfterAndSafeLog(t *testing.T) {
	const rawKey = "sk-gw-rate-limit-secret"
	providerCalled := false
	provider := handlerFakeProvider{
		generate: func(context.Context, domain.ChatRequest) (domain.ChatResponse, error) {
			providerCalled = true
			return domain.ChatResponse{}, nil
		},
		stream: func(context.Context, domain.ChatRequest) (domain.ChatStream, error) {
			providerCalled = true
			return nil, nil
		},
	}
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			limiter := &recordingLimiter{err: &ratelimit.Error{
				Scope:      ratelimit.ScopeAPIKey,
				RetryAfter: 1500 * time.Millisecond,
			}}
			handler := NewHandlerWithLimiter(newTestChatService(t, provider), limiter)
			var logOutput bytes.Buffer
			handler.logger = slog.New(slog.NewTextHandler(&logOutput, nil))
			router := NewRouter(handler, newAuthTestAuthenticator(t, []gatewayauth.APIKey{
				{
					ID:            "limited-client",
					KeyHash:       sha256.Sum256([]byte(rawKey)),
					Enabled:       true,
					AllowedModels: []string{"default-chat"},
				},
			}))
			body := validRequestJSON()
			if stream {
				body = streamRequestJSON()
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+rawKey)
			request.Header.Set("X-Request-ID", "request-rate-test")
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if recorder.Header().Get("Retry-After") != "2" {
				t.Fatalf("Retry-After = %q, want 2", recorder.Header().Get("Retry-After"))
			}
			if strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
				t.Fatal("rate limit failure committed SSE headers")
			}
			var response openaiapi.ErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Error.Message != "Rate limit exceeded." || response.Error.Type != "rate_limit_error" ||
				response.Error.Param != nil || response.Error.Code != "rate_limit_exceeded" {
				t.Fatalf("error = %#v", response.Error)
			}
			if providerCalled {
				t.Fatal("rate-limited request called provider")
			}
			if calls := limiter.Calls(); len(calls) != 1 || calls[0] != "limited-client" {
				t.Fatalf("limiter calls = %#v", calls)
			}
			logText := logOutput.String()
			for _, field := range []string{
				"request_id=request-rate-test",
				"key_id=limited-client",
				"model=default-chat",
				"limit_scope=api_key",
			} {
				if !strings.Contains(logText, field) {
					t.Fatalf("log = %q, want field %q", logText, field)
				}
			}
			if strings.Contains(logText, rawKey) || strings.Contains(recorder.Body.String(), rawKey) {
				t.Fatal("rate limit error or log exposed API key")
			}
		})
	}
}

func TestRateLimiterRunsAfterAuthenticationAndModelAuthorization(t *testing.T) {
	const rawKey = "sk-gw-order-test"
	limiter := &recordingLimiter{}
	handler := NewHandlerWithLimiter(newTestChatService(t, handlerFakeProvider{}), limiter)
	router := NewRouter(handler, newAuthTestAuthenticator(t, []gatewayauth.APIKey{
		{
			ID:            "restricted-client",
			KeyHash:       sha256.Sum256([]byte(rawKey)),
			Enabled:       true,
			AllowedModels: []string{"other-model"},
		},
	}))

	unauthenticated := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validRequestJSON()))
	unauthenticated.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), unauthenticated)

	unauthorizedModel := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validRequestJSON()))
	unauthorizedModel.Header.Set("Content-Type", "application/json")
	unauthorizedModel.Header.Set("Authorization", "Bearer "+rawKey)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, unauthorizedModel)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("permission status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if calls := limiter.Calls(); len(calls) != 0 {
		t.Fatalf("authentication or authorization failures consumed quota: %#v", calls)
	}
}

func TestRateLimiterAllowsAuthorizedBusinessRequest(t *testing.T) {
	const rawKey = "sk-gw-allowed-rate-test"
	providerCalled := false
	provider := handlerFakeProvider{generate: func(
		_ context.Context,
		request domain.ChatRequest,
	) (domain.ChatResponse, error) {
		providerCalled = true
		return domain.ChatResponse{
			Model:   request.Model,
			Message: domain.Message{Role: domain.RoleAssistant, Content: "ok"},
		}, nil
	}}
	limiter := &recordingLimiter{}
	handler := NewHandlerWithLimiter(newTestChatService(t, provider), limiter)
	router := NewRouter(handler, newAuthTestAuthenticator(t, []gatewayauth.APIKey{
		{
			ID:            "allowed-client",
			KeyHash:       sha256.Sum256([]byte(rawKey)),
			Enabled:       true,
			AllowedModels: []string{"default-chat"},
		},
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validRequestJSON()))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || !providerCalled {
		t.Fatalf("status = %d, provider called = %v, body = %s", recorder.Code, providerCalled, recorder.Body.String())
	}
	if calls := limiter.Calls(); len(calls) != 1 || calls[0] != "allowed-client" {
		t.Fatalf("limiter calls = %#v", calls)
	}
}

func TestModelsEndpointUsesAuthenticatedPrincipalQuota(t *testing.T) {
	const rawKey = "sk-gw-model-list-rate"
	limiter := &recordingLimiter{err: &ratelimit.Error{
		Scope:      ratelimit.ScopeGlobal,
		RetryAfter: time.Second,
	}}
	handler := NewHandlerWithLimiter(newTestChatService(t, handlerFakeProvider{}), limiter)
	router := NewRouter(handler, newAuthTestAuthenticator(t, []gatewayauth.APIKey{
		{
			ID:            "model-reader",
			KeyHash:       sha256.Sum256([]byte(rawKey)),
			Enabled:       true,
			AllowedModels: []string{"*"},
		},
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+rawKey)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if calls := limiter.Calls(); len(calls) != 1 || calls[0] != "model-reader" {
		t.Fatalf("limiter calls = %#v", calls)
	}
}
