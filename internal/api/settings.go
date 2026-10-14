package api

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/koyoca/xb-probe/internal/auth"
	"github.com/koyoca/xb-probe/internal/config"
	"github.com/koyoca/xb-probe/internal/model"
)

// 设置项白名单与取值校验（6.2 / S8）。
//
// 关键安全点：install.master_url 会被拼入安装命令，必须严格校验为 https URL
// 且只含字母、数字与 . - : / _（S8），否则存在 shell 注入风险。

// settingDefaults 是各设置项的“出厂默认值”。
//
// 为什么需要它：settings 表只存管理员显式改过的键，新库读出来是 {}。
// 后台设置页如果只回显库里那几行，管理员既看不到当前生效值，也无从判断
// 某个开关到底是不是默认。因此 GET /settings 返回 defaults 与库值的合并视图。
//
// 这里的取值必须与代码里 GetIntSetting/GetSettingDefault 的兜底值保持一致，
// 否则会出现“页面显示 10、实际按 5 跑”的错位。settingDefaults 与 settingSpecs
// 的键集合由测试强制校验相等。
var settingDefaults = map[string]string{
	"retention.raw_hours": "24",
	"retention.mid_days":  "7",
	"retention.days":      "30",
	"raw_resolution_s":    "10",
	"report_interval_s":   "3",
	"probe_interval_s":    "30",
	"offline_timeout_s":   "30",
	"disk_min_free_mb":    "200",
	"timezone":            "", // 空 = 用 Master 所在系统时区
	"theme.active":        "builtin",
	"site.title":          "XB-Probe",
	// site.layout 决定"公开看板与管理后台是在两个域名下、还是在同一个域名下"。
	//
	// 取值三态，默认 auto = **与升级前完全一致**（配了 host 白名单就分离，
	// 没配就是同站）。之所以不直接把默认写成 single/split：那两种都是"明确
	// 表态"，而已经配了 public_hosts/admin_hosts 的存量部署并没有表过态——
	// 用一个新默认值去重新解释它们的既有配置，是一次静默的行为变更。
	//
	//   auto    跟随 master.yaml：配了 host 白名单就分离，没配就同站
	//   single  明确同站：一个域名下同时提供公开看板与 /admin/
	//   split   明确分离：公开域名只服务公开路径、管理域名只服务管理路径
	"site.layout": "auto",
	// site.public_hosts / site.admin_hosts 是**运行时可改**的 Host 白名单，
	// 用来顶替 master.yaml 里的 public_hosts / admin_hosts。
	//
	//   - 非空 = 以设置项为准（配置文件里的那份被顶替，启动日志会打印最终生效值）
	//   - 空   = 回落到 master.yaml（"没在后台配过"与"配成空"在这里是同一件事，
	//            因为空清单从来不是"谁都别访问"的意思，那是 split 的校验负责的）
	//
	// 出厂留空 = 完全沿用配置文件，升级后行为不变。
	"site.public_hosts": "",
	"site.admin_hosts":  "",
	// site.admin_url 是公开看板顶栏「管理后台」按钮的跳转目标（绝对地址）。
	//
	// 空白默认值是**刻意的**：绝大多数部署是同站的，相对路径 /admin/ 既准确
	// 又不受域名写错影响。只有在公开站与后台不同源（不同端口 / 不同域名）时
	// 才需要它——此时写死的 /admin/ 会跳到公开站自己的路径下，得到 404 或 421。
	//
	// 留空时的推导顺序：master.yaml 的 admin_origin → 「分离」且只有一个
	// 管理域名时按请求协议拼出来 → 都没有就保持同站相对路径。
	"site.admin_url": "",
	// admin.session_ttl_hours 是管理端的「登录有效期」（小时）：从登录那一刻算起，
	// 到点必须重新登录（Cookie 的 MaxAge 与服务端判定用同一个值）。
	//
	// 默认 12 小时与线上既有行为一致（auth.SessionAbsoluteTTL）：老部署升级后
	// 不会因为多了这个设置项而改变会话时长。取值下限 1 小时、上限 720 小时（30 天）
	// —— 不提供"永不过期"：那等于取消这条保护。
	//
	// 它与空闲上限（auth.SessionIdleTTL，固定 2 小时）是两条独立的线：页面停在
	// 那里不动由空闲上限兜底，一直在用则由本项兜底。
	"admin.session_ttl_hours": "12",
	// public.cards 是公开看板的**卡片级**白名单。
	// 取值 "all"（默认）= 不限制；空值 = 一张都不展示；否则是逗号分隔的卡片键。
	// 与 public.fields 的区别：fields 管"某个字段的数据发不发给前端"，
	// cards 管"某张卡片显示不显示"。两者互补——把 kpi_fee 从 cards 里去掉
	// 只是不显示那张卡，而把 monthly_fee 从 fields 里去掉则连数据都不再下发。
	// 可选键清单见 model.PublicCardNames()。
	"public.cards": model.DefaultPublicCards(),
	// public.enabled 是公开站的全局总开关（默认开放）。
	// 关闭后公开页面与 /api/public/v1/* 一律不可匿名访问；一键安装脚本与
	// Agent 发布文件（/install.sh、/install.ps1、/dist/）不受影响，否则关掉
	// 看板会把装机通道也一起关死，新节点再也装不上（见 router.go 的 publicGate）。
	"public.enabled": "1",
	// public.fields 空值 = 不过滤（公开白名单全集）。
	// 具体可选字段清单见 model.PublicFieldNames()。
	"public.fields": "",
	// public.lat_window 是节点卡片点阵的时间窗。30m/1h 读 latency_raw
	// （10 分钟层在这么短的窗口里只有 3/6 个桶，画不出点阵），6h/24h 读
	// latency_10m。取值必须与 state.LatencyBarLayout 的分支一致。
	"public.lat_window": "24h",
	// public.show_ip_badge 控制公开卡片是否渲染 IPv4/IPv6 协议栈标识（默认显示）。
	// 关闭后公开 DTO 的 net_stack 会被清空，既不在前端画标识、也不下发协议栈信息。
	"public.show_ip_badge": "1",
	// Agent 接口 IP 白名单（见 router.go agentIPGate）。enabled 默认关闭，
	// 行为与升级前一致；whitelist 为逗号分隔的单 IP / CIDR，写入前只做
	// "长度 + 字符集"的宽松校验——真正解析失败的白名单条目会被忽略
	// （parseCIDRs 逐条过滤），不会导致整个接口被锁死。
	"agent.ip_whitelist_enabled": "0",
	"agent.ip_whitelist":         "",
	"inject.head":                "",
	"inject.body_end":            "",
	"inject.script_enabled":      "0",
	"heartbeat_url":              "",
	"install.mode":               "on_demand",
	"install.master_url":         "",
	"install.dist_base":          "",
	"install.agent_version":      "",
	"install.code_ttl_min":       "30",
	"install.spki_sha256":        "",
	"install.pubkey":             "",
	// 每日自动备份与轮转（6.6）。
	//   backup.enabled 默认开启：备份是"存在感很低但缺了就致命"的功能，
	//     默认关闭的话没人会去打开它，等出事时才发现从来没备过。
	//   backup.hour 是**Master 本地时区**的小时数（0~23）。用本地时区是因为
	//     管理员脑子里的"凌晨 4 点"就是自己手表上的 4 点；库内时间仍是 UTC。
	//   backup.keep 是保留份数，下限 1（不允许配成 0，见 PruneBackups 的说明）。
	"backup.enabled": "1",
	"backup.hour":    "4",
	"backup.keep":    "7",
}

