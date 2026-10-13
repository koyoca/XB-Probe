package dist

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// 一键卸载这条路径上只有两条性质是**必须**成立的，而它们都能在 dry-run 下验证：
//
//	① dry-run 绝不删任何东西；
//	② 只肯删"看起来是自己建的"路径。
//
// 用真实脚本跑一遍而不是断言脚本文本里出现了哪些字：这两条都是**行为**，
// 而行为恰恰是重构里最容易悄悄丢掉的。删错目录不可逆，这道闸值得用集成测试盯。
//
// 安全性：整组用例只跑 --dry-run，因此不会动到开发机上的任何东西；
// 被删的候选目录也全部在 t.TempDir() 里。

// runDeployScript 用 sh 跑一遍部署脚本，返回合并输出与退出码。
func runDeployScript(t *testing.T, args ...string) (string, int) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("找不到 sh，跳过（本护栏用真实脚本跑行为）")
	}
	cmd := exec.Command(sh, append([]string{"../../scripts/deploy-master.sh"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if !asExitError(err, &ee) {
			t.Fatalf("无法执行脚本：%v（输出：%s）", err, out)
		}
		return string(out), ee.ExitCode()
	}
	return string(out), 0
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// posixPath 把 Windows 盘符路径换成 MSYS/Cygwin 形式（C:\a\b → /c/a/b）。
//
// 被测的门禁要求"路径是绝对路径"。Windows 上 t.TempDir() 给的是 `C:\…`，
// bash 不把它当绝对路径 —— 不转换的话这条用例会在 Windows 上因为"路径不像
// 我们自己建的"而误红，于是它就变成了一条"只在 CI 上绿"的护栏：
// 那种护栏的坏处不是漏报，而是让人开始忽略它的失败。
func posixPath(t *testing.T, p string) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return p
	}
	q := filepath.ToSlash(p)
	if len(q) < 3 || q[1] != ':' {
		t.Fatalf("无法把 %q 转成 POSIX 形式", p)
	}
	return "/" + strings.ToLower(q[:1]) + q[2:]
}

