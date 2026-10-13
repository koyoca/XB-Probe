package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/koyoca/xb-probe/internal/agent/service"
)

// ---------------------------------------------------------------------------
// "升级顺手把服务定义与配置属主修好"这一条的护栏
//
// 真机事故：节点是 v0.1.1 装的（那一版的 unit 里没有 ReadWritePaths=<配置目录>，
// 安装时也没做属主交接），`xb-probe-agent upgrade` 升到 v0.1.2 并重启后
//
//	错误： 读取配置失败: open /etc/xb-probe-agent/agent.yaml: permission denied
//
// ——只换二进制修不了这两样。所以升级必须在**不重启**的前提下重写服务定义
// 并把配置交给服务用户。
// ---------------------------------------------------------------------------

// fakeManager 记录 RewriteDefinition 被怎么调用，用来断言"升级确实去修了"。
//
// 为什么必须用替身：真实实现会写 /etc/systemd、连 SCM、chown 文件——
// 单测里跑不了，而"到底调没调"恰恰是本轮最关键的那件事。
type fakeManager struct {
	calls []string
	err   error
}

func (f *fakeManager) Name() string { return "fake" }

func (f *fakeManager) Install(binPath, cfgPath, user string) error { return nil }

func (f *fakeManager) RewriteDefinition(binPath, cfgPath, user string) error {
	f.calls = append(f.calls, binPath+"|"+cfgPath+"|"+user)
	return f.err
}

func (f *fakeManager) Uninstall(binPath, cfgPath string) error { return nil }

