package prober

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koyoca/xb-probe/internal/model"
)

// 探测器的单元测试。三块内容：TCP 真实握手、ICMP 的权限与 S4 判定、错误枚举映射。
//
// 这里刻意**不做**"能不能 ping 通外网"这类依赖外网的断言：那种测试在 CI/受限网络
// 里会随机红，而它想证明的东西（"错误分类对不对"）完全可以用确定性输入证明。

// ---------------------------------------------------------------------------
// TCP：真实握手
// ---------------------------------------------------------------------------

// TestProbeTCPHandshakeSucceeds 对着本机一个真实 listener 跑一轮探测。
//
// 用 allowPrivate=true 才能探 127.0.0.1——这本身也说明 S4 的黑名单确实生效
// （见 TestProbeBlocksPrivateTargetsByDefault）。
func TestProbeTCPHandshakeSucceeds(t *testing.T) {
	ln := listenLoopback(t)
	defer ln.Close()

	s := New(true).Probe(context.Background(), model.Target{
		ID: 42, Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port,
		Method: "tcp", Count: 3, TimeoutMs: 500,
	})

	if s.Error != "" {
		t.Fatalf("探测本机 listener 应成功，得到 error=%q", s.Error)
	}
	if s.TargetID != 42 {
		t.Errorf("TargetID = %d，期望原样带回 42", s.TargetID)
	}
	if s.RTTAvg == nil || s.RTTMin == nil || s.RTTMax == nil {
		t.Fatalf("成功的一轮必须给出 avg/min/max，得到 %+v", s)
	}
	if *s.RTTAvg <= 0 {
		t.Errorf("RTT 应为正数，得到 %d", *s.RTTAvg)
	}
	if *s.RTTMin > *s.RTTAvg || *s.RTTAvg > *s.RTTMax {
		t.Errorf("min/avg/max 次序不对：%d/%d/%d", *s.RTTMin, *s.RTTAvg, *s.RTTMax)
	}
	if s.Loss != 0 {
		t.Errorf("三次全通时 Loss = %d，期望 0", s.Loss)
	}
}

// TestProbeTCPCountDefaultsAndClamps 用"listener 实际收到了几次握手"来验证次数规则。
//
// 次数是靠副作用观测的：每成功拨号一次就会有一个新连接。这比断言 Loss 更直接——
// 全部失败时 Loss 恒为 10000，看次数是看不出来的。
func TestProbeTCPCountDefaultsAndClamps(t *testing.T) {
	cases := []struct {
		name  string
		count int
		want  int64
	}{
		{"未填次数时默认 3 次", 0, 3},
		{"负数也回退到默认 3 次", -2, 3},
		{"超过上限时夹到 5 次", 7, 5},
		{"合法次数原样使用", 2, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var accepts int64
			port := listenCounting(t, &accepts)

			s := New(true).Probe(context.Background(), model.Target{
				ID: 1, Host: "127.0.0.1", Port: port, Method: "tcp",
				Count: c.count, TimeoutMs: 500,
			})
			if s.Error != "" {
				t.Fatalf("探测失败：%q", s.Error)
			}
			// Accept 与拨号完成之间存在极短的错位：握手在内核 backlog 里已经完成，
			// 但用户态的 Accept 还没返回。等一下再断言次数。
			waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&accepts) >= c.want })
			if got := atomic.LoadInt64(&accepts); got != c.want {
				t.Errorf("listener 收到 %d 次连接，期望 %d 次（MaxCountPerRound=%d）",
					got, c.want, MaxCountPerRound)
			}
		})
	}
}

