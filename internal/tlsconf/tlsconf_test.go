package tlsconf

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koyoca/xb-probe/internal/acme"
	"github.com/koyoca/xb-probe/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func setup(t *testing.T, cfg config.TLSConfig, dataDir string) (*Provider, error) {
	t.Helper()
	return Setup(cfg, dataDir, quietLogger())
}

func mustSetup(t *testing.T, cfg config.TLSConfig, dataDir string) *Provider {
	t.Helper()
	p, err := setup(t, cfg, dataDir)
	if err != nil {
		t.Fatalf("Setup 失败: %v", err)
	}
	return p
}

// writeSelfSignedFiles 生成一对自签证书文件并返回路径与指纹，供 files 模式用例使用。
func writeSelfSignedFiles(t *testing.T, dir string, hosts []string) (certPath, keyPath, fingerprint string) {
	t.Helper()
	certPEM, keyPEM, err := generateSelfSigned(hosts, 365, time.Now())
	if err != nil {
		t.Fatalf("生成自签证书失败: %v", err)
	}
	certPath = filepath.Join(dir, "custom.crt")
	keyPath = filepath.Join(dir, "custom.key")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("写证书失败: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("写私钥失败: %v", err)
	}
	blk, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("解析证书失败: %v", err)
	}
	fingerprint, _ = spkiFingerprint(leaf)
	return certPath, keyPath, fingerprint
}

func TestSetupNoneDisablesTLS(t *testing.T) {
	p := mustSetup(t, config.TLSConfig{Mode: "none"}, t.TempDir())
	if p.TLSConfig() != nil {
		t.Fatal("mode=none 时不应返回 TLS 配置")
	}
	if p.PlainHTTPAddr() != "" {
		t.Fatal("mode=none 时不应有明文监听")
	}
	if p.ChallengeHandler() != nil {
		t.Fatal("mode=none 时不应有挑战处理器")
	}
	st := p.TLSStatus()
	if st.Mode != "none" || st.Source != "disabled" {
		t.Fatalf("状态描述错误: %+v", st)
	}
	if st.SPKISHA256 != "" || st.NotAfter != 0 {
		t.Fatal("没有证书时不应报告指纹与到期时间")
	}
}

func TestSelfSignedCoversIPAndDomain(t *testing.T) {
	dir := t.TempDir()
	p := mustSetup(t, config.TLSConfig{
		Mode:     "selfsigned",
		CacheDir: dir,
		SelfSign: config.SelfSignedConfig{Hosts: []string{"10.0.0.5", "probe.lan", "::1"}, ValidDays: 30},
	}, t.TempDir())

	cert, err := p.getCertificate(&tls.ClientHelloInfo{ServerName: "probe.lan"})
	if err != nil {
		t.Fatalf("取证书失败: %v", err)
	}
	for _, host := range []string{"10.0.0.5", "probe.lan", "::1"} {
		if err := cert.Leaf.VerifyHostname(host); err != nil {
			t.Fatalf("自签证书 SAN 未覆盖 %s: %v", host, err)
		}
	}
	st := p.TLSStatus()
	if st.Source != "self-signed" {
		t.Fatalf("来源应为 self-signed，实际 %q", st.Source)
	}
	if st.SPKISHA256 == "" || !strings.Contains(st.SPKIHex, ":") {
		t.Fatalf("应报告 SPKI 指纹（含十六进制形式）: %+v", st)
	}
	// 指纹必须是"证书公钥"的 SHA-256，而不是整张证书的。
	want := sha256.Sum256(cert.Leaf.RawSubjectPublicKeyInfo)
	if got := base64.StdEncoding.EncodeToString(want[:]); got != st.SPKISHA256 {
		t.Fatalf("SPKI 指纹口径错误：期望 %s，实际 %s", got, st.SPKISHA256)
	}
	if st.NextRenewal <= time.Now().Unix() {
		t.Fatal("自签证书也应给出续期时间")
	}
	// 自签证书必须落盘，否则每次重启都会换一把密钥，按指纹固定的客户端全部失效。
	for _, f := range []string{selfSignedCertFile, selfSignedKeyFile, selfSignedMetaFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("自签产物 %s 未落盘: %v", f, err)
		}
	}
}

