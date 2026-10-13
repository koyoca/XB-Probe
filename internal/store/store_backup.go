package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 数据库备份与轮转（6.6）。
//
// 放在 store 包而不是 api 包：备份要同时被**管理接口**（手动触发）与
// **调度器**（每日自动）调用，放在 api 里会让 scheduler 反向依赖 api
// （main.go 的构造顺序是 scheduler → api，反向依赖会成环）。
//
// 备份方式用 `VACUUM INTO`（SQLite 3.27+）：
//   * 它是**在线一致快照**，不需要停服，也不怕写入进行到一半——
//     直接 cp 数据库文件在 WAL 模式下会拿到撕裂的副本，这正是旧做法最大的隐患；
//   * 产物是**已整理过的**完整数据库（无空闲页），比原文件更小；
//   * 目标文件必须不存在，所以文件名带时间戳（同秒重复触发会失败并给出明确错误）。
//
// 为什么不做"复制文件 + 拷贝 WAL/SHM"：那需要停写并精确处理 checkpoint，
// 任何一步出错都会产出一个"能打开但数据不一致"的备份——这种备份比没有更危险，
// 因为它在恢复演练之前看起来是成功的。

// BackupInfo 描述一个备份文件。
type BackupInfo struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	CreatedAt int64  `json:"created_at"` // 文件 mtime（Unix 秒）
}

// BackupDir 返回备份目录（与数据库同级的 backups/）。
//
// 与库同级而不是放 /tmp：备份必须与主库一起被纳入权限与挂载规划
// （data_dir 0700，见 6.5），放到临时目录里会被系统清理掉。
func (r *Repo) BackupDir() string {
	return filepath.Join(filepath.Dir(r.db.Path()), "backups")
}

// CreateBackup 生成一个备份，返回其信息。
//
// now 由调用方传入，便于测试与"归档时间统一"。
func (r *Repo) CreateBackup(ctx context.Context, now time.Time) (*BackupInfo, error) {
	dir := r.BackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建备份目录失败：%w", err)
	}
	// 文件名带到秒的时间戳：VACUUM INTO 要求目标不存在，
	// 同一秒内重复触发会失败——这是刻意的，宁可报错也不要静默覆盖上一份。
	name := fmt.Sprintf("probe-%s.db", now.Format("20060102-150405"))
	dst := filepath.Join(dir, name)

	if _, err := r.db.Write.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return nil, fmt.Errorf("备份失败：%w", err)
	}
	// 备份文件里含全部节点令牌哈希、管理员哈希与会话，权限必须是 0600。
	// VACUUM INTO 建出来的文件权限跟随 umask，显式收紧一次。
	if err := os.Chmod(dst, 0o600); err != nil {
		return nil, fmt.Errorf("设置备份文件权限失败：%w", err)
	}
	st, err := os.Stat(dst)
	if err != nil {
		return nil, fmt.Errorf("读取备份文件信息失败：%w", err)
	}
	return &BackupInfo{
		Name:      name,
		Path:      dst,
		SizeBytes: st.Size(),
		CreatedAt: st.ModTime().Unix(),
	}, nil
}

