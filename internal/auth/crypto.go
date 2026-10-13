package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

// ---------------------------------------------------------------------------
// 主密钥与凭证加密（9.4）
// ---------------------------------------------------------------------------

// KeySize 是主密钥长度：32 字节（AES-256）。
const KeySize = 32

// Keyring 持有用于加密告警渠道凭证的主密钥。
// 主密钥保存在独立文件（0600）或环境变量中，备份时与数据库分开保管。
type Keyring struct {
	key []byte
}

// LoadOrCreateKeyring 从 keyFile 读取主密钥；不存在则生成并写盘。
// 环境变量 PROBE_MASTER_KEY（base64）优先，便于容器化部署。
func LoadOrCreateKeyring(keyFile string) (*Keyring, error) {
	if env := os.Getenv("PROBE_MASTER_KEY"); env != "" {
		raw, err := base64.StdEncoding.DecodeString(env)
		if err != nil {
			return nil, fmt.Errorf("decode PROBE_MASTER_KEY: %w", err)
		}
		if len(raw) != KeySize {
			return nil, fmt.Errorf("PROBE_MASTER_KEY 必须为 %d 字节", KeySize)
		}
		return &Keyring{key: raw}, nil
	}

	if b, err := os.ReadFile(keyFile); err == nil {
		if len(b) != KeySize {
			return nil, fmt.Errorf("主密钥长度非法：期望 %d 字节", KeySize)
		}
		return &Keyring{key: b}, nil
	}

	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("生成主密钥失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		return nil, fmt.Errorf("写入主密钥失败: %w", err)
	}
	return &Keyring{key: key}, nil
}

// Encrypt 使用 AES-256-GCM 加密（alert_channels.config_enc）。
func (k *Keyring) Encrypt(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(k.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	// 输出：nonce || ciphertext
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt 解密 alert_channels.config_enc。
func (k *Keyring) Decrypt(sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(k.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(sealed) < ns {
		return nil, fmt.Errorf("密文过短")
	}
	return gcm.Open(nil, sealed[:ns], sealed[ns:], nil)
}

// ---------------------------------------------------------------------------
// 密码哈希（argon2id，9.3）
// ---------------------------------------------------------------------------

// GenerateCSRF 生成 CSRF 令牌：HMAC(sessionID) 的前 32 字节十六进制。
// 与会话绑定后无需额外存储即可校验（session 表也保存一份以便轮换）。
func GenerateCSRF(sessionID string) string {
	mac := hmac.New(sha256.New, []byte(sessionID))
	mac.Write([]byte("csrf"))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyCSRF 常量时间校验 CSRF 令牌。
func VerifyCSRF(expected, got string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}
