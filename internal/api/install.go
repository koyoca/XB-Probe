package api

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/koyoca/xb-probe/internal/alert"
	"github.com/koyoca/xb-probe/internal/auth"
	"github.com/koyoca/xb-probe/internal/model"
)

// 一键安装（文档 12.2 / 12.8）。
//
// 原则：Master 只提供静态文件（安装脚本与 Agent 二进制），不向节点推送任何内容；
// 执行动作始终由节点上的人发起，与“无远程控制通道”一致。

// 内嵌副本必须与仓库根的 scripts/ 保持逐字节一致。
//
// 为什么要复制而不是直接嵌：go:embed 不允许 `..`，无法嵌包目录之外的文件。
// 复制带来的漂移风险由 install_embed_test.go 兜住（两份不一致时 go test 直接红），
// 这个 generate 指令则让"重新同步"变成一条命令而不是手工 cp。
//
//	go generate ./internal/api/...
//
//go:generate cp ../../scripts/install.sh scripts/install.sh
//go:generate cp ../../scripts/install.ps1 scripts/install.ps1
//go:embed scripts/install.sh scripts/install.ps1
var installScripts embed.FS

var releaseNameRe = regexp.MustCompile(`^xb-probe-agent_[a-z0-9]+_(amd64|arm64|armv7|armv6|386|riscv64|mips|mipsle|ppc64le|s390x)(\.exe)?$`)

// installAvailable 判断当前是否可以对外提供安装脚本与下载（12.8）。
//   - off        → 关闭
//   - always     → 始终提供
//   - on_demand  → 仅当存在有效注册码时提供（默认）
func (h *Handler) installAvailable() bool {
	mode := "on_demand"
	if h.InstallMode != nil {
		mode = h.InstallMode()
	}
	switch mode {
	case "off":
		return false
	case "always":
		return true
	default:
		n, err := h.Repo.CountActiveEnrollCodes(time.Now().Unix())
		return err == nil && n > 0
	}
}

// handleInstallSh 提供 /install.sh（免鉴权，不含任何节点秘密）。
func (h *Handler) handleInstallSh(w http.ResponseWriter, r *http.Request) {
	h.serveInstallScript(w, r, "scripts/install.sh", "sh")
}

// handleInstallPS1 提供 /install.ps1。
func (h *Handler) handleInstallPS1(w http.ResponseWriter, r *http.Request) {
	h.serveInstallScript(w, r, "scripts/install.ps1", "ps1")
}

func (h *Handler) serveInstallScript(w http.ResponseWriter, r *http.Request, path, kind string) {
	if !h.installAvailable() {
		// on_demand 模式下不存在有效注册码时直接 404，不暴露任何信息。
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "安装入口当前未开放")
		return
	}
	raw, err := installScripts.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "脚本不可用")
		return
	}

	masterURL := ""
	if h.MasterURL != nil {
		masterURL = h.MasterURL()
	}
	if masterURL == "" {
		masterURL = "https://" + r.Host // 为空时取请求的 Host（12.2）
	}
	distBase := h.Repo.GetSettingDefault("install.dist_base", "")
	if distBase == "" {
		distBase = masterURL + "/dist"
	}
	spki := h.Repo.GetSettingDefault("install.spki_sha256", "")
	// minisign 公钥（6.8）：注入到安装脚本里，让节点在下载时能验签 SHA256SUMS。
	// 留空表示该部署未启用签名校验（脚本会跳过这一步）。
	// 注意要做 shell 转义——公钥是多行文本，直接替换进脚本会把 heredoc/引号搞乱。
	pubkey := shellSingleQuote(h.Repo.GetSettingDefault("install.pubkey", ""))

	out := strings.NewReplacer(
		"__MASTER_URL__", masterURL,
		"__DIST_BASE__", distBase,
		"__SPKI__", spki,
		"__MINISIGN_PUB__", pubkey,
	).Replace(string(raw))

	// 剥掉 UTF-8 BOM。脚本文件在磁盘上是**带 BOM** 的，而分发的文本里不能有：
	//
	//	irm .../install.ps1 | [scriptblock]::Create(...)   ← 官方的一行安装命令
	//
	// 里开头的 U+FEFF 会被 PowerShell 当成第一个标识符的一部分，实测报
	// "无法将“\uFEFFWrite-Output”项识别为 cmdlet"——也就是**一行命令直接装不上**。
	//
	// 那为什么磁盘上还要 BOM？因为 Windows PowerShell 5.1 读 *.ps1 时**只看 BOM**
	// 判断编码，没有 BOM 就按当前 ANSI 代码页解码。中文 Windows 是 GBK，而文件是
	// UTF-8：解码错位后某个汉字的尾字节会吞掉紧跟其后的引号，于是整份脚本
	// **语法错误**，`powershell -File scripts\install.ps1` 完全跑不起来
	// （`-File` 是本机安装与 deploy-master.ps1 的用法，不能不管）。
	//
	// 两个消费者对同一个文件的要求正好相反，所以在**这一处**把差异抹平：
	// 磁盘带 BOM（给 -File 用），线上不带（给 iex 用）。
	out = strings.TrimPrefix(out, "\ufeff")

	ct := "text/x-shellscript; charset=utf-8"
	if kind == "ps1" {
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(out))
}

