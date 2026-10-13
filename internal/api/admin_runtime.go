package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yourorg/probe/internal/model"
	"github.com/yourorg/probe/internal/state"
)

// 管理端实时态与历史查询（文档 5.2“全量资产视察”）。
//
// 与公开接口的区别：
//   - 按真实 node_id 查询，隐藏节点（public_visible=0）与归档节点同样可见；
//   - 返回真实 IP、主机名、内核/CPU 型号、令牌状态等敏感字段；
//   - 因此只挂在 /api/admin/v1（会话 + CSRF）之下，绝不进入公开 DTO。

// AdminNodeItem 是后台节点列表的一项：库中的元数据 + 内存运行态快照。
//
// 为什么必须在这里合并运行态：
//
//	nodes 表的 status / last_seen 两列实际上**没有任何代码写入**——
//	store 层有 UpdateNodeStatus，但全仓库找不到调用点，因为运行态按 4.2 的
//	设计只放内存。若列表接口直接返回库里的行，后台看到的永远是
//	"offline / 从未上报"，而同一时刻公开看板上这些节点明明是绿的。
//	（前端曾用 N+1 的 /nodes/{id}/runtime 去补 CPU/内存/磁盘，但状态和最后
//	上报补不回来——那两列在库里的值就是错的。）
//
// Runtime 为 nil 表示该节点从未上报过（刚建好、Agent 还没装）。
type AdminNodeItem struct {
	*model.Node
	Runtime *model.AdminNodeRuntime `json:"runtime,omitempty"`

	// FeeMonthly 是该节点的**月均费用**（最小货币单位），由 node_billing 的
	// 金额按支付周期折算而来（见 model.MonthlyEquivalent）。后台列表页把它
	// 汇总成「月费合计」卡片，与公开看板同一口径——两处若各算各的，
	// 迟早会出现"后台显示 ¥147、公开页显示 ¥196"这种对不上的情况。
	//
	// 用列表项自带而不是单独的汇总接口：列表本来就是一次请求拿全部节点，
	// 再开一个 /billing/summary 只是把同一份数据查两遍。
	FeeMonthly  int64  `json:"fee_monthly"`
	FeeCurrency string `json:"fee_currency,omitempty"`

	// FeeCycle 是该节点的**支付周期**（monthly/quarterly/yearly/once/lifetime），
	// 仅在确实配了价格时下发。存在的理由与公开 DTO 的 fee_cycle 相同：
	// 终身/一次性折算成月均后都是 0，只看 FeeMonthly 分不清"终身"和"没配价格"，
	// 卡片上会把终身节点显示成"—"。两边的字段名保持一致，前端可以共用一段文案逻辑。
	FeeCycle string `json:"fee_cycle,omitempty"`
}

// buildAdminNodeList 把库中的节点元数据与内存运行态合并成列表。
//
// 一次遍历 Registry，不做 N+1 查询；节点数再多也只是内存里的 map 查找。
func (h *Handler) buildAdminNodeList(nodes []*model.Node, now time.Time) []AdminNodeItem {
	// 计费行一次全读（节点数通常只有个位到几十，全表扫描比 N+1 点查划算）。
	// 读失败不阻断列表：少一张月费卡片的总额，不该让整个节点页打不开。
	billings, err := h.Repo.ListBillings()
	if err != nil {
		billings = nil
	}
	out := make([]AdminNodeItem, 0, len(nodes))
	for _, n := range nodes {
		item := AdminNodeItem{Node: n}
		if rt, ok := h.Registry.Get(n.ID); ok && rt.LastSeen > 0 {
			a := h.buildAdminRuntime(rt, now)
			item.Runtime = &a
		}
		if b, ok := billings[n.ID]; ok {
			item.FeeMonthly = model.MonthlyEquivalent(b.PriceMinor, b.Cycle)
			item.FeeCurrency = b.Currency
			// 只有真配了价格才带周期：没配价格时 MonthlyEquivalent 也是 0，
			// 把 cycle 一起下发会让卡片把"没配价格"显示成"终身"。
			if b.PriceMinor > 0 {
				item.FeeCycle = b.Cycle
			}
		}
		out = append(out, item)
	}
	return out
}

// handleNodeRuntime 对应 GET /api/admin/v1/nodes/{id}/runtime。
// 读内存注册表，不落盘、不查时序表（4.2：最新状态只存内存）。
func (h *Handler) handleNodeRuntime(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	rt, ok := h.Registry.Get(id)
	// 运行态里可能已经有该节点，但元数据是在首次上报前由调度器预置的；
	// 因此“是否上报过”只能看 LastSeen，而不是“是否存在于注册表”。
	if !ok || rt.LastSeen == 0 {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "节点尚无上报数据")
		return
	}
	writeJSON(w, http.StatusOK, h.buildAdminRuntime(rt, time.Now()))
}

