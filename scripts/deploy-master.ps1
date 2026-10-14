# XB-Probe —— Master 一键部署（Windows）
#
# 覆盖：放二进制 → 写配置并初始化管理员 → 注册开机自启（计划任务）→ 健康检查
# → 证书状态 → 收尾汇总。幂等：重复执行走「升级」路径（停任务 → 换二进制 → 启动）。
#
# 用法（管理员 PowerShell）：
#   powershell -ExecutionPolicy Bypass -File scripts\deploy-master.ps1 -Bin bin\xb-probe-master.exe -AdminUser ops
#
# 不传 -AdminPassword 时会交互式提示（推荐）。
#
# 交互问答：**参数没给全时会问你**——TLS 方式（要不要 https）与校验方式、公开站/管理端
# 各监听哪个端口、对外访问地址、管理员用户名，最后把将要做的事列一遍让你确认。
# 三种情况不问：-Yes ／ 标准输入被重定向 ／ 该项已显式给出。
# 想在任何环境下都走一遍问答（自测用）：$env:PROBE_ASK_FORCE = "1"。
#
# 只看不装（打印将要写入的 master.yaml，不建目录、不注册任务、不建管理员）：
#   powershell -File scripts\deploy-master.ps1 -Bin bin\xb-probe-master.exe -DryRun
#
# 卸载（默认连数据一起清；数据目录里有数据库与 master.key）：
#   powershell -File scripts\deploy-master.ps1 -Uninstall
#   powershell -File scripts\deploy-master.ps1 -Uninstall -KeepData
#   powershell -File scripts\deploy-master.ps1 -Uninstall -DryRun   # 只列出会删什么
#
# 只升级 Master 本体（换二进制 + 重启计划任务；不问站点/证书/管理员，也不改它们）：
#   powershell -File scripts\deploy-master.ps1 -UpgradeOnly -Bin bin\xb-probe-master.exe
#
# 只更新 Agent 发布文件（查 GitHub 最新 Release 拉进 data_dir/dist/；不动 Master）：
#   powershell -File scripts\deploy-master.ps1 -ReleaseOnly
#   powershell -File scripts\deploy-master.ps1 -ReleaseOnly -DryRun   # 只打印会拉什么
#
# 只重置管理员密码（不卸载、不清数据、不重写配置、不重启任务）：
#   powershell -File scripts\deploy-master.ps1 -ResetPassword
#   口令按 -AdminPassword / $env:PROBE_ADMIN_PASSWORD / 交互的先后取；
#   用户名默认 ops，可用 -AdminUser 指定。
#
# 只看这次会问什么、选哪些模式（给入口脚本用，不碰系统、不需要管理员）：
#   powershell -File scripts\deploy-master.ps1 -AskMode
#
# 说明：Master 在 Windows 上没有内置的服务封装（Agent 才有），这里用
# 「开机时以最高权限运行」的计划任务实现自启；需要真正的服务形态（可 Restart-Service）
# 时请用 NSSM / WinSW 包一层，命令行仍是一句：xb-probe-master -config <配置文件>
#
# dns-01 的 DNS 凭据：优先用环境变量（命令行参数会进 PSReadLine 历史）
#   $env:PROBE_ACME_DNS_PROVIDER（默认 cloudflare）
#   $env:PROBE_ACME_DNS_TOKEN（Cloudflare API Token，需 Zone:DNS:Edit + Zone:Zone:Read）
# 交互执行时这两项会被提示输入，Token 不回显。

param(
    [string]$Bin = "",
    [string]$Dir = "C:\ProgramData\xb-probe-master",
    [string]$Data = "C:\ProgramData\xb-probe-master\data",
    [string]$PublicListen = "",
    [string]$AdminListen = "",
    [string]$PublicOrigin = "",
    [string]$AdminOrigin = "",
    [string]$DistBase = "",
    [string]$AdminUser = "",
    [string]$AdminPassword = $env:PROBE_ADMIN_PASSWORD,
    # TLS：none（默认，交给前置反代）| files（自有证书）| selfsigned（自签）| acme（自动申请）
    [string]$TlsMode = "",
    [string]$TlsCert = "",
    [string]$TlsKey = "",
    [string]$TlsDomain = "",   # 域名，逗号分隔；acme 的 domains / selfsigned 的 SAN
    [string]$TlsIp = "",       # IP，逗号分隔；acme 的 RFC 8738 证书 / selfsigned 的 SAN
    [string]$AcmeEmail = "",
    [string]$AcmeCa = "letsencrypt",
    [string]$AcmeChallenge = "",
    [string]$AcmeDnsProvider = $env:PROBE_ACME_DNS_PROVIDER,
    [string]$AcmeDnsToken = $env:PROBE_ACME_DNS_TOKEN,
    [switch]$NoStartupTask,
    [switch]$ForceConfig,
    [switch]$Yes,
    [switch]$DryRun,
    [switch]$Uninstall,
    [switch]$KeepData,
    # 只更新 Agent 发布文件：查 GitHub 最新 Release，把它的发布件拉进 data_dir/dist/。
    # 不动 Master 的二进制、配置与自启（详见 0.2b）。
    [switch]$ReleaseOnly,
    # 只升级 Master 本体：只换二进制 + 重启计划任务。配置、证书、管理员账号、
    # 发布目录一个都不动（详见 0.3）。
    [switch]$UpgradeOnly,
    # 只重置管理员密码：不卸载、不清数据、不重写配置、不重启任务（详见 0.4）。
    [switch]$ResetPassword,
    # 只打印"做什么"的菜单并把答案写到 stdout 然后退出：入口脚本用它来"先问后做"。
    # 不碰系统、不读配置、不需要管理员，也不需要已经装好（见 0.1b）。
    [switch]$AskMode
)

$ErrorActionPreference = "Stop"

# 出厂默认值：**只有这一份来源**。问题里方括号显示的默认值必须与真正生效的值同源，
# 否则迟早出现"提示写 8080、实际听 8081"。
$DefaultPublicListen = "0.0.0.0:8080"
$DefaultAdminListen = "127.0.0.1:8081"

function Write-Step { param([string]$Msg) Write-Host "==> $Msg" -ForegroundColor Cyan }
function Write-Warn2 { param([string]$Msg) Write-Host "警告：$Msg" -ForegroundColor Yellow }
function Write-Err2 { param([string]$Msg) Write-Host "错误：$Msg" -ForegroundColor Red }

# 0) 目标路径（参数默认值的唯一来源在 param() 与上面那两个常量里）
$target = Join-Path $Dir "xb-probe-master.exe"
$cfg = Join-Path $Dir "master.yaml"

# 发布仓库：查「最新 Release」时的目标。与 shell 侧的 PROBE_RELEASE_REPO 同名同义。
if ($env:PROBE_RELEASE_REPO) { $ReleaseRepo = $env:PROBE_RELEASE_REPO } else { $ReleaseRepo = "koyoca/XB-Probe" }

# 本机平台（与 install.ps1 的命名一致：<os>_<arch>）。"只拉本机"时要用它。
$LocalArch = "amd64"
if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { $LocalArch = "arm64" }
elseif (-not [Environment]::Is64BitOperatingSystem) { $LocalArch = "386" }
$LocalPlatform = "windows_$LocalArch"

# ---------------------------------------------------------------------------
# 0.1) 是否提问
# ---------------------------------------------------------------------------
# -Yes 或标准输入被重定向时不提问——后者若不拦，"所有问题都取默认值"会装出一个
# 默认值拼起来的部署，而且看起来一切正常。
$Ask = $true
if ($Yes) { $Ask = $false }
if ($env:PROBE_NO_ASK) { $Ask = $false }
if ([Console]::IsInputRedirected) { $Ask = $false }
if ($env:PROBE_ASK_FORCE) { $Ask = $true }

# ---------------------------------------------------------------------------
# 0.1b) -AskMode：只问"做什么"，把答案写到 stdout 然后退出
# ---------------------------------------------------------------------------
#
# 菜单**只有这一份**（与 scripts/deploy-master.sh 的 0a-3 一一对应）。入口脚本
# （install-master.ps1）用它是为了"先问后做"：选卸载/重置密码的人不该先等一次
# 完整的 Go 编译；选"只升级本体"的人也不该先答完整站问答才发现自己白答了。
#
# 通道约定（入口要靠它解析）：菜单与提示走 **stderr / 控制台**，答案走 **stdout**
# ——调用方要拿到一个干净的数字，不能被菜单文本污染。
function Show-ModeMenu {
    Write-Host ""
    Write-Host "这个脚本可以做五件事："
    Write-Host ""
    Write-Host "  1) 安装 / 升级 Master"
    Write-Host "     装（或升级）Master 本体：二进制、配置、开机自启，以及新节点要用的"
    Write-Host "     Agent 发布文件。**会重新收敛整站配置**：重写 master.yaml、按需要申请"
    Write-Host "     证书、初始化管理员。适合首次安装，或配置被改坏了要拉回正轨。"
    Write-Host ""
    Write-Host "  2) 卸载 Master"
    Write-Host "     停任务 -> 删计划任务、二进制、配置、数据。默认**连数据一起删**。"
    Write-Host ""
    Write-Host "  3) 只更新 Agent 发布文件"
    Write-Host "     去 GitHub 查最新 Release，把它的 Agent 发布文件拉进 data_dir/dist/。"
    Write-Host "     不动 Master 的二进制、配置与自启（不重启、也不重问任何配置）。"
    Write-Host ""
    Write-Host "  4) 只升级 Master 本体"
    Write-Host "     只换二进制并重启任务。**不重写 master.yaml、不申请证书、不碰管理员"
    Write-Host "     账号、也不重拉 Agent 发布文件**。适合「网站已经跑得好好的，只想把"
    Write-Host "     Master 升到新版本」。启动失败会自动换回旧二进制。"
    Write-Host ""
    Write-Host "  5) 重置管理员密码"
    Write-Host "     只改一个管理员的登录口令（用户名默认 ops，可用 -AdminUser 指定），"
    Write-Host "     其它什么都不动。忘了后台密码时用它，不必卸载重装。"
    Write-Host ""
}

