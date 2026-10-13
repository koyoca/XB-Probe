// 内置主题前端：只通过公开 API 取数，无权访问管理 API（5.3）。
//
// 安全要点（13.4.4）：一律用 textContent 渲染，禁止 innerHTML 拼接不可信字符串。
// CSP 为 script-src 'self'，因此不加载任何外部库，图表用手绘 Canvas。
//
// 布局稳定性（本次重写的重点）：
//   节点卡片每 3 秒被刷新一次。如果每次刷新都清空容器重建 DOM，会出现两个问题：
//     1) 顺序变化 -> 卡片互相穿插（服务端此前遍历 map，顺序本就随机）；
//     2) 元素被销毁重建 -> 悬停态丢失、轻微闪烁、滚动位置跳动。
//   因此这里改用「按键复用」：以 public_id 为键保留元素，只就地更新文本/宽度；
//   顺序完全采用服务端返回的顺序（服务端已按 sort_order, node_id 稳定排序），
//   前端绝不做二次排序，更不会按状态排序。
(function () {
  'use strict';

  var API = '/api/public/v1';
  var POLL_MS = 3000;
  var SPARK_POINTS = 48;

  var current = null;      // 当前详情页的 public_id
  var detailNode = null;   // 当前详情页的节点 DTO（图表副标题要用到总量等字段）
  var detailTab = 'resources'; // 详情页当前标签：resources / latency
  var filter = '\u0000all'; // 当前分组筛选（见 FILTER_ALL / FILTER_NONE）
  var cards = {};          // public_id -> { root, refs }
  var tabSig = '';         // 顶部分组标签的签名，避免每次刷新都重建（会丢焦点）
  var spark = [];          // 聚合下行带宽的最近若干点

  // cardAllow 是 settings.public.cards 下发的"允许展示的卡片键"集合。
  // null 表示不限制（后端没有配、或配了空值），此时全部卡片都显示。
  // 与 public.fields 的区别：那个控制"数据发不发"，这个只控制"卡片显不显示"。
  var cardAllow = null;
  var overviewChrome = true; // 当前是否处于总览视图（决定 .kpis 该不该出现）

  // -------------------------------------------------------------------------
  // 工具
  // -------------------------------------------------------------------------

  function $(id) { return document.getElementById(id); }

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  }

  // setText 只在内容真的变化时写 DOM，减少无谓的重排与闪烁。
  function setText(node, text) {
    if (!node) return;
    var s = String(text);
    if (node.textContent !== s) node.textContent = s;
  }

  // setWidth 设置进度条宽度。
  //
  // 入参是本项目的"百分比 ×100"刻度（0~10000，见 model.PublicNode.CPUPct 的注释），
  // 必须在这里除以 100 才是 CSS 的百分比。曾经漏了这一步，直接 clamp 到 100，
  // 于是"任何 ≥1% 的占用"都被画成满格——所有进度条永远是 100%，
  // 而旁边的数字却写着 12.8%，看起来像进度条坏了。
  // 之所以不在每个调用点除以 100：levelOf() 用的也是同一套刻度（7500/9000），
  // 把换算集中在一处，调用方就只需要传原始值，不会再漏。
  function setWidth(node, percent) {
    if (!node) return;
    var p = Math.max(0, Math.min(10000, Number(percent) || 0)) / 100;
    var s = p.toFixed(1) + '%';
    if (node.style.width !== s) node.style.width = s;
  }

  function setClass(node, cls) {
    if (node && node.className !== cls) node.className = cls;
  }

  function fmtBps(v) {
    if (v === undefined || v === null) return '—';
    if (!v) return '0 B/s';
    var u = ['B/s', 'KB/s', 'MB/s', 'GB/s'], i = 0;
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return v.toFixed(i ? 1 : 0) + ' ' + u[i];
  }

  function fmtBytes(v) {
    if (v === undefined || v === null) return '—';
    if (!v) return '0 B';
    var u = ['B', 'KB', 'MB', 'GB', 'TB'], i = 0;
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    // 小数值保留两位（1.00 TB / 2.45 GB），大数值取整（426 GB / 948 GB）
    return v.toFixed(v < 100 ? 2 : 0) + ' ' + u[i];
  }

  function pct(v, decimals) {
    if (v === undefined || v === null) return '—';
    return (v / 100).toFixed(decimals === undefined ? 0 : decimals) + '%';
  }

  // fmtMoney 把"最小货币单位"整数格式化为人类可读金额。
  // 默认 ¥（CNY），传入 currency 时优先用其符号。
  function fmtMoney(minor, currency) {
    if (minor === undefined || minor === null) return '—';
    var sym = { CNY: '¥', USD: '$', EUR: '€', HKD: 'HK$', JPY: '¥' }[currency] || '¥';
    var major = Number(minor) / 100;
    return sym + major.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  }

  function dur(sec) {
    if (sec === undefined || sec === null || sec < 0) return '—';
    var d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600),
        m = Math.floor((sec % 3600) / 60);
    if (d > 0) return d + '天' + h + '小时';
    if (h > 0) return h + '小时' + m + '分';
    return m + '分';
  }

  function levelOf(percent) {
    if (percent === undefined || percent === null) return '';
    if (percent >= 9000) return 'bad';
    if (percent >= 7500) return 'warn';
    return '';
  }

  // statusText 返回状态胶囊文案（在线显示在线时长，离线显示离线时长）。
  function statusText(n, nowSec) {
    switch (n.status) {
      case 'online':
        return '在线 ' + dur(n.uptime_s);
      case 'warning':
        return '疑似离线';
      case 'archived':
        return '已归档';
      default:
        if (!n.updated_at) return '离线';
        return '离线 ' + dur(Math.max(0, nowSec - n.updated_at));
    }
  }

  // expireText 返回到期文案与告警级别。
  function expireText(days) {
    if (days === undefined || days === null) return { text: '永久', level: '' };
    if (days < 0) return { text: '已过期' + (-days) + '天', level: 'bad' };
    if (days === 0) return { text: '今天到期', level: 'bad' };
    if (days <= 3) return { text: days + '天后到期', level: 'bad' };
    if (days <= 7) return { text: days + '天后到期', level: 'warn' };
    return { text: days + '天后到期', level: '' };
  }

  // -- 国旗：FLAG-COMMON-BEGIN -- 以下区块在两个前端里必须逐字节一致（internal/theme/flag_test.go 钉住）
  // 国旗标识：ISO 3166-1 alpha-2 → [名称, 图形描述]。
  //
  // 为什么自己画，而不是用外链图片或 emoji：
  //   1. CSP 恒为 default-src 'self'，外链图片与 data: URI 都会被浏览器直接拒绝；
  //   2. emoji 国旗（如 U+1F1ED U+1F1F0）在 Windows 上根本不渲染成旗帜，
  //      只会显示成两个字母方块——而看板的使用者很可能就在 Windows 上；
  //   3. 这里画的是**内联 SVG**，由本文件生成：没有网络请求、没有 style 属性、
  //      没有 script，因此完全不受 CSP 影响，也符合"前端零外部依赖"的约定。
  //
  // 图形描述是一门极小的 DSL，只为把几十面旗压进一张表：
  //   'h:c1,c2,...'  横向等分色带
  //   'v:c1,c2,...'  纵向等分色带
  //   'x:底色,十字色,竖条中心比例'  北欧十字
  //   '<svg 片段>'   其余手绘（复杂图形，无前缀）
  // 表里没有的代码不画旗帜——宁可没有标识，也不要画一面错旗。
  function starPts(cx, cy, r) {
    var p = [];
    for (var i = 0; i < 10; i++) {
      var rr = i % 2 ? r * 0.382 : r;
      var a = -Math.PI / 2 + i * Math.PI / 5;
      p.push((cx + rr * Math.cos(a)).toFixed(2) + ',' + (cy + rr * Math.sin(a)).toFixed(2));
    }
    return p.join(' ');
  }
  var FLAGS = {
    CN: ['中国', '<rect width="20" height="14" fill="#DE2910"/><polygon points="' + starPts(4.4, 4.2, 2.9) + '" fill="#FFDE00"/><circle cx="9.3" cy="1.9" r=".8" fill="#FFDE00"/><circle cx="11.5" cy="3.6" r=".8" fill="#FFDE00"/><circle cx="11.5" cy="6.4" r=".8" fill="#FFDE00"/><circle cx="9.3" cy="8.1" r=".8" fill="#FFDE00"/>'],
    HK: ['中国香港', '<rect width="20" height="14" fill="#DE2910"/><circle cx="10" cy="7" r="3.7" fill="#FFFFFF"/><circle cx="10" cy="4.4" r="1.05" fill="#DE2910"/><circle cx="12.5" cy="6.2" r="1.05" fill="#DE2910"/><circle cx="11.6" cy="9.2" r="1.05" fill="#DE2910"/><circle cx="8.4" cy="9.2" r="1.05" fill="#DE2910"/><circle cx="7.5" cy="6.2" r="1.05" fill="#DE2910"/>'],
    TW: ['中国台湾', '<rect width="20" height="14" fill="#FE0000"/><rect width="10" height="7" fill="#000095"/><circle cx="5" cy="3.5" r="2.5" fill="#FFFFFF"/><circle cx="5" cy="3.5" r="1.75" fill="#000095"/><circle cx="5" cy="3.5" r="1.25" fill="#FFFFFF"/>'],
    MO: ['中国澳门', '<rect width="20" height="14" fill="#00785E"/><circle cx="10" cy="6.5" r="2.9" fill="#FFFFFF"/><circle cx="10" cy="3.5" r="1" fill="#FFFFFF"/><circle cx="12.5" cy="5.4" r="1" fill="#FFFFFF"/><circle cx="11.6" cy="8.3" r="1" fill="#FFFFFF"/><circle cx="8.4" cy="8.3" r="1" fill="#FFFFFF"/><circle cx="7.5" cy="5.4" r="1" fill="#FFFFFF"/>'],
    JP: ['日本', '<rect width="20" height="14" fill="#FFFFFF"/><circle cx="10" cy="7" r="4" fill="#BC002D"/>'],
    KR: ['韩国', '<rect width="20" height="14" fill="#FFFFFF"/><circle cx="10" cy="7" r="3.6" fill="#CD2E3A"/><path d="M6.4 7a3.6 3.6 0 0 0 7.2 0z" fill="#0047A0"/>'],
    SG: ['新加坡', '<rect width="20" height="7" fill="#EE2536"/><rect y="7" width="20" height="7" fill="#FFFFFF"/><circle cx="4.8" cy="3.5" r="2.6" fill="#FFFFFF"/><circle cx="6.1" cy="3.5" r="2.2" fill="#EE2536"/><circle cx="7.4" cy="1.9" r=".6" fill="#FFFFFF"/><circle cx="8.8" cy="3.2" r=".6" fill="#FFFFFF"/><circle cx="8.1" cy="4.9" r=".6" fill="#FFFFFF"/>'],
    MY: ['马来西亚', '<rect width="20" height="14" fill="#FFFFFF"/><g fill="#CC0001"><rect width="20" height="1.08"/><rect y="2.15" width="20" height="1.08"/><rect y="4.31" width="20" height="1.08"/><rect y="6.46" width="20" height="1.08"/><rect y="8.62" width="20" height="1.08"/><rect y="10.77" width="20" height="1.08"/><rect y="12.92" width="20" height="1.08"/></g><rect width="10" height="7" fill="#010066"/><circle cx="4.7" cy="3.5" r="2.2" fill="#FFCC00"/><circle cx="5.9" cy="3.5" r="1.8" fill="#010066"/><polygon points="' + starPts(8.1, 3.5, 1.7) + '" fill="#FFCC00"/>'],
    TH: ['泰国', 'h:#A51931,#F4F5F8,#2D2A4A'],
    VN: ['越南', '<rect width="20" height="14" fill="#DA251D"/><polygon points="' + starPts(10, 7, 3.7) + '" fill="#FFFF00"/>'],
    PH: ['菲律宾', '<rect width="20" height="7" fill="#0038A8"/><rect y="7" width="20" height="7" fill="#CE1126"/><path d="M0 0L8.4 7 0 14z" fill="#FFFFFF"/><circle cx="2.7" cy="7" r="1.2" fill="#FCD116"/>'],
    ID: ['印度尼西亚', 'h:#CE1126,#FFFFFF'],
    IN: ['印度', '<rect width="20" height="4.67" fill="#FF9933"/><rect y="4.67" width="20" height="4.67" fill="#FFFFFF"/><rect y="9.33" width="20" height="4.67" fill="#138808"/><circle cx="10" cy="7" r="1.9" fill="none" stroke="#000080" stroke-width=".7"/>'],
    MN: ['蒙古', 'v:#C4272F,#015197,#F9CF02'],
    AU: ['澳大利亚', '<rect width="20" height="14" fill="#00247D"/><path d="M0 0L9 7M9 0L0 7" stroke="#FFFFFF" stroke-width="1.2"/><rect x="3.8" width="1.4" height="7" fill="#FFFFFF"/><rect y="2.8" width="9" height="1.4" fill="#FFFFFF"/><rect x="4.05" width=".9" height="7" fill="#C8102E"/><rect y="3.05" width="9" height=".9" fill="#C8102E"/><circle cx="14.6" cy="4" r="1" fill="#FFFFFF"/><circle cx="16.6" cy="9.2" r="1" fill="#FFFFFF"/><circle cx="12.6" cy="10.2" r=".8" fill="#FFFFFF"/><circle cx="15" cy="6.6" r=".6" fill="#FFFFFF"/>'],
    NZ: ['新西兰', '<rect width="20" height="14" fill="#00247D"/><path d="M0 0L9 7M9 0L0 7" stroke="#FFFFFF" stroke-width="1.2"/><rect x="3.8" width="1.4" height="7" fill="#FFFFFF"/><rect y="2.8" width="9" height="1.4" fill="#FFFFFF"/><rect x="4.05" width=".9" height="7" fill="#C8102E"/><rect y="3.05" width="9" height=".9" fill="#C8102E"/><polygon points="' + starPts(15.4, 8.6, 1.1) + '" fill="#FFFFFF"/><polygon points="' + starPts(16.6, 4.2, .9) + '" fill="#C8102E"/><circle cx="12.4" cy="10.6" r=".7" fill="#FFFFFF"/>'],
    US: ['美国', '<rect width="20" height="14" fill="#FFFFFF"/><g fill="#B22234"><rect width="20" height="1.08"/><rect y="2.15" width="20" height="1.08"/><rect y="4.31" width="20" height="1.08"/><rect y="6.46" width="20" height="1.08"/><rect y="8.62" width="20" height="1.08"/><rect y="10.77" width="20" height="1.08"/><rect y="12.92" width="20" height="1.08"/></g><rect width="8.8" height="7.6" fill="#3C3B6E"/><g fill="#FFFFFF"><circle cx="2" cy="1.8" r=".55"/><circle cx="5" cy="1.8" r=".55"/><circle cx="3.5" cy="3.3" r=".55"/><circle cx="6.5" cy="3.3" r=".55"/><circle cx="2" cy="4.8" r=".55"/><circle cx="5" cy="4.8" r=".55"/><circle cx="7.6" cy="5.8" r=".55"/><circle cx="1.4" cy="6.3" r=".55"/></g>'],
    CA: ['加拿大', '<rect width="20" height="14" fill="#FFFFFF"/><rect width="5" height="14" fill="#D80621"/><rect x="15" width="5" height="14" fill="#D80621"/><path d="M10 2.4l.9 2 1.9-.8-.8 2.1 1.7 1.2-2.1.5.3 2.1-1.9-1.2-1.9 1.2.3-2.1-2.1-.5 1.7-1.2-.8-2.1 1.9.8z" fill="#D80621"/>'],
    MX: ['墨西哥', 'v:#006847,#FFFFFF,#CE1126'],
    BR: ['巴西', '<rect width="20" height="14" fill="#009C3B"/><path d="M10 1.5L19 7l-9 5.5L1 7z" fill="#FFDF00"/><circle cx="10" cy="7" r="3.1" fill="#002776"/>'],
    AR: ['阿根廷', 'h:#74ACDF,#FFFFFF,#74ACDF'],
    CL: ['智利', '<rect width="20" height="14" fill="#FFFFFF"/><rect y="7" width="20" height="7" fill="#D52B1E"/><rect width="6.8" height="7" fill="#0039A6"/><polygon points="' + starPts(3.4, 3.5, 2.1) + '" fill="#FFFFFF"/>'],
    CO: ['哥伦比亚', 'h:#FCD116,#003893,#CE1126'],
    PE: ['秘鲁', 'v:#D91023,#FFFFFF,#D91023'],
    GB: ['英国', '<rect width="20" height="14" fill="#012169"/><path d="M0 0L20 14M20 0L0 14" stroke="#FFFFFF" stroke-width="2.8"/><path d="M0 0L20 14M20 0L0 14" stroke="#C8102E" stroke-width="1.4"/><rect x="8.2" width="3.6" height="14" fill="#FFFFFF"/><rect y="5.2" width="20" height="3.6" fill="#FFFFFF"/><rect x="9" width="2" height="14" fill="#C8102E"/><rect y="6" width="20" height="2" fill="#C8102E"/>'],
    DE: ['德国', 'h:#000000,#DD0000,#FFCE00'],
    FR: ['法国', 'v:#002395,#FFFFFF,#ED2939'],
    NL: ['荷兰', 'h:#AE1C28,#FFFFFF,#21468B'],
    BE: ['比利时', 'v:#000000,#FDDA24,#EF3340'],
    LU: ['卢森堡', 'h:#ED2939,#FFFFFF,#00A1DE'],
    CH: ['瑞士', '<rect width="20" height="14" fill="#D52B1E"/><rect x="8.5" y="3.2" width="3" height="7.6" fill="#FFFFFF"/><rect x="6.2" y="5.5" width="7.6" height="3" fill="#FFFFFF"/>'],
    AT: ['奥地利', 'h:#ED2939,#FFFFFF,#ED2939'],
    IT: ['意大利', 'v:#009246,#FFFFFF,#CE2B37'],
    ES: ['西班牙', 'h:#AA151B,#F1BF00,#AA151B'],
    PT: ['葡萄牙', 'v:#006600,#FF0000'],
    IE: ['爱尔兰', 'v:#169B62,#FFFFFF,#FF883E'],
    SE: ['瑞典', 'x:#006AA7,#FECC00,0.32'],
    NO: ['挪威', '<rect width="20" height="14" fill="#BA0C2F"/><rect x="5.4" width="3.4" height="14" fill="#FFFFFF"/><rect y="5.3" width="20" height="3.4" fill="#FFFFFF"/><rect x="6.4" width="1.4" height="14" fill="#00205B"/><rect y="6.3" width="20" height="1.4" fill="#00205B"/>'],
    DK: ['丹麦', 'x:#C60C30,#FFFFFF,0.32'],
    FI: ['芬兰', 'x:#FFFFFF,#002F6C,0.32'],
    IS: ['冰岛', 'x:#02529C,#FFFFFF,0.32'],
    PL: ['波兰', 'h:#FFFFFF,#DC143C'],
    CZ: ['捷克', '<rect width="20" height="7" fill="#FFFFFF"/><rect y="7" width="20" height="7" fill="#D7141A"/><path d="M0 0L9.6 7 0 14z" fill="#11457E"/>'],
    HU: ['匈牙利', 'h:#CE2939,#FFFFFF,#477050'],
    RO: ['罗马尼亚', 'v:#002B7F,#FCD116,#CE1126'],
    BG: ['保加利亚', 'h:#FFFFFF,#00966E,#D62612'],
    GR: ['希腊', '<rect width="20" height="14" fill="#FFFFFF"/><g fill="#0D5EAF"><rect width="20" height="1.56"/><rect y="3.11" width="20" height="1.56"/><rect y="6.22" width="20" height="1.56"/><rect y="9.33" width="20" height="1.56"/><rect y="12.44" width="20" height="1.56"/></g><rect width="7.8" height="7.8" fill="#0D5EAF"/><rect x="2.9" width="2" height="7.8" fill="#FFFFFF"/><rect y="2.9" width="7.8" height="2" fill="#FFFFFF"/>'],
    RU: ['俄罗斯', 'h:#FFFFFF,#0039A6,#D52B1E'],
    UA: ['乌克兰', 'h:#0057B7,#FFD700'],
    TR: ['土耳其', '<rect width="20" height="14" fill="#E30A17"/><circle cx="7.8" cy="7" r="3.2" fill="#FFFFFF"/><circle cx="9.1" cy="7" r="2.6" fill="#E30A17"/><polygon points="' + starPts(12.7, 7, 2) + '" fill="#FFFFFF"/>'],
    EE: ['爱沙尼亚', 'h:#0072CE,#000000,#FFFFFF'],
    LV: ['拉脱维亚', 'h:#9E3039,#FFFFFF,#9E3039'],
    LT: ['立陶宛', 'h:#FDB913,#006A44,#C1272D'],
    AE: ['阿联酋', '<rect width="20" height="4.67" fill="#00732F"/><rect y="4.67" width="20" height="4.67" fill="#FFFFFF"/><rect y="9.33" width="20" height="4.67" fill="#000000"/><rect width="5" height="14" fill="#FF0000"/>'],
    SA: ['沙特阿拉伯', '<rect width="20" height="14" fill="#006C35"/><rect x="4.2" y="8.2" width="11.6" height="1.5" rx=".75" fill="#FFFFFF"/><rect x="4.2" y="4.9" width="8.6" height="1.1" fill="#FFFFFF"/>'],
    IL: ['以色列', '<rect width="20" height="14" fill="#FFFFFF"/><rect y="1.6" width="20" height="1.9" fill="#0038B8"/><rect y="10.5" width="20" height="1.9" fill="#0038B8"/><path d="M10 3.9l2.5 4.3H7.5z" fill="none" stroke="#0038B8" stroke-width=".8"/><path d="M10 10.1L7.5 5.8h5z" fill="none" stroke="#0038B8" stroke-width=".8"/>'],
    IR: ['伊朗', 'h:#239F40,#FFFFFF,#DA0000'],
    EG: ['埃及', 'h:#CE1126,#FFFFFF,#000000'],
    NG: ['尼日利亚', 'v:#008751,#FFFFFF,#008751'],
    KE: ['肯尼亚', 'h:#000000,#BB0000,#006600']
  };
  function flagSVG(code) {
    var e = FLAGS[code];
    if (!e) return '';
    // 只有 h/v/x 三种"带参数"的形态有冒号前缀，其余一律按手绘 SVG 处理。
    // 刻意不用 indexOf(':') 来切分：手绘片段里一旦出现冒号（伪协议、命名空间、
    // 未来的 data: 之类），整段描述会被切成两半，最终渲染出一面纯色空旗，
    // 而且不报任何错——这类"静默画错"比崩溃难查得多。
    var spec = e[1];
    var m = /^([hvx]):/.exec(spec);
    var kind = m ? m[1] : 's';
    var arg = m ? spec.slice(m[0].length) : spec;
    var inner = '';
    if (kind === 'h' || kind === 'v') {
      var cs = arg.split(',');
      var step = (kind === 'h' ? 14 : 20) / cs.length;
      for (var k = 0; k < cs.length; k++) {
        inner += kind === 'h'
          ? '<rect y="' + (step * k).toFixed(2) + '" width="20" height="' + step.toFixed(2) + '" fill="' + cs[k] + '"/>'
          : '<rect x="' + (step * k).toFixed(2) + '" width="' + step.toFixed(2) + '" height="14" fill="' + cs[k] + '"/>';
      }
    } else if (kind === 'x') {
      var xs = arg.split(',');
      var ox = parseFloat(xs[2]) * 20;
      inner = '<rect width="20" height="14" fill="' + xs[0] + '"/>' +
        '<rect x="' + (ox - 1.7).toFixed(2) + '" width="3.4" height="14" fill="' + xs[1] + '"/>' +
        '<rect y="5.3" width="20" height="3.4" fill="' + xs[1] + '"/>';
    } else {
      inner = arg;
    }
    // 根元素必须带 xmlns：svgNode() 走的是 XML 解析（image/svg+xml），而 XML
    // 对无前缀元素**没有默认命名空间**——缺了 xmlns，<svg> 会落在空命名空间，
    // document.importNode 进 HTML 后就只是一个未知元素（ctor=Element），
    // 浏览器按 0×0 布局：不报错、不画图，旗帜整体"消失"。这正是踩过的坑：
    // 同一段标记走 innerHTML 却是好的（HTML 解析器会把 <svg> 自动归入 SVG
    // 命名空间），两条路径表现不同，源码级护栏也看不出来。
    return '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 14" width="20" height="14" aria-hidden="true">' +
      '<rect width="20" height="14" fill="#dfe4ee"/>' + inner + '</svg>';
  }
  // -- 国旗：FLAG-COMMON-END --

  // svgNode 把一个 SVG 字符串解析成 DOM 节点（与后台前端同名同实现）。
  //
  // 为什么不用 innerHTML：本项目约定不可信字符串一律走 textContent。这里解析的
  // 是**本文件自己生成**的国旗 SVG（内容只来自上面的 FLAGS 常量表，不含任何
  // 外部数据），用 XML 解析能明确拿到 <svg> 根节点并落到 SVG 命名空间；
  // 失败时返回 null，调用方据此不画任何东西——绝不把异常抛出去中断整张卡片的渲染。
  function svgNode(svgText) {
    try {
      var doc = new DOMParser().parseFromString(svgText, 'image/svg+xml');
      var root = doc && doc.documentElement;
      if (!root || root.nodeName === 'parsererror' ||
        String(root.nodeName).toLowerCase() !== 'svg') {
        return null;
      }
      return document.importNode(root, true);
    } catch (e) {
      return null;
    }
  }

  // renderFlag 渲染国家/地区标识。
  //
  // 未设置 country（空串）或代码不在 FLAGS 表里时不画旗帜，也不填占位符：
  // 这一格由 CSS 固定宽度（.n-flag），所以"有没有旗帜"不会让同一行的
  // 名称起点左右跳动——卡片位置固定是本项目的硬约束。
  function renderFlag(box, code) {
    if (!box) return;
    if (box._code === code) return; // 3 秒轮询一次，代码没变就别重建 DOM
    box._code = code;
    box.textContent = '';
    box.removeAttribute('title');
    if (!code) return;
    var svg = svgNode(flagSVG(code));
    if (!svg) return;
    box.appendChild(svg);
    var e = FLAGS[code];
    box.title = e ? e[0] : ('地区 ' + code);
  }

  // renderStackBadges 渲染 IPv4/IPv6 标识。
  //
  // net_stack 是后端算好的协议栈标识（"v4"/"v6"/"v4+v6"，没有则为空串），
  // 前端只做展示、不接触任何地址。为空时渲染 "—"：与其让这一格消失导致
  // 同行内容宽度随节点变化，不如明确告诉读者"这里没有数据"。
  function renderStackBadges(box, stack) {
    box.textContent = '';
    var parts = stack ? String(stack).split('+') : [];
    if (!parts.length) {
      box.appendChild(el('span', 'n-badges-none', '—'));
      return;
    }
    parts.forEach(function (p) {
      var v = p.trim().toLowerCase();
      var text = v === 'v6' ? 'IPv6' : (v === 'v4' ? 'IPv4' : '');
      if (!text) return;
      var b = el('span', 'ip-badge ' + v, text);
      b.title = '该节点已配置 ' + text + ' 地址';
      box.appendChild(b);
    });
    if (!box.children.length) box.appendChild(el('span', 'n-badges-none', '—'));
  }

  // renderLatencyBars 渲染节点卡片上的"探测目标延迟情况"点阵。
  //
  // 每行一小节：表头（目标名 + 当前延迟 + 当前丢包）+ 两行小点——
  // 第一行是延迟、第二行是丢包，都按"墨的深浅"分级（越严重墨越重，
  // 见 latencyColor），不再用绿/橙/红。每个点代表时间窗里的一小段，
  // 段长由后端按窗口下发（b.step），标题与悬浮提示都据此生成。
  //
  // 时间窗（30 分钟 / 1 / 6 / 24 小时）来自 settings.public.lat_window，
  // 由 /site 下发；标题里的"近 N 小时"与"每个点约 X"都不硬编码，
  // 否则管理员改完设置、点阵数据先到而标题还挂着旧文案。
  //
  // 数据可能整段缺失（rtt[i]/loss[i] 为 null）：那一段画灰点，
  // 语义是"那段时间没有探测"，绝不能涂深假装健康。
  // 轮询每 3 秒一次但点阵数据约 60 秒才变一次，用签名跳过无谓的 DOM 重建。
  //
  // LAT_ROWS 是**恒定行数**：有的节点有专属目标、有的没有，行数随节点变化
  // 会把卡片高度拉得参差不齐（同一行网格里的卡片底边对不齐）。行数恒定后
  // 所有卡片等高；不足的补占位行（灰点 + "—"，与"缺值渲染 — 不省略行"
  // 的约定同源），超过的折叠进块内滚动区。占位行让不同卡片的第 i 行
  // 保持在同一水平线上——点阵本来就是拿来看"横向对齐"的。
  var LAT_ROWS = 4;

  // 点阵时间窗（settings.public.lat_window，/site 下发）。默认 24h；
  // 标题文案与"每个点约多久"的悬浮提示都按它生成，不硬编码。
  var latWindow = '24h';
  var LAT_WINDOW_LABEL = {
    '30m': '近 30 分钟', '1h': '近 1 小时', '6h': '近 6 小时', '24h': '近 24 小时'
  };
  function latWindowLabel() { return LAT_WINDOW_LABEL[latWindow] || LAT_WINDOW_LABEL['24h']; }
  function stepLabel(s) {
    if (s % 3600 === 0) return (s / 3600) + ' 小时';
    if (s % 60 === 0) return (s / 60) + ' 分钟';
    return s + ' 秒';
  }

  function renderLatencyBars(box, bars) {
    var on = cardOn('lat_bars');
    if (!on) {
      box.hidden = true;
      box.textContent = '';
      box._sig = null;
      return;
    }
    var list = bars || [];
    // 签名带上时间窗：管理员改了 public.lat_window 之后、调度器还没重算完
    // 点阵数据的那一分钟里，标题也要先跟上，别让"近 24 小时"挂着旧文案。
    var sig = latWindow + '|on' + JSON.stringify(list);
    if (box._sig === sig) return;
    box._sig = sig;
    box.hidden = false;
    box.textContent = '';

    var tip = el('div', 'lb-title');
    tip.textContent = '延迟 / 丢包 · ' + latWindowLabel();
    if (list.length && list[0].step) {
      tip.title = '每个点约 ' + stepLabel(list[0].step) +
        '；上行是延迟、下行是丢包，悬浮某点可看该段时间的数值。';
    } else {
      tip.title = '上行是延迟、下行是丢包，悬浮某点可看该段时间的数值。';
    }
    box.appendChild(tip);

    if (!list.length) {
      // 一条目标都没有（从未上报 / 未配置）：明确说"没有数据"，
      // 而不是留一块空白让人以为是渲染坏了。块高由 CSS 钉住，卡片高度不受影响。
      box.appendChild(el('div', 'lb-empty', '暂无探测数据'));
      return;
    }

    list.forEach(function (b) { box.appendChild(latRow(b)); });
    // 占位行的点数取自真实行（后端 LatencyBarsSegments 变了也不会漂移）。
    var dots = (list[0] && list[0].rtt && list[0].rtt.length) || 32;
    for (var i = list.length; i < LAT_ROWS; i++) box.appendChild(latPlaceholderRow(dots));
  }

  // latRow 渲染一行（一条探测目标）：目标名 + 当前延迟/丢包 + 两行小点。
  function latRow(b) {
    var row = el('div', 'lb-row');
    var head = el('div', 'lb-head');
    // 行标签用**目标名**（管理员起的名字），不再是运营组常量——
    // "电信-上海""专线-内网"比"电信""自定义"能表达的信息多得多。
    // 与详情页同一规则：名字里已写明运营商就不再缀"· 电信"，
    // 同一个词出现两遍是噪声；读不出时才补后缀兜底。
    var label = b.name || CARRIER[b.carrier] || b.carrier;
    var carrier = CARRIER[b.carrier] || '';
    if (carrier && label.indexOf(carrier) < 0) label += ' · ' + carrier;
    head.appendChild(el('span', 'lb-name', label));

    var lastRtt = lastNonNull(b.rtt);
    var lastLoss = lastNonNull(b.loss);
    var rt = el('span', 'lb-val');
    if (lastRtt != null) {
      rt.textContent = (lastRtt / 1000).toFixed(0) + ' ms';
      rt.style.color = latencyColor(lastRtt);
    } else {
      rt.textContent = '—';
      rt.className = 'lb-val muted-v';
    }
    head.appendChild(rt);

    var ls = el('span', 'lb-val lb-loss');
    if (lastLoss != null) {
      ls.textContent = (lastLoss / 100).toFixed(1) + '%';
      ls.style.color = lossColor(lastLoss);
    } else {
      ls.textContent = '—';
      ls.className = 'lb-val muted-v';
    }
    head.appendChild(ls);
    row.appendChild(head);

    row.appendChild(dotsLine(b, 'rtt'));
    row.appendChild(dotsLine(b, 'loss'));
    return row;
  }

  // latPlaceholderRow 占位行：与真实行同结构（同名同值的"—"、全灰点），
  // 只为把行数补齐到 LAT_ROWS，让卡片等高、各行跨卡片水平对齐。
  function latPlaceholderRow(n) {
    var row = el('div', 'lb-row lb-row-ph');
    var head = el('div', 'lb-head');
    head.appendChild(el('span', 'lb-name muted-v', '—'));
    head.appendChild(el('span', 'lb-val muted-v', '—'));
    head.appendChild(el('span', 'lb-val lb-loss muted-v', '—'));
    row.appendChild(head);
    row.appendChild(emptyDots(n));
    row.appendChild(emptyDots(n));
    return row;
  }

  function emptyDots(n) {
    var line = el('div', 'lb-dots');
    for (var i = 0; i < n; i++) line.appendChild(el('i', 'lb-dot none'));
    return line;
  }

  function lastNonNull(arr) {
    if (!arr) return null;
    for (var i = arr.length - 1; i >= 0; i--) {
      if (arr[i] != null) return arr[i];
    }
    return null;
  }

  // latencyColor / lossColor：灰阶"墨量"刻度。约定只有一条——**越严重墨越重**
  // （--c1 最醒目，--c5 最接近底色）。健康的链路安静地淡着，出问题的链路
  // 自己黑出来；暗色下整体反转成"越严重越亮"，语义不变。
  // 离散 5 档而不是连续渐变：档位即色阶成员，深浅切换时取到的都是实色。
  function latencyColor(rttUs) {
    var ms = (rttUs || 0) / 1000;
    if (ms <= 50)  return cssVar('--c5', '#c4c4c4');
    if (ms <= 100) return cssVar('--c4', '#9e9e9e');
    if (ms <= 200) return cssVar('--c3', '#7a7a7a');
    if (ms <= 500) return cssVar('--c2', '#555555');
    return cssVar('--c1', '#2e2e2e');
  }

  // lossColor 丢包点的颜色：没有"健康的高丢包"，所以 0 就是全档里最淡的
  // 一格（与"无数据"的轨道灰仍可区分——那个更淡且透明），其余按严重度
  // 加墨，跳变比渐变醒目。
  function lossColor(lossX100) {
    if (lossX100 <= 0)    return cssVar('--c5', '#c4c4c4');
    if (lossX100 < 500)   return cssVar('--c4', '#9e9e9e');
    if (lossX100 < 2000)  return cssVar('--c3', '#7a7a7a');
    if (lossX100 < 10000) return cssVar('--c2', '#555555');
    return cssVar('--c1', '#2e2e2e');
  }

  function dotsLine(bar, kind) {
    var line = el('div', 'lb-dots');
    var arr = kind === 'rtt' ? bar.rtt : bar.loss;
    var n = (bar.rtt || []).length;
    for (var i = 0; i < n; i++) {
      var d = el('i', 'lb-dot');
      var v = arr ? arr[i] : null;
      var when = fmtHM((bar.from || 0) + i * (bar.step || 1));
      if (v == null) {
        d.className = 'lb-dot none';
        d.title = when + ' · 无数据';
      } else if (kind === 'rtt') {
        d.style.backgroundColor = latencyColor(v);
        d.title = when + ' · ' + (v / 1000).toFixed(0) + ' ms';
      } else {
        d.style.backgroundColor = lossColor(v);
        d.title = when + ' · 丢包 ' + (v / 100).toFixed(1) + '%';
      }
      line.appendChild(d);
    }
    return line;
  }

  function fmtHM(ts) {
    var d = new Date(ts * 1000);
    var hh = String(d.getHours()).padStart(2, '0');
    var mm = String(d.getMinutes()).padStart(2, '0');
    return (d.getMonth() + 1) + '/' + d.getDate() + ' ' + hh + ':' + mm;
  }

  function getJSON(path) {
    return fetch(API + path, { headers: { Accept: 'application/json' } }).then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    });
  }

  var CARRIER = { telecom: '电信', unicom: '联通', mobile: '移动', custom: '自定义' };

  // 探测状态与节点状态的中文文案（共用一套语义色）。
  var CARRIER_NAME = {
    ok: '正常', warn: '偏高', bad: '很高', unknown: '无数据',
    online: '在线', offline: '离线', warning: '疑似离线'
  };

  // -------------------------------------------------------------------------
  // 节点卡片（结构固定，缺值用占位而不是省略行，保证高度恒定）
  // -------------------------------------------------------------------------

  // makeMetric 构建一条指标：标签行 + 进度条 + 固定高度的说明行。
  function makeMetric(label) {
    var box = el('div', 'nb');
    var row = el('div', 'nb-row');
    var k = el('span', 'nb-k');
    k.appendChild(document.createTextNode(label + ' '));
    var kb = el('b');
    k.appendChild(kb);
    var v = el('span', 'nb-v');
    row.appendChild(k);
    row.appendChild(v);

    var track = el('div', 'track');
    var fill = el('i');
    track.appendChild(fill);

    var sub = el('div', 'nb-sub');

    box.appendChild(row);
    box.appendChild(track);
    box.appendChild(sub);
    return { box: box, kb: kb, v: v, fill: fill, sub: sub };
  }

  function makeCard(n) {
    var root = el('article', 'node');
    root.tabIndex = 0;
    root.setAttribute('role', 'button');

    var head = el('div', 'n-head');
    var dot = el('span', 'dot');
    // 国旗标识。这一格**恒占位**（CSS 固定 20px 宽）：未设置国家时留空，
    // 这样同一行的名称起点不会随"这个节点有没有国家"而左右跳动。
    var flag = el('span', 'n-flag');
    var name = el('span', 'n-name');
    var chip = el('span', 'n-chip');
    head.appendChild(dot);
    head.appendChild(flag);
    head.appendChild(name);
    head.appendChild(chip);

    var sub = el('div', 'n-sub');
    var os = el('span', 'n-os');
    var exp = el('span', 'n-exp');
    // 协议栈标识（IPv4/IPv6）。这一格恒占位：没有协议栈信息时渲染 "—"，
    // 而不是把整格省掉——省掉会让同一行其它内容的宽度随节点不同而变，
    // 卡片高度也就跟着跳。
    var badges = el('span', 'n-badges');
    sub.appendChild(os);
    sub.appendChild(badges);
    sub.appendChild(exp);

    var bars = el('div', 'n-bars');
    // 四条指标条各自记下卡片键：updateCard 按 cardOn 逐块隐藏，并按剩余
    // 块数重排 .n-bars 的列数（与 .kpis / .dinfo 同一套做法）。
    var mCpu = makeMetric('CPU');
    var mMem = makeMetric('内存');
    var mDisk = makeMetric('磁盘');
    var mTraf = makeMetric('流量');
    mCpu.box.setAttribute('data-card', 'node_cpu');
    mMem.box.setAttribute('data-card', 'node_mem');
    mDisk.box.setAttribute('data-card', 'node_disk');
    mTraf.box.setAttribute('data-card', 'node_traffic');
    bars.appendChild(mCpu.box);
    bars.appendChild(mMem.box);
    bars.appendChild(mDisk.box);
    bars.appendChild(mTraf.box);

    // 续费价格（折算到月均）。与四条进度条一样是**全局开关**（public.cards
    // 的 node_fee），缺价格渲染 "—"，显隐一致时卡片依旧等高。
    var fee = el('div', 'n-fee');
    fee.setAttribute('data-card', 'node_fee');
    fee.hidden = true;
    var feeK = el('span', 'fee-k', '续费');
    var feeV = el('span', 'fee-v', '—');
    fee.appendChild(feeK);
    fee.appendChild(feeV);

    var net = el('div', 'n-net');
    net.setAttribute('data-card', 'node_net');
    var rateRow = el('div', 'net-row');
    var rateRxDir = el('span', 'dir down', '↓');
    var rateRx = el('b');
    var rateTxDir = el('span', 'dir up up-gap', '↑');
    var rateTx = el('b');
    rateRow.appendChild(rateRxDir); rateRow.appendChild(rateRx);
    rateRow.appendChild(rateTxDir); rateRow.appendChild(rateTx);

    var totRow = el('div', 'net-row totals');
    var totRxDir = el('span', 'dir down', '↓');
    var totRx = el('b');
    var totTxDir = el('span', 'dir up up-gap', '↑');
    var totTx = el('b');
    totRow.appendChild(totRxDir); totRow.appendChild(totRx);
    totRow.appendChild(totTxDir); totRow.appendChild(totTx);

    // 速率与续费同行：速率在左、月均价格在右。价格不再单独占一行——那一行
    // 90% 的时间只有"续费 ¥xx / 月"几个字，却要占掉整张卡一行的高度。
    // node_fee 与 node_net 是两个独立开关，四种组合都要成立：
    // 都开=同行两侧；只开价格=价格独占这行；只开速率=速率独占；都关=整块隐藏。
    var rateLine = el('div', 'net-line');
    rateLine.appendChild(rateRow);
    rateLine.appendChild(fee);

    net.appendChild(rateLine);
    net.appendChild(totRow);

    // 探测目标延迟情况（点阵进度条）。默认隐藏：开关是全局的（public.cards），
    // 关闭时所有卡片同步隐藏；数据未到时由 updateCard 决定画点还是画占位。
    var lat = el('div', 'n-lat');
    lat.hidden = true;

    root.appendChild(head);
    root.appendChild(sub);
    root.appendChild(bars);
    root.appendChild(net);
    root.appendChild(lat);

    var open = function () {
      location.hash = '#/n/' + encodeURIComponent(n.public_id);
    };
    root.addEventListener('click', open);
    root.addEventListener('keydown', function (ev) {
      if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); open(); }
    });

    return {
      root: root,
      dot: dot, flag: flag, name: name, chip: chip, os: os, exp: exp, badges: badges,
      bars: bars, fee: fee, feeV: feeV, net: net, rateRow: rateRow, totRow: totRow,
      cpu: mCpu, mem: mMem, disk: mDisk, traf: mTraf,
      rateRx: rateRx, rateTx: rateTx, totRx: totRx, totTx: totTx,
      lat: lat
    };
  }

  // applyNodeCardBlocks 把 public.cards 的白名单应用到单张节点卡片上的各区块。
  //
  // 四条进度条是**纵向**排列的（.n-bars 是 flex column），所以关掉其中一条
  // 不需要重排列数，只要 hidden 掉即可，剩下的自然向上收拢。这一条与
  // .kpis / .dinfo 那两个横向网格不同——那边必须按剩余张数挂 n<N>/d<N> 类，
  // 否则末行会空出格子。这里不要照搬，多挂一个没有对应 CSS 的类只是噪音。
  //
  // 一条都不剩时整条 .n-bars 仍要隐藏：留着会剩下一道带边框/间距的空壳，
  // 而且每张卡都空一道，看着像渲染坏了。
  function applyNodeCardBlocks(c) {
    c.cpu.box.hidden = !cardOn('node_cpu');
    c.mem.box.hidden = !cardOn('node_mem');
    c.disk.box.hidden = !cardOn('node_disk');
    c.traf.box.hidden = !cardOn('node_traffic');

    c.bars.hidden = !(cardOn('node_cpu') || cardOn('node_mem') ||
                      cardOn('node_disk') || cardOn('node_traffic'));
    // 速率行与价格行在同一个 .net-line 里（速率左、价格右），两个开关各自
    // 只管自己的那一半；.n-net 整块在两者全关时才隐藏。
    c.fee.hidden = !cardOn('node_fee');
    c.rateRow.hidden = !cardOn('node_net');
    c.totRow.hidden = !cardOn('node_net');
    c.net.hidden = !(cardOn('node_fee') || cardOn('node_net'));
  }

  function updateMetric(m, percent, valueText, subText, labelBold) {
    setText(m.kb, labelBold === undefined ? '' : labelBold);
    setText(m.v, valueText);
    setWidth(m.fill, percent);
    setClass(m.fill, levelOf(percent));
    setText(m.sub, subText || '—');
  }

  function updateCard(c, n, nowSec) {
    var status = n.status || 'offline';
    setClass(c.dot, 'dot ' + status);
    setText(c.name, n.name || n.public_id);
    setClass(c.chip, 'n-chip ' + status);
    setText(c.chip, statusText(n, nowSec));
    renderFlag(c.flag, n.country);

    // 先按 public.cards 决定哪些区块出现，再填值。
    // 顺序很重要：块被关掉后仍然照常算值纯属浪费，更重要的是——如果把
    // "关掉"写在填值之后，缺值占位（—）会先写进 DOM，视觉上仍会闪一下。
    applyNodeCardBlocks(c);

    // 从未上报过的节点（刚建好、Agent 还没装）：所有指标渲染成 "—"。
    //
    // 这里渲染 0% 会造成实打实的误读——0% 的意思是"测到了，使用率为零"，
    // 而事实是"没有任何数据"。看板上一堆 0% 会让人以为节点健康得很。
    var noData = !n.updated_at;

    // 系统信息：虚拟化方式 · 架构（缺一项就只显示另一项）
    var osParts = [];
    if (n.virt) osParts.push(n.virt);
    if (n.arch) osParts.push(n.arch);
    setText(c.os, osParts.length ? osParts.join(' · ') : (n.os || '—'));

    var ex = expireText(n.expire_days);
    setClass(c.exp, 'n-exp ' + ex.level);
    setText(c.exp, ex.text);

    renderStackBadges(c.badges, n.net_stack);

    // 续费价格：按支付周期折算到月均（后端 MonthlyEquivalent 口径）。
    // once/lifetime 折算后是 0，靠 fee_cycle 区分"终身/一次性"与"没配价格"
    // ——两者数值都是 0，但文案必须不同，不能都写 "—"。
    var feeText = '—';
    if (n.monthly_fee > 0) {
      feeText = fmtMoney(n.monthly_fee, n.monthly_fee_currency) + ' / 月';
    } else if (n.fee_cycle === 'lifetime') {
      feeText = '终身';
    } else if (n.fee_cycle === 'once') {
      feeText = '一次性';
    }
    setText(c.feeV, feeText);

    renderLatencyBars(c.lat, n.lat_bars);

    // CPU：核心数放在标签里，负载放在说明行
    updateMetric(
      c.cpu,
      noData ? 0 : n.cpu_pct,
      noData ? '—' : pct(n.cpu_pct),
      noData ? '—' : (n.load1 === undefined || n.load1 === null
        ? '—' : '负载 ' + (n.load1 / 100).toFixed(2)),
      n.cpu_cores ? n.cpu_cores + ' 核' : ''
    );

    // 内存 / 磁盘：说明行显示 已用 / 总量
    updateMetric(c.mem, noData ? 0 : n.mem_pct, noData ? '—' : pct(n.mem_pct),
      noData ? '—' : fmtBytes(n.mem_used) + ' / ' + fmtBytes(n.mem_total));

    updateMetric(c.disk, noData ? 0 : n.disk_pct, noData ? '—' : pct(n.disk_pct),
      noData ? '—' : fmtBytes(n.disk_used) + ' / ' + fmtBytes(n.disk_total));

    // 流量：不限额度时进度条置零并显示"不限"，避免显示成 0% 让人误以为没用量
    if (noData) {
      updateMetric(c.traf, 0, '—', '—');
    } else if (n.traffic_quota > 0) {
      updateMetric(c.traf, n.traffic_used_pct, pct(n.traffic_used_pct),
        fmtBytes(n.traffic_used) + ' / ' + fmtBytes(n.traffic_quota));
    } else {
      updateMetric(c.traf, 0, '不限',
        n.traffic_used === undefined ? '—' : fmtBytes(n.traffic_used) + ' 已用');
    }

    setText(c.rateRx, noData ? '—' : fmtBps(n.rx_bps));
    setText(c.rateTx, noData ? '—' : fmtBps(n.tx_bps));
    setText(c.totRx, noData ? '—' : fmtBytes(n.rx_used));
    setText(c.totTx, noData ? '—' : fmtBytes(n.tx_used));
  }

  // -------------------------------------------------------------------------
  // 顶部：分组筛选标签
  // -------------------------------------------------------------------------

  function groupKey(n) { return n.group ? String(n.group) : ''; }

  // 筛选用两个哨兵值。「全部」与「未分组」绝不能用同一个空串表示——
  // 那样点"未分组"会等价于点"全部"，两类节点全都显示出来。
  var FILTER_ALL = '\u0000all';
  var FILTER_NONE = '\u0000nogroup';

  function tabItems(list) {
    var counts = {};
    var order = [];
    list.forEach(function (n) {
      var g = groupKey(n);
      if (!(g in counts)) { counts[g] = 0; order.push(g); }
      counts[g]++;
    });

    var items = [{ key: FILTER_ALL, label: '全部', count: list.length }];
    // 有分组的按首次出现顺序（即服务端的稳定顺序）排列，未分组固定放最后
    order.forEach(function (g) {
      if (g === '') return;
      items.push({ key: g, label: g, count: counts[g] });
    });
    if (counts['']) items.push({ key: FILTER_NONE, label: '未分组', count: counts[''] });
    return items;
  }

  function buildTabs(list) {
    var items = tabItems(list);

    // 签名没变就不重建：重建会让正在被点击/聚焦的按钮失效
    var sig = items.map(function (i) { return i.label + ':' + i.count; }).join('|');
    if (sig === tabSig) {
      applyFilter();
      return;
    }
    tabSig = sig;

    // 当前选中的分组若已不存在（节点被移出公开列表），退回"全部"
    var stillThere = items.some(function (i) { return i.key === filter; });
    if (!stillThere) filter = FILTER_ALL;

    var box = $('tabs');
    box.textContent = '';
    items.forEach(function (it) {
      var b = el('button', 'tab');
      b.type = 'button';
      b.setAttribute('role', 'tab');
      b.dataset.key = it.key;
      b.appendChild(document.createTextNode(it.label));
      b.appendChild(el('span', 'n', it.count));
      b.addEventListener('click', function () {
        filter = it.key;
        applyFilter();
      });
      box.appendChild(b);
    });
    applyFilter();
  }

  // applyFilter 只切换可见性，绝不重排 DOM：网格位置因此始终固定。
  function applyFilter() {
    var tabs = $('tabs').children;
    for (var i = 0; i < tabs.length; i++) {
      var t = tabs[i];
      t.setAttribute('aria-selected', t.dataset.key === filter ? 'true' : 'false');
    }
    Object.keys(cards).forEach(function (id) {
      var c = cards[id];
      var show;
      if (filter === FILTER_ALL) show = true;
      else if (filter === FILTER_NONE) show = c.group === '';
      else show = c.group === filter;
      if (c.root.hidden === show) c.root.hidden = !show;
    });
  }

  // -------------------------------------------------------------------------
  // 列表刷新：按键复用，顺序完全交给服务端
  // -------------------------------------------------------------------------

  function renderNodes(list) {
    var grid = $('nodes');
    var seen = {};
    var nowSec = Math.floor(Date.now() / 1000);

    list.forEach(function (n, idx) {
      var id = n.public_id;
      seen[id] = true;
      var c = cards[id];
      if (!c) {
        c = makeCard(n);
        cards[id] = c;
      }
      c.group = groupKey(n);
      updateCard(c, n, nowSec);

      // 只在位置确实不对时才移动；顺序稳定时这里一次 DOM 操作都不做。
      var at = grid.children[idx];
      if (at !== c.root) grid.insertBefore(c.root, at || null);
    });

    // 已被隐藏/删除的节点：移除卡片，避免残留幽灵
    Object.keys(cards).forEach(function (id) {
      if (seen[id]) return;
      var c = cards[id];
      if (c.root.parentNode) c.root.parentNode.removeChild(c.root);
      delete cards[id];
    });

    $('empty-tip').hidden = list.length > 0;
    buildTabs(list);
  }

  function renderOverview(o) {
    setText($('kpi-online'), o.nodes_online);
    setText($('kpi-total'), o.nodes_total);
    var off = (o.nodes_total || 0) - (o.nodes_online || 0);
    setText($('kpi-offline'), off > 0 ? off + ' 个离线' : '全部在线');

    setText($('kpi-today-rx'), fmtBytes(o.today_rx));
    setText($('kpi-today-tx'), fmtBytes(o.today_tx));
    setText($('kpi-total-rx'), fmtBytes(o.rx_used));
    setText($('kpi-total-tx'), fmtBytes(o.tx_used));

    setText($('kpi-rx'), fmtBps(o.rx_bps));
    setText($('kpi-tx'), fmtBps(o.tx_bps));

    // 月费合计：由后端 PublicOverview 汇总所有公开节点的 price_minor。
    // 若管理员在 public.fields 里关掉了 monthly_fee，字段不存在，显示 —。
    setText($('kpi-fee'), fmtMoney(o.monthly_fee, o.monthly_fee_currency));
    setText($('kpi-fee-currency'), (o.monthly_fee_currency || 'CNY') + ' / 月');

    spark.push(o.rx_bps || 0);
    if (spark.length > SPARK_POINTS) spark.shift();
    drawSpark();
  }

  // 最忙节点由前端从节点列表算出：概览接口只有聚合值，没有单节点峰值。
  function renderBusiest(list) {
    var best = null;
    list.forEach(function (n) {
      if (n.cpu_pct === undefined || n.cpu_pct === null) return;
      if (n.status === 'archived') return;
      if (!best || n.cpu_pct > best.cpu_pct) best = n;
    });
    if (!best) {
      setText($('kpi-busy-pct'), '—');
      setText($('kpi-busy-name'), '暂无数据');
      return;
    }
    setText($('kpi-busy-pct'), pct(best.cpu_pct, 1));
    setText($('kpi-busy-name'), best.name || best.public_id);
  }

  function refreshList() {
    getJSON('/overview').then(renderOverview).catch(function () { });

    getJSON('/nodes').then(function (list) {
      list = list || [];
      renderBusiest(list);
      renderNodes(list);
      setText($('updated'), '更新于 ' + new Date().toLocaleTimeString());
    }).catch(function () {
      setText($('updated'), '连接中断，重试中…');
    });
  }

  // -------------------------------------------------------------------------
  // 卡片级公开展示开关（settings.public.cards）
  // -------------------------------------------------------------------------

  // cardOn 判断某张卡片当前是否允许展示。
  function cardOn(key) {
    if (!cardAllow) return true; // 未配置 = 全部展示
    return cardAllow[key] === true;
  }

  // applyCards 应用后端下发的卡片白名单，并同步汇总卡的显隐与列数。
  //
  // 三种入参必须区别对待（后端刻意用 null / [] / [...] 区分）：
  //   null / undefined —— 不限制，全部展示
  //   []               —— 一张都不展示
  //   [...]            —— 只展示列出的这些
  // 写成 `if (list && list.length)` 会把 [] 也当成"不限制"，于是管理员
  // 取消全部勾选后看板反而全亮——正是这个开关最不能出的错。
  //
  // 为什么列数要一起算：.kpis 是定列网格，关掉两张卡后若还按 5 列排，
  // 末尾会空出两格——和 .dinfo 早期"6 张排成 4+2"是同一类问题。
  // 这里按**实际显示张数**挂 n<N> 类，让每行正好铺满。
  function applyCards(list) {
    cardAllow = null;
    if (list !== null && list !== undefined) {
      cardAllow = {};
      list.forEach(function (k) { cardAllow[k] = true; });
    }
    // 已建好的节点卡片要立刻跟上：它们的显隐是在 updateCard 里决定的，
    // 而 updateCard 每 3 秒才跑一次。不在这里补一次的话，管理员刚关掉
    // 「节点卡 · 内存」，列表页上的内存条要等下一轮轮询才消失——明明
    // 设置已经保存成功，看上去却像没生效（这类"开关慢半拍"最容易被
    // 当成 bug 反复试）。这里只改显隐，不动数值，开销可忽略。
    //
    // lat_bars 不在这里处理：它的显隐由 renderLatencyBars 自己判（那里
    // 还要顺带清空 DOM 与签名缓存），在别处改 hidden 会被它下一次调用覆盖。
    Object.keys(cards).forEach(function (id) {
      applyNodeCardBlocks(cards[id]);
    });
    syncOverviewChrome();
  }

  // syncOverviewChrome 统一决定汇总区（分组标签 + 汇总卡）的可见性。
  //
  // 两处会改它：路由切换（总览显示、详情隐藏）与卡片白名单（可能一张都不留）。
  // 集中在一个函数里，避免 loadSite 的异步回调把详情页上的汇总卡又"打开"了。
  function syncOverviewChrome() {
    var tabs = $('tabs');
    if (tabs) tabs.hidden = !overviewChrome;

    var box = $('kpis');
    if (!box) return;
    var shown = 0;
    Array.prototype.forEach.call(box.querySelectorAll('[data-card]'), function (n) {
      var on = cardOn(n.getAttribute('data-card'));
      n.hidden = !on;
      if (on) shown++;
    });
    box.className = 'kpis' + (shown >= 1 && shown <= 5 ? ' n' + shown : '');
    box.hidden = !overviewChrome || shown === 0;
  }

  // -------------------------------------------------------------------------
  // 聚合带宽迷你趋势图
  // -------------------------------------------------------------------------

  function drawSpark() {
    var cv = $('spark');
    if (!cv) return;
    var dpr = window.devicePixelRatio || 1;
    var w = cv.clientWidth || 200, h = cv.clientHeight || 22;
    cv.width = Math.round(w * dpr);
    cv.height = Math.round(h * dpr);
    var ctx = cv.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    if (spark.length < 2) return;

    var max = 1;
    spark.forEach(function (v) { if (v > max) max = v; });

    var step = w / (SPARK_POINTS - 1);
    var off = SPARK_POINTS - spark.length;
    var line = getComputedStyle(document.documentElement)
      .getPropertyValue('--dir-down').trim() || '#868686';

    ctx.beginPath();
    spark.forEach(function (v, i) {
      var x = (off + i) * step;
      var y = h - (v / max) * (h - 4) - 2;
      if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
    });
    ctx.strokeStyle = line;
    ctx.lineWidth = 1.4;
    ctx.lineJoin = 'round';
    ctx.stroke();

    ctx.lineTo((off + spark.length - 1) * step, h);
    ctx.lineTo(off * step, h);
    ctx.closePath();
    ctx.globalAlpha = 0.12;
    ctx.fillStyle = line;
    ctx.fill();
    ctx.globalAlpha = 1;
  }

  // -------------------------------------------------------------------------
  // 详情视图
  // -------------------------------------------------------------------------

  function renderDetail(n) {
    detailNode = n;
    setText($('detail-name'), n.name || n.public_id);
    renderFlag($('detail-flag'), n.country);
    var st = $('detail-status');
    setText(st, statusText(n, Math.floor(Date.now() / 1000)));
    setClass(st, 'chip ' + (n.status || 'offline'));

    var ex = expireText(n.expire_days);
    var osParts = [];
    if (n.virt) osParts.push(n.virt);
    if (n.arch) osParts.push(n.arch);

    // 头部信息条：三列，对应参考图的"系统 / CPU / 内存·硬盘"那一块。
    // 之前是 14 张等宽小卡，同样的信息占了四行，扫一眼拿不到重点。
    //
    // 每格的第一个元素是 settings.public.cards 的卡片键：管理员可以逐格
    // 关掉（比如不想公开"本周期流量"），关掉后列数按剩余张数重排。
    var info = $('detail-info');
    info.textContent = '';
    var online = n.status === 'online';
    var groups = [
      ['info_system', '系统', n.os || '—', '架构', osParts.length ? osParts.join(' · ') : '—'],
      ['info_cpu', 'CPU', (n.cpu_cores ? n.cpu_cores + ' 核' : '—') + ' · ' + pct(n.cpu_pct),
       '负载', n.load1 === undefined || n.load1 === null ? '—' : (n.load1 / 100).toFixed(2)],
      // "内存 / 磁盘"这一格原先写的是 mem_used / disk_total —— 分子是内存、
      // 分母是磁盘，两个不同量纲的数被一条斜杠连起来，读起来像"这台机器内存
      // 用了 167 GB"。改成两个指标各自成对，顺序与键一一对应，也顺手带上
      // 总量：只给"已用 2.91 GB"是读不出紧不紧张的。
      ['info_mem', '内存 / 磁盘',
       fmtBytes(n.mem_used) + ' / ' + fmtBytes(n.mem_total) + ' · ' +
       fmtBytes(n.disk_used) + ' / ' + fmtBytes(n.disk_total),
       '交换区', pct(n.swap_pct)],
      ['info_today', '今日流量', '↓ ' + fmtBytes(n.today_rx) + ' · ↑ ' + fmtBytes(n.today_tx),
       '实时速率', '↓ ' + fmtBps(n.rx_bps) + ' · ↑ ' + fmtBps(n.tx_bps)],
      ['info_period', '本周期流量', n.traffic_quota > 0
        ? fmtBytes(n.traffic_used) + ' / ' + fmtBytes(n.traffic_quota) + '（' + pct(n.traffic_used_pct) + '）'
        : fmtBytes(n.traffic_used) + '（不限）',
       '本周期 ↓↑', fmtBytes(n.rx_used) + ' / ' + fmtBytes(n.tx_used)],
      ['info_uptime', '运行时长', online ? dur(n.uptime_s) : '—', '到期', ex.text]
    ];
    var shownInfo = 0;
    groups.forEach(function (g) {
      if (!cardOn(g[0])) return;
      var cell = el('div', 'di');
      cell.setAttribute('data-card', g[0]);
      [[g[1], g[2]], [g[3], g[4]]].forEach(function (pair) {
        var r = el('div', 'di-row');
        r.appendChild(el('span', 'di-k', pair[0]));
        r.appendChild(el('span', 'di-v', pair[1]));
        cell.appendChild(r);
      });
      info.appendChild(cell);
      shownInfo++;
    });
    // 列数跟着实际张数走，避免末行空位；一张都不留时整条隐藏
    // （否则会剩下一条 1px 高、带边框的空壳）。
    info.className = 'dinfo' + (shownInfo >= 1 && shownInfo <= 6 ? ' d' + shownInfo : '');
    info.hidden = shownInfo === 0;

    var expChip = $('detail-expire');
    setText(expChip, ex.text);
    setClass(expChip, 'chip ' + (ex.level || ''));
  }

  // rangeSeconds 把下拉框的范围值换算成秒，用于把 X 轴钉在"完整请求窗口"上。
  // axisWindow 计算指定范围的 X 轴窗口：[now-范围, now]。
  function axisWindow(range) {
    var now = Math.floor(Date.now() / 1000);
    return { tMin: now - rangeSeconds(range), tMax: now };
  }

  function rangeSeconds(range) {
    switch (range) {
      case '30m': return 30 * 60;
      case '1h': return 3600;
      case '5h': return 5 * 3600;
      case '24h': return 24 * 3600;
      case '7d': return 7 * 86400;
      case '30d': return 30 * 86400;
      default: return 24 * 3600;
    }
  }

  function refreshDetail() {
    var id = current;
    var range = $('range-select').value;
    getJSON('/nodes/' + encodeURIComponent(id)).then(function (n) {
      if (current !== id) return;
      renderDetail(n);
      setText($('updated'), '更新于 ' + new Date().toLocaleTimeString());
    }).catch(function () { });

    if (detailTab === 'resources') {
      getJSON('/nodes/' + encodeURIComponent(id) +
              '/history?range=' + range + '&metrics=cpu,mem,net_rx,net_tx,disk_used')
        .then(function (series) {
          if (current !== id) return;
          var by = {};
          (series || []).forEach(function (s) { by[s.metric] = s.points || []; });
          var n = detailNode || {};

          var win = axisWindow(range);
          drawChart($('c-cpu'), [{ points: by.cpu, unit: 'pct', name: 'CPU 使用率' }], win);
          capMeta('cap-cpu', [{ pts: by.cpu, unit: 'pct' }]);

          drawChart($('c-mem'), [{ points: by.mem, unit: 'bytes', name: '内存已用' }], win);
          capMeta('cap-mem', [{ pts: by.mem, unit: 'bytes' }], n.mem_total);

          drawChart($('c-net'), [
            { points: by.net_rx, unit: 'bps', name: '↓ 下行', color: cssVar('--dir-down', '#868686') },
            { points: by.net_tx, unit: 'bps', name: '↑ 上行', color: cssVar('--dir-up', '#2e2e2e') }
          ], win);
          capMeta('cap-net', [
            { pts: by.net_rx, unit: 'bps', name: '↓' },
            { pts: by.net_tx, unit: 'bps', name: '↑' }
          ]);

          drawChart($('c-disk'), [{ points: by.disk_used, unit: 'bytes', name: '磁盘已用' }], win);
          capMeta('cap-disk', [{ pts: by.disk_used, unit: 'bytes' }], n.disk_total);
        }).catch(function () { });
    }

    if (detailTab === 'latency') {
      getJSON('/nodes/' + encodeURIComponent(id) + '/latency?range=' + range)
        .then(function (list) {
          if (current !== id) return;
          var items = list || [];
          // 未探通的目标也要留在图例里（显示"无数据"），否则"这里其实有目标、
          // 只是全挂了"会被静默地当成"没配目标"。
          // 多序列颜色按**序号**从灰阶色阶里取（而不是按各自均值取色）：
          // 同档均值的两条目标在旧方案里会拿到同一个颜色而无法分辨，
          // 参考主题的规矩是"色阶按序列分配、明度不够虚线补"。
          // 序号取自全量列表，隐藏某条不影响其余曲线的色号。
          var allSets = items.map(function (t, i) {
            return {
              points: t.points || [], unit: 'us',
              name: t.name || ('目标 #' + t.target_id),
              targetID: t.target_id,
              color: t.avg_rtt == null ? cssVar('--muted', '#868686') : paletteColor(i),
              dash: paletteDash(i)
            };
          });
          // 折线开关：只绘制未被关闭的目标。
          var activeSets = allSets.filter(function (s) { return latToggles[s.targetID] !== false; });
          drawChart($('c-latency'), activeSets, axisWindow(range));
          capMeta('cap-latency', items.filter(function (t) {
            return latToggles[t.target_id] !== false;
          }).map(function (t, i) {
            return { pts: t.points || [], unit: 'us', name: t.name, idx: i };
          }));
          renderLatencyLegend(items, allSets);
          renderLatencyList(items);
        }).catch(function () { });
    }
  }

  // capMeta 生成图表副标题：当前值 · 峰值（· 总量）。
  //
  // 之前图卡只有一行"CPU 使用率"，曲线画出来了却读不到具体数值，
  // 只能拿眼睛去量网格线。这里的峰值与当前值都直接由同一批点算出，
  // 与曲线严格一致；多序列时带上序列名，副标题因此本身就是画布图例的
  // 可读替代（画布里的文字既不能选中，也无法被读屏软件识别）。
  function capMeta(id, specs, total) {
    var node = $(id);
    if (!node) return;
    var has = specs.some(function (s) { return s.pts && s.pts.length; });
    if (!has) { setText(node, '该范围无数据'); return; }
    var unit = specs[0].unit;
    var peak = 0;
    var cur = specs.map(function (s) {
      var label = s.name ? s.name + ' ' : (s.prefix || '');
      if (!s.pts || !s.pts.length) return label + '无数据';
      s.pts.forEach(function (p) { if (p.value > peak) peak = p.value; });
      return label + fmtVal(s.pts[s.pts.length - 1].value, unit);
    });
    var parts = [];
    parts.push(specs.length === 1 ? '当前 ' + cur[0] : cur.join(' '));
    parts.push('峰值 ' + fmtVal(peak, unit));
    if (total) parts.push('总量 ' + fmtBytes(total));
    setText(node, parts.join(' · '));
  }

  // renderLatencyLegend 渲染折线开关图例。点击可隐藏/显示某条目标折线。
  function renderLatencyLegend(items, allSets) {
    var box = $('latency-legend');
    if (!box) return;
    box.textContent = '';
    if (!items || !items.length) return;
    items.forEach(function (t, i) {
      var color = (allSets[i] || {}).color || paletteColor(i);
      var on = latToggles[t.target_id] !== false;
      var item = el('span', 'item' + (on ? '' : ' off'));
      var dot = el('span', 'dot');
      dot.style.backgroundColor = color;
      item.appendChild(dot);
      item.appendChild(document.createTextNode(
        (t.name || ('目标 #' + t.target_id)) +
        (t.avg_rtt == null ? '（无数据）' : ' ' + (t.avg_rtt / 1000).toFixed(1) + ' ms')));
      item.addEventListener('click', function () {
        latToggles[t.target_id] = !on;
        refreshDetail();
      });
      box.appendChild(item);
    });
  }

  // renderLatencyList 渲染"延迟明细"。数据来自 /latency（按目标 + 时间窗口统计），
  // 不是节点 DTO 里的 latency 字段——后者只有最后一轮探测的瞬时值，
  // 换时间跨度时不会跟着变。
  function renderLatencyList(items) {
    var box = $('latency-list');
    box.textContent = '';
    if (!items || !items.length) {
      box.appendChild(el('p', 'muted', '该节点没有配置探测目标，或所选时间范围内没有探测数据。'));
      return;
    }
    items.forEach(function (t) {
      var c = el('div', 'stat');
      var k = el('div', 'k');
      var dot = el('span', 'dot');
      if (t.avg_rtt == null) {
        dot.className = 'dot unknown';
      } else {
        dot.style.backgroundColor = latencyColor(t.avg_rtt);
      }
      k.appendChild(dot);
      // 目标名多半已经把运营商写进去了（"电信-上海"、"广东移动"），再缀一个
      // "· 电信"就成了同一个词出现两遍。只有名字里读不出运营商时才补后缀，
      // 既去掉噪声，又不会把"这条链路走的哪个网"这个分类信息弄丢。
      var carrier = CARRIER[t.carrier] || '';
      var title = t.name || ('目标 #' + t.target_id);
      if (carrier && title.indexOf(carrier) < 0) title += ' · ' + carrier;
      k.appendChild(document.createTextNode(title));
      c.appendChild(k);

      c.appendChild(el('div', 'v', t.avg_rtt == null ? '无数据' : (t.avg_rtt / 1000).toFixed(1) + ' ms'));
      c.appendChild(el('div', 'sub', t.avg_rtt == null
        ? '窗口内未探通 · 状态 ' + (CARRIER_NAME[t.status] || t.status || '未知')
        : '最小 ' + (t.min_rtt / 1000).toFixed(1) + ' · 最大 ' + (t.max_rtt / 1000).toFixed(1) +
          ' ms · 丢包 ' + ((t.loss || 0) / 100).toFixed(1) + '% · ' +
          (CARRIER_NAME[t.status] || t.status || '未知')));
      box.appendChild(c);
    });
  }

  // -------------------------------------------------------------------------
  // Canvas 图表
  // -------------------------------------------------------------------------
  //
  // 自己用 Canvas 画而不是引图表库：CSP 是 script-src 'self'，不能引 CDN，
  // 主题又要能独立分发。
  //
  // 这一版按"能读数"重做。之前只有 3 条网格线（0 / 一半 / 满格）、只在两端
  // 标时间、折线下方没有填充，跨度一长基本看不出量级。现在的规则：
  //   - Y 轴 4 段 5 条网格线，上限取"整齐数字"：百分比取 8/16/24/32 这种，
  //     字节与网速走 1024 进制取 1/2/4 GiB 这种。直接拿最大值当上限会让
  //     曲线永远顶到边框，标签还全是 3.71 GB / 1.85 GB 这类没法读的数。
  //   - X 轴按跨度自动选步长（5 分钟 / 15 分钟 / 1 小时 / 6 小时 / 1 天…），
  //     刻度对齐到整点整分，而不是把区间四等分（那会得到 15:07、16:22）。
  //   - 单序列在折线下方填淡色区域，末端画圆点标出"当前值"。
  //   - 多序列在左上角画图例，带各自的当前值——延迟图要看的就是这几个数。
  // -------------------------------------------------------------------------

  // 灰阶色阶（style.css 的 --c1…--c5，cssVar 取的是**当前主题**下的实色，
  // 因此深浅切换后无需任何转换）。多序列的区分 = 明度档位 + 虚线线型：
  // 这是参考主题 NodeDetail 里 PALETTE 的做法——灰阶里明度只够两三档，
  // 剩下的由 dash 承担；纯靠颜色区分在灰阶下必然撞色。
  var PALETTE = ['--c1', '--c2', '--c3', '--c4', '--c5', '--c2'];
  var PALETTE_DASH = [null, [6, 3], [2, 3], [10, 4, 2, 4], [1, 4], [8, 3]];

  function paletteColor(i) {
    return cssVar(PALETTE[i % PALETTE.length], '#555555');
  }
  function paletteDash(i) {
    return PALETTE_DASH[i % PALETTE_DASH.length];
  }

  // latToggles 记录延迟图里被手动关闭的目标（key = target_id，value = false）。
  // 按 target_id 而不是按索引记，是因为轮询刷新后目标顺序不变但索引可能变。
  var latToggles = {};

  function cssVar(name, fallback) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name);
    return (v && v.trim()) || fallback;
  }

  function prepare(cv) {
    var dpr = window.devicePixelRatio || 1;
    var w = cv.clientWidth || 320;
    var h = cv.clientHeight || 160;
    cv.width = Math.round(w * dpr);
    cv.height = Math.round(h * dpr);
    var ctx = cv.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    return { ctx: ctx, w: w, h: h };
  }

  // 刻度步长的"好看"候选（十进制用）。
  var NICE_STEPS = [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10];

  // 1024 进制下的步长候选：以 1/2/2.5/4/5/8 个数量级为单位，
  // 这样字节轴的刻度会落在 1 MiB、2 GiB 这种位置。
  var NICE_BINARY = [1, 2, 2.5, 3, 4, 5, 6, 8, 10, 12.5, 16, 20, 25, 40, 50,
                     80, 100, 125, 200, 250, 500, 625, 800, 1000];

  // niceTop 返回"整齐"的 Y 轴上限（区间被切成 4 段）。
  // binary 为真时按 1024 进制取整（字节、网速）。
  function niceTop(v, binary) {
    if (!(v > 0)) return binary ? 1024 : 1;
    var rough = v * 1.12 / 4;
    var base, steps;
    if (binary) {
      base = 1;
      while (base * 1024 <= rough && base < 1099511627776) base *= 1024;
      steps = NICE_BINARY;
      var b = base;
      for (var i = 0; i < steps.length; i++) {
        if (steps[i] * b >= rough) return steps[i] * b * 4;
      }
      return 1000 * b * 4;
    }
    var exp = Math.floor(Math.log(rough) / Math.LN10);
    base = Math.pow(10, exp);
    for (var k = 0; k < NICE_STEPS.length; k++) {
      if (NICE_STEPS[k] * base >= rough) return NICE_STEPS[k] * base * 4;
    }
    return 10 * base * 4;
  }

  // fmtVal 是图表里"带单位、带小数"的取值文案（图例 / 副标题用）。
  function fmtVal(v, unit) {
    if (unit === 'pct') return (v / 100).toFixed(1) + '%';
    if (unit === 'us') return (v / 1000).toFixed(1) + ' ms';
    if (unit === 'bps') return fmtBps(v);
    return fmtBytes(v);
  }

  // axisLabel 是坐标轴刻度文案，比 fmtVal 更短（刻度位置紧张）。
  function axisLabel(v, unit) {
    if (unit === 'pct') {
      var n = v / 100;
      return (n % 1 === 0 ? n : n.toFixed(1)) + '%';
    }
    if (unit === 'us') return (v / 1000).toFixed(0) + ' ms';
    if (unit === 'bps') return fmtBytes(v) + '/s';
    return fmtBytes(v);
  }

  // X 轴可选的时间步长（秒）。
  var TIME_STEPS = [60, 120, 300, 600, 900, 1800, 3600, 7200, 3 * 3600,
                    6 * 3600, 12 * 3600, 86400, 2 * 86400, 7 * 86400, 14 * 86400];

  // timeTicks 返回落在 [tMin, tMax] 内、对齐到自然时间边界的刻度。
  function timeTicks(tMin, tMax, want) {
    var span = Math.max(1, tMax - tMin);
    var target = span / Math.max(2, want);
    var step = TIME_STEPS[TIME_STEPS.length - 1];
    for (var i = 0; i < TIME_STEPS.length; i++) {
      if (TIME_STEPS[i] >= target) { step = TIME_STEPS[i]; break; }
    }
    var out = [];
    if (step >= 86400) {
      // 天级刻度必须对齐到**本地**零点。Math.ceil(tMin/step)*step 对齐的是 UTC 零点，
      // 在 UTC+8 下"10/6"那条日期线会落在本地 10/6 早上 8 点——日期分隔卡在一天中间，
      // 最近 7 天/30 天的 X 轴看起来就是"数字不对"。按本地日历日推进才与标签一致。
      var days = Math.max(1, Math.round(step / 86400));
      var d = new Date(tMin * 1000);
      d.setHours(0, 0, 0, 0);
      var t0 = Math.round(d.getTime() / 1000);
      for (var t = t0; t <= tMax; t += days * 86400) {
        if (t >= tMin) out.push(t);
      }
      if (out.length < 2) out = [tMin, tMax];
      return out;
    }
    for (var t2 = Math.ceil(tMin / step) * step; t2 <= tMax; t2 += step) out.push(t2);
    if (out.length < 2) out = [tMin, tMax];
    return out;
  }

  function timeLabel(ts, span) {
    var d = new Date(ts * 1000);
    if (span > 3 * 86400) return (d.getMonth() + 1) + '/' + d.getDate();
    var hh = String(d.getHours()).padStart(2, '0');
    var mm = String(d.getMinutes()).padStart(2, '0');
    return hh + ':' + mm;
  }

  // gapThreshold 从一条序列自身的采样间隔推出"多大间隔算断档"。
  //
  // 背景：节点离线（或被 Master 断联）期间不会有任何样本落盘，时间序列里
  // 那一段就是**根本没有点**。旧实现直接把相邻两点连成一条直线，于是
  // "断联 6 小时"在看板上显示成一条横跨 6 小时、平滑得毫无异常的水平线——
  // 恰恰是最该被看见的故障，被画成了最健康的样子。必须断开。
  //
  // 阈值不能写死：不同跨度的精度层差了几个数量级（30m 走内存层约 3 秒、
  // 24h 走 metrics_1m 约 60 秒、7d/30d 走 10 分钟层约 600 秒）。写死一个
  // 秒数必然在某一层上失效——太小会把正常间隔当断档（曲线被打成虚线），
  // 太大则漏判真正的断联。
  //
  // 因此用**数据自己的中位间隔**当基准：中位数对离群值不敏感，正好用来
  // 描述"这条曲线正常的点距"。判据取 3 倍——单次丢样本、轻微抖动（2 倍）
  // 不触发断点，真正的整段缺失（几十上百倍）必然触发。
  //
  // 样本太少（<4 点）时中位数没有统计意义，返回 Infinity 即不断开。
  function gapThreshold(points) {
    if (!points || points.length < 4) return Infinity;
    var gaps = [];
    for (var i = 1; i < points.length; i++) {
      var g = points[i].ts - points[i - 1].ts;
      if (g > 0) gaps.push(g);
    }
    if (gaps.length < 3) return Infinity;
    gaps.sort(function (a, b) { return a - b; });
    var mid = gaps.length >> 1;
    var median = gaps.length % 2
      ? gaps[mid]
      : (gaps[mid - 1] + gaps[mid]) / 2;
    if (!(median > 0)) return Infinity;
    return median * 3;
  }

  // segmentsOf 把一条时间序列按断档切成若干连续段（每段是 xy 坐标数组）。
  //
  // 抽出来单独用是因为折线、面积、末点三处都要按同一套分段来画：
  // 只在画折线时断开、面积仍连成一片的话，图上会留下一块覆盖了断档区间
  // 的淡化色块——比不断开还难解释。
  function segmentsOf(points, thresh, project) {
    var segs = [], cur = [];
    for (var i = 0; i < points.length; i++) {
      if (i > 0 && thresh !== Infinity &&
          points[i].ts - points[i - 1].ts > thresh) {
        if (cur.length) segs.push(cur);
        cur = [];
      }
      cur.push(project(points[i]));
    }
    if (cur.length) segs.push(cur);
    return segs;
  }

  // drawChart 绘制一组曲线。sets: [{points, unit, name, color}]
  // 没有点的序列不参与取值域计算，但仍会出现在图例里（显示 —）。

  // -------------------------------------------------------------------------
  // 图表悬停（鼠标查看具体数值）
  // -------------------------------------------------------------------------
  //
  // 悬停通过"重画一遍 + 叠加十字线与提示框"实现：canvas 是位图，没法只在
  // 上面加一个可移动的 DOM 元素；而整套绘制由 drawChart 一次完成，重画成本
  // 可接受（移动端 3 秒才轮询一次，悬停重画远低于轮询频率）。
  //
  // initChartHover 只给每张画布挂一次监听；drawChart 每次绘制后更新
  // cv._hoverState 快照与 cv._last（重画参数），mousemove 把光标 x 换算成
  // 时间戳后带 hoverTs 重画。

  function initChartHover(cv, sets, axisWin) {
    cv._last = { sets: sets, axisWin: axisWin };
    if (cv._hoverInit) return;
    cv._hoverInit = true;
    cv.addEventListener('mousemove', function (ev) {
      if (!cv._last || !cv._hoverState) return;
      var r = cv.getBoundingClientRect();
      var x = ev.clientX - r.left;
      var st = cv._hoverState;
      if (x < st.padL - 4 || x > st.padL + st.plotW + 4) {
        if (cv._hoverTs != null) { cv._hoverTs = null; drawChart(cv, cv._last.sets, cv._last.axisWin); }
        return;
      }
      var ts = st.tMin + (x - st.padL) / st.plotW * (st.tMax - st.tMin);
      if (ts !== cv._hoverTs) {
        cv._hoverTs = ts;
        drawChart(cv, cv._last.sets, cv._last.axisWin, ts);
      }
    });
    cv.addEventListener('mouseleave', function () {
      if (cv._hoverTs != null) {
        cv._hoverTs = null;
        drawChart(cv, cv._last.sets, cv._last.axisWin);
      }
    });
  }

  // drawChartHover 画十字线、每条序列的取样圆点与提示框。
  // 序列取 hoverTs **附近**的那个点（而不是插值）：点代表"那一次的真实测量"，
  // 插值会展示一个从未被测量过的数。"附近"有容差（1.5×平均点距，下限为
  // 窗口宽度的 0.5%）——目标刚建或中途才启用时，数据只覆盖窗口尾部，
  // 若无容差地取"最近样本"，悬停窗口左侧会把十字线和提示框钉在几小时外，
  // 看起来就是"悬停没有详情"。
  function drawChartHover(cv, ctx, hoverTs, ax) {
    if (hoverTs == null || !ax.live.length) return;
    // 十字线跟手：画在光标处，而不是最近样本处。
    var x = ax.padL + ax.plotW * ((hoverTs - ax.tMin) / (ax.tMax - ax.tMin));
    var tolFloor = (ax.tMax - ax.tMin) / 200;
    var hits = [], miss = [];
    ax.live.forEach(function (s) {
      if (!s.points || !s.points.length) return;
      var avg = s.points.length > 1
        ? (s.points[s.points.length - 1].ts - s.points[0].ts) / (s.points.length - 1) : 0;
      var tol = Math.max(avg * 1.5, tolFloor);
      var best = null, bestD = Infinity;
      s.points.forEach(function (pt) {
        var d = Math.abs(pt.ts - hoverTs);
        if (d < bestD) { bestD = d; best = pt; }
      });
      if (best && bestD <= tol) hits.push({ s: s, pt: best });
      else miss.push(s);
    });

    // 十字线
    ctx.save();
    ctx.strokeStyle = cssVar('--line-strong', '#868686');
    ctx.lineWidth = 1;
    ctx.setLineDash([4, 3]);
    ctx.beginPath();
    ctx.moveTo(Math.round(x) + .5, ax.padT);
    ctx.lineTo(Math.round(x) + .5, ax.padT + ax.plotH);
    ctx.stroke();
    ctx.setLineDash([]);

    // 光标附近一条样本都没有：只画十字线，不出提示框——悬停在数据空档里
    // 弹一个全是"无数据"的框，比不弹更吵。
    if (!hits.length) { ctx.restore(); return; }

    // 提示框
    var lines = [];
    var d = new Date(hits[0].pt.ts * 1000);
    var hh = String(d.getHours()).padStart(2, '0');
    var mm = String(d.getMinutes()).padStart(2, '0');
    var ss = String(d.getSeconds()).padStart(2, '0');
    lines.push({ text: (d.getMonth() + 1) + '/' + d.getDate() + ' ' + hh + ':' + mm + ':' + ss, head: true });
    hits.forEach(function (h) {
      lines.push({ text: (h.s.name ? h.s.name + '  ' : '') + axisLabel(h.pt.value, h.s.unit), color: h.s.color });
    });
    // 数据没覆盖到光标处的序列也留在提示框里（标"无数据"）：与图例
    // "未探通的目标标注无数据"同一套口径，缺哪条一眼可见。
    miss.forEach(function (s) {
      lines.push({ text: (s.name ? s.name + '  ' : '') + '无数据', color: s.color, muted: true });
    });

    ctx.font = '11px sans-serif';
    var w = 0;
    lines.forEach(function (l) { w = Math.max(w, ctx.measureText(l.text).width); });
    w += 24;
    var lh = 15, boxH = lines.length * lh + 10;
    var bx = x + 10, by = ax.padT + 8;
    if (bx + w > ax.padL + ax.plotW) bx = x - w - 10;

    ctx.globalAlpha = .92;
    ctx.fillStyle = cssVar('--card', '#ffffff');
    ctx.strokeStyle = cssVar('--line', '#dedede');
    ctx.beginPath();
    ctx.rect(bx, by, w, boxH);
    ctx.fill(); ctx.stroke();
    ctx.globalAlpha = 1;

    var ty = by + 13;
    lines.forEach(function (l) {
      if (l.head) {
        ctx.fillStyle = cssVar('--muted', '#868686');
      } else {
        if (l.color) { ctx.fillStyle = l.color; ctx.fillRect(bx + 7, ty - 4, 8, 8); }
        ctx.fillStyle = l.muted ? cssVar('--muted', '#868686') : cssVar('--fg', '#121212');
      }
      ctx.textAlign = 'left';
      ctx.textBaseline = 'middle';
      ctx.fillText(l.text, bx + (l.head ? 7 : 20), ty);
      ty += lh;
    });

    // 各序列取样点画在提示框之上，最后补一次。
    // 横坐标用样本自己的 ts（容差内允许与十字线差一点）：点代表"那一次的
    // 真实测量"，挪到光标处就是在撒谎。
    hits.forEach(function (h) {
      var st = cv._hoverState;
      var px = ax.padL + ax.plotW * ((h.pt.ts - ax.tMin) / (ax.tMax - ax.tMin));
      var y = ax.padT + ax.plotH - ax.plotH * Math.min(1, h.pt.value / (st ? st.top : 1));
      ctx.beginPath();
      ctx.arc(px, y, 3.2, 0, Math.PI * 2);
      ctx.fillStyle = h.s.color;
      ctx.fill();
      ctx.strokeStyle = cssVar('--card', '#ffffff');
      ctx.stroke();
    });
    ctx.restore();
  }

  function drawChart(cv, sets, axisWin, hoverTs) {
    if (!cv) return;
    sets = sets || [];
    initChartHover(cv, sets, axisWin);
    var p = prepare(cv), ctx = p.ctx;
    var live = sets.filter(function (s) { return s.points && s.points.length; });

    if (!live.length) {
      ctx.fillStyle = cssVar('--muted', '#868686');
      ctx.font = '12px sans-serif';
      ctx.textAlign = 'left';
      ctx.textBaseline = 'middle';
      ctx.fillText('该时间范围内没有数据', 12, p.h / 2);
      return;
    }

    var unit = live[0].unit || 'raw';
    var padL = 62, padR = 10, padT = 10, padB = 24;
    var plotW = Math.max(10, p.w - padL - padR);
    var plotH = Math.max(10, p.h - padT - padB);

    var tMin = Infinity, tMax = -Infinity, vMax = 0;
    live.forEach(function (s) {
      s.points.forEach(function (pt) {
        if (pt.ts < tMin) tMin = pt.ts;
        if (pt.ts > tMax) tMax = pt.ts;
        if (pt.value > vMax) vMax = pt.value;
      });
    });
    if (!isFinite(tMin)) return;
    // axisWin 把 X 轴钉在"完整请求窗口"上：数据覆盖不满窗口（新节点、刚启用的
    // 目标、数据层保留期短）时，若按数据实际跨度取刻度，"最近 7 天"会退化成
    // 只画有数据的那一小段，日期标签也随之变成时分。窗口起点取请求范围，
    // 终点取 max(数据终点, 窗口终点)。
    if (axisWin && axisWin.tMin != null) {
      tMin = Math.min(tMin, axisWin.tMin);
      tMax = Math.max(tMax, axisWin.tMax != null ? axisWin.tMax : tMax);
    }
    if (tMax <= tMin) tMax = tMin + 1;
    var span = tMax - tMin;

    var top = niceTop(vMax, unit === 'bytes' || unit === 'bps');
    // 百分比轴画到 120% 没有意义，超过 100% 就压回 100%。
    if (unit === 'pct' && top > 10000) top = 10000;

    // 悬停状态：mousemove 处理器按这份快照把光标 x 换算成时间戳。
    cv._hoverState = {
      tMin: tMin, tMax: tMax, padL: padL, padT: padT, padR: padR,
      plotW: plotW, plotH: plotH, top: top, unit: unit,
      live: live.map(function (s, i) {
        return { points: s.points, name: s.name, unit: s.unit || unit,
                 color: s.color || paletteColor(i) };
      })
    };

    var axis = cssVar('--line', '#dedede');
    var muted = cssVar('--muted', '#868686');

    // ---- 横向网格线 + Y 轴标签 ----
    ctx.font = '11px sans-serif';
    ctx.textAlign = 'right';
    ctx.textBaseline = 'middle';
    for (var i = 0; i <= 4; i++) {
      var y = Math.round(padT + plotH - plotH * i / 4) + .5;
      ctx.strokeStyle = axis;
      ctx.globalAlpha = i === 0 ? 1 : .55;
      ctx.beginPath();
      ctx.moveTo(padL, y);
      ctx.lineTo(padL + plotW, y);
      ctx.stroke();
      ctx.globalAlpha = 1;
      ctx.fillStyle = muted;
      ctx.fillText(axisLabel(top * i / 4, unit), padL - 8, y);
    }

    // ---- X 轴时间刻度 ----
    var ticks = timeTicks(tMin, tMax, Math.max(2, Math.floor(plotW / 110)));
    ctx.textBaseline = 'top';
    ticks.forEach(function (ts, idx) {
      var x = Math.round(padL + plotW * ((ts - tMin) / span)) + .5;
      if (idx > 0 && idx < ticks.length - 1) {
        ctx.strokeStyle = axis;
        ctx.globalAlpha = .3;
        ctx.beginPath();
        ctx.moveTo(x, padT);
        ctx.lineTo(x, padT + plotH);
        ctx.stroke();
        ctx.globalAlpha = 1;
      }
      // 贴边的刻度要改成左/右对齐。居中对齐时，落在绘图区最右侧的那个刻度会有
      // 一半文字伸到画布外被裁掉——"19:25"读成"19:2"。数据只覆盖一小段时
      // （比如刚接上探测的目标）最后一个刻度正好压在最右端，必然被裁。
      var align = 'center';
      if (x - padL < 26) align = 'left';
      else if (padL + plotW - x < 26) align = 'right';
      ctx.textAlign = align;
      ctx.fillStyle = muted;
      ctx.fillText(timeLabel(ts, span), x, padT + plotH + 7);
    });

    // ---- 折线 + 面积 ----
    //
    // 断档处理：断联期间没有样本，序列里那一段就是空的。按 gapThreshold
    // 把每条序列切成连续段，段与段之间**不连线**——否则断联会画成一条
    // 平滑的水平线，故障被渲染成健康。面积也按同样的分段填充，不然会在
    // 断档区间留下一块底色。
    live.forEach(function (s, idx) {
      var color = s.color || paletteColor(idx);
      // 线型只由序列自己声明（s.dash，null = 实线）：资源图的序列不声明，
      // 就保持实线；延迟图按序号取 PALETTE_DASH。绝不在这里兜底分配，
      // 否则单序列的资源曲线也会被无端画成虚线。
      var dash = s.dash || null;
      var segs = segmentsOf(s.points, gapThreshold(s.points), function (pt) {
        return [padL + plotW * ((pt.ts - tMin) / span),
                padT + plotH - plotH * Math.min(1, pt.value / top)];
      });

      // 面积只在单序列时填：多条曲线各填一层会糊成一片，反而看不清。
      if (live.length === 1) {
        ctx.globalAlpha = .14;
        ctx.fillStyle = color;
        segs.forEach(function (seg) {
          if (seg.length < 2) return;
          ctx.beginPath();
          ctx.moveTo(seg[0][0], padT + plotH);
          seg.forEach(function (q) { ctx.lineTo(q[0], q[1]); });
          ctx.lineTo(seg[seg.length - 1][0], padT + plotH);
          ctx.closePath();
          ctx.fill();
        });
        ctx.globalAlpha = 1;
      }

      ctx.strokeStyle = color;
      ctx.lineWidth = live.length > 1 ? 1.4 : 1.8;
      ctx.lineJoin = 'round';
      ctx.lineCap = 'round';
      if (dash && dash.length) ctx.setLineDash(dash); else ctx.setLineDash([]);
      segs.forEach(function (seg) {
        // 单点段画不出线段（moveTo 后没有 lineTo）；用极短的横线顶一下，
        // 让"孤立的一次测量"仍然可见，而不是彻底消失。
        ctx.beginPath();
        if (seg.length === 1) {
          ctx.moveTo(seg[0][0] - .6, seg[0][1]);
          ctx.lineTo(seg[0][0] + .6, seg[0][1]);
        } else {
          ctx.moveTo(seg[0][0], seg[0][1]);
          for (var k = 1; k < seg.length; k++) ctx.lineTo(seg[k][0], seg[k][1]);
        }
        ctx.stroke();
      });
      ctx.setLineDash([]);

      // 末点：一眼看出"当前值在哪"。取**最后一段**的末点——若最后一段
      // 只有一个孤立点，它同样就是当前值。
      var tail = segs.length ? segs[segs.length - 1] : null;
      if (tail && tail.length) {
        var lastPt = tail[tail.length - 1];
        ctx.beginPath();
        ctx.arc(lastPt[0], lastPt[1], 2.6, 0, Math.PI * 2);
        ctx.fillStyle = color;
        ctx.fill();
      }
    });

    // ---- 多序列图例（带当前值）----
    if (sets.length > 1) {
      var rows = sets.length;
      var lx = padL + 8, ly = padT + 12;
      ctx.textAlign = 'left';
      ctx.textBaseline = 'middle';
      ctx.font = '11px sans-serif';

      // 先铺一层背景，免得图例压在曲线上看不清
      ctx.globalAlpha = .82;
      ctx.fillStyle = cssVar('--card', '#ffffff');
      ctx.fillRect(lx - 6, ly - 11, 210, rows * 15 + 8);
      ctx.globalAlpha = 1;

      var fg = cssVar('--fg', '#121212');
      sets.forEach(function (s, idx) {
        var color = s.color || paletteColor(idx);
        // 色样与曲线同线型：灰阶主题里两条曲线可能只差一个虚线样式，
        // 色样若是实心方块就传达不出这一点（参考主题图例的画法）。
        ctx.strokeStyle = color;
        ctx.lineWidth = 2;
        ctx.lineCap = 'butt';
        if (s.dash && s.dash.length) ctx.setLineDash(s.dash); else ctx.setLineDash([]);
        ctx.beginPath();
        ctx.moveTo(lx, ly);
        ctx.lineTo(lx + 12, ly);
        ctx.stroke();
        ctx.setLineDash([]);
        ctx.fillStyle = fg;
        var txt = (s.name || ('序列 ' + (idx + 1))) + '  ';
        txt += s.points && s.points.length
          ? fmtVal(s.points[s.points.length - 1].value, s.unit || unit)
          : '无数据';
        ctx.fillText(txt, lx + 17, ly);
        ly += 15;
      });
    }

    drawChartHover(cv, ctx, hoverTs, { tMin: tMin, tMax: tMax, padL: padL, padT: padT,
      plotW: plotW, plotH: plotH, span: span, live: cv._hoverState ? cv._hoverState.live : [] });
  }

  // -------------------------------------------------------------------------
  // 路由与轮询
  // -------------------------------------------------------------------------

  // setOverviewChrome 切换"只有总览才需要"的那部分界面（分组筛选 + 四张汇总卡）。
  //
  // 详情页上留着它们有两处不好：一来把真正要看的曲线挤到首屏之外，得先滚一屏；
  // 二来会误导——汇总卡里的"实时网速"是全站合计、"最忙节点"是别的机器，
  // 和下面这台机器的曲线并排放着，很容易被当成同一台机器的两个视图。
  function setOverviewChrome(show) {
    overviewChrome = show;
    syncOverviewChrome();
  }

  function setDetailTab(tab) {
    detailTab = tab === 'latency' ? 'latency' : 'resources';
    var tabs = $('detail-tabs');
    if (tabs) {
      for (var i = 0; i < tabs.children.length; i++) {
        var b = tabs.children[i];
        b.classList.toggle('active', b.dataset.tab === detailTab);
      }
    }
    var resPanel = $('tab-resources');
    var latPanel = $('tab-latency');
    if (resPanel) resPanel.hidden = detailTab !== 'resources';
    if (latPanel) latPanel.hidden = detailTab !== 'latency';
    if (current) refreshDetail();
  }

  function route() {
    var h = location.hash || '';
    var m = h.match(/^#\/n\/(.+)$/);
    if (m) {
      current = decodeURIComponent(m[1]);
      $('list-view').hidden = true;
      $('detail-view').hidden = false;
      setOverviewChrome(false);
      refreshDetail();
    } else {
      current = null;
      $('detail-view').hidden = true;
      $('list-view').hidden = false;
      setOverviewChrome(true);
      refreshList();
    }
  }

  $('btn-back').addEventListener('click', function () { location.hash = ''; });
  $('range-select').addEventListener('change', refreshDetail);
  var detailTabs = $('detail-tabs');
  if (detailTabs) {
    detailTabs.addEventListener('click', function (ev) {
      if (ev.target && ev.target.dataset && ev.target.dataset.tab) {
        setDetailTab(ev.target.dataset.tab);
      }
    });
  }
  window.addEventListener('hashchange', route);

  // 视口尺寸变化后必须重画：Canvas 的像素尺寸是在绘制时按 clientWidth 定死的，
  // CSS 只是把位图拉伸，不重画就是一张糊图。做 200ms 防抖，避免拖拽窗口时
  // 每个像素都触发一次重绘与请求。
  var resizeTimer = 0;
  window.addEventListener('resize', function () {
    drawSpark();
    if (resizeTimer) clearTimeout(resizeTimer);
    resizeTimer = setTimeout(function () {
      resizeTimer = 0;
      if (current) refreshDetail(); else refreshList();
    }, 200);
  });

  // -------------------------------------------------------------------------
  // 深浅色切换
  // -------------------------------------------------------------------------
  // CSS 端的约定（style.css「手动主题切换」一节）：<html> 上没有 data-theme
  // 时跟随系统（prefers-color-scheme），写了 'light'/'dark' 就是访客的显式
  // 选择。只有切换动作落 localStorage（key: probe.theme）——**启动时绝不**
  // 把系统答案写进去，否则访客没碰过按钮也会被钉死在加载那天的系统模式
  // （参考主题 useTheme 的原注释）。canvas 里的颜色取的是 CSS 变量实色，
  // 所以切换后要立刻 tick() 重画一遍。
  //
  // ?theme=dark|light 查询参数只作用于本次页面加载、不落盘：给截图与
  // 排障用，也方便把"某个模式下的看板"直接链接给别人。
  var THEME_KEY = 'probe.theme';

  function systemDark() {
    return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
  }
  function currentTheme() {
    var t = document.documentElement.getAttribute('data-theme');
    return t === 'dark' || t === 'light' ? t : (systemDark() ? 'dark' : 'light');
  }
  function syncThemeBtn() {
    // 图标（太阳/月亮）由 CSS 按 data-theme / 系统偏好切换显示，
    // JS 只负责把"要去往的模式"写进无障碍标签。
    var btn = document.getElementById('btn-theme');
    if (btn) {
      var next = currentTheme() === 'dark' ? '浅色' : '深色';
      btn.setAttribute('aria-label', '切换到' + next);
      btn.setAttribute('title', '切换到' + next);
    }
  }
  function applyTheme(t) {
    if (t === 'dark' || t === 'light') {
      document.documentElement.setAttribute('data-theme', t);
    } else {
      document.documentElement.removeAttribute('data-theme');
    }
    syncThemeBtn();
  }

  (function initTheme() {
    var q = '';
    try { q = new URLSearchParams(location.search).get('theme') || ''; } catch (e) { }
    var saved = null;
    try { saved = localStorage.getItem(THEME_KEY); } catch (e) { }
    var t = (q === 'dark' || q === 'light') ? q
          : (saved === 'dark' || saved === 'light') ? saved
          : ''; // 跟随系统：不写 data-theme，交给 CSS 的媒体查询
    if (t) applyTheme(t); else syncThemeBtn();
    var btn = document.getElementById('btn-theme');
    if (btn) btn.addEventListener('click', function () {
      var next = currentTheme() === 'dark' ? 'light' : 'dark';
      applyTheme(next);
      try { localStorage.setItem(THEME_KEY, next); } catch (e) { }
      tick();
    });
  })();

  function tick() {
    if (current) refreshDetail();
    else refreshList();
  }

  function loadSite() {
    getJSON('/site').then(function (s) {
      if (s.title) {
        document.title = s.title;
        setText($('site-title'), s.title);
      }
      if (s.lat_window) latWindow = s.lat_window;
      if (s.admin_url) applyAdminURL(s.admin_url);
      applyCards(s.cards);
    }).catch(function () { });
  }

  // applyAdminURL 把顶栏「管理后台」按钮指到服务端算好的地址。
  //
  // 为什么不能写死 /admin/：公开看板与后台很可能不在同一个源上（不同端口、
  // 不同域名），此时 /admin/ 会跳到公开站自己的路径下，得到 404 或 421——
  // 现象是"点登录按钮打不开后台"。
  //
  // 只在 /site 明确给出绝对地址时才改：拿不到就保留 HTML 里写好的 /admin/
  // （同站部署下它本来就是对的）。改不动也不报错——一个跳转链接不该
  // 让整页停下来。
  function applyAdminURL(url) {
    var btn = $('btn-login');
    if (!btn) return;
    // 服务端已经限定了 http/https 前缀与字符集；这里再确认一次，
    // 因为 href 是可执行上下文（javascript: 前缀会在点击时执行）。
    if (!/^https?:\/\/[A-Za-z0-9._:\/-]+$/.test(url)) return;
    btn.setAttribute('href', url);
  }

  loadSite();
  route();
  setupRefreshInterval();

  // 刷新间隔选择：存 localStorage，改选即刻生效（重建轮询计时器）。
  // 固定的 setInterval(POLL_MS) 没法响应下拉框变化，所以封装在这里。
  function setupRefreshInterval() {
    var sel = document.getElementById('refresh-interval');
    if (!sel) return;
    var KEY = 'probe.refresh_s';
    var saved = parseInt(localStorage.getItem(KEY), 10);
    if ([1, 3, 5, 10, 30].indexOf(saved) >= 0) sel.value = String(saved);
    var timer = 0;
    function schedule() {
      if (timer) clearInterval(timer);
      timer = setInterval(tick, parseInt(sel.value, 10) * 1000);
    }
    sel.addEventListener('change', function () {
      localStorage.setItem(KEY, sel.value);
      schedule();
    });
    schedule();
  }
})();