// TestProbeTCPRefusedIsReportedAsRefused 断言端口关闭时是 refused 而不是 timeout/other。
//
// 这个区分对运维很重要：refused = 主机活着、端口上的服务没了；timeout = 链路不通。
// 两者指向完全不同的排查方向，混成一个枚举等于把线索丢掉。
func TestProbeTCPRefusedIsReportedAsRefused(t *testing.T) {
	port := closedLoopbackPort(t)

	s := New(true).Probe(context.Background(), model.Target{
		ID: 1, Host: "127.0.0.1", Port: port, Method: "tcp", Count: 1, TimeoutMs: 500,
	})

	if s.Error != model.ProbeErrRefused {
		t.Fatalf("error = %q，期望 %q", s.Error, model.ProbeErrRefused)
	}
	if s.RTTAvg != nil {
		t.Errorf("整轮失败时 RTTAvg 必须为 nil，得到 %v", *s.RTTAvg)
	}
	if s.Loss != 10000 {
		t.Errorf("整轮失败时 Loss = %d，期望 10000", s.Loss)
	}
}

// TestProbeTCPRejectsIllegalPort 断言端口越界时报 other 而不是去拨一个非法地址。
func TestProbeTCPRejectsIllegalPort(t *testing.T) {
	for _, port := range []int{0, -1, 65536, 100000} {
		s := New(true).Probe(context.Background(), model.Target{
			ID: 1, Host: "127.0.0.1", Port: port, Method: "tcp", Count: 1, TimeoutMs: 200,
		})
		if s.Error != model.ProbeErrOther {
			t.Errorf("端口 %d 的 error = %q，期望 %q", port, s.Error, model.ProbeErrOther)
		}
	}
}

// TestProbeCancelledContextIsNotReportedAsSuccess 断言 ctx 取消不会被当成"测到了"。
//
// 这是 shutdown 路径：探到一半进程要退。此时既没有 RTT 也不能报成功——
// 若把 RTTAvg 留成非 nil，一个正在关闭的 Agent 会上报一轮"0 丢包"的假数据。
func TestProbeCancelledContextIsNotReportedAsSuccess(t *testing.T) {
	ln := listenLoopback(t)
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 进循环前就已取消

	s := New(true).Probe(ctx, model.Target{
		ID: 1, Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port,
		Method: "tcp", Count: 3, TimeoutMs: 500,
	})

	if s.Error == "" {
		t.Fatal("ctx 已取消时必须有错误码，不能报成功")
	}
	if s.RTTAvg != nil {
		t.Errorf("ctx 已取消时 RTTAvg 必须为 nil，得到 %v", *s.RTTAvg)
	}
}

// ---------------------------------------------------------------------------
// S4：内网/回环目标
// ---------------------------------------------------------------------------

// TestProbeBlocksPrivateTargetsByDefault 断言默认配置下探不到内网与回环。
//
// 这是 S4 的核心：节点被攻陷后不能被当成扫描内网的跳板。
// 注意检查发生在**拨号前**（Probe 里的解析期检查 + dialer 的 Control 回调），
// 所以这里连 listener 都不用起。
func TestProbeBlocksPrivateTargetsByDefault(t *testing.T) {
	p := New(false) // 默认：不放行私网
	for _, host := range []string{
		"127.0.0.1",       // 回环
		"10.0.0.1",        // 私网 A
		"192.168.1.1",     // 私网 C
		"172.16.0.1",      // 私网 B
		"169.254.169.254", // 云元数据
		"0.0.0.0",         // 未指定
		"::1",             // IPv6 回环
		"fd00::1",         // IPv6 唯一本地
	} {
		s := p.Probe(context.Background(), model.Target{
			ID: 1, Host: host, Port: 80, Method: "tcp", Count: 1, TimeoutMs: 200,
		})
		if s.Error != model.ProbeErrBlockedTarget {
			t.Errorf("目标 %s 的 error = %q，期望 %q", host, s.Error, model.ProbeErrBlockedTarget)
		}
		if s.RTTAvg != nil {
			t.Errorf("目标 %s 被拒绝时不该有 RTT", host)
		}
		if s.Loss != 10000 {
			t.Errorf("目标 %s 被拒绝时 Loss = %d，期望 10000", host, s.Loss)
		}
	}
}

