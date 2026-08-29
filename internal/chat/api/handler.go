// Package api 实现 OpenAI 兼容的 Gin 协议适配层。
package api

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/service"
)

// Handler 处理 OpenAI 兼容聊天请求。
type Handler struct {
	chatService *service.ChatService
	newID       func() (string, error)
	now         func() time.Time
	logger      *slog.Logger
}

// NewHandler 创建聊天 HTTP Handler。
func NewHandler(chatService *service.ChatService) *Handler {
	return &Handler{
		chatService: chatService,
		newID:       newCompletionID,
		now:         time.Now,
		logger:      slog.Default(),
	}
}

// CreateChatCompletion 处理 POST /v1/chat/completions。
func (h *Handler) CreateChatCompletion(c *gin.Context) {
	if !isJSONContentType(c.GetHeader("Content-Type")) {
		h.writeError(c, domain.NewInvalidRequestError(
			"Content-Type must be application/json",
			"",
			"invalid_content_type",
		))
		return
	}

	request, err := decodeRequest(c.Request.Body)
	if err != nil {
		h.writeError(c, err)
		return
	}
	if err := validateRequest(request); err != nil {
		h.writeError(c, err)
		return
	}

	domainRequest := toDomainRequest(request)
	if request.Stream {
		h.streamChatCompletion(c, request.Model, domainRequest)
		return
	}
	h.generateChatCompletion(c, request.Model, domainRequest)
}

func (h *Handler) generateChatCompletion(c *gin.Context, requestedModel string, request domain.ChatRequest) {
	response, err := h.chatService.Generate(c.Request.Context(), request)
	if err != nil {
		h.writeError(c, err)
		return
	}
	id, err := h.newID()
	if err != nil {
		h.writeError(c, domain.NewInternalError(err))
		return
	}
	c.JSON(http.StatusOK, toOpenAIResponse(id, h.now().Unix(), requestedModel, response))
}

func (h *Handler) writeError(c *gin.Context, err error) {
	status, response := toHTTPError(err)
	c.AbortWithStatusJSON(status, response)
}

func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

func newCompletionID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "chatcmpl-" + hex.EncodeToString(bytes), nil
}