function Read-ModeAnswer {
    # 读不到（标准输入被重定向 / -Yes）时取 1：与 shell 侧一致，默认是"安装"。
    Show-ModeMenu
    $a = ""
    try { $a = Read-Host "请输入 1、2、3、4 或 5 [1]" } catch { $a = "" }
    if (-not $a) { $a = "1" }
    switch -Regex ($a.Trim()) {
        "^2$|^uninstall$|^卸载$" { return "2" }
        "^3$|^release$|^更新$|^发布文件$" { return "3" }
        "^4$|^upgrade$|^升级本体$|^只升级本体$" { return "4" }
        "^5$|^password$|^重置密码$|^改密码$" { return "5" }
        default { return "1" }
    }
}

if ($AskMode) {
    # 答案走 stdout（Write-Output），菜单走控制台。**只输出那一个数字**。
    Write-Output (Read-ModeAnswer)
    exit 0
}

# ---------------------------------------------------------------------------
# 0.1c) 四个「只做一件事」的模式互斥
# ---------------------------------------------------------------------------
#
# 判据写成"数一数有几个"，而不是"两两组合列举"：多一个模式就要多 N 对组合，
# 写全很啰嗦且漏一对没人发现。同时给就是自相矛盾的要求，直接拒绝——而不是让其中
# 一个悄悄赢：悄悄赢的那种，用户以为做了 A、实际做了 B，而且两条路都不报错。
$oneShot = @()
if ($Uninstall) { $oneShot += "-Uninstall" }
if ($ReleaseOnly) { $oneShot += "-ReleaseOnly" }
if ($UpgradeOnly) { $oneShot += "-UpgradeOnly" }
if ($ResetPassword) { $oneShot += "-ResetPassword" }
if ($oneShot.Count -gt 1) {
    Write-Err2 "这几项都只做一件事，不能同时使用：$($oneShot -join " ")"
    Write-Host "  「卸载」「只更新发布文件」「只升级本体」「重置密码」一次只能选一个。" -ForegroundColor Red
    exit 2
}

# ---------------------------------------------------------------------------
# 0.1d) 管理员权限检查
# ---------------------------------------------------------------------------
$id = [Security.Principal.WindowsIdentity]::GetCurrent()
$pr = New-Object Security.Principal.WindowsPrincipal($id)
if (-not $DryRun -and -not $pr.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Err2 "请用「以管理员身份运行」的 PowerShell 执行。"
    exit 1
}

# Read-Answer：读一行，空答案取默认值。不问时直接把默认值交回去——
# "问"与"不问"对后面的逻辑是同一件事。
function Read-Answer {
    param([string]$Prompt, [string]$Default = "")
    if (-not $Ask) { return $Default }
    $p = $Prompt
    if ($Default) { $p = "$Prompt [$Default]" }
    $a = Read-Host $p
    if (-not $a) { return $Default }
    return $a
}

# Read-Required：不允许空答案（ACME 的域名这类），最多重问三次。
function Read-Required {
    param([string]$Prompt)
    if (-not $Ask) { return "" }
    for ($i = 0; $i -lt 3; $i++) {
        $a = Read-Host $Prompt
        if ($a) { return $a }
        Write-Host "  （这一项不能为空）"
    }
    return ""
}

# Read-Secret：不回显地读一行。-AsSecureString 由 .NET 自己办，
# 不存在 shell 里那种"退格被当成字面 ^H"的问题。
function Read-Secret {
    param([string]$Prompt)
    if (-not $Ask) { return "" }
    $sec = Read-Host "$Prompt（不回显）" -AsSecureString
    return [Runtime.InteropServices.Marshal]::PtrToStringAuto([Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec))
}

function Confirm-Yes {
    param([string]$Prompt)
    if (-not $Ask) { return $true }
    $a = Read-Host "$Prompt [Y/n]"
    if ($a -match "^(n|N|no|NO|No)$") { return $false }
    return $true
}

# ---- 小工具：端口归一化、默认地址推导、用户名校验 --------------------------

function Get-PortOf { param([string]$Addr) return $Addr.Substring($Addr.LastIndexOf(":") + 1) }

# Normalize-Listen：允许只写端口（"9000"），也允许写 "IP:端口"。
function Normalize-Listen {
    param([string]$Value, [string]$DefaultHost)
    if ($Value -match ":") { return $Value }
    return "${DefaultHost}:$Value"
}

function Test-PortOk {
    param([string]$Value)
    if (-not $Value) { return $false }
    $p = Get-PortOf $Value
    $n = 0
    if (-not [int]::TryParse($p, [ref]$n)) { return $false }
    return ($n -ge 1 -and $n -le 65535)
}

# Get-OriginDefault：端口是该协议的默认值时不写出来——
# `https://example.com` 比 `https://example.com:443` 更像人写的东西。
function Get-OriginDefault {
    param([string]$Scheme, [string]$HostName, [string]$Port)
    # ${Scheme} 必须带花括号：`"$Scheme://…"` 里 PowerShell 会把 `$Scheme:` 当成
    # 作用域限定的变量名（psdrive:name 那种写法）直接报"变量引用无效"。
    $h = $HostName
    if ($h -match ":") { $h = "[$h]" }
    if ($Scheme -eq "http" -and $Port -eq "80") { return "${Scheme}://$h" }
    if ($Scheme -eq "https" -and $Port -eq "443") { return "${Scheme}://$h" }
    return "${Scheme}://${h}:$Port"
}

# Get-AdminUrlFrom：补成后台入口地址，幂等（不会得到 //admin/ 或 /admin/admin/）。
function Get-AdminUrlFrom {
    param([string]$Origin)
    $o = $Origin.TrimEnd("/")
    if ($o -match "/admin$") { return "$o/" }
    return "$o/admin/"
}

# 用户名只允许字母、数字与 . _ @ -
# 有这个判断是因为实际发生过：提示"管理员用户名 [ops]"时有人把上一题的答案
# （一个 host:port）又粘了一遍，脚本照单全收，于是建出一个名字里带冒号的管理员，
# 后台登不进去——而失败发生在"部署成功"之后。
function Test-AdminUserOk {
    param([string]$Value)
    if (-not $Value) { return $false }
    return ($Value -match "^[A-Za-z0-9._@-]+$")
}

function Get-PublicIp {
    try {
        $r = Invoke-WebRequest -Uri "https://api.ipify.org" -UseBasicParsing -TimeoutSec 5
        $ip = "$($r.Content)".Trim()
        if ($ip) { return $ip }
    } catch { }
    return ""
}

