package alert

import (
	"fmt"
	"strings"
)

// 事件状态键的中文说明（唯一的出产地）。
//
// 为什么需要它：alert_events.kind 落库的是**状态键**而不是规则名——
// 简单类型是 `offline`/`cpu`，带参数的复合类型是 `traffic:80`、`expire:30`、
// `probe_fail:12`（见 engine.ObserveKind 的说明：规则按 ruleKind 查、
// 连续计数按 stateKind 分开，所以落库的必然是复合键）。
//
// 管理员在后台看到的是 `expire:30` 这种字符串，而它的含义（"到期前 30 天那一档"）
// 只存在于 scheduler 的几行代码里。这里把它翻译成中文，**由服务端出**，
// 前端只管显示 —— 两边各写一份映射必然在加档位时漏掉一处。
var kindLabels = map[string]string{
	KindOffline:        "离线",
	KindOnline:         "节点上线",
	KindCPU:            "CPU 越阈值",
	KindMem:            "内存越阈值",
	KindDisk:           "磁盘越阈值",
	KindTraffic:        "流量梯级",
	KindExpire:         "到期提醒",
	KindProbeFail:      "探测连续失败",
	KindAuditRetention: "审计保留期变更",
}

// KindLabel 返回状态键的中文说明。
//
//   - `expire:30`      → 到期提醒（剩余 ≤ 30 天）
//   - `expire:0`       → 到期提醒（今天到期）
//   - `traffic:80`     → 流量梯级（已用 ≥ 80%）
//   - `probe_fail:12`  → 探测连续失败（目标 #12）
//
// 认不出的键**原样返回**，不猜也不吞：将来新增一种复合键时，
// 后台宁可显示一个看不懂的 `newkind:7`，也不要显示一句编造的中文。
func KindLabel(kind string) string {
	base, arg, hasArg := strings.Cut(kind, ":")
	label, ok := kindLabels[base]
	if !ok {
		return kind
	}
	if !hasArg {
		return label
	}
	switch base {
	case KindTraffic:
		return fmt.Sprintf("%s（已用 ≥ %s%%）", label, arg)
	case KindExpire:
		if arg == "0" {
			return label + "（今天到期）"
		}
		return fmt.Sprintf("%s（剩余 ≤ %s 天）", label, arg)
	case KindProbeFail:
		return fmt.Sprintf("%s（目标 #%s）", label, arg)
	default:
		return fmt.Sprintf("%s（%s）", label, arg)
	}
}
