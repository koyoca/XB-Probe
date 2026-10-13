package tzdata_test

import (
	"testing"
	"time"

	// 空导入本包即把 IANA 时区库链进测试二进制；下面的断言才有意义。
	_ "github.com/koyoca/xb-probe/internal/tzdata"
)

// TestIANAZonesResolveWithEmbeddedDatabase 验证内嵌时区库真的可用。
//
// 局限说明（写清楚比假装严谨好）：在开发机上 GOROOT 通常存在，
// 即使不内嵌也能 LoadLocation 成功，所以本测试无法单独证明"内嵌了"。
// 真正端到端的回归防线是 scripts/smoke-test.sh —— 它会真的 PUT 一次带
// tz="Asia/Shanghai" 的计费并断言 HTTP 200（缺少内嵌库时会 400）。
// 本测试的价值在于：一旦有人把 internal/tzdata 删掉，至少这里的
// "Asia/Shanghai 必须是 +08:00 且不是本地时区别名" 会先把问题挡住。
func TestIANAZonesResolveWithEmbeddedDatabase(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation(Asia/Shanghai) 失败：%v（内嵌时区库可能被移除）", err)
	}
	if loc.String() != "Asia/Shanghai" {
		t.Fatalf("期望拿到具名时区 Asia/Shanghai，得到 %q", loc.String())
	}

	// 2026-01-15 12:00 UTC 在 Asia/Shanghai 应为 20:00，偏移 +8h。
	utc := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	local := utc.In(loc)
	if h := local.Hour(); h != 20 {
		t.Fatalf("UTC 12:00 在上海应为 20:00，得到 %02d:00", h)
	}
	if _, off := local.Zone(); off != 8*3600 {
		t.Fatalf("Asia/Shanghai 偏移应为 28800 秒，得到 %d", off)
	}

	// 再验一个不带 DST 之外的、偏移为负的时区，避免"碰巧等于本地时区"而误判通过。
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation(America/Los_Angeles) 失败：%v", err)
	}
	if _, off := utc.In(la).Zone(); off >= 0 {
		t.Fatalf("冬季 America/Los_Angeles 偏移应为负值，得到 %d", off)
	}
}
