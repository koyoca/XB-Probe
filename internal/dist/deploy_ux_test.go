package dist

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 本文件钉的是**用户实际撞上的四个问题**。它们有一个共同点：
// 都属于"脚本照常跑完、看起来一切正常，而结果是错的"——所以不可能靠
// 运行一次脚本发现，只能靠断言把它钉住。
//
// 四条分别对应：
//
//	① 收尾打印的管理后台地址，不许由**监听地址**反推
//	② 公开站与管理端监听同一个地址端口，必须被拦住
//	③ 收尾的卸载提示，必须是用户能直接粘贴执行的命令
//	④ 答案记忆文件里不许出现口令与 DNS Token

// ---------------------------------------------------------------------------
// ① 管理后台地址不许由监听地址反推
// ---------------------------------------------------------------------------

// 用户填 0.0.0.0:8080，收尾却打印 https://127.0.0.1:8080/admin/。
//
// 三处错叠在一起：端口语义丢了（他填的与印出来的不一样）、地址是回环
// （只有本机能开）、协议是 https（tls.mode=none 时根本没有 https 监听）。
// 根因是拿 ADMIN_LISTEN（一个**监听**地址）当访问地址用。
//
// 判据只看**行首**的赋值语句：脚本里有一段注释记着这个错误原来的写法
// （那是有意留的"别再犯"的说明），把它也算成实现就成了"注释即代码"的笑话。
// 但**兜底分支**允许（也确实需要）用它——"既没有对外域名、也没有反代"的部署
// 只有本机地址可给，那种情况下它是对的，前提是必须同时给出 ADMIN_NOTE 说明
// 它只在本机有效（下面第二条判据钉这个）。
func TestAdminURLIsNotDerivedFromListenAddress(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	bad := `ADMIN_URL="$SCHEME://$ADMIN_ADDR/admin/"`
	hits := 0
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue // 注释里的历史记录，不是实现
		}
		if trimmed == bad {
			hits++
		}
	}
	if hits > 1 {
		t.Errorf("有 %d 处用监听地址拼管理后台地址（只允许「没有域名也没有反代」的兜底分支有 1 处）："+
			"ADMIN_ADDR 是把 0.0.0.0:8080 归一化成 127.0.0.1:8080 得来的，"+
			"端口语义丢了、地址必然只在回环有效、协议还可能与实际监听不符。", hits)
	}

	// 反向也要钉：必须真的存在"同站"这条分支（用 public_origin 推导后台地址）。
	// 只删掉错的那行、没补上正确的推导，用户拿到的会是空地址或回环地址。
	must := `ADMIN_URL="$(admin_url_from "$PUBLIC_ORIGIN")"`
	if !strings.Contains(body, must) {
		t.Errorf("没有找到「同站」分支：%s\n"+
			"  「同站」的正确表达是「后台在公开域名下的 /admin/」，"+
			"不是让两个端口监听同一个地址（那不可能）。", must)
	}

	// 用回环地址兜底时必须**说出来**。不说的话，用户会把它当成公网后台地址
	// 贴给别人或写进笔记——而这正是"地址错"这件事最实际的危害。
	if !strings.Contains(body, "这是**本机**地址") {
		t.Error("用本机回环地址兜底时没有明确标注它只在本机有效：" +
			"不加标注的话，用户会把它当成公网地址")
	}
}

// fakeBin 造一个存在的"二进制"文件。
//
// 脚本在 dry-run 之前会检查 --bin 指向的文件**存在**（这条检查是对的：
// 少了它，用户会先看到一大段配置预览、最后才被告知二进制找不到）。
// 这里只需一个能过 -f 的文件——dry-run 不会执行它。
func fakeBin(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "xb-probe-master")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("造临时二进制失败：%v", err)
	}
	return p
}

// ---------------------------------------------------------------------------
// ② 端口撞车必须被拦住
// ---------------------------------------------------------------------------

// 用户把管理端监听填成 0.0.0.0:8080（与公开站完全同端口）。
// 一个进程无法在同一地址端口上监听两次，Master 启动时报
//
//	listen tcp 127.0.0.1:8081: bind: address already in use
//
// 然后"已安全退出"——而那句报错很像"端口被别的程序占了"，用户无从下手。
//
// 这条护栏跑真脚本：它必须在**改动系统之前**就以非 0 退出，并把原因说清楚。
func TestSameListenPortIsRejected(t *testing.T) {
	out, rc := runDeployScript(t,
		"--bin", fakeBin(t),
		"--dry-run",
		"--public-listen", "0.0.0.0:8080",
		"--admin-listen", "0.0.0.0:8080",
	)
	if rc == 0 {
		t.Fatalf("公开站与管理端填了同一个地址端口，脚本却成功了（rc=0）。\n输出：\n%s", out)
	}
	if !strings.Contains(out, "同一个地址端口") {
		t.Errorf("拒绝的理由没说清楚（应当点明「同一个地址端口」并解释一个进程监听不了两次）。\n输出：\n%s", out)
	}
}

// 反过来说：端口**只差地址**、端口不同就必须放过。
// 少了这一条，上面那条会退化成"只要两个 listen 字符串有点像就拒绝"。
func TestDifferentListenPortsAreAccepted(t *testing.T) {
	out, rc := runDeployScript(t,
		"--bin", fakeBin(t),
		"--dry-run",
		"--public-listen", "0.0.0.0:8080",
		"--admin-listen", "127.0.0.1:8081",
	)
	if rc != 0 {
		t.Fatalf("两个不同的端口应当被接受，实际 rc=%d。\n输出：\n%s", rc, out)
	}
	// 顺带确认 dry-run 真的跑到了"打印将要写入的配置"这一步——
	// 否则"rc=0"可能只是它在更早的地方安静地退出了。
	if !strings.Contains(out, "admin_listen") {
		t.Fatalf("dry-run 没有打印出将要写入的配置，rc=0 说明不了任何事。\n输出：\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// ③ 卸载提示必须可执行
// ---------------------------------------------------------------------------

// 用户是 `sh -c "$(curl …)"` 跑起来的，本机**没有** deploy-master.sh。
// 而收尾那句 `sh deploy-master.sh --uninstall` 假设了它存在。
//
// 判据：提示必须根据"入口是谁"给出不同命令，而不是写死一句本地路径。
func TestUninstallHintMatchesHowUserStartedTheScript(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	// 三种入口各一条：免编译 / 源码 / PowerShell。
	// 每条都要能在用户机器上直接粘贴执行。
	for _, want := range []struct{ name, marker string }{
		{"免编译入口（install-master-bin.sh）", "install-master-bin.sh"},
		{"源码入口（install-master.sh）", "install-master.sh"},
		{"Windows 入口（install-master.ps1）", "install-master.ps1"},
	} {
		if !strings.Contains(body, want.marker) {
			t.Errorf("卸载提示里没有 %s 的还原命令：入口不同，卸载的命令也不同。\n"+
				"  对 `curl … | sh` 跑起来的人，本机没有 deploy-master.sh——"+
				"那个文件从来没被保存到磁盘上。", want.name)
		}
	}

	// 环境变量必须是入口**传下来**的，而不是脚本自己猜的。
	// 猜（比如去 /proc 里翻父进程命令行）在 `sh -c` 这种形态下拿不到东西。
	if !strings.Contains(body, "PROBE_SELF_ENTRY") {
		t.Error("没有 PROBE_SELF_ENTRY：卸载提示无从知道用户是怎么把脚本叫起来的")
	}

	// 两个入口都必须真的把它 export 出去。只在一个入口设置，
	// 另一个入口的用户就还会看到那句不可执行的提示。
	for _, rel := range []string{
		"../../install-master-bin.sh",
		"../../install-master.sh",
	} {
		b := readRepoFile(t, rel)
		if !strings.Contains(b, "PROBE_SELF_ENTRY") {
			t.Errorf("%s 没有设置 PROBE_SELF_ENTRY —— 从它进来的用户"+
				"看到的卸载提示仍然是本地路径", rel)
		}
	}
}

// ---------------------------------------------------------------------------
// ④ 答案记忆里不许有敏感项
// ---------------------------------------------------------------------------

// 答案文件是为了"改一项不必把前面全重答一遍"（用户第 4 点），
// 它记的是端口、地址、域名这类**非敏感**项。
//
// 口令与 DNS API Token 一概不记：它们落在一个磁盘上的明文文件里（哪怕 0600），
// 只是把"写进 shell 历史"这个问题换了个地方发生。
//
// 判据是"写文件的那一段里不许出现这两个键"，而不是"检查文件内容"——
// 后者要有一次真实运行才能验，而这条不变量必须在**代码层面**就成立。
func TestAnswersFileNeverStoresSecrets(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	// ans_put 是唯一的写入口。逐个调用点看它写了什么键。
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "ans_put ") {
			continue
		}
		for _, secret := range []string{"ADMIN_PASSWORD", "ACME_DNS_TOKEN"} {
			if strings.Contains(trimmed, secret) {
				t.Errorf("答案文件里写了敏感项：%s\n"+
					"  口令与 DNS Token 不该落进任何明文文件——"+
					"它们已经刻意不走命令行参数了，答案文件是同一个理由。\n"+
					"  想拦住这类改动，ans_put 里也有一道运行时的拒绝（见那里的 case）。", trimmed)
			}
		}
	}

	// 反向：必须真的在写点什么，否则上面那个循环空转、这条护栏形同虚设。
	if n := strings.Count(body, "\nans_put "); n == 0 {
		t.Fatal("脚本里一个 ans_put 调用都没有——判据与脚本写法脱节了，这条护栏已经形同虚设")
	}

	// ans_put 内部还要有一道防线：即使有人在别处误加了一个敏感键，
	// 也不该写进文件。两道判据看着冗余，但它们验的是不同的东西：
	// 上面那条验"当前没有这样的调用"，这条验"将来也不会写出这样的内容"。
	if !strings.Contains(body, "ACME_DNS_TOKEN | ADMIN_PASSWORD) return 0") &&
		!strings.Contains(body, "ADMIN_PASSWORD | ACME_DNS_TOKEN) return 0") {
		t.Error("ans_put 里没有敏感项的黑名单：它只靠调用方自觉，那是一层会被随手绕过的约定。")
	}
}

// ---------------------------------------------------------------------------
// ⑤ Caddyfile 存在 ≠ 配好了：不许因为"文件存在"就跳过写入
// ---------------------------------------------------------------------------

// 真机反馈：脚本打印"反代已配好、部署完成"，用户 netstat 却发现 Caddy
// **只开了 80、没有 443**，https 打不开。
//
// 根因是判据用错了：`[ -f "$CADDYFILE" ]` 就保留。这台机器上 Caddy 是别人
// （发行版包 / x-ui 面板 / 更早的 nginx 迁移）装的，那份配置里根本没有我们要
// 反代的域名——保留它不是"尊重既有配置"，而是**让 Caddy 没有 443 站点**。
//
// 正确判据是"里面有没有本次要用的那个域名"，且失败时要能当场验出来。
func TestCaddyfileIsNotKeptJustBecauseItExists(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	// 判据一：不许出现"只看文件存在就保留"。
	//
	// 结构在第七轮改过：现在"保留"由 `elif Caddyfile_has_domain "$_caddy_dom"; then`
	// 触发（在此之前的 elif 处理"是我们写的"与 --force-config）。
	// 所以判据不再是"某个 if 的形式"，而是"**保留这个动作必须经过域名判据**"。
	//
	// 只看**行首的 if/elif**：分支体里也会出现同样的子串（例如
	// `elif grep -Eq "^[[:space:]]*[^#{]" "$CADDYFILE"` 是"追加"分支，它本来就该
	// 在"没有目标域名"时走到）。用全行 Contains 判会把正确实现也判红。
	sawGuardedIf := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(trimmed, "if ") && !strings.HasPrefix(trimmed, "elif ") {
			continue
		}
		if strings.Contains(trimmed, "Caddyfile_has_domain") {
			sawGuardedIf = true
		}
		// "文件存在 + 未 force-config"这种组合如果出现在分支条件里、且没有域名判据，
		// 那就是回到了"存在就保留"的老路。
		if strings.Contains(trimmed, `-f "$CADDYFILE"`) &&
			strings.Contains(trimmed, "FORCE_CONFIG") &&
			!strings.Contains(trimmed, "Caddyfile_has_domain") {
			t.Error("Caddyfile 的保留判据只看「文件存在」——\n" +
				"  本机 Caddy 常是别人装的（发行版包、x-ui、更早的 nginx 迁移），\n" +
				"  那份配置里没有我们要反代的域名，保留它等于 Caddy 不开 443、拿不到证书。\n" +
				"  判据要换成「里面有没有本次要用的域名」（见 Caddyfile_has_domain）。\n" +
				"  实际写法：" + trimmed)
		}
	}
	if !sawGuardedIf {
		t.Fatal("「保留既有站点块」不再经过域名判据：\n" +
			"  判据与脚本写法脱节了（脚本改过结构却没同步这里），这条护栏已经形同虚设。")
	}

	// 判据二：必须真的检查域名在不在文件里，且用的是行首站点标签而非子串。
	// 用子串的话，一句注释里顺嘴提到域名就能让判据失效。
	if !strings.Contains(body, "Caddyfile_has_domain()") {
		t.Fatal("脚本里没有 Caddyfile_has_domain 判据：Caddyfile 的复用/追加/覆盖" +
			"就没有依据，只能退回到「文件存在就跳过」那个错误行为。")
	}
	if !strings.Contains(body, `^[[:space:]]*$1(:[0-9]+)?[[:space:]]*\{`) {
		t.Error("Caddyfile_has_domain 用的不是「行首站点标签」判据：\n" +
			"  子串匹配会让注释里出现的域名也算命中——一句话就能让它失效。")
	}

	// 判据三：caddy 模式下必须**当场验证 443 与证书**，不许只说"稍后验证"。
	// 这正是用户撞上的那个缺口：脚本跑完一切正常，只有 netstat 能看出问题。
	if strings.Contains(body, "Caddy 负责申请与续期（它自己会重试），稍后验证 https 是否可用") {
		t.Error("caddy 分支仍在用「稍后验证 https 是否可用」收尾——\n" +
			"  用户上次就是这样被骗过的：脚本说部署完成，实际 443 没在听。\n" +
			"  必须当场请求一次 https://<域名>/api/public/v1/site，失败即回滚并给出诊断。")
	}
	if !strings.Contains(body, `https://$_cd_dom/api/public/v1/site`) {
		t.Error("caddy 分支没有真的去请求一次 https 站点：\n" +
			"  「caddy 进程活着」不足以说明反代生效——它当然活着，还开着 80。")
	}

	// 判据四：这次探测必须**在本机内闭环**（--resolve 指到回环，SNI/Host 仍是域名），
	// 不能拿"本机访问自己的公网域名"当成败依据。
	//
	// v0.1.3 首次部署真机撞到：这台主机的公网 IP 由机房做映射、本机只有私网地址，
	// **回不到自己的公网 IP**。于是 Caddy 明明起来了、证书早就在
	// /root/.local/share/caddy 里（日志里连一行 tls.obtain 都没有）、站外打开正常，
	// 本机 curl 却 60 秒不通 → 脚本判定"反代未生效"，把一次成功的部署整份回滚，
	// 站点反而被脚本自己弄下线。判据问错了对象：外面能不能进来属于网络拓扑
	// （机房/安全组放不放行），不是这次安装装坏了什么。
	if !strings.Contains(body, `--resolve "$_cd_dom:443:127.0.0.1"`) {
		t.Error("caddy 分支的可用性判据不是「本机回环探测」：\n" +
			"  用本机访问自己的公网域名当成败依据，在「公网 IP 由机房映射」的主机上\n" +
			"  必然误判失败，并把本来成功的部署回滚掉。\n" +
			"  判据要连 127.0.0.1:443、SNI 与 Host 仍用域名；公网回路那一趟只能警告。")
	}
}

