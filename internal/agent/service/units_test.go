package service

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 本文件的断言都落在**渲染结果**上，而不是 grep 源码。
//
// 这不是风格偏好，是这条护栏能不能成立的前提：模板原先写在各自带 `//go:build` 的文件里，
// 本机是 Windows —— 只有 Linux/FreeBSD 才能编译到的那一半，改动是对是错本机根本验不到
// （"全绿只是 Windows 的结论"）。搬进 units.go（无 build tag 的纯函数）之后就随时可断言。
//
// 背景是用户真机上的这个故障：
//
//	配置写入 /etc/probe-agent/agent.yaml（权限 0600）
//	probe-agent[558816]: 错误： 读取配置失败: open /etc/probe-agent/agent.yaml: permission denied
//
// 安装的每一步都打印"成功"，唯一失败的是服务本身 —— 因为 unit 里 `User=probe`
// 而配置文件是 `0600 root:root`。

const (
	tBin  = "/opt/probe/probe-agent"
	tCfg  = "/opt/probe/agent.yaml"
	tUser = "probe"

	defBin = "/usr/local/bin/probe-agent"
	defCfg = "/etc/probe-agent/agent.yaml"
)

// serviceRenderers 把五个平台的服务定义都拿出来（FreeBSD 的签名少一个 user 参数，包一层）。
func serviceRenderers() map[string]func(bin, cfg, user string) string {
	return map[string]func(bin, cfg, user string) string{
		"systemd": renderSystemdUnit,
		"openrc":  renderOpenRCScript,
		"procd":   renderProcdScript,
		"sysv":    renderSysVScript,
		"freebsd": func(bin, cfg, _ string) string { return renderFreeBSDRcScript(bin, cfg) },
	}
}

// TestEveryServiceScriptUsesThePathsItWasGiven 钉住"三条路径只能来自参数"。
//
// 这些模板过去把二进制路径写死成 `/usr/local/bin/probe-agent`（openrc、sysv）或
// `/usr/bin/probe-agent`（procd）。而 Install 是收 binPath 的：`--dir /opt/probe`
// 装出来的服务指向一个不存在的文件，症状是"服务起了就退出"，与真正的原因隔得很远。
func TestEveryServiceScriptUsesThePathsItWasGiven(t *testing.T) {
	// 每种方言里"这两件事"各自的写法，逐个点名，而不是只查一个公共子串。
	extra := map[string][]string{
		"systemd": {"ReadWritePaths=" + filepath.Dir(tCfg)},
		"openrc":  {`command="` + tBin + `"`, `command_user="` + tUser + `"`},
		"procd":   {"command " + tBin + " run --config " + tCfg, "procd_set_param user " + tUser},
		"sysv":    {`DAEMON="` + tBin + `"`, `USER="` + tUser + `"`},
		"freebsd": {`procname="` + tBin + `"`, "run --config " + tCfg},
	}

	for name, render := range serviceRenderers() {
		t.Run(name, func(t *testing.T) {
			out := render(tBin, tCfg, tUser)

			if !strings.Contains(out, tBin) {
				t.Errorf("脚本里没有出现传入的二进制路径 %s：\n%s", tBin, out)
			}
			if !strings.Contains(out, tCfg) {
				t.Errorf("脚本里没有出现传入的配置路径 %s：\n%s", tCfg, out)
			}
			// 渲染残留：参数个数与模板里的 %s 不一致时，fmt 会把这些字面量拼进结果。
			// FreeBSD 那份就是这么错的（3 个 %s 传了 4 个参数），而 `%!(EXTRA string=probe)`
			// 会被 sh 当命令执行 —— `service probe_agent start` 的退出码来自一次
			// "command not found"，看起来像服务起不来。
			if strings.Contains(out, "%!") || strings.Contains(out, "%s") {
				t.Errorf("渲染结果里有未替换的格式残留（参数个数与模板不匹配）:\n%s", out)
			}
			// 写死的默认路径：默认值只有一份来源（调用方传来的 binPath/cfgPath）。
			for _, hard := range []string{
				"/usr/local/bin/probe-agent", "/usr/bin/probe-agent", "/etc/probe-agent",
			} {
				if strings.Contains(out, hard) {
					t.Errorf("脚本里写死了 %q —— 它必须来自参数（--dir 装出来的服务会指向错的地方）:\n%s",
						hard, out)
				}
			}
			for _, want := range extra[name] {
				if !strings.Contains(out, want) {
					t.Errorf("缺少 %q：\n%s", want, out)
				}
			}
		})
	}
}

