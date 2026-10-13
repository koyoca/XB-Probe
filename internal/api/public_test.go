package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yourorg/probe/internal/model"
)

// 公开延迟接口的两条行为契约，都是踩过坑之后补的。

// 1) 抽稀必须保留末点。曲线的"当前值"就是最后一个点，丢掉它会让
// 图例里的数字与曲线尾部对不上——看板使用者会以为图错了。
func TestDecimateKeepsLastPointAndCapsLength(t *testing.T) {
	pts := make([]model.PublicHistoryPoint, 1000)
	for i := range pts {
		pts[i] = model.PublicHistoryPoint{TS: int64(1700000000 + i*10), Value: int64(i)}
	}

	out := decimate(pts, 480)
	if len(out) != 480 {
		t.Fatalf("抽稀后应有 480 个点，实得 %d", len(out))
	}
	if out[0] != pts[0] {
		t.Errorf("首点必须保留：得到 %+v，期望 %+v", out[0], pts[0])
	}
	if out[len(out)-1] != pts[len(pts)-1] {
		t.Errorf("末点必须保留：得到 %+v，期望 %+v", out[len(out)-1], pts[len(pts)-1])
	}
	// 时间必须单调不减，否则曲线会回折。
	for i := 1; i < len(out); i++ {
		if out[i].TS < out[i-1].TS {
			t.Fatalf("第 %d 个点时间回退：%d -> %d", i, out[i-1].TS, out[i].TS)
		}
	}
}

// 点数不超过上限时必须原样返回（不能无故丢点，否则线会变粗糙）。
func TestDecimatePassthroughWhenUnderLimit(t *testing.T) {
	pts := []model.PublicHistoryPoint{{TS: 1, Value: 1}, {TS: 2, Value: 2}, {TS: 3, Value: 3}}
	out := decimate(pts, 480)
	if len(out) != 3 {
		t.Fatalf("点数未超上限时不应抽稀，实得 %d 个", len(out))
	}
	for i := range pts {
		if out[i] != pts[i] {
			t.Errorf("第 %d 个点被改动：%+v", i, out[i])
		}
	}
}

// 2) 后台自填的目标名称送进公开响应前必须清洗：控制字符会破坏 JSON 的可读性，
// 也便于构造诡异的排版。长度也要截断，避免超长名称把看板排版撑坏。
func TestSanitizeLabelStripsControlCharsAndTruncates(t *testing.T) {
	got := sanitizeLabel("  电信\t上海\n主用  ")
	if got != "电信上海主用" {
		t.Errorf("清洗结果 = %q，期望 %q", got, "电信上海主用")
	}
	long := make([]rune, 200)
	for i := range long {
		long[i] = 'A'
	}
	if n := len([]rune(sanitizeLabel(string(long)))); n != 48 {
		t.Errorf("超长标签应截到 48 个字符，实得 %d", n)
	}
	if sanitizeLabel("") != "" {
		t.Errorf("空标签应返回空串（由调用方回退到「目标 #id」）")
	}
}

// 3) 公开站总开关（public.enabled）关闭时的放行规则。
//
// 这是"关掉看板不能把装机通道一起关死"这条产品约定的落点：如果安装路径
// 也被一起拦掉，新节点将永远无法接入，而这是不可挽回的操作性事故。
func TestIsInstallPathStaysOpenWhenPublicClosed(t *testing.T) {
	// 必须放行。
	for _, p := range []string{"/install.sh", "/install.ps1", "/dist/", "/dist/1.0.0/SHA256SUMS"} {
		if !isInstallPath(p) {
			t.Errorf("%q 在公开站关闭时仍须放行（否则新节点装不上）", p)
		}
	}
	// 必须拦掉。
	for _, p := range []string{"/", "/index.html", "/style.css", "/api/public/v1/nodes",
		"/install.sh.bak", "/dist", "/assets/dist/x"} {
		if isInstallPath(p) {
			t.Errorf("%q 不应被当作安装路径放行", p)
		}
	}
}

// ---------------------------------------------------------------------------
// 隐藏节点（public_visible=0）与归档节点不得从公开接口被读到
// ---------------------------------------------------------------------------

