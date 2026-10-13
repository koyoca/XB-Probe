// Package cli 实现 probe-agent 的命令行（文档 3.2 / 3.4 / 12.3）。
//
//	probe-agent run                                    前台运行（由服务管理器调用）
//	probe-agent install --master <URL> --code <注册码>  注册、写配置、创建服务并启动
//	probe-agent reenroll --code <注册码>                用新注册码换取新令牌
//	probe-agent upgrade [--check|--version X|--rollback] 拉取并替换自身（自升级）
//	probe-agent uninstall                              本机卸载（需 root）
//	probe-agent version                                显示版本
//
// 权限最小化：仅 install/uninstall 需要 root；服务以专用低权限用户运行（3.2）。
package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/yourorg/probe/internal/agent"
	"github.com/yourorg/probe/internal/agent/agentconfig"
	"github.com/yourorg/probe/internal/agent/client"
	"github.com/yourorg/probe/internal/agent/service"
	"github.com/yourorg/probe/internal/agent/updater"
	"github.com/yourorg/probe/internal/dist"
	"github.com/yourorg/probe/internal/model"
)

// Run 是命令入口。version 由 -ldflags 注入。
func Run(args []string, version string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:], version)
	case "install":
		return cmdInstall(args[1:], version)
	case "reenroll":
		return cmdReenroll(args[1:], version)
	case "upgrade":
		return cmdUpgrade(args[1:], version)
	case "uninstall":
		return cmdUninstall(args[1:])
	case "version", "-v", "--version":
		fmt.Println("probe-agent", version, runtime.GOOS+"/"+runtime.GOARCH, "(发布标签 "+dist.CurrentTag()+")")
		return 0
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "未知命令：%s\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `用法：probe-agent <命令> [参数]

命令：
  run          前台运行
  install      注册并安装为系统服务（需 root）
  reenroll     用新注册码换取新令牌（令牌丢失或轮换时使用）
  upgrade      从 Master 拉取新版本并替换自身（自升级）
  uninstall    本机卸载（需 root）
  version      显示版本

upgrade 参数：
  --check                只查询是否有新版本，不下载（适合放进定时任务做巡检）
  --version <版本号>      升级到指定版本（默认是最新；也用于显式降级/重装）
  --dry-run              下载并校验，但不替换
  --rollback             把上一次备份的二进制还原回来（升级后起不来时用）
  --target <路径>         要替换的可执行文件路径（默认取当前进程自身）
  --config <路径>         配置文件路径

install 参数：
  --master <URL>         Master 地址（必需，必须为 https）
  --code <注册码>         一次性注册码（必需；也可用环境变量 PROBE_CODE）
  --dir <目录>            自定义配置目录
  --no-service           只写配置，不注册系统服务（容器或手动前台运行）
  --spki-sha256 <指纹>    固定 Master 公钥指纹（自签证书场景，可重复）
  --force                已安装时重新注册并换取新令牌
  --insecure-http        仅本机回环开发场景放行 http://
