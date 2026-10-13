package traffic

import (
	"sync"
	"time"

	"github.com/koyoca/xb-probe/internal/model"
)

// 流量跟踪器（文档 4.3 / 13.4.1 / A8）。
//
// 职责：
//   - 按网卡分别求增量（修复 A1 的“全网卡求和”缺陷）。
//   - 维护当前周期已用量，并在周期切换时归零。
//   - 基线至少每 60 秒落盘一次（A8：Master 崩溃后靠累计值差值自然补回）。

// Store 是跟踪器依赖的持久层（由 store.Repo 实现）。
type Store interface {
	LoadNICBaselines(nodeID int64) (map[string]NICBaseline, error)
	SaveNICBaselines(nodeID int64, m map[string]NICBaseline) error
	// LoadTrafficState 返回当前周期已用量；(ok=false) 表示尚无记录。
	LoadTrafficState(nodeID int64) (periodStart, rx, tx int64, ok bool, err error)
	SaveTrafficState(nodeID int64, periodStart, rx, tx int64) error
	// AddTrafficDaily 累加当日流量，day 形如 2026-10-11（节点时区）。
	AddTrafficDaily(nodeID int64, day string, rx, tx int64) error
}

// FlushInterval 是基线与周期用量的落盘间隔（A8：不超过 60 秒）。
const FlushInterval = 60 * time.Second

type nodeAcc struct {
	baselines   map[string]NICBaseline
	periodStart int64
	rx, tx      int64
	dirty       bool
	lastFlush   time.Time
}

// Tracker 是内存态流量累加器。
type Tracker struct {
	store Store
	// Billing 提供每节点的重置日与时区（返回 ok=false 表示用默认值）。
	Billing func(nodeID int64) (resetDay int, tz string, ok bool)

	mu   sync.Mutex
	accs map[int64]*nodeAcc
}

// NewTracker 创建跟踪器。
func NewTracker(store Store, billing func(nodeID int64) (int, string, bool)) *Tracker {
	return &Tracker{store: store, Billing: billing, accs: make(map[int64]*nodeAcc)}
}

// ensureLocked 取出节点的内存累加器，没有就从库里装载一份。调用方需持有 t.mu。
//
// 基线必须**一起装载**：只造一个空的 baselines 表会让紧随其后的第一次上报
// 把每张网卡都当成"首次见到"（ComputeDelta 规则 1 → 增量为 0），
// 白白丢掉一个上报间隔的流量。
func (t *Tracker) ensureLocked(nodeID int64, now time.Time) *nodeAcc {
	if acc, ok := t.accs[nodeID]; ok {
		return acc
	}
	baselines, err := t.store.LoadNICBaselines(nodeID)
	if err != nil || baselines == nil {
		baselines = map[string]NICBaseline{}
	}
	acc := &nodeAcc{baselines: baselines, lastFlush: now}
	ps, r, x, found, _ := t.store.LoadTrafficState(nodeID)
	if found {
		acc.periodStart, acc.rx, acc.tx = ps, r, x
	}
	t.accs[nodeID] = acc
	return acc
}

// billingLoc 解析节点的计费时区与重置日。
//
// Billing 允许为 nil（部分测试只关心增量规则），此时退回"每月 1 日 UTC"。
func (t *Tracker) billingLoc(nodeID int64) (*time.Location, int) {
	resetDay, tz := 0, ""
	if t.Billing != nil {
		resetDay, tz, _ = t.Billing(nodeID)
	}
	loc := time.UTC
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	return loc, resetDay
}

// periodStart 返回节点在 now 所处的计费周期起点（Unix 秒）。
func (t *Tracker) periodStart(nodeID int64, now time.Time) int64 {
	loc, resetDay := t.billingLoc(nodeID)
	start, _ := PeriodWindow(now, resetDay, loc)
	return start.Unix()
}