// shellSingleQuote 把任意文本安全地包成 shell 单引号字面量（6.8）。
//
// 安装脚本里的 minisign 公钥是多行文本，而脚本本身是 shell 代码——
// 未转义地插进去，公钥里的反引号/$( )/单引号会被 shell **当代码执行**。
// 设置项虽然只有管理员能改，但"管理员能写设置"不等于"管理员能在这台
// Master 上以服务身份执行任意命令"，这条边界必须守住。
//
// 单引号内的内容是字面量，唯一需要处理的是单引号本身：
// 用 `'\”` 收尾再重启一个单引号串（POSIX 标准写法）。
func shellSingleQuote(s string) string {
	if s == "" {
		return ""
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// handleDist 提供 /dist/ 下的发布文件（含 SHA256SUMS 与 SHA256SUMS.minisig）。
//
// 发布文件的**权威存放位置是** `data_dir/dist/<版本>/`（`xb-probe-master dist pull`
// 写在这里，后台"发布文件"页也按这个结构读）。但安装脚本要的是"当前这一版"，
// 它并不知道版本号，所以取的是扁平路径：
//
//	$DIST_BASE/xb-probe-agent_linux_amd64      安装脚本下载构件
//	$MASTER_URL/dist/SHA256SUMS             安装脚本取校验和（12.8）
//
// 于是这里把扁平路径当作**最新版本的别名**：先按字面找，找不到再落到最新版本目录里。
//
// 为什么用别名而不是"拉取时再拷一份到根目录"：那样会有两份物理副本，
// 而它们必然会漂移——发布新版本后忘了刷新扁平副本，就会出现
// "新装节点拿到旧二进制、老节点却升级到新二进制"的版本错位，且没有任何报错。
// 别名只有一个来源，不可能错位。别名解析用的 latestDistVersion 与 Agent
// 自升级用的是同一个函数，所以"安装脚本给的版本"与"升级目标版本"永远一致。
func (h *Handler) handleDist(w http.ResponseWriter, r *http.Request) {
	if !h.installAvailable() {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "分发入口当前未开放")
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/dist/")
	full, err := safeDistPath(h.DistDir, rel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "路径非法")
		return
	}
	if st, err := os.Stat(full); err != nil || st.IsDir() {
		// 扁平别名：/dist/<文件> → /dist/<最新版本>/<文件>
		if alt, ok := h.flatDistAlias(rel); ok {
			full = alt
		}
	}
	f, err := os.Open(full)
	if err != nil {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "文件不存在")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "文件不存在")
		return
	}
	if strings.HasSuffix(rel, "SHA256SUMS") || strings.HasSuffix(rel, ".minisig") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(rel)))
	}
	http.ServeContent(w, r, filepath.Base(rel), st.ModTime(), f)
}

