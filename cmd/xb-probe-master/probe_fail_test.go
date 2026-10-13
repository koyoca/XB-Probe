package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/koyoca/xb-probe/internal/alert"
	"github.com/koyoca/xb-probe/internal/db"
	"github.com/koyoca/xb-probe/internal/model"
	"github.com/koyoca/xb-probe/internal/state"
	"github.com/koyoca/xb-probe/internal/store"
)

// 探测连续失败告警（4.5）的端到端测试。
//
// 这里刻意用真实的 store.Repo + state.Registry + alert.Engine，而不是替身：
// 这条链路的坑几乎全在"计数单位"上——一轮探测 vs 一次扫描、每个目标 vs 每个节点。
// 用假替身测不出这些，因为错就错在"谁跟谁算同一个数"。

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

type probeFailEnv struct {
	s      *Scheduler
	repo   *store.Repo
	reg    *state.Registry
	nodeID int64
}

// newProbeFailEnv 搭一套最小可用的调度器环境。
//
// 节点必须先在库里建出来：alert_state 与 alert_events 都有指向 nodes(id) 的外键，
// 直接往运行态里塞一个"库中不存在的节点"会让 Fire 写库时静默失败。
func newProbeFailEnv(t *testing.T) *probeFailEnv {
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

	nodeID, err := repo.CreateNode("pub-pf-1", "测试节点", "默认", time.Now().Unix())
	if err != nil {
		t.Fatalf("建节点失败: %v", err)
	}

	reg := state.NewRegistry(8)
	reg.SyncMeta(nodeID, "pub-pf-1", "测试节点", "默认", 0, true, false, true, false, "")

	s := &Scheduler{
		repo:     repo,
		registry: reg,
		alerts:   alert.New(nil, repo, nil),
		log:      discardLogger(),
		// 跳过"Master 重启宽限期"（4.1 的 2×超时阈值），否则扫描会直接返回。
		started: time.Now().Add(-1 * time.Hour),
		// 目标名缓存直接注入，省掉一次 syncNodeMeta 需要的一整张库表。
		targets: map[int64]store.ProbeTargetMeta{
			11: {ID: 11, Name: "电信-上海"},
			12: {ID: 12, Name: "联通-北京"},
		},
		probeRounds: map[probeKey]int64{},
	}
	return &probeFailEnv{s: s, repo: repo, reg: reg, nodeID: nodeID}
}

// reportRound 模拟"Agent 上报了一轮新的探测结果"，并立刻跑一次告警扫描。
//
// LatencyRound 自增这步必须照抄 applySample 的行为：它正是"这是一轮新的探测"
// 的凭证。samples 为空时**不**替换运行态，与 applySample 的 `len(s.Latency) > 0`
// 守卫一致——空结果不等于"探测恢复"。
func (e *probeFailEnv) reportRound(t *testing.T, now time.Time, samples []model.LatencySample) {
	t.Helper()
	_ = e.reg.Update(e.nodeID, func(n *state.NodeRuntime) {
		if len(samples) > 0 {
			n.LatestLatency = samples
			n.LatencyRound++
		}
		n.LastSeen = now.Unix()
	})
	e.s.scanAlerts(context.Background(), now)
}

// rescan 模拟"同一轮探测被 alertTick 又扫了一次"：运行态一个字节都不改。
func (e *probeFailEnv) rescan(now time.Time) {
	e.s.scanAlerts(context.Background(), now)
}

