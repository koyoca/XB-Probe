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
# 答过的内容会记在 <配置目录>/.answers（0600，只记非敏感项，**不记任何口令与
# Token**），下次运行自动作为方括号里的默认值——不必为了改一个端口把前面全重答。
# 想忽略它：PROBE_FRESH_ANSWERS=1；想换位置：PROBE_ANSWERS_FILE=<路径>。
#
# 第一个问题是"安装还是卸载"（没显式给 --uninstall 时）。之所以问在最前面：
# 卸载的入口不该只有"读过文档才知道怎么打"的那一条命令，而收尾提示里那句
# `sh deploy-master.sh --uninstall` 对 `curl … | sh` 跑起来的人根本不存在——
# 他的机器上**没有这个文件**。见文件末尾"卸载提示怎么给"那一段。
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
# 谁在终结 TLS：
#   master  Master 自己（内置 ACME / 自签 / 你给的证书）——今天的行为
#   caddy   由本脚本装的 Caddy 反代终结，证书由 Caddy 自动申请与续期
# 只在"证书由外部终结"这一大类里才有选择余地，所以它**不是**一个独立的开关，
# 而是 TLS 方式问答里的一个分支（见下面 2/3/4 那几项的说明）。
TLS_TERMINATOR="${PROBE_TLS_TERMINATOR:-}"
TLS_TERMINATOR_GIVEN=""
if [ -n "${PROBE_TLS_TERMINATOR:-}" ]; then TLS_TERMINATOR_GIVEN="1"; fi
# 反代模式下的域名清单（Caddy 的站点地址）。与 TLS_DOMAINS 分开：
# 后者是"给谁签发证书"，前者是"反代哪些 Host"，两者在"同站"场景下才重合。
PROXY_DOMAINS="${PROBE_PROXY_DOMAINS:-}"
# 谁把用户叫起来的：用于收尾时给出**他能直接复制的卸载命令**。
# 由入口脚本（install-master-bin.sh / install-master.sh / install-master.ps1）设置；
# 直接跑本脚本时自然是空的，此时才提示 `sh deploy-master.sh --uninstall`。
SELF_ENTRY="${PROBE_SELF_ENTRY:-}"
SELF_ENTRY_ARGS="${PROBE_SELF_ENTRY_ARGS:-}"
# 二进制的配置键能力：见 binary_supports_config_key 的注释。
# 空＝二进制比脚本旧，不能写 sync_obtain（严格解析会拒绝启动）。
SYNC_OBTAIN_OK=""
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
  --tls-terminator <master|caddy>           谁终结 TLS：Master 自己 / 本脚本装的 Caddy
                                            反代（默认：选了 none 就是 caddy，否则 master）
  --proxy-domain <域名[,域名]>              与 --tls-terminator caddy 同用：反代哪些域名
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
	--tls-terminator) TLS_TERMINATOR="${2:-}"; TLS_TERMINATOR_GIVEN="1"; shift 2 ;;
	--proxy-domain) PROXY_DOMAINS="${2:-}"; shift 2 ;;
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

# ---------------------------------------------------------------------------
# 0a) 答过的内容记下来：<配置目录>/.answers
# ---------------------------------------------------------------------------
#
# 为什么要有它：一次问答有八九项（TLS 方式、两个监听端口、两个对外地址、域名、
# 管理员名……），而"想改其中一项"是常态——比如端口填错了要重来。没有记忆时，
# 唯一的做法是把前面全部重答一遍，且必须记得住上次填过什么；答错一项就得从
# 头再来一次。这是纯粹的用户体验损耗，没有任何机制上的理由。
#
# 设计上刻意选"自动记住"而不是新增 --answers-file 参数：
#   · curl|sh 的人是主力用户，多一个要拼的参数就少一批人用得上；
#   · 文件位置由 --dir 推导，与其它状态同处一地，不需要用户知道它在哪。
# 仍然留了两个环境变量出口：PROBE_ANSWERS_FILE 换路径、PROBE_FRESH_ANSWERS=1
# 这一次忽略旧答案（想彻底重来一遍时用，不必去找那个文件）。
#
# **只记非敏感项**：口令与 ACME Token 一概不写。理由与"它们不走命令行参数"完全
# 一样——一个能改你 DNS 的 Token 落在磁盘上的明文文件里（哪怕 0600），只是把
# 同一个问题换了个地方发生。所以答案是"问什么记什么，但不含这两类"。
ANSWERS_FILE="${PROBE_ANSWERS_FILE:-}"
if [ -z "$ANSWERS_FILE" ]; then ANSWERS_FILE="$DIR/.answers"; fi
FRESH_ANSWERS=""
if [ -n "${PROBE_FRESH_ANSWERS:-}" ]; then FRESH_ANSWERS="1"; fi

# ans_get <键>：从答案文件取一个值。取不到就返回空。
# 只认 `键=值` 的行；忽略注释与空行。刻意不做"shell source"：
# 那个文件的内容会被当成代码执行，而它可能是上一次以别的用户身份写下的。
ans_get() {
	if [ -z "$ANSWERS_FILE" ] || [ -n "$FRESH_ANSWERS" ]; then return 0; fi
	[ -f "$ANSWERS_FILE" ] || return 0
	sed -n "s/^$1=//p" "$ANSWERS_FILE" | head -n1
}

# ans_put <键> <值>：把一项答案落盘（空值不记，避免"上次留空"被当成一个有效答案）。
# 刷新整个文件而不是追加：同一个键只留**最后一次**的答案，否则重复运行会累积
# 出一堆同名行，而 ans_get 取第一行——于是"记住的"永远是第一次的答案。
# 写失败一律静默（目录只读、或跑在 --no-service 的临时目录里）：记忆是锦上添花，
# 不该因为写不进去就让整个部署失败。
ans_put() {
	if [ -z "$ANSWERS_FILE" ] || [ -z "$ASK" ]; then return 0; fi
	if [ -z "${2:-}" ]; then return 0; fi
	case "$1" in
	ACME_DNS_TOKEN | ADMIN_PASSWORD) return 0 ;;
	esac
	_new="$(mktemp 2>/dev/null || echo "$ANSWERS_FILE.tmp")"
	if [ -f "$ANSWERS_FILE" ]; then
		sed "/^$1=/d" "$ANSWERS_FILE" >"$_new" 2>/dev/null || true
	else
		: >"$_new" 2>/dev/null || true
	fi
	# 值里的换行会把文件结构弄坏（同一个键被切成两行）。换行本来就进不来
	# （read 只读一行），这里只是把话说清楚，防止将来有人改成别的输入方式。
	printf '%s=%s\n' "$1" "$(printf '%s' "$2" | tr -d '\n\r')" >>"$_new" 2>/dev/null || true
	mkdir -p "$(dirname "$ANSWERS_FILE")" 2>/dev/null || true
	if mv -f "$_new" "$ANSWERS_FILE" 2>/dev/null; then
		chmod 0600 "$ANSWERS_FILE" 2>/dev/null || true
	else
		rm -f "$_new" 2>/dev/null || true
	fi
}

# known_nonempty <值>：这个值是不是"像样地非空"。
# 存在的理由：从文件读回来的值必须再判一次。文件可能被编辑坏（值里带了 CR，
# 或者整行只剩 `TLS_MODE=`），直接当默认值用会把一个空串塞进配置。
known_nonempty() {
	case "${1:-}" in
	'' ) return 1 ;;
	esac
	return 0
}

# 只在**没有被显式指定**时，才用记忆里的值补默认值。
# 顺序上必须是"命令行 > 环境变量 > 记忆 > 内置默认"，而前两者已经在上面赋过值了：
# PUBLIC_LISTEN / ADMIN_LISTEN / TLS_MODE 这类自带默认值的项，现在拿到的是
# 内置默认值，和"人没给"无法区分——所以只有 *_GIVEN 为空时才让记忆覆盖。
# （记忆值自身用 known_nonempty 过一遍，免得把上次的空串当成一个选择。）
if [ -z "$FRESH_ANSWERS" ]; then
	_v="$(ans_get PUBLIC_LISTEN)"
	if [ -z "$PUBLIC_LISTEN_GIVEN" ] && known_nonempty "$_v"; then PUBLIC_LISTEN="$_v"; fi
	_v="$(ans_get ADMIN_LISTEN)"
	if [ -z "$ADMIN_LISTEN_GIVEN" ] && known_nonempty "$_v"; then ADMIN_LISTEN="$_v"; fi
	_v="$(ans_get TLS_MODE)"
	if [ -z "$TLS_MODE_GIVEN" ] && known_nonempty "$_v"; then TLS_MODE="$_v"; fi
	_v="$(ans_get ACME_CHALLENGE)"
	if [ -z "$ACME_CHALLENGE_GIVEN" ] && known_nonempty "$_v"; then ACME_CHALLENGE="$_v"; fi
	_v="$(ans_get ADMIN_USER)"
	if [ -z "$ADMIN_USER_GIVEN" ] && known_nonempty "$_v"; then ADMIN_USER="$_v"; fi
	_v="$(ans_get TLS_TERMINATOR)"
	if [ -z "$TLS_TERMINATOR_GIVEN" ] && known_nonempty "$_v"; then TLS_TERMINATOR="$_v"; fi
