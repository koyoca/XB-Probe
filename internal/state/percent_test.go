package state

import "testing"

// state.Percent 的溢出护栏。
//
// 它同时服务三条路径：内存/磁盘占用、带宽、以及流量额度。前两者天然取不到
// 溢出量级（磁盘总容量再大也就 1e13 量级），**流量**不一样：它是计费周期内的
// 累计值、口径相加、倍率最高 1000，而且后台新增了「手工矫正本周期用量」输入
// （上限 1 PiB）——那条输入是把 used 送进这个函数的最短路径。
//
// 旧实现是整数除法的 used*10000/total。当 used 超过 ≈9.2e14 时乘积溢出 int64
// 变成负数，而负值会**绕过**"超过 10000 就收敛到 10000"的判断，最终把一个
// 负百分比画到进度条上（宽度为负 → 渲染成 0 宽或 100% 宽，两种都很难看懂）。
func TestPercentDoesNotOverflowOnHugeUsage(t *testing.T) {
	// 9.3e14 字节 ≈ 930 TB：整数版在这里溢出（9.3e18 > 2^63-1），
	// 结果是 -9146 而不是 9300。这正是"手工矫正"能填出来的量级。
	const used = int64(930_000_000_000_000)
	const quota = int64(1_000_000_000_000_000)
	if got := Percent(used, quota); got != 9300 {
		t.Fatalf("Percent(%d,%d) = %d，期望 9300（负数说明走了会溢出的整数乘法）", used, quota, got)
	}
	// 极端值也不许出现负数或超过 10000 的数。
	for _, q := range []int64{1, 1 << 20, quota} {
		if got := Percent(used, q); got < 0 || got > 10000 {
			t.Fatalf("Percent(%d,%d) = %d，越界", used, q, got)
		}
	}
}

func TestPercentBoundaries(t *testing.T) {
	cases := []struct {
		used, total int64
		want        int
		why         string
	}{
		{0, 100, 0, "没用过"},
		{50, 100, 5000, "一半"},
		{100, 100, 10000, "恰好用满"},
		{200, 100, 10000, "超额收敛到 10000（前端把 10000 当满额）"},
		{50, 0, 0, "total<=0 = 不限额，没有百分比可言"},
		{50, -1, 0, "负基数按不限额处理"},
		{-5, 100, 0, "负用量视为无数据"},
		{1, 3, 3333, "除不尽向下取整"},
	}
	for _, c := range cases {
		if got := Percent(c.used, c.total); got != c.want {
			t.Errorf("Percent(%d,%d) = %d，期望 %d（%s）", c.used, c.total, got, c.want, c.why)
		}
	}
}
