package model

import (
	"encoding/json"
	"sort"
	"strings"
)

// 本文件是公开 API 的 DTO 白名单（文档 5.1 / 7.3 / S1）。
//
// 安全要点：公开响应的结构中“根本不存在”IP、主机名、内核/CPU 型号、令牌、
// 价格等字段——安全由类型系统保证，而不是“查出来再过滤”。参见 9.4。
//
// 本文件另提供一层运行期收窄（settings.public.fields，7.3）：管理员可以把
// 白名单里的任意字段关掉，被关掉的字段不会出现在 JSON 中。这只是“在已经安全
// 的集合里再缩小”，不是安全边界本身——真正的边界是上面的类型约束。

// PublicSite 对应 GET /api/public/v1/site。
type PublicSite struct {
	Title   string `json:"title"`
	Theme   string `json:"theme"`
	Meta    string `json:"meta,omitempty"` // 注入的 meta（仅管理员可写）
	Version string `json:"version,omitempty"`
	// Cards 是允许展示的公开卡片键列表（settings.public.cards，7.3）。
	//
	// 三种取值必须能区分开，因此这个字段**不加 omitempty**：
	//   null  —— 不限制，全部展示（值为 "all"）
	//   []    —— 一张都不展示（管理员取消了全部勾选）
	//   [...] —— 只展示列出的这些
	// 若加了 omitempty，空切片会被序列化掉，前端拿到 undefined 只能当成
	// "不限制"，于是"全部关掉"退化成"全部打开"。
	//
	// 键是**渲染用的标识**，不含任何数据，因此不受 public.fields 掩码影响。
	Cards []string `json:"cards"`
	// LatWindow 是节点卡片点阵的时间窗（settings.public.lat_window）：
	// "30m" / "1h" / "6h" / "24h"。前端用它生成"延迟 / 丢包 · 近 X"的标题，
	// 并按窗口换算悬浮提示里的"每个点约多久"。空值视作 "24h"（旧快照兼容）。
	LatWindow string `json:"lat_window,omitempty"`
}

// PublicOverview 对应 GET /api/public/v1/overview。
// 由内存快照提供，避免每次请求现算（A9 / 13.4.8）。
type PublicOverview struct {
	NodesOnline        int    `json:"nodes_online"`
	NodesTotal         int    `json:"nodes_total"`
	RxBps              int64  `json:"rx_bps"` // 聚合实时带宽
	TxBps              int64  `json:"tx_bps"`
	RxUsed             int64  `json:"rx_used"` // 本周期下载量（字节）
	TxUsed             int64  `json:"tx_used"` // 本周期上传量
	TrafficUsed        int64  `json:"traffic_used"`
	TodayRx            int64  `json:"today_rx"` // 今日下载量（traffic_daily 当日累计）
	TodayTx            int64  `json:"today_tx"`
	MonthlyFee         int64  `json:"monthly_fee"`          // 所有公开节点的月费合计（最小货币单位）
	MonthlyFeeCurrency string `json:"monthly_fee_currency"` // 币种（取第一个非空币种；多币种时自行判断）
	GeneratedAt        int64  `json:"generated_at"`         // 快照时间

	mask PublicFieldMask
}

// WithFieldMask 返回按掩码收窄后的副本（nil 掩码表示不过滤）。
func (o PublicOverview) WithFieldMask(m PublicFieldMask) PublicOverview {
	o.mask = m
	return o
}

// MarshalJSON 在掩码生效时剔除未允许的键。
func (o PublicOverview) MarshalJSON() ([]byte, error) {
	type plain PublicOverview // 去掉方法集，避免递归；未导出字段本就不会序列化
	return o.mask.apply(json.Marshal(plain(o)))
}

