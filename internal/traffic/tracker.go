package traffic

import (
	"sync"
	"time"

	"github.com/yourorg/probe/internal/model"
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

// Add 处理一次上报的网卡累计值，返回本次增量（已按计量口径折算由调用方决定）。
func (t *Tracker) Add(nodeID int64, nics map[string]model.NICCounters, bootTime int64, counterBits int, now time.Time) (rx, tx int64, resetPeriod bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	acc, ok := t.accs[nodeID]
	if !ok {
		baselines, err := t.store.LoadNICBaselines(nodeID)
		if err != nil || baselines == nil {
			baselines = map[string]NICBaseline{}
		}
		ps, r, x, found, _ := t.store.LoadTrafficState(nodeID)
		acc = &nodeAcc{baselines: baselines, lastFlush: now}
		if found {
			acc.periodStart = ps
			acc.rx, acc.tx = r, x
		}
		t.accs[nodeID] = acc
	}

	// 周期切换检测。
	resetDay, tz, _ := t.Billing(nodeID)
	loc := time.UTC
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
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

// Drop 移除节点内存态（归档/删除时调用）。
func (t *Tracker) Drop(nodeID int64) {
	t.mu.Lock()
	delete(t.accs, nodeID)
	t.mu.Unlock()
}
