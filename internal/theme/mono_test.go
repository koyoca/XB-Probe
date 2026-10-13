package theme

import (
	"strings"
	"testing"
)

// 本文件的护栏钉住「黑白灰阶」主题的三条主张（移植自 monitor-theme-default，
// 详见 style.css 顶部「风格来源」一节）：
//
//  1. 灰阶承载信息——全套 UI 不出现饱和色，唯一的彩色是 --online 状态点；
//  2. 边框即层次——卡片没有阴影，hover 只描深边框；
//  3. 手动深浅切换——data-theme 写在 <html> 上，两套暗色块必须逐行一致。
//
// 这些约定靠注释约束不了：新增组件时顺手写一个 #16a34a 就把主题打回彩色了，
// 所以每条都用源码级断言钉住。所有测试都必须通过「变异验证」（抹掉被保护的
// 行必须变红），否则护栏本身可能是摆设。

// monoForbidden 是历史彩色主题用过的颜色。它们不该再出现在公开看板的
// 任何源文件里（国旗 SVG 不受限——旗帜颜色是身份信息，参考主题同样保留）。
var monoForbidden = []string{
	"#2563eb", "#16a34a", "#d97706", "#dc2626", "#7c3aed", "#0891b2",
	"#2f7de1", "#7c5cf0", "#60a5fa", "#a78bfa", "#34d399", "#fbbf24", "#f87171",
	"hsl(120", "hsl(39", "hsl(0,", "hsl(80",
}

func TestPublicBoardIsMonochrome(t *testing.T) {
	for _, rel := range []string{
		"internal/theme/builtin/style.css",
		"internal/theme/builtin/app.js",
		"internal/theme/builtin/index.html",
	} {
		src := readFrontend(t, rel)
		lower := strings.ToLower(src)
		for _, c := range monoForbidden {
			if strings.Contains(lower, c) {
				t.Errorf("%s 出现了彩色主题的遗留颜色 %s——灰阶主题里颜色只留给 --online 与国旗", rel, c)
			}
		}
	}
}

// TestDotOnlineIsTheOnlyColor 状态点的分色：在线=--online（唯一的彩色），
// 离线/疑似离线=灰（不再上报的机器只是"变旧了"，参考主题拒绝用红色表达），
// 探测延迟三档走灰阶色阶且"越严重墨越重"（--c1 比 --c3 更靠近前景）。
func TestDotOnlineIsTheOnlyColor(t *testing.T) {
	css := readFrontend(t, "internal/theme/builtin/style.css")
	if got := cssRuleBody(t, css, ".dot.online"); !strings.Contains(got, "var(--online)") {
		t.Errorf(".dot.online 必须用 var(--online)（唯一的彩色），实得：%s", got)
	}
	for _, sel := range []string{".dot.offline", ".dot.warning"} {
		if got := cssRuleBody(t, css, sel); !strings.Contains(got, "var(--offline)") {
			t.Errorf("%s 必须是灰（var(--offline)），红色是彩色主题的遗产，实得：%s", sel, got)
		}
	}
	// 「越严重墨越重」用 --line-strong--muted-2 的灰阶顺序校验不够直观，
	// 直接比对 --c1/--c3 在亮色下的深浅（c1 应更暗 = 更重的墨）。
	for _, v := range []string{"--c1", "--c3"} {
		if !strings.Contains(css, v+":") {
			t.Fatalf("style.css 缺少灰阶色阶变量 %s", v)
		}
	}
	// 亮色 1 次 + 两套暗色块各 1 次 = 3。
	if n := strings.Count(css, "--online:"); n != 3 {
		t.Errorf("--online 应出现 3 次（亮色 1 + 暗色 2），实际 %d 次", n)
	}
}

// TestProgressFillIsForeground 进度条填充必须是前景墨色且没有阈值变色规则：
// 参考主题 Meter 组件的原话是 "Monochrome, since the length of the bar carries
// the message"。warn/bad 类仍由 JS 挂上（到期文案等真正需要分级的地方要用），
// 但 CSS 里**不得**再给 .track i.warn/.bad 任何规则。
func TestProgressFillIsForeground(t *testing.T) {
	css := readFrontend(t, "internal/theme/builtin/style.css")
	if got := cssRuleBody(t, css, ".track i"); !strings.Contains(got, "var(--accent)") {
		t.Errorf(".track i 的填充必须是 var(--accent)（=前景墨色），实得：%s", got)
	}
	if strings.Contains(css, ".track i.warn") || strings.Contains(css, ".track i.bad") {
		t.Error("灰阶主题里进度条不按阈值变色：不得出现 .track i.warn / .track i.bad 规则")
	}
}

