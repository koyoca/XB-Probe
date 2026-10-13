package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/koyoca/xb-probe/internal/alert"
	"github.com/koyoca/xb-probe/internal/model"
	"github.com/koyoca/xb-probe/internal/pipeline"
	"github.com/koyoca/xb-probe/internal/state"
	"github.com/koyoca/xb-probe/internal/store"
	"github.com/koyoca/xb-probe/internal/traffic"
)

// Scheduler 驱动状态机、告警扫描、计费刷新与数据清理（文档 4.1 / 4.5 / 4.2 清理 / 13.4.8）。

// 离线确认窗口（4.1）。
const offlineConfirmWindow = 30 * time.Second

// heartbeatInterval 是外部心跳的发送间隔（A5）。
//
// 抽成常量而不是内联在 Run 里，是因为后台状态卡要把它展示给管理员
// （"每 X 分钟一次"），两处必须用同一个值。
const heartbeatInterval = 5 * time.Minute

// 到期提醒档位（4.4）。
var expireTiers = []int{30, 7, 3, 1, 0}

// Scheduler 是后台调度器。
type Scheduler struct {
	repo     *store.Repo
	registry *state.Registry
	tracker  *traffic.Tracker
	writer   *pipeline.Writer
	multi    *pipeline.MultiResolution
	latency  *pipeline.LatencyMultiResolution
	alerts   *alert.Engine
	snap     *state.Snapshotter
	log      *slog.Logger

	stopCh   chan struct{}
	started  time.Time
	billings map[int64]store.BillingRow

	// targets 是探测目标的展示元数据缓存（id → 名称/运营商）。
	//
	// aggregateLatency 每秒都要跑一次，而它只有上报里的 target_id；
	// 没有这份缓存就只能把每个目标都标成 carrier=custom，
	// 看板的"三网延迟"会退化成一堆"自定义"。缓存由 syncNodeMeta（15 秒）刷新，
	// 热路径上不查库。
	targets map[int64]store.ProbeTargetMeta

	// hb 是外部心跳的运行态（只存内存，不落库）。
	//
	// 为什么不落库：这只是"最后一次成功/失败发生在什么时候"的瞬时状态，
	// 重启后重新计数即可；落库反而引入"重启后残留过期状态"的困惑
	// （比如库里写着 3 天前成功，但实际刚启动还没发过）。
	// 后台要展示的是"现在这一秒心跳是否通"，内存态足够且更准确。
	hb heartbeatState

	// lastBackupDay 记录"上一次自动备份的日期（YYYY-MM-DD，Master 本地时区）"。
	//
	// 用"日期"而不是"距上次的时间差"来做去重：调度器的小时 tick 在同一小时内
	// 可能因时钟调整而多跑一次，用时间差会在临界点重复备份；
	// 而"今天备过没有"是幂等的，重启后也能从备份文件名（内含日期）推断出来，
	// 不会因为重启就多备一份。
	lastBackupDay string

	// probeRounds 记录每个 (节点, 目标) 已经计过数的那一轮探测的**轮次号**
	// （state.NodeRuntime.LatencyRound），用来实现"按轮计数"（4.5 探测连续失败）。
	//
	// **为什么必须按轮去重**：Agent 每 probe_interval_s（默认 30 秒）才探一轮，
	// 而 n.LatestLatency 是持久的——applySample 只在 len(s.Latency)>0 时整体替换，
	// 所以同一轮的失败样本会在 alertTick（10 秒）上被反复看到。若每次扫描都
	// Observe 一次，默认 ConfirmN=3 会在 30 秒内（也就是**一轮**）就满足门槛，
	// "连续 3 轮确认"退化成"失败一次即告警"。按轮去重后计数单位才真是一轮探测。
	//
	// 与 hb 一样只由调度器单协程（Run 的 select 循环）读写，因此不加锁；
	// 条目由 pruneProbeRounds 按当前运行态裁剪，不会随"建过又删掉的节点/目标"无限增长。
	probeRounds map[probeKey]int64
}

// probeKey 是探测轮次去重表的键。
type probeKey struct {
	node   int64
	target int64
}

// heartbeatState 记录外部心跳的最近结果。
//
// 全部字段由 sendHeartbeat 在调度器单一协程内写入、由后台接口读取，
// 因此用互斥锁保护（后台读取来自 HTTP 协程）。
type heartbeatState struct {
	mu sync.Mutex

	// LastAttempt 是最后一次"尝试发送"的 Unix 秒（无论成败）。
	LastAttempt int64
	// LastOK 是最后一次成功（2xx）的 Unix 秒；0 表示从未成功过。
	LastOK int64
	// FailCount 是自上次成功以来的连续失败次数。
	FailCount int
	// LastStatus 是最后一次的 HTTP 状态码（网络错误时为 0）。
	LastStatus int
	// LastError 是最后一次的错误描述（成功时为空）。
	//
	// 注意：这里存的是**本地生成的**错误文本（"超时"/"连接被拒绝"等分支我们自己判定），
	// 或者是 Go 网络错误字符串。后者可能包含主机名/IP，但这是**管理员自己配置的
	// 心跳地址**，且只展示在需要登录的后台，不属于公开 DTO 的泄漏面。
	LastError string
	// Sent 是自进程启动以来累计的成功发送次数，用于粗看"心跳是否一直在跑"。
	Sent int64
}

// snapshot 返回一份拷贝，供 HTTP 协程读取。
func (h *heartbeatState) snapshot() heartbeatSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return heartbeatSnapshot{
		LastAttempt: h.LastAttempt,
		LastOK:      h.LastOK,
		FailCount:   h.FailCount,
		LastStatus:  h.LastStatus,
		LastError:   h.LastError,
		Sent:        h.Sent,
	}
}

