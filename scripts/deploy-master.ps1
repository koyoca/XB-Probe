# Server Probe —— Master 一键部署（Windows）
#
# 覆盖：放二进制 → 写配置并初始化管理员 → 注册开机自启（计划任务）→ 健康检查。
# 幂等：重复执行走「升级」路径（停任务 → 换二进制 → 启动）。
#
# 用法（管理员 PowerShell）：
#   powershell -ExecutionPolicy Bypass -File scripts\deploy-master.ps1 -Bin bin\probe-master.exe -AdminUser ops
#
# 不传 -AdminPassword 时会交互式提示（推荐）。
#
# 说明：Master 在 Windows 上没有内置的服务封装（Agent 才有），这里用
# 「开机时以最高权限运行」的计划任务实现自启；需要真正的服务形态（可 Restart-Service）
# 时请用 NSSM / WinSW 包一层，命令行仍是一句：probe-master -config <配置文件>

param(
    [string]$Bin = "",
    [string]$Dir = "C:\ProgramData\probe-master",
    [string]$Data = "C:\ProgramData\probe-master\data",
    [string]$PublicListen = "0.0.0.0:8080",
    [string]$AdminListen = "127.0.0.1:8081",
    [string]$PublicOrigin = "",
    [string]$AdminOrigin = "",
    [string]$DistBase = "",
    [string]$AdminUser = "ops",
    [string]$AdminPassword = $env:PROBE_ADMIN_PASSWORD,
    [switch]$NoStartupTask,
    [switch]$ForceConfig
)

$ErrorActionPreference = "Stop"

function Write-Step {
    param([string]$Msg)
    Write-Host "==> $Msg" -ForegroundColor Cyan
}

# 0) 管理员权限检查
$id = [Security.Principal.WindowsIdentity]::GetCurrent()
$pr = New-Object Security.Principal.WindowsPrincipal($id)
if (-not $pr.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host "错误：请用「以管理员身份运行」的 PowerShell 执行。" -ForegroundColor Red
    exit 1
}

$target = Join-Path $Dir "probe-master.exe"

if (-not $Bin -and -not (Test-Path $target)) {
    Write-Host "错误：既没有 -Bin，也没有已安装的 $target。" -ForegroundColor Red
    exit 2
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
    Write-Host "警告：收紧数据目录 ACL 失败：$($_.Exception.Message)" -ForegroundColor Yellow
}

# 2) 二进制
Write-Step "2/6 安装二进制"
if ($Bin) {
    if (-not (Test-Path $Bin)) {
        Write-Host "错误：找不到 $Bin" -ForegroundColor Red
        exit 2
    }
    Copy-Item -Force $Bin $target
}
& $target version

# 3) 配置
$cfg = Join-Path $Dir "master.yaml"
Write-Step "3/6 写配置 $cfg"
if ((Test-Path $cfg) -and -not $ForceConfig) {
    Write-Host "    已存在，保留（要覆盖请加 -ForceConfig）"
} else {
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
    # 绝不写 install: 段。install.mode / install.master_url / install.dist_base
    # 都是**数据库设置**（repo.GetSettingDefault 读取），不是配置文件字段：
    # MasterConfig 无 install 键，且 YAML 解析非严格——写了不报错却永不生效。
    # 唯一入口是后台「设置 → 安装与下载」。master_url 未设置时回落到 public_origin。
    [void]$lines.Add("tls:")
    [void]$lines.Add('  mode: "none"')
    [void]$lines.Add('log_level: "info"')
    # 统一写 LF：YAML 解析器对 CRLF 宽容，但配置文件里混着两种换行迟早出怪事。
    $utf8 = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($cfg, (($lines -join "`n") + "`n"), $utf8)
}

# 4) 初始化管理员
Write-Step "4/6 初始化管理员"
if (-not $AdminPassword) {
    $sec = Read-Host "管理员 $AdminUser 的口令" -AsSecureString
    $AdminPassword = [Runtime.InteropServices.Marshal]::PtrToStringAuto([Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec))
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
    $taskName = "probe-master"
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
        Write-Host "警告：启动计划任务失败：$($_.Exception.Message)" -ForegroundColor Yellow
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
$ok = $false
for ($i = 0; $i -lt 20; $i++) {
    Start-Sleep -Seconds 1
    try {
        $r = Invoke-WebRequest -Uri "http://$healthAddr/api/public/v1/site" -UseBasicParsing -TimeoutSec 3
        if ($r.StatusCode -eq 200) {
            $ok = $true
            break
        }
    } catch {
        # 还没起来，继续等
    }
}
if ($ok) {
    Write-Host "    公开 API 已就绪" -ForegroundColor Green
} else {
    Write-Host "    20 秒内没探到公开 API，请查看 $Data 下的日志" -ForegroundColor Yellow
}

Write-Host ""
Write-Host "接下来要做的事：" -ForegroundColor Cyan
Write-Host "  1) 登录 ${AdminOrigin}/admin/ 并启用 TOTP、保存恢复码"
Write-Host "  2) 在「设置 → 安装与下载」核对（配置文件里写它们无效）："
Write-Host "       · Master 公网地址 install.master_url —— 留空则回落到 public_origin"
Write-Host "       · 二进制下载源 install.dist_base  —— 留空则由本进程提供 /dist/"
Write-Host "       · 安装入口模式 install.mode       —— 默认 on_demand"
Write-Host "  3) 生成注册码，用它装第一个 Agent（见 docs/04 的 Agent 一键部署）"
Write-Host "  4) probe-master dist pull --version <版本> --from <发布源> --pubkey <公钥>   # Agent 自升级用"
Write-Host "  5) 公开看板与后台是否分成两个域名 —— 在「设置 → 站点形态」里选："
Write-Host "       跟随配置文件（默认）/ 同站（一个域名）/ 分离（两个域名，需同时填两份域名清单）"
Write-Host "       运行时设置，改完即时生效、不用重启；分离靠 Host 而不是监听地址（见 docs/04 §2.5）"
Write-Host "  6) 配 TLS（acme 或前置反代 + trusted_proxies）与外部心跳 heartbeat_url"
if (-not $PublicOrigin) {
    Write-Host "⚠ 没有给 -PublicOrigin：install.master_url 拿不到值，Master 会按请求的 Host 回填。" -ForegroundColor Yellow
    Write-Host "  走域名访问时正常；但公开站若是明文 HTTP 直连 IP，回填出来的是 https:// —— Agent 会连不上。" -ForegroundColor Yellow
}
if ($DistBase) {
    Write-Host "⚠ 已记录 -DistBase，但它写不进配置文件（install.* 全是 DB 设置）。" -ForegroundColor Yellow
    Write-Host "  请在「设置 → 安装与下载」把「二进制下载源」填成：$DistBase" -ForegroundColor Yellow
}
