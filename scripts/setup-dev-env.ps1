<#
.SYNOPSIS
    XB-Probe —— Windows 开发环境一键搭建（用户级，无需管理员）。

.DESCRIPTION
    做三件事：
      1) 安装 Go 工具链（zip 解压到用户目录，不写 Program Files、不改系统 PATH）
      2) 配置 Go 环境变量（国内代理、关闭工具链自动下载、模块缓存）
      3) 可选安装辅助工具（govulncheck / staticcheck / gofumpt…）并构建本项目

    官方下载站 go.dev 在国内常不可达，脚本会自动回退到国内镜像（阿里云/中科大/清华）。
    校验和优先取 go.dev 官方发布；取不到时给出显著警告并继续（可用 -RequireChecksum 强制）。

.PARAMETER GoVersion
    要安装的 Go 版本，默认 1.27.2（>= go.mod 声明的 1.22 即可）。

.PARAMETER InstallRoot
    安装根目录，默认 %USERPROFILE%\.workbuddy\binaries\go；多版本会并存于该目录下。

.PARAMETER WithTools
    同时安装辅助工具（govulncheck、staticcheck、gofumpt、goimports）。

.PARAMETER SkipBuild
    只装工具链，不构建本项目。

.PARAMETER AddToUserPath
    把 Go 的 bin 目录写入「用户级 PATH」（持久生效，只影响当前用户）。

.PARAMETER RequireChecksum
    取不到官方 SHA-256 时报错退出，而不是仅警告。

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\scripts\setup-dev-env.ps1
.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\scripts\setup-dev-env.ps1 -WithTools -AddToUserPath
#>

[CmdletBinding()]
param(
    [string] $GoVersion = '1.27.2',
    [string] $InstallRoot = (Join-Path $env:USERPROFILE '.workbuddy\binaries\go'),
    [string[]] $Mirrors = @(
        'https://mirrors.aliyun.com/golang',
        'https://mirrors.ustc.edu.cn/golang',
        'https://mirrors.tuna.tsinghua.edu.cn/golang'
    ),
    [switch] $WithTools,
    [switch] $SkipBuild,
    [switch] $AddToUserPath,
    [switch] $RequireChecksum
)

$ErrorActionPreference = 'Stop'
# PS 5.1 下 Invoke-WebRequest 的进度条会把大文件下载拖慢数倍
$ProgressPreference = 'SilentlyContinue'

function Write-Step { param([string]$m) Write-Host "`n==> $m" -ForegroundColor Cyan }
function Write-Ok   { param([string]$m) Write-Host "    [OK] $m" -ForegroundColor Green }
function Write-Warn2{ param([string]$m) Write-Host "    [!]  $m" -ForegroundColor Yellow }
function Write-Err2 { param([string]$m) Write-Host "    [x]  $m" -ForegroundColor Red }

function Get-Sha256 {
    param([string]$Path)
    return (Get-FileHash -Path $Path -Algorithm SHA256).Hash.ToLower()
}

# 优先用系统自带的 curl.exe（比 Invoke-WebRequest 快且稳定），否则回退
function Invoke-Download {
    param([string]$Url, [string]$OutFile, [int]$TimeoutSec = 900)
    $curl = Get-Command curl.exe -ErrorAction SilentlyContinue
    if ($curl) {
        & $curl.Source -fL --max-time $TimeoutSec --retry 2 -s -S -o $OutFile $Url
        if ($LASTEXITCODE -ne 0) { throw "curl 下载失败（exit=$LASTEXITCODE）：$Url" }
        return
    }
    Invoke-WebRequest -Uri $Url -OutFile $OutFile -UseBasicParsing -TimeoutSec $TimeoutSec
}

