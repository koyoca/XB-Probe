package traffic

import (
	"testing"
	"time"

	"github.com/yourorg/probe/internal/model"
)

// 13.4.1 的四条流量增量规则（A1 的核心修订）。
func TestComputeDeltaRules(t *testing.T) {
	base := NICBaseline{LastRx: 1000, LastTx: 500, BootTime: 100, UpdatedAt: 100, Valid: true}

	t.Run("规则1_首次见到网卡增量为零", func(t *testing.T) {
		d := ComputeDelta(NICBaseline{}, model.NICCounters{Rx: 9999, Tx: 9999}, 100, 64)
		if d.Rx != 0 || d.Tx != 0 || d.Restarted || d.Wrapped {
			t.Fatalf("首次见到网卡应只记基线，得到 %+v", d)
		}
	})

	t.Run("规则2_正常增长取差值", func(t *testing.T) {
		d := ComputeDelta(base, model.NICCounters{Rx: 1600, Tx: 900}, 100, 64)
		if d.Rx != 600 || d.Tx != 400 || d.Restarted || d.Wrapped {
			t.Fatalf("正常增长应为差值，得到 %+v", d)
		}
	})

	t.Run("规则3a_boot_time变化按重启处理", func(t *testing.T) {
		// 累计值变小 + 开机时间变化 → 重启，增量 = 当前值
		d := ComputeDelta(base, model.NICCounters{Rx: 50, Tx: 20}, 999, 64)
		if d.Rx != 50 || d.Tx != 20 || !d.Restarted {
			t.Fatalf("重启应取当前值，得到 %+v", d)
		}
	})

	t.Run("规则3b_32位计数器回绕", func(t *testing.T) {
		prev := NICBaseline{LastRx: counterWrap32 - 100, LastTx: counterWrap32 - 100, BootTime: 5, UpdatedAt: 5, Valid: true}
		d := ComputeDelta(prev, model.NICCounters{Rx: 50, Tx: 30}, 5, 32)
		if !d.Wrapped {
			t.Fatalf("应判定为回绕，得到 %+v", d)
		}
		wantRx := int64(50) + counterWrap32 - (counterWrap32 - 100)
		if d.Rx != wantRx {
			t.Fatalf("回绕增量应为 %d，得到 %d", wantRx, d.Rx)
		}
	})

	t.Run("规则3c_64位计数器变小按重置处理", func(t *testing.T) {
		// 计数器位宽未知为 64 且开机时间没变：不回绕，按重置（取当前值）
		prev := NICBaseline{LastRx: counterWrap32 - 100, LastTx: 10, BootTime: 5, UpdatedAt: 5, Valid: true}
		d := ComputeDelta(prev, model.NICCounters{Rx: 50, Tx: 5}, 5, 64)
		if d.Wrapped {
			t.Fatalf("64 位计数器不应判定为回绕，得到 %+v", d)
		}
		if d.Rx != 50 {
			t.Fatalf("重置应取当前值，得到 %d", d.Rx)
		}
	})

	t.Run("虚拟网卡增减不会造成虚高", func(t *testing.T) {
		// 这是 A1 要解决的场景：eth0 平稳增长，docker0 消失。
		// 按网卡分别计量时，eth0 只贡献自己的真实增量。
		eth0 := NICBaseline{LastRx: 1000, LastTx: 500, BootTime: 100, UpdatedAt: 100, Valid: true}
		d := ComputeDelta(eth0, model.NICCounters{Rx: 1200, Tx: 700}, 100, 64)
		if d.Rx != 200 || d.Tx != 200 {
			t.Fatalf("eth0 增量应为 200/200，得到 %+v", d)
		}
	})
}

func TestApplyMode(t *testing.T) {
	cases := []struct {
		mode string
		want int64
	}{
		{model.TrafficBoth, 300},
		{model.TrafficUp, 200},
		{model.TrafficDown, 100},
		{model.TrafficMax, 200},
	}
	for _, c := range cases {
		if got := ApplyMode(100, 200, c.mode); got != c.want {
			t.Errorf("ApplyMode(%s) = %d，期望 %d", c.mode, got, c.want)
		}
	}
}

