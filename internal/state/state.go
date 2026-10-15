// Package state 维护 Master 的内存运行态：节点最新状态、实时环形缓冲、
// 以及供公开 API 复用的内存快照。
// （目录/包名用 state 而非 runtime，避免与标准库 runtime 冲突。）
//
// 设计要点（4.2 / A9）：
//   - 最新状态只存在内存，不落盘；落盘的是聚合结果。
//   - 实时层为 30 分钟（≈600 点 @3s）的环形缓冲，每节点约 60 KB。
//   - 公开概览每 1~2 秒生成一次内存快照，供所有请求共用，避免重复计算。
package state

import (
	"sort"
	"sync"
	"time"

	"github.com/koyoca/xb-probe/internal/model"
)

// RingPoint 是实时层的一个采样点。
//
// MemUsed / DiskUsed 是**字节**，与 MemPct 并存而不是二选一：
// "最近 30 分钟"这条历史曲线走的是内存环形缓冲（不读时序表），而前端/公开接口
// 对 `mem`、`disk_used` 声明的单位都是 bytes（见 api.unitOf）。
// 曾经这里只有 MemPct，于是那一层：
//   - `disk_used` 没有对应字段，pickRing 落进 default 返回 0 —— 磁盘曲线在
//     "最近 30 分钟"下**永远是 0 B**（24 小时视图读时序表，所以看起来只坏了一档）；
//   - `mem` 返回的是百分比，却被当成字节画，坐标轴数字完全不对。
//
// 两个字段都是"业务上合法的 0"（刚开机的机器磁盘占用不会是 0，但空盘可以是），
// 所以**不能**用"是否为零"来判断有没有数据——判据是字段在不在。
type RingPoint struct {
	TS       int64
	CPU      int   // ×100
	MemPct   int   // ×100
	MemUsed  int64 // 字节
	DiskUsed int64 // 字节
	RxBps    int64
	TxBps    int64
}

// RingBuffer 是固定容量的环形缓冲，满时覆盖最旧数据。
// 并发安全由其持有者（NodeRuntime 的锁）保证。
type RingBuffer struct {
	buf  []RingPoint
	head int
	size int
}

// NewRingBuffer 按容量创建环形缓冲。
// 容量建议 = 实时层时长 / 上报间隔，例如 30*60/3 = 600。
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 600
	}
	return &RingBuffer{buf: make([]RingPoint, capacity)}
}

// Push 写入一个点；满时覆盖最旧。
func (r *RingBuffer) Push(p RingPoint) {
	r.buf[r.head] = p
	r.head = (r.head + 1) % len(r.buf)
	if r.size < len(r.buf) {
		r.size++
	}
}

// Since 返回 ts >= from 的点（按时间升序）。
func (r *RingBuffer) Since(from int64) []RingPoint {
	out := make([]RingPoint, 0, r.size)
	n := len(r.buf)
	start := (r.head - r.size + n) % n
	for i := 0; i < r.size; i++ {
		p := r.buf[(start+i)%n]
		if p.TS >= from {
			out = append(out, p)
		}
	}
	return out
}

