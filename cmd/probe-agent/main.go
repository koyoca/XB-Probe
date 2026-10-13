// probe-agent 是 Server Probe 的节点侧代理（文档第 3 章 / 第 12 章）。
//
// Agent 是纯外联程序：不监听任何端口，不解析也不执行任何脚本或命令，
// 因此系统不存在任何远程控制通道（包括远程卸载与远程命令），从架构上排除 RCE。
package main

import (
	"os"

	"github.com/yourorg/probe/internal/agent/cli"
)

// version 由 -ldflags "-X main.version=..." 注入（见 Makefile）。
var version = "dev"

func main() {
	os.Exit(cli.Run(os.Args[1:], version))
}
