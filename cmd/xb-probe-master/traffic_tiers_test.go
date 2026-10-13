package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/koyoca/xb-probe/internal/alert"
	"github.com/koyoca/xb-probe/internal/db"
	"github.com/koyoca/xb-probe/internal/model"
	"github.com/koyoca/xb-probe/internal/state"
	"github.com/koyoca/xb-probe/internal/store"
	"github.com/koyoca/xb-probe/internal/traffic"
)

// 流量配额梯级告警（4.5 / 13.4）的行为测试。
//
// 这是三类告警里**唯一此前没有任何测试的一路**：离线看 LastSeen、资源看阈值、
// 探测失败看轮次，都有测试；只有流量梯级既没有单测也没有冒烟断言。
// 它的坑与别处不同——**每一档是一个独立状态键**（traffic:80 / :95 / :100），
// "每档只告警一次"完全依赖 Fire 内部的冷却，而不是自家计数。

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

type trafficTierEnv struct {
	s       *Scheduler
	repo    *store.Repo
	reg     *state.Registry
	tracker *traffic.Tracker
	nodeID  int64
	// rxC / txC 是"Agent 上报的网卡累计值"，addUsage 只负责往上加。
	rxC int64
	txC int64
}

// newTrafficTierEnv 搭一套只跑 checkTrafficTiers 的最小环境。
//
// 用真实的 store.Repo 与 traffic.Tracker（Repo 实现了 traffic.Store）：
// 梯级的难点在"已用量怎么算出来"，用假 tracker 直接塞一个数字进去，
// 就把 ApplyMode / ApplyMultiplier / 周期归零这一整段都跳过了——那正是要测的部分。
func newTrafficTierEnv(t *testing.T, b store.BillingRow) *trafficTierEnv {
	t.Helper()

	dbs, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = dbs.Close() })
	if err := dbs.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	repo := store.New(dbs)

	nodeID, err := repo.CreateNode("pub-tt", "流量节点", "默认", time.Now().Unix())
	if err != nil {
		t.Fatalf("建节点失败: %v", err)
	}

	reg := state.NewRegistry(8)
	reg.SyncMeta(nodeID, "pub-tt", "流量节点", "默认", 0, true, false, true, false, "")

	// 重置日 1 号、UTC：周期边界固定在每月 1 日，便于构造"跨周期"。
	tr := traffic.NewTracker(repo, func(int64) (int, string, bool) { return 1, "UTC", true })

	b.NodeID = nodeID
	s := &Scheduler{
		repo:        repo,
		registry:    reg,
		tracker:     tr,
		alerts:      alert.New(nil, repo, nil),
		log:         discardLogger(),
		started:     time.Now().Add(-time.Hour),
		billings:    map[int64]store.BillingRow{nodeID: b},
		targets:     map[int64]store.ProbeTargetMeta{},
		probeRounds: map[probeKey]int64{},
	}
	return &trafficTierEnv{s: s, repo: repo, reg: reg, tracker: tr, nodeID: nodeID}
}

// addUsage 模拟一次上报：网卡累计值往上走 delta。
//
// 第一次调用时基线无效，增量恒为 0（见 ComputeDelta：!prev.Valid → 空增量），
// 这正是真机上"装上 Agent 的第一轮不产生流量"的行为，不是测试偷懒。
func (e *trafficTierEnv) addUsage(now time.Time, addRx, addTx int64) {
	e.rxC += addRx
	e.txC += addTx
	e.tracker.Add(e.nodeID, map[string]model.NICCounters{"eth0": {Rx: e.rxC, Tx: e.txC}}, 0, 64, now)
}

func (e *trafficTierEnv) scan(now time.Time) {
	e.scanWith(alert.DefaultRules(), now)
}

func (e *trafficTierEnv) scanWith(rules map[string]alert.Rule, now time.Time) {
	rt, ok := e.reg.Get(e.nodeID)
	if !ok {
		panic("运行态里没有该节点（夹具出错）")
	}
	e.s.checkTrafficTiers(context.Background(), rules, rt, now.Unix())
}

// fires 统计各档**告警**事件（warn）的条数；恢复通知是 info，不计入。
func (e *trafficTierEnv) fires(t *testing.T) map[string]int {
	t.Helper()
	return e.count(t, alert.LevelWarn)
}

func (e *trafficTierEnv) recoveries(t *testing.T) map[string]int {
	t.Helper()
	return e.count(t, alert.LevelInfo)
}

func (e *trafficTierEnv) count(t *testing.T, level string) map[string]int {
	t.Helper()
	all, _, err := e.repo.ListAlertEvents(200, 0)
	if err != nil {
		t.Fatalf("读告警事件失败: %v", err)
	}
	out := map[string]int{}
	for _, ev := range all {
		if ev.Level == level {
			out[ev.Kind]++
		}
	}
	return out
}

