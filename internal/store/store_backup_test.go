package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koyoca/xb-probe/internal/auth"
)

// 6.2：恢复码必须**原子消费**——同一个码并发使用只能成功一次。
//
// 这里与注册码（TestEnrollCodeAtomicConsumption）是同一类缺陷：
// 若实现写成"先 SELECT 判断未用，再 UPDATE 置已用"，两个并发请求会同时
// 看到 used_at IS NULL 从而都判定成功。于是"一次性"恢复码可以被无限复用，
// 而管理员以为自己手上那张纸早就作废了。
//
// 正确实现只能用 UPDATE ... WHERE used_at IS NULL 的受影响行数来判定。
func TestRecoveryCodeAtomicConsumption(t *testing.T) {
	r := newTestRepo(t)
	adminID := mustAdmin(t, r, "ops")

	hash := auth.HashRecoveryCode("ABCD-EFGH-JKMN")
	if _, err := r.ReplaceRecoveryCodes(adminID, [][]byte{hash}, 1_760_000_000); err != nil {
		t.Fatalf("写入恢复码失败：%v", err)
	}

	const concurrency = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := r.ConsumeRecoveryCode(hash, 1_760_000_001)
			if err != nil {
				return
			}
			if ok {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if success != 1 {
		t.Errorf("同一恢复码并发消费应恰好成功 1 次，实际 %d 次", success)
	}
	// 再消费一次也必须失败。
	if _, ok, _ := r.ConsumeRecoveryCode(hash, 1_760_000_002); ok {
		t.Error("已消费的恢复码不应再次通过")
	}
}

// 恢复码的规范化必须在**存储侧**也成立：
// 用带连字符的形式写入，用不带连字符的形式消费，必须命中同一条记录。
//
// 这条一旦不成立，症状是"管理员按纸上的码输进去登不上"——
// 而且因为哈希不同，连"码用过了"这种线索都没有。
func TestRecoveryCodeHashRoundTripFormats(t *testing.T) {
	r := newTestRepo(t)
	adminID := mustAdmin(t, r, "ops2")

	// 签发：带连字符（纸面上的形态）。
	if _, err := r.ReplaceRecoveryCodes(adminID, [][]byte{auth.HashRecoveryCode("WXYZ-2345-6789")}, 1); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	// 消费：不带连字符 + 小写（管理员手输的常见形态）。
	got, ok, err := r.ConsumeRecoveryCode(auth.HashRecoveryCode("wxyz 2345 6789"), 2)
	if err != nil {
		t.Fatalf("消费失败：%v", err)
	}
	if !ok {
		t.Fatal("不同书写形式的同一个码应当命中")
	}
	if got != adminID {
		t.Errorf("应返回 adminID=%d，得到 %d", adminID, got)
	}
}

// 重新生成必须**作废全部旧码**。
//
// 重新生成的典型动机就是"我怀疑恢复码泄露了"。若实现只做 INSERT 不 DELETE，
// 旧码依然可用，等于重置了个寂寞——而且后台显示的可用数量还是对的，
// 管理员完全看不出问题。
func TestReplaceRecoveryCodesInvalidatesOld(t *testing.T) {
	r := newTestRepo(t)
	adminID := mustAdmin(t, r, "ops3")

	oldHash := auth.HashRecoveryCode("AAAA-BBBB-CCCC")
	if _, err := r.ReplaceRecoveryCodes(adminID, [][]byte{oldHash}, 1); err != nil {
		t.Fatalf("首次写入失败：%v", err)
	}
	newHash := auth.HashRecoveryCode("DDDD-EEEE-FFFF")
	if _, err := r.ReplaceRecoveryCodes(adminID, [][]byte{newHash}, 2); err != nil {
		t.Fatalf("重新写入失败：%v", err)
	}

	if _, ok, _ := r.ConsumeRecoveryCode(oldHash, 3); ok {
		t.Error("旧恢复码应已作废")
	}
	// 先断言数量（此时新码尚未消费），再消费新码。
	total, unused, err := r.CountRecoveryCodes(adminID)
	if err != nil {
		t.Fatalf("统计失败：%v", err)
	}
	if total != 1 || unused != 1 {
		t.Errorf("替换后应只有 1 个未用码（旧码应被删除，而非 1+1），实际 total=%d unused=%d",
			total, unused)
	}
	if _, ok, _ := r.ConsumeRecoveryCode(newHash, 4); !ok {
		t.Error("新恢复码应可用")
	}
}

// 统计接口要能区分"总量"与"未用量"，后台据此提示"恢复码快用完了"。
func TestCountRecoveryCodesTracksUnused(t *testing.T) {
	r := newTestRepo(t)
	adminID := mustAdmin(t, r, "ops4")

	codes, err := auth.GenerateRecoveryCodes(5)
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	hashes := make([][]byte, 0, len(codes))
	for _, c := range codes {
		hashes = append(hashes, auth.HashRecoveryCode(c))
	}
	if _, err := r.ReplaceRecoveryCodes(adminID, hashes, 1); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	total, unused, _ := r.CountRecoveryCodes(adminID)
	if total != 5 || unused != 5 {
		t.Fatalf("初始应为 5/5，实际 %d/%d", total, unused)
	}
	for i := 0; i < 2; i++ {
		if _, ok, _ := r.ConsumeRecoveryCode(auth.HashRecoveryCode(codes[i]), int64(10+i)); !ok {
			t.Fatalf("消费第 %d 个失败", i)
		}
	}
	total, unused, _ = r.CountRecoveryCodes(adminID)
	if total != 5 || unused != 3 {
		t.Errorf("消费 2 个后应为 total=5 unused=3，实际 %d/%d", total, unused)
	}
}

// HasTOTP / SetTOTPSecret：启用与关闭的往返。
func TestSetTOTPSecretTogglesAndResetsLock(t *testing.T) {
	r := newTestRepo(t)
	adminID := mustAdmin(t, r, "ops5")

	if on, _ := r.HasTOTP(adminID); on {
		t.Fatal("初始不应启用 TOTP")
	}
	// 先制造一次锁定与失败计数，验证启用时会清零。
	if err := r.RecordLoginFailure(adminID, 1, 900, 100); err != nil {
		t.Fatalf("记录失败：%v", err)
	}
	a, _ := r.GetAdminByID(adminID)
	if a.LockedUntil == 0 {
		t.Fatal("应已被锁定（前置条件不成立）")
	}

	secret, _ := auth.GenerateTOTPSecret()
	if err := r.SetTOTPSecret(adminID, []byte(secret)); err != nil {
		t.Fatalf("启用失败：%v", err)
	}
	if on, _ := r.HasTOTP(adminID); !on {
		t.Error("应已启用 TOTP")
	}
	a, _ = r.GetAdminByID(adminID)
	if a.LockedUntil != 0 || a.FailedCount != 0 {
		t.Errorf("启用 TOTP 应清零锁定与失败计数（刚绑完码的人往往已被锁），"+
			"实际 locked=%d failed=%d", a.LockedUntil, a.FailedCount)
	}

	// 关闭。
	if err := r.SetTOTPSecret(adminID, nil); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	if on, _ := r.HasTOTP(adminID); on {
		t.Error("应已关闭 TOTP")
	}
}

// ---------------------------------------------------------------------------
// 6.6 备份与轮转
// ---------------------------------------------------------------------------

// 备份必须产出可读的独立数据库文件，且权限收紧到 0600。
func TestCreateBackupProducesReadableFile(t *testing.T) {
	r := newTestRepo(t)
	mustNode(t, r, "backup-node")

	info, err := r.CreateBackup(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("备份失败：%v", err)
	}
	if info.SizeBytes <= 0 {
		t.Error("备份文件不应为空")
	}
	if !strings.HasPrefix(info.Name, "probe-") || !strings.HasSuffix(info.Name, ".db") {
		t.Errorf("备份文件名不符合约定：%s", info.Name)
	}
	st, err := os.Stat(info.Path)
	if err != nil {
		t.Fatalf("备份文件不存在：%v", err)
	}
	if st.Size() != info.SizeBytes {
		t.Errorf("报告大小 %d 与实际 %d 不一致", info.SizeBytes, st.Size())
	}
	// 备份里含令牌哈希与会话，必须是 0600（Windows 上 chmod 语义有限，跳过断言）。
	if !isWindows() {
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("备份文件权限应为 0600，实际 %o", perm)
		}
	}
}

