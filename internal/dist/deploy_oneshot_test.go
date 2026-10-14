package dist

import (
	"regexp"
	"strings"
	"testing"
)

// 本文件钉的是用户提的两个「只想做一件事，别的都别动」的模式：
//
//	4) --upgrade-only    只升级 Master 本体（换二进制 + 重启服务）
//	5) --reset-password  只重置管理员密码
//
// 用户原话：
//
//	「如果 master 更新，但是我只想更新本体，如果我选择 1) 安装 / 升级 Master，
//	  那么他会全部走一遍流程，包括站点设置、证书申请、管理员账号设置等全部流程，
//	  但是这些我不想做，我仅仅只想升级 master 本体」；
//	「如果我 web 界面忘记密码，就必须利用脚本卸载重装，所以 master 或者一键部署
//	  脚本具备重新设置密码选项」。
//
// 这两个模式的失效方式都不是"跑不起来"，而是**跑起来了、还打印了「完成」，
// 但顺手改掉了用户明确要求别动的东西**——或者反过来，"只重置密码"先把服务停了。
// 所以判据全部落在"这段代码里**没有**哪些动作"以及"顺序对不对"上。

// menuDispatch 切出 0b 里「菜单答案 → 标志」的那一段 case。
//
// 为什么要切区间：0b 的派发和 --mode 的映射**长得几乎一样**（都是 `4) UPGRADE_ONLY="1"`），
// 全文 Contains 会让其中一份替另一份作证——删掉一边、另一边还在，判据照样绿，
// 而故障是真的（"菜单画了但答进去不生效"，或者"入口问过之后又被问一遍"）。
func menuDispatch(t *testing.T, body string) string {
	t.Helper()
	return shRegion(t, "scripts/deploy-master.sh", body,
		`case "$(ask_mode_menu)" in`, "\nesac\n")
}

// modeMapping 切出 --mode 的取值映射（入口脚本把答案送下来的那一条路）。
func modeMapping(t *testing.T, body string) string {
	t.Helper()
	return shRegion(t, "scripts/deploy-master.sh", body,
		`case "${MODE_IN:-}" in`, "\nesac\n")
}

// TestOneShotFlagsHaveDefaultsBeforeUse 钉住 `set -eu` 下的一个通用坑：
// **先给默认值，再解引用**。
//
// 这不是假想的：加这两个模式时，第 192 行附近那一串 `XXX=""` 初始化漏了
// UPGRADE_ONLY 与 RESET_PASSWORD，而 0b 的派发条件里写着 `[ -z "$UPGRADE_ONLY" ]`。
// `set -u` 让整脚本在那一行直接以 `unbound variable` 退出——症状是**整个交互入口
// 全废**（`--ask-mode` 都跑不起来），而报错行在几百行之外，与"新加了两个开关"
// 看不出关系。
//
// 判据是"初始化出现在第一次解引用之前"，而不是"文件里有这一行"：后者在
// "初始化被挪到使用点之后"时照样绿，而那正是同一种失效。
func TestOneShotFlagsHaveDefaultsBeforeUse(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")
	for _, v := range []string{"UNINSTALL", "RELEASE_ONLY", "UPGRADE_ONLY", "RESET_PASSWORD", "MIGRATE_LEGACY"} {
		init := strings.Index(body, v+`=""`)
		if init < 0 {
			t.Errorf("%s 没有拿到默认值（%s=\"\"）：\n"+
				"  set -u 下第一次 `[ -z \"$%s\" ]` 就会以 unbound variable 中止，\n"+
				"  而那一行往往在几百行之外——症状是「整个入口都跑不起来」，根因看不出来。",
				v, v, v)
			continue
		}
		use := strings.Index(body, "$"+v)
		if use < 0 {
			t.Errorf("%s 初始化了却再没被解引用——那这个开关根本没生效。", v)
			continue
		}
		if init > use {
			t.Errorf("%s 的默认值(%d)排在第一次使用(%d)之后：\n"+
				"  set -u 下这等于没初始化。判据要的就是顺序，不是「文件里有这一行」。",
				v, init, use)
		}
	}
}

