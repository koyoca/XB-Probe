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
	// install.max_enroll_codes 是单节点有效注册码的数量上限。默认 3 与
	// 升级前写死的值一致；现在可调——节点注册失败排查时经常要连签几个码，
	// 上限太小会把人卡在"无法重新获取"的死胡同里。
	"install.max_enroll_codes": "3",
	"install.spki_sha256":      "",
	"install.pubkey":           "",
	// CDN 与来源 IP（middleware.go resolveClientIP）。
	//   cdn.enabled 开启后，cdn.ranges 里的网段会被当作"已知的 CDN 边缘跳"：
	//   解析 X-Forwarded-For 时把这些跳跳过去，取到的才是真实客户端地址。
	//   网段来源必须可靠（CDN 官方公布的回源/边缘网段）；来源不落在名单里时
	//   一律按对端地址处理，伪造头洗不进来。
	"cdn.enabled": "0",
	"cdn.ranges":  "",
	// node.custom_countries 是管理员自定义的国家/地区清单，每行一条：
	//   两位代码|中文名    例如  GB|英国
	// 两位代码用 ISO 3166-1 alpha-2（节点卡片与下拉框都以它为键）；名称仅用于
	// 展示。内置 FLAGS 表覆盖常见地区，清单在这里补齐它没有的。
	"node.custom_countries": "",
	// 每日自动备份与轮转（6.6）。
	//   backup.enabled 默认开启：备份是"存在感很低但缺了就致命"的功能，
	//     默认关闭的话没人会去打开它，等出事时才发现从来没备过。
	//   backup.hour 是**Master 本地时区**的小时数（0~23）。用本地时区是因为
	//     管理员脑子里的"凌晨 4 点"就是自己手表上的 4 点；库内时间仍是 UTC。
	//   backup.keep 是保留份数，下限 1（不允许配成 0，见 PruneBackups 的说明）。
	"backup.enabled": "1",
	"backup.hour":    "4",
	"backup.keep":    "7",
	// audit.retention_days **故意不在这里**：它只由「审计日志」页的专用面板
	// 管理，那条路径带密码（TOTP 开启再加动态码）二次确认并立即清理一次。
	// 放进设置页就会多出一条**不设防**的入口（普通 PUT /settings），
	// 而保留期决定了能追溯多久的历史操作，属于必须确认的高危项。
	// 该键仍可存在于 settings 表里，读侧用 GetIntSetting 自带默认值 30。
	// alert.retention_days 是告警事件的保留天数。此前**硬编码 90 天**
	// （见调度器清理段），管理员无法调整——而告警量大的部署里 90 天会占掉
	// 可观的空间，告警量小的又想留更久。默认 90 与升级前行为一致。
	"alert.retention_days": "90",
	// 多币种换算（月费合计）。
	//
	//   billing.base_currency 是**主币种**：月费合计一律以它计价（ISO 4217，如 CNY）。
	//     留空 = 沿用旧行为（取第一个有月支出的节点的币种、不做任何换算）。
	//   billing.rates 是"相对主币种的汇率表"，逗号分隔的 `币种:数值`，
	//     数值含义是 **1 单位该币种 = 多少主币种**。例：主币种 CNY 时
	//     `USD:7` 表示 1 USD = 7 CNY。
	//
	// 为什么存"相对主币种"而不是两两汇率：N 种货币的两两汇率需要 N² 个数字，
	// 而且改一个就得连带改一圈；相对主币种只要 N-1 个，且与"合计以主币种计价"
	// 的语义天然对齐。主币种自身的汇率恒为 1，不需要也不能配置。
	"billing.base_currency": "",
	"billing.rates":         "",
	// 访问日志与 IP 黑名单（对应 migration 0011 的 access_log 表）。
	//
	// access_log_level 三档：
	//   0 = 关闭（默认，这张表随访问量线性增长，个人部署未必想看）
	//   1 = 仅网站访问：跳过静态资源与 Agent 上报（默认档，信息量最有用）
	//   2 = 全部：静态资源、Agent 上报都记（排查"节点到底有没有上报"时用）
	// 每次请求现读，改完立即生效、不必重启。
	//
	// 键名从 v0.3.0 的 access_log_enabled（0/1）迁到 access_log_level，
	// 读侧保留了旧键兜底，所以升级不会让已开启的部署突然变回关闭。
	"security.access_log_level": "0",
	"security.access_log_days":  "30",
	// ip_blacklist：一行（或逗号）一条，支持单 IP、CIDR、`1.2.3.*`。
	// 命中即 403，**公开站与管理端都拦**；Agent 接口不受影响
	// （黑名单是"不许访问网站"，而 Agent 是节点上报通道，误封会让监控静默中断）。
	"security.ip_blacklist": "",
	// Agent 远程运维：自动升级 + 目标版本。
	//
	// auto_upgrade 打开后，Agent 每次拉配置时若发现 target_version 与自己
	// 不同就自行下载替换（复用 `xb-probe-agent upgrade` 同一条实现）。
	// target_version 留空 = 不下发版本（只在"想钉某个版本"时填，如 v0.3.0）。
	// 注意：**升级后要等 service manager 重启进程**才生效（systemd 的
	// Restart=always 会自动拉起），不会在进程内热替换。
	// online.segment_min：公开详情页「在线情况」时间线每格多少分钟。
	// 默认 10 与 10 分钟聚合层对齐；调小能看到更细的断点（受指标层精度限制：
	// 24h 窗口读 metrics_10m，格长小于 10 分钟时相邻格会取到同一个时间点）。
	"online.segment_min":   "10",
	"agent.auto_upgrade":   "0",
	"agent.target_version": "",
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
	"install.max_enroll_codes":   intRange(1, 20),
	"install.spki_sha256":        base64ish,
	"install.pubkey":             validateMinisignPubKey,
	"cdn.enabled":                enumValue("0", "1"),
	"cdn.ranges":                 validateCIDRList,
	"node.custom_countries":      validateCustomCountries,
	"backup.enabled":             enumValue("0", "1"),
	"backup.hour":                intRange(0, 23),
	// 下限 1：配成 0 会让 PruneBackups 保留 0 份（尽管它自身有兜底，
	// 也不该让管理员有机会配出一个"永远不保留备份"的状态）。
	"backup.keep": intRange(1, 90),
	// audit.retention_days 不在白名单：只允许走审计页那条带二次确认的路径
	// （见 settingDefaults 处的说明）。范围校验由 handleSetAuditRetention 负责。
	"alert.retention_days": intRange(1, 3650),
	// 多币种（见 settingDefaults 的说明）。
	"billing.base_currency":     validateCurrencyCode,
	"billing.rates":             validateCurrencyRates,
	"security.access_log_level": enumValue("0", "1", "2"),
	"security.access_log_days":  intRange(1, 365),
	// 黑名单不做强校验：允许存"暂时写错的条目"，解析侧会跳过非法行。
	// 若在这里直接拒存，管理员改一半保存失败、旧名单还留着，更难排查。
	"security.ip_blacklist": func(string) error { return nil },
	"online.segment_min":    intRange(5, 120),
	"agent.auto_upgrade":    enumValue("0", "1"),
	// 只接受 vX.Y.Z 或空：写错版本号会让每个 Agent 各下载一次再失败。
	"agent.target_version": validateVersionTag,
}

