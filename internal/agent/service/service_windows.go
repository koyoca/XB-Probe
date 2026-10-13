//go:build windows

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Windows 服务管理（12.4）。
//
// Agent 自身实现 svc.Handler（见 service_windows_run.go），因此不需要 nssm 之类的包装器；
// 安装时用 SCM 的 CreateService 注册，卸载时先停止再删除。
//
// 关键约束：正在运行的 exe 无法直接删除（Windows 会锁住文件），
// 所以卸载流程是「停服务 → 删服务 → 派生子进程延迟删除二进制」。
const windowsServiceName = "xb-probe-agent"

type windowsManager struct{}

func (m *windowsManager) Name() string { return "windows" }

// writeDefinition 更新已注册服务的"可执行路径 + 启动参数"。
//
// **不删除、不停止**正在运行的服务：删了重建（Install 的做法）会把服务停掉，
// 而升级路径要在"不动正在跑的服务"的前提下把新定义放好（见 Manager.RewriteDefinition）。
// 服务尚未注册时返回错误而不是顺手创建——这个入口是"修定义"，不是"装服务"。
func (m *windowsManager) writeDefinition(binPath, cfgPath string) error {
	mg, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务控制管理器失败（请以管理员身份运行）：%w", err)
	}
	defer mg.Disconnect()

	s, err := mg.OpenService(windowsServiceName)
	if err != nil {
		// 没注册过这个服务（容器 / 只前台 run 的机器）：**不要**顺手创建，
		// 那是 Install 的活，而且用户并没有要过（见 ErrNotInstalled）。
		return fmt.Errorf("%w（SCM 里没有 %s）：%v", ErrNotInstalled, windowsServiceName, err)
	}
	defer s.Close()

	c, err := s.Config()
	if err != nil {
		return fmt.Errorf("读取服务配置失败：%w", err)
	}
	exePath, err := filepath.Abs(binPath)
	if err != nil {
		exePath = binPath
	}
	cfgAbs, err := filepath.Abs(cfgPath)
	if err != nil {
		cfgAbs = cfgPath
	}
	// BinaryPathName 是一整条命令行字符串：含空格的路径必须各自加引号，
	// 否则 `C:\Program Files\...` 会被拆成两个参数，服务起来就是"找不到文件"。
	c.BinaryPathName = windows.EscapeArg(exePath) + " run --config " + windows.EscapeArg(cfgAbs)
	if err := s.UpdateConfig(c); err != nil {
		return fmt.Errorf("更新服务定义失败：%w", err)
	}
	return nil
}

// RewriteDefinition 只更新 SCM 登记项，不启动/不重启。
func (m *windowsManager) RewriteDefinition(binPath, cfgPath, user string) error {
	return m.writeDefinition(binPath, cfgPath)
}

// Install 注册并启动 Windows 服务。
//
// 用 LocalSystem 而不是 LocalService：Agent 需要 ICMP（raw socket）与读取网卡计数器，
// LocalService 在多数机器上会被拒绝（ICMP 需要管理员或 SeCreateGlobalPrivilege 之外的特权）；
// 但服务本身不做任何"写系统"动作，权限面仍然极小。
func (m *windowsManager) Install(binPath, cfgPath, user string) error {
	mg, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务控制管理器失败（请以管理员身份运行）：%w", err)
	}
	defer mg.Disconnect()

	// 已存在则先删掉，保证幂等（--force 路径会走到这里）。
	if s, err := mg.OpenService(windowsServiceName); err == nil {
		_, _ = s.Control(svc.Stop)
		_ = s.Delete()
		s.Close()
		time.Sleep(500 * time.Millisecond)
	}

	exePath, err := filepath.Abs(binPath)
	if err != nil {
		exePath = binPath
	}
	cfgAbs, err := filepath.Abs(cfgPath)
	if err != nil {
		cfgAbs = cfgPath
	}

	s, err := mg.CreateService(windowsServiceName, exePath, mgr.Config{
		DisplayName:  "XB-Probe Agent",
		Description:  "自用轻量级服务器探针 Agent（纯外联，不监听端口）",
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
	}, "run", "--config", cfgAbs)
	if err != nil {
		return fmt.Errorf("创建服务失败：%w", err)
	}
	defer s.Close()

	// 崩溃自动重启：前三次失败后各等 5 秒重试，之后每 60 秒一次。
	// 这是 Windows 上等价于 systemd `Restart=always` 的配置。
	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
	}, 60)

	if err := s.Start(); err != nil {
		// 启动失败不是致命错误：可能只是还没配好网络。把服务留在"已注册"状态即可。
		fmt.Fprintln(os.Stderr, "警告：服务已注册但启动失败（可稍后 `sc start xb-probe-agent`）：", err)
	}
	fmt.Printf("Windows 服务 %s 已注册（自动启动 + 崩溃重启）。\n", windowsServiceName)
	return nil
}

