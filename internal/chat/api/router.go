package api

import (
	"github.com/gin-gonic/gin"

	gatewayauth "github.com/zhaohaip/ai-api-gateway-go/internal/auth"
)

// NewRouter 创建并集中注册聊天路由。
func NewRouter(handler *Handler, authenticator gatewayauth.APIKeyAuthenticator) *gin.Engine {
	router := gin.New()
	router.Use(gin.Recovery())
	protected := router.Group("/v1")
	protected.Use(apiKeyAuthentication(authenticator))
	protected.POST("/chat/completions", handler.CreateChatCompletion)
	protected.GET("/models", handler.ListModels)
	return router
}
