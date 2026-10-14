#!/bin/sh
# XB-Probe —— Master 一键部署（Linux / systemd）
#
# 覆盖三件事：放二进制 → 写配置并初始化管理员 → 注册 systemd 服务并做健康检查。
# 幂等：重复执行会走"升级"路径（停服务 → 换二进制 → 启动），不会重建数据目录、
# 也不会覆盖已有的 master.yaml（除非 --force-config）。
#
# 用法（root）：
#   sh deploy-master.sh --bin ./bin/xb-probe-master \
#       --public-origin https://status.example.com \
#       --admin-origin  https://admin.example.com \
#       --admin-user ops --admin-password '<强口令>'
#
# 不传 --admin-password 时会交互式提示输入（推荐：口令不进 shell 历史）。
# 也可以先 export PROBE_ADMIN_PASSWORD=... 再执行。
#
# 交互问答：**参数没给全时会问你**——对外访问方式（要不要 https、证书谁办）、
# 公开站/管理端的访问地址、两边各监听哪个端口、管理员用户名，紧接着是口令，
# 最后把将要做的事列一遍让你确认（账号口令在确认之前就都问完了）。
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
# 第一个问题是"做什么"（没显式给 --uninstall / --release-only 时）：安装或升级、
# 卸载、只更新 Agent 发布文件。之所以问在最前面：
# 卸载的入口不该只有"读过文档才知道怎么打"的那一条命令，而收尾提示里那句
# `sh deploy-master.sh --uninstall` 对 `curl … | sh` 跑起来的人根本不存在——
# 他的机器上**没有这个文件**。见文件末尾"卸载提示怎么给"那一段。
#
# "只更新 Agent 发布文件"是同一类问题：GitHub 上出了新的 Agent 版本时，原先要
# 自己去 Release 页看版本号、再手敲一条 `xb-probe-master dist pull --version 0.1.2
# --from .../download/v0.1.2`（版本号敲错就是 404）。放进这一问之后，
# "把新节点装到的 Agent 升到最新"＝重跑脚本 → 选 3。
#
# 只看不装（打印将要写入的 master.yaml，不建用户/目录、不注册服务、不建管理员）：
#   PROBE_NO_ASK=1 sh deploy-master.sh --bin ./bin/xb-probe-master --dry-run
#
# 常用可选参数：
#   --dir <目录>          配置目录（默认 /etc/xb-probe-master）
#   --data <目录>         数据目录（默认 /var/lib/xb-probe-master）
#   --bin-dir <目录>      二进制安装目录（默认 /usr/local/bin；只读根文件系统用 /opt/xb-probe-master）
#   --public-listen <地址> 公开站监听（默认 0.0.0.0:8080；只写端口如 9000 等价于 0.0.0.0:9000）
#   --admin-listen  <地址> 管理端监听（留空 = 与公开站共用同一个监听，后台在公开
#                          地址的 /admin/ 下——"Master 自己终结 TLS"时的默认值；
#                          填地址则独立监听，默认 127.0.0.1:8081 只监听回环，
#                          那种形态只适合反代在前，或只用 SSH 隧道访问后台）
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
# 版本指纹：让"我到底跑的是哪一版脚本"变成一个可核对的事实
# ---------------------------------------------------------------------------
#
# 起因是一次很难查的故障：用户机器上取到的部署脚本是**旧版**（raw CDN 缓存了
# `main`），于是脚本里已经修好的逻辑根本没生效，而仓库、CI、我们这边的核对
# 全都显示"已经修好了"。用户看到的只有同一个错误反复出现。
#
# 光靠"输出一个哈希"不够——用户没有对照物。所以这里做三件事：
#   ① 打印本脚本的 sha256 前 8 位（有对照物的人一眼能认出跑的是不是同一份）；
#   ② 打印脚本里那几处**关键修复**的标记（用 grep 探，探不到就明确说"这份脚本
#      里没有某某修复"）——这才是可执行的诊断，不必去做哈希对比；
#   ③ 通过入口脚本进来时（PROBE_SELF_ENTRY=bin/src），顺带说明脚本与二进制
#      可能来自不同的 ref。
script_self_check() {
	_sha=""
	if command -v sha256sum >/dev/null 2>&1; then
		_sha="$(sha256sum "$0" 2>/dev/null | awk '{print $1}')"
	elif command -v shasum >/dev/null 2>&1; then
		_sha="$(shasum -a 256 "$0" 2>/dev/null | awk '{print $1}')"
	fi
	if [ -n "$_sha" ]; then
		printf '部署脚本    scripts/deploy-master.sh sha256=%s…\n' "$(printf '%s' "$_sha" | cut -c1-8)" >&2
	fi
	# 每一项都是"某个已修的缺陷"的**唯一标记**。取的是那段代码里最不会因为
	# 重构而消失的一个串（函数名、报错文案），而不是行号——行号会漂。
	_missing=""
	for _mark in binary_supports_config_key Caddyfile_has_domain CADDY_MARK PLAIN_HTTP; do
		if ! grep -q "$_mark" "$0" 2>/dev/null; then
			_missing="$_missing $_mark"
		fi
	done
	if [ -n "$_missing" ]; then
		printf '  ⚠ 本脚本缺少以下修复标记：%s\n' "$_missing" >&2
		printf '    → 你拿到的是旧版脚本（很可能是 CDN 缓存）。请重跑安装命令，\n' >&2
		printf '      或改用带随机串的地址强制回源：\n' >&2
		printf '      curl -fsSL "https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master-bin.sh?t=$(date +%%s)" -o imb.sh\n' >&2
	fi
	if [ -n "${PROBE_SELF_ENTRY:-}" ]; then
		printf '  入口 %s（部署脚本来自 --script-ref，二进制来自 Release 标签，两者可能不同）\n' "$PROBE_SELF_ENTRY" >&2
	fi
}

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

# ---------------------------------------------------------------------------
# 输出着色：步骤头（青）、成功（绿）、警告（黄）、错误（红）
# ---------------------------------------------------------------------------
#
# 颜色码本身先算好，**用不用由每个 helper 在调用时按输出流现判**（_color_ok）：
#   · 管道 / `> 日志 2>&1` / CI / 测试抓取 → 该流不是终端，一个 ESC 都不落，
#     断言不会因为颜色码莫名失败，日志里也不会出现一团 ^[36m；
#   · 命令替换（`$(...)`）会把 fd 换成管道——启动时判一次的话，颜色码会被
#     写进调用方正要解析的内容里（--ask-mode 的答案就是这么取的），所以必须现判；
#   · `say … >&2` 这类重定向也由此自动判对（函数里看到的 fd 1 已是重定向后的）。
# 另外尊重 NO_COLOR 与 TERM=dumb。
#
# 交互题的提示**刻意不着色**：它们由 readline 打印（`read -e -r -p`），ANSI 序列
# 会被当成可打印宽度参与行编辑计算，退格到行首重绘时光标算错——那正是 read_line
# 上面那段注释里踩过的坑。所以颜色只上在**整行输出**上（步骤头、清单、警告、
# 错误、收尾汇总），提问行保持原样。
C_STEP="$(printf '\033[1;36m')"
C_OK="$(printf '\033[1;32m')"
C_WARN="$(printf '\033[1;33m')"
C_ERR="$(printf '\033[1;31m')"
C_DIM="$(printf '\033[2m')"
C_BLD="$(printf '\033[1m')"
C_RST="$(printf '\033[0m')"

# _color_ok <fd>：这个流当下是不是终端、且用户没关颜色。
_color_ok() {
	[ -t "$1" ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-}" != "dumb"
}

# _clr <fd> <颜色码>：这个流是终端才返回颜色码，否则返回空串。
# 给"一大块输出只判一次"用：`_lb="$(_clr 2 "$C_BLD")"` 之后整块复用。
# 注意判的是**传进去的那个 fd**：命令替换只会把 fd 1 换成管道，fd 2 原样继承，
# 所以 `"$(_clr 2 …)"` 在 stderr 仍是终端时照样拿得到颜色。
_clr() {
	if _color_ok "$1"; then printf '%s' "$2"; fi
}

# say <内容>：步骤头（青）。与原先的 `echo "==> …"` 同文同流（stdout）。
say() {
	if _color_ok 1; then printf '%s==> %s%s\n' "$C_STEP" "$*" "$C_RST"
	else printf '==> %s\n' "$*"; fi
}
# ok <内容>：成功行（绿）。
ok() {
	if _color_ok 1; then printf '%s%s%s\n' "$C_OK" "$*" "$C_RST"
	else printf '%s\n' "$*"; fi
}
# warn <内容>：警告（黄，stderr）。
warn() {
	if _color_ok 2; then printf '%s警告：%s%s\n' "$C_WARN" "$*" "$C_RST" >&2
	else printf '警告：%s\n' "$*" >&2; fi
}
# err <内容>：错误（红，stderr）。
err() {
	if _color_ok 2; then printf '%s错误：%s%s\n' "$C_ERR" "$*" "$C_RST" >&2
	else printf '错误：%s\n' "$*" >&2; fi
}

BIN_SRC=""
DIR="${PROBE_DIR:-/etc/xb-probe-master}"
DATA="${PROBE_DATA:-/var/lib/xb-probe-master}"
BIN_DIR="${PROBE_BIN_DIR:-/usr/local/bin}"
PUBLIC_LISTEN="${PROBE_PUBLIC_LISTEN:-0.0.0.0:8080}"
# 管理端监听。**空串是一个合法的、有意义的值**：与公开站共用同一个监听
# （后台在公开地址的 /admin/ 下，即"同站"）。见 admin_shares_public_listener。
ADMIN_LISTEN="${PROBE_ADMIN_LISTEN:-127.0.0.1:8081}"
# 问答里"共用同一个监听"的说法。用户看得懂、也能照着敲；内部一律归一成空串。
ADMIN_MERGE_LABEL="与公开站相同"
PUBLIC_ORIGIN="${PROBE_PUBLIC_ORIGIN:-}"
ADMIN_ORIGIN="${PROBE_ADMIN_ORIGIN:-}"
DIST_BASE="${PROBE_DIST_BASE:-}"
# 发布文件（Agent 二进制的自举来源）从哪来、拉哪些。见 "6c) 发布文件入库"。
# RELEASE_REPO 是本项目的发布仓库；--dist-source 可以直接给完整 URL 前缀来
# 覆盖它（GitHub 在部分机房不可达，这时指向自建镜像）。
RELEASE_REPO="${PROBE_RELEASE_REPO:-koyoca/XB-Probe}"
DIST_SOURCE="${PROBE_DIST_SOURCE:-}"
DIST_PULL="${PROBE_DIST_PULL:-}"
DIST_PULL_GIVEN=""
NO_DIST_PULL="${PROBE_NO_DIST_PULL:-}"
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
# 只更新 Agent 发布文件（不动 Master 的二进制、配置与服务）——问答题里的第 3 项。
RELEASE_ONLY=""
# 只替换 Master 本体（不碰配置、证书、管理员账号）——问答题里的第 4 项。
UPGRADE_ONLY=""
# 只重置管理员密码（不动二进制与配置）——问答题里的第 5 项。
RESET_PASSWORD=""
# 上面这几个"只做一件事"的开关与 UNINSTALL / RELEASE_ONLY 是同一类：**必须**在这里
# 给出默认值。`set -eu` 下少一个就会在"判断它有没有被设置"的那一行直接 unbound
# variable 退出——而那行恰好在问答题派发处，等于整个交互入口全废（本轮真实踩过）。
# "做什么"这一问由谁问：--ask-mode 只问不做事（入口脚本用），--mode 是入口问完之后
# 把答案传回来（于是不会再问第二遍）。两者都见 0a-3。
ASK_MODE=""
MODE_IN=""
MODE_GIVEN=""
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
# 用户选的是"纯 HTTP"（对外访问方式 5）。归一化后 TLS_MODE 会变成 none，
# 两者在下游大部分分支里行为相同，只有"不许配反代"那条判断需要区分。
PLAIN_HTTP=""
# Caddy 的位置与"我们那段"的标记。
# 定义放在这里（而不是用到它的 1400 行附近）是因为**卸载路径也要用**——
# 卸载段在 TLS 问答之前就执行了，那时后面那些变量还没赋值。之前只有一个
# CADDYFILE 定义、且在卸载段之后，于是卸载只能选择"Caddy 完全不管"。
CADDY_DIR="${PROBE_CADDY_DIR:-/etc/caddy}"
CADDYFILE="$CADDY_DIR/Caddyfile"
# 追加站点块时写在前面的一行注释。卸载时靠它精确定位"哪一段是我们的"，
# 这样用户自己加的站点不会被牵连；反过来，文件里没有这一行就意味着
# "我们没动过它"（或者整份都是我们写的，见下面的判据）。
CADDY_MARK="# ---- 以下由 scripts/deploy-master.sh 追加 ----"
# 我们生成的内容的"头部指纹"。整份文件由我们写时，render_caddyfile 的第一行
# 就是它；追加到别人的文件里时，它出现在 CADDY_MARK 之后。
#
# 为什么需要一个**头部**指纹而不只是 CADDY_MARK：已部署的机器上，Caddyfile
# 往往是上一版脚本**整份生成**的（没有 CADDY_MARK），而升级时必须能认出
# "这是我写的、可以替换"。否则旧站点块会被原样保留——真机上就出现了
# "脚本说部署完成、/admin/ 依然 404"（旧块里没有分流规则）。
CADDY_HEAD="# 由 scripts/deploy-master.sh 生成"
# 我们那一段的结束标记。有了它，"替换我们写的段落"才能既精确又不牵连别人。
CADDY_END="# ---- 以上由 scripts/deploy-master.sh 生成 ----"
# "用户是否显式给过"这几个标记，用来区分"没给"和"给了一个恰好等于默认值的值"。# 缺了它们，像 TLS_MODE / PUBLIC_LISTEN 这种自带默认值的项就永远分不清
# "来自默认值"还是"人来指定过"，结果是该问的没问、不该问的反复问。
PUBLIC_LISTEN_GIVEN=""
ADMIN_LISTEN_GIVEN=""
# 公开监听这个值是不是来自"上次的答案"（见 ans_get PUBLIC_LISTEN 那一段）。
PUBLIC_LISTEN_FROM_ANSWERS=""
ADMIN_LISTEN_FROM_ANSWERS=""
# 被丢弃的旧管理端监听（只绑回环、而这次没有反代）。见"管理端监听"的默认值那一段。
ADMIN_LISTEN_STALE=""
TLS_MODE_GIVEN=""
ADMIN_USER_GIVEN=""
ACME_CHALLENGE_GIVEN=""
if [ -n "${PROBE_PUBLIC_LISTEN:-}" ]; then PUBLIC_LISTEN_GIVEN="1"; fi
if [ -n "${PROBE_ADMIN_LISTEN:-}" ]; then ADMIN_LISTEN_GIVEN="1"; fi
if [ -n "${PROBE_TLS_MODE:-}" ]; then TLS_MODE_GIVEN="1"; fi
if [ -n "${PROBE_ADMIN_USER:-}" ]; then ADMIN_USER_GIVEN="1"; fi
if [ -n "${PROBE_ACME_CHALLENGE:-}" ]; then ACME_CHALLENGE_GIVEN="1"; fi

script_self_check

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
  --bin <路径>              xb-probe-master 二进制（或已安装时省略以只做配置/服务）

推荐：
  --public-origin <URL>     公开站对外地址（6.1 源分离，配了才做 Host 白名单）
  --admin-origin  <URL>     管理端对外地址
  --admin-user <用户名>      管理员用户名（默认 ops）
  --admin-password <口令>    不传则交互输入；也可用环境变量 PROBE_ADMIN_PASSWORD

可选：
  --dir <目录>              配置目录（默认 /etc/xb-probe-master）
  --data <目录>             数据目录（默认 /var/lib/xb-probe-master）
  --bin-dir <目录>          二进制安装目录（默认 /usr/local/bin）
  --public-listen <地址>     默认 0.0.0.0:8080（只写端口如 9000 = 0.0.0.0:9000）
  --admin-listen <地址>      留空 = 与公开站共用同一个监听（后台在公开地址的
                            /admin/ 下）；填地址则独立监听——默认 127.0.0.1:8081
                            只监听回环，那种形态需要反代在前（或只用 SSH 隧道）
  --dist-base <URL>         发布文件镜像前缀
  --dist-pull <all|local|skip>
                            部署完成后把 Agent 发布文件拉进 data_dir/dist/
                            （默认 all；local 只拉本机架构，skip 先不拉）
  --dist-source <URL前缀>    发布文件的来源（默认按二进制版本指向本项目的
                            GitHub Release；GitHub 不可达时给自建镜像）
  --no-dist-pull            等价于 --dist-pull skip
  --no-service              不注册 systemd（容器场景）
  --force-config            覆盖已有配置
  --dry-run                 只打印计划与将要写入的配置
  --yes                     不提问（等价于 PROBE_NO_ASK=1）
  -h, --help                显示本帮助

参数没给全时本脚本会交互提问（TLS 方式、监听端口、公开/管理端地址、管理员）。
不问的情形：PROBE_NO_ASK=1 / 标准输入不是终端 / 该项已显式给出。

TLS：
  --tls-mode <none|files|selfsigned|acme>   默认 none
  --tls-terminator <master|caddy>           谁终结 TLS（**默认由对外访问方式推导**：
                                            选 1 = caddy，选 2/3/4 = master，选 5 为空；
                                            一般不需要显式给）
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

只更新 Agent 发布文件（不动 Master 的二进制、配置与服务）：
  --release-only            去 GitHub 查**最新 Release**，把它的 Agent 发布文件
                            拉进 data_dir/dist/<最新版本>/。
                            版本号由脚本自己查（不必再手敲 --version），也不需要
                            注册码；已装好的节点随后用 `xb-probe-agent upgrade` 升级。
                            与 --uninstall 互斥；配 --dry-run 只查版本不写盘。

只升级 Master 本体（不动配置、证书、站点设置与管理员账号）：
  --upgrade-only            用 --bin 给的新二进制替换已安装的 xb-probe-master，
                            然后停/起服务并当场验活。**不重写 master.yaml**、
                            不申请证书、不动 Caddy、不碰管理员账号、也不重拉
                            Agent 发布文件。
                            与 --uninstall / --release-only / --reset-password 互斥；
                            启动失败会自动把旧二进制换回来（配置从头到尾没动过）。

重置管理员密码：
  --reset-password          改 --admin-user（默认 ops）的登录口令，不动其它任何东西。
                            口令取 --admin-password 或 PROBE_ADMIN_PASSWORD，
                            都没给就交互输入两次。
                            与其它三种"只做一件事"的模式互斥；配 --dry-run 只回显计划。

给入口脚本用的两个（一般不需要手工敲）：
  --ask-mode                只问"做什么"（1 安装/升级、2 卸载、3 只更新发布文件、
                            4 只升级本体、5 重置管理员密码），把答案写到 stdout
                            就退出。不碰系统、不需要 root。
                            install-master-bin.sh / install-master.sh 靠它**在下载
                            二进制、装工具链之前**就知道用户要做什么。
  --mode <1|2|3|4|5>        入口脚本问完之后把答案传回来，于是不会再问第二遍。
                            2 等价于 --uninstall、3 等价于 --release-only、
                            4 等价于 --upgrade-only、5 等价于 --reset-password。
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
	--dist-source) DIST_SOURCE="${2:-}"; shift 2 ;;
	--dist-pull) DIST_PULL="${2:-}"; DIST_PULL_GIVEN="1"; shift 2 ;;
	--no-dist-pull) NO_DIST_PULL="1"; shift ;;
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
	--release-only) RELEASE_ONLY="1"; shift ;;
	--upgrade-only) UPGRADE_ONLY="1"; shift ;;
	--reset-password) RESET_PASSWORD="1"; shift ;;
	--ask-mode) ASK_MODE="1"; shift ;;
	--mode) MODE_IN="${2:-}"; shift 2 ;;
	--keep-data) KEEP_DATA="1"; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "未知参数：$1" >&2; usage; exit 2 ;;
	esac
done

# ---------------------------------------------------------------------------
# 配置探测与诊断 helper（必须定义在任何调用点之前——见下文 0d/0e 的说明）
# ---------------------------------------------------------------------------
# binary_supports_config_key <键名>：二进制认不认识某个配置键。
#
# 为什么需要它：`install-master-bin.sh` 下的二进制来自 **Release 标签**，而部署脚本
# 来自 **main**。两者版本不同步——脚本可能写出二进制还不认识的键，而 LoadMaster 是
# 严格解析（KnownFields(true)），后果是**服务启动即失败**（用户在真机上就撞到了：
# `field sync_obtain not found in type config.ACMEConfig`）。
#
# 判据必须问二进制本人，不能靠"比版本号"：版本号规则会变、也需要有人记得维护。
# 做法＝拿一份**只含这一个键**的最小配置喂给它，看它是"未知键"还是别的错误。
# 用 tls status 而不是启动服务：它是只读的、不需要端口、立即返回。
#
# ⚠⚠ 探测器必须先证明"自己真的读到了我们给它的那份文件"，否则它会给出
# 一个**恒为"认识"的假结论**——这一条是踩过的事故，值得写清楚：
#
#   xb-probe-master tls status -config <临时文件>
#
# 在**没有 tls 子命令**的旧二进制上（Release 的 v0.1.0 就是），`tls` 不匹配
# main() 里任何 case，于是 fall through 到服务启动；而 flag.Parse 遇到第一个
# 非 flag 参数 `tls` 就**停止解析**，`-config <临时文件>` 被整个忽略，读的是
# 默认路径的真实配置。结果是"基线"与"待测"两次调用读的是**同一份**文件：
# 基线通过（因为那份真实配置本来就能解析），待测也没有"未知键"报错 ——
# 于是探测恒回答"认识"。脚本写下二进制不认识的 sync_obtain，服务随即拒绝启动。
#
# 修法是加一个**哨兵**：同样结构，键名换成一个必然不存在的名字。
#   · 哨兵没有被"点名拒绝" → 这条通道根本没测到我们的文件 → 结论不可信 → 返回 1；
#   · 哨兵被点名拒绝、而待测键没有 → 通道有效，且二进制确实认识该键 → 返回 0。
# 哨兵一次覆盖了"子命令不存在"与"-config 被忽略"两种失效——它们的**症状相同**，
# 光看待测键那一次的输出分辨不出来。它同时取代了原来的"基线"：探测配置若自身
# 语法错误，报错会是语法错而不是 `field <哨兵> not found`，哨兵判据同样不匹配。
#
# "无法判断"一律按"不认识"处理：宁可少写一个键（功能降级到旧行为），
# 也不能写下去让服务起不来。
PROBE_SENTINEL_KEY="probe_sentinel_key_that_cannot_exist"

# _probe_write <文件> <键名>：写一份只含该键的最小配置。
# listen 值一律加引号：`127.0.0.1:0` 不加引号会被 YAML 当成映射键，那样二进制
# 报的是**语法错**而不是"未知键"，两种失败在"有没有报错"这个层面看不出区别。
_probe_write() {
	cat >"$1" <<EOF
public_listen: "127.0.0.1:0"
admin_listen: "127.0.0.1:0"
tls:
  mode: "acme"
  acme:
    $2: false
EOF
}

# _probe_channel_ok：这条探测通道**有效吗**——即我们给它的文件真的被读了吗。
# 判据还是哨兵：一份只含哨兵键的最小配置，必须被**点名**拒绝。
_probe_channel_ok() {
	[ -x "$BIN_DIR/xb-probe-master" ] || return 1
	_probe="$(mktemp 2>/dev/null || printf '%s' "/tmp/probe-channel-$$.yaml")"
	_probe_write "$_probe" "$PROBE_SENTINEL_KEY"
	_out="$("$BIN_DIR/xb-probe-master" tls status -config "$_probe" 2>&1)"
	rm -f "$_probe" 2>/dev/null || true
	case "$_out" in
	*"field $PROBE_SENTINEL_KEY not found"*) return 0 ;;
	esac
	return 1
}

binary_supports_config_key() {
	_key="${1:-}"
	[ -z "$_key" ] && return 1
	_probe_channel_ok || return 1
	_probe="$(mktemp 2>/dev/null || printf '%s' "/tmp/probe-support-$$.yaml")"
	_probe_write "$_probe" "$_key"
	_out="$("$BIN_DIR/xb-probe-master" tls status -config "$_probe" 2>&1)"
	rm -f "$_probe" 2>/dev/null || true
	case "$_out" in
	*"field $_key not found"* | *"not found in type"*) return 1 ;;
	esac
	return 0
}

# config_parses_ok <配置文件>：用二进制自己验一遍这份配置能不能被解析。
# 返回 0 = 能；1 = 不能（原始输出留在 _CP_OUT，里面点名了是哪个键）；
#       2 = **判断不了**（通道失效，例如旧二进制没有 tls 子命令）。
#
# 这一层专为"保留旧配置"服务：上一轮写坏的配置会被这一轮原样保留，
# 若不在写完之后立刻验，用户要等输完口令、服务起不来、回滚之后才看到原因。
# "判断不了"时绝不猜——交给第 4 步 init-admin 的输出兜底，那条路径对任何版本都有效。
config_parses_ok() {
	_CP_OUT=""
	[ -f "${1:-}" ] || return 2
	_probe_channel_ok || return 2
	_CP_OUT="$("$BIN_DIR/xb-probe-master" tls status -config "$1" 2>&1)"
	case "$_CP_OUT" in
	*"not found in type"* | *"unmarshal"* | *"解析配置失败"*) return 1 ;;
	esac
	return 0
}

# config_mismatch_report <二进制的原始输出>：报出"配置与二进制不匹配"，**不动系统**。
#
# 拆成"报告"与"回滚"两半，是因为它有两个使用者，而两者该做的事不一样：
#   · 安装流程（第 3 步写完配置 / 第 4 步 init-admin）—— 配置可能刚被本脚本改写，
#     报完必须 rollback 把这一轮改动撤掉；
#   · --upgrade-only —— 此时**一个字节都还没写**，报完直接退出即可。让它去 rollback
#     反而是错的（回滚会去停服务、删刚写的东西，而这里根本没写过）。
# 点名的是哪个键（存在 _CM_BAD_KEY 里，供调用方拼自己的回滚理由）与三种修法两处完全
# 相同，各写一份迟早会有一份忘了更新。
config_mismatch_report() {
	_CM_BAD_KEY="$(printf '%s\n' "${1:-}" | sed -n 's/.*field \([A-Za-z0-9_]*\) not found.*/\1/p' | head -n 1)"
	echo >&2
	err "$CFG 里有当前二进制不认识的键${_CM_BAD_KEY:+（$_CM_BAD_KEY）}。"
	echo >&2
	echo "  这通常不是你填错了，而是**两边版本不一致**：" >&2
	echo "    · 二进制来自 GitHub Release 标签，部署脚本来自 main 分支；" >&2
	echo "    · 脚本按最新的配置格式写文件，而这台机器上的二进制比它旧。" >&2
	echo >&2
	# 修法按"代价从小到大"排：先给只摘掉那一行（保留你其余的设置），
	# 再给整份重写，最后是换成源码安装（两边同源，从根上不再错配）。
	cat >&2 <<EOF
  修法（任选一种）：
    1) 只删掉这一行，然后重跑本脚本：
