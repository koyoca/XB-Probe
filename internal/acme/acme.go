package acme

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var base64URL = base64.RawURLEncoding

// 挑战类型（RFC 8555 §8）。
const (
	ChallengeHTTP01    = "http-01"
	ChallengeTLSALPN01 = "tls-alpn-01"
	// ChallengeDNS01 不占用本机任何端口，且是通配符证书唯一可用的方式；
	// 代价是需要域名 DNS 的写权限（见 dns01.go 的 DNSProvider）。
	ChallengeDNS01 = "dns-01"
)

// IsSupportedChallenge 判断挑战类型是否被本客户端支持。
// 集中一处是为了让配置校验与运行期判断不会各写一份而漂移。
func IsSupportedChallenge(v string) bool {
	switch v {
	case ChallengeHTTP01, ChallengeTLSALPN01, ChallengeDNS01:
		return true
	}
	return false
}

// ACME 目录地址。Let's Encrypt 生产/预发是绝大多数人的选择；
// 预发环境有不被信任的证书但**没有速率限制**，调试期务必先用它。
const (
	LetsEncrypt        = "https://acme-v02.api.letsencrypt.org/directory"
	LetsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"

	// ZeroSSL 需要外部账户绑定（EAB：kid + HMAC 密钥），本客户端未实现，
	// 留作常量仅为让配置校验能给出明确提示。真要接请用自建 CA（step-ca 等）
	// 并通过 ResolveDirectory 传入完整目录 URL。
	ZeroSSL = "https://acme.zerossl.com/v2/DV90"
)

// ResolveDirectory 把配置里的人读取值映射为目录 URL。
// 以 http(s):// 开头时原样返回（自建 CA、测试目录都走这条）。
func ResolveDirectory(ca string) string {
	switch strings.ToLower(strings.TrimSpace(ca)) {
	case "", "letsencrypt", "le", "letsencrypt-production":
		return LetsEncrypt
	case "letsencrypt-staging", "le-staging", "staging":
		return LetsEncryptStaging
	case "zerossl":
		return ZeroSSL
	}
	ca = strings.TrimSpace(ca)
	if strings.HasPrefix(ca, "https://") || strings.HasPrefix(ca, "http://") {
		return ca
	}
	return LetsEncrypt
}

// Identifier 是订单里的标识符（RFC 8555 §7.1.3 与 RFC 8738）。
type Identifier struct {
	Type  string `json:"type"` // dns | ip
	Value string `json:"value"`
}

// DNSIdentifier 构造域名标识符。
func DNSIdentifier(v string) Identifier {
	return Identifier{Type: "dns", Value: strings.ToLower(strings.TrimSpace(v))}
}

// IPIdentifier 构造 IP 标识符（RFC 8738）。证书类型是"IP 证书"。
func IPIdentifier(v string) Identifier {
	return Identifier{Type: "ip", Value: strings.TrimSpace(v)}
}

// IdentifierFor 按取值自动判定类型：能 ParseIP 的当 ip，否则当 dns。
func IdentifierFor(v string) Identifier {
	v = strings.TrimSpace(v)
	if ip := net.ParseIP(v); ip != nil {
		return IPIdentifier(ip.String())
	}
	return DNSIdentifier(v)
}

// Problem 是 RFC 7807 风格的 ACME 错误对象。
type Problem struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
	Status int    `json:"status"`
}

func (p *Problem) Error() string {
	if p.Detail != "" {
		return p.Type + ": " + p.Detail
	}
	if p.Type != "" {
		return p.Type
	}
	return fmt.Sprintf("HTTP %d", p.Status)
}

// IsBadNonce 判断错误是否为"nonce 失效"——这类错误重试一次即可，
// 是 ACME 的常规现象（服务端 nonce 池轮换、多实例部署）。
func IsBadNonce(err error) bool {
	var p *Problem
	if errors.As(err, &p) {
		return p.Type == "urn:ietf:params:acme:error:badNonce"
	}
	return false
}

