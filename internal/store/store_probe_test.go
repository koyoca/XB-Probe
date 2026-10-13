package store

import (
	"testing"

	"github.com/koyoca/xb-probe/internal/model"
)

// 回归：节点 A 的延迟点阵绝不能读到"节点 B 专属目标"的桶。
//
// 缺陷现象（用户上报）：没有专属探测目标的节点，卡片上也渲染出了别的节点的
// 专属探测目标。根因是 LatencyBucketsForNode 的 JOIN 只按 target_id 关联，
// 没校验目标作用域——只要 latency_10m 里存在一行 target_id 指向别的节点的专属目标，
// 它就会被算进本节点的点阵。
//
// 这条用例覆盖"库里已有脏行"的场景（历史误写 / 越权上报），是读侧的护栏；
// 写侧的准入过滤（api.allowedTargetIDs）另有 api 包用例覆盖。
func TestLatencyBucketsForNodeRejectsForeignTarget(t *testing.T) {
	r := newTestRepo(t)
	a := mustNode(t, r, "nodeA")
	b := mustNode(t, r, "nodeB")
	now := int64(1_760_000_000)

	// B 的专属目标（node_id = B）——A 不该看到它。
	tb := &model.ProbeTarget{NodeID: b, Name: "B专线", Carrier: "telecom",
		Host: "1.1.1.1", Port: 443, Method: "tcp", TimeoutMs: 2000, Enabled: true, ShowInBars: true}
	foreignID, err := r.CreateProbeTarget(tb)
	if err != nil {
		t.Fatalf("建 B 专属目标失败：%v", err)
	}
	// A 自己的专属目标（对照组：必须仍然看得到）。
	ta := &model.ProbeTarget{NodeID: a, Name: "A专线", Carrier: "unicom",
		Host: "8.8.8.8", Port: 53, Method: "tcp", TimeoutMs: 2000, Enabled: true, ShowInBars: true}
	ownID, err := r.CreateProbeTarget(ta)
	if err != nil {
		t.Fatalf("建 A 专属目标失败：%v", err)
	}
	// 一个全局目标（b.node_id 为 NULL，A 也必须看得到）。
	tg := &model.ProbeTarget{Name: "全局-移动", Carrier: "mobile",
		Host: "114.114.114.114", Port: 53, Method: "tcp", TimeoutMs: 2000, Enabled: true, ShowInBars: true}
	globalID, err := r.CreateProbeTarget(tg)
	if err != nil {
		t.Fatalf("建全局目标失败：%v", err)
	}

	ins := func(targetID int64, rtt int64) {
		t.Helper()
		if _, err := r.db.Write.Exec(
			`INSERT INTO latency_10m(node_id, target_id, ts, rtt_avg, rtt_min, rtt_max, loss)
			 VALUES(?,?,?,?,?,?,?)`, a, targetID, now-600, rtt, rtt-100, rtt+100, 0); err != nil {
			t.Fatalf("写 A 的桶(target=%d)失败：%v", targetID, err)
		}
	}
	ins(foreignID, 12345) // 脏行：A 的记录指向 B 的专属目标
	ins(ownID, 2000)      // 合法：A 自己的专属目标
	ins(globalID, 3000)   // 合法：全局目标

	buckets, err := r.LatencyBucketsForNode(a, now-3600)
	if err != nil {
		t.Fatalf("查桶失败：%v", err)
	}

	byCarrier := map[string]int{}
	for _, x := range buckets {
		byCarrier[x.Carrier]++
	}
	if byCarrier["telecom"] != 0 {
		t.Errorf("泄漏：节点 A 读到了 %d 个属于节点 B 专属目标(telecom)的桶", byCarrier["telecom"])
	}
	if byCarrier["unicom"] != 1 {
		t.Errorf("节点 A 应读到自己专属目标(unicom)的 1 个桶，实际 %d", byCarrier["unicom"])
	}
	if byCarrier["mobile"] != 1 {
		t.Errorf("节点 A 应读到全局目标(mobile)的 1 个桶，实际 %d", byCarrier["mobile"])
	}
	if len(buckets) != 2 {
		t.Errorf("节点 A 的桶数应为 2（自己 + 全局），实际 %d", len(buckets))
	}
	// 点阵现在按**目标**一行（行标签是目标名），SELECT 必须把 target_id 与
	// 目标名一起带出来——漏了任何一个，聚合就只能退回按运营组合并。
	for _, x := range buckets {
		switch x.Carrier {
		case "unicom":
			if x.TargetID != ownID || x.Name != "A专线" {
				t.Errorf("专属桶应带 target_id=%d/name=A专线，实得 id=%d name=%q", ownID, x.TargetID, x.Name)
			}
		case "mobile":
			if x.TargetID != globalID || x.Name != "全局-移动" {
				t.Errorf("全局桶应带 target_id=%d/name=全局-移动，实得 id=%d name=%q", globalID, x.TargetID, x.Name)
			}
		}
	}
}
