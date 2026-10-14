#!/bin/sh
# XB-Probe —— Master 整机一键部署
#
# 在**全新的 Linux 机器**上一条命令完成：检查/安装依赖（git、Go）→ 拉取源码 →
# 编译 → 交互式配置 → 注册服务并启动 → 健康检查。整个过程幂等，重复执行即"升级"。
#
# 用法（stdin 必须留给交互，所以**不要**用 `curl … | sh`）：
#
#   # 推荐：先看再跑（看得见的脚本才谈得上信任）
#   curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master.sh -o install-master.sh
#   sudo sh install-master.sh
#
#   # 或者一条命令（bash 才有 <() 进程替换）
#   sudo bash <(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master.sh)
#
# 为什么不用 `curl … | sudo sh`：管道会把 stdin 占掉，脚本里的 read 立刻拿到 EOF，
# 于是所有交互项都取了默认值（甚至空口令）——这正是"一键装完却登不进后台"的常见原因。
# 同理，容器/无 root 场景请显式加 --no-service，别指望它自己去猜。
#
# 完全非交互（批量/CI）：
#   sudo sh install-master.sh --yes --public-origin https://status.example.com \
#        --tls-mode acme --tls-domain status.example.com
#   口令仍由 deploy-master.sh 交互输入；要彻底无人值守就设 PROBE_ADMIN_PASSWORD。
#
# 常用参数：
#   --repo <URL>        源码仓库（默认 https://github.com/koyoca/XB-Probe.git）
#   --ref <分支/标签>    默认 main
#   --src <目录>        源码目录（默认 /opt/xb-probe；默认保留，便于下次升级）
#   --go-version <版本>  缺少 Go 时下载的版本（默认 1.27.2）
#   --goproxy <URL>     模块代理（默认不动你的配置；失败时会提示国内镜像）
#   --no-deps           不自动安装缺失的依赖，只报错
#   --yes               不提问，全部取给定参数/默认值
#   --dry-run           只打印将要执行的步骤，不改动系统
#   --uninstall         卸载（连数据一起清；加 --keep-data 保留数据目录）
#   --keep-data         与 --uninstall 同用
#   --upgrade-only      只升级 Master 本体（编译之后只换二进制，不问站点/证书/账号）
#   --reset-password    只重置管理员密码（不编译、不卸载、不清数据）
#   -h, --help          显示本帮助
#
# 其余参数原样透传给 scripts/deploy-master.sh（--public-origin、--admin-origin、
# --admin-user、--public-listen、--admin-listen、--dir、--data、--bin-dir、
# --dist-base、--no-service、--force-config，以及全部 --tls-* 与 --acme-* 参数）。
#
# dns-01 的 DNS 凭据不走命令行（会进 shell 历史），用环境变量：
#   PROBE_ACME_DNS_PROVIDER（默认 cloudflare）、PROBE_ACME_DNS_TOKEN
#
# 卸载**不碰源码与 Go**：它从仓库取回 scripts/deploy-master.sh 执行 --uninstall，
# 所以不需要先装一套工具链，也不会因为 --src 不在而失败。
#
# 问答分两处，不是漏了：TLS 方式、对外地址、管理员账号在这里问（排在装依赖与编译
# 之前，答案错了就不必动系统）；**监听端口**由 scripts/deploy-master.sh 在落盘前问。
# 端口的默认值只有部署脚本那一份来源，跟着它问才不会出现"提示写 8080、实际听 8081"。

set -eu

REPO="${XB_REPO:-https://github.com/koyoca/XB-Probe.git}"
REF="${XB_REF:-main}"
SRC="${XB_SRC:-/opt/xb-probe}"
GO_VERSION="${XB_GO_VERSION:-1.27.2}"
GOPROXY_OVERRIDE="${XB_GOPROXY:-}"
NO_DEPS=""
NON_INTERACTIVE=""
DRY_RUN=""

# 透传给 deploy-master.sh 的值。这里**刻意不重复** deploy-master.sh 的默认值
# （配置目录、监听地址等）：两处各写一份默认值，迟早在两边漂移，而"哪一份生效"
# 取决于调用方式，是最难查的一类问题。留空即代表"用对方的默认值"。
PUBLIC_ORIGIN="${XB_PUBLIC_ORIGIN:-}"
ADMIN_ORIGIN="${XB_ADMIN_ORIGIN:-}"
ADMIN_USER="${XB_ADMIN_USER:-}"
PUBLIC_LISTEN=""
ADMIN_LISTEN=""
DIR=""
DATA=""
BIN_DIR=""
DIST_BASE=""
# 发布文件（Agent 二进制的自举来源）的拉取范围与来源前缀。留空 = 交给
# deploy-master.sh 按二进制版本给默认值并提问（默认值只有一处来源）。
DIST_PULL=""
DIST_SOURCE=""
NO_DIST_PULL=""
TLS_MODE=""
TLS_CERT=""
TLS_KEY=""
TLS_DOMAINS=""
TLS_IPS=""
ACME_EMAIL=""
ACME_CA=""
ACME_CHALLENGE=""
# 谁终结 TLS：master（Master 自己）/ caddy（部署脚本装 Caddy 反代）。
# 留空 = 由 deploy-master.sh 按 TLS 方式给出建议值并提问（默认值只有一处来源）。
TLS_TERMINATOR=""
PROXY_DOMAINS=""
NO_SERVICE=""
FORCE_CONFIG=""
UNINSTALL=""
# 只更新 Agent 发布文件（不动 Master 的二进制、配置与服务）。
RELEASE_ONLY=""
# 只升级 Master 本体：照旧取源码 + 编译（本脚本的意义就在这），但**不再问**
# 站点/证书/管理员那一套，编译完只把二进制换上去。
UPGRADE_ONLY=""
# 只重置管理员密码：不需要源码、不需要 Go，所以和卸载一样走"编译之前"的早退通路。
RESET_PASSWORD=""
# 改名前老布局（probe-master 时代）的迁移开关：原样转给部署脚本，由它停旧服务、
# 搬数据与配置（目标已有内容绝不覆盖）。不加速改装出来的是一套空库。
MIGRATE_LEGACY=""
KEEP_DATA=""

# go.mod 声明的最低 Go 版本。低于它构建会直接失败，所以先在依赖检查时拦下，
# 而不是等编译报一堆看不懂的语法错误。
MIN_GO="1.26"

