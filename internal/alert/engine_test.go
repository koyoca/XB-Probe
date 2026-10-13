package alert

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 内存 Store
// ---------------------------------------------------------------------------

// fakeStore 是 alert.Store 的内存实现。
//
// 刻意返回 state 的**副本**（而不是内部对象）：真实实现是每次 SELECT 出一行新对象，
// 若引擎误以为"改返回的对象就等于落库"，这个 fake 会把这个错误暴露出来——
// 引擎必须显式调 UpsertAlertState。
type fakeStore struct {
	mu     sync.Mutex
	states map[string]AlertState
	events []Event
	nextID int64
	marked []int64

	getErr    error
	upsertErr error
	insertErr error
	markErr   error
}

func newFakeStore() *fakeStore {
	return &fakeStore{states: map[string]AlertState{}}
}

func stateKey(nodeID int64, kind string) string { return fmt.Sprintf("%d|%s", nodeID, kind) }

func (f *fakeStore) GetAlertState(nodeID int64, kind string) (*AlertState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	st, ok := f.states[stateKey(nodeID, kind)]
	if !ok {
		return nil, nil
	}
	cp := st
	return &cp, nil
}

func (f *fakeStore) UpsertAlertState(st AlertState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.states[stateKey(st.NodeID, st.Kind)] = st
	return nil
}

func (f *fakeStore) InsertAlertEvent(ev Event) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	f.nextID++
	ev.ID = f.nextID
	f.events = append(f.events, ev)
	return ev.ID, nil
}

func (f *fakeStore) MarkDelivered(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked = append(f.marked, id)
	// 真实实现是 `UPDATE alert_events SET delivered=1 WHERE id=?`。
	// fake 必须同样把事件本身的标记翻过来，否则"是否已送达"的断言就是空断言
	// ——只检查调用记录，查不出"调用到了但没生效"。
	for i := range f.events {
		if f.events[i].ID == id {
			f.events[i].Delivered = true
		}
	}
	return f.markErr
}

// 以下为测试辅助（都加锁，避免与并发用例互相干扰）。

func (f *fakeStore) seed(st AlertState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[stateKey(st.NodeID, st.Kind)] = st
}

func (f *fakeStore) eventsCopy() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Event(nil), f.events...)
}

func (f *fakeStore) stateOf(nodeID int64, kind string) (AlertState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.states[stateKey(nodeID, kind)]
	return st, ok
}

func (f *fakeStore) markedCopy() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.marked...)
}

// ---------------------------------------------------------------------------
// 记录型渠道
// ---------------------------------------------------------------------------

type recChannel struct {
	name  string
	fail  int // 前 n 次调用返回错误
	calls int
	msgs  []Message
}

func (c *recChannel) Name() string { return c.name }

func (c *recChannel) Send(_ context.Context, m Message) error {
	c.calls++
	if c.fail > 0 {
		c.fail--
		return errors.New("通道故障")
	}
	c.msgs = append(c.msgs, m)
	return nil
}

// ---------------------------------------------------------------------------
// 规则默认值
// ---------------------------------------------------------------------------

