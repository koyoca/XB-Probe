# XB-Probe —— Master 整机一键部署（Windows）
#
# 一条命令完成：检查/安装依赖（git、Go）→ 拉取源码 → 编译 → 交互式配置 → 注册开机自启 → 健康检查。
# 幂等：重复执行即"升级"（重新 fetch、重新编译、替换二进制）。
#
# 用法（**管理员** PowerShell）：
#
#   # 推荐：先下载再执行（PS 5.1 只认文件开头的 BOM 判编码，下载成文件最稳）
#   irm https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master.ps1 -OutFile "$env:TEMP\install-master.ps1"
#   powershell -ExecutionPolicy Bypass -File "$env:TEMP\install-master.ps1"
#
# 不建议 `irm ... | iex`：本项目脚本带 param() 块，管道执行拿不到参数，
# 且 BOM 在管道里会被当成标识符的一部分直接报语法错。
#
# 完全非交互：
#   powershell -ExecutionPolicy Bypass -File install-master.ps1 -Yes `
#       -PublicOrigin https://status.example.com -TlsMode acme -TlsDomain status.example.com
#   口令仍由 deploy-master.ps1 交互输入；无人值守时设环境变量 PROBE_ADMIN_PASSWORD。
#
# 卸载（默认连数据一起清；数据目录里有数据库与 master.key）：
#   powershell -ExecutionPolicy Bypass -File install-master.ps1 -Uninstall
#   powershell -ExecutionPolicy Bypass -File install-master.ps1 -Uninstall -KeepData
#   powershell -ExecutionPolicy Bypass -File install-master.ps1 -Uninstall -DryRun
#
# 只升级 Master 本体（照旧编译，但**不再问**站点/证书/管理员那一套）：
#   powershell -ExecutionPolicy Bypass -File install-master.ps1 -UpgradeOnly
#
# 只重置管理员密码（不卸载、不清数据、不重写配置）：
#   powershell -ExecutionPolicy Bypass -File install-master.ps1 -ResetPassword
#   口令按 $env:PROBE_ADMIN_PASSWORD / 交互的先后取（也可先给 deploy-master.ps1
#   的 -AdminPassword）；用户名默认 ops，可用 -AdminUser 指定。
#
# 卸载与重置密码**不碰源码与 Go**：它们从仓库取回 scripts\deploy-master.ps1 执行
# -Uninstall / -ResetPassword，所以不需要先装一套工具链，也不会因为 -Src 不在而失败。
#
# ⚠ 与 shell 入口（install-master.sh）的一处**有意的不对称**：那边会先弹一张
# 「1 安装 / 2 卸载 / 3 只更新发布文件 / 4 只升级本体 / 5 重置密码」的菜单，本脚本
# **没有菜单**——上面那两个新模式只走命令行开关。原因是菜单里的第 3 项
# （只更新 Agent 发布文件）在 Windows 侧还没有对应实现，现在就把菜单搬过来，用户
# 答 3 会被引导到"什么也没做"或"当成安装"，那比没有菜单更糟。等第 3 项补齐再搬。
#
# 问答分两处，不是漏了：TLS 方式、对外地址、管理员账号在这里问（排在装依赖与编译
# 之前，答案错了就不必动系统）；**监听端口**由 scripts\deploy-master.ps1 在落盘前问。
# 端口的默认值只有部署脚本那一份来源，跟着它问才不会出现"提示写 8080、实际听 8081"。
#
# dns-01 的 DNS 凭据：用环境变量 PROBE_ACME_DNS_PROVIDER / PROBE_ACME_DNS_TOKEN，
# 或由 scripts\deploy-master.ps1 交互提示（Token 不回显）。

