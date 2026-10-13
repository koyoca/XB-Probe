package theme

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 外部主题的加载、路径穿越防护与热切换（5.3）。
//
// 为什么单独补这一块：主题是**唯一一个把磁盘目录暴露给外部请求**的入口，
// 而此前它只有"前端源码护栏"（unified_theme_test.go 那一类），没有任何行为测试。
// 冒烟脚本只覆盖了管理端路径（/admin/%2e%2e/master.yaml），**主题目录内**的
// `../` 与"外部主题热切换"两项一直是空的。

// writeTheme 在 themesDir 下建一个主题目录（含 theme.json 与入口文件）。
func writeTheme(t *testing.T, dir, name, html string) {
	t.Helper()
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("建主题目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "theme.json"),
		[]byte(`{"name":"`+name+`","version":"1.0.0","api_version":"v1","entry":"index.html"}`), 0o644); err != nil {
		t.Fatalf("写 theme.json 失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte(html), 0o644); err != nil {
		t.Fatalf("写入口文件失败：%v", err)
	}
}

// TestManagerOpenRefusesToEscapeThemeRoot 是这条链路最要紧的一条。
//
// 断言方式刻意做成**行为式**而不是"看 safeJoin 有没有返回 error"：
// `safeJoin` 对 `../x` 这类输入其实不会报错——`path.Clean("/" + rel)` 会把开头的
// 上跳直接吃掉（"../secret.txt" → "secret.txt"），所以它天然安全，只是读不到东西。
// 换句话说，**只断言"返回了 error"会得到假绿**（要么永远通过，要么永远失败）。
// 真正要钉住的是结果：无论怎么构造路径，都读不到主题目录之外的文件内容。
func TestManagerOpenRefusesToEscapeThemeRoot(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "t1", "<html>t1</html>")
	// 主题目录**之外**的敏感文件：穿越成功就会读到它。
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("MASTER-KEY-MATERIAL"), 0o644); err != nil {
		t.Fatalf("写敏感文件失败：%v", err)
	}

	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager 失败：%v", err)
	}

	const secret = "MASTER-KEY-MATERIAL"
	for _, evil := range []string{
		"../secret.txt",
		"../../secret.txt",
		"/../secret.txt",
		"sub/../../secret.txt",
		`..\secret.txt`, // Windows 分隔符也要一起吃掉
		"./../secret.txt",
	} {
		got, err := m.Open("t1", evil)
		if err == nil {
			t.Errorf("Open(%q) 竟然成功了：%q", evil, string(got))
			continue
		}
		if bytes.Contains(got, []byte(secret)) {
			t.Errorf("Open(%q) 读到了主题目录之外的文件：%q", evil, string(got))
		}
	}

	// 正向对照：主题内的正常文件必须读得到，否则"一律拒绝"也能通过上面那组断言。
	got, err := m.Open("t1", "index.html")
	if err != nil {
		t.Fatalf("主题内的正常文件读不到：%v", err)
	}
	if !bytes.Contains(got, []byte("t1")) {
		t.Errorf("读到的内容不对：%q", string(got))
	}
	if _, err := m.Open("t1", "css/app.css"); err == nil {
		t.Errorf("不存在的文件应返回错误（否则前端会把 200 空响应当成有效资源）")
	}
}

// safeJoin 的返回值绝不能带上跳段：这是纵深防御的最后一道，
// 即使将来有人改了 Clean 的方式，"结果不含 .."这条仍然成立。
func TestSafeJoinNeverReturnsAnEscapingPath(t *testing.T) {
	for _, in := range []string{
		"index.html", "css/app.css", "/css/app.css", "",
		"../secret.txt", "../../etc/passwd", "a/../../../b", "..", `..\..\windows\win.ini`,
	} {
		got, err := safeJoin(in)
		if err != nil {
			// 显式拒绝同样是合法结果（本实现靠规范化而非报错，但两者都安全）。
			continue
		}
		if strings.HasPrefix(got, "..") || strings.Contains(got, "/../") || strings.Contains(got, `\..`) {
			t.Errorf("safeJoin(%q) 返回了带目录上跳的路径 %q", in, got)
		}
		if filepath.IsAbs(got) {
			t.Errorf("safeJoin(%q) 返回了绝对路径 %q", in, got)
		}
	}
	// 空路径收敛到入口文件，否则主题根目录会读不到首页。
	if got, err := safeJoin(""); err != nil || got != "index.html" {
		t.Errorf("空路径应收敛到 index.html，实得 %q / err=%v", got, err)
	}
}

