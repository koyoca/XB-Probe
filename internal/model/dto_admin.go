package model

// 本文件是管理 API 的补充 DTO（文档 5.2：全量资产视察）。
//
// 与公开 DTO 相反：这里包含真实 IP、主机名、内核、CPU 型号、令牌状态等敏感信息，
// 因此只能由 /api/admin/v1（会话 + CSRF）返回，绝不进入公开响应。

// AdminNodeRuntime 是节点的实时运行态（取内存注册表，不落盘，4.2）。
// 供管理后台展示“此刻”的负载与探测结果，与 DB 中粘滞的 status 字段互补。
type AdminNodeRuntime struct {
	NodeID      int64  `json:"node_id"`
	PublicID    string `json:"public_id"`
	Status      string `json:"status"`
	LastSeen    int64  `json:"last_seen"`
	UptimeS     int64  `json:"uptime_s"`
	SuspectFrom int64  `json:"suspect_since"`
	Archived    bool   `json:"archived"`

	// 指标（与 6.1 单位约定一致）
	CPUPct   int   `json:"cpu_pct"`  // ×100
	Load1    int   `json:"load1"`    // ×100
	MemUsed  int64 `json:"mem_used"` // 字节
	MemTotal int64 `json:"mem_total"`
	MemPct   int   `json:"mem_pct"` // ×100
	SwapUsed int64 `json:"swap_used"`
	SwapPct  int   `json:"swap_pct"`
	DiskUsed int64 `json:"disk_used"`
	DiskPct  int   `json:"disk_pct"`
	RxBps    int64 `json:"rx_bps"`
	TxBps    int64 `json:"tx_bps"`
	Conns    int   `json:"conns"`

	// 流量与资产
	TrafficQuota   int64 `json:"traffic_quota"`
	TrafficUsed    int64 `json:"traffic_used"`
	TrafficUsedPct int   `json:"traffic_used_pct"`
	// PeriodRx/PeriodTx 是当前计费周期的下载/上传量（运营口径，未按计量模式折算）。
	// TrafficUsed 是按 traffic_mode 折算后的计费用量（down/up/max/both），
	// 两者含义不同：看板上"周期流量 ↓↑"要的是前者，进度条要的是后者。
	PeriodRx int64 `json:"period_rx"`
	PeriodTx int64 `json:"period_tx"`
	// TodayRx/TodayTx 是节点本地时区下"今天"的下载/上传量（来自 traffic_daily）。
	TodayRx  int64 `json:"today_rx"`
	TodayTx  int64 `json:"today_tx"`
	ExpireAt int64 `json:"expire_at"`

	// 环境
	BootTime      int64  `json:"boot_time"`
	CounterBits   int    `json:"counter_bits"`
	Virt          string `json:"virt,omitempty"`
	OS            string `json:"os,omitempty"`
	ConfigVersion string `json:"config_version,omitempty"`

	// 最新一轮探测结果（含错误枚举，13.4.4）
	Latency []LatencySample `json:"latency"`
}

// AdminProbeLatency 是管理端单目标延迟序列（GET /nodes/{id}/latency）。
type AdminProbeLatency struct {
	TargetID int64                `json:"target_id"`
	Name     string               `json:"name,omitempty"`
	Host     string               `json:"host,omitempty"`
	Method   string               `json:"method,omitempty"`
	Carrier  string               `json:"carrier,omitempty"`
	Enabled  bool                 `json:"enabled"`
	Points   []PublicHistoryPoint `json:"points"`

	// 窗口统计，口径与公开接口 PublicLatencySeries 完全一致：
	// 后台详情页与公开详情页要显示同一套"最小/最大/平均/丢包"，
	// 两边各算一套的话，管理员看到的和访客看到的会对不上。
	// 窗口内一次都没探通时 RTT 四项为 null（序列化成 null 而不是 0）。
	Last   *int64 `json:"last_rtt"`
	Avg    *int64 `json:"avg_rtt"`
	Min    *int64 `json:"min_rtt"`
	Max    *int64 `json:"max_rtt"`
	Loss   int    `json:"loss"`   // 窗口平均丢包率 ×100
	Status string `json:"status"` // ok/warn/bad/unknown
}