func (m *windowsManager) Uninstall(binPath, cfgPath string) error {
	mg, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务控制管理器失败（请以管理员身份运行）：%w", err)
	}
	defer mg.Disconnect()

	if s, err := mg.OpenService(windowsServiceName); err == nil {
		// 先请求停止，最多等 15 秒；超时就强杀进程树。
		if _, err := s.Control(svc.Stop); err == nil {
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				st, err := s.Query()
				if err != nil || st.State == svc.Stopped {
					break
				}
				time.Sleep(300 * time.Millisecond)
			}
		}
		if err := s.Delete(); err != nil {
			s.Close()
			return fmt.Errorf("删除服务失败：%w", err)
		}
		s.Close()
	}

	// 延迟删除二进制：当前进程（如果是被卸载的 Agent 自己）仍在运行，
	// 直接 os.Remove 会失败。派生一个 `cmd /c ping -n 3 127.0.0.1 & del <path>` 的子进程，
	// 它在 2 秒后执行删除——那时本进程已经退出。
	if binPath != "" {
		if err := delayDeleteFile(binPath); err != nil {
			fmt.Fprintln(os.Stderr, "警告：二进制将在下次重启后可手动删除：", err)
		}
	}
	return nil
}

// delayDeleteFile 派生一个延迟删除进程（Windows 无 POSIX 信号，只能这样绕开文件锁）。
func delayDeleteFile(path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil // 不存在就不用删
	}
	binDir := filepath.Dir(path)
	exe := filepath.Base(path)
	// 用 timeout 而不是 ping 做延迟（ping 在部分精简系统上不可用）。
	script := fmt.Sprintf(`timeout /t 3 /nobreak >nul & del /f /q "%s"`, exe)
	cmd := exec.Command("cmd", "/c", script)
	cmd.Dir = binDir
	cmd.SysProcAttr = hiddenWindow()
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// RunWindowsService 由 cmd/xb-probe-agent 在检测到以服务方式启动时调用。
// cfgPath 只用于日志/排障，run 收到 stop channel 后应优雅退出。
func RunWindowsService(cfgPath string, run func(cfgPath string, stop <-chan struct{}) error, _ <-chan struct{}) error {
	return svc.Run(windowsServiceName, &probeServiceHandler{cfgPath: cfgPath, run: run})
}

type probeServiceHandler struct {
	cfgPath string
	run     func(cfgPath string, stop <-chan struct{}) error
}

// Execute 实现 svc.Handler：把 SCM 的 Stop/Shutdown 请求翻译成一个 stop channel，
// 让主循环优雅退出（与 Linux 侧 SIGTERM 的处理等价）。
func (h *probeServiceHandler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	stop := make(chan struct{})
	done := make(chan error, 1)

	go func() { done <- h.run(h.cfgPath, stop) }()

	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			if err != nil {
				return true, 1
			}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				close(stop)
				// 等主循环收尾；超时 10 秒就直接返回，SCM 会强杀。
				select {
				case <-done:
				case <-time.After(10 * time.Second):
				}
				return false, 0
			}
		}
	}
}

// IsWindowsService 判断当前进程是否由 SCM 启动（用于区分前台 run 与服务 run）。
func IsWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// serviceStatusText 返回服务状态的中文描述（供 status 子命令输出）。
func serviceStatusText() string {
	mg, err := mgr.Connect()
	if err != nil {
		return "无法连接 SCM"
	}
	defer mg.Disconnect()
	s, err := mg.OpenService(windowsServiceName)
	if err != nil {
		return "未安装"
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return "查询失败"
	}
	switch st.State {
	case svc.Stopped:
		return "已停止"
	case svc.StartPending:
		return "正在启动"
	case svc.Running:
		return "运行中"
	case svc.StopPending:
		return "正在停止"
	}
	return "未知"
}

var _ = strings.TrimSpace

// Detect 返回 Windows 服务管理器（SCM）。
func Detect() Manager { return &windowsManager{} }
