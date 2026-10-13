package theme

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 断档（gap）绘制的回归护栏。
//
// 背景缺陷：节点或 Master 断联期间不落任何样本，时间序列里那一段就是**空**。
// 前端两个画图函数（公开看板 internal/theme/builtin/app.js 与后台
// internal/api/adminui/admin.js 的 drawChart）都把相邻两点直接连成直线，
// 于是"断联 6 小时"被画成一条横跨 6 小时的平滑水平线——最该被看见的故障
// 被渲染成了最健康的样子。
//
// 修法是：按序列自身相邻点间隔的**中位数 ×3** 判定断档，把序列切成连续段，
// 折线/面积/末点都按段绘制，段间不连线。
//
// 为什么用源码级护栏而不是把 JS 跑起来：跑 JS 需要 Node，会把 go test 从
// "纯 Go 零依赖"变成依赖外部运行时（CI 与三平台交叉环境都得配）。本项目的
// 前端零外部依赖、两个前端物理隔离（两套 embed），最实际的保护是盯住
// "两个文件都在用分段绘制"这件事本身——这与 install_embed_test.go 的思路一致。
//
// 关键：这个护栏必须在**两个**前端上都跑，因为它们是两份独立副本，
// 只改一处是这类缺陷最容易留下的形态（改了公开看板、忘了后台）。

// frontendFiles 是两个必须同时修的文件（相对仓库根）。
var frontendFiles = []string{
	"internal/theme/builtin/app.js",
	"internal/api/adminui/admin.js",
}

func readFrontend(t *testing.T, rel string) string {
	t.Helper()
	// 测试的工作目录是本包目录（internal/theme），向上一级到仓库根。
	p := filepath.Join("..", "..", rel)
	b, err := os.ReadFile(filepath.Clean(p))
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", rel, err)
	}
	return string(b)
}

// 两个前端都必须定义断档检测的两个函数。
func TestFrontendsDefineGapHelpers(t *testing.T) {
	for _, rel := range frontendFiles {
		src := readFrontend(t, rel)
		for _, fn := range []string{"function gapThreshold(", "function segmentsOf("} {
			if !strings.Contains(src, fn) {
				t.Errorf("%s 缺少 %q：断档绘制没有实现，折线会再次跨越无数据区间", rel, fn)
			}
		}
	}
}

// 折线绘制必须按分段画：每个连续段各自 beginPath，段间不连线。
//
// 判据是"画折线的循环里出现了对段数组的遍历"，而不是直接遍历所有点。
// 具体地：修复后的写法是 `segs.forEach(function (seg) {` 之后才 beginPath。
func TestFrontendsStrokePerSegment(t *testing.T) {
	for _, rel := range frontendFiles {
		src := readFrontend(t, rel)
		if !strings.Contains(src, "segmentsOf(s.points, gapThreshold(s.points)") {
			t.Errorf("%s 的折线没有按 gapThreshold/segmentsOf 分段，断档仍会被连成直线", rel)
		}
	}
}

// 面积填充同样必须分段。只断开折线、面积仍连成一片的话，图上会留下
// 一块覆盖了断档区间的淡化色块——比不断开还难解释。
func TestFrontendsFillAreaPerSegment(t *testing.T) {
	for _, rel := range frontendFiles {
		src := readFrontend(t, rel)
		// 面积分支应遍历 segs 且跳过单点段（单点段填不出面积）。
		if !strings.Contains(src, "if (seg.length < 2) return;") {
			t.Errorf("%s 的面积填充没有按段处理，断档区间会被底色覆盖", rel)
		}
	}
}

// 旧写法必须彻底消失：一条 beginPath 之后把所有点 lineTo 过去。
//
// 这里用"修复前的那段特征代码"作为负向断言——它一旦回来就说明有人
// 回滚了逻辑（或者新加了一个画图函数又照抄了老写法）。
func TestFrontendsNoNaivePolylineOverAllPoints(t *testing.T) {
	// 修复前的形态：先 map 出全部 xy，再在同一个 path 里 moveTo/lineTo 全部点。
	naive := "xy.forEach(function (q, k) {\n        if (k === 0) ctx.moveTo(q[0], q[1]); else ctx.lineTo(q[0], q[1]);"
	for _, rel := range frontendFiles {
		src := readFrontend(t, rel)
		if strings.Contains(src, naive) {
			t.Errorf("%s 仍在使用「一次性把所有点连起来」的旧写法，断档会被填平", rel)
		}
	}
}

// 两个前端的分段逻辑必须一致：阈值系数、最小样本数、中位数取法。
//
// 两处实现是有意重复的（两套 embed、后台不随主题变化），因此必须靠测试
// 锁住"看起来一样"。曾经后台把 rtt_avg 的 NULL 读成 0（公开侧早修过），
// 就是这类双份实现脱节的先例。
//
// 比较时**去掉全部空白与注释**：两边换行位置不同（一个三元表达式写成两行
// 只是排版差异）不该判失败，真正要锁的是逻辑本身。
func TestFrontendGapLogicIdentical(t *testing.T) {
	extract := func(src, start, end string) string {
		i := strings.Index(src, start)
		if i < 0 {
			return ""
		}
		j := strings.Index(src[i:], end)
		if j < 0 {
			return ""
		}
		return src[i : i+j]
	}

	normalize := func(body string) string {
		var b strings.Builder
		for _, ln := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(ln)
			if trimmed == "" || strings.HasPrefix(trimmed, "//") {
				continue
			}
			// 去掉行内注释（`code; // 说明`）。
			if k := strings.Index(trimmed, "//"); k >= 0 &&
				!strings.Contains(trimmed[:k], "\"") && !strings.Contains(trimmed[:k], "'") {
				trimmed = strings.TrimSpace(trimmed[:k])
			}
			b.WriteString(trimmed)
		}
		// 彻底去掉空白：换行位置与缩进差异不是逻辑差异。
		return strings.Join(strings.Fields(b.String()), "")
	}

	var bodies []string
	for _, rel := range frontendFiles {
		src := readFrontend(t, rel)
		body := extract(src, "function gapThreshold(", "function segmentsOf(")
		if body == "" {
			t.Fatalf("%s 里找不到 gapThreshold 函数体", rel)
		}
		bodies = append(bodies, normalize(body))
	}
	if bodies[0] != bodies[1] {
		t.Errorf("两个前端的 gapThreshold 实现已脱节：\n--- %s ---\n%s\n--- %s ---\n%s",
			frontendFiles[0], bodies[0], frontendFiles[1], bodies[1])
	}
}

// 阈值系数必须留在源码里（3 倍中位数）。改小会让正常间隔被当断档
// （曲线被打成虚线），改大则漏判真正的断联。
func TestFrontendGapThresholdIsMedianTimesThree(t *testing.T) {
	for _, rel := range frontendFiles {
		src := readFrontend(t, rel)
		if !strings.Contains(src, "return median * 3;") {
			t.Errorf("%s 的断档阈值不再是中位数 ×3，正常间隔或真断联必有一头被误判", rel)
		}
	}
}
