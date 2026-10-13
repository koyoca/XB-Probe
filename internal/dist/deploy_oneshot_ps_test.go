package dist

import (
	"strings"
	"testing"
)

// TestWindowsScriptsMirrorTheShellOnes 守 Windows 侧的同一份能力。
//
// 为什么两个平台都要钉：这两条通路在用户眼里是"同一个功能"，而它们**各写一遍**。
// 只做 shell 侧的话，Windows 用户会看到文档里写着"可以只升级本体 / 可以改口令"，
// 自己那条命令却报「找不到与参数名 UpgradeOnly 匹配的参数」——而仓库里没人会发现
// （CI 跑在 Linux 上，ps1 根本不会被执行）。
//
// 判据挑的都是"顺序"与"不许出现什么"，因为 PowerShell 侧的失效方式和 shell 一样：
// 不是报错，而是**做多了**（顺手把配置重写一遍）或者**位置错了**（在问答之后才判，
// 于是用户先被问了一整轮）。
func TestWindowsScriptsMirrorTheShellOnes(t *testing.T) {
	ps := readRepoFile(t, "../../scripts/deploy-master.ps1")

	for _, sw := range []string{"[switch]$UpgradeOnly", "[switch]$ResetPassword"} {
		if !strings.Contains(ps, sw) {
			t.Errorf("deploy-master.ps1 没有 %s 这个开关。", sw)
		}
	}

	// ① 两个块必须排在"交互问答"**之前**。
	//    顺序错了的症状正是用户抱怨的那件事：选了"只升级本体"，还是被问一遍
	//    站点、证书、管理员。而两个块各自以 exit 收尾，所以"排在前面"就够了。
	iUp := strings.Index(ps, "# 0.3) 只升级 Master 本体")
	iRp := strings.Index(ps, "# 0.4) 重置管理员密码")
	iQA := strings.Index(ps, "# 0.5) 交互问答")
	iRel := strings.Index(ps, "# 0.2b) 只更新 Agent 发布文件")
	if iUp < 0 || iRp < 0 {
		t.Fatal("deploy-master.ps1 里找不到 0.3 / 0.4 两个模式块——判据已与脚本写法脱节。")
	}
	if iQA < 0 {
		t.Fatal("deploy-master.ps1 里找不到交互问答段——判据已与脚本写法脱节。")
	}
	if iUp > iQA || iRp > iQA || iRel > iQA {
		t.Error("两个新模式块排在了交互问答之后：\n" +
			"  那样一来「只升级本体」的用户还是会被问一遍站点、证书、管理员——\n" +
			"  正是这两条通路要消掉的东西。")
	}
	if !strings.Contains(ps, "[switch]$ReleaseOnly") {
		t.Error("deploy-master.ps1 没有 -ReleaseOnly 这个开关。")
	}

	up := shRegion(t, "scripts/deploy-master.ps1", ps,
		"# 0.3) 只升级 Master 本体", "# 0.4) 重置管理员密码")
	rp := shRegion(t, "scripts/deploy-master.ps1", ps,
		"# 0.4) 重置管理员密码", "if (-not $Bin -and -not (Test-Path $target))")

	// ②b 只更新 Agent 发布文件（-ReleaseOnly）：菜单第 3 项必须真的有实现。
	//
	// 这一条是**被一次事故逼出来的**：本轮先把 5 项菜单搬到了 Windows 侧，而那时
	// ps1 里根本没有 `dist pull` 这一步——菜单第 3 项写着"去 GitHub 查最新 Release
	// 把发布文件拉进 data_dir/dist/"，实际什么都不做。这正是 shell 侧早就钉过的
	// "菜单画了但分支没接"，到 Windows 侧重演了一遍，而且更糟：用户看到的是
	// **一句承诺**。
	//
	// 更硬的后果也在同一处：没有这一步，Windows 一键部署出来的 Master 里
	// data_dir\dist\ 是空的 → 新节点拿不到装机通道 → Agent 根本装不上。
	// Linux 侧早就修了（TestDeployPullsReleaseFilesForNodeBootstrap）。
	if iRel < 0 {
		t.Error("deploy-master.ps1 里没有 -ReleaseOnly 这一块：\n" +
			"  菜单第 3 项是对用户的一句承诺；没有它，data_dir\\dist\\ 会一直是空的，\n" +
			"  新节点拿不到装机通道（/dist/ 恒 404），Agent 装不上。")
	} else {
		rel := shRegion(t, "scripts/deploy-master.ps1", ps,
			"# 0.2b) 只更新 Agent 发布文件", "# 0.3) 只升级 Master 本体")
		for _, forbidden := range []string{
			"Set-Content",            // 写配置
			"Copy-Item",              // 换二进制
			"Register-ScheduledTask", // 动自启
			"Stop-ScheduledTask",
			"Start-ScheduledTask",
			"init-admin",
		} {
			if strings.Contains(rel, forbidden) {
				t.Errorf("0.2b（只更新发布文件）里出现了 %q：\n"+
					"  这个模式只往 data_dir\\dist\\ 里写文件。换二进制、重写配置、动自启\n"+
					"  都会把「只是更新 Agent 版本」变成一次没人预期的改动。", forbidden)
			}
		}
		// 必须真的调用 dist pull，而且要是**调用形式**（`& $bin @args`），
		// 不是注释里提一句。
		if !strings.Contains(rel, "& $roBin @pullArgs") {
			t.Error("0.2b 没有真的执行 dist pull——那发布文件是怎么来的？")
		}
		// **每一条分支**都得是 dist pull。只判"文件里有 `"dist", "pull"` 这句"
		// 是不行的：它有两条分支（全部平台 / 只拉本机），改坏其中一条，另一条
		// 会替它作证——而用户正好有可能走那条被改坏的。
		nAssign := strings.Count(rel, "$pullArgs = @(")
		nDist := strings.Count(rel, `$pullArgs = @("dist", "pull"`)
		if nAssign < 1 || nDist != nAssign {
			t.Errorf("0.2b 的 %d 条 dist pull 分支里有 %d 条不是 dist pull：\n"+
				"  改坏一条分支，另一条会替它作证——而用户正好可能走被改坏的那条。",
				nAssign, nAssign-nDist)
		}
		// 版本号必须过形态闸（纯数字点分），否则会把 HTML 错误页拼进 URL。
		if !strings.Contains(ps, `if ($tag -notmatch '^\d+(\.\d+)*$') { return "" }`) {
			t.Error("版本查询没有「纯数字点分」形态闸：\n" +
				"  解析失败时拿到的是别的文字，拼进 .../releases/download/v<那段文字> 之后\n" +
				"  症状变成「发布文件拉取失败」，根因被埋掉。")
		}
		// 判据必须落在磁盘上。
		if !strings.Contains(rel, "SHA256SUMS") || !strings.Contains(rel, "xb-probe-agent_*") {
			t.Error("0.2b 的成败判据没有落在磁盘上（SHA256SUMS + 产物计数）：\n" +
				"  /dist/ 受 install.mode 门控，刚部署完返回 404 是预期行为，\n" +
				"  拿 HTTP 状态码当判据会把每次正常部署都报成失败。")
		}
	}

	// ② 只换二进制：这些动作在 0.3 里一个都不许出现。
	//
	// 判据用"会写东西/会改注册"的**cmdlet 名**，而不是中文注释里的词：
	// 注释里必然出现"证书""管理员"这些字，靠 Contains 中文词判会把说明文字
	// 当成实现（这类假红在本项目已经踩过）。
	for _, forbidden := range []string{
		"Register-ScheduledTask", // 重注册开机自启（会连参数一起改写）
		"New-ScheduledTaskAction",
		"Set-Content", // 写文件——配置就是这么被重写的
		"init-admin",  // 初始化管理员
		"$TlsMode",    // 任何 TLS 处理
		"$AcmeEmail",
	} {
		if strings.Contains(up, forbidden) {
			t.Errorf("0.3（只升级本体）里出现了 %q：\n"+
				"  这个模式只换二进制并重启计划任务。用户明确说了「这些我不想做」。", forbidden)
		}
	}
	if !strings.Contains(up, "Invoke-UpgradeRollback") {
		t.Error("0.3 没有还原路径（备份了却不还原等于没备份）。")
	}
	if !strings.Contains(up, ".bak") {
		t.Error("0.3 覆盖前没有备份旧二进制：换上去起不来就没得还原了。")
	}
	// 真跑一次 version 再动手：这是本模式唯一"在动系统之前失败"的机会。
	if !strings.Contains(up, "& $Bin version") {
		t.Error("0.3 没有在停任务之前验「新二进制跑得起来」：\n" +
			"  等停了任务才发现装上去起不来，就白断了一次服务。")
	}
	if !strings.Contains(up, "Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue).State") {
		t.Error("0.3 起任务后没有验活：判据要挑「真的在跑」，不挑「命令没报错」。")
	}

	// ③ 只改口令：不许停任务、删文件、重注册、初始化管理员。
	for _, forbidden := range []string{
		"Unregister-ScheduledTask",
		"Stop-ScheduledTask",
		"Start-ScheduledTask",
		"Remove-Item",
		"Set-Content",
		"init-admin",
	} {
		if strings.Contains(rp, forbidden) {
			t.Errorf("0.4（只重置密码）里出现了 %q：\n"+
				"  用户是在「进不去后台」的时候用它，任何多余动作都会把救急手段变成事故。", forbidden)
		}
	}
	if !strings.Contains(rp, "reset-admin") {
		t.Error("0.4 没有调用 reset-admin——那口令是怎么改的？")
	}
	if !strings.Contains(rp, "--i-know-what-i-am-doing") {
		t.Error("0.4 没有带 reset-admin 的那道闸。")
	}
	if !strings.Contains(rp, "-AsSecureString") {
		t.Error("0.4 交互输入口令时没有用 -AsSecureString：\n" +
			"  口令会明文显示在屏幕上，而这一步恰恰是「旁边有人看着」也会发生的场景。")
	}
	if !strings.Contains(rp, "再输一遍") {
		t.Error("0.4 没有要求确认第二遍：打错一个字符的结局是「口令已更新」+ 登不进去。")
	}

	// ④ 互斥必须排在管理员检查**之前**：两者都是纯粹的"参数长得对不对"，
	//    被权限问题挡在前面的话，非管理员永远看不到那句真正有用的提示。
	//
	//    两边脚本里的集合变量名不同（部署脚本是 $oneShot，入口是 $oneShotIn），
	//    这里刻意钉各自的**判断条件**而不是变量名：改名不该让护栏变红，但把这段
	//    判断删掉或挪走必须变红。
	iExcl := strings.Index(ps, ".Count -gt 1")
	iAdmin := strings.Index(ps, "# 0.1d) 管理员权限检查")
	if iExcl < 0 || iAdmin < 0 {
		t.Fatal("deploy-master.ps1 里找不到互斥判断或管理员检查——判据已与脚本写法脱节。")
	}
	if iExcl > iAdmin {
		t.Error("互斥判断排在管理员检查之后：\n" +
			"  非管理员给了一对互相矛盾的开关，只会看到「需要管理员」，看不到\n" +
			"  「这两项不能同时用」——后者才是他真正要修的东西。")
	}
	// 同样要**逐个**钉：只判"有 .Count -gt 1 这个判断"，漏数一个模式也全绿。
	for _, fl := range []string{"-Uninstall", "-ReleaseOnly", "-UpgradeOnly", "-ResetPassword"} {
		if !strings.Contains(ps, `$oneShot += "`+fl+`"`) {
			t.Errorf("deploy-master.ps1 没有把 %s 计入互斥：\n"+
				"  矛盾的组合会静默做其中一件，而两条路都不报错。", fl)
		}
	}
	// -AskMode 放行是"先问后做"的前提：那一步必然发生在"还没决定要装什么"的时候。
	if !strings.Contains(ps, "-not $DryRun -and -not $pr.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)") {
		t.Error("deploy-master.ps1 的管理员检查里没有放行 -AskMode：\n" +
			"  它只打印一张菜单并把答案写到 stdout，是入口脚本「先问后做」的第一步，\n" +
			"  要求管理员权限等于把整条链路的入口堵死。")
	}

	// ⑤ 入口脚本：解析、转发、互斥。
	ips := readRepoFile(t, "../../install-master.ps1")
	for _, sw := range []string{"[switch]$UpgradeOnly", "[switch]$ResetPassword"} {
		if !strings.Contains(ips, sw) {
			t.Errorf("install-master.ps1 没有 %s 这个开关。", sw)
		}
	}
	if !strings.Contains(ips, `$dargs = @("-ResetPassword")`) {
		t.Error("install-master.ps1 解析了 -ResetPassword 却没有转给部署脚本：\n" +
			"  参数被读走之后什么也不做，是最难发现的一种失效。")
	}
	if !strings.Contains(ips, `$forward["UpgradeOnly"] = $true`) {
		t.Error("install-master.ps1 没有把 -UpgradeOnly 交给部署脚本：\n" +
			"  不交下去就等于「选 1 安装/升级」——整站问答又回来了。")
	}
	if !strings.Contains(ips, "$oneShotIn.Count -gt 1") {
		t.Error("install-master.ps1 没有入口级的互斥判断：\n" +
			"  它自己拼参数（而不是原样透传），被丢掉的那个永远到不了部署脚本，\n" +
			"  于是一个自相矛盾的组合会安安静静地做另一件事。")
	}
	// 光有"按个数判断"还不够——还得**每个**单次模式都被数进去。只判"有这个判断"
	// 的话，漏数一个模式照样全绿（本脚本的变异 M32 就抓到了这一处：删掉
	// `-ReleaseOnly` 那一行，护栏仍然说一切正常）。
	for _, fl := range []string{"-Uninstall", "-ReleaseOnly", "-UpgradeOnly", "-ResetPassword"} {
		if !strings.Contains(ips, `$oneShotIn += "`+fl+`"`) {
			t.Errorf("install-master.ps1 没有把 %s 计入入口处的互斥：\n"+
				"  不数它，它与别的单次模式同时给出时会被早退通路静默丢掉——\n"+
				"  用户以为做了 A、实际做了 B。", fl)
		}
	}
	// 取回来的部署脚本认不认识这几个新开关，必须先探再传：本脚本与它同源于
	// 同一个 ref 时才安全，用户把 -Ref 指到旧标签时就不是了。
	//
	// 判据钉在**那一份清单**上（两个开关写在同一个数组里），而不是各钉一条 if：
	// 只钉一条的话，将来加第三个忘记探就没人拦——而漏探的后果是 PowerShell 抛一句
	// 「找不到与参数名 X 匹配的参数」，既不说明原因也不说明"这次什么都没做"。
	if !strings.Contains(ips, `foreach ($probe in @("ResetPassword", "ReleaseOnly"))`) {
		t.Error("install-master.ps1 没有先探再传（-ResetPassword / -ReleaseOnly）。")
	}
	// 入口也要真的会问菜单（菜单只有一份，在部署脚本里），并且把 2/3/4/5 都映射上。
	// 少映射一个的后果不是"没这个功能"，而是**用户选了 A、脚本做了 B**。
	if !strings.Contains(ips, `-File $askScript -AskMode`) {
		t.Error("install-master.ps1 没有用 -AskMode 去问「做什么」：\n" +
			"  它必须先问再决定要不要装 Go / 要不要编译，否则选卸载的人要白等一次编译。")
	}
	for _, arm := range []string{`"2" { $Uninstall = $true }`, `"3" { $ReleaseOnly = $true }`,
		`"4" { $UpgradeOnly = $true }`, `"5" { $ResetPassword = $true }`} {
		if !strings.Contains(ips, arm) {
			t.Errorf("install-master.ps1 的菜单答案映射里缺 %q：\n"+
				"  用户答了这一项，脚本却按「安装」走——选了 A 做了 B。", arm)
		}
	}
}

