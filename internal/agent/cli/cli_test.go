package cli

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/yourorg/probe/internal/agent/agentconfig"
	"github.com/yourorg/probe/internal/agent/service"
	"github.com/yourorg/probe/internal/dist"
	"github.com/yourorg/probe/internal/model"
)

// ---------------------------------------------------------------------------
// 测试基础设施
// ---------------------------------------------------------------------------

// captureOutput 把 os.Stdout / os.Stderr 换成管道，返回 fn 写出的内容。
//
// CLI 的输出目标就是这两个全局变量（fmt.Println / fmt.Fprint(os.Stderr, ...)，
// flag 包的 Output() 也是惰性解析 os.Stderr），所以这是唯一能在不重构生产代码的
// 前提下断言"用户到底看到了什么"的办法。
//
// 必须 defer 还原：本包的测试之间共享进程，漏还原会让后续测试的输出串味。
func captureOutput(t *testing.T, fn func()) (string, string) {
	t.Helper()

	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建 stdout 管道失败：%v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建 stderr 管道失败：%v", err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()

	fn()

	// 先关写端，ReadAll 才能读到 EOF。
	if err := wOut.Close(); err != nil {
		t.Fatalf("关闭 stdout 写端失败：%v", err)
	}
	if err := wErr.Close(); err != nil {
		t.Fatalf("关闭 stderr 写端失败：%v", err)
	}
	outB, _ := io.ReadAll(rOut)
	errB, _ := io.ReadAll(rErr)
	_ = rOut.Close()
	_ = rErr.Close()
	return string(outB), string(errB)
}

// runCLI 执行一次命令，返回退出码与两个输出流。
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	code := -1
	out, errOut := captureOutput(t, func() { code = Run(args, "testver") })
	return code, out, errOut
}

// spkiPin 算出证书的 SPKI SHA-256（base64），即 Agent 的 --spki-sha256 期望值。
func spkiPin(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	if cert == nil {
		t.Fatal("测试服务器没有证书")
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// enrollServer 是一个只会应答 /enroll 的假 Master，记录收到的请求与鉴权头。
type enrollServer struct {
	*httptest.Server
	token  string
	nodeID int64
	cfg    model.AgentConfig

	mu       sync.Mutex
	enrolled []model.EnrollRequest
	authHdr  []string
	hits     int
}

func newEnrollServer(t *testing.T, token string, nodeID int64, cfg model.AgentConfig) *enrollServer {
	t.Helper()
	es := &enrollServer{token: token, nodeID: nodeID, cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/v1/enroll", func(w http.ResponseWriter, r *http.Request) {
		var req model.EnrollRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		es.mu.Lock()
		es.enrolled = append(es.enrolled, req)
		es.authHdr = append(es.authHdr, r.Header.Get("Authorization"))
		es.hits++
		es.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(model.EnrollResponse{
			NodeID: es.nodeID,
			Token:  es.token,
			Config: es.cfg,
		})
	})
	es.Server = httptest.NewTLSServer(mux)
	t.Cleanup(es.Close)
	return es
}

func (es *enrollServer) requests() ([]model.EnrollRequest, []string, int) {
	es.mu.Lock()
	defer es.mu.Unlock()
	reqs := make([]model.EnrollRequest, len(es.enrolled))
	copy(reqs, es.enrolled)
	hdrs := make([]string, len(es.authHdr))
	copy(hdrs, es.authHdr)
	return reqs, hdrs, es.hits
}

// ---------------------------------------------------------------------------
// Run 的分派与退出码
// ---------------------------------------------------------------------------

func TestRunWithoutArgsPrintsUsageAndReportsMisuse(t *testing.T) {
	code, out, errOut := runCLI(t)
	if code != 2 {
		t.Fatalf("无参数应返回 2（用法错误），得到 %d", code)
	}
	if out != "" {
		t.Errorf("用法不应写到 stdout，实际：%q", out)
	}
	if !strings.Contains(errOut, "用法：probe-agent") {
		t.Fatalf("stderr 缺少用法提示，实际：%q", errOut)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	code, _, errOut := runCLI(t, "frobnicate")
	if code != 2 {
		t.Fatalf("未知命令应返回 2，得到 %d", code)
	}
	if !strings.Contains(errOut, "未知命令：frobnicate") {
		t.Fatalf("stderr 未指出未知命令，实际：%q", errOut)
	}
}

func TestRunHelpVariantsReturnZero(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			code, out, errOut := runCLI(t, arg)
			if code != 0 {
				t.Fatalf("%s 应返回 0，得到 %d", arg, code)
			}
			if out != "" {
				t.Errorf("帮助不应写到 stdout，实际：%q", out)
			}
			if !strings.Contains(errOut, "用法：probe-agent") {
				t.Fatalf("stderr 缺少用法提示，实际：%q", errOut)
			}
		})
	}
}

