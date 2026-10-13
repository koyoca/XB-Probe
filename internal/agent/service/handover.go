package service

import (
	"os"
	"path/filepath"
)

// 配置目录/文件的"交接"：安装期间它们由 **root** 写（install/reenroll 都要 sudo），
// 而服务是以**专用低权限用户**跑的 —— 不把属主交过去，服务读到的是一个
// root:root 0600 的文件，启动即 `permission denied`。
//
// 这就是真机上报的那个故障：
//
//	已注册节点（node_id=1），配置写入 /etc/xb-probe-agent/agent.yaml（权限 0600）。
//	安装完成。
//	xb-probe-agent[558816]: 错误： 读取配置失败: open /etc/xb-probe-agent/agent.yaml: permission denied
//
// 安装脚本每一步都打印了"成功"，唯一失败的是服务本身 —— 而 `User=probe`
// 与 `0600 root:root` 是分两个文件、两个时刻各自"正确"的，撞在一起才出问题。

// ManifestName 是安装清单的文件名（卸载时只删除清单里记录的路径，避免误删，S13）。
//
// 它同时被 cli 包读写，所以放在这里当唯一来源——两处各写一个字面量，
// 改名时漏掉一处就是"卸载删不干净"或"清单读不到"。
const ManifestName = "install.manifest"

// DefaultUser 是 Agent 运行时使用的专用低权限用户。
//
// 单独一个常量而不是在四个 manager 里各写一次字面量 `"probe"`：
// 交接属主时必须**用同一个名字**，两处不一致会得到"用户建了、文件交给了别人"。
const DefaultUser = "probe"

// handoverPath 描述一个待交接的路径。
type handoverPath struct {
	Path string
	Mode os.FileMode
	Dir  bool
}

// handoverTargets 列出安装后必须归服务用户所有的路径。
//
// 抽成纯函数（不碰系统调用）是为了能在任意平台上直接断言这张表——本机是 Windows，
// 真正的 chown 只能在 unix 上跑，但"要交接哪些路径、什么权限"是平台无关的。
//
// 目录需要**可写**（0750 且属主是服务用户），因为运行期 Master 下发新配置时，
// agentconfig.Save 会在同目录写 `agent.yaml.tmp` 再 rename——只给读权限的话，
// 配置能读、却写不回去，症状是"后台改的设置重启就没了"。
func handoverTargets(cfgPath string) []handoverPath {
	dir := filepath.Dir(cfgPath)
	return []handoverPath{
		{Path: dir, Mode: 0o750, Dir: true},
		{Path: cfgPath, Mode: 0o600},
		{Path: filepath.Join(dir, ManifestName), Mode: 0o600},
	}
}