// validateVersionTag 校验版本号形如 v1.2.3（或为空）。
//
// 为什么要挡：这个值会被下发到每个 Agent 去拼下载地址，写错一个字符
// 就是"全部节点各下载一次再失败"，而失败点在节点侧、Master 日志里看不出来。
func validateVersionTag(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if !strings.HasPrefix(v, "v") || len(v) < 2 {
		return fmt.Errorf("版本号须形如 v0.3.0")
	}
	for _, r := range v[1:] {
		if (r < '0' || r > '9') && r != '.' {
			return fmt.Errorf("版本号只能含数字与点")
		}
	}
	return nil
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

// validateCIDRList 校验 cdn.ranges：换行或逗号分隔的 CDN 边缘网段清单，
// 每项必须是合法的单 IP 或 CIDR。这项配置直接影响"谁是真实客户端"的判定
// （写错了会把 CDN 节点当成客户端、或把攻击者的地址当成客户端），
// 因此格式错误必须在保存时拦下，而不是上线后 silently 不生效。
func validateCIDRList(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > 8192 {
		return fmt.Errorf("清单过长")
	}
	for _, item := range splitCIDRList(v) {
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

// splitCIDRList 把网段清单拆成条目：这类清单通常从 CDN 官方页面整页复制，
// 分隔符按最宽的形式认（换行、逗号、空格）。
func splitCIDRList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t'
	})
}

