//go:build !windows

package cli

import "context"

// runAsService 在非 Windows 平台上是空操作：服务管理由 systemd / OpenRC / procd /
// rc.d 负责，进程收到 SIGTERM 后自行退出，不需要额外的服务模式分支。
//
// 返回 false 让调用方走正常的前台运行路径。
func runAsService(cfgPath string, force bool, run func(context.Context) error) bool {
	return false
}
