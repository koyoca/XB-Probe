package db

import (
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// openTestDB 在临时目录里开一个全新的库。
func openTestDB(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func pragmaText(t *testing.T, d *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := d.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatalf("读取 PRAGMA %s 失败: %v", name, err)
	}
	return v
}

func pragmaInt(t *testing.T, d *sql.DB, name string) int {
	t.Helper()
	var v int
	if err := d.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatalf("读取 PRAGMA %s 失败: %v", name, err)
	}
	return v
}

// existingTables 返回库里所有非内部表名（升序）。
func existingTables(t *testing.T, d *sql.DB) []string {
	t.Helper()
	rows, err := d.Query(
		`SELECT name FROM sqlite_master
		 WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("读取 sqlite_master 失败: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// expectedTables 是 0001~0006 六份迁移建出来的全部表。
//
// 这里刻意把表名逐个钉死：迁移脚本被改错、改名、或被"整理"掉一张表，
// 只有真的启动一次服务才会发现——而那张表可能是 latency_10m 这种
// "7d/30d 曲线全靠它"的表（空表不会报错，只是页面永远没有数据）。
// 增删表必须同时改这里，属于有意为之的摩擦。
var expectedTables = []string{
	"admin_recovery_codes",
	"admins",
	"alert_channels",
	"alert_events",
	"alert_rules",
	"alert_state",
	"audit_log",
	"enroll_codes",
	"latency_10m",
	"latency_raw",
	"metrics_10m",
	"metrics_1m",
	"metrics_raw",
	"node_billing",
	"node_tokens",
	"nodes",
	"probe_targets",
	"schema_migrations",
	"sessions",
	"settings",
	"traffic_daily",
	"traffic_nic_state",
	"traffic_state",
}

// ---------------------------------------------------------------------------
// DSN
// ---------------------------------------------------------------------------

// TestBuildDSNPinsEveryPragma 4.2 的第 4 条要求六个 PRAGMA 一个不少。
//
// 少一个的后果都不轻：没有 WAL 就退化成读写互斥；没有 busy_timeout
// 会在并发写时直接抛 SQLITE_BUSY；没有 foreign_keys 则级联删除静默失效
// （删节点时它的时序数据会留下来，越积越多）。
func TestBuildDSNPinsEveryPragma(t *testing.T) {
	const path = "/srv/probe/probe.db"
	dsn := buildDSN(path)

	if !strings.HasPrefix(dsn, "file:") {
		t.Fatalf("modernc 驱动要求 file: URI 形式，得到 %q", dsn)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DSN 不是合法 URL: %v", err)
	}
	if u.Path != path {
		t.Errorf("DSN 路径 = %q，期望 %q", u.Path, path)
	}
	if u.Scheme != "file" {
		t.Errorf("DSN scheme = %q，期望 file", u.Scheme)
	}

	want := []string{
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"busy_timeout(5000)",
		"foreign_keys(ON)",
		"temp_store(MEMORY)",
		"auto_vacuum(INCREMENTAL)",
	}
	got := u.Query()["_pragma"]
	if len(got) != len(want) {
		t.Fatalf("PRAGMA 条数 = %d，期望 %d：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个 PRAGMA = %q，期望 %q", i+1, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Open
// ---------------------------------------------------------------------------

func TestOpenCreatesDataDirAndDBFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("应自动创建数据目录: %v", err)
	}
	want := filepath.Join(dir, "probe.db")
	if st.Path() != want {
		t.Errorf("Path() = %q，期望 %q", st.Path(), want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("库文件应已创建: %v", err)
	}
}

// TestOpenTightensPermissions 数据目录 0700、库文件 0600（S13）。
//
// Windows 上 os.Chmod 只映射到"只读"属性，POSIX 权限位读回来不是 0700/0600，
// 因此这里按平台跳过；真机（FreeBSD/LXC）的验证见第 98 项。
func TestOpenTightensPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的 os.Chmod 不维护 POSIX 权限位，改由真机验证")
	}

	base := t.TempDir()
	dir := filepath.Join(base, "data")
	// 先把目录建成宽松权限，模拟"用 umask 022 手工建过 data 目录"的常见情形。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dir, 0o755)

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("数据目录权限 = %#o，期望 0700（已有目录也要收紧）", got)
	}
	fi, err := os.Stat(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("库文件权限 = %#o，期望 0600", got)
	}
}

// TestOpenAppliesPragmasOnBothConnections 读写两条连接都必须带上 PRAGMA。
//
// PRAGMA 是**按连接**生效的：只在写连接上设 foreign_keys，读连接上的
// 外键约束依然是关的。两条连接共用同一个 DSN，这里两边都查一遍。
func TestOpenAppliesPragmasOnBothConnections(t *testing.T) {
	st := openTestDB(t)

	type probe struct {
		name    string
		intVal  bool
		wantInt int
		wantStr string
	}
	probes := []probe{
		{name: "journal_mode", wantStr: "wal"}, // 持久化在文件头
		{name: "synchronous", intVal: true, wantInt: 1},
		{name: "foreign_keys", intVal: true, wantInt: 1},
		{name: "busy_timeout", intVal: true, wantInt: 5000},
		{name: "temp_store", intVal: true, wantInt: 2},  // MEMORY
		{name: "auto_vacuum", intVal: true, wantInt: 2}, // INCREMENTAL
	}

	for _, side := range []struct {
		name string
		db   *sql.DB
	}{{"写连接", st.Write}, {"读连接", st.Read}} {
		for _, p := range probes {
			if p.intVal {
				if got := pragmaInt(t, side.db, p.name); got != p.wantInt {
					t.Errorf("%s 的 %s = %d，期望 %d", side.name, p.name, got, p.wantInt)
				}
			} else {
				if got := pragmaText(t, side.db, p.name); got != p.wantStr {
					t.Errorf("%s 的 %s = %q，期望 %q", side.name, p.name, got, p.wantStr)
				}
			}
		}
	}
}

func TestOpenUsesSingleWriterAndPooledReaders(t *testing.T) {
	st := openTestDB(t)

	if got := st.Write.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("写连接 MaxOpenConns = %d，期望 1（全库唯一写者）", got)
	}
	if got := st.Read.Stats().MaxOpenConnections; got != 8 {
		t.Errorf("读连接 MaxOpenConns = %d，期望 8", got)
	}
}

func TestOpenFailsWhenDataDirIsAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(f)
	if err == nil {
		_ = st.Close()
		t.Fatal("数据目录位置是一个普通文件时应报错")
	}
	if st != nil {
		t.Error("失败时不应返回 Store")
	}
}

// TestOpenRejectsNonSQLiteFile 根本不是 SQLite 的文件必须被拒绝。
func TestOpenRejectsNonSQLiteFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	garbage := strings.Repeat("this is definitely not a sqlite database.", 40)
	if err := os.WriteFile(filepath.Join(dir, "probe.db"), []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Open(dir)
	if err == nil {
		_ = st.Close()
		t.Fatal("非 SQLite 文件必须被拒绝，不能返回可用的 Store")
	}
	if st != nil {
		t.Error("失败时不应返回 Store")
	}
}

// TestOpenRefusesPageCorruptedDatabase 13.4.8 的**真实场景**：文件头合法、
// 页内字节被破坏（比特腐烂 / 从坏盘恢复出来的库）。
//
// 断言的是"契约"：Open 必须拒绝、不返回可用 Store，且错误信息能让运维
// 分辨这是数据损坏而不是配置问题。具体由 Ping 还是 quick_check 抓到不在这里断言
// （这条链路上 quick_check 其实没机会执行，见 TestPingAndCheckDetectsPageCorruption）。
func TestOpenRefusesPageCorruptedDatabase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}
	// 多写点数据，保证库不止一页。
	for i := 0; i < 200; i++ {
		if _, err := st.Write.Exec(
			`INSERT INTO settings(key, value, updated_at) VALUES(?, 'v', ?)`,
			"k"+strconv.Itoa(i), i); err != nil {
			t.Fatalf("准备数据失败: %v", err)
		}
	}
	path := st.Path()
	if err := st.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	// 干净关闭后 WAL 已合并进主库；残留的 -wal/-shm 会干扰下面的破坏，
	// 一并清掉，确保结果只取决于主库文件本身。
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const pageSize = 4096
	const tail = 512
	if len(raw) < 3*pageSize {
		t.Fatalf("库文件只有 %d 字节，不足以做页破坏测试", len(raw))
	}
	// 只破坏**最后一个页内部的字节**，并且刻意保留该页的页头（含 cell 指针数组之前的部分）。
	//
	// 破坏范围必须精确控制：把第 1 页之后的整片清掉是不行的——连接建立时
	// `auto_vacuum(INCREMENTAL)` 会先读指针映射页，于是 Ping 阶段就会报
	// "database disk image is malformed"，quick_check 那段代码根本没被执行到
	// （这个测试的第一版就是这样，一次变异验证把它照出来了）。
	// 只动末页内部的字节，打开与 Ping 都不会碰到它，唯有全库遍历能发现。
	for i := len(raw) - tail; i < len(raw); i++ {
		raw[i] = 0
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(dir)
	if err == nil {
		_ = st2.Close()
		t.Fatal("页被破坏的库必须被拒绝（否则服务会在运行中零星报错）")
	}
	// 错误信息要让运维一眼看出是"数据坏了"而不是"配置写错了"。
	msg := err.Error()
	if !strings.Contains(msg, "quick_check") &&
		!strings.Contains(msg, "integrity check failed") &&
		!strings.Contains(msg, "malformed") {
		t.Fatalf("错误信息应表明是数据损坏，得到: %v", err)
	}
}

// TestPingAndCheckDetectsPageCorruption 单独验证启动完整性检查**本身**（13.4.8）。
//
// 上面那个测试走的是 Open 的完整链路，而链路里有一层遮蔽：Open 的 DSN 带
// `auto_vacuum(INCREMENTAL)`，连接建立时就会读指针映射页，于是页损坏总在
// Ping 阶段先被报出来，pingAndCheck 里 quick_check 那段代码根本没被执行到。
// 用它当靶子等于没测——一次变异验证（把 quick_check 整段删掉，测试照样绿）
// 正是这么发现的。
//
// 所以这里手工拼一个**不带任何 PRAGMA** 的 Store：库能正常打开、Ping 也通过，
// 只剩"遍历全库"这一种方式能发现问题，quick_check 的增量价值才被真正覆盖。
func TestPingAndCheckDetectsPageCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.db")

	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	if _, err := d.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	// 写入足够多的行，让表数据占据多个页，且最后一个页是"表叶子页"。
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO t(id, v) VALUES(?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 120)
	for i := 0; i < 3000; i++ {
		if _, err := stmt.Exec(i, fmt.Sprintf("row-%06d-%s", i, payload)); err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const pageSize = 4096
	if len(raw) < 4*pageSize {
		t.Fatalf("库只有 %d 字节，不足以做页破坏测试", len(raw))
	}
	// 抹掉最后一个页的页头（每页前 64 字节是页头与 cell 指针区）。
	// 打开时不会读它，Ping 也不会，只有全库遍历会踩到。
	lastPage := (len(raw) - 1) / pageSize * pageSize
	for i := lastPage; i < lastPage+64 && i < len(raw); i++ {
		raw[i] = 0
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	d2, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	defer func() { _ = d2.Close() }()
	if err := d2.Ping(); err != nil {
		// 这个驱动/平台在打开阶段就读到了坏页——那说明"打开即校验"，
		// 本用例想测的增量部分无从构造，如实跳过而不是假装通过。
		t.Skipf("打开阶段已发现损坏，无法单独覆盖 quick_check: %v", err)
	}

	st := &Store{Write: d2, Read: d2, path: path}
	err = st.pingAndCheck()
	if err == nil {
		t.Fatal("页头被破坏的库必须被启动完整性检查判为损坏")
	}
	if !strings.Contains(err.Error(), "integrity check failed") &&
		!strings.Contains(err.Error(), "quick_check") {
		t.Fatalf("错误信息应来自完整性检查，得到: %v", err)
	}
}

// TestPingAndCheckPassesOnHealthyDatabase 健康库不得被误判（否则服务永远起不来）。
func TestPingAndCheckPassesOnHealthyDatabase(t *testing.T) {
	st := openTestDB(t)
	if err := st.Migrate(); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}
	if err := st.pingAndCheck(); err != nil {
		t.Fatalf("健康库不应被完整性检查拒绝: %v", err)
	}
}

func TestCloseIsSafe(t *testing.T) {
	st := openTestDB(t)
	if err := st.Close(); err != nil {
		t.Fatalf("Close 不应报错: %v", err)
	}
	// 二次关闭不应 panic（sql.DB.Close 是幂等的）。
	if err := st.Close(); err != nil {
		t.Errorf("重复 Close 不应报错: %v", err)
	}
	// 连接为 nil 时也要能走通，否则 defer Close 会在部分初始化失败时 panic。
	if err := (&Store{}).Close(); err != nil {
		t.Errorf("空 Store 的 Close 不应报错: %v", err)
	}
}

// TestForeignKeysAreEnforcedOnWriteConnection 外键必须真的生效，而不只是"PRAGMA 读回来是 1"。
//
// admin_recovery_codes.admin_id 带 ON DELETE CASCADE：删掉管理员必须连带删掉他的恢复码。
// 外键关着时这条约束完全静默失效——恢复码会一直留在库里，
// 而"重置管理员""删管理员"这类操作看起来都成功了。
// 这里用真实数据验证约束本身，比读 PRAGMA 的值更能说明问题。
func TestForeignKeysAreEnforcedOnWriteConnection(t *testing.T) {
	st := openTestDB(t)
	if err := st.Migrate(); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}

	res, err := st.Write.Exec(
		`INSERT INTO admins(username, pw_hash, created_at) VALUES('admin', 'hash', 1)`)
	if err != nil {
		t.Fatalf("插入管理员失败: %v", err)
	}
	adminID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write.Exec(
		`INSERT INTO admin_recovery_codes(admin_id, code_hash, created_at) VALUES(?, ?, 1)`,
		adminID, []byte{0x01, 0x02}); err != nil {
		t.Fatalf("插入恢复码失败: %v", err)
	}

	// 外键打开时，指向不存在管理员的恢复码应被拒绝。
	if _, err := st.Write.Exec(
		`INSERT INTO admin_recovery_codes(admin_id, code_hash, created_at) VALUES(99999, ?, 1)`,
		[]byte{0x03}); err == nil {
		t.Error("外键约束未生效：允许了指向不存在管理员的恢复码")
	}

	if _, err := st.Write.Exec(`DELETE FROM admins WHERE id=?`, adminID); err != nil {
		t.Fatalf("删除管理员失败: %v", err)
	}
	var left int
	if err := st.Read.QueryRow(
		`SELECT COUNT(*) FROM admin_recovery_codes WHERE admin_id=?`, adminID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("级联删除未生效（foreign_keys 没打开？），残留 %d 行恢复码", left)
	}
}

func TestReadConnectionSeesCommittedWrites(t *testing.T) {
	st := openTestDB(t)
	if err := st.Migrate(); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}
	if _, err := st.Write.Exec(
		`INSERT INTO settings(key, value, updated_at) VALUES('probe.test.key', 'v1', 1)`); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	var v string
	if err := st.Read.QueryRow(
		`SELECT value FROM settings WHERE key='probe.test.key'`).Scan(&v); err != nil {
		t.Fatalf("读连接应能看到已提交的写入（WAL 跨连接可见性）: %v", err)
	}
	if v != "v1" {
		t.Errorf("读到 %q，期望 v1", v)
	}
}

// ---------------------------------------------------------------------------
// Migrate
// ---------------------------------------------------------------------------

func TestMigrateCreatesSchemaAndRecordsVersions(t *testing.T) {
	st := openTestDB(t)
	if err := st.Migrate(); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}

	rows, err := st.Read.Query(`SELECT version, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var versions []int
	for rows.Next() {
		var v, at int64
		if err := rows.Scan(&v, &at); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, int(v))
		if at <= 0 {
			t.Errorf("迁移 %d 的 applied_at 应为正数时间戳，得到 %d", v, at)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	want := embeddedVersions(t)
	if len(versions) != len(want) {
		t.Fatalf("已应用 %d 个迁移，内嵌脚本有 %d 个：%v vs %v", len(versions), len(want), versions, want)
	}
	for i := range want {
		if versions[i] != want[i] {
			t.Errorf("第 %d 个版本 = %d，期望 %d（必须按版本号升序执行）", i+1, versions[i], want[i])
		}
	}
}

func TestMigrateCreatesExpectedTables(t *testing.T) {
	st := openTestDB(t)
	if err := st.Migrate(); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}

	got := existingTables(t, st.Write)
	want := append([]string(nil), expectedTables...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("表清单不符。\n实际: %v\n期望: %v", got, want)
	}
}

// TestMigrateIsIdempotent 重复 Migrate 不得重跑已应用的迁移。
//
// 重跑的后果是真实的：迁移里有 ALTER TABLE，重复执行会直接报错让服务起不来；
// 更糟的是"加了数据的一次性迁移"会被执行第二遍。
func TestMigrateIsIdempotent(t *testing.T) {
	st := openTestDB(t)
	if err := st.Migrate(); err != nil {
		t.Fatalf("首次 Migrate 失败: %v", err)
	}
	before := appliedAtMap(t, st)

	if err := st.Migrate(); err != nil {
		t.Fatalf("二次 Migrate 失败: %v", err)
	}
	after := appliedAtMap(t, st)

	if len(before) != len(after) {
		t.Fatalf("迁移条数变了：%d → %d", len(before), len(after))
	}
	for v, at := range before {
		if after[v] != at {
			t.Errorf("迁移 %d 被重新执行了（applied_at %d → %d）", v, at, after[v])
		}
	}
}

// TestMigrateOnReopenedDatabaseAppliesNothing 模拟"重启一次"。
//
// 这是最常见的真实路径：进程重启后重新 Open + Migrate，必须一个都不重跑。
func TestMigrateOnReopenedDatabaseAppliesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Migrate(); err != nil {
		t.Fatal(err)
	}
	before := appliedAtMap(t, first)
	_ = first.Close()

	second, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	defer func() { _ = second.Close() }()
	if err := second.Migrate(); err != nil {
		t.Fatalf("重启后 Migrate 失败: %v", err)
	}
	after := appliedAtMap(t, second)

	if len(before) != len(after) {
		t.Fatalf("重启后应无新迁移：%d → %d", len(before), len(after))
	}
	for v, at := range before {
		if after[v] != at {
			t.Errorf("迁移 %d 在重启后被重跑（applied_at %d → %d）", v, at, after[v])
		}
	}
}

