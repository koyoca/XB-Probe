package state

import (
	"testing"

	"github.com/koyoca/xb-probe/internal/model"
)

// TestNetStackOf 覆盖协议栈标识的四种组合。
//
// 这条看着琐碎，但"双栈到底输出成 v4+v6 还是 v6+v4"一旦两处写法不一致，
// 公开看板与后台详情就会给出不同答案，而这种差异在 UI 上很难归因。
func TestNetStackOf(t *testing.T) {
	cases := []struct {
		v4, v6 bool
		want   string
	}{
		{false, false, ""},
		{true, false, "v4"},
		{false, true, "v6"},
		{true, true, "v4+v6"},
	}
	for _, c := range cases {
		if got := netStackOf(c.v4, c.v6); got != c.want {
			t.Errorf("netStackOf(%v, %v) = %q，期望 %q", c.v4, c.v6, got, c.want)
		}
	}
}

// TestSyncMetaIPFlags 验证 SyncMeta 会把"有没有 IP"同步进运行态，
// 并且后续同步（比如只改分组）不会把之前置上的标识清掉。
func TestSyncMetaIPFlags(t *testing.T) {
	g := NewRegistry(8)
	g.SyncMeta(1, "pub00000001", "n1", "g", 0, true, false, true, true, "")
	n, ok := g.byNode[1]
	if !ok {
		t.Fatal("节点未创建")
	}
	if !n.HasIPv4 || !n.HasIPv6 {
		t.Fatalf("首次同步后协议栈标识丢失：v4=%v v6=%v", n.HasIPv4, n.HasIPv6)
	}
	// 只改名称，不再传 IP 信息也不该清空——调用方必须显式传 false 才算撤销。
	g.SyncMeta(1, "pub00000001", "改名", "g", 0, true, false, false, false, "")
	if n.HasIPv4 || n.HasIPv6 {
		t.Fatalf("显式传 false 未生效：v4=%v v6=%v", n.HasIPv4, n.HasIPv6)
	}
}

