package agentconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/koyoca/xb-probe/internal/model"
)

// Agent 本地配置的测试：默认值、落盘、远端配置的覆盖边界。
//
// 这里的每一条都对应一个"看起来正常、实际会坏"的写法：
// 默认值漏了 → ticker panic 或网卡统计虚高；覆盖边界松了 → 远端能改根信任；
// 落盘不原子 → 断电后配置文件半截，服务起不来。

// TestLoadAppliesDefaults 钉住三个数值默认值与它们的边界（3.1 / 13.4.6 / 13.4.8）。
func TestLoadAppliesDefaults(t *testing.T) {
	cases := []struct {
		name                  string
		report, probe, buffer string
		wantReport            int
		wantProbe             int
		wantBuffer            int
	}{
		{"未填走默认", "", "", "", 3, 30, 100},
		{"上报间隔下界外", "report_interval_s: 1", "", "", 3, 30, 100},
		{"上报间隔上界外", "report_interval_s: 61", "", "", 3, 30, 100},
		{"上报间隔合法值保留", "report_interval_s: 5", "", "", 5, 30, 100},
		{"探测间隔低于 10 秒被抬到 30", "", "probe_interval_s: 5", "", 3, 30, 100},
		{"探测间隔正好 10 秒保留", "", "probe_interval_s: 10", "", 3, 10, 100},
		{"探测间隔 45 秒保留", "", "probe_interval_s: 45", "", 3, 45, 100},
		{"缓冲为 0 走默认", "", "", "buffer_samples: 0", 3, 30, 100},
		{"缓冲为负走默认", "", "", "buffer_samples: -3", 3, 30, 100},
		{"缓冲超上限走默认", "", "", "buffer_samples: 1001", 3, 30, 100},
		{"缓冲合法值保留", "", "", "buffer_samples: 200", 3, 30, 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			yaml := strings.Join([]string{c.report, c.probe, c.buffer}, "\n")
			cfg := loadYAML(t, yaml)
			if cfg.ReportIntervalS != c.wantReport {
				t.Errorf("ReportIntervalS = %d，期望 %d", cfg.ReportIntervalS, c.wantReport)
			}
			if cfg.ProbeIntervalS != c.wantProbe {
				t.Errorf("ProbeIntervalS = %d，期望 %d", cfg.ProbeIntervalS, c.wantProbe)
			}
			if cfg.BufferSamples != c.wantBuffer {
				t.Errorf("BufferSamples = %d，期望 %d", cfg.BufferSamples, c.wantBuffer)
			}
		})
	}
}

// TestLoadNICDefaults 断言网卡默认排除项只在"两张清单都没配"时注入。
//
// 默认值里的 lo/docker*/veth* 是流量统计的正确性前提：容器网卡与回环也被计入的话，
// 同一份流量会被算两遍（宿主机看到的容器流量 + 容器自己的），数字直接翻倍。
// 而一旦管理员显式配了 include，就说明他要的是白名单语义——此时再塞一份 exclude
// 会让"我只想统计 eth0"变成"统计 eth0 且排除 lo/…"，语义被悄悄改掉。
func TestLoadNICDefaults(t *testing.T) {
	t.Run("都没配时注入默认排除项", func(t *testing.T) {
		cfg := loadYAML(t, "")
		want := []string{"lo", "docker*", "veth*", "br-*", "virbr*", "tun*", "tap*", "wg*"}
		if len(cfg.NICExclude) != len(want) {
			t.Fatalf("默认排除项 = %v，期望 %v", cfg.NICExclude, want)
		}
		for i := range want {
			if cfg.NICExclude[i] != want[i] {
				t.Fatalf("默认排除项 = %v，期望 %v", cfg.NICExclude, want)
			}
		}
		if cfg.NICInclude != nil {
			t.Errorf("include 不该被注入默认值：%v", cfg.NICInclude)
		}
	})

	t.Run("配了 include 时不注入 exclude", func(t *testing.T) {
		cfg := loadYAML(t, "nic_include:\n  - eth0")
		if len(cfg.NICInclude) != 1 || cfg.NICInclude[0] != "eth0" {
			t.Fatalf("NICInclude = %v", cfg.NICInclude)
		}
		if len(cfg.NICExclude) != 0 {
			t.Errorf("配了白名单时不该再注入排除项：%v", cfg.NICExclude)
		}
	})

	t.Run("显式配了 exclude 时原样保留", func(t *testing.T) {
		cfg := loadYAML(t, "nic_exclude:\n  - lo\n  - my0*")
		if len(cfg.NICExclude) != 2 || cfg.NICExclude[1] != "my0*" {
			t.Errorf("NICExclude = %v", cfg.NICExclude)
		}
	})
}

