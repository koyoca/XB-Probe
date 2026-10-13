package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yourorg/probe/internal/model"
)

// TestSplitListKeepsUnsetDistinctFromEmpty 是不变量 8 在**生产端**的护栏。
//
// 网卡清单是三态：未配置 → nil（语义是"按 Agent 内置默认过滤"）/ 配置了 → 白名单。
// 一旦 splitList 退回空切片，线协议上就变成 "nic_exclude": []，
// Agent 反序列化出**非 nil 空切片**，于是 agentconfig.ApplyServerConfig 的
// `!= nil` 守卫被绕过，内置的 lo/docker*/veth* 默认排除项被整体抹掉：
// 流量统计把容器网卡与回环一起算进去，数字凭空虚高，
// 而配置文件、日志、后台界面全都"看起来正常"。
func TestSplitListKeepsUnsetDistinctFromEmpty(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantNil bool
		want    []string
	}{
		{"未设置", "", true, nil},
		{"只有分隔符与空白", " , ,\n \t ", true, nil},
		{"单个", "lo", false, []string{"lo"}},
		{"逗号分隔", "lo,docker*", false, []string{"lo", "docker*"}},
		{"混合分隔符", " lo , veth* \n br-* ", false, []string{"lo", "veth*", "br-*"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitList(c.in)
			if c.wantNil {
				if got != nil {
					t.Fatalf("未配置的清单必须返回 nil（空切片会被当成「要求不过滤」），得到 %#v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("配置了清单却返回 nil：%q", c.in)
			}
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Fatalf("切分结果 = %v，期望 %v", got, c.want)
			}
		})
	}
}

// TestBuildEffectiveEmitsNullForUnsetNICLists 断言线上序列化形态。
//
// 只看 `eff.NICExclude == nil` 还不够：真正决定 Agent 行为的是 JSON 里那个 null。
// EffectiveConfig 的字段刻意**不加 omitempty**，就是为了让
// "未配置(null)" 与 "配置为空([])" 在报文里可区分。
func TestBuildEffectiveEmitsNullForUnsetNICLists(t *testing.T) {
	eff := BuildEffective(map[string]string{
		"report_interval_s": "5",
		"probe_interval_s":  "20",
	}, nil)

	if eff.ReportIntervalS != 5 || eff.ProbeIntervalS != 20 {
		t.Fatalf("间隔未从设置项读取：%d/%d", eff.ReportIntervalS, eff.ProbeIntervalS)
	}
	if eff.NICInclude != nil || eff.NICExclude != nil {
		t.Fatalf("未配置的网卡清单必须是 nil：include=%#v exclude=%#v", eff.NICInclude, eff.NICExclude)
	}

	raw, err := json.Marshal(eff)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	js := string(raw)
	for _, want := range []string{`"nic_include":null`, `"nic_exclude":null`} {
		if !strings.Contains(js, want) {
			t.Fatalf("报文里必须是 %s（空数组会让 Agent 清掉内置默认排除项）：%s", want, js)
		}
	}

	// 反向：配置了就必须是数组，不能因为"看起来是空"又被吞掉。
	eff2 := BuildEffective(map[string]string{"nic_exclude": "lo"}, nil)
	raw2, _ := json.Marshal(eff2)
	if !strings.Contains(string(raw2), `"nic_exclude":["lo"]`) {
		t.Fatalf("配置的网卡清单未出现在报文里：%s", string(raw2))
	}
}

// TestComputeConfigVersionTreatsNilAndEmptyNICListsAlike 钉住"改 splitList 不会
// 让已部署节点集体重新拉配置"这一前提。
//
// ComputeConfigVersion 里的 sortedCopy 会把 nil 归一成 []，所以
// splitList 的返回值从 []string{} 改成 nil 时摘要不变——代码注释里写了这句话，
// 这里把它变成可执行的断言，否则哪天 sortedCopy 被"简化"掉，
// 全量节点会在下一次发布时同时重新拉取配置（一次自制的惊群）。
func TestComputeConfigVersionTreatsNilAndEmptyNICListsAlike(t *testing.T) {
	base := EffectiveConfig{ReportIntervalS: 3, ProbeIntervalS: 30}

	withNil := base
	withEmpty := base
	withEmpty.NICExclude = []string{}
	withEmpty.NICInclude = []string{}

	a := ComputeConfigVersion(withNil)
	if a == "" {
		t.Fatal("配置摘要为空：ComputeConfigVersion 序列化失败")
	}
	if a != ComputeConfigVersion(withEmpty) {
		t.Fatalf("nil 与空切片算出了不同摘要（%s vs %s）：已部署节点会被无谓地重新推配置",
			a, ComputeConfigVersion(withEmpty))
	}

	// 顺序无关：网卡清单是集合，同内容不同顺序必须同摘要，否则
	// 后台每保存一次设置就会把所有节点推一遍。
	one := base
	one.NICExclude = []string{"b", "a"}
	two := base
	two.NICExclude = []string{"a", "b"}
	if ComputeConfigVersion(one) != ComputeConfigVersion(two) {
		t.Fatal("网卡清单顺序影响了配置摘要")
	}

	// targets 按 id 排序，同理。
	t1 := base
	t1.Targets = []model.Target{{ID: 2}, {ID: 1}}
	t2 := base
	t2.Targets = []model.Target{{ID: 1}, {ID: 2}}
	if ComputeConfigVersion(t1) != ComputeConfigVersion(t2) {
		t.Fatal("探测目标顺序影响了配置摘要")
	}
}

// TestAtoiDefault 覆盖间隔解析的兜底：非法值必须回落到默认，而不是 0。
//
// 0 会被 Agent 的 applyDefaults 再兜一次，但"服务端下发 0"本身就让
// 两端看到的口径不一致（后台显示 0 秒、Agent 实际 3 秒）。
func TestAtoiDefault(t *testing.T) {
	cases := []struct {
		in   string
		def  int
		want int
	}{
		{"", 3, 3},
		{"5", 3, 5},
		{"0", 3, 3},   // 0 视为未设置
		{"abc", 7, 7}, // 非数字
		{"12x", 7, 7}, // 中途出现非数字
		{"-9", 7, 7},  // 负号不是数字
		{"600", 3, 600},
	}
	for _, c := range cases {
		if got := atoiDefault(c.in, c.def); got != c.want {
			t.Errorf("atoiDefault(%q, %d) = %d，期望 %d", c.in, c.def, got, c.want)
		}
	}
}
