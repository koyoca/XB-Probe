// Package prober 实现 Agent 的延迟探测：TCP 握手与 ICMP Echo（文档 3.3 / 12.5）。
//
// 安全要点（S4 / 13.4.6）：
//   - 在拨号层对每个解析结果检查并拒绝回环、私网、链路本地、组播与云元数据地址，
//     而不是只在配置阶段检查，以防 DNS 重绑定。
//   - 仅当配置 allow_private_targets: true 时放行。
//   - 速率上限：探测周期不低于 10 秒，每目标每轮不超过 5 次，目标总数不超过 30。
package prober

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"github.com/koyoca/xb-probe/internal/model"
)

// MaxTargetsPerNode 是单节点的目标数量上限（3.1 / 13.4.6）。
const MaxTargetsPerNode = 30

// MaxCountPerRound 是每目标每轮的探测次数上限（13.4.6）。
const MaxCountPerRound = 5

// lookupIP 是 net.LookupIP 的间接层，唯一目的是让测试能注入确定的解析结果。
//
// 为什么需要它：真实网络里存在"**任何**名字都解析成功"的 DNS——沙箱/容器网络的
// 流量拦截、Captive Portal、运营商劫持页都会这么干，连 RFC 2606 保留的 `*.invalid`
// 也一样返回地址（本机实测：`no-such-host.invalid` → `198.18.0.25`）。
// 在这种环境下，"域名拼错了必须报解析失败"的断言会因为"居然解析成功、而且还真能
// 连上"而凭空变红——那条用例测的就不再是代码，而是脚下这张网。
// 本包测试文件开头已写明"刻意不做依赖外网的断言"，这里是兑现它的接缝。
var lookupIP = net.LookupIP

// Prober 执行探测。
type Prober struct {
	allowPrivate bool
}

// New 创建探测器。
func New(allowPrivate bool) *Prober { return &Prober{allowPrivate: allowPrivate} }

// rttStats 汇总一轮探测里的多次尝试。
//
// 抽出来的唯一理由是**可测**：min/avg/max/loss 四个数全部直接进延迟曲线，
// 而它们的边界情况在真实网络里难以复现——偏偏"首次成功"这一档最容易写错：
// 回环探测的首个 RTT 实测就是 **0 微秒**，而拿 0 当"未初始化"哨兵的写法
// 会把这个 0 丢掉，min 于是取自第二个样本，算出 min > avg 的不可能三元组。
type rttStats struct {
	sum, min, max int64
	ok, total     int
}

// add 记录一次尝试；ok=false 表示这次失败（只有 total 前进）。
func (s *rttStats) add(rtt int64, ok bool) {
	s.total++
	if !ok {
		return
	}
	s.ok++
	s.sum += rtt
	// 判"首个成功样本"用的是 ok == 1，**不是** min == 0。
	if s.ok == 1 || rtt < s.min {
		s.min = rtt
	}
	if rtt > s.max {
		s.max = rtt
	}
}