// ---------------------------------------------------------------------------
// ⑦ "已经答复了但状态不对"必须当场认出来，不许再猜
// ---------------------------------------------------------------------------

// 真机（v0.1.3）第二轮：回环探测 60 秒不通，脚本把原因猜成"证书没拿到 /
// 域名没解析 / 端口被占 / 反代目标不通"四条，逐条都是错的。
//
// 实际形态是：请求**已经穿过 Caddy 到达 Master**，被服务端的域名门控拒了
// （站点形态按域名判，配置里的公开域名不是用户正在用的那个）。这一类
// "已经有答复、只是状态不对"与"连接层根本没通"是两种完全不同的病，
// 混在一句"打不开"里就只能靠猜。这条钉四件事：
//
//	① 两层分开判（连接层 vs HTTP 层），HTTP 层非 2xx 立即停手，不再空等；
//	② curl 的原话要打出来——以前丢进 /dev/null，最要紧的事实从未露面；
//	③ 回环探测必须 --noproxy（设了 https_proxy 的机器上，curl 会把请求交给
//	   代理，--resolve 一并失效，现象恰好是"本机连不上自己的回环"）；
//	④ 反代在前的健康检查必须带对外域名当 Host，否则按回环 Host 判必被拒——
//	   这个"健康检查"以前永远看不出服务行不行。
func TestSiteGateRejectionIsDiagnosedInsteadOfGuessed(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	// ① 连接层与 HTTP 层分开：必须有一个"已经答复了"的旁路标志。
	if !strings.Contains(body, `_cd_hf="$_cd_code"`) {
		t.Error("caddy 就绪探测没有把「已经答复了但状态不对」单独拎出来：\n" +
			"  连接层没通（证书可能还在申请）值得等；服务端已经答复了就是配置问题，\n" +
			"  再等 60 秒也是同一个答案。混在一起就只能把两种病报成一句「打不开」。")
	}
	if !strings.Contains(body, `_cd_code="000"`) {
		t.Error("探测没有区分「连不上」与「HTTP 状态」：\n" +
			"  必须自己拿状态码分类（2xx 就绪 / 000 继续等 / 其余当场停）。")
	}

	// ② curl 的原话必须露面。
	if !strings.Contains(body, `_cd_err="$(curl`) || !strings.Contains(body, "（退出码 $_cd_rc）") {
		t.Error("连接失败时没有把 curl 的原话打出来：\n" +
			"  以前整段 2>/dev/null，用户屏幕上永远只有「60 秒内仍不可用」，\n" +
			"  而真正的原因（连不上/证书/代理）就在 curl 的报错里。")
	}

	// ③ 回环探测不许经过代理。这条不是洁癖：本机的开发环境里就有
	//    https_proxy，一次探死端口的实验直接返回了代理的 "HTTP 502 ←
	//    upstream connect failed"，也就是把「连不上」伪装成「服务端答复了」。
	if n := strings.Count(body, `--noproxy '*'`); n < 3 {
		t.Errorf("回环探测只有 %d 处带 --noproxy '*'（至少要覆盖：就绪轮询、公网回路、分段探测）：\n"+
			"  设了 http_proxy/https_proxy 的机器上，curl 会把请求交给代理，--resolve 随之失效，\n"+
			"  现象恰好是「本机连不上自己的回环」——而 Caddy 那边一行日志都不会有。", n)
	}
	if !strings.Contains(body, "https_proxy=") || !strings.Contains(body, "回环探测已用 --noproxy") {
		t.Error("设了代理时没有一句提示：\n" +
			"  「本机连不上自己的回环」在代理环境下是必然的，得让用户知道探测绕过了代理。")
	}

	// ④ 统一出口：421 要说清"域名不在白名单"以及两条可执行的修法。
	if !strings.Contains(body, "site_gate_advice()") {
		t.Fatal("没有 site_gate_advice 这个统一出口：\n" +
			"  健康检查与就绪探测都会撞上「服务端拒绝」，两处各写一遍必然漂移。")
	}
	for _, want := range []string{
		"421 = 域名不在白名单",
		"加 --force-config",
		"public_origin|public_hosts|admin_origin|admin_hosts",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("421 的诊断里缺 %q：\n"+
				"  用户拿到的必须是「哪一层拒的 + 现在配置里写的什么 + 怎么改」，\n"+
				"  而不是让它自己去猜证书、DNS、端口。", want)
		}
	}

	// ⑤ 反代在前的健康检查要带对外域名当 Host。
	if !strings.Contains(body, `-H "Host: $HC_HOST"`) {
		t.Error("caddy 模式下的健康检查没有带对外域名当 Host：\n" +
			"  站点形态按域名门控，拿 127.0.0.1 当 Host 必被 421；于是这个检查\n" +
			"  永远看不出服务到底行不行，只是白等 20 秒再打印一段状态。")
	}

	// 反向：四条猜测的旧文案不许回潮——它把"配置不认这个域名"整类问题
	// 都指向了证书与端口，用户按它查只会越查越远。
	if strings.Contains(body, "1) 证书没拿到（域名没解析到本机 / 80 端口不通 / CA 拒绝/限流）") {
		t.Error("caddy 就绪失败的诊断又回到了四条猜测：\n" +
			"  那四条在真机上条条不成立（证书早在缓存里、域名解析正常、443 没被占、\n" +
			"  反代目标活着）。要按「分段探测的结果」说话，而不是列可能性。")
	}
}

// ⑨ 同一个 4xx 的三种来源，必须按「服务端的原话」分开（v0.1.3 真机第三轮）。
//
// 上一版把 404 解释成"脚本与二进制版本错配"——那是猜测（真机里版本是配套的）。
// 同一个 404 至少三种来源，修法毫不相干：
//
//	· Master 自己的 JSON「公开接口已关闭」——安装是对的，是"公开看板"开关的事；
//	· Go 原生的 "404 page not found"      —— 这条路根本没注册；
//	· 两者都不是（HTML/代理的错误页）      —— 这个 404 压根不是 Master 回的。
//
// 所以判据只能是"把原话读出来"，并且**按归因决定要不要回滚**：能归因为运行期
// 状态的只警告——拿它回滚会把一次本来成功的安装整份撤掉（连 caddy 一起停），
// 站点反而被脚本自己弄下线。
func TestRejectionIsClassifiedByServerOwnWords(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	// ① 健康检查必须取回响应体；-o /dev/null 那种写法不许回潮。
	if !strings.Contains(body, `_hc_body="$(printf`) {
		t.Error("健康检查只留状态码、把响应体丢进 /dev/null：\n" +
			"  同一个 404 的三种来源只能靠原话分开，丢掉它就只能猜。")
	}
	if strings.Contains(body, `-o /dev/null -w '%{http_code}' $CURL_EXTRA`) {
		t.Error("健康检查又回到 -o /dev/null 了：服务端的原话必须露面。")
	}
	if !strings.Contains(body, "服务端的原话（先看这行——它通常直接说明了原因）") {
		t.Error("健康检查失败时没有把服务端的原话打给用户看。")
	}

	// ② 三个分类分支都在。
	for _, want := range []string{
		"公开接口已关闭",
		"page not found",
		"大概率**不是 Master 回的**",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("4xx 的分类里缺 %q —— 三种来源的修法毫不相干，必须逐条说清。", want)
		}
	}

	// ③ 按归因决定回滚：出口是 _DGA_FATAL，两处调用点各判一次。
	if !strings.Contains(body, `_DGA_FATAL="0"`) {
		t.Error("没有「运行期状态 → 不回滚」的分支（_DGA_FATAL=\"0\"）：\n" +
			"  看板被关掉这种原因，回滚也治不好它，只会把装好的东西撤掉。")
	}
	if n := strings.Count(body, `if [ "$_DGA_FATAL" = "1" ]; then`); n < 2 {
		t.Errorf("只有 %d 处按 _DGA_FATAL 决定是否回滚（健康检查与 Caddy 就绪探测各需一处）：\n"+
			"  少一处就是「有的地方警告、有的地方照旧撤安装」。", n)
	}

	// ④ 现场证据：4xx 是不是我们自己回的，必须给证据而不是给猜测。
	if !strings.Contains(body, "probe_scene_evidence()") {
		t.Fatal("没有 probe_scene_evidence：\n" +
			"  真机上「Master 拒绝服务」很可能压根不是 Master 回的（旧进程占着端口/代理代答），" +
			"不给证据就只能靠猜。")
	}
	for _, want := range []string{
		"谁在听 ",
		"环境里的代理变量",
		"系统里与本项目有关的单元",
		"Master 启动日志（含最终生效的站点形态与域名清单）",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("现场证据里缺 %q：\n"+
				"  这四条分别对应「端口被占」「代理代答」「遗留服务」「后台设置顶替配置文件」。", want)
		}
	}

	// ⑤ 回环探测一概不许经过代理——健康检查这两条探测路径以前漏了 --noproxy。
	if n := strings.Count(body, `--noproxy '*'`); n < 6 {
		t.Errorf("--noproxy '*' 只有 %d 处：健康检查的探测也必须带上——\n"+
			"  代理会把「服务端回了什么」替换成「代理回了什么」（状态码与响应体一起变）。", n)
	}

	// 反向：把 404 归因到"版本错配"的旧文案不许回潮。
	if strings.Contains(body, "通常是**脚本与二进制版本错配**") {
		t.Error("404 又回到「版本错配」这条猜测上：真机里脚本与 v0.1.3 都有这个路由。")
	}

	// ⑥ Windows 侧是同一件事：健康检查必须记住"服务端答复了什么"，
	//    并把 4xx 与"还没起来"分开（以前两种都报成"没探到公开 API"）。
	ps := readRepoFile(t, "../../scripts/deploy-master.ps1")
	for _, want := range []string{"$probeCode", "$probeBody", "公开接口已关闭", "if ($probeCode -ge 400 -and $probeCode -lt 500) { break }"} {
		if !strings.Contains(ps, want) {
			t.Errorf("deploy-master.ps1 的健康检查缺 %q：\n"+
				"  两个平台对同一个现象必须给同一类判据，否则一边能看出原因、一边只能猜。", want)
		}
	}
}

// ⑩ 改 master.yaml 不许用 `sed -i`（脚本自己改与教用户改都不行）。
//
// 这份文件里有 ACME Token，权限是 0640 root:probe。原位替换（-i）会换 inode、
// 按 umask 重建权限位：轻则变成 0644（凭证泄露），重则 0600 root:root
// （服务读不到配置，下次重启直接起不来）。两个改动点都必须是
// "先写临时文件、再覆盖回原 inode"——覆盖写动的是原 inode，属主与权限位一个不动。
func TestMasterConfigIsNeverEditedInPlace(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	// 真正的调用才拦：注释里为了说明这个坑会提到它，那种提及是好的。
	for _, bad := range []string{`sed -i '`, `sed -i "`} {
		if strings.Contains(body, bad) {
			t.Errorf("部署脚本里出现了 %s：\n"+
				"  master.yaml 含 ACME Token（0640 root:probe），原位替换会重建权限位。", bad)
		}
	}
	// ① 脚本自己改 sync_obtain：临时文件 + cat 覆盖。
	if n := strings.Count(body, `cat "$_sy_tmp" >"$CFG"`); n != 1 {
		t.Errorf("sync_obtain 的回写方式不对（找到 %d 处「覆盖回原 inode」）：\n"+
			"  要 mktemp 出临时文件、sed 写进去，再 cat 覆盖回来。", n)
	}
	// ② 教用户的「删掉某个键」也必须给这一套，不能给 sed -i。
	if !strings.Contains(body, "> $CFG.tmp") || !strings.Contains(body, "cat $CFG.tmp > $CFG") {
		t.Error("给用户的「只删掉这一行」修法还是原位替换：\n" +
			"  脚本自己避开的坑不该让用户踩，而且用户那条命令是 root 跑的。")
	}
}

