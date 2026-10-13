//go:build freebsd

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// FreeBSD 服务管理器：rc.d（12.4）。macOS 不在支持范围内。
func Detect() Manager { return &freebsdManager{} }

// ---------------------------------------------------------------------------
// FreeBSD rc.d
// ---------------------------------------------------------------------------

const (
	// FreeBSD 的本地服务脚本目录（base system 的 rc.d 在 /etc/rc.d）。
	freebsdRcPath = "/usr/local/etc/rc.d/probe-agent"
)

type freebsdManager struct{}

func (m *freebsdManager) Name() string { return "rcd" }

// FreeBSD rc.d 脚本内容由 renderFreeBSDRcScript 提供（见 units.go）。
//
// ⚠ 这里**故意不调用** HandoverToUser：该脚本以 root 运行（原因见 units.go 里
// renderFreeBSDRcScript 的说明），配置文件保持 root 属主正好匹配它的运行身份。
// 若将来改成以 probe 运行，**必须**同时加上交接，否则会复现 Linux 上那个
// "安装完成但服务 permission denied"的故障。
// writeDefinition 建用户、写 rc.d 脚本 —— **不含 enable 与启动**。
func (m *freebsdManager) writeDefinition(binPath, cfgPath, user string) error {
	if user == "" {
		user = DefaultUser
	}
	if err := ensureFreeBSDUser(user); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(freebsdRcPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(freebsdRcPath, []byte(renderFreeBSDRcScript(binPath, cfgPath)), 0o755); err != nil {
		return fmt.Errorf("写入 rc.d 脚本失败: %w", err)
	}
	return nil
}

// RewriteDefinition 只重写 rc.d 脚本，不 enable 也不启动（没注册就别修）。
//
// ⚠ 与 Linux 侧一样**不做**属主交接（原因见上面 Install 的注释）。
func (m *freebsdManager) RewriteDefinition(binPath, cfgPath, user string) error {
	if !exists(freebsdRcPath) {
		return ErrNotInstalled
	}
	return m.writeDefinition(binPath, cfgPath, user)
}

func (m *freebsdManager) Install(binPath, cfgPath, user string) error {
	if err := m.writeDefinition(binPath, cfgPath, user); err != nil {
		return err
	}
	// enable：写入 rc.conf.d 而不是直接改 /etc/rc.conf（后者容易被手动编辑搞乱）。
	if err := os.WriteFile("/etc/rc.conf.d/probe_agent", []byte("probe_agent_enable=\"YES\"\n"), 0o644); err != nil {
		// 有些套件没有 /etc/rc.conf.d 目录，此时用 sysrc。
		if err2 := run("sysrc", "probe_agent_enable=YES"); err2 != nil {
			fmt.Fprintln(os.Stderr, "警告：设置开机自启失败（可手动 sysrc probe_agent_enable=YES）：", err2)
		}
	}
	return run("service", "probe_agent", "start")
}

func (m *freebsdManager) Uninstall(binPath, cfgPath string) error {
	_ = run("service", "probe_agent", "stop")
	if err := os.Remove(freebsdRcPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove("/etc/rc.conf.d/probe_agent")
	return nil
}

// ensureFreeBSDUser 用 pw(8) 创建用户（FreeBSD 没有 useradd）。
func ensureFreeBSDUser(user string) error {
	if exec.Command("id", "-u", user).Run() == nil {
		return nil
	}
	if _, err := exec.LookPath("pw"); err != nil {
		return fmt.Errorf("未找到 pw(8)，无法创建专用用户 %s", user)
	}
	// pw useradd <name> -d /nonexistent -s /usr/sbin/nologin -c "..." -u <free uid>
	// 用 -u 自动分配 uid（freebsd 的 pw 支持 "-u" 后跟具体值，这里用 sysuid 方式）
	return run("pw", "useradd", user,
		"-d", "/nonexistent",
		"-s", "/usr/sbin/nologin",
		"-c", "Server Probe Agent",
		"-u", "1001")
}
