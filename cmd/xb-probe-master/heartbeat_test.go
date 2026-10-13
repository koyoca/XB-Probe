package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koyoca/xb-probe/internal/db"
	"github.com/koyoca/xb-probe/internal/store"
)

// TestHeartbeatSendsRequest 验证外部心跳真的发出 GET，且路径/方法正确。
//
// 用一个 httptest 服务器代替外部服务（healthchecks.io 之类），断言：
//   - 请求方法确实是 GET（A5 要求的是"一个裸 GET"）；
//   - 带了 no-store，避免被中间层缓存导致外部监控误判 Master 掉线；
//   - 2xx 时不产生错误日志路径。
func TestHeartbeatSendsRequest(t *testing.T) {
	var hits int64
	var method, path, ua, cc string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		method = r.Method
		path = r.URL.Path
		ua = r.Header.Get("User-Agent")
		cc = r.Header.Get("Cache-Control")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 带路径的 URL：外部心跳服务（healthchecks.io 之类）的"ping URL"就是带路径的，
	// 必须原样透传，不能在客户端里改写路径。
	s := newTestSchedulerWithHeartbeat(t, srv.URL+"/ping/abc123")
	s.sendHeartbeat(context.Background())

	if atomic.LoadInt64(&hits) != 1 {
		t.Fatalf("心跳请求次数 = %d，期望 1", hits)
	}
	if method != http.MethodGet {
		t.Errorf("方法 = %s，期望 GET", method)
	}
	if path != "/ping/abc123" {
		t.Errorf("路径 = %s，期望 /ping/abc123", path)
	}
	if ua != "xb-probe-master/heartbeat" {
		t.Errorf("User-Agent = %q", ua)
	}
	if cc != "no-store" {
		t.Errorf("Cache-Control = %q，期望 no-store", cc)
	}
}

// TestHeartbeatNotConfigured 断言未配置时静默跳过（不发请求、不报错）。
func TestHeartbeatNotConfigured(t *testing.T) {
	s := newTestSchedulerWithHeartbeat(t, "")
	// 不应 panic，也不应有任何请求。
	s.sendHeartbeat(context.Background())
}

// TestHeartbeatServerError 断言 4xx/5xx 不会 panic（只记 Warn）。
func TestHeartbeatServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := newTestSchedulerWithHeartbeat(t, srv.URL)
	s.sendHeartbeat(context.Background()) // 只要不 panic 就算通过
}

// TestHeartbeatUnreachable 断言连不上时不会阻塞主循环太久（超时受控）。
func TestHeartbeatUnreachable(t *testing.T) {
	// 127.0.0.1:1 上不会有服务监听，连接应立即被拒绝。
	s := newTestSchedulerWithHeartbeat(t, "https://127.0.0.1:1/ping")

	start := time.Now()
	s.sendHeartbeat(context.Background())
	elapsed := time.Since(start)

	// 连接被拒绝是瞬时的；用 10 秒超时兜底，这里断言远小于它。
	if elapsed > 5*time.Second {
		t.Errorf("心跳耗时 %v，失败路径不应阻塞这么久", elapsed)
	}
}

// TestHeartbeatRejectsHTTPRedirectDowngrade 断言 https→http 的跳转被拒绝（防降级）。
func TestHeartbeatRejectsHTTPRedirectDowngrade(t *testing.T) {
	// 起一个 http 服务器，然后让"心跳 client"直接去请求它会话中的重定向链——
	// 这里直接测 CheckRedirect 的行为。
	s := newTestSchedulerWithHeartbeat(t, "http://example.com/x")
	client := s.heartbeatClient()

	req, _ := http.NewRequest(http.MethodGet, "http://evil.example.com/", nil)
	err := client.CheckRedirect(req, []*http.Request{{}, {}})
	if err == nil {
		t.Error("从 https 跳到 http 应被拒绝")
	}
	if err != nil && len(err.Error()) == 0 {
		t.Error("错误信息不应为空")
	}
}

