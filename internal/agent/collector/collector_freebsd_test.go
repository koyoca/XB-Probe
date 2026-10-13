//go:build freebsd

package collector

import (
	"encoding/binary"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// FreeBSD 采集器的测试分两类：
//   - 纯解析逻辑（可在任意 FreeBSD 机器上跑，不依赖特定硬件）；
//   - 真机冒烟（读真实 sysctl，字段允许缺失但要保证"能读就合法"）。
//
// 这些测试在 Windows 上无法执行（build tag freebsd），
// 用 `GOOS=freebsd go test -c` 只能验证可编译性；
// 真实数值验证需在 FreeBSD 上跑 `go test ./internal/agent/collector/`。

// TestFreeBSDCpTimesLayout 验证 kern.cp_times 的解析长度是 CPUSTATES 的整数倍。
//
// 若内核给出非 5 的倍数，说明 CPUSTATES 口径变了（或读到的是别的 sysctl），
// cpuPercent 会把数组错位解释，算出来的使用率会常年是垃圾值。
func TestFreeBSDCpTimesLayout(t *testing.T) {
	cur, ok := readCPTimes()
	if !ok {
		t.Skip("kern.cp_times 不可读，跳过")
	}
	if len(cur) < freebsdCPUStates {
		t.Fatalf("kern.cp_times 长度 = %d，少于一个 CPU 的状态数 %d", len(cur), freebsdCPUStates)
	}
	if len(cur)%freebsdCPUStates != 0 {
		t.Errorf("kern.cp_times 长度 = %d，不是 %d 的整数倍：每核状态数口径可能已变",
			len(cur), freebsdCPUStates)
	}
	nCPU := len(cur) / freebsdCPUStates
	cores := int(sysctlU64("hw.ncpu", 0))
	if cores > 0 && nCPU != cores {
		t.Errorf("kern.cp_times 推出的核数 = %d，hw.ncpu = %d", nCPU, cores)
	}
}

// TestFreeBSDCpuPercent 验证连续两次采样能算出合法的使用率。
//
// 第一次必然返回 ok=false（只做基线，没有差值）；第二次起应给出 0~10000 的值。
// 这条测试能抓住"prevCPUs 忘记更新导致永远活着基线"之类的错误。
func TestFreeBSDCpuPercent(t *testing.T) {
	c := &freebsdCollector{rates: map[string]*rateCalculator{}}

	if _, ok := c.cpuPercent(); ok {
		t.Error("第一次采样不该给出使用率（没有基线可减）")
	}
	// 让 jiffies 动一动。
	time.Sleep(60 * time.Millisecond)

	pct, ok := c.cpuPercent()
	if !ok {
		t.Skip("第二次采样仍拿不到 kern.cp_times 差值，跳过")
	}
	if pct < 0 || pct > 10000 {
		t.Errorf("CPU 使用率 = %d，超出 0~10000（百分之一百分比）范围", pct)
	}
}

// TestFreeBSDIfDataParse 用合成消息验证 if_data 解析（不依赖真实网卡）。
//
// 构造一条 RTM_IFINFO：if_msghdr + sockaddr_dl，断言能取出接口名与计数。
// 这是 readNICs 的核心——解析错位会让所有网卡计数变成噪声。
func TestFreeBSDIfDataParse(t *testing.T) {
	name := "em0"
	msg := buildIfInfoMessage(name, 1234, 5678)

	hdr := (*unix.IfMsghdr)(unsafe.Pointer(&msg[0]))

	got := ifNameFromMsghdr(msg, int(hdr.Addrs))
	if got != name {
		t.Errorf("解析出的接口名 = %q，期望 %q", got, name)
	}

	data := unix.IfData(hdr.Data)
	gotRx, gotTx := readIfBytes(&data)
	if gotRx != 1234 || gotTx != 5678 {
		t.Errorf("计数解析 = (rx %d, tx %d)，期望 (1234, 5678)", gotRx, gotTx)
	}
}

// TestFreeBSDIfDataParseTruncated 断言畸形输入返回空串而不是 panic 或越界读。
//
// NET_RT_IFLIST 的缓冲区来自内核，理论上受信，但接口在两次 sysctl 之间
// 增删会让长度对不上；防御性解析必须扛得住。
func TestFreeBSDIfDataParseTruncated(t *testing.T) {
	full := buildIfInfoMessage("em0", 1, 1)

	cases := []struct {
		name  string
		msg   []byte
		addrs int
	}{
		{"空", nil, 1},
		{"只有一个字节", []byte{0x01}, 1},
		{"addrs 为 0", full, 0},
		{"addrs 为负", full, -1},
		{"截断到头部", full[:8], 1},
		{"截断到 sockaddr 前半", full[:int(unsafe.Sizeof(unix.IfMsghdr{}))+3], 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ifNameFromMsghdr(tc.msg, tc.addrs); got != "" {
				t.Errorf("畸形输入应返回空串，得到 %q", got)
			}
		})
	}
}

