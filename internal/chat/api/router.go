package api

import "github.com/gin-gonic/gin"

// NewRouter 创建并集中注册聊天路由。
func NewRouter(handler *Handler) *gin.Engine {
	router := gin.New()
	router.Use(gin.Recovery())
	router.POST("/v1/chat/completions", handler.CreateChatCompletion)
	router.GET("/v1/models", handler.ListModels)
	return router
}