function Test-UrlExists {
    param([string]$Url, [int]$TimeoutSec = 12)
    $curl = Get-Command curl.exe -ErrorAction SilentlyContinue
    try {
        if ($curl) {
            & $curl.Source -fsIL --max-time $TimeoutSec -o NUL $Url *> $null
            return ($LASTEXITCODE -eq 0)
        }
        $r = Invoke-WebRequest -Uri $Url -Method Head -UseBasicParsing -TimeoutSec $TimeoutSec
        return ($r.StatusCode -ge 200 -and $r.StatusCode -lt 400)
    } catch {
        return $false
    }
}

# ---------------------------------------------------------------------------
# 0. 已有可用 Go 则直接跳过安装
# ---------------------------------------------------------------------------
Write-Step "检查现有 Go 工具链"
$existing = Get-Command go -ErrorAction SilentlyContinue
$needInstall = $true
if ($existing) {
    $v = (& go env GOVERSION) 2>$null
    Write-Ok "已检测到 go：$v（$($existing.Source)）"
    if ($v -match 'go1\.(\d+)\.(\d+)') {
        $major = [int]$Matches[1]; $minor = [int]$Matches[2]
        if ($major -gt 1 -or ($major -eq 1 -and $minor -ge 26)) {
            Write-Ok "版本满足 go.mod 要求（>= 1.26），跳过安装。"
            $needInstall = $false
        } else {
            Write-Warn2 "版本过低（需要 >= 1.26），将安装 $GoVersion 到用户目录并在本次会话中前置。"
        }
    }
}

# ---------------------------------------------------------------------------
# 1. 下载 Go
# ---------------------------------------------------------------------------
$goRoot   = Join-Path $InstallRoot ('go' + $GoVersion)
$goBin    = Join-Path $goRoot 'bin'
$goExe    = Join-Path $goBin 'go.exe'
$file     = "go$GoVersion.windows-amd64.zip"
$cacheDir = Join-Path $InstallRoot 'downloads'
$zipPath  = Join-Path $cacheDir $file

if ($needInstall) {
    if (-not (Test-Path $cacheDir)) { New-Item -ItemType Directory -Path $cacheDir -Force | Out-Null }

    Write-Step "解析下载地址（官方优先，失败回退国内镜像）"
    $official = "https://go.dev/dl/$file"
    $candidates = @($official) + ($Mirrors | ForEach-Object { "$_/$file" })
    $zipUrl = $null
    foreach ($u in $candidates) {
        if (Test-UrlExists -Url $u) { $zipUrl = $u; Write-Ok "可用源：$u"; break }
        Write-Warn2 "不可用：$u"
    }
    if (-not $zipUrl) {
        Write-Err2 "所有下载源都不可用。请手动下载 $file 放到 $cacheDir 后重跑本脚本。"
        exit 1
    }

    Write-Step "下载 Go $GoVersion（约 75 MB）"
    if (-not (Test-Path $zipPath)) {
        Invoke-Download -Url $zipUrl -OutFile $zipPath
    } else {
        Write-Ok "已存在缓存的压缩包，跳过下载：$zipPath"
    }
    Write-Ok ("下载完成：{0:N1} MB" -f ((Get-Item $zipPath).Length / 1MB))

    # ---- 校验和：优先官方发布 ----
    Write-Step "校验 SHA-256"
    $actual = Get-Sha256 -Path $zipPath
    $expected = $null
    foreach ($src in @("$official.sha256", "https://dl.google.com/go/$file.sha256")) {
        try {
            $tmp = Join-Path $cacheDir "$file.sha256"
            Invoke-Download -Url $src -OutFile $tmp -TimeoutSec 20
            $txt = (Get-Content -Path $tmp -Raw -ErrorAction Stop)
            if ($txt -match '^[0-9a-fA-F]{64}') {
                $expected = $Matches[0].ToLower()
                Write-Ok "已获取官方校验和：$src"
                Remove-Item $tmp -Force -ErrorAction SilentlyContinue
                break
            }
            Remove-Item $tmp -Force -ErrorAction SilentlyContinue
        } catch {
            Write-Warn2 "取不到校验和：$src"
        }
    }
    if ($expected) {
        if ($actual -ne $expected) {
            Write-Err2 "SHA-256 不匹配！已删除下载文件，请更换镜像重试。"
            Write-Err2 "  期望 $expected"
            Write-Err2 "  实际 $actual"
            Remove-Item $zipPath -Force
            exit 1
        }
        Write-Ok "SHA-256 校验通过"
    } else {
        Write-Warn2 "无法取得官方校验和（go.dev 不可达）。本次 SHA-256 = $actual"
        Write-Warn2 "已仅通过 HTTPS 从可信镜像下载；如需强校验，请联网后加 -RequireChecksum 重跑。"
        if ($RequireChecksum) { Write-Err2 "-RequireChecksum 已指定，终止。"; exit 1 }
    }

    # ---- 解压 ----
    Write-Step "解压到 $goRoot"
    if (Test-Path $goRoot) {
        Write-Warn2 "目标目录已存在，先移除旧版本（仅限该目录内）：$goRoot"
        Remove-Item $goRoot -Recurse -Force
    }
    New-Item -ItemType Directory -Path $InstallRoot -Force | Out-Null
    Expand-Archive -Path $zipPath -DestinationPath $InstallRoot -Force
    if (-not (Test-Path $goExe)) {
        Write-Err2 "解压后未找到 $goExe，压缩包结构异常。"
        exit 1
    }
    Write-Ok "解压完成"
}

