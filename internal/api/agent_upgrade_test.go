package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koyoca/xb-probe/internal/auth"
)

// Agent 自升级取件接口（12.6）的测试。
//
// 这里刻意走 HTTP 而不是直接调 handler 内部的函数：这条链路要钉的是
// **鉴权与错误语义**（没令牌不能取、平台没发布要能区分于"Master 没配好"），
// 这些只有在完整请求里才成立。

// newUpgradeEnv 建一个带节点令牌的 handler，并把 distDir 指向临时目录。
func newUpgradeEnv(t *testing.T) (*Handler, string, string) {
	t.Helper()
	h, repo := newTOTPTestHandler(t)
	h.Tokens = auth.NewTokenManager(repo)

	nodeID, err := repo.CreateNode("pub-up", "升级节点", "默认", time.Now().Unix())
	if err != nil {
		t.Fatalf("建节点失败：%v", err)
	}
	plain, hash, _, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("生成令牌失败：%v", err)
	}
	if _, err := repo.CreateToken(nodeID, hash, "t", time.Now().Unix()); err != nil {
		t.Fatalf("写令牌失败：%v", err)
	}
	h.DistDir = t.TempDir()
	return h, h.DistDir, plain
}

// seedVersion 在 dist 目录下放一个版本：一个构件 + 一份 SHA256SUMS。
func seedVersion(t *testing.T, distDir, version, artifact, content string) {
	t.Helper()
	dir := filepath.Join(distDir, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建版本目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, artifact), []byte(content), 0o644); err != nil {
		t.Fatalf("写构件失败：%v", err)
	}
	sum, err := sha256Hex(content)
	if err != nil {
		t.Fatalf("计算校验和失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"),
		[]byte(sum+"  "+artifact+"\n"), 0o644); err != nil {
		t.Fatalf("写 SHA256SUMS 失败：%v", err)
	}
}

func get(t *testing.T, h *Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.AgentRoutes().ServeHTTP(w, req)
	return w
}

// 没有令牌一律 401：分发二进制是"一次污染、全部受害"的入口，
// 不能靠"路径没人知道"来保护。
func TestAgentReleaseLatestRequiresToken(t *testing.T) {
	h, dir, _ := newUpgradeEnv(t)
	seedVersion(t, dir, "1.2.3", "xb-probe-agent_linux_amd64", "binary")

	if w := get(t, h, "/api/agent/v1/releases/latest?platform=linux_amd64", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("无令牌应 401，实得 %d", w.Code)
	}
	if w := get(t, h, "/api/agent/v1/artifact?platform=linux_amd64", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("无令牌取构件应 401，实得 %d", w.Code)
	}
	if w := get(t, h, "/api/agent/v1/releases/latest?platform=linux_amd64", "pk_bogus"); w.Code != http.StatusUnauthorized {
		t.Errorf("无效令牌应 401，实得 %d", w.Code)
	}
}

// platform 直接进路径拼接，必须挡住穿越。
func TestAgentReleaseLatestRejectsBadPlatform(t *testing.T) {
	h, dir, tok := newUpgradeEnv(t)
	seedVersion(t, dir, "1.2.3", "xb-probe-agent_linux_amd64", "binary")

	for _, bad := range []string{"", "../../etc", "linux/../windows_amd64", `..\windows_amd64`} {
		w := get(t, h, "/api/agent/v1/releases/latest?platform="+bad+"", tok)
		if w.Code != http.StatusBadRequest {
			t.Errorf("platform=%q 应 400，实得 %d", bad, w.Code)
		}
	}
}

func TestAgentReleaseLatestReportsVersionAndUpToDate(t *testing.T) {
	h, dir, tok := newUpgradeEnv(t)
	seedVersion(t, dir, "1.10.0", "xb-probe-agent_linux_amd64", "new-binary")
	// 顺手放一个旧版本：必须挑新的那个。
	seedVersion(t, dir, "1.9.0", "xb-probe-agent_linux_amd64", "old-binary")

	w := get(t, h, "/api/agent/v1/releases/latest?platform=linux_amd64&current=1.9.0", tok)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实得 %d：%s", w.Code, w.Body.String())
	}
	var rel agentRelease
	if err := json.Unmarshal(w.Body.Bytes(), &rel); err != nil {
		t.Fatalf("解析响应失败：%v（%s）", err, w.Body.String())
	}
	if rel.Version != "1.10.0" {
		t.Errorf("应挑最新版本，实得 %q", rel.Version)
	}
	if rel.Artifact != "xb-probe-agent_linux_amd64" || rel.SHA256 == "" || rel.Size != int64(len("new-binary")) {
		t.Errorf("构件描述不对：%+v", rel)
	}
	if rel.UpToDate {
		t.Errorf("current=1.9.0 不应判为已最新：%+v", rel)
	}

	// 已是最新 → up_to_date。
	w = get(t, h, "/api/agent/v1/releases/latest?platform=linux_amd64&current=1.10.0", tok)
	_ = json.Unmarshal(w.Body.Bytes(), &rel)
	if !rel.UpToDate {
		t.Errorf("current=1.10.0 应判为已最新：%+v", rel)
	}
	// 比最新还新（回滚场景）也不主动降级。
	w = get(t, h, "/api/agent/v1/releases/latest?platform=linux_amd64&current=2.0.0", tok)
	_ = json.Unmarshal(w.Body.Bytes(), &rel)
	if !rel.UpToDate {
		t.Errorf("current=2.0.0 不应被降级：%+v", rel)
	}
}