// TestPublicDetailEndpointsHideInvisibleNodes 钉住三个详情类接口的可见性闸门。
//
// 为什么还要单独立一条：列表侧的过滤发生在**快照**里
// （`internal/state.TestSnapshotOnlyContainsVisibleNodes` 已覆盖），
// 但详情 / 历史 / 延迟这三个 handler 是各自 `GetByPublicID` 之后**各判一次**
// `!rt.PublicVisible || rt.Archived`——三处判断是分开写的，漏掉任何一处都会变成
// "列表里看不到，但知道 public_id 就能拉到完整数据"。这种泄漏形态最容易被忽略：
// 页面看起来是对的，只有直接打接口才看得见。
//
// 必须有正向对照（可见节点 200）：只断言 404 的话，把 handler 改成"一律 404"
// 也能通过，那不是护栏而是摆设。
func TestPublicDetailEndpointsHideInvisibleNodes(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	// 可见 / 隐藏（public_visible=0）/ 归档（可见但已归档）三种节点。
	h.Registry.SyncMeta(1, "pub-vis", "可见", "", 0, true, false, true, false, "")
	h.Registry.SyncMeta(2, "pub-hid", "隐藏", "", 0, false, false, true, false, "")
	h.Registry.SyncMeta(3, "pub-arc", "归档", "", 0, true, true, true, false, "")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/public/v1/nodes/{public_id}", h.handlePublicNode)
	mux.HandleFunc("GET /api/public/v1/nodes/{public_id}/history", h.handlePublicHistory)
	mux.HandleFunc("GET /api/public/v1/nodes/{public_id}/latency", h.handlePublicLatency)

	cases := []struct {
		name string
		path string
		want int
	}{
		// 正向对照：可见节点三个接口都必须是 200。
		{"可见-详情", "/api/public/v1/nodes/pub-vis", http.StatusOK},
		{"可见-历史", "/api/public/v1/nodes/pub-vis/history?range=24h", http.StatusOK},
		{"可见-延迟", "/api/public/v1/nodes/pub-vis/latency?range=24h", http.StatusOK},
		// 隐藏节点：三个接口都必须 404（不是 200，也不是 403——403 会暴露"存在但不可见"）。
		{"隐藏-详情", "/api/public/v1/nodes/pub-hid", http.StatusNotFound},
		{"隐藏-历史", "/api/public/v1/nodes/pub-hid/history?range=24h", http.StatusNotFound},
		{"隐藏-延迟", "/api/public/v1/nodes/pub-hid/latency?range=24h", http.StatusNotFound},
		// 归档节点：与隐藏同样处理。
		{"归档-详情", "/api/public/v1/nodes/pub-arc", http.StatusNotFound},
		{"归档-历史", "/api/public/v1/nodes/pub-arc/history?range=24h", http.StatusNotFound},
		{"归档-延迟", "/api/public/v1/nodes/pub-arc/latency?range=24h", http.StatusNotFound},
		// 不存在的标识：同样 404（与隐藏节点无法区分，避免枚举探测）。
		{"不存在-详情", "/api/public/v1/nodes/pub-none", http.StatusNotFound},
	}

	for _, c := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s：%s 返回 %d，期望 %d（body=%s）", c.name, c.path, rec.Code, c.want, rec.Body.String())
		}
	}
}

// 隐藏节点的详情必须真的拿不到数据：404 之外，响应体里也不能出现节点名。
// 只断言状态码的话，"返回 200 但正文是空的"这类改动漏不掉、但"返回了完整节点"
// 会被漏掉（取决于 writeErr 之后有没有 return）。
func TestPublicDetailBodyLeaksNothingForHiddenNode(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	h.Registry.SyncMeta(9, "pub-secret", "不该被看到的节点", "", 0, false, false, true, false, "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/public/v1/nodes/pub-secret", nil)
	req.SetPathValue("public_id", "pub-secret")
	h.handlePublicNode(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("隐藏节点详情应 404，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"不该被看到的节点", "pub-secret"} {
		if strings.Contains(body, leak) {
			t.Errorf("隐藏节点的响应体里出现了 %q：%s", leak, body)
		}
	}
}