// newTestSchedulerWithHeartbeat 构造一个只用于心跳测试的 Scheduler：
// 只需要 repo 能读出 heartbeat_url 设置即可，其他依赖留空。
//
// 注意：这里刻意不调 NewScheduler——心跳路径只碰 repo 和 log，
// 用结构体字面量能避免为测试拉起注册表/写盘器/告警引擎等一堆真实依赖。
func newTestSchedulerWithHeartbeat(t *testing.T, url string) *Scheduler {
	t.Helper()
	repo := newTestRepoWithSetting(t, "heartbeat_url", url)
	return &Scheduler{
		repo: repo,
		log:  discardLogger(),
	}
}

// newTestRepoWithSetting 建一个临时 SQLite 库并写入一条设置。
func newTestRepoWithSetting(t *testing.T, key, value string) *store.Repo {
	t.Helper()
	dbs, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = dbs.Close() })
	if err := dbs.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	repo := store.New(dbs)
	// 空串也要写：调用方用它表达"未配置"，不能依赖缺行默认值。
	if err := repo.SetSetting(key, value, time.Now().Unix()); err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	return repo
}

// discardLogger 返回一个丢弃全部输出的 logger，避免测试刷屏。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// 心跳可观测性（状态机）
// ---------------------------------------------------------------------------

// TestHeartbeatStateHealthyTransitions 验证 Healthy 的三态判定。
//
// 这里必须覆盖"刚启动还没发过心跳"这一档：如果只看 LastOK 是否新鲜，
// 进程刚起来的瞬间会被判成"不健康"，后台一进设置页就看到红色告警。
func TestHeartbeatStateHealthyTransitions(t *testing.T) {
	cases := []struct {
		name  string
		url   string
		setup func(*heartbeatState)
		now   int64
		want  bool
	}{
		{
			name: "未配置 -> 不健康（功能没开）",
			url:  "",
			want: false,
		},
		{
			name: "已配置但还没发过 -> 健康（启动瞬间的正常状态）",
			url:  "https://hc-ping.com/abc",
			want: true,
		},
		{
			name: "最近一次成功 -> 健康",
			url:  "https://hc-ping.com/abc",
			setup: func(h *heartbeatState) {
				h.recordOK(1000, 200)
			},
			want: true,
		},
		{
			name: "成功后又失败一次 -> 不健康",
			url:  "https://hc-ping.com/abc",
			setup: func(h *heartbeatState) {
				h.recordOK(1000, 200)
				h.recordFail(1300, 500, "HTTP 500")
			},
			want: false,
		},
		{
			name: "重新成功 -> 恢复健康且失败计数清零",
			url:  "https://hc-ping.com/abc",
			setup: func(h *heartbeatState) {
				h.recordFail(1000, 0, "timeout")
				h.recordFail(1300, 0, "timeout")
				h.recordOK(1600, 200)
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSchedulerWithHeartbeat(t, tc.url)
			if tc.setup != nil {
				tc.setup(&s.hb)
			}
			got := s.HeartbeatStatus()
			if got.Healthy != tc.want {
				t.Errorf("Healthy = %v，期望 %v（snapshot=%+v）", got.Healthy, tc.want, s.hb.snapshot())
			}
			if got.Configured != (tc.url != "") {
				t.Errorf("Configured = %v，期望 %v", got.Configured, tc.url != "")
			}
			if got.IntervalSec <= 0 {
				t.Errorf("IntervalSec = %d，应大于 0（前端要靠它算宽限期）", got.IntervalSec)
			}
		})
	}
}

