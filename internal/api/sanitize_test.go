package api

import (
	"strings"
	"testing"

	"github.com/koyoca/xb-probe/internal/model"
)

// 13.4.4 / S1：节点上报的不可信字符串必须按字段白名单与长度校验。
func TestSanitizeHostname(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"vps-1.example.com", "vps-1.example.com"},
		{"a b\tc", "a_b_c"},
	}
	for _, c := range cases {
		if got := sanitizeHostname(c.in); got != c.want {
			t.Errorf("sanitizeHostname(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}

	// 注入串：只保留白名单字符，其余一律变 _；不得残留任何可注入字符。
	evil := `"><script>alert(1)</script>`
	got := sanitizeHostname(evil)
	if strings.ContainsAny(got, `"<>()/'`) {
		t.Errorf("清洗后仍含可注入字符：%q", got)
	}
	if len(got) != len(evil) {
		t.Errorf("一一映射替换，长度应不变：%q", got)
	}

	long := strings.Repeat("a", 100)
	if got := sanitizeHostname(long); len(got) != 63 {
		t.Errorf("超过 63 字符应被截断，得到长度 %d", len(got))
	}
	if got := sanitizeHostname(""); got != "" {
		t.Errorf("空值应返回空，得到 %q", got)
	}
}

func TestSanitizeTextRemovesAngleAndControl(t *testing.T) {
	got := sanitizeText(`Debian <script>12`+"\n", 64)
	if strings.ContainsAny(got, "<>") {
		t.Errorf("应移除尖括号，得到 %q", got)
	}
	if strings.ContainsAny(got, "\n") {
		t.Errorf("应移除控制字符，得到 %q", got)
	}

	// 长度上限
	if got := sanitizeText(strings.Repeat("x", 200), 64); len(got) != 64 {
		t.Errorf("应截断到 64，得到 %d", len(got))
	}
}

func TestSanitizeAgentVersion(t *testing.T) {
	if got := sanitizeAgentVersion("1.2.3"); got != "1.2.3" {
		t.Errorf("合法语义化版本应原样返回，得到 %q", got)
	}
	if got := sanitizeAgentVersion("1.2.3-rc1"); got != "1.2.3-rc1" {
		t.Errorf("带预发布标识应通过，得到 %q", got)
	}
	for _, bad := range []string{"v1.2", "1.2", "abc", "../../etc/passwd", "1.2.3; rm -rf /"} {
		if got := sanitizeAgentVersion(bad); got != "" {
			t.Errorf("非法版本 %q 应被丢弃，得到 %q", bad, got)
		}
	}
}

func TestSanitizeErrorIsEnum(t *testing.T) {
	for _, ok := range []string{"timeout", "refused", "unreachable", "dns_failed",
		"icmp_not_permitted", "blocked_target", "other", ""} {
		if got := sanitizeError(ok); got != ok {
			t.Errorf("枚举值 %q 应原样返回，得到 %q", ok, got)
		}
	}
	if got := sanitizeError("内部错误 <script>"); got != model.ProbeErrOther {
		t.Errorf("未知错误应归为 other，得到 %q", got)
	}
	if got := sanitizeError("TIMEOUT"); got != "timeout" {
		t.Errorf("应做大小写归一，得到 %q", got)
	}
}

func TestSanitizeNIC(t *testing.T) {
	if got := sanitizeNIC("eth0"); got != "eth0" {
		t.Errorf("合法网卡名应原样返回，得到 %q", got)
	}
	got := sanitizeNIC("docker0; rm -rf /")
	if strings.ContainsAny(got, "; /") {
		t.Errorf("应收敛到 hostname 字符集，得到 %q", got)
	}
	if got := sanitizeNIC(strings.Repeat("n", 40)); len(got) != 32 {
		t.Errorf("应截断到 32，得到 %d", len(got))
	}
}

func TestSanitizeStatic(t *testing.T) {
	s := &model.Static{
		Hostname:     `evil"><script>`,
		OS:           "Debian <b>12</b>",
		AgentVersion: "not-a-version",
		CPUCores:     -5,
		MemTotal:     -1,
	}
	sanitizeStatic(s)
	if strings.ContainsAny(s.Hostname, `"<>`) {
		t.Errorf("hostname 仍有可注入字符：%q", s.Hostname)
	}
	if strings.ContainsAny(s.OS, "<>") {
		t.Errorf("os 仍有尖括号：%q", s.OS)
	}
	if s.AgentVersion != "" {
		t.Errorf("非法版本应为空，得到 %q", s.AgentVersion)
	}
	if s.CPUCores != 0 || s.MemTotal != 0 {
		t.Errorf("越界数值应清零，得到 cores=%d mem=%d", s.CPUCores, s.MemTotal)
	}
}

// 9.2 / 13.4.4：样本数值范围校验，越界丢弃整条样本。
func TestValidateSample(t *testing.T) {
	now := int64(1_760_000_000)
	p := func(v int) *int { return &v }
	p64 := func(v int64) *int64 { return &v }

	good := &model.Sample{TS: now, Metrics: model.Metrics{CPU: p(5000), MemUsed: p64(1024)}}
	if err := validateSample(good, now); err != nil {
		t.Fatalf("合法样本不应报错：%v", err)
	}

	cases := []struct {
		name string
		s    *model.Sample
	}{
		{"CPU 超过 10000", &model.Sample{TS: now, Metrics: model.Metrics{CPU: p(10001)}}},
		{"CPU 为负", &model.Sample{TS: now, Metrics: model.Metrics{CPU: p(-1)}}},
		{"内存为负", &model.Sample{TS: now, Metrics: model.Metrics{MemUsed: p64(-1)}}},
		{"ts 为 0", &model.Sample{TS: 0}},
		{"ts 在未来超出容差", &model.Sample{TS: now + 301}},
		{"ts 超出补报窗口", &model.Sample{TS: now - 86401}},
		{"连接数越界", &model.Sample{TS: now, Metrics: model.Metrics{Conns: p(1_000_001)}}},
	}
	for _, c := range cases {
		if err := validateSample(c.s, now); err == nil {
			t.Errorf("%s 应被拒绝", c.name)
		}
	}

	// 边界值应当通过
	if err := validateSample(&model.Sample{TS: now - 86400}, now); err != nil {
		t.Errorf("恰好 24 小时前的补报样本应通过：%v", err)
	}
	if err := validateSample(&model.Sample{TS: now + 300}, now); err != nil {
		t.Errorf("5 分钟内的时钟偏差应容忍：%v", err)
	}
}

func TestSanitizeMetricsDropsNegativeAndLimits(t *testing.T) {
	m := &model.Metrics{NICs: map[string]model.NICCounters{
		"eth0":     {Rx: 100, Tx: 200},
		"bad;nic":  {Rx: 1, Tx: 1},
		"negative": {Rx: -1, Tx: 5},
	}}
	sanitizeMetrics(m)
	if _, ok := m.NICs["negative"]; ok {
		t.Error("负值计数器的网卡应被丢弃")
	}
	if _, ok := m.NICs["bad;nic"]; ok {
		t.Error("网卡名应被清洗，原名不应保留")
	}
	if len(m.NICs) != 2 {
		t.Errorf("应保留 2 项，得到 %d", len(m.NICs))
	}
}

func TestSanitizeLatency(t *testing.T) {
	list := sanitizeLatency([]model.LatencySample{
		{TargetID: 1, Loss: 20000},
		{TargetID: 2, Error: "内部错误"},
	})
	if list[0].Loss != 10000 {
		t.Errorf("丢包率应被夹到 10000，得到 %d", list[0].Loss)
	}
	if list[1].Error != model.ProbeErrOther {
		t.Errorf("未知错误应归为 other，得到 %q", list[1].Error)
	}

	// 数量上限 128
	big := make([]model.LatencySample, 200)
	if got := sanitizeLatency(big); len(got) != 128 {
		t.Errorf("应截断到 128，得到 %d", len(got))
	}
}

// S8：设置项白名单与取值校验（防止 install.master_url 拼入命令时被注入）。
func TestSettingSpecsWhitelist(t *testing.T) {
	if allowedSettingKey("evil.key") {
		t.Error("白名单外的键必须被拒绝")
	}
	if !allowedSettingKey("install.master_url") {
		t.Error("白名单内的键应被接受")
	}

	bad := []struct{ k, v string }{
		{"install.master_url", "http://m.example.com"},            // 必须 https（S7）
		{"install.master_url", "https://m.example.com; rm -rf /"}, // 字符集受限（S8）
		{"install.master_url", "https://m.example.com/$(id)"},
		{"retention.days", "0"},
		{"retention.days", "366"},
		{"report_interval_s", "1"},
		{"probe_interval_s", "5"},
		{"timezone", "Not/AZone"},
		{"install.mode", "sometimes"},
		{"inject.script_enabled", "2"},
		{"heartbeat_url", "http://hc.example.com"},
	}
	for _, c := range bad {
		if err := validateSettingValue(c.k, c.v); err == nil {
			t.Errorf("%s=%q 应被拒绝", c.k, c.v)
		}
	}

	good := []struct{ k, v string }{
		{"install.master_url", "https://m.example.com"},
		{"install.master_url", ""},
		{"retention.days", "365"},
		{"report_interval_s", "3"},
		{"probe_interval_s", "30"},
		{"timezone", "Asia/Shanghai"},
		{"install.mode", "on_demand"},
		{"inject.script_enabled", "1"},
		{"heartbeat_url", "https://hc-ping.com/abc"},
	}
	for _, c := range good {
		if err := validateSettingValue(c.k, c.v); err != nil {
			t.Errorf("%s=%q 应被接受，得到 %v", c.k, c.v, err)
		}
	}
}

// settingDefaults 与 settingSpecs 的键集合必须完全一致，且每个默认值都要能通过
// 它自己的校验器。否则会出现“设置了但没人校验”（漏网键）或
// “默认值本身就非法导致启动即失败”的情况。
func TestSettingDefaultsCoverSpecsAndAreValid(t *testing.T) {
	for k := range settingSpecs {
		def, ok := settingDefaults[k]
		if !ok {
			t.Errorf("设置项 %s 有校验器但没有默认值", k)
			continue
		}
		if err := validateSettingValue(k, def); err != nil {
			t.Errorf("设置项 %s 的默认值 %q 不合法：%v", k, def, err)
		}
		if !allowedSettingKey(k) {
			t.Errorf("设置项 %s 不在白名单内", k)
		}
	}
	for k := range settingDefaults {
		if _, ok := settingSpecs[k]; !ok {
			t.Errorf("默认值表里的 %s 没有对应校验器（可能是多余或拼错的键）", k)
		}
	}
	// 抽查几个关键默认值，防止被误改。
	for k, want := range map[string]string{
		"report_interval_s":     "3",
		"raw_resolution_s":      "10",
		"probe_interval_s":      "30",
		"disk_min_free_mb":      "200",
		"retention.raw_hours":   "24",
		"retention.mid_days":    "7",
		"retention.days":        "30",
		"install.mode":          "on_demand",
		"inject.script_enabled": "0",
		"theme.active":          "builtin",
	} {
		if got := settingDefaults[k]; got != want {
			t.Errorf("默认值 %s 应为 %q，实际 %q", k, want, got)
		}
	}
}

// S8：allowedURL 只允许 https 与受限字符集。
func TestAllowedURL(t *testing.T) {
	for _, ok := range []string{"https://m.example.com", "https://m.example.com:8443", "https://1.2.3.4"} {
		if !allowedURL(ok) {
			t.Errorf("%q 应被接受", ok)
		}
	}
	for _, bad := range []string{
		"http://m.example.com", "https://m.exam ple.com", "https://m.example.com/`id`",
		"https://m.example.com|x", "ftp://m.example.com", "",
	} {
		if allowedURL(bad) {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}
