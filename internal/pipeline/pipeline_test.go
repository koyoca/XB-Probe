package pipeline

import (
	"testing"

	"github.com/yourorg/probe/internal/db"
)

// ---------------------------------------------------------------------------
// 聚合桶（A2 / 13.4.2）
// ---------------------------------------------------------------------------

// 桶内取均值（向下取整），CPU 与流量峰值取最大值。
func TestMetricsBucketMeanAndMax(t *testing.T) {
	b := &metricsBucket{start: 3600} // 桶起点由 aggregator 按分辨率算好后传入
	b.add(MetricsInput{CPU: 1000, Load1: 200, Conns: 10, MemUsed: 100, SwapUsed: 10, DiskUsed: 1000, RxBps: 100, TxBps: 200})
	b.add(MetricsInput{CPU: 3000, Load1: 400, Conns: 30, MemUsed: 300, SwapUsed: 30, DiskUsed: 3000, RxBps: 900, TxBps: 100})
	b.add(MetricsInput{CPU: 2000, Load1: 300, Conns: 20, MemUsed: 200, SwapUsed: 20, DiskUsed: 2000, RxBps: 500, TxBps: 300})

	got := b.row()
	if got.TS != 3600 {
		t.Errorf("行的 TS 必须是桶起点 3600，得到 %d", got.TS)
	}
	if got.CPU != 2000 { // (1000+3000+2000)/3 = 2000
		t.Errorf("CPU 均值应为 2000，得到 %d", got.CPU)
	}
	if got.CPUMax != 3000 {
		t.Errorf("CPU 峰值应为 3000，得到 %d", got.CPUMax)
	}
	if got.Load1 != 300 { // (200+400+300)/3 = 300
		t.Errorf("load1 均值应为 300，得到 %d", got.Load1)
	}
	if got.MemUsed != 200 || got.SwapUsed != 20 || got.DiskUsed != 2000 {
		t.Errorf("内存/swap/磁盘均值不对：%+v", got)
	}
	if got.RxBps != 500 { // (100+900+500)/3 = 500
		t.Errorf("rx 均值应为 500，得到 %d", got.RxBps)
	}
	if got.RxBpsMax != 900 || got.TxBpsMax != 300 {
		t.Errorf("流量峰值不对：rx=%d tx=%d", got.RxBpsMax, got.TxBpsMax)
	}
	if got.Conns != 20 {
		t.Errorf("连接数均值应为 20，得到 %d", got.Conns)
	}
}

// 空桶不得 panic（n=0 时按 1 处理），也不得把桶起点错误地留在 0 之外。
func TestMetricsBucketEmptyDoesNotPanic(t *testing.T) {
	b := &metricsBucket{}
	got := b.row()
	if got.CPU != 0 || got.MemUsed != 0 {
		t.Errorf("空桶应全为 0：%+v", got)
	}
}

// 跨桶时闭合旧桶并回调；乱序样本（早于当前桶起点）直接丢弃，不污染桶。
func TestAggregatorRolloverAndOutOfOrderDrop(t *testing.T) {
	type ev struct {
		node int64
		row  MetricsRow
	}
	var emitted []ev
	a := newAggregator(10, func(nodeID int64, r MetricsRow) { emitted = append(emitted, ev{nodeID, r}) })

	a.AddNode(7, 100, MetricsInput{CPU: 1000})
	a.AddNode(7, 105, MetricsInput{CPU: 3000}) // 同桶
	a.AddNode(7, 111, MetricsInput{CPU: 5000}) // 跨桶 → 闭合桶 [100,110)，开新桶

	if len(emitted) != 1 {
		t.Fatalf("跨桶应闭合 1 个桶，得到 %d：%+v", len(emitted), emitted)
	}
	if emitted[0].node != 7 {
		t.Errorf("回调必须带回真实节点 ID 7，得到 %d（把时间戳当节点 ID 是致命错误）", emitted[0].node)
	}
	if emitted[0].row.TS != 100 || emitted[0].row.CPU != 2000 || emitted[0].row.CPUMax != 3000 {
		t.Errorf("闭合桶内容不对：%+v", emitted[0].row)
	}

	// 乱序：ts 早于当前桶起点 110 → 丢弃，不产生新回调也不改变当前桶。
	a.AddNode(7, 50, MetricsInput{CPU: 99999})
	if len(emitted) != 1 {
		t.Errorf("乱序样本不应闭合任何桶，得到 %d", len(emitted))
	}
	a.FlushAll()
	if len(emitted) != 2 {
		t.Fatalf("FlushAll 应闭合剩余桶，得到 %d", len(emitted))
	}
	if emitted[1].row.TS != 110 || emitted[1].row.CPU != 5000 || emitted[1].row.CPUMax != 5000 {
		t.Errorf("乱序样本污染了当前桶：%+v", emitted[1].row)
	}
}