// TestSystemdUnitKeepsSandboxAndLetsAgentWriteItsConfig 钉住沙箱与"可写路径"同时成立。
//
// ProtectSystem=strict 会把整个文件系统（含 /etc）对服务挂成只读，而 Agent 运行期
// **要写回配置**：Master 改了探测目标/间隔后 syncConfig 会落盘，那是"重启后还生效"
// 的唯一保证。少了 ReadWritePaths，服务能跑、上报也正常，只有远端配置落盘失败，
// 并被记成"拉取配置失败"——离真正的原因（权限）很远。
func TestSystemdUnitKeepsSandboxAndLetsAgentWriteItsConfig(t *testing.T) {
	def := renderSystemdUnit(defBin, defCfg, tUser)
	for _, want := range []string{
		"User=" + tUser,
		"ExecStart=" + defBin + " run --config " + defCfg,
		"Restart=always",
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"AmbientCapabilities=CAP_NET_RAW",
		"ReadWritePaths=" + filepath.Dir(defCfg),
	} {
		if !strings.Contains(def, want) {
			t.Errorf("systemd 单元缺少 %q：\n%s", want, def)
		}
	}

	// 关键：ReadWritePaths 必须随配置目录走。写死 /etc/probe-agent 的话，
	// 所有 `--dir` 安装都会失去写回能力（读得到、写不回去）。
	custom := renderSystemdUnit(tBin, tCfg, tUser)
	if !strings.Contains(custom, "ReadWritePaths="+filepath.Dir(tCfg)) {
		t.Errorf("自定义配置目录时 ReadWritePaths 没跟着走：\n%s", custom)
	}
	if strings.Contains(custom, "ReadWritePaths="+filepath.Dir(defCfg)) {
		t.Errorf("自定义配置目录时仍写死默认目录 %s：\n%s", filepath.Dir(defCfg), custom)
	}
}

// TestHandoverTargetsGiveServiceUserReadAndWrite 钉住"交接哪几条路径、什么权限"。
//
// 目录必须**可写**（属主可写）：运行期 agentconfig.Save 要在同目录写
// `agent.yaml.tmp` 再 rename。只给读权限的话配置能读、写不回去。
func TestHandoverTargetsGiveServiceUserReadAndWrite(t *testing.T) {
	dir := filepath.Dir(defCfg)
	byPath := map[string]handoverPath{}
	for _, p := range handoverTargets(defCfg) {
		byPath[p.Path] = p
	}

	// 精确比对整张表：多一条是越权，少一条是运行期 permission denied。
	for _, want := range []string{dir, defCfg, filepath.Join(dir, ManifestName)} {
		if _, ok := byPath[want]; !ok {
			t.Errorf("交接清单里缺少 %s（清单=%v）", want, byPath)
		}
	}
	if len(byPath) != 3 {
		t.Errorf("交接清单应恰好是「目录 + 配置 + 安装清单」3 条，实际 %d 条：%v", len(byPath), byPath)
	}

	d := byPath[dir]
	if !d.Dir {
		t.Error("配置目录没有被标记为目录")
	}
	if d.Mode.Perm() != 0o750 {
		t.Errorf("配置目录权限应为 0750，实际 %#o", d.Mode.Perm())
	}
	if d.Mode.Perm()&0o200 == 0 {
		t.Error("配置目录对属主不可写：运行期写回 agent.yaml.tmp 会失败（后台改的设置重启就没了）")
	}
	if c := byPath[defCfg]; c.Mode.Perm() != 0o600 || c.Dir {
		t.Errorf("配置文件应为 0600 的普通文件，实际 mode=%#o dir=%v", c.Mode.Perm(), c.Dir)
	}
	if m := byPath[filepath.Join(dir, ManifestName)]; m.Mode.Perm() != 0o600 {
		t.Errorf("安装清单权限应为 0600，实际 %#o", m.Mode.Perm())
	}

	// 目录必须由 cfgPath 推导（--dir 场景）。
	other := filepath.Dir(tCfg)
	got := handoverTargets(tCfg)
	if len(got) == 0 || got[0].Path != other {
		t.Fatalf("交接的目录不是 cfgPath 的父目录：%v（期望 %s）", got, other)
	}
}