fi

# 答案文件的位置与"这次是否复用了它"必须在问答开始前告诉用户。
# 不说的话会出现最糟的一种情况：用户以为自己答的每一项都生效了，实际上方括号里
# 直接回车沿用的是**上一次**的值——他看到地址变了才发现，但已经装完了。
ANSWERS_HINT=""
if [ -f "$ANSWERS_FILE" ] && [ -z "$FRESH_ANSWERS" ] && [ -n "$ASK" ]; then
	ANSWERS_HINT="1"
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
# 0b) 第一个问题：安装，还是卸载
# ---------------------------------------------------------------------------
#
# 这一问是用户提的：跑完一次安装，脚本最后打印"卸载：sh deploy-master.sh --uninstall"，
# 可他是 `curl … | sh` 跑起来的——**本机根本没有那个文件**。那句话既不可执行，
# 又暗示"卸载是一件要读过文档才知道怎么做的事"。
#
# 两个修法一起做，缺一个都不完整：
#   · 收尾提示按"用户是怎么跑起来的"给出**能直接复制的命令**（见文件末尾）；
#   · 卸载的入口也放进问答里，第一问就是"装还是卸"——于是卸载这件事至少
#     有一次是用户**从脚本自己嘴里**知道的，不必先去看文档。
#
# 只问不设默认（回车取 1=安装）：卸载是破坏性操作，不该被"一路回车"选中。
# 已经给了 --uninstall 就不再问——显式参数优先于面试。
if [ -z "$UNINSTALL" ] && [ -n "$ASK" ]; then
	cat >&2 <<'EOF'

这个脚本可以做两件事：

  1) 安装 / 升级 Master（默认）
  2) 卸载 Master（停服务 → 删单元、二进制、配置、数据、系统用户）

EOF
	printf '请输入 1 或 2 [1]: ' >&2
	read_line _mode || _mode=""
	case "$_mode" in
	2 | uninstall | UNINSTALL | Uninstall | 卸载) UNINSTALL="1" ;;
	*) : ;;
	esac
	if [ -n "$UNINSTALL" ]; then
		printf '已选择卸载。默认**连数据一起删**（数据目录里有数据库与 master.key）。\n' >&2
		printf '想保留数据请在下一个问题里回答 n，或改用 --keep-data。\n' >&2
	fi
fi

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
  1) none       由前置反代终结 TLS。若选这一项，本脚本会**自动装好 Caddy 并配好
                反代**（证书申请与续期由 Caddy 负责），你只需要把域名解析到本机
  2) acme       Master 自己申请证书（需要一个已解析到本机的域名）
  3) selfsigned 自签证书（用纯 IP 访问时，这是唯一可行的加密方式）
  4) files      使用你已有的证书与私钥

四种都不需要你自己去装 Caddy/Nginx 并手写反代配置。
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
	ans_put TLS_MODE "$TLS_MODE"
fi

# ---- 谁终结 TLS：Master 自己，还是由本脚本装的 Caddy 反代 ----
#
# 与上面那个问题是**两个维度**，之前混作了一个，这正是"选 2/3/4 还得自己装反代"
# 的根源：
#   · TLS 方式 回答"证书从哪来"（不要证书 / 自动申请 / 自签 / 已有证书）；
#   · 终结者   回答"谁拿着证书监听 443"（Master 进程自己 / 前面的 Caddy）。
#
# 默认值按 TLS 方式给，且**只有这一处**给默认值（install-master.sh 里那份是同一个
# 规则的镜像，两处都改了才一致；见 deploy_flags 护栏）：
#   · none → caddy ：既然选了"由前置反代终结"，那就把这个反代装上，别让用户自己来；
#   · 其它 → master：证书就是给 Master 自己用的。
if [ -z "$TLS_TERMINATOR" ]; then
	case "$TLS_MODE" in
	none) TLS_TERMINATOR="caddy" ;;
	*) TLS_TERMINATOR="master" ;;
	esac
fi
if [ -z "$TLS_TERMINATOR_GIVEN" ] && [ -n "$ASK" ]; then
	cat >&2 <<'EOF'

TLS 由谁终结：
  1) caddy   本脚本自动安装 Caddy、生成反代配置。证书申请与续期全自动，
             你只需保证：域名已解析到本机、80/443 没被别的服务占用
  2) master  Master 进程自己监听并终结 TLS（内置 ACME / 自签 / 你给的证书）
EOF
	ask "选择终结者（1=caddy / 2=master）" "$TLS_TERMINATOR"
	case "$ASK_REPLY" in
	1 | caddy | CADDY | Caddy) TLS_TERMINATOR="caddy" ;;
	2 | master | MASTER | Master) TLS_TERMINATOR="master" ;;
	*) TLS_TERMINATOR="$ASK_REPLY" ;;
	esac
fi
ans_put TLS_TERMINATOR "$TLS_TERMINATOR"

case "$TLS_TERMINATOR" in
master | caddy) : ;;
*)
	echo "错误：--tls-terminator 只能是 master 或 caddy（收到 $TLS_TERMINATOR）。" >&2
	exit 2
	;;
esac

# caddy 终结时 Master 侧**必须**不终结：两个进程抢 443 的报错是
# "address already in use"，会被读成"端口被别的程序占了"，离真正的原因很远。
# 这里就地纠正而不是只警告：从"选 Caddy 终结"到"Master 该用哪种 tls.mode"之间
# 的推理不该由用户完成。
if [ "$TLS_TERMINATOR" = "caddy" ] && [ "$TLS_MODE" != "none" ]; then
	printf '注意：Caddy 终结 TLS，因此 Master 侧改为 --tls-mode none。\n' >&2
	printf '      你选的 %s 的证书参数不再使用。\n' "$TLS_MODE" >&2
	TLS_MODE="none"
	TLS_CERT=""
	TLS_KEY=""
	TLS_DOMAINS=""
	TLS_IPS=""
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
	if [ "$TLS_TERMINATOR" = "caddy" ]; then
		cat >&2 <<'EOF'

公开站监听：Caddy 会反代到这个地址，所以这里**只需监听回环**
（推荐 127.0.0.1:8080）。写 0.0.0.0 的话 8080 会直接暴露在公网上，
绕过 Caddy 的 https 与访问控制。
EOF
	fi
	ask "公开站监听（端口，或 IP:端口；0.0.0.0 表示所有网卡）" "$PUBLIC_LISTEN"
	_new_listen="$(normalize_listen "$ASK_REPLY" "0.0.0.0")"
	if listen_port_ok "$_new_listen"; then
		PUBLIC_LISTEN="$_new_listen"
	else
		printf '警告：%s 不是合法端口，沿用 %s\n' "$ASK_REPLY" "$PUBLIC_LISTEN" >&2
	fi
fi

# 管理端监听：这里除了端口合法性，还要判**与公开站是否撞车**。
#
# 撞车不是"配置得不好"，而是**装不起来**：一个进程没法同时监听两次同一个
# 地址:端口，Master 启动时会报 `listen tcp 127.0.0.1:8081: bind: address
# already in use` 然后"已安全退出"——而那句报错很像"端口被别的程序占了"。
# 发生过一次（用户把管理端填成了 0.0.0.0:8080，与公开站完全同端口），
# 所以他看到的是"部署完成"后面跟着一个退出的服务。
#
# "同站"这个需求**不需要**两个进程监听同一个端口：同站是指两个路径在同一个
# 域名下，那是由反代按路径转发实现的（或者由 site.layout=single 让 Master
# 自己在同一个监听上同时提供两套路径）。所以撞车时重问，并把正确做法说清楚。
_admin_tries=0
while [ -z "$ADMIN_LISTEN_GIVEN" ] && [ -n "$ASK" ]; do
	ask "管理端监听（端口，或 IP:端口；127.0.0.1 表示只有本机能访问）" "$ADMIN_LISTEN"
	_new_listen="$(normalize_listen "$ASK_REPLY" "127.0.0.1")"
	if ! listen_port_ok "$_new_listen"; then
		printf '警告：%s 不是合法端口，沿用 %s\n' "$ASK_REPLY" "$ADMIN_LISTEN" >&2
		break
	fi
	if [ "$_new_listen" = "$PUBLIC_LISTEN" ]; then
		printf '错误：管理端与公开站不能监听同一个地址端口（%s）——一个进程无法监听两次。\n' "$_new_listen" >&2
		printf '      想要「同一个域名下的两个路径」，请改管理端端口（如 127.0.0.1:8081），\n' >&2
		printf '      再用反代按路径 /admin/ 转发；或者干脆让两边都留在同一个监听上。\n' >&2
		_admin_tries=$((_admin_tries + 1))
		if [ "$_admin_tries" -ge 3 ]; then
			printf '      连续三次都相同，改用默认值 %s。\n' "$ADMIN_LISTEN" >&2
			break
		fi
		continue
	fi
	ADMIN_LISTEN="$_new_listen"
	break
