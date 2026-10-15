package api

import (
	"path/filepath"
	"strings"
	"testing"
)

// TOTP 相关的**前端**护栏。
//
// 这一层最危险的失败模式全是"接口通了但接线接错"：路径写成了另一个并未注册的
// 前缀、或者渲染顺序反了把刚下发的明文恢复码抹掉。两种都不会被后端测试抓到
// （后端只保证"收到这个请求就正确响应"），所以判据必须跨文件。

// TestFrontendTOTPStatusPathIsRegistered 钉"前端拉 TOTP 状态的路径必须真的注册过"。
//
// 真事：refreshTotpStatus 曾写成 api('/maintenance/totp')，而 TOTP 端点全部挂在
// 账户安全之下（/account/totp*）。该路径没有注册 → 404 → .catch 把
// state.totpEnabled 恒置为 false → 二次确认对话框**永远不显示动态验证码输入框**
// → 提交空的 code → 删备份/恢复备份/清历史数据/改审计保留期**全部**报
// "动态验证码或恢复码错误"（即便密码与动态码都对）。
//
// 一个字符之差就能让一整组破坏性维护操作全部不可用，且现象发生在别的页面，
// 排查时几乎不会想到是这里——所以专门钉住。
func TestFrontendTOTPStatusPathIsRegistered(t *testing.T) {
	js := readSrc(t, filepath.Join("adminui", "admin.js"))

	// 1) 不许再出现任何指向 TOTP 的 /maintenance/* 调用（那是错的命名空间）。
	if strings.Contains(js, "/maintenance/totp") {
		t.Fatal("admin.js 里仍有 /maintenance/totp：TOTP 端点挂在 /account/totp*，" +
			"该路径未注册，会 404 并让 state.totpEnabled 恒为 false（破坏性操作全部报“动态码错误”）")
	}

	// 2) 前端确实调用 /account/totp。
	if !strings.Contains(js, "api('/account/totp')") {
		t.Fatal("admin.js 没有调用 GET /account/totp，TOTP 启用状态无从获取")
	}

	// 3) 后端真的注册了这个路径（前端与路由双向对齐）。
	router := readSrc(t, "router.go")
	if !strings.Contains(router, `"GET /api/admin/v1/account/totp"`) {
		t.Fatal("router.go 未注册 GET /api/admin/v1/account/totp：前端调用会 404")
	}
}

// TestTOTPEnableShowsRecoveryCodesAfterRender 钉"恢复码必须在渲染完成之后才写入"。
//
// 真事：启用成功的回调里先 showRecoveryCodes(res.recovery_code) 再 loadTOTP()，
// 而 loadTOTP → renderRecovery 第一件事就是 clear($('recovery-box')) ——
// 刚下发、只此一次可见的明文恢复码会在同一帧里被抹掉，表现为
// "成功开启后没看到恢复码，直接消失了"（用户实测）。丢了就只能重新生成。
func TestTOTPEnableShowsRecoveryCodesAfterRender(t *testing.T) {
	js := readSrc(t, filepath.Join("adminui", "admin.js"))
	// 切出"启用对话框 → 恢复码展示"这一段，只在区间内判定，
	// 免得文件别处的同名调用替被测实现作证。
	seg := regionOf(t, js, "function showTOTPSetupDialog", "function showRecoveryCodes")

	// 回调里必须由 loadTOTP() 驱动展示（loadTOTP 返回 Promise 是这套顺序的前提）。
	if !strings.Contains(seg, "loadTOTP().then(function () {") {
		t.Fatal("启用成功后没有在 loadTOTP() 完成后再展示恢复码：" +
			"renderRecovery 的 clear() 会把刚下发的明文恢复码抹掉")
	}
	// 展示必须排在 loadTOTP() 之后（用出现位置判定顺序）。
	render := strings.Index(seg, "loadTOTP().then(function () {")
	show := strings.Index(seg, "showRecoveryCodes(res.recovery_code)")
	if show < 0 {
		t.Fatal("启用成功的回调里没有调用 showRecoveryCodes(res.recovery_code)")
	}
	if show < render {
		t.Fatal("showRecoveryCodes 排在 loadTOTP() 之前：会被随后的 renderRecovery 清空")
	}

	// loadTOTP 必须把 Promise 传出去，否则上面的 .then 无从挂载。
	if !strings.Contains(js, "return api('/account/totp').then(function (r) {") {
		t.Fatal("loadTOTP 没有 return Promise：调用方无法在渲染完成后再写入恢复码")
	}
}

// TestUnarchivePathIsRegistered 钉"解除归档"的前后端接线。
//
// 背景（用户实测的死锁）：`xb-probe-agent uninstall` 会归档节点并吊销令牌，
// 重新安装后上报被 `nodes.status='archived'` 拒掉 → 节点永远装不上。
// 修法是双保险：enroll 时自动解除 + 后台一个手工入口。
// 手工入口最容易漏的就是接线（前端调了后端没注册 = 404 = 点按钮没反应），
// 与 TOTP 那次是同一个坑，所以放在同一处判据里钉。
func TestUnarchivePathIsRegistered(t *testing.T) {
	js := readSrc(t, filepath.Join("adminui", "admin.js"))
	router := readSrc(t, "router.go")

	if !strings.Contains(js, "api('/nodes/' + id + '/unarchive', { method: 'POST' })") {
		t.Error("admin.js 没有调用 POST /nodes/{id}/unarchive：界面上没有解除归档的出口")
	}
	if !strings.Contains(router, `"POST /api/admin/v1/nodes/{id}/unarchive"`) {
		t.Fatal("router.go 未注册 POST /api/admin/v1/nodes/{id}/unarchive：前端调用会 404")
	}
	// 已归档的节点必须给出解入口，否则用户只能删节点重建。
	if !strings.Contains(js, "if (n.archived_at) {") ||
		!strings.Contains(js, "解除归档") {
		t.Error("节点详情没有按归档状态切换「归档 / 解除归档」按钮")
	}
	// 审计动作要有中文名（否则审计页显示裸英文键）。
	if !strings.Contains(js, "'node.unarchive': '解除归档'") {
		t.Error("审计动作 node.unarchive 缺中文映射")
	}
}
