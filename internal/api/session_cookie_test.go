package api

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/koyoca/xb-probe/internal/auth"
)

// 会话 Cookie 的协议感知（本次修复的核心回归测试）。
//
// 线上故障形态：部署为 tls.mode=none（纯 HTTP）时，服务端下发
// `Set-Cookie: __Host-probe_session=...; Secure`。浏览器对 __Host- 前缀有
// 硬性要求（必须 Secure 且不带 Domain），而 http:// 下带 Secure 的 Cookie
// 会被直接丢弃——两者叠加导致登录接口返回 200 却永远建立不了会话，
// 前端表现为"登录成功却立刻被打回登录页"。服务端日志完全正常，极难排查。

func newCookieTestHandler(tlsMode string, trustedProxies []string) *Handler {
	return &Handler{Deps: Deps{TLSMode: tlsMode, TrustedProxies: trustedProxies}}
}

// 纯 HTTP 且无代理：必须使用不带前缀、不带 Secure 的 Cookie。
func TestSessionCookiePlainHTTP(t *testing.T) {
	h := newCookieTestHandler("none", nil)
	r := httptest.NewRequest(http.MethodPost, "http://example.test/api/admin/v1/auth/login", nil)

	if h.requestSecure(r) {
		t.Fatal("纯 HTTP 下 requestSecure 必须为 false")
	}

	w := httptest.NewRecorder()
	h.setSessionCookie(w, r, "sess-value", 3600)

	raw := w.Header().Get("Set-Cookie")
	if raw == "" {
		t.Fatal("未下发 Set-Cookie")
	}
	if strings.Contains(raw, "__Host-") {
		t.Errorf("纯 HTTP 下不得使用 __Host- 前缀（浏览器会丢弃）：%s", raw)
	}
	if strings.Contains(raw, "Secure") {
		t.Errorf("纯 HTTP 下不得带 Secure（浏览器会丢弃）：%s", raw)
	}
	if !strings.Contains(raw, auth.PlainCookieName+"=sess-value") {
		t.Errorf("Cookie 名/值不符：%s", raw)
	}
	// HttpOnly 与 SameSite=Strict 在两种协议下都必须保留。
	if !strings.Contains(raw, "HttpOnly") || !strings.Contains(raw, "SameSite=Strict") {
		t.Errorf("缺少 HttpOnly 或 SameSite=Strict：%s", raw)
	}
}

// 本机 TLS：必须使用 __Host- 前缀 + Secure。
func TestSessionCookieOverTLS(t *testing.T) {
	h := newCookieTestHandler("none", nil)
	r := httptest.NewRequest(http.MethodPost, "https://example.test/api/admin/v1/auth/login", nil)
	r.TLS = &tls.ConnectionState{}

	if !h.requestSecure(r) {
		t.Fatal("r.TLS 非空时 requestSecure 必须为 true")
	}

	w := httptest.NewRecorder()
	h.setSessionCookie(w, r, "sess-value", 3600)
	raw := w.Header().Get("Set-Cookie")

	if !strings.HasPrefix(raw, auth.SecureCookieName+"=") {
		t.Errorf("HTTPS 下 Cookie 名应为 %s：%s", auth.SecureCookieName, raw)
	}
	if !strings.Contains(raw, "Secure") {
		t.Errorf("HTTPS 下必须带 Secure：%s", raw)
	}
}

// tls.mode 为终止型（files/selfsigned/acme）时，本进程就是 TLS 终点，
// 即便拿到的是裸请求也应判为安全。
func TestSessionCookieTLSModeFiles(t *testing.T) {
	for _, mode := range []string{"files", "selfsigned", "acme"} {
		h := newCookieTestHandler(mode, nil)
		r := httptest.NewRequest(http.MethodPost, "http://example.test/api/admin/v1/auth/login", nil)
		if !h.requestSecure(r) {
			t.Errorf("tls.mode=%s 时 requestSecure 应为 true", mode)
		}
	}
}