// 轮转必须只保留最新的 keep 份，且删除的是**最旧**的。
func TestPruneBackupsKeepsNewest(t *testing.T) {
	r := newTestRepo(t)
	dir := r.BackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	// 手工造 5 个文件名带递增时间戳的"备份"。
	var names []string
	for i := 1; i <= 5; i++ {
		n := "probe-2026010" + string(rune('0'+i)) + "-040000.db"
		names = append(names, n)
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatalf("造文件失败：%v", err)
		}
	}

	removed, err := r.PruneBackups(2)
	if err != nil {
		t.Fatalf("轮转失败：%v", err)
	}
	if len(removed) != 3 {
		t.Fatalf("应删除 3 份，实际 %d 份：%v", len(removed), removed)
	}
	// 被删的必须是最早的三份。
	for _, want := range names[:3] {
		found := false
		for _, got := range removed {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("最早的文件 %s 应被删除，实际删除：%v", want, removed)
		}
	}
	list, _ := r.ListBackups()
	if len(list) != 2 {
		t.Fatalf("应剩 2 份，实际 %d", len(list))
	}
	// 列表必须按时间**倒序**（最新在前），后台直接照序渲染。
	if list[0].Name != names[4] {
		t.Errorf("列表首个应为最新的 %s，实际 %s", names[4], list[0].Name)
	}
}

