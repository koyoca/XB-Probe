package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/koyoca/xb-probe/internal/model"
	"github.com/koyoca/xb-probe/internal/netutil"
)

// 节点读写（6.3 / 5.2）。

const nodeCols = `id, public_id, name, group_name, sort_order, status, public_visible,
	COALESCE(ip_v4,''), COALESCE(ip_v6,''), COALESCE(provider,''), COALESCE(location,''), COALESCE(note,''),
	COALESCE(country,''), COALESCE(ip_auto,1),
	COALESCE(ip_v4_list,''), COALESCE(ip_v6_list,''),
	COALESCE(hostname,''), COALESCE(os,''), COALESCE(kernel,''), COALESCE(arch,''), COALESCE(cpu_model,''),
	COALESCE(cpu_cores,0), COALESCE(mem_total,0), COALESCE(swap_total,0), COALESCE(disk_total,0),
	COALESCE(boot_time,0), COALESCE(agent_version,''),
	COALESCE(os_family,''), COALESCE(virt,''), COALESCE(service_manager,''), COALESCE(counter_bits,0),
	last_seen, created_at, COALESCE(archived_at,0),
	COALESCE(alert_probe_targets,'')`

func scanNode(sc interface{ Scan(...any) error }) (*model.Node, error) {
	n := &model.Node{}
	var csv4, csv6, csvA string
	err := sc.Scan(&n.ID, &n.PublicID, &n.Name, &n.GroupName, &n.SortOrder, &n.Status, &n.PublicVisible,
		&n.IPv4, &n.IPv6, &n.Provider, &n.Location, &n.Note,
		&n.Country, &n.IPAuto,
		&csv4, &csv6,
		&n.Hostname, &n.OS, &n.Kernel, &n.Arch, &n.CPUModel,
		&n.CPUCores, &n.MemTotal, &n.SwapTotal, &n.DiskTotal, &n.BootTime, &n.AgentVersion,
		&n.OSFamily, &n.Virt, &n.ServiceManager, &n.CounterBits,
		&n.LastSeen, &n.CreatedAt, &n.ArchivedAt,
		&csvA)
	if err != nil {
		return nil, err
	}
	n.IPv4List = splitIPList(csv4)
	n.IPv6List = splitIPList(csv6)
	n.AlertProbeTargets = splitIntList(csvA)
	return n, nil
}

