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
	"github.com/yourorg/probe/internal/store"
	"github.com/yourorg/probe/internal/theme"
)

// 公开 API（文档 7.3 / 5.1）。
//
// 安全要点：响应模型是独立 DTO，字段白名单从类型层面排除 IP、主机名、内核/CPU 型号、
// 令牌、价格（除非显式放开）。隐藏节点（public_visible=0）不会出现。

// handleSite 对应 GET /api/public/v1/site。
func (h *Handler) handleSite(w http.ResponseWriter, r *http.Request) {
	title := h.Repo.GetSettingDefault("site.title", "Server Probe")
	active := h.Themes.Active()
	if v := h.Repo.GetSettingDefault("theme.active", ""); v != "" {
		active = v
	}
	writeJSON(w, http.StatusOK, model.PublicSite{
		Title:   title,
		Theme:   active,
		Meta:    h.Repo.GetSettingDefault("inject.head", ""),
		Version: h.Version,
		Cards:   h.publicCards(),
		// 点阵时间窗原样下发（空值=默认 24h），前端据此生成"近 X"标题与
		// "每个点约多久"的悬浮提示，不必把窗口文案硬编码在两处。
		LatWindow: h.Repo.GetSettingDefault("public.lat_window", "24h"),
		// 「管理后台」按钮去哪儿。公开站与后台不同源时，写死的 /admin/
		// 会跳到公开站自己的路径下（404/421），所以由服务端算好再下发。
		AdminURL: h.adminEntryURL(r),
	})
}

// publicCards 返回允许展示的公开卡片键。
//
//	nil   —— 不限制，全部展示（设置项为 "all"）
//	非 nil —— 只展示列出的键；空切片表示一张都不展示
//
// 只要设置项不是 "all" 就必须返回**非 nil** 切片：nil 与空切片在 JSON 里
// 分别是 null 与 []，前端靠这个区分"不限制"和"全部关掉"。这里用
// make(..., 0, len(m)) 保证空的时候也不是 nil。
func (h *Handler) publicCards() []string {
	m := model.ParsePublicCards(h.Repo.GetSettingDefault("public.cards", model.DefaultPublicCards()))
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m))
	// 按固定顺序输出而不是 map 迭代顺序：前端拿它做 diff 时顺序稳定更省心。
	for _, k := range model.PublicCardNames() {
		if m[k] {
			out = append(out, k)
		}
	}
	return out
}

// publicFieldMask 读取 settings.public.fields 并解析为掩码（7.3）。
//
// 未配置或解析后为空 → 返回 nil，表示不过滤（公开白名单全集）。
// 在响应阶段读取而不是快照阶段：管理员改完设置立刻生效，不必等下一次快照。
func (h *Handler) publicFieldMask() model.PublicFieldMask {
	return model.ParsePublicFields(h.Repo.GetSettingDefault("public.fields", ""))
}

// handleOverview 对应 GET /api/public/v1/overview（读内存快照，A9）。
func (h *Handler) handleOverview(w http.ResponseWriter, r *http.Request) {
	snap := h.Snap.Last()
	if snap == nil {
		h.Snap.Refresh(time.Now())
		snap = h.Snap.Last()
	}
	if snap == nil {
		writeJSON(w, http.StatusOK, model.PublicOverview{})
		return
	}
	w.Header().Set("Cache-Control", "max-age=2")
	writeJSON(w, http.StatusOK, snap.Overview.WithFieldMask(h.publicFieldMask()))
}

// handlePublicNodes 对应 GET /api/public/v1/nodes。
func (h *Handler) handlePublicNodes(w http.ResponseWriter, r *http.Request) {
	snap := h.Snap.Last()
	if snap == nil {
		h.Snap.Refresh(time.Now())
		snap = h.Snap.Last()
	}
	if snap == nil {
		writeJSON(w, http.StatusOK, []model.PublicNode{})
		return
	}
	mask := h.publicFieldMask()
	out := make([]model.PublicNode, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		out = append(out, n.WithFieldMask(mask))
	}
	w.Header().Set("Cache-Control", "max-age=2")
	writeJSON(w, http.StatusOK, out)
}

