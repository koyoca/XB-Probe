package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// public.fields 运行期收窄（7.3）的回归测试。
//
// 要守住的契约：
//  1. 掩码为 nil（默认）时输出全部白名单字段——不能因为收窄逻辑而少输出。
//  2. 掩码非 nil 时，未列出的字段必须从 JSON 中彻底消失（不是置零后仍出现）。
//  3. 核心字段（public_id / name / status）永远不能被关掉，
//     否则前端会渲染出一张没有任何标识的卡片。
//  4. 收窄是在"已经安全的集合"里再缩小，绝不能反过来引入字段。

func samplePublicNode() PublicNode {
	return PublicNode{
		PublicID:       "abc123def456",
		Name:           "香港-01",
		Group:          "hk",
		Status:         "online",
		OS:             "linux",
		Arch:           "x86_64",
		Virt:           "kvm",
		CPUCores:       2,
		CPUPct:         3600,
		Load1:          71,
		MemUsed:        2_633_000_000,
		MemTotal:       5_368_709_120,
		MemPct:         4904,
		SwapPct:        0,
		DiskUsed:       148_335_018_000,
		DiskTotal:      179_314_688_000,
		DiskPct:        8273,
		RxBps:          3_774_873,
		TxBps:          1_782_579,
		RxUsed:         1_826_000_000_000,
		TxUsed:         852_000_000_000,
		TrafficUsed:    4_576_000_000_000,
		TrafficQuota:   1_099_511_627_776,
		TrafficUsedPct: 4162,
		TodayRx:        15_032_385_126,
		TodayTx:        7_623_401_882,
		UptimeS:        2_592_000,
		UpdatedAt:      1_760_000_000,
		MonthlyFee:     4500, MonthlyFeeCurrency: "CNY", FeeCycle: "monthly",
		NetStack: "v4+v6",
		Country:  "HK",
		LatencyBars: []PublicLatencyBar{
			{Carrier: "telecom", From: 1_760_000_000, Step: 2700,
				RTT:  []*int64{pubI64(42000), nil},
				Loss: []*int64{pubI64(0), pubI64(300)}},
		},
		ExpireDays: pubInt(100),
		Latency: []PublicLatency{
			{Carrier: "telecom", RTTAvg: pubPtr(12000), Loss: 0, Status: "ok"},
		},
	}
}

// pubInt 用于构造 *int 可选字段。
func pubInt(v int) *int { return &v }

// pubI64 用于构造 *int64 可选字段（延迟点阵的 RTT/Loss 段值）。
func pubI64(v int64) *int64 { return &v }

func keysOf(t *testing.T, v any) map[string]bool {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("反序列化失败：%v（原文 %s）", err, raw)
	}
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// 掩码为 nil 时行为必须与改造前完全一致：白名单字段一个不少。
//
// 这里以前有一份 nodeOverviewOnly 例外表（today_rx / today_tx 只属于概览）。
// 现在取消该例外：节点详情页也要显示"今日流量"（与后台/看板参考版式一致），
// 而这两个键本来就在共享的 publicFieldOrder 白名单里，
// 加到节点 DTO 上属于"白名单内的字段终于真的输出了"，不是扩大暴露面
// ——它们与 traffic_used / rx_used 同级，都是流量数值。
func TestPublicNodeNoMaskEmitsAllWhitelistedFields(t *testing.T) {
	got := keysOf(t, samplePublicNode())

	// 曾经 monthly_fee / monthly_fee_currency 只在 PublicOverview 上出现；
	// 现在节点卡片要显示单节点月均续费价格，这两个键（以及区分"终身/一次性"
	// 与"没配价格"的 fee_cycle）已经挂到了 PublicNode 上，没有例外键了。
	overviewOnly := map[string]bool{}

	missing := 0
	for _, f := range PublicFieldNames() {
		if overviewOnly[f] {
			continue
		}
		if !got[f] {
			missing++
			t.Errorf("未收窄时字段 %q 必须存在（默认全放开）", f)
		}
	}
	for _, f := range []string{"public_id", "name", "status"} {
		if !got[f] {
			t.Errorf("核心字段 %q 缺失", f)
		}
	}
	if missing > 0 {
		t.Logf("共 %d 个字段缺失", missing)
	}
}

// 概览也必须有白名单里的所有字段。
func TestPublicOverviewNoMaskEmitsAllWhitelistedFields(t *testing.T) {
	o := PublicOverview{
		NodesOnline: 1, NodesTotal: 2,
		RxBps: 1, TxBps: 2,
		RxUsed: 3, TxUsed: 4, TrafficUsed: 5,
		TodayRx: 6, TodayTx: 7,
		MonthlyFee: 1999, MonthlyFeeCurrency: "CNY",
		GeneratedAt: 1_760_000_000,
	}
	got := keysOf(t, o)

	// 这些键只在 PublicNode 上出现，PublicOverview 上不成立，跳过。
	nodeOnly := map[string]bool{
		"group": true, "os": true, "arch": true, "virt": true,
		"cpu_cores": true, "cpu_pct": true, "load1": true,
		"mem_used": true, "mem_total": true, "mem_pct": true, "swap_pct": true,
		"disk_used": true, "disk_total": true, "disk_pct": true,
		"traffic_quota": true, "traffic_used_pct": true,
		"uptime_s": true, "expire_days": true, "latency": true, "updated_at": true,
		// 协议栈标识、国家代码与延迟点阵挂在单个节点上，概览没有对应字段。
		"net_stack": true, "country": true, "lat_bars": true,
		// 计费周期原值同样挂在单个节点上（概览只有月费合计，没有周期概念）。
		"fee_cycle": true,
	}

	missing := 0
	for _, f := range PublicFieldNames() {
		if nodeOnly[f] {
			continue
		}
		if !got[f] {
			missing++
			t.Errorf("未收窄时字段 %q 必须存在（默认全放开）", f)
		}
	}
	for _, f := range []string{"nodes_online", "nodes_total", "generated_at"} {
		if !got[f] {
			t.Errorf("核心字段 %q 缺失", f)
		}
	}
	if missing > 0 {
		t.Logf("共 %d 个字段缺失", missing)
	}
}

