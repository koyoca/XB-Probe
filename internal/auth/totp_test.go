package auth

import (
	"strings"
	"testing"
	"time"
)

// 注：RFC 6238 标准向量的基础校验已存在于 auth_test.go 的
// TestTOTPMatchesRFC6238Vectors / TestVerifyTOTPRoundTrip，本文件只补
// **启用流程**相关的原语（otpauth URI、密钥规范化、恢复码）。
// 重复实现同一组向量没有价值，反而会在改密钥算法时改漏一处。

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decodeBase32(s)
	if err != nil {
		t.Fatalf("解码 base32 失败：%v", err)
	}
	return b
}

// VerifyTOTP 必须容忍 ±1 个时间窗口的时钟偏差。
//
// 手机与服务器的时间几乎不可能毫秒级对齐；不允许偏差的话，
// 在窗口边界上会出现"这一秒能登、下一秒不能"的随机失败。
func TestVerifyTOTPAcceptsAdjacentWindows(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("生成密钥失败：%v", err)
	}
	key := mustDecode(t, secret)
	base := time.Unix(1_600_000_000, 0)
	// 用中心窗口的码去验前后各一个窗口的时刻，都应通过。
	code := totp(key, base.Unix()/TOTPPeriod)
	for _, delta := range []time.Duration{-TOTPPeriod, 0, TOTPPeriod} {
		if !VerifyTOTP([]byte(secret), code, base.Add(delta*time.Second)) {
			t.Errorf("偏移 %v 秒时应接受中心窗口的码", delta)
		}
	}
	// 偏差 3 个窗口必须拒绝——放宽到这一步等于把 6 位码的有效期拉到 3 分钟。
	if VerifyTOTP([]byte(secret), code, base.Add(3*TOTPPeriod*time.Second)) {
		t.Error("偏移 3 个窗口时应拒绝")
	}
}

func TestVerifyTOTPRejectsMalformed(t *testing.T) {
	secret, _ := GenerateTOTPSecret()
	now := time.Now()
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "  ", "12 45"} {
		if VerifyTOTP([]byte(secret), bad, now) {
			t.Errorf("畸形输入 %q 不应通过", bad)
		}
	}
	if VerifyTOTP(nil, "123456", now) {
		t.Error("空密钥不应通过")
	}
}

// OTPAuthURI 必须能被标准认证器解析，且账号名里的特殊字符要转义。
//
// 断言写成一个**完整值**（issuer=XB-Probe）而不是身份前缀：旧实现里
// TOTPIssuer 是 "Server Probe"，而这里断言 "issuer=Server" —— 靠"前缀恰好是
// 另一个名字的前缀"蒙混过关。产品改名后就变成一条永远会红的假红，或者反过来
// 掩盖"issuer 被写成了别的 Server 开头的名字"。
func TestOTPAuthURI(t *testing.T) {
	uri := OTPAuthURI("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "admin")
	for _, want := range []string{
		"otpauth://totp/", "secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ",
		"issuer=XB-Probe", "algorithm=SHA1", "digits=6", "period=30",
		// label 里也带上 issuer（认证器 App 就按它分组），改名时两处都要跟着走。
		"otpauth://totp/XB-Probe:admin?",
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("URI 缺少 %q：%s", want, uri)
		}
	}
	// 账号名含中文与空格时必须转义，否则 URI 非法、扫码直接失败。
	uri2 := OTPAuthURI("GEZDGNBVGY3TQOJQ", "运维 管理员")
	if strings.Contains(uri2, "运维 管理员") {
		t.Errorf("账号名未转义：%s", uri2)
	}
	if !strings.Contains(uri2, "%20") {
		t.Errorf("空格应被转义为 %%20：%s", uri2)
	}
}