// handlePublicNode 对应 GET /api/public/v1/nodes/{public_id}。
func (h *Handler) handlePublicNode(w http.ResponseWriter, r *http.Request) {
	publicID := r.PathValue("public_id")
	rt, ok := h.Registry.GetByPublicID(publicID)
	if !ok || !rt.PublicVisible || rt.Archived {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "节点不存在")
		return
	}
	w.Header().Set("Cache-Control", "max-age=2")
	writeJSON(w, http.StatusOK, h.buildPublicNode(rt, time.Now(), true).WithFieldMask(h.publicFieldMask()))
}

// handlePublicHistory 对应 GET /nodes/{public_id}/history。
// 按跨度自动选层：≤30 分钟读内存，≤24h 读高精度层，≤7d 读中精度层，更长读低精度层（4.2）。
func (h *Handler) handlePublicHistory(w http.ResponseWriter, r *http.Request) {
	publicID := r.PathValue("public_id")
	rt, ok := h.Registry.GetByPublicID(publicID)
	if !ok || !rt.PublicVisible || rt.Archived {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "节点不存在")
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
	span := rangeDuration(rangeStr)

	if rangeStr == "30m" {
		// 内存实时层：必须走 RingPoints 在锁内取快照。
		// GetByPublicID 返回的是副本且 Ring 已被置空，直接读 rt.Ring 会 panic，
		// 更要紧的是环形缓冲是原地 Push 的，锁外读会与上报路径竞争。
		from := now.Add(-30 * time.Minute).Unix()
		pts, _ := h.Registry.RingPointsByPublicID(publicID, from)
		series := make([]model.PublicSeries, 0, len(metrics))
		for _, m := range metrics {
			s := model.PublicSeries{Metric: m, Unit: unitOf(m)}
			for _, p := range pts {
				s.Points = append(s.Points, model.PublicHistoryPoint{TS: p.TS, Value: pickRing(p, m)})
			}
			series = append(series, s)
		}
		w.Header().Set("Cache-Control", "max-age=2")
		writeJSON(w, http.StatusOK, series)
		return
	}

	table := h.Repo.MetricsTableForRange(rangeStr)
	from := now.Add(-span).Unix()
	series := make([]model.PublicSeries, 0, len(metrics))
	for _, m := range metrics {
		pts, err := h.Repo.QueryMetricHistory(table, rt.NodeID, from, now.Unix(), m, 500)
		if err != nil {
			// 查询失败静默丢掉会让"为什么曲线是空的"无从排查（本缺陷的
			// 根因 AVG 浮点 Scan 失败就是这么藏了几个小时才被发现的）。
			h.logErr("handlePublicHistory", fmt.Errorf("metric %s: %w", m, err))
			continue
		}
		s := model.PublicSeries{Metric: m, Unit: unitOf(m)}
		for _, p := range pts {
			s.Points = append(s.Points, model.PublicHistoryPoint{TS: p.TS, Value: p.Value})
		}
		series = append(series, s)
	}
	w.Header().Set("Cache-Control", "max-age=2")
	writeJSON(w, http.StatusOK, series)
}

