package alert

import (
	"strings"
	"testing"
)

// TestEveryDefaultRuleKindHasAChineseLabel 钉住"新增一种告警规则就必须同时给中文名"。
//
// 判据取自 DefaultRules() 的键集合，而不是在测试里抄一份清单：
// 抄一份的话，将来加了新规则类型、映射忘了补，测试照样全绿 ——
// 而管理员在后台看到的会是一个英文键名（正是本次要修的那个毛病）。
func TestEveryDefaultRuleKindHasAChineseLabel(t *testing.T) {
	for kind := range DefaultRules() {
		label := KindLabel(kind)
		if _, ok := kindLabels[kind]; !ok {
			t.Errorf("规则类型 %q 没有中文名：后台的「类型」列会直接显示这个英文键", kind)
		}
		if label == "" {
			t.Errorf("KindLabel(%q) 返回空串", kind)
		}
		if strings.Contains(label, kind) && !strings.Contains(label, "（") {
			t.Errorf("KindLabel(%q) 原样返回了键名（%q）：中文名没配上", kind, label)
		}
	}
}

// TestKindLabelTranslatesCompositeStateKeys 钉住带参数的复合键。
//
// 事件落库的是**状态键**：复合类型的形态是 `<基础名>:<参数>`，
// 参数的含义各不相同（traffic 是百分比档位、expire 是剩余天数、probe_fail 是目标 ID）。
// 只翻译基础名、把参数丢掉（"流量梯级"三个字）等于还是不告诉用户是哪一档。
func TestKindLabelTranslatesCompositeStateKeys(t *testing.T) {
	cases := map[string]string{
		"offline":       "离线",
		"cpu":           "CPU 越阈值",
		"traffic:80":    "流量梯级（已用 ≥ 80%）",
		"traffic:100":   "流量梯级（已用 ≥ 100%）",
		"expire:30":     "到期提醒（剩余 ≤ 30 天）",
		"expire:0":      "到期提醒（今天到期）",
		"probe_fail:12": "探测连续失败（目标 #12）",
	}
	for kind, want := range cases {
		if got := KindLabel(kind); got != want {
			t.Errorf("KindLabel(%q) = %q，期望 %q", kind, got, want)
		}
	}
}

// TestKindLabelNeverFabricatesUnknownKinds 钉住"认不出就原样返回"。
//
// 另一条路是"猜一个中文出来"：将来新增复合键时，后台会显示一句编造的说明，
// 比显示一个看不懂的 `newkind:7` 更坏 —— 后者至少能被搜到、被贴进 issue。
func TestKindLabelNeverFabricatesUnknownKinds(t *testing.T) {
	for _, kind := range []string{"", "newkind", "newkind:7", "unknown:1", "traffic:abc"} {
		got := KindLabel(kind)
		switch kind {
		case "traffic:abc":
			// 已知基础名 + 无法解释的参数：保留参数，不假装它是百分比。
			if !strings.Contains(got, "abc") {
				t.Errorf("KindLabel(%q) = %q：把无法解释的参数丢掉了", kind, got)
			}
		default:
			if got != kind {
				t.Errorf("KindLabel(%q) = %q，期望原样返回", kind, got)
			}
		}
	}
}
