package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TOTP（RFC 6238）实现（文档 9.3：可选二次验证）。
// 密钥以 base32（无填充）存储；库内建议加密保存（admins.totp_secret 已标注“加密存储”）。

// TOTPPeriod 是时间步长。
const TOTPPeriod = 30

// GenerateTOTPSecret 生成 20 字节随机密钥的 base32 表示。
func GenerateTOTPSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

// VerifyTOTP 校验 6 位动态码，允许 ±1 个时间窗口的时钟偏差。
func VerifyTOTP(secret []byte, code string, now time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}
	key := secret
	// 兼容直接存 base32 文本的情况。
	if len(key) == 0 {
		return false
	}
	if k, err := decodeBase32(string(key)); err == nil && looksBase32(string(key)) {
		key = k
	}

	counter := now.Unix() / TOTPPeriod
	for _, delta := range []int64{0, -1, 1} {
		if subtle.ConstantTimeCompare([]byte(totp(key, counter+delta)), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

func looksBase32(s string) bool {
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '2' && r <= '7' {
			continue
		}
		return false
	}
	return len(s) >= 16
}

func decodeBase32(s string) ([]byte, error) {
	s = strings.ToUpper(strings.TrimRight(s, "="))
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
}

// totp 计算指定计数器的 6 位码。
func totp(key []byte, counter int64) string {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(counter))

	mac := hmac.New(sha1.New, key)
	mac.Write(buf)
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	value := (int32(sum[offset]&0x7f) << 24) |
		(int32(sum[offset+1]) << 16) |
		(int32(sum[offset+2]) << 8) |
		int32(sum[offset+3])

	return fmt.Sprintf("%06d", value%1000000)
}

// ---------------------------------------------------------------------------
// 启用流程辅助（6.2）
// ---------------------------------------------------------------------------

// TOTPIssuer 是 otpauth:// URI 里的发行方名称，认证器 App 用它做分组。
const TOTPIssuer = "Server Probe"

// OTPAuthURI 生成供认证器 App 扫码/手工录入的标准 otpauth URI（RFC 6238 附录 + Key URI Format）。
//
// 为什么必须给 URI 而不只是给密钥字符串：绝大多数管理员用手机 App 扫码配置，
// 手工输 32 位 base32 极易出错，而输错一位的后果是"启用后动态码永远不对，
// 自己被锁在后台外面"。URI 里带上发行方与账号，App 里也能看清是哪台机器。
//
// 注意 label 里的冒号与 URI 里的 & ? = 都必须转义：账号名允许中文与空格，
// 不转义会拼出非法 URI，扫码直接失败。
func OTPAuthURI(secretBase32, account string) string {
	label := url.PathEscape(TOTPIssuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secretBase32)
	q.Set("issuer", TOTPIssuer)
	// 6 位 / 30 秒 / SHA1 是 RFC 6238 的默认组合，也是所有认证器 App 的默认值。
	// 显式写出来是为了让"配置一眼可查"，避免有人以为我们用 SHA256 而手动改设置。
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", strconv.Itoa(TOTPPeriod))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// NormalizeTOTPSecret 规范化管理员提交的密钥（去掉空格、连字符并转大写）。
//
// 认证器 App 展示的密钥常带空格分组（如 "abcd efgh ijkl mnop"），
// 复制粘贴进表单时若直接存原文，base32 解码会失败，表现为"扫出来了但码不对"。
func NormalizeTOTPSecret(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(s)
	if s == "" {
		return "", fmt.Errorf("密钥为空")
	}
	if !looksBase32(s) {
		return "", fmt.Errorf("密钥必须是 base32 字符（A-Z 与 2-7）")
	}
	if _, err := decodeBase32(s); err != nil {
		return "", fmt.Errorf("密钥不是合法的 base32：%w", err)
	}
	return s, nil
}

// GenerateRecoveryCodes 生成 n 个一次性恢复码的明文。
//
// 形态：XXXX-XXXX-XXXX（每段 4 位，字母表去掉易混淆的 0/O/1/I/L），
// 共约 40 bit 熵。恢复码只在"动态验证器不可用"时兜底使用，
// 且必须配合密码 + 单次消费 + 输错即计入失败锁定，40 bit 足够。
//
// 为什么不用 base32 全字母表：恢复码要人工从纸上抄录，0/O 与 1/I/l 混读
// 会让人以为"码是对的但登不上"，这种事在紧急场景下代价极高。
func GenerateRecoveryCodes(n int) ([]string, error) {
	const alphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // 无 0 O 1 I L
	out := make([]string, 0, n)
	buf := make([]byte, 4)
	for i := 0; i < n; i++ {
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		raw := make([]byte, 0, 12)
		for j := 0; j < 12; j++ {
			// 取低 5 位映射到 31 字母表；用模运算而非位运算是为了
			// 均匀分布（31 不是 2 的幂，取位会有偏）。
			raw = append(raw, alphabet[int(buf[j%len(buf)])%len(alphabet)])
		}
		out = append(out, string(raw[0:4])+"-"+string(raw[4:8])+"-"+string(raw[8:12]))
	}
	return out, nil
}

// TOTPCode 计算指定时刻的 6 位动态码。
//
// 导出它是为了两件正经事：
//  1. **测试**：端到端验证登录链路时必须能算出"此刻的正确码"，
//     否则只能测"错误的码被拒绝"，而"正确的码被接受"这条最关键的分支
//     反而测不到——那恰恰是"启用了 TOTP 就登不进去"的病灶所在。
//  2. **`probe-master show-totp` 之类的运维兜底**：管理员手机丢了又没有恢复码时，
//     可以登录服务器本机直接算出当前码（前提是能读到库里的密钥）。
//
// 注意：它不校验任何东西，拿到密钥的人本就能算出码。密钥的保密性是唯一防线。
func TOTPCode(secretBase32 string, at time.Time) (string, error) {
	s, err := NormalizeTOTPSecret(secretBase32)
	if err != nil {
		return "", err
	}
	key, err := decodeBase32(s)
	if err != nil {
		return "", err
	}
	return totp(key, at.Unix()/TOTPPeriod), nil
}

// NormalizeRecoveryCode 规范化恢复码，使"是否带连字符、大小写、空格"不影响校验。
//
// 库内哈希的是**规范化后**的形式，校验侧必须调用同一个函数——这与注册码
// （EnrollCodeHash / NormalizeEnrollCode）踩过的坑完全同型：
// 签发侧哈希规范化形式、核验侧哈希原文，会让所有码静默失效。
func NormalizeRecoveryCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	return strings.NewReplacer("-", "", " ", "", "\t", "").Replace(s)
}

// HashRecoveryCode 计算恢复码的 SHA-256（与 NormalizeRecoveryCode 配对使用）。
func HashRecoveryCode(plain string) []byte {
	return HashToken(NormalizeRecoveryCode(plain))
}