param(
    [string]$Repo = $(if ($env:XB_REPO) { $env:XB_REPO } else { "https://github.com/koyoca/XB-Probe.git" }),
    [string]$Ref = $(if ($env:XB_REF) { $env:XB_REF } else { "main" }),
    [string]$Src = $(if ($env:XB_SRC) { $env:XB_SRC } else { "C:\ProgramData\xb-probe" }),
    [string]$GoVersion = $(if ($env:XB_GO_VERSION) { $env:XB_GO_VERSION } else { "1.27.2" }),
    [string]$GoProxy = $env:XB_GOPROXY,
    [switch]$NoDeps,
    [switch]$Yes,
    [switch]$DryRun,
    [switch]$Uninstall,
    [switch]$KeepData,
    # 只升级 Master 本体：照旧取源码 + 编译（本脚本的意义就在这），但**不再问**
    # 站点/证书/管理员那一套，编译完只把二进制换上去。
    [switch]$UpgradeOnly,
    # 只重置管理员密码：不需要源码、不需要 Go，所以和卸载一样走"编译之前"的早退通路。
    [switch]$ResetPassword,

    # 以下原样透传给 scripts\deploy-master.ps1
    [string]$Dir = "",
    [string]$Data = "",
    [string]$PublicListen = "",
    [string]$AdminListen = "",
    [string]$PublicOrigin = "",
    [string]$AdminOrigin = "",
    [string]$AdminUser = "",
    [string]$DistBase = "",
    [string]$TlsMode = "",
    [string]$TlsCert = "",
    [string]$TlsKey = "",
    [string]$TlsDomain = "",
    [string]$TlsIp = "",
    [string]$AcmeEmail = "",
    [string]$AcmeCa = "",
    [string]$AcmeChallenge = "",
    [switch]$NoStartupTask,
    [switch]$ForceConfig
)

$ErrorActionPreference = "Stop"

# go.mod 声明的最低 Go 版本，低于它构建会直接失败，所以在依赖检查阶段就拦下来。
$MinGo = "1.26"

function Write-Step { param([string]$Msg) Write-Host "==> $Msg" -ForegroundColor Cyan }
function Write-Warn2 { param([string]$Msg) Write-Host "警告：$Msg" -ForegroundColor Yellow }
function Write-Err2 { param([string]$Msg) Write-Host "错误：$Msg" -ForegroundColor Red }

function Test-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    $pr = New-Object Security.Principal.WindowsPrincipal($id)
    return $pr.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Get-GoExe {
    $c = Get-Command go -ErrorAction SilentlyContinue
    if ($c) { return $c.Source }
    if (Test-Path "C:\go\bin\go.exe") { return "C:\go\bin\go.exe" }
    return $null
}

# 版本按 major.minor 比较即可（go.mod 只约束到 1.x 这一级）。
function Test-GoVersionOk {
    param([string]$Have, [string]$Want)
    try {
        $h = [version]($Have -replace "[^0-9.].*$", "")
        $w = [version]($Want -replace "[^0-9.].*$", "")
    } catch { return $false }
    if ($h.Major -gt $w.Major) { return $true }
    if ($h.Major -lt $w.Major) { return $false }
    return ($h.Minor -ge $w.Minor)
}