// 不同节点各自独立分桶，互不干扰。
func TestAggregatorPerNodeIsolation(t *testing.T) {
	type key struct{ node, ts int64 }
	seen := map[key]MetricsRow{}
	a := newAggregator(10, func(nodeID int64, r MetricsRow) { seen[key{nodeID, r.TS}] = r })

	a.AddNode(1, 100, MetricsInput{CPU: 1000})
	a.AddNode(2, 100, MetricsInput{CPU: 7777})
	a.AddNode(1, 200, MetricsInput{CPU: 2000}) // 只闭合节点 1 的桶
	a.FlushAll()

	// 节点 1 的桶在跨桶时闭合，内容只应包含它自己的采样点。
	r1, ok := seen[key{1, 100}]
	if !ok {
		t.Fatalf("节点 1 的桶未闭合：%+v", seen)
	}
	if r1.CPU != 1000 {
		t.Errorf("节点 1 的桶被节点 2 污染：%+v", r1)
	}
	// 节点 2 的桶由 FlushAll 闭合，内容应是它自己的 7777。
	r2, ok := seen[key{2, 100}]
	if !ok {
		t.Fatalf("节点 2 的桶未闭合：%+v", seen)
	}
	if r2.CPU != 7777 {
		t.Errorf("节点 2 的桶内容丢失：%+v", r2)
	}
}

// ---------------------------------------------------------------------------
// 三层多分辨率（4.2：10s / 1m / 10m 三层，写时分层）
// ---------------------------------------------------------------------------

func TestMultiResolutionThreeLayers(t *testing.T) {
	type emitted struct {
		table string
		node  int64
		row   MetricsRow
	}
	var got []emitted
	m := NewMultiResolution(10, func(table string, nodeID int64, row MetricsRow) {
		got = append(got, emitted{table, nodeID, row})
	})

	// 三个采样点落在不同的分钟桶、但同一个 10 分钟桶内。
	m.Add(5, 100, MetricsInput{CPU: 1000})
	m.Add(5, 200, MetricsInput{CPU: 2000})
	m.Add(5, 300, MetricsInput{CPU: 3000})
	m.FlushAll()

	byTable := map[string][]MetricsRow{}
	for _, e := range got {
		if e.node != 5 {
			t.Errorf("节点 ID 应为 5，得到 %d", e.node)
		}
		byTable[e.table] = append(byTable[e.table], e.row)
	}

	// 10s 层：三个点分属 100/200/300 三个桶 → 3 行。
	if len(byTable["metrics_raw"]) != 3 {
		t.Errorf("metrics_raw 应有 3 行，得到 %d", len(byTable["metrics_raw"]))
	}
	// 1m 层：100s、200s、300s 分属三个分钟桶 → 3 行。
	if len(byTable["metrics_1m"]) != 3 {
		t.Errorf("metrics_1m 应有 3 行，得到 %d", len(byTable["metrics_1m"]))
	}
	// 10m 层：全部落在 [0,600) 同一个桶 → 1 行，均值 (1000+2000+3000)/3 = 2000。
	if len(byTable["metrics_10m"]) != 1 {
		t.Fatalf("metrics_10m 应有 1 行，得到 %d", len(byTable["metrics_10m"]))
	}
	if r := byTable["metrics_10m"][0]; r.CPU != 2000 || r.CPUMax != 3000 || r.TS != 0 {
		t.Errorf("10m 层聚合结果不对：%+v", r)
	}
}

