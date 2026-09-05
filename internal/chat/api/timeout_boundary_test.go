package api

import (
	"context"
	"errors"
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

// Advance fires only timers due in this interval. Tests advance while Recv or
// Provider is at a barrier; callbacks are synchronous, never real-time sleeps.
func (f *controlledTimerFactory) Advance(delta time.Duration) {
	f.mu.Lock()
	timers := append([]*controlledTimer(nil), f.timers...)
	f.mu.Unlock()
	for _, timer := range timers {
		timer.mu.Lock()
		fire := false
		if timer.active {
			timer.remaining -= delta
			fire = timer.remaining <= 0
			if fire {
				timer.active = false
			}
		}
		callback := timer.callback
		timer.mu.Unlock()
		if fire {
			callback()
		}
	}
}

func assertTimersStopped(t *testing.T, f *controlledTimerFactory) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, timer := range f.timers {
		timer.mu.Lock()
		active := timer.active
		timer.mu.Unlock()
		if active {
			t.Errorf("timer %d remains active", i)
		}
	}
}

// A Recv-entry acknowledgement establishes that all preceding writes, flushes
// and timer changes finished. While awaiting a result, the writer is untouched.
type boundaryStream struct {
	ctx     context.Context
	entered chan struct{}
	results chan streamResult
	mu      sync.Mutex
	closes  int
}

func (s *boundaryStream) Recv() (domain.ChatChunk, error) {
	select {
	case s.entered <- struct{}{}:
	case <-s.ctx.Done():
		return domain.ChatChunk{}, s.ctx.Err()
	}
	select {
	case result := <-s.results:
		return result.chunk, result.err
	case <-s.ctx.Done():
		return domain.ChatChunk{}, s.ctx.Err()
	}
}
func (s *boundaryStream) Close() error { s.mu.Lock(); s.closes++; s.mu.Unlock(); return nil }

// The production Lease contract explicitly permits repeated Release calls.
// Count effective releases, and forward EVERY call to the real lease so that
// the spy cannot hide a broken production idempotency guarantee.
type boundaryLease struct {
	concurrencylimit.Lease
	effective countingLease
}

func (l *boundaryLease) Release() { l.Lease.Release(); l.effective.Release() }

type boundaryController struct {
	*concurrencylimit.MemoryController
	leases []*boundaryLease // read only after the HTTP goroutine exits
}

func (c *boundaryController) Acquire(key string) (concurrencylimit.Lease, error) {
	lease, err := c.MemoryController.Acquire(key)
	if err != nil {
		return nil, err
	}
	spy := &boundaryLease{Lease: lease}
	c.leases = append(c.leases, spy)
	return spy, nil
}

// Exercise real subsequent HTTP admission, both available and occupied, for
// global and per-key limits. Over-admission would expose a negative count.
func assertBoundarySlots(t *testing.T, c *concurrencylimit.MemoryController, previous ...concurrencylimit.Lease) {
	t.Helper()
	handler := NewHandlerWithRequestControls(newTestChatService(t, handlerFakeProvider{}), unlimitedLimiter{}, c)
	handler.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	router := newTestRouter(t, handler)
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+testGatewayAPIKey)
		r := httptest.NewRecorder()
		router.ServeHTTP(r, req)
		return r
	}
	for i := 0; i < 2; i++ {
		lease, err := c.Acquire("test-client")
		if err != nil {
			t.Fatalf("subsequent lease: %v", err)
		}
		// A stale release must not free the newly acquired request's slot.
		for _, old := range previous {
			old.Release()
		}
		for _, key := range []string{"test-client", "another-client"} {
			extra, err := c.Acquire(key)
			if err == nil {
				extra.Release()
				t.Error("occupied controller admitted extra lease")
			}
		}
		lease.Release()
		if r := request(); r.Code != http.StatusOK {
			t.Fatalf("subsequent HTTP request: %d %s", r.Code, r.Body.String())
		}
	}
}

type boundaryRun struct {
	stream     *boundaryStream
	clock      *controlledTimerFactory
	controller *boundaryController
	writer     *flushRecorder
	done       chan struct{}
	cancel     context.CancelCauseFunc
}

func startBoundaryStream(t *testing.T, timeouts StreamTimeouts) *boundaryRun {
	t.Helper()
	return startBoundaryStreamWithWriter(t, timeouts, func(w *flushRecorder) http.ResponseWriter { return w })
}

