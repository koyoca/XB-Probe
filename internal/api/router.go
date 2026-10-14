package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/koyoca/xb-probe/internal/acme"
	"github.com/koyoca/xb-probe/internal/auth"
)

// 路由装配（文档 5.2 / 7.0 / 13.4.5）。
//
// 三组前缀使用不同的中间件栈：
//   - /api/agent/v1  ：Bearer 令牌；无 Cookie、无 CORS。
//   - /api/admin/v1  ：会话 Cookie(__Host-) + CSRF；不返回 CORS 头，跨源预检必失败。
//   - /api/public/v1 ：免鉴权、只读，输出安全响应头。
//
// 公开站与管理端建议部署在不同源（status.example.com / admin.example.com）。

// PublicRoutes 返回公开站（含主题静态资源与安装脚本）的处理器。
func (h *Handler) PublicRoutes() http.Handler {
	mux := http.NewServeMux()

	// --- ACME http-01 挑战（必须在 GET / 的兜底之前注册） ---
	// 免鉴权、且要绕过 host 白名单与"公开站已关闭"开关：CA 是按 IP 访问的，
	// 它既不知道我们的域名，也无从登录。
	//
	// ⚠ 这个模式**必须**带方法（"GET "），不能只写路径。Go 1.22 起的 ServeMux
	// 会在注册时检查冲突：`/.well-known/acme-challenge/`（不带方法 = 匹配所有
	// 方法）与兜底的 `GET /` 互不包含（前者路径更具体但方法更多，后者方法更窄
	// 但路径更宽），于是**注册即 panic**：
	//
	//	panic: pattern "GET /" conflicts with pattern "/.well-known/acme-challenge/":
	//	        GET / matches fewer methods than /.well-known/acme-challenge/,
	//	        but has a more general path pattern
	//
	// 这个 panic 发生在装配路由时（进程启动阶段），任何"选 acme + http-01"的
	// 部署都**必然**装上就退（systemd 反复重启），现场只剩一句 panic。
	// 真机上就是这样：用户选 2) master(http-01) 后服务起不来、80 端口从未被监听，
	// 而证书申请那一步给出的却是"申请失败"，把人引向 DNS/防火墙/限流。
	// 带上 "GET " 之后两者是"方法相同、路径更具体"的关系，不再冲突；
	// 且 Go 的 ServeMux 里 GET 模式**同时匹配 HEAD**（真机验证），CA 用哪种都能应答。
	if h.ChallengeHandler != nil {
		mux.Handle("GET "+acme.HTTP01Prefix, h.ChallengeHandler)
	}

	// --- 公开 API ---
	mux.HandleFunc("GET /api/public/v1/site", h.handleSite)
	mux.HandleFunc("GET /api/public/v1/overview", h.handleOverview)
	mux.HandleFunc("GET /api/public/v1/nodes", h.handlePublicNodes)
	mux.HandleFunc("GET /api/public/v1/nodes/{public_id}", h.handlePublicNode)
	mux.HandleFunc("GET /api/public/v1/nodes/{public_id}/history", h.handlePublicHistory)
	mux.HandleFunc("GET /api/public/v1/nodes/{public_id}/latency", h.handlePublicLatency)

	// --- 一键安装（12.2）：免鉴权，不含任何节点秘密 ---
	mux.HandleFunc("GET /install.sh", h.handleInstallSh)
	mux.HandleFunc("GET /install.ps1", h.handleInstallPS1)
	mux.HandleFunc("GET /dist/", h.handleDist)

	// --- 前端（内置主题 / 外部主题） ---
	mux.HandleFunc("GET /", h.handleThemeAsset)

	var h2 http.Handler = mux
	h2 = BodyLimit(1<<20, h2) // 公开接口请求体上限 1 MB
	// 站点形态与 host 白名单（6.1）：配了 public_hosts / admin_hosts（或后台的
	// site.public_hosts / site.admin_hosts）才生效；放在限速之内，这样"拿错域名
	// 来扫"的流量同样会被限速，不会成为绕过限速的口子。
	//
	// 这里是**公开站那一侧**的判定：公开路径必须在公开域名上。
	// 管理路径由 AdminRoutes 自己的门控负责（同源部署时 merged 的 ServeMux 会把
	// /admin/ 与 /api/admin/ 直接派给 AdminRoutes，根本到不了这里）；
	// 只有"公开与管理分开监听"时 /api/admin/... 才会落到公开入口的兜底路由上，
	// 那时也必须按管理域名判——见 origin.go 的 siteGate。
	h2 = h.siteGate(false, h2)
	h2 = RateLimitMiddleware(NewIPLimiter(300, 600), h2)
	// 公开站总开关（设置项 public.enabled）：放在最内层，紧贴路由，
	// 这样被拒绝的请求同样要经过限速与安全响应头，不会变成绕过限速的口子。
	h2 = h.publicGate(h2)
	// 仅当管理员开启了脚本注入时才下发 nonce（5.3 / 13.4.5）。
	h2 = SecurityHeaders(h.securityPolicy(), h.scriptInjectEnabled, h2)
	return h2
}

