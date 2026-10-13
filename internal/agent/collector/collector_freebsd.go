//go:build freebsd

package collector

import (
	"encoding/binary"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/koyoca/xb-probe/internal/model"
)

// FreeBSD 采集实现（12.5）。
//
// 全部走 sysctl + 路由表 syscall，不依赖外部命令（无 `top`/`netstat`/`vmstat`）：
//   - CPU      ：kern.cp_times（每核 jiffies 数组，两次差值算使用率）
//   - 负载     ：vm.loadavg（struct loadavg：3 个定点数 + 刻度）
//   - 内存     ：hw.physmem / vm.stats.vm.v_free_count / v_inactive_count / v_cache_count
//   - 交换区   ：vm.swap_total（字节）/ vm.stats.vm.v_swappgsin… 这里用 swap_total 与现有交换用量
//   - 网卡     ：NET_RT_IFLIST（PF_ROUTE）+ RTM_IFINFO 里的 64 位 if_data 计数
//   - 磁盘     ：unix.Statfs（Blocks/Bavail/Bsize）
//   - 开机时间 ：kern.boottime
//
// macOS 不在支持范围内（只做 Windows + FreeBSD + Linux），故未实现 darwin 分支。

// New 返回 FreeBSD 采集器。
func New(opts Options) Collector {
	return &freebsdCollector{
		opts:  opts,
		rates: map[string]*rateCalculator{},
	}
}

type freebsdCollector struct {
	opts Options

	mu       sync.Mutex
	rates    map[string]*rateCalculator
	prevCPUs []uint64 // 每核 jiffies（user/nice/sys/intr/idle）扁平化后的快照
	primed   bool
}

// FreeBSD CPU 状态数组的每核字段顺序（sys/sys/resource.h 的 CPUSTATES = 5）：
// 0=CP_USER 1=CP_NICE 2=CP_SYS 3=CP_INTR 4=CP_IDLE
const freebsdCPUStates = 5

// Static 读取静态信息。
func (c *freebsdCollector) Static() (*model.Static, error) {
	hostname, _ := os.Hostname()

	memTotal := sysctlU64("hw.physmem", 0)
	// hw.physmem 是 "可供内核使用的物理内存"，需要加上被内核预留给缓冲的部分
	// （hw.physmem 在某些套件里略小于插卡容量）；这里以 hw.realmem 优先。
	if realm := sysctlU64("hw.realmem", 0); realm > memTotal {
		memTotal = realm
	}

	cpuModel := trimField(sysctlString("hw.model"), 64)
	cores := int(sysctlU64("hw.ncpu", 1))
	if cores <= 0 {
		cores = runtime.NumCPU()
	}

	return &model.Static{
		Hostname:     hostname,
		OS:           freebsdOSName(),
		Kernel:       trimField(sysctlString("kern.osrelease"), 64),
		Arch:         runtime.GOARCH,
		CPUModel:     cpuModel,
		CPUCores:     cores,
		MemTotal:     int64(memTotal),
		SwapTotal:    int64(sysctlU64("vm.swap_total", 0)),
		DiskTotal:    freebsdDiskTotalOf(c.diskPaths()),
		AgentVersion: c.opts.AgentVersion,
		Virt:         freebsdVirt(),
	}, nil
}

