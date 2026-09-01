package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
	"github.com/zhaohaip/ai-api-gateway-go/internal/concurrencylimit"
)

type controlledTimerFactory struct {
	mu     sync.Mutex
	timers []*controlledTimer
	ready  chan struct{}
}

func newControlledTimerFactory() *controlledTimerFactory {
	return &controlledTimerFactory{ready: make(chan struct{}, 16)}
}

func (f *controlledTimerFactory) New(duration time.Duration, callback func()) timeoutTimer {
	timer := &controlledTimer{duration: duration, callback: callback, active: true}
	f.mu.Lock()
	f.timers = append(f.timers, timer)
	f.mu.Unlock()
	select {
	case f.ready <- struct{}{}:
	default:
	}
	return timer
}

func (f *controlledTimerFactory) Wait(t *testing.T, index int) *controlledTimer {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		f.mu.Lock()
		if len(f.timers) > index {
			timer := f.timers[index]
			f.mu.Unlock()
			return timer
		}
		f.mu.Unlock()
		select {
		case <-f.ready:
		case <-deadline.C:
			t.Fatalf("timer %d was not created", index)
		}
	}
}

type controlledTimer struct {
	mu         sync.Mutex
	duration   time.Duration
	callback   func()
	active     bool
	resetCount int
}

func (t *controlledTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := t.active
	t.active = false
	return wasActive
}

func (t *controlledTimer) Reset(duration time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := t.active
	t.duration = duration
	t.active = true
	t.resetCount++
	return wasActive
}

func (t *controlledTimer) Trigger() bool {
	t.mu.Lock()
	if !t.active {
		t.mu.Unlock()
		return false
	}
	t.active = false
	callback := t.callback
	t.mu.Unlock()
	callback()
	return true
}

func (t *controlledTimer) ResetCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resetCount
}

type timeoutBlockingStream struct {
	ctx        context.Context
	chunks     []domain.ChatChunk
	mu         sync.Mutex
	next       int
	closeCount int
	blocked    chan struct{}
	blockOnce  sync.Once
}

func newTimeoutBlockingStream(ctx context.Context, chunks ...domain.ChatChunk) *timeoutBlockingStream {
	return &timeoutBlockingStream{ctx: ctx, chunks: chunks, blocked: make(chan struct{})}
}

func (s *timeoutBlockingStream) Recv() (domain.ChatChunk, error) {
	s.mu.Lock()
	if s.next < len(s.chunks) {
		chunk := s.chunks[s.next]
		s.next++
		s.mu.Unlock()
		return chunk, nil
	}
	s.mu.Unlock()
	s.blockOnce.Do(func() { close(s.blocked) })
	<-s.ctx.Done()
	return domain.ChatChunk{}, s.ctx.Err()
}

func (s *timeoutBlockingStream) Close() error {
	s.mu.Lock()
	s.closeCount++
	s.mu.Unlock()
	return nil
}

func (s *timeoutBlockingStream) CloseCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCount
}

func TestNonStreamTimeoutControlsProviderContextAndReleasesConcurrency(t *testing.T) {
	factory := newControlledTimerFactory()
	providerContext := make(chan context.Context, 1)
	provider := handlerFakeProvider{generate: func(
		ctx context.Context,
		_ domain.ChatRequest,
	) (domain.ChatResponse, error) {
		providerContext <- ctx
		<-ctx.Done()
		return domain.ChatResponse{}, ctx.Err()
	}}
	controller := newAPIConcurrencyController(t, 1, 1)
	handler := NewHandlerWithTimeouts(
		newTestChatService(t, provider),
		unlimitedLimiter{},
		controller,
		Timeouts{NonStream: time.Minute},
	)
	handler.newTimer = factory.New
	var logs bytes.Buffer
	handler.logger = slog.New(slog.NewTextHandler(&logs, nil))
	router := newTestRouter(t, handler)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- performRequest(router, context.Background(), validRequestJSON(), "application/json")
	}()

	ctx := <-providerContext
	timer := factory.Wait(t, 0)
	if !timer.Trigger() {
		t.Fatal("non-stream timeout timer was inactive")
	}
	recorder := waitRecorder(t, result)
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if response := decodeErrorResponse(t, recorder); response.Error.Code != "upstream_timeout" {
		t.Fatalf("error = %#v", response.Error)
	}
	if timeout := timeoutFromContext(ctx); timeout == nil || timeout.typeName != timeoutTypeNonStream {
		t.Fatalf("provider context cause = %v", context.Cause(ctx))
	}
	lease, err := controller.Acquire("test-client")
	if err != nil {
		t.Fatalf("concurrency slot was not released: %v", err)
	}
	lease.Release()
	for _, field := range []string{
		"key_id=test-client",
		"model=default-chat",
		"provider=test",
		"stream=false",
		"timeout_type=non_stream",
		"duration=1m0s",
		"response_committed=false",
	} {
		if !strings.Contains(logs.String(), field) {
			t.Fatalf("log = %q, want %q", logs.String(), field)
		}
	}
}

