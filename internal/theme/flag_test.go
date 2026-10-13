package theme

import (
	"strings"
	"testing"
)

// 国旗标识的回归护栏。
//
// 背景（用户上报的需求）：节点小卡片上要能看到该节点所在国家/地区的国旗。
//
// 实现约束决定了它必须长成现在这样，因此每一层都需要护栏：
//  1. CSP 恒为 default-src 'self' —— 外链图片与 data: URI 都被浏览器拒绝，
//     所以旗帜只能是**内联 SVG**，由 JS 用本地的常量表生成；
//  2. 前端零外部依赖 —— 不能引任何旗帜图标库；
//  3. emoji 国旗在 Windows 上不渲染成旗帜（显示成两个字母方块），
//     而本项目的主要使用者就在 Windows 上，因此不能走 emoji 那条捷径；
//  4. 两个前端（公开看板 / 管理后台）是有意重复的两份副本，
//     只改一处是这类需求最容易留下的形态。
//
// 护栏分三类：国家表在两边**逐 token 一致**、两个前端都把它接进了卡片、
// CSS 固定宽度（保证"有没有国旗"不改变卡片布局）。

// extractFlagBlock 取出两个显式标记之间的国旗公共区块。
func extractFlagBlock(t *testing.T, rel string) string {
	t.Helper()
	src := readFrontend(t, rel)
	const begin = "// -- 国旗：FLAG-COMMON-BEGIN --"
	const end = "// -- 国旗：FLAG-COMMON-END --"
	i := strings.Index(src, begin)
	j := strings.Index(src, end)
	if i < 0 || j < 0 || j <= i {
		t.Fatalf("%s 里找不到国旗公共区块的 BEGIN/END 标记", rel)
	}
	return src[i:j]
}

// normalizeJS 去掉整行注释与全部空白，只留下逻辑本身。
//
// 换行位置与缩进差异不是逻辑差异（一个对象字面量写成一行或多行只是排版），
// 真正要锁的是"两个前端画的是同一批旗、同一套图形描述"。
func normalizeJS(body string) string {
	var b strings.Builder
	for _, ln := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		b.WriteString(trimmed)
	}
	return strings.Join(strings.Fields(b.String()), "")
}

// 两个前端的国旗表必须逐 token 一致。
//
// 只改一处时最典型的后果：公开看板有旗、后台没有（或反过来），
// 而且因为两边都不报错，只能靠肉眼比对才发现。
func TestFrontendFlagTablesIdentical(t *testing.T) {
	var normed []string
	for _, rel := range frontendFiles {
		normed = append(normed, normalizeJS(extractFlagBlock(t, rel)))
	}
	if normed[0] == "" || normed[1] == "" {
		t.Fatalf("国旗区块解析为空：len=%d/%d", len(normed[0]), len(normed[1]))
	}
	if normed[0] != normed[1] {
		t.Errorf("两个前端的国旗表已脱节（说明只改了一处）：\n--- %s ---\n%s\n--- %s ---\n%s",
			frontendFiles[0], normed[0], frontendFiles[1], normed[1])
	}
}

// 国旗表必须覆盖到常用的部署地区。数量下限只是为了拦住"表被清空/被截断"
// 这类事故，具体名单不锁死（用户可能合法地增删）。
func TestFlagTableCoversCommonRegions(t *testing.T) {
	block := extractFlagBlock(t, "internal/theme/builtin/app.js")
	// 表里每一行形如 `  XX: ['名称', ...],`，统计出现次数即可。
	got := strings.Count(block, ": ['")
	if got < 40 {
		t.Errorf("国旗表只有 %d 个地区，疑似被截断（期望 ≥40）", got)
	}
	for _, code := range []string{"CN: [", "HK: [", "TW: [", "MO: [", "JP: [", "US: [", "DE: ["} {
		if !strings.Contains(block, code) {
			t.Errorf("国旗表缺少 %q", strings.TrimSuffix(code, ": ["))
		}
	}
}

// 未登记的代码必须**不画旗**（返回空串），而不是画一面默认旗。
//
// 这与项目里"缺值渲染 — 而不是编一个值"同源：画错旗比不画旗更有害，
// 因为它看起来是确定信息。
func TestFlagUnknownCodeRendersNothing(t *testing.T) {
	for _, rel := range frontendFiles {
		block := extractFlagBlock(t, rel)
		if !strings.Contains(block, "if (!e) return '';") {
			t.Errorf("%s 的 flagSVG 对未登记代码没有返回空串：会画出错误的旗帜", rel)
		}
	}
}

// 图形描述 DSL 的四个分支必须齐全（横带/竖带/北欧十字/手绘）。
// 少任何一个分支，对应形态的旗帜都会渲染成空白，而且不报错。
func TestFlagDSLHandlesAllForms(t *testing.T) {
	for _, rel := range frontendFiles {
		block := extractFlagBlock(t, rel)
		for _, frag := range []string{
			"kind === 'h' || kind === 'v'", // 横/竖色带
			"kind === 'x'",                 // 北欧十字
			"inner = arg;",                 // 手绘兜底
			"function starPts(",            // 五角星（CN/VN/CL/TR…）
		} {
			if !strings.Contains(block, frag) {
				t.Errorf("%s 的国旗 DSL 缺少分支 %q", rel, frag)
			}
		}
	}
}

