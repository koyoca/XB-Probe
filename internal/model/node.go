package model

// 节点领域模型（文档 6.3）与状态机（4.1）。

// 节点状态（4.1）。
const (
	StatusOnline   = "online"
	StatusWarning  = "warning"
	StatusOffline  = "offline"
	StatusArchived = "archived"
)

// Node 是 nodes 表的映射。敏感字段（IP 等）仅管理端可见。
//
// 该结构只出现在管理 API（7.2）中；公开 API 使用独立的 PublicNode 白名单 DTO（5.1）。
// 字段名统一为 snake_case，与文档 7.2 的响应约定一致。
type Node struct {
	ID            int64  `json:"id"`
	PublicID      string `json:"public_id"`
	Name          string `json:"name"`
	GroupName     string `json:"group"`
	SortOrder     int    `json:"sort_order"`
	Status        string `json:"status"`
	PublicVisible bool   `json:"public_visible"`

	IPv4     string `json:"ip_v4,omitempty"`
	IPv6     string `json:"ip_v6,omitempty"`
	Provider string `json:"provider,omitempty"`
	Location string `json:"location,omitempty"`
	// IPAuto 为 true 表示 ip_v4/ip_v6 允许被自动探测（Agent 自报的公网 IP、
	// 或 Master 看到的来源地址）改写；管理员手工填过之后会变成 false（"覆盖自动探测"）。
	// 见 migrations/0007_node_ip_auto.sql。
	IPAuto bool `json:"ip_auto"`
	// Country 是节点的国家/地区代码（ISO 3166-1 alpha-2，如 "HK"/"JP"），
	// 用于在节点卡片上显示国旗。与 Location（自由文本、仅管理端可见）不同，
	// 它是**结构化**取值，并且会进入公开 DTO（可被 public.fields 关掉）。
	Country string `json:"country,omitempty"`
	Note    string `json:"note,omitempty"`

	Hostname     string `json:"hostname,omitempty"`
	OS           string `json:"os,omitempty"`
	Kernel       string `json:"kernel,omitempty"`
	Arch         string `json:"arch,omitempty"`
	CPUModel     string `json:"cpu_model,omitempty"`
	CPUCores     int    `json:"cpu_cores"`
	MemTotal     int64  `json:"mem_total"`
	SwapTotal    int64  `json:"swap_total"`
	DiskTotal    int64  `json:"disk_total"`
	BootTime     int64  `json:"boot_time"`
	AgentVersion string `json:"agent_version,omitempty"`

	// 12.7 迁移新增
	OSFamily       string `json:"os_family,omitempty"`
	Virt           string `json:"virt,omitempty"`
	ServiceManager string `json:"service_manager,omitempty"`
	CounterBits    int    `json:"counter_bits"`

	LastSeen   int64 `json:"last_seen"`
	CreatedAt  int64 `json:"created_at"`
	ArchivedAt int64 `json:"archived_at,omitempty"`

	// Billing 仅在"节点详情"（GET /nodes/{id}）里带上，列表接口为 nil 且被
	// omitempty 省略。带上的原因：计费表单必须能回显当前值——表单每次打开都是
	// 空的，管理员改一个字段就会把其余字段悄悄清零。
	Billing *Billing `json:"billing,omitempty"`
}

// Token 是 node_tokens 表的映射。库内只存哈希，明文永不落库（9.1）。
// TokenHash 标了 json:"-"，即便被误序列化也不会泄出哈希（9.4）。
type Token struct {
	ID         int64  `json:"id"`
	NodeID     int64  `json:"node_id"`
	TokenHash  []byte `json:"-"`
	TokenHint  string `json:"hint"` // 前 6 位，仅供后台辨认
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
	LastIP     string `json:"last_ip"`
	RevokedAt  int64  `json:"revoked_at"`
}

// Revoked 表示令牌已吊销。
func (t *Token) Revoked() bool { return t.RevokedAt > 0 }

// Billing 是 node_billing 表的映射（4.4 / 6.4）。
type Billing struct {
	NodeID       int64  `json:"node_id"`
	PriceMinor   int64  `json:"price_minor"`
	Currency     string `json:"currency"`
	Cycle        string `json:"cycle"` // monthly/quarterly/yearly/once/lifetime
	ExpireAt     int64  `json:"expire_at"`
	TrafficQuota int64  `json:"traffic_quota"` // 字节，0=不限
	TrafficMode  string `json:"traffic_mode"`  // both/up/down/max
	ResetDay     int    `json:"reset_day"`     // 1~31
	TZ           string `json:"tz"`            // 空=全局时区
	// TrafficMultiplier 是流量倍率：计费流量 = 计量口径 × 倍率（0 视为 1）。
	TrafficMultiplier float64 `json:"traffic_multiplier"`
}

// 支付周期。
const (
	CycleMonthly   = "monthly"
	CycleQuarterly = "quarterly"
	CycleYearly    = "yearly"
	CycleOnce      = "once"
	CycleLifetime  = "lifetime"
)

// 流量计量口径（4.3）。
const (
	TrafficBoth = "both"
	TrafficUp   = "up"
	TrafficDown = "down"
	TrafficMax  = "max"
)

// ProbeTarget 是 probe_targets 表的映射（6.3）。
// host/port/method 不可原地修改，修改即新建并停用旧目标（A4 / 13.4.6）。
type ProbeTarget struct {
	ID        int64  `json:"id"`
	NodeID    int64  `json:"node_id"` // 0 表示全局目标
	Name      string `json:"name"`
	Carrier   string `json:"carrier"` // telecom/unicom/mobile/custom
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Method    string `json:"method"` // tcp/icmp
	TimeoutMs int    `json:"timeout_ms"`
	Enabled   bool   `json:"enabled"`
	// ShowInBars 只管"公开看板点阵画不画这一行"，与 Enabled（Agent 探不探）
	// 互补：想在后台保留探测、但不对访客展示时用 enabled=1 + show_in_bars=false。
	// 见迁移 0004 与 LatencyBucketsForNode 的过滤条件。
	ShowInBars bool `json:"show_in_bars"`
}
