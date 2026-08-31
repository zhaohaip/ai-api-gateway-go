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
	"testing"

	openaiapi "github.com/zhaohaip/ai-api-gateway-go/api/openai"
	gatewayauth "github.com/zhaohaip/ai-api-gateway-go/internal/auth"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/service"
)

const (
	enabledGatewayKey  = "sk-gw-enabled-test"
	disabledGatewayKey = "sk-gw-disabled-test"
)

func TestAPIKeyMiddlewareRejectsInvalidAuthorization(t *testing.T) {
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
	handler := NewHandler(newTestChatService(t, provider))
	var logOutput bytes.Buffer
	handler.logger = slog.New(slog.NewTextHandler(&logOutput, nil))
	router := NewRouter(handler, newAuthTestAuthenticator(t, []gatewayauth.APIKey{
		{
			ID:            "enabled-client",
			KeyHash:       sha256.Sum256([]byte(enabledGatewayKey)),
			Enabled:       true,
			AllowedModels: []string{"default-chat"},
		},
		{
			ID:            "disabled-client",
			KeyHash:       sha256.Sum256([]byte(disabledGatewayKey)),
			Enabled:       false,
			AllowedModels: []string{"default-chat"},
		},
	}))

	tests := []struct {
		name    string
		headers []string
		url     string
		cookie  bool
		secret  string
	}{
		{name: "missing header"},
		{name: "non Bearer scheme", headers: []string{"Basic " + enabledGatewayKey}, secret: enabledGatewayKey},
		{name: "empty Bearer token", headers: []string{"Bearer "}},
		{name: "multiple headers", headers: []string{"Bearer " + enabledGatewayKey, "Bearer another"}, secret: enabledGatewayKey},
		{name: "invalid key", headers: []string{"Bearer sk-gw-invalid-test"}, secret: "sk-gw-invalid-test"},
		{name: "disabled key", headers: []string{"Bearer " + disabledGatewayKey}, secret: disabledGatewayKey},
		{
			name:   "query and cookie are ignored",
			url:    "/v1/chat/completions?api_key=" + enabledGatewayKey,
			cookie: true,
			secret: enabledGatewayKey,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requestURL := test.url
			if requestURL == "" {
				requestURL = "/v1/chat/completions"
			}
			request := httptest.NewRequest(http.MethodPost, requestURL, strings.NewReader(validRequestJSON()))
			request.Header.Set("Content-Type", "application/json")
			for _, header := range test.headers {
				request.Header.Add("Authorization", header)
			}
			if test.cookie {
				request.AddCookie(&http.Cookie{Name: "api_key", Value: enabledGatewayKey})
			}
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, request)

			assertAuthenticationError(t, recorder)
			if test.secret != "" && (strings.Contains(recorder.Body.String(), test.secret) ||
				strings.Contains(logOutput.String(), test.secret)) {
				t.Fatal("authentication failure exposed the API key")
			}
		})
	}
	if providerCalled {
		t.Fatal("authentication failure called a provider")
	}
}

func TestAPIKeyPrincipalPropagatesToProvider(t *testing.T) {
	provider := handlerFakeProvider{generate: func(
		ctx context.Context,
		request domain.ChatRequest,
	) (domain.ChatResponse, error) {
		principal, exists := gatewayauth.PrincipalFromContext(ctx)
		if !exists || principal.KeyID != "demo-client" {
			t.Fatalf("provider principal = %#v, exists = %v", principal, exists)
		}
		return domain.ChatResponse{
			Model:   request.Model,
			Message: domain.Message{Role: domain.RoleAssistant, Content: "ok"},
		}, nil
	}}
	handler := NewHandler(newTestChatService(t, provider))
	router := NewRouter(handler, newAuthTestAuthenticator(t, []gatewayauth.APIKey{
		{
			ID:            "demo-client",
			KeyHash:       sha256.Sum256([]byte(enabledGatewayKey)),
			Enabled:       true,
			AllowedModels: []string{"default-chat"},
		},
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validRequestJSON()))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+enabledGatewayKey)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if request.Header.Get("Authorization") != "" {
		t.Fatal("authenticated request retained the Authorization header downstream")
	}
}

func TestModelAuthorizationDeniesNonStreamingAndStreamingRequests(t *testing.T) {
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
	handler := NewHandler(newTestChatService(t, provider))
	router := NewRouter(handler, newAuthTestAuthenticator(t, []gatewayauth.APIKey{
		{
			ID:            "restricted-client",
			KeyHash:       sha256.Sum256([]byte(enabledGatewayKey)),
			Enabled:       true,
			AllowedModels: []string{"fast-chat"},
		},
	}))
	for _, stream := range []bool{false, true} {
		body := validRequestJSON()
		if stream {
			body = streamRequestJSON()
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+enabledGatewayKey)
		recorder := httptest.NewRecorder()

		router.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusForbidden {
			t.Fatalf("stream=%v status = %d, body = %s", stream, recorder.Code, recorder.Body.String())
		}
		if strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatalf("stream=%v committed SSE headers", stream)
		}
		var response openaiapi.ErrorResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if response.Error.Type != "permission_error" || response.Error.Code != "model_access_denied" ||
			response.Error.Param == nil || *response.Error.Param != "model" {
			t.Fatalf("stream=%v error = %#v", stream, response.Error)
		}
	}
	if providerCalled {
		t.Fatal("permission failure called a provider")
	}
}

