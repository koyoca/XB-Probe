// Package acme 是一个刻意最小化的 ACME v2（RFC 8555）客户端。
//
// 为什么不直接用 golang.org/x/crypto/acme/autocert：
//
//  1. 依赖代价。autocert 会经 golang.org/x/net/idna 拖进 golang.org/x/text
//     （国际化域名规范化）。本项目依赖刻意最小化（直接依赖 5 个、合计 18 个模块），
//     为一个只在"用域名申请证书"这一条路径上才用得到的规范化功能引入一个
//     文本处理库不划算。
//  2. IP 证书。autocert 把标识符写死成 dns（RFC 8555 的 identifier.type
//     固定 "dns"），拿不到 RFC 8738 的 "ip" 标识符。而"给一个只有公网 IP、
//     没有域名的节点签证书"正是本项目要支持的选择之一。
//  3. 可控。下单流程、挑战类型、续期时机全部显式可见，出问题时有日志可查。
//
// 实现范围（够用即止）：
//   - 账户：ES256（P-256）JWK，首次 newAccount 后记住 kid。
//   - 下单：newOrder → 逐个 authorization → 挑战应答 → finalize → 下载链。
//   - 挑战：http-01 与 tls-alpn-01 两种（DNS-01 需要各厂商 API，不在范围内，
//     这种情况请走"自定义证书"模式）。
//   - 标识符：dns 与 ip。
//   - 缓存：账户密钥、账户 URL、已签发证书落盘，重启不重签。
//
// 不做：证书吊销、密钥轮换、外部账户绑定（EAB，ZeroSSL 需要，见 ResolveDirectory 注释）。
package acme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
)

// b64 是 RFC 7515 的 base64url 无填充编码。ACME 全程使用它。
func b64(b []byte) string { return base64URL.EncodeToString(b) }

// jwkEC 是 P-256 公钥的 JWK 表示（RFC 7518 §6.2.1）。
//
// 字段顺序即 RFC 7638 要求的字典序（crv, kty, x, y），因此可以直接
// json.Marshal 得到规范化 JSON 去算指纹——顺序错了指纹就全错，
// 而 ACME 服务端会用自己算的指纹核对 keyAuthorization。
type jwkEC struct {
	Crv string `json:"crv"`
	Kty string `json:"kty"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func jwkFromPublic(pub *ecdsa.PublicKey) jwkEC {
	size := (pub.Curve.Params().BitSize + 7) / 8
	return jwkEC{
		Crv: "P-256",
		Kty: "EC",
		X:   b64(padBigInt(pub.X, size)),
		Y:   b64(padBigInt(pub.Y, size)),
	}
}

// padBigInt 把大整数左侧补零到固定字节宽度。
// P-256 的坐标是定长 32 字节；直接用 big.Int.Bytes() 会丢掉前导零，
// 得到一个 31 字节的 x/y，JWK 立刻不合法。
func padBigInt(n *big.Int, size int) []byte {
	b := n.Bytes()
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}

// thumbprint 是 JWK 的 SHA-256 指纹（RFC 7638），base64url 无填充。
func (k jwkEC) thumbprint() string {
	canonical, err := json.Marshal(k)
	if err != nil {
		// jwkEC 的字段都是纯 ASCII 字符串，Marshal 不会失败。
		panic("acme: JWK 无法序列化: " + err.Error())
	}
	sum := sha256.Sum256(canonical)
	return b64(sum[:])
}

// keyAuthorization 是挑战应答内容：token + "." + 账户密钥指纹（RFC 8555 §8.1）。
func keyAuthorization(token, thumbprint string) string {
	return token + "." + thumbprint
}

// jwsHeader 是 JWS 的受保护头。
//   - 首次 newAccount 用 jwk（尚未有 kid）；此后一律用 kid。
//   - POST-as-GET 请求（读 authz / order / 证书）payload 为空字符串。
type jwsHeader struct {
	Alg   string `json:"alg"`
	Nonce string `json:"nonce,omitempty"`
	URL   string `json:"url"`
	Kid   string `json:"kid,omitempty"`
	JWK   *jwkEC `json:"jwk,omitempty"`
}

// signJWS 产出一个 flattened JWS JSON（RFC 7515 §7.2.2）。
//
// payload 为 nil 表示 POST-as-GET：此时 payload 字段必须是空串，
// 而不是 "e30"（即 base64url 的 "{}"）——这一处写错会被服务端当成
// "用空对象触发挑战"从而误触发校验，或直接 malformed。
//
// 签名是 ES256：对 "protected.payload" 的 SHA-256 摘要做 ECDSA 签名，
// 再按 R || S 各 32 字节定长拼接（JWS 要定长，而 ecdsa.Sign 返回的是
// 去前导零的变长大整数）。
func signJWS(key *ecdsa.PrivateKey, hdr jwsHeader, payload []byte) (string, error) {
	protJSON, err := json.Marshal(hdr)
	if err != nil {
		return "", fmt.Errorf("序列化 JWS 头失败: %w", err)
	}
	protected := b64(protJSON)
	body := ""
	if payload != nil {
		body = b64(payload)
	}
	digest := sha256.Sum256([]byte(protected + "." + body))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("签名失败: %w", err)
	}
	size := (key.Curve.Params().BitSize + 7) / 8
	sig := make([]byte, 0, size*2)
	sig = append(sig, padBigInt(r, size)...)
	sig = append(sig, padBigInt(s, size)...)

	out := struct {
		Protected string `json:"protected"`
		Payload   string `json:"payload"`
		Signature string `json:"signature"`
	}{protected, body, b64(sig)}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("序列化 JWS 失败: %w", err)
	}
	return string(raw), nil
}

// newP256Key 生成一把 P-256 私钥（账户密钥与证书密钥都用它）。
func newP256Key() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}