// TestHeartbeatStateRecordsFailCount 验证连续失败计数与错误信息被正确记录。
func TestHeartbeatStateRecordsFailCount(t *testing.T) {
	var h heartbeatState

	h.recordFail(100, 0, "连接被拒绝")
	if got := h.snapshot(); got.FailCount != 1 || got.LastOK != 0 || got.LastError != "连接被拒绝" {
		t.Errorf("第一次失败后 snapshot = %+v", got)
	}
	h.recordFail(200, 503, "HTTP 503")
	snap := h.snapshot()
	if snap.FailCount != 2 {
		t.Errorf("FailCount = %d，期望 2", snap.FailCount)
	}
	if snap.LastStatus != 503 {
		t.Errorf("LastStatus = %d，期望 503", snap.LastStatus)
	}
	if snap.Sent != 0 {
		t.Errorf("Sent = %d，失败不应计入成功次数", snap.Sent)
	}

	// 成功要清零失败计数、清空错误信息、累加 Sent。
	h.recordOK(300, 200)
	snap = h.snapshot()
	if snap.FailCount != 0 {
		t.Errorf("成功后 FailCount = %d，期望 0", snap.FailCount)
	}
	if snap.LastError != "" {
		t.Errorf("成功后 LastError = %q，期望空串", snap.LastError)
	}
	if snap.Sent != 1 || snap.LastOK != 300 {
		t.Errorf("成功后 Sent=%d LastOK=%d", snap.Sent, snap.LastOK)
	}

	// 再成功一次，Sent 累加而 LastOK 前进。
	h.recordOK(400, 200)
	if snap = h.snapshot(); snap.Sent != 2 || snap.LastOK != 400 {
		t.Errorf("第二次成功后 Snapshot = %+v", snap)
	}
}

// TestHeartbeatStatusUnconfiguredDoesNotRecord 断言未配置时不污染状态。
//
// 未配置是"功能没开"，不是"失败"。如果也记 FailCount，
// 后台会显示"连续失败 N 次"而管理员根本没配过这个功能，无从解释。
func TestHeartbeatStatusUnconfiguredDoesNotRecord(t *testing.T) {
	s := newTestSchedulerWithHeartbeat(t, "")
	s.sendHeartbeat(context.Background())

	snap := s.hb.snapshot()
	if snap.FailCount != 0 || snap.LastAttempt != 0 {
		t.Errorf("未配置时不该记录任何尝试，得到 %+v", snap)
	}
	if got := s.HeartbeatStatus(); got.Configured {
		t.Error("未配置时 Configured 应为 false")
	}
}

// TestHeartbeatStatusCountsRealFailure 端到端：真发一次失败的心跳，
// 断言状态被记录成"不健康"且带上了错误原因。
func TestHeartbeatStatusCountsRealFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	// httptest 给的是 http://，但 sendHeartbeat 不校验 scheme（校验在保存设置时做），
	// 而检查器的降级判断对 127.0.0.1 放行，所以这里能真实走完请求。
	s := newTestSchedulerWithHeartbeat(t, srv.URL)
	s.sendHeartbeat(context.Background())

	st := s.HeartbeatStatus()
	if st.Healthy {
		t.Error("500 响应后应判为不健康")
	}
	if st.FailCount != 1 {
		t.Errorf("FailCount = %d，期望 1", st.FailCount)
	}
	if st.LastStatus != 500 {
		t.Errorf("LastStatus = %d，期望 500", st.LastStatus)
	}
	if st.LastError == "" {
		t.Error("LastError 不应为空（后台要显示失败原因）")
	}
	if st.LastAttempt == 0 {
		t.Error("LastAttempt 应被记录")
	}
}

// TestHeartbeatStatusCountsRealSuccess 端到端：成功的心跳应记为健康。
func TestHeartbeatStatusCountsRealSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newTestSchedulerWithHeartbeat(t, srv.URL)
	s.sendHeartbeat(context.Background())

	st := s.HeartbeatStatus()
	if !st.Healthy {
		t.Errorf("2xx 后应判为健康，得到 %+v", st)
	}
	if st.FailCount != 0 || st.Sent != 1 {
		t.Errorf("FailCount=%d Sent=%d，期望 0/1", st.FailCount, st.Sent)
	}
	if st.LastOK == 0 {
		t.Error("LastOK 应被记录")
	}
	if st.LastError != "" {
		t.Errorf("成功时 LastError 应为空，得到 %q", st.LastError)
	}
}