// TestDefaultRulesPinDocumentedDefaults 钉住 4.5 的内置默认规则。
//
// 这些数字直接决定"多久算离线""什么算超阈值"，改错一个就会让告警要么永不触发、
// 要么刷屏，而在本地演示里两种情况都不容易被发现。
func TestDefaultRulesPinDocumentedDefaults(t *testing.T) {
	rules := DefaultRules()

	if len(rules) != 7 {
		t.Fatalf("内置规则应为 7 条，得到 %d 条", len(rules))
	}

	type want struct {
		enabled   bool
		threshold int
		durationS int
		confirmN  int
		levels    []int
		cooldownS int
	}
	expect := map[string]want{
		KindOffline:   {true, 0, 0, 3, nil, 3600},
		KindCPU:       {true, 9000, 300, 0, nil, 3600},
		KindMem:       {true, 9000, 300, 0, nil, 3600},
		KindDisk:      {true, 9000, 300, 0, nil, 3600},
		KindTraffic:   {true, 0, 0, 0, []int{80, 95, 100}, 21600},
		KindExpire:    {true, 0, 0, 0, nil, 86400},
		KindProbeFail: {true, 0, 0, 3, nil, 3600},
	}

	for kind, w := range expect {
		r, ok := rules[kind]
		if !ok {
			t.Fatalf("缺少内置规则 %q", kind)
		}
		// 表里的键与结构体自带的 Kind 必须一致，否则 Fire 用 baseKind 查规则、
		// 状态却按 rule.Kind 落库，两边会分叉。
		if r.Kind != kind {
			t.Errorf("规则 %q 的 Kind 字段是 %q，两者必须一致", kind, r.Kind)
		}
		if r.Enabled != w.enabled {
			t.Errorf("规则 %q Enabled=%v，期望 %v", kind, r.Enabled, w.enabled)
		}
		if r.CooldownS != w.cooldownS {
			t.Errorf("规则 %q CooldownS=%d，期望 %d", kind, r.CooldownS, w.cooldownS)
		}
		if r.Params.Threshold != w.threshold {
			t.Errorf("规则 %q Threshold=%d，期望 %d", kind, r.Params.Threshold, w.threshold)
		}
		if r.Params.DurationS != w.durationS {
			t.Errorf("规则 %q DurationS=%d，期望 %d", kind, r.Params.DurationS, w.durationS)
		}
		if r.Params.ConfirmN != w.confirmN {
			t.Errorf("规则 %q ConfirmN=%d，期望 %d", kind, r.Params.ConfirmN, w.confirmN)
		}
		if fmt.Sprint(r.Params.Levels) != fmt.Sprint(w.levels) {
			t.Errorf("规则 %q Levels=%v，期望 %v", kind, r.Params.Levels, w.levels)
		}
	}
}

// TestDefaultRulesReturnsIndependentCopy 每次调用都必须拿到独立副本。
//
// New(nil, ...) 直接吃 DefaultRules()，后台改规则时也会走它；若返回共享 map，
// 一次 delete 就会永久删掉内置规则，重启前再也恢复不了。
func TestDefaultRulesReturnsIndependentCopy(t *testing.T) {
	first := DefaultRules()
	delete(first, KindCPU)
	first[KindDisk] = Rule{Kind: KindDisk, Enabled: false}

	second := DefaultRules()
	if len(second) != 7 {
		t.Fatalf("删改一份不应影响下一次调用，得到 %d 条", len(second))
	}
	if !second[KindDisk].Enabled {
		t.Fatal("改一份不应影响下一次调用里的 disk 规则")
	}
}

func TestNewFallsBackToDefaultRules(t *testing.T) {
	e := New(nil, newFakeStore(), nil)
	if len(e.rules) != 7 {
		t.Fatalf("rules 为 nil 时应使用内置默认规则，得到 %d 条", len(e.rules))
	}
	// cpu 默认「持续 300 秒」：第一次命中还不能触发。
	if e.Observe(1, KindCPU, 1000) {
		t.Fatal("cpu 默认规则要求持续 300 秒，首轮不应触发")
	}
	if !e.Observe(1, KindCPU, 1300) {
		t.Fatal("距首次命中满 300 秒应触发")
	}
}

// ---------------------------------------------------------------------------
// Observe：连续确认 / 持续时长
// ---------------------------------------------------------------------------

func TestObserveRequiresConfiguredConfirmations(t *testing.T) {
	rules := map[string]Rule{
		KindOffline: {Kind: KindOffline, Enabled: true, Params: Params{ConfirmN: 3}},
	}
	e := New(rules, newFakeStore(), nil)

	for i, want := range []bool{false, false, true, true} {
		if got := e.Observe(7, KindOffline, 1000); got != want {
			t.Fatalf("第 %d 次 Observe 得到 %v，期望 %v", i+1, got, want)
		}
	}
}

func TestObserveTreatsNonPositiveConfirmAsOne(t *testing.T) {
	for _, n := range []int{0, -5} {
		rules := map[string]Rule{
			KindMem: {Kind: KindMem, Enabled: true, Params: Params{ConfirmN: n}},
		}
		e := New(rules, newFakeStore(), nil)
		if !e.Observe(1, KindMem, 100) {
			t.Fatalf("ConfirmN=%d 应按 1 次确认处理，首轮即应触发", n)
		}
	}
}