// TestDarkBlocksStayInSync 手动切换依赖两套暗色变量逐行一致：
// :root[data-theme="dark"] 与媒体查询里 :root:not([data-theme="light"])。
// 抽出两块的声明（去掉注释与空白）逐字符比对——不一致就等于"手动切到暗色"
// 和"系统暗色"看到两个不同的站。
func TestDarkBlocksStayInSync(t *testing.T) {
	css := readFrontend(t, "internal/theme/builtin/style.css")
	blockOf := func(anchor string) string {
		at := strings.Index(css, anchor)
		if at < 0 {
			t.Fatalf("style.css 找不到 %s", anchor)
		}
		open := strings.Index(css[at:], "{")
		closeAt := strings.Index(css[at:], "}")
		if open < 0 || closeAt < open {
			t.Fatalf("%s 的块结构不完整", anchor)
		}
		var b strings.Builder
		for _, line := range strings.Split(css[at+open+1:at+closeAt], "\n") {
			// 去掉行内注释再比对：注释只活在 [data-theme] 块里（给 oklch 原值），
			// 若整行跳过含注释的行，两块永远比不齐。
			if i := strings.Index(line, "/*"); i >= 0 {
				line = line[:i]
			}
			line = strings.TrimSpace(line)
			if line == "" || !strings.HasPrefix(line, "--") {
				continue
			}
			b.WriteString(line)
		}
		return b.String()
	}
	a := blockOf(`:root[data-theme="dark"]`)
	b := blockOf(`:root:not([data-theme="light"])`)
	if a == "" || b == "" {
		t.Fatal("两套暗色块至少有一套没抽到声明——选择器或结构变了，更新本测试")
	}
	if a != b {
		t.Fatalf("两套暗色变量不一致：\n[data-theme] %s\n[media]      %s", a, b)
	}
}

// TestCardsHaveNoShadow 卡片层次只来自 1px 边框。--shadow 变量应已删除；
// box-shadow 全文件只允许出现一次——国旗上那圈 1px 细描边（参考主题的
// ring-1 ring-foreground/10 的等价物，没有它浅色旗帜会在白卡上消失）。
func TestCardsHaveNoShadow(t *testing.T) {
	css := readFrontend(t, "internal/theme/builtin/style.css")
	if strings.Contains(css, "var(--shadow)") {
		t.Error("--shadow 变量应已随灰阶化删除：卡片层次只来自边框")
	}
	if n := strings.Count(css, "box-shadow"); n != 1 {
		t.Errorf("box-shadow 只允许出现在国旗描边这一处（1 次），实际 %d 次", n)
	}
	if !strings.Contains(css, "box-shadow: 0 0 0 1px") {
		t.Error("仅存的那处 box-shadow 应是国旗的 1px 细描边")
	}
}

// TestActiveTabIsInverted 选中的页签 = 反白墨块（bg-primary 的等价物）。
// 灰阶里"最重的墨"是最强的选中信号；如果改回了描边+阴影的旧样式，
// 页签在一片白卡中间会立刻失去辨识度。
func TestActiveTabIsInverted(t *testing.T) {
	css := readFrontend(t, "internal/theme/builtin/style.css")
	if got := cssRuleBody(t, css, `.tab[aria-selected="true"]`); !strings.Contains(got, "var(--fg)") {
		t.Errorf(`.tab[aria-selected="true"] 应为反白墨块（background: var(--fg)），实得：%s`, got)
	}
	if got := cssRuleBody(t, css, ".tab-bar .tab.active"); !strings.Contains(got, "var(--fg)") {
		t.Errorf(".tab-bar .tab.active 应为反白墨块，实得：%s", got)
	}
}

// TestGreyscaleChartPalette app.js 的多序列调色板必须是「CSS 变量名 + 虚线」，
// 不再是彩色 hex 数组；延迟取色按值走灰阶档位。
func TestGreyscaleChartPalette(t *testing.T) {
	src := readFrontend(t, "internal/theme/builtin/app.js")
	if !strings.Contains(src, "var PALETTE = ['--c1', '--c2', '--c3', '--c4', '--c5'") {
		t.Error("PALETTE 应为灰阶色阶变量名表（--c1…--c5），彩色 hex 数组是旧主题的遗产")
	}
	if !strings.Contains(src, "PALETTE_DASH") {
		t.Error("灰阶里明度不够区分多序列，必须有 PALETTE_DASH 虚线表配合")
	}
	// 虚线表必须真的有内容：被清成一排 null 时上面的存在性检查照样通过。
	if !strings.Contains(src, "[6, 3]") || !strings.Contains(src, "[10, 4, 2, 4]") {
		t.Error("PALETTE_DASH 应含真实的虚线数组（[6, 3] / [10, 4, 2, 4] 等），全 null 等于没有")
	}
	body := funcBodyOf(t, src, "function latencyColor(")
	for _, lvl := range []string{"--c1", "--c2", "--c3", "--c4", "--c5"} {
		if !strings.Contains(body, lvl) {
			t.Errorf("latencyColor 应覆盖灰阶全部 5 档，缺 %s", lvl)
		}
	}
	// 深浅切换后 canvas 取的是新主题的实色——切换处理器必须触发重画。
	toggle := funcBodyOf(t, src, "(function initTheme()")
	if !strings.Contains(toggle, "tick()") {
		t.Error("主题切换后必须调用 tick() 重画 canvas，否则曲线还是旧主题的颜色")
	}
	if !strings.Contains(src, "probe.theme") {
		t.Error("主题偏好应落在 localStorage['probe.theme']")
	}
	if !strings.Contains(src, "?theme") && !strings.Contains(src, "'theme'") {
		t.Error("应支持 ?theme= 查询参数（截图与排障的免落盘入口）")
	}
}