# 装 Go：官方 zip 解到 C:\go（与官方 MSI 的默认布局一致）。
# 只在系统里没有可用 Go 时才调用；已有 C:\go 会先改名归档，不直接删。
function Install-Go {
    $arch = "amd64"
    $pa = $env:PROCESSOR_ARCHITECTURE
    if ($pa -eq "ARM64") { $arch = "arm64" }
    elseif ($pa -eq "x86") { $arch = "386" }

    $zip = Join-Path $env:TEMP "go$GoVersion.windows-$arch.zip"
    $urls = @(
        "https://golang.google.cn/dl/go$GoVersion.windows-$arch.zip",
        "https://go.dev/dl/go$GoVersion.windows-$arch.zip",
        "https://mirrors.aliyun.com/golang/go$GoVersion.windows-$arch.zip"
    )
    $ok = $false
    foreach ($u in $urls) {
        Write-Step "下载 Go $GoVersion（$u）"
        try {
            Invoke-WebRequest -Uri $u -OutFile $zip -UseBasicParsing -TimeoutSec 300
            if ((Get-Item $zip).Length -gt 10MB) { $ok = $true; break }
        } catch {
            Write-Warn2 "$u 不可用，换下一个源"
        }
    }
    if (-not $ok) {
        Write-Host "错误：三个源都没取到 Go $GoVersion。请手动安装 Go $MinGo+ 后重跑（或用 -GoVersion 指定存在的版本）。" -ForegroundColor Red
        exit 1
    }

    # Expand-Archive 不能覆盖已存在的文件，所以先解到临时目录再搬。
    $tmp = Join-Path $env:TEMP ("xbgo-" + [Guid]::NewGuid().ToString("N").Substring(0, 8))
    New-Item -ItemType Directory -Force -Path $tmp | Out-Null
    Expand-Archive -Path $zip -DestinationPath $tmp -Force
    if (Test-Path "C:\go") {
        Rename-Item "C:\go" ("C:\go.bak." + (Get-Date -Format "yyyyMMddHHmmss")) -Force
    }
    Move-Item (Join-Path $tmp "go") "C:\go"
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
    Remove-Item $zip -Force -ErrorAction SilentlyContinue
    $env:Path = "C:\go\bin;" + $env:Path
    Write-Step "已安装 Go（C:\go\bin，仅本次会话生效；要长期生效请把它加进系统 PATH）"
    return "C:\go\bin\go.exe"
}

# ---------------------------------------------------------------------------
# 卸载 / 重置密码：-Uninstall / -ResetPassword（排在装依赖与编译之前）
# ---------------------------------------------------------------------------
#
# 这两个模式都不需要源码、不需要 Go，所以必须排在"装依赖 / clone / 编译"**之前**
# ——否则"卸个东西"或"改个口令"要先装一套工具链，是本末倒置，而且一旦工具链装不上
# 就什么都做不了。
#
# 逻辑只有一份（scripts\deploy-master.ps1 的 -Uninstall / -ResetPassword），这里从
# 仓库把它取回来执行。**刻意不自己实现一遍**：两份逻辑迟早漂移，而这里赌的是用户的
# 系统。
#
# -UpgradeOnly **不在**这一段里：它要编译，所以走不了早退通路（见下面第 6 步）。
#
# 三个单次模式在入口这里**也必须互斥**：下面那段早退通路是"自己拼一串参数"，所以
# 一旦同时给了两个，被丢掉的那个**永远不会到达部署脚本**——而部署脚本的互斥判断
# （0.1c）本来是能抓住这种组合的。少了这一段判断，`-UpgradeOnly -Uninstall` 会
# 安安静静地卸载，用户以为自己在升级本体。
$oneShotIn = @()
if ($Uninstall) { $oneShotIn += "-Uninstall" }
if ($UpgradeOnly) { $oneShotIn += "-UpgradeOnly" }
if ($ResetPassword) { $oneShotIn += "-ResetPassword" }
if ($oneShotIn.Count -gt 1) {
    Write-Err2 "这几项都只做一件事，不能同时使用：$($oneShotIn -join " ")"
    Write-Host "  「卸载」「只升级本体」「重置密码」一次只能选一个。" -ForegroundColor Red
    exit 2
}

