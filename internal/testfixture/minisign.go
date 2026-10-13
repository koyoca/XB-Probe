// Package testfixture 存放**被密码学签名过的**测试固定数据。
//
// 单独成包的理由与 internal/netutil 完全一样：同一组字节串有两个使用者
// （internal/auth 的 minisign 验签单测、cmd/xb-probe-master 的 SHA256SUMS
// 验签单测），各写一份必然漂移。
//
// 而它比普通常量更脆：改一个字节，真实签名就再也验不过，报错还是
// "内容与签名不符（可能被篡改或签名对应的是另一个文件）"——看起来像一次
// 安全事件，实际是自己改坏了测试数据。第 14 轮的产品改名
// （probe-agent → xb-probe-agent）就是这么撞上的：批量替换把两处固定数据里
// 的产物名一起换掉了，两边的真实签名同时失效。
//
// ⚠ 本包里的任何字符串都**不得**参与批量改名。需要跟着新版二进制名走的话，
// 必须按下面的命令**重新签一份**（与 docs 的发布流程一致）：
//
//	minisign -G -p ms.pub -s ms.key -W
//	printf 'aaa  probe-agent_linux_amd64\nbbb  probe-agent_windows_amd64.exe\n' > SHA256SUMS
//	minisign -S -s ms.key -m SHA256SUMS -t "timestamp:1760000000 file:SHA256SUMS"
//
// 本包只被 _test.go 引用，不会进入任何生产二进制。
package testfixture

// 下面三个常量由真实的 minisign 0.11 生成，不是按规范手拼的。
//
// 手拼数据会把"自己理解的格式"固化进测试里：自研验签最容易错的不是算法，
// 而是格式——签名文件的算法标识是 "ED"(0x4445)、公钥文件是 "Ed"(0x6445)，
// 两边都按 0x6445 校验时，用自造数据的单测能过，真实验签 100% 被拒。
const (
	MinisignPub = `untrusted comment: minisign public key B56B8445221E5AEE
RWTuWh4iRYRrtRxnZ4S2i+5JWAetk3dpGiNA3qnSUK2yX23mNzaLGZtv
`

	// MinisignSums 是**被签名的原文**，逐字节一致（含末尾换行）。
	MinisignSums = "aaa  probe-agent_linux_amd64\nbbb  probe-agent_windows_amd64.exe\n"

	MinisignSig = `untrusted comment: signature from minisign secret key
RUTuWh4iRYRrtQJ80UjfRuItOPcFCinFlrs71EOoYfVFmKnY2Z68yRsg7Jy1ZFEzSq/oi5F3KTtLV49S7KQbJasfM2+Z1/hk8gI=
trusted comment: timestamp:1760000000 file:SHA256SUMS
2FKpRlaJSJobgpgklznVbZ4DuwoJn6S1+A0pAcbq9qk339/kmqyDKUwsYqpHSZCmJ1rgFyR78DrKne0Q1WH6Ag==
`
)