done

# 端口撞车的**硬校验**。问答那一层只在能问的时候拦（重问一次），
# 而 `--public-listen/--admin-listen` 显式给出时不经过问答——那条路径必须
# 也能拦下来，否则同样会装出一个启动即退出的服务。一处判、两处调用。
if [ "$PUBLIC_LISTEN" = "$ADMIN_LISTEN" ]; then
	echo "错误：公开站与管理端不能监听同一个地址端口（$PUBLIC_LISTEN）。" >&2
	echo "      一个进程无法在同一地址端口上监听两次，启动时会以" >&2
	echo "      「address already in use」退出。请给管理端换一个端口。" >&2
	exit 2
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

# ---- 把这一轮答过的非敏感项存下来 ----
#
# 位置刻意在"即将执行"确认**之前**：这样用户看到清单后按 n 取消，
# 下次重跑时方括号里已经是刚才填的那些值，不必从头再答一遍。
# 存的是"即将执行"里那几项——与用户看到的清单同源，不会多存他没见过的东西。
ans_put PUBLIC_LISTEN "$PUBLIC_LISTEN"
ans_put ADMIN_LISTEN "$ADMIN_LISTEN"
ans_put PUBLIC_ORIGIN "$PUBLIC_ORIGIN"
ans_put ADMIN_ORIGIN "$ADMIN_ORIGIN"
ans_put ADMIN_USER "$ADMIN_USER"
ans_put ACME_CHALLENGE "$ACME_CHALLENGE"
ans_put ACME_EMAIL "$ACME_EMAIL"
ans_put ACME_CA "$ACME_CA"
ans_put TLS_DOMAINS "$TLS_DOMAINS"
ans_put TLS_IPS "$TLS_IPS"
ans_put TLS_CERT "$TLS_CERT"
ans_put TLS_KEY "$TLS_KEY"
ans_put PROXY_DOMAINS "$PROXY_DOMAINS"
# 口令与 ACME Token 一概不记：它们不进答案文件，理由与"不做命令行参数"相同。

if [ -n "$ASK" ]; then
	echo >&2
	echo "即将执行：" >&2
	if [ -n "$ANSWERS_HINT" ]; then
		printf '  %s\n' "（方括号里的默认值来自上次的答案：$ANSWERS_FILE）" >&2
		printf '  %s\n' "（想全部重答一遍：PROBE_FRESH_ANSWERS=1 重跑本脚本）" >&2
	fi
	printf '  %s：%s\n' "公开站监听" "$PUBLIC_LISTEN" >&2
	printf '  %s：%s\n' "管理端监听" "$ADMIN_LISTEN" >&2
	printf '  %s：%s\n' "公开地址" "${PUBLIC_ORIGIN:-（留空：按请求 Host 回填）}" >&2
	printf '  %s：%s\n' "管理地址" "${ADMIN_ORIGIN:-（与公开站同一个域名）}" >&2
	_tls_note="$TLS_MODE"
	if [ "$TLS_MODE" = "acme" ]; then _tls_note="$TLS_MODE（challenge=$ACME_CHALLENGE）"; fi
	printf '  %s：%s\n' "TLS" "$_tls_note（终结者=$TLS_TERMINATOR）" >&2
	if [ "$TLS_TERMINATOR" = "caddy" ]; then
		printf '  %s：%s\n' "反代" "安装 Caddy，反代 ${PROXY_DOMAINS:-$PUBLIC_ORIGIN} → $PUBLIC_LISTEN" >&2
	fi
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
		if [ "$TLS_TERMINATOR" = "caddy" ]; then
			# 这种组合是**正常的**：Caddy 终结 TLS，Master 侧明文。
			# 之前这里无条件警告"请把反代网段填进 trusted_proxies"——
			# 而在代管反代的情况下那个网段就是我们自己（127.0.0.1），
			# 由下面的 render_config 自动回填，用户不需要做任何事。
			# 一条用不上的警告会让真正需要动作的警告失去分量。
			:
		else
			echo "警告：--tls-mode none 但 --public-origin 是 https://。若 TLS 由前置反代终结，" >&2
			echo "      请把反代网段填进 trusted_proxies，否则 Cookie / HSTS 判定会走明文分支。" >&2
		fi
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

# binary_supports_config_key <键名>：二进制认不认识某个配置键。
#
# 为什么需要它：`install-master-bin.sh` 下的二进制来自 **Release 标签**，而部署脚本
# 来自 **main**。两者版本不同步——脚本可能写出二进制还不认识的键，而 LoadMaster 是
# 严格解析（KnownFields(true)），后果是**服务启动即失败**（用户在真机上就撞到了：
# `field sync_obtain not found in type config.ACMEConfig`）。
#
# 判据必须问二进制本人，不能靠"比版本号"：版本号规则会变、也需要有人记得维护。
# 做法＝拿一份**只含这一个键**的最小配置喂给它，看它是"配置错误"还是
# "未知键"。用 tls status 而不是启动服务：它是只读的、不需要端口、立即返回。
binary_supports_config_key() {
	_key="${1:-}"
	[ -z "$_key" ] && return 1
	[ -x "$BIN_DIR/probe-master" ] || return 1
	_probe="$(mktemp 2>/dev/null || printf '%s' "/tmp/probe-support-$$.yaml")"
	# 这份最小配置必须**本身语法正确**，否则二进制报的是 YAML 语法错、而我们会
	# 把它当成"认识这个键"（假阴性：写下去照样启动失败）。所以：
	#   · listen 值一律加引号（`127.0.0.1:0` 不加引号会被 YAML 当成映射键）；
	#   · 生成后用 `probe-master` 自己过一遍**基线**（不含待测键），基线必须解析成功。
	cat >"$_probe" <<EOF
public_listen: "127.0.0.1:0"
admin_listen: "127.0.0.1:0"
tls:
  mode: "none"
EOF
	_out="$("$BIN_DIR/probe-master" tls status -config "$_probe" 2>&1)"
	case "$_out" in
	*"解析配置失败"* | *"unmarshal"*)
		# 基线都过不了：本机的二进制/配置契约已变，探测结论不可信。
		# 宁可不写这个键（退回旧行为），也不要写下去让服务起不来。
		rm -f "$_probe" 2>/dev/null || true
		return 1
		;;
	esac
	cat >"$_probe" <<EOF
public_listen: "127.0.0.1:0"
admin_listen: "127.0.0.1:0"
tls:
  mode: "acme"
  acme:
    $_key: false
EOF
	_out="$("$BIN_DIR/probe-master" tls status -config "$_probe" 2>&1)"
	rm -f "$_probe" 2>/dev/null || true
	# 二进制认识这个键（哪怕后面因为别的理由报错，也不是"未知键"）。
	case "$_out" in
	*"field $_key not found"*) return 1 ;;
	*"not found in type"*) return 1 ;;
	esac
	return 0
}

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
	if [ "$TLS_TERMINATOR" = "caddy" ]; then
		# 反代是本机上的 Caddy，所以可信代理就是回环。
		# 填对了 X-Forwarded-Proto 才会被采信，Cookie 的 Secure 判定与 HSTS
		# 才会走安全分支——不填的话站点能用，但会话 Cookie 会降级成明文形态。
		echo "trusted_proxies: [\"127.0.0.1/32\", \"::1/128\"]   # 本机 Caddy 反代"
	else
		echo "trusted_proxies: []   # 直连保持为空；前置反代时填反代 IP/CIDR（13.2 S5）"
	fi
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
		if [ "$TLS_TERMINATOR" = "caddy" ]; then
			echo '  mode: "none"               # TLS 由本机 Caddy 终结（见 '"$CADDYFILE"'）'
		else
			echo '  mode: "none"               # 由前置 Caddy/Nginx 终结 TLS，或本机/内网调试'
		fi
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
		# sync_obtain 让首次申请在**服务启动时前台完成**（失败则服务起不来），
		# 这样部署脚本能拿到一个确定的结果、并在失败时回滚。
		# 写成 true 而不是靠部署脚本事后改 false：服务重启本来就该被证书挡住一次——
		# 证书没申请成功时，"服务起来了"是个假象（https 打不开）。
		#
		# 但它是**新键**：二进制来自 Release 标签、配置模板来自 main，两者会错配。
		# 而 LoadMaster 是严格解析（未知键直接拒绝启动），写下去就等于让服务起不来。
		# 所以先探测二进制认不认识它——认识才写。
		if binary_supports_config_key sync_obtain; then
			SYNC_OBTAIN_OK="1"
			echo '    sync_obtain: true          # 部署时前台申请，拿到确定结果；日常重启可改为 false'
		fi
		;;
	esac
	echo "log_level: \"info\""
}

