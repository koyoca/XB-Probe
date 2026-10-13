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
// 行数与目标数一致（有几条画几行，**不补占位行**）。卡片等高由 CSS 的固定
// 块高保证，而不是靠在 JS 里补出几行假数据。这里的护栏钉住这两件事
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

// TestLatencyBarsNeverFabricatePlaceholderRows 钉住"只渲染真实存在的探测目标"。
//
// 这里钉的是与上一版**相反**的结论，所以两边的判据都要写清楚：
//
//	旧做法：行数恒定 4，不足的补占位行（全灰点 + 三个 "—"）。
//	        目的是让同一行网格里的卡片等高。
//	问题：  占位行与真实行**长得一模一样**。"只有 1 条探测目标"的节点因此画成
//	        "1 条有数据 + 3 条全挂"——而后者正是需要立刻处理的故障形态，
//	        被凭空造出来了。用户看到的是一张"三个目标全挂"的卡片。
//	新做法：有几条目标画几行；等高改由 CSS 的固定块高保证（空白就老实空着）。
//
// 判据挑的是"补位那两样东西在不在"：只断言"渲染了 list.forEach"的话，
// 既补占位行又渲染真行的实现同样是绿的。
func TestLatencyBarsNeverFabricatePlaceholderRows(t *testing.T) {
	src := readFrontend(t, "internal/theme/builtin/app.js")

	// 1) 补位机制整体不存在（函数都删掉了，不是"有函数但没调用"）。
	for _, dead := range []string{"latPlaceholderRow", "emptyDots", "var LAT_ROWS"} {
		if strings.Contains(src, dead) {
			t.Errorf("app.js 里还有 %q：按实际目标数渲染就不会有这些东西，\n"+
				"  留着它只会让下一个人以为「行数要保持恒定」仍然是约定。", dead)
		}
	}

	// 2) 渲染只遍历真实目标。
	render := funcBodyOf(t, src, "function renderLatencyBars(")
	if !strings.Contains(render, "list.forEach(") {
		t.Errorf("renderLatencyBars 没有直接按实际目标列表渲染行（render 体：%s）", render)
	}

	// 3) 一条目标都没有时必须明确说"没有数据"，不能留白或整块隐藏。
	//    这条与上面并不矛盾：空列表是"确实没有目标"，而占位行伪造的是"有的目标挂掉了"。
	if !strings.Contains(render, `'lb-empty'`) {
		t.Errorf("renderLatencyBars 对无数据节点没有渲染「暂无探测数据」占位")
	}
}

// TestLatencyBarsCSSStillPinsHeight 钉住"等高"这个真正的约束没有被上面那次改动弄丢。
//
// 去掉占位行之后，卡片等高**只剩** CSS 固定块高这一条保障了：一旦有人把
// .n-lat 的 height 改成 auto 或删掉，卡片会立刻参差不齐，而且不会有任何报错。
func TestLatencyBarsCSSPinsHeight(t *testing.T) {
	css := readFrontend(t, "internal/theme/builtin/style.css")
	rule := cssRuleBody(t, css, ".n-lat")
	// 高度恒定 + 超出滚动：目标多于 4 条的节点在块内滚，所有卡片外框一致。
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

// TestLatencyBarsNoOrphanPlaceholderCSS 钉住样式表里不留占位行的孤儿规则。
//
// .lb-row-ph 是给占位行用的类名。占位行删掉之后它就是死规则 ——
// 而"删了 JS 忘了删 CSS"这种残留会让下一个人以为占位行还在用。
func TestLatencyBarsNoOrphanPlaceholderCSS(t *testing.T) {
	css := readFrontend(t, "internal/theme/builtin/style.css")
	if strings.Contains(css, ".lb-row-ph") {
		t.Error("style.css 里还剩 .lb-row-ph 规则：占位行已经删掉，这是一条孤儿样式")
	}
}
