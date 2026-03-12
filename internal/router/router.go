package router

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"windows-compile-service/internal/api"
	"windows-compile-service/internal/app"
)

// Register 统一创建并注册 HTTP 路由。
func Register(compileSlots chan struct{}) *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Logger(), gin.Recovery())

	handler := api.NewHandler(compileSlots)
	engine.POST("/compile", handler.Compile)
	engine.GET("/healthz", health)
	engine.GET("/version", version)

	return engine
}

// health 提供最小健康检查响应。
func health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// version 返回当前服务版本号。
func version(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"version": app.Version})
}
