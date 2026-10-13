// Package updater 实现 Agent 的自升级（12.6 的客户端侧）。
//
// 取件走 **agent 作用域**（节点令牌鉴权）而不是公开的 /dist/：
// install.mode=on_demand 下注册码一旦被消费，/dist/ 就关闭了——
// 按它的语义这是对的（不该长期匿名分发二进制），但升级会被一起关死。
//
// 三条不可让步的规则：
//
//  1. **先校验后落地**。下载到同目录的临时文件，哈希对上才允许改名为正式文件；
//     校验通过前目标路径上不会出现任何内容，中途崩溃不会留下"看着像已完成、实际是坏的"二进制。
//  2. **绝不主动降级**。"比最新还新"也算无需动作（Master 回滚版本时不能把全网一起拖下去）；
//     降级只能由运维显式指定 --version。
//  3. **替换失败必须还原**。改名失败会把备份换回来，不留半套。
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"github.com/koyoca/xb-probe/internal/agent/client"
)

// 接口路径（与 internal/api 的 Agent 路由一一对应）。
const (
	releasePath  = "/api/agent/v1/releases/latest"
	artifactPath = "/api/agent/v1/artifact"
)

// Release 是"有没有新版本"的答案，字段与服务端 agentRelease 一致。
type Release struct {
	Version  string `json:"version"`
	Artifact string `json:"artifact"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Current  string `json:"current"`
	UpToDate bool   `json:"up_to_date"`
}

// Options 是 Upgrade 的参数。
type Options struct {
	Platform string // 发布标签（linux_amd64 / windows_amd64 / linux_armv7）
	Current  string // 当前版本号（用于展示与 up_to_date 判定）
	Version  string // 指定版本；空表示最新
	Target   string // 要被替换的可执行文件路径
	DryRun   bool   // 下载并校验，但不替换
}

// Result 描述一次升级的结果。
type Result struct {
	From     string // 原版本
	To       string // 新版本
	Path     string // 被替换的文件
	Backup   string // 备份文件路径（.old）
	Size     int64  // 写入字节数
	NoChange bool   // 已是最新，未做任何改动
}

// Check 查询 Master 上的最新发布版本。
func Check(ctx context.Context, c *client.Client, platform, current string) (*Release, error) {
	if platform == "" {
		return nil, errors.New("缺少 platform（发布标签）")
	}
	q := url.Values{}
	q.Set("platform", platform)
	if current != "" {
		q.Set("current", current)
	}
	var rel Release
	if err := c.Get(ctx, releasePath+"?"+q.Encode(), &rel); err != nil {
		return nil, err
	}
	// 版本号或校验和缺失时不能"当作最新"——那会让升级永远静默跳过，
	// 而运维以为已经在自动更新了。
	if rel.Version == "" || rel.SHA256 == "" {
		return nil, fmt.Errorf("服务端返回的版本信息不完整（version=%q sha256=%q）", rel.Version, rel.SHA256)
	}
	return &rel, nil
}

// Upgrade 下载并替换自身。
//
// 替换成功后**不自己重启**：进程自我重启在 Windows 服务、systemd、容器里各有各的坑，
// 统一交给服务管理器（Restart=always / SCM 失败恢复）或运维执行重启命令，
// 由调用方（cli）把该执行的命令打印出来。
func Upgrade(ctx context.Context, c *client.Client, o Options) (*Result, error) {
	if o.Target == "" {
		return nil, errors.New("未指定要替换的目标路径")
	}
	if o.Platform == "" {
		return nil, errors.New("缺少 platform（发布标签）")
	}

	rel, err := Check(ctx, c, o.Platform, o.Current)
	if err != nil {
		return nil, err
	}
	// 指定版本时不看 up_to_date：显式降级/重装是运维的决定。
	if o.Version == "" && rel.UpToDate {
		return &Result{From: o.Current, To: rel.Version, Path: o.Target, NoChange: true}, nil
	}
	version := o.Version
	if version == "" {
		version = rel.Version
	}

	dir := filepath.Dir(o.Target)
	tmp := filepath.Join(dir, filepath.Base(o.Target)+".new")
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败：%w", err)
	}

	q := url.Values{}
	q.Set("platform", o.Platform)
	q.Set("version", version)
	resp, err := c.Download(ctx, artifactPath+"?"+q.Encode())
	if err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("下载构件失败：%w", err)
	}
	defer resp.Body.Close()

	// 校验和来自响应头（服务端从 SHA256SUMS 取）。指定版本时它是唯一的期望值，
	// 所以缺失必须报错——宁可失败也不能"没校验就装"。
	want := resp.Header.Get("X-Checksum-Sha256")
	if want == "" {
		f.Close()
		_ = os.Remove(tmp)
		return nil, errors.New("服务端未返回校验和（X-Checksum-Sha256），拒绝安装")
	}
	// 两处都说了校验和时必须一致：不一致说明"清单"与"实际下发"不是同一份，
	// 这类不一致正是投毒最典型的形态。
	if version == rel.Version && rel.SHA256 != "" && want != rel.SHA256 {
		f.Close()
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("校验和不一致（清单 %s / 下发 %s），拒绝安装", rel.SHA256, want)
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("写入构件失败：%w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("关闭构件文件失败：%w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("校验和不匹配：下载 %s，期望 %s（文件已删除）", got, want)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("设置可执行权限失败：%w", err)
	}

	if o.DryRun {
		_ = os.Remove(tmp)
		return &Result{From: o.Current, To: version, Path: o.Target, Size: n}, nil
	}

	backup := o.Target + ".old"
	// 上一轮留下的备份先清掉；Windows 上它可能还被占用，删不掉就跳过——
	// 下面那次改名会失败并给出明确错误，不会静默装上一个坏的。
	_ = os.Remove(backup)
	if err := os.Rename(o.Target, backup); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("备份当前二进制失败（未做任何改动）：%w", err)
	}
	if err := os.Rename(tmp, o.Target); err != nil {
		// 还原：把备份换回去，绝不留"目标缺失 + 有 .new"的半套状态。
		if rerr := os.Rename(backup, o.Target); rerr != nil {
			return nil, fmt.Errorf("替换失败且还原失败：%v / %v（请手工恢复 %s）", err, rerr, backup)
		}
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("替换失败（已还原为原版本）：%w", err)
	}
	return &Result{From: o.Current, To: version, Path: o.Target, Backup: backup, Size: n}, nil
}

// Rollback 把 .old 备份换回来。
//
// 当前的（可能跑不起来的）那个不直接删，挪成 .broken 留现场：
// 升级失败本身就是事故，把"坏的那个"留着才好复盘。
func Rollback(target string) error {
	backup := target + ".old"
	if _, err := os.Stat(backup); err != nil {
		return fmt.Errorf("没有可用的备份（%s）：%w", backup, err)
	}
	broken := target + ".broken"
	_ = os.Remove(broken)
	if err := os.Rename(target, broken); err != nil {
		return fmt.Errorf("挪开当前二进制失败：%w", err)
	}
	if err := os.Rename(backup, target); err != nil {
		if rerr := os.Rename(broken, target); rerr != nil {
			return fmt.Errorf("还原失败且还原现场失败：%v / %v", err, rerr)
		}
		return fmt.Errorf("还原失败（已还原现场）：%w", err)
	}
	return nil
}
