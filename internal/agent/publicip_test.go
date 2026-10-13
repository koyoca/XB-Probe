package agent

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// 公网地址自助探测的护栏。
//
// 这组测试全部通过 httpGetPublicIP 这个接缝跑：真实的探测要连第三方服务，
// 那样的测试在离线环境里要么永远红、要么被跳过——而被跳过的护栏等于没有。
//
// 替身"已生效"由断言本身证明：这些用例断言的是**替身给的值**（203.0.113.9），
// 若接缝没接上（还是真的去打网络），拿到的一定是空串，用例必红。

// stubPublicIP 把探测函数换成替身，返回被调用次数的指针。
func stubPublicIP(t *testing.T, ip string, err error) *int {
	t.Helper()
	calls := 0
	orig := httpGetPublicIP
	httpGetPublicIP = func(context.Context, string) (string, error) {
		calls++
		return ip, err
	}
	t.Cleanup(func() { httpGetPublicIP = orig })
	return &calls
}

// TestRefreshPublicIPStoresValueAndKeepsItOnFailure 钉住两件事：
// 探测成功要写进运行态；探测**失败不能把已知地址清掉**。
//
// 后一条不是洁癖：公网地址查询服务抽风是常态，清掉的话节点地址栏会周期性地变空，
// 看起来像"节点掉线了"。地址真的变了，下一次成功探测自然会覆盖。
func TestRefreshPublicIPStoresValueAndKeepsItOnFailure(t *testing.T) {
	m := newFakeMaster(t, "")
	cfg := defaultTestConfig()
	cfg.PublicIPURL = "https://ip.example.invalid/"
	env := newAgentEnv(t, m, cfg)
	ctx := context.Background()

	calls := stubPublicIP(t, "203.0.113.9", nil)
	if got := env.d.refreshPublicIP(ctx, cfg.PublicIPURL); got != "203.0.113.9" {
		t.Fatalf("探测返回值 = %q，期望 203.0.113.9（若为空说明接缝没生效、真的去打了网络）", got)
	}
	if *calls != 1 {
		t.Fatalf("替身被调用 %d 次，期望 1", *calls)
	}
	if got := env.d.observedPublicIP(); got != "203.0.113.9" {
		t.Fatalf("探测结果未写入运行态：%q", got)
	}

	// 服务不可达 → 保留上一次的地址。
	stubPublicIP(t, "", errors.New("connection refused"))
	if got := env.d.refreshPublicIP(ctx, cfg.PublicIPURL); got != "" {
		t.Errorf("失败时应返回空串，得到 %q", got)
	}
	if got := env.d.observedPublicIP(); got != "203.0.113.9" {
		t.Errorf("单次探测失败把已知的公网地址清掉了：%q", got)
	}

	// 拿到了响应但里面没有公网地址（例如服务经代理返回了 127.0.0.1）→ 同样不当成新值。
	// netutil 那一侧负责拒绝，这里断言 Agent 不会把空当有效值写进去。
	calls2 := stubPublicIP(t, "", nil)
	if got := env.d.refreshPublicIP(ctx, cfg.PublicIPURL); got != "" {
		t.Errorf("响应里没有公网地址时应返回空串，得到 %q", got)
	}
	if *calls2 != 1 {
		t.Errorf("替身被调用 %d 次，期望 1", *calls2)
	}
	if got := env.d.observedPublicIP(); got != "203.0.113.9" {
		t.Errorf("取不到地址时把已知地址清掉了：%q", got)
	}
}

// TestTickReportsProbedPublicIP 钉住"探测结果真的进了上报体"。
//
// 这是整条链路上最容易断的一环：探测写得再好，只要 tick 里没把 req.PublicIP 填上，
// 用户看到的就还是老样子（地址栏不变），而且没有任何报错。两个包的状态分别都对，
// 唯独中间没接上。
func TestTickReportsProbedPublicIP(t *testing.T) {
	m := newFakeMaster(t, "")
	cfg := defaultTestConfig()
	cfg.PublicIPURL = "https://ip.example.invalid/"
	env := newAgentEnv(t, m, cfg)

	stubPublicIP(t, "203.0.113.9", nil)
	if got := env.d.refreshPublicIP(context.Background(), cfg.PublicIPURL); got != "203.0.113.9" {
		t.Fatalf("探测失败：%q", got)
	}

	if _, err := env.d.tick(context.Background()); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	got := m.sent()
	if len(got) != 1 {
		t.Fatalf("收到 %d 次上报，期望 1", len(got))
	}
	if got[0].PublicIP != "203.0.113.9" {
		t.Errorf("上报体的 public_ip = %q，期望 203.0.113.9（探测结果没被带上）", got[0].PublicIP)
	}
}

