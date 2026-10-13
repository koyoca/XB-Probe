package dist

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 解析规则是"校验是否真的生效"的前提，所以每条都写成"必须拒绝"的断言：
// 一条非法行被当成有效校验和，表现就是"所有文件都校验通过"——
// 而那恰恰是攻击者最容易利用的形态。
func TestParseSumsRejectsNonHex(t *testing.T) {
	body := []byte(strings.Join([]string{
		"# 注释行",
		"",
		"not-a-hash  xb-probe-agent_linux_amd64",
		"zz" + strings.Repeat("z", 62) + "  xb-probe-agent_windows_amd64.exe", // 长度够但不是十六进制
		strings.Repeat("a", 63) + "  xb-probe-agent_freebsd_amd64",            // 十六进制但只有 63 位
		"deadbeef  xb-probe-agent_linux_arm64",                                // 太短
		"onlyonefield",
	}, "\n"))
	if got := ParseSums(body); len(got) != 0 {
		t.Errorf("非法行不应产生任何校验和条目，实得 %+v", got)
	}
}

func TestParseSumsNormalizesNamesAndModes(t *testing.T) {
	want := sha256.Sum256([]byte("x"))
	hashHex := hex.EncodeToString(want[:])
	body := hashHex + "  ./xb-probe-agent_linux_amd64\n" +
		strings.ToUpper(hashHex) + " *xb-probe-agent_windows_amd64.exe\n" +
		hashHex + "  xb-probe-agent_freebsd_amd64\n"

	got := ParseSums([]byte(body))
	if got["xb-probe-agent_linux_amd64"] != hashHex {
		t.Errorf("带 ./ 前缀的文件名应归一化，实得 %+v", got)
	}
	if got["xb-probe-agent_windows_amd64.exe"] != hashHex {
		t.Errorf("binary 模式（* 前缀）与大写哈希都应接受，实得 %+v", got)
	}
	if len(got) != 3 {
		t.Errorf("应解析出 3 条，实得 %d：%+v", len(got), got)
	}
}

// 构件名必须与 Makefile 的 dist 目标逐字一致，否则"服务端有这个文件、
// Agent 却去下载另一个名字"会变成上线当天才暴露的 404。
func TestArtifactNameMatchesReleaseNaming(t *testing.T) {
	cases := map[string]string{
		"linux_amd64":   "xb-probe-agent_linux_amd64",
		"windows_amd64": "xb-probe-agent_windows_amd64.exe",
		"linux_armv7":   "xb-probe-agent_linux_armv7",
		"freebsd_arm":   "xb-probe-agent_freebsd_arm",
		"windows_386":   "xb-probe-agent_windows_386.exe",
		"linux_riscv64": "xb-probe-agent_linux_riscv64",
	}
	for tag, want := range cases {
		osName := tag[:strings.Index(tag, "_")]
		if got := ArtifactName(osName, tag[strings.Index(tag, "_")+1:]); got != want {
			t.Errorf("ArtifactName(%s) = %q，期望 %q", tag, got, want)
		}
	}
}

// 未注入 PlatformTag 时不得 panic，且要给出可解析的 os_tag 形式
// （服务端会据此回一个明确的"该平台未发布此版本"）。
func TestCurrentTagFallsBackToRuntime(t *testing.T) {
	old := PlatformTag
	defer func() { PlatformTag = old }()

	PlatformTag = "linux_armv6"
	if got := CurrentTag(); got != "linux_armv6" {
		t.Errorf("注入值应优先生效，实得 %q", got)
	}
	if got := SelfArtifactName(); got != "xb-probe-agent_linux_armv6" {
		t.Errorf("SelfArtifactName = %q", got)
	}

	PlatformTag = ""
	tag := CurrentTag()
	if !strings.Contains(tag, "_") {
		t.Errorf("回退标签应形如 <os>_<arch>，实得 %q", tag)
	}
}

func TestSHA256FileMatchesKnownDigest(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bin")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	want := sha256.Sum256([]byte("hello"))
	got, err := SHA256File(p)
	if err != nil {
		t.Fatalf("SHA256File 失败：%v", err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("SHA256File = %q，期望 %q", got, hex.EncodeToString(want[:]))
	}
	if _, err := SHA256File(filepath.Join(dir, "nope")); err == nil {
		t.Error("不存在的文件应返回错误（否则「校验失败」会被当成「没有这个文件」）")
	}
}
