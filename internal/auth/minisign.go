package auth

// minisign 签名校验（6.8 / 12.8）。
//
// 为什么自己实现而不是引第三方库：本项目「依赖最小化」的硬约束
// （见 `docs/05-实现细节与常见问题.md`），而 minisign 的密码学部分恰好**完全落在 Go 标准库里**——
// Ed25519 签名 + BLAKE2b-512 预哈希，两者都在 crypto/ 下。需要自己写的只是
// 「文本格式解析」（两个 base64 行 + 注释行）与「文件读取」，合计不到 200 行。
//
// ## minisign 的格式（与 Ed25519 裸签名有两点关键差异）
//
// 1. **签名对象是 BLAKE2b-512(file) 的结果，不是 file 本身**。
//    官方实现出于性能考虑对文件做流式预哈希（`sig_alg` = "ED" 表示预哈希模式，
//    即 "Ed25519 + prehashed"）。直接对文件字节做 Ed25519 会**永远验签失败**，
//    而错误信息只会说"签名无效"，看起来像密钥配错了——这是最容易踩的坑。
//
// 2. **公钥与签名都带 2 字节的小端算法标识前缀**：
//    公钥文件里 key_id 是 8 字节，签名文件里额外有 sig_alg(2) + key_id(8)，
//    这两个头**不参与** Ed25519 验签，验签对象就是后面 64 字节的裸签名。
//    如果把整行 base64 解码结果直接喂给 ed25519.Verify，长度不对会立刻报错。
//
// ## 本项目的定位
//
// Master 自身**不签名**（签名必须用离线私钥，绝不能在跑服务的机器上放着）。
// 这里的代码只做**验签**，供两处使用：
//   - 安装脚本 / `probe-master dist pull` 在拉取发布产物时校验 SHA256SUMS；
//   - 后台「发布文件」页在管理员上传后自检公钥配置是否正确。
// 真正的签名动作由维护者在离线环境执行（见 docs 的发布流程）。

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"golang.org/x/crypto/blake2b"
	"strings"
)

// minisign 的算法标识常量（小端）。
const (
	// 算法标识（小端）：**公钥与签名的两字节并不相同**，这是实测出来的。
	//
	//   - 公钥文件（.pub）里是 "Ed" → 0x6445
	//   - 签名文件（.minisig）里是 "ED" → 0x4445
	//
	// 字面意思都是"Ed25519 + 对预哈希值签名"，但第二个字节的大小写不同。
	// 一开始两边都按 0x6445 校验，结果**所有真实签名全部被拒**——
	// 幸好用真实 minisign 生成密钥对做了对拍，否则这个错误会在
	// "签名无效"的报错下被误判成密钥配错了。
	minisignPubAlg = 0x6445
	minisignSigAlg = 0x4445
)

// MinisignPublicKey 是解析后的 minisign 公钥。
type MinisignPublicKey struct {
	KeyID     [8]byte
	PublicKey ed25519.PublicKey
}

// ErrMinisignInvalid 表示签名或公钥格式非法（与"验签不通过"区分开）。
var ErrMinisignInvalid = errors.New("minisign 数据格式非法")

// ParseMinisignPublicKey 解析 minisign 公钥文本（通常是 .pub 文件的内容）。
//
// 格式：
//
//	untrusted comment: minisign public key <ID>
//	RWQf...（base64：2 字节算法标识 + 8 字节 key_id + 32 字节公钥）
//
// 第 2 行起才是数据；空行与后续的 trusted comment 行一律忽略。
func ParseMinisignPublicKey(text string) (*MinisignPublicKey, error) {
	line := firstDataLine(text)
	if line == "" {
		return nil, fmt.Errorf("%w：找不到公钥数据行", ErrMinisignInvalid)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(line))
	if err != nil {
		return nil, fmt.Errorf("%w：公钥不是合法 base64：%v", ErrMinisignInvalid, err)
	}
	// 2 (alg) + 8 (key id) + 32 (pubkey) = 42
	if len(raw) != 42 {
		return nil, fmt.Errorf("%w：公钥长度应为 42 字节，实际 %d", ErrMinisignInvalid, len(raw))
	}
	alg := binary.LittleEndian.Uint16(raw[0:2])
	// 官方公钥的 alg 是 "Ed"（0x6445）。这里做校验但只在明显不对时报错，
	// 以免将来 minisign 换标识时把全部用户挡在门外。
	if alg != minisignPubAlg {
		return nil, fmt.Errorf("%w：公钥算法标识未知（0x%04x）", ErrMinisignInvalid, alg)
	}
	pk := &MinisignPublicKey{}
	copy(pk.KeyID[:], raw[2:10])
	pk.PublicKey = ed25519.PublicKey(append([]byte(nil), raw[10:42]...))
	if len(pk.PublicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w：公钥不是 32 字节", ErrMinisignInvalid)
	}
	return pk, nil
}

// MinisignSignature 是解析后的 minisign 签名。
type MinisignSignature struct {
	KeyID     [8]byte
	Signature []byte // 64 字节裸 Ed25519 签名
	Trusted   string // trusted comment（仅展示用，不参与密码学校验）
}