// Sample 读取一个采样点。
func (c *freebsdCollector) Sample() (*model.Metrics, error) {
	now := time.Now()
	m := &model.Metrics{}

	if pct, ok := c.cpuPercent(); ok {
		v := pct
		m.CPU = &v
	}
	if load, ok := freebsdLoad1(); ok {
		m.Load1 = &load
	}

	// 内存：已用 = 物理内存 − 空闲 − 非活跃 − 缓存（与 Linux MemAvailable 口径对齐）。
	if total := sysctlU64("hw.physmem", 0); total > 0 {
		free := sysctlU64("vm.stats.vm.v_free_count", 0)
		inactive := sysctlU64("vm.stats.vm.v_inactive_count", 0)
		cache := sysctlU64("vm.stats.vm.v_cache_count", 0)
		pageSize := sysctlU64("vm.stats.vm.v_page_size", 4096)
		avail := (free + inactive + cache) * pageSize
		if avail > total {
			avail = total
		}
		used := int64(total - avail)
		m.MemUsed = &used

		// 交换用量：vm.swap_total 是总量，已用需要从 swapinfo 的 kinfo_swap 拿；
		// sysctl 没有直接的"已用"值，因此按下式估算：
		// 已用 = 总量 − (swap 空闲页 × pageSize)。FreeBSD 14 起提供 vm.swap_total 与
		// vm.stats.vm.v_swap… 但后者是分页事件计数而非剩余量，取不到就留空，
		// 让 Master 显示 "—"（"没测到"不该渲染成 0）。
		if st := sysctlU64("vm.swap_total", 0); st > 0 {
			if usedSwap, ok := freebsdSwapUsed(st); ok {
				m.SwapUsed = &usedSwap
			}
		}
	}

	used := freebsdDiskUsedOf(c.diskPaths())
	m.DiskUsed = &used

	nics, rxTotal, txTotal := c.readNICs()
	m.NICs = nics
	rx, tx := c.sumRates(rxTotal, txTotal, now)
	m.RxBps = &rx
	m.TxBps = &tx

	if boot, ok := freebsdBootTime(); ok {
		m.BootTime = &boot
	}

	return m, nil
}

// diskPaths 返回要统计的挂载点；FreeBSD 的根是 "/"，与 Linux 一致。
func (c *freebsdCollector) diskPaths() []string {
	return c.opts.diskPathsOr([]string{"/"})
}

func (c *freebsdCollector) sumRates(rx, tx int64, at time.Time) (int64, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.rates["__total__"]
	if !ok {
		r = &rateCalculator{}
		c.rates["__total__"] = r
	}
	return r.rate(rx, tx, at)
}

