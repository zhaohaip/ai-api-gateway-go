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

type streamTimeoutState struct {
	ctx        context.Context
	cancel     context.CancelCauseFunc
	newTimer   timeoutTimerFactory
	first      time.Duration
	idle       time.Duration
	firstTimer timeoutTimer
	idleTimer  timeoutTimer
	totalTimer timeoutTimer
	closeOnce  sync.Once
}

func newStreamTimeoutState(
	parent context.Context,
	timeouts StreamTimeouts,
	newTimer timeoutTimerFactory,
) *streamTimeoutState {
	ctx, cancel := context.WithCancelCause(parent)
	state := &streamTimeoutState{
		ctx:      ctx,
		cancel:   cancel,
		newTimer: newTimer,
		first:    timeouts.FirstChunk,
		idle:     timeouts.Idle,
	}
	if timeouts.Total > 0 {
		state.totalTimer = newTimer(timeouts.Total, func() {
			cancel(&gatewayTimeout{typeName: timeoutTypeStreamTotal, duration: timeouts.Total})
		})
	}
	if timeouts.FirstChunk > 0 {
		state.firstTimer = newTimer(timeouts.FirstChunk, func() {
			cancel(&gatewayTimeout{
				typeName: timeoutTypeStreamFirstChunk,
				duration: timeouts.FirstChunk,
			})
		})
	}
	return state
}

func (s *streamTimeoutState) Context() context.Context {
	return s.ctx
}

func (s *streamTimeoutState) FirstChunkReceived() {
	if s.firstTimer != nil {
		s.firstTimer.Stop()
		s.firstTimer = nil
	}
}

func (s *streamTimeoutState) OutputSent() {
	if s.idle <= 0 {
		return
	}
	if s.idleTimer == nil {
		s.idleTimer = s.newTimer(s.idle, func() {
			s.cancel(&gatewayTimeout{typeName: timeoutTypeStreamIdle, duration: s.idle})
		})
		return
	}
	s.idleTimer.Reset(s.idle)
}

func (s *streamTimeoutState) Close() {
	s.closeOnce.Do(func() {
		if s.firstTimer != nil {
			s.firstTimer.Stop()
		}
		if s.idleTimer != nil {
			s.idleTimer.Stop()
		}
		if s.totalTimer != nil {
			s.totalTimer.Stop()
		}
		s.cancel(nil)
	})
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
