//go:build windows

package collector

import (
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWindowsCollectorLive 在真实 Windows 上跑一次采集，断言关键字段非零。
//
// 这类"采集器真的能出数"的测试必须在本机跑：跨平台交叉编译只能证明语法正确，
// 证明不了 API 绑定对了——历史上就踩过"编译通过但 API 参数顺序错、返回全 0"的坑。
func TestWindowsCollectorLive(t *testing.T) {
	c := New(Options{AgentVersion: "test"})

	st, err := c.Static()
	if err != nil {
		t.Fatalf("Static() 出错: %v", err)
	}
	if st.Hostname == "" {
		t.Error("Hostname 为空")
	}
	if st.CPUCores <= 0 {
		t.Errorf("CPUCores = %d，应为正数", st.CPUCores)
	}
	if st.MemTotal <= 0 {
		t.Errorf("MemTotal = %d，应为正数（GlobalMemoryStatusEx 可能绑定失败）", st.MemTotal)
	}
	if st.DiskTotal <= 0 {
		t.Errorf("DiskTotal = %d，应为正数（GetDiskFreeSpaceEx 可能绑定失败）", st.DiskTotal)
	}
	if st.OS == "" {
		t.Error("OS 为空")
	}
	t.Logf("静态信息：%s / %s / %s / %d 核 / 内存 %.1f GB / 磁盘 %.1f GB / virt=%s",
		st.Hostname, st.OS, st.Kernel, st.CPUCores,
		float64(st.MemTotal)/1e9, float64(st.DiskTotal)/1e9, st.Virt)

	// CPU 使用率需要两次采样才有值，间隔要能观察到时间推进。
	if _, err := c.Sample(); err != nil {
		t.Fatalf("首次 Sample() 出错: %v", err)
	}
	// 等一点时间，确保 GetSystemTimes 的计数确实变化。
	time.Sleep(300 * time.Millisecond)

	m, err := c.Sample()
	if err != nil {
		t.Fatalf("第二次 Sample() 出错: %v", err)
	}
	if m.CPU == nil {
		t.Error("CPU 为 nil，两次采样差值未生效")
	} else if *m.CPU < 0 || *m.CPU > 10000 {
		t.Errorf("CPU = %d，超出 [0,10000] 范围", *m.CPU)
	}
	if m.MemUsed == nil {
		t.Error("MemUsed 为 nil")
	} else if *m.MemUsed <= 0 || *m.MemUsed > st.MemTotal {
		t.Errorf("MemUsed = %d，应在 (0, %d] 之间", *m.MemUsed, st.MemTotal)
	}
	if m.DiskUsed == nil || *m.DiskUsed <= 0 {
		t.Errorf("DiskUsed = %v，应为正数", m.DiskUsed)
	}
	if m.BootTime == nil || *m.BootTime <= 0 {
		t.Errorf("BootTime = %v，应为正数（GetTickCount64）", m.BootTime)
	}
	// Load1 在 Windows 上必须为空（没有 POSIX load average）。
	if m.Load1 != nil {
		t.Errorf("Load1 = %v，Windows 上必须为 nil", *m.Load1)
	}

	t.Logf("采样：CPU=%s 内存已用=%.2f GB 磁盘已用=%.1f GB 网卡数=%d 接收=%.1f KB/s 发送=%.1f KB/s",
		fmtPtr(m.CPU), float64(derefI64(m.MemUsed))/1e9, float64(derefI64(m.DiskUsed))/1e9,
		len(m.NICs), float64(derefI64(m.RxBps))/1024, float64(derefI64(m.TxBps))/1024)

	if len(m.NICs) == 0 {
		t.Error("NICs 为空：GetAdaptersAddresses / GetIfEntry2Ex 绑定可能有问题")
	}
	for name, c := range m.NICs {
		if c.Rx < 0 || c.Tx < 0 {
			t.Errorf("网卡 %s 计数为负：rx=%d tx=%d", name, c.Rx, c.Tx)
		}
	}
}

// TestWindowsCPUTimesAdvances 断言 GetSystemTimes 的三个计数器真的在推进、且不会回退。
//
// **不要断言"idle 必须增长"**（原写法，2026-10-12 第二次挂掉后才定位到根因）：
// `go test ./...` 会让各包并行编译+运行，把 12 个逻辑核彻底占满，
// 此时 idle 时间**一个节拍都不会增长**——失败现场就是 kernel/user 都在涨、只有 idle 一条不变。
// 实测（本机 12 核，进程内烧满 12 个 goroutine 共 0.8 秒）：
//
//	idle Δ ≈ 0.0 ms    kernel Δ ≈ 1906 ms    user Δ ≈ 7828 ms
//
// 原实现是"轮询最多 2 秒等三项同时推进"，并把原因归给"系统时钟节拍"——方向错了：
// 轮询再久也等不到一个物理上不会增长的数字。它单跑能过，只是因为启动斜坡会漏进一两个
// tick（所以连"改成依赖 idle"的变异都测不出来：变异版单跑照样绿）。结论是
// **idle 这个信号朝两个方向都会骗人**，不能拿来做断言。
//
// 改法：**主动造负载**，断言 user 必须涨。user Δ 是秒级（数千万个单位），
// 与机器闲不闲无关，属稳健信号；再无条件断言**总时间**推进，作为"计数器还活着"的最低证据。
//
// 这里**不声明**能抓住 idle/user 指针接反：真 idle 仍可能漏进零星 tick，那条路同样不稳。
// 要覆盖"绑定死了"走的是产品侧变异——让 systemTimes 恒返回零值（见 .tmpcheck/mutate.py）。
func TestWindowsCPUTimesAdvances(t *testing.T) {
	a, ok := systemTimes()
	if !ok {
		t.Fatal("systemTimes() 第一次调用失败")
	}

	// 主动烧 CPU：每核一个 goroutine 空转；用原子计数保证循环不会被优化掉，
	// 顺便让下面的 burn.Load() 能自证"负载真的造出来了"（否则这个测试会自己失效）。
	var burn atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					burn.Add(1)
				}
			}
		}()
	}
	defer func() { close(stop); wg.Wait() }()

	const (
		pollEvery = 50 * time.Millisecond
		budget    = 2 * time.Second
	)
	deadline := time.Now().Add(budget)
	var last winCPUTimes
	advanced := false
	for {
		time.Sleep(pollEvery)
		b, ok := systemTimes()
		if !ok {
			t.Fatal("systemTimes() 后续调用失败")
		}
		last = b
		// 单调性是硬约束：任何一项回退都说明读的不是同一个来源或有溢出。
		if b.idle < a.idle || b.kernel < a.kernel || b.user < a.user {
			t.Fatalf("计数回退：idle %d→%d, kernel %d→%d, user %d→%d",
				a.idle, b.idle, a.kernel, b.kernel, a.user, b.user)
		}
		if b.user > a.user {
			advanced = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
	}

	if burn.Load() == 0 {
		t.Fatal("空转循环一次都没执行，这个测试自己失效了")
	}
	if !advanced {
		t.Errorf("烧满 CPU %v 后 user 计数仍未推进：user %d→%d（idle %d→%d，kernel %d→%d）",
			budget, a.user, last.user, a.idle, last.idle, a.kernel, last.kernel)
	}
	if last.idle+last.kernel+last.user <= a.idle+a.kernel+a.user {
		t.Errorf("CPU 总时间未推进：%d → %d（三个计数器都像死的）",
			a.idle+a.kernel+a.user, last.idle+last.kernel+last.user)
	}
}

