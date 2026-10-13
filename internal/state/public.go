package state

import (
	"sort"
	"time"

	"github.com/yourorg/probe/internal/model"
	"github.com/yourorg/probe/internal/traffic"
)

// 公开 DTO 的唯一构建入口（5.1 / 7.3 / S1）。
//
// 为什么必须只有一份：列表页（公开快照）与详情页（按 public_id 现算）如果
// 各写一份字段映射，加字段时必然漏掉其中一处，表现为"列表上有值、点进去没有"
// 这种极难发现的不一致。这里集中构建，两处共用。

// BuildPublicNode 由运行态生成公开 DTO（白名单全集）。
//
// 注意：这里输出的是白名单里的全部字段；settings.public.fields 的收窄在
// 响应阶段完成（api 层），因此管理员改设置后立即生效，无需等下一次快照。
//
// withLatency 控制是否带上延迟指示灯：列表页不需要，详情页才需要。
func BuildPublicNode(n *NodeRuntime, at time.Time, withLatency bool) model.PublicNode {
	p := model.PublicNode{
		PublicID:       n.PublicID,
		Name:           n.Name,
		Group:          n.Group,
		Status:         n.Status,
		OS:             n.OS,
		Arch:           n.Arch,
		Virt:           n.Virt,
		CPUCores:       n.CPUCores,
		CPUPct:         derefInt(n.LatestMetrics.CPU),
		Load1:          derefInt(n.LatestMetrics.Load1),
		MemUsed:        derefInt64(n.LatestMetrics.MemUsed),
		MemTotal:       n.MemTotal,
		MemPct:         Percent(derefInt64(n.LatestMetrics.MemUsed), n.MemTotal),
		SwapPct:        Percent(derefInt64(n.LatestMetrics.SwapUsed), n.SwapTotal),
		DiskUsed:       derefInt64(n.LatestMetrics.DiskUsed),
		DiskTotal:      n.DiskTotal,
		DiskPct:        Percent(derefInt64(n.LatestMetrics.DiskUsed), n.DiskTotal),
		RxBps:          derefInt64(n.LatestMetrics.RxBps),
		TxBps:          derefInt64(n.LatestMetrics.TxBps),
		RxUsed:         n.PeriodRx,
		TxUsed:         n.PeriodTx,
		TrafficUsed:    n.TrafficUsed,
		TrafficQuota:   n.TrafficQuota,
		TrafficUsedPct: n.TrafficUsedPct,
		TodayRx:        n.TodayRx,
		TodayTx:        n.TodayTx,
		UptimeS:        n.Uptime(at.Unix()),
		UpdatedAt:      n.LastSeen,
	}
	if n.ExpireAt > 0 {
		d := traffic.DaysUntil(n.ExpireAt, at.Unix())
		p.ExpireDays = &d
	}
	p.NetStack = netStackOf(n.HasIPv4, n.HasIPv6)
	// Country 只输出两位国家代码（不是机房名/供应商/备注那类自由文本），
	// 供卡片画国旗。空值表示未设置，前端不画旗帜。
	p.Country = n.Country
	// 续费价格：与「月费合计」同一折算口径（MonthlyEquivalent）。
	// fee_cycle 带周期原值——once/lifetime 折算后是 0，前端要靠周期区分
	// "终身/一次性"与"根本没配价格"（两者数值都是 0，文案必须不同）。
	p.MonthlyFee = model.MonthlyEquivalent(n.PriceMinor, n.Cycle)
	p.MonthlyFeeCurrency = n.Currency
	if n.PriceMinor > 0 {
		p.FeeCycle = n.Cycle
	}
	if len(n.LatencyBars) > 0 {
		// 拷贝一份：运行态里的切片会被下一轮聚合整体替换，
		// 直接交给快照等于把内部内存交给调用方长期持有。
		p.LatencyBars = append([]model.PublicLatencyBar(nil), n.LatencyBars...)
	}
	if withLatency && len(n.PublicLatency) > 0 {
		// 拷贝一份，避免把内部切片交给调用方长期持有。
		p.Latency = append([]model.PublicLatency(nil), n.PublicLatency...)
	}
	return p
}

// netStackOf 把两个布尔量拼成协议栈标识串。
//
// 单独抽成函数是因为"双栈要显示成 v4+v6 还是 v4/v6"这种细节容易两处写法不一致，
// 而它同时出现在列表卡片与详情页，不一致会立刻被看出来。
func netStackOf(v4, v6 bool) string {
	switch {
	case v4 && v6:
		return "v4+v6"
	case v4:
		return "v4"
	case v6:
		return "v6"
	default:
		return ""
	}
}

