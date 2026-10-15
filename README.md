# XB-Probe（自用轻量级服务器探针）

面向个人多节点 VPS 的极轻量监控与资产管理系统：一个 **Master**（内嵌 SQLite、管理后台、公开看板，零外部依赖）+ 若干 **Agent**（纯外联上报，不监听端口）。Master 自带 HTTPS，也可置于 Caddy/Nginx 之后；看板与后台可分域名或同站，运行时切换无需重启。

## 特性

- **零入站攻击面**：Agent 纯外联不监听端口；Master 从不主动连 Agent，无远程控制通道。
- **展示与管理隔离**：公开看板走独立 DTO 白名单。
- **单文件零依赖**：全 Go、免 CGO 内嵌 SQLite；前端原生 HTML/CSS/JS，CSP 钉死 `default-src 'self'`。
- **自带 HTTPS**：内置零依赖 ACME v2（域名 + RFC 8738 IP 证书，三种挑战），也支持已有证书、自签或前置反代。
- **多平台采集**：Linux / Windows / FreeBSD（macOS 不支持）。
- **告警与资产**：离线/阈值/探测失败三类告警（按 (节点,类型) 冷却），Telegram 与 SMTP 渠道；计费、月费折算、流量配额与续期提醒。

## 部署（Linux）

```bash
# 方式 A：预编译二进制（推荐，小机器无需 Go/git/编译；仅 linux/amd64、linux/arm64）
sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master-bin.sh)"
```
```
# 方式 B：取源码现场编译（其它架构含 FreeBSD 用这个）
sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master.sh)"
```

### 证书申请

| 选项 | 对外 | 证书谁办 |
|---|---|---|
| `1) auto`（默认） | HTTPS | 脚本自动装 Caddy 申请/续期 |
| `2) acme` | HTTPS | Master 自己申请（内置 ACME） |
| `3) selfsigned` | HTTPS（浏览器提示不受信任） | 自签，适合纯 IP 访问 |
| `4) files` | HTTPS | 用已有证书/私钥 |
| `5) plain` | 纯 HTTP | 不装反代 |

```bash
# 先看会写什么（不改动系统）
sudo sh install-master-bin.sh --dry-run --version vX.Y.Z

# 完全非交互
sudo sh install-master-bin.sh --yes --public-origin https://status.example.com \
  --tls-mode acme --tls-domain status.example.com --acme-email ops@example.com

# 交给 Caddy 代管（Master 只监听回环）
sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master-bin.sh)" xb \
  --tls-terminator caddy --proxy-domain status.example.com \
  --public-listen 127.0.0.1:8080 --admin-listen 127.0.0.1:8081 --public-origin https://status.example.com
```

> 不要用 `curl … | sudo sh`：管道占掉 stdin，交互项会静默取默认（甚至空口令）。一行式带参数时先插占位名（如上例的 `xb`），否则参数会被拼进脚本正文。
>
> 免编译入口的二进制来自 **Release 标签**、脚本来自 **`main`**：二者不同步时脚本可能写出二进制不认识的配置键而被拒启动，用方式 B 可彻底避免。

**Windows**：以管理员 PowerShell 执行（脚本带 BOM，须落盘后运行，勿用 `irm … | iex`）：

```powershell
$f="$env:TEMP\xb-install.ps1"; irm https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master.ps1 -OutFile $f; powershell -ExecutionPolicy Bypass -File $f
```

参数同 Linux 版；口令以 `-AsSecureString` 读入不回显；用"最高权限计划任务"自启。

**部署后必做**：① 打开 `https://<域名>/admin/` 登录；② **启用 TOTP 并保存恢复码**（系统 → 安全，手机丢了的唯一退路）；③ 前置反代时把代理网段填进 `trusted_proxies`，否则登录后掉线；④ **离线另存 `master.key`**（加密告警凭证，丢了不可解密、泄漏等于凭证泄漏）。

## TLS 与证书

| `tls.mode` | 适用 | 说明 |
|---|---|---|
| `none` | 前置 Caddy/Nginx 卸载 TLS | 需配 `trusted_proxies`；`--tls-terminator caddy` 自动装好反代 |
| `files` | 已有证书 | 读 `cert_file`/`key_file`，变更热加载 |
| `selfsigned` | 内网/私有 IP 要 IP 证书 | 本进程生成，SAN 覆盖域名与 IP |
| `acme` | 有公网 IP 自动申请续期 | 内置 ACME v2，三种挑战 |

