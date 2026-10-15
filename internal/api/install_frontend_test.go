package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 一键安装命令的**前端**护栏。
//
// 用户报的原始现象："不管选择哪个系统，linux、windows、还是无 curl 环境，
// 都是把 4 个命令都出来了"——前端把服务端返回的 commands 全量渲染，
// 选择器形同虚设。这一层只能靠源码级判据盯住（DOM 渲染行为不在 HTTP 冒烟范围）。

// TestInstallCommandRendersOnlySelectedPlatform 钉"只渲染所选平台那一条"。
func TestInstallCommandRendersOnlySelectedPlatform(t *testing.T) {
	js := readSrc(t, "adminui/admin.js")
	// 切出安装命令面板的渲染段。
	seg := regionOf(t, js, "inst.appendChild(el('h3', null, '一键安装命令", "inst.appendChild(irow)")

	// 旧的实现是遍历全部平台键并逐个输出。
	if strings.Contains(seg, "['shell', 'wget', 'powershell', 'step_by_step'].forEach") {
		t.Fatal("安装命令仍在遍历全部平台并全量渲染：选择器形同虚设，" +
			"用户会把 powershell 那条复制到 Linux 上")
	}
	// 新实现必须按所选平台取单条。
	if !strings.Contains(seg, "var cmd = r.commands && r.commands[want]") {
		t.Error("没有按所选平台取单条命令（r.commands[plat.value]）")
	}
}

// TestInstallCommandIsClickToCopy 钉"命令可点击复制"。
//
// 用户要求"新增点击命令可以直接复制命令"：手选长命令容易漏字符，
// 而漏一个字符的表现是"脚本报未知参数"。
func TestInstallCommandIsClickToCopy(t *testing.T) {
	js := readSrc(t, "adminui/admin.js")

	if !strings.Contains(js, "function copyableCommand(") {
		t.Fatal("缺少 copyableCommand()：命令不可点击复制")
	}
	body := regionOf(t, js, "function copyableCommand(cmd)", "\n  }\n")
	// 整块可点 + 有独立按钮，两条路都要有（只给按钮的话，用户习惯性去点命令本身）。
	if !strings.Contains(body, "addEventListener('click'") {
		t.Error("命令块本身没有绑定点击复制")
	}
	if !strings.Contains(body, "复制命令") {
		t.Error("没有提供明确的「复制命令」按钮")
	}
	// 剪贴板不可用时必须降级为选中文本，而不是直接报"复制失败"。
	if !strings.Contains(js, "function selectTextOf(") {
		t.Error("缺少剪贴板降级路径 selectTextOf()：纯 HTTP 部署下复制会直接失败")
	}
	sel := regionOf(t, js, "function selectTextOf(node)", "\n  }\n")
	if !strings.Contains(sel, "createRange") || !strings.Contains(sel, "Ctrl+C") {
		t.Error("降级路径没有真的选中文本并提示手动复制")
	}
}

// makefilePlatforms 解析 Makefile 里的 PLATFORMS 清单。
//
// 必须**逐行拼续行**，不能指望一条正则吃下多行：Go 的 regexp 里
// `.*(?:\\\n.*)*` 会静默只匹配第一行（本文件第一版就是这么错的，
// 只解析到 6 个平台却仍然"通过"断言）。解析完还要自检数量，
// 免得 Makefile 一改格式，判据就悄悄退化成"拿半份清单比对"。
func makefilePlatforms(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("读取 Makefile 失败：%v", err)
	}
	var out []string
	collecting := false
	for _, ln := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(ln, "PLATFORMS :=") {
			collecting = true
			ln = strings.TrimPrefix(ln, "PLATFORMS :=")
		} else if !collecting {
			continue
		}
		// 续行以 `\` 结尾；最后一行没有 `\`。
		// **必须先消费这一行再决定结束**——上一版在这里先 break，
		// 结果整条 freebsd 行（含要钉的 freebsd/arm）被整行漏掉，
		// 而漏掉的表现恰好是"比对通过"（两边都少同一批），比报错危险得多。
		tail := strings.HasSuffix(strings.TrimRight(ln, " \t"), "\\")
		ln = strings.TrimRight(ln, " \t\\")
		for _, p := range strings.Fields(ln) {
			out = append(out, strings.Replace(p, "/", "_", 1))
		}
		if !tail {
			break
		}
	}
	if len(out) < 10 {
		t.Fatalf("从 Makefile 只解析到 %d 个平台（应 ≥10）：%v\n"+
			"要么 Makefile 改了格式，要么判据失效——两种情况都必须先修判据，不能放过", len(out), out)
	}
	return out
}

// TestReleasePlatformListMatchesMakefile 钉"发布平台清单与 Makefile 对齐"。
//
// 真事：handleAgentReleases 的 expected 清单多列了 darwin（采集器根本不编 darwin）、
// 又漏了 windows_386 / freebsd_386 / freebsd_arm，于是页面常年误报"缺失 darwin"，
// 而真正没发布的 freebsd/arm 反而没人提醒。
// 同一处的 releaseNameRe 也漏了裸 `arm`，导致 freebsd_arm 的 SHA-256 一栏永远是 `-`。
func TestReleasePlatformListMatchesMakefile(t *testing.T) {
	want := makefilePlatforms(t)

	// 从 install.go 抽出 expected 清单。
	src := readSrc(t, "install.go")
	seg := regionOf(t, src, "expected := []string{", "}")
	var got []string
	for _, q := range regexp.MustCompile(`"([a-z0-9_]+)"`).FindAllStringSubmatch(seg, -1) {
		got = append(got, q[1])
	}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("发布平台清单与 Makefile 的 PLATFORMS 不一致：\n  页面提示用 %v\n  Makefile 编 %v\n"+
			"（不一致会误报缺失或漏报缺失）", got, want)
	}
	if strings.Contains(seg, `"darwin`) {
		t.Error("列表里出现了 darwin：采集器不编 darwin（只有 linux/windows/freebsd）")
	}
}

// TestReleaseNameReCoversArm 钉 releaseNameRe 覆盖裸 arm。
//
// 这一条正是用户报的"SHA-256 一直是 -"的根因：正则里只有 armv7/armv6，
// 没有 `arm`，于是 xb-probe-agent_freebsd_arm 匹配不上、computeSHA 分支进不去。
func TestReleaseNameReCoversArm(t *testing.T) {
	// 所有 Makefile 平台都要被正则认出来（转成产物名后）。
	for _, p := range makefilePlatforms(t) {
		parts := strings.SplitN(p, "_", 2)
		if len(parts) != 2 {
			continue
		}
		name := "xb-probe-agent_" + p
		if parts[0] == "windows" {
			name = "xb-probe-agent_windows_" + parts[1]
		}
		if !releaseNameRe.MatchString(name) {
			t.Errorf("产物名 %s 不被 releaseNameRe 匹配：它的 SHA-256 会永远显示 `-`", name)
		}
	}

	// SHA256SUMS 系列必须也被认（管理员要能核对校验和文件本身）。
	for _, n := range []string{"SHA256SUMS", "SHA256SUMS.master", "SHA256SUMS.minisig"} {
		if !sumsNameRe.MatchString(n) {
			t.Errorf("%s 不被 sumsNameRe 匹配：发布文件页里它的 SHA-256 会是空的", n)
		}
	}
}