// flatDistAlias 把扁平路径解析到最新版本目录下的同名文件；没有则返回 false。
//
// 只认**单段**路径（`<文件>`）。带子目录的请求（`1.0.0/x`、`a/b`）不走别名，
// 否则 `/dist/<版本>/<文件>` 会被改写成 `/dist/<最新版本>/<版本>/<文件>`，
// 把一个明确的"没有这个文件"变成令人费解的 404。
func (h *Handler) flatDistAlias(rel string) (string, bool) {
	if rel == "" || strings.Contains(rel, "/") {
		return "", false
	}
	version, err := latestDistVersion(h.DistDir)
	if err != nil || version == "" {
		return "", false
	}
	alt, err := safeDistPath(h.DistDir, filepath.Join(version, rel))
	if err != nil {
		return "", false
	}
	st, err := os.Stat(alt)
	if err != nil || st.IsDir() {
		return "", false
	}
	return alt, true
}

// safeDistPath 防路径穿越：规范化后必须落在 DistDir 内。
func safeDistPath(base, rel string) (string, error) {
	if base == "" {
		return "", fmt.Errorf("未配置分发目录")
	}
	clean := filepath.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || strings.Contains(clean, "..") {
		return "", fmt.Errorf("非法路径")
	}
	full := filepath.Join(base, filepath.FromSlash(clean))
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	absFull, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(absFull, absBase+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界")
	}
	return absFull, nil
}

// handleInstallCommand 生成一键安装命令（12.2）。
func (h *Handler) handleInstallCommand(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	var req struct {
		Platform string `json:"platform"`
		TTLMin   int    `json:"ttl_min"`
	}
	_ = decodeJSON(r, &req)
	if req.Platform == "" {
		req.Platform = "shell"
	}
	if req.TTLMin > 0 {
		_ = h.Repo.SetSetting("install.code_ttl_min", fmt.Sprint(req.TTLMin), time.Now().Unix())
	}
	if req.TTLMin < 5 || req.TTLMin > 1440 {
		req.TTLMin = int(h.Repo.GetIntSetting("install.code_ttl_min", 30))
	}

	code, expiresAt, err := h.newEnrollCode(id, actorOf(r), req.Platform)
	if err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, err.Error())
		return
	}

	masterURL := ""
	if h.MasterURL != nil {
		masterURL = h.MasterURL()
	}
	if masterURL == "" {
		masterURL = "https://" + r.Host
	}
	if !allowedURL(masterURL) {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam,
			"install.master_url 未设置或格式非法（必须是 https，且仅含字母数字与 .-:/_）")
		return
	}
	spki := h.Repo.GetSettingDefault("install.spki_sha256", "")
	if v := h.Repo.GetSettingDefault("install.agent_version", ""); v != "" {
		_ = v // 版本只影响下载路径，此处保持最新版
	}

	// 命令中参数一律加引号（S8）。
	q := `'` + code + `'`
	commands := map[string]string{
		"shell": fmt.Sprintf("curl -fsSL %s/install.sh | sudo sh -s -- --code %s", masterURL, q),
		"wget":  fmt.Sprintf("wget -qO- %s/install.sh | sudo sh -s -- --code %s", masterURL, q),
		"powershell": fmt.Sprintf(
			"& ([scriptblock]::Create((irm %s/install.ps1))) -Code %s", masterURL, `'`+code+`'`),
		"step_by_step": strings.Join([]string{
			fmt.Sprintf("curl -fsSLo install.sh %s/install.sh", masterURL),
			"less install.sh",
			fmt.Sprintf("sudo sh install.sh --code %s", q),
		}, "\n"),
	}
	if spki != "" {
		for k, v := range commands {
			if k == "powershell" {
				commands[k] = v + " -SpkiSha256 '" + spki + "'"
			} else if k != "step_by_step" {
				commands[k] = v + " --spki-sha256 '" + spki + "'"
			}
		}
	}

	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "install_command.create", fmt.Sprint(id), RealIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"code":       code,
		"expires_at": expiresAt,
		"commands":   commands,
	})
}

