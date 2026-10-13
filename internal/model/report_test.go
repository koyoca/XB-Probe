package model

import "testing"

// 延迟状态档位（LatencyStatus）的回归测试。
//
// 这个函数被两个地方共用：调度器（每秒汇总的实时指示灯）与公开 API
// （历史窗口统计）。所以它的阈值必须有一份明确的测试锁住——
// 两处显示同一条链路却给出不同颜色的那种 bug，靠人眼是发现不了的。

func TestLatencyStatusThresholds(t *testing.T) {
	us := func(v int64) *int64 { return &v }

	cases := []struct {
		name string
		rtt  *int64
		err  string
		want string
	}{
		{"无 RTT 且无错误 -> bad（探通了但拿不到延时，按最差处理）", nil, "", LatencyBad},
		{"探测报错 -> unknown（数字不可信，比 bad 更严重）", us(12000), ProbeErrTimeout, LatencyUnknown},
		{"探测报错且无 RTT -> unknown", nil, ProbeErrRefused, LatencyUnknown},
		{"0 微秒 -> ok", us(0), "", LatencyOK},
		{"49ms -> ok（阈值下沿）", us(49_999), "", LatencyOK},
		{"50ms -> warn（阈值上沿，恰好跨档）", us(50_000), "", LatencyWarn},
		{"149ms -> warn", us(149_999), "", LatencyWarn},
		{"150ms -> bad", us(150_000), "", LatencyBad},
		{"1000ms -> bad", us(1_000_000), "", LatencyBad},
	}
	for _, c := range cases {
		if got := LatencyStatus(c.rtt, c.err); got != c.want {
			t.Errorf("%s：LatencyStatus = %q，期望 %q", c.name, got, c.want)
		}
	}
}