// ParseMinisignSignature 解析 .minisig 文件内容。
//
// 格式（4 行，minisign 默认输出）：
//
//	untrusted comment: signature from minisign secret key
//	RWS...（base64：2 alg + 8 key_id + 64 sig）
//	trusted comment: timestamp:... file:SHA256SUMS
//	<base64：对 "sig||trusted_comment" 的全局签名，本实现忽略>
//
// ★ 解析要点：**不能先把 comment 行全过滤掉再取第 N 行**。
// 第 3 行才是 `trusted comment:`，而第 4 行是另一个 base64 数据行；
// 过滤掉注释后，"第 2 条数据行"是那个全局签名而不是信任注释。
// 这里按行扫描并记录"谁是第一个 base64 数据行、谁带 trusted comment 前缀"，
// 才对得上语义（这个错曾经让 Trusted 读出一串 base64）。
//
// 第 4 行是 minisign 的"信任注释签名"，用来防止 trusted comment 被篡改。
// 本项目的用途是校验 SHA256SUMS 的内容，而 trusted comment 只是展示信息，
// 所以这里**明确不校验**第 4 行，并在文档中说明——若将来要把 trusted comment
// 当安全依据（例如校验里面的时间戳），必须补上这一层的校验。
func ParseMinisignSignature(text string) (*MinisignSignature, error) {
	var (
		dataLine string // 第一个 base64 数据行（签名本体）
		trusted  string // trusted comment 的内容（若有）
	)
	for _, ln := range normalizedLines(text) {
		switch {
		case strings.HasPrefix(ln, "trusted comment:"):
			// 只取第一条，避免把末尾的全局签名行误当成又一条注释。
			if trusted == "" {
				trusted = strings.TrimSpace(strings.TrimPrefix(ln, "trusted comment:"))
			}
		case strings.HasPrefix(ln, "untrusted comment:"):
			// 跳过
		default:
			// 第一个非注释行即签名本体。
			if dataLine == "" {
				dataLine = ln
			}
		}
	}
	if dataLine == "" {
		return nil, fmt.Errorf("%w：找不到签名数据行", ErrMinisignInvalid)
	}
	raw, err := base64.StdEncoding.DecodeString(dataLine)
	if err != nil {
		return nil, fmt.Errorf("%w：签名不是合法 base64：%v", ErrMinisignInvalid, err)
	}
	// 2 (alg) + 8 (key id) + 64 (sig) = 74
	if len(raw) != 74 {
		return nil, fmt.Errorf("%w：签名长度应为 74 字节，实际 %d", ErrMinisignInvalid, len(raw))
	}
	alg := binary.LittleEndian.Uint16(raw[0:2])
	if alg != minisignSigAlg {
		return nil, fmt.Errorf("%w：签名算法标识未知（0x%04x，期望 0x%04x）",
			ErrMinisignInvalid, alg, minisignSigAlg)
	}
	sig := &MinisignSignature{Signature: append([]byte(nil), raw[10:74]...), Trusted: trusted}
	copy(sig.KeyID[:], raw[2:10])
	return sig, nil
}

// VerifyMinisign 校验 data 的 minisign 签名。
//
// 校验不通过返回带说明的 error（而不是 bool），因为调用方几乎总是要把原因
// 展示给用户或写进日志——"签名无效"和"密钥 ID 不匹配"对排障的意义完全不同。
func VerifyMinisign(pub *MinisignPublicKey, data []byte, sig *MinisignSignature) error {
	if pub == nil || sig == nil {
		return fmt.Errorf("%w：公钥或签名为空", ErrMinisignInvalid)
	}
	// key_id 不匹配是无歧义的失败信号：说明这份签名不是这把私钥签的。
	// 提前判掉可以避免把"拿错公钥"误报成"签名被篡改"。
	if pub.KeyID != sig.KeyID {
		return fmt.Errorf("签名密钥 ID 不匹配（公钥 %X，签名 %X）：这份发布产物不是由该公钥对应的私钥签名的",
			pub.KeyID, sig.KeyID)
	}
	// ★ 关键：minisign 签的是 BLAKE2b-512(file)，不是 file 本身。
	digest := blake2b.Sum512(data)
	if !ed25519.Verify(pub.PublicKey, digest[:], sig.Signature) {
		return errors.New("minisign 签名校验失败：内容与签名不符（可能被篡改或签名对应的是另一个文件）")
	}
	return nil
}

// VerifyMinisignText 是给配置了文本公钥/签名的调用方用的便捷入口。
func VerifyMinisignText(pubText string, data []byte, sigText string) error {
	pub, err := ParseMinisignPublicKey(pubText)
	if err != nil {
		return err
	}
	sig, err := ParseMinisignSignature(sigText)
	if err != nil {
		return err
	}
	return VerifyMinisign(pub, data, sig)
}

// firstDataLine 返回第一个非注释、非空的"数据行"。
//
// minisign 文件的注释行固定是 `untrusted comment: ` / `trusted comment: ` 开头。
// 只按"是否以这两个前缀开头"过滤太脆（用户可能手工去掉注释），
// 所以采用"跳过空行与以 comment 开头的行，取第一条其余内容"的宽松策略——
// 反正后面的 base64 解码会把真正格式错误的输入挡掉。
func firstDataLine(text string) string {
	lines := dataLines(text)
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

// normalizedLines 去掉行尾空白并丢弃空行（保留内容以便按前缀分派）。
func normalizedLines(text string) []string {
	var out []string
	for _, ln := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// dataLines 抽出去掉注释与空行后的数据行（供公钥解析使用）。
//
// 注意：**不要用它解析签名文件**——签名文件里既有 `trusted comment:`
// 又有第二个 base64 数据行，过滤掉注释后行号语义就错位了
// （ParseMinisignSignature 因此改用 normalizedLines 自行分派）。
func dataLines(text string) []string {
	var out []string
	for _, t := range normalizedLines(text) {
		if strings.HasPrefix(t, "untrusted comment:") || strings.HasPrefix(t, "trusted comment:") {
			continue
		}
		out = append(out, t)
	}
	return out
}