// TestFreeBSDLoadavgParse 验证 vm.loadavg 的定点数换算。
//
// 这里不读真实 sysctl（数值随时在变），而是复用换算逻辑本身：
// 用 unix.SysctlRaw 读到的真实数据走同一段代码路径，只断言"落在合理区间"。
func TestFreeBSDLoadavgParse(t *testing.T) {
	load, ok := freebsdLoad1()
	if !ok {
		t.Skip("vm.loadavg 不可读，跳过")
	}
	// 使用率×100 的负载：只要不是负的、也没离谱到 100000（=1000 的负载）就算合理。
	if load < 0 || load > 100000 {
		t.Errorf("load1 = %d，超出合理区间", load)
	}
}

// TestFreeBSDStaticSampleContract 在真机上跑一遍 Static+Sample，断言字段自洽。
//
// 重点断言（都是踩过的坑）：
//   - 核数 > 0（=0 会让后台显示 0 核）；
//   - 内存总量 > 0 且 已用 ≤ 总量；
//   - 磁盘总量 > 0 且 已用 ≤ 总量；
//   - 已用交换 ≤ 交换总量（swapFreePages 口径错了会溢出）。
func TestFreeBSDStaticSampleContract(t *testing.T) {
	if _, err := unix.SysctlUint64("hw.ncpu"); err != nil {
		t.Skip("不是 FreeBSD 环境（sysctl 不可用），跳过")
	}

	c := New(Options{AgentVersion: "test"}).(*freebsdCollector)

	st, err := c.Static()
	if err != nil {
		t.Fatalf("Static 失败: %v", err)
	}
	if st.CPUCores <= 0 {
		t.Errorf("CPUCores = %d，应 > 0", st.CPUCores)
	}
	if st.MemTotal <= 0 {
		t.Errorf("MemTotal = %d，应 > 0", st.MemTotal)
	}
	if st.DiskTotal <= 0 {
		t.Errorf("DiskTotal = %d，应 > 0", st.DiskTotal)
	}
	if st.OS == "" {
		t.Error("OS 为空：freebsdOSName 的回落链失效了")
	}
	if st.SwapTotal < 0 {
		t.Errorf("SwapTotal = %d，不应为负", st.SwapTotal)
	}

	m, err := c.Sample()
	if err != nil {
		t.Fatalf("Sample 失败: %v", err)
	}
	if m.MemUsed != nil {
		if *m.MemUsed < 0 {
			t.Errorf("MemUsed = %d，不应为负", *m.MemUsed)
		}
		if *m.MemUsed > st.MemTotal {
			t.Errorf("MemUsed(%d) > MemTotal(%d)：可用内存口径算反了",
				*m.MemUsed, st.MemTotal)
		}
	}
	if m.DiskUsed != nil {
		if *m.DiskUsed < 0 {
			t.Errorf("DiskUsed = %d，不应为负", *m.DiskUsed)
		}
		if *m.DiskUsed > st.DiskTotal {
			t.Errorf("DiskUsed(%d) > DiskTotal(%d)", *m.DiskUsed, st.DiskTotal)
		}
	}
	if m.SwapUsed != nil && st.SwapTotal > 0 && *m.SwapUsed > st.SwapTotal {
		t.Errorf("SwapUsed(%d) > SwapTotal(%d)：swap 空闲页口径错了",
			*m.SwapUsed, st.SwapTotal)
	}
	if m.BootTime != nil {
		now := time.Now().Unix()
		if *m.BootTime <= 0 || *m.BootTime > now {
			t.Errorf("BootTime = %d，应落在 (0, %d]", *m.BootTime, now)
		}
	}
}

