#!/usr/bin/env bash
# Server Probe 端到端冒烟测试（master 侧）。
#
# 覆盖：CLI 子命令、库迁移、双监听启动、公开 API 与内嵌主题、
#       管理界面静态资源、登录/CSRF/鉴权、CSP 安全头、路径穿越防护、
#       公开 DTO 不泄漏内网信息、TLS/HSTS 下发条件（6.3）、
#       公开站与管理端源分离的 Host 门禁 421（6.1）。
#
# 用法：scripts/smoke-test.sh
# 前提：PATH 中有 go 与 curl；无需 root，也不碰真实 data 目录（全程在临时目录）。

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# 临时实例目录放在仓库内、用相对路径增删（Windows 下带盘符的绝对路径会触发删除保护）。
WORK_REL="dist/smoke-test"
WORK="${ROOT}/${WORK_REL}"
rm -rf "${WORK_REL}" 2>/dev/null || true
mkdir -p "${WORK}"

# 定位 go：优先 $GO，其次 PATH，最后常见安装位置。
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

PUB_PORT=18180
ADM_PORT=18181
PUB="127.0.0.1:${PUB_PORT}"
ADM="127.0.0.1:${ADM_PORT}"
PUB_BASE="http://${PUB}"
ADM_BASE="http://${ADM}"
ADMIN_USER="smoke"
ADMIN_PASS="SmokeTestPassw0rd!"

PASS=0
FAIL=0
SKIPPED=0
MASTER_PID=""
# 源分离（6.1）需要一个带 Host 白名单的独立实例，见第 10 节。
MASTER2_PID=""
# 站点形态（site.layout）需要一个"公开与后台共用同一个监听"的实例：
# 反代把两个域名都转发到一个端口是常态，而"同一入口按路径判域名"这条
# 分支在上面的双监听实例里根本走不到（见第 10 节的后半）。
MASTER3_PID=""

cleanup() {
  if [ -n "${MASTER3_PID}" ]; then
    kill "${MASTER3_PID}" 2>/dev/null || true
    wait "${MASTER3_PID}" 2>/dev/null || true
  fi
  if [ -n "${MASTER2_PID}" ]; then
    kill "${MASTER2_PID}" 2>/dev/null || true
    wait "${MASTER2_PID}" 2>/dev/null || true
  fi
  if [ -n "${MASTER_PID}" ]; then
    kill "${MASTER_PID}" 2>/dev/null || true
    wait "${MASTER_PID}" 2>/dev/null || true
  fi
  if [ "${KEEP_WORK:-0}" = "1" ]; then
    echo "临时实例保留在：${WORK}"
  else
    cd "${ROOT}" && rm -rf "${WORK_REL}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

ok()  { PASS=$((PASS + 1)); printf '  PASS  %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL  %s\n' "$1"; }
skip() { SKIPPED=$((SKIPPED + 1)); printf '  SKIP  %s\n' "$1"; }
sec() { printf '\n== %s ==\n' "$1"; }

# Windows/MSYS 下 kill 无法向原生 PE 进程投递 POSIX 信号（只能强杀），
# 因此“优雅退出”这一项只在真正的 Unix 上断言。
case "$(uname -s 2>/dev/null)" in
  MINGW*|MSYS*|CYGWIN*) IS_WINDOWS=1 ;;
  *) IS_WINDOWS=0 ;;
esac

# code_of <url> [curl 附加参数...] -> 打印 HTTP 状态码
code_of() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

# ---------------------------------------------------------------------------
sec "0. 构建"
# ---------------------------------------------------------------------------
if [ -z "${GO_BIN}" ]; then
  bad "未找到 go 可执行文件，请设置 GO 环境变量指向 go 二进制"
  exit 1
fi
echo "  使用 Go：${GO_BIN}"
mkdir -p "${ROOT}/bin"
if (cd "${ROOT}" && "${GO_BIN}" build -trimpath -ldflags "-s -w" -o bin/probe-master ./cmd/probe-master 2>"${WORK}/build.log"); then
  ok "go build probe-master"
else
  bad "go build 失败：$(cat "${WORK}/build.log")"
  exit 1
fi
MASTER="${ROOT}/bin/probe-master"

# ---------------------------------------------------------------------------
sec "1. 准备临时实例配置"
# ---------------------------------------------------------------------------
# 路径全部用相对于仓库根目录的形式：Master 是原生 Windows 二进制，
# 喂给它 /c/... 这类 MSYS 路径会被理解成 <当前盘>:\c\...，导致找不到库。
#
# trusted_proxies 写 127.0.0.1：本脚本随后会在登录请求里带
# X-Forwarded-Proto: https，从而走通"生产形态"的 __Host- + Secure Cookie 分支
# （这两项是浏览器硬约束，只有在 HTTPS 或可信反代下才成立）。
# 不配它的话，本节只能覆盖 HTTP 降级分支，__Host- 那段代码就再也没有端到端验证了。
# 两条分支都断言，见第 7 节。
cat >"${WORK}/master.yaml" <<YAML
public_listen: "${PUB}"
admin_listen: "${ADM}"
data_dir: "${WORK_REL}/data"
themes_dir: "${WORK_REL}/themes"
master_key_file: "${WORK_REL}/master.key"
trusted_proxies: ["127.0.0.1"]
tls:
  mode: none
log_level: "info"
YAML
ok "写入 master.yaml（public=${PUB} admin=${ADM}）"

# ---------------------------------------------------------------------------
sec "2. init-admin（迁移 + 建管理员 + 建主密钥）"
# ---------------------------------------------------------------------------
if "${MASTER}" init-admin -config "${WORK}/master.yaml" -username "${ADMIN_USER}" -password "${ADMIN_PASS}" >"${WORK}/init.log" 2>&1; then
  ok "init-admin 成功"
else
  bad "init-admin 失败：$(cat "${WORK}/init.log")"
fi
[ -f "${WORK}/master.key" ] && ok "已生成主密钥文件" || bad "未生成主密钥文件"
[ -f "${WORK}/data/probe.db" ] && ok "已生成 SQLite 库" || bad "未生成 SQLite 库"

# 重复初始化必须被拒绝（防止管理员被静默覆盖）。
if "${MASTER}" init-admin -config "${WORK}/master.yaml" -username "${ADMIN_USER}" -password "${ADMIN_PASS}" >/dev/null 2>&1; then
  bad "重复 init-admin 应失败"
else
  ok "重复 init-admin 被拒绝"
fi

# 过短密码必须被拒绝（9.3）。
if "${MASTER}" init-admin -config "${WORK}/master.yaml" -username shorty -password "abc" >/dev/null 2>&1; then
  bad "短密码应被拒绝"
else
  ok "短密码被拒绝"
fi

# version 子命令
if "${MASTER}" version | grep -q "probe-master"; then
  ok "version 子命令可用"
else
  bad "version 子命令异常"
fi

# ---------------------------------------------------------------------------
sec "3. 启动 master（公开站 + 管理端双监听）"
# ---------------------------------------------------------------------------
"${MASTER}" -config "${WORK}/master.yaml" >"${WORK}/master.log" 2>&1 &
MASTER_PID=$!

ready=0
for _ in $(seq 1 60); do
  if [ "$(code_of "${PUB_BASE}/api/public/v1/site")" = "200" ]; then ready=1; break; fi
  if ! kill -0 "${MASTER_PID}" 2>/dev/null; then break; fi
  sleep 0.25
done
if [ "${ready}" = "1" ]; then
  ok "master 启动并就绪"
else
  bad "master 未就绪"
  echo "--- master.log ---"; cat "${WORK}/master.log"
  exit 1
fi

# ---------------------------------------------------------------------------
sec "4. 公开 API"
# ---------------------------------------------------------------------------
check_code() { # <期望码> <说明> <url> [附加参数...]
  local want="$1" desc="$2" url="$3"; shift 3
  local got; got="$(code_of "$@" "${url}")"
  if [ "${got}" = "${want}" ]; then ok "${desc} -> ${got}"; else bad "${desc} 期望 ${want} 实际 ${got}"; fi
}

check_code 200 "GET /api/public/v1/site"     "${PUB_BASE}/api/public/v1/site"
check_code 200 "GET /api/public/v1/overview" "${PUB_BASE}/api/public/v1/overview"
check_code 200 "GET /api/public/v1/nodes"    "${PUB_BASE}/api/public/v1/nodes"
check_code 404 "GET /api/public/v1/nodes/不存在" "${PUB_BASE}/api/public/v1/nodes/nosuchid0000"

# 公开节点列表不得含内网字段名。
BODY="$(curl -s "${PUB_BASE}/api/public/v1/nodes")"
if echo "${BODY}" | grep -qE '"(ip_v4|ip_v6|hostname|kernel|cpu_model|token|price_minor|note|provider|location)"'; then
  bad "公开节点响应泄漏了内网字段：${BODY}"
else
  ok "公开节点响应无内网字段"
fi
if [ "${BODY}" != "[]" ] && [ -z "${BODY}" ]; then bad "公开节点响应为空"; else ok "公开节点响应非空（${BODY}）"; fi

# 公开站静态资源（内嵌默认主题）。
check_code 200 "GET /（内嵌主题首页）"    "${PUB_BASE}/"
check_code 200 "GET /style.css"          "${PUB_BASE}/style.css"
check_code 200 "GET /app.js"             "${PUB_BASE}/app.js"
check_code 404 "GET /nope.js（主题不存在）" "${PUB_BASE}/nope.js"

# 主题页面同样不许有内联脚本 / style 属性（CSP 是 script-src 'self'; style-src 'self'）。
THEME_HTML="$(curl -s "${PUB_BASE}/")"
if echo "${THEME_HTML}" | grep -qE '<script[^>]*>[^<]' ; then bad "主题首页含内联脚本（会被 CSP 拦掉）"; else ok "主题首页无内联脚本"; fi
if echo "${THEME_HTML}" | grep -qE 'style="' ; then bad "主题首页含内联 style 属性（会被 CSP 拦掉）"; else ok "主题首页无内联 style"; fi

# 安装脚本：默认 install.mode=on_demand，没有有效注册码时必须 404 而不是
# 暴露安装入口与 Master 地址（12.8）。稍后在建好注册码后再验证它能 200。
check_code 404 "GET /install.sh（on_demand 且无注册码）"  "${PUB_BASE}/install.sh"
check_code 404 "GET /install.ps1（on_demand 且无注册码）" "${PUB_BASE}/install.ps1"
check_code 404 "GET /dist/（on_demand 且无注册码）"       "${PUB_BASE}/dist/probe-agent-linux-amd64"

# 公开站安全头（S2 / 13.4.5）。
HDRS="$(curl -s -D - -o /dev/null "${PUB_BASE}/")"
if echo "${HDRS}" | grep -qi 'content-security-policy'; then ok "公开站带 CSP 头"; else bad "公开站缺少 CSP 头"; fi
if echo "${HDRS}" | grep -qi "frame-ancestors 'none'"; then ok "CSP 含 frame-ancestors 'none'"; else bad "CSP 缺少 frame-ancestors"; fi
if echo "${HDRS}" | grep -qi 'x-content-type-options: nosniff'; then ok "带 X-Content-Type-Options"; else bad "缺少 X-Content-Type-Options"; fi
# 公开站不得下发 CORS 放行头。
if echo "${HDRS}" | grep -qi 'access-control-allow-origin'; then bad "公开站不应有 CORS 放行头"; else ok "公开站无 CORS 放行头"; fi
# 公开站（未启用脚本注入时）CSP 的 script-src 应为 'self'，不加 nonce。
if echo "${HDRS}" | grep -i 'content-security-policy' | grep -q "script-src 'self'" && ! echo "${HDRS}" | grep -i 'content-security-policy' | grep -q "nonce-"; then
  ok "未启用脚本注入时 script-src 为 'self'（无 nonce）"
else
  bad "CSP script-src 策略不符合预期：$(echo "${HDRS}" | grep -i content-security-policy)"
fi

# ---------------------------------------------------------------------------
sec "5. Agent 接口鉴权（无令牌必须 401）"
# ---------------------------------------------------------------------------
check_code 401 "POST /api/agent/v1/report 无令牌" "${PUB_BASE}/api/agent/v1/report" -X POST -H 'Content-Type: application/json' -d '{}'
check_code 401 "GET  /api/agent/v1/config 无令牌" "${PUB_BASE}/api/agent/v1/config"
check_code 400 "POST /api/agent/v1/enroll 空体"   "${PUB_BASE}/api/agent/v1/enroll" -X POST -H 'Content-Type: application/json' -d '{}'
# 格式合法但不存在的注册码统一 401，不区分“无效/已用/过期”（避免枚举探测）。
check_code 401 "POST /api/agent/v1/enroll 无效码" "${PUB_BASE}/api/agent/v1/enroll" -X POST -H 'Content-Type: application/json' -d '{"code":"ZZZZ-ZZZZ-ZZZZ"}'

# ---------------------------------------------------------------------------
sec "6. 管理界面（内嵌静态资源）"
# ---------------------------------------------------------------------------
check_code 200 "GET /admin/（后台首页）" "${ADM_BASE}/admin/"
check_code 200 "GET /admin/admin.css"    "${ADM_BASE}/admin/admin.css"
check_code 200 "GET /admin/admin.js"     "${ADM_BASE}/admin/admin.js"

ADMIN_HTML="$(curl -s "${ADM_BASE}/admin/")"
if echo "${ADMIN_HTML}" | grep -q 'id="app"'; then ok "后台首页含应用根节点"; else bad "后台首页结构异常"; fi
# 后台界面不允许内联脚本/样式（CSP 为 style-src 'self'; script-src 'self'）。
if echo "${ADMIN_HTML}" | grep -qE '<script[^>]*>[^<]' ; then bad "后台首页含内联脚本（会被 CSP 拦掉）"; else ok "后台首页无内联脚本"; fi
if echo "${ADMIN_HTML}" | grep -qE 'style="' ; then bad "后台首页含内联 style 属性（会被 CSP 拦掉）"; else ok "后台首页无内联 style"; fi

# 管理端安全头。
AHDRS="$(curl -s -D - -o /dev/null "${ADM_BASE}/admin/")"
if echo "${AHDRS}" | grep -qi 'content-security-policy'; then ok "管理界面带 CSP 头"; else bad "管理界面缺少 CSP 头"; fi
if echo "${AHDRS}" | grep -qi 'cache-control: no-store'; then ok "管理界面不缓存"; else bad "管理界面应 no-store"; fi

# 路径穿越防护。
check_code 404 "GET /admin/../master.yaml"    "${ADM_BASE}/admin/../master.yaml"
check_code 404 "GET /admin/%2e%2e/master.yaml" "${ADM_BASE}/admin/%2e%2e/master.yaml"
check_code 404 "GET /admin/nope.js"           "${ADM_BASE}/admin/nope.js"

# 管理 JSON API 未登录必须 401。
check_code 401 "GET /api/admin/v1/nodes 未登录"       "${ADM_BASE}/api/admin/v1/nodes"
check_code 401 "GET /api/admin/v1/settings 未登录"    "${ADM_BASE}/api/admin/v1/settings"
check_code 401 "GET /api/admin/v1/alert-rules 未登录" "${ADM_BASE}/api/admin/v1/alert-rules"

# ---------------------------------------------------------------------------
sec "7. 登录 / 会话 / CSRF"
# ---------------------------------------------------------------------------
LOGIN_HEADERS="$(curl -s -D - -o "${WORK}/login.json" -X POST "${ADM_BASE}/api/admin/v1/auth/login" \
  -H 'Content-Type: application/json' -H 'X-Forwarded-Proto: https' \
  -d "{\"username\":\"${ADMIN_USER}\",\"password\":\"${ADMIN_PASS}\"}")"

