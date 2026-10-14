package dist

import (
	"os/exec"
	"strings"
	"testing"
)

// 后台入口地址：**按用户实际怎么访问**推导，而不是从监听地址反推。
//
// 这条护栏跑的是**脚本里那段真代码**（抽出来在 sh 下执行），因为这两个错误
// 都属于"脚本照常跑完、打印一行看起来很像回事的地址，而那个地址打不开"：
//
//	① 拿监听地址当访问地址：填 0.0.0.0:8080 → 印出 https://127.0.0.1:8080/admin/
//	   （端口语义丢了、只有本机能开、协议还可能不是 https）；
//	② 只按"有没有 public_origin"分叉：**管理端独立监听**的部署被印成
//	   <公开地址>/admin/ —— 而那个地址由公开监听提供服务，/admin/ 根本不在
//	   那棵路由树上。真机（腾讯云，selfsigned + 管理端 127.0.0.1:8081）：
//	   公开看板能开、按提示访问 /admin/ 打不开，用户以为"后台没装上"。
//
// 生成一份"只是看看"的断言是不够的——所以这里把函数体抽出来跑，覆盖四种形态。
func TestAdminEntryURLMatchesHowUsersActuallyReachIt(t *testing.T) {
	funcs := shellFuncsForAdminEntry(t)

	cases := []struct {
		name         string
		terminator   string
		publicListen string
		adminListen  string
		publicOrigin string
		adminOrigin  string
		proxyDomains string
		scheme       string
		wantURL      string
		wantNoteHas  string
	}{
		{
			// 真机那一例的**旧默认**：独立监听 + 只绑回环。地址必须照实说是本机地址，
			// 并且必须说明公网该怎么访问（把回环地址当成公网地址贴出去是最实际的危害）。
			name:       "管理端独立监听且只绑回环",
			terminator: "master", publicListen: "0.0.0.0:443", adminListen: "127.0.0.1:8081",
			publicOrigin: "https://166.0.18.99", scheme: "https",
			wantURL: "https://127.0.0.1:8081/admin/", wantNoteHas: "本机",
		},
		{
			// 独立监听 + 对外可连：同一个主机、**管理端自己的端口**。
			name:       "管理端独立监听且对外可连",
			terminator: "master", publicListen: "0.0.0.0:443", adminListen: "0.0.0.0:8081",
			publicOrigin: "https://166.0.18.99", scheme: "https",
			wantURL: "https://166.0.18.99:8081/admin/", wantNoteHas: "防火墙",
		},
		{
			// 新默认：与公开站共用同一个监听（后台在公开地址的 /admin/ 下）。
			name:       "管理与公开共用同一个监听",
			terminator: "master", publicListen: "0.0.0.0:443", adminListen: "",
			publicOrigin: "https://166.0.18.99", scheme: "https",
			wantURL: "https://166.0.18.99/admin/", wantNoteHas: "同站",
		},
		{
			// 反代形态：对外就是域名，与内部监听端口无关（回环是**正确**的默认值）。
			name:       "反代形态下管理端监听回环",
			terminator: "caddy", publicListen: "127.0.0.1:8080", adminListen: "127.0.0.1:8081",
			publicOrigin: "https://mo.example.com", proxyDomains: "mo.example.com", scheme: "https",
			wantURL: "https://mo.example.com/admin/", wantNoteHas: "反代",
		},
		{
			// 显式给了管理地址：以它为准（有人把后台放在另一个域名下）。
			name:       "显式配置管理地址时以它为准",
			terminator: "master", publicListen: "0.0.0.0:443", adminListen: "127.0.0.1:8081",
			publicOrigin: "https://a.example.com", adminOrigin: "https://adm.example.com", scheme: "https",
			wantURL: "https://adm.example.com/admin/",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := runShellSnippet(t, adminEntrySnippet(funcs, c.terminator, c.publicListen,
				c.adminListen, c.publicOrigin, c.adminOrigin, c.proxyDomains, c.scheme))
			lines := strings.SplitN(strings.TrimRight(out, "\n"), "\n", 2)
			if lines[0] != c.wantURL {
				t.Errorf("后台入口地址 = %q，期望 %q\n（监听=%q 公开地址=%q 反代=%q）",
					lines[0], c.wantURL, c.adminListen, c.publicOrigin, c.terminator)
			}
			note := ""
			if len(lines) > 1 {
				note = lines[1]
			}
			if c.wantNoteHas != "" && !strings.Contains(note, c.wantNoteHas) {
				t.Errorf("说明里没有提到 %q：%q\n"+
					"  地址对不对只是一半——说不清「这是本机地址 / 要走反代」的话，"+
					"用户会照着它去配防火墙，或者把它当公网地址贴给别人。", c.wantNoteHas, note)
			}
		})
	}
}

