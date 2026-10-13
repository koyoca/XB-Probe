package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koyoca/xb-probe/internal/agent/agentconfig"
	"github.com/koyoca/xb-probe/internal/agent/client"
	"github.com/koyoca/xb-probe/internal/agent/prober"
	"github.com/koyoca/xb-probe/internal/model"
)

// Agent 主循环的测试。
//
// 这里不碰网络外的任何东西：Master 用 httptest 顶替，采集器用替身（Collector 是接口）。
// 覆盖的重点全是"改了不生效"高发区——缓冲补报的顺序与上限、配置热更新的时机、
// 探测循环每轮重读配置（不变量 11 踩过的那一类）。

// ---------------------------------------------------------------------------
// 替身与脚手架
// ---------------------------------------------------------------------------

// fakeCollector 是可编程的采集器替身。
type fakeCollector struct {
	metrics *model.Metrics
	static  *model.Static
	err     error
}

func (f *fakeCollector) Sample() (*model.Metrics, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.metrics, nil
}

func (f *fakeCollector) Static() (*model.Static, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.static, nil
}

// fakeMaster 顶替 Master，记录收到的上报与配置请求。
type fakeMaster struct {
	srv *httptest.Server

	mu        sync.Mutex
	reports   []model.ReportRequest
	cfgHits   int
	lastETag  string
	version   string // 上报响应里带回的 config_version
	remote    *model.AgentConfig
	reportErr int // 非 0 时上报返回该状态码
	cfgErr    int // 非 0 时拉配置返回该状态码
}

func newFakeMaster(t *testing.T, version string) *fakeMaster {
	t.Helper()
	m := &fakeMaster{version: version}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agent/v1/report":
			if m.reportErr != 0 {
				w.WriteHeader(m.reportErr)
				_, _ = w.Write([]byte(`{"error":{"code":"boom","message":"故意失败"}}`))
				return
			}
			var req model.ReportRequest
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &req)
			m.mu.Lock()
			m.reports = append(m.reports, req)
			ver := m.version
			m.mu.Unlock()
			writeJSONAgent(t, w, model.ReportResponse{OK: true, ConfigVersion: ver})
		case "/api/agent/v1/config":
			m.mu.Lock()
			m.cfgHits++
			m.lastETag = r.Header.Get("If-None-Match")
			cfg, code := m.remote, m.cfgErr
			m.mu.Unlock()
			if code != 0 {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"error":{"code":"boom","message":"故意失败"}}`))
				return
			}
			if cfg == nil {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"无配置"}}`))
				return
			}
			writeJSONAgent(t, w, cfg)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *fakeMaster) sent() []model.ReportRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]model.ReportRequest(nil), m.reports...)
}

func (m *fakeMaster) configHits() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfgHits
}

// agentEnv 是一套可直接调 tick/probeLoop 的守护进程环境。
type agentEnv struct {
	d    *Daemon
	m    *fakeMaster
	coll *fakeCollector
}

func newAgentEnv(t *testing.T, m *fakeMaster, cfg *agentconfig.Config) *agentEnv {
	t.Helper()
	if cfg.MasterURL == "" {
		cfg.MasterURL = m.srv.URL
	}
	cli, err := client.New(client.Options{
		MasterURL: cfg.MasterURL, Token: cfg.Token,
		InsecureHTTP: strings.HasPrefix(cfg.MasterURL, "http://"),
		Timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("建客户端失败: %v", err)
	}
	coll := &fakeCollector{
		metrics: &model.Metrics{CPU: intPtr(1234)},
		static:  &model.Static{Hostname: "test-host", OS: "Debian 12", CPUCores: 4},
	}
	d := &Daemon{
		cfgPath:     filepath.Join(t.TempDir(), "agent.yaml"),
		cfg:         cfg,
		cli:         cli,
		coll:        coll,
		probe:       prober.New(cfg.AllowPrivateTargets),
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		version:     "test-1.0",
		staticDirty: true,
	}
	return &agentEnv{d: d, m: m, coll: coll}
}

func intPtr(v int) *int { return &v }

func writeJSONAgent(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("写响应失败: %v", err)
	}
}