// TestWindowsLegacyLayoutGuard 追 Windows 侧同一件事：改名之前（probe-master
// 时代）装过的机器，跑新脚本会按新路径写死 → 摸不到对象。shell 侧的对应判据
// 在 TestLegacyLayoutIsRefusedNotSilentlyBroken。Windows 不做自动搬迁（用得少、
// 牵涉 master.key），但必须同样**拦下来给能照着做的手工步骤**，且拦在
// 交互问答之前。
func TestWindowsLegacyLayoutGuard(t *testing.T) {
	ps := readRepoFile(t, "../../scripts/deploy-master.ps1")
	for _, m := range []string{
		`Get-ScheduledTask -TaskName "probe-master"`,
		`C:/ProgramData/probe-master`,
		`Unregister-ScheduledTask -TaskName probe-master`,
	} {
		if !strings.Contains(ps, m) {
			t.Errorf("Windows 侧老布局拦截缺关键件：%s", m)
		}
	}
	iMutex := strings.Index(ps, `$oneShot.Count -gt 1`)
	iGuard := strings.Index(ps, `0.1c-2) 老布局检测`)
	iRel := strings.Index(ps, `# 0.2b) 只更新 Agent 发布文件`)
	if iMutex < 0 || iGuard < 0 || iRel < 0 {
		t.Fatal("判据锚点丢了——ps1 结构与本测试的假设脱节，先改锚点再改实现。")
	}
	if iGuard < iMutex {
		t.Errorf("Windows 老布局拦截(%d)排在了互斥(%d)之前：\n"+
			"  互斥是更基本的参数合法性检查，应该先拦自相矛盾，再拦老布局。", iGuard, iMutex)
	}
	if iGuard > iRel {
		t.Errorf("Windows 老布局拦截(%d)排在了模式块(%d)之后：\n"+
			"  那样老布局机器会先走进 0.2b/0.3/0.4，摸不到对象才报错。", iGuard, iRel)
	}
}
