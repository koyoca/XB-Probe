// Package service 负责系统服务（守护进程）的识别与安装/卸载（文档 3.2 / 12.4）。
//
// 各平台的识别逻辑在各自的 build-tag 文件里：
//   - Linux：service_linux.go（systemd → OpenRC → procd → SysV）
//   - FreeBSD：service_freebsd.go（rc.d）
//   - Windows：service_windows.go（SCM）
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Manager 是服务管理器的抽象。
type Manager interface {
	// Name 返回标识：systemd/openrc/procd/sysv/rcd/windows/none
	Name() string
	// Install 创建并启动服务。
	Install(binPath, cfgPath, user string) error
	// Uninstall 停止并移除服务。
	Uninstall(binPath, cfgPath string) error
}

// ---------------------------------------------------------------------------
// 未实现的平台
// ---------------------------------------------------------------------------

// unsupportedManager 明确报错而不是静默失败，避免留下半安装状态。
type unsupportedManager struct {
	name string
	hint string
}

func (m *unsupportedManager) Name() string { return m.name }

func (m *unsupportedManager) Install(binPath, cfgPath, user string) error {
	return fmt.Errorf("当前平台的服务管理器 %s 尚未实现：%s", m.name, m.hint)
}

func (m *unsupportedManager) Uninstall(binPath, cfgPath string) error {
	return fmt.Errorf("当前平台的服务管理器 %s 尚未实现：%s", m.name, m.hint)
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s 失败: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// EnsureDirs 创建配置目录并设置权限（配置 0600 由调用方负责）。
func EnsureDirs(cfgPath string) error {
	return os.MkdirAll(filepath.Dir(cfgPath), 0o755)
}
