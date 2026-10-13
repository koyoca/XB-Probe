package acme

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// dns-01 挑战（RFC 8555 §8.4）。
//
// 为什么值得支持：http-01 要 80 端口、tls-alpn-01 要 443 端口，而现实里
// 这两种端口常常已经被别的程序占用（或者运营商直接封 80）；只有 dns-01
// **完全不碰本机端口**，代价是要有域名 DNS 的写权限。它也是通配符证书
// （*.example.com）唯一可用的挑战方式——CA 无法为通配符指定一个具体的
// HTTP 路径去校验。

// DNSProvider 是 dns-01 的"记录写手"。
//
// 接口刻意保持最小：写一条 TXT、返回一个清理函数。各家 DNS 的 API 差异极大
// （有的用长期 Token、有的用 AK/SK 签名、有的要先建子域），但它们在 ACME
// 眼里都只是"把这条 TXT 放进去、验完删掉"，因此适配一家只需要实现这两个动作，
// 不必碰协议里其它任何部分。
type DNSProvider interface {
	// Present 写入挑战 TXT 记录并返回清理函数。
	//
	// recordName 是**完整**记录名（如 _acme-challenge.example.com），
	// recordValue 是已经算好的 TXT 值（base64url(SHA-256(keyAuthorization))）。
	// 返回的清理函数必须是幂等的：挑战失败、超时、正常结束都会调用它，
	// 残留一条 TXT 记录本身无害，但会让人怀疑"到底哪条是当前在用的"。
	Present(ctx context.Context, recordName, recordValue string) (func(context.Context) error, error)
}

// dnsTXTValue 计算 dns-01 的 TXT 记录值：base64url(SHA-256(keyAuthorization))。
//
// 注意与 tls-alpn-01 的差别：那个把摘要放进证书扩展，这个放进 DNS。
// 两者摘的是同一个 keyAuthorization，因此这里与 challenge.go 里那段
// 必须永远算出同一个值——对不上时 CA 只会报"校验失败"，看不出是哪一步。
func dnsTXTValue(keyAuth string) string {
	sum := sha256.Sum256([]byte(keyAuth))
	return base64URL.EncodeToString(sum[:])
}

// challengeRecordName 返回某个标识符对应的挑战记录名。
//
// 通配符要**去掉 `*.` 前缀**：RFC 8555 规定通配符授权的标识符值就是
// example.com（wildcard 标志单独给出），而 DNS 里写
// `_acme-challenge.*.example.com` 是非法域名——CA 会直接判失败。
func challengeRecordName(identifier string) string {
	v := strings.ToLower(strings.TrimSpace(identifier))
	// 末尾的根点（"example.com."）在 DNS 里合法、在记录名里多余：
	// Cloudflare 一类的 API 会把它当成另一个名字，查询直接落空。
	v = strings.TrimSuffix(v, ".")
	return "_acme-challenge." + strings.TrimPrefix(v, "*.")
}

// ---------------------------------------------------------------------------
// Cloudflare
// ---------------------------------------------------------------------------

// CloudflareConfig 是 Cloudflare DNS 的接入参数。
type CloudflareConfig struct {
	// APIToken 是 Cloudflare 的 API Token（推荐用「编辑区域 DNS」模板，
	// 权限 Zone:DNS:Edit + Zone:Zone:Read）。**不是** Global API Key。
	APIToken string
	// BaseURL 便于测试注入；默认 https://api.cloudflare.com/client/v4。
	BaseURL string
	// HTTPClient 可注入（测试用桩服务器）。
	HTTPClient *http.Client
	// Settle 是记录写入后等待传播的固定时长，默认 5 秒。
	//
	// 为什么需要它：Cloudflare 的 API 一旦返回 200，权威 NS 上就已经有这条
	// 记录了，但 CA 侧可能有短暂的上游缓存（尤其是刚做过一次失败的校验）。
	// 等几秒的代价远小于一次失败的申请——Let's Encrypt 对失败校验有速率限制
	// （每域名每小时 5 次），撞上去要等一小时。
	Settle time.Duration
	Logger *slog.Logger
}

// Cloudflare 是 Cloudflare DNS 的 DNSProvider 实现。
type Cloudflare struct {
	token  string
	base   string
	hc     *http.Client
	settle time.Duration
	log    *slog.Logger
}

// 默认 Cloudflare API 前缀与传播等待。
const (
	cloudflareAPIBase = "https://api.cloudflare.com/client/v4"
	cloudflareSettle  = 5 * time.Second
)

