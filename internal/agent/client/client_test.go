package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/probe/internal/model"
)

// Agent 与 Master 之间这条链路的测试。
//
// 重点不在"请求发出去了没有"，而在四件容易悄悄坏掉的事：
//   - 令牌只走 Authorization 头（不进 URL、不进日志、注册时不带）；
//   - 拒绝 http:// 明文（S7）与跨主机重定向（防令牌被转发）；
//   - 自签证书走 SPKI 固定，而不是"跳过校验"；
//   - 错误按 7.0 的 envelope 解析，401/403 能被上层识别成"不该自毁"。

// ---------------------------------------------------------------------------
// 地址与传输安全
// ---------------------------------------------------------------------------

// TestNewRejectsInsecureURLs 断言明文 http 默认被拒，且回环例外也要显式开启。
func TestNewRejectsInsecureURLs(t *testing.T) {
	cases := []struct {
		name string
		o    Options
		ok   bool
	}{
		{"https 默认允许", Options{MasterURL: "https://probe.example.com"}, true},
		{"http 默认拒绝（S7）", Options{MasterURL: "http://probe.example.com"}, false},
		{
			"开了开关但目标不是回环，仍拒绝",
			Options{MasterURL: "http://192.168.1.10:1828", InsecureHTTP: true}, false,
		},
		{"开了开关且是本机回环，放行", Options{MasterURL: "http://127.0.0.1:1828", InsecureHTTP: true}, true},
		{"localhost 也算回环", Options{MasterURL: "http://localhost:1828", InsecureHTTP: true}, true},
		{"其他协议一律拒绝", Options{MasterURL: "ftp://probe.example.com"}, false},
		{"地址无法解析", Options{MasterURL: "://bad"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl, err := New(c.o)
			if c.ok && err != nil {
				t.Fatalf("本应通过，却报错：%v", err)
			}
			if !c.ok {
				if err == nil {
					t.Fatal("本应被拒绝，却通过了")
				}
				if cl != nil {
					t.Error("失败时不应返回可用的 client")
				}
			}
		})
	}
}

// TestNewRejectsBadPin 断言指纹格式非法时直接失败，而不是"静默降级成不校验"。
//
// 这条是安全底线：pin 写错必须让 Agent 起不来，不能让它在无校验的情况下继续跑。
func TestNewRejectsBadPin(t *testing.T) {
	if _, err := New(Options{
		MasterURL:  "https://probe.example.com",
		SPKISHA256: []string{"!!!不是 base64!!!"},
	}); err == nil {
		t.Fatal("非法指纹应报错")
	}
}

