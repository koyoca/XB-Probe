//go:build linux

package service

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
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

const systemdUnitPath = "/etc/systemd/system/xb-probe-agent.service"

type systemdManager struct{}

func (m *systemdManager) Name() string { return "systemd" }

// writeDefinition 建用户、交接配置属主、写 unit、daemon-reload —— **不含启动**。
//
// daemon-reload 只让 systemd 重读单元文件，**不会**重启服务：升级路径要在
// "不动正在跑的服务"的前提下把新定义放好（见 Manager.RewriteDefinition 的说明）。
func (m *systemdManager) writeDefinition(binPath, cfgPath, user string) error {
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
	return run("systemctl", "daemon-reload")
}

// RewriteDefinition 只重写 unit 与配置属主，不启动/不重启（供 upgrade 的修复用）。
//
// "没注册就别修"：容器/`--no-service` 跑的节点没有 unit，凭空写一份出来
// 就是给用户造了一个他没要过的系统状态（见 ErrNotInstalled）。
// 检查**不能**放进 writeDefinition：Install 走的是同一段代码，而新装时 unit 恰恰还不存在。
func (m *systemdManager) RewriteDefinition(binPath, cfgPath, user string) error {
	if !exists(systemdUnitPath) {
		return ErrNotInstalled
	}
	return m.writeDefinition(binPath, cfgPath, user)
}

func (m *systemdManager) Install(binPath, cfgPath, user string) error {
	if err := m.writeDefinition(binPath, cfgPath, user); err != nil {
		return err
	}
	return run("systemctl", "enable", "--now", "xb-probe-agent")
}

func (m *systemdManager) Uninstall(binPath, cfgPath string) error {
	_ = run("systemctl", "disable", "--now", "xb-probe-agent")
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
	openrcScriptPath = "/etc/init.d/xb-probe-agent"
	openrcConfPath   = "/etc/conf.d/xb-probe-agent"
	// openrcLogPath 是 init 脚本里 output_log/error_log 指向的文件。
	// 它同时被"预建并交接属主"那一步使用——两处必须同一个值。
	openrcLogPath = "/var/log/xb-probe-agent.log"
	openrcSvcName = "xb-probe-agent"
)

type openrcManager struct{}

func (m *openrcManager) Name() string { return "openrc" }