// 掩码生效时，未列出的字段必须彻底消失，而不是"仍出现但为 0"。
func TestPublicNodeMaskRemovesFields(t *testing.T) {
	mask := PublicFieldMask{"cpu_pct": true, "mem_pct": true}
	got := keysOf(t, samplePublicNode().WithFieldMask(mask))

	for _, f := range []string{"cpu_pct", "mem_pct"} {
		if !got[f] {
			t.Errorf("已允许的字段 %q 不应被删掉", f)
		}
	}
	for _, f := range []string{"disk_pct", "rx_bps", "traffic_quota", "uptime_s", "latency", "group"} {
		if got[f] {
			t.Errorf("未允许的字段 %q 必须从 JSON 中消失", f)
		}
	}
	// 核心字段不受掩码影响。
	for _, f := range []string{"public_id", "name", "status"} {
		if !got[f] {
			t.Errorf("核心字段 %q 不得被掩码关闭", f)
		}
	}
}

// 掩码里塞核心字段为 false 也不能关掉它们（防手滑把看板改空）。
func TestPublicNodeMaskCannotDropCoreFields(t *testing.T) {
	mask := PublicFieldMask{"cpu_pct": true}
	got := keysOf(t, samplePublicNode().WithFieldMask(mask))
	for _, f := range []string{"public_id", "name", "status"} {
		if !got[f] {
			t.Errorf("核心字段 %q 被关闭了", f)
		}
	}
}

// 概览共用同一套掩码键，保证"关掉某字段"在列表与概览上语义一致。
func TestPublicOverviewMaskRemovesFields(t *testing.T) {
	o := PublicOverview{
		NodesOnline: 2, NodesTotal: 3,
		RxBps: 1024, TxBps: 2048,
		RxUsed: 100, TxUsed: 200, TrafficUsed: 300,
		TodayRx: 10, TodayTx: 20,
		GeneratedAt: 1_760_000_000,
	}
	got := keysOf(t, o.WithFieldMask(PublicFieldMask{"today_rx": true}))

	if !got["today_rx"] {
		t.Error("已允许的 today_rx 不应被删掉")
	}
	for _, f := range []string{"rx_used", "tx_used", "traffic_used", "today_tx", "rx_bps"} {
		if got[f] {
			t.Errorf("未允许的字段 %q 必须消失", f)
		}
	}
	// 节点计数与快照时间是渲染所必需的，不在可收窄范围内。
	for _, f := range []string{"nodes_online", "nodes_total", "generated_at"} {
		if !got[f] {
			t.Errorf("核心字段 %q 不得被掩码关闭", f)
		}
	}
}

// ParsePublicFields：空/全未知 → nil（不过滤），避免手滑把看板改空。
func TestParsePublicFields(t *testing.T) {
	if ParsePublicFields("") != nil {
		t.Error("空串应返回 nil（不过滤）")
	}
	if ParsePublicFields("  ,, ; ") != nil {
		t.Error("只有分隔符应返回 nil（不过滤）")
	}
	if ParsePublicFields("不存在的字段,另一个") != nil {
		t.Error("全部是未知字段时应返回 nil（不过滤），否则看板会被清空")
	}

	m := ParsePublicFields("CPU_PCT, mem_pct ,unknown,disk_pct")
	for _, want := range []string{"cpu_pct", "mem_pct", "disk_pct"} {
		if !m[want] {
			t.Errorf("%q 应被解析出来（大小写与空格不敏感）", want)
		}
	}
	if m["unknown"] || len(m) != 3 {
		t.Errorf("未知字段不应进入掩码：%+v", m)
	}
}

// 掩码与列表字符串可往返，且列表顺序固定（便于后台比对与展示）。
func TestFormatPublicFieldListRoundTrip(t *testing.T) {
	in := "cpu_pct,mem_pct,disk_pct"
	s := FormatPublicFieldList(ParsePublicFields(in))
	if s == "" {
		t.Fatal("不应返回空")
	}
	again := FormatPublicFieldList(ParsePublicFields(s))
	if s != again {
		t.Errorf("往返不稳定：%q -> %q", s, again)
	}
	if strings.Count(s, ",") != 2 {
		t.Errorf("字段个数不对：%q", s)
	}
	// nil 掩码表示全放开。
	if FormatPublicFieldList(nil) != DefaultPublicFields() {
		t.Error("nil 掩码应等同于默认全部字段")
	}
}

// 可收窄字段清单本身不得混入敏感字段名——这是最后一道闸。
func TestPublicFieldNamesAreSafe(t *testing.T) {
	for _, f := range PublicFieldNames() {
		for _, bad := range forbiddenPublicKeys {
			if strings.Contains(f, bad) {
				t.Errorf("可收窄字段清单里出现了敏感字段名 %q（命中禁止子串 %q）", f, bad)
			}
		}
	}
	// 清单与 DefaultPublicFields 必须一致，避免两处漂移。
	names := PublicFieldNames()
	if strings.Join(names, ",") != DefaultPublicFields() {
		t.Errorf("PublicFieldNames 与 DefaultPublicFields 不一致")
	}
	// 不允许核心字段出现在"可收窄"清单里（它们永远输出）。
	for _, f := range names {
		if publicCoreFields[f] {
			t.Errorf("核心字段 %q 不应出现在可收窄清单中", f)
		}
	}
}
