package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yourorg/probe/internal/acme"
	"github.com/yourorg/probe/internal/model"
)

// ---------------------------------------------------------------------------
// 6.1 host 白名单
// ---------------------------------------------------------------------------

func serveGate(t *testing.T, allowed []string, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("inner"))
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	req.Host = host
	hostGate(allowed, inner).ServeHTTP(rec, req)
	return rec
}

func TestHostGateDisabledWithoutAllowList(t *testing.T) {
	rec := serveGate(t, nil, "anything.example.com", "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("未配置白名单时不应拦截，实际 %d", rec.Code)
	}
}

func TestHostGateRejectsMismatchedHost(t *testing.T) {
	// 公开域名访问管理端 —— 这正是 6.1 要挡住的场景。
	rec := serveGate(t, []string{"admin.example.com"}, "status.example.com", "/admin/")
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("域名不匹配应返回 421，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "421") {
		t.Fatalf("提示页应说明 421，实际: %s", rec.Body.String())
	}

	// 接口路径返回 JSON，便于脚本/前端识别。
	rec = serveGate(t, []string{"admin.example.com"}, "status.example.com", "/api/admin/v1/auth/me")
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("接口应返回 421，实际 %d", rec.Code)
	}
	var env model.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("接口 421 应为 JSON 错误信封: %s", rec.Body.String())
	}
	if env.Error.Code != "misdirected_request" {
		t.Fatalf("错误码应为 misdirected_request，实际 %q", env.Error.Code)
	}
}

func TestHostGateAcceptsDefaultPortVariants(t *testing.T) {
	// 白名单写主域名，请求带默认端口 —— 在浏览器眼里是同一个源。
	for _, host := range []string{"admin.example.com", "admin.example.com:443", "ADMIN.example.com.", "admin.example.com:80"} {
		rec := serveGate(t, []string{"admin.example.com"}, host, "/admin/")
		if rec.Code != http.StatusOK {
			t.Fatalf("Host=%q 应被放行（默认端口/大小写/尾点差异不应算不同源），实际 %d", host, rec.Code)
		}
	}
	// 反向：白名单带默认端口，请求不带。
	rec := serveGate(t, []string{"admin.example.com:443"}, "admin.example.com", "/admin/")
	if rec.Code != http.StatusOK {
		t.Fatalf("白名单带 :443 时应与不带端口的 Host 等价，实际 %d", rec.Code)
	}
}

func TestHostGateRequiresNonDefaultPortMatch(t *testing.T) {
	// :8443 与 :443 是两个不同的源，不能互相顶替。
	if rec := serveGate(t, []string{"admin.example.com:8443"}, "admin.example.com", "/admin/"); rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("非默认端口不匹配时应拒绝，实际 %d", rec.Code)
	}
	if rec := serveGate(t, []string{"admin.example.com:8443"}, "admin.example.com:8443", "/admin/"); rec.Code != http.StatusOK {
		t.Fatalf("端口一致时应放行，实际 %d", rec.Code)
	}
	if rec := serveGate(t, []string{"admin.example.com"}, "admin.example.com:8443", "/admin/"); rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("请求带了非默认端口而白名单没有，应拒绝，实际 %d", rec.Code)
	}
}

// TestHostGateSkipsACMEChallenge：CA 是按 IP 访问的，Host 是 IP 字面量，
// 永远不会命中域名白名单，因此挑战路径必须无条件放行。
func TestHostGateSkipsACMEChallenge(t *testing.T) {
	rec := serveGate(t, []string{"admin.example.com"}, "203.0.113.10", acme.HTTP01Prefix+"tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("ACME 挑战路径不应受 host 白名单限制，实际 %d", rec.Code)
	}
}