// NewCloudflare 构造 Cloudflare 提供方。
func NewCloudflare(cfg CloudflareConfig) (*Cloudflare, error) {
	token := strings.TrimSpace(cfg.APIToken)
	if token == "" {
		return nil, errors.New("acme: 使用 Cloudflare dns-01 必须配置 API Token")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = cloudflareAPIBase
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	settle := cfg.Settle
	if settle <= 0 {
		settle = cloudflareSettle
	}
	return &Cloudflare{token: token, base: base, hc: hc, settle: settle, log: log}, nil
}

// cfEnvelope 是 Cloudflare v4 API 的统一响应外壳。
type cfEnvelope struct {
	Success  bool            `json:"success"`
	Errors   []cfError       `json:"errors"`
	Messages []cfError       `json:"messages"`
	Result   json.RawMessage `json:"result"`
	// result_info 只用到总数，用于"zone 找不到"时的诊断。
	ResultInfo struct {
		Total int `json:"total_count"`
	} `json:"result_info"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e cfEnvelope) errString() string {
	if len(e.Errors) == 0 {
		return "（未给出原因）"
	}
	parts := make([]string, 0, len(e.Errors))
	for _, x := range e.Errors {
		parts = append(parts, fmt.Sprintf("[%d] %s", x.Code, x.Message))
	}
	return strings.Join(parts, "; ")
}

// do 发一次 JSON 请求并解开外壳。
func (c *Cloudflare) do(ctx context.Context, method, path string, body any) (*cfEnvelope, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	url := c.base + path
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "xb-probe-master-acme/1")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用 Cloudflare API 失败: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var env cfEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("Cloudflare 返回了无法解析的内容（HTTP %d）：%s", resp.StatusCode, truncate(raw, 200))
	}
	// 401/403 单看 errors 里的文案往往只有 "Invalid API Token"，
	// 对"token 少了某个权限"这种最常见的情形几乎没有指向性，这里补一句。
	if !env.Success {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return &env, fmt.Errorf("Cloudflare 拒绝了 API Token（HTTP %d）：%s；"+
				"请确认 Token 具备 Zone:DNS:Edit 与 Zone:Zone:Read 权限，且覆盖该域名",
				resp.StatusCode, env.errString())
		}
		if resp.StatusCode == http.StatusNotFound {
			return &env, fmt.Errorf("Cloudflare 查无此资源（HTTP 404）：%s", env.errString())
		}
		return &env, fmt.Errorf("Cloudflare API 返回失败（HTTP %d）：%s", resp.StatusCode, env.errString())
	}
	return &env, nil
}

// cfZone 是 zone 列表项里用得到的字段。
type cfZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// zoneFor 找出某个记录名所属的 zone。
//
// 做法是从最长的候选名开始逐级去掉最左标签去查——`_acme-challenge.a.b.example.com`
// 可能是 a.b.example.com 这个 zone 里的记录，也可能直接属于 example.com。
// Cloudflare 的 zone 列表接口对 `name` 是**精确匹配**，所以必须逐个试。
func (c *Cloudflare) zoneFor(ctx context.Context, recordName string) (*cfZone, error) {
	labels := strings.Split(recordName, ".")
	for i := 0; i+1 < len(labels); i++ { // 至少留两个标签（"example.com"）
		candidate := strings.Join(labels[i:], ".")
		env, err := c.do(ctx, http.MethodGet, "/zones?name="+urlQueryEscape(candidate)+"&status=active", nil)
		if err != nil {
			return nil, err
		}
		var zones []cfZone
		if err := json.Unmarshal(env.Result, &zones); err != nil {
			return nil, fmt.Errorf("解析 Cloudflare zone 列表失败: %w", err)
		}
		if len(zones) > 0 && zones[0].ID != "" {
			return &zones[0], nil
		}
	}
	return nil, fmt.Errorf("在 Cloudflare 上找不到 %s 所属的 zone——"+
		"请确认该域名确实托管在这个账户下，且 API Token 具备 Zone:Zone:Read 权限", recordName)
}

// Present 写入 TXT 记录，返回删除它的清理函数。
func (c *Cloudflare) Present(ctx context.Context, recordName, recordValue string) (func(context.Context) error, error) {
	recordName = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(recordName)), ".")
	if recordName == "" || recordValue == "" {
		return nil, errors.New("acme: dns-01 的记录名与记录值都不能为空")
	}
	zone, err := c.zoneFor(ctx, recordName)
	if err != nil {
		return nil, err
	}
	c.log.Info("写入 ACME dns-01 挑战记录", "zone", zone.Name, "name", recordName)

	env, err := c.do(ctx, http.MethodPost, "/zones/"+zone.ID+"/dns_records", map[string]any{
		"type":    "TXT",
		"name":    recordName,
		"content": recordValue,
		// 120 秒是最短常规 TTL：挑战存在的时间以分钟计，设长一点没有好处，
		// 反而会让"上一次失败留下的旧记录"缓存更久。
		"ttl":     120,
		"comment": "xb-probe-master ACME dns-01（校验完成后自动删除）",
	})
	if err != nil {
		return nil, err
	}
	var rec struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(env.Result, &rec); err != nil || rec.ID == "" {
		return nil, errors.New("Cloudflare 未返回新建记录 ID，无法在之后清理它")
	}

	cleanup := func(cctx context.Context) error {
		_, derr := c.do(cctx, http.MethodDelete, "/zones/"+zone.ID+"/dns_records/"+rec.ID, nil)
		if derr != nil {
			// 删除失败不能升级成错误：此时证书可能已经签下来了，
			// 为了"清理一条 TXT 记录"把成功的申请报成失败，只会误导人。
			c.log.Warn("删除 ACME dns-01 挑战记录失败（可手动清理，不影响已签发的证书）",
				"name", recordName, "id", rec.ID, "err", derr.Error())
			return nil
		}
		c.log.Info("已删除 ACME dns-01 挑战记录", "name", recordName)
		return nil
	}

	if c.settle > 0 {
		select {
		case <-ctx.Done():
			// 上下文已取消时也要把刚写的记录删掉，否则会留一地垃圾。
			_ = cleanup(context.WithoutCancel(ctx))
			return nil, ctx.Err()
		case <-time.After(c.settle):
		}
	}
	return cleanup, nil
}

// urlQueryEscape 只用于拼 zone 名——zone 名里可能出现通配符等需要转义的字符，
// 但不会出现空格，因此用 QueryEscape 的等价物即可。
func urlQueryEscape(v string) string {
	var b strings.Builder
	for _, r := range []byte(v) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '.', r == '_', r == '~':
			b.WriteByte(r)
		default:
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}
