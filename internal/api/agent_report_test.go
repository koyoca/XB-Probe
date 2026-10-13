package api

import (
	"testing"
	"time"

	"github.com/koyoca/xb-probe/internal/model"
	"github.com/koyoca/xb-probe/internal/pipeline"
	"github.com/koyoca/xb-probe/internal/state"
)

// 写侧准入回归：节点上报的延迟里若混入"不属于本节点、也不是全局目标"的
// target_id，必须被 applySample 直接丢弃，不得进入聚合桶。
//
// 这是 item「节点小卡片下面的延迟探测，有的节点没有专属探测目标，但是同样渲染了
// 别的节点的专属探测目标」的写侧防线；读侧防线见
// store.TestLatencyBucketsForNodeRejectsForeignTarget。
func TestApplySampleDropsForeignTargetLatency(t *testing.T) {
	h, repo := newTOTPTestHandler(t)
	now := time.Now()
	nowUnix := now.Unix()

	a, err := repo.CreateNode("pub-a", "nodeA", "", nowUnix)
	if err != nil {
		t.Fatalf("建节点 A 失败：%v", err)
	}
	b, err := repo.CreateNode("pub-b", "nodeB", "", nowUnix)
	if err != nil {
		t.Fatalf("建节点 B 失败：%v", err)
	}

	foreign := &model.ProbeTarget{NodeID: b, Name: "B独享", Carrier: "telecom",
		Host: "1.1.1.1", Port: 443, Method: "tcp", TimeoutMs: 2000, Enabled: true, ShowInBars: true}
	foreignID, err := repo.CreateProbeTarget(foreign)
	if err != nil {
		t.Fatalf("建 B 专属目标失败：%v", err)
	}
	own := &model.ProbeTarget{NodeID: a, Name: "A独享", Carrier: "unicom",
		Host: "8.8.8.8", Port: 53, Method: "tcp", TimeoutMs: 2000, Enabled: true, ShowInBars: true}
	ownID, err := repo.CreateProbeTarget(own)
	if err != nil {
		t.Fatalf("建 A 专属目标失败：%v", err)
	}

	// 换取聚合器，只收集 10 分钟层落盘的目标 id。
	var written []int64
	h.Latency = pipeline.NewLatencyMultiResolution(30, func(table string, nodeID int64, row pipeline.LatencyRow) {
		if table == "latency_10m" {
			written = append(written, row.TargetID)
		}
	})
	// applySample 还会走指标聚合与流量跟踪；测试只关心延迟，给个空聚合器即可。
	h.Multi = pipeline.NewMultiResolution(30, func(table string, nodeID int64, row pipeline.MetricsRow) {})

	h.Registry.GetOrCreate(a, "", "")
	eff, _ := h.effectiveConfig(a)
	allowed := allowedTargetIDs(eff)

	rtt := int64(1500)
	s := &model.Sample{TS: nowUnix, Latency: []model.LatencySample{
		{TargetID: int(foreignID), RTTAvg: &rtt, Loss: 0}, // 越权：B 的专属目标
		{TargetID: int(ownID), RTTAvg: &rtt, Loss: 0},     // 合法：A 自己的目标
	}}
	h.applySample(a, s, now, nil, allowed)
	h.Latency.FlushAll()

	sawOwn := false
	for _, id := range written {
		if id == foreignID {
			t.Errorf("越权写入：节点 A 的延迟桶出现节点 B 专属目标 %d", foreignID)
		}
		if id == ownID {
			sawOwn = true
		}
	}
	if !sawOwn {
		t.Errorf("合法目标 %d 未写入（准入过滤把本节点自己的目标也丢了）", ownID)
	}
}

// allowedTargetIDs 必须只包含"本节点可见且已启用"的目标。
func TestAllowedTargetIDsScope(t *testing.T) {
	h, repo := newTOTPTestHandler(t)
	nowUnix := time.Now().Unix()
	a, _ := repo.CreateNode("pub-a", "nodeA", "", nowUnix)
	b, _ := repo.CreateNode("pub-b", "nodeB", "", nowUnix)

	globalID, _ := repo.CreateProbeTarget(&model.ProbeTarget{Name: "全局", Carrier: "mobile",
		Host: "114.114.114.114", Port: 53, Method: "tcp", TimeoutMs: 2000, Enabled: true, ShowInBars: true})
	ownID, _ := repo.CreateProbeTarget(&model.ProbeTarget{NodeID: a, Name: "A独享", Carrier: "unicom",
		Host: "8.8.8.8", Port: 53, Method: "tcp", TimeoutMs: 2000, Enabled: true, ShowInBars: true})
	foreignID, _ := repo.CreateProbeTarget(&model.ProbeTarget{NodeID: b, Name: "B独享", Carrier: "telecom",
		Host: "1.1.1.1", Port: 443, Method: "tcp", TimeoutMs: 2000, Enabled: true, ShowInBars: true})
	disabledID, _ := repo.CreateProbeTarget(&model.ProbeTarget{NodeID: a, Name: "A停用", Carrier: "custom",
		Host: "9.9.9.9", Port: 53, Method: "tcp", TimeoutMs: 2000, Enabled: false, ShowInBars: true})

	eff, _ := h.effectiveConfig(a)
	allowed := allowedTargetIDs(eff)

	if !allowed[globalID] {
		t.Errorf("全局目标 %d 应在 A 的允许集合里", globalID)
	}
	if !allowed[ownID] {
		t.Errorf("A 的专属目标 %d 应在允许集合里", ownID)
	}
	if allowed[foreignID] {
		t.Errorf("B 的专属目标 %d 不该出现在 A 的允许集合里", foreignID)
	}
	if allowed[disabledID] {
		t.Errorf("已停用的目标 %d 不该出现在允许集合里", disabledID)
	}
}