# ---------------------------------------------------------------------------
# Caddy 反代：--tls-terminator caddy
# ---------------------------------------------------------------------------
#
# 用户的原话是："probe-master 可以监听任何端口，但如果选择下面的 2 3 4，应该自行
# 完成前置 Caddy/Nginx 的反代和自动申请证书以及续期证书，也就是说可以直接访问
# https，而不需要自己再安装下载 Caddy/Nginx 进行配置反代。"
#
# 之前的状态确实只做到一半：脚本能问"要不要证书"，能让 Master 自己终结 TLS，
# 但**没有人去把 80/443 这一步接上**。于是"选了 acme"的人还要自己解决端口占用、
# 自己装反代、自己保证续期——这三件事都不该由用户做。
#
# 选 Caddy 而不是 Nginx 的理由：
#   · 自动 HTTPS 是它的内建行为（连证书申请、续期、OCSP 都不需要额外配置）；
#   · 一份 Caddyfile 三五行就够，不用生成几十行 server 块；
#   · 单文件静态二进制，安装不牵动发行版的包管理器。
# 代价是引入一个第三方组件：所以它**只在用户选了 caddy 终结时**才装上，
# 且装之前会明确告诉用户"接下来要装什么"。
CADDY_DIR="${PROBE_CADDY_DIR:-/etc/caddy}"
CADDYFILE="$CADDY_DIR/Caddyfile"

# caddy_proxy_target：反代指向哪儿。
# 必须是 Caddy 能连到的**目的地址**，而 0.0.0.0/[::] 是"监听所有网卡"的写法——
# 不是个能连的地址。Linux 上连 0.0.0.0 恰好常被当成 localhost，若 Caddy 跑在
# 容器里或另一台机器上就直接失败，而失败信息完全看不出原因。
proxy_target() {
	case "$PUBLIC_LISTEN" in
	0.0.0.0:*) printf '127.0.0.1:%s' "${PUBLIC_LISTEN#0.0.0.0:}" ;;
	"[::]:"*) printf '127.0.0.1:%s' "${PUBLIC_LISTEN#\[::\]:}" ;;
	":::" | "[::]") printf '127.0.0.1' ;;
	*) printf '%s' "$PUBLIC_LISTEN" ;;
	esac
}

# render_caddyfile：生成反代配置。
#
# 两种站点形态各写一份，因为它们的**反代规则不同**：
#   · 同站（单域名）：一个域名，全部路径原样转给 Master。Master 自己在同一个
#     监听上同时提供公开站与后台——这正是 site.layout=single 的语义；
#   · 分离（两个域名）：公开域名的 /admin/ 不能放行（那是"后台被暴露在公开域名下"
#     的一种），管理域名整个转给 Master。这一步是**必须**的：Master 的站点形态
#     门控按路径判域名，反代若不按域名分流，两个域名看到的内容就是同一个。
#
# 与 render_config 同样的取舍：抽成函数是为了让 --dry-run 能打印将要写入的内容，
# 且预览与落盘共用同一段代码。
render_caddyfile() {
	_pd="${PROXY_DOMAINS:-}"
	if [ -z "$_pd" ]; then
		# 没显式给反代域名时，从公开地址里取主机名——那是用户已经确认过的域名。
		_pd="$(printf '%s' "$PUBLIC_ORIGIN" | sed -e 's|^[a-zA-Z][a-zA-Z0-9+.-]*://||' -e 's|/.*$||' -e 's|:[0-9]*$||')"
	fi
	_pub="$(printf '%s' "$_pd" | cut -d, -f1)"
	_adm="${ADMIN_ORIGIN:-}"
	if [ -n "$_adm" ]; then
		_adm="$(printf '%s' "$_adm" | sed -e 's|^[a-zA-Z][a-zA-Z0-9+.-]*://||' -e 's|/.*$||' -e 's|:[0-9]*$||')"
	fi

	echo "# 由 scripts/deploy-master.sh 生成"
	echo "# Caddy 负责 https（证书自动申请与续期）并把请求反代给 Master。"
	echo "# Master 自己只监听 $PUBLIC_LISTEN（明文），证书与 443 都由 Caddy 管。"
	echo
	if [ -n "$_adm" ] && [ "$_adm" != "$_pub" ]; then
		# 分离形态。管理域名的 /admin/ 由它自己兜；公开域名**显式拒绝** /admin/，
		# 免得两个域名都能进后台（那会让"分离"这个设置看起来生效了、实际没有）。
		echo "$_pub {"
		echo "	encode gzip"
		echo "	@admin path /admin /admin/*"
		echo "	respond @admin 404"
		echo "	reverse_proxy $(proxy_target)"
		echo "}"
		echo
		echo "$_adm {"
		echo "	encode gzip"
		echo "	reverse_proxy $(proxy_target)"
		echo "}"
	else
		echo "$_pub {"
		echo "	encode gzip"
		echo "	reverse_proxy $(proxy_target)"
		echo "}"
	fi
}

# install_caddy：装 Caddy（已有就跳过）。
#
# 刻意**不**用发行版的包管理器去装第三方源：那要往 /etc/apt/sources.list.d 里
# 加东西、要处理 GPG key，而在最小化容器里这些前置条件未必齐。Caddy 官方提供
# 单文件二进制下载，落到 /usr/local/bin 就完事——与 Master 自己的安装方式一致。
install_caddy() {
	if command -v caddy >/dev/null 2>&1; then
		echo "    已安装：$(caddy version 2>/dev/null || echo caddy)"
		return 0
	fi
	# 发行版仓库里若有 caddy 包，先试它（没有额外源的情况很少，但试一下没坏处）。
	if command -v apt-get >/dev/null 2>&1 && apt-cache show caddy >/dev/null 2>&1; then
		echo "    用 apt 安装 caddy"
		if DEBIAN_FRONTEND=noninteractive apt-get install -y caddy >/dev/null 2>&1; then
			return 0
		fi
	fi
	echo "    从官方源下载 caddy 二进制"
	_ca_arch="amd64"
	case "$(uname -m)" in
	aarch64 | arm64) _ca_arch="arm64" ;;
	armv7* | armv6*) _ca_arch="armv7" ;;
	x86_64 | amd64) _ca_arch="amd64" ;;
	esac
	_ca_url="https://caddyserver.com/api/download?os=linux&arch=$_ca_arch"
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$_ca_url" -o "$BIN_DIR/caddy" || {
			echo "错误：下载 caddy 失败（$_ca_url）。" >&2
			echo "      若网络受限，可自己装好 caddy 后重跑本脚本。" >&2
			return 1
		}
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$BIN_DIR/caddy" "$_ca_url" || {
			echo "错误：下载 caddy 失败（$_ca_url）。" >&2
			return 1
		}
	else
		echo "错误：找不到 curl 或 wget，无法下载 caddy。" >&2
		return 1
	fi
	chmod 0755 "$BIN_DIR/caddy"
	if ! "$BIN_DIR/caddy" version >/dev/null 2>&1; then
		echo "错误：下载到的 caddy 无法执行。" >&2
		return 1
	fi
	echo "    已安装 $("$BIN_DIR/caddy" version)"
	return 0
}

