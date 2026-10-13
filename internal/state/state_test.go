package state

import (
	"fmt"
	"testing"
	"time"

	"github.com/yourorg/probe/internal/model"
)

// 回归测试：运行态是 Agent 首次上报时惰性创建的，若元数据不同步，
// 公开看板会永远为空（PublicID 为空、PublicVisible 恒为 false）。
func TestSyncMetaCreatesEntryWithDBMetadata(t *testing.T) {
	g := NewRegistry(10)

	existed := g.SyncMeta(7, "abc123def456", "香港-01", "hk", 0, true, false, false, false, "HK")
	if existed {
		t.Error("首次同步应返回 existed=false")
	}

	n, ok := g.Get(7)
	if !ok {
		t.Fatal("同步后运行态应存在")
	}
	if n.PublicID != "abc123def456" || n.Name != "香港-01" || n.Group != "hk" {
		t.Errorf("元数据未写入：%+v", n)
	}
	if !n.PublicVisible {
		t.Error("public_visible 未写入——公开看板会看不到这个节点")
	}
	if n.Archived {
		t.Error("未归档的节点不应标记 Archived")
	}
	// 关键：同步元数据不等于“已上报”，LastSeen 必须保持 0。
	if n.LastSeen != 0 {
		t.Errorf("同步元数据不应伪装成已上报，LastSeen=%d", n.LastSeen)
	}
	if n.Status != model.StatusOffline {
		t.Errorf("未上报节点状态应为 offline，得到 %q", n.Status)
	}
	// Get 返回的是副本，且 Ring 被刻意置空：实时层必须走 RingPoints 在锁内取快照，
	// 否则会与上报路径的原地 Push 竞争。这里同时验证环形缓冲确实已初始化。
	if n.Ring != nil {
		t.Error("Get 返回的副本不应携带 Ring 指针，实时层请用 Registry.RingPoints")
	}
	if pts, ok := g.RingPoints(7, 0); !ok || pts == nil {
		t.Errorf("环形缓冲未初始化：ok=%v pts=%v", ok, pts)
	}

	// 必须能按 public_id 反查（公开节点详情依赖它）。
	byPublic, ok := g.GetByPublicID("abc123def456")
	if !ok || byPublic.NodeID != 7 {
		t.Errorf("public 索引未建立：%v %+v", ok, byPublic)
	}
}

// public_id 变更时必须重建索引，否则旧键会继续指向该节点、新键查不到。
func TestSyncMetaReindexesPublicID(t *testing.T) {
	g := NewRegistry(10)
	g.SyncMeta(1, "oldid0000000", "n1", "", 0, true, false, false, false, "")

	g.SyncMeta(1, "newid0000000", "n1", "", 0, true, false, false, false, "")

	if _, ok := g.GetByPublicID("oldid0000000"); ok {
		t.Error("旧 public_id 索引应被清除")
	}
	n, ok := g.GetByPublicID("newid0000000")
	if !ok || n.NodeID != 1 {
		t.Errorf("新 public_id 索引未建立：%v", ok)
	}
}

// 同步元数据不得冲掉已经采集到的指标与上报时间。
func TestSyncMetaPreservesRuntimeData(t *testing.T) {
	g := NewRegistry(10)
	rt := g.GetOrCreate(3, "p3", "旧名")
	cpu := 4321
	rt.LatestMetrics.CPU = &cpu
	rt.LastSeen = 1_760_000_000
	rt.BootTime = 1_759_000_000

	g.SyncMeta(3, "p3", "新名", "g", 0, true, false, false, false, "")

	got, _ := g.Get(3)
	if got.LastSeen != 1_760_000_000 {
		t.Errorf("LastSeen 被冲掉：%d", got.LastSeen)
	}
	if got.LatestMetrics.CPU == nil || *got.LatestMetrics.CPU != 4321 {
		t.Errorf("指标被冲掉：%+v", got.LatestMetrics.CPU)
	}
	if got.BootTime != 1_759_000_000 {
		t.Errorf("BootTime 被冲掉：%d", got.BootTime)
	}
	if got.Name != "新名" {
		t.Errorf("名称未更新：%q", got.Name)
	}
	if _, ok := g.RingPoints(3, 0); !ok {
		t.Error("元数据同步后实时层不应丢失")
	}
}

// SyncMeta 幂等：重复同步同一份元数据不产生副作用。
func TestSyncMetaIsIdempotent(t *testing.T) {
	g := NewRegistry(10)
	g.SyncMeta(5, "pid000000000", "n", "g", 0, true, false, false, false, "")
	existed := g.SyncMeta(5, "pid000000000", "n", "g", 0, true, false, false, false, "")
	if !existed {
		t.Error("第二次同步应返回 existed=true")
	}
	if n, _ := g.Get(5); n.PublicID != "pid000000000" {
		t.Errorf("幂等同步后元数据异常：%+v", n)
	}
}