// TestBuildLatencyBars 覆盖点阵聚合的关键行为：
// 分段对齐、段内求平均、空段输出 nil（无数据 != 0）、**按目标一行**、
// 行序 = 运营组顺序 → target id、名字随行输出。
func TestBuildLatencyBars(t *testing.T) {
	const segs = int(LatencyBarsSegments)
	step := LatencyBarsWindowS / int64(segs)
	now := int64(1_800_000_000)
	// 对齐到与实现相同的段边界（to = now - now%step）。
	to := now - now%step
	from := to - LatencyBarsWindowS

	i64 := func(v int64) *int64 { return &v }

	buckets := []model.LatencyBucket{
		// 电信-上海：第一段两条（求平均），第二段整轮失败（RTT=nil 但 Loss 有值）。
		{TargetID: 11, Name: "电信-上海", Carrier: "telecom", TS: from + 10, RTT: i64(40000), Loss: 0},
		{TargetID: 11, Name: "电信-上海", Carrier: "telecom", TS: from + 20, RTT: i64(50000), Loss: 200},
		{TargetID: 11, Name: "电信-上海", Carrier: "telecom", TS: from + step + 10, RTT: nil, Loss: 10000},
		// 移动-广州：只有一段，验证多目标各行输出。
		{TargetID: 13, Name: "移动-广州", Carrier: "mobile", TS: from + 30, RTT: i64(210000), Loss: 0},
	}
	bars := BuildLatencyBars(buckets, now, LatencyBarsWindowS, LatencyBarsSegments)
	if len(bars) != 2 {
		t.Fatalf("应输出 2 行（telecom/mobile 各一条目标），实得 %d", len(bars))
	}
	if bars[0].Carrier != "telecom" || bars[1].Carrier != "mobile" {
		t.Fatalf("行序应为固定的 telecom→mobile，实得 %s → %s", bars[0].Carrier, bars[1].Carrier)
	}
	if bars[0].Name != "电信-上海" || bars[1].Name != "移动-广州" {
		t.Fatalf("目标名应随行输出，实得 %q / %q", bars[0].Name, bars[1].Name)
	}
	tb := bars[0]
	if len(tb.RTT) != segs || len(tb.Loss) != segs {
		t.Fatalf("每行应恒为 %d 段，实得 rtt=%d loss=%d", segs, len(tb.RTT), len(tb.Loss))
	}
	if tb.RTT[0] == nil || *tb.RTT[0] != 45000 {
		t.Fatalf("第一段 RTT 应为两条的平均 45000，实得 %v", tb.RTT[0])
	}
	if tb.RTT[1] != nil {
		t.Fatalf("第二段整轮失败应输出 nil（不能读成 0 假装健康），实得 %v", *tb.RTT[1])
	}
	if tb.Loss[1] == nil || *tb.Loss[1] != 10000 {
		t.Fatalf("第二段丢包应为 10000，实得 %v", tb.Loss[1])
	}
	if tb.RTT[3] != nil || tb.Loss[3] != nil {
		t.Fatal("没有任何桶的段必须输出 nil（不能沿用上一段补位）")
	}
	// 窗口外的桶必须被丢弃。
	outside := append([]model.LatencyBucket{}, buckets...)
	outside = append(outside, model.LatencyBucket{TargetID: 11, Name: "电信-上海", Carrier: "telecom", TS: from - 10, RTT: i64(999999), Loss: 0})
	for _, b := range BuildLatencyBars(outside, now, LatencyBarsWindowS, LatencyBarsSegments) {
		for i, v := range b.RTT {
			if v != nil && *v == 999999 {
				t.Fatalf("窗口外的桶混进了第 %d 段", i)
			}
		}
	}
	// "custom" 运营组也要能单独成行。
	onlyCustom := []model.LatencyBucket{{TargetID: 21, Name: "专线-内网", Carrier: "custom", TS: from + 10, RTT: i64(30000), Loss: 0}}
	cb := BuildLatencyBars(onlyCustom, now, LatencyBarsWindowS, LatencyBarsSegments)
	if len(cb) != 1 || cb[0].Carrier != "custom" || cb[0].Name != "专线-内网" {
		t.Fatalf("仅 custom 时应输出一行 custom，实得 %+v", cb)
	}
}

// TestBuildLatencyBarsPerTarget 是"按目标一行"的行为护栏：
//
// 旧实现按**运营组**聚合，同一运营组下的多条专属目标会被混成一行，
// 行标签也只能是"电信/联通/移动/自定义"四个常量——管理员起的目标名
// （真正的可读标识）完全到不了前端。改成按 target_id 聚合后：
// 同运营组的多条目标各占一行，行序 = 运营组顺序 → target id。
func TestBuildLatencyBarsPerTarget(t *testing.T) {
	step := LatencyBarsWindowS / int64(LatencyBarsSegments)
	now := int64(1_800_000_000)
	to := now - now%step
	from := to - LatencyBarsWindowS
	i64 := func(v int64) *int64 { return &v }

	buckets := []model.LatencyBucket{
		// 两条 custom 专属目标（不同 target）：必须各占一行，不许合并。
		{TargetID: 41, Name: "专线-内网A", Carrier: "custom", TS: from + 10, RTT: i64(10000), Loss: 0},
		{TargetID: 42, Name: "专线-内网B", Carrier: "custom", TS: from + 10, RTT: i64(90000), Loss: 0},
		// 全局电信目标：运营组顺序应排在 custom 之前。
		{TargetID: 7, Name: "电信-上海", Carrier: "telecom", TS: from + 10, RTT: i64(30000), Loss: 0},
		// 同名同运营组的不同目标（后台"同步到多节点"会造出这种数据，
		// 但同一节点上同名不同 id 也可能：名字不参与去重，id 才是键）。
		{TargetID: 43, Name: "专线-内网A", Carrier: "custom", TS: from + 10, RTT: i64(50000), Loss: 0},
	}
	bars := BuildLatencyBars(buckets, now, LatencyBarsWindowS, LatencyBarsSegments)
	if len(bars) != 4 {
		t.Fatalf("4 条不同 target 应输出 4 行（不按运营组合并），实得 %d：%+v", len(bars), bars)
	}
	want := []struct{ name, carrier string }{
		{"电信-上海", "telecom"}, // telecom 先于 custom
		{"专线-内网A", "custom"}, // id 41
		{"专线-内网B", "custom"}, // id 42
		{"专线-内网A", "custom"}, // id 43（同 id 才聚合；同名的不同 id 各自成行）
	}
	for i, w := range want {
		if bars[i].Name != w.name || bars[i].Carrier != w.carrier {
			t.Fatalf("第 %d 行应为 %s/%s，实得 %s/%s", i, w.name, w.carrier, bars[i].Name, bars[i].Carrier)
		}
	}
	// 41/43 同名不同 id：各自的值不能串。
	if bars[1].RTT[0] == nil || *bars[1].RTT[0] != 10000 {
		t.Fatalf("id41 第一段应为 10000，实得 %v", bars[1].RTT[0])
	}
	if bars[3].RTT[0] == nil || *bars[3].RTT[0] != 50000 {
		t.Fatalf("id43 第一段应为 50000（同名不同 id 不得混值），实得 %v", bars[3].RTT[0])
	}
	// 未知运营组不丢数据，排在已知的后面。
	odd := []model.LatencyBucket{{TargetID: 51, Name: "怪组", Carrier: "whatever", TS: from + 10, RTT: i64(20000), Loss: 0}}
	ob := BuildLatencyBars(odd, now, LatencyBarsWindowS, LatencyBarsSegments)
	if len(ob) != 1 || ob[0].Carrier != "whatever" {
		t.Fatalf("未知运营组应保留为一行，实得 %+v", ob)
	}
}

