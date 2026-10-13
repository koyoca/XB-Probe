package model

import (
	"sort"
	"strings"
)

// 公开看板的**卡片级**开关（settings.public.cards，7.3）。
//
// 与 public.fields 的分工：
//   - public.fields 决定"某个字段的数据发不发给前端"（安全/信息粒度的收窄）；
//   - public.cards  决定"某张卡片显示不显示"（版面可控性）。
//
// 两者互补且互不替代。把 kpi_fee 从 cards 里去掉只是不渲染那张卡（数据仍在
// 响应里）；把 monthly_fee 从 fields 里去掉才是连数据都不再下发。
//
// 关闭时不隐藏数据本身，是因为"卡片太多想精简版面"和"这个数据不对外开放"
// 是两个完全不同的诉求，用同一个开关表达会让管理员误判自己关掉的是哪一个。

// 公开卡片的三个分组。
const (
	PublicCardGroupOverview = "overview" // 列表页顶部汇总卡
	PublicCardGroupDetail   = "detail"   // 详情页信息小卡（.dinfo 六格）
	PublicCardGroupNode     = "node"     // 列表页节点卡片上的区块
)

// PublicCardsAll 是 public.cards 的"不限制"哨兵值，也是出厂默认值。
//
// 为什么需要哨兵而不是用空串表示不限制：设成空串的话，"管理员取消了全部
// 勾选"和"从没配过"会变成同一个值，前者会被当成后者——管理员明明关掉了
// 所有卡片，页面却把 11 张全渲染出来。这类"开关关不掉"是最难受的缺陷方向。
//
// 语义：
//
//	"all"           → 不限制，全部展示（默认）
//	""              → 一张都不展示
//	"kpi_fee,kpi_net"... → 只展示列出的这些
const PublicCardsAll = "all"

// PublicCard 描述一张可开关的公开卡片。
type PublicCard struct {
	Key   string `json:"key"`
	Group string `json:"group"`
	Name  string `json:"name"`
	Desc  string `json:"desc"`
}

// publicCardOrder 是卡片全集，顺序即后台展示顺序。
var publicCardOrder = []PublicCard{
	// --- 列表页汇总卡 ---
	{"kpi_nodes", PublicCardGroupOverview, "汇总 · 节点", "在线 / 总数与离线数"},
	{"kpi_busy", PublicCardGroupOverview, "汇总 · 最忙节点", "CPU 占用最高的那一台"},
	{"kpi_today", PublicCardGroupOverview, "汇总 · 今日流量", "全站今日上下行与累计"},
	{"kpi_net", PublicCardGroupOverview, "汇总 · 实时网速", "全站聚合带宽与迷你趋势"},
	{"kpi_fee", PublicCardGroupOverview, "汇总 · 月费合计", "所有公开节点的月均费用合计"},
	// --- 节点卡片上的区块 ---
	//
	// 这几张是**列表页每张节点卡片内部**的区块，与详情页的 info_* 小卡是
	// 两套独立版面：详情页那六格管的是"点进去之后看到的系统/CPU/内存…"，
	// 这里管的是"列表页一眼扫过去时每张卡上那四条进度条"。
	//
	// 曾经这里只有 lat_bars 一个键，CPU / 内存 / 磁盘 / 流量四条进度条与
	// 下方的实时速率两行**根本没有开关**——updateCard() 无条件渲染。于是
	// 管理员在「公开看板」里取消勾选 info_mem 之后，详情页那格消失了、
	// 列表页每张卡上的「内存」进度条却还在，看起来就是"开关关不掉"。
	// 补上这五个键，勾掉哪块就少哪块，并按剩余块数重排列数。
	{"node_cpu", PublicCardGroupNode, "节点卡 · CPU", "CPU 使用率进度条（含核数与 1 分钟负载）"},
	{"node_mem", PublicCardGroupNode, "节点卡 · 内存", "内存使用率进度条（含已用 / 总量）"},
	{"node_disk", PublicCardGroupNode, "节点卡 · 磁盘", "磁盘使用率进度条（含已用 / 总量）"},
	{"node_traffic", PublicCardGroupNode, "节点卡 · 流量", "本周期流量进度条（含已用 / 额度）"},
	{"node_fee", PublicCardGroupNode, "节点卡 · 续费价格", "折算到月均的续费金额（年付÷12、季付÷3；终身/一次性显示文案）"},
	{"node_net", PublicCardGroupNode, "节点卡 · 实时速率", "卡片底部的实时速率与本周期累计上下行两行"},
	{"lat_bars", PublicCardGroupNode, "节点卡 · 延迟情况", "每条探测目标两行点阵：延迟与丢包（时间窗由 public.lat_window 决定，悬浮看分段详情）"},
	// --- 详情页信息小卡 ---
	{"info_system", PublicCardGroupDetail, "详情 · 系统", "操作系统与指令集架构"},
	{"info_cpu", PublicCardGroupDetail, "详情 · CPU", "核数、使用率与 1 分钟负载"},
	{"info_mem", PublicCardGroupDetail, "详情 · 内存 / 磁盘", "内存与磁盘的已用 / 总量、交换区"},
	{"info_today", PublicCardGroupDetail, "详情 · 今日流量 / 速率", "节点本地时区下的今日流量与实时速率"},
	{"info_period", PublicCardGroupDetail, "详情 · 本周期流量", "计费周期内的用量与额度占比"},
	{"info_uptime", PublicCardGroupDetail, "详情 · 运行时长 / 到期", "开机时长与剩余到期天数"},
}

// PublicCardList 返回卡片全集（按固定顺序）。
func PublicCardList() []PublicCard {
	out := make([]PublicCard, len(publicCardOrder))
	copy(out, publicCardOrder)
	return out
}

// PublicCardNames 返回卡片键全集。
func PublicCardNames() []string {
	out := make([]string, 0, len(publicCardOrder))
	for _, c := range publicCardOrder {
		out = append(out, c.Key)
	}
	return out
}

// ParsePublicCards 解析逗号/空白分隔的卡片键列表。
//
// 返回值约定：
//
//	nil        —— 不限制，全部展示（值为 "all"）
//	非 nil（含空 map）—— 只展示值为 true 的键；空 map 表示一张都不展示
//
// 未知键直接丢弃（校验阶段已拦下，这里只是防御）。
func ParsePublicCards(s string) map[string]bool {
	known := make(map[string]bool, len(publicCardOrder))
	for _, c := range publicCardOrder {
		known[c.Key] = true
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == ';'
	})
	for _, part := range parts {
		if strings.EqualFold(strings.TrimSpace(part), PublicCardsAll) {
			return nil
		}
	}
	m := map[string]bool{}
	for _, part := range parts {
		k := strings.ToLower(strings.TrimSpace(part))
		if known[k] {
			m[k] = true
		}
	}
	return m
}

// FormatPublicCardList 把掩码还原为设置项字符串（按固定顺序，便于比对）。
func FormatPublicCardList(m map[string]bool) string {
	if m == nil {
		return PublicCardsAll
	}
	out := make([]string, 0, len(m))
	for _, c := range publicCardOrder {
		if m[c.Key] {
			out = append(out, c.Key)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// DefaultPublicCards 返回出厂默认值（不限制）。
func DefaultPublicCards() string { return PublicCardsAll }
