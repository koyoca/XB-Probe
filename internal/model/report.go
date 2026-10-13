// Package model 定义 Master 与 Agent 之间的协议结构，
// 以及 Master 内部的领域模型。对应设计文档 7.1（Agent API）与 8.1。
//
// 单位约定（7.0 / 6.1）：时间为 Unix 秒（UTC）；百分比与负载 ×100；
// 流量为字节；延迟为微秒。可选字段用指针表示“缺失”（如 Windows 无 load）。
package model

// NICCounters 是单块网卡的累计字节数（13.4.1）。只包含经过滤后的网卡。
type NICCounters struct {
	Rx int64 `json:"rx"`
	Tx int64 `json:"tx"`
}

// Metrics 是一个采样点的指标。
type Metrics struct {
	CPU      *int                   `json:"cpu"`   // 百分比 ×100
	Load1    *int                   `json:"load1"` // ×100；Windows 为空
	MemUsed  *int64                 `json:"mem_used"`
	SwapUsed *int64                 `json:"swap_used"`
	DiskUsed *int64                 `json:"disk_used"`
	RxBps    *int64                 `json:"rx_bps"` // 所有统计网卡之和
	TxBps    *int64                 `json:"tx_bps"`
	Conns    *int                   `json:"conns"`
	BootTime *int64                 `json:"boot_time"`
	NICs     map[string]NICCounters `json:"nics"` // 按网卡累计值（A1 修订后取代 rx_total/tx_total）
}

// LatencySample 是某个探测目标一轮探测的汇总。
// 全部失败时 RTT 为 nil，Error 给出枚举原因（13.4.4）。
type LatencySample struct {
	TargetID int    `json:"target_id"`
	RTTAvg   *int64 `json:"rtt_avg"` // 微秒
	RTTMin   *int64 `json:"rtt_min"`
	RTTMax   *int64 `json:"rtt_max"`
	Loss     int    `json:"loss"` // 丢包率 ×100（0~10000）
	Error    string `json:"error,omitempty"`
}

// 探测错误枚举（13.4.4）。
const (
	ProbeErrTimeout          = "timeout"
	ProbeErrRefused          = "refused"
	ProbeErrUnreachable      = "unreachable"
	ProbeErrDNSFailed        = "dns_failed"
	ProbeErrICMPNotPermitted = "icmp_not_permitted"
	ProbeErrBlockedTarget    = "blocked_target"
	ProbeErrOther            = "other"
)

// 延迟状态档位（公开看板据此上色）。
const (
	LatencyOK      = "ok"
	LatencyWarn    = "warn"
	LatencyBad     = "bad"
	LatencyUnknown = "unknown"
)

// 档位阈值（微秒）。
const (
	latencyWarnUS = 50_000  // ≥ 50ms 记为 warn
	latencyBadUS  = 150_000 // ≥ 150ms 记为 bad
)

// LatencyStatus 把一次探测结果映射为状态档位。
//
// 放在 model 里而不是各调用点各写一份：调度器（实时指示灯）与公开 API
// （历史窗口统计）必须用同一套阈值，否则同一条链路会在两处显示成不同颜色。
// unknown 与 bad 的区别是"数据可不可信"——探测本身报错（超时/拒绝/DNS 失败）
// 说明这个数字根本拿不到，而 bad 至少探通了只是慢。
func LatencyStatus(rttAvg *int64, errText string) string {
	if errText != "" {
		return LatencyUnknown
	}
	if rttAvg == nil {
		return LatencyBad
	}
	switch {
	case *rttAvg < latencyWarnUS:
		return LatencyOK
	case *rttAvg < latencyBadUS:
		return LatencyWarn
	default:
		return LatencyBad
	}
}

// Sample 是一个采样点。正常情况下一次上报只含 1 条；
// 断网恢复后按 ts 升序批量补报（13.4.2）。
type Sample struct {
	TS      int64           `json:"ts"`
	Metrics Metrics         `json:"metrics"`
	Latency []LatencySample `json:"latency,omitempty"`
}

