//go:build windows

package host

import (
	"context"
	"net/http"
	"time"

	"golang.org/x/sys/windows/svc"
)

// Run 在 Windows 上自动判断当前进程是否由 SCM 以服务方式启动。
// 如果是服务模式，则走原生 Windows 服务入口；
// 否则按普通前台进程启动，方便直接双击或命令行调试。
func Run() error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return err
	}

	if isService {
		return svc.Run(ServiceName, &serviceHandler{})
	}

	return listenAndServe(NewHTTPServer(DefaultHTTPAddr))
}

// serviceHandler 实现 Windows 服务协议。
type serviceHandler struct{}

// Execute 是 SCM 与服务进程交互的核心入口。
func (h *serviceHandler) Execute(_ []string, changes <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	status <- svc.Status{State: svc.StartPending}

	server := NewHTTPServer(DefaultHTTPAddr)
	serverErr := make(chan error, 1)

	go func() {
		serverErr <- listenAndServe(server)
	}()

	status <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case err := <-serverErr:
			status <- svc.Status{State: svc.StopPending}
			if err != nil {
				return false, 1
			}
			return false, 0
		case req := <-changes:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				shutdown(server)
				if err := <-serverErr; err != nil && err != http.ErrServerClosed {
					return false, 1
				}
				return false, 0
			default:
			}
		}
	}
}

// shutdown 给 HTTP 服务一个有限时间做优雅退出，避免 SCM 长时间等待。
func shutdown(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}
