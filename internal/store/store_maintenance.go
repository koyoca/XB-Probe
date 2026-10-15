package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 维护类操作（6.6 补充）：时序数据清理与备份恢复。
//
// 这两件事都直接动库文件/大批量数据，放在 store 包而不是 api 包，
// 与 CreateBackup 同样的理由：它们可能被调度器复用，且不应让 api 反向依赖自己。

// PurgeOldData 删除早于 beforeTs 的时序数据，避免库无限膨胀。
//
// 为什么只动这几张表：它们是无业务主键、只按 ts/day 追加的"遥测流水"，
// 涨得最快也最不值钱；节点/令牌/设置/告警这些"实体"不能按时间一刀切。
// 删除走唯一的写连接：writer 此刻新写入的行的 ts 都 >= now > beforeTs，
// 不会被误删，因此无需事务。
//
// traffic_daily 的分区键是节点本地日期（YYYY-MM-DD 文本），用 cutoff 当天
// 的 UTC 日期字符串做 "<" 比较即可（边界差几小时在无尽的历史数据面前无所谓）。
func (r *Repo) PurgeOldData(beforeTs int64) (map[string]int64, error) {
	tables := []string{"metrics_raw", "metrics_1m", "metrics_10m", "latency_raw", "latency_10m"}
	out := make(map[string]int64, len(tables)+1)
	for _, t := range tables {
		res, err := r.db.Write.Exec(`DELETE FROM "`+t+`" WHERE ts < ?`, beforeTs)
		if err != nil {
			return out, fmt.Errorf("清理 %s 失败：%w", t, err)
		}
		n, _ := res.RowsAffected()
		out[t] = n
	}
	day := time.Unix(beforeTs, 0).UTC().Format("2006-01-02")
	res, err := r.db.Write.Exec(`DELETE FROM traffic_daily WHERE day < ?`, day)
	if err != nil {
		return out, fmt.Errorf("清理 traffic_daily 失败：%w", err)
	}
	n, _ := res.RowsAffected()
	out["traffic_daily"] = n
	return out, nil
}