// keep <= 0 时必须**什么都不删**。
//
// "保留 0 份"几乎总是配置失误（设置被清空等），按字面执行会把全部备份删光，
// 是不可逆的数据损失。宁可什么都不做。
func TestPruneBackupsRefusesNonPositiveKeep(t *testing.T) {
	r := newTestRepo(t)
	dir := r.BackupDir()
	_ = os.MkdirAll(dir, 0o700)
	for i := 1; i <= 3; i++ {
		n := "probe-2026020" + string(rune('0'+i)) + "-040000.db"
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, keep := range []int{0, -1} {
		removed, err := r.PruneBackups(keep)
		if err != nil {
			t.Fatalf("keep=%d 不应报错：%v", keep, err)
		}
		if len(removed) != 0 {
			t.Errorf("keep=%d 不应删除任何文件，实际删了 %v", keep, removed)
		}
	}
	if list, _ := r.ListBackups(); len(list) != 3 {
		t.Errorf("文件数应保持 3，实际 %d", len(list))
	}
}

// ListBackups 在目录不存在时返回空列表而不是错误。
//
// 全新安装从未备份过是正常状态，报错会让后台"备份"卡片显示成故障。
func TestListBackupsMissingDirIsEmpty(t *testing.T) {
	r := newTestRepo(t)
	list, err := r.ListBackups()
	if err != nil {
		t.Fatalf("目录不存在不应报错：%v", err)
	}
	if len(list) != 0 {
		t.Errorf("应为空列表，实际 %d 项", len(list))
	}
}

// ListBackups 必须忽略目录里的无关文件。
//
// 备份目录是运维会用 scp/rsync 进出的地方，常混进 .tmp、.sha256、
// 甚至别人手工放的其它 db。无差别列出会让轮转误删这些文件。
func TestListBackupsIgnoresForeignFiles(t *testing.T) {
	r := newTestRepo(t)
	dir := r.BackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(n string) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("probe-20260301-040000.db") // 应被识别
	write("probe-20260301-040000.db.tmp")
	write("other.db")
	write("README.txt")
	write("probe-notadb.txt")

	list, err := r.ListBackups()
	if err != nil {
		t.Fatalf("列出失败：%v", err)
	}
	if len(list) != 1 || list[0].Name != "probe-20260301-040000.db" {
		t.Errorf("应只识别 1 个合法备份，实际 %+v", list)
	}
}

// 同秒内重复备份必须失败（VACUUM INTO 要求目标不存在），
// 而不是静默覆盖上一份——覆盖会让"刚备完就发现库坏了"无路可退。
func TestCreateBackupSameSecondFails(t *testing.T) {
	r := newTestRepo(t)
	ts := time.Now()
	if _, err := r.CreateBackup(context.Background(), ts); err != nil {
		t.Fatalf("首次备份应成功：%v", err)
	}
	if _, err := r.CreateBackup(context.Background(), ts); err == nil {
		t.Error("同一秒第二次备份应失败（同名文件已存在）")
	}
}

// 手动删除单份备份：删除之后列表里不再有它，其余不受影响。
//
// 为什么需要这个能力：轮转（backup.keep）只按数量裁最旧的，处理不了
// "这份备份是坏的 / 特别大 / 含误操作前的数据，想立刻清掉"这类诉求，
// 而备份盘满往往是主库写不进去的前奏。
func TestDeleteBackupRemovesOnlyThatFile(t *testing.T) {
	r := newTestRepo(t)
	dir := r.BackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	names := []string{"probe-20260401-040000.db", "probe-20260402-040000.db", "probe-20260403-040000.db"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.DeleteBackup(names[1]); err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, names[1])); !os.IsNotExist(err) {
		t.Error("目标文件应已被删除")
	}
	list, _ := r.ListBackups()
	if len(list) != 2 {
		t.Fatalf("应剩 2 份，实际 %d：%+v", len(list), list)
	}
	for _, b := range list {
		if b.Name == names[1] {
			t.Errorf("被删的文件不应再出现在列表里：%+v", b)
		}
	}

	// 重复删除同一份 → 明确的"不存在"错误（用于映到 404），而不是静默成功。
	if err := r.DeleteBackup(names[1]); !errors.Is(err, ErrBackupNotFound) {
		t.Errorf("重复删除应返回 ErrBackupNotFound，实际 %v", err)
	}
}

