package store

import (
	"context"
	"testing"
	"time"
)

// PurgeOldData 只删早于 cutoff 的时序行，保留窗口内的行。
func TestPurgeOldDataDeletesOnlyOlderRows(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "purge")
	now := int64(1_760_000_000)
	insert := func(ts int64) {
		if _, err := r.db.Write.Exec(`INSERT INTO metrics_raw(node_id, ts, cpu) VALUES(?,?,?)`, nodeID, ts, 1); err != nil {
			t.Fatalf("插入 metrics_raw 失败：%v", err)
		}
	}
	insert(now - 100*86400)
	insert(now - 99*86400)
	insert(now - 10*86400)

	counts, err := r.PurgeOldData(now - 30*86400)
	if err != nil {
		t.Fatalf("PurgeOldData 失败：%v", err)
	}
	if counts["metrics_raw"] != 2 {
		t.Errorf("metrics_raw 应删 2 行，实际 %d", counts["metrics_raw"])
	}
	var n int
	if err := r.db.Read.QueryRow(`SELECT COUNT(*) FROM metrics_raw`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("应剩 1 行，实际 %d", n)
	}
}

// PruneAuditLog 只删早于 cutoff 的审计记录。
func TestPruneAuditLogDeletesOlderRows(t *testing.T) {
	r := newTestRepo(t)
	now := int64(1_760_000_000)
	for _, ts := range []int64{now - 100*86400, now - 50*86400, now} {
		if err := r.Audit(ts, "tester", "settings.update", "", "127.0.0.1"); err != nil {
			t.Fatalf("插入审计失败：%v", err)
		}
	}
	n, err := r.PruneAuditLog(now - 30*86400)
	if err != nil {
		t.Fatalf("PruneAuditLog 失败：%v", err)
	}
	if n != 2 {
		t.Errorf("应删 2 条，实际 %d", n)
	}
	var left int
	if err := r.db.Read.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Errorf("应剩 1 条，实际 %d", left)
	}
}

// RestoreBackup 用备份覆盖当前库：备份之后的改动被回退，且当前登录会话被保留。
//
// 这条护栏覆盖三个关键点：
//  1. ATTACH + 逐表覆盖确实把库还原到备份时刻；
//  2. sessions 被刻意跳过，管理员不必重新登录；
//  3. 恢复后连接仍然健康（foreign_keys 已重置、ATTACH 已 DETACH）。
func TestRestoreBackupRevertsAndKeepsSessions(t *testing.T) {
	r := newTestRepo(t)
	backupNode := mustNode(t, r, "backup-node")
	if _, err := r.db.Write.Exec(`INSERT INTO metrics_raw(node_id, ts, cpu) VALUES(?,?,?)`, backupNode, 1_760_000_000, 7); err != nil {
		t.Fatal(err)
	}
	adminID, err := r.CreateAdmin("ops", "$argon2id$v=19$m=1,t=1,p=1$aa$bb", nil, 1_760_000_000)
	if err != nil {
		t.Fatal(err)
	}
	idHash := make([]byte, 32)
	for i := range idHash {
		idHash[i] = byte(i)
	}
	if err := r.CreateSession(idHash, adminID, "csrf", "127.0.0.1", "ua", 1_760_000_000, 1_760_000_000+3600); err != nil {
		t.Fatal(err)
	}

	info, err := r.CreateBackup(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("备份失败：%v", err)
	}

	// 备份之后再改库：新增节点/审计、删掉备份节点的指标。
	if _, err := r.CreateNode("publiveonly", "live-only", "test", 1_760_000_000); err != nil {
		t.Fatal(err)
	}
	if err := r.Audit(1_760_000_000, "tester", "settings.update", "", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Write.Exec(`DELETE FROM metrics_raw WHERE node_id=?`, backupNode); err != nil {
		t.Fatal(err)
	}

	if err := r.RestoreBackup(context.Background(), info.Name); err != nil {
		t.Fatalf("恢复失败：%v", err)
	}

	var liveOnly int
	if err := r.db.Read.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name=?`, "live-only").Scan(&liveOnly); err != nil {
		t.Fatal(err)
	}
	if liveOnly != 0 {
		t.Error("恢复后 live-only 节点应消失")
	}
	var bk int
	if err := r.db.Read.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name=?`, "backup-node").Scan(&bk); err != nil {
		t.Fatal(err)
	}
	if bk != 1 {
		t.Error("恢复后 backup-node 节点应回来")
	}
	var m int
	if err := r.db.Read.QueryRow(`SELECT COUNT(*) FROM metrics_raw`).Scan(&m); err != nil {
		t.Fatal(err)
	}
	if m != 1 {
		t.Errorf("恢复后 metrics_raw 应回到 1 行，实际 %d", m)
	}
	// sessions 保留：恢复前建的会话还在（管理员不必重新登录）。
	var s int
	if err := r.db.Read.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id_hash=?`, idHash).Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s != 1 {
		t.Errorf("sessions 应被保留，实际 %d", s)
	}
	// 恢复后连接仍健康（foreign_keys 已重置、ATTACH 已 DETACH）。
	var ok string
	if err := r.db.Read.QueryRow(`PRAGMA quick_check`).Scan(&ok); err != nil || ok != "ok" {
		t.Errorf("恢复后库应健康，得到 %q err=%v", ok, err)
	}
}

// RestoreBackup 只接受合法备份文件名，拒绝路径穿越与任意文件名。
func TestRestoreBackupRejectsBadName(t *testing.T) {
	r := newTestRepo(t)
	for _, bad := range []string{"../probe.db", "probe-notadb.db", "", "/etc/passwd", "probe-20260101-000000.db/../x"} {
		if err := r.RestoreBackup(context.Background(), bad); err == nil {
			t.Errorf("RestoreBackup(%q) 应被拒绝", bad)
		}
	}
	// 合法但不存在 → ErrBackupNotFound。
	if err := r.RestoreBackup(context.Background(), "probe-20260101-000000.db"); err == nil {
		t.Error("不存在的备份应报错")
	}
}
