<#
.SYNOPSIS
    XB-Probe —— Windows 构建脚本（不依赖 make）。

.DESCRIPTION
    等价于 Makefile 的 master / agent / dist / checksums 目标，纯 PowerShell 实现，
    不需要安装 make。所有产物均为 CGO_ENABLED=0 静态编译，-trimpath + -ldflags "-s -w"，
    不使用 UPX（文档 12.6）。

.PARAMETER Version
    写入二进制的版本号（-X main.version），默认 1.0.0。

.PARAMETER Dist
    交叉编译全部发布平台（Agent 13 个 + Master 2 个）到 dist\<Version>\，
    并生成 SHA256SUMS 与 SHA256SUMS.master 两份校验和（等价于 make release-assets）。

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
# 不注入的后果是静默的：32 位 ARM 节点会去要 xb-probe-agent_linux_arm，
# 而发布矩阵里只有 ..._armv7/_armv6，于是自升级永远报"该平台未发布此版本"。
function Get-PlatformTag {
    param([string] $os, [string] $arch, [string] $goarm)
    # 只有 linux 的 armv7/armv6 走 armv 前缀；freebsd/arm 这类不带 GOARM 的目标
    # 必须原样输出 freebsd_arm——写成 armv0 会与发布矩阵（Makefile PLATFORMS）不符。
    if ($arch -eq 'arm' -and $goarm -ne '0') { return "${os}_armv$goarm" }
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

    & go build -trimpath -ldflags $ldflags -o 'bin/xb-probe-master.exe' ./cmd/xb-probe-master
    if ($LASTEXITCODE -ne 0) { Write-Err2 "xb-probe-master 编译失败"; exit 1 }
    Write-Ok "bin\xb-probe-master.exe"

    # Agent 带上本机发布标签：`xb-probe-agent upgrade` 要靠它挑对产物名。
    $hostTag = Get-PlatformTag (& go env GOOS).Trim() (& go env GOARCH).Trim() (& go env GOARM).Trim()
    & go build -trimpath -ldflags (Get-Ldflags $hostTag) -o 'bin/xb-probe-agent.exe' ./cmd/xb-probe-agent
    if ($LASTEXITCODE -ne 0) { Write-Err2 "xb-probe-agent 编译失败"; exit 1 }
    Write-Ok "bin\xb-probe-agent.exe（发布标签 $hostTag）"

    Get-ChildItem bin | ForEach-Object { Write-Host ("    {0,-22} {1,8:N1} MB" -f $_.Name, ($_.Length / 1MB)) -ForegroundColor Gray }
    exit 0
}

# ---------------------------------------------------------------------------
# 交叉编译发布产物：Agent 全平台 + Master 两个 Linux 平台（与 Makefile 的
# release-assets 目标等价：dist + master-dist + 两份校验和）。
# 平台清单必须与 Makefile 的 PLATFORMS 逐项一致——CI 的 release.yml 断言 13 个，
# 漂一个就会"本地编的与流水线发的不是同一套"。
# ---------------------------------------------------------------------------
Write-Step "交叉编译 Agent -> dist\$Version\"

$distDir = Join-Path $projRoot "dist\$Version"
New-Item -ItemType Directory -Force -Path $distDir | Out-Null
$env:CGO_ENABLED = '0'

# os/arch 后接 GOARM（非 arm 目标写 0 表示不设置）。
# 与 Makefile PLATFORMS 对齐（13 个）：darwin 已明确移除——采集器只有
# linux/windows/freebsd 三份实现，macOS 会落到 stub（在线但无数据），
# 见 Makefile 顶部注释与 docs/01、docs/02。
$platforms = @(
    @('linux',   'amd64',   '0'),
    @('linux',   'arm64',   '0'),
    @('linux',   'arm',     '7'),   # 文件名记为 armv7
    @('linux',   'arm',     '6'),   # 文件名记为 armv6
    @('linux',   '386',     '0'),
    @('linux',   'riscv64', '0'),
    @('windows', 'amd64',   '0'),
    @('windows', '386',     '0'),
    @('windows', 'arm64',   '0'),
    @('freebsd', 'amd64',   '0'),
    @('freebsd', 'arm64',   '0'),
    @('freebsd', '386',     '0'),
    @('freebsd', 'arm',     '0')    # 不带 GOARM，产物名 freebsd_arm
)

