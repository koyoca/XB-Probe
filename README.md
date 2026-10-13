# XB-Probe（自用轻量级服务器探针）

面向个人多节点 VPS 的极轻量、高安全、可插拔监控与资产管理系统。

一个 **Master** 二进制（内嵌 SQLite、管理后台、公开看板与 IANA 时区库，零外部依赖）+ 若干 **Agent**
（纯外联上报，不监听任何端口）即可管理任意多台节点。Master 自带 HTTPS，也可以只做前置反代后面的应用；
公开看板与管理后台既能两个域名分离、也能同站，**运行时切换、无需重启**。

状态：`go vet` 无告警 · `go test ./...` 25 个包全绿 · 端到端冒烟 311 项通过（0 失败 / 1 跳过）·
Agent 13 个平台与 Master 的发布产物均由 CI 构建。

## 特性

- **零入站攻击面**：Agent 纯外联，不监听端口，防火墙可全阻断入站；Master 从不主动连接 Agent，
  系统**不存在任何远程控制通道**（含远程卸载与远程命令）。
- **展示与管理物理隔离**：公开看板走独立 DTO 白名单（模型里根本没有 IP / 令牌 / 价格字段），
  可分两个域名，也可同站。
- **单文件、零依赖**：全 Go，免 CGO 的内嵌 SQLite（`modernc.org/sqlite`）；前端只用原生
  HTML/CSS/JS（不引 CDN），因此 CSP 可以钉死在 `default-src 'self'`。
- **自带 HTTPS**：内置零依赖 ACME v2 客户端（**域名证书 + RFC 8738 IP 证书**、
  `http-01` / `tls-alpn-01` / `dns-01`（Cloudflare，支持通配符）），也可以使用已有证书、自签证书，
  或交给前置 Caddy/Nginx。
- **多平台采集**：Linux（`/proc` + `/sys`，含 cgroup v1/v2 与容器识别）、Windows（纯 Win32 API，
  无外部命令）、FreeBSD（`sysctl` + `NET_RT_IFLIST`）。**macOS 明确不支持**。
- **告警与资产**：离线 / 资源阈值 / 「探测连续失败」三类告警（按 `(节点, 类型)` 独立冷却，
  恢复通知不受冷却限制），Telegram 与 SMTP 渠道；计费周期、月费折算、流量配额与续期提醒。

## 部署（Linux）

两种方式，选一种即可。**目标机器是 1 核小机器就用第一种**——它不需要 Go、不需要 git、不编译。

### 方式 A：预编译二进制（推荐）

```bash
sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master-bin.sh)"
```

下载对应架构的 `probe-master`、用 `SHA256SUMS.master` 校验、真跑一次 `version` 自检，然后交给部署脚本
完成配置、初始化管理员、注册服务并做健康检查。机器上只需要 `curl`（或 `wget`）。

```bash
# 指定版本 / 换下载加速前缀 / 只看不装（下载并校验，不改动系统）
sudo sh install-master-bin.sh --version v0.1.0
sudo sh install-master-bin.sh --mirror https://ghfast.top/https://github.com
sh install-master-bin.sh --dry-run --version v0.1.0
```

只发布 `linux/amd64` 与 `linux/arm64`。其它架构（含 FreeBSD）请用方式 B，它会现场编译。

部署脚本取自 `main`（可用 `--script-ref` 换成某个标签）：**二进制按版本钉死、脚本独立演进**，
这样修一句提问或补一条诊断不必重发一版。

### 方式 B：取源码现场编译

在**全新机器**上一条命令完成：检查/安装依赖（git、Go）→ `git clone` 拉取源码 → 编译 →
交互式配置 → 注册 systemd 服务并启动 → 健康检查。

```bash
sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master.sh)"
```

若机器上已有 Go ≥ 1.26 就直接用，否则脚本会下载官方 Go；支持 systemd / OpenRC / procd / SysV 自动探测。

### 两种方式共同的注意事项

