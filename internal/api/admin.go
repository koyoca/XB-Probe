package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/koyoca/xb-probe/internal/alert"
	"github.com/koyoca/xb-probe/internal/auth"
	"github.com/koyoca/xb-probe/internal/model"
	"github.com/koyoca/xb-probe/internal/state"
	"github.com/koyoca/xb-probe/internal/store"
	"github.com/koyoca/xb-probe/internal/traffic"
)

// 管理 API（文档 7.2 / 5.2）。所有写请求必须携带 X-CSRF-Token（由 requireAdmin 校验）。

type adminCtxKey struct{}

func contextWithAdmin(ctx context.Context, s *auth.Session) context.Context {
	return context.WithValue(ctx, adminCtxKey{}, s)
}

func adminFrom(r *http.Request) *auth.Session {
	s, _ := r.Context().Value(adminCtxKey{}).(*auth.Session)
	return s
}

// ---------------------------------------------------------------------------
// 认证
// ---------------------------------------------------------------------------

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		TOTP     string `json:"totp"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}
	ip := RealIP(r)
	now := nowFunc()

	admin, err := h.Repo.GetAdminByUsername(req.Username)
	if err != nil || admin == nil {
		// 统一错误，不区分用户名不存在/密码错误（9.3）。
		writeErr(w, http.StatusUnauthorized, "unauthorized", "用户名或密码错误")
		return
	}
	if admin.LockedUntil > now.Unix() {
		writeErr(w, http.StatusTooManyRequests, "locked", "账户已锁定，请稍后再试")
		return
	}
	if err := auth.VerifyPassword(req.Password, admin.PWHash); err != nil {
		_ = h.Repo.RecordLoginFailure(admin.ID, 5, 15*60, now.Unix())
		_ = h.Repo.Audit(now.Unix(), admin.Username, "auth.login_fail", "", ip)
		writeErr(w, http.StatusUnauthorized, "unauthorized", "用户名或密码错误")
		return
	}
	// 可选 TOTP 二次验证（9.3）；不可用时可改用一次性恢复码（6.2）。
	//
	// 两种凭证共用同一个输入框：管理员手机丢了的时候，最不想做的就是
	// "先想清楚该往哪个框里填"。checkSecondFactor 依次尝试动态码与恢复码，
	// 恢复码校验成功即被消费。
	if len(admin.TOTPSecret) > 0 {
		if !h.checkSecondFactor(admin, req.TOTP, now) {
			_ = h.Repo.RecordLoginFailure(admin.ID, 5, 15*60, now.Unix())
			writeErr(w, http.StatusUnauthorized, "invalid_totp", "动态验证码或恢复码错误")
			return
		}
	}

	plain, idHash, err := auth.NewSessionID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "内部错误")
		return
	}
	csrf := auth.GenerateCSRF(plain)
	lim := h.sessionLimits()
	sess := auth.Session{CreatedAt: now.Unix()}
	expires := auth.TouchDeadline(&sess, now, lim)
	if err := h.Repo.CreateSession(idHash, admin.ID, csrf, ip, r.UserAgent(), now.Unix(), expires); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "内部错误")
		return
	}
	_ = h.Repo.ResetLoginFailures(admin.ID)
	_ = h.Repo.Audit(now.Unix(), admin.Username, "auth.login", "", ip)

	// Cookie 的 MaxAge 与**服务端**的绝对上限取同一个值：两者不一致时，
	// 表现为"浏览器还带着 Cookie，服务端已经判过期"（每 3 秒一堆 401），
	// 或者反过来的"服务端认，浏览器早就不发了"。
	h.setSessionCookie(w, r, plain, int(lim.Absolute.Seconds()))
	writeJSON(w, http.StatusOK, map[string]any{
		"username":   admin.Username,
		"csrf_token": csrf, // 前端保存并在写请求的 X-CSRF-Token 中回传
	})
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s := adminFrom(r); s != nil {
		_ = h.Repo.DeleteSession(s.IDHash)
	}
	auth.ClearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	s := adminFrom(r)
	admin, _ := h.Repo.GetAdminByID(s.AdminID)
	name := ""
	if admin != nil {
		name = admin.Username
	}
	writeJSON(w, http.StatusOK, map[string]any{"username": name, "csrf_token": s.CSRFToken})
}

func (h *Handler) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	s := adminFrom(r)
	var req struct {
		Old string `json:"old_password"`
		New string `json:"new_password"`
	}
	if err := decodeJSON(r, &req); err != nil || len(req.New) < 12 {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "新密码至少 12 位")
		return
	}
	admin, err := h.Repo.GetAdminByID(s.AdminID)
	if err != nil || admin == nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "会话无效")
		return
	}
	if err := auth.VerifyPassword(req.Old, admin.PWHash); err != nil {
		writeErr(w, http.StatusForbidden, "forbidden", "原密码错误")
		return
	}
	hash, err := auth.HashPassword(req.New)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "内部错误")
		return
	}
	if err := h.Repo.UpdatePassword(admin.ID, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "内部错误")
		return
	}
	_ = h.Repo.Audit(time.Now().Unix(), admin.Username, "auth.password_change", "", RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

// reauth 校验"密码（始终）+ 动态码/恢复码（TOTP 开启时）"，用于保护破坏性维护操作
// （删除/恢复备份、清理历史数据、修改审计保留期等）。
//
// 与登录侧共用同一套核验（auth.VerifyPassword + checkSecondFactor），但**不**创建会话、
// 也不返回令牌——它只是"用当前已登录身份再核一次第二因子"，相当于一次轻量提权确认。
// 成功返回 admin；失败已写错误响应并返回 nil。
//
// 状态码口径（与 handleChangePassword 一致，这是本函数的关键）：
//   - 会话本身失效 → 401（前端据此跳登录页，合理）；
//   - 密码/动态码**核验失败** → 403。会话还是好的，只是这一次提权没通过，
//     若也用 401，前端会把它当成"会话过期"而把人踢回登录页——密码打错一个字
//     就莫名登出，正是本轮要修的可疑行为。
//
// **Body 必须可重复读**：二次确认的请求体里同时装着业务参数（保留天数、
// 截止日期……），调用方在 reauth 之后还要按自己的结构体再解一次同一个 body。
// HTTP 的 Body 是只读一次的流，这里不解回去，后面那次 decodeJSON 只会读到
// EOF——于是用户明明填了 10，却回报"保留天数须为整数""请求体解析失败"。
// 删/恢复备份、立即清理审计之所以是好的，只因它们恰好不解第二遍。
func (h *Handler) reauth(w http.ResponseWriter, r *http.Request) *store.Admin {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体读取失败")
		return nil
	}
	_ = r.Body.Close()
	// 把刚读走的字节还回去——**并且自己不能再从 r.Body 里解**：第一次 Decode
	// 会把流推到末尾，调用方随后那次 decodeJSON 又是 EOF。所以这里直接用
	// json.Unmarshal 验 raw 字节，还原好的 Body 原封不动留给调用方。
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return nil
	}
	s := adminFrom(r)
	if s == nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "会话无效")
		return nil
	}
	admin, err := h.Repo.GetAdminByID(s.AdminID)
	if err != nil || admin == nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "会话无效")
		return nil
	}
	if err := auth.VerifyPassword(req.Password, admin.PWHash); err != nil {
		_ = h.Repo.RecordLoginFailure(admin.ID, 5, 15*60, time.Now().Unix())
		_ = h.Repo.Audit(time.Now().Unix(), admin.Username, "auth.reauth_fail", "", RealIP(r))
		writeErr(w, http.StatusForbidden, "forbidden", "密码错误")
		return nil
	}
	if len(admin.TOTPSecret) > 0 && !h.checkSecondFactor(admin, req.Code, time.Now()) {
		_ = h.Repo.RecordLoginFailure(admin.ID, 5, 15*60, time.Now().Unix())
		writeErr(w, http.StatusForbidden, "forbidden", "动态验证码或恢复码错误")
		return nil
	}
	return admin
}

// ---------------------------------------------------------------------------
// 节点
// ---------------------------------------------------------------------------

func (h *Handler) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.Repo.ListNodes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
		return
	}
	// 合并内存运行态：库里的 status / last_seen 是死列（无人写入），
	// 只返回它们会让后台列表永远显示"离线 / 从未上报"。详见 AdminNodeItem。
	writeJSON(w, http.StatusOK, h.buildAdminNodeList(nodes, time.Now()))
}

func (h *Handler) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Group string `json:"group"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Name == "" {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "名称必填")
		return
	}
	publicID, err := auth.RandomHex(6) // 12 位随机，对外标识（5.1）
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "内部错误")
		return
	}
	now := time.Now()
	id, err := h.Repo.CreateNode(publicID, sanitizeText(req.Name, 64), sanitizeText(req.Group, 32), now.Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "创建失败")
		return
	}
	_ = h.Repo.Audit(now.Unix(), actorOf(r), "node.create", fmt.Sprint(id), RealIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "public_id": publicID})
}