// TestRepairServiceDefinitionPassesTheInstalledPaths 钉住"修的是哪份定义"。
//
// 三个参数一个都不能是写死的：binPath 决定 unit 里的 ExecStart、cfgPath 决定
// ReadWritePaths 与 unit 里的 --config、user 决定 unit 里的 User= 与属主交接的目标。
// 传错任何一个，得到的都是一份"看起来修好了、服务还是起不来"的定义。
func TestRepairServiceDefinitionPassesTheInstalledPaths(t *testing.T) {
	f := &fakeManager{}
	if err := repairServiceDefinition(f, "/etc/xb-probe-agent/agent.yaml", "/usr/local/bin/xb-probe-agent"); err != nil {
		t.Fatalf("repairServiceDefinition 返回错误：%v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("RewriteDefinition 调用了 %d 次（应为 1）：要么没修，要么修了不只一次", len(f.calls))
	}
	want := "/usr/local/bin/xb-probe-agent|/etc/xb-probe-agent/agent.yaml|" + service.DefaultUser
	if f.calls[0] != want {
		t.Errorf("RewriteDefinition 收到的参数不对：\n  实际 %s\n  期望 %s", f.calls[0], want)
	}

	// 失败必须能传出去（调用方要靠它决定提示里写"已修复"还是"请手工执行"）。
	f2 := &fakeManager{err: errors.New("模拟失败")}
	if err := repairServiceDefinition(f2, "/etc/xb-probe-agent/agent.yaml", "/usr/local/bin/xb-probe-agent"); err == nil {
		t.Error("RewriteDefinition 报错时 repairServiceDefinition 却返回 nil：失败会被说成修好了")
	}
}

// TestRepairAfterUpgradeSkipsAreExplicitAndActionable 钉住三条"跳过"路径。
//
// 三种情况确实不该动手（用户明说别动、目标不是自己、没权限），但**每一条都必须
// 给出能照着做的补救命令**：跳过而只字不提，用户看到的就是"升级成功了、服务还是起不来"。
func TestRepairAfterUpgradeSkipsAreExplicitAndActionable(t *testing.T) {
	const (
		cfg = "/etc/xb-probe-agent/agent.yaml"
		bin = "/usr/local/bin/xb-probe-agent"
	)
	for _, c := range []struct {
		name         string
		canRepair    bool
		customTarget bool
		skip         bool
		wantWord     string
	}{
		{"用户显式 --no-service-repair", true, false, true, "--no-service-repair"},
		{"--target 指向别的二进制", true, true, false, "--target"},
		{"不是 root", false, false, false, "root"},
	} {
		f := &fakeManager{}
		note := repairAfterUpgrade(f, cfg, bin, c.canRepair, c.customTarget, c.skip)
		if len(f.calls) != 0 {
			t.Errorf("%s：不该动手却调用了 RewriteDefinition（%v）", c.name, f.calls)
		}
		if !strings.Contains(note, c.wantWord) {
			t.Errorf("%s：提示里没有点明原因（缺 %q）：%q", c.name, c.wantWord, note)
		}
		// 补救命令必须原样可复制：这一条就是用户此刻真正需要的那一步。
		if !strings.Contains(note, "install --service-only") {
			t.Errorf("%s：跳过了却没给补救命令。\n"+
				"  用户看到的是「升级成功」而服务起不来，而修它的命令就一个："+
				"sudo xb-probe-agent install --service-only", c.name)
		}
	}
}

// TestRepairAfterUpgradeRunsAndReportsFailure 正常路径：既动手、又在失败时给出补救。
func TestRepairAfterUpgradeRunsAndReportsFailure(t *testing.T) {
	const (
		cfg = "/etc/xb-probe-agent/agent.yaml"
		bin = "/usr/local/bin/xb-probe-agent"
	)
	ok := &fakeManager{}
	if note := repairAfterUpgrade(ok, cfg, bin, true, false, false); len(ok.calls) != 1 {
		t.Fatalf("该修的时候没调 RewriteDefinition（%v）：%q", ok.calls, note)
	} else if !strings.Contains(note, "重写服务定义") {
		t.Errorf("修好了却没说清楚修了什么：%q", note)
	}

	bad := &fakeManager{err: errors.New("模拟权限不足")}
	note := repairAfterUpgrade(bad, cfg, bin, true, false, false)
	if !strings.Contains(note, "模拟权限不足") {
		t.Errorf("修复失败时提示里没有带上原因：%q", note)
	}
	if !strings.Contains(note, "install --service-only") {
		t.Errorf("修复失败时没给补救命令：%q", note)
	}
	// 修复失败**不能**把升级说成失败：二进制已经换好、下次重启才生效的东西，
	// 不该让用户以为"升级没成功"而反复重试（每次都要重新下几十 MB）。
	if strings.Contains(note, "升级失败") {
		t.Errorf("修复失败的提示里出现了「升级失败」，会把用户引到错误的路上：%q", note)
	}
}

// TestRepairAfterUpgradeTreatsNotInstalledAsNormal 钉住"没装服务"不算失败。
//
// 容器里 `--no-service` 跑的节点本来就没有服务定义可改。报成"警告：重写服务定义失败"
// 会让用户去查一个不存在的问题 —— 而真正的含义是"这台机器不需要这一步"。
// 这里还顺带验 `errors.Is` 能穿过包装：各平台返回的是包了一层的错误
// （Windows 那份会带上 SCM 的原始报错），直接比对 `== ErrNotInstalled` 是认不出来的。
func TestRepairAfterUpgradeTreatsNotInstalledAsNormal(t *testing.T) {
	f := &fakeManager{err: fmt.Errorf("%w（SCM 里没有 xb-probe-agent）：模拟", service.ErrNotInstalled)}
	note := repairAfterUpgrade(f, "/etc/xb-probe-agent/agent.yaml", "/usr/local/bin/xb-probe-agent", true, false, false)

	if strings.Contains(note, "警告") {
		t.Errorf("「没注册成服务」被报成了警告：%q\n"+
			"  容器/--no-service 的节点每次升级都会收到它，而它并不表示出了问题。", note)
	}
	if !strings.Contains(note, "没有把 Agent 注册成系统服务") {
		t.Errorf("提示里没把真实情况说清楚（应点明没有服务定义可改）：%q", note)
	}
}

// TestUpgradeRepairsEvenWhenAlreadyLatest 是**次序断言**（这轮最容易漏、也最难发现的一条）。
//
// 修复必须发生在 updater.Upgrade 之后、"已是最新"的早退**之前**：
// 老节点最常见的状态就是"二进制早升级过了，但 unit 与配置属主还是更早那次安装的"——
// 那台机器跑 `xb-probe-agent upgrade` 只会得到"已是最新，无需升级"。
// 若把修复放在"真的换了二进制"之后，最需要修的那台恰恰永远修不到，
// 而且命令还会告诉它"一切正常"。
func TestUpgradeRepairsEvenWhenAlreadyLatest(t *testing.T) {
	src, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatalf("读 cli.go 失败：%v", err)
	}
	body, err := funcBodyOf(string(src), "func cmdUpgrade(args []string, version string) int {")
	if err != nil {
		t.Fatalf("切出 cmdUpgrade 失败（结构变了，判据要跟着改）：%v", err)
	}

	const call = "repairNote := repairAfterUpgrade("
	iCall := strings.Index(body, call)
	if iCall < 0 {
		t.Fatalf("cmdUpgrade 里没有调用 repairAfterUpgrade：\n"+
			"  它就是「升级顺手修服务定义」这件事本身，缺了它这条路上什么都没修（%q）", call)
	}
	// ① 必须在二进制落地之后：反了就会按旧路径去改 unit（RewriteDefinition 收到的
	//    是替换前的可执行文件，改了等于没改）。
	iUpgrade := strings.Index(body, "updater.Upgrade(")
	if iUpgrade < 0 {
		t.Fatal("cmdUpgrade 里找不到 updater.Upgrade")
	}
	if iCall < iUpgrade {
		t.Error("修复发生在替换二进制之前：unit 会指向替换前的那个路径")
	}
	// ② 必须在 dry-run 的早退之后：dry-run 一个字节都不该写。
	iDry := strings.Index(body, `if *dry {`)
	iDryMsg := strings.Index(body, "未改动系统")
	if iDry < 0 || iDryMsg < 0 {
		t.Fatal("cmdUpgrade 里找不到 dry-run 分支：结构变了，判据要跟着改")
	}
	if iCall < iDry || iCall < iDryMsg {
		t.Error("修复发生在 dry-run 的早退之前：dry-run 会真的去改系统定义")
	}
	// ③ 必须在"已是最新"的早退之前：否则最需要修的节点永远走不到修复。
	//
	// 锚点必须带**缩进**：dry-run 块里也有一句 `if res.NoChange {`（缩进两层的
	// 那一处，只是决定打哪句提示）。只找 `if res.NoChange {` 会命中最前面那句，
	// 于是修复看起来"排在它后面"——判据本身报假红（这一版就是这么红的）。
	iNoChange := strings.Index(body, "\n\tif res.NoChange {")
	if iNoChange < 0 {
		t.Fatalf("cmdUpgrade 里找不到顶层的「已是最新」分支：判据要跟着改")
	}
	if iCall > iNoChange {
		t.Error("修复排在「已是最新」之后：\n" +
			"  只升级过二进制、unit 还是旧版的老节点跑 upgrade 会得到「已是最新，无需升级」，\n" +
			"  而它恰恰是唯一需要修的那台 —— 修复必须在那条早退之前。")
	}
	// ④ 两条出口都要把结论打出来：修了却不说，用户仍然要去猜服务为什么起不来。
	if n := strings.Count(body, "fmt.Println(repairNote)"); n != 2 {
		t.Errorf("只有 %d 条出口打印了修复结论（应为 2：「已是最新」与「已升级」各一条）：\n"+
			"  动手了却不告诉用户，等于把下一句「还是 permission denied」留给他自己查", n)
	}
	// ⑤ 开关要真的接上（画了开关不接线＝没有开关）。
	if !strings.Contains(body, `fs.Bool("no-service-repair"`) {
		t.Error("--no-service-repair 没有注册：容器/自定义 unit 的场景没有退路")
	}
	// 判据收在"冒号后面是什么"上：`*noRepair` 只要出现在**正确的那个参数位**才算接上。
	// 只查 Contains("∗noRepair") 会放过"传到了 customTarget 位"这种改法
	// ——那时它照样编译得过（变量被用到了），而开关的语义已经没了。
	if !strings.Contains(body, ", *noRepair)") {
		t.Error("--no-service-repair 没有接到「跳过修复」那个参数位上（传错位置＝永远不生效）")
	}
	if !strings.Contains(body, "isRoot()") {
		t.Error("修复没有做权限判断：非 root 时它会一路写完才发现失败，而提示里最好直接说清楚")
	}
	// ⑥ 只许修一次：dry-run 分支里"顺手也修一下"是最像好意的改法，而它会让
	//    一条"什么都不写"的命令真的去改系统定义。
	if n := strings.Count(body, "repairAfterUpgrade("); n != 1 {
		t.Errorf("cmdUpgrade 里 repairAfterUpgrade( 出现了 %d 次（应为 1）：\n"+
			"  dry-run 分支或别处再多调一次，就等于让「不写系统」的路径也去改服务定义", n)
	}
}

// funcBodyOf 切出一个顶层函数的函数体（从签名起到下一个行首的 `}`）。
//
// 为什么不能用 strings.Contains 找整份文件：同一份 cli.go 里 install/reinstall
// 也各自调用服务管理器，全文搜索会被"另一条路径的同名实现"满足。
func funcBodyOf(src, sig string) (string, error) {
	i := strings.Index(src, sig)
	if i < 0 {
		return "", errors.New("找不到函数签名：" + sig)
	}
	rest := src[i:]
	if e := strings.Index(rest, "\n}\n"); e >= 0 {
		return rest[:e], nil
	}
	return rest, nil
}
