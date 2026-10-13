// Package tlsconf 决定"证书从哪来、什么时候换、握手里给哪张"。
//
// 四种取证书方式（对应 master.yaml 的 tls.mode）：
//
//	none        不启用 TLS（前置反代卸载，或本机/内网调试）
//	files       自定义证书：直接读 cert_file / key_file，支持热加载
//	selfsigned  自签：本进程生成 ECDSA 证书，SAN 覆盖 IP 与域名
//	acme        自动申请：ACME v2，支持域名证书与 RFC 8738 的 IP 证书，
//	            挑战可选 http-01 / tls-alpn-01 / dns-01（后者支持通配符）
//
// 设计上刻意保持"轻"：不引入任何新依赖（ACME 协议由 internal/acme 自实现），
// 证书落盘复用同一把密钥（续期不换钥匙，否则已按 SPKI 指纹固定
// 的 Agent 会全部掉线），并且启动不被网络阻塞——ACME 首次申请在后台进行，
// 申请结果落进 status.json（见 status.go）供 `xb-probe-master tls status` 查看。
package tlsconf

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/koyoca/xb-probe/internal/acme"
	"github.com/koyoca/xb-probe/internal/config"
	"github.com/koyoca/xb-probe/internal/model"
)

const (
	certFileName = "cert.pem"
	keyFileName  = "cert.key"

	selfSignedCertFile = "selfsigned.pem"
	selfSignedKeyFile  = "selfsigned.key"
	selfSignedMetaFile = "selfsigned.json"

	// fileReloadInterval 限制自定义证书的 mtime 检查频率：
	// 否则每次 TLS 握手都要 stat 两次磁盘。
	fileReloadInterval = 30 * time.Second
	// renewCheckInterval 是后台管理循环的常规节拍。
	renewCheckInterval = 12 * time.Hour
	// retryWithoutCert 是没有可用证书时的重试间隔（尽快自愈）。
	retryWithoutCert = 2 * time.Minute
	// acmeObtainTimeout 限制单次申请的总耗时（含挑战等待与轮询）。
	acmeObtainTimeout = 5 * time.Minute
)

// Provider 是证书提供方：对外给 tls.Config / 挑战处理器 / 运行态。
type Provider struct {
	mode     string
	log      *slog.Logger
	cacheDir string
	minVer   uint16

	// files
	certFile, keyFile string

	// selfsigned
	ssHosts     []string
	ssValidDays int

	// acme
	acmeClient   *acme.Client
	acmeDomains  []string
	acmeIPs      []string
	challenge    string
	acmeHTTPAddr string

	mu          sync.RWMutex
	cert        *tls.Certificate
	nextRenewal time.Time
	lastErr     string
	// lastAttempt 是最近一次申请/续期尝试的开始时间，只用于状态展示
	// （见 status.go）：运维判断"它是不是还在重试"靠的就是它。
	lastAttempt time.Time

	// renewMu 串行化申请/续期：并发下单会撞 CA 的速率限制。
	renewMu sync.Mutex

	// fileMu 保护自定义证书的热加载状态。
	fileMu   sync.Mutex
	fileMod  time.Time
	fileSize int64
	fileNext time.Time
}

