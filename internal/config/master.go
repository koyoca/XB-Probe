package config

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// MasterConfig 对应 master.yaml（第 10 章）。
type MasterConfig struct {
	PublicListen   string    `yaml:"public_listen"`
	AdminListen    string    `yaml:"admin_listen"`
	DataDir        string    `yaml:"data_dir"`
	ThemesDir      string    `yaml:"themes_dir"`
	MasterKeyFile  string    `yaml:"master_key_file"`
	TrustedProxies []string  `yaml:"trusted_proxies"`
	PublicOrigin   string    `yaml:"public_origin"`
	AdminOrigin    string    `yaml:"admin_origin"`
	PublicHosts    []string  `yaml:"public_hosts"`
	AdminHosts     []string  `yaml:"admin_hosts"`
	TLS            TLSConfig `yaml:"tls"`
	LogLevel       string    `yaml:"log_level"`
}

// TLSConfig 描述证书获取方式与 TLS 相关策略。
//
// Mode 决定"证书从哪来"，四种选择互斥：
//
//	none        不启用 TLS。由前置 Caddy/Nginx 卸载 TLS，或本机/内网调试。
//	files       自定义证书：直接读 cert_file / key_file（已有的商业证书、
//	            其他 ACME 客户端签发的证书、企业内部 CA 签发的证书都走这条）。
//	selfsigned  自签证书：本进程生成并落盘，SAN 覆盖 selfsigned.hosts 里的
//	            域名与 **IP**（这就是"IP 证书"的本地可用形态——公网 CA
//	            不会给私有地址签证书）。
//	acme        自动申请：走 ACME v2（Let's Encrypt 等），支持域名证书与
//	            RFC 8738 的 IP 证书，挑战方式可选 http-01 / tls-alpn-01。
type TLSConfig struct {
	Mode          string           `yaml:"mode"`
	CertFile      string           `yaml:"cert_file"`
	KeyFile       string           `yaml:"key_file"`
	CacheDir      string           `yaml:"cache_dir"`
	MinVer        string           `yaml:"min_version"`
	HSTS          HSTSConfig       `yaml:"hsts"`
	SelfSign      SelfSignedConfig `yaml:"selfsigned"`
	ACME          ACMEConfig       `yaml:"acme"`
	DomainsLegacy []string         `yaml:"acme_domains"`
}

// HSTSConfig 是 Strict-Transport-Security 的开关与参数。
//
// 只在"客户端确实走 HTTPS"时才下发（本机 TLS，或可信代理带
// X-Forwarded-Proto: https）。在明文 HTTP 上下发 HSTS 是无意义的：
// 浏览器按规范直接忽略，却会让人误以为已经加固。
type HSTSConfig struct {
	// Enabled 默认 true（指针以区分"未配置"与"显式关闭"）。
	Enabled *bool `yaml:"enabled"`
	// MaxAge 单位秒，默认 31536000（1 年）。
	MaxAge int `yaml:"max_age"`
	// IncludeSubDomains 默认 false。开启后**所有子域**都会被强制 HTTPS，
	// 若同一主域下还有只跑 HTTP 的子域，浏览器会直接把它们打死——
	// 因此默认关闭，由操作者显式开启。
	IncludeSubDomains bool `yaml:"include_subdomains"`
	// Preload 默认 false，仅在你确实要提交到 HSTS preload 列表时开启。
	Preload bool `yaml:"preload"`
}

// SelfSignedConfig 描述自签证书。
type SelfSignedConfig struct {
	// Hosts 是写入 SAN 的条目，IP 与域名都可以（"192.168.1.10"、"probe.lan"）。
	// 留空则用 localhost / 127.0.0.1 / ::1。
	Hosts []string `yaml:"hosts"`
	// ValidDays 有效期天数，默认 825。
	ValidDays int `yaml:"valid_days"`
}

