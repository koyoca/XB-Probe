package store

import (
	"testing"
	"time"
)

// 时序清理与节点历史清理的回归测试。
//
// ★ 这一组测试存在的唯一理由：这两条路径曾经**完全不可用**，
// 而它们没有任何测试覆盖，所以坏了很久没人发现。
//
// 根因是 SQL 里用了 `rowid`，而五张时序表建表时是
// `PRIMARY KEY(node_id, ts) WITHOUT ROWID` —— 这种表**没有 rowid 伪列**，
// 语句直接报 `SQL logic error: no such column: rowid`。
//
// 这个缺陷最恶劣的地方在于它**不出声**：
//   * 调度器只在日志里记一行 "时序清理失败"，退出码照旧是 0，健康检查照旧通过；
//   * 症状要等几个月后磁盘涨满才显现，而那时离问题被引入已经太远了。
//
// 所以下面刻意断言"行确实被删掉了"，而不是"函数没返回错误"——
// 只有前者能挡住这类静默失效。

// insertMetricsRow 直接插一行 metrics_* 数据（测试专用，绕开 pipeline）。
func insertMetricsRow(t *testing.T, r *Repo, table string, nodeID, ts int64) {
	t.Helper()
	_, err := r.db.Write.Exec(
		`INSERT OR IGNORE INTO `+table+`(
			node_id, ts, cpu, cpu_max, load1, mem_used, swap_used, disk_used,
			rx_bps, tx_bps, rx_bps_max, tx_bps_max, conns)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nodeID, ts, 1000, 2000, 50, 1024, 0, 2048, 100, 200, 300, 400, 5)
	if err != nil {
		t.Fatalf("插入 %s 失败：%v", table, err)
	}
}

// insertLatencyRow 直接插一行 latency_* 数据。
func insertLatencyRow(t *testing.T, r *Repo, table string, nodeID, targetID, ts int64) {
	t.Helper()
	_, err := r.db.Write.Exec(
		`INSERT OR IGNORE INTO `+table+`(node_id, target_id, ts, rtt_avg, rtt_min, rtt_max, loss)
		 VALUES(?,?,?,?,?,?,?)`,
		nodeID, targetID, ts, 12345, 12000, 13000, 0)
	if err != nil {
		t.Fatalf("插入 %s 失败：%v", table, err)
	}
}

func countRows(t *testing.T, r *Repo, table string) int64 {
	t.Helper()
	var n int64
	if err := r.db.Read.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("统计 %s 失败：%v", table, err)
	}
	return n
}

// TestCleanupTimeSeriesActuallyDeletes 覆盖 4.2 的每小时清理。
//
// 断言的是"过期行消失、未过期行保留"，而不是"没报错"——
// 旧实现会在这里返回 `no such column: rowid` 并留下全部行。
//
// ★ 五张表的保留窗口**不一样**，测试数据必须按各自的窗口取：
//
//	metrics_raw / latency_raw  → rawHours（小时，默认 24）
//	metrics_1m                 → midDays（天，默认 7）
//	metrics_10m / latency_10m  → totalDays（天，默认 30）
//
// 曾经写成"所有表都用 100 小时前的行当过期数据"，
// 结果 1m/10m 三张表根本不该删（100 小时 < 7 天），测试却断言它们被删——
// 是**测试**错了，不是实现错了。这类误报会诱使人去"修"本来正确的代码。
func TestCleanupTimeSeriesActuallyDeletes(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "n1")
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	const rawHours, midDays, totalDays = 24, 7, 30
	rawOld := now.Add(-100 * time.Hour).Unix() // > 24h：raw 表过期
	rawFresh := now.Add(-1 * time.Hour).Unix() // < 24h：raw 表保留
	midOld := now.AddDate(0, 0, -10).Unix()    // > 7d：1m 表过期
	midFresh := now.AddDate(0, 0, -2).Unix()   // < 7d：1m 表保留
	totalOld := now.AddDate(0, 0, -40).Unix()  // > 30d：10m 表过期
	totalFresh := now.AddDate(0, 0, -5).Unix() // < 30d：10m 表保留

	// raw 类表：1 过期 + 1 保留。
	insertMetricsRow(t, r, "metrics_raw", nodeID, rawOld)
	insertMetricsRow(t, r, "metrics_raw", nodeID, rawFresh)
	insertLatencyRow(t, r, "latency_raw", nodeID, 7, rawOld)
	insertLatencyRow(t, r, "latency_raw", nodeID, 7, rawFresh)
	// 1m 表：按 midDays。
	insertMetricsRow(t, r, "metrics_1m", nodeID, midOld)
	insertMetricsRow(t, r, "metrics_1m", nodeID, midFresh)
	// 10m 类表：按 totalDays。
	insertMetricsRow(t, r, "metrics_10m", nodeID, totalOld)
	insertMetricsRow(t, r, "metrics_10m", nodeID, totalFresh)
	insertLatencyRow(t, r, "latency_10m", nodeID, 7, totalOld)
	insertLatencyRow(t, r, "latency_10m", nodeID, 7, totalFresh)

	if err := r.CleanupTimeSeries(now, rawHours, midDays, totalDays); err != nil {
		t.Fatalf("CleanupTimeSeries 返回错误（这正是 rowid 缺陷的症状）：%v", err)
	}

	for _, tbl := range []string{"metrics_raw", "metrics_1m", "metrics_10m", "latency_raw", "latency_10m"} {
		if got := countRows(t, r, tbl); got != 1 {
			t.Errorf("%s：期望只剩 1 行（未过期的），实际 %d 行", tbl, got)
		}
	}
}

// TestCleanupTimeSeriesLargeBatch 覆盖分批逻辑。
//
// 分批上界（hi）必须每轮严格推进，否则会退化成一个
// "每轮 LIMIT 取到同一批、删不掉、上界不动"的死循环 —— 清理线程会卡死。
// 这里放 12000 行（> 5000 的批大小）确保至少走两轮。
func TestCleanupTimeSeriesLargeBatch(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "n1")
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	const rows = 12000
	tx, err := r.db.Write.Begin()
	if err != nil {
		t.Fatalf("开启事务失败：%v", err)
	}
	base := now.Add(-100 * time.Hour).Unix()
	for i := 0; i < rows; i++ {
		if _, err := tx.Exec(
			`INSERT INTO metrics_raw(node_id, ts, cpu) VALUES(?,?,?)`,
			nodeID, base+int64(i), 100); err != nil {
			_ = tx.Rollback()
			t.Fatalf("批量插入第 %d 行失败：%v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if got := countRows(t, r, "metrics_raw"); got != rows {
		t.Fatalf("准备数据失败：期望 %d 行，实际 %d", rows, got)
	}

	// 设一个非常短的保留窗口，让全部 12000 行过期。
	// 若分批上界推进有 bug，这里会挂死（测试超时就是失败信号）。
	if err := r.CleanupTimeSeries(now, 1, 7, 30); err != nil {
		t.Fatalf("CleanupTimeSeries 返回错误：%v", err)
	}
	if got := countRows(t, r, "metrics_raw"); got != 0 {
		t.Errorf("期望 0 行，实际 %d 行（分批未删干净）", got)
	}
}

// TestCleanupTimeSeriesNoRowsIsNotError 空表必须是"正常无事发生"。
//
// 旧实现的报错与"真出错"混在一起，运维根本分不清是配置问题还是缺陷；
// 新实现用 sql.NullInt64 收 MAX(ts)，空集走正常返回路径。
func TestCleanupTimeSeriesNoRowsIsNotError(t *testing.T) {
	r := newTestRepo(t)
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	if err := r.CleanupTimeSeries(now, 24, 7, 30); err != nil {
		t.Fatalf("空表清理不应报错，实际：%v", err)
	}
}

// TestPurgeNodeHistoryActuallyDeletes 覆盖 A7 / 13.4.8：
// 归档或删除节点时要清掉它的历史时序行。
//
// 与 CleanupTimeSeries 是**同一处根因**（都写了 rowid），
// 但代码路径不同（purgeLoop vs cleanupBatch），必须各自回归，
// 否则很容易"修了一个漏了另一个"。
func TestPurgeNodeHistoryActuallyDeletes(t *testing.T) {
	r := newTestRepo(t)
	victim := mustNode(t, r, "victim")
	other := mustNode(t, r, "other")
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC).Unix()

	// 待清理节点：每张表 3 行。
	for _, tbl := range []string{"metrics_raw", "metrics_1m", "metrics_10m"} {
		for i := 0; i < 3; i++ {
			insertMetricsRow(t, r, tbl, victim, now+int64(i))
		}
	}
	for _, tbl := range []string{"latency_raw", "latency_10m"} {
		for i := 0; i < 3; i++ {
			insertLatencyRow(t, r, tbl, victim, 7, now+int64(i))
		}
	}
	// 干扰项：另一个节点的数据**绝不能**被误删。
	for _, tbl := range []string{"metrics_raw", "metrics_1m", "metrics_10m"} {
		insertMetricsRow(t, r, tbl, other, now)
	}
	insertLatencyRow(t, r, "latency_raw", other, 7, now)
	insertLatencyRow(t, r, "latency_10m", other, 7, now)

	if err := r.PurgeNodeHistory(victim, 5000); err != nil {
		t.Fatalf("PurgeNodeHistory 返回错误（这正是 rowid 缺陷的症状）：%v", err)
	}

	for _, tbl := range []string{"metrics_raw", "metrics_1m", "metrics_10m", "latency_raw", "latency_10m"} {
		if got := countRows(t, r, tbl); got != 1 {
			t.Errorf("%s：期望只剩另一节点的 1 行，实际 %d 行", tbl, got)
		}
	}
}

// TestPurgeNodeHistoryBatchLoop 覆盖 purgeLoop 的多轮推进。
//
// batch 传 2、插 7 行 → 必须走 4 轮才能删完；
// 若上界不推进就会死循环或残留。
func TestPurgeNodeHistoryBatchLoop(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "n1")
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC).Unix()

	const rows = 7
	for i := 0; i < rows; i++ {
		insertMetricsRow(t, r, "metrics_raw", nodeID, now+int64(i))
	}
	if err := r.PurgeNodeHistory(nodeID, 2); err != nil {
		t.Fatalf("PurgeNodeHistory 返回错误：%v", err)
	}
	if got := countRows(t, r, "metrics_raw"); got != 0 {
		t.Errorf("期望 0 行，实际 %d 行", got)
	}
}

// TestCleanupNeverTouchesFreshData 保证清理不会把还在窗口内的数据删掉。
//
// 这是"删得太狠"方向的护栏：分批条件里同时有 ts < cut 与 ts <= hi，
// 两个条件写反一个就会把最新数据一起清掉——而那种损失同样是静默的。
func TestCleanupNeverTouchesFreshData(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "n1")
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	// 边界：正好落在 cut 上的行**不应**被删（条件是严格小于）。
	// rawHours=24 → cut = now-24h。
	cut := now.Add(-24 * time.Hour).Unix()
	atBoundary := cut        // ts == cut，保留
	justBefore := cut - 1    // 过期，删除
	wellInside := now.Unix() // 保留

	insertMetricsRow(t, r, "metrics_raw", nodeID, justBefore)
	insertMetricsRow(t, r, "metrics_raw", nodeID, atBoundary)
	insertMetricsRow(t, r, "metrics_raw", nodeID, wellInside)

	if err := r.CleanupTimeSeries(now, 24, 7, 30); err != nil {
		t.Fatalf("CleanupTimeSeries 返回错误：%v", err)
	}
	// justBefore 被删，atBoundary 与 wellInside 保留 → 2 行。
	if got := countRows(t, r, "metrics_raw"); got != 2 {
		t.Errorf("期望保留 2 行（ts == cut 的边界行必须留下），实际 %d 行", got)
	}
	var n int64
	if err := r.db.Read.QueryRow(
		`SELECT COUNT(*) FROM metrics_raw WHERE ts = ?`, atBoundary).Scan(&n); err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if n != 1 {
		t.Errorf("ts == cut 的边界行被误删了（应保留）")
	}
}
