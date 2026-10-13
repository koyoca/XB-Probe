//go:build windows

package service

import (
	"syscall"
	"unsafe"
)

// hiddenWindow 返回一个"不弹出控制台窗口"的 SysProcAttr。
//
// 卸载流程要派生 `cmd /c timeout ... & del ...` 的子进程；如果它是从服务
// （无控制台）里派生的，会尝试创建新控制台并可能闪出黑框。
// CREATE_NO_WINDOW = 0x08000000 是唯一可靠抑制方式（HideWindow 只对 GUI 进程有效）。
func hiddenWindow() *syscall.SysProcAttr {
	const createNoWindow = 0x08000000
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}

// 避免 unsafe 未使用（后续如需读 SCM 结构会用到）。
var _ = unsafe.Pointer(nil)