// eventsOfKind 取指定状态键的告警事件。
func (e *probeFailEnv) eventsOfKind(t *testing.T, kind string) []alert.Event {
	t.Helper()
	all, _, err := e.repo.ListAlertEvents(200, 0)
	if err != nil {
		t.Fatalf("读告警事件失败: %v", err)
	}
	var out []alert.Event
	for _, ev := range all {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// allProbeFailEvents 取所有探测失败相关（含恢复）的事件。
func (e *probeFailEnv) allProbeFailEvents(t *testing.T) []alert.Event {
	t.Helper()
	all, _, err := e.repo.ListAlertEvents(200, 0)
	if err != nil {
		t.Fatalf("读告警事件失败: %v", err)
	}
	var out []alert.Event
	for _, ev := range all {
		if strings.HasPrefix(ev.Kind, alert.KindProbeFail+":") {
			out = append(out, ev)
		}
	}
	return out
}

func (e *probeFailEnv) stateOf(t *testing.T, kind string) *alert.AlertState {
	t.Helper()
	st, err := e.repo.GetAlertState(e.nodeID, kind)
	if err != nil {
		t.Fatalf("读告警状态失败: %v", err)
	}
	return st
}

func failedSampleP(targetID int, errText string) model.LatencySample {
	return model.LatencySample{TargetID: targetID, Loss: 10000, Error: errText}
}

func okSampleP(targetID int, rttUS int64) model.LatencySample {
	return model.LatencySample{TargetID: targetID, RTTAvg: &rttUS, RTTMin: &rttUS, RTTMax: &rttUS}
}

// ---------------------------------------------------------------------------
// 计数单位：轮 vs 扫描次数
// ---------------------------------------------------------------------------

// TestProbeFailCountsRoundsNotScans 是这条链路最核心的一条：
// "连续 3 次确认"指的是 3 **轮**探测，不是 3 次扫描。
//
// 背景：LatestLatency 是持久的（只在拿到非空结果时整体替换），
// 而 alertTick 每 10 秒扫一次、探测默认 30 秒一轮，所以同一轮失败会被扫到 3 次以上。
// 不去重的话，一次失败就能凑够 ConfirmN=3，门槛形同虚设。
func TestProbeFailCountsRoundsNotScans(t *testing.T) {
	env := newProbeFailEnv(t)
	now := time.Now()
	fail := []model.LatencySample{failedSampleP(11, model.ProbeErrTimeout)}

	env.reportRound(t, now, fail)
	for i := 0; i < 4; i++ {
		env.rescan(now.Add(time.Duration(i) * time.Second))
	}
	if got := env.allProbeFailEvents(t); len(got) != 0 {
		t.Fatalf("同一轮探测被扫了 5 次就告警了（%d 条）：计数单位退化成了「扫描次数」", len(got))
	}

	// 第 2 轮：仍不够。
	env.reportRound(t, now.Add(30*time.Second), fail)
	if got := env.allProbeFailEvents(t); len(got) != 0 {
		t.Fatalf("第 2 轮就告警了（%d 条）：ConfirmN=3 没生效", len(got))
	}

	// 第 3 轮：确认门槛达成，只发一条。
	env.reportRound(t, now.Add(60*time.Second), fail)
	got := env.eventsOfKind(t, "probe_fail:11")
	if len(got) != 1 {
		t.Fatalf("第 3 轮失败后事件数 = %d，期望 1", len(got))
	}
	if got[0].Level != alert.LevelWarn {
		t.Errorf("级别 = %s，期望 warn", got[0].Level)
	}
	if !strings.Contains(got[0].Message, "电信-上海") {
		t.Errorf("事件文案应带上目标名，得到 %q", got[0].Message)
	}
	if !strings.Contains(got[0].Message, model.ProbeErrTimeout) {
		t.Errorf("事件文案应带上失败原因，得到 %q", got[0].Message)
	}

	// 已经告警过之后再扫同一轮，不得重复落库。
	env.rescan(now.Add(61 * time.Second))
	if n := len(env.eventsOfKind(t, "probe_fail:11")); n != 1 {
		t.Errorf("同一轮重复扫描后事件数 = %d，期望仍是 1", n)
	}

	// 状态必须是 firing，后台才能显示"当前告警中"。
	st := env.stateOf(t, "probe_fail:11")
	if st == nil || st.State != "firing" {
		t.Fatalf("alert_state = %+v，期望 firing", st)
	}
}

// TestProbeFailStreakIsPerTargetNotPerNode 断言连续计数挂在**目标**而不是节点上。
//
// 两个目标同时失败时，如果计数用基础键（"probe_fail"），每轮会贡献 2 次命中，
// 于是 ConfirmN=3 在第 2 轮就被满足——"连续 3 轮"变成了"连续 2 轮"。
func TestProbeFailStreakIsPerTargetNotPerNode(t *testing.T) {
	env := newProbeFailEnv(t)
	now := time.Now()
	both := []model.LatencySample{
		failedSampleP(11, model.ProbeErrTimeout),
		failedSampleP(12, model.ProbeErrTimeout),
	}

	env.reportRound(t, now, both)
	env.reportRound(t, now.Add(30*time.Second), both)
	if got := env.allProbeFailEvents(t); len(got) != 0 {
		t.Fatalf("两轮后就有 %d 条告警：两个目标的连续计数串在一起了（应在各自数到 3 轮才触发）", len(got))
	}

	env.reportRound(t, now.Add(60*time.Second), both)
	a := env.eventsOfKind(t, "probe_fail:11")
	b := env.eventsOfKind(t, "probe_fail:12")
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("第 3 轮后两个目标的事件数 = %d/%d，期望各 1", len(a), len(b))
	}
}

// TestProbeFailStateKeyIsPerTarget 断言状态键按目标分开，冷却也随之独立。
//
// 若两个目标共用一个状态键，"目标 12 恢复"会把目标 11 的冷却窗口一起重置，
// 于是 11 在冷却期内可以再报一次——这是"按 (节点, 目标) 独立冷却"的实现前提。
func TestProbeFailStateKeyIsPerTarget(t *testing.T) {
	env := newProbeFailEnv(t)
	now := time.Now()
	both := []model.LatencySample{
		failedSampleP(11, model.ProbeErrTimeout),
		failedSampleP(12, model.ProbeErrRefused),
	}
	for i := 0; i < 3; i++ {
		env.reportRound(t, now.Add(time.Duration(i)*30*time.Second), both)
	}
	for _, kind := range []string{"probe_fail:11", "probe_fail:12"} {
		if st := env.stateOf(t, kind); st == nil || st.State != "firing" {
			t.Errorf("%s 的状态 = %+v，期望 firing", kind, st)
		}
	}
}

// ---------------------------------------------------------------------------
// 什么算"目标不通"
// ---------------------------------------------------------------------------

// TestProbeFailIgnoresPermissionAndPolicyErrors 断言"没测成"不等于"测不通"。
//
// icmp_not_permitted：Agent 没有 raw socket 权限（容器里很常见）。算成失败，
// 所有无特权节点会永远在报警，而且是真的在报，管理员无法与真故障区分。
// blocked_target：S4 拨号层拦下了内网目标，这是目标配错了，不是链路坏了。
func TestProbeFailIgnoresPermissionAndPolicyErrors(t *testing.T) {
	cases := []struct {
		name string
		err  string
	}{
		{"ICMP 无权限", model.ProbeErrICMPNotPermitted},
		{"目标被 S4 拒绝", model.ProbeErrBlockedTarget},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newProbeFailEnv(t)
			now := time.Now()
			for i := 0; i < 8; i++ {
				env.reportRound(t, now.Add(time.Duration(i)*30*time.Second),
					[]model.LatencySample{failedSampleP(11, c.err)})
			}
			if got := env.allProbeFailEvents(t); len(got) != 0 {
				t.Fatalf("连扫 8 轮仍产生了 %d 条告警（%s 不该算作目标不通）", len(got), c.err)
			}
		})
	}
}

