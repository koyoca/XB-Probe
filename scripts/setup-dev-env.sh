#!/bin/sh
# Server Probe —— Linux / macOS / FreeBSD 开发环境一键搭建
#
# 做三件事：安装 Go 工具链（用户级，不覆盖系统目录）、配置 Go 环境变量、
# 可选安装辅助工具并构建本项目。官方站 go.dev 不可达时自动回退国内镜像。
#
# 用法：
#   sh scripts/setup-dev-env.sh                     # 装 Go 1.27.2 + 配置 + 构建
#   sh scripts/setup-dev-env.sh --tools             # 额外装 govulncheck/staticcheck/gofumpt/goimports
#   sh scripts/setup-dev-env.sh --version 1.26.5    # 指定版本
#   sh scripts/setup-dev-env.sh --skip-build        # 只装工具链
#   sh scripts/setup-dev-env.sh --no-path           # 不写 shell rc
#   sh scripts/setup-dev-env.sh --require-checksum  # 取不到官方 SHA-256 就报错退出
#   sudo sh scripts/setup-dev-env.sh                # root 时默认装到 /usr/local
#
# 环境变量：
#   GO_INSTALL_DIR  安装根目录（默认：root -> /usr/local，普通用户 -> $HOME/.local/share/probe-go）

set -eu

GO_VERSION="1.27.2"
GO_INSTALL_DIR="${GO_INSTALL_DIR:-}"
WITH_TOOLS=0
SKIP_BUILD=0
WRITE_PATH=1
REQUIRE_CHECKSUM=0
MIRRORS="https://mirrors.aliyun.com/golang https://mirrors.ustc.edu.cn/golang https://mirrors.tuna.tsinghua.edu.cn/golang"

while [ $# -gt 0 ]; do
  case "$1" in
    --version)          GO_VERSION="$2"; shift 2 ;;
    --version=*)        GO_VERSION="${1#*=}"; shift ;;
    --dir)              GO_INSTALL_DIR="$2"; shift 2 ;;
    --dir=*)            GO_INSTALL_DIR="${1#*=}"; shift ;;
    --tools)            WITH_TOOLS=1; shift ;;
    --skip-build)       SKIP_BUILD=1; shift ;;
    --no-path)          WRITE_PATH=0; shift ;;
    --require-checksum) REQUIRE_CHECKSUM=1; shift ;;
    -h|--help)          sed -n '2,25p' "$0"; exit 0 ;;
    *) echo "未知参数：$1（--help 查看用法）" >&2; exit 2 ;;
  esac
done

c_cyan() { printf '\033[36m%s\033[0m\n' "$1"; }
c_ok()   { printf '\033[32m    [OK] %s\033[0m\n' "$1"; }
c_warn() { printf '\033[33m    [!]  %s\033[0m\n' "$1"; }
c_err()  { printf '\033[31m    [x]  %s\033[0m\n' "$1" >&2; }
step()   { printf '\n\033[36m==> %s\033[0m\n' "$1"; }

# ---------------------------------------------------------------------------
# 0. 平台与架构识别（对应文档 12.4）
# ---------------------------------------------------------------------------
step "识别系统与架构"
OS=$(uname -s)
case "$OS" in
  Linux)   GOOS=linux ;;
  Darwin)  GOOS=darwin ;;
  FreeBSD) GOOS=freebsd ;;
  *) c_err "不支持的系统：$OS"; exit 1 ;;
esac
MACHINE=$(uname -m)
case "$MACHINE" in
  x86_64|amd64)      GOARCH=amd64 ;;
  aarch64|arm64)     GOARCH=arm64 ;;
  armv7l|armv7)      GOARCH=arm; GOARM_NOTE="Go 官方只发布 32 位 ARM 的 armv6l 包，将安装 armv6l 工具链并默认 GOARM=7" ;;
  armv6l|armv6)      GOARCH=arm ;;
  i386|i686)         GOARCH=386 ;;
  riscv64)           GOARCH=riscv64 ;;
  loongarch64)       GOARCH=loong64 ;;
  *) c_err "不支持的架构：$MACHINE"; exit 1 ;;