# ---------------------------------------------------------------------------
# 0.2) 卸载 -Uninstall
# ---------------------------------------------------------------------------
#
# 位置刻意在 TLS 问答**之前**：卸载不该再问"你要哪种证书"。
#
# "卸载干净"包含四件事，少一件都会留下需要人手收拾的尾巴：
# 计划任务、二进制、配置目录（master.yaml + master.key）、数据目录
# （数据库 + 主题 + 发布件 + 证书）。默认**连数据一起删**（这才叫干净），
# 但要过两道闸：① 只肯删"看起来是本脚本建的"路径；② 交互执行时再确认一次。
if ($Uninstall) {
    # Test-OursPath：这条路径能不能被我们删。
    #
    # 这条闸看着多余——路径不就是用户自己给的吗？但它是**可传参**的，
    # 一个手滑的 `-Data C:\` 会把整个盘清掉。判据不是"用户确认过"，
    # 而是"这个路径像不像本脚本创建的"：不像就拒绝，把删除留给用户自己决定。
    #
    # 至少两级是**必要**条件而不是形式主义：`C:\probe` 恰好会被"含 probe"放过，
    # 而它同样是"盘根下顺手建的一个名字"，删掉不可逆。
    function Test-OursPath {
        param([string]$P)
        if (-not $P) { return $false }
        $t = $P.TrimEnd("\", "/")
        if (-not $t) { return $false }
        # `C:\ProgramData\xb-probe-master` 命中（盘根 + 一级 + 分隔符）；
        # `C:` 与 `C:\probe` 都不命中。
        if ($t -notmatch "^[A-Za-z]:[\\/][^\\/]+[\\/]") { return $false }
        return ($t -like "*probe*")
    }

    # Remove-ItemReported：删除并把结果**回显出来**。
    # 逐项回显是刻意的：卸载最常出的问题不是删错，而是"以为删了、实际因为
    # 占用或权限没删掉"，而这件事对用户完全不可见。
    function Remove-ItemReported {
        param([string]$Path, [string]$What)
        if ($DryRun) {
            if (Test-Path $Path) { Write-Host "    [dry-run] 将删除 $Path（$What）" }
            else { Write-Host "    $Path 不存在（$What）" }
            return
        }
        if (-not (Test-Path $Path)) { Write-Host "    $Path 不存在（$What）"; return }
        try {
            Remove-Item -LiteralPath $Path -Recurse -Force -ErrorAction Stop
        } catch {
            Write-Host "    ⚠ 残留：$Path（$What）—— $($_.Exception.Message)" -ForegroundColor Yellow
            return
        }
        if (Test-Path $Path) {
            Write-Host "    ⚠ 残留：$Path（$What）" -ForegroundColor Yellow
        } else {
            Write-Host "    已删除 $Path（$What）"
        }
    }

    foreach ($p in @($Dir, $Data)) {
        if (-not (Test-OursPath $p)) {
            Write-Err2 "拒绝删除 $p —— 它不像本脚本创建的目录（路径中应含 probe）。"
            Write-Host "      确认要删请手工执行：Remove-Item -Recurse -Force '$p'" -ForegroundColor Red
            exit 2
        }
    }

    if (-not $KeepData -and -not $DryRun -and $Ask) {
        $a = Read-Host "这会删除数据目录 $Data（含数据库与 master.key），不可恢复。继续？ [y/N]"
        if ($a -notmatch "^(y|Y|yes|YES|Yes)$") {
            Write-Host "已取消，未改动系统。" -ForegroundColor Yellow
            exit 0
        }
    }

    Write-Step "卸载 XB-Probe Master"
    if ($DryRun) { Write-Host "    （dry-run：只列出将要做的事，不删除任何东西）" }

    Write-Step "1/4 注销并停止计划任务"
    $taskName = "xb-probe-master"
    $existing = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    if (-not $existing) {
        Write-Host "    计划任务 $taskName 不存在"
    } elseif ($DryRun) {
        Write-Host "    [dry-run] 停止并注销计划任务 $taskName"
    } else {
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
        if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
            Write-Host "    ⚠ 残留：计划任务 $taskName 仍存在" -ForegroundColor Yellow
        } else {
            Write-Host "    已停止并注销计划任务 $taskName"
        }
    }

    Write-Step "2/4 删除二进制"
    Remove-ItemReported -Path $target -What "Master 可执行文件"

    Write-Step "3/4 删除配置目录"
    Remove-ItemReported -Path $Dir -What "配置（master.yaml、master.key）"

    Write-Step "4/4 删除数据目录"
    if ($KeepData) {
        Write-Host "    保留（-KeepData）：$Data"
    } else {
        Remove-ItemReported -Path $Data -What "数据（数据库、主题、发布件、证书）"
    }

    Write-Host ""
    if ($DryRun) {
        Write-Host "==> dry-run 结束：以上操作均未执行。去掉 -DryRun 即真正卸载。" -ForegroundColor Cyan
    } else {
        Write-Host "==> 卸载完成。" -ForegroundColor Green
        Write-Host "    自启是计划任务、不是 Windows 服务，所以没有需要额外清理的服务登记项。"
    }
    exit 0
}