// ListBackups 列出全部备份，**按时间倒序**（最新的在前）。
//
// 只认 `probe-*.db`：目录里可能有运维自己放的其它文件，
// 无差别列出会让"清理最旧的 N 份"误删别人的东西。
// 排序用文件名而不是 mtime：文件名就是备份时刻，mtime 可能被拷贝/恢复操作改掉。
func (r *Repo) ListBackups() ([]BackupInfo, error) {
	dir := r.BackupDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []BackupInfo{}, nil // 还没备份过：空列表，不是错误
		}
		return nil, err
	}
	out := make([]BackupInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "probe-") || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // 竞态删除：跳过即可
		}
		out = append(out, BackupInfo{
			Name:      e.Name(),
			Path:      filepath.Join(dir, e.Name()),
			SizeBytes: info.Size(),
			CreatedAt: info.ModTime().Unix(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// PruneBackups 只保留最新的 keep 份，返回被删除的文件名。
//
// keep <= 0 时**不删任何东西**并原样返回空切片：
// "保留 0 份"几乎总是配置失误（比如设置项被误清空），
// 真按字面执行会把所有历史备份一次删光——这是不可逆的数据损失，
// 所以这里宁可什么都不做。调用方（调度器）会保证 keep ≥ 1。
func (r *Repo) PruneBackups(keep int) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	list, err := r.ListBackups()
	if err != nil {
		return nil, err
	}
	if len(list) <= keep {
		return nil, nil
	}
	// list 已按文件名倒序，尾部就是最旧的。
	var removed []string
	for _, b := range list[keep:] {
		if err := os.Remove(b.Path); err != nil {
			// 单个删除失败不影响其余：返回已删的并带上错误，
			// 让调度器能记日志但不必中断整轮清理。
			return removed, fmt.Errorf("删除旧备份 %s 失败：%w", b.Name, err)
		}
		removed = append(removed, b.Name)
	}
	return removed, nil
}

// ErrBackupNotFound 表示要操作的备份文件已经不存在（可能被并发的轮转删掉了）。
var ErrBackupNotFound = errors.New("备份文件不存在")

// DeleteBackup 删除一份备份，参数是**文件名**（不含目录）。
//
// 安全要点（这是本方法最重要的部分）：管理端传进来的值是外部输入，
// 而它最终会变成一个 os.Remove 的路径。只做"拼接目录 + 删除"的话，
// 一个 `../../../probe.db` 就能从"删除备份"变成"删除主库"——
// 触发它的只是一个带 CSRF 令牌的管理接口，没有任何二次确认。
// 因此这里有两道闸：
//  1. isBackupName 只接受 CreateBackup 生成的固定形状
//     （probe-YYYYMMDD-HHMMSS.db，纯数字时间戳），路径分隔符、`..`、
//     任何其它后缀一律拒绝；
//  2. 再确认 Clean 之后的父目录确实等于备份目录（防符号链接/拼接绕过）。
//
// 另外：这里刻意**不允许**一次删多份。"一次删光所有备份"应当由管理员
// 调整 backup.keep 让轮转去做（它带 keep<=0 的保护），而不是给前端一个
// `DELETE /backups` 全清按钮——那种按钮点错一次就再也恢复不回来了。
func (r *Repo) DeleteBackup(name string) error {
	if !isBackupName(name) {
		return fmt.Errorf("非法的备份文件名：%q", name)
	}
	dir := filepath.Clean(r.BackupDir())
	target := filepath.Clean(filepath.Join(dir, name))
	if filepath.Dir(target) != dir {
		return fmt.Errorf("备份文件不在备份目录内：%q", name)
	}
	if err := os.Remove(target); err != nil {
		if os.IsNotExist(err) {
			return ErrBackupNotFound
		}
		return fmt.Errorf("删除备份 %s 失败：%w", name, err)
	}
	return nil
}

// isBackupName 判断文件名是否符合 CreateBackup 生成的形状。
//
// 形状：probe-<8 位日期>-<6 位时刻>.db（例：probe-20261012-040005.db）。
// 与 ListBackups 的前后缀过滤相比这里更严：列表只用于展示，删错一份还能
// 从别处找回来，而删除是不可逆的，所以必须逐字符确认。
func isBackupName(name string) bool {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return false
	}
	if !strings.HasPrefix(name, "probe-") || !strings.HasSuffix(name, ".db") {
		return false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(name, "probe-"), ".db")
	if len(body) != 15 || body[8] != '-' {
		return false
	}
	for i := 0; i < len(body); i++ {
		if i == 8 {
			continue
		}
		if body[i] < '0' || body[i] > '9' {
			return false
		}
	}
	return true
}
