// Package client 是 Agent 与 Master 通信的 HTTPS 客户端。
//
// 安全要点：
//   - 全程 TLS 1.2+；默认严格校验证书，不提供“跳过校验”开关（9.1）。
//   - 自签证书场景用 SPKI 公钥指纹固定（13.4.9），支持多指纹并存以平滑轮换。
//   - 拒绝 http:// 的 Master 地址，仅本机回环可由 --insecure-http 放行（S7）。
//   - 令牌放在 Authorization 头，不进入 URL 与日志（9.1）。
package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/koyoca/xb-probe/internal/model"
)

// Client 是 Master 的 API 客户端。
type Client struct {
	base  *url.URL
	token string
	hc    *http.Client
}

// Options 是客户端选项。
type Options struct {
	MasterURL    string
	Token        string
	SPKISHA256   []string // 固定的公钥（SPKI）SHA-256，base64；可多个
	InsecureHTTP bool     // 仅回环 + 显式开启时放行 http://
	Timeout      time.Duration
}

// New 创建客户端。
func New(o Options) (*Client, error) {
	u, err := url.Parse(o.MasterURL)
	if err != nil {
		return nil, fmt.Errorf("Master 地址非法: %w", err)
	}

	switch u.Scheme {
	case "https":
	case "http":
		if !o.InsecureHTTP {
			return nil, fmt.Errorf("拒绝 http:// 的 Master 地址（S7）；仅本机回环可用 --insecure-http 放行")
		}
		if !isLoopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("--insecure-http 仅允许用于本机回环地址")
		}
	default:
		return nil, fmt.Errorf("不支持的协议：%s", u.Scheme)
	}

	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: u.Hostname(),
	}
	if len(o.SPKISHA256) > 0 {
		pins, err := parsePins(o.SPKISHA256)
		if err != nil {
			return nil, err
		}
		// 固定公钥指纹：校验证书的 SPKI，而非整张证书（S3）。
		tlsCfg.InsecureSkipVerify = true // 关掉链校验，改为下方 SPKI 校验
		tlsCfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("未收到证书")
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
			got := base64.StdEncoding.EncodeToString(sum[:])
			for _, pin := range pins {
				if pin == got {
					return nil
				}
			}
			return fmt.Errorf("公钥指纹不匹配（配置了 %d 个指纹）", len(pins))
		}
	}

	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	transport := &http.Transport{
		TLSClientConfig:     tlsCfg,
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // 自行处理 gzip，明确控制
	}

	return &Client{
		base:  u,
		token: o.Token,
		hc: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// 不跟随跨主机重定向，避免令牌被转发到其他主机。
				if len(via) > 0 && req.URL.Host != via[0].URL.Host {
					return fmt.Errorf("拒绝跨主机重定向")
				}
				return nil
			},
		},
	}, nil
}

// SetToken 在注册成功后设置令牌。
func (c *Client) SetToken(tok string) { c.token = tok }

// Enroll 用注册码换取节点令牌（不需要既有令牌）。
func (c *Client) Enroll(ctx context.Context, req model.EnrollRequest) (*model.EnrollResponse, error) {
	var out model.EnrollResponse
	if err := c.do(ctx, http.MethodPost, "/api/agent/v1/enroll", req, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// Report 上报样本，返回 Master 的响应（含 config_version）。
func (c *Client) Report(ctx context.Context, req model.ReportRequest) (*model.ReportResponse, error) {
	var out model.ReportResponse
	if err := c.do(ctx, http.MethodPost, "/api/agent/v1/report", req, &out, true); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetConfig 拉取配置；etag 命中时返回 notModified=true（3.1）。
func (c *Client) GetConfig(ctx context.Context, etag string) (*model.AgentConfig, bool, error) {
	u := c.resolve("/api/agent/v1/config")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, err
	}
	c.setHeaders(httpReq, true)
	if etag != "" {
		httpReq.Header.Set("If-None-Match", `"`+etag+`"`)
	}

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, true, nil
	}
	body, err := readBody(resp)
	if err != nil {
		return nil, false, err
	}
	if err := checkStatus(resp.StatusCode, body); err != nil {
		return nil, false, err
	}
	var cfg model.AgentConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, false, err
	}
	return &cfg, false, nil
}

