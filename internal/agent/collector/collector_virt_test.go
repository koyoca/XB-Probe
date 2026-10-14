package collector

import "testing"

// 容器身份解析的回归护栏（用户上报：节点是 LXC 容器，虚拟化却显示 kvm）。
//
// 为什么这些判据值得单测：真正的 detectVirt 是 linux 专属实现（//go:build linux），
// 在开发机（Windows）上根本不会被编译，更不会被执行——"本地永远跳过"等于"从未验证"。
// 所以把**纯解析**部分（自报名归一化、environ 取值、NSpid 计数）放进无 build tag
// 的文件，在这里钉住；读文件、看路径那些副作用留在 linux 实现里。
func TestContainerEnvNameIsNormalized(t *testing.T) {
	// 每一条都对应一种真实运行时的自报值。
	cases := []struct{ in, want string }{
		{"lxc", "lxc"},
		{"lxc-libvirt", "lxc"},
		{"docker", "docker"},
		{"podman", "docker"},
		{"containerd", "docker"},
		{"kubepods", "k8s"},
		{"systemd-nspawn", "systemd-nspawn"},
		{"openvz", "openvz"},
		{"wsl", "wsl"},
		// 自报了名字但不在已知集合里 → 仍是"在容器里"，只是叫不出名字。
		// 这一条是本次 bug 的关键：**绝不能**因为认不出名字就回落到宿主机的
		// 指纹（那会把 LXC 报成宿主那台 KVM 虚机的 kvm）。
		{"some-new-runtime", "container"},
		// 空值 = 没有这条证据，交给后面的判据。
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := containerNameOf(c.in); got != c.want {
			t.Errorf("containerNameOf(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// envValue 要从 NUL 转成换行后的 environ 里精确取出 container= 的值，
// 不能被同名前缀的别的键骗到（比如 container_id=，或别的键的值里恰好含 container=）。
func TestEnvValueTakesWholeKey(t *testing.T) {
	blob := []byte("PATH=/usr/bin\ncontainer=lxc\ncontainer_id=abc\nHOME=/root\n")
	if got := envValue(blob, "container"); got != "lxc" {
		t.Errorf("envValue 取到 %q，期望 lxc（前缀匹配必须是完整键名，container_id 不算）", got)
	}
	// PID 1 的 environ 里没有这个变量时，返回空串（调用方据此继续找别处）。
	if got := envValue([]byte("PATH=/usr/bin\n"), "container"); got != "" {
		t.Errorf("没有该键时应当返回空串，实得 %q", got)
	}
	// 值里带等号（base64、URL 之类）要原样保留。
	if got := envValue([]byte("container=a=b\n"), "container"); got != "a=b" {
		t.Errorf("值里的等号被截断了：%q", got)
	}
}

// NSpid 的个数就是"在几层 PID 命名空间里"：宿主机 1 个、容器 ≥2 个。
func TestNsPidCount(t *testing.T) {
	hostLike := "Name:\tagent\nPid:\t1234\nNSpid:\t1234\nPPid:\t1\n"
	if got := nsPidCount(hostLike); got != 1 {
		t.Errorf("宿主机形态的 NSpid 应当解析出 1 个 PID，实得 %d", got)
	}
	boxLike := "Name:\tagent\nPid:\t42\nNSpid:\t987654 42\nPPid:\t1\n"
	if got := nsPidCount(boxLike); got != 2 {
		t.Errorf("容器形态的 NSpid 应当解析出 2 个 PID，实得 %d", got)
	}
	// 没有这一行（老内核 / 受限的 /proc）：返回 0 = "拿不到证据"，
	// 调用方据此回落到其它判据，而不是当成"不在容器里"。
	if got := nsPidCount("Name:\tagent\nPid:\t42\n"); got != 0 {
		t.Errorf("缺少 NSpid 行时应当返回 0（拿不到证据），实得 %d", got)
	}
}
