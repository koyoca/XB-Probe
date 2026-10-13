// Package pipeline 实现 Master 的写入管线与多分辨率内存聚合
// （文档 4.2 与 A2 / 13.4.2）。
//
// 流程：上报 → 更新内存运行态 → 同时进入各分辨率聚合桶（均值 + 峰值）
//
//	→ 桶闭合产出一行 → 投入写队列 → 单写协程批量事务提交。
//
// 与 v1 的差异：落盘的是聚合结果；原始秒级数据只保留在内存环形缓冲。
package pipeline

import (
	"sync"
)

// MetricsRow 对应 metrics_raw / metrics_1m / metrics_10m 的一行。
type MetricsRow struct {
	TS       int64 // 桶起点
	CPU      int
	CPUMax   int
	Load1    int
	MemUsed  int64
	SwapUsed int64
	DiskUsed int64
	RxBps    int64
	TxBps    int64
	RxBpsMax int64
	TxBpsMax int64
	Conns    int
}

// LatencyRow 对应 latency_raw / latency_10m 的一行。
// 全部探测失败时 RTT 三字段为 nil。
type LatencyRow struct {
	TS       int64
	TargetID int64
	RTTAvg   *int64
	RTTMin   *int64
	RTTMax   *int64
	Loss     int // ×100
}

// MetricsInput 是采集到的原始指标（已转为整数单位）。
type MetricsInput struct {
	CPU      int // ×100
	Load1    int // ×100
	Conns    int
	MemUsed  int64 // 字节
	SwapUsed int64
	DiskUsed int64
	RxBps    int64
	TxBps    int64
}

// metricsBucket 是一个进行中的聚合桶。
type metricsBucket struct {
	start int64
	n     int64

	sumCPU, maxCPU  int64
	sumLoad         int64
	sumMem, sumSwap int64
	sumDisk         int64
	sumRx, maxRx    int64
	sumTx, maxTx    int64
	sumConns        int64
}

// add 累加一个采样点。
// 注意：不要在这里用 "start == 0 表示未设置" 的写法去补 start。桶起点 0 是
// 合法值（每个分辨率下与 epoch 对齐的第一个窗口），早期版本用 ts 覆盖它，
// 导致第一个窗口每来一个采样点就误判为跨桶、凭空多出若干行。
// start 一律由 aggregator 在创建桶时按 bucketStart() 设好。
func (b *metricsBucket) add(m MetricsInput) {
	b.n++
	b.sumCPU += int64(m.CPU)
	if int64(m.CPU) > b.maxCPU {
		b.maxCPU = int64(m.CPU)
	}
	b.sumLoad += int64(m.Load1)
	b.sumMem += m.MemUsed
	b.sumSwap += m.SwapUsed
	b.sumDisk += m.DiskUsed
	b.sumRx += m.RxBps
	if m.RxBps > b.maxRx {
		b.maxRx = m.RxBps
	}
	b.sumTx += m.TxBps
	if m.TxBps > b.maxTx {
		b.maxTx = m.TxBps
	}
	b.sumConns += int64(m.Conns)
}

// row 把桶转换为一行（均值向下取整，峰值取最大）。
func (b *metricsBucket) row() MetricsRow {
	n := b.n
	if n == 0 {
		n = 1
	}
	return MetricsRow{
		TS:       b.start,
		CPU:      int(b.sumCPU / n),
		CPUMax:   int(b.maxCPU),
		Load1:    int(b.sumLoad / n),
		MemUsed:  b.sumMem / n,
		SwapUsed: b.sumSwap / n,
		DiskUsed: b.sumDisk / n,
		RxBps:    b.sumRx / n,
		TxBps:    b.sumTx / n,
		RxBpsMax: b.maxRx,
		TxBpsMax: b.maxTx,
		Conns:    int(b.sumConns / n),
	}
}

// aggregator 是某一分辨率下、所有节点的指标聚合器。
type aggregator struct {
	resolution int64 // 秒
	// emit 回调节点 ID 与闭合后的行。节点 ID 必须由聚合器带回：
	// MetricsRow 里没有 node_id 字段（那是表的主键之一），早期版本在这里
	// 误把 r.TS 当成 nodeID 传出，导致所有时序行的 node_id 变成桶时间戳。
	emit func(nodeID int64, r MetricsRow)

	mu      sync.Mutex
	buckets map[int64]*metricsBucket // key = nodeID
}

