package store

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/koyoca/xb-probe/internal/auth"
	"github.com/koyoca/xb-probe/internal/db"
	"github.com/koyoca/xb-probe/internal/model"
	"github.com/koyoca/xb-probe/internal/traffic"
)

// newTestRepo 在临时目录建一个迁移完毕的库（每个测试独立，互不干扰）。
func newTestRepo(t *testing.T) *Repo {
	t.Helper()
	st, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("迁移失败：%v", err)
	}
	return New(st)
}

func mustNode(t *testing.T, r *Repo, name string) int64 {
	t.Helper()
	id, err := r.CreateNode("pub"+name, name, "test", 1_760_000_000)
	if err != nil {
		t.Fatalf("创建节点失败：%v", err)
	}
	return id
}

// S6 / 13.4.7：注册码必须原子消费——同一码并发使用只能成功一次。
func TestEnrollCodeAtomicConsumption(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "n1")
	now := int64(1_760_000_000)

	code, err := auth.GenerateEnrollCode()
	if err != nil {
		t.Fatalf("生成注册码失败：%v", err)
	}
	hash := auth.HashToken(auth.NormalizeEnrollCode(code))
	if err := r.CreateEnrollCode(nodeID, hash, code[:6], now+1800, "admin:1", "shell", now); err != nil {
		t.Fatalf("写入注册码失败：%v", err)
	}

	const workers = 10
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gotNode, ok, _, err := r.ConsumeEnrollCode(hash, now+1)
			if err != nil {
				return
			}
			if ok && gotNode == nodeID {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if success != 1 {
		t.Fatalf("并发 %d 次消费应恰好成功 1 次，实际 %d 次", workers, success)
	}
	// 二次消费必须失败，且归因为"已使用"
	if _, ok, reason, _ := r.ConsumeEnrollCode(hash, now+2); ok {
		t.Error("同一注册码不应被使用两次")
	} else if reason != "used" {
		t.Errorf("二次消费的归因应为 used，得到 %q", reason)
	}
}

func TestEnrollCodeExpired(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "n2")
	now := int64(1_760_000_000)
	hash := auth.HashToken("EXPIREDCODE1")

	if err := r.CreateEnrollCode(nodeID, hash, "EXPIRE", now-1, "admin:1", "shell", now-60); err != nil {
		t.Fatalf("写入注册码失败：%v", err)
	}
	if _, ok, reason, _ := r.ConsumeEnrollCode(hash, now); ok {
		t.Error("已过期的注册码不应被消费")
	} else if reason != "expired" {
		t.Errorf("过期注册码的归因应为 expired，得到 %q", reason)
	}
	// 不存在的码：统一按"missing"归因，上层沿用"无效或已过期"口径（避免枚举探测）。
	if _, ok, reason, _ := r.ConsumeEnrollCode(auth.HashToken("NOSUCHCODE01"), now); ok {
		t.Error("不存在的注册码不应被消费")
	} else if reason != "missing" {
		t.Errorf("不存在注册码的归因应为 missing，得到 %q", reason)
	}
}

// 12.2：同一节点同时最多 3 个有效注册码。
func TestEnrollCodeLimitPerNode(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "n3")
	now := int64(1_760_000_000)
	for i := 0; i < 3; i++ {
		h := auth.HashToken("CODE" + string(rune('A'+i)))
		if err := r.CreateEnrollCode(nodeID, h, "CODE0"+string(rune('A'+i)), now+1800, "admin:1", "shell", now); err != nil {
			t.Fatalf("第 %d 个注册码应可创建：%v", i+1, err)
		}
	}
	h4 := auth.HashToken("CODE4")
	if err := r.CreateEnrollCode(nodeID, h4, "CODE04", now+1800, "admin:1", "shell", now); err == nil {
		t.Error("第 4 个注册码应被拒绝（上限 3）")
	}
	// 上限可调：调到 5 之后第 4、5 个都能创建，第 6 个被拒。
	if err := r.SetSetting("install.max_enroll_codes", "5", now); err != nil {
		t.Fatalf("写入上限设置失败：%v", err)
	}
	if err := r.CreateEnrollCode(nodeID, h4, "CODE04", now+1800, "admin:1", "shell", now); err != nil {
		t.Errorf("上限调到 5 后第 4 个注册码应可创建：%v", err)
	}
	h5 := auth.HashToken("CODE5")
	if err := r.CreateEnrollCode(nodeID, h5, "CODE05", now+1800, "admin:1", "shell", now); err != nil {
		t.Errorf("上限调到 5 后第 5 个注册码应可创建：%v", err)
	}
	if err := r.CreateEnrollCode(nodeID, auth.HashToken("CODE6"), "CODE06", now+1800, "admin:1", "shell", now); err == nil {
		t.Error("第 6 个注册码应被拒绝（上限 5）")
	}
}

