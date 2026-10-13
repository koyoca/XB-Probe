package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/probe/internal/acme"
	"github.com/yourorg/probe/internal/config"
)

// 站点形态（site.layout / site.public_hosts / site.admin_hosts）的门控测试。
//
// 这一簇设置回答的是"公开看板与管理后台在两个域名下还是一个域名下"。
// 它比普通的开关更容易出错，原因有两个：
//
//  1. 门控必须**按路径**判域名。同一个入口上 /admin/ 与 / 属于不同的域名，
//     这是"一个端口 + 两个域名 + 反代"的常态；只看入口等于没有分离。
//  2. 配错的方向是有害的：配宽了 = 公开域名能打开后台；配窄了 = 管理员被锁在
//     自己后台外面，而改回来恰好需要访问后台。
//
// 因此测试用"直连门控"的方式做（内层换成一个只会回 204 的桩），把门控的判定
// 与下游处理器的行为彻底分开——下游是谁、能不能跑起来，都不影响这一层的结论。

// layoutProbe 建一个"只记录自己有没有被调到"的下游处理器。
func layoutProbe() (http.Handler, *bool) {
	reached := false
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}), &reached
}

// callGate 用给定 Host 与路径过一遍门控，返回状态码与"下游是否被调到"。
func callGate(t *testing.T, h *Handler, adminEntry bool, path, host string) (int, bool) {
	t.Helper()
	inner, reached := layoutProbe()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	h.siteGate(adminEntry, inner).ServeHTTP(rec, req)
	return rec.Code, *reached
}

// setSiteSettings 往库里写站点形态设置项。
func setSiteSettings(t *testing.T, h *Handler, kv map[string]string) {
	t.Helper()
	now := time.Now().Unix()
	for k, v := range kv {
		if err := h.Repo.SetSetting(k, v, now); err != nil {
			t.Fatalf("写设置 %s 失败：%v", k, err)
		}
	}
}

// 一份白名单都没配 → 不限制。这是本机调试、容器内运行与"还没配域名"的默认形态，
// 它必须保持"零配置可用"，否则升级之后所有人的后台都打不开。
func TestSiteGateUnrestrictedWithoutAnyHosts(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	for _, path := range []string{"/", "/api/public/v1/site", "/admin/", "/api/admin/v1/settings"} {
		code, reached := callGate(t, h, false, path, "随便什么域名")
		if !reached || code != http.StatusNoContent {
			t.Errorf("没有任何白名单时 %s 不应被拦，实得 code=%d reached=%v", path, code, reached)
		}
	}
}

// 分离形态：公开路径只认公开域名，管理路径只认管理域名。
func TestSiteGateSplitIsPathAware(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	setSiteSettings(t, h, map[string]string{
		"site.layout":       siteLayoutSplit,
		"site.public_hosts": "status.example.com",
		"site.admin_hosts":  "admin.example.com",
	})

	cases := []struct {
		path string
		host string
		want int
	}{
		{"/", "status.example.com", http.StatusNoContent},
		{"/api/public/v1/site", "status.example.com", http.StatusNoContent},
		{"/", "admin.example.com", http.StatusMisdirectedRequest},
		// 关键的一条：同一个入口上，管理域名访问管理路径必须放行。
		// 早期实现按"入口"判域名，这种部署下管理后台根本进不去。
		{"/admin/", "admin.example.com", http.StatusNoContent},
		{"/api/admin/v1/settings", "admin.example.com", http.StatusNoContent},
		// 反过来：公开域名不许碰管理路径——这正是分离要挡住的事。
		{"/admin/", "status.example.com", http.StatusMisdirectedRequest},
		{"/api/admin/v1/settings", "status.example.com", http.StatusMisdirectedRequest},
		// 第三个域名两边都不认。
		{"/", "other.example.com", http.StatusMisdirectedRequest},
		{"/admin/", "other.example.com", http.StatusMisdirectedRequest},
	}
	for _, c := range cases {
		code, _ := callGate(t, h, false, c.path, c.host)
		if code != c.want {
			t.Errorf("分离形态 %s @ %s = %d，期望 %d", c.path, c.host, code, c.want)
		}
	}
}