// ---------------------------------------------------------------------------
// ⑥ 脚本写出的配置键，二进制必须认识（否则严格解析让服务起不来）
// ---------------------------------------------------------------------------

// 真机反馈：
//
//	启动失败： 解析配置失败: yaml: unmarshal errors:
//	  line 16: field sync_obtain not found in type config.ACMEConfig
//
// 根因是**脚本与二进制的版本错配**：配置模板来自 main（会写新键），而
// install-master-bin.sh 下载的二进制来自 Release 标签（不认识新键）。而
// LoadMaster 是严格解析（KnownFields(true)），写下去＝服务启动即失败。
//
// 这条护栏钉两件事：① 写这类新键之前必须探测；② 探测必须问二进制本人。
func TestConfigKeysAreProbedBeforeWriting(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	if !strings.Contains(body, "binary_supports_config_key()") {
		t.Fatal("脚本里没有 binary_supports_config_key：\n" +
			"  配置模板来自 main、二进制来自 Release 标签，两者会错配；\n" +
			"  而 LoadMaster 严格解析未知键，写下去就是「服务起不来」。")
	}

	// sync_obtain 是本轮新增键的典型：写入必须被探测包住。
	// 判据看的是"这个键的 echo 行是否在 if binary_supports_config_key 之下"，
	// 而不是"文件里有没有这个字符串"——后者在注释里也命中。
	lines := strings.Split(body, "\n")
	guarded := false
	for i, line := range lines {
		if !strings.Contains(line, "sync_obtain: true") {
			continue
		}
		// 往上找最近的 8 行，看有没有一道 binary_supports_config_key 闸。
		lo := i - 8
		if lo < 0 {
			lo = 0
		}
		for j := lo; j < i; j++ {
			if strings.Contains(lines[j], "binary_supports_config_key sync_obtain") {
				guarded = true
			}
		}
	}
	if !guarded {
		t.Error("sync_obtain 的写入没有被 binary_supports_config_key 包住：\n" +
			"  旧二进制（Release 标签那份）不认识这个键，严格解析会让服务启动失败——\n" +
			"  这条正是用户在真机上撞到的 `field sync_obtain not found`。")
	}

	// 探测必须先证明"自己真的读到了我们给它的那份文件"——这就是哨兵的作用。
	//
	// 没有哨兵时，探测在**旧二进制**上会恒回答"认识"（假阳性，用户撞到的正是这个）：
	//   `xb-probe-master tls status -config X` 在没有 tls 子命令的二进制上会 fall through
	//   到服务启动，而 flag.Parse 遇到非 flag 参数 `tls` 就停止解析 → `-config X`
	//   被忽略 → 于是"基线"与"待测"两次调用读的是**同一份**真实配置 → 恒"认识"。
	// 哨兵一次覆盖"子命令不存在"与"-config 被忽略"两种失效——它们症状相同，
	// 光看待测键那一次的输出分辨不出来。
	//
	// 这个方向比"基线"更硬：基线只能证明"配置没写错"，证明不了"它读了我们的文件"。
	if !strings.Contains(body, "PROBE_SENTINEL_KEY") {
		t.Error("探测没有哨兵自检：\n" +
			"  旧二进制没有 tls 子命令时，`tls status -config X` 会 fall through 到服务启动、\n" +
			"  `-config X` 被忽略 → 两次调用读同一份真实配置 → 探测恒回答「认识」，\n" +
			"  于是脚本写下二进制不认识的键，服务随即拒绝启动（真机撞到的就是这个）。")
	}
	// 哨兵判据必须要求**点名**拒绝哨兵键。只判"有没有报错"不行：
	// 通道失效时（读的是默认配置）也可能因为别的原因报错，那也会被当成"通道有效"。
	if !strings.Contains(body, `*"field $PROBE_SENTINEL_KEY not found"*`) {
		t.Error("哨兵判据太松：必须要求二进制**点名**拒绝哨兵键：\n" +
			"  只判断「有没有 not found」会把读默认配置时的其它错误当成通道有效。")
	}
	if !strings.Contains(body, "_probe_channel_ok()") {
		t.Error("哨兵检查没有抽成独立的 _probe_channel_ok：\n" +
			"  探测与「配置能否解析」两处都要用它，各写一份迟早有一份忘了同步。")
	}
	// 光"文件里有 _probe_channel_ok"**不够**：它可以存在却没人调用它
	// （变异验证抓到的正是这个：把探测里那句闸删掉，上面三条断言全绿）。
	// 必须断言 binary_supports_config_key 的**函数体**里真的用了它。
	start := strings.Index(body, "binary_supports_config_key() {")
	if start < 0 {
		t.Fatal("找不到 binary_supports_config_key 的定义")
	}
	// 函数体＝从定义到下一个**行首**的 `}`（函数体内的 `}` 都有缩进）。
	relEnd := strings.Index(body[start:], "\n}")
	if relEnd < 0 {
		t.Fatal("binary_supports_config_key 的函数体没有结束")
	}
	fnBody := body[start : start+relEnd]
	if !strings.Contains(fnBody, "_probe_channel_ok") {
		t.Error("binary_supports_config_key 体内没有用 _probe_channel_ok 做闸：\n" +
			"  哨兵函数存在 ≠ 探测真的用了它；少了这一句，旧二进制上探测又变成恒「认识」。")
	}
	if !strings.Contains(body, `public_listen: "127.0.0.1:0"`) {
		t.Error("探针配置里的监听地址没有加引号：\n" +
			"  `127.0.0.1:0` 不加引号在 YAML 里会被当成映射键（`did not find expected key`），\n" +
			"  探测结果就全部不可信了。")
	}
}

// 配置落盘之后必须**立刻**用二进制自己验一遍，而不是等到服务起不来。
//
// 真机日志里第 4 步（init-admin）已经打印了 `field sync_obtain not found`，
// 脚本却照常往下走：写 systemd 单元、启动服务、失败、回滚——而回滚给出的理由是
// "启动后立即退出"，离真正的原因（配置与二进制版本错配）很远。
//
// 这条钉两个位置：
//
//	① 第 3 步写完/保留配置之后要有一次校验；
//	② 第 4 步 init-admin **不许**用管道吃掉退出码。
//	   `cmd 2>&1 | sed ... || echo`（曾经的写法）的退出码是 sed 的，永远是 0，
//	   于是 init-admin 的任何失败都被静默吞掉。
func TestConfigIsValidatedRightAfterWriteAndInitAdminKeepsItsExitCode(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	if !strings.Contains(body, "config_parses_ok \"$CFG\"") {
		t.Error("第 3 步写完配置后没有用二进制校验它能被解析：\n" +
			"  上一轮写坏的配置会被这一轮原样保留，不在这里拦住的话，用户要等\n" +
			"  输完口令、服务起不来、回滚之后才看到原因，而回滚的理由是「启动后立即退出」。")
	}
	// 校验失败必须有明确的诊断出口（两处共用同一份，免得一份忘了更新）。
	if !strings.Contains(body, "fail_config_mismatch()") {
		t.Error("没有 fail_config_mismatch：配置与二进制不匹配时，\n" +
			"  必须报出「两边版本不一致」并给出可执行的修法，而不是笼统的「启动失败」。")
	}
	if !strings.Contains(body, "_ia_rc=$?") {
		t.Error("第 4 步 init-admin 没有保存真实退出码：\n" +
			"  `cmd 2>&1 | sed ... || echo` 的退出码是**管道最后一个命令**（sed）的，永远是 0，\n" +
			"  于是 `||` 那半句永不执行、init-admin 的失败被静默吞掉，脚本继续往下走。")
	}
	// 反向：不许再出现"管道 + ||"那种把退出码吃掉的组合。
	// 只看"有没有 _ia_rc=$?"不够——两处可以并存（新加的判据被旧的管道绕过）。
	if strings.Contains(body, `| sed 's/^/    /' || \`) {
		t.Error("第 4 步仍存在 `cmd | sed ... || ...` 的写法：\n" +
			"  管道的退出码是 sed 的，init-admin 的失败会被吞掉。先把输出收进变量再自己判。")
	}

	// 反向之二：**拿到退出码还不够，赋值本身必须在 `set -e` 之外**。
	//
	// 脚本开头是 `set -eu`，而"只有赋值的简单命令"（`x="$(cmd)"`）的退出码就是
	// 命令替换的退出码：cmd 失败时 `set -e` 会在那一行把整个脚本干掉，紧跟其后的
	// `_ia_rc=$?`、printf、`已存在` 判定、错误提示与 rollback 全都不执行。
	//
	// 真机现象（v0.1.3 重跑：上一次装到一半失败回过滚、库里已经有 admin，
	// 于是 init-admin 返回"已存在"这个**预期内**的非 0）：第 4 步打完口令提示后
	// 一行输出都没有、也没有回滚，只剩入口脚本一句"部署脚本执行失败"，
	// 看上去像"口令不对"。
	//
	// 唯一写法是放进 AND-OR 列表（`|| xxx_rc=$?`）——顺带也让函数体内的
	// `x="$(…)"` 脱离 -e（POSIX：-e 被忽略的上下文里，其内部任何命令都不导致
	// shell 退出），`config_parses_ok` 那条正是靠这一点才不会死在自己身上。
	for _, want := range []string{`|| _ia_rc=$?`, `|| _rp_rc=$?`, `|| _cp_rc=$?`} {
		if !strings.Contains(body, want) {
			t.Errorf("退出码没有在 set -e 之外拿到（缺 %s）：\n"+
				"  `x=\"$(cmd)\"` 里 cmd 失败时，set -e 会当场杀掉整个脚本——\n"+
				"  输出、诊断、回滚全都不执行，用户只看到「部署脚本执行失败」。\n"+
				"  要写成 `…)\" || xxx_rc=$?`（放进 AND-OR 列表才脱离 -e）。", want)
		}
	}
}

// ---------------------------------------------------------------------------
// 本轮（第六轮）的六条：都来自真机实测
// ---------------------------------------------------------------------------

// 用户原话："脚本这里的第一项选择 1 none，那么应该是没有 https，而是 http，
// 为啥还要安装 caddy 申请证书进行反代"。
//
// 矛盾确实存在，而且是文案的错：那一问回答的是"证书从哪来"，选项 1 的标签却
// 叫 `none`（字面意思是"不要证书"），说明里写着"自动装好 Caddy 申请证书"。
// 修法是把这一问改成"对外访问方式"，HTTPS 与纯 HTTP 各占一个选项。
//
// 这条护栏钉两件事：
//
//	① 选项 1 的标签不许再是 `none`（它与同一段说明里的"申请证书"直接冲突）；
//	② "纯 HTTP"必须是一个**独立可见**的选项，不能只是"不选 HTTPS 就没有"。
func TestTLSPromptDoesNotCallTheHTTPSOptionNone(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	// ① 找问答段里纯 HTTP 那一项。它就是"不要 https"的正式选项。
	if !strings.Contains(body, "不使用 HTTPS，只提供纯 HTTP") {
		t.Error("TLS 问答里没有「纯 HTTP」这一项的说明：\n" +
			"  用户看到选项 1 叫 none、说明却写着「自动装 Caddy 申请证书」，得出\n" +
			"  「那到底有没有 https」的疑问是完全合理的。HTTPS 与纯 HTTP 必须各占一项。")
	}

	// ② 该选项必须能被选到，而不是只有说明没有分支。
	if !strings.Contains(body, `5 | plain | PLAIN | Plain | none | NONE | None) TLS_MODE="plain"`) {
		t.Error("「纯 HTTP」这一项没有对应的解析分支：\n" +
			"  说明里列了第 5 项却解析不到，用户选 5 会被当成一个未知值塞给 tls.mode。")
	}

	// ③ 反向钉：选项 1（内部是 none）必须映射到 caddy 终结者——现在这一问
	// 直接决定终结者（用户要求："选择 1 直接是 caddy 反代，选择 2 直接是
	// Master 反代"），所以解析分支里必须同时带上 caddy 这个别名。
	if !strings.Contains(body, `1 | auto | AUTO | Auto | caddy | CADDY | Caddy) TLS_MODE="none"`) {
		t.Error("选项 1 没有映射到 tls.mode=none（并接受 caddy 作为别名）：\n" +
			"  这一问的语义是「谁做反代」——选 1 就是本脚本装的 Caddy 终结 TLS，\n" +
			"  而 Caddy 终结时 Master 侧本来就该是 none；选 2~4 才是 Master 自己终结。\n" +
			"  两条链路必须接上。")
	}
	if !strings.Contains(body, `2 | acme | ACME | Acme | master | MASTER | Master) TLS_MODE="acme"`) {
		t.Error("选项 2 没有映射到 tls.mode=acme（并接受 master 作为别名）：\n" +
			"  选 2 的语义是「Master 自己终结 TLS」，对应 acme 模式。")
	}
}

