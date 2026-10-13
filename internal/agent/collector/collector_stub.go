//go:build !linux && !windows && !freebsd

package collector

import (
	"os"
	"runtime"

	"github.com/koyoca/xb-probe/internal/model"
)

// New 返回非 Linux 平台的采集器。
//
// 骨架说明（12.5）：各平台应提供独立实现，替换下面的 stub：
//   - macOS  ：sysctl（kern.boottime、hw.memsize、vm.page_free_count、hw.loadavg）与 getifaddrs
//   - FreeBSD：sysctl 与 devstat；负载可从 vm.loadavg 读取
//   - Windows：GetSystemTimes（CPU）、GlobalMemoryStatusEx（内存）、
//     GetIfTable2（网卡累计值）、GetDiskFreeSpaceEx（磁盘）、无 load average 上报空值
//
// 本 stub 只上报最小可用信息，避免在不支持的平台上产生错误数据。
func New(opts Options) Collector {
	return &stubCollector{opts: opts}
}

type stubCollector struct {
	opts Options
}

func (c *stubCollector) Static() (*model.Static, error) {
	hostname, _ := os.Hostname()
	cores := runtime.NumCPU()
	return &model.Static{
		Hostname:     hostname,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		CPUCores:     cores,
		AgentVersion: c.opts.AgentVersion,
		Virt:         "none",
	}, nil
}

func (c *stubCollector) Sample() (*model.Metrics, error) {
	// 未实现的平台返回空指标；Master 会以空值展示而不中断上报（12.5）。
	return &model.Metrics{}, nil
}