func TestNonStreamCompletesBeforeTimeout(t *testing.T) {
	factory := newControlledTimerFactory()
	handler := NewHandlerWithTimeouts(
		newTestChatService(t, handlerFakeProvider{generate: func(
			_ context.Context,
			request domain.ChatRequest,
		) (domain.ChatResponse, error) {
			return domain.ChatResponse{
				Model:   request.Model,
				Message: domain.Message{Role: domain.RoleAssistant, Content: "ok"},
			}, nil
		}}),
		unlimitedLimiter{},
		unlimitedConcurrencyController{},
		Timeouts{NonStream: time.Minute},
	)
	handler.newTimer = factory.New
	recorder := performRequest(newTestRouter(t, handler), context.Background(), validRequestJSON(), "application/json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if factory.Wait(t, 0).Trigger() {
		t.Fatal("completed request left its timeout timer active")
	}
}

func TestSSEFirstChunkTimeoutReturnsJSONBeforeCommit(t *testing.T) {
	factory := newControlledTimerFactory()
	streamCreated := make(chan *timeoutBlockingStream, 1)
	handler, controller := newTimeoutStreamHandler(t, factory, StreamTimeouts{FirstChunk: time.Minute}, func(
		ctx context.Context,
	) domain.ChatStream {
		stream := newTimeoutBlockingStream(ctx, domain.ChatChunk{})
		streamCreated <- stream
		return stream
	})
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- performRequest(newTestRouter(t, handler), context.Background(), streamRequestJSON(), "application/json")
	}()
	stream := <-streamCreated
	waitSignal(t, stream.blocked, "stream did not wait for its first valid chunk")
	if !factory.Wait(t, 0).Trigger() {
		t.Fatal("first-chunk timeout timer was inactive")
	}
	recorder := waitRecorder(t, result)
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatal("first-chunk timeout committed SSE headers")
	}
	if response := decodeErrorResponse(t, recorder); response.Error.Code != "stream_first_chunk_timeout" {
		t.Fatalf("error = %#v", response.Error)
	}
	assertTimedOutStreamCleanup(t, stream, controller)
}