func newAggregator(resolution int64, emit func(nodeID int64, r MetricsRow)) *aggregator {
	return &aggregator{resolution: resolution, buckets: make(map[int64]*metricsBucket), emit: emit}
}

// bucketStart 计算 ts 所在桶的起点。
func (a *aggregator) bucketStart(ts int64) int64 { return ts - ts%a.resolution }

// AddNode 写入某节点的一个采样点；跨桶时闭合旧桶。
// 乱序样本（早于当前桶起点）直接丢弃（13.4.2）。
func (a *aggregator) AddNode(nodeID, ts int64, in MetricsInput) {
	start := a.bucketStart(ts)

	a.mu.Lock()
	defer a.mu.Unlock()

	cur, ok := a.buckets[nodeID]
	if !ok {
		a.buckets[nodeID] = &metricsBucket{start: start}
		a.buckets[nodeID].add(in)
		return
	}
	if cur.start == start {
		cur.add(in)
		return
	}
	if ts < cur.start {
		return // 乱序样本，丢弃
	}
	if a.emit != nil && cur.n > 0 {
		a.emit(nodeID, cur.row())
	}
	a.buckets[nodeID] = &metricsBucket{start: start}
	a.buckets[nodeID].add(in)
}

// FlushAll 闭合所有未完成的桶（正常退出时调用，4.2 第 5 条）。
func (a *aggregator) FlushAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for nodeID, b := range a.buckets {
		if b.n > 0 && a.emit != nil {
			a.emit(nodeID, b.row())
		}
		a.buckets[nodeID] = &metricsBucket{}
	}
}

// MultiResolution 组织三层指标聚合（10s / 1m / 10m）。
type MultiResolution struct {
	Raw *aggregator // 高精度层，粒度 = raw_resolution_s（3~60，默认 10）
	M1  *aggregator // 中精度层，60s
	M10 *aggregator // 低精度层，600s
}

// NewMultiResolution 创建三层聚合器。emit 回调收到 (表名, 节点ID, 行)。
func NewMultiResolution(rawResolutionS int, emit func(table string, nodeID int64, row MetricsRow)) *MultiResolution {
	if rawResolutionS < 3 || rawResolutionS > 60 {
		rawResolutionS = 10
	}
	return &MultiResolution{
		Raw: newAggregator(int64(rawResolutionS), func(nodeID int64, r MetricsRow) { emit("metrics_raw", nodeID, r) }),
		M1:  newAggregator(60, func(nodeID int64, r MetricsRow) { emit("metrics_1m", nodeID, r) }),
		M10: newAggregator(600, func(nodeID int64, r MetricsRow) { emit("metrics_10m", nodeID, r) }),
	}
}

// Add 把一个采样点同时送入三层。
func (m *MultiResolution) Add(nodeID, ts int64, in MetricsInput) {
	m.Raw.AddNode(nodeID, ts, in)
	m.M1.AddNode(nodeID, ts, in)
	m.M10.AddNode(nodeID, ts, in)
}

// FlushAll 闭合全部未完成桶。
func (m *MultiResolution) FlushAll() {
	m.Raw.FlushAll()
	m.M1.FlushAll()
	m.M10.FlushAll()
}

// ---------------------------------------------------------------------------
// 延迟聚合：只有两层（原始层 + 10 分钟层，4.2）
// ---------------------------------------------------------------------------

type latencyKey struct{ nodeID, targetID int64 }

type latencyBucket struct {
	start   int64
	n       int64
	sumAvg  int64
	nAvg    int64
	min     int64
	max     int64
	sumLoss int64
}

// add 累加一条延迟采样。start 由调用方按 bucketStart 设好（理由同 metricsBucket.add）。
func (b *latencyBucket) add(r LatencyRow) {
	b.n++
	if r.RTTAvg != nil {
		b.sumAvg += *r.RTTAvg
		b.nAvg++
	}
	if r.RTTMin != nil && (b.min == 0 || *r.RTTMin < b.min) {
		b.min = *r.RTTMin
	}
	if r.RTTMax != nil && *r.RTTMax > b.max {
		b.max = *r.RTTMax
	}
	b.sumLoss += int64(r.Loss)
}

