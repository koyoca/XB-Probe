#!/bin/sh
# XB-Probe —— Master 免编译一键部署（Linux / systemd）
#
# 与 install-master.sh 的区别只有一处：**机器上不需要 Go、不需要 git、不编译**。
# 它下载的是 GitHub Releases 里已经编好的 probe-master，因此：
#   · 1 核小内存的机器也能装（编译要几分钟 + 几百 MB 构建缓存，这台机器不用付）
#   · 没有 Go 工具链、没有 gcc、只读根文件系统都能跑
#   · 拿到的是发布者编的那一份，与你本地编的可能不是同一个二进制
#     （这条既是优点也是代价：要自己编就回去用 install-master.sh）
#
# 用法（stdin 必须留给交互，所以**不要**用 `curl … | sh`）：
#
#   # 推荐：先看再跑
#   curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master-bin.sh -o install-master-bin.sh
#   sudo sh install-master-bin.sh
#
#   # 或者一条命令
#   sudo bash <(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master-bin.sh)
#
#   # 注意：`sh -c "…"` 之后的第一个词会成为 $0，**不是**参数。
#   # 要透传参数必须插一个占位名（下面那个 xb），否则参数会被拼进脚本文本，
#   # 表现为莫名其妙的"需要 root"：
#   sudo sh -c "$(curl -fsSL <URL>)" xb --version v0.1.0 --tls-mode acme --tls-domain status.example.com
#
# 常用参数：
#   --version <vX.Y.Z>   指定版本（默认取最新 Release）
#   --repo <owner/repo>  仓库（默认 koyoca/XB-Probe）
#   --mirror <URL前缀>   加速下载（如 https://ghfast.top/https://github.com）
#   --arch <amd64|arm64> 覆盖架构探测（默认按 uname -m）
#   --no-service         只装不注册服务（容器里用；不需要 root）
#   --yes                不提问，全部取给定参数/默认值
#   --dry-run            下载并校验，但不安装、不改动系统
#   -h, --help           显示本帮助
#
# 其余参数原样透传给 scripts/deploy-master.sh（--public-origin、--admin-origin、
# --admin-user、--public-listen、--admin-listen、--dir、--data、--bin-dir、
# --dist-base、--force-config，以及全部 --tls-* 与 --acme-* 参数）。

set -eu

REPO_RAW="${XB_REPO:-koyoca/XB-Probe}"
VERSION="${XB_VERSION:-}"
MIRROR="${XB_MIRROR:-}"
ARCH_OVERRIDE="${XB_ARCH:-}"
NO_SERVICE=""
NON_INTERACTIVE=""
DRY_RUN=""

# 透传给 deploy-master.sh 的参数：本脚本**不解析**它们，原样转发。
# 刻意不在这里再写一份默认值（配置目录、监听地址等）：两处各写一份默认值，
# 迟早会漂移，而"哪一份生效"取决于调用方式，是最难查的一类问题。
PASSTHRU=""

usage() {
	cat <<'EOF'
install-master-bin.sh —— XB-Probe Master 免编译部署（从 Releases 取预编译二进制）

  --version <vX.Y.Z>    指定版本（默认最新 Release）
  --repo <owner/repo>   仓库（默认 koyoca/XB-Probe）
  --mirror <URL前缀>    加速下载
  --arch <amd64|arm64>  覆盖架构探测
  --no-service          只装不注册服务（不需要 root）
  --yes                 不提问
  --dry-run             只下载校验，不安装
  -h, --help            显示本帮助

  其余参数透传给 scripts/deploy-master.sh：
  --public-origin/--admin-origin/--admin-user/--public-listen/--admin-listen/
  --dir/--data/--bin-dir/--dist-base/--force-config/
  --tls-mode/--tls-cert/--tls-key/--tls-domain/--tls-ip/--acme-email/--acme-ca/--acme-challenge
EOF
}

say() { printf '==> %s\n' "$*"; }
warn() { printf '警告：%s\n' "$*" >&2; }
err() { printf '错误：%s\n' "$*" >&2; }

