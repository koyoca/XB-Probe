package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/koyoca/xb-probe/internal/acme"
	"github.com/koyoca/xb-probe/internal/theme"
)

// ACME http-01 的挑战路由：装了它，路由装配必须仍然能跑完。
//
// 这条护栏来自一次真机故障。用户选 "2) master（Master 自己申请证书，http-01）"
// 之后服务起不来、80 端口从未被监听，而它给出的线索是"证书申请失败"——于是
// 排查方向被带到 DNS / 防火墙 / CA 限流上。
//
// 真正的原因在**装配路由**这一步：
//
//	mux.Handle(acme.HTTP01Prefix, h.ChallengeHandler)   // 不带方法 = 匹配所有方法
//	mux.HandleFunc("GET /", h.handleThemeAsset)         // 兜底
//
// Go 1.22 起的 ServeMux 会在注册时判断两个模式是否冲突。这两个互不包含：
// 挑战路径的**路径更具体但方法更多**（任意方法），兜底的**路径更宽但方法更窄**
// （GET/HEAD）。于是 `Handle` 直接 panic（net/http 的 "conflicts with"），
// 进程死在 main 里 —— 任何"选 acme + http-01"的部署都必然如此，与网络无关。
//
// 修法是给挑战模式带上方法（"GET "）。这条测试把三件事一起钉住：
//
//	① 装配不 panic（回归这条 panic 本身就是它存在的理由）；
//	② 挑战路径仍然能走到挑战处理器（修法不能把挑战挡在门外）；
//	③ 这个模式**确实带了方法**——用一个非 GET 方法请求，它必须被 ServeMux
//	   按 405 拒掉，而不是被挑战处理器接住。少了 ③，把 "GET " 去掉后
//	   这条测试依然是绿的（除非又恰好触发 panic，而 panic 只在同时存在
//	   兜底模式时才发生），护栏就变成了摆设。
func TestChallengeRouteCoexistsWithCatchAll(t *testing.T) {
	reached := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("token.thumbprint"))
	})

	h, _ := newTOTPTestHandler(t)
	h.ChallengeHandler = handler
	// 兜底那一趟要真的把主题入口渲染出来，所以这里装一个真管理器
	// （只用内置主题，不碰磁盘）。
	themes, err := theme.NewManager("")
	if err != nil {
		t.Fatalf("构造主题管理器失败：%v", err)
	}
	h.Themes = themes

	// ① 装配。mode=acme + challenge=http-01 时 ChallengeHandler 非空，
	//    这条路径以前走到 RootRoutes 就 panic。
	root := h.RootRoutes()
	admin := h.AdminRoutes()

	for _, c := range []struct {
		name string
		h    http.Handler
	}{
		{"公开入口", root},
		{"管理端入口", admin},
	} {
		// ② GET 必须命中挑战处理器。
		reached = false
		rec := httptest.NewRecorder()
		c.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, acme.HTTP01Prefix+"tok", nil))
		if !reached {
			t.Errorf("%s：GET %s 没有走到挑战处理器——证书申请会因此失败",
				c.name, acme.HTTP01Prefix+"tok")
		}
		if rec.Code != http.StatusOK {
			t.Errorf("%s：GET 挑战路径 = %d，期望 200", c.name, rec.Code)
		}

		// HEAD 也要能应答：Go 的 ServeMux 里 GET 模式同时匹配 HEAD，
		// 而 CA 用哪种方法取挑战在 RFC 8555 里没有强制（Boulder 用 GET）。
		// 这条同时防止将来有人"为了严谨"改成 `mux.Handle("GET …")` + 单独
		// 一个会被兜底抢走的 HEAD 分支。
		reached = false
		rec = httptest.NewRecorder()
		c.h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, acme.HTTP01Prefix+"tok", nil))
		if !reached || rec.Code != http.StatusOK {
			t.Errorf("%s：HEAD 挑战路径 = %d reached=%v，期望 200 且命中挑战处理器",
				c.name, rec.Code, reached)
		}

		// ③ 挑战模式必须带方法。
		reached = false
		rec = httptest.NewRecorder()
		c.h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, acme.HTTP01Prefix+"tok", nil))
		if reached {
			t.Errorf("%s：POST %s 走到了挑战处理器——说明注册时没有带方法（\"GET \"）。\n"+
				"  不带方法的模式与兜底的 GET / 属于冲突关系，装配时会 panic。",
				c.name, acme.HTTP01Prefix+"tok")
		}
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s：POST 挑战路径 = %d，期望 405（由 ServeMux 按方法拒绝）", c.name, rec.Code)
		}
	}

	// 兜底没有被挑战抢走：不带挑战的普通路径不能落到挑战处理器上，
	// 而要真的把主题入口发出来（`GET /` 是站点的兜底路由）。
	reached = false
	rec := httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if reached {
		t.Error("公开站的兜底路径被挑战处理器接住了——站点页面会变成挑战应答")
	}
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Errorf("GET / = %d（body %d 字节），期望 200 且非空——兜底路由被抢走了",
			rec.Code, rec.Body.Len())
	}
}