// raw_resolution_s 超出 [3,60] 时收敛到默认 10 秒。
func TestMultiResolutionClampsRawResolution(t *testing.T) {
	for _, in := range []int{0, 1, 2, 61, 3600, -5} {
		m := NewMultiResolution(in, func(string, int64, MetricsRow) {})
		if m.Raw.resolution != 10 {
			t.Errorf("raw=%d 应被收敛为 10，得到 %d", in, m.Raw.resolution)
		}
	}
	// 合法值必须原样保留。
	m := NewMultiResolution(30, func(string, int64, MetricsRow) {})
	if m.Raw.resolution != 30 {
		t.Errorf("合法的 raw_resolution_s=30 不应被改写，得到 %d", m.Raw.resolution)
	}
	if m.M1.resolution != 60 || m.M10.resolution != 600 {
		t.Errorf("中间层/低精度层分辨率应为 60/600，得到 %d/%d", m.M1.resolution, m.M10.resolution)
	}
}

// ---------------------------------------------------------------------------
// 延迟聚合（只有两层：raw 30s + 10m）
// ---------------------------------------------------------------------------

func TestLatencyAggregatorAvgMinMaxLoss(t *testing.T) {
	type out struct {
		node int64
		row  LatencyRow
	}
	var got []out
	l := NewLatencyAggregator(30, func(nodeID int64, row LatencyRow) { got = append(got, out{nodeID, row}) })

	p := func(v int64) *int64 { return &v }
	// 100/105/108 都落在同一个 30 秒桶 [90,120) 内。
	l.Add(1, LatencyRow{TS: 100, TargetID: 9, RTTAvg: p(1000), RTTMin: p(1000), RTTMax: p(1000), Loss: 0})
	l.Add(1, LatencyRow{TS: 105, TargetID: 9, RTTAvg: p(3000), RTTMin: p(500), RTTMax: p(3000), Loss: 10000}) // 100%
	l.Add(1, LatencyRow{TS: 108, TargetID: 9, RTTAvg: p(2000), RTTMin: p(900), RTTMax: p(2000), Loss: 0})
	l.FlushAll()

	if len(got) != 1 {
		t.Fatalf("同一 30s 桶应只产 1 行，得到 %d", len(got))
	}
	r := got[0].row
	if got[0].node != 1 || r.TargetID != 9 {
		t.Errorf("node/target 不对：%+v", got[0])
	}
	if r.TS != 90 {
		t.Errorf("TS 应为桶起点 90，得到 %d", r.TS)
	}
	if r.RTTAvg == nil || *r.RTTAvg != 2000 { // (1000+3000+2000)/3
		t.Errorf("rtt_avg 应为 2000，得到 %v", r.RTTAvg)
	}
	if r.RTTMin == nil || *r.RTTMin != 500 {
		t.Errorf("rtt_min 应为 500，得到 %v", r.RTTMin)
	}
	if r.RTTMax == nil || *r.RTTMax != 3000 {
		t.Errorf("rtt_max 应为 3000，得到 %v", r.RTTMax)
	}
	if r.Loss != 3333 { // 10000/3 向下取整
		t.Errorf("loss 应为 3333，得到 %d", r.Loss)
	}
}

// 一个桶内全部探测失败 → RTT 三个字段必须是 nil（而不是 0，0 会被前端画成 0ms）。
func TestLatencyAggregatorAllFailedGivesNilRTT(t *testing.T) {
	var got []LatencyRow
	l := NewLatencyAggregator(30, func(_ int64, row LatencyRow) { got = append(got, row) })

	l.Add(1, LatencyRow{TS: 100, TargetID: 9, Loss: 10000}) // 全部失败
	l.FlushAll()

	if len(got) != 1 {
		t.Fatalf("应产出 1 行，得到 %d", len(got))
	}
	r := got[0]
	if r.RTTAvg != nil || r.RTTMin != nil || r.RTTMax != nil {
		t.Errorf("全部失败时 RTT 应为 nil：avg=%v min=%v max=%v", r.RTTAvg, r.RTTMin, r.RTTMax)
	}
	if r.Loss != 10000 {
		t.Errorf("loss 应为 100%% 即 10000，得到 %d", r.Loss)
	}
}

// 分辨率非法（<10）时收敛到默认 30 秒。
func TestLatencyAggregatorClampsResolution(t *testing.T) {
	l := NewLatencyAggregator(0, nil)
	if l.resolution != 30 {
		t.Errorf("resolution 应收敛为 30，得到 %d", l.resolution)
	}
	l = NewLatencyAggregator(20, nil)
	if l.resolution != 20 {
		t.Errorf("合法的 20 应保留，得到 %d", l.resolution)
	}
}

