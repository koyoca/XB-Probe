<#
.SYNOPSIS
    Server Probe —— Windows 构建脚本（不依赖 make）。

.DESCRIPTION
    等价于 Makefile 的 master / agent / dist / checksums 目标，纯 PowerShell 实现，
    不需要安装 make。所有产物均为 CGO_ENABLED=0 静态编译，-trimpath + -ldflags "-s -w"，
    不使用 UPX（文档 12.6）。

.PARAMETER Version
    写入二进制的版本号（-X main.version），默认 1.0.0。

.PARAMETER Dist
    交叉编译全部发布平台（仅 Agent）到 dist\<Version>\，并生成 SHA256SUMS。

.PARAMETER Test
    先跑 go vet 与 go test。

.PARAMETER Race
    跑 go test -race（需要 C 编译器 / MinGW-w64）。

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\scripts\build.ps1
.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\scripts\build.ps1 -Version 1.1.0 -Dist
#>

[CmdletBinding()]
param(
    [string] $Version = '1.0.0',
    [switch] $Dist,
    [switch] $Test,
    [switch] $Race
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

function Write-Step { param([string]$m) Write-Host "`n==> $m" -ForegroundColor Cyan }
function Write-Ok   { param([string]$m) Write-Host "    [OK] $m" -ForegroundColor Green }
function Write-Warn2{ param([string]$m) Write-Host "    [!]  $m" -ForegroundColor Yellow }
function Write-Err2 { param([string]$m) Write-Host "    [x]  $m" -ForegroundColor Red }

$projRoot = Split-Path -Parent $PSScriptRoot
if (-not (Test-Path (Join-Path $projRoot 'go.mod'))) {
    Write-Err2 "未在 $projRoot 找到 go.mod"
    exit 1
}
Set-Location $projRoot

$go = (Get-Command go -ErrorAction SilentlyContinue)
if (-not $go) { Write-Err2 "找不到 go，请先运行 scripts\setup-dev-env.ps1"; exit 1 }
Write-Ok "使用 $((& go version))"

$module = ((Get-Content (Join-Path $projRoot 'go.mod') |
        Where-Object { $_ -match '^module\s+\S+' } | Select-Object -First 1) -replace '^module\s+', '').Trim()
if (-not $module) { Write-Err2 "无法从 go.mod 解析出 module 名"; exit 1 }

$ldflags = "-s -w -X main.version=$Version"

# 发布标签 → 产物名的第 2 段。armv7/armv6 在 Go 里**都是** GOARCH=arm（靠 GOARM 区分），
# 运行时拿不到 GOARM，所以标签只能构建时写进二进制（见 internal/dist.CurrentTag）。
# 不注入的后果是静默的：32 位 ARM 节点会去要 probe-agent_linux_arm，
# 而发布矩阵里只有 ..._armv7/_armv6，于是自升级永远报"该平台未发布此版本"。
function Get-PlatformTag {
    param([string] $os, [string] $arch, [string] $goarm)
    if ($arch -eq 'arm') { return "${os}_armv$goarm" }
    return "${os}_${arch}"
}
function Get-Ldflags {
    param([string] $tag)
    return "$ldflags -X $module/internal/dist.PlatformTag=$tag"
}

# ---------------------------------------------------------------------------
# 可选：检查与测试
# ---------------------------------------------------------------------------
if ($Test -or $Race) {
    Write-Step "静态检查与测试"
    & go vet ./...
    if ($LASTEXITCODE -ne 0) { Write-Warn2 "go vet 有告警" }
    if ($Race) {
        # -race 需要 cgo 与 C 编译器（MinGW-w64 / gcc）
        $env:CGO_ENABLED = '1'
        & go test -race ./...
        $env:CGO_ENABLED = '0'
        if ($LASTEXITCODE -ne 0) { Write-Warn2 "go test -race 未全部通过（缺 gcc 时会报 -race requires cgo）" }
    } else {
        & go test ./...
    }
}

# ---------------------------------------------------------------------------
# 本机构建
# ---------------------------------------------------------------------------
if (-not $Dist) {
    Write-Step "构建 Master 与 Agent -> bin\"
    New-Item -ItemType Directory -Force -Path (Join-Path $projRoot 'bin') | Out-Null
    $env:CGO_ENABLED = '0'

    & go build -trimpath -ldflags $ldflags -o 'bin/probe-master.exe' ./cmd/probe-master
    if ($LASTEXITCODE -ne 0) { Write-Err2 "probe-master 编译失败"; exit 1 }
    Write-Ok "bin\probe-master.exe"

    # Agent 带上本机发布标签：`probe-agent upgrade` 要靠它挑对产物名。
    $hostTag = Get-PlatformTag (& go env GOOS).Trim() (& go env GOARCH).Trim() (& go env GOARM).Trim()
    & go build -trimpath -ldflags (Get-Ldflags $hostTag) -o 'bin/probe-agent.exe' ./cmd/probe-agent
    if ($LASTEXITCODE -ne 0) { Write-Err2 "probe-agent 编译失败"; exit 1 }
    Write-Ok "bin\probe-agent.exe（发布标签 $hostTag）"

    Get-ChildItem bin | ForEach-Object { Write-Host ("    {0,-22} {1,8:N1} MB" -f $_.Name, ($_.Length / 1MB)) -ForegroundColor Gray }
    exit 0
}

# ---------------------------------------------------------------------------
# 交叉编译发布产物（仅 Agent；Master 只在宿主平台构建，见文档 12.6）
# ---------------------------------------------------------------------------
Write-Step "交叉编译 Agent -> dist\$Version\"

$distDir = Join-Path $projRoot "dist\$Version"
New-Item -ItemType Directory -Force -Path $distDir | Out-Null
$env:CGO_ENABLED = '0'

# os/arch 后接 GOARM（非 arm 目标写 0 表示不设置）
$platforms = @(
    @('linux',   'amd64',   '7'),
    @('linux',   'arm64',   '0'),
    @('linux',   'arm',     '7'),   # 文件名记为 armv7
    @('linux',   'arm',     '6'),   # 文件名记为 armv6
    @('linux',   '386',     '0'),
    @('linux',   'riscv64', '0'),
    @('darwin',  'amd64',   '0'),
    @('darwin',  'arm64',   '0'),
    @('freebsd', 'amd64',   '0'),
    @('freebsd', 'arm64',   '0'),
    @('windows', 'amd64',   '0'),
    @('windows', 'arm64',   '0')
)

$failed = @()
foreach ($p in $platforms) {
    $os = $p[0]; $arch = $p[1]; $goarm = $p[2]
    $tag = $arch
    if ($arch -eq 'arm') { $tag = "armv$goarm" }
    $ext = ''
    if ($os -eq 'windows') { $ext = '.exe' }

    $out = Join-Path $distDir "probe-agent_${os}_${tag}${ext}"
    Write-Host "    -> $os/$tag" -ForegroundColor Gray

    $env:GOOS = $os
    $env:GOARCH = $arch
    if ($goarm -ne '0') { $env:GOARM = $goarm } else { Remove-Item Env:GOARM -ErrorAction SilentlyContinue }

    & go build -trimpath -ldflags (Get-Ldflags "${os}_${tag}") -o $out ./cmd/probe-agent
    if ($LASTEXITCODE -ne 0) {
        Write-Err2 "$os/$tag 编译失败"
        $failed += "$os/$tag"
    }
}
Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
Remove-Item Env:GOARM -ErrorAction SilentlyContinue
$env:CGO_ENABLED = '0'

if ($failed.Count -gt 0) {
    Write-Warn2 ("以下平台编译失败：" + ($failed -join ', '))
    Write-Warn2 "常见原因：该 GOOS/GOARCH 不在 Go 官方支持矩阵内（用 go tool dist list 确认）"
}

# ---------------------------------------------------------------------------
# SHA256SUMS（与 sha256sum 输出格式一致：<hash>  <文件名>）
# ---------------------------------------------------------------------------
Write-Step "生成 SHA256SUMS"
$sumsPath = Join-Path $distDir 'SHA256SUMS'
$lines = @()
Get-ChildItem -Path $distDir -Filter 'probe-agent_*' -File |
    Sort-Object Name |
    ForEach-Object {
        $h = (Get-FileHash -Path $_.FullName -Algorithm SHA256).Hash.ToLower()
        $lines += "$h  $($_.Name)"
    }
if ($lines.Count -eq 0) {
    Write-Err2 "dist 目录下没有产物"
    exit 1
}
# 用 LF + 无 BOM，保证 sha256sum -c 在 Linux/Unix 侧可用
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
[System.IO.File]::WriteAllText($sumsPath, (($lines -join "`n") + "`n"), $utf8NoBom)
Write-Ok "$sumsPath（$($lines.Count) 个文件）"

Write-Step "完成"
Write-Host @"
发布前还需（对应 12.6 / 12.8 / S10）：
  1) 用离线密钥签名：minisign -Sm dist\$Version\SHA256SUMS
  2) 把 dist\$Version\ 放到 Master 的 <data_dir>\dist\$Version\ 或第三方 CDN
  3) 确认后台「发布文件」页面各平台齐全
"@ -ForegroundColor Gray
