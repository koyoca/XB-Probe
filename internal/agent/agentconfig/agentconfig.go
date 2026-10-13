// Package agentconfig 管理 Agent 本地配置（/etc/probe-agent/agent.yaml，权限 0600）。
package agentconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"

	"github.com/yourorg/probe/internal/model"
)

// Config 是 Agent 配置（对应文档 3.1 / 7.1 GET /config）。
// 只包含声明式数据：Agent 不解析、不执行任何脚本或命令（3.1 安全边界）。
type Config struct {
	MasterURL string `yaml:"master_url"`
	Token     string `yaml:"token"`

	// 自签证书场景固定公钥指纹，可多个并存以平滑轮换（13.4.9）。
	SPKISHA256 []string `yaml:"spki_sha256,omitempty"`

	// 开发专用：允许 http:// 的 Master 地址（仅回环）。S7。
	InsecureHTTP bool `yaml:"insecure_http,omitempty"`

	ReportIntervalS int `yaml:"report_interval_s"`
	ProbeIntervalS  int `yaml:"probe_interval_s"`

	NICInclude []string `yaml:"nic_include,omitempty"`
	NICExclude []string `yaml:"nic_exclude,omitempty"`
	DiskPaths  []string `yaml:"disk_paths,omitempty"`

	// 允许探测内网/回环目标（默认 false，防内网扫描跳板，S4）。
	AllowPrivateTargets bool `yaml:"allow_private_targets,omitempty"`

	BufferSamples int `yaml:"buffer_samples,omitempty"`

	// 服务管理器识别结果，卸载时按其还原（12.4）。
	ServiceManager string `yaml:"service_manager,omitempty"`

	ConfigVersion string `yaml:"config_version,omitempty"`

	Targets []model.Target `yaml:"targets"`
}

// DefaultPath 返回平台的默认配置路径（12.4）。
func DefaultPath() string {
	switch runtime.GOOS {
	case "windows":
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "probe-agent", "agent.yaml")
		}
		return `C:\ProgramData\probe-agent\agent.yaml`
	case "darwin", "freebsd":
		return "/usr/local/etc/probe-agent/agent.yaml"
	default:
		return "/etc/probe-agent/agent.yaml"
	}
}

// DefaultBinaryDir 返回平台的默认安装目录。
func DefaultBinaryDir() string {
	switch runtime.GOOS {
	case "windows":
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "probe-agent")
		}
		return `C:\ProgramData\probe-agent`
	default:
		return "/usr/local/bin"
	}
}

// Load 读取配置。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	c := &Config{}
	if err := yaml.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	c.applyDefaults()
	return c, nil
}

func (c *Config) applyDefaults() {
	if c.ReportIntervalS < 2 || c.ReportIntervalS > 60 {
		c.ReportIntervalS = 3
	}
	if c.ProbeIntervalS < 10 {
		c.ProbeIntervalS = 30 // 不低于 10 秒（13.4.6）
	}
	if c.BufferSamples <= 0 || c.BufferSamples > 1000 {
		c.BufferSamples = 100
	}
	if len(c.NICExclude) == 0 && len(c.NICInclude) == 0 {
		c.NICExclude = []string{"lo", "docker*", "veth*", "br-*", "virbr*", "tun*", "tap*", "wg*"}
	}
}

// Save 以 0600 写入配置（含令牌，绝不使用更宽松权限）。
func Save(path string, c *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ApplyServerConfig 用 Master 下发的配置覆盖本地探测清单与间隔（8.1 第 3 步）。
//
// 刻意**不覆盖** master_url / token / spki_sha256 / insecure_http /
// allow_private_targets / service_manager：前四项是"连接到谁"的根信任，
// 由本地配置决定；allow_private_targets 决定要不要放行内网目标（S4）。
// 允许远端改这些，等于把"改一个后台设置就能让所有节点去扫内网"变成可能。
//
// 网卡清单是**三态**（与不变量 8 同源）：
//   - nil（Master 未下发）→ 保持本地默认，不覆盖；
//   - 非空 → 按远端清单过滤。
//
// 把 nil 当成"清空"会把 Agent 内置的 lo/docker*/veth* 默认排除项抹掉，
// 而 collector 内部在两张清单都为空时是"全部放行"的——
// 于是流量把容器网卡与回环一起统计，数字凭空虚高（典型的"配置越改越坏"）。
func (c *Config) ApplyServerConfig(remote model.AgentConfig) {
	c.ReportIntervalS = remote.ReportIntervalS
	c.ProbeIntervalS = remote.ProbeIntervalS
	if remote.NICInclude != nil {
		c.NICInclude = remote.NICInclude
	}
	if remote.NICExclude != nil {
		c.NICExclude = remote.NICExclude
	}
	c.Targets = remote.Targets
	c.ConfigVersion = remote.ConfigVersion
}

// Redacted 返回去除令牌的副本，便于日志输出（9.1：令牌不进入日志）。
func (c *Config) Redacted() Config {
	cp := *c
	if cp.Token != "" {
		cp.Token = "pk_***"
	}
	return cp
}
