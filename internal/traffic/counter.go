// Package traffic 实现流量计量（文档 4.3 与 A1 / 13.4.1）。
//
// 核心修订：按网卡分别求差，而不是“所有网卡累计值之和求差”。
// 后者在 docker/veth 等虚拟网卡增减时会让总和下降，被误判为重启，
// 从而把新总和全额计入，造成数 GB 级的虚高。
package traffic

import "github.com/yourorg/probe/internal/model"

// counterWrap32 是 32 位计数器的模（4 GiB）。
const counterWrap32 int64 = 1 << 32

// wrapProximity 判定“上次值接近 2^32”。取值 2^31：即上次值已落在 32 位空间的上半区，
// 再涨一点就会回绕。可根据实际情况调整。
const wrapProximity int64 = 1 << 31

// NICBaseline 是某节点某网卡的累计值基线（traffic_nic_state）。
type NICBaseline struct {
	LastRx    int64
	LastTx    int64
	BootTime  int64
	UpdatedAt int64
	// Valid 为 false 表示首次见到该网卡（只记录基线，增量为 0）。
	Valid bool
}

// Delta 是一次上报产生的增量。
type Delta struct {
	Rx int64
	Tx int64
	// Restarted 表示本次按“重启”处理（用于日志与后台提示）。
	Restarted bool
	// Wrapped 表示本次按“32 位回绕”处理。
	Wrapped bool
}

// ComputeDelta 按 13.4.1 的四条规则计算增量。
//  1. 首次见到某网卡：只记录基线，增量为 0。
//  2. 当前值 ≥ 基线：增量 = 当前值 − 基线。
//  3. 当前值 < 基线：boot_time 变化 → 重启，增量 = 当前值；
//     否则 counterBits=32 且基线接近 2^32 → 回绕，增量 = 当前值 + 2^32 − 基线；
//     其他情况 → 计数器被重置，增量 = 当前值。
//  4. 网卡消失：不产生增量（由上层的基线保留策略处理）。
func ComputeDelta(prev NICBaseline, cur model.NICCounters, bootTime int64, counterBits int) Delta {
	if !prev.Valid {
		return Delta{}
	}

	switch {
	case cur.Rx >= prev.LastRx && cur.Tx >= prev.LastTx:
		return Delta{Rx: cur.Rx - prev.LastRx, Tx: cur.Tx - prev.LastTx}

	case bootTime != 0 && prev.BootTime != 0 && bootTime != prev.BootTime:
		// 规则 3a：重启
		return Delta{Rx: cur.Rx, Tx: cur.Tx, Restarted: true}

	case counterBits == 32 &&
		(prev.LastRx >= counterWrap32-wrapProximity || prev.LastTx >= counterWrap32-wrapProximity):
		// 规则 3b：32 位回绕
		return Delta{
			Rx:      cur.Rx + counterWrap32 - prev.LastRx,
			Tx:      cur.Tx + counterWrap32 - prev.LastTx,
			Wrapped: true,
		}

	default:
		// 规则 3c：其他情况按计数器重置
		return Delta{Rx: cur.Rx, Tx: cur.Tx, Restarted: true}
	}
}

// ApplyMode 按计量口径把双向增量折算为一个数值（4.3）。
func ApplyMode(rx, tx int64, mode string) int64 {
	switch mode {
	case model.TrafficUp:
		return tx
	case model.TrafficDown:
		return rx
	case model.TrafficMax:
		if rx > tx {
			return rx
		}
		return tx
	default: // both
		return rx + tx
	}
}

// ApplyMultiplier 把"计量口径"折算成"计费流量"：计费 = 计量 × 倍率（4.4）。
//
// 为什么单独一个函数而不塞进 ApplyMode：两者的语义完全不同，前者是"统计哪些
// 方向的流量"（口径），后者是"按几倍扣量"（倍率）。混在一起以后，任何一处
// 需要单独展示原始计量值（例如"周期流量 ↓↑"卡片）都会算错。
//
// 倍率 <= 0 一律按 1 处理：库里缺列、老数据为 0、前端漏填都可能出现 0，
// 这时"按 1 倍计"是唯一不会把数据变成 0 的合理兜底。
func ApplyMultiplier(used int64, multiplier float64) int64 {
	if multiplier <= 0 {
		return used
	}
	if multiplier == 1 {
		return used
	}
	// 四舍五入到字节：int64(1.5*3) = 4（截断也恰好是 4），但 0.1×15 会得到
	// 1.4999…，截断就少 1 字节，长期累积会让百分比与额度对不上。
	return int64(float64(used)*multiplier + 0.5)
}

// Billable 是一次到位的口径 + 倍率折算，供调用方少写一行。
func Billable(rx, tx int64, mode string, multiplier float64) int64 {
	return ApplyMultiplier(ApplyMode(rx, tx, mode), multiplier)
}

// BaselineCleanupDays 是网卡消失后基线的保留天数（13.4.1 规则 4）。
const BaselineCleanupDays = 7

// Expired 判断基线是否可清理（over = 现在，cutoff = updated_at + 7 天）。
func (b NICBaseline) Expired(now, updatedAt int64) bool {
	return now-updatedAt > BaselineCleanupDays*86400
}
