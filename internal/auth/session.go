package auth

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// 会话生命周期（9.3）：空闲 2 小时、绝对 12 小时过期。
const (
	SessionIdleTTL     = 2 * time.Hour
	SessionAbsoluteTTL = 12 * time.Hour

	// SecureCookieName 使用 __Host- 前缀：仅限当前主机、必须 Secure、Path=/。
	// 兄弟子域无法读取，配合管理端独立源可防止 S2（主题脚本窃取会话）。
	//
	// 注意：浏览器对 __Host- 前缀有硬性要求——必须同时带 Secure 且不带 Domain。
	// 因此它只在真正走 HTTPS 时可用。
	SecureCookieName = "__Host-probe_session"

	// PlainCookieName 是纯 HTTP 部署（tls.mode=none，例如本机/内网调试）下的
	// 回退名。此时绝不能带 __Host- 前缀也不能带 Secure，否则浏览器会直接丢弃
	// 该 Cookie，症状是"登录成功却立刻被打回登录页"。
	//
	// 纯 HTTP 下会话仍受 HttpOnly + SameSite=Strict + 自定义 CSRF 头保护，
	// 但传输层明文，仅建议用于本机或受信内网。
	PlainCookieName = "probe_session"

	// CSRFHeader 是所有写请求必须携带的自定义头（13.4.5）。
	// 跨源请求会因预检失败而被拒绝。
	CSRFHeader = "X-CSRF-Token"
)

// CookieNameFor 按当前请求是否走 HTTPS 返回应使用的会话 Cookie 名。
func CookieNameFor(secure bool) string {
	if secure {
		return SecureCookieName
	}
	return PlainCookieName
}

// SessionStore 是会话持久层（由 Master 仓储实现）。
type SessionStore interface {
	// CreateSession 保存会话（存 SHA-256 哈希，不存明文）。
	CreateSession(idHash []byte, adminID int64, csrf, ip, ua string, createdAt, expiresAt int64) error
	// LookupSession 按哈希读取会话；不存在返回 (nil, nil)。
	LookupSession(idHash []byte) (*Session, error)
	// TouchSession 续期空闲 TTL，并返回新的 expires_at。
	TouchSession(idHash []byte, expiresAt int64) error
	// DeleteSession 删除会话。
	DeleteSession(idHash []byte) error
}

// Session 是 sessions 表的映射。
type Session struct {
	IDHash    []byte
	AdminID   int64
	CSRFToken string
	IP        string
	UserAgent string
	CreatedAt int64
	ExpiresAt int64
}

// NewSessionID 生成 256 位会话 ID 与其 SHA-256。
func NewSessionID() (plain string, idHash []byte, err error) {
	plain, err = RandomHex(32)
	if err != nil {
		return "", nil, err
	}
	return plain, HashToken(plain), nil
}

// SetSessionCookie 写入管理端会话 Cookie。
//
// secure 必须反映该请求真实的传输协议：只有 HTTPS（本机 TLS 或可信反代的
// X-Forwarded-Proto: https）才为 true。纯 HTTP 下传 true 会导致浏览器
// 静默丢弃 Cookie——这是"登录不上"最常见的原因。
//
// 域名始终留空：__Host- 前缀要求不带 Domain 属性。
func SetSessionCookie(w http.ResponseWriter, plainID string, maxAge int, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieNameFor(secure),
		Value:    plainID,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	})
}

// ClearSessionCookie 登出/会话失效时清除 Cookie。
// 两个名字都清：部署可能在 HTTPS 与 HTTP 之间切换过，残留的旧 Cookie 会
// 让浏览器同时带上两个会话值，从而出现"登出后仍是登录态"的错觉。
func ClearSessionCookie(w http.ResponseWriter) {
	for _, name := range []string{SecureCookieName, PlainCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/",
			HttpOnly: true, Secure: name == SecureCookieName,
			SameSite: http.SameSiteStrictMode, MaxAge: -1,
		})
	}
}

// ReadSessionCookie 读取会话 Cookie。
// 依次尝试 HTTPS 与 HTTP 两种名字，因此协议切换期间存量会话不会立刻失效。
func ReadSessionCookie(r *http.Request) (string, bool) {
	for _, name := range []string{SecureCookieName, PlainCookieName} {
		c, err := r.Cookie(name)
		if err == nil && c.Value != "" {
			return c.Value, true
		}
	}
	return "", false
}

// CheckIdleExpiry 判定会话是否因空闲过期（9.3）。
func CheckIdleExpiry(s *Session, now time.Time) error {
	if now.Unix() > s.ExpiresAt {
		return fmt.Errorf("会话过期")
	}
	if now.Unix()-s.ExpiresAt > int64(SessionAbsoluteTTL.Seconds()) {
		return fmt.Errorf("会话绝对过期")
	}
	return nil
}

// BearerToken 从 Authorization 头解析 Bearer 令牌（7.1）。
// 令牌不进入 URL 与查询串（9.1）。
func BearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}