// IsAccountNotFound 判断错误是否为"账户不存在"（缓存里的 kid 失效，
// 例如服务端数据被清空）。此时应丢弃缓存的 kid 重新注册。
func IsAccountNotFound(err error) bool {
	var p *Problem
	if errors.As(err, &p) {
		return p.Type == "urn:ietf:params:acme:error:accountDoesNotExist"
	}
	return false
}

// directory 是 ACME 目录（RFC 8555 §7.1.1）。
type directory struct {
	NewNonce   string `json:"newNonce"`
	NewAccount string `json:"newAccount"`
	NewOrder   string `json:"newOrder"`
	RevokeCert string `json:"revokeCert"`
	KeyChange  string `json:"keyChange"`
}

// order 是订单对象（RFC 8555 §7.1.3）。
type order struct {
	Status         string       `json:"status"`
	Expires        string       `json:"expires"`
	Identifiers    []Identifier `json:"identifiers"`
	Authorizations []string     `json:"authorizations"`
	Finalize       string       `json:"finalize"`
	Certificate    string       `json:"certificate"`
	Error          *Problem     `json:"error"`
}

// authorization 是授权对象（只取用得到的字段）。
type authorization struct {
	Status     string      `json:"status"`
	Identifier Identifier  `json:"identifier"`
	Wildcard   bool        `json:"wildcard"`
	Challenges []challenge `json:"challenges"`
}

type challenge struct {
	Type   string `json:"type"`
	URL    string `json:"url"`
	Token  string `json:"token"`
	Status string `json:"status"`
}

// Config 是客户端配置。
type Config struct {
	// DirectoryURL 是 ACME 目录地址；空则用 Let's Encrypt 生产目录。
	DirectoryURL string
	// Email 用于账户联系（可选；不填则注册一个无联系方式的账户）。
	Email string
	// CacheDir 存放账户密钥/账户 URL/已签发证书；空则不落盘（仅测试用）。
	CacheDir string
	// HTTPClient 可注入（测试用桩 ACME 服务器）；空则用内置超时配置。
	HTTPClient *http.Client
	// PollInterval 是等待授权/订单状态时的轮询间隔，0 表示默认 2 秒。
	// 服务端返回的 Retry-After 会覆盖它（夹到 [100ms, 10s]）。
	PollInterval time.Duration
	// DNSProvider 是 dns-01 用的记录写手。只有 challenge=dns-01 时必须提供；
	// 为空时选择 dns-01 会在下单前就报错，而不是跑到一半才失败。
	DNSProvider DNSProvider
	// Logger 可空。
	Logger *slog.Logger
}

// Client 是 ACME v2 客户端。
//
// 并发安全：内部状态（nonce 池、账户 URL、挑战注册表）都有锁保护，
// 但 Obtain 本身不应并发调用同一组标识符——续期循环已经做了串行化。
type Client struct {
	cfg Config

	dirURL string
	key    *ecdsa.PrivateKey
	jwk    jwkEC
	thumb  string
	hc     *http.Client
	log    *slog.Logger
	poll   time.Duration

	mu         sync.Mutex
	accountURL string
	nonces     []string
	dir        directory
	dirLoaded  bool

	tokens tokenStore
	alpn   alpnStore
}

// New 创建客户端：加载或生成账户密钥（CacheDir 为空时只在内存里）。
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.DirectoryURL) == "" {
		cfg.DirectoryURL = LetsEncrypt
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c := &Client{cfg: cfg, dirURL: cfg.DirectoryURL, hc: hc, log: log}
	c.poll = cfg.PollInterval
	if c.poll <= 0 {
		c.poll = 2 * time.Second
	}

	key, err := loadOrCreateAccountKey(cfg.CacheDir)
	if err != nil {
		return nil, err
	}
	c.key = key
	c.jwk = jwkFromPublic(&key.PublicKey)
	c.thumb = c.jwk.thumbprint()

	if cfg.CacheDir != "" {
		if url, err := loadAccountURL(cfg.CacheDir); err == nil {
			c.accountURL = url
		}
	}
	return c, nil
}