// handlePublicLatency 对应 GET /nodes/{public_id}/latency。
//
// 返回**每个探测目标**一条曲线 + 该时间窗口内的统计。
//
// 这里有两点是踩过坑的：
//  1. 目标名称必须带出来。过去只返回 target_id，前端只能画成"目标 #1/目标 #2"，
//     看板使用者根本不知道这条线对应哪个探测点。名称是后台自填的标签，
//     与节点 name 同级；host/port 一律不输出（公开接口不含主机信息）。
//  2. rtt_avg 允许为 NULL（整轮探测失败）。旧实现用 COALESCE(rtt_avg,0) 把它读成 0，
//     于是"探测全挂"在看板上画成一条掉到 0 的漂亮低谷——恰好是最需要被看见的故障，
//     被渲染成了最健康的形状。现在 NULL 直接跳过该点，并单独给状态与丢包。
func (h *Handler) handlePublicLatency(w http.ResponseWriter, r *http.Request) {
	publicID := r.PathValue("public_id")
	rt, ok := h.Registry.GetByPublicID(publicID)
	if !ok || !rt.PublicVisible || rt.Archived {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "节点不存在")
		return
	}
	rangeStr := r.URL.Query().Get("range")
	table := h.Repo.LatencyTableForRange(rangeStr)
	if table == "" {
		// 兜底：表名为空会让拼出来的 SQL 直接语法错误（500）。
		// 宁可返回一条"暂无数据"的序列，也不要让整页查询失败。
		h.logErr("handlePublicLatency", fmt.Errorf("LatencyTableForRange(%q) 返回空表名", rangeStr))
		writeJSON(w, http.StatusOK, []model.PublicLatencySeries{})
		return
	}
	now := time.Now()
	from := now.Add(-rangeDuration(rangeStr)).Unix()

	// DESC + 后续按 target 升序整理：LIMIT 5000 截断时必须保住**最新**的数据。
	// 曾经的 ASC 写法在高频探测 + 长窗口下会把最早的数据留下、最近的裁掉，
	// 曲线整体往过去漂移，X 轴时间与真实时刻对不上。
	rows, err := h.Repo.DB().Read.Query(
		`SELECT target_id, ts, rtt_avg, COALESCE(loss,0) FROM `+table+`
		 WHERE node_id=? AND ts>=? ORDER BY ts DESC LIMIT 5000`, rt.NodeID, from)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
		return
	}
	defer rows.Close()

	type acc struct {
		points         []model.PublicHistoryPoint
		lossSum, lossN int64
		sum, min, max  int64
		n              int
	}
	byTarget := map[int64]*acc{}
	for rows.Next() {
		var tid, ts int64
		var rtt sql.NullInt64
		var loss int64
		if err := rows.Scan(&tid, &ts, &rtt, &loss); err != nil {
			continue
		}
		a := byTarget[tid]
		if a == nil {
			a = &acc{min: -1}
			byTarget[tid] = a
		}
		a.lossSum += loss
		a.lossN++
		if rtt.Valid {
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
	}

	// 目标元数据一次读出（不按目标循环查库）。
	metas, _ := h.Repo.ListAllProbeTargetMeta()

	// 查询改成了 DESC 截断（保住最新数据），这里把每个目标的时间序列恢复升序，
	// 供折线直接按 X 递增绘制。
	for _, a := range byTarget {
		sort.Slice(a.points, func(i, j int) bool { return a.points[i].TS < a.points[j].TS })
	}

	// 图例口径 = **后台当前启用的目标集合**（全局 + 该节点专属）。
	//
	// 这里踩过两个坑，方向相反但表现相似：
	//
	//  1. 只按"有数据"输出 → 配置了却从未探通（或整窗口全挂）的目标会彻底
	//     消失，看板上看起来像没配过这个目标。所以要把启用的目标补进来，
	//     明确列出并标注"无数据"。
	//
	//  2. 只做"补入"、不做"剔除" → 刚在后台停用/删除一个目标，公开详情页的
	//     图例里它**还在**（靠时间窗口内的历史行撑着，24h 跨度下能挂一天），
	//     看起来就像"改配置根本没生效"。所以这里以启用集合为白名单，
	//     不在其中的 target_id 一律不输出——历史数据仍在库里，
	//     但已经不属于"这台机器当前的探测目标"。
	//
	// 启用集合为空时输出空数组（而不是"把所有历史数据倒出来"）：
	// 没有探测目标就是没有，硬塞一堆历史曲线反而是在掩盖配置。
	var enabled []*model.ProbeTarget
	if targets, err := h.Repo.ListProbeTargets(rt.NodeID); err == nil {
		for _, t := range targets {
			if t.Enabled {
				enabled = append(enabled, t)
			}
		}
	}

	ids := make([]int64, 0, len(enabled))
	for _, t := range enabled {
		ids = append(ids, t.ID)
		if _, ok := byTarget[t.ID]; !ok {
			byTarget[t.ID] = &acc{min: -1}
		}
		if _, ok := metas[t.ID]; !ok {
			metas[t.ID] = store.ProbeTargetMeta{ID: t.ID, Name: t.Name, Carrier: t.Carrier, Enabled: true}
		}
	}
	// 按 target_id 升序输出：map 遍历顺序随机，不排序会让图例颜色每 3 秒换一次。
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]model.PublicLatencySeries, 0, len(ids))
	for _, tid := range ids {
		a := byTarget[tid]
		// 空切片而不是 nil：目标是有的、只是这个窗口没有数据。
		// 序列化成 null 会让前端必须写 `t.points || []` 来兜底，
		// 而"数组字段就该是数组"是更省心的契约。
		pts := a.points
		if pts == nil {
			pts = []model.PublicHistoryPoint{}
		}
		s := model.PublicLatencySeries{
			TargetID: tid,
			Carrier:  carrierCustom,
			Points:   decimate(pts, maxLatencyPoints),
		}
		if m, ok := metas[tid]; ok {
			s.Carrier = m.Carrier
			s.Name = sanitizeLabel(m.Name)
		}
		if s.Name == "" {
			s.Name = "目标 #" + strconv.FormatInt(tid, 10)
		}
		if a.lossN > 0 {
			s.Loss = int(a.lossSum / a.lossN)
		}
		if a.n > 0 {
			last := a.points[len(a.points)-1].Value
			avg := a.sum / int64(a.n)
			mn, mx := a.min, a.max
			s.Last, s.Avg, s.Min, s.Max = &last, &avg, &mn, &mx
		}
		// 状态按窗口平均 RTT 判定；窗口内一次都没探通 → 数据不可信。
		if s.Avg == nil {
			s.Status = model.LatencyUnknown
		} else {
			s.Status = model.LatencyStatus(s.Avg, "")
		}
		out = append(out, s)
	}
	w.Header().Set("Cache-Control", "max-age=2")
	writeJSON(w, http.StatusOK, out)
}

