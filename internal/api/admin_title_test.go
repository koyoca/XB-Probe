package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 登录页标题必须跟随后台的站点标题设置（用户反馈：登录界面里的 XB-Probe 标题
// 应当与站点标题一致）。
//
// 为什么只能在服务端填：登录页是最早出现的画面，此时前端还没登录、拿不到
// /settings，所以 index.html 里留了 __SITE_TITLE__ 占位符，由下发这一刻替换。
// 这条链断掉的表现是"设置页改了标题、公开看板变了、登录页还是 XB-Probe"——
// 看起来像设置没生效。
func TestAdminIndexTitleComesFromSiteSetting(t *testing.T) {
	h, repo := newTOTPTestHandler(t)
	now := int64(1_760_000_000)

	get := func(path string) string {
		rec := httptest.NewRecorder()
		h.handleAdminUI(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s 返回 %d", path, rec.Code)
		}
		return rec.Body.String()
	}

	if err := repo.SetSetting("site.title", "我的监控", now); err != nil {
		t.Fatalf("写设置失败：%v", err)
	}
	body := get("/admin/")
	if strings.Contains(body, "__SITE_TITLE__") {
		t.Error("index.html 里的站点标题占位符没有被替换（登录页会显示 __SITE_TITLE__）")
	}
	if !strings.Contains(body, `<h1 id="login-title">我的监控</h1>`) {
		t.Error("登录页标题没有跟随后台设置的站点标题")
	}
	if !strings.Contains(body, `<title id="page-title">我的监控 管理后台</title>`) {
		t.Error("浏览器标题栏没有跟随站点标题")
	}
	// 登录成功后 admin.js 会接管这三处（改设置不必重新登录），文案形态要对得上。
	if !strings.Contains(body, `<span class="brand" id="brand-title">我的监控</span>`) {
		t.Error("顶栏品牌名没有跟随站点标题")
	}

	// 站点标题是管理员可控文本，必须转义后才能进 HTML——否则一个设置项就能
	// 往登录页里注入标记（登录页是所有未登录访客都会看到的页面）。
	if err := repo.SetSetting("site.title", `<img src=x onerror=alert(1)>`, now); err != nil {
		t.Fatalf("写设置失败：%v", err)
	}
	if escaped := get("/admin/"); strings.Contains(escaped, "<img src=x") {
		t.Error("站点标题里的 HTML 没有转义：可以往登录页注入标记")
	}

	// 静态资源不该被"过一遍替换"（它们里面没有占位符），内容保持原样。
	if js := get("/admin/admin.js"); !strings.Contains(js, "function boot(") {
		t.Error("admin.js 没有被正常下发（替换逻辑误伤了静态资源）")
	}
}
