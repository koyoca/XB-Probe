package store

import (
	"testing"
)

// 节点地址的三态与历史数据修复。
//
// 这组测试对着一个真实故障：老实现把上报连接的对端地址无条件写进 nodes.ip_v4，
// 同机部署的节点因此存着 127.0.0.1；新规则不再写这种值，于是**没人会去覆盖它** ——
// 不清理的话，用户升级后看到地址栏照旧，会合理地认为改动没生效。

// TestClearNonPublicObservedIPsOnlyTouchesAutoRows 钉住清理的两个方向。
//
// 两个方向都要钉：只验"清掉了"的话，"把手工填的地址也一起清掉"这种实现同样全绿，
// 而那是把用户的数据删了。
func TestClearNonPublicObservedIPsOnlyTouchesAutoRows(t *testing.T) {
	r := newTestRepo(t)

	// 自动态 + 回环/私网历史值 → 应被清掉。
	loopback := mustNode(t, r, "loopback")
	if _, err := r.UpdateNodeObservedIP(loopback, "127.0.0.1"); err != nil {
		t.Fatalf("写回环地址失败：%v", err)
	}
	v6loop := mustNode(t, r, "v6loop")
	if _, err := r.UpdateNodeObservedIP(v6loop, "fd00::1"); err != nil {
		t.Fatalf("写 ULA 地址失败：%v", err)
	}

	// 自动态 + 真公网地址 → 必须留着（否则这条清理会把好数据也毁掉）。
	good := mustNode(t, r, "good")
	if _, err := r.UpdateNodeObservedIP(good, "203.0.113.9"); err != nil {
		t.Fatalf("写公网地址失败：%v", err)
	}

	// 手动态 + 内网地址 → 留着。管理员填了内网地址是他的选择
	// （内网节点用内网地址完全合理），一次静默的启动清理不该动它。
	manual := mustNode(t, r, "manual")
	if err := r.UpdateNodeAssets(manual, "192.168.1.5", "", "", "", "", ""); err != nil {
		t.Fatalf("手工填地址失败：%v", err)
	}

	n, err := r.ClearNonPublicObservedIPs()
	if err != nil {
		t.Fatalf("清理失败：%v", err)
	}
	if n != 2 {
		t.Errorf("清理了 %d 行，期望 2（回环 v4 + ULA v6）", n)
	}

	check := func(id int64, wantV4, wantV6, what string) {
		t.Helper()
		node, err := r.GetNode(id)
		if err != nil || node == nil {
			t.Fatalf("读节点失败：%v", err)
		}
		if node.IPv4 != wantV4 || node.IPv6 != wantV6 {
			t.Errorf("%s：地址 = (%q, %q)，期望 (%q, %q)", what, node.IPv4, node.IPv6, wantV4, wantV6)
		}
	}
	check(loopback, "", "", "回环地址应被清掉")
	check(v6loop, "", "", "ULA 地址应被清掉")
	check(good, "203.0.113.9", "", "公网地址不该被清掉")
	check(manual, "192.168.1.5", "", "手工填的地址不该被清掉")

	// 幂等：再跑一次无事可做。
	if n, err := r.ClearNonPublicObservedIPs(); err != nil || n != 0 {
		t.Errorf("第二次清理改动了 %d 行（err=%v），应为 0", n, err)
	}
}

// TestUpdateNodeObservedIPRefusesManualRowsAndRewritesOnlyOnChange 钉住写入侧的门控。
//
//   - ip_auto=0（手工填过）时**不写**：这是面板上"填写即覆盖"四个字的兑现；
//   - 值与库里相同时**不写**：上报是几秒一次的高频路径，每次都写库是纯粹的浪费
//     （返回值 > 0 也是"真的改了"这个信号，它必须是真的）。
//
// 第二条容易被写成"总是返回 true"，那样调用方就失去了"这次到底改没改"的信息。
func TestUpdateNodeObservedIPRefusesManualRowsAndRewritesOnlyOnChange(t *testing.T) {
	r := newTestRepo(t)

	auto := mustNode(t, r, "auto")
	changed, err := r.UpdateNodeObservedIP(auto, "203.0.113.9")
	if err != nil || !changed {
		t.Fatalf("首次写入应返回 changed=true：changed=%v err=%v", changed, err)
	}
	changed, err = r.UpdateNodeObservedIP(auto, "203.0.113.9")
	if err != nil {
		t.Fatalf("重复写入失败：%v", err)
	}
	if changed {
		t.Error("值没变却报告 changed=true（调用方会以为地址变了）")
	}

	manual := mustNode(t, r, "manual2")
	if err := r.UpdateNodeAssets(manual, "198.51.100.77", "", "", "", "", ""); err != nil {
		t.Fatalf("手工填地址失败：%v", err)
	}
	changed, err = r.UpdateNodeObservedIP(manual, "203.0.113.9")
	if err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if changed {
		t.Error("手工指定过地址的节点不该被自动探测改写")
	}
	if n, _ := r.GetNode(manual); n.IPv4 != "198.51.100.77" {
		t.Errorf("手工地址被改成了 %q", n.IPv4)
	}
}

// TestUpdateNodeObservedIPPicksColumnByAddressFamily 钉住"IPv6 不准写进 v4 列"。
//
// 写错列的症状是：一个公网 IPv6 节点在界面上显示一个谁也连不上的 v4 地址，
// 而两个字段都是字符串，类型系统不会替我们拦住。
func TestUpdateNodeObservedIPPicksColumnByAddressFamily(t *testing.T) {
	r := newTestRepo(t)
	id := mustNode(t, r, "dualstack")

	if _, err := r.UpdateNodeObservedIP(id, "2001:4860:4860::8888"); err != nil {
		t.Fatalf("写 IPv6 失败：%v", err)
	}
	n, _ := r.GetNode(id)
	if n.IPv6 != "2001:4860:4860::8888" {
		t.Errorf("IPv6 地址没写进 ip_v6：%+v", n)
	}
	if n.IPv4 != "" {
		t.Errorf("IPv6 地址被写进了 ip_v4：%q", n.IPv4)
	}
}