# Resolve-LatestAgentVersion：查 GitHub 最新 Release，输出归一化后的版本号；
# 查不到就返回空字符串（**由调用方决定怎么办**，不在这里 exit）。
#
# 为什么放在脚本里而不是二进制里（`dist latest` 之类）：部署脚本来自 main
# （每次取最新），而节点上装着的 xb-probe-master 来自某个 **Release 标签**——
# 两者版本天生不同步。放进二进制就等于要求"先把 Master 升到带这个子命令的版本
# 才能用它"，而这个功能恰恰是用在"Master 已经在跑、只想更新 Agent 发布文件"的
# 场合。放脚本里则**任何版本都能用**，因为 `dist pull --version/--from` 这个契约
# 早就存在且稳定。
#
# 两个来源，任一个能用就行（API 有 60 次/小时匿名限额，机房出口 IP 常常被共享，
# 所以必须有一条不限额的兜底）：
#   ① api.github.com/repos/<repo>/releases/latest  → tag_name
#   ② github.com/<repo>/releases/latest 的 302 Location → .../releases/tag/vX.Y.Z
#
# ⚠ 返回值必须过**形态闸**（纯数字点分）。这不是防御性编程：解析失败时拿到的
# 往往是**别的文字**（HTML 错误页、旧二进制的 usage 行）。不校验就会把那段文字
# 当版本号，拼出 `.../releases/download/v<那段文字>`，然后以"发布文件拉取失败"
# 的面目出现，根因被埋掉。同理，这也挡住了形如 `../../x` 的 tag 变成磁盘路径。
function Resolve-LatestAgentVersion {
    # Windows PowerShell 5.1 默认只开 TLS 1.0/1.1，而 GitHub 只收 1.2——
    # 不设这一句，两台机器上的表现会不一样（有的装过 .NET 补丁就能通）。
    try { [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 } catch { }

    $tag = ""
    # ① API
    try {
        $r = Invoke-RestMethod -Uri "https://api.github.com/repos/$ReleaseRepo/releases/latest" `
            -Headers @{ "Accept" = "application/vnd.github+json"; "User-Agent" = "probe-deploy" } `
            -TimeoutSec 20 -ErrorAction Stop
        if ($r -and $r.tag_name) { $tag = [string]$r.tag_name }
    } catch {
        Write-Host "    （GitHub API 不可用：$($_.Exception.Message)）" -ForegroundColor DarkGray
    }

    # ② 网页重定向兜底：HttpWebRequest 关掉自动跳转，直接从 Location 里取。
    if (-not $tag) {
        try {
            $req = [System.Net.HttpWebRequest]::Create("https://github.com/$ReleaseRepo/releases/latest")
            $req.AllowAutoRedirect = $false
            $req.Method = "HEAD"
            $req.UserAgent = "probe-deploy"
            $req.Timeout = 20000
            $resp = $req.GetResponse()
            $loc = [string]$resp.Headers["Location"]
            $resp.Close()
            if ($loc -match "/releases/tag/v?([0-9][0-9.]*)$") { $tag = $matches[1] }
        } catch {
            Write-Host "    （GitHub 网页兜底也不可用：$($_.Exception.Message)）" -ForegroundColor DarkGray
        }
    }

    $tag = "$tag".Trim()
    if ($tag -match "^[Vv]") { $tag = $tag.Substring(1) }
    if ($tag -notmatch '^\d+(\.\d+)*$') { return "" }
    return $tag
}

# ---------------------------------------------------------------------------
# 0.2b) 只更新 Agent 发布文件 -ReleaseOnly
# ---------------------------------------------------------------------------
#
# 与 shell 侧（scripts/deploy-master.sh 的 0c）同一件事：查 GitHub 最新 Release →
# 校验版本号形态 → 用 `dist pull` 把 Agent 发布文件拉进 data_dir/dist/。**不动**
# Master 的二进制、配置、计划任务，也不问 TLS / 端口 / 管理员。
#
# 这一块在 Windows 侧长期缺失，而**缺失的后果比"少一个功能"严重**：没有它，
# 一键部署出来的 Master 里 data_dir/dist/ 是空的，于是新节点拿不到装机通道
# （`/dist/` 恒 404），Agent 根本装不上。Linux 侧早就修了这条
# （见 internal/dist 里的 TestDeployPullsReleaseFilesForNodeBootstrap），
# Windows 侧一直只是"提示用户自己手工敲一条 dist pull"——那正是这个项目
# 明确要消灭的东西。
#
# 判据落在**磁盘**上（SHA256SUMS 存在且非空 + 至少有一个 xb-probe-agent_* 产物），
# 不落在 HTTP 状态码上：/dist/ 受 install.mode 门控，刚部署完返回 404 是预期行为。
if ($ReleaseOnly) {
    Write-Step "只更新 Agent 发布文件"
    Write-Host "    不动 Master 的二进制、配置与自启——只把 Agent 发布文件拉进数据目录。"

    # 用哪个二进制去取件：-Bin（调用方刚编译/刚下载的那份）> 已安装的那份。
    # 允许 -Bin 是有意的：安装入口编译出来的二进制一定不比节点上装着的旧。
    $roBin = $target
    $roFrom = "已安装的那份"
    if ($Bin -and (Test-Path -LiteralPath $Bin)) {
        $roBin = $Bin
        $roFrom = "-Bin 指定的那份"
    }

    # **先校验二进制，再去用它**：顺序应当是"能用的前提先成立"，而不是先撞一下
    # 再解释。dry-run 是唯一例外——它一个字节都不写，正是"什么都还没装的时候
    # 先看看会发生什么"的场合，所以那时只警告。
    if (-not (Test-Path -LiteralPath $roBin)) {
        if ($DryRun) {
            Write-Warn2 "[dry-run] 注意：$roBin 不存在，正式执行会在这里停下。"
            Write-Host "              （用 -Bin 给一份二进制，例如刚编译出来的那份）" -ForegroundColor Yellow
        } else {
            Write-Err2 "找不到 Master 二进制：$roBin（$roFrom）"
            Write-Host "      它要用来读 $cfg 并把发布文件写进 $Data\dist\。" -ForegroundColor Red
            Write-Host "      用 -Bin 给一份二进制（例如刚编译出来的那份）。这台机器还没装过" -ForegroundColor Red
            Write-Host "      Master 的话，请先走一次安装流程。" -ForegroundColor Red
            exit 1
        }
    }

    # 版本查询要在取件之前完成，**失败即止**。这时候退回去拉"本机二进制版本"
    # 是静默换成另一件事：用户以为更新到了最新，实际拉的是他早就在跑的旧版本。
    Write-Step "正在查 GitHub 上的最新 Release（$ReleaseRepo）…"
    $roVer = Resolve-LatestAgentVersion
    if (-not $roVer) {
        Write-Err2 "查不到最新 Release 的版本号，已中止（没有拉取任何东西）。"
        Write-Host "      手工做法：到 https://github.com/$ReleaseRepo/releases 看最新版本号，" -ForegroundColor Red
        Write-Host "      然后执行上面的第 4 条命令。" -ForegroundColor Red
        if (-not $DryRun) { exit 1 }
        Write-Host "==> dry-run：未改动任何东西。" -ForegroundColor Cyan
        exit 0
    }
    Write-Host "==> 最新发布版本：v$roVer"

    $source = "https://github.com/$ReleaseRepo/releases/download/v$roVer"
    if ($DryRun) {
        Write-Host "    [dry-run] 将会拉取到 $Data\dist\$roVer\，来源 $source"
        Write-Host "    [dry-run] 正式执行时还会问一次「拉全部平台还是只拉本机」，再校验落盘。"
        Write-Host "==> dry-run：未改动任何东西。" -ForegroundColor Cyan
        exit 0
    }
    if (-not (Test-Path -LiteralPath $cfg)) {
        Write-Err2 "读不到 $cfg —— 这台机器上还没有部署过 Master？"
        Write-Host "      首次部署请先走一次安装流程。" -ForegroundColor Red
        exit 1
    }

    # 默认值刻意写死 "1"（全部平台）而不是沿用记忆：这个模式存在的意义就是"拉最新"，
    # 上次选过"跳过"的人不该因为一次历史选择而在这里什么都不做。
    $scope = "1"
    if ($Ask) { $scope = Read-Host "拉取范围（1 全部平台 / 2 只拉本机 / 3 跳过）[1]" }
    if (-not $scope) { $scope = "1" }
    switch -Regex ($scope.Trim()) {
        "^3$|^skip$" {
            Write-Host "已跳过：没有拉取任何文件（Master 与发布目录都没变）。" -ForegroundColor Yellow
            exit 0
        }
        "^2$|^local$" {
            Write-Host "    范围：只拉本机（$LocalPlatform）"
            $plat = $LocalPlatform
            if (-not $plat) {
                Write-Err2 "认不出本机平台，不能只拉本机。可以改选全部平台。"
                exit 1
            }
            $pullArgs = @("dist", "pull", "-config", $cfg, "-version", $roVer, "-platform", $plat, "-from", $source)
        }
        default {
            Write-Host "    范围：全部平台"
            $pullArgs = @("dist", "pull", "-config", $cfg, "-version", $roVer, "-all", "-from", $source)
        }
    }
    Write-Host "    来源：$source"

    $pullOut = ""
    try {
        $pullOut = (& $roBin @pullArgs) 2>&1 | Out-String
        $pullRc = $LASTEXITCODE
        if ($null -eq $pullRc) { $pullRc = 0 }
    } catch {
        $pullOut = "$($_.Exception.Message)"
        $pullRc = 1
    }
    Write-Host ($pullOut -replace "(?m)^", "    ")

    # 判据：文件真的在、且非空。
    $sums = Join-Path $Data "dist\$roVer\SHA256SUMS"
    $n = 0
    if (Test-Path -LiteralPath $sums) {
        $n = @(Get-ChildItem -LiteralPath (Join-Path $Data "dist\$roVer") -File -Filter "xb-probe-agent_*" -ErrorAction SilentlyContinue).Count
    }
    if ($pullRc -ne 0 -or -not (Test-Path -LiteralPath $sums) -or (Get-Item $sums).Length -eq 0 -or $n -le 0) {
        Write-Err2 "发布文件没有入库（$Data\dist\$roVer 里没有可用的 SHA256SUMS）。"
        Write-Host "      手工补救：" -ForegroundColor Red
        Write-Host "      & `"$roBin`" dist pull -config `"$cfg`" -version $roVer -all -from `"$source`"" -ForegroundColor Red
        exit 1
    }
    Write-Host "    已入库 $n 个 Agent 产物 + SHA256SUMS → $Data\dist\$roVer"
    Write-Host "==> 完成：新节点装到的 Agent 就是这一版。" -ForegroundColor Green
    Write-Host "    新节点：后台「节点 → 注册码」生成注册码，按页面给的命令装即可"
    Write-Host "    已装节点：xb-probe-agent upgrade --check 然后 xb-probe-agent upgrade"
    exit 0
}

# ---------------------------------------------------------------------------
# 0.3) 只升级 Master 本体 -UpgradeOnly
# ---------------------------------------------------------------------------
#
# 用户原话：「如果 master 更新，但是我只想更新本体，如果我选择 1) 安装 / 升级 Master，
# 那么他会全部走一遍流程，包括站点设置、证书申请、管理员账号设置等全部流程，但是
# 这些我不想做，我仅仅只想升级 master 本体」。
#
# 所以本块只做一件事：把二进制换掉、把计划任务重启一次。**一个字节的配置都不写**。
# 位置在 TLS 问答之前，而且办完就 exit——上面那些"站点/证书/管理员"的问答因此
# 一次都不会出现。
function Invoke-UpgradeRollback {
    param([string]$Why)
    Write-Err2 "升级失败：$Why"
    if ($script:UpgradeBackup -and (Test-Path $script:UpgradeBackup)) {
        try {
            Copy-Item -LiteralPath $script:UpgradeBackup -Destination $target -Force
            Stop-ScheduledTask -TaskName "xb-probe-master" -ErrorAction SilentlyContinue
            Start-ScheduledTask -TaskName "xb-probe-master" -ErrorAction SilentlyContinue
            Start-Sleep -Seconds 1
            $st = (Get-ScheduledTask -TaskName "xb-probe-master" -ErrorAction SilentlyContinue).State
            if ($st -eq "Running") {
                Write-Host "    已还原旧二进制并重启任务（状态：$st）。" -ForegroundColor Yellow
            } else {
                Write-Host "    已还原旧二进制，但任务仍未起来——请手工查看：Get-ScheduledTask -TaskName xb-probe-master" -ForegroundColor Red
            }
        } catch {
            Write-Host "    还原失败：$($_.Exception.Message)" -ForegroundColor Red
        }
        Write-Host "    备份仍留在 $script:UpgradeBackup（确认无误后可自行删除）。" -ForegroundColor Yellow
    } else {
        Write-Host "    没有可用的备份（升级前就不存在 $target），未做还原。" -ForegroundColor Yellow
    }
    exit 1
}

if ($UpgradeOnly) {
    Write-Step "只升级 Master 本体"

    if (-not $Bin) {
        Write-Err2 "只升级本体需要指定新二进制：-Bin <新版的 xb-probe-master.exe>。"
        Write-Host "  · 走一键安装脚本的话，install-master.ps1 -UpgradeOnly 会把新二进制带过来；" -ForegroundColor Red
        Write-Host "  · 手工执行时就是上面那条命令。" -ForegroundColor Red
        exit 2
    }
    if (-not (Test-Path -LiteralPath $Bin)) {
        Write-Err2 "-Bin 指向的文件不存在：$Bin"
        exit 2
    }
    $srcFull = (Resolve-Path -LiteralPath $Bin).Path
    $dstFull = $null
    if (Test-Path -LiteralPath $target) { $dstFull = (Resolve-Path -LiteralPath $target).Path }
    if ($dstFull -and $srcFull -eq $dstFull) {
        Write-Host "    源与目标相同（$target）：没有可升级的内容，什么都没做。" -ForegroundColor Yellow
        exit 0
    }
    # 三件事全部在**停任务之前**做完。这是本模式唯一能"在动系统之前失败"的机会：
    # 等停了任务才发现二进制跑不起来，就白断了一次服务。
    $newVer = $null
    try { $newVer = (& $Bin version) 2>&1 | Out-String } catch { $newVer = $null }
    if (-not $newVer) {
        Write-Err2 "$Bin 跑不起来（架构不对？文件没传完？）。"
        # 不用 PS7 的三元运算符（`? :`）：本文件要能在 Windows PowerShell 5.1 上跑。
        $bits = "32"
        if ([Environment]::Is64BitOperatingSystem) { $bits = "64" }
        Write-Host "      换成 Windows $bits 位的那份产物再试。" -ForegroundColor Red
        exit 2
    }
    Write-Host "    新二进制：$Bin"
    Write-Host "    版本：$($newVer.Trim())"
    Write-Host "    不会改动：$cfg、证书、管理员账号、$Data\dist\"

    if ($DryRun) {
        Write-Host "    [dry-run] 停任务 -> 备份 $target -> 拷入 $Bin -> 启动任务 -> 验活"
        Write-Host "==> dry-run：未改动任何东西。" -ForegroundColor Cyan
        exit 0
    }

    $script:UpgradeBackup = "$target.bak"
    if (Test-Path -LiteralPath $target) {
        try {
            Copy-Item -LiteralPath $target -Destination $script:UpgradeBackup -Force -ErrorAction Stop
        } catch {
            Write-Err2 "备份现有二进制到 $script:UpgradeBackup 失败，已中止（不升级没有退路的二进制）。"
            exit 1
        }
        Write-Host "    已备份旧二进制到 $script:UpgradeBackup"
    }

    $taskName = "xb-probe-master"
    $hasTask = [bool](Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue)
    if ($hasTask) { Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue }

    try {
        Copy-Item -LiteralPath $Bin -Destination $target -Force -ErrorAction Stop
    } catch {
        Invoke-UpgradeRollback "写入 $target 失败：$($_.Exception.Message)"
    }
    Write-Host "    已写入 $target"

    if (-not $hasTask) {
        Write-Warn2 "本机没有 $taskName 计划任务（没装过？或用了 -NoStartupTask），不会替你重启。"
        Write-Host "      新二进制已在位，下次启动就是新版本。"
        Write-Host "==> 升级完成" -ForegroundColor Green
        exit 0
    }
    try {
        Start-ScheduledTask -TaskName $taskName -ErrorAction Stop
    } catch {
        Invoke-UpgradeRollback "启动计划任务失败：$($_.Exception.Message)"
    }
    # Start-ScheduledTask 一返回就认为"成功"，而进程可能**立刻**因为别的原因退出
    # （端口被占、配置里的键不认识）。多等一秒再确认一次，把"根本没起来"抓在
    # 回滚点上——判据要挑"真的可用"，不挑"命令没报错"。
    Start-Sleep -Seconds 1
    $st = (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue).State
    if ($st -ne "Running") {
        Invoke-UpgradeRollback "新二进制启动后没有跑起来（状态：$st；端口被占或配置不匹配都会这样）"
    }

    Write-Host "==> 升级完成" -ForegroundColor Green
    Write-Host "    运行中的版本：$($newVer.Trim())"
    Write-Host "    任务状态：$st"
    Write-Host "    未改动：$cfg、证书、管理员账号、$Data\dist\"
    Write-Host "    旧二进制留在 $script:UpgradeBackup（确认无误后可删）。"
    exit 0
}

# ---------------------------------------------------------------------------
# 0.4) 重置管理员密码 -ResetPassword
# ---------------------------------------------------------------------------
#
# 用户原话：「如果我 web 界面忘记密码，就必须利用脚本卸载重装，所以 master 或者
# 一键部署脚本具备重新设置密码选项」。卸载重装会连数据一起清掉，为了改一个口令
# 付这个代价毫无道理。
#
# 实现只有一步：调 `xb-probe-master reset-admin`。它有 --i-know-what-i-am-doing
# 这道闸，这里**主动传**它——那道闸挡的是"随手敲一条命令误改生产口令"，而本模式
# 是用户明确选了「重置管理员密码」，属于它的正当用途。反过来说，这也意味着本模式
# 不该出现在任何"无人值守的默认路径"里。
if ($ResetPassword) {
    Write-Step "重置管理员密码"
    $rpUser = $AdminUser
    if (-not $rpUser) { $rpUser = "ops" }

    if ($DryRun) {
        Write-Host "    [dry-run] 将会执行：$target reset-admin -config $cfg -username $rpUser"
        Write-Host "    [dry-run] 口令在正式执行时交互输入（或由 -AdminPassword / 环境变量 PROBE_ADMIN_PASSWORD 提供）。"
        Write-Host "==> dry-run：未改动任何东西。" -ForegroundColor Cyan
        exit 0
    }
    if (-not (Test-Path -LiteralPath $target)) {
        Write-Err2 "找不到 $target —— 这台机器上还没有装 Master？首次部署请走安装流程。"
        exit 1
    }
    if (-not (Test-Path -LiteralPath $cfg)) {
        Write-Err2 "读不到 $cfg —— 拿不到配置，不敢继续。"
        exit 1
    }

    # 口令来源优先级与安装流程一致：参数 > 环境变量 > 交互。
    # 交互时问两遍并要求一致——重置口令没有"再输一次确认"的话，打错一个字符就变成
    # "重置成功但登不进去"，而那看起来完全像 bug。
    if (-not $AdminPassword) {
        $sec1 = Read-Host "管理员 $rpUser 的新口令（不回显）" -AsSecureString
        $sec2 = Read-Host "再输一遍确认" -AsSecureString
        $p1 = [Runtime.InteropServices.Marshal]::PtrToStringAuto(
            [Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec1))
        $p2 = [Runtime.InteropServices.Marshal]::PtrToStringAuto(
            [Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec2))
        if ($p1 -ne $p2) {
            Write-Err2 "两次输入的口令不一致，未做任何改动。"
            exit 2
        }
        $AdminPassword = $p1
    }
    if (-not $AdminPassword) {
        Write-Err2 "口令不能为空。"
        exit 2
    }

    # 与安装流程同一个道理：**把输出收进来再自己判退出码**。直接把外部命令的输出
    # 往控制台一扔、只看有没有抛异常的话，很多失败会被当成"完成"。
    $rpOut = ""
    $rpRc = 1
    try {
        $rpOut = (& $target reset-admin -config $cfg -username $rpUser `
                -password $AdminPassword --i-know-what-i-am-doing) 2>&1 | Out-String
        $rpRc = $LASTEXITCODE
        if ($null -eq $rpRc) { $rpRc = 0 }
    } catch {
        $rpOut = "$($_.Exception.Message)"
        $rpRc = 1
    }
    Write-Host ($rpOut -replace "(?m)^", "    ")
    if ($rpRc -ne 0) {
        Write-Err2 "重置口令失败（上面是 xb-probe-master 的输出）。"
        Write-Host "      用户名「$rpUser」不存在时也会这样——可用 -AdminUser 指定。" -ForegroundColor Red
        exit 1
    }
    # 口令不进任何后续输出/历史。
    Remove-Variable AdminPassword -ErrorAction SilentlyContinue
    Write-Host "==> 完成：管理员 $rpUser 的口令已更新，其它任何东西都没动。" -ForegroundColor Green
    Write-Host "    用新口令登录后，建议在「系统设置 -> 安全」里核对 TOTP 状态。"
    exit 0
}

if (-not $Bin -and -not (Test-Path $target)) {
    Write-Err2 "既没有 -Bin，也没有已安装的 $target。"
    exit 2
}

# ---------------------------------------------------------------------------
# 0.5) 交互问答（问什么、什么时候问）
# ---------------------------------------------------------------------------
#
# 放在参数校验**之前**：先把值问全，校验才有东西可校。
# 提示里的默认值与真正生效的值同源（都来自上面那两个常量）。
$TlsModeGiven = [bool]$TlsMode
if (-not $TlsMode) {
    if ($Ask) {
        Write-Host ""
        Write-Host "下面几个问题决定对外访问方式，直接回车取方括号里的默认值。" -ForegroundColor Cyan
        Write-Host ""
        Write-Host "对外访问方式："
        Write-Host "  1) none        不使用 https，只提供纯 HTTP（默认；也适合本机/内网调试）"
        Write-Host "  2) acme        使用 https，证书由 Master 内置 ACME 客户端自动申请"
        Write-Host "                 （需要一个已解析到本机的域名）"
        Write-Host "  3) selfsigned  使用 https，但用自签证书（用纯 IP 访问时，这是唯一可行的方式）"
        Write-Host "  4) files       使用 https，证书与私钥用你已有的文件"
        Write-Host ""
        Write-Host "想让 Agent 直接用 https 连上来，就得在这里选一种；选 1 就只能由前置反代终结 TLS。" -ForegroundColor Gray
    }
    $ans = Read-Answer "选择对外访问方式（1/2/3/4）" "1"
    switch ($ans) {
        "1" { $TlsMode = "none" }
        "2" { $TlsMode = "acme" }
        "3" { $TlsMode = "selfsigned" }
        "4" { $TlsMode = "files" }
        default { $TlsMode = $ans }
    }
}

# 每种 TLS 模式各自的必填项。这里只负责"问"，合法性由下面的校验段统一判。
if ($TlsMode -eq "acme") {
    if (-not $TlsDomain -and -not $TlsIp) {
        $TlsDomain = Read-Required "域名（逗号分隔可多个；必须已解析到本机）"
    }
    if (-not $AcmeEmail) {
        $AcmeEmail = Read-Answer "邮箱（可选，CA 用它发到期提醒）" ""
    }
    if (-not $AcmeChallenge -and $Ask) {
        Write-Host ""
        Write-Host "证书的校验方式（三个前提条件不同，选错了会在申请时才失败）：" -ForegroundColor Cyan
        Write-Host "  1) http-01      本机在 80 端口应答（默认；最省事，需要 80 可达且没被别的服务占着）"
        Write-Host "  2) tls-alpn-01  本机在 443 端口用 ALPN 应答（80 被占用、443 由本进程直接监听时选它）"
        Write-Host "  3) dns-01       在 DNS 服务商处写一条 TXT 记录（支持通配符 *.example.com；需要 API Token）"
    }
    if (-not $AcmeChallenge) {
        $ans = Read-Answer "选择校验方式（1/2/3）" "1"
        switch ($ans) {
            "1" { $AcmeChallenge = "http-01" }
            "2" { $AcmeChallenge = "tls-alpn-01" }
            "3" { $AcmeChallenge = "dns-01" }
            default { $AcmeChallenge = $ans }
        }
    }
    if ($AcmeChallenge -eq "dns-01") {
        if (-not $AcmeDnsProvider) { $AcmeDnsProvider = "cloudflare" }
        if (-not $AcmeDnsToken -and $Ask) {
            Write-Host "  · 建议在 Cloudflare 单独建一个 Token：权限只给 Zone:DNS:Edit 与 Zone:Zone:Read，" -ForegroundColor Gray
            Write-Host "    且只覆盖要签发的那个域名（不要用账户级 Global API Key）。" -ForegroundColor Gray
            $AcmeDnsToken = Read-Secret "Cloudflare API Token"
        }
    }
}

if ($TlsMode -eq "files") {
    if (-not $TlsCert) { $TlsCert = Read-Required "证书文件路径" }
    if (-not $TlsKey) { $TlsKey = Read-Required "私钥文件路径" }
}

if ($TlsMode -eq "selfsigned" -and -not $TlsDomain -and -not $TlsIp) {
    $h = Read-Answer "证书要覆盖的域名/IP（逗号分隔；回车=localhost/127.0.0.1/::1）" ""
    if ($h) {
        if ($h -match "[a-zA-Z]") { $TlsDomain = $h } else { $TlsIp = $h }
    }
}

$PublicListenGiven = [bool]$PublicListen
if (-not $PublicListen) { $PublicListen = $DefaultPublicListen }
if (-not $PublicListenGiven -and $Ask) {
    $v = Read-Answer "公开站监听（端口，或 IP:端口；0.0.0.0 表示所有网卡）" $PublicListen
    $v = Normalize-Listen $v "0.0.0.0"
    if (Test-PortOk $v) { $PublicListen = $v } else { Write-Warn2 "$v 不是合法端口，沿用 $PublicListen" }
}

$AdminListenGiven = [bool]$AdminListen
if (-not $AdminListen) { $AdminListen = $DefaultAdminListen }
if (-not $AdminListenGiven -and $Ask) {
    $v = Read-Answer "管理端监听（端口，或 IP:端口；127.0.0.1 表示只有本机能访问）" $AdminListen
    $v = Normalize-Listen $v "127.0.0.1"
    if (Test-PortOk $v) { $AdminListen = $v } else { Write-Warn2 "$v 不是合法端口，沿用 $AdminListen" }
}

# 公开 IP 只在"真要问 public_origin"时才去探测：它要发一次外部请求，
# 而绝大多数调用（自动化、CI、显式给了地址）根本不需要这个默认值。
if (-not $PublicOrigin -and $Ask) { $PubIpForHint = Get-PublicIp } else { $PubIpForHint = $env:PROBE_PUBLIC_IP }

if (-not $PublicOrigin) {
    $hint = ""
    $pport = Get-PortOf $PublicListen
    if ($TlsMode -eq "acme") {
        if ($TlsDomain) { $hint = Get-OriginDefault "https" (($TlsDomain -split ",")[0].Trim()) $pport }
        elseif ($TlsIp) { $hint = Get-OriginDefault "https" (($TlsIp -split ",")[0].Trim()) $pport }
    } elseif ($TlsMode -eq "none") {
        $host0 = $PubIpForHint
        if (-not $host0) { $host0 = "127.0.0.1" }
        $hint = Get-OriginDefault "http" $host0 $pport
    } else {
        $host0 = $PubIpForHint
        if (-not $host0) { $host0 = "127.0.0.1" }
        $hint = Get-OriginDefault "https" $host0 $pport
    }
    if (-not $Ask -and -not $PubIpForHint) {
        # 非交互时**绝不**用 127.0.0.1 兜底：它会被写进 public_origin，随后成为
        # install.master_url，于是所有 Agent 都去连自己的回环地址——装得成、永远不上报。
        $hint = ""
    }
    $PublicOrigin = Read-Answer "公开访问地址（对外 URL，不带末尾斜杠）" $hint
}

if (-not $AdminOrigin -and $Ask) {
    $AdminOrigin = Read-Answer "管理端地址（回车=与公开站同一个域名，即「同站」）" ""
}

if (-not $AdminUser) {
    if ($Ask) {
        for ($i = 0; $i -lt 3; $i++) {
            $u = Read-Answer "管理员用户名" "ops"
            if (Test-AdminUserOk $u) { $AdminUser = $u; break }
            Write-Host "  （用户名只允许字母、数字与 . _ @ -）" -ForegroundColor Yellow
        }
    } else {
        $AdminUser = "ops"
    }
}
if (-not $AdminUser) { $AdminUser = "ops" }
# 兜底：-AdminUser 那条路径不经过上面的循环。
if (-not (Test-AdminUserOk $AdminUser)) {
    Write-Err2 "管理员用户名只允许字母、数字与 . _ @ -（收到 $AdminUser）。"
    exit 2
}

# ---------------------------------------------------------------------------
# 0.6) TLS 参数校验
#
# 配置文件是**严格解析**的（LoadMaster 用 KnownFields），键名写错会启动即失败；
# 但"值合法、语义不成立"（例如 acme 却没给任何域名/IP）解析得过，要等申请证书
# 才暴露。所以每种模式的前置条件都在这里前置拦下。
# ---------------------------------------------------------------------------
if (@("none", "files", "selfsigned", "acme") -notcontains $TlsMode) {
    Write-Err2 "-TlsMode 只能是 none / files / selfsigned / acme（收到 $TlsMode）。"
    exit 2
}
if ($TlsMode -eq "none" -and ($TlsCert -or $TlsKey -or $TlsDomain -or $TlsIp -or $AcmeEmail)) {
    Write-Err2 "给了证书相关参数却 -TlsMode none。要么把模式写对，要么去掉这些参数——"
    Write-Err2 "      否则证书不会生效，而命令行看起来像「配了 TLS」。"
    exit 2
}
if ($TlsMode -eq "files") {
    if (-not $TlsCert -or -not $TlsKey) {
        Write-Err2 "mode=files 必须同时给 -TlsCert 与 -TlsKey。"
        exit 2
    }
    foreach ($f in @($TlsCert, $TlsKey)) {
        if (-not (Test-Path $f)) { Write-Err2 "找不到证书文件 $f。"; exit 2 }
    }
}
if ($TlsMode -eq "acme") {
    if (-not $TlsDomain -and -not $TlsIp) {
        Write-Err2 "mode=acme 至少要给一个 -TlsDomain 或 -TlsIp。"
        exit 2
    }
    if (@("http-01", "tls-alpn-01", "dns-01") -notcontains $AcmeChallenge) {
        Write-Err2 "-AcmeChallenge 只能是 http-01 / tls-alpn-01 / dns-01。"
        exit 2
    }
    # dns-01 的两条专属前置条件。都放在这里报错，而不是等申请证书时才失败——
    # 那时的错误信息离原因很远，排起来要绕一大圈。
    if ($AcmeChallenge -eq "dns-01") {
        if (-not $TlsDomain) {
            Write-Err2 "challenge=dns-01 只支持域名（DNS 里没有 IP 的挑战记录可写）。"
            Write-Err2 "      请用 -TlsDomain 给出域名，或改用 http-01/tls-alpn-01。"
            exit 2
        }
        if (-not $AcmeDnsProvider) { $AcmeDnsProvider = "cloudflare" }
        if ($AcmeDnsProvider -ne "cloudflare") {
            Write-Err2 "tls.acme.dns_provider 当前仅支持 cloudflare（收到 $AcmeDnsProvider）。"
            exit 2
        }
        if (-not $AcmeDnsToken) {
            Write-Err2 "challenge=dns-01 必须提供 DNS API Token。"
            Write-Err2 "      交互执行时会被提示输入；无人值守请设 `$env:PROBE_ACME_DNS_TOKEN。"
            exit 2
        }
    }
}
# 装了证书却只给 http:// 的对外地址，是最难发现的一种"部署成功"：
# 服务在听 443，访客拿到的是明文，install.master_url 还会把 Agent 指向 http://。
if ($PublicOrigin) {
    if ($TlsMode -eq "none" -and $PublicOrigin.StartsWith("https://")) {
        Write-Warn2 "TLS 方式 none 但公开地址是 https://：若 TLS 由前置反代终结，请把反代网段填进 trusted_proxies，否则 Cookie/HSTS 会按明文处理。"
    } elseif ($TlsMode -ne "none" -and $PublicOrigin.StartsWith("http://")) {
        Write-Err2 "-TlsMode $TlsMode 与 -PublicOrigin $PublicOrigin 不一致。"
        Write-Err2 "      配了证书就应当对外宣告 https://（端口不是 443 时写成 https://host:端口）。"
        exit 2
    }
}

# 逗号清单 → YAML 行内序列：`"a", "b"`。只认逗号，与 .sh 侧一致。
function ConvertTo-YamlList {
    param([string]$Raw)
    $parts = @($Raw -split "," | Where-Object { $_.Trim() } | ForEach-Object { '"' + $_.Trim() + '"' })
    return ($parts -join ", ")
}

# ---------------------------------------------------------------------------
# 0.7) 确认
# ---------------------------------------------------------------------------
if ($Ask) {
    Write-Host ""
    Write-Host "即将执行：" -ForegroundColor Cyan
    $tlsNote = $TlsMode
    if ($TlsMode -eq "acme") { $tlsNote = "$TlsMode（challenge=$AcmeChallenge）" }
    Write-Host "  公开站监听：$PublicListen"
    Write-Host "  管理端监听：$AdminListen"
    Write-Host "  公开地址：$(if ($PublicOrigin) { $PublicOrigin } else { '（留空：按请求 Host 回填）' })"
    Write-Host "  管理地址：$(if ($AdminOrigin) { $AdminOrigin } else { '（与公开站同一个域名）' })"
    Write-Host "  TLS：$tlsNote"
    Write-Host "  管理员：$AdminUser（口令随后输入）"
    Write-Host "  配置目录：$Dir"
    Write-Host "  数据目录：$Data"
    Write-Host "  自启：$(if ($NoStartupTask) { '不注册（-NoStartupTask）' } else { '计划任务 xb-probe-master（开机自启）' })"
    Write-Host ""
    if (-not (Confirm-Yes "继续？")) {
        Write-Host "已取消，未改动系统。" -ForegroundColor Yellow
        exit 0
    }
}

# ---------------------------------------------------------------------------
# render_config：把最终生效的参数渲染成 master.yaml。
# 抽成函数是为了让 -DryRun 打印"将要写入的内容"时走**同一段代码**——
# 预览与落盘各写一份，迟早出现"预览说一套、落盘写另一套"。
# ---------------------------------------------------------------------------
function Get-ConfigLines {
    $lines = New-Object System.Collections.ArrayList
    [void]$lines.Add("# 由 scripts\deploy-master.ps1 生成，完整配置项见 configs\master.yaml.example")
    [void]$lines.Add("public_listen: ""$PublicListen""")
    [void]$lines.Add("admin_listen: ""$AdminListen""")
    [void]$lines.Add("data_dir: ""$Data""")
    [void]$lines.Add("themes_dir: ""$Data\themes""")
    [void]$lines.Add("master_key_file: ""$Dir\master.key""")
    [void]$lines.Add("trusted_proxies: []")
    if ($PublicOrigin) { [void]$lines.Add("public_origin: ""$PublicOrigin""") }
    if ($AdminOrigin) { [void]$lines.Add("admin_origin: ""$AdminOrigin""") }
    # **绝不能**写 install: 段。install.mode / install.master_url / install.dist_base
    # 都是**数据库设置**（repo.GetSettingDefault 读取），不是配置文件字段：
    # MasterConfig 里没有 install 这个键，而 LoadMaster 走**严格解析**（KnownFields）
    # ——写了它会直接启动失败并点名是哪个键。它们的唯一入口是后台「设置 → 安装与下载」。
    # master_url 未设置时会回落到 public_origin，所以给了 -PublicOrigin 就不必再单独设它。
    [void]$lines.Add("tls:")
    switch ($TlsMode) {
        "none" {
            [void]$lines.Add('  mode: "none"               # 由前置 Caddy/Nginx 终结 TLS，或本机/内网调试')
        }
        "files" {
            [void]$lines.Add('  mode: "files"              # 自定义证书；文件变更会热加载，无需重启')
            [void]$lines.Add("  cert_file: ""$TlsCert""")
            [void]$lines.Add("  key_file:  ""$TlsKey""")
        }
        "selfsigned" {
            [void]$lines.Add('  mode: "selfsigned"         # 自签：SAN 覆盖下面的域名与 IP（IP 证书的唯一可用形态）')
            [void]$lines.Add('  selfsigned:')
            $all = @()
            if ($TlsDomain) { $all += $TlsDomain }
            if ($TlsIp) { $all += $TlsIp }
            if ($all.Count -gt 0) {
                [void]$lines.Add("    hosts: [$(ConvertTo-YamlList ($all -join ','))]")
            } else {
                [void]$lines.Add('    hosts: []                 # 留空 = localhost / 127.0.0.1 / ::1')
            }
        }
        "acme" {
            [void]$lines.Add('  mode: "acme"               # 内置 ACME v2 客户端，自动申请与续期')
            [void]$lines.Add('  acme:')
            if ($AcmeEmail) { [void]$lines.Add("    email: ""$AcmeEmail""") }
            [void]$lines.Add("    ca: ""$AcmeCa""")
            [void]$lines.Add("    challenge: ""$AcmeChallenge""")
            if ($AcmeChallenge -eq "dns-01") {
                [void]$lines.Add("    dns_provider: ""$AcmeDnsProvider""")
                # Token 落在 master.yaml（ACL 已收紧到 Administrators/SYSTEM）。
                # 别把这份文件复制进备份目录或贴进工单。
                [void]$lines.Add("    dns_api_token: ""$AcmeDnsToken""")
            }
            if ($TlsDomain) { [void]$lines.Add("    domains: [$(ConvertTo-YamlList $TlsDomain)]") }
            if ($TlsIp) { [void]$lines.Add("    ips: [$(ConvertTo-YamlList $TlsIp)]") }
        }
    }
    [void]$lines.Add('log_level: "info"')
    return $lines
}

# -DryRun：参数校验都做过了，但一个字节都不写。
if ($DryRun) {
    Write-Host ""
    Write-Step "dry-run：以下是将要写入 $cfg 的内容（未改动系统）"
    Write-Host ""
    # 走同一个渲染函数：预览与落盘不可能不一致。
    (Get-ConfigLines) | ForEach-Object { Write-Host $_ }
    Write-Host ""
    Write-Step "dry-run：未创建目录、未安装二进制、未注册计划任务、未初始化管理员。"
    Write-Host "    去掉 -DryRun 即执行（其余参数完全一致）。"
    exit 0
}

# 1) 目录（数据目录收紧 ACL：里面有 master.key 与数据库）
Write-Step "1/6 创建目录"
New-Item -ItemType Directory -Force -Path $Dir | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $Data "themes") | Out-Null
# 只给 Administrators 与 SYSTEM 完全控制，等价于 Linux 上的 0700。
try {
    $acl = Get-Acl $Data
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($who in @("BUILTIN\Administrators", "NT AUTHORITY\SYSTEM")) {
        $rule = New-Object System.Security.AccessControl.FileSystemAccessRule($who, "FullControl", "ContainerInherit,ObjectInherit", "None", "Allow")
        $acl.AddAccessRule($rule)
    }
    Set-Acl -Path $Data -AclObject $acl
} catch {
    Write-Warn2 "收紧数据目录 ACL 失败：$($_.Exception.Message)"
}

