package store

// CountActiveEnrollCodes 统计当前有效的注册码数量（install.mode=on_demand 时使用，12.8）。
func (r *Repo) CountActiveEnrollCodes(now int64) (int, error) {
	var n int
	err := r.db.Read.QueryRow(
		`SELECT COUNT(*) FROM enroll_codes WHERE used_at IS NULL AND expires_at>?`, now).Scan(&n)
	return n, err
}
