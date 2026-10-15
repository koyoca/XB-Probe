// Package collector 是 Agent 的采集层抽象（文档 12.5）。
//
// 各平台以独立文件（build tag）提供薄封装实现：
//   - Linux：读 /proc、statfs、/sys/class/net（本骨架已实现）
//   - macOS / FreeBSD：sysctl 与 getifaddrs
//   - Windows：GetSystemTimes、GlobalMemoryStatusEx、GetIfTable2、GetDiskFreeSpaceEx
//
// 不引入大型采集依赖，以守住 RSS ≤ 10 MB（11. 非功能指标）。
package collector

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/koyoca/xb-probe/internal/model"
)

// Collector 是平台采集接口。
type Collector interface {
	// Static 返回静态信息（启动时与变化时上报）。
	Static() (*model.Static, error)
	// Sample 返回一个采样点的指标（不含探测结果）。
	Sample() (*model.Metrics, error)
}

// Options 是采集选项。
type Options struct {
	NICInclude   []string
	NICExclude   []string
	DiskPaths    []string
	AgentVersion string
}

// connsProvider 是可选的连接数采集（Linux 由 /proc/net/tcp 统计）。
type connsProvider interface {
	Conns() int
}

// diskPaths 返回要统计的挂载点；为空时按平台默认。
func (o Options) diskPathsOr(def []string) []string {
	if len(o.DiskPaths) > 0 {
		return o.DiskPaths
	}
	return def
}