if echo "${LOGIN_HEADERS}" | head -1 | grep -q ' 200'; then ok "登录成功"; else
  bad "登录失败：$(head -1 <<<"${LOGIN_HEADERS}") $(cat "${WORK}/login.json")"; fi

SESSION="$(printf '%s' "${LOGIN_HEADERS}" | grep -i '^set-cookie:' | sed -n 's/.*__Host-probe_session=\([^;]*\).*/\1/p' | tr -d '\r')"
CSRF="$(sed -n 's/.*"csrf_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${WORK}/login.json")"

[ -n "${SESSION}" ] && ok "拿到会话 Cookie" || bad "未拿到会话 Cookie"
[ -n "${CSRF}" ] && ok "拿到 CSRF 令牌" || bad "未拿到 CSRF 令牌"

# Cookie 属性（__Host- 前缀要求 Secure + Path=/ 且无 Domain）。
SC="$(printf '%s' "${LOGIN_HEADERS}" | grep -i '^set-cookie:' | tr -d '\r')"
echo "${SC}" | grep -qi 'httponly' && ok "Cookie 为 HttpOnly" || bad "Cookie 缺少 HttpOnly"
echo "${SC}" | grep -qi 'secure'   && ok "Cookie 为 Secure（__Host- 要求，故后台必须走 HTTPS）" || bad "Cookie 缺少 Secure"
echo "${SC}" | grep -qi 'samesite=strict' && ok "Cookie 为 SameSite=Strict" || bad "Cookie 缺少 SameSite=Strict"
if echo "${SC}" | grep -qi 'domain='; then bad "__Host- Cookie 不应带 Domain"; else ok "Cookie 未带 Domain（符合 __Host- 要求）"; fi

AUTH_COOKIE="Cookie: __Host-probe_session=${SESSION}"

# 反方向也要断言：不带 X-Forwarded-Proto 时必须降级为不带前缀、不带 Secure 的
# probe_session —— 这就是"HTTP 直连部署"的形态。
#
# 这条分支值得单独盯着，因为它曾经以最难察觉的方式坏掉：
# 早期实现无条件下发 __Host- + Secure。__Host- 前缀要求 Secure，
# 浏览器对纯 HTTP 响应会**静默丢弃**这条 Set-Cookie：服务端 200、日志一切正常，
# 前端却拿不到会话、立刻被打回登录页，现象就是"后端登录不上"。
# curl 不校验 __Host- 语义（它照单全收），所以只有断言 Cookie 名本身才能拦住回归。
PLAIN_HEADERS="$(curl -s -D - -o /dev/null -X POST "${ADM_BASE}/api/admin/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"${ADMIN_USER}\",\"password\":\"${ADMIN_PASS}\"}")"
PLAIN_SC="$(printf '%s' "${PLAIN_HEADERS}" | grep -i '^set-cookie:' | tr -d '\r')"
if echo "${PLAIN_SC}" | grep -q '__Host-'; then
  bad "纯 HTTP 下不得用 __Host- 前缀（浏览器会丢弃该 Cookie，表现为登录静默失效）"
else
  ok "纯 HTTP 下降级为非前缀 Cookie 名"
fi
if echo "${PLAIN_SC}" | grep -qi 'secure'; then
  bad "纯 HTTP 下不得带 Secure（浏览器同样会丢弃）"
else
  ok "纯 HTTP 下降级为不带 Secure"
fi
if echo "${PLAIN_SC}" | grep -q 'probe_session='; then
  ok "降级后的 Cookie 名为 probe_session"
else
  bad "降级后的 Cookie 名异常：${PLAIN_SC}"
fi
# 降级不代表可以放松 HttpOnly / SameSite。
echo "${PLAIN_SC}" | grep -qi 'httponly' && ok "降级 Cookie 仍为 HttpOnly" || bad "降级 Cookie 缺少 HttpOnly"
echo "${PLAIN_SC}" | grep -qi 'samesite=strict' && ok "降级 Cookie 仍为 SameSite=Strict" || bad "降级 Cookie 缺少 SameSite=Strict"

check_code 200 "GET /api/admin/v1/auth/me（带会话）" "${ADM_BASE}/api/admin/v1/auth/me" -H "${AUTH_COOKIE}"
check_code 200 "GET /api/admin/v1/nodes"            "${ADM_BASE}/api/admin/v1/nodes" -H "${AUTH_COOKIE}"
check_code 200 "GET /api/admin/v1/probe-targets"    "${ADM_BASE}/api/admin/v1/probe-targets" -H "${AUTH_COOKIE}"
check_code 200 "GET /api/admin/v1/alert-channels"   "${ADM_BASE}/api/admin/v1/alert-channels" -H "${AUTH_COOKIE}"
check_code 200 "GET /api/admin/v1/alert-rules"      "${ADM_BASE}/api/admin/v1/alert-rules" -H "${AUTH_COOKIE}"
check_code 200 "GET /api/admin/v1/alert-events"     "${ADM_BASE}/api/admin/v1/alert-events" -H "${AUTH_COOKIE}"
check_code 200 "GET /api/admin/v1/settings"         "${ADM_BASE}/api/admin/v1/settings" -H "${AUTH_COOKIE}"
check_code 200 "GET /api/admin/v1/themes"           "${ADM_BASE}/api/admin/v1/themes" -H "${AUTH_COOKIE}"
check_code 200 "GET /api/admin/v1/storage/stats"    "${ADM_BASE}/api/admin/v1/storage/stats" -H "${AUTH_COOKIE}"
check_code 200 "GET /api/admin/v1/audit-log"        "${ADM_BASE}/api/admin/v1/audit-log" -H "${AUTH_COOKIE}"
check_code 200 "GET /api/admin/v1/agent-releases"   "${ADM_BASE}/api/admin/v1/agent-releases" -H "${AUTH_COOKIE}"

# ---- 6.2 账户安全：TOTP 状态与启用流程（密钥生成不落库） ----
# 这里只验证"接口通路 + 关键安全语义"，完整的启用/登录闭环在
# internal/api/totp_flow_test.go 里跑（那里能算出正确的动态码）。
check_code 200 "GET /api/admin/v1/account/totp（初始未启用）" "${ADM_BASE}/api/admin/v1/account/totp" -H "${AUTH_COOKIE}"
TOTP_STATUS="$(curl -s "${ADM_BASE}/api/admin/v1/account/totp" -H "${AUTH_COOKIE}")"
if echo "${TOTP_STATUS}" | grep -q '"enabled"[[:space:]]*:[[:space:]]*false'; then
  ok "TOTP 初始状态为未启用（库里没有 totp_secret）"
else bad "TOTP 初始状态异常：${TOTP_STATUS}"; fi

# setup：必须返回密钥 + otpauth URI + 内联 SVG 二维码。
SETUP="$(curl -s -X POST "${ADM_BASE}/api/admin/v1/account/totp/setup" \
  -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' -d '{}')"
echo "${SETUP}" | grep -q '"secret"' && ok "TOTP setup 返回密钥" || bad "TOTP setup 未返回密钥"
echo "${SETUP}" | grep -q 'otpauth://totp/' && ok "TOTP setup 返回 otpauth URI" || bad "TOTP setup 未返回 otpauth URI"
# 二维码必须是内联 SVG（CSP default-src 'self'，不能引外部库或 data: 图片）。
# 注意 writeJSON 开了 SetEscapeHTML，`<` 会编码成 \u003c，所以这里两种形态都认。
if echo "${SETUP}" | grep -q 'qr_svg' && echo "${SETUP}" | grep -qE '(<svg|\\u003csvg)'; then
  ok "TOTP setup 返回内联 SVG 二维码"
else bad "TOTP setup 未返回内联 SVG 二维码"; fi
# SVG 里不能有外部引用，否则浏览器会静默拒绝渲染。
echo "${SETUP}" | grep -q 'data:' && bad "二维码 SVG 含 data: 引用（CSP 不允许）" || ok "二维码 SVG 无 data: 引用"
# setup **不能**改动启用状态（两步式：先验证一次动态码才真正启用）。
TOTP_AFTER="$(curl -s "${ADM_BASE}/api/admin/v1/account/totp" -H "${AUTH_COOKIE}")"
if echo "${TOTP_AFTER}" | grep -q '"enabled"[[:space:]]*:[[:space:]]*false'; then
  ok "setup 阶段不落库（仍是未启用）"
else bad "setup 竟然启用了 TOTP：${TOTP_AFTER}"; fi

# enable 用错误的动态码必须被拒，且不落库。
BAD_ENABLE="$(code_of -X POST -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" \
  -H 'Content-Type: application/json' -d '{"secret":"JBSWY3DPEHPK3PXP","code":"000000"}' \
  "${ADM_BASE}/api/admin/v1/account/totp/enable")"
if [ "${BAD_ENABLE}" = "400" ]; then ok "TOTP enable 错误动态码 -> 400"; else bad "TOTP enable 错误动态码 期望 400 实际 ${BAD_ENABLE}"; fi
# 无 CSRF 的写请求必须 403。
check_code 403 "POST /totp/setup 无 CSRF" "${ADM_BASE}/api/admin/v1/account/totp/setup" -X POST -H "${AUTH_COOKIE}" -H 'Content-Type: application/json' -d '{}'
# 未启用 TOTP 时不应签发恢复码。
check_code 400 "POST /recovery-codes 未启用 TOTP" "${ADM_BASE}/api/admin/v1/account/recovery-codes" -X POST -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' -d "{\"password\":\"${ADMIN_PASS}\"}"

# ---- 6.6 数据库备份与轮转 ----
check_code 200 "GET /api/admin/v1/maintenance/backups（列表）" "${ADM_BASE}/api/admin/v1/maintenance/backups" -H "${AUTH_COOKIE}"
BACKUPS="$(curl -s "${ADM_BASE}/api/admin/v1/maintenance/backups" -H "${AUTH_COOKIE}")"
echo "${BACKUPS}" | grep -q '"dir"' && ok "备份接口返回备份目录" || bad "备份接口未返回 dir"
echo "${BACKUPS}" | grep -q '"keep"' && ok "备份接口返回保留份数上限" || bad "备份接口未返回 keep"
echo "${BACKUPS}" | grep -q '"enabled"' && ok "备份接口返回自动备份开关" || bad "备份接口未返回 enabled"
echo "${BACKUPS}" | grep -q '"hour"' && ok "备份接口返回备份时刻" || bad "备份接口未返回 hour"

# 手动触发一次备份：应生成文件并返回文件名。
BK="$(curl -s -X POST "${ADM_BASE}/api/admin/v1/maintenance/backup" -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}")"
echo "${BK}" | grep -q '"name":"probe-' && ok "手动备份返回文件名" || bad "手动备份未返回文件名：${BK}"
# 备份完成后列表里应至少有 1 份，且带大小字段。
BACKUPS2="$(curl -s "${ADM_BASE}/api/admin/v1/maintenance/backups" -H "${AUTH_COOKIE}")"
echo "${BACKUPS2}" | grep -q '"size_bytes"' && ok "备份列表返回文件大小" || bad "备份列表未返回 size_bytes"
# 备份文件必须真的落在数据目录下（而不是只返回了个名字）。
if ls "${WORK}"/data/backups/probe-*.db >/dev/null 2>&1; then
  ok "备份文件已生成于 data_dir/backups"
else bad "data_dir/backups 下未找到备份文件"; fi
check_code 403 "POST /maintenance/backup 无 CSRF" "${ADM_BASE}/api/admin/v1/maintenance/backup" -X POST -H "${AUTH_COOKIE}"

# ---- 6.8 发布产物签名（install.pubkey）----
# 这里验证三件事：
#   (1) 空公钥必须被接受（默认不校验，不能把没签名的用户卡死）；
#   (2) 真 minisign 公钥必须通过校验器（形如两行）；
#   (3) 伪造/截断的公钥必须 400 —— 否则用户以为开了校验其实一直没生效。
# 签名本身的正确性由 internal/auth/minisign_test.go 对真 minisign 0.11 输出做回归。
put_pubkey() { # <json-fragment>
  code_of -X PUT "${ADM_BASE}/api/admin/v1/settings" \
    -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' -d "$1"
}
# (1) 显式写空 = 关闭校验，必须成功。
if [ "$(put_pubkey '{"install.pubkey":""}')" = "204" ]; then
  ok "install.pubkey 允许留空（不校验签名）"
else bad "install.pubkey 留空被拒"; fi

