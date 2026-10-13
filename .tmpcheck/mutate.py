#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""变异验证：逐条抹掉被护栏保护的行，断言对应测试**变红**。

不变量 20 的要求：源码级护栏"每条都要变异验证"，否则护栏本身可能是摆设。
本脚本只做一次性校验，不属于仓库产物。

用法：python .tmpcheck/mutate.py
"""
import atexit
import io
import os
import signal
import subprocess
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
GOBIN = "C:/Users/msecth/.workbuddy/binaries/go/go1.27.2/go/bin"

# 本机平台，用于跳过"仅某个平台有效"的变异（见 main() 里的 only 判定）。
HOST_PLATFORM = ("windows" if sys.platform.startswith("win")
                 else "darwin" if sys.platform == "darwin" else "linux")

# 开工前把所有会被改动的文件整份快照下来，退出时（含被 Ctrl-C / SIGTERM 打断）
# 无条件写回。第一版没有这层保护，一次超时被杀就留下了一份**变异状态**的 app.js
# ——后续所有"基线就红"都是这个原因，非常难归因。
SNAPSHOT = {}
DIRTY = set()


def snapshot_all():
    for item in MUTANTS:
        rel = item[1]
        if rel in SNAPSHOT:
            continue
        with io.open(os.path.join(ROOT, rel), encoding="utf-8") as f:
            SNAPSHOT[rel] = f.read()


def restore_all():
    for rel, content in SNAPSHOT.items():
        try:
            with io.open(os.path.join(ROOT, rel), "w", encoding="utf-8", newline="") as f:
                f.write(content)
        except Exception as e:  # 尽力而为：退出路径上不能抛
            sys.stderr.write("恢复 %s 失败：%r\n" % (rel, e))


def _on_signal(signum, frame):
    restore_all()
    sys.exit(130)


# (说明, 相对文件, 旧串, 新串, 目标包, 测试名)
MUTANTS = [
    # --- 顶栏 ---
    ("抹掉 node_fee 的 cardOn 守卫", "internal/theme/builtin/app.js",
     "c.fee.hidden = !cardOn('node_fee');", "c.fee.hidden = false;",
     "./internal/theme/", "TestNodeFeeIsGated"),
    ("续费价格行的 data-card 键改名", "internal/theme/builtin/app.js",
     "fee.setAttribute('data-card', 'node_fee');", "fee.setAttribute('data-card', 'node_price');",
     "./internal/theme/", "TestNodeCardBlocksCarryDataCardAttr"),
    ("图标按钮退化回文字按钮", "internal/theme/builtin/index.html",
     '<button class="icon-btn tgl" id="btn-theme"', '<button class="tgl" id="btn-theme"',
     "./internal/theme/", "TestTopbarUsesIconButtons"),
    ("主题按钮里塞回可见文字", "internal/theme/builtin/index.html",
     '<path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>\n          </button>',
     '<path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>深色\n          </button>',
     "./internal/theme/", "TestTopbarUsesIconButtons"),
    ("去掉登录图标（未登录入口）", "internal/theme/builtin/index.html",
     'class="icon-btn" id="btn-login" href="/admin/"', 'id="btn-login" href="/admin/"',
     "./internal/theme/", "TestTopbarUsesIconButtons"),
    ("站名挪到按钮组之后", "internal/theme/builtin/index.html",
     '<h1 class="site-title" id="site-title">Server Probe</h1>\n        <span class="muted" id="updated"></span>',
     '<span class="muted" id="updated"></span>',
     "./internal/theme/", "TestTopbarTitleComesFirst"),
    ("顶栏对齐退回 baseline", "internal/theme/builtin/style.css",
     ".topbar-right {\n  margin-left: auto;\n  display: flex;\n  align-items: center;",
     ".topbar-right {\n  margin-left: auto;\n  display: flex;\n  align-items: baseline;",
     "./internal/theme/", "TestTopbarIsVerticallyCentered"),
    ("图标按钮尺寸漂移", "internal/theme/builtin/style.css",
     ".icon-btn {\n  width: 28px;\n  height: 28px;",
     ".icon-btn {\n  width: 30px;\n  height: 28px;",
     "./internal/theme/", "TestTopbarIsVerticallyCentered"),
    ("删掉一处图标切换 CSS 规则", "internal/theme/builtin/style.css",
     ':root[data-theme="dark"] .tgl .ico-moon { display: none; }',
     ':root[data-theme="dark"] .tgl .ico-moon { display: block; }',
     "./internal/theme/", "TestThemeIconSwapIsCSSOnly"),

    ("后台节点卡丢掉续费行", "internal/api/adminui/admin.js",
     "var fee = el('div', 'n-fee');", "var fee = el('div', 'n-fee-x');",
     "./internal/theme/", "TestAdminNodeCardShowsFee"),
    ("后台续费文案不认金额", "internal/api/adminui/admin.js",
     "if (n.fee_monthly > 0) {", "if (n.fee_monthly2 > 0) {",
     "./internal/theme/", "TestAdminNodeCardShowsFee"),
    ("后台算出金额却没写进 DOM", "internal/api/adminui/admin.js",
     "setText(c.feeV, feeText);", "setText(c.feeVx, feeText);",
     "./internal/theme/", "TestAdminNodeCardShowsFee"),
    ("后台不再下发 fee_cycle", "internal/api/admin_runtime.go",
     "FeeCycle string", "FeeCyc string",
     "./internal/theme/", "TestAdminNodeCardShowsFee"),
    ("两前端续费行行高不一致", "internal/api/adminui/admin.css",
     ".n-fee {\n  display: flex;\n  align-items: baseline;\n  gap: 6px;\n  height: 16px;",
     ".n-fee {\n  display: flex;\n  align-items: baseline;\n  gap: 6px;\n  height: 14px;",
     "./internal/theme/", "TestFeeRowIsIdenticalInBothFrontends"),

    # --- 点阵时间窗 ---
    ("点阵标题写死近 24 小时", "internal/theme/builtin/app.js",
     "tip.textContent = '延迟 / 丢包 · ' + latWindowLabel();",
     "tip.textContent = '延迟 / 丢包 · 近 24 小时';",
     "./internal/theme/", "TestLatencyWindowIsConfigurable"),
    ("重绘签名丢掉 latWindow", "internal/theme/builtin/app.js",
     "var sig = latWindow + '|on' + JSON.stringify(list);",
     "var sig = 'on' + JSON.stringify(list);",
     "./internal/theme/", "TestLatencyWindowIsConfigurable"),
    ("不再从 /site 读时间窗", "internal/theme/builtin/app.js",
     "if (s.lat_window) latWindow = s.lat_window;", "if (s.lat_wnd) latWindow = s.lat_wnd;",
     "./internal/theme/", "TestLatencyWindowIsConfigurable"),
    ("每点时长写死 45 分钟", "internal/theme/builtin/app.js",
     "tip.title = '每个点约 ' + stepLabel(list[0].step) +",
     "tip.title = '每个点约 ' + stepLabel(2700) +",
     "./internal/theme/", "TestLatencyWindowIsConfigurable"),

    # --- 后台换装 ---
    ("后台某变量不再与公开看板同值", "internal/api/adminui/admin.css",
     "  --fg: #121212;          /* oklch(0.18 0 0)，正文/进度条填充 */",
     "  --fg: #131313;          /* oklch(0.18 0 0)，正文/进度条填充 */",
     "./internal/theme/", "TestAdminSharesPublicPalette"),
    ("两套暗色块之一被改坏", "internal/api/adminui/admin.css",
     '  :root:not([data-theme="light"]) {\n    color-scheme: dark;\n    --bg: #0a0a0a;',
     '  :root:not([data-theme="light"]) {\n    color-scheme: dark;\n    --bg: #0c0c0c;',
     "./internal/theme/", "TestAdminDarkBlocksStayInSync"),
    ("后台 --panel 从别名改成字面值", "internal/api/adminui/admin.css",
     "  --card: #ffffff;\n  --panel: var(--card);",
     "  --card: #ffffff;\n  --panel: #ffffff;",
     "./internal/theme/", "TestAdminPanelAliasesCard"),
    ("后台进度条复活阈值变色", "internal/api/adminui/admin.css",
     ".track i {", ".track i.warn { background: #16a34a; }\n.track i {",
     "./internal/theme/", "TestAdminProgressBarIsInk"),
    ("后台图表调色板退回彩色", "internal/api/adminui/admin.js",
     "var PALETTE = ['--c1', '--c2', '--c3', '--c4', '--c5', '--c2'];",
     "var PALETTE = ['#3b82f6', '#22c55e', '#f59e0b', '#ef4444', '#6366f1', '#3b82f6'];",
     "./internal/theme/", "TestAdminChartUsesGreyscalePalette"),
    ("latencyColor 丢掉最淡一档", "internal/api/adminui/admin.js",
     "if (ms <= 50)  return cssVar('--c5', '#c4c4c4');",
     "if (ms <= 50)  return cssVar('--c4', '#9e9e9e');",
     "./internal/theme/", "TestAdminChartUsesGreyscalePalette"),
    ("虚线不再复位", "internal/api/adminui/admin.js",
     "ctx.setLineDash([])", "ctx.setLineDash([0, 0])",
     "./internal/theme/", "TestAdminChartUsesGreyscalePalette", True),
    ("登录卡丢掉主题切换按钮", "internal/api/adminui/index.html",
     '<button type="button" class="icon-btn tgl" aria-label="切换深浅色" title="切换深浅色">',
     '<button type="button" class="icon-btn" aria-label="切换深浅色" title="切换深浅色">',
     "./internal/theme/", "TestAdminLoginPageIsThemed"),
    ("只绑一颗 .tgl（顶栏那颗失灵）", "internal/api/adminui/admin.js",
     "document.querySelectorAll('.tgl')", "document.querySelectorAll('.tgl:not(:first-child)')",
     "./internal/theme/", "TestAdminLoginPageIsThemed", True),
    ("设置项中文名没登记", "internal/api/adminui/admin.js",
     "'public.lat_window': ['节点卡点阵时间窗'", "'public.lat_wnd': ['节点卡点阵时间窗'",
     "./internal/theme/", "TestLatencyWindowRegisteredInAdmin"),
    ("时间窗选项少一档", "internal/api/adminui/admin.js",
     "30m=近 30 分钟,1h=近 1 小时,6h=近 6 小时,24h=近 24 小时",
     "30m=近 30 分钟,1h=近 1 小时,24h=近 24 小时",
     "./internal/theme/", "TestLatencyWindowRegisteredInAdmin"),

    ("后台 ?theme= 覆盖被删", "internal/api/adminui/admin.js",
     "q = new URLSearchParams(location.search).get('theme') || '';", "q = '';",
     "./internal/theme/", "TestAdminLoginPageIsThemed"),

    # --- 后端：点阵时间窗 ---
    ("段长退回包级常量", "internal/state/public.go",
     "\tstep := windowS / int64(segments)", "\tstep := LatencyBarsWindowS / int64(LatencyBarsSegments)",
     "./internal/state/", "TestBuildLatencyBarsHonoursWindow"),
    ("下标上界退回包级常量", "internal/state/public.go",
     "if idx < 0 || idx >= segments {", "if idx < 0 || idx >= LatencyBarsSegments {",
     "./internal/state/", "TestBuildLatencyBarsHonoursWindow"),
    ("30 分钟档改读 10 分钟层", "internal/state/public.go",
     'case "30m":\n\t\treturn BarLayout{WindowS: 1800, Segments: 30, Raw: true}',
     'case "30m":\n\t\treturn BarLayout{WindowS: 1800, Segments: 30, Raw: false}',
     "./internal/state/", "TestLatencyBarLayout"),
    ("未知取值返回零值", "internal/state/public.go",
     "\tdefault: // \"24h\" 与任何未知值：保持历史行为\n\t\treturn BarLayout{WindowS: LatencyBarsWindowS, Segments: LatencyBarsSegments, Raw: false}",
     "\tdefault: // \"24h\" 与任何未知值：保持历史行为\n\t\treturn BarLayout{}",
     "./internal/state/", "TestLatencyBarLayout"),
    ("ReadFrom 不留对齐余量", "internal/state/public.go",
     "func (l BarLayout) ReadFrom(now int64) int64 { return now - l.WindowS - l.Step() }",
     "func (l BarLayout) ReadFrom(now int64) int64 { return now - l.WindowS }",
     "./internal/state/", "TestLatencyBarLayout"),

    # --- 第 109 项：全站去原生蓝 + 自绘日期弹层 ---
    ("勾选框 accent 退回默认蓝（公开）", "internal/theme/builtin/style.css",
     'input[type="checkbox"], input[type="radio"] { accent-color: var(--fg); }',
     'input[type="checkbox"], input[type="radio"] { accent-color: auto; }',
     "./internal/theme/", "TestFormControlsUseInkAccent"),
    ("勾选框 accent 退回默认蓝（后台）", "internal/api/adminui/admin.css",
     'input[type="checkbox"], input[type="radio"] { accent-color: var(--fg); }',
     'input[type="checkbox"], input[type="radio"] { accent-color: auto; }',
     "./internal/theme/", "TestFormControlsUseInkAccent"),
    ("到期日退回原生 date 输入", "internal/api/adminui/admin.js",
     "var expInput = addInput('b-expire', 'text', unixToDateInput(savedExpire));",
     "var expInput = addInput('b-expire', 'date', unixToDateInput(savedExpire));",
     "./internal/theme/", "TestDatePopupIsSelfDrawn"),
    ("自绘弹层打不开", "internal/api/adminui/admin.js",
     "      render();\n      pop.hidden = false;\n    }",
     "      render();\n      pop.hidden = true;\n    }",
     "./internal/theme/", "TestDatePopupIsSelfDrawn"),
    ("自绘弹层丢样式", "internal/api/adminui/admin.css",
     ".dp-grid { display: grid; grid-template-columns: repeat(7, 1fr); gap: 2px; }",
     ".dp-grid-x { display: grid; grid-template-columns: repeat(7, 1fr); gap: 2px; }",
     "./internal/theme/", "TestDatePopupIsSelfDrawn"),

    # --- 第 110 项：悬停十字线跟手 + 命中容差 ---
    ("悬停十字线钉回最近样本（公开）", "internal/theme/builtin/app.js",
     "var x = ax.padL + ax.plotW * ((hoverTs - ax.tMin) / (ax.tMax - ax.tMin));",
     "var x = ax.padL + ax.plotW * ((hits.length ? hits[0].pt.ts : hoverTs - ax.tMin) / (ax.tMax - ax.tMin));",
     "./internal/theme/", "TestChartHoverFollowsCursorAndHonoursTolerance"),
    ("悬停十字线钉回最近样本（后台）", "internal/api/adminui/admin.js",
     "var x = ax.padL + ax.plotW * ((hoverTs - ax.tMin) / (ax.tMax - ax.tMin));",
     "var x = ax.padL + ax.plotW * ((hits.length ? hits[0].pt.ts : hoverTs - ax.tMin) / (ax.tMax - ax.tMin));",
     "./internal/theme/", "TestChartHoverFollowsCursorAndHonoursTolerance"),
    ("命中容差被关掉（公开）", "internal/theme/builtin/app.js",
     "if (best && bestD <= tol) hits.push({ s: s, pt: best });",
     "if (best) hits.push({ s: s, pt: best });",
     "./internal/theme/", "TestChartHoverFollowsCursorAndHonoursTolerance"),
    ("悬停空档照样弹框（后台）", "internal/api/adminui/admin.js",
     "if (!hits.length) { ctx.restore(); return; }",
     "if (false) { ctx.restore(); return; }",
     "./internal/theme/", "TestChartHoverFollowsCursorAndHonoursTolerance"),

    # --- 第 111 / 112 项：审计分页与价格并排 ---
    ("审计默认每页退回 50", "internal/api/adminui/admin.js",
     "audit: { limit: 20, offset: 0, total: 0 }",
     "audit: { limit: 50, offset: 0, total: 0 }",
     "./internal/theme/", "TestAuditDefaultPageSizeIs20"),
    ("net-line 行高漂移", "internal/api/adminui/admin.css",
     ".net-line {\n  display: flex;\n  align-items: baseline;\n  justify-content: space-between;\n  gap: 10px;\n  height: 17px;",
     ".net-line {\n  display: flex;\n  align-items: baseline;\n  justify-content: space-between;\n  gap: 10px;\n  height: 19px;",
     "./internal/theme/", "TestFeeRowIsIdenticalInBothFrontends"),
    ("价格挪出速率行（公开）", "internal/theme/builtin/app.js",
     "rateLine.appendChild(fee);",
     "rateLine.appendChild(feeV);",
     "./internal/theme/", "TestNodeFeeIsGated"),
    ("net 显隐不看价格开关", "internal/theme/builtin/app.js",
     "c.net.hidden = !(cardOn('node_fee') || cardOn('node_net'));",
     "c.net.hidden = !cardOn('node_net');",
     "./internal/theme/", "TestNodeFeeIsGated"),

    # --- 第 96 项：告警引擎与渠道（internal/alert）---
    ("清洗函数不处理换行（消息注入）", "internal/alert/channel.go",
     'strings.ToUpper(level), stripControl(nodeName), stripControl(kind), stripControl(detail))',
     'strings.ToUpper(level), nodeName, stripControl(kind), stripControl(detail))',
     "./internal/alert/", "TestBuildMessageEscapesControlChars"),
    ("邮件头不清洗换行（头注入）", "internal/alert/channel.go",
     's = strings.ReplaceAll(s, "\\n", " ")',
     's = s',
     "./internal/alert/", "TestSanitizeHeaderNeutralisesInjection"),
    ("MarkdownV2 保留字符不再转义", "internal/alert/channel.go",
     "if strings.ContainsRune(reserved, r) {",
     "if false {",
     "./internal/alert/", "TestEscapeTelegramMarkdownV2EscapesEveryReservedChar"),
    ("Telegram 不声明 parse_mode（转义白做）", "internal/alert/channel.go",
     'payload["parse_mode"] = mode',
     "_ = mode",
     "./internal/alert/", "TestTelegramSendDeclaresParseModeItEscapedFor"),
    ("parse_mode 不做大小写归一化", "internal/alert/channel.go",
     'return "HTML"',
     "return mode",
     "./internal/alert/", "TestTelegramSendDeclaresParseModeItEscapedFor"),
    ("未识别的 parse_mode 也照发", "internal/alert/channel.go",
     'case "markdownv2":\n\t\treturn "MarkdownV2"\n\t}\n\treturn ""',
     'case "markdownv2":\n\t\treturn "MarkdownV2"\n\t}\n\treturn mode',
     "./internal/alert/", "TestTelegramSendDeclaresParseModeItEscapedFor"),
    ("最后一次失败后仍退避（调度被拖住）", "internal/alert/engine.go",
     "if i == attempts-1 {",
     "if i < 0 {",
     "./internal/alert/", "TestSendWithRetryGivesUpAfterThreeAttempts"),
    ("退避不看 ctx 取消", "internal/alert/engine.go",
     "case <-ctx.Done():\n\t\t\treturn ctx.Err()",
     "case <-ctx.Done():\n\t\t\tbreak",
     "./internal/alert/", "TestSendWithRetryStopsOnContextCancel"),
    ("持续时长按最后一次命中计时", "internal/alert/engine.go",
     "if rule.Params.DurationS > 0 && now-v.firstAt < int64(rule.Params.DurationS) {",
     "if rule.Params.DurationS > 0 && now-v.lastSeen < int64(rule.Params.DurationS) {",
     "./internal/alert/", "TestObserveMeasuresDurationFromFirstHit"),
    ("连续确认计数不分节点（误报）", "internal/alert/engine.go",
     "key := streakKey{fmt.Sprint(nodeID), stateKind}",
     "key := streakKey{\"all\", stateKind}",
     "./internal/alert/", "TestObserveStreakIsPerNodeAndPerKind"),
    ("告警状态按规则落库而非梯级", "internal/alert/engine.go",
     'NodeID: nodeID, Kind: stateKind, State: "firing",',
     'NodeID: nodeID, Kind: baseKind, State: "firing",',
     "./internal/alert/", "TestFireCooldownIsPerTierNotPerRule"),
    ("派发结果不再决定送达标记", "internal/alert/engine.go",
     'if e.dispatch(ctx, Message{Title: fmt.Sprintf("[%s] %s", strings.ToUpper(level), name), Body: detail}) {',
     "if false {",
     "./internal/alert/", "TestFireInsertsEventAndArmsCooldown"),
    ("fake store 不真的翻转送达标记", "internal/alert/engine_test.go",
     "f.events[i].Delivered = true",
     "f.events[i].Delivered = false",
     "./internal/alert/", "TestFireInsertsEventAndArmsCooldown"),

    # --- 第 97 项：internal/db（连接 PRAGMA、迁移执行）---
    ("退化为 DELETE 日志模式（丢掉 WAL）", "internal/db/db.go",
     '"journal_mode(WAL)",', '"journal_mode(DELETE)",',
     "./internal/db/", "TestBuildDSNPinsEveryPragma"),
    ("DSN 里丢掉 foreign_keys", "internal/db/db.go",
     '"foreign_keys(ON)",', '"foreign_keys(OFF)",',
     "./internal/db/", "TestBuildDSNPinsEveryPragma"),
    ("外键失效（级联删除静默失灵）", "internal/db/db.go",
     '"foreign_keys(ON)",', '"foreign_keys(OFF)",',
     "./internal/db/", "TestForeignKeysAreEnforcedOnWriteConnection"),
    ("写连接不再是单写者", "internal/db/db.go",
     "write.SetMaxOpenConns(1)", "write.SetMaxOpenConns(2)",
     "./internal/db/", "TestOpenUsesSingleWriterAndPooledReaders"),
    ("启动完全不做完整性校验（坏库被放行）", "internal/db/db.go",
     "\tif err := s.Write.Ping(); err != nil {\n\t\treturn fmt.Errorf(\"ping: %w\", err)\n\t}\n\tvar result string\n\tif err := s.Write.QueryRow(`PRAGMA quick_check`).Scan(&result); err != nil {\n\t\treturn fmt.Errorf(\"quick_check: %w\", err)\n\t}\n\tif !strings.EqualFold(result, \"ok\") {\n\t\treturn fmt.Errorf(\"database integrity check failed (%s); 请从最近备份恢复\", result)\n\t}\n\treturn nil",
     "\tvar result string\n\t_ = strings.EqualFold(result, \"ok\")\n\treturn nil",
     "./internal/db/", "TestOpenRefusesPageCorruptedDatabase"),
    ("启动不做 quick_check 全库校验", "internal/db/db.go",
     "\tvar result string\n\tif err := s.Write.QueryRow(`PRAGMA quick_check`).Scan(&result); err != nil {\n\t\treturn fmt.Errorf(\"quick_check: %w\", err)\n\t}\n\tif !strings.EqualFold(result, \"ok\") {\n\t\treturn fmt.Errorf(\"database integrity check failed (%s); 请从最近备份恢复\", result)\n\t}\n\treturn nil",
     "\tvar result string\n\t_ = strings.EqualFold(result, \"ok\")\n\treturn nil",
     "./internal/db/", "TestPingAndCheckDetectsPageCorruption"),
    ("迁移不再跳过已应用版本（重启即炸）", "internal/db/migrate.go",
     "SELECT version FROM schema_migrations", "SELECT 0 FROM schema_migrations",
     "./internal/db/", "TestMigrateIsIdempotent"),
    ("迁移不再按版本号升序执行", "internal/db/migrate.go",
     "out[i].version < out[j].version", "out[i].version > out[j].version",
     "./internal/db/", "TestMigrateCreatesSchemaAndRecordsVersions"),
    ("applied_at 不记真实时间", "internal/db/migrate.go",
     "m.version, time.Now().Unix());", "m.version, time.Now().Unix()*0);",
     "./internal/db/", "TestMigrateCreatesSchemaAndRecordsVersions"),
    ("迁移里的表被改名（建表清单漂移）", "internal/db/migrations/0005_totp_recovery.sql",
     "admin_recovery_codes", "admin_recovery_codes_x",
     "./internal/db/", "TestMigrateCreatesExpectedTables", True),

    # --- 采集器（Windows 真机测试；第 8 位限定平台）---
    # 这里刻意**没有**"把 user 断言改回 idle 断言"这条变异：实测（12 核、烧满 0.8 秒）
    # idle Δ ≈ 0.0 ms 但会漏进零星 tick，所以那种变异版**单跑照样绿**，
    # 放进来只会得到一条假的"[变异未变红]"。它的问题不是"没被护栏拦住"，
    # 而是"这个信号本身就不可靠"——处理方式是换掉断言（见该测试的注释），不是加变异。
    ("CPU 计数不再取自 GetSystemTimes（恒返回零值）",
     "internal/agent/collector/collector_windows.go",
     "return winCPUTimes{idle: idle.uint64(), kernel: kern.uint64(), user: user.uint64()}, true",
     "return winCPUTimes{}, true",
     "./internal/agent/collector/", "TestWindowsCPUTimesAdvances"),
    ("空转循环不再累加（负载没造出来，测试自己失效）",
     "internal/agent/collector/collector_windows_test.go",
     "\t\t\t\t\tburn.Add(1)", "\t\t\t\t\tburn.Load()",
     "./internal/agent/collector/", "TestWindowsCPUTimesAdvances", False, "windows"),

    # --- 第 119 项：接通 alert.KindProbeFail（探测连续失败告警）---
    # 这一组几乎全部围绕"计数单位"：probe_fail 的语义是"连续 N **轮**探测失败"，
    # 而 LatestLatency 是持久的、alertTick 比探测间隔密得多，
    # 所以"按轮去重"一旦失效，门槛会在 30 秒内（也就是一轮里）被满足。
    ("probe_fail 按轮去重失效（计数单位退化成扫描次数）", "cmd/probe-master/scheduler.go",
     "if last, ok := s.probeRounds[key]; ok && last == n.LatencyRound {",
     "if last, ok := s.probeRounds[key]; ok && last != n.LatencyRound {",
     "./cmd/probe-master/", "TestProbeFailCountsRoundsNotScans"),
    # 注意：这条变异的测试必须落在 internal/api。
    # 一开始挂在 cmd/probe-master 的 TestProbeFailCountsRoundsNotScans 上，结果**没变红**——
    # 因为那个测试的 reportRound 辅助函数自己照着 applySample 的样子给轮次号加一，
    # 根本没走这条生产代码（典型的"替身比真身宽松"）。变异验证把这一点照出来了，
    # 于是补了 internal/api 的 TestApplySampleLatencyRoundAdvancesOnlyOnANewRound，
    # 断言"带延迟才 +1、不带延迟不动、空切片也不动"。
    ("上报不再标记新轮次（同一轮的失败被反复计数）", "internal/api/agent.go",
     "n.LatencyRound++", "n.LatencyRound += 0",
     "./internal/api/", "TestApplySampleLatencyRoundAdvancesOnlyOnANewRound"),
    ("probe_fail 的 state 键退化成节点级（多目标计数串在一起）", "cmd/probe-master/scheduler.go",
     "\t\tstateKind := fmt.Sprintf(\"%s:%d\", alert.KindProbeFail, l.TargetID)",
     "\t\tstateKind := alert.KindProbeFail",
     "./cmd/probe-master/", "TestProbeFailStreakIsPerTargetNotPerNode"),
    ("权限/策略类错误被算成探测失败", "cmd/probe-master/scheduler.go",
     "case model.ProbeErrICMPNotPermitted, model.ProbeErrBlockedTarget:\n\t\treturn false",
     "case model.ProbeErrICMPNotPermitted, model.ProbeErrBlockedTarget:\n\t\treturn true",
     "./cmd/probe-master/", "TestProbeFailIgnoresPermissionAndPolicyErrors"),
    ("探通的样本被当成失败（恢复通知再也发不出来）", "cmd/probe-master/scheduler.go",
     "func probeFailed(l model.LatencySample) bool {\n\tif l.Error == \"\" {\n\t\treturn false\n\t}",
     "func probeFailed(l model.LatencySample) bool {\n\tif l.Error == \"\" {\n\t\treturn true\n\t}",
     "./cmd/probe-master/", "TestProbeFailRecoversOnceAndResetsStreak"),
    ("probe_fail 忽略规则的停用开关", "cmd/probe-master/scheduler.go",
     "rule, ok := rules[alert.KindProbeFail]\n\tif !ok || !rule.Enabled {",
     "rule, ok := rules[alert.KindProbeFail]\n\tif !ok {",
     "./cmd/probe-master/", "TestProbeFailRuleDisabledProducesNothing"),
    ("恢复时不清零连续计数（恢复后一失败即告警）", "cmd/probe-master/scheduler.go",
     "\t\t\ts.alerts.Clear(n.NodeID, stateKind)\n\t\t\tif st, err := s.repo.GetAlertState(n.NodeID, stateKind); err == nil &&",
     "\t\t\tif st, err := s.repo.GetAlertState(n.NodeID, stateKind); err == nil &&",
     "./cmd/probe-master/", "TestProbeFailRecoversOnceAndResetsStreak"),
    ("探测轮次去重表不再裁剪（随历史目标无限增长）", "cmd/probe-master/scheduler.go",
     "\tfor k := range s.probeRounds {\n\t\tif !live[k] {\n\t\t\tdelete(s.probeRounds, k)\n\t\t}\n\t}",
     "\tfor k := range s.probeRounds {\n\t\tif !live[k] {\n\t\t\t_ = k\n\t\t}\n\t}",
     "./cmd/probe-master/", "TestPruneProbeRoundsDropsStaleEntries"),

    # --- 第 121 项：internal/agent/prober（ICMP 地址类型、错误归类、rtt 统计）---
    ("ICMP 回退路径用错目标地址类型（Windows/FreeBSD 上 WriteTo 恒 invalid argument）",
     "internal/agent/prober/prober.go",
     "dst = &net.IPAddr{IP: target}", "dst = &net.UDPAddr{IP: target}",
     "./internal/agent/prober/", "TestProbeICMPNeverReportsAFalseFailure"),
    ("EPERM 再次被当成未知错误（权限不足被误报为线路故障）",
     "internal/agent/prober/prober.go",
     "case strings.Contains(msg, \"permission\"), strings.Contains(msg, \"not permitted\"):",
     "case strings.Contains(msg, \"permission\"):",
     "./internal/agent/prober/", "TestClassifyErrMapsHostUnreachable"),
    ("no route to host 又被归到 DNS 解析失败（跨平台归类不一致）",
     "internal/agent/prober/prober.go",
     "case strings.Contains(msg, \"no such host\"):\n\t\treturn model.ProbeErrDNSFailed",
     "case strings.Contains(msg, \"no such host\"), strings.Contains(msg, \"no route to host\"):\n\t\treturn model.ProbeErrDNSFailed",
     "./internal/agent/prober/", "TestClassifyErrMapsHostUnreachable"),
    ("首个成功样本的 0 微秒被哨兵吃掉（min > avg）",
     "internal/agent/prober/prober.go",
     "\tif s.ok == 1 || rtt < s.min {", "\tif s.min == 0 || rtt < s.min {",
     "./internal/agent/prober/", "TestRTTStatsKeepsZeroAsFirstSample"),
    # 这条对应"域名拼错却被报成 S4 拦截"那个旧缺陷。它的判据是注入解析替身之后的
    # checkResolved 返回值，所以**不依赖本机 DNS**——放在 DNS 会解析一切的环境里
    # 也照样变红（早先那版断言依赖真实 DNS，在那种环境里会凭空红/凭空绿）。
    ("解析失败又被报成 S4 拦截（域名拼错显示成「目标被拒绝」，且永不告警）",
     "internal/agent/prober/prober.go",
     "\tips, err := lookupIP(host)\n\tif err != nil {\n\t\treturn fmt.Errorf(\"解析目标失败: %w\", err)\n\t}",
     "\tips, err := lookupIP(host)\n\tif err != nil {\n\t\treturn errBlockedTarget\n\t}",
     "./internal/agent/prober/", "TestProbeResolveFailureIsNotReportedAsBlockedTarget"),

    # --- 第 120 项：internal/agent/client（SPKI 指纹容错）---
    ("SPKI 指纹不再容忍换行/空白（终端折行拷进来就起不来）",
     "internal/agent/client/client.go",
     "\t\t\tswitch r {\n\t\t\tcase ' ', '\\t', '\\r', '\\n':\n\t\t\t\treturn -1\n\t\t\t}",
     "\t\t\tswitch r {\n\t\t\tcase '\\x00':\n\t\t\t\treturn -1\n\t\t\t}",
     "./internal/agent/client/", "TestParsePinsNormalizesBase64"),
    ("SPKI 指纹不再容忍无填充 base64（tr -d '=' 的输出被拒）",
     "internal/agent/client/client.go",
     "\t\tif err != nil {\n\t\t\traw, err = base64.RawStdEncoding.DecodeString(s)\n\t\t}",
     "\t\tif err != nil {\n\t\t\traw = nil\n\t\t}",
     "./internal/agent/client/", "TestParsePinsNormalizesBase64"),

    # --- 第 122 项：internal/agent 与 agentconfig（配置热更新真的生效）---
    ("远端没下发网卡清单时也覆盖（Agent 内置默认排除项被抹掉）",
     "internal/agent/agentconfig/agentconfig.go",
     "\tif remote.NICExclude != nil {\n\t\tc.NICExclude = remote.NICExclude\n\t}",
     "\tc.NICExclude = remote.NICExclude",
     "./internal/agent/agentconfig/", "TestApplyServerConfigKeepsLocalNICDefaultsWhenRemoteOmits"),
    ("改配置后不重建采集器（网卡过滤项的改动永远不生效）",
     "internal/agent/agent.go",
     "\td.coll = collector.New(collector.Options{\n\t\tNICInclude:   d.cfg.NICInclude,\n\t\tNICExclude:   d.cfg.NICExclude,\n\t\tDiskPaths:    d.cfg.DiskPaths,\n\t\tAgentVersion: d.version,\n\t})",
     "\t_ = collector.Options{}",
     "./internal/agent/", "TestSyncedConfigIsPersistedAndProberReplaced"),

    # --- internal/config：NIC 清单的三态契约（生产端）---
    ("未配置的网卡清单退化成空切片（= 要求不过滤，抹掉内置默认）",
     "internal/config/version.go",
     "func splitList(s string) []string {\n\tif s == \"\" {\n\t\treturn nil\n\t}",
     "func splitList(s string) []string {\n\tif s == \"\" {\n\t\treturn []string{}\n\t}",
     "./internal/config/", "TestBuildEffectiveEmitsNullForUnsetNICLists"),
    ("splitList 不再把制表符/回车/换行当分隔符（生成幽灵网卡条目）",
     "internal/config/version.go",
     "\t\tif r == ',' || r == ' ' || r == '\\n' || r == '\\r' || r == '\\t' {",
     "\t\tif r == ',' || r == ' ' {",
     "./internal/config/", "TestSplitListKeepsUnsetDistinctFromEmpty"),

    # --- 第 123 项：internal/agent/service 与 cli、cmd/probe-agent ---
    ("未实现平台的服务安装静默成功（留下半安装状态）",
     "internal/agent/service/service.go",
     "func (m *unsupportedManager) Install(binPath, cfgPath, user string) error {\n\treturn fmt.Errorf(\"当前平台的服务管理器 %s 尚未实现：%s\", m.name, m.hint)\n}",
     "func (m *unsupportedManager) Install(binPath, cfgPath, user string) error {\n\treturn nil\n}",
     "./internal/agent/service/", "TestUnsupportedManagerFailsLoudly"),
    ("安装清单改取末行（卸载时删错二进制）", "internal/agent/cli/cli.go",
     "\treturn strings.TrimSpace(lines[0]), nil",
     "\treturn strings.TrimSpace(lines[len(lines)-1]), nil",
     "./internal/agent/cli/", "TestReadManifest"),
    ("注册码不再规范化（前后空白/小写导致核验失败）", "internal/agent/cli/cli.go",
     "\treq := model.EnrollRequest{\n\t\tCode:           strings.ToUpper(strings.TrimSpace(*code)),",
     "\treq := model.EnrollRequest{\n\t\tCode:           *code,",
     "./internal/agent/cli/", "TestInstallHappyPathWritesConfig"),
    ("安装后不采用服务端下发的参数与目标", "internal/agent/cli/cli.go",
     "\tcfg.ApplyServerConfig(resp.Config)\n\n\tif err := agentconfig.Save(cfgPath, cfg); err != nil {",
     "\tif err := agentconfig.Save(cfgPath, cfg); err != nil {",
     "./internal/agent/cli/", "TestInstallHappyPathWritesConfig"),
    ("入口把子命令名也当成参数（argv 偏移错位）", "cmd/probe-agent/main.go",
     "cli.Run(os.Args[1:], version)", "cli.Run(os.Args, version)",
     "./cmd/probe-agent/", "TestMainOnlyDelegatesToCLI"),

    # --- 第 127 项：补三个"代码已实现但没测试"的缺口 -------------------------
    # 公开详情类的三个 handler 各自判一次可见性，所以这两条都开 replace_all：
    # 只改其中一处的变异会被"另外两处仍然正确"掩盖，看不出泄漏。
    ("公开详情不再过滤隐藏节点（public_visible=0 被读到）", "internal/api/public.go",
     "\tif !ok || !rt.PublicVisible || rt.Archived {",
     "\tif !ok || rt.Archived {",
     "./internal/api/", "TestPublicDetailEndpointsHideInvisibleNodes", True),
    ("公开详情不再过滤归档节点", "internal/api/public.go",
     "\tif !ok || !rt.PublicVisible || rt.Archived {",
     "\tif !ok || !rt.PublicVisible {",
     "./internal/api/", "TestPublicDetailEndpointsHideInvisibleNodes", True),
    # 主题目录是唯一把磁盘目录暴露给外部请求的入口。
    # 注意"绕过 fs.FS 直读"这条：io/fs 本身也会拒绝 `..`，所以只删掉 safeJoin 里的
    # 上跳判断**不会**让用例变红（两种写法都读不到）——真正会红的是"换了读取方式"。
    ("切换主题不校验存在性（静默切到不存在的主题）", "internal/theme/theme.go",
     "func (m *Manager) SetActive(name string) error {\n\tif !m.Exists(name) {",
     "func (m *Manager) SetActive(name string) error {\n\tif false {",
     "./internal/theme/", "TestManagerHotSwitchesActiveTheme"),
    ("切换主题不生效（热切换失效，仍读旧主题）", "internal/theme/theme.go",
     "\tm.active = name", "\tm.active = m.active",
     "./internal/theme/", "TestManagerHotSwitchesActiveTheme"),
    ("主题资源绕过 fs.FS 直读磁盘（路径穿越）", "internal/theme/theme.go",
     "\tp, err := safeJoin(rel)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\treturn fs.ReadFile(t.fsys, p)",
     # 保留 `_ = t`：删掉对 t 的引用会让变异体**编译不过**（declared and not used），
     # 而编译失败的 rc 同样非 0 —— 会被误记成"变红"，实际什么行为都没验到
     # （本轮第一次就是这么混过去的，现由脚本的 build failed 检测兜住）。
     "\t_ = t\n\treturn os.ReadFile(filepath.Join(m.dir, name, rel))",
     "./internal/theme/", "TestManagerOpenRefusesToEscapeThemeRoot"),
    # 流量梯级：三类告警里此前唯一没有单测的一路。
    ("流量梯级不看规则是否启用", "cmd/probe-master/scheduler.go",
     "\trule, ok := rules[alert.KindTraffic]\n\tif !ok || !rule.Enabled {",
     "\trule, ok := rules[alert.KindTraffic]\n\tif !ok {",
     "./cmd/probe-master/", "TestTrafficTiersStaysSilentWithoutQuotaRuleOrTracking"),
    ("流量梯级不看配额（未设上限的节点也告警）", "cmd/probe-master/scheduler.go",
     "\tb, ok := s.billings[n.NodeID]\n\tif !ok || b.TrafficQuota <= 0 {",
     "\tb, ok := s.billings[n.NodeID]\n\tif !ok {",
     "./cmd/probe-master/", "TestTrafficTiersStaysSilentWithoutQuotaRuleOrTracking"),
    ("流量梯级越级告警（80% 直接报 100%）", "cmd/probe-master/scheduler.go",
     "\tfor _, lv := range levels {\n\t\tstateKind := fmt.Sprintf(\"traffic:%d\", lv)\n\t\tif pct >= lv {",
     "\tfor _, lv := range levels {\n\t\tstateKind := fmt.Sprintf(\"traffic:%d\", lv)\n\t\tif pct >= 100 {",
     "./cmd/probe-master/", "TestTrafficTiersFireOncePerLevel"),
    ("流量梯级不按口径与倍率折算", "cmd/probe-master/scheduler.go",
     "\trx, tx, tracked := s.tracker.Used(n.NodeID)\n\tif !tracked {\n\t\treturn\n\t}\n\tused := traffic.Billable(rx, tx, b.TrafficMode, b.TrafficMultiplier)",
     "\trx, tx, tracked := s.tracker.Used(n.NodeID)\n\tif !tracked {\n\t\treturn\n\t}\n\tused := rx + tx",
     "./cmd/probe-master/", "TestTrafficTiersHonorModeAndMultiplier"),
    ("告警冷却失效（同一档每次扫描都发）", "internal/alert/engine.go",
     "\tif st != nil && st.State == \"firing\" && now-st.LastSentAt < int64(rule.CooldownS) {",
     "\tif st != nil && st.State == \"firing\" && now-st.LastSentAt < 0 {",
     "./cmd/probe-master/", "TestTrafficTiersFireOncePerLevel"),
    ("新周期不恢复（节点永远显示流量已用完）", "cmd/probe-master/scheduler.go",
     "\t\t} else if st, err := s.repo.GetAlertState(n.NodeID, stateKind); err == nil && st != nil && st.State == \"firing\" {",
     "\t\t} else if st, err := s.repo.GetAlertState(n.NodeID, stateKind); err == nil && st != nil && st.State == \"ok\" {",
     "./cmd/probe-master/", "TestTrafficTiersRecoverOnNewPeriod"),

    # --- 第 128~130 项：Agent 自升级（12.6 客户端侧） ------------------------
    # 查询串被塞进 u.Path 会转义成 %3F，服务端看到的是"名字里带问号的路径"→ 404。
    ("客户端不再拆分查询串（带 ? 的请求全部 404）", "internal/agent/client/client.go",
     "\tu := *c.base\n\tif i := strings.Index(path, \"?\"); i >= 0 {\n\t\tu.RawQuery = path[i+1:]\n\t\tpath = path[:i]\n\t}\n\tu.Path = strings.TrimRight(u.Path, \"/\") + path",
     "\tu := *c.base\n\tu.Path = strings.TrimRight(u.Path, \"/\") + path",
     "./internal/agent/client/", "TestQueryStringReachesServer"),
    # 这条必须挂在"内容哈希比对"那条用例上：挂在 badChecksum 用例上时，
    # 它会在**交叉校验**处就返回，这行删不删都不变红（一度被误判成"护栏是摆设"）。
    ("校验失败后不删半成品（目录里留下坏二进制）", "internal/agent/updater/updater.go",
     "\tif got := hex.EncodeToString(h.Sum(nil)); got != want {\n\t\t_ = os.Remove(tmp)",
     "\tif got := hex.EncodeToString(h.Sum(nil)); got != want {",
     "./internal/agent/updater/", "TestUpgradeRefusesContentHashMismatchAndLeavesNothingBehind"),
    ("缺少校验和时放行（没校验就装）", "internal/agent/updater/updater.go",
     "\tif want == \"\" {", "\tif false {",
     "./internal/agent/updater/", "TestUpgradeRefusesMissingChecksum"),
    ("清单与下发的校验和不再交叉核对（投毒形态）", "internal/agent/updater/updater.go",
     "\tif version == rel.Version && rel.SHA256 != \"\" && want != rel.SHA256 {",
     "\tif false {",
     "./internal/agent/updater/", "TestUpgradeRefusesWhenManifestAndArtifactDisagree"),
    ("已是最新也照升（定时巡检变成每分钟重写二进制）", "internal/agent/updater/updater.go",
     "\tif o.Version == \"\" && rel.UpToDate {", "\tif false {",
     "./internal/agent/updater/", "TestUpgradeNoChangeWhenUpToDate"),
    ("指定版本被 up_to_date 挡住（无法降级/重装）", "internal/agent/updater/updater.go",
     "\tif o.Version == \"\" && rel.UpToDate {", "\tif rel.UpToDate {",
     "./internal/agent/updater/", "TestUpgradeHonorsPinnedVersionOverUpToDate"),
    ("版本信息不完整也当作可用（升级永远静默跳过）", "internal/agent/updater/updater.go",
     "\tif rel.Version == \"\" || rel.SHA256 == \"\" {", "\tif false {",
     "./internal/agent/updater/", "TestCheckRejectsIncompleteRelease"),
    ("回滚不留现场（坏掉的那个被直接删掉）", "internal/agent/updater/updater.go",
     "\tif err := os.Rename(target, broken); err != nil {",
     "\tif err := os.Remove(target); err != nil {",
     "./internal/agent/updater/", "TestRollbackRestoresPreviousBinary"),
    ("升级取件不再校验令牌（任何人都能拉二进制）", "internal/api/agent_upgrade.go",
     "\tnodeID, err := h.Tokens.Authenticate(token, RealIP(r), time.Now())\n\tif err != nil {\n\t\twriteErr(w, http.StatusUnauthorized, model.ErrInvalidToken, \"令牌无效\")\n\t\treturn 0, false\n\t}",
     "\tnodeID, err := h.Tokens.Authenticate(token, RealIP(r), time.Now())\n\tif err != nil {\n\t\treturn 1, true\n\t}",
     "./internal/api/", "TestAgentReleaseLatestRequiresToken"),
    ("platform 不再做穿越校验（拼进路径）", "internal/api/agent_upgrade.go",
     "\tif strings.Contains(platform, \"/\") || strings.Contains(platform, \"..\") || strings.Contains(platform, `\\`) {",
     "\tif false {",
     "./internal/api/", "TestAgentReleaseLatestRejectsBadPlatform", True),
    # 注意：这个变异体必须真的改变语义。第一版写成
    # `strconv.Itoa(x) != strconv.Itoa(y)`——它与 `x != y` 完全等价，
    # 是个空操作，于是"没变红"被误报成"护栏是摆设"。实际上护栏没问题，
    # 是变异体自己没变异。字符串比较要在**整串**上做才与数值序不同
    # （"1.10.0" vs "1.9.0"：第 3 个字符 '1' < '9'，得到相反结果）。
    ("版本号改成字符串比较（1.10 < 1.9）", "internal/api/agent_upgrade.go",
     '\tas := strings.Split(a, ".")',
     '\tif a != b {\n\t\tif a > b {\n\t\t\treturn 1\n\t\t}\n\t\treturn -1\n\t}\n\tas := strings.Split(a, ".")',
     "./internal/api/", "TestCompareVersionIsNumericNotLexical"),
    ("挑版本取最旧而不是最新", "internal/api/agent_upgrade.go",
     "\t\tif best == \"\" || compareVersion(name, best) > 0 {",
     "\t\tif best == \"\" || compareVersion(name, best) < 0 {",
     "./internal/api/", "TestAgentReleaseLatestReportsVersionAndUpToDate"),
    ("版本目录名不再要求纯数字（nightly 会被选中）", "internal/api/agent_upgrade.go",
     "\t\t\tif part[i] < '0' || part[i] > '9' {", "\t\t\tif false {",
     "./internal/api/", "TestIsVersionNameRejectsNonNumeric"),
    ("忽略注入的发布标签（armv7 取错产物）", "internal/dist/dist.go",
     "\tif PlatformTag != \"\" {", "\tif false {",
     "./internal/dist/", "TestCurrentTagFallsBackToRuntime"),
    ("校验和不再要求 64 位十六进制（非哈希行被当成有效）", "internal/dist/dist.go",
     "\t\tif len(hash) != 64 || !isHex(hash) {", "\t\tif false {",
     "./internal/dist/", "TestParseSumsRejectsNonHex"),
    ("Windows 产物不加 .exe（取件永远 404）", "internal/dist/dist.go",
     "\tif osName == \"windows\" {\n\t\tname += \".exe\"\n\t}",
     "\tif false {\n\t\tname += \".exe\"\n\t}",
     "./internal/dist/", "TestArtifactNameMatchesReleaseNaming"),

    # ---- 部署与运维（docs/04）：脚本编码、/dist 扁平别名、发布标签可观测性 ----
    #
    # BOM 剥除：分发的 install.ps1 开头带 U+FEFF 时，官方那一行安装命令
    # （scriptblock 构造 API）会把 BOM 当成标识符的一部分而直接失败。
    # 变异成剥一个不存在的字符（零宽空格），行为等价于"不剥"。
    ("分发的 install.ps1 不再剥 BOM（一行式在线安装失败）", "internal/api/install.go",
     'out = strings.TrimPrefix(out, "\\ufeff")', 'out = strings.TrimPrefix(out, "\\u200b")',
     "./internal/api/", "TestInstallPS1BOMOnDiskButNotOnTheWire"),
    # 扁平别名必须只作用于单段路径：放开后 /dist/<版本>/<文件> 会被改写成
    # /dist/<最新版本>/<版本>/<文件>，把"没有这个文件"变成谜之 404。
    ("/dist 别名也作用于子路径（带版本的路径被改道成谜之 404）", "internal/api/install.go",
     'if rel == "" || strings.Contains(rel, "/") {', 'if rel == "" {',
     "./internal/api/", "TestDistAliasDoesNotApplyToSubPaths"),
    # 根目录显式文件必须优先：它代表运维手工钉住的那一份。
    ("/dist 别名覆盖根目录的显式文件（运维钉住的版本被换掉）", "internal/api/install.go",
     'if st, err := os.Stat(full); err != nil || st.IsDir() {', 'if true {',
     "./internal/api/", "TestDistExplicitFlatFileWinsOverAlias"),
    # 别名必须落到**最新**版本上：落错版本会让新装节点与升级节点拿到不同版本。
    ("/dist 扁平别名固定落到某个版本（新旧节点版本错位）", "internal/api/install.go",
     'version, err := latestDistVersion(h.DistDir)', 'version, err := "1.0.0", error(nil)',
     "./internal/api/", "TestDistFlatPathResolvesToNewestVersion"),
    # 发布标签是"手上的二进制是不是发布构建"的唯一可观测点，
    # 打错等于把这个观测点变成噪音。
    ("version 打印的发布标签写死（运维看不出标签有没有注入）", "internal/agent/cli/cli.go",
     '"(发布标签 "+dist.CurrentTag()+")")', '"(发布标签 "+"nope"+")")',
     "./internal/agent/cli/", "TestRunVersionVariants"),
    # --- 配置严格解析：部署脚本曾写出一个不存在的 install: 段 ---
    # 非严格解析会把它整段吞掉（配置看着写好了，实际毫无作用）。
    # 退回宽松模式后，"未知键必须报错"的守卫必须立刻变红。
    ("配置文件不再拒绝未知键（静默吞掉 install: 段）", "internal/config/master.go",
     "dec.KnownFields(true)", "dec.KnownFields(false)",
     "./internal/config/", "TestLoadMasterRejectsUnknownKey"),
    # 光"报错"不够：还得让运维一眼看出是哪个键写错了。
    ("解析失败的报错不点名是哪个键", "internal/config/master.go",
     'return nil, fmt.Errorf("解析配置失败: %w", err)', 'return nil, fmt.Errorf("解析配置失败")',
     "./internal/config/", "TestLoadMasterRejectsUnknownKey"),
    # 部署脚本必须只写 MasterConfig 认识的顶层键。这里把已经修掉的
    # `install:` 段塞回去——install.* 是数据库设置，写进配置只会静默失效。
    ("部署脚本塞回不存在的 install: 段（Linux）", "scripts/deploy-master.sh",
     "echo 'tls:'", "echo \"install:\"\n\t\techo 'tls:'",
     "./internal/config/", "TestDeployScriptsOnlyWriteKnownConfigKeys"),
    ("部署脚本塞回不存在的 install: 段（Windows）", "scripts/deploy-master.ps1",
     '[void]$lines.Add("tls:")', '[void]$lines.Add("install:")\n    [void]$lines.Add("tls:")',
     "./internal/config/", "TestDeployScriptsOnlyWriteKnownConfigKeys"),
    # --- 站点形态：公开看板与管理后台是"两个域名"还是"一个站点" ---
    # 门控必须**按路径**判域名。同一个入口上 /admin/ 与 / 属于不同域名，
    # 这是"一个端口 + 两个域名 + 反代"的常态；只看入口等于没有分离。
    ("域名门控不再按路径区分管理端与公开站", "internal/api/origin.go",
     "\t\tcase !adminEntry && isAdminPath(r.URL.Path):\n", "\t\tcase false:\n",
     "./internal/api/", "TestSiteGateSplitIsPathAware"),
    # 默认形态（auto）下"只配了公开域名、没配管理域名"必须不拦管理路径：
    # 这是升级前的行为，拦上就等于所有存量实例的后台集体打不开。
    ("auto 且未配管理域名时也去拦管理路径（存量部署后台打不开）", "internal/api/origin.go",
     "\t\t\t// 否则存量部署会突然打不开后台。\n\t\t\tallowed = adm",
     "\t\t\t// 否则存量部署会突然打不开后台。\n\t\t\tallowed = []string{\"__none__\"}",
     "./internal/api/", "TestSiteGateAutoKeepsLegacyBehaviourWithoutAdminHosts"),
    # 反过来：明确选了分离却没给管理域名时，必须**拒绝**而不是悄悄放行——
    # 放行意味着"公开域名也能打开后台"，正是选择分离要避免的事。
    ("分离形态缺管理域名时放行管理路径（公开域名能打开后台）", "internal/api/origin.go",
     "case !adminEntry && isAdminPath(r.URL.Path) && len(adm) == 0 && layout == siteLayoutSplit:",
     "case !adminEntry && isAdminPath(r.URL.Path) && false:",
     "./internal/api/", "TestSiteGateSplitWithoutAdminHostsRefusesAdminPath"),
    # 设置项为空 = "没在后台配过" → 回落到 master.yaml，而不是"配成空清单"。
    # 两者混起来会把整站锁死：空清单谁也不匹配，公开域名与管理域名同时失效。
    ("空设置项不再回落到配置文件（白名单形同虚设）", "internal/api/origin.go",
     'if v := strings.TrimSpace(h.settingOr("site.public_hosts", "")); v != "" {',
     'if v := strings.TrimSpace(h.settingOr("site.public_hosts", "")); true {',
     "./internal/api/", "TestSiteGateEmptySettingFallsBackToConfig"),
    # 同站形态必须真的"同站"：留着"公开域名打不开 /admin/"这条规则，
    # 就是选择同站的人想摆脱的东西。
    ("同站形态不生效（同一域名下仍打不开 /admin/）", "internal/api/origin.go",
     "case layout == siteLayoutSingle:", "case false:",
     "./internal/api/", "TestSiteGateSingleTreatsHostsAsOneSite"),
    # 门控装配在进程启动时完成，而站点形态是**运行时**设置。若把清单与形态
    # 读进闭包变量，"改完即时生效"就是假的：界面显示已分离、实际仍在放行，
    # 且没有任何报错。这正是端到端冒烟第一次跑出来的形态。
    ("门控在装配时把清单冻结成闭包变量（设置改了不生效）", "internal/api/origin.go",
     "\treturn http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {\n\t\tpub, adm := h.effectiveHosts()\n\t\tlayout := h.SiteLayoutMode()\n",
     "\tpub, adm := h.effectiveHosts()\n\tlayout := h.SiteLayoutMode()\n\treturn http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {\n",
     "./internal/api/", "TestSiteGateReadsSettingsPerRequest"),
    # ACME http-01 挑战必须豁免域名门控：CA 是按 IP 访问的，Host 永远是 IP
    # 字面量。这条豁免曾在门控重写时丢掉过，症状是几周后"整站证书过期"，
    # 与域名白名单看上去毫无关系。
    ("域名门控不再豁免 ACME 挑战（证书续期必然失败）", "internal/api/origin.go",
     "if strings.HasPrefix(r.URL.Path, acme.HTTP01Prefix) {",
     'if strings.HasPrefix(r.URL.Path, acme.HTTP01Prefix+"\\x00") {',
     "./internal/api/", "TestSiteGateExemptsACMEChallenge"),
    # 防自锁校验：选分离却没凑齐两个域名时必须拒绝保存。
    ("保存时不再校验分离形态的域名完整性（可把自己锁在后台外）", "internal/api/origin.go",
     "if layout != siteLayoutSplit {", 'if layout != "__never__" {',
     "./internal/api/", "TestValidateSiteLayoutRejectsSplitWithoutBothHosts"),
    # 管理路径的识别必须精确：模糊判据会把 /administrator、/admin-old 也算进来，
    # 于是公开站上任何名字里带 admin 的路径都会被要求管理域名。
    ("管理路径用模糊判据识别（/administrator 被误判为管理端）", "internal/api/origin.go",
     'return p == "/admin" || strings.HasPrefix(p, "/admin/") || strings.HasPrefix(p, "/api/admin/")',
     'return strings.Contains(p, "admin")',
     "./internal/api/", "TestIsAdminPathIsExact"),
    # 清单的分隔符要按最宽的形式认：这类清单是人工粘贴来的，
    # 只认逗号会让"粘进来带一个换行"整条被拒。
    ("域名清单只认逗号（粘贴来的换行让整条被拒）", "internal/api/origin.go",
     "case ',', ';', ' ', '\\t', '\\r', '\\n':", "case ',':",
     "./internal/api/", "TestValidateHostList"),
    # 后台的域名校验与配置文件那条必须共用同一套判定，否则"配置文件里能写、
    # 后台存不进去"。这里把校验反过来，合法条目反而被拒。
    ("域名清单校验失效（非法条目照收）", "internal/api/settings.go",
     "if err := config.CheckHostWhitelistEntry(item); err != nil {",
     "if err := config.CheckHostWhitelistEntry(item); err == nil {",
     "./internal/api/", "TestValidateHostList"),
    # 配置文件那一侧的同一道校验：删掉它就只剩"后台校验"一条腿，
    # 而 master.yaml 正是更早被读到的那一份。
    ("配置文件不再校验 host 白名单条目", "internal/config/master.go",
     "if err := CheckHostWhitelistEntry(h); err != nil {", "if err := error(nil); err != nil {",
     "./internal/config/", "TestTLSModeValidation"),
    # 默认形态必须是 auto：拿 single/split 当默认，就是用新默认值重新解释
    # 存量部署的既有配置，属于静默的行为变更。
    ("站点形态默认值改成分离（存量部署被静默改配置）", "internal/api/settings.go",
     '"site.layout": "auto",', '"site.layout": "split",',
     "./internal/api/", "TestSiteGateAutoKeepsLegacyBehaviourWithoutAdminHosts"),
    # 后台下拉框必须给出服务端接受的**全部**取值，少一个就是"某个合法形态
    # 在界面上根本选不出来"，管理员会以为它不存在。
    ("站点形态下拉框少一个取值", "internal/api/adminui/admin.js",
     ",single=同站（一个域名）,split=分离（两个域名）'", ",single=同站（一个域名）'",
     "./internal/api/", "TestSiteLayoutOptionsMatchBackendValues"),
    # 漏了中文标签，设置页会把站点形态这项的英文键名直接当标题渲染出来。
    ("站点形态设置项没有中文标签", "internal/api/adminui/admin.js",
     "'site.layout': ['站点形态'", "'site.layout': ['site.layout'",
     "./internal/theme/", "TestAdminSiteLayoutSettingsHaveChineseLabels"),
]


def run(go_pkg, test_name):
    env = dict(os.environ)
    env["PATH"] = GOBIN + os.pathsep + env.get("PATH", "")
    p = subprocess.run(
        ["go", "test", "-count=1", "-run", "^" + test_name + "$", go_pkg],
        cwd=ROOT, env=env, capture_output=True, text=True, encoding="utf-8", errors="replace",
    )
    return p.returncode, (p.stdout or "") + (p.stderr or "")


def preflight():
    """开工前自检：所有锚点必须命中，否则拒绝启动。

    为什么需要这一步：本脚本每轮都从**开工快照**取原文再写回，所以一旦磁盘上
    已经有上一轮遗留的变异（例如上一次运行被超时/SIGKILL 打断、还原钩子没跑到），
    这份"脏内容"就会被当成基线继承下来，最后还会被写回去——表现为
    "基线就红"和"变异未变红"混在一起，而真正的原因在快照那一刻就已经注定了。
    实测踩过一次：一个被超时打断的运行把 channel.go 的一行变异留在磁盘上，
    下一轮全量跑里三个 parse_mode 用例全部报"基线就红"。
    """
    bad = []
    for desc, rel, old, new in [m[:4] for m in MUTANTS]:
        src = SNAPSHOT[rel]
        if old not in src:
            hint = "（该文件已包含变异后的内容 → 上一轮残留未还原）" if new in src else "（锚点已失效，需更新）"
            bad.append("%-40s %s %s" % (desc, rel, hint))
    if bad:
        print("开工前自检未通过，拒绝启动——先修好下面这些再跑：")
        for b in bad:
            print("  ! " + b)
        return False
    print("开工前自检通过：%d 条变异的锚点全部命中，工作区无残留。" % len(MUTANTS))
    return True


def main():
    # 可选过滤：`python .tmpcheck/mutate.py internal/alert` 只跑命中的那几条，
    # 迭代时不必为全部变异付编译时间（不带参数则全跑）。
    # 支持多个过滤词（逗号分隔），任一命中即跑：
    #   python .tmpcheck/mutate.py 半成品,"install: 段"
    # 只按**逗号**切分，不按空白：过滤词里常有空格（如 "install: 段"），
    # 按空白切会把一个词拆成两半，误命中无关的变异。
    filters = []
    for a in sys.argv[1:]:
        filters += [x.strip() for x in a.split(",") if x.strip()]

    snapshot_all()
    atexit.register(restore_all)
    signal.signal(signal.SIGINT, _on_signal)
    signal.signal(signal.SIGTERM, _on_signal)

    if not preflight():
        return 2

    bad = []
    total = 0
    for i, item in enumerate(MUTANTS, 1):
        desc, rel, old, new, pkg, test_name = item[:6]
        if filters and not any(f in rel or f in desc for f in filters):
            continue
        # 可选第 8 位＝限定平台。带 `//go:build windows` 的测试文件在别的平台上
        # 是"没有可运行的测试"：基线绿、变异也绿，会被误报成"[变异未变红]"。
        # 所以平台不符时直接跳过并说明原因，而不是留一条永远"通过"的记录。
        only = item[7] if len(item) > 7 else ""
        if only and only != HOST_PLATFORM:
            print("[跳过] %02d %s —— 仅 %s 平台有效（当前 %s）" % (i, desc, only, HOST_PLATFORM))
            continue
        total += 1
        replace_all = len(item) > 6 and item[6]
        path = os.path.join(ROOT, rel)
        # 每轮都从**快照**取原文，而不是从磁盘：万一上一轮的恢复没成功，
        # 这一轮也不会把坏内容当成"基线"，问题会立刻暴露而不是被悄悄继承。
        src = SNAPSHOT[rel]
        with io.open(path, "w", encoding="utf-8", newline="") as f:
            f.write(src)

        # 锚点必须唯一命中，否则"变异"可能打偏（不变量 20 的坑 ③）。
        n = src.count(old)
        if (not replace_all and n != 1) or n == 0:
            print("[锚点异常] %02d %s —— 在 %s 命中 %d 次（应为 %s）"
                  % (i, desc, rel, n, ">=1" if replace_all else "1"))
            bad.append(desc)
            continue

        # 先确认未变异时是绿的（否则"变红"毫无意义）。
        base_rc, base_out = run(pkg, test_name)
        if base_rc != 0:
            print("[基线就红] %02d %s\n%s" % (i, desc, base_out[-1500:]))
            bad.append(desc)
            continue

        try:
            with io.open(path, "w", encoding="utf-8", newline="") as f:
                f.write(src.replace(old, new) if replace_all else src.replace(old, new, 1))
            rc, out = run(pkg, test_name)
        finally:
            with io.open(path, "w", encoding="utf-8", newline="") as f:
                f.write(src)

        if rc == 0:
            print("[变异未变红] %02d %s —— %s 仍通过：护栏是摆设" % (i, desc, test_name))
            bad.append(desc)
        else:
            # 变异必须**能编译**才算真的验到了行为。编译失败的 rc 同样非 0，
            # 会被误记成"变红"，而它其实什么都没验——本轮第 101 条（主题资源
            # 绕过 fs.FS）第一次就留下了一个空的失败原因行，查下去才发现是
            # `declared and not used`。这类"假红"比"未变红"更隐蔽，因为它看起来是通过的。
            if "build failed" in out or "declared and not used" in out:
                print("[无效变异] %02d %s —— 变异后编译不过，这条没验到任何行为" % (i, desc))
                bad.append("无效变异（编译不过）：" + desc)
                continue
            first = ""
            for line in out.splitlines():
                if line.startswith("    ") and (".go:" in line or ".js" in line or ".css" in line or ".html" in line):
                    first = line.strip()
                    break
            if not first:
                for line in out.splitlines():
                    if line.startswith("panic:") or "index out of range" in line:
                        first = line.strip()
                        break
            print("[OK] %02d %s → %s 变红：%s" % (i, desc, test_name, first[:130]))

    # 收尾自检：磁盘内容必须与快照逐字节一致（"变异没被还原"的护栏）。
    restore_all()
    for rel, content in SNAPSHOT.items():
        with io.open(os.path.join(ROOT, rel), encoding="utf-8") as f:
            if f.read() != content:
                print("[未还原] %s 与快照不一致" % rel)
                bad.append("未还原：" + rel)

    print("\n共 %d 条变异，%d 条未通过" % (total, len(bad)))
    for b in bad:
        print("  ! " + b)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