```yaml
tls:
  mode: acme
  acme:
    email: "ops@example.com"
    ca: "letsencrypt-staging"     # 先 staging 试，别烧生产配额
    challenge: "http-01"          # 或 tls-alpn-01（不需 80）/ dns-01（支持通配符）
    domains: ["status.example.com"]
```

三种挑战**选错在申请时才失败**：`http-01` 需本机 80 可达未被占用（不支持通配符）；`tls-alpn-01` 用于 80 被占、443 本进程监听（不支持通配符）；`dns-01` 走 DNS API（现 Cloudflare），无需入站端口，支持 `*.example.com`。

`dns-01` 凭据不走命令行参数（防留 shell 历史），用环境变量 `PROBE_ACME_DNS_PROVIDER` / `PROBE_ACME_DNS_TOKEN`，交互时提示输入不回显。证书运行态（`xb-probe-master tls status` 或 `GET /api/admin/v1/tls/status`）可见 SAN / 到期 / SPKI 指纹 / 最近失败原因。

## 站点形态：两域名还是一站点

`系统设置 → 站点形态` 运行时切换，无需重启：

| `site.layout` | 效果 |
|---|---|
| `single` | 一个域名同时提供看板与 `/admin/` |
| `split` | 公开域名只服务公开路径，管理域名只服务管理路径（用错域名 **421**） |
| `auto`（默认） | 配了 host 白名单就分离，没配就同站 |

`split` 时两个域名清单都必须给，否则保存被拒（防自锁）；Host 不匹配返回 421 而非 404。

## 添加被监控节点（Agent）

管理端「节点」新建节点 → 生成注册码（一次性、短时有效）→ 复制后台命令到目标机执行：

```bash
curl -fsSL https://status.example.com/install.sh | sudo sh -s -- --code ABCD-EFGH-JKLM
```

装好后纯外联上报，无需开放入站端口。

## 升级、备份与回滚

```bash
# Master：再跑一次部署脚本即可（幂等升级）
sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master-bin.sh)"

# Agent 自升级（走节点令牌鉴权）
sudo xb-probe-agent upgrade --check && sudo xb-probe-agent upgrade
sudo xb-probe-agent install --service-only   # 服务定义/属主有更新时重写并重启
```

升级三保证：**先校验后落地**、**绝不主动降级**、**替换失败必须还原**（`.old` 留底，`upgrade --rollback` 一键换回）。

备份在「系统设置 → 数据库备份」手动或按日自动（`VACUUM INTO` 一致快照）。**不要用 `cp` 拷贝数据库文件**（WAL 模式会拿到撕裂副本）。

## 卸载

```bash
# Linux（默认连数据一起清；加 --keep-data 保留数据目录）
sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/uninstall-master.sh)"
sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/uninstall-master.sh)" xb --keep-data

# Windows
$f="$env:TEMP\xb-uninstall.ps1"; irm https://raw.githubusercontent.com/koyoca/XB-Probe/main/uninstall-master.ps1 -OutFile $f; powershell -ExecutionPolicy Bypass -File $f
```

只删像本脚本建的路径（`/`、`/etc` 等一律拒绝）；`--dry-run` 只列计划不动系统。安装脚本第 1 问也会先问"安装还是卸载"。

## 从源码构建（开发）

```bash
make master agent              # 或 make dist 交叉编译全部平台
cp configs/master.yaml.example master.yaml   # 改 data_dir / master_key_file 为可写路径
./bin/xb-probe-master init-admin --config ./master.yaml -username admin
./bin/xb-probe-master --config ./master.yaml
```

需 Go ≥ 1.26（发布用 1.27.2），`CGO_ENABLED=0` 静态编译。自检：`go test ./...` 与 `bash scripts/smoke-test.sh`。

## 合规与安全提示

上线前确认：公开站与管理端分离为不同源、启用 TOTP 并保存恢复码、`trusted_proxies` 配置正确、备份确实恢复得回来。本项目是**自用监控工具**，请只监控你拥有或已获授权的设备。
