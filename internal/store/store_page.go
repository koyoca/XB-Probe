package store

import "fmt"

// 分页参数的收敛与窗口描述（6.x：告警事件 / 审计日志的分页查询）。
//
// 为什么把"分页"做成 store 层的公共设施而不是各写一份：
//
//	这两张表都需要「总数 + 一个窗口」，而且都需要处理同一个坑——保留期清理
//	会让总数变小。若 limit/offset 的收敛规则和"越界回退"在两处各写一遍，
//	迟早会出现"告警事件能翻到最后一页、审计日志停在空页"这种不一致。
//
// 另外 NormalizePage 是导出的：api 层要用**同一套**规则算出回给前端的
// limit/offset，否则前端按自己以为的窗口算页码，和实际查询的窗口对不上。

const (
	// DefaultPageSize 是不传 limit 时的每页条数。
	DefaultPageSize = 50
	// MaxPageSize 是每页条数上限。设上限是为了挡住 ?limit=100000
	// 这种"一次把整张表读进内存"的请求——分页接口本身不该有这个能力。
	MaxPageSize = 200
)

// PageMeta 描述一次分页查询**实际生效**的窗口。
//
// 它可能与请求参数不同（越界会被拉回最后一页），所以调用方必须用返回值
// 而不是自己记着的入参——否则前端会显示"第 9 / 3 页"配一张第 3 页的表。
type PageMeta struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

// NormalizePage 把任意的 (limit, offset) 收敛成合法值。
//
// limit<=0 → DefaultPageSize；limit>MaxPageSize → MaxPageSize；
// offset<0 → 0。所有这些都**不报错**：分页参数是展示层的东西，
// 为一个越界的 ?offset=-1 返回 400 只会让前端多一处要处理的失败分支。
func NormalizePage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// countRows 统计表行数。
//
// 表名只接受白名单里的字面量。这个函数是包内私有、调用点只有本仓库里的两处，
// 但仍然显式白名单一次：以后若有人图省事把一个外部字符串接进来，
// 那就是一个 SQL 注入点，而在分页这种"看起来只读"的地方最容易放松警惕。
func (r *Repo) countRows(table string) (int, error) {
	switch table {
	case "alert_events", "audit_log":
	default:
		return 0, fmt.Errorf("不支持的表名：%s", table)
	}
	var n int
	if err := r.db.Read.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
