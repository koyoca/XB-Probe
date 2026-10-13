package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// httpValidator 从客户端的 HTTP 挑战端点取应答——等价于真实 CA 从公网抓取
// /.well-known/acme-challenge/<token>。走的是同一个 Handler，因此能验证
// "客户端确实把 token 注册进去了"。
func httpValidator(c *Client) func(string, string, string) (string, error) {
	return func(_, _, token string) (string, error) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, HTTP01Prefix+token, nil)
		c.HTTP01Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			return "", fmt.Errorf("挑战端点在 token=%s 时返回 %d", token, rec.Code)
		}
		return rec.Body.String(), nil
	}
}

// alpnValidator 从挑战证书的 acmeIdentifier 扩展里解出摘要，
// 顺带校验 SAN 与 critical 标志——这三件事任一不对，真实 CA 都会判失败。
func alpnValidator(c *Client) func(string, string, string) (string, error) {
	return func(_, identifier, _ string) (string, error) {
		cert, ok := c.TLSALPNCertificate(identifier)
		if !ok {
			return "", fmt.Errorf("客户端未注册 %s 的 tls-alpn-01 挑战证书", identifier)
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return "", err
		}
		if err := leaf.VerifyHostname(identifier); err != nil {
			return "", fmt.Errorf("挑战证书 SAN 未覆盖 %s: %w", identifier, err)
		}
		for _, ext := range leaf.Extensions {
			if !ext.Id.Equal(oidACMEIdentifier) {
				continue
			}
			if !ext.Critical {
				return "", errors.New("acmeIdentifier 扩展必须是 critical")
			}
			var sum []byte
			if _, err := asn1.Unmarshal(ext.Value, &sum); err != nil {
				return "", fmt.Errorf("扩展值不是 OCTET STRING: %w", err)
			}
			if len(sum) != 32 {
				return "", fmt.Errorf("扩展里的摘要应为 32 字节，实际 %d", len(sum))
			}
			return base64.RawURLEncoding.EncodeToString(sum), nil
		}
		return "", errors.New("挑战证书缺少 acmeIdentifier 扩展")
	}
}

func testClient(t *testing.T, dir, directoryURL string) *Client {
	t.Helper()
	c, err := New(Config{
		DirectoryURL: directoryURL,
		CacheDir:     dir,
		Email:        "ops@example.com",
		PollInterval: 10 * time.Millisecond, // 测试里不必真等 2 秒
	})
	if err != nil {
		t.Fatalf("创建 ACME 客户端失败: %v", err)
	}
	return c
}

func testCertKey(t *testing.T) crypto.Signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成证书密钥失败: %v", err)
	}
	return k
}

func parseLeaf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		t.Fatalf("返回内容不是 PEM：%s", string(certPEM[:min(80, len(certPEM))]))
	}
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("解析叶证书失败: %v", err)
	}
	return leaf
}

// TestObtainHTTP01DomainAndIP 是最关键的一条：走完整下单流程，
// 且同时申请**域名与 IP** 两种标识符（IP 证书即 RFC 8738）。
func TestObtainHTTP01DomainAndIP(t *testing.T) {
	var client *Client
	ca := newStubCA(t, func(ct, id, tok string) (string, error) {
		return httpValidator(client)(ct, id, tok)
	})
	dir := t.TempDir()
	client = testClient(t, dir, ca.directoryURL())

	ids := []Identifier{DNSIdentifier("status.example.com"), IPIdentifier("203.0.113.10")}
	certPEM, err := client.Obtain(context.Background(), ids, testCertKey(t), ChallengeHTTP01)
	if err != nil {
		t.Fatalf("Obtain 失败: %v", err)
	}
	leaf := parseLeaf(t, certPEM)

	if err := leaf.VerifyHostname("status.example.com"); err != nil {
		t.Fatalf("证书未覆盖域名 status.example.com: %v", err)
	}
	if err := leaf.VerifyHostname("203.0.113.10"); err != nil {
		t.Fatalf("证书未覆盖 IP 203.0.113.10（IP 证书路径）: %v", err)
	}
	if leaf.Issuer.CommonName != "Stub ACME CA" {
		t.Fatalf("签发者应为桩 CA，得到 %q", leaf.Issuer.CommonName)
	}
	// 证书链必须是"叶 + 中间"，只返回叶证书会让部分客户端校验失败。
	if n := strings.Count(string(certPEM), "BEGIN CERTIFICATE"); n != 2 {
		t.Fatalf("应返回叶+CA 两段 PEM，实际 %d 段", n)
	}
	if ca.lastContact == nil || ca.lastContact[0] != "mailto:ops@example.com" {
		t.Fatalf("账户联系方式未带邮箱: %v", ca.lastContact)
	}
	// 账户 URL 必须落盘（否则每次重启都重新注册，很快撞上 CA 的账户配额）。
	if _, err := os.Stat(filepath.Join(dir, accountURLFile)); err != nil {
		t.Fatalf("账户 URL 未落盘: %v", err)
	}
	// http-01 的 token 用完必须撤销，不能留下常驻端点。
	if _, ok := client.tokens.get("tok-nothing"); ok {
		t.Fatal("不应存在未注册的 token")
	}
}

func TestObtainTLSALPN01(t *testing.T) {
	var client *Client
	ca := newStubCA(t, func(ct, id, tok string) (string, error) {
		return alpnValidator(client)(ct, id, tok)
	})
	client = testClient(t, t.TempDir(), ca.directoryURL())

	ids := []Identifier{DNSIdentifier("admin.example.com")}
	certPEM, err := client.Obtain(context.Background(), ids, testCertKey(t), ChallengeTLSALPN01)
	if err != nil {
		t.Fatalf("tls-alpn-01 下单失败: %v", err)
	}
	if err := parseLeaf(t, certPEM).VerifyHostname("admin.example.com"); err != nil {
		t.Fatalf("证书未覆盖域名: %v", err)
	}
	// 挑战结束后必须清理挑战证书，否则会一直把挑战证书留在库里。
	if _, ok := client.TLSALPNCertificate("admin.example.com"); ok {
		t.Fatal("挑战结束后应清理 tls-alpn-01 挑战证书")
	}
}