// 同站形态：任一台已配置的域名都能访问两边。
//
// "同站"的含义就是不分彼此，因此不能还留着"公开域名打不开 /admin/"这条规则——
// 那正是选择同站的人要摆脱的东西。
func TestSiteGateSingleTreatsHostsAsOneSite(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	setSiteSettings(t, h, map[string]string{
		"site.layout":       siteLayoutSingle,
		"site.public_hosts": "status.example.com",
		"site.admin_hosts":  "admin.example.com",
	})

	for _, host := range []string{"status.example.com", "admin.example.com"} {
		for _, path := range []string{"/", "/admin/", "/api/admin/v1/settings"} {
			code, reached := callGate(t, h, false, path, host)
			if !reached || code != http.StatusNoContent {
				t.Errorf("同站形态 %s @ %s 应放行，实得 code=%d reached=%v", path, host, code, reached)
			}
		}
	}
	// 名单外的域名仍然要拦：同站不等于不设防。
	if code, _ := callGate(t, h, false, "/", "other.example.com"); code != http.StatusMisdirectedRequest {
		t.Errorf("同站形态下名单外域名应 421，实得 %d", code)
	}
}

// 默认（auto）必须与升级前完全一致：只配了公开域名、没配管理域名时，
// 管理路径**不**被拦。这条是"别用新默认值重新解释存量配置"的护栏——
// 一旦拦上，所有"只配了公开域名 + 同源部署"的存量实例会立刻进不去后台。
func TestSiteGateAutoKeepsLegacyBehaviourWithoutAdminHosts(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	setSiteSettings(t, h, map[string]string{
		"site.public_hosts": "status.example.com",
	})

	code, reached := callGate(t, h, false, "/admin/", "status.example.com")
	if !reached || code != http.StatusNoContent {
		t.Errorf("auto 且未配管理域名时管理路径不应被拦，实得 code=%d reached=%v", code, reached)
	}
	// 但公开路径仍然按公开域名拦。
	if code, _ := callGate(t, h, false, "/", "other.example.com"); code != http.StatusMisdirectedRequest {
		t.Errorf("auto 下公开路径仍应受公开域名约束，实得 %d", code)
	}
}

// 明确选了分离、却只给了一个域名：管理路径一律拒绝。
//
// 这是"配错方向有害"里的窄方向。宁可让管理员立刻看到 421（他手里还有
// master.yaml 可以改），也不能因为缺一个域名就悄悄退化成"公开域名也能进后台"。
func TestSiteGateSplitWithoutAdminHostsRefusesAdminPath(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	setSiteSettings(t, h, map[string]string{
		"site.layout":       siteLayoutSplit,
		"site.public_hosts": "status.example.com",
	})

	code, reached := callGate(t, h, false, "/admin/", "status.example.com")
	if code != http.StatusMisdirectedRequest || reached {
		t.Errorf("分离形态缺管理域名时管理路径必须拒绝，实得 code=%d reached=%v", code, reached)
	}
	if code, _ := callGate(t, h, false, "/api/admin/v1/settings", "admin.example.com"); code != http.StatusMisdirectedRequest {
		t.Errorf("缺管理域名时任何域名的管理路径都应拒绝，实得 %d", code)
	}
}

// 独立的管理端入口（admin_listen 与 public_listen 不同时才会存在）只做白名单，
// 不参与"管理路径在公开入口要不要开放"的判断——那个入口上根本没有公开路径，
// 拿公开域名的判断去套它，会让"分离部署 + 分离监听"这种最该能用的组合反而进不去。
func TestSiteGateAdminEntryOnlyAppliesAllowList(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	setSiteSettings(t, h, map[string]string{
		"site.layout":       siteLayoutSplit,
		"site.public_hosts": "status.example.com",
		"site.admin_hosts":  "admin.example.com",
	})

	if code, reached := callGate(t, h, true, "/admin/", "admin.example.com"); !reached || code != http.StatusNoContent {
		t.Errorf("管理入口上管理域名应放行，实得 code=%d reached=%v", code, reached)
	}
	if code, _ := callGate(t, h, true, "/admin/", "status.example.com"); code != http.StatusMisdirectedRequest {
		t.Errorf("管理入口上公开域名应 421，实得 %d", code)
	}
}