// TestObserveMeasuresDurationFromFirstHit 持续时长以**首次命中**为起点。
//
// 若起点被后续命中刷新（例如用 lastSeen 计时），一个每 3 秒抖动一次的节点
// 会永远凑不满 300 秒——"持续超阈值 5 分钟"这个语义就完全失效了。
func TestObserveMeasuresDurationFromFirstHit(t *testing.T) {
	rules := map[string]Rule{
		KindCPU: {Kind: KindCPU, Enabled: true, Params: Params{Threshold: 9000, DurationS: 300}},
	}
	e := New(rules, newFakeStore(), nil)

	if e.Observe(1, KindCPU, 1000) {
		t.Fatal("首次命中的当轮不应触发")
	}
	if e.Observe(1, KindCPU, 1100) {
		t.Fatal("距首次命中 100 秒不应触发")
	}
	if e.Observe(1, KindCPU, 1299) {
		t.Fatal("距首次命中 299 秒不应触发")
	}
	// 中间命中过多次，但起点仍是 1000；这里恰好满 300 秒。
	if !e.Observe(1, KindCPU, 1300) {
		t.Fatal("距**首次**命中满 300 秒即应触发，起点不应被后续命中刷新")
	}
}

func TestObserveDisabledOrUnknownKindNeverFires(t *testing.T) {
	rules := map[string]Rule{
		KindDisk: {Kind: KindDisk, Enabled: false, Params: Params{ConfirmN: 1}},
	}
	e := New(rules, newFakeStore(), nil)

	for i := 0; i < 10; i++ {
		if e.Observe(1, KindDisk, 500) {
			t.Fatal("已禁用的规则不应触发")
		}
	}
	if e.Observe(1, "no_such_kind", 500) {
		t.Fatal("未知规则不应触发")
	}
}

func TestClearResetsStreak(t *testing.T) {
	rules := map[string]Rule{
		KindOffline: {Kind: KindOffline, Enabled: true, Params: Params{ConfirmN: 3}},
	}
	e := New(rules, newFakeStore(), nil)

	e.Observe(1, KindOffline, 1000)
	e.Observe(1, KindOffline, 1001)
	e.Clear(1, KindOffline)

	// 计数已归零，再观察两次仍不到 3 次。
	if e.Observe(1, KindOffline, 1002) {
		t.Fatal("Clear 之后的第 1 次不应触发")
	}
	if e.Observe(1, KindOffline, 1003) {
		t.Fatal("Clear 之后的第 2 次不应触发")
	}
	if !e.Observe(1, KindOffline, 1004) {
		t.Fatal("Clear 之后的第 3 次应触发")
	}

	// 清理不存在的键不应 panic。
	e.Clear(999, "no_such_kind")
}

// TestObserveStreakIsPerNodeAndPerKind 计数必须按 (节点, 类型) 隔离。
//
// 这是最容易被写错的一处：若共用一个大计数器，节点 A 的两次抖动会让节点 B
// 直接跳过确认门槛，产生"凭空离线"的误报。
func TestObserveStreakIsPerNodeAndPerKind(t *testing.T) {
	rules := map[string]Rule{
		KindOffline: {Kind: KindOffline, Enabled: true, Params: Params{ConfirmN: 3}},
		KindProbeFail: {
			Kind: KindProbeFail, Enabled: true, Params: Params{ConfirmN: 3},
		},
	}
	e := New(rules, newFakeStore(), nil)

	for i := 0; i < 3; i++ {
		e.Observe(1, KindOffline, 1000)
	}
	if !e.Observe(1, KindOffline, 1000) {
		t.Fatal("节点 1 的 offline 应已满 3 次")
	}
	if e.Observe(2, KindOffline, 1000) {
		t.Fatal("节点 2 是独立计数，第 1 次不应触发")
	}
	if e.Observe(1, KindProbeFail, 1000) {
		t.Fatal("同一节点的不同类型是独立计数，第 1 次不应触发")
	}
}

// TestObserveIsSafeUnderConcurrency 并发调用不得丢计数（也是 -race 的靶子）。
func TestObserveIsSafeUnderConcurrency(t *testing.T) {
	const n = 50
	rules := map[string]Rule{
		KindOffline: {Kind: KindOffline, Enabled: true, Params: Params{ConfirmN: n}},
	}
	e := New(rules, newFakeStore(), nil)

	var triggered int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e.Observe(1, KindOffline, 1000) {
				atomic.AddInt64(&triggered, 1)
			}
		}()
	}
	wg.Wait()

	if triggered != 1 {
		t.Fatalf("恰好第 %d 次应返回 true，实际 %d 次返回 true（计数丢失或重复触发）", n, triggered)
	}
}