// 用户原话："我访问 https://域名/ 一切正常，但是访问 https://域名/admin/ 提示
// 404 page not found"。
//
// 根因：默认是公开站 0.0.0.0:8080、管理端 127.0.0.1:8081（两个不同监听），
// 而 Caddy 把所有请求都转给了 8080——8080 上的路由树里没有 /admin/，于是 404。
// "管理端只监听回环"这个安全默认没错，错在反代没有把后台那几条路径分流过去。
//
// 这条护栏钉：只要两个监听地址不是同一个，Caddyfile 里就必须有 /admin/ 与
// /api/admin/ 的分流规则，且它们指向**管理端**那个地址。
func TestCaddyfileSplitsAdminPathsToAdminListener(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	// 分流规则必须存在。
	for _, must := range []string{
		"@adminui path /admin /admin/*",
		"@adminapi path /api/admin/*",
	} {
		if !strings.Contains(body, must) {
			t.Errorf("Caddyfile 里缺分流规则 %q：\n"+
				"  公开站与管理端默认是两个监听地址，不按路径分流的话 /admin/ 永远\n"+
				"  落在公开站那棵路由树上——它的 404 就是用户看到的现象。", must)
		}
	}

	// 分流的**目标**必须是管理端，不是公开站。只写 handle 不写对的目标，
	// 表面上"配了分流"，实际仍在同一个地址上打转。
	if !strings.Contains(body, `_adm_target="$(proxy_target "$ADMIN_LISTEN")"`) {
		t.Error("分流规则没有取管理端地址（_adm_target）：\n" +
			"  handle @adminui { reverse_proxy <谁> } 里的 <谁> 必须是 ADMIN_LISTEN\n" +
			"  的代理目标，否则分流等于没分。")
	}

	// proxy_target 必须**接受参数**。不带参数时它读 PUBLIC_LISTEN——那正是
	// "管理端被转到公开站"这个 bug 的写法。取的是函数定义那一行，注释不算。
	if !strings.Contains(body, `_pt_addr="${1:-$PUBLIC_LISTEN}"`) {
		t.Error("proxy_target 不接受地址参数：\n" +
			"  它只认 PUBLIC_LISTEN 时，管理端分流的目标地址无从表达，\n" +
			"  /admin/ 只能被转到公开站（就是那个 404）。")
	}

	// 反向钉：两个地址**相同**时不该多写分流（那会让同源部署的 Caddyfile
	// 凭空多一层 handle，且把请求绕一圈回到同一个进程）。
	if !strings.Contains(body, `if [ "$_adm_target" != "$_pub_target" ]; then _need_split_admin="1"; fi`) {
		t.Error("没有按「两个监听是否相同」来决定要不要分流：\n" +
			"  无条件分流会在同源部署时把请求绕一圈；无条件不分流则会漏掉\n" +
			"  默认配置（8080/8081 两个地址）——两种都要防。")
	}
}

// 用户原话："卸载脚本时候，caddy 并没有停止监听和卸载"。
//
// 这是真缺陷：卸载路径原来只处理六件事（服务单元、二进制、配置、数据、
// 系统用户、init 残留），**完全没有 Caddy**——Caddy 继续占着 80/443，
// 用户以为卸载干净了。
//
// 但补这段时最容易犯的错是"把别人的 Caddy 也删了"：这台机器上很可能原本
// 就有一个（发行版包、x-ui 之类）。所以护栏要同时钉住"要清理"和"只清理
// 我们自己的"两个方向。
func TestUninstallStopsAndCleansCaddy(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	if !strings.Contains(body, "停止并清理 Caddy") {
		t.Fatal("卸载路径里没有 Caddy 清理步骤：\n" +
			"  这是用户实测报的缺陷——卸载后 Caddy 仍在监听 80/443。")
	}

	// ① 必须**停**服务。只删配置不停进程的话，Caddy 手里的监听不会释放。
	//
	// 判据要同时躲开两个陷阱（变异验证时都踩到过）：
	//   · dry-run 分支里有一句 `echo "    [dry-run] systemctl stop caddy（若在跑）"`
	//     也含这个子串，全文 Contains 会把它当实现；
	//   · **回滚段**里也有一条真实的 `systemctl stop caddy`（安装失败时停掉
	//     本次装的 Caddy），它是另一条路径的实现，不能替卸载那段作证。
	// 所以：只在"卸载段"这个区间里找行首的真实命令。
	// 区的边界用卸载独有的步骤标题——它们只在卸载路径里出现。
	uniStart := strings.Index(body, "==> 1/8 停止并注销服务")
	uniEnd := strings.Index(body, "==> 8/8 删除系统用户")
	if uniStart < 0 || uniEnd < 0 || uniEnd <= uniStart {
		t.Fatal("找不到卸载段的边界标记（判据与脚本脱节）")
	}
	uniBody := body[uniStart:uniEnd]

	stopReal, stopDry := 0, 0
	for _, line := range strings.Split(uniBody, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "systemctl stop caddy") {
			stopReal++
		}
		if strings.HasPrefix(trimmed, "echo ") && strings.Contains(trimmed, "systemctl stop caddy") {
			stopDry++
		}
	}
	if stopReal < 1 {
		t.Error("卸载段里没有真的停 caddy 服务：\n" +
			"  进程还活着，80/443 就还占着；删掉配置文件只影响下次启动。\n" +
			"  （回滚段里那条 stop 是安装失败的善后，不能替卸载作证。）")
	}
	if stopDry < 1 {
		t.Error("卸载的 dry-run 没有列出「将停止 caddy」：\n" +
			"  dry-run 的意义就是把会做的事列全，漏一项就少一次预演。")
	}

	// ② 只删**我们写的**单元。判据是 Description 里的 "XB-Probe"——
	// 官方包那份写的是 "Caddy web server"。用"文件存在"当判据会把别人的删掉。
	if !strings.Contains(body, `grep -q 'XB-Probe' "$CADDY_SVC"`) {
		t.Error("没有用 Description 标记分辨 caddy.service 是不是本脚本写的：\n" +
			"  只看「文件存在」会把用户自己的 Caddy 单元删掉——那比没卸载干净严重得多。")
	}

	// ③ Caddyfile 优先从 .bak 还原（我们追加过就会留 .bak），
	// 这样用户自己加的站点不会被牵连。
	if !strings.Contains(body, `mv -f "$CADDYFILE.bak" "$CADDYFILE"`) {
		t.Error("Caddyfile 没有从 .bak 还原的路径：\n" +
			"  本脚本往已有 Caddyfile **追加**过站点块，卸载就该用备份把它恢复原样；\n" +
			"  直接删文件会把用户原来的站点一起带走。")
	}

	// ④ 没有 .bak 时要能按标记行**精确摘除**我们那一段（而不是整份删）。
	if !strings.Contains(body, `awk -v mark="$CADDY_MARK"`) {
		t.Error("没有按 CADDY_MARK 摘除追加段落的逻辑：\n" +
			"  没有 .bak 说明我们没追加过、或者用户手工删了备份；这时只能靠标记行\n" +
			"  定位我们写过的那一段，整份删文件会牵连别人的站点。")
	}

	// ⑤ 卸载只在**卸载路径**里清理 Caddy，不能顺手影响安装路径的判定。
	// 这段用的是变量 CADDY_SVC（卸载局部），安装路径用的是 CADDY_UNIT——
	// 两者别混，混了会让安装时"以为已经装过单元文件"。
	if !strings.Contains(body, "CADDY_UNIT=/etc/systemd/system/caddy.service") {
		t.Error("安装路径的 CADDY_UNIT 不见了：\n" +
			"  卸载用 CADDY_SVC、安装用 CADDY_UNIT，两者刻意分开；\n" +
			"  删掉安装侧那个会让「单元已存在就跳过」的保护失效。")
	}
}

// 用户原话："按删除键时，本行的提示却消失了，例如
// `选择 TLS 方式（1/2/3/4） [1]:` 输入 1 再删除 1，就变成空行"。
//
// 根因是 readline 不知道"这一行前面已经有别的东西"：它把自己管理的范围当作
// 从光标处开始的整行，退格删到行首时重绘，把外面 printf 出来的提示一并擦掉。
// 修法是把提示用 `read -e -p` 交给 readline，它就知道那是提示而非输入。
//
// 这条护栏钉：read_line 必须支持提示参数，且 ask/confirm 都要走这条路径。
func TestPromptIsOwnedByReadlineSoBackspaceKeepsIt(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	if !strings.Contains(body, `IFS= read -e -r -p "$2" "$1"`) {
		t.Error("read_line 没有把提示交给 readline（-p）：\n" +
			"  提示用 printf 另打一句时，退格删到行首会把提示一起擦掉——\n" +
			"  这正是用户看到的「提示直接消失、变成空行」。")
	}

	// ask 必须走 read_line 的提示参数，而不是自己 printf。
	if !strings.Contains(body, `read_line ASK_REPLY "$_prompt"`) {
		t.Error("ask 没有把提示传给 read_line：\n" +
			"  一 printf 一 read 的写法就是上面那个缺陷的成因。")
	}

	// confirm 与卸载确认同理。
	if !strings.Contains(body, `read_line _ca "$1 [Y/n]: "`) {
		t.Error("confirm 的提示没有交给 readline。")
	}
	if !strings.Contains(body, `read_line _uc "这会删除数据目录`) {
		t.Error("卸载确认的提示没有交给 readline。")
	}
}

// 用户原话："有时候交互式输入后，会出现过几秒才提示输入下一个，但是提前回车了，
// 还有就是我并没有输入任何字符却出现无用的字符"。
//
// 他贴的证据里有一句 `https://mo.319082.xyzL，不带末尾斜杠）`——那个 L 不是他敲的，
// 而是启动管道 `curl -fsSL … | sh` 里 curl 选项的残留字符落在了脚本的 stdin 上。
// 下面几个 read 于是"凭空"读到一个字符，把一道题答歪，并且把后续问答错位。
//
// 这条护栏钉：问答开始前要清一次 stdin 残留，**且只在 stdin 不是 tty 时清**
// （真 tty 上用户可能已经开始打字，清空会吃掉他的输入）。
func TestStdinIsDrainedOnlyWhenNotATTY(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	if !strings.Contains(body, `[ ! -t 0 ]`) {
		t.Fatal("没有「只在非 tty 时才清 stdin」的判断：\n" +
			"  无条件清空会吃掉用户已经开始敲的输入——那比残留字符更糟。")
	}

	// 清的动作要存在，且用 -t 0 探测（非阻塞），不能用会阻塞的读。
	if !strings.Contains(body, `IFS= read -r -t 0 -n 1 _junk`) {
		t.Error("没有用 `read -t 0` 非阻塞地清 stdin 残留：\n" +
			"  残留字符来自 `curl … | sh` 的管道，不处理就会答歪题目。\n" +
			"  用阻塞读会直接把脚本挂在那儿等人回车。")
	}

	// 反向钉：这个清理不能在**卸载**路径之外的地方乱吃输入。
	// 它必须排在 ASK 判定之后（非交互时根本不需要清）。
	iDrain := strings.Index(body, `IFS= read -r -t 0 -n 1 _junk`)
	iAsk := strings.Index(body, "ASK=\"\"")
	if iDrain < 0 || iAsk < 0 {
		t.Fatal("找不到清 stdin 或 ASK 判定的位置（判据与脚本脱节）")
	}
	if iDrain < iAsk {
		t.Error("清 stdin 排在了 ASK 判定之前：\n" +
			"  非交互部署（CI、--yes）根本不需要清，也不该去碰 stdin。")
	}
}

