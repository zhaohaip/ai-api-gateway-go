package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/zhaohaip/ai-api-gateway-go/internal/concurrencylimit"
	"github.com/zhaohaip/ai-api-gateway-go/internal/ratelimit"
)

type recordingConcurrencyController struct {
	mu    sync.Mutex
	err   error
	keys  []string
	lease concurrencylimit.Lease
}

func (c *recordingConcurrencyController) Acquire(keyID string) (concurrencylimit.Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = append(c.keys, keyID)
	if c.err != nil {
		return nil, c.err
	}
	if c.lease != nil {
		return c.lease, nil
	}
	return unlimitedConcurrencyLease{}, nil
}

func (c *recordingConcurrencyController) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.keys...)
}

func TestConcurrencyLimitReturnsDistinctOpenAIErrorAndSafeLog(t *testing.T) {
	const rawKey = "sk-gw-concurrency-secret"
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
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
			controller := &recordingConcurrencyController{err: &concurrencylimit.Error{
				Scope: concurrencylimit.ScopeAPIKey,
			}}
			handler := NewHandlerWithRequestControls(
				newTestChatService(t, provider),
				unlimitedLimiter{},
				controller,
			)
			var logOutput bytes.Buffer
			handler.logger = slog.New(slog.NewTextHandler(&logOutput, nil))
			router := NewRouter(handler, newAuthTestAuthenticator(t, []gatewayauth.APIKey{
				{
					ID:            "concurrency-client",
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
			request.Header.Set("X-Request-ID", "request-concurrency-test")
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if recorder.Header().Get("Retry-After") != "" {
				t.Fatalf("Retry-After = %q, want empty", recorder.Header().Get("Retry-After"))
			}
			if strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
				t.Fatal("concurrency rejection committed SSE headers")
			}
			var response openaiapi.ErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Error.Message != "Concurrency limit exceeded." ||
				response.Error.Type != "rate_limit_error" || response.Error.Param != nil ||
				response.Error.Code != "concurrency_limit_exceeded" {
				t.Fatalf("error = %#v", response.Error)
			}
			if providerCalled {
				t.Fatal("concurrency rejection called provider")
			}
			if calls := controller.Calls(); len(calls) != 1 || calls[0] != "concurrency-client" {
				t.Fatalf("controller calls = %#v", calls)
			}
			logText := logOutput.String()
			for _, field := range []string{
				"request_id=request-concurrency-test",
				"key_id=concurrency-client",
				"model=default-chat",
				"stream=" + map[bool]string{false: "false", true: "true"}[stream],
				"limit_scope=api_key",
			} {
				if !strings.Contains(logText, field) {
					t.Fatalf("log = %q, want field %q", logText, field)
				}
			}
			if strings.Contains(logText, rawKey) || strings.Contains(recorder.Body.String(), rawKey) {
				t.Fatal("concurrency error or log exposed API key")
			}
		})
	}
}

func TestFrequencyLimitRejectionDoesNotAcquireConcurrencySlot(t *testing.T) {
	rateLimiter := &recordingLimiter{err: &ratelimit.Error{
		Scope:      ratelimit.ScopeGlobal,
		RetryAfter: time.Second,
	}}
	controller := &recordingConcurrencyController{}
	handler := NewHandlerWithRequestControls(newTestChatService(t, handlerFakeProvider{}), rateLimiter, controller)

	recorder := performRequest(newTestRouter(t, handler), context.Background(), validRequestJSON(), "application/json")

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if calls := controller.Calls(); len(calls) != 0 {
		t.Fatalf("frequency rejection acquired concurrency slot: %#v", calls)
	}
}

func TestNonStreamingRequestReleasesConcurrencySlot(t *testing.T) {
	tests := []struct {
		name        string
		ctx         func() context.Context
		providerErr error
		wantStatus  int
	}{
		{name: "success", ctx: context.Background, wantStatus: http.StatusOK},
		{name: "provider error", ctx: context.Background, providerErr: domain.NewError(
			domain.ErrorKindUpstream,
			"upstream unavailable",
			"",
			"upstream_error",
			nil,
		), wantStatus: http.StatusBadGateway},
		{name: "canceled", ctx: canceledContext, providerErr: context.Canceled, wantStatus: statusClientClosedRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := newAPIConcurrencyController(t, 1, 1)
			provider := handlerFakeProvider{generate: func(
				_ context.Context,
				request domain.ChatRequest,
			) (domain.ChatResponse, error) {
				if test.providerErr != nil {
					return domain.ChatResponse{}, test.providerErr
				}
				return domain.ChatResponse{
					Model:   request.Model,
					Message: domain.Message{Role: domain.RoleAssistant, Content: "ok"},
				}, nil
			}}
			handler := NewHandlerWithRequestControls(
				newTestChatService(t, provider),
				unlimitedLimiter{},
				controller,
			)

			recorder := performRequest(newTestRouter(t, handler), test.ctx(), validRequestJSON(), "application/json")

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			lease, err := controller.Acquire("test-client")
			if err != nil {
				t.Fatalf("slot was not released: %v", err)
			}
			lease.Release()
		})
	}
}

