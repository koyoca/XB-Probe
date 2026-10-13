package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// 未知顶层命令必须**明确拒绝**，绝不能 fall through 到服务启动。
//
// 这一条是从一次真机事故里挖出来的根因，值得把链条写全：
//
//  1. 部署脚本用 `xb-probe-master tls status -config <临时文件>` 探测
//     "二进制认不认识某个配置键"（配置模板来自 main、二进制来自 Release 标签，
//     两者会错配，而 LoadMaster 是严格解析，写错一个键＝服务起不来）。
//  2. 旧二进制（Release 的 v0.1.0）**没有 tls 子命令**，而当时的 main() 顶层
//     switch 没有 default：`tls` 不匹配任何 case，一路落到 flag.Parse + run()。
//  3. flag.Parse 遇到第一个非 flag 参数 `tls` 就**停止解析**，
//     于是 `-config <临时文件>` 被整个忽略，读的是默认路径的真实配置。
//  4. 结果"基线"与"待测"两次调用读的是**同一份**文件 → 探测恒回答"认识"
//     → 脚本写下 sync_obtain → 服务启动失败 → 回滚，而回滚的理由是
//     "启动后立即退出"，把真正的原因藏了起来。
//
// 修法是两步：① main() 加 default 拒绝未知命令（本测试钉的就是这一步）；
// ② 探测加哨兵自检（脚本侧，见 internal/dist/deploy_ux_test.go）。
// 只做 ② 也能挡住，但 ① 修的是"打错命令就把服务启动起来"这个本身的错误。
func TestUnknownCommandIsRejectedInsteadOfStartingTheServer(t *testing.T) {
	if _, err := os.Stat("../../configs/master.yaml.example"); err != nil {
		t.Fatal("本测试要从 cmd/xb-probe-master 目录跑：", err)
	}

	// 抓 stderr：用法是打给 stderr 的，顺便断言它真的给了提示。
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal("建管道失败：", err)
	}
	os.Stderr = w
	code := runMain([]string{"xb-probe-master", "foobar", "-config", "/nonexistent/nope.yaml"})
	w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	out := buf.String()

	if code != 2 {
		t.Errorf("未知命令的退出码 = %d，期望 2（不是 0、也不是 1）：\n"+
			"  期望它明确拒绝；返回 0/1 意味着它可能 fall through 到了别的地方。", code)
	}
	if !strings.Contains(out, "未知命令") {
		t.Error("未知命令没有说明是哪个命令不认识：\n" +
			"  用户需要看到 `未知命令 \"foobar\"` 才知道自己打错了什么。")
	}
	// 关键：绝不能落到服务启动。启动路径会去读配置文件并打印"启动失败/读取配置"。
	if strings.Contains(out, "启动失败") {
		t.Errorf("未知命令 fall through 到了服务启动（输出里有「启动失败」）：\n"+
			"  这是本测试要钉死的行为——`xb-probe-master tls ...` 在旧二进制上\n"+
			"  曾经真的把服务启动起来，还顺手忽略了 -config。\n实际输出：\n%s", out)
	}
}

// `-config X` 这类 flag 写法必须仍然走"启动服务"，不能被上面的 default 误伤。
// 判据：它去读了 -config 指定的那个文件（而不是默认路径）。
func TestConfigFlagStillReachesTheServiceStartupPath(t *testing.T) {
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal("建管道失败：", err)
	}
	os.Stderr = w
	// 文件不存在 → 启动路径会报"读取配置失败"并点名这个路径。
	code := runMain([]string{"xb-probe-master", "-config", "/definitely/not/here.yaml"})
	w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "读取配置失败") {
		t.Errorf("-config 没走到服务启动路径（退出码 %d）：\n"+
			"  default 分支只该拦**非 flag** 的参数，`-config X` 仍是启动服务的正常写法。\n"+
			"实际输出：\n%s", code, out)
	}
	if !strings.Contains(out, "not/here.yaml") {
		t.Errorf("-config 指定的路径没有被使用：\n"+
			"  flag.Parse 必须以 `-` 开头的参数继续解析，否则 -config 会被忽略。\n"+
			"实际输出：\n%s", out)
	}
}