// TestUpgradeOnlyTouchesNothingButTheBinary 守 --upgrade-only 的边界。
//
// 这个模式的全部价值就是"别的都不动"。它同时必须要有的几件事（备份、验活、失败
// 还原、动手之前先验配置）与**绝不能**有的事（重写配置、初始化管理员、动 Caddy、
// 请证书）都在这里钉住——少一条是"用户要的功能没做到"，多一条是"用户不要的改动
// 被做了"，两者的症状都是"脚本说完成"。
func TestUpgradeOnlyTouchesNothingButTheBinary(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")
	region := shRegion(t, "scripts/deploy-master.sh", body,
		"# 0d) 只升级 Master 本体", "# 0e) 重置管理员密码")

	if !strings.Contains(body, `--upgrade-only) UPGRADE_ONLY="1"; shift ;;`) {
		t.Error("部署脚本没有解析 --upgrade-only。")
	}
	if !strings.Contains(body, `4 | upgrade | UPGRADE | Upgrade | 升级本体 | 只升级本体) echo "4" ;;`) {
		t.Error("菜单第 4 项没有归一到 4：用户看得见这一项、答进去却不被认。")
	}
	// 菜单 → 标志（0b）与 --mode → 标志（映射）是**两条独立的路**，两边的判据
	// 必须各自切区间：它们连写法都一样，全文 Contains 会让其中一份替另一份作证。
	if !regexp.MustCompile(`(?m)^\t4\) UPGRADE_ONLY="1" ;;`).MatchString(menuDispatch(t, body)) {
		t.Error("菜单答案 4 没有映射到 UPGRADE_ONLY（菜单画了但分支没接）：\n" +
			"  用户选了「只升级本体」，脚本照样去装整站。")
	}
	if !regexp.MustCompile(`(?m)^4\)\n\tUPGRADE_ONLY="1"\n\tMODE_GIVEN="1"`).MatchString(modeMapping(t, body)) {
		t.Error("--mode 的取值映射里没有正确接上 4：入口脚本问过之后再送答案下来时，\n" +
			"  这个模式不生效，而用户已经被问过一遍——正是「先问」想消掉的那个体验。")
	}
	if !strings.Contains(modeMapping(t, body), "1、2、3、4 或 5") {
		t.Error("--mode 的非法取值提示还停在旧的取值集合上：\n" +
			"  用户写了 4 却被报「只能是 1、2、3」，会以为这个功能不存在。")
	}

	if !strings.Contains(region, `if [ -n "$UPGRADE_ONLY" ]; then`) {
		t.Fatal("找不到 0d 的模式分支：判据与脚本结构脱节了。")
	}

	// ① 只换二进制：这些动作一个都不许出现。
	for _, forbidden := range []string{
		"render_config", // 重写 master.yaml
		"init-admin",    // 初始化管理员
		"setup_caddy",   // 装/配 Caddy
		"obtain_cert",   // 请证书
		"mkdir -p \"$DIR\"",
	} {
		if strings.Contains(region, forbidden) {
			t.Errorf("0d（只升级本体）里出现了 %q：\n"+
				"  这个模式只换二进制并重启服务。用户明确说了「这些我不想做」，\n"+
				"  「顺手」做掉任何一件都会让这个模式名不副实。", forbidden)
		}
	}

	// ② 动手之前必须先把两件事验完：新二进制跑得起来、它认得出这份配置。
	//    顺序很要紧——等停了服务才发现装上去起不来，就白白断了一次服务。
	iStop := strings.Index(region, "systemctl stop xb-probe-master")
	iVer := strings.Index(region, `"$BIN_SRC" version`)
	iParse := strings.Index(region, "config_parses_ok")
	if iVer < 0 || iParse < 0 {
		t.Error("0d 没有在停服务之前验「新二进制能不能跑」与「它认不认得现有配置」：\n" +
			"  这是本模式唯一能「在动系统之前失败」的机会。")
	} else if iStop >= 0 && (iVer > iStop || iParse > iStop) {
		t.Error("新二进制的自检排在停服务之后：\n" +
			"  等停了服务才发现装上去起不来，就白白断了一次服务——而它本来可以先验。")
	}

	// ③ 判出「这份配置新二进制不认」时必须**当场退出**。
	//    这一条是本轮真实修掉的 bug：config_mismatch_report 只报告不退出（它另一个
	//    使用者要接着回滚），所以调用点后面漏一个 exit，执行流会**一路走到
	//    「停服务 → 换二进制」**上去——与注释里写的"到此为止"正好相反，
	//    而且中间没有任何报错。
	//
	//    判据必须切出**那一个 if 块**：光看"调用点之后还有没有 exit"是没用的——
	//    这一段后面本来就有好几处（非 root、没有 systemctl），随便哪一个都会
	//    替它作证。所以先切到它自己的 `fi`，再在这个块里找。
	iReport := strings.Index(region, `config_mismatch_report "$_CP_OUT"`)
	if iReport < 0 {
		t.Error("0d 在配置错配时没有走共用的诊断（两种修法/点名键会变成第二份）。")
	} else {
		blk := region[iReport:]
		j := strings.Index(blk, "\n\t\tfi\n")
		if j < 0 {
			// 切不出这个 if 块就必须**拒绝**，不能退回"在剩下整段里找 exit"：
			// 那样一来，块里那个 exit 被删掉也不会红（后面的非 root / 无 systemctl
			// 分支里还有 exit）——判据失效却仍然全绿，是最糟的一种。
			t.Fatal("0d 的配置错配分支切不出边界（找不到它自己的 fi）——判据已与脚本写法脱节。")
		}
		blk = blk[:j]
		if !regexp.MustCompile(`(?m)^\s*exit\b`).MatchString(blk) {
			t.Error("0d 判出配置错配后**没有退出**（那个 if 块里没有 exit）：\n" +
				"  config_mismatch_report 只报告不退出（另一个使用者要接着回滚），\n" +
				"  漏了 exit 就会一路执行到「停服务 → 换二进制」——即「明知起不来还装上去」。")
		}
	}

	// ④ 备份 + 失败还原 + 验活：换二进制是一条没有退路的动作。
	if !strings.Contains(region, "xb-probe-master.bak") {
		t.Error("0d 覆盖前没有备份旧二进制：换上去起不来就没得还原了。")
	}
	if !shCalls(region, "upgrade_only_rollback") {
		t.Error("0d 没有还原路径（备份了却不还原等于没备份）。")
	}
	if !strings.Contains(region, "systemctl is-active --quiet xb-probe-master") {
		t.Error("0d 起服务后没有验活：\n" +
			"  Type=simple 下 systemctl start 一返回就认为成功，而进程可能已经开始\n" +
			"  因为端口被占 / 配置键写错而退出。判据要挑「真的可用」，不挑「进程活着」。")
	}

	// ⑤ dry-run 必须在**任何写入之前**退出，而且不需要 root。
	iDry := strings.Index(region, "DRY_RUN")
	if iDry < 0 {
		t.Error("0d 没有 dry-run 分行。")
	} else if iStop >= 0 && iDry > iStop {
		t.Error("0d 的 dry-run 判断排在停服务之后：dry-run 一个字节都不该写。")
	}
	if !strings.Contains(region, `[ "$(id -u)" -ne 0 ]`) {
		t.Error("0d 没有 root 检查（要写 $BIN_DIR 并重启服务）。")
	}
}

