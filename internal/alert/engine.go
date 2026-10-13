// Package alert 实现告警规则判定、收敛、冷却与分发（文档 4.5 / 8.4）。
//
// 关键约束：
//   - 离线与探测失败需连续 N 次确认（默认 3）；指标越阈值需持续一段时间（默认 5 分钟）。
//   - 每个 (节点, 事件类型) 独立冷却窗口（默认 60 分钟），状态存 alert_state。
//   - 恢复事件不受冷却限制，但只在对应异常已通知过的前提下发送。
//   - 不可信字符串在输出端转义（13.4.4 / S1）。
package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// 事件类型（4.5）。
const (
	KindOffline   = "offline"
	KindCPU       = "cpu"
	KindMem       = "mem"
	KindDisk      = "disk"
	KindTraffic   = "traffic"
	KindExpire    = "expire"
	KindProbeFail = "probe_fail"
)

// 事件级别。
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Params 是告警规则的参数（alert_rules.params，JSON）。
type Params struct {
	Threshold int   `json:"threshold,omitempty"` // 百分比 ×100
	DurationS int   `json:"duration_s,omitempty"`
	ConfirmN  int   `json:"confirm_n,omitempty"` // 连续确认次数
	Levels    []int `json:"levels,omitempty"`    // 流量梯级，如 [80,95,100]
}

// Rule 对应 alert_rules 表。
type Rule struct {
	Kind      string `json:"kind"`
	Enabled   bool   `json:"enabled"`
	Params    Params `json:"params"`
	CooldownS int    `json:"cooldown_s"`
}

// DefaultRules 返回内置默认规则（4.5）。
func DefaultRules() map[string]Rule {
	return map[string]Rule{
		KindOffline: {Kind: KindOffline, Enabled: true,
			Params: Params{ConfirmN: 3}, CooldownS: 3600},
		KindCPU: {Kind: KindCPU, Enabled: true,
			Params: Params{Threshold: 9000, DurationS: 300}, CooldownS: 3600},
		KindMem: {Kind: KindMem, Enabled: true,
			Params: Params{Threshold: 9000, DurationS: 300}, CooldownS: 3600},
		KindDisk: {Kind: KindDisk, Enabled: true,
			Params: Params{Threshold: 9000, DurationS: 300}, CooldownS: 3600},
		KindTraffic: {Kind: KindTraffic, Enabled: true,
			Params: Params{Levels: []int{80, 95, 100}}, CooldownS: 21600},
		KindExpire: {Kind: KindExpire, Enabled: true,
			Params: Params{}, CooldownS: 86400},
		KindProbeFail: {Kind: KindProbeFail, Enabled: true,
			Params: Params{ConfirmN: 3}, CooldownS: 3600},
	}
}

// AlertState 对应 alert_state 表。
type AlertState struct {
	NodeID     int64  `json:"node_id"`
	Kind       string `json:"kind"` // 如 offline、traffic:80、expire:7
	State      string `json:"state"`
	Since      int64  `json:"since"`
	LastSentAt int64  `json:"last_sent_at"`
	Detail     string `json:"detail,omitempty"`
}

