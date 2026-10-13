package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yourorg/probe/internal/model"
	"github.com/yourorg/probe/internal/theme"
)

// 公开看板顶栏「管理后台」按钮的跳转地址（site.admin_url 及其推导规则）。
//
// 为什么值得单独测：公开站与后台**不在同一个源**是常态（不同端口、不同域名，
// 或一个端口 + 两个域名由反代分流）。写死的 `/admin/` 在这种部署下会跳到
// 公开站自己的路径里，得到 404 或 421 —— 用户看到的现象是"点登录按钮打不开
// 后台"，而站内一切看起来都正常，只有拿真域名点一次才发现。
//
// 另一个方向同样要防：**不该猜的时候不能猜**。多管理域名时挑一个拼出去，
// 效果是把用户送到一个陌生站点——比不给地址更糟。

// newAdminEntryHandler 造一个可用的 Handler，并把站点形态写进**设置项**。
//
// 走设置项而不是 Deps 字段：那才是管理员在后台实际会改的路径，测它等于同时
// 覆盖了"设置项 → 生效值"这一段。
func newAdminEntryHandler(t *testing.T, layout, adminHosts, adminURL string) *Handler {
	t.Helper()
	h, _ := newTOTPTestHandler(t)
	m, err := theme.NewManager("")
	if err != nil {
		t.Fatalf("初始化主题管理器失败：%v", err)
	}
	h.Themes = m
	kv := map[string]string{}
	if layout != "" {
		kv["site.layout"] = layout
	}
	if adminHosts != "" {
		kv["site.admin_hosts"] = adminHosts
	}
	if adminURL != "" {
		kv["site.admin_url"] = adminURL
	}
	if len(kv) > 0 {
		setSiteSettings(t, h, kv)
	}
	return h
}

// adminEntryReq 造一个带可选 X-Forwarded-Proto 与来源地址的请求。
func adminEntryReq(proto, remote string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/public/v1/site", nil)
	if remote != "" {
		req.RemoteAddr = remote
	}
	if proto != "" {
		req.Header.Set("X-Forwarded-Proto", proto)
	}
	return req
}

// 同站部署（single，或默认的 auto 且没配管理域名）：保持相对路径。
//
// 这一条看着像"什么都没做"，但它防的是过度设计——给同站部署也拼一个绝对
// 地址，等于凭空引入一个可能写错的域名，而相对路径本来就是对的。
//
// 后半段（配了管理域名却仍是 single）才是**能区分实现**的那一半：只测"没配域名"
// 时返回空串是测不出东西的——那种情况下推导链本来也推不出地址，把 `single` 的
// 早退整段删掉它照样绿。真正需要断言的是：显式选了 single 时，早退必须先于一切推导。
func TestAdminEntryURLLeavesSameSiteRelative(t *testing.T) {
	for _, layout := range []string{siteLayoutSingle, ""} {
		h := newAdminEntryHandler(t, layout, "", "")
		for _, req := range []*http.Request{
			adminEntryReq("", ""),
			adminEntryReq("https", "10.0.0.1:1"),
		} {
			if got := h.adminEntryURL(req); got != "" {
				t.Errorf("layout=%q 同站时应保持相对路径 /admin/，实得 %q", layout, got)
			}
		}
	}

	// 显式 single：此时"同站"与配没配管理域名无关。single 形态下域名门禁并不生效，
	// 用户完全可能从另一个域名打开看板，所以这里拼出来的绝对地址不一定可达。
	h := newAdminEntryHandler(t, siteLayoutSingle, "admin.example.com", "")
	if got := h.adminEntryURL(adminEntryReq("", "")); got != "" {
		t.Errorf("显式 single 形态下，即使配了 site.admin_hosts 也应保持相对路径，实得 %q", got)
	}
}

// 显式设置项优先——包括同站形态。有人就是要把按钮指向别处。
func TestAdminEntryURLExplicitSettingWins(t *testing.T) {
	h := newAdminEntryHandler(t, siteLayoutSingle, "", "http://ops.example.com:9443")
	if got := h.adminEntryURL(adminEntryReq("", "")); got != "http://ops.example.com:9443/admin/" {
		t.Fatalf("显式的 site.admin_url 应优先（同站也不例外），实得 %q", got)
	}
}

// 回落 master.yaml 的 admin_origin（部署脚本会问出来写进去）。
func TestAdminEntryURLFallsBackToConfigAdminOrigin(t *testing.T) {
	h := newAdminEntryHandler(t, "", "", "")
	h.AdminOrigin = "https://admin.example.com"
	if got := h.adminEntryURL(adminEntryReq("", "")); got != "https://admin.example.com/admin/" {
		t.Fatalf("应回落 master.yaml 的 admin_origin，实得 %q", got)
	}
}

