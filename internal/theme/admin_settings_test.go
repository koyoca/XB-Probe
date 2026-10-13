package theme

import (
	"strings"
	"testing"
)

// 后台"系统设置"页的两项可用性护栏。
//
// 用户报出的两个问题都不是功能错了，而是**界面让人无法确认功能对不对**：
//
// 其一，保存后的提示看不见。保存按钮在长表单的最底部（20+ 个字段、五个板块），
// 而提示是文档流里的横条、挂在 <main> 顶部。点完保存视线停在底部，
// 提示出现在视口之外 —— 操作者的结论是"点了保存没反应"，于是反复点击、
// 甚至怀疑保存失败。提示必须跟随**视口**，不能跟随文档流。
//
// 其二，设置项标签漏了中文。backup.enabled / backup.hour / backup.keep 三个键
// 在 SETTING_LABELS 里没有条目，buildSettingInput 走到 `var text = spec ? spec[0] : key`
// 的 else 分支，直接把原始键名当标签显示，还落进"其他"分组 ——
// 管理员看到的是三个英文键名，不知道各自是什么意思、取值范围是多少。
//
// 这两件事都无法在 HTTP 冒烟里观察（DOM 位置与渲染文本），所以放在源码级护栏里，
// 与 gap_test.go / node_cards_test.go 同一思路：不引入 Node 依赖，只盯住
// "两份资源里该有的结构还在不在"。

const (
	adminJS     = "internal/api/adminui/admin.js"
	adminHTML   = "internal/api/adminui/index.html"
	adminCSSRel = "internal/api/adminui/admin.css"
)

// 提示容器必须是 position: fixed，且不能被文档流里的祖先约束。
//
// 判据拆成三条，分别对应这一条修复的三个必要环节：
//  1. 容器存在（HTML 里 id="toast-wrap"）；
//  2. HTML 结构上它不在 <main> 里 —— 只要还在 <main> 里，
//     position:fixed 仍能生效（main 没有 transform/filter），
//     但会被 <main> 的 padding 与 max-width 约束定位上下文，且语义上又变回"页面内元素"。
//     更关键的是：<main> 有 padding，容器若在内部，left:0/right:0 就不是视口边缘。
//  3. CSS 里 .toast-wrap 是 fixed。
func TestAdminToastIsViewportPinned(t *testing.T) {
	html := readFrontend(t, adminHTML)
	css := readFrontend(t, adminCSSRel)

	if !strings.Contains(html, `id="toast-wrap"`) {
		t.Errorf("%s 缺少 id=\"toast-wrap\"：提示没有独立容器，只能退回文档流横条", adminHTML)
	}

	// 容器必须出现在 <main> 开始之前，即夹在 </header> 与 <main> 之间。
	//
	// 注意不能用 "出现位置早于 <main>" 这种朴素判据：注释里也会写 <main>
	// （本文件的注释就写了），那样会把说明文字当成标记。所以先剥掉 HTML 注释。
	stripped := stripHTMLComments(html)
	headerEnd := strings.Index(stripped, "</header>")
	mainOpen := strings.Index(stripped, "<main>")
	wrapAt := strings.Index(stripped, `id="toast-wrap"`)
	if headerEnd < 0 || mainOpen < 0 || wrapAt < 0 {
		t.Fatalf("%s 里找不到 </header> / <main> / toast-wrap，无法判定布局位置", adminHTML)
	}
	if wrapAt < headerEnd {
		t.Errorf("提示容器在 </header> 之前：应紧跟顶栏，作为其下方的浮动层")
	}
	if wrapAt > mainOpen {
		t.Errorf("提示容器在 <main> 内部（main@%d, wrap@%d）：应移到 <main> 之外，"+
			"否则 left/right:0 是相对带 padding 的 main 而不是视口", mainOpen, wrapAt)
	}

	if !strings.Contains(css, ".toast-wrap") {
		t.Fatalf("%s 缺少 .toast-wrap 样式", adminCSSRel)
	}
	block := cssRuleBody(t, css, ".toast-wrap")
	if !strings.Contains(block, "position: fixed") {
		t.Errorf(".toast-wrap 不是 position: fixed：提示会回到文档流，"+
			"在长表单底部点保存时仍然看不见。实际规则体：\n%s", block)
	}
	// 空白区域必须让点击穿透，否则这层"看不见的壳"会吃掉页面上半屏的交互。
	if !strings.Contains(block, "pointer-events: none") {
		t.Errorf(".toast-wrap 缺 pointer-events: none：容器空白区会拦截页面点击")
	}
	// 单条提示自身要能点（点击关闭），所以 .toast 必须把 pointer-events 收回来。
	toastBlock := cssRuleBody(t, css, ".toast")
	if !strings.Contains(toastBlock, "pointer-events: auto") {
		t.Errorf(".toast 缺 pointer-events: auto：父容器设了 none 后提示本身也点不动了")
	}
}

