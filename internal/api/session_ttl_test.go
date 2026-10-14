package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/koyoca/xb-probe/internal/auth"
)

// 后台「登录有效期」设置项的回归护栏（用户要求："新增后台可设置登录后过期时间，
// 过期后必须重新登录"）。
//
// 这条链上有三处各自独立、任何一处断了都"看起来生效但实际没生效"：
//  1. 设置项 → 服务端判定用的时长（h.sessionLimits）；
//  2. 同一个时长 → 下发 Cookie 的 Max-Age（两者不一致 = 浏览器还带着 Cookie
//     而服务端已经判过期，后台每 3 秒刷一屏 401）；
//  3. 会话行本身也要按这个时长判（只靠 Cookie 的话，拿到过 Cookie 值的人——
//     备份、日志、被偷的设备——可以绕过浏览器继续用）。
func TestSessionTTLSettingControlsExpiry(t *testing.T) {
	h, repo := newTOTPTestHandler(t)
	now := time.Now().Unix()

	// 默认值：与 auth 的出厂值（12 小时 / 空闲 2 小时）一致，
	// 这样老部署升级后会话时长不会悄悄变化。
	if got := h.sessionLimits().Absolute; got != 12*time.Hour {
		t.Errorf("默认登录有效期 = %v，期望 12 小时", got)
	}
	if got := h.sessionLimits().Idle; got != auth.SessionIdleTTL {
		t.Errorf("空闲上限 = %v，期望 %v", got, auth.SessionIdleTTL)
	}

	// 配成 48 小时：登录下发的 Cookie 必须跟着走。
	if err := repo.SetSetting("admin.session_ttl_hours", "48", now); err != nil {
		t.Fatalf("写设置失败：%v", err)
	}
	if got := h.sessionLimits().Absolute; got != 48*time.Hour {
		t.Fatalf("设置未生效：登录有效期 = %v，期望 48 小时", got)
	}

	req := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(`{"username":"ops","password":"`+testPw+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleLogin(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("登录失败（%d）：%s", w.Code, w.Body.String())
	}

	var plain string
	maxAge := -1
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.PlainCookieName {
			plain, maxAge = c.Value, c.MaxAge
		}
	}
	if plain == "" {
		t.Fatal("登录成功但没有下发会话 Cookie")
	}
	if maxAge != 48*3600 {
		t.Errorf("Cookie Max-Age = %d 秒，期望 48 小时（%d 秒）："+
			"Cookie 与服务端的有效期必须是同一个数", maxAge, 48*3600)
	}

	// 会话行本身：刚登录时有效；把"登录时刻"往回拨 13 小时（超过 12 小时的有效期）
	// 之后必须判过期——这条判据不依赖 Cookie，是"过期后必须重新登录"的真正兑现。
	sess, err := repo.LookupSession(auth.HashToken(plain))
	if err != nil || sess == nil {
		t.Fatalf("读不到刚建的会话：%v", err)
	}
	if err := auth.CheckSession(sess, time.Now(), h.sessionLimits()); err != nil {
		t.Errorf("刚登录的会话就被判无效：%v", err)
	}
	expired := *sess
	expired.CreatedAt -= int64(49 * 3600) // 超过配的 48 小时
	if err := auth.CheckSession(&expired, time.Now(), h.sessionLimits()); err == nil {
		t.Error("超过登录有效期的会话仍被判有效：设置项没有真正决定要不要重新登录")
	}

	// 库里被手工写成非法值（0 / 负数 / 垃圾）：回到默认，而不是"所有人立刻被踢下线"。
	for _, bad := range []string{"0", "-3", "abc"} {
		if err := repo.SetSetting("admin.session_ttl_hours", bad, now); err != nil {
			t.Fatalf("写设置失败：%v", err)
		}
		if got := h.sessionLimits().Absolute; got != 12*time.Hour {
			t.Errorf("设置成 %q 时登录有效期 = %v，期望回落到默认 12 小时", bad, got)
		}
	}
}
