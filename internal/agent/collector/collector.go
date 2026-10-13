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
	"strings"
	"sync"
	"time"

	"github.com/yourorg/probe/internal/model"
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