// HeartbeatStatus 实现 model.HeartbeatReader，供管理后台读取心跳状态。
//
// 这里刻意**每次现读设置项**（而不是缓存），因为 heartbeat_url 是管理员
// 随时可改的；缓存了就会出现"刚配好但状态卡还显示未配置"的时序错乱。
// 读设置是单条 SQLite 查询，后台接口调用频率极低，开销可以忽略。
func (s *Scheduler) HeartbeatStatus() model.AdminHeartbeat {
	url := s.repo.GetSettingDefault("heartbeat_url", "")
	snap := s.hb.snapshot()

	out := model.AdminHeartbeat{
		Configured:  url != "",
		URL:         url,
		IntervalSec: int(heartbeatInterval / time.Second),
		LastAttempt: snap.LastAttempt,
		LastOK:      snap.LastOK,
		FailCount:   snap.FailCount,
		LastStatus:  snap.LastStatus,
		LastError:   snap.LastError,
		Sent:        snap.Sent,
	}
	// Healthy 的定义要谨慎：**不能**用 LastOK 是否在某个时间窗内来判断，
	// 因为进程刚启动时还没发过心跳（LastOK=0），那会被误判成"不健康"。
	// 正确口径是看"最近一次尝试是否成功"：
	//   - 未配置 -> false（功能没开，谈不上健康）
	//   - 配了但还没尝试过 -> true（启动瞬间的正常状态，不该报红）
	//   - 最近一次成功 -> true
	//   - 连续失败 >= 2 -> false（错过整整一个周期，外部监控大概率已经开始告警）
	switch {
	case url == "":
		out.Healthy = false
	case snap.LastAttempt == 0:
		out.Healthy = true // 还没到第一个心跳点（或刚启动），不算故障
	case snap.FailCount == 0:
		out.Healthy = true
	default:
		out.Healthy = false
	}
	return out
}

// heartbeatSnapshot 是 heartbeatState 的只读快照。
type heartbeatSnapshot struct {
	LastAttempt int64
	LastOK      int64
	FailCount   int
	LastStatus  int
	LastError   string
	Sent        int64
}

// recordOK 记录一次成功。
func (h *heartbeatState) recordOK(now int64, status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.LastAttempt = now
	h.LastOK = now
	h.FailCount = 0
	h.LastStatus = status
	h.LastError = ""
	h.Sent++
}

// recordFail 记录一次失败。
func (h *heartbeatState) recordFail(now int64, status int, errMsg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.LastAttempt = now
	h.FailCount++
	h.LastStatus = status
	h.LastError = errMsg
}

// NewScheduler 创建调度器。
func NewScheduler(repo *store.Repo, reg *state.Registry, tr *traffic.Tracker,
	w *pipeline.Writer, m *pipeline.MultiResolution, l *pipeline.LatencyMultiResolution,
	a *alert.Engine, snap *state.Snapshotter, log *slog.Logger) *Scheduler {
	return &Scheduler{
		repo: repo, registry: reg, tracker: tr, writer: w, multi: m, latency: l,
		alerts: a, snap: snap, log: log,
		stopCh:   make(chan struct{}),
		started:  time.Now(),
		billings: map[int64]store.BillingRow{},
		targets:  map[int64]store.ProbeTargetMeta{},

		probeRounds: map[probeKey]int64{},
	}
}

// Run 启动调度循环。
func (s *Scheduler) Run(ctx context.Context) {
	snapTick := time.NewTicker(1 * time.Second)   // A9：公开快照 1~2 秒刷新
	alertTick := time.NewTicker(10 * time.Second) // 4.5：调度器每 10 秒扫描
	metaTick := time.NewTicker(15 * time.Second)  // 节点元数据同步（名称/分组/公开可见性）
	slowTick := time.NewTicker(60 * time.Second)  // 计费与流量刷新
	hourTick := time.NewTicker(1 * time.Hour)     // 清理
	hbTick := time.NewTicker(heartbeatInterval)   // A5：外部心跳
	defer func() {
		snapTick.Stop()
		alertTick.Stop()
		metaTick.Stop()
		slowTick.Stop()
		hourTick.Stop()
		hbTick.Stop()
	}()

	// 启动即同步一次：否则重启后内存运行态为空，公开看板会空到第一次 Agent 上报为止。
	s.syncNodeMeta()
	s.refreshBilling()
	s.refreshTodayTraffic(time.Now())
	s.syncLatencyBars(time.Now())
	s.snapshot(time.Now())
	// 外部心跳也立刻打一次：Master 刚起来时最需要告诉外界"我还活着"，
	// 否则最长要等 5 分钟才发第一个心跳。
	s.sendHeartbeat(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-snapTick.C:
			s.snapshot(time.Now())
		case <-alertTick.C:
			s.scanAlerts(ctx, time.Now())
		case <-metaTick.C:
			s.syncNodeMeta()
		case <-slowTick.C:
			s.syncNodeMeta()
			s.refreshBilling()
			s.refreshRuntimeBilling()
			s.refreshTodayTraffic(time.Now())
			s.syncLatencyBars(time.Now())
			s.snapshot(time.Now())
		case <-hbTick.C:
			s.sendHeartbeat(ctx)
		case <-hourTick.C:
			s.cleanup(time.Now())
		}
	}
}