EOF
	if [ -n "$_CM_BAD_KEY" ]; then
		# 这里**故意不**给 `sed -i`：它换 inode、按 umask 重建权限位，而这份配置里有
		# 证书私钥路径与 ACME Token（0640 root:probe），改完可能变成 0644（凭证泄露）
		# 或 0600 root:root（服务读不到配置，站点起不来）。改成"先写临时文件、
		# 再覆盖回原 inode"：任何一步失败，原文都还完整躺在那里。
		echo "         sed \"/^[[:space:]]*$_CM_BAD_KEY:/d\" $CFG > $CFG.tmp \\" >&2
		echo "           && cat $CFG.tmp > $CFG && rm -f $CFG.tmp" >&2
	else
		printf '         rm -f %s\n' "$CFG" >&2
	fi
	cat >&2 <<EOF
    2) 删掉整份配置重跑（脚本会按当前二进制的能力重写）：
         rm -f $CFG
         sudo sh -c "\$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master-bin.sh)" xb
    3) 改用源码安装（二进制与脚本同源，不会再错配）：
         curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master.sh | sh
EOF
	echo >&2
}



# ---------------------------------------------------------------------------
# 0-0) 由默认值推导出来的路径、平台与模式互斥
# ---------------------------------------------------------------------------
#
# 这几项**上移到问答之前**，是为了让 0b/0c 两个模式分支能用同一份取值：
#   · CFG    —— "只更新发布文件"要读的配置（data_dir 从它里面来）。
#               全文件**只在这里定义一次**：早先它写在 3/7 那一步里，于是任何
#               更早就要用它的分支都只能自己再拼一次 "$DIR/master.yaml"，
#               两份规则迟早漂移（改了一处、另一处还在用旧的）。
#   · _mp_plat —— 平台名（install.sh 的命名一致），问答里要显示、拉取时要传给 --platform。
# 位置在参数解析循环之后：--dir / --bin-dir 都已经生效，取值是最终值。
CFG="$DIR/master.yaml"

# 本机平台（与 install.sh 的命名一致：<os>_<arch>）。
case "$(uname -s 2>/dev/null)" in
Linux) _mp_os="linux" ;;
Darwin) _mp_os="darwin" ;;
FreeBSD) _mp_os="freebsd" ;;
*) _mp_os="" ;;
esac
case "$(uname -m 2>/dev/null)" in
x86_64 | amd64) _mp_arch="amd64" ;;
aarch64 | arm64) _mp_arch="arm64" ;;
armv7l) _mp_arch="armv7" ;;
armv6l) _mp_arch="armv6" ;;
i386 | i686) _mp_arch="386" ;;
riscv64) _mp_arch="riscv64" ;;
*) _mp_arch="" ;;
esac
_mp_plat="${_mp_os}_${_mp_arch}"

# --mode <1|2|3|4|5>：入口脚本已经问过"做什么"了，把答案原样带过来。
# 五个取值与 0a-3 的菜单一一对应。入口脚本用它是为了"不再问第二遍"——
# 不然用户会在入口那里答一次、进了部署脚本又被问一次同一件事。
case "${MODE_IN:-}" in
'') : ;;
1) MODE_GIVEN="1" ;;
2)
	UNINSTALL="1"
	MODE_GIVEN="1"
	;;
3)
	RELEASE_ONLY="1"
	MODE_GIVEN="1"
	;;
4)
	UPGRADE_ONLY="1"
	MODE_GIVEN="1"
	;;
5)
	RESET_PASSWORD="1"
	MODE_GIVEN="1"
	;;
*)
	err "--mode 只能是 1、2、3、4 或 5（收到「$MODE_IN」）。"
	echo "  1 = 安装 / 升级 Master，2 = 卸载 Master，3 = 只更新 Agent 发布文件，" >&2
	echo "  4 = 只升级 Master 本体，5 = 重置管理员密码。" >&2
	exit 2
	;;
esac

# "只做一件事就退出"的四种模式**两两互斥**。同时给就是自相矛盾的要求，直接拒绝
# ——而不是让其中一个悄悄赢：悄悄赢的那种，用户以为做了 A、实际做了 B，
# 而且两条路都不会报错。
#
# 判据写成"数一数有几个"，而不是"两两组合列举"：早先是 `[ -n A ] && [ -n B ]`
# 只盖住 A/B 这一对，第 14 轮加了模式 4/5 之后，`--upgrade-only --uninstall`
# 这类新组合会**静默通过**（4 个标志 → C(4,2)=6 对，写全很啰嗦且漏一对没人发现）。
# 这个判断必须放在 --mode 的映射**之后**：--mode 2/3/4/5 也会把对应的标志置起来，
# 于是 `--mode 2 --release-only` 这种自相矛盾的组合同样会被拦下。
_oneshot_names=""
_oneshot_count=0
_oneshot_note() {
	_oneshot_count=$((_oneshot_count + 1))
	_oneshot_names="$_oneshot_names $1"
}
if [ -n "$UNINSTALL" ]; then _oneshot_note --uninstall; fi
if [ -n "$RELEASE_ONLY" ]; then _oneshot_note --release-only; fi
if [ -n "$UPGRADE_ONLY" ]; then _oneshot_note --upgrade-only; fi
if [ -n "$RESET_PASSWORD" ]; then _oneshot_note --reset-password; fi
if [ "$_oneshot_count" -gt 1 ]; then
	err "这几项都只做一件事，不能同时使用：$_oneshot_names"
	echo "  「卸载」「只更新发布文件」「只升级本体」「重置密码」一次只能选一个。" >&2
	exit 2
fi

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
# 0-1) 清掉 stdin 里不属于用户的残留
# ---------------------------------------------------------------------------
#
# 用户报的现象："有时候交互式输入后，会出现过几秒才提示输入下一个，但是提前回车了，
# 还有就是我并没有输入任何字符却出现无用的字符"。他贴的证据里有一句
# `https://mo.319082.xyzL，不带末尾斜杠）` —— 那个 `L` 不是他敲的。
#
# 那个 `L` 来自启动方式的管道：`curl -fsSL … | sh` 时，curl 的**选项字面量**在某些
# 终端/PTY 组合下会有一部分落进管道残留，而管道那侧正是脚本的 stdin。下面几个
# read 于是"凭空"读到一个字符，把一道题答成它意料之外的内容；紧接着的下一题就被
# 这一轮的错位顶到几秒后才显示。
#
# 判据必须精确：**只在 stdin 其实就是管道/文件（非 tty）时**才清。真 tty 上用户
# 完全可能在脚本启动的瞬间就开始打字，那时清空就等于吃掉他的输入。
# 清法用 `read -t 0`：它只探测"现在能不能立刻读到"，不阻塞、不等待；读到东西就
# 丢掉再试一次，直到缓冲区空。`-n 1` 保证一次只吃一个字符，不会连用户后续的
# 输入一起吞掉。
if [ -n "$ASK" ] && [ ! -t 0 ]; then
	_drain=0
	while [ "$_drain" -lt 64 ] && IFS= read -r -t 0 -n 1 _junk 2>/dev/null; do
		_drain=$((_drain + 1))
	done
	unset _junk 2>/dev/null || true
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
	if [ -z "$PUBLIC_LISTEN_GIVEN" ] && known_nonempty "$_v"; then
		PUBLIC_LISTEN="$_v"
		# 记下来源：公开监听的默认值要随"谁终结 TLS"变（caddy→回环 8080，
		# master→443）。区分"这个值来自记忆"与"还是内置默认"，才不至于把
		# 用户上次答的端口悄悄改掉。
		PUBLIC_LISTEN_FROM_ANSWERS="1"
	fi
	_v="$(ans_get ADMIN_LISTEN)"
	if [ -z "$ADMIN_LISTEN_GIVEN" ] && known_nonempty "$_v"; then
		ADMIN_LISTEN="$_v"
		ADMIN_LISTEN_FROM_ANSWERS="1"
	fi
	_v="$(ans_get TLS_MODE)"
	if [ -z "$TLS_MODE_GIVEN" ] && known_nonempty "$_v"; then TLS_MODE="$_v"; fi
	_v="$(ans_get ACME_CHALLENGE)"
	if [ -z "$ACME_CHALLENGE_GIVEN" ] && known_nonempty "$_v"; then ACME_CHALLENGE="$_v"; fi
	_v="$(ans_get ADMIN_USER)"
	if [ -z "$ADMIN_USER_GIVEN" ] && known_nonempty "$_v"; then ADMIN_USER="$_v"; fi
	_v="$(ans_get TLS_TERMINATOR)"
	if [ -z "$TLS_TERMINATOR_GIVEN" ] && known_nonempty "$_v"; then TLS_TERMINATOR="$_v"; fi
fi

# "与公开站相同"（同站）是**用户看得懂的说法**，但配置里必须落成空串。
# 命令行、环境变量、记忆三条来路都在这里归一，免得多处各写一遍。
# （记忆里那句"回环管理端监听"能不能用，取决于形态，所以放到问完形态之后再判。）
case "${ADMIN_LISTEN:-}" in
"" | "$ADMIN_MERGE_LABEL" | same | merge) ADMIN_LISTEN="" ;;
esac

# 答案文件的位置与"这次是否复用了它"必须在问答开始前告诉用户。
# 不说的话会出现最糟的一种情况：用户以为自己答的每一项都生效了，实际上方括号里
# 直接回车沿用的是**上一次**的值——他看到地址变了才发现，但已经装完了。
ANSWERS_HINT=""
if [ -f "$ANSWERS_FILE" ] && [ -z "$FRESH_ANSWERS" ] && [ -n "$ASK" ]; then
	ANSWERS_HINT="1"
fi

# read_line <变量名> [提示]：读一行。
#
# 关于提示为什么要**交给 readline 自己打印**（-p），而不是在外面 printf 一句：
#
# 用户报过两件事，根因是同一个——
#   · "按删除键时，本行的提示却消失了：输入 1 再删除 1，整行变成空行"；
#   · "没有输入任何字符却出现无用的字符"。
# readline（`read -e`）并不知道"这一行前面已经有别的东西"。它把自己管理的区域
# 当作从光标当前位置开始的整行，于是：
#   · 退格删到行首时，它重新输出整行 —— 我们 printf 出来的那句提示正好在它
#     认为的这行里，被一并擦掉；用户看到的就是"提示没了、只剩空行"；
#   · 若此时终端里还残留着上一次的输出（例如上一行末尾的字符），readline 会把
#     它当作自己这行的一部分参与重绘，屏幕上就冒出莫名其妙的多余字符。
# 把提示用 `-p` 交给 readline，它就知道这是"提示"而不是"我的输入"，会把它
# 排除在重绘范围之外——退格删到提示右边就停住，提示始终在。
#
# 非 bash 分支同样把提示交给 read：`read -r <var>?<prompt>` 是 POSIX 之外的
# bash 扩展，所以这里退回"各自打印 + 读"，行为不如 bash，但不会打错行。
read_line() {
	if [ -n "${BASH_VERSION:-}" ]; then
		if [ $# -ge 2 ]; then
			IFS= read -e -r -p "$2" "$1"
		else
			IFS= read -e -r "$1"
		fi
	else
		if [ $# -ge 2 ]; then printf '%s' "$2" >&2; fi
		IFS= read -r "$1"
	fi
}

# ask <提示> <默认值>：结果放进 ASK_REPLY；提示走 stderr，避免 `> 日志` 时交互看不见。
# 不问时直接把默认值交回去——"问"与"不问"对后面的逻辑是同一件事。
ask() {
	if [ -z "$ASK" ]; then ASK_REPLY="${2:-}"; return 0; fi
	if [ -n "${2:-}" ]; then _prompt="$1 [$2]: "; else _prompt="$1: "; fi
	read_line ASK_REPLY "$_prompt" || ASK_REPLY=""
	if [ -z "$ASK_REPLY" ]; then ASK_REPLY="${2:-}"; fi
}

# ask_secret <提示>：不回显地读一行（API Token 这类）。
# 刻意**不用** read -e：readline 会重新打开回显，把刚敲的 Token 打到屏幕上。
ask_secret() {
	if [ -z "$ASK" ]; then ASK_REPLY=""; return 0; fi
	# 这里可以放心着色：它**不是** readline 打印的提示（readline 会把 ANSI 序列
	# 当成可打印宽度算进行编辑，光标会错），而是自己 printf 的，终端原样显示。
	if _color_ok 2; then
		printf '%s%s（不回显）:%s ' "$C_STEP" "$1" "$C_RST" >&2
	else
		printf '%s（不回显）: ' "$1" >&2
	fi
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
		printf '%s  （这一项不能为空）%s\n' "$(_clr 2 "$C_WARN")" "$(_clr 2 "$C_RST")" >&2
		_ai=$((_ai + 1))
	done
	return 0
}

# confirm <提示>：回车算"是"。
confirm() {
	if [ -z "$ASK" ]; then return 0; fi
	read_line _ca "$1 [Y/n]: " || _ca=""
	case "$_ca" in
	n | N | no | NO | No) return 1 ;;
	*) return 0 ;;
	esac
}

# ---------------------------------------------------------------------------
# 0a-2) Agent 发布文件的公共零件（6c 与"只更新发布文件"共用）
# ---------------------------------------------------------------------------
#
# 这一组函数是**必须共用**的，不是顺手抽出来的：
#   · 6c（部署收尾把发布文件拉进 data_dir/dist/）与
#   · 0c（只更新发布文件，见下面的模式分支）
# 做的是同一件事——"把某个版本的发布文件取回来并校验落盘"。两份实现的下场是
# 典型的"改一处＝没改"：某天给拉取加上一个新参数、或改了失败时的提示，只落在一处，
# 另一条路静默地继续用旧行为，而两条路都自称成功。
#
# 三件事刻意只在这里出现一次：
#   ① 默认发布源 URL 的拼法（`.../releases/download/v<版本>`）；
#   ② 失败时给用户的手工补救命令；
#   ③ "拉哪个范围"的问答措辞与默认值。

# default_dist_source <版本>：默认发布源的 URL 前缀。
#
# 版本号要带 v 前缀（Release 标签是 v0.1.2），而目录名不带——这两者的区别
# 就是当年"照默认 URL 去拉必然 404"的根因，所以拼法只能有一处。
default_dist_source() {
	echo "https://github.com/$RELEASE_REPO/releases/download/v$1"
}

# dist_pull_hint [版本]：拉取失败/跳过时的手工补救提示。
#
# 三条失败路径共用（跳过、版本号不是发布版本、拉取失败），省得改一处漏两处。
dist_pull_hint() {
	echo "    可以手动补（或在能访问 GitHub 的机器上拉好后拷进 $DATA/dist/）："
	printf '      %s dist pull --version %s --from %s\n' \
		"$BIN_DIR/xb-probe-master" "${1:-<版本>}" "${DIST_SOURCE:-$(default_dist_source "${1:-<版本>}")}"
	echo "    这个机房的网络到不了 GitHub 时，也可以把「设置 → 安装与下载」里的"
	echo "    install.dist_base 指向你自己的镜像，再从那台镜像上取发布文件。"
}

# ask_dist_pull_scope [默认值]：问"拉哪些平台的发布文件"，结果写进 DIST_PULL。
#
# 6c（部署收尾）与 0c（只更新发布文件）问的是同一个问题：措辞、选项、默认值、
# 以及"上次选过什么"的记忆都只能有一份——两份措辞的下场是改一处漏一处。
#
# 默认值留空时取记忆里的选择（.answers 的 DIST_PULL）。记忆只当**默认值**：
# 拉取是幂等的，重新问一次不花钱；而"上次选过 local"的人不该被悄悄升级成
# "这次拉全部 80MB"。
#
# "先跳过"这一项两条路都保留：在部署流程里它是"先装完、稍后自己补"，
# 在只更新发布文件时它就是"算了，什么都不做"。同一个词、同一个含义。
ask_dist_pull_scope() {
	_ds_def="${1:-}"
	if [ -z "$_ds_def" ]; then
		_ds_def="1"
		case "$(ans_get DIST_PULL)" in
		local) _ds_def="2" ;;
		skip) _ds_def="3" ;;
		esac
	fi
	if [ -n "$ASK" ]; then
		printf '  1) 全部平台   拉 Release 里全部 Agent 产物（推荐：节点可能是别的架构）\n' >&2
		printf '  2) 只拉本机   只拉 %s，省流量\n' "${_mp_plat:-本机}" >&2
		printf '  3) 先跳过     之后自己跑（不跑的话一键安装脚本会 404）\n' >&2
	fi
	ask "选择发布文件拉取范围（1/2/3）" "$_ds_def"
	case "$ASK_REPLY" in
	2 | local | LOCAL | Local | 本机) DIST_PULL="local" ;;
	3 | skip | SKIP | Skip | none | 跳过) DIST_PULL="skip" ;;
	*) DIST_PULL="all" ;;
	esac
	ans_put DIST_PULL "$DIST_PULL"
}

# pull_agent_release <版本> <范围 all|local> <二进制路径>：把发布文件取回来并确认真的落了盘。
#
# 返回 0 表示"磁盘上确实有了"；非 0 表示失败（**由调用方决定怎么处理**，
# 不在这里 exit——部署流程里这一步失败不该把整个部署拖倒，见 6c 的 ①）。
#
# 判据落在磁盘上（`-s .../SHA256SUMS` + 产物计数），不落在 HTTP 状态码上：
# /dist/ 受 install.mode 门控，刚部署完一台新 Master 时它返回 404 是**预期行为**，
# 拿它当判据会把每次正常部署都报成失败。见 6c 的 ②。
#
# **二进制必须是参数，不能靠全局变量**：这里原先直接用 `$_mp_bin`，而它是在 6c
# （部署收尾）里才赋值的。6c 与 0c 共用本函数，可 0c 走的是"只更新发布文件"模式，
# **6c 根本不会执行** → 变量未定义 → `set -u` 下硬报错：
#
#     deploy-master.sh: line 729: _mp_bin: unbound variable
#
# 这正是用户实测撞到的。教训：一个被两条路共用的函数，不能依赖"某一条路恰好在上文
# 设过的变量"——那种依赖在另一条路上是**隐式**的，读代码时看不出来，只有真跑才炸。
# 改成参数之后，"这条路上的二进制是哪个"变成调用点上一句明摆着的话。
pull_agent_release() {
	_pa_ver="$1"
	_pa_scope="$2"
	_pa_bin="${3:-}"
	# 版本号是本函数唯一的参数来源；空值绝不能带进 dist pull —— 那会拼出
	# `--version ""`，而下游按目录名去创建 data_dir/dist//，症状是"拉了个空版本"。
	if [ -z "$_pa_ver" ]; then
		err "没有可用的发布版本号，不拉取。"
		return 1
	fi
	# 同理：二进制没给或不可执行就当场说清楚，不要让它变成一句
	# "unbound variable"（那既不说明是哪个变量，也不说明该怎么办）。
	if [ -z "$_pa_bin" ] || [ ! -x "$_pa_bin" ]; then
		err "找不到可执行的 Master 二进制：${_pa_bin:-（调用方没给）}"
		echo "      它要用 dist pull 把发布文件写进 $DATA/dist/。用 --bin-dir 指定已安装" >&2
		echo "      的 Master 在哪，或用 --bin 给一份二进制（例如刚编译出来的那份）。" >&2
		return 1
	fi
	if [ -z "$DIST_SOURCE" ]; then
		DIST_SOURCE="$(default_dist_source "$_pa_ver")"
	fi
	echo "    来源：$DIST_SOURCE"

	_pa_rc=""
	if [ "$_pa_scope" = "local" ]; then
		echo "    范围：只拉 $_mp_plat"
		"$_pa_bin" dist pull --config "$CFG" --version "$_pa_ver" \
			--platform "$_mp_plat" --from "$DIST_SOURCE" || _pa_rc=$?
	else
		echo "    范围：全部平台"
		"$_pa_bin" dist pull --config "$CFG" --version "$_pa_ver" --all \
			--from "$DIST_SOURCE" || _pa_rc=$?
	fi
	_pa_rc="${_pa_rc:-0}"

	# 判据：文件真的在、且非空。
	if [ "$_pa_rc" != "0" ] || [ ! -s "$DATA/dist/$_pa_ver/SHA256SUMS" ]; then
		return 1
	fi
	_pa_n="$(ls -1 "$DATA/dist/$_pa_ver" 2>/dev/null | grep -c '^xb-probe-agent_' || true)"
	if [ "${_pa_n:-0}" -le 0 ]; then
		return 1
	fi

	# 发布文件是新装的（root 建的），数据目录的主人是 probe。按文件权限
	# 0644/目录 0755 本来也读得到，但"数据目录里的东西属于 probe"是
	# 一条更简单的规则——将来有人把 data_dir 挪到别处时不会踩坑。
	if id -u probe >/dev/null 2>&1; then
		chown -R probe:probe "$DATA/dist" 2>/dev/null || \
			chown -R probe "$DATA/dist" 2>/dev/null || true
	fi
	_pa_done="$DATA/dist/$_pa_ver"
	printf '    已入库 %s 个 Agent 产物 + SHA256SUMS → %s\n' "$_pa_n" "$_pa_done"
	return 0
}

# resolve_latest_agent_version：查 GitHub 上的最新 Release，输出归一化后的版本号。
#
# 为什么这条查询在**脚本里**而不是二进制里（`dist latest` 之类）：
# 部署脚本来自 `main`（每次取最新），而节点上装着的 xb-probe-master 来自某个
# **Release 标签**——两者版本天生不同步（这条差异踩过好几次，见文件里
# binary_supports_config_key 的说明）。把查询放进二进制，等于要求"先把
# Master 升级到带这个子命令的版本才能用它"，而这个功能恰恰是用在"Master
# 已经在跑、只想更新 Agent 发布文件"的场合。放在脚本里则**任何版本都能用**，
# 因为下游的 `dist pull --version/--from` 契约是稳定的、早就存在的。
#
# 两个来源，任一个能用就行（GitHub API 有 60 次/小时的匿名限额，机房出口 IP
# 常常是被共享的，所以必须有一条不限额的兜底）：
#   ① api.github.com/repos/<repo>/releases/latest  → tag_name
#   ② github.com/<repo>/releases/latest 的 302 Location → .../releases/tag/v0.1.2
#
# ⚠ 返回值必须过**形态闸**（纯数字点分）。这条不是防御性编程：解析失败时
# 拿到的往往是**别的文字**——比如旧二进制的 usage 行、HTML 错误页、JSON 里的
# 一整段。不校验就会把那段文字当版本号，拼出一个 `.../releases/download/v<那段文字>`
# 的 URL，然后以"发布文件拉取失败"的面目出现，根因被埋掉。同理，这也挡住了
# 形如 `../../x` 的 tag 变成 data_dir 下的路径。
resolve_latest_agent_version() {
	_rl_api="https://api.github.com/repos/$RELEASE_REPO/releases/latest"
	_rl_web="https://github.com/$RELEASE_REPO/releases/latest"
	_rl_fetch="$(command -v curl 2>/dev/null || true)"
	_rl_wget="$(command -v wget 2>/dev/null || true)"
	_rl_tag=""

	if [ -n "$_rl_fetch" ]; then
		# `|| true` 不能省：脚本开头是 `set -eu`，而"赋值 + 命令替换"里的命令
		# 返回非 0 会让整个脚本立刻退出——偏偏这里最常见的正是**正常的失败**
		# （GitHub 不可达、API 限流 403）。被测的失败必须能被自己的代码处理。
		_rl_tag="$("$_rl_fetch" -fsSL --max-time 20 \
			-H 'Accept: application/vnd.github+json' \
			-H 'User-Agent: probe-deploy' "$_rl_api" 2>/dev/null |
			sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1 || true)"
	elif [ -n "$_rl_wget" ]; then
		_rl_tag="$("$_rl_wget" -qO- --timeout=20 \
			--header='Accept: application/vnd.github+json' "$_rl_api" 2>/dev/null |
			sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1 || true)"
	fi

	# 兜底：不限额的重定向来源。`-o /dev/null -w '%{redirect_url}'` 不跟跳转，
	# 直接给出 Location，省掉解析 HTML 这件事。
	if [ -z "$_rl_tag" ] && [ -n "$_rl_fetch" ]; then
		_rl_loc="$("$_rl_fetch" -fsS -o /dev/null -w '%{redirect_url}' --max-time 20 "$_rl_web" 2>/dev/null || true)"
		case "$_rl_loc" in
		*/releases/tag/*) _rl_tag="${_rl_loc##*/releases/tag/}" ;;
		esac
	fi

	if [ -z "$_rl_tag" ]; then
		echo "查不到最新发布版本（GitHub 不可达？仓库 $RELEASE_REPO 还没有 Release？）。" >&2
		return 1
	fi

	# 归一化：Release 标签带 v 前缀，发布文件的目录名不带。
	_rl_ver="${_rl_tag#v}"
	_rl_ver="${_rl_ver#V}"
	# 形态闸：纯数字点分。判据写在**取到的那个值**上，而不是"有没有拿到东西"。
	case "$_rl_ver" in
	'' | *[!0-9.]* | .* | *. | *..*)
		echo "最新发布标签是「$_rl_tag」，它不是一个「纯数字点分」的版本号，不敢拿它去拼下载路径。" >&2
		echo "  （这条闸同时也是在挡形如 v../../x 的标签变成 data_dir 下的路径。）" >&2
		echo "  用自定义标签（nightly 之类）时请显式指定版本：" >&2
		printf '    %s dist pull --config %s --version <版本> \\\n' "$BIN_DIR/xb-probe-master" "$CFG" >&2
		printf '      --from https://github.com/%s/releases/download/v<版本>\n' "$RELEASE_REPO" >&2
		return 1
		;;
	esac
	echo "$_rl_ver"
	return 0
}

