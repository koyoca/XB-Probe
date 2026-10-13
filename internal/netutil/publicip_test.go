package netutil

import (
	"net"
	"testing"
)

func mustParse(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("测试自身有误：%q 不是合法地址", s)
	}
	return ip
}

// TestIsPublicIPRejectsEveryNonRoutableClass 是这条判断的护栏。
//
// 它的失败模式非常安静：过滤掉"多一个"没什么感觉，而**漏掉一个**就会让
// 127.0.0.1 / 192.168.x.x / 100.64.x.x 这类地址出现在"节点的公网地址"栏里，
// 界面看起来一切正常。所以这里把每一类都单列一行，而不是笼统地"跑一批地址"。
//
// 特别注意 @private-v6（ULA fc00::/7）：Go 的 IP.IsGlobalUnicast() 对它是**返回 true** 的
// （ULA 在 Go 眼里算全球单播），只靠 IsGlobalUnicast 会漏掉一整类地址 ——
// 这也是下面"只写 IsGlobalUnicast 会怎样"那条对照存在的原因。
func TestIsPublicIPRejectsEveryNonRoutableClass(t *testing.T) {
	reject := []struct{ name, addr string }{
		{"@loopback-v4", "127.0.0.1"},
		{"@loopback-v4-any", "127.9.9.9"},
		{"@loopback-v6", "::1"},
		{"@private-v4-10", "10.0.0.5"},
		{"@private-v4-172", "172.16.31.7"},
		{"@private-v4-192", "192.168.1.1"},
		{"@private-v6", "fd00::1"},
		{"@private-v6-ula-low", "fc00::1"},
		{"@linklocal-v4", "169.254.169.254"},
		{"@linklocal-v6", "fe80::1"},
		{"@unspecified-v4", "0.0.0.0"},
		{"@unspecified-v6", "::"},
		{"@multicast-v4", "224.0.0.1"},
		{"@multicast-v6", "ff02::1"},
		{"@broadcast", "255.255.255.255"},
		{"@cgnat", "100.64.0.1"},
		{"@cgnat-high", "100.127.255.254"},
		{"@empty", ""},
		{"@junk", "not-an-ip"},
		{"@hostname", "example.com"},
		{"@v4-with-port", "1.2.3.4:80"},
	}
	for _, c := range reject {
		if IsPublicIP(c.addr) {
			t.Errorf("%s：%q 不该被当成公网地址", c.name, c.addr)
		}
	}

	accept := []struct{ name, addr string }{
		{"@v4", "1.2.3.4"},
		{"@v4-real", "203.0.113.9"},
		{"@v4-with-space", " 8.8.8.8 "},
		{"@v4-cgnat-boundary-low", "100.63.255.255"},
		{"@v4-cgnat-boundary-high", "100.128.0.0"},
		{"@v6", "2001:4860:4860::8888"},
		{"@v6-full", "2606:4700:4700::1111"},
	}
	for _, c := range accept {
		if !IsPublicIP(c.addr) {
			t.Errorf("%s：%q 应当被当成公网地址", c.name, c.addr)
		}
	}
}

// TestIsPublicIPNeedsMoreThanGlobalUnicast 是上一条的对照组。
//
// 单写 net.IP.IsGlobalUnicast() 是最容易犯的错（它名字听起来正好就是我们要的），
// 而它对 ULA 与 CGNAT 都放行。这里直接对**同一批地址**断言两种实现的结论不同，
// 从而钉死"必须额外排除私网与 CGNAT"这件事 —— 若哪天有人把 IsPrivate 那行删掉，
// 这条会红。
func TestIsPublicIPNeedsMoreThanGlobalUnicast(t *testing.T) {
	for _, addr := range []string{"fd00::1", "fc00::1"} {
		ip := mustParse(t, addr)
		if !ip.IsGlobalUnicast() {
			t.Fatalf("%s：前提不成立——本用例假定 IsGlobalUnicast 对它返回 true，"+
				"若 Go 改了语义，请换一个仍然成立的地址来钉这条规则", addr)
		}
		if IsPublicIP(addr) {
			t.Errorf("%s 是 ULA，只判 IsGlobalUnicast 会放行，必须被 IsPublicIP 拒绝", addr)
		}
	}
	for _, addr := range []string{"100.64.0.1", "100.127.255.254"} {
		ip := mustParse(t, addr)
		if ip.IsPrivate() {
			t.Fatalf("%s：前提不成立——CGNAT 不在 IsPrivate 的覆盖范围内", addr)
		}
		if IsPublicIP(addr) {
			t.Errorf("%s 是 CGNAT 地址，必须被 IsPublicIP 拒绝", addr)
		}
	}
}

// TestFirstPublicIPReadsEveryShapeOfAnswer 钉住"不按格式适配"这件事。
//
// 文本里同时出现回环/私网与公网地址时，**必须跳过前者取后者** ——
// 这正是真实故障的形状：查询服务经由本机代理返回了 127.0.0.1，
// 若取"第一个 ParseIP 成功的"，写进地址栏的就是回环地址。
func TestFirstPublicIPReadsEveryShapeOfAnswer(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"plain", "1.2.3.4\n", "1.2.3.4"},
		{"json", `{"ip":"1.2.3.4","country":"CN"}`, "1.2.3.4"},
		{"json-spaced", `{ "ip" : "203.0.113.9" }`, "203.0.113.9"},
		{"with-scheme", "master seen from 203.0.113.9:443\n", "203.0.113.9"},
		{"skip-loopback", "127.0.0.1 203.0.113.9", "203.0.113.9"},
		{"skip-private", "192.168.1.1\n8.8.8.8\n", "8.8.8.8"},
		{"v6", "2001:4860:4860::8888\n", "2001:4860:4860::8888"},
		{"only-loopback", "127.0.0.1\n", ""},
		{"only-private", "10.1.2.3\n", ""},
		{"empty", "", ""},
		{"html-error-page", "<html><body>403 Forbidden</body></html>", ""},
		{"version-looking-junk", "service 1.0.0 build 2024\n", ""},
	}
	for _, c := range cases {
		if got := FirstPublicIP(c.body); got != c.want {
			t.Errorf("%s：FirstPublicIP(%q) = %q，期望 %q", c.name, c.body, got, c.want)
		}
	}
}
