-- 节点「探测失败重点监控」（6.x / #6）。
--
-- 在节点详情里勾选的探测目标：本节点探测该目标失败时立即告警，且**覆盖**全局
-- probe_fail 规则对 icmp_not_permitted / blocked_target 这类「非真实故障」错误的忽略
-- （容器 / LXC 节点用 ICMP 探测而 Agent 没有 raw socket 权限时，原本会被静默放过，
-- 表现为「明明在报错却从不告警」）。
--
-- 存成逗号分隔的目标 ID 列表；空串表示未开启重点监控（沿用全局规则）。
ALTER TABLE nodes ADD COLUMN alert_probe_targets TEXT NOT NULL DEFAULT '';