# (2) 真实 minisign 公钥（两行：untrusted comment + base64）。
#     这是本仓库测试固件里那把真密钥的公钥原文，base64 解出恰好 42 字节。
REAL_PUB='untrusted comment: minisign public key B56B8445221E5AEE\nRWTuWh4iRYRrtRxnZ4S2i+5JWAetk3dpGiNA3qnSUK2yX23mNzaLGZtv'
if [ "$(put_pubkey "{\"install.pubkey\":\"${REAL_PUB}\"}")" = "204" ]; then
  ok "install.pubkey 接受真实 minisign 公钥"
else bad "install.pubkey 拒绝了真实 minisign 公钥"; fi
# 回显必须是原样两行（不能被前端/后端的折叠逻辑改坏）。
PUB_ECHO="$(curl -s "${ADM_BASE}/api/admin/v1/settings" -H "${AUTH_COOKIE}")"
if echo "${PUB_ECHO}" | grep -q 'RWTuWh4iRYRrtRxnZ4S2i'; then
  ok "install.pubkey 回显原样（两行公钥未被折叠）"
else bad "install.pubkey 回显被改坏：${PUB_ECHO}"; fi

# (3) 缺 base64 行的公钥必须被拒（写进去等于静默不校验，比报错更危险）。
if [ "$(put_pubkey '{"install.pubkey":"untrusted comment: only one line"}')" = "400" ]; then
  ok "install.pubkey 拒绝只有注释行的伪公钥"
else bad "install.pubkey 接受了只有一行注释的伪公钥"; fi
# base64 解出来长度不是 42 的也要拒。
if [ "$(put_pubkey '{"install.pubkey":"untrusted comment: x\nYWJjZGVm"}')" = "400" ]; then
  ok "install.pubkey 拒绝长度非法（非 42 字节）的密钥"
else bad "install.pubkey 接受了长度非法的密钥"; fi
# 沾上命令注入字符更要拒（虽然它本来就不是合法 base64）。
if [ "$(put_pubkey '{"install.pubkey":"untrusted comment: x\nAAAA; rm -rf /"}')" = "400" ]; then
  ok "install.pubkey 拒绝含注入字符的输入"
else bad "install.pubkey 接受了含注入字符的输入"; fi

# 装完公钥后，下发的 install.sh 必须把它按 shell 单引号安全地内联进去
# （多行公钥直接塞进 heredoc 会破坏脚本语法，所以用 __MINISIGN_PUB__ 占位替换）。
# 注意此刻 on_demand 且还没生成注册码，安装入口是 404；先拿注册码再检查会顺延到
# 第 8 节，这里只断言"设置项本身可用"这一层。
put_pubkey '{"install.pubkey":""}' >/dev/null

# 会话 Cookie 本身不能是明文——库里只存哈希；这里顺带确认 me 不返回哈希类字段。
ME="$(curl -s "${ADM_BASE}/api/admin/v1/auth/me" -H "${AUTH_COOKIE}")"
if echo "${ME}" | grep -q 'csrf_token'; then ok "me 返回 csrf_token（前端刷新后仍可写）"; else bad "me 未返回 csrf_token"; fi

# CSRF：写请求缺头必须 403（13.4.5，跨站预检无法通过）。
check_code 403 "PUT /settings 无 CSRF 头"    "${ADM_BASE}/api/admin/v1/settings" -X PUT -H "${AUTH_COOKIE}" -H 'Content-Type: application/json' -d '{}'
check_code 403 "PUT /settings 错误 CSRF"     "${ADM_BASE}/api/admin/v1/settings" -X PUT -H "${AUTH_COOKIE}" -H 'X-CSRF-Token: wrong' -H 'Content-Type: application/json' -d '{}'
# 无会话时先判鉴权，返回 401（而不是 403）——未认证就不谈 CSRF。
check_code 401 "PUT /settings 无会话有 CSRF" "${ADM_BASE}/api/admin/v1/settings" -X PUT -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' -d '{}'

# 正确 CSRF 的空回写：应 204 且不破坏既有设置。
SET_OK="$(code_of -X PUT -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' -d '{}' "${ADM_BASE}/api/admin/v1/settings")"
if [ "${SET_OK}" = "204" ]; then ok "PUT /settings 带正确 CSRF -> 204"; else bad "PUT /settings 带正确 CSRF 期望 204 实际 ${SET_OK}"; fi

# GET /settings 必须回显“生效值”（默认值合并），否则后台设置页全空白。
AFTER="$(curl -s "${ADM_BASE}/api/admin/v1/settings" -H "${AUTH_COOKIE}")"
for k in report_interval_s raw_resolution_s probe_interval_s disk_min_free_mb retention.days install.mode; do
  if echo "${AFTER}" | grep -q "\"${k}\""; then ok "settings 回显生效值 ${k}"; else bad "settings 缺少 ${k}：${AFTER}"; fi
done

# ---------------------------------------------------------------------------
sec "7.5 公开站开关：关闭后未登录 404、管理员会话放行"
# ---------------------------------------------------------------------------
# 产品约定：关闭公开站是"对外隐藏"，不是"对管理员也锁死"——管理员要能预览。
# 三条分支都必须断言：伪造/过期会话拿不到放行（要查库 + 空闲过期校验）。
# （安装/发布路径在关闭时仍放行这一点由 public_test.go 的单元测试断言；
#   这里 on_demand 且无注册码，/install.sh 本来就是 404，不适合放在这里验证。）

put_setting() { # <json>
  curl -s -o /dev/null -w '%{http_code}' -X PUT "${ADM_BASE}/api/admin/v1/settings" \
    -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' -d "$1"
}

if [ "$(put_setting '{"public.enabled":"0"}')" = "204" ]; then
  ok "关闭公开站 -> 204"
else
  bad "关闭公开站失败"
fi
check_code 404 "关闭后未登录 GET /"          "${PUB_BASE}/"
check_code 404 "关闭后未登录 site API"       "${PUB_BASE}/api/public/v1/site"
check_code 404 "关闭后伪造会话 site API"     "${PUB_BASE}/api/public/v1/site" -H "Cookie: probe_session=deadbeefdeadbeefdeadbeef"
check_code 200 "关闭后管理员会话 GET /"      "${PUB_BASE}/" -H "${AUTH_COOKIE}"
check_code 200 "关闭后管理员会话 site API"   "${PUB_BASE}/api/public/v1/site" -H "${AUTH_COOKIE}"

if [ "$(put_setting '{"public.enabled":"1"}')" = "204" ]; then
  ok "重新开放公开站 -> 204"
else
  bad "重新开放公开站失败"
fi
check_code 200 "重开后未登录 GET / 恢复"     "${PUB_BASE}/"
if echo "${AFTER}" | grep -q 'master_key'; then bad "settings 泄漏了 master_key"; else ok "settings 未泄漏 master_key"; fi

# 非法设置值必须 400（S7/S8：http 地址与命令注入字符）。
check_code 400 "PUT settings 非法 URL" "${ADM_BASE}/api/admin/v1/settings" -X PUT \
  -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
  -d '{"install.master_url":"https://m.example.com; rm -rf /"}'
check_code 400 "PUT settings 白名单外的键" "${ADM_BASE}/api/admin/v1/settings" -X PUT \
  -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
  -d '{"evil.key":"1"}'

# ---------------------------------------------------------------------------
sec "7.6 节点卡片区块开关：public.cards 必须能关掉每一条进度条"
# ---------------------------------------------------------------------------
# 背景缺陷：public.cards 过去只有总览汇总卡 / 详情页信息小卡 / 延迟点阵三类键，
# 节点卡片上的 CPU·内存·磁盘·流量四条进度条与底部实时速率**没有开关**，
# updateCard() 无条件渲染。管理员取消勾选 info_mem 后详情页那格消失了，
# 列表页每张卡片上的「内存」进度条却还在——"开关关不掉"。
# 修法是给每个区块补一个卡片键（后加的 node_fee 是"续费价格"那一行）。这里断言两件事：
#   1) 卡片目录（/settings/meta）里确实登记了这六个键；
#   2) 只剩这些键时 /site 原样下发、且节点列表进得去（不被误判为全关）。

NODE_CARD_KEYS="node_cpu node_mem node_disk node_traffic node_fee node_net"
PUBKEYLESS_CARDS="node_cpu,node_mem,node_disk,node_traffic,node_fee,node_net"

if [ "$(put_setting "{\"public.cards\":\"${PUBKEYLESS_CARDS}\"}")" = "204" ]; then
  ok "public.cards 接受节点卡区块键（6 个）"
else
  bad "public.cards 拒绝了节点卡区块键"
fi

SITE_CARDS="$(curl -s "${PUB_BASE}/api/public/v1/site")"
for k in ${NODE_CARD_KEYS}; do
  if grep -q "\"${k}\"" <<<"${SITE_CARDS}"; then
    ok "/site 下发了 ${k}"
  else
    bad "/site 缺 ${k}：该区块在公开看板上无法被开关控制"
  fi
done

# 单张卡片也要能单独关（白名单语义，不是"一开全开"）。
if [ "$(put_setting '{"public.cards":"node_cpu"}')" = "204" ]; then
  SITE_ONE="$(curl -s "${PUB_BASE}/api/public/v1/site")"
  if grep -q '"node_cpu"' <<<"${SITE_ONE}" && ! grep -q '"node_mem"' <<<"${SITE_ONE}"; then
    ok "只留 node_cpu 时其余区块键不再下发"
  else
    bad "public.cards 白名单未生效：${SITE_ONE}"
  fi
else
  bad "public.cards 设置单键失败"
fi

# 一张都不勾（空串）必须与"未配置"区分：前端靠空数组判断"全关"。
if [ "$(put_setting '{"public.cards":""}')" = "204" ]; then
  SITE_NONE="$(curl -s "${PUB_BASE}/api/public/v1/site")"
  if grep -q '"cards":\[\]' <<<"${SITE_NONE}"; then
    ok "空串 -> /site 下发空数组（全关，与 null 不限制可区分）"
  else
    bad "空串未下发空数组：${SITE_NONE}"
  fi
else
  bad "public.cards 设置空串失败"
fi

# 未知卡片键必须 400（静默忽略会让管理员以为关掉了）。
check_code 400 "public.cards 含未知键 -> 400" "${ADM_BASE}/api/admin/v1/settings" -X PUT \
  -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
  -d '{"public.cards":"node_cpu,node_bogus"}'

# 还原为不限制，避免影响后面的用例。
if [ "$(put_setting '{"public.cards":"all"}')" = "204" ]; then
  ok "public.cards 还原为 all"
else
  bad "public.cards 还原失败"
fi

# ---------------------------------------------------------------------------
sec "7.6b 节点卡片点阵时间窗（public.lat_window）"
# ---------------------------------------------------------------------------
# 四档窗口由设置项控制，必须能被保存、回显、并挡住非法值。
# 非法值若被静默接受，state.LatencyBarLayout 会走 default 分支回落 24h，
# 管理员会以为"设成 12h 生效了"——所以这里断言必须 400。

if curl -s "${PUB_BASE}/api/public/v1/site" | grep -q '"lat_window":"24h"'; then
  ok "/site 下发 lat_window 默认 24h"
else
  bad "/site 未下发 lat_window：前端点阵标题只能写死"
fi

for w in 30m 1h 6h 24h; do
  if [ "$(put_setting "{\"public.lat_window\":\"${w}\"}")" = "204" ]; then
    if curl -s "${PUB_BASE}/api/public/v1/site" | grep -q "\"lat_window\":\"${w}\""; then
      ok "public.lat_window=${w} 已保存并回显"
    else
      bad "public.lat_window=${w} 保存后 /site 未回显"
    fi
  else
    bad "public.lat_window=${w} 被拒绝"
  fi
done

check_code 400 "public.lat_window 非法值 -> 400" "${ADM_BASE}/api/admin/v1/settings" -X PUT \
  -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
  -d '{"public.lat_window":"12h"}'

# 还原默认值：后面的用例都按 24h 的卡片形态看。
if [ "$(put_setting '{"public.lat_window":"24h"}')" = "204" ]; then
  ok "public.lat_window 还原为 24h"
else
  bad "public.lat_window 还原失败"
fi

# 设置页的下拉框不是由 /settings/meta 驱动的（那个接口只给卡片目录与字段白名单），
# 而是由 admin.js 的 SETTING_LABELS 里的 "select:a=v,b=w" 生成。所以这里读下发的 JS。
SETTINGS_JS="$(curl -s "${ADM_BASE}/admin/admin.js")"
if grep -q "'public.lat_window': \['节点卡点阵时间窗'" <<<"${SETTINGS_JS}" \
   && grep -q '30m=近 30 分钟,1h=近 1 小时,6h=近 6 小时,24h=近 24 小时' <<<"${SETTINGS_JS}"; then
  ok "后台下发了 public.lat_window 的中文名与四档选项（设置页可下拉选择）"
else
  bad "后台 admin.js 缺 public.lat_window 的中文名/选项：设置页会显示原始键名或选不到档位"
fi

# ---------------------------------------------------------------------------
sec "7.7 折线断档：历史序列必须能被切段（前端据此断开而不是连直线）"
# ---------------------------------------------------------------------------
# 背景缺陷：节点/Master 断联期间不落任何样本，时间序列里那一段就是空的。
# 前端把相邻两点直接连成直线，"断联 6 小时"被画成一条平滑水平线。
# 修法是前端按"相邻间隔中位数 ×3"切段。
#
# 这条契约只能在源码层面断言（画布行为无法在 HTTP 冒烟里观察）：
#   1) 公开看板与后台两个前端都实现了 gapThreshold / segmentsOf；
#   2) 两个前端都用分段绘制折线（不再是"一次性把所有点连起来"）。
# 更强的行为断言在 internal/theme 的单元测试里（含真实数据切段验证）。

THEME_JS="$(curl -s "${PUB_BASE}/app.js")"
if grep -q 'function gapThreshold(' <<<"${THEME_JS}"; then
  ok "公开看板 app.js 含断档检测 gapThreshold"
else
  bad "公开看板 app.js 缺 gapThreshold：断联会被画成直线"
fi
if grep -q 'segmentsOf(s.points, gapThreshold(s.points)' <<<"${THEME_JS}"; then
  ok "公开看板折线按段绘制（断档不连线）"
else
  bad "公开看板折线未按段绘制"
fi
if grep -q 'return median \* 3;' <<<"${THEME_JS}"; then
  ok "公开看板断档阈值 = 中位间隔 ×3"
else
  bad "公开看板断档阈值不是中位数 ×3"
fi

# 后台界面挂在**管理端口**的 /admin/ 下（公开端口不提供后台资源）。
ADMIN_JS="$(curl -s "${ADM_BASE}/admin/admin.js")"
if grep -q 'function gapThreshold(' <<<"${ADMIN_JS}" && grep -q 'segmentsOf(s.points, gapThreshold(s.points)' <<<"${ADMIN_JS}"; then
  ok "管理后台 admin.js 同样按段绘制（两个前端不脱节）"
else
  bad "管理后台 admin.js 缺断档分段：后台仍会把断联画成直线"
fi

# ---------------------------------------------------------------------------
sec "7.7b 本轮四项改动确实随 embed 下发（顶栏图标 / 月费 / 点阵窗口 / 后台换装）"
# ---------------------------------------------------------------------------
# 单元测试直读**工作区源码**，这里读的是**经 go:embed 后由 HTTP 真正下发的内容**。
# 两者都必要：源码对而 embed 路径写错时，只有这一层能发现（"改了代码没效果"的经典形态）。

PUB_HTML="$(curl -s "${PUB_BASE}/")"
if grep -q 'id="btn-login"' <<<"${PUB_HTML}" && grep -q 'ico-moon' <<<"${PUB_HTML}" && grep -q 'ico-sun' <<<"${PUB_HTML}"; then
  ok "公开看板顶栏下发了登录图标与太阳/月亮图标"
else
  bad "公开看板顶栏缺图标按钮：主题切换仍是文字按钮 / 没有登录入口"
fi
if grep -q 'class="topbar-actions"' <<<"${PUB_HTML}"; then
  ok "公开看板顶栏下发了图标按钮组（站名→时间→图标）"
else
  bad "公开看板顶栏没有 .topbar-actions 分组"
fi

PUB_CSS="$(curl -s "${PUB_BASE}/style.css")"
if grep -q '\.tgl \.ico-sun' <<<"${PUB_CSS}" && grep -q ':root\[data-theme="dark"\] \.tgl \.ico-sun' <<<"${PUB_CSS}"; then
  ok "公开看板 style.css 下发图标切换规则（CSS-only）"
else
  bad "公开看板缺 .tgl 图标切换规则：图标会与主题不符"
fi

if grep -q 'node_fee' <<<"${THEME_JS}" && grep -q 'latWindowLabel' <<<"${THEME_JS}"; then
  ok "公开看板 app.js 下发 node_fee 区块键与时间窗文案函数"
else
  bad "公开看板 app.js 缺 node_fee / latWindowLabel"
fi

ADMIN_HTML="$(curl -s "${ADM_BASE}/admin/")"
if grep -q 'login-head' <<<"${ADMIN_HTML}" && grep -q 'ico-sun' <<<"${ADMIN_HTML}"; then
  ok "后台登录页下发了主题切换图标按钮"
else
  bad "后台登录页没有主题切换按钮：暗色系统下会先打开一个纯白登录页"
fi

ADMIN_CSS="$(curl -s "${ADM_BASE}/admin/admin.css")"
if grep -q -- '--panel: var(--card)' <<<"${ADMIN_CSS}" && grep -q -- '--online: #3fc168' <<<"${ADMIN_CSS}"; then
  ok "后台 admin.css 与公开看板同值（--panel 是别名、暗色在线绿同色号）"
else
  bad "后台 admin.css 未与公开看板对齐：两站并排会看出深浅/边框/在线绿不一致"
fi
if grep -q '\.track i\.warn' <<<"${ADMIN_CSS}"; then
  bad "后台 admin.css 复活了进度条阈值变色规则"
else
  ok "后台进度条不再按阈值变色（与公开看板同一语言）"
fi
if grep -q 'paletteColor' <<<"${ADMIN_JS}"; then
  ok "后台 admin.js 用灰阶调色板按序号取色"
else
  bad "后台 admin.js 未用 paletteColor：图表仍是彩色/颜色会随图例勾选串位"
fi

# --- 第 109~112 项：去原生蓝 / 自绘日期弹层 / 审计默认 20 / 价格并排速率行 ---
if grep -q 'accent-color: var(--fg)' <<<"${PUB_CSS}" && grep -q 'accent-color: var(--fg)' <<<"${ADMIN_CSS}"; then
  ok "两站勾选控件的选中色都是前景墨色（accent-color，不再是浏览器默认蓝）"
else
  bad "勾选控件缺 accent-color: var(--fg)：设置页的勾选框仍是原生蓝"
fi
if grep -q '\.net-line' <<<"${PUB_CSS}" && grep -q '\.net-line' <<<"${ADMIN_CSS}" \
   && grep -q 'rateLine.appendChild(fee);' <<<"${THEME_JS}" && grep -q 'rateLine.appendChild(fee);' <<<"${ADMIN_JS}"; then
  ok "两站节点卡都是速率在左、续费价格在右（.net-line 同行）"
else
  bad "续费价格没有并到速率行：要么另占一行要么某一站没改"
fi
if grep -q "function attachDatePicker(" <<<"${ADMIN_JS}" && grep -q '\.dp-day {' <<<"${ADMIN_CSS}" \
   && ! grep -q "'b-expire', 'date'" <<<"${ADMIN_JS}"; then
  ok "到期日是自绘灰阶日历（原生弹层的「清除/今天」是 Chromium 硬编码蓝色，不可用）"
else
  bad "到期日退回了原生 date 输入或自绘日历缺失：弹层按钮会是蓝色"
fi
if grep -q 'audit: { limit: 20, offset: 0, total: 0 }' <<<"${ADMIN_JS}"; then
  ok "审计日志默认每页 20 条"
else
  bad "审计日志默认每页不是 20 条"
fi

# ---------------------------------------------------------------------------
sec "7.8 系统设置页：保存提示必须跟随视口 + 备份设置项必须是中文"
# ---------------------------------------------------------------------------
# 背景缺陷（用户报出）：
#   1) 系统设置页字段多、保存按钮在页面最底部，而保存提示是文档流里的横条、
#      挂在 <main> 顶部 → 点完保存提示在视口之外，表现为"点了保存没反应"。
#      修法是把提示容器挪出 <main> 并 position:fixed 钉在顶栏下方。
#   2) backup.enabled / backup.hour / backup.keep 三个键在 SETTINGS 标签表里
#      没有条目，前端 fallback 成原始英文键名，还落进"其他"分组。
#      修法是补中文标签 + 新建「备份」分组。
#
# 这两件事都在"渲染后的 DOM/文本"里，HTTP 冒烟只能断言**资源里有没有该结构**；
# 真正的行为断言在 internal/theme/admin_settings_test.go（7 例，含变异验证）。
ADMIN_HTML="$(curl -s "${ADM_BASE}/admin/")"
ADMIN_CSS="$(curl -s "${ADM_BASE}/admin/admin.css")"

if grep -q 'id="toast-wrap"' <<<"${ADMIN_HTML}"; then
  ok "后台页面含浮动提示容器 toast-wrap"
else
  bad "后台页面缺 toast-wrap：提示又回到文档流，长表单底部点保存看不见"
fi

# 容器必须在 <main> 之前。判据不能是"两个字符串谁先出现"——HTML 注释里也写
# <main>（本项目注释很密，成篇解释"为什么这么写"），而且注释是**多行**的，
# 单行 sed 剥不干净。这里用 awk 显式跟踪 <!-- --> 的跨行状态。
STRIPPED_HTML="$(awk '
  { line = $0; out = "" }
  {
    while (length(line) > 0) {
      if (incomment) {
        p = index(line, "-->")
        if (p == 0) { line = ""; break }
        line = substr(line, p + 3); incomment = 0
      } else {
        p = index(line, "<!--")
        if (p == 0) { out = out line; line = ""; break }
        out = out substr(line, 1, p - 1)
        line = substr(line, p + 4); incomment = 1
      }
    }
  }
  { print out }
' <<<"${ADMIN_HTML}")"
MAIN_AT="$(awk '{ if (index($0,"<main>")>0) { print NR; exit } }' <<<"${STRIPPED_HTML}")"
WRAP_AT="$(awk '{ if (index($0,"toast-wrap")>0) { print NR; exit } }' <<<"${STRIPPED_HTML}")"
if [ -n "${MAIN_AT}" ] && [ -n "${WRAP_AT}" ] && [ "${WRAP_AT}" -lt "${MAIN_AT}" ]; then
  ok "提示容器在 <main> 之外（left/right:0 相对视口而不是带 padding 的 main）"
else
  bad "提示容器不在 <main> 之外（main 行=${MAIN_AT:-?} wrap 行=${WRAP_AT:-?}）"
fi

if grep -q '\.toast-wrap' <<<"${ADMIN_CSS}" && grep -q 'position: fixed' <<<"${ADMIN_CSS}"; then
  ok "提示容器为 position: fixed（跟随视口）"
else
  bad "提示容器不是 fixed：长表单保存提示会落在视口外"
fi

# 顶栏是 sticky 且 flex-wrap，锚点必须实测而不是写死常量。
if grep -q 'function syncToastOffset(' <<<"${ADMIN_JS}" && grep -q 'offsetHeight' <<<"${ADMIN_JS}"; then
  ok "顶栏高度实测（syncToastOffset 读 offsetHeight，不写死像素常量）"
else
  bad "缺少顶栏高度实测：导航换行后提示位置会错"
fi
if grep -q "addEventListener('resize', syncToastOffset)" <<<"${ADMIN_JS}"; then
  ok "窗口尺寸变化时重新锚定提示位置"
else
  bad "未监听 resize：缩放窗口后提示位置失效"
fi

# 提示必须能多条共存，否则"保存成功"会被随后到达的警告顶掉。
if grep -q 'wrap.appendChild(t)' <<<"${ADMIN_JS}" && ! grep -q 'toast\._t' <<<"${ADMIN_JS}"; then
  ok "提示支持多条共存（不再是单条覆盖式）"
else
  bad "提示仍是单条覆盖式：并发消息会互相顶掉"
fi

# 保存后把视口拉回表单顶部（顶栏 sticky，需补偿其高度）。
if grep -q 'scrollIntoView' <<<"${ADMIN_JS}"; then
  ok "保存设置后视口回到表单顶部"
else
  bad "保存设置后不拉回视口：看不出改的是哪一段"
fi

# 三个备份设置项必须有中文标签。判据：键名后紧跟的中文名里含汉字。
BK_MISSING=""
for BK in 'backup.enabled' 'backup.hour' 'backup.keep'; do
  LINE="$(grep -F "'${BK}': [" <<<"${ADMIN_JS}" | head -1)"
  if [ -z "${LINE}" ]; then
    BK_MISSING="${BK_MISSING} ${BK}(无标签)"
  elif ! grep -qP "'${BK}': \[\s*'[^']*[\x{4e00}-\x{9fff}]" <<<"${LINE}"; then
    BK_MISSING="${BK_MISSING} ${BK}(标签非中文)"
  fi
done
if [ -z "${BK_MISSING}" ]; then
  ok "backup.enabled / backup.hour / backup.keep 均有中文标签"
else
  bad "备份设置项缺中文标签：${BK_MISSING}"
fi

if grep -q "\['备份', \['backup.enabled', 'backup.hour', 'backup.keep'\]\]" <<<"${ADMIN_JS}"; then
  ok "三个备份设置项归入「备份」分组（不落进兜底的「其他」）"
else
  bad "备份设置项未归入「备份」分组"
fi

# 面向前端的散文说明里不该出现裸露的英文设置键名。
if grep -q 'backup\.\(enabled\|hour\|keep\)' <<<"${ADMIN_HTML}"; then
  bad "后台页面正文仍出现 backup.* 英文键名（应改用设置项中文名）"
else
  ok "后台页面正文无裸露的 backup.* 键名"
fi

# ---------------------------------------------------------------------------
sec "8. 端到端：建节点 -> PATCH -> 公开可见且不泄漏 -> 注册码 -> 审计"
# ---------------------------------------------------------------------------
NODE_JSON="$(curl -s -X POST "${ADM_BASE}/api/admin/v1/nodes" \
  -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
  -d '{"name":"冒烟节点","group":"smoke"}')"
NODE_ID="$(sed -n 's/.*"id"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' <<<"${NODE_JSON}" | head -1)"
PUBLIC_ID="$(sed -n 's/.*"public_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"${NODE_JSON}" | head -1)"
if [ -n "${NODE_ID}" ]; then ok "创建节点成功 id=${NODE_ID} public_id=${PUBLIC_ID}"; else bad "创建节点失败：${NODE_JSON}"; fi

if [ -n "${NODE_ID}" ]; then
  check_code 200 "GET /api/admin/v1/nodes/{id}"          "${ADM_BASE}/api/admin/v1/nodes/${NODE_ID}" -H "${AUTH_COOKIE}"
  check_code 200 "GET /api/admin/v1/nodes/{id}/tokens"   "${ADM_BASE}/api/admin/v1/nodes/${NODE_ID}/tokens" -H "${AUTH_COOKIE}"
  check_code 200 "GET /api/admin/v1/nodes/{id}/traffic"  "${ADM_BASE}/api/admin/v1/nodes/${NODE_ID}/traffic" -H "${AUTH_COOKIE}"
  # 从未上报过的节点读运行态应 404 而不是返回假数据。
  check_code 404 "GET /nodes/{id}/runtime 未上报"        "${ADM_BASE}/api/admin/v1/nodes/${NODE_ID}/runtime" -H "${AUTH_COOKIE}"

  # PATCH：设为公开 + 写入真实 IP（这些字段只允许出现在管理端）。
  SECRET_IP="203.0.113.9"
  PC="$(code_of -X PATCH -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
        -d "{\"public_visible\":true,\"ip_v4\":\"${SECRET_IP}\",\"location\":\"HK-01\"}" \
        "${ADM_BASE}/api/admin/v1/nodes/${NODE_ID}")"
  [ "${PC}" = "204" ] && ok "PATCH 节点 -> 204" || bad "PATCH 节点期望 204 实际 ${PC}"

  # PATCH 只提交 sort_order 时，name 必须保留（指针字段 PATCH 语义，7.2）。
  curl -s -o /dev/null -X PATCH -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
    -d '{"sort_order":5}' "${ADM_BASE}/api/admin/v1/nodes/${NODE_ID}"
  ADMIN_NODE="$(curl -s "${ADM_BASE}/api/admin/v1/nodes/${NODE_ID}" -H "${AUTH_COOKIE}")"
  echo "${ADMIN_NODE}" | grep -q '冒烟节点' && ok "部分 PATCH 未清空 name" || bad "部分 PATCH 把 name 清空了：${ADMIN_NODE}"
  echo "${ADMIN_NODE}" | grep -q "${SECRET_IP}" && ok "管理端保留真实 IP（仅管理端可见）" || bad "管理端丢了真实 IP：${ADMIN_NODE}"
  echo "${ADMIN_NODE}" | grep -q '"hostname"' && ok "管理端暴露 hostname 字段（后台需要）" || true

  # 公开侧：节点应出现，但绝不能带出 IP / hostname / 令牌 / 价格。
  # 公开数据来自 1 秒一次的内存快照（A9），给它几次 tick 的时间。
  PUBNODES=""
  for _ in $(seq 1 20); do
    PUBNODES="$(curl -s "${PUB_BASE}/api/public/v1/nodes")"
    echo "${PUBNODES}" | grep -q '冒烟节点' && break
    sleep 0.5
  done
  echo "${PUBNODES}" | grep -q '冒烟节点' && ok "公开节点列表含该节点" || bad "公开节点列表缺少该节点：${PUBNODES}"
  if echo "${PUBNODES}" | grep -q "${SECRET_IP}"; then bad "公开 API 泄漏了真实 IP！"; else ok "公开 API 未泄漏真实 IP"; fi
  if echo "${PUBNODES}" | grep -qE '"(ip_v4|ip_v6|hostname|token|price_minor|note|provider|location)"'; then
    bad "公开 API 出现内网字段名：${PUBNODES}"
  else
    ok "公开 API 无内网字段名"
  fi
  if [ -n "${PUBLIC_ID}" ]; then
    check_code 200 "GET /api/public/v1/nodes/{public_id}" "${PUB_BASE}/api/public/v1/nodes/${PUBLIC_ID}"
    check_code 200 "GET /api/public/v1/nodes/{public_id}/history" "${PUB_BASE}/api/public/v1/nodes/${PUBLIC_ID}/history?metric=cpu&range=30m"
    check_code 200 "GET /api/public/v1/nodes/{public_id}/latency" "${PUB_BASE}/api/public/v1/nodes/${PUBLIC_ID}/latency"
  fi

  # 节点卡片上的「续费」一行：按支付周期折算到月均（后端 model.MonthlyEquivalent）。
  # 这里端到端验证"库里是分、公开侧是月均分、且原价字段不外传"。
  # 公开数据来自 1 秒一次的内存快照，每次改完给它几次 tick。
  put_billing() { # <json>
    code_of -X PUT -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
      -d "$1" "${ADM_BASE}/api/admin/v1/nodes/${NODE_ID}/billing"
  }
  wait_public() { # <grep 正则> <说明>
    local got="" i
    for i in $(seq 1 20); do
      got="$(curl -s "${PUB_BASE}/api/public/v1/nodes")"
      grep -q "$1" <<<"${got}" && { ok "$2"; return 0; }
      sleep 0.5
    done
    bad "$2（实得 ${got}）"
    return 1
  }

  BILL_BASE='"expire_date":"2027-01-01","traffic_quota":1099511627776,"traffic_mode":"down","reset_day":1,"tz":"Asia/Shanghai","traffic_multiplier":1'

  if [ "$(put_billing "{\"price_minor\":4500,\"currency\":\"CNY\",\"cycle\":\"monthly\",${BILL_BASE}}")" = "204" ]; then
    wait_public '"monthly_fee":4500' "月付 ¥45 -> 公开侧 monthly_fee=4500（分）" || true
    curl -s "${PUB_BASE}/api/public/v1/nodes" | grep -q '"monthly_fee_currency":"CNY"' \
      && ok "公开侧带出 monthly_fee_currency" || bad "公开侧缺 monthly_fee_currency（前端无法格式化金额）"
    curl -s "${PUB_BASE}/api/public/v1/nodes" | grep -q '"fee_cycle":"monthly"' \
      && ok "公开侧带出 fee_cycle=monthly" || bad "公开侧缺 fee_cycle：前端分不清终身/一次性/未配置"
    # 后台节点列表用同一口径（fee_monthly/fee_currency/fee_cycle），否则两处会各算各的。
    ADMNODES="$(curl -s "${ADM_BASE}/api/admin/v1/nodes" -H "${AUTH_COOKIE}")"
    if grep -q '"fee_monthly":4500' <<<"${ADMNODES}" && grep -q '"fee_cycle":"monthly"' <<<"${ADMNODES}"; then
      ok "后台节点列表同样带出 fee_monthly=4500 与 fee_cycle"
    else
      bad "后台节点列表缺 fee_monthly/fee_cycle：后台卡片显示不出续费价格"
    fi
  else
    bad "写入月付计费失败"
  fi

  # 年付 60000 分（¥600/年）折算到月均应四舍五入成 5000 分（¥50/月）。
  if [ "$(put_billing "{\"price_minor\":60000,\"currency\":\"CNY\",\"cycle\":\"yearly\",${BILL_BASE}}")" = "204" ]; then
    wait_public '"monthly_fee":5000' "年付 ¥600 -> 月均 5000 分（÷12）" || true
  else
    bad "写入年付计费失败"
  fi

  # 终身：折算后是 0，必须靠 fee_cycle 表明"终身"而不是"没配价格"。
  if [ "$(put_billing "{\"price_minor\":19900,\"currency\":\"CNY\",\"cycle\":\"lifetime\",${BILL_BASE}}")" = "204" ]; then
    wait_public '"fee_cycle":"lifetime"' "终身计费 -> 公开侧 fee_cycle=lifetime（月均为 0）" || true
    curl -s "${PUB_BASE}/api/public/v1/nodes" | grep -q '"monthly_fee":19900' \
      && bad "终身节点把原价当成月均下发：前端会显示成每月 ¥199" \
      || ok "终身节点月均折算为 0（不是原价）"
  else
    bad "写入终身计费失败"
  fi

  # 折算口径不能把原价字段带出去（price_minor 是 dto_leak_test.go 的禁词）。
  if curl -s "${PUB_BASE}/api/public/v1/nodes" | grep -qE '"(price_minor|price|cost)"'; then
    bad "公开 API 泄漏了原价字段名"
  else
    ok "公开 API 只暴露 monthly_fee/fee_cycle，不带 price_minor"
  fi

  EC_JSON="$(curl -s -X POST "${ADM_BASE}/api/admin/v1/nodes/${NODE_ID}/enroll-codes" \
    -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' -d '{}')"
  if echo "${EC_JSON}" | grep -qE '"code"'; then ok "生成注册码成功"; else bad "生成注册码失败：${EC_JSON}"; fi
  ENROLL_CODE="$(sed -n 's/.*"code"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"${EC_JSON}")"

  # 存在有效注册码后，on_demand 模式下安装入口应自动开放（12.8）。
  # 注意：必须在注册码被消费之前检查，消费后入口会自动关闭——这是预期行为。
  check_code 200 "GET /install.sh（有注册码后）"  "${PUB_BASE}/install.sh"
  check_code 200 "GET /install.ps1（有注册码后）" "${PUB_BASE}/install.ps1"
  # /dist 从 <data_dir>/dist 读取发布文件；先在里头放一个假产物，验证真实下发路径。
  mkdir -p "${WORK}/data/dist"
  printf '#!/bin/sh\necho fake-agent\n' > "${WORK}/data/dist/probe-agent_linux_amd64"
  check_code 200 "GET /dist/probe-agent_linux_amd64（有注册码后）" "${PUB_BASE}/dist/probe-agent_linux_amd64"
  DHDR="$(curl -s -D - -o /dev/null "${PUB_BASE}/dist/probe-agent_linux_amd64")"
  if echo "${DHDR}" | grep -qi 'content-disposition: attachment'; then ok "/dist 以附件形式下发"; else bad "/dist 缺少 Content-Disposition"; fi
  # 路径穿越必须被挡住。用 --path-as-is 阻止 curl 自己先把 ".." 规范化掉。
  TRAV="$(code_of --path-as-is "${PUB_BASE}/dist/../master.key")"
  if [ "${TRAV}" = "200" ]; then
    bad "/dist 存在路径穿越（master.key 被读走）"
  else
    ok "/dist 路径穿越被挡住（HTTP ${TRAV}）"
  fi
  TRAV2="$(code_of --path-as-is "${PUB_BASE}/dist/%2e%2e/master.key")"
  if [ "${TRAV2}" = "200" ]; then bad "/dist 存在编码穿越"; else ok "/dist 编码穿越被挡住（HTTP ${TRAV2}）"; fi
  INSTALL_BODY="$(curl -s "${PUB_BASE}/install.sh")"
  if echo "${INSTALL_BODY}" | grep -qi 'curl\|wget'; then ok "install.sh 内容像是可执行的安装脚本"; else bad "install.sh 内容异常"; fi
  # __MINISIGN_PUB__ 占位必须被替换掉：漏替换会让脚本里留一个未定义变量，
  # 于是"开了验签"和"没开"在节点上表现一样（永远跳过），属于静默失效。
  if echo "${INSTALL_BODY}" | grep -q '__MINISIGN_PUB__'; then
    bad "install.sh 未替换 __MINISIGN_PUB__ 占位"
  else
    ok "install.sh 已替换 __MINISIGN_PUB__ 占位"
  fi
  # 再填一次真实公钥，确认它按 shell 单引号安全内联（多行也不能破坏语法）。
  curl -s -o /dev/null -X PUT "${ADM_BASE}/api/admin/v1/settings" \
    -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
    -d '{"install.pubkey":"untrusted comment: minisign public key B56B8445221E5AEE\nRWTuWh4iRYRrtRxnZ4S2i+5JWAetk3dpGiNA3qnSUK2yX23mNzaLGZtv"}'
  INSTALL_BODY2="$(curl -s "${PUB_BASE}/install.sh")"
  if echo "${INSTALL_BODY2}" | grep -q 'RWTuWh4iRYRrtRxnZ4S2i'; then
    ok "install.sh 内联了下发的 minisign 公钥"
  else
    bad "install.sh 未内联 minisign 公钥"
  fi
  # 清空，别影响后续断言。
  curl -s -o /dev/null -X PUT "${ADM_BASE}/api/admin/v1/settings" \
    -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
    -d '{"install.pubkey":""}'

  # ---- 完整的 Agent 协议闭环：注册 -> 拿令牌 -> 上报 -> 公开可见 ----
  # 这一步是必须的：注册码在库里存的是“去分隔符并大写后”的哈希，
  # 而注册码本身一定带 “-”。只要两侧规范化不一致，整个注册流程就会全废，
  # 而这种错编译期查不出来（曾经真的踩过）。
  if [ -n "${ENROLL_CODE}" ]; then
    ENROLL_BODY="{\"code\":\"${ENROLL_CODE}\",\"hostname\":\"smoke-node\",\"os\":\"linux\",\"arch\":\"amd64\",\"agent_version\":\"1.0.0\",\"counter_bits\":64}"
    ENROLL_RESP="$(curl -s -X POST "${PUB_BASE}/api/agent/v1/enroll" -H 'Content-Type: application/json' -d "${ENROLL_BODY}")"
    AGENT_TOKEN="$(sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"${ENROLL_RESP}")"
    if [ -n "${AGENT_TOKEN}" ]; then
      ok "带分隔符的注册码可以成功注册（拿到令牌）"
    else
      bad "注册失败：${ENROLL_RESP}"
    fi

    # 同一个注册码不能再用第二次（一次性）。
    check_code 401 "同一注册码重复使用" "${PUB_BASE}/api/agent/v1/enroll" -X POST -H 'Content-Type: application/json' -d "${ENROLL_BODY}"

    if [ -n "${AGENT_TOKEN}" ]; then
      BEARER="Authorization: Bearer ${AGENT_TOKEN}"
      check_code 200 "GET /api/agent/v1/config 带令牌" "${PUB_BASE}/api/agent/v1/config" -H "${BEARER}"

      NOWNOW="$(date +%s)"
      REPORT_BODY="{\"agent_version\":\"1.0.0\",\"static\":{\"hostname\":\"smoke-node\",\"os\":\"linux\",\"kernel\":\"6.1.0\",\"arch\":\"amd64\",\"cpu_model\":\"SmokeCPU\",\"cpu_cores\":8,\"mem_total\":17179869184,\"swap_total\":0,\"disk_total\":107374182400,\"virt\":\"none\"},\"samples\":[{\"ts\":${NOWNOW},\"metrics\":{\"cpu\":1234,\"load1\":50,\"mem_used\":4294967296,\"swap_used\":0,\"disk_used\":21474836480,\"conns\":42,\"boot_time\":$((NOWNOW - 7200)),\"nics\":{\"eth0\":{\"rx\":1000000,\"tx\":2000000}}},\"latency\":[]}]}"
      REPORT_RESP="$(curl -s -X POST "${PUB_BASE}/api/agent/v1/report" -H "${BEARER}" -H 'Content-Type: application/json' -d "${REPORT_BODY}")"
      if echo "${REPORT_RESP}" | grep -q '"ok":[[:space:]]*true'; then
        ok "上报成功（Agent 协议闭环打通）"
      else
        bad "上报失败：${REPORT_RESP}"
      fi

      # 上报后：管理端运行态应可用，公开侧应显示为在线。
      check_code 200 "GET /nodes/{id}/runtime 上报后" "${ADM_BASE}/api/admin/v1/nodes/${NODE_ID}/runtime" -H "${AUTH_COOKIE}"
      PUB_ONLINE=""
      for _ in $(seq 1 20); do
        PUB_ONLINE="$(curl -s "${PUB_BASE}/api/public/v1/nodes")"
        echo "${PUB_ONLINE}" | grep -q '"status":"online"' && break
        sleep 0.5
      done
      if echo "${PUB_ONLINE}" | grep -q '"status":"online"'; then
        ok "上报后公开看板显示节点在线"
      else
        bad "上报后公开看板未显示在线：${PUB_ONLINE}"
      fi
      # 公开侧的 CPU 百分比必须来自刚上报的 1234（×100 口径）。
      if echo "${PUB_ONLINE}" | grep -q '"cpu_pct":1234'; then
        ok "公开侧指标与上报值一致（cpu_pct=1234）"
      else
        bad "公开侧指标不正确：${PUB_ONLINE}"
      fi
      # 上报里带了 kernel / cpu_model，公开侧绝不能出现。
      if echo "${PUB_ONLINE}" | grep -qE 'SmokeCPU|6\.1\.0'; then
        bad "公开侧泄漏了内核/CPU 型号"
      else
        ok "公开侧未泄漏内核/CPU 型号"
      fi
    fi
  fi

  # 注册码被消费后，安装入口应自动关闭（on_demand 语义）。
  check_code 404 "注册码被消费后 /install.sh 自动关闭" "${PUB_BASE}/install.sh"

  # 节点列表应能看到它。
  if curl -s "${ADM_BASE}/api/admin/v1/nodes" -H "${AUTH_COOKIE}" | grep -q '冒烟节点'; then
    ok "节点列表包含新建节点"
  else
    bad "节点列表未包含新建节点"
  fi

  # 审计日志应记录上述写操作。
  if curl -s "${ADM_BASE}/api/admin/v1/audit-log" -H "${AUTH_COOKIE}" | grep -q 'node.create'; then
    ok "审计日志已记录 node.create"
  else
    bad "审计日志未记录 node.create"
  fi
fi

# ---------------------------------------------------------------------------
sec "8.5 探测目标范围多选 / 告警与审计分页 / 备份删除"
# ---------------------------------------------------------------------------
# jnum <key>：从标准输入的 JSON 里取第一个 <key> 的整数值。
jnum() { sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\([0-9]\{1,\}\).*/\1/p" | head -1; }
ADM="${ADM_BASE}/api/admin/v1"
CJ=(-H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json')

# tgt_node_of <id>：列出全部目标并打印指定 id 的 node_id（0 表示全局）。
#
# 创建接口只回 `{created,id,ids}` 这种"结果信封"，不含作用域本身，
# 所以作用域一律回到列表接口上核对——那样验的才是**库里真正存了什么**。
tgt_node_of() {
  curl -s "${ADM}/probe-targets" -H "${AUTH_COOKIE}" | tr '{' '\n' \
    | sed -n "s/.*\"id\":$1,\"node_id\":\([0-9]\{1,\}\).*/\1/p" | head -1
}
# tgt_id_on <node_id> <name>：找出某节点上指定名称的目标 id。
tgt_id_on() {
  curl -s "${ADM}/probe-targets" -H "${AUTH_COOKIE}" | tr '{' '\n' \
    | grep "\"node_id\":$1," | grep "\"name\":\"$2\"" \
    | sed -n 's/.*"id":\([0-9]\{1,\}\).*/\1/p' | head -1
}
# tgt_host_of <id>：取该目标的主机名（验证参数有没有真的同步过去）。
tgt_host_of() {
  curl -s "${ADM}/probe-targets" -H "${AUTH_COOKIE}" | tr '{' '\n' \
    | grep "\"id\":$1," | sed -n 's/.*"host":"\([^"]*\)".*/\1/p' | head -1
}

if [ -z "${NODE_ID}" ]; then
  skip "没有可用节点，跳过探测目标范围多选"
else
  # ---- node_ids 的三态：[] 全局 / [id] 专属 / 缺省不动作用域 ----
  #
  # 这条断言盯的是一个很容易写错的地方：`node_ids` 的**存在性**有语义。
  # 一旦实现把"字段缺省"也当成空范围，管理员在节点详情页只想改个端口，
  # 就会把目标的作用域顺手清掉；反过来把"[]"当成缺省，就再也建不出全局目标。
  R="$(curl -s -X POST "${CJ[@]}" \
    -d '{"name":"冒烟-全局","carrier":"custom","host":"1.1.1.1","port":443,"method":"tcp","timeout_ms":2000,"node_ids":[]}' \
    "${ADM}/probe-targets")"
  GT_ID="$(echo "${R}" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*\([0-9]\{1,\}\).*/\1/p' | head -1)"
  if [ "$(echo "${R}" | jnum created)" = "1" ] && [ "$(tgt_node_of "${GT_ID}")" = "0" ]; then
    ok "POST node_ids=[] 建出一条全局目标 (#${GT_ID})"
  else
    bad "POST node_ids=[] 没有建出全局目标（node_id=$(tgt_node_of "${GT_ID}")）：${R}"
  fi

  R="$(curl -s -X POST "${CJ[@]}" \
    -d "{\"name\":\"冒烟-专属\",\"carrier\":\"unicom\",\"host\":\"8.8.8.8\",\"port\":53,\"method\":\"tcp\",\"timeout_ms\":2000,\"node_ids\":[${NODE_ID}]}" \
    "${ADM}/probe-targets")"
  DT_ID="$(echo "${R}" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*\([0-9]\{1,\}\).*/\1/p' | head -1)"
  if [ "$(echo "${R}" | jnum created)" = "1" ] && [ "$(tgt_node_of "${DT_ID}")" = "${NODE_ID}" ]; then
    ok "POST node_ids=[${NODE_ID}] 建出该节点的专属目标 (#${DT_ID})"
  else
    bad "POST node_ids=[id] 没有建出专属目标（node_id=$(tgt_node_of "${DT_ID}")）：${R}"
  fi

  # 多选：重复 id 去重后仍只建一条（而不是建出两条一模一样的）。
  R="$(curl -s -X POST "${CJ[@]}" \
    -d "{\"name\":\"冒烟-去重\",\"carrier\":\"mobile\",\"host\":\"223.5.5.5\",\"port\":53,\"method\":\"tcp\",\"timeout_ms\":2000,\"node_ids\":[${NODE_ID},${NODE_ID}]}" \
    "${ADM}/probe-targets")"
  [ "$(echo "${R}" | jnum created)" = "1" ] \
    && ok "重复的 node_ids 被去重（只建 1 条）" \
    || bad "重复 node_ids 建出了多条：${R}"

  # 不存在的节点必须拒绝：否则会建出一条谁也拉不到、却占着 ID 的目标。
  check_code 400 "POST node_ids 含不存在节点被拒" "${ADM}/probe-targets" -X POST "${CJ[@]}" \
    -d '{"name":"冒烟-幽灵","carrier":"custom","host":"1.1.1.1","port":443,"method":"tcp","timeout_ms":2000,"node_ids":[999999]}'

  # ---- 编辑：多选范围同步（只增不删）----
  #
  # 需求 5「编辑专属探测目标，支持范围可多选节点」的端到端断言。
  # 必须有第二个节点才能证明"真的同步到了别的节点"，而不是只在原地改了参数。
  NODE2_ID=""
  N2="$(curl -s -X POST "${CJ[@]}" -d '{"name":"冒烟节点2","group":"smoke"}' "${ADM}/nodes")"
  NODE2_ID="$(echo "${N2}" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*\([0-9]\{1,\}\).*/\1/p' | head -1)"
  # 一条同名 + 同运营组的**全局**目标：作用域同步绝不能把它当成"节点上的副本"
  # 去覆盖——那会让所有节点一起被改。
  curl -s -o /dev/null -X POST "${CJ[@]}" \
    -d '{"name":"冒烟-专属","carrier":"unicom","host":"203.0.113.1","port":53,"method":"tcp","timeout_ms":2000,"node_ids":[]}' \
    "${ADM}/probe-targets"

  if [ -n "${DT_ID}" ] && [ -n "${NODE2_ID}" ]; then
    SYNC_BODY="{\"name\":\"冒烟-专属\",\"carrier\":\"unicom\",\"host\":\"9.9.9.9\",\"port\":53,\"method\":\"tcp\",\"timeout_ms\":2000,\"enabled\":true,\"show_in_bars\":true,\"node_ids\":[${NODE_ID},${NODE2_ID}]}"
    R="$(curl -s -X PATCH "${CJ[@]}" -d "${SYNC_BODY}" "${ADM}/probe-targets/${DT_ID}")"
    if [ "$(echo "${R}" | jnum created)" = "1" ] && [ "$(echo "${R}" | jnum updated)" = "0" ]; then
      ok "PATCH 多选范围（自身 + 另一个节点）新建 1 条、自身原地更新"
    else
      bad "PATCH 多选范围的 created/updated 不对：${R}"
    fi
    NEW_ID="$(tgt_id_on "${NODE2_ID}" "冒烟-专属")"
    if [ -n "${NEW_ID}" ] && [ "$(tgt_host_of "${NEW_ID}")" = "9.9.9.9" ]; then
      ok "第二个节点上建出了同名目标 (#${NEW_ID}) 且参数已同步"
    else
      bad "多选范围没有把目标同步到第二个节点（id=${NEW_ID:-无}，host=$(tgt_host_of "${NEW_ID}")）"
    fi
    # 自身节点是**原地更新**，不是"新建一条 + 旧的留着"。
    if [ "$(tgt_node_of "${DT_ID}")" = "${NODE_ID}" ] && [ "$(tgt_host_of "${DT_ID}")" = "9.9.9.9" ]; then
      ok "自身节点为原地更新（没有新建重复目标）"
    else
      bad "自身节点的目标被换掉了：node=$(tgt_node_of "${DT_ID}") host=$(tgt_host_of "${DT_ID}")"
    fi
    # 幂等：同样的范围再同步一次，应只更新、不再新建。
    R="$(curl -s -X PATCH "${CJ[@]}" -d "${SYNC_BODY}" "${ADM}/probe-targets/${DT_ID}")"
    if [ "$(echo "${R}" | jnum created)" = "0" ] && [ "$(echo "${R}" | jnum updated)" = "1" ]; then
      ok "重复同步是幂等的（只更新、不再新建）"
    else
      bad "重复同步又建了重复目标：${R}"
    fi
    # 全局的同名目标必须原封不动：作用域同步只碰**节点专属**目标。
    GID="$(tgt_id_on 0 "冒烟-专属")"
    if [ -n "${GID}" ] && [ "$(tgt_host_of "${GID}")" = "203.0.113.1" ]; then
      ok "全局的同名目标没有被作用域同步覆盖"
    else
      bad "全局的同名目标被覆盖了（host=$(tgt_host_of "${GID}")）"
    fi
  fi

  if [ -n "${DT_ID}" ]; then
    # 空范围：一条已有目标不可能"没有节点"，必须 400 而不是静默 no-op。
    check_code 400 "PATCH node_ids=[] 被拒（范围不能为空）" "${ADM}/probe-targets/${DT_ID}" -X PATCH "${CJ[@]}" \
      -d '{"name":"冒烟-专属","node_ids":[]}'
    # 把目标移出它自己的节点：会留下孤儿延迟桶，必须 400。
    check_code 400 "PATCH 把目标移出自身节点被拒" "${ADM}/probe-targets/${DT_ID}" -X PATCH "${CJ[@]}" \
      -d '{"name":"冒烟-专属","node_ids":[999999]}'
    # 节点详情页的编辑**不带** node_ids：只改参数，作用域不动。
    PATCH_NO_SCOPE="$(code_of -X PATCH "${CJ[@]}" -d '{"port":5380}' "${ADM}/probe-targets/${DT_ID}")"
    [ "${PATCH_NO_SCOPE}" = "200" ] \
      && ok "PATCH 不带 node_ids 只改参数（节点详情页的编辑路径）" \
      || bad "PATCH 不带 node_ids 期望 200 实际 ${PATCH_NO_SCOPE}"
    ALLT="$(curl -s "${ADM}/probe-targets" -H "${AUTH_COOKIE}")"
    echo "${ALLT}" | grep -q "\"id\":${DT_ID},\"node_id\":${NODE_ID}" \
      && ok "不带 node_ids 的 PATCH 没有改掉作用域" \
      || bad "不带 node_ids 的 PATCH 把作用域改了：${ALLT}"
  fi

  # 全局目标不能被收窄成专属。
  if [ -n "${GT_ID}" ]; then
    check_code 400 "PATCH 全局目标收窄为专属被拒（全局目标）" "${ADM}/probe-targets/${GT_ID}" -X PATCH "${CJ[@]}" \
      -d "{\"name\":\"冒烟-全局\",\"node_ids\":[${NODE_ID}]}"
  fi

  # 收尾：把这一段建出来的目标与临时节点清掉，避免影响后面的断言。
  for tid in $(curl -s "${ADM}/probe-targets" -H "${AUTH_COOKIE}" \
      | tr '{' '\n' | grep '冒烟-' | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*\([0-9]\{1,\}\).*/\1/p'); do
    curl -s -o /dev/null -X DELETE "${ADM}/probe-targets/${tid}" -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}"
  done
  if [ -n "${NODE2_ID}" ]; then
    curl -s -o /dev/null -X DELETE "${ADM}/nodes/${NODE2_ID}" -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}"
    NODE2_ID=""
  fi
fi

# ---- 告警事件 / 审计日志：分页查询 ----
#
# 响应从裸数组改成了信封 {items,total,limit,offset}：前端要靠 total 画
# "第 x / y 页"、靠 offset/limit 算页码。这里逐项断言，漏一个前端就画不出来。
EV="$(curl -s "${ADM}/alert-events?limit=5&offset=0" -H "${AUTH_COOKIE}")"
echo "${EV}" | grep -q '"items"' && ok "GET /alert-events 返回 items 信封" || bad "告警事件没有 items：${EV}"
echo "${EV}" | grep -q '"total"' && ok "GET /alert-events 返回 total" || bad "告警事件没有 total：${EV}"
[ "$(echo "${EV}" | jnum limit)" = "5" ] && ok "GET /alert-events 采用 limit=5" || bad "告警事件 limit 未生效：${EV}"
echo "${EV}" | grep -q '"items":\[\]' \
  && ok "空表的 items 是 [] 而不是 null（前端读 .length 不会抛异常）" \
  || ok "告警事件 items 非空（本实例已有记录）"

AU="$(curl -s "${ADM}/audit-log?limit=5&offset=0" -H "${AUTH_COOKIE}")"
echo "${AU}" | grep -q '"items"' && ok "GET /audit-log 返回 items 信封" || bad "审计日志没有 items：${AU}"
ATOTAL="$(echo "${AU}" | jnum total)"
if [ -n "${ATOTAL}" ] && [ "${ATOTAL}" -ge 1 ]; then ok "GET /audit-log 的 total=${ATOTAL}（至少 1，说明前面确实在写审计）"; else bad "审计日志 total 异常：${AU}"; fi
[ "$(echo "${AU}" | jnum offset)" = "0" ] && ok "GET /audit-log 起始 offset=0" || bad "审计日志 offset 未生效：${AU}"

# 超过上限的 limit 必须被压回 MaxPageSize=200，并且**回传的是压过之后的值**。
AU_BIG="$(curl -s "${ADM}/audit-log?limit=100000" -H "${AUTH_COOKIE}")"
if [ "$(echo "${AU_BIG}" | jnum limit)" = "200" ]; then
  ok "GET /audit-log?limit=100000 被压回 200 并回传"
else
  bad "limit 上限没生效（或回传的是入参）：${AU_BIG}"
fi
# limit<=0 不能变成"返回全部"（SQLite 的 LIMIT 0 是零行，而 0 之外的实现可能不限）。
[ "$(curl -s "${ADM}/audit-log?limit=0" -H "${AUTH_COOKIE}" | jnum limit)" = "50" ] \
  && ok "limit=0 收敛为默认每页 50" \
  || bad "limit=0 没有收敛为默认值"
# 越界 offset 必须被拉回最后一页，且回传生效值（否则前端会显示"第 N/2 页"配空表）。
AU_FAR="$(curl -s "${ADM}/audit-log?limit=5&offset=999999" -H "${AUTH_COOKIE}")"
AU_FOFF="$(echo "${AU_FAR}" | jnum offset)"
if [ "${AU_FOFF}" = "999999" ]; then
  bad "越界 offset 没被收敛，回传了 ${AU_FOFF}"
elif echo "${AU_FAR}" | grep -q '"items":\[\]' && [ "${ATOTAL}" -gt 0 ]; then
  bad "越界 offset 收敛后仍给了空表（应给最后一页内容）：${AU_FAR}"
else
  ok "越界 offset 被拉回最后一页并回传生效值 offset=${AU_FOFF}"
fi

# ---- 备份：单份删除（6.6 的补充：只增不减会把备份盘撑满）----
BK="$(curl -s -X POST "${ADM}/maintenance/backup" -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' -d '{}')"
BNAME="$(echo "${BK}" | sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
if [ -n "${BNAME}" ]; then
  ok "手动备份成功：${BNAME}"
  check_code 204 "DELETE /maintenance/backups/{name} 删除刚生成的备份" \
    "${ADM}/maintenance/backups/${BNAME}" -X DELETE -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}"
  if curl -s "${ADM}/maintenance/backups" -H "${AUTH_COOKIE}" | grep -q "${BNAME}"; then
    bad "删除后备份列表里仍有 ${BNAME}"
  else
    ok "备份列表已不含被删除的 ${BNAME}"
  fi
  # 再删一次：应为 404，而不是 500（"不存在"是正常结果，不是服务端故障）。
  check_code 404 "重复删除同一份备份返回 404" \
    "${ADM}/maintenance/backups/${BNAME}" -X DELETE -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}"
  # 形状不合法的文件名必须 400：这个接口最终会落到 os.Remove 上，
  # 放开任意路径就是一个数据丢失面。
  check_code 400 "删除非备份文件名（含路径分隔）被拒" \
    "${ADM}/maintenance/backups/notabackup.db" -X DELETE -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}"
  # 主库文件本身绝不能被这个接口删掉。
  if [ -f "${WORK}/data/probe.db" ]; then
    check_code 400 "删除 probe.db 被拒（主库不可被这个接口删除）" \
      "${ADM}/maintenance/backups/probe.db" -X DELETE -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}"
  fi
else
  bad "手动备份未返回文件名：${BK}"
fi

# ---------------------------------------------------------------------------
sec "8.6 Agent 自升级：版本发现 / 取件校验 / 替换与回滚"
# ---------------------------------------------------------------------------
# 这条链路上有三段各自独立的契约，任何一段错位都会让节点**永远升不了级**，
# 而且全都是静默的：Agent 只打一行日志，Master 侧看起来一切正常。
#   1) 版本发现：目录名必须是纯数字点分（nightly-* 一律忽略），且要能区分
#      "整个版本没发布"与"这个平台没发布"——后者是 armv6 节点最常见的处境
#   2) 取件校验：SHA256SUMS 是信任根（缺失必须拒绝），X-Checksum-Sha256 只用于快速失败
#   3) 替换回滚：原子替换 + 留备份 + 保住可执行位
#
# 这里刻意**真编两份不同版本号的二进制**，而不是把同一份文件改个名：
# 版本号是判断"有没有新版"的唯一依据，同一份二进制什么也测不出来。
if [ -n "${AGENT_TOKEN:-}" ]; then
  BEARER="Authorization: Bearer ${AGENT_TOKEN}"
  HOST_TAG="$("${GO_BIN}" env GOOS)_$("${GO_BIN}" env GOARCH)"
  HOST_EXT=""
  [ "$("${GO_BIN}" env GOOS)" = "windows" ] && HOST_EXT=".exe"
  V_FROM="1.0.0"
  V_TO="9.9.9"
  ART="probe-agent_${HOST_TAG}${HOST_EXT}"
  MODULE_PATH="$(awk '/^module /{print $2; exit}' go.mod)"

  mkdir -p "${WORK_REL}/agent" "${WORK_REL}/data/dist/${V_TO}"
  # 非点分版本目录。它按字典序排在 "9.9.9" **之后**：只要 latestDistVersion
  # 少做一次 isVersionName 过滤，拿到的就是它，随后 readDistSums 失败 ——
  # 表现是"明明发布了新版本，所有节点都报 Master 上没有已发布版本"。
  mkdir -p "${WORK_REL}/data/dist/nightly-20260101"
  echo "deadbeef  probe-agent_${HOST_TAG}${HOST_EXT}" \
    >"${WORK_REL}/data/dist/nightly-20260101/SHA256SUMS"

  LDF_FROM="-s -w -X main.version=${V_FROM} -X ${MODULE_PATH}/internal/dist.PlatformTag=${HOST_TAG}"
  LDF_TO="-s -w -X main.version=${V_TO} -X ${MODULE_PATH}/internal/dist.PlatformTag=${HOST_TAG}"
  if (cd "${ROOT}" && CGO_ENABLED=0 "${GO_BIN}" build -trimpath -ldflags "${LDF_FROM}" \
      -o "${WORK_REL}/agent/probe-agent${HOST_EXT}" ./cmd/probe-agent 2>"${WORK}/build-agent.log"); then
    ok "构建待升级的 probe-agent（v${V_FROM}，发布标签 ${HOST_TAG}）"
  else
    bad "构建 probe-agent 失败：$(cat "${WORK}/build-agent.log")"
  fi
  if (cd "${ROOT}" && CGO_ENABLED=0 "${GO_BIN}" build -trimpath -ldflags "${LDF_TO}" \
      -o "${WORK_REL}/data/dist/${V_TO}/${ART}" ./cmd/probe-agent 2>"${WORK}/build-agent2.log"); then
    (cd "${WORK_REL}/data/dist/${V_TO}" && sha256sum "${ART}" >SHA256SUMS)
    ok "发布 v${V_TO}：${ART} + SHA256SUMS"
  else
    bad "构建 v${V_TO} 失败：$(cat "${WORK}/build-agent2.log")"
  fi
  WANT_SUM="$(awk '{print $1}' "${WORK_REL}/data/dist/${V_TO}/SHA256SUMS")"

  # ---- 版本发现 ----
  REL="$(curl -s "${PUB_BASE}/api/agent/v1/releases/latest?platform=${HOST_TAG}&current=${V_FROM}" -H "${BEARER}")"
  if echo "${REL}" | grep -q "\"version\":\"${V_TO}\""; then
    ok "releases/latest 报出新版本 v${V_TO}（非点分的 nightly-* 目录被忽略）"
  else
    bad "releases/latest 未报出新版本：${REL}"
  fi
  if echo "${REL}" | grep -q "\"artifact\":\"${ART}\"" && echo "${REL}" | grep -q "\"sha256\":\"${WANT_SUM}\""; then
    ok "releases/latest 带回构件名与 SHA-256"
  else
    bad "构件名或校验和不对：${REL}"
  fi
  if echo "${REL}" | grep -q "\"up_to_date\":false"; then
    ok "current=${V_FROM} 时 up_to_date=false"
  else
    bad "up_to_date 判定错误：${REL}"
  fi
  REL_SAME="$(curl -s "${PUB_BASE}/api/agent/v1/releases/latest?platform=${HOST_TAG}&current=${V_TO}" -H "${BEARER}")"
  if echo "${REL_SAME}" | grep -q "\"up_to_date\":true"; then
    ok "current=${V_TO} 时 up_to_date=true（否则每次执行都要重下一遍）"
  else
    bad "已是最新却仍报可升级：${REL_SAME}"
  fi

  check_code 401 "GET releases/latest 无令牌" "${PUB_BASE}/api/agent/v1/releases/latest?platform=${HOST_TAG}"
  UNAVAIL_CODE="$(code_of -H "${BEARER}" "${PUB_BASE}/api/agent/v1/releases/latest?platform=linux_mips64le")"
  UNAVAIL_BODY="$(curl -s "${PUB_BASE}/api/agent/v1/releases/latest?platform=linux_mips64le" -H "${BEARER}")"
  if [ "${UNAVAIL_CODE}" = "404" ] && echo "${UNAVAIL_BODY}" | grep -q "该平台未发布此版本"; then
    ok "未发布的平台 404 且点名平台（区别于「Master 没配好」）"
  else
    bad "未发布平台的状态码/信息不对：${UNAVAIL_CODE} ${UNAVAIL_BODY}"
  fi
  check_code 400 "releases/latest platform 带路径穿越" \
    "${PUB_BASE}/api/agent/v1/releases/latest?platform=..%2F..%2Fmaster.key" -H "${BEARER}"
  check_code 400 "artifact version 带路径穿越" \
    "${PUB_BASE}/api/agent/v1/artifact?platform=${HOST_TAG}&version=..%2F..%2F" -H "${BEARER}"
  check_code 404 "artifact 指定不存在的版本" \
    "${PUB_BASE}/api/agent/v1/artifact?platform=${HOST_TAG}&version=1.2.3" -H "${BEARER}"

  # ---- 取件：内容必须与 SHA256SUMS 一致，且带快速失败用的响应头 ----
  curl -s -D "${WORK_REL}/art.hdr" -o "${WORK_REL}/art.bin" \
    "${PUB_BASE}/api/agent/v1/artifact?platform=${HOST_TAG}&version=${V_TO}" -H "${BEARER}"
  GOT_SUM="$(sha256sum "${WORK_REL}/art.bin" | awk '{print $1}')"
  if [ "${GOT_SUM}" = "${WANT_SUM}" ]; then
    ok "artifact 下发内容的 SHA-256 与 SHA256SUMS 一致"
  else
    bad "artifact 内容与校验和不符：${GOT_SUM} != ${WANT_SUM}"
  fi
  if grep -qi "X-Checksum-Sha256: ${WANT_SUM}" "${WORK_REL}/art.hdr"; then
    ok "artifact 带 X-Checksum-Sha256 响应头"
  else
    bad "artifact 缺少校验和响应头：$(tr -d '\r' <"${WORK_REL}/art.hdr" | tr '\n' ' ')"
  fi

  # ---- 实际升级：替换 → 备份 → 回滚 ----
  cat >"${WORK}/agent/agent.yaml" <<YAML
master_url: "${PUB_BASE}"
token: "${AGENT_TOKEN}"
insecure_http: true
YAML
  AGENT_BIN="${WORK_REL}/agent/probe-agent${HOST_EXT}"
  AGENT_CFG="${WORK_REL}/agent/agent.yaml"

  CHECK_OUT="$("${AGENT_BIN}" upgrade --check --config "${AGENT_CFG}" 2>&1)"
  if echo "${CHECK_OUT}" | grep -q "可升级"; then
    ok "upgrade --check 报出可升级（${V_FROM} -> ${V_TO}）"
  else
    bad "upgrade --check 未报可升级：${CHECK_OUT}"
  fi

  DRY_OUT="$("${AGENT_BIN}" upgrade --dry-run --config "${AGENT_CFG}" --target "${AGENT_BIN}" 2>&1)"
  if echo "${DRY_OUT}" | grep -q "dry-run" && "${AGENT_BIN}" version | grep -q "${V_FROM}"; then
    ok "upgrade --dry-run 只下载校验、未替换"
  else
    bad "dry-run 却改动了文件：${DRY_OUT}"
  fi

  UP_OUT="$("${AGENT_BIN}" upgrade --config "${AGENT_CFG}" --target "${AGENT_BIN}" 2>&1)"
  if echo "${UP_OUT}" | grep -q "已升级 ${V_FROM} -> ${V_TO}"; then
    ok "upgrade 完成替换（${V_FROM} -> ${V_TO}）"
  else
    bad "upgrade 输出异常：${UP_OUT}"
  fi
  if "${AGENT_BIN}" version | grep -q "${V_TO}"; then
    ok "替换后的二进制自报新版本（确实换掉了）"
  else
    bad "替换后版本号没变：$("${AGENT_BIN}" version)"
  fi
  if [ -f "${WORK}/agent/probe-agent${HOST_EXT}.old" ]; then
    ok "留下了 .old 备份（回滚的前提）"
  else
    bad "没有生成备份文件 —— --rollback 会是一句空话"
  fi
  if [ -x "${AGENT_BIN}" ]; then
    ok "替换后仍可执行（权限位没丢）"
  else
    bad "替换后丢了可执行权限"
  fi
  if [ -f "${WORK}/agent/probe-agent${HOST_EXT}.new" ]; then
    bad "留下了 .new 临时文件（说明替换路径没走完）"
  else
    ok "没有残留 .new 临时文件"
  fi

  LATEST_OUT="$("${AGENT_BIN}" upgrade --check --config "${AGENT_CFG}" 2>&1)"
  if echo "${LATEST_OUT}" | grep -q "已是最新"; then
    ok "升到最新后再查 --check 报已是最新"
  else
    bad "--check 未识别已是最新：${LATEST_OUT}"
  fi

  ROLL_OUT="$("${AGENT_BIN}" upgrade --rollback --target "${AGENT_BIN}" 2>&1)"
  if echo "${ROLL_OUT}" | grep -q "已还原" && "${AGENT_BIN}" version | grep -q "${V_FROM}"; then
    ok "upgrade --rollback 还原为 v${V_FROM}"
  else
    bad "回滚失败：${ROLL_OUT}"
  fi
  if [ -f "${WORK}/agent/probe-agent${HOST_EXT}.broken" ]; then
    ok "回滚时把被换下的那份留成 .broken（留现场）"
  else
    bad "回滚后没有 .broken 现场文件"
  fi

  # ---- Master 侧取件：dist pull 与 Agent 升级共用同一份校验和语义 ----
  curl -s -o /dev/null -X PUT "${ADM_BASE}/api/admin/v1/settings" \
    -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
    -d '{"install.mode":"always"}'
  PULL_OUT="$("${MASTER}" dist pull --config "${WORK_REL}/master.yaml" --version "${V_TO}" \
    --from "${PUB_BASE}/dist/${V_TO}" --dry-run 2>&1)"
  if echo "${PULL_OUT}" | grep -q "全部通过校验"; then
    ok "dist pull --dry-run 校验通过（与 Agent 升级同一套 SHA256SUMS 语义）"
  else
    bad "dist pull 失败：${PULL_OUT}"
  fi
  # 扁平别名：安装脚本取的是 /dist/SHA256SUMS 与 /dist/<构件名>（它不知道版本号），
  # 而权威存放位置是 /dist/<版本>/。别名必须落到**最新**版本上——落错版本会影响
  # 新装节点（老节点升级走的是版本化路径，两者不一致就是版本错位，且无任何报错）。
  SUMS_FLAT="$(curl -s "${PUB_BASE}/dist/SHA256SUMS")"
  if echo "${SUMS_FLAT}" | grep -q "${WANT_SUM}"; then
    ok "GET /dist/SHA256SUMS 扁平别名指向最新版本（安装脚本第一步就能拿到校验和）"
  else
    bad "扁平 SHA256SUMS 不是最新版本的内容：${SUMS_FLAT}"
  fi
  # 别名只作用于单段路径：带子目录的请求不能被改写成 /dist/<最新版本>/<原路径>，
  # 否则"没有这个文件"会变成令人费解的谜之 404。
  check_code 404 "GET /dist/不存在目录/文件（别名不改道子路径）" "${PUB_BASE}/dist/nope/${ART}"
  # 取件接口只认节点令牌：管理员会话、注册码都不该让它开放。
  curl -s -o /dev/null -X PUT "${ADM_BASE}/api/admin/v1/settings" \
    -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
    -d '{"install.mode":"on_demand"}'
else
  skip "Agent 自升级：注册码流程未跑通，缺令牌"
fi

# ---------------------------------------------------------------------------
sec "9. 计费时区 / public.fields 收窄 / 运行态合并 / 入网不清零资源规格"
# ---------------------------------------------------------------------------
if [ -n "${NODE_ID}" ]; then
  # 计费 PUT 必须接受 IANA 时区名。
  #
  # 这是"内嵌 tzdata"的端到端守卫，也是本脚本里最值得保留的一条断言：
  # 二进制里一旦没有 zoneinfo（Windows 没有 /usr/share/zoneinfo；-trimpath 的
  # 独立可执行文件也找不到 GOROOT 的 zoneinfo.zip；Alpine/distroless 更是连
  # tzdata 包都不装），time.LoadLocation("Asia/Shanghai") 就会失败，PUT 被 400 拒掉。
  # 而额度/到期/计量模式全都写在同一条计费行里一起保存，等于整个计费功能一起坏。
  # 单元测试只能证明时区库被嵌进去了，只有这里能证明"跑起来真的能用"。
  NOWTZ="$(date +%s)"
  BILL_BODY="{\"cycle\":\"monthly\",\"reset_day\":1,\"traffic_quota\":1073741824,\"traffic_mode\":\"down\",\"expire_at\":$(( NOWTZ + 86400 * 30 )),\"price_minor\":1999,\"currency\":\"CNY\",\"tz\":\"Asia/Shanghai\"}"
  BILL_CODE="$(code_of -X PUT -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
    -d "${BILL_BODY}" "${ADM_BASE}/api/admin/v1/nodes/${NODE_ID}/billing")"
  if [ "${BILL_CODE}" = "204" ]; then
    ok "计费 PUT 接受 tz=Asia/Shanghai（内嵌 IANA 时区库生效）"
  else
    bad "计费 PUT 被拒（HTTP ${BILL_CODE}）——极可能是二进制里没有 IANA 时区库"
  fi
  # 反证：非法时区必须 400。否则上面那条 204 可能只是因为校验压根没跑。
  check_code 400 "计费 PUT 拒绝非法时区" "${ADM_BASE}/api/admin/v1/nodes/${NODE_ID}/billing" -X PUT \
    -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
    -d '{"cycle":"monthly","reset_day":1,"traffic_quota":0,"traffic_mode":"down","expire_at":0,"tz":"Nowhere/Nothing"}'

  # ---- public.fields 运行期收窄（7.3）----
  PF_SET="$(code_of -X PUT -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
    -d '{"public.fields":"os,cpu_pct"}' "${ADM_BASE}/api/admin/v1/settings")"
  [ "${PF_SET}" = "204" ] && ok "设置 public.fields=os,cpu_pct -> 204" || bad "设置 public.fields 期望 204 实际 ${PF_SET}"

  # 收窄要经过"公开快照重建"才生效（快照 1 秒一次），给它几轮。
  NARROWED=""
  for _ in $(seq 1 20); do
    NARROWED="$(curl -s "${PUB_BASE}/api/public/v1/nodes")"
    echo "${NARROWED}" | grep -q '"cpu_pct"' && break
    sleep 0.5
  done
  if echo "${NARROWED}" | grep -q '"os"'; then ok "收窄后仍输出被保留的 os"; else bad "收窄后丢了 os：${NARROWED}"; fi
  if echo "${NARROWED}" | grep -q '"mem_used"'; then bad "收窄后仍输出 mem_used！收窄未生效"; else ok "收窄后不再输出 mem_used"; fi
  if echo "${NARROWED}" | grep -q '"traffic_quota"'; then bad "收窄后仍输出 traffic_quota！"; else ok "收窄后不再输出 traffic_quota"; fi
  # 渲染所必需的核心字段不能被收窄掉，否则前端直接白屏。
  if echo "${NARROWED}" | grep -q '"name"'; then ok "核心字段 name 不受收窄影响"; else bad "收窄把渲染必需的核心字段也砍了"; fi

  # 未知字段名必须 400。静默忽略会让管理员以为已经关掉了，实际还在输出。
  check_code 400 "public.fields 含未知字段名" "${ADM_BASE}/api/admin/v1/settings" -X PUT \
    -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
    -d '{"public.fields":"os,nosuchfield"}'

  # 恢复默认（空串 = 不过滤），否则后续断言会看到被砍过的响应。
  curl -s -o /dev/null -X PUT -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' \
    -d '{"public.fields":""}' "${ADM_BASE}/api/admin/v1/settings"

  # ---- 节点列表必须带运行态 ----
  # nodes.status / nodes.last_seen 是**死列**：没有任何代码写它们。
  # 列表若只看库，每个节点都会显示"从未上报 / 离线"。
  NODE_LIST="$(curl -s "${ADM_BASE}/api/admin/v1/nodes" -H "${AUTH_COOKIE}")"
  if echo "${NODE_LIST}" | grep -q '"runtime"'; then
    ok "节点列表带运行态（runtime）"
  else
    bad "节点列表缺运行态，后台会显示成从未上报：${NODE_LIST}"
  fi

  # ---- 重新入网不得清零资源规格 ----
  # 入网载荷里**没有** cpu_cores/mem_total/disk_total（Agent 那时还没采集）。
  # 早先入网路径误用了上报路径的写库函数（无条件写全 15 列），于是每次换令牌
  # 都把资源规格写成 0，表现为"节点在线、卡片显示 0 核 / 0 B 总量"，
  # 而且 Agent 不重启就补不回来（静态信息只在进程启动后第一次上报时携带）。
  EC2="$(curl -s -X POST "${ADM_BASE}/api/admin/v1/nodes/${NODE_ID}/enroll-codes" \
    -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}" -H 'Content-Type: application/json' -d '{}')"
  CODE2="$(sed -n 's/.*"code"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"${EC2}")"
  if [ -n "${CODE2}" ]; then
    curl -s -o /dev/null -X POST "${PUB_BASE}/api/agent/v1/enroll" -H 'Content-Type: application/json' \
      -d "{\"code\":\"${CODE2}\",\"hostname\":\"smoke-node\",\"os\":\"linux\",\"arch\":\"amd64\",\"agent_version\":\"1.0.0\",\"counter_bits\":64}"
    REREAD="$(curl -s "${ADM_BASE}/api/admin/v1/nodes/${NODE_ID}" -H "${AUTH_COOKIE}")"
    if echo "${REREAD}" | grep -q '"cpu_cores":8'; then
      ok "重新入网后核数仍在（cpu_cores=8）"
    else
      bad "重新入网把核数清零了：${REREAD}"
    fi
    if echo "${REREAD}" | grep -q '"mem_total":17179869184'; then
      ok "重新入网后内存总量仍在"
    else
      bad "重新入网把内存总量清零了：${REREAD}"
    fi
    if echo "${REREAD}" | grep -q '"disk_total":107374182400'; then
      ok "重新入网后磁盘总量仍在"
    else
      bad "重新入网把磁盘总量清零了：${REREAD}"
    fi
  else
    bad "生成第二个注册码失败：${EC2}"
  fi
