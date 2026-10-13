package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 写队列约束（4.2 / 13.4.8）。
const (
	// DefaultQueueSize 是有界写队列容量。满时丢弃最旧的“高精度”行并计数（A6）。
	DefaultQueueSize = 10000
	// batchMaxRows 达到该行数即提交。
	batchMaxRows = 200
	// batchMaxWait 或达到该时长即提交。
	batchMaxWait = 5 * time.Second
)

// Row 是投入写队列的一行。Table 必须是 allowedTables 中的表名。
type Row struct {
	Table   string
	NodeID  int64
	Metrics *MetricsRow
	Latency *LatencyRow
}

// 允许写入的表（白名单，防止表名注入）。
var allowedTables = map[string]bool{
	"metrics_raw": true,
	"metrics_1m":  true,
	"metrics_10m": true,
	"latency_raw": true,
	"latency_10m": true,
}

// 高精度层表（队列满时优先丢弃）。
var highPrecisionTables = map[string]bool{
	"metrics_raw": true,
	"latency_raw": true,
}

// Stats 是写入管线的运行统计，展示在后台“存储统计”页（13.4.8）。
type Stats struct {
	Queued    int64 // 当前队列长度
	Written   int64 // 累计写入行数
	Dropped   int64 // 因队列满丢弃的行数
	Paused    bool  // 是否因磁盘水位暂停时序写入
	LastFlush int64 // 最近一次提交时间
}

// DiskFreeFunc 返回指定路径剩余空间（MB）。磁盘水位低于阈值时暂停写入。
type DiskFreeFunc func() int64