// 顶栏是 sticky 且 flex-wrap，高度会随换行变化，所以锚点必须实测而不是写死。
func TestAdminToastOffsetIsMeasured(t *testing.T) {
	js := readFrontend(t, adminJS)
	css := readFrontend(t, adminCSSRel)

	if !strings.Contains(js, "function syncToastOffset(") {
		t.Fatalf("%s 缺少 syncToastOffset()：顶栏高度变化后提示会盖住顶栏或悬空", adminJS)
	}
	// 必须用 offsetHeight 实测，而不是把 48 之类的常量写进 JS。
	body := funcBodyOf(t, js, "function syncToastOffset(")
	if !strings.Contains(body, "offsetHeight") {
		t.Errorf("syncToastOffset 没有读取 offsetHeight：顶栏换行与否高度不同（48px vs ~90px），"+
			"写死常量必然在一侧算错。函数体：\n%s", body)
	}
	if !strings.Contains(css, "var(--toast-top") {
		t.Errorf("%s 的 .toast-wrap 没有使用 --toast-top 变量：syncToastOffset 测出的值无处落地", adminCSSRel)
	}
	// 回退值只用于首帧（syncToastOffset 跑之前），所以 CSS 里必须有一个合理的默认。
	if !strings.Contains(css, "var(--toast-top,") {
		t.Errorf(".toast-wrap 的 --toast-top 没有回退值：脚本执行前提示会贴到页面顶端（y=0）")
	}
	// 窗口尺寸变化必须重测。
	if !strings.Contains(js, "addEventListener('resize', syncToastOffset)") {
		t.Errorf("%s 没有在 resize 时重测顶栏高度：缩放窗口后提示位置就错了", adminJS)
	}
}

// 提示必须支持多条共存，否则"保存成功"会被随后到达的警告顶掉。
func TestAdminToastSupportsMultipleMessages(t *testing.T) {
	js := readFrontend(t, adminJS)
	body := funcBodyOf(t, js, "function toast(")

	// 旧实现是取唯一元素、改 textContent、随后隐藏——一次只活一条。
	for _, bad := range []string{"t.textContent = msg", "toast._t"} {
		if strings.Contains(body, bad) {
			t.Errorf("toast() 仍是单条覆盖式（含 %q）：并发到达的两条消息会互相顶掉", bad)
		}
	}
	if !strings.Contains(body, "appendChild") {
		t.Errorf("toast() 没有把新提示追加进容器：多消息无法共存。函数体：\n%s", body)
	}
	// 成功与报错应分角色，读屏软件才能区分播报优先级。
	if !strings.Contains(body, "'alert'") || !strings.Contains(body, "'status'") {
		t.Errorf("toast() 没有按成功/报错区分 role：读屏时错误与成功提示无法区分")
	}
}

// 保存后必须把视口拉回表单顶部：顶部是锚点，视线也回到"我改了哪儿"。
func TestAdminSettingsSaveRestoresScroll(t *testing.T) {
	js := readFrontend(t, adminJS)

	idx := strings.Index(js, "$('btn-save-settings').addEventListener")
	if idx < 0 {
		t.Fatalf("%s 找不到保存设置按钮的绑定", adminJS)
	}
	// 取该绑定到下一个顶层 "$(" 绑定之间的片段。
	rest := js[idx:]
	end := strings.Index(rest[1:], "\n  $('")
	if end < 0 {
		t.Fatalf("无法界定保存按钮绑定代码块的结尾")
	}
	block := rest[:end+1]

	if !strings.Contains(block, "scrollIntoView") {
		t.Errorf("保存设置后没有 scrollIntoView：长表单底部点保存，视口停在底部，"+
			"看不出保存的是哪一段。代码块：\n%s", block)
	}
	// 顶栏是 sticky 的，直接滚到元素顶端会被顶栏压住，必须补偿顶栏高度。
	if !strings.Contains(block, "offsetHeight") {
		t.Errorf("保存后的滚动没有补偿 sticky 顶栏高度：目标区块顶部会被顶栏遮住")
	}
}