// TestLinuxManagersHandoverConfigRightAfterCreatingUser 钉住"调用发生了、而且时机对"。
//
// 为什么要在这条上做源码断言：四个 manager 的 Install 都必须在**建好用户之后、
// 启动服务之前**交接，而这段代码在本机（Windows）编不到、也跑不了。
//
// 判据的形状是有讲究的：
//   - **切区间**再判，不全文搜索——本文件里四个 manager 都有 Install，
//     全文找 `HandoverToUser(` 会被"另一个 manager 的实现"满足（替身作证）；
//   - 判的是**次序**（ensureUser < HandoverToUser < 起服务），不是"某个字符串相邻"。
//     相邻写法一改注释就失灵（第一版就是这么红的），而次序才是真正的要求：
//     交接早于建用户查不到 UID，晚于起服务则服务已经以 probe 身份读过一次配置了。
//   - 再补一条**数量守恒**：每个 Install 实现都必须有一次交接。只遍历四个已知名字的话，
//     将来新增一个 manager 却忘了交接，测试照样全绿。
func TestLinuxManagersHandoverConfigRightAfterCreatingUser(t *testing.T) {
	src := readSourceFile(t, "service_linux.go")

	// 每个 manager 起服务的那一行（各自的方言，"start" 这个词在 systemd 那里并不出现）。
	managers := []struct {
		name  string
		start string
	}{
		{"systemdManager", `"systemctl", "enable", "--now"`},
		{"openrcManager", `"rc-service", "probe-agent", "start"`},
		{"procdManager", `run(procdInitPath, "start")`},
		{"sysvManager", `run(sysvInitPath, "start")`},
	}
	for _, m := range managers {
		// 判据钉在 writeDefinition 上：它是"建用户 + 交接属主 + 写定义"的那一段，
		// 启动命令**只能在** Install 里（升级路径会调 RewriteDefinition，
		// 那一步绝不能让服务在用户没预期的时刻重启）。
		write := funcBody(t, src, "func (m *"+m.name+") writeDefinition(binPath, cfgPath, user string) error {")
		inst := funcBody(t, src, "func (m *"+m.name+") Install(binPath, cfgPath, user string) error {")
		rew := funcBody(t, src, "func (m *"+m.name+") RewriteDefinition(binPath, cfgPath, user string) error {")

		iEnsure := strings.Index(write, "ensureUser(user, binPath)")
		iHand := strings.Index(write, "HandoverToUser(cfgPath, user)")
		iStart := strings.Index(inst, m.start)
		if iEnsure < 0 || iStart < 0 {
			t.Fatalf("%s 切不出关键片段（writeDefinition 里建用户=%d、Install 里起服务=%d）："+
				"结构变了，判据要跟着改", m.name, iEnsure, iStart)
		}
		if iHand < 0 {
			t.Errorf("%s 建完专用用户却没有交接配置属主：\n"+
				"  服务以该用户启动后会去读配置文件，而它是以 root 身份写的 0600 ——\n"+
				"  症状就是真机上的「读取配置失败: permission denied」。", m.name)
			continue
		}
		if iHand < iEnsure {
			t.Errorf("%s 在创建用户之前就交接属主：那时还查不到该用户的 UID", m.name)
		}
		// 建用户与交接都在 writeDefinition 里，而启动在 Install 里 ——
		// "交接早于启动"因此由结构保证；下面再钉住 Install 确实是先写定义后启动。
		if !strings.Contains(inst, "m.writeDefinition(") {
			t.Errorf("%s 的 Install 没有委托 writeDefinition：\n"+
				"  两份实现的下场是「修好了安装、忘了升级」，而这两条路必须走同一段代码。", m.name)
		} else if i := strings.Index(inst, "m.writeDefinition("); i > iStart {
			t.Errorf("%s 的 Install 先起服务再写定义：服务已经以该用户身份读过一次配置了", m.name)
		}
		// RewriteDefinition 必须是**一层委托**：既不能自己再写一份定义，
		// 更不能顺手把服务起起来。
		if !strings.Contains(rew, "m.writeDefinition(") {
			t.Errorf("%s 的 RewriteDefinition 没有委托 writeDefinition：\n"+
				"  它必须与 Install 共用同一段「写定义」代码（否则升级修出来的定义和装出来的不一样）。", m.name)
		}
		// "不启动"要**两边都查**：RewriteDefinition 委托给 writeDefinition，
		// 所以启动命令出现在**被委托的那一段**里同样会被升级路径执行到。
		// （这一版最初只查了 RewriteDefinition，变异 M01 把启动命令放进
		//  writeDefinition —— 判据全绿，等于没防住。）
		for _, body := range []struct{ what, text string }{
			{"writeDefinition", write},
			{"RewriteDefinition", rew},
		} {
			for _, forbid := range []string{m.start, "m.Install("} {
				if strings.Contains(body.text, forbid) {
					t.Errorf("%s 的 %s 里出现了 %s：\n"+
						"  这两段都是「只写定义」用的 —— 升级路径会在用户正用着服务的时候调它，\n"+
						"  升级进程有时就是那个服务自己（自杀）。启动只能留在 Install 里。",
						m.name, body.what, forbid)
				}
			}
		}
	}

	// 数量守恒：每个 Install 实现都必须有一次交接。只遍历四个已知名字的话，
	// 将来新增一个 manager 却忘了交接，测试照样全绿。
	// 判据从"Install 数 == 交接数"改成"建用户数 == 交接数"：交接现在就发生在
	// 建用户的旁边（同一个 writeDefinition），两个动作一一对应。
	ensures := strings.Count(src, "ensureUser(user, binPath)")
	handovers := strings.Count(src, "HandoverToUser(cfgPath, user)")
	if ensures != handovers {
		t.Errorf("建用户 %d 处，交接配置属主 %d 处：每个新建专用用户的服务实现都必须交接"+
			"（新增 manager 时最容易漏的就是这一步）", ensures, handovers)
	}
	writes := strings.Count(src, ") writeDefinition(binPath, cfgPath, user string) error {")
	rewrites := strings.Count(src, ") RewriteDefinition(binPath, cfgPath, user string) error {")
	if writes == 0 || writes != rewrites {
		t.Errorf("writeDefinition %d 个，RewriteDefinition %d 个：每个服务实现都要能被「只写定义」地重写"+
			"（升级路径靠它），少一个就等于那条路上这个平台修不了", writes, rewrites)
	}

	// 用户名的唯一来源：各 manager 不许再写自己的字面量。
	for _, f := range []string{"service_linux.go", "service_freebsd.go"} {
		if src := readSourceFile(t, f); strings.Contains(src, `user = "probe"`) {
			t.Errorf("%s 里还有写死的用户名字面量，应统一用 DefaultUser"+
				"（两处不一致会得到「用户建了、文件交给了别人」）", f)
		}
	}
}