if ($Uninstall -or $ResetPassword) {
    $earlyWhat = "卸载"
    if ($ResetPassword) { $earlyWhat = "重置管理员密码" }

    # 两者都要管理员：卸载要注销计划任务、删 ProgramData 下的目录；重置密码要写
    # 数据目录里的数据库。只有"只看不删"的 -DryRun 例外。
    if (-not $DryRun -and -not (Test-Admin)) {
        Write-Err2 "$earlyWhat需要管理员权限（-DryRun 不需要）。"
        Write-Host "      请用「以管理员身份运行」的 PowerShell 重跑。" -ForegroundColor Red
        exit 1
    }

    # Get-RawBase：把 GitHub 仓库地址换算成 raw 内容前缀（不带尾斜杠）。推导不出来
    # 就返回空——调用方据此"报错并给替代命令"，而不是瞎猜一个地址去下载一个可能不是
    # 我们要的脚本。**下载并执行一个来源不明的脚本**是这条链路上最危险的一步，
    # 宁可失败也不猜。
    function Get-RawBase {
        param([string]$RepoUrl, [string]$GitRef)
        if ($RepoUrl -notmatch "^https://github\.com/") { return "" }
        $r = $RepoUrl -replace "^https://github\.com/", ""
        $r = $r -replace "\.git$", ""
        if ($r -notmatch "^[^/]+/[^/]+$") { return "" }
        return "https://raw.githubusercontent.com/$r/$GitRef"
    }

    $rawBase = Get-RawBase $Repo $Ref
    if (-not $rawBase) {
        Write-Err2 "-Repo 不是 GitHub 地址（$Repo），推导不出脚本下载地址。"
        Write-Host "      请改用：" -ForegroundColor Red
        Write-Host '      $u = "https://raw.githubusercontent.com/koyoca/XB-Probe/main/uninstall-master.ps1"' -ForegroundColor Red
        Write-Host '      irm $u -OutFile "$env:TEMP\uninstall-master.ps1"' -ForegroundColor Red
        Write-Host '      powershell -ExecutionPolicy Bypass -File "$env:TEMP\uninstall-master.ps1"' -ForegroundColor Red
        exit 2
    }

    $rawUrl = "$rawBase/scripts/deploy-master.ps1"
    Write-Step "取部署脚本（$Ref）以执行$earlyWhat"
    $tmpd = Join-Path $env:TEMP ("xb-uninst-" + [Guid]::NewGuid().ToString("N").Substring(0, 8))
    New-Item -ItemType Directory -Force -Path $tmpd | Out-Null
    $deploy = Join-Path $tmpd "deploy-master.ps1"
    $got = $false
    try {
        Invoke-WebRequest -Uri $rawUrl -OutFile $deploy -UseBasicParsing -TimeoutSec 120
        $got = $true
    } catch {
        Write-Warn2 "$rawUrl 取不到（$($_.Exception.Message)）"
    }
    if (-not $got -or -not (Test-Path $deploy) -or (Get-Item $deploy).Length -eq 0) {
        Write-Err2 "取不到 $rawUrl（网络？-Ref 写错了？）"
        exit 1
    }

    # 取回来的**必须**是部署脚本：连 -Uninstall 这个开关都没有就说明拿错了文件，
    # 与其拿去执行（未知参数会让它直接报错，甚至做别的事），不如现在就说清楚。
    $deployText = Get-Content -LiteralPath $deploy -Raw
    if ($deployText -notmatch "Uninstall") {
        Write-Err2 "$rawUrl 看起来不是部署脚本（没有 -Uninstall 开关）。"
        exit 1
    }
    # 同理，-ResetPassword 也要先探一下对面认不认识：本脚本来自仓库当前 ref，而
    # 部署脚本从同一 ref 取——正常情况下两者同源。用户把 -Ref 指到某个旧标签时就不
    # 同源了，那时 PowerShell 会抛一句「找不到与参数名 ResetPassword 匹配的参数」，
    # 那句话既不说明原因、也不说明"这次什么都没做"。先探再传，报错才说得清。
    if ($ResetPassword -and $deployText -notmatch "ResetPassword") {
        Write-Err2 "取回的部署脚本不认识 -ResetPassword（大概 -Ref 指到了较早的提交）。"
        Write-Host "      这次什么都没做。请改用 -Ref main，或先升级仓库再重跑。" -ForegroundColor Red
        exit 2
    }

    # 非交互（-Yes）也要传下去，否则子脚本还会再问一遍"继续？[y/N]"。
    # 用环境变量而不是命令行开关：取回来的部署脚本可能来自用户指定的旧 ref，
    # 而旧版本对未知参数是硬失败，环境变量则新旧都能收。
    if ($Yes) { $env:PROBE_NO_ASK = "1" }

    if ($ResetPassword) {
        $dargs = @("-ResetPassword")
    } else {
        $dargs = @("-Uninstall")
        if ($KeepData) { $dargs += "-KeepData" }
    }
    if ($DryRun) { $dargs += "-DryRun" }
    if ($Dir) { $dargs += @("-Dir", $Dir) }
    if ($Data) { $dargs += @("-Data", $Data) }
    if ($ResetPassword -and $AdminUser) { $dargs += @("-AdminUser", $AdminUser) }

    Write-Step "开始$earlyWhat（接下来的输出来自 scripts\deploy-master.ps1）"
    & powershell -ExecutionPolicy Bypass -File $deploy @dargs
    $uninstRc = $LASTEXITCODE
    Remove-Item $tmpd -Recurse -Force -ErrorAction SilentlyContinue
    if ($uninstRc -ne 0) {
        Write-Err2 "$earlyWhat 失败（上面是它的输出）。"
        exit 1
    }
    exit 0
}