func (h *Handler) handleGetNode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	n, err := h.Repo.GetNode(id)
	if err != nil || n == nil {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "节点不存在")
		return
	}
	// 计费信息随详情一起返回，供后台表单回显。
	// 读失败不阻断详情：计费表缺失只是少一块表单，不该让整个页面打不开。
	if b, err := h.Repo.GetBilling(id); err == nil && b != nil {
		n.Billing = billingToModel(*b)
	}
	writeJSON(w, http.StatusOK, n)
}

// billingToModel 把 store 行映射为 model 结构（保持 model 不依赖 store）。
func billingToModel(b store.BillingRow) *model.Billing {
	return &model.Billing{
		NodeID: b.NodeID, PriceMinor: b.PriceMinor, Currency: b.Currency,
		Cycle: b.Cycle, ExpireAt: b.ExpireAt, TrafficQuota: b.TrafficQuota,
		TrafficMode: b.TrafficMode, ResetDay: b.ResetDay, TZ: b.TZ,
		TrafficMultiplier: b.TrafficMultiplier,
	}
}

func (h *Handler) handlePatchNode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	// 指针字段：未提交的字段保持原值（PATCH 语义，7.2）。
	var req struct {
		Name              *string   `json:"name"`
		Group             *string   `json:"group"`
		SortOrder         *int      `json:"sort_order"`
		PublicVisible     *bool     `json:"public_visible"`
		Note              *string   `json:"note"`
		IPv4              *string   `json:"ip_v4"`
		IPv6              *string   `json:"ip_v6"`
		IPv4List          *[]string `json:"ip_v4_list"`
		IPv6List          *[]string `json:"ip_v6_list"`
		AlertProbeTargets *[]int64  `json:"alert_probe_targets"`
		Provider          *string   `json:"provider"`
		Location          *string   `json:"location"`
		Country           *string   `json:"country"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}
	cur, err := h.Repo.GetNode(id)
	if err != nil || cur == nil {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "节点不存在")
		return
	}

	name, group, note := cur.Name, cur.GroupName, cur.Note
	sortOrder, visible := cur.SortOrder, cur.PublicVisible
	// ip4/ip6 取"本次修改后的最终值"：下面要据此刷新运行态的协议栈标识，
	// 用 cur 的原值会让"刚填上 IPv6"的节点在公开看板上少一个标识。
	ip4, ip6 := cur.IPv4, cur.IPv6
	country := cur.Country
	if req.Name != nil {
		name = sanitizeText(*req.Name, 64)
	}
	if req.Group != nil {
		group = sanitizeText(*req.Group, 32)
	}
	if req.Note != nil {
		note = sanitizeText(*req.Note, 256)
	}
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	if req.PublicVisible != nil {
		visible = *req.PublicVisible
	}
	if err := h.Repo.UpdateNodeMeta(id, name, group, sortOrder, visible, note); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "更新失败")
		return
	}

	// 资产元数据（真实 IP / 机房 / 服务商 / 国家）单独更新，允许清空。
	if req.IPv4 != nil || req.IPv6 != nil || req.IPv4List != nil || req.IPv6List != nil ||
		req.Provider != nil || req.Location != nil || req.Country != nil {
		prov, loc := cur.Provider, cur.Location
		if req.IPv4 != nil {
			ip4 = strings.TrimSpace(*req.IPv4)
		}
		if req.IPv6 != nil {
			ip6 = strings.TrimSpace(*req.IPv6)
		}
		// 多地址清单优先：提交清单时，主地址恒取清单第一项（或清空）。
		// 这样前端只暴露"清单"一个入口，主地址与清单不再是两个需要同步的来源。
		if req.IPv4List != nil {
			ip4 = firstNonEmptyIP(*req.IPv4List)
		}
		if req.IPv6List != nil {
			ip6 = firstNonEmptyIP(*req.IPv6List)
		}
		if req.Provider != nil {
			prov = sanitizeText(*req.Provider, 64)
		}
		if req.Location != nil {
			loc = sanitizeText(*req.Location, 64)
		}
		if req.Country != nil {
			// 只接受两位大写字母（大小写会被规范化）；非法值一律落成"未设置"。
			// 这里刻意不报 400：country 是纯展示字段，为一个拼错的国家代码
			// 把整次节点编辑请求打回去，会让管理员在"改个名字"时莫名失败。
			country = model.NormalizeCountry(*req.Country)
		}
		if err := h.Repo.UpdateNodeAssets(id, ip4, ip6, prov, loc, country, note); err != nil {
			writeErr(w, http.StatusInternalServerError, model.ErrInternal, "资产信息更新失败")
			return
		}
		// 多地址清单（ip_v4_list / ip_v6_list）单独落库。
		if req.IPv4List != nil || req.IPv6List != nil {
			v4l, v6l := []string{}, []string{}
			if req.IPv4List != nil {
				v4l = *req.IPv4List
			}
			if req.IPv6List != nil {
				v6l = *req.IPv6List
			}
			if err := h.Repo.UpdateNodeIPLists(id, v4l, v6l); err != nil {
				writeErr(w, http.StatusInternalServerError, model.ErrInternal, "多地址更新失败")
				return
			}
		}
	}

	// 元数据同步用 SyncMeta 而不是 Update：节点可能还没首次上报（此时还没被上报路径
	// 惰性创建），用 Update 会静默失败，导致公开可见性改动丢失。
	h.Registry.SyncMeta(id, cur.PublicID, name, group, sortOrder, visible, cur.ArchivedAt > 0, ip4 != "", ip6 != "", country)

	// 「重点监控探测目标」单独落库：提交即整份替换（含清空），PATCH 语义。
	if req.AlertProbeTargets != nil {
		if err := h.Repo.UpdateNodeAlertTargets(id, *req.AlertProbeTargets); err != nil {
			writeErr(w, http.StatusInternalServerError, model.ErrInternal, "重点监控目标更新失败")
			return
		}
	}

	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "node.update", fmt.Sprint(id), RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

// firstNonEmptyIP 返回列表中第一个非空白地址；全空时返回 ""（表示清空主地址）。
func firstNonEmptyIP(list []string) string {
	for _, x := range list {
		if t := strings.TrimSpace(x); t != "" {
			return t
		}
	}
	return ""
}

func (h *Handler) handleArchiveNode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	now := time.Now().Unix()
	cur, _ := h.Repo.GetNode(id)
	if cur == nil {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "节点不存在")
		return
	}
	if err := h.Repo.ArchiveNode(id, now); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "归档失败")
		return
	}
	// 先用 SyncMeta 保证元数据就位（节点可能从未上报过），再推进状态。
	h.Registry.SyncMeta(id, cur.PublicID, cur.Name, cur.GroupName, cur.SortOrder, cur.PublicVisible, true, cur.IPv4 != "", cur.IPv6 != "", cur.Country)
	_ = h.Registry.Update(id, func(n *state.NodeRuntime) {
		n.Status = model.StatusArchived
	})
	h.Tokens.InvalidateNode(id)
	_ = h.Repo.Audit(now, actorOf(r), "node.archive", fmt.Sprint(id), RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

// handleUnarchiveNode 解除归档，让节点重新参与采集与告警。
//
// 为什么需要它：归档原本是**单向**的——`xb-probe-agent uninstall` 会归档节点
// 并吊销令牌，而重新安装后上报会被 `nodes.status='archived'` 拒掉，
// 全仓库却没有解除入口。enroll 路径现在会在重新注册时自动解除（那是主要路径），
// 但管理员仍需要能手工把一个"莫名归档"的节点接回来：
//   - 误点了归档按钮；
//   - 节点是别人卸载的，而现在要把它恢复成采集状态（且不想重新装 agent）。
//
// 不清令牌：归档时令牌已被吊销（uninstall 那一路）或仍在有效期内（后台点归档
// 那一路）。前者需要重新注册才能拿到新令牌，后者原令牌仍可用——两种都由
// 调用方自己判断，这里只负责把节点从归档态放出来。
func (h *Handler) handleUnarchiveNode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	now := time.Now().Unix()
	cur, _ := h.Repo.GetNode(id)
	if cur == nil {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "节点不存在")
		return
	}
	if cur.ArchivedAt == 0 {
		// 已是正常状态：不报错（幂等），避免前端"点了没反应"的困惑。
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.Repo.UnarchiveNode(id); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "解除归档失败")
		return
	}
	// 两处都要复位：库（见 UnarchiveNode）+ 内存运行态。运行态里的 Archived
	// 决定公开看板是否展示它、调度器是否跳过离线判定——只改库不改运行态，
	// 要等下一次 SyncMeta（可达数分钟）才生效。
	h.Registry.SyncMeta(id, cur.PublicID, cur.Name, cur.GroupName, cur.SortOrder,
		cur.PublicVisible, false, cur.IPv4 != "", cur.IPv6 != "", cur.Country)
	_ = h.Repo.Audit(now, actorOf(r), "node.unarchive", fmt.Sprint(id), RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	purge := r.URL.Query().Get("purge") == "true"
	if err := h.Repo.DeleteNode(id, purge); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "删除失败")
		return
	}
	h.Registry.Remove(id)
	h.Tokens.InvalidateNode(id)
	if h.Tracker != nil {
		h.Tracker.Drop(id)
	}
	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r),
		fmt.Sprintf("node.delete(purge=%v)", purge), fmt.Sprint(id), RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// 令牌
// ---------------------------------------------------------------------------

func (h *Handler) handleListTokens(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	tokens, err := h.Repo.ListTokens(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
		return
	}
	// 只返回提示（前 6 位）与最后使用信息，绝不返回令牌明文或哈希（9.1）。
	out := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, map[string]any{
			"id": t.ID, "hint": t.TokenHint, "created_at": t.CreatedAt,
			"last_used_at": t.LastUsedAt, "last_ip": t.LastIP, "revoked": t.Revoked(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) handleCreateEnrollCode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	code, expiresAt, err := h.newEnrollCode(id, actorOf(r), "shell")
	if err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": code, "expires_at": expiresAt})
}

// handleListEnrollCodes 列出某节点的注册码（仅前 6 位提示 + 状态）。
//
// 为什么必须能看：一次性注册码在界面上曾经"签发即失踪"——对话框一关就再也
// 找不回来，管理员既不知道自己手里是否还有效码，也不知道 401 是因为用过还是
// 过期。状态由服务端算好（避免各端时钟不一致导致"还剩 3 分钟"变"已过期"）。
func (h *Handler) handleListEnrollCodes(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	codes, err := h.Repo.ListEnrollCodes(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
		return
	}
	now := time.Now().Unix()
	out := make([]map[string]any, 0, len(codes))
	for _, c := range codes {
		status := "active"
		switch {
		case c.UsedAt > 0:
			status = "used"
		case c.ExpiresAt <= now:
			status = "expired"
		}
		out = append(out, map[string]any{
			"id": c.ID, "hint": c.Hint, "created_at": c.CreatedAt,
			"expires_at": c.ExpiresAt, "used_at": c.UsedAt,
			"platform": c.Platform, "status": status,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	tid, ok := pathInt64(r, "tid")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法令牌 ID")
		return
	}
	now := time.Now().Unix()
	if err := h.Repo.RevokeToken(id, tid, now); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "吊销失败")
		return
	}
	// 立即从内存缓存剔除，后续请求返回 403（8.2 第 1 步）。
	h.Tokens.InvalidateNode(id)
	_ = h.Repo.Audit(now, actorOf(r), "token.revoke", fmt.Sprint(tid), RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// 计费
// ---------------------------------------------------------------------------

func (h *Handler) handlePutBilling(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	var req struct {
		PriceMinor   int64  `json:"price_minor"`
		Currency     string `json:"currency"`
		Cycle        string `json:"cycle"`
		ExpireAt     int64  `json:"expire_at"`
		ExpireDate   string `json:"expire_date"` // YYYY-MM-DD，优先生效
		TrafficQuota int64  `json:"traffic_quota"`
		TrafficMode  string `json:"traffic_mode"`
		ResetDay     int    `json:"reset_day"`
		TZ           string `json:"tz"`
		// TrafficMultiplier：流量倍率，计费流量 = 计量口径 × 倍率。0 视为 1。
		TrafficMultiplier float64 `json:"traffic_multiplier"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}
	if !validCycle(req.Cycle) || !validTrafficMode(req.TrafficMode) || req.ResetDay < 1 || req.ResetDay > 31 {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "周期/口径/重置日取值非法")
		return
	}
	if req.TZ != "" {
		if _, err := time.LoadLocation(req.TZ); err != nil {
			writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "时区名称非法")
			return
		}
	}
	// 流量倍率：0 与缺省都按 1 处理（老前端只发字节额度，不发倍率）；
	// 上限 1000 是"防止把额度一次性算爆"的护栏，正常业务不会用到这么大。
	if req.TrafficMultiplier < 0 || req.TrafficMultiplier > 1000 {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "流量倍率应在 0~1000 之间")
		return
	}
	if req.TrafficMultiplier == 0 {
		req.TrafficMultiplier = 1
	}
	// 到期日解析放在后端做：只有后端持有完整的 IANA 时区库
	// （internal/tzdata 已内嵌 time/tzdata），前端用 new Date 换算会在
	// "节点时区与浏览器时区不同"时偏一天。
	if req.ExpireDate != "" && req.Cycle != model.CycleLifetime {
		loc := h.billingLoc(req.TZ)
		t, err := time.ParseInLocation("2006-01-02", req.ExpireDate, loc)
		if err != nil {
			writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "到期日格式应为 YYYY-MM-DD")
			return
		}
		// 到期日 = 该日 23:59:59（节点时区）。若取 00:00:00，管理员选 12-31
		// 会在 12-31 零点就判为过期，白丢一天。
		req.ExpireAt = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, loc).Unix()
	}
	if req.Cycle == model.CycleLifetime {
		req.ExpireAt = 0 // 永久节点忽略到期时间（4.4）
	}
	b := store.BillingRow{
		NodeID: id, PriceMinor: req.PriceMinor, Currency: sanitizeText(req.Currency, 8),
		Cycle: req.Cycle, ExpireAt: req.ExpireAt, TrafficQuota: req.TrafficQuota,
		TrafficMode: req.TrafficMode, ResetDay: req.ResetDay, TZ: req.TZ,
		TrafficMultiplier: req.TrafficMultiplier,
	}
	if err := h.Repo.UpsertBilling(b); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "保存失败")
		return
	}
	h.refreshBillingRuntime(id, b)
	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "billing.update", fmt.Sprint(id), RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleRenew(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	b, err := h.Repo.GetBilling(id)
	if err != nil || b == nil {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "计费信息不存在")
		return
	}
	// 永久节点不参与续费顺延（4.4）。
	if b.Cycle == model.CycleLifetime {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "永久节点无需续费")
		return
	}
	loc := time.UTC
	if b.TZ != "" {
		if l, err := time.LoadLocation(b.TZ); err == nil {
			loc = l
		}
	}
	base := b.ExpireAt
	if base < time.Now().Unix() {
		base = time.Now().Unix() // 已过期则从当前时间起算
	}
	b.ExpireAt = traffic.NextExpire(base, b.Cycle, loc)
	if err := h.Repo.UpsertBilling(*b); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "续费失败")
		return
	}
	h.refreshBillingRuntime(id, *b)
	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "billing.renew", fmt.Sprint(id), RealIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"expire_at": b.ExpireAt})
}