// 后台令牌面板要能看到注册码（前 6 位提示 + 状态）。曾经的实现只存哈希，
// "已签发但未被使用"的码在后台无处可查——注册失败时既拿不回码、也无法
// 判断该等它过期还是重新签发。
func TestListEnrollCodesShowsHintsAndStatus(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "n5")
	now := int64(1_760_000_000)

	if err := r.CreateEnrollCode(nodeID, auth.HashToken("ABCDEFGHJK12"), "ABCDEF", now+1800, "admin:1", "shell", now); err != nil {
		t.Fatalf("写注册码 1 失败：%v", err)
	}
	if err := r.CreateEnrollCode(nodeID, auth.HashToken("ZZZZZZZZZZ99"), "ZZZZZZ", now+1800, "admin:1", "shell", now); err != nil {
		t.Fatalf("写注册码 2 失败：%v", err)
	}
	codes, err := r.ListEnrollCodes(nodeID)
	if err != nil {
		t.Fatalf("列注册码失败：%v", err)
	}
	if len(codes) != 2 {
		t.Fatalf("应有 2 条注册码，得到 %d 条", len(codes))
	}
	// 未使用且未过期 → 有效。
	if codes[0].Hint != "ZZZZZZ" || codes[0].UsedAt != 0 {
		t.Errorf("第一条应为未使用的 ZZZZZZ，实际 %+v", codes[0])
	}
	// 消费一条后 → 已使用，且提示位仍在。
	if _, ok, _, err := r.ConsumeEnrollCode(auth.HashToken("ABCDEFGHJK12"), now+1); err != nil || !ok {
		t.Fatalf("消费注册码失败：ok=%v err=%v", ok, err)
	}
	codes, err = r.ListEnrollCodes(nodeID)
	if err != nil {
		t.Fatalf("列注册码失败：%v", err)
	}
	var used int
	for _, c := range codes {
		if c.UsedAt > 0 {
			used++
			if c.Hint != "ABCDEF" {
				t.Errorf("已使用注册码的提示位应为 ABCDEF，得到 %q", c.Hint)
			}
		}
	}
	if used != 1 {
		t.Errorf("应有 1 条已使用，得到 %d 条", used)
	}
}

// 8.2 / 13.4.7：新令牌首次使用成功后，同节点其余未吊销令牌自动失效。
func TestTokenFirstUseRevokesOthers(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "n4")
	now := int64(1_760_000_000)

	_, h1, _, _ := auth.GenerateToken()
	_, h2, _, _ := auth.GenerateToken()
	id1, err := r.CreateToken(nodeID, h1, "aaaaaa", now)
	if err != nil {
		t.Fatalf("创建令牌 1 失败：%v", err)
	}
	id2, err := r.CreateToken(nodeID, h2, "bbbbbb", now)
	if err != nil {
		t.Fatalf("创建令牌 2 失败：%v", err)
	}

	// 两个都还可用
	if _, _, revoked, _ := r.FindTokenByHash(h1); revoked {
		t.Error("此时令牌 1 不应被吊销")
	}
	// 令牌 2 首次使用 → 令牌 1 应被吊销
	if err := r.TouchToken(id2, nodeID, "203.0.113.7", now+10); err != nil {
		t.Fatalf("TouchToken 失败：%v", err)
	}
	if _, _, revoked, _ := r.FindTokenByHash(h1); !revoked {
		t.Error("新令牌首次使用后，旧令牌应自动失效（保证同一时刻只有一个有效令牌）")
	}
	if _, _, revoked, _ := r.FindTokenByHash(h2); revoked {
		t.Error("正在使用的令牌不应被吊销")
	}

	// 再调用一次不应误伤自己
	if err := r.TouchToken(id2, nodeID, "203.0.113.7", now+20); err != nil {
		t.Fatalf("再次 TouchToken 失败：%v", err)
	}
	if _, _, revoked, _ := r.FindTokenByHash(h2); revoked {
		t.Error("重复使用不应吊销自身")
	}
	_ = id1
}