// ---------------------------------------------------------------------------
// 写队列（4.2 / 13.4.2 / 13.4.8）
// ---------------------------------------------------------------------------

func newTestWriter(t *testing.T, diskFree DiskFreeFunc, diskMinMB int64) (*Writer, func()) {
	t.Helper()
	st, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开数据库失败：%v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("迁移失败：%v", err)
	}
	w, err := NewWriter(st.Write, 16, diskFree, diskMinMB)
	if err != nil {
		t.Fatalf("创建写协程失败：%v", err)
	}
	return w, func() { _ = st.Close() }
}

func countRows(t *testing.T, w *Writer, sqlStr string, args ...any) int {
	t.Helper()
	var n int
	if err := w.db.QueryRow(sqlStr, args...).Scan(&n); err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	return n
}

// 13.4.2：INSERT OR IGNORE —— 同一 (node_id, ts) 补报第二次必须被忽略，不产生重复行。
func TestWriterCommitIsInsertOrIgnore(t *testing.T) {
	w, closeDB := newTestWriter(t, nil, 0)
	defer closeDB()

	row := MetricsRow{TS: 1000, CPU: 1234, CPUMax: 5678, Load1: 100, MemUsed: 111, Conns: 3}
	batch := []Row{
		{Table: "metrics_raw", NodeID: 1, Metrics: &row},
		{Table: "metrics_raw", NodeID: 1, Metrics: &row}, // 同主键，应被 IGNORE
		{Table: "metrics_1m", NodeID: 1, Metrics: &row},
	}
	if err := w.commit(batch); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	if n := countRows(t, w, `SELECT COUNT(*) FROM metrics_raw`); n != 1 {
		t.Errorf("metrics_raw 应只有 1 行（IGNORE 生效），得到 %d", n)
	}
	if n := countRows(t, w, `SELECT COUNT(*) FROM metrics_1m`); n != 1 {
		t.Errorf("metrics_1m 应有 1 行，得到 %d", n)
	}
	// 值也要写对。
	var cpu, cpuMax, conns int
	if err := w.db.QueryRow(
		`SELECT cpu, cpu_max, conns FROM metrics_raw WHERE node_id=1 AND ts=1000`,
	).Scan(&cpu, &cpuMax, &conns); err != nil {
		t.Fatalf("读取写入值失败：%v", err)
	}
	if cpu != 1234 || cpuMax != 5678 || conns != 3 {
		t.Errorf("写入的值不对：cpu=%d cpu_max=%d conns=%d", cpu, cpuMax, conns)
	}
}

// 延迟行：RTT 为 nil 时必须落成 NULL，不能落成 0。
func TestWriterCommitLatencyNilRTT(t *testing.T) {
	w, closeDB := newTestWriter(t, nil, 0)
	defer closeDB()

	avg := int64(1500)
	batch := []Row{
		{Table: "latency_raw", NodeID: 1, Latency: &LatencyRow{TS: 30, TargetID: 9, Loss: 10000}},
		{Table: "latency_raw", NodeID: 1, Latency: &LatencyRow{TS: 60, TargetID: 9, RTTAvg: &avg, RTTMin: &avg, RTTMax: &avg, Loss: 0}},
	}
	if err := w.commit(batch); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	var nullCnt int
	if err := w.db.QueryRow(
		`SELECT COUNT(*) FROM latency_raw WHERE ts=30 AND rtt_avg IS NULL AND rtt_min IS NULL AND rtt_max IS NULL`,
	).Scan(&nullCnt); err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if nullCnt != 1 {
		t.Errorf("全部失败的样本 rtt 应落成 NULL，匹配行数 %d", nullCnt)
	}
	var v int64
	if err := w.db.QueryRow(`SELECT rtt_avg FROM latency_raw WHERE ts=60`).Scan(&v); err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if v != 1500 {
		t.Errorf("rtt_avg 应为 1500，得到 %d", v)
	}
}

