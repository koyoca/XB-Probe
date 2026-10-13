package service

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDetectIsNeverNilAndNamesAreKnown 钉住"识别结果只能是文档 12.4 里的枚举值"。
//
// Master 会把 ServiceManager 原样存进节点的 static 信息并展示在后台；
// 冒出一个未登记的名字（比如手滑写成 "system-v"）会一路静默到 UI。
func TestDetectIsNeverNilAndNamesAreKnown(t *testing.T) {
	m := Detect()
	if m == nil {
		t.Fatal("Detect() 返回 nil，调用方会直接 panic")
	}
	known := map[string]bool{
		"systemd": true, "openrc": true, "procd": true,
		"sysv": true, "rcd": true, "windows": true, "none": true,
	}
	name := m.Name()
	if !known[name] {
		t.Fatalf("服务管理器标识 %q 不在文档枚举内", name)
	}
	if name == "none" && runtime.GOOS == "windows" {
		t.Errorf("Windows 上应识别为 windows，得到 none")
	}
}

// TestUnsupportedManagerFailsLoudly 未实现的平台必须报错而不是静默成功：
// 静默成功会让 install 打印"安装完成"却什么都没注册，留下半安装状态。
func TestUnsupportedManagerFailsLoudly(t *testing.T) {
	m := &unsupportedManager{name: "plan9", hint: "请手动配置 rio 启动脚本"}

	if m.Name() != "plan9" {
		t.Fatalf("Name 未透传：%q", m.Name())
	}
	ins := m.Install("/usr/local/bin/xb-probe-agent", "/etc/xb-probe-agent/agent.yaml", "probe")
	uni := m.Uninstall("/usr/local/bin/xb-probe-agent", "/etc/xb-probe-agent/agent.yaml")

	for what, err := range map[string]error{"Install": ins, "Uninstall": uni} {
		if err == nil {
			t.Fatalf("%s 在未实现平台上必须报错", what)
		}
		msg := err.Error()
		if !strings.Contains(msg, "plan9") {
			t.Errorf("%s 的错误信息未包含平台标识：%q", what, msg)
		}
		if !strings.Contains(msg, "请手动配置 rio 启动脚本") {
			t.Errorf("%s 的错误信息未带排查提示：%q", what, msg)
		}
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "present")

	if exists(file) {
		t.Fatal("不存在的文件被判为存在")
	}
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !exists(file) {
		t.Fatal("存在的文件被判为不存在")
	}
	if !exists(dir) {
		t.Fatal("存在的目录被判为不存在")
	}
}

func TestEnsureDirsCreatesParents(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(base, "xb-probe-agent", "nested", "agent.yaml")

	if err := EnsureDirs(cfgPath); err != nil {
		t.Fatalf("创建配置目录失败：%v", err)
	}
	info, err := os.Stat(filepath.Dir(cfgPath))
	if err != nil {
		t.Fatalf("目录未创建：%v", err)
	}
	if !info.IsDir() {
		t.Fatalf("创建出来的不是目录：%v", info.Mode())
	}
	// 幂等：服务管理器可能重复调用。
	if err := EnsureDirs(cfgPath); err != nil {
		t.Fatalf("重复调用应成功：%v", err)
	}
}

func TestRunSucceedsAndReportsFailureWithOutput(t *testing.T) {
	okName, okArgs := succeedingCommand()
	if err := run(okName, okArgs...); err != nil {
		t.Fatalf("成功命令却报错：%v", err)
	}

	failName, failArgs := failingCommand()
	err := run(failName, failArgs...)
	if err == nil {
		t.Fatal("失败命令却返回 nil")
	}
	msg := err.Error()
	// 服务管理命令（systemctl / sc / rc-service）失败时，唯一能定位问题的就是
	// 子进程输出与执行的完整命令行，所以两者都必须进错误信息。
	if !strings.Contains(msg, failName) {
		t.Errorf("错误信息缺少命令名：%q", msg)
	}
	if !strings.Contains(msg, "失败") {
		t.Errorf("错误信息缺少失败标记：%q", msg)
	}
}

func succeedingCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", "exit 0"}
	}
	return "true", nil
}

func failingCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", "exit 3"}
	}
	return "false", nil
}