func TestRevokeToken(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "n5")
	now := int64(1_760_000_000)
	_, h, hint, _ := auth.GenerateToken()
	id, err := r.CreateToken(nodeID, h, hint, now)
	if err != nil {
		t.Fatalf("创建令牌失败：%v", err)
	}
	if err := r.RevokeToken(nodeID, id, now+5); err != nil {
		t.Fatalf("吊销失败：%v", err)
	}
	if _, _, revoked, _ := r.FindTokenByHash(h); !revoked {
		t.Error("吊销后 FindTokenByHash 应报告 revoked")
	}
	// 列表接口只暴露提示，不暴露哈希
	tokens, err := r.ListTokens(nodeID)
	if err != nil {
		t.Fatalf("列出令牌失败：%v", err)
	}
	if len(tokens) != 1 || tokens[0].TokenHint != hint {
		t.Fatalf("令牌列表异常：%+v", tokens)
	}
	if len(tokens[0].TokenHash) != 0 {
		t.Error("ListTokens 不应返回令牌哈希")
	}
}

// 7.2：PATCH 节点时未提交的字段应保持原值（否则改一个字段会清空其他字段）。
func TestUpdateNodeMetaKeepsValues(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "keepme")
	if err := r.UpdateNodeMeta(nodeID, "renamed", "grp", 5, false, "note"); err != nil {
		t.Fatalf("更新失败：%v", err)
	}
	n, err := r.GetNode(nodeID)
	if err != nil || n == nil {
		t.Fatalf("读取节点失败：%v", err)
	}
	if n.Name != "renamed" || n.GroupName != "grp" || n.SortOrder != 5 || n.PublicVisible || n.Note != "note" {
		t.Fatalf("字段未正确更新：%+v", n)
	}
}

func TestUpdateNodeAssets(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "assets")
	if err := r.UpdateNodeAssets(nodeID, "203.0.113.9", "", "Vultr", "东京", "JP", "测试机"); err != nil {
		t.Fatalf("更新资产失败：%v", err)
	}
	n, _ := r.GetNode(nodeID)
	if n.IPv4 != "203.0.113.9" || n.Provider != "Vultr" || n.Location != "东京" || n.Country != "JP" {
		t.Fatalf("资产字段未写入：%+v", n)
	}
	// 清空也应生效（对应后台把 IP 覆盖项清掉）
	if err := r.UpdateNodeAssets(nodeID, "", "", "", "", "", ""); err != nil {
		t.Fatalf("清空资产失败：%v", err)
	}
	n, _ = r.GetNode(nodeID)
	if n.IPv4 != "" || n.Provider != "" || n.Country != "" {
		t.Fatalf("资产字段应被清空：%+v", n)
	}
}

