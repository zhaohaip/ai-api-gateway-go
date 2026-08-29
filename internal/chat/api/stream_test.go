package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	openaiapi "github.com/zhaohaip/ai-api-gateway-go/api/openai"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/service"
)

type streamResult struct {
	chunk domain.ChatChunk
	err   error
}

type handlerFakeStream struct {
	results    []streamResult
	recv       func(int) (domain.ChatChunk, error)
	recvCount  int
	closeCount int
	closeError error
}

func (s *handlerFakeStream) Recv() (domain.ChatChunk, error) {
	call := s.recvCount
	s.recvCount++
	if s.recv != nil {
		return s.recv(call)
	}
	if call >= len(s.results) {
		return domain.ChatChunk{}, io.EOF
	}
	return s.results[call].chunk, s.results[call].err
}

func (s *handlerFakeStream) Close() error {
	s.closeCount++
	return s.closeError
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushCount int
}

func (r *flushRecorder) Flush() {
	r.flushCount++
	r.ResponseRecorder.Flush()
}

func TestHandlerStreamsOpenAICompatibleSSE(t *testing.T) {
	finishReason := "stop"
	stream := &handlerFakeStream{results: []streamResult{
		{},
		{chunk: contentChunk("AI")},
		{chunk: contentChunk(" API gateway")},
		{chunk: domain.ChatChunk{
			FinishReason: &finishReason,
			Usage: &domain.Usage{
				PromptTokens:     3,
				CompletionTokens: 2,
				TotalTokens:      5,
			},
		}},
	}}
	provider := handlerFakeProvider{
		generate: func(_ context.Context, _ domain.ChatRequest) (domain.ChatResponse, error) {
			t.Fatal("stream=true request called Provider.Generate")
			return domain.ChatResponse{}, nil
		},
		stream: func(_ context.Context, request domain.ChatRequest) (domain.ChatStream, error) {
			if request.Model != "default-chat" || len(request.Messages) != 1 {
				t.Fatalf("stream request = %#v", request)
			}
			return stream, nil
		},
	}
	handler := newQuietHandler(provider)
	handler.newID = func() (string, error) { return "chatcmpl-stream-test", nil }
	handler.now = func() time.Time { return time.Unix(1787880000, 0) }
	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	performRequestWithWriter(
		NewRouter(handler),
		recorder,
		context.Background(),
		streamRequestJSON(),
		"application/json",
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	assertSSEHeaders(t, recorder.Header())
	events := parseSSEEvents(t, recorder.Body.String())
	if len(events) != 4 {
		t.Fatalf("event count = %d, events = %#v", len(events), events)
	}
	if recorder.flushCount != len(events) {
		t.Fatalf("flush count = %d, want %d", recorder.flushCount, len(events))
	}
	if events[len(events)-1] != doneEvent {
		t.Fatalf("last event = %q, want [DONE]", events[len(events)-1])
	}

	chunks := make([]openaiapi.ChatCompletionChunk, 0, len(events)-1)
	for _, event := range events[:len(events)-1] {
		var chunk openaiapi.ChatCompletionChunk
		if err := json.Unmarshal([]byte(event), &chunk); err != nil {
			t.Fatalf("decode chunk %q: %v", event, err)
		}
		chunks = append(chunks, chunk)
	}
	for index, chunk := range chunks {
		if chunk.ID != "chatcmpl-stream-test" || chunk.Created != 1787880000 || chunk.Model != "default-chat" {
			t.Fatalf("chunk %d metadata = %#v", index, chunk)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("chunk %d object = %q", index, chunk.Object)
		}
	}
	first := chunks[0]
	if len(first.Choices) != 1 || first.Choices[0].Delta.Role == nil || *first.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("first chunk = %#v", first)
	}
	if first.Choices[0].Delta.Content == nil || *first.Choices[0].Delta.Content != "AI" {
		t.Fatalf("first content = %#v", first.Choices[0].Delta)
	}
	if chunks[1].Choices[0].Delta.Role != nil || chunks[1].Choices[0].Delta.Content == nil {
		t.Fatalf("second delta = %#v", chunks[1].Choices[0].Delta)
	}
	final := chunks[2]
	if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "stop" {
		t.Fatalf("final chunk = %#v", final)
	}
	if final.Usage == nil || final.Usage.TotalTokens != 5 {
		t.Fatalf("final usage = %#v", final.Usage)
	}
	if stream.closeCount != 1 {
		t.Fatalf("Close() count = %d, want 1", stream.closeCount)
	}
}

