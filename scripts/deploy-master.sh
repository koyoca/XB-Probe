#!/bin/sh
# Server Probe —— Master 一键部署（Linux / systemd）
#
# 覆盖三件事：放二进制 → 写配置并初始化管理员 → 注册 systemd 服务并做健康检查。
# 幂等：重复执行会走"升级"路径（停服务 → 换二进制 → 启动），不会重建数据目录、
# 也不会覆盖已有的 master.yaml（除非 --force-config）。
#
# 用法（root）：
#   sh deploy-master.sh --bin ./bin/probe-master \
#       --public-origin https://status.example.com \
#       --admin-origin  https://admin.example.com \
#       --admin-user ops --admin-password '<强口令>'
#
# 不传 --admin-password 时会交互式提示输入（推荐：口令不进 shell 历史）。
# 也可以先 export PROBE_ADMIN_PASSWORD=... 再执行。
#
# 交互问答：**参数没给全时会问你**——TLS 方式（要不要 https）、公开站/管理端的
# 访问地址、两边各监听哪个端口、管理员用户名与口令，最后把将要做的事列一遍让你确认。
# 之所以问在这一层（而不是各自的 install-*.sh 里）：
#   · 默认值（监听地址、TLS 模式、目录）只有本文件这一份来源，问题里方括号中的
#     默认值必须与真正生效的值同源，否则迟早出现"提示写 8080、实际听 8081"；
#   · 直接跑本脚本的人（安装脚本第 7 步就建议这么重跑）也该被问到，而不是必须
#     回过头去手改 master.yaml。
# 三种情况不问（否则会把答案全取成默认值，装出一个"默认值拼起来的"部署）：
#   ① PROBE_NO_ASK=1（安装脚本的 --yes 会设它）
#   ② 标准输入不是终端（CI、`curl … | sh`、输出重定向到日志）
#   ③ 该值已经由命令行或 PROBE_* 环境变量显式给出
# 想在任何环境下都走一遍问答（自测/演示用）：PROBE_ASK_FORCE=1。
#
# 只看不装（打印将要写入的 master.yaml，不建用户/目录、不注册服务、不建管理员）：
#   PROBE_NO_ASK=1 sh deploy-master.sh --bin ./bin/probe-master --dry-run
#
# 常用可选参数：
#   --dir <目录>          配置目录（默认 /etc/probe-master）
#   --data <目录>         数据目录（默认 /var/lib/probe-master）
#   --bin-dir <目录>      二进制安装目录（默认 /usr/local/bin；只读根文件系统用 /opt/probe-master）
#   --public-listen <地址> 公开站监听（默认 0.0.0.0:8080；只写端口如 9000 等价于 0.0.0.0:9000）
#   --admin-listen  <地址> 管理端监听（默认 127.0.0.1:8081 —— 只监听回环，
#                          公网访问请走反代，或显式写 0.0.0.0:8081 自行承担风险）
#   --dist-base <URL>     发布文件镜像前缀（不放行则由 Master 自己提供 /dist/）
#   --no-service          只安装不注册服务（容器里用）
#   --force-config        覆盖已有配置文件
#   --dry-run             只打印将要做的事与将要写入的配置，不改动系统
#   --yes                 不提问（等价于 PROBE_NO_ASK=1）
#
# TLS（默认 none；四种方式详见 configs/master.yaml.example 的 tls 段）：
#   --tls-mode <none|files|selfsigned|acme>
#   --tls-cert <证书> --tls-key <私钥>      mode=files：自定义证书（热加载）
#   --tls-domain <域名[,域名]>              mode=acme/selfsigned：域名（SAN）
#   --tls-ip <IP[,IP]>                      mode=acme/selfsigned：IP（RFC 8738 / SAN）
#   --acme-email <邮箱> --acme-ca <CA>       mode=acme：账户邮箱与 CA（默认 letsencrypt）
#   --acme-challenge <http-01|tls-alpn-01|dns-01>
#                                            mode=acme：默认 http-01（需要 80 端口可达）
#   dns-01 的凭据不走命令行（会进 shell 历史）：
#     PROBE_ACME_DNS_PROVIDER（默认 cloudflare）
#     PROBE_ACME_DNS_TOKEN（Cloudflare API Token，需 Zone:DNS:Edit + Zone:Zone:Read）
#   交互执行时这两项会被提示输入，Token 不回显。
#
# 交互输入的退格/方向键：本脚本会尽量把自己换到 bash 下运行（`read -e` 走 readline）。
# 换不了（没有 bash，或脚本不是以文件形式存在）时退回内核 tty 的行编辑，那种情况下
# 若终端发的退格码与 tty 的 erase 字符不一致，退格会被当成字面字符打出来。
#
# 卸载（默认连数据一起清；数据目录里有数据库与 master.key）：
#   sh deploy-master.sh --uninstall            # 停服务 → 删单元/二进制/配置/数据/系统用户
#   sh deploy-master.sh --uninstall --keep-data
#
# 配了 --tls-mode 却与 --public-origin 的协议不一致（例如 mode=acme 却给 http://）
# 会被拒绝：证书装上了，访客却仍在明文上，是最难被察觉的一种"部署成功"。

set -eu

# ---------------------------------------------------------------------------
# 交互输入的可编辑性：尽量在 bash 下运行本脚本
# ---------------------------------------------------------------------------
#
# 现象：在 Ubuntu 上 `sudo sh deploy-master.sh`，输入时按退格**不删字符**，
# 而是插入一个 "^H"，方向键同样没用，只能 Ctrl+C 重来。
#
# 原因不在本脚本，而在"谁负责行编辑"：
#   · dash（Ubuntu 的 /bin/sh）把行编辑完全交给内核 tty 的 erase 字符。很多
#     终端/SSH 客户端发的退格是 ^H(0x08)，而 tty 那侧配的常是 ^?(0x7f)，
#     于是内核不认为它是"删除"，只当成一个可打印字符插进缓冲区；
#   · bash 的 `read -e` 自己跑 readline，把 ^H 与 ^? **都**绑成 backward-delete-char，
#     无论终端怎么配都能正常删除，方向键/Home/End 也随之可用。
#
# 所以能换 bash 就换。两个前置条件缺一不可：
#   · $0 是一个真实文件——`sh -c "$(curl …)"` 那种用法没有文件，只能保持原样；
#   · bash 可用。
# PROBE_REEXEC 防的是"万一 bash 起来后 BASH_VERSION 仍为空"时的无限自我调用。
if [ -z "${PROBE_REEXEC:-}" ] && [ -z "${BASH_VERSION:-}" ] && command -v bash >/dev/null 2>&1; then
	if [ -f "$0" ]; then
		PROBE_REEXEC=1
		export PROBE_REEXEC
		exec bash "$0" "$@"
	fi
fi

BIN_SRC=""
DIR="${PROBE_DIR:-/etc/probe-master}"
DATA="${PROBE_DATA:-/var/lib/probe-master}"
BIN_DIR="${PROBE_BIN_DIR:-/usr/local/bin}"
PUBLIC_LISTEN="${PROBE_PUBLIC_LISTEN:-0.0.0.0:8080}"
ADMIN_LISTEN="${PROBE_ADMIN_LISTEN:-127.0.0.1:8081}"
PUBLIC_ORIGIN="${PROBE_PUBLIC_ORIGIN:-}"
ADMIN_ORIGIN="${PROBE_ADMIN_ORIGIN:-}"
DIST_BASE="${PROBE_DIST_BASE:-}"
ADMIN_USER="${PROBE_ADMIN_USER:-ops}"
ADMIN_PASSWORD="${PROBE_ADMIN_PASSWORD:-}"
NO_SERVICE=""
FORCE_CONFIG=""
TLS_MODE="${PROBE_TLS_MODE:-none}"
TLS_CERT="${PROBE_TLS_CERT:-}"
TLS_KEY="${PROBE_TLS_KEY:-}"
TLS_DOMAINS="${PROBE_TLS_DOMAINS:-}"
TLS_IPS="${PROBE_TLS_IPS:-}"
ACME_EMAIL="${PROBE_ACME_EMAIL:-}"
ACME_CA="${PROBE_ACME_CA:-letsencrypt}"
ACME_CHALLENGE="${PROBE_ACME_CHALLENGE:-http-01}"
# dns-01 专用的两项。它们**刻意只走环境变量与交互输入，不做命令行参数**：
# 一个能改你 DNS 的 API Token 进了 shell 历史，等于把它写在 .bash_history 里。
# （与 --admin-password 同一个理由，见文件头的说明。）
ACME_DNS_PROVIDER="${PROBE_ACME_DNS_PROVIDER:-}"
ACME_DNS_TOKEN="${PROBE_ACME_DNS_TOKEN:-}"
DRY_RUN=""
NO_ASK=""
UNINSTALL=""
KEEP_DATA=""

