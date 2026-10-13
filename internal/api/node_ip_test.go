package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/koyoca/xb-probe/internal/auth"
	"github.com/koyoca/xb-probe/internal/pipeline"
	"github.com/koyoca/xb-probe/internal/store"
)

// 节点地址回填的回归：**"来源地址"不等于"节点的公网地址"**。
//
// 用户反馈的原话是「节点的 IP 地址始终是 127.0.0.1，无法自助探测获取真正的公网 IP」。
// 根因是老实现无条件把 `RealIP(r)` 写进 nodes.ip_v4，而且只在"列为空"时才写：
// Master 与 Agent 同机部署时来源地址就是 127.0.0.1，写进去之后就再也改不掉。
//
// 这条链路必须端到端验（真的发一次 /api/agent/v1/report），而不是只单测那个小函数：
// 中间任何一环断掉 —— 忘了过滤、忘了传 req.PublicIP、写错了列 —— 用户看到的
// 都是"地址还是错的"，而且没有任何报错。
func TestAgentReportNeverStoresLoopbackAsNodeAddress(t *testing.T) {
	env := newReportEnv(t)

	// 1) 来源是回环（Master 与 Agent 同机）→ 一个字节都不许写。
	//    这是本条 bug 的核心场景，也是"什么都不写"这个决定的兑现：
	//    "暂时不知道它的公网地址"与"它的公网地址是 127.0.0.1"是两件事。
	env.report(t, "127.0.0.1:5000", "")
	if got := env.ipv4(t); got != "" {
		t.Fatalf("来源为回环地址时写入了节点地址：%q（应保持为空）", got)
	}

	// 2) 来源是私网 → 同样不写。
	env.report(t, "192.168.31.20:5000", "")
	if got := env.ipv4(t); got != "" {
		t.Fatalf("来源为私网地址时写入了节点地址：%q", got)
	}

	// 3) 来源是公网（Master 与 Agent 不同机）→ 写进去。默认路径必须仍然能用，
	//    否则"修好同机场景"就变成了"所有节点都没地址了"。
	env.report(t, "203.0.113.9:5000", "")
	if got := env.ipv4(t); got != "203.0.113.9" {
		t.Fatalf("来源为公网地址时未写入节点地址：%q", got)
	}

	// 4) Agent 自报的公网地址优先于来源地址。
	//    场景：节点在 NAT 后面 + Master 前面有反代，来源是反代地址而节点自己知道真身。
	env.report(t, "198.51.100.1:5000", "198.51.100.7")
	if got := env.ipv4(t); got != "198.51.100.7" {
		t.Fatalf("Agent 自报的公网地址没有覆盖来源地址：%q", got)
	}

	// 5) Agent 自报了一个**非公网**地址（探测服务经代理返回 127.0.0.1）→
	//    不能写，而且不能因此把来源地址那一步也一起跳过。
	env.report(t, "203.0.113.11:5000", "127.0.0.1")
	if got := env.ipv4(t); got != "203.0.113.11" {
		t.Fatalf("自报值非公网时应回落到来源地址，得到 %q", got)
	}
}

// TestAgentReportRespectsManualNodeAddress 断言管理员手工填过的地址不会被自动探测改写。
//
// 这是 ip_auto 的全部意义：面板上写着"填写即覆盖"，那么之后就一次都不能被覆盖回去 ——
// 否则用户会看到地址栏自己在变，而且找不到规律（其实是每次上报都在改）。
//
// 两个方向都要钉：锁住之后**不许**被改；清空之后**必须**能恢复自动。
// 只钉一半的话，"永远锁死"这种实现也能全绿。
func TestAgentReportRespectsManualNodeAddress(t *testing.T) {
	env := newReportEnv(t)

	// 管理员手工填一个地址（与库里不同 ⇒ 锁定 ip_auto=0）。
	if err := env.repo.UpdateNodeAssets(env.id, "198.51.100.77", "", "", "", "", ""); err != nil {
		t.Fatalf("手工填地址失败：%v", err)
	}
	if n, _ := env.repo.GetNode(env.id); n == nil || n.IPAuto {
		t.Fatalf("手工填写后 ip_auto 应为 false，得到 %+v", n)
	}

	env.report(t, "203.0.113.9:5000", "")
	if got := env.ipv4(t); got != "198.51.100.77" {
		t.Fatalf("手工指定的地址被自动探测改写了：%q", got)
	}

	// 清空两栏 ⇒ 恢复自动探测（这是给用户的"退路"）。
	if err := env.repo.UpdateNodeAssets(env.id, "", "", "", "", "", ""); err != nil {
		t.Fatalf("清空地址失败：%v", err)
	}
	env.report(t, "203.0.113.9:5000", "")
	if got := env.ipv4(t); got != "203.0.113.9" {
		t.Fatalf("恢复自动探测后地址未被回填：%q", got)
	}
}

// ---------------------------------------------------------------------------
// 脚手架
// ---------------------------------------------------------------------------

// reportEnv 是一套"能真的发一次上报"的环境。
//
// newTOTPTestHandler 只装了管理端需要的依赖；上报路径还要令牌管理器与两个聚合器，
// 缺任何一样都会 panic 或静默不落库（后者更糟：测试会绿，因为什么都没发生）。
type reportEnv struct {
	h     *Handler
	repo  *store.Repo
	id    int64
	token string
}

func newReportEnv(t *testing.T) *reportEnv {
	t.Helper()
	h, repo := newTOTPTestHandler(t)
	h.Tokens = auth.NewTokenManager(repo)
	h.Multi = pipeline.NewMultiResolution(30, func(string, int64, pipeline.MetricsRow) {})
	h.Latency = pipeline.NewLatencyMultiResolution(30, func(string, int64, pipeline.LatencyRow) {})

	id, err := repo.CreateNode("pub-ip", "ipnode", "", time.Now().Unix())
	if err != nil {
		t.Fatalf("建节点失败：%v", err)
	}
	plain, hash, hint, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("生成令牌失败：%v", err)
	}
	if _, err := repo.CreateToken(id, hash, hint, time.Now().Unix()); err != nil {
		t.Fatalf("写入令牌失败：%v", err)
	}
	return &reportEnv{h: h, repo: repo, id: id, token: plain}
}

// report 发一次真实的上报请求，断言 200。
func (e *reportEnv) report(t *testing.T, remoteAddr, publicIP string) {
	t.Helper()
	body := map[string]any{
		"agent_version": "test-1.0",
		"samples": []map[string]any{
			{"ts": time.Now().Unix(), "metrics": map[string]any{}},
		},
	}
	if publicIP != "" {
		body["public_ip"] = publicIP
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/agent/v1/report", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.token)
	// 不走 RealIPMiddleware：RealIP 在没有上下文值时回落到 RemoteAddr，
	// 正好用来构造"来源地址是什么"这一前提。
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	e.h.handleReport(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("上报应返回 200，得到 %d：%s", w.Code, w.Body.String())
	}
}

// ipv4 读回库里节点的地址栏。
func (e *reportEnv) ipv4(t *testing.T) string {
	t.Helper()
	n, err := e.repo.GetNode(e.id)
	if err != nil || n == nil {
		t.Fatalf("读节点失败：%v", err)
	}
	return n.IPv4
}
