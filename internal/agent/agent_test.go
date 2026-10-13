package agent

import (
	"testing"
	"time"

	"github.com/yourorg/probe/internal/config"
)

// TestIntervalDurationGuardsNonPositive 锁定"非正间隔必须兜底"这条规则。
//
// 上报/探测间隔来自手写的 agent.yaml 或 Master 下发的配置，两者都可能出现 0 或负数。
// 一旦把 0 交给 time.NewTicker / time.NewTimer，会直接 panic("non-positive interval")，
// 整个 Agent 进程当场挂掉——而配置是对的（只是没填），现场很难联想到是间隔的锅。
func TestIntervalDurationGuardsNonPositive(t *testing.T) {
	cases := []struct {
		name     string
		seconds  int
		fallback int
		want     time.Duration
	}{
		{"零值退回兜底", 0, 30, 30 * time.Second},
		{"负数退回兜底", -5, 30, 30 * time.Second},
		{"正常值原样返回", 7, 30, 7 * time.Second},
		{"1 秒是合法值", 1, 3, 1 * time.Second},
		{"兜底自身也不为 0", 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := intervalDuration(c.seconds, c.fallback); got != c.want {
				t.Fatalf("intervalDuration(%d, %d) = %v，期望 %v", c.seconds, c.fallback, got, c.want)
			}
		})
	}
}

// TestIntervalDurationNeverZero 单独把"兜底值也不能产出 0"提出来，
// 因为上面那组用例里 fallback=0 的那条本身就是演示"调用方有责任传正数"，
// 这里明确断言常见调用路径（Agent 实际用到的两个兜底常量）不会产出 0。
func TestIntervalDurationNeverZero(t *testing.T) {
	if d := intervalDuration(0, defaultReportIntervalS); d <= 0 {
		t.Fatalf("上报间隔兜底后为 %v，会把 ticker 打崩", d)
	}
	if d := intervalDuration(0, defaultProbeIntervalS); d <= 0 {
		t.Fatalf("探测间隔兜底后为 %v，会把定时器打崩", d)
	}
}

// TestAgentDefaultsMatchMasterDefaults 交叉校验两侧的默认间隔口径。
//
// Master 用 config.BuildEffective 的默认值算出 config_version，Agent 用下面两个常量兜底。
// 两边一旦漂移，就会出现"后台显示 3 秒、Agent 按 10 秒跑"这种谁也说不清的现象，
// 而且不会报错。这条测试把两个包的默认值钉在一起。
func TestAgentDefaultsMatchMasterDefaults(t *testing.T) {
	eff := config.BuildEffective(map[string]string{}, nil)
	if eff.ReportIntervalS != defaultReportIntervalS {
		t.Fatalf("上报间隔默认值不一致：Master=%d，Agent 兜底=%d",
			eff.ReportIntervalS, defaultReportIntervalS)
	}
	if eff.ProbeIntervalS != defaultProbeIntervalS {
		t.Fatalf("探测间隔默认值不一致：Master=%d，Agent 兜底=%d",
			eff.ProbeIntervalS, defaultProbeIntervalS)
	}
}