# "用户是否显式给过"这几个标记，用来区分"没给"和"给了一个恰好等于默认值的值"。
# 缺了它们，像 TLS_MODE / PUBLIC_LISTEN 这种自带默认值的项就永远分不清
# "来自默认值"还是"人来指定过"，结果是该问的没问、不该问的反复问。
PUBLIC_LISTEN_GIVEN=""
ADMIN_LISTEN_GIVEN=""
TLS_MODE_GIVEN=""
ADMIN_USER_GIVEN=""
ACME_CHALLENGE_GIVEN=""
if [ -n "${PROBE_PUBLIC_LISTEN:-}" ]; then PUBLIC_LISTEN_GIVEN="1"; fi
if [ -n "${PROBE_ADMIN_LISTEN:-}" ]; then ADMIN_LISTEN_GIVEN="1"; fi
if [ -n "${PROBE_TLS_MODE:-}" ]; then TLS_MODE_GIVEN="1"; fi
if [ -n "${PROBE_ADMIN_USER:-}" ]; then ADMIN_USER_GIVEN="1"; fi
if [ -n "${PROBE_ACME_CHALLENGE:-}" ]; then ACME_CHALLENGE_GIVEN="1"; fi

# yaml_list 把 "a, b,c" 渲染成 YAML 行内序列 `"a", "b", "c"`。
# 用 tr 把逗号换成空格再交给 for 做字段切分——这里刻意**只认逗号**：
# 域名/IP 清单里本来就不该出现空格，容忍空格会把 "a b" 这样一个输入
# 悄悄拆成两个条目（配置文件能写、后台设置页存不进去的同类问题）。
yaml_list() {
	_out=""
	for _x in $(printf '%s' "${1:-}" | tr ',' ' '); do
		if [ -z "$_out" ]; then _out="\"$_x\""; else _out="$_out, \"$_x\""; fi
	done
	printf '%s' "$_out"
}

usage() {
	cat <<'EOF'
deploy-master.sh —— Master 一键部署

必需：
  --bin <路径>              probe-master 二进制（或已安装时省略以只做配置/服务）

推荐：
  --public-origin <URL>     公开站对外地址（6.1 源分离，配了才做 Host 白名单）
  --admin-origin  <URL>     管理端对外地址
  --admin-user <用户名>      管理员用户名（默认 ops）
  --admin-password <口令>    不传则交互输入；也可用环境变量 PROBE_ADMIN_PASSWORD

可选：
  --dir <目录>              配置目录（默认 /etc/probe-master）
  --data <目录>             数据目录（默认 /var/lib/probe-master）
  --bin-dir <目录>          二进制安装目录（默认 /usr/local/bin）
  --public-listen <地址>     默认 0.0.0.0:8080（只写端口如 9000 = 0.0.0.0:9000）
  --admin-listen <地址>      默认 127.0.0.1:8081（只监听回环，公网请走反代）
  --dist-base <URL>         发布文件镜像前缀
  --no-service              不注册 systemd（容器场景）
  --force-config            覆盖已有配置
  --dry-run                 只打印计划与将要写入的配置
  --yes                     不提问（等价于 PROBE_NO_ASK=1）
  -h, --help                显示本帮助

参数没给全时本脚本会交互提问（TLS 方式、监听端口、公开/管理端地址、管理员）。
不问的情形：PROBE_NO_ASK=1 / 标准输入不是终端 / 该项已显式给出。

TLS：
  --tls-mode <none|files|selfsigned|acme>   默认 none
  --tls-cert <文件> --tls-key <文件>          mode=files
  --tls-domain <域名[,域名]>                  mode=acme/selfsigned
  --tls-ip <IP[,IP]>                          mode=acme/selfsigned
  --acme-email <邮箱>                         mode=acme（可选）
  --acme-ca <CA>                             默认 letsencrypt
  --acme-challenge <http-01|tls-alpn-01|dns-01>
                                             默认 http-01
  dns-01 的凭据用环境变量（不进 shell 历史）：
    PROBE_ACME_DNS_PROVIDER   默认 cloudflare
    PROBE_ACME_DNS_TOKEN      Cloudflare API Token

卸载：
  --uninstall               停服务并删除单元/二进制/配置/数据/系统用户
                            （默认连数据一起清；数据目录里有数据库与 master.key）
  --keep-data               与 --uninstall 同用，保留数据目录
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--bin) BIN_SRC="${2:-}"; shift 2 ;;
	--dir) DIR="${2:-}"; shift 2 ;;
	--data) DATA="${2:-}"; shift 2 ;;
	--bin-dir) BIN_DIR="${2:-}"; shift 2 ;;
	--public-listen) PUBLIC_LISTEN="${2:-}"; PUBLIC_LISTEN_GIVEN="1"; shift 2 ;;
	--admin-listen) ADMIN_LISTEN="${2:-}"; ADMIN_LISTEN_GIVEN="1"; shift 2 ;;
	--public-origin) PUBLIC_ORIGIN="${2:-}"; shift 2 ;;
	--admin-origin) ADMIN_ORIGIN="${2:-}"; shift 2 ;;
	--dist-base) DIST_BASE="${2:-}"; shift 2 ;;
	--admin-user) ADMIN_USER="${2:-}"; ADMIN_USER_GIVEN="1"; shift 2 ;;
	--admin-password) ADMIN_PASSWORD="${2:-}"; shift 2 ;;
	--no-service) NO_SERVICE="1"; shift ;;
	--force-config) FORCE_CONFIG="1"; shift ;;
	--dry-run) DRY_RUN="1"; shift ;;
	--yes | -y) NO_ASK="1"; shift ;;
	--tls-mode) TLS_MODE="${2:-}"; TLS_MODE_GIVEN="1"; shift 2 ;;
	--tls-cert) TLS_CERT="${2:-}"; shift 2 ;;
	--tls-key) TLS_KEY="${2:-}"; shift 2 ;;
	--tls-domain) TLS_DOMAINS="${2:-}"; shift 2 ;;
	--tls-ip) TLS_IPS="${2:-}"; shift 2 ;;
	--acme-email) ACME_EMAIL="${2:-}"; shift 2 ;;
	--acme-ca) ACME_CA="${2:-}"; shift 2 ;;
	--acme-challenge) ACME_CHALLENGE="${2:-}"; ACME_CHALLENGE_GIVEN="1"; shift 2 ;;
	--uninstall) UNINSTALL="1"; shift ;;
	--keep-data) KEEP_DATA="1"; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "未知参数：$1" >&2; usage; exit 2 ;;
	esac
done

# ---------------------------------------------------------------------------
# 0) 交互问答：问什么、什么时候问
# ---------------------------------------------------------------------------
#
# 这一段必须排在下面的参数校验**之前**：先把值问全，校验才有东西可校。
#
# 实现上的一个取舍：开关用**环境变量**（PROBE_NO_ASK）而不是新增一个命令行参数。
# 免编译安装会从 Releases 取**与二进制同标签**的部署脚本，那份老脚本对未知参数是
# 硬失败（exit 2）——多一个参数就等于"新安装脚本 + 老部署脚本"直接装不上。
# 环境变量新旧脚本都能安全接收（老脚本忽略它），所以安装脚本可以无条件导出。
if [ -n "${PROBE_NO_ASK:-}" ]; then NO_ASK="1"; fi
ASK=""
if [ -z "$NO_ASK" ]; then
	if [ -t 0 ] || [ -n "${PROBE_ASK_FORCE:-}" ]; then ASK="1"; fi
fi

# read_line <变量名>：读一行。
#
# bash 下用 `read -e`（走 readline）：退格、方向键、Home/End 都正常，且 ^H 与 ^?
# 两种退格码都被绑为 backward-delete-char——这正是"按退格却打出 ^H"的根治办法。
# 其它 shell 退回普通 read（由内核 tty 决定，行为不如前者，但不比现在更差）。
# 脚本开头会尽量把自己换到 bash 下运行，所以这里通常走得到 -e 分支。
read_line() {
	if [ -n "${BASH_VERSION:-}" ]; then
		IFS= read -e -r "$1"
	else
		IFS= read -r "$1"
	fi
}

