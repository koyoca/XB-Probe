package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/koyoca/xb-probe/internal/netutil"
)

// 公网地址自助探测（问题「节点的 IP 地址始终是 127.0.0.1」的 Agent 侧一半）。
//
// 为什么放在独立的 goroutine 而不是上报路径里：
//   - 上报间隔是 3 秒，网络往返最快的第三方面服务也要几十毫秒，串进去会让
//     每一次上报都被这条链路绑架；这个服务不可达时更糟 —— 上报会跟着一起变慢。
//   - 地址几乎不变，探测频率理应是分钟级而不是秒级。
//
// 探测结果只是**候选值**：Master 侧还要过 ip_auto（管理员是否手工填过）
// 与 netutil.IsPublicIP 两道闸才会写进节点地址（见 internal/api/agent.go）。

const (
	// publicIPTimeout 单次请求超时。取短值：卡住也不影响上报，只是地址停在旧值。
	publicIPTimeout = 8 * time.Second
	// publicIPMaxBody 最多读响应体的前若干字节。这类服务的响应都很短，
	// 用 LimitReader 兜住"服务返回了一个巨大页面"（或返回错误页）的情况。
	publicIPMaxBody = 512
	// publicIPIdleWait 未配置 public_ip_url 时重新检查配置的间隔。
	// 配置也可能来自 Reload（改了本地 agent.yaml），不能"启动时没配就再也不看"。
	publicIPIdleWait = 60 * time.Second
)

var publicIPClient = &http.Client{Timeout: publicIPTimeout}

// httpGetPublicIP 是本包唯一一处对 **Master 之外的主机**发起请求的地方。
//
// 抽成包级变量是为了让测试能替身它：不然这条链路只能靠"真的连一次公网服务"来验，
// 那样的测试在离线环境里要么永远红、要么被跳过 —— 而"被跳过的护栏等于没有"。
// 替身必须能证明自己生效（见 publicip_test.go 的哨兵断言）。
var httpGetPublicIP = func(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	// 有些服务按 UA 拦非浏览器请求（默认的 Go-http-client 常被挡），
	// 401/403 的错误页里没有地址，看起来就像"探测永远失败"。
	req.Header.Set("User-Agent", "xb-probe-agent")
	resp, err := publicIPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, publicIPMaxBody))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 解析交给 netutil：它同时负责"从任意格式里抠地址"与"必须是公网地址"。
	// 后者很关键 —— 服务经代理返回 127.0.0.1 时必须一个都不接受。
	return netutil.FirstPublicIP(string(body)), nil
}

// publicIPLoop 周期性探测本机公网地址；未配置时只定期重读配置。
//
// 结构与 probeLoop 一致：每轮在锁内重新取配置，改配置即时生效、不需要重启。
//
// ⚠ 退出条件必须是循环里的 `select ctx.Done()`，**不能**靠 sleepCtx 自己"感知取消"：
// sleepCtx 在 ctx 已取消时是立刻返回的，循环于是变成毫秒级的忙等（占满一个核，
// 而且永远不退出）。这个错误是护栏抓出来的 —— 见 publicip_test.go 里那段
// `cancel(); <-done`（旧写法会直接挂住，测试永远不返回）。
func (d *Daemon) publicIPLoop(ctx context.Context) {
	for {
		d.mu.Lock()
		url := d.cfg.PublicIPURL
		interval := d.cfg.PublicIPInterval()
		d.mu.Unlock()

		if url == "" {
			// 未配置就只定期重读配置，一次网络都不碰（默认关闭要真的是关闭）。
			// 等待期间间隔比探测间隔短得多，因为这里只是看一眼配置。
			interval = publicIPIdleWait
		} else {
			// 先取再等：配了地址的节点**第一轮就取**，不必先等一整个间隔
			// （否则刚装好的节点要等 15 分钟地址栏才变，用户以为没生效）。
			d.refreshPublicIP(ctx, url)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// refreshPublicIP 做一次探测并写入运行态，返回探测到的地址（未取到返回空串）。
//
// 失败**不清空**上一次的值：探测服务偶尔抽风不该让地址栏变空；
// 地址真的变了，下一次成功探测自然会覆盖。
func (d *Daemon) refreshPublicIP(ctx context.Context, url string) string {
	ip, err := httpGetPublicIP(ctx, url)
	if err != nil {
		d.log.Warn("公网地址探测失败", "url", url, "err", err.Error())
		return ""
	}
	if ip == "" {
		// 拿到了响应但里面没有公网地址。这不是网络故障：要么那个服务从我们的
		// 角度看过来就是个私网/回环地址（说明它自己没在公网上），要么返回了错误页。
		d.log.Warn("公网地址探测没拿到公网地址（响应里没有可用的公网地址）", "url", url)
		return ""
	}
	d.mu.Lock()
	changed := d.pubIP != ip
	d.pubIP = ip
	d.mu.Unlock()
	if changed {
		d.log.Info("已探测到本机公网地址", "ip", ip)
	}
	return ip
}

// observedPublicIP 读取最近一次探测到的公网地址（未配置或尚未成功时为空串）。
func (d *Daemon) observedPublicIP() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pubIP
}