func (h *Handler) handleTraffic(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	switch r.URL.Query().Get("period") {
	case "daily":
		days, err := h.Repo.QueryTrafficDaily(id, r.URL.Query().Get("from"), r.URL.Query().Get("to"))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
			return
		}
		writeJSON(w, http.StatusOK, days)
	default:
		st, err := h.Repo.GetTrafficState(id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

// maxManualTrafficBytes 是手工矫正「单方向已用流量」的上限（1 PiB）。
//
// 作用是挡误填（比如把价格当字节填进流量框）并给下游的百分比换算留足上界：
// traffic.Percent 与 traffic.Billable（倍率最高 1000）在这个量级上都不会溢出。
const maxManualTrafficBytes int64 = 1 << 50

// handleSetTrafficUsage 手工矫正某节点「当前计费周期已用流量」（4.3）。
//
// 用途：Agent 重装/换机导致累计值从零开始、或运营商侧已计入的流量需要在面板上
// 对齐。**只改本周期合计**，不改 traffic_daily 的按日明细——日明细是"那天实际
// 跑了多少"的历史事实，矫正一个合计值不该顺手改写历史。
//
// 单位是**原始计量值**（字节，未按 traffic_mode 折算、未乘 traffic_multiplier），
// 与 GET /nodes/{id}/traffic?period=current 返回的 period_rx/period_tx 完全同口径。
// 前端把这两个数字直接回填进输入框，所以是"看到什么就改什么"，不需要管理员
// 心算倍率与口径。折算后的计费流量由 refreshBillingRuntime 按库里现有的
// 口径与倍率重算。
func (h *Handler) handleSetTrafficUsage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	var req struct {
		PeriodRx int64 `json:"period_rx"`
		PeriodTx int64 `json:"period_tx"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}
	if req.PeriodRx < 0 || req.PeriodTx < 0 ||
		req.PeriodRx > maxManualTrafficBytes || req.PeriodTx > maxManualTrafficBytes {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam,
			"流量取值应在 0 ~ "+fmtBytes(maxManualTrafficBytes)+" 之间")
		return
	}
	// traffic_state 对 nodes(id) 有外键：不先判存在性的话，一个不存在的 id
	// 会在 Upsert 时撞成外键错误，最后变成一个"保存失败"的 500。
	if n, err := h.Repo.GetNode(id); err != nil || n == nil {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "节点不存在")
		return
	}
	if h.Tracker == nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "流量计量未接线")
		return
	}
	now := time.Now()
	if err := h.Tracker.SetUsage(id, req.PeriodRx, req.PeriodTx, now); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "保存失败")
		return
	}
	h.applyTrafficUsage(id)
	_ = h.Repo.Audit(now.Unix(), actorOf(r), "traffic.override", fmt.Sprint(id), RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

// applyTrafficUsage 把矫正后的用量同步进运行态并重算计费流量。
//
// 不能只写库：详情页与看板读的是 state.NodeRuntime 里的 TrafficUsed，而它由
// 调度器每 60 秒从 tracker 重算一次。这里按同样的口径手动跑一遍，管理员保存后
// 立刻能看到新数字——否则他会以为没保存成功，然后再点一次。
func (h *Handler) applyTrafficUsage(nodeID int64) {
	if h.Registry == nil || h.Tracker == nil {
		return
	}
	rx, tx, _ := h.Tracker.Used(nodeID)
	_ = h.Registry.Update(nodeID, func(n *state.NodeRuntime) {
		n.PeriodRx, n.PeriodTx = rx, tx
	})
	// 只有存在计费行时才走 refreshBillingRuntime：它会用 b 覆盖价格、周期、
	// 到期时间等字段，拿一个零值 BillingRow 去调用会把它们全部清空。
	if b, err := h.Repo.GetBilling(nodeID); err == nil && b != nil {
		h.refreshBillingRuntime(nodeID, *b)
	}
}

// ---------------------------------------------------------------------------
// 探测目标
// ---------------------------------------------------------------------------

func (h *Handler) handleListTargets(w http.ResponseWriter, r *http.Request) {
	// 必须用全量查询：ListProbeTargets(0) 只返回全局目标，
	// 节点专属目标会从列表里"凭空消失"。支持 ?node_id= 收窄到单个节点。
	var (
		targets []*model.ProbeTarget
		err     error
	)
	if qs := r.URL.Query().Get("node_id"); qs != "" {
		var nodeID int64
		if _, perr := fmt.Sscanf(qs, "%d", &nodeID); perr != nil || nodeID <= 0 {
			writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 node_id")
			return
		}
		targets, err = h.Repo.ListProbeTargets(nodeID)
	} else {
		targets, err = h.Repo.ListEveryProbeTarget()
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
		return
	}
	writeJSON(w, http.StatusOK, targets)
}

func (h *Handler) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID    int64  `json:"node_id"`
		Name      string `json:"name"`
		Carrier   string `json:"carrier"`
		Host      string `json:"host"`
		Port      int    `json:"port"`
		Method    string `json:"method"`
		TimeoutMs int    `json:"timeout_ms"`
		// 省略视为 true（新建目标默认显示在公开点阵里）。
		ShowInBars *bool `json:"show_in_bars"`
		// NodeIDs 是"多选节点作用域"。存在性本身有语义，因此用切片而非单值：
		//   字段缺省(nil)      → 按 NodeID 单值语义处理（向后兼容旧前端）
		//   []（空数组）        → 建**一条全局目标**（下发给所有节点）
		//   [1,3,7]            → 为这三个节点各建一条同参数目标
		// 这样"全局"与"没填"在请求里是可区分的，不会退化成含糊的默认值。
		NodeIDs []int64 `json:"node_ids"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}
	if err := validateTargetFields(req.Host, req.Port, req.Method, req.TimeoutMs); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, err.Error())
		return
	}

	scope, err := h.resolveTargetScope(req.NodeIDs, req.NodeID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, err.Error())
		return
	}
	showInBars := true
	if req.ShowInBars != nil {
		showInBars = *req.ShowInBars
	}
	base := model.ProbeTarget{
		Name: sanitizeText(req.Name, 32), Carrier: normalizeCarrier(req.Carrier),
		Host: req.Host, Port: req.Port, Method: req.Method, TimeoutMs: req.TimeoutMs,
		Enabled: true, ShowInBars: showInBars,
	}
	ids := make([]int64, 0, len(scope))
	for _, nodeID := range scope {
		t := base
		t.NodeID = nodeID
		id, err := h.Repo.CreateProbeTarget(&t)
		if err != nil {
			// 已经建出来的不回收：它们本身是合法目标，删掉反而更乱。
			writeErr(w, http.StatusInternalServerError, model.ErrInternal,
				fmt.Sprintf("创建失败（已成功 %d 条）：%s", len(ids), err.Error()))
			return
		}
		_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "target.create", fmt.Sprint(id), RealIP(r))
		ids = append(ids, id)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      ids[0],
		"ids":     ids,
		"created": len(ids),
	})
}