func TestRunVersionVariants(t *testing.T) {
	for _, arg := range []string{"version", "-v", "--version"} {
		t.Run(arg, func(t *testing.T) {
			code, out, _ := runCLI(t, arg)
			if code != 0 {
				t.Fatalf("%s 应返回 0，得到 %d", arg, code)
			}
			if !strings.Contains(out, "testver") {
				t.Fatalf("版本输出未包含注入的版本号，实际：%q", out)
			}
			// 版本行带平台，便于运维在工单里直接贴。
			if !strings.Contains(out, runtime.GOOS+"/"+runtime.GOARCH) {
				t.Fatalf("版本输出未包含平台，实际：%q", out)
			}
			// 发布标签必须一并打印：`probe-agent upgrade` 靠它挑产物名，而
			// "标签没注入"是静默的（go build 对指向不存在符号的 -X 不报错）。
			// 这行是运维唯一能直接看出"手上这份是发布构建吗"的地方。
			if !strings.Contains(out, dist.CurrentTag()) {
				t.Fatalf("版本输出未包含发布标签，实际：%q", out)
			}
		})
	}
}

func TestUsageMentionsEveryCommand(t *testing.T) {
	_, _, errOut := runCLI(t, "help")
	for _, cmd := range []string{"run", "install", "reenroll", "uninstall", "version"} {
		if !strings.Contains(errOut, cmd) {
			t.Errorf("用法未提及命令 %q，实际：%q", cmd, errOut)
		}
	}
	// install 的四个关键开关必须出现在帮助里：漏写会让人用 --dir 之类不存在的参数瞎试。
	for _, flagName := range []string{"--master", "--code", "--dir", "--no-service", "--spki-sha256", "--force", "--insecure-http"} {
		if !strings.Contains(errOut, flagName) {
			t.Errorf("用法未提及 install 参数 %q", flagName)
		}
	}
}

