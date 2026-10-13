package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/koyoca/xb-probe/internal/state"
)

// 这一组护栏针对的是"30 分钟"这一档独有的数据通路：它不读时序表，
// 而是从内存环形缓冲（state.RingPoint）取点。因此新增/改名一个指标时，
// 时序表那一侧（metricExpr）会自动覆盖到，环形层却会**静默落进 default 返回 0**。

// ringSentinel 的每个字段都是**互不相同且非零**的值。
//
// 这个形状是判据成立的前提：pickRing 的失败模式是"落进 default 返回 0"，
// 所以只要返回值是 0 就一定说明该指标没有被处理。而"字段之间不相等"还能顺带
// 抓住"取错了字段"——如果都用 1，取错字段的实现在这里同样是绿的。
func ringSentinel() state.RingPoint {
	return state.RingPoint{
		TS:       1_760_000_000,
		CPU:      1101,
		MemPct:   2202,
		MemUsed:  3305303,
		DiskUsed: 4404404,
		RxBps:    5505505,
		TxBps:    6606606,
	}
}

// TestPickRingReturnsNonZeroForEveryMetricTheFrontendsRequest 从两个前端的源码里
// 抠出实际的 `metrics=` 清单，逐个走一遍 pickRing。
//
// 为什么要去读前端源码而不是在测试里抄一份清单：抄一份的话，前端加了指标、
// 测试里那份没跟着加 —— 缺陷照样漏出去。读源码则"前端问什么，就必须答得出什么"。
func TestPickRingReturnsNonZeroForEveryMetricTheFrontendsRequest(t *testing.T) {
	// 两个前端在"资源监控"面板里请求的指标（各自一份，改一处=没改）。
	// 锚点带上 `metrics=`，避免误匹配到 JS 里其它同名字符串。
	re := regexp.MustCompile(`metrics=([a-z_,]+)`)

	files := map[string]string{
		"公开看板": filepath.Join("..", "theme", "builtin", "app.js"),
		"管理后台": filepath.Join("adminui", "admin.js"),
	}
	p := ringSentinel()

	for name, rel := range files {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(rel)
			if err != nil {
				t.Fatalf("读取 %s 失败：%v", rel, err)
			}
			m := re.FindAllStringSubmatch(string(raw), -1)
			if len(m) == 0 {
				t.Fatalf("%s 里找不到 `metrics=` 请求参数：判据与前端源码脱节了（不是没缺陷，是没验到）", rel)
			}
			seen := map[string]bool{}
			for _, g := range m {
				for _, metric := range strings.Split(g[1], ",") {
					metric = strings.TrimSpace(metric)
					if metric == "" || seen[metric] {
						continue
					}
					seen[metric] = true
					if got := pickRing(p, metric); got == 0 {
						t.Errorf("pickRing(%q) 返回 0：环形层没有这个指标的分支，落进了 default。\n"+
							"  「最近 30 分钟」不读时序表，所以这一档会画成一条恒为 0 的曲线，\n"+
							"  而其它时间范围读时序表、一切正常 —— 典型的只坏一个范围。", metric)
					}
				}
			}
		})
	}
}

// TestPickRingMapsEachMetricToItsOwnField 钉住"取的是哪个字段"，而不只是"非零"。
//
// 30 分钟那一档的另一个历史缺陷正是取错字段：`mem` 返回了百分比字段，
// 而接口对 `mem` 声明的单位是 bytes，坐标轴于是把 "4500" 当成 4500 字节。
func TestPickRingMapsEachMetricToItsOwnField(t *testing.T) {
	p := ringSentinel()
	cases := map[string]int64{
		"cpu":       int64(p.CPU),
		"mem":       p.MemUsed,
		"mem_used":  p.MemUsed,
		"disk_used": p.DiskUsed,
		"net_rx":    p.RxBps,
		"net_tx":    p.TxBps,
		"mem_pct":   int64(p.MemPct),
	}
	for metric, want := range cases {
		if got := pickRing(p, metric); got != want {
			t.Errorf("pickRing(%q) = %d，期望 %d", metric, got, want)
		}
	}
}

// TestPickRingMetricsAgreeWithUnitOf 钉住"环形层支持的指标"与"单位声明的指标"是同一套。
//
// 这两个函数分开写就必然会漂移：unitOf 认 bytes 的指标、pickRing 不认，
// 结果就是上面那条"恒为 0 的曲线"。这里用 unitOf 的返回值当清单来源
// （除了只走时序表、由环形层用 mem_pct 表达的 `mem` 别名）。
func TestPickRingMetricsAgreeWithUnitOf(t *testing.T) {
	p := ringSentinel()
	for _, metric := range []string{"cpu", "mem", "mem_used", "disk_used", "net_rx", "net_tx"} {
		if unitOf(metric) == "raw" {
			t.Errorf("unitOf(%q) = raw：这个指标单位没声明，前端无法格式化", metric)
		}
		if got := pickRing(p, metric); got == 0 {
			t.Errorf("unitOf 认 %q，pickRing 却不认（返回 0）：两边漂移了", metric)
		}
	}
}