// carrierCustom 是探测目标运营商的兜底值。
const carrierCustom = "custom"

// maxLatencyPoints 是单条延迟曲线返回给前端的最大点数。
// 30 天跨度的 latency_10m 可以有几千个点，全量下发既浪费带宽，
// 也会让 Canvas 把点挤成一片实心色块。
const maxLatencyPoints = 480

// decimate 等距抽稀到至多 max 个点，并始终保留最后一个点。
// 保留末点很重要：曲线的"当前值"就是它，丢掉会让图例与曲线尾部对不上。
func decimate(pts []model.PublicHistoryPoint, max int) []model.PublicHistoryPoint {
	if max <= 1 || len(pts) <= max {
		return pts
	}
	step := float64(len(pts)-1) / float64(max-1)
	out := make([]model.PublicHistoryPoint, 0, max)
	for i := 0; i < max-1; i++ {
		out = append(out, pts[int(float64(i)*step)])
	}
	out = append(out, pts[len(pts)-1])
	return out
}

// sanitizeLabel 清洗后台自填的标签后再送进公开响应。
func sanitizeLabel(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 48 {
		s = s[:48]
	}
	// 控制字符会让 JSON 里出现不可见的影响阅读的字符，也便于注入奇怪排版，
	// 这里统一剔除（13.4.6 对不可信字符串的处理原则同样适用于自填标签）。
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// handleThemeAsset 提供主题静态资源（含入口注入）。路由为 GET /。
func (h *Handler) handleThemeAsset(w http.ResponseWriter, r *http.Request) {
	name := h.Themes.Active()
	if v := h.Repo.GetSettingDefault("theme.active", ""); v != "" {
		name = v
	}
	rel := strings.TrimPrefix(r.URL.Path, "/")
	if rel == "" {
		html, err := h.Themes.Entry(name)
		if err != nil {
			http.Error(w, "主题不可用", http.StatusInternalServerError)
			return
		}
		head := h.Repo.GetSettingDefault("inject.head", "")
		body := ""
		// 脚本注入默认关闭（5.3）；开启时才注入 body_end，并替换 CSP nonce。
		if h.scriptInjectEnabled() {
			body = h.Repo.GetSettingDefault("inject.body_end", "")
		}
		out := theme.Inject(html, head, body, NonceFrom(r))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(out)
		return
	}

	data, err := h.Themes.Open(name, rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentTypeOf(rel))
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------------------
// DTO 构建
// ---------------------------------------------------------------------------

// buildPublicNode 把运行态转换为脱敏 DTO（字段白名单）。
// 注意：这里没有任何 IP / 主机名 / 内核 / CPU 型号 / 令牌 / 价格字段。
// buildPublicNode 由运行态生成公开 DTO。
//
// 实际映射逻辑集中在 state.BuildPublicNode：列表页（内存快照）与详情页
// （按 public_id 现算）必须共用同一份字段映射，否则加字段时必然漏掉一处。
func (h *Handler) buildPublicNode(n *state.NodeRuntime, now time.Time, withLatency bool) model.PublicNode {
	return state.BuildPublicNode(n, now, withLatency)
}

// rangeDuration 把跨度字符串转为时长。
func rangeDuration(s string) time.Duration {
	switch s {
	case "30m":
		return 30 * time.Minute
	case "1h":
		return time.Hour
	case "5h":
		return 5 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	default: // 24h
		return 24 * time.Hour
	}
}

// unitOf 返回某个历史指标的单位，前端据此格式化坐标轴。
//
// 注意 "mem" 的表达式是 mem_used（**字节**），不是百分比：曾经这里写成
// pct_x100，与实际数值不符。前端目前自带一份 unitOf 没被这个错误影响，
// 但公开 API 自己说的单位必须是准的。
func unitOf(metric string) string {
	switch metric {
	case "cpu":
		return "pct_x100"
	case "mem", "mem_used", "disk_used":
		return "bytes"
	case "net_rx", "net_tx":
		return "bps"
	default:
		return "raw"
	}
}

func pickRing(p state.RingPoint, metric string) int64 {
	switch metric {
	case "cpu":
		return int64(p.CPU)
	case "mem":
		return int64(p.MemPct)
	case "net_rx":
		return p.RxBps
	case "net_tx":
		return p.TxBps
	default:
		return 0
	}
}

func contentTypeOf(rel string) string {
	switch {
	case strings.HasSuffix(rel, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(rel, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(rel, ".json"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(rel, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(rel, ".html"):
		return "text/html; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// scriptInjectEnabled 判断管理员是否开启了页尾脚本注入（5.3，默认关闭）。
// 开启后 CSP 才会下发 nonce，注入的 <script> 才能执行（13.4.5）。
func (h *Handler) scriptInjectEnabled() bool {
	return h.Repo.GetSettingDefault("inject.script_enabled", "0") == "1"
}

// publicEnabled 判断公开站是否对外开放（设置项 public.enabled，默认开放）。
//
// 每次请求都读一次库而不是缓存：这是一个"关掉就立刻生效"的开关，管理员
// 按下保存后最不希望看到的是还要重启 Master 才起作用。settings 表是
// 主键点查，单次开销可忽略。
func (h *Handler) publicEnabled() bool {
	return h.Repo.GetSettingDefault("public.enabled", "1") == "1"
}