// Event 对应 alert_events 表。
type Event struct {
	ID        int64  `json:"id"`
	NodeID    int64  `json:"node_id"`
	Kind      string `json:"kind"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	CreatedAt int64  `json:"created_at"`
	Delivered bool   `json:"delivered"`
}

// Store 是告警引擎依赖的持久层。
type Store interface {
	GetAlertState(nodeID int64, kind string) (*AlertState, error)
	UpsertAlertState(st AlertState) error
	InsertAlertEvent(ev Event) (int64, error)
	MarkDelivered(id int64) error
}

// ChannelProvider 返回**当前**启用的告警渠道。
//
// 为什么是函数而不是一份切片：渠道是管理员在后台随时增删改的运行时设置，
// 而引擎活得比任何一次配置修改都长。把切片塞进构造函数里，等于在启动那一刻
// 把"渠道清单"拍平成快照 —— 之后在后台新建的 Telegram 渠道，告警**永远走不到**它。
//
// 真机上的症状极具误导性：后台点"测试发送"能收到（那条路是现读设置、现建渠道），
// 但真出事时一条告警都收不到，而告警事件表里 delivered=0、日志里没有任何报错。
// 这与站点形态那次的教训是同一个：**清单必须每次现读**，冻结进闭包/构造器
// 得到的"改完即时生效"是假的，而且不报错。
type ChannelProvider func() []Channel

// Engine 负责判定、收敛、冷却与分发。
type Engine struct {
	rules map[string]Rule
	store Store
	// chans 是"每次分发时现取渠道"的提供者，不是渠道快照。见 ChannelProvider。
	chans ChannelProvider

	// 连续确认计数：(nodeID, kind) -> 计数与首次时间
	mu       sync.Mutex
	streak   map[streakKey]streakVal
	nodeName func(nodeID int64) string
}

type streakKey struct{ node, kind string }
type streakVal struct {
	count    int
	firstAt  int64
	lastSeen int64
}

// New 用一份**固定的**渠道列表创建告警引擎。
//
// 只适合"渠道在进程生命周期内不会变"的场景（测试、以及把渠道写死在配置文件里的用法）。
// 从数据库读渠道的 Master **必须**用 NewDynamic：否则后台新建的渠道对告警不可见。
func New(rules map[string]Rule, store Store, channels []Channel) *Engine {
	return NewDynamic(rules, store, func() []Channel { return channels })
}

// NewDynamic 用"每次分发现取"的提供者创建告警引擎（Master 走这条）。
//
// provide 会在**每次真正要投递**时被调用一次（不是每个采样点），
// 所以它可以安全地读数据库、解密渠道配置：告警是低频事件，而且此时
// "渠道是不是最新的"比"省一次查询"重要得多。
func NewDynamic(rules map[string]Rule, store Store, provide ChannelProvider) *Engine {
	if rules == nil {
		rules = DefaultRules()
	}
	if provide == nil {
		provide = func() []Channel { return nil }
	}
	return &Engine{
		rules:  rules,
		store:  store,
		chans:  provide,
		streak: make(map[streakKey]streakVal),
	}
}

// SetChannelProvider 替换渠道提供者（测试与热重载用）。
func (e *Engine) SetChannelProvider(provide ChannelProvider) {
	if provide == nil {
		provide = func() []Channel { return nil }
	}
	e.chans = provide
}

// SetNodeNameResolver 注入节点名解析（用于消息文本）。
func (e *Engine) SetNodeNameResolver(fn func(nodeID int64) string) { e.nodeName = fn }

// Observe 记录一次条件命中，返回是否达到触发门槛（连续 N 次或持续 T 秒，4.5）。
//
// 等价于 ObserveKind(nodeID, kind, kind, now)，用于"规则键即状态键"的简单类型
// （offline / cpu / mem / disk）。
func (e *Engine) Observe(nodeID int64, kind string, now int64) bool {
	return e.ObserveKind(nodeID, kind, kind, now)
}

// ObserveKind 是 Observe 的完整形式：规则按 ruleKind 查，连续计数按 stateKind 分开。
//
// 为什么必须把两者拆开：带梯级的复合状态键（traffic:80、probe_fail:12）在规则表里
// 并不存在，拿复合键去查规则会**命中不到**——而 Observe 把"规则不存在"与
// "规则被停用"一样当作"不触发"，于是复合键永远达不到门槛（静默失效）。
// 反过来，若统一用基础键计数，"每个目标的连续失败次数"会塌缩成"节点级的失败次数"：
// 目标 A 第 1 轮失败、目标 B 第 2、3 轮失败会被算成同一条连续记录，
// ConfirmN=3 只用两轮就满足，与 4.5 的"连续 N 次确认"不符。
// alert_state 本来就是按 stateKind 落库的，这里保持同一维度。
func (e *Engine) ObserveKind(nodeID int64, ruleKind, stateKind string, now int64) bool {
	rule, ok := e.rules[ruleKind]
	if !ok || !rule.Enabled {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	key := streakKey{fmt.Sprint(nodeID), stateKind}
	v := e.streak[key]
	if v.count == 0 {
		v.firstAt = now
	}
	v.count++
	v.lastSeen = now
	e.streak[key] = v

	confirm := rule.Params.ConfirmN
	if confirm <= 0 {
		confirm = 1
	}
	if v.count < confirm {
		return false
	}
	if rule.Params.DurationS > 0 && now-v.firstAt < int64(rule.Params.DurationS) {
		return false
	}
	return true
}

// Clear 清除连续确认计数（条件恢复时调用）。
func (e *Engine) Clear(nodeID int64, kind string) {
	e.mu.Lock()
	delete(e.streak, streakKey{fmt.Sprint(nodeID), kind})
	e.mu.Unlock()
}

// Fire 在已确认的条件下触发一次告警（含冷却判定）。
// kind 可为带梯级的复合键，如 "traffic:80"、"expire:7"。
func (e *Engine) Fire(ctx context.Context, nodeID int64, baseKind, stateKind, level, detail string, now int64) error {
	rule, ok := e.rules[baseKind]
	if !ok || !rule.Enabled {
		return nil
	}

	st, err := e.store.GetAlertState(nodeID, stateKind)
	if err != nil {
		return err
	}

	// 冷却：已 firing 且在冷却窗口内则抑制（4.5）。
	if st != nil && st.State == "firing" && now-st.LastSentAt < int64(rule.CooldownS) {
		return nil
	}

	name := fmt.Sprintf("node#%d", nodeID)
	if e.nodeName != nil {
		name = e.nodeName(nodeID)
	}
	msg := BuildMessage(level, name, stateKind, detail)

	ev := Event{NodeID: nodeID, Kind: stateKind, Level: level, Message: msg, CreatedAt: now}
	id, err := e.store.InsertAlertEvent(ev)
	if err != nil {
		return err
	}

	// 分发到所有启用渠道；失败按指数退避重试 3 次，仍失败标记 delivered=0（4.5）。
	if e.dispatch(ctx, Message{Title: fmt.Sprintf("[%s] %s", strings.ToUpper(level), name), Body: detail}) {
		_ = e.store.MarkDelivered(id)
	}

	since := now
	if st != nil && st.State == "firing" {
		since = st.Since
	}
	return e.store.UpsertAlertState(AlertState{
		NodeID: nodeID, Kind: stateKind, State: "firing",
		Since: since, LastSentAt: now, Detail: detail,
	})
}

// Recover 条件恢复：置 ok 并发一次恢复通知（不受冷却限制，4.5）。
func (e *Engine) Recover(ctx context.Context, nodeID int64, stateKind, detail string, now int64) error {
	st, err := e.store.GetAlertState(nodeID, stateKind)
	if err != nil {
		return err
	}
	if st == nil || st.State != "firing" {
		return nil // 未通知过异常，不发恢复
	}

	name := fmt.Sprintf("node#%d", nodeID)
	if e.nodeName != nil {
		name = e.nodeName(nodeID)
	}
	id, err := e.store.InsertAlertEvent(Event{
		NodeID: nodeID, Kind: stateKind, Level: LevelInfo,
		Message: BuildMessage(LevelInfo, name, stateKind, "已恢复："+detail), CreatedAt: now,
	})
	if err != nil {
		return err
	}
	if e.dispatch(ctx, Message{Title: "[OK] " + name, Body: "已恢复：" + detail}) {
		_ = e.store.MarkDelivered(id)
	}
	return e.store.UpsertAlertState(AlertState{
		NodeID: nodeID, Kind: stateKind, State: "ok", Since: now, LastSentAt: now, Detail: detail,
	})
}

// dispatch 向所有渠道投递，任一渠道成功即视为已送达。
//
// 渠道列表在这里**现取**（见 ChannelProvider）：管理员刚在后台加的渠道，
// 下一条告警就该走到它，不需要重启 Master。
func (e *Engine) dispatch(ctx context.Context, m Message) bool {
	chans := e.chans()
	if len(chans) == 0 {
		return false
	}
	okAny := false
	for _, c := range chans {
		if err := sendWithRetry(ctx, c, m); err == nil {
			okAny = true
		}
	}
	return okAny
}

// sendWithRetry 指数退避重试 3 次（4.5）。
//
// 退避只发生在两次尝试**之间**。这里曾在最后一次失败后也睡了一轮：退避的作用
// 是把相邻尝试错开，最后那次之后已经无事可做，纯粹白等 2 秒。而 Fire 是在
// scheduler 的节点循环里同步调用的，节点一多（每轮每节点一次 Fire）就会把整个
// 调度循环拖住——配置了一个坏的告警渠道，连采集数据都会跟着延迟。
func sendWithRetry(ctx context.Context, c Channel, m Message) error {
	var err error
	backoff := 500 * time.Millisecond
	const attempts = 3
	for i := 0; i < attempts; i++ {
		if err = c.Send(ctx, m); err == nil {
			return nil
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return err
}

// ParseParams 从 alert_rules.params 的 JSON 解析参数。
func ParseParams(raw string) (Params, error) {
	var p Params
	if strings.TrimSpace(raw) == "" {
		return p, nil
	}
	err := json.Unmarshal([]byte(raw), &p)
	return p, err
}