// 管理端监听的**默认值**随"谁终结 TLS"变。
//
//	· Master 自己终结 TLS（没有反代）→ 留空 = 与公开站共用同一个监听。
//	  填回环端口在这个形态下等于装出一个"只有本机能打开"的后台，
//	  而失败现场看起来像"后台没装上"（真机就是这么撞上的）。
//	· 反代在前（Caddy 代管）→ 127.0.0.1:8081，由反代把 /admin/ 分流过来。
//
// 同时钉住"显式给的值不许被默认值覆盖"——那会把 `--admin-listen` 变成摆设。
func TestAdminListenDefaultFollowsTerminator(t *testing.T) {
	cases := []struct {
		name       string
		terminator string
		extra      []string
		want       string
	}{
		{"Master 自己终结 TLS：与公开站共用同一个监听", "master", nil, `admin_listen: ""`},
		{"反代在前：回环 8081，由反代分流", "caddy", nil, `admin_listen: "127.0.0.1:8081"`},
		{"显式指定时以它为准", "master", []string{"--admin-listen", "127.0.0.1:8081"},
			`admin_listen: "127.0.0.1:8081"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := []string{"--bin", fakeBin(t), "--dry-run", "--tls-terminator", c.terminator}
			args = append(args, c.extra...)
			out, rc := runDeployScript(t, args...)
			if rc != 0 {
				t.Fatalf("dry-run 应当成功，实际 rc=%d。\n输出：\n%s", rc, out)
			}
			// 先确认真的走到了"打印将要写入的配置"这一步：否则 rc=0 什么也说明不了。
			if !strings.Contains(out, "public_listen:") {
				t.Fatalf("dry-run 没有打印出将要写入的配置，rc=0 说明不了任何事。\n输出：\n%s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("期望配置里出现 %s，实际输出：\n%s", c.want, out)
			}
		})
	}
}

// shellFuncsForAdminEntry 抽出脚本里"地址推导"那一段（split_port → resolve_admin_entry）。
// 定位靠两个锚点，缺一个就报"判据已与脚本写法脱节"——静默地抽到半截、
// 跑出一堆空值再断言失败，会让人以为是实现错了。
func shellFuncsForAdminEntry(t *testing.T) string {
	t.Helper()
	body := readRepoFile(t, "../../scripts/deploy-master.sh")
	start := strings.Index(body, "split_port() {")
	end := strings.Index(body, "# 公开 IP 只在")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("没能在脚本里定位到地址推导那一段（判据已与脚本写法脱节）")
	}
	block := strings.TrimRight(body[start:end], " \t\r\n")
	// 前面补一个换行：第一个函数就在片段开头，没有它 "\nxxx() {" 判不到。
	hay := "\n" + block
	for _, fn := range []string{"split_port", "split_host", "reachable_host",
		"is_loopback_listen", "admin_shares_public_listener", "resolve_admin_entry"} {
		if !strings.Contains(hay, "\n"+fn+"() {") {
			t.Fatalf("抽出来的片段里缺 %s()（判据已与脚本写法脱节）", fn)
		}
	}
	return block
}

// adminEntrySnippet 拼出"给一组配置、打印地址与说明"的 sh 片段。
func adminEntrySnippet(funcs, terminator, publicListen, adminListen, publicOrigin, adminOrigin, proxyDomains, scheme string) string {
	return "set -eu\n" + funcs + "\n" +
		"TLS_TERMINATOR=" + shQuote(terminator) + "\n" +
		"PUBLIC_LISTEN=" + shQuote(publicListen) + "\n" +
		"ADMIN_LISTEN=" + shQuote(adminListen) + "\n" +
		"PUBLIC_ORIGIN=" + shQuote(publicOrigin) + "\n" +
		"ADMIN_ORIGIN=" + shQuote(adminOrigin) + "\n" +
		"PROXY_DOMAINS=" + shQuote(proxyDomains) + "\n" +
		"PUBIP=''\n" +
		"resolve_admin_entry " + shQuote(scheme) + "\n" +
		`printf '%s\n%s\n' "$ADMIN_URL" "$ADMIN_NOTE"` + "\n"
}

// runShellSnippet 跑一段 sh（与部署脚本同一解释器族）。
func runShellSnippet(t *testing.T, script string) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("找不到 sh，跳过（本护栏用真实脚本片段跑行为）")
	}
	out, err := exec.Command(sh, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("片段执行失败：%v\n输出：\n%s", err, out)
	}
	return string(out)
}

// shQuote 把值放进单引号里（`'` 用 '\” 收尾再接）。
func shQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}