// ACMEConfig 描述自动申请。
type ACMEConfig struct {
	Email     string   `yaml:"email"`
	CA        string   `yaml:"ca"`
	Challenge string   `yaml:"challenge"`
	HTTPAddr  string   `yaml:"http_addr"`
	Domains   []string `yaml:"domains"`
	IPs       []string `yaml:"ips"`

	// SyncObtain 让首次申请**在启动时前台完成**，失败则进程直接起不来。
	//
	// 默认 false：一个 DNS 或网络问题不该让整个 Master 起不来，所以首次申请
	// 历来在后台跑、结果落进 status.json。部署脚本会在生成配置时把它设为
	// true——那一刻用户正在等一个确定答案（"到底能不能用 https"），
	// 而后台申请给出的是一段没有任何反馈的空窗。申请成功后部署脚本会把它
	// 改回 false（见 deploy-master.sh），于是正常重启不再被证书阻塞。
	SyncObtain bool `yaml:"sync_obtain"`

	// ---- challenge=dns-01 专用 ----
	//
	// DnsAPIProvider 目前只认 cloudflare；DnsAPIToken 是 Cloudflare 的
	// API Token（**不是** Global API Key）。
	//
	// 这个 Token 权限很大（能改你的 DNS），因此：
	//   - 请在 Cloudflare 上单独建一个只含 Zone:DNS:Edit + Zone:Zone:Read、
	//     且只覆盖目标域名的 Token，不要用账户级 Token；
	//   - master.yaml 是 0640 root:probe，与 master.key 同级，
	//     不要把它复制进备份目录或贴进工单。
	DnsAPIProvider string `yaml:"dns_provider"`
	DnsAPIToken    string `yaml:"dns_api_token"`
}

// 默认值常量。测试与文档都引用它们，避免各处硬编码不一致。
const (
	DefaultTLSMode       = "none"
	DefaultMinTLSVersion = "1.2"
	DefaultHSTSMaxAge    = 31536000
	DefaultSelfSignedDay = 825
	DefaultACMEChallenge = "http-01"
	DefaultACMEHTTPAddr  = ":80"
	DefaultACMECacheDir  = "tls"
	// DefaultACMEDNSProvider 是目前唯一实现的 dns-01 提供方。
	// 只实现一家时给个默认值，比让所有人都必须写出 "cloudflare" 更省事；
	// 写错别的名字仍会报错并列出可选值。
	DefaultACMEDNSProvider = "cloudflare"
)