esac
# 官方 tar 命名：linux-armv6l / darwin-arm64 / freebsd-amd64…
case "$GOARCH" in
  arm) ARCH_TAG="armv6l" ;;
  *)   ARCH_TAG="$GOARCH" ;;
esac
[ -n "${GOARM_NOTE:-}" ] && c_warn "$GOARM_NOTE"
c_ok "$GOOS/$GOARCH（下载包后缀 $ARCH_TAG）"

# ---------------------------------------------------------------------------
# 1. 是否已有可用 Go
# ---------------------------------------------------------------------------
step "检查现有 Go 工具链"
NEED_INSTALL=1
if command -v go >/dev/null 2>&1; then
  CUR=$(go version 2>/dev/null | awk '{print $3}')
  c_ok "已检测到 $CUR（$(command -v go)）"
  V=${CUR#go1.}
  V=${V%%.*}
  if [ -n "$V" ] && [ "$V" -ge 26 ] 2>/dev/null; then
    c_ok "版本满足 go.mod 要求（>= 1.26），跳过安装。"
    NEED_INSTALL=0
  else
    c_warn "版本过低（需要 >= 1.26），将安装 Go $GO_VERSION。"
  fi
fi

# ---------------------------------------------------------------------------
# 2. 安装目录与下载器
# ---------------------------------------------------------------------------
if [ -z "$GO_INSTALL_DIR" ]; then
  if [ "$(id -u)" = "0" ]; then GO_INSTALL_DIR=/usr/local
  else GO_INSTALL_DIR="$HOME/.local/share/probe-go"; fi
fi
GO_ROOT="$GO_INSTALL_DIR/go$GO_VERSION"
GO_BIN="$GO_ROOT/go/bin"
CACHE_DIR="$GO_INSTALL_DIR/downloads"
FILE="go$GO_VERSION.$GOOS-$ARCH_TAG.tar.gz"

DL=""
for c in curl wget fetch; do command -v "$c" >/dev/null 2>&1 && { DL=$c; break; }; done
if [ -z "$DL" ]; then
  c_err "需要 curl / wget / fetch 之一来下载。"
  c_err "Debian/Ubuntu: apt install -y curl ；Alpine: apk add curl ；FreeBSD 自带 fetch。"
  exit 1
fi
c_ok "下载器：$DL"

download() { # download <url> <out>
  case "$DL" in
    curl)  curl -fL --max-time 900 --retry 2 -s -S -o "$2" "$1" ;;
    wget)  wget -q --timeout=900 -t 3 -O "$2" "$1" ;;
    fetch) fetch -q -o "$2" "$1" ;;
  esac
}
url_ok() { # url_ok <url>
  case "$DL" in
    curl)  curl -fsIL --max-time 12 -o /dev/null "$1" 2>/dev/null ;;
    wget)  wget -q --spider --timeout=12 "$1" 2>/dev/null ;;
    fetch) fetch -q -s -o /dev/null "$1" 2>/dev/null ;;
  esac
}
sha256_of() { # sha256_of <file>
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v sha256 >/dev/null 2>&1; then sha256 -q "$1"
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then openssl dgst -sha256 "$1" | awk '{print $NF}'
  else return 1; fi
}

