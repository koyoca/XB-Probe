package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id 参数（9.3）。生产可调高 memory 以增强抗暴力破解能力。
type argonParams struct {
	memory      uint32 // KiB
	iterations  uint32
	parallelism uint8
	saltLen     uint32
	keyLen      uint32
}

var defaultArgon = argonParams{
	memory:      64 * 1024, // 64 MB
	iterations:  3,
	parallelism: 2,
	saltLen:     16,
	keyLen:      32,
}

// ErrInvalidHash 表示哈希串格式非法。
var ErrInvalidHash = errors.New("argon2: 哈希格式非法")

// ErrMismatch 表示密码不匹配。
var ErrMismatch = errors.New("argon2: 密码不匹配")

// HashPassword 生成 argon2id 哈希，格式：
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt-b64>$<hash-b64>
func HashPassword(password string) (string, error) {
	salt := make([]byte, defaultArgon.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt,
		defaultArgon.iterations, defaultArgon.memory, defaultArgon.parallelism, defaultArgon.keyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, defaultArgon.memory, defaultArgon.iterations, defaultArgon.parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword 校验明文密码（常量时间比较）。
func VerifyPassword(password, encoded string) error {
	p, salt, key, err := decodeHash(encoded)
	if err != nil {
		return err
	}
	other := argon2.IDKey([]byte(password), salt, p.iterations, p.memory, p.parallelism, p.keyLen)
	if subtle.ConstantTimeCompare(key, other) == 1 {
		return nil
	}
	return ErrMismatch
}

func decodeHash(encoded string) (p argonParams, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, ErrInvalidHash
	}
	var version int
	if _, err = fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if _, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.iterations, &p.parallelism); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	p.saltLen = uint32(len(salt))
	p.keyLen = uint32(len(key))
	return p, salt, key, nil
}