// NodeRuntime 是单个节点的内存运行态。所有字段访问需持有 Registry 的锁。
type NodeRuntime struct {
	NodeID   int64
	PublicID string
	Name     string
	Group    string
	// Country 是节点的国家/地区代码（ISO 3166-1 alpha-2，空=未设置），
	// 供公开卡片显示国旗。与 HasIPv4/HasIPv6 同样的思路：只同步"展示所需的
	// 结构化取值"，不把管理端那套自由文本（location/provider）带进运行态。
	Country string
	// SortOrder 是管理员显式指定的展示顺序（nodes.sort_order）。
	// 相同值（默认全为 0）时回退到 NodeID 升序，保证顺序永远确定。
	SortOrder int

	// HasIPv4 / HasIPv6 表示该节点是否配置了对应版本的地址。
	//
	// 只保留"有没有"这个布尔量、不保存地址本身：公开看板要在节点卡片上显示
	// IPv4/IPv6 标识，而公开 DTO 按设计不得携带任何地址（dto_leak_test.go 以
	// 字段名子串做护栏）。把判断放在这里做掉，下游就没有任何机会误取到地址。
	HasIPv4 bool
	HasIPv6 bool

	Status string
	// PublicVisible 决定是否出现在公开看板（5.1）。
	PublicVisible bool
	// Archived 的节点停止告警与离线计时（4.1）。
	Archived bool

	LastSeen     int64 // 最近一次上报时间（Unix 秒）
	SuspectSince int64 // 疑似离线起算时间；0 表示未进入疑似态（4.1）

	// 最新指标与探测结果（用于实时展示与告警判定）。
	LatestMetrics model.Metrics
	LatestLatency []model.LatencySample

	// LatencyRound 在每次"用新一轮探测结果整体替换 LatestLatency"时自增。
	//
	// 存在的理由：LatestLatency 是**持久**的（只在拿到非空结果时整体替换），
	// 而调度器每 10 秒扫描一次、Agent 每 probe_interval_s（默认 30 秒）才探一轮，
	// 所以同一轮的失败会被扫描反复看到。而告警要按"轮"计数（4.5 的连续 N 次确认
	// 指的是 N 轮，不是 N 次扫描），因此需要一个"这是新的一轮还是同一轮"的凭证。
	//
	// 用自增计数器而不是时间戳：不受 Agent/Master 时钟跳变影响，
	// 也不存在"同一秒内跑了两轮"的歧义。进程重启后归零——
	// 消费方（调度器的 probeRounds 表）同样从空开始，不会跨重启比较。
	LatencyRound int64

	// LatencyBars 是节点卡片"延迟情况"点阵的数据（近 24 小时按运营组分段）。
	// 由调度器周期聚合（约 60 秒一次）整体替换；为 nil 表示还没聚合过，
	// 前端该渲染成"暂无数据"而不是空点阵。
	LatencyBars []model.PublicLatencyBar

	// 静态信息（仅在变化时更新）。
	OS          string
	Arch        string
	Virt        string
	CPUCores    int
	MemTotal    int64
	SwapTotal   int64
	DiskTotal   int64
	BootTime    int64
	CounterBits int

	// 该节点当前有效配置的摘要（13.4.3）。
	ConfigVersion string

	// --- 由调度器/计费扫描写入，供公开快照纯内存构建（A9）---
	TrafficQuota   int64 // 字节，0=不限
	TrafficUsed    int64 // 当前周期已用（字节，已按计量口径折算）
	TrafficUsedPct int   // 0~10000
	PeriodRx       int64 // 当前周期下载量（字节）
	PeriodTx       int64 // 当前周期上传量（字节）
	TodayRx        int64 // 今日下载量（字节，来自 traffic_daily）
	TodayTx        int64 // 今日上传量（字节）
	ExpireAt       int64 // 0 表示永久/未设置
	PriceMinor     int64 // 该支付周期的一次性金额（最小货币单位），供公开概览按月均折算
	Currency       string
	Cycle          string // monthly/quarterly/yearly/once/lifetime；空视作 monthly
	PricePublic    bool   // 管理员是否在白名单中放开价格（默认 false）
	PublicLatency  []model.PublicLatency

	Ring *RingBuffer
}

// Uptime 返回开机时长（秒）；无数据返回 0。
func (n *NodeRuntime) Uptime(now int64) int64 {
	if n.BootTime <= 0 || now <= n.BootTime {
		return 0
	}
	return now - n.BootTime
}