// cpuPercent 用 kern.cp_times 两次差值计算总体使用率（×100）。
//
// 与 Linux 口径一致：idle = CP_IDLE；busy = 总 jiffies − idle。
// 注意 CP_INTR（中断处理）算作忙碌时间，这一点与 Linux 的 softirq 处理方式一致。
func (c *freebsdCollector) cpuPercent() (int, bool) {
	cur, ok := readCPTimes()
	if !ok || len(cur) < freebsdCPUStates {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.primed {
		c.prevCPUs, c.primed = cur, true
		return 0, false
	}
	if len(c.prevCPUs) != len(cur) {
		c.prevCPUs = cur
		return 0, false
	}
	var dTotal, dIdle float64
	for i := 0; i+freebsdCPUStates <= len(cur); i += freebsdCPUStates {
		for s := 0; s < freebsdCPUStates; s++ {
			d := float64(cur[i+s]) - float64(c.prevCPUs[i+s])
			if d < 0 {
				d = float64(cur[i+s]) // 计数器重置
			}
			dTotal += d
			if s == 4 { // CP_IDLE
				dIdle += d
			}
		}
	}
	c.prevCPUs = cur
	if dTotal <= 0 {
		return 0, false
	}
	used := (dTotal - dIdle) / dTotal * 10000
	if used < 0 {
		used = 0
	}
	if used > 10000 {
		used = 10000
	}
	return int(used), true
}

// ---------------------------------------------------------------------------
// 网卡：NET_RT_IFLIST
// ---------------------------------------------------------------------------

// readNICs 用 NET_RT_IFLIST 取回所有接口的 64 位累计计数。
//
// if_data 在 FreeBSD 12+ 是 64 位计数器（Ibytes/Obytes），不会 32 位回绕，
// 与 Master 侧的"按网卡求差"口径天然匹配。
func (c *freebsdCollector) readNICs() (map[string]model.NICCounters, int64, int64) {
	include := c.opts.NICInclude
	exclude := c.opts.NICExclude
	if len(exclude) == 0 && len(include) == 0 {
		exclude = DefaultNICExclude()
	}
	// FreeBSD 上常见的虚拟接口命名补充：bridge/epair/ng/tap/gif/stf。
	exclude = append(exclude, "bridge*", "epair*", "ng*", "gif*", "stf*", "pflog*", "enc*")

	out := map[string]model.NICCounters{}
	var rxTotal, txTotal int64

	entries, err := freebsdIfData()
	if err != nil {
		return out, 0, 0
	}
	for name, d := range entries {
		if !nicAllowed(name, include, exclude) {
			continue
		}
		rx := int64(d.Ibytes)
		tx := int64(d.Obytes)
		out[name] = model.NICCounters{Rx: rx, Tx: tx}
		rxTotal += rx
		txTotal += tx
	}
	return out, rxTotal, txTotal
}

// freebsdIfData 解析 NET_RT_IFLIST 返回的路由消息流，提取每个接口的 if_data。
func freebsdIfData() (map[string]unix.IfData, error) {
	mib := []int32{unix.CTL_NET, unix.AF_ROUTE, 0, 0, unix.NET_RT_IFLIST, 0}
	buf, err := sysctlMibRaw(mib)
	if err != nil {
		return nil, err
	}

	out := map[string]unix.IfData{}
	for off := 0; off+4 <= len(buf); {
		msglen := int(binary.LittleEndian.Uint16(buf[off:]))
		if msglen < 4 || off+msglen > len(buf) {
			break
		}
		msgType := buf[off+3]
		if msgType == unix.RTM_IFINFO && msglen >= 4+int(unsafe.Sizeof(unix.IfMsghdr{})) {
			hdr := (*unix.IfMsghdr)(unsafe.Pointer(&buf[off]))
			name := ifNameFromMsghdr(buf[off:off+msglen], int(hdr.Addrs))
			if name != "" {
				// IfMsghdr.Data 在 FreeBSD 上布局与 unix.IfData 一致（见 ztypes 注释）。
				out[name] = unix.IfData(hdr.Data)
			}
		}
		off += msglen
	}
	return out, nil
}

// ifNameFromMsghdr 从 if_msghdr 之后的 sockaddrs 里取出接口名（sdl_data）。
//
// RTM_IFINFO 的 Addrs 计数标明后面跟了几个 sockaddr；实际接口名在第一个
// sockaddr_dl 的 sdl_data 里。这里做防御性解析：长度不足就返回空串。
func ifNameFromMsghdr(msg []byte, addrs int) string {
	if addrs <= 0 {
		return ""
	}
	base := int(unsafe.Sizeof(unix.IfMsghdr{}))
	if base+2 > len(msg) {
		return ""
	}
	sa := msg[base:]
	saLen := int(saLenOf(sa))
	if saLen <= 0 || saLen > len(sa) {
		return ""
	}
	// sockaddr_dl 布局：{ len uint8; family uint8; index uint16; type uint8;
	//                      nlen uint8; alen uint8; slen uint8; data [...] }
	if len(sa) < 8 {
		return ""
	}
	nlen := int(sa[5])
	if nlen == 0 || 8+nlen > len(sa) {
		return ""
	}
	return string(sa[8 : 8+nlen])
}

// saLenOf 读取 sockaddr 的 sa_len（第一个字节；FreeBSD 用 BSD sockaddr 布局）。
func saLenOf(sa []byte) uint8 {
	if len(sa) < 1 {
		return 0
	}
	return sa[0]
}

// ---------------------------------------------------------------------------
// sysctl 读取
// ---------------------------------------------------------------------------

func sysctlU64(name string, def uint64) uint64 {
	v, err := unix.SysctlUint64(name)
	if err != nil {
		return def
	}
	return v
}

func sysctlString(name string) string {
	v, err := unix.Sysctl(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// sysctlMibRaw 按 MIB 数组直接读 sysctl（SysctlRaw 只接受名字）。
func sysctlMibRaw(mib []int32) ([]byte, error) {
	// 先取长度。
	n := uintptr(0)
	if _, _, errno := unix.Syscall6(unix.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		0, uintptr(unsafe.Pointer(&n)), 0, 0); errno != 0 {
		return nil, errno
	}
	if n == 0 {
		return nil, nil
	}
	// 留一点余量：接口可能在两次调用之间增加。
	buf := make([]byte, n+n/8+512)
	nn := uintptr(len(buf))
	if _, _, errno := unix.Syscall6(unix.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&nn)), 0, 0); errno != 0 {
		return nil, errno
	}
	return buf[:nn], nil
}

// readCPTimes 读 kern.cp_times：每核 CPUSTATES 个 uint64（jiffies）。
func readCPTimes() ([]uint64, bool) {
	raw, err := unix.SysctlRaw("kern.cp_times")
	if err != nil || len(raw) < 8 {
		return nil, false
	}
	n := len(raw) / 8
	out := make([]uint64, n)
	for i := 0; i < n; i++ {
		out[i] = binary.LittleEndian.Uint64(raw[i*8:])
	}
	return out, true
}

// freebsdLoad1 读 vm.loadavg（struct loadavg: 3 × fixed-point int32 + scale）。
func freebsdLoad1() (int, bool) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 16 {
		return 0, false
	}
	// struct loadavg { fixpt_t ldavg[3]; long fscale; }，fixpt_t 是 uint32。
	l0 := binary.LittleEndian.Uint32(raw[0:])
	scale := binary.LittleEndian.Uint32(raw[12:])
	if scale == 0 {
		return 0, false
	}
	return int(float64(l0) / float64(scale) * 100), true
}

