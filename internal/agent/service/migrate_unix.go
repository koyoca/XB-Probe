//go:build linux || freebsd

package service

import (
	"fmt"
	"os"
	"path/filepath"
)

// 项目在改名（probe-* → xb-probe-*）之前，服务定义、配置目录与日志用的是另一套路径。
// 老节点上这些东西**不会自己消失**，而它们的危害是静默的：
//
//   - 旧的 probe-agent.service / /etc/init.d/probe-agent 还在，且**仍在运行** ——
//     于是机器上有两个 Agent 同时往同一个 Master 上报，一个用旧令牌、一个用新令牌。
//     表现出来是"流量翻倍 / 节点一会儿在线一会儿离线"，而两边的日志各自都很正常。
//   - 旧配置目录 /etc/probe-agent 里的 agent.yaml 是 node_id 与令牌所在，
//     不搬过来就等于"这台机器要重新注册"，用户看到的是"我的节点丢了"。
//
// 因此安装/升级的第一件事是把这个老布局收拾干净。它必须是**幂等**的：
// 没装过老版本的机器上跑一遍什么都不会发生。
//
// 两个刻意的选择：
//   - **只在目标不存在时才搬**。老目录与新目录同时存在时，新目录是权威
//     （可能是用户手工放进去的），搬过去等于把它覆盖掉。
//   - **先停服务再删定义**。只删文件不停服务的话，那个进程会一直跑到重启为止。

// legacyDirMoves 是"旧配置目录 → 新配置目录"。顺序无所谓，各对之间互不重叠。
func legacyDirMoves() [][2]string {
	return [][2]string{
		{"/etc/probe-agent", "/etc/xb-probe-agent"},
		{"/usr/local/etc/probe-agent", "/usr/local/etc/xb-probe-agent"},
	}
}

// legacyFiles 是旧的服务定义与日志文件（停掉服务后逐个删除）。
func legacyFiles() []string {
	return []string{
		"/etc/systemd/system/probe-agent.service",
		"/etc/init.d/probe-agent",
		"/etc/conf.d/probe-agent",
		"/usr/local/etc/rc.d/probe-agent",
		"/var/log/probe-agent.log",
	}
}

// MigrateLegacy 把改名之前留下的服务定义与配置目录迁移到新布局。
//
// 绝不返回错误把安装挡死：迁移是"顺手清理"，真失败了也应该继续安装
// （最坏的结果是老服务还跑着，那是用户能在 ps 里看见的显式状态，
// 而"因为迁不动所以装不上"是更难自查的失败）。
func MigrateLegacy() {
	for _, f := range legacyFiles() {
		if !exists(f) {
			continue
		}
		removeLegacyServiceDefinition(f)
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "提示：旧的 %s 没能删除（可手动删除）：%v\n", f, err)
			continue
		}
		fmt.Printf("已清理改名前的旧文件：%s\n", f)
	}

	for _, m := range legacyDirMoves() {
		oldDir, newDir := m[0], m[1]
		if !exists(oldDir) || exists(newDir) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(newDir), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "提示：无法创建 %s（可手动迁移 %s）：%v\n", filepath.Dir(newDir), oldDir, err)
			continue
		}
		if err := os.Rename(oldDir, newDir); err != nil {
			fmt.Fprintf(os.Stderr, "提示：旧配置目录 %s 没能迁移到 %s（可手动 mv）：%v\n", oldDir, newDir, err)
			continue
		}
		fmt.Printf("已把旧配置目录 %s 迁移到 %s（令牌与 node_id 原样保留）。\n", oldDir, newDir)
	}
}

// removeLegacyServiceDefinition 在删除旧的 init 文件之前，先把对应服务停下来。
//
// 按路径判定该停哪一类，而不是复用 Detect()：Detect() 回答的是"**这台机器**现在
// 用哪个管理器"，而这里问的是"**这个文件**当年是被谁接管的"。两者在
// "systemd 机器上残留了一个旧的 OpenRC 脚本"这类情况下答案不同。
func removeLegacyServiceDefinition(path string) {
	name := filepath.Base(path)
	switch {
	case name == "probe-agent.service":
		_ = run("systemctl", "disable", "--now", "probe-agent")
		_ = run("systemctl", "daemon-reload")
	case path == "/etc/init.d/probe-agent":
		_ = run("rc-service", "probe-agent", "stop")
		_ = run("rc-update", "del", "probe-agent", "default")
		_ = run(path, "stop")
		_ = run(path, "disable")
	case path == "/usr/local/etc/rc.d/probe-agent":
		_ = run("service", "probe-agent", "stop")
	}
}