# ---------------------------------------------------------------------------
# 3. 下载并安装
# ---------------------------------------------------------------------------
if [ "$NEED_INSTALL" = "1" ]; then
  mkdir -p "$CACHE_DIR"
  TARBALL="$CACHE_DIR/$FILE"

  step "解析下载地址"
  OFFICIAL="https://go.dev/dl/$FILE"
  URL=""
  for u in "$OFFICIAL" $(for m in $MIRRORS; do echo "$m/$FILE"; done); do
    if url_ok "$u"; then URL="$u"; c_ok "可用源：$u"; break; fi
    c_warn "不可用：$u"
  done
  if [ -z "$URL" ]; then
    c_err "所有源都不可用。请手动下载 $FILE 放到 $CACHE_DIR 后重跑。"
    exit 1
  fi

  step "下载 Go $GO_VERSION"
  if [ -f "$TARBALL" ]; then
    c_ok "已有缓存，跳过下载：$TARBALL"
  else
    download "$URL" "$TARBALL" || { c_err "下载失败"; rm -f "$TARBALL"; exit 1; }
  fi
  c_ok "大小：$(wc -c <"$TARBALL" | awk '{printf "%.1f MB", $1/1048576}')"

  step "校验 SHA-256"
  ACTUAL=$(sha256_of "$TARBALL" || echo "")
  EXPECTED=""
  for src in "$OFFICIAL.sha256" "https://dl.google.com/go/$FILE.sha256"; do
    TMP="$CACHE_DIR/$FILE.sha256"
    if download "$src" "$TMP" 2>/dev/null && grep -qE '^[0-9a-fA-F]{64}$' "$TMP" 2>/dev/null; then
      EXPECTED=$(tr -d '\r\n ' <"$TMP" | tr 'A-Z' 'a-z')
      c_ok "已获取官方校验和：$src"
      rm -f "$TMP"
      break
    fi
    rm -f "$TMP"
    c_warn "取不到校验和：$src"
  done
  if [ -n "$EXPECTED" ]; then
    if [ -z "$ACTUAL" ]; then
      c_warn "本机没有可用的 sha256 工具，跳过比对（已从 HTTPS 下载）"
    elif [ "$ACTUAL" != "$EXPECTED" ]; then
      c_err "SHA-256 不匹配！已删除下载文件。期望 $EXPECTED，实际 $ACTUAL"
      rm -f "$TARBALL"
      exit 1
    else
      c_ok "SHA-256 校验通过"
    fi
  else
    c_warn "无法取得官方校验和（go.dev 不可达）。本次 SHA-256 = ${ACTUAL:-不可计算}"
    c_warn "已仅通过 HTTPS 从可信镜像下载；如需强校验，联网后加 --require-checksum 重跑。"
    [ "$REQUIRE_CHECKSUM" = "1" ] && { c_err "--require-checksum 已指定，终止。"; exit 1; }
  fi

  step "解压到 $GO_ROOT"
  if [ -e "$GO_ROOT" ]; then
    c_warn "目标已存在，先移除旧版本（仅限该目录内）：$GO_ROOT"
    rm -rf "$GO_ROOT"
  fi
  mkdir -p "$GO_ROOT"
  tar -C "$GO_ROOT" -xzf "$TARBALL"
  [ -x "$GO_BIN/go" ] || { c_err "解压后未找到 $GO_BIN/go"; exit 1; }
  c_ok "解压完成"

  # current 软链，便于 PATH 固定指向一个稳定路径
  ln -sfn "$GO_ROOT/go" "$GO_INSTALL_DIR/current" 2>/dev/null || true
fi

# 若走了跳过分支，沿用系统 go
if [ ! -x "$GO_BIN/go" ]; then
  GO_BIN="$(dirname "$(command -v go)")"
fi

# ---------------------------------------------------------------------------
# 4. PATH
# ---------------------------------------------------------------------------
step "配置 PATH"
PATH="$GO_BIN:$PATH"
export PATH
c_ok "本次会话：$GO_BIN"

if [ "$WRITE_PATH" = "1" ]; then
  MARK="# >>> probe-go toolchain >>>"
  BLOCK="$MARK
export PATH=\"$GO_BIN:\$PATH\"
# <<< probe-go toolchain <<<"
  for rc in "$HOME/.bashrc" "$HOME/.profile" "$HOME/.zshrc"; do
    case "$rc" in
      */.zshrc) [ -f "$rc" ] || continue ;;
      *) [ -f "$rc" ] || : >"$rc" ;;
    esac
    if grep -qF "$MARK" "$rc" 2>/dev/null; then
      c_ok "已存在，跳过：$rc"
    else
      printf '\n%s\n' "$BLOCK" >>"$rc"
      c_ok "已写入：$rc"
    fi
  done
  c_warn "新终端生效；当前终端请执行： export PATH=\"$GO_BIN:\$PATH\""
