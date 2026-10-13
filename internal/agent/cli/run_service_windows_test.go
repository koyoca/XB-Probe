//go:build windows

package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenServiceLogWritesToConfigDir 钉住服务日志的位置与权限。
//
// 服务模式下 stdout 是黑洞，agent.log 是排障唯一入口；
// 目录换了（比如跟着 cwd 走）就会变成"日志不知道跑哪去了"。
func TestOpenServiceLogWritesToConfigDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "nested", "agent.yaml")

	f := openServiceLog(cfgPath)
	if f == nil {
		t.Fatal("openServiceLog 返回 nil，服务模式下日志会全部丢失")
	}
	defer f.Close()

	want := filepath.Join(filepath.Dir(cfgPath), "agent.log")
	got, err := filepath.Abs(f.Name())
	if err != nil {
		t.Fatalf("取日志文件名失败：%v", err)
	}
	wantAbs, err := filepath.Abs(want)
	if err != nil {
		t.Fatalf("取期望路径失败：%v", err)
	}
	if got != wantAbs {
		t.Fatalf("日志位置不对：%q，期望 %q", got, wantAbs)
	}

	// 配置目录不存在时应被创建出来（服务首次启动时目录可能还没建）。
	if _, err := os.Stat(filepath.Dir(cfgPath)); err != nil {
		t.Fatalf("日志目录未创建：%v", err)
	}

	// 写入要真的落到文件（追加模式），否则"日志文件存在但永远是空的"。
	const line = "probe-agent 启动\n"
	if _, err := f.WriteString(line); err != nil {
		t.Fatalf("写日志失败：%v", err)
	}
	raw, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("读取日志失败：%v", err)
	}
	if string(raw) != line {
		t.Fatalf("日志内容不对：%q", string(raw))
	}
}

// TestRunAsServiceStaysInForegroundForPlainProcess 普通前台进程绝不能被
// 当成系统服务：一旦被吞掉，`probe-agent run` 会静默返回、什么都不干。
func TestRunAsServiceStaysInForegroundForPlainProcess(t *testing.T) {
	called := false
	entered := runAsService(filepath.Join(t.TempDir(), "agent.yaml"), false, func(_ context.Context) error {
		called = true
		return nil
	})
	if entered {
		t.Fatal("普通进程被判为服务模式，主循环会被吞掉")
	}
	if called {
		t.Fatal("前台进程不应把主循环交给服务处理器")
	}
}