# 2) 二进制
Write-Step "2/6 安装二进制"
if ($Bin) {
    if (-not (Test-Path $Bin)) {
        Write-Err2 "找不到 $Bin"
        exit 2
    }
    # 源与目标同一个文件时 Copy-Item 会抛"不能覆盖自身"——加个判断，
    # 否则升级路径上会得到一个与真实原因毫无关系的报错。
    if ((Resolve-Path $Bin).Path -ne (Resolve-Path $target -ErrorAction SilentlyContinue).Path) {
        Copy-Item -Force $Bin $target
    } else {
        Write-Host "    源与目标相同（$Bin），跳过拷贝"
    }
}
& $target version

# 3) 配置
Write-Step "3/6 写配置 $cfg"
if ((Test-Path $cfg) -and -not $ForceConfig) {
    Write-Host "    已存在，保留（要覆盖请加 -ForceConfig）"
} else {
    # 统一写 LF：YAML 解析器对 CRLF 宽容，但配置文件里混着两种换行迟早出怪事。
    $utf8 = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($cfg, (((Get-ConfigLines) -join "`n") + "`n"), $utf8)
    Write-Host "    已写入"
}

# 4) 初始化管理员
Write-Step "4/6 初始化管理员"
if (-not $AdminPassword) {
    $AdminPassword = Read-Secret "管理员 $AdminUser 的口令"
}
if (-not $AdminPassword) {
    Write-Err2 "口令不能为空。"
    exit 2
}
try {
    $out = & $target init-admin -config $cfg -username $AdminUser -password $AdminPassword 2>&1
    Write-Host "    $out"
} catch {
    Write-Host "    （init-admin 未成功：管理员可能已存在；改口令用 reset-admin）" -ForegroundColor Yellow
}
$keyPath = Join-Path $Dir "master.key"
if (Test-Path $keyPath) {
    Write-Host "    请务必把 $keyPath 离线另存一份（不要放进备份目录）" -ForegroundColor Yellow
}