# ---------------------------------------------------------------------------
# 0a-3) "做什么"这一问：0b 与入口脚本**共用同一份**
# ---------------------------------------------------------------------------
#
# 入口脚本（install-master-bin.sh / install-master.sh）也要问这一问，而且必须**在
# 它自己动手之前**问：
#   · 免编译入口要先知道答案，才知道**要不要下载**那几十 MB 的 xb-probe-master；
#   · 编译入口要先知道答案，才知道**要不要装一套 Go 工具链并编译几分钟**。
# 用户实测反馈过一次：跑一键命令，先看它下完二进制、才被问"做什么"，然后他选了 3
# （只更新发布文件）——那次下载纯属浪费，而这台机器上本来就是有二进制的。
#
# 让入口自己再抄一份菜单是不行的：那样就有两份"做什么"，将来加第 4 个模式时
# 入口那份不会跟着变，用户会看到一个**缺项**的菜单，而这不会报任何错。
# 所以菜单只在这里，入口用 `--ask-mode` 取答案（见下面）。
#
# 通道约定（很重要，入口要靠它解析）：
#   · 菜单与提示走 **stderr** —— 那是"给人看的"；
#   · 答案（`1` ~ `5`）走 **stdout** —— 那是"给脚本用的"，调用方要能
#     `$(...)` 拿到一个干净的数字，不会被菜单文本污染。
ask_mode_menu() {
	printf '%s这个脚本可以做五件事（回车=1）：%s\n\n' "$(_clr 2 "$C_STEP")" "$(_clr 2 "$C_RST")" >&2
	cat >&2 <<'EOF'
  1) 安装 / 升级 Master —— 二进制、配置、systemd 服务、Agent 发布文件一并装好；
     会重写 master.yaml、按需申请证书、初始化管理员（升级走同一路径，配置不变的部分自动保留）
  2) 卸载 Master —— 停服务并删除二进制、配置、数据、系统用户（默认连数据一起删）
  3) 只更新 Agent 发布文件 —— 拉取最新 Release 的发布文件，不动 Master 本体
  4) 只升级 Master 本体 —— 只换二进制并重启服务，不重写配置、不动证书与管理员
  5) 重置管理员密码 —— 只改一个管理员的登录口令，其它什么都不动

EOF
	read_line _mode "请输入 1、2、3、4 或 5 [1]: " || _mode=""
	case "$_mode" in
	2 | uninstall | UNINSTALL | Uninstall | 卸载) echo "2" ;;
	3 | release | RELEASE | Release | 更新 | 发布文件) echo "3" ;;
	4 | upgrade | UPGRADE | Upgrade | 升级本体 | 只升级本体) echo "4" ;;
	5 | password | PASSWORD | Password | 重置密码 | 改密码) echo "5" ;;
	*) echo "1" ;;
	esac
}

# --ask-mode：只问这一问，把答案写到 stdout 然后退出。**不碰系统、不需要 root、
# 不读配置、也不要求二进制已经装好**——入口脚本是在"什么都还没做"的时候调它的。
if [ -n "$ASK_MODE" ]; then
	ask_mode_menu
	exit 0
fi

# ---------------------------------------------------------------------------
# 0b) 第一个问题：安装/升级、卸载、只更新发布文件、只升级本体，还是重置密码
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
# 不设"默认选中卸载"：卸载是破坏性操作，不该被"一路回车"选中（回车取 1=安装）。
#
# 第 3 项（只更新发布文件）是用户加的：Master 装着某个版本、GitHub 上已经有更新的
# Agent 发布版时，更新它原本要人肉敲一条带具体版本号的 dist pull 命令
# （`--version 0.1.2 --from .../download/v0.1.2`）——版本号得自己去 Release 页看，
# 敲错就是 404。放进这一问之后，"更新 Agent"变成"重跑脚本 → 选 3"，版本号由脚本查。
#
# 第 4、5 项同样是用户加的，两句原话：
#   「如果 master 更新，但是我只想更新本体……他会全部走一遍流程，包括站点设置、
#     证书申请、管理员账号设置等全部流程，但是这些我不想做」；
#   「如果我 web 界面忘记密码，就必须利用脚本卸载重装，所以 master 或者一键部署
#     脚本具备重新设置密码选项」。
# 共同点是"只想做一件事、别的都别动"，所以它们都排在工作量最大的问答**之前**：
# 选错的人不必先走完一遍整站问答才发现自己白答了。（这一点在 sh 入口脚本里更值钱
# ——那边选错要多付一次完整的 Go 编译。）
#
# 它们是**只读发布目录之外什么都不碰**的模式：不重装二进制、不重写配置、不重启服务，
# 所以放在卸载位之后、TLS 问答之前，选它就直接做完退出（见 0c / 0d / 0e）。
#
# 四种"不问"：已经给了 --uninstall / --release-only / --upgrade-only /
# --reset-password（显式参数优先于面试）、入口脚本已经问过并把答案通过 --mode
# 传下来了（MODE_GIVEN）、以及全局的 ASK 关掉时。
if [ -z "$UNINSTALL" ] && [ -z "$RELEASE_ONLY" ] && [ -z "$UPGRADE_ONLY" ] &&
	[ -z "$RESET_PASSWORD" ] && [ -z "$MODE_GIVEN" ] && [ -n "$ASK" ]; then
	case "$(ask_mode_menu)" in
	2) UNINSTALL="1" ;;
	3) RELEASE_ONLY="1" ;;
	4) UPGRADE_ONLY="1" ;;
	5) RESET_PASSWORD="1" ;;
	esac
	if [ -n "$UNINSTALL" ]; then
		printf '已选择卸载。默认**连数据一起删**（数据目录里有数据库与 master.key）。\n' >&2
		printf '想保留数据请在下一个问题里回答 n，或改用 --keep-data。\n' >&2
	fi
fi

# ---------------------------------------------------------------------------
# 0c) 只更新 Agent 发布文件（--release-only；问答题里的第 3 项）
# ---------------------------------------------------------------------------
#
# 它做的事只有三件：查最新版本 → 校验形态 → 拉发布文件（复用 0a-2 的那套零件）。
#
# 刻意**不做**的事，每一条都是"顺手做了会造成伤害"：
#   · 不换二进制、不写 master.yaml、不动单元、不重启服务——用户要的是"更新
#     新节点装到的 Agent 版本"，把 Master 一起换了属于另一件事；
#   · 不问 TLS / 监听端口 / 管理员口令：与这件事毫无关系，问了只会让人以为在重装；
#   · 不建目录、不建用户（那些由 3/7 步负责），这里只往已有目录里写文件。
#
# 放在卸载块**之前**：两者都是"只做一件事就退出"的模式，紧挨着 0b 更好读。
# 同时给 --uninstall 与 --release-only 的组合已在 0-0 段拒绝，不会走到这里。
if [ -n "$RELEASE_ONLY" ]; then
	say "只更新 Agent 发布文件"
	echo "    不动 Master 的二进制、配置与服务——只把 Agent 发布文件拉进数据目录。"

	# 用哪个二进制去取件，优先级：--bin（调用方刚编译/刚下载的那份）> 已安装的那份。
	# 允许 --bin 是有意的：安装入口（install-master.sh）编译出来的二进制一定不比
	# 节点上装着的旧，用它就不必"先升级 Master 才能更新 Agent 发布文件"。
	if [ -n "$BIN_SRC" ]; then
		_ro_bin="$BIN_SRC"
		_ro_from="--bin 指定的那份"
	else
		_ro_bin="$BIN_DIR/xb-probe-master"
		_ro_from="已安装的那份"
	fi

	# **先校验二进制，再去用它**。这一步原先排在使用之后（`$_ro_bin version` 写在
	# 前面），于是"二进制不可执行"时会先跑一次注定失败的调用、再由下面那句
	# 报错——报错来得太晚，中间那句 `version` 的输出还得靠 `2>/dev/null` 藏起来。
	# 顺序应当是"能用的前提先成立"，而不是先撞一下再解释。
	#
	# dry-run 是唯一的例外：它一个字节都不写，正是"什么都还没装的时候先看看会发生
	# 什么"的场合，所以缺二进制只警告。非 dry-run 缺了就直接停——它要靠这个二进制
	# 读配置并把文件写进 data_dir/dist/，没有它做什么都是假的。
	if [ ! -x "$_ro_bin" ]; then
		if [ -n "$DRY_RUN" ]; then
			echo "    [dry-run] 注意：$_ro_bin 不可执行，正式执行会在这里停下。" >&2
			echo "              （用 --bin-dir 指定已安装的 Master 在哪，或用 --bin 给一份二进制）" >&2
		else
			err "找不到可执行的 Master 二进制：$_ro_bin（$_ro_from）"
			echo "      它要用来读 $CFG 并把发布文件写进 $DATA/dist/。" >&2
			echo "      用 --bin-dir 指定已安装的 Master 在哪，或用 --bin 给一份二进制" >&2
			echo "      （例如刚编译出来的那份）。这台机器还没装过 Master 的话，请改选 1（安装）。" >&2
			exit 1
		fi
	fi

	# 版本查询要在取件之前完成，**失败即止**。这时候退回去拉"本机二进制版本"
	# 是静默换成另一件事：用户以为更新到了最新，实际拉的是他早就在跑的旧版本，
	# 而一路上没有任何提示。
	say "正在查 GitHub 上的最新 Release（${RELEASE_REPO}）…"
	if [ -n "$DRY_RUN" ]; then
		if _ro_probe="$(resolve_latest_agent_version)"; then
			_ro_ver="$_ro_probe"
		else
			echo "    [dry-run] 版本查询失败（原因见上）；正式执行时这里会中止。" >&2
			say "dry-run：未改动任何东西。"
			exit 1
		fi
	else
		_ro_ver="$(resolve_latest_agent_version)" || exit 1
	fi

	# 形态闸再过一遍。resolve_* 内部已经校验过，这里要的是"不管它将来怎么改，
	# 带进 dist pull 的一定是纯数字点分"——判据押在**要用的那个值**上，
	# 而不是押在"查询函数有没有做过校验"这件事上。
	case "$_ro_ver" in
	'' | *[!0-9.]* | .* | *. | *..*)
		err "查到的版本号「$_ro_ver」不是纯数字点分，拒绝使用。"
		exit 1
		;;
	esac

	# 本机版本只在二进制真的可执行时才问——上面已经保证过（dry-run 除外）。
	# 这里刻意不用 `${_ro_local:+…}` 那种嵌套引号写法：POSIX sh 下
	# `"…${v:+"…$v…"}…"` 的引号归属容易读错、也容易在不同 shell 上表现不一，
	# 拼一个变量出来更直白。
	_ro_local=""
	if [ -x "$_ro_bin" ]; then
		_ro_local="$("$_ro_bin" version 2>/dev/null | awk '{print $NF}')"
	fi
	_ro_binmsg="$_ro_bin（$_ro_from）"
	if [ -n "$_ro_local" ]; then
		_ro_binmsg="$_ro_bin（$_ro_from，版本 $_ro_local）"
	fi
	say "最新发布版本：v$_ro_ver"
	echo "    取件用的二进制：$_ro_binmsg"

	if [ -n "$DRY_RUN" ]; then
		echo "    [dry-run] 将会拉取到 $DATA/dist/$_ro_ver/，来源 $(default_dist_source "$_ro_ver")"
		echo "    [dry-run] 正式执行时还会问一次「拉全部平台还是只拉本机」，再校验落盘（dry-run 不问）。"
		say "dry-run：未改动任何东西。"
		exit 0
	fi

	if [ "$(id -u)" -ne 0 ]; then
		err "更新发布文件要往 $DATA/dist/ 里写，需要 root 或 sudo。"
		exit 1
	fi
	if [ ! -f "$CFG" ]; then
		err "读不到 $CFG —— 这台机器上还没有部署过 Master？"
		echo "      首次部署请选 1（安装 / 升级）。" >&2
		exit 1
	fi

	# 默认值刻意写死 "1" 而不是沿用记忆：这个模式存在的意义就是"拉最新"，
	# 上次选过"跳过"的人不该因为一次历史选择而在这里什么都不做。
	ask_dist_pull_scope "1"
	if [ "$DIST_PULL" = "skip" ]; then
		echo "已跳过：没有拉取任何文件（Master 与发布目录都没变）。"
		exit 0
	fi
	if [ "$DIST_PULL" = "local" ] && [ -z "$_mp_plat" ]; then
		err "认不出本机平台（uname 结果不在已知表里），不能只拉本机。"
		echo "      可以改选全部平台，或直接用 xb-probe-master dist pull 手工指定。" >&2
		exit 1
	fi
	if pull_agent_release "$_ro_ver" "$DIST_PULL" "$_ro_bin"; then
		say "完成：$DATA/dist/$_ro_ver 已就绪（新节点装到的 Agent 就是这一版）"
		echo "    新节点：后台「节点 → 注册码」生成注册码，按页面给的命令装即可"
		echo "    已装节点：sudo xb-probe-agent upgrade --check && sudo xb-probe-agent upgrade"
	else
		err "发布文件没有入库（$DATA/dist/$_ro_ver 里没有可用的 SHA256SUMS）。"
		dist_pull_hint "$_ro_ver"
		exit 1
	fi
	exit 0
fi

# ---------------------------------------------------------------------------
# 0d) 只升级 Master 本体（--upgrade-only；问答题里的第 4 项）
# ---------------------------------------------------------------------------
#
# 用户原话：「如果 master 更新，但是我只想更新本体 …… 如果我选择 1) 安装 / 升级
# Master，那么他会全部走一遍流程，包括站点设置、证书申请、管理员账号设置等全部
# 流程，但是这些我不想做，我仅仅只想升级 master 本体」。
#
# 所以这里只做四件事：停服务 → 换二进制（先备份）→ 起服务 → 当场验活。
# 刻意**不做**的事，每一条都是"顺手做了会造成伤害"：
#   · 不重写 master.yaml —— 站点形态、监听端口、TLS 方式都是运维自己定的，
#     升级一个版本不该改它们（选 1 会重写，那才是它的用途）；
#   · 不申请证书、不动 Caddy —— 证书与反代与"换二进制"毫无关系；
#   · 不碰管理员账号（要改口令用第 5 项）；
#   · 不重拉 Agent 发布文件（那是第 3 项）。
#
# 与选 1 的真正区别：选 1 是"重新收敛到脚本认为正确的状态"，本模式假设系统
# 当前是对的，只把二进制换掉。
#
# 失败**必须回滚**。新二进制起不来（端口被占、配置里有它不认识的键、架构不对）
# 时要有"把备份换回去并重启"这条退路，否则一次"只想升个版本"会变成一次停机事故
# ——而本模式从头到尾没动过配置，回滚回去的状态是确定可用的。
upgrade_only_rollback() {
	echo >&2
	err "$1"
	systemctl --no-pager status xb-probe-master 2>/dev/null | sed -n '1,10p' >&2 || true
	journalctl -u xb-probe-master --no-pager -n 20 2>/dev/null | sed 's/^/    /' >&2 || true
	if [ -f "${UPGRADE_BACKUP:-}" ]; then
		echo "    正在换回升级前的二进制…" >&2
		if command -v install >/dev/null 2>&1; then
			install -m 0755 "$UPGRADE_BACKUP" "$BIN_DIR/xb-probe-master"
		else
			cp -f "$UPGRADE_BACKUP" "$BIN_DIR/xb-probe-master" && chmod 0755 "$BIN_DIR/xb-probe-master"
		fi
		if systemctl start xb-probe-master; then
			sleep 1
			if systemctl is-active --quiet xb-probe-master; then
				echo "    已还原到升级前的版本，服务在跑；master.yaml 全程未被改动。" >&2
			else
				echo "    已还原旧二进制，但服务仍未起来——请手工看：journalctl -u xb-probe-master -n 50" >&2
			fi
		else
			echo "    还原后启动失败——请手工看：journalctl -u xb-probe-master -n 50" >&2
		fi
		echo "    备份仍留在 ${UPGRADE_BACKUP:-}（确认无误后可自行删除）。" >&2
	else
		echo "    没有可用的备份（升级前就不存在 $BIN_DIR/xb-probe-master），未做还原。" >&2
	fi
	exit 1
}

if [ -n "$UPGRADE_ONLY" ]; then
	say "只升级 Master 本体"

	# 新二进制从哪来：只认 --bin。入口脚本（install-master.sh 编译、
# install-master-bin.sh 下载）本来就是这么把二进制交给本脚本的，
	# 用户从菜单选第 4 项时走的就是那条路。
	if [ -z "$BIN_SRC" ]; then
		err "只升级本体需要指定新二进制。"
		echo "  · 走一键安装脚本的话直接选第 4 项即可（脚本会把新二进制带过来）；" >&2
		echo "  · 手工执行时用 --bin 指到新版：--bin /path/to/xb-probe-master" >&2
		exit 2
	fi
	if [ ! -f "$BIN_SRC" ]; then
		err "--bin 指向的文件不存在：$BIN_SRC"
		exit 2
	fi
	if [ "$BIN_SRC" = "$BIN_DIR/xb-probe-master" ]; then
		echo "    源与目标相同（$BIN_SRC）：没有可升级的内容，什么都没做。"
		exit 0
	fi

	# 三件事全部在**停服务之前**做完。这是本模式唯一能"在动系统之前失败"的机会：
	# 等停了服务才发现二进制跑不起来或读不懂现有配置，就白白断了一次服务。
	if ! _uo_new_ver="$("$BIN_SRC" version 2>/dev/null)"; then
		err "$BIN_SRC 跑不起来（架构不对？文件没传完？）。"
		echo "      本机平台：$_mp_plat。用 --bin 换成正确的那个文件再试。" >&2
		exit 2
	fi
	if [ -f "$CFG" ]; then
		_uo_cp_rc=0
		config_parses_ok "$CFG" || _uo_cp_rc=$?
		# 返回 1 = 明确判出"这份配置新二进制不认"，这是本模式唯一真正危险的情况：
		# 换上去服务必定起不来。到这里直接停，比"装上去 → 起不来 → 回滚"更干净。
		# 返回 2 = 判不了（旧二进制没有 tls 子命令之类），**不猜**，放行。
		if [ "$_uo_cp_rc" = "1" ]; then
			err "$BIN_SRC 读不懂现有的 $CFG —— 换上去服务起不来，已中止（什么都没改）。"
			config_mismatch_report "$_CP_OUT"
			# config_mismatch_report **只报告不退出**（它另一个使用者要接着回滚），
			# 所以这里必须自己 exit，否则会一路走到"停服务 → 换二进制"上去。
			exit 2
		fi
	fi
	echo "    新二进制：$BIN_SRC"
	echo "    版本：${_uo_new_ver:-（未报告版本）}"
	echo "    不会改动：$CFG、证书、Caddy 配置、管理员账号、$DATA/dist/"

	if [ -n "$DRY_RUN" ]; then
		echo "    [dry-run] 停服务 → 备份 $BIN_DIR/xb-probe-master → 拷入 $BIN_SRC → 起服务 → 验活"
		say "dry-run：未改动任何东西。"
		exit 0
	fi
	if [ "$(id -u)" -ne 0 ]; then
		err "升级本体要写 $BIN_DIR 并重启服务，需要 root 或 sudo（--dry-run 不需要）。"
		exit 1
	fi
	# 没有 systemd 时不猜也不硬来：Master 官方只注册 systemd（见第 5 步），
	# 容器/Alpine 容器里本来就是手工托管的，那就把替换步骤说清楚由用户执行。
	if ! command -v systemctl >/dev/null 2>&1; then
		warn "本机没有 systemctl（容器或非 systemd 发行版），不会替你重启服务。"
		echo "      手工替换步骤：停掉 Master 进程 → 用 $BIN_SRC 覆盖 $BIN_DIR/xb-probe-master → 重新启动" >&2
		echo "      当前配置是 $CFG（本模式不改它）。" >&2
		exit 0
	fi

	UPGRADE_BACKUP="$BIN_DIR/xb-probe-master.bak"
	if [ -f "$BIN_DIR/xb-probe-master" ]; then
		cp -f "$BIN_DIR/xb-probe-master" "$UPGRADE_BACKUP" || {
			err "备份现有二进制到 $UPGRADE_BACKUP 失败，已中止（不升级没有退路的二进制）。"
			exit 1
		}
		echo "    已备份旧二进制到 $UPGRADE_BACKUP"
	fi

	systemctl stop xb-probe-master >/dev/null 2>&1 || true
	if command -v install >/dev/null 2>&1; then
		install -m 0755 "$BIN_SRC" "$BIN_DIR/xb-probe-master" || upgrade_only_rollback "写入 $BIN_DIR/xb-probe-master 失败"
	else
		{ cp -f "$BIN_SRC" "$BIN_DIR/xb-probe-master" && chmod 0755 "$BIN_DIR/xb-probe-master"; } ||
			upgrade_only_rollback "写入 $BIN_DIR/xb-probe-master 失败"
	fi
	echo "    已写入 $BIN_DIR/xb-probe-master"

	systemctl start xb-probe-master || upgrade_only_rollback "新二进制启动失败（日志见上）"
	# Type=simple 下 systemctl start 一返回就认为"成功"，而进程可能已经开始
	# 申请证书、也可能**立刻**因为别的原因退出（端口被占、配置键写错）。
	# 多给一秒再确认一次，把"根本没起来"抓在回滚点上。
	sleep 1
	systemctl is-active --quiet xb-probe-master ||
		upgrade_only_rollback "新二进制启动后立即退出（端口被占用、配置键写错都会这样）"

	# 验活：服务活着 + 版本是新的那个。两条都要，缺一条都不算"升级成功"。
	_uo_now="$(systemctl show -p MainPID --value xb-probe-master 2>/dev/null || echo "")"
	say "升级完成"
	echo "    运行中的版本：${_uo_new_ver:-（未报告版本）}"
	echo "    服务状态：$(systemctl is-active xb-probe-master 2>/dev/null || echo unknown)${_uo_now:+（MainPID $_uo_now）}"
	echo "    未改动：$CFG、证书、Caddy、管理员账号、$DATA/dist/"
	echo "    旧二进制留在 $UPGRADE_BACKUP（确认无误后可删）。"
	exit 0
fi