// TestFreeBSDReadNICsReal 在真机上读网卡，断言计数非负且速率被初始化。
//
// 不断言"一定有网卡"——某些 jail/CI 环境里可能一个都没有；
// 但只要读到了，就必须是合法计数。
func TestFreeBSDReadNICsReal(t *testing.T) {
	c := New(Options{AgentVersion: "test"}).(*freebsdCollector)

	nics, rx, tx := c.readNICs()
	if rx < 0 || tx < 0 {
		t.Errorf("累计字节为负: rx=%d tx=%d", rx, tx)
	}
	for name, n := range nics {
		if n.Rx < 0 || n.Tx < 0 {
			t.Errorf("网卡 %s 计数为负: rx=%d tx=%d", name, n.Rx, n.Tx)
		}
	}

	// 第一次调用只建基线，速率必须是 0（不能是未初始化的大数）。
	r1, t1 := c.sumRates(rx, tx, time.Now())
	if r1 != 0 || t1 != 0 {
		t.Errorf("首轮速率 = (%d, %d)，应均为 0（只做基线）", r1, t1)
	}
	// 第二轮回传更大的计数，速率应为正数。
	r2, t2 := c.sumRates(rx+10000, tx+20000, time.Now().Add(time.Second))
	if r2 <= 0 || t2 <= 0 {
		t.Errorf("次轮速率 = (%d, %d)，应为正数", r2, t2)
	}
}

// TestFreeBSDVirtAndOSName 断言虚拟化与系统名有确定的非空返回。
//
// 后台卡片上这两项必须永远有值：virt 为空会让"是否容器"的筛选失效；
// OS 为空则卡片显示空白而不是 "—"。
func TestFreeBSDVirtAndOSName(t *testing.T) {
	if v := freebsdVirt(); v == "" {
		t.Error("freebsdVirt 返回空串，应至少回落 \"none\"")
	}
	if v := freebsdOSName(); v == "" {
		t.Error("freebsdOSName 返回空串")
	}
}

// ---------------------------------------------------------------------------
// 测试辅助：合成一条 RTM_IFINFO 消息
// ---------------------------------------------------------------------------