func startBoundaryStreamWithWriter(t *testing.T, timeouts StreamTimeouts, wrap func(*flushRecorder) http.ResponseWriter) *boundaryRun {
	t.Helper()
	parent, cancel := context.WithCancelCause(context.Background())
	run := &boundaryRun{clock: newControlledTimerFactory(), controller: &boundaryController{MemoryController: newAPIConcurrencyController(t, 1, 1)}, writer: &flushRecorder{ResponseRecorder: httptest.NewRecorder()}, done: make(chan struct{}), cancel: cancel}
	created := make(chan *boundaryStream, 1)
	h := NewHandlerWithTimeouts(newTestChatService(t, handlerFakeProvider{stream: func(ctx context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
		s := &boundaryStream{ctx: ctx, entered: make(chan struct{}, 1), results: make(chan streamResult, 1)}
		created <- s
		return s, nil
	}}), unlimitedLimiter{}, run.controller, Timeouts{Stream: timeouts})
	h.newTimer = run.clock.New
	h.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	router := newTestRouter(t, h)
	writer := wrap(run.writer)
	t.Cleanup(func() { cancel(context.Canceled); waitSignal(t, run.done, "request cleanup did not finish") })
	go func() {
		defer close(run.done)
		performRequestWithWriter(router, writer, parent, streamRequestJSON(), "application/json")
	}()
	select {
	case run.stream = <-created:
	case <-time.After(time.Second):
		t.Fatal("provider not entered")
	}
	run.waitRecv(t)
	return run
}
func (r *boundaryRun) waitRecv(t *testing.T) {
	t.Helper()
	waitSignal(t, r.stream.entered, "Recv barrier not reached")
}
func (r *boundaryRun) send(t *testing.T, chunk domain.ChatChunk) {
	t.Helper()
	r.stream.results <- streamResult{chunk: chunk}
	r.waitRecv(t)
}
func (r *boundaryRun) finish(t *testing.T) {
	t.Helper()
	r.stream.results <- streamResult{err: io.EOF}
	r.waitDone(t)
}
func (r *boundaryRun) waitDone(t *testing.T) {
	t.Helper()
	waitSignal(t, r.done, "stream did not finish")
	r.stream.mu.Lock()
	closes := r.stream.closes
	r.stream.mu.Unlock()
	if closes != 1 {
		t.Errorf("Close count = %d", closes)
	}
	if len(r.controller.leases) != 1 || r.controller.leases[0].effective.Count() != 1 {
		t.Error("lease did not release exactly once")
	}
	assertTimersStopped(t, r.clock)
	if r.writer.Flushed {
		assertSSEHeaders(t, r.writer.Header())
		if r.writer.Code != http.StatusOK {
			t.Errorf("committed SSE status = %d", r.writer.Code)
		}
	}
	assertBoundarySlots(t, r.controller.MemoryController, r.controller.leases[0])
}
func assertBoundaryCause(t *testing.T, ctx context.Context, kind timeoutType) {
	t.Helper()
	cause := timeoutFromContext(ctx)
	if cause == nil || cause.typeName != kind {
		t.Fatalf("cause = %v, want %s", context.Cause(ctx), kind)
	}
}
func assertBoundaryNoTerminal(t *testing.T, r *flushRecorder) {
	t.Helper()
	body := r.Body.String()
	if strings.Contains(body, doneEvent) || strings.Contains(body, `"error"`) || strings.Contains(body, `"finish_reason":"`) {
		t.Fatalf("unexpected terminal output: %s", body)
	}
}

