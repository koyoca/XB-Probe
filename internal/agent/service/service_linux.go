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

// unitTemplate 启用 systemd 沙箱（3.2 权限最小化）：
//   - 专用低权限用户 probe
//   - NoNewPrivileges / ProtectSystem=strict / ProtectHome / PrivateTmp
//   - AmbientCapabilities=CAP_NET_RAW 以支持 ICMP（3.3 / 12.5）
func (m *systemdManager) unitTemplate(binPath, cfgPath, user string) string {
	return fmt.Sprintf(`[Unit]
Description=Server Probe Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
ExecStart=%s run --config %s
Restart=always
RestartSec=5
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
AmbientCapabilities=CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_RAW
ReadWritePaths=/dev/shm

[Install]
WantedBy=multi-user.target
`, user, binPath, cfgPath)
}

func (m *systemdManager) Install(binPath, cfgPath, user string) error {
	if user == "" {
		user = "probe"
	}
	if err := ensureUser(user, binPath); err != nil {
		return err
	}
	if err := os.WriteFile(systemdUnitPath, []byte(m.unitTemplate(binPath, cfgPath, user)), 0o644); err != nil {
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

// openrcScript 是符合 OpenRC 约定的 init 脚本。
//
// 要点：
//   - `#!/sbin/openrc-run` 由 OpenRC 的 runscript 解释，不要写成 sh 脚本；
//   - command_background=true + pidfile 是 OpenRC 推荐的"非 daemonize 程序"托管方式；
//   - supervise_daemon_args / respawn 提供崩溃自动拉起；
//   - 容器内（LXC/Docker）OpenRC 的依赖若不存在，`rc-service ... start` 仍可正常工作，
//     但 `rc-update add` 写入的 runlevel 目录可能不存在——所以这一步失败只警告不致命。
func (m *openrcManager) openrcScript(cfgPath, user string) string {
	return fmt.Sprintf(`#!/sbin/openrc-run

name="probe-agent"
description="Server Probe Agent"

command="/usr/local/bin/probe-agent"
command_args="run --config %s"
command_user="%s"
command_background="true"
pidfile="/run/probe-agent.pid"
output_log="/var/log/probe-agent.log"
error_log="/var/log/probe-agent.log"

respawn_delay=5
respawn_max=0

depend() {
	need net
	after firewall
}
`, cfgPath, user)
}

func (m *openrcManager) Install(binPath, cfgPath, user string) error {
	if user == "" {
		user = "probe"
	}
	if err := ensureUser(user, binPath); err != nil {
		return err
	}
	if err := os.MkdirAll("/run", 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(openrcConfPath, []byte("# Server Probe Agent（由安装脚本生成）\n"), 0o644); err != nil {
		return fmt.Errorf("写入 OpenRC 配置失败: %w", err)
	}
	if err := os.WriteFile(openrcScriptPath, []byte(m.openrcScript(cfgPath, user)), 0o755); err != nil {
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

// procdScript 是 OpenWrt 的 /etc/init.d 脚本（rc.common 风格 + procd 服务定义）。
func (m *procdManager) procdScript(cfgPath, user string) string {
	return fmt.Sprintf(`#!/bin/sh /etc/rc.common

USE_PROCD=1
START=95
STOP=10

start_service() {
	procd_open_instance
	procd_set_param command /usr/bin/probe-agent run --config %s
	procd_set_param user %s
	procd_set_param respawn 3600 5 5
	procd_set_param stdout 1
	procd_set_param stderr 1
	procd_close_instance
}
`, cfgPath, user)
}

func (m *procdManager) Install(binPath, cfgPath, user string) error {
	if user == "" {
		user = "probe"
	}
	if err := ensureUser(user, binPath); err != nil {
		return err
	}
	if err := os.WriteFile(procdInitPath, []byte(m.procdScript(cfgPath, user)), 0o755); err != nil {
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

func (m *sysvManager) sysvScript(cfgPath, user string) string {
	return fmt.Sprintf(`#!/bin/sh
### BEGIN INIT INFO
# Provides:          probe-agent
# Required-Start:    $network $remote_fs
# Required-Stop:     $network $remote_fs
# Default-Start:     2 3 4 5
# Default-Stop:      0 1 6
# Short-Description: Server Probe Agent
### END INIT INFO

DAEMON="/usr/local/bin/probe-agent"
ARGS="run --config %s"
PIDFILE="/var/run/probe-agent.pid"
USER="%s"

start() {
	if [ -f "$PIDFILE" ] && kill -0 "$(cat $PIDFILE)" 2>/dev/null; then
		echo "probe-agent 已在运行"
		return 0
	fi
	if command -v start-stop-daemon >/dev/null 2>&1; then
		start-stop-daemon --start --quiet --background --make-pidfile \
			--pidfile "$PIDFILE" --chuid "$USER" --exec "$DAEMON" -- $ARGS
	else
		su -s /bin/sh -c "$DAEMON $ARGS &" "$USER" && echo $! > "$PIDFILE"
	fi
	echo "probe-agent 已启动"
}

stop() {
	if [ -f "$PIDFILE" ]; then
		kill "$(cat $PIDFILE)" 2>/dev/null && rm -f "$PIDFILE"
	fi
	echo "probe-agent 已停止"
}

case "$1" in
	start)   start ;;
	stop)    stop ;;
	restart) stop; sleep 1; start ;;
	status)  if [ -f "$PIDFILE" ] && kill -0 "$(cat $PIDFILE)" 2>/dev/null; then echo "running"; else echo "stopped"; exit 3; fi ;;
	*)       echo "用法: $0 {start|stop|restart|status}"; exit 1 ;;
esac
`, cfgPath, user)
}

func (m *sysvManager) Install(binPath, cfgPath, user string) error {
	if user == "" {
		user = "probe"
	}
	if err := ensureUser(user, binPath); err != nil {
		return err
	}
	if err := os.WriteFile(sysvInitPath, []byte(m.sysvScript(cfgPath, user)), 0o755); err != nil {
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
