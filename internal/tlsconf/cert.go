package tlsconf

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 本文件放"证书的读写与描述"这类纯函数，与 Provider 的调度逻辑分开，
// 便于单测直接针对它们断言（不需要起服务、不需要网络）。

// loadKeyPair 从磁盘读取证书链与私钥。
func loadKeyPair(certFile, keyFile string) (*tls.Certificate, []byte, []byte, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("读取证书 %s 失败: %w", certFile, err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("读取私钥 %s 失败: %w", keyFile, err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("证书与私钥不匹配（%s / %s）: %w", certFile, keyFile, err)
	}
	pair.Leaf = leafOf(&pair)
	if pair.Leaf == nil {
		return nil, nil, nil, fmt.Errorf("证书 %s 不含可解析的叶证书", certFile)
	}
	return &pair, certPEM, keyPEM, nil
}

// pairFromPEM 由两段 PEM（证书链 + 私钥）构造 tls.Certificate。
func pairFromPEM(certPEM, keyPEM []byte) (*tls.Certificate, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("证书与私钥不匹配: %w", err)
	}
	pair.Leaf = leafOf(&pair)
	if pair.Leaf == nil {
		return nil, errors.New("证书链不含可解析的叶证书")
	}
	return &pair, nil
}

// keyPairFromPEM 由 PEM 证书链 + 私钥对象构造 tls.Certificate。
func keyPairFromPEM(certPEM []byte, key crypto.Signer) (*tls.Certificate, error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return pairFromPEM(certPEM, keyPEM)
}

// leafOf 返回证书链中的叶证书（第一张，也是 ACME 与自签的唯一一张）。
// 解析失败返回 nil，由调用方决定是报错还是继续。
func leafOf(c *tls.Certificate) *x509.Certificate {
	if c == nil || len(c.Certificate) == 0 {
		return nil
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil
	}
	return leaf
}

// certSummary 是给后台/日志看的证书摘要。
type certSummary struct {
	Subject string
	Issuer  string
	SANs    []string
}

func summarize(leaf *x509.Certificate) certSummary {
	if leaf == nil {
		return certSummary{}
	}
	sans := make([]string, 0, len(leaf.DNSNames)+len(leaf.IPAddresses))
	sans = append(sans, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		sans = append(sans, ip.String())
	}
	return certSummary{
		Subject: leaf.Subject.String(),
		Issuer:  leaf.Issuer.String(),
		SANs:    sans,
	}
}

// spkiFingerprint 计算证书公钥（SubjectPublicKeyInfo）的 SHA-256。
//
// 它等于安装命令里的 install.spki_sha256——Agent 按它做证书固定，
// 因此**续期时必须复用同一把密钥**，换钥匙等于让所有已装 Agent 掉线。
func spkiFingerprint(leaf *x509.Certificate) (b64 string, hexColon string) {
	if leaf == nil {
		return "", ""
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	b64 = base64.StdEncoding.EncodeToString(sum[:])
	parts := make([]string, 0, len(sum))
	for _, b := range sum {
		parts = append(parts, strings.ToUpper(fmt.Sprintf("%02x", b)))
	}
	return b64, strings.Join(parts, ":")
}

// renewAt 计算"该在什么时候续期"。
//
// 规则：剩余寿命不足总寿命的 1/3 时续期。90 天的 Let's Encrypt 证书
// → 剩 30 天时续；6 天的 IP 证书（RFC 8738 短周期）→ 剩 2 天时续。
// 用比例而不是固定天数，才能同时适配这两种寿命差 15 倍的证书。
func renewAt(leaf *x509.Certificate, now time.Time) time.Time {
	if leaf == nil {
		return time.Time{}
	}
	lifetime := leaf.NotAfter.Sub(leaf.NotBefore)
	if lifetime <= 0 {
		return leaf.NotAfter
	}
	return leaf.NotAfter.Add(-lifetime / 3)
}

// certCoversAll 判断证书 SAN 是否覆盖给定的全部条目（IP 与域名都认）。
func certCoversAll(leaf *x509.Certificate, hosts []string) bool {
	if leaf == nil {
		return false
	}
	for _, h := range hosts {
		if net.ParseIP(h) != nil {
			if leaf.VerifyHostname(h) != nil {
				return false
			}
			continue
		}
		if leaf.VerifyHostname(strings.ToLower(h)) != nil {
			return false
		}
	}
	return true
}

// hostsEqual 比较两组主机名条目（与顺序无关，大小写不敏感）。
// 用来判断"配置改了没"——改了就必须重签自签证书，否则 SAN 还是旧的。
func hostsEqual(a, b []string) bool {
	na := normalizeSorted(a)
	nb := normalizeSorted(b)
	if len(na) != len(nb) {
		return false
	}
	for i := range na {
		if na[i] != nb[i] {
			return false
		}
	}
	return true
}

func normalizeSorted(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		if v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// generateSelfSigned 生成一张自签的服务器证书（ECDSA P-256）。
//
// SAN 同时接受 IPv4、IPv6 与域名：这正是"IP 证书"的本地可用形态——
// 公网 CA 不会给 RFC1918 私有地址签发证书，而内网探针之间用 IP 访问
// 又确实需要一张带 IP SAN 的证书，配合 SPKI 指纹固定即可安全使用。
func generateSelfSigned(hosts []string, validDays int, now time.Time) (certPEM, keyPEM []byte, err error) {
	if len(hosts) == 0 {
		return nil, nil, errors.New("自签证书至少需要一个主机条目")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hosts[0]},
		// 回拨 1 小时：容器/虚机时钟常有几十秒漂移，NotBefore 取"现在"
		// 会让刚生成的证书在一部分客户端上判为"尚未生效"。
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(0, 0, validDays),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, strings.ToLower(h))
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// selfSignedMeta 记录自签证书是按哪套参数生成的。
// 少了它就无法判断"配置改了没"，只能每次启动重签 —— 而重签会换掉密钥，
// 让所有按指纹固定的客户端失效。
type selfSignedMeta struct {
	Hosts     []string `json:"hosts"`
	ValidDays int      `json:"valid_days"`
	CreatedAt int64    `json:"created_at"`
}

func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(raw, '\n'), 0o600)
}