// TestRewriteDefinitionNeverStartsTheService 把"只写定义"这个契约在**每个平台**上都钉住。
//
// 为什么单列一条：Linux 那四个 manager 上面已经覆盖了，但 FreeBSD 与 Windows 的实现
// 带 build tag，本机的 `go test` 跑不到它们 —— 而那正是"改坏了也没人发现"的那一半
// （units.go 顶部记着一次同类事故：freebsd 模板参数个数不对，只在真机上现形）。
// 这里按**源码文本**断言，不受 build tag 影响。
func TestRewriteDefinitionNeverStartsTheService(t *testing.T) {
	for _, c := range []struct {
		file   string
		bodies []string
		forbid []string
	}{
		{
			file: "service_freebsd.go",
			bodies: []string{
				"func (m *freebsdManager) writeDefinition(binPath, cfgPath, user string) error {",
				"func (m *freebsdManager) RewriteDefinition(binPath, cfgPath, user string) error {",
			},
			// rc.d 的启动命令，以及"设置开机自启"（它不属于"写定义"这一步：
			// 升级路径不该顺手改开机自启，那是安装在做的事）。
			forbid: []string{`"service", "probe_agent", "start"`, `run("sysrc"`},
		},
		{
			file: "service_windows.go",
			bodies: []string{
				"func (m *windowsManager) writeDefinition(binPath, cfgPath string) error {",
				"func (m *windowsManager) RewriteDefinition(binPath, cfgPath, user string) error {",
			},
			// 删了重建会把正在跑的服务停掉（Install 才那么做）；
			// 升级路径只 UpdateConfig，不 Start / Stop / Delete。
			forbid: []string{"s.Start()", "s.Control(svc.Stop)", "s.Delete()"},
		},
	} {
		src := readSourceFile(t, c.file)
		for _, sig := range c.bodies {
			body := funcBody(t, src, sig)
			if !strings.Contains(body, "\n") {
				t.Fatalf("%s 里切不出 %q 的函数体：结构变了，判据要跟着改", c.file, sig)
			}
			for _, bad := range c.forbid {
				if strings.Contains(body, bad) {
					t.Errorf("%s 的 %s 里出现了 %s：\n"+
						"  RewriteDefinition 只写定义、不启动不重启（升级路径会在服务运行中调它），"+
						"  要启动就把它留在 Install 里。", c.file, sig, bad)
				}
			}
		}
	}
}