// newEnrollCode 生成一次性注册码（Crockford base32、库内仅存哈希与前 6 位提示、绑定节点）。
func (h *Handler) newEnrollCode(nodeID int64, actor, platform string) (string, int64, error) {
	code, err := auth.GenerateEnrollCode()
	if err != nil {
		return "", 0, fmt.Errorf("生成注册码失败")
	}
	ttlMin := h.Repo.GetIntSetting("install.code_ttl_min", 30)
	now := time.Now()
	expiresAt := now.Add(time.Duration(ttlMin) * time.Minute).Unix()

	// 提示位只存前 6 位（与 node_tokens.token_hint 同思路）：后台令牌面板要能
	// 看到"已签发但未被使用"的注册码，否则"码已发出但界面上无迹可查"，
	// 一次对话框关闭就永远拿不回来。
	hint := code
	if len(hint) > 6 {
		hint = hint[:6]
	}
	if err := h.Repo.CreateEnrollCode(nodeID, auth.EnrollCodeHash(code), hint,
		expiresAt, actor, platform, now.Unix()); err != nil {
		return "", 0, err
	}
	return code, expiresAt, nil
}

// handleAgentReleases 列出 dist 中各版本、各平台文件及其 SHA-256（12.2）。
func (h *Handler) handleAgentReleases(w http.ResponseWriter, r *http.Request) {
	type fileInfo struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256,omitempty"`
	}
	type release struct {
		Version string     `json:"version"`
		Files   []fileInfo `json:"files"`
	}

	var releases []release
	entries, err := os.ReadDir(h.DistDir)
	if err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "读取发布目录失败")
		return
	}

	found := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rel := release{Version: e.Name()}
		files, _ := os.ReadDir(filepath.Join(h.DistDir, e.Name()))
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			info := fileInfo{Name: name}
			if st, err := f.Info(); err == nil {
				info.Size = st.Size()
			}
			if releaseNameRe.MatchString(name) {
				info.SHA256 = fileSHA256(filepath.Join(h.DistDir, e.Name(), name))
				found[platformKey(name)] = true
			}
			rel.Files = append(rel.Files, info)
		}
		releases = append(releases, rel)
	}

	// 提示缺失的平台（12.6）
	expected := []string{
		"linux_amd64", "linux_arm64", "linux_armv7", "linux_armv6", "linux_386", "linux_riscv64",
		"darwin_amd64", "darwin_arm64", "freebsd_amd64", "freebsd_arm64",
		"windows_amd64", "windows_arm64",
	}
	var missing []string
	for _, p := range expected {
		if !found[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	sort.Slice(releases, func(i, j int) bool { return releases[i].Version > releases[j].Version })

	writeJSON(w, http.StatusOK, map[string]any{
		"releases": releases,
		"missing":  missing,
	})
}

func platformKey(fileName string) string {
	name := strings.TrimSuffix(fileName, ".exe")
	parts := strings.Split(name, "_")
	if len(parts) < 3 {
		return ""
	}
	return strings.Join(parts[1:], "_")
}

func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// buildChannelByID 读取并解密渠道配置，构造可发送的渠道（9.4）。
func (h *Handler) buildChannelByID(id int64) (alert.Channel, error) {
	rows, err := h.Repo.ListChannels()
	if err != nil {
		return nil, err
	}
	for _, c := range rows {
		if c.ID != id {
			continue
		}
		plain, err := h.Keyring.Decrypt(c.ConfigEnc)
		if err != nil {
			return nil, err
		}
		switch c.Type {
		case "telegram":
			var cfg alert.TelegramConfig
			if err := json.Unmarshal(plain, &cfg); err != nil {
				return nil, err
			}
			return alert.NewTelegramChannel(c.Name, cfg), nil
		case "smtp":
			var cfg alert.SMTPConfig
			if err := json.Unmarshal(plain, &cfg); err != nil {
				return nil, err
			}
			return alert.NewSMTPChannel(c.Name, cfg), nil
		}
	}
	return nil, nil
}