// TestMigrationFilesFollowNamingConvention 文件名必须严格是 NNNN_name.sql。
//
// migrate.go 靠第一个下划线切出版本号：多一个下划线没问题，少一个就直接报错、
// 服务起不来。这个测试让"文件名写错"在 go test 阶段就暴露，而不是等到部署。
func TestMigrationFilesFollowNamingConvention(t *testing.T) {
	patterns := regexp.MustCompile(`^\d{4}_[a-z0-9_]+\.sql$`)
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatalf("读取内嵌迁移目录失败: %v", err)
	}

	seen := map[int]string{}
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			t.Errorf("迁移目录里出现非 .sql 文件: %s", name)
			continue
		}
		count++
		if !patterns.MatchString(name) {
			t.Errorf("迁移文件名 %q 不符合 NNNN_name.sql 约定", name)
			continue
		}
		idx := strings.IndexByte(name, '_')
		v, err := strconv.Atoi(name[:idx])
		if err != nil {
			t.Errorf("解析 %q 的版本号失败: %v", name, err)
			continue
		}
		if prev, dup := seen[v]; dup {
			t.Errorf("版本号 %d 重复：%s 与 %s", v, prev, name)
		}
		seen[v] = name
	}
	if count == 0 {
		t.Fatal("内嵌迁移目录是空的，Master 会以空库启动")
	}
}

