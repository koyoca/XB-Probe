package store

import "database/sql"

// 访问日志的读写（对应 migration 0011）。
//
// 列表只做"时间倒序 + 可选按 IP 精确匹配"，不做模糊搜索：这张表增长很快
// （一次页面加载就是十几个请求），LIKE 扫描会让翻页越来越慢。需要查某个 IP
// 就从页面上的链接点进来（ip 精确匹配命中复合索引）。

// AccessLogEntry 是一条访问记录。
type AccessLogEntry struct {
	ID     int64  `json:"id"`
	TS     int64  `json:"ts"`
	IP     string `json:"ip"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Bytes  int64  `json:"bytes"`
	DurMs  int64  `json:"dur_ms"`
	UA     string `json:"ua,omitempty"`
}

// InsertAccessLog 写入一条访问记录。
func (r *Repo) InsertAccessLog(e AccessLogEntry) error {
	_, err := r.db.Write.Exec(
		`INSERT INTO access_log(ts, ip, method, path, status, bytes, dur_ms, ua)
		 VALUES(?,?,?,?,?,?,?,?)`,
		e.TS, e.IP, e.Method, e.Path, e.Status, e.Bytes, e.DurMs, e.UA)
	return err
}

// CountAccessLog 返回总条数（可按 IP 过滤，ip 为空表示不过滤）。
func (r *Repo) CountAccessLog(ip string) (int64, error) {
	var n int64
	var err error
	if ip == "" {
		err = r.db.Read.QueryRow(`SELECT COUNT(*) FROM access_log`).Scan(&n)
	} else {
		err = r.db.Read.QueryRow(`SELECT COUNT(*) FROM access_log WHERE ip=?`, ip).Scan(&n)
	}
	return n, err
}

// ListAccessLog 按时间倒序分页读取。
func (r *Repo) ListAccessLog(limit, offset int, ip string) ([]AccessLogEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var (
		rows *sql.Rows
		err  error
	)
	const cols = `SELECT id, ts, ip, method, path, status, bytes, dur_ms, COALESCE(ua,'')`
	if ip == "" {
		rows, err = r.db.Read.Query(cols+` FROM access_log ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	} else {
		rows, err = r.db.Read.Query(cols+` FROM access_log WHERE ip=? ORDER BY id DESC LIMIT ? OFFSET ?`, ip, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AccessLogEntry, 0, limit)
	for rows.Next() {
		var e AccessLogEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.IP, &e.Method, &e.Path, &e.Status,
			&e.Bytes, &e.DurMs, &e.UA); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneAccessLog 删除早于 beforeTs 的记录，返回删除行数。
func (r *Repo) PruneAccessLog(beforeTs int64) (int64, error) {
	res, err := r.db.Write.Exec(`DELETE FROM access_log WHERE ts < ?`, beforeTs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ClearAccessLog 清空整张表，返回删除行数。
func (r *Repo) ClearAccessLog() (int64, error) {
	res, err := r.db.Write.Exec(`DELETE FROM access_log`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