# ask <提示> <默认值>：结果放进 ASK_REPLY；提示走 stderr，避免 `> 日志` 时交互看不见。
# 不问时直接把默认值交回去——"问"与"不问"对后面的逻辑是同一件事。
ask() {
	if [ -z "$ASK" ]; then ASK_REPLY="${2:-}"; return 0; fi
	if [ -n "${2:-}" ]; then printf '%s [%s]: ' "$1" "$2" >&2
	else printf '%s: ' "$1" >&2; fi
	read_line ASK_REPLY || ASK_REPLY=""
	if [ -z "$ASK_REPLY" ]; then ASK_REPLY="${2:-}"; fi
}

# ask_secret <提示>：不回显地读一行（API Token 这类）。
# 刻意**不用** read -e：readline 会重新打开回显，把刚敲的 Token 打到屏幕上。
ask_secret() {
	if [ -z "$ASK" ]; then ASK_REPLY=""; return 0; fi
	printf '%s（不回显）: ' "$1" >&2
	stty -echo 2>/dev/null || true
	IFS= read -r ASK_REPLY || ASK_REPLY=""
	stty echo 2>/dev/null || true
	printf '\n' >&2
}

# ask_required <提示>：不允许空答案（ACME 的域名、files 的证书路径这类）。
# 最多重问三次后把空串交回调用方，由下面的校验段统一报错——一处判比两处判更不易漂移。
ask_required() {
	if [ -z "$ASK" ]; then ASK_REPLY=""; return 0; fi
	_ai=0
	while [ "$_ai" -lt 3 ]; do
		ask "$1" ""
		if [ -n "$ASK_REPLY" ]; then return 0; fi
		printf '  （这一项不能为空）\n' >&2
		_ai=$((_ai + 1))
	done
	return 0
}

# confirm <提示>：回车算"是"。
confirm() {
	if [ -z "$ASK" ]; then return 0; fi
	printf '%s [Y/n]: ' "$1" >&2
	read_line _ca || _ca=""
	case "$_ca" in
	n | N | no | NO | No) return 1 ;;
	*) return 0 ;;
	esac
}

# ---------------------------------------------------------------------------
# 卸载：--uninstall
# ---------------------------------------------------------------------------
#
# 位置刻意在 TLS 问答**之前**：卸载不该再问"你要哪种证书"。
#
# "卸载干净"包含六件事，少任何一件都会留下需要人手收拾的尾巴：
# 服务单元、二进制、配置目录（master.yaml + master.key）、数据目录（数据库 +
# 主题 + 发布件 + 证书）、系统用户与组、以及其它 init 系统的残留。
#
# 默认**连数据一起删**（这才是"干净"），但要过两道闸：
#   ① 只肯删"看起来是自己建的"路径（路径里必须含 probe）——`--data /var/lib`
#      这种手滑不该变成一场灾难；
#   ② 交互执行时还要再确认一次（默认 No）。
# 想留数据加 --keep-data。
if [ -n "$UNINSTALL" ]; then
	if [ -z "$DRY_RUN" ] && [ "$(id -u)" -ne 0 ]; then
		echo "错误：卸载需要 root 或 sudo（--dry-run 不需要）。" >&2
		exit 1
	fi

	# uninstall_path_ok：这条路径能不能被我们删。
	#
	# 判据是"这个路径像不像本脚本创建的"，而不是"用户确认过"：`--data` 是可传参的，
	# 一个手滑的 `--data /var/lib` 会把整个目录清空。三项都要满足：
	#   ① 绝对路径；② 含 probe（"本脚本建的"唯一可用证据）；③ 至少两级。
	#
	# ③ 不是多余的：`/probe` 恰好被 ② 放过（`/*probe*` 里两个 `*` 都能匹配空），
	# 而它同样是"根目录下顺手建的一个名字"，删掉不可逆。三项任意一项判不出来就拒绝，
	# 把 rm -rf 留给用户自己决定。
	uninstall_path_ok() {
		case "$1" in
		"" | "/" | "/etc" | "/usr" | "/var" | "/opt" | "/home" | "/root" | "/srv" | "/tmp") return 1 ;;
		esac
		case "$1" in
		/*probe*) : ;;
		*) return 1 ;;
		esac
		case "${1#/}" in
		*/*) return 0 ;;
		esac
		return 1
	}

	for _p in "$DIR" "$DATA"; do
		if ! uninstall_path_ok "$_p"; then
			echo "错误：拒绝删除 $_p —— 它不像本脚本创建的目录（路径中应含 probe）。" >&2
			echo "      确认要删请手工执行：rm -rf '$_p'" >&2
			exit 2
		fi
	done

	if [ -z "$KEEP_DATA" ] && [ -z "$DRY_RUN" ] && [ -n "$ASK" ]; then
		printf '这会删除数据目录 %s（含数据库与 master.key），不可恢复。继续？ [y/N]: ' "$DATA" >&2
		read_line _uc || _uc=""
		case "$_uc" in
		y | Y | yes | YES | Yes) : ;;
		*)
			echo "已取消，未改动系统。" >&2
			exit 0
			;;
		esac
	fi

	# rm_path <路径> <说明>：删除并把结果**回显出来**。
	# 逐项回显是刻意的：卸载最常出的问题不是删错，而是"以为删了、实际因为
	# 只读挂载或权限没删掉"，而这件事对用户完全不可见。
	rm_path() {
		_p="$1"
		_what="$2"
		if [ -n "$DRY_RUN" ]; then
			if [ -e "$_p" ]; then echo "    [dry-run] 将删除 $_p（$_what）"
			else echo "    $_p 不存在（$_what）"; fi
			return 0
		fi
		if [ ! -e "$_p" ]; then
			echo "    $_p 不存在（$_what）"
			return 0
		fi
		rm -rf "$_p" 2>/dev/null || true
		if [ -e "$_p" ]; then
			echo "    ⚠ 残留：$_p（$_what）—— 请手工处理（只读挂载？权限？）"
		else
			echo "    已删除 $_p（$_what）"
		fi
	}

	echo "==> 卸载 Server Probe Master"
	if [ -n "$DRY_RUN" ]; then
		echo "    （dry-run：只列出将要做的事，不删除任何东西）"
	fi

	echo "==> 1/6 停止并注销服务"
	UNIT_PATH=/etc/systemd/system/probe-master.service
	if [ -n "$DRY_RUN" ]; then
		echo "    [dry-run] systemctl disable --now probe-master；删除 $UNIT_PATH"
	elif command -v systemctl >/dev/null 2>&1; then
		systemctl disable --now probe-master >/dev/null 2>&1 || true
		if [ -f "$UNIT_PATH" ]; then
			rm -f "$UNIT_PATH" 2>/dev/null || true
			systemctl daemon-reload >/dev/null 2>&1 || true
			systemctl reset-failed probe-master >/dev/null 2>&1 || true
			if [ -f "$UNIT_PATH" ]; then echo "    ⚠ 残留：$UNIT_PATH"
			else echo "    已停止并删除 $UNIT_PATH"; fi
		else
			echo "    $UNIT_PATH 不存在（未注册过 systemd 服务）"
		fi
	else
		echo "    没有 systemctl：跳过（若用 OpenRC，见下一步）"
	fi

	echo "==> 2/6 其它 init 系统残留"
	for _f in /etc/init.d/probe-master /etc/supervisor/conf.d/probe-master.conf; do
		if [ -e "$_f" ]; then
			rm_path "$_f" "其它进程管理器的单元"
			if command -v rc-update >/dev/null 2>&1; then
				rc-update del probe-master >/dev/null 2>&1 || true
			fi
		else
			echo "    $_f 不存在"
		fi
	done

	echo "==> 3/6 删除二进制"
	rm_path "$BIN_DIR/probe-master" "Master 可执行文件"

	echo "==> 4/6 删除配置目录"
	rm_path "$DIR" "配置（master.yaml、master.key）"

	echo "==> 5/6 删除数据目录"
	if [ -n "$KEEP_DATA" ]; then
		echo "    保留（--keep-data）：$DATA"
	else
		rm_path "$DATA" "数据（数据库、主题、发布件、证书）"
	fi

	echo "==> 6/6 删除系统用户"
	if [ -n "$DRY_RUN" ]; then
		echo "    [dry-run] userdel probe；groupdel probe"
	elif id -u probe >/dev/null 2>&1; then
		# 用户还被占用时 userdel 会失败（同名进程在跑、或别处仍以 probe 运行）。
		# 不 set -e 中断：剩下的清理项更该跑完，失败原因单独报出来。
		userdel probe >/dev/null 2>&1 || true
		groupdel probe >/dev/null 2>&1 || true
		if id -u probe >/dev/null 2>&1; then
			echo "    ⚠ 用户 probe 仍在（可能仍有进程以它运行，或被别的服务使用）"
		else
			echo "    已删除用户与组 probe"
		fi
	else
		echo "    用户 probe 不存在"
	fi

	echo
	if [ -n "$DRY_RUN" ]; then
		echo "==> dry-run 结束：以上操作均未执行。去掉 --dry-run 即真正卸载。"
	else
		echo "==> 卸载完成。"
		echo "    未删除的只有系统包（本脚本从未安装过它们）与你自己放进来的文件。"
	fi
	exit 0