// TestLoadReportsErrors 断言读不到/解析不了时必须报错，而不是给一份空配置。
//
// 给空配置是最坏的选择：Agent 会用空 master_url 去连，报出来的错
// 会是"不支持的协议："这种与真实原因（文件不存在）毫无关系的错误。
func TestLoadReportsErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "不存在.yaml")); err == nil {
		t.Error("文件不存在应报错")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("master_url: [未闭合\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if _, err := Load(bad); err == nil {
		t.Error("YAML 语法错误应报错")
	} else if !strings.Contains(err.Error(), "解析配置失败") {
		t.Errorf("错误信息应指明是解析失败，得到 %v", err)
	}
}

// TestSaveRoundTripsAndLeavesNoTmp 断言落盘是"先写临时文件再改名"的原子替换。
//
// 直接覆写原文件的话，进程在写一半时被杀（断电、OOM、systemd 强停）会留下
// 半截 YAML：下次启动解析失败，Agent 起不来，而现场只剩一个坏文件。
// 临时文件还必须在成功后消失，否则会一直被当成"待处理残留"。
func TestSaveRoundTripsAndLeavesNoTmp(t *testing.T) {
	dir := t.TempDir()
	// 多套一层不存在的目录：Save 要负责建出来（安装脚本不一定预先建过）。
	path := filepath.Join(dir, "sub", "agent.yaml")
	want := &Config{
		MasterURL:       "https://master.example.com",
		Token:           "pk_abc",
		SPKISHA256:      []string{"AAAA"},
		ReportIntervalS: 5,
		ProbeIntervalS:  30,
		Targets:         []model.Target{{ID: 4, Host: "1.1.1.1", Port: 443, Method: "tcp"}},
	}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got.Token != "pk_abc" || got.MasterURL != want.MasterURL {
		t.Errorf("往返后内容不一致：%+v", got)
	}
	if len(got.Targets) != 1 || got.Targets[0].ID != 4 {
		t.Errorf("目标清单未落盘：%+v", got.Targets)
	}
	if got.ReportIntervalS != 5 {
		t.Errorf("间隔未落盘：%d", got.ReportIntervalS)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("保存成功后仍留下临时文件 %s", e.Name())
		}
	}

	// 覆盖写：第二次保存必须生效（否则"换令牌"永远不会落盘）。
	want.Token = "pk_new"
	if err := Save(path, want); err != nil {
		t.Fatalf("二次 Save 失败: %v", err)
	}
	if again, _ := Load(path); again.Token != "pk_new" {
		t.Errorf("覆盖写未生效：token=%q", again.Token)
	}
}

