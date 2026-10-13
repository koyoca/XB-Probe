#!/bin/sh
# xb-probe-agent 一键安装脚本（文档 12.3）
#
# 安全说明（12.8）：
#   - 本脚本与下载文件只是静态内容，不含任何节点秘密。
#   - 注册码仅出现在命令行或环境变量中，单次使用、短时有效、绑定节点，安装后即失效。
#   - 二进制下载严格校验 TLS；SHA256SUMS 始终取自 Master，即使二进制来自第三方镜像也无法被替换。
#   - 对管道执行有顾虑时，请使用分步命令：先下载、查看、再执行。
#
# 用法：sudo sh install.sh --code ABCD-EFGH-JKLM [--master https://m.example.com]
#      注册码亦可用环境变量 PROBE_CODE 传入，避免出现在进程列表。

set -eu

MASTER_URL="${PROBE_MASTER_URL:-__MASTER_URL__}"
DIST_BASE="${PROBE_DIST_BASE:-__DIST_BASE__}"
SPKI="${PROBE_SPKI_SHA256:-__SPKI__}"
MINISIGN_PUB="${PROBE_MINISIGN_PUB:-__MINISIGN_PUB__}"
DIR="${PROBE_DIR:-}"
CODE="${PROBE_CODE:-}"
PUBIP="${PROBE_PUBLIC_IP_URL:-}"
PLATFORM=""

usage() {
  cat <<'EOF'
xb-probe-agent 安装脚本
  --code <注册码>       一次性注册码（必需）
  --master <URL>        Master 地址（默认内置）
  --spki-sha256 <指纹>  自签证书时固定 Master 公钥指纹（可重复）
  --minisign-pub <公钥> minisign 公钥（校验 SHA256SUMS；默认内置，留空则跳过）
  --dir <目录>          自定义配置目录
  --public-ip-url <URL> 公网地址探测服务（默认关闭），见下方说明
  -h, --help            显示帮助

关于 --public-ip-url：Master 与 Agent 同机部署时，Master 看到的来源地址是
127.0.0.1，节点地址栏里就永远是回环地址；此时只有节点自己出网问一句才能拿到
真正的公网地址。打开它会**定期访问第三方服务**（默认 15 分钟一次），
因此需要你显式给出地址，例如 --public-ip-url https://api.ipify.org
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --code)   CODE="${2:-}"; shift 2 ;;
    --master) MASTER_URL="${2:-}"; shift 2 ;;
    --spki-sha256) SPKI="${2:-}"; shift 2 ;;
    --minisign-pub) MINISIGN_PUB="${2:-}"; shift 2 ;;
    --dir)    DIR="${2:-}"; shift 2 ;;
    --public-ip-url) PUBIP="${2:-}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数：$1" >&2; usage; exit 2 ;;
  esac
done

# 1) 检查 root
if [ "$(id -u)" -ne 0 ]; then
  echo "错误：请使用 root 或 sudo 执行。" >&2
  exit 1
fi

if [ -z "$CODE" ]; then
  echo "错误：缺少注册码 --code。" >&2
  usage
  exit 2
fi

# 拒绝非 https 的 Master 地址（S7）
case "$MASTER_URL" in
  https://*) : ;;
  *127.0.0.1*|*localhost*) : ;;  # 本机回环开发放行
  *) echo "错误：Master 地址必须是 https://。" >&2; exit 2 ;;
esac

# 2) 识别系统与架构
OS_RAW="$(uname -s)"
ARCH_RAW="$(uname -m)"
case "$OS_RAW" in
  Linux)   OS="linux" ;;
  Darwin)  OS="darwin" ;;
  FreeBSD) OS="freebsd" ;;
  *) echo "错误：不支持的系统 $OS_RAW。" >&2; exit 2 ;;
esac
case "$ARCH_RAW" in
  x86_64|amd64) OS_ARCH="amd64" ;;
  aarch64|arm64) OS_ARCH="arm64" ;;
  armv7l) OS_ARCH="armv7" ;;
  armv6l) OS_ARCH="armv6" ;;
  i386|i686) OS_ARCH="386" ;;
  riscv64) OS_ARCH="riscv64" ;;
  *) echo "错误：不支持的架构 $ARCH_RAW。" >&2; exit 2 ;;
esac
PLATFORM="${OS}_${OS_ARCH}"
echo "==> 平台：$PLATFORM"

DOWNLOAD=""
if command -v curl >/dev/null 2>&1; then
  DOWNLOAD="curl -fsSL"
