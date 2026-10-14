package theme

import (
	"strings"
	"testing"
)

// 本文件钉住 2026-10-12 的三件事。它们都是"用户看得见、但测试看不见"的改动，
// 因此必须用源码级断言固定下来（每条都做过变异验证）：
//
//  1. 顶栏：主题切换从文字按钮「深色」换成图标按钮，站名/时间/图标顺序与对齐固定，
//     未登录也要有一个通往管理后台的图标入口；
//  2. 后台 UI 与登录页**换装成与公开看板同一套黑白灰阶**——这推翻了设计文档 5.2
//     早期"后台固定配色"的设定，两边的调色板必须逐项一致（暗色块逐行一致）；
//  3. 节点卡片新增"续费价格"行（含展示开关）与点阵时间窗（30m/1h/6h/24h）。

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// elementAt 取出 html 里包含 anchor 的那个**完整元素**（开始标签到配对结束标签）。
//
// 必须取整个元素而不是"锚点附近的若干字符"：判断"图标在按钮**内部**"时，
// 一段固定长度的窗口会把相邻的按钮/链接一起框进去，于是把图标挪到
// 另一个按钮里也照样通过——这正是不变量 20 说的"守卫冗余"。
//
// 朴素行内配对：回退到最近的 '<' 取标签名，再按同名标签数深度，
// 忽略自闭合标签。本项目的 HTML 不嵌同类标签，够用。
func elementAt(t *testing.T, html, anchor string) string {
	t.Helper()
	at := strings.Index(html, anchor)
	if at < 0 {
		t.Fatalf("HTML 里找不到锚点 %q", anchor)
	}
	open := strings.LastIndex(html[:at], "<")
	if open < 0 {
		t.Fatalf("锚点 %q 之前没有标签起始", anchor)
	}
	rest := html[open+1:]
	nameEnd := strings.IndexAny(rest, " \t\r\n/>")
	if nameEnd <= 0 {
		t.Fatalf("锚点 %q 所在的标签名解析失败：%q", anchor, rest[:min(20, len(rest))])
	}
	name := rest[:nameEnd]
	if name == "!--" {
		t.Fatalf("锚点 %q 落在注释里：%q", anchor, html[open:open+40])
	}
	depth := 0
	for i := open; i < len(html); {
		lt := strings.Index(html[i:], "<")
		if lt < 0 {
			break
		}
		lt += i
		tail := html[lt+1:]
		if strings.HasPrefix(tail, "/"+name) {
			depth--
			if depth == 0 {
				gt := strings.Index(html[lt:], ">")
				if gt < 0 {
					t.Fatalf("<%s> 的结束标签未闭合", name)
				}
				return html[open : lt+gt+1]
			}
			i = lt + 1
			continue
		}
		if strings.HasPrefix(tail, name) && len(tail) > len(name) &&
			strings.ContainsAny(tail[len(name):len(name)+1], " \t\r\n/>") {
			gt := strings.Index(html[lt:], ">")
			if gt < 0 {
				t.Fatalf("<%s> 的开始标签未闭合", name)
			}
			if !strings.HasSuffix(html[lt:lt+gt], "/") {
				depth++
			}
			i = lt + gt + 1
			continue
		}
		i = lt + 1
	}
	t.Fatalf("锚点 %q 所在的 <%s> 没有配对结束标签", anchor, name)
	return ""
}

// stripSVG 去掉所有 <svg>…</svg> 段（含自闭合的 <svg/>）。
// 判断"按钮里有没有可见文字"时要先剥掉图标：图标里没有文本节点，
// 但它的 path 数据很长，留着会掩盖真正想知道的东西。
func stripSVG(s string) string {
	var b strings.Builder
	for {
		at := strings.Index(s, "<svg")
		if at < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:at])
		rest := s[at:]
		end := strings.Index(rest, "</svg>")
		if end < 0 {
			// 自闭合或未闭合：丢掉到最后一个 '>' 为止。
			gt := strings.LastIndex(rest, ">")
			if gt < 0 {
				return b.String()
			}
			s = rest[gt+1:]
			continue
		}
		s = rest[end+len("</svg>"):]
	}
}

// stripTags 去掉所有 <...> 标签，只留文本节点。
func stripTags(s string) string {
	var b strings.Builder
	for {
		lt := strings.Index(s, "<")
		if lt < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:lt])
		gt := strings.Index(s[lt:], ">")
		if gt < 0 {
			return b.String()
		}
		s = s[lt+gt+1:]
	}
}

// ---------------------------------------------------------------------------
// 1. 顶栏
// ---------------------------------------------------------------------------

