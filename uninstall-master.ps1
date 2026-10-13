# Server Probe —— Master 一键卸载（Windows）
#
# 一条命令把已部署的 Master 卸干净：计划任务、二进制、配置目录（master.yaml +
# master.key）、数据目录（数据库、主题、发布件、证书）。
#
# 用法（**管理员** PowerShell；-DryRun 不需要管理员）：
#
#   # 推荐：先下载再执行（PS 5.1 只认文件开头的 BOM 判编码，下载成文件最稳）
#   irm https://raw.githubusercontent.com/koyoca/XB-Probe/main/uninstall-master.ps1 -OutFile "$env:TEMP\uninstall-master.ps1"
#   powershell -ExecutionPolicy Bypass -File "$env:TEMP\uninstall-master.ps1"
#
#   # 只看会删什么（一个字节都不动）
#   powershell -ExecutionPolicy Bypass -File "$env:TEMP\uninstall-master.ps1" -DryRun
#
#   # 保留数据目录（数据库与 master.key 留着，留着才能重新装上来接着用）
#   powershell -ExecutionPolicy Bypass -File "$env:TEMP\uninstall-master.ps1" -KeepData
#
# 为什么不用 `irm … | iex`：BOM 在管道里会被当成标识符的一部分直接报语法错，
# 而且管道执行拿不到参数。
#
# 删除逻辑只有一份（scripts\deploy-master.ps1 -Uninstall）：本脚本从仓库把它取回来
# 执行。**刻意不在这里自己实现一遍删除**——两份删除逻辑迟早漂移，而这里赌的是
# 用户的系统。也正因如此，本脚本**不需要源码、不需要 Go**，不会因为 -Src 不在、
# 或工具链装不上而卸不了。

param(
    [string]$Repo = $(if ($env:XB_REPO) { $env:XB_REPO } else { "https://github.com/koyoca/XB-Probe.git" }),
    [string]$Ref = $(if ($env:XB_REF) { $env:XB_REF } else { "main" }),
    [switch]$KeepData,
    [switch]$DryRun,
    [switch]$Yes,

    # 以下原样透传给 scripts\deploy-master.ps1
    [string]$Dir = "",
    [string]$Data = ""
)

$ErrorActionPreference = "Stop"

function Write-Step { param([string]$Msg) Write-Host "==> $Msg" -ForegroundColor Cyan }
function Write-Warn2 { param([string]$Msg) Write-Host "警告：$Msg" -ForegroundColor Yellow }
function Write-Err2 { param([string]$Msg) Write-Host "错误：$Msg" -ForegroundColor Red }

# 卸载要注销计划任务、删 ProgramData 下的目录，需要管理员；只有"只看不删"的
# -DryRun 例外——否则"我只想看看会删什么"也得先提权，等于逼人跳过预览。
if (-not $DryRun) {
    $wId = [Security.Principal.WindowsIdentity]::GetCurrent()
    $wPr = New-Object Security.Principal.WindowsPrincipal($wId)
    if (-not $wPr.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        Write-Err2 "卸载需要管理员权限（-DryRun 不需要）。"
        Write-Host "      请用「以管理员身份运行」的 PowerShell 重跑。" -ForegroundColor Red
        exit 1
    }
}

# Get-RawBase：把 GitHub 仓库地址换算成 raw 内容前缀（不带尾斜杠）。推导不出来就
# 返回空——调用方据此"报错并给替代命令"，而不是瞎猜一个地址去下载一个可能不是我们
# 要的脚本。**下载并执行一个来源不明的脚本**是这条链路上最危险的一步，宁可失败也不猜。
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
Write-Step "取部署脚本（$Ref）以执行卸载"
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

# 取回来的**必须**是部署脚本：连 -Uninstall 这个开关都没有就说明拿错了文件，与其
# 拿去执行（未知参数会让它直接报错，甚至做别的事），不如现在就说清楚。
if ((Get-Content -LiteralPath $deploy -Raw) -notmatch "Uninstall") {
    Write-Err2 "$rawUrl 看起来不是部署脚本（没有 -Uninstall 开关）。"
    exit 1
}

# -Yes / PROBE_NO_ASK 也要传下去，否则子脚本还会再问一遍"继续？[y/N]"。
if ($Yes) { $env:PROBE_NO_ASK = "1" }

$dargs = @("-Uninstall")
if ($KeepData) { $dargs += "-KeepData" }
if ($DryRun) { $dargs += "-DryRun" }
if ($Dir) { $dargs += @("-Dir", $Dir) }
if ($Data) { $dargs += @("-Data", $Data) }

Write-Step "开始卸载（接下来的输出来自 scripts\deploy-master.ps1）"
& powershell -ExecutionPolicy Bypass -File $deploy @dargs
$uninstRc = $LASTEXITCODE
Remove-Item $tmpd -Recurse -Force -ErrorAction SilentlyContinue
if ($uninstRc -ne 0) {
    Write-Err2 "卸载脚本执行失败（上面是它的输出）。"
    exit 1
}
exit 0