// 两种"没有"必须分开报：Master 没发布任何版本 vs 这个平台没编。
// 混成一句会让 armv6 节点反复重试，而根因是发布矩阵里没它。
func TestAgentReleaseLatestDistinguishesMissingCases(t *testing.T) {
	h, dir, tok := newUpgradeEnv(t)

	if w := get(t, h, "/api/agent/v1/releases/latest?platform=linux_amd64", tok); w.Code != http.StatusNotFound {
		t.Errorf("没有任何已发布版本应 404，实得 %d", w.Code)
	}
	seedVersion(t, dir, "1.0.0", "xb-probe-agent_linux_amd64", "x")
	w := get(t, h, "/api/agent/v1/releases/latest?platform=linux_armv6", tok)
	if w.Code != http.StatusNotFound {
		t.Errorf("该平台未发布应 404，实得 %d", w.Code)
	}
	body := w.Body.String()
	if body != "" && !strings.Contains(body, "该平台未发布此版本") {
		t.Errorf("错误信息应说明是「平台未发布」，实得 %s", body)
	}
}

// 取构件：内容必须与文件一致，且响应头带校验和（用于快速失败）。
func TestAgentArtifactServesBytesWithChecksum(t *testing.T) {
	h, dir, tok := newUpgradeEnv(t)
	seedVersion(t, dir, "1.2.3", "xb-probe-agent_linux_amd64", "binary-bytes")

	w := get(t, h, "/api/agent/v1/artifact?platform=linux_amd64", tok)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实得 %d：%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "binary-bytes" {
		t.Errorf("构件内容不对：%q", got)
	}
	want, _ := sha256Hex("binary-bytes")
	if got := w.Header().Get("X-Checksum-Sha256"); got != want {
		t.Errorf("X-Checksum-Sha256 = %q，期望 %q", got, want)
	}
	if got := w.Header().Get("X-Artifact-Version"); got != "1.2.3" {
		t.Errorf("X-Artifact-Version = %q", got)
	}

	// 指定版本：拿到的是那个版本的文件。
	seedVersion(t, dir, "1.0.0", "xb-probe-agent_linux_amd64", "older-bytes")
	w = get(t, h, "/api/agent/v1/artifact?platform=linux_amd64&version=1.0.0", tok)
	if w.Code != http.StatusOK || w.Body.String() != "older-bytes" {
		t.Errorf("按版本取件失败：%d / %q", w.Code, w.Body.String())
	}
}

// 版本号比较必须按数值段比：字符串比会让 1.10.0 < 1.9.0，
// 于是 1.10 发布后所有 Agent 反而"已经是最新"了。
func TestCompareVersionIsNumericNotLexical(t *testing.T) {
	if compareVersion("1.10.0", "1.9.0") <= 0 {
		t.Error("1.10.0 应大于 1.9.0（字符串比会得到相反结果）")
	}
	if compareVersion("1.9.0", "1.10.0") >= 0 {
		t.Error("1.9.0 应小于 1.10.0")
	}
	if compareVersion("1.2.3", "1.2.3") != 0 {
		t.Error("相同版本应为 0")
	}
	if compareVersion("2.0", "1.99.99") <= 0 {
		t.Error("2.0 应大于 1.99.99")
	}
	// 空串与非法值不得把比较带偏（比如 dev 版本）。
	if compareVersion("", "0.0.1") >= 0 {
		t.Error("空版本应小于任何正式版本")
	}
}

// 版本目录名只认纯数字点分：手工拷进去的 nightly 目录不能被"挑最新"选中，
// 否则全网 Agent 会一起换到一个没打算发的二进制。
func TestIsVersionNameRejectsNonNumeric(t *testing.T) {
	for _, s := range []string{"", "v1.2.3", "nightly-2026", "1.2.3-rc1", "1.2.3."} {
		if isVersionName(s) {
			t.Errorf("%q 不该被当成版本号", s)
		}
	}
	if !isVersionName("1.2.3") || !isVersionName("12") {
		t.Error("纯数字点分应被接受")
	}
}

// 构件名必须与 Makefile 的发布命名一致（windows 带 .exe）。
func TestArtifactForTag(t *testing.T) {
	cases := map[string]string{
		"linux_amd64":   "xb-probe-agent_linux_amd64",
		"windows_amd64": "xb-probe-agent_windows_amd64.exe",
		"linux_armv7":   "xb-probe-agent_linux_armv7",
	}
	for tag, want := range cases {
		if got := artifactForTag(tag); got != want {
			t.Errorf("artifactForTag(%q) = %q，期望 %q", tag, got, want)
		}
	}
}

// sha256Hex 计算内容的 SHA-256（小写十六进制），用于生成测试用的 SHA256SUMS。
func sha256Hex(s string) (string, error) {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:]), nil
}