// TestSelfSignedReusesKeyAcrossRestarts 是最重要的一条不变量：
// 重启不得换密钥（SPKI 指纹必须稳定），否则所有已按指纹固定的 Agent 掉线。
func TestSelfSignedReusesKeyAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	cfg := config.TLSConfig{
		Mode:     "selfsigned",
		CacheDir: dir,
		SelfSign: config.SelfSignedConfig{Hosts: []string{"probe.lan"}, ValidDays: 30},
	}
	first, err := setup(t, cfg, t.TempDir())
	if err != nil {
		t.Fatalf("首次 Setup 失败: %v", err)
	}
	second, err := setup(t, cfg, t.TempDir())
	if err != nil {
		t.Fatalf("再次 Setup 失败: %v", err)
	}
	if first.TLSStatus().SPKISHA256 != second.TLSStatus().SPKISHA256 {
		t.Fatal("重启后 SPKI 指纹发生变化：证书被重签了（应按 meta 复用）")
	}
}

// TestSelfSignedRegeneratesWhenHostsChange：改了 hosts 就必须重签，
// 否则证书 SAN 还是旧的，浏览器仍会报名字不匹配。
func TestSelfSignedRegeneratesWhenHostsChange(t *testing.T) {
	dir := t.TempDir()
	base := config.TLSConfig{
		Mode:     "selfsigned",
		CacheDir: dir,
		SelfSign: config.SelfSignedConfig{Hosts: []string{"probe.lan"}, ValidDays: 30},
	}
	first := mustSetup(t, base, t.TempDir())

	changed := base
	changed.SelfSign.Hosts = []string{"probe.lan", "10.0.0.9"}
	second := mustSetup(t, changed, t.TempDir())

	if first.TLSStatus().SPKISHA256 == second.TLSStatus().SPKISHA256 {
		t.Fatal("hosts 变化后应重新签发证书")
	}
	cert, err := second.getCertificate(&tls.ClientHelloInfo{ServerName: "10.0.0.9"})
	if err != nil {
		t.Fatalf("取证书失败: %v", err)
	}
	if err := cert.Leaf.VerifyHostname("10.0.0.9"); err != nil {
		t.Fatalf("新证书未覆盖新增的 IP: %v", err)
	}
}

func TestFilesModeLoadsCustomCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, fp := writeSelfSignedFiles(t, dir, []string{"custom.example.com"})

	p := mustSetup(t, config.TLSConfig{Mode: "files", CertFile: certPath, KeyFile: keyPath}, t.TempDir())
	if got := p.TLSStatus().SPKISHA256; got != fp {
		t.Fatalf("SPKI 指纹应与文件一致：%s vs %s", got, fp)
	}
	if p.TLSStatus().Source != "custom" {
		t.Fatalf("来源应为 custom，实际 %q", p.TLSStatus().Source)
	}
	if p.PlainHTTPAddr() != "" || p.ChallengeHandler() != nil {
		t.Fatal("files 模式不应有 ACME 相关监听/处理器")
	}
}

func TestFilesModeFailsFastOnBadInput(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, _ := writeSelfSignedFiles(t, dir, []string{"a.example.com"})

	if _, err := setup(t, config.TLSConfig{Mode: "files", CertFile: certPath, KeyFile: keyPath + ".missing"}, t.TempDir()); err == nil {
		t.Fatal("私钥缺失时应快速失败")
	}
	if _, err := setup(t, config.TLSConfig{Mode: "files", CertFile: filepath.Join(dir, "nope.crt"), KeyFile: keyPath}, t.TempDir()); err == nil {
		t.Fatal("证书缺失时应快速失败")
	}

	// 证书与私钥不配对也要失败——否则会在运行期变成握手错误，更难查。
	otherCert, otherKey, err := generateSelfSigned([]string{"b.example.com"}, 30, time.Now())
	if err != nil {
		t.Fatalf("生成第二张证书失败: %v", err)
	}
	otherKeyPath := filepath.Join(dir, "other.key")
	if err := os.WriteFile(otherKeyPath, otherKey, 0o600); err != nil {
		t.Fatalf("写第二个私钥失败: %v", err)
	}
	_ = otherCert
	if _, err := setup(t, config.TLSConfig{Mode: "files", CertFile: certPath, KeyFile: otherKeyPath}, t.TempDir()); err == nil {
		t.Fatal("证书与私钥不匹配时应快速失败")
	}
}

