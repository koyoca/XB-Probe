//go:build linux

package service

import (
	"fmt"
	"os"
	"os/exec"
)

// Linux 服务管理器：systemd / OpenRC / procd / SysV（12.4）。
//
// Detect 的判定顺序很关键，尤其在容器里：
//   - LXC/Docker 中的 Alpine 只有 OpenRC（没有 systemd），必须真的能装服务；
//   - OpenWrt 有 /etc/init.d 但那是 procd 的 rc.common 风格脚本，不能当 SysV 处理；
//   - 先看 /run/systemd/system 而不是 /sbin/systemctl，因为前者才能证明
//     systemd 真的是 PID 1（在 systemd 容器里也可能只有 systemctl 而没有 init）。
func Detect() Manager {
	switch {
	case exists("/run/systemd/system"):
		return &systemdManager{}
	case exists("/etc/openwrt_release"), exists("/etc/rc.common") && exists("/sbin/procd"):
		return &procdManager{}
	case exists("/run/openrc"), exists("/sbin/openrc"), exists("/etc/init.d/rc"):
		return &openrcManager{}
	case exists("/etc/init.d"):
		return &sysvManager{}
	default:
		return &unsupportedManager{name: "none", hint: "未识别到服务管理器，请使用 --no-service 并自行前台运行"}
	}
}

// ---------------------------------------------------------------------------
// systemd（Linux 主路径）
// ---------------------------------------------------------------------------

const systemdUnitPath = "/etc/systemd/system/probe-agent.service"

type systemdManager struct{}

func (m *systemdManager) Name() string { return "systemd" }