// resolveTargetScope 把"多选 node_ids / 单选 node_id"两种入参折成一个作用域列表。
//
// 返回的列表里 0 表示全局目标。规则：
//   - 给了 node_ids（非 nil）：空数组 → [0]（一条全局目标）；否则逐个节点
//     （0 会被剔除，避免"误勾了全局又选了节点"造出重复的全局目标）；
//   - 没给 node_ids：沿用旧的单值 node_id。
//
// 每个节点都必须是库中真实存在的：专属目标指向不存在的节点时，任何 Agent
// 都拉不到它，却占着一个 ID——表现为"创建成功了但哪儿都看不到它生效"。
func (h *Handler) resolveTargetScope(nodeIDs []int64, single int64) ([]int64, error) {
	if nodeIDs == nil {
		if single <= 0 {
			return []int64{0}, nil
		}
		if cur, _ := h.Repo.GetNode(single); cur == nil {
			return nil, fmt.Errorf("节点 %d 不存在", single)
		}
		return []int64{single}, nil
	}
	if len(nodeIDs) == 0 {
		return []int64{0}, nil
	}
	seen := map[int64]bool{}
	out := make([]int64, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		if id <= 0 || seen[id] {
			continue
		}
		if cur, _ := h.Repo.GetNode(id); cur == nil {
			return nil, fmt.Errorf("节点 %d 不存在", id)
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		// 入参全是不合法的节点 id（比如 0 或重复），退化成全局而不是报错？
		// 不：那会在管理员"想指定节点却填错"时意外建出全局目标。
		return nil, fmt.Errorf("未选中任何有效节点")
	}
	return out, nil
}

func (h *Handler) handlePatchTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	// 全部字段可选：PATCH 语义 = 未提交的字段保持原值。
	// 早期只放开 name/enabled（A4 的"host/port/method 不可改"），后台因此
	// 只能"改名 + 停用 + 重建"三步走——填错一个端口号要重建一条目标。
	var req struct {
		Name       *string `json:"name"`
		Carrier    *string `json:"carrier"`
		Host       *string `json:"host"`
		Port       *int    `json:"port"`
		Method     *string `json:"method"`
		TimeoutMs  *int    `json:"timeout_ms"`
		Enabled    *bool   `json:"enabled"`
		ShowInBars *bool   `json:"show_in_bars"`
		// NodeIDs 是"多选节点范围"，**只在显式给出时**才会动作用域：
		//   字段缺省(nil) → 只改参数，作用域不变（节点详情页的「编辑」走这条路）
		//   [3]          → 范围就是节点 #3 这一条
		//   [3,7,9]      → 把这套参数同步到这三个节点（缺的建、有的覆盖）
		// 与 handleCreateTarget 的差别：那里 `[]` 表示"建一条全局目标"，而一条
		// **已有**目标不可能"改成没有节点"，所以这里 `[]` 直接报错而不是当 no-op——
		// 静默的 no-op 会让管理员以为范围已经改好了。
		NodeIDs []int64 `json:"node_ids"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}
	cur, err := h.Repo.GetProbeTarget(id)
	if err != nil || cur == nil {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "目标不存在")
		return
	}
	next := *cur
	if req.Name != nil {
		next.Name = sanitizeText(*req.Name, 32)
	}
	if req.Carrier != nil {
		next.Carrier = normalizeCarrier(*req.Carrier)
	}
	if req.Host != nil {
		next.Host = strings.TrimSpace(*req.Host)
	}
	if req.Port != nil {
		next.Port = *req.Port
	}
	if req.Method != nil {
		next.Method = strings.ToLower(strings.TrimSpace(*req.Method))
	}
	if req.TimeoutMs != nil {
		next.TimeoutMs = *req.TimeoutMs
	}
	if req.Enabled != nil {
		next.Enabled = *req.Enabled
	}
	if req.ShowInBars != nil {
		next.ShowInBars = *req.ShowInBars
	}
	// 校验放在"合并之后"：这样改名 + 改端口一起提交时，校验看到的是最终值，
	// 不会因为中间态（旧端口 + 新主机）误报。
	if err := validateTargetFields(next.Host, next.Port, next.Method, next.TimeoutMs); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, err.Error())
		return
	}

	// 范围同步的校验必须在写入**之前**全部做完：参数一旦落库才发现范围非法，
	// 管理员看到的是"保存失败但值已经变了"，比直接拒绝更糟。
	var want []int64
	if req.NodeIDs != nil {
		if next.NodeID <= 0 {
			writeErr(w, http.StatusBadRequest, model.ErrInvalidParam,
				"全局目标的作用域不可收窄：如需限定节点，请新建一条专属目标")
			return
		}
		if len(req.NodeIDs) == 0 {
			writeErr(w, http.StatusBadRequest, model.ErrInvalidParam,
				"范围不能为空：专属目标至少需要一个节点")
			return
		}
		want, err = h.resolveTargetScope(req.NodeIDs, next.NodeID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, err.Error())
			return
		}
		if !containsID(want, next.NodeID) {
			// 允许把目标移出自身节点会留下一堆孤儿延迟桶：读侧的 JOIN 守卫
			// （store_probe.go 的 LatencyBucketsForNode）会直接把它们过滤掉，
			// 表现为"这条目标的历史数据凭空消失了"，很难跟"没探过"区分开。
			writeErr(w, http.StatusBadRequest, model.ErrInvalidParam,
				fmt.Sprintf("不能把这条目标移出它自己的节点：请保持节点 #%d 处于勾选状态", next.NodeID))
			return
		}
	}

	if err := h.Repo.UpdateProbeTarget(&next); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "更新失败")
		return
	}
	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "target.update", fmt.Sprint(id), RealIP(r))

	ids := []int64{id}
	created, updated := 0, 0
	for _, nodeID := range want {
		if nodeID == next.NodeID {
			continue
		}
		sib, ferr := h.findSiblingTarget(&next, nodeID)
		if ferr != nil {
			writeErr(w, http.StatusInternalServerError, model.ErrInternal, "读取目标失败")
			return
		}
		if sib == nil {
			t := next
			t.ID = 0
			t.NodeID = nodeID
			nid, cerr := h.Repo.CreateProbeTarget(&t)
			if cerr != nil {
				// 与创建接口一样：已经建出来的不回收，并在错误里如实报出进度，
				// 否则管理员不知道到底同步了几个节点。
				writeErr(w, http.StatusInternalServerError, model.ErrInternal,
					fmt.Sprintf("同步到节点 #%d 失败（已成功 %d 条）：%s", nodeID, len(ids), cerr.Error()))
				return
			}
			_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "target.create", fmt.Sprint(nid), RealIP(r))
			ids = append(ids, nid)
			created++
			continue
		}
		// 只覆盖"这套参数"这几个字段，node_id 不动——它本来就在这个节点上。
		sib.Name, sib.Carrier = next.Name, next.Carrier
		sib.Host, sib.Port, sib.Method = next.Host, next.Port, next.Method
		sib.TimeoutMs, sib.Enabled, sib.ShowInBars = next.TimeoutMs, next.Enabled, next.ShowInBars
		if uerr := h.Repo.UpdateProbeTarget(sib); uerr != nil {
			writeErr(w, http.StatusInternalServerError, model.ErrInternal,
				fmt.Sprintf("同步到节点 #%d 失败：%s", nodeID, uerr.Error()))
			return
		}
		_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "target.update", fmt.Sprint(sib.ID), RealIP(r))
		ids = append(ids, sib.ID)
		updated++
	}
	// 返回更新后的完整目标，外加作用域同步的结果：前端据此提示
	// "已同步到 3 个节点（新建 2 条）"，不用再发一次 GET /probe-targets。
	writeJSON(w, http.StatusOK, map[string]any{
		"target": next, "ids": ids, "created": created, "updated": updated,
	})
}

// containsID 是 []int64 的成员判断。作用域列表都很短（节点数级别），
// 线性扫描比建 map 更省事，也不会引入"忘了关 map"这类问题。
func containsID(list []int64, v int64) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// findSiblingTarget 在指定节点上找"同一个目标"的既有专属目标，找不到返回 nil。
//
// 库里没有"目标组"字段，所以用「同名 + 同运营组」作为分组键。这不是新发明：
// 延迟点阵本来就是按 carrier 分组的（store_probe.go 的 LatencyBucketsForNode
// 用 COALESCE(t.carrier,'custom')），这两个字段是这套系统里既有的、用户可感知的
// 身份。只在匹配**同名 + 同运营组 + 同节点**时命中，误伤面很小。
//
// 命中多条时取 id 最小的那条：保证同一份数据每次都同步到同一条目标上，
// 不会在两条同名目标之间来回跳（那会让延迟曲线看着"忽高忽低"）。
func (h *Handler) findSiblingTarget(base *model.ProbeTarget, nodeID int64) (*model.ProbeTarget, error) {
	all, err := h.Repo.ListProbeTargets(nodeID)
	if err != nil {
		return nil, err
	}
	var best *model.ProbeTarget
	for _, t := range all {
		// ListProbeTargets(nodeID) 会连带返回全局目标，这里必须排除掉：
		// 把全局目标"覆盖"成本节点的专属参数会影响到所有节点。
		if t.ID == base.ID || t.NodeID != nodeID {
			continue
		}
		if t.Name != base.Name || normalizeCarrier(t.Carrier) != normalizeCarrier(base.Carrier) {
			continue
		}
		if best == nil || t.ID < best.ID {
			best = t
		}
	}
	return best, nil
}

func (h *Handler) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	if err := h.Repo.DeleteProbeTarget(id); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "删除失败")
		return
	}
	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "target.delete", fmt.Sprint(id), RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// 告警
// ---------------------------------------------------------------------------

func (h *Handler) handleListChannels(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Repo.ListChannels()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
		return
	}
	// 绝不返回 config_enc 或其中的凭证（9.4）。
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, map[string]any{"id": c.ID, "type": c.Type, "name": c.Name, "enabled": c.Enabled})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type    string          `json:"type"`
		Name    string          `json:"name"`
		Config  json.RawMessage `json:"config"`
		Enabled *bool           `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil || (req.Type != "telegram" && req.Type != "smtp") {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "type 必须为 telegram 或 smtp")
		return
	}
	enc, err := h.Keyring.Encrypt(req.Config)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "加密失败")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	id, err := h.Repo.InsertChannel(req.Type, sanitizeText(req.Name, 48), enc, enabled)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "创建失败")
		return
	}
	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "alert_channel.create", fmt.Sprint(id), RealIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (h *Handler) handlePatchChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	var req struct {
		Name    *string         `json:"name"`
		Config  json.RawMessage `json:"config"`
		Enabled *bool           `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}
	// PATCH 语义：未提交的字段保持原值（否则只改启停会把名称清空）。
	cur, err := h.findChannel(id)
	if err != nil || cur == nil {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "渠道不存在")
		return
	}
	name := cur.Name
	if req.Name != nil {
		name = sanitizeText(*req.Name, 48)
	}
	enabled := cur.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	var enc []byte
	if len(req.Config) > 0 {
		enc, err = h.Keyring.Encrypt(req.Config)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, model.ErrInternal, "加密失败")
			return
		}
	} else {
		enc = cur.ConfigEnc // 未提交 config 时保留原密文，避免被置空
	}
	if err := h.Repo.UpdateChannel(id, name, enc, enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "更新失败")
		return
	}
	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "alert_channel.update", fmt.Sprint(id), RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

// findChannel 按 ID 定位渠道行（channel 数量为个位数，线性查找足够）。
func (h *Handler) findChannel(id int64) (*store.ChannelRow, error) {
	rows, err := h.Repo.ListChannels()
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i], nil
		}
	}
	return nil, nil
}

func (h *Handler) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	if err := h.Repo.DeleteChannel(id); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "删除失败")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "非法 ID")
		return
	}
	ch, err := h.buildChannelByID(id)
	if err != nil || ch == nil {
		writeErr(w, http.StatusNotFound, model.ErrNotFound, "渠道不存在或配置无法解密")
		return
	}
	if err := ch.Send(r.Context(), alert.Message{Title: "XB-Probe 测试消息", Body: "渠道配置正常。"}); err != nil {
		writeErr(w, http.StatusBadGateway, "send_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleGetRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.Repo.LoadAlertRules()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
		return
	}
	// 每条规则额外带一个中文名。由**服务端**出（alert.KindLabel 是唯一来源）：
	// 前端自己写一份 kind→中文 的映射，将来加规则类型时必然只改一处。
	//
	// 嵌入 alert.Rule 而不是包一层 `rule` 字段：内嵌结构体的字段会被 JSON 展平，
	// 于是 PUT 回来的请求体形状不变（handlePutRules 仍解成 map[string]alert.Rule，
	// 多出来的 label 被忽略）——前端可以把 GET 的响应原样送回去。
	type ruleView struct {
		alert.Rule
		Label string `json:"label"`
	}
	out := make(map[string]ruleView, len(rules))
	for kind, rule := range rules {
		out[kind] = ruleView{Rule: rule, Label: alert.KindLabel(kind)}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) handlePutRules(w http.ResponseWriter, r *http.Request) {
	var rules map[string]alert.Rule
	if err := decodeJSON(r, &rules); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}
	for _, rule := range rules {
		raw, err := json.Marshal(rule.Params)
		if err != nil {
			continue
		}
		if err := h.Repo.SaveAlertRule(rule, string(raw)); err != nil {
			writeErr(w, http.StatusInternalServerError, model.ErrInternal, "保存失败")
			return
		}
	}
	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "alert_rules.update", "", RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleListEvents(w http.ResponseWriter, r *http.Request) {
	limit, offset := store.NormalizePage(
		queryInt(r, "limit", store.DefaultPageSize), queryInt(r, "offset", 0))
	events, meta, err := h.Repo.ListAlertEvents(limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
		return
	}
	// 返回信封而不是裸数组：分页必须带上 total，前端才算得出"第 3 / 12 页"。
	// limit/offset 回传的是**实际生效**的窗口（越界会被拉回最后一页），
	// 前端据此纠正自己的页码，避免"页码写着第 9 页、表里是第 3 页"。
	//
	// 每条事件额外带 kind_label：落库的 kind 是**状态键**，带参数的复合类型长这样——
	// `expire:30`、`traffic:80`、`probe_fail:12`。管理员在表里看到 `expire:30`
	// 根本不知道那是什么，而它的含义只存在于 alert.KindLabel 里。
	type eventView struct {
		alert.Event
		KindLabel string `json:"kind_label"`
	}
	items := make([]eventView, 0, len(events))
	for _, e := range events {
		items = append(items, eventView{Event: e, KindLabel: alert.KindLabel(e.Kind)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "limit": meta.Limit, "offset": meta.Offset, "total": meta.Total,
	})
}

// ---------------------------------------------------------------------------
// 系统
// ---------------------------------------------------------------------------

func (h *Handler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	all, err := h.Repo.AllSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
		return
	}
	// 不返回任何密钥类设置。
	delete(all, "master_key")

	// 合并出厂默认值，让后台设置页能显示“当前生效值”。
	// 库里存过的键优先，未存过的用默认值补齐；defaults 里没有的键（未来新增）原样保留。
	for k, def := range settingDefaults {
		if _, ok := all[k]; !ok {
			all[k] = def
		}
	}
	writeJSON(w, http.StatusOK, all)
}

// handleSettingsMeta 返回设置页需要的"可选项清单"（只读、不含任何敏感值）。
//
// 为什么不让前端自己写死一份：卡片键分布在 Go 的 model 包里，前端再抄一份
// 迟早会漂移——加了新卡片但后台勾选框里没有，管理员就再也关不掉它。
// 走接口的好处是清单的唯一来源就是 model.PublicCardList()。
func (h *Handler) handleSettingsMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		// 公开看板的卡片目录（settings.public.cards 的可选值）。
		"cards": model.PublicCardList(),
		// 公开字段白名单全集（settings.public.fields 的可选值）。
		"fields": model.PublicFieldNames(),
		// "不限制"哨兵值，前端渲染"全部展示"开关时要用。
		"cards_all": model.PublicCardsAll,
	})
}

func (h *Handler) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}
	// 跨字段校验必须在**写入之前**整体做完：逐键写的话，前一个键已经落库、
	// 后一个键才报错，就留下一个半配置状态。而站点形态的半配置状态恰好是
	// "管理员把自己锁在后台外面"——save 成功、页面随即 421，再也改不回来。
	if err := h.validateSiteLayoutFor(in); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, err.Error())
		return
	}
	now := time.Now().Unix()
	for k, v := range in {
		if !allowedSettingKey(k) {
			writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "不允许的设置项："+k)
			return
		}
		if err := validateSettingValue(k, v); err != nil {
			writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, err.Error())
			return
		}
		if err := h.Repo.SetSetting(k, v, now); err != nil {
			writeErr(w, http.StatusInternalServerError, model.ErrInternal, "保存失败")
			return
		}
	}
	h.logSiteLayoutChange(in)
	// 设置变更后各节点 config_version 自动重算（13.4.3）：无需手动自增。
	// 若使用了按节点缓存，应在此处清空；本实现按需计算，故无需额外操作。
	_ = h.Repo.Audit(now, actorOf(r), "settings.update", "", RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleListThemes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"active": h.Themes.Active(),
		"themes": h.Themes.List(),
	})
}

func (h *Handler) handleSetTheme(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}
	if err := h.Themes.SetActive(req.Name); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, err.Error())
		return
	}
	if err := h.Repo.SetSetting("theme.active", req.Name, time.Now().Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "保存失败")
		return
	}
	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "theme.set", req.Name, RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleStorageStats(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Repo.StorageStats()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "统计失败")
		return
	}
	stats := h.Writer.Stats()

	// 数据库文件实际占用（含 WAL 等）。
	dbPath := h.Repo.DB().Path()
	var dbSize int64
	if fi, err := os.Stat(dbPath); err == nil {
		dbSize = fi.Size()
	}

	// 按当前行数估算单节点每日占用。
	var nodeCount int64 = 1
	for _, t := range rows {
		if t.Name == "nodes" {
			nodeCount = maxInt64(1, t.Count)
			break
		}
	}
	var tsBytes int64
	for _, t := range rows {
		switch t.Name {
		case "metrics_raw", "metrics_1m", "metrics_10m", "latency_raw", "latency_10m", "traffic_daily":
			tsBytes += t.Count * t.BytesPerRow
		}
	}
	dailyPerNode := tsBytes / nodeCount

	writeJSON(w, http.StatusOK, map[string]any{
		"tables": rows,
		"writer": map[string]any{
			"queued": stats.Queued, "written": stats.Written,
			"dropped": stats.Dropped, "paused": stats.Paused,
		},
		"db_path":              dbPath,
		"db_size_bytes":        dbSize,
		"db_size_human":        fmtBytes(dbSize),
		"per_node_daily_bytes": dailyPerNode,
		"per_node_daily_human": fmtBytes(dailyPerNode),
	})
}

func (h *Handler) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	limit, offset := store.NormalizePage(
		queryInt(r, "limit", store.DefaultPageSize), queryInt(r, "offset", 0))
	rows, meta, err := h.Repo.ListAuditLog(limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "查询失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": rows, "limit": meta.Limit, "offset": meta.Offset, "total": meta.Total,
		"retention_days": int(h.Repo.GetIntSetting("audit.retention_days", 30)),
	})
}

// handleSetAuditRetention 修改审计日志保留期（6.5 补充）。
//
// 保留期决定了"多久之前的审计记录会被自动清理"，直接关系到能否追溯历史操作，
// 因此**必须密码（TOTP 开启时再加动态码）二次确认**——这与删除/恢复备份同一口径。
// 若"审计保留期变更"告警规则处于启用状态，每次修改都会发一条告警（见告警页开关）。
func (h *Handler) handleSetAuditRetention(w http.ResponseWriter, r *http.Request) {
	admin := h.reauth(w, r)
	if admin == nil {
		return
	}
	var req struct {
		Days int `json:"days"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Days < 1 || req.Days > 3650 {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "保留天数须为 1~3650 之间的整数")
		return
	}
	if err := h.Repo.SetSetting("audit.retention_days", fmt.Sprint(req.Days), time.Now().Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "保存失败")
		return
	}
	_ = h.Repo.Audit(time.Now().Unix(), admin.Username, "settings.update",
		"audit.retention_days="+fmt.Sprint(req.Days), RealIP(r))
	// 规则被关闭时 Fire 直接返回 nil——这就是"告警页可开关"的落点。
	if h.Alerts != nil {
		_ = h.Alerts.Fire(r.Context(), 0, alert.KindAuditRetention, alert.KindAuditRetention,
			alert.LevelInfo, fmt.Sprintf("审计日志保留期已改为 %d 天", req.Days), time.Now().Unix())
	}
	// 立刻按新保留期清理一次，让改动马上生效（否则最长要等下一小时的调度）。
	_, _ = h.Repo.PruneAuditLog(time.Now().AddDate(0, 0, -req.Days).Unix())
	w.WriteHeader(http.StatusNoContent)
}

