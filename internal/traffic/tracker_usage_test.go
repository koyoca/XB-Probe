package traffic

import (
	"testing"
	"time"

	"github.com/koyoca/xb-probe/internal/model"
)

// 后台「手工矫正本周期已用流量」的回归护栏（问题 10）。
//
// 用户原话：「后台可矫正节点的流量使用情况」。
//
// 这个功能最容易被写成一个"看起来能跑"的实现：直接 UPSERT 一行 traffic_state
// 就返回 200。它会通过所有"库里数值对不对"的断言，然后在真实运行中**必定失效**，
// 因为 Tracker 才是运行期的权威、traffic_state 只是它的落盘副本：
//
//	① 不写内存 → 下一次 flushLocked（≤60 秒）拿内存旧值把矫正值原样写回；
//	② 不重算 period_start → 紧随其后的第一次上报命中"周期切换"分支，
//	   把矫正值当作上一周期的余量清零；
//	③ 不登记累加器 → 调度器的 refreshRuntimeBilling 每 60 秒用 tracker.Used
//	   重算看板用量，而 Used 对没有累加器的节点返回 0。
//
// 三条都是"数字自己变回去、全程无报错"，所以三条都必须单独钉。
//
// ---------------------------------------------------------------------------
// 脚手架
// ---------------------------------------------------------------------------

// fakeStore 是 traffic.Store 的内存替身。
type fakeStore struct {
	baselines map[string]NICBaseline
	nicLoads  int
	nicSaves  int

	hasState     bool
	periodStart  int64
	stateRx      int64
	stateTx      int64
	stateSaveCnt int
}

func newFakeStore() *fakeStore {
	return &fakeStore{baselines: map[string]NICBaseline{}}
}