// Thumbprint 返回账户密钥的 JWK 指纹。
func (c *Client) Thumbprint() string { return c.thumb }

// AccountURL 返回已注册的账户 URL（未注册时为空串）。
func (c *Client) AccountURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accountURL
}

// DirectoryURL 返回实际使用的目录地址。
func (c *Client) DirectoryURL() string { return c.dirURL }

// ---------------------------------------------------------------------------
// HTTP 与 nonce
// ---------------------------------------------------------------------------

func (c *Client) fetchDirectory(ctx context.Context) (directory, error) {
	c.mu.Lock()
	if c.dirLoaded {
		d := c.dir
		c.mu.Unlock()
		return d, nil
	}
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.dirURL, nil)
	if err != nil {
		return directory{}, err
	}
	req.Header.Set("User-Agent", "probe-master-acme/1")
	resp, err := c.hc.Do(req)
	if err != nil {
		return directory{}, fmt.Errorf("获取 ACME 目录失败: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if err != nil {
		return directory{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return directory{}, fmt.Errorf("获取 ACME 目录失败: HTTP %d %s", resp.StatusCode, truncate(body, 200))
	}
	var d directory
	if err := json.Unmarshal(body, &d); err != nil {
		return directory{}, fmt.Errorf("解析 ACME 目录失败: %w", err)
	}
	if d.NewNonce == "" || d.NewAccount == "" || d.NewOrder == "" {
		return directory{}, errors.New("ACME 目录缺少 newNonce/newAccount/newOrder 字段")
	}
	c.mu.Lock()
	c.dir, c.dirLoaded = d, true
	c.mu.Unlock()
	return d, nil
}

// takeNonce 取出一个可用 nonce：优先复用上一个响应里带回来的，
// 否则向 newNonce 端点要一个。每次 POST 消耗一个 nonce。
func (c *Client) takeNonce(ctx context.Context) (string, error) {
	c.mu.Lock()
	if n := len(c.nonces); n > 0 {
		nonce := c.nonces[n-1]
		c.nonces = c.nonces[:n-1]
		c.mu.Unlock()
		return nonce, nil
	}
	c.mu.Unlock()

	d, err := c.fetchDirectory(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, d.NewNonce, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "probe-master-acme/1")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取 nonce 失败: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	nonce := resp.Header.Get("Replay-Nonce")
	if nonce == "" {
		return "", fmt.Errorf("获取 nonce 失败: HTTP %d 未返回 Replay-Nonce", resp.StatusCode)
	}
	return nonce, nil
}

// captureNonce 把响应里的 Replay-Nonce 存起来供下次使用。
func (c *Client) captureNonce(resp *http.Response) {
	if n := resp.Header.Get("Replay-Nonce"); n != "" {
		c.mu.Lock()
		if len(c.nonces) < 16 { // 有界，避免异常服务端把内存撑爆
			c.nonces = append(c.nonces, n)
		}
		c.mu.Unlock()
	}
}

// roundTrip 发出一次 JWS 签名的 POST。
//
// badNonce 是 ACME 的常规抖动（nonce 池轮换/多实例），重试一次即可；
// 其余错误直接返回 Problem 供上层判断。
func (c *Client) roundTrip(ctx context.Context, url string, payload []byte, kid string, useJWK bool) (*http.Response, []byte, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		nonce, err := c.takeNonce(ctx)
		if err != nil {
			return nil, nil, err
		}
		hdr := jwsHeader{Alg: "ES256", Nonce: nonce, URL: url}
		if useJWK {
			j := c.jwk
			hdr.JWK = &j
		} else {
			hdr.Kid = kid
		}
		signed, err := signJWS(c.key, hdr, payload)
		if err != nil {
			return nil, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(signed))
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Content-Type", "application/jose+json")
		req.Header.Set("User-Agent", "probe-master-acme/1")
		req.Header.Set("Accept-Language", "en")

		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("ACME 请求失败(%s): %w", url, err)
		}
		c.captureNonce(resp)
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, body, nil
		}
		prob := parseProblem(resp, body)
		if attempt == 0 && IsBadNonce(prob) {
			lastErr = prob
			c.log.Debug("ACME nonce 失效，重试一次", "url", url)
			continue
		}
		return resp, body, prob
	}
	return nil, nil, lastErr
}

