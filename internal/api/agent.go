package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yourorg/probe/internal/auth"
	"github.com/yourorg/probe/internal/config"
	"github.com/yourorg/probe/internal/model"
	"github.com/yourorg/probe/internal/pipeline"
	"github.com/yourorg/probe/internal/state"
)

// Agent API（文档 7.1）：/api/agent/v1。
//
// 设计要点：Agent 只做外呼；Master 绝不下发命令。配置仅含声明式数据。

// handleEnroll 用一次性注册码换取节点专属令牌（3.4 / 13.4.7）。
func (h *Handler) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req model.EnrollRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}
	// 规范化后再校验与哈希：注册码固定形如 ABCD-EFGH-JKLM，
	// 用户可能抄成小写、带空格或漏掉分隔符，都必须能识别为同一个码。
	req.Code = auth.NormalizeEnrollCode(req.Code)
	if !auth.IsValidEnrollCode(req.Code) {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidCode, "注册码格式非法")
		return
	}

	now := time.Now()
	nodeID, ok, err := h.Repo.ConsumeEnrollCode(auth.EnrollCodeHash(req.Code), now.Unix())
	if err != nil {
		h.logErr("consume enroll code", err)
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "内部错误")
		return
	}
	if !ok {
		// 注册码无效/已使用/已过期——统一返回 401，不泄露具体原因。
		writeErr(w, http.StatusUnauthorized, model.ErrInvalidCode, "注册码无效或已过期")
		return
	}

	plain, hash, hint, err := auth.GenerateToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "内部错误")
		return
	}
	if _, err := h.Repo.CreateToken(nodeID, hash, hint, now.Unix()); err != nil {
		h.logErr("create token", err)
		writeErr(w, http.StatusInternalServerError, model.ErrInternal, "内部错误")
		return
	}
	_ = h.Repo.Audit(now.Unix(), fmt.Sprintf("node:%d", nodeID), "token.issue", hint, RealIP(r))
	_ = h.Repo.UpdateNodeIPs(nodeID, RealIP(r))

	// 记录运行环境（12.2 新增字段）。
	//
	// 这里**不能**用 UpdateNodeStatic：入网载荷只有 hostname/os/arch/virt 等环境信息，
	// 没有 cpu_cores/mem_total/disk_total/kernel/cpu_model，写下去就是把已入库的
	// 资源规格清零——现象是节点在线、卡片却显示 0 核 / 0 B 总量，
	// 且 Agent 不重启就再也补不回来（静态信息只在进程启动后第一次上报时携带）。
	// 资源规格统一由上报路径的完整 Static 负责。
	//
	// 字段仍要过一遍 sanitizeStatic：入网请求同样是**不可信输入**，
	// hostname/os/arch 都会进库、最终出现在前端。
	if st, err := h.Repo.GetNode(nodeID); err == nil && st != nil {
		env := &model.Static{
			Hostname: req.Hostname, OS: req.OS, Arch: req.Arch,
			Virt: req.Virt, AgentVersion: req.AgentVersion,
		}
		sanitizeStatic(env)
		if err := h.Repo.UpdateNodeEnv(nodeID, env.Hostname, env.OS, osFamilyOf(env.OS),
			env.Arch, env.Virt, req.ServiceManager, env.AgentVersion, req.CounterBits); err != nil {
			h.logErr("update node env", err)
		}
	}

	eff, version := h.effectiveConfig(nodeID)
	h.Registry.GetOrCreate(nodeID, "", "")
	// 必须通过 Update 写入：GetOrCreate 返回的是内部指针，直接赋值等于无锁写，
	// 会与上报路径的整体赋值构成数据竞争。
	_ = h.Registry.Update(nodeID, func(n *state.NodeRuntime) { n.ConfigVersion = version })

	writeJSON(w, http.StatusOK, model.EnrollResponse{
		NodeID: nodeID,
		Token:  plain, // 仅此一次返回明文令牌
		Config: agentConfigOf(version, eff),
	})
}