脚本会依次问你：**对外访问方式（HTTPS 由谁办证书 / 纯 HTTP）**、域名与邮箱、**公开站与管理端
各监听哪个端口**、公开站与管理端地址、管理员账号与口令，**每一步直接回车取方括号里的默认值**。
最后会把将要做的事列一遍让你确认。

第一问的五个选项，说的是"**对外看起来是什么**"，不是"证书从哪来"：

| 选项 | 对外 | 证书谁办 |
|---|---|---|
| `1) auto`（默认） | HTTPS | 脚本自动装好 Caddy，证书由 Caddy 申请与续期 |
| `2) acme` | HTTPS | `probe-master` 自己申请（内置 ACME 客户端） |
| `3) selfsigned` | HTTPS（浏览器会提示不受信任） | 自签，适合只能用纯 IP 访问的场景 |
| `4) files` | HTTPS | 用你已有的证书与私钥 |
| `5) plain` | 纯 HTTP | ——不涉及，脚本**不装任何反代** |

> 为什么把 `none` 这个标签去掉了：早先这一问叫"TLS 方式"，选项 1 的标签是 `none`，
> 说明里却写着"自动装好 Caddy 申请证书"——字面意思是"不要证书"，与说明正好相反。
> 现在 HTTPS 与纯 HTTP 各占一项，不再自相矛盾。<br>
> 想显式表达同一件事：`--tls-mode none --tls-terminator caddy` 等价于选项 1，
> `--tls-mode plain` 等价于选项 5。

也就是说：**不需要事后手改 `master.yaml`**。想先看看会写成什么样，用部署脚本自己的预览：

```bash
sudo sh /path/to/deploy-master.sh --bin ./probe-master --dry-run    # 打印将要写入的 master.yaml
```

全程幂等——**再执行一次就是升级**（不会重建数据目录，也不会覆盖已有 `master.yaml`）。
参数给全了就不问（`--yes` 等价于"全部别问"），所以同样的脚本也适合自动化。

跑完会打印一段**部署完成**汇总（公开看板地址、管理后台地址、传输安全、站点形态，以及接下来该做什么）；
选 `acme` 时还会等证书申请出结果，并把**成功 / 失败 / 仍在申请**逐条报出来，失败时按挑战方式给出排查方向。
同样的信息随时可查：

```bash
probe-master tls status --config /etc/probe-master/master.yaml            # 等待并报告（rc 0=就绪 1=失败 2=进行中）
probe-master tls status --config /etc/probe-master/master.yaml --json     # 给监控/脚本用
```

> **为什么不是 `curl … | sudo sh`**：管道会把 stdin 占掉，脚本里的 `read` 立刻拿到 EOF，
> 于是所有交互项都静默取默认值（甚至空口令）——这正是"一键装完却登不进后台"的常见原因。
> 上面的 `sh -c "$(...)"` 保留了终端 stdin。先看再跑（推荐）或没有 bash 时：
>
> ```bash
> curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master.sh -o install-master.sh
> less install-master.sh && sudo sh install-master.sh
> ```

**要在一行式里带参数，必须先插一个占位名**——`sh -c` 之后的第一个词会成为 `$0`，不会被当作参数：

```bash
sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master-bin.sh)" xb \
  --no-service --dir ./probe-master --data ./probe-master/data --bin-dir ./probe-master/bin
```

若写成 `…"$(curl …)" --yes`，`--yes` 会被拼进脚本正文而不是当作参数，表现为莫名其妙的"需要 root"报错。

容器或没有 systemd、没有 root 时可以完全不用 root（两个脚本都支持 `--no-service`）：

```bash
sh install-master-bin.sh --no-service --dir ./probe-master --data ./probe-master/data --bin-dir ./probe-master/bin
```

完全非交互（CI / 批量部署）：

```bash
sudo sh install-master-bin.sh --yes \
  --public-origin https://status.example.com \
  --tls-mode acme --tls-domain status.example.com --acme-email ops@example.com
```

