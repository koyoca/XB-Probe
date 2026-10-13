package theme

import (
	"strings"
	"testing"
)

// 后台"重新签发令牌"必须走站内对话框，不能再用原生 alert/confirm。
//
// 这不是审美问题，是两个实际的不可用场景：
//   - 浏览器可以"阻止此页面再显示对话框"。此后 confirm 静默返回 false、
//     alert 什么都不显示 —— 点了按钮界面毫无反应，管理员只会以为按钮坏了。
//     而"重新签发令牌"的结果（注册码）**只能从这个弹窗里拿到**，
//     被吞掉就等于这个功能不可用。
//   - 深色主题下原生弹窗是刺眼的白底，且注册码无法选中/复制。
func TestAdminReenrollUsesInPageDialog(t *testing.T) {
	js := readFrontend(t, "internal/api/adminui/admin.js")

	body := funcBodyOf(t, js, "function reenroll(")
	if body == "" {
		t.Fatal("admin.js 里找不到 reenroll —— 判据与源码结构脱节了")
	}
	for _, native := range []string{"alert(", "confirm(", "prompt("} {
		if strings.Contains(body, native) {
			t.Errorf("reenroll 里还在用原生 %s：\n"+
				"  被「阻止此页面再显示对话框」吞掉之后界面毫无反应，\n"+
				"  而注册码只有这个弹窗会显示 —— 等于功能不可用。", native)
		}
	}
	if !strings.Contains(body, "askDialog(") {
		t.Error("reenroll 没有用站内对话框（askDialog）")
	}
	// 签发是"改状态"的操作，必须先问一句：以前是点一下就已经签发了，
	// 而用这个按钮的人往往正是怀疑令牌泄漏的那一个。
	if !strings.Contains(body, "hideCancel") {
		t.Error("reenroll 没有区分「确认」与「展示结果」两种形态：\n" +
			"  确认那一步必须有取消（否则一点就签发），展示那一步不该有取消。")
	}
	if !strings.Contains(body, "'xb-probe-agent reenroll --code '") {
		t.Error("reenroll 没有把要照抄到节点上的命令给出来（管理员还需要自己去猜命令格式）")
	}
}

// TestAdminAskDialogIsWiredToTheMarkup 钉住对话框的 DOM 契约。
//
// askDialog 是按 id 取元素的（$('ask-modal') 等）。改 HTML 时漏掉一个 id，
// 症状是 JS 在运行时抛 null 异常、对话框永远打不开 —— 而这是纯前端，
// 没有编译期检查能拦住。
func TestAdminAskDialogIsWiredToTheMarkup(t *testing.T) {
	html := readFrontend(t, "internal/api/adminui/index.html")
	js := readFrontend(t, "internal/api/adminui/admin.js")

	// askDialog / closeAskDialog 里引用到的每个 id 都必须在 HTML 里存在。
	for _, id := range []string{
		"ask-modal", "ask-title", "ask-desc", "ask-value-wrap",
		"ask-value", "ask-copy", "ask-extra", "ask-ok", "ask-cancel",
	} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 里缺少 id=%q：askDialog 会在运行时取到 null", id)
		}
	}
	// 反方向：注册码是凭据，对话框关掉之后不该还留在 DOM 里。
	if !strings.Contains(js, "setText($('ask-value'), '')") {
		t.Error("closeAskDialog 没有清空 ask-value：注册码会留在 DOM 里等着被下一次读屏/截图带出去")
	}
	// 剪贴板 API 只在安全上下文可用（纯 HTTP 的内网后台用不了），必须有降级。
	if !strings.Contains(js, "navigator.clipboard") || !strings.Contains(js, "selectAskValue()") {
		t.Error("复制按钮没有降级路径：纯 HTTP 部署下 navigator.clipboard 不可用，点击会毫无反应")
	}
}