// Add 处理一次上报的网卡累计值，返回本次增量（已按计量口径折算由调用方决定）。
func (t *Tracker) Add(nodeID int64, nics map[string]model.NICCounters, bootTime int64, counterBits int, now time.Time) (rx, tx int64, resetPeriod bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	acc := t.ensureLocked(nodeID, now)

	// 周期切换检测。
	loc, resetDay := t.billingLoc(nodeID)
	start, _ := PeriodWindow(now, resetDay, loc)
	if acc.periodStart != start.Unix() {
		if acc.periodStart != 0 {
			resetPeriod = true
			acc.rx, acc.tx = 0, 0
		}
		acc.periodStart = start.Unix()
		acc.dirty = true
	}

	for name, cur := range nics {
		prev := acc.baselines[name]
		d := ComputeDelta(prev, cur, bootTime, counterBits)
		rx += d.Rx
		tx += d.Tx
		acc.baselines[name] = NICBaseline{
			LastRx: cur.Rx, LastTx: cur.Tx,
			BootTime: bootTime, UpdatedAt: now.Unix(), Valid: true,
		}
		if d.Rx != 0 || d.Tx != 0 {
			acc.dirty = true
		}
	}

	acc.rx += rx
	acc.tx += tx
	if rx != 0 || tx != 0 {
		day := now.In(loc).Format("2006-01-02")
		_ = t.store.AddTrafficDaily(nodeID, day, rx, tx)
	}

	if now.Sub(acc.lastFlush) >= FlushInterval {
		_ = t.flushLocked(nodeID, acc, now)
	}
	return rx, tx, resetPeriod
}

// flushLocked 落盘基线与周期用量。调用方需持有 t.mu。
func (t *Tracker) flushLocked(nodeID int64, acc *nodeAcc, now time.Time) error {
	if !acc.dirty {
		acc.lastFlush = now
		return nil
	}
	if err := t.store.SaveNICBaselines(nodeID, acc.baselines); err != nil {
		return err
	}
	if err := t.store.SaveTrafficState(nodeID, acc.periodStart, acc.rx, acc.tx); err != nil {
		return err
	}
	acc.dirty = false
	acc.lastFlush = now
	return nil
}

// Flush 强制落盘全部节点的累积量（正常退出时调用）。
func (t *Tracker) Flush(now time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	var first error
	for nodeID, acc := range t.accs {
		if err := t.flushLocked(nodeID, acc, now); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Used 返回某节点当前周期已用量（内存值）。
func (t *Tracker) Used(nodeID int64) (rx, tx int64, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	acc, ok := t.accs[nodeID]
	if !ok {
		return 0, 0, false
	}
	return acc.rx, acc.tx, true
}

// SetUsage 覆盖某节点当前周期的原始计量值（后台手工矫正，4.3）。
//
// ★ 必须**同时**改内存态。Tracker 是运行期的权威，traffic_state 只是它的
// 落盘副本：只写库的话，下一次 flushLocked（≤60 秒）会拿内存里的旧值把矫正值
// 原样写回去。管理员会看到"保存成功"、随后数字自己变回原样，而且全程无报错。
//
// ★ periodStart 一并按"现在"重算。矫正值的语义是"本周期已用这么多"，
// 若沿用记录里的旧周期起点（比如这台机器上个月报过一次就再没上报），
// 紧随其后的第一次上报会命中周期切换分支，把矫正值当作上一周期的余量清零。
//
// ★ ensureLocked 会把这个节点登记进 accs，即使它从未上报过。这是有意的：
// 调度器的 refreshRuntimeBilling 每 60 秒用 tracker.Used 重算看板上的用量，
// Used 对没有累加器的节点返回 0——不登记的话，"矫正一台离线节点"的效果
// 活不过一个慢循环。
func (t *Tracker) SetUsage(nodeID, rx, tx int64, now time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	acc := t.ensureLocked(nodeID, now)
	acc.rx, acc.tx = rx, tx
	acc.periodStart = t.periodStart(nodeID, now)
	// 只落 traffic_state：网卡基线没被碰过，不必连带重写一遍。
	// 顺带避免把长期离线节点的基线 updated_at 抬到"现在"——那会让
	// CleanupNICBaselines 的 7 天判定永远追不上它们。
	return t.store.SaveTrafficState(nodeID, acc.periodStart, acc.rx, acc.tx)
}

// Drop 移除节点内存态（归档/删除时调用）。
func (t *Tracker) Drop(nodeID int64) {
	t.mu.Lock()
	delete(t.accs, nodeID)
	t.mu.Unlock()
}