// systemd 单元的内容由 renderSystemdUnit 提供（见 units.go，含沙箱与 ReadWritePaths 的说明）。
func (m *systemdManager) Install(binPath, cfgPath, user string) error {
	if user == "" {
		user = DefaultUser
	}
	if err := ensureUser(user, binPath); err != nil {
		return err
	}
	// 建好用户**立刻**交接配置属主：unit 里的 User=<user> 一启动就会去读
	// <cfgPath>，而它是刚才以 root 身份写的 0600 —— 那正是真机上
	// "安装完成"之后服务反复 `读取配置失败: permission denied` 的原因（见 handover.go）。
	if err := HandoverToUser(cfgPath, user); err != nil {
		return err
	}
	if err := os.WriteFile(systemdUnitPath, []byte(renderSystemdUnit(binPath, cfgPath, user)), 0o644); err != nil {
		return fmt.Errorf("写入 systemd 单元失败: %w", err)
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := run("systemctl", "enable", "--now", "probe-agent"); err != nil {
		return err
	}
	return nil
}

func (m *systemdManager) Uninstall(binPath, cfgPath string) error {
	_ = run("systemctl", "disable", "--now", "probe-agent")
	if err := os.Remove(systemdUnitPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = run("systemctl", "daemon-reload")
	return nil
}

// ensureUser 创建专用低权限用户 probe（无登录 shell、无家目录）。
//
// Alpine 用 BusyBox 的 adduser 而不是 useradd，参数也不一样，这里两条路都试。
func ensureUser(user, binPath string) error {
	if exec.Command("id", "-u", user).Run() == nil {
		return nil // 已存在
	}
	if _, err := exec.LookPath("useradd"); err == nil {
		return run("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", user)
	}
	if _, err := exec.LookPath("adduser"); err == nil {
		// BusyBox adduser：-S 系统用户、-H 不建家目录、-s 指定 shell、-D 不交互
		return run("adduser", "-S", "-H", "-s", "/sbin/nologin", "-D", user)
	}
	return fmt.Errorf("未找到 useradd/adduser，无法创建专用用户 %s", user)
}

// ---------------------------------------------------------------------------
// OpenRC（Alpine / Gentoo / Devuan；也是 LXC 里 Alpine 容器的默认 init）
// ---------------------------------------------------------------------------

const (
	openrcScriptPath = "/etc/init.d/probe-agent"
	openrcConfPath   = "/etc/conf.d/probe-agent"
)

type openrcManager struct{}

func (m *openrcManager) Name() string { return "openrc" }

// OpenRC 的 init 脚本内容由 renderOpenRCScript 提供（见 units.go）。
func (m *openrcManager) Install(binPath, cfgPath, user string) error {
	if user == "" {
		user = DefaultUser
	}
	if err := ensureUser(user, binPath); err != nil {
		return err
	}
	if err := HandoverToUser(cfgPath, user); err != nil {
		return err
	}
	if err := os.MkdirAll("/run", 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(openrcConfPath, []byte("# Server Probe Agent（由安装脚本生成）\n"), 0o644); err != nil {
		return fmt.Errorf("写入 OpenRC 配置失败: %w", err)
	}
	if err := os.WriteFile(openrcScriptPath, []byte(renderOpenRCScript(binPath, cfgPath, user)), 0o755); err != nil {
		return fmt.Errorf("写入 OpenRC 服务脚本失败: %w", err)
	}
	// rc-update 在容器里可能因为 runlevel 目录缺失而失败（如 LXC 的 Alpine 精简镜像），
	// 此时开机能手动 `rc-service probe-agent start`；因此只警告不中断。
	if err := run("rc-update", "add", "probe-agent", "default"); err != nil {
		fmt.Fprintln(os.Stderr, "警告：rc-update 注册开机自启失败（容器环境下常见，可手动 rc-service 启动）：", err)
	}
	return run("rc-service", "probe-agent", "start")
}

func (m *openrcManager) Uninstall(binPath, cfgPath string) error {
	_ = run("rc-service", "probe-agent", "stop")
	_ = run("rc-update", "del", "probe-agent", "default")
	for _, p := range []string{openrcScriptPath, openrcConfPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// procd（OpenWrt / ImmortalWrt）
// ---------------------------------------------------------------------------

const procdInitPath = "/etc/init.d/probe-agent"

type procdManager struct{}

func (m *procdManager) Name() string { return "procd" }

// procd 的服务脚本内容由 renderProcdScript 提供（见 units.go）。
func (m *procdManager) Install(binPath, cfgPath, user string) error {
	if user == "" {
		user = DefaultUser
	}
	if err := ensureUser(user, binPath); err != nil {
		return err
	}
	if err := HandoverToUser(cfgPath, user); err != nil {
		return err
	}
	if err := os.WriteFile(procdInitPath, []byte(renderProcdScript(binPath, cfgPath, user)), 0o755); err != nil {
		return fmt.Errorf("写入 procd 服务脚本失败: %w", err)
	}
	if err := run(procdInitPath, "enable"); err != nil {
		return err
	}
	return run(procdInitPath, "start")
}

func (m *procdManager) Uninstall(binPath, cfgPath string) error {
	_ = run(procdInitPath, "stop")
	_ = run(procdInitPath, "disable")
	if err := os.Remove(procdInitPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// SysV init（兜底）
// ---------------------------------------------------------------------------

const sysvInitPath = "/etc/init.d/probe-agent"

type sysvManager struct{}

func (m *sysvManager) Name() string { return "sysv" }

// SysV init 脚本内容由 renderSysVScript 提供（见 units.go）。
func (m *sysvManager) Install(binPath, cfgPath, user string) error {
	if user == "" {
		user = DefaultUser
	}
	if err := ensureUser(user, binPath); err != nil {
		return err
	}
	if err := HandoverToUser(cfgPath, user); err != nil {
		return err
	}
	if err := os.WriteFile(sysvInitPath, []byte(renderSysVScript(binPath, cfgPath, user)), 0o755); err != nil {
		return fmt.Errorf("写入 init 脚本失败: %w", err)
	}
	// 各家自启命令不同，依次尝试，全都失败只警告（服务仍可手动 /etc/init.d/probe-agent start）。
	for _, c := range [][]string{
		{"update-rc.d", "probe-agent", "defaults"},
		{"chkconfig", "--add", "probe-agent"},
		{"insserv", "probe-agent"},
	} {
		if _, err := exec.LookPath(c[0]); err == nil {
			if err := run(c[0], c[1:]...); err == nil {
				break
			}
		}
	}
	return run(sysvInitPath, "start")
}

func (m *sysvManager) Uninstall(binPath, cfgPath string) error {
	_ = run(sysvInitPath, "stop")
	for _, c := range [][]string{
		{"update-rc.d", "-f", "probe-agent", "remove"},
		{"chkconfig", "--del", "probe-agent"},
		{"insserv", "-r", "probe-agent"},
	} {
		if _, err := exec.LookPath(c[0]); err == nil {
			_ = run(c[0], c[1:]...)
			break
		}
	}
	if err := os.Remove(sysvInitPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