fi

# detect_public_ip：只用于"给一个像样的默认值"，拿不到就返回空，不算错误。
# 先问外部服务：NAT 后面的机器，本地路由里的源地址是私网地址，没有参考价值。
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

# split_port <地址>：取端口。取**最后一个**冒号之后，`[::]:8080` 这种 IPv6 字面量也对。
split_port() { printf '%s' "${1##*:}"; }

# normalize_listen <输入> <缺省主机>：允许只写端口（"9000"），也允许写 "IP:端口"。
# 地址部分是什么形式交给内核判断；在这里校验主机名/IP 只会把合法写法误伤。
normalize_listen() {
	case "$1" in
	*:*) printf '%s' "$1" ;;
	*) printf '%s:%s' "$2" "$1" ;;
	esac
}

listen_port_ok() {
	_lp="$(split_port "$1")"
	case "$_lp" in
	'' | *[!0-9]*) return 1 ;;
	esac
	if [ "$_lp" -ge 1 ] && [ "$_lp" -le 65535 ]; then return 0; fi
	return 1
}

# origin_default <http|https> <主机> <端口>：端口是该协议的默认值时不写出来——
# `https://example.com` 比 `https://example.com:443` 更像人写的东西，也让
# install.master_url 与用户自己填的地址长得一样。
origin_default() {
	case "$2" in
	*:*) _oh="[$2]" ;;
	*) _oh="$2" ;;
	esac
	if [ "$1" = "http" ] && [ "$3" = "80" ]; then printf '%s://%s' "$1" "$_oh"; return 0; fi
	if [ "$1" = "https" ] && [ "$3" = "443" ]; then printf '%s://%s' "$1" "$_oh"; return 0; fi
	printf '%s://%s:%s' "$1" "$_oh" "$3"
}

# admin_url_from <对外地址>：补成后台入口地址，幂等。
# 直接拼 "$ORIGIN/admin/" 会在地址本身带尾斜杠时得到 "...//admin/"——
# 多数浏览器能容忍，但它会出现在用户复制粘贴的那一行里，值得顺手做对。
admin_url_from() {
	_o="${1%/}"
	case "$_o" in
	*/admin) printf '%s/' "$_o" ;;
	*/admin/) printf '%s' "$_o" ;;
	*) printf '%s/admin/' "$_o" ;;
	esac
}

# admin_user_ok：用户名只允许字母、数字与 . _ @ -
# 有这个判断是因为实际发生过：交互提示"管理员用户名 [ops]"时，有人把上一题的
# 答案（一个 host:port）又粘了一遍，脚本照单全收，然后 init-admin 建出一个
# 名字里带冒号的管理员——后台登不进去，且失败发生在"部署成功"之后。
admin_user_ok() {
	case "$1" in
	'' | *[!A-Za-z0-9._@-]*) return 1 ;;
	esac
	return 0
}

# 公开 IP 只在"真要问 public_origin"时才去探测：它要发一次外部请求，
# 而绝大多数调用（自动化、CI、显式给了地址）根本不需要这个默认值。
PUBIP=""
if [ -z "$PUBLIC_ORIGIN" ] && [ -n "$ASK" ]; then PUBIP="$(detect_public_ip)"; fi

if [ -z "$TLS_MODE_GIVEN" ]; then
	if [ -n "$ASK" ]; then
		cat >&2 <<'EOF'

下面几个问题决定对外访问方式，直接回车取方括号里的默认值。

TLS 方式：
  1) none        由前置 Caddy/Nginx 终结 TLS（默认；也适合本机/内网调试）
  2) acme        内置 ACME 客户端自动申请证书（需要一个已解析到本机的域名）
  3) selfsigned  自签证书（用纯 IP 访问时，这是唯一可行的加密方式）
  4) files       使用你已有的证书与私钥

想让 Agent 直接用 https 连上来，就得在这里选一种；选 none 就必须由前置反代终结 TLS。
EOF
	fi
	ask "选择 TLS 方式（1/2/3/4）" "1"
	case "$ASK_REPLY" in
	1 | none | NONE | None) TLS_MODE="none" ;;
	2 | acme | ACME | Acme) TLS_MODE="acme" ;;
	3 | selfsigned | SelfSigned | SELF_SIGNED) TLS_MODE="selfsigned" ;;
	4 | files | FILES | Files) TLS_MODE="files" ;;
	*) TLS_MODE="$ASK_REPLY" ;;
	esac
fi

# 每种 TLS 模式各自的必填项。这里只负责"问"，合法性由下面的校验段统一判。
if [ "$TLS_MODE" = "acme" ]; then
	if [ -z "$TLS_DOMAINS$TLS_IPS" ]; then
		ask_required "域名（逗号分隔可多个；必须已解析到本机）"
		TLS_DOMAINS="$ASK_REPLY"
	fi
	if [ -z "$ACME_EMAIL" ]; then
		ask "邮箱（可选，CA 用它发到期提醒）" ""
		ACME_EMAIL="$ASK_REPLY"
	fi
	if [ -z "$ACME_CHALLENGE_GIVEN" ] && [ -n "$ASK" ]; then
		cat >&2 <<'EOF'

证书的校验方式（三个前提条件不同，选错了会在申请时才失败）：
  1) http-01      本机在 80 端口应答（默认；最省事，需要 80 可达且没被别的服务占着）
  2) tls-alpn-01  本机在 443 端口用 ALPN 应答（80 被占用、且 443 由本进程直接监听时选它）
  3) dns-01       在 DNS 服务商处写一条 TXT 记录（支持通配符 *.example.com；需要 API Token）
EOF
		ask "选择校验方式（1/2/3）" "1"
		case "$ASK_REPLY" in
		1 | http-01) ACME_CHALLENGE="http-01" ;;
		2 | tls-alpn-01) ACME_CHALLENGE="tls-alpn-01" ;;
		3 | dns-01) ACME_CHALLENGE="dns-01" ;;
		*) ACME_CHALLENGE="$ASK_REPLY" ;;
		esac
	fi
	# dns-01 需要 DNS 服务商凭据。只问、不校验——合法性交给下面的校验段一处判，
	# 免得"能不能放过"这件事在脚本里有两个各说各话的版本。
	if [ "$ACME_CHALLENGE" = "dns-01" ]; then
		if [ -z "$ACME_DNS_PROVIDER" ]; then ACME_DNS_PROVIDER="cloudflare"; fi
		if [ -z "$ACME_DNS_TOKEN" ] && [ -n "$ASK" ]; then
			cat >&2 <<'EOF'
  · 建议在 Cloudflare 单独建一个 Token：权限只给 Zone:DNS:Edit 与 Zone:Zone:Read，
    且只覆盖要签发的那个域名（不要用账户级 Global API Key）。
EOF
			ask_secret "Cloudflare API Token"
			ACME_DNS_TOKEN="$ASK_REPLY"
		fi
	fi
fi

if [ "$TLS_MODE" = "files" ]; then
	if [ -z "$TLS_CERT" ]; then
		ask_required "证书文件路径"
		TLS_CERT="$ASK_REPLY"
	fi
	if [ -z "$TLS_KEY" ]; then
		ask_required "私钥文件路径"
		TLS_KEY="$ASK_REPLY"
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

# 先问端口，再据此给"公开访问地址"一个像样的默认值。顺序反过来的话，
# 默认地址里的端口会和用户刚选的端口对不上（提示 8080、实际听 9000）。
if [ -z "$PUBLIC_LISTEN_GIVEN" ] && [ -n "$ASK" ]; then
	ask "公开站监听（端口，或 IP:端口；0.0.0.0 表示所有网卡）" "$PUBLIC_LISTEN"
	_new_listen="$(normalize_listen "$ASK_REPLY" "0.0.0.0")"
	if listen_port_ok "$_new_listen"; then
		PUBLIC_LISTEN="$_new_listen"
	else
		printf '警告：%s 不是合法端口，沿用 %s\n' "$ASK_REPLY" "$PUBLIC_LISTEN" >&2
	fi
