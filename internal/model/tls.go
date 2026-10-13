package model

// TLSStatus 是证书与 TLS 的运行态描述（只读，供后台展示与排障）。
//
// 时间字段一律 Unix 秒（UTC），与项目其余时间字段保持同一单位约定。
// 它只描述"现在用的是哪张证书、什么时候该续期"，不含任何私钥或密钥材料。
type TLSStatus struct {
	// Mode 是配置里的 tls.mode：none / files / selfsigned / acme。
	Mode string `json:"mode"`
	// Source 是证书来源的人类可读描述：custom / self-signed / acme。
	Source string `json:"source"`
	// Challenge 是 ACME 挑战类型（仅 mode=acme 时有值）。
	Challenge string `json:"challenge,omitempty"`
	// Domains 是配置里声明的标识符（域名与 IP 的并集）。
	Domains []string `json:"domains"`
	// SANs 是从当前证书里读出来的实际 SAN 列表。
	SANs      []string `json:"sans"`
	Subject   string   `json:"subject"`
	Issuer    string   `json:"issuer"`
	NotBefore int64    `json:"not_before"`
	NotAfter  int64    `json:"not_after"`
	// SPKISHA256 是证书公钥的 SHA-256 指纹（base64），与安装命令里
	// install.spki_sha256 同源。续期时**必须复用同一把密钥**，否则
	// 已按指纹固定的 Agent 会全部连不上。
	SPKISHA256 string `json:"spki_sha256"`
	// SPKIHex 是同一指纹的冒号分隔大写十六进制（便于人工核对）。
	SPKIHex string `json:"spki_hex"`
	// NextRenewal 是计划续期时间（Unix 秒，0 表示不需要自动续期）。
	NextRenewal int64 `json:"next_renewal,omitempty"`
	// LastError 是最近一次申请/续期的错误（成功后清空）。
	LastError string `json:"last_error,omitempty"`
}

// TLSStatusReader 由证书提供方实现并注入接口层。
// 与 model.HeartbeatReader 同一套路：接口层只读快照，不反向依赖具体实现。
type TLSStatusReader interface {
	TLSStatus() TLSStatus
}
