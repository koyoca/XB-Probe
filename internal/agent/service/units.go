package service

import (
	"fmt"
	"path/filepath"
)

// 服务定义（systemd unit / OpenRC 脚本 / procd / SysV / FreeBSD rc.d）的**唯一**出产地。
//
// 为什么把这些模板从各自的平台文件里搬出来、并做成纯函数：
//
//  1. **它们以前把二进制路径写死了**（openrc 的 `command="/usr/local/bin/probe-agent"`、
//     procd 的 `command /usr/bin/probe-agent`），而 Install 明明是收 binPath 的。
//     用 `--dir /opt/probe` 装出来的服务指向一个不存在的路径，症状是"服务起了就退出"
//     或"command not found"，与真正的原因（模板写死）隔得很远。
//     现在三个路径（二进制、配置、用户）统一由参数带进来，没有第二份来源。
//
//  2. 模板是纯字符串函数，放在**没有 build tag** 的文件里就能在任意平台直接渲染并断言。
//     以前 Linux/FreeBSD 的渲染结果只能靠 grep 源码"验证"——本机是 Windows，
//     而"改坏了也没人发现"的正是这半边（见 units_test.go）。
//
// 顺带修掉一个只在 FreeBSD 上现形的老问题：freebsd 那份模板只有 3 个 %s 却传了 4 个参数，
// fmt 会把多余参数渲染成字面量 `%!(EXTRA string=probe)` 追加在脚本末尾。那一行会被 sh
// 当成命令执行（`EXTRA string=probe`），于是 `service probe_agent start` 的退出码来自
// 一次 "command not found" 而不是 run_rc_command —— 看起来像"服务起不来"。

// renderSystemdUnit 渲染 systemd 单元。
//
// 启用 systemd 沙箱（3.2 权限最小化）：
//   - 专用低权限用户 probe（由 ensureUser 创建）
//   - NoNewPrivileges / ProtectSystem=strict / ProtectHome / PrivateTmp
//   - AmbientCapabilities=CAP_NET_RAW 以支持 ICMP（3.3 / 12.5）
//
// ⚠ ReadWritePaths=<配置目录> 不是可选项：ProtectSystem=strict 会把整个文件系统
// （含 /etc）对服务挂成只读，而 Agent 运行期**要写回配置**——Master 改了探测目标/间隔后，
// syncConfig 会 agentconfig.Save 落盘（它是"下次重启还生效"的唯一保证）。
// 少了这一行，服务本身能跑、上报也正常，只有远端配置的落盘会失败并被记成
// "拉取配置失败"，极难与"权限问题"联系起来。Master 侧的 unit 同样是这个写法（ReadWritePaths=$DATA）。
//
// 这里**不能**写死 `/etc/probe-agent`：`--dir` 允许把配置放到别处，
// 写死会让所有自定义目录的安装都失去写回能力。所以由 cfgPath 推导。
func renderSystemdUnit(binPath, cfgPath, user string) string {
	cfgDir := filepath.Dir(cfgPath)
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
ReadWritePaths=%s

[Install]
WantedBy=multi-user.target
`, user, binPath, cfgPath, cfgDir)
}

// renderOpenRCScript 渲染 OpenRC（Alpine / Gentoo / Devuan；也是 LXC 里 Alpine 容器的默认 init）。
//
// 要点：
//   - `#!/sbin/openrc-run` 由 OpenRC 的 runscript 解释，不要写成 sh 脚本；
//   - command_background=true + pidfile 是 OpenRC 推荐的"非 daemonize 程序"托管方式；
//   - supervise_daemon_args / respawn 提供崩溃自动拉起；
//   - 容器内（LXC/Docker）OpenRC 的依赖若不存在，`rc-service ... start` 仍可正常工作，
//     但 `rc-update add` 写入的 runlevel 目录可能不存在——所以那一步失败只警告不致命。
//
// TODO（与 Alpine/LXC 真机验证一起收口）：服务以非 root 的 probe 用户运行，
// 而 output_log 指向 /var/log/probe-agent.log —— 该文件需要事先建好并把属主交给服务用户，
// 否则 openrc-run 打不开日志文件。真机上确认过再补，不凭猜改。
func renderOpenRCScript(binPath, cfgPath, user string) string {
	return fmt.Sprintf(`#!/sbin/openrc-run

name="probe-agent"
description="Server Probe Agent"

command="%s"
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
`, binPath, cfgPath, user)
}

// renderProcdScript 渲染 OpenWrt 的 /etc/init.d 脚本（rc.common 风格 + procd 服务定义）。
func renderProcdScript(binPath, cfgPath, user string) string {
	return fmt.Sprintf(`#!/bin/sh /etc/rc.common

USE_PROCD=1
START=95
STOP=10

start_service() {
	procd_open_instance
	procd_set_param command %s run --config %s
	procd_set_param user %s
	procd_set_param respawn 3600 5 5
	procd_set_param stdout 1
	procd_set_param stderr 1
	procd_close_instance
}
`, binPath, cfgPath, user)
}

// renderSysVScript 渲染 SysV init 脚本（兜底路径）。
func renderSysVScript(binPath, cfgPath, user string) string {
	return fmt.Sprintf(`#!/bin/sh
### BEGIN INIT INFO
# Provides:          probe-agent
# Required-Start:    $network $remote_fs
# Required-Stop:     $network $remote_fs
# Default-Start:     2 3 4 5
# Default-Stop:      0 1 6
# Short-Description: Server Probe Agent
### END INIT INFO

DAEMON="%s"
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
`, binPath, cfgPath, user)
}

// renderFreeBSDRcScript 渲染 FreeBSD rc.d 脚本。
//
// 关键点与 Linux 的 OpenRC/systemd 都不同：
//   - 必须 `# PROVIDE:` / `# REQUIRE:` 注释块，否则 rcorder 会忽略它；
//   - 用 /usr/sbin/daemon 做后台化（FreeBSD 官方推荐的 rc 脚本写法），
//     因为它能自动写 pidfile 并在进程退出后重启；
//   - _probe_agent 用户若不存在，用 pw(8) 创建（FreeBSD 没有 useradd）。
//
// ⚠ 这里**故意没有** `-u <user>`：脚本以 root 运行，与 Linux 侧"专用低权限用户"的约定
// 不一致。原因是 FreeBSD 上非 root 能否开 ICMP（raw socket）需要真机确认，而
// FreeBSD 没有 Linux 那种 ping_group_range 之类的无特权 ICMP 通路；在真机验证之前
// 不动特权模型。配置因此也保持 root 属主（root 不受 0600 限制），
// 所以 FreeBSD 分支**不调用** HandoverToUser。
func renderFreeBSDRcScript(binPath, cfgPath string) string {
	return fmt.Sprintf(`#!/bin/sh

# PROVIDE: probe_agent
# REQUIRE: NETWORKING
# KEYWORD: shutdown

. /etc/rc.subr

name="probe_agent"
rcvar="probe_agent_enable"
pidfile="/var/run/${name}.pid"

command="/usr/sbin/daemon"
procname="%s"
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
`, binPath, cfgPath, cfgPath, cfgPath)
}
