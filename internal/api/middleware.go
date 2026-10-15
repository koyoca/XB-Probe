package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
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
// 仅当 TCP 对端命中 trustedProxies（或本身就是 Cloudflare 边缘）时才解析代理头。
//
// extra 是**每次请求现取**的补充可信网段（后台的 CDN 列表）：CDN 网段随设置
// 增删，不能在启动时定死；返回 nil/空表示未启用。静态部分照旧只解析一次。
func RealIPMiddleware(trustedProxies []string, extra func() []string, next http.Handler) http.Handler {
	static := parseCIDRs(trustedProxies)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trusted := static
		if extra != nil {
			if e := parseCIDRs(extra()); len(e) > 0 {
				trusted = make([]*net.IPNet, 0, len(static)+len(e))
				trusted = append(trusted, static...)
				trusted = append(trusted, e...)
			}
		}
		ctx := context.WithValue(r.Context(), ctxKeyRealIP{}, resolveClientIP(r, trusted))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// cdnRangeList 读取后台的 CDN 边缘网段清单（设置 cdn.enabled / cdn.ranges）。
// 未启用时返回 nil。每次现读：列表可以在后台随时增删，不需要重启 Master。
func (h *Handler) cdnRangeList() []string {
	if h.Repo == nil {
		return nil
	}
	if h.Repo.GetSettingDefault("cdn.enabled", "0") != "1" {
		return nil
	}
	return splitCIDRList(h.Repo.GetSettingDefault("cdn.ranges", ""))
}

// trustCIDRs 是"这一跳算不算代理"的唯一取值来源：
// 配置文件里的 trusted_proxies + 后台 CDN 列表（开启时）。
//
// RealIPMiddleware 之外的调用方（Agent IP 白名单、会话 Cookie 的 Secure 判定）
// 必须都用它——三处各拼各的清单，迟早有一处漏掉 CDN 网段，表现就是
// "同一个请求，日志里是真实客户端、Cookie 却降级 / 白名单却拒绝"。
func (h *Handler) trustCIDRs() []*net.IPNet {
	c := parseCIDRs(h.TrustedProxies)
	if e := parseCIDRs(h.cdnRangeList()); len(e) > 0 {
		c = append(c, e...)
	}
	return c
}

// resolveClientIP 是"这个请求到底是谁发的"的唯一判据（中间件与 Agent IP 白名单共用）。
//
// 大前提：**只有当这一跳确实是代理时**才去看它带来的头；对端不可信时一律用对端地址。
// 否则任何人都能自己写一个 X-Forwarded-For / CF-Connecting-IP，把自己洗进白名单。
//
// 头的优先级（均以"这一跳可信"为前提）：
//  1. CF-Connecting-IP：Cloudflare 专用头，**单值**，由 CF 边缘覆盖写入，比 XFF 准
//     （XFF 是"追加"语义，客户端可以先塞一个假值）。但它只有在能证明链路确实过了
//     CF 时才采信——见下面 cloudflareSawEdge 的两条证明路径。
//  2. X-Forwarded-For：从右往左跳过代理跳（可信代理 + CF 边缘），取第一个真实地址。
//  3. X-Real-IP：nginx 单值头的兜底。
//
// 真机反馈（本条 bug）：Master 套了 Cloudflare 之后，节点地址被写成 172.68.x.x
// （CF 边缘的回源地址）。链路是 CF → 本机 Caddy → Master：XFF 最右边那一跳是 CF
// 边缘，而 trusted_proxies 里只写了回环，于是解析停在 CF 那一跳上，把 CF 自己
// 当成了节点地址。
func resolveClientIP(r *http.Request, trusted []*net.IPNet) string {
	peer := remoteHost(r.RemoteAddr)

	// 路径 A：对端本身就是 CF 边缘（CF 直接回源，或 CF 前面的代理已退出链路）。
	// "来源地址落在 CF 网段"这件事无法伪造：TCP 三次握手要求对方真能从该网段发包。
	if cfEdge(peer) {
		if ip := clientIPHeader(r, "CF-Connecting-IP"); ip != "" {
			return ip
		}
		if ip, _ := walkXFF(r.Header.Get("X-Forwarded-For"), trusted); ip != "" {
			return ip
		}
		return peer
	}
	if !isTrusted(peer, trusted) {
		return peer
	}

	// 路径 B：对端是（自建的）可信代理。此时 CF 只可能出现在 XFF 链里——
	// 链里出现 CF 边缘这一跳，就等于证明"这次请求确实过了 CF"，于是 CF 写的
	// CF-Connecting-IP 才可信。没有这一跳时绝不采信该头：旁路 CF 直连你反代的
	// 人也能塞一个同名头。
	xff := r.Header.Get("X-Forwarded-For")
	ip, sawCF := walkXFF(xff, trusted)
	if sawCF {
		if v := clientIPHeader(r, "CF-Connecting-IP"); v != "" {
			return v
		}
	}
	if ip != "" {
		return ip
	}
	if v := clientIPHeader(r, "X-Real-IP"); v != "" {
		return v
	}
	return peer
}

// clientIPHeader 读一个单值来源头，只接受**合法 IP**。
// 非法值（空、带端口、带逗号、任意字符串）一律当没给——否则一个畸形头就能
// 把节点地址写成一句垃圾。
func clientIPHeader(r *http.Request, name string) string {
	v := strings.TrimSpace(r.Header.Get(name))
	if v == "" || strings.ContainsAny(v, ", ") {
		return ""
	}
	if net.ParseIP(v) == nil {
		return ""
	}
	return v
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

// walkXFF 从 XFF 右侧（代理追加端）开始，跳过所有"中间跳"，返回第一个真实地址。
//
// 跳过的两类：
//  1. 命中所配 trusted_proxies 的地址（管理员声明的反代）；
//  2. Cloudflare 边缘网段（见 cloudflareEdgeRanges：这几段只可能是 CF 自己）。
//
// 第二个返回值表示"链里出现过 CF 边缘"——它是"这次请求确实过了 CF"的证明，
// 调用方据此才敢采信 CF-Connecting-IP。
//
// 取右侧而不是左侧：XFF 是追加语义，客户端可以自己塞一个假值在最左边，
// 而右侧几跳是各层代理逐个追加的，越靠右越接近真实发起者。
func walkXFF(xff string, trusted []*net.IPNet) (string, bool) {
	parts := strings.Split(xff, ",")
	sawCF := false
	for i := len(parts) - 1; i >= 0; i-- {
		ip := strings.TrimSpace(parts[i])
		if ip == "" {
			continue
		}
		if cfEdge(ip) {
			sawCF = true
			continue
		}
		if !isTrusted(ip, trusted) {
			return ip, sawCF
		}
	}
	return "", sawCF
}

// rightmostUntrusted 已并入 walkXFF / resolveClientIP（本条链路上只有一个出口，
// 免得"两个地方各判一次可信代理、其中一处忘了 CF 那一跳"）。

// cloudflareEdgeRanges 是 Cloudflare 边缘地址段的**快照**
// （2026-10 取自 https://www.cloudflare.com/ips-v4 与 /ips-v6）。
//
// 为什么值得内建：这几段地址只可能是 Cloudflare 自己——TCP 三次握手要求对端
// 真能从该网段发包，客户端伪造不了源地址。因此"来源在 CF 网段"本身就是
// "这一跳是 CF 边缘"的充分证据，不需要管理员再在 trusted_proxies 里声明一次。
//
// 用途只有两个，都在"谁是真实客户端"这条链上：
//  1. 解析 XFF 时把 CF 那一跳跳过去（否则套了 CF 的部署会把 CF 的回源地址
//     当成客户端地址——真机里节点地址就是这么变成 172.68.x.x 的）；
//  2. 判定请求是不是 HTTPS（CF 会写 X-Forwarded-Proto，见 session_cookie.go）。
//
// 名单多年未变，但确实变过；更新时改这里即可。它**不是**安全边界：
// 真正的边界是"只有 CF 会从这些地址回源"。
var cloudflareEdgeRanges = []string{
	"173.245.48.0/20",
	"103.21.244.0/22",
	"103.22.200.0/22",
	"103.31.4.0/22",
	"141.101.64.0/18",
	"108.162.192.0/18",
	"190.93.240.0/20",
	"188.114.96.0/20",
	"197.234.240.0/22",
	"198.41.128.0/17",
	"162.158.0.0/15",
	"104.16.0.0/13",
	"104.24.0.0/14",
	"172.64.0.0/13",
	"131.0.72.0/22",
	"2400:cb00::/32",
	"2606:4700::/32",
	"2803:f800::/32",
	"2405:b500::/32",
	"2405:8100::/32",
	"2a06:98c0::/29",
	"2c0f:f248::/32",
}

var cloudflareEdges = parseCIDRs(cloudflareEdgeRanges)

// cfEdge 判断地址是否落在 Cloudflare 边缘网段内。
func cfEdge(ip string) bool { return isTrusted(ip, cloudflareEdges) }

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

// statusRecorder 捕获下游响应的状态码与字节数，供访问日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
	size   int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.size += n
	return n, err
}

// AccessLogMiddleware 记录每次请求的访问日志（方法/路径/状态码/来源 IP/耗时）。
//
// 来源 IP 取自 RealIP(r)——因此必须挂在 RealIPMiddleware 之内（更靠近路由），
// 才能拿到经可信代理还原后的真实来源，而不是 TCP 对端的 127.0.0.1。
// 默认关闭（master.yaml 的 access_log），由 Handler.AccessLog 控制开关。
// 只记录路径、不记录完整 URL，避免把查询串里的令牌写进日志。
func AccessLogMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("access",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"ip", RealIP(r),
			"dur_ms", time.Since(start).Milliseconds(),
			"bytes", rec.size,
		)
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