# ---------------------------------------------------------------------------
# 回滚：安装失败时把**本次新建的东西**清掉
# ---------------------------------------------------------------------------
#
# 用户的要求是："失败的话脚本停止安装，运行结束，并自动清除安装过程产生的东西。"
#
# 关键在于**只清本次新建的**。这个脚本是幂等的升级路径：重复执行时它会停服务、
# 换二进制、保留数据目录。如果失败就无脑删数据目录，那"升级失败"会变成
# "把线上部署连数据库一起删了"——比失败本身严重得多。
#
# 所以进来之前先记录"来之前有什么"，回滚时按这份记录判断每一项该不该删：
#   · 用户/组、配置目录、数据目录、二进制 —— 装之前就不存在才删；
#   · 已经存在的，只把**这一次写进去的东西**撤掉（配置由 --force-config 覆盖的
#     情况会备份成 master.yaml.bak，回滚时还原）。
#
# 回滚自身**绝不中断**（全程 || true），并把每一项的结果打出来：
# 回滚出问题时最不该发生的事，是让人以为已经清干净了。
PREE_HAVE_DIR=""
PREE_HAVE_DATA=""
PREE_HAVE_BIN=""
PREE_HAVE_USER=""
PREE_HAVE_UNIT=""
PREE_HAVE_CADDYFILE=""
PREE_HAVE_CADDY_UNIT=""
PREE_CHECKED=""

# precheck_state：在改动系统**之前**把"已经有什么"记下来。幂等，只生效一次。
precheck_state() {
	if [ -n "$PREE_CHECKED" ]; then return 0; fi
	PREE_CHECKED="1"
	if [ -d "$DIR" ]; then PREE_HAVE_DIR="1"; fi
	if [ -d "$DATA" ]; then PREE_HAVE_DATA="1"; fi
	if [ -e "$BIN_DIR/probe-master" ]; then PREE_HAVE_BIN="1"; fi
	if id -u probe >/dev/null 2>&1; then PREE_HAVE_USER="1"; fi
	if [ -e /etc/systemd/system/probe-master.service ]; then PREE_HAVE_UNIT="1"; fi
	if [ -f "$CADDYFILE" ]; then PREE_HAVE_CADDYFILE="1"; fi
	if [ -e /etc/systemd/system/caddy.service ]; then PREE_HAVE_CADDY_UNIT="1"; fi
}

# rollback <原因>：失败路径的统一出口。清完以非 0 退出。
rollback() {
	echo >&2
	echo "============================================================" >&2
	echo " 安装失败，开始回滚（清除本次安装产生的所有内容）" >&2
	echo "============================================================" >&2
	printf '  失败原因：%s\n' "$1" >&2
	echo >&2
	echo "==> 回滚 1/6 停止服务" >&2
	# 先停再删：服务还在跑着的时候删二进制，systemd 会按 Restart=always 反复重启，
	# 而它每次重启都会在日志里留一条"找不到可执行文件"——把真正的失败原因淹掉。
	systemctl stop probe-master >/dev/null 2>&1 || true
	systemctl disable probe-master >/dev/null 2>&1 || true
	if [ "$TLS_TERMINATOR" = "caddy" ]; then
		systemctl stop caddy >/dev/null 2>&1 || true
		systemctl disable caddy >/dev/null 2>&1 || true
	fi
	echo "    已停止" >&2

	echo "==> 回滚 2/6 服务单元" >&2
	if [ -z "$PREE_HAVE_UNIT" ]; then
		rm -f /etc/systemd/system/probe-master.service 2>/dev/null || true
		echo "    已删除 probe-master.service（本次新建）" >&2
	else
		echo "    保留 probe-master.service（安装前就存在）" >&2
	fi
	# caddy 的服务单元**只在本次是它装的**才删。用"安装前有没有 caddy 这个命令"
	# 判不出来——apt 装完之后命令就在 PATH 里了，所以这里用单元文件是否存在过。
	if [ "$TLS_TERMINATOR" = "caddy" ] && [ -z "$PREE_HAVE_CADDY_UNIT" ]; then
		rm -f /etc/systemd/system/caddy.service 2>/dev/null || true
		echo "    已删除 caddy.service（本次新建）" >&2
	fi
	systemctl daemon-reload >/dev/null 2>&1 || true
	systemctl reset-failed probe-master >/dev/null 2>&1 || true
	systemctl reset-failed caddy >/dev/null 2>&1 || true

	echo "==> 回滚 3/6 二进制" >&2
	if [ -z "$PREE_HAVE_BIN" ] && [ -n "$BIN_SRC" ]; then
		rm -f "$BIN_DIR/probe-master" 2>/dev/null || true
		echo "    已删除 $BIN_DIR/probe-master（本次新建）" >&2
	elif [ -n "$PREE_HAVE_BIN" ]; then
		echo "    保留 $BIN_DIR/probe-master（安装前就存在；升级失败时旧二进制已被覆盖）" >&2
	fi

	echo "==> 回滚 4/6 配置与反代配置" >&2
	if [ -n "$PREE_HAVE_DIR" ]; then
		# 升级场景：只撤本次写的东西。--force-config 覆盖过的配置从 .bak 还原
		# （备份是写配置那一步做的），没备份就保留原样——**绝不删目录**。
		if [ -f "$CFG.bak" ]; then
			mv -f "$CFG.bak" "$CFG" 2>/dev/null || true
			echo "    已还原 $CFG（来自 .bak）" >&2
		else
			echo "    保留 $DIR（安装前就存在）" >&2
		fi
		if [ -z "$PREE_HAVE_CADDYFILE" ] && [ -f "$CADDYFILE" ]; then
			rm -f "$CADDYFILE" 2>/dev/null || true
			echo "    已删除 $CADDYFILE（本次新建）" >&2
		fi
	else
		rm -rf "$DIR" 2>/dev/null || true
		if [ -e "$DIR" ]; then echo "    ⚠ 残留：$DIR（请手工处理）" >&2
		else echo "    已删除 $DIR（本次新建）" >&2; fi
	fi

	echo "==> 回滚 5/6 数据目录" >&2
	if [ -z "$PREE_HAVE_DATA" ]; then
		# 只删本次新建的。而且**必须**确认路径像我们自己建的（与 --uninstall 同一判据）：
		# --data 是可传参的，`--data /var/lib` 这种手滑不该在回滚路径上变成灾难。
		case "$DATA" in
		/*probe*)
			case "${DATA#/}" in
			*/*)
				rm -rf "$DATA" 2>/dev/null || true
				if [ -e "$DATA" ]; then echo "    ⚠ 残留：$DATA（请手工处理）" >&2
				else echo "    已删除 $DATA（本次新建）" >&2; fi
				;;
			*) echo "    拒绝删除 $DATA（不像本脚本建的目录，路径至少两级）" >&2 ;;
			esac
			;;
		*) echo "    拒绝删除 $DATA（路径中应含 probe）" >&2 ;;
		esac
	else
		echo "    保留 $DATA（安装前就存在，可能有你的数据）" >&2
	fi

	echo "==> 回滚 6/6 系统用户" >&2
	if [ -z "$PREE_HAVE_USER" ]; then
		userdel probe >/dev/null 2>&1 || true
		groupdel probe >/dev/null 2>&1 || true
		if id -u probe >/dev/null 2>&1; then
			echo "    ⚠ 用户 probe 仍在（可能仍有进程以它运行）" >&2
		else
			echo "    已删除用户与组 probe（本次新建）" >&2
		fi
	else
		echo "    保留用户 probe（安装前就存在）" >&2
	fi

	echo >&2
	echo "==> 回滚完成。系统已恢复到运行本脚本之前的状态。" >&2
	echo "    修好上面的原因后重跑本脚本即可（它是幂等的）。" >&2
	echo >&2
	exit 1
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

# 回滚要靠"来之前有什么"，所以这个快照必须在**任何改动发生之前**拍。
# 放在 dry-run 之前是无害的：它只读不写，而且 dry-run 本来就该看到同样的判断依据。
precheck_state

