// probe-master 是 Server Probe 的中枢服务（文档第 4 章 / 第 10 章）。
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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yourorg/probe/internal/alert"
	"github.com/yourorg/probe/internal/api"
	"github.com/yourorg/probe/internal/auth"
	"github.com/yourorg/probe/internal/config"
	"github.com/yourorg/probe/internal/db"
	"github.com/yourorg/probe/internal/pipeline"
	"github.com/yourorg/probe/internal/state"
	"github.com/yourorg/probe/internal/store"
	"github.com/yourorg/probe/internal/theme"
	"github.com/yourorg/probe/internal/tlsconf"
	"github.com/yourorg/probe/internal/traffic"

	// 空导入：把 IANA 时区库编译进来。没有它时 time.LoadLocation 在
	// Windows 与精简容器（Alpine/distroless）上会失败，直接导致计费的
	// tz 校验 400、以及"今日流量"静默落到错误时区。详见包内注释。
	_ "github.com/yourorg/probe/internal/tzdata"
)

// version 由 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "init-admin":
			if err := runInitAdmin(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "错误：", err)
				os.Exit(1)
			}
			return
		case "reset-admin":
			if err := runResetAdmin(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "错误：", err)
				os.Exit(1)
			}
			return
		case "dist":
			if err := runDist(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "错误：", err)
				os.Exit(1)
			}
			return
		case "version", "-v", "--version":
			fmt.Println("probe-master", version)
			return
		}
	}

	cfgPath := flag.String("config", "/etc/probe-master/master.yaml", "配置文件路径")
	flag.Parse()

	if err := run(*cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		os.Exit(1)
	}
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
	alerts := alert.New(rules, repo, loadChannels(repo, keyring, logger))
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
		PublicHosts:      cfg.PublicHosts,
		AdminHosts:       cfg.AdminHosts,
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

	// 13) 启动写协程、调度循环与证书续期循环
	writer.Start()
	go sched.Run(ctx)
	tlsProvider.Run(ctx)

	// 14) HTTP 服务
	httpErr := make(chan error, 3)

	// 公开站与 Agent 接口共用同一监听地址（各自独立中间件栈，见 api.RootRoutes）。
	pubSrv := newServer(cfg.PublicListen, h.RootRoutes())
	go func() { httpErr <- serve(pubSrv, tlsProvider) }()

	// 管理端：推荐使用独立监听地址（13.4.5）。
	// 若与公开站同地址，则把管理端挂在同一路由树上（同源部署，需接受"主题视为可信代码"的前提）。
	var adminSrv *http.Server
	if cfg.AdminListen != "" && cfg.AdminListen != cfg.PublicListen {
		adminSrv = newServer(cfg.AdminListen, h.AdminRoutes())
		go func() { httpErr <- serve(adminSrv, tlsProvider) }()
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
	var plainSrv *http.Server
	if addr := tlsProvider.PlainHTTPAddr(); addr != "" {
		plainSrv = newServer(addr, tlsProvider.PlainHTTPHandler())
		go func() { httpErr <- serve(plainSrv, nil) }()
		logger.Info("已启动 ACME http-01 明文监听", "addr", addr)
	}

	scheme := "http"
	if cfg.TLSActive() {
		scheme = "https"
	}
	logger.Info("probe-master 已启动", "version", version,
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
func serve(srv *http.Server, provider *tlsconf.Provider) error {
	var tlsCfg *tls.Config
	if provider != nil {
		tlsCfg = provider.TLSConfig()
	}
	var err error
	if tlsCfg != nil {
		srv.TLSConfig = tlsCfg
		err = srv.ListenAndServeTLS("", "")
	} else {
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