var settingSpecs = map[string]func(string) error{
	"retention.raw_hours":        intRange(1, 72),
	"retention.mid_days":         intRange(1, 30),
	"retention.days":             intRange(1, 365),
	"raw_resolution_s":           intRange(3, 60),
	"report_interval_s":          intRange(2, 60),
	"probe_interval_s":           intRange(10, 600),
	"offline_timeout_s":          intRange(10, 3600),
	"disk_min_free_mb":           intRange(50, 100000),
	"timezone":                   validateTimezone,
	"theme.active":               anyValue,
	"site.title":                 maxLen(64),
	"site.layout":                enumValue(siteLayoutValues...),
	"site.public_hosts":          validateHostList,
	"site.admin_hosts":           validateHostList,
	"site.admin_url":             optionalHTTPURL,
	"admin.session_ttl_hours":    intRange(1, 720),
	"public.enabled":             enumValue("0", "1"),
	"public.fields":              validatePublicFields,
	"public.cards":               validatePublicCards,
	"public.lat_window":          enumValue("30m", "1h", "6h", "24h"),
	"public.show_ip_badge":       enumValue("0", "1"),
	"agent.ip_whitelist_enabled": enumValue("0", "1"),
	"agent.ip_whitelist":         validateIPWhitelist,
	"inject.head":                maxLen(8192),
	"inject.body_end":            maxLen(8192),
	"inject.script_enabled":      enumValue("0", "1"),
	"heartbeat_url":              optionalHTTPSURL,
	"install.mode":               enumValue("on_demand", "always", "off"),
	"install.master_url":         optionalStrictURL,
	"install.dist_base":          optionalStrictURL,
	"install.agent_version":      maxLen(32),
	"install.code_ttl_min":       intRange(5, 1440),
	"install.spki_sha256":        base64ish,
	"install.pubkey":             validateMinisignPubKey,
	"backup.enabled":             enumValue("0", "1"),
	"backup.hour":                intRange(0, 23),
	// 下限 1：配成 0 会让 PruneBackups 保留 0 份（尽管它自身有兜底，
	// 也不该让管理员有机会配出一个"永远不保留备份"的状态）。
	"backup.keep": intRange(1, 90),
}