usage() {
	cat <<'EOF'
install-master.sh —— XB-Probe Master 整机一键部署

  --repo <URL>         源码仓库（默认 https://github.com/koyoca/XB-Probe.git）
  --ref <分支/标签>     默认 main
  --src <目录>         源码目录（默认 /opt/xb-probe）
  --go-version <版本>   缺少 Go 时下载的版本（默认 1.27.2）
  --goproxy <URL>      模块代理（默认沿用你的配置）
  --no-deps            不自动安装缺失的依赖
  --yes                不提问，全取默认值
  --dry-run            只打印计划
  --uninstall          卸载（连数据一起清；加 --keep-data 保留数据目录）
  --keep-data          与 --uninstall 同用
  --release-only       只更新 Agent 发布文件（查 GitHub 最新 Release 拉进
                       data_dir/dist/，不动 Master 的二进制/配置/服务）。
                       同样不需要源码与 Go，与 --uninstall 互斥。
  --upgrade-only       只升级 Master 本体：照旧取源码 + 编译，但**不再问**站点
                       地址、证书、监听端口、管理员账号那一套，编译完只把新二进制
                       换上去并重启服务（配置、证书、管理员、data_dir/dist 都不动）。
  --reset-password     只重置管理员密码（不需要源码与 Go，不卸载、不清数据）。
                       口令按 PROBE_ADMIN_PASSWORD / --admin-password / 交互的
                       顺序取；用户名默认 ops，可用 --admin-user 指定。
  --migrate-legacy     改名之前（probe-master 时代）装过的机器用它搬家：搬
                       /var/lib/probe-master 与 /etc/probe-master 到新路径、
                       改写配置里写死的老路径、停用旧单元，然后照常安装。
                       **指定它即等于选了「安装 / 升级」，不会再问模式**。
                       目标已有内容绝不覆盖，可反复重跑。
  -h, --help           显示本帮助

  其余参数透传给 scripts/deploy-master.sh：
  --public-origin/--admin-origin/--admin-user/--public-listen/--admin-listen/
  --dir/--data/--bin-dir/--dist-base/--dist-pull/--dist-source/--no-dist-pull/
  --no-service/--force-config/
  --tls-mode/--tls-cert/--tls-key/--tls-domain/--tls-ip/--acme-email/--acme-ca/--acme-challenge/
  --tls-terminator/--proxy-domain

  dns-01 的凭据用环境变量：PROBE_ACME_DNS_PROVIDER、PROBE_ACME_DNS_TOKEN
EOF
}

say() { printf '==> %s\n' "$*"; }
warn() { printf '警告：%s\n' "$*" >&2; }
err() { printf '错误：%s\n' "$*" >&2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--repo) REPO="${2:-}"; shift 2 ;;
	--ref) REF="${2:-}"; shift 2 ;;
	--src) SRC="${2:-}"; shift 2 ;;
	--go-version) GO_VERSION="${2:-}"; shift 2 ;;
	--goproxy) GOPROXY_OVERRIDE="${2:-}"; shift 2 ;;
	--no-deps) NO_DEPS="1"; shift ;;
	# --yes 除了让本脚本不提问，还要让**部署脚本**也别问（它还会问监听端口，
	# 那些项本脚本不收集）。用环境变量而不是把 --yes 透传：部署脚本可能来自用户
	# 指定的旧 ref，而旧版本对未知参数是硬失败（exit 2），环境变量则新旧都能收。
	--yes | -y)
		NON_INTERACTIVE="1"
		PROBE_NO_ASK=1
		export PROBE_NO_ASK
		shift
		;;
	--dry-run) DRY_RUN="1"; shift ;;
	--public-origin) PUBLIC_ORIGIN="${2:-}"; shift 2 ;;
	--admin-origin) ADMIN_ORIGIN="${2:-}"; shift 2 ;;
	--admin-user) ADMIN_USER="${2:-}"; shift 2 ;;
	--public-listen) PUBLIC_LISTEN="${2:-}"; shift 2 ;;
	--admin-listen) ADMIN_LISTEN="${2:-}"; shift 2 ;;
	--dir) DIR="${2:-}"; shift 2 ;;
	--data) DATA="${2:-}"; shift 2 ;;
	--bin-dir) BIN_DIR="${2:-}"; shift 2 ;;
	--dist-base) DIST_BASE="${2:-}"; shift 2 ;;
	--dist-pull) DIST_PULL="${2:-}"; shift 2 ;;
	--dist-source) DIST_SOURCE="${2:-}"; shift 2 ;;
	--no-dist-pull) NO_DIST_PULL="1"; shift ;;
	--tls-mode) TLS_MODE="${2:-}"; shift 2 ;;
	--tls-cert) TLS_CERT="${2:-}"; shift 2 ;;
	--tls-key) TLS_KEY="${2:-}"; shift 2 ;;
	--tls-domain) TLS_DOMAINS="${2:-}"; shift 2 ;;
	--tls-ip) TLS_IPS="${2:-}"; shift 2 ;;
	--acme-email) ACME_EMAIL="${2:-}"; shift 2 ;;
	--acme-ca) ACME_CA="${2:-}"; shift 2 ;;
	--acme-challenge) ACME_CHALLENGE="${2:-}"; shift 2 ;;
	--tls-terminator) TLS_TERMINATOR="${2:-}"; shift 2 ;;
	--proxy-domain) PROXY_DOMAINS="${2:-}"; shift 2 ;;
	--no-service) NO_SERVICE="1"; shift ;;
	--force-config) FORCE_CONFIG="1"; shift ;;
	--uninstall) UNINSTALL="1"; shift ;;
	--release-only) RELEASE_ONLY="1"; shift ;;
	--upgrade-only) UPGRADE_ONLY="1"; shift ;;
	--reset-password) RESET_PASSWORD="1"; shift ;;
	--migrate-legacy) MIGRATE_LEGACY="1"; shift ;;
	--keep-data) KEEP_DATA="1"; shift ;;
	-h | --help) usage; exit 0 ;;
	*) err "未知参数：$1"; usage; exit 2 ;;
	esac
done

# ---------------------------------------------------------------------------
# 小工具
# ---------------------------------------------------------------------------

# download <url>：把内容写到 stdout。curl / wget 任一即可。
download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --max-time 120 "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO- --timeout=120 "$1"
	else
		return 1
	fi
}