fi

if [ -z "$ADMIN_LISTEN_GIVEN" ] && [ -n "$ASK" ]; then
	ask "管理端监听（端口，或 IP:端口；127.0.0.1 表示只有本机能访问）" "$ADMIN_LISTEN"
	_new_listen="$(normalize_listen "$ASK_REPLY" "127.0.0.1")"
	if listen_port_ok "$_new_listen"; then
		ADMIN_LISTEN="$_new_listen"
	else
		printf '警告：%s 不是合法端口，沿用 %s\n' "$ASK_REPLY" "$ADMIN_LISTEN" >&2
	fi
fi

if [ -z "$PUBLIC_ORIGIN" ]; then
	if [ -n "$PUBIP" ]; then _host="$PUBIP"; else _host="127.0.0.1"; fi
	_pport="$(split_port "$PUBLIC_LISTEN")"
	case "$TLS_MODE" in
	acme)
		# 用第一个标识符做默认地址；端口交给 origin_default 处理——
		# 监听 8443 却把默认地址写成 https://域名，等于默认值本身就指错了地方。
		_dom0="$(printf '%s' "$TLS_DOMAINS" | cut -d, -f1)"
		_ip0="$(printf '%s' "$TLS_IPS" | cut -d, -f1)"
		if [ -n "$_dom0" ]; then _def="$(origin_default https "$_dom0" "$_pport")"
		elif [ -n "$_ip0" ]; then _def="$(origin_default https "$_ip0" "$_pport")"
		else _def="$(origin_default https "$_host" "$_pport")"; fi
		;;
	selfsigned | files) _def="$(origin_default https "$_host" "$_pport")" ;;
	*) _def="$(origin_default http "$_host" "$_pport")" ;;
	esac
	if [ -z "$ASK" ]; then
		# 非交互时**绝不**用 127.0.0.1 兜底：它会被写进 public_origin，随后成为
		# install.master_url，于是所有 Agent 都去连自己的回环地址——装得成、永远不上报。
		# 宁可为空（留空时 Master 按请求 Host 回填，走域名访问是正常的）。
		if [ -z "$PUBIP" ]; then _def=""; fi
	fi
	ask "公开访问地址（对外 URL，不带末尾斜杠）" "$_def"
	PUBLIC_ORIGIN="$ASK_REPLY"
fi

if [ -z "$ADMIN_ORIGIN" ] && [ -n "$ASK" ]; then
	ask "管理端地址（回车=与公开站同一个域名，即「同站」）" ""
	ADMIN_ORIGIN="$ASK_REPLY"
fi

if [ -z "$ADMIN_USER_GIVEN" ] && [ -n "$ASK" ]; then
	_ui=0
	while [ "$_ui" -lt 3 ]; do
		ask "管理员用户名" "$ADMIN_USER"
		if admin_user_ok "$ASK_REPLY"; then
			ADMIN_USER="$ASK_REPLY"
			break
		fi
		printf '  （用户名只允许字母、数字与 . _ @ -）\n' >&2
		_ui=$((_ui + 1))
	done
fi
if [ -z "$ADMIN_USER" ]; then ADMIN_USER="ops"; fi
# 兜底：--admin-user / PROBE_ADMIN_USER 那条路径不经过上面的循环。
if ! admin_user_ok "$ADMIN_USER"; then
	echo "错误：管理员用户名只允许字母、数字与 . _ @ -（收到 $ADMIN_USER）。" >&2
	exit 2
fi

if [ -n "$ASK" ]; then
	echo >&2
	echo "即将执行：" >&2
	printf '  %s：%s\n' "公开站监听" "$PUBLIC_LISTEN" >&2
	printf '  %s：%s\n' "管理端监听" "$ADMIN_LISTEN" >&2
	printf '  %s：%s\n' "公开地址" "${PUBLIC_ORIGIN:-（留空：按请求 Host 回填）}" >&2
	printf '  %s：%s\n' "管理地址" "${ADMIN_ORIGIN:-（与公开站同一个域名）}" >&2
	_tls_note="$TLS_MODE"
	if [ "$TLS_MODE" = "acme" ]; then _tls_note="$TLS_MODE（challenge=$ACME_CHALLENGE）"; fi
	printf '  %s：%s\n' "TLS" "$_tls_note" >&2
	printf '  %s：%s\n' "管理员" "$ADMIN_USER（口令随后输入）" >&2
	printf '  %s：%s\n' "配置目录" "$DIR" >&2
	printf '  %s：%s\n' "数据目录" "$DATA" >&2
	if [ -n "$NO_SERVICE" ]; then
		printf '  %s：%s\n' "服务" "不注册（--no-service）" >&2
	else
		printf '  %s：%s\n' "服务" "systemd（probe-master，开机自启）" >&2
	fi
	echo >&2
	if ! confirm "继续？"; then
		echo "已取消，未改动系统。" >&2
		exit 0
	fi
fi

# ---- TLS 参数校验：宁可在这里失败，也不要写出一个"看起来配好了"的配置文件 ----
#
# 配置文件是**严格解析**的（LoadMaster 用 KnownFields），写错键名会让 Master
# 启动即失败；但"值合法、语义不成立"（比如 acme 却没给任何域名/IP）解析得过，
# 却要等到启动后申请证书才暴露。所以这里把每种模式的前置条件前置校验。
case "$TLS_MODE" in
none | files | selfsigned | acme) : ;;
*)
	echo "错误：--tls-mode 只能是 none / files / selfsigned / acme（收到 $TLS_MODE）。" >&2
	exit 2
	;;
esac

if [ -n "$TLS_DOMAINS$TLS_IPS$ACME_EMAIL$TLS_CERT$TLS_KEY" ] && [ "$TLS_MODE" = "none" ]; then
	echo "错误：给了证书相关参数却 --tls-mode none。要么把模式写对，要么去掉这些参数——" >&2
	echo "      否则证书不会生效，而命令行看起来像「配了 TLS」。" >&2
	exit 2
fi

if [ "$TLS_MODE" = "files" ]; then
	[ -n "$TLS_CERT" ] && [ -n "$TLS_KEY" ] || {
		echo "错误：mode=files 必须同时给 --tls-cert 与 --tls-key。" >&2
		exit 2
	}
	for f in "$TLS_CERT" "$TLS_KEY"; do
		[ -f "$f" ] || { echo "错误：找不到证书文件 $f。" >&2; exit 2; }
	done
fi

if [ "$TLS_MODE" = "acme" ]; then
	if [ -z "$TLS_DOMAINS$TLS_IPS" ]; then
		echo "错误：mode=acme 至少要给一个 --tls-domain 或 --tls-ip（否则没有可申请的标识符）。" >&2
		exit 2
	fi
	case "$ACME_CHALLENGE" in
	http-01 | tls-alpn-01 | dns-01) : ;;
	*) echo "错误：--acme-challenge 只能是 http-01 / tls-alpn-01 / dns-01。" >&2; exit 2 ;;
	esac
	# dns-01 的两条专属前置条件。都放在这里报错，而不是等申请证书时才失败——
	# 那时的错误信息离原因很远（"写入 dns-01 挑战记录失败"），排起来要绕一大圈。
	if [ "$ACME_CHALLENGE" = "dns-01" ]; then
		if [ -z "$TLS_DOMAINS" ]; then
			echo "错误：challenge=dns-01 只支持域名（DNS 里没有 IP 的挑战记录可写），" >&2
			echo "      请用 --tls-domain 给出域名，或改用 http-01/tls-alpn-01。" >&2
			exit 2
		fi
		if [ -z "$ACME_DNS_TOKEN" ]; then
			echo "错误：challenge=dns-01 必须提供 DNS API Token。" >&2
			echo "      交互执行时会被提示输入；无人值守请设 PROBE_ACME_DNS_TOKEN。" >&2
			exit 2
		fi
	fi
fi

# TLS 与 public_origin 的协议必须自洽。
# 反过来（装了证书却只给 http:// 的对外地址）是最难发现的一种"部署成功"：
# 服务在听 443、访客拿到的是明文，install.master_url 还会把 Agent 指向 http://。
if [ -n "$PUBLIC_ORIGIN" ]; then
	case "$TLS_MODE:$PUBLIC_ORIGIN" in
	none:https://*)
		echo "警告：--tls-mode none 但 --public-origin 是 https://。若 TLS 由前置反代终结，" >&2
		echo "      请把反代网段填进 trusted_proxies，否则 Cookie / HSTS 判定会走明文分支。" >&2
		;;
	files:http://* | selfsigned:http://* | acme:http://*)
		echo "错误：--tls-mode $TLS_MODE 与 --public-origin $PUBLIC_ORIGIN 不一致。" >&2
		echo "      配了证书就应当对外宣告 https://（端口不是 443 时写成 https://host:端口）。" >&2
		exit 2
		;;
	esac