// ---------------------------------------------------------------------------
// Fire：落库、冷却、梯级
// ---------------------------------------------------------------------------

func TestFireInsertsEventAndArmsCooldown(t *testing.T) {
	store := newFakeStore()
	ch := &recChannel{name: "tg"}
	e := New(nil, store, []Channel{ch})

	if err := e.Fire(context.Background(), 7, KindOffline, KindOffline, LevelError, "已离线", 1000); err != nil {
		t.Fatalf("Fire 失败: %v", err)
	}

	events := store.eventsCopy()
	if len(events) != 1 {
		t.Fatalf("应写入 1 条事件，得到 %d 条", len(events))
	}
	ev := events[0]
	if ev.NodeID != 7 || ev.Kind != KindOffline || ev.Level != LevelError || ev.CreatedAt != 1000 {
		t.Errorf("事件字段不符: %+v", ev)
	}
	if want := "ERROR | node#7 | offline | 已离线"; ev.Message != want {
		t.Errorf("事件文本 = %q，期望 %q", ev.Message, want)
	}
	if !ev.Delivered {
		t.Error("渠道发送成功时该事件应被标记为已送达")
	}
	if got := store.markedCopy(); len(got) != 1 || got[0] != ev.ID {
		t.Errorf("应标记事件 %d 已送达，实际 %v", ev.ID, got)
	}

	st, ok := store.stateOf(7, KindOffline)
	if !ok {
		t.Fatal("应写入 alert_state")
	}
	if st.State != "firing" || st.Since != 1000 || st.LastSentAt != 1000 || st.Detail != "已离线" {
		t.Errorf("状态不符: %+v", st)
	}
	if len(ch.msgs) != 1 {
		t.Fatalf("渠道应收到 1 条消息，得到 %d 条", len(ch.msgs))
	}
	if want := "[ERROR] node#7"; ch.msgs[0].Title != want {
		t.Errorf("消息标题 = %q，期望 %q", ch.msgs[0].Title, want)
	}
}

func TestFireSuppressedWithinCooldown(t *testing.T) {
	store := newFakeStore()
	e := New(nil, store, nil)

	if err := e.Fire(context.Background(), 1, KindOffline, KindOffline, LevelError, "已离线", 1000); err != nil {
		t.Fatalf("首次 Fire 失败: %v", err)
	}
	// 冷却 3600 秒：差 3599 秒仍应抑制。
	if err := e.Fire(context.Background(), 1, KindOffline, KindOffline, LevelError, "已离线", 4599); err != nil {
		t.Fatalf("冷却内 Fire 失败: %v", err)
	}
	if got := len(store.eventsCopy()); got != 1 {
		t.Fatalf("冷却窗口内不应重复写事件，得到 %d 条", got)
	}
	st, _ := store.stateOf(1, KindOffline)
	if st.LastSentAt != 1000 {
		t.Fatalf("被抑制时不应改动 LastSentAt，得到 %d", st.LastSentAt)
	}

	// 恰好满 3600 秒可再发一次，且 Since 保持首次时间（异常还在持续）。
	if err := e.Fire(context.Background(), 1, KindOffline, KindOffline, LevelError, "已离线", 4600); err != nil {
		t.Fatalf("冷却结束后 Fire 失败: %v", err)
	}
	if got := len(store.eventsCopy()); got != 2 {
		t.Fatalf("冷却结束后应再写 1 条，得到 %d 条", got)
	}
	st, _ = store.stateOf(1, KindOffline)
	if st.Since != 1000 {
		t.Errorf("异常持续期间 Since 应保持首次时间 1000，得到 %d", st.Since)
	}
	if st.LastSentAt != 4600 {
		t.Errorf("LastSentAt 应刷新为 4600，得到 %d", st.LastSentAt)
	}
}

// TestFireIgnoresCooldownWhenStateIsNotFiring 冷却只对"仍在 firing"的状态生效。
//
// 否则会出一个很难查的现象：节点恢复后又在冷却窗口内二次故障，
// 告警会被上一轮的冷却吃掉，运维只看到"已恢复"再没有下文。
func TestFireIgnoresCooldownWhenStateIsNotFiring(t *testing.T) {
	store := newFakeStore()
	store.seed(AlertState{NodeID: 1, Kind: KindOffline, State: "ok", Since: 900, LastSentAt: 999})
	e := New(nil, store, nil)

	if err := e.Fire(context.Background(), 1, KindOffline, KindOffline, LevelError, "已离线", 1000); err != nil {
		t.Fatalf("Fire 失败: %v", err)
	}
	if got := len(store.eventsCopy()); got != 1 {
		t.Fatalf("状态为 ok 时不应受冷却限制，应写 1 条事件，得到 %d 条", got)
	}
	st, _ := store.stateOf(1, KindOffline)
	if st.State != "firing" || st.Since != 1000 {
		t.Errorf("重新进入 firing 时 Since 应重置为本轮时间，得到 %+v", st)
	}
}