$failed = @()
foreach ($p in $platforms) {
    $os = $p[0]; $arch = $p[1]; $goarm = $p[2]
    $tag = $arch
    if ($arch -eq 'arm' -and $goarm -ne '0') { $tag = "armv$goarm" }
    $ext = ''
    if ($os -eq 'windows') { $ext = '.exe' }

    $out = Join-Path $distDir "xb-probe-agent_${os}_${tag}${ext}"
    Write-Host "    -> $os/$tag" -ForegroundColor Gray

    $env:GOOS = $os
    $env:GOARCH = $arch
    if ($goarm -ne '0') { $env:GOARM = $goarm } else { Remove-Item Env:GOARM -ErrorAction SilentlyContinue }

    & go build -trimpath -ldflags (Get-Ldflags "${os}_${tag}") -o $out ./cmd/xb-probe-agent
    if ($LASTEXITCODE -ne 0) {
        Write-Err2 "$os/$tag 编译失败"
        $failed += "$os/$tag"
    }
}
Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
Remove-Item Env:GOARM -ErrorAction SilentlyContinue
$env:CGO_ENABLED = '0'

if ($failed.Count -gt 0) {
    # 与 Makefile 的 dist 目标一致：任何一个平台失败就整体失败。
    # 早先这里只告警不退出，结果发布出少平台的产物集而构建"成功"——
    # 用户那一端表现为下载 404，仓库里却看不出问题。
    Write-Err2 ("以下平台编译失败：" + ($failed -join ', '))
    Write-Err2 "常见原因：该 GOOS/GOARCH 不在 Go 官方支持矩阵内（用 go tool dist list 确认）"
    exit 1
}

# Master 的交叉编译产物（Makefile 的 master-dist）：给"无需编译"的一键部署用
# （install-master-bin.sh 直接从发布源取这两份）。
# 只发 Linux 两种架构——Master 是常驻服务端，没有人在 Windows/FreeBSD 上拿它当入口。
Write-Step "交叉编译 Master -> dist\$Version"

$masterPlatforms = @(
    @('linux', 'amd64'),
    @('linux', 'arm64')
)

foreach ($p in $masterPlatforms) {
    $os = $p[0]; $arch = $p[1]
    $out = Join-Path $distDir "xb-probe-master_${os}_${arch}"
    Write-Host "    -> $os/$arch" -ForegroundColor Gray

    $env:GOOS = $os
    $env:GOARCH = $arch
    Remove-Item Env:GOARM -ErrorAction SilentlyContinue

    & go build -trimpath -ldflags (Get-Ldflags "${os}_${arch}") -o $out ./cmd/xb-probe-master
    if ($LASTEXITCODE -ne 0) {
        Write-Err2 "xb-probe-master $os/$arch 编译失败"
        exit 1
    }
}
Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
Remove-Item Env:GOARM -ErrorAction SilentlyContinue
$env:CGO_ENABLED = '0'

# ---------------------------------------------------------------------------
# SHA256SUMS（与 sha256sum 输出格式一致：<hash>  <文件名>）
# ---------------------------------------------------------------------------
Write-Step "生成 SHA256SUMS"
$sumsPath = Join-Path $distDir 'SHA256SUMS'
$lines = @()
Get-ChildItem -Path $distDir -Filter 'xb-probe-agent_*' -File |
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

# Master 单独一份校验和（Makefile 的 checksums-master）：合并进 SHA256SUMS 会让
# Agent 侧 sha256sum -c 在自己的目录里找不到 Master 文件而报 FAILED，
# 失败原因看起来完全像"下载被篡改"。
$masterSumsPath = Join-Path $distDir 'SHA256SUMS.master'
$mLines = @()
Get-ChildItem -Path $distDir -Filter 'xb-probe-master_*' -File |
    Sort-Object Name |
    ForEach-Object {
        $h = (Get-FileHash -Path $_.FullName -Algorithm SHA256).Hash.ToLower()
        $mLines += "$h  $($_.Name)"
    }
if ($mLines.Count -eq 0) {
    Write-Err2 "dist 目录下没有 Master 产物"
    exit 1
}
[System.IO.File]::WriteAllText($masterSumsPath, (($mLines -join "`n") + "`n"), $utf8NoBom)
Write-Ok "$masterSumsPath（$($mLines.Count) 个文件）"

# 数量自检（与 .github/workflows/release.yml 同一口径）：少一个平台就必须失败，
# 否则"少发一份产物"只会在用户下载时以 404 的形式暴露。
if ($lines.Count -ne 13) {
    Write-Err2 "期望 13 个 Agent 产物，实际 $($lines.Count) 个"
    exit 1
}
if ($mLines.Count -ne 2) {
    Write-Err2 "期望 2 个 Master 产物，实际 $($mLines.Count) 个"
    exit 1
}

Write-Step "完成"
Write-Host @"
发布前还需（对应 12.6 / 12.8 / S10）：
  1) 用离线密钥签名：minisign -Sm dist\$Version\SHA256SUMS
  2) 把 dist\$Version\ 放到 Master 的 <data_dir>\dist\$Version\ 或第三方 CDN
  3) 确认后台「发布文件」页面各平台齐全
"@ -ForegroundColor Gray