# download <URL>：curl 或 wget 二选一，把内容写到 stdout。
# 失败时不打印 HTML 错误页——那会让后面的 sha256 校验报出"被篡改"这种
# 完全指错方向的结论（真原因常常只是 404）。
download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q --show-progress -O - "$1" 2>/dev/null || wget -q -O - "$1"
	else
		err "找不到 curl 或 wget，无法下载。"
		exit 1
	fi
}

# sha256_of <文件>：优先 GNU coreutils 的 sha256sum，macOS/BSD 退到 shasum。
# 只输出 64 位十六进制串，便于直接比对。
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		err "找不到 sha256sum / shasum，无法校验下载内容。"
		exit 1
	fi
}

# ---------------------------------------------------------------------------
# 参数解析
# ---------------------------------------------------------------------------

while [ $# -gt 0 ]; do
	case "$1" in
	--version) VERSION="${2:-}"; shift 2 ;;
	--repo) REPO_RAW="${2:-}"; shift 2 ;;
	--mirror) MIRROR="${2:-}"; shift 2 ;;
	--arch) ARCH_OVERRIDE="${2:-}"; shift 2 ;;
	--no-service) NO_SERVICE="1"; PASSTHRU="$PASSTHRU --no-service"; shift ;;
	# --yes 只影响本脚本的"是否允许交互"判断，**不能透传**：
	# deploy-master.sh 对未知参数是硬失败（"未知参数" + exit 2），
	# 于是"加了 --yes 反而装不上"——一个只在非交互场景才出现的坑。
	--yes | -y) NON_INTERACTIVE="1"; shift ;;
	--dry-run) DRY_RUN="1"; shift ;;
	-h | --help) usage; exit 0 ;;
	--)
		shift
		while [ $# -gt 0 ]; do PASSTHRU="$PASSTHRU $1"; shift; done
		;;
	*) PASSTHRU="$PASSTHRU $1"; shift ;;
	esac
done

# ---------------------------------------------------------------------------
# 0) 环境前提
# ---------------------------------------------------------------------------

case "$(uname -s)" in
Linux) : ;;
*)
	err "本脚本只支持 Linux（Master 的服务托管是 systemd）。"
	err "Windows 请用 irm 取 install-master.ps1；FreeBSD 请用 install-master.sh。"
	exit 2
	;;
esac

# 架构探测：uname -m 的取值在不同发行版/内核上并不统一，
# x86_64 与 amd64 是同一个东西，aarch64 与 arm64 也是。
if [ -n "$ARCH_OVERRIDE" ]; then
	ARCH="$ARCH_OVERRIDE"
else
	case "$(uname -m)" in
	x86_64 | amd64) ARCH="amd64" ;;
	aarch64 | arm64) ARCH="arm64" ;;
	*)
		err "本仓库只发布 linux/amd64 与 linux/arm64 两种 Master 产物，当前架构是 $(uname -m)。"
		err "请改用会现场编译的 install-master.sh（它支持更窄的架构，只要 Go 能编出来）。"
		exit 2
		;;
	esac
fi

case "$ARCH" in
amd64 | arm64) : ;;
*)
	err "--arch 只能是 amd64 或 arm64（收到 $ARCH）。"
	exit 2
	;;
esac

if [ "$(id -u)" -ne 0 ] && [ -z "$NO_SERVICE" ]; then
	cat >&2 <<EOF
错误：需要 root（要写 /usr/local/bin 与注册 systemd 服务）。

请这样跑（注意 stdin 要留给交互，所以别用管道）：
    sudo sh -c "\$(curl -fsSL <本脚本的 URL>)"
或者先下载再执行：
    curl -fsSL <本脚本的 URL> -o install-master-bin.sh && sudo sh install-master-bin.sh

容器 / 无 systemd / 只读根文件系统的场景，可以不用 root：
    sh install-master-bin.sh --no-service --dir ./probe-master --data ./probe-master/data --bin-dir ./probe-master/bin
EOF
	exit 1
fi

