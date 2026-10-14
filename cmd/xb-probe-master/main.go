// xb-probe-master 是 XB-Probe 的中枢服务（文档第 4 章 / 第 10 章）。
//
// 启动顺序：加载配置 → 打开数据库并迁移 → 构建依赖 → 启动写协程与调度器
//
//	→ 启动公开站与管理端（可分离监听）。
//
// 单二进制：内嵌默认主题、迁移脚本与安装脚本（11. 分发形态）。
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/koyoca/xb-probe/internal/alert"
	"github.com/koyoca/xb-probe/internal/api"
	"github.com/koyoca/xb-probe/internal/auth"
	"github.com/koyoca/xb-probe/internal/config"
	"github.com/koyoca/xb-probe/internal/db"
	"github.com/koyoca/xb-probe/internal/pipeline"
	"github.com/koyoca/xb-probe/internal/state"
	"github.com/koyoca/xb-probe/internal/store"
	"github.com/koyoca/xb-probe/internal/theme"
	"github.com/koyoca/xb-probe/internal/tlsconf"
	"github.com/koyoca/xb-probe/internal/traffic"

	// 空导入：把 IANA 时区库编译进来。没有它时 time.LoadLocation 在
	// Windows 与精简容器（Alpine/distroless）上会失败，直接导致计费的
	// tz 校验 400、以及"今日流量"静默落到错误时区。详见包内注释。
	_ "github.com/koyoca/xb-probe/internal/tzdata"
)

// version 由 -ldflags "-X main.version=..." 注入。
var version = "dev"

// mainUsage：顶层用法。存在的理由见 main() 里 default 分支的注释——
// 未知命令曾经会 fall through 去启动服务，这里至少要让"打错命令"看得见。
const mainUsage = `xb-probe-master —— XB-Probe 中枢服务

用法：
  xb-probe-master -config <配置文件>        启动服务（默认 /etc/xb-probe-master/master.yaml）
  xb-probe-master init-admin  -config <文件> -username <名> -password <口令>
  xb-probe-master reset-admin -config <文件> -username <名> -password <口令>
  xb-probe-master tls status  -config <文件> [-wait N] [-json]
  xb-probe-master dist <子命令>
  xb-probe-master version

无参数启动时按 -config 解析，配置文件路径缺省为 /etc/xb-probe-master/master.yaml。
`

func main() {
	os.Exit(runMain(os.Args))
}

// runMain：顶层分派，返回值就是退出码。
//
// 从 main() 里抽出来是为了让"未知命令必须被拒绝"这件事**可以被测试**：
// 直接 os.Exit 的版本在测试里没法断言退出码。
func runMain(args []string) int {
	if len(args) > 1 {
		switch args[1] {
		case "init-admin":
			if err := runInitAdmin(args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "错误：", err)
				return 1
			}
			return 0
		case "reset-admin":
			if err := runResetAdmin(args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "错误：", err)
				return 1
			}
			return 0
		case "dist":
			if err := runDist(args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "错误：", err)
				return 1
			}
			return 0
		case "tls":
			// tls 子命令自己打印完整报告，并用退出码表达三种结局
			// （0 就绪 / 1 失败 / 2 尚无结果）。失败与"还没结果"都不再
			// 重复打印一遍错误消息——报告里已经写清楚了。
			err := runTLS(args[2:])
			switch {
			case err == nil:
			case errors.Is(err, errCertFailed):
				return 1
			case errors.Is(err, errCertPending):
				return 2
			default:
				fmt.Fprintln(os.Stderr, "错误：", err)
				return 1
			}
			return 0
		case "version", "-v", "--version":
			fmt.Println("xb-probe-master", version)
			return 0
		default:
			// 未知命令必须**明确拒绝**，绝不能 fall through 到下面的服务启动。
			//
			// 曾经这里没有 default，于是 `xb-probe-master tls status -config X` 这样的
			// 命令在**旧二进制**上（还没那个子命令时）会一路落到 flag.Parse + run()：
			//   · flag.Parse 遇到第一个非 flag 参数（`tls`）就停止解析，
			//     于是 `-config X` 被**整个忽略**，读的是默认路径的配置文件；
			//   · 结果是一个"要查询状态"的命令**把服务真的启动起来了**。
			// 这个组合坑得很深：部署脚本用 `tls status -config <临时文件>` 探测
			// "二进制认不认识某个配置键"，而它两次调用读的都是**同一份**真实配置，
			// 于是探测恒回答"认识"，脚本写下二进制不认识的键，服务随即拒绝启动。
			// 用户看到的现象是"脚本说部署完成/回滚了，但错误一直不变"。
			//
			// 只拦非 flag 的参数：`-config X` 这类仍是启动服务的正常写法。
			if !strings.HasPrefix(args[1], "-") {
				fmt.Fprintf(os.Stderr, "错误：未知命令 %q\n\n", args[1])
				fmt.Fprint(os.Stderr, mainUsage)
				return 2
			}
		}
	}

	// 用**自己的** FlagSet 解析 args，而不是全局的 flag.Parse()：
	// 后者解析的是 os.Args，与这里分派用的 args 只是"恰好相同"。一旦不同
	// （测试、或将来把分派嵌进别处），`-config` 就会被**整个忽略**而毫无征兆——
	// 这正是"未知命令 fall through"那次事故里的第二种失效方式，别再留一个。
	fs := flag.NewFlagSet("xb-probe-master", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/xb-probe-master/master.yaml", "配置文件路径")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if err := run(*cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		return 1
	}
	return 0
}

