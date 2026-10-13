package tlsconf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koyoca/xb-probe/internal/acme"
	"github.com/koyoca/xb-probe/internal/config"
)

// TestSetupWritesReadableStatus 是"证书状态可见"这条链路的根：
// CLI 与部署脚本读的是 status.json，而它由服务在启动与每轮重试后写。
// 若哪天写盘被挪走（或路径推导改了），CLI 会永远读到空状态，
// 现象是"部署脚本说证书还没好、后台却明明能用"——没有报错，只能靠人猜。
func TestSetupWritesReadableStatus(t *testing.T) {
	dir := t.TempDir()
	p := mustSetup(t, config.TLSConfig{
		Mode:     "selfsigned",
		CacheDir: dir,
		SelfSign: config.SelfSignedConfig{Hosts: []string{"probe.lan"}, ValidDays: 365},
	}, t.TempDir())

	path := filepath.Join(dir, statusFileName)
	st, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("Setup 之后应当能读到状态文件 %s：%v", path, err)
	}
	if st.Mode != "selfsigned" || st.Source != "self-signed" {
		t.Fatalf("状态里的模式不对: %+v", st)
	}
	if !st.HasCert() {
		t.Fatalf("自签证书在 Setup 时已生成，状态里应当有有效期: %+v", st)
	}
	if st.SPKISHA256 == "" {
		t.Error("状态里缺少 SPKI 指纹——自签部署靠它固定 Agent 的证书")
	}
	if st.UpdatedAt == 0 {
		t.Error("状态缺少写入时间，无法判断它是否新鲜")
	}
	// TLSStatus() 与落盘状态必须一致：两个出口描述同一件事，
	// 不一致时"后台看到的"与"CLI 看到的"会各说一套。
	live := p.TLSStatus()
	if live.Mode != st.Mode || live.NotAfter != st.NotAfter || live.SPKISHA256 != st.SPKISHA256 {
		t.Fatalf("落盘状态与内存状态不一致：\n盘上 %+v\n内存 %+v", st, live)
	}
}

// TestAcmeDNS01WiresProvider：dns-01 时必须在 Setup 阶段就把提供方接上。
//
// 反过来（启动能过、申请时才报"没有 DNS 提供方"）的表现是：
// 服务起来了、证书却永远申请不到，而错误只有去翻日志才看得到。
func TestAcmeDNS01WiresProvider(t *testing.T) {
	dir := t.TempDir()
	p := mustSetup(t, config.TLSConfig{
		Mode:     "acme",
		CacheDir: dir,
		ACME: config.ACMEConfig{
			Domains:        []string{"*.example.com"},
			Challenge:      acme.ChallengeDNS01,
			DnsAPIProvider: "cloudflare",
			DnsAPIToken:    "test-token",
			CA:             "letsencrypt", // 不会联网：申请在后台
		},
	}, t.TempDir())

	// dns-01 不碰本机端口：既不该开明文监听，也不该挂挑战处理器。
	if got := p.PlainHTTPAddr(); got != "" {
		t.Errorf("dns-01 不应开启明文监听，实际 %q", got)
	}
	if p.ChallengeHandler() != nil {
		t.Error("dns-01 不应提供 http-01 挑战处理器")
	}
	st := p.TLSStatus()
	if st.Challenge != acme.ChallengeDNS01 {
		t.Fatalf("挑战类型未透出: %q", st.Challenge)
	}
	if len(st.Domains) != 1 || st.Domains[0] != "*.example.com" {
		t.Fatalf("通配符标识符未透出: %v", st.Domains)
	}
}

// TestAcmeDNS01WithoutTokenFailsAtSetup：缺凭据要在启动时就失败。
func TestAcmeDNS01WithoutTokenFailsAtSetup(t *testing.T) {
	_, err := setup(t, config.TLSConfig{
		Mode:     "acme",
		CacheDir: t.TempDir(),
		ACME: config.ACMEConfig{
			Domains:   []string{"a.example.com"},
			Challenge: acme.ChallengeDNS01,
			// 故意不给 Token
		},
	}, t.TempDir())
	if err == nil {
		t.Fatal("dns-01 缺少 API Token 时应在启动阶段就报错")
	}
	if !strings.Contains(err.Error(), "Token") {
		t.Errorf("错误信息应点名 Token：%v", err)
	}
}