// 入网路径不得清零资源规格。
//
// 回归用例：/api/agent/v1/enroll 的载荷里只有 hostname/os/arch/virt 等环境字段，
// 没有 cpu_cores / mem_total / disk_total / kernel / cpu_model。
// 早先入网路径误调用了 UpdateNodeStatic（无条件写全 15 列），导致每次重新入网
// 都把已入库的资源规格写成 0 —— 节点在线、卡片却显示 "0 核 / 0 B 总量"，
// 而且 Agent 不重启就补不回来（Static 只在进程启动后第一次上报时携带）。
func TestUpdateNodeEnvKeepsResourceStatic(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "env")

	// 先模拟一次正常上报，把完整的资源规格写进库。
	full := &model.Static{
		Hostname: "web-1", OS: "Debian 12", Kernel: "6.1.0-amd64", Arch: "x86_64",
		CPUModel: "AMD EPYC 7443", CPUCores: 4,
		MemTotal: 5 * 1024 * 1024 * 1024, SwapTotal: 2 * 1024 * 1024 * 1024,
		DiskTotal: 167 * 1024 * 1024 * 1024, Virt: "kvm", AgentVersion: "1.0.0",
	}
	if err := r.UpdateNodeStatic(nodeID, full, "linux", "systemd", 64, 1_760_000_000); err != nil {
		t.Fatalf("写入静态信息失败：%v", err)
	}

	// 再模拟一次重新入网：只带环境字段。
	if err := r.UpdateNodeEnv(nodeID, "web-1", "Debian 12", "linux", "amd64", "kvm", "systemd", "demo-keepalive", 64); err != nil {
		t.Fatalf("更新环境信息失败：%v", err)
	}

	n, err := r.GetNode(nodeID)
	if err != nil || n == nil {
		t.Fatalf("读取节点失败：%v", err)
	}

	// 资源规格必须原封不动。
	if n.CPUCores != 4 {
		t.Errorf("入网不应改核数：期望 4，实际 %d", n.CPUCores)
	}
	if n.MemTotal != full.MemTotal {
		t.Errorf("入网不应改内存总量：期望 %d，实际 %d", full.MemTotal, n.MemTotal)
	}
	if n.SwapTotal != full.SwapTotal {
		t.Errorf("入网不应改 swap 总量：期望 %d，实际 %d", full.SwapTotal, n.SwapTotal)
	}
	if n.DiskTotal != full.DiskTotal {
		t.Errorf("入网不应改磁盘总量：期望 %d，实际 %d", full.DiskTotal, n.DiskTotal)
	}
	if n.Kernel != full.Kernel {
		t.Errorf("入网不应改内核版本：期望 %q，实际 %q", full.Kernel, n.Kernel)
	}
	if n.CPUModel != full.CPUModel {
		t.Errorf("入网不应改 CPU 型号：期望 %q，实际 %q", full.CPUModel, n.CPUModel)
	}
	if n.BootTime != 1_760_000_000 {
		t.Errorf("入网不应改启动时间：期望 1760000000，实际 %d", n.BootTime)
	}

	// 环境字段则应当被更新。
	if n.AgentVersion != "demo-keepalive" {
		t.Errorf("环境字段应更新为 demo-keepalive，实际 %q", n.AgentVersion)
	}
	if n.Arch != "amd64" || n.OSFamily != "linux" {
		t.Errorf("环境字段未按入网载荷更新：arch=%q os_family=%q", n.Arch, n.OSFamily)
	}
}

// 6.4：计费行随节点创建自动初始化，避免 upsert 语义混乱。
func TestCreateNodeInitializesBilling(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "billing")
	b, err := r.GetBilling(nodeID)
	if err != nil {
		t.Fatalf("读取计费失败：%v", err)
	}
	if b == nil {
		t.Fatal("节点创建时应初始化 node_billing 行")
	}
	if b.Cycle != model.CycleMonthly || b.ResetDay != 1 {
		t.Errorf("默认值与 6.4 不一致：%+v", b)
	}
}

// 13.4.7：探测目标只能改名与启停的窄接口（RenameOrToggleTarget）仍然保留，
// 它不该有能力碰到 host/port/method —— 只改名字的调用方不需要这么大的权限。
func TestProbeTargetRenameOrToggleKeepsEndpoint(t *testing.T) {
	r := newTestRepo(t)
	id, err := r.CreateProbeTarget(&model.ProbeTarget{
		Name: "电信", Carrier: "telecom", Host: "example.com",
		Port: 443, Method: "tcp", TimeoutMs: 2000, Enabled: true,
	})
	if err != nil {
		t.Fatalf("创建目标失败：%v", err)
	}
	if err := r.RenameOrToggleTarget(id, "电信-上海", false); err != nil {
		t.Fatalf("改名/停用失败：%v", err)
	}
	got, err := r.GetProbeTarget(id)
	if err != nil || got == nil {
		t.Fatalf("读取目标失败：%v", err)
	}
	if got.Name != "电信-上海" || got.Enabled {
		t.Errorf("名称与启停应已更新：%+v", got)
	}
	if got.Host != "example.com" || got.Port != 443 || got.Method != "tcp" {
		t.Errorf("窄接口不应改动 host/port/method：%+v", got)
	}
}

