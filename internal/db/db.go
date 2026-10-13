// Package db 负责 SQLite 连接管理与 schema 迁移。
//
// 设计要点（文档 4.2 / 6.1 / 13.4.8）：
//   - 纯 Go 驱动 modernc.org/sqlite，CGO_ENABLED=0 可静态编译（1. 定位）。
//   - WAL + synchronous=NORMAL + busy_timeout + temp_store=MEMORY + auto_vacuum=INCREMENTAL。
//   - 写连接唯一（单写协程 + 批量事务），读连接独立，互不阻塞。
package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Store 持有读写两条 SQLite 连接。
type Store struct {
	// Write 是唯一的写连接（MaxOpenConns=1）。所有写操作必须经此连接，
	// 由 internal/pipeline 的单写协程串行化。
	Write *sql.DB
	// Read 供查询使用，可并发。
	Read *sql.DB

	path string
}

// pragmas 是每个连接都要设置的 PRAGMA（4.2 第 4 条）。
var pragmas = []string{
	"journal_mode(WAL)",
	"synchronous(NORMAL)",
	"busy_timeout(5000)",
	"foreign_keys(ON)",
	"temp_store(MEMORY)",
	"auto_vacuum(INCREMENTAL)",
}

// Open 打开 dataDir/probe.db。dataDir 权限应为 0700（S13）。
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	// 确保已有目录权限正确（不修改属主）。
	if err := os.Chmod(dataDir, 0o700); err != nil && !os.IsPermission(err) {
		return nil, fmt.Errorf("chmod data dir: %w", err)
	}

	path := filepath.Join(dataDir, "probe.db")
	dsn := buildDSN(path)

	write, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open write conn: %w", err)
	}
	write.SetMaxOpenConns(1) // 单写者
	write.SetMaxIdleConns(1)

	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("open read conn: %w", err)
	}
	read.SetMaxOpenConns(8)

	st := &Store{Write: write, Read: read, path: path}
	if err := st.pingAndCheck(); err != nil {
		_ = st.Close()
		return nil, err
	}
	// 注意：库文件一定是存在的——sql.Open 是惰性的，真正的文件创建发生在
	// pingAndCheck 里 Ping 拿到第一条连接时。若把那个校验整段拿掉，这里会变成
	// "chmod: 找不到文件"，报错信息完全指不到真正的原因。
	if err := os.Chmod(path, 0o600); err != nil && !os.IsPermission(err) {
		return nil, fmt.Errorf("chmod db: %w", err)
	}
	return st, nil
}

func buildDSN(path string) string {
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	// modernc.org/sqlite 使用 file: URI 形式传递 _pragma。
	return "file:" + path + "?" + q.Encode()
}

// pingAndCheck 启动时做 PRAGMA quick_check，失败提示从最近备份恢复（13.4.8）。
func (s *Store) pingAndCheck() error {
	if err := s.Write.Ping(); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	var result string
	if err := s.Write.QueryRow(`PRAGMA quick_check`).Scan(&result); err != nil {
		return fmt.Errorf("quick_check: %w", err)
	}
	if !strings.EqualFold(result, "ok") {
		return fmt.Errorf("database integrity check failed (%s); 请从最近备份恢复", result)
	}
	return nil
}

// Path 返回数据库文件路径（备份/统计使用）。
func (s *Store) Path() string { return s.path }

// Close 关闭读写连接。调用方应先 flush 写队列（4.2 第 5 条）。
func (s *Store) Close() error {
	var err error
	if s.Read != nil {
		if e := s.Read.Close(); e != nil {
			err = e
		}
	}
	if s.Write != nil {
		if e := s.Write.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
