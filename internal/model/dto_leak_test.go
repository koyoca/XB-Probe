package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 本文件是公开 API 的“防泄漏”回归测试（文档 5.1 / 7.3 / S1 / 9.4）。
//
// 设计前提：公开响应的安全不是靠“查出来再过滤”，而是靠类型里根本没有那些字段。
// 因此一旦有人往 PublicXxx 结构体里加了 ip/hostname/token/price 之类的字段，
// 这里必须立刻失败——这是防止最严重的一类回归（把内网信息暴露到公网）的最后一道闸。

// pubPtr 只用于测试里构造可选字段的指针。
func pubPtr(v int64) *int64 { return &v }

// forbiddenPublicKeys 是公开 DTO 的 JSON 键中绝不允许出现的子串。
// 注意：不能把 "os" 放进来，因为 "loss" 也含 "os"；"os" 本身是允许公开的字段。
var forbiddenPublicKeys = []string{
	"ip", "host", "token", "price", "secret", "passw", "cost",
	"kernel", "cpu_model", "credential", "provider", "location", "note",
	"uagent", "user_agent", "mac", "ssh", "port",
}

// publicDTOs 是所有对外输出类型的清单。新增公开 DTO 时同步加到这里。
func publicDTOs() map[string]any {
	return map[string]any{
		"PublicSite":         PublicSite{},
		"PublicOverview":     PublicOverview{},
		"PublicNode":         PublicNode{},
		"PublicLatency":      PublicLatency{},
		"PublicHistoryPoint": PublicHistoryPoint{},
		"PublicSeries":       PublicSeries{},
	}
}

// collectJSONKeys 递归收集一个类型序列化后会出现的所有 JSON 键名。
func collectJSONKeys(t reflect.Type, visited map[reflect.Type]bool, out *[]string, goNames *[]string) {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || visited[t] {
		return
	}
	visited[t] = true
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // 非导出字段根本不会被序列化
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		*out = append(*out, name)
		*goNames = append(*goNames, f.Name)
		collectJSONKeys(f.Type, visited, out, goNames)
	}
}

// 公开 DTO 的字段名与 JSON 键都不得命中禁用词。
func TestPublicDTOKeysAreWhitelisted(t *testing.T) {
	for typeName, sample := range publicDTOs() {
		var keys, goNames []string
		collectJSONKeys(reflect.TypeOf(sample), map[reflect.Type]bool{}, &keys, &goNames)

		for _, k := range keys {
			low := strings.ToLower(k)
			for _, bad := range forbiddenPublicKeys {
				if strings.Contains(low, bad) {
					t.Errorf("%s 的公开字段 %q 命中禁用词 %q：公开 DTO 里不允许存在该字段", typeName, k, bad)
				}
			}
		}
		for _, n := range goNames {
			low := strings.ToLower(n)
			for _, bad := range forbiddenPublicKeys {
				if strings.Contains(low, bad) {
					t.Errorf("%s 的 Go 字段名 %q 命中禁用词 %q", typeName, n, bad)
				}
			}
		}
		if len(keys) == 0 {
			t.Errorf("%s 未解析出任何字段，测试本身可能失效了", typeName)
		}
	}
}