// TestAccountReusedAfterRestart：第二次启动必须复用同一账户（同一 kid），
// 否则重启一次就注册一个新账户，很快撞上 CA 的"每 IP 每日新账户数"限制。
func TestAccountReusedAfterRestart(t *testing.T) {
	// active 指向"当前正在运行的那个客户端"——等价于 CA 去抓真实在跑的服务。
	var active *Client
	ca := newStubCA(t, func(ct, id, tok string) (string, error) {
		return httpValidator(active)(ct, id, tok)
	})
	dir := t.TempDir()

	client := testClient(t, dir, ca.directoryURL())
	active = client
	if _, err := client.Obtain(context.Background(), []Identifier{DNSIdentifier("a.example.com")}, testCertKey(t), ChallengeHTTP01); err != nil {
		t.Fatalf("首次 Obtain 失败: %v", err)
	}
	first := client.AccountURL()
	if first == "" {
		t.Fatal("首次 Obtain 后应有账户 URL")
	}

	restarted := testClient(t, dir, ca.directoryURL())
	active = restarted
	if got := restarted.AccountURL(); got != first {
		t.Fatalf("重启后应复用账户 %q，实际 %q", first, got)
	}
	if _, err := restarted.Obtain(context.Background(), []Identifier{DNSIdentifier("a.example.com")}, testCertKey(t), ChallengeHTTP01); err != nil {
		t.Fatalf("复用账户下单失败: %v", err)
	}
	ca.mu.Lock()
	n := len(ca.accounts)
	ca.mu.Unlock()
	if n != 1 {
		t.Fatalf("全程只应注册 1 个账户，实际 %d 个", n)
	}
}

// TestBadNonceIsRetried：CA 回 badNonce 是常规抖动，客户端必须自动重试一次。
func TestBadNonceIsRetried(t *testing.T) {
	var client *Client
	ca := newStubCA(t, func(ct, id, tok string) (string, error) {
		return httpValidator(client)(ct, id, tok)
	})
	ca.badNonceOnce = true
	client = testClient(t, t.TempDir(), ca.directoryURL())

	if _, err := client.Obtain(context.Background(), []Identifier{DNSIdentifier("a.example.com")}, testCertKey(t), ChallengeHTTP01); err != nil {
		t.Fatalf("badNonce 应被自动重试并最终成功，实际失败: %v", err)
	}
}

// TestObtainSurfacesChallengeFailure：挑战不可达时必须报出"看得懂"的错，
// 而不是笼统的"下单失败"。运维排障全靠这条信息。
func TestObtainSurfacesChallengeFailure(t *testing.T) {
	ca := newStubCA(t, func(_, _, _ string) (string, error) {
		return "", errors.New("模拟 CA 抓不到应答")
	})
	client := testClient(t, t.TempDir(), ca.directoryURL())

	_, err := client.Obtain(context.Background(), []Identifier{DNSIdentifier("a.example.com")}, testCertKey(t), ChallengeHTTP01)
	if err == nil {
		t.Fatal("挑战不可达时应返回错误")
	}
	if !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("错误信息应包含“校验失败”，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "TCP 80") {
		t.Fatalf("错误信息应提示 http-01 的排查方向，实际: %v", err)
	}
}

func TestObtainInputValidation(t *testing.T) {
	ca := newStubCA(t, func(_, _, _ string) (string, error) { return "", nil })
	client := testClient(t, t.TempDir(), ca.directoryURL())

	if _, err := client.Obtain(context.Background(), nil, testCertKey(t), ChallengeHTTP01); err == nil {
		t.Fatal("未指定标识符时应报错")
	}
	// dns-01 现在是受支持的挑战类型，但**没有配置 DNS 提供方**时必须报错
	// （而不是被当成"不支持的挑战类型"糊过去——两者的排查方向完全不同）。
	_, err := client.Obtain(context.Background(), []Identifier{DNSIdentifier("a.example.com")}, testCertKey(t), "dns-01")
	if err == nil || !strings.Contains(err.Error(), "DNS 提供方") {
		t.Fatalf("未配置 DNS 提供方时应明确报错，实际: %v", err)
	}
	// 真正不认识的类型仍然要拒绝。
	_, err = client.Obtain(context.Background(), []Identifier{DNSIdentifier("a.example.com")}, testCertKey(t), "dns-02")
	if err == nil || !strings.Contains(err.Error(), "不支持的挑战类型") {
		t.Fatalf("未知挑战类型应被明确拒绝，实际: %v", err)
	}
}

// TestOrderAlreadyValidSkipsFinalize 不在范围（需要 CA 复用授权），
// 但至少要保证"重复 Obtain 同一个域名"不会报错。
func TestObtainTwiceSucceeds(t *testing.T) {
	var client *Client
	ca := newStubCA(t, func(ct, id, tok string) (string, error) {
		return httpValidator(client)(ct, id, tok)
	})
	client = testClient(t, t.TempDir(), ca.directoryURL())
	key := testCertKey(t)
	for i := 0; i < 2; i++ {
		if _, err := client.Obtain(context.Background(), []Identifier{DNSIdentifier("a.example.com")}, key, ChallengeHTTP01); err != nil {
			t.Fatalf("第 %d 次 Obtain 失败: %v", i+1, err)
		}
	}
}