// TLSActive 是 main 按 cfg.TLSActive() 注入的显式判定；即使 TLSMode 字符串
// 没写对（或将来新增了模式忘了同步 switch），TLSActive 也要能单独决定结果。
func TestRequestSecureHonoursTLSActive(t *testing.T) {
	h := &Handler{Deps: Deps{TLSActive: true}}
	r := httptest.NewRequest(http.MethodPost, "http://example.test/api/admin/v1/auth/login", nil)
	if !h.requestSecure(r) {
		t.Fatal("TLSActive=true 时应判为安全（不依赖 TLSMode 字符串）")
	}

	h2 := &Handler{Deps: Deps{TLSActive: false, TLSMode: "none"}}
	if h2.requestSecure(r) {
		t.Fatal("TLSActive=false 且 mode=none 且无代理时应判为不安全")
	}
}

// 前置 HTTPS 反代：只有在可信代理名单命中时才采信 X-Forwarded-Proto。
func TestSessionCookieForwardedProto(t *testing.T) {
	// 命中可信代理 → 采信。
	h := newCookieTestHandler("none", []string{"127.0.0.1", "10.0.0.0/8"})
	r := httptest.NewRequest(http.MethodPost, "http://example.test/api/admin/v1/auth/login", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-Proto", "https")
	if !h.requestSecure(r) {
		t.Error("可信代理 + X-Forwarded-Proto: https 应判为安全")
	}

	// 不可信来源伪造同头 → 不采信，否则会把用户自己锁在门外。
	r2 := httptest.NewRequest(http.MethodPost, "http://example.test/api/admin/v1/auth/login", nil)
	r2.RemoteAddr = "203.0.113.9:5000"
	r2.Header.Set("X-Forwarded-Proto", "https")
	if h.requestSecure(r2) {
		t.Error("不可信来源伪造 X-Forwarded-Proto 不应被采信")
	}

	// 可信代理但转发的是 http → 不采信。
	r3 := httptest.NewRequest(http.MethodPost, "http://example.test/api/admin/v1/auth/login", nil)
	r3.RemoteAddr = "127.0.0.1:5000"
	r3.Header.Set("X-Forwarded-Proto", "http")
	if h.requestSecure(r3) {
		t.Error("X-Forwarded-Proto: http 不应判为安全")
	}
}

// 清理时必须把两种名字都清掉，否则协议切换后会残留旧会话。
func TestClearSessionCookieClearsBothNames(t *testing.T) {
	w := httptest.NewRecorder()
	auth.ClearSessionCookie(w)

	all := strings.Join(w.Header().Values("Set-Cookie"), "\n")
	for _, name := range []string{auth.SecureCookieName, auth.PlainCookieName} {
		if !strings.Contains(all, name+"=") {
			t.Errorf("登出未清理 %s：%s", name, all)
		}
	}
	if !strings.Contains(all, "Max-Age=0") {
		t.Errorf("清理 Cookie 应带 Max-Age=0：%s", all)
	}
}

// 读取时两种名字都能识别，协议切换期间存量会话不会立刻失效。
func TestReadSessionCookieAcceptsBothNames(t *testing.T) {
	for _, name := range []string{auth.SecureCookieName, auth.PlainCookieName} {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/api/admin/v1/auth/me", nil)
		r.AddCookie(&http.Cookie{Name: name, Value: "abc123"})
		got, ok := auth.ReadSessionCookie(r)
		if !ok || got != "abc123" {
			t.Errorf("%s 应可读取，得到 (%q,%v)", name, got, ok)
		}
	}

	// 无 Cookie → 明确返回未命中，不能返回空串却报 ok。
	r := httptest.NewRequest(http.MethodGet, "http://example.test/api/admin/v1/auth/me", nil)
	if _, ok := auth.ReadSessionCookie(r); ok {
		t.Error("无 Cookie 时不应返回 ok")
	}
}