func TestNormalizeTOTPSecret(t *testing.T) {
	ok := []struct{ in, want string }{
		{"gezdgnbvgy3tqojq", "GEZDGNBVGY3TQOJQ"},
		{" GEZD GNBV GY3T QOJQ ", "GEZDGNBVGY3TQOJQ"},
		{"GEZD-GNBV-GY3T-QOJQ", "GEZDGNBVGY3TQOJQ"},
	}
	for _, c := range ok {
		got, err := NormalizeTOTPSecret(c.in)
		if err != nil {
			t.Errorf("%q 应被接受：%v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q → %q，期望 %q", c.in, got, c.want)
		}
	}
	// 认证器 App 展示的密钥常带空格分组，规范化后必须与不带空格的等价——
	// 否则"扫出来了但码不对"。
	a, _ := NormalizeTOTPSecret("GEZD GNBV GY3T QOJQ")
	b, _ := NormalizeTOTPSecret("GEZDGNBVGY3TQOJQ")
	if a != b {
		t.Errorf("带空格与不带空格应等价：%q vs %q", a, b)
	}

	for _, bad := range []string{"", "   ", "ABC", "GEZD1NBV!", "0189"} {
		if _, err := NormalizeTOTPSecret(bad); err == nil {
			t.Errorf("非法密钥 %q 应被拒绝", bad)
		}
	}
}

// 恢复码：形态、长度、字母表、去重。
func TestGenerateRecoveryCodes(t *testing.T) {
	codes, err := GenerateRecoveryCodes(10)
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	if len(codes) != 10 {
		t.Fatalf("期望 10 个，得到 %d", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		// 形态 XXXX-XXXX-XXXX
		parts := strings.Split(c, "-")
		if len(parts) != 3 {
			t.Errorf("恢复码 %q 应形如 XXXX-XXXX-XXXX", c)
			continue
		}
		for _, p := range parts {
			if len(p) != 4 {
				t.Errorf("恢复码 %q 每段应为 4 位", c)
			}
		}
		// 不得出现易混淆字符：0 O 1 I L
		if strings.ContainsAny(c, "0O1IL") {
			t.Errorf("恢复码 %q 含易混淆字符（0/O/1/I/L）", c)
		}
		if seen[c] {
			t.Errorf("恢复码重复：%q", c)
		}
		seen[c] = true
	}
	// 两批之间不应重复（熵足够时的基本健全性检查）。
	more, _ := GenerateRecoveryCodes(10)
	for _, c := range more {
		if seen[c] {
			t.Errorf("两批之间出现重复：%q", c)
		}
	}
}

// 恢复码的规范化与哈希必须一致——签发侧与核验侧共用一个函数。
//
// 这是从注册码（NormalizeEnrollCode + EnrollCodeHash）踩过的坑里学来的：
// 签发时哈希规范化形式、核验时哈希原文，会让**所有**恢复码静默失效，
// 而单测只要"自己哈希自己验"就照样全绿。
func TestRecoveryCodeHashNormalization(t *testing.T) {
	const plain = "ABCD-EFGH-JKMN"
	variants := []string{
		"ABCD-EFGH-JKMN",
		"abcd-efgh-jkmn",
		"ABCDEFGHJKMN",
		" abcd efgh jkmn ",
		"AbCd-EfGh-JkMn",
	}
	want := HashRecoveryCode(plain)
	for _, v := range variants {
		if got := HashRecoveryCode(v); !Equal(got, want) {
			t.Errorf("%q 的哈希应与 %q 一致", v, plain)
		}
	}
	// 不同码必须得到不同哈希。
	if Equal(HashRecoveryCode("ABCD-EFGH-JKMN"), HashRecoveryCode("ABCD-EFGH-JKMP")) {
		t.Error("不同恢复码不应得到相同哈希")
	}
	if len(want) != 32 {
		t.Errorf("应为 SHA-256（32 字节），得到 %d", len(want))
	}
}

func TestNormalizeRecoveryCode(t *testing.T) {
	got := NormalizeRecoveryCode(" abcd-efgh jkmn\t")
	if got != "ABCDEFGHJKMN" {
		t.Errorf("规范化结果 %q，期望 ABCDEFGHJKMN", got)
	}
}
