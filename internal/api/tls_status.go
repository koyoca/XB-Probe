package api

import (
	"net/http"

	"github.com/koyoca/xb-probe/internal/model"
)

// handleTLSStatus 返回证书与 TLS 的运行态（只读）。
//
// 为什么不做成可在线修改的"设置项"：证书模式由 master.yaml 决定，
// 换模式/换域名都要重启进程（监听器与证书链都绑在启动时）。放进 /settings
// 会让管理员以为改完立刻生效，反而更难排查"为什么没反应"。
// 这里只回显状态，真正的排障线索（当前证书的主机名、到期时间、
// SPKI 指纹、最近一次申请失败原因）都在返回体里。
func (h *Handler) handleTLSStatus(w http.ResponseWriter, r *http.Request) {
	if h.TLSInfo == nil {
		writeJSON(w, http.StatusOK, model.TLSStatus{Mode: h.TLSMode})
		return
	}
	writeJSON(w, http.StatusOK, h.TLSInfo.TLSStatus())
}