// TestFireCooldownIsPerTierNotPerRule 冷却窗口按 stateKind 隔离，不按规则。
//
// 流量梯级（traffic:80 / traffic:95 / traffic:100）与到期梯级（expire:7 …）
// 共用一条规则；若冷却按规则判定，"已达 80%" 发过之后 "已达 100%" 会被静默吞掉。
func TestFireCooldownIsPerTierNotPerRule(t *testing.T) {
	store := newFakeStore()
	e := New(nil, store, nil)
	ctx := context.Background()

	if err := e.Fire(ctx, 1, KindTraffic, "traffic:80", LevelWarn, "流量已达 80%", 1000); err != nil {
		t.Fatal(err)
	}
	// 同一节点、同一规则，但另一个梯级：必须能立刻发出。
	if err := e.Fire(ctx, 1, KindTraffic, "traffic:95", LevelWarn, "流量已达 95%", 1001); err != nil {
		t.Fatal(err)
	}
	if got := len(store.eventsCopy()); got != 2 {
		t.Fatalf("不同梯级各自独立冷却，应写 2 条事件，得到 %d 条", got)
	}

	// 同一个梯级在冷却内：抑制。
	if err := e.Fire(ctx, 1, KindTraffic, "traffic:80", LevelWarn, "流量已达 80%", 2000); err != nil {
		t.Fatal(err)
	}
	if got := len(store.eventsCopy()); got != 2 {
		t.Fatalf("同一梯级在冷却内应被抑制，得到 %d 条事件", got)
	}
	// 梯级状态各存一行。
	for _, kind := range []string{"traffic:80", "traffic:95"} {
		if _, ok := store.stateOf(1, kind); !ok {
			t.Errorf("应写入 %s 的状态行", kind)
		}
	}
}

func TestFireDisabledOrUnknownRuleIsNoop(t *testing.T) {
	rules := map[string]Rule{
		KindCPU: {Kind: KindCPU, Enabled: false, Params: Params{Threshold: 9000}},
	}
	store := newFakeStore()
	e := New(rules, store, nil)
	ctx := context.Background()

	if err := e.Fire(ctx, 1, KindCPU, KindCPU, LevelWarn, "超阈值", 1000); err != nil {
		t.Fatalf("禁用规则应静默返回，得到错误: %v", err)
	}
	if err := e.Fire(ctx, 1, "no_such_kind", "no_such_kind", LevelWarn, "x", 1000); err != nil {
		t.Fatalf("未知规则应静默返回，得到错误: %v", err)
	}
	if got := len(store.eventsCopy()); got != 0 {
		t.Fatalf("不应写入任何事件，得到 %d 条", got)
	}
	if len(store.states) != 0 {
		t.Fatalf("不应写入任何状态，得到 %d 行", len(store.states))
	}
}

// TestFireWithoutChannelsStillArmsCooldown 没有配渠道时事件照样要落库、冷却照常启动。
//
// 这是刻意的取舍：告警事件先进库（后台能看到 delivered=0），冷却也照常走，
// 避免"没人配渠道所以每轮都重新发一遍"。但它必须被测试写死，
// 否则以后有人顺手改成"发送失败就不写状态"会静默改变这条语义。
func TestFireWithoutChannelsStillArmsCooldown(t *testing.T) {
	store := newFakeStore()
	e := New(nil, store, nil)

	if err := e.Fire(context.Background(), 3, KindDisk, KindDisk, LevelWarn, "磁盘将满", 1000); err != nil {
		t.Fatalf("Fire 失败: %v", err)
	}
	events := store.eventsCopy()
	if len(events) != 1 {
		t.Fatalf("无论有无渠道都应写入事件，得到 %d 条", len(events))
	}
	if events[0].Delivered {
		t.Error("没有渠道时不应标记为已送达")
	}
	if got := store.markedCopy(); len(got) != 0 {
		t.Errorf("没有渠道时不应调用 MarkDelivered，实际 %v", got)
	}
	st, ok := store.stateOf(3, KindDisk)
	if !ok || st.State != "firing" {
		t.Fatalf("没有渠道时也要写入 firing 状态（否则每轮都会重发），得到 %+v", st)
	}
}