// 用户第二次撞同一个错（`field sync_obtain not found`）时，我核对发现**远端
// 脚本已经是最新的**——他拿到的是 raw.githubusercontent.com 的 CDN 缓存。
// 于是"仓库里修好了"与"用户跑的是旧版"同时成立，他看到的只有同一个错误反复出现。
//
// 这条护栏钉两件事：① 取脚本时必须做缓存规避；② 部署脚本要能自报身份，
// 让"跑的是哪一版"从猜测变成一个可核对的事实。
func TestScriptFetchBypassesCDNCache(t *testing.T) {
	entry := readRepoFile(t, "../../install-master-bin.sh")

	// ① 请求头 + 查询串，两个都要（只加头，有些中间代理照样命中缓存）。
	//
	// **两条分支各查各的**：curl 那支与 wget 那支都要带缓存规避。用全文
	// Contains 查一个子串时，只改 curl 那支、wget 那支留着，护栏照样绿——
	// 而用 wget 的机器正是复制到旧脚本的那一半（变异验证时踩到过）。
	if !strings.Contains(entry, "curl -fsSL \\\n\t\t\t-H 'Cache-Control: no-cache'") {
		t.Error("curl 分支没有发 Cache-Control: no-cache：\n" +
			"  raw 前面有 CDN，`main` 这种可变引用会被缓存，用户会拿到旧脚本。")
	}
	if !strings.Contains(entry, "wget -q --no-cache") {
		t.Error("wget 分支没有 --no-cache：\n" +
			"  只用 wget 的机器（最小化容器很常见）会照旧拿到 CDN 缓存的旧脚本。")
	}
	// wget 那支有**两条调用**（带进度条优先、失败退回静默），两条都必须是
	// --no-cache。只查一次 Contains 时，改掉其中一条另一条仍绿——那正是
	// 变异验证里 H2 仍然绿的原因（护栏判据不够紧，不是实现有问题）。
	wgetCalls, wgetNoCache := 0, 0
	for _, line := range strings.Split(entry, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "wget ") {
			wgetCalls++
			if strings.Contains(trimmed, "--no-cache") {
				wgetNoCache++
			}
		}
	}
	if wgetCalls == 0 {
		t.Error("找不到 wget 调用（判据与脚本脱节）")
	} else if wgetNoCache != wgetCalls {
		t.Errorf("wget 有 %d 条调用，其中只有 %d 条带 --no-cache：\n"+
			"  两条分支是「先带进度条、失败退回静默」，退化的那条同样会被 CDN 缓存骗到。",
			wgetCalls, wgetNoCache)
	}
	if !strings.Contains(entry, "_bust=") || !strings.Contains(entry, "date +%s") {
		t.Error("取脚本时 URL 上没有随机会话串：\n" +
			"  CDN 的缓存键含查询串，带上它才必然回源。")
	}

	// ② 部署脚本要打印自己的指纹与关键修复标记。
	body := readRepoFile(t, "../../scripts/deploy-master.sh")
	if !strings.Contains(body, "script_self_check()") {
		t.Fatal("部署脚本没有 script_self_check：\n" +
			"  用户没有对照物时，「我跑的是哪一版」只能靠猜。")
	}
	if !strings.Contains(body, "本脚本缺少以下修复标记") {
		t.Error("script_self_check 没有报「缺少哪些修复标记」：\n" +
			"  只说「我是 sha256=…」而用户无从对照，等于没说；\n" +
			"  直接报「这份脚本里没有 binary_supports_config_key」才是可执行的诊断。")
	}
	// 标记清单必须真的探脚本内容，而不是写死的常量。
	if !strings.Contains(body, `for _mark in binary_supports_config_key Caddyfile_has_domain CADDY_MARK PLAIN_HTTP`) {
		t.Error("修复标记清单不完整：\n" +
			"  每一项都对应一个已修的真机缺陷，少一项就少一次「你拿到旧脚本」的提醒。")
	}

	// ③ Windows 入口同样要规避，而且**判据不能只落在 sh 那一侧**：ps1 入口也会
	//    从 raw 取部署脚本（问菜单 / 卸载 / 重置密码 / 更新发布文件，四条路都是），
	//    少了这一句，Windows 用户就拿到旧脚本——而 CI 跑在 Linux 上永远发现不了。
	//
	//    这一条是**实测**逼出来的：给部署脚本加 -ReleaseOnly 之后，按提交 SHA 取
	//    raw 已经是新内容，而 `main` 这个可变引用隔了很久仍发旧内容，于是入口的
	//    "先探再传"正确地拒绝了——但那是 CDN 的锅，不是代码的锅。
	psEntry := readRepoFile(t, "../../install-master.ps1")
	for _, want := range []string{
		`-Headers @{ "Cache-Control" = "no-cache"; "Pragma" = "no-cache" }`,
		`?nocache=" + [Guid]::NewGuid().ToString("N")`,
	} {
		if !strings.Contains(psEntry, want) {
			t.Errorf("install-master.ps1 没有做 raw 的 CDN 缓存规避（缺 %q）：\n"+
				"  症状与 sh 侧那次一模一样——仓库里明明修好了，Windows 用户取到的\n"+
				"  却是旧脚本，于是反复失败在同一个地方，而 CI 全绿。", want)
		}
	}
	// 两处下载（问菜单 / 早退通路）都必须走同一个带规避的函数。
	// 只判"有这个函数"是不行的：其中一处改回裸 Invoke-WebRequest 照样全绿。
	if strings.Count(psEntry, "Save-RawScript ") < 3 { // 1 处定义 + 2 处调用
		t.Error("install-master.ps1 里 Save-RawScript 的调用点少于 2 处：\n" +
			"  只要有一处仍用裸 Invoke-WebRequest，那条路就会拿到 CDN 缓存的旧脚本。")
	}
}

// ---------------------------------------------------------------------------
// 第七轮（后半）：三个都来自真机实测
// ---------------------------------------------------------------------------

// 用户原话："修改下部署脚本 选择 1 直接是 caddy 反代，选择 2 直接是 Master 反代"。
//
// 背景：这两件事本来是同一件事的两种说法（谁做反代 = 谁持有证书、谁监听 443），
// 却被拆成两问。真机上因此产出过至少两种自相矛盾的组合：
//
//	· 第一问标签是 `none`（字面"不要证书"）、第二问却默认 caddy 去申请证书；
//	· 选 2) acme + 2) master，清单里却是"公开站监听 0.0.0.0:8080"。
//
// 这条钉三件事：
//
//	① 终结者由第一问**推导**，不再有第二问；
//	② 推导链路：none→caddy，plain→空，其余→master；
//	③ 公开站监听的默认值随终结者走（caddy→回环 8080，master→443）。
func TestAccessModeQuestionDecidesTerminator(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	// ① 第二问必须消失。留着它，用户就会按出上面那种组合。
	if strings.Contains(body, `ask "选择终结者（1=caddy / 2=master）"`) {
		t.Error("仍在单独提问「TLS 由谁终结」：\n" +
			"  这一问与「对外访问方式」是同一件事的两种说法，拆开只会让用户\n" +
			"  按出一个自相矛盾的组合（真机上出现过）。用户要求：选 1 就是\n" +
			"  Caddy 反代、选 2 就是 Master 反代，一问到底。")
	}

	// ② 推导链路必须在（这是"选 1 = caddy、选 2 = master"的落点）。
	// 判据取 case 的三条分支臂——它们是这段逻辑唯一的表现形式。
	for _, arm := range []string{
		`none) TLS_TERMINATOR="caddy" ;;`,
		`plain) TLS_TERMINATOR="" ;;`,
		`*) TLS_TERMINATOR="master" ;;`,
	} {
		if !strings.Contains(body, arm) {
			t.Errorf("终结者推导缺少分支 %q：\n"+
				"  终结者必须由对外访问方式推导出来（选 1=caddy、选 2/3/4=master、选 5 为空），\n"+
				"  否则就又回到了「两个问题各自给默认值」那种会打架的状态。", arm)
		}
	}

	// ③ 公开站监听的默认值必须随终结者变。
	// 之前两种终结者都是 0.0.0.0:8080，于是选了 master 的人拿到一份
	// "把自己监听在 8080、对外却宣告 https"的配置——https://域名（443）永远打不开。
	if !strings.Contains(body, `master) _pl_default="0.0.0.0:443"`) {
		t.Error("Master 自己终结 TLS 时，公开站监听的默认值不是 0.0.0.0:443：\n" +
			"  Master 要自己监听 443 终结 TLS，默认给 8080 的话 https://域名 打不开。")
	}
	if !strings.Contains(body, `caddy) _pl_default="127.0.0.1:8080"`) {
		t.Error("Caddy 终结 TLS 时，公开站监听的默认值不是 127.0.0.1:8080：\n" +
			"  Caddy 反代过来，Master 只需监听回环；写 0.0.0.0 会把 8080 暴露到公网。")
	}

	// ④ install-master.sh 是同一套问答的镜像：那边也必须没有第二问。
	inst := readRepoFile(t, "../../install-master.sh")
	if strings.Contains(inst, `ask "选择终结者（1=caddy / 2=master）"`) {
		t.Error("install-master.sh 里仍在单独提问「TLS 由谁终结」：\n" +
			"  两个入口的问答必须一致，否则源码安装与免编译安装会给出不同的问题序列。")
	}
	if !strings.Contains(inst, `none) TLS_TERMINATOR="caddy" ;;`) {
		t.Error("install-master.sh 没有把终结者改成推导式。")
	}
}

// 用户原话："xb-probe-master 启动，由于公开和后台是同一个域名，caddy 好像只反代了
// 8080 端口，并没有反代 8081 导致访问管理端 404 page not found"，
// 并附上了机器上的 Caddyfile —— 那是**上一版脚本**写的，里面没有分流规则。
//
// 根因不是"没修"，而是修了**没生效**：升级时判据说"已有该域名的站点块 → 保留"，
// 于是旧块（无 /admin 分流）被原样留下，Caddy 永远不会用到新生成的规则。
//
// 这条钉：判据必须能区分"这个块是谁写的"——含我们的指纹就意味着**可以替换**。
func TestCaddyfileFromOlderVersionGetsReplaced(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	// ① 要有"这是不是我们写的"这个判据。
	if !strings.Contains(body, `_caddy_ours="1"`) {
		t.Error("没有判断「文件里有没有本脚本写的内容」：\n" +
			"  只看「有没有目标域名的站点块」会把**上一版脚本自己写的**旧块也当成\n" +
			"  「别人的配置」保留下来——真机上就是这样：脚本说部署完成，\n" +
			"  /admin/ 还是 404，而 Caddyfile 里明明有一个看起来正常的站点块。")
	}
	// ② 判据要认两种形态：追加段的 CADDY_MARK 与整份生成的头部 CADDY_HEAD。
	//
	// ⚠ 这里必须钉**安装段**那一处，不能全文搜 `$CADDY_HEAD`：卸载段里有一份
	// 同名的判据（caddyfile_restore），它会替安装段作证——把安装段的 CADDY_HEAD
	// 识别删掉，全文搜索照样命中，护栏仍然全绿，而真机上"升级时旧站点块被保留"
	// 的老问题原封不动地回来了（脚本说部署完成、/admin/ 还是 404）。
	// 所以判据取**带安装段缩进**的那两行：安装段是 2 个 tab，卸载段是 3 个。
	const installOwnsCheck = "\t\tif grep -qF \"$CADDY_MARK\" \"$CADDYFILE\" 2>/dev/null || " +
		"grep -qF \"$CADDY_HEAD\" \"$CADDYFILE\" 2>/dev/null; then\n\t\t\t_caddy_ours=\"1\""
	if !strings.Contains(body, installOwnsCheck) {
		t.Error("安装段没有用 CADDY_HEAD（+ CADDY_MARK）认出「这个块是不是我们写的」：\n" +
			"  已部署机器上的 Caddyfile 正是「整份由上一版脚本生成」的形态\n" +
			"  （头部一行 `# 由 …生成` + 站点块，没有 CADDY_MARK）。\n" +
			"  认不出来就只能整份保留，本轮的分流修复在升级时永远不会生效。\n" +
			"  （注意：卸载段里有一份同名判据，别让它替你作证。）")
	}
	// ③ 认出来之后必须**真的替换**（重写），而不是只打印一句提示。
	if !strings.Contains(body, "已按当前版本重写 $CADDYFILE") {
		t.Error("认出旧块之后没有重写：\n" +
			"  这条路径必须真的把旧站点块换成本版本生成的内容。")
	}
	// ④ 追加形态要用 CADDY_END 精确界定，否则摘除会留残（或删掉后面的用户站点）。
	if !strings.Contains(body, `CADDY_END="# ---- 以上由 scripts/deploy-master.sh 生成 ----"`) {
		t.Error("没有 CADDY_END 结束标记：\n" +
			"  我们追加的那一段没有结束位置，「摘掉我们写的、保留别人的」就无从谈起。")
	}
	// ⑤ 反向：**同一个** awk 程序里 END 与「行首 }」不能同时当结束判据——会打架。
	// 站点块收尾的 `}` 会先结束跳过，紧跟其后的 END 行就残留下来（实测踩过）。
	//
	// ⚠ 判据不能写成"某个固定缩进的两行子串"：早先的写法把续行缩进写死成 4 个
	// tab，而安装段那一处在 5 个 tab 上——真把混用改回去也不会变红（假绿）。
	// 所以逐个 awk 程序看：只要**同一个**程序里同时出现 `$0 == end` 与 `/^\}/`，
	// 就是混用（卸载段那条只认 `}` 的规则是合法的，不会被误伤）。
	for idx, off := 0, 0; ; idx++ {
		j := strings.Index(body[off:], "awk -v mark=")
		if j < 0 {
			break
		}
		j += off
		k := strings.Index(body[j:], `' "$CADDYFILE"`)
		if k < 0 {
			t.Fatalf("第 %d 个摘除用 awk 程序的结束位置找不到：结构变了，判据要跟着改。", idx+1)
		}
		prog := body[j : j+k]
		if strings.Contains(prog, "$0 == end") && strings.Contains(prog, `/^\}/`) {
			t.Error("同一个 awk 程序里同时用 CADDY_END 与行首 `}` 当结束判据：\n" +
				"  两者会打架——站点块收尾的 `}` 先结束跳过，紧跟的 END 行残留（实测踩过）。\n" +
				"  按「文件里有没有 END」分流成两条互斥的规则。")
		}
		off = j + 1
	}
}

// 选了 Master 自己终结 TLS，而本机还有本脚本装的 Caddy 在跑：两个进程都要 443，
// 后启动的必然 `bind: address already in use`。真机上出现过，而且被报成
// "启动后立即退出"，等回滚之后用户才知道真正的原因。
//
// 这条钉：动手之前就要发现并给出选择。
func TestMasterTerminatorChecksForCaddyPortConflict(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	const marker = "Master 自己终结 TLS 时的端口冲突预检"
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatal("没有「Master 自己终结 TLS」的端口冲突预检：\n" +
			"  先按选项 1 装了 Caddy、再按选项 2 让 Master 自己终结，Caddy 仍占着 80/443，\n" +
			"  Master 启动必然失败，而失败信息离真正的原因很远。")
	}
	// ⚠ 判据必须落在**这一段区间里**：`systemctl is-active caddy` 与
	// `/etc/systemd/system/caddy.service` 在文件里各有两份（卸载段的
	// caddyfile_restore 还有一份）。全文搜索会被那一份满足——变异只删预检里的
	// 调用、护栏照样全绿，等于什么都没验。所以先切出预检段（到下一个
	// `if [ -n "$ASK" ]; then` 之前），再在段内找。
	end := strings.Index(body[start:], "\nif [ -n \"$ASK\" ]; then")
	if end < 0 {
		t.Fatal("找不到端口冲突预检段的结束位置：结构变了，判据要跟着改" +
			"（别把 end 放宽成 -1，那会让区间变成整份文件，又回到「替身作证」）。")
	}
	region := body[start : start+end]

	if !strings.Contains(region, "systemctl is-active caddy") {
		t.Error("预检没有真的去看 Caddy 在不在跑。")
	}
	// 只许动"本脚本自己装的 Caddy"。停掉别人装的服务比"装不上"严重得多。
	if !strings.Contains(region, "grep -q 'XB-Probe' /etc/systemd/system/caddy.service") {
		t.Error("预检没有区分「Caddy 是不是本脚本装的」：\n" +
			"  无条件停掉 caddy 会把别人（发行版包、x-ui）的服务也停掉。")
	}
}

