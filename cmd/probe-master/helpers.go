package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yourorg/probe/internal/auth"
	"github.com/yourorg/probe/internal/config"
	"github.com/yourorg/probe/internal/db"
	"github.com/yourorg/probe/internal/dist"
	"github.com/yourorg/probe/internal/store"
)

// 本文件包含子命令与公开快照构建函数。

// ---------------------------------------------------------------------------
// 公开快照构建（A9 / 7.3）
// ---------------------------------------------------------------------------

// 字段映射与概览汇总的唯一实现在 internal/state/public.go，这里只负责注入。
func deref64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func jsonUnmarshal(raw []byte, v any) bool { return json.Unmarshal(raw, v) == nil }

// ---------------------------------------------------------------------------
// init-admin / reset-admin（9.3 / S11）
// ---------------------------------------------------------------------------

func runInitAdmin(args []string) error {
	fs := flag.NewFlagSet("init-admin", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/probe-master/master.yaml", "配置文件路径")
	username := fs.String("username", "admin", "管理员用户名")
	password := fs.String("password", "", "密码；留空则交互输入")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadMaster(*cfgPath)
	if err != nil {
		return err
	}
	credentials, err := db.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer credentials.Close()
	if err := credentials.Migrate(); err != nil {
		return err
	}
	repo := store.New(credentials)

	if existing, _ := repo.GetAdminByUsername(*username); existing != nil {
		return fmt.Errorf("管理员 %s 已存在（如需重置密码请用 reset-admin）", *username)
	}

	pw := *password
	if pw == "" {
		pw, err = promptPassword()
		if err != nil {
			return err
		}
	}
	if len(pw) < 12 {
		return fmt.Errorf("密码至少 12 位")
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	id, err := repo.CreateAdmin(*username, hash, nil, time.Now().Unix())
	if err != nil {
		return err
	}
	_ = repo.Audit(time.Now().Unix(), "cli", "admin.create", fmt.Sprint(id), "local")

	// 一并创建主密钥（若不存在）。
	// 理由：master_key_file 路径不可写属于部署配置错误，应该在这里就暴露，
	// 而不是等第一次真正启动 Master 时才失败；而且上线检查清单要求提前备份
	// master.key，没有这个文件就无从备份。注意主密钥一旦丢失，
	// 已加密的告警渠道凭证（telegram bot_token 等）将无法解密。
	if _, err := auth.LoadOrCreateKeyring(cfg.MasterKeyFile); err != nil {
		return fmt.Errorf("创建主密钥失败（检查 master_key_file 路径是否可写）: %w", err)
	}

	// 建议性提醒：启用 TOTP 并保存恢复码（上线检查清单）。
	fmt.Printf("已创建管理员 %s（id=%d）。请在后台启用 TOTP 并保存恢复码。\n", *username, id)
	fmt.Printf("主密钥已就绪：%s —— 请立即备份到离线位置，丢失后无法解密告警凭证。\n", cfg.MasterKeyFile)
	return nil
}

func runResetAdmin(args []string) error {
	fs := flag.NewFlagSet("reset-admin", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/probe-master/master.yaml", "配置文件路径")
	username := fs.String("username", "admin", "管理员用户名")
	password := fs.String("password", "", "新密码；留空则交互输入")
	phone := fs.Bool("i-know-what-i-am-doing", false, "确认在 Master 主机本机执行")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// 仅限本机执行：要求显式确认，且不得通过网络接口触发（S11）。
	if !*phone {
		return fmt.Errorf("reset-admin 仅限本机执行，请加 --i-know-what-i-am-doing 确认")
	}

	cfg, err := config.LoadMaster(*cfgPath)
	if err != nil {
		return err
	}
	credentials, err := db.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer credentials.Close()
	repo := store.New(credentials)

	admin, err := repo.GetAdminByUsername(*username)
	if err != nil || admin == nil {
		return fmt.Errorf("管理员 %s 不存在", *username)
	}
	pw := *password
	if pw == "" {
		pw, err = promptPassword()
		if err != nil {
			return err
		}
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	if err := repo.UpdatePassword(admin.ID, hash); err != nil {
		return err
	}
	_ = repo.ResetLoginFailures(admin.ID)
	_ = repo.Audit(time.Now().Unix(), "cli", "admin.reset_password", fmt.Sprint(admin.ID), "local")
	fmt.Println("密码已重置。")
	return nil
}

func promptPassword() (string, error) {
	fmt.Fprint(os.Stderr, "请输入密码（至少 12 位）：")
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// ---------------------------------------------------------------------------
// dist pull（12.6）
// ---------------------------------------------------------------------------

// runDist 实现 `probe-master dist pull --version <版本> --from <URL>`：
// 从第三方源（GitHub Releases / CDN）拉取发布文件并校验后入库（12.6）。
//
// 为什么必须有校验：`dist` 是"新装 Agent 的自举来源"。若拉取过程被中间人替换，
// 所有新节点都会装上被污染的二进制——这是全链路里唯一"一次污染、全部受害"的点。
// 所以流程严格固定为：先取 SHA256SUMS（本身取自 `--sums-from`，默认与 `--from` 同源）
// → 逐文件下载 → 每个文件必须命中 SHA256SUMS 里的一条 → 校验通过才落盘。
// 任何一个文件校验失败，就删除已下载的全部内容（不留半套），避免"部分可用"的错觉。
func runDist(args []string) error {
	if len(args) == 0 || args[0] != "pull" {
		return fmt.Errorf("用法：probe-master dist pull --version <版本> --from <URL> [--sums-from <URL>] [--platform linux_amd64]")
	}
	fs := flag.NewFlagSet("dist pull", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/probe-master/master.yaml", "配置文件路径")
	version := fs.String("version", "", "版本号")
	from := fs.String("from", "", "发布文件来源 URL 前缀（不含文件名，如 https://example.com/v1.2.3）")
	sumsFrom := fs.String("sums-from", "", "SHA256SUMS 的来源 URL（默认：与 --from 同源）")
	pubKey := fs.String("pubkey", "", "minisign 公钥文本或文件路径；提供后必须存在并校验通过 SHA256SUMS.minisig（6.8）")
	platform := fs.String("platform", "", "只拉取指定平台（如 linux_amd64 / windows_amd64 / freebsd_amd64）；留空表示全部")
	all := fs.Bool("all", false, "拉取全部平台（默认行为；与 --platform 互斥，保留作显式声明）")
	dryRun := fs.Bool("dry-run", false, "只列出将要拉取的文件与校验结果，不写盘")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *version == "" || *from == "" {
		return fmt.Errorf("--version 与 --from 均为必需")
	}
	if *platform != "" && *all {
		return fmt.Errorf("--platform 与 --all 不能同时使用")
	}

	cfg, err := config.LoadMaster(*cfgPath)
	if err != nil {
		return err
	}
	dir := filepath.Join(cfg.DataDir, "dist", *version)
	base := strings.TrimRight(*from, "/")
	sumsURL := *sumsFrom
	if sumsURL == "" {
		sumsURL = base + "/SHA256SUMS"
	}

	client := &http.Client{Timeout: 5 * time.Minute}

	// 1) 取 SHA256SUMS。它是后续所有校验的信任根。
	sumsBody, err := fetchRaw(client, sumsURL)
	if err != nil {
		return fmt.Errorf("获取 SHA256SUMS 失败：%w", err)
	}
	sums := dist.ParseSums(sumsBody)
	if len(sums) == 0 {
		return fmt.Errorf("SHA256SUMS 为空（%s）", sumsURL)
	}

	// 1.5) minisign 验签（6.8）。★ 必须在用 sums 校验任何二进制之前完成。
	//
	// 顺序至关重要：SHA-256 只能证明"这个文件和 SHA256SUMS 记的一致"，
	// 而 SHA256SUMS 本身如果来自被劫持的镜像，攻击者可以连它一起换掉——
	// 那样哈希校验会"全部通过"，却装上了被投毒的二机制。
	// 签名把信任从"来源 URL 可信"收敛成"这份校验和出自维护者私钥"，
	// 这才是 SHA256SUMS 唯一真正的信任锚。
	if *pubKey != "" {
		if err := verifySumsSignature(client, base, sumsBody, *pubKey, sumsURL); err != nil {
			return err
		}
	} else {
		// 未配置公钥时给出显式提示，而不是静默跳过——静默跳过会让
		// 运维以为"校验都做了"，实际上最关键的一环是缺的。
		fmt.Println("⚠ 未提供 --pubkey：已跳过 SHA256SUMS 的 minisign 验签。")
		fmt.Println("  当前仅能证明文件与 SHA256SUMS 一致，无法证明 SHA256SUMS 本身可信。")
		fmt.Println("  生产环境请用 --pubkey 指定公钥（详见 docs 的发布流程）。")
	}
	fmt.Printf("已获取 SHA256SUMS（%d 条），来源 %s\n", len(sums), sumsURL)

	// 2) 选出要拉取的文件。文件名约定：probe-agent_<os>_<arch>[.exe]。
	type job struct{ name, want string }
	var jobs []job
	names := make([]string, 0, len(sums))
	for n := range sums {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !strings.HasPrefix(n, "probe-agent_") {
			continue
		}
		if *platform != "" && !strings.Contains(n, *platform) {
			continue
		}
		jobs = append(jobs, job{name: n, want: sums[n]})
	}
	if len(jobs) == 0 {
		return fmt.Errorf("SHA256SUMS 中没有匹配的产物（--platform=%q）", *platform)
	}
	if !*dryRun {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	// 3) 逐个下载 + 校验。任一失败即整体回滚。
	var written []string
	rollback := func() {
		for _, p := range written {
			_ = os.Remove(p)
		}
	}
	for _, j := range jobs {
		url := base + "/" + j.name
		dst := filepath.Join(dir, j.name)
		sum, size, err := downloadVerified(client, url, dst, j.want, *dryRun)
		if err != nil {
			rollback()
			return fmt.Errorf("拉取 %s 失败：%w（已回滚本次全部下载）", j.name, err)
		}
		written = append(written, dst)
		fmt.Printf("  ✓ %-40s %8s  %s\n", j.name, humanBytes(size), shortSum(sum))
	}

	// 4) 顺带把 SHA256SUMS 自己落到目标目录（供 /dist/<version>/SHA256SUMS 对外提供）。
	//
	// 这里**直接落原始字节**（sumsBody），不按解析结果重新拼装：
	// 重新拼装会改变行序/空白，而签名是对"原始字节"算的——一旦重拼，
	// 落盘后的 SHA256SUMS 就与 .minisig 对不上了，后续本地复验必然失败。
	if !*dryRun {
		sumsPath := filepath.Join(dir, "SHA256SUMS")
		if err := os.WriteFile(sumsPath, sumsBody, 0o644); err != nil {
			rollback()
			return fmt.Errorf("写入 SHA256SUMS 失败：%w", err)
		}
		// 签名与校验和成对保存：Master 自己对外提供 /dist/ 时会把两者一起发出去，
		// 新节点就能在下载时做同样的验签（12.8 的完整链路）。
		if *pubKey != "" {
			sigBody, err := fetchRaw(client, base+"/SHA256SUMS.minisig")
			if err == nil && len(sigBody) > 0 {
				if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS.minisig"), sigBody, 0o644); err != nil {
					rollback()
					return fmt.Errorf("写入 SHA256SUMS.minisig 失败：%w", err)
				}
				fmt.Println("  ✓ SHA256SUMS.minisig 已一并保存（供 /dist 对外验签）")
			}
		}
	}
	_ = version

	if *dryRun {
		fmt.Printf("dry-run 完成：%d 个文件全部通过校验，未写盘。目标目录将是 %s\n", len(jobs), dir)
		return nil
	}
	fmt.Printf("完成：%d 个文件已校验并写入 %s\n", len(jobs), dir)
	return nil
}

// fetchRaw 下载纯文本内容（带上限与状态码检查）。
func fetchRaw(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 限制大小：SHA256SUMS 是纯文本列表，1 MB 足够覆盖数千个文件。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// loadPubKey 把 `--pubkey` 的值解析成公钥文本，支持两种写法：
//   - 直接给公钥内容（以 `untrusted comment:` 或 `RW` 开头的多行文本）
//   - 给公钥文件路径（推荐：写成 /etc/probe-master/minisign.pub，避免命令行里粘贴长文本）
//
// 判定顺序：先看是否是已存在的文件，再当作文本。这样两种写法都不会歧义。
func loadPubKey(spec string) (string, error) {
	if st, err := os.Stat(spec); err == nil && !st.IsDir() {
		b, err := os.ReadFile(spec)
		if err != nil {
			return "", fmt.Errorf("读取公钥文件 %s 失败：%w", spec, err)
		}
		return string(b), nil
	}
	if strings.TrimSpace(spec) == "" {
		return "", fmt.Errorf("--pubkey 为空")
	}
	return spec, nil
}

// verifySumsSignature 校验 SHA256SUMS 的 minisign 签名（6.8）。
//
// 签名文件固定命名为 `SHA256SUMS.minisig`，与 SHA256SUMS 同源（同一个 base）。
// 为什么固定同名而不给个 --sig-url 参数：发布流程里两者必须同源，
// 多一个可配置项就多一种"配错但看起来正常"的可能（比如签名取自 A 源、校验和取自 B 源）。
func verifySumsSignature(client *http.Client, base string, sumsBody []byte, pubSpec, sumsURL string) error {
	pubText, err := loadPubKey(pubSpec)
	if err != nil {
		return err
	}
	// 公钥格式提前校验：格式错的公钥会让后面所有文件都"验签失败"，
	// 而真正的原因（公钥本身坏了）会被埋在一堆失败里。
	if _, err := auth.ParseMinisignPublicKey(pubText); err != nil {
		return fmt.Errorf("minisign 公钥无效：%w", err)
	}

	sigURL := base + "/SHA256SUMS.minisig"
	sigBody, err := fetchRaw(client, sigURL)
	if err != nil {
		return fmt.Errorf("获取签名失败（%s）：%w\n"+
			"提示：若该源确实未提供签名，请去掉 --pubkey；生产环境请换用带签名的源", sigURL, err)
	}

	if err := auth.VerifyMinisignText(pubText, sumsBody, string(sigBody)); err != nil {
		return fmt.Errorf("★ SHA256SUMS 验签失败（已中止，未下载任何文件）：%w", err)
	}
	// 打印信任注释（含签名时间），便于运维确认拿到的是哪一次发布。
	if sig, err := auth.ParseMinisignSignature(string(sigBody)); err == nil && sig.Trusted != "" {
		fmt.Printf("✓ SHA256SUMS 验签通过（%s）\n", sig.Trusted)
	} else {
		fmt.Println("✓ SHA256SUMS 验签通过")
	}
	_ = sumsURL
	return nil
}

// fetchSums 下载并解析 SHA256SUMS（保留旧签名以兼容既有测试与调用方）。
func fetchSums(client *http.Client, url string) (map[string]string, error) {
	body, err := fetchRaw(client, url)
	if err != nil {
		return nil, err
	}
	return dist.ParseSums(body), nil
}

// downloadVerified 下载到 dst 并强制校验 sha256；校验失败会删除半成品文件。
//
// 使用临时文件 + rename 的两段式写入：校验通过前，目标路径上不会出现任何内容，
// 这样即便中途崩溃也不会留下"看着像已完成、实际是坏的"的二进制。
func downloadVerified(client *http.Client, url, dst, want string, dryRun bool) (string, int64, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	h := sha256.New()
	if dryRun {
		n, err := io.Copy(h, resp.Body)
		if err != nil {
			return "", 0, err
		}
		got := hex.EncodeToString(h.Sum(nil))
		if got != want {
			return "", 0, fmt.Errorf("校验和不匹配（期望 %s，实际 %s）", shortSum(want), shortSum(got))
		}
		return got, n, nil
	}

	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return "", 0, err
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return "", 0, closeErr
	}

	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		_ = os.Remove(tmp)
		return "", 0, fmt.Errorf("校验和不匹配（期望 %s，实际 %s）", shortSum(want), shortSum(got))
	}
	// 产物按 0755 落盘（Agent 需要可执行；Windows 下该位无意义但不影响）。
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return "", 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", 0, err
	}
	return got, n, nil
}

func shortSum(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