// TestFilesModeHotReloads 覆盖"换证书不用重启"这一运维常规动作。
func TestFilesModeHotReloads(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, _ := writeSelfSignedFiles(t, dir, []string{"a.example.com"})
	p := mustSetup(t, config.TLSConfig{Mode: "files", CertFile: certPath, KeyFile: keyPath}, t.TempDir())
	before := p.TLSStatus().SPKISHA256

	// 换一份新证书，并把 mtime 推到明显不同（文件系统时间精度可能很粗）。
	newCert, newKey, err := generateSelfSigned([]string{"b.example.com"}, 30, time.Now())
	if err != nil {
		t.Fatalf("生成新证书失败: %v", err)
	}
	if err := os.WriteFile(certPath, newCert, 0o644); err != nil {
		t.Fatalf("覆盖证书失败: %v", err)
	}
	if err := os.WriteFile(keyPath, newKey, 0o600); err != nil {
		t.Fatalf("覆盖私钥失败: %v", err)
	}
	future := time.Now().Add(2 * time.Minute)
	if err := os.Chtimes(certPath, future, future); err != nil {
		t.Fatalf("调整 mtime 失败: %v", err)
	}

	// 绕过节流（生产里 30 秒检查一次，测试不必真的等）。
	p.fileMu.Lock()
	p.fileNext = time.Time{}
	p.fileMu.Unlock()
	p.reloadFilesIfChanged()

	after := p.TLSStatus().SPKISHA256
	if after == before {
		t.Fatal("证书文件被替换后应热加载出新证书")
	}
	cert, err := p.getCertificate(&tls.ClientHelloInfo{ServerName: "b.example.com"})
	if err != nil {
		t.Fatalf("取证书失败: %v", err)
	}
	if err := cert.Leaf.VerifyHostname("b.example.com"); err != nil {
		t.Fatalf("热加载后的证书应覆盖新域名: %v", err)
	}
}

// TestFilesModeKeepsOldCertWhenNewFileBroken：热加载失败时要保住旧证书。
// 半途变成"没有证书"比继续用旧证书糟糕得多。
func TestFilesModeKeepsOldCertWhenNewFileBroken(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, _ := writeSelfSignedFiles(t, dir, []string{"a.example.com"})
	p := mustSetup(t, config.TLSConfig{Mode: "files", CertFile: certPath, KeyFile: keyPath}, t.TempDir())
	before := p.TLSStatus().SPKISHA256

	if err := os.WriteFile(certPath, []byte("坏掉的证书"), 0o644); err != nil {
		t.Fatalf("写坏文件失败: %v", err)
	}
	future := time.Now().Add(2 * time.Minute)
	_ = os.Chtimes(certPath, future, future)
	p.fileMu.Lock()
	p.fileNext = time.Time{}
	p.fileMu.Unlock()
	p.reloadFilesIfChanged()

	if got := p.TLSStatus().SPKISHA256; got != before {
		t.Fatalf("热加载失败时应保留旧证书，指纹却变了：%s -> %s", before, got)
	}
	if _, err := p.getCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Fatalf("仍应能用旧证书完成握手: %v", err)
	}
}

