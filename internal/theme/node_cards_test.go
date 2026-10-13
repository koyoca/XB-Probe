package theme

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 节点卡片区块开关的回归护栏。
//
// 背景缺陷：`public.cards` 过去只有「总览汇总卡」「详情页信息小卡」
// 「节点卡的延迟点阵」三类键，**节点卡片上的 CPU / 内存 / 磁盘 / 流量
// 四条进度条与底部实时速率行根本没有开关**——makeCard/updateCard 无条件
// 渲染。于是管理员在「系统设置 → 公开看板」取消勾选 info_mem 之后，
// 详情页那格确实消失了，列表页每张节点卡片上的「内存」进度条却还在，
// 看起来就是"开关关不掉"。
//
// 修法：给节点卡片的每个区块补一个卡片键，前端按 cardOn 逐块隐藏。
//
// 为什么是源码级护栏：与 gap_test.go 同一理由（不引入 Node 依赖）。
// 这里尤其重要的是**键名登记完整**——少登记一个键，那一块就永远关不掉，
// 而这正是当初缺陷的形态。

// nodeBlockKeys 是节点卡片上的区块键，必须全部被前端识别。
//
// node_fee 是后加的"续费价格"行（2026-10-12）：它与 CPU/内存/磁盘/流量一样是
// **全局开关**，缺价格时渲染 "—" 而不是隐藏整行——一旦按节点隐藏，
// 同一行网格里的卡片高度就会重新参差。
var nodeBlockKeys = []string{
	"node_cpu", "node_mem", "node_disk", "node_traffic", "node_fee", "node_net",
}

// 公开看板前端必须为每个区块键做显隐判断。
func TestNodeCardBlocksAreGated(t *testing.T) {
	src := readFrontend(t, "internal/theme/builtin/app.js")
	for _, k := range nodeBlockKeys {
		if !strings.Contains(src, "cardOn('"+k+"')") {
			t.Errorf("app.js 没有对 %q 做 cardOn 判断：该区块将永远显示，开关无效", k)
		}
	}
}

// 四条进度条与速率行都必须挂上 data-card，否则没有可关的抓手。
func TestNodeCardBlocksCarryDataCardAttr(t *testing.T) {
	src := readFrontend(t, "internal/theme/builtin/app.js")
	for _, k := range nodeBlockKeys {
		if !strings.Contains(src, "'data-card', '"+k+"'") {
			t.Errorf("app.js 里 %q 没有挂 data-card 属性", k)
		}
	}
}

// 区块显隐必须在 updateCard 里被调用——只定义 applyNodeCardBlocks 而不调用
// 等于没修（这是最典型的"改了但没生效"）。
func TestUpdateCardAppliesBlockGating(t *testing.T) {
	src := readFrontend(t, "internal/theme/builtin/app.js")
	ui := strings.Index(src, "function updateCard(")
	if ui < 0 {
		t.Fatal("找不到 updateCard")
	}
	// updateCard 函数体（到下一个顶层 function 为止）。
	rest := src[ui:]
	end := strings.Index(rest[1:], "\n  function ")
	if end < 0 {
		t.Fatal("无法界定 updateCard 函数体")
	}
	body := rest[:end]
	if !strings.Contains(body, "applyNodeCardBlocks(c)") {
		t.Error("updateCard 没有调用 applyNodeCardBlocks，节点卡片区块开关不会生效")
	}
}

// 管理员改完开关后，已建好的卡片必须立刻跟上，而不是等下一轮轮询
// （3 秒后才消失，看起来像"保存没生效"）。
func TestApplyCardsUpdatesExistingCards(t *testing.T) {
	src := readFrontend(t, "internal/theme/builtin/app.js")
	i := strings.Index(src, "function applyCards(")
	if i < 0 {
		t.Fatal("找不到 applyCards")
	}
	rest := src[i:]
	end := strings.Index(rest[1:], "\n  function ")
	if end < 0 {
		end = len(rest) - 1
	}
	body := rest[:end]
	if !strings.Contains(body, "applyNodeCardBlocks(cards[id])") {
		t.Error("applyCards 没有对已建好的卡片重新应用区块显隐，开关会慢半拍")
	}
}

// 一条进度条都不剩时整条 .n-bars 必须隐藏，否则每张卡片都会留下一道空壳。
func TestBarsContainerHiddenWhenAllBlocksOff(t *testing.T) {
	src := readFrontend(t, "internal/theme/builtin/app.js")
	if !strings.Contains(src, "c.bars.hidden = !(cardOn('node_cpu')") {
		t.Error("app.js 没有处理「四条进度条全部关闭」的情形，会留下带边框的空壳")
	}
}

// 节点卡片的区块键必须进入后端的卡片目录（model.PublicCardList），
// 否则后台勾选框里根本没有它们，管理员无从开关。
//
// 这条断言读的是 model 包源码而不是直接调函数：theme 包不应依赖 model 包
// （主题是通用渲染层，不该知道管理后台的数据模型）。
func TestNodeBlockKeysRegisteredInModel(t *testing.T) {
	p := filepath.Clean(filepath.Join("..", "model", "public_cards.go"))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", p, err)
	}
	src := string(b)
	for _, k := range nodeBlockKeys {
		if !strings.Contains(src, `{"`+k+`", PublicCardGroupNode`) {
			t.Errorf("model/public_cards.go 没有把 %q 登记到节点卡片分组：后台勾选框里不会有它", k)
		}
	}
}
