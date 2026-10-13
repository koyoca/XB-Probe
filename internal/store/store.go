// Package store 是 SQLite 持久层，实现各上层包所依赖的窄接口
// （auth.TokenStore / auth.SessionStore / alert.Store）。
//
// 关键点：
//   - 所有写操作走 db.Store.Write（唯一写连接）。
//   - 注册码消费与令牌重签发使用带条件的 UPDATE 并检查受影响行数（S6 / 13.4.7）。
//   - 审计写入与业务写入放在同一事务内。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yourorg/probe/internal/db"
	"github.com/yourorg/probe/internal/model"
)

// Repo 是仓储。
type Repo struct {
	db *db.Store
}

// New 创建仓储。
func New(s *db.Store) *Repo { return &Repo{db: s} }

// DB 暴露底层连接（供维护任务使用）。
func (r *Repo) DB() *db.Store { return r.db }

// ---------------------------------------------------------------------------
// settings
// ---------------------------------------------------------------------------

// GetSetting 读取单项设置。
func (r *Repo) GetSetting(key string) (string, bool, error) {
	var v string
	err := r.db.Read.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// GetSettingDefault 读取设置，缺失时返回默认值。
func (r *Repo) GetSettingDefault(key, def string) string {
	v, ok, err := r.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	return v
}

// GetIntSetting 读取整型设置。
func (r *Repo) GetIntSetting(key string, def int64) int64 {
	v, ok, err := r.GetSetting(key)
	if err != nil || !ok || v == "" {
		return def
	}
	var n int64
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}

// AllSettings 读取全部设置。
func (r *Repo) AllSettings() (map[string]string, error) {
	rows, err := r.db.Read.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetSetting 写入设置（upsert）。
func (r *Repo) SetSetting(key, value string, now int64) error {
	_, err := r.db.Write.Exec(
		`INSERT INTO settings(key, value, updated_at) VALUES(?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, now)
	return err
}

// ---------------------------------------------------------------------------
// auth.TokenStore
// ---------------------------------------------------------------------------

// FindTokenByHash 按哈希查令牌。
func (r *Repo) FindTokenByHash(hash []byte) (tokenID, nodeID int64, revoked bool, err error) {
	var revokedAt sql.NullInt64
	err = r.db.Read.QueryRow(
		`SELECT id, node_id, revoked_at FROM node_tokens WHERE token_hash=?`, hash).
		Scan(&tokenID, &nodeID, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return tokenID, nodeID, revokedAt.Valid && revokedAt.Int64 > 0, nil
}

// NodeStatus 返回节点状态。注：节点运行态未入库时也可能存在，此查询仅取库内值。
func (r *Repo) NodeStatus(nodeID int64) (string, error) {
	var st string
	err := r.db.Read.QueryRow(`SELECT status FROM nodes WHERE id=?`, nodeID).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("node %d 不存在", nodeID)
	}
	return st, err
}

// TouchToken 更新令牌最后使用时间与来源 IP。
// 首次使用时（last_used_at IS NULL）在同一事务内吊销同节点其他令牌（13.4.7）。
func (r *Repo) TouchToken(tokenID, nodeID int64, ip string, at int64) error {
	tx, err := r.db.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var firstUse bool
	var existing sql.NullInt64
	if err := tx.QueryRow(`SELECT last_used_at FROM node_tokens WHERE id=?`, tokenID).Scan(&existing); err != nil {
		return err
	}
	firstUse = !existing.Valid

	if _, err := tx.Exec(
		`UPDATE node_tokens SET last_used_at=?, last_ip=? WHERE id=?`, at, ip, tokenID); err != nil {
		return err
	}
	if firstUse {
		// 新令牌首次使用成功 → 同节点其余未吊销的旧令牌自动失效（8.2 第 3 步）。
		if _, err := tx.Exec(
			`UPDATE node_tokens SET revoked_at=? WHERE node_id=? AND id<>? AND revoked_at IS NULL`,
			at, nodeID, tokenID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CreateToken 创建令牌记录，返回令牌 ID。
func (r *Repo) CreateToken(nodeID int64, hash []byte, hint string, now int64) (int64, error) {
	res, err := r.db.Write.Exec(
		`INSERT INTO node_tokens(node_id, token_hash, token_hint, created_at) VALUES(?,?,?,?)`,
		nodeID, hash, hint, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RevokeToken 吊销指定令牌（8.2 第 1 步）。
func (r *Repo) RevokeToken(nodeID, tokenID, now int64) error {
	_, err := r.db.Write.Exec(
		`UPDATE node_tokens SET revoked_at=? WHERE id=? AND node_id=? AND revoked_at IS NULL`,
		now, tokenID, nodeID)
	return err
}

// ListTokens 列出某节点的令牌（仅提示与最后使用信息）。
func (r *Repo) ListTokens(nodeID int64) ([]model.Token, error) {
	rows, err := r.db.Read.Query(
		`SELECT id, node_id, token_hint, created_at, COALESCE(last_used_at,0),
		        COALESCE(last_ip,''), COALESCE(revoked_at,0)
		 FROM node_tokens WHERE node_id=? ORDER BY id DESC`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Token
	for rows.Next() {
		var t model.Token
		if err := rows.Scan(&t.ID, &t.NodeID, &t.TokenHint, &t.CreatedAt, &t.LastUsedAt, &t.LastIP, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 注册码（S6 / 13.4.7）
// ---------------------------------------------------------------------------

// CreateEnrollCode 创建一次性注册码（30 分钟有效，仅存哈希）。同一节点最多 3 个有效码。
func (r *Repo) CreateEnrollCode(nodeID int64, hash []byte, expiresAt int64, createdBy, platform string, now int64) error {
	tx, err := r.db.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var active int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM enroll_codes WHERE node_id=? AND used_at IS NULL AND expires_at>?`,
		nodeID, now).Scan(&active); err != nil {
		return err
	}
	if active >= 3 {
		return fmt.Errorf("该节点已有 3 个有效注册码，请先等待其过期或被使用")
	}
	if _, err := tx.Exec(
		`INSERT INTO enroll_codes(node_id, code_hash, expires_at, created_at, created_by, platform)
		 VALUES(?,?,?,?,?,?)`, nodeID, hash, expiresAt, now, createdBy, platform); err != nil {
		return err
	}
	if err := r.insertAuditTx(tx, now, createdBy, "enroll_code.create", fmt.Sprint(nodeID), ""); err != nil {
		return err
	}
	return tx.Commit()
}

// ConsumeEnrollCode 原子消费注册码，返回绑定的节点 ID。
// 使用带条件的 UPDATE 并检查受影响行数，避免并发下同一码被用两次（S6）。
func (r *Repo) ConsumeEnrollCode(hash []byte, now int64) (nodeID int64, ok bool, err error) {
	tx, err := r.db.Write.Begin()
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(
		`UPDATE enroll_codes SET used_at=? WHERE code_hash=? AND used_at IS NULL AND expires_at>?`,
		now, hash, now)
	if err != nil {
		return 0, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if n != 1 {
		return 0, false, nil // 无效码/已使用/已过期
	}
	if err := tx.QueryRow(`SELECT node_id FROM enroll_codes WHERE code_hash=?`, hash).Scan(&nodeID); err != nil {
		return 0, false, err
	}
	if err := r.insertAuditTx(tx, now, fmt.Sprintf("node:%d", nodeID), "enroll_code.consume", "", ""); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return nodeID, true, nil
}

// ---------------------------------------------------------------------------
// 审计
// ---------------------------------------------------------------------------

func (r *Repo) insertAuditTx(tx *sql.Tx, ts int64, actor, action, target, ip string) error {
	_, err := tx.Exec(
		`INSERT INTO audit_log(ts, actor, action, target, ip) VALUES(?,?,?,?,?)`,
		ts, actor, action, target, ip)
	return err
}

// Audit 记录一条审计日志（9.4）。
func (r *Repo) Audit(ts int64, actor, action, target, ip string) error {
	return r.insertAuditTx2(ts, actor, action, target, ip)
}

func (r *Repo) insertAuditTx2(ts int64, actor, action, target, ip string) error {
	_, err := r.db.Write.Exec(
		`INSERT INTO audit_log(ts, actor, action, target, ip) VALUES(?,?,?,?,?)`,
		ts, actor, action, target, ip)
	return err
}

// Now 返回当前 Unix 秒（统一入口，便于测试注入）。
func Now() int64 { return time.Now().Unix() }