func TestEmbeddedMigrationsAreNonEmpty(t *testing.T) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", e.Name(), err)
		}
		if strings.TrimSpace(string(body)) == "" {
			t.Errorf("迁移 %s 是空文件（go:embed 只嵌入存在的文件，空文件通常是误写）", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// 辅助：从内嵌 FS 直接算出期望
// ---------------------------------------------------------------------------

// embeddedVersions 直接读内嵌目录算出版本号升序列表。
//
// 刻意不复用 migrationFiles()：那样就成了"拿被测实现当期望值"，
// 解析逻辑写错时测试会一起错。
func embeddedVersions(t *testing.T) []int {
	t.Helper()
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		idx := strings.IndexByte(e.Name(), '_')
		if idx <= 0 {
			t.Fatalf("迁移文件名缺少下划线: %s", e.Name())
		}
		v, err := strconv.Atoi(e.Name()[:idx])
		if err != nil {
			t.Fatalf("解析 %s 的版本号失败: %v", e.Name(), err)
		}
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func appliedAtMap(t *testing.T, st *Store) map[int]int64 {
	t.Helper()
	rows, err := st.Read.Query(`SELECT version, applied_at FROM schema_migrations`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int]int64{}
	for rows.Next() {
		var v, at int64
		if err := rows.Scan(&v, &at); err != nil {
			t.Fatal(err)
		}
		out[int(v)] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
