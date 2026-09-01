// Package api 实现 OpenAI 兼容的 Gin 协议适配层。
package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	gatewayauth "github.com/zhaohaip/ai-api-gateway-go/internal/auth"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/service"
	"github.com/zhaohaip/ai-api-gateway-go/internal/concurrencylimit"
	"github.com/zhaohaip/ai-api-gateway-go/internal/ratelimit"
)

// Handler 处理 OpenAI 兼容聊天请求。
type Handler struct {
	chatService *service.ChatService
	authorizer  gatewayauth.ModelAuthorizer
	limiter     ratelimit.Limiter
	concurrency concurrencylimit.Controller
	newID       func() (string, error)
	now         func() time.Time
	logger      *slog.Logger
}

// NewHandler 创建聊天 HTTP Handler。
func NewHandler(chatService *service.ChatService) *Handler {
	return NewHandlerWithRequestControls(chatService, unlimitedLimiter{}, unlimitedConcurrencyController{})
}

// NewHandlerWithLimiter 创建使用指定请求频率限制器的聊天 HTTP Handler。
func NewHandlerWithLimiter(chatService *service.ChatService, limiter ratelimit.Limiter) *Handler {
	return NewHandlerWithRequestControls(chatService, limiter, unlimitedConcurrencyController{})
}

// NewHandlerWithRequestControls 创建使用指定频率和并发控制器的聊天 HTTP Handler。
func NewHandlerWithRequestControls(
	chatService *service.ChatService,
	limiter ratelimit.Limiter,
	concurrencyController concurrencylimit.Controller,
) *Handler {
	return &Handler{
		chatService: chatService,
		authorizer:  gatewayauth.ModelAuthorizer{},
		limiter:     limiter,
		concurrency: concurrencyController,
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
	principal, exists := gatewayauth.PrincipalFromContext(c.Request.Context())
	if !exists {
		h.writeError(c, gatewayauth.NewAuthenticationError())
		return
	}
	if err := h.authorizer.Authorize(principal, request.Model); err != nil {
		h.writeError(c, err)
		return
	}
	if !h.allowRequest(c, principal, request.Model) {
		return
	}
	lease, ok := h.acquireConcurrency(c, principal, request.Model, request.Stream)
	if !ok {
		return
	}

	domainRequest := toDomainRequest(request)
	if request.Stream {
		h.streamChatCompletion(c, request.Model, domainRequest, lease)
		return
	}
	defer lease.Release()
	h.generateChatCompletion(c, request.Model, domainRequest)
}

// ListModels 处理 GET /v1/models。
func (h *Handler) ListModels(c *gin.Context) {
	principal, exists := gatewayauth.PrincipalFromContext(c.Request.Context())
	if !exists {
		h.writeError(c, gatewayauth.NewAuthenticationError())
		return
	}
	models := h.chatService.ListModels()
	allowedModels := models[:0]
	for _, model := range models {
		if h.authorizer.Allows(principal, model.ID) {
			allowedModels = append(allowedModels, model)
		}
	}
	if !h.allowRequest(c, principal, "") {
		return
	}
	lease, ok := h.acquireConcurrency(c, principal, "", false)
	if !ok {
		return
	}
	defer lease.Release()
	c.JSON(http.StatusOK, toOpenAIModelList(allowedModels, h.now().Unix()))
}

func (h *Handler) allowRequest(c *gin.Context, principal gatewayauth.Principal, model string) bool {
	err := h.limiter.Allow(principal.KeyID)
	if err == nil {
		return true
	}
	var limitErr *ratelimit.Error
	if errors.As(err, &limitErr) {
		h.logger.Warn(
			"请求超过频率限制",
			"request_id", requestID(c),
			"key_id", principal.KeyID,
			"model", model,
			"limit_scope", limitErr.Scope,
		)
	}
	h.writeError(c, err)
	return false
}

func (h *Handler) acquireConcurrency(
	c *gin.Context,
	principal gatewayauth.Principal,
	model string,
	stream bool,
) (concurrencylimit.Lease, bool) {
	lease, err := h.concurrency.Acquire(principal.KeyID)
	if err == nil {
		return lease, true
	}
	var concurrencyErr *concurrencylimit.Error
	if errors.As(err, &concurrencyErr) {
		h.logger.Warn(
			"请求超过并发限制",
			"request_id", requestID(c),
			"key_id", principal.KeyID,
			"model", model,
			"stream", stream,
			"limit_scope", concurrencyErr.Scope,
		)
	}
	h.writeError(c, err)
	return nil, false
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
	writeHTTPError(c, err)
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

func requestID(c *gin.Context) string {
	if value := strings.TrimSpace(c.GetHeader("X-Request-ID")); value != "" {
		return value
	}
	id, err := newCompletionID()
	if err != nil {
		return "unavailable"
	}
	return "req-" + strings.TrimPrefix(id, "chatcmpl-")
}

type unlimitedLimiter struct{}

func (unlimitedLimiter) Allow(string) error {
	return nil
}

type unlimitedConcurrencyController struct{}

func (unlimitedConcurrencyController) Acquire(string) (concurrencylimit.Lease, error) {
	return unlimitedConcurrencyLease{}, nil
}

type unlimitedConcurrencyLease struct{}

func (unlimitedConcurrencyLease) Release() {}
