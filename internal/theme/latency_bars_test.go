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
// 行数与目标数一致（有几条画几行，**不补占位行**）。卡片高度的处理后来又
// 反转过一次（见 TestLatencyBarsCSSCapsHeight）：等高换成 max-height 封顶、
// 内容自适应。这里的护栏钉住这几件事不被改回去——它们单独看都"只是显示问题"。
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
//	新做法：有几条目标画几行；块高由 CSS 的 max-height 封顶（内容自适应，
//	        见 TestLatencyBarsCSSCapsHeight），不再补假数据。
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

	// 3) 一条目标都没有时**整块收起**（连标题一起不画）。
	//
	//    2026-10-15 按用户要求反转了早先的做法（当时画一句"暂无探测数据"）：
	//    "当探测目标为无时，节点卡片底下会显示 延迟 / 丢包 · 近 30 分钟 暂无探测数据
	//     之类的，占用了卡片行数并且也不好看"。
	//    这条与上面并不矛盾：空列表是"这个节点根本没有探测目标"（没有信息量，
	//    所以不显示），而占位行伪造的是"有的目标挂掉了"（错误信息，更不能显示）。
	if strings.Contains(render, `'lb-empty'`) {
		t.Errorf("renderLatencyBars 又给「没有探测目标」的节点画占位文案了：整块应当收起")
	}
	if !strings.Contains(render, "!list.length") || !strings.Contains(render, "box.hidden = true") {
		t.Errorf("renderLatencyBars 没有在「没有探测目标」时收起整块")
	}
}

// TestLatencyBarsCSSCapsHeight 钉住"上限封顶、内容自适应"这个约束（真机反馈后**再次反转**）。
//
//	旧做法：.n-lat 用 height 定高 4 行 + 网格 grid-auto-rows:1fr 强制等高——
//	        等高是保住了，但探测目标只有 1~3 条的节点底部空出一大块
//	        （卡片是固定大小的，目标越少空白越多）。
//	新做法：.n-lat 用 **max-height** 封顶（块随实际行数收缩，超过 4 条在块内滚），
//	        网格 align-items:start 让卡片高度随内容——同一行卡片底边可以不齐，
//	        这是"不留空白"必然的代价，不再视为缺陷。
func TestLatencyBarsCSSCapsHeight(t *testing.T) {
	css := readFrontend(t, "internal/theme/builtin/style.css")
	rule := cssRuleBody(t, css, ".n-lat")
	// 4 行封顶 + 超出滚动：目标多于 4 条的节点在块内滚，块本身不会被无限撑高。
	if !strings.Contains(rule, "max-height: calc(") {
		t.Errorf(".n-lat 没有用 max-height:calc 封顶（规则：%s）", rule)
	}
	if strings.Contains(rule, "height: calc(") && !strings.Contains(rule, "max-height: calc(") {
		t.Errorf(".n-lat 又回到了 height 定高：目标 1~3 条时卡片底部会空出一大块（规则：%s）", rule)
	}
	if !strings.Contains(rule, "overflow-y: auto") {
		t.Errorf(".n-lat 没有块内滚动：目标超过 4 条时会把卡片撑高")
	}
	// calc 依赖的行内各段高度必须显式钉住，否则算式与实际渲染脱节。
	title := cssRuleBody(t, css, ".lb-title")
	if !strings.Contains(title, "line-height: 13px") {
		t.Errorf(".lb-title 没有钉住 line-height: 13px，.n-lat 的 calc 算式会失准")
	}
	// 网格不再强制等高：grid-auto-rows:1fr + align-items:stretch 是"空白"的另一半来源。
	nodes := cssRuleBody(t, css, ".nodes")
	if strings.Contains(nodes, "grid-auto-rows: 1fr") || strings.Contains(nodes, "align-items: stretch") {
		t.Errorf(".nodes 又回到了强制等高（规则：%s）——\n"+
			"  目标少的节点会被拉伸到同最高者一样，底部整块留白。", nodes)
	}
	if !strings.Contains(nodes, "align-items: start") {
		t.Errorf(".nodes 缺少 align-items: start：卡片高度不会随内容收缩（规则：%s）", nodes)
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
