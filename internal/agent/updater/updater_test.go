package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koyoca/xb-probe/internal/agent/client"
)

// 自升级的测试用一个真实起在本地回环上的 HTTP 服务。
//
// 为什么不用假 client 接口：这条链路的坑全在"字节落盘的顺序"上——
// 先校验后改名、改名失败要还原、校验失败不能留下半成品。
// 把这些替换成内存里的假实现，测的就不是真流程了。
//
// 用 http（回环 + InsecureHTTP）而不是 TLS：证书固定由 client 自己的测试覆盖，
// 这里重复一遍只会让失败原因更难读。

type fakeMaster struct {
	srv      *httptest.Server
	version  string
	artifact string
	body     string
	// badChecksum 让响应头故意给出错的校验和（模拟投毒 / 传输损坏）。
	badChecksum bool
	// noChecksum 让响应头不带校验和（模拟服务端没配 SHA256SUMS）。
	noChecksum bool
	// listSHA 是"清单接口"返回的校验和；与下发的不一致用于验证交叉校验。
	listSHA string
	// upToDate 强制清单接口说"已是最新"。
	upToDate bool
	// 记录收到的请求，用于断言 platform/version 传对了。
	gotReleaseQuery  string
	gotArtifactQuery string
}

func newFakeMaster(t *testing.T, body string) *fakeMaster {
	t.Helper()
	fm := &fakeMaster{version: "1.2.0", artifact: "xb-probe-agent_linux_amd64", body: body}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/v1/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fm.gotReleaseQuery = r.URL.RawQuery
		sha := fm.listSHA
		upToDate := fm.upToDate
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"version":"` + fm.version + `","artifact":"` + fm.artifact +
			`","sha256":"` + sha + `","size":` + itoa(len(fm.body)) +
			`,"current":"","up_to_date":` + boolStr(upToDate) + `}`))
	})
	mux.HandleFunc("/api/agent/v1/artifact", func(w http.ResponseWriter, r *http.Request) {
		fm.gotArtifactQuery = r.URL.RawQuery
		sum := sha256.Sum256([]byte(fm.body))
		switch {
		case fm.noChecksum:
		case fm.badChecksum:
			w.Header().Set("X-Checksum-Sha256", strings.Repeat("a", 64))
		default:
			w.Header().Set("X-Checksum-Sha256", hex.EncodeToString(sum[:]))
		}
		w.Header().Set("X-Artifact-Version", fm.version)
		w.Write([]byte(fm.body))
	})
	fm.srv = httptest.NewServer(mux)
	t.Cleanup(fm.srv.Close)
	return fm
}

func (fm *fakeMaster) client(t *testing.T) *client.Client {
	t.Helper()
	c, err := client.New(client.Options{
		MasterURL: fm.srv.URL, Token: "pk_testtoken", InsecureHTTP: true,
	})
	if err != nil {
		t.Fatalf("建客户端失败：%v", err)
	}
	c.SetToken("pk_testtoken")
	return c
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// target 造一个"当前正在运行的二进制"。
func target(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "xb-probe-agent")
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatalf("写目标文件失败：%v", err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", p, err)
	}
	return string(b)
}

// ---- Check ----

func TestCheckReportsLatestAndPassesPlatform(t *testing.T) {
	fm := newFakeMaster(t, "new-bytes")
	sum := sha256.Sum256([]byte("new-bytes"))
	fm.listSHA = hex.EncodeToString(sum[:])

	rel, err := Check(context.Background(), fm.client(t), "linux_amd64", "1.1.0")
	if err != nil {
		t.Fatalf("Check 失败：%v", err)
	}
	if rel.Version != "1.2.0" || rel.Artifact != "xb-probe-agent_linux_amd64" || rel.SHA256 != fm.listSHA {
		t.Errorf("Release 不对：%+v", rel)
	}
	if rel.UpToDate {
		t.Error("up_to_date=false 的响应应如实返回 false")
	}
	if !strings.Contains(fm.gotReleaseQuery, "platform=linux_amd64") {
		t.Errorf("platform 未传对：%q", fm.gotReleaseQuery)
	}
	if !strings.Contains(fm.gotReleaseQuery, "current=1.1.0") {
		t.Errorf("current 未传对：%q", fm.gotReleaseQuery)
	}
}