# 5) 开机自启（计划任务）
if ($NoStartupTask) {
    Write-Step "5/6 跳过自启注册"
} else {
    Write-Step "5/6 注册开机自启（计划任务）"
    $taskName = "xb-probe-master"
    $taskArg = "-config ""$cfg"""
    $action = New-ScheduledTaskAction -Execute $target -Argument $taskArg -WorkingDirectory $Dir
    $trigger = New-ScheduledTaskTrigger -AtStartup
    $settings = New-ScheduledTaskSettingsSet -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -StartWhenAvailable -DontStopIfGoingOnBatteries
    Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Settings $settings -User "NT AUTHORITY\SYSTEM" -RunLevel Highest -Force | Out-Null
    try {
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        Start-ScheduledTask -TaskName $taskName
        $st = (Get-ScheduledTask -TaskName $taskName).State
        Write-Host "    已启动（状态：$st）"
    } catch {
        Write-Warn2 "启动计划任务失败：$($_.Exception.Message)"
    }
}

# 6) 健康检查
Write-Step "6/6 健康检查"
# 0.0.0.0 / [::] 是"监听所有网卡"的写法，不是可连接的目的地址——用它去探会得到
# "没探到公开 API"，让人以为服务没起来。
$healthAddr = $PublicListen
if ($healthAddr -like "0.0.0.0:*") { $healthAddr = "127.0.0.1:" + $healthAddr.Substring(8) }
elseif ($healthAddr -like "[::]:*") { $healthAddr = "127.0.0.1:" + $healthAddr.Substring(5) }
elseif ($healthAddr -eq ":::") { $healthAddr = "127.0.0.1" }