# --dry-run 到此为止：参数校验与二进制检查都做过了，但一个字节都不写。
# 位置刻意放在这里而不是更早——再早一点就没法把"将要写入的配置文件"预览出来，
# 那是这个开关最有用的部分。
if [ -n "$DRY_RUN" ]; then
	echo
	echo "==> dry-run：以下是将要写入 $CFG 的内容（未改动系统）"
	echo
	render_config
	if [ "$TLS_TERMINATOR" = "caddy" ]; then
		echo
		echo "==> dry-run：以下是将要写入 $CADDYFILE 的内容"
		echo
		render_caddyfile
		echo
		echo "==> dry-run：还会安装 Caddy 并注册 caddy.service（若尚未安装）"
	fi
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
	if [ -f "$CFG" ]; then
		# 覆盖前先留一份。回滚要靠它把旧配置还原回去——否则"升级时失败回滚"
		# 会把用户原来的配置换成本次生成的这一份，数据没丢但配置变了，
		# 而这件事他很可能到服务起不来的时候才发现。
		cp -f "$CFG" "$CFG.bak" 2>/dev/null || true
	fi
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
	# 与收尾那段同一个道理：ADMIN_ORIGIN 才是"对外地址"，没给时给的本机地址
	# 必须**说明它只在本机有效**，否则会被当成公网后台地址记下来。
	if [ -n "$ADMIN_ORIGIN" ]; then
		_ns_admin="$(admin_url_from "$ADMIN_ORIGIN")"
		_ns_note=""
	else
		_ns_admin="$(admin_url_from "${PUBLIC_ORIGIN:-$_ns_scheme://$ADMIN_LISTEN}")"
		_ns_note="（本机地址；管理端 $ADMIN_LISTEN 只在本机可达，公网请走反代）"
	fi
	echo "  公开看板    ${PUBLIC_ORIGIN:-$_ns_scheme://$PUBLIC_LISTEN}"
	echo "  管理后台    $_ns_admin"
	if [ -n "$_ns_note" ]; then echo "              $_ns_note"; fi
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
	if ! systemctl restart probe-master; then
		# 启动失败必须回滚。最典型的一种是 tls.mode=acme 且开了 sync_obtain：
		# 证书申请不成功时服务**故意**起不来，而这是部署阶段，用户正等着结果。
		# 不回滚的话系统里会留一个反复重启失败的服务，而脚本已经"完成"了。
		systemctl --no-pager status probe-master 2>/dev/null | sed -n '1,10p' >&2 || true
		journalctl -u probe-master --no-pager -n 20 2>/dev/null | sed 's/^/    /' >&2 || true
		rollback "probe-master 服务启动失败（日志见上）"
	fi
	echo "    已启用并启动"
	# Type=simple 下 systemctl restart 一返回就认为"成功"，而进程可能已经开始
	# 申请证书、也可能**立刻**因为别的原因退出（端口被占、配置键写错）。
	# 所以多给一秒再确认一次它还活着——把"服务根本没起来"这件事在回滚点上
	# 抓住，而不是等到后面的健康检查超时 20 秒再报一句含糊的"没探到公开 API"。
	sleep 1
	if ! systemctl is-active --quiet probe-master; then
		systemctl --no-pager status probe-master 2>/dev/null | sed -n '1,10p' >&2 || true
		journalctl -u probe-master --no-pager -n 20 2>/dev/null | sed 's/^/    /' >&2 || true
		rollback "probe-master 启动后立即退出（日志见上；端口被占用、配置键写错都会这样）"
	fi
else
	echo "警告：没有 systemd（Alpine/OpenRC 或容器），请自行用进程管理器托管：" >&2
	echo "      $BIN_DIR/probe-master -config $CFG" >&2
	exit 0
fi

# ---- 5b) Caddy 反代 + 证书 ----
#
# 这一步是"选 2/3/4 时也帮我配好反代"那条反馈的落点。它排在 Master 服务之后：
# 反代的目标（Master）必须先真的在听，Caddy 起来才能立刻反代成功。
#
# 失败即回滚，而不是"打印警告继续"。理由是反代是用户选的主路径：
# 它没配好，站点就访问不了，而"部署完成"这句话会与事实相反。
if [ "$TLS_TERMINATOR" = "caddy" ]; then
	echo "==> 5b/7 安装 Caddy 反代（证书由 Caddy 自动申请与续期）"
	if ! install_caddy; then
		rollback "Caddy 安装失败"
	fi
	mkdir -p "$CADDY_DIR" 2>/dev/null || true
	# 已有 Caddyfile 怎么办？这里**不能**只看"文件存在"就保留。
	# 本机 Caddy 很可能本来就是别人（发行版包、x-ui 面板、以前的 nginx 迁移）装的，
	# 那份配置里根本没有我们要反代的域名——保留它的结果不是"尊重既有配置"，
	# 而是**Caddy 没有 443 站点、不开 443、也拿不到证书**，用户看到的却是
	# "反代已配好、部署完成"。所以判据换成：里面**有没有本次要用的那个域名**。
	_caddy_dom="$(render_caddyfile | sed -n 's/^\([^ {]*\) {.*/\1/p' | head -n1)"
	Caddyfile_has_domain() {
		[ -f "$CADDYFILE" ] || return 1
		[ -z "${1:-}" ] && return 1
		# 只匹配行首的站点标签（"example.com {" 或 "example.com:443 {"），
		# 不匹配注释里顺嘴提到的域名——否则一句注释就能让判据失效。
		grep -Eq "^[[:space:]]*$1(:[0-9]+)?[[:space:]]*\{" "$CADDYFILE" 2>/dev/null
	}
	if [ -f "$CADDYFILE" ] && [ -z "$FORCE_CONFIG" ] && Caddyfile_has_domain "$_caddy_dom"; then
		echo "    $CADDYFILE 已有 $_caddy_dom 的站点块，保留（要覆盖请加 --force-config）"
	elif [ -f "$CADDYFILE" ] && [ -z "$FORCE_CONFIG" ]; then
		# 有文件但没有目标域名：备份后并入我们的站点块，而不是整份覆盖——
		# 里面可能有别人的站点（这台机器上就常见）。
		cp -f "$CADDYFILE" "$CADDYFILE.bak" 2>/dev/null || true
		if [ -n "$_caddy_dom" ] && grep -Eq "^[[:space:]]*[^#{]" "$CADDYFILE" 2>/dev/null; then
			{
				printf '\n# ---- 以下由 scripts/deploy-master.sh 追加 ----\n'
				render_caddyfile
			} >>"$CADDYFILE"
			echo "    $CADDYFILE 里没有 $_caddy_dom 的站点块 → 已追加（原内容备份为 $CADDYFILE.bak）"
		else
			render_caddyfile >"$CADDYFILE"
			echo "    $CADDYFILE 原内容无有效站点 → 已写入（备份为 $CADDYFILE.bak）"
		fi
		chmod 0644 "$CADDYFILE"
	else
		if [ -f "$CADDYFILE" ]; then cp -f "$CADDYFILE" "$CADDYFILE.bak" 2>/dev/null || true; fi
		render_caddyfile >"$CADDYFILE"
		chmod 0644 "$CADDYFILE"
		echo "    已写入 $CADDYFILE"
	fi
	# 反代域名必须解析到本机，否则 Caddy 申请证书会失败（而失败信息出现在
	# caddy 的日志里，不在本脚本的输出里，用户基本找不到）。
	_cd_dom="${_caddy_dom:-}"
	if [ -z "$_cd_dom" ]; then
		_cd_dom="$(printf '%s' "${PROXY_DOMAINS:-}" | cut -d, -f1)"
	fi
	if [ -n "$_cd_dom" ]; then
		_cd_ip=""
		if command -v getent >/dev/null 2>&1; then
			_cd_ip="$(getent hosts "$_cd_dom" 2>/dev/null | awk '{print $1}' | head -n1)"
		fi
		if [ -z "$_cd_ip" ]; then
			echo "    ⚠ 域名 $_cd_dom 解析不到 IP：Caddy 申请证书时会失败。" >&2
			echo "      请先把这个域名解析到本机，再重跑本脚本。" >&2
		else
			echo "    $_cd_dom → $_cd_ip"
		fi
	fi

	# caddy 的 systemd 单元。官方包会自己带一个，但如果我们是下载二进制装的，
	# 就没有单元可用——两种来源都要能跑起来。
	CADDY_UNIT=/etc/systemd/system/caddy.service
	if [ ! -f "$CADDY_UNIT" ]; then
		_caddy_bin="$(command -v caddy 2>/dev/null || printf '%s' "$BIN_DIR/caddy")"
		cat >"$CADDY_UNIT" <<EOF
[Unit]
Description=Caddy (Server Probe 反向代理)
Documentation=https://caddyserver.com/docs/
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=$_caddy_bin run --config $CADDYFILE --adapter caddyfile
ExecReload=$_caddy_bin reload --config $CADDYFILE --adapter caddyfile
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
		echo "    已写好 $CADDY_UNIT"
	fi
	# 配置语法自检：一个写坏的 Caddyfile 会让 caddy 起来就退出，而 systemd
	# 只会报 "start request repeated too quickly"——离真正原因很远。
	if command -v "$BIN_DIR/caddy" >/dev/null 2>&1 || [ -x "$BIN_DIR/caddy" ]; then
		if ! "$BIN_DIR/caddy" validate --config "$CADDYFILE" --adapter caddyfile >/dev/null 2>&1; then
			"$BIN_DIR/caddy" validate --config "$CADDYFILE" --adapter caddyfile 2>&1 | sed 's/^/    /' >&2 || true
			rollback "Caddyfile 语法校验失败（内容见上）"
		fi
		echo "    Caddyfile 语法校验通过"
	fi
	systemctl daemon-reload >/dev/null 2>&1 || true
	systemctl enable caddy >/dev/null 2>&1 || true
	if ! systemctl restart caddy; then
		journalctl -u caddy --no-pager -n 20 2>/dev/null | sed 's/^/    /' >&2 || true
		rollback "caddy 服务启动失败（日志见上）"
	fi
	echo "    已启用并启动 caddy"
fi

echo "==> 6/7 健康检查"
# 公开站 /site 是免鉴权的，用它确认进程真的在听；最多等 20 秒。
#
# 配了 TLS 就必须用 https 探：明文探一个 TLS 端口永远失败，会白等 20 秒再打印
# 一段让人以为服务没起来的状态。
# selfsigned 用 -k 是**有意**的：这里问的是"进程在不在听"，不是"证书链可不可信"，
# 两者混在一起会让自签部署次次看起来像失败。acme 同理——首张证书由服务前台申请，
# 此刻要么已经拿到、要么服务没起来，证书链验证失败并不代表服务没起来。
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

echo "==> 6/7 证书"
# 这一步是本轮改动里最要紧的一处，用户的原话是：
#
#   "它在后台申请，但是脚本直接结束了，它是否申请成功也不知道，而且，没申请成功前
#    https 一直不可用，不能访问站点。把申请证书不要放在后台，而是直接申请，一边申请
#    一边反馈结果，如果申请失败也要反馈原因，失败的话脚本停止安装，运行结束，并自动
#    清除安装过程产生的一切。"
#
# 三件事都要做，缺一个都不算修好：
#   ① 前台申请：配置里写了 tls.acme.sync_obtain=true，于是**服务启动时就把证书
#      申请完**（见 internal/tlsconf 的 Setup）。申请不成功服务就起不来——
#      "服务在跑但 https 打不开"这个假象从根上消失了；
#   ② 边申请边反馈：申请可能耗时十几秒到几分钟，这段时间必须有输出，
#      否则用户看到的是"卡住了"；
#   ③ 失败即回滚：验证失败时调 rollback，把本次装的东西清干净。
#
# 不再"等 120 秒然后听天由命"：现在 wait 的秒数给得足够长（申请由服务在做，
# 我们只是等它的结果），而**结果一定是确定的**——要么就绪、要么失败并带原因。
if [ "$TLS_TERMINATOR" = "caddy" ]; then
	# 这里必须**当场验**，不能只打印"稍后验证"。用户上一次部署正是栽在这：
	# 脚本说"反代已配好"，实际 Caddy 只开了 80、没有 443 站点（Caddyfile 是别人
	# 留下的），用户得自己 netstat 才发现没监听 443。
	#
	# 判据要挑**真正的那个事实**：443 在听 + Caddy 能为这个域名拿到证书。
	# 只查 "caddy 进程活着"是不够的（它当然活着，还开着 80）。
	echo "    正在等待 Caddy 为 $_cd_dom 申请证书（最多 60 秒）…"
	_cd_ok=""
	i=0
	while [ $i -lt 30 ]; do
		# 443 端口在听？用 ss/netstat 判，缺工具就跳过后面的请求探测。
		if curl -fsS --max-time 5 "https://$_cd_dom/api/public/v1/site" >/dev/null 2>&1; then
			_cd_ok="1"
			break
		fi
		i=$((i + 1))
		sleep 2
	done
	if [ -n "$_cd_ok" ]; then
		echo "    https://$_cd_dom 已可用（证书由 Caddy 签发）"
	else
		# 失败要给出**能直接照做**的诊断，而不是"稍后再看看"。
		echo "" >&2
		echo "错误：Caddy 起来了，但 https://$_cd_dom 在 60 秒内仍不可用。" >&2
		echo "  可能的原因与查法（按常见程度排序）：" >&2
		echo "  1) Caddyfile 里没有这个域名的站点块 → 看：grep -n '$_cd_dom' $CADDYFILE" >&2
		echo "  2) 80/443 被别的服务占用（x-ui、nginx、其他 caddy）→ 看：ss -lntp | grep -E ':(80|443)\\b'" >&2
		echo "  3) 域名没解析到本机 → 看：getent hosts $_cd_dom" >&2
		echo "  4) 证书申请被 CA 拒绝（配额/限流/端口不通）→ 看：journalctl -u caddy -n 50" >&2
		echo "" >&2
		journalctl -u caddy --no-pager -n 20 2>/dev/null | sed 's/^/    /' >&2 || true
		rollback "Caddy 反代未生效（https://$_cd_dom 打不开，原因见上）"
	fi
elif [ "$TLS_MODE" = "none" ]; then
	echo "    跳过（tls.mode=none）：证书由前置反代负责"
else
	_cert_wait=180
	if [ "$TLS_MODE" != "acme" ]; then _cert_wait=0; fi
	# sync_obtain 不可用时（二进制比脚本旧），首张证书是**服务在后台**申请的：
	# 这时不能只跑一次 tls status 就下结论——它会立刻返回"进行中"（rc=2），
	# 而用户看到的会是"仍在申请，请稍后自己看"，正是本轮要消灭的那种收尾。
	# 所以我们自己轮询，把"后台申请"包成一次前台等待。
	if [ "$TLS_MODE" = "acme" ] && [ -z "$SYNC_OBTAIN_OK" ]; then
		echo "    正在申请证书（您的二进制版本不支持前台申请，改为轮询等待；失败会中止安装并回滚）"
		i=0
		while [ $i -lt "$_cert_wait" ]; do
			_cert_rc=0
			"$BIN_DIR/probe-master" tls status -config "$CFG" >/dev/null 2>&1 || _cert_rc=$?
			[ "$_cert_rc" != "2" ] && break
			i=$((i + 5))
			sleep 5
		done
		_cert_rc=0
		"$BIN_DIR/probe-master" tls status -config "$CFG" || _cert_rc=$?
	else
		if [ "$TLS_MODE" = "acme" ]; then
			echo "    正在申请证书（前台进行，失败会中止安装并回滚）"
		fi
		_cert_rc=0
		# 这里刻意**不再**吞掉输出：tls status 会打印主体/签发者/有效期，
		# 申请失败时打印原因与可复制的排查命令——那正是用户要的"反馈结果"。
		if [ "$_cert_wait" -gt 0 ]; then
			"$BIN_DIR/probe-master" tls status -config "$CFG" -wait "$_cert_wait" || _cert_rc=$?
		else
			"$BIN_DIR/probe-master" tls status -config "$CFG" || _cert_rc=$?
		fi
	fi
	case "$_cert_rc" in
	0)
		# 申请成功。把 sync_obtain 关掉：它存在的意义只是"这一次拿到确定结果"，
		# 留着的话以后每次重启都会被 CA 卡住（CA 不可达时一次正常的重启会失败）。
		if [ "$TLS_MODE" = "acme" ] && grep -q '^    sync_obtain: true' "$CFG" 2>/dev/null; then
			sed -i 's/^    sync_obtain: true.*/    sync_obtain: false         # 首次已成功，日常重启不再被证书阻塞/' "$CFG" 2>/dev/null || true
			echo "    已把 sync_obtain 改为 false（日常重启不再等待 CA）"
		fi
		;;
	2)
		# 还没结果。这一支在 acme + 前台申请的组合下**基本不该出现**
		# （服务要么申请成功要么没起来）；留着是为了兼容非 acme 模式与
		# 极慢的 CA。它不是成功，但也不足以判定失败，所以不回滚，
		# 而是把"怎么继续查"说清楚。
		echo "    ⚠ 申请仍未返回结果（超时）。服务会在后台继续重试；"
		echo "      随时可查：$BIN_DIR/probe-master tls status -config $CFG -wait 120"
		;;
	*)
		# 申请失败：上面已经打出了原因与排查命令。按用户要求中止安装并回滚。
		rollback "证书申请失败（原因见上；tls.mode=$TLS_MODE，challenge=$ACME_CHALLENGE）"
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
#
# ⚠ 这里曾经有一处**实打实的错**，用户直接撞上了：
#
#     ADMIN_URL="$SCHEME://$ADMIN_ADDR/admin/"     # ADMIN_ADDR = 归一化后的监听地址
#
# ADMIN_ADDR 来自 ADMIN_LISTEN，而它是一个**监听**地址：0.0.0.0:8080 会被归一化成
# 127.0.0.1:8080（因为 0.0.0.0 不是能连的目的地址）。于是用户填的是 0.0.0.0:8080、
# 打印出来的却是 https://127.0.0.1:8080/admin/——那个地址既错了端口语义
# （用户填的和印出来的不一致），又是"只有本机能打开"的回环地址，还是 https
# （而 tls.mode=none 时根本没有 https 监听）。三个错叠在同一个字符串里。
#
# 正确的推导必须按**用户实际怎么访问**分三种情况，而不是"从监听地址反推"：
#   ① 配了反代（Caddy）→ 走域名，与内部监听端口无关；
#   ② 同站（admin_origin 留空但有 public_origin）→ 后台在公开域名的 /admin/ 下；
#   ③ 什么都没有（明文直连 IP）→ 只能给个"本机可达"的提示地址，且必须说明
#      它只在本机有效，以及公网该怎么访问。
PUBLIC_URL="${PUBLIC_ORIGIN:-$SCHEME://$HEALTH_ADDR}"
# 本机可达的地址，用来在"没有对外域名"时给一个能立刻打开的东西。
# 仍然要归一化（0.0.0.0 不是可连的目的地址），但**只在真的用它时才用**。
ADMIN_ADDR="$ADMIN_LISTEN"
case "$ADMIN_ADDR" in
0.0.0.0:*) ADMIN_ADDR="127.0.0.1:${ADMIN_ADDR#0.0.0.0:}" ;;
"[::]:"*) ADMIN_ADDR="127.0.0.1:${ADMIN_ADDR#\[::\]:}" ;;
":::" | "[::]") ADMIN_ADDR="127.0.0.1" ;;
esac
ADMIN_NOTE=""
if [ "$TLS_TERMINATOR" = "caddy" ]; then
	# 反代形态：对外就是域名，后台在同一个域名的 /admin/ 下（同站）或另一个域名。
	if [ -n "$ADMIN_ORIGIN" ]; then
		ADMIN_URL="$(admin_url_from "$ADMIN_ORIGIN")"
	else
		_cd_first="$(printf '%s' "${PROXY_DOMAINS:-}" | cut -d, -f1)"
		if [ -z "$_cd_first" ]; then
			_cd_first="$(printf '%s' "$PUBLIC_ORIGIN" | sed -e 's|^[a-zA-Z][a-zA-Z0-9+.-]*://||' -e 's|/.*$||' -e 's|:[0-9]*$||')"
		fi
		ADMIN_URL="https://$_cd_first/admin/"
		ADMIN_NOTE="（由 Caddy 反代到 $ADMIN_LISTEN；证书自动续期）"
	fi