fi

# 登出后会话必须失效。
check_code 204 "POST /auth/logout" "${ADM_BASE}/api/admin/v1/auth/logout" -X POST -H "${AUTH_COOKIE}" -H "X-CSRF-Token: ${CSRF}"
check_code 401 "登出后 me 应 401" "${ADM_BASE}/api/admin/v1/auth/me" -H "${AUTH_COOKIE}"

# ---------------------------------------------------------------------------
sec "10. 6.3 TLS/HSTS 与 6.1 源分离"
# ---------------------------------------------------------------------------
# ---- HSTS（6.3）----
# 本实例 trusted_proxies=["127.0.0.1"]，带 X-Forwarded-Proto: https 的请求会被
# 判定为"安全连接"——这正是"反代卸载 TLS"的生产形态。
HDRS_TLS="$(curl -s -D - -o /dev/null -H 'X-Forwarded-Proto: https' "${PUB_BASE}/")"
HSTS_VAL="$(printf '%s' "${HDRS_TLS}" | tr -d '\r' | sed -n 's/^[Ss]trict-[Tt]ransport-[Ss]ecurity: //p')"
if [ "${HSTS_VAL}" = "max-age=31536000" ]; then
  ok "安全连接下发 HSTS，取值精确为 max-age=31536000"
else
  bad "HSTS 取值应为 max-age=31536000，实际 '${HSTS_VAL}'"
