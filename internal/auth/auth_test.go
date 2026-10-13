package auth

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// S8 / 3.4：注册码字符集固定为 Crockford base32，且形如 ABCD-EFGH-JKLM。
func TestEnrollCodeFormatAndCharset(t *testing.T) {
	for i := 0; i < 50; i++ {
		code, err := GenerateEnrollCode()
		if err != nil {
			t.Fatalf("生成失败：%v", err)
		}
		if len(code) != 14 {
			t.Fatalf("长度应为 14（12 位 + 2 个分隔符），得到 %d：%q", len(code), code)
		}
		if code[4] != '-' || code[9] != '-' {
			t.Fatalf("分隔符位置不对：%q", code)
		}
		plain := NormalizeEnrollCode(code)
		if len(plain) != EnrollCodeLength {
			t.Fatalf("去掉分隔符后应为 %d 位，得到 %d", EnrollCodeLength, len(plain))
		}
		// Crockford 去掉了易混淆的 I L O U，避免人工抄错与 shell 元字符歧义。
		if strings.ContainsAny(plain, "ILOU") {
			t.Fatalf("不应包含易混淆字符：%q", plain)
		}
		// 注册码会被拼进安装命令，必须全是字母数字。
		for _, r := range plain {
			if !strings.ContainsRune(crockford, r) {
				t.Fatalf("出现字符集外的字符 %q：%q", r, plain)
			}
		}
	}
}

func TestNormalizeEnrollCode(t *testing.T) {
	got := NormalizeEnrollCode(" abcd-efgh-jklm ")
	if got != "ABCDEFGHJKLM" {
		t.Errorf("应去分隔符/空白并大写，得到 %q", got)
	}
	if NormalizeEnrollCode("ABCD EFGH\tJKLM") != "ABCDEFGHJKLM" {
		t.Error("应去除空格与制表符")
	}
}

// 回归：注册码的“签发侧哈希”与“核验侧哈希”必须一致，且对分隔符/大小写/空白不敏感。
// 曾经签发侧做了规范化、核验侧直接哈希原文，导致**所有**注册码都无法使用
// （注册码本身一定带 “-” 分隔符），也就是整个节点注册流程不可用。
func TestEnrollCodeHashBothSidesAgree(t *testing.T) {
	code, err := GenerateEnrollCode()
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}

	// 签发侧（后台生成时）算出的哈希。
	issued := EnrollCodeHash(code)

	// 核验侧可能收到的各种等价写法，必须算出同一个哈希。
	variants := []string{
		code,                               // ABCD-EFGH-JKLM
		strings.ToLower(code),              // abcd-efgh-jklm
		" " + code + " ",                   // 前后空格（shell 传参常见）
		strings.ReplaceAll(code, "-", ""),  // 漏掉分隔符
		strings.ReplaceAll(code, "-", " "), // 用空格替代分隔符
	}
	for _, v := range variants {
		if got := EnrollCodeHash(v); !bytes.Equal(got, issued) {
			t.Errorf("%q 的哈希与签发侧不一致（注册会失败）", v)
		}
	}

	// 反例：不同注册码必须得到不同哈希。
	other, err := GenerateEnrollCode()
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	if bytes.Equal(EnrollCodeHash(other), issued) {
		t.Error("不同注册码不应产生相同哈希")
	}
	// 且哈希长度符合 SHA-256。
	if len(issued) != 32 {
		t.Errorf("哈希应为 32 字节，得到 %d", len(issued))
	}
}

// 注册码格式校验：长度 12 且只含 Crockford 字符。
func TestIsValidEnrollCode(t *testing.T) {
	code, _ := GenerateEnrollCode()
	if !IsValidEnrollCode(NormalizeEnrollCode(code)) {
		t.Error("自己生成的注册码应通过校验")
	}
	bad := []string{
		"", "ABCDEFGHJKLMN", "ABCDEFGHJKL", // 长度不对
		"ABCDEFGHJKLI", "ABCDEFGHJKLO", "ABCDEFGHJKLU", // 含易混淆字符 I/L/O/U
		"abc-defg-hjkl",   // 未规范化（小写 + 分隔符）
		"ABCD-EFGH-JKLM!", // 含非法符号
		"ABCDEFGH-JKLM",   // 长度 13
	}
	for _, b := range bad {
		if IsValidEnrollCode(b) {
			t.Errorf("%q 不应通过校验", b)
		}
	}
	// 注意：L 不在 Crockford 字符集里，上面的 "ABCDEFGHJKLI" 同时触发两条规则。
}