// 缺 platform 必须报错：它会直接进查询串与产物名，
// 空值会让服务端返回一个"说不清是什么"的答案。
func TestCheckRequiresPlatform(t *testing.T) {
	fm := newFakeMaster(t, "x")
	fm.listSHA = strings.Repeat("b", 64)
	if _, err := Check(context.Background(), fm.client(t), "", "1.0.0"); err == nil {
		t.Error("缺少 platform 应报错")
	}
}

// 版本或校验和缺失时不能"当作最新"——那会让升级永远静默跳过，
// 而运维以为自动更新在跑。
func TestCheckRejectsIncompleteRelease(t *testing.T) {
	fm := newFakeMaster(t, "x")
	fm.version = ""
	fm.listSHA = strings.Repeat("b", 64)
	if _, err := Check(context.Background(), fm.client(t), "linux_amd64", "1.0.0"); err == nil {
		t.Error("缺版本号应报错")
	}
}

// ---- Upgrade ----

func TestUpgradeReplacesBinaryAndKeepsBackup(t *testing.T) {
	fm := newFakeMaster(t, "new-bytes")
	sum := sha256.Sum256([]byte("new-bytes"))
	fm.listSHA = hex.EncodeToString(sum[:])
	p := target(t, "old-bytes")

	res, err := Upgrade(context.Background(), fm.client(t), Options{
		Platform: "linux_amd64", Current: "1.1.0", Target: p,
	})
	if err != nil {
		t.Fatalf("Upgrade 失败：%v", err)
	}
	if res.NoChange || res.From != "1.1.0" || res.To != "1.2.0" {
		t.Errorf("结果不对：%+v", res)
	}
	if got := readFile(t, p); got != "new-bytes" {
		t.Errorf("目标文件未被替换：%q", got)
	}
	if got := readFile(t, p+".old"); got != "old-bytes" {
		t.Errorf("备份内容不对：%q", got)
	}
	// 临时文件必须被改名消耗掉，不能留在目录里。
	if _, err := os.Stat(p + ".new"); err == nil {
		t.Error(".new 残留（说明不是原子改名，而是复制后忘了清理）")
	}
	if !strings.Contains(fm.gotArtifactQuery, "version=1.2.0") {
		t.Errorf("取件请求未带上版本：%q", fm.gotArtifactQuery)
	}
}

// 校验和不匹配：既不能替换，也不能留下半成品。
// 这条是"先校验后落地"的护栏——少了它，一次传输损坏就会把 Agent 换成跑不起来的文件。
func TestUpgradeRefusesBadChecksumAndLeavesNothingBehind(t *testing.T) {
	fm := newFakeMaster(t, "new-bytes")
	sum := sha256.Sum256([]byte("new-bytes"))
	fm.listSHA = hex.EncodeToString(sum[:])
	fm.badChecksum = true
	p := target(t, "old-bytes")

	if _, err := Upgrade(context.Background(), fm.client(t), Options{
		Platform: "linux_amd64", Current: "1.1.0", Target: p,
	}); err == nil {
		t.Fatal("校验和不匹配应报错")
	}
	if got := readFile(t, p); got != "old-bytes" {
		t.Errorf("目标文件被改动了：%q", got)
	}
	if _, err := os.Stat(p + ".new"); err == nil {
		t.Error("校验失败后 .new 必须删掉（不留半成品）")
	}
	if _, err := os.Stat(p + ".old"); err == nil {
		t.Error("校验失败不该产生备份")
	}
}