// handlePruneAuditLog 立即按当前保留期清理一次审计日志（6.5 补充）。
//
// 与"改保留期"分开：管理员想马上回收空间、又不想改配置时用它。同样需二次确认。
func (h *Handler) handlePruneAuditLog(w http.ResponseWriter, r *http.Request) {
	if h.reauth(w, r) == nil {
		return
	}
	days := int(h.Repo.GetIntSetting("audit.retention_days", 30))
	n, err := h.Repo.PruneAuditLog(time.Now().AddDate(0, 0, -days).Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n, "retention_days": days})
}

// handleBackup 手动触发一次备份（6.6）。
//
// 手动备份**同样遵守 backup.keep**：不轮转的话，管理员多点几次"立即备份"
// 就会把磁盘塞满——而备份盘满往往是"主库写不进去"的前奏。
// 返回值里带上轮转结果，让操作者看得见"这次备份顺便删掉了哪些旧份"。
func (h *Handler) handleBackup(w http.ResponseWriter, r *http.Request) {
	info, err := h.Repo.CreateBackup(r.Context(), time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, err.Error())
		return
	}
	removed, pruneErr := h.Repo.PruneBackups(int(h.Repo.GetIntSetting("backup.keep", 7)))
	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "maintenance.backup", info.Name, RealIP(r))
	if len(removed) > 0 {
		_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "maintenance.backup_prune",
			strings.Join(removed, ","), RealIP(r))
	}
	resp := map[string]any{
		"name":       info.Name,
		"path":       info.Path,
		"size_bytes": info.SizeBytes,
		"pruned":     removed,
	}
	// 轮转失败不影响"备份已成功"，但要如实告诉管理员，否则磁盘会悄悄涨满。
	if pruneErr != nil {
		resp["prune_warning"] = pruneErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleListBackups 列出备份文件（6.6）。
//
// 只读，不涉及文件系统之外的信息；路径也一并返回，便于运维直接找到文件去做
// "拷贝到异地"这类操作——本项目刻意不做异地同步（那需要额外的凭证与协议），
// 把文件路径摆出来交给管理员自己的备份工具更符合"零依赖"的定位。
func (h *Handler) handleListBackups(w http.ResponseWriter, r *http.Request) {
	list, err := h.Repo.ListBackups()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "读取备份列表失败")
		return
	}
	total := int64(0)
	for _, b := range list {
		total += b.SizeBytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backups":    list,
		"count":      len(list),
		"total_size": total,
		"dir":        h.Repo.BackupDir(),
		"keep":       h.Repo.GetIntSetting("backup.keep", 7),
		"enabled":    h.Repo.GetSettingDefault("backup.enabled", "1") == "1",
		"hour":       h.Repo.GetIntSetting("backup.hour", 4),
	})
}

