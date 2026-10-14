-- nodes.ip_v4_list / ip_v6_list：支持为一个节点登记多个 IPv4 / IPv6 地址。
--
-- 背景：原 ip_v4 / ip_v6 单列只能存一个地址。多 IP 节点（双栈 + 附加 IPv6、
-- 或一台机器绑了多个公网 IP）在管理端只能挤进一个字段，其余地址无处可记。
--
-- 设计：保留 ip_v4 / ip_v6 作为「主地址」（继续驱动公开卡片的协议栈标识
-- net_stack 与 SyncMeta 的 hasV4/hasV6），另开 ip_v4_list / ip_v6_list 文本列
-- 存放该族的全部地址（逗号分隔）。主地址恒等于列表的第一项（写入时由接口层
-- 保证），这样公开侧与协议栈判定只认主地址，不必改动公开 DTO 与 SyncMeta。
ALTER TABLE nodes ADD COLUMN ip_v4_list TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN ip_v6_list TEXT NOT NULL DEFAULT '';
