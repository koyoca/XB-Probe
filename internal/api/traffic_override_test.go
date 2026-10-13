package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/koyoca/xb-probe/internal/store"
	"github.com/koyoca/xb-probe/internal/traffic"
)

// 后台「手工矫正本周期已用流量」的接口护栏（问题 10）。
//
// 与 internal/traffic/tracker_usage_test.go 的分工：那边钉 tracker 的内存/落盘
// 一致性，这里钉**接口这一层**——单位口径、越界拒绝、以及"保存完之后页面上
// 立刻能看到新数字"。最后一条是用户最直接的体感：只落库不同步运行态的话，
// 管理员保存后要点好几次刷新才看到变化（甚至以为没保存成功）。

// trafficEnv 是"带 tracker 与运行态"的节点环境。
type trafficEnv struct {
	h    *Handler
	repo *store.Repo
	id   int64
}

func newTrafficEnv(t *testing.T) *trafficEnv {
	t.Helper()
	h, repo := newTOTPTestHandler(t)
	h.Tracker = traffic.NewTracker(repo, func(int64) (int, string, bool) { return 1, "UTC", true })
	id := mkNode(t, repo, "tr-node", "流量节点")
	h.Registry.GetOrCreate(id, "tr-node", "流量节点")
	return &trafficEnv{h: h, repo: repo, id: id}
}

// put 发一次矫正请求。PathValue 用**真实节点 ID**而不是写死的 1：
// 写死会让这条护栏依赖"空库里的第一个节点恰好是 1"，换一张建表顺序就静默跑偏。
func (e *trafficEnv) put(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	sid := strconv.FormatInt(e.id, 10)
	w, _ := doJSON(t, e.h.handleSetTrafficUsage, http.MethodPut,
		"/api/admin/v1/nodes/"+sid+"/traffic", body, "id", sid)
	return w
}

func TestSetTrafficUsageOverridesPeriodUsageEverywhere(t *testing.T) {
	env := newTrafficEnv(t)

	// 计费口径：上下行相加 × 倍率 2，额度 1 TiB。
	if err := env.repo.UpsertBilling(store.BillingRow{
		NodeID: env.id, Cycle: "monthly", ResetDay: 1, TZ: "UTC",
		TrafficQuota: 1 << 40, TrafficMode: "both", TrafficMultiplier: 2,
		ExpireAt: time.Now().Add(24 * time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("写计费失败：%v", err)
	}

	const wantRx = int64(1) << 30
	const wantTx = int64(1) << 30
	if w := env.put(t, map[string]any{"period_rx": wantRx, "period_tx": wantTx}); w.Code != http.StatusNoContent {
		t.Fatalf("应返回 204，得到 %d：%s", w.Code, w.Body.String())
	}

	// ① 库里（详情页的"流量"卡片直接读它）。
	st, err := env.repo.GetTrafficState(env.id)
	if err != nil || st == nil {
		t.Fatalf("读周期用量失败：%v", err)
	}
	if st.PeriodRx != wantRx || st.PeriodTx != wantTx {
		t.Fatalf("库里未写入矫正值：rx=%d tx=%d", st.PeriodRx, st.PeriodTx)
	}
	if st.PeriodStart == 0 {
		t.Fatal("周期起点不得为 0：它会让下一次上报把矫正值当成上一周期的余量")
	}

	// ② 运行态（看板与列表读它）。这一步才是"保存后立刻看到新数字"，
	//    也是 refreshRuntimeBilling 在 60 秒后重算时所依赖的取值来源。
	n, ok := env.h.Registry.Get(env.id)
	if !ok {
		t.Fatal("运行态里没有该节点")
	}
	if n.PeriodRx != wantRx || n.PeriodTx != wantTx {
		t.Fatalf("运行态原始计量值未同步：rx=%d tx=%d", n.PeriodRx, n.PeriodTx)
	}
	// 计费流量 = (1 GiB + 1 GiB) × 2
	wantUsed := (wantRx + wantTx) * 2
	if n.TrafficUsed != wantUsed {
		t.Fatalf("计费流量应为 %d，得到 %d", wantUsed, n.TrafficUsed)
	}
	wantPct := int(float64(wantUsed) / float64(int64(1)<<40) * 10000)
	if n.TrafficUsedPct != wantPct {
		t.Fatalf("百分比应为 %d，得到 %d", wantPct, n.TrafficUsedPct)
	}

	// ③ 清零：两栏都填 0 是"重置本周期用量"的唯一入口，必须真的归零。
	if w := env.put(t, map[string]any{"period_rx": 0, "period_tx": 0}); w.Code != http.StatusNoContent {
		t.Fatalf("清零应返回 204，得到 %d", w.Code)
	}
	if st, _ := env.repo.GetTrafficState(env.id); st == nil || st.PeriodRx != 0 || st.PeriodTx != 0 {
		t.Fatalf("清零未生效：%+v", st)
	}
	if n, _ := env.h.Registry.Get(env.id); n.TrafficUsed != 0 || n.TrafficUsedPct != 0 {
		t.Fatalf("清零后运行态未归零：used=%d pct=%d", n.TrafficUsed, n.TrafficUsedPct)
	}
}

// TestSetTrafficUsageRejectsBadInput 钉入参边界。
//
// 上限的存在理由不是"业务上不允许更大"，而是：单方向取值一旦超过约 9.2e14
// 字节，`used*10000` 的整数运算就会溢出成负数，而负数会绕过 state.Percent 里
// "超过 10000 就收敛"的判断，把负百分比画到页面上（见 state 包的同名用例）。
func TestSetTrafficUsageRejectsBadInput(t *testing.T) {
	env := newTrafficEnv(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"下行负数", map[string]any{"period_rx": -1, "period_tx": 0}},
		{"上行负数", map[string]any{"period_rx": 0, "period_tx": -1024}},
		{"下行超过 1 PiB 上限", map[string]any{"period_rx": maxManualTrafficBytes + 1, "period_tx": 0}},
		{"上行超过 1 PiB 上限", map[string]any{"period_rx": 0, "period_tx": maxManualTrafficBytes + 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if w := env.put(t, c.body); w.Code != http.StatusBadRequest {
				t.Fatalf("应返回 400，得到 %d：%s", w.Code, w.Body.String())
			}
			// 拒绝之后库里必须还是空的；写进半截比拒绝更糟。
			if st, _ := env.repo.GetTrafficState(env.id); st != nil {
				t.Fatalf("非法入参写入了数据：%+v", st)
			}
		})
	}

	// 不存在的节点：traffic_state 对 nodes(id) 有外键，不先判存在性的话
	// 会撞成"保存失败"的 500，管理员完全不知道是自己填错了 id。
	w, _ := doJSON(t, env.h.handleSetTrafficUsage, http.MethodPut,
		"/api/admin/v1/nodes/999999/traffic", map[string]any{"period_rx": 1, "period_tx": 1}, "id", "999999")
	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在的节点应返回 404，得到 %d：%s", w.Code, w.Body.String())
	}
}