// Setup 按配置构建 Provider。mode=files 时证书读不出来会直接报错（快速失败）；
// mode=acme 时证书在后台申请，不阻塞启动。
func Setup(cfg config.TLSConfig, dataDir string, logger *slog.Logger) (*Provider, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cacheDir := strings.TrimSpace(cfg.CacheDir)
	if cacheDir == "" {
		cacheDir = filepath.Join(dataDir, "tls")
	}
	p := &Provider{
		mode:        strings.ToLower(strings.TrimSpace(cfg.Mode)),
		log:         logger,
		cacheDir:    cacheDir,
		minVer:      tls.VersionTLS12,
		challenge:   cfg.ACME.Challenge,
		acmeDomains: cfg.ACME.Domains,
		acmeIPs:     cfg.ACME.IPs,
	}

	switch cfg.MinVer {
	case "1.3":
		p.minVer = tls.VersionTLS13
	}

	switch p.mode {
	case "none":
		return p, nil

	case "files":
		p.certFile, p.keyFile = cfg.CertFile, cfg.KeyFile
		if err := p.loadFiles(); err != nil {
			return nil, err
		}

	case "selfsigned":
		p.ssHosts = cfg.SelfSign.Hosts
		p.ssValidDays = cfg.SelfSign.ValidDays
		if err := p.ensureSelfSigned(); err != nil {
			return nil, err
		}

	case "acme":
		p.acmeHTTPAddr = cfg.ACME.HTTPAddr
		acmeCfg := acme.Config{
			DirectoryURL: acme.ResolveDirectory(cfg.ACME.CA),
			Email:        cfg.ACME.Email,
			CacheDir:     filepath.Join(cacheDir, "acme"),
			Logger:       logger,
		}
		// dns-01 需要一个"记录写手"。在这里（而不是申请时）构造：
		// 凭据写错应当在**启动时**就暴露，而不是等首张证书申请失败才发现
		// ——那时错误信息只会是"授权校验失败"，离"Token 权限不对"很远。
		if cfg.ACME.Challenge == acme.ChallengeDNS01 {
			provider, err := newDNSProvider(cfg.ACME, logger)
			if err != nil {
				return nil, err
			}
			acmeCfg.DNSProvider = provider
		}
		client, err := acme.New(acmeCfg)
		if err != nil {
			return nil, fmt.Errorf("初始化 ACME 客户端失败: %w", err)
		}
		p.acmeClient = client
		// 有缓存证书就先用起来，避免重启期间 TLS 不可用；
		// 没有则留给 Run() 在后台申请（启动不被网络阻塞）。
		if pair, err := p.loadCachedACMECert(); err == nil {
			p.installCert(pair, renewAt(pair.Leaf, time.Now()))
		} else if !errors.Is(err, os.ErrNotExist) {
			p.lastErr = err.Error()
			logger.Warn("缓存的 ACME 证书不可用，将重新申请", "err", err.Error())
		} else if cfg.ACME.SyncObtain {
			// 同步申请：**只有部署脚本会打开它**（见 deploy-master.sh 的证书步骤）。
			//
			// 平时刻意不这么做：一个 DNS 或网络问题不该让整个 Master 起不来，
			// 所以首次申请历来在后台跑。但部署那一刻是例外——那时用户正等着
			// 一个确定答案（"到底能不能用 https"），而后台申请给出的是一段
			// 没有任何反馈的空窗，他只能靠猜。
			//
			// 打开它之后的语义是"申请不成功就不启动"：申请失败时 Setup 返回
			// 错误，服务退出，部署脚本据此**停止安装并回滚**。成功时证书已经在
			// 手里，服务起来立刻就能握手——不必再等一次后台重试。
			ctx, cancel := context.WithTimeout(context.Background(), acmeObtainTimeout)
			err := p.obtainACMESync(ctx)
			cancel()
			if err != nil {
				// 状态先落盘再返回：部署脚本的 tls status 要靠它报出原因，
				// 而服务已经退出了，没有别的地方能提供这个原因。
				p.writeStatus()
				return nil, fmt.Errorf("申请证书失败: %w", err)
			}
			logger.Info("证书已在前台申请成功（tls.acme.sync_obtain）")
		}

	default:
		return nil, fmt.Errorf("未知的 tls.mode: %q", cfg.Mode)
	}
	// 启动时先落一次状态：这样"服务起来了但还没申请到证书"也是可读的，
	// 而不是要等到第一次重试之后文件才出现。
	p.writeStatus()
	return p, nil
}

