package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/yourorg/probe/internal/auth"
	"github.com/yourorg/probe/internal/model"
	"github.com/yourorg/probe/internal/store"
)

// TOTP 启用流程与恢复码（6.2）。
//
// 背景：登录侧早就支持 TOTP（handleLogin 里 len(admin.TOTPSecret)>0 时要求动态码），
// 但**没有任何接口能往 totp_secret 里写值**——这个开关实际上是永远打不开的。
// 而且一旦用其它手段写进去，手机丢了就没有任何兜底通道，管理员会被永久锁在门外。
//
// 本文件补三件事：
//  1. GET  /maintenance/totp            —— 查看当前状态（是否启用、剩余恢复码数）
//  2. POST /maintenance/totp/setup      —— 生成密钥（含 otpauth URI + 二维码），**但不启用**
//  3. POST /maintenance/totp/enable     —— 用一次动态码确认后启用，并下发恢复码
//     POST /maintenance/totp/disable    —— 需密码 + 动态码/恢复码，关闭 TOTP
//     POST /maintenance/recovery-codes  —— 重新生成恢复码（作废旧码）
//
// 为什么"生成密钥"与"启用"要分成两步（setup / enable）：
// 直接生成即启用的话，管理员如果扫错码或压根没扫成功，会在**还没验证过**
// 的情况下把自己锁在外面。两步走强制"先用新密钥成功过一次动态码"，
// 确保管理员手上的 App 确实能算出正确的码。

// handleTOTPStatus 返回当前管理员的 TOTP 状态。
func (h *Handler) handleTOTPStatus(w http.ResponseWriter, r *http.Request) {
	s := adminFrom(r)
	admin, err := h.Repo.GetAdminByID(s.AdminID)
	if err != nil || admin == nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "读取账户失败")
		return
	}
	total, unused, err := h.Repo.CountRecoveryCodes(s.AdminID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "读取恢复码失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":              len(admin.TOTPSecret) > 0,
		"recovery_total":       total,
		"recovery_unused":      unused,
		"recovery_recommended": recoveryCodeCount,
		// 提醒阈值：剩余恢复码 ≤ 2 时前端显示橙色警示。
		// 恢复码只有在下发那一次可见，用完就必须重新生成，
		// 不做提醒的话管理员往往到"真的需要"时才发现已经用光了。
		"recovery_low": unused <= 2,
	})
}

// recoveryCodeCount 是每次下发的恢复码数量。
// 10 个足够撑过"手机丢了 + 找回新手机 + 重新绑定"的全过程，
// 又不至于多到没人愿意抄。
const recoveryCodeCount = 10

// handleTOTPSetup 生成一个新的 TOTP 密钥并返回给前端，**但不写入数据库**。
//
// 密钥由前端暂存，等 handleTOTPEnable 收到一次正确的动态码后才落库。
// 这样"扫错码 / 没扫成功"不会把管理员锁在门外。
//
// 返回的 otpauth URI 供手工录入/校验，同时**由服务端直接渲染二维码 SVG**：
// 前端的 CSP 是 default-src 'self'，不能引 CDN 的二维码库，也不允许 data: 图片，
// 因此二维码必须是内联 SVG。放在服务端生成而不是前端手写 QR 编码，
// 是因为二维码"画得出来但扫不出来"时极难排查——服务端这份已用独立参考实现
// 逐模块比对过，前端只负责把 SVG 插进 DOM。
func (h *Handler) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	s := adminFrom(r)
	admin, err := h.Repo.GetAdminByID(s.AdminID)
	if err != nil || admin == nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "读取账户失败")
		return
	}
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "生成密钥失败")
		return
	}
	uri := auth.OTPAuthURI(secret, admin.Username)
	// 二维码渲染失败不能阻断绑定流程：密钥文本仍可手工录入，
	// 前端拿到空字符串时会自动降级为"只显示密钥"的提示。
	svg, svgErr := auth.QRSVG(uri, 4)
	if svgErr != nil {
		if h.Logger != nil {
			h.Logger.Warn("渲染 TOTP 二维码失败", "err", svgErr)
		}
		svg = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"secret":     secret,
		"otpauth":    uri,
		"qr_svg":     svg,
		"issuer":     auth.TOTPIssuer,
		"account":    admin.Username,
		"period":     auth.TOTPPeriod,
		"digits":     6,
		"algorithm":  "SHA1",
		"recovering": false,
	})
}