# ---------------------------------------------------------------------------
# 0e) 重置管理员密码（--reset-password；问答题里的第 5 项）
# ---------------------------------------------------------------------------
#
# 用户原话：「如果我 web 界面忘记密码，就必须利用脚本卸载重装，所以 master 或者
# 一键部署脚本具备重新设置密码选项」。卸载重装会连数据一起清掉，为了改一个口令
# 付这个代价毫无道理。
#
# 实现只有一步：调 `xb-probe-master reset-admin`。它有 `--i-know-what-i-am-doing`
# 这道闸，这里**主动传**它——那道闸挡的是"随手敲一条命令误改生产口令"，
# 而本模式是用户从菜单里明确选了「重置管理员密码」，属于它的正当用途。
# 反过来说：这也意味着本模式不该出现在任何"无人值守的默认路径"里，
# 所以它不参与 --yes 之外的自动选择（菜单默认项仍是 1）。
if [ -n "$RESET_PASSWORD" ]; then
	say "重置管理员密码"
	if [ -n "$DRY_RUN" ]; then
		echo "    [dry-run] 将会执行：$BIN_DIR/xb-probe-master reset-admin -config $CFG -username ${ADMIN_USER:-ops}"
		echo "    [dry-run] 口令在正式执行时交互输入（或由 --admin-password / PROBE_ADMIN_PASSWORD 提供）。"
		say "dry-run：未改动任何东西。"
		exit 0
	fi
	if [ "$(id -u)" -ne 0 ]; then
		err "重置口令要写 $DATA 里的数据库，需要 root 或 sudo（--dry-run 不需要）。"
		exit 1
	fi
	if [ ! -x "$BIN_DIR/xb-probe-master" ]; then
		err "找不到可执行的 $BIN_DIR/xb-probe-master —— 这台机器上还没有装 Master？"
		echo "      首次部署请选 1（安装 / 升级）。" >&2
		exit 1
	fi
	if [ ! -f "$CFG" ]; then
		err "读不到 $CFG —— 拿不到数据目录，不敢继续。"
		exit 1
	fi

	# 口令来源优先级与选 1 的第 4 步完全一致：参数 > 环境变量 > 交互。
	# 交互时问两遍并要求一致——重置口令没有"再输一次确认"的话，
	# 打错一个字符就变成"重置成功但登不进去"，而那看起来像 bug。
	if [ -z "$ADMIN_PASSWORD" ]; then
		printf '管理员 %s 的新口令（不回显）：' "${ADMIN_USER:-ops}" >&2
		stty -echo 2>/dev/null || true
		IFS= read -r ADMIN_PASSWORD || ADMIN_PASSWORD=""
		stty echo 2>/dev/null || true
		printf '\n' >&2
		printf '再输一遍确认：' >&2
		stty -echo 2>/dev/null || true
		IFS= read -r _rp_again || _rp_again=""
		stty echo 2>/dev/null || true
		printf '\n' >&2
		if [ "$ADMIN_PASSWORD" != "$_rp_again" ]; then
			err "两次输入的口令不一致，未做任何改动。"
			exit 2
		fi
	fi
	if [ -z "$ADMIN_PASSWORD" ]; then
		err "口令不能为空。"
		exit 2
	fi

	# 与第 4 步同一个道理：**先把输出收进变量再自己判**。
	# 写成 `cmd 2>&1 | sed …` 的话，管道会把 cmd 的退出码换成 sed 的（恒 0），
	# 任何失败都被静默吞掉，脚本还会接着打印"完成"。
	# ⚠ 必须用 `|| _rp_rc=$?` 接住，不能裸赋值再读 `$?`。
	#
	# 脚本开头是 `set -eu`，而"只有赋值的简单命令"（`x="$(cmd)"`）的退出码**就是**
	# 命令替换的退出码——cmd 失败时 `set -e` 会当场把整个脚本干掉：下一行、
	# 下面的 `if`、以及"重置口令失败"那句全都不执行，用户只看到入口脚本一句
	# "部署脚本执行失败"，连原因都没有。放进 AND-OR 列表才脱离 set -e。
	_rp_rc=0
	_rp_out="$("$BIN_DIR/xb-probe-master" reset-admin -config "$CFG" \
		-username "${ADMIN_USER:-ops}" -password "$ADMIN_PASSWORD" \
		--i-know-what-i-am-doing 2>&1)" || _rp_rc=$?
	printf '%s\n' "$_rp_out" | sed 's/^/    /'
	if [ "$_rp_rc" -ne 0 ]; then
		case "$_rp_out" in
		*"解析配置失败"* | *"unmarshal"* | *"not found in type"*)
			config_mismatch_report "$_rp_out"
			;;
		esac
		err "重置口令失败（上面是 xb-probe-master 的输出）。"
		echo "      用户名「${ADMIN_USER:-ops}」不存在时也会这样——可用 --admin-user 指定。" >&2
		exit 1
	fi
	unset ADMIN_PASSWORD
	say "完成：管理员 ${ADMIN_USER:-ops} 的口令已更新，其它任何东西都没动。"
	echo "    用新口令登录后，建议在「系统设置 → 安全」里核对 TOTP 状态。"
	exit 0
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
		err "卸载需要 root 或 sudo（--dry-run 不需要）。"
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
			err "拒绝删除 $_p —— 它不像本脚本创建的目录（路径中应含 probe）。"
			echo "      确认要删请手工执行：rm -rf '$_p'" >&2
			exit 2
		fi
	done

	if [ -z "$KEEP_DATA" ] && [ -z "$DRY_RUN" ] && [ -n "$ASK" ]; then
		read_line _uc "这会删除数据目录 $DATA（含数据库与 master.key），不可恢复。继续？ [y/N]: " || _uc=""
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

	say "卸载 XB-Probe Master"
	if [ -n "$DRY_RUN" ]; then
		echo "    （dry-run：只列出将要做的事，不删除任何东西）"
	fi

	say "1/8 停止并注销服务"
	UNIT_PATH=/etc/systemd/system/xb-probe-master.service
	if [ -n "$DRY_RUN" ]; then
		echo "    [dry-run] systemctl disable --now xb-probe-master；删除 $UNIT_PATH"
	elif command -v systemctl >/dev/null 2>&1; then
		systemctl disable --now xb-probe-master >/dev/null 2>&1 || true
		if [ -f "$UNIT_PATH" ]; then
			rm -f "$UNIT_PATH" 2>/dev/null || true
			systemctl daemon-reload >/dev/null 2>&1 || true
			systemctl reset-failed xb-probe-master >/dev/null 2>&1 || true
			if [ -f "$UNIT_PATH" ]; then echo "    ⚠ 残留：$UNIT_PATH"
			else echo "    已停止并删除 $UNIT_PATH"; fi
		else
			echo "    $UNIT_PATH 不存在（未注册过 systemd 服务）"
		fi
	else
		echo "    没有 systemctl：跳过（若用 OpenRC，见下一步）"
	fi

	say "2/8 其它 init 系统残留"
	for _f in /etc/init.d/xb-probe-master /etc/supervisor/conf.d/xb-probe-master.conf; do
		if [ -e "$_f" ]; then
			rm_path "$_f" "其它进程管理器的单元"
			if command -v rc-update >/dev/null 2>&1; then
				rc-update del xb-probe-master >/dev/null 2>&1 || true
			fi
		else
			echo "    $_f 不存在"
		fi
	done

	say "3/8 删除二进制"
	rm_path "$BIN_DIR/xb-probe-master" "Master 可执行文件"

	say "4/8 删除配置目录"
	rm_path "$DIR" "配置（master.yaml、master.key）"

	say "5/8 删除数据目录"
	if [ -n "$KEEP_DATA" ]; then
		echo "    保留（--keep-data）：$DATA"
	else
		rm_path "$DATA" "数据（数据库、主题、发布件、证书）"
	fi

	say "6/8 停止并清理 Caddy（仅当它由本脚本安装/配置）"
	# 用户的原话："卸载脚本时候，caddy并没有停止监听和卸载"。
	# 这里最难的不是停服务，而是**分清哪些是我们的、哪些是别人的**：
	# 这台机器上很可能原本就有一个 Caddy（发行版包、x-ui 之类），把它一并停掉删掉
	# 是比"没卸载干净"严重得多的错误。所以三项各用各的判据，且都偏向"不动"：
	#   · 服务单元：Description 里带 "XB-Probe" 才是我们写的那份（官方包那份
	#     写的是 "Caddy web server"）；不是我们的就只停不禁用不删。
	#   · Caddyfile：有 .bak 说明我们**追加**过 → 用 .bak 还原；没有 .bak 且整份
	#     都是我们写的 → 删掉；都没有就只摘掉我们那一段（按追加速记行切分）。
	#   · 命令/二进制：本脚本不会装系统包，所以 /usr/bin/caddy 一律不动。
	CADDY_SVC=/etc/systemd/system/caddy.service
	_caddy_is_ours=""
	if [ -f "$CADDY_SVC" ] && grep -q 'XB-Probe' "$CADDY_SVC" 2>/dev/null; then
		_caddy_is_ours="1"
	fi
	if [ -n "$DRY_RUN" ]; then
		echo "    [dry-run] systemctl stop caddy（若在跑）"
		if [ -n "$_caddy_is_ours" ]; then
			echo "    [dry-run] 删除 $CADDY_SVC（本脚本写的单元）"
		elif [ -f "$CADDY_SVC" ]; then
			echo "    保留 $CADDY_SVC（不是本脚本写的：$CADDY_SVC 里没有 XB-Probe 标记）"
		else
			echo "    $CADDY_SVC 不存在"
		fi
		if [ -f "$CADDYFILE.bak" ]; then
			echo "    [dry-run] 还原 $CADDYFILE（来自 .bak）"
		elif [ -f "$CADDYFILE" ] && grep -q "$CADDY_MARK" "$CADDYFILE" 2>/dev/null; then
			echo "    [dry-run] 摘除 $CADDYFILE 里本脚本追加的段落"
		else
			echo "    $CADDYFILE 无本脚本痕迹，保留"
		fi
	else
		if command -v systemctl >/dev/null 2>&1; then
			if systemctl is-active caddy >/dev/null 2>&1; then
				systemctl stop caddy >/dev/null 2>&1 || true
				echo "    已停止 caddy"
			else
				echo "    caddy 未在运行"
			fi
			if [ -n "$_caddy_is_ours" ]; then
				systemctl disable caddy >/dev/null 2>&1 || true
				rm -f "$CADDY_SVC" 2>/dev/null || true
				systemctl daemon-reload >/dev/null 2>&1 || true
				systemctl reset-failed caddy >/dev/null 2>&1 || true
				if [ -f "$CADDY_SVC" ]; then echo "    ⚠ 残留：$CADDY_SVC"
				else echo "    已删除 $CADDY_SVC（本脚本写的单元）"; fi
			elif [ -f "$CADDY_SVC" ]; then
				echo "    保留 $CADDY_SVC（不是本脚本写的：里面没有 XB-Probe 标记）"
				echo "      它已被停止，但开机仍会自启——需要的话手工 systemctl disable caddy"
			else
				echo "    $CADDY_SVC 不存在"
			fi
		else
			echo "    没有 systemctl：跳过"
		fi
		# caddyfile_restore：把 Caddyfile 恢复成"我们动它之前"的样子。
		# 用户可能已经在这份文件里加了自己的站点，所以能还原就还原、能摘除就摘除，
		# 只有"整份都是我们写的"才删文件。
		if [ -f "$CADDYFILE.bak" ]; then
			mv -f "$CADDYFILE.bak" "$CADDYFILE" 2>/dev/null || true
			if [ -f "$CADDYFILE.bak" ]; then echo "    ⚠ 残留：$CADDYFILE.bak（请手工处理）"
			else echo "    已还原 $CADDYFILE（来自 $CADDYFILE.bak）"; fi
		elif [ -f "$CADDYFILE" ]; then
			# 判据要认**两种**我们的形态：CADDY_MARK（追加段）与 CADDY_HEAD
			# （整份生成时的头部）。只认前者的话，在"整份由我们生成"的机器上
			# 卸载会落到"像是你自己的配置，保留"——Caddyfile 留着、Caddy 继续
			# 拿着旧域名申请证书，而用户以为已经卸干净了。
			if grep -qF "$CADDY_MARK" "$CADDYFILE" 2>/dev/null || grep -qF "$CADDY_HEAD" "$CADDYFILE" 2>/dev/null; then
				# 摘掉本脚本写的那一段。与安装段同一套分流规则（两处必须一致）：
				#   · 有 CADDY_END → "追加"形态，精确摘除 MARK/HEAD..END；
				#   · 没有 CADDY_END → "整份生成"形态，以**行首的 `}`**
				#     （站点块收尾）为界。**不能**一路删到文件末尾，否则用户
				#     加在我们后面的站点会被一起删掉。
				# 用 awk 而不是 sed：速记行里含 `#`，在 sed 的地址里会与分隔符打架。
				if grep -qF "$CADDY_END" "$CADDYFILE" 2>/dev/null; then
					awk -v mark="$CADDY_MARK" -v head="$CADDY_HEAD" -v end="$CADDY_END" '
						$0 == mark || $0 == head { skip = 1; next }
						skip && $0 == end { skip = 0; next }
						!skip { print }
					' "$CADDYFILE" >"$CADDYFILE.tmp" 2>/dev/null || true
				else
					awk -v mark="$CADDY_MARK" -v head="$CADDY_HEAD" '
						$0 == mark || $0 == head { skip = 1; next }
						skip && /^\}/ { skip = 0; next }
						!skip { print }
					' "$CADDYFILE" >"$CADDYFILE.tmp" 2>/dev/null || true
				fi
				mv -f "$CADDYFILE.tmp" "$CADDYFILE" 2>/dev/null || true
				# 摘完之后只剩空行/注释时，文件留着没有意义，删掉更诚实。
				if ! grep -Eq '^[[:space:]]*[^#{[:space:]]' "$CADDYFILE" 2>/dev/null; then
					rm -f "$CADDYFILE" 2>/dev/null || true
					echo "    已删除 $CADDYFILE（摘除本脚本追加的段落后已无有效站点）"
				else
					echo "    已摘除 $CADDYFILE 里本脚本追加的段落（其余站点保留）"
				fi
			elif ! grep -Eq '^[[:space:]]*[^#{[:space:]]' "$CADDYFILE" 2>/dev/null; then
				rm -f "$CADDYFILE" 2>/dev/null || true
				echo "    已删除 $CADDYFILE（空配置）"
			else
				echo "    保留 $CADDYFILE（里面没有本脚本写的标记，像是你自己的配置）"
			fi
		else
			echo "    $CADDYFILE 不存在"
		fi
	fi

	say "8/8 删除系统用户"
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
		say "dry-run 结束：以上操作均未执行。去掉 --dry-run 即真正卸载。"
	else
		say "卸载完成。"
		echo "    未删除的只有系统包（本脚本从未安装过它们）与你自己放进来的文件。"
		# 卸载后还留着监听端口的进程，是"卸载完成"这句话最容易骗人的地方。
		# 直接把还在听的端口列出来，用户不必自己 netstat 去对。
		if command -v ss >/dev/null 2>&1; then
			_leftover="$(ss -lntp 2>/dev/null | awk 'NR>1 && ($4 ~ /:(80|443|8080|8081)$/) {print "      " $1, $4, $6}' | head -n 10)"
			if [ -n "$_leftover" ]; then
				echo "    以下监听端口与本次部署相关，仍在使用中（本脚本不认领的进程不代你停）："
				printf '%s\n' "$_leftover"
			fi
		fi
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

# split_host <地址:端口>：取主机部分。取**最后一个**冒号之前，IPv6 字面量也对。
split_host() {
	case "$1" in
	*:*) printf '%s' "${1%:*}" ;;
	*) printf '%s' "$1" ;;
	esac
}

# reachable_host <监听地址> [兜底主机]：把"监听所有网卡"的写法换成**能连**的地址。
# 0.0.0.0/[::] 是"监听哪块网卡"的写法，不是目的地址：它出现在 URL 里是错的，
# 而 0.0.0.0:8080 被归一成 127.0.0.1:8080 又只在本机有效——两个坑都踩过。
reachable_host() {
	case "$(split_host "$1")" in
	'' | '0.0.0.0' | '::' | '[::]') printf '%s' "${2:-127.0.0.1}" ;;
	*) printf '%s' "$(split_host "$1")" ;;
	esac
}

# is_loopback_listen <地址:端口>：是不是只绑回环（只有本机能连上）。
#
# ⚠ `[::1]` 那一段的**引号是必须的**：case 的模式里 `[...]` 是字符类，
# 不引起来的 `[::1]:*` 会变成"以 : 或 1 开头"，把 `1:8080` 这种也判成回环。
is_loopback_listen() {
	case "$1" in
	'' ) return 1 ;;
	127.* | localhost | localhost:* | '[::1]:'* | '::1':* | '::1') return 0 ;;
	*) return 1 ;;
	esac
}

# 管理端与公开站**共用同一个监听**吗？
#
# 判据是"留空"，这是 master.yaml 的语义（docs/04 §2.5：`admin_listen` 只在你
# 确实想再开一个本地端口时才需要填；`cmd/xb-probe-master` 里空值走 merged 分支，
# 在同一个监听上同时提供公开站与 /admin/）。
# 让"两个监听字符串相等"也算，是为了兼容手工改过的配置。
admin_shares_public_listener() {
	if [ -z "${ADMIN_LISTEN:-}" ] || [ "${ADMIN_LISTEN:-}" = "${PUBLIC_LISTEN:-}" ]; then
		return 0
	fi
	return 1
}

# resolve_admin_entry <对外协议>：算出"后台入口地址"与一句说明。
#
# 写进全局 ADMIN_URL / ADMIN_NOTE。两处用它：收尾总结、--no-service 的前台运行提示。
#
# ⚠ 这里曾经有一处**实打实的错**，用户直接撞上了：
#
#	ADMIN_URL="$SCHEME://$ADMIN_ADDR/admin/"     # ADMIN_ADDR = 归一化后的监听地址
#
# ADMIN_ADDR 来自 ADMIN_LISTEN，而它是一个**监听**地址：0.0.0.0:8080 会被归一化成
# 127.0.0.1:8080（因为 0.0.0.0 不是能连的目的地址）。于是用户填的是 0.0.0.0:8080、
# 打印出来的却是 https://127.0.0.1:8080/admin/——那个地址既错了端口语义
# （用户填的和印出来的不一致），又是"只有本机能打开"的回环地址，还是 https
# （而 tls.mode=none 时根本没有 https 监听）。三个错叠在同一个字符串里。
#
# 修掉那一处之后还留着第二个：分叉只按"有没有 public_origin"，于是**管理端独立
# 监听**的部署一律被印成 https://<公开域名>/admin/ —— 而那个地址由公开监听提供
# 服务，/admin/ 根本不在那棵路由树上。真机（腾讯云，selfsigned + 管理端
# 127.0.0.1:8081）上就是：公开看板能开、按提示访问 /admin/ 得到 404/主题页，
# 用户以为"后台没装上"。
#
# 所以正确的推导必须按**用户实际怎么访问**分四种，而不是"从监听地址反推":
#   ① 配了反代（Caddy）→ 走域名，与内部监听端口无关；
#   ② 显式配了 admin_origin → 就用它（有人把后台放在另一个域名下）；
#   ③ 管理端与公开站共用同一个监听 → 后台在公开地址的 /admin/ 下（同站）；
#   ④ 管理端独立监听 → **就是那个监听地址**：只绑回环就照实说是"本机地址"，
#      绑对外就把公开主机名与它的端口拼出来，并提醒放行端口。
resolve_admin_entry() {
	_rae_scheme="$1"
	# 后台在"本机/对外"到底长什么样，取决于管理端自己的监听地址。
	_rae_admin_host="$(split_host "${ADMIN_LISTEN:-}")"
	_rae_admin_port="$(split_port "${ADMIN_LISTEN:-}")"
	# 公开地址：优先用户填的 public_origin；没填时用"能连"的公开监听地址。
	if [ -n "${PUBLIC_ORIGIN:-}" ]; then
		_rae_public_url="$PUBLIC_ORIGIN"
		_rae_public_host="$(printf '%s' "$PUBLIC_ORIGIN" | sed -e 's|^[a-zA-Z][a-zA-Z0-9+.-]*://||' -e 's|/.*$||' -e 's|:[0-9]*$||')"
	else
		_rae_public_host="$(reachable_host "$PUBLIC_LISTEN")"
		_rae_public_url="$_rae_scheme://$_rae_public_host:$(split_port "$PUBLIC_LISTEN")"
	fi

	ADMIN_NOTE=""
	if [ "${TLS_TERMINATOR:-}" = "caddy" ]; then
		# 反代形态：对外就是域名，后台在同一个域名的 /admin/ 下（同站）或另一个域名。
		if [ -n "${ADMIN_ORIGIN:-}" ]; then
			ADMIN_URL="$(admin_url_from "$ADMIN_ORIGIN")"
		else
			_rae_first="$(printf '%s' "${PROXY_DOMAINS:-}" | cut -d, -f1)"
			if [ -z "$_rae_first" ]; then _rae_first="$_rae_public_host"; fi
			ADMIN_URL="https://$_rae_first/admin/"
			ADMIN_NOTE="（由 Caddy 反代到 ${ADMIN_LISTEN:-$PUBLIC_LISTEN}；证书自动续期）"
		fi
	elif [ -n "${ADMIN_ORIGIN:-}" ]; then
		ADMIN_URL="$(admin_url_from "$ADMIN_ORIGIN")"
	elif admin_shares_public_listener; then
		# 同站：后台就在公开地址的 /admin/ 下。**这才是"同站"的正确表达**——
		# 不是"让两个端口监听同一个地址"（那不可能），而是"同一个域名下的两个路径"。
		ADMIN_URL="$(admin_url_from "$_rae_public_url")"
		ADMIN_NOTE="（同站：后台在公开地址下的 /admin/，与公开看板共用 $PUBLIC_LISTEN 这一个监听）"
	elif is_loopback_listen "$ADMIN_LISTEN"; then
		# 独立监听、且只绑回环：**照实说是本机地址**，并给出能用的两条路。
		# 不说明的话，用户会把这个地址当成"公网后台地址"贴给别人或写进笔记。
		ADMIN_URL="$_rae_scheme://${ADMIN_LISTEN}/admin/"
		ADMIN_NOTE="（这是**本机**地址：管理端监听 $ADMIN_LISTEN 只在本机可达。公网访问请把管理端监听改成一个对外端口，或用 SSH 隧道：ssh -L 8081:$ADMIN_LISTEN 本机）"
	else
		# 独立监听、且对外可连：同一个主机、另一个端口。
		# 主机名优先用公开地址里的那个；没填公开地址时用探测到的公网 IP
		# （明文直连 IP 的部署就是这种），都拿不到才落回本机地址——那种情况下
		# 要把"这是本机地址、公网请换成公网地址"说清楚，否则用户会当成公网地址。
		_rae_h="$_rae_public_host"
		if [ -z "${PUBLIC_ORIGIN:-}" ] && [ -n "${PUBIP:-}" ]; then _rae_h="$PUBIP"; fi
		ADMIN_URL="$_rae_scheme://${_rae_h}:${_rae_admin_port}/admin/"
		if is_loopback_listen "$_rae_h"; then
			ADMIN_NOTE="（管理端是**独立监听**：$ADMIN_LISTEN；这里给的是本机地址，公网请换成这台机器的公网地址加该端口，并确认防火墙已放行）"
		else
			ADMIN_NOTE="（管理端是**独立监听**：$ADMIN_LISTEN；请确认防火墙已放行该端口）"
		fi
	fi
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

# 这一问回答的是**对外看起来是什么**，不是"证书从哪来"。
# 早先这一问直接叫"TLS 方式"，选项 1 的标签是 `none`，说明里却写着"自动装 Caddy
# 申请证书"——于是"选 1 = 没有 https"这个字面意思和"会去申请证书"正好相反，
# 用户看出的矛盾是文案的错，不是他的错。改法是把两件事摆到台面上各占一个选项：
#   1) auto  —— 要 HTTPS，证书交给本脚本装好的 Caddy 自动办（最省事）
#   2) acme  —— 要 HTTPS，证书由 Master 进程自己申请
#   1) caddy  —— 要 HTTPS，证书与反代都交给本脚本装好的 Caddy（最省事）
#   2) master —— 要 HTTPS，证书由 Master 进程自己申请（内置 ACME），Master 自己终结
#   3) selfsigned —— 要 HTTPS，自签（没有域名、只有 IP 时唯一可行）
#   4) files —— 要 HTTPS，用你已有的证书
#   5) plain —— 不要 HTTPS，只有纯 HTTP（**这种模式下脚本不会装任何反代**）
#
# ⚠ 这一问**同时决定"证书从哪来"和"谁做反代"**，因为这两件事本来是同一件事的
# 两种说法。曾经把它们拆成两问（TLS 方式 / 终结者），结果是：
#   · 第一问选 1（标签是 `none`，字面"不要证书"）、第二问却默认 caddy 去申请证书，
#     用户看到"选 1 = 没有 https"与"会去申请证书"直接矛盾；
#   · 第二问的默认值随第一问变化，用户按回车取到的组合未必是他想要的；
#   · 两次真机实测里，两问的组合至少产出了"终结者=master 却仍然监听 8080"这种
#     配置形状说不通的结果。
# 用户的原话：**"选择 1 直接是 caddy 反代，选择 2 直接是 Master 反代"**。
# 所以现在一问到底，终结者由这一问的答案**推导**（见下面的 case），不再单独问。
if [ -z "$TLS_MODE_GIVEN" ]; then
	if [ -n "$ASK" ]; then
		printf '%s下面几个问题决定对外访问方式，直接回车取方括号里的默认值。%s\n' "$(_clr 2 "$C_STEP")" "$(_clr 2 "$C_RST")" >&2
		cat >&2 <<'EOF'

对外访问方式（对外看起来是什么，证书谁来办）：
  1) caddy       HTTPS，证书自动申请与续期，反代也一并配好——只需把域名解析到本机（最省事，推荐）
  2) master      HTTPS，证书由 Master 自己申请，Master 自己监听 443 终结 TLS（需要一个已解析到本机的域名）
  3) selfsigned  HTTPS，自签证书——没有域名、只能用纯 IP 访问时唯一可行的加密方式（浏览器会提示不受信任）
  4) files       HTTPS，证书与私钥用你已有的文件
  5) plain       不使用 HTTPS，只提供纯 HTTP（不装任何反代，证书与 443 都不涉及）

选 2~4 由 Master 自己扛下 443；选 1 反代与证书都交给 Caddy，不必自己装、也不必手写配置。
EOF
	fi
	ask "选择对外访问方式（1/2/3/4/5）" "1"
	case "$ASK_REPLY" in
	1 | auto | AUTO | Auto | caddy | CADDY | Caddy) TLS_MODE="none" ;;
	2 | acme | ACME | Acme | master | MASTER | Master) TLS_MODE="acme" ;;
	3 | selfsigned | SelfSigned | SELF_SIGNED) TLS_MODE="selfsigned" ;;
	4 | files | FILES | Files) TLS_MODE="files" ;;
	5 | plain | PLAIN | Plain | none | NONE | None) TLS_MODE="plain" ;;
	*) TLS_MODE="$ASK_REPLY" ;;
	esac
	ans_put TLS_MODE "$TLS_MODE"
fi

# ---- 谁终结 TLS：由上面那一问**推导**，不再单独提问 ----
#
# 两个维度曾经被拆成两个问题（"证书从哪来" / "谁拿着证书监听 443"），但它们
# 其实是一一对应的，拆开只会让用户按出一个自相矛盾的组合：
#   · 1=caddy（内部是 tls.mode=none）→ caddy：这一项的含义就是"证书交给反代去办"；
#   · 5=plain（纯 HTTP）              → 不留终结者（见下面的归一化）；
#   · 其余（acme/selfsigned/files）   → master：证书本来就是给 Master 自己用的。
# `--tls-terminator` 仍然可以显式给出（命令行或环境变量），那时以它为准——
# 只有这一处给默认值。
if [ -z "$TLS_TERMINATOR" ]; then
	case "$TLS_MODE" in
	none) TLS_TERMINATOR="caddy" ;;
	plain) TLS_TERMINATOR="" ;;
	*) TLS_TERMINATOR="master" ;;
	esac
fi
ans_put TLS_TERMINATOR "$TLS_TERMINATOR"

case "$TLS_TERMINATOR" in
master | caddy) : ;;
'')
	# 只有 pure HTTP 允许空终结者；其它情况空值意味着上面某一段漏了赋值。
	if [ "$TLS_MODE" != "plain" ]; then
		err "未能确定 TLS 终结者（tls.mode=$TLS_MODE）。请显式给 --tls-terminator。"
		exit 2
	fi
	;;
*)
	err "--tls-terminator 只能是 master 或 caddy（收到 $TLS_TERMINATOR）。"
	exit 2
	;;
esac

# caddy 终结时 Master 侧**必须**不终结：两个进程抢 443 的报错是
# "address already in use"，会被读成"端口被别的程序占了"，离真正的原因很远。
# 这里就地纠正而不是只警告：从"选 Caddy 终结"到"Master 该用哪种 tls.mode"之间
# 的推理不该由用户完成。
#
# 但 `plain` 要排除在外：它表达的正是"我不要 https"，若在这里被悄悄改成 none 并
# 装上一个 Caddy，结果与用户的意图相反，而且下面那条"纯 HTTP 不该配反代"的检查
# 会因为 `PLAIN_HTTP` 还没被设上而永远不触发（踩过）。让 `plain` 原样走到
# 归一化那一步，由那里的互斥判断明确报错。
if [ "$TLS_TERMINATOR" = "caddy" ] && [ "$TLS_MODE" != "none" ] && [ "$TLS_MODE" != "plain" ]; then
	printf '注意：Caddy 终结 TLS，因此 Master 侧改为 --tls-mode none。\n' >&2
	printf '      你选的 %s 的证书参数不再使用。\n' "$TLS_MODE" >&2
	TLS_MODE="none"
	TLS_CERT=""
	TLS_KEY=""
	TLS_DOMAINS=""
	TLS_IPS=""
fi

# `plain` 归一化 + 与反代的互斥判断。
#
# `plain`（问答第 5 项，纯 HTTP）落到配置文件里就是 tls.mode: none——"不要 https"
# 在 Master 侧的表示本来就只有这一种。归一化放在**这里**（终结者确定之后、
# 参数校验与所有下游分支之前）的理由：
#   · 下游所有 `TLS_MODE = none` 的分支（地址默认值、证书段、收尾总结）自然生效，
#     不需要每处再多认一个值；
#   · `PLAIN_HTTP` 则为"这是刻意选的纯 HTTP，还是选 1 得到的 none"留下区分依据——
#     两者在配置里长得一样，只有意图不同，而下面这条判断判的正是意图。
#
# 顺序上踩过两次：①最初放在参数校验之后，`--tls-mode plain --tls-terminator caddy`
# 照样装上了 Caddy；②改到"即将执行"确认之后，用户要先按一次回车才看到错误。
# 现在它在问答刚结束的位置，错误在任何确认与落盘之前就报出来。
if [ "$TLS_MODE" = "plain" ]; then
	PLAIN_HTTP="1"
	TLS_MODE="none"