// Writer 是唯一写协程：每 batchMaxWait 或攒满 batchMaxRows 提交一个事务。
type Writer struct {
	db       *sql.DB
	queue    chan Row
	diskFree DiskFreeFunc
	diskMin  int64 // MB

	stmts map[string]*sql.Stmt

	written atomic.Int64
	dropped atomic.Int64
	paused  atomic.Bool
	lastFl  atomic.Int64

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewWriter 创建写协程。diskFree 可为 nil（不启用磁盘水位保护）。
// diskMinMB 对应设置项 disk_min_free_mb（默认 200）。
func NewWriter(db *sql.DB, queueSize int, diskFree DiskFreeFunc, diskMinMB int64) (*Writer, error) {
	if queueSize <= 0 {
		queueSize = DefaultQueueSize
	}
	w := &Writer{
		db:       db,
		queue:    make(chan Row, queueSize),
		diskFree: diskFree,
		diskMin:  diskMinMB,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	if err := w.prepare(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) prepare() error {
	w.stmts = make(map[string]*sql.Stmt, len(allowedTables))
	for t := range allowedTables {
		s, err := w.db.Prepare(insertSQL(t))
		if err != nil {
			return fmt.Errorf("prepare %s: %w", t, err)
		}
		w.stmts[t] = s
	}
	return nil
}

// insertSQL 生成 INSERT OR IGNORE 语句（13.4.2：桶已有行则丢弃补报样本）。
func insertSQL(table string) string {
	switch table {
	case "metrics_raw", "metrics_1m", "metrics_10m":
		return `INSERT OR IGNORE INTO ` + table + `(
			node_id, ts, cpu, cpu_max, load1, mem_used, swap_used, disk_used,
			rx_bps, tx_bps, rx_bps_max, tx_bps_max, conns)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`
	case "latency_raw", "latency_10m":
		return `INSERT OR IGNORE INTO ` + table + `(
			node_id, target_id, ts, rtt_avg, rtt_min, rtt_max, loss)
			VALUES(?,?,?,?,?,?,?)`
	}
	return ""
}

// Start 启动写协程。
func (w *Writer) Start() { go w.loop() }

// Enqueue 把一行投入队列。队列满时丢弃最旧的高精度行（不阻塞采集）。
func (w *Writer) Enqueue(r Row) {
	if !allowedTables[r.Table] {
		return
	}
	if w.paused.Load() {
		return // 磁盘水位保护：暂停时序写入（13.4.8）
	}
	select {
	case w.queue <- r:
		return
	default:
	}
	// 队列满：尝试腾出一个位置，优先丢弃最旧的高精度行。
	if w.drainOneHighPrecision() {
		select {
		case w.queue <- r:
			return
		default:
		}
	}
	w.dropped.Add(1)
}

// drainOneHighPrecision 从队列头部弹出若干行，遇到高精度行即停止。
func (w *Writer) drainOneHighPrecision() bool {
	for i := 0; i < 64; i++ {
		select {
		case old := <-w.queue:
			if highPrecisionTables[old.Table] {
				w.dropped.Add(1)
				return true
			}
			// 非高精度行放回的机会很小，直接计入丢弃并继续找。
			select {
			case w.queue <- old:
			default:
				w.dropped.Add(1)
			}
		default:
			return false
		}
	}
	return false
}

// shouldPause 判断当前是否应因磁盘水位暂停时序写入（13.4.8）。
// diskFree 为 nil 时永不暂停；diskFree 返回 0 表示“探测不到”，按不暂停处理
// （否则探测失败会把整个时序写入永久停掉，比磁盘爆掉更难恢复）。
func (w *Writer) shouldPause() bool {
	if w.diskFree == nil {
		return false
	}
	free := w.diskFree()
	return free > 0 && free < w.diskMin
}

func (w *Writer) loop() {
	defer close(w.doneCh)

	ticker := time.NewTicker(batchMaxWait)
	defer ticker.Stop()

	batch := make([]Row, 0, batchMaxRows)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := w.commit(batch); err != nil {
			// 写入失败：计入丢弃并记录；不阻塞后续批次。
			w.dropped.Add(int64(len(batch)))
		} else {
			w.written.Add(int64(len(batch)))
			w.lastFl.Store(time.Now().Unix())
		}
		batch = batch[:0]
	}

	for {
		w.paused.Store(w.shouldPause())

		select {
		case <-w.stopCh:
			// 正常退出：排空队列后强制 flush（4.2 第 5 条）。
			for {
				select {
				case r := <-w.queue:
					batch = append(batch, r)
					if len(batch) >= batchMaxRows {
						flush()
					}
					continue
				default:
				}
				break
			}
			flush()
			return

		case r := <-w.queue:
			batch = append(batch, r)
			if len(batch) >= batchMaxRows {
				flush()
			}

		case <-ticker.C:
			flush()
		}
	}
}

// commit 在单个事务中提交一批行（4.2 第 3 条）。
func (w *Writer) commit(batch []Row) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, r := range batch {
		stmt, ok := w.stmts[r.Table]
		if !ok {
			continue
		}
		var execErr error
		if r.Metrics != nil {
			m := r.Metrics
			_, execErr = tx.StmtContext(ctx, stmt).Exec(
				r.NodeID, m.TS, m.CPU, m.CPUMax, m.Load1, m.MemUsed, m.SwapUsed, m.DiskUsed,
				m.RxBps, m.TxBps, m.RxBpsMax, m.TxBpsMax, m.Conns)
		} else if r.Latency != nil {
			l := r.Latency
			_, execErr = tx.StmtContext(ctx, stmt).Exec(
				r.NodeID, l.TargetID, l.TS, l.RTTAvg, l.RTTMin, l.RTTMax, l.Loss)
		}
		if execErr != nil {
			if isIgnorable(execErr) {
				continue
			}
			return fmt.Errorf("exec %s: %w", r.Table, execErr)
		}
	}
	return tx.Commit()
}

// isIgnorable 判断是否是可忽略的约束冲突（OR IGNORE 已处理，这里兜底）。
func isIgnorable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "constraint")
}

// Stop 停止写协程并等待排空完成。
func (w *Writer) Stop() {
	w.stopOnce.Do(func() { close(w.stopCh) })
	<-w.doneCh
	_ = w.closeStmts()
}

func (w *Writer) closeStmts() error {
	var first error
	for _, s := range w.stmts {
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Stats 返回当前统计快照。
func (w *Writer) Stats() Stats {
	return Stats{
		Queued:    int64(len(w.queue)),
		Written:   w.written.Load(),
		Dropped:   w.dropped.Load(),
		Paused:    w.paused.Load(),
		LastFlush: w.lastFl.Load(),
	}
}