`)
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

func cmdRun(args []string, version string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fs.String("config", agentconfig.DefaultPath(), "配置文件路径")
	// Windows 专用：显式以服务方式运行。一般不需要，因为 SCM 启动时会自动识别；
	// 保留该开关是为了在服务注册失败时手工验证"服务模式"能跑通。
	asService := fs.Bool("service", false, "以系统服务方式运行（Windows 由 SCM 自动识别，通常无需指定）")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := agentconfig.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}
	cli, err := client.New(client.Options{
		MasterURL:    cfg.MasterURL,
		Token:        cfg.Token,
		SPKISHA256:   cfg.SPKISHA256,
		InsecureHTTP: cfg.InsecureHTTP,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Windows 服务模式下必须交给 SCM：日志不能走 stdout（服务没有控制台），
	// 且必须响应 Stop/Shutdown 控制请求。这段逻辑放在 build-tag 文件里，
	// 其他平台上是空操作（runAsService 直接返回 false）。
	if runAsService(*cfgPath, *asService, func(ctx context.Context) error {
		d := agent.New(*cfgPath, cfg, cli, version)
		return d.Run(ctx)
	}) {
		return 0
	}

	d := agent.New(*cfgPath, cfg, cli, version)
	logger.Info("probe-agent 启动", "version", version, "master", cfg.MasterURL,
		"targets", len(cfg.Targets), "report_interval_s", cfg.ReportIntervalS)

	if err := d.Run(context.Background()); err != nil {
		logger.Error("运行结束", "err", err.Error())
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

func cmdInstall(args []string, version string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	master := fs.String("master", "", "Master 地址")
	code := fs.String("code", os.Getenv("PROBE_CODE"), "一次性注册码（或用环境变量 PROBE_CODE）")
	dir := fs.String("dir", "", "自定义配置目录")
	noService := fs.Bool("no-service", false, "只写配置，不注册系统服务")
	force := fs.Bool("force", false, "已安装时重新注册并换取新令牌")
	insecureHTTP := fs.Bool("insecure-http", false, "允许 http://（仅本机回环开发）")
	var spki multiFlag
	fs.Var(&spki, "spki-sha256", "固定 Master 公钥指纹（可重复）")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *master == "" || *code == "" {
		fmt.Fprintln(os.Stderr, "错误：--master 与 --code 均为必需")
		return 2
	}

	cfgPath, binPath, err := resolvePaths(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}

	// 幂等：已安装且未指定 --force 时只替换二进制并重启服务（12.3）
	if _, err := agentconfig.Load(cfgPath); err == nil && !*force {
		fmt.Println("检测到已安装配置，仅重启服务（如需换令牌请加 --force）。")
		if err := service.Detect().Uninstall(binPath, cfgPath); err != nil {
			fmt.Fprintln(os.Stderr, "清理旧服务时出错（继续）：", err)
		}
		if err := installService(binPath, cfgPath, *noService); err != nil {
			fmt.Fprintln(os.Stderr, "错误：", err)
			return 1
		}
		fmt.Println("已完成。")
		return 0
	}

	cli, err := client.New(client.Options{
		MasterURL: *master, SPKISHA256: spki, InsecureHTTP: *insecureHTTP,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := model.EnrollRequest{
		Code:           strings.ToUpper(strings.TrimSpace(*code)),
		Hostname:       hostname(),
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
		ServiceManager: service.Detect().Name(),
		CounterBits:    counterBits(),
		AgentVersion:   version,
	}
	resp, err := cli.Enroll(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "注册失败：", err)
		return 1
	}

	cfg := &agentconfig.Config{
		MasterURL:      *master,
		Token:          resp.Token,
		SPKISHA256:     spki,
		InsecureHTTP:   *insecureHTTP,
		ServiceManager: service.Detect().Name(),
	}
	cfg.ApplyServerConfig(resp.Config)

	if err := agentconfig.Save(cfgPath, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "写入配置失败：", err)
		return 1
	}
	fmt.Printf("已注册节点（node_id=%d），配置写入 %s（权限 0600）。\n", resp.NodeID, cfgPath)

	if err := installService(binPath, cfgPath, *noService); err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}
	fmt.Println("安装完成。")
	return 0
}

func installService(binPath, cfgPath string, noService bool) error {
	if noService {
		fmt.Println("已跳过服务注册（--no-service）。可前台运行：probe-agent run")
		return nil
	}
	// 记录安装清单（卸载时只删除清单内的路径，避免误删，S13）。
	manifest := filepath.Join(filepath.Dir(cfgPath), "install.manifest")
	_ = os.WriteFile(manifest, []byte(binPath+"\n"+cfgPath+"\n"), 0o600)
	return service.Detect().Install(binPath, cfgPath, "probe")
}

// ---------------------------------------------------------------------------
// reenroll
// ---------------------------------------------------------------------------

func cmdReenroll(args []string, version string) int {
	fs := flag.NewFlagSet("reenroll", flag.ContinueOnError)
	code := fs.String("code", os.Getenv("PROBE_CODE"), "新的一次性注册码")
	cfgPath := fs.String("config", agentconfig.DefaultPath(), "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *code == "" {
		fmt.Fprintln(os.Stderr, "错误：--code 必需")
		return 2
	}

	cfg, err := agentconfig.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}
	cli, err := client.New(client.Options{
		MasterURL: cfg.MasterURL, SPKISHA256: cfg.SPKISHA256, InsecureHTTP: cfg.InsecureHTTP,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := cli.Enroll(ctx, model.EnrollRequest{
		Code: strings.ToUpper(strings.TrimSpace(*code)), Hostname: hostname(),
		OS: runtime.GOOS, Arch: runtime.GOARCH,
		ServiceManager: cfg.ServiceManager, CounterBits: counterBits(), AgentVersion: version,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "换取新令牌失败：", err)
		return 1
	}
	cfg.Token = resp.Token
	cfg.ApplyServerConfig(resp.Config)
	if err := agentconfig.Save(*cfgPath, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "写入配置失败：", err)
		return 1
	}
	fmt.Println("已换取新令牌并更新配置。请重启服务使配置生效。")
	return 0
}

// ---------------------------------------------------------------------------
// uninstall
// ---------------------------------------------------------------------------

func cmdUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	cfgPath := fs.String("config", agentconfig.DefaultPath(), "配置文件路径")
	yes := fs.Bool("yes", false, "跳过确认")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := agentconfig.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}
	if !*yes {
		fmt.Printf("将卸载 probe-agent 并删除 %s。确认执行？[y/N] ", filepath.Dir(*cfgPath))
		var ans string
		_, _ = fmt.Scanln(&ans)
		if !strings.EqualFold(strings.TrimSpace(ans), "y") {
			fmt.Println("已取消。")
			return 0
		}
	}

	// 1) 尽力发送 deregister（失败不阻塞，超时 5 秒，3.4）
	cli, err := client.New(client.Options{
		MasterURL: cfg.MasterURL, Token: cfg.Token,
		SPKISHA256: cfg.SPKISHA256, InsecureHTTP: cfg.InsecureHTTP,
	})
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := cli.Deregister(ctx); err == nil {
			fmt.Println("已通知 Master 归档该节点。")
		} else {
			fmt.Println("未能通知 Master（网络问题）；Master 侧稍后会自动判离线，请管理员手动归档。")
		}
		cancel()
	}

	// 2) 停止并移除服务
	binPath, _ := readManifest(*cfgPath)
	if binPath == "" {
		binPath = filepath.Join(agentconfig.DefaultBinaryDir(), "probe-agent")
	}
	if err := service.Detect().Uninstall(binPath, *cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "移除服务时出错（继续清理）：", err)
	}

	// 3) 清理配置目录与二进制（只删除安装清单记录的内容，S13）
	dir := filepath.Dir(*cfgPath)
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "清理配置目录失败：", err)
	}
	_ = os.Remove(binPath)
	_ = os.Remove(filepath.Join(agentconfig.DefaultBinaryDir(), "probe-agent"))

	fmt.Println("卸载完成。")
	return 0
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// multiFlag 支持重复指定的字符串参数（如多个 SPKI 指纹）。
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func resolvePaths(dir string) (cfgPath, binPath string, err error) {
	if dir != "" {
		svc := service.Detect().Name()
		if svc == "systemd" && !isRoot() {
			return "", "", fmt.Errorf("安装为系统服务需要 root 权限")
		}
		return filepath.Join(dir, "agent.yaml"), filepath.Join(dir, "probe-agent"), nil
	}
	if !isRoot() && runtime.GOOS != "windows" {
		return "", "", fmt.Errorf("安装需要 root 权限（请使用 sudo）")
	}
	return agentconfig.DefaultPath(), filepath.Join(agentconfig.DefaultBinaryDir(), "probe-agent"), nil
}

func isRoot() bool { return os.Geteuid() == 0 }

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

// counterBits 上报计数器位宽，供 Master 判定 32 位回绕（12.5）。
func counterBits() int {
	if runtime.GOARCH == "386" || runtime.GOARCH == "arm" || runtime.GOARCH == "armv7" ||
		runtime.GOARCH == "armv6" || runtime.GOARCH == "mips" || runtime.GOARCH == "mipsle" {
		return 32
	}
	return 64
}

func readManifest(cfgPath string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(cfgPath), "install.manifest"))
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}

// cmdUpgrade 实现 `probe-agent upgrade`：从 Master 拉取新版本并替换自身。
//
// 替换成功后**不自行重启**：进程自我重启在 systemd / Windows 服务 / 容器里各有各的坑，
// 统一交给服务管理器（Restart=always / SCM 失败恢复）或运维执行重启命令，
// 这里只负责把该执行的命令打印清楚。
func cmdUpgrade(args []string, version string) int {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	cfgPath := fs.String("config", agentconfig.DefaultPath(), "配置文件路径")
	check := fs.Bool("check", false, "只查询是否有新版本，不下载")
	pin := fs.String("version", "", "升级到指定版本（默认最新；也可用于显式降级）")
	dry := fs.Bool("dry-run", false, "下载并校验，但不替换")
	rollback := fs.Bool("rollback", false, "把上一次备份的二进制还原回来")
	target := fs.String("target", "", "要替换的可执行文件路径（默认取当前进程的可执行文件）")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	exe := *target
	if exe == "" {
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "错误：无法确定自身路径：", err)
			return 1
		}
		exe = self
	}

	if *rollback {
		if err := updater.Rollback(exe); err != nil {
			fmt.Fprintln(os.Stderr, "回滚失败：", err)
			return 1
		}
		fmt.Println("已还原为上一版本。", restartHint())
		return 0
	}

	cfg, err := agentconfig.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：读取配置失败：", err)
		return 1
	}
	if cfg.MasterURL == "" || cfg.Token == "" {
		fmt.Fprintln(os.Stderr, "错误：配置里没有 Master 地址或令牌，请先执行 install")
		return 1
	}
	c, err := client.New(client.Options{
		MasterURL: cfg.MasterURL, Token: cfg.Token,
		SPKISHA256: cfg.SPKISHA256, InsecureHTTP: cfg.InsecureHTTP,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}

	// 下载整份二进制可能很慢，给足超时；--check 单独用短超时（它只是查一次）。
	timeout := 5 * time.Minute
	if *check {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	platform := dist.CurrentTag()
	rel, err := updater.Check(ctx, c, platform, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "查询版本失败：", err)
		return 1
	}
	if *check {
		if *pin == "" && rel.UpToDate {
			fmt.Printf("已是最新：%s（%s）\n", rel.Version, rel.Artifact)
			return 0
		}
		fmt.Printf("可升级：%s -> %s（%s，%d 字节）\n", version, rel.Version, rel.Artifact, rel.Size)
		return 0
	}

	res, err := updater.Upgrade(ctx, c, updater.Options{
		Platform: platform, Current: version, Version: *pin, Target: exe, DryRun: *dry,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "升级失败：", err)
		return 1
	}
	if res.NoChange {
		fmt.Printf("已是最新：%s，无需升级\n", res.To)
		return 0
	}
	if *dry {
		fmt.Printf("dry-run：%s 下载并校验通过（%d 字节），未替换\n", res.To, res.Size)
		return 0
	}
	fmt.Printf("已升级 %s -> %s（%d 字节）\n备份：%s\n%s\n",
		res.From, res.To, res.Size, res.Backup, restartHint())
	return 0
}

// restartHint 给出"让新版本生效"该执行的命令。
//
// 按服务管理器给命令而不是笼统一句"请重启"：升级后最常见的疑问就是
// "为什么版本号没变"——多数情况是没重启，而命令就在手边能省一次翻文档。
func restartHint() string {
	switch {
	case runtime.GOOS == "windows":
		return "请重启服务使新版本生效：Restart-Service probe-agent"
	default:
		switch service.Detect().Name() {
		case "systemd":
			return "请重启服务使新版本生效：systemctl restart probe-agent"
		case "openrc":
			return "请重启服务使新版本生效：rc-service probe-agent restart"
		case "procd":
			return "请重启服务使新版本生效：service probe-agent restart"
		case "sysv", "rcd":
			return "请重启服务使新版本生效：service probe-agent restart"
		}
	}
	return "请重启 probe-agent 进程使新版本生效"
}