// TestTopbarUsesIconButtons 主题切换与后台入口都必须是图标按钮，且主题切换
// **不能**再退化成文字按钮（用户明确要求把「深色」换成图标并融入整体）。
//
// 反过来说：只断言"存在图标"不够——真正的缺陷形态是有人又把按钮改回
// `<button class="tgl">深色</button>`，那时图标还在（CSS 里），文字按钮也在。
// 所以这里额外断言按钮内部只有 SVG。
func TestTopbarUsesIconButtons(t *testing.T) {
	html := stripHTMLComments(readFrontend(t, "internal/theme/builtin/index.html"))
	// 图标按钮组
	if !strings.Contains(html, `class="topbar-actions"`) {
		t.Fatal("顶栏缺少 .topbar-actions 容器：站名/时间/图标按钮没有分组，flex 对齐会散")
	}
	// 主题切换：必须是 .icon-btn.tgl，且内部同时有月亮与太阳两个图标
	// （显隐交给 CSS，JS 不碰 DOM 图标）。
	tgl := elementAt(t, html, `id="btn-theme"`)
	for _, ico := range []string{`class="ico-moon"`, `class="ico-sun"`} {
		if !strings.Contains(tgl, ico) {
			t.Errorf("主题切换按钮里必须有 %s（另一个由 CSS 按 data-theme 隐藏）", ico)
		}
	}
	if !strings.Contains(tgl, `class="icon-btn tgl"`) {
		t.Errorf("主题切换按钮应同时带 .icon-btn 与 .tgl，实得：%s", tgl)
	}
	// 按钮里不得再有**可见文字**：文字按钮正是这次要换掉的东西。
	// 注意不能直接搜 "深色"——aria-label/title 里写着"切换深浅色"是必须的，
	// 而"切换深浅色"里恰好含子串"浅色"。所以剥掉全部标签后只看剩下的文本。
	if txt := strings.TrimSpace(stripTags(stripSVG(tgl))); txt != "" {
		t.Errorf("主题切换按钮里不得有可见文字（应只有图标，文字放 aria-label/title），残留文本：%q", txt)
	}
	// 而 aria-label 必须还在：纯图标按钮对读屏用户等于不可用。
	if !strings.Contains(tgl, "aria-label=") {
		t.Error("纯图标按钮必须保留 aria-label，否则读屏用户不知道它是干什么的")
	}
	// 未登录也要有登录入口：一个指向 /admin/ 的图标链接。
	login := elementAt(t, html, `id="btn-login"`)
	if !strings.Contains(login, `class="icon-btn"`) || !strings.Contains(login, `href="/admin/"`) {
		t.Errorf("未登录时的登录图标应是 <a class=\"icon-btn\" href=\"/admin/\">，实得：%s", login)
	}
	if !strings.Contains(login, "<svg") {
		t.Errorf("登录入口必须是图标（用户要求未登录时有个登录图标），实得：%s", login)
	}
}

// TestTopbarTitleComesFirst 站名必须在 DOM 里排在时间与图标**之前**。
// 早期把 <h1> 写在按钮组之后，屏幕上就成了"更新于… [月亮] XB-Probe"，
// 站名落在最右、像是按钮的附属；靠 flex order 兜回来只是把问题藏起来。
func TestTopbarTitleComesFirst(t *testing.T) {
	html := stripHTMLComments(readFrontend(t, "internal/theme/builtin/index.html"))
	group := elementAt(t, html, `class="topbar-right"`)
	title := strings.Index(group, `id="site-title"`)
	updated := strings.Index(group, `id="updated"`)
	actions := strings.Index(group, `class="topbar-actions"`)
	if title < 0 || updated < 0 || actions < 0 {
		t.Fatalf("顶栏右侧三件套不齐：title=%d updated=%d actions=%d", title, updated, actions)
	}
	if !(title < updated && updated < actions) {
		t.Errorf("顶栏右侧顺序应为 站名 → 更新时间 → 图标按钮组，实得下标 title=%d updated=%d actions=%d",
			title, updated, actions)
	}
}

// TestTopbarIsVerticallyCentered 右侧一组必须垂直居中。
// 早期用 align-items: baseline，文本与带边框的按钮各按自己的基线落位，
// 站名/时间/按钮三者高高低低——正是用户说的"字体位置不同"。
func TestTopbarIsVerticallyCentered(t *testing.T) {
	css := readFrontend(t, "internal/theme/builtin/style.css")
	rule := cssRuleBody(t, css, ".topbar-right")
	if !strings.Contains(rule, "align-items: center") {
		t.Errorf(".topbar-right 必须 align-items: center（baseline 会让文字与按钮错位），实得：%s", rule)
	}
	if strings.Contains(rule, "align-items: baseline") {
		t.Error(".topbar-right 又用回了 baseline：文字与图标按钮不会落在同一条水平线上")
	}
	// 等宽数字：更新时间每秒跳动，非等宽会让整组左右抖。
	if muted := cssRuleBody(t, css, ".topbar-right .muted"); !strings.Contains(muted, "tabular-nums") {
		t.Errorf("更新时间应使用 tabular-nums，否则秒数跳动时整组会左右抖，实得：%s", muted)
	}
	// 图标按钮的尺寸/描边只写一次，两个按钮共用；分散写会漂移。
	// 颜色不写字面色值，而是给 .icon-btn 一个 color，让 SVG 的
	// stroke="currentColor" 继承——这样 hover / 深色模式都无需另写规则。
	btn := cssRuleBody(t, css, ".icon-btn")
	for _, want := range []string{"width: 28px", "height: 28px", "color: var(--muted-2)"} {
		if !strings.Contains(btn, want) {
			t.Errorf(".icon-btn 缺少 %q：两个图标按钮的尺寸/着色会各自漂移", want)
		}
	}
	if !strings.Contains(cssRuleBody(t, css, ".icon-btn:hover"), "color: var(--fg)") {
		t.Error(".icon-btn:hover 应变重墨（color: var(--fg)）：纯描边图标没有别的 hover 反馈")
	}
	// 图标本身必须用 currentColor 描边，否则上面那条 color 毫无作用、
	// 且暗色模式下图标会保持一个固定颜色（在深底上看不清）。
	html := stripHTMLComments(readFrontend(t, "internal/theme/builtin/index.html"))
	if n := strings.Count(elementAt(t, html, `id="btn-theme"`), `stroke="currentColor"`); n != 2 {
		t.Errorf("太阳/月亮两个图标都要用 stroke=\"currentColor\" 描边，实得 %d 处", n)
	}
}