// TestSPKIPinVerifiesCertificate 用真 TLS 服务器验证 SPKI 固定。
//
// 三分支都要覆盖：
//  1. 不配 pin → 自签证书必须被标准链校验拦下（证明我们没有偷偷 InsecureSkipVerify）；
//  2. pin 正确 → 放行；
//  3. pin 错误 → 拒绝，且错误里说明是指纹不匹配。
func TestSPKIPinVerifiesCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, model.ReportResponse{OK: true})
	}))
	defer srv.Close()

	// 1) 不配 pin：自签证书应被拒。
	if _, err := New(Options{MasterURL: srv.URL, Timeout: 3 * time.Second}); err != nil {
		t.Fatalf("创建失败: %v", err)
	} else {
		cl, _ := New(Options{MasterURL: srv.URL, Timeout: 3 * time.Second})
		if _, err := cl.Report(context.Background(), model.ReportRequest{}); err == nil {
			t.Fatal("自签证书在没有 pin 的情况下被接受了：默认链校验形同虚设")
		}
	}

	pin := spkiPin(t, srv)

	// 2) pin 正确。
	cl, err := New(Options{MasterURL: srv.URL, SPKISHA256: []string{pin}, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if _, err := cl.Report(context.Background(), model.ReportRequest{}); err != nil {
		t.Fatalf("正确的 SPKI 指纹应被接受，得到 %v", err)
	}

	// 2b) 轮换期：列表里同时有旧指纹（错的）与新指纹（对的），必须放行。
	//
	// 13.4.9 要求多指纹并存正是为了平滑轮换；如果只比较第一个，轮换会直接断线。
	// 这里的"旧指纹"必须是**合法 base64**，否则 New 会先在格式校验那一步失败，
	// 测不到"遍历所有指纹"这件事。
	bogusSum := sha256.Sum256([]byte("这不是任何一把真实的钥匙"))
	bogus := base64.StdEncoding.EncodeToString(bogusSum[:])

	cl2, err := New(Options{
		MasterURL:  srv.URL,
		SPKISHA256: []string{bogus, pin},
		Timeout:    3 * time.Second,
	})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if _, err := cl2.Report(context.Background(), model.ReportRequest{}); err != nil {
		t.Fatalf("多指纹列表里含正确指纹时应放行，得到 %v", err)
	}

	// 3) pin 全不对 → 拒绝。
	//
	// 注意这里**不能**用另一个 httptest 服务器的证书当"错误的指纹"：
	// httptest 所有实例共用同一张内部测试证书，指纹是一样的（实测踩过，
	// 于是"错误指纹被接受"的断言永远失败）。
	cl3, err := New(Options{
		MasterURL:  srv.URL,
		SPKISHA256: []string{bogus},
		Timeout:    3 * time.Second,
	})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	_, err = cl3.Report(context.Background(), model.ReportRequest{})
	if err == nil {
		t.Fatal("错误的指纹被接受了")
	}
	if !strings.Contains(err.Error(), "指纹") {
		t.Errorf("错误信息应说明是指纹不匹配，得到 %v", err)
	}
}

// TestRedirectToAnotherHostIsRefused 断言跨主机重定向被拒绝。
//
// 跟随重定向会把 Authorization 头带到另一台主机上——那是把令牌交出去。
func TestRedirectToAnotherHostIsRefused(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("请求被转发到了另一台主机（令牌泄漏）")
		writeJSON(t, w, model.ReportResponse{OK: true})
	}))
	defer target.Close()

	// 两个 httptest 服务器的 host:port 不同 → 属于跨主机。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()

	cl := newPlainClient(t, srv.URL)
	_, err := cl.Report(context.Background(), model.ReportRequest{})
	if err == nil {
		t.Fatal("跨主机重定向没有被拒绝")
	}
	if !strings.Contains(err.Error(), "重定向") {
		t.Errorf("错误信息应提到重定向，得到 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 令牌
// ---------------------------------------------------------------------------

// TestTokenTravelsOnlyInAuthorizationHeader 断言令牌的唯一出口是请求头。
//
// 三条要求：注册（auth=false）不带令牌；上报/拉配置带 Bearer；URL 里永远没有令牌。
func TestTokenTravelsOnlyInAuthorizationHeader(t *testing.T) {
	type got struct {
		path, auth, rawQuery, ua, accept, enc string
	}
	var seen []got
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, got{
			path:     r.URL.Path,
			auth:     r.Header.Get("Authorization"),
			rawQuery: r.URL.RawQuery,
			ua:       r.Header.Get("User-Agent"),
			accept:   r.Header.Get("Accept"),
			enc:      r.Header.Get("Accept-Encoding"),
		})
		switch r.URL.Path {
		case "/api/agent/v1/enroll":
			writeJSON(t, w, model.EnrollResponse{NodeID: 7, Token: "pk_new"})
		case "/api/agent/v1/config":
			writeJSON(t, w, model.AgentConfig{ConfigVersion: "v1"})
		default:
			writeJSON(t, w, model.ReportResponse{OK: true})
		}
	}))
	defer srv.Close()

	cl := newPlainClient(t, srv.URL)
	cl.SetToken("pk_secret_token")

	if _, err := cl.Enroll(context.Background(), model.EnrollRequest{Code: "abc"}); err != nil {
		t.Fatalf("Enroll 失败: %v", err)
	}
	if _, err := cl.Report(context.Background(), model.ReportRequest{}); err != nil {
		t.Fatalf("Report 失败: %v", err)
	}
	if _, _, err := cl.GetConfig(context.Background(), ""); err != nil {
		t.Fatalf("GetConfig 失败: %v", err)
	}

	if len(seen) != 3 {
		t.Fatalf("收到 %d 个请求，期望 3", len(seen))
	}
	if seen[0].auth != "" {
		t.Errorf("注册请求不应带令牌，得到 %q（此时还没有令牌，且注册是一次性凭证）", seen[0].auth)
	}
	for i, want := range []string{"/api/agent/v1/enroll", "/api/agent/v1/report", "/api/agent/v1/config"} {
		if seen[i].path != want {
			t.Errorf("第 %d 个请求路径 = %s，期望 %s", i+1, seen[i].path, want)
		}
		if seen[i].rawQuery != "" {
			t.Errorf("第 %d 个请求带了查询串 %q：令牌/参数不得进 URL", i+1, seen[i].rawQuery)
		}
		if seen[i].ua != "probe-agent" {
			t.Errorf("User-Agent = %q", seen[i].ua)
		}
		if seen[i].accept != "application/json" {
			t.Errorf("Accept = %q", seen[i].accept)
		}
		if seen[i].enc != "gzip" {
			t.Errorf("Accept-Encoding = %q", seen[i].enc)
		}
	}
	if seen[1].auth != "Bearer pk_secret_token" || seen[2].auth != "Bearer pk_secret_token" {
		t.Errorf("上报/拉配置必须带 Bearer 令牌，得到 %q / %q", seen[1].auth, seen[2].auth)
	}
}