`--help` 看全量参数：`--version` / `--repo` / `--mirror` / `--arch` / `--script-ref` / `--tls-mode` /
`--tls-terminator` / `--proxy-domain` / `--public-origin` / `--admin-origin` / `--admin-user` /
`--public-listen` / `--admin-listen` / `--dir` / `--data` / `--bin-dir` / `--dist-base` / `--no-service` /
`--force-config` / `--dry-run` / `--yes` / `--uninstall` / `--keep-data`
（方式 B 另有 `--ref` / `--src` / `--go-version` / `--goproxy` / `--no-deps`）。

脚本的**第一问就是"安装还是卸载"**，直接跑（没带 `--uninstall`）也能从这里进卸载，不需要先找到
`deploy-master.sh`。答过的内容会记在 `<配置目录>/.answers`（权限 0600），下次运行时自动作为
方括号里的默认值回填——想重来不用一次次重敲，想彻底重来加 `PROBE_FRESH_ANSWERS=1`。
**口令类从不下盘**（`ADMIN_PASSWORD` / `ACME_DNS_TOKEN` 一律不写进该文件）。

> **关于版本错配**：免编译入口的**二进制来自 Release 标签**，而**部署脚本来自 `main`**（见下）。
> 两者版本不同步时，脚本可能写出二进制还不认识的配置键——而 `LoadMaster` 是**严格解析**
> （未知键直接拒绝启动），后果是服务起不来。
> 所以脚本在写这类新键之前，会**先拿一份最小的探针配置去问二进制本人**认不认识，
> 不认识就不写（并退回轮询等待证书的旧行为），而不是写完才发现。
> 想彻底避免这类问题，用下面「方式 B（从源码构建）」——二进制与脚本来自同一次检出。

### 交给 Caddy 代管（一个端口也能直接上 https）

`probe-master` 可以只监听任意端口（甚至纯回环），由部署脚本**自动装好 Caddy、生成 Caddyfile、
注册 `caddy.service`**——证书申请与续期都交给 Caddy，不用自己再装 Caddy/Nginx 手工配置：

```bash
sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master-bin.sh)" xb \
  --tls-terminator caddy --proxy-domain status.example.com \
  --public-listen 127.0.0.1:8080 --admin-listen 127.0.0.1:8081 \
  --public-origin https://status.example.com
```

此时 Master 侧自动改成 `tls.mode: none`（只监听回环），反代来源网段自动填进 `trusted_proxies`。
`--tls-terminator master`（默认）表示仍由 Master 自己终结 TLS。

> **后台走哪条路**：公开站与管理端默认是**两个监听地址**（`0.0.0.0:8080` 与 `127.0.0.1:8081`），
> 所以 Caddyfile 里会把 `/admin/`、`/admin/*`、`/api/admin/*` **单独反代到管理端那个地址**，
> 其余走公开站。少这一条的话，用 `https://<域名>/` 一切正常、而 `https://<域名>/admin/`
> 会返回 `404 page not found`——因为请求全被转给了公开站，那棵树里没有后台路由。
> 两个监听**相同**时（同源部署）则不会多这一层分流。

脚本在收尾会**真的请求一次 `https://<域名>`** 来确认 443 与证书都就绪，而不是打印一句
"稍后验证"——失败时给出可直接照做的诊断并回滚。这一点是刻意的：

> 本机若**已经装过 Caddy**（发行版包、x-ui 面板、更早的 nginx 迁移），`/etc/caddy/Caddyfile`
> 往往是别人留下的。若因为"文件存在"就原样保留，那份配置里通常没有你的域名——结果是
> Caddy 只开 80、**不开 443、也拿不到证书**，而脚本却报告"部署完成"。
> 所以脚本的判据是「这份文件里有没有本次要用的域名」：没有就**把站点块追加进去**
> （原文件备份为 `Caddyfile.bak`），不是盲目覆盖、也不是盲目跳过。

若证书申请失败，脚本会**先申请、边申请边反馈，失败即中止安装并回滚本次新建的一切**
（systemd 单元、二进制、配置、数据、系统用户），不会留下一个半成品。

