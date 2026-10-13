package api

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/koyoca/xb-probe/internal/model"
)

// 不可信数据处理（文档 13.4.4 / S1）。
//
// 被入侵的节点可上报任意字符串，这些字符串会出现在后台、公开页与告警消息中。
// 因此入库前按字段白名单与长度校验，输出端一律转义。

var (
	hostnameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,63}$`)
	nicRe      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
	semverRe   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.\-]+)?$`)
)

// 探测错误枚举（13.4.4）。
var validProbeErrors = map[string]bool{
	"":                             true,
	model.ProbeErrTimeout:          true,
	model.ProbeErrRefused:          true,
	model.ProbeErrUnreachable:      true,
	model.ProbeErrDNSFailed:        true,
	model.ProbeErrICMPNotPermitted: true,
	model.ProbeErrBlockedTarget:    true,
	model.ProbeErrOther:            true,
}

// sanitizeHostname：最长 63 字符，仅允许字母、数字与 . _ -，其余替换为 _。
func sanitizeHostname(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 63 {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// sanitizeText：最长 max 字符，去除控制字符与 < >。
func sanitizeText(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= max {
			break
		}
		if unicode.IsControl(r) || r == '<' || r == '>' {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// sanitizeNIC：最长 32 字符，字符集同 hostname。
func sanitizeNIC(s string) string {
	if nicRe.MatchString(s) {
		return s
	}
	if len(s) > 32 {
		s = s[:32]
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// sanitizeAgentVersion：最长 32 字符，须符合语义化版本格式。
func sanitizeAgentVersion(s string) string {
	if len(s) > 32 {
		s = s[:32]
	}
	if semverRe.MatchString(s) {
		return s
	}
	return ""
}

// sanitizeError：探测错误必须是枚举值，否则归为 other。
func sanitizeError(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if validProbeErrors[s] {
		return s
	}
	if s == "" {
		return ""
	}
	return model.ProbeErrOther
}

// sanitizeStatic 清洗静态信息。
func sanitizeStatic(s *model.Static) {
	if s == nil {
		return
	}
	s.Hostname = sanitizeHostname(s.Hostname)
	s.OS = sanitizeText(s.OS, 64)
	s.Kernel = sanitizeText(s.Kernel, 64)
	s.CPUModel = sanitizeText(s.CPUModel, 64)
	s.Arch = sanitizeText(s.Arch, 16)
	s.AgentVersion = sanitizeAgentVersion(s.AgentVersion)
	s.Virt = sanitizeText(s.Virt, 16)
	if s.CPUCores < 0 || s.CPUCores > 4096 {
		s.CPUCores = 0
	}
	if s.MemTotal < 0 || s.SwapTotal < 0 || s.DiskTotal < 0 {
		s.MemTotal, s.SwapTotal, s.DiskTotal = 0, 0, 0
	}
}

// validateSample 对数值做范围校验；越界则丢弃整条样本并计数（13.4.4）。
func validateSample(s *model.Sample, now int64) error {
	if s.TS <= 0 {
		return fmt.Errorf("ts 非法")
	}
	// 补报时间窗默认 24 小时（9.2）；未来时间容忍 5 分钟时钟偏差（7.1）。
	if s.TS > now+300 {
		return fmt.Errorf("ts 超出允许窗口（未来）")
	}
	if s.TS < now-86400 {
		return fmt.Errorf("ts 超出补报时间窗")
	}
	m := &s.Metrics
	checkPct := func(p *int, name string) error {
		if p == nil {
			return nil
		}
		if *p < 0 || *p > 10000 {
			return fmt.Errorf("%s 越界", name)
		}
		return nil
	}
	if err := checkPct(m.CPU, "cpu"); err != nil {
		return err
	}
	// conns 不是百分比：单独放宽到 0~1e6。
	if m.Conns != nil && (*m.Conns < 0 || *m.Conns > 1_000_000) {
		return fmt.Errorf("conns 越界")
	}
	if m.Load1 != nil && (*m.Load1 < 0 || *m.Load1 > 100000) {
		return fmt.Errorf("load1 越界")
	}
	for _, v := range []*int64{m.MemUsed, m.SwapUsed, m.DiskUsed, m.RxBps, m.TxBps} {
		if v != nil && *v < 0 {
			return fmt.Errorf("字节/速率不得为负")
		}
	}
	if len(m.NICs) > 64 {
		return fmt.Errorf("网卡数量超限")
	}
	return nil
}

// sanitizeMetrics 清洗指标中的网卡名，并对异常项做上限保护。
func sanitizeMetrics(m *model.Metrics) {
	if m == nil || m.NICs == nil {
		return
	}
	clean := make(map[string]model.NICCounters, len(m.NICs))
	for name, c := range m.NICs {
		if c.Rx < 0 || c.Tx < 0 {
			continue
		}
		clean[sanitizeNIC(name)] = c
		if len(clean) >= 64 {
			break
		}
	}
	m.NICs = clean
}

// sanitizeLatency 清洗探测结果。
func sanitizeLatency(list []model.LatencySample) []model.LatencySample {
	if len(list) > 128 {
		list = list[:128]
	}
	for i := range list {
		list[i].Error = sanitizeError(list[i].Error)
		if list[i].Loss < 0 || list[i].Loss > 10000 {
			list[i].Loss = 10000
		}
	}
	return list
}