// writeDefinition 建用户、交接配置属主、写 init 脚本与 conf.d —— **不含启动**。
func (m *openrcManager) writeDefinition(binPath, cfgPath, user string) error {
	if user == "" {
		user = DefaultUser
	}
	if err := ensureUser(user, binPath); err != nil {
		return err
	}
	if err := HandoverToUser(cfgPath, user); err != nil {
		return err
	}
	// 日志文件必须在**启动之前**建好并交给服务用户（见 EnsureLogFile）：
	// OpenRC 会把服务的 stdout/stderr 重定向到 output_log，而服务以非 root 跑，
	// /var/log 下自己创建不了这个文件 —— 脚本、conf.d、用户全都就位，只有这一步
	// 没做，`rc-service xb-probe-agent start` 依然是失败的。
	if err := EnsureLogFile(openrcLogPath, user); err != nil {
		return err
	}
	if err := os.MkdirAll("/run", 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(openrcConfPath, []byte("# XB-Probe Agent（由安装脚本生成）\n"), 0o644); err != nil {
		return fmt.Errorf("写入 OpenRC 配置失败: %w", err)
	}
	if err := os.WriteFile(openrcScriptPath, []byte(renderOpenRCScript(binPath, cfgPath, user, openrcLogPath)), 0o755); err != nil {
		return fmt.Errorf("写入 OpenRC 服务脚本失败: %w", err)
	}
	return nil
}

// RewriteDefinition 只重写 init 脚本与配置属主，不启动/不重启（没注册就别修）。
func (m *openrcManager) RewriteDefinition(binPath, cfgPath, user string) error {
	if !exists(openrcScriptPath) {
		return ErrNotInstalled
	}
	return m.writeDefinition(binPath, cfgPath, user)
}

func (m *openrcManager) Install(binPath, cfgPath, user string) error {
	if err := m.writeDefinition(binPath, cfgPath, user); err != nil {
		return err
	}
	// rc-update 在容器里可能因为 runlevel 目录缺失而失败（如 LXC 的 Alpine 精简镜像），
	// 此时开机能手动 `rc-service xb-probe-agent start`；因此只警告不中断。
	if err := run("rc-update", "add", openrcSvcName, "default"); err != nil {
		fmt.Fprintln(os.Stderr, "警告：rc-update 注册开机自启失败（容器环境下常见，可手动 rc-service 启动）：", err)
	}
	if err := run("rc-service", openrcSvcName, "start"); err != nil {
		return openrcStartFailed(err, binPath, cfgPath)
	}
	// "配好了"要当场验：rc-service 的退出码来自 openrc-run，而 openrc-run 在
	// command_background 模式下**派生子进程后即返回**，服务随后立刻退出也算成功。
	// 判据挑"进程真的在"，不挑"start 命令没报错"。
	if !openrcRunning() {
		return openrcStartFailed(fmt.Errorf("start 返回成功，但进程没有起来（通常在几秒内就退出了）"), binPath, cfgPath)
	}
	return nil
}

// openrcRunning 判断 OpenRC 托管的 Agent 进程是否真的在跑。
//
// 用 `rc-service <name> status`，而不是读 pidfile：pidfile 可能是上一次运行留下的，
// 而 pid 复用会让"文件在、进程不在"变成"看起来在跑"。
func openrcRunning() bool {
	return run("rc-service", openrcSvcName, "status") == nil
}

// openrcStartFailed 把"起不来"翻译成一份能照着做的诊断。
//
// 为什么不能只 return err：真机上用户拿到的是 openrc-run 的一句模糊输出，
// 而他需要知道的是"日志在哪、怎么单独起一次看报错"。所以这里把日志尾部
// 一并打出来——Alpine 上排查权限/依赖问题基本都靠这一段。
func openrcStartFailed(err error, binPath, cfgPath string) error {
	fmt.Fprintf(os.Stderr,
		"\nOpenRC 未能把 Agent 拉起来。逐项自查：\n"+
			"  1) 配置读不到：ls -l %s（属主应为服务用户 %s）\n"+
			"  2) 日志写不进去：ls -l %s（属主应为服务用户）\n"+
			"  3) 依赖解析失败：检查 %s 里的 depend()（容器里没有 net 服务时用 use net，不要用 need）\n"+
			"  4) 单独起一次看完整报错：rc-service %s start && rc-service %s status\n"+
			"前台直接运行以看到原始错误：%s run --config %s\n",
		cfgPath, DefaultUser, openrcLogPath, openrcScriptPath,
		openrcSvcName, openrcSvcName, binPath, cfgPath)
	if out, e := exec.Command("tail", "-n", "20", openrcLogPath).CombinedOutput(); e == nil && len(strings.TrimSpace(string(out))) > 0 {
		fmt.Fprintf(os.Stderr, "\n%s 的最后 20 行：\n%s\n", openrcLogPath, strings.TrimSpace(string(out)))
	}
	return fmt.Errorf("OpenRC 启动 %s 失败: %w", openrcSvcName, err)
}

func (m *openrcManager) Uninstall(binPath, cfgPath string) error {
	_ = run("rc-service", openrcSvcName, "stop")
	_ = run("rc-update", "del", openrcSvcName, "default")
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

const procdInitPath = "/etc/init.d/xb-probe-agent"

type procdManager struct{}

func (m *procdManager) Name() string { return "procd" }

// writeDefinition 建用户、交接配置属主、写 procd 脚本 —— **不含启动**。
func (m *procdManager) writeDefinition(binPath, cfgPath, user string) error {
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
	return nil
}

// RewriteDefinition 只重写 procd 脚本与配置属主，不启动/不重启（没注册就别修）。
func (m *procdManager) RewriteDefinition(binPath, cfgPath, user string) error {
	if !exists(procdInitPath) {
		return ErrNotInstalled
	}
	return m.writeDefinition(binPath, cfgPath, user)
}

func (m *procdManager) Install(binPath, cfgPath, user string) error {
	if err := m.writeDefinition(binPath, cfgPath, user); err != nil {
		return err
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

const sysvInitPath = "/etc/init.d/xb-probe-agent"

type sysvManager struct{}

func (m *sysvManager) Name() string { return "sysv" }

// writeDefinition 建用户、交接配置属主、写 SysV init 脚本 —— **不含启动**。
func (m *sysvManager) writeDefinition(binPath, cfgPath, user string) error {
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
	return nil
}

// RewriteDefinition 只重写 init 脚本与配置属主，不启动/不重启（没注册就别修）。
func (m *sysvManager) RewriteDefinition(binPath, cfgPath, user string) error {
	if !exists(sysvInitPath) {
		return ErrNotInstalled
	}
	return m.writeDefinition(binPath, cfgPath, user)
}

func (m *sysvManager) Install(binPath, cfgPath, user string) error {
	if err := m.writeDefinition(binPath, cfgPath, user); err != nil {
		return err
	}
	// 各家自启命令不同，依次尝试，全都失败只警告（服务仍可手动 /etc/init.d/xb-probe-agent start）。
	for _, c := range [][]string{
		{"update-rc.d", "xb-probe-agent", "defaults"},
		{"chkconfig", "--add", "xb-probe-agent"},
		{"insserv", "xb-probe-agent"},
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
		{"update-rc.d", "-f", "xb-probe-agent", "remove"},
		{"chkconfig", "--del", "xb-probe-agent"},
		{"insserv", "-r", "xb-probe-agent"},
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