// Percent 计算 used/total 的万分比（0~10000），供进度条与超额判定。
//
// total<=0 表示"不限额/无基数" → 0；used<0 视为无数据 → 0；
// used>=total 一律 10000（前端把 10000 当满额，给更大的数没有意义）。
//
// ★ 这里刻意用 float64 而不是整数除法的 used*10000/total。后者在 used 超过
// 约 9.2e14（≈820 TB）时乘积溢出 int64 变成负数，而负值会**绕过**上面那条
// "超过 10000 就收敛"的判断，把一个负数百分比直接画到页面上。这个量级看起来
// 很远，但流量是累计值、倍率最高 1000，而且后台新增了「手工矫正本周期用量」
// 输入（上限 1 PiB）——它成了把 used 送进这个函数的最短路径。
//
// 三条调用路径（内存/磁盘、带宽、流量额度）共用这一份实现：分成两份时，
// 同一个数字会在"保存计费后立即重算"与"下一次慢循环"之间来回跳。
func Percent(used, total int64) int {
	if total <= 0 || used <= 0 {
		return 0
	}
	if used >= total {
		return 10000
	}
	return int(float64(used) / float64(total) * 10000)
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Registry 是所有节点运行态的容器。
type Registry struct {
	mu       sync.RWMutex
	byNode   map[int64]*NodeRuntime
	byPublic map[string]*NodeRuntime

	ringCap int
}

// NewRegistry 创建注册表；ringCap 为实时层容量（默认 600）。
func NewRegistry(ringCap int) *Registry {
	return &Registry{
		byNode:   make(map[int64]*NodeRuntime),
		byPublic: make(map[string]*NodeRuntime),
		ringCap:  ringCap,
	}
}

// GetOrCreate 获取或创建节点运行态。
func (g *Registry) GetOrCreate(nodeID int64, publicID, name string) *NodeRuntime {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n, ok := g.byNode[nodeID]; ok {
		if publicID != "" {
			n.PublicID = publicID
		}
		if name != "" {
			n.Name = name
		}
		return n
	}
	n := &NodeRuntime{
		NodeID:   nodeID,
		PublicID: publicID,
		Name:     name,
		Status:   model.StatusOffline,
		Ring:     NewRingBuffer(g.ringCap),
	}
	g.byNode[nodeID] = n
	if publicID != "" {
		g.byPublic[publicID] = n
	}
	return n
}

// clone 复制出一份可脱离锁安全读取的运行态。
//
// 标量字段直接值拷贝；LatestMetrics / LatestLatency / PublicLatency 这几个
// 切片与结构体在写入侧都是"整体替换"（n.X = 新值），不会原地修改，因此共享
// 底层数组是安全的。唯独 Ring 是原地 Push 的环形缓冲，必须置空——实时层请
// 走 Registry.RingPoints 在锁内取快照。
func (n *NodeRuntime) clone() NodeRuntime {
	c := *n
	c.Ring = nil
	return c
}

// Get 按节点 ID 获取运行态的一份只读副本。
//
// 返回副本而不是内部指针：调用方会脱离锁读取大量字段，直接返回指针等于把
// 未同步的字段暴露出去（写侧在报告路径上频繁整体赋值）。副本语义让"读一次
// 状态"变成一个原子动作，也让调用方不可能误改内部状态。
func (g *Registry) Get(nodeID int64) (*NodeRuntime, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	n, ok := g.byNode[nodeID]
	if !ok {
		return nil, false
	}
	c := n.clone()
	return &c, true
}

// GetByPublicID 按对外标识获取运行态副本。
func (g *Registry) GetByPublicID(publicID string) (*NodeRuntime, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	n, ok := g.byPublic[publicID]
	if !ok {
		return nil, false
	}
	c := n.clone()
	return &c, true
}

// RingPoints 返回某节点实时层中 ts >= from 的点（在锁内取快照）。
func (g *Registry) RingPoints(nodeID, from int64) ([]RingPoint, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	n, ok := g.byNode[nodeID]
	if !ok || n.Ring == nil {
		return nil, ok
	}
	return n.Ring.Since(from), true
}

// RingPointsByPublicID 同上，按对外标识。
func (g *Registry) RingPointsByPublicID(publicID string, from int64) ([]RingPoint, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	n, ok := g.byPublic[publicID]
	if !ok || n.Ring == nil {
		return nil, ok
	}
	return n.Ring.Since(from), true
}

// Update 在锁保护下对某节点执行修改函数。
// 上报处理与调度器都通过它修改运行态，避免数据竞争。
func (g *Registry) Update(nodeID int64, fn func(*NodeRuntime)) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	n, ok := g.byNode[nodeID]
	if !ok {
		return false
	}
	fn(n)
	return true
}

// Each 遍历所有节点运行态。
//
// 约束：fn 在持有读锁时被调用，**只允许读取**，不得调用 Registry 的其他方法
// （RWMutex 不可重入，会死锁），也不得修改传入的 NodeRuntime。需要修改时先
// 收集到局部切片，遍历结束后再用 Update 写回。
func (g *Registry) Each(fn func(*NodeRuntime)) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, n := range g.byNode {
		fn(n)
	}
}

// Remove 删除节点运行态（归档/删除时调用）。
func (g *Registry) Remove(nodeID int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n, ok := g.byNode[nodeID]; ok {
		delete(g.byPublic, n.PublicID)
		delete(g.byNode, nodeID)
	}
}