// 内容哈希与"清单/响应头"都对不上时，必须由**哈希比对**自己拦住，
// 并且删掉半成品。
//
// 为什么要单独一条：上面那条 badChecksum 用例让"清单"与"响应头"不一致，
// 于是会先在**交叉校验**处返回，`updater.go` 里"哈希不匹配 → 删 .new"
// 那几行根本轮不到执行——把它删掉也不会变红。这条用例让两处给出一致
// （但都错）的校验和，交叉校验因此通过，把校验推进到内容比对这一步。
func TestUpgradeRefusesContentHashMismatchAndLeavesNothingBehind(t *testing.T) {
	fm := newFakeMaster(t, "new-bytes")
	wrong := strings.Repeat("a", 64)
	fm.listSHA = wrong
	fm.badChecksum = true // 响应头也给 aaaa…（与清单一致）
	p := target(t, "old-bytes")

	_, err := Upgrade(context.Background(), fm.client(t), Options{
		Platform: "linux_amd64", Current: "1.1.0", Target: p,
	})
	if err == nil {
		t.Fatal("内容哈希与校验和对不上时应拒绝安装")
	}
	// 断言是**哪一类**错误：若只断言"报错了"，交叉校验那条路径也能满足它，
	// 这条用例就退化成了上一条的复制品。
	if !strings.Contains(err.Error(), "校验和不匹配") {
		t.Errorf("应报「内容哈希不匹配」，实得：%v", err)
	}
	if got := readFile(t, p); got != "old-bytes" {
		t.Errorf("目标文件被改动了：%q", got)
	}
	if _, err := os.Stat(p + ".new"); err == nil {
		t.Error("内容哈希不匹配后 .new 必须删掉（不留半成品）")
	}
	if _, err := os.Stat(p + ".old"); err == nil {
		t.Error("校验失败不该产生备份")
	}
}

// 服务端没给校验和时必须拒绝安装：没有期望值就等于没有校验。
func TestUpgradeRefusesMissingChecksum(t *testing.T) {
	fm := newFakeMaster(t, "new-bytes")
	sum := sha256.Sum256([]byte("new-bytes"))
	fm.listSHA = hex.EncodeToString(sum[:])
	fm.noChecksum = true
	p := target(t, "old-bytes")

	// 注意 err 的声明必须在 if 之外：写在 if 的初始化语句里，
	// 作用域就只剩那个 if，下面断言错误信息时反而编译不过。
	_, err := Upgrade(context.Background(), fm.client(t), Options{
		Platform: "linux_amd64", Current: "1.1.0", Target: p,
	})
	if err == nil {
		t.Fatal("缺少校验和应拒绝安装")
	}
	// 必须断言**错误信息**：只断言"报错了"会得到假绿——
	// 把那道检查删掉之后，后续的哈希比对同样会失败（期望值是空串），
	// 于是"看起来仍然报错"，实际上"缺校验和"这条守卫已经没了。
	if !strings.Contains(err.Error(), "未返回校验和") {
		t.Errorf("错误应明确指出是缺少校验和，实得：%v", err)
	}
	if got := readFile(t, p); got != "old-bytes" {
		t.Errorf("目标文件被改动了：%q", got)
	}
}

// 清单与下发两处校验和不一致：典型的投毒形态。
func TestUpgradeRefusesWhenManifestAndArtifactDisagree(t *testing.T) {
	fm := newFakeMaster(t, "new-bytes")
	fm.listSHA = strings.Repeat("c", 64) // 与真实下发的不一致
	p := target(t, "old-bytes")

	if _, err := Upgrade(context.Background(), fm.client(t), Options{
		Platform: "linux_amd64", Current: "1.1.0", Target: p,
	}); err == nil {
		t.Fatal("两处校验和不一致应拒绝安装")
	}
	if got := readFile(t, p); got != "old-bytes" {
		t.Errorf("目标文件被改动了：%q", got)
	}
}

// 已是最新 → 不做任何改动（含不产生备份）。
// 反过来：如果这里去下载并替换一遍，定时巡检会变成每分钟重写一次二进制。
func TestUpgradeNoChangeWhenUpToDate(t *testing.T) {
	fm := newFakeMaster(t, "new-bytes")
	sum := sha256.Sum256([]byte("new-bytes"))
	fm.listSHA = hex.EncodeToString(sum[:])
	fm.upToDate = true
	p := target(t, "old-bytes")

	res, err := Upgrade(context.Background(), fm.client(t), Options{
		Platform: "linux_amd64", Current: "1.2.0", Target: p,
	})
	if err != nil {
		t.Fatalf("Upgrade 失败：%v", err)
	}
	if !res.NoChange {
		t.Errorf("应判为无需改动：%+v", res)
	}
	if got := readFile(t, p); got != "old-bytes" {
		t.Errorf("目标文件不应被改动：%q", got)
	}
}

