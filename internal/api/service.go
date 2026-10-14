// Package api 实现三组独立前缀、独立中间件的 HTTP 接口
// （文档 7.0：/api/agent/v1、/api/admin/v1、/api/public/v1）。
//
// 路由使用 Go 1.22 的 http.ServeMux 方法+路径模式与 PathValue 通配符，
// 不引入第三方路由库，符合“依赖最小化”的取向。
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/koyoca/xb-probe/internal/alert"
	"github.com/koyoca/xb-probe/internal/auth"
	"github.com/koyoca/xb-probe/internal/model"
	"github.com/koyoca/xb-probe/internal/pipeline"
	"github.com/koyoca/xb-probe/internal/state"
	"github.com/koyoca/xb-probe/internal/store"
	"github.com/koyoca/xb-probe/internal/theme"
	"github.com/koyoca/xb-probe/internal/traffic"
)

// Deps 是接口层依赖的聚合。
type Deps struct {
	Repo     *store.Repo
	Registry *state.Registry
	Snap     *state.Snapshotter
	Tokens   *auth.TokenManager
	Keyring  *auth.Keyring
	Writer   *pipeline.Writer
	Multi    *pipeline.MultiResolution
	Latency  *pipeline.LatencyMultiResolution
	Alerts   *alert.Engine
	Themes   *theme.Manager
	Tracker  *traffic.Tracker

	Version        string
	TrustedProxies []string
	AccessLog      bool // 是否在 HTTP 层输出访问日志（master.yaml access_log，默认关闭）
	TLSMode        string     // none | files | selfsigned | acme
	TLSActive      bool       // 本进程是否自己终止 TLS（决定 Secure Cookie 与 HSTS）
	HSTS           HSTSConfig // HSTS 下发策略（来自 tls.hsts）
	PublicHosts    []string   // 6.1：公开站允许的 Host（空 = 不限制）
	AdminHosts     []string   // 6.1：管理端允许的 Host（空 = 不限制）
	// AdminOrigin 是 master.yaml 里声明的管理端对外地址（如
	// https://admin.example.com[:8443]）。公开看板的「管理后台」按钮按它跳转；
	// 为空时按下发的 admin_hosts 推导，再不行就保持同站相对路径。
	AdminOrigin string
	TLSInfo     model.TLSStatusReader // 证书运行态（可为 nil，仅影响 /tls/status 的回显）
	DistDir     string                // data_dir/dist
	InstallMode func() string         // on_demand | always | off
	MasterURL   func() string
	Logger      *slog.Logger

	// ChallengeHandler 是 ACME http-01 的应答处理器（mode=acme 且
	// challenge=http-01 时由 tlsconf 提供）。它会被挂到所有监听地址上，
	// 这样无论操作者直接放行 80 端口，还是让反代把 /.well-known/ 转发进来，
	// 挑战都能被应答。为 nil 表示不需要。
	ChallengeHandler http.Handler

	// Heartbeat 暴露外部心跳（A5）的运行态供后台展示。
	// 由 cmd/xb-probe-master 的调度器实现并注入；测试或未接线的场景允许为 nil。
	Heartbeat model.HeartbeatReader
}

// Handler 持有全部 HTTP 处理逻辑。
type Handler struct {
	Deps
	// insecureCookieWarned 保证"未启用 TLS"的告警只打印一次（atomic 因为有并发登录）。
	insecureCookieWarned atomic.Bool
}

// New 创建 Handler。
func New(d Deps) *Handler { return &Handler{Deps: d} }

// nowFunc 是"取现在"的接缝，生产路径恒为 time.Now。
//
// 存在的理由：登录锁定（9.3 的"5 次锁定 15 分钟"）是**时间相关**的行为契约，
// 判据落在"锁定期内连正确口令也进不来、到期那一刻自动放行"这条行为链上。
// 若直接读时钟，验它只能真的 sleep 15 分钟（没人会跑，于是这条断言长期缺失——
// 文档 02 里它就一直是"第三类未补"）；写死成常量又会让 LockedUntil 的比较
// 失去意义。留一个可替换的取时函数，才能把 T、T+899、T+900 三个点钉死。
var nowFunc = time.Now

// ---------------------------------------------------------------------------
// 响应辅助
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true) // 输出端一律转义（13.4.4）
	_ = enc.Encode(v)
}

// writeErr 按 7.0 的错误格式返回。
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, model.ErrorEnvelope{Error: model.APIError{Code: code, Message: msg}})
}

func pathInt64(r *http.Request, name string) (int64, bool) {
	v := r.PathValue(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func queryInt(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// allowedURL 校验 Master 地址：仅 https，字符集受限（S8）。
// --insecure-http 的放行逻辑由 Agent 侧负责，Master 只生成规范命令。
func allowedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "https" {
		return false
	}
	return allowedURLChars(raw)
}

// allowedHTTPURL 与 allowedURL 同一套字符规则，但允许 http://。
//
// 用在"只是给浏览器跳转用"的地址上（如 site.admin_url）：明文 HTTP 直连 IP
// 的部署填不出 https，一律拒绝等于让那类部署永远配不出正确的后台入口。
// 字符集照样收紧——这个值会进 <a href>，放开前缀等于开一个可被后台改写的
// 脚本入口（`javascript:` 之类）。
func allowedHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Host == "" {
		return false
	}
	return allowedURLChars(raw)
}

// allowedURLChars 是 URL 的字符白名单（字母数字与 .-:/_）。
func allowedURLChars(raw string) bool {
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		switch r {
		case '.', '-', ':', '/', '_':
		default:
			return false
		}
	}
	return true
}