// TestManagerHotSwitchesActiveTheme 钉住"换主题不用重启"。
//
// 两条容易被改坏的地方：① 切换后 `Entry()` 必须跟着变（曾经存在"Active 变了但
// 读的还是旧主题"这类缓存不同步的写法）；② 切到不存在的主题必须**报错且保持原样**——
// 悄悄回退到 builtin 会让管理员以为切换成功了。
func TestManagerHotSwitchesActiveTheme(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "alpha", "<html>alpha</html>")
	writeTheme(t, dir, "beta", "<html>beta</html>")

	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager 失败：%v", err)
	}
	if m.Active() != "builtin" {
		t.Fatalf("默认主题应为 builtin，实得 %q", m.Active())
	}
	if !m.Exists("alpha") || !m.Exists("beta") {
		t.Fatalf("外部主题未被扫描到：%+v", m.List())
	}

	// 切到 alpha：Active 与 Entry 必须同步。
	if err := m.SetActive("alpha"); err != nil {
		t.Fatalf("切换主题失败：%v", err)
	}
	if m.Active() != "alpha" {
		t.Errorf("Active 未跟上切换：%q", m.Active())
	}
	ent, err := m.Entry("alpha")
	if err != nil {
		t.Fatalf("读 alpha 入口失败：%v", err)
	}
	if !bytes.Contains(ent, []byte("alpha")) {
		t.Errorf("alpha 入口内容不对：%q", string(ent))
	}

	// 再切一次：同一个 Manager 实例，不重建。
	if err := m.SetActive("beta"); err != nil {
		t.Fatalf("二次切换失败：%v", err)
	}
	if m.Active() != "beta" {
		t.Errorf("二次切换后 Active=%q", m.Active())
	}
	ent, err = m.Entry("beta")
	if err != nil {
		t.Fatalf("读 beta 入口失败：%v", err)
	}
	if !bytes.Contains(ent, []byte("beta")) {
		t.Errorf("beta 入口内容不对：%q", string(ent))
	}

	// 未知主题：必须报错，且当前主题保持不变（不能静默回退）。
	if err := m.SetActive("nope"); err == nil {
		t.Error("切到不存在的主题应返回错误")
	}
	if m.Active() != "beta" {
		t.Errorf("切换失败后当前主题被改成了 %q（应保留 beta）", m.Active())
	}

	// 列表顺序：内置在前，其余按名称排序（前端下拉框依赖这个顺序）。
	list := m.List()
	if len(list) != 3 {
		t.Fatalf("主题列表应有 3 项，实得 %d：%+v", len(list), list)
	}
	if !list[0].Builtin || list[0].Name != "builtin" {
		t.Errorf("内置主题应排在最前：%+v", list[0])
	}
	if list[1].Name != "alpha" || list[2].Name != "beta" {
		t.Errorf("外部主题应按名称排序：%+v / %+v", list[1], list[2])
	}
}

// 没有 themes_dir（或目录不存在）时必须只靠内置主题跑起来：
// 这是"二进制单文件分发"的前提——不能因为外部目录缺失就起不来。
func TestManagerWorksWithoutExternalDir(t *testing.T) {
	m, err := NewManager("")
	if err != nil {
		t.Fatalf("dir 为空时 NewManager 失败：%v", err)
	}
	if !m.Exists("builtin") || m.Active() != "builtin" {
		t.Errorf("内置主题不可用：active=%q", m.Active())
	}
	ent, err := m.Entry("builtin")
	if err != nil {
		t.Fatalf("读内置主题入口失败：%v", err)
	}
	if !bytes.Contains(ent, []byte("<html")) && !bytes.Contains(ent, []byte("<!DOCTYPE")) {
		t.Errorf("内置主题入口内容异常：%.80q", string(ent))
	}

	// 目录不存在（而不是为空）同样不能报错。
	m2, err := NewManager(filepath.Join(t.TempDir(), "no-such-dir"))
	if err != nil {
		t.Fatalf("主题目录不存在时 NewManager 失败：%v", err)
	}
	if !m2.Exists("builtin") {
		t.Error("主题目录不存在时内置主题应仍可用")
	}
}