// 用户真机报的：Master 部署"成功"后，第一条命令
//
//	curl -fsSL https://<域名>/install.sh | sudo sh -s -- --code <注册码>
//
// 就死在
//
//	==> 下载 xb-probe-agent_linux_amd64
//	curl: (22) The requested URL returned error: 404
//	错误：下载失败。
//
// 根因不在安装脚本，而在**部署脚本没把发布文件放进 data_dir/dist/**：
// `/install.sh` 是内嵌在二进制里的（所以那一步取得到、看起来一切正常），
// 它接下来要的 `$DIST_BASE/xb-probe-agent_<os>_<arch>` 与 `/dist/SHA256SUMS`
// 才是发布文件；而部署流程只在收尾清单里把 `xb-probe-master dist pull` 当成
// **事后手工步骤**提了一句。于是一键部署出来的 Master 开箱就没有装机通道。
//
// 这条钉三件事：①部署流程必须自己拉发布文件；②拉失败不许回滚整个部署；
// ③判据落在磁盘上（/dist/ 受 install.mode 门控，刚部署完必然是关的）。
// shRegion 切出脚本里 startMark → endMark 之间的那一段。
//
// **必须切区间**：本文件里别处也有 `rollback`、也有 `dist` 字样，全文搜索会让
// 别的路径替这一段作证（"判据只认那一处实现"）。
func shRegion(t *testing.T, rel, body, startMark, endMark string) string {
	t.Helper()
	i := strings.Index(body, startMark)
	if i < 0 {
		t.Fatalf("%s 里找不到起止标记 %q —— 判据已与脚本写法脱节", rel, startMark)
	}
	rest := body[i:]
	j := strings.Index(rest, endMark)
	if j < 0 {
		t.Fatalf("%s 里找不到结束标记 %q —— 判据已与脚本写法脱节", rel, endMark)
	}
	return rest[:j]
}

// shFuncBody 切出一个 shell 函数的函数体（`名字() {` → 下一个行首的 `}`）。
//
// 为什么不直接对全文 `Contains`：**"文件里有这个函数"不等于"它被调用了"**，
// 反过来"全文出现过 dist pull"也可能来自注释、或来自**另一条路径**的同名实现。
// 判据要么钉函数体内部，要么钉调用点，不能钉"文件里有没有这个词"。
func shFuncBody(t *testing.T, rel, body, sig string) string {
	t.Helper()
	return shRegion(t, rel, body, sig+"() {", "\n}\n")
}

// shCalls 判断一段 shell 里**调用了**某个函数，而不是"某个注释里提到过它"。
//
// 为什么不能用 strings.Contains：脚本的注释里经常就写着函数名（"…都在
// ask_dist_pull_scope 里"），而那行注释离调用点只隔几行。单凭 Contains 判定，
// **把调用整行删掉**这种最该被抓到的回归会照样全绿 —— 又一次"文件里有这个
// 函数"不等于"它被调用了"，只不过这次的替身是注释而不是另一条路径。
//
// 判据落在"行首（允许缩进）就是函数名、后面跟空白或行尾"这个形态上。
func shCalls(region, name string) bool {
	return regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(name) + `(\s|$)`).MatchString(region)
}

// logicalLines 把续行拼成逻辑行。
//
// 两类要在意：
//
//	· 显式续行 —— 行尾的 `\`；
//	· **天然的续行** —— 行尾的管道 `|`（`||` 也以 `|` 结尾，一并覆盖）与 `&&`。
//	  只认 `\` 的话，`… 2>/dev/null |` 这行之后的 `sed … || true)"` 会被当成
//	  另一条独立的逻辑行，于是一处**没带** `|| true` 的调用被它"洗白"。
//
// 为什么要手工拼：Go 的 regexp 是 RE2，`.*(?:\\\n.*)*` 这种写法会**静默地只吃第一行**，
// 于是"跨行的这一条命令"看起来永远只有前半截。判据要按"一条命令"来数，就得先拼。
func logicalLines(s string) []string {
	var out []string
	cur := ""
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimRight(ln, " \t")
		if strings.HasSuffix(t, "\\") || strings.HasSuffix(t, "|") || strings.HasSuffix(t, "&&") {
			cur += t + " "
			continue
		}
		out = append(out, cur+ln)
		cur = ""
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func TestDeployPullsReleaseFilesForNodeBootstrap(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	// 取件只有一份实现：pull_agent_release 的函数体。6c（部署收尾）与 0c
	// （只更新发布文件）都调它——两份实现的下场是"改一处＝没改"。
	pull := shFuncBody(t, "scripts/deploy-master.sh", body, "pull_agent_release")
	src := shFuncBody(t, "scripts/deploy-master.sh", body, "default_dist_source")

	// ① 真的调用 dist pull，而且是**两种范围**都覆盖（--all / --platform）。
	//
	// 版本号必须来自**函数自己的第一个参数**。判据取"赋值自 $1 的那个标识符"
	// 而不是写成 `--version "$_pa_ver"` 这样的字面量：变量名只是拼写，一致地
	// 改名不该变红；而"函数里改用别的变量当版本号"（等于忽略传入的版本）必须变红。
	verArg := regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)="\$1"`).FindStringSubmatch(pull)
	if verArg == nil {
		t.Fatal("取件函数没有把版本号（第一个参数）收进来：判据与实现已经脱节。")
	}
	ver := verArg[1]
	if !strings.Contains(pull, `dist pull --config "$CFG" --version "$`+ver+`"`) {
		t.Errorf("取件函数没有用**它自己的版本参数**去调用 xb-probe-master dist pull（应为 --version \"$%s\"）：\n"+
			"  只打印一句「请自己跑 dist pull」等于没修——用户按收尾清单装第一个 Agent 时照样 404；\n"+
			"  改用别的变量当版本号，等于忽略了调用方传进来的版本。", ver)
	}
	if !strings.Contains(pull, "--platform \"$_mp_plat\"") {
		t.Error("没有「只拉本机平台」这一支：\n" +
			"  全部平台是十几份二进制，机房带宽有限时需要能只拉本机架构。")
	}
	if !strings.Contains(pull, "--all") {
		t.Error("没有「拉全部平台」这一支。")
	}
	// 取件实现只能有一处：pull_agent_release 里恰好两个分支（全部 / 只本机）。
	if n := strings.Count(pull, "dist pull --config"); n != 2 {
		t.Errorf("取件函数里 `dist pull --config` 出现了 %d 次（应为 2：--all 与 --platform 各一次）", n)
	}
	// 函数体之外还出现这个命令时，它只能是**打印给用户看的**（比如失败提示里
	// 那条可复制的补救命令），不能是被执行的第二份实现。
	//
	// 这里刻意不用"全文计数 == 2"：全文 Contains 会把注释与提示文案当成实现
	// （这一条正是被它误伤过——形态闸的提示里就有一条真命令示例）。
	outside := strings.Replace(body, pull, "", 1)
	for i, line := range strings.Split(outside, "\n") {
		if !strings.Contains(line, "dist pull --config") {
			continue
		}
		if !strings.Contains(line, "printf") && !strings.Contains(line, "echo") {
			t.Errorf("`dist pull --config` 在取件函数之外**被执行**了（第 %d 行）：\n"+
				"  %s\n"+
				"  多出来的那一处就是第二份取件实现——两条路迟早漂移，而症状在用户那端。", i+1, line)
		}
	}

	// ② 默认发布源必须跟着**版本**走（0.1.1 → v0.1.1 的 Release 标签），
	//    而且这份拼法全文件只有一处：两处就会漂移（改了一处的 v 前缀、
	//    另一条路没改 → 404）。
	if !strings.Contains(src, `https://github.com/$RELEASE_REPO/releases/download/v$1`) {
		t.Error("默认发布源不是从版本号推导的：\n" +
			"  写死一个版本号会在升级后静默拉到旧产物；不写 v 前缀则 tag 对不上直接 404。")
	}
	if n := strings.Count(body, "releases/download/v$"); n != 1 {
		t.Errorf("发布源 URL 的拼法在脚本里出现了 %d 次（应恰好 1 次，且只在 default_dist_source 里）：\n"+
			"  每个调用点各拼一次 = 迟早有一条路拼错，而它的症状是那个机房的 404。", n)
	}

	// ⑤ 判据落在磁盘上（/dist/ 受 install.mode 门控，刚部署完必然是关的，
	//    拿 HTTP 404 当失败会把每次正常部署都报成失败）。
	if !strings.Contains(pull, `-s "$DATA/dist/$`+ver+`/SHA256SUMS"`) {
		t.Error("没有检查发布文件是否真的落盘：\n" +
			"  /dist/ 在 install.mode=on_demand 且没有未使用注册码时**本来就该**返回 404，\n" +
			"  拿它当成功判据会误报；判据要落在磁盘上的文件。")
	}
	// 空版本号绝不能带进 dist pull：那会拼出 `--version ""`，下游按目录名
	// 创建 data_dir/dist//，症状是"拉了个空版本"却一路 rc=0。
	if !strings.Contains(pull, `if [ -z "$`+ver+`" ]; then`) {
		t.Error("空版本号没有被拦下：\n" +
			"  `--version \"\"` 下游会当成合法参数，把文件写进 data_dir/dist/ 本身。")
	}

	// ---- 6c：部署流程里的策略层（切区间，见上面的说明）----
	region := shRegion(t, "scripts/deploy-master.sh", body,
		"6c/7 发布文件入库", `echo "==> 7/7 完成"`)

	// ③ 版本号不是发布版本时（本地自编译的 dev）必须跳过，而不是硬去 GitHub 撞 404。
	if !strings.Contains(region, `*[!0-9.]*`) {
		t.Error("没有「版本号不是纯数字点分就跳过」的判断：\n" +
			"  本地自编译的二进制版本号是 dev，照默认 URL 去拉必然 404。")
	}
	// ④ 拉失败**不许**回滚整个部署。Master 本身是好的，坏的只是新节点自举；
	//    为一个可能只是网络不可达的下载把部署回滚，是把小问题升级成大问题。
	if strings.Contains(region, "rollback ") {
		t.Error("发布文件拉取这一段里出现了 rollback：\n" +
			"  GitHub 在部分机房不可达是常态，为一个下载失败回滚整个部署会把\n" +
			"  「装好了但装机通道暂时不通」变成「什么都没装成」。这里只该警告 + 给补救命令。")
	}
	// ⑥ 那个"未开放"的 404 要能识别并说清楚（而不是当成自检失败）。
	if !strings.Contains(region, "分发入口当前未开放") {
		t.Error("没有区分「/dist/ 未开放」与「真的坏了」：\n" +
			"  前者是 on_demand 的正常行为（生成注册码后立刻开放），后者才是故障。")
	}
	// ⑦ 这一段必须**调用**共用零件，而不是自己再写一遍取件。
	if !regexp.MustCompile(`pull_agent_release "\$[A-Za-z_][A-Za-z0-9_]*" "\$DIST_PULL"`).MatchString(region) {
		t.Error("6c 没有调用共用的 pull_agent_release：\n" +
			"  自己再写一遍取件，就等着和「只更新发布文件」那条路各修各的。")
	}
	if !shCalls(region, "ask_dist_pull_scope") {
		t.Error("6c 的范围问答没有走共用的 ask_dist_pull_scope：\n" +
			"  两份措辞/两份默认值 = 改一处漏一处。")
	}

	// ⑦ 安装入口（源码安装）必须把这三个参数转发给部署脚本。
	//    不转发 = 用户在安装脚本上给的 --dist-pull skip 被静默丢弃。
	inst := readRepoFile(t, "../../install-master.sh")
	for _, fwd := range []string{
		`set -- "$@" --dist-pull "$DIST_PULL"`,
		`set -- "$@" --dist-source "$DIST_SOURCE"`,
		`set -- "$@" --no-dist-pull`,
	} {
		if !strings.Contains(inst, fwd) {
			t.Errorf("install-master.sh 没有转发 %q：\n"+
				"  参数集护栏（deploy_flags_test.go）要求部署脚本认识的每个参数都被转发；\n"+
				"  少一个就等于用户在安装脚本里做的选择被静默丢弃。", fwd)
		}
	}

	// ⑧ 收尾清单不能还留着"自己去跑 dist pull"这种把必需步骤推给用户的说法。
	if strings.Contains(body, "--pubkey /path/minisign.pub") {
		t.Error("收尾清单里还写着让用户手工执行 dist pull 那一版命令：\n" +
			"  这一步已经在部署流程里做掉了（见 6c），清单要改成「补拉」的说明。")
	}
}