// handleTOTPEnable 校验一次动态码后正式启用 TOTP，并下发一批恢复码。
//
// 请求体：{"secret": "<setup 拿到的 base32>", "code": "123456"}
//
// 恢复码明文**只在这一个响应里出现**，之后库里只有 SHA-256 哈希，
// 任何接口都无法再取回——这是与 node_tokens 相同的安全口径。
func (h *Handler) handleTOTPEnable(w http.ResponseWriter, r *http.Request) {
	s := adminFrom(r)
	var req struct {
		Secret string `json:"secret"`
		Code   string `json:"code"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}

	secret, err := auth.NormalizeTOTPSecret(req.Secret)
	if err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, err.Error())
		return
	}

	now := time.Now()
	// 必须先用这个密钥成功验证一次动态码，才允许落库。
	// 否则扫错码的管理员会在"自己还没验证通过"的状态下被锁在门外。
	if !auth.VerifyTOTP([]byte(secret), strings.TrimSpace(req.Code), now) {
		writeErr(w, http.StatusBadRequest, "invalid_totp",
			"动态验证码不正确，请确认认证器 App 已同步时间后重试")
		return
	}

	if err := h.Repo.SetTOTPSecret(s.AdminID, []byte(secret)); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "保存密钥失败")
		return
	}

	codes, err := h.issueRecoveryCodes(s.AdminID, now.Unix())
	if err != nil {
		// 密钥已保存、恢复码生成失败：这是"能登录但没有兜底码"的降级状态，
		// 必须显式告知，让管理员自己再点一次"重新生成恢复码"。
		_ = h.Repo.Audit(now.Unix(), actorOf(r), "auth.totp_enable", "recovery_failed", RealIP(r))
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": true,
			"warning": "TOTP 已启用，但恢复码生成失败，请点「重新生成恢复码」：" + err.Error(),
		})
		return
	}

	_ = h.Repo.Audit(now.Unix(), actorOf(r), "auth.totp_enable", "", RealIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":       true,
		"recovery_code": codes, // 明文只此一次
		"count":         len(codes),
	})
}

// handleTOTPDisable 关闭 TOTP。
//
// 请求体：{"password": "...", "code": "<动态码或恢复码>"}
//
// 双重确认是刻意的：关闭二次验证本身就是在降低账户安全性，
// 只凭一个已登录会话就能关掉的话，"会话被劫持"会直接升级为"永久失去保护"。
// 密码 + 动态码/恢复码的组合确保操作者确实是账户持有人。
//
// 允许用恢复码关闭：手机丢了又想关掉重绑，这是唯一的自救路径。
func (h *Handler) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	s := adminFrom(r)
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}

	admin, err := h.Repo.GetAdminByID(s.AdminID)
	if err != nil || admin == nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "读取账户失败")
		return
	}
	if err := auth.VerifyPassword(req.Password, admin.PWHash); err != nil {
		_ = h.Repo.RecordLoginFailure(admin.ID, 5, 15*60, time.Now().Unix())
		_ = h.Repo.Audit(time.Now().Unix(), admin.Username, "auth.totp_disable_fail", "bad_password", RealIP(r))
		writeErr(w, http.StatusUnauthorized, "unauthorized", "密码错误")
		return
	}
	if !h.checkSecondFactor(admin, req.Code, time.Now()) {
		_ = h.Repo.RecordLoginFailure(admin.ID, 5, 15*60, time.Now().Unix())
		writeErr(w, http.StatusUnauthorized, "invalid_totp", "动态验证码或恢复码不正确")
		return
	}

	if err := h.Repo.SetTOTPSecret(s.AdminID, nil); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "关闭失败")
		return
	}
	// 关闭 TOTP 时一并清空恢复码：留着它们等于留了一把"能重新登录"的钥匙，
	// 而管理员已经明确表示不再需要二次验证了。
	if _, err := h.Repo.ReplaceRecoveryCodes(s.AdminID, nil, time.Now().Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "清理恢复码失败")
		return
	}
	_ = h.Repo.ResetLoginFailures(s.AdminID)
	_ = h.Repo.Audit(time.Now().Unix(), actorOf(r), "auth.totp_disable", "", RealIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
}

// handleRegenerateRecoveryCodes 重新生成恢复码（旧码全部作废）。
//
// 请求体：{"password": "..."}
// 必须带密码：恢复码是"绕过动态验证器直接登录"的凭证，
// 能凭一个已登录会话就无限重置它们，等于把二次验证降级成摆设。
func (h *Handler) handleRegenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	s := adminFrom(r)
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}
	admin, err := h.Repo.GetAdminByID(s.AdminID)
	if err != nil || admin == nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "读取账户失败")
		return
	}
	if err := auth.VerifyPassword(req.Password, admin.PWHash); err != nil {
		_ = h.Repo.RecordLoginFailure(admin.ID, 5, 15*60, time.Now().Unix())
		_ = h.Repo.Audit(time.Now().Unix(), admin.Username, "auth.recovery_regen_fail", "bad_password", RealIP(r))
		writeErr(w, http.StatusUnauthorized, "unauthorized", "密码错误")
		return
	}
	if len(admin.TOTPSecret) == 0 {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "尚未启用 TOTP，无需恢复码")
		return
	}

	now := time.Now().Unix()
	codes, err := h.issueRecoveryCodes(s.AdminID, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "生成失败")
		return
	}
	_ = h.Repo.Audit(now, actorOf(r), "auth.recovery_regen", "", RealIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"recovery_code": codes,
		"count":         len(codes),
	})
}

// issueRecoveryCodes 生成一批恢复码，存哈希、返回明文。
func (h *Handler) issueRecoveryCodes(adminID int64, now int64) ([]string, error) {
	codes, err := auth.GenerateRecoveryCodes(recoveryCodeCount)
	if err != nil {
		return nil, err
	}
	hashes := make([][]byte, 0, len(codes))
	for _, c := range codes {
		hashes = append(hashes, auth.HashRecoveryCode(c))
	}
	if _, err := h.Repo.ReplaceRecoveryCodes(adminID, hashes, now); err != nil {
		return nil, err
	}
	return codes, nil
}

// checkSecondFactor 校验"动态码或恢复码"，二者任一通过即算通过。
//
// 恢复码验证成功即被消费（单次使用）。这里刻意**不**先按输入长度分流：
// 动态码固定 6 位、恢复码去掉连字符后 12 位，看着可以靠长度区分，
// 但那样会让"前面正好 6 位的输入"走进唯一一条路径，
// 语义上也不该假定恢复码一定长于动态码。依次尝试两条路径更稳。
func (h *Handler) checkSecondFactor(admin *store.Admin, input string, now time.Time) bool {
	input = strings.TrimSpace(input)
	if input == "" {
		return false
	}
	// 1) 先按动态码校验（成本最低，且是绝大多数情况）。
	if auth.VerifyTOTP(admin.TOTPSecret, input, now) {
		return true
	}
	// 2) 再按恢复码校验。哈希的是规范化形式，两侧必须共用同一个函数
	//    （与注册码踩过的坑同型：签发侧规范化、核验侧原文 → 全部静默失效）。
	if _, ok, err := h.Repo.ConsumeRecoveryCode(auth.HashRecoveryCode(input), now.Unix()); err == nil && ok {
		return true
	}
	return false
}