// TestWindowsDiskAndMemoryAPI 单独验证两个容易绑错的 API。
func TestWindowsDiskAndMemoryAPI(t *testing.T) {
	free, total, ok := winDiskSpace(`C:\`)
	if !ok {
		t.Fatal("winDiskSpace(C:\\) 失败")
	}
	if total <= 0 {
		t.Errorf("total = %d，应为正数", total)
	}
	if free < 0 || free > total {
		t.Errorf("free = %d，应在 [0, %d] 之间", free, total)
	}
	t.Logf("C:\\ 总量 %.1f GB，可用 %.1f GB", float64(total)/1e9, float64(free)/1e9)

	var mem winMemStatus
	if !queryMemory(&mem) {
		t.Fatal("GlobalMemoryStatusEx 失败")
	}
	if mem.TotalPhys <= 0 {
		t.Errorf("TotalPhys = %d，应为正数", mem.TotalPhys)
	}
	if mem.AvailPhys > mem.TotalPhys {
		t.Errorf("AvailPhys (%d) > TotalPhys (%d)", mem.AvailPhys, mem.TotalPhys)
	}
	if mem.MemoryLoad > 100 {
		t.Errorf("MemoryLoad = %d，应为百分比", mem.MemoryLoad)
	}
	t.Logf("物理内存 %.1f GB / 可用 %.1f GB / 负载 %d%%",
		float64(mem.TotalPhys)/1e9, float64(mem.AvailPhys)/1e9, mem.MemoryLoad)
}

// TestWindowsNICFiltering 断言回环与虚拟网卡被排除。
func TestWindowsNICFiltering(t *testing.T) {
	c := New(Options{}).(*windowsCollector)
	nics, _, _ := c.readNICs()
	for name := range nics {
		if contains(name, "Loopback") {
			t.Errorf("回环接口 %q 未被排除", name)
		}
		if contains(name, "vEthernet (") {
			t.Errorf("Hyper-V 虚拟接口 %q 未被排除", name)
		}
	}
	t.Logf("统计中的网卡（%d 个）：", len(nics))
	for name := range nics {
		t.Logf("  - %s", name)
	}
}

func fmtPtr(p *int) string {
	if p == nil {
		return "nil"
	}
	return strconv.Itoa(*p)
}

func derefI64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// TestWindowsSwapTotalAndUsedSameBase 回归一个真实的显示缺陷。
//
// 症状：后台/公开看板的"交换区"永远显示 100%。
// 根因：`GlobalMemoryStatusEx` 给的是**提交（commit）**口径，
//
//	TotalPageFile = 物理内存 + 页面文件
//
// 而旧代码算总量时减了 TotalPhys、算已用时没减，两者基数不同：
//
//	swap_total = TotalPageFile − TotalPhys        （页面文件口径，小）
//	swap_used  = TotalPageFile − AvailPageFile    （提交口径，含物理内存，大）
//
// 于是 used > total 恒成立，`state.Percent` 里 `if v > 10000 { v = 10000 }` 把它钳成 100%。
// 本机实测：swap_total ≈ 2.7 GB，swap_used ≈ 34 GB（内存是 32 GB），百分比满格。
//
// 修正后两侧都先减 TotalPhys，保证 used ≤ total。
func TestWindowsSwapTotalAndUsedSameBase(t *testing.T) {
	var mem winMemStatus
	if !queryMemory(&mem) {
		t.Fatal("GlobalMemoryStatusEx 失败")
	}
	t.Logf("TotalPhys=%.2f GiB AvailPhys=%.2f GiB TotalPageFile=%.2f GiB AvailPageFile=%.2f GiB",
		float64(mem.TotalPhys)/(1<<30), float64(mem.AvailPhys)/(1<<30),
		float64(mem.TotalPageFile)/(1<<30), float64(mem.AvailPageFile)/(1<<30))

	total := winSwapTotal(mem)
	used := winSwapUsed(mem)

	if total < 0 {
		t.Errorf("winSwapTotal = %d，不应为负", total)
	}
	if used < 0 {
		t.Errorf("winSwapUsed = %d，不应为负", used)
	}
	// 核心断言：已用不得超过总量。旧实现必挂在这里。
	if total > 0 && used > total {
		t.Errorf("swap 已用(%d) > 总量(%d)：两侧基数不一致，百分比会被钳到 100%%", used, total)
	}
	// 总量应当等于提交限额减去物理内存；未启用页面文件时为 0。
	if mem.TotalPageFile > mem.TotalPhys {
		want := int64(mem.TotalPageFile - mem.TotalPhys)
		if total != want {
			t.Errorf("winSwapTotal = %d，期望 %d", total, want)
		}
	} else if total != 0 {
		t.Errorf("提交限额不大于物理内存时 winSwapTotal 应为 0，得到 %d", total)
	}
}

// TestWindowsSwapPercentSane 端到端断言：用真实的 Sample+Static 走一遍
// `state.Percent` 的口径，百分比必须落在 0~10000 且不恒为满格。
func TestWindowsSwapPercentSane(t *testing.T) {
	c := New(Options{AgentVersion: "test"}).(*windowsCollector)
	st, err := c.Static()
	if err != nil {
		t.Fatalf("Static 失败: %v", err)
	}
	m, err := c.Sample()
	if err != nil {
		t.Fatalf("Sample 失败: %v", err)
	}
	if m.SwapUsed == nil {
		t.Skip("本机未采集到交换区用量（未启用页面文件？），跳过")
	}
	if st.SwapTotal <= 0 {
		t.Fatalf("Sample 给了 swapUsed=%d，但 Static 的 swapTotal 为 0——"+
			"两者基数不一致，Percent 会除零返回 0 或钳到 100%%", *m.SwapUsed)
	}
	// 复刻 state.Percent 的逻辑（同包不能 import state，避免循环依赖）。
	pct := int(*m.SwapUsed * 10000 / st.SwapTotal)
	if pct < 0 || pct > 10000 {
		t.Errorf("swap 百分比 = %d，超出 0~10000", pct)
	}
	t.Logf("swap: used=%.2f GiB total=%.2f GiB → %d%%",
		float64(*m.SwapUsed)/(1<<30), float64(st.SwapTotal)/(1<<30), pct/100)
}