// 后台设置项非空时顶替 master.yaml 里的 host 白名单。
//
// 之所以是顶替而不是并集：白名单是安全开关，管理员在设置页看到的那份就该是
// 真正生效的那份。并集会让"后台删掉的域名依然放行"——那是一种看不见的失效。
func TestSiteGateSettingsOverrideConfigHosts(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	h.PublicHosts = []string{"old.example.com"}
	h.AdminHosts = []string{"oldadmin.example.com"}
	setSiteSettings(t, h, map[string]string{
		"site.public_hosts": "new.example.com",
		"site.admin_hosts":  "newadmin.example.com",
	})

	if code, _ := callGate(t, h, false, "/", "old.example.com"); code != http.StatusMisdirectedRequest {
		t.Errorf("配置文件里的旧公开域名应被顶替（421），实得 %d", code)
	}
	if code, reached := callGate(t, h, false, "/", "new.example.com"); !reached || code != http.StatusNoContent {
		t.Errorf("设置项里的新公开域名应放行，实得 code=%d reached=%v", code, reached)
	}
	if code, _ := callGate(t, h, false, "/admin/", "oldadmin.example.com"); code != http.StatusMisdirectedRequest {
		t.Errorf("配置文件里的旧管理域名应被顶替（421），实得 %d", code)
	}
	if code, reached := callGate(t, h, false, "/admin/", "newadmin.example.com"); !reached || code != http.StatusNoContent {
		t.Errorf("设置项里的新管理域名应放行，实得 code=%d reached=%v", code, reached)
	}
}

// 设置项为空 = 没在后台配过 → 回落到配置文件，而不是"谁都别访问"。
//
// 这两种含义混起来会把整站锁死：把"未配置"当成"配成空清单"，
// 结果是公开域名与管理域名都不再匹配任何请求。
func TestSiteGateEmptySettingFallsBackToConfig(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	h.PublicHosts = []string{"status.example.com"}
	setSiteSettings(t, h, map[string]string{"site.public_hosts": ""})

	if code, reached := callGate(t, h, false, "/", "status.example.com"); !reached || code != http.StatusNoContent {
		t.Errorf("空设置项应回落到配置文件，实得 code=%d reached=%v", code, reached)
	}
	// 光断言"名单内放行"是不够的：把整个回退逻辑删掉、让空值变成"空清单"，
	// 上面那条同样会通过（空清单 = 不限制）。必须再钉一条"名单外仍然被拦"，
	// 才能区分"回落到配置文件"与"退化成不限制"这两种实现。
	if code, _ := callGate(t, h, false, "/", "other.example.com"); code != http.StatusMisdirectedRequest {
		t.Errorf("空设置项时仍应受配置文件约束（名单外 421），实得 %d", code)
	}
}

// 管理路径的识别必须是精确的：只认 /admin 与 /api/admin/。
// 用"路径里含 admin"这种模糊判据会把 /administrator、/admin-old 也算进来，
// 于是公开站上任何一个名字里带 admin 的路径都会被要求管理域名。
func TestIsAdminPathIsExact(t *testing.T) {
	yes := []string{"/admin", "/admin/", "/admin/index.html", "/admin/css/admin.css", "/api/admin/v1/settings"}
	no := []string{"/", "/administrator", "/admin-old", "/api/public/v1/site", "/api/adminX/v1", "/status/admin"}
	for _, p := range yes {
		if !isAdminPath(p) {
			t.Errorf("%s 应被识别为管理路径", p)
		}
	}
	for _, p := range no {
		if isAdminPath(p) {
			t.Errorf("%s 不应被识别为管理路径", p)
		}
	}
}