// dry-run 只说不做：一条命令把要删的东西列出来，而系统一个字节都不动。
func TestUninstallDryRunDeletesNothing(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "xb-probe-master-data")
	conf := filepath.Join(base, "xb-probe-master-conf")
	if err := os.MkdirAll(filepath.Join(data, "themes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	// master.key 是"丢了就再也解不开告警渠道凭证"的那个文件，拿它当探针最合适。
	marker := filepath.Join(data, "probe.db")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := runDeployScript(t, "--uninstall", "--dry-run",
		"--data", posixPath(t, data), "--dir", posixPath(t, conf))
	if code != 0 {
		t.Fatalf("--uninstall --dry-run 应当成功退出，实得 rc=%d\n%s", code, out)
	}
	if !strings.Contains(out, "[dry-run]") {
		t.Errorf("输出里应当逐项标注这是 dry-run，实际：\n%s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("dry-run 竟然删掉了数据文件（%s）：%v", marker, err)
	}
	if _, err := os.Stat(conf); err != nil {
		t.Fatalf("dry-run 竟然删掉了配置目录：%v", err)
	}
}

// 只肯删"看起来是自己建的"目录。
//
// 这条闸看着多余——--data 不就是用户自己给的吗？但它是**可传参**的，
// 一个手滑的 `--data /var/lib` 会把整个目录清空。这里的判据不是"用户确认过"，
// 而是"这个路径像不像本脚本创建的"：不像就拒绝，把 rm -rf 留给用户自己决定。
func TestUninstallRefusesPathsThatAreNotOurs(t *testing.T) {
	base := t.TempDir()
	foreign := filepath.Join(base, "not-ours")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(foreign, "keep.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := runDeployScript(t, "--uninstall", "--dry-run", "--data", posixPath(t, foreign))
	if code == 0 {
		t.Fatalf("对不含 probe 的路径必须拒绝，实得 rc=0\n%s", out)
	}
	if !strings.Contains(out, "拒绝删除") {
		t.Errorf("失败原因应当是明确的「拒绝删除」，而不是别的什么：\n%s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("拒绝之后文件却不见了：%v", err)
	}
}

// 顶层系统目录一律拒绝——哪怕名字里含 probe 也不能例外：
// /probe 这种路径删掉就是整棵树。
//
// `/probe` 这条用例是**唯一能区分两种实现**的那一条：`/etc`、`/usr`、`/var`
// 光靠"必须含 probe"就已经被拒了，把它们放进列表看着热闹，删掉判据也照样绿
// ——那种用例不是护栏，是"看起来有护栏"。真正需要断言的是那个被
// `/*probe*`（两个 `*` 都能匹配空）顺带放过的单段路径。
func TestUninstallRefusesSystemRoots(t *testing.T) {
	for _, p := range []string{"/", "/etc", "/usr", "/var", "/probe", "/xb-probe-master"} {
		out, code := runDeployScript(t, "--uninstall", "--dry-run", "--data", p)
		if code == 0 {
			t.Errorf("必须拒绝 %q，实得 rc=0\n%s", p, out)
		}
	}
}

// 独立入口别"反着来"：uninstall-master.sh 取回部署脚本后必须带上 --uninstall，
// 否则它会把一次卸载悄悄变成一次**安装**——最坏的一种失败，用户以为删干净了。
func TestStandaloneUninstallerActuallyUninstalls(t *testing.T) {
	body := readRepoFile(t, "../../uninstall-master.sh")
	if !strings.Contains(body, `sh "$DEPLOY" --uninstall`) {
		t.Error("uninstall-master.sh 没有把 --uninstall 传给部署脚本——" +
			"这会让一条卸载命令变成一次安装，而用户以为系统已经清干净了")
	}
}

// Windows 侧的两个入口（install-master.ps1 / uninstall-master.ps1）走的是同一件事，
// 只是没有 `set --` 可抄，于是换成往数组里塞开关、再用 @name 展开调用。
//
// 这里刻意断言**"开关与调用用的是同一个数组"**，而不是"文件里出现过 -Uninstall"：
// 后者完全可能与实现脱节——开关被放进 `$dargs`、调用却写成 `-File $deploy`（漏展开），
// 于是子脚本拿不到 -Uninstall。那时它的 -Bin/-Dir 校验会把它送进**安装**分支，
// 用户看到"部署成功"，以为卸载完了。这一条正是为了不让这种一致性只存在于字面上。
//
// 断言用正则抽变量名而不是写死 `$dargs`：改名不该让护栏失效（失效的护栏比没有更糟）。
func TestWindowsInstallersForwardUninstall(t *testing.T) {
	reAssign := regexp.MustCompile(`\$(\w+)\s*=\s*@\(\s*"-Uninstall"\s*\)`)
	reSplat := regexp.MustCompile(`-File\s+\$\w+\s+@(\w+)`)

	for _, rel := range []string{"../../install-master.ps1", "../../uninstall-master.ps1"} {
		body := readRepoFile(t, rel)

		m := reAssign.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("%s 里没有把 -Uninstall 收进参数数组（判据已与脚本写法脱节，"+
				"或者这条卸载通路被改坏了）", rel)
			continue
		}
		bound := m[1]

		s := reSplat.FindStringSubmatch(body)
		if s == nil {
			t.Errorf("%s 调用部署脚本时没有用 @变量 展开参数数组（判据脱节或改坏）："+
				"-Uninstall 会被丢掉，一次卸载会变成一次安装", rel)
			continue
		}
		if s[1] != bound {
			t.Errorf("%s：-Uninstall 收进了 $%s，调用时展开的却是 $%s——"+
				"两个名字不一致时脚本一个字都不会报错，只是卸载变成了安装", rel, bound, s[1])
		}

		// 每个开关都必须"条件变量与开关名对得上"地并进同一个数组：
		//     if ($KeepData) { $dargs += "-KeepData" }
		//
		// 只断言"这一行存在"挡不住 `if ($false) { $dargs += "-KeepData" }`——
		// 文本还在、条件已经死了，开关被静默吞掉，而用户以为它生效了。
		reArm := regexp.MustCompile(`if\s*\(\s*\$(\w+)\s*\)\s*\{\s*\$\w+\s*\+=\s*"(-[A-Za-z]+)"\s*\}`)
		armed := map[string]bool{}
		for _, a := range reArm.FindAllStringSubmatch(body, -1) {
			if !strings.EqualFold("-"+a[1], a[2]) {
				t.Errorf("%s：条件 $%s 与开关 %s 对不上——多半是复制粘贴改了名字，这个开关不会生效",
					rel, a[1], a[2])
			}
			armed[a[2]] = true
		}
		for _, sw := range []string{"-KeepData", "-DryRun"} {
			if !armed[sw] {
				t.Errorf("%s：%s 没有被并进参数数组，这个开关会被静默吞掉——"+
					"用户以为数据保住了（或以为只做了预览），实际没有", rel, sw)
			}
		}
	}
}
