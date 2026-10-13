package acme

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HTTP01Prefix 是 http-01 挑战的固定路径前缀（RFC 8555 §8.3）。
const HTTP01Prefix = "/.well-known/acme-challenge/"

// ALPNProto 是 tls-alpn-01 约定的 ALPN 协议名（RFC 8737 §4）。
const ALPNProto = "acme-tls/1"

// oidACMEIdentifier 即 id-pe-acmeIdentifier（RFC 8737 §3）。
var oidACMEIdentifier = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 31}

// ---------------------------------------------------------------------------
// http-01
// ---------------------------------------------------------------------------

// tokenStore 保存 http-01 的 token → keyAuthorization 映射。
//
// 只有正在验证的 token 才会出现在这里；验证结束立刻删除，
// 因此这个端点不会变成"对外暴露账户指纹"的常驻接口。
type tokenStore struct {
	mu sync.RWMutex
	m  map[string]string
}

func (s *tokenStore) set(token, keyAuth string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]string)
	}
	s.m[token] = keyAuth
}

func (s *tokenStore) get(token string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[token]
	return v, ok
}

func (s *tokenStore) clear(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, token)
}

// HTTP01Handler 是 http-01 挑战应答端点。
//
// 只处理 /.well-known/acme-challenge/<token>，其余一律 404——即使被挂到
// 公开站上也不会多暴露任何东西。它应当被挂到**所有**监听地址：
// 操作者既可能直接放行 80 端口，也可能让反代把 /.well-known/ 转发进来，
// 两条路都能命中同一个处理器。
func (c *Client) HTTP01Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		token, ok := strings.CutPrefix(r.URL.Path, HTTP01Prefix)
		if !ok || token == "" || strings.ContainsRune(token, '/') {
			http.NotFound(w, r)
			return
		}
		keyAuth, ok := c.tokens.get(token)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = io.WriteString(w, keyAuth)
	})
}

// ---------------------------------------------------------------------------
// tls-alpn-01
// ---------------------------------------------------------------------------

// alpnStore 保存"域名 → 挑战用自签证书"。
type alpnStore struct {
	mu sync.RWMutex
	m  map[string]*tls.Certificate
}

func (s *alpnStore) set(serverName string, cert *tls.Certificate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]*tls.Certificate)
	}
	s.m[normalizeServerName(serverName)] = cert
}

func (s *alpnStore) get(serverName string) (*tls.Certificate, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.m[normalizeServerName(serverName)]
	return c, ok
}

func (s *alpnStore) clear(serverName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, normalizeServerName(serverName))
}

// TLSALPNCertificate 取出某域名的 tls-alpn-01 挑战证书（供 GetCertificate 使用）。
// 第二个返回值为 false 表示当前没有针对该域名的挑战在进行。
func (c *Client) TLSALPNCertificate(serverName string) (*tls.Certificate, bool) {
	return c.alpn.get(serverName)
}

// normalizeServerName 统一大小写并去掉尾点，否则 "Example.com" 与
// "example.com." 会被当成两个域名、挑战证书取不到。
func normalizeServerName(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// tlsALPNCertificate 生成 tls-alpn-01 专用自签证书（RFC 8737 §3）：
//   - SAN 必须**精确**等于被验证的标识符（域名写 dnsNames，IP 写 iPAddresses）；
//   - 必须带一个 critical 的 id-pe-acmeIdentifier 扩展，
//     值为 DER(OCTET STRING(SHA-256(keyAuthorization)))。少一层 OCTET STRING
//     包装会被 Boulder 判为无效——这是该扩展最容易写错的地方。
func tlsALPNCertificate(identifier, keyAuth string) (*tls.Certificate, error) {
	key, err := newP256Key()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(keyAuth))
	extValue, err := asn1.Marshal(sum[:])
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: identifier},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		ExtraExtensions: []pkix.Extension{{
			Id:       oidACMEIdentifier,
			Critical: true,
			Value:    extValue,
		}},
	}
	if ip := net.ParseIP(identifier); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{identifier}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
