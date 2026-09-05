package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	gatewayauth "github.com/zhaohaip/ai-api-gateway-go/internal/auth"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
	"github.com/zhaohaip/ai-api-gateway-go/internal/concurrencylimit"
)

const doneEvent = "[DONE]"

type responseWriterUnwrapper interface {
	Unwrap() http.ResponseWriter
}

func (h *Handler) streamChatCompletion(
	c *gin.Context,
	principal gatewayauth.Principal,
	requestedModel string,
	request domain.ChatRequest,
	lease concurrencylimit.Lease,
) {
	defer lease.Release()
	flusher, ok := sseFlusher(c.Writer)
	if !ok {
		h.writeError(c, domain.NewInternalError(errors.New("response writer does not support flushing")))
		return
	}
	id, err := h.newID()
	if err != nil {
		h.writeError(c, domain.NewInternalError(err))
		return
	}
	created := h.now().Unix()

	timeoutState := newStreamTimeoutState(c.Request.Context(), h.timeouts.Stream, h.newTimer)
	var stream domain.ChatStream
	defer func() {
		timeoutState.Close()
		if stream != nil {
			h.closeStream(stream, id)
		}
	}()
	stream, err = h.chatService.Stream(timeoutState.Context(), request)
	if stream != nil {
		stream = newConcurrencyChatStream(stream, lease)
	}
	if err != nil {
		timeoutState.decide(streamFailed, err)
	}
	if h.handleStreamTerminal(c, timeoutState, principal.KeyID, requestedModel, id, false, "create") {
		return
	}

	sent, finished := false, false
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			switch {
			case !sent:
				timeoutState.decide(streamFailed, upstreamStreamError("the upstream service returned an empty stream"))
			case !finished:
				timeoutState.decide(streamFailed, upstreamStreamError("the upstream stream ended without a finish reason"))
			default:
				if timeoutState.decide(streamCompleted, nil) {
					// 完成胜出后，迟到的取消或超时不能改变结果；网络写失败仅记录交付失败。
					if err := writeSSEEvent(c.Writer, flusher, []byte(doneEvent)); err != nil {
						h.logStreamError(id, "write_done", domain.NewInternalError(err))
					}
					return
				}
			}
			h.handleStreamTerminal(c, timeoutState, principal.KeyID, requestedModel, id, sent, "eof")
			return
		}
		if recvErr != nil {
			timeoutState.decide(streamFailed, recvErr)
		}
		if h.handleStreamTerminal(c, timeoutState, principal.KeyID, requestedModel, id, sent, "receive") {
			return
		}
		if chunk.Empty() || !hasOpenAIChunkData(chunk, !sent) {
			continue
		}
		if finished {
			timeoutState.decide(streamFailed, upstreamStreamError("the upstream stream returned data after finishing"))
			h.handleStreamTerminal(c, timeoutState, principal.KeyID, requestedModel, id, sent, "protocol")
			return
		}
		timeoutState.FirstChunkReceived()
		payload, marshalErr := json.Marshal(toOpenAIChunk(id, created, requestedModel, chunk, !sent))
		if marshalErr != nil {
			timeoutState.decide(streamFailed, domain.NewInternalError(marshalErr))
		}
		if h.handleStreamTerminal(c, timeoutState, principal.KeyID, requestedModel, id, sent, "marshal") {
			return
		}
		if !sent {
			setSSEHeaders(c.Writer.Header())
		}
		if writeErr := writeSSEEvent(c.Writer, flusher, payload); writeErr != nil {
			timeoutState.decide(streamFailed, domain.NewInternalError(writeErr))
			// 写出可能部分成功，不再尝试追加 JSON。
			h.handleStreamTerminal(c, timeoutState, principal.KeyID, requestedModel, id, true, "write")
			return
		}
		sent = true
		timeoutState.OutputSent()
		finished = chunk.FinishReason != nil
	}
}

func (h *Handler) handleStreamTerminal(
	c *gin.Context, state *streamTimeoutState, keyID, model, id string, sent bool, stage string,
) bool {
	terminal, cause := state.result()
	switch terminal {
	case streamActive:
		return false
	case streamTimedOut:
		h.handleStreamTimeout(c, keyID, model, sent, timeoutFromContext(state.Context()))
	case streamCanceled:
		if stage == "create" {
			h.handleStreamError(c, id, sent, "canceled", canceledRequestError(cause))
		} else {
			h.logStreamError(id, "canceled", canceledRequestError(cause))
		}
	case streamFailed:
		h.handleStreamError(c, id, sent, stage, cause)
	}
	return true
}

func (h *Handler) handleStreamTimeout(
	c *gin.Context,
	keyID string,
	model string,
	responseCommitted bool,
	timeout *gatewayTimeout,
) {
	h.logTimeout(c, keyID, model, true, timeout, responseCommitted)
	if !responseCommitted {
		h.writeError(c, timeoutDomainError(timeout))
		return
	}
	// SSE 已提交后只能终止连接，不能追加普通 JSON 错误或伪造 [DONE]。
}

func (h *Handler) handleStreamError(c *gin.Context, id string, sent bool, stage string, err error) {
	if !sent {
		h.writeError(c, err)
		return
	}
	h.logStreamError(id, stage, err)
}

func (h *Handler) closeStream(stream domain.ChatStream, id string) {
	if err := stream.Close(); err != nil {
		h.logStreamError(id, "close", err)
	}
}

func (h *Handler) logStreamError(id, stage string, err error) {
	kind, code := domain.ErrorKindInternal, "internal_error"
	var domainErr *domain.Error
	if errors.As(err, &domainErr) {
		kind, code = domainErr.Kind, domainErr.Code
	}
	h.logger.Error(
		"流式聊天响应异常",
		"completion_id", id,
		"stage", stage,
		"error_kind", kind,
		"error_code", code,
	)
}

func sseFlusher(writer gin.ResponseWriter) (http.Flusher, bool) {
	underlying := http.ResponseWriter(writer)
	if unwrapper, ok := writer.(responseWriterUnwrapper); ok {
		underlying = unwrapper.Unwrap()
	}
	if _, ok := underlying.(http.Flusher); !ok {
		return nil, false
	}
	return writer, true
}

func setSSEHeaders(header http.Header) {
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
}

func writeSSEEvent(writer io.Writer, flusher http.Flusher, payload []byte) error {
	event := make([]byte, 0, len(payload)+8)
	event = append(event, "data: "...)
	event = append(event, payload...)
	event = append(event, '\n', '\n')
	if _, err := writer.Write(event); err != nil {
		return fmt.Errorf("write SSE event: %w", err)
	}
	flusher.Flush()
	return nil
}

func upstreamStreamError(message string) *domain.Error {
	return domain.NewError(
		domain.ErrorKindUpstream,
		message,
		"",
		"upstream_error",
		nil,
	)
}
