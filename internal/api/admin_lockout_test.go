package api

import (
	"net/http"
	"testing"
	"time"
)

// 9.3：暴力破解"5 次锁定 15 分钟"的**行为链**。
//
// 为什么不能只测 store.RecordLoginFailure：这条契约有两半，分别落在两个包里——
//  1. 计数到阈值就写 locked_until（store.RecordLoginFailure）；
//  2. 锁定期内**拒绝**，而且拒绝必须发生在**校验口令之前**（api.handleLogin）。
//
// 只测第 1 半的话，把第 58 行那个 `if admin.LockedUntil > now.Unix()` 挪到口令
// 校验之后，或干脆删掉，测试依然全绿——但"知道正确口令的人可以在锁定期内长驱直入"
// 已经是一个真实的绕过。所以这一层必须打真实 HTTP 请求。
//
// 时钟由测试掌握（nowFunc 接缝），于是 T、T+899、T+900 三个点可以精确钉死；
// 否则"到期自动放行"只能靠真的 sleep 15 分钟，没有人会跑。
func TestLoginLockoutAfterFiveFailuresAndUnlockAtExpiry(t *testing.T) {
	h, repo := newTOTPTestHandler(t)

	base := time.Unix(1_760_000_000, 0)
	clock := base
	prev := nowFunc
	nowFunc = func() time.Time { return clock }
	t.Cleanup(func() { nowFunc = prev })

	const adminID = int64(1) // newTOTPTestHandler 建的第一个管理员

	// attempt 发一次登录，返回状态码与错误码（成功时错误码为空）。
	attempt := func(password string) (int, string) {
		w, out := postJSON(t, h.handleLogin,
			map[string]any{"username": "ops", "password": password})
		code := ""
		if e, ok := out["error"].(map[string]any); ok {
			code, _ = e["code"].(string)
		}
		return w.Code, code
	}

	// ---- 前 4 次：401，且账户仍未锁定 ----
	for i := 1; i <= 4; i++ {
		got, code := attempt("wrong-password")
		if got != http.StatusUnauthorized || code != "unauthorized" {
			t.Fatalf("第 %d 次错口令应 401/unauthorized，得到 %d/%q", i, got, code)
		}
		a, _ := repo.GetAdminByID(adminID)
		if a.LockedUntil != 0 {
			t.Fatalf("第 %d 次就锁定了，locked_until=%d（阈值是 5 次）", i, a.LockedUntil)
		}
	}

	// ---- 第 5 次：仍是 401，但这次必须落下锁定 ----
	got, code := attempt("wrong-password")
	if got != http.StatusUnauthorized || code != "unauthorized" {
		t.Fatalf("第 5 次错口令应 401/unauthorized，得到 %d/%q", got, code)
	}
	a, _ := repo.GetAdminByID(adminID)
	if want := base.Unix() + 15*60; a.LockedUntil != want {
		t.Fatalf("第 5 次后应锁定到 %d，实际 %d", want, a.LockedUntil)
	}
	if a.FailedCount != 0 {
		t.Errorf("锁定后 failed_count 应清零，实际 %d", a.FailedCount)
	}

	// ---- 锁定期内：**口令正确也进不来**，且 429/locked ----
	//
	// 这一条是整段测试的重点：它同时挡住"锁检查被挪到口令校验之后"和
	// "锁检查被删掉"两种改坏方式——只要锁没生效，正确口令就会拿到 200。
	got, code = attempt(testPw)
	if got != http.StatusTooManyRequests || code != "locked" {
		t.Fatalf("锁定期内即使口令正确也应 429/locked，得到 %d/%q", got, code)
	}
	// 被拒的这一路必须在计数之前返回；否则每次探测都在悄悄延长/重置计数。
	if a, _ := repo.GetAdminByID(adminID); a.FailedCount != 0 {
		t.Errorf("锁定期内的拒绝不应改动 failed_count，实际 %d", a.FailedCount)
	}

	// ---- 到期前一秒：仍然锁着 ----
	clock = base.Add(899 * time.Second)
	if got, code = attempt(testPw); got != http.StatusTooManyRequests || code != "locked" {
		t.Fatalf("T+899 仍应在锁定期内（429/locked），得到 %d/%q", got, code)
	}

	// ---- 到期那一刻：放行 ----
	//
	// 判据是 locked_until > now，所以 T+900 属于"已解锁"。写死这一秒，
	// 是为了让"把 > 改成 >= 就会多锁一秒"这种差一错误在这里变红。
	clock = base.Add(900 * time.Second)
	if got, code = attempt(testPw); got != http.StatusOK {
		t.Fatalf("T+900 锁定到期，正确口令应登录成功（200），得到 %d/%q", got, code)
	}
	a, _ = repo.GetAdminByID(adminID)
	if a.LockedUntil != 0 || a.FailedCount != 0 {
		t.Errorf("登录成功后应清零计数与锁定，实际 locked=%d failed=%d",
			a.LockedUntil, a.FailedCount)
	}
}