func TestSSEFirstTimeoutIncludesBlockedProviderStreamCreation(t *testing.T) {
	clock := newControlledTimerFactory()
	entered := make(chan context.Context, 1)
	exited := make(chan struct{})
	parent, cancel := context.WithCancel(context.Background())
	controller := newAPIConcurrencyController(t, 1, 1)
	h := NewHandlerWithTimeouts(newTestChatService(t, handlerFakeProvider{stream: func(ctx context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
		defer close(exited)
		// Capture timer existence at the actual call boundary, before waiting.
		clock.mu.Lock()
		count := len(clock.timers)
		clock.mu.Unlock()
		if count != 2 {
			t.Errorf("timers at Provider.Stream entry = %d, want total and first", count)
		}
		entered <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	}}), unlimitedLimiter{}, controller, Timeouts{Stream: StreamTimeouts{FirstChunk: time.Second, Total: time.Minute}})
	h.newTimer = clock.New
	h.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	router := newTestRouter(t, h)
	result := make(chan *httptest.ResponseRecorder, 1)
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); waitSignal(t, done, "blocked provider cleanup") })
	go func() {
		defer close(done)
		result <- performRequest(router, parent, streamRequestJSON(), "application/json")
	}()
	var ctx context.Context
	select {
	case ctx = <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider not entered")
	}
	clock.Advance(time.Second)
	r := waitRecorder(t, result)
	waitSignal(t, exited, "provider did not exit")
	assertBoundaryCause(t, ctx, timeoutTypeStreamFirstChunk)
	if r.Code != 504 || decodeErrorResponse(t, r).Error.Code != "stream_first_chunk_timeout" {
		t.Fatalf("response = %d %s", r.Code, r.Body.String())
	}
	if strings.Contains(r.Header().Get("Content-Type"), "text/event-stream") || r.Flushed || strings.Contains(r.Body.String(), "data:") || strings.Contains(r.Body.String(), doneEvent) {
		t.Fatalf("committed SSE: %v %s", r.Header(), r.Body.String())
	}
	assertTimersStopped(t, clock)
	assertBoundarySlots(t, controller)
}

func TestSSEFilteredChunksKeepOriginalIdleDeadline(t *testing.T) {
	r := startBoundaryStream(t, StreamTimeouts{FirstChunk: time.Second, Idle: 30 * time.Second, Total: 5 * time.Minute})
	r.send(t, contentChunk("first"))
	assertSSEHeaders(t, r.writer.Header())
	if r.writer.flushCount != 1 || !strings.Contains(r.writer.Body.String(), `"content":"first"`) {
		t.Fatal("first output not flushed")
	}
	original := r.writer.Body.String()
	idle := r.clock.Wait(t, 2)
	for _, chunk := range []domain.ChatChunk{{}, roleChunk(), {}} {
		r.clock.Advance(9 * time.Second)
		r.send(t, chunk)
		if r.writer.Body.String() != original || r.writer.flushCount != 1 || idle.ResetCount() != 0 {
			t.Fatal("filtered chunk wrote output or reset idle")
		}
	}
	r.clock.Advance(3 * time.Second)
	r.waitDone(t)
	assertBoundaryCause(t, r.stream.ctx, timeoutTypeStreamIdle)
	assertBoundaryNoTerminal(t, r.writer)
}

