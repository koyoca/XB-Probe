package model

import (
	"encoding/json"
	"testing"
)

// 卡片级公开开关的解析（7.3）。
//
// 三态是这套机制的**全部要点**，写错任何一态都会让开关"关不掉"：
//
//	"all"        → nil（不限制，全部展示）
//	""           → 非 nil 空 map（一张都不展示）
//	"a,b"        → 只展示 a 与 b
func TestParsePublicCardsTriState(t *testing.T) {
	if m := ParsePublicCards(PublicCardsAll); m != nil {
		t.Errorf("%q 必须解析为 nil（不限制），得到 %v", PublicCardsAll, m)
	}
	if m := ParsePublicCards("ALL"); m != nil {
		t.Error("哨兵值应大小写不敏感")
	}
	m := ParsePublicCards("")
	if m == nil {
		t.Fatal("空串必须解析为非 nil 空 map，否则'全部关掉'会退化成'全部打开'")
	}
	if len(m) != 0 {
		t.Errorf("空串应得到空 map，得到 %v", m)
	}
	m = ParsePublicCards(" kpi_fee , KPI_NET ; info_cpu ")
	if len(m) != 3 || !m["kpi_fee"] || !m["kpi_net"] || !m["info_cpu"] {
		t.Errorf("应容忍大小写与多种分隔符，得到 %v", m)
	}
	// 未知键在解析层被丢弃（校验层才是报错的地方）。
	m = ParsePublicCards("kpi_fee,nope")
	if len(m) != 1 || !m["kpi_fee"] {
		t.Errorf("未知键应被丢弃，得到 %v", m)
	}
}

func TestFormatPublicCardListRoundTrip(t *testing.T) {
	if got := FormatPublicCardList(nil); got != PublicCardsAll {
		t.Errorf("nil 掩码应还原为 %q，得到 %q", PublicCardsAll, got)
	}
	if got := FormatPublicCardList(map[string]bool{}); got != "" {
		t.Errorf("空掩码应还原为空串，得到 %q", got)
	}
	src := "kpi_fee,kpi_nodes"
	if got := FormatPublicCardList(ParsePublicCards(src)); got != src {
		t.Errorf("往返后应为 %q，得到 %q", src, got)
	}
}

// PublicSite.Cards 必须能区分 null 与 []。
//
// 这是最容易踩的一个坑：给字段加 omitempty 后，空切片会被整个序列化掉，
// 前端只能拿到 undefined，于是"管理员关掉了全部卡片"退化成"全部展示"。
func TestPublicSiteCardsDistinguishesNullAndEmpty(t *testing.T) {
	all, err := json.Marshal(PublicSite{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsKey(all, "cards") {
		t.Fatalf("cards 字段不应被省略：%s", all)
	}
	var probe struct {
		Cards []string `json:"cards"`
	}
	if err := json.Unmarshal(all, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Cards != nil {
		t.Errorf("nil 切片应序列化为 null，得到 %v", probe.Cards)
	}

	empty, err := json.Marshal(PublicSite{Title: "t", Cards: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(empty, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Cards == nil || len(probe.Cards) != 0 {
		t.Errorf("空切片必须序列化为 []（而不是 null），得到 %v", probe.Cards)
	}
}

func containsKey(b []byte, key string) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil {
		return false
	}
	_, ok := obj[key]
	return ok
}
