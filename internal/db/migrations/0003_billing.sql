-- 0003_billing.sql
-- 计费增强（对应设计文档 4.4 计费与流量）。
--
-- traffic_multiplier：流量倍率。部分机房按单方向计量、但账单按倍率扣量，
-- 或者只统计出向却按双向计费。约定：
--     计费流量 = 计量口径(traffic_mode 折算值) × traffic_multiplier
-- 默认 1.0 表示不做任何放大/缩小，因此老库升级后行为与升级前完全一致。
--
-- 注意：倍率只影响"计费口径"的用量与百分比，不改写 traffic_state /
-- traffic_daily 里的原始计量值——原始数据必须保持可审计，否则日后对不上账
-- 就再也无法还原。

ALTER TABLE node_billing ADD COLUMN traffic_multiplier REAL NOT NULL DEFAULT 1.0;