// 保存前的跨字段校验：选分离却没有把两个域名凑齐时，必须在写入**之前**报错。
//
// 这道校验存在的理由不是"配置要好看"，而是防自锁：分离 + 缺管理域名会让
// 管理后台在保存成功后的下一个请求上返回 421，而把它改回来恰好需要访问后台。
func TestValidateSiteLayoutRejectsSplitWithoutBothHosts(t *testing.T) {
	h, _ := newTOTPTestHandler(t)

	cases := []struct {
		name string
		in   map[string]string
		ok   bool
	}{
		{"分离但一个都没给", map[string]string{"site.layout": siteLayoutSplit}, false},
		{"分离只给公开域名", map[string]string{
			"site.layout": siteLayoutSplit, "site.public_hosts": "status.example.com"}, false},
		{"分离只给管理域名", map[string]string{
			"site.layout": siteLayoutSplit, "site.admin_hosts": "admin.example.com"}, false},
		{"分离且两个都给", map[string]string{
			"site.layout":       siteLayoutSplit,
			"site.public_hosts": "status.example.com",
			"site.admin_hosts":  "admin.example.com"}, true},
		{"同站不需要域名", map[string]string{"site.layout": siteLayoutSingle}, true},
		{"跟随配置文件不额外校验", map[string]string{"site.layout": siteLayoutAuto}, true},
		// 本次没提交 layout，但库里已经是 split —— 也必须校验，
		// 否则"先存 split，再存公开域名"会在第二步留下一个半配置状态。
		{"库里已是分离、本次只给一半", map[string]string{"site.public_hosts": "status.example.com"}, false},
	}
	// 对最后一条：先把库里设成 split。
	if err := h.Repo.SetSetting("site.layout", siteLayoutSplit, time.Now().Unix()); err != nil {
		t.Fatalf("预置设置失败：%v", err)
	}
	for _, c := range cases {
		err := h.validateSiteLayoutFor(c.in)
		if c.ok && err != nil {
			t.Errorf("%s：应通过，实得 %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s：应被拒绝", c.name)
		}
	}
}

// 校验看的是"提交之后最终生效的清单"，而不是只看这一次提交的字段：
// master.yaml 里已经配好两份清单时，选分离当然应该被接受。
func TestValidateSiteLayoutAcceptsConfigProvidedHosts(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	h.PublicHosts = []string{"status.example.com"}
	h.AdminHosts = []string{"admin.example.com"}

	if err := h.validateSiteLayoutFor(map[string]string{"site.layout": siteLayoutSplit}); err != nil {
		t.Errorf("配置文件已提供两份清单时，选分离应通过，实得 %v", err)
	}
}

// 接线测试：PublicRoutes 的中间件链里必须真的挂着这道门。
//
// 上面那些直连门控的用例只证明了"门控本身是对的"；如果 router 忘了挂它，
// 或者哪天被别的中间件绕过，它们一条都不会变红。
func TestPublicRoutesEnforcesAdminHostSplit(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	h.PublicHosts = []string{"status.example.com"}
	h.AdminHosts = []string{"admin.example.com"}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.Host = "status.example.com"
	h.PublicRoutes().ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("公开域名访问 /admin/ 应被公开入口挡下（421），实得 %d", rec.Code)
	}
}

// 管理入口同样要挂门：拿公开域名去访问管理端监听，必须 421 而不是把登录页交出去。
func TestAdminRoutesEnforcesAdminHostSplit(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	h.AdminHosts = []string{"admin.example.com"}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.Host = "status.example.com"
	h.AdminRoutes().ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("管理入口收到错域名应 421，实得 %d", rec.Code)
	}
}

// site.public_hosts / site.admin_hosts 的取值校验。
//
// 最宽的分隔符 + 最严的条目形态：清单是人工粘贴来的，"多一个换行导致整条被拒"
// 会立刻劝退人；而把 URL 当成域名收下更糟——协议与路径会被静默丢掉，
// 于是"配了却永远对不上"。
func TestValidateHostList(t *testing.T) {
	ok := []string{
		"",
		"status.example.com",
		"status.example.com,admin.example.com",
		"status.example.com\n admin.example.com", // 换行 + 空格
		"status.example.com;\tadmin.example.com\r", // 分号 + 制表 + 回车
		"admin.example.com:8443",                   // 非默认端口：显式写出才算同一个源
		"192.0.2.10", "192.0.2.10:8080",            // IP 字面量
		"[2001:db8::1]:8443",           // IPv6 字面量
		"probe.lan", "a-b.example.com", // 内网名与连字符
	}
	for _, v := range ok {
		if err := validateHostList(v); err != nil {
			t.Errorf("%q 应通过，实得 %v", v, err)
		}
	}
	bad := []string{
		"https://admin.example.com", // 带协议
		"admin.example.com/admin",   // 带路径
		"admin.example.com?x=1",     // 带查询串
		"admin.example.com:abc",     // 端口不是数字
		"admin.example.com:",        // 冒号后没有端口
		"a..b",                      // 空标签
		"..",                        // 只有点
		strings.Repeat("a", 600),    // 过长
	}
	for _, v := range bad {
		if err := validateHostList(v); err == nil {
			t.Errorf("%q 应被拒绝", v)
		}
	}
}