// TestStatusPathSharedByBothSides：路径推导必须只有一份实现。
//
// 这里断言的是"CLI 用的推导与 Provider 实际写盘的位置相同"——
// 两条路径各写一遍时，默认目录一改就会静默错位。
func TestStatusPathSharedByBothSides(t *testing.T) {
	dataDir := filepath.Join("var", "lib", "xb-probe-master")
	// 未指定 cache_dir：落到 data_dir/tls（与 applyDefaults 同一个常量）。
	cfg := config.TLSConfig{Mode: "acme"}
	want := filepath.Join(dataDir, config.DefaultACMECacheDir, statusFileName)
	if got := StatusPath(cfg, dataDir); got != want {
		t.Fatalf("StatusPath = %q，期望 %q", got, want)
	}
	// 显式指定 cache_dir 时以它为准。
	cfg.CacheDir = filepath.Join("etc", "probe", "tls")
	if got := StatusPath(cfg, dataDir); got != filepath.Join(cfg.CacheDir, statusFileName) {
		t.Fatalf("显式 cache_dir 未被采纳：%q", got)
	}
	// 证书路径同理，按模式给。
	if got := CertPath(config.TLSConfig{Mode: "acme", CacheDir: "d"}, "dd"); got != filepath.Join("d", certFileName) {
		t.Errorf("acme 的证书路径不对：%q", got)
	}
	if got := CertPath(config.TLSConfig{Mode: "files", CertFile: "/x/y.pem"}, "dd"); got != "/x/y.pem" {
		t.Errorf("files 模式应回显配置里的 cert_file：%q", got)
	}
	if got := CertPath(config.TLSConfig{Mode: "none"}, "dd"); got != "" {
		t.Errorf("mode=none 不该给出证书路径：%q", got)
	}
}

// TestWriteStatusIsAtomic：状态文件必须整份替换，不能出现"读到写了一半"的瞬间。
//
// 读它的可能是另一个进程（CLI / 部署脚本），半截 JSON 的表现为随机解析失败。
// TestWriteStatusIsAtomic：读者是**另一个进程**（CLI 或部署脚本），而它随时可能来读。
// 原地覆盖会留下"读到写了一半的 JSON"的窗口，现象是随机的解析失败——
// 那种失败既不复现也不报错，只会让人以为状态文件本身坏了。
//
// 判据要挑**能区分两种实现**的那一个，而且不能靠时序去碰运气：
//
//	先给状态文件造一个硬链接别名（同一个 inode 的第二个名字），再把别名内容占成哨兵，
//	然后正式写一次。之后别名上还剩不剩哨兵，正好分开两种写法：
//	  · 先写 <name>.tmp 再 rename：目标名被换成一个**新文件**，别名留在旧 inode 上 → 哨兵还在；
//	  · 直接 os.WriteFile(path)：两个名字共享 inode，别名也跟着变成新内容 → 哨兵没了。
//
// 只断言"没留下 .tmp"是不够的：把 writeStatus 改成直接写盘，那条断言照样绿
// （它根本不产生 .tmp），于是这条护栏就成了纯粹的摆设。
//
// 也**不用** os.SameFile 判"文件身份变了"：Windows 上 MoveFileEx 会复用目标的
// NTFS 记录，rename 前后 SameFile 依然为真——那条断言只能在 Linux 绿。
func TestWriteStatusIsAtomic(t *testing.T) {
	dir := t.TempDir()
	p := mustSetup(t, config.TLSConfig{
		Mode:     "selfsigned",
		CacheDir: dir,
		SelfSign: config.SelfSignedConfig{Hosts: []string{"probe.lan"}, ValidDays: 365},
	}, t.TempDir())
	p.writeStatus()

	path := filepath.Join(dir, statusFileName)
	alias := filepath.Join(dir, "alias-same-inode.json")
	if err := os.Link(path, alias); err != nil {
		t.Skipf("文件系统不支持硬链接，无法区分两种写法，跳过本条：%v", err)
	}
	const sentinel = "SENTINEL-OLD-CONTENT"
	if err := os.WriteFile(path, []byte(sentinel), 0o640); err != nil {
		t.Fatalf("写哨兵失败：%v", err)
	}
	if got := readFileString(t, alias); got != sentinel {
		t.Fatalf("硬链接别名没有与状态文件共享内容（%q），这条用例的前提不成立", got)
	}

	p.writeStatus()

	// 正确的实现：别名（旧 inode）上哨兵必须**原样还在**——rename 只是把"路径"指向
	// 了新文件，旧文件与它的别名一点没动。哨兵没了，就说明新内容是**写进旧 inode**
	// 的，也就是读者有可能看到写了一半的 JSON。
	if got := readFileString(t, alias); got != sentinel {
		t.Errorf("状态文件是**原地覆盖**写的：别名上原本是哨兵 %q，写完却变成 %q。"+
			"应当先写 <name>.tmp 再 rename——那样旧文件与它的别名都不会被碰", sentinel, got)
	}
	if got := readFileString(t, path); !strings.Contains(got, "\"mode\"") {
		t.Errorf("重写之后状态文件的内容不像状态 JSON：%q", got)
	}

	// 顺带一条：临时文件不能留下。留着的话，下次按目录遍历找状态的人会把它当成另一个状态。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("原子写留下了临时文件 %s——下次读目录的人会把它当成另一个状态", e.Name())
		}
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s：%v", path, err)
	}
	return string(raw)
}
