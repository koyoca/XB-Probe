#!/bin/sh
# XB-Probe —— Master 一键卸载
#
# 一条命令卸干净（Linux / systemd）：
#
#   sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/koyoca/XB-Probe/main/uninstall-master.sh)"
#
# 只做一件事：把 scripts/deploy-master.sh 取回来，用 --uninstall 跑一遍。
# **刻意不在这里实现一遍删除逻辑**——删除逻辑只有一份，两份迟早漂移，
# 而这条路径赌的是用户的系统。
#
# 为什么还要有这么一个独立脚本（install-master.sh / install-master-bin.sh
# 都已经支持 --uninstall）：卸载发生在"已经决定不用它了"的时候，那时安装脚本
# 往往已经不在这台机器上，而人也不会记得 `--uninstall` 这个参数。
#
# 参数：
#   --repo <owner/repo>   仓库（默认 koyoca/XB-Probe）
#   --script-ref <ref>    取部署脚本的 ref（默认 main）
#   --keep-data           保留数据目录（默认连数据库与 master.key 一起删）
#   --dry-run             只列出将要做的事，不删任何东西
#   --dir/--data/--bin-dir  非默认安装位置时需要显式给出
#   -h, --help            显示本帮助
#
# 想先看看会删掉什么，就加 --dry-run：
#   sudo sh uninstall-master.sh --dry-run

set -eu

REPO_RAW="${XB_REPO:-koyoca/XB-Probe}"
SCRIPT_REF="${XB_SCRIPT_REF:-main}"
PASSTHRU=""

usage() {
	cat <<'EOF'
uninstall-master.sh —— XB-Probe Master 一键卸载

  --repo <owner/repo>   仓库（默认 koyoca/XB-Probe）
  --script-ref <ref>    取部署脚本的 ref（默认 main）
  --keep-data           保留数据目录（默认连数据库与 master.key 一起删）
  --dry-run             只列出将要做的事，不删除任何东西
  --dir <目录>          非默认配置目录
  --data <目录>         非默认数据目录
  --bin-dir <目录>      非默认二进制目录
  -h, --help            显示本帮助

其余参数原样透传给 scripts/deploy-master.sh --uninstall。
EOF
}

say() { printf '==> %s\n' "$*"; }
err() { printf '错误：%s\n' "$*" >&2; }

# download <URL>：curl 或 wget 二选一。
download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O - "$1"
	else
		err "找不到 curl 或 wget，无法下载。"
		exit 1
	fi
}

while [ $# -gt 0 ]; do
	case "$1" in
	--repo) REPO_RAW="${2:-}"; shift 2 ;;
	--script-ref) SCRIPT_REF="${2:-}"; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*)
		# 其余参数（--keep-data/--dry-run/--dir/--data/--bin-dir）不在这里解析，
		# 原样交给部署脚本——**它才是这些参数的唯一实现**。这样"新增一个卸载相关
		# 开关"只需要改一处，也就不会出现两边默认值不一致。
		PASSTHRU="$PASSTHRU $1"
		shift
		;;
	esac
done

# 归一化仓库名：允许 owner/repo、https://github.com/owner/repo[.git]。
REPO="$(printf '%s' "$REPO_RAW" |
	sed -e 's|^https\?://[^/]*/||' -e 's|\.git$||' -e 's|^/||' -e 's|/$||')"
case "$REPO" in
*/*) : ;;
*)
	err "--repo 应形如 owner/repo（收到 $REPO_RAW）。"
	exit 2
	;;
esac

# 只有 --dry-run 能不提权跑；其余一律要 root。
case " $PASSTHRU " in
*" --dry-run "*) : ;;
*)
	if [ "$(id -u)" -ne 0 ]; then
		err "卸载需要 root 或 sudo（想先看看会删什么，加 --dry-run）。"
		exit 1
	fi
	;;
esac

TMP="$(mktemp -d 2>/dev/null || echo "/tmp/xb-uninst.$$")"
mkdir -p "$TMP"
trap 'rm -rf "$TMP"' EXIT INT TERM HUP

say "取部署脚本（$SCRIPT_REF）"
DEPLOY="$TMP/deploy-master.sh"
download "https://raw.githubusercontent.com/$REPO/$SCRIPT_REF/scripts/deploy-master.sh" >"$DEPLOY" || {
	err "取不到 https://raw.githubusercontent.com/$REPO/$SCRIPT_REF/scripts/deploy-master.sh"
	err "检查网络与 --repo/--script-ref。"
	exit 1
}
# 取回来的东西必须是脚本。少了这一条，一个错误页会被当成脚本执行——
# 在一条要删用户数据的路径上，这道检查不能省。
if ! grep -q '^#!/bin/sh' "$DEPLOY"; then
	err "取回的 deploy-master.sh 不像脚本（没有 shebang），已中止。"
	exit 1
fi

say "开始卸载（接下来的输出来自 scripts/deploy-master.sh）"
# $PASSTHRU 不加引号是有意的——它是要按空白切成多个参数传递的。
# shellcheck disable=SC2086
if ! sh "$DEPLOY" --uninstall $PASSTHRU; then
	err "卸载脚本执行失败（上面是它的输出）。"
	exit 1
fi