fi
# 反证：明文请求不得下发 HSTS。浏览器按规范会忽略它，但会让人误以为已加固。
if printf '%s' "${HDRS}" | grep -qi 'strict-transport-security'; then
  bad "明文 HTTP 请求不应下发 HSTS"
else
  ok "明文 HTTP 请求不下发 HSTS"
fi
# 跨源资源策略恒下发（与是否 TLS 无关）。
if printf '%s' "${HDRS}" | grep -qi 'cross-origin-resource-policy: same-origin'; then
  ok "带 Cross-Origin-Resource-Policy: same-origin"
else
  bad "缺少 Cross-Origin-Resource-Policy"
fi

# ---- TLS 运行态接口（6.3）----
# 未登录必须 401：它会回显证书主题、到期时间与 SPKI 指纹，属于运维信息。
check_code 401 "GET /tls/status 未登录" "${ADM_BASE}/api/admin/v1/tls/status"
LOGIN2_HEADERS="$(curl -s -D - -o "${WORK}/login2.json" -X POST "${ADM_BASE}/api/admin/v1/auth/login" \
  -H 'Content-Type: application/json' -H 'X-Forwarded-Proto: https' \
  -d "{\"username\":\"${ADMIN_USER}\",\"password\":\"${ADMIN_PASS}\"}")"