func TestModelsEndpointFiltersByPrincipalPermissions(t *testing.T) {
	provider := handlerFakeProvider{}
	registry, err := service.NewModelRegistry([]service.ModelRoute{
		{ExposedModel: "default-chat", UpstreamModel: "private-default", ProviderName: "provider", Provider: provider},
		{ExposedModel: "fast-chat", UpstreamModel: "private-fast", ProviderName: "provider", Provider: provider},
	})
	if err != nil {
		t.Fatalf("NewModelRegistry() error = %v", err)
	}
	handler := NewHandler(service.NewChatService(registry))
	tests := []struct {
		name          string
		allowedModels []string
		wantModels    []string
	}{
		{name: "explicit model", allowedModels: []string{"fast-chat"}, wantModels: []string{"fast-chat"}},
		{name: "wildcard", allowedModels: []string{"*"}, wantModels: []string{"default-chat", "fast-chat"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := NewRouter(handler, newAuthTestAuthenticator(t, []gatewayauth.APIKey{
				{
					ID:            "client",
					KeyHash:       sha256.Sum256([]byte(enabledGatewayKey)),
					Enabled:       true,
					AllowedModels: test.allowedModels,
				},
			}))
			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			request.Header.Set("Authorization", "Bearer "+enabledGatewayKey)
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			var response openaiapi.ModelListResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode models response: %v", err)
			}
			if len(response.Data) != len(test.wantModels) {
				t.Fatalf("models = %#v", response.Data)
			}
			for index, want := range test.wantModels {
				if response.Data[index].ID != want {
					t.Fatalf("models[%d] = %q, want %q", index, response.Data[index].ID, want)
				}
			}
			if strings.Contains(recorder.Body.String(), "private-") {
				t.Fatalf("model list leaked upstream model: %s", recorder.Body.String())
			}
		})
	}
}

func TestWildcardKeyCanCallEveryRegisteredModel(t *testing.T) {
	calledUpstreamModels := make([]string, 0, 2)
	provider := handlerFakeProvider{generate: func(
		_ context.Context,
		request domain.ChatRequest,
	) (domain.ChatResponse, error) {
		calledUpstreamModels = append(calledUpstreamModels, request.Model)
		return domain.ChatResponse{
			Model:   request.Model,
			Message: domain.Message{Role: domain.RoleAssistant, Content: "ok"},
		}, nil
	}}
	registry, err := service.NewModelRegistry([]service.ModelRoute{
		{ExposedModel: "default-chat", UpstreamModel: "private-default", ProviderName: "provider", Provider: provider},
		{ExposedModel: "fast-chat", UpstreamModel: "private-fast", ProviderName: "provider", Provider: provider},
	})
	if err != nil {
		t.Fatalf("NewModelRegistry() error = %v", err)
	}
	handler := NewHandler(service.NewChatService(registry))
	router := NewRouter(handler, newAuthTestAuthenticator(t, []gatewayauth.APIKey{
		{
			ID:            "internal-client",
			KeyHash:       sha256.Sum256([]byte(enabledGatewayKey)),
			Enabled:       true,
			AllowedModels: []string{"*"},
		},
	}))
	for _, model := range []string{"default-chat", "fast-chat"} {
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+enabledGatewayKey)
		recorder := httptest.NewRecorder()

		router.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("model %q status = %d, body = %s", model, recorder.Code, recorder.Body.String())
		}
	}
	wantUpstreamModels := []string{"private-default", "private-fast"}
	for index, want := range wantUpstreamModels {
		if calledUpstreamModels[index] != want {
			t.Fatalf("upstream model %d = %q, want %q", index, calledUpstreamModels[index], want)
		}
	}
}

func TestModelsEndpointRequiresAuthentication(t *testing.T) {
	handler := NewHandler(newTestChatService(t, handlerFakeProvider{}))
	router := NewRouter(handler, newAuthTestAuthenticator(t, []gatewayauth.APIKey{
		{
			ID:            "client",
			KeyHash:       sha256.Sum256([]byte(enabledGatewayKey)),
			Enabled:       true,
			AllowedModels: []string{"*"},
		},
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	assertAuthenticationError(t, recorder)
}

func TestSSEAuthenticationFailureDoesNotCommitSSEHeaders(t *testing.T) {
	streamCalled := false
	handler := newQuietHandler(t, handlerFakeProvider{stream: func(
		context.Context,
		domain.ChatRequest,
	) (domain.ChatStream, error) {
		streamCalled = true
		return nil, nil
	}})
	router := NewRouter(handler, newAuthTestAuthenticator(t, []gatewayauth.APIKey{
		{
			ID:            "client",
			KeyHash:       sha256.Sum256([]byte(enabledGatewayKey)),
			Enabled:       true,
			AllowedModels: []string{"*"},
		},
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamRequestJSON()))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer invalid")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	assertAuthenticationError(t, recorder)
	if strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatal("authentication failure committed SSE headers")
	}
	if streamCalled {
		t.Fatal("authentication failure started provider stream")
	}
}

func newAuthTestAuthenticator(t testing.TB, apiKeys []gatewayauth.APIKey) gatewayauth.APIKeyAuthenticator {
	t.Helper()
	authenticator, err := gatewayauth.NewMemoryAuthenticator(apiKeys)
	if err != nil {
		t.Fatalf("NewMemoryAuthenticator() error = %v", err)
	}
	return authenticator
}

func assertAuthenticationError(t testing.TB, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q", recorder.Header().Get("WWW-Authenticate"))
	}
	var response openaiapi.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode authentication error: %v", err)
	}
	if response.Error.Message != "Invalid API key." || response.Error.Type != "authentication_error" ||
		response.Error.Param != nil || response.Error.Code != "invalid_api_key" {
		t.Fatalf("error = %#v", response.Error)
	}
}