// freebsdBootTime 读 kern.boottime（struct timeval）。
//
// 注意 tv.Sec 在 32 位 FreeBSD（386/arm）上是 int32，直接 `return tv.Sec` 会编译失败——
// 必须显式转 int64 才能跨架构通过（64 位下 int64→int64 是恒等转换，无副作用）。
func freebsdBootTime() (int64, bool) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil || tv == nil {
		return 0, false
	}
	return int64(tv.Sec), true
}

// freebsdSwapUsed 估算已用交换量。
//
// FreeBSD 没有"剩余交换页数"的 sysctl，但 vm.swap_total 给了总字节数；
// 已用 = 总量 − swap_pager 的空闲页 × pageSize。取不到空闲页时返回 false，
// 让上层留空（而不是伪造 0）。
func freebsdSwapUsed(total uint64) (int64, bool) {
	free := sysctlU64("vm.swap_free_pages", 0)
	if free == 0 {
		return 0, false
	}
	pageSize := sysctlU64("vm.stats.vm.v_page_size", 4096)
	avail := free * pageSize
	if avail > total {
		avail = total
	}
	return int64(total - avail), true
}

// freebsdOSName 从 /etc/os-release 读发行版名，回落 "FreeBSD"。
func freebsdOSName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				return trimField(strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`), 64)
			}
		}
	}
	// 退而求其次：kernel 的产物名（pfSense / OPNsense 等套件会改这里）。
	if v := sysctlString("kern.ident"); v != "" {
		return trimField("FreeBSD "+v, 64)
	}
	return "FreeBSD"
}

// freebsdVirt 判断虚拟化类型。
func freebsdVirt() string {
	if v := sysctlString("kern.vm_guest"); v != "" && v != "none" {
		return v // bhyve / kvm / vmware / xen / hyperv / virtualbox
	}
	if sysctlU64("kern.hv_vmm_present", 0) == 1 {
		return "hypervisor"
	}
	// Jail / VNET 容器：没有真正的虚拟化，但也不是裸机。
	if v := sysctlString("security.jail.jailed"); v == "1" {
		return "jail"
	}
	return "none"
}

// ---------------------------------------------------------------------------
// 磁盘
// ---------------------------------------------------------------------------

func freebsdDiskUsedOf(paths []string) int64 {
	var used int64
	for _, p := range paths {
		var st unix.Statfs_t
		if err := unix.Statfs(p, &st); err != nil {
			continue
		}
		total := int64(st.Blocks * st.Bsize)
		avail := int64(st.Bavail) * int64(st.Bsize)
		if total > avail {
			used += total - avail
		}
	}
	return used
}

func freebsdDiskTotalOf(paths []string) int64 {
	var total int64
	for _, p := range paths {
		var st unix.Statfs_t
		if err := unix.Statfs(p, &st); err != nil {
			continue
		}
		total += int64(st.Blocks * st.Bsize)
	}
	return total
}

// 保证 sort / syscall 被使用（接口名排序在排查时用得到，保留确定性输出）。
var (
	_ = sort.Strings
	_ = syscall.Statfs
)
