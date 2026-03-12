//go:build !windows

package host

// Run 在非 Windows 环境下以前台模式启动服务。
func Run() error {
	return listenAndServe(NewHTTPServer(DefaultHTTPAddr))
}
