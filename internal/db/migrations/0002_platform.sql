-- 0002_platform.sql
-- 一键安装与多平台适配（对应设计文档 12.7）。
-- 记录 Agent 运行环境，供后台展示与安装/卸载逻辑使用。

ALTER TABLE nodes ADD COLUMN os_family TEXT;         -- linux/darwin/freebsd/windows
ALTER TABLE nodes ADD COLUMN virt TEXT;              -- kvm/xen/lxc/openvz/docker/wsl/none
ALTER TABLE nodes ADD COLUMN service_manager TEXT;   -- systemd/openrc/procd/sysv/launchd/rcd/windows/none
ALTER TABLE nodes ADD COLUMN counter_bits INTEGER;   -- 32 或 64

ALTER TABLE enroll_codes ADD COLUMN created_at INTEGER;
ALTER TABLE enroll_codes ADD COLUMN created_by TEXT;
ALTER TABLE enroll_codes ADD COLUMN platform TEXT;   -- 生成命令时选择的平台，仅作记录