// newDNSProvider 按配置构造 dns-01 的记录写手。
//
// 目前只支持 Cloudflare。把"支持哪几家"这件事收在这里，而不是散在
// 配置校验与调用点两处——两处各写一份的表现是：配置说支持、运行时报不支持。
func newDNSProvider(cfg config.ACMEConfig, logger *slog.Logger) (acme.DNSProvider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.DnsAPIProvider)) {
	case "", "cloudflare":
		provider, err := acme.NewCloudflare(acme.CloudflareConfig{
			APIToken: cfg.DnsAPIToken,
			Logger:   logger,
		})
		if err != nil {
			return nil, fmt.Errorf("初始化 Cloudflare DNS 提供方失败: %w", err)
		}
		return provider, nil
	default:
		return nil, fmt.Errorf("不支持的 tls.acme.dns_provider: %q（当前仅支持 cloudflare）", cfg.DnsAPIProvider)
	}
}

// Mode 返回当前的取证书方式。
func (p *Provider) Mode() string { return p.mode }

// TLSConfig 返回监听器要用的 *tls.Config；mode=none 时返回 nil。
func (p *Provider) TLSConfig() *tls.Config {
	if p.mode == "none" {
		return nil
	}
	tc := &tls.Config{
		MinVersion:     p.minVer,
		GetCertificate: p.getCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}
	// tls-alpn-01 需要在 ALPN 里声明 acme-tls/1，否则 CA 的校验握手
	// 根本协商不到我们准备好的挑战证书。
	if p.mode == "acme" && p.challenge == acme.ChallengeTLSALPN01 {
		tc.NextProtos = append([]string{acme.ALPNProto}, tc.NextProtos...)
	}
	return tc
}

// ChallengeHandler 返回 ACME http-01 的挑战应答端点（非 http-01 时为 nil）。
//
// 它需要被挂到**所有**监听地址上：操作者既可能直接放行 80 端口，
// 也可能让反代把 /.well-known/acme-challenge/ 转发到本进程的其它端口。
func (p *Provider) ChallengeHandler() http.Handler {
	if p.mode != "acme" || p.challenge != acme.ChallengeHTTP01 {
		return nil
	}
	return p.acmeClient.HTTP01Handler()
}

// PlainHTTPHandler 返回独立明文监听用的处理器：挑战走应答，其余一律跳 https。
//
// 明文端口只为 ACME 而开，不应变成"第二个站点"——那样既多暴露一份内容，
// 也让 HSTS 的意义被削弱。
func (p *Provider) PlainHTTPHandler() http.Handler {
	challenge := p.ChallengeHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if challenge != nil && strings.HasPrefix(r.URL.Path, acme.HTTP01Prefix) {
			challenge.ServeHTTP(w, r)
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
}

// PlainHTTPAddr 返回是否需要（以及在哪里）启动独立明文监听。
// 空串表示不需要。
func (p *Provider) PlainHTTPAddr() string {
	if p.mode != "acme" || p.challenge != acme.ChallengeHTTP01 {
		return ""
	}
	if isOffAddr(p.acmeHTTPAddr) {
		return ""
	}
	return p.acmeHTTPAddr
}

func isOffAddr(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "none", "-", "":
		return true
	}
	return false
}

// Run 启动后台的申请/续期循环。mode=none/files 时它不做任何事。
func (p *Provider) Run(ctx context.Context) {
	if p.mode != "acme" && p.mode != "selfsigned" {
		return
	}
	go p.manageLoop(ctx)
}

