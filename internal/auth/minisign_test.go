package auth

import (
	"strings"
	"testing"

	"github.com/koyoca/xb-probe/internal/testfixture"
)

// minisign 验签的回归测试（6.8）。
//
// ★ 这一组测试里的公钥/签名/数据**全部由真实 minisign 0.11 生成**，
// 不是我自己按规范拼出来的。这个区别至关重要：
// 自研验签最容易犯的错不是"算法写错"，而是"格式读错"，
// 而按自己的理解拼测试数据只会把同样的误解固化进测试里（真验签时全挂）。
//
// 实际就撞到一个：签名文件的算法标识是 "ED"(0x4445)，
// 而公钥文件是 "Ed"(0x6445)。两边都按 0x6445 校验的话，
// 单测（用自造数据）能过，真实签名 100% 被拒。
//
// 固定数据本身住在 internal/testfixture：同一组字节串在
// cmd/xb-probe-master 的 SHA256SUMS 验签单测里也要用，各写一份必然漂移——
// 而且它是**签名过的**字节串，改一个字节就变成"看起来像安全事件"的假红。
// 重新生成的方法见那个包的头注释。
const (
	realMinisignPub  = testfixture.MinisignPub
	realMinisignSig  = testfixture.MinisignSig
	realMinisignData = testfixture.MinisignSums // 被签名的原文，逐字节一致
)

// TestMinisignVerifyRealSignature 是核心用例：真实签名必须验得过。
//
// 这条测试若挂，说明格式解析或预哈希方式不对——而不是"密码学算错了"。
func TestMinisignVerifyRealSignature(t *testing.T) {
	pub, err := ParseMinisignPublicKey(realMinisignPub)
	if err != nil {
		t.Fatalf("解析真实公钥失败：%v", err)
	}
	sig, err := ParseMinisignSignature(realMinisignSig)
	if err != nil {
		t.Fatalf("解析真实签名失败：%v", err)
	}
	if err := VerifyMinisign(pub, []byte(realMinisignData), sig); err != nil {
		t.Fatalf("真实签名验签失败（格式或预哈希有误）：%v", err)
	}
	// 便捷入口也必须一致。
	if err := VerifyMinisignText(realMinisignPub, []byte(realMinisignData), realMinisignSig); err != nil {
		t.Fatalf("VerifyMinisignText 失败：%v", err)
	}
}

// TestMinisignKeyIDParsing 锁定 key_id 的解析与大小端。
//
// 公钥注释行里是 hex `B56B8445221E5AEE`，而 base64 里是
// 小端存放的 8 字节 → 解出来应为 `EE5A1E2245846BB5`。
// 两处表示**字节序相反**，很容易搞混；这里把实际值固定下来。
func TestMinisignKeyIDParsing(t *testing.T) {
	pub, err := ParseMinisignPublicKey(realMinisignPub)
	if err != nil {
		t.Fatalf("解析公钥失败：%v", err)
	}
	if got := hexUpper(pub.KeyID[:]); got != "EE5A1E2245846BB5" {
		t.Errorf("公钥 key_id 解析错误：期望 EE5A1E2245846BB5，实际 %s", got)
	}
	sig, err := ParseMinisignSignature(realMinisignSig)
	if err != nil {
		t.Fatalf("解析签名失败：%v", err)
	}
	if got := hexUpper(sig.KeyID[:]); got != "EE5A1E2245846BB5" {
		t.Errorf("签名 key_id 与公钥不一致：%s", got)
	}
	if pub.KeyID != sig.KeyID {
		t.Errorf("公钥与签名的 key_id 应当相同")
	}
}

// TestMinisignSignatureAlgDiffersFromPublicKey 固定住那个"大小写陷阱"。
//
// 公钥 → 0x6445 ("Ed")，签名 → 0x4445 ("ED")。
// 若将来有人为了"统一"把两者改成同一个常量，这条会立刻失败并说明原因。
func TestMinisignSignatureAlgDiffersFromPublicKey(t *testing.T) {
	if minisignPubAlg == minisignSigAlg {
		t.Fatal("公钥与签名的算法标识必须不同（0x6445 vs 0x4445）——统一它们会让真实签名全部失效")
	}
	if minisignPubAlg != 0x6445 || minisignSigAlg != 0x4445 {
		t.Fatalf("算法标识被改动：pub=0x%04x sig=0x%04x", minisignPubAlg, minisignSigAlg)
	}
}

// TestMinisignRejectsTamperedData 篡改一个字节必须被发现。
func TestMinisignRejectsTamperedData(t *testing.T) {
	pub, _ := ParseMinisignPublicKey(realMinisignPub)
	sig, _ := ParseMinisignSignature(realMinisignSig)

	// 改一个字符（模拟"替换了某个产物的校验和"）。
	tampered := strings.Replace(realMinisignData, "aaa", "aab", 1)
	if err := VerifyMinisign(pub, []byte(tampered), sig); err == nil {
		t.Fatal("篡改后的内容竟然验签通过了——校验完全失效")
	}
	// 追加一个字节也要被发现（长度变了）。
	if err := VerifyMinisign(pub, []byte(realMinisignData+"x"), sig); err == nil {
		t.Fatal("追加内容后仍验签通过")
	}
}