# ---------------------------------------------------------------------------
# 2. PATH（本次会话 + 可选持久化）
# ---------------------------------------------------------------------------
Write-Step "配置 PATH"
$env:Path = "$goBin;$env:Path"
Write-Ok "已加入本次 PowerShell 会话 PATH"

if ($AddToUserPath) {
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ($null -eq $userPath) { $userPath = '' }
    if ($userPath -notlike "*$goBin*") {
        $newPath = ($userPath.TrimEnd(';') + ';' + $goBin).TrimStart(';')
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
        Write-Ok "已写入用户级 PATH（新开终端生效）"
    } else {
        Write-Ok "用户级 PATH 中已存在，跳过"
    }
    # 让后续 `go` 命令立刻可用（同进程内 Get-Command 缓存刷新）
    $env:Path = $goBin + ';' + ([Environment]::GetEnvironmentVariable('Path', 'Machine')) + ';' + ([Environment]::GetEnvironmentVariable('Path', 'User'))
}

# 如果没走安装分支，goBin 也要存在才能继续
if (-not (Test-Path $goExe)) {
    $c = Get-Command go -ErrorAction SilentlyContinue
    if ($c) { $goExe = $c.Source }
}
if (-not (Test-Path $goExe)) { Write-Err2 "找不到 go.exe"; exit 1 }

Write-Step "版本确认"
& $goExe version
& $goExe env GOROOT GOPATH GOVERSION

# ---------------------------------------------------------------------------
# 3. Go 环境变量（国内代理等）
# ---------------------------------------------------------------------------
Write-Step "配置 Go 环境变量"
$goproxy = 'https://goproxy.cn,direct'
# 探测代理是否可达，可达才写入（避免把联网环境也锁死在单一代理上）
if (Test-UrlExists -Url 'https://goproxy.cn/github.com/gin-gonic/gin/@v/list') {
    & $goExe env -w GOPROXY=$goproxy
    Write-Ok "GOPROXY = $goproxy"
} else {
    Write-Warn2 "goproxy.cn 不可达，未修改 GOPROXY（用默认 proxy.golang.org）"
}
# go.mod 声明 go 1.26（下限），1.27.2 已满足，不需要自动下载工具链；锁死可避免内网/CI 意外联网
& $goExe env -w GOTOOLCHAIN=local
Write-Ok "GOTOOLCHAIN = local"
Write-Ok ("GOMODCACHE = " + (& $goExe env GOMODCACHE))