func TestACMESetupIsNonBlockingAndWiresChallenges(t *testing.T) {
	dir := t.TempDir()
	p := mustSetup(t, config.TLSConfig{
		Mode:     "acme",
		CacheDir: dir,
		ACME: config.ACMEConfig{
			Domains:   []string{"status.example.com"},
			IPs:       []string{"203.0.113.10"},
			Challenge: acme.ChallengeHTTP01,
			HTTPAddr:  ":8080",
			CA:        "letsencrypt", // 不会真的联网：申请在后台进行
		},
	}, t.TempDir())

	// 启动不能被网络阻塞：此时还没有证书，但不该报错。
	st := p.TLSStatus()
	if st.Mode != "acme" || st.Source != "acme" {
		t.Fatalf("状态描述错误: %+v", st)
	}
	if st.Challenge != acme.ChallengeHTTP01 {
		t.Fatalf("挑战类型未透出: %q", st.Challenge)
	}
	if len(st.Domains) != 2 || st.Domains[0] != "status.example.com" || st.Domains[1] != "203.0.113.10" {
		t.Fatalf("域名与 IP 都应出现在标识符清单里: %v", st.Domains)
	}
	if got := p.PlainHTTPAddr(); got != ":8080" {
		t.Fatalf("明文监听地址应为 :8080，实际 %q", got)
	}
	if p.ChallengeHandler() == nil {
		t.Fatal("http-01 模式下必须提供挑战处理器")
	}
	// 账户密钥要落到 acme 子目录（与证书文件分开放）。
	if _, err := os.Stat(filepath.Join(dir, "acme")); err != nil {
		t.Fatalf("ACME 账户目录未创建: %v", err)
	}
	// 还没有证书时，握手必须明确失败（让客户端重试），而不是返回空证书。
	if _, err := p.getCertificate(&tls.ClientHelloInfo{ServerName: "status.example.com"}); err == nil {
		t.Fatal("尚无证书时 getCertificate 应返回错误")
	}
}

func TestACMEHTTPAddrOffDisablesPlainListener(t *testing.T) {
	p := mustSetup(t, config.TLSConfig{
		Mode: "acme",
		ACME: config.ACMEConfig{
			Domains:   []string{"a.example.com"},
			Challenge: acme.ChallengeHTTP01,
			HTTPAddr:  "off",
		},
	}, t.TempDir())
	if got := p.PlainHTTPAddr(); got != "" {
		t.Fatalf("http_addr=off 时不应启动明文监听，实际 %q", got)
	}
	// 但挑战处理器仍要提供——反代可能把 /.well-known/ 转发到其它端口。
	if p.ChallengeHandler() == nil {
		t.Fatal("http_addr=off 时仍应提供挑战处理器")
	}
}

func TestTLSALPN01PrependsALPNProto(t *testing.T) {
	p := mustSetup(t, config.TLSConfig{
		Mode: "acme",
		ACME: config.ACMEConfig{
			Domains:   []string{"a.example.com"},
			Challenge: acme.ChallengeTLSALPN01,
			HTTPAddr:  "off",
		},
	}, t.TempDir())
	tc := p.TLSConfig()
	if len(tc.NextProtos) == 0 || tc.NextProtos[0] != acme.ALPNProto {
		t.Fatalf("tls-alpn-01 必须在 ALPN 首位声明 %s，实际 %v", acme.ALPNProto, tc.NextProtos)
	}
	if p.PlainHTTPAddr() != "" {
		t.Fatal("tls-alpn-01 不需要独立明文监听（http_addr 本就是 off）")
	}
	// ALPN 握手但没有注册挑战证书时，必须明确报错而不是给出业务证书。
	if _, err := p.getCertificate(&tls.ClientHelloInfo{
		ServerName:      "a.example.com",
		SupportedProtos: []string{acme.ALPNProto},
	}); err == nil {
		t.Fatal("无挑战证书时 ALPN 握手应失败")
	}
}

func TestMinTLSVersionApplied(t *testing.T) {
	p := mustSetup(t, config.TLSConfig{
		Mode:     "selfsigned",
		MinVer:   "1.3",
		SelfSign: config.SelfSignedConfig{Hosts: []string{"a.lan"}, ValidDays: 30},
	}, t.TempDir())
	if got := p.TLSConfig().MinVersion; got != tls.VersionTLS13 {
		t.Fatalf("min_version=1.3 应映射到 TLS1.3，实际 %x", got)
	}
}

