#!/usr/bin/env bash
# 交叉编译单个平台组合的 Agent（CI 用，也可本地手动调用）。
#
# 用法：scripts/ci-cross.sh <os>/<tag>
#   例：scripts/ci-cross.sh freebsd/arm
#
# 这里刻意**不直接调用 `make dist`**：CI 的矩阵策略希望每个平台一个 job，
# 失败信息才能精确到"是哪一个平台编不过"。Makefile 的 dist 目标是"一次编完
# 全部 + 生成校验和"，粒度不匹配。
#
# 然而 armv7/armv6 的映射规则、产物命名、.exe 后缀这三件事必须与 Makefile
# 完全一致，否则会出现"CI 说能编、make dist 却编不出"的割裂。因此：
#   **改动本脚本的映射时，必须同步改 Makefile 的 PLATFORMS / dist 目标。**
# （.github/workflows/ci.yml 的 matrix.platform 也与这两处对齐。）
#
# 支持的组合见 Makefile 的 PLATFORMS 注释：只做 Windows + FreeBSD + Linux，
# 不含 macOS（无 darwin 采集器，会落到 collector_stub.go），
# mips64le 因 modernc.org/sqlite 缺该架构生成代码而不可用。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

SPEC="${1:-}"
if [ -z "${SPEC}" ]; then
  echo "用法：scripts/ci-cross.sh <os>/<tag>，例如 freebsd/arm" >&2
  exit 2
fi

os="${SPEC%/*}"
tag="${SPEC#*/}"
if [ "${os}" = "${SPEC}" ] || [ -z "${os}" ] || [ -z "${tag}" ]; then
  echo "非法的平台组合：${SPEC}（应形如 linux/arm64）" >&2
  exit 2
fi

# 架构标签 → GOARCH/GOARM。armv7/armv6 是**产物命名**，不是合法的 GOARCH。
case "${tag}" in
  armv7) goarch=arm; goarm=7 ;;
  armv6) goarch=arm; goarm=6 ;;
  *)     goarch="${tag}"; goarm="" ;;
esac

ext=""
[ "${os}" = "windows" ] && ext=".exe"

# module 名用于注入发布标签。从 go.mod 读而不是写死：
# 写死会在改 module 名后静默失效（-X 指向不存在的符号时 go build **不报错**，
# 只是什么也没注入，表现是自升级取不到构件）。
MODULE="$(sed -n 's/^module[[:space:]]\{1,\}\([^[:space:]]*\).*/\1/p' go.mod | head -1)"
if [ -z "${MODULE}" ]; then
  echo "无法从 go.mod 解析出 module 名" >&2
  exit 1
fi

out="bin/ci/xb-probe-agent_${os}_${tag}${ext}"
mkdir -p bin/ci

echo ">> ${os}/${tag}（GOARCH=${goarch}${goarm:+ GOARM=}${goarm}）→ ${out}"

# -trimpath 去掉本地绝对路径；-s -w 去符号表。与 Makefile 的 LDFLAGS 保持一致。
# 发布标签**必须**注入：armv7/armv6 在 Go 里都是 GOARCH=arm（靠 GOARM 区分），
# 运行时拿不到 GOARM，自升级只能靠这个标签精确匹配产物名（见 internal/dist）。
LDFLAGS="-s -w -X ${MODULE}/internal/dist.PlatformTag=${os}_${tag}"
if [ -n "${goarm}" ]; then
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${goarch}" GOARM="${goarm}" \
    go build -trimpath -ldflags "${LDFLAGS}" -o "${out}" ./cmd/xb-probe-agent
else
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${goarch}" \
    go build -trimpath -ldflags "${LDFLAGS}" -o "${out}" ./cmd/xb-probe-agent
fi

# 产物存在且非空，才算这一步真的过了（早期曾出现 build 静默产出 0 字节文件）。
if [ ! -s "${out}" ]; then
  echo "编译未产出有效文件：${out}" >&2
  exit 1
fi
ls -l "${out}"
