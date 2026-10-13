package dist

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 一键部署的"参数集"写在两个脚本里，而它们必须对得上：
//
//	scripts/deploy-master.sh   —— 真正落盘、注册服务、写 master.yaml 的一方
//	install-master.sh          —— 取源码编译的入口，收集参数后转交给上面那个
//
// 之所以需要这条护栏，是因为**漂移的症状全在用户那一端，且在仓库里完全看不出**：
// 用户明明传了 `--public-listen 9000`，脚本"接受"了，配置文件里却还是 8080——
// 没有报错、没有警告，只有事后翻 master.yaml 才发现参数被吞了。
// 免编译入口（install-master-bin.sh）走的正是这一类的另一边：它靠"未知参数全部
// 原样透传"与部署脚本解耦，所以**不能**认得这些参数、更不能对未知项硬失败——
// 否则部署脚本每加一个参数，这个入口就先坏掉，而坏的理由和用户做的事毫无关系。

// parseArgsRegion 截出 `while [ $# -gt 0 ]; do … done` 这一段。
//
// 只在这一段里抽参数名，是为了避开 usage 文本与注释：它们同样以 `--xxx` 开头
// （还有 `--tls-mode <none|files|…>` 这种带竖线和尖括号的），混进来就等于
// "抽到一堆根本没实现的参数"，这条判据也就形同虚设了。
func parseArgsRegion(t *testing.T, rel, body string) string {
	t.Helper()
	const head = `while [ $# -gt 0 ]; do`
	i := strings.Index(body, head)
	if i < 0 {
		t.Fatalf("%s 里找不到参数解析循环（%q）——判据已与脚本写法脱节", rel, head)
	}
	rest := body[i:]
	j := strings.Index(rest, "\ndone\n")
	if j < 0 {
		t.Fatalf("%s 的参数解析循环没有结束的 done——判据已与脚本写法脱节", rel)
	}
	return rest[:j]
}