// 完整编辑：UpdateProbeTarget 必须能改全部可编辑字段（含 host/port/method）。
//
// 这是对 A4「host/port/method 不可原地修改」的有意放开，原因见 store_probe.go
// 顶部注释：填错一个端口号要"新建 + 停用 + 重建"三步走，实际使用中难以接受。
func TestUpdateProbeTargetFullFields(t *testing.T) {
	r := newTestRepo(t)
	id, err := r.CreateProbeTarget(&model.ProbeTarget{
		Name: "电信", Carrier: "telecom", Host: "1.1.1.1",
		Port: 443, Method: "tcp", TimeoutMs: 2000, Enabled: true,
	})
	if err != nil {
		t.Fatalf("创建目标失败：%v", err)
	}
	cur, err := r.GetProbeTarget(id)
	if err != nil || cur == nil {
		t.Fatalf("读取目标失败：%v", err)
	}
	next := *cur
	next.Name = "电信-上海"
	next.Carrier = "mobile"
	next.Host = "223.5.5.5"
	next.Port = 8443
	next.Method = "icmp"
	next.TimeoutMs = 3500
	next.Enabled = false
	if err := r.UpdateProbeTarget(&next); err != nil {
		t.Fatalf("完整更新失败：%v", err)
	}
	got, err := r.GetProbeTarget(id)
	if err != nil || got == nil {
		t.Fatalf("读回失败：%v", err)
	}
	if got.Name != "电信-上海" || got.Carrier != "mobile" || got.Host != "223.5.5.5" ||
		got.Port != 8443 || got.Method != "icmp" || got.TimeoutMs != 3500 || got.Enabled {
		t.Errorf("字段未全部更新：%+v", got)
	}
	// node_id 不在更新范围内：改作用域等于换一条目标。
	if got.NodeID != cur.NodeID {
		t.Errorf("node_id 不应被改动：%d → %d", cur.NodeID, got.NodeID)
	}
}

