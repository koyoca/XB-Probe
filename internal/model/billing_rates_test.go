package model

import "testing"

// 多币种换算（月费合计以主币种计价）。
//
// 这一层最容易错的地方不是"算错乘数"，而是**该不该乘**：
// 未配置主币种、币种相同、表里没有该币种，这三种情形都必须原样返回，
// 一旦误乘就会让升级后的合计数字凭空变化——而"数字变了"极难被察觉。

func TestParseRatesBasics(t *testing.T) {
	r := ParseRates("cny", "usd:7, jpy:0.05")
	if r.Base != "CNY" {
		t.Errorf("主币种应统一转大写，得到 %q", r.Base)
	}
	if r.Table["USD"] != 7 || r.Table["JPY"] != 0.05 {
		t.Errorf("汇率表解析不对：%+v", r.Table)
	}
}

func TestParseRatesDropsBadEntries(t *testing.T) {
	// 坏条目丢弃、好条目照用：库里若有历史脏值，整张表不能因此失效。
	r := ParseRates("CNY", "USD:7,badentry,JPY:0,EUR:-1,GBP:abc,KRW:180")
	if r.Table["USD"] != 7 {
		t.Error("好条目 USD 被坏条目带崩了")
	}
	if _, ok := r.Table["JPY"]; ok {
		t.Error("汇率为 0 的条目应被丢弃（写成 0 会让该币种金额全变 0）")
	}
	if _, ok := r.Table["EUR"]; ok {
		t.Error("负汇率应被丢弃")
	}
	if _, ok := r.Table["GBP"]; ok {
		t.Error("非数字汇率应被丢弃")
	}
	if r.Table["KRW"] != 180 {
		t.Error("坏条目之后的条目也应照常解析")
	}
}

func TestParseRatesIgnoresBaseItself(t *testing.T) {
	// 主币种自身的汇率恒为 1：表里写上它也不能改变语义，
	// 否则一个手误就能让"本币换算成自身"变成乘一个倍率。
	r := ParseRates("CNY", "CNY:99,USD:7")
	if _, ok := r.Table["CNY"]; ok {
		t.Fatal("主币种不应出现在汇率表里")
	}
	if got := r.Convert(10000, "CNY"); got != 10000 {
		t.Errorf("主币种换算必须原样返回，得到 %d", got)
	}
}

func TestConvertReturnsUnchangedWhenNotConfigured(t *testing.T) {
	// 未配置主币种 = 升级前行为：一律原样返回。
	r := ParseRates("", "USD:7")
	if got := r.Convert(1999, "USD"); got != 1999 {
		t.Errorf("未配主币种时不该换算，得到 %d", got)
	}
}

func TestConvertUnknownCurrencyKeepsValue(t *testing.T) {
	// 表里没有该币种 → 没有依据，按原值计入（不编造倍率）。
	r := ParseRates("CNY", "USD:7")
	if got := r.Convert(1999, "AUD"); got != 1999 {
		t.Errorf("未知币种应按原值计入，得到 %d", got)
	}
	// 币种为空的节点（没填币种）同样原样计入。
	if got := r.Convert(1999, ""); got != 1999 {
		t.Errorf("空币种应按原值计入，得到 %d", got)
	}
}

func TestConvertAppliesRateWithRounding(t *testing.T) {
	r := ParseRates("CNY", "USD:7")
	if got := r.Convert(1999, "USD"); got != 13993 {
		t.Errorf("1999 分美元 × 7 = 13993 分人民币，得到 %d", got)
	}
	// 需要四舍五入的场景：0.05 × 1999 = 99.95 → 100
	r2 := ParseRates("CNY", "JPY:0.05")
	if got := r2.Convert(1999, "JPY"); got != 100 {
		t.Errorf("99.95 应四舍五入到 100，得到 %d", got)
	}
}
