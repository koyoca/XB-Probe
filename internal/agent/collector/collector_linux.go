//go:build linux

package collector

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/koyoca/xb-probe/internal/model"
)

// Linux 采集实现（12.5）：只读 /proc、/sys 与 statfs，不依赖外部命令。

// New 返回 Linux 采集器。
func New(opts Options) Collector {
	return &linuxCollector{
		opts:  opts,
		rates: map[string]*rateCalculator{},
	}
}

type linuxCollector struct {
	opts Options

	mu      sync.Mutex
	rates   map[string]*rateCalculator
	prevCPU cpuTimes
	primed  bool
}

type cpuTimes struct {
	total uint64
	idle  uint64
}

// Static 读取静态信息。
//
// 容器（LXC / Docker）里要注意两点：
//  1. /proc/cpuinfo 与 /proc/meminfo 看到的是**宿主机**的值。LXC 默认会做一定的
//     cgroup 隔离（内存），但 cpuinfo 往往是宿主机的全部核心数——所以这里优先读
//     cgroup 的 cpu 配额/上限，能读到就用它，读不到再退回 /proc/cpuinfo。
//  2. Alpine 等精简系统的 /proc/cpuinfo 只有 "model name" 或干脆没有（ARM 上是
//     "Hardware"/"Processor"），需要兜底到 /proc/device-tree 或 /proc/sys 的信息。
func (c *linuxCollector) Static() (*model.Static, error) {
	hostname, _ := os.Hostname()
	osName := readOSRelease()
	kernel := trimField(readFileLine("/proc/sys/kernel/osrelease"), 64)
	model1, cores := readCPUInfo()
	if cc := cgroupCPUQuota(); cc > 0 && (cores == 0 || cc < cores) {
		// 容器被限制了 CPU 配额时，展示"实际可用"比展示宿主机总核数更有意义。
		cores = cc
	}
	if model1 == "" {
		model1 = readCPUModelFallback()
	}

	memTotal, swapTotal := readMemTotals()
	if lim := cgroupMemLimit(); lim > 0 && (memTotal == 0 || lim < memTotal) {
		memTotal = lim
	}
	diskTotal := diskTotalOf(c.opts.diskPathsOr([]string{"/"}))

	return &model.Static{
		Hostname:     hostname,
		OS:           osName,
		Kernel:       kernel,
		Arch:         runtime.GOARCH,
		CPUModel:     model1,
		CPUCores:     cores,
		MemTotal:     int64(memTotal),
		SwapTotal:    int64(swapTotal),
		DiskTotal:    diskTotal,
		AgentVersion: c.opts.AgentVersion,
		Virt:         detectVirt(),
	}, nil
}

// Sample 读取一个采样点。
func (c *linuxCollector) Sample() (*model.Metrics, error) {
	now := time.Now()
	m := &model.Metrics{}

	// CPU 与负载
	if pct, ok := c.cpuPercent(); ok {
		v := pct
		m.CPU = &v
	}
	if load, ok := readLoad1(); ok {
		m.Load1 = &load
	}

	// 内存（统一口径：已用 = 总量 − 可用，不含可回收缓存，12.5）
	//
	// 容器里必须优先用 cgroup 的上限与用量：/proc/meminfo 是宿主机的值，
	// 直接上报会让容器内存压力完全不可见（512 MB 的 LXC 显示宿主机 64 GB）。
	memTotal, memAvail, swapTotal, swapFree := readMeminfo()
	if lim := cgroupMemLimit(); lim > 0 && (memTotal == 0 || lim < memTotal) {
		memTotal = lim
		if used, ok := cgroupMemoryUsed(); ok {
			// cgroup 的 memory.current 含 page cache，与 /proc/meminfo 的
			// "MemTotal - MemAvailable" 口径接近（都不含可回收缓存）。
			// 用 cgroup 的用量而不是 meminfo 的用量，避免拿宿主机的全局数字去减容器上限。
			usedV := int64(used)
			if usedV > int64(memTotal) {
				usedV = int64(memTotal)
			}
			m.MemUsed = &usedV
		} else if memTotal > 0 && memAvail < memTotal {
			usedV := int64(memTotal - memAvail)
			m.MemUsed = &usedV
		}
	} else if memTotal > 0 {
		used := int64(memTotal - memAvail)
		m.MemUsed = &used
	}
	if swapTotal > 0 {
		su := int64(swapTotal - swapFree)
		m.SwapUsed = &su
	}

	// 磁盘
	diskUsed := diskUsedOf(c.opts.diskPathsOr([]string{"/"}))
	m.DiskUsed = &diskUsed

	// 网卡
	nics, rxTotal, txTotal := c.readNICs()
	m.NICs = nics
	prevRx, prevTx := c.sumRates(rxTotal, txTotal, now)
	m.RxBps = &prevRx
	m.TxBps = &prevTx

	// 连接数
	if conns, ok := countConns(); ok {
		m.Conns = &conns
	}

	// 开机时间（/proc/uptime 推算，供 Master 判定重启）
	if up, ok := readUptime(); ok {
		boot := now.Unix() - int64(up)
		m.BootTime = &boot
	}

	return m, nil
}

