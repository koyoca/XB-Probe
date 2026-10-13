package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// writeYAML 把一段配置写进临时目录并加载。
func writeYAML(t *testing.T, body string) (*MasterConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	return LoadMaster(path)
}

func mustLoad(t *testing.T, body string) *MasterConfig {
	t.Helper()
	c, err := writeYAML(t, body)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	return c
}

func TestTLSDefaults(t *testing.T) {
	c := mustLoad(t, `
data_dir: "./data"
tls:
  mode: none
`)
	if c.TLS.Mode != "none" {
		t.Fatalf("mode 默认应为 none，得到 %q", c.TLS.Mode)
	}
	// 缓存目录必须落在 data_dir 下——账户私钥不该散落在工作目录里。
	want := filepath.Join("./data", DefaultACMECacheDir)
	if c.TLS.CacheDir != want {
		t.Fatalf("cache_dir 默认应为 %q，得到 %q", want, c.TLS.CacheDir)
	}
	if c.MinTLSVersion() != "1.2" {
		t.Fatalf("min_version 默认应为 1.2，得到 %q", c.MinTLSVersion())
	}
	if !c.HSTSEnabled() {
		t.Fatal("HSTS 默认应开启")
	}
	if c.TLS.HSTS.MaxAge != DefaultHSTSMaxAge {
		t.Fatalf("hsts.max_age 默认应为 %d，得到 %d", DefaultHSTSMaxAge, c.TLS.HSTS.MaxAge)
	}
	if c.TLS.HSTS.IncludeSubDomains {
		t.Fatal("include_subdomains 默认应为 false（开启会影响同主域下所有子域）")
	}
	if c.TLS.ACME.Challenge != DefaultACMEChallenge {
		t.Fatalf("acme.challenge 默认应为 %s，得到 %q", DefaultACMEChallenge, c.TLS.ACME.Challenge)
	}
	if c.TLS.ACME.HTTPAddr != DefaultACMEHTTPAddr {
		t.Fatalf("acme.http_addr 默认应为 %s，得到 %q", DefaultACMEHTTPAddr, c.TLS.ACME.HTTPAddr)
	}
	if len(c.TLS.SelfSign.Hosts) != 3 {
		t.Fatalf("selfsigned.hosts 默认应为 localhost/127.0.0.1/::1，得到 %v", c.TLS.SelfSign.Hosts)
	}
	if c.TLS.SelfSign.ValidDays != DefaultSelfSignedDay {
		t.Fatalf("selfsigned.valid_days 默认应为 %d，得到 %d", DefaultSelfSignedDay, c.TLS.SelfSign.ValidDays)
	}
	if c.TLSActive() {
		t.Fatal("mode=none 时 TLSActive 应为 false")
	}
}

func TestTLSModeValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "未知模式",
			body:    "tls:\n  mode: letsencrypt\n",
			wantErr: "tls.mode 取值非法",
		},
		{
			name:    "files 缺文件",
			body:    "tls:\n  mode: files\n  cert_file: /tmp/a.pem\n",
			wantErr: "必须配置 cert_file 与 key_file",
		},
		{
			name: "files 正常",
			body: "tls:\n  mode: files\n  cert_file: /tmp/a.pem\n  key_file: /tmp/a.key\n",
		},
		{
			name:    "acme 未指定标识符",
			body:    "tls:\n  mode: acme\n",
			wantErr: "至少配置一个 acme.domains 或 acme.ips",
		},
		{
			name:    "acme 挑战类型非法",
			body:    "tls:\n  mode: acme\n  acme:\n    domains: [a.example.com]\n    challenge: dns-01\n",
			wantErr: "tls.acme.challenge 取值非法",
		},
		{
			name:    "acme 域名非法",
			body:    "tls:\n  mode: acme\n  acme:\n    domains: [\"https://a.example.com\"]\n",
			wantErr: "tls.acme.domains 含非法域名",
		},
		{
			name:    "acme IP 非法",
			body:    "tls:\n  mode: acme\n  acme:\n    ips: [\"999.1.1.1\"]\n",
			wantErr: "tls.acme.ips 含非法 IP",
		},
		{
			name: "acme 只用 IP 也可以（IP 证书）",
			body: "tls:\n  mode: acme\n  acme:\n    ips: [\"203.0.113.10\"]\n",
		},
		{
			name: "acme 明文监听可写 off（80 端口归反代时）",
			body: "tls:\n  mode: acme\n  acme:\n    domains: [\"a.example.com\"]\n    http_addr: \"off\"\n",
		},
		{
			name:    "acme 明文监听地址非法",
			body:    "tls:\n  mode: acme\n  acme:\n    domains: [\"a.example.com\"]\n    http_addr: \"80\"\n",
			wantErr: "tls.acme.http_addr 应为 地址:端口",
		},
		{
			name: "acme 通配符域名允许",
			body: "tls:\n  mode: acme\n  acme:\n    domains: [\"*.example.com\"]\n",
		},
		{
			name:    "selfsigned 主机条目非法",
			body:    "tls:\n  mode: selfsigned\n  selfsigned:\n    hosts: [\"a b\"]\n",
			wantErr: "tls.selfsigned.hosts 含非法条目",
		},
		{
			name:    "selfsigned 有效期越界",
			body:    "tls:\n  mode: selfsigned\n  selfsigned:\n    valid_days: 9999\n",
			wantErr: "valid_days 应在 1~3650 之间",
		},
		{
			name: "selfsigned 混合 IP 与域名",
			body: "tls:\n  mode: selfsigned\n  selfsigned:\n    hosts: [\"10.0.0.5\", \"probe.lan\", \"::1\"]\n",
		},
		{
			name:    "min_version 非法",
			body:    "tls:\n  mode: none\n  min_version: \"1.0\"\n",
			wantErr: "tls.min_version 取值非法",
		},
		{
			name:    "min_version 拼错也要报错（不能静默退化成 1.2）",
			body:    "tls:\n  mode: none\n  min_version: \"1.4\"\n",
			wantErr: "tls.min_version 取值非法",
		},
		{
			name:    "host 白名单不该带协议",
			body:    "public_hosts: [\"https://status.example.com\"]\n",
			wantErr: "host 白名单条目应为 主机名[:端口]",
		},
		{
			name:    "hsts preload 需要 include_subdomains",
			body:    "tls:\n  mode: none\n  hsts:\n    preload: true\n",
			wantErr: "preload 要求同时开启 include_subdomains",
		},
		{
			name:    "hsts preload 需要足够长的 max_age",
			body:    "tls:\n  mode: none\n  hsts:\n    preload: true\n    include_subdomains: true\n    max_age: 600\n",
			wantErr: "max_age >= 31536000",
		},
		{
			name: "hsts preload 合规",
			body: "tls:\n  mode: none\n  hsts:\n    preload: true\n    include_subdomains: true\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := writeYAML(t, tc.body)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("本应通过，却报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("本应报错（%s），却通过了", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息应包含 %q，实际为 %q", tc.wantErr, err.Error())
			}
		})
	}
}

// TestACMEDomainsLegacyKeyMerged：旧的顶层 acme_domains 必须并进 acme.domains，
// 否则老配置升级后会静默变成"没有域名"从而启动失败。
func TestACMEDomainsLegacyKeyMerged(t *testing.T) {
	c := mustLoad(t, `
tls:
  mode: acme
  acme_domains: ["status.example.com", "STATUS.example.com"]
  acme:
    domains: ["admin.example.com"]
`)
	got := c.ACMEDomains()
	if len(got) != 2 {
		t.Fatalf("应合并去重为 2 条，得到 %v", got)
	}
	// 顺序：acme.domains 在前，旧键补在后面。
	if got[0] != "admin.example.com" || got[1] != "status.example.com" {
		t.Fatalf("合并结果与预期不符: %v", got)
	}
}