## 部署（Windows）

以**管理员** PowerShell 执行：

```powershell
$f="$env:TEMP\xb-install.ps1"; irm https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master.ps1 -OutFile $f; powershell -ExecutionPolicy Bypass -File $f
```

> 脚本带 UTF-8 BOM（Windows PowerShell 5.1 只看 BOM 判断编码，缺了会把中文注释按 GBK 解码、
> 整份脚本语法错误），所以**必须落盘后运行**，不要用 `irm … | iex`。

参数与 Linux 版一致（`-Repo` `-Ref` `-Src` `-GoVersion` `-Dir` `-Data` `-PublicListen`
`-AdminListen` `-PublicOrigin` `-AdminOrigin` `-AdminUser` `-DistBase` `-TlsMode` `-TlsDomain`
`-AcmeEmail` `-NoStartupTask` `-ForceConfig` `-Yes` `-DryRun` `-Uninstall` `-KeepData` …）。

Windows 上的交互输入由 PowerShell 的 `Read-Host` 负责（退格、方向键、粘贴都是原生的），
口令一律以 `-AsSecureString` 读入、不回显。

Windows 上 Master 没有内置服务封装（Agent 才有），用"开机时以最高权限运行"的计划任务实现自启并配失败重启；
数据目录用 ACL 收成"仅 Administrators 与 SYSTEM"（等价 Linux 的 0700）。

## 部署后必做

1. 浏览器打开 `https://<你的域名>/admin/` 登录（本机纯 HTTP 也能登录，但会话是明文传输的）。
2. **启用 TOTP 并保存恢复码**（管理端 → 系统 → 安全）——恢复码是"手机丢了"的唯一退路。
3. 确认 TLS 生效；前置反代时把代理网段填进 `trusted_proxies`，否则会出现"登录后立刻掉线"。
4. **离线另存一份 `master.key`**：它加密告警渠道凭证（TG Bot Token、SMTP 口令），丢了不可解密、
   泄漏等于凭证泄漏。它由 `init-admin` 自动创建，控制台会打印路径。

## TLS 与证书（四选一）

Master 自带 HTTPS，不强制依赖前置代理。`tls.mode`：

| mode         | 适用场景                             | 说明                                                                    |
| ------------ | -------------------------------- | --------------------------------------------------------------------- |
| `none`       | 前置 Caddy/Nginx 卸载 TLS；本机/内网调试    | 需配 `trusted_proxies` 才认 `X-Forwarded-Proto`；用部署脚本的 `--tls-terminator caddy` 会**自动装好 Caddy 与反代** |
| `files`      | 已有证书：商业证书、企业内网 CA、别处签发的          | 读 `cert_file` / `key_file`，**变更热加载**（30 秒节流，读失败保留旧证书）                   |
| `selfsigned` | 内网 / 私有 IP 上想要"IP 证书"            | 本进程生成并落盘，SAN 覆盖 `hosts` 里的域名与 **IP**；Agent 侧用 SPKI 指纹固定即可安全使用        |
| `acme`       | 有公网 IP、想自动申请与续期                  | 内置**零依赖 ACME v2 客户端**，域名证书 + **RFC 8738 IP 证书**，`http-01` / `tls-alpn-01` / `dns-01`；部署时 `acme.sync_obtain: true` 让首张证书**前台申请**、拿到确定结果 |

```yaml
tls:
  mode: acme
  acme:
    email: "ops@example.com"
    ca: "letsencrypt-staging"         # 先拿 staging 试，别把生产配额烧光
    challenge: "http-01"              # 或 tls-alpn-01（不需要 80 端口）
    http_addr: ":80"                  # 80 被反代占用且反代会转发 /.well-known/ 时写 off
    domains: ["status.example.com"]
    ips: ["203.0.113.10"]             # RFC 8738 IP 证书（可选）
    sync_obtain: false                # true = 启动时前台申请首张证书，失败则进程起不来
  hsts: { enabled: true, max_age: 31536000, include_subdomains: false, preload: false }
```

