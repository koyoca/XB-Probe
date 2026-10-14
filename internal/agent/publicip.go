package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
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

// publicIPClient4 / publicIPClient6：同一句话分别强制从 IPv4 / IPv6 出去问。
// 节点经常是双栈的，但"我是谁"在两个协议族里是**两个答案**（v4 出口地址与
// v6 出口地址互不相关），而 Master 侧的地址栏也是两列。只探一次只能填一列，
// 另一列就永远"自动探测中"——这是真机上"IPv6 一直获取不到"的直接原因。
// 强制的办法是换 Dialer 的 network 参数：URL 本身不变（服务要双栈可解析，
// 单栈服务在对应那一族直接失败，拿不到就保持原值）。
func familyTransport(network string) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	d := &net.Dialer{Timeout: publicIPTimeout}
	t.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return d.DialContext(ctx, network, addr)
	}
	return t
}

var publicIPClient4 = &http.Client{Timeout: publicIPTimeout, Transport: familyTransport("tcp4")}
var publicIPClient6 = &http.Client{Timeout: publicIPTimeout, Transport: familyTransport("tcp6")}

// httpGetPublicIP 是本包唯一一处对 **Master 之外的主机**发起请求的地方。
//
// 抽成包级变量是为了让测试能替身它：不然这条链路只能靠"真的连一次公网服务"来验，
// 那样的测试在离线环境里要么永远红、要么被跳过 —— 而"被跳过的护栏等于没有"。
// 替身必须能证明自己生效（见 publicip_test.go 的哨兵断言）。
//
// network 是强制走的协议族："tcp4" / "tcp6"（familyTransport 里覆盖 DialContext
// 的 network 参数）。替身按它区分两个族。
var httpGetPublicIP = func(ctx context.Context, url string, network string) (string, error) {
	client := publicIPClient
	switch network {
	case "tcp4":
		client = publicIPClient4
	case "tcp6":
		client = publicIPClient6
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	// 有些服务按 UA 拦非浏览器请求（默认的 Go-http-client 常被挡），
	// 401/403 的错误页里没有地址，看起来就像"探测永远失败"。
	req.Header.Set("User-Agent", "xb-probe-agent")
	resp, err := client.Do(req)
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

// refreshPublicIP 对配置的 URL 各探一次 v4 与 v6，把结果写进运行态。
//
// 失败**不清空**上一次的值：探测服务偶尔抽风不该让地址栏变空；
// 地址真的变了，下一次成功探测自然会覆盖。
//
// 两个族各自独立：单栈节点/单栈解析的服务在对应那一族直接失败，
// 那一列保持原值，不影响另一列。拿到结果还要**按族核对**——tcp6 那次
// 返回了点分 IPv4（服务按 Host 头回答之类）就不算 v6 的答案，写进去
// 会被 Master 按含不含冒号分错列。
func (d *Daemon) refreshPublicIP(ctx context.Context, url string) {
	v4 := d.probeFamily(ctx, url, "tcp4")
	v6 := d.probeFamily(ctx, url, "tcp6")

	d.mu.Lock()
	if v4 != "" && !strings.Contains(v4, ":") && v4 != d.pubIP {
		d.pubIP = v4
		d.log.Info("已探测到本机公网地址", "family", "ipv4", "ip", v4)
	}
	if v6 != "" && strings.Contains(v6, ":") && v6 != d.pubIP6 {
		d.pubIP6 = v6
		d.log.Info("已探测到本机公网地址", "family", "ipv6", "ip", v6)
	}
	d.mu.Unlock()
}

// probeFamily 做一次指定协议族的探测并解析结果（取不到/失败返回空串）。
func (d *Daemon) probeFamily(ctx context.Context, url, network string) string {
	ip, err := httpGetPublicIP(ctx, url, network)
	if err != nil {
		// 单栈节点在"缺的那一族"失败是常态，不值得一条警告；这里保持安静，
		// 避免每 15 分钟刷一行"探测失败"把真正的网络问题淹掉。
		return ""
	}
	if ip == "" {
		// 拿到了响应但里面没有公网地址。这不是网络故障：要么那个服务从我们的
		// 角度看过来就是个私网/回环地址（说明它自己没在公网上），要么返回了错误页。
		d.log.Warn("公网地址探测没拿到公网地址（响应里没有可用的公网地址）", "url", url, "network", network)
		return ""
	}
	return ip
}

// observedPublicIP 读取最近一次探测到的公网 IPv4（未配置或尚未成功时为空串）。
func (d *Daemon) observedPublicIP() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pubIP
}

// observedPublicIP6 读取最近一次探测到的公网 IPv6（未配置或尚未成功时为空串）。
func (d *Daemon) observedPublicIP6() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pubIP6
}