// TLSStatus 实现 model.TLSStatusReader。
func (p *Provider) TLSStatus() model.TLSStatus {
	p.mu.RLock()
	cert := p.cert
	next := p.nextRenewal
	lastErr := p.lastErr
	p.mu.RUnlock()

	st := model.TLSStatus{
		Mode:        p.mode,
		Source:      sourceOf(p.mode),
		LastError:   lastErr,
		SPKISHA256:  "",
		NextRenewal: 0,
	}
	if p.mode == "acme" {
		st.Challenge = p.challenge
		st.Domains = append(append([]string{}, p.acmeDomains...), p.acmeIPs...)
	}
	if p.mode == "selfsigned" {
		st.Domains = append([]string{}, p.ssHosts...)
	}
	if cert != nil && cert.Leaf != nil {
		s := summarize(cert.Leaf)
		st.Subject = s.Subject
		st.Issuer = s.Issuer
		st.SANs = s.SANs
		st.NotBefore = cert.Leaf.NotBefore.Unix()
		st.NotAfter = cert.Leaf.NotAfter.Unix()
		st.SPKISHA256, st.SPKIHex = spkiFingerprint(cert.Leaf)
	}
	if !next.IsZero() {
		st.NextRenewal = next.Unix()
	}
	return st
}

func sourceOf(mode string) string {
	switch mode {
	case "files":
		return "custom"
	case "selfsigned":
		return "self-signed"
	case "acme":
		return "acme"
	}
	return "disabled"
}

// ---------------------------------------------------------------------------
// 握手
// ---------------------------------------------------------------------------

func (p *Provider) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	// tls-alpn-01：CA 会以 ALPN=acme-tls/1 发起握手，此时必须返回
	// 带 acmeIdentifier 扩展的挑战证书，而不是业务证书。
	if p.mode == "acme" && p.challenge == acme.ChallengeTLSALPN01 && hasProto(hello.SupportedProtos, acme.ALPNProto) {
		if c, ok := p.acmeClient.TLSALPNCertificate(hello.ServerName); ok {
			return c, nil
		}
		return nil, fmt.Errorf("tls-alpn-01 挑战证书尚未就绪（SNI=%q）", hello.ServerName)
	}
	if p.mode == "files" {
		p.reloadFilesIfChanged()
	}
	p.mu.RLock()
	cert := p.cert
	p.mu.RUnlock()
	if cert == nil {
		// 让握手失败并让客户端重试，而不是返回一张空证书（那会变成
		// 一个更难懂的 TLS 内部错误）。Run() 会在后台持续重试。
		return nil, errors.New("TLS 证书尚未就绪")
	}
	return cert, nil
}