fi

# 纯 HTTP 却让 Caddy 去终结 TLS：配置形状自洽（两边都是 tls.mode=none），
# 但与用户意图（"我不要 https"）相反——把反代装上、证书办下来，对外就是 https 了。
# 所以按**意图**拦住，而不是按配置的形状放过。
if [ -n "$PLAIN_HTTP" ] && [ "$TLS_TERMINATOR" = "caddy" ]; then
	err "选了纯 HTTP（对外访问方式 5）就不该让 Caddy 终结 TLS（--tls-terminator caddy）。"
	echo "      要 HTTPS 请选对外访问方式 1/2/3/4；坚持纯 HTTP 请去掉 --tls-terminator。" >&2
	exit 2
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
		printf '%s证书的校验方式（前提条件不同，选错了要在申请时才失败）：%s\n' "$(_clr 2 "$C_STEP")" "$(_clr 2 "$C_RST")" >&2
		cat >&2 <<'EOF'
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

# 公开站监听的默认值取决于**谁终结 TLS** —— 这两件事必须一致，否则装出来的
# 是一份"把自己监听在 8080 上、对外却宣告 https"的配置：
#   · caddy 终结  → Master 只需监听回环，443 由 Caddy 拿着（默认 127.0.0.1:8080）；
#   · master 终结 → Master 自己监听 443 终结 TLS（默认 0.0.0.0:443）。
#
# 之前无论哪种终结者，默认值都是 0.0.0.0:8080。真机上选了 2) master 的用户，
# 清单里看到的就是"公开站监听 0.0.0.0:8080"——Master 会在 8080 上提供 https，
# 而 https://<域名>（443）永远打不开。
#
# 只在"没被显式指定"时调整；来自上次答案的值**不动**，但把后果说清楚
# （他可能就是想 https://域名:8080，那是合法的，只是要知道代价）。
if [ -z "$PUBLIC_LISTEN_GIVEN" ]; then
	case "$TLS_TERMINATOR" in
	caddy) _pl_default="127.0.0.1:8080" ;;
	master) _pl_default="0.0.0.0:443" ;;
	*) _pl_default="" ;;
	esac
	if [ -n "$_pl_default" ] && [ -z "$PUBLIC_LISTEN_FROM_ANSWERS" ]; then
		PUBLIC_LISTEN="$_pl_default"
	elif [ -n "$_pl_default" ] && [ "$PUBLIC_LISTEN" != "$_pl_default" ]; then
		_pl_port="$(split_port "$PUBLIC_LISTEN")"
		printf '\n注意：终结者=%s 时公开站监听一般是 %s，而你上次答的是 %s。\n' \
			"$TLS_TERMINATOR" "$_pl_default" "$PUBLIC_LISTEN" >&2
		if [ "$TLS_TERMINATOR" = "master" ] && [ "$_pl_port" != "443" ]; then
			printf '      Master 会在 %s 上终结 TLS，而 https://<域名>（443）打不开。\n' \
				"$PUBLIC_LISTEN" >&2
		elif [ "$TLS_TERMINATOR" = "caddy" ] && [ "$_pl_port" = "443" ]; then
			printf '      443 要留给 Caddy，Master 监听 443 会与它抢端口、启动即失败。\n' >&2
		fi
		printf '      想改用默认值：加 PROBE_FRESH_ANSWERS=1 重跑；或在这里直接回车后按提示改。\n' >&2
	fi
fi

# 先问端口，再据此给"公开访问地址"一个像样的默认值。顺序反过来的话，
# 默认地址里的端口会和用户刚选的端口对不上（提示 8080、实际听 9000）。
if [ -z "$PUBLIC_LISTEN_GIVEN" ] && [ -n "$ASK" ]; then
	if [ "$TLS_TERMINATOR" = "caddy" ]; then
		printf '%s公开站监听：Caddy 会反代到这个地址，只需监听回环（推荐 127.0.0.1:8080）；%s\n' "$(_clr 2 "$C_STEP")" "$(_clr 2 "$C_RST")" >&2
		printf '%s写 0.0.0.0 会把这个端口直接暴露在公网上，绕过 Caddy 的 https 与访问控制。%s\n' "$(_clr 2 "$C_DIM")" "$(_clr 2 "$C_RST")" >&2
	elif [ "$TLS_TERMINATOR" = "master" ]; then
		printf '%s公开站监听：Master 自己终结 TLS，要监听对外的 443（推荐 0.0.0.0:443）；%s\n' "$(_clr 2 "$C_STEP")" "$(_clr 2 "$C_RST")" >&2
		printf '%s写别的端口也能用，但对外地址里必须带上端口（如 https://你的域名:8443）。%s\n' "$(_clr 2 "$C_DIM")" "$(_clr 2 "$C_RST")" >&2
	fi
	ask "公开站监听（端口，或 IP:端口；0.0.0.0 表示所有网卡）" "$PUBLIC_LISTEN"
	_new_listen="$(normalize_listen "$ASK_REPLY" "0.0.0.0")"
	if listen_port_ok "$_new_listen"; then
		PUBLIC_LISTEN="$_new_listen"
	else
		warn "$ASK_REPLY 不是合法端口，沿用 $PUBLIC_LISTEN"
	fi
fi

# ---- 管理端监听：默认值随"谁终结 TLS"变 ----
#
# 它的取值空间比别的项多一个**空串**：空 = 与公开站共用同一个监听
# （后台在公开地址的 /admin/ 下，也就是"同站"）。见 admin_shares_public_listener。
#
#   · caddy 形态 → 127.0.0.1:8081。Caddy 会把 /admin/ 与 /api/admin/ 分流到这个
#     回环端口，这正是安全默认想要的：管理端不直接暴露在公网上。
#   · 其它形态（Master 自己终结 TLS）→ **空**（与公开站同一个监听）。
#     这里没有反代，回环端口没有任何东西能把它接出来：公开看板照常，但
#     https://<你的地址>/admin/ 会落到公开监听的路由树上、那里没有 /admin/。
#     真机（腾讯云，v0.1.4）上就是这样：用户选了 selfsigned，看板能开、
#     按脚本提示访问后台却打不开，只能怀疑"后台没装上"。而脚本当时印出来的
#     后台地址恰恰是 https://<公开地址>/admin/ —— 它只对"共用监听"成立。
if [ -z "$ADMIN_LISTEN_GIVEN" ]; then
	case "$TLS_TERMINATOR" in
	caddy) _al_default="127.0.0.1:8081" ;;
	*) _al_default="" ;;
	esac
	if [ -n "$ADMIN_LISTEN_FROM_ANSWERS" ]; then
		# 记忆里那句"只绑回环"在没有反代的形态下是**死值**：没有人能把它接出来。
		# 它不作为默认值（否则这次装完还是打不开后台），但也不删——用户确实可能
		# 只想用 SSH 隧道访问后台，问答里直接输入即可。所以要说清为什么不用它了，
		# 否则就成了"我上次答的没生效"。
		#
		# 别的记忆值一律不动：那是用户显式答过的（比如 0.0.0.0:8081），
		# 与公开监听同一条规矩——"来自上次答案的值不动，但把后果说清楚"。
		if [ -z "$_al_default" ] && is_loopback_listen "$ADMIN_LISTEN"; then
			ADMIN_LISTEN_STALE="$ADMIN_LISTEN"
			ADMIN_LISTEN=""
		fi
	else
		ADMIN_LISTEN="$_al_default"
	fi
fi

# 被丢弃的旧管理端监听要说清"为什么不用它了"——否则又是一次
# "我上次答的没生效"。这句话在问答开始前就印出来，用户还有机会改回去。
if [ -n "$ADMIN_LISTEN_STALE" ]; then
	cat >&2 <<EOF

注意：上次答的管理端监听是 $ADMIN_LISTEN_STALE，但这次是「Master 自己终结 TLS、
      没有前置反代」，那个地址谁也接不出来（后台会变成"只有本机能开"）。
      本次改用「$ADMIN_MERGE_LABEL」：后台在公开地址的 /admin/ 下、不占新端口。
      确实只想用 SSH 隧道访问后台：下面的管理端监听把它再输一次即可。
EOF
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
# 域名下，而 Master 支持在**同一个监听**上同时提供两套路径（admin_listen 留空，
# 见 cmd/xb-probe-master 的 merged 分支），也可以由反代按路径转发实现。
# 所以撞车时重问，并把正确做法说清楚。
_admin_tries=0
_adm_loop_warned=""
_adm_default="${ADMIN_LISTEN:-$ADMIN_MERGE_LABEL}"
while [ -z "$ADMIN_LISTEN_GIVEN" ] && [ -n "$ASK" ]; do
	ask "管理端监听（回车=与公开站同一个监听，后台在 /admin/ 下；也可填 IP:端口 单独开一个）" "$_adm_default"
	case "$ASK_REPLY" in
	"" | "$ADMIN_MERGE_LABEL" | same | merge)
		ADMIN_LISTEN=""
		break
		;;
	esac
	_new_listen="$(normalize_listen "$ASK_REPLY" "127.0.0.1")"
	if ! listen_port_ok "$_new_listen"; then
		warn "$ASK_REPLY 不是合法端口，沿用 ${ADMIN_LISTEN:-$ADMIN_MERGE_LABEL}"
		break
	fi
	if [ "$_new_listen" = "$PUBLIC_LISTEN" ]; then
		printf '错误：管理端与公开站不能监听同一个地址端口（%s）——一个进程无法监听两次。\n' "$_new_listen" >&2
		printf '      想要「同一个域名下的两个路径」，请直接回车（%s），\n' "$ADMIN_MERGE_LABEL" >&2
		printf '      或者给管理端另开一个端口（如 127.0.0.1:8081）再由反代按 /admin/ 转发。\n' >&2
		_admin_tries=$((_admin_tries + 1))
		if [ "$_admin_tries" -ge 3 ]; then
			printf '      连续三次都相同，改用「%s」。\n' "$ADMIN_MERGE_LABEL" >&2
			ADMIN_LISTEN=""
			break
		fi
		continue
	fi
	# 独立监听 + **没有反代** + 只绑回环 = 一个谁也打不开的后台。
	# 这不是"配置得不好"，是**装完就不可用**（公开看板正常、后台打不开），
	# 所以在这里说清楚并给默认值；用户再答一次还是回环就尊重他（SSH 隧道合理）。
	if [ "$TLS_TERMINATOR" != "caddy" ] && [ -z "$_adm_loop_warned" ] &&
		is_loopback_listen "$_new_listen"; then
		_adm_loop_warned="1"
		printf '\n%s注意：这次是 Master 自己终结 TLS，没有前置反代。%s\n' "$(_clr 2 "$C_WARN")" "$(_clr 2 "$C_RST")" >&2
		printf '      管理端只监听 %s 谁也接不出来：公开看板照常，但 https://<你的地址>/admin/ 打不开。\n' "$_new_listen" >&2
		printf '      · 回车 = %s（后台在公开地址的 /admin/ 下，不占新端口，推荐）\n' "$ADMIN_MERGE_LABEL" >&2
		printf '      · 或填 0.0.0.0:8081 单独开一个（记得防火墙放行）；只想 SSH 隧道访问就把 %s 再输一次\n' "$_new_listen" >&2
		_adm_default="$ADMIN_MERGE_LABEL"
		continue
	fi
	ADMIN_LISTEN="$_new_listen"
	break
done

# 端口撞车的**硬校验**。问答那一层只在能问的时候拦（重问一次），
# 而 `--public-listen/--admin-listen` 显式给出时不经过问答——那条路径必须
# 也能拦下来，否则同样会装出一个启动即退出的服务。一处判、两处调用。
if [ "$PUBLIC_LISTEN" = "$ADMIN_LISTEN" ]; then
	err "公开站与管理端不能监听同一个地址端口（$PUBLIC_LISTEN）。"
	echo "      一个进程无法在同一地址端口上监听两次，启动时会以" >&2
	echo "      「address already in use」退出。" >&2
	echo "      想让后台跟公开看板共用一个监听：把管理端监听留空（$ADMIN_MERGE_LABEL）。" >&2
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
		printf '%s  （用户名只允许字母、数字与 . _ @ -）%s\n' "$(_clr 2 "$C_WARN")" "$(_clr 2 "$C_RST")" >&2
		_ui=$((_ui + 1))
	done
fi
if [ -z "$ADMIN_USER" ]; then ADMIN_USER="ops"; fi
# 兜底：--admin-user / PROBE_ADMIN_USER 那条路径不经过上面的循环。
if ! admin_user_ok "$ADMIN_USER"; then
	err "管理员用户名只允许字母、数字与 . _ @ -（收到 $ADMIN_USER）。"
	exit 2
fi

# ---- 口令紧跟在用户名后面问，然后才轮到"即将执行"确认 ----
#
# 顺序是刻意安排的（用户原话："要求输入管理员用户名时，紧接着要求输入密码，
# 下一步才是确认"）：账号口令是**同一件事的两半**，中间插一大段别的输出只会
# 让人以为口令环节已经过去了；放在确认之前，用户按下回车时整套凭据已经定下，
# 不存在"确认完才知道还要输什么"。
#
# 仍然只有**没给过**口令时才问（--admin-password / PROBE_ADMIN_PASSWORD 优先，
# 无人值守场景不该被一个问题卡住）；问法沿用 ask_secret（不回显、不用 read -e，
# readline 会重新打开回显，把刚敲的口令打到屏幕上）。
# 空口令不在这里判死：留空往下走，第 4 步那句"口令不能为空"是唯一的判处，
# 免得同一个规则在两处各说各话。口令绝不进 .answers（见 ans_put 的排除表）。
if [ -z "$ADMIN_PASSWORD" ] && [ -n "$ASK" ]; then
	_pi=0
	while [ "$_pi" -lt 3 ]; do
		ask_secret "管理员 $ADMIN_USER 的口令"
		ADMIN_PASSWORD="$ASK_REPLY"
		[ -n "$ADMIN_PASSWORD" ] && break
		printf '%s  （口令不能为空）%s\n' "$(_clr 2 "$C_WARN")" "$(_clr 2 "$C_RST")" >&2
		_pi=$((_pi + 1))
	done
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

# ---------------------------------------------------------------------------
# Master 自己终结 TLS 时的端口冲突预检
# ---------------------------------------------------------------------------
#
# 443 只能有一个主人。真机上出现过：用户先按选项 1 装了 Caddy 反代，过一会儿
# 又按选项 2 让 Master 自己终结——Caddy 还在跑着占 80/443，Master 启动必然
# `bind: address already in use`，而脚本把它报成"启动后立即退出"，等回滚之后
# 用户才知道真正的原因。
#
# 与其等到回滚，不如在**动手之前**说清楚并给他一个选择。只动"本脚本自己装的
# Caddy"（判据与卸载段一致：服务单元里带 XB-Probe 标记）；别人装的
# Caddy/Nginx 只提示、不动手——停掉别人的服务是比"装不上"严重得多的错误。
if [ "$TLS_TERMINATOR" = "master" ] && [ -z "$DRY_RUN" ] && command -v systemctl >/dev/null 2>&1; then
	if systemctl is-active caddy >/dev/null 2>&1; then
		_caddy_ours=""
		if [ -f /etc/systemd/system/caddy.service ] &&
			grep -q 'XB-Probe' /etc/systemd/system/caddy.service 2>/dev/null; then
			_caddy_ours="1"
		fi
		echo >&2
		echo "注意：本机的 Caddy 正在运行，而 Master 自己终结 TLS 需要 443（以及 80 给 ACME）。" >&2
		echo "      两个进程都要 443，后启动的那个必然报 address already in use。" >&2
		if [ -n "$_caddy_ours" ]; then
			if confirm "现在停掉并禁用本脚本装的 Caddy？"; then
				systemctl stop caddy >/dev/null 2>&1 || true
				systemctl disable caddy >/dev/null 2>&1 || true
				echo "      已停止并禁用 caddy（配置仍在 $CADDYFILE，随时可以再启用）" >&2
			else
				echo "      保留 Caddy —— Master 启动时会因 443 被占用而失败并回滚。" >&2
			fi
		else
			echo "      这台机器上的 Caddy 不是本脚本装的：请自行停掉它（systemctl stop caddy）后重跑。" >&2
		fi
	fi
fi

if [ -n "$ASK" ]; then
	# 整块输出前判一次流：stderr 是终端才着色（管道/日志里一个 ESC 都不落）。
	_lb="$(_clr 2 "$C_BLD")"
	_rs="$(_clr 2 "$C_RST")"
	echo >&2
	printf '%s即将执行：%s\n' "$(_clr 2 "$C_STEP")" "$_rs" >&2
	if [ -n "$ANSWERS_HINT" ]; then
		printf '%s  （方括号里的默认值来自上次的答案：%s）%s\n' "$(_clr 2 "$C_DIM")" "$ANSWERS_FILE" "$_rs" >&2
		printf '%s  （想全部重答一遍：PROBE_FRESH_ANSWERS=1 重跑本脚本）%s\n' "$(_clr 2 "$C_DIM")" "$_rs" >&2
	fi
	printf '  %s%s%s：%s\n' "$_lb" "公开站监听" "$_rs" "$PUBLIC_LISTEN" >&2
	printf '  %s%s%s：%s\n' "$_lb" "管理端监听" "$_rs" "${ADMIN_LISTEN:-（与公开站同一个监听：后台在 /admin/ 下）}" >&2
	printf '  %s%s%s：%s\n' "$_lb" "公开地址" "$_rs" "${PUBLIC_ORIGIN:-（留空：按请求 Host 回填）}" >&2
	printf '  %s%s%s：%s\n' "$_lb" "管理地址" "$_rs" "${ADMIN_ORIGIN:-（与公开站同一个域名）}" >&2
	# 清单里的 TLS 一行要说清"对外是 http 还是 https、证书谁办"，
	# 而不是把内部的 tls.mode 值原样抛给用户——`none` 这个词在两种场景下
	# 对外含义完全相反（Caddy 终结时对外是 https），直接显示只会误导。
	if [ "$TLS_MODE" = "plain" ]; then
		_tls_note="纯 HTTP（不使用 HTTPS）"
	elif [ "$TLS_MODE" = "none" ] && [ "$TLS_TERMINATOR" = "caddy" ]; then
		_tls_note="HTTPS，证书由 Caddy 自动申请与续期"
	else
		# 不再显示"终结者=xxx"——那是内部概念，用户选的就是"谁做反代"，
		# 用结果说话即可。
		case "$TLS_MODE" in
		acme) _tls_note="HTTPS，证书由 Master 自己向 CA 申请（challenge=$ACME_CHALLENGE）" ;;
		selfsigned) _tls_note="HTTPS，自签证书（浏览器会提示不受信任）" ;;
		files) _tls_note="HTTPS，使用你提供的证书文件" ;;
		*) _tls_note="$TLS_MODE（终结者=$TLS_TERMINATOR）" ;;
		esac
	fi
	printf '  %s%s%s：%s\n' "$_lb" "对外访问" "$_rs" "$_tls_note" >&2
	if [ "$TLS_TERMINATOR" = "caddy" ]; then
		printf '  %s%s%s：%s\n' "$_lb" "反代" "$_rs" "安装 Caddy，反代 ${PROXY_DOMAINS:-$PUBLIC_ORIGIN} → $PUBLIC_LISTEN" >&2
	fi
	# 口令已经在上一步问过了（用户名 → 口令 → 这里才确认），所以清单里说的是实情。
	if [ -n "$ADMIN_PASSWORD" ]; then
		printf '  %s%s%s：%s（口令已输入，不再显示）\n' "$_lb" "管理员" "$_rs" "$ADMIN_USER" >&2
	else
		printf '  %s%s%s：%s（口令随后输入）\n' "$_lb" "管理员" "$_rs" "$ADMIN_USER" >&2
	fi
	printf '  %s%s%s：%s\n' "$_lb" "配置目录" "$_rs" "$DIR" >&2
	printf '  %s%s%s：%s\n' "$_lb" "数据目录" "$_rs" "$DATA" >&2
	if [ -n "$NO_SERVICE" ]; then
		printf '  %s%s%s：%s\n' "$_lb" "服务" "$_rs" "不注册（--no-service）" >&2
	else
		printf '  %s%s%s：%s\n' "$_lb" "服务" "$_rs" "systemd（xb-probe-master，开机自启）" >&2
	fi
	echo >&2
	if ! confirm "继续？"; then
		warn "已取消，未改动系统。"
		exit 0
	fi
fi

# ---- TLS 参数校验：宁可在这里失败，也不要写出一个"看起来配好了"的配置文件 ----
#
# 配置文件是**严格解析**的（LoadMaster 用 KnownFields），写错键名会让 Master
# 启动即失败；但"值合法、语义不成立"（比如 acme 却没给任何域名/IP）解析得过，
# 却要等到启动后申请证书才暴露。所以这里把每种模式的前置条件前置校验。
# （`plain` 已在上游归一化成 `none`，这里不再需要认它。）
case "$TLS_MODE" in
none | files | selfsigned | acme) : ;;
*)
	err "--tls-mode 只能是 none / files / selfsigned / acme（收到 $TLS_MODE）。"
	exit 2
	;;
esac

if [ -n "$TLS_DOMAINS$TLS_IPS$ACME_EMAIL$TLS_CERT$TLS_KEY" ] && [ "$TLS_MODE" = "none" ]; then
	err "给了证书相关参数却 --tls-mode none。要么把模式写对，要么去掉这些参数——"
	echo "      否则证书不会生效，而命令行看起来像「配了 TLS」。" >&2
	exit 2
fi

if [ "$TLS_MODE" = "files" ]; then
	[ -n "$TLS_CERT" ] && [ -n "$TLS_KEY" ] || {
		err "mode=files 必须同时给 --tls-cert 与 --tls-key。"
		exit 2
	}
	for f in "$TLS_CERT" "$TLS_KEY"; do
		[ -f "$f" ] || { echo "错误：找不到证书文件 $f。" >&2; exit 2; }
	done
fi

if [ "$TLS_MODE" = "acme" ]; then
	if [ -z "$TLS_DOMAINS$TLS_IPS" ]; then
		err "mode=acme 至少要给一个 --tls-domain 或 --tls-ip（否则没有可申请的标识符）。"
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
			err "challenge=dns-01 只支持域名（DNS 里没有 IP 的挑战记录可写），"
			echo "      请用 --tls-domain 给出域名，或改用 http-01/tls-alpn-01。" >&2
			exit 2
		fi
		if [ -z "$ACME_DNS_TOKEN" ]; then
			err "challenge=dns-01 必须提供 DNS API Token。"
			echo "      交互执行时会被提示输入；无人值守请设 PROBE_ACME_DNS_TOKEN。" >&2
			exit 2
		fi
	fi
	# http-01 / tls-alpn-01 都要求**本进程自己**在被 CA 访问的端口上应答
	# （http-01 用 80，tls-alpn-01 用 443）。端口被别的进程占着时，Master
	# 起不来、证书也申请不到，而报出来的现象只是"证书申请失败"，离原因很远。
	#
	# 真机（v0.1.3 → v0.1.4）上用户报"申请期间 80 一直没起来"，真正的原因有两个，
	# 一个是他看不到的**权限**（服务以 probe 用户运行，绑 80 需要
	# CAP_NET_BIND_SERVICE，而当时写的单元没给），另一个是端口占用。这里先把
	# 端口这一条查掉，权限那条由 systemd 单元保证（见 5/7 步）。
	if [ "$ACME_CHALLENGE" = "http-01" ] || [ "$ACME_CHALLENGE" = "tls-alpn-01" ]; then
		_pv_port=80
		[ "$ACME_CHALLENGE" = "tls-alpn-01" ] && _pv_port=443
		_pv_use=""
		if command -v ss >/dev/null 2>&1; then
			_pv_use="$(ss -ltn 2>/dev/null | awk -v p=":$_pv_port\$" '$4 ~ p {print; exit}')"
		fi
		if [ -z "$_pv_use" ] && command -v netstat >/dev/null 2>&1; then
			_pv_use="$(netstat -ltn 2>/dev/null | awk -v p="[:.]$_pv_port[[:space:]]" \
				'/[Ll][Ii][Ss][Tt][Ee][Nn]/ && $4 ~ p {print; exit}')"
		fi
		if [ -n "$_pv_use" ]; then
			warn "challenge=$ACME_CHALLENGE 需要 Master 自己监听 $_pv_port 端口，但它已被占用："
			printf '      %s\n' "$_pv_use" >&2
			echo "      Master 会因此起不来、证书随之申请失败。如果该端口由前置反代占用，" >&2
			echo "      让反代把 /.well-known/acme-challenge/ 转给 Master，并把配置里的" >&2
			echo "      tls.acme.http_addr 设为 off（http-01 场景），否则两边会抢同一个端口。" >&2
		fi
		if [ "$_pv_port" -lt 1024 ]; then
			echo "提示：$_pv_port 属于特权端口（<1024）。本脚本写的 systemd 单元已授予" >&2
			echo "      CAP_NET_BIND_SERVICE，服务绑得上；手工起的进程（直接跑二进制）" >&2
			echo "      需要 root 或自行授予该项能力，否则会 bind: permission denied。" >&2
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
			warn "--tls-mode none 但 --public-origin 是 https://。若 TLS 由前置反代终结，"
			echo "      请把反代网段填进 trusted_proxies，否则 Cookie / HSTS 判定会走明文分支。" >&2
		fi
		;;
	files:http://* | selfsigned:http://* | acme:http://*)
		err "--tls-mode $TLS_MODE 与 --public-origin $PUBLIC_ORIGIN 不一致。"
		echo "      配了证书就应当对外宣告 https://（端口不是 443 时写成 https://host:端口）。" >&2
		exit 2
		;;
	esac
fi

# 配置路径与渲染函数放在这里（而不是"3/7"那一步里面）：--dry-run 要在改动系统
# **之前**就把将要写入的内容打出来。
#
# CFG 自身的定义已经上移到文件前部的"0-0"段（问答与"只更新发布文件"都要用它，
# 只能有一份来源）。这里保留这段说明，是因为"为什么这么早"这个理由仍然成立。