// 指标层的选择必须**跟着保留期走**，而不是写死跨度常量。
//
// 曾经的实现把 1h/5h/24h 硬映射到 metrics_raw、7d 硬映射到 metrics_1m。
// 管理员把 retention.raw_hours 从 24 调小（省磁盘，合法设置，下限 1）之后，
// "最近 24 小时"仍会去查 metrics_raw —— 那张表只剩最近 6 小时的数据，
// 曲线的左半边凭空消失，而设置页显示一切正常。
func TestMetricsTableForRangeFollowsRetention(t *testing.T) {
	t.Run("默认保留期", func(t *testing.T) {
		r := newTestRepo(t) // 不写任何 settings，走默认 24h / 7d
		cases := map[string]string{
			// 30m 曾经返回空串、由调用方改读内存环形缓冲；缓冲随 Master 重启清空，
			// "关掉网站过一阵再回来"时这一档就只剩重启以来的几分钟。现在与 1h 同层。
			"30m": "metrics_raw",
			"1h":  "metrics_raw", // 1h ≤ 24h
			"5h":  "metrics_raw",
			"24h": "metrics_raw", // 24h ≤ 24h，恰好覆盖
			"7d":  "metrics_1m",  // 7d ≤ 7d，恰好覆盖
			"30d": "metrics_10m",
			"":    "metrics_10m", // 未知跨度 → 最粗的一层（保留最久）
			"??":  "metrics_10m",
		}
		for span, want := range cases {
			if got := r.MetricsTableForRange(span); got != want {
				t.Errorf("MetricsTableForRange(%q) = %q，期望 %q", span, got, want)
			}
		}
	})

	t.Run("raw 保留期调小", func(t *testing.T) {
		r := newTestRepo(t)
		if err := r.SetSetting("retention.raw_hours", "6", 1_760_000_000); err != nil {
			t.Fatalf("写设置失败：%v", err)
		}
		// 6h 覆盖得住 1h/5h，但覆盖不住 24h → 24h 必须下沉到 1m 层。
		if got := r.MetricsTableForRange("5h"); got != "metrics_raw" {
			t.Errorf("5h 应留在 raw 层，得到 %q", got)
		}
		if got := r.MetricsTableForRange("24h"); got != "metrics_1m" {
			t.Errorf("raw 只留 6h 时 24h 应下沉到 metrics_1m，得到 %q", got)
		}
	})

	t.Run("raw 保留期调大", func(t *testing.T) {
		r := newTestRepo(t)
		if err := r.SetSetting("retention.raw_hours", "72", 1_760_000_000); err != nil {
			t.Fatalf("写设置失败：%v", err)
		}
		// 72h ≥ 24h，24h 仍然留在 raw 层（上调保留期不应降级精度）。
		if got := r.MetricsTableForRange("24h"); got != "metrics_raw" {
			t.Errorf("raw 留 72h 时 24h 应留在 metrics_raw，得到 %q", got)
		}
	})

	t.Run("mid 保留期调小", func(t *testing.T) {
		r := newTestRepo(t)
		if err := r.SetSetting("retention.mid_days", "3", 1_760_000_000); err != nil {
			t.Fatalf("写设置失败：%v", err)
		}
		// 3 天覆盖不住 7d、30d → 两档都要落到 10m 层。
		if got := r.MetricsTableForRange("7d"); got != "metrics_10m" {
			t.Errorf("mid 只留 3d 时 7d 应下沉到 metrics_10m，得到 %q", got)
		}
		// 同时 raw 的默认 24h 覆盖不住 7d，这条路径不能串到 raw 上。
		if got := r.MetricsTableForRange("1h"); got != "metrics_raw" {
			t.Errorf("1h 仍应留在 raw 层，得到 %q", got)
		}
	})

	t.Run("非法设置被收敛", func(t *testing.T) {
		r := newTestRepo(t)
		// 绕过 API 直接写库（settings 就是普通 KV）：0 会让所有层"立刻过期"，
		// 收敛到下限 1 之后至少 1h 查询还能落在 raw 层。
		if err := r.SetSetting("retention.raw_hours", "0", 1_760_000_000); err != nil {
			t.Fatalf("写设置失败：%v", err)
		}
		if got := r.MetricsTableForRange("1h"); got != "metrics_raw" {
			t.Errorf("raw_hours=0 收敛为 1 后 1h 应留在 metrics_raw，得到 %q", got)
		}
		// 负数同理，且绝不能出现"任何跨度都落不到 raw"以外的怪状态。
		if err := r.SetSetting("retention.raw_hours", "-5", 1_760_000_000); err != nil {
			t.Fatalf("写设置失败：%v", err)
		}
		if got := r.MetricsTableForRange("24h"); got != "metrics_1m" {
			t.Errorf("raw_hours=-5 收敛为 1 后 24h 应下沉到 metrics_1m，得到 %q", got)
		}
		if err := r.SetSetting("retention.mid_days", "999", 1_760_000_000); err != nil {
			t.Fatalf("写设置失败：%v", err)
		}
		// 上限 30 天：30d 恰好覆盖。
		if got := r.MetricsTableForRange("30d"); got != "metrics_1m" {
			t.Errorf("mid_days=999 收敛为 30 后 30d 应落在 metrics_1m，得到 %q", got)
		}
	})

	t.Run("永不返回未知表名", func(t *testing.T) {
		r := newTestRepo(t)
		for _, span := range []string{"30m", "1h", "5h", "24h", "7d", "30d", "", "1y", "abc"} {
			switch got := r.MetricsTableForRange(span); got {
			case "", "metrics_raw", "metrics_1m", "metrics_10m":
			default:
				t.Errorf("MetricsTableForRange(%q) = %q，不是已知的指标表", span, got)
			}
		}
	})
}