// SyncMeta 把库中的节点元数据同步进运行态
// （名称/分组/国家/对外标识/公开可见性/归档/IP 协议栈）。
//
// 为什么必须有它：运行态是 Agent 首次上报时通过 GetOrCreate 惰性创建的，
// 创建时并不知道库里的名称、分组与 public_visible（默认 false、PublicID 为空）。
// 若不主动同步，公开看板永远只能看到空列表——公开节点详情也永远 404。
//
// 本方法只写元数据，绝不触碰 LastSeen / Status / 指标，因此
// “是否上报过”仍然只能由 LastSeen 判定（管理端 /runtime 用它区分 404）。
//
// 返回 existed 表示调用前运行态中是否已有该节点。
func (g *Registry) SyncMeta(nodeID int64, publicID, name, group string, sortOrder int, visible, archived, hasV4, hasV6 bool, country string) (existed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	n, ok := g.byNode[nodeID]
	if !ok {
		g.byNode[nodeID] = &NodeRuntime{
			NodeID:        nodeID,
			PublicID:      publicID,
			Name:          name,
			Group:         group,
			Country:       country,
			SortOrder:     sortOrder,
			HasIPv4:       hasV4,
			HasIPv6:       hasV6,
			Status:        model.StatusOffline,
			PublicVisible: visible,
			Archived:      archived,
			Ring:          NewRingBuffer(g.ringCap),
		}
		if publicID != "" {
			g.byPublic[publicID] = g.byNode[nodeID]
		}
		return false
	}

	// 对外标识变更时重建 public 索引，避免旧键指向同一对象。
	if n.PublicID != publicID {
		if n.PublicID != "" {
			delete(g.byPublic, n.PublicID)
		}
		if publicID != "" {
			g.byPublic[publicID] = n
		}
		n.PublicID = publicID
	}
	n.Name = name
	n.Group = group
	n.Country = country
	n.SortOrder = sortOrder
	n.HasIPv4 = hasV4
	n.HasIPv6 = hasV6
	n.PublicVisible = visible
	n.Archived = archived
	return true
}

// StaticFacts 是节点的静态硬件/系统信息（落库后可用于运行态重建）。
type StaticFacts struct {
	OS          string
	Arch        string
	Virt        string
	CPUCores    int
	MemTotal    int64
	SwapTotal   int64
	DiskTotal   int64
	BootTime    int64
	CounterBits int
}

// RestoreStatic 把库中的静态信息补回运行态，只在运行态对应字段为空时写入。
//
// 为什么必须有它：Agent 只在**进程启动后的第一次上报**携带 static（3.1），
// 之后每次上报都只有 metrics。而 Master 重启后内存运行态是空的、不会从库里
// 自动恢复。两者叠加的后果是——只要 Master 重启而 Agent 没重启，
// CPU 核数、内存总量、磁盘总量、virt、arch 就会永久显示为 "—" / "0 B"，
// 公开看板和后台都一样，直到 Agent 也被重启一次。这既不是 Agent 的问题
// 也不是显示问题，是 Master 侧的恢复缺口。
//
// 采用"仅填空值"而不是直接覆盖：如果运行态里已经有值（说明该节点正在上报，
// 内存里的比库里的更新），就不该用库里的旧值去盖掉它。
func (g *Registry) RestoreStatic(nodeID int64, f StaticFacts) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	n, ok := g.byNode[nodeID]
	if !ok {
		return false
	}
	if n.OS == "" {
		n.OS = f.OS
	}
	if n.Arch == "" {
		n.Arch = f.Arch
	}
	if n.Virt == "" {
		n.Virt = f.Virt
	}
	if n.CPUCores == 0 {
		n.CPUCores = f.CPUCores
	}
	if n.MemTotal == 0 {
		n.MemTotal = f.MemTotal
	}
	if n.SwapTotal == 0 {
		n.SwapTotal = f.SwapTotal
	}
	if n.DiskTotal == 0 {
		n.DiskTotal = f.DiskTotal
	}
	if n.BootTime == 0 {
		n.BootTime = f.BootTime
	}
	if n.CounterBits == 0 {
		n.CounterBits = f.CounterBits
	}
	return true
}