// validateCustomCountries 校验 node.custom_countries：每行一条 `代码|名称`，
// 代码是两位字母（ISO 3166-1 alpha-2，国旗与下拉框都以它为键）。
// 只允许裸代码（没有名称）——名称留空时前端直接显示代码。
// 重复代码按一条计（后写的覆盖先写的），这里不去重只查格式。
func validateCustomCountries(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > 8192 {
		return fmt.Errorf("清单过长")
	}
	n := 0
	for _, ln := range strings.Split(v, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		n++
		if n > 500 {
			return fmt.Errorf("条目过多（上限 500 行）")
		}
		code, name := ln, ""
		if i := strings.Index(ln, "|"); i >= 0 {
			code, name = ln[:i], ln[i+1:]
		}
		code = strings.ToUpper(strings.TrimSpace(code))
		name = strings.TrimSpace(name)
		if len(code) != 2 {
			return fmt.Errorf("地区代码必须是两位字母：%s", code)
		}
		for _, r := range code {
			if r < 'A' || r > 'Z' {
				return fmt.Errorf("地区代码只能是大写字母：%s", code)
			}
		}
		if len(name) > 64 {
			return fmt.Errorf("地区名称过长：%s", name)
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

// validateCurrencyCode 校验主币种：允许空（=不换算），否则必须是三位大写字母。
func validateCurrencyCode(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if len(v) != 3 {
		return fmt.Errorf("币种代码必须是三位字母（如 CNY / USD）")
	}
	for _, r := range strings.ToUpper(v) {
		if r < 'A' || r > 'Z' {
			return fmt.Errorf("币种代码只能是字母")
		}
	}
	return nil
}

// validateCurrencyRates 校验汇率表：逗号/空白分隔的 `币种:数值`。
//
// 与解析层（model.ParseRates 的"坏条目丢弃"）是两层：这里在保存时就拒绝非法格式，
// 避免管理员存进一份看着像生效、实际大半被丢弃的表；解析层仍保留容错，
// 兜住历史脏值。数值必须 > 0——汇率写成 0 会让对应币种金额全部变 0。
func validateCurrencyRates(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > 2048 {
		return fmt.Errorf("汇率表过长")
	}
	for _, item := range strings.FieldsFunc(v, func(c rune) bool {
		return c == ',' || c == ';' || c == ' ' || c == '\t' || c == '\n' || c == '\r'
	}) {
		code, val, ok := strings.Cut(item, ":")
		if !ok {
			code, val, ok = strings.Cut(item, "=")
		}
		if !ok {
			return fmt.Errorf("汇率条目应形如 USD:7（币种:数值）：%s", item)
		}
		code = strings.ToUpper(strings.TrimSpace(code))
		if len(code) != 3 {
			return fmt.Errorf("币种代码必须是三位字母：%s", code)
		}
		for _, r := range code {
			if r < 'A' || r > 'Z' {
				return fmt.Errorf("币种代码只能是字母：%s", code)
			}
		}
		rate, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil {
			return fmt.Errorf("汇率必须是数字：%s", val)
		}
		if rate <= 0 {
			return fmt.Errorf("汇率必须大于 0（写成 0 会让该币种金额全部变成 0）：%s", item)
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
