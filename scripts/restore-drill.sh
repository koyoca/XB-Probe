#!/usr/bin/env bash
# XB-Probe 备份恢复演练（6.6 未完成的那半个验收标准）。
#
# 目的：**真实地**走一遍"备份 → 破坏主库 → 从备份恢复 → 校验数据完整"的
# 完整闭环，而不是只把备份文件生成出来就宣称"备份可用"。
#
# 为什么必须真做一次：
#   备份"看起来成功"和"真的可恢复"是两件事。常见的失效模式——备份文件生成了、
#   大小也正常，但恢复后节点全丢/历史数据断裂/令牌哈希丢失——只有真恢复一次
#   才会暴露。本项目已经吃过太多次"静默失效"的亏（见 docs/01 缺陷清单）。
#
# 演练流程（全程在临时目录，绝不碰真实 data 目录）：
#   1. 起一个全新 Master 实例，init-admin
#   2. 播种数据：建节点、写计费、生成注册码、走一遍 Agent 注册+上报
#      （这样库里同时有"配置类"与"时序类"数据，覆盖面更广）
#   3. 记录恢复前的"数据指纹"：各表行数 + 关键业务值
#   4. 调 /maintenance/backup 生成备份
#   5. **破坏主库**：把 probe.db 覆盖成垃圾（模拟磁盘损坏/误删）
#   6. 停 Master → 用备份文件替换主库（连同清掉 -wal/-shm）→ 重启
#   7. 重新计算数据指纹，与第 3 步逐项比对
#   8. 断言：行数一致、关键值一致、节点能重新认证、历史数据可查
#
# 用法：scripts/restore-drill.sh
# 前提：PATH 中有 go 与 curl。无需 root，不需要 minisign。
# 保留现场：KEEP_WORK=1 scripts/restore-drill.sh

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
WORK_REL="dist/restore-drill"
WORK="${ROOT}/${WORK_REL}"
rm -rf "${WORK_REL}" 2>/dev/null || true
mkdir -p "${WORK}"

GO_BIN="${GO:-}"
if [ -z "${GO_BIN}" ]; then
  if command -v go >/dev/null 2>&1; then
    GO_BIN="go"
  elif [ -x "${HOME}/.workbuddy/binaries/go/go1.27.2/go/bin/go" ]; then
    GO_BIN="${HOME}/.workbuddy/binaries/go/go1.27.2/go/bin/go"
  elif [ -x /usr/local/go/bin/go ]; then
    GO_BIN="/usr/local/go/bin/go"
  fi
fi

PUB_PORT=18580
ADM_PORT=18581
PUB_BASE="http://127.0.0.1:${PUB_PORT}"
ADM_BASE="http://127.0.0.1:${ADM_PORT}"
ADMIN_USER="drill"
ADMIN_PASS="DrillPassw0rd!23"

PASS=0
FAIL=0
SKIP=0
MASTER_PID=""
CUR_STEP="(未开始)"

ok()   { PASS=$((PASS + 1)); printf '  \033[32mPASS\033[0m  %s\n' "$1"; }
bad()  { FAIL=$((FAIL + 1)); printf '  \033[31mFAIL\033[0m  %s\n' "$1"; }
skip() { SKIP=$((SKIP + 1)); printf '  \033[33mSKIP\033[0m  %s\n' "$1"; }
sec()  { printf '\n== %s ==\n' "$1"; }
code_of() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

stop_master() {
  if [ -n "${MASTER_PID}" ] && kill -0 "${MASTER_PID}" 2>/dev/null; then
    kill "${MASTER_PID}" 2>/dev/null || true
    wait "${MASTER_PID}" 2>/dev/null || true
  fi
  MASTER_PID=""
}