// post 用账户 kid 发起 POST（payload 为 nil 即 POST-as-GET）。
func (c *Client) post(ctx context.Context, url string, payload []byte) (*http.Response, []byte, error) {
	c.mu.Lock()
	kid := c.accountURL
	c.mu.Unlock()
	if kid == "" {
		return nil, nil, errors.New("acme: 账户尚未注册")
	}
	resp, body, err := c.roundTrip(ctx, url, payload, kid, false)
	if err != nil && IsAccountNotFound(err) {
		// 缓存的 kid 已失效：清掉它，下次调用会重新注册。
		c.mu.Lock()
		c.accountURL = ""
		c.mu.Unlock()
		if c.cfg.CacheDir != "" {
			_ = saveAccountURL(c.cfg.CacheDir, "")
		}
	}
	return resp, body, err
}

func parseProblem(resp *http.Response, body []byte) *Problem {
	var p Problem
	if err := json.Unmarshal(body, &p); err == nil && p.Type != "" {
		if p.Status == 0 {
			p.Status = resp.StatusCode
		}
		return &p
	}
	return &Problem{Type: "http", Detail: truncate(body, 300), Status: resp.StatusCode}
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// 账户
// ---------------------------------------------------------------------------

func (c *Client) ensureAccount(ctx context.Context) error {
	if c.AccountURL() != "" {
		return nil
	}
	d, err := c.fetchDirectory(ctx)
	if err != nil {
		return err
	}
	payload := map[string]any{"termsOfServiceAgreed": true}
	if email := strings.TrimSpace(c.cfg.Email); email != "" {
		payload["contact"] = []string{"mailto:" + email}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, body, err := c.roundTrip(ctx, d.NewAccount, raw, "", true)
	if err != nil {
		return fmt.Errorf("注册 ACME 账户失败: %w", err)
	}
	kid := resp.Header.Get("Location")
	if kid == "" {
		// 极少数服务端把账户 URL 放在响应体里。
		var acc struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(body, &acc) == nil && acc.URL != "" {
			kid = acc.URL
		}
	}
	if kid == "" {
		return errors.New("注册 ACME 账户失败: 响应未带 Location（账户 URL）")
	}
	c.mu.Lock()
	c.accountURL = kid
	c.mu.Unlock()
	if c.cfg.CacheDir != "" {
		if err := saveAccountURL(c.cfg.CacheDir, kid); err != nil {
			c.log.Warn("账户 URL 落盘失败（下次启动会重新注册）", "err", err.Error())
		}
	}
	c.log.Info("ACME 账户已就绪", "account", kid, "directory", c.dirURL)
	return nil
}

// ---------------------------------------------------------------------------
// 下单
// ---------------------------------------------------------------------------

// Obtain 走完整下单流程并返回 PEM 证书链（叶证书在前，中间证书在后）。
//
// challengeType 取 ChallengeHTTP01 / ChallengeTLSALPN01 / ChallengeDNS01。
// certKey 是新证书的私钥（由调用方生成并持久化，绝不外传）。
func (c *Client) Obtain(ctx context.Context, ids []Identifier, certKey crypto.Signer, challengeType string) ([]byte, error) {
	if len(ids) == 0 {
		return nil, errors.New("acme: 未指定任何域名或 IP")
	}
	if challengeType == "" {
		challengeType = ChallengeHTTP01
	}
	if !IsSupportedChallenge(challengeType) {
		return nil, fmt.Errorf("acme: 不支持的挑战类型 %q（可选 http-01/tls-alpn-01/dns-01）", challengeType)
	}
	// 在**下单之前**就把"缺 DNS 凭据"这类问题拦下来：一旦建了订单又跑不完，
	// 这一单会占着 CA 的速率配额，而且失败原因要等到挑战阶段才出现，
	// 排查时容易被误读成"DNS 权限不对"。
	if challengeType == ChallengeDNS01 && c.cfg.DNSProvider == nil {
		return nil, errors.New("acme: 选择了 dns-01 但没有配置 DNS 提供方（tls.acme.dns_provider 与 dns_api_token）")
	}
	if err := c.ensureAccount(ctx); err != nil {
		return nil, err
	}
	o, orderURL, err := c.newOrder(ctx, ids)
	if err != nil {
		return nil, err
	}

	if o.Status == "" || o.Status == "pending" {
		for _, authzURL := range o.Authorizations {
			if err := c.solveAuthorization(ctx, authzURL, challengeType); err != nil {
				return nil, err
			}
		}
	}

	// 授权全部通过后订单转为 ready（若已有有效授权则可能直接是 ready/valid）。
	if o.Status != "valid" && o.Status != "ready" {
		o, err = c.waitOrder(ctx, orderURL, "ready", "valid")
		if err != nil {
			return nil, err
		}
	}

	if o.Status != "valid" {
		csr, err := createCSR(ids, certKey)
		if err != nil {
			return nil, err
		}
		payload, err := json.Marshal(map[string]string{"csr": b64(csr)})
		if err != nil {
			return nil, err
		}
		if _, _, err := c.post(ctx, o.Finalize, payload); err != nil {
			return nil, fmt.Errorf("提交 CSR 失败: %w", err)
		}
	}

	// 提交 CSR 后订单会走 processing → valid，这里等到 valid 才能取证书。
	o, err = c.waitOrder(ctx, orderURL, "valid")
	if err != nil {
		return nil, err
	}
	if o.Status != "valid" {
		if o.Error != nil {
			return nil, fmt.Errorf("订单未能签发: %w", o.Error)
		}
		return nil, fmt.Errorf("订单未能签发（状态 %s）", o.Status)
	}
	if o.Certificate == "" {
		return nil, errors.New("acme: 订单已有效但没有证书下载地址")
	}
	_, certPEM, err := c.post(ctx, o.Certificate, nil) // POST-as-GET
	if err != nil {
		return nil, fmt.Errorf("下载证书失败: %w", err)
	}
	if !bytes.Contains(certPEM, []byte("BEGIN CERTIFICATE")) {
		return nil, errors.New("acme: 下载到的内容不是 PEM 证书链")
	}
	return certPEM, nil
}

func (c *Client) newOrder(ctx context.Context, ids []Identifier) (*order, string, error) {
	d, err := c.fetchDirectory(ctx)
	if err != nil {
		return nil, "", err
	}
	payload, err := json.Marshal(map[string]any{"identifiers": ids})
	if err != nil {
		return nil, "", err
	}
	resp, body, err := c.post(ctx, d.NewOrder, payload)
	if err != nil {
		return nil, "", fmt.Errorf("创建订单失败: %w", err)
	}
	var o order
	if err := json.Unmarshal(body, &o); err != nil {
		return nil, "", fmt.Errorf("解析订单失败: %w", err)
	}
	url := resp.Header.Get("Location")
	if url == "" {
		return nil, "", errors.New("创建订单失败: 响应未带 Location（订单 URL）")
	}
	return &o, url, nil
}

func (c *Client) fetchOrder(ctx context.Context, url string) (*order, time.Duration, error) {
	resp, body, err := c.post(ctx, url, nil) // POST-as-GET
	if err != nil {
		return nil, 0, err
	}
	var o order
	if err := json.Unmarshal(body, &o); err != nil {
		return nil, 0, fmt.Errorf("解析订单失败: %w", err)
	}
	return &o, parseRetryAfter(resp), nil
}

// waitOrder 轮询订单直到进入 want 里的任一状态（或超时 / 变为 invalid）。
//
// 必须显式列出"可接受状态"而不是笼统地"等到非 pending"：
// ACME 订单有 pending → ready → processing → valid 四个正向状态，
// 授权全部通过后会停在 **ready**（等我们提交 CSR），
// 提交 CSR 之后才走 processing → valid。若只认 valid，第一步就会死等。
//
// ACME 服务端会用 Retry-After 建议轮询间隔，采纳它（夹到 [100ms,10s]）以免
// 把 CA 的速率限制撞满——Let's Encrypt 对"每小时每账户的订单数"是有配额的。
func (c *Client) waitOrder(ctx context.Context, url string, want ...string) (*order, error) {
	deadline := time.Now().Add(3 * time.Minute)
	wait := c.poll
	for {
		o, retry, err := c.fetchOrder(ctx, url)
		if err != nil {
			return nil, err
		}
		if o.Status == "invalid" {
			if o.Error != nil {
				return nil, fmt.Errorf("订单无效: %w", o.Error)
			}
			return nil, errors.New("订单无效")
		}
		for _, w := range want {
			if o.Status == w {
				return o, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等待订单状态超时（当前 %s，期望 %s）", o.Status, strings.Join(want, "/"))
		}
		if retry > 0 {
			wait = retry
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval(wait)):
		}
	}
}

// solveAuthorization 完成一条授权的挑战：注册应答 → 触发校验 → 轮询等待。
func (c *Client) solveAuthorization(ctx context.Context, authzURL, challengeType string) error {
	_, body, err := c.post(ctx, authzURL, nil) // POST-as-GET
	if err != nil {
		return fmt.Errorf("获取授权失败: %w", err)
	}
	var az authorization
	if err := json.Unmarshal(body, &az); err != nil {
		return fmt.Errorf("解析授权失败: %w", err)
	}
	if az.Status == "valid" {
		return nil // 复用的有效授权，无需再挑战
	}
	if az.Identifier.Type == "ip" && challengeType == ChallengeDNS01 {
		return fmt.Errorf("IP 标识符 %s 不能用 dns-01（DNS 里没有 IP 的反向记录可写）："+
			"请改回 http-01 或 tls-alpn-01", az.Identifier.Value)
	}
	if az.Wildcard && challengeType != ChallengeDNS01 {
		return fmt.Errorf("通配符域名 %s 只能用 dns-01 挑战（CA 无法为通配符指定具体的 HTTP 路径）："+
			"请在 tls.acme 里把 challenge 改成 dns-01 并配置 DNS 提供方，"+
			"或改成逐个申请具体的子域名", az.Identifier.Value)
	}
	var ch *challenge
	var available []string
	for i := range az.Challenges {
		available = append(available, az.Challenges[i].Type)
		if az.Challenges[i].Type == challengeType {
			ch = &az.Challenges[i]
		}
	}
	if ch == nil {
		return fmt.Errorf("CA 未提供 %s 挑战（可用：%s）", challengeType, strings.Join(available, ", "))
	}

	cleanup, err := c.present(ctx, az.Identifier.Value, ch.Token, challengeType)
	if err != nil {
		return err
	}
	defer cleanup()

	if _, _, err := c.post(ctx, ch.URL, []byte("{}")); err != nil {
		return fmt.Errorf("触发 %s 校验失败: %w", challengeType, err)
	}
	c.log.Info("已提交 ACME 挑战，等待校验", "type", challengeType, "identifier", az.Identifier.Value)

	deadline := time.Now().Add(2 * time.Minute)
	wait := c.poll
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("等待 %s 校验结果超时", az.Identifier.Value)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval(wait)):
		}
		resp, body, err := c.post(ctx, authzURL, nil)
		if err != nil {
			return err
		}
		if retry := parseRetryAfter(resp); retry > 0 {
			wait = retry
		}
		var cur authorization
		if err := json.Unmarshal(body, &cur); err != nil {
			return fmt.Errorf("解析授权失败: %w", err)
		}
		switch cur.Status {
		case "valid":
			return nil
		case "invalid":
			for _, cc := range cur.Challenges {
				if cc.Type == challengeType && cc.Status == "invalid" {
					return fmt.Errorf("%s 校验失败（%s）：%s",
						challengeType, cur.Identifier.Value, reachabilityHint(challengeType))
				}
			}
			return fmt.Errorf("授权 %s 校验失败", cur.Identifier.Value)
		}
	}
}