func TestFlagParseErrorsReturnTwo(t *testing.T) {
	cases := map[string][]string{
		"run":       {"run", "--no-such-flag"},
		"install":   {"install", "--no-such-flag"},
		"reenroll":  {"reenroll", "--no-such-flag"},
		"uninstall": {"uninstall", "--no-such-flag"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, errOut := runCLI(t, args...)
			if code != 2 {
				t.Fatalf("参数错误应返回 2，得到 %d（stderr=%q）", code, errOut)
			}
			if !strings.Contains(errOut, "no-such-flag") {
				t.Errorf("stderr 未指出出错参数，实际：%q", errOut)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

func TestInstallRequiresMasterAndCode(t *testing.T) {
	dir := t.TempDir()

	code, _, errOut := runCLI(t, "install", "--dir", dir)
	if code != 2 {
		t.Fatalf("缺少 --master/--code 应返回 2，得到 %d", code)
	}
	if !strings.Contains(errOut, "--master 与 --code 均为必需") {
		t.Fatalf("stderr 未给出必需参数提示，实际：%q", errOut)
	}

	// 只给一个也不算数。
	if code, _, _ := runCLI(t, "install", "--master", "https://a.example", "--dir", dir); code != 2 {
		t.Fatalf("只给 --master 应返回 2，得到 %d", code)
	}
	if code, _, _ := runCLI(t, "install", "--code", "ABC", "--dir", dir); code != 2 {
		t.Fatalf("只给 --code 应返回 2，得到 %d", code)
	}

	// 校验失败时绝不能留下任何配置：半安装状态比没安装更难查。
	if _, err := os.Stat(filepath.Join(dir, "agent.yaml")); !os.IsNotExist(err) {
		t.Fatalf("参数校验失败却写了配置：err=%v", err)
	}
}

func TestInstallReadsCodeFromEnv(t *testing.T) {
	// PROBE_CODE 是文档承诺的替代入口（12.3）。这里只验证"环境变量确实被读到了：
	// 于是流程越过"参数缺失"这一关，进到连接校验阶段失败（返回 1 而不是 2）。
	t.Setenv("PROBE_CODE", "ENV-CODE")

	dir := t.TempDir()
	code, _, errOut := runCLI(t, "install", "--master", "://bad", "--dir", dir)
	if code == 2 {
		t.Fatalf("环境变量未被当作注册码（返回 2）：stderr=%q", errOut)
	}
	if code != 1 {
		t.Fatalf("Master 地址非法应返回 1，得到 %d（stderr=%q）", code, errOut)
	}
	if !strings.Contains(errOut, "错误：") {
		t.Errorf("stderr 未以「错误：」报告，实际：%q", errOut)
	}
}

func TestInstallRejectsPlainHTTPWithoutFlag(t *testing.T) {
	dir := t.TempDir()
	code, _, errOut := runCLI(t, "install", "--master", "http://127.0.0.1:1", "--code", "ABC", "--dir", dir)
	if code != 1 {
		t.Fatalf("http:// 未加 --insecure-http 应返回 1，得到 %d", code)
	}
	if !strings.Contains(errOut, "http") {
		t.Errorf("stderr 未说明协议被拒绝，实际：%q", errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.yaml")); !os.IsNotExist(err) {
		t.Fatalf("连接设置被拒后不应写配置：err=%v", err)
	}
}

// TestInstallHappyPath 走完整的注册流程：校验 Master → 换取令牌 → 落盘配置。
//
// 刻意加 --no-service：不传的话 installService 会真的去 SCM 注册系统服务，
// 那既需要管理员权限，又是对宿主机不可逆的副作用。
func TestInstallHappyPathWritesConfig(t *testing.T) {
	srv := newEnrollServer(t, "pk_installed", 42, model.AgentConfig{
		ConfigVersion:   "v1",
		ReportIntervalS: 5,
		ProbeIntervalS:  20,
		NICExclude:      []string{"lo", "docker*"},
		Targets: []model.Target{
			{ID: 7, Name: "本机回环", Host: "127.0.0.1", Port: 80, Method: "tcp", TimeoutMs: 1000, Count: 3},
		},
	})

	dir := t.TempDir()
	pin := spkiPin(t, srv.Certificate())

	code, out, errOut := runCLI(t,
		"install",
		"--master", srv.URL,
		"--code", "  ab-12cd  ", // 前后空白与大小写都应被规范化
		"--dir", dir,
		"--no-service",
		"--spki-sha256", pin,
	)
	if code != 0 {
		t.Fatalf("install 失败：code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if !strings.Contains(out, "已注册节点（node_id=42）") {
		t.Errorf("stdout 未报告 node_id，实际：%q", out)
	}
	if !strings.Contains(out, "--no-service") {
		t.Errorf("--no-service 时应明确提示已跳过服务注册，实际：%q", out)
	}

	cfgPath := filepath.Join(dir, "agent.yaml")
	cfg, err := agentconfig.Load(cfgPath)
	if err != nil {
		t.Fatalf("配置未落盘或不可读：%v", err)
	}
	if cfg.Token != "pk_installed" {
		t.Errorf("令牌未写入，实际 %q", cfg.Token)
	}
	if cfg.MasterURL != srv.URL {
		t.Errorf("MasterURL 被改写：%q", cfg.MasterURL)
	}
	if len(cfg.SPKISHA256) != 1 || cfg.SPKISHA256[0] != pin {
		t.Errorf("SPKI 指纹未按归一化形式落盘：%v", cfg.SPKISHA256)
	}
	// Master 下发的运行参数必须生效。
	if cfg.ReportIntervalS != 5 || cfg.ProbeIntervalS != 20 {
		t.Errorf("上报/探测间隔未采用服务端配置：%d/%d", cfg.ReportIntervalS, cfg.ProbeIntervalS)
	}
	if cfg.ConfigVersion != "v1" {
		t.Errorf("ConfigVersion 未写入：%q", cfg.ConfigVersion)
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0].ID != 7 {
		t.Errorf("探测目标未写入：%+v", cfg.Targets)
	}
	if strings.Join(cfg.NICExclude, ",") != "lo,docker*" {
		t.Errorf("网卡排除清单未写入：%v", cfg.NICExclude)
	}

	// 服务注册被跳过时不得留下安装清单：卸载流程靠清单决定删哪些路径，
	// 写了一份指向"从未安装过的东西"的清单会把卸载变成删错文件的入口。
	manifest := filepath.Join(dir, "install.manifest")
	if _, err := os.Stat(manifest); !os.IsNotExist(err) {
		t.Errorf("--no-service 不应写安装清单：err=%v", err)
	}

	// 请求体的关键字段。
	reqs, auths, hits := srv.requests()
	if hits != 1 {
		t.Fatalf("应只注册一次，实际 %d 次", hits)
	}
	req := reqs[0]
	if req.Code != "AB-12CD" {
		t.Errorf("注册码未做 trim+大写规范化，实际 %q", req.Code)
	}
	if req.OS != runtime.GOOS || req.Arch != runtime.GOARCH {
		t.Errorf("平台字段不对：%s/%s", req.OS, req.Arch)
	}
	if req.AgentVersion != "testver" {
		t.Errorf("AgentVersion 未透传，实际 %q", req.AgentVersion)
	}
	if req.CounterBits != counterBits() {
		t.Errorf("CounterBits 与本地判定不一致：%d vs %d", req.CounterBits, counterBits())
	}
	if req.ServiceManager != service.Detect().Name() {
		t.Errorf("ServiceManager 未上报：%q", req.ServiceManager)
	}
	// 注册请求不带令牌（此时还没有令牌）。
	if auths[0] != "" {
		t.Errorf("注册请求不应携带 Authorization，实际 %q", auths[0])
	}
}

// ---------------------------------------------------------------------------
// reenroll
// ---------------------------------------------------------------------------

func TestReenrollRequiresCode(t *testing.T) {
	code, _, errOut := runCLI(t, "reenroll")
	if code != 2 {
		t.Fatalf("缺少 --code 应返回 2，得到 %d", code)
	}
	if !strings.Contains(errOut, "--code") {
		t.Fatalf("stderr 未提示 --code 必需，实际：%q", errOut)
	}
}

func TestReenrollReplacesTokenAndKeepsConnectTargets(t *testing.T) {
	srv := newEnrollServer(t, "pk_rotated", 9, model.AgentConfig{
		ConfigVersion:   "v2",
		ReportIntervalS: 4,
		ProbeIntervalS:  30,
	})

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	pin := spkiPin(t, srv.Certificate())
	original := &agentconfig.Config{
		MasterURL:      srv.URL,
		Token:          "pk_old",
		SPKISHA256:     []string{pin},
		ServiceManager: "windows",
	}
	if err := agentconfig.Save(cfgPath, original); err != nil {
		t.Fatalf("准备配置失败：%v", err)
	}

	code, out, errOut := runCLI(t, "reenroll", "--code", "new-code", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("reenroll 失败：code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if !strings.Contains(out, "已换取新令牌") {
		t.Errorf("stdout 未报告换令牌成功：%q", out)
	}

	got, err := agentconfig.Load(cfgPath)
	if err != nil {
		t.Fatalf("重新读取配置失败：%v", err)
	}
	if got.Token != "pk_rotated" {
		t.Errorf("令牌未更新：%q", got.Token)
	}
	// 换令牌不允许改"连到谁"：否则注册码泄露就等于可以把节点劫持到攻击者的 Master。
	if got.MasterURL != srv.URL {
		t.Errorf("MasterURL 被改动：%q", got.MasterURL)
	}
	if len(got.SPKISHA256) != 1 || got.SPKISHA256[0] != pin {
		t.Errorf("SPKI 指纹被改动：%v", got.SPKISHA256)
	}
	if got.ServiceManager != "windows" {
		t.Errorf("ServiceManager 被改动：%q", got.ServiceManager)
	}
	if got.ConfigVersion != "v2" {
		t.Errorf("服务端配置未刷新：%q", got.ConfigVersion)
	}

	_, auths, hits := srv.requests()
	if hits != 1 {
		t.Fatalf("应只注册一次，实际 %d 次", hits)
	}
	if auths[0] != "" {
		t.Errorf("reenroll 用的也是注册码，不应带旧令牌，实际 %q", auths[0])
	}
}

func TestReenrollFailsWithoutConfigFile(t *testing.T) {
	dir := t.TempDir()
	code, _, errOut := runCLI(t, "reenroll", "--code", "abc", "--config", filepath.Join(dir, "missing.yaml"))
	if code != 1 {
		t.Fatalf("配置缺失应返回 1，得到 %d", code)
	}
	if !strings.Contains(errOut, "错误：") {
		t.Errorf("stderr 未报告错误，实际：%q", errOut)
	}
}

// ---------------------------------------------------------------------------
// run / uninstall
// ---------------------------------------------------------------------------

func TestRunFailsWithUnreadableConfig(t *testing.T) {
	dir := t.TempDir()
	code, _, errOut := runCLI(t, "run", "--config", filepath.Join(dir, "missing.yaml"))
	if code != 1 {
		t.Fatalf("配置缺失应返回 1，得到 %d", code)
	}
	if !strings.Contains(errOut, "错误：") {
		t.Errorf("stderr 未报告错误，实际：%q", errOut)
	}
}

func TestUninstallFailsWithUnreadableConfig(t *testing.T) {
	dir := t.TempDir()
	// --yes 也救不了：读不到配置就不知道要注销谁，必须先失败。
	code, _, _ := runCLI(t, "uninstall", "--yes", "--config", filepath.Join(dir, "missing.yaml"))
	if code != 1 {
		t.Fatalf("配置缺失应返回 1，得到 %d", code)
	}
}

// TestUninstallPromptDefaultsToCancel 验证"直接回车＝放弃"。
//
// 这条路径很值得钉住：卸载会删配置目录与二进制，一旦默认值变成"是"，
// 一次误敲回车就是不可逆的破坏。这里用文件冒充 stdin 喂一个空行，
// 并断言配置目录仍然完好。
func TestUninstallPromptDefaultsToCancel(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := agentconfig.Save(cfgPath, &agentconfig.Config{
		MasterURL: "https://127.0.0.1:1", Token: "pk_keepme",
	}); err != nil {
		t.Fatalf("准备配置失败：%v", err)
	}

	stdinFile := filepath.Join(dir, "stdin.txt")
	if err := os.WriteFile(stdinFile, []byte("\n"), 0o600); err != nil {
		t.Fatalf("准备 stdin 失败：%v", err)
	}
	f, err := os.Open(stdinFile)
	if err != nil {
		t.Fatalf("打开 stdin 失败：%v", err)
	}
	defer f.Close()

	oldIn := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = oldIn }()

	code, out, _ := runCLI(t, "uninstall", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("取消卸载应返回 0，得到 %d", code)
	}
	if !strings.Contains(out, "确认执行？") {
		t.Errorf("stdout 未出现确认提示，实际：%q", out)
	}
	if !strings.Contains(out, "已取消。") {
		t.Errorf("stdout 未报告已取消，实际：%q", out)
	}

	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("取消后配置不应被删除：%v", err)
	}
	cfg, err := agentconfig.Load(cfgPath)
	if err != nil {
		t.Fatalf("取消后配置不可读：%v", err)
	}
	if cfg.Token != "pk_keepme" {
		t.Errorf("取消后令牌被改动：%q", cfg.Token)
	}
}

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------

func TestResolvePathsWithExplicitDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath, binPath, err := resolvePaths(dir)
	if err != nil {
		t.Fatalf("指定 --dir 不应报错：%v", err)
	}
	if cfgPath != filepath.Join(dir, "agent.yaml") {
		t.Errorf("配置路径不对：%q", cfgPath)
	}
	if binPath != filepath.Join(dir, "probe-agent") {
		t.Errorf("二进制路径不对：%q", binPath)
	}
}

func TestResolvePathsDefaultsPerPlatform(t *testing.T) {
	// 缺省路径由 agentconfig 决定（12.4）；这里只钉"确实走了缺省分支"，
	// 具体字符串的权威断言在 agentconfig 的测试里。
	fakeProgramData := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("ProgramData", fakeProgramData)
	}

	cfgPath, binPath, err := resolvePaths("")
	if err != nil {
		t.Fatalf("Windows/root-less 默认路径不应报错：%v", err)
	}
	if cfgPath != agentconfig.DefaultPath() {
		t.Errorf("配置路径未采用平台默认：%q vs %q", cfgPath, agentconfig.DefaultPath())
	}
	if binPath != filepath.Join(agentconfig.DefaultBinaryDir(), "probe-agent") {
		t.Errorf("二进制路径未采用平台默认：%q", binPath)
	}
	if !filepath.IsAbs(cfgPath) || !filepath.IsAbs(binPath) {
		t.Errorf("默认路径必须是绝对路径：%q %q", cfgPath, binPath)
	}
}

func TestMultiFlagAccumulatesInOrder(t *testing.T) {
	var m multiFlag
	if got := m.String(); got != "" {
		t.Fatalf("空 multiFlag 的 String 应为空串，得到 %q", got)
	}
	for _, v := range []string{"first", "second", "third"} {
		if err := m.Set(v); err != nil {
			t.Fatalf("Set(%q) 报错：%v", v, err)
		}
	}
	if len(m) != 3 {
		t.Fatalf("应累积 3 个值，得到 %d", len(m))
	}
	if m[0] != "first" || m[2] != "third" {
		t.Errorf("顺序被破坏：%v", []string(m))
	}
	if got := m.String(); got != "first,second,third" {
		t.Errorf("String 应以逗号连接，得到 %q", got)
	}
}

func TestReadManifest(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	manifest := filepath.Join(dir, "install.manifest")

	t.Run("缺失时报错", func(t *testing.T) {
		if _, err := readManifest(cfgPath); err == nil {
			t.Fatal("清单不存在却未报错")
		}
	})

	t.Run("取首行且去空白", func(t *testing.T) {
		if err := os.WriteFile(manifest, []byte("  /opt/probe/probe-agent  \n/opt/probe/agent.yaml\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readManifest(cfgPath)
		if err != nil {
			t.Fatalf("读取失败：%v", err)
		}
		if got != "/opt/probe/probe-agent" {
			t.Fatalf("应取首行并 trim，得到 %q", got)
		}
	})

	t.Run("空文件返回空串", func(t *testing.T) {
		if err := os.WriteFile(manifest, []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readManifest(cfgPath)
		if err != nil {
			t.Fatalf("空清单不应报错：%v", err)
		}
		if got != "" {
			t.Fatalf("空清单应返回空串，得到 %q", got)
		}
	})
}

func TestHostnameIsNeverEmpty(t *testing.T) {
	h := hostname()
	if h == "" {
		t.Fatal("hostname 返回空串，注册请求会缺少主机名")
	}
	if real, err := os.Hostname(); err == nil && h != real {
		t.Errorf("hostname 与 os.Hostname 不一致：%q vs %q", h, real)
	}
}

func TestCounterBitsFollowsArchWidth(t *testing.T) {
	// 32 位架构清单来自文档 12.5：Master 需要据此判定计数器回绕，
	// 报错位宽会让 32 位节点的流量在 4 GiB 处被误判为"回绕重置"。
	// 注：armv7/armv6 不是合法 GOARCH（见 Makefile 注释），列在这里也无害——
	// 真正跑 32 位的构建是 GOARCH=arm + GOARM=7/6。
	thirtyTwo := map[string]bool{
		"386": true, "arm": true, "armv7": true, "armv6": true,
		"mips": true, "mipsle": true,
	}
	want := 64
	if thirtyTwo[runtime.GOARCH] {
		want = 32
	}

	got := counterBits()
	if got != want {
		t.Fatalf("%s 的计数器位宽应为 %d，得到 %d", runtime.GOARCH, want, got)
	}
	if got != 32 && got != 64 {
		t.Fatalf("位宽只能是 32 或 64，得到 %d", got)
	}
}

func TestInstallServiceSkipsManifestWhenNoService(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "probe-agent")
	cfgPath := filepath.Join(dir, "agent.yaml")

	out, _ := captureOutput(t, func() {
		if err := installService(binPath, cfgPath, true); err != nil {
			t.Errorf("--no-service 分支不应报错：%v", err)
		}
	})
	if !strings.Contains(out, "已跳过服务注册") {
		t.Errorf("stdout 未提示跳过服务注册，实际：%q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "install.manifest")); !os.IsNotExist(err) {
		t.Errorf("跳过服务注册却写了安装清单：err=%v", err)
	}
}
