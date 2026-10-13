//go:build windows

package service

// HandoverToUser 在 Windows 上是空操作。
//
// Windows 服务由 SCM 以 LocalSystem 启动（见 service_windows.go 的说明），
// 不受文件属主限制；配置文件位于 ProgramData 下，由管理员写、LocalSystem 读。
// 保留这个签名只是为了让 cli 层的调用点不必按平台分叉。
func HandoverToUser(cfgPath, name string) error { return nil }
