package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// 请求防护与安全响应头（文档 9.2 / 13.4.5 / S5 / S9）。

// ctxKeyRealIP 用于在请求上下文中传递解析后的真实来源 IP。
type ctxKeyRealIP struct{}

// RealIP 返回解析后的来源 IP。
func RealIP(r *http.Request) string {
	if v, ok := r.Context().Value(ctxKeyRealIP{}).(string); ok {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RealIPMiddleware 解析来源 IP（S5）。
// 仅当 TCP 对端命中 trustedProxies 时才解析 X-Forwarded-For；默认为空。
func RealIPMiddleware(trustedProxies []string, next http.Handler) http.Handler {
	trusted := parseCIDRs(trustedProxies)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := remoteHost(r.RemoteAddr)
		if isTrusted(ip, trusted) {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				ip = rightmostUntrusted(xff, trusted)
			}
		}
		ctx := context.WithValue(r.Context(), ctxKeyRealIP{}, ip)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func remoteHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func parseCIDRs(list []string) []*net.IPNet {
	var out []*net.IPNet
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			if ip := net.ParseIP(s); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				if _, n, err := net.ParseCIDR(s + "/" + itoa(bits)); err == nil {
					out = append(out, n)
				}
			}
			continue
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func isTrusted(ip string, cidrs []*net.IPNet) bool {
	p := net.ParseIP(ip)
	if p == nil || len(cidrs) == 0 {
		return false
	}
	for _, n := range cidrs {
		if n.Contains(p) {
			return true
		}
	}
	return false
}

// rightmostUntrusted 从 XFF 右侧（代理追加端）开始，跳过可信代理，
// 返回第一个不可信地址——这是最不容易被伪造的位置。
func rightmostUntrusted(xff string, trusted []*net.IPNet) string {
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := strings.TrimSpace(parts[i])
		if ip == "" {
			continue
		}
		if !isTrusted(ip, trusted) {
			return ip
		}
	}
	return strings.TrimSpace(parts[0])
}

// ctxKeyNonce 传递本次请求的 CSP nonce（仅在开启脚本注入时存在）。
type ctxKeyNonce struct{}

// NonceFrom 读取本次请求的 CSP nonce；未开启脚本注入时返回空串。
func NonceFrom(r *http.Request) string {
	v, _ := r.Context().Value(ctxKeyNonce{}).(string)
	return v
}

// HSTSConfig 是 Strict-Transport-Security 的下发策略（来自 master.yaml 的 tls.hsts）。
type HSTSConfig struct {
	Enabled           bool
	MaxAge            int
	IncludeSubDomains bool
	Preload           bool
}

// header 组装 HSTS 头；未启用时返回空串（表示不下发该头）。
func (c HSTSConfig) header() string {
	if !c.Enabled || c.MaxAge <= 0 {
		return ""
	}
	v := "max-age=" + itoa(c.MaxAge)
	if c.IncludeSubDomains {
		v += "; includeSubDomains"
	}
	if c.Preload {
		v += "; preload"
	}
	return v
}

// SecurityPolicy 是安全响应头的下发策略。
type SecurityPolicy struct {
	HSTS HSTSConfig
	// Secure 判定"这次请求是不是 HTTPS 发起的"。为 nil 时按"绝不发 HSTS"处理
	// （宁可不发，也不要在明文连接上发一个被浏览器忽略、却误导人的头）。
	Secure func(*http.Request) bool
}

// SecurityHeaders 输出 13.4.5 规定的响应头。
//
// allowScriptNonce 返回 true 时（公开站开启了脚本注入），本次请求生成一个随机 nonce：
// CSP 的 script-src 带上它，注入的 <script nonce="..."> 才被放行；
// 同时把 nonce 放进请求上下文，供 handleThemeAsset 替换 {{NONCE}}。
//
// 管理端恒传 false：管理端不允许任何注入内容，CSP 始终为最严。
func SecurityHeaders(policy SecurityPolicy, allowScriptNonce func() bool, next http.Handler) http.Handler {
	const baseCSP = "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; " +
		"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		csp := baseCSP
		if allowScriptNonce != nil && allowScriptNonce() {
			if nonce, err := randomNonce(); err == nil {
				// script-src 追加 nonce 后，内联脚本只有带该 nonce 才执行。
				csp = strings.Replace(csp, "script-src 'self';",
					"script-src 'self' 'nonce-"+nonce+"';", 1)
				r = r.WithContext(context.WithValue(r.Context(), ctxKeyNonce{}, nonce))
			}
		}
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		// 公开站与管理端分离为不同源（6.1）之后，资源不该被对方页面内联引用；
		// same-origin 能让"跨源偷偷引用"在浏览器层直接失败。
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if policy.Secure != nil && policy.Secure(r) {
			if v := policy.HSTS.header(); v != "" {
				h.Set("Strict-Transport-Security", v)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// randomNonce 生成 16 字节随机数的 base64 编码（CSP nonce 用）。
func randomNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}

// BodyLimit 限制请求体大小（9.2 / S9）。超限由处理函数映射为 413。
func BodyLimit(maxBytes int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// 令牌桶限速（9.1 / S9）
// ---------------------------------------------------------------------------

// IPLimiter 按来源 IP 或令牌做限速。
type IPLimiter struct {
	mu       sync.Mutex
	limiters map[string]*entry
	rate     rate.Limit
	burst    int
	ttl      time.Duration
}

type entry struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewIPLimiter 创建限速器。perMinute 为每分钟允许次数，burst 为突发容量。
func NewIPLimiter(perMinute, burst int) *IPLimiter {
	if perMinute <= 0 {
		perMinute = 60
	}
	if burst <= 0 {
		burst = perMinute
	}
	l := &IPLimiter{
		limiters: make(map[string]*entry),
		rate:     rate.Limit(float64(perMinute) / 60.0),
		burst:    burst,
		ttl:      10 * time.Minute,
	}
	go l.gc()
	return l
}

// Allow 判断某个键是否放行。
func (l *IPLimiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	e, ok := l.limiters[key]
	if !ok {
		e = &entry{lim: rate.NewLimiter(l.rate, l.burst)}
		l.limiters[key] = e
	}
	e.seen = now
	lim := e.lim
	l.mu.Unlock()
	return lim.Allow()
}

func (l *IPLimiter) gc() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		cut := time.Now().Add(-l.ttl)
		l.mu.Lock()
		for k, e := range l.limiters {
			if e.seen.Before(cut) {
				delete(l.limiters, k)
			}
		}
		l.mu.Unlock()
	}
}

// RateLimitMiddleware 按来源 IP 限速，超限返回 429。
func RateLimitMiddleware(l *IPLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(RealIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "请求过于频繁，请稍后重试")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
