package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

// Timeouts 表示 Chat API 的非流式和 SSE 业务超时。
type Timeouts struct {
	NonStream time.Duration
	Stream    StreamTimeouts
}

// StreamTimeouts 表示 SSE 首包、空闲和总时长超时。
type StreamTimeouts struct {
	FirstChunk time.Duration
	Idle       time.Duration
	Total      time.Duration
}

type timeoutType string

const (
	timeoutTypeNonStream        timeoutType = "non_stream"
	timeoutTypeStreamFirstChunk timeoutType = "stream_first_chunk"
	timeoutTypeStreamIdle       timeoutType = "stream_idle"
	timeoutTypeStreamTotal      timeoutType = "stream_total"
)

type gatewayTimeout struct {
	typeName timeoutType
	duration time.Duration
}

func (e *gatewayTimeout) Error() string {
	return fmt.Sprintf("gateway %s timeout after %s", e.typeName, e.duration)
}

type timeoutTimer interface {
	Stop() bool
	Reset(time.Duration) bool
}

type timeoutTimerFactory func(time.Duration, func()) timeoutTimer

func realTimeoutTimer(duration time.Duration, callback func()) timeoutTimer {
	return time.AfterFunc(duration, callback)
}

func newCallTimeoutContext(
	parent context.Context,
	duration time.Duration,
	typeName timeoutType,
	newTimer timeoutTimerFactory,
) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	var timer timeoutTimer
	if duration > 0 {
		timer = newTimer(duration, func() {
			cancel(&gatewayTimeout{typeName: typeName, duration: duration})
		})
	}
	var closeOnce sync.Once
	return ctx, func() {
		closeOnce.Do(func() {
			if timer != nil {
				timer.Stop()
			}
			cancel(nil)
		})
	}
}

type streamTerminal uint8

const (
	streamActive streamTerminal = iota
	streamCompleted
	streamTimedOut
	streamCanceled
	streamFailed
)

type streamTimeoutState struct {
	ctx         context.Context
	cancel      context.CancelCauseFunc
	newTimer    timeoutTimerFactory
	idle        time.Duration
	mu          sync.Mutex
	terminal    streamTerminal
	parentCause func() error
	stopParent  func() bool
	parentDone  chan struct{}
	firstTimer  timeoutTimer
	idleTimer   timeoutTimer
	totalTimer  timeoutTimer
}

func newStreamTimeoutState(
	parent context.Context,
	timeouts StreamTimeouts,
	newTimer timeoutTimerFactory,
) *streamTimeoutState {
	// 保留请求值，但取消只由终态决策发布，避免父 Context 绕过互斥锁。
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(parent))
	state := &streamTimeoutState{
		ctx:         ctx,
		cancel:      cancel,
		newTimer:    newTimer,
		idle:        timeouts.Idle,
		parentCause: func() error { return context.Cause(parent) },
		parentDone:  make(chan struct{}),
	}
	// 回调可能立即运行；初始化和终态切换使用同一把锁。
	state.mu.Lock()
	state.stopParent = context.AfterFunc(parent, func() {
		defer close(state.parentDone)
		state.decide(streamCanceled, context.Cause(parent))
	})
	if timeouts.Total > 0 {
		state.totalTimer = newTimer(timeouts.Total, func() {
			state.expire(timeoutTypeStreamTotal, timeouts.Total)
		})
	}
	if timeouts.FirstChunk > 0 {
		state.firstTimer = newTimer(timeouts.FirstChunk, func() {
			state.expire(timeoutTypeStreamFirstChunk, timeouts.FirstChunk)
		})
	}
	state.observeParentLocked()
	state.mu.Unlock()
	return state
}

func (s *streamTimeoutState) Context() context.Context { return s.ctx }

func (s *streamTimeoutState) expire(kind timeoutType, duration time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 首包已处理时，Stop 前已经调度的回调也不能再取消请求。
	if kind == timeoutTypeStreamFirstChunk && s.firstTimer == nil {
		return
	}
	s.observeParentLocked()
	s.decideLocked(streamTimedOut, &gatewayTimeout{typeName: kind, duration: duration})
}

func (s *streamTimeoutState) FirstChunkReceived() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstTimer != nil {
		s.firstTimer.Stop()
		s.firstTimer = nil
	}
}

func (s *streamTimeoutState) OutputSent() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal != streamActive || s.idle <= 0 {
		return
	}
	if s.idleTimer == nil {
		s.idleTimer = s.newTimer(s.idle, func() { s.expire(timeoutTypeStreamIdle, s.idle) })
		return
	}
	s.idleTimer.Reset(s.idle)
}

func (s *streamTimeoutState) decide(terminal streamTerminal, cause error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observeParentLocked()
	return s.decideLocked(terminal, cause)
}

func (s *streamTimeoutState) observeParentLocked() {
	if s.terminal == streamActive {
		if cause := s.parentCause(); cause != nil {
			s.decideLocked(streamCanceled, cause)
		}
	}
}

// 终态、计时器和 Cause 在同一临界区确定；写网络数据时不持锁。
func (s *streamTimeoutState) decideLocked(terminal streamTerminal, cause error) bool {
	if s.terminal != streamActive {
		return false
	}
	s.terminal = terminal
	for _, timer := range []timeoutTimer{s.firstTimer, s.idleTimer, s.totalTimer} {
		if timer != nil {
			timer.Stop()
		}
	}
	if s.stopParent() {
		close(s.parentDone)
	}
	// 正常完成沿用 cancel(nil) 清理 Context；由 terminal 区分完成和客户端取消。
	s.cancel(cause)
	return true
}

func (s *streamTimeoutState) result() (streamTerminal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observeParentLocked()
	return s.terminal, context.Cause(s.ctx)
}

func (s *streamTimeoutState) Close() {
	s.decide(streamCanceled, nil)
	// AfterFunc 停止失败表示回调已开始；必须在锁外等待其退出。
	<-s.parentDone
}

func timeoutFromContext(ctx context.Context) *gatewayTimeout {
	timeout, _ := context.Cause(ctx).(*gatewayTimeout)
	return timeout
}

func timeoutDomainError(timeout *gatewayTimeout) *domain.Error {
	code := "upstream_timeout"
	message := "the upstream service timed out"
	switch timeout.typeName {
	case timeoutTypeStreamFirstChunk:
		code = "stream_first_chunk_timeout"
		message = "the upstream stream did not produce its first chunk in time"
	case timeoutTypeStreamIdle:
		code = "stream_idle_timeout"
		message = "the upstream stream was idle for too long"
	case timeoutTypeStreamTotal:
		code = "stream_total_timeout"
		message = "the upstream stream exceeded its total duration"
	}
	return domain.NewError(domain.ErrorKindTimeout, message, "", code, timeout)
}

func canceledRequestError(cause error) *domain.Error {
	return domain.NewError(
		domain.ErrorKindCanceled,
		"the request was canceled by the client",
		"",
		"request_canceled",
		cause,
	)
}

func (h *Handler) logTimeout(
	c *gin.Context,
	keyID string,
	model string,
	stream bool,
	timeout *gatewayTimeout,
	responseCommitted bool,
) {
	h.logger.Warn(
		"模型请求超时",
		"request_id", requestID(c),
		"key_id", keyID,
		"model", model,
		"provider", h.chatService.ProviderName(model),
		"stream", stream,
		"timeout_type", timeout.typeName,
		"duration", timeout.duration,
		"response_committed", responseCommitted,
	)
}