func TestFireMarksDeliveredWhenAnyChannelSucceeds(t *testing.T) {
	store := newFakeStore()
	bad := &recChannel{name: "bad", fail: 1 << 30}
	good := &recChannel{name: "good"}
	e := New(nil, store, []Channel{bad, good})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.Fire(ctx, 1, KindMem, KindMem, LevelWarn, "内存将满", 1000); err != nil {
		t.Fatalf("Fire 失败: %v", err)
	}
	if got := len(store.eventsCopy()); got != 1 {
		t.Fatalf("应写 1 条事件，得到 %d 条", got)
	}
	if !store.eventsCopy()[0].Delivered {
		t.Error("只要有一个渠道成功，事件就应标记为已送达")
	}
	if len(good.msgs) != 1 {
		t.Errorf("成功渠道应收到消息，得到 %d 条", len(good.msgs))
	}
}

func TestFireAllChannelsFailLeavesUndelivered(t *testing.T) {
	store := newFakeStore()
	failing := &recChannel{name: "bad", fail: 1 << 30}
	e := New(nil, store, []Channel{failing})

	// 用短超时的 ctx 打断退避重试，避免测试白等。
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := e.Fire(ctx, 1, KindMem, KindMem, LevelWarn, "内存将满", 1000); err != nil {
		t.Fatalf("渠道全失败不应让 Fire 报错（事件仍然落库）: %v", err)
	}
	events := store.eventsCopy()
	if len(events) != 1 {
		t.Fatalf("应写入事件，得到 %d 条", len(events))
	}
	if events[0].Delivered {
		t.Error("全部渠道失败时不应标记为已送达")
	}
	if got := store.markedCopy(); len(got) != 0 {
		t.Errorf("不应调用 MarkDelivered，实际 %v", got)
	}
}

func TestFireUsesNodeNameResolver(t *testing.T) {
	store := newFakeStore()
	e := New(nil, store, nil)
	e.SetNodeNameResolver(func(nodeID int64) string {
		if nodeID == 42 {
			return "东京\t节点" // 含制表符，输出端必须清洗
		}
		return fmt.Sprintf("node#%d", nodeID)
	})

	if err := e.Fire(context.Background(), 42, KindOffline, KindOffline, LevelError, "已离线", 1000); err != nil {
		t.Fatal(err)
	}
	msg := store.eventsCopy()[0].Message
	if !strings.Contains(msg, "东京 节点") {
		t.Errorf("应使用注入的节点名并清洗控制字符，得到 %q", msg)
	}
	if strings.Contains(msg, "\t") {
		t.Errorf("消息里不应残留制表符: %q", msg)
	}
}

func TestFireAndRecoverPropagateStoreErrors(t *testing.T) {
	ctx := context.Background()

	store := newFakeStore()
	store.getErr = errors.New("读状态失败")
	if err := New(nil, store, nil).Fire(ctx, 1, KindOffline, KindOffline, LevelError, "x", 1000); err == nil {
		t.Error("GetAlertState 失败时 Fire 应返回错误")
	}
	if err := New(nil, store, nil).Recover(ctx, 1, KindOffline, "x", 1000); err == nil {
		t.Error("GetAlertState 失败时 Recover 应返回错误")
	}

	store = newFakeStore()
	store.insertErr = errors.New("写事件失败")
	if err := New(nil, store, nil).Fire(ctx, 1, KindOffline, KindOffline, LevelError, "x", 1000); err == nil {
		t.Error("InsertAlertEvent 失败时 Fire 应返回错误")
	}

	store = newFakeStore()
	store.upsertErr = errors.New("写状态失败")
	if err := New(nil, store, nil).Fire(ctx, 1, KindOffline, KindOffline, LevelError, "x", 1000); err == nil {
		t.Error("UpsertAlertState 失败时 Fire 应返回错误")
	}
}

