//go:build windows

package collector

import (
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/yourorg/probe/internal/model"
)

// Windows 采集实现（12.5）。
//
// 全部走 Win32 API，不依赖外部命令（守住 RSS ≤ 10 MB、无运行时依赖）：
//   - CPU        ：GetSystemTimes（kernel/user/idle 三个 FILETIME，两次差值算使用率）
//   - 内存       ：GlobalMemoryStatusEx（ullTotalPhys / ullAvailPhys / ullTotalPageFile）
//   - 网卡累计量 ：GetIfEntry2Ex（MIB_IF_ROW2，InOctets/OutOctets 是 64 位累计计数）
//   - 磁盘       ：GetDiskFreeSpaceEx
//   - 开机时间   ：GetTickCount64
//
// Windows 没有 POSIX 负载均值，`load1` 上报空值（模型里也是可空字段）；
// 连接数不做统计（需要 GetExtendedTcpTable 且与 Linux 口径不可比）。

// New 返回 Windows 采集器（build tag 由文件名保证，与 collector_linux.go / stub 互斥）。
func New(opts Options) Collector {
	return &windowsCollector{
		opts:  opts,
		rates: map[string]*rateCalculator{},
	}
}

type windowsCollector struct {
	opts Options

	mu      sync.Mutex
	rates   map[string]*rateCalculator
	prevCPU winCPUTimes
	primed  bool
}

type winCPUTimes struct {
	idle, kernel, user uint64
}

