package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/yourorg/probe/internal/model"
)

// 探测目标读写（6.3 / A4 / 13.4.6）。
//
// 关于 host/port/method 的"不可变"：设计文档 A4 原本要求"修改即新建目标并
// 停用旧目标"，理由是同一 target_id 下混入不同探测对象的延迟历史会让曲线
// 失去意义。实际使用中这条规则更常被用成"填错了想改一下"，强制新建会让历史
// 曲线在新 id 下凭空断掉。因此这里放开为**可原地修改**，代价是历史曲线的
// 语义连续性需要管理员自己留意（后台编辑弹窗里有明确提示）。
// 这是对设计文档 A4 的一处**有意偏离**：放开为可原地修改，换取"填错了能改"。

// ListProbeTargets 列出全局目标（nodeID=0）或某节点专属目标，并附上全局目标。
func (r *Repo) ListProbeTargets(nodeID int64) ([]*model.ProbeTarget, error) {
	q := `SELECT id, COALESCE(node_id,0), name, carrier, host, COALESCE(port,0), method, timeout_ms, enabled, COALESCE(show_in_bars,1)
	      FROM probe_targets WHERE (node_id IS NULL`
	var args []any
	if nodeID > 0 {
		q += ` OR node_id=?`
		args = append(args, nodeID)
	}
	q += `) ORDER BY id`
	return r.queryProbeTargets(q, args...)
}

// ListEveryProbeTarget 返回库里的全部目标（全局 + 所有节点专属）。
//
// 管理端的"探测目标"页必须用它而不是 ListProbeTargets(0)：
// 后者按设计只返回全局目标，节点专属目标会被整个吞掉——
// 管理员刚创建的专属目标"创建成功却在列表里消失"，就是这个原因。
func (r *Repo) ListEveryProbeTarget() ([]*model.ProbeTarget, error) {
	q := `SELECT id, COALESCE(node_id,0), name, carrier, host, COALESCE(port,0), method, timeout_ms, enabled, COALESCE(show_in_bars,1)
	      FROM probe_targets ORDER BY id`
	return r.queryProbeTargets(q)
}

func (r *Repo) queryProbeTargets(q string, args ...any) ([]*model.ProbeTarget, error) {
	rows, err := r.db.Read.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ProbeTarget
	for rows.Next() {
		t := &model.ProbeTarget{}
		var enabled, showInBars int
		if err := rows.Scan(&t.ID, &t.NodeID, &t.Name, &t.Carrier, &t.Host, &t.Port, &t.Method, &t.TimeoutMs, &enabled, &showInBars); err != nil {
			return nil, err
		}
		t.Enabled = enabled == 1
		t.ShowInBars = showInBars == 1
		out = append(out, t)
	}
	return out, rows.Err()
}

// LatencyBucketsForNode 读取某节点窗口内的 10 分钟延迟桶，并按目标补上名字与运营组。
//
// 供节点卡片的"延迟情况"点阵聚合使用：调度器周期调用（而不是快照构建时调用——
// 快照每 3 秒重建一次，绝不能在里面查库）。目标被删除后，其历史桶的名字/运营组
// 无法回填（JOIN 不上），这些桶直接丢弃：点阵只表达"当前目标的近期状况"。
//
// **JOIN 必须带上目标的作用域守卫**：只保留"全局目标（node_id IS NULL）或本节点
// 专属目标（node_id = b.node_id）"。少这一条时，只要某行的 target_id 指向**别的
// 节点的专属目标**，它就会被算进本节点的点阵——表现是"没有专属目标的节点照样
// 渲染出别人的专属目标"。这类脏行可能来自历史误写或越权的 Agent 上报
// （Handler 侧另有一道准入过滤，见 internal/api/agent.go 的 allowedTargetIDs）。
//
// SELECT 里带 b.target_id 与 t.name：点阵现在**按目标一行**（行标签是目标名，
// 不是运营组常量），聚合键就是 target_id。同一目标的历史桶可能跨"全局/专属"
// 变更，但 target_id 对本节点而言仍是稳定键。
func (r *Repo) LatencyBucketsForNode(nodeID, from int64) ([]model.LatencyBucket, error) {
	return r.latencyBuckets("latency_10m", nodeID, from)
}

// LatencyRawBucketsForNode 与 LatencyBucketsForNode 相同，但读**原始层**
// （latency_raw，探测粒度）。点阵的短时间窗（30m/1h）落在 10 分钟层上只有
// 3/6 个桶，必须用原始层才有有效数据；表选择由 state.LatencyBarLayout 决定。
// latency_raw 的保留期（retention.raw_hours，默认 24 小时）覆盖这两个窗口。
func (r *Repo) LatencyRawBucketsForNode(nodeID, from int64) ([]model.LatencyBucket, error) {
	return r.latencyBuckets("latency_raw", nodeID, from)
}