// Probe 对单个目标执行一轮探测，返回汇总结果。
func (p *Prober) Probe(ctx context.Context, t model.Target) model.LatencySample {
	out := model.LatencySample{TargetID: t.ID}

	count := t.Count
	if count <= 0 {
		count = 3
	}
	if count > MaxCountPerRound {
		count = MaxCountPerRound
	}
	timeout := time.Duration(t.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	// 目标地址过滤：先做一次解析期检查，拨号层还会再查一次（防 DNS 重绑定）。
	if err := p.checkResolved(t.Host); err != nil {
		out.Loss = 10000
		// 解析失败与"解析到受限地址"必须分开上报：前者是配置/域名问题（dns_failed），
		// 后者才是 S4 拦住了内网目标（blocked_target）。旧实现把两者都写成
		// blocked_target，于是域名写错的目标在界面上显示"目标被拒绝"，
		// 真正的原因完全看不出来；而 probe_fail 告警又刻意排除 blocked_target，
		// 这种误判会让写错域名的目标永远不报警。
		if errors.Is(err, errBlockedTarget) {
			out.Error = model.ProbeErrBlockedTarget
		} else {
			out.Error = model.ProbeErrDNSFailed
		}
		return out
	}

	var (
		st      rttStats
		lastErr string
	)

	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			out.Error = model.ProbeErrOther
			return out
		default:
		}

		var rtt int64
		var err string
		switch strings.ToLower(t.Method) {
		case "icmp":
			rtt, err = p.probeICMP(ctx, t.Host, timeout)
		default:
			rtt, err = p.probeTCP(ctx, t.Host, t.Port, timeout)
		}
		if err != "" {
			lastErr = err
			st.add(0, false)
			continue
		}
		st.add(rtt, true)
	}

	if st.ok == 0 {
		out.Error = lastErr
		if out.Error == "" {
			out.Error = model.ProbeErrOther
		}
		out.Loss = 10000
		return out
	}

	avg := st.sum / int64(st.ok)
	out.RTTAvg = &avg
	out.RTTMin = &st.min
	out.RTTMax = &st.max
	out.Loss = (st.total - st.ok) * 10000 / st.total
	return out
}

// probeTCP 完成 TCP 三次握手并测量耗时（微秒）。
func (p *Prober) probeTCP(ctx context.Context, host string, port int, timeout time.Duration) (int64, string) {
	if port <= 0 || port > 65535 {
		return 0, model.ProbeErrOther
	}
	dialer := p.dialer(timeout)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	conn, err := dialer.DialContext(cctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return 0, classifyErr(err)
	}
	_ = conn.Close()
	return time.Since(start).Microseconds(), ""
}

// dialer 返回带拨号层地址校验的 Dialer（防 DNS 重绑定，S4）。
func (p *Prober) dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{
		Timeout: timeout,
		Control: func(network, address string, c syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return errors.New("非法地址")
			}
			if !p.allowPrivate && isBlockedIP(ip) {
				return fmt.Errorf("目标地址被拒绝：%s", ip)
			}
			return nil
		},
	}
}

// errBlockedTarget 表示目标解析到了受限地址（S4 内网防护）。
// 与"解析失败"区分开，见 checkResolved。
var errBlockedTarget = errors.New("目标解析到受限地址")

// checkResolved 在解析阶段先检查一次目标地址。
//
// 返回 errBlockedTarget 表示命中了 S4 的黑名单；其他错误一律视为解析失败。
func (p *Prober) checkResolved(host string) error {
	if p.allowPrivate {
		return nil
	}
	ips, err := lookupIP(host)
	if err != nil {
		return fmt.Errorf("解析目标失败: %w", err)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return errBlockedTarget
		}
	}
	return nil
}

// isBlockedIP 判定回环、私网、链路本地、组播、未指定与云元数据地址（13.4.6）。
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	// 云元数据地址（169.254.169.254 属链路本地，已被上面的判断覆盖，
	// 这里额外显式列出以表明这是有意为之）。
	if ip.Equal(net.ParseIP("169.254.169.254")) {
		return true
	}
	// IPv6 唯一本地地址 fc00::/7
	if len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc {
		return true
	}
	return false
}

