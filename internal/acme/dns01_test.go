package acme

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// memDNS 是内存版 DNSProvider：把记录存进 map，供"CA 视角"查询。
// 它同时记录调用顺序，用来断言"写完要删"。
type memDNS struct {
	mu      sync.Mutex
	records map[string][]string // 记录名 -> 值（同名可能有多条）
	present int
	removed int
	failOn  string // 非空时，Present 对包含该子串的记录名直接报错
}

func newMemDNS() *memDNS { return &memDNS{records: map[string][]string{}} }

func (m *memDNS) Present(_ context.Context, name, value string) (func(context.Context) error, error) {
	if m.failOn != "" && strings.Contains(name, m.failOn) {
		return nil, fmt.Errorf("测试注入的写入失败：%s", name)
	}
	m.mu.Lock()
	m.records[name] = append(m.records[name], value)
	m.present++
	m.mu.Unlock()
	return func(context.Context) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		vals := m.records[name]
		for i, v := range vals {
			if v == value {
				m.records[name] = append(vals[:i], vals[i+1:]...)
				break
			}
		}
		m.removed++
		return nil
	}, nil
}

// lookup 模拟权威 DNS：取第一条匹配的值（多条同名记录时 CA 会逐条试，
// 单条场景下这里足够）。
func (m *memDNS) lookup(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vals := m.records[name]
	if len(vals) == 0 {
		return "", false
	}
	return vals[len(vals)-1], true
}

// dnsValidator 从内存 DNS 取应答——等价于真实 CA 去查 _acme-challenge 的 TXT。
func dnsValidator(d *memDNS) func(string, string, string) (string, error) {
	return func(_, identifier, _ string) (string, error) {
		v, ok := d.lookup(challengeRecordName(identifier))
		if !ok {
			return "", fmt.Errorf("没有 %s 的 TXT 记录", challengeRecordName(identifier))
		}
		return v, nil
	}
}

func testClientWithDNS(t *testing.T, dir, directoryURL string, d DNSProvider) *Client {
	t.Helper()
	c, err := New(Config{
		DirectoryURL: directoryURL,
		CacheDir:     dir,
		Email:        "ops@example.com",
		PollInterval: 10 * time.Millisecond,
		DNSProvider:  d,
	})
	if err != nil {
		t.Fatalf("创建 ACME 客户端失败: %v", err)
	}
	return c
}

// TestObtainDNS01Wildcard 是最关键的一条：通配符 + 裸域名同时申请。
//
// 它同时守住两件事：① dns-01 记录值必须是 base64url(SHA-256(keyAuthorization))
// ——写错（比如直接写 keyAuthorization）桩 CA 会判 invalid；
// ② 记录名必须去掉 `*.`——写成 _acme-challenge.*.example.com 时桩 CA 查不到记录。
func TestObtainDNS01Wildcard(t *testing.T) {
	dns := newMemDNS()
	ca := newStubCA(t, func(ct, id, tok string) (string, error) {
		return dnsValidator(dns)(ct, id, tok)
	})
	client := testClientWithDNS(t, t.TempDir(), ca.directoryURL(), dns)

	ids := []Identifier{DNSIdentifier("*.example.com"), DNSIdentifier("example.com")}
	certPEM, err := client.Obtain(context.Background(), ids, testCertKey(t), ChallengeDNS01)
	if err != nil {
		t.Fatalf("dns-01 申请失败: %v", err)
	}
	leaf := parseLeaf(t, certPEM)
	if err := leaf.VerifyHostname("anything.example.com"); err != nil {
		t.Errorf("通配符证书未覆盖子域名: %v", err)
	}
	if err := leaf.VerifyHostname("example.com"); err != nil {
		t.Errorf("证书未覆盖裸域名: %v", err)
	}
	if dns.present == 0 {
		t.Error("从未写入任何 TXT 记录，dns-01 形同虚设")
	}
	if dns.removed != dns.present {
		t.Errorf("写入 %d 条记录只清理了 %d 条：挑战结束后 DNS 里会留下垃圾", dns.present, dns.removed)
	}
	for name, vals := range dns.records {
		if len(vals) != 0 {
			t.Errorf("%s 仍残留 %d 条记录", name, len(vals))
		}
	}
}

