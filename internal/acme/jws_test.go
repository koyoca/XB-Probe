package acme

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
)

// fixedKeyPKCS8 是一把固定的 P-256 私钥（PKCS#8 DER 的十六进制）。
// 用固定密钥才能把 JWK 指纹钉成常量；下面 expectedThumbprint 是用
// Python 独立算出来的（canonical JSON + SHA-256 + base64url 去填充），
// 不是本包代码的输出——这样它才能验出"字段顺序错了 / 少补了前导零 /
// 用了标准 base64 而不是 base64url"这三类错误。
const (
	fixedKeyPKCS8 = "308187020100301306072a8648ce3d020106082a8648ce3d030107046d306b0201010420" +
		"8e6f62d16cf4c3dd37967285b55ce6cafad0d0ddc001fabb7eb2767c871b3a76a144034200" +
		"045b755cbf86be213a2a14f899bba604ba6ea132492018ab4749ee161b26df1d1fac440ef0" +
		"a80ec404f846023a3f1156a9cbdef1cd81ba70762ec0f4c48dd28b37"

	expectedThumbprint = "KAYEPJOwwh6GNozHdtXBYr7EzuOmBCJsVU4Mq68xYSI"
	expectedJWKX       = "W3Vcv4a-IToqFPiZu6YEum6hMkkgGKtHSe4WGybfHR8"
	expectedJWKY       = "rEQO8KgOxAT4RgI6PxFWqcve8c2BunB2LsD0xI3Sizc"
)

func fixedKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	der, err := hex.DecodeString(fixedKeyPKCS8)
	if err != nil {
		t.Fatalf("解码固定密钥失败: %v", err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		t.Fatalf("解析固定密钥失败: %v", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("固定密钥类型应为 ECDSA，实际 %T", parsed)
	}
	return key
}

func TestJWKAndThumbprint(t *testing.T) {
	key := fixedKey(t)
	jwk := jwkFromPublic(&key.PublicKey)

	if jwk.Crv != "P-256" || jwk.Kty != "EC" {
		t.Fatalf("JWK 常量字段错误: crv=%q kty=%q", jwk.Crv, jwk.Kty)
	}
	if jwk.X != expectedJWKX {
		t.Fatalf("JWK.x 错误：期望 %s，得到 %s（多半是坐标没补前导零）", expectedJWKX, jwk.X)
	}
	if jwk.Y != expectedJWKY {
		t.Fatalf("JWK.y 错误：期望 %s，得到 %s", expectedJWKY, jwk.Y)
	}
	if got := jwk.thumbprint(); got != expectedThumbprint {
		t.Fatalf("JWK 指纹错误：期望 %s，得到 %s\n"+
			"（canonical JSON 必须严格按 crv,kty,x,y 顺序，且用 base64url 去填充）", expectedThumbprint, got)
	}
}

// TestThumbprintCanonicalJSON：指纹必须基于"只有 4 个字段、固定顺序"的
// 规范化 JSON。这里直接按 RFC 7638 手写一遍并比对，防止将来给 jwkEC
// 加字段（例如 kid）后指纹悄悄变化——那会让所有已签发的 keyAuthorization 失效。
func TestThumbprintCanonicalJSON(t *testing.T) {
	key := fixedKey(t)
	jwk := jwkFromPublic(&key.PublicKey)
	canonical := `{"crv":"P-256","kty":"EC","x":"` + jwk.X + `","y":"` + jwk.Y + `"}`
	sum := sha256.Sum256([]byte(canonical))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := jwk.thumbprint(); got != want {
		t.Fatalf("指纹与手写 canonical JSON 不一致：%s vs %s", got, want)
	}
	// 若 marshal 出的 JSON 顺序不是 crv,kty,x,y，上面这条就会失败；
	// 这里再显式断言一次，让失败原因更直白。
	raw, _ := json.Marshal(jwk)
	if string(raw) != canonical {
		t.Fatalf("JWK 序列化结果不是字典序 canonical JSON：%s", raw)
	}
}

func TestKeyAuthorizationFormat(t *testing.T) {
	token := "evaGxfADs6pSRb2LAv9IZf17Dt3juxGJ-PCt92wr-oA"
	got := keyAuthorization(token, expectedThumbprint)
	want := token + "." + expectedThumbprint
	if got != want {
		t.Fatalf("keyAuthorization 应为 token.thumbprint，得到 %q", got)
	}
	if !strings.HasPrefix(got, token+".") {
		t.Fatal("keyAuthorization 必须以 token 开头并以点分隔")
	}
}

// TestSignJWSVerifies：用独立实现验签（重新解析受保护头、重算签名输入、
// 用公钥验 ECDSA），而不是复用签名那段代码的中间结果。
func TestSignJWSVerifies(t *testing.T) {
	key := fixedKey(t)
	payload := []byte(`{"identifiers":[{"type":"dns","value":"a.example.com"}]}`)

	raw, err := signJWS(key, jwsHeader{
		Alg:   "ES256",
		Nonce: "abc123",
		URL:   "https://acme.example.com/new-order",
		Kid:   "https://acme.example.com/acct/1",
	}, payload)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}

	var jws struct {
		Protected string `json:"protected"`
		Payload   string `json:"payload"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal([]byte(raw), &jws); err != nil {
		t.Fatalf("JWS 不是合法 JSON: %v", err)
	}

	protJSON, err := base64.RawURLEncoding.DecodeString(jws.Protected)
	if err != nil {
		t.Fatalf("protected 不是 base64url: %v", err)
	}
	var hdr map[string]any
	if err := json.Unmarshal(protJSON, &hdr); err != nil {
		t.Fatalf("protected 不是 JSON: %v", err)
	}
	if hdr["alg"] != "ES256" {
		t.Fatalf("alg 应为 ES256，得到 %v", hdr["alg"])
	}
	if hdr["nonce"] != "abc123" {
		t.Fatalf("nonce 未正确写入受保护头: %v", hdr["nonce"])
	}
	if hdr["url"] != "https://acme.example.com/new-order" {
		t.Fatalf("url 未正确写入受保护头: %v", hdr["url"])
	}
	if hdr["kid"] != "https://acme.example.com/acct/1" {
		t.Fatalf("kid 未正确写入受保护头: %v", hdr["kid"])
	}
	if _, hasJWK := hdr["jwk"]; hasJWK {
		t.Fatal("使用 kid 时不应同时带 jwk")
	}

	gotPayload, err := base64.RawURLEncoding.DecodeString(jws.Payload)
	if err != nil {
		t.Fatalf("payload 不是 base64url: %v", err)
	}
	if string(gotPayload) != string(payload) {
		t.Fatalf("payload 往返不一致: %s", gotPayload)
	}

	sig, err := base64.RawURLEncoding.DecodeString(jws.Signature)
	if err != nil {
		t.Fatalf("signature 不是 base64url: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("ES256 签名必须是 64 字节定长（R||S），得到 %d 字节", len(sig))
	}
	digest := sha256.Sum256([]byte(jws.Protected + "." + jws.Payload))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
		t.Fatal("签名验签失败（定长编码或签名输入有误）")
	}
}

// TestSignJWSPostAsGetPayloadEmpty：POST-as-GET 的 payload 字段必须是空串，
// 而不是 "{}" 的 base64url（"e30"）。写错会被服务端当成"用空对象触发挑战"。
func TestSignJWSPostAsGetPayloadEmpty(t *testing.T) {
	key := fixedKey(t)
	raw, err := signJWS(key, jwsHeader{Alg: "ES256", Nonce: "n", URL: "u", Kid: "k"}, nil)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	var jws struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal([]byte(raw), &jws); err != nil {
		t.Fatalf("JWS 不是合法 JSON: %v", err)
	}
	if jws.Payload != "" {
		t.Fatalf("POST-as-GET 的 payload 应为空串，得到 %q", jws.Payload)
	}
}

// TestSignJWSUsesJWKWhenNoKid：账户尚未创建时只能带 jwk。
func TestSignJWSUsesJWKWhenNoKid(t *testing.T) {
	key := fixedKey(t)
	jwk := jwkFromPublic(&key.PublicKey)
	raw, err := signJWS(key, jwsHeader{Alg: "ES256", Nonce: "n", URL: "u", JWK: &jwk}, []byte("{}"))
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	var jws struct {
		Protected string `json:"protected"`
	}
	_ = json.Unmarshal([]byte(raw), &jws)
	protJSON, _ := base64.RawURLEncoding.DecodeString(jws.Protected)
	var hdr map[string]any
	_ = json.Unmarshal(protJSON, &hdr)
	if _, ok := hdr["jwk"]; !ok {
		t.Fatal("首次注册必须带 jwk")
	}
	if _, ok := hdr["kid"]; ok {
		t.Fatal("带 jwk 时不应同时带 kid")
	}
}

func TestIdentifierForAutoDetectsIP(t *testing.T) {
	cases := []struct {
		in    string
		wType string
		wVal  string
	}{
		{"Status.Example.COM", "dns", "status.example.com"},
		{"203.0.113.10", "ip", "203.0.113.10"},
		{"2001:db8::1", "ip", "2001:db8::1"},
		{"*.example.com", "dns", "*.example.com"},
	}
	for _, tc := range cases {
		got := IdentifierFor(tc.in)
		if got.Type != tc.wType || got.Value != tc.wVal {
			t.Fatalf("IdentifierFor(%q) = %v，期望 type=%s value=%s", tc.in, got, tc.wType, tc.wVal)
		}
	}
}

func TestResolveDirectory(t *testing.T) {
	cases := map[string]string{
		"":                                   LetsEncrypt,
		"letsencrypt":                        LetsEncrypt,
		"LE":                                 LetsEncrypt,
		"staging":                            LetsEncryptStaging,
		"letsencrypt-staging":                LetsEncryptStaging,
		"zerossl":                            ZeroSSL,
		"https://ca.internal/acme/directory": "https://ca.internal/acme/directory",
		"怪值":                                 LetsEncrypt,
	}
	for in, want := range cases {
		if got := ResolveDirectory(in); got != want {
			t.Fatalf("ResolveDirectory(%q) = %q，期望 %q", in, got, want)
		}
	}
}