// TestSetTokenReplacesOldValue 断言换令牌真的生效（重新注册的场景）。
func TestSetTokenReplacesOldValue(t *testing.T) {
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		writeJSON(t, w, model.ReportResponse{OK: true})
	}))
	defer srv.Close()

	cl := newPlainClient(t, srv.URL)
	cl.SetToken("pk_old")
	_, _ = cl.Report(context.Background(), model.ReportRequest{})
	cl.SetToken("pk_new")
	_, _ = cl.Report(context.Background(), model.ReportRequest{})

	want := []string{"Bearer pk_old", "Bearer pk_new"}
	for i := range want {
		if i >= len(auths) || auths[i] != want[i] {
			t.Fatalf("Authorization 序列 = %v，期望 %v", auths, want)
		}
	}
}

// TestEmptyTokenOmitsHeader 断言没有令牌时不发空 Bearer。
//
// "Bearer " 这种空令牌会被 Master 当成"有令牌但无效"，与"没带令牌"走不同分支；
// 而 Agent 在注册前确实没有令牌，必须走后者。
func TestEmptyTokenOmitsHeader(t *testing.T) {
	var auth string
	var has bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, has = r.Header["Authorization"]
		writeJSON(t, w, model.ReportResponse{OK: true})
	}))
	defer srv.Close()

	cl := newPlainClient(t, srv.URL) // 没有 SetToken
	if _, err := cl.Report(context.Background(), model.ReportRequest{}); err != nil {
		t.Fatalf("Report 失败: %v", err)
	}
	if has || auth != "" {
		t.Errorf("无令牌时不应发 Authorization 头，得到 %q", auth)
	}
}

// ---------------------------------------------------------------------------
// 各端点的解析
// ---------------------------------------------------------------------------

// TestReportParsesResponseAndBody 断言上报的请求体是合法 JSON、响应被正确解析。
func TestReportParsesResponseAndBody(t *testing.T) {
	var body map[string]any
	var ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctype = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeJSON(t, w, model.ReportResponse{OK: true, ServerTime: 1234, ConfigVersion: "abc123"})
	}))
	defer srv.Close()

	req := model.ReportRequest{
		AgentVersion: "1.2.3",
		Samples:      []model.Sample{{TS: 99}},
	}
	resp, err := newPlainClient(t, srv.URL).Report(context.Background(), req)
	if err != nil {
		t.Fatalf("Report 失败: %v", err)
	}
	if ctype != "application/json" {
		t.Errorf("Content-Type = %q", ctype)
	}
	if body["agent_version"] != "1.2.3" {
		t.Errorf("请求体未正确序列化：%v", body)
	}
	if resp.ConfigVersion != "abc123" || resp.ServerTime != 1234 || !resp.OK {
		t.Errorf("响应解析错误：%+v", resp)
	}
}