func TestNormalizeHostPort(t *testing.T) {
	cases := map[string]string{
		"Example.COM":          "example.com",
		"example.com.":         "example.com",
		"example.com:443":      "example.com",
		"example.com:80":       "example.com",
		"example.com:8443":     "example.com:8443",
		"[::1]:443":            "::1",
		"::1":                  "::1",
		"  Example.com:8443  ": "example.com:8443",
		"":                     "",
	}
	for in, want := range cases {
		if got := normalizeHostPort(in); got != want {
			t.Fatalf("normalizeHostPort(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// HSTS 与安全响应头
// ---------------------------------------------------------------------------

func serveHeaders(t *testing.T, policy SecurityPolicy, allowNonce bool, url string) http.Header {
	t.Helper()
	var nonce func() bool
	if allowNonce {
		nonce = func() bool { return true }
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	SecurityHeaders(policy, nonce, next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec.Header()
}

func TestHSTSOnlyEmittedOnSecureRequests(t *testing.T) {
	policy := SecurityPolicy{
		HSTS:   HSTSConfig{Enabled: true, MaxAge: 31536000},
		Secure: func(*http.Request) bool { return true },
	}
	h := serveHeaders(t, policy, false, "https://example.com/")
	if got := h.Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Fatalf("HTTPS 下应下发 HSTS，实际 %q", got)
	}

	// 明文连接：HSTS 必须缺席（浏览器会忽略，发了只会误导运维）。
	policy.Secure = func(*http.Request) bool { return false }
	h = serveHeaders(t, policy, false, "http://example.com/")
	if got := h.Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("明文连接不应下发 HSTS，实际 %q", got)
	}
}

func TestHSTSIncludeSubdomainsAndPreload(t *testing.T) {
	policy := SecurityPolicy{
		HSTS:   HSTSConfig{Enabled: true, MaxAge: 63072000, IncludeSubDomains: true, Preload: true},
		Secure: func(*http.Request) bool { return true },
	}
	got := serveHeaders(t, policy, false, "https://example.com/").Get("Strict-Transport-Security")
	want := "max-age=63072000; includeSubDomains; preload"
	if got != want {
		t.Fatalf("HSTS 头应为 %q，实际 %q", want, got)
	}
}

func TestHSTSDisabledEmitsNothing(t *testing.T) {
	policy := SecurityPolicy{
		HSTS:   HSTSConfig{Enabled: false, MaxAge: 31536000},
		Secure: func(*http.Request) bool { return true },
	}
	if got := serveHeaders(t, policy, false, "https://example.com/").Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("显式关闭后不应下发 HSTS，实际 %q", got)
	}
}

// TestSecurityHeadersNilSecureIsSafe：没注入判定函数时宁可不发 HSTS。
func TestSecurityHeadersNilSecureIsSafe(t *testing.T) {
	policy := SecurityPolicy{HSTS: HSTSConfig{Enabled: true, MaxAge: 31536000}}
	h := serveHeaders(t, policy, false, "https://example.com/")
	if got := h.Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("缺少 Secure 判定时不应下发 HSTS，实际 %q", got)
	}
	// 与协议无关的头必须始终存在。
	for _, name := range []string{
		"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy",
		"Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy",
	} {
		if h.Get(name) == "" {
			t.Fatalf("缺少安全响应头 %s", name)
		}
	}
	if got := h.Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
		t.Fatalf("CORP 应为 same-origin，实际 %q", got)
	}
}

func TestSecurityHeadersNonceInjectedOnlyWhenAllowed(t *testing.T) {
	policy := SecurityPolicy{}
	withNonce := serveHeaders(t, policy, true, "https://example.com/").Get("Content-Security-Policy")
	if !strings.Contains(withNonce, "nonce-") {
		t.Fatalf("开启脚本注入时应带 nonce，实际 %q", withNonce)
	}
	without := serveHeaders(t, policy, false, "https://example.com/").Get("Content-Security-Policy")
	if strings.Contains(without, "nonce-") {
		t.Fatalf("未开启脚本注入时不应带 nonce，实际 %q", without)
	}
}

// ---------------------------------------------------------------------------
// /api/admin/v1/tls/status
// ---------------------------------------------------------------------------

type fakeTLSStatusReader struct{ st model.TLSStatus }

func (f fakeTLSStatusReader) TLSStatus() model.TLSStatus { return f.st }

func TestTLSStatusHandlerReturnsReader(t *testing.T) {
	h := &Handler{Deps: Deps{TLSInfo: fakeTLSStatusReader{st: model.TLSStatus{
		Mode: "acme", Source: "acme", Challenge: "http-01",
		Domains: []string{"status.example.com"}, SPKISHA256: "abc",
	}}}}
	rec := httptest.NewRecorder()
	h.handleTLSStatus(rec, httptest.NewRequest(http.MethodGet, "/api/admin/v1/tls/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d", rec.Code)
	}
	var got model.TLSStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %s", rec.Body.String())
	}
	if got.Mode != "acme" || got.Challenge != "http-01" || got.SPKISHA256 != "abc" {
		t.Fatalf("未回显证书运行态: %+v", got)
	}
}

// TestTLSStatusHandlerWithoutReader：没注入提供方时也要能回一个最小状态，
// 而不是 500 —— 后台卡片据此判断"当前没启用 TLS"。
func TestTLSStatusHandlerWithoutReader(t *testing.T) {
	h := &Handler{Deps: Deps{TLSMode: "none"}}
	rec := httptest.NewRecorder()
	h.handleTLSStatus(rec, httptest.NewRequest(http.MethodGet, "/api/admin/v1/tls/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d", rec.Code)
	}
	var got model.TLSStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %s", rec.Body.String())
	}
	if got.Mode != "none" {
		t.Fatalf("mode 应回显为 none，实际 %q", got.Mode)
	}
}

// TestTLSStatusEndpointIsAdminOnly：该接口暴露证书指纹等信息，必须有会话。
func TestTLSStatusEndpointIsAdminOnly(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.AdminRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/v1/tls/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录访问应 401，实际 %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// ACME 挑战在关闭公开站时仍可达
// ---------------------------------------------------------------------------

func TestACMEChallengePathClassification(t *testing.T) {
	if !isACMEChallengePath(acme.HTTP01Prefix + "tok") {
		t.Fatal("挑战路径应被识别")
	}
	if isACMEChallengePath("/api/public/v1/site") {
		t.Fatal("普通路径不应被识别为挑战路径")
	}
}