// TestOriginDerivesHostAllowList：配了 origin 就该自动推导 host 白名单（6.1）。
func TestOriginDerivesHostAllowList(t *testing.T) {
	c := mustLoad(t, `
public_origin: "https://Status.Example.com"
admin_origin: "https://admin.example.com:8443"
tls:
  mode: none
`)
	if len(c.PublicHosts) != 1 || c.PublicHosts[0] != "status.example.com" {
		t.Fatalf("public_hosts 应由 public_origin 推导并小写，得到 %v", c.PublicHosts)
	}
	if len(c.AdminHosts) != 1 || c.AdminHosts[0] != "admin.example.com:8443" {
		t.Fatalf("admin_hosts 应保留端口，得到 %v", c.AdminHosts)
	}
}

// TestExplicitHostsWinOverOrigin：显式写了 host 白名单时不要被 origin 覆盖。
func TestExplicitHostsWinOverOrigin(t *testing.T) {
	c := mustLoad(t, `
public_origin: "https://status.example.com"
public_hosts: ["probe.example.com", "PROBE.example.com"]
tls:
  mode: none
`)
	if len(c.PublicHosts) != 1 || c.PublicHosts[0] != "probe.example.com" {
		t.Fatalf("显式 host 白名单应去重后生效，得到 %v", c.PublicHosts)
	}
}

func TestTLSActiveCoversAllTerminatingModes(t *testing.T) {
	for _, mode := range []string{"files", "selfsigned", "acme"} {
		body := "tls:\n  mode: " + mode + "\n"
		switch mode {
		case "files":
			body += "  cert_file: /tmp/a.pem\n  key_file: /tmp/a.key\n"
		case "acme":
			body += "  acme:\n    domains: [\"a.example.com\"]\n"
		}
		c := mustLoad(t, body)
		if !c.TLSActive() {
			t.Fatalf("mode=%s 时 TLSActive 应为 true", mode)
		}
	}
}

func TestMinTLSVersionNormalization(t *testing.T) {
	cases := map[string]string{
		"1.2": "1.2", "1.3": "1.3", "TLS1.3": "1.3", "v1.3": "1.3", "13": "1.3", "": "1.2",
	}
	for in, want := range cases {
		c := mustLoad(t, "tls:\n  mode: none\n  min_version: \""+in+"\"\n")
		if got := c.MinTLSVersion(); got != want {
			t.Fatalf("min_version=%q 应归一化为 %q，得到 %q", in, want, got)
		}
	}
}

func TestHSTSDisabledExplicitly(t *testing.T) {
	c := mustLoad(t, "tls:\n  mode: none\n  hsts:\n    enabled: false\n")
	if c.HSTSEnabled() {
		t.Fatal("显式 enabled: false 时应关闭 HSTS")
	}
}

