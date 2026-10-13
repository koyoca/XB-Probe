package api

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/koyoca/xb-probe/internal/acme"
)

// 6.1 公开站与管理端分离为不同源（status.example.com / admin.example.com）。
//
// "不同源"（origin = scheme + host + port）在代码层面靠三件事落地：
//
//  1. 两个独立监听地址（cmd/xb-probe-master，已支持）；
//  2. 两边只服务自己的路由（PublicRoutes / AdminRoutes，已支持）；
//  3. **Host 白名单**（本文件）：配了 public_hosts / admin_hosts 之后，
//     用错域名的请求返回 421。
//
// 第 3 条是必要的：现实中两个域名往往解析到同一台机器、走同一个反代，
// 光靠"路由分开"挡不住"用公开域名访问 /admin/"。Cookie 作用域
// （__Host- 不带 Domain，天然按主机隔离）、CSP、访问日志也随之一并干净。
//
// 未配置白名单时完全不生效——单机部署与"同源"部署仍照常工作。

// hostGate 在 Host 不在白名单内时返回 421 Misdirected Request。
//
// ACME 的 http-01 挑战路径必须放行：CA 是按 IP 访问 /.well-known/ 的，
// 它发来的 Host 是 IP 字面量，永远不会命中域名白名单。
func hostGate(allowed []string, next http.Handler) http.Handler {
	if len(allowed) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, acme.HTTP01Prefix) {
			next.ServeHTTP(w, r)
			return
		}
		if hostAllowed(r.Host, allowed) {
			next.ServeHTTP(w, r)
			return
		}
		writeMisdirected(w, r)
	})
}

// hostAllowed 判断请求的 Host 是否命中白名单。
func hostAllowed(host string, allowed []string) bool {
	h := normalizeHostPort(host)
	if h == "" {
		return false
	}
	for _, a := range allowed {
		if normalizeHostPort(a) == h {
			return true
		}
	}
	return false
}

// normalizeHostPort 统一大小写、去掉尾点，并把默认端口（80/443）视作"没写端口"。
//
// 这样 "status.example.com"、"status.example.com:443"、"STATUS.example.com."
// 会被认成同一台主机——它们在浏览器眼里确实是同一源（默认端口不参与比较）。
// 非默认端口则必须显式匹配，不能忽略：:8443 与 :443 是两个不同的源。
func normalizeHostPort(v string) string {
	v = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(v), "."))
	if v == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		// 没有端口。可能是裸主机名，也可能是带方括号的 IPv6 字面量。
		return strings.Trim(v, "[]")
	}
	if port == "" {
		return strings.Trim(strings.TrimSuffix(host, "."), "[]")
	}
	if port == "80" || port == "443" {
		return strings.Trim(strings.TrimSuffix(host, "."), "[]")
	}
	return strings.Trim(strings.TrimSuffix(host, "."), "[]") + ":" + port
}

// writeMisdirected 返回 421。
//
// 用 421 而不是 404：这是部署配置错误（域名指过来了、但这里不服务它），
// 运维看到 421 立刻知道是"域名对不上"，比一个 404 好排查得多；
// 而它本身也不泄露任何内容——页面文案与接口错误都很直白。
func writeMisdirected(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeErr(w, http.StatusMisdirectedRequest, "misdirected_request",
			"该域名不服务于这个接口；请使用配置中对应的域名访问")
		return
	}
	// 注意：这段 HTML 会经过 SecurityHeaders，CSP 是 style-src 'self'，
	// 因此不能带 <style> 块或 style 属性。
	const page = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>域名不匹配</title></head>