// securityPolicy 组装安全响应头策略：HSTS 参数 + "本次请求是否 HTTPS"。
// 两者共用 h.requestSecure，避免出现"Cookie 已降级、HSTS 却还在发"的矛盾状态。
func (h *Handler) securityPolicy() SecurityPolicy {
	return SecurityPolicy{HSTS: h.HSTS, Secure: h.requestSecure}
}

// publicGate 是公开站的全局开关（设置项 public.enabled，默认开放）。
//
// 关闭后的行为分三类：
//   - 持有效管理员会话的请求 —— 放行。这是管理员自己的"预览"通道：
//     关掉看板通常是暂时隐藏（维护/切换主题），管理员需要确认改完后的效果。
//     会话要过完整的校验（查库 + 空闲过期），伪造 Cookie 拿不到任何东西；
//     未登录访客看到的仍然是 404，对外表现与完全关闭一致。
//   - 公开页面与 /api/public/v1/*（未登录）—— 一律拒绝。前者返回一张极简提示页（404），
//     后者返回 404 JSON。用 404 而不是 403：对外部扫描者而言"这台机器没有这个
//     站点"比"这里有个被禁止的站点"透露得更少。
//   - /install.sh、/install.ps1、/dist/ —— 放行。一键安装链路与"是否公开展示
//     看板"是两件事，关掉看板不该把装机通道一起关死。这三个路径本来就不含
//     任何节点秘密（令牌要现场注册才拿到），放行的风险为零。
func (h *Handler) publicGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.publicEnabled() || isInstallPath(r.URL.Path) || isACMEChallengePath(r.URL.Path) || h.validAdminSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/public/") {
			writeErr(w, http.StatusNotFound, "not_found", "公开接口已关闭")
			return
		}
		writePublicClosed(w)
	})
}

// validAdminSession 判断请求是否携带有效的管理员会话（与 /api/admin/v1 同一口径）。
//
// 注意三点：Cookie 可能不存在（不算错误）；会话要查库，不能只看 Cookie 格式；
// 必须做空闲过期校验，否则"关页签一小时后回来"的旧会话也能看已关闭的看板。
// 这里刻意不触碰 CSRF（公开站只有 GET，且不执行任何写操作）。
func (h *Handler) validAdminSession(r *http.Request) bool {
	plain, ok := auth.ReadSessionCookie(r)
	if !ok {
		return false
	}
	sess, err := h.Repo.LookupSession(auth.HashToken(plain))
	if err != nil || sess == nil {
		return false
	}
	return auth.CheckIdleExpiry(sess, time.Now()) == nil
}

