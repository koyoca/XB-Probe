//go:build windows

package cli

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/yourorg/probe/internal/agent/service"
)

// runAsService 在 Windows 上处理"以 SCM 服务方式启动"的分支。
//
// 为什么必须单独一条路径：
//   - 服务进程没有控制台，stdout 会丢失，日志要落到文件（ProgramData\probe-agent\agent.log）；
//   - SCM 的停止请求不走信号，必须实现 svc.Handler 才能优雅退出；
//   - `sc start` 启动时命令行是 `probe-agent run --config ...`，无法通过参数区分
//     "前台 run" 与 "服务 run"——只能问 SCM（svc.IsWindowsService）。
//
// 返回值 true 表示"已经以服务模式跑完，调用方应直接返回"。
func runAsService(cfgPath string, force bool, run func(context.Context) error) bool {
	if !force && !service.IsWindowsService() {
		return false
	}

	// 服务模式下把日志写到文件而不是 stdout。
	// 注意不能用 slog 的默认 handler 写到 os.Stdout——服务没有控制台，
	// 写进去就是黑洞，排障时什么都看不到。
	if f := openServiceLog(cfgPath); f != nil {
		defer f.Close()
		logger := slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
		slog.SetDefault(logger)
	}

	stop := make(chan struct{})
	_ = service.RunWindowsService(cfgPath, func(_ string, sc <-chan struct{}) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() { done <- run(ctx) }()

		select {
		case err := <-done:
			return err
		case <-sc:
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
			}
			return nil
		}
	}, stop)
	return true
}

// openServiceLog 打开服务日志文件（按天追加）。
func openServiceLog(cfgPath string) *os.File {
	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, "agent.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}