fi

step "版本确认"
"$GO_BIN/go" version
"$GO_BIN/go" env GOROOT GOPATH GOVERSION

# ---------------------------------------------------------------------------
# 5. Go 环境变量
# ---------------------------------------------------------------------------
step "配置 Go 环境变量"
if url_ok "https://goproxy.cn/github.com/gin-gonic/gin/@v/list"; then
  "$GO_BIN/go" env -w GOPROXY=https://goproxy.cn,direct
  c_ok "GOPROXY = https://goproxy.cn,direct"
else
  c_warn "goproxy.cn 不可达，未修改 GOPROXY"
fi
"$GO_BIN/go" env -w GOTOOLCHAIN=local       # go.mod 声明 1.26（下限），锁死避免意外联网
c_ok "GOTOOLCHAIN = local"
c_ok "GOMODCACHE = $("$GO_BIN/go" env GOMODCACHE)"

# ---------------------------------------------------------------------------
# 6. 辅助工具（可选）
# ---------------------------------------------------------------------------
if [ "$WITH_TOOLS" = "1" ]; then
  step "安装辅助 Go 工具"
  GOBIN_DIR="$("$GO_BIN/go" env GOPATH)/bin"
  PATH="$GOBIN_DIR:$PATH"; export PATH
  for pkg in \
    golang.org/x/vuln/cmd/govulncheck@latest \
    honnef.co/go/tools/cmd/staticcheck@latest \
    mvdan.cc/gofumpt@latest \
    golang.org/x/tools/cmd/goimports@latest ; do
    printf '    -> %s\n' "$pkg"
    if "$GO_BIN/go" install "$pkg"; then c_ok "$pkg"; else c_warn "$pkg 安装失败（可稍后重试）"; fi
  done
  c_ok "工具目录：$GOBIN_DIR"
  case ":$PATH:" in *":$GOBIN_DIR:"*) : ;; esac
  if [ "$WRITE_PATH" = "1" ] && ! grep -qF "$GOBIN_DIR" "$HOME/.bashrc" 2>/dev/null; then
    printf '\nexport PATH="%s:$PATH"\n' "$GOBIN_DIR" >>"$HOME/.bashrc"
    c_ok "已把 GOBIN 追加到 ~/.bashrc"
  fi
fi

# ---------------------------------------------------------------------------
# 7. 构建本项目
# ---------------------------------------------------------------------------
if [ "$SKIP_BUILD" = "0" ]; then
  SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
  PROJ_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
  if [ -f "$PROJ_ROOT/go.mod" ]; then
    step "首次拉取依赖并构建（$PROJ_ROOT）"
    cd "$PROJ_ROOT"
    "$GO_BIN/go" mod tidy
    "$GO_BIN/go" mod download || c_warn "go mod download 失败（依赖未全部落入 GOMODCACHE，构建时可能仍需联网）"
    "$GO_BIN/go" vet ./... || c_warn "go vet 有告警"
    mkdir -p bin
    if CGO_ENABLED=0 "$GO_BIN/go" build -trimpath -ldflags "-s -w" -o bin/probe-master ./cmd/probe-master \
       && CGO_ENABLED=0 "$GO_BIN/go" build -trimpath -ldflags "-s -w" -o bin/probe-agent ./cmd/probe-agent; then
      c_ok "构建完成：bin/probe-master、bin/probe-agent"
      ls -lh bin/
    else
      c_err "构建失败，请查看上方编译错误"
    fi
  else
    c_warn "未在 $PROJ_ROOT 找到 go.mod，跳过构建"
  fi
fi

step "完成"
cat <<'EOF'
下一步：
  1) 新终端执行 go version（或先 export PATH）
  2) cp configs/master.yaml.example master.yaml 并修改 data_dir / 监听地址
  3) ./bin/probe-master init-admin --config ./master.yaml
  4) ./bin/probe-master --config ./master.yaml
详见 docs/03-工具链安装与环境搭建.md
EOF