// TestResetPasswordOnlyChangesThePassword 守 --reset-password 的边界。
//
// 起因是用户的一句话：「如果我 web 界面忘记密码，就必须利用脚本卸载重装」。
// 卸载重装会连数据库一起清掉——为了改一个口令付这个代价毫无道理。
//
// 所以这个模式的判据几乎全是"不许出现什么"：不卸载、不重写配置、不重启服务、
// 不下载、不编译。少一条"不许"，用户就会在最需要它的时刻（进不去后台了）
// 遇到一次没人预期的改动。
func TestResetPasswordOnlyChangesThePassword(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")
	region := shRegion(t, "scripts/deploy-master.sh", body,
		"# 0e) 重置管理员密码", "# 卸载：--uninstall")

	if !strings.Contains(body, `--reset-password) RESET_PASSWORD="1"; shift ;;`) {
		t.Error("部署脚本没有解析 --reset-password。")
	}
	if !strings.Contains(body, `5 | password | PASSWORD | Password | 重置密码 | 改密码) echo "5" ;;`) {
		t.Error("菜单第 5 项没有归一到 5：用户看得见这一项、答进去却不被认。")
	}
	if !regexp.MustCompile(`(?m)^\t5\) RESET_PASSWORD="1" ;;`).MatchString(menuDispatch(t, body)) {
		t.Error("菜单答案 5 没有映射到 RESET_PASSWORD（菜单画了但分支没接）：\n" +
			"  用户选了「重置密码」，脚本照样去装整站（或去卸载）——\n" +
			"  而他本来就是为了「别卸载重装」才选这一项的。")
	}
	if !regexp.MustCompile(`(?m)^5\)\n\tRESET_PASSWORD="1"\n\tMODE_GIVEN="1"`).MatchString(modeMapping(t, body)) {
		t.Error("--mode 的取值映射里没有正确接上 5。")
	}

	if !strings.Contains(region, `if [ -n "$RESET_PASSWORD" ]; then`) {
		t.Fatal("找不到 0e 的模式分支：判据与脚本结构脱节了。")
	}

	for _, forbidden := range []string{
		"render_config", // 重写配置
		"systemctl",     // 停/起服务——改口令根本不用重启
		"rm -f",         // 删文件
		"rm -rf",
		"init-admin", // 初始化管理员（那是"重建账号"，不是"改口令"）
	} {
		if strings.Contains(region, forbidden) {
			t.Errorf("0e（只重置密码）里出现了 %q：\n"+
				"  这个模式只改一个口令。用户是在「进不去后台」的时候用它，\n"+
				"  任何多余动作都会把一个救急手段变成一次事故。", forbidden)
		}
	}

	// 真正动手的那一下必须走 reset-admin，且显式带上那道闸。
	//
	// 判据要落在**调用那一行**上，不能对整段 Contains，还要躲开两种"替它作证"的
	// 东西——这两个坑都是本脚本的变异验证揭出来的：
	//   · 说明注释里就写着「有 --i-know-what-i-am-doing 这道闸」→ 把标志从命令里
	//     删掉、注释还留着，Contains 照样为真（M12 首版**仍绿**）；
	//   · --dry-run 那句 `echo "… reset-admin …"` 也含 reset-admin，而且不是注释，
	//     会排在真正的调用前面（把标志删掉之后仍然"找得到 reset-admin"）。
	// 所以：先拼逻辑行，再只留"真的在调"的那一行——既排除注释，也排除 echo/printf。
	var resetLines []string
	for _, ln := range logicalLines(region) {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "echo") || strings.HasPrefix(t, "printf") {
			continue
		}
		if strings.Contains(t, "reset-admin") {
			resetLines = append(resetLines, t)
		}
	}
	if len(resetLines) != 1 {
		t.Fatalf("0e 里「真的调用 reset-admin」的逻辑行有 %d 条（期望正好 1）——判据已与脚本写法脱节。",
			len(resetLines))
	}
	if !strings.Contains(resetLines[0], "--i-know-what-i-am-doing") {
		t.Error("0e 调用 reset-admin 时没有带那道闸：\n" +
			"  那道闸挡的是「随手敲一条命令误改生产口令」，而本模式是用户从菜单里\n" +
			"  明确选了「重置管理员密码」，属于它的正当用途——所以是**主动传**。")
	}

	// 判成败时必须「先把输出收进变量、再取 $?」。
	// 写成 `cmd 2>&1 | sed …` 的话，管道会把退出码换成 sed 的（恒 0）：任何失败被
	// 静默吞掉，脚本接着打印「完成：口令已更新」，而用户拿着新口令登不进去。
	//
	// 判据要「同一处实现的相邻两行」：命令替换收走输出、紧接着取 $?。
	// 用 `[$][?]` 而不是 `\$\?`——后者在 RE2 里是转义问号，写错一个字符
	// 这条断言就会静静地永远不成立（护栏自己变成假绿）。
	if !regexp.MustCompile(`(?m)2>&1\)"\n\s*[A-Za-z_][A-Za-z0-9_]*=\$[?]`).MatchString(region) {
		t.Error("0e 没有把 reset-admin 的输出收进变量再判退出码：\n" +
			"  凡「配好了」都要当场验，而这里连「做没做成」都还没验。")
	}

	// 交互输入必须问两遍：重置口令没有「再输一次确认」的话，打错一个字符就变成
	// 「重置成功但登不进去」——而那个看起来完全像 bug。
	if !strings.Contains(region, "再输一遍") {
		t.Error("0e 交互输入口令时没有要求确认第二遍：\n" +
			"  打错一个字符的结局是「完成：口令已更新」+ 登不进去，最难查的一类症状。")
	}
	if !strings.Contains(region, `if [ -z "$ADMIN_PASSWORD" ]; then`) {
		t.Error("0e 没有处理「口令为空」（空口令重置成功等于把后台敞开）。")
	}
}