// allowedSettingKey 判断设置键是否在白名单内。
func allowedSettingKey(key string) bool {
	_, ok := settingSpecs[key]
	return ok
}

// validateSettingValue 校验设置值。
func validateSettingValue(key, value string) error {
	fn, ok := settingSpecs[key]
	if !ok {
		return fmt.Errorf("不允许的设置项：%s", key)
	}
	if err := fn(value); err != nil {
		return fmt.Errorf("设置项 %s 取值非法：%v", key, err)
	}
	return nil
}

func intRange(min, max int) func(string) error {
	return func(v string) error {
		if v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("必须是整数")
		}
		if n < min || n > max {
			return fmt.Errorf("必须在 %d~%d", min, max)
		}
		return nil
	}
}

func maxLen(n int) func(string) error {
	return func(v string) error {
		if len(v) > n {
			return fmt.Errorf("长度不得超过 %d", n)
		}
		return nil
	}
}

func enumValue(vals ...string) func(string) error {
	return func(v string) error {
		if v == "" {
			return nil
		}
		for _, a := range vals {
			if v == a {
				return nil
			}
		}
		return fmt.Errorf("必须是 %s 之一", strings.Join(vals, "/"))
	}
}

func anyValue(string) error { return nil }

func validateTimezone(v string) error {
	if v == "" {
		return nil
	}
	if _, err := time.LoadLocation(v); err != nil {
		return fmt.Errorf("时区名称非法")
	}
	return nil
}

// optionalHTTPSURL 允许空值；非空时必须是 https。
func optionalHTTPSURL(v string) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("必须是 https 地址")
	}
	return nil
}

// optionalStrictURL 在 https 基础上再限制字符集（S8）。
func optionalStrictURL(v string) error {
	if v == "" {
		return nil
	}
	if !allowedURL(v) {
		return fmt.Errorf("必须是以 https:// 开头、且仅含字母数字与 .-:/_ 的地址")
	}
	return nil
}

// optionalHTTPURL 允许空值；非空时必须是 http/https 地址。
//
// 与 install.master_url 的差别：那个会被拼进 shell 命令，所以强制 https；
// 这个只是公开看板上「管理后台」按钮的跳转目标，明文 HTTP 直连 IP 的部署
// 同样需要它。字符集照样收紧——它会进 <a href>，放开前缀等于留一个
// 可被后台改写的脚本入口。
func optionalHTTPURL(v string) error {
	if v == "" {
		return nil
	}
	if !allowedHTTPURL(v) {
		return fmt.Errorf("必须以 http:// 或 https:// 开头，且仅含字母数字与 .-:/_")
	}
	return nil
}

// validatePublicFields 校验 public.fields。
//
// 取值是逗号分隔的公开字段名（见 model.PublicFieldNames）。空值表示不过滤，
// 即公开白名单全集。未知字段名直接报错而不是忽略：静默忽略会让管理员以为
// 某个字段已经关掉了，实际仍在输出——这正是最不该出错的方向。
// validateIPWhitelist 校验 Agent 白名单列表：逗号分隔，每项必须是合法的
// 单 IP 或 CIDR。这里逐项严格解析（与 parseCIDRs 同一套语义），把格式错误
// 拦在保存时而不是上线后才发现"白名单压根没生效"。
func validateIPWhitelist(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > 4096 {
		return fmt.Errorf("列表过长")
	}
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Contains(item, "/") {
			if _, _, err := net.ParseCIDR(item); err != nil {
				return fmt.Errorf("非法网段：%s", item)
			}
			continue
		}
		if net.ParseIP(item) == nil {
			return fmt.Errorf("非法 IP：%s", item)
		}
	}
	return nil
}

