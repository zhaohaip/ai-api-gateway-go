package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

const doneEvent = "[DONE]"

type responseWriterUnwrapper interface {
	Unwrap() http.ResponseWriter
}

func (h *Handler) streamChatCompletion(c *gin.Context, requestedModel string, request domain.ChatRequest) {
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

	stream, err := h.chatService.Stream(c.Request.Context(), request)
	if err != nil {
		h.writeError(c, err)
		return
	}
	defer h.closeStream(stream, id)

	sent, finished := false, false
	for {
		chunk, recvErr := stream.Recv()
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

		payload, marshalErr := json.Marshal(toOpenAIChunk(id, created, requestedModel, chunk, !sent))
		if marshalErr != nil {
			h.handleStreamError(c, id, sent, "marshal", domain.NewInternalError(marshalErr))
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
		finished = chunk.FinishReason != nil
	}
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