// TOTP（RFC 6238）用规范中的测试向量校验算法正确性。
func TestTOTPMatchesRFC6238Vectors(t *testing.T) {
	// RFC 6238 附录 B 的 SHA1 密钥（20 字节 ASCII）。
	key := []byte("12345678901234567890")
	cases := []struct {
		counter int64
		want    string // 取 8 位向量的后 6 位（本实现输出 6 位）
	}{
		{1, "287082"},        // T=59
		{37037036, "081804"}, // T=1111111109
		{41152263, "005924"}, // T=1234567890
		{66666666, "279037"}, // T=2000000000
	}
	for _, c := range cases {
		if got := totp(key, c.counter); got != c.want {
			t.Errorf("totp(counter=%d) = %s，期望 %s", c.counter, got, c.want)
		}
	}
}

func TestVerifyTOTPRoundTrip(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("生成密钥失败：%v", err)
	}
	key, err := decodeBase32(secret)
	if err != nil {
		t.Fatalf("密钥应为合法 base32：%v", err)
	}
	now := time.Unix(1_760_000_000, 0)
	code := totp(key, now.Unix()/TOTPPeriod)

	if !VerifyTOTP([]byte(secret), code, now) {
		t.Fatalf("正确的动态码应通过校验（secret=%s code=%s）", secret, code)
	}
	// 允许 ±1 个时间窗的偏差
	if !VerifyTOTP([]byte(secret), code, now.Add(-TOTPPeriod*time.Second)) {
		t.Error("前一个窗口的动态码应被容忍")
	}
	if VerifyTOTP([]byte(secret), code, now.Add(10*TOTPPeriod*time.Second)) {
		t.Error("相隔 5 分钟的动态码不应通过")
	}
	if VerifyTOTP([]byte(secret), "000000", now) && code != "000000" {
		t.Error("错误动态码不应通过")
	}
	if VerifyTOTP([]byte(secret), "12345", now) {
		t.Error("长度不足 6 位应直接拒绝")
	}
	if VerifyTOTP(nil, code, now) {
		t.Error("未启用 TOTP（空密钥）时不应通过")
	}
}

// 9.3：argon2id 哈希必须加盐，且校验正确。
func TestPasswordHashAndVerify(t *testing.T) {
	const pw = "correct horse battery staple"
	h1, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("哈希失败：%v", err)
	}
	if h1 == pw || strings.Contains(h1, pw) {
		t.Fatal("哈希结果中不应出现明文")
	}
	if !strings.HasPrefix(h1, "$argon2id$") {
		t.Errorf("应为 argon2id 编码，得到 %q", h1)
	}
	if err := VerifyPassword(pw, h1); err != nil {
		t.Errorf("正确密码应通过：%v", err)
	}
	if err := VerifyPassword("wrong password", h1); err == nil {
		t.Error("错误密码不应通过")
	}

	// 同一密码两次哈希应不同（随机盐）
	h2, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("哈希失败：%v", err)
	}
	if h1 == h2 {
		t.Error("两次哈希相同，说明没有加盐")
	}
}

// 9.1：令牌为 256 位随机数、带 pk_ 前缀，库内只存 SHA-256 哈希。
func TestTokenGenerationAndHash(t *testing.T) {
	plain, hash, hint, err := GenerateToken()
	if err != nil {
		t.Fatalf("生成令牌失败：%v", err)
	}
	if !strings.HasPrefix(plain, "pk_") {
		t.Errorf("令牌应带 pk_ 前缀，得到 %q", plain)
	}
	if len(hash) != 32 {
		t.Errorf("SHA-256 应为 32 字节，得到 %d", len(hash))
	}
	if hint != plain[:6] {
		t.Errorf("提示应为前 6 位，得到 %q，令牌 %q", hint, plain)
	}
	if got := Hint(plain); got != hint {
		t.Errorf("Hint 不一致：%q vs %q", got, hint)
	}
	if !Equal(hash, HashToken(plain)) {
		t.Error("HashToken 结果应与生成时一致")
	}
	if Equal(HashToken(plain), HashToken(plain+"x")) {
		t.Error("不同令牌的哈希不应相同")
	}

	// 两次生成不应重复
	p2, _, _, _ := GenerateToken()
	if p2 == plain {
		t.Error("两次生成的令牌相同")
	}
}

