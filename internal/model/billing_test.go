package model

import "testing"

// 月均折算（4.4）。
//
// 这是「月费合计」卡片的口径定义：node_billing.price_minor 是**该周期一次性
// 金额**，直接累加会把年付节点算成 12 倍，也会把 lifetime/once 这种没有
// 月支出的节点计进去。这里把每条规则钉死，避免以后再被"简单求和"改回去。
func TestMonthlyEquivalent(t *testing.T) {
	cases := []struct {
		name  string
		price int64
		cycle string
		want  int64
	}{
		{"月付原值", 4900, CycleMonthly, 4900},
		{"周期为空视作月付", 4900, "", 4900},
		{"季付除以 3", 30000, CycleQuarterly, 10000},
		{"年付除以 12", 120000, CycleYearly, 10000},
		{"年付四舍五入", 100, CycleYearly, 8}, // 8.33 → 8
		{"永久不计月支出", 4900, CycleLifetime, 0},
		{"一次性不计月支出", 4900, CycleOnce, 0},
		{"零价为零", 0, CycleMonthly, 0},
		{"负价按零处理", -100, CycleMonthly, 0},
		{"未知周期按月付", 4900, "weird", 4900},
	}
	for _, c := range cases {
		if got := MonthlyEquivalent(c.price, c.cycle); got != c.want {
			t.Errorf("%s：MonthlyEquivalent(%d, %q) = %d，期望 %d",
				c.name, c.price, c.cycle, got, c.want)
		}
	}
}