func TestHandlerFiltersMeaninglessChunksAndMapsReasoning(t *testing.T) {
	finishReason := "stop"
	stream := &handlerFakeStream{results: []streamResult{
		{},
		{chunk: roleChunk()},
		{chunk: roleChunk()},
		{chunk: reasoningChunkWithRole("需要先分析")},
		{},
		{chunk: reasoningChunkWithRole("再组织答案")},
		{chunk: contentChunk("最终答案")},
		{chunk: domain.ChatChunk{Usage: &domain.Usage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		}}},
		{chunk: domain.ChatChunk{FinishReason: &finishReason}},
	}}
	handler := newQuietHandler(handlerFakeProvider{
		stream: func(_ context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
			return stream, nil
		},
	})
	handler.newID = func() (string, error) { return "chatcmpl-reasoning-test", nil }
	handler.now = func() time.Time { return time.Unix(1787911420, 0) }
	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	performRequestWithWriter(
		NewRouter(handler),
		recorder,
		context.Background(),
		streamRequestJSON(),
		"application/json",
	)

	events := parseSSEEvents(t, recorder.Body.String())
	if len(events) != 7 {
		t.Fatalf("event count = %d, events = %#v", len(events), events)
	}
	if recorder.flushCount != len(events) {
		t.Fatalf("flush count = %d, want %d", recorder.flushCount, len(events))
	}
	doneCount := 0
	chunks := make([]openaiapi.ChatCompletionChunk, 0, len(events)-1)
	for _, event := range events {
		if event == doneEvent {
			doneCount++
			continue
		}
		var chunk openaiapi.ChatCompletionChunk
		if err := json.Unmarshal([]byte(event), &chunk); err != nil {
			t.Fatalf("decode chunk %q: %v", event, err)
		}
		if len(chunk.Choices) == 0 && chunk.Usage == nil {
			t.Fatalf("meaningless choices-only chunk was emitted: %s", event)
		}
		chunks = append(chunks, chunk)
	}
	if doneCount != 1 || events[len(events)-1] != doneEvent {
		t.Fatalf("DONE count = %d, events = %#v", doneCount, events)
	}
	if len(chunks) != 6 {
		t.Fatalf("chunk count = %d, chunks = %#v", len(chunks), chunks)
	}
	if len(chunks[0].Choices) != 1 || chunks[0].Choices[0].Delta.Role == nil ||
		*chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Fatalf("role chunk = %#v", chunks[0])
	}
	wantReasoning := []string{"需要先分析", "再组织答案"}
	for index, want := range wantReasoning {
		delta := chunks[index+1].Choices[0].Delta
		if delta.ReasoningContent == nil || *delta.ReasoningContent != want {
			t.Fatalf("reasoning delta %d = %#v", index, delta)
		}
		if delta.Content != nil {
			t.Fatalf("reasoning delta %d contains normal content: %#v", index, delta)
		}
	}
	usageChunk := chunks[4]
	if len(usageChunk.Choices) != 0 || usageChunk.Usage == nil || usageChunk.Usage.TotalTokens != 30 {
		t.Fatalf("usage-only chunk = %#v", usageChunk)
	}
	finalChunk := chunks[5]
	if len(finalChunk.Choices) != 1 || finalChunk.Choices[0].FinishReason == nil ||
		*finalChunk.Choices[0].FinishReason != finishReason {
		t.Fatalf("finish chunk = %#v", finalChunk)
	}
}