// TestEntryScriptsAskBeforeCompiling 守两个入口脚本对这四个模式的接线。
//
// 入口脚本**不实现**这些模式，它只做两件事：问一次（--ask-mode）、把答案送下去
// （--mode N）。所以这里钉的正是这两件事在两个入口上都没有漏——漏了的症状是
// 「用户答了 4，转头又被问一遍」，或者更糟：答案被丢掉，脚本按 1 去装 Master。
func TestEntryScriptsAskBeforeCompiling(t *testing.T) {
	// ---- 源码入口 install-master.sh ----
	inst := readRepoFile(t, "../../install-master.sh")
	if !strings.Contains(inst, `--upgrade-only) UPGRADE_ONLY="1"; shift ;;`) {
		t.Error("install-master.sh 没有解析 --upgrade-only。")
	}
	if !strings.Contains(inst, `--reset-password) RESET_PASSWORD="1"; shift ;;`) {
		t.Error("install-master.sh 没有解析 --reset-password。")
	}
	// 菜单答案的接线：4 走"编译后只换二进制"，5 走"编译之前就早退"。
	if !regexp.MustCompile(`(?m)^\t\t\t4\) UPGRADE_ONLY="1" ;;`).MatchString(inst) {
		t.Error("install-master.sh 的 --ask-mode 答案里没有 4：\n" +
			"  用户从入口的菜单选了「只升级本体」，却被当成「安装/升级」走完整流程——\n" +
			"  正是这次要消掉的那件事。")
	}
	if !regexp.MustCompile(`(?m)^\t\t\t5\) RESET_PASSWORD="1" ;;`).MatchString(inst) {
		t.Error("install-master.sh 的 --ask-mode 答案里没有 5。")
	}
	// 5 必须落到"不需要源码与 Go"的早退通路；4 **不能**落进去（它就是要编译）。
	if !strings.Contains(inst, `set -- --reset-password`) {
		t.Error("install-master.sh 解析了 --reset-password 却没有转给部署脚本：\n" +
			"  参数被读走之后什么也不做，是最难发现的一种失效。")
	}
	// 入口这里也要互斥，而且**必须**在早退通路之前：那段通路用 `set --` 把参数整串
	// 顶掉，被顶掉的那个永远到不了部署脚本——那边本来能抓住的组合，在这里变成
	// "安安静静做了另一件事"（`--upgrade-only --uninstall` → 真的卸载）。
	// 判据同样必须切区间：全文 Contains `-gt 1` 会被部署脚本那份替它作证。
	imExcl := shRegion(t, "install-master.sh", inst,
		"_im_oneshot=\"\"", "if [ -n \"$UNINSTALL\" ] || [ -n \"$RELEASE_ONLY\" ]")
	for _, fl := range []string{"--uninstall", "--release-only", "--upgrade-only", "--reset-password"} {
		if !strings.Contains(imExcl, `_im_oneshot="$_im_oneshot `+fl+`"`) {
			t.Errorf("install-master.sh 没有把 %s 计入入口处的「只做一件事」互斥：\n"+
				"  不数它，它与别的单次模式同时给出时会被早退通路静默顶掉，\n"+
				"  用户以为做了 A、实际做了 B。", fl)
		}
	}
	if !strings.Contains(imExcl, `if [ "$_im_count" -gt 1 ]; then`) {
		t.Error("install-master.sh 的入口互斥没有按个数判断（退回两两列举就会漏掉新组合）。")
	}
	if strings.Contains(imExcl, "exit 0") {
		t.Error("install-master.sh 的入口互斥分支里出现了 exit 0：\n" +
			"  互相矛盾的要求要以非 0 退出，否则「什么都没做」会被上游当成成功。")
	}
	// 只升级本体时不能把站点/证书那一串拼进去（那个模式下它们全是空的）。
	if !strings.Contains(inst, `set -- --bin "$SRC/bin/xb-probe-master" --upgrade-only`) {
		t.Error("install-master.sh 的 --upgrade-only 没有把编译产物一起交下去。")
	}
	// 问到的答案要**真的送下去**：写死成 --mode 1 就等于把 4/5 的答案丢掉。
	if !strings.Contains(inst, `set -- "$@" --mode "$__ASKED_MODE"`) {
		t.Error("install-master.sh 把 --mode 写死了：\n" +
			"  答案 4/5 会被当成 1 送下去——用户选的「只升级本体」变成「装一遍整站」。")
	}

	// ---- 免编译入口 install-master-bin.sh ----
	bin := readRepoFile(t, "../../install-master-bin.sh")
	if !strings.Contains(bin, `--upgrade-only) UPGRADE_ONLY="1"; PASSTHRU="$PASSTHRU --upgrade-only"; shift ;;`) {
		t.Error("install-master-bin.sh 没有识别 --upgrade-only（它会让入口在非交互下再次提问）。")
	}
	if !strings.Contains(bin, `--reset-password) RESET_PASSWORD="1"; PASSTHRU="$PASSTHRU --reset-password"; shift ;;`) {
		t.Error("install-master-bin.sh 没有识别 --reset-password。")
	}
	if !strings.Contains(bin, `1 | 2 | 3 | 4 | 5) MODE_ASKED="1" ;;`) {
		t.Error("install-master-bin.sh 只认 1/2/3 的菜单答案：\n" +
			"  答 4/5 会走进兜底分支，被当成「取回的脚本不认识 --ask-mode」，\n" +
			"  于是用户被问第二遍，而且下载路径也回到了最费事的那一条。")
	}
	if !strings.Contains(bin, `elif [ -n "$UPGRADE_ONLY" ]; then`) ||
		!strings.Contains(bin, `MODE="4"`) ||
		!strings.Contains(bin, `elif [ -n "$RESET_PASSWORD" ]; then`) ||
		!strings.Contains(bin, `MODE="5"`) {
		t.Error("install-master-bin.sh 的模式判定里没有 4/5（显式给了标志还会再问一遍）。")
	}
	// 3a 的早退通路：5 要进去（不下二进制），4 **必须留在外面**——升级本体靠的
	// 就是那个要下载的二进制，把它也早退掉，部署脚本会以「需要指定新二进制」收场。
	early := shRegion(t, "install-master-bin.sh", bin,
		`if [ "$MODE" = "2" ] || [ "$MODE" = "3" ]`, "say \"开始$_early_what")
	if !strings.Contains(early, `"$MODE" = "5"`) {
		t.Error("install-master-bin.sh 的重置密码没有走「不下载任何东西」那条早退通路。")
	}
	if strings.Contains(early, `"$MODE" = "4"`) {
		t.Error("install-master-bin.sh 把「只升级本体」也放进了早退通路：\n" +
			"  那条路根本不下载二进制，而升级本体要的就是它——必然以\n" +
			"  「只升级本体需要指定新二进制」收场。")
	}
}

