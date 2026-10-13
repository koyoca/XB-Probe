package tlsconf

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/koyoca/xb-probe/internal/config"
)

// 本文件把"证书现在到底怎么样了"落成一份可读的文件。
//
// 为什么需要它：ACME 的首次申请是**后台**做的（不阻塞启动），于是
// "部署脚本跑完了，证书到底签下来没有"在界面上没有任何答案——日志在
// journald 里，进程内存里的失败原因没人看得见，用户只能靠反复刷新浏览器
// 猜。把状态落盘之后，`xb-probe-master tls status` 与部署脚本都能读它，
// 把"没反馈"变成"有结果、有原因"。
//
// 它只是一份**展示用**快照，不参与任何决策：证书是否可用一律以内存中的
// 实际证书为准。因此这里写坏了（磁盘满、权限不对）也不会影响服务，
// 最多是状态页看不到东西——所以失败只记日志、不上抛。

// statusFileName 是状态文件名，与 cert.pem / cert.key 同目录。
const statusFileName = "status.json"

// Status 是证书状态的落盘快照。
//
// 字段全部是"给人看"的：时间一律 Unix 秒（与本项目其它地方一致），
// 空串表示"还没拿到这个信息"，而不是零值有别的含义。
type Status struct {
	Mode        string   `json:"mode"`
	Source      string   `json:"source"`
	Challenge   string   `json:"challenge,omitempty"`
	Domains     []string `json:"domains,omitempty"`
	Subject     string   `json:"subject,omitempty"`
	Issuer      string   `json:"issuer,omitempty"`
	SANs        []string `json:"sans,omitempty"`
	NotBefore   int64    `json:"not_before,omitempty"`
	NotAfter    int64    `json:"not_after,omitempty"`
	NextRenewal int64    `json:"next_renewal,omitempty"`
	SPKISHA256  string   `json:"spki_sha256,omitempty"`
	// LastError 是**最近一次失败**的原因；成功一次就会被清空。
	// 它必须能被读到，否则"申请失败"在现场只有"没有证书"这一个现象。
	LastError string `json:"last_error,omitempty"`
	// LastAttemptAt 是最近一次申请/续期尝试的开始时间。
	LastAttemptAt int64 `json:"last_attempt_at,omitempty"`
	// UpdatedAt 是本文件写入时间，用来判断"这份状态是不是这一轮写的新鲜货"。
	UpdatedAt int64 `json:"updated_at"`
}

// HasCert 表示这份状态里已经有一张证书。
func (s Status) HasCert() bool { return s.NotAfter > 0 }

// cacheDirOf 返回证书缓存目录（与 Setup 里那条推导规则必须完全一致）。
func cacheDirOf(cfg config.TLSConfig, dataDir string) string {
	if v := strings.TrimSpace(cfg.CacheDir); v != "" {
		return v
	}
	return filepath.Join(dataDir, config.DefaultACMECacheDir)
}

// StatusPath 返回状态文件路径。
//
// 导出给 cmd/xb-probe-master 用：命令行进程与服务进程必须算出**同一个**路径，
// 各写一份推导规则的话，一旦哪天默认目录改了，CLI 会一直读到一个不存在的文件，
// 报出来的现象是"状态永远是空的"——比报错更难查。
func StatusPath(cfg config.TLSConfig, dataDir string) string {
	return filepath.Join(cacheDirOf(cfg, dataDir), statusFileName)
}

// CertPath 返回当前模式下证书文件的路径（mode=none 返回空串）。
//
// 同样是为了"只有一份推导规则"：状态报告要告诉用户"证书在哪个文件里"，
// 而这个位置由上面的常量决定；在 CLI 里再拼一遍 cert.pem 就是等着哪天改名后
// 打印出一个不存在的路径。
func CertPath(cfg config.TLSConfig, dataDir string) string {
	switch strings.ToLower(strings.TrimSpace(cfg.Mode)) {
	case "files":
		return cfg.CertFile
	case "selfsigned":
		return filepath.Join(cacheDirOf(cfg, dataDir), selfSignedCertFile)
	case "acme":
		return filepath.Join(cacheDirOf(cfg, dataDir), certFileName)
	}
	return ""
}

// ReadStatus 读取状态文件。文件不存在时返回 os.ErrNotExist。
func ReadStatus(path string) (Status, error) {
	var st Status
	raw, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, err
	}
	return st, nil
}

// snapshot 生成当前状态（读锁内取全部字段，写盘在锁外）。
func (p *Provider) snapshot() Status {
	p.mu.RLock()
	cert := p.cert
	next := p.nextRenewal
	lastErr := p.lastErr
	attempt := p.lastAttempt
	p.mu.RUnlock()

	st := Status{
		Mode:          p.mode,
		Source:        sourceOf(p.mode),
		LastError:     lastErr,
		LastAttemptAt: unixOrZero(attempt),
		UpdatedAt:     time.Now().Unix(),
	}
	if p.mode == "acme" {
		st.Challenge = p.challenge
		st.Domains = append(append([]string{}, p.acmeDomains...), p.acmeIPs...)
	}
	if p.mode == "selfsigned" {
		st.Domains = append([]string{}, p.ssHosts...)
	}
	if cert != nil && cert.Leaf != nil {
		s := summarize(cert.Leaf)
		st.Subject, st.Issuer, st.SANs = s.Subject, s.Issuer, s.SANs
		st.NotBefore, st.NotAfter = cert.Leaf.NotBefore.Unix(), cert.Leaf.NotAfter.Unix()
		st.SPKISHA256, _ = spkiFingerprint(cert.Leaf)
	}
	st.NextRenewal = unixOrZero(next)
	return st
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// writeStatus 把状态写进 status.json（原子替换）。
//
// 用"临时文件 + rename"而不是直接覆盖：读它的是另一个进程（CLI 或部署脚本），
// 直接覆盖会出现"读到写了一半的 JSON"的窗口，表现为随机的解析失败。
func (p *Provider) writeStatus() {
	if p.cacheDir == "" {
		return
	}
	st := p.snapshot()
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	raw = append(raw, '\n')
	path := filepath.Join(p.cacheDir, statusFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o640); err != nil {
		p.log.Debug("写入 TLS 状态文件失败", "err", err.Error())
		return
	}
	// rename 在同一目录内是原子的；失败（跨设备、权限）时退化为直接写。
	if err := os.Rename(tmp, path); err != nil {
		_ = os.WriteFile(path, raw, 0o640)
		_ = os.Remove(tmp)
	}
}

// ErrNoStatusFile 由调用方用来区分"还没写过状态"与"状态文件损坏"。
var ErrNoStatusFile = errors.New("尚未生成 TLS 状态文件（服务可能还没启动过）")