// sendHeartbeat 实现 A5：每 5 分钟向 heartbeat_url 发一次 GET。
//
// 设计要点（对照 A5 与 7.5）：
//   - **绝不阻塞主循环**：整体超时 10 秒，失败只记一条 Warn 日志，不重试、不排队；
//     "Master 还活着"的信号本来就是"每隔 5 分钟就有一个请求"，
//     失败重试反而会让外部监控看到杂乱的到达时间。
//   - 这是**唯一**的 Master 出站请求，所以必须受同样的安全约束：
//     只允许 https（settings 里的 optionalHTTPSURL 校验器已经在保存时就拒绝 http），
//     且不携带任何节点信息、不带 Token、不带查询参数——只发一个裸 GET。
//   - 未配置时静默跳过（不算错误）。
//
// 每次结果都写入 s.hb，供后台"外部心跳"状态卡读取——只写日志的话，
// 管理员配完根本不知道到底通不通（这是补可观测性的原因）。
func (s *Scheduler) sendHeartbeat(ctx context.Context) {
	now := time.Now().Unix()

	url := s.repo.GetSettingDefault("heartbeat_url", "")
	if url == "" {
		// 未配置不算失败，不污染状态（否则后台会显示"连续失败 N 次"而无从解释）。
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		// 地址本身非法（比如用户直接改了库）。这属于配置错误，计入失败以便后台暴露。
		s.hb.recordFail(now, 0, "心跳地址非法："+err.Error())
		s.log.Warn("外部心跳请求构造失败", "err", err.Error())
		return
	}
	// 明确声明不缓存：外部监控服务（healthchecks.io / uptime-kuma 等）依赖真实到达。
	req.Header.Set("User-Agent", "xb-probe-master/heartbeat")
	req.Header.Set("Cache-Control", "no-store")

	client := s.heartbeatClient()
	resp, err := client.Do(req)
	if err != nil {
		s.hb.recordFail(now, 0, err.Error())
		s.log.Warn("外部心跳发送失败（不影响监控功能）", "err", err.Error())
		return
	}
	defer resp.Body.Close()
	// 读掉少量 body 以便连接复用，同时避免服务端因为不读而卡住。
	_, _ = io.CopyN(io.Discard, resp.Body, 4<<10)

	if resp.StatusCode >= 400 {
		s.hb.recordFail(now, resp.StatusCode, fmt.Sprintf("HTTP %d", resp.StatusCode))
		s.log.Warn("外部心跳被拒绝", "status", resp.StatusCode)
		return
	}
	s.hb.recordOK(now, resp.StatusCode)
	s.log.Debug("外部心跳已发送", "status", resp.StatusCode)
}

// heartbeatClient 返回外部心跳用的 HTTP 客户端。
//
// 每次新建而不是复用全局 client：心跳 5 分钟一次，连接复用收益可以忽略；
// 而独立 client 能确保超时与 TLS 设置不会被别处的默认值改掉。
func (s *Scheduler) heartbeatClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 允许最多 3 次跳转，但降级到 http 一律拒绝（防止 https→http 降级）。
			if len(via) >= 3 {
				return fmt.Errorf("心跳地址跳转次数过多")
			}
			if req.URL.Scheme != "https" && !isLoopbackHost(req.URL.Hostname()) {
				return fmt.Errorf("心跳地址不允许跳转到非 https")
			}
			return nil
		},
	}
}

// isLoopbackHost 判断是否本机地址（本机调试允许 http）。
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// offlineTimeout 计算“多久没有上报即视为离线”（4.1）。
// 默认取 max(30s, 10 × report_interval_s)；设置项 offline_timeout_s 可显式覆盖。
func (s *Scheduler) offlineTimeout() int64 {
	if v := s.repo.GetIntSetting("offline_timeout_s", 0); v >= 10 {
		return v
	}
	timeout := int64(30)
	if v := 10 * s.repo.GetIntSetting("report_interval_s", 3); v > 30 {
		timeout = v
	}
	return timeout
}

// syncLatencyBars 聚合节点卡片"延迟情况"点阵的数据（按 public.lat_window
// 选定的时间窗分段，默认近 24 小时）。
//
// 为什么放在 60 秒的慢循环而不是快照构建里：快照每秒重建且纯内存，
// 在里面查库会把"看板刷新"和"磁盘负载"直接绑死。点阵对实时性不敏感——
// 每个点本来就是十几分钟到 45 分钟的均值，晚一分钟更新毫无感知。
// 窗口 ≤1h 时数据源是 latency_raw（探测粒度），更长的窗口用 latency_10m；
// 两层都能用是因为 LatencyMultiResolution 双写（7d/30d 曲线修复的前置条件）。
func (s *Scheduler) syncLatencyBars(now time.Time) {
	lay := state.LatencyBarLayout(s.repo.GetSettingDefault("public.lat_window", "24h"))
	nodes, err := s.repo.ListNodes()
	if err != nil {
		s.log.Error("读取节点列表失败（延迟点阵）", "err", err.Error())
		return
	}
	from := lay.ReadFrom(now.Unix())
	for _, n := range nodes {
		if n.ArchivedAt > 0 || !n.PublicVisible {
			continue
		}
		var buckets []model.LatencyBucket
		var err error
		if lay.Raw {
			buckets, err = s.repo.LatencyRawBucketsForNode(n.ID, from)
		} else {
			buckets, err = s.repo.LatencyBucketsForNode(n.ID, from)
		}
		if err != nil {
			s.log.Error("读取延迟桶失败", "node", n.ID, "err", err.Error())
			continue
		}
		bars := state.BuildLatencyBars(buckets, now.Unix(), lay.WindowS, lay.Segments)
		_ = s.registry.Update(n.ID, func(rt *state.NodeRuntime) {
			rt.LatencyBars = bars
		})
	}
}