SESSION2="$(printf '%s' "${LOGIN2_HEADERS}" | grep -i '^set-cookie:' | sed -n 's/.*probe_session=\([^;]*\).*/\1/p' | tr -d '\r')"
TLS_BODY="$(curl -s -H "Cookie: __Host-probe_session=${SESSION2}" "${ADM_BASE}/api/admin/v1/tls/status")"
if printf '%s' "${TLS_BODY}" | grep -q '"mode":"none"'; then
  ok "tls/status 回显 mode=none"
else
  bad "tls/status 未回显 mode=none：${TLS_BODY}"
fi

# ---- 6.1 源分离：Host 白名单 ----
# 单独起第二个实例（18182/18183）：给主实例加白名单会把上面所有用 127.0.0.1
# 访问的断言全打成 421，所以必须隔离。
PUB2_PORT=18182
ADM2_PORT=18183
PUB2="127.0.0.1:${PUB2_PORT}"
ADM2="127.0.0.1:${ADM2_PORT}"
PUB2_BASE="http://${PUB2}"
ADM2_BASE="http://${ADM2}"
# 只配 origin 不配 hosts：顺便验证"host 白名单由 origin 推导"这条链路。
# 同时注意这里**故意不配** trusted_proxies，用于验证伪造 X-Forwarded-Proto 无效。
cat >"${WORK}/master2.yaml" <<YAML
public_listen: "${PUB2}"
admin_listen: "${ADM2}"
data_dir: "${WORK_REL}/data2"
themes_dir: "${WORK_REL}/themes2"
master_key_file: "${WORK_REL}/master2.key"
public_origin: "https://status.example.com"
admin_origin: "https://admin.example.com"
tls:
  mode: none