// 延迟表的映射**永远不能返回空串**。
//
// 调用方 handlePublicLatency 会把结果直接拼进 SQL：
//
//	SELECT ... FROM  WHERE node_id=?   -- 语法错误 → HTTP 500
//
// 曾经的实现把 "30m" 映射成 ""，于是公开详情页的"最近 30 分钟"这一档恒为
// 500、图例全空。这个测试钉住"任何跨度都必须给出一张真实的表"。
func TestLatencyTableForRangeNeverEmpty(t *testing.T) {
	r := newTestRepo(t)
	for _, span := range []string{"30m", "1h", "5h", "24h", "7d", "30d", "", "unknown"} {
		if got := r.LatencyTableForRange(span); got == "" {
			t.Errorf("LatencyTableForRange(%q) 不能返回空表名", span)
		} else if got != "latency_raw" && got != "latency_10m" {
			t.Errorf("LatencyTableForRange(%q) = %q，不是已知的延迟表", span, got)
		}
	}
	// 短跨度必须落在精度最高的原始层；30m 以前错误地绕过了这一层。
	for _, span := range []string{"30m", "1h", "5h", "24h"} {
		if got := r.LatencyTableForRange(span); got != "latency_raw" {
			t.Errorf("LatencyTableForRange(%q) = %q，期望 latency_raw", span, got)
		}
	}
	for _, span := range []string{"7d", "30d"} {
		if got := r.LatencyTableForRange(span); got != "latency_10m" {
			t.Errorf("LatencyTableForRange(%q) = %q，期望 latency_10m", span, got)
		}
	}
}

// A8 / 13.4.1：网卡基线按节点批量落盘并可读回；读回时 Valid 恒为 true。
func TestNICBaselineRoundTrip(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "nic")
	now := int64(1_760_000_000)

	in := map[string]traffic.NICBaseline{
		"eth0": {LastRx: 1000, LastTx: 500, BootTime: 100, UpdatedAt: now},
		"eth1": {LastRx: 2000, LastTx: 700, BootTime: 100, UpdatedAt: now},
	}
	if err := r.SaveNICBaselines(nodeID, in); err != nil {
		t.Fatalf("落盘基线失败：%v", err)
	}

	out, err := r.LoadNICBaselines(nodeID)
	if err != nil {
		t.Fatalf("读取基线失败：%v", err)
	}
	if len(out) != 2 {
		t.Fatalf("应读回 2 张网卡，实际 %d：%+v", len(out), out)
	}
	eth0, ok := out["eth0"]
	if !ok {
		t.Fatalf("缺少 eth0：%+v", out)
	}
	if eth0.LastRx != 1000 || eth0.LastTx != 500 || eth0.BootTime != 100 || eth0.UpdatedAt != now {
		t.Errorf("eth0 基线不一致：%+v", eth0)
	}
	if !eth0.Valid {
		t.Error("从库里读回的基线 Valid 应为 true")
	}

	// 同节点同网卡再写一次应是 UPSERT 而不是插入新行。
	if err := r.SaveNICBaselines(nodeID, map[string]traffic.NICBaseline{
		"eth0": {LastRx: 9000, LastTx: 8000, BootTime: 100, UpdatedAt: now + 60},
	}); err != nil {
		t.Fatalf("二次落盘失败：%v", err)
	}
	out2, err := r.LoadNICBaselines(nodeID)
	if err != nil {
		t.Fatalf("二次读取失败：%v", err)
	}
	if len(out2) != 2 {
		t.Fatalf("UPSERT 后仍应是 2 行，实际 %d", len(out2))
	}
	if out2["eth0"].LastRx != 9000 || out2["eth0"].UpdatedAt != now+60 {
		t.Errorf("UPSERT 未生效：%+v", out2["eth0"])
	}

	// 不同节点之间必须互不干扰。
	other := mustNode(t, r, "nic2")
	if m, err := r.LoadNICBaselines(other); err != nil || len(m) != 0 {
		t.Fatalf("另一节点不应看到基线：%v %+v", err, m)
	}

	// 空 map 直接返回，不产生任何写入。
	if err := r.SaveNICBaselines(nodeID, nil); err != nil {
		t.Fatalf("空基线应直接返回：%v", err)
	}
}

// 13.4.1 规则 4：超过 7 天未更新的基线要被清掉，仍在有效期内的保留。
func TestCleanupNICBaselines(t *testing.T) {
	r := newTestRepo(t)
	nodeID := mustNode(t, r, "nicclean")
	now := int64(1_760_000_000)
	stale := now - (traffic.BaselineCleanupDays+1)*86400
	fresh := now - 3600

	if err := r.SaveNICBaselines(nodeID, map[string]traffic.NICBaseline{
		"stale0": {LastRx: 1, LastTx: 1, UpdatedAt: stale},
		"fresh0": {LastRx: 2, LastTx: 2, UpdatedAt: fresh},
	}); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}

	n, err := r.CleanupNICBaselines(now)
	if err != nil {
		t.Fatalf("清理失败：%v", err)
	}
	if n != 1 {
		t.Errorf("应清理 1 行，实际 %d", n)
	}
	out, err := r.LoadNICBaselines(nodeID)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if _, ok := out["stale0"]; ok {
		t.Error("过期基线应已被删除")
	}
	if _, ok := out["fresh0"]; !ok {
		t.Error("未过期基线不应被删除")
	}
}