// TestApplyServerConfigOverwritesOnlyWhitelistedFields 钉住覆盖边界。
func TestApplyServerConfigOverwritesOnlyWhitelistedFields(t *testing.T) {
	local := &Config{
		MasterURL:           "https://local.example.com",
		Token:               "pk_local",
		SPKISHA256:          []string{"LOCALPIN"},
		InsecureHTTP:        true,
		AllowPrivateTargets: true,
		ServiceManager:      "systemd",
		DiskPaths:           []string{"/data"},
		ConfigVersion:       "old",
		PublicIPURL:         "https://ip.example.invalid/",
	}
	remote := model.AgentConfig{
		ConfigVersion:   "new",
		ReportIntervalS: 7,
		ProbeIntervalS:  45,
		NICInclude:      []string{"eth0"},
		NICExclude:      []string{"lo"},
		Targets:         []model.Target{{ID: 1, Host: "1.1.1.1", Port: 443, Method: "tcp"}},
	}
	local.ApplyServerConfig(remote)

	// 应当被覆盖的。
	if local.ConfigVersion != "new" || local.ReportIntervalS != 7 || local.ProbeIntervalS != 45 {
		t.Errorf("版本/间隔未生效：%+v", local)
	}
	if len(local.Targets) != 1 || local.Targets[0].ID != 1 {
		t.Errorf("目标清单未生效：%+v", local.Targets)
	}
	if len(local.NICInclude) != 1 || len(local.NICExclude) != 1 {
		t.Errorf("网卡清单未生效：%v / %v", local.NICInclude, local.NICExclude)
	}

	// 不该被远端改动的（根信任与 S4 开关）。
	if local.MasterURL != "https://local.example.com" {
		t.Errorf("master_url 被远端改成了 %q（等于允许后台把节点指向别处）", local.MasterURL)
	}
	if local.Token != "pk_local" {
		t.Errorf("令牌被远端改动：%q", local.Token)
	}
	if len(local.SPKISHA256) != 1 || local.SPKISHA256[0] != "LOCALPIN" {
		t.Errorf("证书指纹被远端改动：%v", local.SPKISHA256)
	}
	if !local.InsecureHTTP {
		t.Error("insecure_http 被远端改动")
	}
	if !local.AllowPrivateTargets {
		t.Error("allow_private_targets 被远端改动（S4 开关只应由本地配置决定）")
	}
	if local.ServiceManager != "systemd" {
		t.Errorf("service_manager 被远端改动：%q", local.ServiceManager)
	}
	if len(local.DiskPaths) != 1 {
		t.Errorf("磁盘路径被远端改动：%v", local.DiskPaths)
	}
	// public_ip_url 同样只由本地决定：允许远端改它，等于后台一点保存就让
	// 所有节点开始往第三方地址发请求（这是我们不想替用户做的决定）。
	if local.PublicIPURL != "https://ip.example.invalid/" {
		t.Errorf("public_ip_url 被远端改动：%q", local.PublicIPURL)
	}
}

// TestPublicIPIntervalDefaultsAndClamp 钉住公网地址探测间隔的三个边界。
//
// 下限不是洁癖：这个探测打的是**外部**服务，把它设成几秒等于拿别人的公共服务
// 当自己的探针。上界同理——设成几天不如关掉，而"关掉"有明确的表达方式（留空 URL）。
func TestPublicIPIntervalDefaultsAndClamp(t *testing.T) {
	if got := (&Config{}).PublicIPInterval(); got.Seconds() != float64(PublicIPDefaultIntervalS) {
		t.Errorf("未配置时间的间隔 = %v，期望默认 %d 秒", got, PublicIPDefaultIntervalS)
	}

	low := &Config{PublicIPIntervalS: 5}
	low.applyDefaults()
	if low.PublicIPIntervalS != PublicIPMinIntervalS {
		t.Errorf("过小的间隔 = %d，期望被抬到下限 %d", low.PublicIPIntervalS, PublicIPMinIntervalS)
	}
	if low.PublicIPInterval().Seconds() != float64(PublicIPMinIntervalS) {
		t.Errorf("抬升后的 PublicIPInterval() = %v", low.PublicIPInterval())
	}

	high := &Config{PublicIPIntervalS: 30 * 24 * 3600}
	high.applyDefaults()
	if high.PublicIPIntervalS != PublicIPMaxIntervalS {
		t.Errorf("过大的间隔 = %d，期望被压到上限 %d", high.PublicIPIntervalS, PublicIPMaxIntervalS)
	}

	ok := &Config{PublicIPIntervalS: 300}
	ok.applyDefaults()
	if ok.PublicIPIntervalS != 300 {
		t.Errorf("合法间隔被改动了：%d", ok.PublicIPIntervalS)
	}
}

// TestApplyServerConfigKeepsLocalNICDefaultsWhenRemoteOmits 断言"远端没配"不等于"清空"。
//
// 这是三态契约（不变量 8 的同源教训）：Master 未下发 nic_exclude 时序列化出来是
// null，Agent 必须保持自己的默认排除项。旧实现无条件覆盖，于是 Agent 一启动、
// 第一次拉到配置，lo/docker*/veth* 就被抹成空——流量统计从此把容器网卡和回环
// 一起算进去，数字凭空虚高，而且**没有任何日志或界面提示**。
func TestApplyServerConfigKeepsLocalNICDefaultsWhenRemoteOmits(t *testing.T) {
	local := loadYAML(t, "") // 未配网卡 → 拿到内置默认排除项
	if len(local.NICExclude) == 0 {
		t.Fatal("前置条件不成立：本地应有默认排除项")
	}
	before := len(local.NICExclude)

	local.ApplyServerConfig(model.AgentConfig{
		ConfigVersion:   "v1",
		ReportIntervalS: 3,
		ProbeIntervalS:  30,
		NICInclude:      nil, // Master 未下发
		NICExclude:      nil, // Master 未下发
	})

	if len(local.NICExclude) != before {
		t.Errorf("未下发网卡清单时本地默认被清掉了：%d → %v", len(local.NICExclude), local.NICExclude)
	}
	if len(local.NICInclude) != 0 {
		t.Errorf("未下发时不该凭空产生白名单：%v", local.NICInclude)
	}
}