// PublicNode 对应 GET /api/public/v1/nodes 与 /nodes/{public_id}。
// 字段为默认公开白名单（7.3）；实际输出再按 settings 的 public.fields 收窄。
// public_visible=0 的节点不会出现在列表中。
type PublicNode struct {
	// --- 核心字段：不可关闭，前端没有它们就无法渲染 ---
	PublicID string `json:"public_id"`
	Name     string `json:"name"`
	Status   string `json:"status"` // online/warning/offline/archived

	// --- 可选字段：可由 public.fields 收窄 ---
	Group string `json:"group,omitempty"`
	OS    string `json:"os,omitempty"`
	Arch  string `json:"arch,omitempty"` // x86_64/aarch64/…（不含内核版本、CPU 型号）
	Virt  string `json:"virt,omitempty"` // kvm/lxc/docker/…，按 13.4.4 长度与字符集清洗

	CPUCores int `json:"cpu_cores"`
	CPUPct   int `json:"cpu_pct"` // ×100
	Load1    int `json:"load1"`   // 1 分钟负载 ×100；Windows 采集器无此值时为 0

	MemUsed  int64 `json:"mem_used"` // 字节
	MemTotal int64 `json:"mem_total"`
	MemPct   int   `json:"mem_pct"`
	SwapPct  int   `json:"swap_pct"`

	DiskUsed  int64 `json:"disk_used"`
	DiskTotal int64 `json:"disk_total"`
	DiskPct   int   `json:"disk_pct"`

	RxBps  int64 `json:"rx_bps"` // 实时速率（字节/秒）
	TxBps  int64 `json:"tx_bps"`
	RxUsed int64 `json:"rx_used"` // 本周期累计（字节）
	TxUsed int64 `json:"tx_used"`

	TrafficUsed    int64 `json:"traffic_used"`
	TrafficQuota   int64 `json:"traffic_quota"`    // 0 表示不限
	TrafficUsedPct int   `json:"traffic_used_pct"` // 0~10000；0 表示不限

	// TodayRx/TodayTx 是节点本地时区下"今天"的下载/上传量。
	//
	// 这两个键一直写在 publicFieldOrder 里（可在后台按节点白名单收窄），
	// 但结构体上没有对应字段——于是白名单里躺着一个永远不生效的开关，
	// 而看板也没法显示"今日流量"。补上字段，白名单与实际输出才对得上。
	TodayRx int64 `json:"today_rx"`
	TodayTx int64 `json:"today_tx"`

	// NetStack 只描述节点具备哪些 IP 协议栈（"v4" / "v6" / "v4+v6"，都为空时省略），
	// 用于在节点卡片上显示 IPv4/IPv6 标识，不含任何地址。
	//
	// 键名刻意不含 ip 子串：公开 DTO 的字段黑名单（dto_leak_test.go）把 "ip" 当作
	// 疑似地址泄露的子串护栏，命中即视为回归失败。这里输出的只是协议栈版本、
	// 不是地址，但仍沿用不含 ip 的键名，以免把护栏的告警意义稀释掉。
	NetStack string `json:"net_stack,omitempty"`

	// Country 是节点的国家/地区代码（ISO 3166-1 alpha-2，如 "HK"/"JP"），
	// 供节点卡片渲染国旗标识；未设置时为空串（前端不画旗帜）。
	//
	// 它与 Location（管理端自由文本，被 dto_leak_test 的字段黑名单挡在公开侧外）
	// 是两码事：这里只发一个两位代码，既不含机房名也不含供应商，
	// 而且它属于 public.fields 的可收窄字段——管理员不想公开节点所在国家时可关掉。
	Country string `json:"country,omitempty"`

	// LatencyBars 是节点卡片上的"探测目标延迟情况"点阵（时间窗由
	// public.lat_window 设置决定，默认近 24 小时）。
	//
	// 每条探测目标一行：把窗口均分成若干段，每段给出平均 RTT 与平均丢包率，
	// 前端据此画两行小点（延迟一行、丢包一行），悬浮可看该段时间与数值。
	// 聚合由调度器周期写进运行态，快照构建纯内存拷贝——绝不能在快照路径查库。
	LatencyBars []PublicLatencyBar `json:"lat_bars,omitempty"`

	// MonthlyFee 是该节点折算到**月均**的续费价格（最小货币单位），
	// 与「月费合计」同一折算口径（model.MonthlyEquivalent：年付÷12、季付÷3、
	// once/lifetime → 0）。是否在卡片上渲染由 public.cards 的 node_fee 开关
	// 控制；数据是否下发由 public.fields 的 monthly_fee 键控制（互补，见
	// public_cards.go 顶部的分工说明）。
	//
	// 键名沿用 monthly_fee：公开 DTO 的字段黑名单禁的是 price/cost 子串，
	// overview 早已用这个键输出月费合计，节点级沿用同一个名字。
	MonthlyFee int64 `json:"monthly_fee"`
	// MonthlyFeeCurrency 是该节点计费币种（ISO 4217，如 CNY）；未设置时省略。
	MonthlyFeeCurrency string `json:"monthly_fee_currency,omitempty"`
	// FeeCycle 是计费周期原值（monthly/quarterly/yearly/once/lifetime）。
	// 月均折算会把 once/lifetime 折成 0，前端需要周期原值才能把"终身/一次性"
	// 与"没配价格"区分开（两者都是 0，但文案完全不同）。
	FeeCycle string `json:"fee_cycle,omitempty"`

	UptimeS int64 `json:"uptime_s"`
	// UpdatedAt 是最近一次上报时间（Unix 秒），前端据此换算离线时长。
	UpdatedAt  int64           `json:"updated_at"`
	ExpireDays *int            `json:"expire_days,omitempty"` // nil 表示永久/未设置
	Latency    []PublicLatency `json:"latency"`
	// 说明：价格仅在管理员在白名单中显式放开时才追加输出（4.4 / 7.3）。

	mask PublicFieldMask
}