fi

# 配置路径与渲染函数放在这里（而不是"3/7"那一步里面）：--dry-run 要在改动系统
# **之前**就把将要写入的内容打出来。
CFG="$DIR/master.yaml"

# render_config：把最终生效的参数渲染成 master.yaml。
# 抽成函数是为了让 --dry-run 打印"将要写入的内容"时走**同一段代码**——
# 预览与落盘各写一份，迟早出现"预览说一套、落盘写另一套"。
#
# 只写必需项，其余走代码内默认值；完整可选项见 configs/master.yaml.example。
render_config() {
	echo "# 由 scripts/deploy-master.sh 生成于 $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
	echo "# 完整配置项与说明见 configs/master.yaml.example"
	echo "public_listen: \"$PUBLIC_LISTEN\""
	echo "admin_listen: \"$ADMIN_LISTEN\""
	echo "data_dir: \"$DATA\""
	echo "themes_dir: \"$DATA/themes\""
	echo "master_key_file: \"$DIR/master.key\""
	echo "trusted_proxies: []   # 直连保持为空；前置反代时填反代 IP/CIDR（13.2 S5）"
	if [ -n "$PUBLIC_ORIGIN" ]; then echo "public_origin: \"$PUBLIC_ORIGIN\""; fi
	if [ -n "$ADMIN_ORIGIN" ]; then echo "admin_origin: \"$ADMIN_ORIGIN\""; fi
	# 这里**绝不能**写 install: 段。install.mode / install.master_url /
	# install.dist_base 都是**数据库设置**（读取方式是 repo.GetSettingDefault），
	# 不是配置文件的字段：MasterConfig 里没有 install 这个键，而 LoadMaster 走
	# **严格解析**（KnownFields(true)）——写了它会**直接启动失败并点名是哪个键**。
	# 之所以值得写这么长一段，是因为旧版解析是非严格的：脚本写进去的这一段
	# 会被整段静默吞掉，配置文件里白纸黑字、后台设置页却是空的，只能靠人比对。
	# 它们的唯一入口是后台「设置 → 安装与下载」。
	# master_url 未设置时会回落到 public_origin（见 cmd/probe-master/main.go
	# 的 MasterURL 闭包），所以给了 --public-origin 就不必再单独设它。
	case "$TLS_MODE" in
	none)
		echo 'tls:'
		echo '  mode: "none"               # 由前置 Caddy/Nginx 终结 TLS，或本机/内网调试'
		;;
	files)
		echo 'tls:'
		echo '  mode: "files"              # 自定义证书；文件变更会热加载，无需重启'
		echo "  cert_file: \"$TLS_CERT\""
		echo "  key_file:  \"$TLS_KEY\""
		;;
	selfsigned)
		echo 'tls:'
		echo '  mode: "selfsigned"         # 自签：SAN 覆盖下面的域名与 IP（IP 证书的唯一可用形态）'
		echo '  selfsigned:'
		# 空 hosts 时写 hosts: []，让"留空则默认 localhost/127.0.0.1/::1"这条语义
		# 在配置文件里**看得见**，而不是靠人去读代码。
		if [ -n "$TLS_DOMAINS$TLS_IPS" ]; then
			echo "    hosts: [$(yaml_list "${TLS_DOMAINS}${TLS_IPS:+,$TLS_IPS}")]"
		else
			echo '    hosts: []                 # 留空 = localhost / 127.0.0.1 / ::1'
		fi
		;;
	acme)
		echo 'tls:'
		echo '  mode: "acme"               # 内置 ACME v2 客户端，自动申请与续期'
		echo '  acme:'
		# 一律用 if 而不是 `[ ... ] && echo`：后者在 set -e 下能否中断取决于
		# 它在管道/列表里的位置，是一个很容易改坏的隐式约定。
		if [ -n "$ACME_EMAIL" ]; then echo "    email: \"$ACME_EMAIL\""; fi
		echo "    ca: \"$ACME_CA\""
		echo "    challenge: \"$ACME_CHALLENGE\""
		if [ "$ACME_CHALLENGE" = "dns-01" ]; then
			echo "    dns_provider: \"$ACME_DNS_PROVIDER\""
			# Token 落在 master.yaml（0640 root:probe），与 master.key 同级。
			# 权限够用，但别把这份文件复制进备份目录或贴进工单。
			echo "    dns_api_token: \"$ACME_DNS_TOKEN\""
		fi
		if [ -n "$TLS_DOMAINS" ]; then echo "    domains: [$(yaml_list "$TLS_DOMAINS")]"; fi
		if [ -n "$TLS_IPS" ]; then echo "    ips: [$(yaml_list "$TLS_IPS")]"; fi
		;;
	esac
	echo "log_level: \"info\""
}

# --dry-run 不写任何东西，因此不需要 root——这样可以在桌面机上先把将要写入的
# master.yaml 打出来看一遍。
if [ -z "$DRY_RUN" ] && [ "$(id -u)" -ne 0 ] && [ -z "$NO_SERVICE" ]; then
	echo "错误：请用 root 或 sudo 执行（容器场景可加 --no-service 并以 --dir/--data 指定可写目录）。" >&2
	exit 1
fi

# 健康检查用的地址。0.0.0.0 / [::] 是"监听所有网卡"的写法，**不是可连接的目的地址**：
# Linux 上连 0.0.0.0 通常被当成 localhost（实现细节），而 :: 在多数系统上直接失败。
# 用错了会得到"20 秒内没探到公开 API"，让人以为服务没起来。
HEALTH_ADDR="$PUBLIC_LISTEN"
case "$HEALTH_ADDR" in
0.0.0.0:*) HEALTH_ADDR="127.0.0.1:${HEALTH_ADDR#0.0.0.0:}" ;;
"[::]:"*) HEALTH_ADDR="127.0.0.1:${HEALTH_ADDR#\[::\]:}" ;;
":::") HEALTH_ADDR="127.0.0.1" ;;
esac

if [ -z "$BIN_SRC" ] && [ ! -x "$BIN_DIR/probe-master" ]; then
	echo "错误：既没有 --bin，$BIN_DIR 里也没有已安装的 probe-master。" >&2
	exit 2
fi
if [ -n "$BIN_SRC" ] && [ ! -f "$BIN_SRC" ]; then
	echo "错误：找不到二进制 $BIN_SRC（先跑 make master 或 build.ps1）。" >&2
	exit 2
fi

# --dry-run 到此为止：参数校验与二进制检查都做过了，但一个字节都不写。
# 位置刻意放在这里而不是更早——再早一点就没法把"将要写入的配置文件"预览出来，
# 那是这个开关最有用的部分。
if [ -n "$DRY_RUN" ]; then
	echo
	echo "==> dry-run：以下是将要写入 $CFG 的内容（未改动系统）"
	echo
	render_config
	echo
	echo "==> dry-run：未创建用户/目录、未安装二进制、未注册服务、未初始化管理员。"
	echo "    去掉 --dry-run 即执行（其余参数完全一致）。"
	exit 0
fi

echo "==> 1/7 创建系统用户与目录"
# 服务以专用低权限用户运行（3.2）。Alpine/BusyBox 没有 useradd，退回 adduser。
if ! id -u probe >/dev/null 2>&1; then
	if command -v useradd >/dev/null 2>&1; then
		useradd --system --no-create-home --shell /usr/sbin/nologin probe || \
			useradd -r -s /usr/sbin/nologin probe || echo "警告：创建用户失败，继续（将以现有用户运行）" >&2
	elif command -v adduser >/dev/null 2>&1; then
		adduser -S -H -D probe 2>/dev/null || adduser -S -H probe 2>/dev/null || \
			echo "警告：创建用户失败，继续" >&2
	else
		echo "警告：找不到 useradd/adduser，将以 root 运行（建议手工建 probe 用户）" >&2
	fi
fi
mkdir -p "$DIR" "$DATA" "$DATA/themes"
chmod 0750 "$DIR"
# 数据目录必须 0700：里面有 master.key（告警渠道凭证的主密钥）与数据库。
chmod 0700 "$DATA"
if id -u probe >/dev/null 2>&1; then
	chown -R probe:probe "$DATA" 2>/dev/null || chown -R probe "$DATA" 2>/dev/null || true
