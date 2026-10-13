//go:build windows

package service

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// legacyWindowsService 是改名之前注册的 Windows 服务名。
//
// 它与新名字是两个独立的 SCM 条目，**不会互相覆盖**：不清理的话，
// 老服务还在开机自启、还在上报，而新服务也起来了 —— 同一个节点两份上报。
const legacyWindowsService = "probe-agent"

// MigrateLegacy 清理改名之前留下的 Windows 服务条目与配置目录。
//
// 不返回错误把安装挡死（理由同 unix 侧：迁移失败最坏是老服务还在跑，
// 而"因为迁不动所以装不上"更难自查）。
func MigrateLegacy() {
	removeLegacyWindowsService()

	oldDir := filepath.Join(os.Getenv("ProgramData"), "probe-agent")
	newDir := filepath.Join(os.Getenv("ProgramData"), "xb-probe-agent")
	if oldDir != newDir && exists(oldDir) && !exists(newDir) {
		if err := os.Rename(oldDir, newDir); err != nil {
			fmt.Fprintf(os.Stderr, "提示：旧配置目录 %s 没能迁移到 %s（可手动移动）：%v\n", oldDir, newDir, err)
		} else {
			fmt.Printf("已把旧配置目录 %s 迁移到 %s（令牌与 node_id 原样保留）。\n", oldDir, newDir)
		}
	}
}

func removeLegacyWindowsService() {
	mg, err := mgr.Connect()
	if err != nil {
		return // 非管理员或 SCM 不可用：交给 Install 去报那个更准确的错
	}
	defer mg.Disconnect()

	s, err := mg.OpenService(legacyWindowsService)
	if err != nil {
		return // 没装过老版本
	}
	// 先停再删：只 DeleteService 的话，正在跑的进程会一直活到重启。
	if _, err := s.Control(svc.Stop); err == nil {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			st, qerr := s.Query()
			if qerr != nil || st.State == svc.Stopped {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	if err := s.Delete(); err == nil {
		fmt.Printf("已清理改名前的旧服务：%s\n", legacyWindowsService)
	}
	s.Close()
}
