package api

import (
	"strings"
	"testing"

	"github.com/yourorg/probe/internal/model"
)

// public.fields 的取值校验（7.3）。
//
// 关键取舍：未知字段名必须报错，而不是静默忽略。若静默忽略，管理员会以为
// 某个字段已经关掉了，实际仍在对外输出——这是唯一不能出错的方向。
func TestValidatePublicFields(t *testing.T) {
	// 空值 = 不过滤（公开白名单全集）。
	if err := validatePublicFields(""); err != nil {
		t.Errorf("空值应通过：%v", err)
	}
	// 全量清单必须原样通过（后台"全选"按钮就会提交它）。
	if err := validatePublicFields(model.DefaultPublicFields()); err != nil {
		t.Errorf("默认全量清单应通过：%v", err)
	}
	// 大小写、空格、分号都宽容。
	if err := validatePublicFields(" CPU_PCT , mem_pct ; disk_pct "); err != nil {
		t.Errorf("应容忍大小写与多种分隔符：%v", err)
	}
	// 未知字段必须被拒。
	for _, bad := range []string{"cpu_pct,不存在的字段", "ip_v4", "hostname", "price_minor"} {
		if err := validatePublicFields(bad); err == nil {
			t.Errorf("%q 应被拒绝（防止误以为已关闭）", bad)
		}
	}
	// 敏感字段名不得出现在可选清单里，因此它们在语义上也不可能被"允许"。
	for _, bad := range []string{"ip", "host", "token", "price"} {
		if err := validatePublicFields(bad); err == nil {
			t.Errorf("敏感字段名 %q 竟被接受", bad)
		}
	}
}

// settingDefaults 与 settingSpecs 的键集合必须完全一致，
// 否则会出现"某个设置项有默认值但无法保存"或反之的错位。
func TestSettingDefaultsAndSpecsKeySetsMatch(t *testing.T) {
	for k := range settingDefaults {
		if _, ok := settingSpecs[k]; !ok {
			t.Errorf("默认值里的 %q 在校验器表里没有对应项", k)
		}
	}
	for k := range settingSpecs {
		if _, ok := settingDefaults[k]; !ok {
			t.Errorf("校验器表里的 %q 没有默认值，后台会显示为空白", k)
		}
	}
}

// public.fields 的默认值必须覆盖全部可收窄字段（默认全放开）。
func TestPublicFieldsDefaultIsFullWhitelist(t *testing.T) {
	def, ok := settingDefaults["public.fields"]
	if !ok {
		t.Fatal("public.fields 缺少默认值")
	}
	if def != "" {
		t.Errorf("默认值应为空串（表示不过滤），得到 %q", def)
	}
	// 空串解析后必须是 nil（不过滤），而不是"全部关闭"。
	if model.ParsePublicFields(def) != nil {
		t.Error("空默认值必须被解析为 nil（不过滤），否则看板会一片空白")
	}
	if !strings.Contains(model.DefaultPublicFields(), "cpu_pct") {
		t.Error("默认全量清单应包含 cpu_pct")
	}
}

// public.cards 的取值校验。
//
// 与 public.fields 的关键差异：这里**空串是合法值**，含义是"一张卡片都不展示"。
// 因此默认值不能用空串，必须用哨兵 "all"，否则"从没配过"与"全部关掉"会
// 变成同一个值——管理员关掉全部卡片后看板反而全亮。
func TestValidatePublicCards(t *testing.T) {
	cases := []struct {
		in   string
		want bool // true = 应通过
	}{
		{"", true},                            // 全部关掉
		{"all", true},                         // 不限制
		{"ALL", true},                         // 哨兵大小写不敏感
		{"kpi_fee", true},                     // 单张
		{"kpi_fee, kpi_net ; info_cpu", true}, // 多张 + 混合分隔符
		{model.DefaultPublicCards(), true},    // 默认值
		{"kpi_fee,不存在", false},                // 未知键必须被拒
		{"price", false},                      // 不是卡片键
		{"cpu_pct", false},                    // 是字段键但不是卡片键
	}
	for _, c := range cases {
		err := validatePublicCards(c.in)
		if c.want && err != nil {
			t.Errorf("validatePublicCards(%q) 应通过，实得 %v", c.in, err)
		}
		if !c.want && err == nil {
			t.Errorf("validatePublicCards(%q) 应被拒绝", c.in)
		}
	}
}

// public.cards 的默认值必须是哨兵 "all"（不限制），且能被解析为 nil。
func TestPublicCardsDefaultIsUnrestricted(t *testing.T) {
	def, ok := settingDefaults["public.cards"]
	if !ok {
		t.Fatal("public.cards 缺少默认值")
	}
	if def != model.PublicCardsAll {
		t.Errorf("默认值应为 %q，得到 %q", model.PublicCardsAll, def)
	}
	if model.ParsePublicCards(def) != nil {
		t.Error("默认值必须解析为 nil（不限制），否则看板会一片空白")
	}
	// 反过来：空串必须解析为**非 nil** 的空 map（一张都不展示）。
	m := model.ParsePublicCards("")
	if m == nil {
		t.Fatal("空串必须解析为非 nil 空 map，否则'全部关掉'会退化成'全部打开'")
	}
	if len(m) != 0 {
		t.Errorf("空串应解析为空 map，得到 %v", m)
	}
}

// 卡片目录必须与 public.fields 的可收窄字段区分开，且键名不能撞上敏感词。
func TestPublicCardNamesAreSafe(t *testing.T) {
	names := model.PublicCardNames()
	if len(names) == 0 {
		t.Fatal("卡片目录为空")
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("卡片键重复：%s", n)
		}
		seen[n] = true
		for _, bad := range []string{"ip", "host", "token", "price", "cost"} {
			if strings.Contains(n, bad) {
				t.Errorf("卡片键 %q 含敏感词 %q", n, bad)
			}
		}
	}
	// 每张卡片都必须能通过校验（否则后台勾不出来）。
	for _, n := range names {
		if err := validatePublicCards(n); err != nil {
			t.Errorf("卡片键 %q 通不过校验：%v", n, err)
		}
	}
	// 月费合计卡必须在目录里——它是本轮新增的卡片，漏掉就再也关不掉了。
	found := false
	for _, n := range names {
		if n == "kpi_fee" {
			found = true
		}
	}
	if !found {
		t.Error("卡片目录应包含月费合计卡 kpi_fee")
	}
}
