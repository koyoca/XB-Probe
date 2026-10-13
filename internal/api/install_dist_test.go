package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// /dist/ 的两套路径语义：**带版本**是权威存放位置，**扁平**是"最新版本"的别名。
//
// 为什么要有别名：安装脚本不知道版本号，取的是 `${DIST_BASE}/xb-probe-agent_linux_amd64`
// 与 `${MASTER_URL}/dist/SHA256SUMS`（12.8）；而 `xb-probe-master dist pull`、后台
// "发布文件"页、README 用的是 `data/dist/<版本>/`。两者对不上时，一键安装会在
// "无法获取校验和"那里中止——而这台 Master 上一切看起来都正常。
//
// 别名解析必须与 Agent 自升级用同一套"什么是最新版本"的规则，否则会出现
// "新装节点拿到 A 版、老节点升级到 B 版"的版本错位。

// newDistTestHandler 建一个 DistDir 指向临时目录、且分发入口恒开的 handler。
func newDistTestHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	h, _ := newTOTPTestHandler(t)
	h.InstallMode = func() string { return "always" }
	h.DistDir = t.TempDir()
	return h, h.DistDir
}

// seedDistFile 在 distDir 下写一个文件（自动建目录）。
func seedDistFile(t *testing.T, distDir, rel, content string) {
	t.Helper()
	full := filepath.Join(distDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
}

// distGet 取 /dist/ 下的一个路径，返回状态码与响应体。
func distGet(t *testing.T, h *Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handleDist(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

// TestDistFlatPathResolvesToNewestVersion 钉住别名指向**最新**版本。
//
// 三个版本目录刻意布置成"字典序与版本序不一致"：nightly-20260101 按字典序
// 排在 9.9.9 之后，若别名解析少做一次 isVersionName 过滤，就会取到它。
func TestDistFlatPathResolvesToNewestVersion(t *testing.T) {
	h, dir := newDistTestHandler(t)
	seedDistFile(t, dir, "1.0.0/xb-probe-agent_linux_amd64", "OLD-BINARY")
	seedDistFile(t, dir, "1.0.0/SHA256SUMS", "aaaa  xb-probe-agent_linux_amd64\n")
	seedDistFile(t, dir, "9.9.9/xb-probe-agent_linux_amd64", "NEW-BINARY")
	seedDistFile(t, dir, "9.9.9/SHA256SUMS", "bbbb  xb-probe-agent_linux_amd64\n")
	seedDistFile(t, dir, "9.9.9/SHA256SUMS.minisig", "NEW-SIG")
	// 非点分目录：不是合法版本，必须被忽略。
	seedDistFile(t, dir, "nightly-20260101/xb-probe-agent_linux_amd64", "NIGHTLY-BINARY")
	seedDistFile(t, dir, "nightly-20260101/SHA256SUMS", "cccc  xb-probe-agent_linux_amd64\n")

	if code, body := distGet(t, h, "/dist/SHA256SUMS"); code != 200 || body != "bbbb  xb-probe-agent_linux_amd64\n" {
		t.Errorf("扁平 SHA256SUMS 未指向最新版本：%d %q", code, body)
	}
	if code, body := distGet(t, h, "/dist/xb-probe-agent_linux_amd64"); code != 200 || body != "NEW-BINARY" {
		t.Errorf("扁平构件名未指向最新版本：%d %q", code, body)
	}
	if code, body := distGet(t, h, "/dist/SHA256SUMS.minisig"); code != 200 || body != "NEW-SIG" {
		t.Errorf("扁平签名文件未指向最新版本：%d %q", code, body)
	}
	// 带版本的原路径必须**照旧**可用（别名不能把权威路径改道）。
	if code, body := distGet(t, h, "/dist/1.0.0/xb-probe-agent_linux_amd64"); code != 200 || body != "OLD-BINARY" {
		t.Errorf("带版本的路径被别名改道了：%d %q", code, body)
	}
}

// 根目录下的显式文件优先于别名：运维手工放了一份就用那份，不去猜。
func TestDistExplicitFlatFileWinsOverAlias(t *testing.T) {
	h, dir := newDistTestHandler(t)
	seedDistFile(t, dir, "9.9.9/xb-probe-agent_linux_amd64", "NEW-BINARY")
	seedDistFile(t, dir, "xb-probe-agent_linux_amd64", "PINNED-BINARY")

	if code, body := distGet(t, h, "/dist/xb-probe-agent_linux_amd64"); code != 200 || body != "PINNED-BINARY" {
		t.Errorf("根目录的显式文件应优先：%d %q", code, body)
	}
}

// 别名只作用于**单段**路径：带子目录的请求必须原样 404，
// 不能被改写成 /dist/<最新版本>/<原路径>（那会把"没有这个文件"变成谜之 404）。
func TestDistAliasDoesNotApplyToSubPaths(t *testing.T) {
	h, dir := newDistTestHandler(t)
	seedDistFile(t, dir, "9.9.9/xb-probe-agent_linux_amd64", "NEW-BINARY")

	for _, p := range []string{
		"/dist/nope/xb-probe-agent_linux_amd64",
		"/dist/1.0.0/xb-probe-agent_linux_amd64", // 存在 9.9.9，但 1.0.0 下确实没有
		"/dist/9.9.9/",                           // 目录
	} {
		if code, _ := distGet(t, h, p); code != 404 {
			t.Errorf("%s 应为 404，实得 %d", p, code)
		}
	}

	// 关键用例：让 `<最新版本>/<子路径>` **确实存在**。
	// 上面那几条路径无论别名是否改道都恰好不存在，所以它们测不出差别——
	// "别名也作用于子路径"这个变异体正是靠这样一条用例才抓得住。
	// 别名只服务于 `/dist/<文件>` 这种单段路径，`/dist/<子目录>/<文件>`
	// 必须原样 404，否则"按子目录找文件"会被悄悄改道到最新版本里去。
	seedDistFile(t, dir, "9.9.9/inner/thing.bin", "INNER")
	if code, body := distGet(t, h, "/dist/inner/thing.bin"); code != 404 {
		t.Errorf("/dist/inner/thing.bin 应为 404（别名不作用于子路径），实得 %d，body=%q",
			code, body)
	}
}

// 一个版本都没有时，扁平路径仍是 404（别名不能凭空造出内容）。
func TestDistAliasWithoutAnyVersion(t *testing.T) {
	h, dir := newDistTestHandler(t)
	seedDistFile(t, dir, "not-a-version/xb-probe-agent_linux_amd64", "X")
	if code, _ := distGet(t, h, "/dist/SHA256SUMS"); code != 404 {
		t.Errorf("没有任何合法版本时应 404，实得 %d", code)
	}
}

// 别名不能成为绕过路径穿越的口子。
func TestDistAliasStillRejectsTraversal(t *testing.T) {
	h, dir := newDistTestHandler(t)
	seedDistFile(t, dir, "9.9.9/SHA256SUMS", "bbbb  x\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "master.key"), []byte("SECRET"), 0o600); err != nil {
		t.Fatalf("写敏感文件失败：%v", err)
	}
	for _, p := range []string{"/dist/../master.key", "/dist/..%2fmaster.key", "/dist/%2e%2e/master.key"} {
		code, body := distGet(t, h, p)
		if code == 200 || body == "SECRET" {
			t.Errorf("%s 泄漏了目录外内容：%d %q", p, code, body)
		}
	}
}