// 分离形态 + 只有一个管理域名 → 用**本次请求的协议**拼出来。
// 这正是"域名 + 反代"的部署自动得到 https://admin.example.com/admin/ 的那条路。
func TestAdminEntryURLDerivedFromSingleAdminHost(t *testing.T) {
	h := newAdminEntryHandler(t, siteLayoutSplit, "admin.example.com", "")
	h.TLSActive = true
	if got := h.adminEntryURL(adminEntryReq("", "")); got != "https://admin.example.com/admin/" {
		t.Fatalf("分离 + 唯一管理域名 + 本机 TLS 应推出 https 地址，实得 %q", got)
	}

	// 明文 HTTP 直连 IP 的部署：mode=none、无可信代理 → http。
	h2 := newAdminEntryHandler(t, siteLayoutSplit, "admin.example.com", "")
	if got := h2.adminEntryURL(adminEntryReq("", "")); got != "http://admin.example.com/admin/" {
		t.Fatalf("明文部署应推出 http 地址，实得 %q", got)
	}
}

// 多个管理域名：猜哪个都是错的，保持相对路径。
func TestAdminEntryURLAmbiguousAdminHostsStayRelative(t *testing.T) {
	h := newAdminEntryHandler(t, siteLayoutSplit, "a.example.com, b.example.com", "")
	if got := h.adminEntryURL(adminEntryReq("", "")); got != "" {
		t.Fatalf("多个管理域名无法判断该去哪个，应保持相对路径，实得 %q", got)
	}
}

// X-Forwarded-Proto 只对可信反代生效。
//
// 不设这道限就等于让任意客户端用一个请求头决定按钮跳去 https 还是 http——
// 反向代理场景确实需要它，但采信范围必须限定在 trusted_proxies 内。
func TestAdminEntryURLTrustsForwardedProtoOnlyFromTrustedProxy(t *testing.T) {
	h := newAdminEntryHandler(t, siteLayoutSplit, "admin.example.com", "")
	h.TrustedProxies = []string{"10.0.0.0/8"}

	if got := h.adminEntryURL(adminEntryReq("https", "10.1.2.3:5555")); got != "https://admin.example.com/admin/" {
		t.Fatalf("可信反代声明 https 时应按 https 生成，实得 %q", got)
	}
	if got := h.adminEntryURL(adminEntryReq("https", "203.0.113.9:5555")); got != "http://admin.example.com/admin/" {
		t.Fatalf("不可信来源伪造的头不应被采信，实得 %q", got)
	}
}

// withAdminPath 幂等：给不带路径、带尾斜杠、带 /admin、带 /admin/ 的形式
// 都应得到同一个结果，不能出现 /admin/admin/。
func TestWithAdminPathIsIdempotent(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://a.example.com", "https://a.example.com/admin/"},
		{"https://a.example.com/", "https://a.example.com/admin/"},
		{"https://a.example.com/admin", "https://a.example.com/admin/"},
		{"https://a.example.com/admin/", "https://a.example.com/admin/"},
		{"  https://a.example.com  ", "https://a.example.com/admin/"},
		{"", ""},
	}
	for _, c := range cases {
		if got := withAdminPath(c.in); got != c.want {
			t.Errorf("withAdminPath(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 接线守卫：解析出来的地址必须真的出现在 /api/public/v1/site 的响应里。
//
// 只测 adminEntryURL 是"函数对"，这条才是"页面拿得到"。少了它，把
// handleSite 里那一行删掉，上面所有用例照样全绿。
func TestHandleSitePublishesAdminURL(t *testing.T) {
	h := newAdminEntryHandler(t, "", "", "")
	h.AdminOrigin = "https://admin.example.com"

	rec := httptest.NewRecorder()
	h.handleSite(rec, adminEntryReq("", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("handleSite 应返回 200，实得 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var site model.PublicSite
	if err := json.Unmarshal(rec.Body.Bytes(), &site); err != nil {
		t.Fatalf("解析 PublicSite 失败：%v", err)
	}
	if site.AdminURL != "https://admin.example.com/admin/" {
		t.Fatalf("handleSite 应把解析结果下发到 admin_url，实得 %q", site.AdminURL)
	}
}

// 同站部署不该下发 admin_url（omitempty）——公开响应里多一个用不上的字段，
// 既是多余的指纹，也让前端多一条本可避免的分支。
func TestHandleSiteOmitsAdminURLOnSameSite(t *testing.T) {
	h := newAdminEntryHandler(t, siteLayoutSingle, "", "")
	rec := httptest.NewRecorder()
	h.handleSite(rec, adminEntryReq("", ""))
	if strings.Contains(rec.Body.String(), "admin_url") {
		t.Fatalf("同站部署不该下发 admin_url，实际 body=%s", rec.Body.String())
	}
}