# 配了 TLS 就必须用 https 探：明文探一个 TLS 端口永远失败，会白等 20 秒。
# selfsigned / acme 还要跳过证书链校验——这里问的是"进程在不在听"，
# 不是"证书链可不可信"（自签本就不可信；acme 的首张证书是后台按需申请的）。
# 注意：-SkipCertificateCheck 是 PS 6+，这里要兼容 5.1，所以用回调。
$scheme = "http"
if ($TlsMode -ne "none") { $scheme = "https" }
$bypassCert = $false
if ($TlsMode -eq "selfsigned" -or $TlsMode -eq "acme") {
    $bypassCert = $true
    $oldCallback = [System.Net.ServicePointManager]::ServerCertificateValidationCallback
    [System.Net.ServicePointManager]::ServerCertificateValidationCallback = { $true }
}
$ok = $false
for ($i = 0; $i -lt 20; $i++) {
    Start-Sleep -Seconds 1
    try {
        $r = Invoke-WebRequest -Uri "${scheme}://$healthAddr/api/public/v1/site" -UseBasicParsing -TimeoutSec 3
        if ($r.StatusCode -eq 200) {
            $ok = $true
            break
        }
    } catch {
        # 还没起来，继续等
    }
}
if ($bypassCert) {
    [System.Net.ServicePointManager]::ServerCertificateValidationCallback = $oldCallback
}
if ($ok) {
    Write-Host "    公开 API 已就绪" -ForegroundColor Green
} else {
    Write-Warn2 "20 秒内没探到公开 API，请查看 $Data 下的日志"
}

