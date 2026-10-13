// Package agent 实现 Agent 常驻守护进程的主循环（文档 3.1 / 8.1）。
//
// Agent 是无状态常驻守护进程，只做“本地采集、主动外呼”：
//   - 不做远程自更新，不响应任何远程指令（9.5）。
//   - 上报失败指数退避（上限 60 秒）并在本地缓存最近若干条，恢复后按 ts 升序补报（13.4.2）。
//   - 配置同步纯被动：上报响应带回 config_version，不一致时才 GET /config（带 ETag）。
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/yourorg/probe/internal/agent/agentconfig"
	"github.com/yourorg/probe/internal/agent/client"
	"github.com/yourorg/probe/internal/agent/collector"
	"github.com/yourorg/probe/internal/agent/prober"
	"github.com/yourorg/probe/internal/model"
)

// 退避与轮询参数（3.1 / 7.1）。
const (
	minBackoff       = 1 * time.Second
	maxBackoff       = 60 * time.Second
	unauthorizedWait = 10 * time.Minute // 401/403 时退避至 10 分钟，且不自毁

	// 兜底间隔：与 Master 侧 config.BuildEffective 的默认值保持一致（3s / 30s）。
	// 配置被写成 0 或负数时（手改 agent.yaml、Master 下发异常）用它顶上，
	// 否则 time.NewTicker(0) 会 panic，整个 Agent 直接挂掉。
	defaultReportIntervalS = 3
	defaultProbeIntervalS  = 30
)

// intervalDuration 把秒数换成 time.Duration；非正值退回 fallback，避免 ticker panic。
func intervalDuration(seconds, fallback int) time.Duration {
	if seconds <= 0 {
		seconds = fallback
	}
	return time.Duration(seconds) * time.Second
}

// Daemon 是 Agent 主循环。
type Daemon struct {
	cfgPath string
	cfg     *agentconfig.Config
	cli     *client.Client
	coll    collector.Collector
	probe   *prober.Prober
	log     *slog.Logger

	mu          sync.Mutex
	buffer      []model.Sample // 待补报样本（有界，满时丢最旧）
	latest      []model.LatencySample
	curEtcd     string
	version     string
	staticDirty bool
}

// New 创建守护进程。
func New(cfgPath string, cfg *agentconfig.Config, cli *client.Client, version string) *Daemon {
	d := &Daemon{
		cfgPath: cfgPath,
		cfg:     cfg,
		cli:     cli,
		coll: collector.New(collector.Options{
			NICInclude:   cfg.NICInclude,
			NICExclude:   cfg.NICExclude,
			DiskPaths:    cfg.DiskPaths,
			AgentVersion: version,
		}),
		probe:       prober.New(cfg.AllowPrivateTargets),
		log:         slog.Default(),
		version:     version,
		staticDirty: true,
	}
	return d
}

// Run 启动主循环，直到 ctx 取消。
func (d *Daemon) Run(ctx context.Context) error {
	go d.probeLoop(ctx)

	// 首次上报：携带 static（3.1）
	backoff := minBackoff
	reportTicker := time.NewTicker(d.reportInterval())
	defer reportTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-reportTicker.C:
		}

		wait, err := d.tick(ctx)
		if err != nil {
			// 指数退避（上限 60 秒）；401/403 使用更长的固定间隔且不自毁（7.1）。
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && apiErr.IsUnauthorized() {
				d.log.Error("令牌无效或已吊销，退避重试（不会自毁）",
					"code", apiErr.Code, "retry_in", unauthorizedWait.String())
				sleepCtx(ctx, unauthorizedWait)
				reportTicker.Reset(d.reportInterval())
				continue
			}
			d.log.Warn("上报失败，稍后重试", "err", err.Error(), "backoff", backoff.String())
			sleepCtx(ctx, backoff)
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			reportTicker.Reset(d.reportInterval())
			continue
		}

		backoff = minBackoff
		if wait > 0 {
			reportTicker.Reset(wait)
		} else {
			reportTicker.Reset(d.reportInterval())
		}
	}
}

// reportInterval 在锁内读取上报间隔。
//
// 间隔是**被动配置**（Master 通过 /config 下发），随时可能变；旧实现直接读
// d.cfg.ReportIntervalS，既与 syncConfig 的写入构成数据竞争，又让"后台把上报间隔
// 从 3 秒改成 10 秒"必须重启 Agent 才生效——表现就是"后台设置没反应"。
func (d *Daemon) reportInterval() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return intervalDuration(d.cfg.ReportIntervalS, defaultReportIntervalS)
}

