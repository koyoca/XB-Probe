package api

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 后台「矫正本周期已用流量」的**前端**护栏。
//
// 这一层最危险的失败模式不是"按钮没渲染出来"，而是**字段名与单位悄悄对不上**：
//
//	键名写错（period_rx → periodRx）→ 后端解码出 0/0 → 管理员一保存，
//	    节点的本周期用量被静默清零，界面还会提示"已矫正本周期用量"。
//	漏掉单位换算（存 100 而不是 100 GB）→ 用量变成 100 字节，
//	    进度条从 80% 跳到 0%，看起来像"矫正生效了"。
//
// 两种都不会报错、都不会被后端测试抓到（后端只保证"收到这两个键就写"）。
// 所以判据必须**跨文件**：以后端的请求体键名为主，反向要求前端发出来的键
// 完全一致；并确认前端在发之前真的过了单位换算。

// readSrc 读取仓库内的源文件。
func readSrc(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", rel, err)
	}
	return string(raw)
}

// regionOf 切出 [start, end) 之间的第一个区间。
//
// 切成区间再找锚点，而不是在整份文件里 Contains：同一个文件里常常有别的函数
// 也出现同样的字符串（比如公共的 request 辅助），整文件查找会让"另一处实现"
// 替被测实现作证（见 MEMORY.md 里"判据只能认那一处实现"）。
func regionOf(t *testing.T, src, start, end string) string {
	t.Helper()
	i := strings.Index(src, start)
	if i < 0 {
		t.Fatalf("锚点 %q 不存在：判据与源码脱节了", start)
	}
	rest := src[i:]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("锚点 %q 之后找不到 %q：判据与源码脱节了", start, end)
	}
	return rest[:j]
}

func TestTrafficOverrideBodyKeysMatchBackend(t *testing.T) {
	adminGo := readSrc(t, "admin.go")
	// 后端的入参结构体（唯一权威）。
	beBody := regionOf(t, adminGo, "func (h *Handler) handleSetTrafficUsage", "\n}\n")
	beRe := regexp.MustCompile(`json:"(period_[a-z]+)"`)
	be := beRe.FindAllStringSubmatch(beBody, -1)
	if len(be) != 2 {
		t.Fatalf("后端 handleSetTrafficUsage 里应恰好解析 2 个流量字段，找到 %d 个：%v", len(be), be)
	}
	want := make([]string, 0, len(be))
	for _, m := range be {
		want = append(want, m[1])
	}
	sort.Strings(want)

	js := readSrc(t, filepath.Join("adminui", "admin.js"))
	fnBody := regionOf(t, js, "function renderTrafficOverride", "\n  }\n")

	// 前端发出去的两个键。
	feRe := regexp.MustCompile(`(period_[a-z]+)\s*:`)
	fe := feRe.FindAllStringSubmatch(fnBody, -1)
	if len(fe) != 2 {
		t.Fatalf("前端应恰好发出 2 个流量字段，找到 %d 个：%v", len(fe), fe)
	}
	got := make([]string, 0, len(fe))
	for _, m := range fe {
		got = append(got, m[1])
	}
	sort.Strings(got)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("前后端流量字段名不一致：前端发 %v，后端解析 %v（键名对不上会把用量静默清零）", got, want)
	}

	// 真的走 PUT /nodes/{id}/traffic（路径写错会 404，但断言不能只靠这个：
	// 它同时也是"这个表单确实接在新接口上"的证据）。
	if !strings.Contains(fnBody, `'/traffic', { method: 'PUT'`) {
		t.Fatal("矫正表单没有 PUT 到 /nodes/{id}/traffic")
	}

	// 单位换算：两个方向都必须先乘上所选单位（MB/GB/TB）再发出。
	if n := strings.Count(fnBody, "quotaToBytes("); n < 2 {
		t.Fatalf("两个方向的输入都必须过 quotaToBytes 换算，只找到 %d 处（漏了单位会把 GB 当字节存）", n)
	}
}

// TestTrafficOverrideAuditActionIsLabelled 钉审计动作的中文说明。
//
// 后端记的是动作键（traffic.override），前端只做键→中文查表；少一条就是
// 审计日志里孤零零一行英文键，管理员看不懂那是什么操作。
func TestTrafficOverrideAuditActionIsLabelled(t *testing.T) {
	const action = "traffic.override"

	if !strings.Contains(readSrc(t, "admin.go"), `"`+action+`"`) {
		t.Fatalf("后端没有记录 %s 审计动作（接口改了名字？护栏该跟着改）", action)
	}
	if !strings.Contains(readSrc(t, filepath.Join("adminui", "admin.js")), `'`+action+`'`) {
		t.Fatalf("前端 AUDIT_ACTION 缺少 %s 的中文说明", action)
	}
}

// TestTrafficPercentKeepsSingleImplementation 钉"百分之比只有一份实现"。
//
// 流量百分比有两条产出路径：调度器每 60 秒重算一次，以及"保存计费/矫正用量后
// 立即重算"（否则管理员会以为没保存成功）。这两处曾经各写一遍
// `used * 10000 / quota` 加各自的钳位，于是同一个节点能在两次刷新之间显示
// 两个不同的百分比。现在统一走 state.Percent，这条护栏挡的是"又被内联回去"。
func TestTrafficPercentKeepsSingleImplementation(t *testing.T) {
	cases := []struct {
		name  string
		file  string
		start string
	}{
		{
			name:  "保存计费后立即重算",
			file:  "admin.go",
			start: "func (h *Handler) refreshBillingRuntime",
		},
		{
			name:  "调度器慢循环",
			file:  filepath.Join("..", "..", "cmd", "xb-probe-master", "scheduler.go"),
			start: "func (s *Scheduler) refreshRuntimeBilling",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := readSrc(t, c.file)
			body := regionOf(t, src, c.start, "\n}\n")
			if !strings.Contains(body, "state.Percent(") {
				t.Fatalf("%s 不再调用 state.Percent：百分比又被内联了一遍", c.start)
			}
			if strings.Contains(body, "* 10000 /") {
				t.Fatalf("%s 里出现了内联的万分比整数乘法：除了会与 state.Percent 漂移，"+
					"used 超过 ≈9.2e14 时乘积还会溢出成负百分比", c.start)
			}
		})
	}
}