// defaultTestConfig 返回一份"可跑"的配置（间隔取最小值以缩短测试耗时）。
func defaultTestConfig() *agentconfig.Config {
	return &agentconfig.Config{
		ReportIntervalS: 2,
		ProbeIntervalS:  1,
		BufferSamples:   100,
	}
}

// ---------------------------------------------------------------------------
// 上报与本地缓冲
// ---------------------------------------------------------------------------

// TestTickSendsStaticOnlyWhenDirty 断言 static 只在需要时携带。
//
// 上报体上限 256 KB，而 static 是每次上报里最大的一块固定开销；
// 每次都带会让 50 个节点 × 每 3 秒一次的上报量白白翻几倍。
func TestTickSendsStaticOnlyWhenDirty(t *testing.T) {
	m := newFakeMaster(t, "")
	env := newAgentEnv(t, m, defaultTestConfig())
	ctx := context.Background()

	if _, err := env.d.tick(ctx); err != nil {
		t.Fatalf("首次上报失败: %v", err)
	}
	got := m.sent()
	if len(got) != 1 {
		t.Fatalf("收到 %d 次上报，期望 1", len(got))
	}
	if got[0].Static == nil {
		t.Fatal("首次上报必须带 static（Master 重启后靠它恢复静态信息）")
	}
	if got[0].AgentVersion != "test-1.0" {
		t.Errorf("AgentVersion = %q", got[0].AgentVersion)
	}
	if len(got[0].Samples) != 1 {
		t.Fatalf("首次上报样本数 = %d，期望 1", len(got[0].Samples))
	}
	if got[0].Samples[0].Metrics.CPU == nil || *got[0].Samples[0].Metrics.CPU != 1234 {
		t.Errorf("指标没有被带上：%+v", got[0].Samples[0].Metrics)
	}

	// 第二次：static 已成功送达，不应再带。
	if _, err := env.d.tick(ctx); err != nil {
		t.Fatalf("第二次上报失败: %v", err)
	}
	got = m.sent()
	if got[1].Static != nil {
		t.Error("static 已成功上报过，第二次不应再携带")
	}
}

// TestTickRequeuesFailedSamplesAndKeepsOrder 断言失败样本回到缓冲，下一次按 ts 升序补报。
//
// "按 ts 升序"是 13.4.2 的明确要求：Master 侧按 ts 去重/覆盖，
// 乱序补报会让较新的点被较旧的点覆盖掉（曲线出现回退）。
func TestTickRequeuesFailedSamplesAndKeepsOrder(t *testing.T) {
	m := newFakeMaster(t, "")
	m.reportErr = http.StatusInternalServerError
	env := newAgentEnv(t, m, defaultTestConfig())
	ctx := context.Background()

	if _, err := env.d.tick(ctx); err == nil {
		t.Fatal("上报 500 时应返回错误，让 Run 走退避")
	}
	env.d.mu.Lock()
	buffered := len(env.d.buffer)
	env.d.mu.Unlock()
	if buffered != 1 {
		t.Fatalf("失败后缓冲中有 %d 条，期望 1", buffered)
	}

	// 恢复后：缓冲 + 本轮样本一起发，且升序。
	m.reportErr = 0
	if _, err := env.d.tick(ctx); err != nil {
		t.Fatalf("恢复后上报失败: %v", err)
	}
	got := m.sent()
	last := got[len(got)-1]
	if len(last.Samples) != 2 {
		t.Fatalf("补报后样本数 = %d，期望 2（1 条缓冲 + 1 条本轮）", len(last.Samples))
	}
	if last.Samples[0].TS > last.Samples[1].TS {
		t.Errorf("补报未按 ts 升序：%d > %d", last.Samples[0].TS, last.Samples[1].TS)
	}
	env.d.mu.Lock()
	left := len(env.d.buffer)
	env.d.mu.Unlock()
	if left != 0 {
		t.Errorf("补报成功后缓冲应清空，仍有 %d 条", left)
	}
}

