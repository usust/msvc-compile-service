package host

import (
	"net/http"
	"time"

	"windows-compile-service/internal/router"
)

const (
	// DefaultHTTPAddr 是服务默认监听地址。
	DefaultHTTPAddr = ":10000"
	// ServiceName 是注册到 Windows SCM 的服务名称。
	ServiceName = "MSVCCompileService"
)

// NewHTTPServer 创建统一的 HTTP Server 实例，供前台模式和 Windows 服务模式复用。
func NewHTTPServer(addr string) *http.Server {
	compileSlots := make(chan struct{}, 3)
	engine := router.Register(compileSlots)

	return &http.Server{
		Addr:              addr,
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// listenAndServe 屏蔽正常关闭时的 http.ErrServerClosed，仅在真正异常时返回错误。
func listenAndServe(srv *http.Server) error {
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}

	return nil
}