// 表名白名单：不在白名单里的表名必须被静默丢弃，绝不能拼进 SQL。
func TestWriterRejectsUnknownTable(t *testing.T) {
	w, closeDB := newTestWriter(t, nil, 0)
	defer closeDB()
	defer w.closeStmts()

	m := MetricsRow{TS: 1, CPU: 1}
	w.Enqueue(Row{Table: "sqlite_master; DROP TABLE nodes", NodeID: 1, Metrics: &m})
	w.Enqueue(Row{Table: "nodes", NodeID: 1, Metrics: &m})
	if got := len(w.queue); got != 0 {
		t.Errorf("非法表名不应入队，队列长度 %d", got)
	}
	if _, ok := w.stmts["nodes"]; ok {
		t.Error("不应为白名单外的表准备语句")
	}
}

// 13.4.8：磁盘水位低于阈值时暂停时序写入（内存运行态不受影响，只停落盘）。
func TestWriterPausesOnDiskPressure(t *testing.T) {
	free := int64(50) // 50 MB，低于阈值 200 MB
	w, closeDB := newTestWriter(t, func() int64 { return free }, 200)
	defer closeDB()
	defer w.closeStmts()

	// 水位判定本身。
	if !w.shouldPause() {
		t.Error("剩余 50MB < 阈值 200MB，应判定为暂停")
	}
	free = 200 // 恰好等于阈值：不应暂停
	if w.shouldPause() {
		t.Error("剩余空间等于阈值时不应暂停")
	}
	free = 0 // 探测不到剩余空间：按不暂停处理，避免永久停写
	if w.shouldPause() {
		t.Error("探测不到剩余空间时不应暂停")
	}

	// paused 置位时 Enqueue 必须静默丢弃，不阻塞采集。
	w.paused.Store(true)
	m := MetricsRow{TS: 1, CPU: 1}
	w.Enqueue(Row{Table: "metrics_raw", NodeID: 1, Metrics: &m})
	if got := len(w.queue); got != 0 {
		t.Errorf("暂停状态下不应入队，队列长度 %d", got)
	}

	// 恢复后必须能重新入队。
	w.paused.Store(false)
	w.Enqueue(Row{Table: "metrics_raw", NodeID: 1, Metrics: &m})
	if got := len(w.queue); got != 1 {
		t.Errorf("恢复后应入队成功，队列长度 %d", got)
	}
}

// diskFree 为 nil（未启用磁盘水位保护）时永不暂停。
func TestWriterNoDiskProbeNeverPauses(t *testing.T) {
	w, closeDB := newTestWriter(t, nil, 200)
	defer closeDB()
	defer w.closeStmts()
	if w.shouldPause() {
		t.Error("未配置磁盘探测时不应暂停")
	}
}

// 队列满时优先丢弃最旧的高精度行，并把丢弃计入 Stats（A6）。
func TestWriterDropsOldestHighPrecisionWhenFull(t *testing.T) {
	w, closeDB := newTestWriter(t, nil, 0)
	defer closeDB()
	defer w.closeStmts()

	m := MetricsRow{TS: 1, CPU: 1}

	// 先把小容量队列填满高精度行。
	for i := 0; i < cap(w.queue); i++ {
		w.Enqueue(Row{Table: "metrics_raw", NodeID: 1, Metrics: &m})
	}
	if got := len(w.queue); got != cap(w.queue) {
		t.Fatalf("队列应已填满，实际 %d/%d", got, cap(w.queue))
	}

	// 再塞一行：应挤掉最旧的高精度行（计入丢弃），而不是无限阻塞或整批失败。
	before := w.dropped.Load()
	w.Enqueue(Row{Table: "metrics_10m", NodeID: 1, Metrics: &m})
	if got := len(w.queue); got != cap(w.queue) {
		t.Errorf("腾位后队列应仍为满容量 %d，得到 %d", cap(w.queue), got)
	}
	if got := w.dropped.Load(); got != before+1 {
		t.Errorf("应恰好丢弃 1 行高精度数据，实际丢弃 %d", got-before)
	}
	// 队尾应是刚投进去的低精度行：它比高精度行更值得保留。
	var last Row
	for len(w.queue) > 0 {
		last = <-w.queue
	}
	if last.Table != "metrics_10m" {
		t.Errorf("低精度行应入队成功（在队尾），得到队尾为 %q", last.Table)
	}
}