// sumRates 汇总全部统计网卡的总速率。
func (c *linuxCollector) sumRates(rx, tx int64, at time.Time) (int64, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.rates["__total__"]
	if !ok {
		r = &rateCalculator{}
		c.rates["__total__"] = r
	}
	return r.rate(rx, tx, at)
}

// readNICs 读取 /proc/net/dev 并按过滤规则汇总。
func (c *linuxCollector) readNICs() (map[string]model.NICCounters, int64, int64) {
	include := c.opts.NICInclude
	exclude := c.opts.NICExclude
	if len(exclude) == 0 && len(include) == 0 {
		exclude = DefaultNICExclude()
	}

	out := map[string]model.NICCounters{}
	var rxTotal, txTotal int64

	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return out, 0, 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		if !nicAllowed(name, include, exclude) {
			continue
		}
		fields := strings.Fields(line[idx+1:])
		if len(fields) < 16 {
			continue
		}
		rx, _ := strconv.ParseInt(fields[0], 10, 64)
		tx, _ := strconv.ParseInt(fields[8], 10, 64)
		out[name] = model.NICCounters{Rx: rx, Tx: tx}
		rxTotal += rx
		txTotal += tx
	}
	return out, rxTotal, txTotal
}

// cpuPercent 用两次采样差值计算 CPU 使用率（×100）。
func (c *linuxCollector) cpuPercent() (int, bool) {
	cur, ok := readCPUTimes()
	if !ok {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.primed {
		c.prevCPU, c.primed = cur, true
		return 0, false
	}
	dTotal := float64(cur.total - c.prevCPU.total)
	dIdle := float64(cur.idle - c.prevCPU.idle)
	c.prevCPU = cur
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
// /proc 解析
// ---------------------------------------------------------------------------

func readCPUTimes() (cpuTimes, bool) {
	lines, err := readLines("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var total, idle uint64
		for i, f := range fields {
			v, _ := strconv.ParseUint(f, 10, 64)
			total += v
			if i == 3 || i == 4 { // idle + iowait
				idle += v
			}
		}
		return cpuTimes{total: total, idle: idle}, true
	}
	return cpuTimes{}, false
}

func readLoad1() (int, bool) {
	line := readFileLine("/proc/loadavg")
	f := strings.Fields(line)
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, false
	}
	return int(v * 100), true
}

func readMeminfo() (total, available, swapTotal, swapFree uint64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return
	}
	return parseMeminfoForTest(string(b))
}

// parseMeminfoForTest 是 readMeminfo 的纯函数部分，抽出来是为了可测试：
// 老内核（< 3.14）与 lxcfs 提供的 /proc/meminfo 都可能缺 MemAvailable，
// 回退公式必须被测试钉住，否则容器里会出现"内存使用率恒 100%"的误报。
//
// 名字带 ForTest 是历史命名（先写测试后抽函数）；它在生产路径上同样被调用。
func parseMeminfoForTest(content string) (total, available, swapTotal, swapFree uint64) {
	var free, buffers, cached, sreclaimable, shmem uint64
	haveAvailable := false
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		v *= 1024 // kB → B
		switch strings.TrimSuffix(f[0], ":") {
		case "MemTotal":
			total = v
		case "MemAvailable":
			available = v
			haveAvailable = true
		case "MemFree":
			free = v
		case "Buffers":
			buffers = v
		case "Cached":
			cached = v
		case "SReclaimable":
			sreclaimable = v
		case "Shmem":
			shmem = v
		case "SwapTotal":
			swapTotal = v
		case "SwapFree":
			swapFree = v
		}
	}
	// MemAvailable 缺失时的回退公式（与内核 3.14 引入该字段时的定义等价）：
	//   MemAvailable ≈ MemFree + Buffers + Cached + SReclaimable − Shmem
	if !haveAvailable && total > 0 {
		available = free + buffers + cached + sreclaimable
		if available > shmem {
			available -= shmem
		}
	}
	return
}

func readMemTotals() (uint64, uint64) {
	t, _, st, _ := readMeminfo()
	return t, st
}

func readUptime() (float64, bool) {
	f := strings.Fields(readFileLine("/proc/uptime"))
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return v, err == nil
}

