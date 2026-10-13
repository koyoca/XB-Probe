// Package auth 提供令牌、会话、CSRF 与凭证加密等安全原语。
// 对应设计文档 9.1 / 9.3 / 9.4 与 13.4.7。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// TokenPrefix 是长期令牌前缀（9.1）。
const TokenPrefix = "pk_"

// tokenLen 是令牌随机部分长度：32 字节 = 256 位。
const tokenLen = 32

// GenerateToken 生成一个新的长期令牌，返回明文、SHA-256 哈希与前 6 位提示。
// 明文只在此刻存在，随后仅用于返回给节点一次（3.4 / 9.1）。
func GenerateToken() (plain string, hash []byte, hint string, err error) {
	raw := make([]byte, tokenLen)
	if _, err = rand.Read(raw); err != nil {
		return "", nil, "", fmt.Errorf("rand: %w", err)
	}
	plain = TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	h := sha256.Sum256([]byte(plain))
	return plain, h[:], Hint(plain), nil
}

// HashToken 计算令牌的 SHA-256（库内只存哈希）。
func HashToken(plain string) []byte {
	h := sha256.Sum256([]byte(plain))
	return h[:]
}

// HashHex 返回哈希的十六进制形式，用作内存缓存键。
func HashHex(hash []byte) string { return hex.EncodeToString(hash) }

// Hint 返回令牌前 6 位（含前缀），仅供后台辨认，不泄露密钥（6.3 token_hint）。
func Hint(plain string) string {
	if len(plain) <= 6 {
		return plain
	}
	return plain[:6]
}

// Equal 常量时间比较，避免时序侧信道（9.1）。
func Equal(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// RandomHex 生成 n 字节随机数的十六进制串（用于会话 ID、CSRF、public_id）。
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ---------------------------------------------------------------------------
// 令牌校验缓存（13.4.7 校验顺序）
//   1) SHA-256 → 内存缓存命中
//   2) 未命中 → 查库并回填缓存
//   3) 常量时间比较
//   4) 检查 revoked_at 与节点未归档
// ---------------------------------------------------------------------------

// TokenStore 是 TokenManager 依赖的持久层（由 Master 的仓储实现）。
type TokenStore interface {
	// FindTokenByHash 按哈希查令牌；不存在返回 (0, 0, false, nil)。
	FindTokenByHash(hash []byte) (tokenID, nodeID int64, revoked bool, err error)
	// NodeStatus 返回节点状态（archived 视为不可用）。
	NodeStatus(nodeID int64) (status string, err error)
	// TouchToken 更新最后使用时间与来源 IP。
	TouchToken(tokenID, nodeID int64, ip string, at int64) error
}

type cacheEntry struct {
	tokenID int64
	nodeID  int64
	revoked bool
}

// TokenManager 维护 哈希→节点 的内存映射，吊销即时生效（8.2）。
type TokenManager struct {
	store TokenStore

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

// NewTokenManager 创建令牌管理器。
func NewTokenManager(store TokenStore) *TokenManager {
	return &TokenManager{store: store, cache: make(map[string]cacheEntry)}
}

// Authenticate 校验明文令牌，成功返回节点 ID。
// 返回的错误码用于映射 HTTP 状态（401/403）。
func (m *TokenManager) Authenticate(plain, srcIP string, now time.Time) (nodeID int64, err error) {
	if !strings.HasPrefix(plain, TokenPrefix) {
		return 0, fmt.Errorf("invalid")
	}
	key := HashHex(HashToken(plain))

	m.mu.RLock()
	e, ok := m.cache[key]
	m.mu.RUnlock()

	if !ok {
		tokenID, nodeID, revoked, err := m.store.FindTokenByHash(HashToken(plain))
		if err != nil {
			return 0, err
		}
		if tokenID == 0 {
			return 0, fmt.Errorf("invalid")
		}
		e = cacheEntry{tokenID: tokenID, nodeID: nodeID, revoked: revoked}
		m.mu.Lock()
		m.cache[key] = e
		m.mu.Unlock()
	}

	if e.revoked {
		return 0, fmt.Errorf("revoked")
	}

	status, err := m.store.NodeStatus(e.nodeID)
	if err != nil {
		return 0, err
	}
	if status == "archived" {
		return 0, fmt.Errorf("archived")
	}

	_ = m.store.TouchToken(e.tokenID, e.nodeID, srcIP, now.Unix())
	return e.nodeID, nil
}

// Revoke 从缓存中剔除令牌，使吊销即时生效（8.2 第 1 步）。
func (m *TokenManager) Revoke(plainOrHash string, isPlain bool) {
	key := plainOrHash
	if isPlain {
		key = HashHex(HashToken(plainOrHash))
	}
	m.mu.Lock()
	delete(m.cache, key)
	m.mu.Unlock()
}

// InvalidateNode 使某节点的所有缓存条目失效。
func (m *TokenManager) InvalidateNode(nodeID int64) {
	m.mu.Lock()
	for k, v := range m.cache {
		if v.nodeID == nodeID {
			delete(m.cache, k)
		}
	}
	m.mu.Unlock()
}