// scriptFlags 抽取"分支臂"上的参数名，也就是 `--xxx)` / `-h | --help)` 这种。
//
// 判据是"整条臂"而不是"这一行里出现过 --xxx"：后者会把紧邻的说明性注释一起
// 吃进来（`# --yes 只影响…不能用 --yes 透传…`），抽出来的集合就不再等于
// "脚本真正接受的参数"，这条护栏会给出假绿。
func scriptFlags(t *testing.T, rel, body string) map[string]bool {
	t.Helper()
	region := parseArgsRegion(t, rel, body)
	reArm := regexp.MustCompile(
		`(?m)^[ \t]*((?:--[a-z][a-z-]*|-y|-h)(?:[ \t]*\|[ \t]*(?:--[a-z][a-z-]*|-y|-h))*)\)`)
	out := map[string]bool{}
	for _, m := range reArm.FindAllStringSubmatch(region, -1) {
		for _, f := range strings.Split(m[1], "|") {
			f = strings.TrimSpace(f)
			if strings.HasPrefix(f, "--") {
				out[f] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s 里一个参数都没抽到——判据已与脚本写法脱节", rel)
	}
	return out
}

// forwardedFlags 抽取 `set -- … --xxx …` 里真正转交给部署脚本的参数名。
func forwardedFlags(t *testing.T, rel, body string) map[string]bool {
	t.Helper()
	reFlag := regexp.MustCompile(`(?:^|[ \t])(--[a-z][a-z-]*)`)
	out := map[string]bool{}
	stmts := 0
	for _, line := range strings.Split(body, "\n") {
		k := strings.Index(line, "set -- ")
		if k < 0 {
			continue
		}
		stmts++
		// 只看 `set -- ` 之后的部分：那之前的 `--` 属于别的语法，不是参数名。
		// （`set -- "$@" …` 里那个孤立的 `--` 因为后面跟的是空白，匹配不上 [a-z]。）
		for _, m := range reFlag.FindAllStringSubmatch(line[k:], -1) {
			out[m[1]] = true
		}
	}
	if stmts == 0 || len(out) == 0 {
		t.Fatalf("%s 里没找到任何 `set --` 转发语句——判据已与脚本写法脱节", rel)
	}
	return out
}

func TestDeployAndInstallScriptsShareTheSameFlagSet(t *testing.T) {
	deploy := readRepoFile(t, "../../scripts/deploy-master.sh")
	inst := readRepoFile(t, "../../install-master.sh")
	binInst := readRepoFile(t, "../../install-master-bin.sh")

	deployFlags := scriptFlags(t, "scripts/deploy-master.sh", deploy)
	instFlags := scriptFlags(t, "install-master.sh", inst)

	// 下面这些是**有意**不由安装脚本转发的，不是漏了：
	//   --bin            源码安装路径是自己编译出来的，二进制路径由它自己定
	//   --admin-password 口令不该走命令行（会进 shell 历史）；无人值守用
	//                    PROBE_ADMIN_PASSWORD，其余场景由部署脚本交互输入
	//   --ask-mode       入口脚本**内部**用它去问"做什么"（拿回答案就退出），
	//                    对用户没有意义；把它当成用户参数转发过去只会让流程
	//                    在问完一句之后直接退出，后面的安装全不执行。
	//   --mode           同上，是入口脚本把问到的答案**带下去**用的通道。
	//                    install-master.sh 确实会传它（`set -- "$@" --mode "$__ASKED_MODE"`），
	//                    但那是脚本算出来的，不需要用户解析或给值——而这条判据
	//                    查的是"解析循环里有没有这个臂"，所以在这里说明。
	allowMissing := map[string]bool{
		"--bin":            true,
		"--admin-password": true,
		"--ask-mode":       true,
		"--mode":           true,
	}

	var missing []string
	for f := range deployFlags {
		if instFlags[f] || allowMissing[f] {
			continue
		}
		missing = append(missing, f)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("部署脚本接受这些参数，而 install-master.sh 既不解析也不转发：%v\n"+
			"  症状：用户传了参数、脚本收下了，部署出来的配置却是默认值，且不报任何错。\n"+
			"  修法：在 install-master.sh 里补上解析与转发；确实不该转发的，在 allowMissing 里说明原因。",
			missing)
	}

	// 反向也查一遍。而且这一向更隐蔽：参数被解析进变量、然后什么也不做——
	// 脚本作者与用户都以为它生效了，只有对比 master.yaml 才看得出来。
	privateFlags := map[string]bool{
		"--repo": true, "--ref": true, "--src": true, "--go-version": true,
		"--goproxy": true, "--no-deps": true, "--yes": true, "-y": true,
		"--dry-run": true, "-h": true, "--help": true,
	}
	fwd := forwardedFlags(t, "install-master.sh", inst)
	var notForwarded []string
	for f := range instFlags {
		if privateFlags[f] || fwd[f] {
			continue
		}
		notForwarded = append(notForwarded, f)
	}
	sort.Strings(notForwarded)
	if len(notForwarded) > 0 {
		t.Errorf("install-master.sh 解析了这些参数，却没有转交给部署脚本：%v\n"+
			"  参数被读走之后什么也不做，是最难发现的一种失效：两边都以为它生效了。",
			notForwarded)
	}

	// 免编译入口刻意**不认识**部署脚本的参数，全部原样透传。下面这一条守在
	// 这个"刻意"上：一旦有人在它的兜底分支里改成对未知参数硬失败，部署脚本以后
	// 每加一个参数都会先把这个入口弄坏，而报错信息会和用户真正做的事毫不相关。
	//
	// 只在解析循环里找 `*)` 分支：注释与 usage 文本里同样会出现"未知参数"这类字眼，
	// 用全文 Contains 判会把一句说明文字当成实现（这一条正是被它误判过才改成现在这样）。
	binRegion := parseArgsRegion(t, "install-master-bin.sh", binInst)
	reCatchAll := regexp.MustCompile(`(?m)^[ \t]*\*\)[ \t]*(.*)$`)
	catches := reCatchAll.FindAllStringSubmatch(binRegion, -1)
	if len(catches) == 0 {
		t.Fatal("install-master-bin.sh 的参数解析里没有 `*)` 兜底分支——判据已与脚本写法脱节")
	}
	passthru := false
	for _, c := range catches {
		if strings.Contains(c[1], `PASSTHRU="$PASSTHRU $1"`) {
			passthru = true
		}
	}
	if !passthru {
		t.Errorf("install-master-bin.sh 的 `*)` 兜底分支不再原样透传（实为 %q）——"+
			"它正是靠「全部透传」与部署脚本解耦的；一改成硬失败，部署脚本以后每加一个"+
			"参数都会先坏掉这个入口，而报错信息与用户真正做的事毫不相关", catches[0][1])
	}
}