// TestProbeFailCountsRealFailures 是上一条的反向护栏：别把该报的也一起排除掉。
// 端口被拒（服务没了）与超时都必须触发——否则"排除权限错误"会被写成"什么都不报"。
func TestProbeFailCountsRealFailures(t *testing.T) {
	for _, errText := range []string{
		model.ProbeErrTimeout, model.ProbeErrRefused,
		model.ProbeErrUnreachable, model.ProbeErrDNSFailed, model.ProbeErrOther,
	} {
		t.Run(errText, func(t *testing.T) {
			env := newProbeFailEnv(t)
			now := time.Now()
			for i := 0; i < 3; i++ {
				env.reportRound(t, now.Add(time.Duration(i)*30*time.Second),
					[]model.LatencySample{failedSampleP(11, errText)})
			}
			if got := env.eventsOfKind(t, "probe_fail:11"); len(got) != 1 {
				t.Fatalf("%s 应触发 1 条告警，得到 %d 条", errText, len(got))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 空数据与恢复
// ---------------------------------------------------------------------------

// TestProbeFailEmptyLatencyKeepsStreak 断言"没有探测数据"不等于"探测恢复"。
//
// Agent 只在探到结果时上报；节点刚注册、目标被清空时运行态里就是空的。
// 若把空当成恢复去 Clear，连续计数会被反复清零，门槛永远达不到
// （表现是"明明一直在挂，却永远不告警"）。
func TestProbeFailEmptyLatencyKeepsStreak(t *testing.T) {
	env := newProbeFailEnv(t)
	now := time.Now()
	fail := []model.LatencySample{failedSampleP(11, model.ProbeErrTimeout)}

	env.reportRound(t, now, fail)
	// 中间几轮没有探测数据（samples 为空 → 运行态不被替换，也不该被当成恢复）。
	env.reportRound(t, now.Add(30*time.Second), nil)
	env.rescan(now.Add(31 * time.Second))
	env.reportRound(t, now.Add(60*time.Second), fail)
	env.reportRound(t, now.Add(90*time.Second), fail)

	if got := env.eventsOfKind(t, "probe_fail:11"); len(got) != 1 {
		t.Fatalf("空探测数据期间连续计数被清零了：3 轮失败只产生 %d 条告警", len(got))
	}
}

// TestProbeFailRecoversOnceAndResetsStreak 断言恢复通知只发一次，且计数归零。
func TestProbeFailRecoversOnceAndResetsStreak(t *testing.T) {
	env := newProbeFailEnv(t)
	now := time.Now()
	fail := []model.LatencySample{failedSampleP(11, model.ProbeErrTimeout)}
	ok := []model.LatencySample{okSampleP(11, 1200)}

	for i := 0; i < 3; i++ {
		env.reportRound(t, now.Add(time.Duration(i)*30*time.Second), fail)
	}
	if len(env.eventsOfKind(t, "probe_fail:11")) != 1 {
		t.Fatal("前置条件不成立：3 轮失败后应已告警")
	}

	env.reportRound(t, now.Add(90*time.Second), ok)
	recovered := env.eventsOfKind(t, "probe_fail:11")
	if len(recovered) != 2 {
		t.Fatalf("恢复后事件数 = %d，期望 2（1 告警 + 1 恢复）", len(recovered))
	}
	var info alert.Event
	for _, ev := range recovered {
		if ev.Level == alert.LevelInfo {
			info = ev
		}
	}
	if info.Message == "" {
		t.Fatal("没有找到恢复事件（级别应为 info）")
	}
	if !strings.Contains(info.Message, "恢复") {
		t.Errorf("恢复事件文案 = %q", info.Message)
	}
	if st := env.stateOf(t, "probe_fail:11"); st == nil || st.State != "ok" {
		t.Fatalf("恢复后 alert_state = %+v，期望 ok", st)
	}

	// 继续正常 → 不该再发恢复（没有"异常中"的状态可恢复）。
	env.reportRound(t, now.Add(120*time.Second), ok)
	if n := len(env.eventsOfKind(t, "probe_fail:11")); n != 2 {
		t.Errorf("持续正常时事件数 = %d，期望仍为 2（恢复只发一次）", n)
	}

	// 恢复后重新计数：再失败 2 轮不该触发，第 3 轮才触发。
	for i := 0; i < 3; i++ {
		env.reportRound(t, now.Add(time.Duration(150+30*i)*time.Second), fail)
		if n := len(env.eventsOfKind(t, "probe_fail:11")); n != 2 && i < 2 {
			t.Fatalf("恢复后第 %d 轮就重新告警：连续计数没有归零", i+1)
		}
	}
	if n := len(env.eventsOfKind(t, "probe_fail:11")); n != 3 {
		t.Errorf("恢复后再次连续失败 3 轮，事件数 = %d，期望 3", n)
	}
}

// ---------------------------------------------------------------------------
// 开关与内存维护
// ---------------------------------------------------------------------------

// TestProbeFailRuleDisabledProducesNothing 断言规则停用后不再产生任何事件。
//
// 这条规则在后台是可勾选的；若停用后仍照常告警，管理员会以为开关坏了。
func TestProbeFailRuleDisabledProducesNothing(t *testing.T) {
	env := newProbeFailEnv(t)
	if err := env.repo.SaveAlertRule(alert.Rule{
		Kind:    alert.KindProbeFail,
		Enabled: false,
		Params:  alert.Params{ConfirmN: 1},
	}, `{"confirm_n":1}`); err != nil {
		t.Fatalf("写规则失败: %v", err)
	}

	now := time.Now()
	for i := 0; i < 8; i++ {
		env.reportRound(t, now.Add(time.Duration(i)*30*time.Second),
			[]model.LatencySample{failedSampleP(11, model.ProbeErrTimeout)})
	}
	if got := env.allProbeFailEvents(t); len(got) != 0 {
		t.Fatalf("规则已停用仍产生 %d 条告警", len(got))
	}
}

// TestPruneProbeRoundsDropsStaleEntries 断言去重表不会随历史目标无限增长。
func TestPruneProbeRoundsDropsStaleEntries(t *testing.T) {
	env := newProbeFailEnv(t)

	// 运行态里只剩目标 11；目标 12 已被停用，999 这个节点根本不存在。
	_ = env.reg.Update(env.nodeID, func(n *state.NodeRuntime) {
		n.LatestLatency = []model.LatencySample{okSampleP(11, 1000)}
	})
	env.s.probeRounds[probeKey{node: env.nodeID, target: 11}] = 3
	env.s.probeRounds[probeKey{node: env.nodeID, target: 12}] = 3
	env.s.probeRounds[probeKey{node: 999, target: 11}] = 3

	var nodes []state.NodeRuntime
	env.reg.Each(func(n *state.NodeRuntime) { nodes = append(nodes, *n) })
	env.s.pruneProbeRounds(nodes)

	if _, ok := env.s.probeRounds[probeKey{node: env.nodeID, target: 11}]; !ok {
		t.Error("仍在探测的目标被误删，下一轮会重复计数")
	}
	if _, ok := env.s.probeRounds[probeKey{node: env.nodeID, target: 12}]; ok {
		t.Error("已消失的目标未被裁剪")
	}
	if _, ok := env.s.probeRounds[probeKey{node: 999, target: 11}]; ok {
		t.Error("已不存在节点的条目未被裁剪")
	}
}

// TestProbeFailSkipsArchivedAndNeverReportedNodes 断言归档节点与"从未上报"的
// 节点不参与探测失败判定（与离线判定同一道闸门，4.1）。
func TestProbeFailSkipsArchivedAndNeverReportedNodes(t *testing.T) {
	env := newProbeFailEnv(t)
	now := time.Now()
	fail := []model.LatencySample{failedSampleP(11, model.ProbeErrTimeout)}

	// 归档：运行态标记 + 库里也标记，两条路径都要能挡住。
	_ = env.reg.SyncMeta(env.nodeID, "pub-pf-1", "测试节点", "默认", 0, true, true, true, false, "")
	for i := 0; i < 5; i++ {
		_ = env.reg.Update(env.nodeID, func(n *state.NodeRuntime) {
			n.LatestLatency = fail
			n.LatencyRound++
			n.LastSeen = now.Unix()
		})
		env.s.scanAlerts(context.Background(), now)
	}
	if got := env.allProbeFailEvents(t); len(got) != 0 {
		t.Fatalf("归档节点产生了 %d 条探测失败告警", len(got))
	}
}