// PruneAuditLog 删除早于 beforeTs 的审计日志，返回删除行数。
// 由调度器每小时按 audit.retention_days 调用，也可由后台"立即清理"手动触发。
func (r *Repo) PruneAuditLog(beforeTs int64) (int64, error) {
	res, err := r.db.Write.Exec(`DELETE FROM audit_log WHERE ts < ?`, beforeTs)
	if err != nil {
		return 0, fmt.Errorf("清理审计日志失败：%w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RestoreBackup 用一份备份文件覆盖恢复当前库。
//
// 关键约束：modernc.org/sqlite 是纯 Go 驱动，没有 mattn 那种 Backup API，
// 也不能简单地"停服换文件"——pipeline.Writer 握着自己那份 *sql.DB 写连接，
// 热替换文件会让它写入一个已被覆盖/删掉的连接。所以这里用 SQLite 原生的
// ATTACH + 逐表覆盖式复制，在**当前唯一的写连接**上完成，不触碰任何已有连接。
//
// 安全性：
//   - 文件名与目录双重校验（复用 isBackupName + 父目录判定），防路径穿越；
//   - 只读打开备份先做 quick_check，并比对 user_version，版本不一致直接拒绝
//     （不同版本的 schema 复制会产出一份谁也读不懂的库）；
//   - 复制期间临时关闭 foreign_keys：恢复是整库覆盖，父子表的删除顺序无法保证，
//     关掉 FK 才能让 DELETE 任意顺序通过；复制的数据本身是一致的；
//   - 刻意保留 sessions 表：恢复后管理员不必重新登录。
func (r *Repo) RestoreBackup(ctx context.Context, name string) error {
	if !isBackupName(name) {
		return fmt.Errorf("非法的备份文件名：%q", name)
	}
	dir := filepath.Clean(r.BackupDir())
	src := filepath.Clean(filepath.Join(dir, name))
	if filepath.Dir(src) != dir {
		return fmt.Errorf("备份文件不在备份目录内：%q", name)
	}
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return ErrBackupNotFound
		}
		return fmt.Errorf("读取备份失败：%w", err)
	}

	// 只读打开备份：先校验完整性与版本，再决定要不要动生产库。
	bdb, err := sql.Open("sqlite", "file:"+src+"?_pragma=mode(ro)")
	if err != nil {
		return fmt.Errorf("打开备份失败：%w", err)
	}
	defer bdb.Close()
	if err := bdb.Ping(); err != nil {
		return fmt.Errorf("打开备份失败：%w", err)
	}
	var bchk string
	if err := bdb.QueryRow(`PRAGMA quick_check`).Scan(&bchk); err != nil || !strings.EqualFold(bchk, "ok") {
		return fmt.Errorf("备份完整性校验未通过（%s）", bchk)
	}
	var bver, lver int
	if err := bdb.QueryRow(`PRAGMA user_version`).Scan(&bver); err != nil {
		return fmt.Errorf("读取备份版本失败：%w", err)
	}
	if err := r.db.Read.QueryRow(`PRAGMA user_version`).Scan(&lver); err != nil {
		return fmt.Errorf("读取当前版本失败：%w", err)
	}
	if bver != lver {
		return fmt.Errorf("备份版本(%d)与当前库版本(%d)不一致，拒绝恢复以免数据损坏", bver, lver)
	}

	// 备份里有哪些用户表（不含 sqlite_* 内部表）。
	brows, err := bdb.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return fmt.Errorf("读取备份表清单失败：%w", err)
	}
	var tables []string
	for brows.Next() {
		var n string
		if err := brows.Scan(&n); err != nil {
			_ = brows.Close()
			return fmt.Errorf("读取备份表清单失败：%w", err)
		}
		tables = append(tables, n)
	}
	if err := brows.Err(); err != nil {
		_ = brows.Close()
		return fmt.Errorf("读取备份表清单失败：%w", err)
	}
	_ = brows.Close()
	// 提前关闭只读备份连接：它持着备份文件的读锁，会在后面 DETACH 时与 srcbk
	// 争用（"database srcbk is locked"）。这里表清单已取完，没必要再留着。
	_ = bdb.Close()

	w := r.db.Write
	// foreign_keys 必须在事务外切换（SQLite 不允许在事务内改它）。
	if _, err := w.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("准备恢复失败：%w", err)
	}
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		_, _ = w.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
		return fmt.Errorf("开始恢复事务失败：%w", err)
	}
	// 转义单引号后再作为 ATTACH 的文件名（路径来自受控的备份目录，仍做转义以防意外）。
	quoted := strings.ReplaceAll(src, "'", "''")
	if _, err := tx.Exec(`ATTACH DATABASE '` + quoted + `' AS srcbk`); err != nil {
		_ = tx.Rollback()
		_, _ = w.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
		return fmt.Errorf("挂载备份失败：%w", err)
	}
	for _, t := range tables {
		if t == "sessions" {
			continue // 保留当前在线会话
		}
		if _, err := tx.Exec(`DELETE FROM main."` + t + `"`); err != nil {
			_ = tx.Rollback()
			_, _ = w.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
			return fmt.Errorf("清空表 %s 失败：%w", t, err)
		}
		if _, err := tx.Exec(`INSERT INTO main."` + t + `" SELECT * FROM srcbk."` + t + `"`); err != nil {
			_ = tx.Rollback()
			_, _ = w.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
			return fmt.Errorf("恢复表 %s 失败：%w", t, err)
		}
	}
	// 注意：DETACH 放在 COMMIT **之后**执行——在事务内 DETACH 刚读过的事务
	// 会报 "database srcbk is locked"。ATTACH 是连接级的，COMMIT 后依然有效。
	if err := tx.Commit(); err != nil {
		_, _ = w.ExecContext(ctx, `DETACH DATABASE srcbk`)
		_, _ = w.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
		return fmt.Errorf("提交恢复失败：%w", err)
	}
	if _, err := w.ExecContext(ctx, `DETACH DATABASE srcbk`); err != nil {
		_, _ = w.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
		return fmt.Errorf("卸载备份失败：%w", err)
	}
	if _, err := w.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		return fmt.Errorf("恢复后重置外键约束失败：%w", err)
	}
	return nil
}