func TestSSEOutputKindsResetIdleDeadline(t *testing.T) {
	for _, test := range []struct {
		name   string
		chunk  domain.ChatChunk
		marker string
	}{
		{"content", contentChunk("second"), `"content":"second"`},
		{"usage", domain.ChatChunk{Usage: &domain.Usage{TotalTokens: 3}}, `"total_tokens":3`},
		{"finish", domain.ChatChunk{FinishReason: stringPointer("stop")}, `"finish_reason":"stop"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := startBoundaryStream(t, StreamTimeouts{Idle: 30 * time.Second})
			r.send(t, contentChunk("first"))
			r.clock.Advance(20 * time.Second)
			r.send(t, test.chunk)
			if r.clock.Wait(t, 0).ResetCount() != 1 || r.writer.flushCount != 2 || !strings.Contains(r.writer.Body.String(), test.marker) {
				t.Fatal("valid chunk did not write and reset timer")
			}
			r.clock.Advance(10 * time.Second)
			if context.Cause(r.stream.ctx) != nil {
				t.Fatal("old deadline still canceled stream")
			}
			if test.name != "finish" {
				r.send(t, domain.ChatChunk{FinishReason: stringPointer("stop")})
			}
			r.finish(t)
			if strings.Count(r.writer.Body.String(), doneEvent) != 1 {
				t.Fatalf("normal protocol missing DONE: %s", r.writer.Body.String())
			}
		})
	}
}

func TestDisabledNonStreamTimeoutAtRuntime(t *testing.T) {
	clock := newControlledTimerFactory()
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	parent, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	result := make(chan *httptest.ResponseRecorder, 1)
	controller := newAPIConcurrencyController(t, 1, 1)
	h := NewHandlerWithTimeouts(newTestChatService(t, handlerFakeProvider{generate: func(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
		entered <- ctx
		select {
		case <-release:
			return domain.ChatResponse{Model: req.Model, Message: domain.Message{Role: domain.RoleAssistant, Content: "released"}}, nil
		case <-ctx.Done():
			return domain.ChatResponse{}, ctx.Err()
		}
	}}), unlimitedLimiter{}, controller, Timeouts{NonStream: 0})
	h.newTimer = clock.New
	router := newTestRouter(t, h)
	t.Cleanup(func() { cancel(); waitSignal(t, done, "non-stream cleanup") })
	go func() {
		defer close(done)
		result <- performRequest(router, parent, validRequestJSON(), "application/json")
	}()
	var ctx context.Context
	select {
	case ctx = <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider not entered")
	}
	clock.Advance(24 * time.Hour)
	if context.Cause(ctx) != nil {
		t.Fatalf("disabled timeout canceled: %v", context.Cause(ctx))
	}
	clock.mu.Lock()
	count := len(clock.timers)
	clock.mu.Unlock()
	if count != 0 {
		t.Fatalf("disabled timeout created %d timers", count)
	}
	select {
	case <-done:
		t.Fatal("blocked request finished before release")
	default:
	}
	close(release)
	r := waitRecorder(t, result)
	if r.Code != 200 || !strings.Contains(r.Body.String(), "released") {
		t.Fatalf("response = %d %s", r.Code, r.Body.String())
	}
	assertTimersStopped(t, clock)
	assertBoundarySlots(t, controller)
}

func TestDisabledStreamTimeoutsAtRuntime(t *testing.T) {
	for _, kind := range []string{"first", "idle", "total"} {
		t.Run(kind, func(t *testing.T) {
			timeouts := StreamTimeouts{}
			switch kind {
			case "first":
				timeouts.Idle = 30 * time.Second
				timeouts.Total = 5 * time.Minute
			case "idle":
				timeouts.FirstChunk = time.Second
				timeouts.Total = 48 * time.Hour
			case "total":
				timeouts.FirstChunk = time.Second
				timeouts.Idle = 30 * time.Second
			}
			r := startBoundaryStream(t, timeouts)
			if kind == "first" {
				r.clock.Advance(4 * time.Minute)
				if context.Cause(r.stream.ctx) != nil || r.writer.flushCount != 0 {
					t.Fatal("disabled first timeout terminated or committed response")
				}
			}
			r.send(t, contentChunk("first"))
			switch kind {
			case "idle":
				r.clock.Advance(24 * time.Hour)
			case "total":
				for i := 0; i < 20; i++ {
					r.clock.Advance(20 * time.Second)
					r.send(t, contentChunk("ongoing"))
				}
			}
			if context.Cause(r.stream.ctx) != nil {
				t.Fatalf("disabled %s timer canceled request: %v", kind, context.Cause(r.stream.ctx))
			}
			r.send(t, domain.ChatChunk{FinishReason: stringPointer("stop")})
			r.finish(t)
			assertSSEHeaders(t, r.writer.Header())
			if r.writer.Code != 200 || strings.Count(r.writer.Body.String(), doneEvent) != 1 || strings.Contains(r.writer.Body.String(), `"error"`) {
				t.Fatalf("response = %d %s", r.writer.Code, r.writer.Body.String())
			}
		})
	}
}

func TestDisabledFirstTimeoutKeepsIdleAndTotalEnabled(t *testing.T) {
	for _, kind := range []timeoutType{timeoutTypeStreamIdle, timeoutTypeStreamTotal} {
		t.Run(string(kind), func(t *testing.T) {
			r := startBoundaryStream(t, StreamTimeouts{FirstChunk: 0, Idle: 30 * time.Second, Total: 5 * time.Minute})
			r.clock.Advance(4 * time.Minute)
			if context.Cause(r.stream.ctx) != nil {
				t.Fatal("disabled first timeout fired")
			}
			if kind == timeoutTypeStreamIdle {
				r.send(t, contentChunk("first"))
				r.clock.Advance(30 * time.Second)
			} else {
				r.clock.Advance(time.Minute)
			}
			r.waitDone(t)
			assertBoundaryCause(t, r.stream.ctx, kind)
			if kind == timeoutTypeStreamTotal {
				if r.writer.Code != 504 || decodeErrorResponse(t, r.writer.ResponseRecorder).Error.Code != "stream_total_timeout" || r.writer.Flushed {
					t.Fatalf("response = %d %s", r.writer.Code, r.writer.Body.String())
				}
			} else {
				assertBoundaryNoTerminal(t, r.writer)
			}
		})
	}
}

// Both callbacks start at a shared barrier. Record the first cause before
// allowing a later cancellation, independently of which event wins scheduling.
func TestStreamClientCancellationRacesGatewayTimeout(t *testing.T) {
	r := startBoundaryStream(t, StreamTimeouts{Idle: time.Minute, Total: 2 * time.Minute})
	r.send(t, contentChunk("first"))
	clientErr := errors.New("client disconnected")
	idle := r.clock.Wait(t, 1)
	runBoundaryRace(t, func() { r.cancel(clientErr) }, func() { idle.Trigger() })
	first := context.Cause(r.stream.ctx)
	if first != clientErr {
		assertBoundaryCause(t, r.stream.ctx, timeoutTypeStreamIdle)
	}
	r.waitDone(t)
	r.clock.Wait(t, 0).callback() // already-dispatched callback may outlive Stop
	r.cancel(errors.New("late cancellation"))
	if context.Cause(r.stream.ctx) != first {
		t.Fatal("first cause overwritten")
	}
	assertBoundaryNoTerminal(t, r.writer)
}

func runBoundaryRace(t *testing.T, left, right func()) {
	t.Helper()
	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for _, fn := range []func(){left, right} {
		go func(fn func()) { defer wg.Done(); <-gate; fn() }(fn)
	}
	close(gate)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitSignal(t, done, "race workers did not exit")
}

func TestStreamTerminalEventsRaceTimeout(t *testing.T) {
	for _, test := range []struct {
		name     string
		timeouts StreamTimeouts
		terminal streamResult
		finished bool
	}{
		{"idle versus EOF", StreamTimeouts{Idle: time.Minute}, streamResult{err: io.EOF}, true},
		{"total versus final chunk", StreamTimeouts{Total: time.Minute}, streamResult{chunk: domain.ChatChunk{FinishReason: stringPointer("stop")}}, false},
		{"read error versus idle", StreamTimeouts{Idle: time.Minute}, streamResult{err: errors.New("upstream read failed")}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Ordered cases pin the known winner; the shared-barrier case exercises
			// overlap without imposing a scheduler-dependent winner.
			for _, order := range []string{"timeout first", "terminal first", "simultaneous"} {
				t.Run(order, func(t *testing.T) {
					r := startBoundaryStream(t, test.timeouts)
					r.send(t, contentChunk("first"))
					if test.finished {
						r.send(t, domain.ChatChunk{FinishReason: stringPointer("stop")})
					}
					timer := r.clock.Wait(t, 0)
					timeout := func() { timer.Trigger() }
					terminal := func() { r.stream.results <- test.terminal }
					switch order {
					case "timeout first":
						timeout()
						terminal()
					case "terminal first":
						terminal()
						if test.terminal.err == nil {
							r.waitRecv(t)
							r.finish(t)
						} else {
							r.waitDone(t)
						}
						timeout()
					default:
						runBoundaryRace(t, timeout, terminal)
					}
					// If the final chunk won, supply its required EOF. A buffered channel
					// permits this even when cancellation has already ended Recv.
					if test.terminal.err == nil && order == "simultaneous" {
						select {
						case <-r.stream.entered:
							r.stream.results <- streamResult{err: io.EOF}
						case <-r.done:
						case <-time.After(time.Second):
							t.Fatal("final chunk race stalled")
						}
					}
					r.waitDone(t)
					cause := context.Cause(r.stream.ctx)
					timer.callback()
					r.cancel(errors.New("late client"))
					if context.Cause(r.stream.ctx) != cause {
						t.Fatal("first context cause overwritten")
					}
					body := r.writer.Body.String()
					count := strings.Count(body, doneEvent)
					wantDone := cause == context.Canceled && test.name != "read error versus idle"
					if count > 1 || (wantDone && count != 1) || (!wantDone && count != 0) {
						t.Fatalf("cause=%v DONE count=%d body=%s", cause, count, body)
					}
					if strings.Contains(body, `"error"`) {
						t.Fatalf("post-commit JSON error: %s", body)
					}
					if order == "timeout first" && timeoutFromContext(r.stream.ctx) == nil {
						t.Fatal("timeout lost despite completing before terminal event")
					}
				})
			}
		})
	}
}

func TestConcurrencyStreamCloseRacesRecvTermination(t *testing.T) {
	for _, recvErr := range []error{io.EOF, errors.New("read failed"), context.Canceled} {
		t.Run(recvErr.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			underlying := &boundaryStream{ctx: ctx, entered: make(chan struct{}, 1), results: make(chan streamResult, 1)}
			controller := newAPIConcurrencyController(t, 1, 1)
			lease, err := controller.Acquire("test-client")
			if err != nil {
				t.Fatal(err)
			}
			spy := &boundaryLease{Lease: lease}
			stream := newConcurrencyChatStream(underlying, spy)
			done := make(chan struct{})
			t.Cleanup(func() { cancel(); waitSignal(t, done, "Recv cleanup") })
			go func() {
				defer close(done)
				_, err := stream.Recv()
				if !errors.Is(err, recvErr) {
					t.Errorf("Recv = %v, want %v", err, recvErr)
				}
			}()
			waitSignal(t, underlying.entered, "Recv did not start")
			runBoundaryRace(t, func() {
				if err := stream.Close(); err != nil {
					t.Error(err)
				}
			}, func() { underlying.results <- streamResult{err: recvErr} })
			waitSignal(t, done, "Recv did not exit")
			if err := stream.Close(); err != nil {
				t.Fatal(err)
			}
			underlying.mu.Lock()
			closes := underlying.closes
			underlying.mu.Unlock()
			if closes != 1 || spy.effective.Count() != 1 {
				t.Fatalf("closes=%d effective releases=%d", closes, spy.effective.Count())
			}
			assertBoundarySlots(t, controller, spy)
		})
	}
}

func TestSSEUsageAfterFinishIsProtocolErrorWithoutIdleResetOrDone(t *testing.T) {
	r := startBoundaryStream(t, StreamTimeouts{Idle: 30 * time.Second})
	r.send(t, contentChunk("first"))
	r.send(t, domain.ChatChunk{FinishReason: stringPointer("stop")})
	before := r.writer.Body.String()
	timer := r.clock.Wait(t, 0)
	resets := timer.ResetCount()
	r.stream.results <- streamResult{chunk: domain.ChatChunk{Usage: &domain.Usage{TotalTokens: 3}}}
	r.waitDone(t)
	if r.writer.Body.String() != before || r.writer.flushCount != 2 || timer.ResetCount() != resets {
		t.Fatalf("usage after finish wrote output or reset idle: %s", r.writer.Body.String())
	}
	if strings.Contains(before, doneEvent) || strings.Contains(before, `"error"`) {
		t.Fatalf("unexpected terminal output: %s", before)
	}
}

// Model a timer callback becoming runnable when the final network write starts,
// after EOF was checked but before [DONE] reaches the client. This makes the
// failure seen in the shared-barrier race reproducible without sleeps.
type boundaryDoneWriter struct {
	*flushRecorder
	beforeDone func()
}

func (w *boundaryDoneWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), doneEvent) {
		w.beforeDone()
	}
	return w.flushRecorder.Write(p)
}

func TestSSEIdleTimeoutRacingDoneWriteKeepsTerminalOutcomeConsistent(t *testing.T) {
	var r *boundaryRun
	var causeBeforeWrite error
	r = startBoundaryStreamWithWriter(t, StreamTimeouts{Idle: time.Minute}, func(w *flushRecorder) http.ResponseWriter {
		return &boundaryDoneWriter{flushRecorder: w, beforeDone: func() {
			// No test goroutine remains blocked in this hook, even on failure.
			r.clock.Wait(t, 0).Trigger()
			causeBeforeWrite = context.Cause(r.stream.ctx)
		}}
	})
	r.send(t, contentChunk("first"))
	r.send(t, domain.ChatChunk{FinishReason: stringPointer("stop")})
	r.finish(t)
	// A design that atomically finalizes normal completion before writing DONE
	// may legitimately stop the timer first. Neither event is forced to win.
	if causeBeforeWrite == context.Canceled {
		if context.Cause(r.stream.ctx) != causeBeforeWrite || strings.Count(r.writer.Body.String(), doneEvent) != 1 {
			t.Fatal("normal completion outcome was not retained")
		}
		return
	}
	assertBoundaryCause(t, r.stream.ctx, timeoutTypeStreamIdle)
	if causeBeforeWrite != context.Cause(r.stream.ctx) {
		t.Fatal("cancellation cause changed after terminal write")
	}
	if strings.Contains(r.writer.Body.String(), doneEvent) {
		t.Fatalf("idle timeout was effective before terminal write, but response contains DONE; cause=%v body=%s", causeBeforeWrite, r.writer.Body.String())
	}
}