# 探测/诊断 helper（PROBE_SENTINEL_KEY、_probe_write、_probe_channel_ok、
# binary_supports_config_key、config_parses_ok、config_mismatch_report）
# **已上移到文件前部的「0-0」段之前**：脚本是自上而下执行的，函数必须定义在
# 任何调用点之前，而 0d/0e 两个"只做一件事"的模式分支（只升级本体、重置口令）
# 要在 TLS 问答**之前**就用它们做配置诊断——不然管理员只想换一个二进制，
# 却要先回答一轮"你要哪种证书"。

# fail_config_mismatch <二进制的原始输出>：报出"配置与二进制不匹配"并**回滚**。
#
# 报告部分（点名是哪个键 + 三种修法）住在 config_mismatch_report，定义在文件前部，
# 因为它有两个使用者：本函数（安装流程：配置可能刚被本脚本改写，报完必须撤掉这一
# 轮的改动）与 --upgrade-only 模式（此时一个字节都还没写，报完直接退出即可）。
# 两处的原因与修法完全一样，各写一份迟早会有一份忘了更新。
fail_config_mismatch() {
	config_mismatch_report "${1:-}"
	if [ -n "$_CM_BAD_KEY" ]; then
		rollback "配置键 $_CM_BAD_KEY 与当前二进制不匹配（版本错配，见上面的说明）"
	else
		rollback "配置文件无法被当前二进制解析（版本错配，见上面的说明）"
	fi
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
	# master_url 未设置时会回落到 public_origin（见 cmd/xb-probe-master/main.go
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
# 用户的原话是："xb-probe-master 可以监听任何端口，但如果选择下面的 2 3 4，应该自行
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
# CADDY_DIR / CADDYFILE / CADDY_MARK 定义在文件顶部的变量区（卸载路径也要用）。
# proxy_target [监听地址]：反代指向哪儿。不给参数时用 PUBLIC_LISTEN（兼容旧调用）。
# 必须是 Caddy 能连到的**目的地址**，而 0.0.0.0/[::] 是"监听所有网卡"的写法——
# 不是个能连的地址。Linux 上连 0.0.0.0 恰好常被当成 localhost，若 Caddy 跑在
# 容器里或另一台机器上就直接失败，而失败信息完全看不出原因。
proxy_target() {
	_pt_addr="${1:-$PUBLIC_LISTEN}"
	case "$_pt_addr" in
	0.0.0.0:*) printf '127.0.0.1:%s' "${_pt_addr#0.0.0.0:}" ;;
	"[::]:"*) printf '127.0.0.1:%s' "${_pt_addr#\[::\]:}" ;;
	":::" | "[::]") printf '127.0.0.1' ;;
	*) printf '%s' "$_pt_addr" ;;
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
	# 管理端是否在**另一个监听地址**上——这决定了要不要多写一条反代规则。
	#
	# 用户实测报过："访问 https://域名/ 一切正常，但 https://域名/admin/ 提示
	# 404 page not found"。原因就在这里：默认配置是公开站 0.0.0.0:8080、
	# 管理端 127.0.0.1:8081（两个不同地址），而 Caddy 原来把所有请求都转给
	# 8080——8080 上的路由树里根本没有 /admin/，于是 404。
	# "管理端只监听回环"这个安全默认没错，错在反代没有把后台那几条路径分流过去。
	# 判据用 proxy_target 的产物而不是原始字符串：0.0.0.0:8080 与 127.0.0.1:8080
	# 指的是同一个进程，直接比字符串会把它们当成两个。
	_adm_target="$(proxy_target "$ADMIN_LISTEN")"
	_pub_target="$(proxy_target "$PUBLIC_LISTEN")"
	_need_split_admin=""
	if [ "$_adm_target" != "$_pub_target" ]; then _need_split_admin="1"; fi

	echo "$CADDY_HEAD"
	echo "# Caddy 负责 https（证书自动申请与续期）并把请求反代给 Master。"
	echo "# 公开站 $PUBLIC_LISTEN、管理端 ${ADMIN_LISTEN:-$PUBLIC_LISTEN（与公开站共用同一个监听）}（都是明文，证书与 443 由 Caddy 管）。"
	echo
	if [ -n "$_adm" ] && [ "$_adm" != "$_pub" ]; then
		# 分离形态。管理域名的 /admin/ 由它自己兜；公开域名**显式拒绝** /admin/，
		# 免得两个域名都能进后台（那会让"分离"这个设置看起来生效了、实际没有）。
		echo "$_pub {"
		echo "	encode gzip"
		echo "	@admin path /admin /admin/*"
		echo "	respond @admin 404"
		echo "	reverse_proxy $_pub_target"
		echo "}"
		echo
		echo "$_adm {"
		echo "	encode gzip"
		if [ -n "$_need_split_admin" ]; then
			# 管理域名上，后台与它的 API 转给管理端监听，其余（公开看板本身）
			# 仍走公开站——同站形态下这两部分本来就住在一个域名下。
			echo "	@adminui path /admin /admin/*"
			echo "	@adminapi path /api/admin/*"
			echo "	handle @adminui {"
			echo "		reverse_proxy $_adm_target"
			echo "	}"
			echo "	handle @adminapi {"
			echo "		reverse_proxy $_adm_target"
			echo "	}"
			echo "	handle {"
			echo "		reverse_proxy $_pub_target"
			echo "	}"
		else
			echo "	reverse_proxy $_pub_target"
		fi
		echo "}"
	else
		echo "$_pub {"
		echo "	encode gzip"
		if [ -n "$_need_split_admin" ]; then
			# 同站形态 + 管理端独立监听：一个域名，但后台那几条路径要分流。
			# 顺序要紧：Caddy 按 handle 出现的先后匹配，先给 /admin/、/api/admin/
			# 两个更具体的前缀，剩下的才落到公开站。
			echo "	@adminui path /admin /admin/*"
			echo "	@adminapi path /api/admin/*"
			echo "	handle @adminui {"
			echo "		reverse_proxy $_adm_target"
			echo "	}"
			echo "	handle @adminapi {"
			echo "		reverse_proxy $_adm_target"
			echo "	}"
			echo "	handle {"
			echo "		reverse_proxy $_pub_target"
			echo "	}"
		else
			echo "	reverse_proxy $_pub_target"
		fi
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
			err "下载 caddy 失败（$_ca_url）。"
			echo "      若网络受限，可自己装好 caddy 后重跑本脚本。" >&2
			return 1
		}
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$BIN_DIR/caddy" "$_ca_url" || {
			err "下载 caddy 失败（$_ca_url）。"
			return 1
		}
	else
		err "找不到 curl 或 wget，无法下载 caddy。"
		return 1
	fi
	chmod 0755 "$BIN_DIR/caddy"
	if ! "$BIN_DIR/caddy" version >/dev/null 2>&1; then
		err "下载到的 caddy 无法执行。"
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
	if [ -e "$BIN_DIR/xb-probe-master" ]; then PREE_HAVE_BIN="1"; fi
	if id -u probe >/dev/null 2>&1; then PREE_HAVE_USER="1"; fi
	if [ -e /etc/systemd/system/xb-probe-master.service ]; then PREE_HAVE_UNIT="1"; fi
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
	say "回滚 1/6 停止服务" >&2
	# 先停再删：服务还在跑着的时候删二进制，systemd 会按 Restart=always 反复重启，
	# 而它每次重启都会在日志里留一条"找不到可执行文件"——把真正的失败原因淹掉。
	systemctl stop xb-probe-master >/dev/null 2>&1 || true
	systemctl disable xb-probe-master >/dev/null 2>&1 || true
	if [ "$TLS_TERMINATOR" = "caddy" ]; then
		systemctl stop caddy >/dev/null 2>&1 || true
		systemctl disable caddy >/dev/null 2>&1 || true
	fi
	echo "    已停止" >&2

	say "回滚 2/6 服务单元" >&2
	if [ -z "$PREE_HAVE_UNIT" ]; then
		rm -f /etc/systemd/system/xb-probe-master.service 2>/dev/null || true
		echo "    已删除 xb-probe-master.service（本次新建）" >&2
	else
		echo "    保留 xb-probe-master.service（安装前就存在）" >&2
	fi
	# caddy 的服务单元**只在本次是它装的**才删。用"安装前有没有 caddy 这个命令"
	# 判不出来——apt 装完之后命令就在 PATH 里了，所以这里用单元文件是否存在过。
	if [ "$TLS_TERMINATOR" = "caddy" ] && [ -z "$PREE_HAVE_CADDY_UNIT" ]; then
		rm -f /etc/systemd/system/caddy.service 2>/dev/null || true
		echo "    已删除 caddy.service（本次新建）" >&2
	fi
	systemctl daemon-reload >/dev/null 2>&1 || true
	systemctl reset-failed xb-probe-master >/dev/null 2>&1 || true
	systemctl reset-failed caddy >/dev/null 2>&1 || true

	say "回滚 3/6 二进制" >&2
	if [ -z "$PREE_HAVE_BIN" ] && [ -n "$BIN_SRC" ]; then
		rm -f "$BIN_DIR/xb-probe-master" 2>/dev/null || true
		echo "    已删除 $BIN_DIR/xb-probe-master（本次新建）" >&2
	elif [ -n "$PREE_HAVE_BIN" ]; then
		echo "    保留 $BIN_DIR/xb-probe-master（安装前就存在；升级失败时旧二进制已被覆盖）" >&2
	fi

	say "回滚 4/6 配置与反代配置" >&2
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

	say "回滚 5/6 数据目录" >&2
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

	say "回滚 6/6 系统用户" >&2
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
	say "回滚完成。系统已恢复到运行本脚本之前的状态。" >&2
	echo "    修好上面的原因后重跑本脚本即可（它是幂等的）。" >&2
	echo >&2
	exit 1
}

# --dry-run 不写任何东西，因此不需要 root——这样可以在桌面机上先把将要写入的
# master.yaml 打出来看一遍。
if [ -z "$DRY_RUN" ] && [ "$(id -u)" -ne 0 ] && [ -z "$NO_SERVICE" ]; then
	err "请用 root 或 sudo 执行（容器场景可加 --no-service 并以 --dir/--data 指定可写目录）。"
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

if [ -z "$BIN_SRC" ] && [ ! -x "$BIN_DIR/xb-probe-master" ]; then
	err "既没有 --bin，$BIN_DIR 里也没有已安装的 xb-probe-master。"
	exit 2
fi
if [ -n "$BIN_SRC" ] && [ ! -f "$BIN_SRC" ]; then
	err "找不到二进制 $BIN_SRC（先跑 make master 或 build.ps1）。"
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
	say "dry-run：以下是将要写入 $CFG 的内容（未改动系统）"
	echo
	render_config
	if [ "$TLS_TERMINATOR" = "caddy" ]; then
		echo
		say "dry-run：以下是将要写入 $CADDYFILE 的内容"
		echo
		render_caddyfile
		echo
		say "dry-run：还会安装 Caddy 并注册 caddy.service（若尚未安装）"
	fi
	echo
	say "dry-run：未创建用户/目录、未安装二进制、未注册服务、未初始化管理员。"
	echo "    去掉 --dry-run 即执行（其余参数完全一致）。"
	echo
	say "dry-run：最后还会把 Agent 发布文件拉进 $DATA/dist/（xb-probe-master dist pull）。"
	echo "    不拉的话一键安装脚本会在下载 xb-probe-agent 那一步 404；用 --no-dist-pull 跳过。"
	exit 0
fi

say "1/7 创建系统用户与目录"
# 服务以专用低权限用户运行（3.2）。Alpine/BusyBox 没有 useradd，退回 adduser。
if ! id -u probe >/dev/null 2>&1; then
	if command -v useradd >/dev/null 2>&1; then
		useradd --system --no-create-home --shell /usr/sbin/nologin probe || \
			useradd -r -s /usr/sbin/nologin probe || echo "警告：创建用户失败，继续（将以现有用户运行）" >&2
	elif command -v adduser >/dev/null 2>&1; then
		adduser -S -H -D probe 2>/dev/null || adduser -S -H probe 2>/dev/null || \
			warn "创建用户失败，继续"
	else
		warn "找不到 useradd/adduser，将以 root 运行（建议手工建 probe 用户）"
	fi
fi
mkdir -p "$DIR" "$DATA" "$DATA/themes"
chmod 0750 "$DIR"
# 数据目录必须 0700：里面有 master.key（告警渠道凭证的主密钥）与数据库。
chmod 0700 "$DATA"
if id -u probe >/dev/null 2>&1; then
	chown -R probe:probe "$DATA" 2>/dev/null || chown -R probe "$DATA" 2>/dev/null || true
fi

say "2/7 安装二进制"
if [ -n "$BIN_SRC" ]; then
	# 只读根文件系统（部分 LXC 模板、distroless）上 /usr/local/bin 不可写，
	# 用 --bin-dir 指到可写目录即可——systemd 单元里的 ExecStart 会跟着走。
	mkdir -p "$BIN_DIR" || { echo "错误：无法创建 $BIN_DIR（只读根文件系统？用 --bin-dir 指到可写目录）。" >&2; exit 2; }
	if [ "$BIN_SRC" = "$BIN_DIR/xb-probe-master" ]; then
		# 源与目标同一个文件：install/cp 都会报 "are the same file" 并以非 0 退出，
		# 在 set -e 下直接把整个部署打断，而错误信息完全看不出发生了什么。
		echo "    源与目标相同（$BIN_SRC），跳过拷贝"
	elif command -v install >/dev/null 2>&1; then
		install -m 0755 "$BIN_SRC" "$BIN_DIR/xb-probe-master"
	else
		cp -f "$BIN_SRC" "$BIN_DIR/xb-probe-master" && chmod 0755 "$BIN_DIR/xb-probe-master"
	fi
	"$BIN_DIR/xb-probe-master" version || true
fi

# _cfg_scalar <文件> <缩进> <键>：从 master.yaml 里取一个带引号的标量值。
# 只服务下面"本次回答 vs 现有配置"的逐项比对——配置由本脚本生成、格式固定，
# 用不着完整的 YAML 解析。取不到（键不存在）返回空串。
_cfg_scalar() {
	sed -n "s/^$2$3:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$1" 2>/dev/null | head -n1
}

say "3/7 写配置文件 $CFG"
if [ -f "$CFG" ] && [ -z "$FORCE_CONFIG" ]; then
	# 不能一句"已存在，保留"就完事：保留的前提是**旧配置与本次回答说的是同一套**。
	# v0.1.3 真机连着栽在这里——旧配置是 acme+旧域名，这一轮问答里改成了
	# selfsigned/新域名，配置却原样保留，于是：
	#   · 摘要里打印的是新答案，实际跑的是旧配置（"我明明选了自签"）；
	#   · 服务按旧模式起，问答改的部分全部没生效，还看不到任何报错；
	#   · 旧配置若带 sync_obtain: true，服务会在申请失败时直接退出（反复重启），
	#     而 80/443 都不在听——用户只能靠 netstat 才能发现。
	# 所以逐项比对：不一致就按本次回答重写（旧文件备份 .bak）；一致才保留。
	_cfg_diff=""
	_o_publ="$(_cfg_scalar "$CFG" '' public_listen)"
	_o_adml="$(_cfg_scalar "$CFG" '' admin_listen)"
	_o_pubo="$(_cfg_scalar "$CFG" '' public_origin)"
	_o_admo="$(_cfg_scalar "$CFG" '' admin_origin)"
	_o_mode="$(_cfg_scalar "$CFG" '  ' mode)"
	_o_chal="$(_cfg_scalar "$CFG" '    ' challenge)"
	if [ "$_o_publ" != "$PUBLIC_LISTEN" ]; then
		_cfg_diff="$_cfg_diff
      · 公开监听：${_o_publ:-（无）} → $PUBLIC_LISTEN"
	fi
	if [ "$_o_adml" != "$ADMIN_LISTEN" ]; then
		_cfg_diff="$_cfg_diff
      · 管理监听：${_o_adml:-（与公开站同一个监听）} → ${ADMIN_LISTEN:-（与公开站同一个监听）}"
	fi
	if [ "$_o_pubo" != "$PUBLIC_ORIGIN" ]; then
		_cfg_diff="$_cfg_diff
      · 公开地址：${_o_pubo:-（无）} → ${PUBLIC_ORIGIN:-（无）}"
	fi
	if [ "$_o_admo" != "$ADMIN_ORIGIN" ]; then
		_cfg_diff="$_cfg_diff
      · 管理地址：${_o_admo:-（无）} → ${ADMIN_ORIGIN:-（无）}"
	fi
	if [ "$_o_mode" != "$TLS_MODE" ]; then
		_cfg_diff="$_cfg_diff
      · 取证书方式：${_o_mode:-（无）} → $TLS_MODE"
	fi
	if [ "$TLS_MODE" = "acme" ] && [ "$_o_chal" != "$ACME_CHALLENGE" ]; then
		_cfg_diff="$_cfg_diff
      · ACME 校验方式：${_o_chal:-（无）} → $ACME_CHALLENGE"
	fi
	if [ -z "$_cfg_diff" ]; then
		echo "    已存在且与本次回答一致，保留（要覆盖请加 --force-config）"
	else
		echo "    已存在，但与本次回答不一致：$_cfg_diff"
		echo "    按本次回答重写（旧配置备份为 $CFG.bak）。"
		cp -f "$CFG" "$CFG.bak" 2>/dev/null || true
		render_config >"$CFG"
		chmod 0640 "$CFG"
		echo "    已写入"
	fi
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

# 落盘之后**立刻**用二进制自己验一遍。
#
# 这一层是给"保留旧配置"那条路径准备的：上一轮写坏的配置（含二进制不认识的键）
# 会被这一轮原样保留，若在这里不拦，用户要等输完口令、服务起不来、回滚之后
# 才看到原因——而回滚给的理由是"启动后立即退出"，离真正的原因很远。
# 返回 2（判断不了，例如旧二进制没有 tls 子命令）时**什么都不做**：
# 不猜、不误报，交给第 4 步 init-admin 的输出兜底（那条对任何版本都有效）。
#
# ⚠ 调用必须放进 AND-OR 列表（`|| _cp_rc=$?`），不能裸调用。
# 两个理由，任一都足以致命：
#   ① 本脚本开头是 `set -eu`。裸调用时 -e **在函数内部也生效**，而函数体里那句
#      `_CP_OUT="$(tls status …)"` 就是"只有赋值的简单命令"——tls 状态非 0
#      （rc=1 申请失败 / rc=2 申请中）时它会**在函数里**把整个脚本干掉，
#      下面这个 case 与 fail_config_mismatch 永远不会执行；
#   ② 函数返回 1（配置与二进制错配）本来就要靠 `$?` 来接，裸调用同样先被 -e 杀掉。
# 放进 AND-OR 列表后 -e 被忽略（POSIX：-e 被忽略的上下文里，其内部任何命令都不
# 导致 shell 退出），返回值也照常拿得到。
_cp_rc=0
config_parses_ok "$CFG" || _cp_rc=$?
case "$_cp_rc" in
1) fail_config_mismatch "$_CP_OUT" ;;
esac

say "4/7 初始化管理员"
if [ -z "$ADMIN_PASSWORD" ]; then
	# 交互路径下口令在问答段就已经问过（用户名 → 口令 → 确认的顺序），走到这里
	# 必然是**没问成**的几种情况：--yes / 非交互忘了给 PROBE_ADMIN_PASSWORD、
	# 或用户在问答里三次都输了空口令。所以这里是唯一的判处点，也是最后的机会。
	# 与 ask_secret 同一个道理：这里也**不用** read -e（readline 会重新打开回显），
	# 但提示仍然要打在光标所在处、读完后补一个换行，否则下一条输出会接在这一行后面。
	printf '管理员 %s 的口令（不回显）：' "$ADMIN_USER" >&2
	stty -echo 2>/dev/null || true
	IFS= read -r ADMIN_PASSWORD || ADMIN_PASSWORD=""
	stty echo 2>/dev/null || true
	printf '\n' >&2
fi
if [ -z "$ADMIN_PASSWORD" ]; then
	err "口令不能为空。"
	exit 2
fi
# init-admin 在管理员已存在时会报错退出，这属于预期（幂等升级），所以不用
# `set -e` 硬中断——但**必须拿到它真实的退出码**，并且把"配置解析失败"这一类
# 与"管理员已存在"区分开。
#
# ⚠ 这里曾经写成 `cmd 2>&1 | sed 's/^/    /' || echo "（管理员已存在，跳过）"`。
# 管道的退出码是**最后一个命令**（sed）的，永远是 0，于是 `||` 那半句永不执行、
# init-admin 的任何失败都被静默吞掉：脚本继续往下写 systemd 单元并启动服务，
# 服务必然起不来，最后以"启动失败"回滚——用户看到的失败原因离真正的原因很远
# （真机日志里，第 4 步已经打印了 `field sync_obtain not found`，脚本却照常往下走）。
# 所以：先把输出收进变量再自己判，不要让管道吃掉退出码。
#
# ⚠ 但"收进变量"本身还不够——**必须同时放进 AND-OR 列表**（`|| _ia_rc=$?`）。
# 本脚本开头是 `set -eu`，而"只有赋值的简单命令"（`x="$(cmd)"`）的退出码**就是**
# 命令替换的退出码：init-admin 返回非 0 时（最典型的是"管理员已存在"这个**预期内**
# 的幂等分支），`set -e` 会在这一行把整个脚本干掉——下面那句 printf、`已存在`
# 判定、错误提示与 rollback 全都不执行。
# 真机现象（v0.1.3 重跑：上一次装到一半失败回过滚，库里已经有一个 admin）：
# 第 4 步打完口令提示后**一行输出都没有**、也没有回滚，只剩入口脚本一句
# "部署脚本执行失败"——看上去像"口令不对"，其实是脚本自己把自己杀了。
_ia_rc=0
_ia_out="$("$BIN_DIR/xb-probe-master" init-admin -config "$CFG" \
	-username "$ADMIN_USER" -password "$ADMIN_PASSWORD" 2>&1)" || _ia_rc=$?
printf '%s\n' "$_ia_out" | sed 's/^/    /'

# 配置里有当前二进制不认识的键。这一类必须**立刻停下**并点名是哪个键：
# 继续往下走的唯一结果就是"服务启动失败 → 回滚"，而回滚的原因写成
# "启动后立即退出"，把真正的原因（版本错配）藏起来了。
#
# 根因是两边来源不同：二进制来自 Release 标签、配置模板来自 main，
# 于是脚本会写出二进制还不认识的键，而 LoadMaster 是严格解析（未知键直接拒绝启动）。
# 探测（binary_supports_config_key）正常情况下会拦住这些键，但**旧二进制连探测
# 用的子命令都没有**，探测只能按"不认识"降级——所以这一层兜底是必需的。
case "$_ia_out" in
*"解析配置失败"* | *"unmarshal"* | *"not found in type"*)
	# 与第 3 步共用同一份诊断：原因与修法完全一样，各写一份迟早有一份忘了更新。
	fail_config_mismatch "$_ia_out"
	;;
esac

if [ "$_ia_rc" -ne 0 ]; then
	case "$_ia_out" in
	*"已存在"*)
		# 幂等升级时管理员已经建过了，这是预期内的，继续。
		echo "    （管理员已存在，跳过；改口令用 xb-probe-master reset-admin）"
		;;
	*)
		echo >&2
		err "初始化管理员失败（上面是 xb-probe-master 的输出）。"
		rollback "初始化管理员失败"
		;;
	esac
fi
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
	say "5~7 跳过（--no-service）"
	case "$TLS_MODE" in
	none) _ns_scheme="http" ;;
	*) _ns_scheme="https" ;;
	esac
	echo "    前台运行：$BIN_DIR/xb-probe-master -config $CFG"
	echo
	# 与收尾那段**共用**同一份推导：两处各写一遍的下场是"改一处＝没改"，
	# 而这两处印的是同一个东西（后台入口地址），措辞一致才不会被当成两个结论。
	resolve_admin_entry "$_ns_scheme"
	echo "  公开看板    ${PUBLIC_ORIGIN:-$_ns_scheme://$PUBLIC_LISTEN}"
	echo "  管理后台    $ADMIN_URL"
	if [ -n "$ADMIN_NOTE" ]; then echo "              $ADMIN_NOTE"; fi
	echo
	exit 0
fi

say "5/7 写 systemd 单元"
UNIT=/etc/systemd/system/xb-probe-master.service
if command -v systemctl >/dev/null 2>&1 && [ -d /etc/systemd/system ]; then
	# 自己终结 TLS 时（acme / selfsigned / files），进程要绑 443；challenge 选
	# http-01 时还要绑 80。**这两个都是特权端口**（内核
	# net.ipv4.ip_unprivileged_port_start 默认 1024），而服务以 probe 用户运行，
	# 不授予这项能力就恒 `bind: permission denied`。
	#
	# 真机（腾讯云，v0.1.3 → v0.1.4）上用户报的就是这个：80 端口一直起不来、
	# 证书因此申请失败。他先怀疑防火墙，但机器上 80 完全空闲、防火墙全开——
	# 拦住它的是这一行缺失的能力位。而失败的申请每次都会消耗一次 CA 的
	# "失败授权"额度，反复重试之后现象就变成了 "rateLimited"。
	#
	#   · NoNewPrivileges=true **不**妨碍它：Ambient 能力由 systemd 在 exec
	#     之前赋予，属于"启动时就有的"，不是运行中通过 setuid/文件能力获取的；
	#   · 只留这一项（CapabilityBoundingSet）比留全部更紧。
	#
	# 文案里刻意不写反引号：这段字符串会被展开进一个**不加引号**的 heredoc，
	# 而那里的反引号会被当命令替换执行（同一个坑在本脚本的收尾提示里踩过一次）。
	_cap_block=""
	if [ "$TLS_MODE" != "none" ]; then
		_cap_block="# 绑定低位端口（443；http-01 还要 80）。没有它，probe 用户绑不上 80，
# 现象是「申请证书期间 80 一直没起来」——与防火墙无关。
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE"
	fi
	cat >"$UNIT" <<EOF