// 即使上游把敏感值塞进了 name/group 这类公开字段，序列化结果里也不得出现
// IP、主机名、内核、令牌、价格这些“值”。这里同时验证“字段白名单”确实生效。
func TestPublicNodeJSONCarriesNoSensitiveValues(t *testing.T) {
	// 一个“装满敏感信息”的源节点——它代表数据库里的真实行。
	src := Node{
		ID:            4242,
		PublicID:      "a1b2c3d4e5f6",
		Name:          "香港-01",
		GroupName:     "hk",
		Status:        StatusOnline,
		PublicVisible: true,
		IPv4:          "203.0.113.77",
		IPv6:          "2001:db8::dead:beef",
		Provider:      "SECRETPROVIDER",
		Location:      "SECRETDC",
		Note:          "internal-note-SECRET",
		Hostname:      "secret-host-01",
		OS:            "linux",
		Kernel:        "5.15.0-secretkernel",
		Arch:          "amd64",
		CPUModel:      "EPYC-SECRET",
		CPUCores:      8,
		MemTotal:      16 << 30,
		DiskTotal:     100 << 30,
		BootTime:      1_700_000_000,
		AgentVersion:  "1.2.3",
	}

	pub := PublicNode{
		PublicID:   src.PublicID,
		Name:       src.Name,
		Group:      src.GroupName,
		Status:     src.Status,
		OS:         src.OS,
		UptimeS:    3600,
		CPUPct:     1234,
		MemPct:     5600,
		SwapPct:    0,
		DiskPct:    3100,
		RxBps:      1024,
		TxBps:      2048,
		ExpireDays: nil,
		Latency: []PublicLatency{
			{Carrier: "telecom", RTTAvg: pubPtr(12000), Loss: 0, Status: "ok"},
			{Carrier: "unicom", RTTAvg: nil, Loss: 10000, Status: "bad"},
		},
		UpdatedAt: 1_760_000_000,
	}
	raw, err := json.Marshal(pub)
	if err != nil {
		t.Fatalf("序列化公开节点失败：%v", err)
	}
	got := string(raw)
	t.Logf("公开节点 JSON：%s", got)

	forbiddenValues := []string{
		src.IPv4, src.IPv6, "2001:db8", // IP
		src.Hostname, src.Kernel, src.CPUModel, // 主机名/内核/CPU 型号
		src.Provider, src.Location, src.Note, // 供应商/机房/备注
		"SECRET", "secret", // 任何 SECRET 残留
	}
	for _, v := range forbiddenValues {
		if v == "" {
			continue
		}
		if strings.Contains(got, v) {
			t.Errorf("公开 JSON 泄漏了敏感值 %q：%s", v, got)
		}
	}
	// 内部自增 ID 也不应出现（公开侧只用 public_id 定位）。
	if strings.Contains(got, `"id"`) || strings.Contains(got, "4242") {
		t.Errorf("公开 JSON 不应包含内部主键：%s", got)
	}

	// 字段白名单：输出的键必须逐个在允许集合内。
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	allowed := map[string]bool{
		"public_id": true, "name": true, "group": true, "status": true,
		"os": true, "arch": true, "virt": true,
		"country":   true, // ISO alpha-2，仅用于画国旗；不含机房/供应商信息
		"cpu_cores": true, "cpu_pct": true, "load1": true,
		"mem_used": true, "mem_total": true, "mem_pct": true, "swap_pct": true,
		"disk_used": true, "disk_total": true, "disk_pct": true,
		"rx_bps": true, "tx_bps": true, "rx_used": true, "tx_used": true,
		"traffic_used": true, "traffic_quota": true, "traffic_used_pct": true,
		"today_rx": true, "today_tx": true,
		// 单节点月均续费价格（折算口径与月费合计一致；fee_cycle 只是
		// monthly/quarterly/yearly/once/lifetime 枚举，不是金额）。
		"monthly_fee": true, "monthly_fee_currency": true, "fee_cycle": true,
		"uptime_s": true, "expire_days": true, "latency": true, "updated_at": true,
	}
	for k := range generic {
		if !allowed[k] {
			t.Errorf("公开节点出现了白名单外的字段 %q", k)
		}
	}
}

// 反向对照：同样的敏感值放进内部模型时**必须**存在，
// 以此证明上面的断言不是“因为值是空的所以没检出”。
func TestInternalModelsDeliberatelyHoldSensitiveData(t *testing.T) {
	n := Node{
		IPv4:     "203.0.113.77",
		Hostname: "secret-host-01",
		Kernel:   "5.15.0-secretkernel",
		Provider: "SECRETPROVIDER",
	}
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("序列化内部节点失败：%v", err)
	}
	got := string(raw)
	for _, want := range []string{"203.0.113.77", "secret-host-01", "5.15.0-secretkernel", `"ip_v4"`, `"hostname"`, `"kernel"`} {
		if !strings.Contains(got, want) {
			t.Errorf("内部 Node 应包含 %q（否则本测试失去对照意义）：%s", want, got)
		}
	}

	b := Billing{NodeID: 1, PriceMinor: 499900, Currency: "CNY", Cycle: "monthly", ResetDay: 1}
	braw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("序列化计费失败：%v", err)
	}
	if !strings.Contains(string(braw), "499900") || !strings.Contains(string(braw), `"price_minor"`) {
		t.Errorf("内部 Billing 应含价格字段：%s", braw)
	}

	// 令牌哈希必须是 json:"-"，即便被误序列化也不能出哈希；但 last_ip 会出。
	tok := Token{ID: 1, NodeID: 1, TokenHash: []byte{0xde, 0xad, 0xbe, 0xef}, TokenHint: "pk_abc1", LastIP: "198.51.100.9"}
	traw, err := json.Marshal(tok)
	if err != nil {
		t.Fatalf("序列化令牌失败：%v", err)
	}
	if strings.Contains(string(traw), "3q2+7w") || strings.Contains(string(traw), `"token_hash"`) {
		t.Errorf("令牌哈希绝不能出现在 JSON 中：%s", traw)
	}
	if !strings.Contains(string(traw), "198.51.100.9") {
		t.Errorf("内部 Token 的 last_ip 应有值（对照用）：%s", traw)
	}
}

// 公开 DTO 里不允许出现内部自增 ID 这类定位符——公网一律用 public_id。
func TestPublicDTOsHaveNoInternalID(t *testing.T) {
	var keys, _ = func() ([]string, []string) {
		var k, g []string
		collectJSONKeys(reflect.TypeOf(PublicNode{}), map[reflect.Type]bool{}, &k, &g)
		return k, g
	}()
	// "public_id" 合法；单独出现的 "id" 非法。
	for _, k := range keys {
		if k == "id" || k == "node_id" || k == "nodeID" {
			t.Errorf("公开 DTO 不应暴露内部主键字段 %q", k)
		}
	}
}