# raw_base <仓库地址> <ref>：把 GitHub 仓库地址换算成 raw 内容前缀（不带尾斜杠）。
# 推导不出来就返回空——调用方据此选择"报错并给替代命令"，而不是瞎猜一个地址
# 去下载一个可能不是我们要的脚本。**下载并执行一个来源不明的脚本**是这条链路上
# 最危险的一步，宁可失败也不猜。
raw_base() {
	case "$1" in
	https://github.com/*) _r="${1#https://github.com/}" ;;
	git@github.com:*) _r="${1#git@github.com:}" ;;
	*) printf ''; return 0 ;;
	esac
	_r="${_r%.git}"
	case "$_r" in
	*/*) : ;;
	*) printf ''; return 0 ;;
	esac
	printf 'https://raw.githubusercontent.com/%s/%s' "$_r" "$2"
}

# ver_ge <要求> <实际>：版本号按 major.minor 比较（够用，且不依赖 sort -V 的差异）。
# 非数字后缀（1.27rc1、1.27.2 之类）一律只取数字部分，避免 cut 出空串再拿去 -ge。
ver_ge() {
	_want_major="$(printf '%s' "$1" | cut -d. -f1 | tr -cd '0-9')"
	_want_minor="$(printf '%s' "$1" | cut -d. -f2 | tr -cd '0-9')"
	_have_major="$(printf '%s' "$2" | cut -d. -f1 | tr -cd '0-9')"
	_have_minor="$(printf '%s' "$2" | cut -d. -f2 | tr -cd '0-9')"
	[ -n "$_want_major" ] && [ -n "$_have_major" ] || return 1
	[ -n "$_want_minor" ] || _want_minor=0
	[ -n "$_have_minor" ] || _have_minor=0
	if [ "$_have_major" -gt "$_want_major" ]; then return 0; fi
	if [ "$_have_major" -lt "$_want_major" ]; then return 1; fi
	[ "$_have_minor" -ge "$_want_minor" ]
}

# go arch（Go 官方 tarball 的架构名，和 uname -m 不是一回事）
go_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) printf 'amd64' ;;
	aarch64 | arm64) printf 'arm64' ;;
	armv7l | armv6l | arm) printf 'armv6l' ;; # Go 只发 linux-armv6l，v6/v7 共用
	i386 | i686) printf '386' ;;
	riscv64) printf 'riscv64' ;;
	ppc64le) printf 'ppc64le' ;;
	s390x) printf 's390x' ;;
	loongarch64) printf 'loong64' ;;
	*) printf '' ;;
	esac
}

# ask <提示> <默认值>：结果放进 ASK_REPLY。提示走 stderr，避免 `> 日志` 时交互看不见。
ask() {
	if [ -n "$NON_INTERACTIVE" ]; then
		ASK_REPLY="${2:-}"
		return 0
	fi
	if [ -n "${2:-}" ]; then
		printf '%s [%s]: ' "$1" "$2" >&2
	else
		printf '%s: ' "$1" >&2
	fi
	IFS= read -r ASK_REPLY || ASK_REPLY=""
	[ -n "$ASK_REPLY" ] || ASK_REPLY="${2:-}"
}

# confirm <提示>：回车算"是"（默认项是大写 Y）。
confirm() {
	if [ -n "$NON_INTERACTIVE" ]; then return 0; fi
	printf '%s [Y/n]: ' "$1" >&2
	IFS= read -r _ans || _ans=""
	case "$_ans" in
	n | N | no | NO | No) return 1 ;;
	*) return 0 ;;
	esac
}

# detect_public_ip：尽力拿到对外 IP，只用于"默认值提示"，拿不到就返回空。
# 先问外部服务（NAT 后的机器上，路由里的源地址是私网地址，没有参考价值）；
# 拿不到再退回本地路由。两处都失败不算错误——用户自己填就是了。
detect_public_ip() {
	if command -v curl >/dev/null 2>&1; then
		_ip="$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)"
		if [ -n "$_ip" ]; then printf '%s' "$_ip"; return 0; fi
	fi
	if command -v ip >/dev/null 2>&1; then
		_ip="$(ip route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -n1)"
		if [ -n "$_ip" ]; then printf '%s' "$_ip"; return 0; fi
	fi
	printf ''
}

pkg_install() {
	if command -v apt-get >/dev/null 2>&1; then
		apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "$@"
	elif command -v dnf >/dev/null 2>&1; then
		dnf install -y "$@"
	elif command -v yum >/dev/null 2>&1; then
		yum install -y "$@"
	elif command -v apk >/dev/null 2>&1; then
		apk add --no-cache "$@"
	elif command -v zypper >/dev/null 2>&1; then
		zypper -n install "$@"
	elif command -v pacman >/dev/null 2>&1; then
		pacman -Sy --noconfirm "$@"
	else
		return 1
	fi
}

# install_go：从官方源（含国内镜像）装一份 Go 到 /usr/local/go。
# 已经存在的 /usr/local/go 会先改名归档，不直接删——万一用户在里面放了别的东西，
# 删除是不可逆的，改名还能捞回来。
install_go() {
	_arch="$(go_arch)"
	if [ -z "$_arch" ]; then
		err "不支持的架构 $(uname -m)：请自行安装 Go ${MIN_GO}+ 后重跑（或加 --no-deps 后手动装）。"
		return 1
	fi
	_tmp="$(mktemp -d 2>/dev/null || echo /tmp/xbgo.$$)"
	mkdir -p "$_tmp"
	for _base in \
		https://golang.google.cn/dl \
		https://go.dev/dl \
		https://mirrors.aliyun.com/golang; do
		_url="$_base/go${GO_VERSION}.linux-${_arch}.tar.gz"
		say "下载 Go ${GO_VERSION}（$_url）"
		if download "$_url" >"$_tmp/go.tgz" 2>/dev/null && [ -s "$_tmp/go.tgz" ]; then
			if tar -C "$_tmp" -xzf "$_tmp/go.tgz" 2>/dev/null && [ -x "$_tmp/go/bin/go" ]; then
				if [ -e /usr/local/go ]; then
					mv /usr/local/go "/usr/local/go.bak.$(date -u '+%Y%m%d%H%M%S')" 2>/dev/null || true
				fi
				mv "$_tmp/go" /usr/local/go || {
					err "无法写入 /usr/local/go（权限不足？）。"
					rm -rf "$_tmp"
					return 1
				}
				rm -rf "$_tmp"
				PATH="/usr/local/go/bin:$PATH"
				export PATH
				say "已安装 Go $(/usr/local/go/bin/go version | awk '{print $3}')"
				return 0
			fi
		fi
		warn "$_url 不可用，换下一个源"
	done
	rm -rf "$_tmp"
	err "三个源都没取到 Go ${GO_VERSION}。请手动安装 Go ${MIN_GO}+，或用 --go-version 指定一个存在的版本。"
	return 1
}

# ---------------------------------------------------------------------------
# 0) 环境前提
# ---------------------------------------------------------------------------

OS_RAW="$(uname -s)"
case "$OS_RAW" in
Linux) : ;;
FreeBSD)
	warn "FreeBSD 上本脚本只做「取源码 + 编译」，服务请自行用 rc.d / 你的进程管理器托管。"
	NO_SERVICE="1"
	;;
Darwin)
	warn "macOS 不是 Master 的目标平台（采集器只覆盖 Linux/Windows/FreeBSD）。"
	warn "这里只帮你编译出二进制，服务托管请自行处理。"
	NO_SERVICE="1"
	;;
*)
	err "不支持的系统 $OS_RAW（Master 支持 Linux / Windows / FreeBSD；Windows 请用 install-master.ps1）。"
	exit 2
	;;
esac

# 卸载与"重置密码"各有自己的一小段检查（下面）。这里把它们排除在外，否则非 root
# 卸载会先撞上这段"要写 $BIN_DIR 与注册 systemd 服务"的提示——那段话描述的完全
# 不是在做的事（重置密码要的是写数据库的权限，跟 $BIN_DIR、systemd 都无关）。
# --upgrade-only **不在**排除之列：它同样要写 $BIN_DIR、同样要重启 systemd 服务，
# 而且这里挡住是对的——不然用户要先白装一套 Go 工具链、白编译几分钟才被告知没权限。
if [ "$(id -u)" -ne 0 ] && [ -z "$NO_SERVICE" ] && [ -z "$UNINSTALL" ] && [ -z "$RESET_PASSWORD" ]; then
	cat >&2 <<EOF
错误：需要 root（要写 $BIN_DIR 与注册 systemd 服务）。

请这样跑（注意 stdin 要留给交互，所以别用管道）：
    sudo sh -c "\$(curl -fsSL <本脚本的 URL>)"
或者先下载再执行：
    curl -fsSL <本脚本的 URL> -o install-master.sh && sudo sh install-master.sh

容器 / 无 systemd / 只读根文件系统的场景，可以不用 root：
    sh install-master.sh --no-service --dir ./xb-probe-master --data ./xb-probe-master/data --bin-dir ./xb-probe-master/bin
EOF
	exit 1
fi

# ---------------------------------------------------------------------------
# 0b) 先问"做什么"（同样排在依赖检查与编译之前）
# ---------------------------------------------------------------------------
#
# 与免编译入口同一个道理，这里省的更多：这个脚本的代价是**装一套 Go 工具链 +
# 克隆仓库 + 编译几分钟**。老流程是先把这些都做完、到 deploy-master.sh 里才问
# "做什么"——于是选了"卸载"或"只更新 Agent 发布文件"的人白等一次完整编译，
# 而这两件事**根本不需要源码，也不需要 Go**。
#
# 菜单只有一份（在 scripts/deploy-master.sh 的 0a-3），这里用 --ask-mode 把答案
# 取回来。取回来之后分三类：
#   · 2 / 5 → 折成 --uninstall / --reset-password，交给下面那段早退通路（卸载本来就
#     是为它准备的，重置密码同理：两者都不走源码、不走 Go）；
#   · 4 → 置 UPGRADE_ONLY。这条**要编译**（本脚本的意义就是"从源码装"），所以它不
#     走早退通路，而是跳过下面整段 TLS/地址/管理员问答，编译完只把二进制换上去。
#     它也不需要 __ASKED_MODE：下面把 --upgrade-only 显式传下去，部署脚本见到这个
#     标志就不会再问第二遍。
#   · 1 → 记下 __ASKED_MODE，最后交给部署脚本时带上 --mode 1，免得用户被问第二遍。
#
# 问不到就**什么都不做**，退回原来的行为（由 deploy-master.sh 稍后再问）：网络不通、
# --repo 不是 GitHub 地址、--ref 指到了一个还没有 --ask-mode 的旧标签——这些都不该
# 让安装直接失败，因为"多问一次"只是体验差一点，而"因此装不上"是真损失。
__ASKED_MODE=""
if [ -z "$UNINSTALL" ] && [ -z "$RELEASE_ONLY" ] &&
	[ -z "$UPGRADE_ONLY" ] && [ -z "$RESET_PASSWORD" ] &&
	[ -z "$NON_INTERACTIVE" ] && [ -z "$DRY_RUN" ] && [ -t 0 ]; then
	_base="$(raw_base "$REPO" "$REF")"
	if [ -n "$_base" ]; then
		_tmpq="$(mktemp -d 2>/dev/null || echo "/tmp/xb-askmode.$$")"
		mkdir -p "$_tmpq"
		if download "$_base/scripts/deploy-master.sh" >"$_tmpq/deploy-master.sh" 2>/dev/null &&
			[ -s "$_tmpq/deploy-master.sh" ]; then
			# 菜单与提示走它的 stderr（直接显示给用户），答案只可能是 1/2/3/4/5。
			#
			# --migrate-legacy 要一起带过去：它把答案说死了（只与"安装 / 升级"
			# 搭配），部署脚本见到它就不再问这一问。不带的话用户会在这里被问
			# 一次，选错就白跑——菜单第 4 项"只升级本体"看着最像"升级"，而它
			# 恰恰是老布局机器上最该避开的那个（见 deploy-master.sh 的 0-1 段）。
			if [ -n "$MIGRATE_LEGACY" ]; then
				_ask_out="$(sh "$_tmpq/deploy-master.sh" --ask-mode --migrate-legacy 2>/dev/null || true)"
			else
				_ask_out="$(sh "$_tmpq/deploy-master.sh" --ask-mode 2>/dev/null || true)"
			fi
			case "$_ask_out" in
			1) __ASKED_MODE="1" ;;
			2) UNINSTALL="1" ;;
			3) RELEASE_ONLY="1" ;;
			4) UPGRADE_ONLY="1" ;;
			5) RESET_PASSWORD="1" ;;
			esac
		fi
		rm -rf "$_tmpq" 2>/dev/null || true
	fi
fi

# ---------------------------------------------------------------------------
# 卸载 / 只更新 Agent 发布文件（排在依赖检查与编译之前）
# ---------------------------------------------------------------------------
# 这几个模式都不需要源码、不需要 Go，所以必须排在
# "装依赖 / clone / 编译"**之前**——否则"更新个 Agent 发布文件"要先装一套工具链，
# 是本末倒置，而且一旦工具链装不上就更新不了。
#
# 三者的取件/删除/改口令逻辑都只有一份（scripts/deploy-master.sh 的 --uninstall /
# --release-only / --reset-password），这里从仓库把它取回来执行。**刻意不自己实现
# 一遍**：两份逻辑迟早漂移，而这里赌的是用户的系统。
#
# --upgrade-only **不在**这一段里：它要编译，所以走不了早退通路（见下面第 6 步）。
#
# 三个单次模式在入口这里**也必须互斥**：下面那段早退通路是"用一串新的参数顶掉原来
# 的（set --）"，所以一旦同时给了两个，被顶掉的那个**永远不会到达部署脚本**——
# 而部署脚本的互斥判断（0-0 段）本来是能抓住这种组合的。少了这一段判断，
# `--upgrade-only --uninstall` 会安安静静地卸载，用户以为自己在升级本体。
#
# 判据同样写成"数一数"，而不是两两列举。
_im_oneshot=""
_im_count=0
if [ -n "$UNINSTALL" ]; then _im_count=$((_im_count + 1)); _im_oneshot="$_im_oneshot --uninstall"; fi
if [ -n "$RELEASE_ONLY" ]; then _im_count=$((_im_count + 1)); _im_oneshot="$_im_oneshot --release-only"; fi
if [ -n "$UPGRADE_ONLY" ]; then _im_count=$((_im_count + 1)); _im_oneshot="$_im_oneshot --upgrade-only"; fi
if [ -n "$RESET_PASSWORD" ]; then _im_count=$((_im_count + 1)); _im_oneshot="$_im_oneshot --reset-password"; fi
if [ "$_im_count" -gt 1 ]; then
	err "这几项都只做一件事，不能同时使用：$_im_oneshot"
	err "  「卸载」「只更新发布文件」「只升级本体」「重置密码」一次只能选一个。"
	exit 2
fi

if [ -n "$UNINSTALL" ] || [ -n "$RELEASE_ONLY" ] || [ -n "$RESET_PASSWORD" ]; then
	if [ -z "$DRY_RUN" ] && [ "$(id -u)" -ne 0 ]; then
		if [ -n "$UNINSTALL" ]; then
			err "卸载需要 root 或 sudo（--dry-run 不需要）。"
		elif [ -n "$RESET_PASSWORD" ]; then
			err "重置口令要写 $DATA 里的数据库，需要 root 或 sudo（--dry-run 不需要）。"
		else
			err "更新发布文件要往 data_dir/dist 里写，需要 root 或 sudo（--dry-run 不需要）。"
		fi
		exit 1
	fi
	_base="$(raw_base "$REPO" "$REF")"
	if [ -z "$_base" ]; then
		err "--repo 不是 GitHub 地址（$REPO），推导不出脚本下载地址。"
		err "请改用：sudo sh -c \"\$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/uninstall-master.sh)\""
		exit 2
	fi
	if [ -n "$UNINSTALL" ]; then
		_early_what="卸载"
		set -- --uninstall
		[ -n "$KEEP_DATA" ] && set -- "$@" --keep-data
	elif [ -n "$RESET_PASSWORD" ]; then
		_early_what="重置管理员密码"
		set -- --reset-password
		[ -n "$ADMIN_USER" ] && set -- "$@" --admin-user "$ADMIN_USER"
	else
		# 措辞要能被套进下面那两句（"取部署脚本…以执行X" / "开始X…"），
		# 所以不要再带"只"字：那会读成"开始只更新…"，像是漏了一个词。
		_early_what="更新 Agent 发布文件"
		set -- --release-only
	fi
	[ -n "$DRY_RUN" ] && set -- "$@" --dry-run
	[ -n "$DIR" ] && set -- "$@" --dir "$DIR"
	[ -n "$DATA" ] && set -- "$@" --data "$DATA"
	[ -n "$BIN_DIR" ] && set -- "$@" --bin-dir "$BIN_DIR"
	say "取部署脚本（$REF）以执行$_early_what"
	_tmpd="$(mktemp -d 2>/dev/null || echo "/tmp/xb-uninst.$$")"
	mkdir -p "$_tmpd"
	# shellcheck disable=SC2064
	# `|| true`：EXIT trap 的返回值会成为整个脚本的退出码，清理失败不该把一次
	# 成功的卸载/更新变成"失败"（详见 install-master-bin.sh 里同一处的说明）。
	trap "rm -rf '$_tmpd' 2>/dev/null || true" EXIT HUP INT TERM
	if ! download "$_base/scripts/deploy-master.sh" >"$_tmpd/deploy-master.sh" 2>/dev/null ||
		[ ! -s "$_tmpd/deploy-master.sh" ]; then
		err "取不到 $_base/scripts/deploy-master.sh（网络？--ref 写错了？）"
		exit 1
	fi
	say "开始$_early_what（接下来的输出来自 scripts/deploy-master.sh）"
	if ! sh "$_tmpd/deploy-master.sh" "$@"; then
		err "$_early_what失败（上面是它的输出）。"
		exit 1
	fi
	exit 0
fi

# 非交互式终端 + 又要提问 = 必然把所有问题答成默认值。与其装出一个"默认值拼起来的"
# 部署，不如立刻停下来说清楚怎么用。
if [ -z "$NON_INTERACTIVE" ] && [ ! -t 0 ]; then
	err "标准输入不是终端，无法交互提问。"
	cat >&2 <<'EOF'
两种做法：
  · 要交互：别用管道，改用 `sudo sh -c "$(curl -fsSL <URL>)"` 或先下载再执行
  · 不交互：加 --yes，并显式给出 --public-origin（口令设 PROBE_ADMIN_PASSWORD）
EOF
	exit 2
fi

# ---------------------------------------------------------------------------
# 1) 交互问答（放在装依赖之前：答案是错的就没必要动系统）
# ---------------------------------------------------------------------------
#
# --upgrade-only 把这一整段（TLS 方式、对外地址、管理员账号）连同下面的"2) 确认"
# 一起跳过：它只换二进制，站点、证书、端口、管理员**一个都不动**，所以问这些
# 既是白问、又容易让人以为"改了什么"。用户原话：
#   「他会全部走一遍流程，包括站点设置、证书申请、管理员账号设置等全部流程，
#     但是这些我不想做，我仅仅只想升级 master 本体」。
#
# 这段体量太大（两百来行），包进 if 之后**刻意不整体缩进一格**：那会让本次改动
# 看起来动了全文每一行，日后 diff 再也读不出"这次到底改了哪几处"。首尾各留一条
# 显眼的分隔注释，靠它定位块边界。
# ==== 站点/证书/管理员问答 开始（upgrade-only 时整段跳过）====
if [ -z "$UPGRADE_ONLY" ]; then

PUBIP="$(detect_public_ip)"

if [ -z "$NON_INTERACTIVE" ]; then
	echo >&2
	echo "下面几个问题决定对外访问方式，直接回车取方括号里的默认值。" >&2
	echo >&2
	cat >&2 <<'EOF'
对外访问方式：
  1) caddy      使用 HTTPS。证书自动申请与续期，反代也一并配好——本脚本会装好
                Caddy 并把请求转给 Master，你只需要把域名解析到本机。
                **最省事，推荐**
  2) master     使用 HTTPS。证书由 Master 进程自己申请（内置 ACME 客户端），
                Master 自己监听 443 并终结 TLS（需要一个已解析到本机的域名）
  3) selfsigned 使用 HTTPS，但用自签证书（用纯 IP 访问时，这是唯一可行的加密
                方式；浏览器会提示证书不受信任）。Master 自己终结
  4) files      使用 HTTPS，证书与私钥用你已有的文件。Master 自己终结

选 1 不需要你自己装 Caddy 或 Nginx 并手写配置；选 2~4 由 Master 自己扛下 443。
EOF
fi

if [ -z "$TLS_MODE" ]; then
	ask "选择对外访问方式（1/2/3/4）" "1"
	case "$ASK_REPLY" in
	1 | none | NONE | None | caddy | CADDY | Caddy) TLS_MODE="none" ;;
	2 | acme | ACME | Acme | master | MASTER | Master) TLS_MODE="acme" ;;
	3 | selfsigned | SelfSigned | SELF_SIGNED) TLS_MODE="selfsigned" ;;
	4 | files | FILES | Files) TLS_MODE="files" ;;
	*) TLS_MODE="$ASK_REPLY" ;;
	esac
fi
case "$TLS_MODE" in
none | acme | selfsigned | files) : ;;
*)
	err "--tls-mode 只能是 none / acme / selfsigned / files（收到 $TLS_MODE）。"
	exit 2
	;;
esac

if [ "$TLS_MODE" = "acme" ]; then
	if [ -z "$TLS_DOMAINS$TLS_IPS" ]; then
		ask "域名（逗号分隔可多个；必须已解析到本机）" ""
		TLS_DOMAINS="$ASK_REPLY"
	fi
	if [ -z "$TLS_DOMAINS$TLS_IPS" ]; then
		err "ACME 至少要有一个域名或 IP（否则没有可申请的标识符）。"
		exit 2
	fi
	if [ -z "$ACME_EMAIL" ]; then
		ask "邮箱（可选，CA 用它发到期提醒）" ""
		ACME_EMAIL="$ASK_REPLY"
	fi
fi

if [ "$TLS_MODE" = "files" ]; then
	if [ -z "$TLS_CERT" ]; then
		ask "证书文件路径" ""
		TLS_CERT="$ASK_REPLY"
	fi
	if [ -z "$TLS_KEY" ]; then
		ask "私钥文件路径" ""
		TLS_KEY="$ASK_REPLY"
	fi
	for _f in "$TLS_CERT" "$TLS_KEY"; do
		if [ ! -f "$_f" ]; then
			err "找不到文件 $_f。"
			exit 2
		fi
	done
fi

# ---------------------------------------------------------------------------
# 1b) 谁终结 TLS：由上面那一问**推导**，不再单独提问
# ---------------------------------------------------------------------------
#
# 这两件事本来是同一件事的两种说法：选了 1) caddy 就是"证书交给反代去办"，
# 选了 2~4 就是"Master 自己拿着一份证书监听 443"。拆成两问只会让用户按出一个
# 自相矛盾的组合（真机上确实出现过"终结者=master 却仍然监听 8080"）。
# 用户的原话：**"选择 1 直接是 caddy 反代，选择 2 直接是 Master 反代"**。
#
# `--tls-terminator` 仍然可以显式给出，那时以它为准；只有这一处给默认值
# （scripts/deploy-master.sh 里是同一个规则的镜像，两处必须一致）。
if [ -z "$TLS_TERMINATOR" ]; then
	case "$TLS_MODE" in
	none) TLS_TERMINATOR="caddy" ;;
	plain) TLS_TERMINATOR="" ;;
	*) TLS_TERMINATOR="master" ;;
	esac
fi

# caddy 终结时，TLS 由 Caddy 负责，Master 侧必须**不终结**（否则两个进程抢 443，
# 而 cmaster 报出的 "address already in use" 会被误读成"端口被别的程序占了"）。
# 这里就地纠正而不是只给个警告：用户选了"不要证书"之外的选项时就落进这个组合，
# 让他自己去想"那我该把 --tls-mode 改成什么"是没道理的。
if [ "$TLS_TERMINATOR" = "caddy" ]; then
	if [ "$TLS_MODE" != "none" ]; then
		printf '注意：选了 caddy 终结 TLS，则 Master 侧不终结（--tls-mode none）。' >&2
		printf '你选的 --tls-mode %s 的证书相关参数将被忽略。\n' "$TLS_MODE" >&2
		TLS_MODE="none"
		TLS_CERT=""
		TLS_KEY=""
		TLS_DOMAINS=""
		TLS_IPS=""
	fi
	# Caddy 需要一个域名（HTTP-01/TLS-ALPN 都要能解析到本机）。这里问的是
	# **反代哪些域名**——同站部署时公开站与后台在同一个域名下，只有一个。
	if [ -z "$PROXY_DOMAINS" ] && [ -z "$PUBLIC_ORIGIN" ] && [ -z "$NON_INTERACTIVE" ]; then
		ask "要反代的域名（逗号分隔可多个；必须已解析到本机）" ""
		PROXY_DOMAINS="$ASK_REPLY"
	fi
fi

if [ "$TLS_MODE" = "selfsigned" ] && [ -z "$TLS_DOMAINS$TLS_IPS" ]; then
	ask "证书要覆盖的域名/IP（逗号分隔；回车=localhost/127.0.0.1/::1）" ""
	if [ -n "$ASK_REPLY" ]; then
		case "$ASK_REPLY" in
		*:*) TLS_IPS="$ASK_REPLY" ;;
		*[a-zA-Z]*) TLS_DOMAINS="$ASK_REPLY" ;;
		*) TLS_IPS="$ASK_REPLY" ;;
		esac
	fi
fi

if [ -z "$PUBLIC_ORIGIN" ]; then
	# 非交互时**绝不**用 127.0.0.1 兜底：它会被写进 public_origin，随后成为
	# install.master_url，于是所有 Agent 都去连自己的回环地址——装得成、永远不上报。
	# 宁可为空（留空时 Master 按请求 Host 回填，走域名访问是正常的）。
	if [ -n "$PUBIP" ]; then _host="$PUBIP"; else _host="127.0.0.1"; fi
	case "$TLS_MODE" in
	acme)
		_dom="$(printf '%s' "$TLS_DOMAINS" | cut -d, -f1)"
		_ip0="$(printf '%s' "$TLS_IPS" | cut -d, -f1)"
		if [ -n "$_dom" ]; then
			_def="https://$_dom"
		elif [ -n "$_ip0" ]; then
			_def="https://$_ip0"
		elif [ -n "$NON_INTERACTIVE" ]; then
			_def=""
		else
			_def="https://$_host"
		fi
		;;
	selfsigned | files)
		if [ -n "$NON_INTERACTIVE" ] && [ -z "$PUBIP" ]; then _def=""; else _def="https://$_host:8080"; fi
		;;
	*)
		if [ -n "$NON_INTERACTIVE" ] && [ -z "$PUBIP" ]; then _def=""; else _def="http://$_host:8080"; fi
		;;
	esac
	ask "公开访问地址（对外 URL，不带末尾斜杠）" "$_def"
	PUBLIC_ORIGIN="$ASK_REPLY"
fi

# 协议与 TLS 必须自洽。这里查一遍是为了"答完立刻纠错"，
# deploy-master.sh 里还会再查一遍——那份才是真正落盘前把关的权威实现。
case "$TLS_MODE:$PUBLIC_ORIGIN" in
files:http://* | selfsigned:http://* | acme:http://*)
	err "TLS 方式 $TLS_MODE 与公开地址 $PUBLIC_ORIGIN 不一致（配了证书就该宣告 https://）。"
	exit 2
	;;
none:https://*)
	warn "TLS 方式 none 却给 https:// 的公开地址：TLS 若由前置反代终结，"
	warn "请把反代网段填进 master.yaml 的 trusted_proxies，否则 Cookie/HSTS 会按明文处理。"
	;;
esac

if [ -z "$ADMIN_ORIGIN" ]; then
	ask "管理端地址（回车=与公开站同一个域名，即「同站」）" ""
	ADMIN_ORIGIN="$ASK_REPLY"
fi

if [ -z "$ADMIN_USER" ]; then
	ask "管理员用户名" "ops"
	ADMIN_USER="$ASK_REPLY"
fi
[ -n "$ADMIN_USER" ] || ADMIN_USER="ops"

# ---------------------------------------------------------------------------
# 2) 确认
# ---------------------------------------------------------------------------

if [ -z "$NON_INTERACTIVE" ]; then
	echo >&2
	echo "即将执行：" >&2
	printf '  %-10s %s\n' "源码" "$SRC（git clone $REPO，分支 $REF）" >&2
	printf '  %-10s %s\n' "公开地址" "$PUBLIC_ORIGIN" >&2
	printf '  %-10s %s\n' "管理地址" "${ADMIN_ORIGIN:-与公开地址相同}" >&2
	# 监听端口不在这里问：默认值只有部署脚本那一份来源，问题与默认值必须同源，
	# 所以端口由部署脚本在落盘前问（这里只说明"还会问一次"）。
	printf '  %-10s %s\n' "监听端口" "随后确认（默认 8080 / 8081）" >&2
	printf '  %-10s %s\n' "管理员" "$ADMIN_USER（口令随后交互输入）" >&2
	printf '  %-10s %s\n' "TLS" "$TLS_MODE" >&2
	if [ -n "$NO_SERVICE" ]; then
		printf '  %-10s %s\n' "服务" "不注册（--no-service）" >&2
	else
		printf '  %-10s %s\n' "服务" "systemd（xb-probe-master，开机自启）" >&2
	fi
	echo >&2
	if ! confirm "继续？"; then
		echo "已取消，未改动系统。" >&2
		exit 0
	fi
fi
fi
# ==== 站点/证书/管理员问答 结束（upgrade-only 时整段跳过）====

# upgrade-only 不问上面那些，但下面第 6 步要把 ADMIN_USER 透传下去（--admin-user），
# 而它在那条路上一直是空的。这里补上同一个默认值——默认值只能有一份来源，
# 所以取自部署脚本一致的那一个（ops），不另立一个。
[ -n "$ADMIN_USER" ] || ADMIN_USER="ops"

# ---------------------------------------------------------------------------
# 3) 依赖
# ---------------------------------------------------------------------------

if [ -z "$DRY_RUN" ]; then
	# git 是必需的（取源码）；tar 只在需要装 Go 时用得上。
	for _cmd in git; do
		if ! command -v "$_cmd" >/dev/null 2>&1; then
			if [ -n "$NO_DEPS" ]; then
				err "缺少 $_cmd，且指定了 --no-deps。请先安装：apt install git / yum install git / apk add git"
				exit 1
			fi
			say "缺少 $_cmd，尝试自动安装"
			pkg_install git || {
				err "自动安装 $_cmd 失败，请手动安装后重跑。"
				exit 1
			}
		fi
	done
fi

GO_BIN=""
if command -v go >/dev/null 2>&1; then
	GO_VER="$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')"
	if ver_ge "$MIN_GO" "$GO_VER"; then
		GO_BIN="$(command -v go)"
		say "使用系统 Go $GO_VER"
	else
		warn "系统 Go 是 $GO_VER，低于项目要求的 $MIN_GO"
	fi
fi

if [ -z "$GO_BIN" ]; then
	if [ -n "$DRY_RUN" ]; then
		say "（dry-run）将安装 Go ${GO_VERSION} 到 /usr/local/go"
	else
		install_go || exit 1
		GO_BIN="/usr/local/go/bin/go"
	fi
fi

if [ -n "$GO_BIN" ]; then
	_cur_proxy="$("$GO_BIN" env GOPROXY 2>/dev/null || echo '')"
	if [ -z "$GOPROXY_OVERRIDE" ] && [ "$_cur_proxy" = "https://proxy.golang.org,direct" ]; then
		# 只覆盖"官方默认值"这一种情况：用户自己配过代理就绝不动它。
		GOPROXY_OVERRIDE="https://goproxy.cn,direct"
		say "GOPROXY 是官方默认值，本次改用国内镜像（想固定下来用 go env -w GOPROXY=…）"
	fi
	if [ -n "$GOPROXY_OVERRIDE" ]; then
		export GOPROXY="$GOPROXY_OVERRIDE"
	fi
fi

# ---------------------------------------------------------------------------
# 4) 取源码 / 5) 编译
# ---------------------------------------------------------------------------

if [ "$DRY_RUN" = "1" ]; then
	say "（dry-run）取源码：$REPO（$REF）→ $SRC"
	say "（dry-run）编译：cd $SRC && CGO_ENABLED=0 go build -trimpath -o bin/xb-probe-master ./cmd/xb-probe-master"
	if [ -n "$UPGRADE_ONLY" ]; then
		say "（dry-run）部署：sh $SRC/scripts/deploy-master.sh --bin $SRC/bin/xb-probe-master --upgrade-only"
		say "（dry-run）只换 Master 二进制并重启服务；配置、证书、管理员账号、data_dir/dist 都不动。"
	else
		say "（dry-run）部署：sh $SRC/scripts/deploy-master.sh --bin $SRC/bin/xb-probe-master …"
	fi
	say "（dry-run）以上为计划，未改动系统。"
	exit 0
fi

if [ -d "$SRC/.git" ]; then
	say "更新已有源码 $SRC（丢弃该目录内的本地改动）"
	git -C "$SRC" fetch --depth 1 origin "$REF" || {
		err "git fetch 失败：检查网络，或换 --repo/--ref。"
		exit 1
	}
	git -C "$SRC" reset --hard --quiet FETCH_HEAD
	# 清掉上一版遗留的未跟踪文件，否则旧文件可能让构建结果与源码不一致。
	# 不动 bin/（上一版的二进制留着，编译失败时还能回滚）。
	git -C "$SRC" clean -fdq -e bin || true
else
	if [ -e "$SRC" ] && [ -n "$(ls -A "$SRC" 2>/dev/null)" ]; then
		err "$SRC 已存在、非空、且不是 git 仓库。请换 --src 指到别的目录，或先清空它。"
		exit 2
	fi
	say "克隆源码到 $SRC"
	mkdir -p "$(dirname "$SRC")"
	git clone --depth 1 --branch "$REF" "$REPO" "$SRC" || {
		err "git clone 失败：检查网络与仓库地址（--repo），或镜像仓库用 --repo 指定。"
		exit 1
	}
fi

if [ ! -f "$SRC/scripts/deploy-master.sh" ]; then
	err "$SRC 里没有 scripts/deploy-master.sh——源码不完整，或 --ref 指到了一个早期提交。"
	exit 1
fi

VER="$(git -C "$SRC" describe --tags --always 2>/dev/null || echo dev)"
[ -n "$VER" ] || VER="dev"
say "编译 xb-probe-master（版本 $VER）"
if ! (
	cd "$SRC" &&
		CGO_ENABLED=0 "$GO_BIN" build -trimpath -ldflags "-s -w -X main.version=$VER" \
			-o bin/xb-probe-master ./cmd/xb-probe-master
); then
	err "编译失败。若是拉依赖超时/被墙，试：--goproxy https://goproxy.cn,direct"
	err "若是 Go 版本问题，试：--go-version 1.27.2（或先卸载旧 Go）"
	exit 1
fi
say "已生成 $SRC/bin/xb-probe-master"

# ---------------------------------------------------------------------------
# 6) 交给 deploy-master.sh 完成落盘、服务与健康检查
# ---------------------------------------------------------------------------

# 0b 已经问过"做什么"且答案是 1，把答案带下去，免得用户被问第二遍。
# **只在真的问出来过的时候才传**：--mode 是新加的，老版本的部署脚本对未知参数
# 是硬失败（exit 2）。而"上面那次 --ask-mode 成功了"正好等价于"这个脚本认识
# --mode"（同一个文件），所以这个条件本身就把兼容性判断做完了。
#
# --upgrade-only 走的是**另一套参数**：上面那些站点/证书/账号/地址全都没问过，
# 变量也都是空的，这时候**绝不能**把它们拼进去。最典型的是 `--tls-mode "$TLS_MODE"`
# 那一行——它是无条件拼的，空值会被部署脚本判成"非法取值"直接 exit 2。所以这里
# 分成两支，upgrade-only 那一支只带"从哪来、往哪去"。
if [ -n "$UPGRADE_ONLY" ]; then
	set -- --bin "$SRC/bin/xb-probe-master" --upgrade-only
	# 路径类参数照旧转发：它们决定"升级的是哪一份安装"，与本模式直接相关。
	[ -n "$DIR" ] && set -- "$@" --dir "$DIR"
	[ -n "$DATA" ] && set -- "$@" --data "$DATA"
	[ -n "$BIN_DIR" ] && set -- "$@" --bin-dir "$BIN_DIR"
	[ -n "$MIGRATE_LEGACY" ] && set -- "$@" --migrate-legacy
else
set -- --bin "$SRC/bin/xb-probe-master"
[ -n "$__ASKED_MODE" ] && set -- "$@" --mode "$__ASKED_MODE"
[ -n "$MIGRATE_LEGACY" ] && set -- "$@" --migrate-legacy
[ -n "$PUBLIC_ORIGIN" ] && set -- "$@" --public-origin "$PUBLIC_ORIGIN"
[ -n "$ADMIN_ORIGIN" ] && set -- "$@" --admin-origin "$ADMIN_ORIGIN"
[ -n "$ADMIN_USER" ] && set -- "$@" --admin-user "$ADMIN_USER"
[ -n "$PUBLIC_LISTEN" ] && set -- "$@" --public-listen "$PUBLIC_LISTEN"
[ -n "$ADMIN_LISTEN" ] && set -- "$@" --admin-listen "$ADMIN_LISTEN"
[ -n "$DIR" ] && set -- "$@" --dir "$DIR"
[ -n "$DATA" ] && set -- "$@" --data "$DATA"
[ -n "$BIN_DIR" ] && set -- "$@" --bin-dir "$BIN_DIR"
[ -n "$DIST_BASE" ] && set -- "$@" --dist-base "$DIST_BASE"
# 发布文件拉取范围/来源也必须转发：用户在安装脚本上给的 --dist-pull skip，
# 如果没传下去，部署脚本会照默认值去拉几十 MB，而用户明确说了不要。
[ -n "$DIST_PULL" ] && set -- "$@" --dist-pull "$DIST_PULL"
[ -n "$DIST_SOURCE" ] && set -- "$@" --dist-source "$DIST_SOURCE"
[ -n "$NO_DIST_PULL" ] && set -- "$@" --no-dist-pull
[ -n "$NO_SERVICE" ] && set -- "$@" --no-service
[ -n "$FORCE_CONFIG" ] && set -- "$@" --force-config
set -- "$@" --tls-mode "$TLS_MODE"
[ -n "$TLS_CERT" ] && set -- "$@" --tls-cert "$TLS_CERT"
[ -n "$TLS_KEY" ] && set -- "$@" --tls-key "$TLS_KEY"
[ -n "$TLS_DOMAINS" ] && set -- "$@" --tls-domain "$TLS_DOMAINS"
[ -n "$TLS_IPS" ] && set -- "$@" --tls-ip "$TLS_IPS"
[ -n "$ACME_EMAIL" ] && set -- "$@" --acme-email "$ACME_EMAIL"
[ -n "$ACME_CA" ] && set -- "$@" --acme-ca "$ACME_CA"
[ -n "$ACME_CHALLENGE" ] && set -- "$@" --acme-challenge "$ACME_CHALLENGE"
# 谁终结 TLS（master 自己 / 由部署脚本装的 Caddy）。这两个参数必须**转发出去**：
# 参数集护栏（internal/dist/deploy_flags_test.go）会逐项比对"部署脚本认识的每个
# 参数都要被安装脚本转发"，少一个就红——而它红了正是对的，因为不转发等于
# 用户在安装脚本里做的选择会被静默丢弃。
[ -n "$TLS_TERMINATOR" ] && set -- "$@" --tls-terminator "$TLS_TERMINATOR"
[ -n "$PROXY_DOMAINS" ] && set -- "$@" --proxy-domain "$PROXY_DOMAINS"
fi

say "开始部署（接下来的输出来自 scripts/deploy-master.sh）"
# 与 install-master-bin.sh 同一个机制：告诉部署脚本"用户是怎么把这一串叫起来的"，
# 收尾的卸载提示才还原得出可粘贴执行的命令。
PROBE_SELF_ENTRY=src
PROBE_SCRIPT_ENTRY_URL="https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master.sh"
export PROBE_SELF_ENTRY PROBE_SCRIPT_ENTRY_URL
sh "$SRC/scripts/deploy-master.sh" "$@"

cat <<EOF

==> 完成。
  · 管理端：${ADMIN_ORIGIN:-$PUBLIC_ORIGIN}/admin/
  · 源码留在 $SRC：要升级就再跑一次本脚本（会自动 fetch + 重编译 + 换二进制），
    或者只重跑部署：sudo sh $SRC/scripts/deploy-master.sh --bin $SRC/bin/xb-probe-master
EOF