// 4.4 流量倍率：计费流量 = 计量口径 × 倍率。
//
// 这里最要紧的不是乘法本身，而是三条兜底：倍率为 0（老库缺列/前端漏填）必须
// 按 1 倍处理——若按 0 处理，所有节点的用量都会瞬间归零，管理员会以为数据丢了。
func TestApplyMultiplier(t *testing.T) {
	const gb int64 = 1 << 30

	if got := ApplyMultiplier(gb, 1); got != gb {
		t.Errorf("倍率 1 应原样返回，得到 %d", got)
	}
	if got := ApplyMultiplier(gb, 2); got != 2*gb {
		t.Errorf("倍率 2 应为 2 倍，得到 %d", got)
	}
	if got := ApplyMultiplier(gb, 0.5); got != gb/2 {
		t.Errorf("倍率 0.5 应折半，得到 %d", got)
	}
	// 0 与负数一律按 1 处理（宁可显示原值，也不能凭空清零）。
	for _, m := range []float64{0, -1, -0.5} {
		if got := ApplyMultiplier(gb, m); got != gb {
			t.Errorf("倍率 %v 应按 1 处理，得到 %d", m, got)
		}
	}
	// 浮点误差不得导致少算：0.1 × 15 在二进制里是 1.4999…，截断会丢 1 字节。
	if got := ApplyMultiplier(15, 0.1); got != 2 {
		t.Errorf("0.1 倍率应四舍五入而非截断，得到 %d", got)
	}
	// 口径与倍率串联：先按 mode 折算，再乘倍率。
	if got := Billable(100, 200, model.TrafficMax, 1.5); got != 300 {
		t.Errorf("Billable(max=200, ×1.5) 应为 300，得到 %d", got)
	}
	if got := Billable(100, 200, model.TrafficBoth, 0); got != 300 {
		t.Errorf("倍率 0 应按 1 倍处理，得到 %d", got)
	}
}

// 4.3 周期窗口：重置日大于当月天数时收敛到当月最后一天。
func TestPeriodWindowResetDay(t *testing.T) {
	utc := time.UTC

	t.Run("31日在2月收敛为29日_闰年", func(t *testing.T) {
		// 2028-02-15 处于 [2028-02-29, 2028-03-31) 之前，所以应落在上一窗口
		now := time.Date(2028, 2, 15, 12, 0, 0, 0, utc)
		start, end := PeriodWindow(now, 31, utc)
		if start != time.Date(2028, 1, 31, 0, 0, 0, 0, utc) {
			t.Errorf("start = %v，期望 2028-01-31", start)
		}
		if end != time.Date(2028, 2, 29, 0, 0, 0, 0, utc) {
			t.Errorf("end = %v，期望 2028-02-29（闰年收敛）", end)
		}
	})

	t.Run("31日在2月收敛为28日_平年", func(t *testing.T) {
		now := time.Date(2027, 3, 1, 0, 0, 0, 0, utc)
		start, end := PeriodWindow(now, 31, utc)
		if start != time.Date(2027, 2, 28, 0, 0, 0, 0, utc) {
			t.Errorf("start = %v，期望 2027-02-28", start)
		}
		if end != time.Date(2027, 3, 31, 0, 0, 0, 0, utc) {
			t.Errorf("end = %v，期望 2027-03-31", end)
		}
	})

	t.Run("窗口起点为当天00点", func(t *testing.T) {
		now := time.Date(2026, 10, 11, 17, 30, 0, 0, utc)
		start, end := PeriodWindow(now, 1, utc)
		if start != time.Date(2026, 10, 1, 0, 0, 0, 0, utc) {
			t.Errorf("start = %v，期望 2026-10-01 00:00", start)
		}
		if end != time.Date(2026, 11, 1, 0, 0, 0, 0, utc) {
			t.Errorf("end = %v，期望 2026-11-01 00:00", end)
		}
	})

	t.Run("跨年边界", func(t *testing.T) {
		now := time.Date(2026, 12, 20, 0, 0, 0, 0, utc)
		start, end := PeriodWindow(now, 15, utc)
		if start != time.Date(2026, 12, 15, 0, 0, 0, 0, utc) {
			t.Errorf("start = %v，期望 2026-12-15", start)
		}
		if end != time.Date(2027, 1, 15, 0, 0, 0, 0, utc) {
			t.Errorf("end = %v，期望 2027-01-15", end)
		}
	})

	t.Run("非法重置日按1处理", func(t *testing.T) {
		now := time.Date(2026, 10, 11, 0, 0, 0, 0, utc)
		start, _ := PeriodWindow(now, 0, utc)
		if start != time.Date(2026, 10, 1, 0, 0, 0, 0, utc) {
			t.Errorf("start = %v，期望 2026-10-01", start)
		}
	})

	t.Run("时区生效", func(t *testing.T) {
		loc, err := time.LoadLocation("Asia/Shanghai")
		if err != nil {
			t.Skip("缺少时区数据库")
		}
		// UTC 2026-09-30 20:00 == 上海 2026-10-01 04:00，应已进入 10 月窗口
		now := time.Date(2026, 9, 30, 20, 0, 0, 0, utc)
		start, _ := PeriodWindow(now, 1, loc)
		if start != time.Date(2026, 10, 1, 0, 0, 0, 0, loc) {
			t.Errorf("start = %v，期望上海时间 2026-10-01 00:00", start)
		}
	})
}