// TestShippedTemplatesLoad 钉住随仓库分发的三份配置。
//
// 模板是最容易被"改代码忘了改文档"腐蚀的地方：一个注释掉的键名写错、
// 缩进串位，用户在别处复制粘贴后才会炸，而那时已经很难定位。这里直接
// 加载真实文件，任何解析/校验失败都会在 go test 阶段暴露。
func TestShippedTemplatesLoad(t *testing.T) {
	type tmpl struct {
		path string
		// allModes 为真时要求文件把四种取证书方式都写出来。
		allModes bool
	}
	for _, tc := range []tmpl{
		// 发布/部署的权威模板：必须列全四种方式。用户明确要求"申请方式可分多种"，
		// 少写一种就等于那个能力事实上不可发现。
		{"../../configs/master.yaml.example", true},
		// 本机与演示环境的配置：只要可加载 + 安全的默认值即可。
		{"../../master.yaml", false},
		{"../../dist/demo/master.yaml", false},
	} {
		name := strings.ReplaceAll(strings.TrimPrefix(tc.path, "../../"), "../../", "__")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(tc.path)
			if err != nil {
				t.Skipf("模板不存在，跳过: %v", err)
			}
			c, err := LoadMaster(tc.path)
			if err != nil {
				t.Fatalf("模板 %s 无法加载: %v", tc.path, err)
			}
			// 模板必须是"开箱即明文"的安全默认：配了就能直接启动，
			// 不会因为在没有证书的机器上默认要求 TLS 而起不来。
			if c.TLS.Mode != "none" {
				t.Fatalf("模板 %s 默认 mode 应为 none，得到 %q", tc.path, c.TLS.Mode)
			}
			if c.TLSActive() {
				t.Fatalf("模板 %s 默认不应自行终止 TLS", tc.path)
			}
			// 未显式关闭时 HSTS 默认开启，但只在安全连接上下发（见 api.SecurityHeaders）。
			if !c.HSTSEnabled() {
				t.Fatalf("模板 %s 未显式关闭 HSTS，应为启用", tc.path)
			}
			if got := c.MinTLSVersion(); got != "1.2" {
				t.Fatalf("模板 %s 默认 min_version 应为 1.2，得到 %q", tc.path, got)
			}
			if tc.allModes {
				body := string(raw)
				for _, mode := range []string{"none", "files", "selfsigned", "acme"} {
					if !strings.Contains(body, mode) {
						t.Fatalf("模板 %s 未提及 tls.mode=%s，四种取证书方式必须都有样例", tc.path, mode)
					}
				}
			}
		})
	}
}

// 未知键必须让加载失败，而不是被静默丢掉。
//
// 这条守卫来自一次真实的事故：部署脚本往 master.yaml 里生成了一个 `install:` 段，
// 但 install.* 其实是**数据库设置**（MasterConfig 里没有 install 这个键），
// 而非严格的 yaml.Unmarshal 会把它整段吞掉——配置白纸黑字写在文件里，
// 后台却是空的，只有靠人比对才能发现。严格模式把这一类变成"启动即报错"。
func TestLoadMasterRejectsUnknownKey(t *testing.T) {
	_, err := writeYAML(t, `
public_listen: "127.0.0.1:8080"
install:
  mode: "on_demand"
`)
	if err == nil {
		t.Fatal("未知键 install 应导致加载失败，而不是被静默忽略")
	}
	// 断言错误信息里点名了那个键：只说"解析失败"区分不出是键名写错还是
	// YAML 语法坏了，运维对着日志定位不到问题。
	if !strings.Contains(err.Error(), "install") {
		t.Errorf("错误信息应点出未知键 install，实得：%v", err)
	}
}

// 已知键仍然照常工作（防止"严格模式"矫枉过正把合法配置也拦掉）。
func TestLoadMasterAcceptsKnownKeys(t *testing.T) {
	c := mustLoad(t, `
public_listen: "127.0.0.1:8080"
admin_listen:  "127.0.0.1:8081"
data_dir:      "./data"
trusted_proxies: ["127.0.0.1"]
tls:
  mode: none
  min_version: "1.3"
log_level: "debug"
`)
	if c.LogLevel != "debug" {
		t.Fatalf("log_level 应为 debug，得到 %q", c.LogLevel)
	}
	if c.MinTLSVersion() != "1.3" {
		t.Fatalf("min_version 应为 1.3，得到 %q", c.MinTLSVersion())
	}
	if len(c.TrustedProxies) != 1 {
		t.Fatalf("trusted_proxies 应保留 1 条，得到 %v", c.TrustedProxies)
	}
}