// handleDeleteBackup 删除单份备份（6.6）。
//
// 为什么需要它：备份只增不减会把备份盘撑满，而"备份盘满"通常是主库写不进去的
// 前奏。轮转（backup.keep）只按数量裁最旧的，无法处理"某一份备份是坏的/特别大/
// 包含误操作前的错误数据，想立刻清掉"这类诉求。
//
// 语义上是**单份删除**：路径参数只接受文件名，且必须通过 store.isBackupName
// 的形状校验（probe-YYYYMMDD-HHMMSS.db）。刻意不提供"清空全部备份"的接口——
// 那应该通过调小 backup.keep 让轮转去做，多一道配置确认就少一次不可逆的误操作。
func (h *Handler) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	// 删除备份属于不可逆操作，必须经过密码（TOTP 开启时再加动态码）二次确认。
	if h.reauth(w, r) == nil {
		return
	}
	name := r.PathValue("name")
	if err := h.Repo.DeleteBackup(name); err != nil {
		if errors.Is(err, store.ErrBackupNotFound) {
			writeErr(w, http.StatusNotFound, model.ErrNotFound, "备份不存在（可能已被轮转删除）")
			return
		}
		// 文件名非法与删除失败在这里合并成 400/500：非法名字是客户端问题，
		// 其余（权限、IO）是服务端问题。
		if strings.HasPrefix(err.Error(), "非法的备份文件名") ||
			strings.HasPrefix(err.Error(), "备份文件不在备份目录内") {
			writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, err.Error())
		return
	}
	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "maintenance.backup_delete", name, RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