// 旗帜必须经 svgNode 解析成 DOM 再 appendChild，不能拼 innerHTML。
// 本项目的既定约定是"不可信内容只用 textContent"；旗帜虽是本地生成，
// 保持同一条路径能避免以后有人把外部字符串塞进来。
func TestFlagRenderingAvoidsInnerHTML(t *testing.T) {
	for _, rel := range frontendFiles {
		src := readFrontend(t, rel)
		body := funcBodyOf(t, src, "  function renderFlag(")
		if strings.Contains(body, "innerHTML") {
			t.Errorf("%s 的 renderFlag 使用了 innerHTML", rel)
		}
		if !strings.Contains(body, "svgNode(flagSVG(code))") {
			t.Errorf("%s 的 renderFlag 没有经 svgNode(flagSVG(...)) 生成旗帜", rel)
		}
	}
}

// 两个前端都必须把国旗接进节点卡片：占位元素 + 每轮刷新时写入。
//
// 只定义不接线（表在、函数在，卡片上什么都不显示）是这类需求最常见的
// "看起来做完了但没效果"的形态，必须有断言钉住。
func TestFrontendsWireFlagIntoNodeCard(t *testing.T) {
	for _, rel := range frontendFiles {
		src := readFrontend(t, rel)
		if !strings.Contains(src, "var flag = el('span', 'n-flag')") {
			t.Errorf("%s 的节点卡片没有国旗占位元素", rel)
		}
		if !strings.Contains(src, "renderFlag(c.flag, n.country)") {
			t.Errorf("%s 没有在刷新卡片时写入国旗", rel)
		}
	}
}

// 公开看板的详情页标题旁也要有国旗：同一个 DTO 字段，两处展示，
// 少一处就会出现"列表有旗、点进去没有"的不一致。
func TestPublicDetailHeaderHasFlag(t *testing.T) {
	src := readFrontend(t, "internal/theme/builtin/app.js")
	if !strings.Contains(src, "renderFlag($('detail-flag'), n.country)") {
		t.Errorf("公开看板详情页标题没有渲染国旗")
	}
	html := readFrontend(t, "internal/theme/builtin/index.html")
	if !strings.Contains(stripHTMLComments(html), `id="detail-flag"`) {
		t.Errorf("公开主题 index.html 缺少 detail-flag 占位元素")
	}
}

// 后台的国家/地区下拉必须由国旗表生成，而不是另抄一份国家列表。
//
// 抄一份的后果：下拉里能选的代码与前端能画的代码逐渐分叉，
// 出现"选了国家但卡片上没有旗帜"，且两边都没有任何报错。
func TestAdminCountrySelectDerivesFromFlagTable(t *testing.T) {
	src := readFrontend(t, "internal/api/adminui/admin.js")
	if !strings.Contains(src, "Object.keys(FLAGS)") {
		t.Errorf("后台的国家下拉不是由 FLAGS 生成的，国家列表会与国旗表脱节")
	}
	if !strings.Contains(src, "cSel.id = 'n-country'") {
		t.Errorf("后台缺少 id 为 n-country 的国家选择框")
	}
	if !strings.Contains(src, "country: $('n-country').value") {
		t.Errorf("保存节点时没有把 country 提交给后端，选择框形同虚设")
	}
}

// CSS 必须给国旗一格**固定宽度**：否则"这个节点有没有国家"会让同一行的
// 名称起点左右跳动——卡片位置固定是本项目的硬约束。
func TestFlagCSSReservesFixedWidth(t *testing.T) {
	for _, rel := range []string{
		"internal/theme/builtin/style.css",
		"internal/api/adminui/admin.css",
	} {
		css := readFrontend(t, rel)
		rule := cssRuleBody(t, css, ".n-flag")
		if !strings.Contains(rule, "width: 20px") || !strings.Contains(rule, "height: 14px") {
			t.Errorf("%s 的 .n-flag 没有固定 20x14：卡片位置会随「有没有国家」跳动（规则：%s）", rel, rule)
		}
	}
}

// 生成的 <svg> 根元素必须带 xmlns（SVG 命名空间声明）。
//
// svgNode() 用 XML 解析（image/svg+xml），而 XML 对无前缀元素**没有默认
// 命名空间**——缺 xmlns 时 <svg> 落在空命名空间，document.importNode 进
// HTML 后只是普通未知元素，按 0×0 布局：不报错、整面旗帜"消失"
// （无头 Chrome 实测：ctor=Element、rect=0.0x0.0、ns=null）。
// 同一段标记走 innerHTML 却是好的——HTML 解析器会把 <svg> 自动归入 SVG
// 命名空间。两条路径表现不同、源码上看不出差异，这正是漏测的成因，
// 必须有这条护栏钉住。
func TestFlagSVGRootDeclaresXMLNS(t *testing.T) {
	for _, rel := range frontendFiles {
		block := extractFlagBlock(t, rel)
		if !strings.Contains(block, `'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 14"`) {
			t.Errorf("%s 的 flagSVG 根元素缺 xmlns：XML 解析后旗帜落在空命名空间，渲染成 0x0（不可见）", rel)
		}
	}
}