func TestHandlerStreamFailureBeforeFirstChunkReturnsJSONError(t *testing.T) {
	stream := &handlerFakeStream{results: []streamResult{{err: domain.NewError(
		domain.ErrorKindRateLimited,
		"the upstream service rate limited the request",
		"",
		"upstream_rate_limited",
		nil,
	)}}}
	handler := newQuietHandler(handlerFakeProvider{
		stream: func(_ context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
			return stream, nil
		},
	})

	recorder := performRequest(NewRouter(handler), context.Background(), streamRequestJSON(), "application/json")
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("first-chunk error committed SSE headers: %#v", recorder.Header())
	}
	response := decodeErrorResponse(t, recorder)
	if response.Error.Code != "upstream_rate_limited" {
		t.Fatalf("error = %#v", response.Error)
	}
	if stream.closeCount != 1 {
		t.Fatalf("Close() count = %d, want 1", stream.closeCount)
	}
}

func TestHandlerPartialStreamFailureStopsWithoutJSONOrDone(t *testing.T) {
	stream := &handlerFakeStream{results: []streamResult{
		{chunk: contentChunk("partial")},
		{err: domain.NewError(domain.ErrorKindUpstream, "upstream failed", "", "upstream_error", nil)},
	}}
	handler := newQuietHandler(handlerFakeProvider{
		stream: func(_ context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
			return stream, nil
		},
	})

	recorder := performRequest(NewRouter(handler), context.Background(), streamRequestJSON(), "application/json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), doneEvent) || strings.Contains(recorder.Body.String(), `"error"`) {
		t.Fatalf("partial failure appended terminal data: %s", recorder.Body.String())
	}
	if len(parseSSEEvents(t, recorder.Body.String())) != 1 {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	if stream.closeCount != 1 {
		t.Fatalf("Close() count = %d, want 1", stream.closeCount)
	}
}

func TestHandlerStreamCancellationPropagatesAndCloses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), streamRequestContextKey{}, "value"))
	var providerContext context.Context
	stream := &handlerFakeStream{}
	stream.recv = func(call int) (domain.ChatChunk, error) {
		if call == 0 {
			cancel()
			return contentChunk("partial"), nil
		}
		<-providerContext.Done()
		return domain.ChatChunk{}, providerContext.Err()
	}
	handler := newQuietHandler(handlerFakeProvider{
		stream: func(receivedContext context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
			providerContext = receivedContext
			if receivedContext.Value(streamRequestContextKey{}) != "value" {
				t.Fatal("request context value was not propagated")
			}
			return stream, nil
		},
	})

	recorder := performRequest(NewRouter(handler), ctx, streamRequestJSON(), "application/json")
	if !errors.Is(providerContext.Err(), context.Canceled) {
		t.Fatalf("provider context error = %v, want canceled", providerContext.Err())
	}
	if strings.Contains(recorder.Body.String(), doneEvent) || strings.Contains(recorder.Body.String(), `"error"`) {
		t.Fatalf("canceled stream appended terminal data: %s", recorder.Body.String())
	}
	if stream.closeCount != 1 {
		t.Fatalf("Close() count = %d, want 1", stream.closeCount)
	}
}

func TestHandlerEmptyStreamReturnsJSONErrorAndCloses(t *testing.T) {
	stream := &handlerFakeStream{}
	handler := newQuietHandler(handlerFakeProvider{
		stream: func(_ context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
			return stream, nil
		},
	})

	recorder := performRequest(NewRouter(handler), context.Background(), streamRequestJSON(), "application/json")
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatal("empty stream committed SSE headers")
	}
	if response := decodeErrorResponse(t, recorder); response.Error.Code != "upstream_error" {
		t.Fatalf("error = %#v", response.Error)
	}
	if stream.closeCount != 1 {
		t.Fatalf("Close() count = %d, want 1", stream.closeCount)
	}
}