// 三个备份设置项必须有中文标签，且落进「备份」分组。
func TestAdminBackupSettingsHaveChineseLabels(t *testing.T) {
	js := readFrontend(t, adminJS)

	keys := []string{"backup.enabled", "backup.hour", "backup.keep"}
	for _, k := range keys {
		// 判据：'key': ['中文名', ... —— 值必须以引号开头的中文标签开头，
		// 而不是 null（=不显示）或缺条目（=退回显示原始英文键名）。
		marker := "'" + k + "': ["
		at := strings.Index(js, marker)
		if at < 0 {
			t.Errorf("%s 的 SETTING_LABELS 缺少 %q：设置页会把英文键名当标签显示", adminJS, k)
			continue
		}
		rest := js[at+len(marker):]
		rest = strings.TrimLeft(rest, " \t")
		if strings.HasPrefix(rest, "null") {
			t.Errorf("%q 被标成 null（不显示）：它必须可配置", k)
			continue
		}
		if !strings.HasPrefix(rest, "'") {
			t.Errorf("%q 的标签不是字符串字面量，实际起始：%.40s", k, rest)
			continue
		}
		label := rest[1 : strings.Index(rest[1:], "'")+1]
		if label == k {
			t.Errorf("%q 的标签就是键名本身：等于没有中文名", k)
		}
		if !hasHan(label) {
			t.Errorf("%q 的标签 %q 不含汉字：管理员看到的仍是英文键名", k, label)
		}
	}

	// 三个键必须出现在同一分组里，否则它们会散落到"其他"分组。
	gi := strings.Index(js, "var SETTING_GROUPS")
	if gi < 0 {
		t.Fatalf("%s 找不到 SETTING_GROUPS", adminJS)
	}
	groups := js[gi:]
	ge := strings.Index(groups, "];")
	if ge < 0 {
		t.Fatalf("无法界定 SETTING_GROUPS 数组")
	}
	groups = groups[:ge]
	for _, k := range keys {
		if !strings.Contains(groups, "'"+k+"'") {
			t.Errorf("SETTING_GROUPS 里没有 %q：它会落进兜底的「其他」分组", k)
		}
	}
	if !strings.Contains(groups, "['备份',") {
		t.Errorf("SETTING_GROUPS 缺少「备份」分组标题")
	}
}

// 设置页正文不应再出现裸露的英文设置键名。
//
// 这是缺陷的**症状**而不是成因：只要某个键在 SETTING_LABELS 里漏了，
// buildSettingInput 就会把原始键名当标签渲染出来。与其逐个键写断言，
// 不如直接盯住"这页面上不该出现 xxx.yyy 形态的标识符"这一条外观契约。
func TestAdminSettingsPageHasNoRawKeyLabels(t *testing.T) {
	html := readFrontend(t, adminHTML)

	// index.html 里备份说明曾写 `backup.enabled` / `backup.hour` / `backup.keep`。
	// <code> 标签包着的技术标识符在"面向管理员的散文说明"里同样是噪音。
	for _, k := range []string{"backup.enabled", "backup.hour", "backup.keep"} {
		if strings.Contains(html, k) {
			t.Errorf("%s 正文仍出现 %q：说明文字应使用设置项的中文名", adminHTML, k)
		}
	}
}

