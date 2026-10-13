-- 0004_probe_show_in_bars.sql
--
-- 探测目标新增"是否在公开看板点阵中显示"开关（show_in_bars）。
--
-- 与 enabled 的区别：enabled 管"Agent 探不探"（停用后不再产生新样本），
-- show_in_bars 只管"点阵画不画这一行"。两者互补的典型场景：
--   - 专线目标只想让管理员在后台看，不想出现在访客的点阵里 → enabled=1, show_in_bars=0
--   - 目标临时停用排查 → enabled=0（历史桶在点阵聚合时会被一并过滤，不会画残影）
--
-- 默认 1：老库升级后所有目标保持"显示"，行为与升级前一致。
-- 聚合查询（LatencyBucketsForNode）同时按 enabled=1 AND show_in_bars=1 过滤，
-- 因此停用/隐藏目标的历史桶即使还留在 latency_10m 里也不会渲染出来。

ALTER TABLE probe_targets ADD COLUMN show_in_bars INTEGER NOT NULL DEFAULT 1;
