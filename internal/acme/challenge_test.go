package acme

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTP01HandlerServesOnlyRegisteredTokens(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	h := c.HTTP01Handler()
	c.tokens.set("known-token", "known-token."+c.Thumbprint())

	cases := []struct {
		name string
		path string
		want int
		body string
	}{
		{"已注册的 token", HTTP01Prefix + "known-token", http.StatusOK, "known-token." + c.Thumbprint()},
		{"未注册的 token", HTTP01Prefix + "other", http.StatusNotFound, ""},
		{"非挑战路径", "/index.html", http.StatusNotFound, ""},
		{"挑战前缀下的嵌套路径", HTTP01Prefix + "known-token/extra", http.StatusNotFound, ""},
		{"前缀本身（空 token）", HTTP01Prefix, http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != tc.want {
				t.Fatalf("%s 应返回 %d，实际 %d", tc.path, tc.want, rec.Code)
			}
			if tc.body != "" && rec.Body.String() != tc.body {
				t.Fatalf("应答内容错误：%q", rec.Body.String())
			}
		})
	}

	// 清理后必须立刻不可用：挑战应答不该常驻。
	c.tokens.clear("known-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, HTTP01Prefix+"known-token", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("撤销后应返回 404，实际 %d", rec.Code)
	}
}

func TestHTTP01HandlerRejectsNonGet(t *testing.T) {
	c, _ := New(Config{})
	c.tokens.set("tok", "tok.abc")
	rec := httptest.NewRecorder()
	c.HTTP01Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, HTTP01Prefix+"tok", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST 应返回 404，实际 %d", rec.Code)
	}
}

// TestTLSALPNCertificateShape 独立校验挑战证书的每一项硬性要求：
// SAN 精确匹配、扩展必须 critical、扩展值是 DER(OCTET STRING(SHA-256(keyAuth)))。
// 少一层 OCTET STRING 包装是最常见的错误，Boulder 会直接判无效。
func TestTLSALPNCertificateShape(t *testing.T) {
	const (
		name    = "probe.example.com"
		keyAuth = "some-token.some-thumbprint"
	)
	cert, err := tlsALPNCertificate(name, keyAuth)
	if err != nil {
		t.Fatalf("生成挑战证书失败: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("解析挑战证书失败: %v", err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != name {
		t.Fatalf("SAN 必须精确等于被验证的域名，实际 %v", leaf.DNSNames)
	}
	if len(leaf.IPAddresses) != 0 {
		t.Fatalf("域名场景不应带 IP SAN，实际 %v", leaf.IPAddresses)
	}

	var found bool
	for _, ext := range leaf.Extensions {
		if !ext.Id.Equal(oidACMEIdentifier) {
			continue
		}
		found = true
		if !ext.Critical {
			t.Fatal("id-pe-acmeIdentifier 必须是 critical")
		}
		var sum []byte
		rest, err := asn1.Unmarshal(ext.Value, &sum)
		if err != nil {
			t.Fatalf("扩展值不是 DER OCTET STRING: %v", err)
		}
		if len(rest) != 0 {
			t.Fatalf("扩展值尾部有多余字节: %d", len(rest))
		}
		want := sha256.Sum256([]byte(keyAuth))
		if string(sum) != string(want[:]) {
			t.Fatalf("扩展里的摘要不是 SHA-256(keyAuthorization)")
		}
	}
	if !found {
		t.Fatal("挑战证书缺少 id-pe-acmeIdentifier 扩展")
	}
}

// TestTLSALPNCertificateForIP：被验证的是 IP 时必须写进 IPAddresses，
// 写成 DNSNames 会被 CA 判为"证书未覆盖该标识符"。
func TestTLSALPNCertificateForIP(t *testing.T) {
	cert, err := tlsALPNCertificate("203.0.113.7", "t.t")
	if err != nil {
		t.Fatalf("生成挑战证书失败: %v", err)
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "203.0.113.7" {
		t.Fatalf("IP 标识符应写进 IPAddresses，实际 %v / %v", leaf.IPAddresses, leaf.DNSNames)
	}
	if len(leaf.DNSNames) != 0 {
		t.Fatalf("IP 场景不应带 DNS SAN，实际 %v", leaf.DNSNames)
	}
}

func TestALPNStoreNormalizesServerName(t *testing.T) {
	var s alpnStore
	s.set("Example.COM.", nil)
	if _, ok := s.get("example.com"); !ok {
		t.Fatal("大小写与尾点应被归一化后再查找")
	}
	s.clear("EXAMPLE.com")
	if _, ok := s.get("example.com"); ok {
		t.Fatal("clear 也应走同一套归一化")
	}
}

func TestNormalizeServerName(t *testing.T) {
	cases := map[string]string{
		"Example.COM":   "example.com",
		"example.com.":  "example.com",
		" example.com ": "example.com",
		"":              "",
	}
	for in, want := range cases {
		if got := normalizeServerName(in); got != want {
			t.Fatalf("normalizeServerName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestReachabilityHintMentionsRightPort(t *testing.T) {
	if !strings.Contains(reachabilityHint(ChallengeHTTP01), "TCP 80") {
		t.Fatal("http-01 的排查提示应指向 TCP 80")
	}
	if !strings.Contains(reachabilityHint(ChallengeTLSALPN01), "443") {
		t.Fatal("tls-alpn-01 的排查提示应指向 TCP 443")
	}
}