// TestLatencyBarLayout 钉住 settings.public.lat_window → 聚合参数的映射。
//
// 这张表是**唯一口径**：调度器据此决定段数、每点秒数与数据源表。
// 三件事都必须对：
//  1. WindowS 能被 Segments 整除——BuildLatencyBars 会把窗口向下对齐到段边界，
//     除不尽时段长被截断，点阵与真实时间就错位了；
//  2. 短窗口（30m/1h）必须标 Raw=true。10 分钟层在这两个窗口下只有 3/6 个桶，
//     画 30 个点几乎全是灰格——这正是"改了时间窗却看不到数据"的成因；
//  3. 未知取值必须落回 24h（历史行为），绝不能返回零值——零值会让窗口变成 0 秒。
func TestLatencyBarLayout(t *testing.T) {
	cases := []struct {
		window   string
		windowS  int64
		segments int
		raw      bool
	}{
		{"30m", 1800, 30, true},
		{"1h", 3600, 30, true},
		{"6h", 21600, 36, false},
		{"24h", 86400, 32, false},
		{"", 86400, 32, false},      // 设置项缺失
		{"bogus", 86400, 32, false}, // 未知取值
	}
	for _, c := range cases {
		got := LatencyBarLayout(c.window)
		if got.WindowS != c.windowS || got.Segments != c.segments || got.Raw != c.raw {
			t.Errorf("LatencyBarLayout(%q) = %+v，期望 {WindowS:%d Segments:%d Raw:%v}",
				c.window, got, c.windowS, c.segments, c.raw)
			continue
		}
		if got.Segments <= 0 || got.WindowS%int64(got.Segments) != 0 {
			t.Errorf("LatencyBarLayout(%q)：WindowS=%d 不能被 Segments=%d 整除",
				c.window, got.WindowS, got.Segments)
		}
	}
	// ReadFrom 必须比窗口起点再早一段：窗口起点会被对齐到段边界，
	// 只查 now-WindowS 会漏掉最旧那一段（24h 下 45 分钟）→ 点阵最左一格恒灰。
	lay := LatencyBarLayout("24h")
	const now = int64(1_800_000_000)
	if got, want := lay.ReadFrom(now), now-lay.WindowS-lay.Step(); got != want {
		t.Errorf("ReadFrom(%d) = %d，期望 %d", now, got, want)
	}
	if lay.ReadFrom(now) >= now-lay.WindowS {
		t.Error("ReadFrom 没有留出对齐余量：最旧一段会永远显示为「无数据」")
	}
}

