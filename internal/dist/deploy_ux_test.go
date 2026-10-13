package dist

import (
	"os"
	"path/filepath"
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
	p := filepath.Join(t.TempDir(), "probe-master")
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

	// 判据一：不许出现"文件存在就保留"这种短路，必须同时看域名。
	//
	// 只看**行首的 if**：修正后的写法里有好几个分支也带这个子串
	// （`elif [ -f "$CADDYFILE" ] && [ -z "$FORCE_CONFIG" ]; then` 是"追加"分支，
	// 它本来就该在"没有目标域名"时走到）。用全行 Contains 判会把正确实现也判红。
	sawGuardedIf := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "if [ -f \"$CADDYFILE\" ] && [ -z \"$FORCE_CONFIG\" ]") {
			if !strings.Contains(trimmed, "Caddyfile_has_domain") {
				t.Error("Caddyfile 的保留判据只看「文件存在」——\n" +
					"  本机 Caddy 常是别人装的（发行版包、x-ui、更早的 nginx 迁移），\n" +
					"  那份配置里没有我们要反代的域名，保留它等于 Caddy 不开 443、拿不到证书。\n" +
					"  判据要换成「里面有没有本次要用的域名」（见 Caddyfile_has_domain）。\n" +
					"  实际写法：" + trimmed)
			} else {
				sawGuardedIf = true
			}
		}
	}
	if !sawGuardedIf {
		t.Fatal("没找到「存在 + 未 force-config」这个判据的 if 分支：\n" +
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

	// 探测本身要有"基线必须能解析"的前置检查。没有它，一份**语法错误**的
	// 探针配置会让二进制报 YAML 错误，而那被当成"认识这个键"（假阴性）。
	if !strings.Contains(body, `*"解析配置失败"*`) {
		t.Error("binary_supports_config_key 没有基线校验：\n" +
			"  探针配置自己写坏时二进制报的是 YAML 语法错，会被误判成「认识该键」——\n" +
			"  那就等于探测失效、照样写下去让服务起不来。")
	}
	if !strings.Contains(body, `public_listen: "127.0.0.1:0"`) {
		t.Error("探针配置里的监听地址没有加引号：\n" +
			"  `127.0.0.1:0` 不加引号在 YAML 里会被当成映射键（`did not find expected key`），\n" +
			"  探测结果就全部不可信了。")
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

	// ③ 反向钉：选项 1（auto，内部仍是 none）必须映射到 caddy 终结者。
	// 只改文案、没改默认值链路的话，"选 1 = 自动办证书"这句说明就是假的。
	if !strings.Contains(body, `1 | auto | AUTO | Auto) TLS_MODE="none"`) {
		t.Error("选项 1（auto）没有映射到 tls.mode=none：\n" +
			"  auto 的语义是「HTTPS 由脚本装的 Caddy 终结」，而 Caddy 终结时\n" +
			"  Master 侧本来就该是 none——两条链路必须接上。")
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

	// ② 只删**我们写的**单元。判据是 Description 里的 "Server Probe"——
	// 官方包那份写的是 "Caddy web server"。用"文件存在"当判据会把别人的删掉。
	if !strings.Contains(body, `grep -q 'Server Probe' "$CADDY_SVC"`) {
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
}
