package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/koyoca/xb-probe/internal/traffic"
)

// 流量与计费持久层（4.3 / 6.4 / 13.4.1）。

// TrafficState 对应 traffic_state 表。
type TrafficState struct {
	NodeID      int64 `json:"node_id"`
	PeriodStart int64 `json:"period_start"`
	PeriodRx    int64 `json:"period_rx"`
	PeriodTx    int64 `json:"period_tx"`
	UpdatedAt   int64 `json:"updated_at"`
}

// GetTrafficState 读取当前周期已用量。
func (r *Repo) GetTrafficState(nodeID int64) (*TrafficState, error) {
	ts := &TrafficState{NodeID: nodeID}
	err := r.db.Read.QueryRow(
		`SELECT period_start, period_rx, period_tx, updated_at FROM traffic_state WHERE node_id=?`, nodeID).
		Scan(&ts.PeriodStart, &ts.PeriodRx, &ts.PeriodTx, &ts.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ts, nil
}

// UpsertTrafficState 写入当前周期已用量。
func (r *Repo) UpsertTrafficState(st TrafficState) error {
	_, err := r.db.Write.Exec(
		`INSERT INTO traffic_state(node_id, period_start, period_rx, period_tx, updated_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   period_start=excluded.period_start, period_rx=excluded.period_rx,
		   period_tx=excluded.period_tx, updated_at=excluded.updated_at`,
		st.NodeID, st.PeriodStart, st.PeriodRx, st.PeriodTx, st.UpdatedAt)
	return err
}

// LoadTrafficState 以原始值暴露当前周期已用量（实现 traffic.Store）。
func (r *Repo) LoadTrafficState(nodeID int64) (periodStart, rx, tx int64, ok bool, err error) {
	ts, err := r.GetTrafficState(nodeID)
	if err != nil || ts == nil {
		return 0, 0, 0, false, err
	}
	return ts.PeriodStart, ts.PeriodRx, ts.PeriodTx, true, nil
}

// SaveTrafficState 以原始值写入当前周期已用量（实现 traffic.Store）。
func (r *Repo) SaveTrafficState(nodeID, periodStart, rx, tx int64) error {
	return r.UpsertTrafficState(TrafficState{
		NodeID: nodeID, PeriodStart: periodStart, PeriodRx: rx, PeriodTx: tx, UpdatedAt: Now(),
	})
}

// AddTrafficDaily 累加当日流量（traffic_daily 一天一行）。
func (r *Repo) AddTrafficDaily(nodeID int64, day string, rx, tx int64) error {
	_, err := r.db.Write.Exec(
		`INSERT INTO traffic_daily(node_id, day, rx, tx) VALUES(?,?,?,?)
		 ON CONFLICT(node_id, day) DO UPDATE SET rx=rx+excluded.rx, tx=tx+excluded.tx`,
		nodeID, day, rx, tx)
	return err
}

// TrafficDay 是按日流量的一行（traffic_daily）。
type TrafficDay struct {
	Day string `json:"day"`
	Rx  int64  `json:"rx"`
	Tx  int64  `json:"tx"`
}

// TrafficDailyRow 是带节点 ID 的按日流量行。
type TrafficDailyRow struct {
	NodeID int64  `json:"node_id"`
	Day    string `json:"day"`
	Rx     int64  `json:"rx"`
	Tx     int64  `json:"tx"`
}

// QueryTrafficDailySince 一次取回所有节点自 sinceDay 起的按日流量（含当日）。
//
// 用于公开看板的"今日流量"：各节点时区可能不同，"今天"不是同一天，
// 因此不能在 SQL 里用单一日期过滤，而是取一个上界足够宽的窗口（调用方传
// 昨天即可覆盖 ±1 天的时区偏移），再由调用方按节点各自的时区挑出当天。
// 单次查询而不是按节点循环，避免节点多时出现 N+1。
func (r *Repo) QueryTrafficDailySince(sinceDay string) ([]TrafficDailyRow, error) {
	rows, err := r.db.Read.Query(
		`SELECT node_id, day, rx, tx FROM traffic_daily WHERE day>=? ORDER BY node_id, day`, sinceDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrafficDailyRow
	for rows.Next() {
		var e TrafficDailyRow
		if err := rows.Scan(&e.NodeID, &e.Day, &e.Rx, &e.Tx); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// QueryTrafficDaily 查询按日流量。
func (r *Repo) QueryTrafficDaily(nodeID int64, fromDay, toDay string) ([]TrafficDay, error) {
	rows, err := r.db.Read.Query(
		`SELECT day, rx, tx FROM traffic_daily WHERE node_id=? AND day>=? AND day<=? ORDER BY day`,
		nodeID, fromDay, toDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrafficDay
	for rows.Next() {
		var e TrafficDay
		if err := rows.Scan(&e.Day, &e.Rx, &e.Tx); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 网卡基线（13.4.1）
// ---------------------------------------------------------------------------

// LoadNICBaselines 读取某节点所有网卡的累计值基线。
func (r *Repo) LoadNICBaselines(nodeID int64) (map[string]traffic.NICBaseline, error) {
	rows, err := r.db.Read.Query(
		`SELECT nic, last_rx, last_tx, COALESCE(boot_time,0), updated_at
		 FROM traffic_nic_state WHERE node_id=?`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]traffic.NICBaseline{}
	for rows.Next() {
		var nic string
		var b traffic.NICBaseline
		if err := rows.Scan(&nic, &b.LastRx, &b.LastTx, &b.BootTime, &b.UpdatedAt); err != nil {
			return nil, err
		}
		b.Valid = true
		out[nic] = b
	}
	return out, rows.Err()
}

// SaveNICBaselines 批量落盘基线，间隔不超过 60 秒（A8 / 13.4.1）。
func (r *Repo) SaveNICBaselines(nodeID int64, m map[string]traffic.NICBaseline) error {
	if len(m) == 0 {
		return nil
	}
	tx, err := r.db.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(
		`INSERT INTO traffic_nic_state(node_id, nic, last_rx, last_tx, boot_time, updated_at)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(node_id, nic) DO UPDATE SET
		   last_rx=excluded.last_rx, last_tx=excluded.last_tx,
		   boot_time=excluded.boot_time, updated_at=excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for nic, b := range m {
		if _, err := stmt.Exec(nodeID, nic, b.LastRx, b.LastTx, b.BootTime, b.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CleanupNICBaselines 清理超过 7 天未更新的基线（13.4.1 规则 4）。
func (r *Repo) CleanupNICBaselines(now int64) (int64, error) {
	cutoff := now - traffic.BaselineCleanupDays*86400
	res, err := r.db.Write.Exec(`DELETE FROM traffic_nic_state WHERE updated_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// 计费（4.4）
// ---------------------------------------------------------------------------

// GetBilling 读取节点计费信息。
func (r *Repo) GetBilling(nodeID int64) (*BillingRow, error) {
	b := &BillingRow{NodeID: nodeID}
	err := r.db.Read.QueryRow(
		`SELECT COALESCE(price_minor,0), COALESCE(currency,''), cycle, COALESCE(expire_at,0),
		        traffic_quota, traffic_mode, reset_day, COALESCE(tz,''),
		        COALESCE(traffic_multiplier,1.0)
		 FROM node_billing WHERE node_id=?`, nodeID).
		Scan(&b.PriceMinor, &b.Currency, &b.Cycle, &b.ExpireAt,
			&b.TrafficQuota, &b.TrafficMode, &b.ResetDay, &b.TZ, &b.TrafficMultiplier)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// BillingRow 是 node_billing 的行。
type BillingRow struct {
	NodeID       int64  `json:"node_id"`
	PriceMinor   int64  `json:"price_minor"`
	Currency     string `json:"currency"`
	Cycle        string `json:"cycle"`
	ExpireAt     int64  `json:"expire_at"`
	TrafficQuota int64  `json:"traffic_quota"`
	TrafficMode  string `json:"traffic_mode"`
	ResetDay     int    `json:"reset_day"`
	TZ           string `json:"tz"`
	// TrafficMultiplier 是流量倍率：计费流量 = 计量口径 × 倍率（0 视为 1）。
	TrafficMultiplier float64 `json:"traffic_multiplier"`
}

// UpsertBilling 写入计费信息。
func (r *Repo) UpsertBilling(b BillingRow) error {
	// 兜底：小于等于 0 的倍率一律存 1，避免把"没填"变成"流量清零"。
	if b.TrafficMultiplier <= 0 {
		b.TrafficMultiplier = 1
	}
	_, err := r.db.Write.Exec(
		`INSERT INTO node_billing(node_id, price_minor, currency, cycle, expire_at,
		   traffic_quota, traffic_mode, reset_day, tz, traffic_multiplier)
		 VALUES(?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   price_minor=excluded.price_minor, currency=excluded.currency, cycle=excluded.cycle,
		   expire_at=excluded.expire_at, traffic_quota=excluded.traffic_quota,
		   traffic_mode=excluded.traffic_mode, reset_day=excluded.reset_day, tz=excluded.tz,
		   traffic_multiplier=excluded.traffic_multiplier`,
		b.NodeID, b.PriceMinor, b.Currency, b.Cycle, b.ExpireAt,
		b.TrafficQuota, b.TrafficMode, b.ResetDay, b.TZ, b.TrafficMultiplier)
	return err
}

// ListExpiringNodes 列出参与到期扫描的节点（永久节点不参与，4.4）。
func (r *Repo) ListExpiringNodes() ([]BillingRow, error) {
	rows, err := r.db.Read.Query(
		`SELECT b.node_id, COALESCE(b.price_minor,0), COALESCE(b.currency,''), b.cycle,
		        COALESCE(b.expire_at,0), b.traffic_quota, b.traffic_mode, b.reset_day,
		        COALESCE(b.tz,''), COALESCE(b.traffic_multiplier,1.0)
		 FROM node_billing b JOIN nodes n ON n.id=b.node_id
		 WHERE n.status<>'archived' AND b.cycle<>'lifetime' AND b.expire_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BillingRow
	for rows.Next() {
		var b BillingRow
		if err := rows.Scan(&b.NodeID, &b.PriceMinor, &b.Currency, &b.Cycle, &b.ExpireAt,
			&b.TrafficQuota, &b.TrafficMode, &b.ResetDay, &b.TZ, &b.TrafficMultiplier); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListBillings 一次性读出全部计费行，按 node_id 索引。
//
// 用途：后台节点列表要显示「月费合计」，而列表本身只有节点元数据。
// 按节点逐个 GetBilling 是 N+1，节点一多就是几十次点查；这里一次取回。
func (r *Repo) ListBillings() (map[int64]BillingRow, error) {
	rows, err := r.db.Read.Query(
		`SELECT node_id, COALESCE(price_minor,0), COALESCE(currency,''), cycle,
		        COALESCE(expire_at,0), traffic_quota, traffic_mode, reset_day,
		        COALESCE(tz,''), COALESCE(traffic_multiplier,1.0)
		 FROM node_billing`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]BillingRow{}
	for rows.Next() {
		var b BillingRow
		if err := rows.Scan(&b.NodeID, &b.PriceMinor, &b.Currency, &b.Cycle, &b.ExpireAt,
			&b.TrafficQuota, &b.TrafficMode, &b.ResetDay, &b.TZ, &b.TrafficMultiplier); err != nil {
			return nil, err
		}
		out[b.NodeID] = b
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 时序查询（按跨度自动选层，4.2 / 7.3）
// ---------------------------------------------------------------------------

// 指标与延迟各层的保留期设置项（与 internal/api/settings.go 的默认值、
// Scheduler.cleanup 的取用方式保持一致；store 不能 import api，所以这里
// 自带一份常量，三处必须同步改）。
const (
	settingRetentionRawHours = "retention.raw_hours"
	settingRetentionMidDays  = "retention.mid_days"

	defRetentionRawHours = 24
	defRetentionMidDays  = 7
)

// retentionRawHours 读取原始层保留小时数，返回时已收敛到合法区间。
//
// 与 api 的 intRange(1,72) 双重保险：即使有人绕过 API 直接改库里的设置
// （settings 表就是普通 KV），也不会算出 0 或负数导致"立刻全部过期"。
func (r *Repo) retentionRawHours() int {
	n := int(r.GetIntSetting(settingRetentionRawHours, defRetentionRawHours))
	if n < 1 {
		return 1
	}
	if n > 72 {
		return 72
	}
	return n
}

// retentionMidDays 读取中间层（1 分钟粒度）保留天数，同样收敛到 intRange(1,30)。
func (r *Repo) retentionMidDays() int {
	n := int(r.GetIntSetting(settingRetentionMidDays, defRetentionMidDays))
	if n < 1 {
		return 1
	}
	if n > 30 {
		return 30
	}
	return n
}

// MetricsTableForRange 按查询跨度**与保留期**选择指标表。
//
// 三层精度：raw（上报粒度，retention.raw_hours）→ 1m（retention.mid_days）
// → 10m（retention.days）。跨度落在哪层就查哪层；30 分钟档与 1h 一样落
// metrics_raw（曾经它返回空串、由调用方改读内存环形缓冲——缓冲随 Master
// 重启清空，"关掉网站过一阵再回来"时这一档就只剩重启以来的几分钟）。
//
// 三层的选择口径都是"该层保留期覆盖得住整个查询窗口吗"：
//
//	raw 覆盖要求 rawHours*3600 ≥ span
//	1m  覆盖要求 midDays*86400 ≥ span
//
// 覆盖不住就下沉到更粗的层——宁可给一条粗粒度但连续（有历史）的曲线，
// 也不要给一条"表已被清理"的空白曲线。
//
// 这里**必须**读设置项而不是写死常量。曾经的实现把 1h/5h/24h 硬映射到
// metrics_raw、7d 硬映射到 metrics_1m：只要管理员把 retention.raw_hours 从
// 24 调小（比如 6，省磁盘），"最近 24 小时"就会去查一张只留了 6 小时数据的
// 表——曲线左半边凭空消失，而后台设置页显示一切正常，排查时完全看不出
// 是"保留期"与"查询层"对不上。
func (r *Repo) MetricsTableForRange(rangeStr string) string {
	span := rangeDurationSeconds(rangeStr)
	if span <= 0 {
		// 未知跨度：给最粗的一层。它保留最久，永远不可能"已过期"。
		return "metrics_10m"
	}
	if int64(r.retentionRawHours())*3600 >= span {
		return "metrics_raw"
	}
	if int64(r.retentionMidDays())*86400 >= span {
		return "metrics_1m"
	}
	return "metrics_10m"
}

// rangeDurationSeconds 把跨度字符串换算成秒。
// 未知跨度返回 0，由调用方决定兜底（这里是回落到最粗的一层）。
func rangeDurationSeconds(rangeStr string) int64 {
	switch rangeStr {
	case "30m":
		return 30 * 60
	case "1h":
		return 3600
	case "5h":
		return 5 * 3600
	case "24h":
		return 24 * 3600
	case "7d":
		return 7 * 86400
	case "30d":
		return 30 * 86400
	default:
		return 0
	}
}

// LatencyTableForRange 按跨度选择延迟表（4.2）。
//
// 延迟只有原始层（latency_raw，探测粒度）与 10 分钟层（latency_10m）。
//
// 这里**永远不能返回空串**。曾经的实现把 "30m" 映射成 ""（注释写着"走内存
// LatestLatency"），但调用方 handlePublicLatency 是把它直接拼进 SQL 的：
//
//	SELECT ... FROM  WHERE node_id=?   -- 语法错误 → 500
//
// 于是"最近 30 分钟"这一档在公开详情页上恒为 500，图例全空。调用方既没有
// 内存层可用（内存里只有一轮 LatestLatency，画不出曲线），也没有做空值判断。
// 现在 30m 直接落到 latency_raw —— 那本来就是 24 小时内精度最高的一层，
// 只是窗口更短，语义完全正确。
func (r *Repo) LatencyTableForRange(rangeStr string) string {
	if rangeStr == "1h" || rangeStr == "5h" || rangeStr == "24h" || rangeStr == "30m" {
		return "latency_raw"
	}
	return "latency_10m"
}

// HistoryPoint 是历史曲线上的一个点。
type HistoryPoint struct {
	TS    int64
	Value int64
}

// QueryMetricHistory 查询单个指标的历史序列（7.3 / S9）。
//
// 返回点数 ≤ limit（上限 500）。窗口内的原始点超过 limit 时，**按时间均匀
// 分桶取均值**而不是只取最早的 limit 个点——后者曾让"最近 7 天"实际只画出
// 开头 8 个小时的数据，X 轴刻度随之显示成"20:00 21:00"这样的时分
// （数据跨度不足以触发日期标签），而"最近 24 小时"也只有开头 1 个多小时。
// 分桶后曲线覆盖完整窗口，X 轴标签自然恢复正确。
func (r *Repo) QueryMetricHistory(table string, nodeID, from, to int64, metric string, limit int) ([]HistoryPoint, error) {
	if !allowedHistoryTable[table] {
		return nil, errors.New("非法的查询表名")
	}
	expr, err := metricExpr(metric)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	span := to - from
	if span <= 0 {
		span = 1
	}
	// 桶宽 = 窗口/limit，向上取整到秒。窗口比 limit 短时 step=1（逐点输出）。
	step := (span + int64(limit) - 1) / int64(limit)
	if step < 1 {
		step = 1
	}
	// 桶表达式在 SELECT 与 GROUP BY 里各出现一次（不用别名：部分驱动对
	// GROUP BY 别名的支持不一致，报错会被上层吞成"空序列"）。
	// AVG 结果是 REAL，必须 CAST 回整数——HistoryPoint.Value 是 int64，
	// 直接 Scan 浮点会报 invalid syntax 且被上层吞掉。
	rows, err := r.db.Read.Query(
		`SELECT (ts/?)*?, CAST(AVG(`+expr+`) AS INTEGER) FROM `+table+`
		 WHERE node_id=? AND ts>=? AND ts<=? GROUP BY (ts/?)*? ORDER BY 1`,
		step, step, nodeID, from, to, step, step)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryPoint
	for rows.Next() {
		var p HistoryPoint
		if err := rows.Scan(&p.TS, &p.Value); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

var allowedHistoryTable = map[string]bool{
	"metrics_raw": true, "metrics_1m": true, "metrics_10m": true,
}

// metricExpr 把指标名映射为 SQL 表达式（白名单，杜绝注入）。
func metricExpr(metric string) (string, error) {
	switch metric {
	case "cpu":
		return "COALESCE(cpu,0)", nil
	case "mem", "mem_used":
		return "COALESCE(mem_used,0)", nil
	case "net_rx":
		return "COALESCE(rx_bps,0)", nil
	case "net_tx":
		return "COALESCE(tx_bps,0)", nil
	case "disk_used":
		return "COALESCE(disk_used,0)", nil
	case "conns":
		return "COALESCE(conns,0)", nil
	}
	return "", errors.New("不支持的指标")
}

// StorageTableStat 是单表的存储统计。
type StorageTableStat struct {
	Name        string `json:"name"`
	Count       int64  `json:"count"`
	Description string `json:"description"`
	BytesPerRow int64  `json:"bytes_per_row"`
}

// StorageStats 汇总各表行数与体积（GET /storage/stats）。
func (r *Repo) StorageStats() ([]StorageTableStat, error) {
	// 估算行宽（字节）：含 SQLite 行头、索引与页面碎片冗余后的经验值。
	// 时序表有大量索引，实际占用通常比裸行大 30%~50%。
	desc := map[string]string{
		"nodes":         "节点静态信息（名称、资产、计费）",
		"node_tokens":   "Agent 访问令牌哈希与使用记录",
		"metrics_raw":   "原始指标（按 report_interval_s 粒度）",
		"metrics_1m":    "1 分钟聚合指标",
		"metrics_10m":   "10 分钟聚合指标（长期曲线）",
		"latency_raw":   "原始探测延迟（按 probe_interval_s 粒度）",
		"latency_10m":   "10 分钟聚合延迟（长期曲线）",
		"traffic_daily": "每日流量统计（按节点时区日界）",
		"alert_events":  "告警事件记录",
		"audit_log":     "管理操作审计日志",
	}
	bytesPerRow := map[string]int64{
		"nodes": 512, "node_tokens": 128,
		"metrics_raw": 96, "metrics_1m": 96, "metrics_10m": 96,
		"latency_raw": 72, "latency_10m": 72,
		"traffic_daily": 48, "alert_events": 256, "audit_log": 192,
	}
	var out []StorageTableStat
	for _, t := range []string{
		"nodes", "node_tokens", "metrics_raw", "metrics_1m", "metrics_10m",
		"latency_raw", "latency_10m", "traffic_daily", "alert_events", "audit_log",
	} {
		var n int64
		if err := r.db.Read.QueryRow(`SELECT COUNT(*) FROM ` + t).Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, StorageTableStat{
			Name:        t,
			Count:       n,
			Description: desc[t],
			BytesPerRow: bytesPerRow[t],
		})
	}
	return out, nil
}

// CheckpointWAL 执行每日 WAL 截断与增量 vacuum（4.2 清理）。
func (r *Repo) CheckpointWAL() error {
	if _, err := r.db.Write.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	_, err := r.db.Write.Exec(`PRAGMA incremental_vacuum`)
	return err
}

// CleanupTimeSeries 删除过期时序行（每小时分批执行，4.2 清理）。
//
// ★ 为什么这里**不能**用 `WHERE rowid IN (SELECT rowid ... LIMIT n)`：
// 五张时序表（metrics_raw/1m/10m、latency_raw/10m）建表时都是
// `PRIMARY KEY(node_id, ts) WITHOUT ROWID` —— 为的是把主键当聚簇索引，
// 让"按节点 + 时间范围"的查询直接命中 B 树、省掉一层回表。
// WITHOUT ROWID 的表**没有 rowid 伪列**，旧写法每次都在这里撞上
// `SQL logic error: no such column: rowid` 并整轮中止。
//
// 这个缺陷最恶劣的地方是**它自己不出声**：调度器只在日志里记一行 ERROR，
// 而"没有清理"的症状要几个月后磁盘涨满才显现。同一处错误也存在于
// PurgeNodeHistory（归档/删除节点时清理历史）。两者已一并改为按主键分批。
//
// 分批删法：每轮取一个**比当前所有待删行都小**的 ts 上界，
// 保证每轮至少能删掉 5000 行（不会出现"LIMIT 取到同一批、上界推不动"的死循环），
// 而 WHERE 直接落在主键第二列上，可以走索引范围扫描。
// 见 cleanupBatch。
func (r *Repo) CleanupTimeSeries(now time.Time, rawHours, midDays, totalDays int) error {
	type job struct {
		table string
		cut   int64
	}
	jobs := []job{
		{"metrics_raw", now.Add(-time.Duration(rawHours) * time.Hour).Unix()},
		{"latency_raw", now.Add(-time.Duration(rawHours) * time.Hour).Unix()},
		{"metrics_1m", now.AddDate(0, 0, -midDays).Unix()},
		{"metrics_10m", now.AddDate(0, 0, -totalDays).Unix()},
		{"latency_10m", now.AddDate(0, 0, -totalDays).Unix()},
	}
	for _, j := range jobs {
		if err := r.cleanupBatch(j.table, j.cut); err != nil {
			return err
		}
	}
	return nil
}

// cleanupBatch 分批删除 table 中 ts < cut 的行，直到删干净。
//
// ★ 这里**不能**再引入一个"上一轮删到哪"的上界（hi）来做推进标记。
// 踩过的坑：写成 `WHERE ts < cut AND ts < hi`，其中 hi 取上一轮批次的 MAX(ts)——
// 看似"防止重复扫描同一批"，实际上上一轮的 DELETE 已经把 `ts <= hi` 的行全删了，
// 所以下一轮的 `ts < hi` 必然命中 0 行，`MAX(ts)` 返回 NULL，循环**提前终止**，
// 剩下 7000/12000 行原封不动地留着（测试 TestCleanupTimeSeriesLargeBatch 精确复现）。
//
// 正确的推进方式：**每轮都取"当前剩余行里最老的 5000 行"**，
// 进度由 DELETE 本身保证 —— 删掉的行不会再出现在下一轮的子查询里。
// 终止条件只有两个：子查询为空（MAX(ts) 为 NULL），或本轮删了 0 行（防御性）。
func (r *Repo) cleanupBatch(table string, cut int64) error {
	const batch = 5000
	for {
		// 用 sql.NullInt64 收 MAX(ts)：子查询为空时它是 NULL，
		// 直接 Scan 进 int64 会报 "converting NULL to int64 is unsupported"，
		// 而"没有更老的行了"是正常结束条件，不该走错误分支。
		var next sql.NullInt64
		err := r.db.Read.QueryRow(
			`SELECT MAX(ts) FROM (SELECT ts FROM `+table+` WHERE ts < ? ORDER BY ts LIMIT ?)`,
			cut, batch).Scan(&next)
		if err != nil {
			return err
		}
		if !next.Valid {
			return nil // 已经没有 ts < cut 的行
		}
		res, err := r.db.Write.Exec(`DELETE FROM `+table+` WHERE ts < ? AND ts <= ?`, cut, next.Int64)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}