// TestDNS01RecordNameAndValue：记录名与记录值的形态。
func TestDNS01RecordNameAndValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"example.com", "_acme-challenge.example.com"},
		{"*.example.com", "_acme-challenge.example.com"}, // 通配符必须去掉 `*.`
		{"STATUS.Example.COM.", "_acme-challenge.status.example.com"},
	}
	for _, c := range cases {
		if got := challengeRecordName(c.in); got != c.want {
			t.Errorf("challengeRecordName(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
	// 记录值 = base64url(SHA-256(keyAuthorization))，与 tls-alpn-01 同一个摘要。
	const keyAuth = "tok.thumbprint"
	sum := sha256.Sum256([]byte(keyAuth))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); dnsTXTValue(keyAuth) != want {
		t.Errorf("dnsTXTValue 与 RFC 8555 规定的摘要不一致：%q", dnsTXTValue(keyAuth))
	}
}

// TestDNS01WithoutProviderFailsBeforeOrder：没有 DNS 凭据时必须在**下单之前**失败。
//
// 若放行到挑战阶段才失败，会白占 CA 的速率配额，而且错误现场离真正的原因很远。
func TestDNS01WithoutProviderFailsBeforeOrder(t *testing.T) {
	dns := newMemDNS()
	var newOrders int
	ca := newStubCA(t, func(ct, id, tok string) (string, error) { return dnsValidator(dns)(ct, id, tok) })
	ca.countNewOrders = &newOrders
	client := testClientWithDNS(t, t.TempDir(), ca.directoryURL(), nil)

	_, err := client.Obtain(context.Background(), []Identifier{DNSIdentifier("a.example.com")}, testCertKey(t), ChallengeDNS01)
	if err == nil {
		t.Fatal("缺少 DNS 提供方却申请成功了")
	}
	if !strings.Contains(err.Error(), "dns_provider") && !strings.Contains(err.Error(), "DNS 提供方") {
		t.Errorf("错误信息没有指出缺的是 DNS 提供方：%v", err)
	}
	if newOrders != 0 {
		t.Errorf("已经向 CA 建了 %d 个订单——应在下单之前就失败", newOrders)
	}
}

// TestWildcardRejectsNonDNSChallenge：通配符配 http-01/tls-alpn-01 时，
// 错误信息必须点名"通配符只能用 dns-01"，否则用户会一直去查 80 端口。
func TestWildcardRejectsNonDNSChallenge(t *testing.T) {
	dns := newMemDNS()
	ca := newStubCA(t, func(ct, id, tok string) (string, error) { return dnsValidator(dns)(ct, id, tok) })
	client := testClientWithDNS(t, t.TempDir(), ca.directoryURL(), dns)

	_, err := client.Obtain(context.Background(), []Identifier{DNSIdentifier("*.example.com")}, testCertKey(t), ChallengeHTTP01)
	if err == nil {
		t.Fatal("通配符配 http-01 竟然成功了")
	}
	if !strings.Contains(err.Error(), "dns-01") {
		t.Errorf("错误信息没有指出该用 dns-01：%v", err)
	}
}

// TestDNS01PresentFailureIsReported：DNS 写入失败要如实报出，且不留下半个订单的假成功。
func TestDNS01PresentFailureIsReported(t *testing.T) {
	dns := newMemDNS()
	dns.failOn = "_acme-challenge"
	ca := newStubCA(t, func(ct, id, tok string) (string, error) { return dnsValidator(dns)(ct, id, tok) })
	client := testClientWithDNS(t, t.TempDir(), ca.directoryURL(), dns)

	_, err := client.Obtain(context.Background(), []Identifier{DNSIdentifier("a.example.com")}, testCertKey(t), ChallengeDNS01)
	if err == nil {
		t.Fatal("DNS 写入失败却申请成功了")
	}
	if !strings.Contains(err.Error(), "写入 dns-01 挑战记录失败") {
		t.Errorf("错误信息没有指出是写入 DNS 记录这一步失败了：%v", err)
	}
}

func TestIsSupportedChallenge(t *testing.T) {
	for _, v := range []string{ChallengeHTTP01, ChallengeTLSALPN01, ChallengeDNS01} {
		if !IsSupportedChallenge(v) {
			t.Errorf("%s 应当被支持", v)
		}
	}
	for _, v := range []string{"", "dns02", "http01", "DNS-01"} {
		if IsSupportedChallenge(v) {
			t.Errorf("%q 不应被支持", v)
		}
	}
}

// ---------------------------------------------------------------------------
// Cloudflare 提供方
// ---------------------------------------------------------------------------

// cfStub 是一个最小的 Cloudflare v4 桩服务：只实现 zone 查询、
// 记录创建与删除，并校验 Authorization 头。
type cfStub struct {
	srv    *httptest.Server
	mu     sync.Mutex
	zones  []string
	madeAt []string // 已创建记录名
	token  string
	denied bool // 为 true 时所有请求都返回 403（模拟权限不足）
}

func newCFStub(t *testing.T, zones ...string) *cfStub {
	t.Helper()
	s := &cfStub{zones: zones, token: "test-token"}
	mux := http.NewServeMux()
	mux.HandleFunc("/zones", func(w http.ResponseWriter, r *http.Request) {
		if s.authBad(w, r) {
			return
		}
		name := r.URL.Query().Get("name")
		result := []map[string]any{}
		for _, z := range s.zones {
			if z == name {
				result = append(result, map[string]any{"id": "zone-" + z, "name": z})
			}
		}
		writeCF(w, map[string]any{"success": true, "result": result})
	})
	mux.HandleFunc("/zones/", func(w http.ResponseWriter, r *http.Request) {
		if s.authBad(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dns_records"):
			var body struct {
				Type    string `json:"type"`
				Name    string `json:"name"`
				Content string `json:"content"`
				TTL     int    `json:"ttl"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Type != "TXT" || body.Name == "" || body.Content == "" {
				writeCF(w, map[string]any{"success": false, "errors": []map[string]any{
					{"code": 9003, "message": "字段不合法"},
				}})
				return
			}
			s.mu.Lock()
			s.madeAt = append(s.madeAt, body.Name)
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			writeCF(w, map[string]any{"success": true, "result": map[string]any{"id": "rec-1", "name": body.Name}})
		case r.Method == http.MethodDelete:
			s.mu.Lock()
			if n := len(s.madeAt); n > 0 {
				s.madeAt = s.madeAt[:n-1]
			}
			s.mu.Unlock()
			writeCF(w, map[string]any{"success": true, "result": map[string]any{"id": "rec-1"}})
		default:
			writeCF(w, map[string]any{"success": false, "errors": []map[string]any{{"code": 7003, "message": "no route"}}})
		}
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *cfStub) authBad(w http.ResponseWriter, r *http.Request) bool {
	if s.denied {
		w.WriteHeader(http.StatusForbidden)
		writeCF(w, map[string]any{"success": false, "errors": []map[string]any{
			{"code": 10000, "message": "Authentication error"},
		}})
		return true
	}
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		w.WriteHeader(http.StatusUnauthorized)
		writeCF(w, map[string]any{"success": false, "errors": []map[string]any{
			{"code": 10000, "message": "Invalid API Token"},
		}})
		return true
	}
	return false
}

func writeCF(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestCloudflareZonedRecordLifecycle(t *testing.T) {
	stub := newCFStub(t, "example.com")
	cf, err := NewCloudflare(CloudflareConfig{
		APIToken:   "test-token",
		BaseURL:    stub.srv.URL,
		Settle:     10 * time.Millisecond, // 测试里不等真实的 5 秒
		HTTPClient: stub.srv.Client(),
	})
	if err != nil {
		t.Fatalf("构造 Cloudflare 提供方失败: %v", err)
	}
	remove, err := cf.Present(context.Background(), "_acme-challenge.sub.example.com", "VALUE1")
	if err != nil {
		t.Fatalf("写入记录失败: %v", err)
	}
	stub.mu.Lock()
	made := append([]string{}, stub.madeAt...)
	stub.mu.Unlock()
	if len(made) != 1 || made[0] != "_acme-challenge.sub.example.com" {
		t.Fatalf("zone 发现结果不对：创建的记录是 %v", made)
	}
	if err := remove(context.Background()); err != nil {
		t.Fatalf("清理记录失败: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.madeAt) != 0 {
		t.Errorf("清理后记录仍在：%v", stub.madeAt)
	}
}

func TestCloudflareZoneNotFoundExplainsWhy(t *testing.T) {
	stub := newCFStub(t) // 一个 zone 都没有
	cf, _ := NewCloudflare(CloudflareConfig{
		APIToken: "test-token", BaseURL: stub.srv.URL,
		Settle: time.Millisecond, HTTPClient: stub.srv.Client(),
	})
	_, err := cf.Present(context.Background(), "_acme-challenge.example.com", "V")
	if err == nil {
		t.Fatal("zone 不存在却写成功了")
	}
	// 排查这类问题的人最需要知道的是"权限"和"域名归属"，只说"找不到"不够。
	for _, want := range []string{"zone", "Token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息缺少 %q 的指向：%v", want, err)
		}
	}
}

func TestCloudflareTokenRejectedMentionsPermissions(t *testing.T) {
	stub := newCFStub(t, "example.com")
	stub.denied = true
	cf, _ := NewCloudflare(CloudflareConfig{
		APIToken: "test-token", BaseURL: stub.srv.URL,
		Settle: time.Millisecond, HTTPClient: stub.srv.Client(),
	})
	_, err := cf.Present(context.Background(), "_acme-challenge.example.com", "V")
	if err == nil {
		t.Fatal("Token 被拒却写成功了")
	}
	if !strings.Contains(err.Error(), "Zone:DNS:Edit") {
		t.Errorf("Token 被拒时应提示所需权限：%v", err)
	}
}

func TestCloudflareRequiresToken(t *testing.T) {
	if _, err := NewCloudflare(CloudflareConfig{APIToken: "  "}); err == nil {
		t.Fatal("空 Token 应当直接报错，而不是等到请求时才失败")
	}
}