// syncNodeMeta 把库中的节点元数据同步进内存运行态，并清掉库里已删除的幽灵节点。
//
// 运行态是 Agent 首次上报时才惰性创建的，创建时不知道名称/分组/public_visible，
// 因此这一步是公开看板与节点名称能正确显示的前提（详见 state.Registry.SyncMeta）。
func (s *Scheduler) syncNodeMeta() {
	nodes, err := s.repo.ListNodes()
	if err != nil {
		s.log.Error("同步节点元数据失败", "err", err.Error())
		return
	}
	keep := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		keep[n.ID] = true
		s.registry.SyncMeta(n.ID, n.PublicID, n.Name, n.GroupName, n.SortOrder, n.PublicVisible, n.ArchivedAt > 0, n.IPv4 != "", n.IPv6 != "", n.Country)
		// 把库里存的静态信息补回运行态。Agent 只在进程启动后的第一次上报带
		// static，Master 重启后若不主动恢复，CPU 核数/内存总量/磁盘总量/virt/arch
		// 就会一直是空的（详见 state.Registry.RestoreStatic）。
		s.registry.RestoreStatic(n.ID, state.StaticFacts{
			OS:          n.OS,
			Arch:        n.Arch,
			Virt:        n.Virt,
			CPUCores:    n.CPUCores,
			MemTotal:    n.MemTotal,
			SwapTotal:   n.SwapTotal,
			DiskTotal:   n.DiskTotal,
			BootTime:    n.BootTime,
			CounterBits: n.CounterBits,
		})
	}
	if removed := s.registry.Prune(keep); removed > 0 {
		s.log.Info("清理了库中已不存在的运行态", "count", removed)
	}

	// 同一个 15 秒周期里刷新探测目标元数据缓存（延迟汇总要用）。
	if metas, err := s.repo.ListAllProbeTargetMeta(); err != nil {
		s.log.Error("同步探测目标元数据失败", "err", err.Error())
	} else {
		s.targets = metas
	}
}

// Stop 停止调度器。
func (s *Scheduler) Stop() { close(s.stopCh) }

// snapshot 生成公开快照前先聚合三网延迟。
func (s *Scheduler) snapshot(now time.Time) {
	s.aggregateLatency()
	s.snap.Refresh(now)
}

// aggregateLatency 把最新探测结果按运营商汇总为公开指示灯。
//
// 先在 Each（读锁）里只收集结果，遍历结束后再用 Update 写回：RWMutex 不可
// 重入，且 Each 内写字段会与并发的上报路径构成数据竞争。
//
// 汇总口径：
//   - 同一个运营商下的多个目标取平均 RTT（只统计探通的目标，未探通的不参与平均，
//     否则一个有故障的目标会把整条"电信"拉成红色，掩盖其余目标其实正常）；
//   - 丢包与状态取**最差**值，宁可高报不可漏报；
//   - 输出按运营商固定排序。这里必须自己排序：Go 的 map 遍历顺序是随机的，
//     每秒一次的刷新若输出乱序，公开看板上这一行就会持续跳动。
func (s *Scheduler) aggregateLatency() {
	// 载波顺序固定，保证输出稳定（见上面的注释）。
	order := []string{"telecom", "unicom", "mobile", "custom"}

	type pending struct {
		nodeID int64
		out    []model.PublicLatency
	}
	var queue []pending

	s.registry.Each(func(n *state.NodeRuntime) {
		if len(n.LatestLatency) == 0 {
			return
		}
		agg := map[string]*model.PublicLatency{}
		for _, l := range n.LatestLatency {
			carrier := "custom"
			if m, ok := s.targets[int64(l.TargetID)]; ok && m.Carrier != "" {
				carrier = m.Carrier
			}
			a := agg[carrier]
			if a == nil {
				a = &model.PublicLatency{Carrier: carrier}
				agg[carrier] = a
			}
			a.Targets++
			if l.Loss > a.Loss {
				a.Loss = l.Loss
			}
			if st := model.LatencyStatus(l.RTTAvg, l.Error); latencyRank(st) > latencyRank(a.Status) {
				a.Status = st
			}
			if l.RTTAvg != nil {
				if a.RTTAvg == nil {
					v := *l.RTTAvg
					a.RTTAvg = &v
				} else {
					v := (*a.RTTAvg + *l.RTTAvg) / 2
					a.RTTAvg = &v
				}
			}
		}
		out := make([]model.PublicLatency, 0, len(agg))
		for _, c := range order {
			if a, ok := agg[c]; ok {
				out = append(out, *a)
			}
		}
		queue = append(queue, pending{nodeID: n.NodeID, out: out})
	})

	for _, p := range queue {
		_ = s.registry.Update(p.nodeID, func(n *state.NodeRuntime) { n.PublicLatency = p.out })
	}
}

// latencyRank 把状态映射成"严重程度"，用于取最差。
// unknown（探测出错）比 bad（探通但很慢）更严重：前者意味着数据不可信。
func latencyRank(status string) int {
	switch status {
	case model.LatencyUnknown:
		return 3
	case model.LatencyBad:
		return 2
	case model.LatencyWarn:
		return 1
	case model.LatencyOK:
		return 0
	default:
		return -1
	}
}