// TestThemeIconSwapIsCSSOnly 太阳/月亮的显隐必须完全由 CSS 完成。
// 让 JS 去切 DOM（隐藏一个 <svg>）的话，三种来源会漏：
// 跟随系统（无 data-theme）、?theme= 覆盖、手动切换——漏一个就是"图标和主题对不上"。
func TestThemeIconSwapIsCSSOnly(t *testing.T) {
	for _, rel := range []string{
		"internal/theme/builtin/style.css",
		"internal/api/adminui/admin.css",
	} {
		css := readFrontend(t, rel)
		for _, want := range []string{
			".tgl .ico-sun { display: none; }",
			".tgl .ico-moon { display: block; }",
			`:root[data-theme="dark"] .tgl .ico-sun { display: block; }`,
			`:root[data-theme="dark"] .tgl .ico-moon { display: none; }`,
			`:root:not([data-theme="light"]) .tgl .ico-sun { display: block; }`,
			`:root:not([data-theme="light"]) .tgl .ico-moon { display: none; }`,
		} {
			if !strings.Contains(css, want) {
				t.Errorf("%s 缺少图标切换规则 %q：某个主题来源下图标会与主题不符", rel, want)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 2. 后台换装：与公开看板同一套灰阶
// ---------------------------------------------------------------------------

// unifiedVars 是两边必须逐项同值的调色板变量（顺序即断言顺序）。
// 后台多出来的 --panel / --accent-fg 是别名，见 TestAdminPanelAliasesCard。
var unifiedVars = []string{
	"--bg", "--fg", "--muted", "--muted-2", "--card", "--line", "--line-strong",
	"--c1", "--c2", "--c3", "--c4", "--c5", "--online", "--offline", "--accent",
	"--ok", "--warn", "--bad", "--chip", "--track", "--dir-down", "--dir-up",
}

// varsOf 抽出某个选择器块里的 `--x: value;` 声明（去注释、去空白）。
func varsOf(t *testing.T, css, anchor string) map[string]string {
	t.Helper()
	body := cssRuleBody(t, css, anchor)
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "/*"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if !strings.HasPrefix(line, "--") || !strings.HasSuffix(line, ";") {
			continue
		}
		line = strings.TrimSuffix(line, ";")
		kv := strings.SplitN(line, ":", 2)
		if len(kv) != 2 {
			continue
		}
		out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}
	return out
}

// TestAdminSharesPublicPalette 后台与公开看板的调色板必须一致。
//
// 这是 2026-10-12 的用户决策（"后台 UI 也要更改，整体风格采用跟公开看板一样的风格"），
// 它推翻了设计文档 5.2 早期"后台固定配色、不随公开主题变化"的设定。
// 断言的是**取值相等**而不是"两边都有这些变量"——后者在各自保留一套
// 彩色/灰阶配色时照样通过，正是这次要禁止的状态。
func TestAdminSharesPublicPalette(t *testing.T) {
	pub := varsOf(t, readFrontend(t, "internal/theme/builtin/style.css"), ":root")
	adm := varsOf(t, readFrontend(t, "internal/api/adminui/admin.css"), ":root")
	for _, v := range unifiedVars {
		p, a := pub[v], adm[v]
		if p == "" || a == "" {
			t.Errorf("两边都必须定义 %s（公开看板=%q 后台=%q）——少一边就会出现「只有一边是灰阶」的残留", v, p, a)
			continue
		}
		if p != a {
			t.Errorf("%s 两边不一致：公开看板 %q / 后台 %q——后台必须与公开看板同一套灰阶", v, p, a)
		}
	}
}

// TestAdminPanelAliasesCard 后台独有的两个变量必须是**别名**，不能另立色值。
//
// --panel 是后台的历史命名（等价于公开看板的 --card），--accent-fg 是反白按钮
// 上的文字（等价于 --bg）。允许它们存在是为了不改后台几百处引用，
// 但如果写成字面色值，深浅两套就得各写一遍，漏一处就是"暗色下按钮上的字看不见"
// ——直接断言它们写成 var(...) 指向上面那两个变量。
func TestAdminPanelAliasesCard(t *testing.T) {
	css := readFrontend(t, "internal/api/adminui/admin.css")
	for _, block := range []string{":root", `:root[data-theme="dark"]`, `:root:not([data-theme="light"])`} {
		v := varsOf(t, css, block)
		if got := v["--panel"]; got != "var(--card)" {
			t.Errorf("%s 的 --panel 应为 var(--card)（后台别名），实得 %q", block, got)
		}
		if got := v["--accent-fg"]; got != "var(--bg)" {
			t.Errorf("%s 的 --accent-fg 应为 var(--bg)（反白按钮文字），实得 %q", block, got)
		}
	}
}

// TestAdminDarkBlocksStayInSync 后台的`两套`暗色块必须逐行一致，且与公开看板的
// 暗色取值也一致。
//
// 后台的暗色块是**复制**出来的两份（手动 data-theme 一份、媒体查询一份），
// 与公开看板一样靠"逐行相同"来保证"手动切暗色"和"系统暗色"看到同一个站。
// 只改一份的后果很隐蔽：自己点切换是对的，系统暗色的访客看到另一个站。
func TestAdminDarkBlocksStayInSync(t *testing.T) {
	css := readFrontend(t, "internal/api/adminui/admin.css")
	declsOf := func(anchor string) []string {
		body := cssRuleBody(t, css, anchor)
		var out []string
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if i := strings.Index(line, "/*"); i >= 0 {
				line = strings.TrimSpace(line[:i])
			}
			// 注释只活在 [data-theme] 块里，整行跳过会把两块越比越"相等"，
			// 所以先剥注释再判空。
			if line == "" {
				continue
			}
			out = append(out, line)
		}
		return out
	}
	manual := declsOf(`:root[data-theme="dark"]`)
	media := declsOf(`:root:not([data-theme="light"])`)
	if len(manual) == 0 || len(media) == 0 {
		t.Fatal("后台两套暗色块至少有一套没抽到声明——选择器或结构变了，更新本测试")
	}
	if len(manual) != len(media) {
		t.Fatalf("后台两套暗色块声明条数不同（%d vs %d）：\n手动 %v\n媒体 %v",
			len(manual), len(media), manual, media)
	}
	for i := range manual {
		if manual[i] != media[i] {
			t.Fatalf("后台两套暗色块第 %d 行不一致：\n[data-theme] %q\n[media]      %q",
				i+1, manual[i], media[i])
		}
	}
	// 与公开看板的暗色取值同源：抽查最关键的几项。
	pubDark := varsOf(t, readFrontend(t, "internal/theme/builtin/style.css"), `:root[data-theme="dark"]`)
	admDark := varsOf(t, css, `:root[data-theme="dark"]`)
	for _, v := range []string{"--bg", "--fg", "--c1", "--c5", "--online"} {
		if pubDark[v] != admDark[v] {
			t.Errorf("暗色下 %s 两边不一致：公开看板 %q / 后台 %q", v, pubDark[v], admDark[v])
		}
	}
}

// TestAdminHasNoLegacySaturatedColors 后台不得残留彩色主题的配色。
//
// 后台原来有一套自己的蓝/绿/橙/红（含自己的 latencyColor 与 CHART_COLORS 数组）。
// 只删 CSS 里的变量是不够的：JS 里那些 hex/hsl 会被直接写进 canvas。
func TestAdminHasNoLegacySaturatedColors(t *testing.T) {
	// 国旗 SVG 不受限（旗帜颜色是身份信息），所以逐行过滤掉含 flag 数据的行。
	for _, rel := range []string{"internal/api/adminui/admin.css", "internal/api/adminui/admin.js"} {
		src := readFrontend(t, rel)
		for _, line := range strings.Split(src, "\n") {
			if strings.Contains(line, "svg") || strings.Contains(line, "starPts") {
				continue // 国旗
			}
			low := strings.ToLower(line)
			for _, bad := range append(append([]string{}, monoForbidden...),
				"#9ca3af", "#3b82f6", "#22c55e", "#ef4444", "#f59e0b", "#6366f1", "chart_colors") {
				if strings.Contains(low, bad) {
					t.Errorf("%s 残留彩色主题配色 %s：%s", rel, bad, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestAdminChartUsesGreyscalePalette 后台的图表必须与公开看板用同一张灰阶调色板，
// 且虚线必须复位。
//
// setLineDash 是**有状态**的：描边序列后不复位，坐标轴、网格、悬停十字线
// 会一起变成虚线——图上看起来"到处都是虚线"，但每一处单独看都没写错。
func TestAdminChartUsesGreyscalePalette(t *testing.T) {
	src := readFrontend(t, "internal/api/adminui/admin.js")
	if !strings.Contains(src, "var PALETTE = ['--c1', '--c2', '--c3', '--c4', '--c5'") {
		t.Error("后台 PALETTE 应为灰阶色阶变量名表（--c1…--c5），彩色 hex 数组是旧配色的遗产")
	}
	if !strings.Contains(src, "[6, 3]") || !strings.Contains(src, "[10, 4, 2, 4]") {
		t.Error("后台 PALETTE_DASH 应含真实虚线数组，全 null 等于没有——灰阶里明度不足以区分多序列")
	}
	if !strings.Contains(src, "function paletteColor(") || !strings.Contains(src, "function paletteDash(") {
		t.Error("后台应通过 paletteColor/paletteDash 取色：按序号取值才能保证图例勾选后颜色不串位")
	}
	body := funcBodyOf(t, src, "function latencyColor(")
	for _, lvl := range []string{"--c1", "--c2", "--c3", "--c4", "--c5"} {
		if !strings.Contains(body, lvl) {
			t.Errorf("后台 latencyColor 应覆盖灰阶全部 5 档，缺 %s（口径要与公开看板一致）", lvl)
		}
	}
	// 虚线必须成对出现：设置后要复位。
	if n := strings.Count(src, "ctx.setLineDash("); n < 4 {
		t.Errorf("setLineDash 调用过少（%d 处）：序列描边后必须复位，否则坐标轴也会变虚线", n)
	}
	if !strings.Contains(src, "ctx.setLineDash([])") {
		t.Error("缺少 ctx.setLineDash([]) 复位：虚线状态会泄漏到坐标轴与悬停线")
	}
}

// TestAdminProgressBarIsInk 后台进度条同样按"墨色填充、长度承载信息"处理，
// 不再按阈值变色（与公开看板同一语言）。
func TestAdminProgressBarIsInk(t *testing.T) {
	css := readFrontend(t, "internal/api/adminui/admin.css")
	if got := cssRuleBody(t, css, ".track i"); !strings.Contains(got, "var(--fg)") {
		t.Errorf(".track i 的填充应为 var(--fg)（前景墨色），实得：%s", got)
	}
	for _, dead := range []string{".track i.warn", ".track i.bad"} {
		if strings.Contains(css, dead) {
			t.Errorf("灰阶主题里进度条不按阈值变色：不得出现 %s 规则", dead)
		}
	}
}

// TestAdminLoginPageIsThemed 登录页也是这次换装的一部分：
// 登录卡必须有与顶栏同款的深浅切换按钮（.login-head），否则访客在
// 系统暗色下打开一个纯白登录页——"整体风格统一"就断在第一步。
func TestAdminLoginPageIsThemed(t *testing.T) {
	html := stripHTMLComments(readFrontend(t, "internal/api/adminui/index.html"))
	head := elementAt(t, html, `class="login-head"`)
	if !strings.Contains(head, `class="icon-btn tgl"`) {
		t.Errorf("登录卡缺 .login-head + .tgl 切换按钮，实得：%s", head)
	}
	if !strings.Contains(head, `class="ico-moon"`) || !strings.Contains(head, `class="ico-sun"`) {
		t.Error("登录卡的切换按钮也要带两个图标（显隐由 CSS 决定）")
	}
	// 顶栏也要有一颗：登录后不刷新页面，没有它会以为"主题按钮没了"。
	if !strings.Contains(html, `<button class="icon-btn tgl" type="button"`) {
		t.Error("后台顶栏缺少图标化的主题切换按钮")
	}
	// 两处共用同一段逻辑，因此找的是所有 .tgl 而不是某一个 var 。
	js := readFrontend(t, "internal/api/adminui/admin.js")
	if !strings.Contains(js, "document.querySelectorAll('.tgl')") {
		t.Error("后台主题切换应绑定全部 .tgl（登录卡 + 顶栏），只绑一个会让另一颗按钮失灵")
	}
	if !strings.Contains(js, "'probe.theme'") {
		t.Error("后台应复用 localStorage['probe.theme']：与公开看板同一个偏好键，站内不再出现两套记忆")
	}
	// ?theme=dark|light 免落盘覆盖必须与公开看板一致：截图与排障要靠它，
	// 而登录页在会话建立前没有任何别的入口能改主题。
	if !strings.Contains(js, "new URLSearchParams(location.search).get('theme')") {
		t.Error("后台缺 ?theme= 查询参数覆盖：截暗色登录页只能先改系统偏好")
	}
	// 查询参数**不能**落盘——否则一次截图链接就把访客的偏好永久改掉。
	if strings.Contains(js, "localStorage.setItem(ADMIN_THEME_KEY, q)") {
		t.Error("?theme= 覆盖不得写进 localStorage：一次截图链接会永久改掉偏好")
	}
	// 切换后要重跑当前视图的 loader 重画 canvas（颜色是绘制时取的实色）；
	// 但登录页上绝不能触发——会话未建立，loader 会打出一屏 401。
	if !strings.Contains(js, "$('app').hidden") {
		t.Error("切换处理器必须先判 #app 是否可见：登录页上重跑 loader 会打出一屏 401")
	}
}

// ---------------------------------------------------------------------------
// 3. 节点卡片：续费价格与点阵时间窗
// ---------------------------------------------------------------------------

// TestNodeFeeIsGated 续费价格行必须能被 public.cards 的 node_fee 关掉，
// 并且与其它区块一样"缺值渲染 — 不省略行"。
//
// 价格是**全局开关**（不是逐节点可选）：一旦某些节点开、某些关，
// 同一行网格里卡片高度就会参差——所以缺价格时渲染 "—" 而不是隐藏整行。
func TestNodeFeeIsGated(t *testing.T) {
	src := readFrontend(t, "internal/theme/builtin/app.js")
	if !strings.Contains(src, "'data-card', 'node_fee'") {
		t.Error("续费价格行没有挂 data-card=\"node_fee\"：没有可关的抓手")
	}
	if !strings.Contains(src, "cardOn('node_fee')") {
		t.Error("续费价格行没有做 cardOn('node_fee') 判断，开关无效")
	}
	// 终身 / 一次性折算后都是 0，但文案必须不同——靠 fee_cycle 区分，
	// 不能只看 monthly_fee == 0 就写 "—"。
	for _, want := range []string{"'lifetime'", "'once'", "n.monthly_fee", "n.fee_cycle"} {
		if !strings.Contains(src, want) {
			t.Errorf("续费价格的文案判断缺少 %s：终身/一次性/未配置三种情况会串成一个", want)
		}
	}
	// 缺价格时不能省略行：占位必须是 "—"。
	if !strings.Contains(src, "var feeText = '—';") {
		t.Error("续费价格的缺省文案应为 \"—\"（不省略行，否则卡片高度会参差）")
	}
	// 第 112 项：价格与实时速率同行（.net-line：速率左、价格右），
	// 不再是独立一行；两个开关各自只管自己的那一半。
	if !strings.Contains(src, "rateLine.appendChild(fee);") {
		t.Error("公开看板 .n-fee 没有挂进 .net-line：价格又变成独立一行了")
	}
	for _, want := range []string{
		"c.fee.hidden = !cardOn('node_fee');",
		"c.rateRow.hidden = !cardOn('node_net');",
		"c.totRow.hidden = !cardOn('node_net');",
		"c.net.hidden = !(cardOn('node_fee') || cardOn('node_net'));",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("公开看板卡片显隐逻辑缺少 %q：开关组合下卡片高度会跳", want)
		}
	}
}

// TestAdminNodeCardShowsFee 后台节点卡片也要有同款的「续费」一行。
//
// 用户的要求是"节点卡片新增续费价格"，第 4 条又要求后台与公开看板同一风格——
// 只在公开看板加、后台不加，正是两前端最典型的脱节形态。
// 后台没有 public.cards 那套逐块开关（管理员本就该看到全部信息），
// 所以这一行恒定显示，缺价格时渲染 "—"（省掉行会破坏"卡片等高"）。
func TestAdminNodeCardShowsFee(t *testing.T) {
	js := readFrontend(t, "internal/api/adminui/admin.js")
	card := funcBodyOf(t, js, "function makeNodeCard(")
	if !strings.Contains(card, "el('div', 'n-fee')") {
		t.Error("后台 makeNodeCard 没有构建 .n-fee 行：后台节点卡片看不到续费价格")
	}
	if !strings.Contains(card, "el('span', 'fee-k', '续费')") {
		t.Error("后台 .n-fee 行缺少「续费」标签（结构与公开看板不一致）")
	}
	upd := funcBodyOf(t, js, "function updateNodeCard(")
	// 必须断言**完整表达式**而不是裸字段名：n.fee_monthly2 也包含子串 "n.fee_monthly"，
	// 用 Contains("n.fee_monthly") 时变量改名照样通过——正是"守卫冗余"那一类假通过。
	for _, want := range []string{
		"if (n.fee_monthly > 0) {",
		"fmtMoney(n.fee_monthly, n.fee_currency)",
		"n.fee_cycle === 'lifetime'",
		"n.fee_cycle === 'once'",
		"var feeText = '—';",
	} {
		if !strings.Contains(upd, want) {
			t.Errorf("后台 updateNodeCard 的续费文案判断缺少 %q：终身/一次性/未配置会串成一个", want)
		}
	}
	if !strings.Contains(upd, "setText(c.feeV, feeText);") {
		t.Error("后台算出了 feeText 却没写进 DOM：金额永远不会显示")
	}
	// 第 112 项：价格不再单独占一行，而是与实时速率同行（速率左、价格右）。
	// 后台没有逐块开关，价格恒显示——但 DOM 结构必须与公开看板一致。
	if !strings.Contains(card, "rateLine.appendChild(fee);") {
		t.Error("后台 .n-fee 没有挂进 .net-line：价格要么消失了要么又另起一行")
	}
	if !strings.Contains(card, "net.appendChild(rateLine); net.appendChild(totRow);") {
		t.Error("后台 .net-line 没有挂进 .n-net：速率与价格都上不了卡片")
	}
	if !strings.Contains(card, "root.appendChild(bars); root.appendChild(net);") {
		t.Error("后台节点卡片没有把 .n-net 挂进 DOM")
	}
	// 后端必须把周期一起下发——只有金额的话，终身节点（月均 0）会显示成 "—"。
	item := readFrontend(t, "internal/api/admin_runtime.go")
	if !strings.Contains(item, "FeeCycle string") || !strings.Contains(item, `json:"fee_cycle,omitempty"`) {
		t.Error("AdminNodeItem 缺 FeeCycle/fee_cycle：后台分不清「终身」与「没配价格」")
	}
}

// TestFeeRowIsIdenticalInBothFrontends .n-fee 的样式必须两份逐字符一致。
//
// 卡片等高是硬约束，而行高由 `.n-fee { height: 16px }` 决定——
// 两边各自微调（14px / 17px）不会报错、不会难看，只会让两站的卡片不一样高。
func TestFeeRowIsIdenticalInBothFrontends(t *testing.T) {
	ruleOf := func(rel string) string {
		return strings.Join(strings.Fields(cssRuleBody(t, readFrontend(t, rel), ".n-fee")), " ")
	}
	pub := ruleOf("internal/theme/builtin/style.css")
	adm := ruleOf("internal/api/adminui/admin.css")
	if pub == "" || adm == "" {
		t.Fatal(".n-fee 规则没抽到——选择器改名了，更新本测试")
	}
	if pub != adm {
		t.Errorf(".n-fee 规则两边不一致：\n公开看板 %s\n后台     %s", pub, adm)
	}
	if !strings.Contains(pub, "height: 16px") {
		t.Errorf(".n-fee 必须钉住 height: 16px（卡片等高依赖它），实得：%s", pub)
	}
	// 第 112 项：价格与速率同行的容器 .net-line 也必须两边一致
	// （行高 17px 钉死，速率/价格两种开关组合下卡片高度都不变）。
	lineOf := func(rel string) string {
		return strings.Join(strings.Fields(cssRuleBody(t, readFrontend(t, rel), ".net-line")), " ")
	}
	pubLine := lineOf("internal/theme/builtin/style.css")
	admLine := lineOf("internal/api/adminui/admin.css")
	if pubLine == "" || admLine == "" {
		t.Fatal(".net-line 规则没抽到——选择器改名了，更新本测试")
	}
	if pubLine != admLine {
		t.Errorf(".net-line 规则两边不一致：\n公开看板 %s\n后台     %s", pubLine, admLine)
	}
	if !strings.Contains(pubLine, "height: 17px") || !strings.Contains(pubLine, "justify-content: space-between") {
		t.Errorf(".net-line 必须钉住 height:17px 且两端对齐（速率左、价格右），实得：%s", pubLine)
	}
}

// TestLatencyWindowIsConfigurable 点阵时间窗必须由设置项驱动，且标题 / 悬浮提示 /
// 重绘签名三处都跟着窗口走。
//
// 这三处漏任何一处，症状都不同、且都不报错：
//   - 标题写死 → 改成 30 分钟后点阵上方还写着「近 24 小时」；
//   - 悬浮提示写死 → 提示说"每个点约 45 分钟"，而每个点其实是 1 分钟；
//   - 签名不带窗口 → 管理员改完设置、调度器还没重算完的那一分钟里标题不刷新。
func TestLatencyWindowIsConfigurable(t *testing.T) {
	src := readFrontend(t, "internal/theme/builtin/app.js")
	// 窗口值来自 /site（settings.public.lat_window），不是写死的常量。
	if !strings.Contains(src, "s.lat_window") {
		t.Error("app.js 没有从 /site 读取 lat_window：时间窗设置不会生效")
	}
	for _, w := range []string{"'30m'", "'1h'", "'6h'", "'24h'"} {
		if !strings.Contains(src, w+":") {
			t.Errorf("缺少时间窗标签 %s（LAT_WINDOW_LABEL 应覆盖四档）", w)
		}
	}
	// 标题文案必须由函数生成；写死"近 24 小时"会让改完设置后标题不动。
	if !strings.Contains(src, `'延迟 / 丢包 · ' + latWindowLabel()`) {
		t.Error(`点阵标题应写成 '延迟 / 丢包 · ' + latWindowLabel()，否则改成 30 分钟后标题还是「近 24 小时」`)
	}
	// 签名要带上窗口：管理员改完设置、调度器还没重算完的那一分钟里，
	// 标题也要先跟上（否则旧文案会一直挂着直到数据变化）。
	if !strings.Contains(src, "var sig = latWindow + '|on'") {
		t.Error("点阵重绘签名没带上 latWindow：只改窗口时标题不会刷新")
	}
	// 每点时长必须取自后端下发的 step，不能前端自己算死。
	if !strings.Contains(src, "每个点约 ' + stepLabel(list[0].step)") {
		t.Error("悬浮提示里的「每个点约 X」应取自后端下发的 b.step")
	}
}

// TestLatencyWindowRegisteredInAdmin 设置项必须在后台登记中文名与分组，
// 否则后台显示原始键名 public.lat_window（不变量 15）。
func TestLatencyWindowRegisteredInAdmin(t *testing.T) {
	js := readFrontend(t, "internal/api/adminui/admin.js")
	// 断言**标签映射这一条**，而不是裸的键名：SETTING_GROUPS 里也有 'public.lat_window'，
	// 只查键名的话"标签被改错/删掉"仍然通过（键在分组里、设置页显示的是原始键名）。
	if !strings.Contains(js, "'public.lat_window': ['节点卡点阵时间窗'") {
		t.Fatal("后台 SETTING_LABELS 没登记 public.lat_window：设置页会显示原始键名")
	}
	// 必须落在「公开看板」分组里，紧邻 public.cards——它是同一次配置动作。
	// 精确钉住整组内容（含 public.show_ip_badge）：IP 标识开关与 lat_window
	// 同属公开看板配置，分组漂移（键被挪走/删掉）必须在这里现形。
	if !strings.Contains(js, "['公开看板', ['public.enabled', 'public.cards', 'public.lat_window', 'public.show_ip_badge']]") {
		t.Error("public.lat_window 应归入「公开看板」分组（与 public.enabled / public.cards / public.show_ip_badge 同组）")
	}
	// 选项文案要与枚举值一一对应（30m/1h/6h/24h），少一个就选不到。
	for _, opt := range []string{"30m=近 30 分钟", "1h=近 1 小时", "6h=近 6 小时", "24h=近 24 小时"} {
		if !strings.Contains(js, opt) {
			t.Errorf("后台设置项缺少选项 %q", opt)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. 第 109~112 项：全站去原生蓝、悬停容差、审计分页、价格与速率同行
// ---------------------------------------------------------------------------

// TestFormControlsUseInkAccent 勾选框/单选框的选中色必须是前景墨色（第 109 项）。
//
// 浏览器默认的 accent 蓝（Chrome #1a73e8 系）是黑白灰阶界面里唯一的彩色，
// 用户点名要求去掉。accent-color 是继承属性，钉在 input 上即可覆盖两站
// 所有勾选控件（设置页的开关组、告警渠道勾选、注册码表单……）。
// 断言的是**完整分组选择器的规则体**：只查 "accent-color" 子串的话，
// 有人把规则挪进 .date-pop 里照样通过。
func TestFormControlsUseInkAccent(t *testing.T) {
	for _, rel := range []string{"internal/theme/builtin/style.css", "internal/api/adminui/admin.css"} {
		css := readFrontend(t, rel)
		body := strings.Join(strings.Fields(
			cssRuleBody(t, css, `input[type="checkbox"], input[type="radio"]`)), " ")
		if !strings.Contains(body, "accent-color: var(--fg)") {
			t.Errorf("%s 勾选控件没有 accent-color: var(--fg)（原生蓝突兀）：%s", rel, body)
		}
	}
}

// TestDatePopupIsSelfDrawn 到期日弹层必须自绘，且不得再出现原生 date 输入（第 109 项）。
//
// Chromium 原生日历弹层的「清除 / 今天」按钮颜色在源码里写死为 #0078D4
// （third_party/blink/.../calendar_picker.css 的 .clear-button/.today-button），
// 页面上任何 CSS 都影响不到它——所以"把蓝色按钮染灰"这条路不存在，
// 谁把 addInput('b-expire','date',...) 改回来，蓝色按钮就会跟着回来。
func TestDatePopupIsSelfDrawn(t *testing.T) {
	js := readFrontend(t, "internal/api/adminui/admin.js")
	if strings.Contains(js, `'b-expire', 'date'`) {
		t.Error("到期日还在用原生 <input type=date>：弹层「清除/今天」会是 Chromium 硬编码的蓝色")
	}
	if !strings.Contains(js, "function attachDatePicker(") {
		t.Fatal("缺 attachDatePicker：自绘日历没了，到期日没法选")
	}
	body := funcBodyOf(t, js, "function attachDatePicker(")
	for _, want := range []string{
		`el('button', 'dp-act', '清除')`,
		`el('button', 'dp-act', '今天')`,
		"input.value = view.y + '-' + p2(view.m + 1) + '-' + p2(dnum)",
		"pop.hidden = false",
		"if (pop.hidden) open(); else close();",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("attachDatePicker 缺少 %q：选择/清除/开合链路不完整", want)
		}
	}
	// 弹层必须落进 admin.css（断言**规则头**而不是裸子串：".dp-grid" 是
	// ".dp-grid-x" 的前缀，改名字符串照样命中——变异验证抓出来过的假通过），
	// 且取值只允许主题变量与中性色。
	css := readFrontend(t, "internal/api/adminui/admin.css")
	for _, sel := range []string{".date-wrap {", ".date-pop {", ".dp-grid {", ".dp-day {", ".dp-act {"} {
		if !strings.Contains(css, sel) {
			t.Errorf("admin.css 缺少规则 %s：自绘日历没有样式", sel)
		}
	}
	dayRule := strings.Join(strings.Fields(cssRuleBody(t, css, ".dp-day")), " ")
	for _, bad := range []string{"#0078D4", "#1a73e8", "blue"} {
		if strings.Contains(strings.ToLower(dayRule), strings.ToLower(bad)) {
			t.Errorf(".dp-day 规则里出现蓝色取值 %s：%s", bad, dayRule)
		}
	}
}

// TestChartHoverFollowsCursorAndHonoursTolerance 折线图悬停的三个硬要求（第 110 项）：
//
//  1. 十字线跟手：x 由 hoverTs 算出（画在光标处），而不是"最近样本"处——
//     目标刚建/中途启用时数据只覆盖窗口尾部，无容差地取最近样本会把
//     十字线和提示框钉在几小时外，看起来就是"悬停没有详情"；
//  2. 命中有容差：1.5×平均点距，下限为窗口宽度的 0.5%（span/200）；
//  3. 没覆盖到光标处的序列在提示框里写「无数据」，不拿几小时外的值冒充。
//
// 两份前端是同一套机制的两份拷贝，表达式必须两边都在——改一处等于没改。
func TestChartHoverFollowsCursorAndHonoursTolerance(t *testing.T) {
	for _, rel := range []string{"internal/theme/builtin/app.js", "internal/api/adminui/admin.js"} {
		src := readFrontend(t, rel)
		body := funcBodyOf(t, src, "function drawChartHover(")
		for _, want := range []string{
			"var x = ax.padL + ax.plotW * ((hoverTs - ax.tMin) / (ax.tMax - ax.tMin));",
			"var tolFloor = (ax.tMax - ax.tMin) / 200;",
			"var tol = Math.max(avg * 1.5, tolFloor);",
			"if (best && bestD <= tol) hits.push({ s: s, pt: best });",
			"else miss.push(s);",
			"if (!hits.length) { ctx.restore(); return; }",
			"miss.forEach(function (s) {",
			"+ '无数据'",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s 的 drawChartHover 缺少 %q：悬停会退回到\"钉在最近样本\"的旧行为", rel, want)
			}
		}
		// 取样点必须画在样本自己的横坐标上（px），不能挪到十字线 x 上——
		// 点代表"那一次的真实测量"，挪到光标处就是在撒谎。
		if !strings.Contains(body, "var px = ax.padL + ax.plotW * ((h.pt.ts - ax.tMin)") ||
			!strings.Contains(body, "ctx.arc(px, y, 3.2, 0, Math.PI * 2);") {
			t.Errorf("%s 的取样点没有按样本自身 ts 定位", rel)
		}
	}
}

// TestAuditDefaultPageSizeIs20 审计日志默认每页 20 条（第 111 项，与事件一致）。
// PAGE_SIZES 必须仍包含 20——下拉框靠它回显当前生效值。
func TestAuditDefaultPageSizeIs20(t *testing.T) {
	js := readFrontend(t, "internal/api/adminui/admin.js")
	if !strings.Contains(js, "audit: { limit: 20, offset: 0, total: 0 }") {
		t.Error("审计日志默认每页不是 20 条")
	}
	if !strings.Contains(js, "var PAGE_SIZES = [20, 50, 100, 200];") {
		t.Error("PAGE_SIZES 应为 [20, 50, 100, 200]")
	}
}