// 一键部署脚本生成的 `master.yaml`，只允许出现 MasterConfig 认识的**顶层键**。
//
// 这条护栏守的是一个已经真实发生过的事故：脚本曾经生成 `install:` 段，而
// `install.*` 其实是**数据库设置**（`repo.GetSettingDefault` 读），不是配置文件
// 字段。非严格解析会把整段静默丢掉——配置里白纸黑字，后台却是空的。
// 严格解析虽然能在启动时拦住，但那已经是"装完一台才炸"；在脚本这一层提前拦住，
// 代价小得多，报错也更直白。
//
// 判据取自脚本里"生成字符串"的写法本身（`echo "…"` / `$lines.Add("…")`），
// 只取紧跟引号的第一个 token：顶层键不带缩进，`tls` 段内部的键在字符串里带缩进，
// 靠这个差异把两者分开。
func TestDeployScriptsOnlyWriteKnownConfigKeys(t *testing.T) {
	known := map[string]bool{}
	rt := reflect.TypeOf(MasterConfig{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		known[strings.Split(tag, ",")[0]] = true
	}
	if len(known) == 0 {
		t.Fatal("从 MasterConfig 上没读到任何 yaml 标签，判据本身失效了")
	}

	// 例：`\t\techo "public_listen: \"...\""` / `  [void]$lines.Add("tls:")`
	// 以及单引号形式的 `echo 'tls:'`。字符串里带缩进的（`"  mode: "`）不匹配。
	keyRe := regexp.MustCompile(`(?:echo\s+|lines\.Add\()['"]([a-z_]+):`)

	for _, rel := range []string{
		"../../scripts/deploy-master.sh",
		"../../scripts/deploy-master.ps1",
	} {
		raw, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("读不到部署脚本 %s：%v", rel, err)
		}
		body := string(raw)
		matches := keyRe.FindAllStringSubmatch(body, -1)
		if len(matches) == 0 {
			t.Fatalf("%s 里一个配置键都没抽到——判据与脚本写法脱节了，这条护栏已经形同虚设", rel)
		}
		// 脚本必须至少写上监听地址与 tls 段，否则说明抽取范围跑偏。
		for _, must := range []string{"public_listen", "tls"} {
			if !strings.Contains(body, `"`+must+`:`) && !strings.Contains(body, `'`+must+`:`) {
				t.Errorf("%s 里没找到应当生成的键 %q，判据可能已与脚本写法脱节", rel, must)
			}
		}
		for _, m := range matches {
			if !known[m[1]] {
				t.Errorf("%s 生成了 master.yaml 里不存在的键 %q —— "+
					"它要么是笔误，要么本该是数据库设置（如 install.*），写进配置文件只会静默失效",
					rel, m[1])
			}
		}
	}
}

// host 白名单条目必须允许带端口，且与后台设置项共用同一份判定。
//
// 这里钉的是"能带端口"这一条：:8443 与 :443 是两个不同的源，
// 白名单必须能表达它。早期实现复用了自签证书 SAN 的那套校验（SAN 里不允许
// 端口——证书里根本没有端口这个概念），结果白名单里写不出带端口的条目，
// 而配置文件里明明写着 admin.example.com:8443 的样例。
func TestHostWhitelistEntryAllowsPort(t *testing.T) {
	c := mustLoad(t, `
data_dir: "./data"
public_origin: "https://status.example.com"
admin_origin: "https://admin.example.com:8443"
`)
	if len(c.PublicHosts) != 1 || c.PublicHosts[0] != "status.example.com" {
		t.Fatalf("公开域名推导错误：%v", c.PublicHosts)
	}
	if len(c.AdminHosts) != 1 || c.AdminHosts[0] != "admin.example.com:8443" {
		t.Fatalf("带端口的管理域名应原样保留：%v", c.AdminHosts)
	}

	// 反过来：明显不是"主机名[:端口]"的写法必须被拒，而不是等到运行时
	// 对不上域名、只留下一个 421 让人猜。
	for _, bad := range []string{
		"https://admin.example.com",
		"admin.example.com/admin",
		"admin.example.com:abc",
		"admin.example.com:",
		"a..b",
	} {
		if err := CheckHostWhitelistEntry(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	for _, good := range []string{
		"status.example.com", "admin.example.com:8443", "192.0.2.10",
		"192.0.2.10:8080", "[2001:db8::1]:8443", "probe.lan",
	} {
		if err := CheckHostWhitelistEntry(good); err != nil {
			t.Errorf("%q 应通过，实得 %v", good, err)
		}
	}
}
