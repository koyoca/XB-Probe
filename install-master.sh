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
#   -h, --help          显示本帮助
#
# 其余参数原样透传给 scripts/deploy-master.sh（--public-origin、--admin-origin、
# --admin-user、--public-listen、--admin-listen、--dir、--data、--bin-dir、
# --dist-base、--no-service、--force-config，以及全部 --tls-* 与 --acme-* 参数）。

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
TLS_MODE=""
TLS_CERT=""
TLS_KEY=""
TLS_DOMAINS=""
TLS_IPS=""
ACME_EMAIL=""
ACME_CA=""
ACME_CHALLENGE=""
NO_SERVICE=""
FORCE_CONFIG=""

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
  -h, --help           显示本帮助

  其余参数透传给 scripts/deploy-master.sh：
  --public-origin/--admin-origin/--admin-user/--public-listen/--admin-listen/
  --dir/--data/--bin-dir/--dist-base/--no-service/--force-config/
  --tls-mode/--tls-cert/--tls-key/--tls-domain/--tls-ip/--acme-email/--acme-ca/--acme-challenge
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
	--yes | -y) NON_INTERACTIVE="1"; shift ;;
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
	--tls-mode) TLS_MODE="${2:-}"; shift 2 ;;
	--tls-cert) TLS_CERT="${2:-}"; shift 2 ;;
	--tls-key) TLS_KEY="${2:-}"; shift 2 ;;
	--tls-domain) TLS_DOMAINS="${2:-}"; shift 2 ;;
	--tls-ip) TLS_IPS="${2:-}"; shift 2 ;;
	--acme-email) ACME_EMAIL="${2:-}"; shift 2 ;;
	--acme-ca) ACME_CA="${2:-}"; shift 2 ;;
	--acme-challenge) ACME_CHALLENGE="${2:-}"; shift 2 ;;
	--no-service) NO_SERVICE="1"; shift ;;
	--force-config) FORCE_CONFIG="1"; shift ;;
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

if [ "$(id -u)" -ne 0 ] && [ -z "$NO_SERVICE" ]; then
	cat >&2 <<EOF
错误：需要 root（要写 $BIN_DIR 与注册 systemd 服务）。

请这样跑（注意 stdin 要留给交互，所以别用管道）：
    sudo sh -c "\$(curl -fsSL <本脚本的 URL>)"
或者先下载再执行：
    curl -fsSL <本脚本的 URL> -o install-master.sh && sudo sh install-master.sh

容器 / 无 systemd / 只读根文件系统的场景，可以不用 root：
    sh install-master.sh --no-service --dir ./probe-master --data ./probe-master/data --bin-dir ./probe-master/bin
EOF
	exit 1
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

PUBIP="$(detect_public_ip)"

if [ -z "$NON_INTERACTIVE" ]; then
	echo >&2
	echo "下面几个问题决定对外访问方式，直接回车取方括号里的默认值。" >&2
	echo >&2
	cat >&2 <<'EOF'
TLS 方式：
  1) none        由前置 Caddy/Nginx 终结 TLS（默认；也适合本机/内网调试）
  2) acme        内置 ACME 客户端自动申请证书（需要一个已解析到本机的域名）
  3) selfsigned  自签证书（用纯 IP 访问时，这是唯一可行的加密方式）
  4) files       使用你已有的证书与私钥
EOF
fi

if [ -z "$TLS_MODE" ]; then
	ask "选择 TLS 方式（1/2/3/4）" "1"
	case "$ASK_REPLY" in
	1 | none | NONE | None) TLS_MODE="none" ;;
	2 | acme | ACME | Acme) TLS_MODE="acme" ;;
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
	printf '  %-10s %s\n' "管理员" "$ADMIN_USER（口令随后交互输入）" >&2
	printf '  %-10s %s\n' "TLS" "$TLS_MODE" >&2
	if [ -n "$NO_SERVICE" ]; then
		printf '  %-10s %s\n' "服务" "不注册（--no-service）" >&2
	else
		printf '  %-10s %s\n' "服务" "systemd（probe-master，开机自启）" >&2
	fi
	echo >&2
	if ! confirm "继续？"; then
		echo "已取消，未改动系统。" >&2
		exit 0
	fi
fi

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
	say "（dry-run）编译：cd $SRC && CGO_ENABLED=0 go build -trimpath -o bin/probe-master ./cmd/probe-master"
	say "（dry-run）部署：sh $SRC/scripts/deploy-master.sh --bin $SRC/bin/probe-master …"
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
say "编译 probe-master（版本 $VER）"
if ! (
	cd "$SRC" &&
		CGO_ENABLED=0 "$GO_BIN" build -trimpath -ldflags "-s -w -X main.version=$VER" \
			-o bin/probe-master ./cmd/probe-master
); then
	err "编译失败。若是拉依赖超时/被墙，试：--goproxy https://goproxy.cn,direct"
	err "若是 Go 版本问题，试：--go-version 1.27.2（或先卸载旧 Go）"
	exit 1
fi
say "已生成 $SRC/bin/probe-master"

# ---------------------------------------------------------------------------
# 6) 交给 deploy-master.sh 完成落盘、服务与健康检查
# ---------------------------------------------------------------------------

set -- --bin "$SRC/bin/probe-master"
[ -n "$PUBLIC_ORIGIN" ] && set -- "$@" --public-origin "$PUBLIC_ORIGIN"
[ -n "$ADMIN_ORIGIN" ] && set -- "$@" --admin-origin "$ADMIN_ORIGIN"
[ -n "$ADMIN_USER" ] && set -- "$@" --admin-user "$ADMIN_USER"
[ -n "$PUBLIC_LISTEN" ] && set -- "$@" --public-listen "$PUBLIC_LISTEN"
[ -n "$ADMIN_LISTEN" ] && set -- "$@" --admin-listen "$ADMIN_LISTEN"
[ -n "$DIR" ] && set -- "$@" --dir "$DIR"
[ -n "$DATA" ] && set -- "$@" --data "$DATA"
[ -n "$BIN_DIR" ] && set -- "$@" --bin-dir "$BIN_DIR"
[ -n "$DIST_BASE" ] && set -- "$@" --dist-base "$DIST_BASE"
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

say "开始部署（接下来的输出来自 scripts/deploy-master.sh）"
sh "$SRC/scripts/deploy-master.sh" "$@"

cat <<EOF

==> 完成。
  · 管理端：${ADMIN_ORIGIN:-$PUBLIC_ORIGIN}/admin/
  · 源码留在 $SRC：要升级就再跑一次本脚本（会自动 fetch + 重编译 + 换二进制），
    或者只重跑部署：sudo sh $SRC/scripts/deploy-master.sh --bin $SRC/bin/probe-master
EOF
