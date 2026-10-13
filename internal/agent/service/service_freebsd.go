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

// freebsdRcScript 是 FreeBSD rc.d 脚本。
//
// 关键点与 Linux 的 OpenRC/systemd 都不同：
//   - 必须 `# PROVIDE:` / `# REQUIRE:` 注释块，否则 rcorder 会忽略它；
//   - 用 /usr/sbin/daemon 做后台化（FreeBSD 官方推荐的 rc 脚本写法），
//     因为它能自动写 pidfile 并在进程退出后重启；
//   - _probe_agent 用户若不存在，用 pw(8) 创建（FreeBSD 没有 useradd）。
func (m *freebsdManager) freebsdRcScript(cfgPath, user string) string {
	return fmt.Sprintf(`#!/bin/sh

# PROVIDE: probe_agent
# REQUIRE: NETWORKING
# KEYWORD: shutdown

. /etc/rc.subr

name="probe_agent"
rcvar="probe_agent_enable"
pidfile="/var/run/${name}.pid"

command="/usr/sbin/daemon"
procname="/usr/local/bin/probe-agent"
command_args="-p ${pidfile} -r -f ${procname} run --config %s"
start_precmd="probe_agent_precmd"

probe_agent_precmd()
{
	if [ ! -f %s ]; then
		err 1 "%s 不存在，请先安装"
	fi
}

load_rc_config $name
: ${probe_agent_enable:="YES"}
run_rc_command "$1"
`, cfgPath, cfgPath, cfgPath, user)
}

func (m *freebsdManager) Install(binPath, cfgPath, user string) error {
	if user == "" {
		user = "probe"
	}
	if err := ensureFreeBSDUser(user); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(freebsdRcPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(freebsdRcPath, []byte(m.freebsdRcScript(cfgPath, user)), 0o755); err != nil {
		return fmt.Errorf("写入 rc.d 脚本失败: %w", err)
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