// 后台备份状态卡的悬浮说明同样不该指向英文键名。
func TestAdminBackupCardTipsUseChineseNames(t *testing.T) {
	js := readFrontend(t, adminJS)

	at := strings.Index(js, "function loadBackups(")
	if at < 0 {
		t.Fatalf("%s 找不到 loadBackups()", adminJS)
	}
	body := js[at:]
	if end := strings.Index(body, "\n  $('btn-refresh-backups')"); end > 0 {
		body = body[:end]
	}
	for _, k := range []string{"backup.enabled", "backup.hour", "backup.keep"} {
		if strings.Contains(body, k) {
			t.Errorf("备份状态卡的提示里仍出现 %q：应改为设置项中文名，"+
				"否则管理员在设置页找不到对应字段", k)
		}
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// cssRuleBody 取出某个选择器的规则体（第一个 { 到匹配前的最后一个 }）。
// 只处理扁平的规则，本文件用到的都是这种。
func cssRuleBody(t *testing.T, css, sel string) string {
	t.Helper()
	at := strings.Index(css, sel+" {")
	if at < 0 {
		// 允许 .a, .b { 这类选择器组。
		at = strings.Index(css, sel)
	}
	if at < 0 {
		t.Fatalf("CSS 里找不到选择器 %q", sel)
	}
	open := strings.Index(css[at:], "{")
	closeAt := strings.Index(css[at:], "}")
	if open < 0 || closeAt < 0 || closeAt < open {
		t.Fatalf("选择器 %q 的规则体不完整", sel)
	}
	return css[at+open : at+closeAt]
}

// funcBodyOf 取出以 sig 开头、到行首的单个 "}" 结束的函数体。
// 只适用于本文件里那些不含嵌套块级语法的辅助函数？不——toast 里有 if，
// 所以这里按**缩进**判定结束：签名行的缩进深度决定函数的右花括号缩进。
func funcBodyOf(t *testing.T, src, sig string) string {
	t.Helper()
	at := strings.Index(src, sig)
	if at < 0 {
		t.Fatalf("找不到 %q", sig)
	}
	// 取签名所在行的缩进。
	lineStart := strings.LastIndex(src[:at], "\n") + 1
	indent := src[lineStart:at]
	closer := "\n" + indent + "}"
	end := strings.Index(src[at:], closer)
	if end < 0 {
		t.Fatalf("%q 的函数体找不到结束位置", sig)
	}
	return src[at : at+end]
}

// stripHTMLComments 去掉 <!-- ... --> 注释。
//
// 做标记位置比较前必须剥注释：本项目的 HTML 里注释写得很密（成篇解释"为什么
// 必须这么写"），注释中出现的 <main> 会把朴素的下标比较带偏。
// 未闭合的注释直接截断到结尾（HTML 语法如此）。
func stripHTMLComments(s string) string {
	var b strings.Builder
	rest := s
	for {
		at := strings.Index(rest, "<!--")
		if at < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:at])
		closeAt := strings.Index(rest[at:], "-->")
		if closeAt < 0 {
			return b.String()
		}
		rest = rest[at+closeAt+3:]
	}
}

// hasHan 判断字符串里是否含汉字（U+4E00–U+9FFF）。
func hasHan(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// 站点形态的三个设置项必须有中文标签，且落进「站点形态」分组。
//
// 与备份那组同理：漏了标签，设置页会把 site.layout 这样的英文键名直接当标题
// 渲染出来；漏了分组，它会掉进兜底的「其他」分组——而这三项是"配错就把自己
// 锁在后台外面"的开关，不该藏在角落里。
func TestAdminSiteLayoutSettingsHaveChineseLabels(t *testing.T) {
	js := readFrontend(t, adminJS)

	keys := []string{"site.layout", "site.public_hosts", "site.admin_hosts"}
	for _, k := range keys {
		marker := "'" + k + "': ["
		at := strings.Index(js, marker)
		if at < 0 {
			t.Errorf("%s 的 SETTING_LABELS 缺少 %q：设置页会把英文键名当标签显示", adminJS, k)
			continue
		}
		rest := strings.TrimLeft(js[at+len(marker):], " \t")
		if strings.HasPrefix(rest, "null") {
			t.Errorf("%q 被标成 null（不显示）：它必须可配置", k)
			continue
		}
		if !strings.HasPrefix(rest, "'") {
			t.Errorf("%q 的标签不是字符串字面量，实际起始：%.40s", k, rest)
			continue
		}
		label := rest[1 : strings.Index(rest[1:], "'")+1]
		if !hasHan(label) {
			t.Errorf("%q 的标签 %q 不含汉字：管理员看到的仍是英文键名", k, label)
		}
	}

	gi := strings.Index(js, "var SETTING_GROUPS")
	if gi < 0 {
		t.Fatalf("%s 找不到 SETTING_GROUPS", adminJS)
	}
	groups := js[gi:]
	ge := strings.Index(groups, "];")
	if ge < 0 {
		t.Fatalf("无法界定 SETTING_GROUPS 数组")
	}
	groups = groups[:ge]
	for _, k := range keys {
		if !strings.Contains(groups, "'"+k+"'") {
			t.Errorf("SETTING_GROUPS 里没有 %q：它会落进兜底的「其他」分组", k)
		}
	}
	if !strings.Contains(groups, "['站点形态',") {
		t.Errorf("SETTING_GROUPS 缺少「站点形态」分组标题")
	}
}
