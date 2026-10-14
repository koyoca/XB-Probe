package main

import (
	"os"
	"strings"
	"testing"
)

// 启动顺序里有一条**硬约束**：ACME 的挑战端口必须在任何证书申请之前就绪。
//
// 为什么用源码断言而不是跑一遍：这条约束是"代码的排布"，只有 main.go 里
// 那几十行的先后关系能表达它，而 main() 又没法在单测里整体跑起来（它要开库、
// 绑端口、装信号）。源码断言虽脆，但能把"有人把申请挪回监听之前"这种改法
// 当场拦下来 —— 这个错犯过一次，代价是用户在真机上折腾了好几轮：
//
//	他在一台 80 端口完全空闲、防火墙全开的机器上选 http-01，日志里
//	"申请期间 80 一直没起来"，于是先去查防火墙、查限流，全都不是。
//	真正的原因有两层：① 申请被放在 tlsconf.Setup 里，而 Setup 跑在
//	main 启动 80 监听**之前**——申请时挑战端口根本没人听，CA 一定验不过；
//	② 服务以 probe 用户运行，绑 80（特权端口）需要 CAP_NET_BIND_SERVICE，
//	而当时的 systemd 单元没给（② 由 scripts/deploy-master.sh 的单测守着）。
func TestChallengeListenerStartsBeforeAnyCertificateObtain(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读取 main.go 失败：%v", err)
	}
	src := string(raw)

	// 挑战端口的**同步**绑定。同步是关键：绑不上的原因要当场报出来，
	// 丢进 goroutine 只会被随后那次申请失败淹掉。
	ln := strings.Index(src, `net.Listen("tcp", addr)`)
	if ln < 0 {
		t.Fatal("main.go 里找不到 ACME 明文端口的同步 net.Listen —— " +
			"不能同步绑定的话，绑定失败（端口被占 / 权限不足）会被静默吞掉")
	}
	obtain := strings.Index(src, "tlsProvider.ObtainSyncOnStartup(ctx)")
	if obtain < 0 {
		t.Fatal("main.go 里没有调用 ObtainSyncOnStartup —— 部署期拿不到一个确定的申请结果")
	}
	run := strings.Index(src, "tlsProvider.Run(ctx)")
	if run < 0 {
		t.Fatal("main.go 里没有调用 tlsProvider.Run —— 后台续期循环不会启动")
	}

	if ln > obtain {
		t.Error("ACME 挑战端口的监听排在证书申请**之后**：" +
			"申请期间挑战端口没在听，CA 一定验不过（真机上「80 完全空闲、" +
			"防火墙全开却申请失败」就是这个顺序造成的）")
	}
	if ln > run {
		t.Error("ACME 挑战端口的监听排在续期循环启动**之后**：" +
			"Run 启动时会立刻 tick 一次、可能直接发起申请，同样会撞上「端口还没听」")
	}
}
