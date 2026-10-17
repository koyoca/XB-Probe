package api

import (
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/koyoca/xb-probe/internal/acme"
	"github.com/koyoca/xb-probe/internal/store"
)

// 访问日志的落库通道 + IP 黑名单（设置项 security.*）。
//
// 两条硬约束，改之前先读：
//
//  1. **绝不能阻塞请求**。写日志是旁路行为，磁盘慢、库被锁、队列满时
//     都必须直接丢弃那一条，而不是让用户的页面卡住。所以这里是带缓冲的
//     channel + 单写协程，满了就丢（并记一次丢弃计数供日志观察）。
//  2. **必须记 RealIP 解析后的地址**。经 CDN/反代时 TCP 对端是边缘节点，
//     直接记 r.RemoteAddr 会让整份日志变成几个重复的边缘 IP、完全失去意义
//     （用户特别提醒了这一点）。RealIP 已按 trusted_proxies + CDN 网段还原。

// accessQueueSize 是访问日志的缓冲深度。
//
// 1000 条 ≈ 一次突发流量；正常部署（个人看板）每秒十几个请求，写协程完全跟得上。
// 队列满时丢弃最旧是不可能的（channel 做不到），这里选择丢弃**新的**：
// 突发时保住已经排队的记录，比让队列变成"只有最后几条"更有用。
const accessQueueSize = 1000

// AccessRecorder 异步把访问记录写入 access_log 表。
type AccessRecorder struct {
	repo    *store.Repo
	enabled func() bool

	ch     chan store.AccessLogEntry
	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once

	mu     sync.Mutex
	droped int64
}

// NewAccessRecorder 创建记录器。enabled 每次现读设置，改动即时生效。
func NewAccessRecorder(repo *store.Repo, enabled func() bool) *AccessRecorder {
	return &AccessRecorder{
		repo:    repo,
		enabled: enabled,
		ch:      make(chan store.AccessLogEntry, accessQueueSize),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

// Start 启动写协程。
func (a *AccessRecorder) Start() {
	go func() {
		defer close(a.doneCh)
		for {
			select {
			case e := <-a.ch:
				_ = a.repo.InsertAccessLog(e)
			case <-a.stopCh:
				// 退出前把队列排空：剩下的都是已经答应记下来的。
				for {
					select {
					case e := <-a.ch:
						_ = a.repo.InsertAccessLog(e)
					default:
						return
					}
				}
			}
		}
	}()
}

// Stop 停止写协程并排空队列。
func (a *AccessRecorder) Stop() {
	a.once.Do(func() { close(a.stopCh) })
	<-a.doneCh
}

// Record 记一条访问。非阻塞：关掉时直接返回，队列满时丢弃。
func (a *AccessRecorder) Record(e store.AccessLogEntry) {
	if a == nil || a.enabled == nil || !a.enabled() {
		return
	}
	select {
	case a.ch <- e:
	default:
		a.mu.Lock()
		a.droped++
		a.mu.Unlock()
	}
}

// Dropped 返回因队列满被丢弃的条数（供诊断）。
func (a *AccessRecorder) Dropped() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.droped
}

// parseIPList 把设置里的文本解析成前缀集合。
//
// 支持三种写法，一行（或逗号）一条：
//   - 单个 IP：`1.2.3.4`（按 /32 或 /128 处理）
//   - CIDR：`1.2.3.0/24`
//   - 通配尾段：`1.2.3.*`（等价 /24，只在 v4 下有意义）
//
// 非法条目**跳过而不是整份失效**：一行写错不该让整张黑名单悄悄变成"不生效"，
// 那会让管理员以为封禁生效了而实际上没封。
func parseIPList(raw string) []netip.Prefix {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';' || r == ' '
	})
	out := make([]netip.Prefix, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		// 通配尾段：1.2.3.* → 1.2.3.0/24
		if strings.HasSuffix(f, ".*") {
			if p, err := netip.ParsePrefix(strings.TrimSuffix(f, ".*") + ".0/24"); err == nil {
				out = append(out, p.Masked())
			}
			continue
		}
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(f); err == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out
}

