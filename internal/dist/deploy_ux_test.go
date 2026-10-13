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