// TestReleaseOnlyModeUpdatesAgentReleaseWithoutTouchingMaster 守"只更新 Agent 发布文件"
// 这个模式（问答题第 3 项 / --release-only）。
//
// 用户的原话是："每次更新 Agent 版本就要重新执行一条带具体版本号的 dist pull 命令，
// 那直接写进一键部署脚本里，在脚本开头添加第三个选项，拉取最新版本的 Agent。"
//
// 于是这个模式必须满足三件事，缺一件这个功能就等于没做：
//
//	① 版本号由脚本自己查（GitHub 最新 Release），不需要人再去看 Release 页；
//	② 查不到就是**查不到**——不许悄悄退回"本机二进制版本"，那会让用户以为
//	   更新到了最新、实际拉的是他早就在跑的旧版本，而且全程没有提示；
//	③ 它只动发布目录，不碰 Master 的二进制/配置/服务（否则"更新 Agent"
//	   变成了"顺手把 Master 也换了一遍"）。
func TestReleaseOnlyModeUpdatesAgentReleaseWithoutTouchingMaster(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	// ---- ① 开头那一问里真的有第三项 ----
	if !strings.Contains(body, "3) 只更新 Agent 发布文件") {
		t.Error("开头那一问里没有第三项「只更新 Agent 发布文件」：\n" +
			"  这一项就是用户要的入口——没有它，更新 Agent 版本仍然要靠人肉敲版本号。")
	}
	// 菜单加了项、提示语没跟着改，是这类改动最典型的半成品：用户盯着
	// 「请输入 1 或 2」根本不会去试 3。
	//
	// 判据不写死"1、2、3"这种字面量：菜单项数是会长的（第 4、5 项就是后来加的），
	// 写死一次就得跟着改一次，改漏了谁也不知道。这里改成从 ask_mode_menu 自己
	// **数出来**有几项，再要求提示语把每一项都念到——菜单加到 6 项而提示语还停在
	// 5 项，会立刻变红。
	menu := shFuncBody(t, "scripts/deploy-master.sh", body, "ask_mode_menu")
	reItem := regexp.MustCompile(`(?m)^[ \t]+([0-9])\) `)
	nItems := 0
	for _, m := range reItem.FindAllStringSubmatch(menu, -1) {
		if n, _ := strconv.Atoi(m[1]); n > nItems {
			nItems = n
		}
	}
	if nItems < 2 {
		t.Fatalf("从菜单里数出 %d 项——判据已与脚本写法脱节", nItems)
	}
	// 每一项（第 1 项除外——它就是兜底分支本身）都必须有一条 `\tN | 别名… )`
	// 的取值臂。少了它，用户看见第 N 项、敲 N 进去，却被最后一个 `*) echo "1"`
	// 接走——**静默去装 Master**。这比"提示语没改"恶劣得多：提示语没改只是看不见，
	// 这条是会做错事。
	for i := 2; i <= nItems; i++ {
		arm := regexp.MustCompile(`(?m)^\t` + strconv.Itoa(i) + ` \| `)
		if !arm.MatchString(menu) {
			t.Errorf("菜单里画了第 %d 项，却没有 `%d |` 这条取值臂：\n"+
				"  用户看得见、答进去不被认，而兜底分支会把它当成「安装/升级」——\n"+
				"  选了 A 却做了 B，还是破坏性的那一个。", i, i)
		}
	}
	nums := make([]string, 0, nItems)
	for i := 1; i <= nItems; i++ {
		nums = append(nums, strconv.Itoa(i))
	}
	wantPrompt := "read_line _mode \"请输入 " +
		strings.Join(nums[:nItems-1], "、") + " 或 " + nums[nItems-1] + " [1]: \""
	if !strings.Contains(body, wantPrompt) {
		t.Errorf("菜单提示语没有跟着改成 %d 个选项（期望 %q）：\n"+
			"  用户看到「请输入 1 或 2」就不会去试后面的项，那些入口等于不存在。",
			nItems, wantPrompt)
	}
	// 第 3 项的"菜单 → 变量"映射现在落在 ask_mode_menu 里——菜单**只有那一份**
	// （入口脚本也靠它问，见 --ask-mode），0b 只负责把答案接过去。所以判据要分两半，
	// 而且两半都必须钉住：
	//   · 菜单那一半丢了 → 用户根本看不到第 3 项；
	//   · 0b 那一半丢了   → 用户选了 3，脚本照样去装 Master。
	// 只判其中一半，另一种丢法就是假绿。
	if !strings.Contains(body, `3 | release | RELEASE | Release | 更新 | 发布文件) echo "3" ;;`) {
		t.Error("菜单里第 3 项没有归一到 3：\n" +
			"  用户看得见这一项、答进去却不被认，等于这一项不存在。")
	}
	if !strings.Contains(body, `case "$(ask_mode_menu)" in`) ||
		!strings.Contains(body, `3) RELEASE_ONLY="1" ;;`) {
		t.Error("第 3 项没有映射到 RELEASE_ONLY（菜单画了但分支没接）。")
	}

	// ---- ② 模式分支：切区间，别处的 resolve/pull 不许替它作证 ----
	//
	// 结束标记跟着结构走：0c 后面现在紧跟着 0d（只升级本体），而 0d 里**有**
	// `install -m 0755`——它是本模式最该被排除的动作。早先这里用的是"# 卸载：
	// --uninstall"（更靠后的一个标题），于是 0d/0e 整段被算进 0c 的区间，
	// 下面第 ⑦ 条会报出一个**根本没在 0c 里发生**的"违规"。
	region := shRegion(t, "scripts/deploy-master.sh", body,
		"# 0c) 只更新 Agent 发布文件", "# 0d) 只升级 Master 本体")

	if !strings.Contains(region, `if [ -n "$RELEASE_ONLY" ]; then`) {
		t.Fatal("找不到 0c 的模式分支：判据与脚本结构脱节了。")
	}

	// 查询结果落到哪个变量上：下面几条断言全部绑在**这个标识符**上，
	// 而不是绑在某个具体拼写上——一致改名仍然绿，而"查到 A、拿 B 去取件"
	// 或者"查完不判成败就继续"必须变红。
	relArg := regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)="\$\(resolve_latest_agent_version\)" \|\| exit 1`).FindStringSubmatch(region)
	if relArg == nil {
		t.Error("版本查询失败没有直接中止：\n" +
			"  查不到最新版本时必须报错退出（`查版本 || exit 1`）；悄悄退回「本机二进制版本」\n" +
			"  会让用户以为更新到了最新，而实际拉的是他早就在跑的旧版本——全程没有任何提示。")
	}
	rel := "\u0000"
	if relArg != nil {
		rel = relArg[1]
	}

	// 先查版本、再取件，而且**取件的成败必须被判断**。次序反了就会拿一个空
	// 版本号去 dist pull；写成 `pull ... || true` 则把"更新失败"静默成"更新成功"。
	//
	// 第三个参数是**哪个二进制去取件**。这里同样不绑变量名（一致改名仍绿），
	// 但要求它是"一个变量"而不是写死的路径——写死就等于忽略 --bin / --bin-dir。
	// 这条守在用户那次实测故障的同一条路上：原先这个函数直接用 `$_mp_bin`，
	// 而那个变量只在 6c 赋值，0c 走到这里就成了 `unbound variable`。
	iResolve := strings.Index(region, "resolve_latest_agent_version")
	rePull := regexp.MustCompile(
		`if pull_agent_release "\$` + rel + `" "\$DIST_PULL" "\$[A-Za-z_][A-Za-z0-9_]*"; then`)
	loc := rePull.FindStringIndex(region)
	iPull := -1
	if loc != nil {
		iPull = loc[0]
	}
	if iPull < 0 {
		t.Error("模式分支没有把**查到的那个版本**和**要用的那个二进制**一起交给共用的 pull_agent_release，并判它的成败：\n" +
			"  自己再写一遍取件，两条路就会各修各的；版本交给另一个变量（或写死）等于查了却不用它；\n" +
			"  二进制写死路径则忽略 --bin/--bin-dir；漏掉第三个参数就是用户实测撞到的\n" +
			"  `_mp_bin: unbound variable`——那个变量只在 6c 赋值，而这条路上 6c 不执行；\n" +
			"  写成 `pull … || true` 则用户看到的最后一行仍然是「完成」。")
	}
	if iResolve >= 0 && iPull >= 0 && iResolve > iPull {
		t.Error("先取件后查版本：\n" +
			"  顺序反了就会拿一个还没算出来的版本号去拉——空值或不相关的值都会一路跑下去。")
	}
	// ④ 带进 dist pull 的那个值必须是"纯数字点分"。查询函数内部已经校验过一次，
	//    这里要的是**判据押在要用的那个值上**：不管查询函数将来怎么改，拼进下载
	//    路径的一定得是这个形态。
	if !strings.Contains(region, `case "$`+rel+`" in`) || !strings.Contains(region, `*[!0-9.]*`) {
		t.Error("模式分支没有对**要用的那个版本号**再做一次「纯数字点分」形态校验：\n" +
			"  查询失败时拿到的往往是别的文字（HTML 错误页、旧二进制的 usage 行），\n" +
			"  拼进 .../releases/download/v<那段文字> 之后，症状是「发布文件拉取失败」，根因被埋掉。")
	}
	// ⑤ 与部署流程共用同一套零件，而且**是调用**（注释里也写着这个名字，
	//    单凭 Contains 判定的话，把调用删掉仍然绿）。
	if !shCalls(region, "ask_dist_pull_scope") {
		t.Error("模式分支没有走共用的范围问答（措辞/默认值会出现第二份）。")
	}
	// ⑥ 默认值必须**显式写死**，不能沿用 .answers 里的记忆：这个模式存在的意义
	//    就是"拉最新"，上次选过"先跳过"的人不该因为一次历史选择在这里什么都不做，
	//    而他一路上看到的最后一行是"完成"。
	if !strings.Contains(region, `ask_dist_pull_scope "1"`) {
		t.Error("模式分支的范围问答没有把默认值写死成「全部平台」：\n" +
			"  沿用记忆时，上次选了「先跳过」的人重跑这个模式会什么都不做，\n" +
			"  而他看到的最后一行是「完成」——「更新 Agent 版本」就此静默失败。")
	}
	// ⑦ 只动发布目录：不许出现换二进制 / 写配置 / 重启服务这些动作。
	for _, forbidden := range []string{"render_config", "systemctl restart", "install -m 0755"} {
		if strings.Contains(region, forbidden) {
			t.Errorf("模式分支里出现了 %q：\n"+
				"  这个模式只该往 data_dir/dist/ 里写文件。「顺手」把二进制换了、\n"+
				"  配置重写了或服务重启了，用户要的「只是更新 Agent 发布文件」就变成了\n"+
				"  一次没人预期的改动。", forbidden)
		}
	}
	// ⑧ dry-run 不写系统、不需要 root（与全局约定一致）。而"缺二进制"这件事在
	//    dry-run 下**不能是致命的**：判据不是"某句话长什么样"，而是"DRY_RUN 的处理
	//    出现在第一个 exit 1 之前"——先判 DRY_RUN 再决定是警告还是退出，顺序反了
	//    就等于 dry-run 也要求环境完整。
	iNoBin := strings.Index(region, `[ ! -x "$_ro_bin" ]`)
	if iNoBin < 0 {
		t.Error("模式分支没有检查取件用的二进制是否可用：\n" +
			"  不可执行时应当给一句能照着做的提示（--bin-dir / --bin），而不是等取件时才炸。")
	} else {
		tail := region[iNoBin:]
		iDry := strings.Index(tail, "DRY_RUN")
		iExit := strings.Index(tail, "exit 1")
		if iDry < 0 || (iExit >= 0 && iDry > iExit) {
			t.Error("dry-run 下缺二进制直接失败了：\n" +
				"  dry-run 的价值正是「什么都还没装的时候先看看会发生什么」，\n" +
				"  它一个字节都不写，不该因为环境不完整而跑不起来。")
		}
	}
	if !strings.Contains(region, `[ "$(id -u)" -ne 0 ]`) {
		t.Error("模式分支没有 root 检查：要往 data_dir/dist/ 里写，非 root 会以「权限不足」收场。")
	}

	// ---- ⑨ 互斥：两个模式同时给必须被拒绝，而不是让其中一个悄悄赢 ----
	//
	// 判据**不**写"某两个标志两两与一下"：那种写法盖不住新加的模式（4 个单次模式
	// 有 C(4,2)=6 对，写全很啰嗦、漏一对也没人发现）。所以这里钉的是"数一数"
	// 这个机制本身，并逐个要求每个单次模式都被数进去——少计一个，那一对又会
	// 静默通过，而症状是"用户以为做了 A、实际做了 B，两条路都不报错"。
	for _, fl := range []string{"--uninstall", "--release-only", "--upgrade-only", "--reset-password"} {
		if !strings.Contains(body, "_oneshot_note "+fl) {
			t.Errorf("%s 没有被计入「只做一件事」的互斥判断：\n"+
				"  不数它，它与别的单次模式同时给出时就会静默通过——\n"+
				"  用户以为做了 A、实际做了 B，而两条路都不报错。", fl)
		}
	}
	if !strings.Contains(body, `if [ "$_oneshot_count" -gt 1 ]; then`) {
		t.Error("「只做一件事」的互斥没有按个数判断（退回两两列举就会漏掉新组合）。")
	}

	// ---- ⑩ 查询函数本身：两个来源 + 形态闸 + set -eu 下的失败处理 ----
	lookup := shFuncBody(t, "scripts/deploy-master.sh", body, "resolve_latest_agent_version")
	if !strings.Contains(lookup, "https://api.github.com/repos/$RELEASE_REPO/releases/latest") {
		t.Error("查询没有走 GitHub API：这是最直接、最精确的一个来源。")
	}
	if !strings.Contains(lookup, "releases/tag/") {
		t.Error("查询没有重定向兜底：\n" +
			"  GitHub API 的匿名限额是 60 次/小时，机房出口 IP 常常被整段共享，\n" +
			"  限流时必须有第二条不限额的路，否则这个功能会在某些机房里随机失灵。")
	}
	// set -eu 下的失败处理：**数"真的去网络的调用"与"带 `|| true` 的调用"并比对相等**。
	//
	// 这里原本写的是 `Count(lookup, "|| true)") >= 3` —— 那个阈值是没用的：
	// 函数里实测有 5 处（`command -v` 那两处也凑数），于是**删掉任意一处的
	// `|| true` 仍然绿**（不是推理，是变异 M14 当场抓出来的）。判据必须落在
	// "每一个网络调用都带护栏"，而不是"总个数比某个数多"。
	netCalls, guarded := 0, 0
	for _, ln := range logicalLines(lookup) {
		// 只数"作为命令替换去执行的"那些；`[ -n "$_rl_fetch" ]` 这类判断不算，
		// `_rl_fetch="$(command -v …)"` 也不含 `"$_rl_fetch"` 这个形态，自然被排除。
		if !strings.Contains(ln, "$(") {
			continue
		}
		if !strings.Contains(ln, `"$_rl_fetch"`) && !strings.Contains(ln, `"$_rl_wget"`) {
			continue
		}
		netCalls++
		if strings.Contains(ln, "|| true") {
			guarded++
		}
	}
	if netCalls < 3 || guarded != netCalls {
		t.Errorf("查询函数里「真的去网络的调用」有 %d 处，其中带 `|| true` 的只有 %d 处：\n"+
			"  脚本开头是 set -eu，「赋值 + 命令替换」里的命令返回非 0 会让整个脚本\n"+
			"  立刻退出——偏偏这里最常见的正是**正常的失败**（GitHub 不可达、API 限流 403）。\n"+
			"  被测的失败必须能被自己的代码处理；少一处就会让「查不到最新版本」\n"+
			"  变成「脚本直接崩了」，而调用方那句 `|| exit 1` 连执行的机会都没有。", netCalls, guarded)
	}
	if !strings.Contains(lookup, `*[!0-9.]*`) {
		t.Error("查询函数里没有「纯数字点分」的形态闸：\n" +
			"  返回给调用方的必须已经是一个能直接拼进 URL 的版本号。")
	}

	// ---- ⑪ 源码安装入口：解析 + 转发 + 不需要 Go/源码 ----
	inst := readRepoFile(t, "../../install-master.sh")
	if !strings.Contains(inst, `--release-only) RELEASE_ONLY="1"; shift ;;`) {
		t.Error("install-master.sh 没有解析 --release-only（参数集护栏也会红）。")
	}
	if !strings.Contains(inst, "set -- --release-only") {
		t.Error("install-master.sh 解析了 --release-only 却没有转给部署脚本：\n" +
			"  参数被读走之后什么也不做，是最难发现的一种失效。")
	}
	if !strings.Contains(inst, `if [ -n "$UNINSTALL" ] || [ -n "$RELEASE_ONLY" ] || [ -n "$RESET_PASSWORD" ]; then`) {
		t.Error("install-master.sh 的 --release-only / --reset-password 没有走「排在依赖检查与编译之前」那条早退路径：\n" +
			"  它们不需要源码、不需要 Go。走正常路径就等于「更新个发布文件先装一套工具链」，\n" +
			"  而且工具链装不上就更新不了。")
	}
	// --upgrade-only 是这条规矩的**例外**，而例外也要钉住，否则"忘了把它加进早退
	// 通路"和"故意让它去编译"这两种状态在仓库里长得一样：前者会让本模式彻底失效
	// （升级本体靠的就是现场编译出来的那个二进制）。
	if !strings.Contains(inst, `set -- --bin "$SRC/bin/xb-probe-master" --upgrade-only`) {
		t.Error("install-master.sh 没有把 --upgrade-only 连编译产物一起交给部署脚本：\n" +
			"  这个模式的二进制只能来自现场编译，不带 --bin 过去它必然以「需要指定新二进制」收场。")
	}
	// 只升级本体时**绝不能**把站点/证书那一串拼进参数：它们在那个模式下全是空的，
	// 而其中 `--tls-mode "$TLS_MODE"` 是无条件拼的——空值会被部署脚本判成非法取值。
	if !regexp.MustCompile(`(?m)^if \[ -n "\$UPGRADE_ONLY" \]; then\n\tset -- --bin .* --upgrade-only\n`).MatchString(inst) {
		t.Error("install-master.sh 没有按 --upgrade-only 分成两支拼参数：\n" +
			"  那些变量在那个模式下全是空的，无条件拼进去会把空值当成非法取值（如 --tls-mode \"\"），\n" +
			"  结果是「只升级本体」连部署脚本都进不去。")
	}
}

// ---------------------------------------------------------------------------
// ⑤ 第 3 步必须把配置收敛到本次回答；连接层不通不能静默放过
// ---------------------------------------------------------------------------
//
// v0.1.3 真机（2026-10-14，腾讯云）连着栽出三条：
//
//	① 问答里选了 selfsigned，第 3 步却"已存在，保留"——旧配置是 acme+sync_obtain，
//	   服务按旧模式反复退出（申请失败即退出），80/443 全都不在听，
//	   部署却照样报告成功；
//	② 健康检查对"20 秒连接层不通"（000/空码）一律静默放过，正好把 ① 掩盖了；
//	③ 证书步骤打印"您的二进制版本不支持前台申请"——但配置里明明写着
//	   sync_obtain: true（这句话只在 render_config 探测过才成立，配置被保留时
//	   根本没探测过），顺着它去查"版本错配"就把人带偏了。
//	另有一处纯 bug：收尾提示的 heredoc 里 `/install.sh | sh` 的反引号被 shell
//	当成命令替换，每次部署完都报一行 "line NNNN: /install.sh: No such file"。
func TestStep3ConvergesConfigToAnswers(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	if !strings.Contains(body, "_cfg_scalar() {") {
		t.Error("第 3 步没有 _cfg_scalar 提取函数：没有它就无法逐项比对「本次回答 vs 现有配置」。")
	}
	if !strings.Contains(body, "已存在且与本次回答一致，保留") {
		t.Error("第 3 步缺少「一致才保留」的分支：幂等重跑不应每次都重写配置。")
	}
	if !strings.Contains(body, "按本次回答重写（旧配置备份为 $CFG.bak）") {
		t.Error("第 3 步缺少「不一致就按本次回答重写」的分支：\n" +
			"  摘要打印的是新答案、实际跑的是旧配置，是「我明明选了自签」的直接来源。")
	}
	// 反向：旧的静默保留文案不许回潮。
	if strings.Contains(body, `echo "    已存在，保留（要覆盖请加 --force-config）"`) {
		t.Error("第 3 步又回到了「已存在，保留」：问答改了 TLS 模式/域名也不会生效。")
	}
	// 比对必须覆盖 TLS 模式——这正是真机栽的那一项。
	//
	// 判据落在**调用行**上：同一行里既有 `_cfg_scalar "$CFG"` 又有那个键名。
	// 不能只对整段 body 做 `strings.Contains(body, key)`——`mode`、`challenge`
	// 这种短词在脚本里遍地都是（注释、别的变量名），那样等于没判。
	if !strings.Contains(body, "_cfg_scalar() {") {
		t.Fatal("找不到 _cfg_scalar 的定义：后面的判据无意义。")
	}
	for _, key := range []string{"public_listen", "admin_listen", "public_origin", "admin_origin", "mode", "challenge"} {
		pat := regexp.MustCompile(`(?m)_cfg_scalar "\$CFG"[^\n]*` + key)
		if !pat.MatchString(body) {
			t.Errorf("第 3 步的比对没有覆盖 %s：\n"+
				"  判据是「同一行里既有 _cfg_scalar \"$CFG\" 又有该键名」。\n"+
				"  漏掉哪一项，那一项就被旧配置吞掉——用户改了域名/模式却按旧配置跑。", key)
		}
	}
}

func TestHealthCheckNeverToleratesDeadListener(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	// 连接层不通（空码/000）必须分模式处理，不能再与 2xx 混在一个 case 里静默放过。
	if regexp.MustCompile(`(?m)^case "\$_hc_code" in\n2\* \| "" \| 0\*\) : ;;`).MatchString(body) {
		t.Error("健康检查又把「连接层不通」与「就绪」合并静默放过：\n" +
			"  服务反复退出时部署照样报告成功，用户只能靠 netstat 才能发现 443 没在听。")
	}
	if !strings.Contains(body, `rollback "Master 未在 $HEALTH_ADDR 上提供服务（20 秒连接层不通）"`) {
		t.Error("非 acme 模式下连接层不通应当回滚（不存在「证书还没好」这回事）。")
	}
	if !strings.Contains(body, "可能只是首张证书还没就绪") {
		t.Error("acme 模式下连接层不通要先提示「可能只是证书没就绪」，把判定交给证书步骤。")
	}
	// 那句只在「render 探测过」时才成立的话不许回潮。
	if strings.Contains(body, "您的二进制版本不支持前台申请") {
		t.Error("证书步骤又在断言「二进制不支持前台申请」：配置被保留时根本没探测过，" +
			"v0.1.3 真机上配置里明明是 sync_obtain: true。按配置的实际内容说话。")
	}
	if !strings.Contains(body, "配置带 sync_obtain: true：申请不成功服务会退出") {
		t.Error("证书步骤缺少按 sync_obtain 实际取值区分的两段提示。")
	}
}

func TestFinishTipsNeverExecuteBackticks(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	// 收尾提示是未加引号的 heredoc（cat <<EOF），里面出现裸反引号就是命令替换：
	// "/install.sh | sh" 前后那一对反引号会真的去执行 /install.sh，
	// 每次部署完都报 "line NNNN: /install.sh: No such file or directory"。
	// Go 的双引号字符串不支持 \` 转义，这里用拼接写出反引号本身。
	bt := "\x60" // 反引号
	if strings.Contains(body, bt+"/install.sh | sh"+bt) {
		t.Error("收尾提示里有未转义的反引号命令替换：\n" +
			"  heredoc 是 cat <<EOF（未加引号），反引号会被执行。写成 反斜杠+反引号 转义。")
	}
	if !strings.Contains(body, "\\"+bt+"/install.sh | sh"+"\\"+bt) {
		t.Error("收尾提示里的 /install.sh 示例缺少转义（反斜杠+反引号）。")
	}
}

// ACME 的挑战端口是**特权端口**，systemd 单元必须显式授予绑定能力。
//
// 用户的原话是「我选择 80 端口，在申请期间，80 端口一直没起来，所以才申请失败」。
// 他把原因归给网络，但机器上 80 完全空闲、防火墙全开 —— 真正拦住它的是这一条：
// 单元里写的是 User=probe，而内核的 net.ipv4.ip_unprivileged_port_start 默认
// 1024，非 root 进程绑 80 直接 permission denied。证书因此永远申请不到，而每次
// 失败的申请都会消耗一次 CA 的"失败授权"额度，反复重试之后现象就变成了
// "rateLimited"——那是**次生现象**，却最容易把人引向"是限流害的"这个错误结论。
//
// 三个方向都要钉：
//   - 有：Master 自己终结 TLS（acme/selfsigned/files）时要绑 443，http-01 还要绑 80；
//   - 有条件：TLS 交给前置反代（mode=none）时不绑任何低位端口，多给能力没有理由；
//   - 有上限：申请失败会让进程退出，Restart=always 每 3 秒就再申请一次。
func TestMasterUnitGrantsLowPortBinding(t *testing.T) {
	body := readRepoFile(t, "../../scripts/deploy-master.sh")

	const ambient = "AmbientCapabilities=CAP_NET_BIND_SERVICE"
	if n := strings.Count(body, ambient); n != 1 {
		t.Errorf("AmbientCapabilities=CAP_NET_BIND_SERVICE 出现 %d 次（期望恰好 1 次，就在 _cap_block 里）：\n"+
			"  probe 用户绑不上 80/443（特权端口），http-01 校验必然失败 —— "+
			"现象正是「申请证书期间 80 一直没起来」。", n)
	}
	if !strings.Contains(body, "CapabilityBoundingSet=CAP_NET_BIND_SERVICE") {
		t.Error("缺 CapabilityBoundingSet=CAP_NET_BIND_SERVICE：只给 Ambient 而不收 Bounding，" +
			"等于把其它能力也留着，与「最小权限」相悖。")
	}
	// 条件化：能力只在「Master 自己终结 TLS」时授予。
	iGate := strings.Index(body, `if [ "$TLS_MODE" != "none" ]; then`)
	iCap := strings.Index(body, ambient)
	iUse := strings.Index(body, "$_cap_block")
	switch {
	case iGate < 0:
		t.Fatal("找不到 _cap_block 的赋值条件：能力必须只在「Master 自己终结 TLS」时授予")
	case iCap < iGate:
		t.Error("能力位不在 TLS_MODE != none 的条件块里：" +
			"无条件授予会连「TLS 交给前置反代」的部署也拿到绑定特权端口的能力")
	case iUse < 0:
		t.Error("_cap_block 没有被写进 systemd 单元（单元模板里必须有 $_cap_block）")
	}

	// 单次失败不能变成"每 3 秒重来一次"：sync_obtain 失败时进程会**故意**退出，
	// Restart=always 立刻把它拉起来再申请一次，每次都算一次"失败授权"。
	// Let's Encrypt 是每标识符每账号每小时 5 次，打满之后连正确的尝试都做不了。
	if !strings.Contains(body, "StartLimitBurst=") {
		t.Error("单元缺少 StartLimitBurst：申请失败会陷入「退出 → 3 秒后重启 → 再申请」的循环，" +
			"把 CA 的失败授权额度烧光（真机上就是这么从「申请失败」变成 rateLimited 的）")
	}

	// 部署前要能提前发现"挑战端口被别人占着"，而不是等证书申请失败了再猜。
	if !strings.Contains(body, "需要 Master 自己监听 $_pv_port 端口，但它已被占用") {
		t.Error("缺少 http-01/tls-alpn-01 的端口占用前置检查：" +
			"端口被占时 Master 起不来、证书申请失败，而报出来的现象离原因很远")
	}
}
