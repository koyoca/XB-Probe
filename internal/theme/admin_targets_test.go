package theme

import (
	"strings"
	"testing"
)

// 管理后台「探测目标范围多选」与「分页」的源码级护栏。
//
// 背景（用户上报的需求）：
//
//	4. 节点详情添加专属探测目标那里，可编辑专属探测目标；
//	5. 探测目标页面，编辑专属探测目标，支持范围可多选节点；
//	6. 告警页面中的告警事件、审计日志中的日志，添加分页查询。
//
// 这几条的失败模式都很"安静"——页面不会报错，只是行为不对：
//
//	- 范围列表**可见时**才会提交 node_ids（不可见时提交空数组 = 后端 400），
//	  所以 `if (!$('tm-nodes').hidden)` 这一句是"节点详情页能不能改参数"的开关，
//	  写反了就是"在节点详情页改个端口都存不进去"。
//	- 自身所在节点必须勾上且不可取消（后端会拒绝把目标移出自身节点）。
//	- 分页响应从裸数组换成了信封 {items,total,limit,offset}：漏改一处就会
//	  遍历一个对象，表格空白且只有一行控制台报错。
//	- 页码/总数必须用**服务端回传**的生效窗口，否则越界时页码和内容互相矛盾。

// 范围列表的两种隐藏情形（全局目标 / 节点详情页）都必须走"不提交 node_ids"。
//
// 断言的是那个**条件本身**：只要有人把它改成"总是提交"或"反过来判断"，
// 节点详情页的编辑就会永久 400。
func TestTargetEditorSubmitsScopeOnlyWhenVisible(t *testing.T) {
	src := readFrontend(t, adminJS)
	if !strings.Contains(src, "if (!$('tm-nodes').hidden) {") {
		t.Fatal("编辑弹窗不再按「范围列表是否可见」决定要不要提交 node_ids：" +
			"这会让全局目标与节点详情页的编辑被后端以「范围不能为空」拒掉")
	}
	// 提交的是勾选出来的节点数组（而不是别的字段名）。
	if !strings.Contains(src, "body.node_ids = ids;") {
		t.Error("提交的字段名不是 node_ids")
	}
	// 弹窗里的多选列表必须有 id，否则上面那句 $('tm-nodes') 永远是 undefined。
	if !strings.Contains(readFrontend(t, adminHTML), `id="tm-nodes"`) {
		t.Error("index.html 里没有 #tm-nodes，弹窗的范围多选没有落点")
	}
}

// 自身所在的节点必须被锁住：后端拒绝把目标移出自身节点（会留下孤儿延迟桶），
// 前端要提前锁，而不是让管理员点了保存才看到报错。
func TestTargetScopeLocksOwnerNode(t *testing.T) {
	src := readFrontend(t, adminJS)
	if !strings.Contains(src, "if (n.id === lockedID) i.disabled = true;") {
		t.Error("范围列表没有把「目标自身所在的节点」锁成不可取消")
	}
	if !strings.Contains(src, "i.checked = n.id === lockedID || checked.indexOf(n.id) >= 0;") {
		t.Error("锁定的节点没有被强制勾选——禁用的复选框仍是勾选态，必须显式置 true")
	}
	// 被禁用的复选框也必须算进提交结果里：它本来就该在范围内。
	if !strings.Contains(src, "if (i.checked) out.push(parseInt(i.value, 10));") {
		t.Error("checkedNodeIDs 不是按 i.checked 收集——按 disabled 过滤会把锁定节点漏掉")
	}
}

// 节点详情页的编辑必须带上 lockScope，否则那个页面也能改作用域，
// 而它的语义是"某个节点的视角"。
func TestNodeDetailEditLocksScope(t *testing.T) {
	src := readFrontend(t, adminJS)
	if !strings.Contains(src, "openTargetEditor(x, { lockScope: true })") {
		t.Error("节点详情页的「编辑」没有传 lockScope:true")
	}
	if !strings.Contains(src, "if (opts.lockScope) {") {
		t.Error("openTargetEditor 没有处理 lockScope")
	}
}

