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
# 常用可选参数：
#   --dir <目录>          配置目录（默认 /etc/probe-master）
#   --data <目录>         数据目录（默认 /var/lib/probe-master）
#   --bin-dir <目录>      二进制安装目录（默认 /usr/local/bin；只读根文件系统用 /opt/probe-master）
#   --public-listen <地址> 公开站监听（默认 0.0.0.0:8080）
#   --admin-listen  <地址> 管理端监听（默认 127.0.0.1:8081）
#   --dist-base <URL>     发布文件镜像前缀（不放行则由 Master 自己提供 /dist/）
#   --no-service          只安装不注册服务（容器里用）
#   --force-config        覆盖已有配置文件
#
# TLS（默认 none；四种方式详见 configs/master.yaml.example 的 tls 段）：
#   --tls-mode <none|files|selfsigned|acme>
#   --tls-cert <证书> --tls-key <私钥>      mode=files：自定义证书（热加载）
#   --tls-domain <域名[,域名]>              mode=acme/selfsigned：域名（SAN）
#   --tls-ip <IP[,IP]>                      mode=acme/selfsigned：IP（RFC 8738 / SAN）
#   --acme-email <邮箱> --acme-ca <CA>       mode=acme：账户邮箱与 CA（默认 letsencrypt）
#   --acme-challenge <http-01|tls-alpn-01>   mode=acme：默认 http-01（需要 80 端口可达）
#
# 配了 --tls-mode 却与 --public-origin 的协议不一致（例如 mode=acme 却给 http://）
# 会被拒绝：证书装上了，访客却仍在明文上，是最难被察觉的一种"部署成功"。

set -eu

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
  --public-listen <地址>     默认 0.0.0.0:8080
  --admin-listen <地址>      默认 127.0.0.1:8081
  --dist-base <URL>         发布文件镜像前缀
  --no-service              不注册 systemd（容器场景）
  --force-config            覆盖已有配置
  -h, --help                显示本帮助

TLS：
  --tls-mode <none|files|selfsigned|acme>   默认 none
  --tls-cert <文件> --tls-key <文件>          mode=files
  --tls-domain <域名[,域名]>                  mode=acme/selfsigned
  --tls-ip <IP[,IP]>                          mode=acme/selfsigned
  --acme-email <邮箱>                         mode=acme（可选）
  --acme-ca <CA>                             默认 letsencrypt
  --acme-challenge <http-01|tls-alpn-01>     默认 http-01
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--bin) BIN_SRC="${2:-}"; shift 2 ;;
	--dir) DIR="${2:-}"; shift 2 ;;
	--data) DATA="${2:-}"; shift 2 ;;
	--bin-dir) BIN_DIR="${2:-}"; shift 2 ;;
	--public-listen) PUBLIC_LISTEN="${2:-}"; shift 2 ;;
	--admin-listen) ADMIN_LISTEN="${2:-}"; shift 2 ;;
	--public-origin) PUBLIC_ORIGIN="${2:-}"; shift 2 ;;
	--admin-origin) ADMIN_ORIGIN="${2:-}"; shift 2 ;;
	--dist-base) DIST_BASE="${2:-}"; shift 2 ;;
	--admin-user) ADMIN_USER="${2:-}"; shift 2 ;;
	--admin-password) ADMIN_PASSWORD="${2:-}"; shift 2 ;;
	--no-service) NO_SERVICE="1"; shift ;;
	--force-config) FORCE_CONFIG="1"; shift ;;
	--tls-mode) TLS_MODE="${2:-}"; shift 2 ;;
	--tls-cert) TLS_CERT="${2:-}"; shift 2 ;;
	--tls-key) TLS_KEY="${2:-}"; shift 2 ;;
	--tls-domain) TLS_DOMAINS="${2:-}"; shift 2 ;;
	--tls-ip) TLS_IPS="${2:-}"; shift 2 ;;
	--acme-email) ACME_EMAIL="${2:-}"; shift 2 ;;
	--acme-ca) ACME_CA="${2:-}"; shift 2 ;;
	--acme-challenge) ACME_CHALLENGE="${2:-}"; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) echo "未知参数：$1" >&2; usage; exit 2 ;;
	esac
done

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
	http-01 | tls-alpn-01) : ;;
	*) echo "错误：--acme-challenge 只能是 http-01 或 tls-alpn-01。" >&2; exit 2 ;;
	esac
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

if [ "$(id -u)" -ne 0 ] && [ -z "$NO_SERVICE" ]; then
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

CFG="$DIR/master.yaml"
echo "==> 3/7 写配置文件 $CFG"
if [ -f "$CFG" ] && [ -z "$FORCE_CONFIG" ]; then
	echo "    已存在，保留（要覆盖请加 --force-config）"
else
	# 只写必需项，其余走代码内默认值；完整可选项见 configs/master.yaml.example。
	{
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
			if [ -n "$TLS_DOMAINS" ]; then echo "    domains: [$(yaml_list "$TLS_DOMAINS")]"; fi
			if [ -n "$TLS_IPS" ]; then echo "    ips: [$(yaml_list "$TLS_IPS")]"; fi
			;;
		esac
		echo "log_level: \"info\""
	} >"$CFG"
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
	echo "==> 5~7 跳过（--no-service）。前台运行：$BIN_DIR/probe-master -config $CFG"
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

echo "==> 7/7 完成。接下来要做的事"
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
cat <<EOF
  1) 登录管理端 ${ADMIN_ORIGIN:-${SCHEME}://$ADMIN_LISTEN}/admin/ 并**启用 TOTP、保存恢复码**
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
     它是**运行时设置，改完即时生效、不用重启**（见 docs/04 §2.5）。
     分离靠的是 Host 而不是监听地址，因此「一个端口 + 两个域名（反代）」也能分离；
     反之**明文 HTTP 直连 IP** 的部署填不出域名清单，别选「分离」——会把后台锁在外面。
  6) 配外部心跳 heartbeat_url，并确认能收到失联通知
  7) 自己再跑一次：bash scripts/restore-drill.sh（确认备份在这台机器上真能恢复）
EOF

# TLS 的收尾提示按模式给。写死一句"去配 TLS"是没用的：真正会卡住人的
# 是每种模式各自的前置条件（80 端口、证书路径、私钥指纹）。
case "$TLS_MODE" in
none)
	cat >&2 <<'EOF'
ℹ 当前 tls.mode=none：对外**必须**由前置 Caddy/Nginx 终结 TLS，否则会话是明文的。
  反代方案里别忘了把反代网段填进 master.yaml 的 trusted_proxies，
  否则 X-Forwarded-Proto 不被采信、Cookie 会走明文降级分支。详见 docs/04 §2.4。
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
