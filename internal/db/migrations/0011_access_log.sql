-- 0011_access_log.sql
-- 访问日志（后台"访问日志"页）与 IP 黑名单的存储位。
--
-- 为什么自建一张表而不是只读 Web 服务器日志：
--   本项目的形态是"Master 单进程对外"，前置 Caddy/Nginx 时访问日志在反代那边，
--   没有反代时就更没有地方存。管理员要回答的问题（谁在什么时候访问了什么、
--   哪些 IP 在扫路径）只能在应用层记。
--
-- ip 列存的是**RealIP 解析后的真实客户端地址**（不是 TCP 对端）：
-- 经 CDN/反代时对端是边缘节点，直接记 r.RemoteAddr 会把所有访问都记成
-- 那几个边缘 IP，日志完全失去意义。RealIP 已按 trusted_proxies + CDN 网段
-- 逐跳还原，这里只用结果。
CREATE TABLE IF NOT EXISTS access_log(
  id      INTEGER PRIMARY KEY,
  ts      INTEGER NOT NULL,
  ip      TEXT    NOT NULL,
  method  TEXT    NOT NULL,
  path    TEXT    NOT NULL,
  status  INTEGER NOT NULL,
  bytes   INTEGER NOT NULL DEFAULT 0,
  dur_ms  INTEGER NOT NULL DEFAULT 0,
  ua      TEXT
);

-- 列表按时间倒序翻页，清理按 ts 截断：两个索引分别服务这两条查询。
CREATE INDEX IF NOT EXISTS idx_access_log_ts ON access_log(ts);
-- 按 IP 过滤时走这个复合索引（ip 等值 + ts 范围）。
CREATE INDEX IF NOT EXISTS idx_access_log_ip ON access_log(ip, ts);