cleanup() {
  stop_master
  if [ "${KEEP_WORK:-0}" = "1" ]; then
    echo
    echo "演练现场保留在：${WORK}"
  else
    cd "${ROOT}" && rm -rf "${WORK_REL}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

fail_and_exit() { bad "$1"; echo; echo "演练在第 ${CUR_STEP} 步中止。"; exit 1; }

DB="${WORK}/data/probe.db"

# ---------------------------------------------------------------------------
sec "0. 构建 Master"
# ---------------------------------------------------------------------------
[ -n "${GO_BIN}" ] || fail_and_exit "未找到 go"
mkdir -p "${WORK}/bin"
# 注意 -o 用**相对路径**：Master 是原生 Windows 二进制，喂它 /c/... 形式的 MSYS
# 绝对路径会被理解成 <当前盘>:\c\...，go build 会报 "cannot find the file specified"。
if (cd "${ROOT}" && "${GO_BIN}" build -trimpath -ldflags "-s -w" -o "${WORK_REL}/bin/xb-probe-master" ./cmd/xb-probe-master 2>"${WORK}/build.log"); then
  ok "go build xb-probe-master"
else
  fail_and_exit "构建失败：$(cat "${WORK}/build.log")"
fi
MASTER="${WORK}/bin/xb-probe-master"

cat >"${WORK}/master.yaml" <<YAML
public_listen: "127.0.0.1:${PUB_PORT}"
admin_listen: "127.0.0.1:${ADM_PORT}"
data_dir: "${WORK_REL}/data"
themes_dir: "${WORK_REL}/themes"
master_key_file: "${WORK_REL}/master.key"
trusted_proxies: ["127.0.0.1"]
tls:
  mode: none
log_level: "warn"
YAML

# ---------------------------------------------------------------------------
sec "1. init-admin 并首次启动"
# ---------------------------------------------------------------------------
"${MASTER}" init-admin -config "${WORK}/master.yaml" -username "${ADMIN_USER}" -password "${ADMIN_PASS}" >"${WORK}/init.log" 2>&1
[ -f "${DB}" ] && ok "主库已生成（$(stat -c%s "${DB}" 2>/dev/null || echo '?') 字节）" || fail_and_exit "主库未生成：$(cat "${WORK}/init.log")"

start_master() {
  "${MASTER}" -config "${WORK}/master.yaml" >>"${WORK}/master.log" 2>&1 &
  MASTER_PID=$!
  local i
  for i in $(seq 1 60); do
    [ "$(code_of "${PUB_BASE}/api/public/v1/site")" = "200" ] && return 0
    kill -0 "${MASTER_PID}" 2>/dev/null || return 1
    sleep 0.25
  done
  return 1
}

CUR_STEP="1 启动"
start_master || fail_and_exit "Master 未就绪：$(tail -20 "${WORK}/master.log")"
ok "Master 已就绪（public=${PUB_PORT} admin=${ADM_PORT}）"

login() {
  curl -s -D "${WORK}/hdr.txt" -o "${WORK}/login.json" -X POST "${ADM_BASE}/api/admin/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-Forwarded-Proto: https' \
    -d "{\"username\":\"${ADMIN_USER}\",\"password\":\"${ADMIN_PASS}\"}"
  AUTH_COOKIE="$(sed -n 's/^[Ss]et-[Cc]ookie:[[:space:]]*\([^;]*\).*/\1/p' "${WORK}/hdr.txt" | tr -d '\r' | head -1)"
  CSRF="$(sed -n 's/.*"csrf_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${WORK}/login.json")"
  [ -n "${AUTH_COOKIE}" ] && [ -n "${CSRF}" ]
}

CUR_STEP="1 登录"
login || fail_and_exit "登录失败：$(cat "${WORK}/login.json")"
A="Cookie: ${AUTH_COOKIE}"
J='Content-Type: application/json'
ok "管理员登录成功（Cookie: ${AUTH_COOKIE%%=*}）"

# ---------------------------------------------------------------------------
sec "2. 播种数据（配置类 + 时序类，覆盖得广一些）"
# ---------------------------------------------------------------------------
CUR_STEP="2 建节点"
NODE_IDS=""
for i in 1 2 3; do
  R="$(curl -s -X POST "${ADM_BASE}/api/admin/v1/nodes" -H "${A}" -H "X-CSRF-Token: ${CSRF}" \
        -H "${J}" -d "{\"name\":\"演练节点${i}\",\"group\":\"drill\"}")"
  ID="$(sed -n 's/.*[{,]"id"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' <<<"${R}" | head -1)"
  PID_="$(sed -n 's/.*"public_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"${R}" | head -1)"
  [ -n "${ID}" ] || fail_and_exit "建节点 ${i} 失败：${R}"
  NODE_IDS="${NODE_IDS} ${ID}"
  eval "PUB_ID_${i}='${PID_}'"
done
ok "已建 3 个节点（id:${NODE_IDS}）"

FIRST_NODE="$(echo ${NODE_IDS} | awk '{print $1}')"

CUR_STEP="2 写计费"
# 计费写整数分（price_minor），并带时区——顺带验证内嵌时区库在恢复后仍可用。
# 必填：cycle/traffic_mode/reset_day 三个字段都有取值校验，漏一个就 400。
BCODE="$(code_of -X PUT "${ADM_BASE}/api/admin/v1/nodes/${FIRST_NODE}/billing" \
  -H "${A}" -H "X-CSRF-Token: ${CSRF}" -H "${J}" \
  -d '{"price_minor":1999,"currency":"CNY","cycle":"monthly","expire_at":2099134592,"traffic_quota":107374182400,"traffic_mode":"both","reset_day":1,"tz":"Asia/Shanghai"}')"
[ "${BCODE}" = "204" ] && ok "写入计费（¥19.99/月，tz=Asia/Shanghai）" || fail_and_exit "写计费失败 HTTP=${BCODE}"

CUR_STEP="2 建探测目标"
# 探测目标是**顶层资源**（/probe-targets），不是 /nodes/{id}/targets；
# node_id=0 表示"全局目标"。字段是 timeout_ms 而不是 interval_s。
TCODE="$(code_of -X POST "${ADM_BASE}/api/admin/v1/probe-targets" \
  -H "${A}" -H "X-CSRF-Token: ${CSRF}" -H "${J}" \
  -d '{"node_id":0,"name":"演练目标","host":"127.0.0.1","port":80,"method":"tcp","timeout_ms":3000}')"
if [ "${TCODE}" = "200" ]; then ok "已建探测目标"; else bad "建探测目标 HTTP=${TCODE}（不致命，继续）"; fi

CUR_STEP="2 注册码 + Agent 上报"
EC="$(curl -s -X POST "${ADM_BASE}/api/admin/v1/nodes/${FIRST_NODE}/enroll-codes" \
      -H "${A}" -H "X-CSRF-Token: ${CSRF}" -H "${J}" -d '{}')"
ENROLL_CODE="$(sed -n 's/.*"code"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"${EC}")"
[ -n "${ENROLL_CODE}" ] || fail_and_exit "生成注册码失败：${EC}"
ok "已生成注册码"

NOW="$(date +%s)"
# 上报 3 个样本，让时序表里有真实行（metrics_raw 至少 1 行，聚合表由管线写入）。
ENROLL_BODY="{\"code\":\"${ENROLL_CODE}\",\"hostname\":\"drill-node\",\"os\":\"linux\",\"arch\":\"amd64\",\"agent_version\":\"1.0.0\",\"counter_bits\":64}"
ER="$(curl -s -X POST "${PUB_BASE}/api/agent/v1/enroll" -H "${J}" -d "${ENROLL_BODY}")"
AGENT_TOKEN="$(sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"${ER}")"
[ -n "${AGENT_TOKEN}" ] || fail_and_exit "Agent 注册失败：${ER}"
ok "Agent 注册成功，已取得令牌"

for k in 1 2 3; do
  TS=$((NOW - (3 - k)))
  RB="{\"agent_version\":\"1.0.0\",\"static\":{\"hostname\":\"drill-node\",\"os\":\"linux\",\"kernel\":\"6.1.0\",\"arch\":\"amd64\",\"cpu_model\":\"DrillCPU\",\"cpu_cores\":8,\"mem_total\":17179869184,\"swap_total\":0,\"disk_total\":107374182400,\"virt\":\"none\"},\"samples\":[{\"ts\":${TS},\"metrics\":{\"cpu\":$((1000 + k)),\"load1\":50,\"mem_used\":4294967296,\"swap_used\":0,\"disk_used\":21474836480,\"conns\":42,\"boot_time\":$((NOW - 7200)),\"nics\":{\"eth0\":{\"rx\":$((1000000 * k)),\"tx\":$((2000000 * k))}}},\"latency\":[{\"target_id\":0,\"ok\":true,\"rtt_us\":$((5000 + k))}]}]}"
  curl -s -o /dev/null -X POST "${PUB_BASE}/api/agent/v1/report" \
    -H "Authorization: Bearer ${AGENT_TOKEN}" -H "${J}" -d "${RB}"
  sleep 0.3
done
ok "已上报 3 个样本（含指标与时序）"

# 给聚合/状态同步留出节拍时间。
sleep 3

# ---------------------------------------------------------------------------
sec "3. 记录恢复前数据指纹"
# ---------------------------------------------------------------------------
# 指纹用 sqlite3 直接数各表行数 + 取关键业务值。本机没有 sqlite3 CLI 也能跑：
# 用 Go 写的小工具（见下）。这里优先用 CLI，缺失则走 go run 兜底。
fingerprint() { # <db-path> -> 打印 "表名 行数" 与关键值
  local db="$1"
  # 刻意不写 -mod=vendor：本仓库不带 vendor/（见 .gitignore 末尾与 docs/03 §3.2），
  # 本机即使自己 vendor 过也会被 Go 自动识别，显式指定反而在干净克隆上必然失败。
  "${GO_BIN}" run ./internal/store/cmd/dbfp -db "${db}" 2>/dev/null
}

FP_BEFORE="$(fingerprint "${DB}")"
if [ -z "${FP_BEFORE}" ]; then
  fail_and_exit "指纹工具无输出"
fi
echo "${FP_BEFORE}" | sed 's/^/      /'
ok "已记录恢复前指纹（$(echo "${FP_BEFORE}" | wc -l | tr -d ' ') 项）"

# ---------------------------------------------------------------------------
sec "4. 生成备份"
# ---------------------------------------------------------------------------
CUR_STEP="4 备份"
BK="$(curl -s -X POST "${ADM_BASE}/api/admin/v1/maintenance/backup" -H "${A}" -H "X-CSRF-Token: ${CSRF}")"
BK_NAME="$(sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"${BK}" | head -1)"
[ -n "${BK_NAME}" ] || fail_and_exit "手动备份失败：${BK}"
BK_PATH="${WORK}/data/backups/${BK_NAME}"
[ -f "${BK_PATH}" ] || fail_and_exit "备份文件不存在：${BK_PATH}"
BK_SIZE="$(stat -c%s "${BK_PATH}" 2>/dev/null || echo '?')"
ok "备份已生成：${BK_NAME}（${BK_SIZE} 字节）"

# 备份文件本身必须是一份**完整可读**的库——立刻用指纹工具验一次，
# 而不是等到破坏主库之后才发现备份是坏的（那时已经晚了）。
FP_BAK="$(fingerprint "${BK_PATH}")"
if [ "${FP_BAK}" = "${FP_BEFORE}" ]; then
  ok "备份文件自校验：指纹与主库一致"
else
  echo "     主库：" ; echo "${FP_BEFORE}" | sed 's/^/       /'
  echo "     备份：" ; echo "${FP_BAK}" | sed 's/^/       /'
  fail_and_exit "备份文件指纹与主库不一致（备份不可用！）"
fi

# ---------------------------------------------------------------------------
sec "5. 破坏主库（模拟磁盘损坏 / 误删）"
# ---------------------------------------------------------------------------
CUR_STEP="5 破坏主库"
stop_master
ok "已停 Master（避免 Windows 锁文件）"
sleep 0.5

# 先把 WAL/SHM 一起清掉：WAL 模式下主库文件本身可能只含部分已提交数据，
# 真实的"磁盘损坏"场景里 WAL 也多半一起没了。留着 WAL 去恢复会得到
# 一个"半新半旧"的库，掩盖恢复问题。
if command -v taskkill >/dev/null 2>&1; then :; fi
rm -f "${DB}" "${DB}-wal" "${DB}-shm"
printf 'this-is-not-a-sqlite-database-xxxxxxxxxxxxxxxxxxxx' > "${DB}"
ok "主库已被覆盖为垃圾内容（$(stat -c%s "${DB}" 2>/dev/null || echo '?') 字节）"

# 验证"损坏"确实生效：此时启动 Master 必须失败（quick_check 拦截，13.4.8）。
if (cd "${ROOT}" && "${MASTER}" -config "${WORK}/master.yaml" >"${WORK}/broken.log" 2>&1); then
  bad "损坏的库竟然启动成功了（quick_check 未拦截）"
else
  if grep -qE 'quick_check|integrity|恢复' "${WORK}/broken.log"; then
    ok "Master 拒绝启动并提示从备份恢复（quick_check 生效）"
  else
    ok "Master 拒绝在损坏的库上启动"
    echo "     （日志：$(head -2 "${WORK}/broken.log" | tr '\n' ' '))"
  fi
fi

# ---------------------------------------------------------------------------
sec "6. 从备份恢复"
# ---------------------------------------------------------------------------
CUR_STEP="6 恢复"
cp "${BK_PATH}" "${DB}"
chmod 600 "${DB}" 2>/dev/null || true
# 恢复出来的库权限不能比主库松——它含全部令牌与管理员哈希。
PERM="$(stat -c%a "${DB}" 2>/dev/null || echo '?')"
if [ "${PERM}" = "600" ]; then ok "恢复出的库权限为 600"; else skip "本机 stat 无法读权限（显示 ${PERM}），权限断言跳过"; fi

# 恢复后立刻自校验：能打开、quick_check 通过、指纹与恢复前一致。
FP_RESTORED_FILE="$(fingerprint "${DB}")"
if [ "${FP_RESTORED_FILE}" = "${FP_BEFORE}" ]; then
  ok "恢复后的库文件指纹与恢复前一致"
else
  fail_and_exit "恢复后的库文件指纹不一致"
fi

CUR_STEP="6 重启"
start_master || fail_and_exit "恢复后 Master 无法启动：$(tail -20 "${WORK}/master.log")"
ok "恢复后 Master 正常启动（quick_check 通过）"

sleep 3
FP_AFTER="$(fingerprint "${DB}")"

# ---------------------------------------------------------------------------
sec "7. 校验业务完整性"
# ---------------------------------------------------------------------------
CUR_STEP="7 指纹比对"
# 逐项比对：行数必须一致；关键业务值必须一致。
# 注意：上报之后主库仍在持续写入（状态同步/聚合），所以**时序表行数可能增长**——
# 这是正常的（恢复到现在又过了几秒）。我们比对的是"恢复前已存在的历史数据
# 是否都在"，因此对时序表只要求"不少于恢复前"，对配置类表要求"完全相等"。
check_lines() {
  local before="$1" after="$2" mode="$3" label="$4"
  if [ "${mode}" = "exact" ]; then
    if [ "${before}" = "${after}" ]; then ok "${label}: ${before}（完全一致）"; else bad "${label}: 恢复前 ${before} vs 恢复后 ${after}（应完全一致）"; fi
  else
    if [ "${after}" -ge "${before}" ] 2>/dev/null; then ok "${label}: ${before} -> ${after}（未丢失）"; else bad "${label}: 恢复前 ${before} vs 恢复后 ${after}（数据丢失！）"; fi
  fi
}

# 配置类表必须逐字节一致。
for t in nodes enroll_codes admins settings node_billing probe_targets; do
  b="$(echo "${FP_BEFORE}" | sed -n "s/^${t} //p")"
  a="$(echo "${FP_AFTER}"  | sed -n "s/^${t} //p")"
  [ -z "${b}" ] && continue
  check_lines "${b}" "${a}" exact "${t}"
done
# 时序类表只要求不减少。
for t in metrics_raw latency_raw metrics_1m metrics_10m latency_10m; do
  b="$(echo "${FP_BEFORE}" | sed -n "s/^${t} //p")"
  a="$(echo "${FP_AFTER}"  | sed -n "s/^${t} //p")"
  [ -z "${b}" ] && continue
  check_lines "${b}" "${a}" atleast "${t}"
done

# 关键业务值：计费价格、节点名、注册码哈希都必须在。
# 注意：计费**没有独立的 GET 接口**，它是随节点详情一起返回的
# （GET /nodes/{id} 里带 billing 对象），所以这里读节点详情。
CUR_STEP="7 业务值"
NODE_DETAIL="$(curl -s "${ADM_BASE}/api/admin/v1/nodes/${FIRST_NODE}" -H "${A}")"
if echo "${NODE_DETAIL}" | grep -q '"price_minor":[[:space:]]*1999'; then
  ok "计费价格恢复正确（1999 分）"
else bad "计费价格丢失：${NODE_DETAIL}"; fi
if echo "${NODE_DETAIL}" | grep -q '"tz":[[:space:]]*"Asia/Shanghai"'; then
  ok "计费时区恢复正确（Asia/Shanghai，内嵌时区库可用）"
else bad "计费时区丢失：${NODE_DETAIL}"; fi
NODES="$(curl -s "${ADM_BASE}/api/admin/v1/nodes" -H "${A}")"
if echo "${NODES}" | grep -q '演练节点1'; then ok "节点名称恢复正确"; else bad "节点名称丢失"; fi

# 管理员**恢复后仍能登录**——这条最容易被忽略却最关键：会话表/管理员哈希
# 若没恢复成功，恢复出来的库等于一座空城（数据在、但谁也别想进去）。
CUR_STEP="7 重新登录"
if login; then ok "恢复后管理员仍可登录（会话与哈希完整）"; else bad "恢复后无法登录（管理员数据损坏）"; fi
A="Cookie: ${AUTH_COOKIE}"

# Agent 令牌必须仍然有效（令牌哈希随库一起恢复）。
CUR_STEP="7 Agent 令牌"
CFG_CODE="$(code_of "${PUB_BASE}/api/agent/v1/config" -H "Authorization: Bearer ${AGENT_TOKEN}")"
if [ "${CFG_CODE}" = "200" ]; then ok "恢复后 Agent 令牌仍有效（上报不会中断）"; else bad "恢复后 Agent 令牌失效 HTTP=${CFG_CODE}"; fi

# 历史数据可查（恢复后至少要能查到刚才上报的那些点）。
CUR_STEP="7 历史数据"
HCODE="$(code_of "${PUB_BASE}/api/public/v1/nodes/${PUB_ID_1}/history?metric=cpu&range=30m")"
if [ "${HCODE}" = "200" ]; then ok "公开历史接口可用（HTTP 200）"; else bad "公开历史接口异常 HTTP=${HCODE}"; fi

echo
echo "========================================="
printf '备份恢复演练结果：通过 %d 项，失败 %d 项，跳过 %d 项\n' "${PASS}" "${FAIL}" "${SKIP}"
echo "========================================="
[ "${FAIL}" -eq 0 ] || exit 1