# ---------------------------------------------------------------------------
# 4. 辅助工具（可选）
# ---------------------------------------------------------------------------
if ($WithTools) {
    Write-Step "安装辅助 Go 工具（写入 GOBIN）"
    $gobin = (& $goExe env GOPATH)
    $gobin = Join-Path $gobin 'bin'
    $env:Path = "$gobin;$env:Path"

    $tools = @(
        @{ name = 'govulncheck'; pkg = 'golang.org/x/vuln/cmd/govulncheck@latest' },
        @{ name = 'staticcheck'; pkg = 'honnef.co/go/tools/cmd/staticcheck@latest' },
        @{ name = 'gofumpt';     pkg = 'mvdan.cc/gofumpt@latest' },
        @{ name = 'goimports';   pkg = 'golang.org/x/tools/cmd/goimports@latest' }
    )
    foreach ($t in $tools) {
        Write-Host "    -> $($t.name)" -ForegroundColor Gray
        & $goExe install $t.pkg
        if ($LASTEXITCODE -ne 0) { Write-Warn2 "$($t.name) 安装失败（可稍后单独重试）" }
        else { Write-Ok "$($t.name) 安装完成 -> $gobin" }
    }
    if ($userPath -like "*$gobin*" -or $AddToUserPath) {
        $cur = [Environment]::GetEnvironmentVariable('Path', 'User')
        if ($null -eq $cur) { $cur = '' }
        if ($cur -notlike "*$gobin*") {
            [Environment]::SetEnvironmentVariable('Path', ($cur.TrimEnd(';') + ';' + $gobin).TrimStart(';'), 'User')
            Write-Ok "已把 GOBIN 写入用户级 PATH"
        }
    } else {
        Write-Warn2 "GOBIN ($gobin) 尚未持久化到 PATH，可加 -AddToUserPath 重跑"
    }
}

# ---------------------------------------------------------------------------
# 5. 构建本项目
# ---------------------------------------------------------------------------
if (-not $SkipBuild) {
    $projRoot = Split-Path -Parent $PSScriptRoot   # scripts/ 的上一级
    if (Test-Path (Join-Path $projRoot 'go.mod')) {
        Write-Step "首次拉取依赖并构建（$projRoot）"
        Push-Location $projRoot
        try {
            & $goExe mod tidy
            if ($LASTEXITCODE -ne 0) { throw "go mod tidy 失败" }
            & $goExe mod download
            if ($LASTEXITCODE -ne 0) { Write-Warn2 "go mod download 失败（依赖未全部落入 GOMODCACHE，构建时可能仍需联网）" }
            & $goExe vet ./...
            & $goExe build -trimpath -ldflags '-s -w' -o bin/xb-probe-master.exe ./cmd/xb-probe-master
            & $goExe build -trimpath -ldflags '-s -w' -o bin/xb-probe-agent.exe  ./cmd/xb-probe-agent
            if ($LASTEXITCODE -eq 0) {
                Write-Ok "构建完成：bin\xb-probe-master.exe、bin\xb-probe-agent.exe"
            } else {
                Write-Err2 "构建失败，请查看上方编译错误"
            }
        } finally {
            Pop-Location
        }
    } else {
        Write-Warn2 "未在上层目录找到 go.mod，跳过构建"
    }
}

Write-Step "完成"
Write-Host @"
下一步：
  1) 新开一个终端（让持久化的 PATH 生效），执行：go version
  2) 复制配置：copy configs\master.yaml.example master.yaml（按需改 data_dir / 监听地址）
  3) 初始化管理员：.\bin\xb-probe-master.exe init-admin --config .\master.yaml
  4) 启动：.\bin\xb-probe-master.exe --config .\master.yaml
详见 docs\03-工具链安装与环境搭建.md
"@ -ForegroundColor Gray