func (b *latencyBucket) row(targetID int64) LatencyRow {
	out := LatencyRow{TS: b.start, TargetID: targetID}
	if b.nAvg > 0 {
		avg := b.sumAvg / b.nAvg
		out.RTTAvg = &avg
	}
	if b.min > 0 {
		mn := b.min
		out.RTTMin = &mn
	}
	if b.max > 0 {
		mx := b.max
		out.RTTMax = &mx
	}
	n := b.n
	if n == 0 {
		n = 1
	}
	out.Loss = int(b.sumLoss / n)
	return out
}

// LatencyAggregator 聚合延迟数据，按 (nodeID, targetID) 分桶。
type LatencyAggregator struct {
	resolution int64
	onRow      func(nodeID int64, row LatencyRow)

	mu      sync.Mutex
	buckets map[latencyKey]*latencyBucket
}

// NewLatencyAggregator 创建延迟聚合器；resolutionS 默认 30（探测原始层）。
func NewLatencyAggregator(resolutionS int, onRow func(nodeID int64, row LatencyRow)) *LatencyAggregator {
	if resolutionS < 10 {
		resolutionS = 30
	}
	return &LatencyAggregator{
		resolution: int64(resolutionS),
		onRow:      onRow,
		buckets:    make(map[latencyKey]*latencyBucket),
	}
}

// Add 写入一条延迟采样。
func (l *LatencyAggregator) Add(nodeID int64, r LatencyRow) {
	start := r.TS - r.TS%l.resolution
	r.TS = start
	key := latencyKey{nodeID, r.TargetID}

	l.mu.Lock()
	defer l.mu.Unlock()

	cur, ok := l.buckets[key]
	if !ok {
		l.buckets[key] = &latencyBucket{start: start}
		l.buckets[key].add(r)
		return
	}
	if cur.start == start {
		cur.add(r)
		return
	}
	if r.TS < cur.start {
		return
	}
	if l.onRow != nil && cur.n > 0 {
		l.onRow(nodeID, cur.row(r.TargetID))
	}
	l.buckets[key] = &latencyBucket{start: start}
	l.buckets[key].add(r)
}

// FlushAll 闭合所有未完成的延迟桶。
func (l *LatencyAggregator) FlushAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, b := range l.buckets {
		if b.n > 0 && l.onRow != nil {
			l.onRow(key.nodeID, b.row(key.targetID))
		}
		l.buckets[key] = &latencyBucket{}
	}
}

// LatencyMultiResolution 组织延迟的两层聚合：原始探测粒度 + 10 分钟层。
//
// 延迟不需要 metrics 那种三层：原始层保留最近 24 小时细节，10 分钟层支撑
// 7 天 / 30 天的大跨度曲线。没有 10 分钟层时，选 7d/30d 会落到空表。
type LatencyMultiResolution struct {
	Raw *LatencyAggregator // 原始探测粒度
	M10 *LatencyAggregator // 10 分钟聚合层
}

// NewLatencyMultiResolution 创建延迟两层聚合器。
// rawResolutionS 是探测间隔；m10ResolutionS 固定 600 秒（10 分钟）。
func NewLatencyMultiResolution(rawResolutionS int, onRow func(table string, nodeID int64, row LatencyRow)) *LatencyMultiResolution {
	return &LatencyMultiResolution{
		Raw: NewLatencyAggregator(rawResolutionS, func(nodeID int64, row LatencyRow) {
			onRow("latency_raw", nodeID, row)
		}),
		M10: NewLatencyAggregator(600, func(nodeID int64, row LatencyRow) {
			onRow("latency_10m", nodeID, row)
		}),
	}
}

// Add 把一条延迟采样同时送入两层聚合桶。
func (m *LatencyMultiResolution) Add(nodeID int64, r LatencyRow) {
	m.Raw.Add(nodeID, r)
	m.M10.Add(nodeID, r)
}

// FlushAll 闭合全部未完成桶（优雅退出时调用）。
func (m *LatencyMultiResolution) FlushAll() {
	m.Raw.FlushAll()
	m.M10.FlushAll()
}
