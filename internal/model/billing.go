package model

// 支付周期与「月均费用」折算（4.4）。
//
// 为什么必须折算：node_billing.price_minor 存的是**该周期的一次性金额**，
// 不是"每月多少钱"。年付 ¥1200 的节点若直接按 1200 累加进「月费合计」，
// 会把月支出虚高成 12 倍；而 lifetime（永久）与 once（一次性）根本没有
// "每月再付"的概念，计进去等于凭空多出一笔不存在的月支出。
//
// 折算规则（四舍五入到分）：
//
//	monthly   → 原值
//	quarterly → price / 3
//	yearly    → price / 12
//	once      → 0（一次性支出，不计入月均）
//	lifetime  → 0（永久，无后续月支出）
//	其他/空   → 视作 monthly（数据缺失时按最常见的周期处理）
//
// 注意这是**展示口径**，不参与任何计费或告警计算——额度比较用的是
// traffic.Billable 的字节数，与金额无关。
func MonthlyEquivalent(priceMinor int64, cycle string) int64 {
	if priceMinor <= 0 {
		return 0
	}
	switch cycle {
	case CycleQuarterly:
		return roundDiv(priceMinor, 3)
	case CycleYearly:
		return roundDiv(priceMinor, 12)
	case CycleOnce, CycleLifetime:
		return 0
	default: // monthly 或未设置
		return priceMinor
	}
}

// roundDiv 做四舍五入的整数除法，避免 ¥100/年 这类小额被截断成 0。
func roundDiv(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	return (a + b/2) / b
}