`sync_obtain` 的两种取值各有用途：**默认 `false`** 时首张证书在后台申请（不阻塞启动），
申请期间站点暂时不可用但进程能起来，适合日常重启；**部署时脚本会临时置 `true`**，
让申请在前台完成、拿到明确成败，申请成功后自动改回 `false`——否则每次重启都要等 CA 响应。

三种挑战方式的前提条件不同，**选错会在申请时才失败**，所以先看清楚：

| challenge      | 前提                                                   | 通配符 |
| -------------- | ---------------------------------------------------- | --- |
| `http-01`      | 本机 80 端口可达且没被别的服务占着（反代要转发 `/.well-known/`）           | 不支持 |
| `tls-alpn-01`  | 80 被占用、443 由本进程直接监听                                  | 不支持 |
| `dns-01`       | DNS 服务商 API 凭据（现支持 Cloudflare），**不需要开放任何入站端口** | **支持 `*.example.com`** |

```yaml
tls:
  mode: acme
  acme:
    challenge: "dns-01"
    dns_provider: "cloudflare"        # 目前只实现 Cloudflare
    dns_api_token: "..."              # 建议单独建一个 Token：只给 Zone:DNS:Edit + Zone:Zone:Read
    domains: ["example.com", "*.example.com"]
```

`dns-01` 的凭据**刻意不提供命令行参数**（会留在 shell 历史里）。无人值守时用环境变量
`PROBE_ACME_DNS_PROVIDER` / `PROBE_ACME_DNS_TOKEN`，交互执行时脚本会提示输入且不回显。

两个刻意做成这样的点：

- **续期复用同一把私钥**（只换证书不换密钥）。Agent 的证书固定用的是安装信息里的 SPKI 指纹，
  换密钥指纹就变、**所有 Agent 会同时离线**；自签证书同理，配置没变就复用磁盘上的产物。
- **HSTS 只在"确认走 HTTPS"的连接上下发**。明文 HTTP 上发它浏览器按规范直接忽略，却会造成
  "已经加固"的错觉。判定与会话 Cookie 的 `Secure` **共用同一逻辑**，不会出现"Cookie 已降级、HSTS 还在发"。

首张证书在后台申请，**不阻塞启动**；证书运行态（SAN / 到期 / SPKI 指纹 / 最近失败原因）见
`GET /api/admin/v1/tls/status`，也可以用 `probe-master tls status` 在命令行看同一份；
自签指纹会同时以 base64 与冒号十六进制两种写法打印，可直接填给 Agent。

## 公开看板与后台：两个域名，还是一个站点

两种部署方式都支持，且**可在管理端「系统设置 → 站点形态」运行时切换，不需要重启**：

| `site.layout`  | 效果                                                  |
| -------------- | --------------------------------------------------- |
| `single`       | 一个域名下同时提供公开看板与 `/admin/`，两边不再区分域名                    |
| `split`        | 公开域名只服务公开路径、管理域名只服务管理路径，用错域名 **421**                |
| `auto`（默认）     | 跟随 `master.yaml`：配了 host 白名单就分离、没配就同站（与升级前行为完全一致）      |

也可以直接在 `master.yaml` 里声明，配了 `origin` 会自动推导 Host 白名单：

```yaml
public_origin: "https://status.example.com"
admin_origin:  "https://admin.example.com"
```

- **Host 不匹配返回 421 Misdirected Request**，而不是 404：这是部署配置错误（域名解析指过来了、
  但这里不服务它），421 让运维一眼看出"域名对不上"，同时不泄露任何内容。
- 默认端口视为"没写端口"（`a.com` 与 `a.com:443` 同源）；`admin.example.com:8443` 则是**另一个源**。
- **选 `split` 时两个域名清单都必须给**，否则保存被直接拒绝——这是防自锁：分离 + 缺管理域名会让
  后台在保存成功后的**下一个**请求上 421，而把它改回来恰好需要访问后台。
