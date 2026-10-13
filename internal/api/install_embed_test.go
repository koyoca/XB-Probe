package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 安装脚本存在**两份物理副本**，这是本仓库一个真实踩过的坑：
//
//	scripts/install.sh            运维/文档里给人看的、也可直接下载用
//	internal/api/scripts/install.sh  go:embed 真正分发给节点的、被 /install.sh 提供
//
// go:embed 只认**包目录下**的文件，所以不能直接嵌 ../../scripts/install.sh
// （embed 不允许 `..`）。于是只能复制一份，而复制就一定会漂移——
// 第 6.8 项就发生过：给 scripts/install.sh 加了 minisign 验签与
// __MINISIGN_PUB__ 占位，但**忘了同步内嵌副本**，结果是
// "本地脚本改好了、线上下发的还是旧脚本"，且没有任何报错，
// 只有真去节点上装才会发现验签根本没生效。
//
// 这个测试把"两份必须逐字节一致"钉死：只要有人改了其中一份忘了另一份，
// `go test` 立刻红，而不是等到部署时才发现。
//
// 修复方式（二选一）：
//   - 手动复制：cp scripts/install.sh internal/api/scripts/install.sh
//   - 或跑 go generate ./internal/api/...（见 install.go 里的 //go:generate）
func TestEmbeddedInstallScriptsMatchSource(t *testing.T) {
	pairs := []struct {
		src   string // 仓库根下的权威副本
		embed string // 内嵌副本（相对本包）
	}{
		{"../../scripts/install.sh", "scripts/install.sh"},
		{"../../scripts/install.ps1", "scripts/install.ps1"},
	}
	for _, p := range pairs {
		want, err := os.ReadFile(filepath.Clean(p.src))
		if err != nil {
			t.Fatalf("读取权威副本 %s 失败：%v", p.src, err)
		}
		got, err := os.ReadFile(filepath.Clean(p.embed))
		if err != nil {
			t.Fatalf("读取内嵌副本 %s 失败：%v", p.embed, err)
		}
		if string(want) != string(got) {
			t.Errorf("%s 与 %s 不一致——改了其中一份要同步另一份（cp %s %s）",
				p.embed, p.src, strings.TrimPrefix(p.src, "../../"), p.embed)
		}
	}
}

// 内嵌副本必须真的被 go:embed 读进来（防止有人误删文件后 embed 变成空串）。
func TestEmbeddedInstallScriptReadable(t *testing.T) {
	for _, name := range []string{"scripts/install.sh", "scripts/install.ps1"} {
		b, err := installScripts.ReadFile(name)
		if err != nil {
			t.Fatalf("内嵌脚本 %s 不可读：%v", name, err)
		}
		if len(b) == 0 {
			t.Errorf("内嵌脚本 %s 是空的", name)
		}
	}
}

// 下发的 install.sh 必须带上 __MINISIGN_PUB__ 占位符，
// 否则 serveInstallScript 的替换无事可做，公钥永远注入不进去。
func TestInstallShHasMinisignPlaceholder(t *testing.T) {
	b, err := installScripts.ReadFile("scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"__MINISIGN_PUB__", "__MASTER_URL__", "__DIST_BASE__", "__SPKI__"} {
		if !strings.Contains(s, want) {
			t.Errorf("install.sh 缺少占位符 %s（serveInstallScript 的替换会静默失效）", want)
		}
	}
}

// PowerShell 脚本的编码要求是**两头相反**的，这条测试把两侧都钉住。
//
// 同一个 install.ps1 有两个消费者：
//
//	irm https://<master>/install.ps1 | & ([scriptblock]::Create(...))   线上，经 HTTP 分发
//	powershell -ExecutionPolicy Bypass -File scripts\install.ps1        本机，按文件执行
//
// 而 Windows PowerShell 5.1（`powershell`，不是 `pwsh`）读 *.ps1 时**只看 BOM**
// 判断编码，没有 BOM 就按当前 ANSI 代码页解码。中文 Windows 是 GBK，文件是 UTF-8，
// 解码错位后某个汉字的尾字节会吞掉紧跟其后的引号——实测整份脚本报十来处
// 「表达式或语句中包含意外的标记」而**根本跑不起来**，且与脚本逻辑毫无关系。
//
// 但 HTTP 那侧不能带 BOM：脚本块构造 API 会把开头的 U+FEFF 当成第一个标识符的
// 一部分，实测报「无法将“\uFEFFWrite-Output”项识别为 cmdlet」——官方那一行安装命令直接失败。
//
// 所以规则是：**磁盘副本必须带 BOM，分发出去的文本必须没有 BOM**
// （抹平差异的地方在 serveInstallScript）。两侧都是静默失效，都要断言。
func TestInstallPS1BOMOnDiskButNotOnTheWire(t *testing.T) {
	raw, err := installScripts.ReadFile("scripts/install.ps1")
	if err != nil {
		t.Fatalf("读取内嵌 install.ps1 失败：%v", err)
	}
	if !bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		t.Errorf("磁盘上的 install.ps1 丢了 UTF-8 BOM——" +
			"中文 Windows 上 PowerShell 5.1 会按 GBK 解码，脚本语法错误、完全无法执行")
	}

	h, _ := newTOTPTestHandler(t)
	h.InstallMode = func() string { return "always" }
	h.MasterURL = func() string { return "https://probe.example.com" }

	rec := httptest.NewRecorder()
	h.handleInstallPS1(rec, httptest.NewRequest(http.MethodGet, "/install.ps1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /install.ps1 = %d，期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.HasPrefix(body, "\ufeff") {
		t.Errorf("分发的 install.ps1 开头带了 U+FEFF——" +
			"一行式在线安装会把 BOM 当成标识符，命令会直接失败")
	}
	// 顺手钉住"替换真的发生了"：返回的必须是渲染后的脚本，不是带占位符的模板。
	if strings.Contains(body, "__MASTER_URL__") || strings.Contains(body, "__SPKI__") {
		t.Errorf("占位符没有被替换，节点会拿到一份模板脚本")
	}
	if !strings.Contains(body, "https://probe.example.com") {
		t.Errorf("Master 地址没有注入到脚本里")
	}
}
