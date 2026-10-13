package traffic

import "time"

// 流量周期窗口计算（文档 4.3）。
//
// 规则：按“每月重置日 + 时区”计算当前窗口；重置日大于当月天数时取当月最后一天
// （如 31 日在 2 月取 28/29 日）；窗口起点以 00:00 为准。

// PeriodWindow 返回 now 所处计费周期的 [start, end)。
// resetDay 取值 1~31，非法值按 1 处理。loc 为空时使用 UTC。
func PeriodWindow(now time.Time, resetDay int, loc *time.Location) (start, end time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	if resetDay < 1 || resetDay > 31 {
		resetDay = 1
	}
	t := now.In(loc)

	thisMonth := resetInMonth(t.Year(), t.Month(), resetDay, loc)
	if !t.Before(thisMonth) {
		next := t.AddDate(0, 1, 0)
		return thisMonth, resetInMonth(next.Year(), next.Month(), resetDay, loc)
	}
	prev := t.AddDate(0, -1, 0)
	return resetInMonth(prev.Year(), prev.Month(), resetDay, loc), thisMonth
}

// resetInMonth 返回某年某月的重置时刻（该月 00:00），自动按当月天数收敛。
func resetInMonth(year int, month time.Month, resetDay int, loc *time.Location) time.Time {
	last := lastDayOfMonth(year, month)
	if resetDay > last {
		resetDay = last
	}
	return time.Date(year, month, resetDay, 0, 0, 0, 0, loc)
}

// lastDayOfMonth 返回某年某月的最后一天。
func lastDayOfMonth(year int, month time.Month) int {
	// 下月 1 号往前一天即为本月最后一天。
	return time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1).Day()
}

// InPeriod 判断 ts（Unix 秒）是否落在 [start, end) 内。
func InPeriod(ts int64, start, end time.Time) bool {
	return ts >= start.Unix() && ts < end.Unix()
}

// --- 续费顺延（4.4）---

// NextExpire 按支付周期把到期时间顺延一期。
// expireAt 为 0 或 cycle=lifetime 时返回 0（永久节点不参与到期扫描）。
func NextExpire(expireAt int64, cycle string, loc *time.Location) int64 {
	if cycle == "lifetime" || expireAt == 0 {
		return 0
	}
	if loc == nil {
		loc = time.UTC
	}
	t := time.Unix(expireAt, 0).In(loc)
	switch cycle {
	case "monthly":
		t = t.AddDate(0, 1, 0)
	case "quarterly":
		t = t.AddDate(0, 3, 0)
	case "yearly":
		t = t.AddDate(1, 0, 0)
	case "once":
		// 一次性：续费后顺延一个月，由管理员再手动调整。
		t = t.AddDate(0, 1, 0)
	default:
		t = t.AddDate(0, 1, 0)
	}
	return t.Unix()
}

// LocalDay 返回 now 在指定时区下的日期字符串（YYYY-MM-DD），与
// traffic_daily.day 的存储口径一致（节点时区）。
//
// 各节点时区可能不同，"今天"不是同一天，因此按节点挑当日行时必须用它，
// 而不能用 Master 本机日期。tz 为空或无法解析时回退到本机时区。
func LocalDay(now time.Time, tz string) string {
	loc := time.Local
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	return now.In(loc).Format("2006-01-02")
}

// DaysUntil 返回距离到期的天数（未到期向上取整，已过期向下取整，可为负）。
//
// 两个方向刻意不对称，因为两个方向的"取整"含义不同：
//
//	未到期：diff > 0 → ceil。只剩 3 小时要显示"1 天后到期"而不是"0 天"，
//	        否则看板上会出现"0天后到期"这种看不懂的文案，也容易让人以为
//	        还有整整一天。
//	已过期：diff < 0 → 向零截断（也就是"已经过去了几个整天"）。
//	        曾经这里也写成 ceil（向远离零的方向取整），于是 seed 里
//	        "3 天前过期"的节点在看板上显示成**"已过期4天"**——只要不是恰好
//	        落在整天的边界上就统一多算一天。往前多报一天属于凭空造数据。
func DaysUntil(expireAt, now int64) int {
	if expireAt == 0 {
		return 0
	}
	diff := expireAt - now
	if diff > 0 {
		return int((diff + 86399) / 86400)
	}
	// diff <= 0：-diff/86400 向零截断。恰好 0 时（刚过期）返回 0，
	// 前端会渲染成"今天到期"。
	return -int((-diff) / 86400)
}