// TestGetConfigSendsQuotedETagAndHandles304 断言 ETag 按 HTTP 规范带引号发送，
// 且 304 被翻译成 notModified=true（而不是当成错误或空配置）。
func TestGetConfigSendsQuotedETagAndHandles304(t *testing.T) {
	var ifNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ifNoneMatch = r.Header.Get("If-None-Match")
		if ifNoneMatch != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeJSON(t, w, model.AgentConfig{ConfigVersion: "v9", ReportIntervalS: 3,
			Targets: []model.Target{{ID: 1, Host: "example.com", Port: 443, Method: "tcp"}}})
	}))
	defer srv.Close()

	cl := newPlainClient(t, srv.URL)

	cfg, notModified, err := cl.GetConfig(context.Background(), "")
	if err != nil {
		t.Fatalf("首次拉取失败: %v", err)
	}
	if notModified {
		t.Error("不带 ETag 时不该返回 notModified")
	}
	if cfg == nil || cfg.ConfigVersion != "v9" || len(cfg.Targets) != 1 {
		t.Fatalf("配置解析错误：%+v", cfg)
	}
	if ifNoneMatch != "" {
		t.Errorf("没有 ETag 时不应发 If-None-Match，得到 %q", ifNoneMatch)
	}

	cfg, notModified, err = cl.GetConfig(context.Background(), "v9")
	if err != nil {
		t.Fatalf("带 ETag 拉取失败: %v", err)
	}
	if !notModified {
		t.Error("304 应翻译成 notModified=true")
	}
	if cfg != nil {
		t.Error("304 时不应返回配置体")
	}
	// ETag 必须是带引号的形式：不加引号的话 Master 侧的比较会不一致，
	// 表现就是"每次都全量下发配置"。
	if ifNoneMatch != `"v9"` {
		t.Errorf("If-None-Match = %q，期望 %q", ifNoneMatch, `"v9"`)
	}
}

// TestGzipResponseIsDecoded 断言 Content-Encoding: gzip 的响应被解压。
//
// 客户端把 DisableCompression 设为 true（自己控制压缩），所以这里的解压路径
// 是唯一的解压路径——它坏了就是"所有上报响应都解析失败"。
func TestGzipResponseIsDecoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		enc := json.NewEncoder(zw)
		_ = enc.Encode(model.ReportResponse{OK: true, ConfigVersion: "gz1"})
		_ = zw.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	resp, err := newPlainClient(t, srv.URL).Report(context.Background(), model.ReportRequest{})
	if err != nil {
		t.Fatalf("解压后的响应应能解析，得到 %v", err)
	}
	if resp.ConfigVersion != "gz1" {
		t.Errorf("ConfigVersion = %q，期望 gz1", resp.ConfigVersion)
	}
}

// TestDeregisterUsesPostAndToleratesEmptyBody 断言注销用的方法/路径正确，
// 且空响应体不会导致解析错误（"尽力而为"的调用不能因为空 body 失败）。
func TestDeregisterUsesPostAndToleratesEmptyBody(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK) // 空 body
	}))
	defer srv.Close()

	if err := newPlainClient(t, srv.URL).Deregister(context.Background()); err != nil {
		t.Fatalf("Deregister 失败: %v", err)
	}
	if method != http.MethodPost || path != "/api/agent/v1/deregister" {
		t.Errorf("注销请求 = %s %s", method, path)
	}
}

// ---------------------------------------------------------------------------
// 错误处理
// ---------------------------------------------------------------------------