elif command -v wget >/dev/null 2>&1; then
  DOWNLOAD="wget -qO-"
else
  echo "错误：需要 curl 或 wget。" >&2
  exit 2
fi

TMP="$(mktemp -d 2>/dev/null || mktemp -d -t probe)"
if [ -z "$TMP" ] || [ ! -d "$TMP" ]; then
  # BusyBox 的 mktemp 在部分极简 Alpine/LXC 镜像里不可用，退回到 /tmp 下手动建目录。
  TMP="/tmp/probe-install.$$"
  mkdir -p "$TMP" || { echo "错误：无法创建临时目录。" >&2; exit 1; }
fi
BIN="$TMP/xb-probe-agent"
trap 'rm -rf "$TMP"' EXIT INT TERM

# 3) 下载二进制与校验和
if [ "$OS" = "windows" ]; then
  NAME="xb-probe-agent_${OS}_${OS_ARCH}.exe"
else
  NAME="xb-probe-agent_${OS}_${OS_ARCH}"
fi
echo "==> 下载 $NAME"
$DOWNLOAD "$DIST_BASE/$NAME" > "$BIN" || { echo "错误：下载失败。" >&2; exit 1; }

# SHA256SUMS 始终取自 Master（12.8）
SUMS="$TMP/SHA256SUMS"
$DOWNLOAD "$MASTER_URL/dist/SHA256SUMS" > "$SUMS" || { echo "错误：无法获取校验和。" >&2; exit 1; }

# 3.5) minisign 验签（6.8）——必须在用 SHA256SUMS 校验二进制**之前**完成。
#
# 为什么这一步不能省：SHA-256 只能证明"这个文件和 SHA256SUMS 里记的一致"。
# 如果攻击者控制了 DIST_BASE 指向的镜像，他可以把 SHA256SUMS 一起换掉，
# 于是哈希校验"全部通过"，却装上了投毒的二机制。minisign 把信任锚从
# "镜像 URL 可信"收敛为"这份校验和出自维护者的离线私钥"。
#
# 关于降级策略（重要）：minisign 不是所有系统都预装，而这个脚本要在
# Alpine/LXC/distroless 等极简环境里跑。因此这里**不强制要求** minisign 存在：
#   - 内置了公钥 且 系统里有 minisign → 强制验签，失败即中止；
#   - 内置了公钥 但 没有 minisign  → 明确警告"未能验签"后继续（否则会
#     在大量正常系统上直接装不上，把安全改进变成可用性事故）；
#   - 没内置公钥               → 静默跳过（未启用该功能）。
# 要"强制必须验签"的部署方，请用 --minisign-pub 显式传入公钥并自行保证
# 目标系统已装 minisign（或在内部镜像前置一层校验）。
if [ -n "$MINISIGN_PUB" ]; then
  SIG="$TMP/SHA256SUMS.minisig"
  if $DOWNLOAD "$MASTER_URL/dist/SHA256SUMS.minisig" > "$SIG" 2>/dev/null; then
    if command -v minisign >/dev/null 2>&1; then
      # 公钥既可以是一行公钥文本，也可以是本地文件路径。
      case "$MINISIGN_PUB" in
        *"untrusted comment:"*|RW*) PUBFILE="$TMP/minisign.pub"; printf '%s\n' "$MINISIGN_PUB" > "$PUBFILE" ;;
        *) PUBFILE="$MINISIGN_PUB" ;;
      esac
      if minisign -V -p "$PUBFILE" -m "$SUMS" >/dev/null 2>&1; then
        echo "==> SHA256SUMS 签名校验通过"
      else
        echo "错误：SHA256SUMS 的 minisign 签名校验失败，已中止安装。" >&2
        echo "      这可能意味着镜像被篡改，或公钥配置有误。" >&2
        exit 1
      fi
    else
      echo "警告：系统里没有 minisign，无法验证 SHA256SUMS 的签名。" >&2
      echo "      本次仅依赖 SHA-256 与 TLS；如需强制验签请先安装 minisign。" >&2
    fi
  else
    echo "警告：未能获取 SHA256SUMS.minisig，跳过签名校验。" >&2
  fi
fi

# 4) 校验 SHA-256
EXPECTED="$(awk -v n="$NAME" '$2==n || $2=="*"n {print $1}' "$SUMS" | head -n1)"
if [ -z "$EXPECTED" ]; then
  echo "错误：校验和中找不到 $NAME。" >&2; exit 1