func hasProto(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// files：自定义证书（支持热加载）
// ---------------------------------------------------------------------------

func (p *Provider) loadFiles() error {
	pair, _, _, err := loadKeyPair(p.certFile, p.keyFile)
	if err != nil {
		return err
	}
	if st, err := os.Stat(p.certFile); err == nil {
		p.fileMod, p.fileSize = st.ModTime(), st.Size()
	}
	p.installCert(pair, time.Time{})
	s := summarize(pair.Leaf)
	p.log.Info("已加载自定义证书", "subject", s.Subject, "not_after", pair.Leaf.NotAfter.Format(time.RFC3339))
	if time.Now().After(pair.Leaf.NotAfter) {
		p.log.Error("自定义证书已过期，请尽快更换", "not_after", pair.Leaf.NotAfter.Format(time.RFC3339))
	}
	return nil
}

// reloadFilesIfChanged 在证书文件被替换后热加载。
// 换证书是运维常规动作（续期脚本刚写完新文件），要求重启进程很反直觉。
// 失败时保留旧证书继续服务，只记一条告警——半途变成"没有证书"更糟。
func (p *Provider) reloadFilesIfChanged() {
	p.fileMu.Lock()
	if time.Now().Before(p.fileNext) {
		p.fileMu.Unlock()
		return
	}
	p.fileNext = time.Now().Add(fileReloadInterval)
	st, err := os.Stat(p.certFile)
	if err != nil {
		p.fileMu.Unlock()
		return
	}
	if st.ModTime().Equal(p.fileMod) && st.Size() == p.fileSize {
		p.fileMu.Unlock()
		return
	}
	// 先在这里推进记录，避免文件写坏时每 30 秒重试一次刷屏。
	p.fileMod, p.fileSize = st.ModTime(), st.Size()
	p.fileMu.Unlock()

	pair, _, _, err := loadKeyPair(p.certFile, p.keyFile)
	if err != nil {
		p.log.Warn("自定义证书热加载失败，继续使用旧证书", "err", err.Error())
		return
	}
	p.installCert(pair, time.Time{})
	s := summarize(pair.Leaf)
	p.log.Info("自定义证书已热加载", "subject", s.Subject, "not_after", pair.Leaf.NotAfter.Format(time.RFC3339))
}

// ---------------------------------------------------------------------------
// selfsigned
// ---------------------------------------------------------------------------

func (p *Provider) selfSignedPaths() (cert, key, meta string) {
	return filepath.Join(p.cacheDir, selfSignedCertFile),
		filepath.Join(p.cacheDir, selfSignedKeyFile),
		filepath.Join(p.cacheDir, selfSignedMetaFile)
}

// ensureSelfSigned 尽量复用磁盘上的自签证书，复用不了就重签。
//
// 复用很关键：重签会换密钥，而 SPKI 指纹一旦变化，所有按指纹固定的
// 客户端（Agent、监控探针）都要重新装机。所以只要参数没变、
// SAN 还覆盖当前 hosts、剩余寿命还够，就绝不重签。
func (p *Provider) ensureSelfSigned() error {
	if pair, ok := p.loadSelfSigned(); ok {
		p.installCert(pair, renewAt(pair.Leaf, time.Now()))
		return nil
	}
	return p.regenerateSelfSigned()
}

func (p *Provider) loadSelfSigned() (*tls.Certificate, bool) {
	certPath, keyPath, metaPath := p.selfSignedPaths()
	var meta selfSignedMeta
	if err := readJSON(metaPath, &meta); err != nil {
		return nil, false
	}
	if !hostsEqual(meta.Hosts, p.ssHosts) || meta.ValidDays != p.ssValidDays {
		return nil, false
	}
	pair, _, _, err := loadKeyPair(certPath, keyPath)
	if err != nil || pair.Leaf == nil {
		return nil, false
	}
	now := time.Now()
	if !now.Before(renewAt(pair.Leaf, now)) {
		return nil, false
	}
	if !certCoversAll(pair.Leaf, p.ssHosts) {
		return nil, false
	}
	return pair, true
}

func (p *Provider) regenerateSelfSigned() error {
	certPEM, keyPEM, err := generateSelfSigned(p.ssHosts, p.ssValidDays, time.Now())
	if err != nil {
		return err
	}
	certPath, keyPath, metaPath := p.selfSignedPaths()
	if err := writeFile(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("写入自签证书失败: %w", err)
	}
	if err := writeFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("写入自签私钥失败: %w", err)
	}
	if err := writeJSON(metaPath, selfSignedMeta{Hosts: p.ssHosts, ValidDays: p.ssValidDays, CreatedAt: time.Now().Unix()}); err != nil {
		p.log.Warn("自签证书元信息落盘失败（下次启动会重签）", "err", err.Error())
	}
	pair, err := pairFromPEM(certPEM, keyPEM)
	if err != nil {
		return err
	}
	p.installCert(pair, renewAt(pair.Leaf, time.Now()))
	p.log.Info("已生成自签证书", "sans", strings.Join(summarize(pair.Leaf).SANs, ","),
		"not_after", pair.Leaf.NotAfter.Format(time.RFC3339))
	return nil
}

// ---------------------------------------------------------------------------
// acme
// ---------------------------------------------------------------------------

func (p *Provider) acmeCertPath() string { return filepath.Join(p.cacheDir, certFileName) }
func (p *Provider) acmeKeyPath() string  { return filepath.Join(p.cacheDir, keyFileName) }

func (p *Provider) loadCachedACMECert() (*tls.Certificate, error) {
	pair, _, _, err := loadKeyPair(p.acmeCertPath(), p.acmeKeyPath())
	if err != nil {
		return nil, err
	}
	if !certCoversAll(pair.Leaf, p.identifiersAsHosts()) {
		return nil, errors.New("缓存的证书未覆盖当前配置的标识符")
	}
	return pair, nil
}

