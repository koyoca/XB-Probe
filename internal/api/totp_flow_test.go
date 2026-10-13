package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/probe/internal/auth"
	"github.com/yourorg/probe/internal/db"
	"github.com/yourorg/probe/internal/state"
	"github.com/yourorg/probe/internal/store"
)

// 6.2 的端到端校验：TOTP 启用流程 + 恢复码登录。
//
// 为什么必须做这一层而不是只测 auth 包里的函数：
// 登录链路上有四处**各自独立**的契约，任何一处错位都会让"启用了 TOTP 的管理员
// 登不进后台"，而单测每一层都是绿的：
//  1. 启用时存进去的密钥形态（base32 文本）
//  2. 登录时读取密钥并做兼容解码（VerifyTOTP 的 looksBase32 分支）
//  3. 恢复码签发侧与核验侧共用 NormalizeRecoveryCode
//  4. 恢复码消费必须原子（并发/重复使用）
// 这些只有把 HTTP 请求真正打进去才会暴露。

const testPw = "CorrectHorseBattery1!"

func newTOTPTestHandler(t *testing.T) (*Handler, *store.Repo) {
	t.Helper()
	dbs, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = dbs.Close() })
	if err := dbs.Migrate(); err != nil {
		t.Fatalf("迁移失败：%v", err)
	}
	repo := store.New(dbs)

	hash, err := auth.HashPassword(testPw)
	if err != nil {
		t.Fatalf("哈希密码失败：%v", err)
	}
	if _, err := repo.CreateAdmin("ops", hash, nil, time.Now().Unix()); err != nil {
		t.Fatalf("创建管理员失败：%v", err)
	}

	h := New(Deps{
		Repo:     repo,
		Registry: state.NewRegistry(60),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return h, repo
}

// postJSON 发一个带 JSON body 的请求，返回响应与解码后的 map。
func postJSON(t *testing.T, h http.HandlerFunc, body any, setup ...func(*http.Request)) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for _, f := range setup {
		f(req)
	}
	w := httptest.NewRecorder()
	h(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

// 完整走一遍：setup → enable → 用动态码登录 → 用恢复码登录。
func TestTOTPEnableFlowAndRecoveryLogin(t *testing.T) {
	h, repo := newTOTPTestHandler(t)
	now := time.Now()

	// ---- 1) setup：拿到密钥但不落库 ----
	w, out := postJSON(t, h.handleTOTPSetup, map[string]any{},
		withAdmin(&auth.Session{AdminID: 1, CSRFToken: "csrf"}))
	if w.Code != http.StatusOK {
		t.Fatalf("setup 应返回 200，得到 %d：%s", w.Code, w.Body.String())
	}
	secret, _ := out["secret"].(string)
	if secret == "" {
		t.Fatal("setup 应返回 secret")
	}
	if uri, _ := out["otpauth"].(string); !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Errorf("应返回合法的 otpauth URI，得到 %q", uri)
	}
	// 二维码 SVG 必须由服务端给出：前端 CSP 是 default-src 'self'，
	// 既不能引 CDN 的二维码库也不能用 data: 图片，只能插内联 SVG。
	// 这里顺带校验它是自包含的（无外部引用），否则浏览器会静默拒绝加载。
	svg, _ := out["qr_svg"].(string)
	if !strings.HasPrefix(svg, "<svg") {
		t.Fatalf("setup 应返回内联 SVG 二维码，得到 %q", svg)
	}
	if strings.Contains(strings.ReplaceAll(svg, "http://www.w3.org/2000/svg", ""), "http") {
		t.Error("二维码 SVG 不应引用外部资源（CSP 为 default-src 'self'）")
	}
	// 关键：setup **不能**落库，否则扫错码的人会被自己锁在外面。
	if on, _ := repo.HasTOTP(1); on {
		t.Fatal("setup 阶段不应写入密钥（否则没验证成功就已经启用了）")
	}

	// ---- 2) enable：先给一个错码，必须被拒且不落库 ----
	code := mustTOTP(t, secret, now)
	// 构造一个必然错误的 6 位码。
	badCode := "000000"
	if badCode == code {
		badCode = "111111"
	}
	w, _ = postJSON(t, h.handleTOTPEnable,
		map[string]any{"secret": secret, "code": badCode},
		withAdmin(&auth.Session{AdminID: 1, CSRFToken: "csrf"}))
	if w.Code == http.StatusOK {
		t.Fatal("错误动态码不应启用成功")
	}
	if on, _ := repo.HasTOTP(1); on {
		t.Fatal("校验失败时不应写入密钥")
	}

	// ---- 3) enable：正确动态码 → 启用 + 下发恢复码 ----
	w, out = postJSON(t, h.handleTOTPEnable,
		map[string]any{"secret": secret, "code": code},
		withAdmin(&auth.Session{AdminID: 1, CSRFToken: "csrf"}))
	if w.Code != http.StatusOK {
		t.Fatalf("正确动态码应启用成功，得到 %d：%s", w.Code, w.Body.String())
	}
	if on, _ := repo.HasTOTP(1); !on {
		t.Fatal("启用后应已写入密钥")
	}
	rawCodes, _ := out["recovery_code"].([]any)
	if len(rawCodes) != recoveryCodeCount {
		t.Fatalf("应下发 %d 个恢复码，实际 %d", recoveryCodeCount, len(rawCodes))
	}
	codes := make([]string, 0, len(rawCodes))
	for _, c := range rawCodes {
		codes = append(codes, c.(string))
	}

	// ---- 4) 未带动态码登录必须失败 ----
	req := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(`{"username":"ops","password":"`+testPw+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.handleLogin(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("启用 TOTP 后不带动态码应 401，得到 %d", w.Code)
	}

	// ---- 5) 正确动态码登录成功 ----
	body := map[string]string{"username": "ops", "password": testPw,
		"totp": mustTOTP(t, secret, time.Now())}
	raw, _ := json.Marshal(body)
	req = httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.handleLogin(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("正确动态码应登录成功，得到 %d：%s", w.Code, w.Body.String())
	}

	// ---- 6) 用恢复码登录（替换动态码），必须成功 ----
	body = map[string]string{"username": "ops", "password": testPw, "totp": codes[0]}
	raw, _ = json.Marshal(body)
	req = httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.handleLogin(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("恢复码应能登录，得到 %d：%s", w.Code, w.Body.String())
	}

	// ---- 7) 同一个恢复码**不能**再次使用 ----
	// 这是"一次性"语义的核心：手机丢过一次的管理员会把用过的码划掉，
	// 如果服务器还认，等于那张纸永久有效。
	req = httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.handleLogin(w, req)
	if w.Code == http.StatusOK {
		t.Error("已消费的恢复码不应再次登录成功")
	}

	// ---- 8) 另一个恢复码仍可用（没被误伤） ----
	body = map[string]string{"username": "ops", "password": testPw, "totp": codes[1]}
	raw, _ = json.Marshal(body)
	req = httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.handleLogin(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("第二个恢复码应可用，得到 %d：%s", w.Code, w.Body.String())
	}

	// ---- 9) 恢复码书写形式宽容：不带连字符、小写 ----
	messy := strings.ToLower(strings.ReplaceAll(codes[2], "-", ""))
	body = map[string]string{"username": "ops", "password": testPw, "totp": messy}
	raw, _ = json.Marshal(body)
	req = httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.handleLogin(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("不带连字符/小写的恢复码应能登录，得到 %d：%s", w.Code, w.Body.String())
	}
}

// 关闭 TOTP 需要密码 + 二次凭证（会话本身不足以关闭）。
func TestTOTPDisableRequiresPasswordAndCode(t *testing.T) {
	h, repo := newTOTPTestHandler(t)
	secret, _ := auth.GenerateTOTPSecret()
	_ = repo.SetTOTPSecret(1, []byte(secret))

	// 密码错 → 拒绝。
	w, _ := postJSON(t, h.handleTOTPDisable,
		map[string]any{"password": "WrongPassword123!", "code": mustTOTP(t, secret, time.Now())},
		withAdmin(&auth.Session{AdminID: 1, CSRFToken: "c"}))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("密码错误应 401，得到 %d", w.Code)
	}
	if on, _ := repo.HasTOTP(1); !on {
		t.Fatal("密码错误时不应关闭 TOTP")
	}

	// 二次凭证错 → 拒绝。
	w, _ = postJSON(t, h.handleTOTPDisable,
		map[string]any{"password": testPw, "code": "000000"},
		withAdmin(&auth.Session{AdminID: 1, CSRFToken: "c"}))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("二次凭证错误应 401，得到 %d", w.Code)
	}
	if on, _ := repo.HasTOTP(1); !on {
		t.Fatal("凭证错误时不应关闭 TOTP")
	}
	// 注意：上面两次失败会累计失败计数，第 2 次已达上限触发锁定。
	// 这是预期行为（防爆破），但会让后续用正确凭证也被锁——
	// 所以这里显式清零，模拟"管理员等了 15 分钟"。
	_ = repo.ResetLoginFailures(1)

	// 正确密码 + 正确动态码 → 关闭成功。
	w, _ = postJSON(t, h.handleTOTPDisable,
		map[string]any{"password": testPw, "code": mustTOTP(t, secret, time.Now())},
		withAdmin(&auth.Session{AdminID: 1, CSRFToken: "c"}))
	if w.Code != http.StatusOK {
		t.Fatalf("正确凭证应关闭成功，得到 %d：%s", w.Code, w.Body.String())
	}
	if on, _ := repo.HasTOTP(1); on {
		t.Error("应已关闭 TOTP")
	}
}

