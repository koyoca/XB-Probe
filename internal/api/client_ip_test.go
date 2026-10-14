package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 「来源 IP 是谁」的回归护栏（用户上报：Master 套上 Cloudflare 之后，
// 节点地址变成 172.68.22.149 —— CLOUDFLARENET 的回源地址）。
//
// 修法有两条，缺一条都还不算修好：
//  1. XFF 里 **Cloudflare 边缘那一跳必须被跳过**（真机链路是 CF → 本机 Caddy → Master，
//     最右边那一跳就是 CF 自己，只信"可信代理"的实现在这里停住，把 CF 当成了客户端）；
//  2. CF-Connecting-IP 是单值头、比 XFF 准，但它只有在**链路确实过了 CF** 时才采信
//     （见下面那条"旁路 CF 直连反代"的用例）。
func TestResolveClientIPBehindCloudflare(t *testing.T) {
	trusted := parseCIDRs([]string{"127.0.0.1/32", "::1/128"})
	const cfEdge = "172.68.22.149" // 落在 172.64.0.0/13 里
	const client = "203.0.113.9"

	cases := []struct {
		name string
		peer string
		hdr  map[string]string
		want string
	}{
		{
			name: "CF 直接回源（带 CF-Connecting-IP）",
			peer: cfEdge + ":443",
			hdr:  map[string]string{"CF-Connecting-IP": client},
			want: client,
		},
		{
			name: "CF 直接回源（只有 XFF）",
			peer: cfEdge + ":443",
			hdr:  map[string]string{"X-Forwarded-For": client},
			want: client,
		},
		{
			name: "CF → 本机 Caddy → Master（真机场景：CF 那一跳必须跳过）",
			peer: "127.0.0.1:54321",
			hdr:  map[string]string{"X-Forwarded-For": client + ", " + cfEdge},
			want: client,
		},
		{
			name: "CF 链 + CF-Connecting-IP：以 CF 的话为准",
			peer: "127.0.0.1:54321",
			hdr: map[string]string{
				"X-Forwarded-For":  "10.9.9.9, " + cfEdge,
				"CF-Connecting-IP": client,
			},
			want: client,
		},
		{
			name: "客户端自己塞的假值在左边：取右侧那个",
			peer: cfEdge + ":443",
			hdr:  map[string]string{"X-Forwarded-For": "1.2.3.4, " + client},
			want: client,
		},
		{
			name: "对端不可信：伪造什么头都不认",
			peer: "198.51.100.7:443",
			hdr: map[string]string{
				"X-Forwarded-For":  client,
				"CF-Connecting-IP": client,
			},
			want: "198.51.100.7",
		},
		{
			name: "可信反代但链里没有 CF：不采信 CF-Connecting-IP",
			// 旁路 CF 直连反代的人也能塞一个同名头，所以这条头必须要有
			// "链路里出现过 CF 边缘"作为背书。
			peer: "127.0.0.1:54321",
			hdr: map[string]string{
				"X-Forwarded-For":  client,
				"CF-Connecting-IP": "198.51.100.99",
			},
			want: client,
		},
		{
			name: "畸形头（带逗号）当没给，退回对端地址",
			peer: cfEdge + ":443",
			hdr:  map[string]string{"CF-Connecting-IP": client + ", 1.2.3.4"},
			want: cfEdge,
		},
	}

	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/public/v1/site", nil)
		req.RemoteAddr = c.peer
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		if got := resolveClientIP(req, trusted); got != c.want {
			t.Errorf("%s：resolveClientIP = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// Cloudflare 边缘也应当被当作"可信的一跳"来判定协议：否则套了 CF 的部署
// 明明对外是 HTTPS，会话 Cookie 却被降级成不带 Secure 的明文形态（S2 的缓解失效）。
func TestRequestSecureTreatsCloudflareEdgeAsProxy(t *testing.T) {
	h := &Handler{Deps: Deps{TLSMode: "none"}}

	ok := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	ok.RemoteAddr = "172.68.22.149:443"
	ok.Header.Set("X-Forwarded-Proto", "https")
	if !h.requestSecure(ok) {
		t.Error("对端是 Cloudflare 边缘且 X-Forwarded-Proto: https 时未判为 HTTPS：" +
			"套 CF 的部署拿不到 Secure Cookie")
	}

	// 直接连上来的客户端伪造同一个头：仍然不许采信（它会骗到一个 Secure Cookie，
	// 把自己锁在门外，而且掩盖了真实的传输协议）。
	forged := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	forged.RemoteAddr = "198.51.100.7:443"
	forged.Header.Set("X-Forwarded-Proto", "https")
	if h.requestSecure(forged) {
		t.Error("不可信来源的 X-Forwarded-Proto 被采信了：任意客户端都能伪造 HTTPS")
	}
}