func (p *Provider) identifiersAsHosts() []string {
	out := make([]string, 0, len(p.acmeDomains)+len(p.acmeIPs))
	out = append(out, p.acmeDomains...)
	out = append(out, p.acmeIPs...)
	return out
}

func (p *Provider) identifiers() []acme.Identifier {
	ids := make([]acme.Identifier, 0, len(p.acmeDomains)+len(p.acmeIPs))
	for _, d := range p.acmeDomains {
		ids = append(ids, acme.DNSIdentifier(d))
	}
	for _, ip := range p.acmeIPs {
		ids = append(ids, acme.IPIdentifier(ip))
	}
	return ids
}

// loadOrCreateCertKey 加载或生成证书私钥。
//
// **续期绝不换钥匙**：Agent 按 install.spki_sha256 做证书固定，
// 换钥匙等于把所有已装 Agent 一次性打死。因此私钥单独落盘、长期复用，
// 只有文件损坏无法解析时才重建（此时会明确告警需要重新装机）。
func (p *Provider) loadOrCreateCertKey() (crypto.Signer, error) {
	path := p.acmeKeyPath()
	if raw, err := os.ReadFile(path); err == nil {
		if blk, _ := pem.Decode(raw); blk != nil {
			if parsed, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
				if signer, ok := parsed.(crypto.Signer); ok {
					return signer, nil
				}
			}
		}
		p.log.Warn("已有的证书私钥无法解析，将重新生成（按 SPKI 指纹固定的客户端需要重新装机）", "path", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writeFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func (p *Provider) manageLoop(ctx context.Context) {
	p.tick(ctx)
	for {
		p.mu.RLock()
		haveCert := p.cert != nil
		next := p.nextRenewal
		p.mu.RUnlock()

		wait := renewCheckInterval
		if !haveCert {
			wait = retryWithoutCert
		} else if !next.IsZero() {
			if d := time.Until(next); d > 0 && d < wait {
				wait = d
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		p.tick(ctx)
	}
}

func (p *Provider) tick(ctx context.Context) {
	switch p.mode {
	case "acme":
		p.renewACME(ctx)
	case "selfsigned":
		p.renewSelfSignedIfNeeded()
	}
	// 每轮之后落一次状态：`xb-probe-master tls status` 与部署脚本读的就是它，
	// 放在 tick 的出口（而不是各分支里）能保证"无论走哪条分支，状态都是新的"。
	p.writeStatus()
}

// markAttempt 记录一次尝试的开始时间（供状态展示）。
func (p *Provider) markAttempt() {
	p.mu.Lock()
	p.lastAttempt = time.Now()
	p.mu.Unlock()
}

func (p *Provider) needsRenewal() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.cert == nil {
		return true
	}
	if p.nextRenewal.IsZero() {
		return true
	}
	return !time.Now().Before(p.nextRenewal)
}

func (p *Provider) renewSelfSignedIfNeeded() {
	if !p.needsRenewal() {
		return
	}
	if !p.renewMu.TryLock() {
		return
	}
	defer p.renewMu.Unlock()
	p.markAttempt()
	if err := p.regenerateSelfSigned(); err != nil {
		p.setErr(err)
		p.log.Error("刷新自签证书失败", "err", err.Error())
	}
}

func (p *Provider) renewACME(ctx context.Context) {
	if !p.needsRenewal() {
		return
	}
	if !p.renewMu.TryLock() {
		return
	}
	defer p.renewMu.Unlock()
	p.markAttempt()

	key, err := p.loadOrCreateCertKey()
	if err != nil {
		p.setErr(err)
		p.log.Error("加载证书私钥失败", "err", err.Error())
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, acmeObtainTimeout)
	defer cancel()

	certPEM, err := p.acmeClient.Obtain(reqCtx, p.identifiers(), key, p.challenge)
	if err != nil {
		p.setErr(err)
		p.log.Error("ACME 申请证书失败", "err", err.Error(), "identifiers", strings.Join(p.identifiersAsHosts(), ","))
		return
	}
	if err := writeFile(p.acmeCertPath(), certPEM, 0o644); err != nil {
		p.log.Warn("证书落盘失败（本次仍可用，重启后会重新申请）", "err", err.Error())
	}
	pair, err := keyPairFromPEM(certPEM, key)
	if err != nil {
		p.setErr(err)
		p.log.Error("解析签发结果失败", "err", err.Error())
		return
	}
	p.installCert(pair, renewAt(pair.Leaf, time.Now()))
	s := summarize(pair.Leaf)
	p.log.Info("ACME 证书已就绪", "subject", s.Subject, "issuer", s.Issuer,
		"not_after", pair.Leaf.NotAfter.Format(time.RFC3339), "sans", strings.Join(s.SANs, ","))
}

// obtainACMESync 同步申请一张证书并返回结果（失败时返回错误）。
//
// 与 renewACME 的关系：两者跑的是同一套申请流程，区别只在"错误往哪去"。
// renewACME 是后台循环的一部分，失败只能记进状态、等下一轮重试；这里被
// Setup 调用（部署脚本打开的 tls.acme.sync_obtain），失败要**变成返回错误**
// 让进程起不来，部署脚本才能据此回滚。
//
// 不直接复用 renewACME 的原因：它先判 needsRenewal()，而"还没有证书"时的
// 续期判断走的是另一条分支（nextRenewal 为零值），并且它把错误吞进日志。
// 硬套会得到一个"部署时永远申请不到证书"的静默失败——所以这里把流程
// 单独写一遍，但**共用** loadOrCreateCertKey / identifiers / installCert，
// 保证密钥复用与标识符计算不会出现两份实现。
func (p *Provider) obtainACMESync(ctx context.Context) error {
	if !p.renewMu.TryLock() {
		// 走到这里说明后台循环已经在申请了（Run 与 Setup 不会并发到这个点，
		// 但真发生时应当等它，而不是再发一次请求去撞 CA 的速率限制）。
		return fmt.Errorf("已有一次申请在进行中")
	}
	defer p.renewMu.Unlock()
	p.markAttempt()

	key, err := p.loadOrCreateCertKey()
	if err != nil {
		p.setErr(err)
		return fmt.Errorf("加载证书私钥失败: %w", err)
	}
	certPEM, err := p.acmeClient.Obtain(ctx, p.identifiers(), key, p.challenge)
	if err != nil {
		p.setErr(err)
		return err
	}
	if err := writeFile(p.acmeCertPath(), certPEM, 0o644); err != nil {
		// 这里**不**当成失败：证书已经拿到手、本次部署就能用 https，
		// 只是重启后需要重新申请。把它报成失败会让一次成功的部署被回滚。
		p.log.Warn("证书落盘失败（本次仍可用，重启后会重新申请）", "err", err.Error())
	}
	pair, err := keyPairFromPEM(certPEM, key)
	if err != nil {
		p.setErr(err)
		return fmt.Errorf("解析签发结果失败: %w", err)
	}
	p.installCert(pair, renewAt(pair.Leaf, time.Now()))
	s := summarize(pair.Leaf)
	p.log.Info("ACME 证书已就绪", "subject", s.Subject, "issuer", s.Issuer,
		"not_after", pair.Leaf.NotAfter.Format(time.RFC3339), "sans", strings.Join(s.SANs, ","))
	return nil
}

// ---------------------------------------------------------------------------
// 状态
// ---------------------------------------------------------------------------

func (p *Provider) installCert(cert *tls.Certificate, next time.Time) {
	p.mu.Lock()
	p.cert = cert
	p.nextRenewal = next
	p.lastErr = ""
	p.mu.Unlock()
}

func (p *Provider) setErr(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	p.lastErr = err.Error()
	p.mu.Unlock()
}
