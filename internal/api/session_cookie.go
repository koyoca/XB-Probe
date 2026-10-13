package api

import (
	"net/http"
	"strings"

	"github.com/yourorg/probe/internal/auth"
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
//  4. 对端命中 trusted_proxies 且 X-Forwarded-Proto: https → 是。
//  5. 其余（含无代理的纯 HTTP）→ 否。
//
// 第 4 条只对可信代理生效。否则任意客户端都能伪造 X-Forwarded-Proto: https
// 骗到一个 Secure Cookie，把自己锁在门外；不可信输入不该影响安全决策。
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
	if isTrusted(remoteHost(r.RemoteAddr), parseCIDRs(h.TrustedProxies)) {
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