# ---------------------------------------------------------------------------
# 0) 前提
# ---------------------------------------------------------------------------

# -DryRun 一个字节都不写，所以也不需要管理员——否则"先看看它会做什么"这件
# 唯一不需要信任就能做的事，反而成了最麻烦的一件。
if (-not (Test-Admin) -and -not $NoStartupTask -and -not $DryRun) {
    Write-Host "错误：请用「以管理员身份运行」的 PowerShell 执行。" -ForegroundColor Red
    Write-Host "      （只编译不注册服务时，可以加 -NoStartupTask 后以普通用户运行）" -ForegroundColor Red
    exit 1
}

if (-not $Yes -and [Environment]::UserInteractive -eq $false) {
    Write-Host "错误：当前会话无法交互提问。请加 -Yes 并显式给出参数，或在交互式 PowerShell 里运行。" -ForegroundColor Red
    exit 2
}

# ---------------------------------------------------------------------------
# 1) 交互问答（放在装依赖之前：答错了就没必要动系统）
# ---------------------------------------------------------------------------
#
# -UpgradeOnly 把这一整段（TLS 方式、对外地址、管理员账号）连同下面的"2) 确认"
# 一起跳过：它只换二进制，站点、证书、端口、管理员**一个都不动**，所以问这些
# 既是白问、又容易让人以为"改了什么"。用户原话：
#   「他会全部走一遍流程，包括站点设置、证书申请、管理员账号设置等全部流程，
#     但是这些我不想做，我仅仅只想升级 master 本体」。
#
# 这段体量大，包进 if 之后**刻意不整体缩进一格**：那会让本次改动看起来动了全文
# 每一行，日后 diff 再也读不出"这次到底改了哪几处"。首尾各留一条显眼的分隔注释。
# ==== 站点/证书/管理员问答 开始（-UpgradeOnly 时整段跳过）====
if (-not $UpgradeOnly) {

if (-not $Yes) {
    Write-Host ""
    Write-Host "下面几个问题决定对外访问方式，直接回车取方括号里的默认值。" -ForegroundColor Cyan
    Write-Host "  TLS 方式：1) none 交给前置反代（默认） 2) acme 自动申请 3) selfsigned 自签 4) files 自有证书" -ForegroundColor Gray
}

if (-not $TlsMode) {
    $ans = if ($Yes) { "1" } else { Read-Host "选择 TLS 方式（1/2/3/4，默认 1）" }
    if (-not $ans) { $ans = "1" }
    switch ($ans) {
        "1" { $TlsMode = "none" }
        "2" { $TlsMode = "acme" }
        "3" { $TlsMode = "selfsigned" }
        "4" { $TlsMode = "files" }
        default { $TlsMode = $ans }
    }
}
if (@("none", "acme", "selfsigned", "files") -notcontains $TlsMode) {
    Write-Host "错误：-TlsMode 只能是 none / acme / selfsigned / files（收到 $TlsMode）。" -ForegroundColor Red
    exit 2
}

if ($TlsMode -eq "acme" -and -not $TlsDomain -and -not $TlsIp) {
    if (-not $Yes) { $TlsDomain = Read-Host "域名（逗号分隔可多个；必须已解析到本机）" }
    if (-not $TlsDomain -and -not $TlsIp) {
        Write-Host "错误：ACME 至少要有一个域名或 IP（否则没有可申请的标识符）。" -ForegroundColor Red
        exit 2
    }
}
if ($TlsMode -eq "acme" -and -not $AcmeEmail -and -not $Yes) {
    $AcmeEmail = Read-Host "邮箱（可选，CA 用它发到期提醒）"
}

if ($TlsMode -eq "files") {
    if (-not $TlsCert -and -not $Yes) { $TlsCert = Read-Host "证书文件路径" }
    if (-not $TlsKey -and -not $Yes) { $TlsKey = Read-Host "私钥文件路径" }
    foreach ($f in @($TlsCert, $TlsKey)) {
        if (-not $f -or -not (Test-Path $f)) {
            Write-Host "错误：找不到文件 $f。" -ForegroundColor Red
            exit 2
        }
    }
}

if ($TlsMode -eq "selfsigned" -and -not $TlsDomain -and -not $TlsIp) {
    if (-not $Yes) {
        $h = Read-Host "证书要覆盖的域名/IP（逗号分隔；回车=localhost/127.0.0.1/::1）"
        if ($h) {
            if ($h -match "[a-zA-Z]") { $TlsDomain = $h } else { $TlsIp = $h }
        }
    }
}

if (-not $PublicOrigin) {
    $hint = ""
    if ($TlsMode -eq "acme") {
        if ($TlsDomain) { $hint = "https://" + ($TlsDomain -split ",")[0] }
        elseif ($TlsIp) { $hint = "https://" + ($TlsIp -split ",")[0] }
    }
    # 非交互且猜不出域名时**不填默认值**：写个 127.0.0.1 进 public_origin，
    # 它会成为 install.master_url，所有 Agent 都会去连自己的回环地址。
    if (-not $hint -and -not $Yes) {
        if ($TlsMode -eq "none") { $hint = "http://<公网IP或域名>:8080" } else { $hint = "https://<公网IP或域名>:8080" }
    }
    if (-not $Yes) { $PublicOrigin = Read-Host "公开访问地址（对外 URL，不带末尾斜杠）[$hint]" }
    if (-not $PublicOrigin -and -not $Yes) { $PublicOrigin = $hint }
}

if (-not $AdminOrigin -and -not $Yes) {
    $AdminOrigin = Read-Host "管理端地址（回车=与公开站同一个域名，即「同站」）"
}

if (-not $AdminUser) {
    if ($Yes) { $AdminUser = "ops" } else { $AdminUser = Read-Host "管理员用户名 [ops]" }
    if (-not $AdminUser) { $AdminUser = "ops" }
}

# ---------------------------------------------------------------------------
# 2) 确认
# ---------------------------------------------------------------------------

if (-not $Yes) {
    Write-Host ""
    Write-Host "即将执行：" -ForegroundColor Cyan
    Write-Host "  源码      $Src（git clone $Repo，分支 $Ref）"
    Write-Host "  公开地址  $PublicOrigin"
    Write-Host "  管理地址  $(if ($AdminOrigin) { $AdminOrigin } else { '与公开地址相同' })"
    Write-Host "  管理员    $AdminUser（口令随后交互输入）"
    Write-Host "  TLS       $TlsMode"
    Write-Host "  服务      $(if ($NoStartupTask) { '不注册开机自启' } else { '计划任务 xb-probe-master（开机自启）' })"
    Write-Host ""
    $go = Read-Host "继续？[Y/n]"
    if ($go -match "^(n|N|no|NO)$") {
        Write-Host "已取消，未改动系统。" -ForegroundColor Yellow
        exit 0
    }
}
}
# ==== 站点/证书/管理员问答 结束（-UpgradeOnly 时整段跳过）====