// buildAdminRuntime 把内存运行态转为管理端 DTO（含敏感字段）。
func (h *Handler) buildAdminRuntime(n *state.NodeRuntime, now time.Time) model.AdminNodeRuntime {
	m := n.LatestMetrics
	out := model.AdminNodeRuntime{
		NodeID:         n.NodeID,
		PublicID:       n.PublicID,
		Status:         n.Status,
		LastSeen:       n.LastSeen,
		UptimeS:        n.Uptime(now.Unix()),
		SuspectFrom:    n.SuspectSince,
		Archived:       n.Archived,
		CPUPct:         deref(m.CPU),
		Load1:          deref(m.Load1),
		MemUsed:        deref(m.MemUsed),
		MemTotal:       n.MemTotal,
		MemPct:         state.Percent(deref(m.MemUsed), n.MemTotal),
		SwapUsed:       deref(m.SwapUsed),
		SwapPct:        state.Percent(deref(m.SwapUsed), n.SwapTotal),
		DiskUsed:       deref(m.DiskUsed),
		DiskPct:        state.Percent(deref(m.DiskUsed), n.DiskTotal),
		RxBps:          deref(m.RxBps),
		TxBps:          deref(m.TxBps),
		Conns:          deref(m.Conns),
		TrafficQuota:   n.TrafficQuota,
		TrafficUsed:    n.TrafficUsed,
		TrafficUsedPct: n.TrafficUsedPct,
		PeriodRx:       n.PeriodRx,
		PeriodTx:       n.PeriodTx,
		TodayRx:        n.TodayRx,
		TodayTx:        n.TodayTx,
		ExpireAt:       n.ExpireAt,
		BootTime:       n.BootTime,
		CounterBits:    n.CounterBits,
		Virt:           n.Virt,
		OS:             n.OS,
		ConfigVersion:  n.ConfigVersion,
	}
	if len(n.LatestLatency) > 0 {
		out.Latency = append([]model.LatencySample(nil), n.LatestLatency...)
	}
	return out
}

// handleAdminHistory 对应 GET /api/admin/v1/nodes/{id}/history。
// 与公开版共用选层逻辑，但按真实 node_id 查询，因此隐藏/归档节点也可查。
func (h *Handler) handleAdminHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	rt, ok := h.Registry.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "节点尚无上报数据")
		return
	}
	rangeStr := r.URL.Query().Get("range")
	if rangeStr == "" {
		rangeStr = "24h"
	}
	metrics := strings.Split(r.URL.Query().Get("metrics"), ",")
	if len(metrics) == 0 || metrics[0] == "" {
		metrics = []string{"cpu", "mem", "net_rx", "net_tx"}
	}

	now := time.Now()
	if rangeStr == "30m" {
		from := now.Add(-30 * time.Minute).Unix()
		// 必须走 RingPoints：Registry.Get 返回的是副本，为避免与上报路径竞争，
		// 副本里的 Ring 已被置为 nil（环形缓冲是原地 Push 的）。直接写 rt.Ring.Since
		// 会在 nil 上取字段而 panic。
		pts, _ := h.Registry.RingPoints(id, from)
		series := make([]model.PublicSeries, 0, len(metrics))
		for _, m := range metrics {
			s := model.PublicSeries{Metric: m, Unit: unitOf(m)}
			for _, p := range pts {
				s.Points = append(s.Points, model.PublicHistoryPoint{TS: p.TS, Value: pickRing(p, m)})
			}
			series = append(series, s)
		}
		writeJSON(w, http.StatusOK, series)
		return
	}

	table := h.Repo.MetricsTableForRange(rangeStr)
	from := now.Add(-rangeDuration(rangeStr)).Unix()
	series := make([]model.PublicSeries, 0, len(metrics))
	for _, m := range metrics {
		pts, err := h.Repo.QueryMetricHistory(table, rt.NodeID, from, now.Unix(), m, 500)
		if err != nil {
			continue
		}
		s := model.PublicSeries{Metric: m, Unit: unitOf(m)}
		for _, p := range pts {
			s.Points = append(s.Points, model.PublicHistoryPoint{TS: p.TS, Value: p.Value})
		}
		series = append(series, s)
	}
	writeJSON(w, http.StatusOK, series)
}

