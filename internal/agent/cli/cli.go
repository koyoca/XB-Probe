// Package cli 实现 xb-probe-agent 的命令行（文档 3.2 / 3.4 / 12.3）。
//
//	xb-probe-agent run                                    前台运行（由服务管理器调用）
//	xb-probe-agent install --master <URL> --code <注册码>  注册、写配置、创建服务并启动
//	xb-probe-agent reenroll --code <注册码>                用新注册码换取新令牌
//	xb-probe-agent upgrade [--check|--version X|--rollback] 拉取并替换自身（自升级）
//	xb-probe-agent uninstall                              本机卸载（需 root）
//	xb-probe-agent version                                显示版本
//
// 权限最小化：仅 install/uninstall 需要 root；服务以专用低权限用户运行（3.2）。
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/koyoca/xb-probe/internal/agent"
	"github.com/koyoca/xb-probe/internal/agent/agentconfig"
	"github.com/koyoca/xb-probe/internal/agent/client"
	"github.com/koyoca/xb-probe/internal/agent/service"
	"github.com/koyoca/xb-probe/internal/agent/updater"
	"github.com/koyoca/xb-probe/internal/dist"
	"github.com/koyoca/xb-probe/internal/model"
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
		fmt.Println("xb-probe-agent", version, runtime.GOOS+"/"+runtime.GOARCH, "(发布标签 "+dist.CurrentTag()+")")
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
	fmt.Fprint(os.Stderr, `用法：xb-probe-agent <命令> [参数]

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
  --dry-run              下载并校验，但不替换（服务定义也不动）
  --rollback             把上一次备份的二进制还原回来（升级后起不来时用）
  --target <路径>         要替换的可执行文件路径（默认取当前进程自身）
  --config <路径>         配置文件路径
  --no-service-repair    不重写服务定义、不交接配置属主

  升级成功后会自动按**当前版本**重写服务定义并把配置交给服务用户（不重启服务）：
  老节点（更早版本装的）unit 里没有配置目录的写权限、配置属主还是 root，
  只换二进制的话重启后会报「读取配置失败: permission denied」。

install 参数：
  --master <URL>         Master 地址（必需，必须为 https）
  --code <注册码>         一次性注册码（必需；也可用环境变量 PROBE_CODE）
  --dir <目录>            自定义配置目录
  --no-service           只写配置，不注册系统服务（容器或手动前台运行）
  --service-only         只按当前版本重写系统服务（unit/init 脚本 + 配置属主）并重启，
                         不联系 Master、不换令牌（**不需要 --master/--code**）
  --spki-sha256 <指纹>    固定 Master 公钥指纹（自签证书场景，可重复）
  --public-ip-url <URL>   公网地址探测服务（如 https://api.ipify.org）。默认关闭。
                          显式传空串（--public-ip-url ""）可关掉已配置的探测。
  --force                已安装时强制重新注册并换取新令牌（也可直接重跑时附带 --code 换令牌）
  --insecure-http        仅本机回环开发场景放行 http://

  关于 --public-ip-url：Master 与 Agent 同机部署时，Master 看到的来源地址是
  127.0.0.1，地址栏里就永远是回环地址；此时只有节点自己出网问一句才能拿到真正的
  公网地址。打开它会**定期访问第三方服务**，是否启用由你决定。
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
	logger.Info("xb-probe-agent 启动", "version", version, "master", cfg.MasterURL,
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
	// 第一件事：收拾改名（probe-* → xb-probe-*）之前留下的旧服务定义与配置目录。
	// 必须在读取配置之前 —— 老机器上的配置在旧目录里，先搬过来，下面的
	// "已安装"幂等分支才能认出来（否则会当成全新安装，白白换一次令牌）。
	service.MigrateLegacy()

	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	master := fs.String("master", "", "Master 地址")
	code := fs.String("code", os.Getenv("PROBE_CODE"), "一次性注册码（或用环境变量 PROBE_CODE）")
	dir := fs.String("dir", "", "自定义配置目录")
	noService := fs.Bool("no-service", false, "只写配置，不注册系统服务")
	serviceOnly := fs.Bool("service-only", false, "只重写系统服务（unit/init 脚本与配置属主）并重启，不注册、不换令牌")
	force := fs.Bool("force", false, "已安装时重新注册并换取新令牌")
	insecureHTTP := fs.Bool("insecure-http", false, "允许 http://（仅本机回环开发）")
	publicIPURL := fs.String("public-ip-url", "", "公网地址探测服务地址（留空=关闭；给了就在已安装的节点上就地更新）")
	var spki multiFlag
	fs.Var(&spki, "spki-sha256", "固定 Master 公钥指纹（可重复）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// "是否显式给了 --public-ip-url" 必须与"它的值"分开记。
	// 只看值的话，`--public-ip-url ""` 与"没给这个参数"无法区分，
	// 而这两件事的意思正好相反：前者是"关掉探测"，后者是"别动我的设置"。
	publicIPGiven := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "public-ip-url" {
			publicIPGiven = true
		}
	})

	// --service-only：只按**当前二进制里的模板**重写服务定义并重启，不联系 Master、不换令牌。
	//
	// 为什么需要它：服务定义（systemd unit / init 脚本）是嵌在二进制里的模板渲染出来的，
	// 所以"unit 模板升级"必须拿着新二进制重跑一遍安装流程；而在此之前只有两条路，
	// 都不通：`--force` 要重新注册（得先去后台生成一个新注册码，而这个码根本不会被用到），
	// 或者手工去改 /etc/systemd/system/xb-probe-agent.service。
	// 真机上就是这么卡住的——老节点 unit 里没有 ReadWritePaths、配置属主是 root，
	// 服务起不来，而用户手里没有"只修服务定义"这条命令。
	//
	// 与下面那条幂等分支（已安装且未 --force）的关系：那条是"重跑安装命令"的兼容路径，
	// 要求带 --master/--code（哪怕不注册）；这一条是它的**显式形式**，不需要注册码。
	if *serviceOnly {
		cfgPath, binPath, err := resolvePaths(*dir, *noService)
		if err != nil {
			fmt.Fprintln(os.Stderr, "错误：", err)
			return 1
		}
		if _, err := agentconfig.Load(cfgPath); err != nil {
			fmt.Fprintf(os.Stderr,
				"错误：%s 处没有可用配置，--service-only 只重装服务、不会注册节点（首次安装请去掉它）：%v\n",
				cfgPath, err)
			return 1
		}
		if err := service.Detect().Uninstall(binPath, cfgPath); err != nil {
			fmt.Fprintln(os.Stderr, "清理旧服务时出错（继续）：", err)
		}
		if err := installService(binPath, cfgPath, *noService); err != nil {
			fmt.Fprintln(os.Stderr, "错误：", err)
			return 1
		}
		fmt.Println("已完成：服务定义已按当前版本重写（配置与令牌未改动）。")
		return 0
	}

	if *master == "" || *code == "" {
		fmt.Fprintln(os.Stderr, "错误：--master 与 --code 均为必需")
		return 2
	}

	cfgPath, binPath, err := resolvePaths(*dir, *noService)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}
	// 先尝试读取已安装配置——下面幂等分支与重注册分支都要用到它。
	cur, curErr := agentconfig.Load(cfgPath)

	// 幂等：已安装、未给 --force、也未给 --code 时，只重装服务、复用旧令牌（12.3）。
	// 给了 --code 就必须用注册码换新令牌（哪怕没给 --force）：否则用户换了注册码重跑
	// 安装命令，旧令牌若已被吊销就永远 token_revoked 注册不上（#249）。
	// 只想重写服务定义、连 --master/--code 都不想给时，用 --service-only（等价且更明确）。
	if curErr == nil && !*force && *code == "" {
		fmt.Println("检测到已安装配置，仅重装系统服务（不换令牌；如需换令牌请用 --force，或重跑时附带 --code）。")
		// 显式给了 --public-ip-url 就落到本地配置里：这条路径是**唯一**能让
		// 已经装好的节点打开公网地址探测的办法（否则只能手工改 agent.yaml）。
		// 没给就一个字都不动 —— 这个开关的语义是"我明确要设置它"，
		// 不是"顺手把别人的设置重置成默认值"。
		if publicIPGiven {
			cur.PublicIPURL = strings.TrimSpace(*publicIPURL)
			if err := agentconfig.Save(cfgPath, cur); err != nil {
				fmt.Fprintln(os.Stderr, "错误：写入配置失败：", err)
				return 1
			}
			if cur.PublicIPURL == "" {
				fmt.Println("已关闭公网地址探测（public_ip_url 置空）。")
			} else {
				fmt.Printf("已设置 public_ip_url=%s（探测间隔 %d 秒）。\n",
					cur.PublicIPURL, cur.PublicIPIntervalS)
			}
		}
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
	// 保留已安装节点的本地设置（公网地址探测等）：显式给了 --public-ip-url 才覆盖，
	// 没给就沿袭旧配置——否则重注册会把用户设过的 public_ip_url 清掉（#249）。
	if publicIPGiven {
		cfg.PublicIPURL = strings.TrimSpace(*publicIPURL)
	} else if cur != nil {
		cfg.PublicIPURL = cur.PublicIPURL
		cfg.PublicIPIntervalS = cur.PublicIPIntervalS
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
		fmt.Println("已跳过服务注册（--no-service）。可前台运行：xb-probe-agent run")
		return nil
	}
	// 记录安装清单（卸载时只删除清单内的路径，避免误删，S13）。
	manifest := filepath.Join(filepath.Dir(cfgPath), service.ManifestName)
	_ = os.WriteFile(manifest, []byte(binPath+"\n"+cfgPath+"\n"), 0o600)
	// 配置属主的交接在 Install 内部完成（建好用户之后、启动服务之前，见 service/handover.go）：
	// 这里刚写的 manifest 也是 root:root 0600，一并交过去，卸载时不必区分是谁写的。
	err := service.Detect().Install(binPath, cfgPath, service.DefaultUser)
	if errors.Is(err, service.ErrNoManager) {
		// 这里**不是**安装失败：配置写好了、节点注册了、二进制装好了，
		// 只是这台机器上没有可用的服务管理器（精简容器镜像常见）。
		//
		// 以前这条路由分发给节点的 install.sh 用 `cat /proc/1/comm` 去猜，
		// 猜错（Alpine 的 PID 1 是 busybox init、容器里更是什么都有）就把
		// --no-service 硬塞进来，"安装"看上去成功、服务从来没注册过 ——
		// 用户以为是权限问题，实际是脚本替他做了个不必要的决定。
		// 现在把这个判断交还给**真正知道答案的那一端**（agent 自己的 Detect）。
		fmt.Printf("\n本机没有识别到可用的服务管理器（%v）。\n", err)
		printForegroundHint(binPath, cfgPath)
		return nil
	}
	return err
}

// printForegroundHint 给出"没有 init 系统时怎么让它常驻"的可照做命令。
//
// 不写一句"请自行托管"就完事：容器用户真正需要的是**确切的命令行**，
// 以及"重启容器后它不会自己回来"这个事实。
func printForegroundHint(binPath, cfgPath string) {
	fmt.Printf("配置与令牌都已就位，节点已注册；用下面任一种方式让它常驻：\n"+
		"  · 前台/自建守护：%s run --config %s\n"+
		"  · 容器：把上面这行作为 entrypoint 或写成 Supervisor/s6 的配置\n"+
		"注意：这种方式在重启后不会自动拉起。\n", binPath, cfgPath)
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
	// 配置是**以 root 身份**重写的（reenroll 必须 sudo），而服务以专用低权限用户跑：
	// 不把属主交回去，这次重写就会在下次重启时变成 `permission denied` ——
	// 与"配置写坏了"同样的症状，却发生在一次"成功"的令牌轮换之后。
	// 与 install 走同一段交接逻辑，不另写一份。
	if err := service.HandoverToUser(*cfgPath, service.DefaultUser); err != nil {
		fmt.Fprintln(os.Stderr, "警告：未能把配置交给服务用户，服务重启后可能读不到配置：", err)
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
		fmt.Printf("将卸载 xb-probe-agent 并删除 %s。确认执行？[y/N] ", filepath.Dir(*cfgPath))
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
		binPath = filepath.Join(agentconfig.DefaultBinaryDir(), "xb-probe-agent")
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
	_ = os.Remove(filepath.Join(agentconfig.DefaultBinaryDir(), "xb-probe-agent"))

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

// needsRootForExplicitDir 判定"把 Agent 装进一个用户显式指定的目录（--dir）"是否需要 root。
//
// 抽成纯函数、并且把环境事实（是否 root、init 是不是 systemd）当参数传进来，
// 有两个理由：
//  1. 这条规则**只在 Linux 非 root 上才会被触发**。原地写条件就等于"只有 CI 验得到"，
//     本机（Windows）永远测不出问题——它确实在 CI 上红过一次。
//  2. 不读环境才能用一张真值表把它钉死（见 cli_test.go 的 TestExplicitDirRootRuleTruthTable）。
//
// 关键是 noService：**只有注册系统服务才需要 root**，而 --no-service 明确表示不注册
// （容器 / 无 init / 只前台运行；install.sh 会按 /proc/1/comm 自动加它）。
// 早先的实现只看 init 名字、完全不理 --no-service，于是
// `xb-probe-agent install --no-service --dir <可写目录>` 在 Linux 非 root 下被误判成
// "安装为系统服务需要 root 权限"而直接失败——正是它自己的帮助文本承诺的用法。
func needsRootForExplicitDir(noService bool, initName string, root bool) bool {
	if noService {
		return false
	}
	return initName == "systemd" && !root
}

func resolvePaths(dir string, noService bool) (cfgPath, binPath string, err error) {
	if dir != "" {
		if needsRootForExplicitDir(noService, service.Detect().Name(), isRoot()) {
			return "", "", fmt.Errorf("安装为系统服务需要 root 权限（或加 --no-service 只放文件）")
		}
		return filepath.Join(dir, "agent.yaml"), filepath.Join(dir, "xb-probe-agent"), nil
	}
	// 缺省路径是系统目录（/usr/local/bin、/etc/xb-probe-agent）：能不能写进去本身
	// 就取决于权限，与是否注册服务无关，所以这条不看 noService。
	if !isRoot() && runtime.GOOS != "windows" {
		return "", "", fmt.Errorf("安装需要 root 权限（请使用 sudo，或用 --dir 指定可写目录）")
	}
	return agentconfig.DefaultPath(), filepath.Join(agentconfig.DefaultBinaryDir(), "xb-probe-agent"), nil
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
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(cfgPath), service.ManifestName))
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}

// cmdUpgrade 实现 `xb-probe-agent upgrade`：从 Master 拉取新版本并替换自身。
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
	noRepair := fs.Bool("no-service-repair", false,
		"不重写服务定义、不交接配置属主（容器里没有服务管理器、或 unit 由自己托管时用）")
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
	if *dry {
		// dry-run 一个字节都不写：服务定义与配置属主同样不动。
		if res.NoChange {
			fmt.Printf("已是最新：%s，无需升级\n", res.To)
		} else {
			fmt.Printf("dry-run：%s 下载并校验通过（%d 字节），未替换\n", res.To, res.Size)
		}
		fmt.Println("dry-run：未改动系统（服务定义与配置属主也不动）。")
		return 0
	}

	// 二进制已经确定留在 exe 这个位置了，接着把**上一次安装留下的**服务定义与
	// 配置属主补齐 —— 见 repairAfterUpgrade 的说明。
	//
	// 注意这里在"已是最新"的分支**之前**：老节点最常见的状态恰恰是
	// "二进制已经升级过、但 unit 与配置属主还是更早那次安装的"。若把修复放在
	// "真的换了二进制"之后，那台机器就会一直停在"已是最新，无需升级"，
	// 而它恰恰是最需要修的那台。
	//
	// canRepair：重写 /etc 下的 unit、把文件的属主交给 probe 都需要 root。
	// Windows 上不在这里拦——它连 SCM 都连不上时会自己报"请以管理员身份运行"。
	canRepair := isRoot() || runtime.GOOS == "windows"
	repairNote := repairAfterUpgrade(service.Detect(), *cfgPath, exe, canRepair, *target != "", *noRepair)

	if res.NoChange {
		fmt.Printf("已是最新：%s，无需升级\n", res.To)
		fmt.Println(repairNote)
		return 0
	}
	fmt.Printf("已升级 %s -> %s（%d 字节）\n备份：%s\n%s\n",
		res.From, res.To, res.Size, res.Backup, restartHint())
	fmt.Println(repairNote)
	return 0
}

// repairAfterUpgrade 在升级之后按**当前二进制里的模板**重写服务定义，并把配置
// 属主交给服务用户（属主交接在 RewriteDefinition 内部，与 install 走同一段逻辑）。
//
// 为什么升级必须做这件事：`upgrade` 只换二进制，而节点上的 unit/init 脚本与配置
// 属主都是**更早那次安装**留下的。真机事故就是这么来的 ——
//
//	v0.1.1 装的节点：unit 里没有 ReadWritePaths=<配置目录>（ProtectSystem=strict
//	下配置写不回去），配置还是 0600 root:root（unit 里 User=probe 一启动就读不到）。
//	升级到 v0.1.2 并重启后：「读取配置失败: /etc/xb-probe-agent/agent.yaml: permission denied」
//	——换二进制修不了这两样，用户看到的是"升级了反而起不来"。
//
// **不启动也不重启服务**（重启由用户/服务管理器执行，见 cmdUpgrade 的约定）；
// 这里只让新定义在下次启动时生效。失败只警告，不改升级的结论。
//
// 每个"要不要做"的判据都做成参数（而不是在这里读全局/环境）：这几条分支
// 恰好是本轮事故的边界——"已是最新"、"没给 root"、"指定了 --target"——
// 只有在任意平台上都能直接断言，才谈得上被测到。
func repairAfterUpgrade(m service.Manager, cfgPath, binPath string, canRepair, customTarget, skip bool) string {
	if skip {
		return "提示：--no-service-repair，未重写服务定义与配置属主；" +
			"若服务重启后报配置 permission denied，执行：sudo xb-probe-agent install --service-only"
	}
	if customTarget {
		// --target 是"替换另一个路径上的二进制"（容器/离线场景），那种场合的服务
		// 定义不归本命令管：按 --target 去改 unit 会把它指到一个临时路径上。
		return "提示：指定了 --target，未改动服务定义；" +
			"若需要按当前版本重写服务定义，执行：sudo xb-probe-agent install --service-only"
	}
	if !canRepair {
		return "提示：当前不是 root，未重写服务定义与配置属主（交接属主需要 root）；" +
			"请执行：sudo xb-probe-agent install --service-only"
	}
	if err := repairServiceDefinition(m, cfgPath, binPath); err != nil {
		// "这台机器没把 Agent 装成服务"不是失败：容器 / --no-service 跑的节点
		// 本来就没有定义可改。当成"警告"发出去，用户会去查一个不存在的问题。
		if errors.Is(err, service.ErrNotInstalled) {
			return "提示：本机没有把 Agent 注册成系统服务，跳过服务定义重写" +
				"（容器或 --no-service 场景属正常）。"
		}
		return "警告：重写服务定义/交接配置属主失败：" + err.Error() +
			"\n      可以手工执行：sudo xb-probe-agent install --service-only"
	}
	return "已按当前版本重写服务定义（含配置目录的写权限）并把配置交给服务用户，" +
		"重启后生效（不会自行重启）。"
}

// repairServiceDefinition 是本包对 service.Manager 的唯一一处"修复"调用点。
//
// 把 Manager 作为参数传进来（而不是在里面直接 service.Detect()）是为了让测试能塞一个
// 记录调用的假实现进去：否则"升级到底有没有修"这件事只能靠读源码相信。
func repairServiceDefinition(m service.Manager, cfgPath, binPath string) error {
	return m.RewriteDefinition(binPath, cfgPath, service.DefaultUser)
}

// restartHint 给出"让新版本生效"该执行的命令。
//
// 按服务管理器给命令而不是笼统一句"请重启"：升级后最常见的疑问就是
// "为什么版本号没变"——多数情况是没重启，而命令就在手边能省一次翻文档。
func restartHint() string {
	switch {
	case runtime.GOOS == "windows":
		return "请重启服务使新版本生效：Restart-Service xb-probe-agent"
	default:
		switch service.Detect().Name() {
		case "systemd":
			return "请重启服务使新版本生效：systemctl restart xb-probe-agent"
		case "openrc":
			return "请重启服务使新版本生效：rc-service xb-probe-agent restart"
		case "procd":
			return "请重启服务使新版本生效：service xb-probe-agent restart"
		case "sysv", "rcd":
			return "请重启服务使新版本生效：service xb-probe-agent restart"
		}
	}
	return "请重启 xb-probe-agent 进程使新版本生效"
}