// TestApplySampleLatencyRoundAdvancesOnlyOnANewRound 钉住"轮次号"的唯一写入点。
//
// 为什么单独立一条：调度器判定「探测连续失败」时靠 `LatencyRound` 区分
// "新的一轮探测"与"同一轮被反复扫到"。这条链路最容易出的错在后面那半——
// 若轮次号在"上报但没带延迟"时也自增，按轮去重会失效、`ConfirmN=3` 会在一轮内满足；
// 若轮次号在带延迟时**不**自增，则永远只有第 1 轮能计数，告警再也发不出来。
// 两个方向都是静默的，所以两侧都要断言。
//
// 前提（由 Agent 侧保证）：`Latency` 只在一轮探测**首次**上报时携带，
// 之后被 `takeLatency` 清空——见 `internal/agent` 的 `TestProbeLoopReReadsTargetsEachRound`。
// 否则每次 3 秒的上报都会推进轮次号，去重同样失效。
func TestApplySampleLatencyRoundAdvancesOnlyOnANewRound(t *testing.T) {
	h, repo := newTOTPTestHandler(t)
	now := time.Now()
	nowUnix := now.Unix()

	a, err := repo.CreateNode("pub-a", "nodeA", "", nowUnix)
	if err != nil {
		t.Fatalf("建节点失败：%v", err)
	}
	// 测试只关心延迟轮次，聚合与流量给空实现即可。
	h.Multi = pipeline.NewMultiResolution(30, func(string, int64, pipeline.MetricsRow) {})
	h.Latency = pipeline.NewLatencyMultiResolution(30, func(string, int64, pipeline.LatencyRow) {})
	h.Registry.GetOrCreate(a, "", "")

	// 这两个读取器都走 Update，与生产代码用同一把锁，避免直接摸指针。
	round := func() int64 {
		var v int64
		h.Registry.Update(a, func(n *state.NodeRuntime) { v = n.LatencyRound })
		return v
	}
	latency := func() []model.LatencySample {
		var v []model.LatencySample
		h.Registry.Update(a, func(n *state.NodeRuntime) { v = n.LatestLatency })
		return v
	}
	rtt := int64(1200)

	// 1) 纯指标上报（Agent 未带延迟）——不得推进轮次，否则按轮去重失效。
	h.applySample(a, &model.Sample{TS: nowUnix, Metrics: model.Metrics{}}, now, nil, nil)
	if got := round(); got != 0 {
		t.Fatalf("未携带延迟的上报推进了轮次号（= %d）：按轮去重会失效", got)
	}
	if len(latency()) != 0 {
		t.Fatalf("未携带延迟的上报却写入了 LatestLatency：%v", latency())
	}

	// 2) 携带延迟 → 恰好 +1，且样本内容确实被写入。
	h.applySample(a, &model.Sample{TS: nowUnix + 3, Latency: []model.LatencySample{
		{TargetID: 11, RTTAvg: &rtt},
	}}, now, nil, nil)
	if got := round(); got != 1 {
		t.Fatalf("携带延迟的上报未推进轮次号：round = %d，期望 1", got)
	}
	if got := latency(); len(got) != 1 || got[0].TargetID != 11 {
		t.Fatalf("LatestLatency 未被替换：%v", got)
	}

	// 3) 再来一轮 → +1（这是"连续 N 轮"能被数到的前提）。
	h.applySample(a, &model.Sample{TS: nowUnix + 6, Latency: []model.LatencySample{
		{TargetID: 11, Error: model.ProbeErrTimeout},
	}}, now, nil, nil)
	if got := round(); got != 2 {
		t.Fatalf("第二轮未推进轮次号：round = %d，期望 2", got)
	}

	// 4) 空切片（len == 0）既不算新的一轮，也不得清掉上一轮的内容——
	//    "目标本轮没上报"与"目标恢复了"必须可区分。
	prev := latency()
	h.applySample(a, &model.Sample{TS: nowUnix + 9, Latency: []model.LatencySample{}}, now, nil, nil)
	if got := round(); got != 2 {
		t.Fatalf("空延迟切片推进了轮次号：round = %d，期望仍是 2", got)
	}
	after := latency()
	if len(after) != len(prev) || after[0].TargetID != prev[0].TargetID || after[0].Error != prev[0].Error {
		t.Fatalf("空延迟切片把上一轮结果清掉了：%v → %v", prev, after)
	}
}
