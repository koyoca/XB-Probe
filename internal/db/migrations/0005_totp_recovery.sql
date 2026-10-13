-- 0005_totp_recovery.sql
--
-- 6.2：TOTP 启用流程 + 一次性恢复码。
--
-- 背景：0001 里 admins.totp_secret 已经存在，登录侧 `handleLogin` 也早已会校验
-- （len(TOTPSecret) > 0 时要求动态码）。但**没有任何接口能写入这个字段**——
-- 也就是说 TOTP 实际上是个"永远打不开的开关"：功能全在，入口没做。
-- 更糟的是一旦有人用别的手段写进了密钥（比如直接改库），
-- 手机丢了就再也登不进后台，没有任何兜底通道。
--
-- 本次补两件事：
--   1. 启用/关闭 TOTP 的接口（见 internal/api/admin_totp.go）
--   2. 恢复码——一组一次性口令，用于动态验证器不可用时登录
--
-- 恢复码的存储设计（与 node_tokens 一致的安全口径）：
--   * 只存 SHA-256 哈希，库内**不存明文**——拿到库文件也拿不到可用恢复码；
--   * 明文凭据只在"生成/重置"那一次响应里返回，之后无法再取回；
--   * used_at 非空即已消费，登录时校验后立即置位（单次使用）；
--   * 重新生成会先删掉该管理员的全部旧码，避免"重置了但旧码还能用"。

CREATE TABLE admin_recovery_codes(
  id INTEGER PRIMARY KEY,
  admin_id INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
  code_hash BLOB NOT NULL,               -- SHA-256(规范化后的恢复码)
  created_at INTEGER NOT NULL,
  used_at INTEGER);                      -- 非空即已消费
CREATE INDEX idx_recovery_admin ON admin_recovery_codes(admin_id);
-- 登录时按哈希直查，需要唯一性保证"同一码不会被两个管理员共用"。
CREATE UNIQUE INDEX idx_recovery_hash ON admin_recovery_codes(code_hash);