func (f *fakeStore) LoadNICBaselines(int64) (map[string]NICBaseline, error) {
	f.nicLoads++
	out := make(map[string]NICBaseline, len(f.baselines))
	for k, v := range f.baselines {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) SaveNICBaselines(_ int64, m map[string]NICBaseline) error {
	f.nicSaves++
	for k, v := range m {
		f.baselines[k] = v
	}
	return nil
}

func (f *fakeStore) LoadTrafficState(int64) (int64, int64, int64, bool, error) {
	return f.periodStart, f.stateRx, f.stateTx, f.hasState, nil
}

func (f *fakeStore) SaveTrafficState(_ int64, periodStart, rx, tx int64) error {
	f.stateSaveCnt++
	f.hasState = true
	f.periodStart, f.stateRx, f.stateTx = periodStart, rx, tx
	return nil
}

func (f *fakeStore) AddTrafficDaily(int64, string, int64, int64) error { return nil }

// monthlyUTC 是测试用的计费口径：每月 1 日、UTC。
func monthlyUTC(int64) (int, string, bool) { return 1, "UTC", true }

func TestSetUsageLandsInStoreAndMemory(t *testing.T) {
	st := newFakeStore()
	// 库里已有 eth0 的累计值基线。矫正**必须**把它一起装载进内存，
	// 否则紧随其后的第一次上报会把 eth0 当成"首次见到"（ComputeDelta
	// 规则 1 → 增量为 0），白丢一个上报间隔的流量。
	st.baselines["eth0"] = NICBaseline{LastRx: 1000, LastTx: 2000, BootTime: 7, UpdatedAt: 1, Valid: true}
	tr := NewTracker(st, monthlyUTC)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	oct1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix()

	const wantRx = int64(5) << 30
	const wantTx = int64(1) << 30
	if err := tr.SetUsage(7, wantRx, wantTx, now); err != nil {
		t.Fatalf("SetUsage 失败：%v", err)
	}

	// ① 落库。
	if !st.hasState || st.stateRx != wantRx || st.stateTx != wantTx {
		t.Fatalf("未落库：hasState=%v rx=%d tx=%d", st.hasState, st.stateRx, st.stateTx)
	}
	if st.periodStart != oct1 {
		t.Fatalf("周期起点应为 %d（10 月 1 日），得到 %d", oct1, st.periodStart)
	}
	// ② 内存（运行期权威）。
	if rx, tx, ok := tr.Used(7); !ok || rx != wantRx || tx != wantTx {
		t.Fatalf("内存态未同步：ok=%v rx=%d tx=%d", ok, rx, tx)
	}
	// ③ 不许连带重写网卡基线。顺带重写会把长期离线节点的 updated_at 抬到
	//    "现在"，CleanupNICBaselines 的 7 天判定就永远追不上它们。
	if st.nicSaves != 0 {
		t.Fatalf("SetUsage 不该写网卡基线，却写了 %d 次", st.nicSaves)
	}

	// ④ 紧随其后的上报：矫正值必须还在，且只叠加这一轮的增量。
	drx, dtx, reset := tr.Add(7, map[string]model.NICCounters{"eth0": {Rx: 1100, Tx: 2100}}, 7, 64, now.Add(10*time.Second))
	if reset {
		t.Fatal("矫正之后不应判定为周期切换")
	}
	if drx != 100 || dtx != 100 {
		t.Fatalf("增量应为 100/100（基线必须被装载），得到 %d/%d", drx, dtx)
	}
	if rx, tx, _ := tr.Used(7); rx != wantRx+100 || tx != wantTx+100 {
		t.Fatalf("矫正值被上报覆盖了：rx=%d tx=%d", rx, tx)
	}
}

// TestSetUsageRecomputesStalePeriodStart 钉"周期起点必须按现在重算"。
//
// 场景：这台机器上个月报过一次就再没上报（或者 Master 刚重启、库里留着上个月
// 的行）。若沿用记录里的旧起点，矫正值会在**下一次上报**时被判为"上一周期的
// 余量"清零——管理员填完 4 KB，10 秒后变成 0。
func TestSetUsageRecomputesStalePeriodStart(t *testing.T) {
	st := newFakeStore()
	st.hasState = true
	st.periodStart = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix()
	st.stateRx, st.stateTx = 111, 222

	tr := NewTracker(st, monthlyUTC)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	oct1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix()

	if err := tr.SetUsage(7, 4096, 8192, now); err != nil {
		t.Fatalf("SetUsage 失败：%v", err)
	}
	if st.periodStart != oct1 {
		t.Fatalf("周期起点未按现在重算：得到 %d，应为 %d", st.periodStart, oct1)
	}

	_, _, reset := tr.Add(7, map[string]model.NICCounters{}, 0, 64, now.Add(time.Second))
	if reset {
		t.Fatal("矫正值被判成上一周期的余量清零了（周期起点没有重算）")
	}
	if rx, tx, _ := tr.Used(7); rx != 4096 || tx != 8192 {
		t.Fatalf("矫正值被清零：rx=%d tx=%d", rx, tx)
	}
}

// TestSetUsageRegistersNodeThatNeverReported 钉"没有累加器的节点也要能被 Used 读到"。
//
// 调度器的 refreshRuntimeBilling 每 60 秒用 tracker.Used 重算看板用量，而 Used
// 对不在 accs 里的节点返回 (0,0,false)。不登记的话，"矫正一台当前离线节点"的
// 效果活不过一个慢循环——管理员每次刷新页面都会看到它又变回 0。
func TestSetUsageRegistersNodeThatNeverReported(t *testing.T) {
	st := newFakeStore() // 既没有基线，也没有周期记录
	tr := NewTracker(st, monthlyUTC)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

	if err := tr.SetUsage(42, 1234, 5678, now); err != nil {
		t.Fatalf("SetUsage 失败：%v", err)
	}
	rx, tx, ok := tr.Used(42)
	if !ok || rx != 1234 || tx != 5678 {
		t.Fatalf("从未上报过的节点被矫正后 Used 必须返回矫正值：ok=%v rx=%d tx=%d", ok, rx, tx)
	}
	if st.nicLoads == 0 {
		t.Fatal("未装载网卡基线：第一次上报会把每张网卡都当成首次见到，白丢一轮流量")
	}
}