- ACME 的 `/.well-known/acme-challenge/` 路径豁免该门禁（CA 按 IP 访问，永远命中不了域名白名单）。
- **公开看板顶栏的「管理后台」按钮跟着站点形态走**：同站时是相对路径 `/admin/`；分离或不同端口时
  自动补成绝对地址（优先设置项 `site.admin_url`，其次 `admin_origin`，再次由唯一的管理域名推导）。
  多管理域名时**不猜**，保持相对路径。要指向别处就直接填 `site.admin_url`。

典型接法是「一个端口（比如 8080）+ 两个域名都解析到这台机器 + 反代按 Host 转发」，反代方案里别忘了
把反代网段填进 `trusted_proxies`。

## 添加被监控节点（Agent 一键安装）

在管理端「节点」里新建节点 → 生成注册码（**一次性、短时有效，装完即失效**）→ 把后台给出的命令
复制到目标机执行：

```bash
curl -fsSL https://status.example.com/install.sh | sudo sh -s -- --code ABCD-EFGH-JKLM
```

Windows 节点用后台给出的 PowerShell 命令。Agent 装好后纯外联上报，**不需要开放任何入站端口**；
容器里会自动跳过系统服务注册（只放好二进制并前台拉起），Alpine/BusyBox 的 `mktemp`、缺失的
`install` 命令也都有兜底。

## 升级、备份与回滚

```bash
# Master：再跑一次部署脚本即可（幂等，走"停服务 → 换二进制 → 启动"的升级路径）
sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master-bin.sh)"

# Agent 自升级（取件走节点令牌鉴权，不依赖公开的 /dist/）
sudo probe-agent upgrade --check && sudo probe-agent upgrade
sudo systemctl restart probe-agent     # 升级不会自行重启，输出里会给出该执行的命令
```

升级的三条硬保证：**先校验后落地**（校验和不对就什么都不做）、**绝不主动降级**
（"比最新还新"也算无需动作；降级要显式 `--version`）、**替换失败必须还原**（不留半套状态）。
替换前的二进制留在 `<目标>.old`，`probe-agent upgrade --rollback` 一键换回，被换下的那份留成
`.broken` 供复盘。

备份在管理端「系统设置 → 数据库备份」手动触发或按日自动（`VACUUM INTO` 在线一致快照 + `keep` 轮转）。
**不要用 `cp` 拷贝数据库文件**——WAL 模式下会拿到撕裂副本，能打开、能查询、但数据不一致。
恢复时在管理端选一份快照执行恢复；条件允许的话，请在一台闲置实例上先走一遍，确认备份真的可用。

## 卸载

卸载**不需要源码、不需要 Go**：它从仓库取回部署脚本执行，所以不会因为工具链装不上而卸不掉。

```bash
# Linux（默认连数据一起清：系统服务、二进制、配置目录、数据目录、probe 用户）
sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/uninstall-master.sh)"

sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/uninstall-master.sh)" xb --keep-data
sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/uninstall-master.sh)" xb --dry-run
```

```powershell
# Windows
$f="$env:TEMP\xb-uninstall.ps1"; irm https://raw.githubusercontent.com/koyoca/XB-Probe/main/uninstall-master.ps1 -OutFile $f; powershell -ExecutionPolicy Bypass -File $f
```

已经克隆过源码的机器不用再下脚本，两个入口都带 `--uninstall`：

```bash
sudo sh install-master.sh --uninstall --keep-data       # 或 install-master-bin.sh --uninstall
```

**直接重跑安装脚本也会先问"安装还是卸载"**（第 1 问），所以即使当初是用一行命令装的、
本机并没有 `deploy-master.sh`，也照样能从这里进卸载。安装收尾时脚本会根据你是从哪个入口进来的，
打印一条**可原样粘贴**的卸载命令。

Windows 同理：`powershell -File install-master.ps1 -Uninstall -KeepData`。

- 删除前先过闸：只肯删**像本脚本建的**路径（绝对路径、含 `probe`、至少两级），
  `/`、`/etc`、`/probe` 这类一律拒绝，并把要删的东西列出来；交互执行时还会再确认一次。