# -UpgradeOnly 不问上面那些，但第 6 步要把 -AdminUser 透传下去（重置密码/管理员相关），
# 而它在那条路上一直是空的。默认值与部署脚本一致（ops），不另立一个。
if ($UpgradeOnly -and -not $AdminUser) { $AdminUser = "ops" }

# ---------------------------------------------------------------------------
# 3) 依赖
# ---------------------------------------------------------------------------

if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
    if ($NoDeps) {
        Write-Host "错误：缺少 git，且指定了 -NoDeps。请先安装：winget install --id Git.Git" -ForegroundColor Red
        exit 1
    }
    Write-Step "缺少 git，尝试用 winget 安装"
    if (Get-Command winget -ErrorAction SilentlyContinue) {
        winget install --id Git.Git -e --source winget --accept-package-agreements --accept-source-agreements
        $env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")
    } else {
        Write-Host "错误：没有 winget，请手动安装 git 后重跑。" -ForegroundColor Red
        exit 1
    }
    if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
        Write-Host "错误：git 装好了但当前会话的 PATH 还没刷新，请重开一个 PowerShell 再跑。" -ForegroundColor Red
        exit 1
    }
}

$goExe = Get-GoExe
$goOk = $false
if ($goExe) {
    $goVer = (& $goExe version) -replace "go version go", "" -replace " .*$", ""
    if (Test-GoVersionOk -Have $goVer -Want $MinGo) {
        $goOk = $true
        Write-Step "使用系统 Go $goVer"
    } else {
        Write-Warn2 "系统 Go 是 $goVer，低于项目要求的 $MinGo"
    }
}
if (-not $goOk) {
    if ($DryRun) {
        Write-Step "（dry-run）将安装 Go $GoVersion 到 C:\go"
        $goExe = "C:\go\bin\go.exe"
    } else {
        $goExe = Install-Go
    }
}