// handleReport 处理指标与探测结果上报（8.1）。
func (h *Handler) handleReport(w http.ResponseWriter, r *http.Request) {
	token, ok := auth.BearerToken(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, model.ErrInvalidToken, "缺少令牌")
		return
	}
	nodeID, err := h.Tokens.Authenticate(token, RealIP(r), time.Now())
	if err != nil {
		switch err.Error() {
		case "revoked":
			writeErr(w, http.StatusForbidden, model.ErrTokenRevoked, "令牌已吊销")
		case "archived":
			writeErr(w, http.StatusForbidden, model.ErrNodeArchived, "节点已归档")
		default:
			writeErr(w, http.StatusUnauthorized, model.ErrInvalidToken, "令牌无效")
		}
		return
	}

	var req model.ReportRequest
	if err := decodeJSON(r, &req); err != nil {
		if isTooLarge(err) {
			writeErr(w, http.StatusRequestEntityTooLarge, model.ErrPayloadTooBig, "请求体过大")
			return
		}
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "请求体解析失败")
		return
	}
	if len(req.Samples) > 100 {
		writeErr(w, http.StatusBadRequest, model.ErrInvalidParam, "单次样本数超过 100")
		return
	}

	now := time.Now()
	nowUnix := now.Unix()

	// 该节点**被允许探测**的目标集合（全局目标 + 本节点专属目标，且已启用）。
	//
	// 上报里的 target_id 是 Agent 自称的，必须回到 Master 下发的清单来核验：
	// 不核验的话，节点 A 只要上报一个属于节点 B 专属目标的 id，这条延迟就会以
	// A 的名义落进 latency_* 表，进而在 A 的卡片上渲染出"别人的专属目标"
	// （点阵串台）。store.LatencyBucketsForNode 的 JOIN 守卫是读侧的兜底，
	// 这里是写侧准入——两处都要有，任何一处单独存在都挡不住"库里已有脏行"的旧数据库。
	eff, version := h.effectiveConfig(nodeID)
	allowed := allowedTargetIDs(eff)

	if req.Static != nil {
		sanitizeStatic(req.Static)
		// 把 Agent 首次上报携带的完整静态信息落库（12.2）。
		//
		// 注册接口只写入了它当时知道的字段（主机名/系统/架构/virt 等），
		// CPU 核数、内存总量、磁盘总量这些要等第一次上报才拿得到。
		// 不落库的后果不只是"后台看不到"——Master 重启后内存运行态为空，
		// 而 Agent 只在进程启动后的第一次上报带 static，于是这些字段
		// 会一直空着，除非 Agent 也重启一次。详见 state.Registry.RestoreStatic。
		//
		// os_family / service_manager / counter_bits 来自注册接口，本次请求里
		// 没有，因此从库里已有的行取值，避免用空串把注册阶段的信息覆盖掉。
		bootTime := int64(0)
		for i := range req.Samples {
			if b := req.Samples[i].Metrics.BootTime; b != nil && *b > 0 {
				bootTime = *b
				break
			}
		}
		osFamily, serviceManager, counterBits := "", "", 0
		if cur, err := h.Repo.GetNode(nodeID); err == nil && cur != nil {
			osFamily, serviceManager, counterBits = cur.OSFamily, cur.ServiceManager, cur.CounterBits
		}
		if err := h.Repo.UpdateNodeStatic(nodeID, req.Static, osFamily, serviceManager, counterBits, bootTime); err != nil {
			h.logErr("persist static", err)
		}
	}

	// 确保运行态存在（首次上报前可能只有库记录）。
	h.Registry.GetOrCreate(nodeID, "", "")
	dropped := 0
	for i := range req.Samples {
		s := &req.Samples[i]
		if err := validateSample(s, nowUnix); err != nil {
			dropped++
			continue
		}
		sanitizeMetrics(&s.Metrics)
		s.Latency = sanitizeLatency(s.Latency)
		h.applySample(nodeID, s, now, req.Static, allowed)
	}
	if dropped > 0 {
		h.logErr("report", fmt.Errorf("节点 %d 丢弃 %d 条越界样本", nodeID, dropped))
	}
	_ = h.Repo.UpdateNodeIPs(nodeID, RealIP(r))

	// 状态由调度器按 4.1 的判定逻辑推进；这里只刷新 last_seen。
	_ = h.Registry.Update(nodeID, func(n *state.NodeRuntime) {
		n.LastSeen = nowUnix
		n.SuspectSince = 0
		if n.Status == model.StatusOffline {
			n.Status = model.StatusOnline
		}
	})

	// version 已在上面（算允许目标集合时）取到，这里直接复用，不再重复查库。
	_ = h.Registry.Update(nodeID, func(n *state.NodeRuntime) { n.ConfigVersion = version })

	writeJSON(w, http.StatusOK, model.ReportResponse{
		OK:            true,
		ServerTime:    nowUnix,
		ConfigVersion: version,
	})
}

