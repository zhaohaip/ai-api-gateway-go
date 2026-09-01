package api

import (
	"context"
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
	stream, err := h.chatService.Stream(timeoutState.Context(), request)
	if timeout := timeoutFromContext(timeoutState.Context()); timeout != nil {
		if stream != nil {
			h.closeStream(stream, id)
		}
		timeoutState.Close()
		h.handleStreamTimeout(c, principal.KeyID, requestedModel, false, timeout)
		return
	}
	if err != nil {
		timeoutState.Close()
		h.writeError(c, err)
		return
	}
	stream = newConcurrencyChatStream(stream, lease)
	defer func() {
		timeoutState.Close()
		h.closeStream(stream, id)
	}()

	sent, finished := false, false
	for {
		chunk, recvErr := stream.Recv()
		if timeout := timeoutFromContext(timeoutState.Context()); timeout != nil {
			h.handleStreamTimeout(c, principal.KeyID, requestedModel, sent, timeout)
			return
		}
		if timeoutState.Context().Err() != nil {
			h.logStreamError(
				id,
				"canceled",
				canceledRequestError(context.Cause(timeoutState.Context())),
			)
			return
		}
		if errors.Is(recvErr, io.EOF) {
			h.finishStream(c, flusher, id, sent, finished)
			return
		}
		if recvErr != nil {
			h.handleStreamError(c, id, sent, "receive", recvErr)
			return
		}
		if chunk.Empty() {
			continue
		}
		if !hasOpenAIChunkData(chunk, !sent) {
			continue
		}
		if finished {
			h.logStreamError(id, "protocol", upstreamStreamError("the upstream stream returned data after finishing"))
			return
		}
		timeoutState.FirstChunkReceived()

		payload, marshalErr := json.Marshal(toOpenAIChunk(id, created, requestedModel, chunk, !sent))
		if marshalErr != nil {
			h.handleStreamError(c, id, sent, "marshal", domain.NewInternalError(marshalErr))
			return
		}
		if timeout := timeoutFromContext(timeoutState.Context()); timeout != nil {
			h.handleStreamTimeout(c, principal.KeyID, requestedModel, sent, timeout)
			return
		}
		if !sent {
			setSSEHeaders(c.Writer.Header())
		}
		if writeErr := writeSSEEvent(c.Writer, flusher, payload); writeErr != nil {
			h.logStreamError(id, "write", domain.NewInternalError(writeErr))
			return
		}
		sent = true
		timeoutState.OutputSent()
		finished = chunk.FinishReason != nil
	}
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

func (h *Handler) finishStream(c *gin.Context, flusher http.Flusher, id string, sent, finished bool) {
	if !sent {
		h.writeError(c, upstreamStreamError("the upstream service returned an empty stream"))
		return
	}
	if !finished {
		h.logStreamError(id, "eof", upstreamStreamError("the upstream stream ended without a finish reason"))
		return
	}
	if err := writeSSEEvent(c.Writer, flusher, []byte(doneEvent)); err != nil {
		h.logStreamError(id, "write_done", domain.NewInternalError(err))
	}
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