// TestRewriteDefinitionDoesNotFabricateAService 钉住"没注册就别修"。
//
// 容器里用 `--no-service` 跑、或手工前台跑的节点本来就没有服务定义；升级时照样写一份
// 出来，就是凭空给用户造了一个他没要过的系统状态。
//
// 判据必须落在 **RewriteDefinition** 上而不是共用的 writeDefinition：Install 走的是
// 同一段写定义代码，而**新装**时 unit 恰恰还不存在 —— 检查放错地方会把新装直接挡死
// （这条"放错位置"的变异在 M13 里有一份对照）。
func TestRewriteDefinitionDoesNotFabricateAService(t *testing.T) {
	for _, c := range []struct{ file, sig string }{
		{"service_linux.go", "func (m *systemdManager) RewriteDefinition(binPath, cfgPath, user string) error {"},
		{"service_linux.go", "func (m *openrcManager) RewriteDefinition(binPath, cfgPath, user string) error {"},
		{"service_linux.go", "func (m *procdManager) RewriteDefinition(binPath, cfgPath, user string) error {"},
		{"service_linux.go", "func (m *sysvManager) RewriteDefinition(binPath, cfgPath, user string) error {"},
		{"service_freebsd.go", "func (m *freebsdManager) RewriteDefinition(binPath, cfgPath, user string) error {"},
		// Windows 是例外：它的 Install **不委托** writeDefinition（自己 CreateService），
		// 所以这道检查就放在 writeDefinition 里 —— 下一段会钉住那个前提。
		{"service_windows.go", "func (m *windowsManager) writeDefinition(binPath, cfgPath string) error {"},
	} {
		body := funcBody(t, readSourceFile(t, c.file), c.sig)
		if !strings.Contains(body, "ErrNotInstalled") {
			t.Errorf("%s 的 %s 没有「没注册就别修」的判断：\n"+
				"  没有服务定义时它会照样写一份出来 —— 容器/--no-service 的节点升级一次\n"+
				"  就凭空多出一个 unit（还要为此收到一条「警告」）。", c.file, c.sig)
		}
	}

	// Windows 的前提：Install 一旦也改成委托 writeDefinition，上面那道检查就会
	// 把**新装**挡死（新装时 SCM 里当然还没有这个服务）。前提变了，判据要跟着搬。
	if inst := funcBody(t, readSourceFile(t, "service_windows.go"),
		"func (m *windowsManager) Install(binPath, cfgPath, user string) error {"); strings.Contains(inst, "m.writeDefinition(") {
		t.Error("windowsManager.Install 开始委托 writeDefinition 了：\n" +
			"  那 writeDefinition 里的「没注册就别修」会把新装挡死（新装时服务还不存在）。\n" +
			"  正确做法是把那道检查移到 RewriteDefinition（与 Linux 侧一致）。")
	}

	// 反向：共用的 writeDefinition 里**不能**有这道检查。
	for _, sig := range []string{
		"func (m *systemdManager) writeDefinition(binPath, cfgPath, user string) error {",
		"func (m *openrcManager) writeDefinition(binPath, cfgPath, user string) error {",
		"func (m *procdManager) writeDefinition(binPath, cfgPath, user string) error {",
		"func (m *sysvManager) writeDefinition(binPath, cfgPath, user string) error {",
	} {
		body := funcBody(t, readSourceFile(t, "service_linux.go"), sig)
		if strings.Contains(body, "ErrNotInstalled") {
			t.Errorf("%s 里出现了「没注册就别修」的判断：\n"+
				"  Install 走的是同一段代码，而新装时 unit 还不存在 —— 放这里会把新装挡死。", sig)
		}
	}
}