// TestCheckStatusParsesErrorEnvelope 断言错误响应按 7.0 的 envelope 解析，
// 拿不到 envelope 时退回状态码文本。
func TestCheckStatusParsesErrorEnvelope(t *testing.T) {
	cases := []struct {
		name    string
		code    int
		body    string
		wantErr bool
		wantC   string
		wantM   string
	}{
		{"200 视为成功", 200, `{}`, false, "", ""},
		{"204 视为成功", 204, ``, false, "", ""},
		{"401 带 envelope", 401, `{"error":{"code":"invalid_token","message":"令牌无效"}}`,
			true, "invalid_token", "令牌无效"},
		{"403 带 envelope", 403, `{"error":{"code":"node_archived","message":"已归档"}}`,
			true, "node_archived", "已归档"},
		{"503 无 envelope", 503, `<html>bad gateway</html>`, true, "unknown", "Service Unavailable"},
		{"500 空 body", 500, ``, true, "unknown", "Internal Server Error"},
		{"错误码为空也退回状态码文本", 500, `{"error":{"code":"","message":"x"}}`,
			true, "unknown", "Internal Server Error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkStatus(c.code, []byte(c.body))
			if !c.wantErr {
				if err != nil {
					t.Fatalf("不应报错，得到 %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("应报错")
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("错误类型应为 *APIError，得到 %T", err)
			}
			if apiErr.Status != c.code || apiErr.Code != c.wantC || apiErr.Message != c.wantM {
				t.Errorf("解析结果 %+v，期望 status=%d code=%q message=%q",
					apiErr, c.code, c.wantC, c.wantM)
			}
		})
	}
}

// TestAPIErrorUnauthorizedClassification 断言只有 401/403 被当作"令牌问题"。
//
// 这个判定直接决定 Agent 的行为（7.1）：401/403 要退避到 10 分钟且**不自毁**，
// 而 429/500 走普通指数退避。判错会让限流变成"停摆 10 分钟"或反之。
func TestAPIErrorUnauthorizedClassification(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{401, true},
		{403, true},
		{400, false},
		{404, false},
		{429, false},
		{500, false},
	}
	for _, c := range cases {
		e := &APIError{Status: c.status}
		if got := e.IsUnauthorized(); got != c.want {
			t.Errorf("status=%d IsUnauthorized()=%v，期望 %v", c.status, got, c.want)
		}
	}
	// 错误文案要带上状态码与错误码，便于排查（9.1 要求令牌不出现，这里本来也没有）。
	e := &APIError{Status: 401, Code: "invalid_token", Message: "令牌无效"}
	if !strings.Contains(e.Error(), "401") || !strings.Contains(e.Error(), "invalid_token") {
		t.Errorf("错误文案信息不全：%q", e.Error())
	}
}

// TestServerErrorSurfacesAsAPIError 端到端：401 经 Report 冒泡后仍可被识别。
func TestServerErrorSurfacesAsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_token","message":"令牌无效"}}`))
	}))
	defer srv.Close()

	_, err := newPlainClient(t, srv.URL).Report(context.Background(), model.ReportRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("应为 *APIError，得到 %v", err)
	}
	if !apiErr.IsUnauthorized() {
		t.Error("401 必须能被识别为令牌问题")
	}
}

// TestContextCancellationPropagates 断言 ctx 取消能让请求立刻返回。
func TestContextCancellationPropagates(t *testing.T) {
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 一直不响应，直到测试明确放行（两个分支都写，是为了不依赖
		// "服务端能否察觉到客户端断连"这个实现细节）。
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
	}))
	// defer 顺序要紧：`srv.Close()` 会等所有在途请求处理完，
	// 所以它必须**先注册、后执行**（LIFO），否则处理器还在阻塞，Close 永远不返回——
	// 实测把这个测试进程整个挂死到 -timeout。
	defer srv.Close()
	defer close(unblock)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := newPlainClient(t, srv.URL).Report(ctx, model.ReportRequest{})
	if err == nil {
		t.Fatal("ctx 已取消，应报错")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("取消后仍等了 %v 才返回", elapsed)
	}
}

// TestResolveJoinsPathPrefix 断言带路径前缀的 Master 地址能正确拼接。
//
// 反代把 Master 挂在子路径下（/probe）是常见部署方式；拼错会得到 //api/... 这种
// 被部分反代规范化的路径，表现是"注册返回 404"。
func TestResolveJoinsPathPrefix(t *testing.T) {
	cases := []struct{ base, want string }{
		{"https://a.example.com", "https://a.example.com/api/agent/v1/report"},
		{"https://a.example.com/", "https://a.example.com/api/agent/v1/report"},
		{"https://a.example.com/probe", "https://a.example.com/probe/api/agent/v1/report"},
		{"https://a.example.com/probe/", "https://a.example.com/probe/api/agent/v1/report"},
	}
	for _, c := range cases {
		cl, err := New(Options{MasterURL: c.base})
		if err != nil {
			t.Fatalf("New(%q) 失败: %v", c.base, err)
		}
		if got := cl.resolve("/api/agent/v1/report"); got != c.want {
			t.Errorf("resolve(%q) = %q，期望 %q", c.base, got, c.want)
		}
	}
}

