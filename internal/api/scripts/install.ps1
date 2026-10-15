# xb-probe-agent Windows 安装脚本（文档 12.3 / 12.6）
#
# 安全说明（12.8）：脚本与下载文件只是静态内容，不含任何节点秘密；
# 注册码单次使用、短时有效、绑定节点，安装后即失效；下载严格校验 TLS 与 SHA-256。

[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$Code,
  [string]$MasterUrl = '__MASTER_URL__',
  [string]$DistBase  = '__DIST_BASE__',
  [string]$SpkiSha256 = '__SPKI__',
  # 公网地址探测服务（默认关闭）。Master 与 Agent 同机部署时 Master 看到的
  # 来源地址是 127.0.0.1，只有节点自己出网问一句才能拿到真正的公网地址。
  # 打开它会定期（默认 15 分钟）访问这个第三方地址，所以必须显式给出。
  [string]$PublicIpUrl = ''
)

$ErrorActionPreference = 'Stop'

# 需要管理员权限
$isAdmin = ([Security.Principal.WindowsPrincipal] `
  [Security.Principal.WindowsIdentity]::GetCurrent()
).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) { throw '请以管理员身份运行 PowerShell。' }

if ($MasterUrl -notlike 'https://*') {
  if ($MasterUrl -notlike '*127.0.0.1*' -and $MasterUrl -notlike '*localhost*') {
    throw 'Master 地址必须是 https://（本机回环除外）。'
  }
}

# 识别架构
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  'AMD64' { 'amd64' }
  'ARM64' { 'arm64' }
  default { throw "不支持的架构：$($env:PROCESSOR_ARCHITECTURE)" }
}

$installDir = Join-Path $env:ProgramData 'xb-probe-agent'
New-Item -ItemType Directory -Force -Path $installDir | Out-Null

$name = "xb-probe-agent_windows_$arch.exe"
$tmp  = Join-Path $env:TEMP $name

Write-Host "==> 下载 $name"
Invoke-WebRequest -UseBasicParsing -Uri "$DistBase/$name" -OutFile $tmp

# SHA256SUMS 始终取自 Master
Write-Host '==> 校验 SHA-256'
$sums = Invoke-WebRequest -UseBasicParsing -Uri "$MasterUrl/dist/SHA256SUMS" |
  Select-Object -ExpandProperty Content
$expected = ($sums -split "`n" | Where-Object { $_ -match [regex]::Escape($name) } |
  Select-Object -First 1) -split '\s+' | Select-Object -First 1
if (-not $expected) { throw '校验和中找不到对应文件。' }
$actual = (Get-FileHash -Algorithm SHA256 -Path $tmp).Hash.ToLower()
if ($expected.ToLower() -ne $actual) {
  Remove-Item -Force $tmp
  throw 'SHA-256 校验失败，已删除下载文件。'
}

$target = Join-Path $installDir 'xb-probe-agent.exe'
Move-Item -Force $tmp $target

# 注册并启动 Windows 服务（Agent 自身实现服务接口，无需 nssm）
$args = @('install', '--master', $MasterUrl, '--code', $Code, '--dir', $installDir)
if ($SpkiSha256 -and $SpkiSha256 -ne '__SPKI__') {
  $args += @('--spki-sha256', $SpkiSha256)
}
if ($PublicIpUrl) {
  $args += @('--public-ip-url', $PublicIpUrl)
}
& $target @args

Write-Host '==> 安装完成。若页面未显示节点，请检查服务状态：Get-Service xb-probe-agent'
# 二进制装在 ProgramData 下、并不在 PATH 里。不说清楚路径和卸载命令，
# 用户装完想卸载时根本找不到这个 exe（实测直接敲 xb-probe-agent 会报
# 「无法识别为 cmdlet」），只能回来问"Windows 上怎么卸载"。
Write-Host "==> 程序目录：$installDir（不在 PATH，需用完整路径执行）"
Write-Host "==> 卸载命令（管理员 PowerShell 执行）：& '$target' uninstall --yes"
