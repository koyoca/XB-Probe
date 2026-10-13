package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/yourorg/probe/internal/model"
)

// 节点读写（6.3 / 5.2）。

const nodeCols = `id, public_id, name, group_name, sort_order, status, public_visible,
	COALESCE(ip_v4,''), COALESCE(ip_v6,''), COALESCE(provider,''), COALESCE(location,''), COALESCE(note,''),
	COALESCE(country,''),
	COALESCE(hostname,''), COALESCE(os,''), COALESCE(kernel,''), COALESCE(arch,''), COALESCE(cpu_model,''),
	COALESCE(cpu_cores,0), COALESCE(mem_total,0), COALESCE(swap_total,0), COALESCE(disk_total,0),
	COALESCE(boot_time,0), COALESCE(agent_version,''),
	COALESCE(os_family,''), COALESCE(virt,''), COALESCE(service_manager,''), COALESCE(counter_bits,0),
	last_seen, created_at, COALESCE(archived_at,0)`

func scanNode(sc interface{ Scan(...any) error }) (*model.Node, error) {
	n := &model.Node{}
	err := sc.Scan(&n.ID, &n.PublicID, &n.Name, &n.GroupName, &n.SortOrder, &n.Status, &n.PublicVisible,
		&n.IPv4, &n.IPv6, &n.Provider, &n.Location, &n.Note,
		&n.Country,
		&n.Hostname, &n.OS, &n.Kernel, &n.Arch, &n.CPUModel,
		&n.CPUCores, &n.MemTotal, &n.SwapTotal, &n.DiskTotal, &n.BootTime, &n.AgentVersion,
		&n.OSFamily, &n.Virt, &n.ServiceManager, &n.CounterBits,
		&n.LastSeen, &n.CreatedAt, &n.ArchivedAt)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// ListNodes 列出全部节点（管理端）。
func (r *Repo) ListNodes() ([]*model.Node, error) {
	rows, err := r.db.Read.Query(`SELECT ` + nodeCols + ` FROM nodes ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GetNode 按 ID 读取节点。
func (r *Repo) GetNode(id int64) (*model.Node, error) {
	n, err := scanNode(r.db.Read.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return n, err
}

// GetNodeByPublicID 按对外标识读取节点。
func (r *Repo) GetNodeByPublicID(publicID string) (*model.Node, error) {
	n, err := scanNode(r.db.Read.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE public_id=?`, publicID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return n, err
}

// CreateNode 创建节点。publicID 由调用方生成（随机 12 位，5.1）。
func (r *Repo) CreateNode(publicID, name, group string, now int64) (int64, error) {
	res, err := r.db.Write.Exec(
		`INSERT INTO nodes(public_id, name, group_name, status, created_at) VALUES(?,?,?,'offline',?)`,
		publicID, name, group, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// 初始化计费行，避免后续 upsert 语义混乱。
	if _, err := r.db.Write.Exec(
		`INSERT INTO node_billing(node_id) VALUES(?) ON CONFLICT(node_id) DO NOTHING`, id); err != nil {
		return 0, err
	}
	return id, nil
}

// UpdateNodeMeta 修改公开可见性等元数据（PATCH /nodes/{id}）。
func (r *Repo) UpdateNodeMeta(id int64, name, group string, sortOrder int, publicVisible bool, note string) error {
	_, err := r.db.Write.Exec(
		`UPDATE nodes SET name=?, group_name=?, sort_order=?, public_visible=?, note=? WHERE id=?`,
		name, group, sortOrder, boolToInt(publicVisible), note, id)
	return err
}

// UpdateNodeAssets 更新资产元数据（仅管理端，含真实 IP 的手动覆盖）。
//
// country 是按 model.NormalizeCountry 规范化后的两位国家代码（空串=未设置）；
// 它与 provider/location 同类，都属于"管理员手工维护、与 Agent 上报无关"的字段，
// 因此共用同一个写入口而不是单开一个 UPDATE —— 单开口子容易出现
// "改了国家却把机房写回旧值"这类部分更新事故。
func (r *Repo) UpdateNodeAssets(id int64, ipv4, ipv6, provider, location, country, note string) error {
	_, err := r.db.Write.Exec(
		`UPDATE nodes SET ip_v4=?, ip_v6=?, provider=?, location=?, country=?, note=? WHERE id=?`,
		nullIfEmpty(ipv4), nullIfEmpty(ipv6), provider, location, nullIfEmpty(country), note, id)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// UpdateNodeStatus 更新节点状态（状态机由 service 层驱动，4.1）。
func (r *Repo) UpdateNodeStatus(id int64, status string, lastSeen int64) error {
	_, err := r.db.Write.Exec(`UPDATE nodes SET status=?, last_seen=? WHERE id=?`, status, lastSeen, id)
	return err
}

// ArchiveNode 归档节点（停止告警与离线计时，保留历史）。
func (r *Repo) ArchiveNode(id, now int64) error {
	_, err := r.db.Write.Exec(
		`UPDATE nodes SET status='archived', archived_at=? WHERE id=?`, now, id)
	return err
}

// DeleteNode 删除节点；purge=true 时同时清除历史数据（后台分批清理，A7）。
func (r *Repo) DeleteNode(id int64, purge bool) error {
	if _, err := r.db.Write.Exec(`DELETE FROM nodes WHERE id=?`, id); err != nil {
		return err
	}
	if !purge {
		return nil
	}
	return r.PurgeNodeHistory(id, 5000)
}

// PurgeNodeHistory 分批删除节点的时序与延迟数据，避免长事务（A7 / 13.4.8）。
func (r *Repo) PurgeNodeHistory(nodeID int64, batch int) error {
	if batch <= 0 {
		batch = 5000
	}
	tables := []string{"metrics_raw", "metrics_1m", "metrics_10m"}
	for _, t := range tables {
		if err := r.purgeLoop(t, `node_id=?`, nodeID, batch); err != nil {
			return err
		}
	}
	// 延迟表需先收集 target_id 对应的行，直接按 node_id 删即可。
	for _, t := range []string{"latency_raw", "latency_10m"} {
		if err := r.purgeLoop(t, `node_id=?`, nodeID, batch); err != nil {
			return err
		}
	}
	return nil
}

// purgeLoop 按主键分批删除某节点的历史行。
//
// ★ 旧实现写的是 `DELETE FROM t WHERE rowid IN (SELECT rowid FROM t WHERE node_id=? LIMIT n)`，
// 但 metrics_raw/1m/10m、latency_raw/10m 都是 `PRIMARY KEY(node_id, ts) WITHOUT ROWID`，
// 没有 rowid 伪列 —— 于是**归档/删除节点时清理历史永远失败**（`no such column: rowid`）。
// 与 CleanupTimeSeries 是同一处根因，两处必须一起改，否则修一个漏一个。
//
// 改为按主键分批：先 SELECT 出一个比当前所有待删行都大的 (node_id, ts) 上界中的 ts，
// WHERE 直接落在主键第二列上可走索引范围扫描，且上界每轮严格推进，
// 不会出现"LIMIT 反复取到同一批"的死循环。（node_id 已由 where 参数固定，这里只按 ts 推进。）
func (r *Repo) purgeLoop(table, where string, arg any, batch int) error {
	for {
		// 上界取"当前待删行里最大的 ts"，删完这批再重新取，天然向前推进。
		var next sql.NullInt64
		err := r.db.Read.QueryRow(
			fmt.Sprintf(`SELECT MAX(ts) FROM (SELECT ts FROM %s WHERE %s ORDER BY ts LIMIT %d)`,
				table, where, batch), arg).Scan(&next)
		if err != nil {
			return err
		}
		if !next.Valid {
			return nil // 没有待删行了
		}
		res, err := r.db.Write.Exec(
			fmt.Sprintf(`DELETE FROM %s WHERE %s AND ts <= ?`, table, where), arg, next.Int64)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

// UpdateNodeStatic 更新 Agent 上报的静态信息与环境字段（12.7）。
// bootTime 取自最新样本的 metrics.boot_time。
//
// **契约：只允许上报路径（/api/agent/v1/report）调用，且载荷必须是完整的 Static。**
// 本函数是无条件写全 15 列——传入 Static 里没填的字段会被写成 0 或空串。
// 而入网请求（/api/agent/v1/enroll）的载荷里只有 hostname/os/arch/virt 等环境信息，
// 根本没有 cpu_cores/mem_total/disk_total/kernel/cpu_model。早先入网路径误用了本函数，
// 结果是**每次重新入网都把已入库的资源规格清零**：节点明明在线，卡片上却是 0 核 / 0 B 总量。
// 入网路径请改用 UpdateNodeEnv。
func (r *Repo) UpdateNodeStatic(id int64, s *model.Static, osFamily, serviceManager string, counterBits int, bootTime int64) error {
	_, err := r.db.Write.Exec(
		`UPDATE nodes SET hostname=?, os=?, kernel=?, arch=?, cpu_model=?, cpu_cores=?,
		 mem_total=?, swap_total=?, disk_total=?, boot_time=?, agent_version=?,
		 os_family=?, virt=?, service_manager=?, counter_bits=?
		 WHERE id=?`,
		s.Hostname, s.OS, s.Kernel, s.Arch, s.CPUModel, s.CPUCores,
		s.MemTotal, s.SwapTotal, s.DiskTotal, bootTime, s.AgentVersion,
		osFamily, s.Virt, serviceManager, counterBits, id)
	return err
}

// UpdateNodeEnv 只写入"运行环境"类字段，供入网路径使用（12.2）。
//
// 与 UpdateNodeStatic 的区别：不碰 kernel / cpu_model / cpu_cores / mem_total /
// swap_total / disk_total / boot_time —— 这些是资源规格，入网时 Agent 还没采集，
// 一旦写入就是把好数据清零。资源规格统一等上报路径（完整 Static）来写。
func (r *Repo) UpdateNodeEnv(id int64, hostname, osName, osFamily, arch, virt, serviceManager, agentVersion string, counterBits int) error {
	_, err := r.db.Write.Exec(
		`UPDATE nodes SET hostname=?, os=?, os_family=?, arch=?, virt=?,
		 service_manager=?, agent_version=?, counter_bits=?
		 WHERE id=?`,
		hostname, osName, osFamily, arch, virt, serviceManager, agentVersion, counterBits, id)
	return err
}

// UpdateNodeIPs 更新来源地址（默认取上报连接的对端地址，可手动覆盖）。
func (r *Repo) UpdateNodeIPs(id int64, ip string) error {
	if ip == "" {
		return nil
	}
	var col string
	if strings.Contains(ip, ":") {
		col = "ip_v6"
	} else {
		col = "ip_v4"
	}
	_, err := r.db.Write.Exec(`UPDATE nodes SET `+col+`=? WHERE id=? AND (`+col+` IS NULL OR `+col+`='')`, ip, id)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