func TestPlainHTTPHandlerRedirectsNonChallenge(t *testing.T) {
	p := mustSetup(t, config.TLSConfig{
		Mode: "acme",
		ACME: config.ACMEConfig{
			Domains:   []string{"a.example.com"},
			Challenge: acme.ChallengeHTTP01,
			HTTPAddr:  ":80",
		},
	}, t.TempDir())
	h := p.PlainHTTPHandler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://a.example.com/dashboard?x=1", nil)
	req.Host = "a.example.com:80"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("非挑战路径应 301，实际 %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://a.example.com/dashboard?x=1" {
		t.Fatalf("跳转地址错误: %q", loc)
	}

	// 挑战路径（未注册的 token）应由挑战处理器自己回 404，而不是被跳转。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://a.example.com"+acme.HTTP01Prefix+"unknown", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未注册的挑战 token 应 404，实际 %d", rec.Code)
	}
}

// TestACMEFailureIsRecordedAndRetried：申请失败必须留下可查的错误，
// 并且循环要继续重试（否则一次网络抖动就永久没有证书）。
func TestACMEFailureIsRecordedAndRetried(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()

	p := mustSetup(t, config.TLSConfig{
		Mode: "acme",
		ACME: config.ACMEConfig{
			Domains:   []string{"a.example.com"},
			Challenge: acme.ChallengeTLSALPN01,
			HTTPAddr:  "off",
			CA:        broken.URL,
		},
	}, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Run(ctx)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if p.TLSStatus().LastError != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if p.TLSStatus().LastError == "" {
		t.Fatal("申请失败后应记录错误，便于后台/日志排障")
	}
}

func TestRenewAtIsOneThirdOfLifetime(t *testing.T) {
	now := time.Now()
	cases := []struct {
		lifetime time.Duration
		wantLeft time.Duration
	}{
		{90 * 24 * time.Hour, 30 * 24 * time.Hour}, // Let's Encrypt 常见寿命
		{6 * 24 * time.Hour, 2 * 24 * time.Hour},   // RFC 8738 短周期 IP 证书
	}
	for _, tc := range cases {
		leaf := &x509.Certificate{NotBefore: now, NotAfter: now.Add(tc.lifetime)}
		got := renewAt(leaf, now)
		if left := leaf.NotAfter.Sub(got); left != tc.wantLeft {
			t.Fatalf("寿命 %v 应在剩余 %v 时续期，实际剩余 %v", tc.lifetime, tc.wantLeft, left)
		}
	}
	// 时间倒挂的证书不应产生负数续期点。
	leaf := &x509.Certificate{NotBefore: now, NotAfter: now.Add(-time.Hour)}
	if got := renewAt(leaf, now); got.IsZero() {
		t.Fatal("时间倒挂时应退化为按 NotAfter 续期，而不是零值")
	}
}

func TestHostsEqualIgnoresOrderAndCase(t *testing.T) {
	if !hostsEqual([]string{"A.LAN", "10.0.0.1"}, []string{"10.0.0.1", "a.lan"}) {
		t.Fatal("顺序与大小写不应影响比较")
	}
	if hostsEqual([]string{"a.lan"}, []string{"a.lan", "b.lan"}) {
		t.Fatal("条目数不同应判为不同")
	}
	if !hostsEqual([]string{"a.lan"}, []string{" a.lan "}) {
		t.Fatal("首尾空白应被归一化后判定为相同")
	}
}

func TestCertCoversAllRequiresIPAndDNS(t *testing.T) {
	certPEM, _, err := generateSelfSigned([]string{"a.lan", "10.0.0.1"}, 30, time.Now())
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	blk, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !certCoversAll(leaf, []string{"a.lan", "10.0.0.1"}) {
		t.Fatal("应判定为覆盖")
	}
	if certCoversAll(leaf, []string{"a.lan", "10.0.0.2"}) {
		t.Fatal("缺少的 IP 应判为不覆盖")
	}
	if certCoversAll(leaf, []string{"other.lan"}) {
		t.Fatal("缺少的域名应判为不覆盖")
	}
	if certCoversAll(nil, []string{"a.lan"}) {
		t.Fatal("nil 证书应判为不覆盖")
	}
}

func TestSanityOfSourceOf(t *testing.T) {
	cases := map[string]string{
		"none":       "disabled",
		"files":      "custom",
		"selfsigned": "self-signed",
		"acme":       "acme",
		"未知":         "disabled",
	}
	for in, want := range cases {
		if got := sourceOf(in); got != want {
			t.Fatalf("sourceOf(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// SyncObtain 让首次申请**在 Setup 里前台完成**，失败时 Setup 直接返回错误。
//
// 为什么这条不变量值得钉住：部署脚本靠它把"证书到底成没成"变成一个**确定的
// 结果**（成了就继续、没成就回滚）。少了它，部署脚本只能等一段时间然后猜，
// 而用户看到的正是"脚本结束了，https 还是打不开，也不知道为什么"。
//
// 两条必须同时成立，缺一条这条通路就是坏的：
//
//	① 默认（false）**不得**阻塞启动 —— 一次 DNS 抖动不该让 Master 起不来；
//	② 打开 true 时**必须**阻塞并且失败要冒泡 —— 否则部署脚本的回滚分支永远
//	   不会触发，用户拿到的是一个"部署成功"但 https 不可用的部署。
func TestACMEObtainSyncBlocksAndBubblesUpErrors(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()

	base := config.ACMEConfig{
		Domains:   []string{"a.example.com"},
		Challenge: acme.ChallengeTLSALPN01,
		HTTPAddr:  "off",
		CA:        broken.URL,
	}

	// ② 打开 sync_obtain：申请失败必须**让 Setup 失败**。
	acfg := base
	acfg.SyncObtain = true
	if _, err := setup(t, config.TLSConfig{Mode: "acme", ACME: acfg}, t.TempDir()); err == nil {
		t.Fatal("sync_obtain=true 时，申请失败必须让 Setup 返回错误（部署脚本据此回滚）")
	}

	// ① 默认关闭：同样的坏 CA，Setup 必须**立刻成功返回**（申请在后台做）。
	xcfg := base
	xcfg.SyncObtain = false
	if _, err := setup(t, config.TLSConfig{Mode: "acme", ACME: xcfg}, t.TempDir()); err != nil {
		t.Fatalf("sync_obtain 关闭时 Setup 不该被网络阻塞，实际报错：%v", err)
	}
}

// 申请失败时，错误必须已经**落进状态文件**。
//
// 这条容易被忽略但很关键：sync_obtain 失败时服务会退出，此时唯一能告诉用户
// "为什么失败"的就是 status.json。不落盘的话，部署脚本的 `tls status` 只能
// 报一句"还没结果"——而真正的原因（CA 返回了什么）就永远丢了。
func TestACMEObtainSyncWritesStatusBeforeFailing(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()

	dir := t.TempDir()
	cache := filepath.Join(dir, "tls")
	cfg := config.TLSConfig{
		Mode:     "acme",
		CacheDir: cache,
		ACME: config.ACMEConfig{
			Domains:    []string{"a.example.com"},
			Challenge:  acme.ChallengeTLSALPN01,
			HTTPAddr:   "off",
			CA:         broken.URL,
			SyncObtain: true,
		},
	}
	if _, err := setup(t, cfg, dir); err == nil {
		t.Fatal("预期 Setup 失败")
	}

	st, err := ReadStatus(StatusPath(cfg, dir))
	if err != nil {
		t.Fatalf("申请失败后必须留下状态文件（部署脚本的 tls status 靠它报原因）：%v", err)
	}
	if st.LastError == "" {
		t.Fatal("状态文件里没有 LastError —— 用户拿不到失败原因")
	}
	if st.LastAttemptAt == 0 {
		t.Fatal("状态文件里没有 LastAttemptAt —— tls status 无法区分「本轮失败」与「上次遗留」")
	}
}