// TestUnarchiveNodeClearsBothMarkers 钉"解除归档"必须同时清掉两个标记。
//
// 背景（用户实测的死锁）：`xb-probe-agent uninstall` 会归档节点并吊销令牌；
// 重新安装拿到的新令牌在上报时被 `Tokens.Authenticate` 以
// `nodes.status='archived'` 拒掉，而归档标记的**权威来源**却是
// `archived_at`（运行态、公开看板、调度器都看它）——两列各管一摊。
// 只清其中一列 = 那一边继续拦着，现象依旧是"令牌有效却报 node_archived"。
func TestUnarchiveNodeClearsBothMarkers(t *testing.T) {
	r := newTestRepo(t)
	id := mustNode(t, r, "unarchive")

	if err := r.ArchiveNode(id, 1_760_000_000); err != nil {
		t.Fatalf("归档失败：%v", err)
	}
	n, err := r.GetNode(id)
	if err != nil {
		t.Fatalf("读取节点失败：%v", err)
	}
	if n.ArchivedAt == 0 {
		t.Fatal("归档后 archived_at 应为非 0")
	}
	if st, _ := r.NodeStatus(id); st != "archived" {
		t.Fatalf("归档后 status 应为 archived，实际 %q", st)
	}

	if err := r.UnarchiveNode(id); err != nil {
		t.Fatalf("解除归档失败：%v", err)
	}
	n2, err := r.GetNode(id)
	if err != nil {
		t.Fatalf("读取节点失败：%v", err)
	}
	if n2.ArchivedAt != 0 {
		t.Errorf("解除归档后 archived_at 应清零，实际 %d（运行态/公开看板仍会把它当归档）", n2.ArchivedAt)
	}
	// status 必须是 Authenticate 认得的"非归档"值，否则令牌仍会被拒。
	st2, err := r.NodeStatus(id)
	if err != nil {
		t.Fatalf("读取状态失败：%v", err)
	}
	if st2 == "archived" {
		t.Error("解除归档后 status 仍为 archived：Tokens.Authenticate 会继续拒绝该节点的令牌（死锁未解）")
	}
}

// TestEnrollPathUnarchivesNode 是上面那条的**端到端**版本：走真实的
// "注册码消费 → 建令牌"两步，确认解除归档确实发生在注册流程里，
// 而不是只有 store 层的一个孤儿方法（"有这个方法"≠"它被调用了"）。
//
// 判据落在 handler 源码上：这条链路跨 api/store 两层，
// 而 api 层没有轻量的 enroll 测试夹具（要起完整 Handler），
// 因此用"锚点必须存在且成对出现"来钉接线。
func TestEnrollPathUnarchivesNode(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "api", "agent.go"))
	if err != nil {
		t.Fatalf("读取 agent.go 失败：%v", err)
	}
	s := string(src)
	at := strings.Index(s, "func (h *Handler) handleEnroll")
	if at < 0 {
		t.Fatal("找不到 handleEnroll")
	}
	// 切到下一个顶层函数。
	rest := s[at:]
	end := strings.Index(rest[1:], "\nfunc ")
	if end < 0 {
		t.Fatal("无法界定 handleEnroll 的函数体")
	}
	body := rest[:end+1]

	if !strings.Contains(body, "UnarchiveNode(") {
		t.Error("handleEnroll 没有调用 UnarchiveNode：卸载后重新安装的节点会永远上报不上去" +
			"（令牌有效但被 nodes.status=archived 拒掉，且没有任何解除入口）")
	}
	if !strings.Contains(body, "st.ArchivedAt > 0") {
		t.Error("handleEnroll 没有判断 archived_at：应对『已归档』的节点解除归档，" +
			"对正常节点不必多写一次库")
	}
}