func reachabilityHint(challengeType string) string {
	switch challengeType {
	case ChallengeTLSALPN01:
		return "请确认 TCP 443 可达，且本进程返回的挑战证书带 acmeIdentifier 扩展（SNI 必须是被校验的域名）"
	case ChallengeDNS01:
		return "请确认 TXT 记录已写入并由权威 DNS 生效（_acme-challenge.<域名>），" +
			"且记录值等于 base64url(SHA-256(keyAuthorization))；" +
			"多数情况是 DNS API Token 的权限范围不含该域名，或记录还没传播完"
	}
	return "请确认 TCP 80 可达且 /.well-known/acme-challenge/ 应答内容与 account key 指纹匹配"
}

// present 把挑战应答登记到对应的应答通道，返回一个撤销函数。
//
// http-01：登记 token → keyAuthorization，由 HTTP01Handler 在
// /.well-known/acme-challenge/<token> 上吐出（验证期间必须可达）。
// tls-alpn-01：为域名生成一张带 acmeIdentifier 扩展的自签证书，
// 由调用方在 ALPN=acme-tls/1 的握手里返回。
// dns-01：把摘要写进 `_acme-challenge.<域名>` 的 TXT 记录，
// 校验完成后由返回的清理函数删掉。
func (c *Client) present(ctx context.Context, identifier, token, challengeType string) (func(), error) {
	keyAuth := keyAuthorization(token, c.thumb)
	switch challengeType {
	case ChallengeHTTP01:
		c.tokens.set(token, keyAuth)
		return func() { c.tokens.clear(token) }, nil
	case ChallengeTLSALPN01:
		cert, err := tlsALPNCertificate(identifier, keyAuth)
		if err != nil {
			return func() {}, err
		}
		c.alpn.set(identifier, cert)
		return func() { c.alpn.clear(identifier) }, nil
	case ChallengeDNS01:
		if c.cfg.DNSProvider == nil {
			return func() {}, errors.New("acme: 选择了 dns-01 但没有配置 DNS 提供方")
		}
		remove, err := c.cfg.DNSProvider.Present(ctx, challengeRecordName(identifier), dnsTXTValue(keyAuth))
		if err != nil {
			return func() {}, fmt.Errorf("写入 dns-01 挑战记录失败: %w", err)
		}
		// 清理用一个**独立**的上下文：present 的 ctx 属于"提交挑战"这一次调用，
		// 而清理发生在校验结束之后——那时它多半已经超时或被取消。
		// 用它去删记录，等于每次失败都会在 DNS 里留下一条垃圾 TXT。
		return func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = remove(cctx)
		}, nil
	}
	return func() {}, fmt.Errorf("acme: 不支持的挑战类型 %q", challengeType)
}

// pollInterval 把间隔夹到 [100ms, 10s]。
// 下限防"Retry-After: 0"打成忙等，上限防某个 CA 建议一个荒谬的长间隔把
// 首次签发的等待时间拖到不可接受。
func pollInterval(d time.Duration) time.Duration {
	if d < 100*time.Millisecond {
		return 100 * time.Millisecond
	}
	if d > 10*time.Second {
		return 10 * time.Second
	}
	return d
}

// createCSR 生成 PKCS#10 请求，SAN 覆盖全部标识符。
func createCSR(ids []Identifier, key crypto.Signer) ([]byte, error) {
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{},
	}
	for _, id := range ids {
		if id.Type == "ip" {
			ip := net.ParseIP(id.Value)
			if ip == nil {
				return nil, fmt.Errorf("acme: 非法的 IP 标识符 %q", id.Value)
			}
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, id.Value)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, fmt.Errorf("生成 CSR 失败: %w", err)
	}
	return der, nil
}

// parseRetryAfter 解析 Retry-After 头（秒数形式）。
func parseRetryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}