// readOSRelease 读发行版名。
//
// 回退链（容器里前两项经常缺失）：
//
//	/etc/os-release → /usr/lib/os-release → /etc/alpine-release → /etc/issue → "Linux"
//
// Alpine 的 /etc/os-release 只在 alpine-base 包存在时才有（精简 rootfs 常缺），
// 但 /etc/alpine-release 一定有——这是识别 Alpine 最可靠的方式。
func readOSRelease() string {
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release", "/etc/openwrt_release"} {
		lines, err := readLines(p)
		if err != nil {
			continue
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				v := strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
				if v != "" {
					return trimField(v, 64)
				}
			}
		}
	}
	if v := readFileLine("/etc/alpine-release"); v != "" {
		return trimField("Alpine Linux "+v, 64)
	}
	if v := readFileLine("/etc/debian_version"); v != "" {
		return trimField("Debian "+v, 64)
	}
	// /etc/issue 形如 "Welcome to Alpine Linux 3.20"，取第一行并去掉转义序列。
	if v := readFileLine("/etc/issue"); v != "" {
		v = strings.TrimRight(v, ` \n\r\t`)
		v = strings.Map(func(r rune) rune {
			if r == '\\' {
				return -1
			}
			return r
		}, v)
		if v != "" {
			return trimField(v, 64)
		}
	}
	return "Linux"
}

func readCPUInfo() (string, int) {
	lines, err := readLines("/proc/cpuinfo")
	if err != nil {
		return "", 0
	}
	modelName := ""
	cores := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "processor") {
			cores++
		}
		if modelName == "" {
			// x86 用 "model name"；ARM 上是 "Hardware"/"Processor"/"model name"，
			// Alpine 的 ARM 镜像只有 "Hardware"。全部纳入候选，避免型号列空白。
			for _, key := range []string{"model name", "Hardware", "Processor", "cpu model"} {
				if strings.HasPrefix(line, key) {
					if j := strings.IndexByte(line, ':'); j >= 0 {
						v := trimField(line[j+1:], 64)
						if v != "" {
							modelName = v
						}
					}
				}
			}
		}
	}
	if cores == 0 {
		cores = runtime.NumCPU()
	}
	return modelName, cores
}