// 上面那些渲染器测试验的是"模板拿到参数后渲染得对不对"，而**参数是从哪来的**在
// service_linux.go / service_freebsd.go 里 —— 那两个文件带 build tag，本机编不到也跑不了，
// 于是"调用点把 binPath 写死"这种改动，渲染器测试全绿、真机上服务却指向错路径。
//
// 判据用正则数**调用次数**，而不是钉死某一行文本：
//   - 调用总数与"把 (binPath, cfgPath) 原样传下去"的次数必须相等 ——
//     只要有一处改传字面量，两个数就对不上；
//   - 每加一个服务实现就得加一处调用，数量表要一起改（改了就是提醒你复核接线）。
func TestBuildTaggedFilesPassTheRealPathsToRenderers(t *testing.T) {
	callRe := regexp.MustCompile(`render[A-Za-z]+\(`)
	withPathsRe := regexp.MustCompile(`render[A-Za-z]+\(binPath, cfgPath`)

	for _, c := range []struct {
		file string
		want int
	}{
		{"service_linux.go", 4},   // systemd / openrc / procd / sysv
		{"service_freebsd.go", 1}, // rc.d
	} {
		src := readSourceFile(t, c.file)
		calls := len(callRe.FindAllString(src, -1))
		withPaths := len(withPathsRe.FindAllString(src, -1))
		if calls != c.want {
			t.Errorf("%s 里渲染器调用 %d 处，期望 %d 处：判据与源码结构脱节了",
				c.file, calls, c.want)
		}
		if withPaths != calls {
			t.Errorf("%s 里有 %d 处渲染器调用没把 (binPath, cfgPath) 原样传下去（共 %d 处）：\n"+
				"  写死路径的服务脚本指向一个不存在的二进制/配置，症状是「服务起了就退出」，\n"+
				"  与真正的原因（调用点写死）隔得很远。", c.file, calls-withPaths, calls)
		}
	}
}

// funcBody 切出一个函数体（签名 → 下一个行首的 `}`）。
func funcBody(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("找不到函数 %q：判据与源码结构脱节了", sig)
	}
	rest := src[i:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读取 %s 失败（测试的工作目录应为本包目录）：%v", name, err)
	}
	return string(raw)
}
