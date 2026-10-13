-- 0001_init.sql
-- 初始 schema。对应设计文档 6.2 / 6.3 / 6.4 / 6.5 / 13.4.1。
-- 通用约定（6.1）：时间为 Unix 秒(UTC)；金额为最小货币单位整数；百分比与负载 ×100；
-- 布尔为 0/1；时序表 WITHOUT ROWID + 复合主键 (node_id, ts)。
-- PRAGMA foreign_keys=ON 由连接串开启（见 internal/db/db.go）。

-- ============ 6.2 系统与认证 ============
-- 注：schema_migrations 由迁移执行器在引导阶段创建（internal/db/migrate.go）。

CREATE TABLE settings(
  key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE admins(
  id INTEGER PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  pw_hash TEXT NOT NULL,                 -- argon2id
  totp_secret BLOB,                      -- 可选二次验证，加密存储
  failed_count INTEGER NOT NULL DEFAULT 0,
  locked_until INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL);

CREATE TABLE sessions(
  id_hash BLOB PRIMARY KEY,              -- SHA-256(会话 ID)，库内不存明文
  admin_id INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
  csrf_token TEXT NOT NULL,
  ip TEXT, user_agent TEXT,
  created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL);
CREATE INDEX idx_sessions_exp ON sessions(expires_at);

CREATE TABLE audit_log(
  id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, actor TEXT NOT NULL,
  action TEXT NOT NULL, target TEXT, ip TEXT);

-- ============ 6.3 节点、令牌与探测目标 ============
CREATE TABLE nodes(
  id INTEGER PRIMARY KEY,
  public_id TEXT NOT NULL UNIQUE,        -- 随机 12 位，对外标识
  name TEXT NOT NULL,                    -- 公开显示名（脱敏）
  group_name TEXT NOT NULL DEFAULT '',
  sort_order INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'offline'
    CHECK(status IN ('online','warning','offline','archived')),
  public_visible INTEGER NOT NULL DEFAULT 1,
  -- 敏感资产信息（仅管理端可见）
  ip_v4 TEXT, ip_v6 TEXT,                -- 默认取上报连接的来源地址，可手动覆盖
  provider TEXT, location TEXT, note TEXT,
  -- Agent 上报的静态信息
  hostname TEXT, os TEXT, kernel TEXT, arch TEXT, cpu_model TEXT, cpu_cores INTEGER,
  mem_total INTEGER, swap_total INTEGER, disk_total INTEGER, boot_time INTEGER,
  agent_version TEXT,
  last_seen INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, archived_at INTEGER);

CREATE TABLE node_tokens(
  id INTEGER PRIMARY KEY,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  token_hash BLOB NOT NULL UNIQUE,       -- SHA-256(令牌)，库内不存明文
  token_hint TEXT NOT NULL,              -- 令牌前 6 位，仅供后台辨认
  created_at INTEGER NOT NULL,
  last_used_at INTEGER, last_ip TEXT,
  revoked_at INTEGER);                   -- 非空即已吊销
CREATE INDEX idx_tokens_node ON node_tokens(node_id);

CREATE TABLE enroll_codes(
  id INTEGER PRIMARY KEY,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  code_hash BLOB NOT NULL UNIQUE,
  expires_at INTEGER NOT NULL, used_at INTEGER);

CREATE TABLE probe_targets(
  id INTEGER PRIMARY KEY,
  node_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,  -- NULL 表示全局目标
  name TEXT NOT NULL,
  carrier TEXT NOT NULL DEFAULT 'custom'
    CHECK(carrier IN ('telecom','unicom','mobile','custom')),
  host TEXT NOT NULL,
  port INTEGER,                          -- method=tcp 时必填
  method TEXT NOT NULL DEFAULT 'tcp' CHECK(method IN ('tcp','icmp')),
  timeout_ms INTEGER NOT NULL DEFAULT 2000,
  enabled INTEGER NOT NULL DEFAULT 1);

-- ============ 6.4 计费、流量与资产 ============
CREATE TABLE node_billing(
  node_id INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  price_minor INTEGER, currency TEXT,
  cycle TEXT NOT NULL DEFAULT 'monthly'
    CHECK(cycle IN ('monthly','quarterly','yearly','once','lifetime')),
  expire_at INTEGER,                     -- lifetime 为空
  traffic_quota INTEGER NOT NULL DEFAULT 0,   -- 字节，0 = 不限
  traffic_mode TEXT NOT NULL DEFAULT 'both'
    CHECK(traffic_mode IN ('both','up','down','max')),
  reset_day INTEGER NOT NULL DEFAULT 1 CHECK(reset_day BETWEEN 1 AND 31),
  tz TEXT);                              -- 空 = 使用全局时区

CREATE TABLE traffic_state(
  node_id INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  period_start INTEGER NOT NULL,
  period_rx INTEGER NOT NULL DEFAULT 0,  -- 当前周期已用
  period_tx INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL);

CREATE TABLE traffic_daily(
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  day TEXT NOT NULL,                     -- YYYY-MM-DD（节点时区）
  rx INTEGER NOT NULL DEFAULT 0, tx INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(node_id, day)) WITHOUT ROWID;

-- 13.4.1：各网卡累计值基线（取代 v1 的“全网卡求和”做法，修复 A1）
CREATE TABLE traffic_nic_state(
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  nic TEXT NOT NULL,
  last_rx INTEGER NOT NULL, last_tx INTEGER NOT NULL,
  boot_time INTEGER, updated_at INTEGER NOT NULL,
  PRIMARY KEY(node_id, nic)) WITHOUT ROWID;

-- ============ 6.5 时序数据与告警 ============
-- 指标表：metrics_raw(10 秒)、metrics_1m、metrics_10m 三张表结构相同
CREATE TABLE metrics_raw(
  node_id INTEGER NOT NULL, ts INTEGER NOT NULL,  -- ts = 桶起点
  cpu INTEGER, cpu_max INTEGER,                   -- 百分比 ×100
  load1 INTEGER,                                  -- ×100
  mem_used INTEGER, swap_used INTEGER, disk_used INTEGER,   -- 字节
  rx_bps INTEGER, tx_bps INTEGER,
  rx_bps_max INTEGER, tx_bps_max INTEGER,
  conns INTEGER,
  PRIMARY KEY(node_id, ts)) WITHOUT ROWID;

CREATE TABLE metrics_1m(
  node_id INTEGER NOT NULL, ts INTEGER NOT NULL,
  cpu INTEGER, cpu_max INTEGER,
  load1 INTEGER,
  mem_used INTEGER, swap_used INTEGER, disk_used INTEGER,
  rx_bps INTEGER, tx_bps INTEGER,
  rx_bps_max INTEGER, tx_bps_max INTEGER,
  conns INTEGER,
  PRIMARY KEY(node_id, ts)) WITHOUT ROWID;

CREATE TABLE metrics_10m(
  node_id INTEGER NOT NULL, ts INTEGER NOT NULL,
  cpu INTEGER, cpu_max INTEGER,
  load1 INTEGER,
  mem_used INTEGER, swap_used INTEGER, disk_used INTEGER,
  rx_bps INTEGER, tx_bps INTEGER,
  rx_bps_max INTEGER, tx_bps_max INTEGER,
  conns INTEGER,
  PRIMARY KEY(node_id, ts)) WITHOUT ROWID;

-- 延迟表：latency_raw（探测周期原始点）、latency_10m 两张表结构相同
CREATE TABLE latency_raw(
  node_id INTEGER NOT NULL, target_id INTEGER NOT NULL, ts INTEGER NOT NULL,
  rtt_avg INTEGER, rtt_min INTEGER, rtt_max INTEGER,  -- 微秒；全部失败为 NULL
  loss INTEGER NOT NULL DEFAULT 0,                    -- 丢包率 ×100（0~10000）
  PRIMARY KEY(node_id, target_id, ts)) WITHOUT ROWID;

CREATE TABLE latency_10m(
  node_id INTEGER NOT NULL, target_id INTEGER NOT NULL, ts INTEGER NOT NULL,
  rtt_avg INTEGER, rtt_min INTEGER, rtt_max INTEGER,
  loss INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(node_id, target_id, ts)) WITHOUT ROWID;

CREATE TABLE alert_channels(
  id INTEGER PRIMARY KEY,
  type TEXT NOT NULL CHECK(type IN ('telegram','smtp')),
  name TEXT NOT NULL,
  config_enc BLOB NOT NULL,              -- AES-GCM 加密的 JSON（Bot Token、SMTP 密码等）
  enabled INTEGER NOT NULL DEFAULT 1);

CREATE TABLE alert_rules(
  kind TEXT PRIMARY KEY,                 -- offline/cpu/mem/disk/traffic/expire/probe_fail
  enabled INTEGER NOT NULL DEFAULT 1,
  params TEXT NOT NULL,                  -- JSON：阈值、持续时间、确认次数、梯级
  cooldown_s INTEGER NOT NULL DEFAULT 3600);

CREATE TABLE alert_state(
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,                    -- 如 offline、traffic:80、expire:7
  state TEXT NOT NULL CHECK(state IN ('ok','firing')),
  since INTEGER NOT NULL, last_sent_at INTEGER NOT NULL DEFAULT 0, detail TEXT,
  PRIMARY KEY(node_id, kind)) WITHOUT ROWID;

CREATE TABLE alert_events(
  id INTEGER PRIMARY KEY,
  node_id INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
  kind TEXT NOT NULL, level TEXT NOT NULL, message TEXT NOT NULL,
  created_at INTEGER NOT NULL, delivered INTEGER NOT NULL DEFAULT 0);
CREATE INDEX idx_alert_events_time ON alert_events(created_at);