// Prune 删除运行态中已不存在于库里的节点（配合 SyncMeta 使用，防快照出现幽灵节点）。
// keep 是当前库中仍存在的节点 ID 集合。
func (g *Registry) Prune(keep map[int64]bool) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	removed := 0
	for id, n := range g.byNode {
		if keep[id] {
			continue
		}
		delete(g.byPublic, n.PublicID)
		delete(g.byNode, id)
		removed++
	}
	return removed
}

// ---------------------------------------------------------------------------
// 公开快照（A9 / 13.4.8）
// ---------------------------------------------------------------------------

// Snapshot 是公开 API 复用的只读快照。
type Snapshot struct {
	GeneratedAt time.Time
	Overview    model.PublicOverview
	Nodes       []model.PublicNode
}

// Snapshotter 周期性生成公开快照，供所有公开请求共用。
type Snapshotter struct {
	registry *Registry

	// rates 返回当前生效的多币种汇率表。为 nil 时表示不换算
	// （等价于零值 model.Rates{}，与升级前行为一致）。
	//
	// 做成"每次 Refresh 现取"而不是启动时拍快照：管理员在后台改完主币种/
	// 汇率，看板的月费合计必须立刻跟着变——与 InstallMode 等运行时钩子同一口径。
	// 拍快照的话要重启才生效，而"改完没反应"正是本项目反复踩过的那类故障。
	rates func() model.Rates

	mu   sync.RWMutex
	last *Snapshot
}

// NewSnapshotter 创建快照器。
func NewSnapshotter(reg *Registry) *Snapshotter {
	return &Snapshotter{registry: reg}
}

// SetRatesProvider 注入多币种汇率来源（见字段 rates 的说明）。
func (s *Snapshotter) SetRatesProvider(f func() model.Rates) {
	s.rates = f
}

// currentRates 取当前汇率表；未注入时返回零值（不换算）。
func (s *Snapshotter) currentRates() model.Rates {
	if s.rates == nil {
		return model.Rates{}
	}
	return s.rates()
}

// lessNode 是节点展示顺序的唯一比较函数：先按管理员指定的 SortOrder，
// 再按 NodeID（创建顺序）兜底。必须全序且与状态无关，否则前端每秒重绘
// 就会出现卡片互相穿插。
func lessNode(a, b *NodeRuntime) bool {
	if a.SortOrder != b.SortOrder {
		return a.SortOrder < b.SortOrder
	}
	return a.NodeID < b.NodeID
}

// Refresh 重新生成快照。
//
// 注意：必须按 NodeID 显式排序后再构建。registry.byNode 是 map，Go 的 map
// 迭代顺序在每次遍历时都是随机的——不排序的话快照里的节点顺序每秒都在变，
// 前端每秒重绘就会看到卡片互相穿插、位置乱跳。这是"卡片动来动去"的根因。
// 排序键用 NodeID（创建顺序），它不随状态变化，因此卡片位置天然稳定。
func (s *Snapshotter) Refresh(at time.Time) {
	var nodes []*NodeRuntime
	var visible []*NodeRuntime
	s.registry.mu.RLock()
	for _, n := range s.registry.byNode {
		nodes = append(nodes, n)
		if n.PublicVisible && !n.Archived {
			visible = append(visible, n)
		}
	}
	s.registry.mu.RUnlock()

	sort.Slice(nodes, func(i, j int) bool { return lessNode(nodes[i], nodes[j]) })
	sort.Slice(visible, func(i, j int) bool { return lessNode(visible[i], visible[j]) })

	// 直接调用 state 内的唯一实现，不再用回调注入：
	// 注入点一旦漏设，快照会静默变成空的，线上表现为"看板一个节点都没有"，
	// 且没有任何报错。少一个可忘记的接线就少一类这种故障。
	snap := &Snapshot{GeneratedAt: at}
	snap.Overview = BuildPublicOverview(visible, at, s.currentRates())
	snap.Nodes = make([]model.PublicNode, 0, len(visible))
	for _, n := range visible {
		snap.Nodes = append(snap.Nodes, BuildPublicNode(n, at, false))
	}

	s.mu.Lock()
	s.last = snap
	s.mu.Unlock()
}

// Last 返回最近一次快照（可能为 nil，此时应同步生成一次）。
func (s *Snapshotter) Last() *Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last
}