func (e *trafficTierEnv) stateOf(t *testing.T, lv int) string {
	t.Helper()
	st, err := e.repo.GetAlertState(e.nodeID, fmt.Sprintf("traffic:%d", lv))
	if err != nil {
		t.Fatalf("读告警状态失败: %v", err)
	}
	if st == nil {
		return "(无)"
	}
	return st.State
}

// ---------------------------------------------------------------------------
// 每档一次
// ---------------------------------------------------------------------------

// TestTrafficTiersFireOncePerLevel 钉住"打到 80/95/100 各告警一次"。
//
// 两条最容易改坏的地方：
//  1. **不许越级**：80% 时只能出 80 档。若把循环写成"触发最高那一档"，
//     80% 就直接报 100%，管理员收到的第一条就是"流量用完了"。
//  2. **冷却内不重复**：这一档没有自己的连续计数，"每档一次"完全靠 Fire 里的
//     冷却（默认 21600 秒）。若哪天有人把它换成"每次扫描都 Fire"，
//     一个超配额的节点会以 10 秒一条的速度刷屏——这条断言专门拦这个。
func TestTrafficTiersFireOncePerLevel(t *testing.T) {
	e := newTrafficTierEnv(t, store.BillingRow{
		TrafficQuota: 1000, TrafficMode: model.TrafficBoth, TrafficMultiplier: 1,
	})
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	// 先建基线：第一次上报增量恒为 0（ComputeDelta 在基线无效时不算增量），
	// 与真机上"Agent 装上后的第一轮不产生流量"完全一致。
	e.addUsage(now, 0, 0)

	// 已用 800/1000 = 80%。
	e.addUsage(now, 400, 400)
	e.scan(now)
	if got := e.fires(t); got["traffic:80"] != 1 {
		t.Fatalf("80%% 应触发 traffic:80 一次，实得 %+v", got)
	}
	if got := e.fires(t); got["traffic:95"] != 0 || got["traffic:100"] != 0 {
		t.Errorf("80%% 不应触发更高档（越级会把第一条告警直接报成 100%%）：%+v", got)
	}

	// 同一档连扫两次：冷却窗口内不得重复。
	e.scan(now.Add(10 * time.Second))
	e.scan(now.Add(20 * time.Second))
	if got := e.fires(t); got["traffic:80"] != 1 {
		t.Errorf("冷却窗口内重复告警（80 档共 %d 条）", got["traffic:80"])
	}

	// 到 95%：95 档出现，80 档**不新增**。
	e.addUsage(now.Add(30*time.Second), 75, 75)
	e.scan(now.Add(30 * time.Second))
	got := e.fires(t)
	if got["traffic:80"] != 1 || got["traffic:95"] != 1 || got["traffic:100"] != 0 {
		t.Errorf("95%% 时的档位统计不对：%+v", got)
	}

	// 到 100%：三档各一次。
	e.addUsage(now.Add(40*time.Second), 25, 25)
	e.scan(now.Add(40 * time.Second))
	got = e.fires(t)
	if got["traffic:80"] != 1 || got["traffic:95"] != 1 || got["traffic:100"] != 1 {
		t.Errorf("100%% 时应为三档各一次：%+v", got)
	}
}

// TestTrafficTiersRecoverOnNewPeriod 钉住"新周期自动重置"。
//
// 跨周期后已用量归零，三档都应发一次恢复通知，且状态不再停留在 firing——
// 若恢复分支丢了，节点会在下个周期继续显示"流量已用完"，
// 而告警事件列表里再也看不到任何异常（因为冷却把它压住了）。
func TestTrafficTiersRecoverOnNewPeriod(t *testing.T) {
	e := newTrafficTierEnv(t, store.BillingRow{
		TrafficQuota: 1000, TrafficMode: model.TrafficBoth, TrafficMultiplier: 1,
	})
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	e.addUsage(now, 0, 0) // 建基线（首轮增量 0）

	// 用超 100%：1200/1000。
	e.addUsage(now, 600, 600)
	e.scan(now)
	if got := e.fires(t); got["traffic:80"] != 1 || got["traffic:95"] != 1 || got["traffic:100"] != 1 {
		t.Fatalf("前提不成立：超配额时三档都应告警，实得 %+v", got)
	}

	// 跨到下一个周期（4 月）：累计值不变，但周期边界变了 → 已用量归零。
	next := time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
	e.addUsage(next, 0, 0)
	e.scan(next)

	rec := e.recoveries(t)
	for _, lv := range []int{80, 95, 100} {
		key := fmt.Sprintf("traffic:%d", lv)
		if rec[key] != 1 {
			t.Errorf("%s 应发一次恢复通知，实得 %d 条（%+v）", key, rec[key], rec)
		}
		if st := e.stateOf(t, lv); st == "firing" {
			t.Errorf("%s 在新周期仍停留在 firing（节点会一直显示「流量已用完」）", key)
		}
	}

	// 再扫一次：不得重复恢复（Recover 只在 firing 时才发）。
	e.scan(next.Add(time.Minute))
	rec2 := e.recoveries(t)
	for _, lv := range []int{80, 95, 100} {
		key := fmt.Sprintf("traffic:%d", lv)
		if rec2[key] != 1 {
			t.Errorf("%s 重复恢复：%d 条", key, rec2[key])
		}
	}
}