// handleAdminLatency 对应 GET /api/admin/v1/nodes/{id}/latency。
// 附带目标名称/host/method，便于后台直接显示“哪个目标在抖”。
//
// 与公开接口 handlePublicLatency 保持同口径（同一张表、同一套窗口统计、
// 同样按"当前启用的目标集合"输出）。曾经两边各写一份，结果后台详情页把
// rtt_avg 的 NULL 读成 0、还没有汇总统计，管理员看到的曲线和访客看到的
// 根本不是一回事——故障排查时被这条差异误导过。
func (h *Handler) handleAdminLatency(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	rangeStr := r.URL.Query().Get("range")
	table := h.Repo.LatencyTableForRange(rangeStr)
	if table == "" {
		// 表名为空会让拼出来的 SQL 直接语法错误（500）；宁可返回空序列。
		h.logErr("handleAdminLatency", fmt.Errorf("LatencyTableForRange(%q) 返回空表名", rangeStr))
		writeJSON(w, http.StatusOK, []*model.AdminProbeLatency{})
		return
	}
	now := time.Now()
	from := now.Add(-rangeDuration(rangeStr)).Unix()

	type acc struct {
		s                *model.AdminProbeLatency
		points           []model.PublicHistoryPoint
		lossSum, lossN   int64
		sum, min, max, n int64
	}
	byTarget := map[int64]*acc{}

	// DESC + Go 侧升序整理：LIMIT 截断时保住**最新**数据（与公开侧
	// handlePublicLatency 同一套修正，ASC 截断会让曲线往过去漂移）。
	rows, err := h.Repo.DB().Read.Query(
		`SELECT target_id, ts, rtt_avg, COALESCE(loss,0) FROM `+table+`
		 WHERE node_id=? AND ts>=? ORDER BY ts DESC LIMIT 5000`, id, from)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var tid, ts, loss int64
		var rtt sql.NullInt64
		if err := rows.Scan(&tid, &ts, &rtt, &loss); err != nil {
			continue
		}
		a := byTarget[tid]
		if a == nil {
			a = &acc{s: &model.AdminProbeLatency{TargetID: tid}, min: -1}
			byTarget[tid] = a
		}
		a.lossSum += loss
		a.lossN++
		// rtt_avg 为 NULL 表示这一轮探测整体失败。
		//
		// 绝不能 COALESCE 成 0：那样"整窗口全挂"会画成一条掉到 0 的漂亮低谷，
		// 恰好把最需要被看见的故障渲染成了最健康的形状。NULL 直接跳过该点，
		// 汇总统计里也不计入——n=0 时四项 RTT 输出 null，前端显示"无数据"。
		if !rtt.Valid {
			continue
		}
		a.points = append(a.points, model.PublicHistoryPoint{TS: ts, Value: rtt.Int64})
		a.sum += rtt.Int64
		if a.min < 0 || rtt.Int64 < a.min {
			a.min = rtt.Int64
		}
		if rtt.Int64 > a.max {
			a.max = rtt.Int64
		}
		a.n++
	}

	// 图例口径 = 后台当前启用的目标集合（与公开侧一致）：
	// 配置了却从未探通的目标要明确列出并标注"无数据"，
	// 而已经停用/删除的目标不该靠历史行继续挂在详情里。
	// DESC 查询后先把每个目标的时间序列恢复升序。
	for _, a := range byTarget {
		sort.Slice(a.points, func(i, j int) bool { return a.points[i].TS < a.points[j].TS })
	}
	var ids []int64
	if targets, err := h.Repo.ListProbeTargets(id); err == nil {
		for _, t := range targets {
			if !t.Enabled {
				continue
			}
			ids = append(ids, t.ID)
			a := byTarget[t.ID]
			if a == nil {
				a = &acc{s: &model.AdminProbeLatency{TargetID: t.ID}, min: -1}
				byTarget[t.ID] = a
			}
			a.s.Name, a.s.Host, a.s.Method, a.s.Carrier, a.s.Enabled = t.Name, t.Host, t.Method, t.Carrier, true
		}
	}
	// 按 target_id 升序：map 遍历顺序随机，不排序会让图例颜色每 3 秒换一次。
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]*model.AdminProbeLatency, 0, len(ids))
	for _, tid := range ids {
		a := byTarget[tid]
		pts := a.points
		if pts == nil {
			pts = []model.PublicHistoryPoint{}
		}
		s := a.s
		s.Points = decimate(pts, maxLatencyPoints)
		if s.Name == "" {
			s.Name = "目标 #" + strconv.FormatInt(tid, 10)
		}
		if a.lossN > 0 {
			s.Loss = int(a.lossSum / a.lossN)
		}
		if a.n > 0 {
			last, avg, mn, mx := a.points[len(a.points)-1].Value, a.sum/a.n, a.min, a.max
			s.Last, s.Avg, s.Min, s.Max = &last, &avg, &mn, &mx
		}
		if s.Avg == nil {
			s.Status = model.LatencyUnknown
		} else {
			s.Status = model.LatencyStatus(s.Avg, "")
		}
		out = append(out, s)
	}
	writeJSON(w, http.StatusOK, out)
}