[Unit]
Description=XB-Probe Master
After=network-online.target
Wants=network-online.target
# tls.acme.sync_obtain 打开时，「申请不成功」表现为进程**故意**退出（好让部署
# 脚本拿到确定结果并回滚），而 Restart=always 会每 3 秒把它拉起来再申请一次。
# 每次失败都算一次"失败授权"，Let's Encrypt 的额度是每标识符每账号每小时 5 次
# ——打满之后连一次**正确**的尝试都做不了。所以连续失败要停下来等人工介入。
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
ExecStart=$BIN_DIR/xb-probe-master -config $CFG
Restart=always
RestartSec=3
User=probe
Group=probe
$_cap_block
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
	systemctl enable xb-probe-master >/dev/null 2>&1 || true
	if ! systemctl restart xb-probe-master; then
		# 启动失败必须回滚。最典型的一种是 tls.mode=acme 且开了 sync_obtain：
		# 证书申请不成功时服务**故意**起不来，而这是部署阶段，用户正等着结果。
		# 不回滚的话系统里会留一个反复重启失败的服务，而脚本已经"完成"了。
		systemctl --no-pager status xb-probe-master 2>/dev/null | sed -n '1,10p' >&2 || true
		journalctl -u xb-probe-master --no-pager -n 20 2>/dev/null | sed 's/^/    /' >&2 || true
		rollback "xb-probe-master 服务启动失败（日志见上）"
	fi
	echo "    已启用并启动"
	# Type=simple 下 systemctl restart 一返回就认为"成功"，而进程可能已经开始
	# 申请证书、也可能**立刻**因为别的原因退出（端口被占、配置键写错）。
	# 所以多给一秒再确认一次它还活着——把"服务根本没起来"这件事在回滚点上
	# 抓住，而不是等到后面的健康检查超时 20 秒再报一句含糊的"没探到公开 API"。
	sleep 1
	if ! systemctl is-active --quiet xb-probe-master; then
		systemctl --no-pager status xb-probe-master 2>/dev/null | sed -n '1,10p' >&2 || true
		journalctl -u xb-probe-master --no-pager -n 20 2>/dev/null | sed 's/^/    /' >&2 || true
		rollback "xb-probe-master 启动后立即退出（日志见上；端口被占用、配置键写错都会这样）"
	fi
else
	warn "没有 systemd（Alpine/OpenRC 或容器），请自行用进程管理器托管："
	echo "      $BIN_DIR/xb-probe-master -config $CFG" >&2
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
	say "5b/7 安装 Caddy 反代（证书由 Caddy 自动申请与续期）"
	if ! install_caddy; then
		rollback "Caddy 安装失败"
	fi
	mkdir -p "$CADDY_DIR" 2>/dev/null || true
	# 已有 Caddyfile 怎么办？这里**不能**只看"文件存在"就保留。
	# 本机 Caddy 很可能本来就是别人（发行版包、x-ui 面板、以前的 nginx 迁移）装的，
	# 那份配置里根本没有我们要反代的域名——保留它的结果不是"尊重既有配置"，
	# 而是**Caddy 没有 443 站点、不开 443、也拿不到证书**，用户看到的却是
	# "反代已配好、部署完成"。所以判据换成：里面**有没有本次要用的那个域名**。
	# 已有 Caddyfile 怎么办？这里**不能**只看"文件存在"就保留，也不能只看
	# "有没有目标域名的站点块"就保留——后者漏掉了最关键的一种情况：
	# **那个块是上一版脚本自己写的**。
	#
	# 真机踩过：上一版脚本写过 `mo.319082.xyz` 的站点块（那时还没有 /admin 分流），
	# 升级时判据只说"已有该域名的站点块 → 保留"，于是本轮修复根本没生效——
	# 用户看到的现象是"脚本说部署完成，/admin/ 还是 404 page not found"，
	# 而且 Caddyfile 里明明有一个看起来完全正常的站点块。
	#
	# 所以判据是**这个块是谁写的**：
	#   ① 文件里有我们的指纹（CADDY_MARK 段，或整份生成时的 CADDY_HEAD）
	#      → 摘掉我们写的那一段，写入当前版本生成的内容（升级要让修复生效）；
	#   ② 没有我们的痕迹、但有同名站点块 → 别人写的，保留并说明；
	#   ③ 没有我们的痕迹、也没有同名站点 → 追加（保留别人的站点）；
	#   ④ 文件不存在 → 直接写。
	_caddy_dom="$(render_caddyfile | sed -n 's/^\([^ {]*\) {.*/\1/p' | head -n1)"
	Caddyfile_has_domain() {
		[ -f "$CADDYFILE" ] || return 1
		[ -z "${1:-}" ] && return 1
		# 只匹配行首的站点标签（"example.com {" 或 "example.com:443 {"），
		# 不匹配注释里顺嘴提到的域名——否则一句注释就能让判据失效。
		grep -Eq "^[[:space:]]*$1(:[0-9]+)?[[:space:]]*\{" "$CADDYFILE" 2>/dev/null
	}
	# 文件里有没有**我们写的**内容。
	_caddy_ours=""
	if [ -f "$CADDYFILE" ]; then
		if grep -qF "$CADDY_MARK" "$CADDYFILE" 2>/dev/null || grep -qF "$CADDY_HEAD" "$CADDYFILE" 2>/dev/null; then
			_caddy_ours="1"
		fi
	fi
	if [ ! -f "$CADDYFILE" ]; then
		render_caddyfile >"$CADDYFILE"
		chmod 0644 "$CADDYFILE"
		echo "    已写入 $CADDYFILE"
	elif [ -n "$_caddy_ours" ]; then
		# 我们自己写的（可能是**上一版**写的）：摘掉旧段、写入新段。
		# 这是"升级后修复真的生效"的关键一步。
		cp -f "$CADDYFILE" "$CADDYFILE.bak" 2>/dev/null || true
		# 摘除分两种历史形态，规则**不能混用**（混用会打架）：
		#   · 有 CADDY_END → 是"追加"形态：从 CADDY_MARK（或 CADDY_HEAD）到
		#     CADDY_END 精确摘除。这里**不能**再看行首 `}`——站点块收尾的 `}`
		#     会先结束跳过，紧跟其后的 END 行就残留下来了（实测踩过）。
		#   · 没有 CADDY_END → 是"整份生成"形态（上一版的写法：头部一行
		#     CADDY_HEAD + 站点块，没有结束标记）。这种文件整份都是我们写的，
		#     直接按当前版本重写最可预测；试图用 `}` 规则去摘会在**分离形态**
		#     （两个站点块）下摘不干净，留下重复的站点定义让 Caddy 起不来。
		if grep -qF "$CADDY_END" "$CADDYFILE" 2>/dev/null; then
			awk -v mark="$CADDY_MARK" -v head="$CADDY_HEAD" -v end="$CADDY_END" '
				$0 == mark || $0 == head { skip = 1; next }
				skip && $0 == end { skip = 0; next }
				!skip { print }
			' "$CADDYFILE" >"$CADDYFILE.tmp" 2>/dev/null || true
			mv -f "$CADDYFILE.tmp" "$CADDYFILE" 2>/dev/null || true
			if grep -Eq "^[[:space:]]*[^#{[:space:]]" "$CADDYFILE" 2>/dev/null; then
				# 摘完之后还有别人的站点：追加我们这一版，并用首尾标记界定。
				{
					printf '\n%s\n' "$CADDY_MARK"
					render_caddyfile
					printf '%s\n' "$CADDY_END"
				} >>"$CADDYFILE"
				echo "    已更新 $CADDYFILE 里本脚本写的站点块（旧内容备份为 $CADDYFILE.bak）"
			else
				render_caddyfile >"$CADDYFILE"
				echo "    已按当前版本重写 $CADDYFILE（旧内容备份为 $CADDYFILE.bak）"
			fi
		else
			# "整份生成"形态（上一版的写法：头部一行 CADDY_HEAD + 站点块，
			# 没有结束标记）。这种文件**整份都是我们写的**，按当前版本重写是
			# 最可预测的做法。
			#
			# 为什么不用"以行首 `}` 为界摘除"：旧版在**分离形态**下会写两个
			# 站点块（公开域名 + 管理域名），而 CADDY_HEAD 只有开头那一个。
			# 摘除会在第一个 `}` 处停下，第二个块残留下来——结果是配置里同时
			# 有旧的管理域名块与新生成的块，**重复的站点定义会让 Caddy 直接
			# 起不来**。半个配置比"用户自己加的站点被覆盖"严重得多，所以这里
			# 一律重写；真有人在里面加过自己的站点，原文件在 $CADDYFILE.bak。
			render_caddyfile >"$CADDYFILE"
			echo "    已按当前版本重写 $CADDYFILE（旧内容备份为 $CADDYFILE.bak）"
			echo "      这次会带上按路径分流的规则（/admin/ 与 /api/admin/ 转给管理端）。"
		fi
		chmod 0644 "$CADDYFILE"
	elif [ -n "$FORCE_CONFIG" ]; then
		if [ -f "$CADDYFILE" ]; then cp -f "$CADDYFILE" "$CADDYFILE.bak" 2>/dev/null || true; fi
		render_caddyfile >"$CADDYFILE"
		chmod 0644 "$CADDYFILE"
		echo "    已写入 $CADDYFILE（--force-config）"
	elif Caddyfile_has_domain "$_caddy_dom"; then
		echo "    $CADDYFILE 已有 $_caddy_dom 的站点块（**不是本脚本写的**），保留。"
		echo "      若那个块不是你要的，加 --force-config 覆盖。"
	elif grep -Eq "^[[:space:]]*[^#{]" "$CADDYFILE" 2>/dev/null; then
		# 有文件但没有目标域名、也没有我们的痕迹：里面可能是别人的站点
		# （这台机器上就常见），追加而不是整份覆盖。
		cp -f "$CADDYFILE" "$CADDYFILE.bak" 2>/dev/null || true
		{
			printf '\n%s\n' "$CADDY_MARK"
			render_caddyfile
			printf '%s\n' "$CADDY_END"
		} >>"$CADDYFILE"
		chmod 0644 "$CADDYFILE"
		echo "    $CADDYFILE 里没有 $_caddy_dom 的站点块 → 已追加（原内容备份为 $CADDYFILE.bak）"
	else
		if [ -f "$CADDYFILE" ]; then cp -f "$CADDYFILE" "$CADDYFILE.bak" 2>/dev/null || true; fi
		render_caddyfile >"$CADDYFILE"
		chmod 0644 "$CADDYFILE"
		echo "    $CADDYFILE 原内容无有效站点 → 已写入（备份为 $CADDYFILE.bak）"
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
Description=Caddy (XB-Probe 反向代理)
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

# site_gate_advice <HTTP 码> <域名>：把"服务端**已经答复**了、只是状态不对"讲清楚。
#
# 这一类的共同点：不是连不上、也不是证书没拿到，而是服务端明确拒绝了这次请求。
# 重试一分钟也不会变——所以每个撞上它的地方都当场停手，不做无谓等待。
# 真机（v0.1.3）就是在这里空等了 60 秒，最后只拿到四条猜测；而事实是
# 站点形态按**域名**门控，配置里生效的公开域名不是用户正在用的那个。
#
# 三个参数：<HTTP 码> <域名> <响应体>。响应体那一项是本轮真机逼出来的——
# 同一个 404 至少有三种完全不同的来源（看板被关掉的 JSON、Go 原生没注册路由的
# 纯文本、本机代理自己答的 HTML），不看原话就只能猜，而这三条的修法毫不相干。
#
# 出口：环境变量 `_DGA_FATAL`
#   "1"（默认）—— 这次安装没达成目的，调用方应当回滚；
#   "0"         —— 能归因为**运行期状态**（看板开关、代理、端口被占），
#                  调用方只警告、**不要撤销**这次本来成功的安装。
site_gate_advice() {
	_dga_code="${1:-}"
	_dga_dom="${2:-}"
	_dga_body="${3:-}"
	_DGA_FATAL="1"
	echo "  请求已经走完网络、进了服务端，只是被**拒**了——所以这不是证书或端口问题，" >&2
	echo "  再等下去也不会变，原因在下面这几类里（按响应体的原话对号入座）：" >&2
	case "$_dga_code" in
	421)
		echo "  · HTTP 421 = 域名不在白名单。站点形态是按域名门控的，$_dga_dom 不在" >&2
		echo "    当前生效的公开域名清单里——那样**所有**请求都会被拒（含本机自检）。" >&2
		echo "    最常见的原因是：本机 $CFG 是**上一次安装留下的**。第 3 步对已存在的" >&2
		echo "    配置只做「保留」，所以你在问答里填的新域名根本没写进去。" >&2
		echo "    先看它现在记的是什么：" >&2
		echo "      grep -nE 'public_origin|public_hosts|admin_origin|admin_hosts' $CFG" >&2
		echo "    修法（任选其一）：" >&2
		echo "      ① 按本次答案重写配置后重跑本脚本：加 --force-config" >&2
		echo "         （旧配置会先备份成 $CFG.bak）" >&2
		echo "      ② 进后台「系统设置 → 站点形态」把公开域名改成 $_dga_dom ——" >&2
		echo "         但域名被拒时后台同样打不开，这条路通常走不通。" >&2
		;;
	403 | 404)
		# 404 有三种来源，修法毫不相干，所以必须按**响应体的原话**分：
		#   · Master 自己的 JSON（"公开接口已关闭"）—— 安装是好的，是开关的事；
		#   · Go 原生的 "404 page not found"      —— 这条路根本没注册，属装错了；
		#   · 两者都不是（HTML、代理的错误页）    —— 这个 404 不是 Master 回的。
		case "$_dga_body" in
		*"公开接口已关闭"*)
			_DGA_FATAL="0"
			echo "  · 响应体是 Master 自己的话「公开接口已关闭」——**安装是对的**，" >&2
			echo "    是「公开看板」这个开关被关掉了（数据库设置项 public.enabled 不是 1）。" >&2
			echo "    打开它：浏览器进后台（反代会把 /admin/ 与 /api/admin/ 转给管理端，" >&2
			echo "    公开站关着也不影响后台），在「系统设置 → 公开看板」里设为「开放」。" >&2
			echo "    注意：它不是 master.yaml 里的字段，重写配置（--force-config）改不了它。" >&2
			;;
		*"page not found"*)
			echo "  · 响应体是 Go 原生的「404 page not found」= 这条路**根本没注册**。" >&2
			echo "    正常部署不会出现（脚本与 v0.1.3 二进制都有 /api/public/v1/site），" >&2
			echo "    查这两处：① 到底是谁在听 $HEALTH_ADDR（见下面「现场证据」）；" >&2
			echo "    ② 装上去的二进制是不是这个版本的：$BIN_DIR/xb-probe-master version" >&2
			;;
		*)
			echo "  · 响应体既不是 Master 的 JSON、也不是 Go 原生的 404 —— 那这个 $_dga_code" >&2
			echo "    大概率**不是 Master 回的**（中间还站着别的进程：本机代理、旧版布局的" >&2
			echo "    probe-master 服务，或别的程序占着这个端口）。证据见下面「现场证据」。" >&2
			;;
		esac
		;;
	*)
		echo "  · HTTP $_dga_code：去服务端日志找原因——" >&2
		echo "      journalctl -u xb-probe-master --no-pager -n 50" >&2
		;;
	esac
	return 0
}

# probe_scene_evidence：把"这个 4xx 到底是谁回的"一次问清。
#
# 三条证据合起来能把最常见的三种误判分开：
#   · 谁在听这个端口 —— 旧版布局的 probe-master 与别的程序会占同一个端口，
#     于是"Master 拒绝服务"其实是"另一个进程在答"；
#   · 环境里有没有代理 —— 有代理时 curl 会把连回环的请求交给它，
#     状态码与响应体都变成代理的（脚本已用 --noproxy '*' 绕开，这里只做回显）；
#   · 系统里还有哪些 probe 单元 —— 遗留服务的名字与状态一眼可见。
# 最后附上 Master 自己的启动日志：它会打印**最终生效**的站点形态与域名清单，
# 后台设置项顶替配置文件这件事只有这行日志能证明。
probe_scene_evidence() {
	_hpe_addr="${1:-$HEALTH_ADDR}"
	_hpe_port="${_hpe_addr##*:}"
	echo "  现场证据（把「其实是别人回的」与「服务里没这个路由」分开）：" >&2
	echo "    · 谁在听 $_hpe_addr：" >&2
	_hpe_l=""
	if command -v ss >/dev/null 2>&1; then
		_hpe_l="$(ss -lntp 2>/dev/null)" || true
	elif command -v netstat >/dev/null 2>&1; then
		_hpe_l="$(netstat -ntpl 2>/dev/null)" || true
	fi
	if [ -z "$_hpe_l" ]; then
		echo "        （本机没有 ss / netstat，查不了）" >&2
	else
		_hpe_row="$(printf '%s\n' "$_hpe_l" | grep -E "[:.]$_hpe_port([^0-9]|$)" || true)"
		if [ -n "$_hpe_row" ]; then
			printf '%s\n' "$_hpe_row" | sed 's/^/        /' >&2
		else
			echo "        （没有进程在听 $_hpe_port）" >&2
		fi
	fi
	echo "    · 环境里的代理变量（脚本的探测已用 --noproxy '*' 绕开）：" >&2
	_hpe_px="$(env | grep -iE '^(http|https|all|ftp)_proxy=' || true)"
	if [ -n "$_hpe_px" ]; then
		printf '%s\n' "$_hpe_px" | sed 's/^/        /' >&2
	else
		echo "        （无）" >&2
	fi
	echo "    · 系统里与本项目有关的单元：" >&2
	_hpe_u="$(ls /etc/systemd/system/ 2>/dev/null | grep -i probe || true)"
	if [ -n "$_hpe_u" ]; then
		printf '%s\n' "$_hpe_u" | sed 's/^/        /' >&2
		_hpe_u2="$(systemctl list-units --all --no-legend --no-pager 2>/dev/null | grep -i probe || true)"
		if [ -n "$_hpe_u2" ]; then
			printf '%s\n' "$_hpe_u2" | sed 's/^/        /' >&2
		fi
	else
		echo "        （无）" >&2
	fi
	echo "    · Master 启动日志（含最终生效的站点形态与域名清单）：" >&2
	journalctl -u xb-probe-master --no-pager -n 15 2>/dev/null | sed 's/^/        /' >&2 || true
	return 0
}

say "6/7 健康检查"
# 公开站 /site 是免鉴权的，用它确认进程真的在听；最多等 20 秒。
#
# 配了 TLS 就必须用 https 探：明文探一个 TLS 端口永远失败，会白等 20 秒再打印
# 一段让人以为服务没起来的状态。
# selfsigned 用 -k 是**有意**的：这里问的是"进程在不在听"，不是"证书链可不可信"，
# 两者混在一起会让自签部署次次看起来像失败。acme 同理——首张证书由服务前台申请，
# 此刻要么已经拿到、要么服务没起来，证书链验证失败并不代表服务没起来。
#
# 反代在前的部署还必须**带上对外域名当 Host**：站点形态是按域名门控的，
# 拿 127.0.0.1 当 Host 会被判 421。以前这里正是这样——于是这个"健康检查"
# 永远看不出服务到底行不行，只是白等 20 秒再打印一段状态（v0.1.3 真机上
# 它就把一次配置错配完整地掩盖了过去）。
case "$TLS_MODE" in
none) SCHEME="http"; CURL_EXTRA="" ;;
files) SCHEME="https"; CURL_EXTRA="" ;;
selfsigned | acme) SCHEME="https"; CURL_EXTRA="-k" ;;
esac
HC_HOST=""
if [ "$TLS_TERMINATOR" = "caddy" ] && [ -n "${_cd_dom:-}" ]; then
	HC_HOST="$_cd_dom"
fi
# 探测一律 **--noproxy '*'**：设了 http_proxy / https_proxy 的机器上，curl 会把
# 连回环的请求交给代理，于是"服务端回了什么"变成"代理回了什么"——状态码与响应体
# 全是代理的。这条探的是**服务端本人**，必须直连回环。
i=0
_hc_code=""
_hc_body=""
while [ $i -lt 20 ]; do
	if command -v curl >/dev/null 2>&1; then
		_hc_rc=0
		if [ -n "$HC_HOST" ]; then
			_hc_raw="$(curl -sS --max-time 5 --noproxy '*' $CURL_EXTRA -H "Host: $HC_HOST" \
				-o - -w '\n%{http_code}' "${SCHEME}://${HEALTH_ADDR}/api/public/v1/site" 2>/dev/null)" || _hc_rc=$?
		else
			_hc_raw="$(curl -sS --max-time 5 --noproxy '*' $CURL_EXTRA \
				-o - -w '\n%{http_code}' "${SCHEME}://${HEALTH_ADDR}/api/public/v1/site" 2>/dev/null)" || _hc_rc=$?
		fi
		if [ "$_hc_rc" != "0" ]; then
			_hc_code="" # 连接层没通：服务可能还在起（下面的 case 按空串处理）
			_hc_body=""
		else
			# 末行是 -w 打出来的状态码，其余是**服务端的原话**。
			# 以前这里 -o /dev/null 把原话整个丢掉，只剩一个状态码，于是
			# 同一个 404 的三种来源（看板被关掉 / 路由没注册 / 代理自己答的）
			# 完全分不开，只能靠猜——真机就是这么被挡住三轮的。
			_hc_code="$(printf '%s\n' "$_hc_raw" | tail -n 1)"
			_hc_body="$(printf '%s\n' "$_hc_raw" | sed '$d' | tr -d '\r\n' | cut -c1-300)"
		fi
		case "$_hc_code" in
		2*) break ;;
		"") : ;; # 连接层没通：服务可能还在起，继续等
		*) break ;; # 已经答复了但状态不对：等也没用，交给下面判
		esac
	elif command -v wget >/dev/null 2>&1; then
		# wget 在自签/未签发证书下必须 --no-check-certificate，否则同样白等 20 秒。
		if wget -q --no-check-certificate -O- "${SCHEME}://${HEALTH_ADDR}/api/public/v1/site" >/dev/null 2>&1; then break; fi
		_hc_code=""
	else
		break
	fi
	i=$((i + 1))
	sleep 1
done
systemctl --no-pager status xb-probe-master | sed -n '1,8p' || true
# 非 2xx 是**确定性的拒绝**，不是"还没起来"：当场停手，并把断点说清楚。
# 以前这里只是白等 20 秒，然后交给后面那步 60 秒超时去猜——两次都猜错了方向。
#
# 两条原则：
#   ① 只有"探的 Host 就是我们要对外发布的那个域名"时才敢据此判失败（HC_HOST 非空）。
#      其余情形（自签/ACME 由 Master 自己终结 TLS，探的是回环地址）里，拿回环 Host
#      去问一个按域名门控的服务，本来就会被判"域名不对"——那是探测方式的偏差，
#      不是部署坏了，所以只提示、不回滚。
#   ② 判据问的是「这次安装装出来的东西对不对」，不是「你的机器上还有没有别的东西
#      挡着」。能归因为运行期状态的（看板开关被关了、端口被别的进程占着、请求走了
#      代理）只警告；拿它回滚会把一次本来成功的安装整份撤掉（连 caddy 一起停掉），
#      站点反而被脚本自己弄下线。
case "$_hc_code" in
2*) : ;;
"" | 0*)
	# 20 秒全是连接层失败。按模式分两种，混成一种会把真话报成假话：
	#   · acme（Master 自己终结 TLS）：可能只是"首张证书还没就绪，443 暂时握不上
	#     手"——服务是好的。继续走证书步骤，由它给出确定结果；
	#   · 其余模式（none / files / selfsigned，含 caddy 反代）：不存在"证书还没好"
	#     这回事——连不上就是服务没起来或监听地址不对。v0.1.3 真机上这里曾把一次
	#     "服务反复退出"（配置与问答不一致导致按旧模式起、起不来）静默放过，
	#     部署照样报告成功，用户是拿 netstat 才发现 443 根本没在听。
	if [ "$TLS_MODE" = "acme" ] && [ "$TLS_TERMINATOR" != "caddy" ]; then
		echo "    注：20 秒内连不上 $HEALTH_ADDR（连接层不通）。acme 模式下这" >&2
		echo "      可能只是首张证书还没就绪（443 暂时握不上手），继续走下面的" >&2
		echo "      证书步骤，由它给出确定结果。若服务其实在反复重启，查：" >&2
		echo "        systemctl status xb-probe-master; journalctl -u xb-probe-master -n 50" >&2
		# http-01 还要看 80：CA 是**从公网**访问这个端口来验的，它没在听就等于
		# 申请一定失败。这是"证书申请失败"里最容易查错方向的一个原因——用户会
		# 先去怀疑防火墙，而真正缺的往往是"绑定低位端口"的能力位（服务以 probe
		# 用户运行，80 < 1024 属于特权端口）。v0.1.4 之前真机就是这样。
		if [ "$ACME_CHALLENGE" = "http-01" ]; then
			echo "      另外：challenge=http-01 要求 Master 自己监听 80（CA 从公网来验它）。" >&2
			echo "      若 80 不在听，先分清是"被占"还是"没权限"：" >&2
			echo "        ss -ltnp | grep ':80'" >&2
			echo "        · 被别的进程占 → 让反代转发 /.well-known/acme-challenge/，" >&2
			echo "          并把 tls.acme.http_addr 设为 off；" >&2
			echo "        · 报 permission denied → 缺 CAP_NET_BIND_SERVICE（手工写的单元" >&2
			echo "          / 直接跑二进制容易漏；本脚本 5/7 步写的单元已包含它）。" >&2
		fi
	else
		echo "" >&2
		err "20 秒内连不上 $HEALTH_ADDR——Master 没有在这个地址上提供服务。"
		echo "      $TLS_MODE 模式下不存在\"证书还没好\"这回事，连不上就是服务没起来：" >&2
		echo "        systemctl status xb-probe-master" >&2
		echo "        journalctl -u xb-probe-master --no-pager -n 50" >&2
		echo "" >&2
		rollback "Master 未在 $HEALTH_ADDR 上提供服务（20 秒连接层不通）"
	fi
	;;
*)
	if [ -n "$HC_HOST" ]; then
		echo "" >&2
		err "Master 用域名 $HC_HOST 访问 /api/public/v1/site 回了 HTTP $_hc_code。"
		if [ -n "$_hc_body" ]; then
			echo "  服务端的原话（先看这行——它通常直接说明了原因）：" >&2
			echo "    $_hc_body" >&2
		fi
		site_gate_advice "$_hc_code" "$HC_HOST" "$_hc_body"
		probe_scene_evidence
		echo "" >&2
		if [ "$_DGA_FATAL" = "1" ]; then
			rollback "Master 拒绝服务这个域名（HTTP $_hc_code）"
		fi
		echo "    ⚠ 上面这条是运行期状态，不是这次安装装坏了什么——" >&2
		echo "      **不撤销本次安装**（二进制、配置、服务与反代都已就位）。" >&2
		echo "      按上面的提示处理完，重跑本脚本即可（幂等）。" >&2
		echo "" >&2
	else
		echo "    注：Master 对 $HEALTH_ADDR 回了 HTTP $_hc_code（本次探测用的是监听地址当 Host）。" >&2
		echo "      站点形态按域名门控的部署里这属正常现象，继续走后面的证书步骤。" >&2
	fi
	;;