// TestBuildLatencyBarsHonoursWindow 是时间窗参数化的**行为**护栏。
//
// 曾经 BuildLatencyBars 的签名加了 windowS/segments，函数体却仍用包级常量
// 算段长与下标上界（常量对应 24h/32 段 = 2700 秒/点）。30 分钟窗口下：
// 取样区间宽成 4500 秒、idx 的分母还是 2700 → 30 个点里只有前 2 个可能被填上，
// 前端拿到的 step 也仍是"每个点约 45 分钟"。签名改了、行为没改，
// 只看函数签名或常量的静态护栏完全抓不到——所以这里直接喂两份样本、
// 断言它们落在**不同的段**且段长等于窗口算出来的值。
func TestBuildLatencyBarsHonoursWindow(t *testing.T) {
	lay := LatencyBarLayout("1h") // 3600s / 30 段 = 每点 120 秒
	const now = int64(1_800_000_000)
	step := lay.Step()
	to := now - now%step
	from := to - lay.WindowS

	i64 := func(v int64) *int64 { return &v }
	buckets := []model.LatencyBucket{
		// 最旧一段（段 0）
		{TargetID: 7, Name: "电信-上海", Carrier: "telecom", TS: from + 10, RTT: i64(30000), Loss: 0},
		// 最新一段（段 29）——段长若仍是 2700，这两条会挤进同一段，
		// 段 29 变成 nil，点阵看起来"只有最左边一格有数据"。
		{TargetID: 7, Name: "电信-上海", Carrier: "telecom", TS: from + 29*step + 5, RTT: i64(90000), Loss: 0},
		// 刚越过对齐后窗口末尾的一条（落在"第 segments 段"）：必须被干净丢弃。
		// 它专门盯住下标上界——若上界写回了包级常量 LatencyBarsSegments(32)，
		// 这一条会落到下标 30 上、直接越界，而不是像现在这样被静默跳过。
		{TargetID: 7, Name: "电信-上海", Carrier: "telecom", TS: to + 1, RTT: i64(123456), Loss: 0},
	}
	bars := BuildLatencyBars(buckets, now, lay.WindowS, lay.Segments)
	if len(bars) != 1 {
		t.Fatalf("应输出 1 行，实得 %d", len(bars))
	}
	b := bars[0]
	if b.Step != step {
		t.Errorf("输出 step = %d，期望 %d（必须是本次窗口的段长，不是包级常量推出来的 2700）",
			b.Step, step)
	}
	if len(b.RTT) != lay.Segments || len(b.Loss) != lay.Segments {
		t.Fatalf("点数应为 %d，实得 RTT=%d Loss=%d", lay.Segments, len(b.RTT), len(b.Loss))
	}
	if b.RTT[0] == nil || *b.RTT[0] != 30000 {
		t.Errorf("段 0 应为 30000，实得 %v（最旧一段被对齐余量吃掉了？）", b.RTT[0])
	}
	if b.RTT[lay.Segments-1] == nil || *b.RTT[lay.Segments-1] != 90000 {
		t.Errorf("段 %d 应为 90000，实得 %v（段长退化成 2700 时两条样本会挤进同一段）",
			lay.Segments-1, b.RTT[lay.Segments-1])
	}
	// 中间段一个桶都没有 → 必须是 nil（"无数据"），不能沿用前后段的值。
	if b.RTT[15] != nil {
		t.Errorf("无数据的段 15 应为 nil，实得 %v", *b.RTT[15])
	}
	// 窗口外那条（123456）不得出现在任何一段里。
	for i, v := range b.RTT {
		if v != nil && *v == 123456 {
			t.Errorf("段 %d 收下了越过窗口末尾的样本：下标上界没有跟着 segments 走", i)
		}
	}
}