// TestLegacyLayoutIsRefusedNotSilentlyBroken 守改名前老布局的机器。
//
// 起因是用户实测：v0.1.2 时代装的老布局（probe-master.service）跑新的
// --upgrade-only，脚本按新布局写死路径 →「Unit xb-probe-master.service not
// found」→ 回滚也失败。改名那一轮只给 Agent 配了 MigrateLegacy，Master 侧
// 对老布局没有任何感知。修法是**拒绝并给出迁移办法**（--migrate-legacy 让
// 脚本搬，目标已有内容绝不覆盖），而不是静默搬家——"只做一件事"的模式
// 不该顺手改半个系统。
//
// 判据有三层：
//  1. 开关与关键件都在（少一个就是功能没了）；
//  2. 拦截在交互菜单**之后**——菜单选 4/5 的答案要到菜单那里才置进标志位，
//     放早了"菜单选 4"这条路会绕过检测（显式 --upgrade-only 反而拦得住，
//     是最阴的一种假绿）；
//  3. 拦截在四个单次模式块**之前**——放晚了老布局机器会先走进 0c/0d/0e，
//     报错回到 systemd 那句 Unit not found。
func TestLegacyLayoutIsRefusedNotSilentlyBroken(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")
	for _, m := range []string{
		`--migrate-legacy) MIGRATE_LEGACY="1"; shift ;;`,
		`legacy_master_present()`,
		`legacy_master_recipe()`,
		`systemctl stop probe-master.service`,
		`已搬数据：/var/lib/probe-master → $DATA`,
		`systemctl disable probe-master.service`,
	} {
		if !strings.Contains(body, m) {
			t.Errorf("老布局拦截缺关键件：%s", m)
		}
	}
	iMenu := strings.Index(body, `case "$(ask_mode_menu)" in`)
	iGuard := strings.Index(body, `0-1) 老布局检测`)
	iOneShot := strings.Index(body, "\nif [ -n \"$RELEASE_ONLY\" ]; then\n")
	if iMenu < 0 || iGuard < 0 || iOneShot < 0 {
		t.Fatal("判据锚点丢了——脚本结构与本测试的假设脱节，先改锚点再改实现。")
	}
	if iGuard < iMenu {
		t.Errorf("老布局拦截(%d)排在了交互菜单(%d)之前：\n"+
			"  菜单选 4/5 的答案要到菜单那里才置进标志位，放早了拦不住这条路。",
			iGuard, iMenu)
	}
	if iGuard > iOneShot {
		t.Errorf("老布局拦截(%d)排在了单次模式块(%d)之后：\n"+
			"  那样老布局机器会先走进 0c/0d/0e，报错回到 systemd 的 Unit not found。",
			iGuard, iOneShot)
	}
}