fi

echo "==> 2/7 安装二进制"
if [ -n "$BIN_SRC" ]; then
	# 只读根文件系统（部分 LXC 模板、distroless）上 /usr/local/bin 不可写，
	# 用 --bin-dir 指到可写目录即可——systemd 单元里的 ExecStart 会跟着走。
	mkdir -p "$BIN_DIR" || { echo "错误：无法创建 $BIN_DIR（只读根文件系统？用 --bin-dir 指到可写目录）。" >&2; exit 2; }
	if [ "$BIN_SRC" = "$BIN_DIR/probe-master" ]; then
		# 源与目标同一个文件：install/cp 都会报 "are the same file" 并以非 0 退出，
		# 在 set -e 下直接把整个部署打断，而错误信息完全看不出发生了什么。
		echo "    源与目标相同（$BIN_SRC），跳过拷贝"
	elif command -v install >/dev/null 2>&1; then
		install -m 0755 "$BIN_SRC" "$BIN_DIR/probe-master"
	else
		cp -f "$BIN_SRC" "$BIN_DIR/probe-master" && chmod 0755 "$BIN_DIR/probe-master"
	fi
	"$BIN_DIR/probe-master" version || true
fi

echo "==> 3/7 写配置文件 $CFG"
if [ -f "$CFG" ] && [ -z "$FORCE_CONFIG" ]; then
	echo "    已存在，保留（要覆盖请加 --force-config）"
else
	render_config >"$CFG"
	chmod 0640 "$CFG"
	echo "    已写入"
fi

echo "==> 4/7 初始化管理员"
if [ -z "$ADMIN_PASSWORD" ]; then
	printf '管理员 %s 的口令（不回显）：' "$ADMIN_USER" >&2
	stty -echo 2>/dev/null || true
	read -r ADMIN_PASSWORD
	stty echo 2>/dev/null || true
	printf '\n' >&2
fi
if [ -z "$ADMIN_PASSWORD" ]; then
	echo "错误：口令不能为空。" >&2
	exit 2
fi
# init-admin 在管理员已存在时会报错退出，这属于预期（幂等升级），故不 set -e 中断。
"$BIN_DIR/probe-master" init-admin -config "$CFG" \
	-username "$ADMIN_USER" -password "$ADMIN_PASSWORD" 2>&1 | sed 's/^/    /' || \
	echo "    （管理员已存在，跳过；改口令用 probe-master reset-admin）"
# master.key 与备份必须分开存放：丢了它，已配置的告警渠道凭证就解不开了。
if [ -f "$DIR/master.key" ]; then
	chmod 0600 "$DIR/master.key"
	echo "    ⚠ $DIR/master.key 必须**离线另存一份**（0600，不要放进备份目录）"
fi

# ⚠ 归属修正是**必须**的，不是锦上添花。
#
# 服务以 User=probe 运行（见下面的单元），而上面两步都是以 root 执行的：
#   - init-admin 建出来的 probe.db / WAL 归 root —— 服务连库都打不开
#     （"attempt to write a readonly database"）；
#   - 配置与 master.key 归 root 且 0640/0600 —— 服务读不到配置，直接起不来
#     （master.key 是解密告警渠道凭证的主密钥，必须可读）。
# 目录本身先前已 chown 给 probe，但那是在文件被创建**之前**做的，覆盖不到新文件。
if id -u probe >/dev/null 2>&1; then
	chown -R probe:probe "$DATA" 2>/dev/null || chown -R probe "$DATA" 2>/dev/null || true
	chown probe:probe "$CFG" 2>/dev/null || chown probe "$CFG" 2>/dev/null || true
	[ -f "$DIR/master.key" ] && { chown probe:probe "$DIR/master.key" 2>/dev/null || chown probe "$DIR/master.key" 2>/dev/null || true; }
	# 配置目录只读给组：服务需要能"穿过"目录读到 master.yaml。
	chgrp probe "$DIR" 2>/dev/null || true
	# 把归属/权限打印出来。以 root 身份跑 `test -r` 永远是 true，
	# 那种"检查"只会给出虚假的安心；看得见的 ls 才是可核查的。
	ls -l "$CFG" 2>/dev/null | sed 's/^/    /'
	[ -f "$DIR/master.key" ] && ls -l "$DIR/master.key" 2>/dev/null | sed 's/^/    /'
	ls -ld "$DATA" 2>/dev/null | sed 's/^/    /'
else
	echo "    ⚠ 未创建 probe 用户：若服务不以 root 运行，请自行修正 $DIR 与 $DATA 的归属" >&2
fi

if [ -n "$NO_SERVICE" ]; then
	echo "==> 5~7 跳过（--no-service）"
	case "$TLS_MODE" in
	none) _ns_scheme="http" ;;
	*) _ns_scheme="https" ;;
	esac
	echo "    前台运行：$BIN_DIR/probe-master -config $CFG"
	echo
	if [ -n "$ADMIN_ORIGIN" ]; then
		_ns_admin="$(admin_url_from "$ADMIN_ORIGIN")"
	else
		_ns_admin="$_ns_scheme://$ADMIN_LISTEN/admin/"
	fi
	echo "  公开看板    ${PUBLIC_ORIGIN:-$_ns_scheme://$PUBLIC_LISTEN}"
	echo "  管理后台    $_ns_admin"
	echo
	exit 0
fi

echo "==> 5/7 写 systemd 单元"
UNIT=/etc/systemd/system/probe-master.service
if command -v systemctl >/dev/null 2>&1 && [ -d /etc/systemd/system ]; then
	cat >"$UNIT" <<EOF
[Unit]
Description=Server Probe Master
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BIN_DIR/probe-master -config $CFG
Restart=always
RestartSec=3
User=probe
Group=probe
# 最小权限：私有 /tmp、只读系统目录、禁止提权（3.2 / S 系列加固）
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
NoNewPrivileges=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
ReadWritePaths=$DATA
WorkingDirectory=$DATA

[Install]
WantedBy=multi-user.target
EOF
	systemctl daemon-reload
	systemctl enable probe-master >/dev/null 2>&1 || true
	systemctl restart probe-master
	echo "    已启用并启动"
else
	echo "警告：没有 systemd（Alpine/OpenRC 或容器），请自行用进程管理器托管：" >&2
	echo "      $BIN_DIR/probe-master -config $CFG" >&2
	exit 0
fi

echo "==> 6/7 健康检查"
# 公开站 /site 是免鉴权的，用它确认进程真的在听；最多等 20 秒。
#
# 配了 TLS 就必须用 https 探：明文探一个 TLS 端口永远失败，会白等 20 秒再打印
# 一段让人以为服务没起来的状态。
# selfsigned 用 -k 是**有意**的：这里问的是"进程在不在听"，不是"证书链可不可信"，
# 两者混在一起会让自签部署次次看起来像失败。acme 同理——首张证书是后台按需申请的，
# 此刻要么还没有证书、要么刚签发，证书链验证失败并不代表服务没起来。
case "$TLS_MODE" in
none) SCHEME="http"; CURL_EXTRA="" ;;
files) SCHEME="https"; CURL_EXTRA="" ;;
selfsigned | acme) SCHEME="https"; CURL_EXTRA="-k" ;;
esac
i=0
while [ $i -lt 20 ]; do
	if command -v curl >/dev/null 2>&1; then
		if curl -fsS $CURL_EXTRA "${SCHEME}://${HEALTH_ADDR}/api/public/v1/site" >/dev/null 2>&1; then break; fi
	elif command -v wget >/dev/null 2>&1; then
		# wget 在自签/未签发证书下必须 --no-check-certificate，否则同样白等 20 秒。
		if wget -q --no-check-certificate -O- "${SCHEME}://${HEALTH_ADDR}/api/public/v1/site" >/dev/null 2>&1; then break; fi
	else
		break
	fi
	i=$((i + 1))
	sleep 1
done
systemctl --no-pager status probe-master | sed -n '1,8p' || true

echo "==> 证书状态"
# 为什么这一步必须在部署脚本里做：ACME 的首张证书是**后台按需申请**的（刻意如此——
# 一个 DNS 或网络问题不该让整个 Master 起不来）。于是"服务已经启动"与"证书已经就绪"
# 之间有一段**没有任何反馈**的空窗：脚本打完最后一段提示就退出，浏览器却还是打不开，
# 用户唯一能做的就是猜。这一步把那段空窗变成一份有过程、有结果、有原因的报告。
#
# 等多久由 PROBE_TLS_WAIT 控制（默认 120 秒）。申请正常时几秒就返回；
# 等不到也只是"还没结果"，不会把已经装好的部署判成失败。
if [ "$TLS_MODE" = "none" ]; then
	echo "    跳过（tls.mode=none）：证书由前置 Caddy/Nginx 负责"