// TestTickCapsSamplesAt100AndKeepsTheRest 断言单次上报不超过 100 条（7.0），
// 多出来的仍然留在缓冲里等下一轮，而不是被丢掉。
func TestTickCapsSamplesAt100AndKeepsTheRest(t *testing.T) {
	m := newFakeMaster(t, "")
	cfg := defaultTestConfig()
	cfg.BufferSamples = 500
	env := newAgentEnv(t, m, cfg)

	// 造 120 条积压（ts 递增）。
	env.d.mu.Lock()
	for i := 1; i <= 120; i++ {
		env.d.buffer = append(env.d.buffer, model.Sample{TS: int64(i)})
	}
	env.d.mu.Unlock()

	now := time.Now().Unix()
	if _, err := env.d.tick(context.Background()); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	sent := m.sent()[0].Samples
	if len(sent) != 100 {
		t.Fatalf("单次上报样本数 = %d，期望上限 100", len(sent))
	}
	if sent[0].TS != 22 {
		t.Errorf("最先发出的应是第 22 条（前 21 条留下轮补报），得到 ts=%d", sent[0].TS)
	}
	if sent[len(sent)-1].TS != now {
		t.Errorf("最后一条应是本轮新样本（ts=%d），得到 %d", now, sent[len(sent)-1].TS)
	}
	env.d.mu.Lock()
	left := len(env.d.buffer)
	first := int64(0)
	if left > 0 {
		first = env.d.buffer[0].TS
	}
	env.d.mu.Unlock()
	if left != 21 || first != 1 {
		t.Errorf("剩余缓冲 = %d 条（首条 ts=%d），期望 21 条且首条 ts=1", left, first)
	}
}

// TestRequeueDropsOldestWhenBufferFull 断言缓冲满时丢最旧的（13.4.8）。
func TestRequeueDropsOldestWhenBufferFull(t *testing.T) {
	m := newFakeMaster(t, "")
	cfg := defaultTestConfig()
	cfg.BufferSamples = 3
	env := newAgentEnv(t, m, cfg)

	env.d.requeue([]model.Sample{{TS: 1}, {TS: 2}, {TS: 3}, {TS: 4}, {TS: 5}})

	env.d.mu.Lock()
	defer env.d.mu.Unlock()
	if len(env.d.buffer) != 3 {
		t.Fatalf("缓冲长度 = %d，期望夹到上限 3", len(env.d.buffer))
	}
	if env.d.buffer[0].TS != 3 || env.d.buffer[2].TS != 5 {
		t.Errorf("应丢最旧的：留下 %d..%d", env.d.buffer[0].TS, env.d.buffer[2].TS)
	}
}

// TestTickSurvivesCollectorFailure 断言采集失败不影响上报（12.5）。
//
// 采集失败时上报**空指标**而不是不上报：不上报会被 Master 判成离线，
// 于是"某台机器的采集器坏了"会表现为"节点掉线"，排查方向完全错。
func TestTickSurvivesCollectorFailure(t *testing.T) {
	m := newFakeMaster(t, "")
	env := newAgentEnv(t, m, defaultTestConfig())
	env.coll.err = io.ErrUnexpectedEOF

	if _, err := env.d.tick(context.Background()); err != nil {
		t.Fatalf("采集失败不应让上报也失败，得到 %v", err)
	}
	got := m.sent()
	if len(got) != 1 || len(got[0].Samples) != 1 {
		t.Fatalf("采集失败时仍应上报一条样本，得到 %+v", got)
	}
	if got[0].Samples[0].Metrics.CPU != nil {
		t.Error("采集失败时指标应为空，而不是带着上一轮的旧值")
	}
}

// ---------------------------------------------------------------------------
// 配置热更新
// ---------------------------------------------------------------------------