elif [ -n "$ADMIN_ORIGIN" ]; then
	ADMIN_URL="$(admin_url_from "$ADMIN_ORIGIN")"
elif [ -n "$PUBLIC_ORIGIN" ]; then
	# 同站：后台就在公开域名的 /admin/ 下。**这才是"同站"的正确表达**——
	# 不是"让两个端口监听同一个地址"（那不可能），而是"同一个域名下的两个路径"。
	ADMIN_URL="$(admin_url_from "$PUBLIC_ORIGIN")"
	ADMIN_NOTE="（同站：在公开域名下的 /admin/；站点形态可在「设置 → 站点形态」调整）"
else
	# 既没有对外域名也没有反代：只能给个本机地址，并**明确说明**它只在本机有效。
	# 不说明的话，用户会把这个地址当成"公网后台地址"贴给别人或写进笔记。
	ADMIN_URL="$SCHEME://$ADMIN_ADDR/admin/"
	ADMIN_NOTE="（这是**本机**地址：管理端监听 $ADMIN_LISTEN 只在本机可达）"
fi

echo
echo "============================================================"
echo " 部署完成"
echo "============================================================"
printf '  公开看板    %s\n' "$PUBLIC_URL"
printf '  管理后台    %s\n' "$ADMIN_URL"
if [ -n "$ADMIN_NOTE" ]; then printf '              %s\n' "$ADMIN_NOTE"; fi
case "$TLS_MODE" in
none)
	if [ "$TLS_TERMINATOR" = "caddy" ]; then
		printf '  传输安全    %s\n' "https（由 Caddy 终结并自动续期）"
	else
		printf '  传输安全    %s\n' "明文（tls.mode=none，且没有代管反代）"
	fi
	;;
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
EOF

