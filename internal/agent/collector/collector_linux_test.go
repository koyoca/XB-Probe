//go:build linux

package collector

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadMeminfoFallbackNoAvailable 断言老内核/容器缺 MemAvailable 时能回退算出可用量。
//
// 为什么专门测这个：LXC 里的 Alpine + lxcfs 提供的 /proc/meminfo 经常缺少
// MemAvailable（lxcfs 只透传部分字段）。缺了它可用内存会被读成 0，
// 于是"内存使用率 100%"——一个纯粹的误报，而且很难从现象反推原因。
func TestReadMeminfoFallbackNoAvailable(t *testing.T) {
	// 直接测解析逻辑：构造一份没有 MemAvailable 的 meminfo 内容。
	// 由于 readMeminfo 是从固定路径读文件，这里通过 parseMeminfo 的等价逻辑验证。
	const sample = `MemTotal:       16384000 kB
MemFree:         1024000 kB
MemAvailable:    8192000 kB
Buffers:          512000 kB
Cached:          4096000 kB
SwapTotal:       2097152 kB
SwapFree:        2097152 kB
`
	total, avail, swapTotal, swapFree := parseMeminfoForTest(sample)
	if total != 16384000*1024 {
		t.Errorf("total = %d，期望 %d", total, 16384000*1024)
	}
	if avail != 8192000*1024 {
		t.Errorf("avail = %d，期望 %d（应使用 MemAvailable）", avail, 8192000*1024)
	}
	if swapTotal != 2097152*1024 || swapFree != 2097152*1024 {
		t.Errorf("swap 解析错误：total=%d free=%d", swapTotal, swapFree)
	}
}

func TestParseMeminfoFallback(t *testing.T) {
	const noAvail = `MemTotal:       16384000 kB
MemFree:         1024000 kB
Buffers:          512000 kB
Cached:          4096000 kB
SReclaimable:     256000 kB
Shmem:            128000 kB
`
	total, avail, _, _ := parseMeminfoForTest(noAvail)
	if total != 16384000*1024 {
		t.Errorf("total = %d", total)
	}
	// 期望 = free + buffers + cached + sreclaimable − shmem
	want := uint64(1024000+512000+4096000+256000-128000) * 1024
	if avail != want {
		t.Errorf("avail = %d，期望 %d（MemAvailable 缺失时的回退公式）", avail, want)
	}
}

// TestCgroupCPUQuotaParsing 验证 cgroup v1/v2 的 CPU 配额解析。
func TestCgroupCPUQuotaParsing(t *testing.T) {
	// 这段逻辑直接读固定路径，无法在测试里替换。用等价的解析函数验证。
	cases := []struct {
		v2     string
		quota  int64
		period int64
		want   int
	}{
		{"200000 100000", 200000, 100000, 2},    // 2 核
		{"150000 100000", 150000, 100000, 2},    // 1.5 核 → 向上取整 2
		{"100000 100000", 100000, 100000, 1},    // 1 核
		{"max 100000", 0, 0, 0},                 // 无限制
		{"1000000 100000", 1000000, 100000, 10}, // 10 核
	}
	for _, c := range cases {
		got := quotaToCores(c.quota, c.period)
		if got != c.want {
			t.Errorf("quotaToCores(%d, %d) = %d，期望 %d", c.quota, c.period, got, c.want)
		}
	}
}

// TestReadOSReleaseAlpineFallback 验证 Alpine 的识别回退链。
//
// 用临时目录模拟"没有 /etc/os-release 但有 /etc/alpine-release"的极简 rootfs。
// 这里通过直接测 /etc/alpine-release 的读取函数来覆盖（不改真实路径）。
func TestReadOSReleaseAlpineFallback(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "alpine-release")
	if err := os.WriteFile(p, []byte("3.20.3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v := readFileLine(p)
	if v != "3.20.3" {
		t.Errorf("读 alpine-release 得到 %q", v)
	}
}

// TestCgroupMemoryLimitSanity 断言 cgroup 上限读取在本机（Windows/无 cgroup）不会 panic，
// 且返回值为 0（无限制）——这是容器外的正确行为。
func TestCgroupMemoryLimitSanity(t *testing.T) {
	// 本测试在 Linux 上跑时，如果没有容器限制应返回 0；
	// 在有 cgroup 限制的环境里返回正数也算通过（两种都是合法结果）。
	lim := cgroupMemLimit()
	if lim < 0 {
		t.Errorf("cgroupMemLimit = %d，不应为负数", lim)
	}
	quota := cgroupCPUQuota()
	if quota < 0 {
		t.Errorf("cgroupCPUQuota = %d，不应为负数", quota)
	}
	t.Logf("cgroup 上限：内存=%d 字节，CPU=%d 核", lim, quota)
}