// TestMinisignRejectsWrongKey 用另一把公钥验签必须报"密钥 ID 不匹配"。
//
// 这里刻意区分错误类型：key_id 不匹配与"内容被改"是两回事，
// 合并成一句"签名无效"会让排障时无从下手。
func TestMinisignRejectsWrongKey(t *testing.T) {
	// 另一把"格式完全合法但内容不同"的公钥：key_id = DEADBEEFDEADBEEF（32 字节全零公钥）。
	// 用程序生成可保证长度恰为 42 字节（手工拼 base64 极易差一两个字节）。
	other := `untrusted comment: minisign public key DEADBEEFDEADBEEF
RWTerb7v3q2+7wAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
`
	pub, err := ParseMinisignPublicKey(other)
	if err != nil {
		// 全零 key_id 与全零公钥在格式上完全合法，不该解析失败。
		t.Fatalf("解析占位公钥失败：%v", err)
	}
	sig, _ := ParseMinisignSignature(realMinisignSig)
	err = VerifyMinisign(pub, []byte(realMinisignData), sig)
	if err == nil {
		t.Fatal("用错误的公钥竟然验签通过")
	}
	if !strings.Contains(err.Error(), "密钥 ID 不匹配") {
		t.Errorf("错误信息应指出密钥 ID 不匹配，实际：%v", err)
	}
}

// TestMinisignRejectsMalformed 各种畸形输入必须返回错误而不是 panic。
func TestMinisignRejectsMalformed(t *testing.T) {
	pub, _ := ParseMinisignPublicKey(realMinisignPub)
	sig, _ := ParseMinisignSignature(realMinisignSig)

	cases := []struct {
		name string
		pub  string
		sig  string
	}{
		{"公钥为空", "", realMinisignSig},
		{"签名为空", realMinisignPub, ""},
		{"公钥只有注释行", "untrusted comment: hi\n", realMinisignSig},
		{"公钥不是 base64", "untrusted comment: hi\n!!!!\n", realMinisignSig},
		{"公钥长度不对", "untrusted comment: hi\nRWTuWh4iRYRr\n", realMinisignSig},
		{"签名不是 base64", realMinisignPub, "untrusted comment: x\n!!!!\n"},
		{"签名长度不对", realMinisignPub, "untrusted comment: x\nRUTuWh4i\n"},
		{"两者都空", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 只要求"不 panic 且返回错误"，不限定具体错误类型。
			if err := VerifyMinisignText(c.pub, []byte(realMinisignData), c.sig); err == nil {
				t.Errorf("畸形输入应当返回错误")
			}
		})
	}

	// nil 参数也不能 panic。
	if err := VerifyMinisign(nil, []byte("x"), sig); err == nil {
		t.Error("nil 公钥应返回错误")
	}
	if err := VerifyMinisign(pub, []byte("x"), nil); err == nil {
		t.Error("nil 签名应返回错误")
	}
}

// TestMinisignToleratesReformatting 现实中的文件常被编辑器改动，
// 解析必须对 CRLF / 多余空行 / 缺少注释行 都宽容。
func TestMinisignToleratesReformatting(t *testing.T) {
	// CRLF + 多余空行 + 末尾无换行。
	pub := "untrusted comment: minisign public key B56B8445221E5AEE\r\n\r\n" +
		"RWTuWh4iRYRrtRxnZ4S2i+5JWAetk3dpGiNA3qnSUK2yX23mNzaLGZtv"
	sig := "untrusted comment: signature from minisign secret key\r\n" +
		"RUTuWh4iRYRrtQJ80UjfRuItOPcFCinFlrs71EOoYfVFmKnY2Z68yRsg7Jy1ZFEzSq/oi5F3KTtLV49S7KQbJasfM2+Z1/hk8gI=\r\n" +
		"trusted comment: timestamp:1760000000 file:SHA256SUMS\r\n" +
		"2FKpRlaJSJobgpgklznVbZ4DuwoJn6S1+A0pAcbq9qk339/kmqyDKUwsYqpHSZCmJ1rgFyR78DrKne0Q1WH6Ag=="
	if err := VerifyMinisignText(pub, []byte(realMinisignData), sig); err != nil {
		t.Fatalf("对 CRLF / 空行的容忍不足：%v", err)
	}

	// 完全去掉注释行（有人会把 .minisig 当纯数据保存）。
	barePub := "RWTuWh4iRYRrtRxnZ4S2i+5JWAetk3dpGiNA3qnSUK2yX23mNzaLGZtv"
	bareSig := "RUTuWh4iRYRrtQJ80UjfRuItOPcFCinFlrs71EOoYfVFmKnY2Z68yRsg7Jy1ZFEzSq/oi5F3KTtLV49S7KQbJasfM2+Z1/hk8gI="
	if err := VerifyMinisignText(barePub, []byte(realMinisignData), bareSig); err != nil {
		t.Fatalf("无注释行的输入应当也能解析：%v", err)
	}
}

// TestMinisignTrustedCommentExposed 信任注释是可读出来展示的。
func TestMinisignTrustedCommentExposed(t *testing.T) {
	sig, err := ParseMinisignSignature(realMinisignSig)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !strings.Contains(sig.Trusted, "file:SHA256SUMS") {
		t.Errorf("信任注释未正确提取：%q", sig.Trusted)
	}
}

func hexUpper(b []byte) string {
	const digits = "0123456789ABCDEF"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}
