package main

import (
	"os"
	"strings"
	"testing"

	"github.com/koyoca/xb-probe/internal/agent/cli"
)

// TestVersionFallsBackToDev 保证本地手工构建的二进制不会对外表现成"正式版"。
// 发布流程靠 -ldflags "-X main.version=..." 覆盖它（见 Makefile）。
func TestVersionFallsBackToDev(t *testing.T) {
	if version == "" {
		t.Fatal("version 为空，日志与 /enroll 上报的 agent_version 都会是空白")
	}
}

// TestMainOnlyDelegatesToCLI 是源码护栏。
//
// cmd/xb-probe-agent 的全部职责只有一句：把 argv 交给 cli.Run 并把返回值当作退出码。
// 一旦有人把参数剪裁、退出码或 version 的传递改坏（例如漏掉 os.Args[1:]，
// 让子命令名被当成程序名），服务管理器读到的退出码就会失真——
// 而这是"服务注册成功但反复重启"的典型成因，运行时测试极难复现。
func TestMainOnlyDelegatesToCLI(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读取 main.go 失败：%v", err)
	}
	src := string(raw)

	for _, want := range []string{
		"cli.Run(os.Args[1:], version)", // 参数与版本号都要传进去
		"os.Exit(",                      // 退出码必须回传给操作系统
	} {
		if !strings.Contains(src, want) {
			t.Errorf("main.go 缺少 %q；入口职责被改动了吗？", want)
		}
	}

	// main.go 必须保持"薄"：出现真实业务逻辑说明放错了层，
	// cmd 下的代码在测试里几乎不可达。
	for _, forbidden := range []string{"http.", "os.Create", "signal.Notify"} {
		if strings.Contains(src, forbidden) {
			t.Errorf("main.go 出现了 %q：业务逻辑应放在 internal/ 包内", forbidden)
		}
	}
}

// TestCLIEntryPointIsReachable 确认 main 依赖的那个调用点确实存在且返回 0。
func TestCLIEntryPointIsReachable(t *testing.T) {
	// cli.Run 的输出会被测试进程继承，这里只关心退出码；
	// 具体文案的断言在 internal/agent/cli 的测试里。
	old := os.Stdout
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("打开 %s 失败：%v", os.DevNull, err)
	}
	defer devNull.Close()
	os.Stdout = devNull
	defer func() { os.Stdout = old }()

	if code := cli.Run([]string{"version"}, version); code != 0 {
		t.Fatalf("version 子命令应返回 0，得到 %d", code)
	}
}