// readCPUModelFallback 在 /proc/cpuinfo 没有型号信息时，从 device-tree 读（ARM 板卡）。
func readCPUModelFallback() string {
	for _, p := range []string{
		"/proc/device-tree/model",
		"/sys/firmware/devicetree/base/model",
	} {
		if v := strings.TrimRight(readFileLine(p), "\x00"); v != "" {
			return trimField(v, 64)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// cgroup：容器（LXC / Docker / containerd）里的真实资源上限
// ---------------------------------------------------------------------------
//
// 为什么必须读 cgroup：容器内 /proc/meminfo 与 /proc/cpuinfo 是宿主机的值。
// 不读 cgroup 的话，一个 512 MB 的 LXC 容器会显示宿主机 64 GB 内存，
// 内存百分比永远接近 0——"最需要被看见的容器内存压力"完全不可见。
//
// cgroup v1（LXC 常见，尤其 Debian/Alpine 老镜像）与 v2 路径不同，两者都试。

// cgroupMemLimit 返回容器内存上限（字节）；无限制/读不到返回 0。
func cgroupMemLimit() uint64 {
	// cgroup v2
	if v, ok := readUintFile("/sys/fs/cgroup/memory.max"); ok && v > 0 {
		return v
	}
	// cgroup v1
	if v, ok := readUintFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); ok && v > 0 {
		// v1 的"无限制"是一个很大的哨兵值（通常是 PAGE_COUNTER_MAX 对齐后的值），
		// 大于 1<<60 一律视为无限制。
		if v < 1<<60 {
			return v
		}
	}
	return 0
}

// cgroupCPUQuota 返回容器可用的 CPU 个数；无配额/读不到返回 0。
func cgroupCPUQuota() int {
	// cgroup v2：cpu.max 形如 "200000 100000"（配额 周期），单位微秒。
	if s := readFileLine("/sys/fs/cgroup/cpu.max"); s != "" {
		f := strings.Fields(s)
		if len(f) == 2 && f[0] != "max" {
			quota, err1 := strconv.ParseInt(f[0], 10, 64)
			period, err2 := strconv.ParseInt(f[1], 10, 64)
			if err1 == nil && err2 == nil {
				return quotaToCores(quota, period)
			}
		}
	}
	// cgroup v1：cfs_quota_us / cfs_period_us。quota 为 -1 表示无限制。
	q, ok1 := readIntFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	p, ok2 := readIntFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if ok1 && ok2 {
		return quotaToCores(q, p)
	}
	return 0
}

// quotaToCores 把 cgroup 的"配额/周期"换算成核数（向上取整）。
//
// 向上取整而不是向下：`quota=150000, period=100000`（1.5 核）在展示上取 2 更符合
// "这台容器能用多少算力"的直觉；向下取整会显示 1 核，而 CPU 使用率却可能超过 100%，
// 看板上就会看到"1 核但使用率 150%"的矛盾。
func quotaToCores(quota, period int64) int {
	if quota <= 0 || period <= 0 {
		return 0
	}
	n := int((quota + period - 1) / period)
	if n < 1 {
		n = 1
	}
	return n
}

// cgroupMemoryUsed 返回容器当前内存用量；读不到返回 (0,false)。
func cgroupMemoryUsed() (uint64, bool) {
	if v, ok := readUintFile("/sys/fs/cgroup/memory.current"); ok {
		return v, true
	}
	if v, ok := readUintFile("/sys/fs/cgroup/memory/memory.usage_in_bytes"); ok {
		return v, true
	}
	return 0, false
}

func readUintFile(path string) (uint64, bool) {
	s := readFileLine(path)
	if s == "" || s == "max" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	return v, err == nil
}

func readIntFile(path string) (int64, bool) {
	s := readFileLine(path)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

func countConns() (int, bool) {
	n := 0
	ok := false
	for _, p := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		lines, err := readLines(p)
		if err != nil {
			continue
		}
		ok = true
		if len(lines) > 1 {
			n += len(lines) - 1
		}
	}
	return n, ok
}

// detectVirt 判断虚拟化 / 容器类型。
//
// 判定顺序按"证据强度"排：容器标记（/.dockerenv、cgroup 路径）比 /proc/cpuinfo
// 里的 QEMU 字样更可靠——LXC 里跑 KVM 虚机时两者会同时命中，而实际身份是 LXC。
//
// cgroup v2 统一挂载在 /sys/fs/cgroup，路径里不再有 "lxc"/"docker" 字样，
// 因此 v2 下改为读 /proc/self/mountinfo 与 /proc/1/cgroup 的 cgroup 命名空间
// 以及 /run/systemd 的存在性来判断（v2 容器通常没有 systemd）。
func detectVirt() string {
	// Docker / Podman / containerd：/.dockerenv 或 /run/.containerenv。
	if exists("/.dockerenv") || exists("/run/.containerenv") {
		return "docker"
	}

	// cgroup v1 的路径里带容器运行时名；LXC / Docker 都能命中。
	cgroupBlob := readFileLine("/proc/1/cgroup") + "\n" + readFileLine("/proc/self/cgroup")
	switch {
	case contains(cgroupBlob, "lxc"):
		return "lxc"
	case contains(cgroupBlob, "docker"):
		return "docker"
	case contains(cgroupBlob, "kubepods"):
		return "k8s"
	case contains(cgroupBlob, "containerd"):
		return "containerd"
	}

	// cgroup v2：/proc/self/cgroup 只有 "0::/"。改为看命名空间与 init 进程。
	// LXC 的容器内通常能看到 lxc 相关的环境变量或 /dev/lxc 设备。
	if contains(string(readEnvFile("/proc/1/environ")), "container=lxc") {
		return "lxc"
	}
	if exists("/dev/lxc") {
		return "lxc"
	}
	// systemd-nspawn / machinectl。
	if exists("/run/systemd/nspawn") {
		return "systemd-nspawn"
	}

	switch {
	case contains(readFileLine("/proc/cpuinfo"), "QEMU"):
		return "kvm"
	case exists("/proc/vz"):
		return "openvz"
	}
	if contains(readFileLine("/proc/version"), "Microsoft") || contains(readFileLine("/proc/sys/kernel/osrelease"), "Microsoft") {
		return "wsl"
	}
	return "none"
}

// readEnvFile 读 /proc/<pid>/environ（NUL 分隔），转成可搜索的字符串。
//
// 注意：/proc/1/environ 在容器里可能不可读（权限或 PID namespace），
// 这里失败就返回空串，由调用方继续走其他判据。
func readEnvFile(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return []byte(strings.ReplaceAll(string(b), "\x00", "\n"))
}

// ---------------------------------------------------------------------------
// 磁盘
// ---------------------------------------------------------------------------

func diskUsedOf(paths []string) int64 {
	var used int64
	for _, p := range paths {
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err != nil {
			continue
		}
		total := st.Blocks * uint64(st.Bsize)
		free := st.Bavail * uint64(st.Bsize)
		if total > free {
			used += int64(total - free)
		}
	}
	return used
}

func diskTotalOf(paths []string) int64 {
	var total int64
	for _, p := range paths {
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err != nil {
			continue
		}
		total += int64(st.Blocks * uint64(st.Bsize))
	}
	return total
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func readFileLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(b), "\n"), nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