fi
# 末尾的 sed 剔掉 GNU coreutils 的"已转义"标记（路径含反斜杠时它在整行前加 `\`，
# 于是拿到 `\f01d9…` 而非 `f01d9…`，比对失败且把原因指向"被篡改"）。
# 本脚本跑在 Linux 上一般不会触发，但校验路径上的错误诊断代价太高，不值得赌。
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "$BIN" | awk '{print $1}' | sed 's/^\\//')"
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL="$(shasum -a 256 "$BIN" | awk '{print $1}' | sed 's/^\\//')"
elif command -v sha256 >/dev/null 2>&1; then
  ACTUAL="$(sha256 -q "$BIN")"
elif command -v openssl >/dev/null 2>&1; then
  ACTUAL="$(openssl dgst -sha256 "$BIN" | awk '{print $NF}')"
else
  echo "错误：找不到可用的 sha256 工具。" >&2; exit 1
fi
if [ "$EXPECTED" != "$ACTUAL" ]; then
  rm -f "$BIN"
  echo "错误：SHA-256 校验失败，已删除下载文件。" >&2
  exit 1
fi
chmod 0755 "$BIN"

# 5) 安装到 /usr/local/bin（只读根文件系统改用 /opt/xb-probe-agent）
#
# 注意：Alpine / BusyBox 的精简镜像里**没有 `install` 命令**（那是 coreutils 的），
# 只有 cp。因此优先用 install，没有就退回 cp + chmod。同理 /usr/local/bin
# 在部分只读 rootfs（如 LXC 的只读模板、distroless）上不可写，需要退到 /opt。
TARGET_DIR="/usr/local/bin"
if ! mkdir -p "$TARGET_DIR" 2>/dev/null || [ ! -w "$TARGET_DIR" ]; then
  TARGET_DIR="/opt/xb-probe-agent"
  mkdir -p "$TARGET_DIR" || { echo "错误：无法创建安装目录 $TARGET_DIR。" >&2; exit 1; }
fi
if command -v install >/dev/null 2>&1; then
  install -m 0755 "$BIN" "$TARGET_DIR/xb-probe-agent"
else
  cp -f "$BIN" "$TARGET_DIR/xb-probe-agent" && chmod 0755 "$TARGET_DIR/xb-probe-agent"
fi
echo "==> 已安装到 $TARGET_DIR/xb-probe-agent"

# 6) 注册系统服务
#
# **不再用 `cat /proc/1/comm` 去猜**。这条启发式在 Alpine 上恰好错在最常见的情况上：
# PID 1 是 busybox 的 `init`（Alpine 默认），而 OpenRC 是被 inittab 拉起来的 ——
# "PID 1 叫什么"与"本机能不能注册服务"根本不是一回事。原来那版把不在白名单里的
# 一律塞 `--no-service`，于是在有完整 OpenRC 的 Alpine 上用户拿到一个
# "装好了、但没有服务"的节点，只能自己前台 `run`；他还以为是自己权限不够。
#
# 判断权交还给 **xb-probe-agent 自己**（service.Detect()）：它认得出来就注册服务；
# 认不出来（真正的精简容器镜像）会打印可照做的前台/容器托管命令并**照常完成安装**
# （退出码 0）。这样"能不能装服务"只有一个实现，不会两边漂移。
#
# 确实想只写配置不注册服务时，用显式开关：PROBE_NO_SERVICE=1
NO_SVC=""
[ "${PROBE_NO_SERVICE:-}" = "1" ] && NO_SVC="--no-service"

set -- install --master "$MASTER_URL" --code "$CODE"
[ -n "$DIR" ] && set -- "$@" --dir "$DIR"
[ -n "$SPKI" ] && set -- "$@" --spki-sha256 "$SPKI"
[ -n "$PUBIP" ] && set -- "$@" --public-ip-url "$PUBIP"
[ -n "$NO_SVC" ] && set -- "$@" "$NO_SVC"
"$TARGET_DIR/xb-probe-agent" "$@"

# 7) 等待首次上报（最多 15 秒）
echo "==> 等待首次上报…"
i=0
while [ $i -lt 15 ]; do
  if "$TARGET_DIR/xb-probe-agent" version >/dev/null 2>&1; then break; fi
  i=$((i+1)); sleep 1
done
echo "==> 安装完成。若页面未显示节点，请查看服务日志（systemd：journalctl -u xb-probe-agent；OpenRC：rc-service xb-probe-agent status 与 /var/log/xb-probe-agent.log）。"