// TestTickPullsConfigOnlyWhenVersionChanges 断言只在 config_version 变化时拉配置（8.1 第 3 步）。
//
// 每次都拉会让 50 个节点每 3 秒打一次配置接口，而它绝大多数时候返回 304——
// 纯属浪费（设计文档明确要求"只有受影响的节点会重拉"）。
func TestTickPullsConfigOnlyWhenVersionChanges(t *testing.T) {
	m := newFakeMaster(t, "v1")
	m.remote = &model.AgentConfig{
		ConfigVersion: "v1", ReportIntervalS: 2, ProbeIntervalS: 30,
		Targets: []model.Target{{ID: 1, Host: "example.com", Port: 443, Method: "tcp"}},
	}
	cfg := defaultTestConfig()
	cfg.ConfigVersion = "v1"
	env := newAgentEnv(t, m, cfg)
	ctx := context.Background()

	// 版本一致 → 不拉。
	if _, err := env.d.tick(ctx); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	if hits := m.configHits(); hits != 0 {
		t.Fatalf("版本未变却拉了 %d 次配置", hits)
	}

	// 版本变化 → 拉一次。
	m.version = "v2"
	m.remote = &model.AgentConfig{
		ConfigVersion: "v2", ReportIntervalS: 5, ProbeIntervalS: 45,
		NICExclude: []string{"lo"},
		Targets: []model.Target{
			{ID: 7, Host: "1.1.1.1", Port: 443, Method: "tcp"},
			{ID: 8, Host: "8.8.8.8", Method: "icmp"},
		},
	}
	if _, err := env.d.tick(ctx); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	if hits := m.configHits(); hits != 1 {
		t.Fatalf("版本变化后应拉 1 次配置，实际 %d 次", hits)
	}
	env.d.mu.Lock()
	applied := *env.d.cfg
	env.d.mu.Unlock()
	if applied.ConfigVersion != "v2" || len(applied.Targets) != 2 {
		t.Errorf("远端配置未生效：%+v", applied)
	}
	if applied.ProbeIntervalS != 45 || applied.ReportIntervalS != 5 {
		t.Errorf("间隔未随配置更新：probe=%d report=%d",
			applied.ProbeIntervalS, applied.ReportIntervalS)
	}

	// 同一版本再来一次 → 不重复拉。
	if _, err := env.d.tick(ctx); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	if hits := m.configHits(); hits != 1 {
		t.Errorf("版本未再变化却又拉了配置：共 %d 次", hits)
	}
}

// TestSyncedConfigIsPersistedAndProberReplaced 断言热更新的三个副作用：
// 配置落盘（重启后仍生效）、探测器被整体替换（AllowPrivateTargets 跟着换）、
// 采集器被整体替换（网卡/磁盘过滤项跟着换）。
func TestSyncedConfigIsPersistedAndProberReplaced(t *testing.T) {
	m := newFakeMaster(t, "v2")
	m.remote = &model.AgentConfig{
		ConfigVersion: "v2", ReportIntervalS: 2, ProbeIntervalS: 30,
		NICExclude: []string{"lo", "docker*"},
		Targets:    []model.Target{{ID: 9, Host: "example.com", Port: 443, Method: "tcp"}},
	}
	cfg := defaultTestConfig()
	cfg.ConfigVersion = "v1"
	env := newAgentEnv(t, m, cfg)

	env.d.mu.Lock()
	oldProber, oldColl := env.d.probe, env.d.coll
	env.d.mu.Unlock()

	if _, err := env.d.tick(context.Background()); err != nil {
		t.Fatalf("上报失败: %v", err)
	}

	env.d.mu.Lock()
	proberReplaced := env.d.probe != oldProber
	collReplaced := env.d.coll != oldColl
	applied := *env.d.cfg
	env.d.mu.Unlock()

	if !proberReplaced {
		t.Error("拉取新配置后探测器实例必须重建（否则 AllowPrivateTargets 还是旧的拨号策略）")
	}
	// 采集器的网卡/磁盘过滤项只在 collector.New 时被读取：不重建就等于
	// "后台改了网卡过滤，Agent 一直按老规则统计"，而且没有任何提示。
	if !collReplaced {
		t.Error("拉取新配置后采集器实例必须重建（否则网卡过滤项的改动永远不生效）")
	}
	if len(applied.NICExclude) != 2 {
		t.Errorf("远端下发的网卡排除项未生效：%v", applied.NICExclude)
	}

	// 落盘：进程重启后 agentconfig.Load 能读回同一份配置。
	onDisk, err := agentconfig.Load(env.d.cfgPath)
	if err != nil {
		t.Fatalf("读回落盘配置失败: %v", err)
	}
	if onDisk.ConfigVersion != "v2" || len(onDisk.Targets) != 1 || onDisk.Targets[0].ID != 9 {
		t.Errorf("落盘的配置不对：%+v", onDisk)
	}
	if len(onDisk.NICExclude) != 2 {
		t.Errorf("网卡过滤项未落盘：%v", onDisk.NICExclude)
	}
}

