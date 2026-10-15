package model

import (
	"strconv"
	"strings"
)

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

// ---------------------------------------------------------------------------
// 多币种换算（月费合计以主币种计价）
// ---------------------------------------------------------------------------

// Rates 是"相对主币种"的汇率表：键是币种代码，值是 **1 单位该币种 = 多少主币种**。
//
// 用法与语义：
//
//	rates := model.ParseRates("USD:7,JPY:0.05")
//	rates.Convert(1999, "USD", "CNY")  // 1999 分美元 → 人民币分
//
// 主币种自身不需要出现在表里（换算时按 1 处理）；表里若出现主币种，
// 以 1 为准——否则一个手误就能让"本币换算成自身"变成一个倍率。
type Rates struct {
	// Base 是主币种（ISO 4217 大写）。空串表示不换算（沿用旧行为）。
	Base string
	// Table 是 币种 → 相对主币种的汇率。数值为 0 或负数的条目在解析时被丢弃。
	Table map[string]float64
}

// Convert 把以 from 币种计价的金额（最小单位整数分）换算成主币种。
//
// 三种"不换算"的情形都必须**原样返回**，而不是返回 0：
//   - 未配置主币种（Base 为空）：旧行为，照旧累加；
//   - from 就是主币种：汇率恒为 1；
//   - 表里没有 from 的汇率：没有依据，宁可按原值计入（屏幕上会显示成主币种金额，
//     但金额本身没被错误地乘上一个编造的倍率）。这一点是刻意的取舍——
//     少算和多算都会误导，而"按 1 算"至少与升级前完全一致。
func (r Rates) Convert(minor int64, from string) int64 {
	if r.Base == "" || minor == 0 {
		return minor
	}
	if from == "" || from == r.Base {
		return minor
	}
	rate, ok := r.Table[from]
	if !ok || rate <= 0 {
		return minor
	}
	return int64(float64(minor)*rate + 0.5)
}

// ParseRates 解析 `币种:汇率` 逗号（或空白）分隔的清单。
//
// 容错原则：**坏条目丢弃、好条目照用**。这与设置项校验（保存时就拒绝非法格式）
// 是两层——校验保证"经后台保存的值是好的"，解析层的容错保证"哪怕库里有历史脏值，
// 也不会让整张表失效、把合计变成 0"。币种统一转大写。
func ParseRates(base, raw string) Rates {
	r := Rates{Base: strings.ToUpper(strings.TrimSpace(base)), Table: map[string]float64{}}
	if r.Base == "" {
		return r
	}
	for _, item := range strings.FieldsFunc(raw, func(c rune) bool {
		return c == ',' || c == ';' || c == ' ' || c == '\t' || c == '\n' || c == '\r'
	}) {
		code, val, ok := strings.Cut(item, ":")
		if !ok {
			code, val, ok = strings.Cut(item, "=")
		}
		if !ok {
			continue
		}
		code = strings.ToUpper(strings.TrimSpace(code))
		if code == "" || code == r.Base {
			continue // 主币种自身的汇率恒为 1，不吃表里的值
		}
		rate, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil || rate <= 0 {
			continue
		}
		r.Table[code] = rate
	}
	return r
}