// nicAllowed 判断网卡是否计入统计。
// 默认排除 lo、docker*、veth*、br-*、virbr*、tun/tap/wg*（3.1 / 12.5）。
func nicAllowed(name string, include, exclude []string) bool {
	if len(include) > 0 {
		matched := false
		for _, p := range include {
			if ok, _ := filepath.Match(p, name); ok {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, p := range exclude {
		if ok, _ := filepath.Match(p, name); ok {
			return false
		}
	}
	return true
}

// DefaultNICExclude 是默认排除列表。
func DefaultNICExclude() []string {
	return []string{"lo", "docker*", "veth*", "br-*", "virbr*", "tun*", "tap*", "wg*"}
}

// rateCalculator 负责把累计字节数换算为速率（B/s）。
// 首次调用返回 0；计数器回绕/清零时按新值处理（与 Master 侧口径一致）。
type rateCalculator struct {
	mu     sync.Mutex
	lastRx int64
	lastTx int64
	lastAt time.Time
	primed bool
}

// rate 计算当前速率。
func (r *rateCalculator) rate(rx, tx int64, at time.Time) (rxBps, txBps int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.primed {
		r.lastRx, r.lastTx, r.lastAt, r.primed = rx, tx, at, true
		return 0, 0
	}
	elapsed := at.Sub(r.lastAt).Seconds()
	if elapsed <= 0 {
		return 0, 0
	}
	dr, dt := rx-r.lastRx, tx-r.lastTx
	if dr < 0 {
		dr = rx // 计数器重置
	}
	if dt < 0 {
		dt = tx
	}
	r.lastRx, r.lastTx, r.lastAt = rx, tx, at
	return int64(float64(dr) / elapsed), int64(float64(dt) / elapsed)
}

// pctX100 把 used/total 转为 ×100 的整数百分比。
func pctX100(used, total int64) int {
	if total <= 0 {
		return 0
	}
	v := int(used * 10000 / total)
	if v < 0 {
		return 0
	}
	if v > 10000 {
		return 10000
	}
	return v
}

// trimField 去除空白并截断（用于内核版本等字符串）。
func trimField(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// contains 是跨平台共用的小工具（Linux 的 /proc 探测与 Windows 的注册表探测都要用）。
// 放在本文件而不是各平台文件里，避免再往每个 build tag 下各抄一份。
func contains(s, sub string) bool { return strings.Contains(s, sub) }

// ---------------------------------------------------------------------------
// 容器身份解析（纯函数，供 Linux 的 detectVirt 使用；放在无 build tag 的文件里
// 是为了能在任何平台上被测试覆盖——linux 专属文件里的测试在开发机（Windows）
// 根本不会被执行，"被跳过的护栏等于没有"）
// ---------------------------------------------------------------------------

// containerNameOf 把容器运行时**自报**的名字归一化成本项目展示用的取值。
//
// 输入来自环境变量 container=<值>（LXC 的 lxc-start 默认注入 container=lxc，
// systemd 会把它透给各服务单元；docker / podman / systemd-nspawn 同理）。
//
//	"lxc" / "lxc-libvirt"      → lxc
//	"podman" / "containerd"    → docker（对本项目的展示没有区别）
//	"kubepods" / "kube"        → k8s
//	"systemd-nspawn" / "nspawn"→ systemd-nspawn
//	其他非空值                  → container（认得出"在容器里"，但叫不出名字）
//	空串                        → ""（没有这条证据）
func containerNameOf(v string) string {
	switch strings.TrimSpace(v) {
	case "":
		return ""
	case "lxc", "lxc-libvirt":
		return "lxc"
	case "docker":
		return "docker"
	case "podman", "containerd":
		return "docker"
	case "kubepods", "kube":
		return "k8s"
	case "systemd-nspawn", "nspawn":
		return "systemd-nspawn"
	case "openvz":
		return "openvz"
	case "wsl":
		return "wsl"
	default:
		// 自报了名字但不在已知集合里：报"通用容器"。
		// 绝不 fallthrough 到宿主机的指纹判据上去——那正是"LXC 显示 kvm"的成因。
		return "container"
	}
}

// envValue 从 "K=V\nK=V" 形态的内容里取 K 的值。
// readEnvFile 已经把 NUL 分隔换成了换行，所以这里按行找前缀即可。
func envValue(blob []byte, key string) string {
	prefix := key + "="
	for _, ln := range strings.Split(string(blob), "\n") {
		if strings.HasPrefix(ln, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(ln, prefix))
		}
	}
	return ""
}

// nsPidCount 解析 /proc/self/status 里的 NSpid 行，返回它列出的 PID 个数。
//
// NSpid 按"由外到内"列出该进程在每一层 PID 命名空间里的 PID：宿主机上只有一个值；
// 容器（有自己的 PID 命名空间）里有两个及以上。返回 0 表示没有这一行（老内核或
// 受限的 /proc）——那是"拿不到证据"，不等于"不在容器里"。
func nsPidCount(status string) int {
	for _, ln := range strings.Split(status, "\n") {
		if !strings.HasPrefix(ln, "NSpid:") {
			continue
		}
		return len(strings.Fields(strings.TrimPrefix(ln, "NSpid:")))
	}
	return 0
}

// initialPidNSIno 是**初始** PID 命名空间的 inode。这个值在内核源码里是静态
// 初始化的常量（init_pid_ns），历代内核都不变——因此"readlink 出来的 inode
// 不是它"本身就是"我们在另一个 PID 命名空间里"的充分证据。
const initialPidNSIno int64 = 4026531836

// pidNSIno 从 readlink(/proc/self/ns/pid) 的结果（形如 "pid:[4026531836]"）里
// 取出命名空间 inode。解析失败返回 0——那是"拿不到证据"，调用方不得当成
// "不在容器里"。
//
// 为什么需要它：NSpid 这一行是 Linux 4.15（2018）才加进 /proc/self/status 的。
// 老内核（比如还跑着 3.10 的 CentOS 7 宿主机）上的 LXC 容器根本没有这一行，
// nsPidCount 恒为 0，"隔离事实"这一层就漏了——容器里读到的 DMI 又全是宿主的，
// 最后 LXC 被报成宿主那台 KVM 虚机的 kvm。readlink 在任何内核上都可用，
// 且不需要任何权限。
func pidNSIno(link string) int64 {
	link = strings.TrimSpace(link)
	i := strings.IndexByte(link, '[')
	j := strings.LastIndexByte(link, ']')
	if i < 0 || j <= i {
		return 0
	}
	n, err := strconv.ParseInt(link[i+1:j], 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// inContainerByPidNS 汇总两条"隔离事实"判据：NSpid 个数 ≥2，或 PID 命名空间
// inode 不是初始值。任一命中即在容器里；两条都拿不到证据时返回 false，
// 由调用方继续走宿主机指纹（那是"证据不足"时的无奈之举，不是误报来源）。
func inContainerByPidNS(nspid int, link string) bool {
	if nspid > 1 {
		return true
	}
	if ino := pidNSIno(link); ino != 0 && ino != initialPidNSIno {
		return true
	}
	return false
}
