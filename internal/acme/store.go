package acme

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 账户状态的磁盘缓存布局（目录由 CacheDir 指定）：
//
//	<dir>/account.key  账户私钥（PKCS#8 PEM，0600）
//	<dir>/account.url  账户 URL（kid），单行文本
//
// 账户密钥必须持久化：每次重启都重新注册会迅速撞上 CA 的"每 IP 每日新账户数"
// 限制，而且旧账户签发的证书无法再用新账户续期。
// 证书本身的落盘由 internal/tlsconf 统一负责（四种模式共用一套）。

const (
	accountKeyFile = "account.key"
	accountURLFile = "account.url"
)

func loadOrCreateAccountKey(dir string) (*ecdsa.PrivateKey, error) {
	if dir == "" {
		return newP256Key()
	}
	path := filepath.Join(dir, accountKeyFile)
	raw, err := os.ReadFile(path)
	if err == nil {
		blk, _ := pem.Decode(raw)
		if blk == nil {
			return nil, fmt.Errorf("ACME 账户密钥 %s 不是 PEM 格式", path)
		}
		parsed, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("解析 ACME 账户密钥失败: %w", err)
		}
		ec, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("ACME 账户密钥类型应为 ECDSA，实际为 %T", parsed)
		}
		return ec, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("读取 ACME 账户密钥失败: %w", err)
	}
	key, err := newP256Key()
	if err != nil {
		return nil, err
	}
	if err := writePrivateKey(path, key); err != nil {
		return nil, err
	}
	return key, nil
}

func loadAccountURL(dir string) (string, error) {
	if dir == "" {
		return "", os.ErrNotExist
	}
	raw, err := os.ReadFile(filepath.Join(dir, accountURLFile))
	if err != nil {
		return "", err
	}
	url := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(url, "http") {
		return "", errors.New("account.url 内容不是 URL")
	}
	return url, nil
}

func saveAccountURL(dir, url string) error {
	if dir == "" {
		return nil
	}
	if url == "" {
		// 账户 URL 失效（accountDoesNotExist）：删掉缓存文件，下次重新注册。
		if err := os.Remove(filepath.Join(dir, accountURLFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, accountURLFile), []byte(url+"\n"), 0o600)
}

// writePrivateKey 以 PKCS#8 PEM 写入私钥，权限 0600。
// 目录不存在时按 0700 创建——ACME 缓存目录里有账户私钥，不该给同机其他用户读。
func writePrivateKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	blk := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	return os.WriteFile(path, pem.EncodeToMemory(blk), 0o600)
}

// SanitizeName 把一段标识符文本转成安全的文件名片段。
// 用于把 status.example.com / 203.0.113.10 变成磁盘上的文件名。
func SanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		case r == '*' || r == ':':
			// 通配符与 IPv6 冒号：换成 '_'，避免与文件系统语义冲突。
			b.WriteByte('_')
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._")
	if out == "" {
		out = "cert"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}
