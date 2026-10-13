//go:build linux || freebsd

package service

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
)

// lookupUser 是 os/user 的接缝。
//
// os/user 的结果来自"外部世界"（/etc/passwd、NSS、LDAP…），按项目里的做法，
// 凡护栏要断言"查到了 UID"这类事，就得先能替换它、并断言替身真的生效。
var lookupUser = user.Lookup

// HandoverToUser 把配置目录与配置文件交给服务用户。
//
// 必须在 **ensureUser 之后**、**启动服务之前**调用：
//   - 之前：用户还不存在，查不到 UID；
//   - 之后：服务一起来就按 unit 里的 User= 读配置，此时属主还没换就是 permission denied。
//
// 失败**必须**当成安装失败：这正是上面那个真机故障的成因，静默继续只会让用户
// 拿到一个"装成功了但起不来"的节点。
func HandoverToUser(cfgPath, name string) error {
	if name == "" {
		name = DefaultUser
	}
	u, err := lookupUser(name)
	if err != nil {
		return fmt.Errorf("查不到服务用户 %s，无法交接配置属主: %w", name, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("服务用户 %s 的 UID 不是数字（%q）: %w", name, u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return fmt.Errorf("服务用户 %s 的 GID 不是数字（%q）: %w", name, u.Gid, err)
	}

	for _, t := range handoverTargets(cfgPath) {
		if _, err := os.Lstat(t.Path); err != nil {
			// 清单文件可能还没写（--no-service 那支就只写配置）；目录与配置必须在。
			if os.IsNotExist(err) && !t.Dir {
				continue
			}
			return fmt.Errorf("交接前无法访问 %s: %w", t.Path, err)
		}
		if err := os.Chown(t.Path, uid, gid); err != nil {
			return fmt.Errorf("把 %s 的属主改为 %s 失败: %w", t.Path, name, err)
		}
		if err := os.Chmod(t.Path, t.Mode); err != nil {
			return fmt.Errorf("设置 %s 权限为 %#o 失败: %w", t.Path, t.Mode, err)
		}
	}
	return nil
}

// EnsureLogFile 预建服务日志文件并把属主交给服务用户（不存在则创建，存在则只改属主）。
//
// 为什么必须有这一步（Alpine/OpenRC 真机故障）：
// OpenRC 的 init 脚本里写了 output_log/error_log，而服务是以**非 root** 用户跑的。
// 日志文件在 /var/log（root 属主、0755）下，服务用户自己创建不了它；
// 于是 `rc-service xb-probe-agent start` 在重定向这一步就失败，表现为
// "服务注册成功但起不来"，而错误只有一句 openrc-run 的模糊提示。
//
// 这里**不动已有文件的属主之外的东西**：日志可能是上次运行留下的，清空它等于
// 把用户排查故障的现场擦掉。只在不存在时创建。
func EnsureLogFile(path, name string) error {
	if name == "" {
		name = DefaultUser
	}
	u, err := lookupUser(name)
	if err != nil {
		return fmt.Errorf("查不到服务用户 %s，无法准备日志文件: %w", name, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("服务用户 %s 的 UID 不是数字（%q）: %w", name, u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return fmt.Errorf("服务用户 %s 的 GID 不是数字（%q）: %w", name, u.Gid, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("创建服务日志 %s 失败: %w", path, err)
	}
	_ = f.Close()
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("把日志 %s 的属主改为 %s 失败: %w", path, name, err)
	}
	return nil
}
