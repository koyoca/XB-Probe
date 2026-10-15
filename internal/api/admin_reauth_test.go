package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/koyoca/xb-probe/internal/auth"
)

// reauth 为了取密码/动态码会把请求体读走一遍，而"改审计保留期""清理历史数据"
// 这两个接口在它之后还要按**自己的**结构体再解一次同一个 body（保留天数、
// 截止日期就和密码装在同一个 JSON 里）。
//
// 曾经 reauth 没有把 Body 还回去，于是第二次 decodeJSON 只读到 EOF：
// 用户明明填了 10，却回报"保留天数须为 1~3650 之间的整数"（改保留期）与
// "请求体解析失败"（清理历史数据）。删/恢复备份之所以是好的，只因它们恰好
// 不解第二遍——同一个坑，只是被"恰好"遮住，加新接口时还会再踩。
//
// 所以这条测试同时钉住两个接口：都必须在同一个请求体里既带密码、又带业务参数。
func TestReauthKeepsBodyReadableForCaller(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	admin, err := h.Repo.GetAdminByUsername("ops")
	if err != nil {
		t.Fatalf("取管理员失败：%v", err)
	}
	sess := &auth.Session{AdminID: admin.ID}

	// ---- 1) 清理历史数据：days 必须真的被读到 ----
	raw, _ := json.Marshal(map[string]any{"days": 10, "password": testPw, "code": ""})
	req := httptest.NewRequest(http.MethodPost, "/maintenance/storage/purge", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithAdmin(req.Context(), sess))
	w := httptest.NewRecorder()
	h.handlePurgeData(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("同一请求体里带密码+days 应 200，得到 %d：%s", w.Code, w.Body.String())
	}

	// ---- 2) 改审计保留期：days 必须真的被读到，并且落库 ----
	raw2, _ := json.Marshal(map[string]any{"days": 10, "password": testPw, "code": ""})
	req2 := httptest.NewRequest(http.MethodPut, "/maintenance/audit/retention", bytes.NewReader(raw2))
	req2.Header.Set("Content-Type", "application/json")
	req2 = req2.WithContext(contextWithAdmin(req2.Context(), sess))
	w2 := httptest.NewRecorder()
	h.handleSetAuditRetention(w2, req2)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("同一请求体里带密码+days 应 204，得到 %d：%s", w2.Code, w2.Body.String())
	}
	if got := h.Repo.GetIntSetting("audit.retention_days", 30); got != 10 {
		t.Errorf("保留期应已改为 10，实际 %d（接口通了但值没进去）", got)
	}

	// ---- 3) 立即清理审计日志：只带密码也该照旧能跑（回归保护） ----
	raw3, _ := json.Marshal(map[string]any{"password": testPw, "code": ""})
	req3 := httptest.NewRequest(http.MethodPost, "/maintenance/audit/prune", bytes.NewReader(raw3))
	req3.Header.Set("Content-Type", "application/json")
	req3 = req3.WithContext(contextWithAdmin(req3.Context(), sess))
	w3 := httptest.NewRecorder()
	h.handlePruneAuditLog(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("立即清理应 200，得到 %d：%s", w3.Code, w3.Body.String())
	}

	// ---- 4) 密码错仍必须是 403（不是 401，否则前端又把人踢去登录页） ----
	raw4, _ := json.Marshal(map[string]any{"days": 10, "password": "wrong-password-xyz", "code": ""})
	req4 := httptest.NewRequest(http.MethodPut, "/maintenance/audit/retention", bytes.NewReader(raw4))
	req4.Header.Set("Content-Type", "application/json")
	req4 = req4.WithContext(contextWithAdmin(req4.Context(), sess))
	w4 := httptest.NewRecorder()
	h.handleSetAuditRetention(w4, req4)
	if w4.Code != http.StatusForbidden {
		t.Errorf("密码错误应 403，得到 %d：%s", w4.Code, w4.Body.String())
	}
}