if ($GoProxy) {
    $env:GOPROXY = $GoProxy
} elseif ($goExe -and (Test-Path $goExe)) {
    $cur = (& $goExe env GOPROXY) 2>$null
    if ($cur -eq "https://proxy.golang.org,direct") {
        # 只覆盖"官方默认值"这一种情况：用户自己配过代理就绝不动它。
        $env:GOPROXY = "https://goproxy.cn,direct"
        Write-Step "GOPROXY 是官方默认值，本次改用国内镜像（想固定下来用 go env -w GOPROXY=…）"
    }
}

# ---------------------------------------------------------------------------
# 4) 取源码 / 5) 编译
# ---------------------------------------------------------------------------

if ($DryRun) {
    Write-Step "（dry-run）取源码：$Repo（$Ref）→ $Src"
    Write-Step "（dry-run）编译：cd $Src; CGO_ENABLED=0 go build -trimpath -o bin\xb-probe-master.exe .\cmd\xb-probe-master"
    if ($UpgradeOnly) {
        Write-Step "（dry-run）部署：scripts\deploy-master.ps1 -Bin $Src\bin\xb-probe-master.exe -UpgradeOnly"
        Write-Step "（dry-run）只换 Master 二进制并重启任务；配置、证书、管理员账号、数据目录都不动。"
    } else {
        Write-Step "（dry-run）部署：scripts\deploy-master.ps1 -Bin $Src\bin\xb-probe-master.exe …"
    }
    Write-Step "（dry-run）以上为计划，未改动系统。"
    exit 0
}

if (Test-Path (Join-Path $Src ".git")) {
    Write-Step "更新已有源码 $Src（丢弃该目录内的本地改动）"
    & git -C $Src fetch --depth 1 origin $Ref
    if ($LASTEXITCODE -ne 0) { Write-Host "错误：git fetch 失败，检查网络或 -Repo/-Ref。" -ForegroundColor Red; exit 1 }
    & git -C $Src reset --hard --quiet FETCH_HEAD
    # 清掉上一版遗留的未跟踪文件；不动 bin\（上一版二进制留着，编译失败还能回滚）。
    & git -C $Src clean -fdq -e bin
} else {
    if ((Test-Path $Src) -and (Get-ChildItem $Src -Force | Select-Object -First 1)) {
        Write-Host "错误：$Src 已存在、非空、且不是 git 仓库。请换 -Src，或先清空它。" -ForegroundColor Red
        exit 2
    }
    Write-Step "克隆源码到 $Src"
    New-Item -ItemType Directory -Force -Path (Split-Path $Src -Parent) | Out-Null
    & git clone --depth 1 --branch $Ref $Repo $Src
    if ($LASTEXITCODE -ne 0) { Write-Host "错误：git clone 失败，检查网络与仓库地址（-Repo）。" -ForegroundColor Red; exit 1 }
}

