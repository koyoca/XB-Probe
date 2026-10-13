package store

import (
	"database/sql"
	"errors"

	"github.com/yourorg/probe/internal/auth"
)

// 实现 auth.SessionStore（9.3）。库内只存 SHA-256(会话 ID)，不存明文。

// CreateSession 保存会话。
func (r *Repo) CreateSession(idHash []byte, adminID int64, csrf, ip, ua string, createdAt, expiresAt int64) error {
	_, err := r.db.Write.Exec(
		`INSERT INTO sessions(id_hash, admin_id, csrf_token, ip, user_agent, created_at, expires_at)
		 VALUES(?,?,?,?,?,?,?)`, idHash, adminID, csrf, ip, ua, createdAt, expiresAt)
	return err
}

// LookupSession 按哈希读取会话。
func (r *Repo) LookupSession(idHash []byte) (*auth.Session, error) {
	s := &auth.Session{IDHash: idHash}
	err := r.db.Read.QueryRow(
		`SELECT admin_id, csrf_token, COALESCE(ip,''), COALESCE(user_agent,''), created_at, expires_at
		 FROM sessions WHERE id_hash=?`, idHash).
		Scan(&s.AdminID, &s.CSRFToken, &s.IP, &s.UserAgent, &s.CreatedAt, &s.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// TouchSession 续期空闲 TTL。
func (r *Repo) TouchSession(idHash []byte, expiresAt int64) error {
	_, err := r.db.Write.Exec(`UPDATE sessions SET expires_at=? WHERE id_hash=?`, expiresAt, idHash)
	return err
}

// DeleteSession 删除会话。
func (r *Repo) DeleteSession(idHash []byte) error {
	_, err := r.db.Write.Exec(`DELETE FROM sessions WHERE id_hash=?`, idHash)
	return err
}

// CleanupSessions 清理过期会话（保留 90 天后一并清理，见 6.5 补充）。
func (r *Repo) CleanupSessions(now int64) (int64, error) {
	res, err := r.db.Write.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// admins
// ---------------------------------------------------------------------------

// Admin 是 admins 表的映射。
type Admin struct {
	ID          int64
	Username    string
	PWHash      string
	TOTPSecret  []byte
	FailedCount int
	LockedUntil int64
	CreatedAt   int64
}

// GetAdminByUsername 按用户名读取管理员。
func (r *Repo) GetAdminByUsername(username string) (*Admin, error) {
	a := &Admin{}
	err := r.db.Read.QueryRow(
		`SELECT id, username, pw_hash, totp_secret, failed_count, locked_until, created_at
		 FROM admins WHERE username=?`, username).
		Scan(&a.ID, &a.Username, &a.PWHash, &a.TOTPSecret, &a.FailedCount, &a.LockedUntil, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// GetAdminByID 按 ID 读取管理员。
func (r *Repo) GetAdminByID(id int64) (*Admin, error) {
	a := &Admin{}
	err := r.db.Read.QueryRow(
		`SELECT id, username, pw_hash, totp_secret, failed_count, locked_until, created_at
		 FROM admins WHERE id=?`, id).
		Scan(&a.ID, &a.Username, &a.PWHash, &a.TOTPSecret, &a.FailedCount, &a.LockedUntil, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// CreateAdmin 创建管理员（首次启动 init-admin 使用，9.3）。
func (r *Repo) CreateAdmin(username, pwHash string, totpSecret []byte, now int64) (int64, error) {
	res, err := r.db.Write.Exec(
		`INSERT INTO admins(username, pw_hash, totp_secret, created_at) VALUES(?,?,?,?)`,
		username, pwHash, totpSecret, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecordLoginFailure 记录登录失败并按策略锁定（9.3：5 次锁定 15 分钟）。
func (r *Repo) RecordLoginFailure(adminID int64, maxFailed int, lockSeconds int64, now int64) error {
	tx, err := r.db.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var failed int
	if err := tx.QueryRow(`SELECT failed_count FROM admins WHERE id=?`, adminID).Scan(&failed); err != nil {
		return err
	}
	failed++
	var lockedUntil int64
	if failed >= maxFailed {
		lockedUntil = now + lockSeconds
		failed = 0
	}
	if _, err := tx.Exec(`UPDATE admins SET failed_count=?, locked_until=? WHERE id=?`,
		failed, lockedUntil, adminID); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetLoginFailures 登录成功后清零失败计数。
func (r *Repo) ResetLoginFailures(adminID int64) error {
	_, err := r.db.Write.Exec(
		`UPDATE admins SET failed_count=0, locked_until=0 WHERE id=?`, adminID)
	return err
}

// UpdatePassword 修改密码。
func (r *Repo) UpdatePassword(adminID int64, pwHash string) error {
	_, err := r.db.Write.Exec(`UPDATE admins SET pw_hash=? WHERE id=?`, pwHash, adminID)
	return err
}

// ---------------------------------------------------------------------------
// TOTP 启用流程与恢复码（6.2）
// ---------------------------------------------------------------------------

// SetTOTPSecret 写入/清除管理员的 TOTP 密钥。
//
// secret 为 nil 表示关闭 TOTP（登录时 len(TOTPSecret)==0 即跳过二次验证）。
// 这里同时清零失败计数与锁定：刚绑定完密钥的管理员往往已经因为反复试码
// 被锁过，不清理的话"绑定成功却仍然登不进"，看起来像功能坏了。
func (r *Repo) SetTOTPSecret(adminID int64, secret []byte) error {
	_, err := r.db.Write.Exec(
		`UPDATE admins SET totp_secret=?, failed_count=0, locked_until=0 WHERE id=?`,
		secret, adminID)
	return err
}

// HasTOTP 判定管理员是否已启用 TOTP。
func (r *Repo) HasTOTP(adminID int64) (bool, error) {
	var n int
	err := r.db.Read.QueryRow(
		`SELECT COUNT(*) FROM admins WHERE id=? AND totp_secret IS NOT NULL AND length(totp_secret)>0`,
		adminID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ReplaceRecoveryCodes 用一批新的恢复码哈希替换该管理员的全部旧码，返回写入条数。
//
// 整体在一个事务里"先删后插"：不做删除的话，重新生成后旧码仍然可用——
// 而重新生成的典型场景恰恰是"我怀疑恢复码泄露了"，留着旧码等于白重置。
func (r *Repo) ReplaceRecoveryCodes(adminID int64, hashes [][]byte, now int64) (int, error) {
	tx, err := r.db.Write.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM admin_recovery_codes WHERE admin_id=?`, adminID); err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(
		`INSERT INTO admin_recovery_codes(admin_id, code_hash, created_at) VALUES(?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = stmt.Close() }()

	for _, h := range hashes {
		if _, err := stmt.Exec(adminID, h, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(hashes), nil
}

// CountRecoveryCodes 返回该管理员**尚未使用**的恢复码数量。
//
// 后台需要这个数字来提醒"恢复码快用完了"，因此只统计未消费的。
func (r *Repo) CountRecoveryCodes(adminID int64) (total, unused int, err error) {
	err = r.db.Read.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN used_at IS NULL THEN 1 ELSE 0 END),0)
		 FROM admin_recovery_codes WHERE admin_id=?`, adminID).Scan(&total, &unused)
	return total, unused, err
}

// ConsumeRecoveryCode 尝试消费一个恢复码。
//
// 返回 (adminID, true, nil) 表示校验成功且已标记为已用；
// 返回 (0, false, nil) 表示这个码不存在或已经被用过。
//
// 必须在**同一事务**里"查 + 置 used_at"，否则同一个恢复码可以被并发请求
// 重复使用（与注册码的原子消费是同一类问题，见 EnrollCodeAtomicConsumption）。
// 这里用 UPDATE ... WHERE used_at IS NULL 的受影响行数来判定：
// 只有真正把某一行从"未用"改成"已用"的那个请求才算成功，天然原子。
func (r *Repo) ConsumeRecoveryCode(codeHash []byte, now int64) (int64, bool, error) {
	res, err := r.db.Write.Exec(
		`UPDATE admin_recovery_codes SET used_at=? WHERE code_hash=? AND used_at IS NULL`,
		now, codeHash)
	if err != nil {
		return 0, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if n == 0 {
		return 0, false, nil
	}
	// 消费成功后再回查所属管理员（此时该行一定存在且唯一）。
	var adminID int64
	if err := r.db.Read.QueryRow(
		`SELECT admin_id FROM admin_recovery_codes WHERE code_hash=?`, codeHash).Scan(&adminID); err != nil {
		return 0, false, err
	}
	return adminID, true, nil
}