// TestRedactedMasksTokenWithoutMutatingOriginal 断言脱敏只作用在副本上。
//
// 若实现里改的是接收者本身（`c.Token = "pk_***"`），调用方随后保存配置就会把
// 真令牌写成星号——一次"为了打日志"的调用直接毁掉节点身份，且要到重启才发现。
//
// 注：该函数目前**没有任何生产调用点**（日志路径本来就不打印令牌）。
// 留着它是因为将来若需要输出配置，这里已经有一个正确做法可循；这条测试
// 保证它至少是被验证过的，而不是一段看着像保护、实则没人用过的代码。
func TestRedactedMasksTokenWithoutMutatingOriginal(t *testing.T) {
	c := &Config{MasterURL: "https://m.example.com", Token: "pk_secret", ReportIntervalS: 5}
	red := c.Redacted()

	if red.Token == "pk_secret" {
		t.Error("脱敏后仍是明文令牌")
	}
	if red.Token == "" {
		t.Error("脱敏不该把令牌清成空串（那会让日志无法区分'有令牌'与'没令牌'）")
	}
	if c.Token != "pk_secret" {
		t.Fatalf("原配置被改动了：token=%q（调用方随后保存就会写坏配置）", c.Token)
	}
	if red.MasterURL != c.MasterURL || red.ReportIntervalS != c.ReportIntervalS {
		t.Error("脱敏不该改动其他字段")
	}

	// 无令牌时保持空（不要凭空出现 "pk_***"）。
	empty := &Config{}
	if got := empty.Redacted(); got.Token != "" {
		t.Errorf("空令牌脱敏后应仍为空，得到 %q", got.Token)
	}
}

// TestDefaultPathsPerPlatform 断言平台默认路径的形状。
//
// 这些路径会被安装脚本与卸载逻辑直接使用，写错的表现是"装到别处去了"
// 或"卸载时删不掉"。
func TestDefaultPathsPerPlatform(t *testing.T) {
	cfgPath := DefaultPath()
	binDir := DefaultBinaryDir()
	if !filepath.IsAbs(cfgPath) {
		t.Errorf("配置路径应是绝对路径：%q", cfgPath)
	}
	if !strings.HasSuffix(filepath.ToSlash(cfgPath), "/xb-probe-agent/agent.yaml") {
		t.Errorf("配置路径应以 xb-probe-agent/agent.yaml 结尾：%q", cfgPath)
	}
	if !filepath.IsAbs(binDir) {
		t.Errorf("安装目录应是绝对路径：%q", binDir)
	}
	if !strings.Contains(filepath.ToSlash(binDir), "xb-probe-agent") && binDir != "/usr/local/bin" {
		t.Errorf("安装目录不对：%q", binDir)
	}

	switch runtime.GOOS {
	case "windows":
		// Windows 上必须以 ProgramData 为根：装到用户目录下的话，
		// 换成另一个账号跑服务就找不到配置了。
		t.Setenv("ProgramData", `D:\ProgramDataX`)
		if got := DefaultPath(); got != filepath.Join(`D:\ProgramDataX`, "xb-probe-agent", "agent.yaml") {
			t.Errorf("未遵循 ProgramData：%q", got)
		}
	case "freebsd":
		if cfgPath != "/usr/local/etc/xb-probe-agent/agent.yaml" {
			t.Errorf("FreeBSD 配置路径 = %q", cfgPath)
		}
	default:
		if cfgPath != "/etc/xb-probe-agent/agent.yaml" {
			t.Errorf("Linux 配置路径 = %q", cfgPath)
		}
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// loadYAML 把一段 YAML 写进临时文件并 Load（顺带绕过 applyDefaults 的可见性）。
func loadYAML(t *testing.T, yaml string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	return cfg
}