// isInstallPath 判断是否为安装/发布链路（开关关闭时仍需放行的路径）。
func isInstallPath(p string) bool {
	return p == "/install.sh" || p == "/install.ps1" || strings.HasPrefix(p, "/dist/")
}

// isACMEChallengePath 判断是否为 ACME http-01 挑战路径。
//
// 它必须与"公开看板是否开放"解耦：管理员常常在看板维护期关掉公开站，
// 若顺手把证书续期也关死了，几周后证书过期会导致整站不可访问——
// 而那时公开站还关着，故障表现会非常难解释。
func isACMEChallengePath(p string) bool {
	return strings.HasPrefix(p, acme.HTTP01Prefix)
}

// writePublicClosed 返回"公开看板未开放"提示页。
//
// 注意：这段 HTML 会经过 SecurityHeaders，CSP 是 style-src 'self'，
// 因此不能带 <style> 块或 style 属性，只用最朴素的语义标签。
func writePublicClosed(w http.ResponseWriter) {
	const page = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>未开放</title></head>
<body>
<h1>公开看板未开放</h1>
<p>管理员已暂时关闭公开节点页面。</p>
<p>如果你是管理员：登录管理后台后刷新本页即可查看（登录状态下不受此开关影响）；
也可以在「系统设置 → 公开看板」中把 <code>公开看板</code> 设为「开放」。</p>
</body></html>
`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(page))
}

// AdminRoutes 返回管理后台的处理器（固定界面，不随主题变化）。
//
// 路径分两类：
//   - /api/admin/v1/*  管理 JSON 接口（会话 + CSRF）
//   - /admin/*         固定后台界面（静态资源，内嵌于二进制）
func (h *Handler) AdminRoutes() http.Handler {
	mux := http.NewServeMux()

	// --- ACME http-01 挑战 ---
	// 管理端监听也可能被反代指向（例如只有 admin 域名配了 80 端口），
	// 因此这里同样挂一份。挑战内容本就是公开的，不构成信息泄露。
	//
	// 同样必须带 "GET "：本 mux 里还有 `GET /admin/` 与大量 `GET /api/admin/v1/…`
	// 模式，不带方法的挑战模式属于"无方法"的一类，与它们的方法集不同，
	// 将来若有哪个兜底模式写成不带方法就会当场 panic（同 PublicRoutes 的注释）。
	if h.ChallengeHandler != nil {
		mux.Handle("GET "+acme.HTTP01Prefix, h.ChallengeHandler)
	}

	// 后台界面（固定，不随主题变化，5.2）。静态资源本身不含任何敏感数据，
	// 真正的鉴权由页面发起的 /api/admin/v1 请求承担。
	mux.HandleFunc("GET /admin/", h.handleAdminUI)

	// 认证（登录接口本身需要限速，但不要求会话）
	mux.HandleFunc("POST /api/admin/v1/auth/login", h.handleLogin)
	mux.HandleFunc("POST /api/admin/v1/auth/logout", h.requireAdmin(h.handleLogout))
	mux.HandleFunc("GET /api/admin/v1/auth/me", h.requireAdmin(h.handleMe))
	mux.HandleFunc("POST /api/admin/v1/auth/password", h.requireAdmin(h.handleChangePassword))

	// 节点
	mux.HandleFunc("GET /api/admin/v1/nodes", h.requireAdmin(h.handleListNodes))
	mux.HandleFunc("POST /api/admin/v1/nodes", h.requireAdmin(h.handleCreateNode))
	mux.HandleFunc("GET /api/admin/v1/nodes/{id}", h.requireAdmin(h.handleGetNode))
	mux.HandleFunc("PATCH /api/admin/v1/nodes/{id}", h.requireAdmin(h.handlePatchNode))
	mux.HandleFunc("POST /api/admin/v1/nodes/{id}/archive", h.requireAdmin(h.handleArchiveNode))
	mux.HandleFunc("DELETE /api/admin/v1/nodes/{id}", h.requireAdmin(h.handleDeleteNode))

	// 实时态与历史（5.2 全量资产视察；按真实 node_id，隐藏节点也可查）
	mux.HandleFunc("GET /api/admin/v1/nodes/{id}/runtime", h.requireAdmin(h.handleNodeRuntime))
	mux.HandleFunc("GET /api/admin/v1/nodes/{id}/history", h.requireAdmin(h.handleAdminHistory))
	mux.HandleFunc("GET /api/admin/v1/nodes/{id}/latency", h.requireAdmin(h.handleAdminLatency))

	// 令牌
	mux.HandleFunc("GET /api/admin/v1/nodes/{id}/tokens", h.requireAdmin(h.handleListTokens))
	mux.HandleFunc("POST /api/admin/v1/nodes/{id}/enroll-codes", h.requireAdmin(h.handleCreateEnrollCode))
	mux.HandleFunc("DELETE /api/admin/v1/nodes/{id}/tokens/{tid}", h.requireAdmin(h.handleRevokeToken))

	// 一键安装（12.2）
	mux.HandleFunc("POST /api/admin/v1/nodes/{id}/install-command", h.requireAdmin(h.handleInstallCommand))
	mux.HandleFunc("GET /api/admin/v1/agent-releases", h.requireAdmin(h.handleAgentReleases))

	// 计费
	mux.HandleFunc("PUT /api/admin/v1/nodes/{id}/billing", h.requireAdmin(h.handlePutBilling))
	mux.HandleFunc("POST /api/admin/v1/nodes/{id}/billing/renew", h.requireAdmin(h.handleRenew))
	mux.HandleFunc("GET /api/admin/v1/nodes/{id}/traffic", h.requireAdmin(h.handleTraffic))
	mux.HandleFunc("PUT /api/admin/v1/nodes/{id}/traffic", h.requireAdmin(h.handleSetTrafficUsage))

	// 探测
	mux.HandleFunc("GET /api/admin/v1/probe-targets", h.requireAdmin(h.handleListTargets))
	mux.HandleFunc("POST /api/admin/v1/probe-targets", h.requireAdmin(h.handleCreateTarget))
	mux.HandleFunc("PATCH /api/admin/v1/probe-targets/{id}", h.requireAdmin(h.handlePatchTarget))
	mux.HandleFunc("DELETE /api/admin/v1/probe-targets/{id}", h.requireAdmin(h.handleDeleteTarget))

	// 告警
	mux.HandleFunc("GET /api/admin/v1/alert-channels", h.requireAdmin(h.handleListChannels))
	mux.HandleFunc("POST /api/admin/v1/alert-channels", h.requireAdmin(h.handleCreateChannel))
	mux.HandleFunc("PATCH /api/admin/v1/alert-channels/{id}", h.requireAdmin(h.handlePatchChannel))
	mux.HandleFunc("DELETE /api/admin/v1/alert-channels/{id}", h.requireAdmin(h.handleDeleteChannel))
	mux.HandleFunc("POST /api/admin/v1/alert-channels/{id}/test", h.requireAdmin(h.handleTestChannel))
	mux.HandleFunc("GET /api/admin/v1/alert-rules", h.requireAdmin(h.handleGetRules))
	mux.HandleFunc("PUT /api/admin/v1/alert-rules", h.requireAdmin(h.handlePutRules))
	mux.HandleFunc("GET /api/admin/v1/alert-events", h.requireAdmin(h.handleListEvents))

	// 系统
	mux.HandleFunc("GET /api/admin/v1/settings", h.requireAdmin(h.handleGetSettings))
	// /settings/meta 是**只读的选项清单**（可选卡片、可选公开字段），
	// 与 /settings（当前值）分开：前者随代码升级变化、后者随管理员操作变化。
	// 后台设置页据此渲染勾选组，避免把卡片清单在前端再抄一份。
	mux.HandleFunc("GET /api/admin/v1/settings/meta", h.requireAdmin(h.handleSettingsMeta))
	mux.HandleFunc("PUT /api/admin/v1/settings", h.requireAdmin(h.handlePutSettings))
	mux.HandleFunc("GET /api/admin/v1/themes", h.requireAdmin(h.handleListThemes))
	mux.HandleFunc("PUT /api/admin/v1/themes/active", h.requireAdmin(h.handleSetTheme))
	mux.HandleFunc("GET /api/admin/v1/storage/stats", h.requireAdmin(h.handleStorageStats))
	mux.HandleFunc("GET /api/admin/v1/audit-log", h.requireAdmin(h.handleAuditLog))
	mux.HandleFunc("POST /api/admin/v1/maintenance/backup", h.requireAdmin(h.handleBackup))
	mux.HandleFunc("GET /api/admin/v1/maintenance/backups", h.requireAdmin(h.handleListBackups))
	// 删除单份备份：备份不许无限堆积（备份盘满往往是主库写不进去的前奏），
	// 但也不能只依赖轮转——管理员需要能手动清掉某一份坏备份或占地方的大备份。
	mux.HandleFunc("DELETE /api/admin/v1/maintenance/backups/{name}", h.requireAdmin(h.handleDeleteBackup))
	mux.HandleFunc("GET /api/admin/v1/maintenance/heartbeat", h.requireAdmin(h.handleHeartbeatStatus))

	// TLS / 证书运行态（只读回显，见 tls_status.go）。
	// 与 /settings 分开：证书模式来自 master.yaml，改它需要重启，不是在线设置项。
	mux.HandleFunc("GET /api/admin/v1/tls/status", h.requireAdmin(h.handleTLSStatus))

	// 账户安全：TOTP 启用流程与恢复码（6.2）
	//
	// 全部挂在"已登录"之下，但每个写操作都额外要求密码或动态码——
	// 会话本身不足以关闭二次验证或重置恢复码，否则会话被劫持就等于
	// 二次验证被永久关掉。
	mux.HandleFunc("GET /api/admin/v1/account/totp", h.requireAdmin(h.handleTOTPStatus))
	mux.HandleFunc("POST /api/admin/v1/account/totp/setup", h.requireAdmin(h.handleTOTPSetup))
	mux.HandleFunc("POST /api/admin/v1/account/totp/enable", h.requireAdmin(h.handleTOTPEnable))
	mux.HandleFunc("POST /api/admin/v1/account/totp/disable", h.requireAdmin(h.handleTOTPDisable))
	mux.HandleFunc("POST /api/admin/v1/account/recovery-codes", h.requireAdmin(h.handleRegenerateRecoveryCodes))

	var h2 http.Handler = mux
	h2 = BodyLimit(1<<20, h2)
	// host 白名单（6.1）：配了 admin_hosts（或后台的 site.admin_hosts）才生效。
	// 放在限速之内，"拿公开域名来试 /admin/"的流量同样被限速。
	//
	// adminEntry=true：这个入口只服务管理路径。它在**同源部署**（公开与管理
	// 共用一个监听）里也是生效的那一道门——那种部署下 ServeMux 会把
	// /admin/ 与 /api/admin/ 直接派到这里，公开入口的门控看不到它们。
	h2 = h.siteGate(true, h2)
	h2 = RateLimitMiddleware(NewIPLimiter(120, 240), h2)
	// 管理端不允许任何注入内容，CSP 恒为最严（13.4.5）。
	h2 = SecurityHeaders(h.securityPolicy(), nil, h2)
	return h2
}

// AgentRoutes 返回 Agent 接口的处理器。
func (h *Handler) AgentRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agent/v1/enroll", h.handleEnroll)
	mux.HandleFunc("POST /api/agent/v1/report", h.handleReport)
	mux.HandleFunc("GET /api/agent/v1/config", h.handleGetConfig)
	mux.HandleFunc("POST /api/agent/v1/deregister", h.handleDeregister)
	// 自升级取件（12.6）：节点令牌鉴权，与 install.mode 无关。
	mux.HandleFunc("GET /api/agent/v1/releases/latest", h.handleAgentReleaseLatest)
	mux.HandleFunc("GET /api/agent/v1/artifact", h.handleAgentArtifact)

	var h2 http.Handler = mux
	// Agent 接口 IP 白名单（系统设置 agent.ip_whitelist_enabled / agent.ip_whitelist）。
	// 放在最外层：白名单外连注册通道也拿不到，避免"没开通的节点绕过管理端直接 enroll"。
	h2 = h.agentIPGate(h2)
	// /report 请求体 ≤ 256 KB（7.0）。
	h2 = BodyLimit(256*1024, h2)
	h2 = RateLimitMiddleware(NewIPLimiter(600, 1200), h2)
	return h2
}

// agentIPGate 是 Agent 接口的来源 IP 白名单。
//
// 设置项 agent.ip_whitelist_enabled 开启后，只有名单内的 IP 才能访问
// /api/agent/v1/*（上报、拉配置、注册、注销全部收口）。名单支持单 IP 与
// CIDR（如 203.0.113.0/24），逗号分隔。
//
// 来源 IP 的取法与 RealIPMiddleware 同一套规则：只有 TCP 对端命中可信代理时
// 才认 X-Forwarded-For，否则谁都能伪造头把自己洗进名单。
func (h *Handler) agentIPGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.Repo.GetSettingDefault("agent.ip_whitelist_enabled", "0") == "1" {
			ip := remoteHost(r.RemoteAddr)
			trusted := parseCIDRs(h.TrustedProxies)
			if isTrusted(ip, trusted) {
				if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
					ip = rightmostUntrusted(xff, trusted)
				}
			}
			allow := strings.Split(h.Repo.GetSettingDefault("agent.ip_whitelist", ""), ",")
			if !isTrusted(ip, parseCIDRs(allow)) {
				writeErr(w, http.StatusForbidden, "forbidden", "来源 IP 不在白名单内")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// RootRoutes 把三组接口装配到同一监听地址上，各自保留独立中间件栈。
//
// 之所以不各起一个 http.Server：公开站与 Agent 接口通常共用同一域名与端口，
// 由反向代理（Caddy/Nginx）统一转发到本机的一个监听地址。
// 管理端若配置了独立监听地址，则单独起一个 Server（见 cmd/xb-probe-master）。
func (h *Handler) RootRoutes() http.Handler {
	root := http.NewServeMux()
	// 更具体的模式优先，因此 Agent 与公开接口互不干扰。
	root.Handle("/api/agent/v1/", h.AgentRoutes())
	root.Handle("/", h.PublicRoutes())
	return root
}

// requireAdmin 管理端鉴权：会话 + CSRF（写请求）。
func (h *Handler) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		plain, ok := auth.ReadSessionCookie(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "未登录")
			return
		}
		sess, err := h.Repo.LookupSession(auth.HashToken(plain))
		if err != nil || sess == nil {
			auth.ClearSessionCookie(w)
			writeErr(w, http.StatusUnauthorized, "unauthorized", "会话无效")
			return
		}
		if err := auth.CheckIdleExpiry(sess, time.Now()); err != nil {
			auth.ClearSessionCookie(w)
			writeErr(w, http.StatusUnauthorized, "unauthorized", "会话过期")
			return
		}
		// 所有写操作校验 CSRF（9.3 / 13.4.5）。
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !auth.VerifyCSRF(sess.CSRFToken, r.Header.Get(auth.CSRFHeader)) {
				writeErr(w, http.StatusForbidden, "forbidden", "CSRF 校验失败")
				return
			}
		}
		ctx := contextWithAdmin(r.Context(), sess)
		next(w, r.WithContext(ctx))
	}
}
