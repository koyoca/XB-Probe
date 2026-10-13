package auth

import (
	"crypto/rand"
	"strings"
)

// 注册码（文档 3.4 / S8）。
//
// 字符集固定为 Crockford base32（去掉易混淆的 I、L、O、U），
// 便于人工抄写且不会与命令中的 shell 元字符冲突。

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// EnrollCodeLength 是注册码的字符数（不含分隔符）。
const EnrollCodeLength = 12

// GenerateEnrollCode 生成形如 ABCD-EFGH-JKLM 的一次性注册码。
func GenerateEnrollCode() (string, error) {
	buf := make([]byte, EnrollCodeLength)
	raw := make([]byte, EnrollCodeLength)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = crockford[int(raw[i])%len(crockford)]
	}
	s := string(buf)
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12], nil
}

// NormalizeEnrollCode 去掉分隔符与空白并转大写，用于比对。
func NormalizeEnrollCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r == '-' || r == ' ' || r == '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// IsValidEnrollCode 判断（已规范化的）注册码是否合法：长度为 12 且全部落在 Crockford 字符集内。
func IsValidEnrollCode(s string) bool {
	if len(s) != EnrollCodeLength {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune(crockford, r) {
			return false
		}
	}
	return true
}

// EnrollCodeHash 计算注册码的入库哈希。
//
// 关键：**签发与核验必须都走这一个函数**。历史实现里签发侧做了
// NormalizeEnrollCode 而核验侧直接哈希原文，于是所有带 “-” 分隔符的注册码
// （也就是全部注册码）都会核验失败——整个注册流程直接不可用。
// 统一入口可以避免两侧再次走偏。
func EnrollCodeHash(s string) []byte {
	return HashToken(NormalizeEnrollCode(s))
}
