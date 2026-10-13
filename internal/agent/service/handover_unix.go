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
