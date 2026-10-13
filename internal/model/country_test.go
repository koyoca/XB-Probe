package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// 国家代码的规范化与校验。
//
// 这个值的三个入口（管理端 PATCH、后台表单、导入脚本）如果各自实现一遍
// 大小写/长度判断，迟早会出现"一个入口存了 hk、另一个存了 HK"——
// 前端按表查旗，大小写不同就查不到，表现为"一半节点没有国旗"，
// 而数据里两边都是普通字符串，肉眼根本看不出问题。
func TestNormalizeCountry(t *testing.T) {
	cases := map[string]string{
		"HK":      "HK", // 标准写法
		"hk":      "HK", // 小写要归一
		" hk ":    "HK", // 前后空白要吃掉
		"Jp":      "JP",
		"":        "", // 未设置
		"   ":     "", // 只有空白 = 未设置
		"H":       "", // 太短
		"HKG":     "", // 太长
		"H1":      "", // 含数字
		"中国":      "", // 非 ASCII
		"CHINA":   "", // 国名而不是代码
		"h k":     "", // 中间有空白
		"HK-01":   "", // 机房编号不是国家代码
		"e\u0301": "", // 带组合字符
	}
	for in, want := range cases {
		if got := NormalizeCountry(in); got != want {
			t.Errorf("NormalizeCountry(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestIsValidCountryCode(t *testing.T) {
	for _, s := range []string{"HK", "CN", "US", "ZZ", "AA"} {
		if !IsValidCountryCode(s) {
			t.Errorf("IsValidCountryCode(%q) 应为 true", s)
		}
	}
	for _, s := range []string{"", "h", "hk", "HKG", "H1", "-", "中"} {
		if IsValidCountryCode(s) {
			t.Errorf("IsValidCountryCode(%q) 应为 false", s)
		}
	}
}

// 公开 DTO 允许出现 country，但绝不允许出现 location/provider/note 这类
// 管理端自由文本——那三个恰好是 dto_leak_test 的禁词，这里再钉一次，
// 防止有人"顺手"把 location 也发到公开接口上（画国旗不需要它）。
func TestPublicCountryFieldIsCodeNotFreeText(t *testing.T) {
	p := PublicNode{Country: "HK", PublicID: "p", Name: "香港-01"}
	rawb, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	raw := string(rawb)
	if !strings.Contains(raw, `"country":"HK"`) {
		t.Errorf("公开节点应带 country 代码：%s", raw)
	}
	for _, bad := range []string{"location", "provider", `"note"`} {
		if strings.Contains(raw, bad) {
			t.Errorf("公开节点不应出现 %q：%s", bad, raw)
		}
	}
}
