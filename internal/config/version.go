// Package config 负责 Master 的配置加载与“按节点有效配置摘要”的计算。
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/koyoca/xb-probe/internal/model"
)

// EffectiveConfig 是某节点的“有效配置”——用于计算 config_version 的规范化输入。
// 对应 13.4.3：不再使用全局版本号，只有受影响的节点会重新拉取配置。
type EffectiveConfig struct {
	ReportIntervalS int            `json:"report_interval_s"`
	ProbeIntervalS  int            `json:"probe_interval_s"`
	NICInclude      []string       `json:"nic_include"`
	NICExclude      []string       `json:"nic_exclude"`
	Targets         []model.Target `json:"targets"`

	// 远程运维指令（见 model.AgentConfig 的说明）：换域名与自动升级。
	MasterURL     string `json:"master_url,omitempty"`
	AutoUpgrade   bool   `json:"auto_upgrade,omitempty"`
	TargetVersion string `json:"target_version,omitempty"`
}

// ComputeConfigVersion 计算配置摘要：规范化 JSON 的 SHA-256 前 8 字节（16 位十六进制）。
// 输入需先按稳定顺序排序（targets 按 id，网卡列表按字典序），否则摘要不稳定。
func ComputeConfigVersion(c EffectiveConfig) string {
	normalized := c
	normalized.NICInclude = sortedCopy(c.NICInclude)
	normalized.NICExclude = sortedCopy(c.NICExclude)
	normalized.Targets = append([]model.Target(nil), c.Targets...)
	sort.Slice(normalized.Targets, func(i, j int) bool {
		return normalized.Targets[i].ID < normalized.Targets[j].ID
	})

	raw, err := json.Marshal(normalized)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

// BuildEffective 从设置项与探测目标构造某节点的有效配置。
// settings 为 settings 表内容；targets 为该节点生效的探测目标（全局 + 专属）。
func BuildEffective(settings map[string]string, targets []*model.ProbeTarget) EffectiveConfig {
	return EffectiveConfig{
		ReportIntervalS: atoiDefault(settings["report_interval_s"], 3),
		ProbeIntervalS:  atoiDefault(settings["probe_interval_s"], 30),
		NICInclude:      splitList(settings["nic_include"]),
		NICExclude:      splitList(settings["nic_exclude"]),
		Targets:         toTargets(targets),
		// 换域名：install.master_url 既用于拼接安装命令，也作为"应切换到的地址"
		// 下发给已装节点——这样改一处就能同时覆盖新装与存量（否则要分两步做，
		// 中间那段时间新装的节点和存量节点指向不同域名，排查起来极乱）。
		MasterURL:     strings.TrimSpace(settings["install.master_url"]),
		AutoUpgrade:   settings["agent.auto_upgrade"] == "1",
		TargetVersion: strings.TrimSpace(settings["agent.target_version"]),
	}
}

func toTargets(in []*model.ProbeTarget) []model.Target {
	out := make([]model.Target, 0, len(in))
	for _, t := range in {
		if !t.Enabled {
			continue
		}
		// ProbeTarget.ID 是库内自增主键（int64）；model.Target 是下发给 Agent 的
		// 线协议类型（int，对应 7.1 的 "id"）。在边界处显式转换，避免隐式截断。
		out = append(out, model.Target{
			ID: int(t.ID), Name: t.Name, Host: t.Host, Port: t.Port,
			Method: t.Method, TimeoutMs: t.TimeoutMs, Count: 3,
		})
	}
	return out
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return def
	}
	return n
}

// splitList 把设置项里的逗号/空白分隔列表切成切片。
//
// 未设置（或设为空串）时返回 **nil**，而不是空切片——这是一个三态契约，
// 和 public.cards 的教训同源（不变量 8）：
//
//	nil   = 未下发，Agent 保持自己的默认值；
//	非空  = 按这份清单过滤。
//
// 若把"未设置"写成 `[]string{}`，序列化出去就是 `[]`，Agent 侧会把它当成
// "管理员要求不过滤任何网卡"，于是内置默认排除项（lo/docker*/veth*/…）被一次抹掉，
// 流量统计把容器网卡与回环一起算进去，数字凭空虚高。
// 注意 ComputeConfigVersion 里的 sortedCopy 会把 nil 归一成 []，
// 所以这里改返回 nil 不会让已部署节点的 config_version 发生变化。
//
// 分隔符必须覆盖制表符与回车：这些值是人从终端、表格、Windows 浏览器里
// 剪贴进来的（与 parsePins 容忍换行的理由完全一样）。少认一个分隔符就会生成
// 一个**幽灵条目**——"\t" 或 "\r" 被当成一条网卡匹配模式，它匹配不到任何接口，
// 于是既不生效也不报错，只在序列化后的 config_version 里留下一点噪声。
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