// 归档标记必须能从同步结果带进来。
func TestSyncMetaArchivedFlag(t *testing.T) {
	g := NewRegistry(10)
	g.SyncMeta(9, "pid000000009", "n9", "", 0, true, true, false, false, "")
	n, _ := g.Get(9)
	if !n.Archived {
		t.Error("归档节点应带 Archived=true，否则会继续出现在公开看板")
	}
}

// Prune 必须清掉库里已删除的节点（含 public 索引）。
func TestPruneRemovesMissingNodes(t *testing.T) {
	g := NewRegistry(10)
	g.SyncMeta(1, "aaaa00000001", "a", "", 0, true, false, false, false, "")
	g.SyncMeta(2, "bbbb00000002", "b", "", 0, true, false, false, false, "")
	g.SyncMeta(3, "cccc00000003", "c", "", 0, false, false, false, false, "")

	removed := g.Prune(map[int64]bool{1: true, 3: true})
	if removed != 1 {
		t.Errorf("应删除 1 个节点，实际 %d", removed)
	}
	if _, ok := g.Get(2); ok {
		t.Error("节点 2 应已被删除")
	}
	if _, ok := g.GetByPublicID("bbbb00000002"); ok {
		t.Error("节点 2 的 public 索引应已被清除")
	}
	if _, ok := g.Get(1); !ok {
		t.Error("节点 1 不应被删除")
	}
	if _, ok := g.Get(3); !ok {
		t.Error("节点 3 不应被删除")
	}

	// 全部保留时不应删除任何东西。
	if n := g.Prune(map[int64]bool{1: true, 3: true}); n != 0 {
		t.Errorf("无需删除时应返回 0，实际 %d", n)
	}
}

// Remove 同时清 byNode 与 byPublic。
func TestRemoveClearsBothIndexes(t *testing.T) {
	g := NewRegistry(10)
	g.SyncMeta(4, "dddd00000004", "d", "", 0, true, false, false, false, "")
	g.Remove(4)
	if _, ok := g.Get(4); ok {
		t.Error("byNode 未清除")
	}
	if _, ok := g.GetByPublicID("dddd00000004"); ok {
		t.Error("byPublic 未清除")
	}
	// 删除不存在的节点不应 panic。
	g.Remove(999)
}

// ---------------------------------------------------------------------------
// 公开快照的稳定顺序（"卡片乱跳"的回归测试）
// ---------------------------------------------------------------------------
//
// 线上故障形态：快照每秒重建一次，构建时直接遍历 registry.byNode。Go 的 map
// 迭代顺序每次都是随机的，于是每秒返回的节点顺序都不同；前端每秒重绘，看到
// 的就是卡片互相穿插、位置乱跳。必须按稳定键排序后才能保证顺序固定。

func TestSnapshotOrderIsStableAcrossRefreshes(t *testing.T) {
	g := NewRegistry(10)
	// 刻意乱序插入，模拟真实环境的创建顺序不确定。
	for _, id := range []int64{9, 2, 7, 1, 5, 3} {
		g.SyncMeta(id, fmt.Sprintf("pub%09d", id), fmt.Sprintf("n%d", id), "grp", 0, true, false, false, false, "")
	}

	s := NewSnapshotter(g)
	// 连刷 50 次：任何一次顺序变化都说明排序没生效。
	var first []string
	for i := 0; i < 50; i++ {
		s.Refresh(time.Unix(1_760_000_000+int64(i), 0))
		snap := s.Last()
		got := make([]string, 0, len(snap.Nodes))
		for _, n := range snap.Nodes {
			got = append(got, n.PublicID)
		}
		if i == 0 {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("第 %d 次刷新的节点数变了：%d != %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("第 %d 次刷新的顺序与首次不同（第 %d 位 %q != %q）：\n首次=%v\n本次=%v",
					i, j, got[j], first[j], first, got)
			}
		}
	}

	// 顺序必须是 NodeID 升序（创建顺序），而不是插入顺序或随机顺序。
	want := []string{"pub000000001", "pub000000002", "pub000000003", "pub000000005", "pub000000007", "pub000000009"}
	for i := range want {
		if first[i] != want[i] {
			t.Fatalf("顺序应为 NodeID 升序：\n实际=%v\n期望=%v", first, want)
		}
	}
}

