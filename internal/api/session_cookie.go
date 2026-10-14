package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/koyoca/xb-probe/internal/auth"
)

// 会话 Cookie 的协议感知下发（9.3 / S2）。
//
// 背景：__Host- 前缀 + Secure 是浏览器的硬性约束，只有 HTTPS 下才成立。
// 本项目支持 tls.mode=none（由前置 Caddy/Nginx 卸载 TLS，或本机内网调试）。
// 此时若无脑下发 Secure Cookie，浏览器会直接丢弃该响应头——服务端日志
// 一切正常（200 + Set-Cookie），前端却因为拿不到会话而立刻被打回登录页。
// 这是本项目最容易被误判为"后端坏了"的故障。
//
// 同一个判定还决定了 HSTS 要不要下发：在明文 HTTP 上下发 HSTS 是无意义的
// （浏览器按规范直接忽略），反而让人误以为已经加固。两处必须共用这一份判定，
// 否则会出现"Cookie 已经降级、HSTS 却还在发"这种自相矛盾的状态。

// requestSecure 判定当前请求是否由客户端以 HTTPS 发起。
//
// 判定顺序：
//  1. 本机 TLS 已终止（r.TLS != nil）→ 是。
//  2. TLSActive（本进程就是 TLS 终点：files / selfsigned / acme）→ 是。
//  3. tls.mode 本身就是终止型（兜底，防调用方漏设 TLSActive）→ 是。
//  4. 对端是可信代理（trusted_proxies）或**对端本身就是 Cloudflare 边缘**，
//     且 X-Forwarded-Proto: https → 是。
//  5. 其余（含无代理的纯 HTTP）→ 否。
//
// 第 4 条只对可信那一跳生效。否则任意客户端都能伪造 X-Forwarded-Proto: https
// 骗到一个 Secure Cookie，把自己锁在门外；不可信输入不该影响安全决策。
//
// 为什么把 CF 边缘也算进来：套了 Cloudflare 时 Master 看到的对端就是 CF 边缘
// 地址，而 trusted_proxies 里通常只有自家反代/回环，于是明明对外是 HTTPS、
// 会话 Cookie 却被降级成明文形态（S2 的缓解手段失效）。CF 边缘地址无法伪造，
// 且 CF 会覆盖写 X-Forwarded-Proto，因此这条信任与 trusted_proxies 同源同理。
func (h *Handler) requestSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if h.TLSActive {
		return true
	}
	switch h.TLSMode {
	case "files", "selfsigned", "acme":
		return true
	}
	peer := remoteHost(r.RemoteAddr)
	if isTrusted(peer, parseCIDRs(h.TrustedProxies)) || cfEdge(peer) {
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
			return true
		}
	}
	return false
}

// setSessionCookie 按请求的真实协议写入会话 Cookie。
func (h *Handler) setSessionCookie(w http.ResponseWriter, r *http.Request, plainID string, maxAge int) {
	secure := h.requestSecure(r)
	if !secure && h.insecureCookieWarned.CompareAndSwap(false, true) {
		if h.Logger != nil {
			h.Logger.Warn("管理端未走 HTTPS，会话 Cookie 已降级为 probe_session（不带 Secure）。" +
				"登录可正常工作，但会话明文传输；生产环境请启用 TLS 或置于 HTTPS 反代之后。")
		}
	}
	auth.SetSessionCookie(w, plainID, maxAge, secure)
}

// sessionLimits 返回当前生效的会话限制。
//
// 后台设置项 admin.session_ttl_hours 是"登录有效期"（小时）：从登录那一刻算起，
// 到点必须重新登录。空闲上限固定走 auth.SessionIdleTTL（页面停在那里不动的情况，
// 由它兜底），两条线取先到的那条。
//
// 管理员把它调大（比如 720 = 30 天）不会让会话变成"永久"：绝对上限始终存在，
// 这是"过期后必须重新登录"这句话的兑现方式。
func (h *Handler) sessionLimits() auth.SessionLimits {
	lim := auth.DefaultSessionLimits()
	hours := h.Repo.GetIntSetting("admin.session_ttl_hours", 12)
	if hours <= 0 {
		// 库里的值可能来自更早的版本或被手工改过；非法值一律回到默认，
		// 不允许出现"有效期 0"这种把所有人立刻踢下线的状态。
		hours = 12
	}
	lim.Absolute = time.Duration(hours) * time.Hour
	return lim
}