// handleRestoreBackup 用指定备份文件覆盖恢复当前库（6.6 补充）。
//
// 为什么需要它：备份如果只是"能生成"，却没有任何途径"用回去"，那它只是
// 一份给人心理安慰的文件。灾难发生后才发现"原来没法恢复"是最坏的情况。
// 但恢复是**最高危**的操作（一次点错就把库还原到过去某个时刻），所以这里
// 强制密码 + 动态码二次确认，且在 store 层先校验备份完整性与版本一致性。
func (h *Handler) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	admin := h.reauth(w, r)
	if admin == nil {
		return
	}
	name := r.PathValue("name")
	if err := h.Repo.RestoreBackup(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrBackupNotFound) {
			writeErr(w, http.StatusNotFound, model.ErrNotFound, "备份不存在（可能已被轮转删除）")
			return
		}
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, err.Error())
		return
	}
	_ = h.Repo.Audit(time.Now().Unix(), admin.Username, "maintenance.backup_restore", name, RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

// handlePurgeData 清理早于指定时间的时序数据（6.6 补充）。
//
// 请求体：{"days": N} 或 {"before_date": "YYYY-MM-DD"}（二选一）。
//   - days：保留最近 N 天，删除更早的全部遥测流水；
//   - before_date：删除该日期之前（不含当天）的全部遥测流水。
//
// 这是不可逆的数据削减，同样需要密码 + 动态码二次确认。
func (h *Handler) handlePurgeData(w http.ResponseWriter, r *http.Request) {
	admin := h.reauth(w, r)
	if admin == nil {
		return
	}
	var req struct {
		Days       int64  `json:"days"`
		BeforeDate string `json:"before_date"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}
	now := time.Now().Unix()
	var beforeTs int64
	switch {
	case req.BeforeDate != "":
		t, err := time.Parse("2006-01-02", req.BeforeDate)
		if err != nil {
			writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "日期格式应为 YYYY-MM-DD")
			return
		}
		beforeTs = t.Unix()
	case req.Days > 0:
		beforeTs = now - req.Days*86400
	default:
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请指定保留天数(days)或截止日期(before_date)")
		return
	}
	if beforeTs <= 0 || beforeTs >= now {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "截止时间无效（必须是一个过去的时间点）")
		return
	}
	counts, err := h.Repo.PurgeOldData(beforeTs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, err.Error())
		return
	}
	total := int64(0)
	for _, c := range counts {
		total += c
	}
	_ = h.Repo.Audit(time.Now().Unix(), admin.Username, "maintenance.data_purge",
		fmt.Sprintf("before=%d total=%d", beforeTs, total), RealIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"deleted": counts, "total": total})
}

// handleHeartbeatStatus 返回外部心跳（A5）的当前状态。
//
// 为什么需要这个只读接口：心跳是"Master 自己还活着"的信号，但它**只写日志**。
// 管理员在后台填完 heartbeat_url 后无法判断到底通不通——
// 是配错了地址、还是外部服务返回了 401、还是根本还没到第一个 5 分钟。
// 这个接口把发送侧的内存状态暴露出来，让"配了但没生效"这类静默失败可见。
//
// 未注入 Heartbeat（比如测试构造的 Handler）时返回 configured=false 而不是报错，
// 这样前端只需处理一种形态。
func (h *Handler) handleHeartbeatStatus(w http.ResponseWriter, r *http.Request) {
	if h.Heartbeat == nil {
		writeJSON(w, http.StatusOK, model.AdminHeartbeat{})
		return
	}
	writeJSON(w, http.StatusOK, h.Heartbeat.HeartbeatStatus())
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func actorOf(r *http.Request) string {
	if s := adminFrom(r); s != nil {
		return fmt.Sprintf("admin:%d", s.AdminID)
	}
	return "system"
}

func validCycle(c string) bool {
	switch c {
	case model.CycleMonthly, model.CycleQuarterly, model.CycleYearly, model.CycleOnce, model.CycleLifetime:
		return true
	}
	return false
}

func validTrafficMode(m string) bool {
	switch m {
	case model.TrafficBoth, model.TrafficUp, model.TrafficDown, model.TrafficMax:
		return true
	}
	return false
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// fmtBytes 把字节数格式化为人类可读字符串（与前端 fmtBytes 保持一致）。
func fmtBytes(v int64) string {
	if v <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(v)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", f, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func normalizeCarrier(c string) string {
	switch c {
	case "telecom", "unicom", "mobile":
		return c
	default:
		return "custom"
	}
}

// validateTargetFields 校验探测目标字段（13.4.6）。
func validateTargetFields(host string, port int, method string, timeoutMs int) error {
	if host == "" || len(host) > 253 {
		return fmt.Errorf("host 非法")
	}
	if method != "tcp" && method != "icmp" {
		return fmt.Errorf("method 必须是 tcp 或 icmp")
	}
	if method == "tcp" && (port < 1 || port > 65535) {
		return fmt.Errorf("tcp 目标必须指定 1~65535 的端口")
	}
	if timeoutMs < 100 || timeoutMs > 30000 {
		return fmt.Errorf("timeout_ms 应在 100~30000")
	}
	return nil
}

// billingLoc 解析计费所用的时区：节点自己的 tz 优先，回退全局 timezone 设置，
// 都为空则用 Master 所在系统时区（与 6.2 的约定一致）。
func (h *Handler) billingLoc(tz string) *time.Location {
	if tz == "" {
		tz = h.Repo.GetSettingDefault("timezone", "")
	}
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
	}
	return time.Local
}

// refreshBillingRuntime 把计费信息写入内存运行态，供公开快照纯内存构建（A9）。
//
// 保存计费后立即重算 TrafficUsed / TrafficUsedPct，而不是等下一次调度器慢循环：
// 管理员刚把倍率从 1 改成 2，页面上的百分比应当马上翻倍，否则他会以为没保存成功。
// 原始计量值取运行态里的 PeriodRx/PeriodTx（与调度器同源，都是 tracker.Used）。
//
// 百分比走 state.Percent，与调度器同一个实现：两处各写一份整数除法时，
// "保存计费后"与"下一次慢循环后"会显示出两个不同的数字。
func (h *Handler) refreshBillingRuntime(nodeID int64, b store.BillingRow) {
	_ = h.Registry.Update(nodeID, func(n *state.NodeRuntime) {
		n.PriceMinor = b.PriceMinor
		n.Currency = b.Currency
		n.Cycle = b.Cycle
		n.TrafficQuota = b.TrafficQuota
		n.ExpireAt = b.ExpireAt
		if b.Cycle == model.CycleLifetime {
			n.ExpireAt = 0
		}
		// 不限量时用量仍要算出来给看板显示，只是没有百分比（Percent 返回 0）。
		n.TrafficUsed = traffic.Billable(n.PeriodRx, n.PeriodTx, b.TrafficMode, b.TrafficMultiplier)
		n.TrafficUsedPct = state.Percent(n.TrafficUsed, b.TrafficQuota)
	})
}