// TestParsePinsNormalizesBase64 断言几种真实的"手工粘贴"写法都指向同一把钥匙。
//
// 指纹是运维从文档/后台页面/终端输出里拷进配置的，格式五花八门：
// 带填充、不带填充（`openssl ... | base64 | tr -d '='`）、终端折行带换行。
// 归一化没做好就会出现"指纹明明一模一样却起不来"——而且报的是"格式非法"，
// 完全指不到真正的原因。
func TestParsePinsNormalizesBase64(t *testing.T) {
	raw := sha256.Sum256([]byte("spki"))
	std := base64.StdEncoding.EncodeToString(raw[:])
	noPad := base64.RawStdEncoding.EncodeToString(raw[:])
	wrapped := std[:20] + "\n" + std[20:] // 终端折行

	pins, err := parsePins([]string{" " + std + " ", noPad, wrapped, ""})
	if err != nil {
		t.Fatalf("parsePins 失败: %v", err)
	}
	if len(pins) != 3 {
		t.Fatalf("空串应被跳过，得到 %d 个指纹：%v", len(pins), pins)
	}
	for i, got := range pins {
		if got != std {
			t.Errorf("第 %d 种写法归一化成 %q，期望 %q", i+1, got, std)
		}
	}

	// 真正非法的输入仍要报错（不能"什么都接受"）。
	if _, err := parsePins([]string{"!!!不是 base64!!!"}); err == nil {
		t.Error("非法指纹应报错")
	}
	if _, err := parsePins([]string{""}); err != nil {
		t.Errorf("全空列表应正常返回空结果，得到 %v", err)
	}
}

// TestDefaultTimeoutIsFinite 断言不配超时时会有一个有限的默认值。
//
// 不设超时的 http.Client 遇到"连上了但永不响应"的对端会一直挂着，
// 而上报是在主循环里同步调的——于是整个 Agent 停摆。
func TestDefaultTimeoutIsFinite(t *testing.T) {
	for _, given := range []time.Duration{0, -time.Second} {
		cl, err := New(Options{MasterURL: "https://a.example.com", Timeout: given})
		if err != nil {
			t.Fatalf("New 失败: %v", err)
		}
		if cl.hc.Timeout <= 0 || cl.hc.Timeout > time.Minute {
			t.Errorf("Timeout=%v 时应回退到有限的默认值，得到 %v", given, cl.hc.Timeout)
		}
	}
	custom := 7 * time.Second
	cl, err := New(Options{MasterURL: "https://a.example.com", Timeout: custom})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if cl.hc.Timeout != custom {
		t.Errorf("显式超时应被采用，得到 %v", cl.hc.Timeout)
	}
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// newPlainClient 建一个指向本机 httptest 服务器的明文客户端。
//
// httptest 给的是 http://127.0.0.1:port，正是 S7 允许的那一种例外
// （回环 + 显式开启），所以这里能真实走完整条请求路径。
func newPlainClient(t *testing.T, base string) *Client {
	t.Helper()
	cl, err := New(Options{MasterURL: base, InsecureHTTP: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("建客户端失败: %v", err)
	}
	return cl
}

// spkiPin 算出 TLS 服务器证书的公钥指纹（与 Agent 侧算法一致）。
func spkiPin(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("测试服务器没有证书")
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("写响应失败: %v", err)
	}
}

// 查询串必须原样到达服务端。
//
// 这条是"真被踩到才补的"：早先 resolve 把整串塞进 u.Path，'?' 被转义成 %3F，
// 服务端收到的是一个名字里带问号的路径 —— 表现就是 404，
// 而单看 `resolve` 的代码完全看不出来（自升级的取件接口就是这样一直 404 的）。
func TestQueryStringReachesServer(t *testing.T) {
	var gotQuery, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotPath = r.URL.RawQuery, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c, err := New(Options{MasterURL: srv.URL, InsecureHTTP: true})
	if err != nil {
		t.Fatalf("建客户端失败: %v", err)
	}
	out := map[string]any{}
	if err := c.Get(context.Background(),
		"/api/agent/v1/releases/latest?platform=linux_armv7&current=1.0.0", &out); err != nil {
		t.Fatalf("带查询串的 GET 失败: %v", err)
	}
	if gotPath != "/api/agent/v1/releases/latest" {
		t.Errorf("路径被查询串污染：%q", gotPath)
	}
	if gotQuery != "platform=linux_armv7&current=1.0.0" {
		t.Errorf("查询串不对：%q", gotQuery)
	}
	if out["ok"] != true {
		t.Errorf("响应未解析：%+v", out)
	}
}