// applySample 把一条样本写入内存运行态、聚合桶与流量跟踪器。
//
// allowed 是本节点被允许探测的目标 id 集合（见 allowedTargetIDs）；上报里不在
// 集合内的 target_id 一律丢弃，防止越权写入别人的专属目标延迟。
func (h *Handler) applySample(nodeID int64, s *model.Sample, now time.Time, static *model.Static, allowed map[int64]bool) {
	m := &s.Metrics

	/// 1) 更新运行态（最新状态只存内存，4.2 第 1 条）
	h.Registry.Update(nodeID, func(n *state.NodeRuntime) {
		n.LatestMetrics = *m
		if len(s.Latency) > 0 {
			n.LatestLatency = s.Latency
			// 记下"换了一轮"：调度器要靠它区分"新的一轮探测"与"同一轮被反复扫到"，
			// 否则"连续 3 轮失败"会在一次失败后就被满足（详见 NodeRuntime.LatencyRound）。
			n.LatencyRound++
		}
		if m.BootTime != nil && *m.BootTime > 0 {
			n.BootTime = *m.BootTime
		}
		if static != nil {
			n.OS = static.OS
			n.Arch = static.Arch
			n.Virt = static.Virt
			n.CPUCores = static.CPUCores
			n.MemTotal = static.MemTotal
			n.SwapTotal = static.SwapTotal
			n.DiskTotal = static.DiskTotal
		}
		// 2) 实时层环形缓冲（30 分钟）
		diskPct := 0
		if n.DiskTotal > 0 {
			diskPct = state.Percent(deref(m.DiskUsed), n.DiskTotal)
		}
		_ = diskPct
		n.Ring.Push(state.RingPoint{
			TS:     s.TS,
			CPU:    deref(m.CPU),
			MemPct: state.Percent(deref(m.MemUsed), n.MemTotal),
			RxBps:  deref(m.RxBps),
			TxBps:  deref(m.TxBps),
		})
	})

	// 3) 多分辨率聚合桶（写入时同步生成，无事后降采样）
	h.Multi.Add(nodeID, s.TS, pipeline.MetricsInput{
		CPU:      deref(m.CPU),
		Load1:    deref(m.Load1),
		Conns:    deref(m.Conns),
		MemUsed:  deref(m.MemUsed),
		SwapUsed: deref(m.SwapUsed),
		DiskUsed: deref(m.DiskUsed),
		RxBps:    deref(m.RxBps),
		TxBps:    deref(m.TxBps),
	})

	// 4) 延迟两层
	for _, l := range s.Latency {
		// 准入过滤：只接受本节点被下发的目标。`allowed` 为空（节点确实没有可探测
		// 目标）时全部丢弃——"没有专属目标的节点不该有点阵"本身就是期望行为。
		if !allowed[int64(l.TargetID)] {
			continue
		}
		row := pipeline.LatencyRow{
			TS: s.TS, TargetID: int64(l.TargetID),
			RTTAvg: l.RTTAvg, RTTMin: l.RTTMin, RTTMax: l.RTTMax, Loss: l.Loss,
		}
		h.Latency.Add(nodeID, row)
	}

	// 5) 流量按网卡计量（13.4.1）
	if len(m.NICs) > 0 && h.Tracker != nil {
		bits := 64
		if h.Registry != nil {
			if n, ok := h.Registry.Get(nodeID); ok && n.CounterBits != 0 {
				bits = n.CounterBits
			}
		}
		_, _, reset := h.Tracker.Add(nodeID, m.NICs, deref(m.BootTime), bits, now)
		if reset {
			h.logErr("traffic", fmt.Errorf("节点 %d 流量周期已重置", nodeID))
		}
	}
}

