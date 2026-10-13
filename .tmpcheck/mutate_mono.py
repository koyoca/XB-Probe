# -*- coding: utf-8 -*-
# 变异验证：抹掉被护栏保护的行 → 对应测试必须 FAIL。找不到锚点要报错，不许静默跳过。
import subprocess, sys, os
os.environ['PATH'] = r'C:/Users/msecth/.workbuddy/binaries/go/go1.27.2/go/bin' + os.pathsep + os.environ['PATH']

CSS = 'internal/theme/builtin/style.css'
JS  = 'internal/theme/builtin/app.js'
orig = {p: open(p, encoding='utf-8').read() for p in (CSS, JS)}

MUT = [
  ('遗留彩色回归', CSS, '--online: #139948;', '--online: #16a34a;', 'TestPublicBoardIsMonochrome'),
  ('离线点改红', CSS, '.dot.offline { background: var(--offline); }', '.dot.offline { background: #dc2626; }', 'TestDotOnlineIsTheOnlyColor'),
  ('进度条阈值变色回归', CSS, '/* 填充 = 前景墨色', '.track i.warn { background: #d97706; }\n/* 填充 = 前景墨色', 'TestProgressFillIsForeground'),
  # media 块里的行没有行尾注释，锚点才打得准（[data-theme] 块的行带 oklch 注释）
  ('暗色块失同步', CSS, '    --warn: #bababa;\n    --bad: #eeeeee;', '    --warn: #bababa;\n    --bad: #f87171;', 'TestDarkBlocksStayInSync'),
  ('卡片阴影回归', CSS, '  padding: 12px 14px;\n  cursor: pointer;', '  padding: 12px 14px;\n  box-shadow: 0 1px 3px rgba(0,0,0,.1);\n  cursor: pointer;', 'TestCardsHaveNoShadow'),
  ('页签选中样式回退', CSS, '.tab[aria-selected="true"] {\n  background: var(--fg);', '.tab[aria-selected="true"] {\n  background: var(--card);', 'TestActiveTabIsInverted'),
  ('调色板退回彩色数组', JS, "var PALETTE = ['--c1', '--c2', '--c3', '--c4', '--c5', '--c2'];", "var PALETTE = ['#2563eb', '#16a34a', '#7a7a7a', '#9e9e9e', '#c4c4c4', '#555'];", 'TestGreyscaleChartPalette'),
  ('切换后不重画', JS, '      tick();\n    });\n  })();', '      /* tick removed */\n    });\n  })();', 'TestGreyscaleChartPalette'),
  ('去掉虚线表', JS, 'var PALETTE_DASH = [null, [6, 3], [2, 3], [10, 4, 2, 4], [1, 4], [8, 3]];', 'var PALETTE_DASH = [null, null, null, null, null, null];', 'TestGreyscaleChartPalette'),
]

fails = 0
for name, path, old, new, test in MUT:
    src = open(path, encoding='utf-8').read()
    if old not in src:
        print('[锚点缺失] %s' % name); fails += 1; continue
    open(path, 'w', encoding='utf-8', newline='').write(src.replace(old, new, 1))
    r = subprocess.run(['go', 'test', './internal/theme/', '-run', test, '-count=1'],
                       capture_output=True, text=True)
    ok = r.returncode != 0
    print(('PASS(变红) ' if ok else 'FAIL(假通过!) ') + name)
    if not ok: fails += 1
    open(path, 'w', encoding='utf-8', newline='').write(orig[path])

for p in (CSS, JS):
    assert open(p, encoding='utf-8').read() == orig[p]
print('还原完成，未通过变异的护栏数:', fails)
sys.exit(1 if fails else 0)