// LoadMaster 读取 master.yaml 并填默认值。
//
// 解析用**严格模式**（KnownFields）：出现未知键直接报错，而不是静默忽略。
// 理由是"静默忽略"已经在部署脚本上真实咬过一次——脚本往配置文件里写了
// `install:` 段，而 install.* 其实是数据库设置（MasterConfig 里没有这个键），
// 于是那段配置既不报错、也永远不生效，只有人去比对后台设置页才能发现。
// 配置项写错名字是同类里最常见的一种：严格模式把它变成启动即失败，
// 而不是"看起来配好了、实际没配"。
func LoadMaster(path string) (*MasterConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	c := &MasterConfig{}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && err != io.EOF {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *MasterConfig) applyDefaults() {
	if c.PublicListen == "" {
		c.PublicListen = "127.0.0.1:8080"
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.ThemesDir == "" {
		c.ThemesDir = c.DataDir + "/themes"
	}
	if c.MasterKeyFile == "" {
		c.MasterKeyFile = c.DataDir + "/master.key"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}

	t := &c.TLS
	if t.Mode == "" {
		t.Mode = DefaultTLSMode
	}
	t.Mode = strings.ToLower(strings.TrimSpace(t.Mode))
	if t.CacheDir == "" {
		t.CacheDir = filepath.Join(c.DataDir, DefaultACMECacheDir)
	}
	if strings.TrimSpace(t.MinVer) == "" {
		t.MinVer = DefaultMinTLSVersion
	}
	if t.HSTS.Enabled == nil {
		on := true
		t.HSTS.Enabled = &on
	}
	if t.HSTS.MaxAge == 0 {
		t.HSTS.MaxAge = DefaultHSTSMaxAge
	}
	if t.SelfSign.ValidDays == 0 {
		t.SelfSign.ValidDays = DefaultSelfSignedDay
	}
	if len(t.SelfSign.Hosts) == 0 {
		t.SelfSign.Hosts = []string{"localhost", "127.0.0.1", "::1"}
	}
	if t.ACME.Challenge == "" {
		t.ACME.Challenge = DefaultACMEChallenge
	}
	t.ACME.Challenge = strings.ToLower(strings.TrimSpace(t.ACME.Challenge))
	t.ACME.DnsAPIProvider = strings.ToLower(strings.TrimSpace(t.ACME.DnsAPIProvider))
	if t.ACME.DnsAPIProvider == "" && t.ACME.Challenge == "dns-01" {
		t.ACME.DnsAPIProvider = DefaultACMEDNSProvider
	}
	t.ACME.DnsAPIToken = strings.TrimSpace(t.ACME.DnsAPIToken)
	if t.ACME.HTTPAddr == "" {
		t.ACME.HTTPAddr = DefaultACMEHTTPAddr
	}
	// 兼容旧键名 acme_domains：并进 acme.domains，并对重复项去重。
	t.ACME.Domains = normalizeList(append(t.ACME.Domains, t.DomainsLegacy...))
	t.ACME.IPs = normalizeList(t.ACME.IPs)
	t.DomainsLegacy = nil

	// 6.1：配了 public_origin / admin_origin 时，若没显式写 host 白名单，
	// 就按 origin 推导——否则"分离为不同源"要靠两处配置保持一致，很容易漏。
	if len(c.PublicHosts) == 0 && c.PublicOrigin != "" {
		if h := hostOfOrigin(c.PublicOrigin); h != "" {
			c.PublicHosts = []string{h}
		}
	}
	if len(c.AdminHosts) == 0 && c.AdminOrigin != "" {
		if h := hostOfOrigin(c.AdminOrigin); h != "" {
			c.AdminHosts = []string{h}
		}
	}
	c.PublicHosts = normalizeHosts(c.PublicHosts)
	c.AdminHosts = normalizeHosts(c.AdminHosts)
}

func (c *MasterConfig) validate() error {
	switch c.TLS.Mode {
	case "none", "files", "selfsigned", "acme":
	default:
		return fmt.Errorf("tls.mode 取值非法：%s（可选 none/files/selfsigned/acme）", c.TLS.Mode)
	}
	rawMinVer := strings.ToLower(strings.TrimSpace(c.TLS.MinVer))
	if rawMinVer != "" {
		if _, ok := minTLSVersions[rawMinVer]; !ok {
			return fmt.Errorf("tls.min_version 取值非法：%s（可选 1.2/1.3）", c.TLS.MinVer)
		}
	}
	if c.TLS.HSTS.MaxAge < 0 || c.TLS.HSTS.MaxAge > 63072000 {
		return fmt.Errorf("tls.hsts.max_age 应在 0~63072000 秒之间（当前 %d）", c.TLS.HSTS.MaxAge)
	}
	if c.TLS.HSTS.Preload && !c.TLS.HSTS.IncludeSubDomains {
		return fmt.Errorf("tls.hsts.preload 要求同时开启 include_subdomains（preload 列表的硬性条件）")
	}
	if c.TLS.HSTS.Preload && c.TLS.HSTS.MaxAge < 31536000 {
		return fmt.Errorf("tls.hsts.preload 要求 max_age >= 31536000（preload 列表的硬性条件）")
	}

	switch c.TLS.Mode {
	case "files":
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			return fmt.Errorf("tls.mode=files 时必须配置 cert_file 与 key_file")
		}
	case "selfsigned":
		if c.TLS.SelfSign.ValidDays < 1 || c.TLS.SelfSign.ValidDays > 3650 {
			return fmt.Errorf("tls.selfsigned.valid_days 应在 1~3650 之间（当前 %d）", c.TLS.SelfSign.ValidDays)
		}
		for _, h := range c.TLS.SelfSign.Hosts {
			if err := CheckHostEntry(h); err != nil {
				return fmt.Errorf("tls.selfsigned.hosts 含非法条目 %q: %w", h, err)
			}
		}
	case "acme":
		if len(c.TLS.ACME.Domains) == 0 && len(c.TLS.ACME.IPs) == 0 {
			return fmt.Errorf("tls.mode=acme 时必须至少配置一个 acme.domains 或 acme.ips")
		}
		switch c.TLS.ACME.Challenge {
		case "http-01", "tls-alpn-01", "dns-01":
		default:
			return fmt.Errorf("tls.acme.challenge 取值非法：%s（可选 http-01/tls-alpn-01/dns-01）", c.TLS.ACME.Challenge)
		}
		for _, d := range c.TLS.ACME.Domains {
			if err := checkDNSName(d); err != nil {
				return fmt.Errorf("tls.acme.domains 含非法域名 %q: %w", d, err)
			}
		}
		for _, ip := range c.TLS.ACME.IPs {
			if net.ParseIP(ip) == nil {
				return fmt.Errorf("tls.acme.ips 含非法 IP：%q", ip)
			}
		}
		// dns-01 三条专属规则。它们的共同点是"配置看起来没问题，
		// 但要等到申请证书时才失败，而那时的错误信息离原因很远"。
		if c.TLS.ACME.Challenge == "dns-01" {
			if len(c.TLS.ACME.IPs) > 0 {
				return fmt.Errorf("tls.acme.challenge=dns-01 不支持 acme.ips（DNS 里没有 IP 的挑战记录可写）：" +
					"请把 IP 拆到另一个部署，或改用 http-01/tls-alpn-01")
			}
			switch c.TLS.ACME.DnsAPIProvider {
			case "cloudflare":
			default:
				return fmt.Errorf("tls.acme.dns_provider 取值非法：%s（当前仅支持 cloudflare）", c.TLS.ACME.DnsAPIProvider)
			}
			if c.TLS.ACME.DnsAPIToken == "" {
				return fmt.Errorf("tls.acme.challenge=dns-01 时必须配置 dns_api_token" +
					"（Cloudflare 的 API Token，需具备 Zone:DNS:Edit 与 Zone:Zone:Read）")
			}
			// dns-01 不碰本机端口，因此 http_addr 在这条路径上毫无意义。
			return nil
		}
		if c.TLS.ACME.HTTPAddr == "" {
			return fmt.Errorf("tls.mode=acme 时 acme.http_addr 不能为空（http-01 需要明文监听；" +
				"若 80 端口已由反代占用、且反代会把 /.well-known/ 转发进来，写 off 关闭独立监听）")
		}
		if !c.ACMEHTTPOff() {
			if _, _, err := net.SplitHostPort(c.TLS.ACME.HTTPAddr); err != nil {
				return fmt.Errorf("tls.acme.http_addr 应为 地址:端口（如 \":80\" 或 \"0.0.0.0:80\"），当前 %q", c.TLS.ACME.HTTPAddr)
			}
		}
	}

	for _, h := range append(append([]string{}, c.PublicHosts...), c.AdminHosts...) {
		if err := CheckHostWhitelistEntry(h); err != nil {
			// 报错必须点名是哪个键、哪一条、错在哪三件事：
			// 配置文件里往往同时有 public_hosts 与 admin_hosts，
			// 只说"某处非法"等于让人从头再读一遍配置。
			return fmt.Errorf("host 白名单条目应为 主机名[:端口]，不含协议与空格：%q（%v）", h, err)
		}
	}
	return nil
}

// minTLSVersions 是允许的 min_version 写法 → 规范化取值。
// 用白名单而不是"猜前缀"：写错一个值就静默退化成 1.2 是最糟的行为
// （你以为加固到了 1.3，实际还是 1.2，而且没有任何提示）。
var minTLSVersions = map[string]string{
	"1.2": "1.2", "12": "1.2", "tls1.2": "1.2", "v1.2": "1.2", "tlsv1.2": "1.2",
	"1.3": "1.3", "13": "1.3", "tls1.3": "1.3", "v1.3": "1.3", "tlsv1.3": "1.3",
}

// MinTLSVersion 返回规范化后的最低 TLS 版本（"1.2" / "1.3"）。
// 未配置或写法无法识别时返回 1.2；无法识别的写法在 validate 阶段已经报错。
func (c *MasterConfig) MinTLSVersion() string {
	if v, ok := minTLSVersions[strings.ToLower(strings.TrimSpace(c.TLS.MinVer))]; ok {
		return v
	}
	return "1.2"
}

// TLSActive 表示本进程是否自己终止 TLS（决定会话 Cookie 能否带 Secure）。
func (c *MasterConfig) TLSActive() bool {
	return c.TLS.Mode == "files" || c.TLS.Mode == "selfsigned" || c.TLS.Mode == "acme"
}

// HSTSEnabled 返回 HSTS 开关的最终取值（未配置时为 true）。
func (c *MasterConfig) HSTSEnabled() bool {
	return c.TLS.HSTS.Enabled == nil || *c.TLS.HSTS.Enabled
}

// ACMEDomains 返回合并去重后的域名清单（含旧键 acme_domains 的内容）。
func (c *MasterConfig) ACMEDomains() []string { return c.TLS.ACME.Domains }

// ACMEIPs 返回 IP 证书的 IP 清单。
func (c *MasterConfig) ACMEIPs() []string { return c.TLS.ACME.IPs }

// ACMEHTTPOff 表示不使用独立的明文监听来跑 http-01。
//
// 适用场景：80 端口已由前置反代占用，且反代已把
// /.well-known/acme-challenge/ 转发给本进程——此时再绑一次 :80 只会
// 因端口冲突而启动失败。写成 off 即可跳过独立监听。
func (c *MasterConfig) ACMEHTTPOff() bool {
	v := strings.ToLower(strings.TrimSpace(c.TLS.ACME.HTTPAddr))
	return v == "off" || v == "none" || v == "-"
}

// hostOfOrigin 从 "https://status.example.com[:8443]" 里取出 host[:port]。
// 解析失败返回空串（不报错：origin 只是提示与推导来源，不是硬约束）。
func hostOfOrigin(origin string) string {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

// normalizeHosts 统一小写并剔除空项与重复项，保证"同一台机器"的一致比较。
func normalizeHosts(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, h := range in {
		h = strings.ToLower(strings.TrimSpace(h))
		h = strings.TrimSuffix(h, ".")
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeList(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CheckHostEntry 校验 hosts 条目是合法 IP 或域名（**不含端口**）。
//
// 用于 tls.selfsigned.hosts：证书的 SAN 里没有"端口"这个概念，
// 写 host:8443 只会得到一个无效条目。
func CheckHostEntry(h string) error {
	if net.ParseIP(h) != nil {
		return nil
	}
	return checkDNSName(h)
}

// CheckHostWhitelistEntry 校验 host 白名单条目：主机名或 IP，**可带端口**。
//
// 与 CheckHostEntry 是两码事，两者混用会造出很难解释的差异：
// 白名单条目常常必须写 :8443 才能与默认端口区分开（:8443 与 :443 是两个不同的源），
// 而证书 SAN 里带端口又是非法的。所以这里不能复用 CheckHostEntry，
// 但**域名标签那一层规则必须复用**——那才是真正会漂移的部分，
// 漂移的表现是"配置文件里能写的域名，后台设置里存不进去"。
func CheckHostWhitelistEntry(entry string) error {
	if strings.Contains(entry, "://") {
		return fmt.Errorf("不应包含协议前缀")
	}
	if strings.ContainsAny(entry, "/\\?#") {
		return fmt.Errorf("不应包含路径或查询串")
	}
	if strings.ContainsAny(entry, " \t\r\n") {
		return fmt.Errorf("不应包含空白")
	}
	host := entry
	if h, port, err := net.SplitHostPort(entry); err == nil {
		if port == "" {
			return fmt.Errorf("冒号后面缺少端口")
		}
		for _, r := range port {
			if r < '0' || r > '9' {
				return fmt.Errorf("端口应为数字")
			}
		}
		host = h
	} else if strings.Contains(entry, ":") {
		// 冒号既不是"主机:端口"的分隔符，整条也不是合法的 IPv6 字面量。
		// 注意裸 IPv6 必须带方括号（[::1]:8443）——否则 "::1" 与 "host:port"
		// 在语法上根本无法区分。
		if net.ParseIP(entry) == nil {
			return fmt.Errorf("含无法解析的端口；IPv6 请写成 [地址]:端口")
		}
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if host == "" {
		return fmt.Errorf("为空")
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	return checkHostLabels(host)
}

// checkDNSName 做一个"够用的"域名校验：拦掉协议、路径、空格与明显错误的标签。
// 不做完整 RFC 校验（那种校验通常过严，反而挡掉合法的内网名）。
func checkDNSName(name string) error {
	if strings.Contains(name, "://") {
		return fmt.Errorf("不应包含协议前缀")
	}
	if strings.ContainsAny(name, "/ \t") {
		return fmt.Errorf("不应包含路径或空白")
	}
	if len(name) > 253 {
		return fmt.Errorf("长度超过 253")
	}
	trimmed := strings.TrimPrefix(name, "*.")
	if trimmed == "" {
		return fmt.Errorf("为空")
	}
	return checkHostLabels(trimmed)
}

// checkHostLabels 校验域名的标签序列（长度、空标签、字符合法性）。
//
// 抽出来是因为"域名长什么样"这一层规则有两个调用方：证书 SAN（CheckHostEntry）
// 和 host 白名单（CheckHostWhitelistEntry）。它们的**外围**规则不同（SAN 不许
// 带端口、白名单必须能带），但标签这一层必须完全一致——不一致的表现就是
// "同一个域名在配置文件里能写、在后台设置里存不进去"。
func checkHostLabels(trimmed string) error {
	if len(trimmed) > 253 {
		return fmt.Errorf("长度超过 253")
	}
	for _, label := range strings.Split(trimmed, ".") {
		if label == "" {
			return fmt.Errorf("存在空标签（连续的点）")
		}
		if len(label) > 63 {
			return fmt.Errorf("标签 %q 超过 63 字符", label)
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			default:
				return fmt.Errorf("标签 %q 含非法字符 %q", label, string(r))
			}
		}
	}
	return nil
}