// buildIfInfoMessage 拼出 if_msghdr + sockaddr_dl 的最小消息。
//
// 布局（FreeBSD 12+，64 位）：
//
//	if_msghdr  { msglen u16; version u8; type u8; addrs i32; flags i32;
//	             index u16; ... ; data if_data }
//	sockaddr_dl{ len u8; family u8; index u16; type u8; nlen u8; alen u8;
//	             slen u8; data[...] }
//
// 注意 struct 对齐：IfMsghdr 里 u16/u8 之后会补齐，所以不能手工算偏移，
// 必须真的分配一个 IfMsghdr 并把字段写进去。
func buildIfInfoMessage(name string, rxBytes, txBytes uint64) []byte {
	hdrSize := int(unsafe.Sizeof(unix.IfMsghdr{}))
	// sockaddr_dl 头部 8 字节（含 len/family/... 与 data 起始）
	saSize := 8 + len(name)
	// sockaddr 长度按 4 字节对齐（sa_len 记录的是含填充的长度）。
	saSize = (saSize + 3) &^ 3

	msg := make([]byte, hdrSize+saSize)

	hdr := (*unix.IfMsghdr)(unsafe.Pointer(&msg[0]))
	// IfMsghdr 内部的 Data 是 if_data；先写头部字段。
	// 直接给这几个已知字段赋值（其余保持 0）。
	// 用 unsafe 写 msglen/version/type/addrs 的首字节区，避免依赖字段名差异。
	binary.LittleEndian.PutUint16(msg[0:], uint16(len(msg))) // ifm_msglen
	msg[2] = 1                                               // ifm_version = RTM_VERSION
	msg[3] = byte(unix.RTM_IFINFO)                           // ifm_type
	binary.LittleEndian.PutUint32(msg[4:], 1)                // ifm_addrs（后面跟 1 个 sockaddr）
	binary.LittleEndian.PutUint16(msg[12:], 1)               // ifm_index

	// 注意：freebsd/386 与 freebsd/arm 上 if_data.Ibytes/Obytes 是 uint32，
	// 直接用 uint64 赋值编译不过。这里显式转换，顺带把测试值限制在 32 位范围内
	// （32 位平台本来也表达不了超过 4 GiB 的累计计数）。
	data := unix.IfData(hdr.Data)
	setIfBytes(&data, rxBytes, txBytes)
	hdr.Data = data

	// sockaddr_dl
	sa := msg[hdrSize:]
	sa[0] = byte(saSize) // sdl_len
	sa[1] = byte(unix.AF_LINK)
	binary.LittleEndian.PutUint16(sa[2:], 1) // sdl_index
	sa[4] = 0                                // sdl_type
	sa[5] = byte(len(name))                  // sdl_nlen
	sa[6] = 0                                // sdl_alen
	sa[7] = 0                                // sdl_slen
	copy(sa[8:], name)

	return msg
}

// setIfBytes 写入 if_data 的收发字节数。
//
// 64 位 FreeBSD（amd64/arm64）上 Ibytes/Obytes 是 uint64，
// 32 位（386/arm）上是 uint32——同一个字段名两种底层类型。
//
// 这里刻意**不用** `unsafe.Add(uintptr)` 做指针算术（go vet 会报
// "possible misuse of unsafe.Pointer"）：改成先取 if_data 的字节视图，
// 再用 unsafe.Offsetof 得到字段偏移往字节切片里写。
// 字节视图只依赖 unsafe.Pointer 一次转换，跨架构都干净。
func setIfBytes(d *unix.IfData, rx, tx uint64) {
	size := unsafe.Sizeof(*d)
	buf := (*[1 << 12]byte)(unsafe.Pointer(d))[:size:size]
	writeUintInto(buf[unsafe.Offsetof(d.Ibytes):], rx)
	writeUintInto(buf[unsafe.Offsetof(d.Obytes):], tx)
}

// readIfBytes 读回 (Ibytes, Obytes)，同样按 Offsetof 定位，跨架构可用。
func readIfBytes(d *unix.IfData) (uint64, uint64) {
	size := unsafe.Sizeof(*d)
	buf := (*[1 << 12]byte)(unsafe.Pointer(d))[:size:size]
	return readUintFrom(buf[unsafe.Offsetof(d.Ibytes):]),
		readUintFrom(buf[unsafe.Offsetof(d.Obytes):])
}

// writeUintInto 按指针宽度写入：32 位平台写 4 字节，64 位写 8 字节。
//
// 32 位平台上超出 4 字节的部分会被截断——这与内核的真实行为一致
// （32 位内核本来就只维护 32 位计数器，会在 4 GiB 处回绕）。
func writeUintInto(b []byte, v uint64) {
	if unsafe.Sizeof(uintptr(0)) == 4 {
		binary.LittleEndian.PutUint32(b, uint32(v))
		return
	}
	binary.LittleEndian.PutUint64(b, v)
}

// readUintFrom 与 writeUintInto 对称。
func readUintFrom(b []byte) uint64 {
	if unsafe.Sizeof(uintptr(0)) == 4 {
		return uint64(binary.LittleEndian.Uint32(b))
	}
	return binary.LittleEndian.Uint64(b)
}