// TestLegacyMigrationOrderIsCorrect 守老布局搬迁的**位置**。
//
// 这一条来自用户实测的第二轮：他照提示加了 --migrate-legacy，却在菜单里顺手选了
// 4（"只升级本体"，看着最像"升级"），于是撞上老布局拦截——一条命令能做完的迁移
// 变成一次报错。顺着查下去还发现更要紧的一件事：搬迁原本和拦截写在同一处（都在
// 交互菜单之后），而"从 .answers 取默认值"在那之前——答案文件就躺在
// /etc/probe-master 里，等搬迁做完再去读，用户已经被从头问了一遍。搬了等于白搬。
//
// 所以搬迁与拦截必须分居两地，用同一个 MIGRATE_LEGACY 串着：
//
//	· 搬迁在"取答案默认值"之前（本用例第 2 条判据）；
//	· 拦截在交互菜单之后（上面 TestLegacyLayoutIsRefusedNotSilentlyBroken 钉着）；
//	· 参数已给就不要再问（第 3 条）——否则又回到"选错就白跑"。
func TestLegacyMigrationOrderIsCorrect(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")
	binEntry := readRepoFile(t, "../../install-master-bin.sh")
	srcEntry := readRepoFile(t, "../../install-master.sh")

	// 1) 两处都在，且函数定义（搬迁要调用它们）排在搬迁之前。
	iFn := strings.Index(body, "ask_mode_menu() {")
	iHelpers := strings.Index(body, "legacy_master_present() {")
	iMig := strings.Index(body, "0-0b) 老布局")
	iAns := strings.Index(body, `if [ -z "$FRESH_ANSWERS" ]; then`)
	iMenuMsg := strings.Index(body, "这个脚本可以做五件事")
	if iFn < 0 || iHelpers < 0 || iMig < 0 || iAns < 0 || iMenuMsg < 0 {
		t.Fatal("判据锚点丢了——脚本结构与本测试的假设脱节，先改锚点再改实现。")
	}
	if iHelpers > iMig {
		t.Errorf("老布局的判据函数(%d)定义在搬迁块(%d)之后：\n"+
			"  shell 自上而下执行，函数必须先于调用点——否则搬迁时是\n"+
			"  「legacy_master_present: not found」，而 set -e 会当场中止。", iHelpers, iMig)
	}

	// 2) 搬迁必须排在"从 .answers 取默认值"之前。
	//    答案文件 <配置目录>/.answers 在老配置目录里，搬完才读得到；顺序反了就是
	//    "搬了数据却让用户从头答一遍"，而且这一点在输出里看不出来（他不报错）。
	if iMig > iAns {
		t.Errorf("老布局搬迁(%d)排在了「从 .answers 取默认值」(%d)之后：\n"+
			"  答案文件在老配置目录里（/etc/probe-master/.answers），搬完才读得到。\n"+
			"  顺序反了＝数据搬过去了、问答却要全部重答一遍，而用户看不出原因。", iMig, iAns)
	}

	// 3) --migrate-legacy 一旦给出，"做什么"就不再问。
	//    它只与"安装 / 升级"搭配，答案已经确定；再问一遍就给了选错的机会——而
	//    "只升级本体"看着最像"升级"，恰恰是老布局机器上最该避开的那个。
	iEarly := strings.Index(body[iFn:], `if [ -n "$MIGRATE_LEGACY" ]; then`)
	iHeredoc := strings.Index(body[iFn:], "cat >&2 <<'EOF'")
	if iEarly < 0 {
		t.Errorf("ask_mode_menu 里没有 --migrate-legacy 的早退：\n" +
			"  那样用户会在菜单里被问一次，选 4 就白跑（实测踩过）。")
	} else if iEarly > iHeredoc {
		t.Errorf("ask_mode_menu 里 --migrate-legacy 的早退(%d)排在了菜单正文(%d)之后：\n"+
			"  排在后面等于没做——菜单已经打印、答案已经被收走了。", iEarly, iHeredoc)
	}

	// 4) 它与四个"只做一件事"的模式互斥：那些承诺"别的都不动"，而它要搬数据库。
	//    判据复用已有的计数，不新增一套两两组合（四个模式加它就是四对，写全会漏）。
	if !strings.Contains(body, `[ -n "$MIGRATE_LEGACY" ] && [ "$_oneshot_count" -gt 0 ]`) {
		t.Error("--migrate-legacy 与「只做一件事」的模式没有互斥判据：\n" +
			"  --upgrade-only --migrate-legacy 会先换二进制（新布局路径），再撞上搬迁，\n" +
			"  报错时人已经懵了。用现成的 _oneshot_count 判，别再来一套两两列举。")
	}

	// 5) dry-run 承诺"一个字节都不写"，而搬迁每一步都是写：必须有 DRY_RUN 分支，
	//    且它排在第一个 cp 之前。
	iDry := strings.Index(body[iMig:], `[ -n "$DRY_RUN" ]`)
	iCp := strings.Index(body[iMig:], `cp -a /var/lib/probe-master/. "$DATA"/`)
	if iDry < 0 || iCp < 0 {
		t.Fatal("搬迁块里找不到 dry-run 分支或数据拷贝——锚点要跟着实现改。")
	}
	if iDry > iCp {
		t.Errorf("dry-run 分支(%d)排在了数据拷贝(%d)之后：\n"+
			"  那样 --dry-run --migrate-legacy 真的会停服务、拷数据库——\n"+
			"  而 dry-run 的承诺是一个字节都不写。", iDry, iCp)
	}

	// 6) --ask-mode 是**纯查询**：入口在"什么都还不知道"的时候调它取答案，
	//    那里承诺"不碰系统、不需要 root、不读配置"。搬迁是停服务 + 拷数据库，
	//    排在它前面就等于一个查询动作会改机器（而且非 root 时以一堆失败收场）。
	if !strings.Contains(body, `if [ -n "$MIGRATE_LEGACY" ] && [ -z "$ASK_MODE" ]; then`) {
		t.Error("搬迁块没有排除 --ask-mode：\n" +
			"  入口取答案的那一趟会顺手停服务、拷数据库；而它可能不是 root，\n" +
			"  于是查询动作以一串 mkdir/cp 的失败收场，答案也取不回来。")
	}

	// 7) 两个入口都要把开关带到 --ask-mode 那一趟去。
	//    入口是"先问后下载"的那一端：答案取错了，后面下载与编译全白做。
	//    而且 set -eu 下引用未定义变量会当场中止，所以默认值也得在。
	for _, e := range []struct {
		name, body, need string
	}{
		{"install-master-bin.sh", binEntry, `_ask_args="$_ask_args --migrate-legacy"`},
		{"install-master.sh", srcEntry, "--ask-mode --migrate-legacy"},
	} {
		if !strings.Contains(e.body, e.need) {
			t.Errorf("%s 没把 --migrate-legacy 带进 --ask-mode 那一趟（找不到 %q）：\n"+
				"  用户加了开关却仍被问「做什么」，选 4 就白跑。", e.name, e.need)
		}
		if !strings.Contains(e.body, `MIGRATE_LEGACY=""`) {
			t.Errorf("%s 缺 MIGRATE_LEGACY 的默认值（set -u 下引用未定义变量会当场中止）", e.name)
		}
	}
}

