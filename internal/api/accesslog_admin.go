package api

import (
	"net/http"

	"github.com/koyoca/xb-probe/internal/model"
)

// 访问日志的后台接口（对应 GET /api/admin/v1/access-log）。
//
// 只读接口，分页 + 可选按 IP 精确过滤。为什么不做关键词搜索：
// access_log 一张表随页面访问线性增长（一次打开就是十几个请求），
// LIKE '%x%' 会让翻页越来越慢；需要看某个 IP 就从列表里点它的地址进来
// （走 idx_access_log_ip 复合索引）。
func (h *Handler) handleAccessLog(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100)
	offset := queryInt(r, "offset", 0)
	ip := sanitizeText(r.URL.Query().Get("ip"), 64)

	total, err := h.Repo.CountAccessLog(ip)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
		return
	}
	items, err := h.Repo.ListAccessLog(limit, offset, ip)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
		return
	}

	// enabled/days 一并回传：页面要显示"当前是否在记录、保留多久"，
	// 省掉一次 /settings 往返，也避免两处读到不一致的开关状态。
	type resp struct {
		Total   int64           `json:"total"`
		Limit   int             `json:"limit"`
		Offset  int             `json:"offset"`
		Enabled bool            `json:"enabled"`
		Days    int             `json:"days"`
		Items   []accessLogItem `json:"items"`
		Note    string          `json:"note,omitempty"`
	}

	out := resp{
		Total:   total,
		Limit:   limit,
		Offset:  offset,
		Enabled: h.accessLogEnabled(),
		Days:    int(h.Repo.GetIntSetting("security.access_log_days", 30)),
		Items:   make([]accessLogItem, 0, len(items)),
	}
	for _, e := range items {
		out.Items = append(out.Items, accessLogItem{
			ID: e.ID, TS: e.TS, IP: e.IP, Method: e.Method, Path: e.Path,
			Status: e.Status, Bytes: e.Bytes, DurMs: e.DurMs, UA: e.UA,
		})
	}
	if !out.Enabled {
		out.Note = "访问日志当前未开启：在「系统设置 → 访问日志与黑名单」里打开后才会开始记录。"
	}
	writeJSON(w, http.StatusOK, out)
}

// accessLogItem 是列表项 DTO（与 store.AccessLogEntry 同形，单独定义是为了
// 固化 JSON 字段名——直接序列化 store 结构会让"改库字段"意外改掉前端契约）。
type accessLogItem struct {
	ID     int64  `json:"id"`
	TS     int64  `json:"ts"`
	IP     string `json:"ip"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Bytes  int64  `json:"bytes"`
	DurMs  int64  `json:"dur_ms"`
	UA     string `json:"ua,omitempty"`
}