func run(cfgPath string) error {
	cfg, err := config.LoadMaster(cfgPath)
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	// 1) 数据库：WAL + 单写连接；启动时 quick_check（13.4.8）
	credentials, err := db.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	if err := credentials.Migrate(); err != nil {
		return fmt.Errorf("数据库迁移失败: %w", err)
	}
	repo := store.New(credentials)

	// 1.5) 一次性修复：清掉"自动"节点上那些从来不是公网地址的历史值。
	//
	// 老实现把上报连接的对端地址无条件写进 nodes.ip_v4，于是同机部署的节点
	// 地址栏里存着 127.0.0.1 —— 而新规则不再写这种值，也就没人会去覆盖它，
	// 它会**永远**留在那（用户看到的就是"改了还是 127.0.0.1"）。
	// 这里用与写入路径同一份判定（internal/netutil）清一次；失败不阻塞启动。
	if n, err := repo.ClearNonPublicObservedIPs(); err != nil {
		logger.Warn("清理历史节点地址失败（不影响启动）", "err", err.Error())
	} else if n > 0 {
		logger.Info("已清理不是公网地址的历史节点地址", "rows", n)
	}

	// 2) 主密钥：加密告警渠道凭证（9.4）
	keyring, err := auth.LoadOrCreateKeyring(cfg.MasterKeyFile)
	if err != nil {
		return err
	}

	// 3) 内存运行态与公开快照（A9）
	reportInterval := int(repo.GetIntSetting("report_interval_s", 3))
	ringCap := 30 * 60 / maxInt(reportInterval, 2)
	registry := state.NewRegistry(ringCap)

	// 4) 写管线：单写协程 + 多分辨率聚合（4.2）
	writer, err := pipeline.NewWriter(credentials.Write, pipeline.DefaultQueueSize, nil,
		repo.GetIntSetting("disk_min_free_mb", 200))
	if err != nil {
		return err
	}
	multi := pipeline.NewMultiResolution(int(repo.GetIntSetting("raw_resolution_s", 10)),
		func(table string, nodeID int64, row pipeline.MetricsRow) {
			r := row
			writer.Enqueue(pipeline.Row{Table: table, NodeID: nodeID, Metrics: &r})
		})
	latencyAgg := pipeline.NewLatencyMultiResolution(int(repo.GetIntSetting("probe_interval_s", 30)),
		func(table string, nodeID int64, row pipeline.LatencyRow) {
			r := row
			writer.Enqueue(pipeline.Row{Table: table, NodeID: nodeID, Latency: &r})
		})

	// 5) 令牌管理器（8.2）
	tokens := auth.NewTokenManager(repo)

	// 6) 流量跟踪器（4.3 / 13.4.1）
	tracker := traffic.NewTracker(repo, func(nodeID int64) (int, string, bool) {
		b, err := repo.GetBilling(nodeID)
		if err != nil || b == nil {
			return 1, "", false
		}
		return b.ResetDay, b.TZ, true
	})

	// 7) 主题（5.3）
	themes, err := theme.NewManager(cfg.ThemesDir)
	if err != nil {
		return err
	}
	if active := repo.GetSettingDefault("theme.active", "builtin"); themes.Exists(active) {
		_ = themes.SetActive(active)
	}

	// 8) 告警引擎（4.5）
	rules, err := repo.LoadAlertRules()
	if err != nil {
		return err
	}
	// 渠道提供者必须"每次分发现取"，不能在这里调一次 loadChannels 把结果拍下来：
	// 用户在后台新建/修改 Tg 渠道后，冻结的那份清单里没有它，告警会一条都发不出去
	// （事件照样落库、delivered=0、日志无报错），而"测试发送"按钮走的是另一条路、
	// 现读设置，所以测试是通的 —— 故障现场因此看起来完全不合理。
	alerts := alert.NewDynamic(rules, repo, func() []alert.Channel {
		return loadChannels(repo, keyring, logger)
	})
	alerts.SetNodeNameResolver(func(nodeID int64) string {
		if n, err := repo.GetNode(nodeID); err == nil && n != nil {
			return n.Name
		}
		return fmt.Sprintf("node#%d", nodeID)
	})

	// 9) 公开快照构建器（A9）
	// 字段映射使用 state 内的唯一实现（state.BuildPublicNode / BuildPublicOverview），
	// 无需注入，避免漏接线导致快照静默变空。
	snap := state.NewSnapshotter(registry)

	// 10) 调度器
	//
	// 提前到接口层之前构造：因为后台的"外部心跳"状态卡需要一个
	// model.HeartbeatReader（由调度器实现）注入 Deps。调度器只是被构造、
	// 还没 Run，此时注入是安全的——注入的是接口，不是它的运行状态。
	// 若把这段放回 11)，就需要先建 Handler 再回填 Deps，反而更难读。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sched := NewScheduler(repo, registry, tracker, writer, multi, latencyAgg, alerts, snap, logger)

	// 11) 证书与 TLS（6.3）
	//
	// 四种取证书方式统一由 tlsconf 决定；mode=acme 时首张证书在后台申请，
	// 不阻塞启动（否则一个 DNS/网络问题就能让整个 Master 起不来）。
	tlsProvider, err := tlsconf.Setup(cfg.TLS, cfg.DataDir, logger)
	if err != nil {
		return fmt.Errorf("TLS 初始化失败: %w", err)
	}

	// 12) 接口层
	distDir := filepath.Join(cfg.DataDir, "dist")
	h := api.New(api.Deps{
		Repo: repo, Registry: registry, Snap: snap, Tokens: tokens, Keyring: keyring,
		Writer: writer, Multi: multi, Latency: latencyAgg, Alerts: alerts,
		Themes: themes, Tracker: tracker,
		Version: version, TrustedProxies: cfg.TrustedProxies, DistDir: distDir,
		TLSMode:   cfg.TLS.Mode,
		TLSActive: cfg.TLSActive(),
		HSTS: api.HSTSConfig{
			Enabled:           cfg.HSTSEnabled(),
			MaxAge:            cfg.TLS.HSTS.MaxAge,
			IncludeSubDomains: cfg.TLS.HSTS.IncludeSubDomains,
			Preload:           cfg.TLS.HSTS.Preload,
		},
		// 6.1：配了 public_hosts / admin_hosts 就强制两站点各用各的域名。
		PublicHosts: cfg.PublicHosts,
		AdminHosts:  cfg.AdminHosts,
		// 公开看板的「管理后台」按钮按它跳转（分离部署时写死的 /admin/ 会跳错）。
		AdminOrigin:      cfg.AdminOrigin,
		TLSInfo:          tlsProvider,
		ChallengeHandler: tlsProvider.ChallengeHandler(),
		InstallMode:      func() string { return repo.GetSettingDefault("install.mode", "on_demand") },
		MasterURL: func() string {
			if v := repo.GetSettingDefault("install.master_url", ""); v != "" {
				return v
			}
			// 未显式配置时回落到 public_origin：既然已经配了公开站域名，
			// 就不该再要求管理员在设置页里手抄一遍——两处不一致正是
			// "安装命令里的地址不对"最常见的来源。
			return cfg.PublicOrigin
		},
		Logger:    logger,
		Heartbeat: sched, // A5 状态卡：只读快照，见 handleHeartbeatStatus
	})

	// 13) 启动写协程与调度循环
	//
	// 证书的续期循环（tlsProvider.Run）**不在这里**：它可能立刻发起一次
	// 申请，而那需要挑战端口已经就绪。它被排在 14) 之后，与前台申请同处
	// 一条"监听已就绪"的时间线（见下面 14b 的说明）。
	writer.Start()
	go sched.Run(ctx)

	// 14) HTTP 服务
	//
	// 三个监听先全部拉起来，再谈证书（见 14b）：申请证书要走挑战，
	// 而挑战要么落在 80（http-01）、要么落在 443（tls-alpn-01）。
	var pubSrv, adminSrv, plainSrv *http.Server
	// closeAll 供 14b 之后的失败路径用：进程即将因为申请失败而退出，
	// 已绑上的端口要立刻放开，否则重启时会撞"address already in use"。
	closeAll := func() {
		for _, s := range []*http.Server{pubSrv, adminSrv, plainSrv} {
			if s != nil {
				_ = s.Close()
			}
		}
	}
	httpErr := make(chan error, 3)

	// 公开站与 Agent 接口共用同一监听地址（各自独立中间件栈，见 api.RootRoutes）。
	pubSrv = newServer(cfg.PublicListen, h.RootRoutes())
	go func() { httpErr <- serve(pubSrv, tlsProvider, nil) }()

	// 管理端：推荐使用独立监听地址（13.4.5）。
	// 若与公开站同地址，则把管理端挂在同一路由树上（同源部署，需接受"主题视为可信代码"的前提）。
	if cfg.AdminListen != "" && cfg.AdminListen != cfg.PublicListen {
		adminSrv = newServer(cfg.AdminListen, h.AdminRoutes())
		go func() { httpErr <- serve(adminSrv, tlsProvider, nil) }()
	} else {
		merged := http.NewServeMux()
		// 同一份 AdminRoutes 处理器挂两个前缀：/api/admin/v1/* 与管理界面 /admin/*。
		// ServeMux 按原路径匹配（无 StripPrefix），因此内部路由可直接复用。
		merged.Handle("/api/admin/v1/", h.AdminRoutes())
		merged.Handle("/admin/", h.AdminRoutes())
		merged.Handle("/", h.RootRoutes())
		pubSrv.Handler = merged
		logger.Warn("管理端与公开站同源部署：主题与注入脚本将被视为可信代码（13.4.5）")
	}

	// ACME http-01 需要一个明文端口。它只在"我们自己去申请证书"时才开，
	// 且只服务挑战路径（其余请求 301 到 https），不会被当成第二个站点。
	//
	// ⚠ 这个监听必须**先于**任何 ACME 申请就绪（见 14b）。http-01 的校验方式
	// 是 CA 从公网访问 http://<域名>:80/.well-known/acme-challenge/<token>，
	// 我们得先在 80 上应答，CA 才可能验过。曾经把同步申请做在 tlsconf.Setup
	// 里（= 在这个监听之前），于是申请期间 80 从未被监听、每次都因"连不上"
	// 而失败，失败又让进程退出、80 更起不来——一台 80 完全空闲、防火墙全开
	// 的机器上照样申请不到证书，用户看到的就是"申请期间 80 一直没起来"。
	//
	// 所以这里用**同步** Listen 而不是丢进 goroutine：绑不上就要当场说清楚
	// 原因（下面的错误文案覆盖了最常见的两种：端口被占、权限不足）。
	// 若是丢进 httpErr 通道，这条失败会被随后那次申请的失败淹掉，现场只剩
	// 一句含糊的"证书申请失败"。
	if addr := tlsProvider.PlainHTTPAddr(); addr != "" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			closeAll()
			return fmt.Errorf("无法监听 ACME http-01 明文端口 %s：%w。"+
				"http-01 要求 CA 能从公网访问这个端口，先把它让出来再启动："+
				"（1）端口被前置反代占用 → 把 tls.acme.http_addr 设为 off，"+
				"并让反代把 /.well-known/acme-challenge/ 转发到本进程；"+
				"（2）提示 permission denied → 80 属于特权端口（<1024），"+
				"当前用户无权绑定，需要给服务授予 CAP_NET_BIND_SERVICE"+
				"（部署脚本写的 systemd 单元已包含这一项，手工装的话请自行补上）", addr, err)
		}
		plainSrv = newServer(addr, tlsProvider.PlainHTTPHandler())
		go func() { httpErr <- serve(plainSrv, nil, ln) }()
		logger.Info("已启动 ACME http-01 明文监听", "addr", addr)
	}

	// 14b) 部署期的前台申请证书（tls.acme.sync_obtain）
	//
	// 位置是**硬约束**：必须在上面所有监听就绪之后（见 ObtainSyncOnStartup
	// 的注释）。平时它是个空操作；只有部署脚本打开 sync_obtain 时才会真的
	// 去申请，并且"申请不成功就不启动"，好让部署脚本拿到一个确定的结果、
	// 失败时回滚，而不是留下一个"服务在跑但 https 打不开"的假象。
	if err := tlsProvider.ObtainSyncOnStartup(ctx); err != nil {
		logger.Error("证书前台申请失败", "err", err.Error())
		closeAll()
		return fmt.Errorf("TLS 初始化失败: %w", err)
	}

	// 14c) 证书续期循环
	//
	// 放在这里而不是 13)：它启动时会立刻 tick 一次，而 tick 可能直接发起
	// 申请——同样需要挑战端口已经就绪。
	tlsProvider.Run(ctx)

	scheme := "http"
	if cfg.TLSActive() {
		scheme = "https"
	}
	logger.Info("xb-probe-master 已启动", "version", version,
		"public", cfg.PublicListen, "admin", cfg.AdminListen, "data_dir", cfg.DataDir,
		"tls_mode", cfg.TLS.Mode)
	logTLSStatus(logger, tlsProvider)
	if cfg.AdminListen == "" || cfg.AdminListen == cfg.PublicListen {
		logger.Info("管理后台入口", "url", scheme+"://"+cfg.PublicListen+"/admin/")
	}
	if len(cfg.PublicHosts) > 0 || len(cfg.AdminHosts) > 0 {
		logger.Info("已启用源分离（Host 白名单）",
			"public_hosts", cfg.PublicHosts, "admin_hosts", cfg.AdminHosts)
	}
	// 站点形态的**最终生效值**在这里落一条：后台的 site.layout /
	// site.public_hosts / site.admin_hosts 会顶替配置文件里的对应项，
	// 只打印配置文件那份会让"到底哪个域名能打开后台"无从确认。
	logger.Info("站点形态（最终生效）",
		"layout", h.SiteLayoutMode(),
		"public_hosts", h.EffectivePublicHosts(), "admin_hosts", h.EffectiveAdminHosts())

	// 未启用 TLS 时，管理端会话只能降级为不带 Secure 的 Cookie（浏览器会丢弃
	// 纯 HTTP 下的 Secure Cookie）。这里显式提醒，避免被误判成"登录接口坏了"。
	if !cfg.TLSActive() && len(cfg.TrustedProxies) == 0 {
		logger.Warn("tls.mode=none 且未配置 trusted_proxies：管理端会话 Cookie 将为 probe_session（不带 Secure）。" +
			"仅适合本机或受信内网调试；对外提供访问请启用 TLS 或置于 HTTPS 反代之后并配置 trusted_proxies。")
	}

	// 15) 优雅退出：排空写队列并强制 flush（4.2 第 5 条）
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case s := <-sig:
		logger.Info("收到退出信号，正在优雅关闭", "signal", s.String())
	case err := <-httpErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP 服务异常", "err", err.Error())
		}
	}

	cancel()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	for _, s := range []*http.Server{pubSrv, adminSrv, plainSrv} {
		if s != nil {
			_ = s.Shutdown(shutdownCtx)
		}
	}
	sched.Stop()
	multi.FlushAll()
	latencyAgg.FlushAll()
	_ = tracker.Flush(time.Now())
	writer.Stop()
	_ = credentials.Close()
	logger.Info("已安全退出")
	return nil
}