// 后台这一侧的校验与配置文件那一侧必须是同一套判定。
//
// 两条路径分开写的话，漂移的表现是"同一个域名在 master.yaml 里能写、
// 在后台设置里存不进去"——这种不一致没法用直觉解释，只能靠人为保持同步，
// 而人为保持同步迟早失败。这条测试直接从两边各问一次，结论必须一致。
func TestHostWhitelistRuleMatchesConfig(t *testing.T) {
	for _, v := range []string{
		"status.example.com", "status.example.com:8443", "192.0.2.10", "probe.lan",
		"https://x.example.com", "x.example.com/y", "a..b", "x.example.com:abc", "x.example.com:",
	} {
		configErr := config.CheckHostWhitelistEntry(v)
		// 后台侧是一个"清单"入口，单个条目直接当作清单来喂。
		settingsErr := validateHostList(v)

		// 后台侧额外有一条"整份清单长度"的上限，这里用短值，不触发它。
		if (configErr == nil) != (settingsErr == nil) {
			t.Errorf("%q 两侧判定不一致：配置文件 %v，后台设置 %v", v, configErr, settingsErr)
		}
	}
}

// 后台下拉框给出的取值必须与这一侧接受的取值**完全一致**。
//
// 两处漂移的症状很隐蔽：服务端接受 auto/single/split，界面却只给了两项，
// 于是某个合法形态在界面上根本选不出来，管理员会以为它不存在；反过来，
// 界面上给了一项服务端不认的取值，保存直接 400，而那看起来像是"后台坏了"。
//
// 取值清单来自 siteLayoutValues（服务端的唯一来源），界面那份从 admin.js 里
// 读出来比对——不是各写一份常量再互相对齐。
func TestSiteLayoutOptionsMatchBackendValues(t *testing.T) {
	raw, err := os.ReadFile("adminui/admin.js")
	if err != nil {
		t.Fatalf("读不到 admin.js：%v", err)
	}
	js := string(raw)

	re := regexp.MustCompile(`'site\.layout':\s*\[\s*'[^']*',\s*'select:([^']*)'`)
	m := re.FindStringSubmatch(js)
	if m == nil {
		t.Fatalf("admin.js 里找不到 site.layout 的下拉定义——判据与写法脱节了，" +
			"这条护栏已经形同虚设（它会假装通过）")
	}

	got := map[string]bool{}
	for _, opt := range strings.Split(m[1], ",") {
		if eq := strings.Index(opt, "="); eq >= 0 {
			got[opt[:eq]] = true
		} else {
			got[opt] = true
		}
	}

	want := map[string]bool{}
	for _, v := range siteLayoutValues {
		want[v] = true
		if !got[v] {
			t.Errorf("服务端接受 %q，但后台下拉框里没有这个选项——管理员选不出来", v)
		}
		// 顺带确认这个取值真的能存进去（取值清单与校验器不许各说各话）。
		if err := validateSettingValue("site.layout", v); err != nil {
			t.Errorf("取值 %q 应在 siteLayoutValues 里，却被校验器拒绝：%v", v, err)
		}
	}
	for v := range got {
		if !want[v] {
			t.Errorf("后台下拉框给了 %q，但服务端不接受它——保存必然 400", v)
		}
	}
}

// ACME 挑战路径必须豁免域名门控。
//
// 这条曾经在实现里丢掉过：原来的 hostGate 单独写了豁免分支，门控重写之后
// 只在"同站"那一条路径上复用了 hostGate，分离路径上的挑战请求就变成了 421。
// 后果很隐蔽——证书续期在几周后才失败，而那时的现象是"整站证书过期"，
// 与"域名白名单"看上去毫无关系。端到端冒烟比单元测试更早抓到了它。
func TestSiteGateExemptsACMEChallenge(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	setSiteSettings(t, h, map[string]string{
		"site.layout":       siteLayoutSplit,
		"site.public_hosts": "status.example.com",
		"site.admin_hosts":  "admin.example.com",
	})

	// 挑战路径在任意 Host 下都必须放行——CA 是按 IP 访问的，
	// 它发来的 Host 是 IP 字面量，永远命中不了域名白名单。
	for _, path := range []string{acme.HTTP01Prefix + "tok", acme.HTTP01Prefix} {
		for _, host := range []string{"203.0.113.7", "bogus.example.com"} {
			code, reached := callGate(t, h, false, path, host)
			if !reached || code != http.StatusNoContent {
				t.Errorf("分离形态下 %s @ %s 必须豁免门控，实得 code=%d reached=%v", path, host, code, reached)
			}
		}
	}
	// 同站形态是另一条分支，同样要豁免。
	setSiteSettings(t, h, map[string]string{"site.layout": siteLayoutSingle})
	if code, reached := callGate(t, h, false, acme.HTTP01Prefix+"tok", "203.0.113.7"); !reached || code != http.StatusNoContent {
		t.Errorf("同站形态下挑战路径也必须豁免，实得 code=%d reached=%v", code, reached)
	}
}