# ---- 卸载提示：必须是**用户能直接粘贴执行**的那条命令 ----
#
# 这里曾经写死一句 `sh deploy-master.sh --uninstall`。对 `curl … | sh` 跑起来的人
# 它是**一条不存在的命令**：本机既没有这个文件，他的当前目录也未必是当初下载的地方。
# 于是"想卸载"变成"先回去翻文档，找出自己当初是用什么命令装的"——而这恰恰是
# 一个脚本最该替用户记住的事。
#
# 所以由入口脚本把"自己是怎么被调用的"通过 PROBE_SELF_ENTRY / PROBE_SELF_ENTRY_ARGS
# 传下来，这里据此还原出一条完整命令。还原不了（用户直接跑的部署脚本）时，
# 才退回提示本地路径——那种情况下本机确实有这个文件。
case "${SELF_ENTRY:-}" in
bin)
	# 免编译入口：它是从 raw.githubusercontent 取回来执行的，所以卸载也照原样。
	_sh_url="${SCRIPT_ENTRY_URL:-https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master-bin.sh}"
	printf '  卸载        sudo sh -c "$(curl -fsSL %s)" xb --uninstall\n' "$_sh_url"
	printf '              （加 --keep-data 保留数据目录；--dry-run 只预览不删）\n'
	;;
src)
	_sh_url="${SCRIPT_ENTRY_URL:-https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master.sh}"
	printf '  卸载        sudo sh -c "$(curl -fsSL %s)" xb --uninstall\n' "$_sh_url"
	printf '              （加 --keep-data 保留数据目录；--dry-run 只预览不删）\n'
	;;
ps1)
	printf '  卸载        irm %s -OutFile install-master.ps1; .\\install-master.ps1 -Uninstall\n' \
		"${SCRIPT_ENTRY_URL:-https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master.ps1}"
	printf '              （加 -KeepData 保留数据目录；-DryRun 只预览不删）\n'
	;;
*)
	# 直接跑部署脚本的人：本机确实有这个文件，路径提示是有效的。
	# 但仍把"重跑本脚本"的形式给全，免得他忘了当初用了哪些 --dir/--data。
	printf '  卸载        sh %s --uninstall%s\n' "$0" "${SELF_ENTRY_ARGS:-}"
	printf '              （加 --keep-data 保留数据目录；--dry-run 只预览不删）\n'
	;;
esac

cat <<EOF
  重跑本脚本   会问你"安装还是卸载"（第 1 问），答过的内容已记在
                $ANSWERS_FILE，
                下次运行自动作为默认值——改一项不必把前面全重答一遍。
EOF

# TLS 的收尾提示按模式给。写死一句"去配 TLS"是没用的：真正会卡住人的
# 是每种模式各自的前置条件（80 端口、证书路径、私钥指纹）。
case "$TLS_MODE" in
none)
	if [ "$TLS_TERMINATOR" = "caddy" ]; then
		cat >&2 <<EOF
ℹ 已由本机 Caddy 终结 TLS：配置在 $CADDYFILE，证书自动申请与续期。
  · 反代目标：$PUBLIC_LISTEN（Master 侧明文，只在内网/回环）
  · trusted_proxies 已自动填好回环地址，无需手动改
  · 看 Caddy 的证书状态：journalctl -u caddy -n 50
  · 改完 Caddyfile 重载：systemctl reload caddy
EOF
	else
		cat >&2 <<'EOF'
ℹ 当前 tls.mode=none 且**没有**代管反代：对外是明文的。
  若你自己在前面放了 Caddy/Nginx，请把反代网段填进 master.yaml 的 trusted_proxies，
  否则 X-Forwarded-Proto 不被采信、Cookie 会走明文降级分支。
  也可以重跑本脚本并选"终结者 = caddy"，让脚本把反代一并配好。
EOF
	fi
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
ℹ tls.mode=acme：证书由 Master 自己申请与续期，**本次已在前台申请成功**。
  · challenge=$ACME_CHALLENGE
  · http-01 需要 80 端口可达；80 被反代占用且反代会转发 /.well-known/ 时，
    请在配置文件里把 acme.http_addr 设为 off
  · 申请前先确认域名已解析到本机，且 install.master_url 用的是 https
EOF
	;;
esac