// TestConfigFetchFailureDoesNotFailTick 断言拉配置失败不阻断上报。
//
// 配置接口挂了不该让"节点在线"这个更重要的信号也一起丢——那会把一个
// 配置服务的问题放大成全网节点离线。
func TestConfigFetchFailureDoesNotFailTick(t *testing.T) {
	m := newFakeMaster(t, "v2")
	m.cfgErr = http.StatusInternalServerError
	cfg := defaultTestConfig()
	cfg.ConfigVersion = "v1"
	env := newAgentEnv(t, m, cfg)

	if _, err := env.d.tick(context.Background()); err != nil {
		t.Fatalf("拉配置失败不应让 tick 返回错误，得到 %v", err)
	}
	if hits := m.configHits(); hits != 1 {
		t.Errorf("应尝试拉 1 次配置，实际 %d 次", hits)
	}
	env.d.mu.Lock()
	ver := env.d.cfg.ConfigVersion
	env.d.mu.Unlock()
	if ver != "v1" {
		t.Errorf("拉取失败时不能改动本地版本号，得到 %q", ver)
	}
}

// ---------------------------------------------------------------------------
// 探测循环
// ---------------------------------------------------------------------------

// TestProbeLoopReReadsTargetsEachRound 断言目标清单与间隔每轮重取。
//
// 旧实现只在启动时建 ticker、只在启动时读目标清单，于是后台改完必须重启 Agent
// 才生效——用户看到的是"后台设置没反应"。这条测试直接钉住那个行为。
func TestProbeLoopReReadsTargetsEachRound(t *testing.T) {
	ln := listenLocal(t)
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	m := newFakeMaster(t, "")
	cfg := defaultTestConfig()
	cfg.ProbeIntervalS = 1 // 一轮 1 秒，测试才跑得动
	cfg.AllowPrivateTargets = true
	env := newAgentEnv(t, m, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go env.d.probeLoop(ctx)

	// 还没配目标 → 不探测，也不上报空延迟。
	time.Sleep(1300 * time.Millisecond)
	if got := env.d.takeLatency(); got != nil {
		t.Fatalf("没有目标时不该产生探测结果，得到 %+v", got)
	}

	// 运行中加目标 → 下一轮就必须开始探测（不等重启）。
	env.d.mu.Lock()
	env.d.cfg.Targets = []model.Target{
		{ID: 11, Host: "127.0.0.1", Port: port, Method: "tcp", Count: 1, TimeoutMs: 500},
	}
	env.d.mu.Unlock()

	got := waitLatency(t, env.d, 5*time.Second)
	if len(got) != 1 || got[0].TargetID != 11 {
		t.Fatalf("探测结果 = %+v，期望恰好目标 11 的一条", got)
	}
	if got[0].RTTAvg == nil {
		t.Errorf("本机 listener 应探通，得到 %+v", got[0])
	}

	// takeLatency 必须取走并清空：否则同一轮结果会被反复上报。
	if again := env.d.takeLatency(); again != nil {
		t.Errorf("takeLatency 未清空缓冲，第二次又取到 %+v", again)
	}
}

// TestReloadSwapsConfigAndAppliesDefaults 断言 Reload 重新读盘并套用默认值。
//
// 默认值里的关键一条是探测间隔不低于 10 秒（13.4.6）：手写配置写成 5 秒
// 必须被抬到 30 秒，否则节点会退化成"扫描器"。
func TestReloadSwapsConfigAndAppliesDefaults(t *testing.T) {
	m := newFakeMaster(t, "")
	cfg := defaultTestConfig()
	env := newAgentEnv(t, m, cfg)

	yaml := "" +
		"master_url: " + m.srv.URL + "\n" +
		"token: pk_test\n" +
		"probe_interval_s: 5\n" +
		"report_interval_s: 2\n" +
		"allow_private_targets: true\n" +
		"targets:\n" +
		"  - id: 3\n" +
		"    host: example.com\n" +
		"    port: 443\n" +
		"    method: tcp\n"
	if err := os.WriteFile(env.d.cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}

	env.d.mu.Lock()
	oldProber := env.d.probe
	env.d.mu.Unlock()

	if err := env.d.Reload(); err != nil {
		t.Fatalf("Reload 失败: %v", err)
	}

	env.d.mu.Lock()
	got, newProber := *env.d.cfg, env.d.probe
	env.d.mu.Unlock()

	if len(got.Targets) != 1 || got.Targets[0].ID != 3 {
		t.Errorf("目标清单未更新：%+v", got.Targets)
	}
	if got.Token != "pk_test" {
		t.Errorf("令牌未更新：%q", got.Token)
	}
	if got.ProbeIntervalS != 30 {
		t.Errorf("探测间隔 = %d，期望被默认值抬到 30（不低于 10 秒）", got.ProbeIntervalS)
	}
	if newProber == oldProber {
		t.Error("Reload 后探测器实例必须重建（拨号策略随 allow_private_targets 变）")
	}
}

// TestRunExitsOnContextCancel 断言主循环可被取消（不会漏 goroutine）。
func TestRunExitsOnContextCancel(t *testing.T) {
	m := newFakeMaster(t, "")
	env := newAgentEnv(t, m, defaultTestConfig())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- env.d.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("取消后 Run 应返回 nil，得到 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 已取消但 Run 没有退出")
	}
}