// latencyBuckets 是两层延迟桶共用的读取实现。
//
// 表名只接受本文件内的两个常量调用点（latency_10m / latency_raw），
// 不接受任何外部输入——这里按表名拼接 SQL，绝不能变成注入面。
func (r *Repo) latencyBuckets(table string, nodeID, from int64) ([]model.LatencyBucket, error) {
	rows, err := r.db.Read.Query(
		`SELECT b.target_id, COALESCE(t.name,''), COALESCE(t.carrier,'custom'),
		        b.ts, b.rtt_avg, b.loss
		 FROM `+table+` b JOIN probe_targets t ON t.id = b.target_id
		 WHERE b.node_id = ? AND b.ts >= ?
		   AND (t.node_id IS NULL OR t.node_id = b.node_id)
		   AND t.enabled = 1 AND COALESCE(t.show_in_bars,1) = 1`, nodeID, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.LatencyBucket
	for rows.Next() {
		var b model.LatencyBucket
		var rtt sql.NullInt64
		if err := rows.Scan(&b.TargetID, &b.Name, &b.Carrier, &b.TS, &rtt, &b.Loss); err != nil {
			continue
		}
		if rtt.Valid {
			v := rtt.Int64
			b.RTT = &v
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListGlobalProbeTargets 只列出全局目标。
func (r *Repo) ListGlobalProbeTargets() ([]*model.ProbeTarget, error) {
	return r.ListProbeTargets(0)
}

// ProbeTargetMeta 是探测目标的展示元数据。
//
// 刻意**不含** host/port：这份数据会被送进公开 DTO 与看板，
// 少一个字段就少一个泄漏面。
type ProbeTargetMeta struct {
	ID      int64
	NodeID  int64 // 0 表示全局目标
	Name    string
	Carrier string // telecom/unicom/mobile/custom
	Enabled bool
}

// ListAllProbeTargetMeta 一次读出全部目标的展示元数据，按 id 索引。
//
// 为什么需要它：调度器每秒都要把最新探测结果汇总成延迟指示，而它手上只有
// 上报里的 target_id，没有名称与运营商就只能退化成"目标 #1"——这正是看板上
// 延迟图例一直写着"目标#1/目标#2"的原因。每秒查库不可接受，因此由调度器在
// 元数据同步（15 秒）里刷新一份内存缓存，而不是在热路径里查。
func (r *Repo) ListAllProbeTargetMeta() (map[int64]ProbeTargetMeta, error) {
	rows, err := r.db.Read.Query(
		`SELECT id, COALESCE(node_id,0), name, carrier, enabled FROM probe_targets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]ProbeTargetMeta{}
	for rows.Next() {
		var m ProbeTargetMeta
		var enabled int
		if err := rows.Scan(&m.ID, &m.NodeID, &m.Name, &m.Carrier, &enabled); err != nil {
			return nil, err
		}
		m.Enabled = enabled == 1
		out[m.ID] = m
	}
	return out, rows.Err()
}

// CreateProbeTarget 新建探测目标。
func (r *Repo) CreateProbeTarget(t *model.ProbeTarget) (int64, error) {
	var nodeID any
	if t.NodeID > 0 {
		nodeID = t.NodeID
	}
	res, err := r.db.Write.Exec(
		`INSERT INTO probe_targets(node_id, name, carrier, host, port, method, timeout_ms, enabled, show_in_bars)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		nodeID, t.Name, t.Carrier, t.Host, nullIfZero(t.Port), t.Method, t.TimeoutMs,
		boolToInt(t.Enabled), boolToInt(t.ShowInBars))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RenameOrToggleTarget 只修改名称与启停（A4）。
//
// 保留这个窄接口是有意的：调度器/迁移脚本里"只想改个名字"的场景不该有能力
// 动到 host/port。需要改全部字段请用 UpdateProbeTarget。
func (r *Repo) RenameOrToggleTarget(id int64, name string, enabled bool) error {
	_, err := r.db.Write.Exec(
		`UPDATE probe_targets SET name=?, enabled=? WHERE id=?`, name, boolToInt(enabled), id)
	return err
}

// UpdateProbeTarget 完整更新一个探测目标（名称/运营商/主机/端口/方式/超时/启停）。
//
// 调用方负责先校验（validateTargetFields）；这里只做写库。
// 不更新 node_id：目标的作用域（全局 / 节点专属）决定它下发给哪些 Agent，
// 改作用域等于换一条目标，不属于"编辑"的范畴。
func (r *Repo) UpdateProbeTarget(t *model.ProbeTarget) error {
	_, err := r.db.Write.Exec(
		`UPDATE probe_targets SET name=?, carrier=?, host=?, port=?, method=?, timeout_ms=?, enabled=?, show_in_bars=?
		 WHERE id=?`,
		t.Name, t.Carrier, t.Host, nullIfZero(t.Port), t.Method, t.TimeoutMs,
		boolToInt(t.Enabled), boolToInt(t.ShowInBars), t.ID)
	return err
}

// DisableTarget 停用目标（“修改即新建并停用旧目标”）。
func (r *Repo) DisableTarget(id int64) error {
	_, err := r.db.Write.Exec(`UPDATE probe_targets SET enabled=0 WHERE id=?`, id)
	return err
}

// DeleteProbeTarget 删除目标，并清理其延迟数据（6.5 数据清理补充）。
func (r *Repo) DeleteProbeTarget(id int64) error {
	tx, err := r.db.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM probe_targets WHERE id=?`, id); err != nil {
		return err
	}
	for _, t := range []string{"latency_raw", "latency_10m"} {
		if _, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE target_id=?`, t), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetProbeTarget 按 ID 读取目标。
func (r *Repo) GetProbeTarget(id int64) (*model.ProbeTarget, error) {
	t := &model.ProbeTarget{}
	var enabled, showInBars int
	err := r.db.Read.QueryRow(
		`SELECT id, COALESCE(node_id,0), name, carrier, host, COALESCE(port,0), method, timeout_ms, enabled, COALESCE(show_in_bars,1)
		 FROM probe_targets WHERE id=?`, id).
		Scan(&t.ID, &t.NodeID, &t.Name, &t.Carrier, &t.Host, &t.Port, &t.Method, &t.TimeoutMs, &enabled, &showInBars)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Enabled = enabled == 1
	t.ShowInBars = showInBars == 1
	return t, nil
}

func nullIfZero(v int) any {
	if v == 0 {
		return nil
	}
	return v
}