<body>
<h1>421 · 域名不匹配</h1>
<p>当前域名不服务于本站点。这通常意味着：该域名的 DNS 或反向代理指向了这台机器，但服务端配置里只允许用另一个域名访问。</p>
<p>请检查管理后台「系统设置 → 站点形态」里的<strong>公开域名</strong>与<strong>管理域名</strong>，
或 master.yaml 中的 <code>public_hosts</code> / <code>admin_hosts</code>（<code>public_origin</code> / <code>admin_origin</code> 会推导出它们），
确认公开看板与管理后台分别使用各自的域名；若只想用一个域名同时访问两者，请把「站点形态」改为「同站」。</p>
</body></html>
`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusMisdirectedRequest)
	_, _ = w.Write([]byte(page))
}

// ---------------------------------------------------------------------------
// 站点形态：公开看板与管理后台是"两个域名"还是"一个站点"
// ---------------------------------------------------------------------------
//
// 配置层能表达的只有"监听地址"（public_listen / admin_listen）：两个地址 =
// 两个入口。但现实中更常见的是**一个监听地址 + 前置反代**——两个域名都解析到
// 同一台机器、都转发到同一个端口，此时"是不是两个站点"完全由 Host 决定，
// 靠监听地址根本表达不出来。于是把它做成可运行时切换的设置项：
//
//	site.layout        auto | single | split
//	site.public_hosts  公开站的域名清单（非空则顶替 master.yaml 的 public_hosts）
//	site.admin_hosts   管理端的域名清单（非空则顶替 master.yaml 的 admin_hosts）
//
// 门控是**按路径**做的，这是关键：同一个入口上 /admin/ 与 / 要判不同的域名。
// 只按入口判断的话，"一个端口 + 两个域名"这种部署就没有任何分离可言。

const (
	// siteLayoutAuto 跟随 master.yaml：配了 host 白名单就分离，没配就同站（默认）。
	// 默认值必须是它，不能用 single/split —— 存量部署并没有"表态"过，
	// 用一个新默认值去重新解释它们的既有配置，就是一次静默的行为变更。
	siteLayoutAuto = "auto"
	// siteLayoutSingle 明确同站：一个域名下同时提供公开看板与 /admin/。
	siteLayoutSingle = "single"
	// siteLayoutSplit 明确分离：公开域名只服务公开路径、管理域名只服务管理路径。
	siteLayoutSplit = "split"
)

// siteLayoutKeys 是"站点形态"这一簇设置项，用于判断一次保存是否动到了它们。
// site.admin_url 也在同一簇里：它决定公开看板的「管理后台」按钮指向哪，
// 改它的症状（"点登录跳到一个打不开的地址"）与改域名清单属于同一类。
var siteLayoutKeys = []string{"site.layout", "site.public_hosts", "site.admin_hosts", "site.admin_url"}

// siteLayoutValues 是 site.layout 的全部合法取值。
//
// 单独列一份是为了让"服务端接受哪些取值"与"后台下拉框提供了哪些选项"
// 能直接比对（见 site_layout_test.go）。这两处漂移的症状很隐蔽：
// 管理员在界面上根本选不出某个合法值，于是以为那个形态不存在。
var siteLayoutValues = []string{siteLayoutAuto, siteLayoutSingle, siteLayoutSplit}

// settingOr 读设置项，未配置时取默认值。
//
// Repo 允许为 nil：Handler 在若干测试里是直接构造的（没有数据库）。
// 门控不能因为读不到设置就崩——那是把一个纯粹的配置问题升级成 500。
// 读不到时按配置文件的取值工作，也就是升级前的行为。
func (h *Handler) settingOr(key, def string) string {
	if h.Repo == nil {
		return def
	}
	return h.Repo.GetSettingDefault(key, def)
}

// siteLayoutDefault 返回 site.layout 的出厂默认值。
//
// 这个回落值**只能**从 settingDefaults 里取，不能在门控里另写一个字面量。
// 原因是同一份默认值有两个消费者，而且它们对不上时不会报任何错：
//
//   - handleGetSettings 会把 settingDefaults 合并进设置页的响应，
//     那是管理员**看到**的"当前生效值"；
//   - 门控这里用它是**实际生效**的形态。
//
// 两处各写一遍的后果是"设置页显示已分离、实际仍在跟随配置文件"——
// 界面上没有任何异常，只有拿真域名试一次才看得出来。
// 变异验证第 145 条（把 settingDefaults 里的 site.layout 改成 split）
// 第一次就是绿的：不是因为护栏不严，而是因为门控根本没用那一份。
func siteLayoutDefault() string {
	if v, ok := settingDefaults["site.layout"]; ok && v != "" {
		return v
	}
	return siteLayoutAuto
}

// SiteLayoutMode 返回规范化后的站点形态取值（auto / single / split）。
//
// 导出给 cmd/xb-probe-master 在启动日志里回显"最终生效值"用：后台的设置项会
// 顶替配置文件里的对应项，只打印配置文件那一份，运维就无从确认到底哪个域名
// 能打开后台。
//
// 回落值走 siteLayoutDefault()（= settingDefaults 那一份），与设置页显示的是同一个。
func (h *Handler) SiteLayoutMode() string {
	switch v := strings.ToLower(strings.TrimSpace(h.settingOr("site.layout", siteLayoutDefault()))); v {
	case siteLayoutSingle:
		return siteLayoutSingle
	case siteLayoutSplit:
		return siteLayoutSplit
	default:
		return siteLayoutAuto
	}
}

// splitHostList 把一份 Host 清单拆成条目。
//
// 分隔符按最宽的形式认（逗号、分号、空白，含 \r 与换行）：这类清单几乎都是
// 从别处的配置或表格里复制来的，"多了一个换行导致整条被拒"是最容易劝退人的
// 一种校验。条目的合法性由 validateHostList 在保存时把关。
func splitHostList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool {
		switch r {
		case ',', ';', ' ', '\t', '\r', '\n':
			return true
		}
		return false
	})
}

// effectiveHosts 返回当前生效的公开站 / 管理端 Host 白名单。
//
// 设置项非空时**以设置项为准**（顶替 master.yaml 里的 public_hosts /
// admin_hosts）。之所以是顶替而不是并集：白名单是安全开关，管理员在设置页
// 看到的那份就应该是真正生效的那份；并集会让"后台删掉的域名依然放行"，
// 那又是一种看不见的失效。
//
// 空值表示"没在后台配过"，回落到配置文件；配置文件也为空则表示"不限制"。
func (h *Handler) effectiveHosts() (pub, adm []string) {
	pub, adm = h.PublicHosts, h.AdminHosts
	if v := strings.TrimSpace(h.settingOr("site.public_hosts", "")); v != "" {
		pub = splitHostList(v)
	}
	if v := strings.TrimSpace(h.settingOr("site.admin_hosts", "")); v != "" {
		adm = splitHostList(v)
	}
	return pub, adm
}

// EffectivePublicHosts / EffectiveAdminHosts 是 effectiveHosts 的单侧出口，
// 供启动日志回显"最终生效的是哪份清单"。
func (h *Handler) EffectivePublicHosts() []string {
	pub, _ := h.effectiveHosts()
	return pub
}

func (h *Handler) EffectiveAdminHosts() []string {
	_, adm := h.effectiveHosts()
	return adm
}

// hostsAfter 返回"把 in 这次提交算进去之后"的两份 Host 清单。
//
// 保存前的跨字段校验必须看**提交之后**的状态，而不是只看这次提交的字段：
// 管理员完全可以先存一次公开域名、再存一次管理域名，那样每一步都是合法的。
func (h *Handler) hostsAfter(in map[string]string) (pub, adm []string) {
	pub, adm = h.effectiveHosts()
	if v := strings.TrimSpace(in["site.public_hosts"]); v != "" {
		pub = splitHostList(v)
	}
	if v := strings.TrimSpace(in["site.admin_hosts"]); v != "" {
		adm = splitHostList(v)
	}
	return pub, adm
}

// isAdminPath 判断路径是否属于管理端（后台界面与管理 API）。
//
// 只认 /admin 与 /api/admin/：刻意不用"路径里含 admin"这种模糊判断，
// 否则 /administrator、/admin-old 都会被算成管理路径。
func isAdminPath(p string) bool {
	return p == "/admin" || strings.HasPrefix(p, "/admin/") || strings.HasPrefix(p, "/api/admin/")
}

// siteGate 是按路径生效的域名门控（站点形态的落地处）。
//
// adminEntry 表示本次调用来自"只服务管理端"的监听地址——只有在
// admin_listen 与 public_listen 不同的时候才会存在这样一个入口。
// 那种入口上不存在公开路径，因此它只做管理域名白名单，不参与
// "管理路径在公开入口要不要开放"的判断。
//
// **白名单与形态都在每次请求里现读**，不在构造时取一次。这一点是硬要求：
// 站点形态是可以在后台改的运行时设置，而门控是在进程启动时就装配好的——
// 若在构造时把清单读成一个闭包变量，"改完设置即时生效"就是假的：
// 界面上显示已分离、实际仍在放行，且没有任何报错。端点冒烟曾抓出过这个形态
// （单元测试里门控是就地构造的，所以完全看不到）。
func (h *Handler) siteGate(adminEntry bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pub, adm := h.effectiveHosts()
		layout := h.SiteLayoutMode()

		// 先决定"本次请求该按哪份清单判"，再交给 hostGate 做实际比对。
		// 把比对（含 ACME 豁免、端口归一化）留给 hostGate 一份实现，
		// 是为了不再出现"两处各写一遍、其中一处漏了豁免"的漂移——
		// 这个形态真的发生过：门控重写时只在一条分支上复用了 hostGate，
		// 另一条分支漏掉了 ACME 豁免，表现为证书续期几周后失败。
		var allowed []string
		switch {
		case layout == siteLayoutSingle:
			// 同站：不分彼此，任一台已配置的域名都能访问两边。
			allowed = append(append([]string{}, pub...), adm...)
		case !adminEntry && isAdminPath(r.URL.Path) && len(adm) == 0 && layout == siteLayoutSplit:
			// 明确要求"分离"、却没给出管理域名：宁可拒绝，也不能把管理后台
			// 悄悄留在一个"公开域名也能打开"的状态上——那正是选择分离的人要
			// 避免的事。保存时已拦过一次，这里是最后一道防线（例如配置文件
			// 被手改成了空清单）。
			writeMisdirected(w, r)
			return
		case !adminEntry && isAdminPath(r.URL.Path):
			// 管理路径按管理域名判。注意 auto 且没配管理域名时 adm 为空，
			// hostGate 对空清单不做限制——这正是升级前的行为，必须保留，
			// 否则存量部署会突然打不开后台。
			allowed = adm
		case adminEntry:
			// 只服务管理端的监听：只做管理域名白名单。
			allowed = adm
		default:
			allowed = pub
		}
		hostGate(allowed, next).ServeHTTP(w, r)
	})
}

// adminEntryURL 返回公开看板上「管理后台」按钮应当跳转的地址（含 /admin/）。
//
// 取值的优先级（越靠前越"显式"）：
//
//  1. 设置项 site.admin_url —— 管理员在后台显式指定的入口地址；
//  2. 站点形态是「同站」时直接返回空：此时相对路径 /admin/ 才是正确答案，
//     一个绝对地址反而可能因为写错域名而失真；
//  3. master.yaml 的 admin_origin（部署脚本会问出来并写进去）；
//  4. 「分离」且只给了一个管理域名时，用**本次请求的协议** + 那个域名拼出来。
//     走域名 + 反代的部署正是靠这条路自动得到 https://admin.example.com/admin/。
//
// 都拿不到就返回空，前端保留默认的 /admin/ —— 也就是升级前的行为。
// 这是**唯一**安全的方向：猜错一个域名比不猜更糟（用户会被送到一个陌生站点）。
func (h *Handler) adminEntryURL(r *http.Request) string {
	// 同站：显式的 site.admin_url 仍然尊重（有人就是要把按钮指向别处）。
	if v := strings.TrimSpace(h.settingOr("site.admin_url", "")); v != "" {
		return withAdminPath(v)
	}
	if h.SiteLayoutMode() == siteLayoutSingle {
		return ""
	}
	if v := strings.TrimSpace(h.AdminOrigin); v != "" {
		return withAdminPath(v)
	}
	_, adm := h.effectiveHosts()
	if len(adm) == 1 {
		scheme := "http"
		if h.requestSecure(r) {
			scheme = "https"
		}
		return withAdminPath(scheme + "://" + adm[0])
	}
	return ""
}

// withAdminPath 把 origin 补成"后台入口地址"。
//
// 幂等：给 "https://admin.example.com" 返回 ".../admin/"，
// 给已经带路径的 ".../admin/" 原样返回（不重复追加）。
func withAdminPath(origin string) string {
	v := strings.TrimRight(strings.TrimSpace(origin), "/")
	if v == "" {
		return ""
	}
	if strings.HasSuffix(v, "/admin") {
		return v + "/"
	}
	return v + "/admin/"
}

// validateSiteLayoutFor 做站点形态的跨字段校验（保存前整体判断）。
//
// 唯一会被拒的组合是"选了分离、却只给了一个域名"：那种状态下管理后台会在
// 保存成功之后的**下一个请求**上返回 421，管理员被锁在门外——而这个错误没法
// 再从后台改回来，因为改它本身就需要访问后台。
func (h *Handler) validateSiteLayoutFor(in map[string]string) error {
	layout := h.SiteLayoutMode()
	if v, ok := in["site.layout"]; ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case siteLayoutSingle:
			layout = siteLayoutSingle
		case siteLayoutSplit:
			layout = siteLayoutSplit
		default:
			layout = siteLayoutAuto
		}
	}
	if layout != siteLayoutSplit {
		return nil
	}
	pub, adm := h.hostsAfter(in)
	if len(pub) == 0 || len(adm) == 0 {
		return fmt.Errorf("站点形态选「分离（两个域名）」时必须同时给出公开域名与管理域名" +
			"（可写在 master.yaml 的 public_hosts / admin_hosts，或填在下面两项里），" +
			"否则管理后台会因为域名不匹配而无法访问")
	}
	return nil
}

// logSiteLayoutChange 在站点形态被改动时落一条日志。
//
// 这类改动的症状是"后台突然打不开了""某个域名突然 421 了"，而它不伴随重启，
// 只看现场很容易误判成网络或证书问题。把"什么时候把形态改成了什么、最终生效
// 的清单是什么"写进日志，是把这类悬案变成可追查的一件事。
func (h *Handler) logSiteLayoutChange(in map[string]string) {
	touched := false
	for _, k := range siteLayoutKeys {
		if _, ok := in[k]; ok {
			touched = true
			break
		}
	}
	if !touched || h.Logger == nil {
		return
	}
	pub, adm := h.hostsAfter(in)
	h.Logger.Info("站点形态已更新",
		"layout", h.SiteLayoutMode(),
		"public_hosts", pub, "admin_hosts", adm)
}