// TestTickOmitsPublicIPWhenNotProbed 断言未配置探测时上报体里不出现这个字段。
//
// 默认关闭是设计的一部分（打开它意味着每台节点定期访问第三方）。
// 这里断言的是"未探测 ⇒ 空串"，而不是"字段不存在"——omitempty 由序列化负责，
// 而**误报一个地址**（例如把上次的值或本机地址带上）才是要防的事。
func TestTickOmitsPublicIPWhenNotProbed(t *testing.T) {
	m := newFakeMaster(t, "")
	env := newAgentEnv(t, m, defaultTestConfig())

	if _, err := env.d.tick(context.Background()); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	if got := m.sent()[0].PublicIP; got != "" {
		t.Errorf("未配置探测却上报了 public_ip=%q", got)
	}
}

// TestPublicIPLoopFetchesImmediatelyAndSkipsWhenUnconfigured 钉住 loop 的三个边界。
//
//   - 配了地址：**第一轮就取**，不能先 sleep 一整个间隔——否则刚装好的节点要等
//     15 分钟地址栏才变，用户以为没生效。
//   - 没配地址：一次网络都不碰（默认关闭就要真的是关闭）。
//   - **取消后必须真的退出**：`cancel(); <-done` 这两行是有意写成会挂住的形状。
//     早先的实现在循环里用 sleepCtx 当等待，而 sleepCtx 在 ctx 已取消时立刻返回，
//     于是循环变成忙等、永不退出 —— 这条用例当时直接挂死（整包测试 6 分钟不返回），
//     正是它把那个 bug 抓出来的。
func TestPublicIPLoopFetchesImmediatelyAndSkipsWhenUnconfigured(t *testing.T) {
	m := newFakeMaster(t, "")

	// 1) 未配置：不碰网络，且取消后能退出。
	cfg := defaultTestConfig()
	env := newAgentEnv(t, m, cfg)
	calls := stubPublicIP(t, "203.0.113.9", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); env.d.publicIPLoop(ctx) }()
	time.Sleep(200 * time.Millisecond) // 远小于 publicIPIdleWait(60s)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 已取消但 publicIPLoop 没有退出（忙等 / 漏 goroutine）")
	}
	if *calls != 0 {
		t.Errorf("没配 public_ip_url 却发起了 %d 次请求", *calls)
	}
	if got := env.d.observedPublicIP(); got != "" {
		t.Errorf("没配探测却拿到了地址 %q", got)
	}

	// 2) 配了：第一轮立刻取到，且取消后能退出。
	cfg2 := defaultTestConfig()
	cfg2.PublicIPURL = "https://ip.example.invalid/"
	cfg2.PublicIPIntervalS = 60
	env2 := newAgentEnv(t, m, cfg2)
	stubPublicIP(t, "203.0.113.9", nil)
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { defer close(done2); env2.d.publicIPLoop(ctx2) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && env2.d.observedPublicIP() == "" {
		time.Sleep(10 * time.Millisecond)
	}
	cancel2()
	select {
	case <-done2:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 已取消但 publicIPLoop 没有退出")
	}
	if got := env2.d.observedPublicIP(); got != "203.0.113.9" {
		t.Fatalf("配了 public_ip_url 却没在第一轮取到地址：%q", got)
	}
}

// TestReloadDropsStaleProbedIPWhenURLChanges 断言换了探测服务就丢掉旧值。
//
// 留着旧值的症状很隐蔽：改了 agent.yaml 里的 public_ip_url 并重启，地址栏里
// 却还显示上一个服务给的地址，而日志里什么都没有。地址本身可能还是对的，
// 于是没人会怀疑"配置没生效"。
func TestReloadDropsStaleProbedIPWhenURLChanges(t *testing.T) {
	m := newFakeMaster(t, "")
	cfg := defaultTestConfig()
	cfg.PublicIPURL = "https://old.example.invalid/"
	env := newAgentEnv(t, m, cfg)

	stubPublicIP(t, "203.0.113.9", nil)
	if got := env.d.refreshPublicIP(context.Background(), cfg.PublicIPURL); got != "203.0.113.9" {
		t.Fatalf("探测失败：%q", got)
	}

	// 写一份新的本地配置（换了 URL）后 Reload。
	yaml := "" +
		"master_url: " + m.srv.URL + "\n" +
		"token: pk_test\n" +
		"public_ip_url: https://new.example.invalid/\n" +
		"targets: []\n"
	if err := os.WriteFile(env.d.cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	if err := env.d.Reload(); err != nil {
		t.Fatalf("Reload 失败: %v", err)
	}
	if got := env.d.observedPublicIP(); got != "" {
		t.Errorf("换掉 public_ip_url 后仍保留着旧服务给的地址：%q", got)
	}
}
