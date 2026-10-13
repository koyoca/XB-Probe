package theme

import (
	"strings"
	"testing"
)

// 节点卡片"延迟 / 丢包"点阵的回归护栏。
//
// 背景（用户上报的三件事）：
//  1. 行标签显示的是运营组常量（电信/联通/移动/自定义），不是管理员起的
//     **目标名**——根因在后端按运营组聚合（同名同运营组的专属目标还会被
//     混成一行），前端只能拿到四个常量之一当标签；
//  2. 有的节点有专属目标、有的没有，行数随节点变化 → 卡片高度参差不齐。
//
// 前端侧的修复是两件事：标签优先用目标名（缺省才退回运营组常量）；
// 行数恒定（不足补占位行、超出块内滚动）。这里的护栏钉住这两件事
// 不被改回去——它们单独看都"只是显示问题"，一起决定卡片等高这一硬约束。
func TestLatencyBarsUseTargetName(t *testing.T) {
	src := readFrontend(t, "internal/theme/builtin/app.js")
	// 注意：funcBodyOf 的签名参数不要带前导缩进——缩进是它从签名行自己
	// 推导的，带上缩进会让推导出的 indent 为空、截取范围一路扩到顶层 }。
	body := funcBodyOf(t, src, "function latRow(")
	// 行标签优先目标名：b.name 缺省才退回运营组常量。
	if !strings.Contains(body, "b.name || CARRIER[b.carrier] || b.carrier") {
		t.Errorf("latRow 的行标签没有优先使用目标名（b.name），会退回显示「电信/自定义」这类运营组常量")
	}
	// 与详情页同一规则：名字里已写明运营商就不再缀后缀（同一个词两遍是噪声）。
	if !strings.Contains(body, "label.indexOf(carrier) < 0") {
		t.Errorf("latRow 没有做「名字已含运营商就不缀后缀」的判断，与详情页规则脱节")
	}
}

func TestLatencyBarsFixedRowCount(t *testing.T) {
	src := readFrontend(t, "internal/theme/builtin/app.js")
	render := funcBodyOf(t, src, "function renderLatencyBars(")
	// 行数常量必须存在且被用于补位循环。
	if !strings.Contains(src, "var LAT_ROWS = 4;") {
		t.Errorf("app.js 缺少 LAT_ROWS = 4 常量：行数会随节点拥有的目标数变化，卡片高度不再恒定")
	}
	if !strings.Contains(render, "i < LAT_ROWS") || !strings.Contains(render, "latPlaceholderRow(") {
		t.Errorf("renderLatencyBars 没有把行数补齐到 LAT_ROWS（占位行），卡片高度会参差不齐")
	}
	// 一条目标都没有时必须明确说"没有数据"，不能留白或整块隐藏。
	if !strings.Contains(render, `'lb-empty'`) {
		t.Errorf("renderLatencyBars 对无数据节点没有渲染「暂无探测数据」占位")
	}
	ph := funcBodyOf(t, src, "function latPlaceholderRow(")
	// 占位行必须与真实行同结构：两行灰点 + "—"，否则补位后行高不一致。
	if !strings.Contains(ph, "'lb-row lb-row-ph'") || strings.Count(ph, "emptyDots(") != 2 {
		t.Errorf("latPlaceholderRow 结构与真实行不一致（需要 lb-row-ph + 两行灰点）")
	}
}

func TestLatencyBarsCSSPinsHeight(t *testing.T) {
	css := readFrontend(t, "internal/theme/builtin/style.css")
	rule := cssRuleBody(t, css, ".n-lat")
	// 高度恒定 + 超出滚动：超过 4 条目标的目标多的节点在块内滚，
	// 所有卡片外框一致（等高卡片的硬约束）。
	if !strings.Contains(rule, "height: calc(") {
		t.Errorf(".n-lat 没有用 calc 钉住固定高度，卡片高度会随目标数量变化（规则：%s）", rule)
	}
	if !strings.Contains(rule, "overflow-y: auto") {
		t.Errorf(".n-lat 没有块内滚动：目标超过 4 条时会把卡片撑高")
	}
	// calc 依赖的行内各段高度必须显式钉住，否则算式与实际渲染脱节。
	title := cssRuleBody(t, css, ".lb-title")
	if !strings.Contains(title, "line-height: 13px") {
		t.Errorf(".lb-title 没有钉住 line-height: 13px，.n-lat 的 calc 高度算式会失准")
	}
}