// scanAlerts 执行离线判定与指标阈值判定（4.1 / 4.5）。
func (s *Scheduler) scanAlerts(ctx context.Context, now time.Time) {
	timeout := s.offlineTimeout()
	// Master 重启宽限期：2 × 超时阈值（4.1）
	if now.Sub(s.started) < time.Duration(2*timeout)*time.Second {
		return
	}

	nowUnix := now.Unix()
	rules, _ := s.repo.LoadAlertRules()

	// 先在读锁内取一份运行态副本，随后所有判定、告警派发都在锁外进行。
	// 两个原因：① Each 内写字段会与上报路径构成数据竞争；② 告警派发要走
	// Telegram/SMTP 网络 I/O 与数据库读写，持锁执行会把每个 Agent 上报
	// （需要写锁）全部阻塞住。
	var nodes []state.NodeRuntime
	s.registry.Each(func(n *state.NodeRuntime) { nodes = append(nodes, *n) })

	type statusChange struct {
		status     string
		suspect    int64
		hasSuspect bool
	}
	changes := make(map[int64]statusChange, len(nodes))

	for i := range nodes {
		n := &nodes[i]
		if n.Archived {
			continue
		}
		// 从未上报过的节点（刚建好、Agent 还没装）不参与离线判定，
		// 否则每新建一个节点都会在 30 秒后被误报为"已离线"。
		if n.LastSeen == 0 {
			continue
		}

		newStatus := n.Status
		newSuspect := n.SuspectSince
		suspectSet := false

		// --- 离线判定 ---
		stale := nowUnix-n.LastSeen > timeout
		switch {
		case stale && n.SuspectSince == 0:
			newSuspect = nowUnix
			suspectSet = true
			newStatus = model.StatusWarning // 疑似离线：过滤网络抖动
		case stale && nowUnix-n.SuspectSince > int64(offlineConfirmWindow.Seconds()):
			if s.alerts.Observe(n.NodeID, alert.KindOffline, nowUnix) {
				newStatus = model.StatusOffline
				_ = s.alerts.Fire(ctx, n.NodeID, alert.KindOffline, alert.KindOffline,
					alert.LevelError, fmt.Sprintf("已离线（超过 %d 秒无上报）", timeout), nowUnix)
			}
		default:
			if !stale {
				// 只要不再 stale，就说明上报恢复了，状态必须回到 online。
				//
				// 这里曾经写成 `if n.Status == model.StatusOffline`，漏掉了
				// warning：节点短暂抖动会进入 warning（疑似离线），此后即使每 3
				// 秒都在正常上报，状态也永远停在 warning —— 看板上一个全绿的
				// 节点会永久显示成橙色。warning 是需要能自己走出来的中间态，
				// 不是终态。
				if n.Status != model.StatusOnline {
					if n.Status == model.StatusOffline {
						_ = s.alerts.Recover(ctx, n.NodeID, alert.KindOffline, "恢复上报", nowUnix)
					}
					newStatus = model.StatusOnline
				}
				s.alerts.Clear(n.NodeID, alert.KindOffline)
				newSuspect = 0
				suspectSet = true
			}
		}

		if newStatus != n.Status || suspectSet {
			changes[n.NodeID] = statusChange{status: newStatus, suspect: newSuspect, hasSuspect: suspectSet}
		}

		// --- 指标阈值 ---
		s.checkMetric(ctx, rules, n, alert.KindCPU, derefInt(n.LatestMetrics.CPU), nowUnix)
		s.checkMetric(ctx, rules, n, alert.KindMem,
			state.Percent(deref64(n.LatestMetrics.MemUsed), n.MemTotal), nowUnix)
		s.checkMetric(ctx, rules, n, alert.KindDisk,
			state.Percent(deref64(n.LatestMetrics.DiskUsed), n.DiskTotal), nowUnix)

		// --- 流量梯级 ---
		s.checkTrafficTiers(ctx, rules, n, nowUnix)

		// --- 探测连续失败（每个目标一个状态键）---
		s.checkProbeFail(ctx, rules, n, nowUnix)
	}

	// 裁剪探测轮次去重表：按当前运行态里"仍在上报的目标"定界。
	// 放在循环外一次做完，避免每个节点各扫一遍整张表。
	s.pruneProbeRounds(nodes)

	// 结论一次性写回，避免每个节点加一次锁。
	for id, c := range changes {
		_ = s.registry.Update(id, func(n *state.NodeRuntime) {
			if c.status != "" {
				n.Status = c.status
			}
			if c.hasSuspect {
				n.SuspectSince = c.suspect
			}
		})
	}
}

func (s *Scheduler) checkMetric(ctx context.Context, rules map[string]alert.Rule,
	n *state.NodeRuntime, kind string, value int, now int64) {
	rule, ok := rules[kind]
	if !ok || !rule.Enabled {
		return
	}
	threshold := rule.Params.Threshold
	if threshold == 0 {
		threshold = 9000
	}
	if value >= threshold {
		if s.alerts.Observe(n.NodeID, kind, now) {
			_ = s.alerts.Fire(ctx, n.NodeID, kind, kind, alert.LevelWarn,
				fmt.Sprintf("%s 超过阈值（%.1f%% ≥ %.1f%%）", kind, float64(value)/100, float64(threshold)/100), now)
		}
		return
	}
	s.alerts.Clear(n.NodeID, kind)
	if st, err := s.repo.GetAlertState(n.NodeID, kind); err == nil && st != nil && st.State == "firing" {
		_ = s.alerts.Recover(ctx, n.NodeID, kind, fmt.Sprintf("%s 恢复正常", kind), now)
	}
}

func (s *Scheduler) checkTrafficTiers(ctx context.Context, rules map[string]alert.Rule,
	n *state.NodeRuntime, now int64) {
	rule, ok := rules[alert.KindTraffic]
	if !ok || !rule.Enabled {
		return
	}
	b, ok := s.billings[n.NodeID]
	if !ok || b.TrafficQuota <= 0 {
		return
	}
	rx, tx, tracked := s.tracker.Used(n.NodeID)
	if !tracked {
		return
	}
	used := traffic.Billable(rx, tx, b.TrafficMode, b.TrafficMultiplier)
	pct := int(used * 100 / b.TrafficQuota)

	levels := rule.Params.Levels
	if len(levels) == 0 {
		levels = []int{80, 95, 100}
	}
	for _, lv := range levels {
		stateKind := fmt.Sprintf("traffic:%d", lv)
		if pct >= lv {
			_ = s.alerts.Fire(ctx, n.NodeID, alert.KindTraffic, stateKind, alert.LevelWarn,
				fmt.Sprintf("流量已达 %d%%（%d 档）", pct, lv), now)
		} else if st, err := s.repo.GetAlertState(n.NodeID, stateKind); err == nil && st != nil && st.State == "firing" {
			_ = s.alerts.Recover(ctx, n.NodeID, stateKind, "流量回落", now)
		}
	}
}