// latencyCarrierOrder 决定点阵行的先后顺序（固定，不随 map 遍历随机）。
// 一行现在对应**一条探测目标**：先按目标的运营组排，同组内按 target id 排
// （id 单调递增、稳定，与创建顺序一致；用名字排会随改名跳动）。
var latencyCarrierOrder = []string{"telecom", "unicom", "mobile", "custom"}

// LatencyBarsWindowS / LatencyBarsSegments 是默认时间窗（24 小时）与分段数。
// 24 小时 × 32 段 ≈ 每点 45 分钟：点数够多能看出趋势，又不至于把卡片撑爆。
// 其余窗口（30m/1h/6h）见 LatencyBarLayout。
const (
	LatencyBarsWindowS  int64 = 24 * 3600
	LatencyBarsSegments       = 32
)

// BarLayout 描述一个点阵时间窗的聚合参数。
//
// Raw 为 true 时数据源取 latency_raw（探测粒度）而不是 latency_10m：
// 30m/1h 的窗口落在 10 分钟层上分别只有 3/6 个桶，画 30 个点几乎全是灰格——
// 短窗口必须用原始层才有有效数据。raw_hours 保留期默认 24 小时，
// 覆盖这两个窗口绰绰有余。
type BarLayout struct {
	WindowS  int64 // 窗口总秒数
	Segments int   // 分段数（= 每行点数）
	Raw      bool  // true → 读 latency_raw；false → 读 latency_10m
}

// Step 是每个点代表的秒数。WindowS 必须能被 Segments 整除。
func (l BarLayout) Step() int64 { return l.WindowS / int64(l.Segments) }

// ReadFrom 给出"足以覆盖整个点阵窗口"的查询起始时间。
//
// 不能直接用 now-WindowS：BuildLatencyBars 会把窗口起点**向下对齐到段边界**，
// 对齐后的起点最多比 now-WindowS 早一个 step。只查 now-WindowS 的话，
// 最旧那一段（24h 窗口下是 45 分钟）一个桶都取不到，点阵最左边一格永远是灰的
// ——看起来像"节点在那个时段掉线了"，其实是查询范围差了一格。
func (l BarLayout) ReadFrom(now int64) int64 { return now - l.WindowS - l.Step() }

// LatencyBarLayout 把 public.lat_window 的取值映射为聚合参数。
// 每个 windowS 都能被 segments 整除（60/120/600/2700），
// 否则 BuildLatencyBars 的"对齐到段边界"会失配。
func LatencyBarLayout(window string) BarLayout {
	switch window {
	case "30m":
		return BarLayout{WindowS: 1800, Segments: 30, Raw: true}
	case "1h":
		return BarLayout{WindowS: 3600, Segments: 30, Raw: true}
	case "6h":
		return BarLayout{WindowS: 6 * 3600, Segments: 36, Raw: false}
	default: // "24h" 与任何未知值：保持历史行为
		return BarLayout{WindowS: LatencyBarsWindowS, Segments: LatencyBarsSegments, Raw: false}
	}
}