func TestSSEHoldsConcurrencySlotUntilStreamEnds(t *testing.T) {
	controller := newAPIConcurrencyController(t, 1, 0)
	started := make(chan struct{})
	continueStream := make(chan struct{})
	stream := &blockingChatStream{
		started:        started,
		continueStream: continueStream,
	}
	provider := handlerFakeProvider{stream: func(
		context.Context,
		domain.ChatRequest,
	) (domain.ChatStream, error) {
		return stream, nil
	}}
	handler := NewHandlerWithRequestControls(
		newTestChatService(t, provider),
		unlimitedLimiter{},
		controller,
	)
	handler.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	router := newTestRouter(t, handler)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- performRequest(router, context.Background(), streamRequestJSON(), "application/json")
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	_, err := controller.Acquire("another-client")
	assertAPIConcurrencyError(t, err, concurrencylimit.ScopeGlobal)
	close(continueStream)

	select {
	case recorder := <-done:
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not finish")
	}
	lease, err := controller.Acquire("another-client")
	if err != nil {
		t.Fatalf("slot was not released after stream end: %v", err)
	}
	lease.Release()
	if stream.CloseCount() != 1 {
		t.Fatalf("underlying Close() count = %d, want 1", stream.CloseCount())
	}
}

func TestSSECreationAndInitializationFailuresReleaseConcurrencySlot(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*Handler)
		provider   handlerFakeProvider
		wantStatus int
	}{
		{
			name: "stream creation error",
			provider: handlerFakeProvider{stream: func(
				context.Context,
				domain.ChatRequest,
			) (domain.ChatStream, error) {
				return nil, domain.NewError(domain.ErrorKindUpstream, "failed", "", "upstream_error", nil)
			}},
			wantStatus: http.StatusBadGateway,
		},
		{
			name: "completion ID initialization error",
			prepare: func(handler *Handler) {
				handler.newID = func() (string, error) { return "", errors.New("ID failed") }
			},
			provider: handlerFakeProvider{stream: func(
				context.Context,
				domain.ChatRequest,
			) (domain.ChatStream, error) {
				t.Fatal("initialization failure created provider stream")
				return nil, nil
			}},
			wantStatus: http.StatusInternalServerError,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := newAPIConcurrencyController(t, 1, 1)
			handler := NewHandlerWithRequestControls(
				newTestChatService(t, test.provider),
				unlimitedLimiter{},
				controller,
			)
			if test.prepare != nil {
				test.prepare(handler)
			}

			recorder := performRequest(newTestRouter(t, handler), context.Background(), streamRequestJSON(), "application/json")

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			lease, err := controller.Acquire("test-client")
			if err != nil {
				t.Fatalf("slot was not released: %v", err)
			}
			lease.Release()
		})
	}
}

func TestSSEFailureAfterStreamCreationClosesStreamAndReleasesSlot(t *testing.T) {
	controller := newAPIConcurrencyController(t, 1, 1)
	stream := &handlerFakeStream{results: []streamResult{{chunk: contentChunk("partial")}}}
	provider := handlerFakeProvider{stream: func(
		context.Context,
		domain.ChatRequest,
	) (domain.ChatStream, error) {
		return stream, nil
	}}
	handler := NewHandlerWithRequestControls(
		newTestChatService(t, provider),
		unlimitedLimiter{},
		controller,
	)
	handler.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	writer := newFailingStreamWriter()

	performRequestWithWriter(
		newTestRouter(t, handler),
		writer,
		context.Background(),
		streamRequestJSON(),
		"application/json",
	)

	if stream.closeCount != 1 {
		t.Fatalf("underlying Close() count = %d, want 1", stream.closeCount)
	}
	lease, err := controller.Acquire("test-client")
	if err != nil {
		t.Fatalf("slot was not released after write failure: %v", err)
	}
	lease.Release()
}

type blockingChatStream struct {
	started        chan struct{}
	continueStream chan struct{}
	startOnce      sync.Once
	mu             sync.Mutex
	recvCount      int
	closeCount     int
}

type failingStreamWriter struct {
	header http.Header
	status int
}

func newFailingStreamWriter() *failingStreamWriter {
	return &failingStreamWriter{header: make(http.Header)}
}

func (w *failingStreamWriter) Header() http.Header {
	return w.header
}

func (w *failingStreamWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *failingStreamWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func (w *failingStreamWriter) Flush() {}

func (s *blockingChatStream) Recv() (domain.ChatChunk, error) {
	s.mu.Lock()
	call := s.recvCount
	s.recvCount++
	s.mu.Unlock()
	if call == 0 {
		s.startOnce.Do(func() { close(s.started) })
		<-s.continueStream
		finishReason := "stop"
		return domain.ChatChunk{FinishReason: &finishReason}, nil
	}
	return domain.ChatChunk{}, io.EOF
}

func (s *blockingChatStream) Close() error {
	s.mu.Lock()
	s.closeCount++
	s.mu.Unlock()
	return nil
}

func (s *blockingChatStream) CloseCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCount
}

func newAPIConcurrencyController(t *testing.T, globalMax, apiKeyMax int) *concurrencylimit.MemoryController {
	t.Helper()
	controller, err := concurrencylimit.NewMemoryController(globalMax, apiKeyMax)
	if err != nil {
		t.Fatalf("NewMemoryController() error = %v", err)
	}
	return controller
}

func assertAPIConcurrencyError(t *testing.T, err error, scope concurrencylimit.Scope) {
	t.Helper()
	var concurrencyErr *concurrencylimit.Error
	if !errors.As(err, &concurrencyErr) || concurrencyErr.Scope != scope {
		t.Fatalf("error = %v, want concurrency scope %q", err, scope)
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