// ---------------------------------------------------------------------------
// 静默条件
// ---------------------------------------------------------------------------

// 没有配额、规则被停用、或者这个节点根本没被计量过 —— 三种情况都必须一声不响。
// 反过来说，任意一种"静默条件失效"都会变成噪音：没有配额的节点流量一大就告警。
func TestTrafficTiersStaysSilentWithoutQuotaRuleOrTracking(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

	// A) 配额为 0（未设置流量上限）：不算百分比。
	e := newTrafficTierEnv(t, store.BillingRow{
		TrafficQuota: 0, TrafficMode: model.TrafficBoth, TrafficMultiplier: 1,
	})
	e.addUsage(now, 0, 0) // 建基线（首轮增量 0）
	e.addUsage(now, 5000, 5000)
	e.scan(now)
	if got := e.fires(t); len(got) != 0 {
		t.Errorf("未设配额不应告警：%+v", got)
	}

	// B) 规则被停用。
	e2 := newTrafficTierEnv(t, store.BillingRow{
		TrafficQuota: 1000, TrafficMode: model.TrafficBoth, TrafficMultiplier: 1,
	})
	e2.addUsage(now, 0, 0) // 建基线（首轮增量 0）
	e2.addUsage(now, 600, 600)
	off := alert.DefaultRules()
	r := off[alert.KindTraffic]
	r.Enabled = false
	off[alert.KindTraffic] = r
	e2.scanWith(off, now)
	if got := e2.fires(t); len(got) != 0 {
		t.Errorf("规则停用后不应告警：%+v", got)
	}
	// 正向对照：同一份用量、规则启用时必须告警，否则上面那条可能是"环境压根没跑起来"。
	e2.scan(now)
	if got := e2.fires(t); got["traffic:80"] != 1 {
		t.Errorf("正向对照失败：规则启用后应告警，实得 %+v", got)
	}

	// C) 节点没被计量过（tracker 里没有它）：Used 返回 ok=false。
	e3 := newTrafficTierEnv(t, store.BillingRow{
		TrafficQuota: 1000, TrafficMode: model.TrafficBoth, TrafficMultiplier: 1,
	})
	e3.scan(now)
	if got := e3.fires(t); len(got) != 0 {
		t.Errorf("未计量的节点不应告警：%+v", got)
	}
}

// ---------------------------------------------------------------------------
// 计量口径与倍率
// ---------------------------------------------------------------------------

// TestTrafficTiersHonorModeAndMultiplier 钉住"按哪一口径算、倍率有没有生效"。
//
// 这两者错了不会报错，只是**告警的时机不对**：口径设成 up 却把 rx 也算进去，
// 出向为主的节点会提前很久触发；倍率没生效则永远打不到配额。
func TestTrafficTiersHonorModeAndMultiplier(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

	// A) 口径 up（只算出向）：rx 900 / tx 100 → 计费流量 100，10%，不该告警。
	e := newTrafficTierEnv(t, store.BillingRow{
		TrafficQuota: 1000, TrafficMode: model.TrafficUp, TrafficMultiplier: 1,
	})
	e.addUsage(now, 0, 0) // 建基线（首轮增量 0）
	e.addUsage(now, 900, 100)
	e.scan(now)
	if got := e.fires(t); len(got) != 0 {
		t.Errorf("口径 up 不应把入向算进去（900/100 应只有 10%%）：%+v", got)
	}

	// B) 倍率 2：实际 400 → 计费 800 → 80 档。
	e2 := newTrafficTierEnv(t, store.BillingRow{
		TrafficQuota: 1000, TrafficMode: model.TrafficBoth, TrafficMultiplier: 2,
	})
	e2.addUsage(now, 0, 0) // 建基线（首轮增量 0）
	e2.addUsage(now, 200, 200)
	e2.scan(now)
	if got := e2.fires(t); got["traffic:80"] != 1 {
		t.Errorf("倍率 2 未生效（400×2=800 应触发 80 档）：%+v", got)
	}

	// C) 倍率 0 视为 1（"没配"不能把流量算成 0）。
	e3 := newTrafficTierEnv(t, store.BillingRow{
		TrafficQuota: 1000, TrafficMode: model.TrafficBoth, TrafficMultiplier: 0,
	})
	e3.addUsage(now, 0, 0) // 建基线（首轮增量 0）
	e3.addUsage(now, 400, 400)
	e3.scan(now)
	if got := e3.fires(t); got["traffic:80"] != 1 {
		t.Errorf("倍率 0 应视作 1（否则流量恒为 0、配额永远打不到）：%+v", got)
	}
}