// ipMatches 判断地址是否命中任一条前缀。
func ipMatches(addr netip.Addr, list []netip.Prefix) bool {
	for _, p := range list {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// accessLogEnabled 读"是否记录访问日志"（每次现读设置，改完立即生效）。
func (h *Handler) accessLogEnabled() bool {
	if h.Repo == nil {
		return false
	}
	return h.Repo.GetSettingDefault("security.access_log_enabled", "0") == "1"
}

// blacklistPrefixes 现读黑名单（改完立即生效，无需重启）。
func (h *Handler) blacklistPrefixes() []netip.Prefix {
	return parseIPList(h.Repo.GetSettingDefault("security.ip_blacklist", ""))
}

// IPBlacklistMiddleware 拒绝命中黑名单的来源。
//
// 位置很关键：必须挂在 RealIPMiddleware **内侧**——黑名单要比对的是
// 还原后的真实客户端地址，而不是 CDN 边缘节点的地址。挂在最外层会把
// 整个 CDN 的出口段当成一个客户端，误封或漏封都会发生。
//
// 命中时返回 403 而不是 404：管理员排查"某个 IP 为什么打不开"时，
// 403 能直接指向黑名单；404 会让人以为是站点没起来。
//
// 不记录访问日志：被拦住的是攻击流量，记下来只会让日志表迅速被刷满
// （真的要看就在系统日志里有一条）。
func (h *Handler) IPBlacklistMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Repo 为 nil 只出现在测试夹具里（只关心路由、不连库）。
		// 生产路径永远有 Repo；这里放行而不是 panic，否则半个 api 包的用例都会挂。
		if h.Repo == nil {
			next.ServeHTTP(w, r)
			return
		}
		list := h.blacklistPrefixes()
		if len(list) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		addr, err := netip.ParseAddr(RealIP(r))
		if err == nil && ipMatches(addr, list) {
			// 纯文本回包：不给攻击者任何指纹，也不加载模板。
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("403 forbidden\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// accessLogMiddleware 把访问记录旁路写入 access_log（与 AccessLogMiddleware
// 的分工：那个是控制台日志、这个是可查询的库内日志）。
//
// 记录范围：只记真实请求，不记 ACME 挑战与静态资源——静态资源一次页面加载
// 就是十几个，全记下来会让"谁访问了页面"这件事淹没在噪声里。
func accessLogMiddleware(rec *AccessRecorder, next http.Handler) http.Handler {
	if rec == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recw, r)
		if skipAccessLog(r.URL.Path) {
			return
		}
		rec.Record(store.AccessLogEntry{
			TS:     start.Unix(),
			IP:     RealIP(r),
			Method: r.Method,
			Path:   truncatePath(r.URL.Path),
			Status: recw.status,
			Bytes:  int64(recw.size),
			DurMs:  time.Since(start).Milliseconds(),
			UA:     truncateUA(r.UserAgent()),
		})
	})
}

// skipAccessLog 判断是否不值得记的路径。
func skipAccessLog(p string) bool {
	if strings.HasPrefix(p, acme.HTTP01Prefix) {
		return true
	}
	switch {
	case strings.HasPrefix(p, "/style.css"),
		strings.HasPrefix(p, "/app.js"),
		strings.HasPrefix(p, "/theme.json"),
		strings.HasPrefix(p, "/favicon"),
		strings.HasPrefix(p, "/admin/static/"):
		return true
	}
	return false
}

// truncatePath 截断过长的路径（扫描器会拼几 KB 的 payload 进来）。
func truncatePath(p string) string {
	const max = 512
	if len(p) <= max {
		return p
	}
	return p[:max] + "…"
}

// truncateUA 截断 UA（同上，防脏数据撑爆表）。
func truncateUA(ua string) string {
	const max = 256
	if len(ua) <= max {
		return ua
	}
	return ua[:max]
}