func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second, // S9：防慢连接
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
}

// serve 启动一个监听：provider 非空且启用了 TLS 时走 TLS，否则明文。
//
// 证书一律通过 tls.Config.GetCertificate 动态提供（files 热加载 / selfsigned
// / acme 都可能在中途换证书），所以 ListenAndServeTLS 的两个文件名传空。
//
// ln 非 nil 表示"套接字已经由调用方绑好"（ACME 明文端口要同步 Listen 才能
// 把绑定失败的原因当场报出来，见 14) 那段注释）；此时只负责在上面 Serve。
func serve(srv *http.Server, provider *tlsconf.Provider, ln net.Listener) error {
	var tlsCfg *tls.Config
	if provider != nil {
		tlsCfg = provider.TLSConfig()
	}
	var err error
	switch {
	case ln != nil && tlsCfg != nil:
		srv.TLSConfig = tlsCfg
		err = srv.ServeTLS(ln, "", "")
	case ln != nil:
		err = srv.Serve(ln)
	case tlsCfg != nil:
		srv.TLSConfig = tlsCfg
		err = srv.ListenAndServeTLS("", "")
	default:
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// logTLSStatus 在启动日志里打一份证书摘要，便于运维一眼确认"线上到底用的是哪张证书"。
// SPKI 指纹尤其重要：Agent 的证书固定用的就是它，指纹变了所有 Agent 都会连不上。
func logTLSStatus(logger *slog.Logger, provider *tlsconf.Provider) {
	st := provider.TLSStatus()
	if st.Mode == "none" {
		return
	}
	args := []any{
		"mode", st.Mode, "source", st.Source,
		"domains", strings.Join(st.Domains, ","),
	}
	if st.Challenge != "" {
		args = append(args, "challenge", st.Challenge)
	}
	if st.NotAfter > 0 {
		args = append(args,
			"subject", st.Subject,
			"not_after", time.Unix(st.NotAfter, 0).UTC().Format(time.RFC3339),
			"spki_sha256", st.SPKISHA256)
	}
	logger.Info("TLS 已启用", args...)
	if st.NotAfter == 0 {
		logger.Warn("尚未取得证书：ACME 正在后台申请；在申请成功前 TLS 握手会失败。" +
			"请确认域名已解析到本机且 80/443 可达（看日志里的失败原因，或后台「TLS」状态）。")
	} else if time.Until(time.Unix(st.NotAfter, 0)) < 7*24*time.Hour {
		logger.Warn("证书即将到期，续期若持续失败请尽快排查", "not_after", time.Unix(st.NotAfter, 0).UTC().Format(time.RFC3339))
	}
	if st.LastError != "" {
		logger.Warn("最近一次证书申请/续期失败", "err", st.LastError)
	}
}

func loadChannels(repo *store.Repo, kr *auth.Keyring, logger *slog.Logger) []alert.Channel {
	rows, err := repo.ListChannels()
	if err != nil {
		return nil
	}
	var out []alert.Channel
	for _, c := range rows {
		if !c.Enabled {
			continue
		}
		plain, err := kr.Decrypt(c.ConfigEnc)
		if err != nil {
			logger.Error("解密告警渠道失败", "channel", c.Name, "err", err.Error())
			continue
		}
		switch c.Type {
		case "telegram":
			var cfg alert.TelegramConfig
			if jsonUnmarshal(plain, &cfg) {
				out = append(out, alert.NewTelegramChannel(c.Name, cfg))
			}
		case "smtp":
			var cfg alert.SMTPConfig
			if jsonUnmarshal(plain, &cfg) {
				out = append(out, alert.NewSMTPChannel(c.Name, cfg))
			}
		}
	}
	return out
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
