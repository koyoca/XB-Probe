package store

import (
	"database/sql"
	"errors"

	"github.com/yourorg/probe/internal/alert"
)

// 实现 alert.Store，并提供规则与渠道的读写（4.5 / 6.5）。

// GetAlertState 读取某 (节点, 事件类型) 的告警状态。
func (r *Repo) GetAlertState(nodeID int64, kind string) (*alert.AlertState, error) {
	st := &alert.AlertState{NodeID: nodeID, Kind: kind}
	err := r.db.Read.QueryRow(
		`SELECT state, since, last_sent_at, COALESCE(detail,'')
		 FROM alert_state WHERE node_id=? AND kind=?`, nodeID, kind).
		Scan(&st.State, &st.Since, &st.LastSentAt, &st.Detail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return st, nil
}

// UpsertAlertState 写入告警状态。
func (r *Repo) UpsertAlertState(st alert.AlertState) error {
	_, err := r.db.Write.Exec(
		`INSERT INTO alert_state(node_id, kind, state, since, last_sent_at, detail)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(node_id, kind) DO UPDATE SET
		   state=excluded.state, since=excluded.since,
		   last_sent_at=excluded.last_sent_at, detail=excluded.detail`,
		st.NodeID, st.Kind, st.State, st.Since, st.LastSentAt, st.Detail)
	return err
}

// InsertAlertEvent 写入告警事件，返回事件 ID。
func (r *Repo) InsertAlertEvent(ev alert.Event) (int64, error) {
	res, err := r.db.Write.Exec(
		`INSERT INTO alert_events(node_id, kind, level, message, created_at, delivered)
		 VALUES(?,?,?,?,?,0)`,
		ev.NodeID, ev.Kind, ev.Level, ev.Message, ev.CreatedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// MarkDelivered 标记事件已送达。
func (r *Repo) MarkDelivered(id int64) error {
	_, err := r.db.Write.Exec(`UPDATE alert_events SET delivered=1 WHERE id=?`, id)
	return err
}

// ListAlertEvents 查询事件历史（GET /alert-events）。
//
// 分页用 limit/offset 而不是"游标（before_id）"：这两张表只有后台管理员一个人
// 在翻，不存在"翻页期间别人插入导致漏行"的并发压力；换来的是前端能直接显示
// "第 3 / 12 页"并能跳页——游标分页做不到这两件事。
func (r *Repo) ListAlertEvents(limit, offset int) ([]alert.Event, PageMeta, error) {
	limit, offset = NormalizePage(limit, offset)
	meta := PageMeta{Limit: limit, Offset: offset}

	total, err := r.countRows("alert_events")
	if err != nil {
		return nil, meta, err
	}
	meta.Total = total
	// 保留期清理会让总数变小。硬停在越界的 offset 上会得到一张空表，
	// 而分页条同时写着"第 9 / 3 页"——两边都不可信。这里把窗口拉回最后一页，
	// 并把**实际生效**的 offset 回传给前端，让页码和表格内容始终一致。
	if offset >= total && total > 0 {
		offset = ((total - 1) / limit) * limit
		meta.Offset = offset
	}

	rows, err := r.db.Read.Query(
		`SELECT id, COALESCE(node_id,0), kind, level, message, created_at, delivered
		 FROM alert_events ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, meta, err
	}
	defer rows.Close()
	out := []alert.Event{}
	for rows.Next() {
		var ev alert.Event
		var delivered int
		if err := rows.Scan(&ev.ID, &ev.NodeID, &ev.Kind, &ev.Level, &ev.Message, &ev.CreatedAt, &delivered); err != nil {
			return nil, meta, err
		}
		ev.Delivered = delivered == 1
		out = append(out, ev)
	}
	return out, meta, rows.Err()
}

// ---------------------------------------------------------------------------
// 规则
// ---------------------------------------------------------------------------

// LoadAlertRules 读取全部告警规则；表为空时返回默认规则（不写库）。
func (r *Repo) LoadAlertRules() (map[string]alert.Rule, error) {
	rows, err := r.db.Read.Query(`SELECT kind, enabled, params, cooldown_s FROM alert_rules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]alert.Rule{}
	for rows.Next() {
		var kind, params string
		var enabled, cooldown int
		if err := rows.Scan(&kind, &enabled, &params, &cooldown); err != nil {
			return nil, err
		}
		p, err := alert.ParseParams(params)
		if err != nil {
			return nil, err
		}
		out[kind] = alert.Rule{Kind: kind, Enabled: enabled == 1, Params: p, CooldownS: cooldown}
	}
	if len(out) == 0 {
		return alert.DefaultRules(), nil
	}
	return out, rows.Err()
}

// SaveAlertRule 写入规则。
func (r *Repo) SaveAlertRule(rule alert.Rule, paramsJSON string) error {
	enabled := 0
	if rule.Enabled {
		enabled = 1
	}
	_, err := r.db.Write.Exec(
		`INSERT INTO alert_rules(kind, enabled, params, cooldown_s) VALUES(?,?,?,?)
		 ON CONFLICT(kind) DO UPDATE SET enabled=excluded.enabled, params=excluded.params,
		   cooldown_s=excluded.cooldown_s`,
		rule.Kind, enabled, paramsJSON, rule.CooldownS)
	return err
}

// ---------------------------------------------------------------------------
// 渠道
// ---------------------------------------------------------------------------

// ChannelRow 是 alert_channels 的行（config_enc 为密文，绝不序列化输出）。
type ChannelRow struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	ConfigEnc []byte `json:"-"`
	Enabled   bool   `json:"enabled"`
}

// ListChannels 列出渠道。
func (r *Repo) ListChannels() ([]ChannelRow, error) {
	rows, err := r.db.Read.Query(
		`SELECT id, type, name, config_enc, enabled FROM alert_channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChannelRow
	for rows.Next() {
		var c ChannelRow
		var enabled int
		if err := rows.Scan(&c.ID, &c.Type, &c.Name, &c.ConfigEnc, &enabled); err != nil {
			return nil, err
		}
		c.Enabled = enabled == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// InsertChannel 新增渠道（config_enc 由调用方用主密钥加密，9.4）。
func (r *Repo) InsertChannel(typ, name string, configEnc []byte, enabled bool) (int64, error) {
	res, err := r.db.Write.Exec(
		`INSERT INTO alert_channels(type, name, config_enc, enabled) VALUES(?,?,?,?)`,
		typ, name, configEnc, boolToInt(enabled))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateChannel 修改渠道。
func (r *Repo) UpdateChannel(id int64, name string, configEnc []byte, enabled bool) error {
	_, err := r.db.Write.Exec(
		`UPDATE alert_channels SET name=?, config_enc=?, enabled=? WHERE id=?`,
		name, configEnc, boolToInt(enabled), id)
	return err
}

// DeleteChannel 删除渠道。
func (r *Repo) DeleteChannel(id int64) error {
	_, err := r.db.Write.Exec(`DELETE FROM alert_channels WHERE id=?`, id)
	return err
}

// AuditLogRow 是审计日志行。
type AuditLogRow struct {
	ID     int64  `json:"id"`
	TS     int64  `json:"ts"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	IP     string `json:"ip"`
}

// ListAuditLog 查询审计日志（GET /audit-log）。
func (r *Repo) ListAuditLog(limit, offset int) ([]AuditLogRow, PageMeta, error) {
	limit, offset = NormalizePage(limit, offset)
	meta := PageMeta{Limit: limit, Offset: offset}

	total, err := r.countRows("audit_log")
	if err != nil {
		return nil, meta, err
	}
	meta.Total = total
	if offset >= total && total > 0 {
		// 与 ListAlertEvents 同理：审计日志也会被保留期清理，窗口要拉回最后一页。
		offset = ((total - 1) / limit) * limit
		meta.Offset = offset
	}

	rows, err := r.db.Read.Query(
		`SELECT id, ts, actor, action, COALESCE(target,''), COALESCE(ip,'')
		 FROM audit_log ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, meta, err
	}
	defer rows.Close()
	out := []AuditLogRow{}
	for rows.Next() {
		var a AuditLogRow
		if err := rows.Scan(&a.ID, &a.TS, &a.Actor, &a.Action, &a.Target, &a.IP); err != nil {
			return nil, meta, err
		}
		out = append(out, a)
	}
	return out, meta, rows.Err()
}
