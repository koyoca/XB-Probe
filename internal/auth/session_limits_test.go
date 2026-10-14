package auth

import (
	"testing"
	"time"
)

// 「登录有效期」的回归护栏（用户要求："新增后台可设置登录后过期时间，过期后必须重新登录"）。
//
// 这条需求的要害在于**两条线**：空闲上限（页面停在那里不动）与绝对上限（从登录那一刻
// 算起）。只做前者，改设置的界面看着像生效了，实际"一直在用就永不过期"；
// 只做后者又不续期，"一直在用也会被掐断"。
func TestSessionLimitsEnforceBothDeadlines(t *testing.T) {
	now := time.Now()
	lim := SessionLimits{Idle: 2 * time.Hour, Absolute: 12 * time.Hour}

	// 刚登录：有效。
	fresh := &Session{CreatedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
	if err := CheckSession(fresh, now, lim); err != nil {
		t.Errorf("刚登录的会话被判无效：%v", err)
	}

	// 空闲超时：距上次活动超过 Idle。
	idle := &Session{CreatedAt: now.Add(-30 * time.Minute).Unix(), ExpiresAt: now.Add(-time.Minute).Unix()}
	if err := CheckSession(idle, now, lim); err == nil {
		t.Error("空闲超过上限的会话仍被判有效")
	}

	// 绝对超时：**一直在用**（空闲截止时间刚续过）但已超过登录有效期。
	// 这一条正是"过期后必须重新登录"的兑现方式——只看 ExpiresAt 的实现会放过它。
	used := &Session{CreatedAt: now.Add(-13 * time.Hour).Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
	if err := CheckSession(used, now, lim); err == nil {
		t.Error("超过登录有效期（绝对上限）的会话仍被判有效：用户改的时长没有真正生效")
	}

	// Absolute = 0 表示"不设绝对上限"（仅空闲过期），此时上面那个会话应当有效。
	if err := CheckSession(used, now, SessionLimits{Idle: 2 * time.Hour}); err != nil {
		t.Errorf("Absolute=0 时不该按绝对上限判过期：%v", err)
	}
}

// 续期不能把绝对上限架空：一个永不离线的页签也必须在该下线的时候下线。
func TestTouchDeadlineNeverExceedsAbsolute(t *testing.T) {
	now := time.Now()
	lim := SessionLimits{Idle: 2 * time.Hour, Absolute: 12 * time.Hour}

	// 登录 11 小时后还在活动：续期结果必须被夹在"登录 + 12 小时"，而不是 now + 2 小时。
	late := &Session{CreatedAt: now.Add(-11 * time.Hour).Unix(), ExpiresAt: now.Unix()}
	got := TouchDeadline(late, now, lim)
	want := late.CreatedAt + int64(lim.Absolute.Seconds())
	if got != want {
		t.Errorf("续期越过了绝对上限：得到 %d，期望被夹在 %d", got, want)
	}

	// 正常情况下就是 now + Idle。
	early := &Session{CreatedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Unix()}
	if got, want := TouchDeadline(early, now, lim), now.Add(lim.Idle).Unix(); got != want {
		t.Errorf("普通续期应为 now+Idle：得到 %d，期望 %d", got, want)
	}
}

// 出厂值必须与设置项的默认值一致（12 小时）：否则"没配过"的部署一升级，
// 会话时长会悄悄变掉，而界面上显示的默认值又与实际不符。
func TestDefaultSessionLimitsMatchDocumentedDefaults(t *testing.T) {
	lim := DefaultSessionLimits()
	if lim.Idle != SessionIdleTTL {
		t.Errorf("默认空闲上限 %v，期望 %v", lim.Idle, SessionIdleTTL)
	}
	if lim.Absolute != 12*time.Hour {
		t.Errorf("默认登录有效期 %v，期望 12 小时（设置项 admin.session_ttl_hours 的默认值）", lim.Absolute)
	}
}
