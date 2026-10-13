// Package netutil 提供"这个地址能不能当作节点的公网地址"这一条判断。
//
// 为什么单独成一个包：Master 与 Agent 都要用它，而且必须**只有一处实现**。
// Master 用它过滤"来源地址"与"Agent 自报地址"（同机部署时来源地址就是 127.0.0.1，
// 写进"节点的公网地址"那一栏就是错的）；Agent 用它过滤"公网地址探测服务"的返回值
// （服务可能因为走代理、DNS 劫持或本身是内网服务而返回一个私网地址）。
// 两处各写一份必然漂移，而漂移的症状是静默的：地址栏里出现一个私网地址。
package netutil

import (
	"net"
	"regexp"
	"sort"
	"strings"
)

// IsPublicIP 判断 s 是不是"可以直接当作节点公网地址"的地址。
//
// 排除项与理由：
//   - 回环（127.0.0.0/8、::1）：同机部署时来源地址就是它，正是"地址栏恒显示 127.0.0.1"的成因。
//   - 私网（RFC 1918 的 10/8、172.16/12、192.168/16 与 RFC 4193 的 fc00::/7）：
//     内网地址对"这个节点的公网地址"没有意义。
//   - 链路本地（169.254/16、fe80::/10）与组播、未指定（0.0.0.0、::）、广播：
//     都不是可路由的单播地址。
//   - 运营商大内网 CGNAT（100.64.0.0/10）：它看起来像公网（IsPrivate 不认它），
//     但跨网根本不可达，放进地址栏只会误导。
//
// 刻意不做的事：
//   - 不解析域名。"公网地址"这一栏是地址，不是主机名；允许域名会让前端与
//     防火墙规则都得跟着处理两者。
//   - 不做 DNS 解析。这个判断必须是纯函数：它被调用在上报这样的高频路径上，
//     任何一次网络查询都会把一次上报拖慢甚至拖挂。
func IsPublicIP(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return false
	}
	// IsGlobalUnicast 一次性排掉回环、组播、链路本地单播、未指定与广播。
	if !ip.IsGlobalUnicast() {
		return false
	}
	// IsPrivate 覆盖 RFC 1918（IPv4）与 RFC 4193 / ULA fc00::/7（IPv6）。
	// ULA 在 Go 的实现里**算** global unicast，所以这一条不能省。
	if ip.IsPrivate() {
		return false
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 0x40 {
		return false // 100.64.0.0/10：CGNAT
	}
	return true
}

// 抠地址用的两条正则。刻意分成 v4/v6 两条而不是"一条宽松字符集"：
// 宽松字符集会把 `203.0.113.9:443` 整段当成一个候选，ParseIP 失败之后
// **连里面那个合法地址一起丢掉**（真实响应里带端口、带 URL 的比比皆是）。
var (
	ipv4CandidateRe = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}`)
	ipv6CandidateRe = regexp.MustCompile(`[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}`)
)

// FirstPublicIP 从一段任意文本里取出第一个公网地址，取不到返回空串。
//
// 之所以要"从文本里抠"而不是让调用方先定格式：公网地址查询服务的返回格式五花八门 ——
// 有纯文本（`1.2.3.4\n`）、有 JSON（`{"ip":"1.2.3.4"}`）、有 JSON 数组、
// 还有夹在一句话里的。挨个适配格式，等于把"换一个服务就要改一次代码"写进设计里。
//
// 两个要点：
//   - 按**出现位置**排序后取第一个合法且公网的候选，而不是"先看 v4 再看 v6"：
//     否则一个同时给出两种地址的响应会永远只取 v4，与文本顺序无关。
//   - 候选的合法性判据是 IsPublicIP。服务返回 127.0.0.1（它自己在回环上、
//     或我们经由本机代理访问它）时必须一个都不接受 —— 否则加的探测反倒把错地址写进去。
func FirstPublicIP(body string) string {
	type cand struct {
		idx  int
		text string
	}
	var cands []cand
	for _, re := range []*regexp.Regexp{ipv4CandidateRe, ipv6CandidateRe} {
		for _, loc := range re.FindAllStringIndex(body, -1) {
			cands = append(cands, cand{loc[0], body[loc[0]:loc[1]]})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].idx < cands[j].idx })
	for _, c := range cands {
		if IsPublicIP(c.text) {
			return c.text
		}
	}
	return ""
}