// WithFieldMask 返回按掩码收窄后的副本（nil 掩码表示不过滤）。
func (n PublicNode) WithFieldMask(m PublicFieldMask) PublicNode {
	n.mask = m
	return n
}

// MarshalJSON 在掩码生效时剔除未允许的键。
func (n PublicNode) MarshalJSON() ([]byte, error) {
	type plain PublicNode
	return n.mask.apply(json.Marshal(plain(n)))
}

// PublicLatencyBar 是节点卡片"延迟情况"点阵的一行。
//
// 一行对应**一条探测目标**（而不是一个运营组）：目标名是管理员起的名字
// （"电信-上海"、"专线-内网"），比四个运营组常量能表达的信息多得多；
// carrier 保留用于排序与"名字里读不出运营商"时的兜底文案。
//
// RTT[i] / Loss[i] 与第 i 个时间段一一对应：段起点 = From + i*Step。
// 两者都允许为 nil：该段一次都没探通（要和"测到了且为 0"区分开，
// 与项目里"缺值渲染 — 而不是 0"的约定同源）。Loss 单位为 ×100（0~10000）。
type PublicLatencyBar struct {
	Name    string   `json:"name,omitempty"` // 探测目标名（管理员可读名，非敏感字段）
	Carrier string   `json:"carrier"`        // telecom/unicom/mobile/custom
	From    int64    `json:"from"`           // 窗口起点（Unix 秒）
	Step    int64    `json:"step"`           // 每段秒数
	RTT     []*int64 `json:"rtt"`            // 每段平均 RTT（微秒）
	Loss    []*int64 `json:"loss"`           // 每段平均丢包率 ×100
}

// LatencyBucket 是点阵聚合的输入：某节点某目标在一个 10 分钟桶里的均值。
// 放在 model 是因为 store 负责读、state 负责聚合，两边都要认识它。
// TargetID 是聚合键（同行 = 同一条目标）；Name/Carrier 来自 JOIN 的目标行。
type LatencyBucket struct {
	TargetID int64
	Name     string
	Carrier  string
	TS       int64
	RTT      *int64 // 微秒；nil = 该桶整轮探测失败
	Loss     int64  // ×100
}

// PublicLatency 是按运营商（电信/联通/移动/自定义）汇总的延迟指示灯。
//
// 这是"一眼看三网"的粗粒度指标，只含汇总值；逐个目标的曲线与窗口统计
// 由 PublicLatencySeries（/nodes/{id}/latency）提供。两者分工明确：
// 前者随快照每秒更新、纯内存构建，后者按需查库、带时间范围。
type PublicLatency struct {
	Carrier string `json:"carrier"` // telecom/unicom/mobile/custom
	RTTAvg  *int64 `json:"rtt_avg"` // 该运营商下各目标的平均 RTT（微秒）
	Loss    int    `json:"loss"`    // 该运营商下最差的平均丢包率 ×100
	Status  string `json:"status"`  // ok/warn/bad/unknown，取最差
	Targets int    `json:"targets"` // 参与汇总的目标数
}

// PublicHistoryPoint 是历史曲线的一个点（GET /nodes/{public_id}/history）。
type PublicHistoryPoint struct {
	TS    int64 `json:"ts"`
	Value int64 `json:"value"` // 单位取决于 metrics 参数
}

// PublicSeries 是按指标分组的历史序列，最多返回 500 点（7.3 / S9）。
type PublicSeries struct {
	Metric string               `json:"metric"` // cpu/mem/net
	Unit   string               `json:"unit"`
	Points []PublicHistoryPoint `json:"points"`
}