esac

say "6/7 证书"
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
	# 判据要挑**真正的那个事实**：Caddy 能为这个域名终结 TLS 并把请求交给 Master。
	# 只查 "caddy 进程活着"是不够的（它当然活着，还开着 80）。
	#
	# ⚠ 探测必须**在本机内闭环**：用 --resolve 把域名指到 127.0.0.1，但 SNI 与
	# Host 仍然是域名。一次问清三件事：证书对这个域名有效、路由指到反代目标、
	# 目标活着。而**不能**用"本机访问自己的公网域名"来判——
	#
	# 站外可达性另算：公网 IP 由机房做映射的主机**回不到自己的公网地址**，
	# 拿"本机访问自己的公网域名"当回滚依据会误杀一次本来成功的部署
	# （站点反而被脚本自己弄下线）。所以那一趟只提示、不回滚。
	# 而回环探测若也失败，说明问题确实在服务端这一侧——那就别猜了，
	# 按下面分段探测的结果说话。

	# _cd_hop <url> [curl 参数…]：探一次，打印 HTTP 码（非 2xx 时附上服务端原话），
	# 连不上则打印 curl 的退出码。分段探测共用它，格式一致，一眼能看出断点在哪层。
	_cd_hop() {
		_cdh_url="$1"
		shift
		_cdh_rc=0
		_cdh_raw="$(curl -sS --max-time 5 -o - -w '\n%{http_code}' "$@" "$_cdh_url" 2>/dev/null)" || _cdh_rc=$?
		_cdh_code="$(printf '%s\n' "$_cdh_raw" | tail -n 1)"
		_cdh_body="$(printf '%s\n' "$_cdh_raw" | sed '$d' | tr -d '\r\n' | cut -c1-160)"
		if [ -z "$_cdh_code" ] || [ "$_cdh_code" = "000" ]; then
			printf '连不上（curl 退出码 %s）' "$_cdh_rc"
		else
			printf 'HTTP %s' "$_cdh_code"
			# `${x#2} = $x` 为真 ⇔ 这个状态码不是 2 开头。用参数展开而不是 case，
			# 免得为一个判断再嵌一层。
			if [ -n "$_cdh_body" ] && [ "${_cdh_code#2}" = "$_cdh_code" ]; then
				printf '  ← %s' "$_cdh_body"
			fi
		fi
		return 0
	}
	# 本机回环探测**不该**经过代理：设了 http_proxy/https_proxy 的机器上，
	# curl 会把请求交给代理，--resolve 也就一并失效——现象是"本机连不上自己的
	# 回环"，而 Caddy 那边一行日志都没有。所以探测一律带 --noproxy '*'。
	_cd_proxy="${https_proxy:-${HTTPS_PROXY:-}}"
	if [ -n "$_cd_proxy" ]; then
		echo "    注：本机设了 https_proxy=$_cd_proxy；回环探测已用 --noproxy '*' 直连，不受它影响。" >&2
	fi
	echo "    正在等待 Caddy 为 $_cd_dom 就绪（最多 60 秒，本机回环探测）…"
	# 连接层与 HTTP 层必须分开判，否则会把两种完全不同的病报成同一句话：
	#   · 连接层没通（curl 到不了 443）——证书可能还在申请，值得等；
	#   · 服务端**已经答复**了但状态不对——配置问题，再等 60 秒也是同一个答案。
	# 真机（v0.1.3）栽的就是第二种：脚本空等 60 秒，然后把"打不开"归因到
	# 证书 / DNS / 端口占用，四条猜测全错。
	_cd_ok=""
	_cd_hf=""
	i=0
	while [ $i -lt 30 ]; do
		_cd_code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 --noproxy '*' \
			--resolve "$_cd_dom:443:127.0.0.1" "https://$_cd_dom/api/public/v1/site" 2>/dev/null)" || _cd_code="000"
		case "$_cd_code" in
		2*)
			_cd_ok="1"
			break
			;;
		0*) : ;; # 连接层没通：证书可能还在申请，继续等
		4*)
			_cd_hf="$_cd_code"
			break
			;; # 4xx：服务端明确拒绝，等下去也不会变
		*) : ;; # 5xx：反代够不着上游或上游出错，可能还在起，继续等
		esac
		i=$((i + 1))
		sleep 2
	done
	if [ -n "$_cd_ok" ]; then
		echo "    https://$_cd_dom 已就绪（Caddy 终结 TLS，证书与反代本机自证通过）"
		# 站外可达性是**另一个问题**，所以单独判，而且只警告、不回滚：
		# 本机够不着自己的公网地址只说明网络拓扑，用它当回滚依据会误杀（见上）。
		if curl -fsS --max-time 8 --noproxy '*' -o /dev/null "https://$_cd_dom/api/public/v1/site" 2>/dev/null; then
			echo "    https://$_cd_dom 公网回路可达"
		else
			echo "" >&2
			echo "    ⚠ 本机走公网回路访问 https://$_cd_dom 不通，但上面的本机自证是好的。" >&2
			echo "      这不代表站点打不开：公网 IP 由机房映射的机器回不到自己的公网地址。" >&2
			echo "      **请用手机或另一台机器打开 https://$_cd_dom 确认**；" >&2
			echo "      若外面也打不开，再查这两处（都是「外面进不来」，与 Caddy 无关）：" >&2
			echo "        · 域名是否指向本机：getent hosts $_cd_dom${_cd_ip:+（本机解析到 $_cd_ip）}" >&2
			echo "        · 机房/安全组是否放行 443 与 80（80 供证书续期）" >&2
			echo "" >&2
		fi
	elif [ -n "$_cd_hf" ]; then
		# 已经答复了：请求走完了 Caddy、进了 Master，是**它**拒的。
		# 这不是证书问题，重试再久也不会变——但"它拒了"仍要按响应体的原话分类：
		# 若是看板开关关掉这种运行期状态，证书与反代其实**已经证明是通的**
		# （能读到 Master 自己的话就是证明），那就只警告、不撤销。
		echo "" >&2
		err "Caddy 与 TLS 都已就绪，但 https://$_cd_dom 对本机（回环）请求回了"
		echo "      HTTP $_cd_hf —— 请求已经穿过 Caddy 到达 Master，是它拒绝了这次访问。" >&2
		_cd_body="$(curl -sS --max-time 5 --noproxy '*' -o - \
			--resolve "$_cd_dom:443:127.0.0.1" "https://$_cd_dom/api/public/v1/site" 2>/dev/null)" || true
		_cd_body="$(printf '%s' "$_cd_body" | tr -d '\r\n' | cut -c1-300)"
		if [ -n "$_cd_body" ]; then
			echo "      服务端原话：$_cd_body" >&2
		fi
		site_gate_advice "$_cd_hf" "$_cd_dom" "$_cd_body"
		echo "" >&2
		if [ "$_DGA_FATAL" = "1" ]; then
			rollback "站点对该域名回了 HTTP $_cd_hf（配置不认这个域名，重试无用）"
		fi
		echo "    ⚠ 上面这条是运行期状态，不是这次安装装坏了什么——本机自证" >&2
		echo "      （证书 + 反代）其实已经通过：能读到 Master 自己的话，就说明请求" >&2
		echo "      穿过了 Caddy。**不撤销本次安装**，继续走完最后的收尾。" >&2
		echo "" >&2
	else
		# 失败要给出**能直接照做**的诊断，而不是"稍后再看看"。
		echo "" >&2
		err "Caddy 起来了，但 https://$_cd_dom 在本机（回环）60 秒内仍不可用。"
		echo "  这条判据连的是 127.0.0.1:443、SNI 与 Host 都是域名；公网通不通、" >&2
		echo "  外面能不能进来都不参与。所以断点只可能是下面这几处之一。" >&2
		echo "  curl 的原话（以前这段把它丢进了 /dev/null，最要紧的事实从未露面）：" >&2
		_cd_rc=0
		_cd_err="$(curl -sS -o /dev/null --max-time 5 --noproxy '*' \
			--resolve "$_cd_dom:443:127.0.0.1" "https://$_cd_dom/api/public/v1/site" 2>&1 >/dev/null)" || _cd_rc=$?
		if [ -n "$_cd_err" ]; then
			echo "    $_cd_err（退出码 $_cd_rc）" >&2
		else
			echo "    （curl 没报错，说明它连上了；看下面的分段探测）" >&2
		fi
		echo "" >&2
		echo "  ── 分段探测：三层各探一次，直接看断在哪一层 ──" >&2
		printf '    · Master 直连（Host=%s）：' "$_cd_dom" >&2
		_cd_hop "http://$HEALTH_ADDR/api/public/v1/site" -H "Host: $_cd_dom" >&2
		printf '\n' >&2
		printf '    · Master 直连（不带域名 Host）  ：' >&2
		_cd_hop "http://$HEALTH_ADDR/api/public/v1/site" >&2
		printf '\n' >&2
		printf '    · 经 Caddy（--resolve 到回环）   ：' >&2
		_cd_hop "https://$_cd_dom/api/public/v1/site" --noproxy '*' --resolve "$_cd_dom:443:127.0.0.1" >&2
		printf '\n' >&2
		# 监听状态：一次取回、逐个判。没有 ss/netstat 就直说"未知"，
		# 不装作查过了——那正是上一轮四条猜测的来源。
		_cd_ports=""
		if command -v ss >/dev/null 2>&1; then
			_cd_ports="$(ss -lnt 2>/dev/null)" || true
		elif command -v netstat >/dev/null 2>&1; then
			_cd_ports="$(netstat -lnt 2>/dev/null)" || true
		fi
		if [ -z "$_cd_ports" ]; then
			echo "    · 监听状态：未知（本机没有 ss / netstat）" >&2
		else
			_cd_l=""
			for _cd_p in 80 443 "${HEALTH_ADDR##*:}"; do
				# 公开端口可能正好是 80/443，去重一次免得同一行出现两次。
				case "$_cd_l" in
				*" $_cd_p:"*) continue ;;
				esac
				if printf '%s\n' "$_cd_ports" | grep -qE "[:.]$_cd_p([^0-9]|$)"; then
					_cd_l="$_cd_l $_cd_p:在听"
				else
					_cd_l="$_cd_l $_cd_p:不在听"
				fi
			done
			echo "    · 监听状态：$_cd_l" >&2
		fi
		if Caddyfile_has_domain "$_cd_dom"; then
			echo "    · $CADDYFILE 里 $_cd_dom 的站点块：有" >&2
		else
			echo "    · $CADDYFILE 里 $_cd_dom 的站点块：**没有**" >&2
		fi
		echo "    · 域名指向：$(getent hosts "$_cd_dom" 2>/dev/null | awk '{print $1}' | head -n1 || true)" >&2
		echo "" >&2
		echo "  按断点对号入座：" >&2
		echo "    · 只有「经 Caddy」那层连不上 → 证书没拿到，看上面 curl 的原话：" >&2
		echo "        journalctl -u caddy --no-pager | grep -iE 'acme|tls\\.obtain|error' | tail -n 30" >&2
		echo "    · 「经 Caddy」那层回 502/504 → 反代够不着上游：Master 没在听 $HEALTH_ADDR" >&2
		echo "    · 443 不在听 → Caddyfile 或端口被占（x-ui、nginx、其他 caddy）" >&2
		echo "    · 「Master 直连」那两层也连不上 → Master 没在听 $HEALTH_ADDR" >&2
		echo "" >&2
		journalctl -u caddy --no-pager -n 20 2>/dev/null | sed 's/^/    /' >&2 || true
		rollback "Caddy 反代未生效（本机回环探测 https://$_cd_dom 打不开，原因见上）"
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
		# SYNC_OBTAIN_OK 只在 render_config 探测成功时才置位；配置被保留/重写前
		# 根本没走过那段，所以这里**不能**断言"二进制不支持前台申请"——
		# v0.1.3 真机上配置里明明写着 sync_obtain: true，这句话却说不支持，
		# 顺着它去查"版本错配"就把人带偏了。按配置的实际内容说话。
		if grep -q 'sync_obtain: true' "$CFG" 2>/dev/null; then
			echo "    正在等服务的首次申请（配置带 sync_obtain: true：申请不成功服务会退出；失败会中止安装并回滚）"
		else
			echo "    正在等服务的后台申请（服务每 2 分钟自动重试；失败会中止安装并回滚）"
		fi
		i=0
		while [ $i -lt "$_cert_wait" ]; do
			_cert_rc=0
			"$BIN_DIR/xb-probe-master" tls status -config "$CFG" >/dev/null 2>&1 || _cert_rc=$?
			[ "$_cert_rc" != "2" ] && break
			i=$((i + 5))
			sleep 5
		done
		_cert_rc=0
		"$BIN_DIR/xb-probe-master" tls status -config "$CFG" || _cert_rc=$?
	else
		if [ "$TLS_MODE" = "acme" ]; then
			echo "    正在申请证书（前台进行，失败会中止安装并回滚）"
		fi
		_cert_rc=0
		# 这里刻意**不再**吞掉输出：tls status 会打印主体/签发者/有效期，
		# 申请失败时打印原因与可复制的排查命令——那正是用户要的"反馈结果"。
		if [ "$_cert_wait" -gt 0 ]; then
			"$BIN_DIR/xb-probe-master" tls status -config "$CFG" -wait "$_cert_wait" || _cert_rc=$?
		else
			"$BIN_DIR/xb-probe-master" tls status -config "$CFG" || _cert_rc=$?
		fi
	fi
	case "$_cert_rc" in
	0)
		# 申请成功。把 sync_obtain 关掉：它存在的意义只是"这一次拿到确定结果"，
		# 留着的话以后每次重启都会被 CA 卡住（CA 不可达时一次正常的重启会失败）。
		if [ "$TLS_MODE" = "acme" ] && grep -q '^    sync_obtain: true' "$CFG" 2>/dev/null; then
			# 改这份文件**不能**用 `sed -i`：它换 inode、按 umask 重建权限位，而这个
			# 文件里有 ACME Token（0640 root:probe），改完可能变成 0644（凭证泄露）
			# 或 0600 root:root（下次重启服务读不到配置，站点起不来）。
			# 要「临时文件 + `>` 覆盖写」：写的是原 inode，属主与权限位一个不动。
			_sy_tmp="$(mktemp "${CFG}.syncXXXXXX" 2>/dev/null)" || _sy_tmp=""
			if [ -n "$_sy_tmp" ]; then
				if sed 's/^    sync_obtain: true.*/    sync_obtain: false         # 首次已成功，日常重启不再被证书阻塞/' \
					"$CFG" >"$_sy_tmp" 2>/dev/null; then
					if cat "$_sy_tmp" >"$CFG" 2>/dev/null; then
						echo "    已把 sync_obtain 改为 false（日常重启不再等待 CA）"
					fi
				fi
				rm -f "$_sy_tmp"
			fi
		fi
		;;
	2)
		# 还没结果。这一支在 acme + 前台申请的组合下**基本不该出现**
		# （服务要么申请成功要么没起来）；留着是为了兼容非 acme 模式与
		# 极慢的 CA。它不是成功，但也不足以判定失败，所以不回滚，
		# 而是把"怎么继续查"说清楚。
		echo "    ⚠ 申请仍未返回结果（超时）。服务会在后台继续重试；"
		echo "      随时可查：$BIN_DIR/xb-probe-master tls status -config $CFG -wait 120"
		;;
	*)
		# 申请失败：上面已经打出了原因与排查命令。按用户要求中止安装并回滚。
		rollback "证书申请失败（原因见上；tls.mode=$TLS_MODE，challenge=$ACME_CHALLENGE）"
		;;
	esac
fi

say "6c/7 发布文件入库（/dist/ 里放的是 Agent 二进制，新节点的自举来源）"
# ---------------------------------------------------------------------------
# 为什么这一步必须在部署里，而不是留给用户"稍后自己跑"
# ---------------------------------------------------------------------------
#
# 真机现象（用户报的就是这个）：Master 部署"成功"了，紧接着按收尾清单去装第一个
# Agent：
#     curl -fsSL https://<域名>/install.sh | sudo sh -s -- --code <注册码>
#       ==> 平台：linux_amd64
#       ==> 下载 xb-probe-agent_linux_amd64
#       curl: (22) The requested URL returned error: 404
#      错误：下载失败。
#
# 根因：/install.sh 是**内嵌在二进制里**的（所以那一步取得到，用户看到的是一份
# 完整可读的脚本），而脚本接下来要的是 `$DIST_BASE/xb-probe-agent_<os>_<arch>` 与
# `/dist/SHA256SUMS`——这两个是 `data_dir/dist/<版本>/` 下的**发布文件**。
# 部署流程从来没把发布文件放进去，只在收尾清单里把 `xb-probe-master dist pull`
# 当成**事后手工步骤**提了一句。于是一个"一键部署成功"的 Master，开箱的装机
# 通道是坏的，而且 404 完全指不出原因（用户第一反应是"是不是反代没配好"）。
#
# 三条边界，都是踩过之后的结论：
#
# ① **失败不回滚**。Master 本身跑得好好的，坏的只是"新节点自举"这一条路。
#    为一个可能只是网络不可达（GitHub 在部分机房连不上）的下载把整个部署回滚，
#    是把一个小问题升级成一个大问题。这里只给警告 + 可复制的补救命令。
#
# ② **判据落在磁盘上，不落在 HTTP 200 上**。/dist/ 受 install.mode 门控
#    （默认 on_demand：只有存在"未使用且未过期"的注册码时才开放）。刚部署完
#    一台新 Master 时一个注册码都还没有，此时请求 /dist/SHA256SUMS 返回 404 是
#    **预期行为**。拿它当失败判据会把每次正常部署都报成失败——判据要挑能区分
#    "真的坏了"与"还没到开放条件"的那一个，所以这里查的是文件在不在、非不非空。
#    之后再用一次真实 HTTP 请求把"现在能不能下载、不能的话为什么"说清楚。
#
# ③ 版本号对不上就跳过。只有"纯数字点分"的版本号才可能对上 Release 标签
#    （0.1.1 → v0.1.1）。本地自编译的二进制版本号是 dev/带 git 描述，
#    照默认 URL 去拉必然 404，不如直接说明白。
_mp_bin="$BIN_DIR/xb-probe-master"
_mp_ver="$("$_mp_bin" version 2>/dev/null | awk '{print $NF}')"

# 本机平台 `_mp_plat` 不在这里算：问答里要显示它、拉取时要传给 --platform，
# 两处必须同一个值 → 定义在文件前部的"0-0"段（一份来源）。

# 只有"纯数字点分"才可能对上 Release 标签。
case "${_mp_ver:-}" in
'' | *[!0-9.]* | .* | *. | *..*) _mp_releasable="" ;;
*) _mp_releasable="1" ;;
esac

if [ -n "$NO_DIST_PULL" ] || [ "$DIST_PULL" = "skip" ]; then
	echo "    已跳过（--no-dist-pull / --dist-pull skip）。"
	echo "    ⚠ 没有发布文件时，一键安装脚本会在下载 xb-probe-agent 那一步 404。"
	# 版本留空 → 提示里用 <版本> 占位。这一支里本来就没有"将要拉取的版本"。
	dist_pull_hint
	_mp_done=""
elif [ -z "$_mp_releasable" ]; then
	echo "    当前二进制版本是「${_mp_ver:-未知}」，不是发布版本号，跳过自动拉取。"
	echo "    ⚠ 没有发布文件时，一键安装脚本会在下载 xb-probe-agent 那一步 404。"
	# 同理：这一支里 $_mp_ver 是 dev 之类，把它填进命令示例只会误导。
	dist_pull_hint
	_mp_done=""
else
	# 范围问答、默认值（含"上次选过什么"的记忆）与那句措辞都在 ask_dist_pull_scope
	# 里——0c（只更新发布文件）走的是同一个函数。两份措辞的下场是"改一处＝没改"：
	# 给范围加一个选项、或改了默认值，只落在一条路上，另一条静默地继续旧行为。
	if [ -z "$DIST_PULL_GIVEN" ]; then
		if [ -n "$ASK" ]; then
			cat >&2 <<'EOF'

Agent 二进制从哪来：Master 的 /dist/ 就是新节点的**自举来源**。
装第一个 Agent 之前必须把发布文件放进 data_dir/dist/，否则安装脚本
下载 xb-probe-agent 时只会得到 404（/install.sh 本身是内嵌的，所以那一步正常）。
EOF
		fi
		ask_dist_pull_scope
	fi

	if [ "$DIST_PULL" = "skip" ]; then
		echo "    已跳过。⚠ 没有发布文件时，一键安装脚本会在下载 xb-probe-agent 那一步 404。"
		dist_pull_hint "$_mp_ver"
		_mp_done=""
	elif [ "$DIST_PULL" = "local" ] && [ -z "$_mp_plat" ]; then
		echo "    认不出本机平台（uname 结果不在已知表里），跳过自动拉取。"
		dist_pull_hint "$_mp_ver"
		_mp_done=""
	else
		# 取件、落盘判据、属主交接都在 pull_agent_release 里，与 0c 共用。
		# 它只回答"成了没有"；**失败不回滚**这个决定留在调用方（理由见上面 ①）——
		# 把回滚做进共用函数里，0c 那条路径就会连"更新失败"一起把整个部署回滚掉。
		_mp_done=""
		# 二进制**显式传进去**：同一个函数还被 0c 调用，而 0c 那条路上没有 _mp_bin。
		if pull_agent_release "$_mp_ver" "$DIST_PULL" "$_mp_bin"; then
			_mp_done="1"
		fi

		if [ -n "$_mp_done" ]; then
			# 真请求一次。能被 install.mode 挡住，那就把"为什么现在打不开、
			# 什么时候能打开"直接说清楚——而不是留一句"稍后自查"。
			_mp_port="$(split_port "$PUBLIC_LISTEN")"
			case "$TLS_TERMINATOR" in
			master)
				_mp_url="https://127.0.0.1:$_mp_port/dist/SHA256SUMS"
				_mp_ck="-k"
				_mp_nc="--no-check-certificate"
				;;
			*)
				_mp_url="http://127.0.0.1:$_mp_port/dist/SHA256SUMS"
				_mp_ck=""
				_mp_nc=""
				;;
			esac
			_mp_body=""
			if command -v curl >/dev/null 2>&1; then
				_mp_body="$(curl -sS $_mp_ck --max-time 10 "$_mp_url" 2>/dev/null || true)"
			elif command -v wget >/dev/null 2>&1; then
				_mp_body="$(wget -q $_mp_nc -O- --timeout=10 "$_mp_url" 2>/dev/null || true)"
			fi
			case "$_mp_body" in
			*xb-probe-agent_*)
				echo "    自检：$_mp_url 已可读，新节点可以下载了"
				;;
			*分发入口当前未开放*)
				# 这不是失败：on_demand 模式下，没有注册码时 /dist/ 本来就该关着，
				# 而且"关着"恰恰是设计意图（没注册码就不该有人能下装机包）。
				echo "    自检：/dist/ 现在返回「未开放」——install.mode=on_demand 的正常行为，"
				echo "          去后台「节点 → 注册码」生成一个注册码后 /dist/ 立刻开放。"
				;;
			'')
				echo "    自检：没连上 $_mp_url（服务可能刚起来，或 listen 在别的地址）。"
				echo "          服务跑起来后可以用浏览器/curl 直接试 $_mp_url"
				;;
			*)
				echo "    ⚠ 自检：$_mp_url 返回的内容不像校验和。"
				echo "      常见原因：install.mode 被设成 off；或 install.dist_base 被指向了别处"
				echo "      （指向别处时，节点其实是从那个镜像下载，本机的 /dist/ 不参与）。"
				;;
			esac
		else
			echo "    ⚠ 发布文件拉取失败。Master 本身不受影响，"
			echo "      但新节点装机时会在下载 xb-probe-agent 那一步 404。"
			dist_pull_hint "$_mp_ver"
		fi
	fi
fi

say "7/7 完成"
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
# 推导过程全在 resolve_admin_entry 里（含它踩过的两个坑：拿监听地址当访问地址、
# 以及"管理端独立监听"时一律印成公开域名下的 /admin/）。这里只负责调用它。
PUBLIC_URL="${PUBLIC_ORIGIN:-$SCHEME://$HEALTH_ADDR}"
resolve_admin_entry "$SCHEME"

echo
printf '%s============================================================%s\n' "$(_clr 1 "$C_OK")" "$(_clr 1 "$C_RST")"
printf '%s 部署完成%s\n' "$(_clr 1 "$C_OK")" "$(_clr 1 "$C_RST")"
printf '%s============================================================%s\n' "$(_clr 1 "$C_OK")" "$(_clr 1 "$C_RST")"
printf '  %s公开看板%s    %s\n' "$(_clr 1 "$C_BLD")" "$(_clr 1 "$C_RST")" "$PUBLIC_URL"
printf '  %s管理后台%s    %s\n' "$(_clr 1 "$C_BLD")" "$(_clr 1 "$C_RST")" "$ADMIN_URL"
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
  4) 发布文件：本脚本在部署时已经把 Agent 发布文件拉进 data_dir/dist/ 了
     （见上面 6c 的输出）。若当时跳过了、或之后发布了新版本，补拉一次：
       xb-probe-master dist pull --version <版本> --from <发布源 URL>
     没有发布文件时，\`/install.sh | sh\` 会在下载 xb-probe-agent 那一步 404
     （/install.sh 本身是内嵌的，所以那一步看起来一切正常，坏在下一步）。
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
	printf '  %s卸载%s        sudo sh -c "$(curl -fsSL %s)" xb --uninstall\n' "$(_clr 1 "$C_BLD")" "$(_clr 1 "$C_RST")" "$_sh_url"
	printf '              （加 --keep-data 保留数据目录；--dry-run 只预览不删）\n'
	;;
src)
	_sh_url="${SCRIPT_ENTRY_URL:-https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master.sh}"
	printf '  %s卸载%s        sudo sh -c "$(curl -fsSL %s)" xb --uninstall\n' "$(_clr 1 "$C_BLD")" "$(_clr 1 "$C_RST")" "$_sh_url"
	printf '              （加 --keep-data 保留数据目录；--dry-run 只预览不删）\n'
	;;
ps1)
	printf '  %s卸载%s        irm %s -OutFile install-master.ps1; .\\install-master.ps1 -Uninstall\n' \
		"$(_clr 1 "$C_BLD")" "$(_clr 1 "$C_RST")" "${SCRIPT_ENTRY_URL:-https://raw.githubusercontent.com/koyoca/XB-Probe/main/install-master.ps1}"
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
  指纹可在后台的「TLS 状态」页或 xb-probe-master 的日志里看到。
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