// splitIPList 把逗号分隔的地址串解析为切片（去空白、去空项、去重、保序）。
func splitIPList(s string) []string {
	if s == "" {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, p := range strings.Split(s, ",") {
		v := strings.TrimSpace(p)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// joinIPList 把地址切片规整为逗号分隔的存储串（去空白、去空项、去重、保序）。
func joinIPList(list []string) string {
	return strings.Join(splitIPList(strings.Join(list, ",")), ",")
}

// splitIntList 把逗号分隔的整数串解析为切片（去空白、去空项、去重、保序，
// 忽略非法值与非正数）。用于「重点监控的探测目标 ID 清单」这类整型清单列。
func splitIntList(s string) []int64 {
	if s == "" {
		return nil
	}
	seen := map[int64]bool{}
	out := make([]int64, 0)
	for _, p := range strings.Split(s, ",") {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// joinIntList 把整型清单规整为逗号分隔的存储串（去重、保序、丢弃非正数）。
func joinIntList(list []int64) string {
	if len(list) == 0 {
		return ""
	}
	seen := map[int64]bool{}
	parts := make([]string, 0, len(list))
	for _, id := range list {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
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
//
// ip_auto（"这个地址是手填的还是自动探测的"）由这里推导，规则三条：
//  1. 两个地址都留空 → 恢复自动（1）。"清空"就是"我不想指定了"的自然表达，
//     也给了管理员一条"把自动探测重新打开"的路。
//  2. 提交的值与库里**不同** → 管理员确实改过 → 锁定（0）。
//     这就是面板上「覆盖自动探测」四个字的字面兑现。
//  3. 提交的值与库里**相同** → 只是编辑面板的回填（打开面板会把当前值带出来）
//     → 保持原值。少了这一条，打开节点面板点一次保存就会**永久关掉自动探测**，
//     而界面上不会有任何提示 —— 典型的"点一下什么都没变，但功能没了"。
func (r *Repo) UpdateNodeAssets(id int64, ipv4, ipv6, provider, location, country, note string) error {
	cur4, cur6 := "", ""
	_ = r.db.Read.QueryRow(
		`SELECT COALESCE(ip_v4,''), COALESCE(ip_v6,'') FROM nodes WHERE id=?`, id).Scan(&cur4, &cur6)

	ipAuto := 1
	switch {
	case ipv4 == "" && ipv6 == "":
		ipAuto = 1
	case ipv4 == cur4 && ipv6 == cur6:
		// 保持原值：只读了 ip_v4/ip_v6，ip_auto 原样不动。
		_, err := r.db.Write.Exec(
			`UPDATE nodes SET ip_v4=?, ip_v6=?, provider=?, location=?, country=?, note=? WHERE id=?`,
			nullIfEmpty(ipv4), nullIfEmpty(ipv6), provider, location, nullIfEmpty(country), note, id)
		return err
	default:
		ipAuto = 0
	}
	_, err := r.db.Write.Exec(
		`UPDATE nodes SET ip_v4=?, ip_v6=?, provider=?, location=?, country=?, note=?, ip_auto=? WHERE id=?`,
		nullIfEmpty(ipv4), nullIfEmpty(ipv6), provider, location, nullIfEmpty(country), note, ipAuto, id)
	return err
}

// UpdateNodeIPLists 写入某个节点的多地址清单（逗号分隔存储，写入口统一规整）。
//
// 主地址（ip_v4 / ip_v6）不在这里改：它由 UpdateNodeAssets 负责，且恒等于列表的
// 第一项（接口层保证），二者不在此耦合，避免"改了清单却把主地址写回旧值"这类
// 部分更新事故（与 UpdateNodeAssets 共用同一写入口的思路同源）。
func (r *Repo) UpdateNodeIPLists(id int64, ipv4List, ipv6List []string) error {
	_, err := r.db.Write.Exec(
		`UPDATE nodes SET ip_v4_list=?, ip_v6_list=? WHERE id=?`,
		joinIPList(ipv4List), joinIPList(ipv6List), id)
	return err
}

// UpdateNodeAlertTargets 写入某节点的「重点监控探测目标」清单（逗号分隔存储）。
//
// 单独的写入口：它与多地址清单一样是节点私有配置，由接口层的 PATCH 单独提交，
// 不与 UpdateNodeMeta/UpdateNodeAssets 混在一起，避免「改个名字却把监控清单写空」。
func (r *Repo) UpdateNodeAlertTargets(id int64, targets []int64) error {
	_, err := r.db.Write.Exec(
		`UPDATE nodes SET alert_probe_targets=? WHERE id=?`,
		joinIntList(targets), id)
	return err
}

// ListNodeAlertTargets 返回所有「开启了重点监控」的节点及其目标清单。
//
// 调度器每 10 秒扫描一次，若对每个节点各查一次库会很浪费；这里一次取回
// 只含非空清单的小映射，checkProbeFail 按节点 ID 查表即可。失败时返回 nil
// （调用方视为"没有任何节点开启重点监控"）——这只是告警增强，不应拖垮整轮扫描。
func (r *Repo) ListNodeAlertTargets() (map[int64][]int64, error) {
	rows, err := r.db.Read.Query(
		`SELECT id, COALESCE(alert_probe_targets,'') FROM nodes WHERE COALESCE(alert_probe_targets,'') <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var id int64
		var csv string
		if err := rows.Scan(&id, &csv); err != nil {
			return nil, err
		}
		if list := splitIntList(csv); len(list) > 0 {
			out[id] = list
		}
	}
	return out, rows.Err()
}

// UpdateNodeObservedIP 用"观测到的地址"更新节点地址，返回是否真的改动了。
//
// 列名按地址族选（`:` ⇒ ip_v6，否则 ip_v4），与前端展示的两个字段一致。
// 这一点不能省：公网 IPv6 节点如果被写进 ip_v4，界面上会变成一个谁也连不上的地址。
//
// 两个前置条件都写在 SQL 的 WHERE 里，而不是先读后判：
//   - `ip_auto=1`：管理员手工填过就不再动它（见 UpdateNodeAssets）；
//   - 与当前值不同：避免每次上报都写一次库（上报是几秒一次的高频路径）。
//
// **调用方必须自己保证 ip 是公网可路由地址**（回环/私网/链路本地不写）：
// 那条判断属于"地址语义"，不属于"节点存储"，实现在 internal/netutil 里
// （Master 与 Agent 共用同一份）。真机上"地址栏恒显示 127.0.0.1"就是这么来的 ——
// Master 与 Agent 同机时来源地址本来就是回环地址，而它照样被写进了
// "这是节点的公网地址"这一栏。
func (r *Repo) UpdateNodeObservedIP(id int64, ip string) (bool, error) {
	if ip == "" {
		return false, nil
	}
	// 列名只能拼字符串（`?` 绑不了标识符）；输入是对端地址，不含引号，没有注入面。
	col := "ip_v4"
	if strings.Contains(ip, ":") {
		col = "ip_v6"
	}
	res, err := r.db.Write.Exec(
		`UPDATE nodes SET `+col+`=? WHERE id=? AND COALESCE(ip_auto,1)=1 AND COALESCE(`+col+`,'')<>?`,
		ip, id, ip)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ClearNonPublicObservedIPs 清掉"自动"节点上那些不是公网地址的历史值，返回改动的行数。
//
// 为什么必须有这一步：老实现把上报连接的对端地址无条件写进 ip_v4，同机部署的节点
// 因此存着 127.0.0.1。新规则不再写这种值，于是**也就没人会去覆盖它** ——
// 用户升级后会看到"地址栏还是 127.0.0.1"，然后合理地认为改动没生效。
//
// 判定复用 internal/netutil（与写入路径同一份实现）：在 SQL 里用 LIKE 枚举私网段
// 看着更"一次搞定"，但那是把同一套地址语义写第二遍 —— 172.16/12 与 100.64/10
// 这两段用 LIKE 根本写不干净，而两处漂移的症状是"有的私网地址被清了、有的没清"。
//
// 只动 ip_auto=1 的行：管理员手工填的地址（哪怕他不小心填了个内网地址）
// 是**他的**决定，不该被一次静默的启动清理抹掉。
func (r *Repo) ClearNonPublicObservedIPs() (int64, error) {
	rows, err := r.db.Read.Query(
		`SELECT id, COALESCE(ip_v4,''), COALESCE(ip_v6,'') FROM nodes WHERE COALESCE(ip_auto,1)=1`)
	if err != nil {
		return 0, err
	}
	type fix struct {
		id  int64
		col string
	}
	var fixes []fix
	for rows.Next() {
		var id int64
		var v4, v6 string
		if err := rows.Scan(&id, &v4, &v6); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if v4 != "" && !netutil.IsPublicIP(v4) {
			fixes = append(fixes, fix{id, "ip_v4"})
		}
		if v6 != "" && !netutil.IsPublicIP(v6) {
			fixes = append(fixes, fix{id, "ip_v6"})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	var n int64
	for _, f := range fixes {
		res, err := r.db.Write.Exec(`UPDATE nodes SET `+f.col+`=NULL WHERE id=?`, f.id)
		if err != nil {
			return n, err
		}
		if got, err := res.RowsAffected(); err == nil {
			n += got
		}
	}
	return n, nil
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