// Windows 默认只统计物理网卡所在的盘；纯 Windows 常见盘符为 C:。
//
// 注意：这里刻意不用 `filepath.VolumeName` 之类的推断——Options.DiskPaths 为空时
// 直接回落 "C:\\" 是最符合直觉的选择（系统盘），与 Linux 侧回落 "/" 语义一致。
func (c *windowsCollector) diskPaths() []string {
	return c.opts.diskPathsOr([]string{`C:\`})
}

// Static 读取静态信息。
func (c *windowsCollector) Static() (*model.Static, error) {
	hostname, _ := os.Hostname()

	var mem winMemStatus
	memTotal := int64(0)
	swapTotal := int64(0)
	if queryMemory(&mem) {
		memTotal = int64(mem.TotalPhys)
		swapTotal = winSwapTotal(mem)
	}

	cpuModel, cores := windowsCPUInfo()
	if cores == 0 {
		cores = runtime.NumCPU()
	}

	return &model.Static{
		Hostname:     hostname,
		OS:           windowsOSName(),
		Kernel:       trimField(windowsBuildNumber(), 64),
		Arch:         runtime.GOARCH,
		CPUModel:     cpuModel,
		CPUCores:     cores,
		MemTotal:     memTotal,
		SwapTotal:    swapTotal,
		DiskTotal:    winDiskTotalOf(c.diskPaths()),
		AgentVersion: c.opts.AgentVersion,
		Virt:         windowsVirt(),
	}, nil
}

// Sample 读取一个采样点。
func (c *windowsCollector) Sample() (*model.Metrics, error) {
	now := time.Now()
	m := &model.Metrics{}

	// CPU：两次 GetSystemTimes 差值。首次采样没有基线，返回空值而不是 0——
	// 0 会被 Master 当成"测到了且为空闲"，与"没测到"是两回事。
	if pct, ok := c.cpuPercent(); ok {
		v := pct
		m.CPU = &v
	}
	// 负载：Windows 无 POSIX load average，刻意不填（保持 nil）。

	// 内存：已用 = 总量 − 可用（与 Linux 的 MemAvailable 口径一致）。
	var mem winMemStatus
	if queryMemory(&mem) && mem.TotalPhys > 0 {
		used := int64(mem.TotalPhys - mem.AvailPhys)
		m.MemUsed = &used
		// 交换区：**必须与 Static 里的 swapTotal 用同一个公式**，
		// 否则 Percent(used,total) 会因两者基数不同而恒被钳到 100%（见 winSwapTotal 注释）。
		if total := winSwapTotal(mem); total > 0 {
			su := winSwapUsed(mem)
			if su > total {
				su = total // 与分母同基数后仍越界就钳住，宁可少报也不能报 >100%
			}
			if su < 0 {
				su = 0
			}
			m.SwapUsed = &su
		}
	}

	used := winDiskUsedOf(c.diskPaths())
	m.DiskUsed = &used

	nics, rxTotal, txTotal := c.readNICs()
	m.NICs = nics
	rx, tx := c.sumRates(rxTotal, txTotal, now)
	m.RxBps = &rx
	m.TxBps = &tx

	// 开机时间：GetTickCount64 是自开机以来的毫秒数。
	if up, ok := uptimeSeconds(); ok {
		boot := now.Unix() - int64(up)
		m.BootTime = &boot
	}

	return m, nil
}

// sumRates 汇总全部统计网卡的总速率。
func (c *windowsCollector) sumRates(rx, tx int64, at time.Time) (int64, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.rates["__total__"]
	if !ok {
		r = &rateCalculator{}
		c.rates["__total__"] = r
	}
	return r.rate(rx, tx, at)
}

// readNICs 遍历系统网卡，读取累计收发字节。
//
// 走 GetIfEntry2Ex 而不是抓 WMI/`netstat -e`：前者是单次 API 调用、无子进程、无 COM，
// 完全符合"Agent 单次采集 CPU 峰值 ≤0.5%"的指标。
func (c *windowsCollector) readNICs() (map[string]model.NICCounters, int64, int64) {
	include := c.opts.NICInclude
	exclude := c.opts.NICExclude
	if len(exclude) == 0 && len(include) == 0 {
		exclude = DefaultNICExclude()
	}
	// Windows 的虚拟网卡命名与 Linux 不同（"vEthernet (...)"、"Loopback"），
	// 在默认排除表基础上再补几条，按 `*` 通配匹配。
	exclude = append(exclude, "vEthernet (*", "*Loopback*", "*WSL*", "*Hyper-V*", "*Npcap*", "*TAP-*")

	out := map[string]model.NICCounters{}
	var rxTotal, txTotal int64

	rows, err := winInterfaceRows()
	if err != nil {
		return out, 0, 0
	}
	for _, row := range rows {
		name := windows.UTF16ToString(row.Alias[:])
		if name == "" {
			name = windows.UTF16ToString(row.Description[:])
		}
		if name == "" {
			continue
		}
		// 回环与已下线的接口不计入。
		if row.Type == windows.IF_TYPE_SOFTWARE_LOOPBACK {
			continue
		}
		if row.OperStatus != windows.IfOperStatusUp {
			continue
		}
		if !nicAllowed(name, include, exclude) {
			continue
		}
		// MIB_IF_ROW2 的 InOctets/OutOctets 是 64 位累计值（不会 32 位回绕）。
		rx := int64(row.InOctets)
		tx := int64(row.OutOctets)
		out[name] = model.NICCounters{Rx: rx, Tx: tx}
		rxTotal += rx
		txTotal += tx
	}
	return out, rxTotal, txTotal
}

// cpuPercent 用两次 GetSystemTimes 差值算使用率。
//
// 口径与 Linux 侧一致：busy = (kernel + user) − idle，其中 kernel 已含 idle 时间，
// 所以不能把三者直接相加当总量（那是 Windows 的经典坑，会把使用率算成一半）。
func (c *windowsCollector) cpuPercent() (int, bool) {
	cur, ok := systemTimes()
	if !ok {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.primed {
		c.prevCPU, c.primed = cur, true
		return 0, false
	}
	dIdle := float64(cur.idle - c.prevCPU.idle)
	dKernel := float64(cur.kernel - c.prevCPU.kernel)
	dUser := float64(cur.user - c.prevCPU.user)
	c.prevCPU = cur

	// kernel 时间包含 idle，故总时间为 (kernel + user)。
	total := dKernel + dUser
	if total <= 0 {
		return 0, false
	}
	used := (total - dIdle) / total * 10000
	if used < 0 {
		used = 0
	}
	if used > 10000 {
		used = 10000
	}
	return int(used), true
}

// ---------------------------------------------------------------------------
// Win32 绑定
// ---------------------------------------------------------------------------

var (
	kernel32                = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes      = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatus  = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetTickCount64      = kernel32.NewProc("GetTickCount64")
)

type winFiletime struct {
	Low  uint32
	High uint32
}

func (f winFiletime) uint64() uint64 { return uint64(f.High)<<32 | uint64(f.Low) }

// systemTimes 读取 GetSystemTimes 的三个 FILETIME（单位 100ns）。
func systemTimes() (winCPUTimes, bool) {
	var idle, kern, user winFiletime
	r, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kern)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return winCPUTimes{}, false
	}
	return winCPUTimes{idle: idle.uint64(), kernel: kern.uint64(), user: user.uint64()}, true
}

type winMemStatus struct {
	Length        uint32
	MemoryLoad    uint32
	TotalPhys     uint64
	AvailPhys     uint64
	TotalPageFile uint64
	AvailPageFile uint64
	TotalVirtual  uint64
	AvailVirtual  uint64
	AvailExtended uint64
}

func queryMemory(m *winMemStatus) bool {
	m.Length = uint32(unsafe.Sizeof(*m))
	r, _, _ := procGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(m)))
	return r != 0
}

// ---------------------------------------------------------------------------
// 交换区（页面文件）
// ---------------------------------------------------------------------------
//
// `GlobalMemoryStatusEx` 给的其实是**提交（commit）**口径，不是页面文件本身：
//
//	ullTotalPageFile = 提交限额 = 物理内存 + 所有页面文件之和
//	ullAvailPageFile = 剩余可提交量
//
// 所以真正属于"页面文件"的那部分是 `TotalPageFile − TotalPhys`。
//
// 这里踩过一个坑：总量按上面的公式算、已用却直接用 `TotalPageFile − AvailPageFile`，
// 两者基数不同 → 已用（含物理内存占用）远大于总量 → `Percent` 恒被钳到 10000，
// 卡片上永远显示"交换区 100%"。修正后两侧都先减 `TotalPhys`，
// 换算成"页面文件口径"后再相减（见 winSwapUsed）。
//
// 注意这个值**包含物理内存**，所以单位是"提交量"而非"换出到磁盘的量"；
// Windows 没有像 Linux SwapCached 那样的"实际换出"计数器，这是最接近的口径。

// winSwapTotal 返回页面文件总量（字节）。取不到或系统未启用页面文件时返回 0。
func winSwapTotal(m winMemStatus) int64 {
	if m.TotalPageFile <= m.TotalPhys {
		return 0 // 未启用页面文件（或提交限额不大于物理内存）
	}
	return int64(m.TotalPageFile - m.TotalPhys)
}

// winSwapUsed 返回页面文件口径下的已用量（字节），与 winSwapTotal 同基数。
//
// 推导：提交已用 = TotalPageFile − AvailPageFile；
//
//	其中物理内存部分最多占 TotalPhys（也可能没占满，用 min 兜住）；
//	页面文件已用 = 提交已用 − 物理内存实际占用的提交量。
//
// 简化但不失真的做法：页面文件已用 = min(提交已用, winSwapTotal)。
// 因为"提交已用"一旦超过物理内存容量，超出部分必然是页面文件承载的；
// 而未超过时页面文件实际几乎没被用（Windows 只在压力下才换出）。
func winSwapUsed(m winMemStatus) int64 {
	total := winSwapTotal(m)
	if total <= 0 {
		return 0
	}
	if m.TotalPageFile <= m.AvailPageFile {
		return 0
	}
	commitUsed := int64(m.TotalPageFile - m.AvailPageFile)
	// 提交已用里，最多 TotalPhys 是物理内存贡献的；剩余才是页面文件。
	if commitUsed <= int64(m.TotalPhys) {
		return 0
	}
	used := commitUsed - int64(m.TotalPhys)
	if used > total {
		used = total
	}
	return used
}

// uptimeSeconds 返回自开机以来的秒数。
func uptimeSeconds() (uint64, bool) {
	r, _, _ := procGetTickCount64.Call()
	if r == 0 {
		return 0, false
	}
	return uint64(r) / 1000, true
}

// winInterfaceRows 用 GetAdaptersAddresses 枚举 IfIndex，再逐个 GetIfEntry2Ex 取计数器。
//
// 为什么不直接用 GetIfTable2：GetIfTable2 返回的是需要手动 FreeMibTable 的 C 数组，
// 解析起来要按 Length 逐条步进指针；而"先拿 Index 再单查"路径全部落在 x/sys 已有绑定里，
// 出错面更小。网卡数量是个位数，多几次调用的开销可以忽略。
func winInterfaceRows() ([]windows.MibIfRow2, error) {
	idx, err := winInterfaceIndexes()
	if err != nil {
		return nil, err
	}
	out := make([]windows.MibIfRow2, 0, len(idx))
	for _, i := range idx {
		var row windows.MibIfRow2
		row.InterfaceIndex = i
		// MibIfRow2Level... 传 0 表示"按 InterfaceIndex 查"。
		if err := windows.GetIfEntry2Ex(0, &row); err != nil {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// winInterfaceIndexes 通过 GetAdaptersAddresses 取回所有接口索引。
func winInterfaceIndexes() ([]uint32, error) {
	// 先要一次大小（必然返回 ERROR_BUFFER_OVERFLOW），再按需分配。
	var size uint32
	const flags = windows.GAA_FLAG_SKIP_UNICAST | windows.GAA_FLAG_SKIP_ANYCAST |
		windows.GAA_FLAG_SKIP_MULTICAST | windows.GAA_FLAG_SKIP_DNS_SERVER |
		windows.GAA_FLAG_INCLUDE_PREFIX
	err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, flags, 0, nil, &size)
	if err != nil && err != windows.ERROR_BUFFER_OVERFLOW {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	first := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
	if err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, flags, 0, first, &size); err != nil {
		return nil, err
	}
	var out []uint32
	for a := first; a != nil; a = a.Next {
		if a.IfIndex != 0 {
			out = append(out, a.IfIndex)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 磁盘 / 系统信息
// ---------------------------------------------------------------------------

func winDiskTotalOf(paths []string) int64 {
	var total int64
	for _, p := range paths {
		if _, t, ok := winDiskSpace(p); ok {
			total += t
		}
	}
	return total
}

func winDiskUsedOf(paths []string) int64 {
	var used int64
	for _, p := range paths {
		if freeAvail, t, ok := winDiskSpace(p); ok && t > freeAvail {
			used += t - freeAvail
		}
	}
	return used
}

// winDiskSpace 返回 (可用字节, 总字节, 是否成功)。
//
// 用 GetDiskFreeSpaceExW 而不是 syscall.GetDiskFreeSpaceEx：
// 后者在 Go 的 windows/syscall 里存在但只接受 *int64 的"可用"单值形态且对路径前缀
// （`\\?\` / UNC）处理更挑剔；直接调 W API 语义最明确。
func winDiskSpace(path string) (int64, int64, bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, false
	}
	var freeAvail, total, totalFree uint64
	r, _, _ := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeAvail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return 0, 0, false
	}
	return int64(freeAvail), int64(total), true
}

// windowsCPUInfo 读取 CPU 型号与核心数。
//
// 走注册表而不是 WMI：HKEY_LOCAL_MACHINE\HARDWARE\DESCRIPTION\System\CentralProcessor\0
// 的 ProcessorNameString 是系统启动时写好的，读取是一次纯本地调用，无 COM/WMI 开销。
func windowsCPUInfo() (string, int) {
	key, err := openRegKey(windows.HKEY_LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`)
	if err != nil {
		return "", 0
	}
	defer windows.RegCloseKey(key)

	name := regString(key, "ProcessorNameString")
	return trimField(name, 64), runtime.NumCPU()
}

// regString 读取 REG_SZ / REG_EXPAND_SZ 值。
//
// x/sys 的注册表辅助函数是 `RegOpenKeyEx(key Handle, subkey *uint16, options, access uint32, result *Handle)`，
// 没有"返回句柄"的便捷形态，这里包一层把错误处理收在一起。
func openRegKey(root windows.Handle, path string) (windows.Handle, error) {
	var h windows.Handle
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	if err := windows.RegOpenKeyEx(root, p, 0, windows.KEY_READ, &h); err != nil {
		return 0, err
	}
	return h, nil
}

func regString(key windows.Handle, name string) string {
	var typ, size uint32
	n, _ := windows.UTF16PtrFromString(name)
	if err := windows.RegQueryValueEx(key, n, nil, &typ, nil, &size); err != nil || size == 0 {
		return ""
	}
	buf := make([]uint16, size/2+1)
	if err := windows.RegQueryValueEx(key, n, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf)
}

func regDword(key windows.Handle, name string) uint32 {
	var typ, size uint32 = 0, 4
	n, _ := windows.UTF16PtrFromString(name)
	var v uint32
	if err := windows.RegQueryValueEx(key, n, nil, &typ, (*byte)(unsafe.Pointer(&v)), &size); err != nil {
		return 0
	}
	return v
}

// windowsBuildNumber 取内核构建号（形如 "10.0.22631"），作为"内核"字段。
func windowsBuildNumber() string {
	key, err := openRegKey(windows.HKEY_LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`)
	if err != nil {
		return ""
	}
	defer windows.RegCloseKey(key)
	cur := regString(key, "CurrentBuildNumber")
	if cur == "" {
		return "Windows"
	}
	return trimField("10.0."+cur, 64)
}

// windowsOSName 取友好的系统名（ProductName + DisplayVersion），失败回落 "Windows"。
func windowsOSName() string {
	key, err := openRegKey(windows.HKEY_LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`)
	if err != nil {
		return "Windows"
	}
	defer windows.RegCloseKey(key)
	product := regString(key, "ProductName")    // 例如 "Windows 11 Pro"
	display := regString(key, "DisplayVersion") // 例如 "24H2"
	if product == "" {
		product = "Windows"
	}
	if display != "" {
		return trimField(product+" "+display, 64)
	}
	return trimField(product, 64)
}

// windowsVirt 判断是否跑在虚拟机里。
//
// 判据用注册表里由固件写入的 SystemManufacturer / SystemProductName——
// 这是各家 hypervisor 都会留痕的位置，且不依赖 WMI。
func windowsVirt() string {
	key, err := openRegKey(windows.HKEY_LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\BIOS`)
	if err != nil {
		return "none"
	}
	defer windows.RegCloseKey(key)
	maker := regString(key, "SystemManufacturer")
	product := regString(key, "SystemProductName")
	blob := maker + " " + product

	switch {
	case contains(blob, "VMware"):
		return "vmware"
	case contains(blob, "VirtualBox"):
		return "virtualbox"
	case contains(blob, "QEMU"), contains(blob, "KVM"):
		return "kvm"
	case contains(blob, "Xen"):
		return "xen"
	case contains(blob, "Microsoft Corporation") && contains(product, "Virtual Machine"):
		return "hyperv"
	case contains(blob, "Parallels"):
		return "parallels"
	case contains(blob, "Amazon EC2"):
		return "kvm"
	case contains(blob, "Aliyun"), contains(blob, "Tencent"), contains(blob, "HVM"):
		return "kvm"
	}
	if contains(blob, "WSL") {
		return "wsl"
	}
	return "none"
}