// BuildLatencyBars 把延迟桶聚合成卡片点阵（**按探测目标一行**）。
//
// 行键是 target_id 而不是运营组：运营组只有四个常量（电信/联通/移动/自定义），
// 而目标名（"电信-上海"、"专线-内网"）才是管理员真正写进去、访客真正读得懂
// 的标签；且同一运营组下可能有多条专属目标，按运营组聚合会把它们混成一行。
// 行序 = 运营组顺序 → target id：前者保持"三网在前、专属在后"的阅读习惯，
// 后者保证稳定（map 遍历顺序随机，不显式排序卡片每秒都会重排）。
//
// windowS/segments 由调用方按 public.lat_window 选择（见 LatencyBarLayout）；
// windowS 必须能被 segments 整除。
//
// 聚合规则刻意从简：段内所有非 nil RTT 求平均、Loss 求平均。
// 某段一个桶都没有 → RTT/Loss 均为 nil（前端画灰点"无数据"），
// 而不是沿用上一段的值——点阵表达的是"那段时间怎么样"，
// 拿旧数据补位会把故障时间窗悄悄涂绿，方向恰好与监控的目的相反。
//
// 输出行固定排序；没有任何桶的目标不输出。
func BuildLatencyBars(buckets []model.LatencyBucket, now int64, windowS int64, segments int) []model.PublicLatencyBar {
	// step 必须由**本次调用的** windowS/segments 推出，不能再用包级常量：
	// 常量对应 24h/32 段（每点 45 分钟）。30 分钟窗口（1800s/30 段，每点 1 分钟）
	// 若沿用常量，取样区间会宽到 4500s、而 idx 的分母还是 2700s——
	// 30 个点里只有前 2 个可能被填上，点阵看上去"几乎全是灰格"，
	// 前端拿到的 step 也仍是"每个点约 45 分钟"。窗口参数化必须贯穿到底。
	step := windowS / int64(segments)
	// 窗口对齐到段边界：让"每段的含义"与真实时间对得上（悬浮提示要显示时间）。
	to := now - now%step
	from := to - windowS

	type seg struct {
		rttSum, rttN, lossSum, lossN int64
	}
	type key struct {
		targetID int64
	}
	meta := map[int64]model.LatencyBucket{} // target_id → 该目标任意一桶（取 Name/Carrier）
	byTarget := map[int64][]*seg{}
	for _, b := range buckets {
		if b.TS < from || b.TS >= to+step {
			continue
		}
		idx := int((b.TS - from) / step)
		if idx < 0 || idx >= segments {
			continue
		}
		s := byTarget[b.TargetID]
		if s == nil {
			s = make([]*seg, segments)
			byTarget[b.TargetID] = s
		}
		if _, ok := meta[b.TargetID]; !ok {
			meta[b.TargetID] = b
		}
		if s[idx] == nil {
			s[idx] = &seg{}
		}
		s[idx].lossSum += b.Loss
		s[idx].lossN++
		if b.RTT != nil {
			s[idx].rttSum += *b.RTT
			s[idx].rttN++
		}
	}

	carrierRank := func(c string) int {
		for i, want := range latencyCarrierOrder {
			if c == want {
				return i
			}
		}
		return len(latencyCarrierOrder) // 未知运营组排最后，不丢数据
	}
	ids := make([]int64, 0, len(byTarget))
	for id := range byTarget {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		mi, mj := meta[ids[i]], meta[ids[j]]
		ri, rj := carrierRank(mi.Carrier), carrierRank(mj.Carrier)
		if ri != rj {
			return ri < rj
		}
		return ids[i] < ids[j]
	})

	out := make([]model.PublicLatencyBar, 0, len(ids))
	for _, id := range ids {
		segs := byTarget[id]
		m := meta[id]
		bar := model.PublicLatencyBar{
			Name:    m.Name,
			Carrier: m.Carrier,
			From:    from,
			Step:    step,
			RTT:     make([]*int64, segments),
			Loss:    make([]*int64, segments),
		}
		hasData := false
		for i, s := range segs {
			if s == nil {
				continue
			}
			hasData = true
			l := s.lossSum / s.lossN
			bar.Loss[i] = &l
			if s.rttN > 0 {
				r := s.rttSum / s.rttN
				bar.RTT[i] = &r
			}
		}
		if hasData {
			out = append(out, bar)
		}
	}
	return out
}

// BuildPublicOverview 汇总公开概览。
// nodes 应由调用方按稳定顺序传入（见 Snapshotter.Refresh）。
func BuildPublicOverview(nodes []*NodeRuntime, at time.Time) model.PublicOverview {
	o := model.PublicOverview{GeneratedAt: at.Unix()}
	for _, n := range nodes {
		o.NodesTotal++
		if n.Status == model.StatusOnline || n.Status == model.StatusWarning {
			o.NodesOnline++
		}
		o.RxBps += derefInt64(n.LatestMetrics.RxBps)
		o.TxBps += derefInt64(n.LatestMetrics.TxBps)
		o.RxUsed += n.PeriodRx
		o.TxUsed += n.PeriodTx
		o.TrafficUsed += n.TrafficUsed
		o.TodayRx += n.TodayRx
		o.TodayTx += n.TodayTx
		// 月费合计必须按支付周期折算成月均，不能直接累加 price_minor：
		// 后者是"该周期一次性金额"，年付节点会被算成 12 倍月支出，
		// 而 lifetime/once 根本没有月支出却被计入（详见 model.MonthlyEquivalent）。
		fee := model.MonthlyEquivalent(n.PriceMinor, n.Cycle)
		o.MonthlyFee += fee
		// 币种取**第一个真正有月支出的节点**的币种：拿一台 lifetime 节点的
		// 币种去标注一堆 CNY 金额，会让总额看起来像美元。
		if fee > 0 && o.MonthlyFeeCurrency == "" && n.Currency != "" {
			o.MonthlyFeeCurrency = n.Currency
		}
	}
	return o
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