// ---------------------------------------------------------------------------
// 上报前的清洗（13.4.4）
// ---------------------------------------------------------------------------

// TestSanitizeStaticTrimsAndStrips 断言静态信息在 Agent 侧就被裁剪与去噪。
//
// 两端都做过清洗是有意的：Agent 侧是"第一道"（避免把非法字符送上网），
// Master 侧是"最后一道"（不信任任何上报）。任何一侧被删掉，另一侧就得独自兜住。
func TestSanitizeStaticTrimsAndStrips(t *testing.T) {
	s := &model.Static{
		Hostname:  strings.Repeat("h", 80),
		OS:        "Debian <script>12\n坏字符",
		Kernel:    strings.Repeat("k", 80),
		CPUModel:  strings.Repeat("c", 80),
		Arch:      strings.Repeat("a", 20),
		CPUCores:  -1,
		MemTotal:  -1,
		SwapTotal: -1,
		DiskTotal: -1,
	}
	sanitizeStatic(s)

	if len(s.Hostname) != 63 {
		t.Errorf("主机名应被截到 63，得到 %d", len(s.Hostname))
	}
	if len(s.Arch) != 16 {
		t.Errorf("架构应被截到 16，得到 %d", len(s.Arch))
	}
	if len(s.Kernel) != 64 || len(s.CPUModel) != 64 {
		t.Errorf("内核/CPU 型号应被截到 64，得到 %d/%d", len(s.Kernel), len(s.CPUModel))
	}
	for _, bad := range []string{"<", ">", "\n"} {
		if strings.Contains(s.OS, bad) {
			t.Errorf("OS 里仍含 %q：%q", bad, s.OS)
		}
	}
	if !strings.Contains(s.OS, "Debian") || !strings.Contains(s.OS, "12") {
		t.Errorf("正常内容不该被误删：%q", s.OS)
	}
	if s.CPUCores != 0 || s.MemTotal != 0 || s.SwapTotal != 0 || s.DiskTotal != 0 {
		t.Errorf("负数规格应被夹到 0，得到 %d/%d/%d/%d",
			s.CPUCores, s.MemTotal, s.SwapTotal, s.DiskTotal)
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起 listener 失败: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln
}

// waitLatency 轮询等待探测结果出现。
func waitLatency(t *testing.T, d *Daemon, budget time.Duration) []model.LatencySample {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if got := d.takeLatency(); got != nil {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("超时仍未拿到探测结果")
	return nil
}