$deployPs1 = Join-Path $Src "scripts\deploy-master.ps1"
if (-not (Test-Path $deployPs1)) {
    Write-Host "错误：$Src 里没有 scripts\deploy-master.ps1——源码不完整，或 -Ref 指到了早期提交。" -ForegroundColor Red
    exit 1
}

$ver = (& git -C $Src describe --tags --always) 2>$null
if (-not $ver) { $ver = "dev" }
Write-Step "编译 xb-probe-master（版本 $ver）"
Push-Location $Src
try {
    $env:CGO_ENABLED = "0"
    & $goExe build -trimpath -ldflags "-s -w -X main.version=$ver" -o "bin\xb-probe-master.exe" ".\cmd\xb-probe-master"
    $buildRc = $LASTEXITCODE
} finally {
    Pop-Location
}
if ($buildRc -ne 0) {
    Write-Host "错误：编译失败。" -ForegroundColor Red
    Write-Host "      若是拉依赖超时/被墙，试：-GoProxy https://goproxy.cn,direct" -ForegroundColor Red
    Write-Host "      若是 Go 版本问题，试：-GoVersion 1.27.2" -ForegroundColor Red
    exit 1
}
$binPath = Join-Path $Src "bin\xb-probe-master.exe"
Write-Step "已生成 $binPath"

# ---------------------------------------------------------------------------
# 6) 交给 deploy-master.ps1 完成落盘、自启与健康检查
# ---------------------------------------------------------------------------

$forward = @{ Bin = $binPath }
if ($UpgradeOnly) { $forward["UpgradeOnly"] = $true }
if ($Dir) { $forward["Dir"] = $Dir }
if ($Data) { $forward["Data"] = $Data }
if ($PublicListen) { $forward["PublicListen"] = $PublicListen }
if ($AdminListen) { $forward["AdminListen"] = $AdminListen }
if ($PublicOrigin) { $forward["PublicOrigin"] = $PublicOrigin }
if ($AdminOrigin) { $forward["AdminOrigin"] = $AdminOrigin }
if ($AdminUser) { $forward["AdminUser"] = $AdminUser }
if ($DistBase) { $forward["DistBase"] = $DistBase }
if ($TlsMode) { $forward["TlsMode"] = $TlsMode }
if ($TlsCert) { $forward["TlsCert"] = $TlsCert }
if ($TlsKey) { $forward["TlsKey"] = $TlsKey }
if ($TlsDomain) { $forward["TlsDomain"] = $TlsDomain }
if ($TlsIp) { $forward["TlsIp"] = $TlsIp }
if ($AcmeEmail) { $forward["AcmeEmail"] = $AcmeEmail }
if ($AcmeCa) { $forward["AcmeCa"] = $AcmeCa }
if ($AcmeChallenge) { $forward["AcmeChallenge"] = $AcmeChallenge }
if ($NoStartupTask) { $forward["NoStartupTask"] = $true }
if ($ForceConfig) { $forward["ForceConfig"] = $true }

Write-Step "开始部署（接下来的输出来自 scripts\deploy-master.ps1）"
& powershell -ExecutionPolicy Bypass -File $deployPs1 @forward

$adminUrl = if ($AdminOrigin) { $AdminOrigin } else { $PublicOrigin }
Write-Host ""
Write-Host "==> 完成。" -ForegroundColor Green
Write-Host "  · 管理端：$adminUrl/admin/"
Write-Host "  · 源码留在 $Src：要升级就再跑一次本脚本（自动 fetch + 重编译 + 换二进制），"
Write-Host "    或者只重跑部署：powershell -File `"$deployPs1`" -Bin `"$binPath`""
