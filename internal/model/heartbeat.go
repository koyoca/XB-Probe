package model

// 外部心跳（A5）的可观测性 DTO。
//
// 背景：心跳的**发送**逻辑在 cmd/xb-probe-master（调度器）里，而**展示**要在
// 管理后台。两者分属不同的包，且 api 不能 import main。因此这里放两个东西：
//   - AdminHeartbeat：跨包传输的普通结构体（无方法，无依赖）；
//   - HeartbeatReader：api 侧的窄接口，由调度器实现并注入 Deps。
//
// 这比"让 api 直接依赖 main"或"把心跳状态塞进数据库"都更干净：
// 前者是循环/反向依赖，后者会把瞬时状态持久化，重启后残留过期数据。

// AdminHeartbeat 是后台"外部心跳"状态卡的响应体。
//
// 所有时间均为 Unix 秒（UTC），与项目其余接口一致；
// 前端负责按本地时区渲染。0 表示"从未发生"。
type AdminHeartbeat struct {
	// Configured 表示是否已配置 heartbeat_url。
	// 未配置时其余字段无意义，前端应显示"未启用"而不是"失败 N 次"。
	Configured bool `json:"configured"`

	// URL 是当前配置的心跳地址，便于管理员在后台直接核对是否填错。
	URL string `json:"url,omitempty"`

	// IntervalSec 是心跳间隔（秒），用于前端计算"距离下次应该多久"。
	IntervalSec int `json:"interval_sec"`

	// LastAttempt / LastOK 分别是最后一次尝试、最后一次成功的时间（Unix 秒）。
	LastAttempt int64 `json:"last_attempt"`
	LastOK      int64 `json:"last_ok"`

	// FailCount 是自上次成功以来的**连续**失败次数。
	FailCount int `json:"fail_count"`

	// LastStatus 是最后一次的 HTTP 状态码；网络层错误（连不上/超时）为 0。
	LastStatus int `json:"last_status"`

	// LastError 是最后一次失败的简要原因（成功时为空串）。
	// 只出现在需要登录的后台接口，不进公开 DTO。
	LastError string `json:"last_error,omitempty"`

	// Sent 是本次进程运行以来累计成功的次数。
	Sent int64 `json:"sent"`

	// Healthy 是后端算好的综合结论，省得前端各写一套判断。
	// 语义：未配置 -> false；已配置且最近一次成功 -> true；
	//       连续失败 >= 2（即错过了一整个周期）-> false。
	Healthy bool `json:"healthy"`
}

// HeartbeatReader 由 cmd/xb-probe-master 的调度器实现，注入 api.Deps 供后台读取。
//
// 只暴露一个方法：api 层不需要知道心跳是怎么发的，
// 只需要"给我一份当前状态的快照"。
type HeartbeatReader interface {
	// HeartbeatStatus 返回心跳状态快照。
	// 实现方必须自己保证并发安全（发送在调度协程、读取在 HTTP 协程）。
	HeartbeatStatus() AdminHeartbeat
}