// configVersion 在锁内读取本地已知的 config_version。
func (d *Daemon) configVersion() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg.ConfigVersion
}

// tick 采集一次并上报；返回下次上报前需要等待的时长（配置变化后可调整）。
func (d *Daemon) tick(ctx context.Context) (time.Duration, error) {
	// 采集器实例会被 syncConfig / Reload 整体替换（网卡与磁盘过滤项只在
	// collector.New 时读取），所以必须**在锁内**取一份再用：无锁读既构成数据竞争，
	// 也可能拿到"新配置 + 旧采集器"，于是后台改的网卡过滤永远不生效。
	d.mu.Lock()
	coll := d.coll
	d.mu.Unlock()

	metrics, err := coll.Sample()
	if err != nil {
		// 采集失败不中断：上报空指标，Master 记为缺失而不误报离线（12.5）。
		d.log.Warn("采集失败", "err", err.Error())
		metrics = &model.Metrics{}
	}

	sample := model.Sample{
		TS:      time.Now().Unix(),
		Metrics: *metrics,
		Latency: d.takeLatency(),
	}

	req := model.ReportRequest{AgentVersion: d.version}
	d.mu.Lock()
	req.Samples = append(req.Samples, d.buffer...)
	d.buffer = nil
	req.Samples = append(req.Samples, sample)
	if d.staticDirty {
		if st, err := coll.Static(); err == nil {
			sanitizeStatic(st)
			req.Static = st
		}
	}
	d.mu.Unlock()

	if len(req.Samples) > 100 {
		// 单次 samples ≤ 100（7.0）：只上报最近 100 条，其余保留待下次补报。
		keep := req.Samples[:len(req.Samples)-100]
		req.Samples = req.Samples[len(req.Samples)-100:]
		d.mu.Lock()
		d.buffer = append(keep, d.buffer...)
		d.mu.Unlock()
	}

	resp, err := d.cli.Report(ctx, req)
	if err != nil {
		d.requeue(req.Samples)
		return 0, err
	}
	if req.Static != nil {
		d.mu.Lock()
		d.staticDirty = false
		d.mu.Unlock()
	}

	// 配置同步：仅当 config_version 变化时才拉取（8.1 第 3 步）
	if resp.ConfigVersion != "" && resp.ConfigVersion != d.configVersion() {
		if err := d.syncConfig(ctx, resp.ConfigVersion); err != nil {
			d.log.Warn("拉取配置失败", "err", err.Error())
		}
	}
	return 0, nil
}

// requeue 上报失败时把样本放回缓冲；满则丢弃最旧（3.1 / 13.4.8）。
func (d *Daemon) requeue(samples []model.Sample) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.buffer = append(d.buffer, samples...)
	if len(d.buffer) > d.cfg.BufferSamples {
		drop := len(d.buffer) - d.cfg.BufferSamples
		d.buffer = d.buffer[drop:]
		d.log.Warn("本地缓冲已满，丢弃最旧样本", "dropped", drop)
	}
}

// syncConfig 拉取并应用远端配置（带 ETag，未变返回 304）。
func (d *Daemon) syncConfig(ctx context.Context, version string) error {
	remote, notModified, err := d.cli.GetConfig(ctx, d.curEtcd)
	if err != nil {
		return err
	}
	if notModified {
		return nil
	}
	d.mu.Lock()
	d.curEtcd = remote.ConfigVersion
	d.cfg.ApplyServerConfig(*remote)
	// 探测器要跟着 AllowPrivateTargets 一起换；必须与 cfg 在同一个锁内替换，
	// 否则 probeLoop 可能读到"新目标清单 + 旧探测器"，用错拨号策略探测内网目标。
	d.probe = prober.New(d.cfg.AllowPrivateTargets)
	// 采集器同理：网卡/磁盘过滤项只在 collector.New 时被读取，不重建的话
	// "后台改了网卡过滤"要重启 Agent 才生效（与间隔那个坑同一类）。
	d.coll = collector.New(collector.Options{
		NICInclude:   d.cfg.NICInclude,
		NICExclude:   d.cfg.NICExclude,
		DiskPaths:    d.cfg.DiskPaths,
		AgentVersion: d.version,
	})
	// 落盘读的也是同一个指针，一并在锁内取出来，避免与 Reload 的 `d.cfg = cfg` 竞争。
	cfg := d.cfg
	d.mu.Unlock()

	if err := agentconfig.Save(d.cfgPath, cfg); err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}
	d.log.Info("配置已更新", "config_version", remote.ConfigVersion,
		"targets", len(remote.Targets), "report_interval_s", remote.ReportIntervalS,
		"probe_interval_s", remote.ProbeIntervalS)
	return nil
}

