package api

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/yourorg/probe/internal/alert"
)

// 分页响应的信封形状回归。
//
// 背景（用户上报的需求）："告警页面中的告警事件、审计日志中的日志，添加分页查询"。
//
// 为什么必须锁住这几条：
//  1. 响应从**裸数组**改成了 `{items,total,limit,offset}`。只改了 handler 而漏改
//     前端时，前端会去遍历一个对象——`(res || []).forEach` 直接抛异常，
//     整张表空白且控制台只有一行报错。所以这里断言 items 的存在与类型。
//  2. 回传的 limit/offset 必须是**实际生效**的窗口：前端用它们算页码
//     （`Math.floor(offset/limit)+1`）。若回传的是入参而查询用的是收敛后的值，
//     页码和表格内容会互相矛盾。
//  3. total 必须存在：没有它前端画不出"第 x / y 页"，也就没法禁用"下一页"。

func auditPage(t *testing.T, h *Handler, query string) (int, map[string]any) {
	t.Helper()
	rec, out := doJSON(t, h.handleAuditLog, http.MethodGet, "/api/admin/v1/audit-log"+query, nil)
	return rec.Code, out
}

func TestAuditLogPaginationEnvelope(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	// 造数据：审计行由 handler 层写（Repo.CreateNode 本身不写审计），
	// 所以这里直接调 Repo.Audit 铺 12 条。
	for i := 0; i < 12; i++ {
		if err := h.Repo.Audit(int64(1_760_000_000+i), "ops", "node.update",
			strconv.Itoa(i+1), "127.0.0.1"); err != nil {
			t.Fatalf("造数据失败：%v", err)
		}
	}

	code, out := auditPage(t, h, "?limit=5&offset=0")
	if code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", code)
	}
	items, ok := out["items"].([]any)
	if !ok {
		t.Fatalf("响应里没有 items 数组（说明还是裸数组或换了 key）：%v", out)
	}
	if len(items) != 5 {
		t.Errorf("第一页应有 5 行，得到 %d", len(items))
	}
	if got, _ := out["limit"].(float64); int(got) != 5 {
		t.Errorf("limit 应为 5，得到 %v", out["limit"])
	}
	if got, _ := out["offset"].(float64); int(got) != 0 {
		t.Errorf("offset 应为 0，得到 %v", out["offset"])
	}
	total, _ := out["total"].(float64)
	if int(total) < 12 {
		t.Errorf("total 应至少 12（造了 12 条），得到 %v", out["total"])
	}

	// 第二页与第一页不能重叠。
	_, out2 := auditPage(t, h, "?limit=5&offset=5")
	items2, _ := out2["items"].([]any)
	if len(items2) == 0 {
		t.Fatal("第二页是空的")
	}
	idOf := func(v any) float64 {
		m, _ := v.(map[string]any)
		f, _ := m["id"].(float64)
		return f
	}
	if idOf(items[0]) == idOf(items2[0]) {
		t.Error("第一页与第二页的第一行相同（offset 没生效）")
	}

	// 超过上限的 limit 要压回上限，并且**回传的是压过之后的值**。
	_, out3 := auditPage(t, h, "?limit=100000")
	if got, _ := out3["limit"].(float64); int(got) != 200 {
		t.Errorf("limit 应被压回 200 并回传，得到 %v", out3["limit"])
	}

	// 越界 offset：拉回最后一页，回传生效值（否则前端会显示"第 N / 2 页"配空表）。
	_, out4 := auditPage(t, h, "?limit=5&offset=100000")
	off, _ := out4["offset"].(float64)
	if int(off) == 100000 {
		t.Errorf("越界 offset 没被收敛，回传了 %v", out4["offset"])
	}
	items4, _ := out4["items"].([]any)
	if len(items4) == 0 {
		t.Error("越界 offset 被收敛后仍然返回空表（应给最后一页的内容）")
	}
	// 生效 offset 必须是 limit 的整数倍，前端才能算出整数页码。
	if int(off)%5 != 0 {
		t.Errorf("生效 offset=%d 不是 limit 的整数倍，前端算出的页码会带小数", int(off))
	}
}

func TestAlertEventsPaginationEnvelope(t *testing.T) {
	h, repo := newTOTPTestHandler(t)
	// alert_events.node_id 有外键，先建个节点再铺 7 条事件。
	nodeID, err := repo.CreateNode("pub-events", "events", "", 1_760_000_000)
	if err != nil {
		t.Fatalf("建节点失败：%v", err)
	}
	for i := 0; i < 7; i++ {
		if _, err := repo.InsertAlertEvent(alert.Event{
			NodeID: nodeID, Kind: "offline", Level: "warn",
			Message: "节点离线", CreatedAt: int64(1_760_000_000 + i),
		}); err != nil {
			t.Fatalf("铺事件失败：%v", err)
		}
	}

	rec, out := doJSON(t, h.handleListEvents, http.MethodGet, "/api/admin/v1/alert-events?limit=7", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", rec.Code)
	}
	items, ok := out["items"].([]any)
	if !ok {
		t.Fatalf("响应里没有 items 数组：%v", out)
	}
	if len(items) != 7 {
		t.Errorf("第一页应有 7 行，得到 %d", len(items))
	}
	if got, _ := out["limit"].(float64); int(got) != 7 {
		t.Errorf("limit 应为 7，得到 %v", out["limit"])
	}
	if got, _ := out["total"].(float64); int(got) != 7 {
		t.Errorf("total 应为 7，得到 %v", out["total"])
	}

	// 越界 offset 与审计日志走的是同一套收敛逻辑；这里单独断言一次，
	// 否则那条规则只在审计日志上被验证（两个 handler 是对称的，但不该只测一半）。
	_, out2 := doJSON(t, h.handleListEvents, http.MethodGet, "/api/admin/v1/alert-events?limit=3&offset=99999", nil)
	if got, _ := out2["offset"].(float64); int(got) == 99999 {
		t.Errorf("告警事件的越界 offset 没被收敛，回传了 %v", out2["offset"])
	}
	// 7 条 / 每页 3 条 → 最后一页从第 6 条开始，也就是 offset=6。
	if got, _ := out2["offset"].(float64); int(got) != 6 {
		t.Errorf("越界 offset 应拉回最后一页起点 6，得到 %v", out2["offset"])
	}
	if got := len(out2["items"].([]any)); got != 1 {
		t.Errorf("最后一页应有 1 行，得到 %d", got)
	}
}