// Static 是仅在 Agent 启动及静态信息变化时携带的静态信息。
type Static struct {
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Kernel       string `json:"kernel"`
	Arch         string `json:"arch"`
	CPUModel     string `json:"cpu_model"`
	CPUCores     int    `json:"cpu_cores"`
	MemTotal     int64  `json:"mem_total"`
	SwapTotal    int64  `json:"swap_total"`
	DiskTotal    int64  `json:"disk_total"`
	AgentVersion string `json:"agent_version"`
	Virt         string `json:"virt"` // kvm/xen/lxc/openvz/docker/wsl/none
}

// ReportRequest 是 POST /api/agent/v1/report 的请求体（≤256 KB，samples ≤100）。
type ReportRequest struct {
	AgentVersion string   `json:"agent_version"`
	Samples      []Sample `json:"samples"`
	Static       *Static  `json:"static,omitempty"`
	// PublicIP 是 Agent 自己探测到的公网地址（agent.public_ip_url，未配置时为空）。
	//
	// 为什么由 Agent 报而不是 Master 从连接里取：Master 看到的来源地址有两个
	// 常见的失真场景 —— Agent 与 Master 同机（来源是 127.0.0.1）、前置了没有列入
	// trusted_proxies 的反代（来源是代理地址）。这两种情况下 Master 记下来的
	// 都是错的，而"我是谁"只有节点自己出网问一句才知道。
	// Master 侧只把它当作**候选值**：只有当节点还没被手工指定过地址（ip_auto=1）
	// 且这个值确实是公网地址时才会写进 nodes.ip_v4。
	PublicIP string `json:"public_ip,omitempty"`
}

// ReportResponse 携带 config_version，Agent 据此判断是否需要拉取配置（3.1 / 8.1）。
type ReportResponse struct {
	OK            bool   `json:"ok"`
	ServerTime    int64  `json:"server_time"`
	ConfigVersion string `json:"config_version"`
}

// Target 是下发给 Agent 的探测目标（纯声明式数据）。
type Target struct {
	ID        int    `json:"id"`
	Name      string `json:"name,omitempty"`
	Host      string `json:"host"`
	Port      int    `json:"port,omitempty"` // method=tcp 时必填
	Method    string `json:"method"`         // tcp | icmp
	TimeoutMs int    `json:"timeout_ms"`
	Count     int    `json:"count"` // 每轮探测次数，≤5（13.4.6）
}

// AgentConfig 是 GET /api/agent/v1/config 的响应体。
type AgentConfig struct {
	ConfigVersion   string   `json:"config_version"`
	ReportIntervalS int      `json:"report_interval_s"`
	ProbeIntervalS  int      `json:"probe_interval_s"`
	NICInclude      []string `json:"nic_include,omitempty"`
	NICExclude      []string `json:"nic_exclude"`
	Targets         []Target `json:"targets"`
}

// EnrollRequest 是 POST /api/agent/v1/enroll 的请求体（12.2 新增环境字段）。
type EnrollRequest struct {
	Code           string `json:"code"`
	Hostname       string `json:"hostname"`
	OS             string `json:"os"`   // 如 Debian 12
	Arch           string `json:"arch"` // amd64/arm64/...
	Virt           string `json:"virt"`
	ServiceManager string `json:"service_manager"` // systemd/openrc/procd/sysv/launchd/rcd/windows/none
	CounterBits    int    `json:"counter_bits"`    // 32 或 64
	AgentVersion   string `json:"agent_version"`
}

// EnrollResponse 仅在注册成功时返回一次令牌（3.4 / 9.1）。
type EnrollResponse struct {
	NodeID int64       `json:"node_id"`
	Token  string      `json:"token"` // pk_ 前缀，256 位随机
	Config AgentConfig `json:"config"`
}

// APIError 是统一错误格式（7.0）：{"error":{"code":"invalid_token","message":"..."}}
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorEnvelope 是错误响应外层。
type ErrorEnvelope struct {
	Error APIError `json:"error"`
}

// 常用错误码（7.0）。
const (
	ErrInvalidToken  = "invalid_token"
	ErrTokenRevoked  = "token_revoked"
	ErrNodeArchived  = "node_archived"
	ErrInvalidCode   = "invalid_code"
	ErrRateLimited   = "rate_limited"
	ErrInvalidParam  = "invalid_param"
	ErrUnauthorized  = "unauthorized"
	ErrForbidden     = "forbidden"
	ErrNotFound      = "not_found"
	ErrPayloadTooBig = "payload_too_large"
	ErrInternal      = "internal_error"
)