// 指定版本时无视 up_to_date（运维显式要求重装/降级）。
func TestUpgradeHonorsPinnedVersionOverUpToDate(t *testing.T) {
	fm := newFakeMaster(t, "pinned-bytes")
	sum := sha256.Sum256([]byte("pinned-bytes"))
	fm.listSHA = hex.EncodeToString(sum[:])
	fm.upToDate = true
	p := target(t, "old-bytes")

	res, err := Upgrade(context.Background(), fm.client(t), Options{
		Platform: "linux_amd64", Current: "1.2.0", Version: "1.1.0", Target: p,
	})
	if err != nil {
		t.Fatalf("Upgrade 失败：%v", err)
	}
	if res.NoChange || res.To != "1.1.0" {
		t.Errorf("指定版本应生效：%+v", res)
	}
	if got := readFile(t, p); got != "pinned-bytes" {
		t.Errorf("目标文件未按指定版本替换：%q", got)
	}
}

// dry-run：下载并校验通过，但不替换、不留文件。
func TestUpgradeDryRunDoesNotReplace(t *testing.T) {
	fm := newFakeMaster(t, "new-bytes")
	sum := sha256.Sum256([]byte("new-bytes"))
	fm.listSHA = hex.EncodeToString(sum[:])
	p := target(t, "old-bytes")

	res, err := Upgrade(context.Background(), fm.client(t), Options{
		Platform: "linux_amd64", Current: "1.1.0", Target: p, DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry-run 失败：%v", err)
	}
	if res.Size != int64(len("new-bytes")) {
		t.Errorf("dry-run 应报告下载字节数：%+v", res)
	}
	if got := readFile(t, p); got != "old-bytes" {
		t.Errorf("dry-run 不应替换：%q", got)
	}
	if _, err := os.Stat(p + ".new"); err == nil {
		t.Error("dry-run 后不应留下 .new")
	}
}

func TestUpgradeRequiresTargetAndPlatform(t *testing.T) {
	fm := newFakeMaster(t, "x")
	fm.listSHA = strings.Repeat("b", 64)
	if _, err := Upgrade(context.Background(), fm.client(t), Options{Platform: "linux_amd64"}); err == nil {
		t.Error("缺 target 应报错")
	}
	if _, err := Upgrade(context.Background(), fm.client(t), Options{Target: "x"}); err == nil {
		t.Error("缺 platform 应报错")
	}
}

// ---- Rollback ----

// 升级后新版本起不来，是运维最需要的一条退路。
func TestRollbackRestoresPreviousBinary(t *testing.T) {
	p := target(t, "bad-new-bytes")
	if err := os.WriteFile(p+".old", []byte("good-old-bytes"), 0o755); err != nil {
		t.Fatalf("写备份失败：%v", err)
	}
	if err := Rollback(p); err != nil {
		t.Fatalf("Rollback 失败：%v", err)
	}
	if got := readFile(t, p); got != "good-old-bytes" {
		t.Errorf("回滚后内容不对：%q", got)
	}
	// 坏的那个留成 .broken 供复盘，而不是直接删掉。
	if got := readFile(t, p+".broken"); got != "bad-new-bytes" {
		t.Errorf(".broken 应保留坏掉的那个：%q", got)
	}
}

func TestRollbackWithoutBackupFailsLoudly(t *testing.T) {
	p := target(t, "x")
	if err := Rollback(p); err == nil {
		t.Error("没有备份时应报错（而不是「什么都不做」假装成功）")
	}
	if got := readFile(t, p); got != "x" {
		t.Errorf("回滚失败不该动原文件：%q", got)
	}
}