// 白名单与形态必须**每次请求现读**，而不是在装配门控时读一次。
//
// 站点形态是可以在后台改的运行时设置，而门控是进程启动时装配好的。
// 若在构造时把清单读进闭包变量，"改完即时生效"就是假的：界面上显示已分离、
// 实际仍在放行，而且没有任何报错——这一类"设置改了不生效"正是本项目最想
// 消灭的缺陷形态。所以这里先把门控/路由**构造一次**，再改设置，最后断言
// 同一个处理器上的行为已经跟着变。
func TestSiteGateReadsSettingsPerRequest(t *testing.T) {
	h, _ := newTOTPTestHandler(t)
	setSiteSettings(t, h, map[string]string{
		"site.layout":       siteLayoutSplit,
		"site.public_hosts": "status.example.com",
		"site.admin_hosts":  "admin.example.com",
	})

	// 只构造一次，之后一直用它。
	gate := h.siteGate(false, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	probe := func(path, host string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = host
		gate.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := probe("/", "bogus.example.com"); got != http.StatusMisdirectedRequest {
		t.Fatalf("分离形态下未知域名应 421，实得 %d（基线不成立，后面的断言没有意义）", got)
	}
	// 改成同站：立刻生效，不需要重新构造处理器。
	setSiteSettings(t, h, map[string]string{"site.layout": siteLayoutSingle})
	if got := probe("/", "bogus.example.com"); got != http.StatusMisdirectedRequest {
		// 同站仍然是"已配置域名的并集"，bogus 不在名单里，所以照旧 421。
		t.Errorf("同站形态下名单外域名仍应 421，实得 %d", got)
	}
	if got := probe("/", "admin.example.com"); got != http.StatusNoContent {
		t.Errorf("改设置后同一个处理器应立即放行管理域名，实得 %d（说明清单被冻结在构造时了）", got)
	}
	// 再改回分离：管理域名访问公开站立刻变回 421。
	setSiteSettings(t, h, map[string]string{"site.layout": siteLayoutSplit})
	if got := probe("/", "admin.example.com"); got != http.StatusMisdirectedRequest {
		t.Errorf("改回分离后管理域名访问公开站应立即 421，实得 %d", got)
	}
}

// 路由装配层同样不能冻结：处理器构造好之后再改设置，行为必须跟着变。
//
// 这里只断言"拒绝"这一个方向——它在门控里就短路返回了，不会走到业务处理器；
// 而"放行"那一步要真的执行到处理器（这个裸 Handler 没有主题与管线，
// 跑到那里会 panic），属于端到端冒烟第 10b 节的职责。
func TestRoutesReadSettingsPerRequest(t *testing.T) {
	h, _ := newTOTPTestHandler(t)

	pub := h.PublicRoutes()
	adm := h.AdminRoutes()

	setSiteSettings(t, h, map[string]string{
		"site.layout":       siteLayoutSplit,
		"site.public_hosts": "status.example.com",
		"site.admin_hosts":  "admin.example.com",
	})

	cases := []struct {
		name string
		h    http.Handler
		path string
		host string
	}{
		{"公开入口：未知域名", pub, "/api/public/v1/site", "bogus.example.com"},
		{"公开入口：管理域名不该看到公开站", pub, "/api/public/v1/site", "admin.example.com"},
		{"管理入口：公开域名不该看到管理 API", adm, "/api/admin/v1/auth/me", "status.example.com"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		req.Host = c.host
		c.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("%s：路由构造之后再配域名应立即生效（421），实得 %d", c.name, rec.Code)
		}
	}
}