// checkProbeFail 判定"探测连续失败"（4.5）。
//
// 设计要点（这几条都是"看起来更简单、但会坏"的写法，改动前先读）：
//
//  1. **粒度是每个目标**，状态键 probe_fail:<target_id>。用一个笼统的 probe_fail
//     只能得到"这节点有点问题"；按目标分开才能说清"上海电信挂了、北京联通好着"，
//     冷却窗口也天然按 (节点, 目标) 独立——否则一个目标恢复会把另一个的冷却一起重置。
//  2. **状态键与规则键分开传**（ObserveKind）：复合键在规则表里不存在。
//  3. **没有探测数据 ≠ 探测恢复**。Agent 只在探到结果时上报，LatestLatency 为空
//     意味着"这轮没有可判定的数据"（节点没配目标、刚注册还没跑第一轮）。
//     下面的循环只遍历上报里出现过的目标，所以"空"天然不会触发任何 Clear——
//     这正是要的语义。**反过来说，任何"按库里的目标清单去判、不在清单里就当恢复"
//     的改法都是回归**：它会把每一次上报间隙都当成恢复，连续计数永远清到 0。
//  4. **按轮去重**（见 probeRounds 字段注释），否则一轮失败就够触发。
//  5. Fire 是同步调用（内部要走渠道 I/O），所以状态判定与去重都在 Fire 之前完成，
//     每个目标每轮最多 Fire 一次。
func (s *Scheduler) checkProbeFail(ctx context.Context, rules map[string]alert.Rule,
	n *state.NodeRuntime, now int64) {
	rule, ok := rules[alert.KindProbeFail]
	if !ok || !rule.Enabled {
		return
	}
	confirm := rule.Params.ConfirmN
	if confirm <= 0 {
		confirm = 1
	}

	for _, l := range n.LatestLatency {
		stateKind := fmt.Sprintf("%s:%d", alert.KindProbeFail, l.TargetID)

		if !probeFailed(l) {
			// 探通了（或这一条不属于"目标不通"）→ 计数归零，已告警过的发恢复通知。
			s.alerts.Clear(n.NodeID, stateKind)
			if st, err := s.repo.GetAlertState(n.NodeID, stateKind); err == nil &&
				st != nil && st.State == "firing" {
				_ = s.alerts.Recover(ctx, n.NodeID, stateKind, "探测已恢复", now)
			}
			continue
		}

		key := probeKey{node: n.NodeID, target: int64(l.TargetID)}
		// 用节点级的轮次号判重：相等即"同一轮探测，之前那次扫描已经计过数了"。
		// 用 == 而不是 <=：轮次号在单进程内单调，等号已经够用；万一遇到异常回退，
		// 宁可多计一次（告警偏早）也不要永远计不上（告警永不触发）。
		if last, ok := s.probeRounds[key]; ok && last == n.LatencyRound {
			continue
		}
		s.probeRounds[key] = n.LatencyRound

		if s.alerts.ObserveKind(n.NodeID, alert.KindProbeFail, stateKind, now) {
			_ = s.alerts.Fire(ctx, n.NodeID, alert.KindProbeFail, stateKind, alert.LevelWarn,
				fmt.Sprintf("探测目标「%s」连续 %d 轮失败（%s）",
					s.targetName(l.TargetID), confirm, l.Error), now)
		}
	}
}

// probeFailed 判断一条探测结果是否算"目标不通"。
//
// 两类错误被排除，它们**不是**"目标侧或链路坏了"，而是 Agent 侧的策略/权限结论：
//   - icmp_not_permitted：Agent 没有 raw socket 权限（容器与低权限服务账号很常见），
//     语义是"没测成"，不是"测到了不通"。算进去会让所有无特权节点永远在报警，
//     而且这种告警是真的在报，管理员无法与真故障区分。
//   - blocked_target：S4 的拨号层防护生效（目标解析到内网/回环/云元数据地址）。
//     这是目标配置错误，探测卡片上已原样显示该错误码；当作"连续失败"报警只会误导。
//
// 其余枚举（timeout/refused/unreachable/dns_failed/other）都算失败：
// 例如 TCP 探测被 refused 说明端口上的服务确实没了，是必须被看见的故障。
func probeFailed(l model.LatencySample) bool {
	if l.Error == "" {
		return false
	}
	switch l.Error {
	case model.ProbeErrICMPNotPermitted, model.ProbeErrBlockedTarget:
		return false
	}
	return true
}

// targetName 取探测目标的展示名；元数据缓存还没刷新过时退回 "目标 #id"。
//
// 缓存由 syncNodeMeta（15 秒）刷新，所以刚建成、还没同步到缓存的目标会短暂
// 显示成 "目标 #id"（后台「最新一轮探测」也走同一份缓存，口径一致）。
func (s *Scheduler) targetName(targetID int) string {
	if m, ok := s.targets[int64(targetID)]; ok && m.Name != "" {
		return m.Name
	}
	return fmt.Sprintf("目标 #%d", targetID)
}

