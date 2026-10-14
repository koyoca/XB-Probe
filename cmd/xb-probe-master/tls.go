package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/koyoca/xb-probe/internal/acme"
	"github.com/koyoca/xb-probe/internal/config"
	"github.com/koyoca/xb-probe/internal/tlsconf"
)

// xb-probe-master tls status —— 报告证书现在是"有了"还是"还在申请/为什么失败"。
//
// 为什么需要这条命令：ACME 的首次申请发生在后台（这是刻意的——一个 DNS 或
// 网络问题不该让整个 Master 起不来），于是"服务已经启动"与"证书已经就绪"
// 之间有一段**没有任何反馈**的空窗。部署脚本跑完了，浏览器还是打不开，
// 用户唯一能做的就是猜。这条命令把那段空窗变成一个有过程、有结果、有原因的报告。
//
// 它只读状态文件（由服务进程写），自己不发任何网络请求、也不碰证书：
// 进程之外去申请证书会白白消耗 CA 的速率配额，还可能和服务里那次申请打架。
func runTLS(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			args = args[1:]
		case "-h", "--help", "help":
			fmt.Print(tlsUsage)
			return nil
		default:
			if !strings.HasPrefix(args[0], "-") {
				return fmt.Errorf("未知的 tls 子命令 %q（可用：status）", args[0])
			}
		}
	}

	fs := flag.NewFlagSet("tls status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "/etc/xb-probe-master/master.yaml", "配置文件路径")
	waitSec := fs.Int("wait", 0, "等待证书就绪的秒数（0 = 只报告当前状态）")
	asJSON := fs.Bool("json", false, "以 JSON 输出原始状态")
	fs.Usage = func() { fmt.Fprint(os.Stderr, tlsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadMaster(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.TLS.Mode == "none" {
		fmt.Println("TLS 未启用（tls.mode=none）：证书由前置的 Caddy/Nginx 负责。")
		return nil
	}

	statusPath := tlsconf.StatusPath(cfg.TLS, cfg.DataDir)
	start := time.Now()
	// 起点的容差：服务可能刚好在我们读文件的前一秒开始尝试，
	// 把它当成"旧的错误"会让人以为申请已经失败过一次。
	startUnix := start.Add(-5 * time.Second).Unix()

	var last tlsconf.Status
	haveLast := false
	deadline := start.Add(time.Duration(*waitSec) * time.Second)
	progressed := false

	for {
		st, err := tlsconf.ReadStatus(statusPath)
		readable := err == nil
		if readable {
			last, haveLast = st, true
		}

		if *asJSON {
			if !readable {
				return fmt.Errorf("读取证书状态失败（%s）：%w", statusPath, err)
			}
			raw, _ := json.MarshalIndent(st, "", "  ")
			fmt.Println(string(raw))
			return nil
		}

		if readable && st.HasCert() {
			printCertReady(cfg, st, statusPath)
			return nil
		}
		// 只认**本轮**产生的错误：上次启动留下的旧错误会让部署脚本
		// 误报"申请失败"，而这次其实还在申请中。
		if readable && st.LastError != "" && st.LastAttemptAt >= startUnix {
			printCertFailed(cfg, st, statusPath)
			return errCertFailed
		}

		if *waitSec <= 0 || time.Now().After(deadline) {
			break
		}
		if !progressed {
			printCertHeader(cfg, statusPath, haveLast, last)
			progressed = true
		}
		fmt.Printf("    等待中…（已等待 %s）\n", fmtDur(time.Since(start)))
		time.Sleep(time.Second)
	}

	if !*asJSON {
		printCertHeader(cfg, statusPath, haveLast, last)
		if haveLast && last.LastAttemptAt > 0 {
			fmt.Printf("    最近一次尝试：%s\n", time.Unix(last.LastAttemptAt, 0).Format("2006-01-02 15:04:05"))
		}
		// 等待超时后，**即使这条错误不是本轮产生的也要打出来**。
		//
		// 场景：部署期前台申请失败过一次，进程在"失败冷却"里重新起来
		// （见 tlsconf 的 syncObtainCooldown —— 不这么做的话 systemd 会每
		// 3 秒重启一次、每次烧掉一个 CA 的失败授权额度）。此时状态文件里
		// 会留一条**上一轮**的失败原因，而上面那个"只认本轮错误"的判断
		// 会把它跳过去。不放出来的话，用户在这一步只能看到一句
		// "还没就绪"，而真正的失败原因就摆在文件里却没人转述。
		if haveLast && last.LastError != "" {
			fmt.Printf("    上一次申请失败的原因：%s\n", last.LastError)
		}
	}
	fmt.Println("==> 证书尚未就绪（申请还在进行中，或服务还没开始申请）")
	printRetryHints(cfg, statusPath)
	// 退出码 2 与"申请失败"（1）分开：前者是"再等等/去看日志"，
	// 后者是"必须处理"，部署脚本据此给不同的下一步建议。
	return errCertPending
}

var (
	errCertFailed  = fmt.Errorf("证书申请失败")
	errCertPending = fmt.Errorf("证书尚未就绪")
)

const tlsUsage = `xb-probe-master tls status —— 查看证书状态

  -config <路径>   配置文件路径（默认 /etc/xb-probe-master/master.yaml）
  -wait <秒>       等待证书就绪的秒数（0 = 只报告当前状态）
  -json            以 JSON 输出原始状态

退出码：0 = 证书已就绪；1 = 本轮申请失败（会打印原因与排查方向）；
        2 = 还没结果（申请仍在进行）。

状态由 Master 进程写在 <缓存目录>/status.json，本命令只读它，不发起申请。
`

func printCertHeader(cfg *config.MasterConfig, statusPath string, have bool, st tlsconf.Status) {
	fmt.Printf("==> 证书状态：tls.mode=%s\n", cfg.TLS.Mode)
	if st.Challenge != "" {
		fmt.Printf("    校验方式：%s%s\n", st.Challenge, challengeNote(st.Challenge))
	}
	if len(st.Domains) > 0 {
		fmt.Printf("    标识符：%s\n", strings.Join(st.Domains, ", "))
	}
	if !have {
		fmt.Printf("    状态文件：%s（尚未生成，服务可能还没启动过）\n", statusPath)
	}
}

func challengeNote(ch string) string {
	switch ch {
	case acme.ChallengeHTTP01:
		return "（需要 TCP 80 可达）"
	case acme.ChallengeTLSALPN01:
		return "（需要 TCP 443 可达并放行 ALPN）"
	case acme.ChallengeDNS01:
		return "（需要域名 DNS 的写权限，支持通配符）"
	}
	return ""
}

func printCertReady(cfg *config.MasterConfig, st tlsconf.Status, statusPath string) {
	printCertHeader(cfg, statusPath, true, st)
	fmt.Println("==> 证书已就绪")
	if st.Subject != "" {
		fmt.Printf("    主体：%s\n", st.Subject)
	}
	if st.Issuer != "" {
		fmt.Printf("    签发者：%s\n", st.Issuer)
	}
	if st.NotAfter > 0 {
		fmt.Printf("    有效期：%s → %s（剩 %d 天）\n",
			time.Unix(st.NotBefore, 0).Format("2006-01-02 15:04"),
			time.Unix(st.NotAfter, 0).Format("2006-01-02 15:04"),
			daysLeft(st.NotAfter))
	}
	if len(st.SANs) > 0 {
		fmt.Printf("    覆盖：%s\n", strings.Join(st.SANs, ", "))
	}
	if st.SPKISHA256 != "" {
		// Agent 侧可以按 SPKI 指纹固定证书（自签部署必用）；指纹换了
		// 所有固定过它的 Agent 都会连不上，所以这里要显式打出来。
		fmt.Printf("    SPKI SHA-256：%s\n", st.SPKISHA256)
	}
	if st.NextRenewal > 0 {
		fmt.Printf("    下次续期：%s（到期前 1/3 时自动开始）\n",
			time.Unix(st.NextRenewal, 0).Format("2006-01-02 15:04"))
	}
	if p := tlsconf.CertPath(cfg.TLS, cfg.DataDir); p != "" {
		fmt.Printf("    证书文件：%s\n", p)
	}
}

func printCertFailed(cfg *config.MasterConfig, st tlsconf.Status, statusPath string) {
	printCertHeader(cfg, statusPath, true, st)
	fmt.Println("==> 证书申请失败")
	if st.LastAttemptAt > 0 {
		fmt.Printf("    最近一次尝试：%s\n", time.Unix(st.LastAttemptAt, 0).Format("2006-01-02 15:04:05"))
	}
	fmt.Printf("    原因：%s\n", st.LastError)
	printRetryHints(cfg, statusPath)
}

// printRetryHints 给出可执行的排查方向。
//
// 只写"请看日志"是没用的：日志里的那句错误往往正是用户看不懂的那句。
// 这里按挑战方式列出"最可能的三件事"，每一条都能直接复制去跑。
func printRetryHints(cfg *config.MasterConfig, statusPath string) {
	fmt.Println("    排查：")
	switch {
	case cfg.TLS.Mode != "acme":
		fmt.Printf("      · 检查证书文件与权限；systemctl restart xb-probe-master 会立即重试\n")
	case cfg.TLS.ACME.Challenge == acme.ChallengeHTTP01:
		fmt.Printf("      · 域名是否已解析到本机：dig +short %s\n", firstDomain(cfg))
		fmt.Printf("      · 80 端口是否空闲且可达：ss -ltnp | grep ':80'\n")
		fmt.Printf("        80 没在听 = 申请**一定**失败（CA 是从公网访问这个端口来验的）。\n")
		fmt.Printf("        报 permission denied → 服务无权绑定特权端口（80 < 1024），\n")
		fmt.Printf("        需要给服务 CAP_NET_BIND_SERVICE（systemd: AmbientCapabilities）。\n")
		fmt.Printf("      · 80 被反代占用、且反代会转发 /.well-known/ 时，把 acme.http_addr 设为 off\n")
	case cfg.TLS.ACME.Challenge == acme.ChallengeTLSALPN01:
		fmt.Printf("      · 域名是否已解析到本机：dig +short %s\n", firstDomain(cfg))
		fmt.Printf("      · 443 端口是否由本进程直接监听（反代终结 TLS 时选 http-01 或 dns-01）\n")
	case cfg.TLS.ACME.Challenge == acme.ChallengeDNS01:
		fmt.Printf("      · DNS API Token 是否有该域名的 Zone:DNS:Edit 权限\n")
		fmt.Printf("      · 域名是否托管在同一个 DNS 服务商账户下\n")
	}
	fmt.Printf("      · 完整状态：cat %s；详细日志：journalctl -u xb-probe-master -n 80\n", statusPath)
	fmt.Println("      · 服务每 2 分钟自动重试；改完配置后 systemctl restart xb-probe-master 可立即重试")
}

func firstDomain(cfg *config.MasterConfig) string {
	if len(cfg.TLS.ACME.Domains) > 0 {
		return cfg.TLS.ACME.Domains[0]
	}
	if len(cfg.TLS.ACME.IPs) > 0 {
		return cfg.TLS.ACME.IPs[0]
	}
	return "<域名>"
}

func daysLeft(unix int64) int64 {
	d := time.Until(time.Unix(unix, 0))
	return int64(d.Hours() / 24)
}

func fmtDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
}
