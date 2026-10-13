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
	if iUp < 0 || iRp < 0 {
		t.Fatal("deploy-master.ps1 里找不到 0.3 / 0.4 两个模式块——判据已与脚本写法脱节。")
	}
	if iQA < 0 {
		t.Fatal("deploy-master.ps1 里找不到交互问答段——判据已与脚本写法脱节。")
	}
	if iUp > iQA || iRp > iQA {
		t.Error("两个新模式块排在了交互问答之后：\n" +
			"  那样一来「只升级本体」的用户还是会被问一遍站点、证书、管理员——\n" +
			"  正是这两条通路要消掉的东西。")
	}

	up := shRegion(t, "scripts/deploy-master.ps1", ps,
		"# 0.3) 只升级 Master 本体", "# 0.4) 重置管理员密码")
	rp := shRegion(t, "scripts/deploy-master.ps1", ps,
		"# 0.4) 重置管理员密码", "if (-not $Bin -and -not (Test-Path $target))")

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
	// 取回来的部署脚本认不认识 -ResetPassword 必须先探再传：本脚本与它同源于
	// 同一个 ref 时才安全，用户把 -Ref 指到旧标签时就不是了。
	if !strings.Contains(ips, `if ($ResetPassword -and $deployText -notmatch "ResetPassword")`) {
		t.Error("install-master.ps1 没有先探再传 -ResetPassword：\n" +
			"  旧 ref 上的部署脚本会抛一句「找不到与参数名 ResetPassword 匹配的参数」，\n" +
			"  那句话既不说明原因、也不说明「这次什么都没做」。")
	}
}