// handleGetConfig 返回该节点的有效配置，支持 ETag/304（3.1）。
func (h *Handler) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	token, ok := auth.BearerToken(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, model.ErrInvalidToken, "缺少令牌")
		return
	}
	nodeID, err := h.Tokens.Authenticate(token, RealIP(r), time.Now())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, model.ErrInvalidToken, "令牌无效")
		return
	}

	eff, version := h.effectiveConfig(nodeID)
	etag := `"` + version + `"`
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, version) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, agentConfigOf(version, eff))
}

// handleDeregister 本机卸载时的尽力通知（3.4）。失败不阻塞卸载。
func (h *Handler) handleDeregister(w http.ResponseWriter, r *http.Request) {
	token, ok := auth.BearerToken(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, model.ErrInvalidToken, "缺少令牌")
		return
	}
	nodeID, err := h.Tokens.Authenticate(token, RealIP(r), time.Now())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, model.ErrInvalidToken, "令牌无效")
		return
	}
	now := time.Now().Unix()
	_ = h.Repo.ArchiveNode(nodeID, now)
	if st, err := h.Repo.ListTokens(nodeID); err == nil {
		for _, t := range st {
			if !t.Revoked() {
				_ = h.Repo.RevokeToken(nodeID, t.ID, now)
			}
		}
	}
	h.Tokens.InvalidateNode(nodeID)
	h.Registry.Remove(nodeID)
	if h.Tracker != nil {
		h.Tracker.Drop(nodeID)
	}
	_ = h.Repo.Audit(now, fmt.Sprintf("node:%d", nodeID), "node.deregister", "", RealIP(r))
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// effectiveConfig 计算某节点的有效配置与摘要（13.4.3）。
//
// 生产实现建议增加按节点缓存，并在 settings/探测目标变更时失效；
// 此处直接计算，语义等价且始终正确。
func (h *Handler) effectiveConfig(nodeID int64) (config.EffectiveConfig, string) {
	settings, err := h.Repo.AllSettings()
	if err != nil {
		settings = map[string]string{}
	}
	targets, err := h.Repo.ListProbeTargets(nodeID)
	if err != nil {
		targets = nil
	}
	eff := config.BuildEffective(settings, targets)
	return eff, config.ComputeConfigVersion(eff)
}

// allowedTargetIDs 把有效配置里的目标清单折成 id 集合。
//
// config.BuildEffective → toTargets 已经滤掉未启用的目标，因此这里不再重复判 enabled。
// 节点的允许集合 = 全局目标 + 本节点专属目标（ListProbeTargets 的语义）。
func allowedTargetIDs(eff config.EffectiveConfig) map[int64]bool {
	set := make(map[int64]bool, len(eff.Targets))
	for _, t := range eff.Targets {
		set[int64(t.ID)] = true
	}
	return set
}

func agentConfigOf(version string, eff config.EffectiveConfig) model.AgentConfig {
	return model.AgentConfig{
		ConfigVersion:   version,
		ReportIntervalS: eff.ReportIntervalS,
		ProbeIntervalS:  eff.ProbeIntervalS,
		NICInclude:      eff.NICInclude,
		NICExclude:      eff.NICExclude,
		Targets:         eff.Targets,
	}
}

func osFamilyOf(os string) string {
	s := strings.ToLower(os)
	switch {
	case strings.Contains(s, "windows"):
		return "windows"
	case strings.Contains(s, "darwin"), strings.Contains(s, "mac"):
		return "darwin"
	case strings.Contains(s, "freebsd"):
		return "freebsd"
	default:
		return "linux"
	}
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

func isTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) && strings.Contains(err.Error(), "request body too large")
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func (h *Handler) logErr(where string, err error) {
	if h.Logger != nil {
		h.Logger.Error("api error", "where", where, "err", err.Error())
	}
}