- 每一项都**逐条回显**"已删除 / 残留 / 不存在"——卸载最常出问题的地方不是删错，而是
  "以为删了、其实因为只读挂载或权限没删掉"，那件事原本对用户完全不可见。
- `--dry-run` 只列计划、不动系统；`--keep-data` 保留数据目录（里面有数据库与 `master.key`）。
- 收尾还会把仍在监听的 `80/443/8080/8081` 列出来，**免去你自己 `netstat` 去核对**。

### Caddy 也会被一起停掉

选了「交给 Caddy 代管」的部署，卸载时会**停掉 `caddy.service`**（否则它继续占着 80/443，
听起来卸干净了、实际没有），并处理它的配置。但这里最要紧的是**分清哪些是我们的**：

- **服务单元**：只有 `caddy.service` 里带 `Server Probe` 标记（我们自己写的那份）才禁用并删除。
  发行版包或 x-ui 自带的那份会被**停掉但不删**，并明确告诉你"它开机仍会自启，需要的话手工
  `systemctl disable caddy`"——把别人的服务删掉比没卸载干净严重得多。
- **Caddyfile**：有过追加就会留下 `Caddyfile.bak`，卸载时**从备份还原**；没有备份则按脚本写入的
  标记行**精确摘除我们那一段**（同文件里别人的站点保留）；只有在"整份都是我们写的"情况下才删文件。
- **二进制**：本脚本从不通过包管理器装 Caddy，所以 `/usr/bin/caddy` 之类的**一律不动**。

## 脚本自身版本核对

部署脚本启动时会打印自己的指纹与**关键修复标记**：

```
部署脚本    scripts/deploy-master.sh sha256=6f296988…
```

若这份脚本缺少某个修复标记（说明你拿到的是**旧版**，最常见的原因是 `raw.githubusercontent.com`
前面的 CDN 缓存了 `main`），它会直接点出来并给出强制回源的取法：

```
  ⚠ 本脚本缺少以下修复标记： binary_supports_config_key
    → 你拿到的是旧版脚本（很可能是 CDN 缓存）。请重跑安装命令，
      或改用带随机串的地址强制回源：
      curl -fsSL "https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master-bin.sh?t=…" -o imb.sh
```

入口脚本（`install-master-bin.sh`）在取部署脚本时已经带上了 `Cache-Control: no-cache` 与随机会话串，
所以正常情况下不会再取到旧版——这一步是给"上游缓存行为不如预期"留的**可诊断**退路：
不必去比哈希，看那行警告就知道。

## 从源码构建（开发）

```bash
make master agent               # 或 make dist 交叉编译全部平台
# Windows 无 make：powershell -ExecutionPolicy Bypass -File .\scripts\build.ps1 -Dist

cp configs/master.yaml.example master.yaml   # 先改 data_dir / master_key_file 为可写路径
./bin/probe-master init-admin --config ./master.yaml -username admin
./bin/probe-master --config ./master.yaml    # 打开 http://127.0.0.1:18281/admin/
```

需要 Go ≥ 1.26（发布产物用 1.27.2 编），`CGO_ENABLED=0` 静态编译。仓库里**没有** `vendor/`，
依赖由 `go.mod` + `go.sum` 锁定、走 `GOPROXY` 拉取；需要离线构建时在本机自行 `go mod vendor`。

自检（会自己起临时实例，不影响你的数据）：

```bash
go test ./...                    # 25 个包
bash scripts/smoke-test.sh       # 端到端冒烟（311 项断言）
```

## 合规与安全提示

上线前请逐项确认：公开站与管理端分离为不同源、启用 TOTP 并保存恢复码、`trusted_proxies` 配置正确、
`dist` 外置并用 `make sign` 签名（公钥填进设置项 `install.pubkey`），以及备份确实恢复得回来。

本项目是**自用监控工具**：请只监控你拥有或已获授权的设备。