// TestProbeAllowsPrivateTargetsWhenConfigured 断言开关确实能放行（否则上面那条
// "被拒绝"可能是因为别的原因——比如所有目标都探不通）。
func TestProbeAllowsPrivateTargetsWhenConfigured(t *testing.T) {
	ln := listenLoopback(t)
	defer ln.Close()

	s := New(true).Probe(context.Background(), model.Target{
		ID: 1, Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port,
		Method: "tcp", Count: 1, TimeoutMs: 500,
	})
	if s.Error != "" {
		t.Fatalf("allow_private_targets=true 时本机回环应可探测，得到 error=%q", s.Error)
	}
}

// stubLookupIP 把解析换成确定性替身，返回还原函数。
//
// 必须用替身、不能用真 DNS：本机/企业网络里存在"任何名字都解析成功"的 DNS
// （沙箱流量拦截、Captive Portal、运营商劫持），实测 `no-such-host.invalid`
// 会被解析到 198.18.0.25 并且真的能连上——那样"域名拼错了必须报错"的断言
// 会因为"居然成功了"而变红，测的是网络而不是代码。
func stubLookupIP(t *testing.T, hosts map[string][]net.IP) func() {
	t.Helper()
	old := lookupIP
	lookupIP = func(host string) ([]net.IP, error) {
		ips, ok := hosts[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return ips, nil
	}
	return func() { lookupIP = old }
}

// TestProbeResolveFailureIsNotReportedAsBlockedTarget 断言"解析不了"与"被 S4 拒绝"
// 是两回事。
//
// 旧实现把 checkResolved 的两种失败合成一个 error，调用方一律记成 blocked_target：
// 域名写错的目标在界面上显示"目标被拒绝"，管理员会去查 S4 白名单，而真正的原因是拼错了。
// 更糟的是 probe_fail 告警刻意排除 blocked_target 与 icmp_not_permitted，
// 这种误判会让写错域名的目标**永远不告警**。
func TestProbeResolveFailureIsNotReportedAsBlockedTarget(t *testing.T) {
	// 解析结果由替身给出："no-such-host.invalid" 刻意不在表里 → 替身返回 not found。
	defer stubLookupIP(t, map[string][]net.IP{
		"127.0.0.1": {net.ParseIP("127.0.0.1")},
	})()

	// 先确认替身确实生效。少了这一句，注入没生效时这条用例会悄悄退回"依赖真实 DNS"，
	// 在本机（DNS 会解析一切）表现为红，在别的机器上表现为绿——两种都不是在验代码。
	if _, err := lookupIP("no-such-host.invalid"); err == nil {
		t.Fatal("解析替身没生效（仍在用真实 DNS）")
	}

	p := New(false)

	// 1) 直接看 checkResolved 的返回值：解析失败不能被当成"命中黑名单"。
	err := p.checkResolved("no-such-host.invalid")
	if errors.Is(err, errBlockedTarget) {
		t.Errorf("解析失败被报成了 S4 拦截：%v", err)
	}
	// 反向：真正命中黑名单时必须就是它。
	if err := p.checkResolved("127.0.0.1"); !errors.Is(err, errBlockedTarget) {
		t.Errorf("回环地址应判为 errBlockedTarget，得到 %v", err)
	}

	// 2) 端到端：Probe 给出的一定是"真的失败了"，而不是"被拒绝"。
	s := p.Probe(context.Background(), model.Target{
		ID: 1, Host: "no-such-host.invalid", Port: 80, Method: "tcp",
		Count: 1, TimeoutMs: 300,
	})
	if s.Error == model.ProbeErrBlockedTarget {
		t.Fatal("解析不了的域名被报成了 blocked_target")
	}
	if s.Error == "" {
		t.Fatal("解析不了的域名必须报错")
	}
}

// TestBlockedIPWhitelist 对黑名单本身做一张真值表。
//
// 单独测它是因为它被用在两处（解析期与拨号期），任何一处退化都会让"防 DNS 重绑定"
// 失效；而端到端用例只覆盖得到其中一处。
func TestBlockedIPWhitelist(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "10.1.2.3", "192.168.0.1", "172.31.255.255",
		"169.254.1.1", "169.254.169.254", "224.0.0.1", "ff02::1", "0.0.0.0", "::",
		"fe80::1", "fc00::1", "fd12:3456::1",
	}
	for _, s := range blocked {
		if ip := net.ParseIP(s); !isBlockedIP(ip) {
			t.Errorf("%s 应被拦截", s)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "223.5.5.5", "2001:4860:4860::8888", "fec0::1"}
	for _, s := range allowed {
		if ip := net.ParseIP(s); isBlockedIP(ip) {
			t.Errorf("%s 不该被拦截", s)
		}
	}
	// nil 也必须拦：调用方解析失败时可能把 nil 传进来。
	if !isBlockedIP(nil) {
		t.Error("nil 地址应被拦截")
	}
}

// TestDialerRejectsBlockedIP 断言拨号层自己也有检查（防 DNS 重绑定）。
//
// 解析期检查可以被"解析时返回公网 IP、拨号时又变成内网 IP"绕过，
// 所以 dialer 的 Control 回调是第二道闸门；它必须独立成立。
func TestDialerRejectsBlockedIP(t *testing.T) {
	d := New(false).dialer(time.Second)
	conn, err := d.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil {
		_ = conn.Close()
		t.Fatal("拨号层放行了回环地址：DNS 重绑定防护失效")
	}
	if !strings.Contains(err.Error(), "拒绝") && !strings.Contains(err.Error(), "受限") {
		t.Errorf("错误文案应说明是地址被拒绝，得到 %v", err)
	}
}

// ---------------------------------------------------------------------------
// ICMP
// ---------------------------------------------------------------------------

// TestProbeICMPEnforcesBlocklistBeforeTouchingSockets 断言 ICMP 路径也执行 S4 判定，
// 而且是在开 socket **之前**。
//
// 顺序很重要：先开 socket 再判断，等于每次被拒都白建一个 raw socket
// （受限环境里那一步本身就会失败，于是错误码会从 blocked_target 变成 icmp_not_permitted，
// 把"目标配错了"伪装成"权限不够"）。
func TestProbeICMPEnforcesBlocklistBeforeTouchingSockets(t *testing.T) {
	p := New(false)
	rtt, errText := p.probeICMP(context.Background(), "127.0.0.1", time.Second)
	if errText != model.ProbeErrBlockedTarget {
		t.Fatalf("error = %q，期望 %q", errText, model.ProbeErrBlockedTarget)
	}
	if rtt != 0 {
		t.Errorf("被拒绝时 rtt 应为 0，得到 %d", rtt)
	}
}

// TestProbeICMPNeverReportsAFalseFailure 断言 ICMP 的两种合法结局只有
// "测到了" 与 "没权限"。
//
// 权限不足（容器、低权限服务账号）必须是 icmp_not_permitted——它表示"没测成"。
// 若退化报成 timeout/unreachable，就会被当成"目标不通"进入告警，
// 于是所有无特权节点永远在报警。
func TestProbeICMPNeverReportsAFalseFailure(t *testing.T) {
	s := New(true).Probe(context.Background(), model.Target{
		ID: 1, Host: "127.0.0.1", Method: "icmp", Count: 1, TimeoutMs: 1000,
	})

	switch s.Error {
	case "": // 本机支持非特权 ICMP：应拿到 RTT
		if s.RTTAvg == nil {
			t.Errorf("报成功但没有 RTT：%+v", s)
		}
	case model.ProbeErrICMPNotPermitted:
		if s.RTTAvg != nil {
			t.Errorf("报权限不足却带着 RTT：%+v", s)
		}
	default:
		t.Fatalf("ICMP 回环探测给出了不该有的错误码 %q（只允许空或 %q）",
			s.Error, model.ProbeErrICMPNotPermitted)
	}
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

// TestClassifyErrMapsHostUnreachable 断言"路由不可达"归到 unreachable。
//
// 它曾经和 "no such host" 写在同一分支里归成 dns_failed：同一个网络故障
// 在 Linux 上（"connect: no route to host"）显示"域名解析失败"，
// 在 Windows 上（"attempted to an unreachable host"）显示"不可达"——平台不一致。
func TestClassifyErrMapsHostUnreachable(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{"dial tcp 10.0.0.1:80: connect: no route to host", model.ProbeErrUnreachable},
		{"dial tcp: A socket operation was attempted to an unreachable host", model.ProbeErrUnreachable},
		{"dial tcp: network is unreachable", model.ProbeErrUnreachable},
		{"dial tcp: network is down", model.ProbeErrUnreachable},
		{"lookup no-such-host.invalid: no such host", model.ProbeErrDNSFailed},
		{"dial tcp 1.2.3.4:80: connect: connection refused", model.ProbeErrRefused},
		{"dial tcp 1.2.3.4:80: i/o timeout", model.ProbeErrTimeout},
		{"context deadline exceeded", model.ProbeErrTimeout},
		{"socket: operation not permitted", model.ProbeErrICMPNotPermitted},
		{"目标地址被拒绝：10.0.0.1", model.ProbeErrBlockedTarget},
		{"something nobody has seen before", model.ProbeErrOther},
	}
	for _, c := range cases {
		if got := classifyErr(errors.New(c.msg)); got != c.want {
			t.Errorf("classifyErr(%q) = %q，期望 %q", c.msg, got, c.want)
		}
	}
	if got := classifyErr(nil); got != "" {
		t.Errorf("classifyErr(nil) = %q，期望空串", got)
	}
}

// TestProbeErrorEnumsAreNonEmpty 是一张防呆网：枚举值不能是空串。
//
// classifyErr 返回空串的语义是"成功"。任何一个枚举常量被改成 ""，
// 都会让"失败"看起来像"成功"——这类改动在 diff 里几乎看不出来。
func TestProbeErrorEnumsAreNonEmpty(t *testing.T) {
	for name, v := range map[string]string{
		"timeout":            model.ProbeErrTimeout,
		"refused":            model.ProbeErrRefused,
		"unreachable":        model.ProbeErrUnreachable,
		"dns_failed":         model.ProbeErrDNSFailed,
		"icmp_not_permitted": model.ProbeErrICMPNotPermitted,
		"blocked_target":     model.ProbeErrBlockedTarget,
		"other":              model.ProbeErrOther,
	} {
		if v == "" {
			t.Errorf("枚举 %s 为空串（会被当成\"探测成功\"）", name)
		}
		if v != name {
			t.Errorf("枚举 %s 的值变成了 %q", name, v)
		}
	}
}

// TestProbeLimits 把两条速率上限钉住（13.4.6）。改小会让节点被当成扫描器，
// 改大则单轮探测可能把周期撑爆。
func TestProbeLimits(t *testing.T) {
	if MaxCountPerRound != 5 {
		t.Errorf("MaxCountPerRound = %d，期望 5", MaxCountPerRound)
	}
	if MaxTargetsPerNode != 30 {
		t.Errorf("MaxTargetsPerNode = %d，期望 30", MaxTargetsPerNode)
	}
}

// ---------------------------------------------------------------------------
// 汇总算术（min/avg/max/loss）
// ---------------------------------------------------------------------------

// TestRTTStatsKeepsZeroAsFirstSample 断言首个成功样本是 0 微秒时 min 必须取 0。
//
// 这不是理论边界：回环 TCP 握手的首个 RTT 实测就是 0 微秒（本机跑出来过
// 0/514/584 三个样本）。旧实现拿 `min == 0` 当"还没赋值"的哨兵，于是
// 0 被丢掉、min 变成第二个样本的 514，算出 min(514) > avg(366) 的不可能三元组，
// 而这个三元组会直接进公开延迟曲线。
//
// 这条用固定样本断言，就是为了不再依赖"回环恰好测到 0"这种运气。
func TestRTTStatsKeepsZeroAsFirstSample(t *testing.T) {
	var s rttStats
	s.add(0, true)
	s.add(514, true)
	s.add(584, true)

	if s.min != 0 {
		t.Errorf("min = %d，期望 0（首个成功样本被哨兵吃掉了）", s.min)
	}
	if s.max != 584 {
		t.Errorf("max = %d，期望 584", s.max)
	}
	avg := s.sum / int64(s.ok)
	if avg != 366 {
		t.Errorf("avg = %d，期望 366", avg)
	}
	if !(s.min <= avg && avg <= s.max) {
		t.Errorf("min/avg/max = %d/%d/%d，三者不自洽", s.min, avg, s.max)
	}
	if s.total != 3 || s.ok != 3 {
		t.Errorf("计数 total=%d ok=%d，期望 3/3", s.total, s.ok)
	}
}

// TestRTTStatsCountsFailuresInTotal 断言失败尝试只进 total（用于算丢包率）。
func TestRTTStatsCountsFailuresInTotal(t *testing.T) {
	var s rttStats
	s.add(1000, true)
	s.add(0, false)
	s.add(0, false)

	if s.ok != 1 || s.total != 3 {
		t.Fatalf("total=%d ok=%d，期望 3/1", s.total, s.ok)
	}
	if s.min != 1000 || s.max != 1000 {
		t.Errorf("min/max 应只由成功样本决定，得到 %d/%d", s.min, s.max)
	}
	if loss := (s.total - s.ok) * 10000 / s.total; loss != 6666 {
		t.Errorf("丢包率 = %d，期望 6666（2/3 向下取整）", loss)
	}
}

// TestProbeAllFailedReportsFullLoss 断言三次全失败时不给 RTT、丢包 100%。
//
// 与上一条的区别：这条走完整的 Probe（含 attempt 循环），
// 确保"全部失败"的判定用的是 ok==0 而不是别的什么计数。
func TestProbeAllFailedReportsFullLoss(t *testing.T) {
	s := New(true).Probe(context.Background(), model.Target{
		ID: 1, Host: "127.0.0.1", Port: closedLoopbackPort(t),
		Method: "tcp", Count: 3, TimeoutMs: 500,
	})
	if s.RTTAvg != nil || s.RTTMin != nil || s.RTTMax != nil {
		t.Errorf("整轮失败时不能有任何 RTT，得到 %+v", s)
	}
	if s.Loss != 10000 {
		t.Errorf("Loss = %d，期望 10000", s.Loss)
	}
	if s.Error == "" {
		t.Error("整轮失败必须带错误码")
	}
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// listenLoopback 起一个丢弃所有连接的 listener，返回它自己。
func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起 listener 失败: %v", err)
	}
	go drain(ln)
	return ln
}

// listenCounting 起一个 listener 并返回端口，收到的连接数累加到 counter。
func listenCounting(t *testing.T, counter *int64) int {
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
			atomic.AddInt64(counter, 1)
			_ = c.Close()
		}
	}()
	// t.Cleanup 在子测试里也会执行，端口用完即关。
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func drain(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}
}

// closedLoopbackPort 拿一个"刚刚还开着、现在已关闭"的端口。
//
// 不能写死 1 或 9 这类端口：它们在部分环境里真的有服务，或者被防火墙丢包
// （丢包 → timeout 而不是 refused，断言就飘了）。先 bind 再 close 拿到的端口
// 在同一台机器上可稳定复现 refused。
func closedLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("关闭 listener 失败: %v", err)
	}
	return port
}

// waitFor 轮询等待条件成立；超时即返回（由调用方断言真实值并给出差异）。
func waitFor(t *testing.T, budget time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