func TestRandomHexLength(t *testing.T) {
	s, err := RandomHex(6)
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	if len(s) != 12 {
		t.Errorf("6 字节应为 12 位十六进制（公开标识要求 12 位），得到 %d：%q", len(s), s)
	}
	s2, _ := RandomHex(6)
	if s == s2 {
		t.Error("两次生成的随机值相同")
	}
}

// 9.3 / 13.4.5：会话 ID 与 CSRF 令牌。
func TestSessionIDAndCSRF(t *testing.T) {
	plain, idHash, err := NewSessionID()
	if err != nil {
		t.Fatalf("生成会话失败：%v", err)
	}
	if len(idHash) != 32 {
		t.Errorf("会话 ID 库里只存哈希（32 字节），得到 %d", len(idHash))
	}
	if strings.Contains(string(idHash), plain) {
		t.Error("库内不应存明文会话 ID")
	}

	csrf := GenerateCSRF(plain)
	if csrf == "" {
		t.Fatal("CSRF 令牌不应为空")
	}
	if !VerifyCSRF(csrf, csrf) {
		t.Error("相同 CSRF 令牌应校验通过")
	}
	if VerifyCSRF(csrf, "forged") {
		t.Error("伪造的 CSRF 令牌不应通过")
	}
	if VerifyCSRF(csrf, csrf+"x") {
		t.Error("长度不同的令牌不应通过")
	}
	// 令牌是 HMAC(会话ID) 派生，故意做成确定性的：服务端无需额外存储即可重算校验。
	// 因此同一会话必须稳定复现，否则 handleMe 回给前端的令牌会和库里存的对不上。
	if GenerateCSRF(plain) != csrf {
		t.Error("同一会话的 CSRF 令牌必须稳定复现")
	}
	// 但令牌必须绑定会话：换一个会话 ID 就必须换一个令牌。
	otherPlain, _, err := NewSessionID()
	if err != nil {
		t.Fatalf("生成第二个会话失败：%v", err)
	}
	if GenerateCSRF(otherPlain) == csrf {
		t.Error("不同会话的 CSRF 令牌不应相同（令牌必须绑定会话）")
	}
	// 令牌本身不得泄漏会话 ID（HMAC 输出必须与会话明文无关）。
	if csrf == plain || strings.Contains(csrf, plain) {
		t.Error("CSRF 令牌不得包含会话明文")
	}
}

// 9.4：渠道凭证用 AES-256-GCM 加密，密文不可直接读出，且篡改会被发现。
func TestKeyringEncryptDecrypt(t *testing.T) {
	dir := t.TempDir()
	kr, err := LoadOrCreateKeyring(dir + "/master.key")
	if err != nil {
		t.Fatalf("加载密钥失败：%v", err)
	}
	secret := []byte(`{"bot_token":"123456:ABC-DEF","chat_id":"-100123"}`)
	enc, err := kr.Encrypt(secret)
	if err != nil {
		t.Fatalf("加密失败：%v", err)
	}
	if strings.Contains(string(enc), "bot_token") {
		t.Fatal("密文中不应出现明文片段")
	}
	// GCM 的 nonce 随机，两次加密结果不同
	enc2, _ := kr.Encrypt(secret)
	if string(enc) == string(enc2) {
		t.Error("两次加密结果相同，nonce 可能未随机化")
	}
	plain, err := kr.Decrypt(enc)
	if err != nil {
		t.Fatalf("解密失败：%v", err)
	}
	if string(plain) != string(secret) {
		t.Errorf("解密结果不一致：%q", plain)
	}
	// 篡改密文应导致解密失败（认证标签不匹配）
	tampered := append([]byte(nil), enc...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := kr.Decrypt(tampered); err == nil {
		t.Error("被篡改的密文不应解密成功")
	}
}