// Deregister 尽力通知 Master 节点已卸载（超时 5 秒，失败不阻塞，3.4）。
func (c *Client) Deregister(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out struct{}
	return c.do(ctx, http.MethodPost, "/api/agent/v1/deregister", nil, &out, true)
}

// ---------------------------------------------------------------------------

func (c *Client) do(ctx context.Context, method, path string, in, out any, auth bool) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, c.resolve(path), body)
	if err != nil {
		return err
	}
	c.setHeaders(httpReq, auth)

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := readBody(resp)
	if err != nil {
		return err
	}
	if err := checkStatus(resp.StatusCode, raw); err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (c *Client) setHeaders(r *http.Request, auth bool) {
	r.Header.Set("Accept", "application/json")
	r.Header.Set("Accept-Encoding", "gzip")
	r.Header.Set("User-Agent", "xb-probe-agent")
	if r.Body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if auth && c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// resolve 把相对路径拼成绝对 URL。
//
// 查询串必须单独放进 RawQuery：直接塞进 u.Path 的话 '?' 会被转义成 %3F
// （net/url 对 encodePath 里的 '?' 一律转义），服务端收到的就是一个
// 名字里带问号的路径 —— 表现为 404，而单看代码完全看不出来。
func (c *Client) resolve(path string) string {
	u := *c.base
	if i := strings.Index(path, "?"); i >= 0 {
		u.RawQuery = path[i+1:]
		path = path[:i]
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String()
}

func readBody(resp *http.Response) ([]byte, error) {
	var r io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	}
	return io.ReadAll(io.LimitReader(r, 1<<20))
}

func checkStatus(code int, body []byte) error {
	if code >= 200 && code < 300 {
		return nil
	}
	var env model.ErrorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Code != "" {
		return &APIError{Status: code, Code: env.Error.Code, Message: env.Error.Message}
	}
	return &APIError{Status: code, Code: "unknown", Message: http.StatusText(code)}
}

// APIError 是带状态码的 API 错误，便于调用方按 401/403/429 分支处理。
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("master 返回 %d %s: %s", e.Status, e.Code, e.Message)
}

// IsUnauthorized 判断是否为令牌无效/吊销（7.1：Agent 退避至 10 分钟重试且不自毁）。
func (e *APIError) IsUnauthorized() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parsePins 归一化 SPKI 指纹列表，便于与证书实际指纹做字节比较。
//
// 必须容忍这三种写法，它们都会真实出现（指纹是运维从终端/文档/后台页面手工拷进来的）：
//   - 带填充的标准 base64（GNU `base64` 的默认输出）；
//   - **不带填充**的 base64（`openssl ... | base64 | tr -d '='` 这类常见命令的输出）——
//     早先只试 StdEncoding，于是这种写法被判为"格式非法"，Agent 直接起不来，
//     而指纹其实一个字节都不差；
//   - 中间夹着空白（终端折行、从 Markdown 表格里复制）。
//
// 空串跳过（配置里留空行是常事），统一成"标准带填充"形式再参与比较。
func parsePins(list []string) ([]string, error) {
	out := make([]string, 0, len(list))
	for _, s := range list {
		s = strings.Map(func(r rune) rune {
			switch r {
			case ' ', '\t', '\r', '\n':
				return -1
			}
			return r
		}, s)
		if s == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			raw, err = base64.RawStdEncoding.DecodeString(s)
		}
		if err != nil {
			return nil, fmt.Errorf("SPKI 指纹格式非法：%s", s)
		}
		out = append(out, base64.StdEncoding.EncodeToString(raw))
	}
	return out, nil
}

// Get 发起一次带令牌的 JSON GET（供自升级一类的扩展接口使用）。
//
// 单独的出口而不是让调用方自己拼 http.Get：令牌必须只出现在 Authorization 头上，
// 且必须沿用同一套 TLS/SPKI 校验。任何"自己发一个请求"的写法都会悄悄绕过指纹固定。
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out, true)
}

// Download 以节点令牌身份发起一次原始 GET，返回整个响应（调用方需关闭 Body）。
//
// 为什么要返回响应而不是字节切片：
//   - 调用方要读响应头里的校验和（X-Checksum-Sha256）做快速失败；
//   - Agent 常跑在内存很小的机器上，把整份二进制读进内存再写盘是没必要的风险，
//     必须边下边算哈希边落盘。
func (c *Client) Download(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.resolve(path), nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, true)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}