// TestFireIgnoresMarkDeliveredError 标记送达失败不能影响主流程。
//
// 事件已经落库、状态也已推进，此时再返回错误只会让调用方重复 Fire 一遍。
func TestFireIgnoresMarkDeliveredError(t *testing.T) {
	store := newFakeStore()
	store.markErr = errors.New("标记失败")
	e := New(nil, store, []Channel{&recChannel{name: "ok"}})

	if err := e.Fire(context.Background(), 1, KindOffline, KindOffline, LevelError, "x", 1000); err != nil {
		t.Fatalf("MarkDelivered 失败不应让 Fire 报错，得到 %v", err)
	}
	if _, ok := store.stateOf(1, KindOffline); !ok {
		t.Error("状态仍应写入")
	}
}

// ---------------------------------------------------------------------------
// Recover
// ---------------------------------------------------------------------------

func TestRecoverNoopWhenNeverNotified(t *testing.T) {
	store := newFakeStore()
	e := New(nil, store, nil)
	ctx := context.Background()

	// 从未有过状态。
	if err := e.Recover(ctx, 1, KindOffline, "恢复上报", 2000); err != nil {
		t.Fatalf("无状态时 Recover 不应报错: %v", err)
	}
	if got := len(store.eventsCopy()); got != 0 {
		t.Fatalf("没通知过异常就不该发恢复通知，得到 %d 条", got)
	}

	// 状态是 ok。
	store.seed(AlertState{NodeID: 1, Kind: KindOffline, State: "ok", Since: 1, LastSentAt: 1})
	if err := e.Recover(ctx, 1, KindOffline, "恢复上报", 2000); err != nil {
		t.Fatal(err)
	}
	if got := len(store.eventsCopy()); got != 0 {
		t.Fatalf("状态已是 ok 时不应再发恢复通知，得到 %d 条", got)
	}
}

func TestRecoverNotifiesOnceAndResetsState(t *testing.T) {
	store := newFakeStore()
	// 配上渠道，才能断言"恢复通知确实送达并被标记"。
	e := New(nil, store, []Channel{&recChannel{name: "tg"}})
	ctx := context.Background()

	if err := e.Fire(ctx, 5, KindOffline, KindOffline, LevelError, "已离线", 1000); err != nil {
		t.Fatal(err)
	}
	if err := e.Recover(ctx, 5, KindOffline, "恢复上报", 2000); err != nil {
		t.Fatal(err)
	}

	events := store.eventsCopy()
	if len(events) != 2 {
		t.Fatalf("应有 1 条告警 + 1 条恢复，得到 %d 条", len(events))
	}
	rec := events[1]
	if rec.Level != LevelInfo {
		t.Errorf("恢复事件级别应为 info，得到 %q", rec.Level)
	}
	if want := "INFO | node#5 | offline | 已恢复：恢复上报"; rec.Message != want {
		t.Errorf("恢复文本 = %q，期望 %q", rec.Message, want)
	}
	if !rec.Delivered {
		t.Error("恢复通知发送成功后应标记已送达")
	}
	st, _ := store.stateOf(5, KindOffline)
	if st.State != "ok" || st.Since != 2000 {
		t.Errorf("恢复后状态应为 ok 且 Since=2000，得到 %+v", st)
	}

	// 再调一次不得重复发恢复。
	if err := e.Recover(ctx, 5, KindOffline, "恢复上报", 3000); err != nil {
		t.Fatal(err)
	}
	if got := len(store.eventsCopy()); got != 2 {
		t.Fatalf("恢复通知只能发一次，得到 %d 条事件", got)
	}
}

// TestRecoverIsNotSubjectToCooldown 恢复通知不受冷却限制（4.5）。
//
// 若恢复也被冷却拦住，会出现"节点早就回来了，看板上还挂着离线告警"。
func TestRecoverIsNotSubjectToCooldown(t *testing.T) {
	store := newFakeStore()
	e := New(nil, store, nil)
	ctx := context.Background()

	if err := e.Fire(ctx, 1, KindOffline, KindOffline, LevelError, "已离线", 1000); err != nil {
		t.Fatal(err)
	}
	// 仅隔 1 秒就恢复，远小于 3600 秒冷却。
	if err := e.Recover(ctx, 1, KindOffline, "恢复上报", 1001); err != nil {
		t.Fatal(err)
	}
	events := store.eventsCopy()
	if len(events) != 2 {
		t.Fatalf("恢复应在冷却窗口内照常发出，得到 %d 条事件", len(events))
	}
	if events[1].Level != LevelInfo {
		t.Errorf("第 2 条应为恢复通知，得到级别 %q", events[1].Level)
	}
}

