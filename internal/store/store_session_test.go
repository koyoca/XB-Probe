package store

import "testing"

// 9.3：登录失败按策略锁定——计数、阈值、锁定时刻与"解锁后重新计数"。
//
// 这是文档 02 里长期挂着的那条"第三类未补"：`RecordLoginFailure` 只在
// TestSetTOTPSecretTogglesAndResetsLock 里被当作**前置条件**顺带用到，从来没有
// 断言过"第 5 次触发锁定、锁 15 分钟"。把 now 当参数传进来（而不是读时钟）使得
// 这条契约可以精确定点验，不需要真的等 15 分钟。
func TestRecordLoginFailureLocksOnFifthAttemptFor15Minutes(t *testing.T) {
	r := newTestRepo(t)
	id := mustAdmin(t, r, "ops-lock")

	const (
		maxFailed   = 5
		lockSeconds = 15 * 60
		now         = int64(1_760_000_000)
	)

	// 前 4 次：只累加计数，**绝不**锁定。
	//
	// 这一半挡住"阈值写小了"（比如写成 4）：提前锁定会让正常手滑的用户
	// 在还没到策略次数时就被关在门外。
	for i := 1; i < maxFailed; i++ {
		if err := r.RecordLoginFailure(id, maxFailed, lockSeconds, now); err != nil {
			t.Fatalf("第 %d 次记录失败：%v", i, err)
		}
		a, err := r.GetAdminByID(id)
		if err != nil || a == nil {
			t.Fatalf("读取管理员失败：%v", err)
		}
		if a.LockedUntil != 0 {
			t.Fatalf("第 %d 次就锁定了（阈值应为 %d），locked_until=%d", i, maxFailed, a.LockedUntil)
		}
		if a.FailedCount != i {
			t.Fatalf("第 %d 次后 failed_count 应为 %d，实际 %d", i, i, a.FailedCount)
		}
	}

	// 第 5 次：锁定。locked_until 必须**精确**等于 now+lockSeconds
	// （早一秒就短了、晚一秒就长了，且"15 分钟"这个承诺只能这么钉）。
	if err := r.RecordLoginFailure(id, maxFailed, lockSeconds, now); err != nil {
		t.Fatalf("第 %d 次记录失败：%v", maxFailed, err)
	}
	a, err := r.GetAdminByID(id)
	if err != nil || a == nil {
		t.Fatalf("读取管理员失败：%v", err)
	}
	if got, want := a.LockedUntil, now+lockSeconds; got != want {
		t.Errorf("第 %d 次应锁定到 %d（now+%d），实际 %d", maxFailed, want, lockSeconds, got)
	}
	// 锁定同时把计数清零：解锁后重新获得 maxFailed 次机会。
	// 若不清零（或清成别的值），策略会退化成"一次锁死"或"提前复发"。
	if a.FailedCount != 0 {
		t.Errorf("锁定后 failed_count 应清零（解锁后重新计数），实际 %d", a.FailedCount)
	}

	// 走到锁定期之后：计数确实从零重新开始，再满 maxFailed 次才再次锁定。
	later := now + lockSeconds
	for i := 1; i <= maxFailed; i++ {
		if err := r.RecordLoginFailure(id, maxFailed, lockSeconds, later); err != nil {
			t.Fatalf("解锁后第 %d 次记录失败：%v", i, err)
		}
	}
	a, _ = r.GetAdminByID(id)
	if got, want := a.LockedUntil, later+lockSeconds; got != want {
		t.Errorf("解锁后第 %d 次应锁定到 %d，实际 %d（说明计数没有被真的清零）",
			maxFailed, want, got)
	}
}

// 阈值与锁定秒数必须**照传进去的值**生效，不能被写死。
//
// 调用点（api 包）传的是 5 / 15*60；如果实现里把这两个数硬编码成别的值，
// 上一条测试仍会绿（它传的恰好也是 5/900），这条用来兜住那种"实现只认某一组参数"。
func TestRecordLoginFailureHonorsPassedPolicy(t *testing.T) {
	r := newTestRepo(t)
	id := mustAdmin(t, r, "ops-policy")

	// 阈值 1：第一次失败就锁，且锁 60 秒（不是 900）。
	if err := r.RecordLoginFailure(id, 1, 60, 5000); err != nil {
		t.Fatalf("记录失败：%v", err)
	}
	a, _ := r.GetAdminByID(id)
	if got, want := a.LockedUntil, int64(5060); got != want {
		t.Errorf("阈值 1 / 锁 60s 时应锁定到 %d，实际 %d", want, got)
	}
	if a.FailedCount != 0 {
		t.Errorf("锁定后 failed_count 应为 0，实际 %d", a.FailedCount)
	}
}

// 登录成功（ResetLoginFailures）必须同时清掉计数与锁定。
//
// "只清计数不清锁定"会让刚被锁的人即使口令正确也要干等满 15 分钟——
// 而这条路径本来就是给"验证通过"用的。
func TestResetLoginFailuresClearsCountAndLock(t *testing.T) {
	r := newTestRepo(t)
	id := mustAdmin(t, r, "ops-reset")

	// 先造出"有计数"和"已锁定"两种状态。
	if err := r.RecordLoginFailure(id, 5, 900, 100); err != nil {
		t.Fatalf("记录失败：%v", err)
	}
	if err := r.RecordLoginFailure(id, 1, 900, 100); err != nil {
		t.Fatalf("记录失败：%v", err)
	}
	a, _ := r.GetAdminByID(id)
	if a.LockedUntil == 0 {
		t.Fatal("前置条件不成立：应已锁定")
	}

	if err := r.ResetLoginFailures(id); err != nil {
		t.Fatalf("清零失败：%v", err)
	}
	a, _ = r.GetAdminByID(id)
	if a.LockedUntil != 0 || a.FailedCount != 0 {
		t.Errorf("登录成功后应同时清零计数与锁定，实际 locked=%d failed=%d",
			a.LockedUntil, a.FailedCount)
	}
}