// probeLoop 按配置周期异步探测目标，结果缓存于内存等待下次上报（3.1）。
//
// 每一轮开始时都在**锁内**重新取「探测器 + 目标清单 + 探测间隔」，原因有三：
//
//  1. 目标清单是后台配置的，改了必须当轮生效。**没配目标就不探测**——
//     旧实现只在启动时建 ticker，间隔改了要重启进程，看起来就是"后台设置不生效"。
//  2. d.probe 会被 syncConfig / Reload（换 AllowPrivateTargets）整体替换，
//     不锁就读会与写方构成数据竞争，读到半新半旧的实例还可能用错拨号策略。
//  3. 间隔兜底：0/负数会让 time.NewTicker panic，用 fallback 顶上。
func (d *Daemon) probeLoop(ctx context.Context) {
	for {
		d.mu.Lock()
		interval := intervalDuration(d.cfg.ProbeIntervalS, defaultProbeIntervalS)
		d.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}

		d.mu.Lock()
		probe := d.probe
		targets := append([]model.Target(nil), d.cfg.Targets...)
		d.mu.Unlock()

		if len(targets) == 0 || probe == nil {
			// 后台没有启用任何探测目标时不做无意义的空转，也不上报空延迟。
			continue
		}
		if len(targets) > prober.MaxTargetsPerNode {
			d.log.Warn("目标数量超过上限，仅探测前 N 个", "limit", prober.MaxTargetsPerNode)
			targets = targets[:prober.MaxTargetsPerNode]
		}

		results := make([]model.LatencySample, 0, len(targets))
		for _, t := range targets {
			results = append(results, probe.Probe(ctx, t))
		}
		d.mu.Lock()
		d.latest = results
		d.mu.Unlock()
	}
}

// takeLatency 取出并清空最近一轮探测结果。
func (d *Daemon) takeLatency() []model.LatencySample {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.latest
	d.latest = nil
	return out
}

// Reload 重新读取配置（配置变化后调用）。
func (d *Daemon) Reload() error {
	cfg, err := agentconfig.Load(d.cfgPath)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.cfg = cfg
	d.probe = prober.New(cfg.AllowPrivateTargets)
	// 采集器也要重建：磁盘路径与网卡过滤项是"建实例时读取"的，不重建就不生效。
	d.coll = collector.New(collector.Options{
		NICInclude:   cfg.NICInclude,
		NICExclude:   cfg.NICExclude,
		DiskPaths:    cfg.DiskPaths,
		AgentVersion: d.version,
	})
	d.mu.Unlock()
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// sanitizeStatic 在 Agent 侧也做一次清洗，避免把非法字符送到 Master（13.4.4）。
func sanitizeStatic(s *model.Static) {
	s.Hostname = sanitizeToken(s.Hostname, 63)
	s.OS = sanitizePlain(s.OS, 64)
	s.Kernel = sanitizePlain(s.Kernel, 64)
	s.CPUModel = sanitizePlain(s.CPUModel, 64)
	s.Arch = sanitizeToken(s.Arch, 16)
	if s.CPUCores < 0 {
		s.CPUCores = 0
	}
	if s.MemTotal < 0 {
		s.MemTotal = 0
	}
	if s.SwapTotal < 0 {
		s.SwapTotal = 0
	}
	if s.DiskTotal < 0 {
		s.DiskTotal = 0
	}
}

func sanitizeToken(s string, max int) string {
	if len(s) > max {
		s = s[:max]
	}
	return s
}

func sanitizePlain(s string, max int) string {
	if len(s) > max {
		s = s[:max]
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '<' || r == '>' || r < 0x20 {
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// NumCPU 便于日志输出环境信息。
func (d *Daemon) NumCPU() int { return runtime.NumCPU() }