// ---------------------------------------------------------------------------
// sendWithRetry
// ---------------------------------------------------------------------------

func TestSendWithRetrySucceedsWithoutRetry(t *testing.T) {
	c := &recChannel{name: "ok"}
	start := time.Now()
	if err := sendWithRetry(context.Background(), c, Message{Title: "t"}); err != nil {
		t.Fatalf("应成功: %v", err)
	}
	if c.calls != 1 {
		t.Fatalf("首次成功就应返回，实际调用 %d 次", c.calls)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("首次成功不应退避，耗时 %v", elapsed)
	}
}

func TestSendWithRetryRecoversAfterTransientFailure(t *testing.T) {
	c := &recChannel{name: "flaky", fail: 2} // 失败 2 次后成功
	start := time.Now()
	if err := sendWithRetry(context.Background(), c, Message{}); err != nil {
		t.Fatalf("第 3 次应成功: %v", err)
	}
	if c.calls != 3 {
		t.Fatalf("应尝试 3 次，实际 %d 次", c.calls)
	}
	// 两次退避 500ms + 1s。
	if elapsed := time.Since(start); elapsed < 1400*time.Millisecond {
		t.Fatalf("两次退避合计应约 1.5 秒，实际 %v", elapsed)
	}
}

// TestSendWithRetryGivesUpAfterThreeAttempts 三次仍失败就放弃，
// 且**最后一次失败后不得再退避**。
//
// 退避的唯一作用是把两次尝试错开；在最后一次失败之后睡觉纯属白等，
// 而 Fire 是在 scheduler 的节点循环里同步调用的——每个失败渠道每轮多等 2 秒，
// 节点一多就会把整个调度循环拖住。
func TestSendWithRetryGivesUpAfterThreeAttempts(t *testing.T) {
	c := &recChannel{name: "always-fail", fail: 1 << 30}
	start := time.Now()
	if err := sendWithRetry(context.Background(), c, Message{}); err == nil {
		t.Fatal("三次都失败时应返回错误")
	}
	elapsed := time.Since(start)

	if c.calls != 3 {
		t.Fatalf("应尝试 3 次，实际 %d 次", c.calls)
	}
	// 正确实现为 500ms + 1s = 约 1.5 秒；多睡一轮会到 3.5 秒。
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("三次尝试耗时 %v（>2.5s）：最后一次失败后不应再退避", elapsed)
	}
}

func TestSendWithRetryStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c := &recChannel{name: "always-fail", fail: 1 << 30}

	start := time.Now()
	err := sendWithRetry(ctx, c, Message{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回 context.DeadlineExceeded，得到 %v", err)
	}
	if c.calls != 1 {
		t.Fatalf("超时发生在首次退避期间，应只尝试 1 次，实际 %d 次", c.calls)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("应在 ctx 超时时立即返回，耗时 %v", elapsed)
	}
}

func TestDispatchWithoutChannelsReturnsFalse(t *testing.T) {
	e := New(nil, newFakeStore(), nil)
	if e.dispatch(context.Background(), Message{Title: "t"}) {
		t.Fatal("没有渠道时应报告未送达")
	}
}

// ---------------------------------------------------------------------------
// ParseParams
// ---------------------------------------------------------------------------

func TestParseParams(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    Params
		wantErr bool
	}{
		{name: "空串", raw: "", want: Params{}},
		{name: "纯空白", raw: "   \t\n", want: Params{}},
		{
			name: "完整参数",
			raw:  `{"threshold":9000,"duration_s":300,"confirm_n":3,"levels":[80,95,100]}`,
			want: Params{Threshold: 9000, DurationS: 300, ConfirmN: 3, Levels: []int{80, 95, 100}},
		},
		{name: "部分字段", raw: `{"confirm_n":3}`, want: Params{ConfirmN: 3}},
		{name: "非法 JSON", raw: `{"confirm_n":`, wantErr: true},
		{name: "类型不符", raw: `{"confirm_n":"3"}`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseParams(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，得到 %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got.Threshold != tc.want.Threshold || got.DurationS != tc.want.DurationS ||
				got.ConfirmN != tc.want.ConfirmN || fmt.Sprint(got.Levels) != fmt.Sprint(tc.want.Levels) {
				t.Errorf("解析结果 = %+v，期望 %+v", got, tc.want)
			}
		})
	}
}
