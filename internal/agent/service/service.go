// Package service 负责系统服务（守护进程）的识别与安装/卸载（文档 3.2 / 12.4）。
//
// 各平台的识别逻辑在各自的 build-tag 文件里：
//   - Linux：service_linux.go（systemd → OpenRC → procd → SysV）
//   - FreeBSD：service_freebsd.go（rc.d）
//   - Windows：service_windows.go（SCM）
package service

import (
	"errors"
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
	// RewriteDefinition 重写服务定义（unit/init 脚本 / rc.d 脚本 / SCM 登记项），
	// **不含启动**。定义的内容由**当前二进制里的模板**渲染。
	//
	// 为什么它必须与 Install 分开（而不是叫 install 的一个参数）：
	// `probe-agent upgrade` 只换二进制，而节点上的服务定义与配置属主是**更早那次安装**
	// 留下的——老版本的 unit 里没有 ReadWritePaths=<配置目录>（ProtectSystem=strict 下
	// 配置写不回去），更老的那次安装也没有把 0600 root:root 的配置交给服务用户
	// （unit 里 User=probe 一启动就是 permission denied）。升级流程要把这两样补齐，
	// 但**不能顺手重启服务**（重启由用户执行，见 cmdUpgrade 的约定）。
	RewriteDefinition(binPath, cfgPath, user string) error
	// Install 重写服务定义并启动服务（= RewriteDefinition + 启动）。
	Install(binPath, cfgPath, user string) error
	// Uninstall 停止并移除服务。
	Uninstall(binPath, cfgPath string) error
}

// ErrNotInstalled 表示"这台机器上没有把 Agent 注册成系统服务"。
//
// 与"修失败了"是两回事：容器里用 `--no-service` 跑、或手工前台跑的节点本来就没有
// 服务定义可改。升级路径必须能把这种情况与真失败分开 —— 否则每个容器节点升一次级
// 就会收到一条"警告"，更糟的是**凭空写出一份用户没要过的 unit**。
//
// 判据落在**已注册**（文件是否存在 / SCM 里有没有这个服务）上，而不是"目录还在不在"：
// 真正的风险是"创建"这个动作，不是"覆盖"。
var ErrNotInstalled = errors.New("本机没有把 Agent 注册成系统服务")

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

func (m *unsupportedManager) RewriteDefinition(binPath, cfgPath, user string) error {
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