func TestHandlerStreamCreationFailureReturnsJSONError(t *testing.T) {
	handler := newQuietHandler(handlerFakeProvider{
		stream: func(_ context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
			return nil, domain.NewError(domain.ErrorKindTimeout, "timed out", "", "upstream_timeout", nil)
		},
	})

	recorder := performRequest(NewRouter(handler), context.Background(), streamRequestJSON(), "application/json")
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatal("stream creation error committed SSE headers")
	}
}

func TestHandlerRejectsWriterWithoutFlush(t *testing.T) {
	streamCalled := false
	handler := newQuietHandler(handlerFakeProvider{
		stream: func(_ context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
			streamCalled = true
			return &handlerFakeStream{}, nil
		},
	})
	writer := newBasicResponseWriter()

	performRequestWithWriter(NewRouter(handler), writer, context.Background(), streamRequestJSON(), "application/json")
	if writer.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", writer.status, writer.body.String())
	}
	if streamCalled {
		t.Fatal("writer without Flush called Provider.Stream")
	}
}

func TestHandlerStreamWithoutFinishReasonDoesNotSendDone(t *testing.T) {
	stream := &handlerFakeStream{results: []streamResult{{chunk: contentChunk("partial")}}}
	handler := newQuietHandler(handlerFakeProvider{
		stream: func(_ context.Context, _ domain.ChatRequest) (domain.ChatStream, error) {
			return stream, nil
		},
	})

	recorder := performRequest(NewRouter(handler), context.Background(), streamRequestJSON(), "application/json")
	if strings.Contains(recorder.Body.String(), doneEvent) {
		t.Fatalf("incomplete stream sent [DONE]: %s", recorder.Body.String())
	}
	if stream.closeCount != 1 {
		t.Fatalf("Close() count = %d, want 1", stream.closeCount)
	}
}

type streamRequestContextKey struct{}

type basicResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newBasicResponseWriter() *basicResponseWriter {
	return &basicResponseWriter{header: make(http.Header)}
}

func (w *basicResponseWriter) Header() http.Header {
	return w.header
}

func (w *basicResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *basicResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func newQuietHandler(provider service.ChatProvider) *Handler {
	handler := NewHandler(service.NewChatService(provider))
	handler.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return handler
}

func contentChunk(content string) domain.ChatChunk {
	return domain.ChatChunk{Delta: domain.ChatDelta{Content: &content}}
}

func reasoningChunkWithRole(content string) domain.ChatChunk {
	role := domain.RoleAssistant
	return domain.ChatChunk{Delta: domain.ChatDelta{Role: &role, ReasoningContent: &content}}
}

func roleChunk() domain.ChatChunk {
	role := domain.RoleAssistant
	return domain.ChatChunk{Delta: domain.ChatDelta{Role: &role}}
}

func streamRequestJSON() string {
	return `{"model":"default-chat","messages":[{"role":"user","content":"hello"}],"stream":true}`
}

func performRequestWithWriter(
	handler http.Handler,
	writer http.ResponseWriter,
	ctx context.Context,
	body string,
	contentType string,
) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", contentType)
	handler.ServeHTTP(writer, request)
}

func assertSSEHeaders(t *testing.T, header http.Header) {
	t.Helper()
	want := map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	}
	for name, value := range want {
		if header.Get(name) != value {
			t.Errorf("header %s = %q, want %q", name, header.Get(name), value)
		}
	}
}

func parseSSEEvents(t *testing.T, body string) []string {
	t.Helper()
	if body == "" {
		return nil
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("SSE body does not end with a blank line: %q", body)
	}
	frames := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n")
	events := make([]string, 0, len(frames))
	for _, frame := range frames {
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("invalid SSE frame = %q", frame)
		}
		events = append(events, strings.TrimPrefix(frame, "data: "))
	}
	return events
}