// probeICMP 发送 ICMP Echo 并测量往返时延（微秒）。
//
// 权限（3.3 / 12.5）：
//   - Linux：优先非特权 datagram socket（需 net.ipv4.ping_group_range 覆盖服务用户）；
//     不可用时上报 icmp_not_permitted，而不是误报离线。
//   - macOS：非特权 datagram socket，无需 root。
//   - Windows：系统 IcmpSendEcho，无需管理员。
//   - FreeBSD：需要 raw socket。
func (p *Prober) probeICMP(ctx context.Context, host string, timeout time.Duration) (int64, string) {
	ips, err := lookupIP(host)
	if err != nil {
		return 0, model.ProbeErrDNSFailed
	}
	var target net.IP
	for _, ip := range ips {
		if ip.To4() != nil {
			target = ip
			break
		}
	}
	if target == nil {
		return 0, model.ProbeErrOther
	}
	if !p.allowPrivate && isBlockedIP(target) {
		return 0, model.ProbeErrBlockedTarget
	}

	// 优先非特权 ICMP（udp4）。
	//
	// **目标地址类型必须跟着 socket 类型走**：udp4 拿到的是 *net.UDPAddr，
	// raw socket 拿到的是 *net.IPAddr；两者混用会得到 EINVAL（"invalid argument"）。
	// 这不是理论问题：Windows 上 udp4 通常建不起来（会退到 raw），
	// 于是"整轮 ICMP 探测都报 other"——网络明明是通的，而探测连续失败告警会一直响。
	// （实测：本机 udp4 报 "The requested protocol has not been configured into the system"，
	// 回退到 raw 后 WriteTo(&net.UDPAddr{}) 每次都是 invalid argument。）
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	dst := net.Addr(&net.UDPAddr{IP: target})
	if err != nil {
		// 退化为 raw socket（FreeBSD 只能走这条路，Windows/Linux 在无特权时也走）。
		conn, err = icmp.ListenPacket("ip4:icmp", "0.0.0.0")
		if err != nil {
			// 两种 socket 都建不起来就是"没权限测"，不是"目标不通"。
			// 报 icmp_not_permitted 而不是 timeout/unreachable：后者会被当成真故障告警。
			return 0, model.ProbeErrICMPNotPermitted
		}
		dst = &net.IPAddr{IP: target}
	}
	defer conn.Close()

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   os.Getpid() & 0xffff,
			Seq:  1,
			Data: []byte("probe"),
		},
	}
	payload, err := msg.Marshal(nil)
	if err != nil {
		return 0, model.ProbeErrOther
	}

	start := time.Now()
	if _, err := conn.WriteTo(payload, dst); err != nil {
		return 0, classifyErr(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return 0, model.ProbeErrTimeout
		}
		rm, err := icmp.ParseMessage(ipv4.ICMPTypeEcho.Protocol(), buf[:n])
		if err != nil {
			continue
		}
		if rm.Type == ipv4.ICMPTypeEchoReply {
			return time.Since(start).Microseconds(), ""
		}
	}
}

// classifyErr 把底层错误映射为 13.4.4 的枚举。
//
// 顺序有讲究：`no route to host` 是**路由不可达**（EHOSTUNREACH/WSAEHOSTUNREACH），
// 必须归到 unreachable。它曾经和 `no such host` 写在同一行归成 dns_failed，
// 于是同一个网络故障在 Linux 上显示"域名解析失败"、在 Windows 上显示"不可达"
// （Windows 的文案里带 unreachable，先被下面的分支捞走了）——平台不一致。
//
// "not permitted" 也必须收：raw socket 被拒时内核给的是 EPERM，
// 文案是 "operation not permitted"，而它**不含** "permission" 这个词
// （是 permitted 不是 permission）。只匹配 "permission" 会让权限问题落进 other，
// 语义从"没测成"变成"测了但不知道怎么回事"，还会被当成真故障进告警。
func classifyErr(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "refused"):
		return model.ProbeErrRefused
	case strings.Contains(msg, "no such host"):
		return model.ProbeErrDNSFailed
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return model.ProbeErrTimeout
	case strings.Contains(msg, "unreachable"), strings.Contains(msg, "network is down"),
		strings.Contains(msg, "no route to host"):
		return model.ProbeErrUnreachable
	case strings.Contains(msg, "permission"), strings.Contains(msg, "not permitted"):
		return model.ProbeErrICMPNotPermitted
	case strings.Contains(msg, "目标地址被拒绝"), strings.Contains(msg, "受限地址"):
		return model.ProbeErrBlockedTarget
	default:
		return model.ProbeErrOther
	}
}