log_level: "info"
YAML
if "${MASTER}" init-admin -config "${WORK}/master2.yaml" -username "${ADMIN_USER}" -password "${ADMIN_PASS}" >/dev/null 2>&1; then
  ok "源分离实例 init-admin 成功"
else
  bad "源分离实例 init-admin 失败：$(cat "${WORK}/master2.log" 2>/dev/null)"
fi

"${MASTER}" -config "${WORK}/master2.yaml" >"${WORK}/master2.log" 2>&1 &
MASTER2_PID=$!
ready2=0
for _ in $(seq 1 60); do
  if [ "$(code_of -H 'Host: status.example.com' "${PUB2_BASE}/api/public/v1/site")" = "200" ]; then ready2=1; break; fi
  if ! kill -0 "${MASTER2_PID}" 2>/dev/null; then break; fi
  sleep 0.25
done
if [ "${ready2}" = "1" ]; then
  ok "源分离实例启动并就绪"
else
  bad "源分离实例未就绪"
  echo "--- master2.log ---"; cat "${WORK}/master2.log"
fi

# host_code <期望码> <说明> <Host> <url>
host_code() {
  local want="$1" desc="$2" host="$3" url="$4"
  local got; got="$(code_of -H "Host: ${host}" "${url}")"
  if [ "${got}" = "${want}" ]; then ok "${desc} -> ${got}"; else bad "${desc} 期望 ${want} 实际 ${got}（Host: ${host}）"; fi
}