// pruneProbeRounds 裁剪探测轮次去重表。
//
// 判活标准是"该目标是否仍出现在节点的最新一轮上报里"：
// LatestLatency 只在拿到非空结果时被整体替换，因此"目标不在了"就意味着它被停用/删除，
// 或者节点已经不再上报它——两种情况都应该把残留的 TS 记忆一并丢掉。
// 不做裁剪的话，这张表会随"建过又删掉的目标"缓慢增长（每节点目标数上限 30，
// 但历史上出现过的目标 id 是无上限的）。
func (s *Scheduler) pruneProbeRounds(nodes []state.NodeRuntime) {
	if len(s.probeRounds) == 0 {
		return
	}
	live := make(map[probeKey]bool, len(s.probeRounds))
	for i := range nodes {
		for _, l := range nodes[i].LatestLatency {
			live[probeKey{node: nodes[i].NodeID, target: int64(l.TargetID)}] = true
		}
	}
	for k := range s.probeRounds {
		if !live[k] {
			delete(s.probeRounds, k)
		}
	}
}

// refreshBilling 载入计费信息并刷新内存运行态。
func (s *Scheduler) refreshBilling() {
	rows, err := s.repo.ListExpiringNodes()
	if err != nil {
		return
	}
	m := make(map[int64]store.BillingRow, len(rows))
	for _, b := range rows {
		m[b.NodeID] = b
	}
	// 同时纳入永久节点（不参与到期扫描，但仍需额度信息）。
	nodes, err := s.repo.ListNodes()
	if err == nil {
		for _, n := range nodes {
			if _, ok := m[n.ID]; ok {
				continue
			}
			if b, err := s.repo.GetBilling(n.ID); err == nil && b != nil {
				m[n.ID] = *b
			}
		}
	}
	s.billings = m
	s.refreshRuntimeBilling()
	s.scanExpiry()
}

// refreshRuntimeBilling 把额度与到期信息写入运行态（供公开快照纯内存构建，A9）。
func (s *Scheduler) refreshRuntimeBilling() {
	for nodeID, b := range s.billings {
		rx, tx, _ := s.tracker.Used(nodeID)
		// 计费口径 = 计量口径(traffic_mode) × 流量倍率（4.4）。
		// PeriodRx/PeriodTx 保持原始计量值不变：看板上的"周期流量 ↓↑"要的是
		// 真实进出量，只有进度条与额度比较才需要倍率。
		used := traffic.Billable(rx, tx, b.TrafficMode, b.TrafficMultiplier)
		_ = s.registry.Update(nodeID, func(n *state.NodeRuntime) {
			n.TrafficQuota = b.TrafficQuota
			n.TrafficUsed = used
			n.PeriodRx = rx
			n.PeriodTx = tx
			n.PriceMinor = b.PriceMinor
			n.Currency = b.Currency
			n.Cycle = b.Cycle
			if b.Cycle == model.CycleLifetime {
				n.ExpireAt = 0
			} else {
				n.ExpireAt = b.ExpireAt
			}
			// 百分比与"保存计费后立即重算"那条路共用 state.Percent，
			// 否则同一个节点在两处会显示两个不同的百分比。
			n.TrafficUsedPct = state.Percent(used, b.TrafficQuota)
		})
	}
}

// refreshTodayTraffic 把各节点"今天"的下载/上传量写入运行态。
//
// 用于公开看板的"今日流量"卡片。要点：
//   - 各节点时区可能不同（node_billing.tz，回退到全局 timezone 设置），
//     "今天"不是同一天，所以不能在 SQL 里按单一日期过滤；这里取昨天的窗口
//     一次性读回（覆盖 ±1 天时区偏移），再按每个节点自己的时区挑当日行。
//   - 单条查询而不是按节点循环，避免节点多时出现 N+1。
//   - 只在 60 秒的慢循环里跑，不参与每秒的快照刷新。
func (s *Scheduler) refreshTodayTraffic(now time.Time) {
	since := now.AddDate(0, 0, -1).Format("2006-01-02")
	rows, err := s.repo.QueryTrafficDailySince(since)
	if err != nil {
		s.log.Error("查询今日流量失败", "err", err.Error())
		return
	}
	byNode := make(map[int64]map[string]store.TrafficDailyRow, len(rows))
	for _, row := range rows {
		m := byNode[row.NodeID]
		if m == nil {
			m = make(map[string]store.TrafficDailyRow, 2)
			byNode[row.NodeID] = m
		}
		m[row.Day] = row
	}

	globalTZ := s.repo.GetSettingDefault("timezone", "")

	// 先在读锁内算出每个节点的目标值，再统一写回（Each 内不得调用 Update）。
	type dayPair struct {
		nodeID int64
		rx, tx int64
	}
	var want []dayPair
	s.registry.Each(func(n *state.NodeRuntime) {
		tz := ""
		if b, ok := s.billings[n.NodeID]; ok {
			tz = b.TZ
		}
		if tz == "" {
			tz = globalTZ
		}
		var rx, tx int64
		if m, ok := byNode[n.NodeID]; ok {
			if r, ok := m[traffic.LocalDay(now, tz)]; ok {
				rx, tx = r.Rx, r.Tx
			}
		}
		if rx != n.TodayRx || tx != n.TodayTx {
			want = append(want, dayPair{nodeID: n.NodeID, rx: rx, tx: tx})
		}
	})
	for _, p := range want {
		_ = s.registry.Update(p.nodeID, func(n *state.NodeRuntime) {
			n.TodayRx = p.rx
			n.TodayTx = p.tx
		})
	}
}