// 未启用 TOTP 时登录**不要求**动态码（不能因为字段是空的就一律拒绝）。
func TestLoginWithoutTOTPNeedsNoCode(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	raw := []byte(`{"username":"ops","password":"` + testPw + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleLogin(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("未启用 TOTP 时应直接登录成功，得到 %d：%s", w.Code, w.Body.String())
	}
}

// 重新生成恢复码需要密码，且旧码立刻作废。
func TestRegenerateRecoveryCodesRequiresPassword(t *testing.T) {
	h, repo := newTOTPTestHandler(t)
	secret, _ := auth.GenerateTOTPSecret()
	_ = repo.SetTOTPSecret(1, []byte(secret))
	old, err := h.issueRecoveryCodes(1, time.Now().Unix())
	if err != nil {
		t.Fatalf("签发失败：%v", err)
	}

	// 密码错 → 拒绝，且旧码仍然有效（不能因为一次失败尝试就把管理员的码废掉）。
	w, _ := postJSON(t, h.handleRegenerateRecoveryCodes,
		map[string]any{"password": "WrongPassword123!"},
		withAdmin(&auth.Session{AdminID: 1, CSRFToken: "c"}))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("密码错误应 401，得到 %d", w.Code)
	}
	if _, ok, _ := repo.ConsumeRecoveryCode(auth.HashRecoveryCode(old[0]), time.Now().Unix()); !ok {
		t.Error("重生成失败时旧恢复码应仍然有效")
	}
	_ = repo.ResetLoginFailures(1)

	// 未启用 TOTP 时应明确拒绝（而不是生成一堆永远用不上的码）。
	_ = repo.SetTOTPSecret(1, nil)
	w, _ = postJSON(t, h.handleRegenerateRecoveryCodes,
		map[string]any{"password": testPw},
		withAdmin(&auth.Session{AdminID: 1, CSRFToken: "c"}))
	if w.Code != http.StatusBadRequest {
		t.Errorf("未启用 TOTP 时应 400，得到 %d", w.Code)
	}
}

// withAdmin 把一个已认证会话注入请求上下文（跳过 requireAdmin）。
func withAdmin(s *auth.Session) func(*http.Request) {
	return func(r *http.Request) {
		*r = *r.WithContext(contextWithAdmin(r.Context(), s))
	}
}

// mustTOTP 算出某时刻的正确动态码；算不出直接失败。
//
// "正确的码能被接受"这条分支必须被真正覆盖——只测"错的码被拒绝"的话，
// 启用 TOTP 之后"永远登不进去"这个最严重的故障会完全测不到。
func mustTOTP(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := auth.TOTPCode(secret, at)
	if err != nil {
		t.Fatalf("计算动态码失败：%v", err)
	}
	return code
}