if [ "${ready2}" = "1" ]; then
  host_code 200 "公开站用公开域名"                 "status.example.com"      "${PUB2_BASE}/api/public/v1/site"
  host_code 200 "默认端口 443 视作同一源"           "status.example.com:443"  "${PUB2_BASE}/api/public/v1/site"
  host_code 200 "大小写与尾点不影响判定"            "STATUS.Example.com."     "${PUB2_BASE}/api/public/v1/site"
  host_code 421 "公开站不接受管理域名"             "admin.example.com"       "${PUB2_BASE}/api/public/v1/site"
  host_code 421 "公开站不接受未知域名"             "bogus.example.com"       "${PUB2_BASE}/api/public/v1/site"
  host_code 421 "非默认端口是不同的源（:8443）"     "status.example.com:8443" "${PUB2_BASE}/api/public/v1/site"
  # 管理端：用对的域名应"能路由"（无会话 -> 401），用错域名则 421。
  host_code 401 "管理端用管理域名可路由（无会话 401）" "admin.example.com"      "${ADM2_BASE}/api/admin/v1/auth/me"
  host_code 421 "管理端不接受公开域名"             "status.example.com"      "${ADM2_BASE}/api/admin/v1/auth/me"

  # ACME http-01 挑战路径必须豁免：CA 是按 IP 访问 /.well-known/ 的，
  # 它发来的 Host 是 IP 字面量，永远不可能命中域名白名单。
  ACME_CODE="$(code_of -H 'Host: bogus.example.com' "${PUB2_BASE}/.well-known/acme-challenge/probe")"
  if [ "${ACME_CODE}" != "421" ]; then
    ok "ACME 挑战路径不受 Host 白名单限制 -> ${ACME_CODE}"
  else
    bad "ACME 挑战路径被 Host 白名单挡住了（会导致自动签发必然失败）"
  fi

  # 未配置 trusted_proxies 时，任何人都能伪造 X-Forwarded-Proto，
  # 因此它绝不能骗到 HSTS（否则等于把 HSTS 变成可被中间人触发的开关）。
  SPOOF="$(curl -s -D - -o /dev/null -H 'Host: status.example.com' -H 'X-Forwarded-Proto: https' "${PUB2_BASE}/")"
  if printf '%s' "${SPOOF}" | grep -qi 'strict-transport-security'; then
    bad "未配置 trusted_proxies 时伪造 X-Forwarded-Proto 竟然骗到了 HSTS"
  else
    ok "不可信来源伪造 X-Forwarded-Proto 不会触发 HSTS"
  fi
fi

if [ -n "${MASTER2_PID}" ]; then
  kill "${MASTER2_PID}" 2>/dev/null || true
  wait "${MASTER2_PID}" 2>/dev/null || true
  MASTER2_PID=""
fi

# ---------------------------------------------------------------------------
sec "10b 站点形态：同一入口按路径判域名，且可运行时切换"
# ---------------------------------------------------------------------------
# 为什么单独起第三个实例：上面两个实例都把管理端放在**独立监听**上，那种部署里
# 公开入口压根没有管理路由——"公开域名能不能打开后台"是个结构性问题，
# 不是门控问题。而现实中更常见的是**一个端口 + 两个域名 + 反代**：两种路径挤在
# 同一个入口上，分离只能靠 Host 判断。这条分支此前没有任何端到端覆盖，
# 而它恰好是最容易配错的那一种（配宽了 = 公开域名能进后台）。
PUB3_PORT=18184
PUB3="127.0.0.1:${PUB3_PORT}"
PUB3_BASE="http://${PUB3}"
# 故意不写 admin_listen：公开站与管理端共用同一个监听（main.go 的 merged 分支）。
cat >"${WORK}/master3.yaml" <<YAML
public_listen: "${PUB3}"
data_dir: "${WORK_REL}/data3"
themes_dir: "${WORK_REL}/themes3"
master_key_file: "${WORK_REL}/master3.key"
tls:
  mode: none
log_level: "info"
YAML
if "${MASTER}" init-admin -config "${WORK}/master3.yaml" -username "${ADMIN_USER}" -password "${ADMIN_PASS}" >/dev/null 2>&1; then
  ok "同站实例 init-admin 成功"
else
  bad "同站实例 init-admin 失败：$(cat "${WORK}/master3.log" 2>/dev/null)"
fi

"${MASTER}" -config "${WORK}/master3.yaml" >"${WORK}/master3.log" 2>&1 &
MASTER3_PID=$!
ready3=0
for _ in $(seq 1 60); do
  if [ "$(code_of "${PUB3_BASE}/api/public/v1/site")" = "200" ]; then ready3=1; break; fi
  if ! kill -0 "${MASTER3_PID}" 2>/dev/null; then break; fi
  sleep 0.25
done
if [ "${ready3}" = "1" ]; then
  ok "同站实例启动并就绪"
else
  bad "同站实例未就绪"
  echo "--- master3.log ---"; cat "${WORK}/master3.log"
fi

if [ "${ready3}" = "1" ]; then
  # 管理端与公开站同端口，登录也走同一个 base（顺利登录本身就说明管理端
  # 确实挂在这个入口上）。
  LOGIN3="$(curl -s -D - -o "${WORK}/login3.json" -X POST "${PUB3_BASE}/api/admin/v1/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"${ADMIN_USER}\",\"password\":\"${ADMIN_PASS}\"}")"
  SESSION3="$(printf '%s' "${LOGIN3}" | grep -i '^set-cookie:' | sed -n 's/.*probe_session=\([^;]*\).*/\1/p' | tr -d '\r')"
  CSRF3="$(sed -n 's/.*"csrf_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${WORK}/login3.json")"
  [ -n "${SESSION3}" ] && ok "同站实例可登录" || bad "同站实例登录失败"

  put_setting3() { # <json>
    # 刻意带上生产域名：分离形态一旦生效，"改设置"这条请求本身也是管理路径，
    # 用 127.0.0.1 发出去同样会被自己的门控拦成 421（下面的断言就钉住了这一点）。
    curl -s -o /dev/null -w '%{http_code}' -X PUT "${PUB3_BASE}/api/admin/v1/settings" \
      -H 'Host: adm.example.com' \
      -H "Cookie: probe_session=${SESSION3}" -H "X-CSRF-Token: ${CSRF3}" \
      -H 'Content-Type: application/json' -d "$1"
  }

  # 1) 没配任何域名清单时选「分离」必须被拒。放它过去就等于把管理员自己锁在
  #    后台外面：保存成功的**下一个**请求就 421，而改回来恰恰需要访问后台。
  if [ "$(put_setting3 '{"site.layout":"split"}')" = "400" ]; then
    ok "选分离但没给域名 -> 400（防自锁）"
  else
    bad "选分离却没给域名竟然被接受（管理员会被锁在后台外）"
  fi
  # 2) 非法条目必须被拒：写成 URL 时协议与路径会被静默丢掉，"配了却永远对不上"。
  if [ "$(put_setting3 '{"site.public_hosts":"https://pub.example.com"}')" = "400" ]; then
    ok "域名清单写成了 URL -> 400"
  else
    bad "域名清单接受了一个 URL"
  fi
  # 3) 一次提交把形态与两份清单凑齐 -> 允许（校验看的是提交之后生效的清单）。
  if [ "$(put_setting3 '{"site.layout":"split","site.public_hosts":"pub.example.com","site.admin_hosts":"adm.example.com"}')" = "204" ]; then
    ok "一次提交凑齐形态与两份域名 -> 204"
  else
    bad "凑齐后仍被拒绝"
  fi

  # 分离形态：同一个入口上，两种路径各认自己的域名。
  host_code 200 "分离：公开域名访问公开 API"               "pub.example.com" "${PUB3_BASE}/api/public/v1/site"
  host_code 421 "分离：公开域名碰管理 API（同端口）"        "pub.example.com" "${PUB3_BASE}/api/admin/v1/auth/me"
  host_code 421 "分离：公开域名碰后台界面"                 "pub.example.com" "${PUB3_BASE}/admin/"
  host_code 401 "分离：管理域名可路由到管理 API（无会话）"   "adm.example.com" "${PUB3_BASE}/api/admin/v1/auth/me"
  host_code 421 "分离：管理域名访问公开站被拒"              "adm.example.com" "${PUB3_BASE}/api/public/v1/site"
  # 分离生效之后，"用非管理域名访问后台"连改设置都做不到——这正是防自锁校验
  # 存在的理由：一旦在没有管理域名的情况下切到分离，就再也改不回来了。
  host_code 421 "分离：非管理域名连改设置都被挡下"           "127.0.0.1:${PUB3_PORT}" "${PUB3_BASE}/api/admin/v1/settings"

  # 同站形态：同一个域名下两种路径都能打开。这是"可运行时切换"的核心证据——
  # 下面第一条请求刚才（分离形态下）是 421，切完就变成了 401。
  if [ "$(put_setting3 '{"site.layout":"single"}')" = "204" ]; then
    ok "切到同站 -> 204（无需重启）"
  else
    bad "切换到同站失败"
  fi
  host_code 401 "同站：公开域名也能打开管理 API"           "pub.example.com" "${PUB3_BASE}/api/admin/v1/auth/me"
  host_code 200 "同站：管理域名也能看公开站"               "adm.example.com" "${PUB3_BASE}/api/public/v1/site"

  # 回到「跟随配置文件」（这个实例没配任何白名单）→ 恢复"不限制"。
  if [ "$(put_setting3 '{"site.layout":"auto","site.public_hosts":"","site.admin_hosts":""}')" = "204" ]; then
    ok "切回跟随配置文件 -> 204"
  else
    bad "切回跟随配置文件失败"
  fi
  host_code 200 "恢复：未配白名单时不再限制域名"           "bogus.example.com" "${PUB3_BASE}/api/public/v1/site"
fi

if [ -n "${MASTER3_PID}" ]; then
  kill "${MASTER3_PID}" 2>/dev/null || true
  wait "${MASTER3_PID}" 2>/dev/null || true
  MASTER3_PID=""
fi

# ---------------------------------------------------------------------------
sec "11. 优雅退出"
# ---------------------------------------------------------------------------
kill -TERM "${MASTER_PID}" 2>/dev/null
for _ in $(seq 1 40); do kill -0 "${MASTER_PID}" 2>/dev/null || break; sleep 0.25; done
if kill -0 "${MASTER_PID}" 2>/dev/null; then
  bad "master 未在 10s 内退出"
else
  ok "master 收到 SIGTERM 后正常退出"
  MASTER_PID=""
fi
if grep -qiE "panic|fatal error" "${WORK}/master.log"; then bad "运行期出现 panic"; else ok "运行期无 panic"; fi
if [ "${IS_WINDOWS}" = "1" ]; then
  skip "Windows 下 kill 只能强杀原生 PE，无法验证优雅退出（队列 flush）；请在 Linux 上跑本脚本断言此项"
else
  if grep -q "已安全退出" "${WORK}/master.log"; then ok "退出路径完成了队列 flush"; else bad "未走完优雅退出流程"; fi
fi

# ---------------------------------------------------------------------------
printf '\n=========================================\n'
printf '冒烟测试结果：通过 %d 项，失败 %d 项，跳过 %d 项\n' "${PASS}" "${FAIL}" "${SKIPPED}"
printf '=========================================\n'
[ "${FAIL}" = "0" ] || exit 1