# ---- 证书状态：把"到底成没成"变成一个有结果的报告 ----
#
# 为什么必须在部署脚本里做：ACME 的首张证书是**后台按需申请**的（刻意如此——
# 一个 DNS 问题不该让整个 Master 起不来）。于是"服务已启动"与"证书已就绪"
# 之间有一段没有任何反馈的空窗，用户看到的只是脚本打完最后一段提示就结束。
Write-Step "证书状态"
if ($TlsMode -eq "none") {
    Write-Host "    跳过（tls.mode=none）：证书由前置 Caddy/Nginx 负责"
} else {
    $waitSec = 0
    if ($TlsMode -eq "acme") { $waitSec = 120 }
    if ($env:PROBE_TLS_WAIT) { $waitSec = [int]$env:PROBE_TLS_WAIT }
    if ($waitSec -gt 0) {
        Write-Host "    等待证书就绪（最多 $waitSec 秒；等不到也不影响使用，Master 每 2 分钟自动重试）"
    }
    $certRc = 0
    & $target tls status -config $cfg -wait $waitSec
    if ($LASTEXITCODE -ne 0) { $certRc = $LASTEXITCODE }
    if ($certRc -eq 2) {
        Write-Host "    （证书还没就绪：申请仍在进行中。随时可再查：）"
        Write-Host "      `"$target`" tls status -config `"$cfg`" -wait 120"
    } elseif ($certRc -ne 0) {
        Write-Host "    （证书申请失败。上面列出的排查项每条都能直接执行；改完配置后）"
        Write-Host "      Restart-ScheduledTask -TaskName xb-probe-master 会立即重试。"
    }
}

# ---- 收尾汇总：把"地址到底是什么"直接摆出来 ----
$pubUrl = $PublicOrigin
if (-not $pubUrl) { $pubUrl = "${scheme}://$healthAddr" }
$adminUrl = ""
if ($AdminOrigin) {
    $adminUrl = Get-AdminUrlFrom $AdminOrigin
} else {
    $adminUrl = "${scheme}://$AdminListen/admin/"
}
$tlsLine = "明文（证书由前置反代负责）"
switch ($TlsMode) {
    "files" { $tlsLine = "https（证书来自 $TlsCert）" }
    "selfsigned" { $tlsLine = "https 自签（浏览器会提示不受信任，属预期）" }
    "acme" { $tlsLine = "https（ACME，challenge=$AcmeChallenge）" }
}

Write-Host ""
Write-Host "============================================================" -ForegroundColor Green
Write-Host " 部署完成" -ForegroundColor Green
Write-Host "============================================================" -ForegroundColor Green
Write-Host "  公开看板    $pubUrl"
Write-Host "  管理后台    $adminUrl"
Write-Host "  传输安全    $tlsLine"
Write-Host "============================================================" -ForegroundColor Green
Write-Host ""

Write-Host "接下来：" -ForegroundColor Cyan
Write-Host "  1) 登录管理端 $adminUrl 并启用 TOTP、保存恢复码"
Write-Host "  2) 在「设置 → 安装与下载」核对（配置文件里写它们无效）："
Write-Host "       · Master 公网地址 install.master_url —— 留空则回落到 public_origin"
Write-Host "       · 二进制下载源 install.dist_base  —— 留空则由本进程提供 /dist/"
Write-Host "       · 安装入口模式 install.mode       —— 默认 on_demand"
Write-Host "  3) 生成注册码，用它装第一个 Agent（见 README 的「添加被监控节点」）"
Write-Host "  4) xb-probe-master dist pull --version <版本> --from <发布源> --pubkey <公钥>   # Agent 自升级用"
Write-Host "  5) 公开看板与后台是否分成两个域名 —— 在「设置 → 站点形态」里选："
Write-Host "       跟随配置文件（默认）/ 同站（一个域名）/ 分离（两个域名，需同时填两份域名清单）"
Write-Host "       运行时设置，改完即时生效、不用重启；分离靠 Host 而不是监听地址"
Write-Host "       公开看板顶栏的「管理后台」按钮会跟着这个设置走，不必手改"
Write-Host "  6) 配外部心跳 heartbeat_url，并确认能收到失联通知"
Write-Host "  卸载：powershell -File scripts\deploy-master.ps1 -Uninstall（-KeepData 保留数据）"
if (-not $PublicOrigin) {
    Write-Warn2 "没有给 -PublicOrigin：install.master_url 拿不到值，Master 会按请求的 Host 回填。"
    Write-Warn2 "  走域名访问时正常；但公开站若是明文 HTTP 直连 IP，回填出来的是 https:// —— Agent 会连不上。"
}
if ($DistBase) {
    Write-Warn2 "已记录 -DistBase，但它写不进配置文件（install.* 全是 DB 设置）。"
    Write-Warn2 "  请在「设置 → 安装与下载」把「二进制下载源」填成：$DistBase"
}