// scanExpiry 到期提醒：默认 30、7、3、1 天与到期当天，每档只发一次（4.4）。
func (s *Scheduler) scanExpiry() {
	now := time.Now()
	nowUnix := now.Unix()
	ctx := context.Background()

	for nodeID, b := range s.billings {
		if b.Cycle == model.CycleLifetime || b.ExpireAt == 0 {
			continue
		}
		days := traffic.DaysUntil(b.ExpireAt, nowUnix)
		for _, tier := range expireTiers {
			stateKind := fmt.Sprintf("expire:%d", tier)
			if days <= tier {
				_ = s.alerts.Fire(ctx, nodeID, alert.KindExpire, stateKind, alert.LevelWarn,
					fmt.Sprintf("距到期还有 %d 天", days), nowUnix)
			}
		}
	}
}

// cleanup 数据清理：过期时序、网卡基线、WAL 截断（4.2 / 13.4.1）。
func (s *Scheduler) cleanup(now time.Time) {
	rawHours := int(s.repo.GetIntSetting("retention.raw_hours", 24))
	midDays := int(s.repo.GetIntSetting("retention.mid_days", 7))
	totalDays := int(s.repo.GetIntSetting("retention.days", 30))

	if err := s.repo.CleanupTimeSeries(now, rawHours, midDays, totalDays); err != nil {
		s.log.Error("时序清理失败", "err", err.Error())
	}
	if _, err := s.repo.CleanupNICBaselines(now.Unix()); err != nil {
		s.log.Error("网卡基线清理失败", "err", err.Error())
	}
	if _, err := s.repo.CleanupSessions(now.Unix()); err != nil {
		s.log.Error("会话清理失败", "err", err.Error())
	}
	// 每日 WAL 截断与增量 vacuum（4.2）
	if now.Hour() == 4 {
		if err := s.repo.CheckpointWAL(); err != nil {
			s.log.Error("WAL 截断失败", "err", err.Error())
		}
	}
	// 告警事件、审计日志、过期注册码保留 90 天（6.5 补充）
	cutoff := now.AddDate(0, 0, -90).Unix()
	_, _ = s.repo.DB().Write.Exec(`DELETE FROM alert_events WHERE created_at < ?`, cutoff)
	_, _ = s.repo.DB().Write.Exec(`DELETE FROM audit_log WHERE ts < ?`, cutoff)
	_, _ = s.repo.DB().Write.Exec(`DELETE FROM enroll_codes WHERE expires_at < ?`, cutoff)

	// 每日自动备份（6.6）。放在 cleanup 里而不是单独开 ticker：
	// 清理是每小时跑一次的既有节拍，备份按"日期去重"天然就是每日一次，
	// 少一个 ticker 就少一处需要 defer Stop 的资源。
	s.maybeDailyBackup(now)
}

// maybeDailyBackup 当天尚未备份时执行一次备份，并按 backup.keep 轮转（6.6）。
//
// 触发条件用"本地日期 + 小时"两个维度：
//   - 小时必须 ≥ backup.hour（默认 4 点）。不能只判日期，否则 Master 在
//     凌晨 1 点重启就会立刻备一份当天的，等真正的 4 点到了又跳过——
//     等于把备份时刻永久钉死在"那次重启"上。
//   - 日期与 lastBackupDay 相同则跳过，保证一天只备一次。
//
// 另外会先扫一遍已有的备份文件名：如果今天的备份已经存在（比如 Master 在
// 4:05 备份完、4:30 重启），重启后 lastBackupDay 是空的，光看内存标记会重复备份。
// 用文件名兜底判断，重启也不会多备。
func (s *Scheduler) maybeDailyBackup(now time.Time) {
	if s.repo.GetSettingDefault("backup.enabled", "1") != "1" {
		return
	}
	hour := int(s.repo.GetIntSetting("backup.hour", 4))
	if now.Hour() < hour {
		return
	}
	today := now.Format("20060102")
	// 内存标记命中：今天已经备过。
	if s.lastBackupDay == today {
		return
	}
	// 重启兜底：内存标记为空，但磁盘上已有今天的备份（文件名 probe-YYYYMMDD-HHMMSS.db）。
	if list, err := s.repo.ListBackups(); err == nil {
		for _, b := range list {
			if strings.HasPrefix(b.Name, "probe-"+today+"-") {
				s.lastBackupDay = today
				return
			}
		}
	} else {
		// 读列表失败不阻断备份——宁可多备一份，也不要因为"读不到目录"而不备份。
		s.log.Warn("读取备份列表失败，继续执行备份", "err", err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	info, err := s.repo.CreateBackup(ctx, now)
	if err != nil {
		// 失败**不**写 lastBackupDay：下一个小时 tick 会重试。
		// 若这里置位，一次瞬时失败（比如磁盘满）会让今天彻底不备份，
		// 而管理员在后台看到的只是"最后备份是昨天"。
		s.log.Error("自动备份失败", "err", err.Error())
		return
	}
	s.lastBackupDay = today
	s.log.Info("自动备份完成", "name", info.Name, "size_bytes", info.SizeBytes)
	_, _ = s.repo.DB().Write.Exec(
		`INSERT INTO audit_log(ts, actor, action, target, ip) VALUES(?,?,?,?,?)`,
		now.Unix(), "system", "maintenance.backup_auto", info.Name, "")

	keep := int(s.repo.GetIntSetting("backup.keep", 7))
	if keep < 1 {
		keep = 1
	}
	removed, err := s.repo.PruneBackups(keep)
	if err != nil {
		s.log.Error("清理旧备份失败", "err", err.Error())
	}
	if len(removed) > 0 {
		s.log.Info("已清理旧备份", "count", len(removed), "keep", keep)
		_, _ = s.repo.DB().Write.Exec(
			`INSERT INTO audit_log(ts, actor, action, target, ip) VALUES(?,?,?,?,?)`,
			now.Unix(), "system", "maintenance.backup_prune", strings.Join(removed, ","), "")
	}
}