# 非交互式终端 + 又要提问 = 必然把所有问题答成默认值（deploy-master.sh 会问口令）。
# 与其装出一个"默认值拼起来的"部署，不如立刻停下来说清楚怎么用。
if [ -z "$NON_INTERACTIVE" ] && [ -z "$DRY_RUN" ] && [ ! -t 0 ]; then
	err "标准输入不是终端，无法交互提问。"
	cat >&2 <<'EOF'
两种做法：
  · 要交互：别用管道，改用 `sudo sh -c "$(curl -fsSL <URL>)" xb`
  · 不交互：加 --yes，并显式给出 --public-origin（口令设 PROBE_ADMIN_PASSWORD）
EOF
	exit 2
fi

# ---------------------------------------------------------------------------
# 1) 归一化仓库名与版本
# ---------------------------------------------------------------------------

# 允许 --repo 写三种形式：owner/repo、https://github.com/owner/repo、
# https://github.com/owner/repo.git —— 归一成 owner/repo。
REPO="$(printf '%s' "$REPO_RAW" |
	sed -e 's|^https\?://[^/]*/||' -e 's|\.git$||' -e 's|^/||' -e 's|/$||')"
case "$REPO" in
*/*) : ;;
*)
	err "--repo 应形如 owner/repo（收到 $REPO_RAW）。"
	exit 2
	;;
esac

# 镜像前缀只管下载：API 与 raw 仍走原始域名（镜像站通常不代理 api.github.com，
# 拿它去问"最新版本"会得到 404，而那看起来像"仓库不存在"）。
ASSET_BASE="https://github.com/$REPO/releases/download"
RAW_BASE="https://raw.githubusercontent.com/$REPO"
if [ -n "$MIRROR" ]; then
	ASSET_BASE="${MIRROR%/}/https://github.com/$REPO/releases/download"
fi

if [ -z "$VERSION" ]; then
	say "查询最新版本（$REPO）"
	_latest="$(download "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
	if [ -z "$_latest" ]; then
		err "取不到最新 Release（可能是仓库还没有发布任何版本，或触发了 GitHub API 的匿名限流）。"
		err "可以显式指定：--version v0.1.0"
		exit 1
	fi
	VERSION="$_latest"
fi

# 统一成带 v 的标签：二进制资产名与标签无关，但 raw/deploy 脚本要按标签取，
# 少一个 v 就会 404，而 404 看起来像"版本不存在"。
case "$VERSION" in
v*) TAG="$VERSION" ;;
*) TAG="v$VERSION" ;;
esac
say "目标版本 $TAG（linux/$ARCH）"

# ---------------------------------------------------------------------------
# 2) 下载 + 校验
# ---------------------------------------------------------------------------

TMP="$(mktemp -d 2>/dev/null || echo "/tmp/xb-bin.$$")"
mkdir -p "$TMP"
# 中断也不能把半个二进制留在 /tmp 里（下次有人误以为那是完整的）。
trap 'rm -rf "$TMP"' EXIT INT TERM HUP

BIN_NAME="probe-master_linux_${ARCH}"
say "下载 $BIN_NAME"
download "$ASSET_BASE/$TAG/$BIN_NAME" >"$TMP/$BIN_NAME" || {
	err "下载失败：$ASSET_BASE/$TAG/$BIN_NAME"
	err "确认版本号是否存在、该架构是否已发布（本仓库只发 amd64/arm64）。"
	exit 1
}
say "下载 $BIN_NAME 的校验和"
download "$ASSET_BASE/$TAG/SHA256SUMS.master" >"$TMP/SHA256SUMS.master" || {
	err "下载失败：$ASSET_BASE/$TAG/SHA256SUMS.master"
	exit 1
}

# 只挑自己要的那一行来校验。不整份 `sha256sum -c`：那份文件里有两种架构，
# 另一个架构的产物并不在本地，整份校验必然报 FAILED（而原因看起来像被篡改）。
WANT="$(awk -v n="$BIN_NAME" '$2 == n || $2 == "*" n {print $1}' "$TMP/SHA256SUMS.master" | head -1)"
if [ -z "$WANT" ]; then
	err "SHA256SUMS.master 里没有 $BIN_NAME 的条目——发布产物与脚本预期不一致。"
	exit 1
fi
case "$WANT" in
*[!0-9a-fA-F]* | "")	err "校验和格式不对（$WANT）。很可能下载到的是错误页而不是校验和文件。"; exit 1 ;;
esac
[ "${#WANT}" -eq 64 ] || {
	err "校验和长度应为 64 位十六进制，实际 ${#WANT} 位（$WANT）。"
	exit 1
}

GOT="$(sha256_of "$TMP/$BIN_NAME")"
if [ "$GOT" != "$WANT" ]; then
	err "校验和不匹配，已丢弃下载内容。"
	err "  期望 $WANT"
	err "  实际 $GOT"
	exit 1
fi
say "校验通过（sha256 $GOT）"
chmod 0755 "$TMP/$BIN_NAME"

# 真跑一次 --version 再安装：架构对了但根本没编出来、或依赖动态库的二进制，
# 会在这里就暴露，而不是等 systemd 反复重启之后才从日志里翻出来。
if ! "$TMP/$BIN_NAME" version >/dev/null 2>&1; then
	err "下载到的二进制无法执行（$BIN_NAME version 失败）。"
	err "如果报 cannot execute binary file，说明该架构的产物有问题，请到 Releases 反馈。"
	exit 1
fi
say "二进制自检通过：$("$TMP/$BIN_NAME" version)"

# ---------------------------------------------------------------------------
# 3) 取同一标签的部署脚本
# ---------------------------------------------------------------------------

# 部署逻辑只有一份（scripts/deploy-master.sh），这里按**同一个标签**取回来执行。
# 这样做有两个好处：① 不必把 600 行配置/服务逻辑抄第二遍（抄了就是两处要一起改）；
# ② 二进制与部署脚本版本严格一致——拿新二进制配旧脚本是"配置键对不上"的经典来源。
say "取部署脚本（$TAG）"
DEPLOY="$TMP/deploy-master.sh"
download "$RAW_BASE/$TAG/scripts/deploy-master.sh" >"$DEPLOY" || {
	err "取不到 $RAW_BASE/$TAG/scripts/deploy-master.sh"
	err "旧版本标签里可能还没有这个脚本（它是与免编译部署一起加的）。"
	exit 1
}
if ! grep -q '^#!/bin/sh' "$DEPLOY"; then
	err "取回的 deploy-master.sh 不像脚本（没有 shebang），已中止。"
	exit 1
fi

if [ -n "$DRY_RUN" ]; then
	echo >&2
	say "（dry-run）下载与校验已完成，未安装、未改动系统。"
	printf '  %-10s %s\n' "二进制" "${TMP}/${BIN_NAME}（已通过 sha256 与 version 自检）" >&2
	printf '  %-10s %s\n' "部署脚本" "$DEPLOY" >&2
	printf '  %-10s %s\n' "下一步" "去掉 --dry-run 即执行：sh $DEPLOY --bin <二进制> $PASSTHRU" >&2
	exit 0
fi

# ---------------------------------------------------------------------------
# 4) 交给 deploy-master.sh 完成落盘、服务与健康检查
# ---------------------------------------------------------------------------

say "开始部署（接下来的输出来自 scripts/deploy-master.sh）"
# 刻意不用 exec 顶替进程：exec 之后 EXIT trap 不会跑，临时目录（含那个几十 MB
# 的二进制）就永远留在 /tmp 里。让本进程等它结束，退出时 trap 负责清理。
# $PASSTHRU 不加引号是有意的——它是要按空白切成多个参数传递的，不是一整段文本。
# shellcheck disable=SC2086
if ! sh "$DEPLOY" --bin "$TMP/$BIN_NAME" $PASSTHRU; then
	err "部署脚本执行失败（上面是它的输出）。"
	err "临时文件已清理，系统可能只改了一半——修好原因后重跑本脚本即可（本脚本与部署脚本都是幂等的）。"
	exit 1
fi