// 管理员显式设置的 sort_order 必须优先于 NodeID，且同样稳定。
func TestSnapshotOrderHonorsSortOrder(t *testing.T) {
	g := NewRegistry(10)
	g.SyncMeta(1, "p1", "n1", "", 30, true, false, false, false, "")
	g.SyncMeta(2, "p2", "n2", "", 10, true, false, false, false, "")
	g.SyncMeta(3, "p3", "n3", "", 20, true, false, false, false, "")

	s := NewSnapshotter(g)
	s.Refresh(time.Unix(1_760_000_000, 0))
	snap := s.Last()

	want := []string{"p2", "p3", "p1"} // 按 sort_order 10/20/30
	for i, n := range snap.Nodes {
		if n.PublicID != want[i] {
			t.Fatalf("sort_order 未生效：实际第 %d 位 %q，期望 %q", i, n.PublicID, want[i])
		}
	}

	// 相同 sort_order 时回退到 NodeID，保证全序（不会因相等而顺序不定）。
	g.SyncMeta(4, "p4", "n4", "", 10, true, false, false, false, "")
	s.Refresh(time.Unix(1_760_000_001, 0))
	snap = s.Last()
	if snap.Nodes[0].PublicID != "p2" || snap.Nodes[1].PublicID != "p4" {
		t.Fatalf("sort_order 相同时应按 NodeID 兜底：%q, %q",
			snap.Nodes[0].PublicID, snap.Nodes[1].PublicID)
	}
}

// 不可见/已归档的节点不得进入公开快照。
func TestSnapshotOnlyContainsVisibleNodes(t *testing.T) {
	g := NewRegistry(10)
	g.SyncMeta(1, "vis1", "可见", "", 0, true, false, false, false, "")
	g.SyncMeta(2, "hid2", "隐藏", "", 0, false, false, false, false, "")
	g.SyncMeta(3, "arc3", "归档", "", 0, true, true, false, false, "")

	s := NewSnapshotter(g)
	s.Refresh(time.Unix(1_760_000_000, 0))
	snap := s.Last()

	if len(snap.Nodes) != 1 || snap.Nodes[0].PublicID != "vis1" {
		t.Fatalf("快照应只含可见且未归档的节点，实际：%+v", snap.Nodes)
	}
	if snap.Overview.NodesTotal != 1 {
		t.Errorf("概览统计应只计可见节点，实际 %d", snap.Overview.NodesTotal)
	}
}

// 回归：Master 重启后，库里的静态信息必须能补回运行态。
//
// 之前的缺口：Agent 只在进程启动后的第一次上报带 static，Master 重启后
// 运行态为空且从不从库恢复，于是 CPU 核数/内存总量/磁盘总量/virt/arch
// 会一直显示为空，直到 Agent 也被重启一次。
func TestRestoreStaticFillsEmptyRuntimeFields(t *testing.T) {
	g := NewRegistry(10)
	g.SyncMeta(1, "p1", "节点一", "亚洲", 0, true, false, false, false, "")

	// 模拟重启后的状态：运行态里静态字段全为空。
	rt, _ := g.Get(1)
	if rt.CPUCores != 0 || rt.MemTotal != 0 || rt.OS != "" {
		t.Fatalf("前提不成立：新建运行态的静态字段应为空，实际 %+v", rt)
	}

	f := StaticFacts{
		OS: "Debian 12", Arch: "x86_64", Virt: "kvm",
		CPUCores: 4, MemTotal: 8 << 30, SwapTotal: 2 << 30,
		DiskTotal: 167 << 30, BootTime: 1_760_000_000, CounterBits: 64,
	}
	if !g.RestoreStatic(1, f) {
		t.Fatal("RestoreStatic 应返回 true")
	}

	rt, _ = g.Get(1)
	if rt.CPUCores != 4 || rt.MemTotal != 8<<30 || rt.DiskTotal != 167<<30 {
		t.Fatalf("静态信息未恢复：%+v", rt)
	}
	if rt.OS != "Debian 12" || rt.Arch != "x86_64" || rt.Virt != "kvm" {
		t.Fatalf("系统信息未恢复：os=%q arch=%q virt=%q", rt.OS, rt.Arch, rt.Virt)
	}
	if rt.BootTime != 1_760_000_000 || rt.CounterBits != 64 {
		t.Fatalf("开机时间/计数器位数未恢复：%d / %d", rt.BootTime, rt.CounterBits)
	}

	// 已有值不得被库里的旧值覆盖（该节点正在上报，内存里的更新）。
	g.RestoreStatic(1, StaticFacts{CPUCores: 99, MemTotal: 1, OS: "旧系统"})
	rt, _ = g.Get(1)
	if rt.CPUCores != 4 || rt.MemTotal != 8<<30 || rt.OS != "Debian 12" {
		t.Fatalf("非空字段被覆盖了：%+v", rt)
	}

	// 不存在的节点返回 false，不 panic。
	if g.RestoreStatic(999, f) {
		t.Error("对不存在的节点 RestoreStatic 应返回 false")
	}
}