// 分页：两个列表都必须走信封，且必须用服务端回传的生效窗口。
func TestPagedListsUseEnvelope(t *testing.T) {
	src := readFrontend(t, adminJS)

	// 不能再用"一次拉 200 条"的老写法。
	for _, bad := range []string{"/alert-events?limit=200", "/audit-log?limit=' + limit"} {
		if strings.Contains(src, bad) {
			t.Errorf("仍然存在不分页的请求：%s", bad)
		}
	}
	for _, want := range []string{
		"api('/alert-events?limit=' + st.limit + '&offset=' + st.offset)",
		"api('/audit-log?limit=' + st.limit + '&offset=' + st.offset)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("缺少分页请求：%s", want)
		}
	}
	// 必须从 items 取数据（响应已不是裸数组）。
	if !strings.Contains(src, "return res.items || [];") {
		t.Error("adoptPage 没有从 res.items 取列表——响应已改成信封，直接遍历会抛异常")
	}
	// 必须把服务端回传的 limit/offset/total 写回本地：越界时页码才不会和内容打架。
	for _, want := range []string{
		"if (typeof res.limit === 'number') st.limit = res.limit;",
		"if (typeof res.offset === 'number') st.offset = res.offset;",
		"if (typeof res.total === 'number') st.total = res.total;",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("分页状态没有采用服务端的生效值：%s", want)
		}
	}
	// 条数上限按服务端 MaxPageSize 走：选项大于 200 时服务端会压回 200，
	// 前端按 1000 算出的页数就全错了。
	if !strings.Contains(src, "var PAGE_SIZES = [20, 50, 100, 200];") {
		t.Error("每页条数选项与服务端 MaxPageSize（200）脱节")
	}
	// 总页数至少为 1：total=0 时不能出现"第 1 / 0 页"。
	if !strings.Contains(src, "Math.max(1, Math.ceil(st.total / st.limit))") {
		t.Error("总页数没有对 0 做兜底，空表会显示「第 1 / 0 页」")
	}
	// 换每页条数要回到第一页（否则会落在越界窗口上）。
	if !strings.Contains(src, "st.offset = 0;") {
		t.Error("切换每页条数后没有重置 offset")
	}
	// 两个列表都要有分页条容器。
	h := readFrontend(t, adminHTML)
	for _, id := range []string{`id="events-pager"`, `id="audit-pager"`} {
		if !strings.Contains(h, id) {
			t.Errorf("index.html 缺少分页条容器 %s", id)
		}
	}
	if !strings.Contains(src, "renderPager($('events-pager'), 'events', loadEvents);") ||
		!strings.Contains(src, "renderPager($('audit-pager'), 'audit', loadAudit);") {
		t.Error("两个列表没有各自接上分页条")
	}
}

// 分页/范围用到的 CSS 必须在位：缺了 .pager 就会挤成一行，
// 缺了 .scope-list 的 max-height，上百个节点会把弹窗撑出视口。
func TestScopeAndPagerCSSPresent(t *testing.T) {
	css := readFrontend(t, adminCSSRel)
	for _, sel := range []string{".scope-list {", ".scope-list > .check-row {", ".pager {"} {
		if !strings.Contains(css, sel) {
			t.Errorf("admin.css 缺少样式 %s", sel)
		}
	}
	// .scope-list 的滚动约束必须真的写在它自己的规则块里。
	// 只断言"文件里出现过 max-height"是不够的——那条规则可能长在别的选择器上。
	i := strings.Index(css, ".scope-list {")
	if i < 0 {
		return
	}
	block := css[i:]
	if j := strings.Index(block, "}"); j > 0 {
		block = block[:j]
	}
	if !strings.Contains(block, "max-height") || !strings.Contains(block, "overflow-y") {
		t.Errorf(".scope-list 缺少 max-height/overflow-y：节点多时会把弹窗撑到超过视口，"+
			"保存按钮滚出屏幕。当前规则：%s", block)
	}
}