// TestLegacyMigrationRewritesOldPathsInConfig 守"光搬文件不够"这件事。
//
// 老 master.yaml 是上一版脚本渲染出来的，里面**写死了绝对路径**：
//
//	data_dir:        "/var/lib/probe-master"
//	themes_dir:      "/var/lib/probe-master/themes"
//	master_key_file: "/etc/probe-master/master.key"
//
// 而新单元是 ProtectSystem=strict + ReadWritePaths=$DATA（见 5/7 写单元那一段），
// 配置仍指着老数据目录时，服务会因为"库在只读文件系统上"起不来；更糟的是回滚
// **不会改配置**，于是现象是"升级失败，原因不明"——正是这一整段代码要消灭的东西。
//
// 所以搬迁必须连这三个前缀一起改。判据：
//  1. 两条前缀改写都在，且排在"拷贝配置"之后（顺序反了改的是老目录里的原文件，
//     等于把回滚要用的那份也改了）；
//  2. 改写用"临时文件 + cat 覆盖"而不是 sed -i——sed -i 换 inode、权限按 umask
//     走，而这份文件里有 ACME Token（0640 root:probe 是有理由的）；
//  3. 手工迁移的说明里也得有这两条，否则照说明做的人会撞上同一个跟头。
func TestLegacyMigrationRewritesOldPathsInConfig(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	iCopy := strings.Index(body, `cp -a /etc/probe-master/. "$DIR"/`)
	iData := strings.Index(body, `s|/var/lib/probe-master|$DATA|g; s|/etc/probe-master|$DIR|g`)
	iInPlace := strings.Index(body, `cat "$_mig_tmp" >"$_mig_cfg"`)
	if iCopy < 0 || iData < 0 || iInPlace < 0 {
		t.Fatal("判据锚点丢了——搬迁块的结构变了，先改锚点再改实现。")
	}
	if iData < iCopy {
		t.Errorf("路径改写(%d)排在了拷贝配置(%d)之前：\n"+
			"  那样改的是 /etc/probe-master 里的**原文件**——回滚时要用它，\n"+
			"  而它自己也被改成了新路径。要改的是 $DIR 里的那一份。", iData, iCopy)
	}
	// 判"用的是原地覆盖"要靠**否定**坏写法：sed -i 在这里换 inode、权限按 umask
	// 走，而这份文件里有 ACME Token。写成"cat 在 sed 之后"太弱（两条都在就行，
	// 顺序无关），所以直接钉住"不许用 sed -i 改这份配置"。
	if strings.Contains(body, `sed -i "s|/var/lib/probe-master|`) {
		t.Error("搬迁用 sed -i 改 master.yaml：\n" +
			"  它换一个 inode、权限按 umask 走，而这份文件里有 ACME Token——\n" +
			"  0640 root:probe 不是装饰。用「临时文件 + cat 覆盖」原地写。")
	}
	if !strings.Contains(body, `cat "$_mig_tmp" >"$_mig_cfg"`) {
		t.Error("搬迁没有用 cat 原地覆盖回配置（属主/权限会丢）")
	}
	if !strings.Contains(body, `sed -i 's|/var/lib/probe-master|$DATA|g' $DIR/master.yaml`) {
		t.Error("手工迁移的说明里没给这两条 sed：\n" +
			"  照说明做的人会保留一份指向老目录的配置，然后撞上新单元的只读沙箱——\n" +
			"  同一个跟头摔两次，第二次还是在没有脚本兜底的情况下。")
	}
}