func TestDaysUntil(t *testing.T) {
	now := int64(1_760_000_000)
	if got := DaysUntil(0, now); got != 0 {
		t.Errorf("永久节点应为 0，得到 %d", got)
	}
	if got := DaysUntil(now+86400, now); got != 1 {
		t.Errorf("恰好一天应为 1，得到 %d", got)
	}
	if got := DaysUntil(now+1, now); got != 1 {
		t.Errorf("不足一天向上取整应为 1，得到 %d", got)
	}
	if got := DaysUntil(now-86400, now); got != -1 {
		t.Errorf("已过期一天应为 -1，得到 %d", got)
	}
	// 已过期方向必须**向零截断**：过期 3 天零 10 分钟只能算 3 天。
	// 曾经两个方向都用 ceil，导致这个节点在看板上显示成"已过期4天"。
	if got := DaysUntil(now-86400*3-600, now); got != -3 {
		t.Errorf("过期 3 天零 10 分钟应为 -3，得到 %d", got)
	}
	if got := DaysUntil(now-86400*3+1, now); got != -2 {
		t.Errorf("还差 1 秒满 3 天应为 -2（已过去两个整天），得到 %d", got)
	}
	// 未到期方向仍然向上取整：剩 1 秒也是"1 天后到期"。
	if got := DaysUntil(now+86400*3, now); got != 3 {
		t.Errorf("恰好 3 天应为 3，得到 %d", got)
	}
	if got := DaysUntil(now+86400*3+1, now); got != 4 {
		t.Errorf("3 天零 1 秒应为 4（向上取整），得到 %d", got)
	}
}

func TestNextExpire(t *testing.T) {
	utc := time.UTC
	jan31 := time.Date(2027, 1, 31, 0, 0, 0, 0, utc).Unix()
	// 月付顺延一期：Go 的 AddDate 在 1/31 + 1 月时收敛到 3/3（2 月无 31 日）
	got := NextExpire(jan31, "monthly", utc)
	if got <= jan31 {
		t.Fatalf("续费后必须晚于原到期时间，得到 %d", got)
	}
	if NextExpire(jan31, "lifetime", utc) != 0 {
		t.Error("永久节点不参与续费顺延，应返回 0")
	}
	if NextExpire(0, "monthly", utc) != 0 {
		t.Error("未设置到期时间应返回 0")
	}
}