func TestSSEPostCommitTimeoutsTerminateWithoutJSONOrDone(t *testing.T) {
	tests := []struct {
		name     string
		timeouts StreamTimeouts
		code     timeoutType
	}{
		{name: "idle", timeouts: StreamTimeouts{Idle: time.Minute}, code: timeoutTypeStreamIdle},
		{name: "total", timeouts: StreamTimeouts{Total: time.Minute}, code: timeoutTypeStreamTotal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factory := newControlledTimerFactory()
			streamCreated := make(chan *timeoutBlockingStream, 1)
			handler, controller := newTimeoutStreamHandler(t, factory, test.timeouts, func(
				ctx context.Context,
			) domain.ChatStream {
				stream := newTimeoutBlockingStream(ctx, contentChunk("first"))
				streamCreated <- stream
				return stream
			})
			var logs bytes.Buffer
			handler.logger = slog.New(slog.NewTextHandler(&logs, nil))
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				result <- performRequest(newTestRouter(t, handler), context.Background(), streamRequestJSON(), "application/json")
			}()
			stream := <-streamCreated
			waitSignal(t, stream.blocked, "stream did not enter its post-chunk wait")
			if !factory.Wait(t, 0).Trigger() {
				t.Fatal("stream timeout timer was inactive")
			}
			recorder := waitRecorder(t, result)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			assertSSEHeaders(t, recorder.Header())
			body := recorder.Body.String()
			if strings.Contains(body, doneEvent) || strings.Contains(body, `"error"`) {
				t.Fatalf("post-commit timeout appended terminal data: %s", body)
			}
			if events := parseSSEEvents(t, body); len(events) != 1 {
				t.Fatalf("events = %#v", events)
			}
			assertTimedOutStreamCleanup(t, stream, controller)
			for _, field := range []string{
				"timeout_type=" + string(test.code),
				"response_committed=true",
			} {
				if !strings.Contains(logs.String(), field) {
					t.Fatalf("log = %q, want %q", logs.String(), field)
				}
			}
		})
	}
}

func TestSSEValidOutputResetsIdleTimeoutAndNormalCompletionSendsDone(t *testing.T) {
	factory := newControlledTimerFactory()
	finishReason := "stop"
	stream := &handlerFakeStream{results: []streamResult{
		{chunk: contentChunk("one")},
		{chunk: contentChunk("two")},
		{chunk: domain.ChatChunk{FinishReason: &finishReason}},
	}}
	handler := NewHandlerWithTimeouts(
		newTestChatService(t, handlerFakeProvider{stream: func(
			context.Context,
			domain.ChatRequest,
		) (domain.ChatStream, error) {
			return stream, nil
		}}),
		unlimitedLimiter{},
		unlimitedConcurrencyController{},
		Timeouts{Stream: StreamTimeouts{Idle: time.Minute}},
	)
	handler.newTimer = factory.New
	handler.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	recorder := performRequest(newTestRouter(t, handler), context.Background(), streamRequestJSON(), "application/json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if count := strings.Count(recorder.Body.String(), "data: "+doneEvent); count != 1 {
		t.Fatalf("DONE count = %d, body = %s", count, recorder.Body.String())
	}
	timer := factory.Wait(t, 0)
	if timer.ResetCount() != 2 {
		t.Fatalf("idle timer reset count = %d, want 2", timer.ResetCount())
	}
	if timer.Trigger() {
		t.Fatal("normal stream completion left idle timer active")
	}
}

func newTimeoutStreamHandler(
	t *testing.T,
	factory *controlledTimerFactory,
	timeouts StreamTimeouts,
	newStream func(context.Context) domain.ChatStream,
) (*Handler, *concurrencylimit.MemoryController) {
	t.Helper()
	controller := newAPIConcurrencyController(t, 1, 1)
	handler := NewHandlerWithTimeouts(
		newTestChatService(t, handlerFakeProvider{stream: func(
			ctx context.Context,
			_ domain.ChatRequest,
		) (domain.ChatStream, error) {
			return newStream(ctx), nil
		}}),
		unlimitedLimiter{},
		controller,
		Timeouts{Stream: timeouts},
	)
	handler.newTimer = factory.New
	handler.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return handler, controller
}

func assertTimedOutStreamCleanup(
	t *testing.T,
	stream *timeoutBlockingStream,
	controller *concurrencylimit.MemoryController,
) {
	t.Helper()
	if stream.CloseCount() != 1 {
		t.Fatalf("stream Close() count = %d, want 1", stream.CloseCount())
	}
	lease, err := controller.Acquire("test-client")
	if err != nil {
		t.Fatalf("concurrency slot was not released: %v", err)
	}
	lease.Release()
}

func waitRecorder(t *testing.T, result <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case recorder := <-result:
		return recorder
	case <-time.After(time.Second):
		t.Fatal("HTTP request did not finish")
		return nil
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}