// PublicLatencySeries 是单个探测目标的延迟曲线与窗口统计
// （GET /nodes/{public_id}/latency，7.3）。
//
// 说明：只暴露目标的**名称**（后台自己填的标签，如"电信-上海"），
// 不暴露探测目标的 host/port —— 公开接口一律不含主机信息，与节点 DTO 同一原则。
// 名称属于管理员自填内容，与节点 name 同级，同样按不可信字符串清洗后输出。
type PublicLatencySeries struct {
	TargetID int64                `json:"target_id"`
	Name     string               `json:"name"`
	Carrier  string               `json:"carrier"` // telecom/unicom/mobile/custom
	Points   []PublicHistoryPoint `json:"points"`  // value 为 rtt_avg（微秒）

	// 以下为本次查询窗口内的统计；窗口内一次都没探通时 RTT 为 null。
	Last   *int64 `json:"last_rtt"`
	Avg    *int64 `json:"avg_rtt"`
	Min    *int64 `json:"min_rtt"`
	Max    *int64 `json:"max_rtt"`
	Loss   int    `json:"loss"`   // 窗口平均丢包率 ×100
	Status string `json:"status"` // ok/warn/bad/unknown
}

// ---------------------------------------------------------------------------
// public.fields 运行期收窄
// ---------------------------------------------------------------------------

// PublicFieldMask 是一组允许输出的 JSON 键。
// nil 表示不过滤（全部允许）；非 nil 时只有值为 true 的键会输出。
type PublicFieldMask map[string]bool

// publicCoreFields 是任何情况下都必须输出的键：没有它们前端无法渲染一条记录。
// 注意它们是“渲染所必需”，不是“敏感”——它们本来就在公开白名单里。
var publicCoreFields = map[string]bool{
	"public_id":    true,
	"name":         true,
	"status":       true,
	"nodes_online": true,
	"nodes_total":  true,
	"generated_at": true,
}

// publicFieldOrder 是可由 public.fields 收窄的字段全集，顺序即后台展示顺序。
var publicFieldOrder = []string{
	"group",
	"os", "arch", "virt",
	"cpu_cores", "cpu_pct", "load1",
	"mem_used", "mem_total", "mem_pct", "swap_pct",
	"disk_used", "disk_total", "disk_pct",
	"rx_bps", "tx_bps", "rx_used", "tx_used",
	"traffic_used", "traffic_quota", "traffic_used_pct",
	"today_rx", "today_tx", "monthly_fee", "monthly_fee_currency", "fee_cycle",
	"net_stack", "country", "lat_bars",
	"uptime_s", "expire_days", "latency", "updated_at",
}

// PublicFieldNames 返回可收窄字段的全集（按后台展示顺序）。
func PublicFieldNames() []string {
	out := make([]string, len(publicFieldOrder))
	copy(out, publicFieldOrder)
	return out
}

// DefaultPublicFields 返回默认全部放开时的字段列表字符串。
func DefaultPublicFields() string {
	return strings.Join(publicFieldOrder, ",")
}

// ParsePublicFields 解析逗号/空白分隔的字段列表。
//
// 宽容策略（避免手滑把看板改空）：
//   - 输入为空或全部是未知字段 → 返回 nil，表示"不过滤"。
//   - 未知字段名直接忽略；大小写与空格不敏感。
func ParsePublicFields(s string) PublicFieldMask {
	known := make(map[string]bool, len(publicFieldOrder))
	for _, f := range publicFieldOrder {
		known[f] = true
	}
	m := PublicFieldMask{}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == ';'
	}) {
		f := strings.ToLower(strings.TrimSpace(part))
		if known[f] {
			m[f] = true
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// FormatPublicFieldList 把掩码还原为逗号分隔的列表（按固定顺序，便于比对）。
func FormatPublicFieldList(m PublicFieldMask) string {
	if m == nil {
		return DefaultPublicFields()
	}
	out := make([]string, 0, len(m))
	for _, f := range publicFieldOrder {
		if m[f] {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// apply 按掩码剔除未允许的键。收到上游 marshal 的错误时原样返回。
func (m PublicFieldMask) apply(b []byte, err error) ([]byte, error) {
	if err != nil || m == nil {
		return b, err
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil {
		// 理论上不会发生；退化为不过滤，绝不因为收窄逻辑本身让接口 500。
		return b, nil
	}
	for k := range obj {
		if !publicCoreFields[k] && !m[k] {
			delete(obj, k)
		}
	}
	return json.Marshal(obj)
}
