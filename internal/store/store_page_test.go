package store

import (
	"testing"

	"github.com/yourorg/probe/internal/alert"
)

// 分页查询（告警事件 / 审计日志）的回归护栏。
//
// 两条容易退化的语义：
//  1. limit/offset 必须被**收敛**：limit=0 或负数不能变成"返回全部"
//     （SQLite 的 LIMIT 0 是"零行"，跟在后面的是"把整张表读进内存"），
//     越界的 offset 也不能直接把空表交给前端——分页条会显示"第 9 / 3 页"。
//  2. 回传的 limit/offset 必须是**实际生效**的窗口，而不是入参。
//     前端按它算页码，两者不一致时页码和表格内容会互相矛盾。
//
// 另外 items 必须是 JSON 里的 `[]` 而不是 `null`：空列表页渲染成 `null`
// 会让前端把 `res.items.length` 抛成异常。

func seedAudit(t *testing.T, r *Repo, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := r.Audit(int64(1_760_000_000+i), "ops", "node.update", "1", "127.0.0.1"); err != nil {
			t.Fatalf("写审计日志失败：%v", err)
		}
	}
}

func seedEvents(t *testing.T, r *Repo, n int) {
	t.Helper()
	// alert_events.node_id 有外键约束，必须挂在一个真实节点上。
	nodeID := mustNode(t, r, "page")
	for i := 0; i < n; i++ {
		if _, err := r.InsertAlertEvent(alert.Event{
			NodeID: nodeID, Kind: "offline", Level: "warn",
			Message: "节点离线", CreatedAt: int64(1_760_000_000 + i),
		}); err != nil {
			t.Fatalf("写告警事件失败：%v", err)
		}
	}
}

func TestNormalizePageClamps(t *testing.T) {
	cases := []struct {
		limit, offset      int
		wantLimit, wantOff int
		why                string
	}{
		{0, 0, DefaultPageSize, 0, "limit=0 不能变成 LIMIT 0（零行）或无限"},
		{-5, -5, DefaultPageSize, 0, "负数必须收敛，而不是原样丢给 SQL"},
		{10, 30, 10, 30, "合法值不该被动"},
		{MaxPageSize + 1, 0, MaxPageSize, 0, "超过上限要压回上限（否则一次把整表读进内存）"},
		{999999, 0, MaxPageSize, 0, "同上，量级再大也一样"},
	}
	for _, c := range cases {
		l, o := NormalizePage(c.limit, c.offset)
		if l != c.wantLimit || o != c.wantOff {
			t.Errorf("NormalizePage(%d,%d) = (%d,%d)，期望 (%d,%d)：%s",
				c.limit, c.offset, l, o, c.wantLimit, c.wantOff, c.why)
		}
	}
}

func TestListAuditLogPagination(t *testing.T) {
	r := newTestRepo(t)
	seedAudit(t, r, 25)

	// ① 第一页：最新的在前（id DESC），窗口大小与总数都要对。
	rows, meta, err := r.ListAuditLog(10, 0)
	if err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if meta.Total != 25 || meta.Limit != 10 || meta.Offset != 0 {
		t.Errorf("窗口元信息错误：%+v", meta)
	}
	if len(rows) != 10 {
		t.Fatalf("第一页应有 10 行，得到 %d", len(rows))
	}
	if rows[0].ID <= rows[9].ID {
		t.Errorf("应按 id 倒序（最新在前），得到 %d → %d", rows[0].ID, rows[9].ID)
	}

	// ② 逐页翻完，必须不重不漏。
	seen := map[int64]bool{}
	for off := 0; off < 25; off += 10 {
		page, _, err := r.ListAuditLog(10, off)
		if err != nil {
			t.Fatalf("查询失败：%v", err)
		}
		for _, x := range page {
			if seen[x.ID] {
				t.Errorf("id=%d 在两页里各出现一次（分页重叠）", x.ID)
			}
			seen[x.ID] = true
		}
	}
	if len(seen) != 25 {
		t.Errorf("逐页翻完只有 %d 行，应为 25（漏行）", len(seen))
	}

	// ③ 越界 offset：必须拉回最后一页，且 meta 里回传的是生效值。
	rows, meta, err = r.ListAuditLog(10, 900)
	if err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if meta.Offset != 20 {
		t.Errorf("越界 offset 应被拉回最后一页起点 20，得到 %d", meta.Offset)
	}
	if len(rows) != 5 {
		t.Errorf("最后一页应有 5 行，得到 %d", len(rows))
	}

	// ④ 空表：items 必须是非 nil 空切片（JSON 里是 []，不是 null）。
	empty := newTestRepo(t)
	rows, meta, err = empty.ListAuditLog(0, 0)
	if err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if rows == nil {
		t.Error("空表的 items 是 nil，序列化后会是 null——前端读 .length 会抛异常")
	}
	if len(rows) != 0 || meta.Total != 0 {
		t.Errorf("空表应返回 0 行 / total=0，得到 %d 行 / total=%d", len(rows), meta.Total)
	}
	if meta.Limit != DefaultPageSize {
		t.Errorf("limit=0 应收敛为默认值 %d，得到 %d", DefaultPageSize, meta.Limit)
	}
}

func TestListAlertEventsPagination(t *testing.T) {
	r := newTestRepo(t)
	seedEvents(t, r, 12)

	rows, meta, err := r.ListAlertEvents(5, 0)
	if err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if meta.Total != 12 || len(rows) != 5 {
		t.Fatalf("第一页应为 5 行 / total=12，得到 %d 行 / total=%d", len(rows), meta.Total)
	}
	if rows[0].ID <= rows[4].ID {
		t.Errorf("应按 id 倒序，得到 %d → %d", rows[0].ID, rows[4].ID)
	}

	// 越界窗口同样要拉回最后一页。
	rows, meta, err = r.ListAlertEvents(5, 100)
	if err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if meta.Offset != 10 {
		t.Errorf("越界 offset 应拉回 10，得到 %d", meta.Offset)
	}
	if len(rows) != 2 {
		t.Errorf("最后一页应有 2 行，得到 %d", len(rows))
	}

	// 超过上限的 limit 要压回上限：否则 ?limit=100000 就能一次拉走整张表。
	_, meta, err = r.ListAlertEvents(100000, 0)
	if err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if meta.Limit != MaxPageSize {
		t.Errorf("limit 应被压回 %d，得到 %d", MaxPageSize, meta.Limit)
	}
}

// 表名白名单：countRows 是包内私有的，但它拼的是 SQL，所以留一条护栏。
func TestCountRowsRejectsUnknownTable(t *testing.T) {
	r := newTestRepo(t)
	if _, err := r.countRows("nodes; DROP TABLE nodes"); err == nil {
		t.Error("countRows 接受了一个非白名单表名——这是 SQL 注入面")
	}
	for _, ok := range []string{"alert_events", "audit_log"} {
		if _, err := r.countRows(ok); err != nil {
			t.Errorf("countRows(%q) 失败：%v", ok, err)
		}
	}
}