else
	_cert_wait=0
	if [ "$TLS_MODE" = "acme" ]; then _cert_wait="${PROBE_TLS_WAIT:-120}"; fi
	if [ "$_cert_wait" -gt 0 ] 2>/dev/null; then
		echo "    等待证书就绪（最多 ${_cert_wait} 秒；等不到也不影响使用，Master 每 2 分钟自动重试）"
	fi
	_cert_rc=0
	"$BIN_DIR/probe-master" tls status -config "$CFG" -wait "$_cert_wait" || _cert_rc=$?
	case "$_cert_rc" in
	0) : ;;
	2)
		echo "    （证书还没就绪：申请仍在进行中。随时可再查：）"
		echo "      $BIN_DIR/probe-master tls status -config $CFG -wait 120"
		;;
	*)
		echo "    （证书申请失败。上面列出的排查项每条都能直接复制执行；改完配置后）"
		echo "      systemctl restart probe-master 会立即重试。"
		;;
	esac
fi

echo "==> 7/7 完成"
if [ -z "$PUBLIC_ORIGIN" ]; then
	cat >&2 <<'EOF'
⚠ 没有给 --public-origin，配置文件里就没有 public_origin，install.master_url
  也就拿不到值（它是 DB 设置，未设置时回落到 public_origin）。
  此时 Master 按**请求的 Host** 回填（走域名/反代访问时一切正常）；但若公开站
  是**明文 HTTP 直连 IP**，回填出来的是 https:// —— Agent 会连不上。
  请在后台「设置 → 安装与下载」补 Master 公网地址。
EOF
fi
if [ -n "$DIST_BASE" ]; then
	cat >&2 <<EOF
⚠ 已记录 --dist-base，但它**写不进配置文件**（install.* 全是 DB 设置）。
  请登录管理端，在「设置 → 安装与下载」把「二进制下载源」填成：
      $DIST_BASE
EOF
fi

# ---- 收尾总结：把"地址到底是什么"直接摆出来 ----
#
# 这一屏是刻意加的。此前脚本打完一大段"接下来要做的事"就结束了，用户拿到的
# 是一串命令行片段而不是"我的看板在哪儿、后台在哪儿"——于是最常见的下一步
# 是回头 SSH 进来翻 master.yaml。地址本来就是脚本自己算出来的，没有理由不印。
# 监听地址里的 0.0.0.0 / [::] 是"监听所有网卡"的写法，不是可点击的目的地址。
ADMIN_ADDR="$ADMIN_LISTEN"
case "$ADMIN_ADDR" in
0.0.0.0:*) ADMIN_ADDR="127.0.0.1:${ADMIN_ADDR#0.0.0.0:}" ;;
"[::]:"*) ADMIN_ADDR="127.0.0.1:${ADMIN_ADDR#\[::\]:}" ;;
":::") ADMIN_ADDR="127.0.0.1" ;;
esac
PUBLIC_URL="${PUBLIC_ORIGIN:-$SCHEME://$HEALTH_ADDR}"
if [ -n "$ADMIN_ORIGIN" ]; then
	ADMIN_URL="$(admin_url_from "$ADMIN_ORIGIN")"
	ADMIN_NOTE=""
elif [ -n "$PUBLIC_ORIGIN" ]; then
	ADMIN_URL="$SCHEME://$ADMIN_ADDR/admin/"
	ADMIN_NOTE="（也可经 $PUBLIC_ORIGIN/admin/ 访问，前提是反代把 /admin/ 转到了 $ADMIN_LISTEN）"
else
	ADMIN_URL="$SCHEME://$ADMIN_ADDR/admin/"
	ADMIN_NOTE=""
fi

echo
echo "============================================================"
echo " 部署完成"
echo "============================================================"
printf '  公开看板    %s\n' "$PUBLIC_URL"
printf '  管理后台    %s\n' "$ADMIN_URL"
if [ -n "$ADMIN_NOTE" ]; then printf '              %s\n' "$ADMIN_NOTE"; fi
case "$TLS_MODE" in
none) printf '  传输安全    %s\n' "明文（证书由前置 Caddy/Nginx 负责）" ;;
files) printf '  传输安全    %s\n' "https（证书来自 $TLS_CERT）" ;;
selfsigned) printf '  传输安全    %s\n' "https 自签（浏览器会提示不受信任，属预期）" ;;
acme) printf '  传输安全    %s\n' "https（ACME，challenge=$ACME_CHALLENGE）" ;;
esac
if [ -n "$ADMIN_ORIGIN" ]; then
	printf '  站点形态    %s\n' "同站 / 分离由「设置 → 站点形态」决定（公开 $PUBLIC_URL，后台 $ADMIN_ORIGIN）"
fi
echo "============================================================"
echo

cat <<EOF
  接下来：
  1) 登录管理端 $ADMIN_URL 并**启用 TOTP、保存恢复码**
  2) 在「设置 → 安装与下载」核对这三项（配置文件里写它们无效）：
       · Master 公网地址 install.master_url —— 留空则回落到 public_origin
       · 二进制下载源 install.dist_base  —— 留空则由本进程提供 /dist/
       · 安装入口模式 install.mode       —— 默认 on_demand（存在未用注册码时才开放）
  3) 生成注册码（节点页 → 注册码），用它装第一个 Agent：
       curl -fsSL ${PUBLIC_ORIGIN:-http://$PUBLIC_LISTEN}/install.sh | sudo sh -s -- --code <注册码>
  4) 发布文件入库（Agent 自升级要用）：
       probe-master dist pull --version <版本> --from <发布源 URL> --pubkey /path/minisign.pub
  5) 公开看板与后台要不要分成两个域名 —— 在「设置 → 站点形态」里选：
       · 跟随配置文件（默认）：master.yaml 里有 host 白名单就分离，没有就同站
       · 同站（一个域名）：两个路径都在同一域名下
       · 分离（两个域名）：必须同时填两份域名清单，否则保存会被拒绝
     它是**运行时设置，改完即时生效、不用重启**。
     分离靠的是 Host 而不是监听地址，因此「一个端口 + 两个域名（反代）」也能分离；
     反之**明文 HTTP 直连 IP** 的部署填不出域名清单，别选「分离」——会把后台锁在外面。
     公开看板顶栏的「管理后台」按钮会跟着这个设置走，不必手改。
  6) 配外部心跳 heartbeat_url，并确认能收到失联通知
  7) 自己走一遍备份恢复演练，确认备份在这台机器上真能恢复
  卸载：sh deploy-master.sh --uninstall（--keep-data 保留数据）
EOF

# TLS 的收尾提示按模式给。写死一句"去配 TLS"是没用的：真正会卡住人的
# 是每种模式各自的前置条件（80 端口、证书路径、私钥指纹）。
case "$TLS_MODE" in
none)
	cat >&2 <<'EOF'
ℹ 当前 tls.mode=none：对外**必须**由前置 Caddy/Nginx 终结 TLS，否则会话是明文的。
  反代方案里别忘了把反代网段填进 master.yaml 的 trusted_proxies，
  否则 X-Forwarded-Proto 不被采信、Cookie 会走明文降级分支。
EOF
	;;
files)
	cat >&2 <<EOF
ℹ tls.mode=files：证书读 $TLS_CERT / $TLS_KEY，文件变更会热加载。
  续期由你自己负责（certbot 等），续期时**务必复用私钥**——
  换私钥会改变 SPKI 指纹，所有固定了指纹的 Agent 会同时失联。
EOF
	;;
selfsigned)
	cat >&2 <<EOF
ℹ tls.mode=selfsigned：浏览器会报「证书不受信任」，这是自签的预期行为。
  装 Agent 时需要把指纹钉进去（install.sh 的 --spki-sha256），
  指纹可在后台的「TLS 状态」页或 probe-master 的日志里看到。
EOF
	;;
acme)
	cat >&2 <<EOF
ℹ tls.mode=acme：**首张证书要在后台按需申请**（不阻塞启动）。
  · challenge=$ACME_CHALLENGE
  · http-01 需要 80 端口可达；80 被反代占用且反代会转发 /.well-known/ 时，
    请在配置文件里把 acme.http_addr 设为 off
  · 申请前先确认域名已解析到本机，且 install.master_url 用的是 https
EOF
	;;
esac