// validateHostList 校验 site.public_hosts / site.admin_hosts。
//
// 取值是一份 "主机名[:端口]" 清单，用来告诉 Master "哪个域名属于公开看板、
// 哪个域名属于管理后台"。两条硬规矩：
//
//  1. 分隔符按**最宽**的形式认（逗号、分号、空格、换行、制表、回车，见
//     splitHostList）。这类清单几乎都是从表格或别处的配置里复制粘贴来的，
//     只认逗号会让"粘进来带一个换行"整条被拒，而管理员的直觉是"这就是一份清单"。
//  2. 条目本身走 config.CheckHostWhitelistEntry —— 与 master.yaml 的
//     public_hosts / admin_hosts **同一份实现**。两处各写一套迟早漂移，
//     而漂移的表现是"配置文件里能写的值，后台保存不了"，这种不一致最难解释。
func validateHostList(v string) error {
	if len(v) > 512 {
		return fmt.Errorf("清单过长（上限 512 字符）")
	}
	for _, item := range splitHostList(v) {
		if err := config.CheckHostWhitelistEntry(item); err != nil {
			return fmt.Errorf("非法域名条目 %s：%v", item, err)
		}
	}
	return nil
}

func validatePublicFields(v string) error {
	if v == "" {
		return nil
	}
	known := make(map[string]bool)
	for _, f := range model.PublicFieldNames() {
		known[f] = true
	}
	for _, part := range strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == ';'
	}) {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		if !known[name] {
			return fmt.Errorf("未知的公开字段 %q", name)
		}
	}
	return nil
}

// validatePublicCards 校验 public.cards（卡片级公开展示开关）。
//
// 三种合法取值：
//   - "all"：不限制，全部展示（出厂默认）
//   - 空串：一张都不展示（管理员取消了全部勾选）
//   - 逗号分隔的卡片键，每个都必须在 model.PublicCardNames() 里
//
// 未知的卡片键直接报错而不是静默忽略：静默忽略会让管理员以为某张卡已经
// 关掉了，实际还在展示——恰好是最不该出错的方向（关不掉比关太多严重）。
func validatePublicCards(v string) error {
	if v == "" || strings.EqualFold(strings.TrimSpace(v), model.PublicCardsAll) {
		return nil
	}
	known := make(map[string]bool)
	for _, k := range model.PublicCardNames() {
		known[k] = true
	}
	for _, part := range strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == ';'
	}) {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		if !known[name] {
			return fmt.Errorf("未知的公开卡片 %q", name)
		}
	}
	return nil
}

func base64ish(v string) error {
	if v == "" {
		return nil
	}
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '+' || r == '/' || r == '=' || r == ',' || r == ' ' {
			continue
		}
		return fmt.Errorf("仅允许 base64 字符与逗号分隔")
	}
	return nil
}

// validateMinisignPubKey 校验 install.pubkey（6.8）。
//
// 这里刻意**不**用 maxLen(256) 这类长度限制：minisign 公钥是两行文本
// （注释行 + 一行 base64），粘贴时还常带空白或换行差异，
// 按长度卡会把合法公钥挡在门外，而对安全毫无帮助。
//
// 真正有意义的是"能不能解析成一把 Ed25519 公钥"——
// 配错公钥的后果是所有节点的安装脚本都会验签失败（装不上 Agent），
// 且报错发生在节点侧、根因在 Master 的设置页，排障路径很长。
// 所以在这里就解析一次，把错误挡在保存那一刻。
//
// 空值放行：表示该部署未启用签名校验。
func validateMinisignPubKey(v string) error {
	if v == "" {
		return nil
	}
	if _, err := auth.ParseMinisignPublicKey(v); err != nil {
		return fmt.Errorf("不是有效的 minisign 公钥（应形如两行：`untrusted comment: ...` + 一行 base64）：%v", err)
	}
	return nil
}