// 删除接口只接受**文件名**，且必须符合 CreateBackup 的形状。
//
// 这是本功能最重要的一条护栏：管理端传来的值最终会变成 os.Remove 的路径。
// 只做"拼接目录 + 删除"的话，一个 ../ 就能从"删除备份"变成"删除主库"，
// 而触发它的只是一个带 CSRF 令牌的管理接口，没有任何二次确认。
func TestDeleteBackupRejectsPathsAndForeignNames(t *testing.T) {
	r := newTestRepo(t)
	dir := r.BackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 在备份目录的上一级（= 数据库所在目录）放一个"主库"文件作为靶子。
	mainDB := filepath.Join(filepath.Dir(dir), "probe.db")
	if err := os.WriteFile(mainDB, []byte("IMPORTANT"), 0o600); err != nil {
		t.Fatal(err)
	}

	bad := []string{
		"../probe.db",                   // 经典上跳：绝不允许
		"..\\probe.db",                  // Windows 分隔符
		"/etc/passwd",                   // 绝对路径
		"probe-20260101-000000.db/../x", // 混在中间
		"",                              // 空
		"README.txt",                    // 目录里的无关文件，不属于本系统
		"other.db",
		"probe-20260101-000000.db.tmp", // 后缀不符
		"probe-notatimestamp.db",       // 时间戳段不合法
		"probe-2026010-000000.db",      // 日期位数不足
		"probe-20260101_000000.db",     // 分隔符不对
		"probe-20260101-000000.sql",    // 后缀不对
		"Probe-20260101-000000.db",     // 大小写不符（生成侧永远是小写 probe-）
	}
	for _, n := range bad {
		if err := r.DeleteBackup(n); err == nil {
			t.Errorf("DeleteBackup(%q) 应被拒绝，实际成功了", n)
		}
	}
	// 靶子必须还在——这是"没被趁机删掉"的实证。
	if b, err := os.ReadFile(mainDB); err != nil || string(b) != "IMPORTANT" {
		t.Fatalf("主库文件被误删或改动：err=%v content=%q", err, b)
	}
	// 目录里的无关文件也不能被删。
	write := func(n string) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("README.txt")
	if err := r.DeleteBackup("README.txt"); err == nil {
		t.Error("无关文件不应被删除")
	}
	if _, err := os.Stat(filepath.Join(dir, "README.txt")); err != nil {
		t.Error("无关文件应仍然存在")
	}
}

// 合法文件名必须被接受（否则"太严"会让功能彻底不能用）。
func TestDeleteBackupAcceptsGeneratedName(t *testing.T) {
	r := newTestRepo(t)
	info, err := r.CreateBackup(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("备份失败：%v", err)
	}
	if !isBackupName(info.Name) {
		t.Fatalf("CreateBackup 生成的名字没通过 isBackupName 校验：%s", info.Name)
	}
	if err := r.DeleteBackup(info.Name); err != nil {
		t.Fatalf("删除自己生成的备份应成功：%v", err)
	}
}

func mustAdmin(t *testing.T, r *Repo, name string) int64 {
	t.Helper()
	id, err := r.CreateAdmin(name, "$argon2id$v=19$m=1,t=1,p=1$aa$bb", nil, 1_760_000_000)
	if err != nil {
		t.Fatalf("创建管理员失败：%v", err)
	}
	return id
}

// isWindows 用于跳过 Unix 特有的权限断言。
//
// Windows 上 FileMode 的权限位是合成的（总是 0666/0444 之类），
// chmod 0600 不会反映到 Mode().Perm() 上，断言必然失败——
// 但这不是缺陷，是平台语义差异，直接跳过比放宽断言更诚实。
func isWindows() bool { return runtime.GOOS == "windows" }
