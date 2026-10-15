// XB-Probe 管理后台前端（固定界面，5.2）。
//
// 约束（13.4.5）：CSP 为 script-src 'self'; style-src 'self'，因此：
//   - 不加载任何 CDN 资源，图表用手绘 Canvas；
//   - 不使用内联 <script> / <style> 与 HTML 中的 style="..." 属性
//     （JS 里对 element.style.x 赋值属于 CSSOM 操作，不受 CSP 限制）；
//   - 不可信字符串一律用 textContent 渲染，禁止 innerHTML 拼接（13.4.4）。
(function () {
  'use strict';

  var API = '/api/admin/v1';
  var CSRF_HEADER = 'X-CSRF-Token';

  var state = {
    csrf: '',
    me: '',
    nodes: [],
    node: null,
    settings: {},
    // 公开看板卡片目录（来自 GET /settings/meta）与"不限制"哨兵值。
    // 目录由后端下发而不是前端写死，避免加了新卡片后勾选框里没有它。
    cardCatalog: [],
    cardsAll: 'all',
    themeList: [],
    rules: {},
    // 探测目标 id -> 名称。节点详情的"最新一轮探测"与延迟面板都要显示真实
    // 目标名，而运行态里只有 target_id。缓一份映射，避免每次渲染都发请求。
    targetNames: {},
    // 延迟图例的折线开关（target_id -> 是否显示；缺省 true）。
    latToggles: {},
    // 节点详情页「历史曲线」当前标签：resources / latency
    nodeHistoryTab: 'resources',
    // 是否已启用 TOTP：决定二次确认对话框要不要显示"动态验证码/恢复码"输入框。
    // 登录后由 GET /maintenance/totp 填充（见 refreshTotpStatus）。
    totpEnabled: false
  };

  // -------------------------------------------------------------------------
  // 基础工具
  // -------------------------------------------------------------------------

  function $(id) { return document.getElementById(id); }

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  }

  function td(text, cls) { return el('td', cls, text); }

  function btn(label, cls, onClick) {
    var b = el('button', cls || 'ghost mini', label);
    b.type = 'button';
    b.addEventListener('click', onClick);
    return b;
  }

  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

  // ipListText 把多地址清单（数组）折叠成展示串；清单为空时回落到主地址，再不行返回空。
  function ipListText(list, primary) {
    if (Array.isArray(list) && list.length) return list.join(', ');
    return primary || '';
  }

  // splitIPInput 把多行/逗号分隔的输入解析为去空白、去空项的地址数组。
  function splitIPInput(s) {
    return String(s || '').split(/[\n,]/).map(function (x) { return x.trim(); })
      .filter(function (x) { return x !== ''; });
  }

  // svgNode 把服务端返回的 SVG 字符串解析成 DOM 节点。
  //
  // 用 DOMParser 而不是 innerHTML：一是 innerHTML 在 CSP 下虽不直接受限，
  // 但本项目约定"不可信字符串一律走 textContent、禁止 innerHTML 拼接"；
  // 二是这里的内容来自服务端、又有固定结构，用 XML 解析能明确拿到 <svg> 根节点，
  // 插进文档后由浏览器按 SVG 命名空间渲染（innerHTML 需要额外的命名空间补丁）。
  // 解析失败时返回一个提示节点，绝不让异常冒出去中断整个绑定流程。
  function svgNode(svgText) {
    try {
      var doc = new DOMParser().parseFromString(svgText, 'image/svg+xml');
      var root = doc && doc.documentElement;
      if (!root || root.nodeName === 'parsererror' ||
        String(root.nodeName).toLowerCase() !== 'svg') {
        return el('p', 'muted note', '二维码解析失败，请使用下方密钥手动添加。');
      }
      // importNode 到当前文档，否则节点属于另一个 document，插入时会有兼容问题。
      return document.importNode(root, true);
    } catch (e) {
      return el('p', 'muted note', '二维码解析失败，请使用下方密钥手动添加。');
    }
  }

  // 探测目标运营组的中文名（与后端模型里的 carrier 枚举一一对应）。
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
    KE: ['肯尼亚', 'h:#000000,#BB0000,#006600'],
    PK: ['巴基斯坦', '<rect width="20" height="14" fill="#01411C"/><rect width="5.4" height="14" fill="#FFFFFF"/><circle cx="13.6" cy="7" r="3.4" fill="#FFFFFF"/><circle cx="15.1" cy="6.2" r="2.9" fill="#01411C"/><polygon points="' + starPts(16.6, 3.4, 1.3) + '" fill="#FFFFFF"/>'],
    BD: ['孟加拉国', '<rect width="20" height="14" fill="#006A4E"/><circle cx="8.4" cy="7" r="4.2" fill="#F42A41"/>'],
    KH: ['柬埔寨', '<rect width="20" height="14" fill="#032EA1"/><rect y="4" width="20" height="6" fill="#E00025"/><rect y="10" width="20" height="4" fill="#032EA1"/>'],
    MM: ['缅甸', 'h:#FECB00,#34B233,#EA2839'],
    LA: ['老挝', '<rect width="20" height="14" fill="#CE1126"/><rect y="3.5" width="20" height="7" fill="#002868"/><circle cx="10" cy="7" r="3" fill="#FFFFFF"/>'],
    BN: ['文莱', '<rect width="20" height="14" fill="#F7E017"/><path d="M0 3.4l20 5.4M0 8.8l20-5.4" stroke="#FFFFFF" stroke-width="2.4"/><path d="M0 4.4l20 5.4M0 9.8l20-5.4" stroke="#000000" stroke-width=".8"/>'],
    KZ: ['哈萨克斯坦', '<rect width="20" height="14" fill="#00AFCA"/><circle cx="10" cy="7" r="2.8" fill="#FEC50C"/>'],
    UZ: ['乌兹别克斯坦', 'h:#0099B5,#FFFFFF,#1EB53A'],
    AZ: ['阿塞拜疆', 'h:#0092BC,#E4002B,#00AE65'],
    GE: ['格鲁吉亚', '<rect width="20" height="14" fill="#FFFFFF"/><rect y="5.6" width="20" height="2.8" fill="#FF0000"/><rect x="8.6" width="2.8" height="14" fill="#FF0000"/>'],
    AM: ['亚美尼亚', 'h:#D90012,#0033A0,#F2A800'],
    QA: ['卡塔尔', '<rect width="20" height="14" fill="#FFFFFF"/><path d="M5 0l2 1.4-2 1.4 2 1.4-2 1.4 2 1.4-2 1.4 2 1.4-2 1.4 2 1.4-2 1.4h15V0z" fill="#8A1538"/>'],
    KW: ['科威特', '<rect width="20" height="14" fill="#FFFFFF"/><rect width="20" height="4.67" fill="#007A3D"/><rect y="9.33" width="20" height="4.67" fill="#CE1126"/><path d="M0 0l5.6 7L0 14z" fill="#000000"/>'],
    BH: ['巴林', '<rect width="20" height="14" fill="#FFFFFF"/><path d="M5 0l2 1.4-2 1.4 2 1.4-2 1.4 2 1.4-2 1.4 2 1.4-2 1.4 2 1.4-2 1.4h15V0z" fill="#CE1126"/>'],
    CY: ['塞浦路斯', '<rect width="20" height="14" fill="#FFFFFF"/><path d="M4.6 8.6c1.4-1.6 3-2.4 4.2-2 .9.3 1.2 1.4 1.6 1.9.3.4 1 .5 1.6.2.7-.3 1.5-.4 2.2 0 .6.3 1 .9 1.2 1.5" stroke="#D57800" stroke-width="1.2" fill="none" stroke-linecap="round"/>'],
    MT: ['马耳他', '<rect width="20" height="14" fill="#FFFFFF"/><rect x="10" width="10" height="14" fill="#CF142B"/><rect x="3.4" y="1.4" width="3.2" height="4" fill="#FFFFFF" stroke="#7F7F7F" stroke-width=".4"/><path d="M5 1.9v3M3.9 3.4h2.2" stroke="#7F7F7F" stroke-width=".5"/>'],
    JO: ['约旦', '<rect width="20" height="14" fill="#FFFFFF"/><rect width="20" height="4.67" fill="#000000"/><rect y="9.33" width="20" height="4.67" fill="#007A3D"/><path d="M0 0l7 7L0 14z" fill="#CE1126"/><polygon points="' + starPts(3.1, 7, 1.5) + '" fill="#FFFFFF"/>'],
    LB: ['黎巴嫩', '<rect width="20" height="14" fill="#FFFFFF"/><rect width="20" height="3.4" fill="#EE161F"/><rect y="10.6" width="20" height="3.4" fill="#EE161F"/><path d="M10 3.8l1.2 2 2-.3-1.1 1.6 1.1 1.6-2-.3L10 10.2 8.8 8.4l-2 .3L7.9 7.1 6.8 5.5l2 .3z" fill="#00A651"/>'],
    IQ: ['伊拉克', 'h:#CE1126,#FFFFFF,#000000'],
    BY: ['白俄罗斯', '<rect width="20" height="14" fill="#C8313E"/><rect y="9" width="20" height="5" fill="#4AA657"/><rect width="3.6" height="14" fill="#FFFFFF"/><path d="M0 .6l3.6 1.2M0 3.2l3.6 1.2M0 5.8l3.6 1.2M0 8.4l3.6 1.2M0 11l3.6 1.2M0 13.2l3.6 1.2" stroke="#C8313E" stroke-width="1"/>'],
    RS: ['塞尔维亚', 'h:#C6363C,#0C4076,#FFFFFF'],
    HR: ['克罗地亚', 'h:#FF0000,#FFFFFF,#171796'],
    SI: ['斯洛文尼亚', 'h:#FFFFFF,#005DA4,#ED1C24'],
    SK: ['斯洛伐克', 'h:#FFFFFF,#0B4EA2,#EE1C25'],
    MD: ['摩尔多瓦', 'v:#003DA5,#FFD200,#C8102E'],
    UY: ['乌拉圭', '<rect width="20" height="14" fill="#FFFFFF"/><g fill="#0038A8"><rect width="20" height="1.75"/><rect y="3.5" width="20" height="1.75"/><rect y="7" width="20" height="1.75"/><rect y="10.5" width="20" height="1.75"/></g><rect width="7.6" height="7" fill="#FFFFFF"/><circle cx="3.8" cy="3.5" r="1.9" fill="#FEDF00"/>'],
    PY: ['巴拉圭', 'h:#D52B1E,#FFFFFF,#0038A8'],
    BO: ['玻利维亚', 'h:#D52B1E,#F9E300,#007934'],
    EC: ['厄瓜多尔', '<rect width="20" height="14" fill="#FFDD00"/><rect y="7" width="20" height="3.5" fill="#0055A4"/><rect y="10.5" width="20" height="3.5" fill="#EF3340"/>'],
    VE: ['委内瑞拉', 'h:#FFCC00,#00247D,#CF142B'],
    CR: ['哥斯达黎加', '<rect width="20" height="14" fill="#002B7F"/><rect y="2.3" width="20" height="9.4" fill="#FFFFFF"/><rect y="4.7" width="20" height="4.7" fill="#CE1126"/>'],
    PA: ['巴拿马', '<rect width="20" height="14" fill="#FFFFFF"/><rect x="10" width="10" height="7" fill="#DA121A"/><rect y="7" width="10" height="7" fill="#005293"/>'],
    GT: ['危地马拉', 'v:#4997D0,#FFFFFF,#4997D0'],
    PR: ['波多黎各', '<rect width="20" height="14" fill="#FFFFFF"/><g fill="#ED0000"><rect width="20" height="2.8"/><rect y="5.6" width="20" height="2.8"/><rect y="11.2" width="20" height="2.8"/></g><path d="M0 0l8.4 7L0 14z" fill="#0050F0"/><polygon points="' + starPts(2.7, 7, 1.5) + '" fill="#FFFFFF"/>'],
    JM: ['牙买加', '<path d="M0 0h20v14H0z" fill="#FED100"/><path d="M0 0l20 14M20 0L0 14" stroke="#000000" stroke-width="3.6"/><path d="M0 0l20 14M20 0L0 14" stroke="#009B3A" stroke-width="1.6"/>'],
    MA: ['摩洛哥', '<rect width="20" height="14" fill="#C1272D"/><polygon points="' + starPts(10, 7, 3.3) + '" fill="none" stroke="#006233" stroke-width=".7"/>'],
    TN: ['突尼斯', '<rect width="20" height="14" fill="#E70013"/><circle cx="10" cy="7" r="3.6" fill="#FFFFFF"/><circle cx="11.2" cy="7" r="2.9" fill="#E70013"/><polygon points="' + starPts(11.6, 7, 1.6) + '" fill="#E70013"/>'],
    DZ: ['阿尔及利亚', '<rect width="20" height="7" fill="#006233"/><rect y="7" width="20" height="7" fill="#FFFFFF"/><circle cx="10.4" cy="7" r="3.4" fill="#D21034"/><circle cx="11.6" cy="7" r="2.8" fill="#FFFFFF"/><polygon points="' + starPts(12, 7, 1.5) + '" fill="#D21034"/>'],
    GH: ['加纳', 'h:#CE1126,#FCD116,#006B3F'],
    ET: ['埃塞俄比亚', 'h:#078930,#FCDD09,#DA121A'],
    TZ: ['坦桑尼亚', '<rect width="20" height="14" fill="#1EB53A"/><path d="M20 14H0L20 0z" fill="#00A3DD"/><path d="M20 0L0 14" stroke="#000000" stroke-width="3.6"/>'],
    MU: ['毛里求斯', 'h:#EA2839,#1A206D,#FFD500,#00A551'],
    FJ: ['斐济', '<rect width="20" height="14" fill="#68BFE5"/><rect width="9.4" height="7" fill="#FFFFFF"/><path d="M0 0l9.4 7M9.4 0L0 7" stroke="#C8102E" stroke-width="1.5"/><rect x="4.1" width="1.2" height="7" fill="#C8102E"/><rect y="2.9" width="9.4" height="1.2" fill="#C8102E"/><circle cx="15.2" cy="7" r="2.4" fill="#FFFFFF"/>']
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
  // 国家/地区关键词表：分组名或节点名里出现这些词时，卡片自动挂上对应国旗。
  //
  // 为什么放在公共区块里：两个前端（公开看板与后台）各有一份同样的卡片渲染，
  // 只改一处就会出现"看板有旗、后台没有"；表也必须在两边逐字节一致
  // （internal/theme/flag_test.go 钉住整个区块）。
  //
  // 匹配规则见 matchCountryHint：中文与多词英文按子串匹配（"洛杉矶"），
  // 两位代码这类短词按**独立词**匹配（避免 us 命中 rust、hk 命中某串）。
  // 表里刻意不收会大量误判的短代码——in / it / my / id / no / is：
  // 它们在节点名里更可能只是英文单词或序号，这些国家改用中文名或全名匹配。
  //
  // 只在"管理员没设过国家/地区"时兜底（见 countryOf）：显式设置永远优先。
  // 数组顺序即优先级，靠前的先命中。
  var COUNTRY_HINTS = [
    ['US', ['美国', '洛杉矶', '圣何塞', '西雅图', '达拉斯', '纽约', '芝加哥', '凤凰城', '硅谷', '迈阿密', 'us', 'usa', 'united states', 'los angeles', 'san jose', 'seattle', 'dallas', 'new york']],
    ['HK', ['香港', 'hongkong', 'hong kong', 'hk']],
    ['TW', ['台湾', '台北', 'taiwan', 'taipei', 'tw']],
    ['JP', ['日本', '东京', '大阪', 'japan', 'tokyo', 'osaka', 'jp']],
    ['KR', ['韩国', '首尔', 'korea', 'seoul', 'kr']],
    ['SG', ['新加坡', '狮城', 'singapore', 'sg']],
    ['MY', ['马来西亚', '吉隆坡', 'malaysia', 'kuala lumpur']],
    ['TH', ['泰国', '曼谷', 'thailand', 'bangkok', 'th']],
    ['VN', ['越南', '胡志明', 'vietnam', 'vn']],
    ['PH', ['菲律宾', '马尼拉', 'philippines', 'manila', 'ph']],
    ['ID', ['印度尼西亚', '印尼', '雅加达', 'indonesia', 'jakarta']],
    ['IN', ['印度', '孟买', '班加罗尔', 'india', 'mumbai']],
    ['AU', ['澳大利亚', '澳洲', '悉尼', '墨尔本', 'australia', 'sydney', 'melbourne', 'au']],
    ['NZ', ['新西兰', 'new zealand', 'nz']],
    ['CA', ['加拿大', '多伦多', '蒙特利尔', 'canada', 'toronto', 'montreal', 'ca']],
    ['BR', ['巴西', '圣保罗', 'brazil', 'sao paulo', 'br']],
    ['DE', ['德国', '法兰克福', 'germany', 'frankfurt', 'de']],
    ['FR', ['法国', '巴黎', 'france', 'paris', 'fr']],
    ['GB', ['英国', '伦敦', 'uk', 'united kingdom', 'britain', 'london']],
    ['NL', ['荷兰', '阿姆斯特丹', 'netherlands', 'amsterdam', 'nl']],
    ['RU', ['俄罗斯', '莫斯科', 'russia', 'moscow', 'ru']],
    ['TR', ['土耳其', '伊斯坦布尔', 'turkey', 'istanbul', 'tr']],
    ['AE', ['阿联酋', '迪拜', 'emirates', 'dubai', 'ae']],
    ['SE', ['瑞典', '斯德哥尔摩', 'sweden', 'stockholm', 'se']],
    ['CH', ['瑞士', '苏黎世', 'switzerland', 'zurich', 'ch']],
    ['IT', ['意大利', '米兰', 'italy', 'milan']],
    ['ES', ['西班牙', '马德里', 'spain', 'madrid', 'es']],
    ['PL', ['波兰', '华沙', 'poland', 'warsaw', 'pl']],
    ['UA', ['乌克兰', 'ukraine', 'ua']],
    ['ZA', ['南非', 'south africa', 'za']],
    ['IE', ['爱尔兰', '都柏林', 'ireland', 'dublin', 'ie']],
    ['FI', ['芬兰', '赫尔辛基', 'finland', 'helsinki', 'fi']],
    ['NO', ['挪威', 'norway']],
    ['AT', ['奥地利', '维也纳', 'austria', 'vienna']]
  ];

  // matchCountryHint 判断一段（已小写化的）文本里是否出现了某个提示词。
  //
  // ASCII 提示词走"独立词"正则：两侧不能是字母数字。少了这一条，
  // us 会在 "rust" 里命中、hk 会在任何含这两个字母的串里命中——旗子就画错了，
  // 而且错得不报错、只能靠肉眼发现。
  // 中文（以及带空格的多词英文）走子串匹配：它们本身就不可能是别的词的一部分。
  var countryHintRe = {};
  function matchCountryHint(s, hint) {
    if (/[^\x00-\x7f]/.test(hint)) return s.indexOf(hint) >= 0;
    var re = countryHintRe[hint];
    if (!re) {
      re = new RegExp('(^|[^a-z0-9])' + hint + '([^a-z0-9]|$)');
      countryHintRe[hint] = re;
    }
    return re.test(s);
  }

  // inferCountry 从分组名与节点名推断国家/地区代码；推不出来就返回空串
  // （空串 = 不画旗，而不是画一面猜的旗）。
  function inferCountry(name, group) {
    var s = (String(group || '') + ' ' + String(name || '')).toLowerCase();
    if (!s.replace(/\s/g, '')) return '';
    for (var i = 0; i < COUNTRY_HINTS.length; i++) {
      var hints = COUNTRY_HINTS[i][1];
      for (var k = 0; k < hints.length; k++) {
        if (matchCountryHint(s, hints[k])) return COUNTRY_HINTS[i][0];
      }
    }
    return '';
  }

  // countryOf 决定卡片/详情页该画哪面旗：管理员设过就用它，否则按分组名、节点名推断。
  //
  // 只在**显示层**兜底，不回写数据库：分组叫"美国-洛杉矶"的节点立刻就有旗，
  // 而管理员一旦在节点设置里明确选过国家，那个选择永远优先（也不怕设置被覆盖）。
  function countryOf(n) {
    if (!n) return '';
    if (n.country) return n.country;
    return inferCountry(n.name, n.group);
  }

  // -- 国旗：FLAG-COMMON-END --

  // renderFlag 渲染国家/地区标识（与公开看板同名同行为）。
  // 未设置不画；内置 FLAGS 表里没有的代码（含管理员自定义的地区）画不出旗，
  // 就以两位代码作为标识——这是管理员明确设置过的值，不是"缺值占位"。
  function renderFlag(box, code) {
    if (!box) return;
    if (box._code === code) return;
    box._code = code;
    box.textContent = '';
    box.removeAttribute('title');
    if (!code) return;
    var svg = svgNode(flagSVG(code));
    if (!svg) {
      var fb = el('span', 'n-flag-code', code);
      box.appendChild(fb);
      box.title = countryLabel(code);
      return;
    }
    box.appendChild(svg);
    box.title = countryLabel(code);
  }

  // customCountryMap 解析「自定义国家/地区」设置：每行 `代码|名称`。
  // 只认两位大写字母的代码；坏行跳过而不是让整个清单失效。
  function customCountryMap() {
    var out = {};
    String(state.settings && state.settings['node.custom_countries'] || '')
      .split('\n').forEach(function (ln) {
        ln = ln.trim();
        if (!ln) return;
        var i = ln.indexOf('|');
        var code = (i < 0 ? ln : ln.slice(0, i)).trim().toUpperCase();
        var name = i < 0 ? '' : ln.slice(i + 1).trim();
        if (/^[A-Z]{2}$/.test(code)) out[code] = name || code;
      });
    return out;
  }

  // countryLabel 把两位国家代码转成中文名（自定义地区用管理员填的名称，
  // 两边都没有就原样回显代码）。
  function countryLabel(code) {
    if (!code) return '—';
    var e = FLAGS[code];
    if (e) return e[0];
    var custom = customCountryMap();
    return custom[code] || code;
  }

  var CARRIER = { telecom: '电信', unicom: '联通', mobile: '移动', custom: '自定义' };

  // 探测状态与节点状态的中文文案（与公开看板共用一套，避免两边说法不一致）。
  var STATUS_NAME = {
    ok: '正常', warn: '偏高', bad: '很高', unknown: '无数据',
    online: '在线', offline: '离线', warning: '疑似离线'
  };

  // 审计日志 action 中文映射。key 与后端审计动作字符串一致。
  var AUDIT_ACTION = {
    'node.create': '创建节点', 'node.update': '更新节点', 'node.delete': '删除节点',
    'node.archive': '归档节点',
    'target.create': '创建探测目标', 'target.update': '更新探测目标', 'target.delete': '删除探测目标',
    'channel.create': '创建告警渠道', 'channel.update': '更新告警渠道', 'channel.delete': '删除告警渠道',
    'rule.create': '创建告警规则', 'rule.update': '更新告警规则', 'rule.delete': '删除告警规则',
    'settings.update': '更新系统设置', 'billing.update': '更新计费信息',
    'billing.renew': '续费', 'traffic.override': '矫正本周期已用流量',
    'auth.login': '登录', 'auth.logout': '登出', 'auth.password': '修改密码',
    'auth.login_fail': '登录失败',
    'auth.totp_enable': '启用 TOTP',
    'auth.totp_disable': '关闭 TOTP',
    'auth.totp_disable_fail': '关闭 TOTP 失败',
    'auth.recovery_regen': '重新生成恢复码',
    'auth.recovery_regen_fail': '重新生成恢复码失败',
    'token.issue': '签发 Agent 令牌', 'token.revoke': '吊销 Agent 令牌',
    'enroll_code.create': '创建注册码', 'enroll_code.consume': '使用注册码注册',
    'release.upload': '上传 Agent 发布文件', 'release.delete': '删除 Agent 发布文件',
    'theme.set': '切换主题', 'maintenance.backup': '手动备份数据库',
    'maintenance.backup_auto': '自动备份数据库',
    'maintenance.backup_prune': '清理过期备份',
    'maintenance.backup_delete': '删除备份文件',
    'maintenance.backup_restore': '从备份恢复数据库',
    'maintenance.data_purge': '清理历史时序数据'
  };

  // AUDIT_ENTITY 把动作前缀映射到被操作对象的中文类型名。
  //
  // 库里的 audit.target 只存了 ID（一串数字），光看表格完全不知道指的是什么；
  // 而动作名本身就带了实体类型，补上前缀即可还原成"节点 5"这种可读形式，
  // 不必改动已落库的历史记录。
  var AUDIT_ENTITY = {
    node: '节点', target: '探测目标', channel: '告警渠道', rule: '告警规则',
    token: 'Agent 令牌', enroll_code: '注册码', release: '发布文件',
    settings: '系统设置', billing: '节点计费', theme: '主题',
    auth: '账号', maintenance: '数据库'
  };

  function auditTargetText(action, target) {
    var raw = target == null ? '' : String(target).trim();
    if (!raw) return '—';
    var prefix = String(action || '').split('.')[0];
    var name = AUDIT_ENTITY[prefix];
    if (!name) return raw;
    return name + ' ' + raw;
  }

  function fmtBytes(v) {
    if (v == null || v === 0) return '0 B';
    var u = ['B', 'KB', 'MB', 'GB', 'TB'], i = 0;
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return v.toFixed(i ? 1 : 0) + ' ' + u[i];
  }

  function fmtBps(v) {
    if (v == null || v === 0) return '0 B/s';
    return fmtBytes(v) + '/s';
  }

  function pct(v) { return v == null ? '-' : (v / 100).toFixed(1) + '%'; }

  // fmtMoney 把"最小货币单位"整数格式化为人类可读金额。
  //
  // 与公开主题 app.js 里同名函数是**有意的两份**：两个前端各自独立、
  // 不共享代码（公开主题可被替换，后台样式固定）。改这里请同步改那边。
  function fmtMoney(minor, currency) {
    if (minor === undefined || minor === null) return '—';
    var sym = { CNY: '¥', USD: '$', EUR: '€', HKD: 'HK$', JPY: '¥' }[currency] || '¥';
    var major = Number(minor) / 100;
    return sym + major.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  }

  function fmtTime(sec) {
    if (!sec) return '-';
    var d = new Date(sec * 1000);
    return d.toLocaleString();
  }

  function ago(sec) {
    if (!sec) return '从未';
    var s = Math.max(0, Math.floor(Date.now() / 1000) - sec);
    if (s < 60) return s + ' 秒前';
    if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
    if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
    return Math.floor(s / 86400) + ' 天前';
  }

  function dur(sec) {
    if (!sec) return '-';
    var d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
    if (d > 0) return d + ' 天 ' + h + ' 小时';
    if (h > 0) return h + ' 小时 ' + m + ' 分';
    return m + ' 分';
  }

  function todayStr(offsetDays) {
    var d = new Date();
    d.setDate(d.getDate() + (offsetDays || 0));
    var m = String(d.getMonth() + 1).padStart(2, '0');
    var day = String(d.getDate()).padStart(2, '0');
    return d.getFullYear() + '-' + m + '-' + day;
  }

  // 浮动提示。可同时存在多条（保存成功 + 排队中的报错），点击即关闭。
  //
  // 为什么不是单条覆盖式：一个动作可能同时产生"成功"与"警告"两条消息
  // （例如保存设置成功、但其中某个值被服务端夹到边界）。单条实现里后一条
  // 会顶掉前一条，其中一条就永远没人看见。
  //
  // 为什么不做成文档流里的横条（原实现）：系统设置页的保存按钮在页面最底部，
  // 横条挂在 <main> 顶部，点击后提示出现在视口之外——"点了保存没反应"。
  function toast(msg, isErr) {
    var wrap = $('toast-wrap');
    if (!wrap) return;
    var t = el('p', 'toast' + (isErr ? ' err' : ''), msg);
    t.setAttribute('role', isErr ? 'alert' : 'status');
    t.title = '点击关闭';
    t.addEventListener('click', function () { if (t.parentNode) t.parentNode.removeChild(t); });
    wrap.appendChild(t);
    // 报错多留一会儿（要读、可能还要照着改），普通提示一扫而过即可。
    setTimeout(function () { if (t.parentNode) t.parentNode.removeChild(t); }, isErr ? 6000 : 3000);
  }

  // 把提示容器钉在顶栏正下方。
  //
  // 顶栏是 sticky 且 flex-wrap，窄屏下导航会换行、高度从 48px 涨到 90px 上下。
  // 因此不写死像素值，而是在布局变化时实测 offsetHeight。
  // 同理也不写死"顶栏高度"作为回退，只用它兜住 measure 之前的首帧。
  function syncToastOffset() {
    var bar = document.querySelector('.topbar');
    var wrap = $('toast-wrap');
    if (!bar || !wrap) return;
    wrap.style.setProperty('--toast-top', bar.offsetHeight + 'px');
  }

  // -------------------------------------------------------------------------
  // 站内对话框（#ask-modal）
  //
  // 为什么不用原生 confirm/alert：
  //   · 浏览器可以"阻止此页面再显示对话框"，此后 confirm 静默返回 false、
  //     alert 什么都不显示 —— 界面看上去毫无反应，管理员只会以为按钮坏了；
  //   · 深色主题下原生弹窗是刺眼的白底，风格与整站不一致；
  //   · 注册码这类内容要能选中/一键复制，原生 alert 做不到。
  //
  // 一个组件承担两种用法：二次确认（只给按钮）与结果展示（多一段可复制的值）。
  // 每次调用都会**重置**所有字段，避免上一次的注册码留在界面上（那是凭据，
  // 不该在对话框关掉之后还留在 DOM 里等下一双眼睛）。
  // -------------------------------------------------------------------------

  var askResolve = null;

  // askDialog 打开对话框，返回 Promise：
  //   确认 → true；取消 / 点遮罩 / 按 Esc → false。
  function askDialog(opts) {
    opts = opts || {};
    setText($('ask-title'), opts.title || '确认');
    setText($('ask-desc'), opts.desc || '');
    $('ask-desc').hidden = !opts.desc;

    var vw = $('ask-value-wrap');
    if (opts.value) {
      setText($('ask-value'), opts.value);
      vw.hidden = false;
      $('ask-copy').hidden = false;
    } else {
      setText($('ask-value'), '');
      vw.hidden = true;
      $('ask-copy').hidden = true;
    }

    setText($('ask-extra'), opts.extra || '');
    $('ask-extra').hidden = !opts.extra;
    setText($('ask-ok'), opts.okText || '确定');
    // 结果展示型（只读）：没有可取消的东西，收起"取消"按钮免得两个按钮都像出口。
    $('ask-cancel').hidden = !!opts.hideCancel;

    // 可选勾选项（如"删除节点时是否连历史一起删"）：默认隐藏，只有传了 checkbox 才显示。
    var cw = $('ask-check-wrap');
    if (opts.checkbox) {
      cw.hidden = false;
      $('ask-check').checked = !!opts.checkbox.checked;
      setText($('ask-check-label'), opts.checkbox.label || '');
    } else {
      cw.hidden = true;
    }

    // 二次确认输入区（密码 + 动态码）：仅破坏性维护操作（删/恢复备份、清数据）才显示。
    var aw = $('ask-auth-wrap');
    if (opts.auth) {
      aw.hidden = false;
      var pw = $('ask-password');
      pw.value = '';
      pw.disabled = false;
      // 仅当已启用 TOTP 时才要动态码；没启用就不必多要一个填不出来的框。
      var codeWrap = $('ask-code-wrap');
      codeWrap.hidden = !state.totpEnabled;
      $('ask-code').value = '';
    } else {
      aw.hidden = true;
    }

    $('ask-modal').hidden = false;
    if (opts.auth) { if ($('ask-password').focus) $('ask-password').focus(); }
    else if ($('ask-ok').focus) $('ask-ok').focus();

    return new Promise(function (resolve) { askResolve = resolve; });
  }

  function closeAskDialog(result) {
    $('ask-modal').hidden = true;
    setText($('ask-value'), ''); // 凭据不留 DOM
    // 密码/动态码是敏感输入，关对话时一并清空，绝不能留在 DOM 里等下一双眼睛。
    if ($('ask-password')) $('ask-password').value = '';
    if ($('ask-code')) $('ask-code').value = '';
    var r = askResolve;
    askResolve = null;
    // 有勾选项时把勾选状态一并带回；没有则退化为纯布尔，保持旧调用点不变。
    var checked = !$('ask-check-wrap').hidden ? $('ask-check').checked : undefined;
    // 需要二次确认时带回密码与动态码（没启用 TOTP 时 code 为空串）。
    var password = !$('ask-auth-wrap').hidden ? $('ask-password').value : undefined;
    var code = (!$('ask-auth-wrap').hidden && !$('ask-code-wrap').hidden) ? $('ask-code').value : undefined;
    if (r) r({ ok: !!result, checked: checked, password: password, code: code });
  }

  $('ask-ok').addEventListener('click', function () { closeAskDialog(true); });
  $('ask-cancel').addEventListener('click', function () { closeAskDialog(false); });
  $('ask-modal').addEventListener('click', function (ev) {
    // 点遮罩关闭 = 取消（点卡片内部不关：那里可能正在选中注册码）。
    if (ev.target === $('ask-modal')) closeAskDialog(false);
  });
  document.addEventListener('keydown', function (ev) {
    if (ev.key === 'Escape' && !$('ask-modal').hidden) closeAskDialog(false);
  });

  $('ask-copy').addEventListener('click', function () {
    var text = $('ask-value').textContent || '';
    if (!text) return;
    // 剪贴板 API 只在安全上下文（https / localhost）可用；纯 HTTP 的部署
    // （内网用 IP 直连后台）会走到降级分支，此时把内容选中并提示手动复制——
    // 直接失败会让用户以为"复制按钮没用"。
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(function () { toast('已复制'); },
        function () { selectAskValue(); });
    } else {
      selectAskValue();
    }
  });

  function selectAskValue() {
    try {
      var r = document.createRange();
      r.selectNodeContents($('ask-value'));
      var sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(r);
      toast('已选中，请按 Ctrl+C / ⌘C 复制');
    } catch (e) {
      toast('请手动选中后复制', true);
    }
  }

  // -------------------------------------------------------------------------
  // API
  // -------------------------------------------------------------------------

  function api(path, opts) {
    opts = opts || {};
    var init = { method: opts.method || 'GET', credentials: 'same-origin', headers: { Accept: 'application/json' } };
    if (opts.body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(opts.body);
    }
    if (init.method !== 'GET' && init.method !== 'HEAD' && state.csrf) {
      init.headers[CSRF_HEADER] = state.csrf;
    }
    return fetch(API + path, init).then(function (res) {
      if (res.status === 401) {
        setAuthed(false);
        throw new Error('未登录或会话已过期');
      }
      if (res.status === 204) return null;
      return res.text().then(function (txt) {
        var data = null;
        if (txt) { try { data = JSON.parse(txt); } catch (e) { data = null; } }
        if (!res.ok) {
          var msg = (data && data.error && data.error.message) || ('HTTP ' + res.status);
          throw new Error(msg);
        }
        return data;
      });
    });
  }

  function guard(promise, okMsg) {
    return promise.then(function (v) {
      if (okMsg) toast(okMsg);
      return v;
    }).catch(function (e) { toast(e.message, true); });
  }

  // -------------------------------------------------------------------------
  // 登录/登出
  // -------------------------------------------------------------------------

  function setAuthed(on) {
    $('app').hidden = !on;
    $('login').hidden = on;
    if (!on) {
      state.csrf = '';
      state.me = '';
    }
  }

  function applySiteTitle(all) {
    var title = (all && all['site.title']) || 'XB-Probe';
    document.title = title + ' 管理后台';
    var loginTitle = $('login-title');
    var brand = $('brand-title');
    var pageTitle = $('page-title');
    if (loginTitle) loginTitle.textContent = title;
    if (brand) brand.textContent = title;
    if (pageTitle) pageTitle.textContent = title + ' 管理后台';
  }

  function boot() {
    api('/auth/me').then(function (me) {
      state.csrf = me.csrf_token;
      state.me = me.username;
      $('whoami').textContent = me.username;
      setAuthed(true);
      showView('nodes');
      loadNodes();
      startNodePoll();
      // 拉取 TOTP 启用状态：决定后续删除/恢复/清数据等二次确认要不要动态码输入框。
      refreshTotpStatus();
      // 后台顶部标题随站点设置同步，登录后即拉取一次。
      api('/settings').then(applySiteTitle).catch(function () { });
      // 目标名映射：晚了最多显示成"目标 #N"，不阻塞首屏。
      loadTargetNames();
    }).catch(function () {
      // 未登录：显示登录表单。若账户已启用 TOTP，第一次登录会返回 invalid_totp。
      setAuthed(false);
      $('login-user').focus();
    });
  }

  $('login-form').addEventListener('submit', function (ev) {
    ev.preventDefault();
    var body = {
      username: $('login-user').value,
      password: $('login-pass').value,
      totp: $('login-totp').value
    };
    $('login-error').textContent = '';
    fetch(API + '/auth/login', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify(body)
    }).then(function (res) {
      return res.text().then(function (txt) {
        var data = null;
        if (txt) { try { data = JSON.parse(txt); } catch (e) { data = null; } }
        if (!res.ok) {
          if (data && data.error && data.error.code === 'invalid_totp') {
            $('totp-row').hidden = false;
            $('login-totp').focus();
          }
          throw new Error((data && data.error && data.error.message) || ('HTTP ' + res.status));
        }
        return data;
      });
    }).then(function (data) {
      state.csrf = data.csrf_token;
      state.me = data.username;
      $('whoami').textContent = data.username;
      $('login-pass').value = '';
      $('login-totp').value = '';
      setAuthed(true);
      showView('nodes');
      loadNodes();
      startNodePoll();
      toast('已登录');
    }).catch(function (e) { $('login-error').textContent = e.message; });
  });

  $('btn-logout').addEventListener('click', function () {
    guard(api('/auth/logout', { method: 'POST' })).then(function () {
      setAuthed(false);
      toast('已登出');
    });
  });

  $('btn-password').addEventListener('click', function () {
    $('password-modal').hidden = false;
    $('password-error').textContent = '';
  });
  $('btn-cancel-password').addEventListener('click', function () {
    $('password-modal').hidden = true;
  });
  $('password-form').addEventListener('submit', function (ev) {
    ev.preventDefault();
    api('/auth/password', {
      method: 'POST',
      body: { old_password: $('pw-old').value, new_password: $('pw-new').value }
    }).then(function () {
      $('pw-old').value = ''; $('pw-new').value = '';
      $('password-modal').hidden = true;
      toast('密码已修改');
    }).catch(function (e) { $('password-error').textContent = e.message; });
  });

  // -------------------------------------------------------------------------
  // 视图切换
  // -------------------------------------------------------------------------

  var loaders = {
    nodes: function () { loadNodes(); },
    // 探测目标页的范围是多选节点，进页面时就要有节点清单可用；
    // 管理员也可能直接用 #/targets 落进来，节点视图从未打开过。
    targets: function () { ensureNodes(); loadTargets(); },
    alerts: function () { loadChannels(); loadRules(); loadEvents(); },
    settings: function () { loadSettings(); loadThemes(); loadStorage(); loadHeartbeat(); loadBackups(); refreshTotpStatus(); },
    account: function () { loadTOTP(); },
    releases: function () { loadReleases(); },
    audit: function () { loadAuditRetention(); loadAudit(); }
  };

  var currentView = 'nodes'; // 当前视图名：深浅切换后按它重跑 loader 重画图表

  function showView(name) {
    currentView = name;
    var views = ['nodes', 'node', 'targets', 'alerts', 'settings', 'account', 'releases', 'audit'];
    views.forEach(function (v) {
      var node = $('view-' + v);
      if (node) node.hidden = (v !== name);
    });
    Array.prototype.forEach.call(document.querySelectorAll('.topnav .nav'), function (b) {
      b.classList.toggle('active', b.getAttribute('data-view') === name);
    });
    if (loaders[name]) loaders[name]();
  }

  Array.prototype.forEach.call(document.querySelectorAll('.topnav .nav'), function (b) {
    b.addEventListener('click', function () { showView(b.getAttribute('data-view')); });
  });

  // 顶栏高度会随导航换行变化（窄屏、缩放、字体不同），提示容器必须跟着重新锚定。
  // resize 之外还要监听字号重排：用户改缩放时不一定触发 resize 事件，
  // 但 .topbar 的 offsetHeight 已经变了，不重测就会盖住顶栏内容。
  window.addEventListener('resize', syncToastOffset);
  window.addEventListener('DOMContentLoaded', syncToastOffset);
  Array.prototype.forEach.call(document.querySelectorAll('.topnav .nav'), function () {
    syncToastOffset();
  });
  syncToastOffset();

  // -------------------------------------------------------------------------
  // 节点列表：卡片网格
  //
  // 与公开看板用同一套视觉语言，但有几点必须不同：
  //   - 数据来自 /api/admin/v1/nodes，每项带一个 runtime 子对象（内存运行态）。
  //     nodes 表里的 status / last_seen 是死列（没有任何代码写入），只能用
  //     runtime 里的值，否则状态列永远是 offline、最后上报永远是 0。
  //   - 显示真实 IP / 主机名 / 是否公开，以及归档状态。
  //   - 卡片整体可点击进入详情页，管理操作全部在详情页里（保持卡片与看板一致，
  //     不塞一排按钮把卡片撑高）。
  //
  // 布局稳定性：卡片每 5 秒被重新赋值一次。因此结构固定、缺值渲染 "—"、
  // 刷新时按键复用元素（以 node id 为键）并**完全按服务端顺序就位**，
  // 前端绝不做二次排序，更不会按状态排序。
  // -------------------------------------------------------------------------

  var nodeCards = {};          // node id -> { root, refs, group, hidden }
  var nodeFilter = '\u0000all'; // 当前分组筛选
  var nodeTabSig = '';
  var nodeSearch = '';
  var nodePollTimer = null;
  var spark = [];

  var FILTER_ALL = '\u0000all';
  var FILTER_NONE = '\u0000nogroup';

  // runtimeOf 取节点的运行态；从未上报过的节点没有 runtime 子对象。
  function runtimeOf(n) { return n.runtime || null; }

  function groupKeyOf(n) {
    // 后台列表的字段名是 group（公开接口才是 group）。
    var g = n.group;
    return g ? String(g) : '';
  }

  function durText(sec) {
    if (sec == null || sec < 0) return '—';
    var d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600),
        m = Math.floor((sec % 3600) / 60);
    if (d > 0) return d + '天' + h + '小时';
    if (h > 0) return h + '小时' + m + '分';
    return m + '分';
  }

  // statusOf 返回 { key, text }：胶囊文案与配色级别。
  function statusOf(n) {
    var rt = runtimeOf(n);
    if (n.archived_at) return { key: 'archived', text: '已归档' };
    if (!rt) return { key: 'offline', text: '从未上报' };
    switch (rt.status) {
      case 'online':  return { key: 'online',  text: '在线 ' + durText(rt.uptime_s) };
      case 'warning': return { key: 'warning', text: '疑似离线' };
      case 'offline': {
        var since = rt.last_seen ? Math.max(0, Math.floor(Date.now() / 1000) - rt.last_seen) : 0;
        return { key: 'offline', text: rt.last_seen ? '离线 ' + durText(since) : '离线' };
      }
      default: return { key: rt.status || 'offline', text: rt.status || '离线' };
    }
  }

  function expireInfoOf(n) {
    var rt = runtimeOf(n);
    if (!rt || !rt.expire_at) return { text: '永久', level: '' };
    var days = Math.ceil((rt.expire_at - Date.now() / 1000) / 86400);
    if (days < 0) return { text: '已过期' + (-days) + '天', level: 'bad' };
    if (days === 0) return { text: '今天到期', level: 'bad' };
    if (days <= 3) return { text: days + '天后到期', level: 'bad' };
    if (days <= 7) return { text: days + '天后到期', level: 'warn' };
    return { text: days + '天后到期', level: '' };
  }

  function levelOf(p) {
    if (p == null) return '';
    if (p >= 9000) return 'bad';
    if (p >= 7500) return 'warn';
    return '';
  }

  function setText(node, text) {
    if (!node) return;
    var s = String(text);
    if (node.textContent !== s) node.textContent = s;
  }
  function setClass(node, cls) { if (node && node.className !== cls) node.className = cls; }
  // setWidth 设置进度条宽度。入参是"百分比 ×100"刻度（0~10000），
  // 必须除以 100 才是 CSS 百分比；漏掉这一步会让所有进度条恒为满格
  // （≥1% 就被 clamp 到 100）。与公开看板 app.js 里同名函数保持一致。
  function setWidth(node, percent) {
    if (!node) return;
    var p = Math.max(0, Math.min(10000, Number(percent) || 0)) / 100;
    var s = p.toFixed(1) + '%';
    if (node.style.width !== s) node.style.width = s;
  }

  // makeMetric 构建一条指标：标签行(16) + 进度条(6) + 说明行(14)，高度固定。
  function makeMetric(label) {
    var box = el('div', 'nb');
    var row = el('div', 'nb-row');
    var k = el('span', 'nb-k', label + ' ');
    var kb = el('b');
    k.appendChild(kb);
    var v = el('span', 'nb-v');
    row.appendChild(k); row.appendChild(v);

    var track = el('div', 'track');
    var fill = el('i');
    track.appendChild(fill);

    var sub = el('div', 'nb-sub');
    box.appendChild(row); box.appendChild(track); box.appendChild(sub);
    return { box: box, kb: kb, v: v, fill: fill, sub: sub };
  }

  function makeNodeCard(n) {
    var root = el('article', 'node');
    root.tabIndex = 0;
    root.setAttribute('role', 'button');

    var head = el('div', 'n-head');
    var dot = el('span', 'dot');
    // 国旗标识：与公开看板同一套（.n-flag 固定宽度占位，未设置国家时留空）。
    var flag = el('span', 'n-flag');
    var name = el('span', 'n-name');
    var chip = el('span', 'n-chip');
    head.appendChild(dot); head.appendChild(flag); head.appendChild(name); head.appendChild(chip);

    var sub = el('div', 'n-sub');
    var os = el('span', 'n-os');
    var ipBadges = el('span', 'ip-badges');
    var exp = el('span', 'n-exp');
    sub.appendChild(os); sub.appendChild(ipBadges); sub.appendChild(exp);

    var bars = el('div', 'n-bars');
    var mCpu = makeMetric('CPU');
    var mMem = makeMetric('内存');
    var mDisk = makeMetric('磁盘');
    var mTraf = makeMetric('流量');
    bars.appendChild(mCpu.box); bars.appendChild(mMem.box);
    bars.appendChild(mDisk.box); bars.appendChild(mTraf.box);

    // 续费价格（折算到月均）。与公开看板的**同一结构、同一文案**：
    // 后台没有 public.cards 那套逐块开关（管理员本来就该看到全部信息），
    // 所以恒显示；缺价格时渲染 "—" 而不是省略行——省掉会让卡片高度参差。
    var fee = el('div', 'n-fee');
    var feeK = el('span', 'fee-k', '续费');
    var feeV = el('span', 'fee-v', '—');
    fee.appendChild(feeK); fee.appendChild(feeV);

    var net = el('div', 'n-net');
    var rateRow = el('div', 'net-row');
    var rRxD = el('span', 'dir down', '↓'), rRx = el('b');
    var rTxD = el('span', 'dir up up-gap', '↑'), rTx = el('b');
    rateRow.appendChild(rRxD); rateRow.appendChild(rRx);
    rateRow.appendChild(rTxD); rateRow.appendChild(rTx);

    var totRow = el('div', 'net-row totals');
    var tRxD = el('span', 'dir down', '↓'), tRx = el('b');
    var tTxD = el('span', 'dir up up-gap', '↑'), tTx = el('b');
    totRow.appendChild(tRxD); totRow.appendChild(tRx);
    totRow.appendChild(tTxD); totRow.appendChild(tTx);

    // 与公开看板同一结构：速率在左、续费价格在右，价格不再单独占一行
    // （改要一起改，两边的卡片 DOM 与行高必须一致）。
    var rateLine = el('div', 'net-line');
    rateLine.appendChild(rateRow);
    rateLine.appendChild(fee);

    net.appendChild(rateLine); net.appendChild(totRow);

    root.appendChild(head); root.appendChild(sub);
    root.appendChild(bars); root.appendChild(net);

    var open = function () { openNode(n.id); };
    root.addEventListener('click', open);
    root.addEventListener('keydown', function (ev) {
      if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); open(); }
    });

    return {
      root: root, dot: dot, flag: flag, name: name, chip: chip, os: os, ipBadges: ipBadges, exp: exp,
      bars: bars, fee: fee, feeV: feeV, net: net, rateRow: rateRow, totRow: totRow,
      cpu: mCpu, mem: mMem, disk: mDisk, traf: mTraf,
      rateRx: rRx, rateTx: rTx, totRx: tRx, totTx: tTx
    };
  }

  function updateMetric(m, percent, valueText, subText, labelBold) {
    setText(m.kb, labelBold === undefined ? '' : labelBold);
    setText(m.v, valueText);
    setWidth(m.fill, percent);
    setClass(m.fill, levelOf(percent));
    setText(m.sub, subText || '—');
  }

  function updateNodeCard(c, n) {
    var rt = runtimeOf(n);
    var st = statusOf(n);
    setClass(c.dot, 'dot ' + st.key);
    setText(c.name, n.name || n.public_id);
    setClass(c.chip, 'n-chip ' + st.key);
    setText(c.chip, st.text);
    renderFlag(c.flag, countryOf(n));

    // 副行：虚拟化 · 架构（库里的静态字段，Master 重启后也能恢复）· 是否公开
    var parts = [];
    var virt = (rt && rt.virt) || n.virt;
    var arch = (rt && rt.arch) || n.arch;
    if (virt) parts.push(virt);
    if (arch) parts.push(arch);
    if (n.public_visible === false) parts.push('未公开');
    setText(c.os, parts.length ? parts.join(' · ') : (n.os || '—'));

    // IP 标识：只在卡片上显示有没有 IPv4/IPv6，不暴露具体地址。
    // 两边都没有时渲染 "—"——留空白会让这张卡片看起来像"还没加载完"。
    clear(c.ipBadges);
    if (n.ip_v4) c.ipBadges.appendChild(el('span', 'ip-badge v4', 'IPv4'));
    if (n.ip_v6) c.ipBadges.appendChild(el('span', 'ip-badge v6', 'IPv6'));
    if (!n.ip_v4 && !n.ip_v6) c.ipBadges.appendChild(el('span', 'n-badges-none', '—'));

    var ex = expireInfoOf(n);
    setClass(c.exp, 'n-exp ' + ex.level);
    setText(c.exp, ex.text);

    // 从未上报过的节点：指标显示 "—"。渲染成 0% 会被读成"使用率为零"，
    // 而事实是"没有数据"，两者含义完全不同。
    var noData = !rt || !rt.last_seen;
    var cores = (rt && rt.cpu_cores) || n.cpu_cores || 0;

    updateMetric(c.cpu, noData ? 0 : rt.cpu_pct, noData ? '—' : pct(rt.cpu_pct),
      noData ? '—' : (rt.load1 == null ? '—' : '负载 ' + (rt.load1 / 100).toFixed(2)),
      cores ? cores + ' 核' : '');

    updateMetric(c.mem, noData ? 0 : rt.mem_pct, noData ? '—' : pct(rt.mem_pct),
      noData ? '—' : fmtBytes(rt.mem_used) + ' / ' + fmtBytes(rt.mem_total || n.mem_total));

    updateMetric(c.disk, noData ? 0 : rt.disk_pct, noData ? '—' : pct(rt.disk_pct),
      noData ? '—' : fmtBytes(rt.disk_used) + ' / ' + fmtBytes(n.disk_total));

    if (noData) {
      updateMetric(c.traf, 0, '—', '—');
    } else if (rt.traffic_quota > 0) {
      updateMetric(c.traf, rt.traffic_used_pct,
        pct(rt.traffic_used_pct) + (rt.traffic_used_pct > 10000 ? ' ⚠' : ''),
        fmtBytes(rt.traffic_used) + ' / ' + fmtBytes(rt.traffic_quota));
    } else {
      updateMetric(c.traf, 0, '不限',
        rt.traffic_used == null ? '—' : fmtBytes(rt.traffic_used) + ' 已用');
    }

    // 续费价格：与公开看板同一段文案逻辑（字段名也一致：fee_monthly/fee_currency/fee_cycle）。
    // monthly/quarterly/yearly 折算后是月均金额；once/lifetime 折算成 0，
    // 必须靠 fee_cycle 区分"终身/一次性"与"没配价格"——否则终身节点会显示成 "—"。
    var feeText = '—';
    if (n.fee_monthly > 0) {
      feeText = fmtMoney(n.fee_monthly, n.fee_currency) + ' / 月';
    } else if (n.fee_cycle === 'lifetime') {
      feeText = '终身';
    } else if (n.fee_cycle === 'once') {
      feeText = '一次性';
    }
    // 没有价格、也没有续费信息（终身/一次性）时整块不显示：后台没有 public.cards
    // 那套逐块开关，但"每张卡片都挂一行「续费 —」"同样没有信息量（用户反馈）。
    // DOM 结构不变，只是 hidden，卡片行高由 .net-line 固定，不因此参差。
    c.fee.hidden = feeText === '—';
    setText(c.feeV, feeText);

    setText(c.rateRx, noData ? '—' : fmtBps(rt.rx_bps));
    setText(c.rateTx, noData ? '—' : fmtBps(rt.tx_bps));
    setText(c.totRx, noData ? '—' : fmtBytes(periodRx(n)));
    setText(c.totTx, noData ? '—' : fmtBytes(periodTx(n)));
  }

  // 周期流量用运行态里的 period_rx/period_tx：那是"这一周期上下行各跑了多少"，
  // 与进度条用的 traffic_used（按 traffic_mode 折算后的计费用量）不是一回事。
  function periodRx(n) {
    var rt = runtimeOf(n);
    return rt ? (rt.period_rx != null ? rt.period_rx : rt.traffic_used) : null;
  }
  function periodTx(n) {
    var rt = runtimeOf(n);
    return rt ? (rt.period_tx != null ? rt.period_tx : null) : null;
  }

  // --- 顶部分组标签 ---------------------------------------------------------

  function nodeTabItems(list) {
    var counts = {}, order = [];
    list.forEach(function (n) {
      var g = groupKeyOf(n);
      if (!(g in counts)) { counts[g] = 0; order.push(g); }
      counts[g]++;
    });
    var items = [{ key: FILTER_ALL, label: '全部', count: list.length }];
    order.forEach(function (g) { if (g !== '') items.push({ key: g, label: g, count: counts[g] }); });
    if (counts['']) items.push({ key: FILTER_NONE, label: '未分组', count: counts[''] });
    return items;
  }

  function buildNodeTabs(list) {
    var items = nodeTabItems(list);
    var sig = items.map(function (i) { return i.label + ':' + i.count; }).join('|');
    if (sig === nodeTabSig) { applyNodeFilter(); return; }
    nodeTabSig = sig;

    var stillThere = items.some(function (i) { return i.key === nodeFilter; });
    if (!stillThere) nodeFilter = FILTER_ALL;

    var box = $('node-tabs');
    clear(box);
    items.forEach(function (it) {
      var b = el('button', 'tab');
      b.type = 'button';
      b.setAttribute('role', 'tab');
      b.dataset.key = it.key;
      b.appendChild(document.createTextNode(it.label));
      b.appendChild(el('span', 'n', it.count));
      b.addEventListener('click', function () { nodeFilter = it.key; applyNodeFilter(); });
      box.appendChild(b);
    });
    applyNodeFilter();
  }

  // applyNodeFilter 只切换 hidden，绝不重排 DOM：网格位置因此始终固定。
  function applyNodeFilter() {
    var tabs = $('node-tabs').children;
    for (var i = 0; i < tabs.length; i++) {
      var t = tabs[i];
      t.setAttribute('aria-selected', t.dataset.key === nodeFilter ? 'true' : 'false');
    }
    Object.keys(nodeCards).forEach(function (id) {
      var c = nodeCards[id];
      var byGroup;
      if (nodeFilter === FILTER_ALL) byGroup = true;
      else if (nodeFilter === FILTER_NONE) byGroup = c.group === '';
      else byGroup = c.group === nodeFilter;

      var bySearch = !nodeSearch || c.haystack.indexOf(nodeSearch) >= 0;
      var show = byGroup && bySearch;
      if (c.root.hidden === show) c.root.hidden = !show;
    });
  }

  // --- 汇总卡片 -------------------------------------------------------------

  function renderNodeKpis(list) {
    var online = 0, offline = 0;
    list.forEach(function (n) {
      if (n.archived_at) return;
      var st = statusOf(n).key;
      if (st === 'online') online++;
      else if (st === 'offline' || st === 'warning') offline++;
    });
    setText($('kpi-online'), online);
    setText($('kpi-total'), list.length);
    setText($('kpi-offline'), offline > 0 ? offline + ' 个离线/存疑' : '全部在线');

    var busy = null;
    list.forEach(function (n) {
      var rt = runtimeOf(n);
      if (!rt || !rt.last_seen) return;
      if (!busy || rt.cpu_pct > busy.cpu_pct) busy = { cpu_pct: rt.cpu_pct, name: n.name || n.public_id };
    });
    if (busy) {
      setText($('kpi-busy-pct'), pct(busy.cpu_pct));
      setText($('kpi-busy-name'), busy.name);
    } else {
      setText($('kpi-busy-pct'), '—');
      setText($('kpi-busy-name'), '暂无数据');
    }

    var tRx = 0, tTx = 0, pRx = 0, pTx = 0, rxBps = 0, txBps = 0;
    list.forEach(function (n) {
      var rt = runtimeOf(n);
      if (!rt || !rt.last_seen) return;
      tRx += rt.today_rx || 0;
      tTx += rt.today_tx || 0;
      pRx += rt.period_rx || 0;
      pTx += rt.period_tx || 0;
      rxBps += rt.rx_bps || 0;
      txBps += rt.tx_bps || 0;
    });
    setText($('kpi-today-rx'), fmtBytes(tRx || null));
    setText($('kpi-today-tx'), fmtBytes(tTx || null));
    setText($('kpi-period-rx'), fmtBytes(pRx || null));
    setText($('kpi-period-tx'), fmtBytes(pTx || null));
    setText($('kpi-rx'), fmtBps(rxBps));
    setText($('kpi-tx'), fmtBps(txBps));

    // 月费合计：按支付周期折算成月均后求和（见 model.MonthlyEquivalent）。
    // 与公开看板的「月费合计」同一口径——直接累加 price_minor 会把年付节点
    // 算成 12 倍，也会把 lifetime/once 这种"没有月支出"的节点计进去。
    // 币种取**第一个真正有月支出的节点**的币种，避免拿一台永久节点的币种
    // 去标注一堆人民币金额。
    var fee = 0, feeCur = '';
    list.forEach(function (n) {
      if (n.archived_at) return;
      var f = n.fee_monthly || 0;
      fee += f;
      if (f > 0 && !feeCur && n.fee_currency) feeCur = n.fee_currency;
    });
    setText($('kpi-fee'), fmtMoney(fee, feeCur));
    setText($('kpi-fee-currency'), (feeCur || 'CNY') + ' / 月 · 按月均折算');

    spark.push(rxBps);
    if (spark.length > 48) spark.shift();
    drawSpark();
  }

  // --- 列表刷新 -------------------------------------------------------------

  function renderNodeList(list) {
    var grid = $('nodes');
    var seen = {};

    list.forEach(function (n, idx) {
      var id = String(n.id);
      seen[id] = true;
      var c = nodeCards[id];
      if (!c) { c = makeNodeCard(n); nodeCards[id] = c; }
      c.group = groupKeyOf(n);
      c.haystack = [n.name, (n.ip_v4_list || []).join(' '), (n.ip_v6_list || []).join(' '), n.ip_v4, n.ip_v6, n.group, n.public_id, n.hostname]
        .filter(Boolean).join(' ').toLowerCase();
      updateNodeCard(c, n);

      // 顺序完全采用服务端返回的顺序；顺序稳定时这里一次 DOM 操作都不做。
      var at = grid.children[idx];
      if (at !== c.root) grid.insertBefore(c.root, at || null);
    });

    Object.keys(nodeCards).forEach(function (id) {
      if (seen[id]) return;
      var c = nodeCards[id];
      if (c.root.parentNode) c.root.parentNode.removeChild(c.root);
      delete nodeCards[id];
    });

    renderNodeKpis(list);
    buildNodeTabs(list);
    $('nodes-empty').hidden = list.length > 0;
    applyNodeFilter();
  }

  function loadNodes() {
    return api('/nodes').then(function (list) {
      state.nodes = list || [];
      renderNodeList(state.nodes);
      refreshNodeChecks();
    }).catch(function (e) { toast(e.message, true); });
  }

  // 轮询：只在"节点"视图可见时跑，切到别的视图就停，别在后台空转。
  function startNodePoll() {
    if (nodePollTimer) return;
    nodePollTimer = setInterval(function () {
      if ($('view-nodes').hidden) return;
      loadNodes();
    }, 5000);
  }

  // refreshNodeChecks 在节点清单刷新后重画已经渲染出来的"节点多选"列表。
  //
  // 这里原本更新的是一个单节点下拉（#t-node）——那个控件早就不存在了，
  // 函数一直在空转。探测目标的范围改成多选之后这个钩子才真正有用：
  // 表单开着的时候，节点页的 5 秒轮询拉到新节点，列表能跟着补上，
  // 而不是必须关掉再打开一次表单。
  //
  // 用 checkedNodeIDs 回填再重画：重画不能把管理员已经勾好的选择抹掉。
  function refreshNodeChecks() {
    var create = $('t-nodes');
    if (create && !create.hidden) buildNodeChecks(create, checkedNodeIDs(create), 0);
    var edit = $('tm-nodes');
    if (edit && !edit.hidden && editingTarget) {
      buildNodeChecks(edit, checkedNodeIDs(edit), editingTarget.node_id);
    }
  }

  $('node-filter').addEventListener('input', function () {
    nodeSearch = ($('node-filter').value || '').trim().toLowerCase();
    applyNodeFilter();
  });
  $('btn-refresh-nodes').addEventListener('click', function () { loadNodes(); });

  $('btn-new-node').addEventListener('click', function () {
    $('new-node-form').hidden = !$('new-node-form').hidden;
  });
  $('btn-cancel-node').addEventListener('click', function () { $('new-node-form').hidden = true; });

  $('btn-create-node').addEventListener('click', function () {
    var name = $('new-node-name').value.trim();
    if (!name) { toast('请填写节点名称', true); return; }
    guard(api('/nodes', { method: 'POST', body: { name: name, group: $('new-node-group').value.trim() } }), '节点已创建')
      .then(function (r) {
        if (!r) return;
        $('new-node-name').value = '';
        $('new-node-group').value = '';
        $('new-node-form').hidden = true;
        return loadNodes().then(function () { openNode(r.id); });
      });
  });

  function archiveNode(id) {
    askDialog({
      title: '归档节点 #' + id,
      desc: '归档后停止告警与离线计时，历史数据保留。',
      okText: '归档'
    }).then(function (r) {
      if (!r.ok) return;
      guard(api('/nodes/' + id + '/archive', { method: 'POST' }), '已归档').then(loadNodes);
    });
  }

  function deleteNode(id, name) {
    askDialog({
      title: '删除节点 #' + id,
      desc: '删除节点「' + (name || id) + '」。',
      checkbox: { label: '同时删除历史时序数据（不可恢复）', checked: false },
      extra: '不勾则仅删除节点记录、保留历史。此操作不可撤销。',
      okText: '删除'
    }).then(function (r) {
      if (!r.ok) return;
      var url = '/nodes/' + id + '?purge=' + (r.checked ? 'true' : 'false');
      guard(api(url, { method: 'DELETE' }), '已删除').then(loadNodes);
    });
  }

  // -------------------------------------------------------------------------
  // 节点详情
  // -------------------------------------------------------------------------

  function openNode(id) {
    showView('node');
    loadNodeDetail(id);
  }

  $('btn-back-nodes').addEventListener('click', function () { showView('nodes'); });
  $('btn-reload-node').addEventListener('click', function () {
    if (state.node) loadNodeDetail(state.node.id);
  });

  function loadNodeDetail(id) {
    var body = $('node-body');
    clear(body);
    body.appendChild(el('p', 'muted', '加载中…'));

    api('/nodes/' + id).then(function (n) {
      state.node = n;
      // 换节点时清掉上一个节点的折线开关与缓存，避免串台。
      state.latToggles = {};
      adminLatItems = null;
      $('node-title').textContent = n.name + ' (#' + n.id + ')';
      clear(body);
      body.appendChild(renderNodeRuntimeCard(n));
      body.appendChild(renderNodeManage(n));
      body.appendChild(renderBillingCard(n));
      body.appendChild(renderHistoryCard(n));
      loadRuntime(n.id, body.firstChild);
      loadTokens(n.id);
      loadEnrollCodes(n.id);
      loadTraffic(n);
      loadNodeTargets(n.id);
    }).catch(function (e) {
      clear(body);
      body.appendChild(el('p', 'error', e.message));
    });
  }

  function renderNodeRuntimeCard(n) {
    var wrap = el('div');
    var row = el('div', 'row');
    var h = el('h2', null, '实时状态');
    row.appendChild(h);
    row.appendChild(el('span', 'spacer'));
    // 状态胶囊先建好、内容留空，等运行态回来再填。
    //
    // 这里曾经用 n.status（nodes 表的列）——那一列没有任何代码写入，永远是
    // offline，于是详情页头部的状态永远是"离线"，哪怕节点正在正常上报。
    // 真实状态只在内存运行态里，必须等 /runtime 返回。
    var stChip = el('span', 'chip');
    stChip.id = 'node-status-chip';
    stChip.hidden = true;
    row.appendChild(stChip);
    if (n.public_visible) row.appendChild(el('span', 'chip', '公开可见'));
    else row.appendChild(el('span', 'chip', '已隐藏'));
    if (n.archived_at) row.appendChild(el('span', 'chip archived', '已归档'));
    wrap.appendChild(row);

    var cards = el('div', 'cards');
    cards.id = 'runtime-cards';
    cards.appendChild(el('div', 'card'));
    wrap.appendChild(cards);

    wrap.appendChild(el('h3', null, '资产与系统信息'));
    var info = el('div', 'info-grid');
    function group(title, rows) {
      var g = el('div', 'info-group');
      g.appendChild(el('h4', null, title));
      var dl = el('dl');
      rows.forEach(function (r) {
        dl.appendChild(el('dt', null, r[0]));
        var v = r[1];
        var dd = el('dd', v === '-' || v == null || v === '' ? 'muted' : null, v == null || v === '' ? '—' : v);
        dl.appendChild(dd);
      });
      g.appendChild(dl);
      return g;
    }
    info.appendChild(group('公开信息', [
      ['公开标识', n.public_id],
      ['可见性', n.public_visible ? '公开可见' : '已隐藏'],
      ['状态', n.archived_at ? '已归档' : '正常']
    ]));
    info.appendChild(group('网络', [
      ['IPv4', ipListText(n.ip_v4_list, n.ip_v4)],
      ['IPv6', ipListText(n.ip_v6_list, n.ip_v6)],
      ['主机名', n.hostname]
    ]));
    info.appendChild(group('位置', [
      ['服务商', n.provider],
      ['机房/位置', n.location],
      ['国家/地区', countryLabel(countryOf(n))]
    ]));
    info.appendChild(group('系统', [
      ['系统', n.os],
      ['内核', n.kernel],
      ['架构', n.arch],
      ['系统族', n.os_family],
      ['虚拟化', n.virt],
      ['服务管理器', n.service_manager]
    ]));
    info.appendChild(group('硬件', [
      ['CPU 型号', n.cpu_model],
      ['CPU 核数', n.cpu_cores ? n.cpu_cores + ' 核' : null],
      ['内存总量', n.mem_total ? fmtBytes(n.mem_total) : null],
      ['交换区', n.swap_total ? fmtBytes(n.swap_total) : null],
      ['磁盘总量', n.disk_total ? fmtBytes(n.disk_total) : null],
      ['计数器位宽', n.counter_bits ? n.counter_bits + ' bit' : null]
    ]));
    info.appendChild(group('维护', [
      ['开机时间', n.boot_time ? fmtTime(n.boot_time) : null],
      ['Agent 版本', n.agent_version],
      ['备注', n.note]
    ]));
    wrap.appendChild(info);
    return wrap;
  }

  function loadRuntime(id, holder) {
    api('/nodes/' + id + '/runtime').then(function (rt) {
      var chip = $('node-status-chip');
      if (chip) {
        var key = rt.status || 'offline';
        chip.hidden = false;
        chip.className = 'chip ' + key;
        chip.textContent = key === 'online'
          ? '在线 ' + dur(rt.uptime_s)
          : (key === 'warning' ? '疑似离线' : '离线 ' + ago(rt.last_seen));
      }
      var box = $('runtime-cards');
      if (!box) return;
      clear(box);
      function card(k, v) {
        var c = el('div', 'card');
        c.appendChild(el('div', 'k', k));
        c.appendChild(el('div', 'v', v));
        return c;
      }
      box.appendChild(card('CPU', pct(rt.cpu_pct)));
      box.appendChild(card('负载 (1min)', rt.load1 ? (rt.load1 / 100).toFixed(2) : '-'));
      box.appendChild(card('内存', pct(rt.mem_pct) + '  ' + fmtBytes(rt.mem_used) + ' / ' + fmtBytes(rt.mem_total)));
      box.appendChild(card('交换区', pct(rt.swap_pct)));
      box.appendChild(card('磁盘', pct(rt.disk_pct) + '  ' + fmtBytes(rt.disk_used)));
      box.appendChild(card('下行', fmtBps(rt.rx_bps)));
      box.appendChild(card('上行', fmtBps(rt.tx_bps)));
      box.appendChild(card('连接数', rt.conns));
      box.appendChild(card('运行时长', dur(rt.uptime_s)));
      box.appendChild(card('最后上报', ago(rt.last_seen)));
      if (rt.config_version) box.appendChild(card('配置摘要', rt.config_version));
      if (rt.traffic_quota > 0) {
        // traffic_used 是"计费口径"的用量（已按计量口径与流量倍率折算），
        // 与下面"周期流量 ↓↑"里的原始进出量不是一回事，标签里写清楚。
        box.appendChild(card('本周期计费流量', fmtBytes(rt.traffic_used) + ' / ' + fmtBytes(rt.traffic_quota)
          + '  (' + pct(rt.traffic_used_pct) + ')'));
      }
      if (rt.latency && rt.latency.length) {
        var lat = el('div', 'card full');
        lat.appendChild(el('div', 'k', '最新一轮探测'));
        var latGrid = el('div', 'grid cards-3');
        rt.latency.forEach(function (l) {
          var c = el('div', 'stat');
          var k = el('div', 'k');
          var dot = el('span', 'dot');
          if (l.error || l.rtt_avg == null) {
            dot.className = 'dot unknown';
          } else {
            dot.style.backgroundColor = latencyColor(l.rtt_avg);
          }
          k.appendChild(dot);
          k.appendChild(document.createTextNode(targetNameOf(l.target_id)));
          c.appendChild(k);
          var v;
          if (l.error) v = '错误';
          else if (l.rtt_avg == null) v = '无数据';
          else v = (l.rtt_avg / 1000).toFixed(1) + ' ms';
          c.appendChild(el('div', 'v', v));
          var sub;
          if (l.error) sub = l.error;
          else if (l.rtt_avg == null) sub = '本轮回路未通';
          else sub = '丢包 ' + (l.loss / 100).toFixed(0) + '%';
          c.appendChild(el('div', 'sub', sub));
          latGrid.appendChild(c);
        });
        lat.appendChild(latGrid);
        box.appendChild(lat);
      }
    }).catch(function () {
      var chip = $('node-status-chip');
      if (chip) {
        chip.hidden = false;
        chip.className = 'chip offline';
        chip.textContent = '从未上报';
      }
      var box = $('runtime-cards');
      if (!box) return;
      clear(box);
      var c = el('div', 'card');
      c.appendChild(el('div', 'k', '实时状态'));
      c.appendChild(el('div', 'v', '尚无上报'));
      box.appendChild(c);
    });
    void holder;
  }

  function renderNodeManage(n) {
    var wrap = el('div');
    wrap.appendChild(el('h2', null, '编辑与运维'));

    // form-grid：字段按列自动排布并让输入框底对齐。
    // 之前是 8 个 label 纵向堆在一个 720px 宽的窄条里，页面右半边全空着。
    var panel = el('div', 'panel form-grid');
    function field(label, id, value) {
      var l = el('label', null, label);
      var i = el('input');
      i.id = id;
      i.type = 'text';
      i.value = value == null ? '' : String(value);
      l.appendChild(i);
      return l;
    }

    // fieldTextarea 与 field 同理，但用于可填多行的多地址输入。
    function fieldTextarea(label, id, value) {
      var l = el('label', 'full');
      l.appendChild(el('span', null, label));
      var ta = el('textarea');
      ta.id = id;
      ta.rows = 2;
      ta.value = value == null ? '' : String(value);
      ta.placeholder = '每行一个地址，可填多个';
      l.appendChild(ta);
      return l;
    }
    panel.appendChild(field('名称', 'n-name', n.name));
    panel.appendChild(field('分组', 'n-group', n.group));
    panel.appendChild(field('排序（越小越靠前）', 'n-sort', n.sort_order));
    // 地址是"自动探测 + 手动覆盖"两态，标签必须把当前状态说清楚：
    //   - 自动态下若不明说，用户看到空值会以为坏了，于是手填一个 —— 那正好把
    //     自动探测永久关掉（UpdateNodeAssets 的三态规则：值与库里不同 = 锁定）；
    //   - 手动态下若不明说，用户改了服务器地址却发现地址栏不变，无从排查。
    // n.ip_auto 缺省视为 true（老数据/接口未升级时不吓唬人）。
    // 多地址：一个节点可登记多个同族地址，每行一个；主地址恒取清单第一项。
    var ipAuto = n.ip_auto !== false;
    var ip4Init = (n.ip_v4_list && n.ip_v4_list.length) ? n.ip_v4_list.join('\n') : (n.ip_v4 || '');
    var ip6Init = (n.ip_v6_list && n.ip_v6_list.length) ? n.ip_v6_list.join('\n') : (n.ip_v6 || '');
    panel.appendChild(fieldTextarea(
      ipAuto ? 'IPv4 地址（自动探测中；在此填写即覆盖，每行一个、可填多个）'
             : 'IPv4 地址（已锁定为手动值；清空并保存可恢复自动探测）',
      'n-ip4-list', ip4Init));
    panel.appendChild(fieldTextarea('IPv6 地址（每行一个，可填多个）', 'n-ip6-list', ip6Init));
    panel.appendChild(el('p', 'muted',
      ipAuto
        ? '自动探测记录的是上报来源地址。Master 与节点同机部署（或节点在 NAT 后）时来源不是公网地址，'
          + '需要在节点上启用自助探测：sudo xb-probe-agent install --public-ip-url https://api.ipify.org'
        : '这些地址已被手动指定，自动探测不会再改写它们。把清单清空并保存即可恢复自动探测。'));
    panel.appendChild(field('服务商', 'n-provider', n.provider));
    panel.appendChild(field('机房/位置', 'n-location', n.location));
    panel.appendChild(field('备注', 'n-note', n.note));

    // 国家/地区：下拉选择而不是自由文本输入。
    // 这个值是**结构化**的（ISO alpha-2），前端要拿它查表画国旗——
    // 让管理员手敲两位代码，等于把"拼错了就画不出旗、还不报错"的风险留给他。
    // 选项由 FLAGS 表 + 系统设置「自定义国家/地区」合并生成：
    // 内置表覆盖常见地区，管理员补齐它没有的地区（如刚上线的机房所在国）。
    var cLabel = el('label', null, '国家/地区（卡片国旗）');
    var cSel = el('select');
    cSel.id = 'n-country';
    var cNone = el('option', null, '未设置');
    cNone.value = '';
    cSel.appendChild(cNone);
    var customMap = customCountryMap();
    var entries = Object.keys(FLAGS).map(function (code) {
      return { code: code, name: FLAGS[code][0], builtin: true };
    });
    Object.keys(customMap).forEach(function (code) {
      if (FLAGS[code]) return; // 与内置表撞码时以内置为准
      entries.push({ code: code, name: customMap[code], builtin: false });
    });
    entries.sort(function (a, b) { return a.name.localeCompare(b.name, 'zh'); });
    entries.forEach(function (e2) {
      var o = el('option', null, e2.name + '（' + e2.code + '）' + (e2.builtin ? '' : '〔自定义〕'));
      o.value = e2.code;
      cSel.appendChild(o);
    });
    // 库里可能存着两边表里都还没有的代码（先于表更新写入的）。这种情况
    // 必须把它作为一项补进下拉，否则一打开表单就会静默把它清成"未设置"。
    if (n.country && !FLAGS[n.country] && !customMap[n.country]) {
      var cEx = el('option', null, n.country + '（未收录）');
      cEx.value = n.country;
      cSel.appendChild(cEx);
    }
    cSel.value = n.country || '';
    cLabel.appendChild(cSel);
    panel.appendChild(cLabel);

    var vis = el('label', 'full');
    var chk = el('input');
    chk.type = 'checkbox';
    chk.id = 'n-visible';
    chk.checked = !!n.public_visible;
    vis.appendChild(chk);
    vis.appendChild(document.createTextNode(' 在公开看板显示'));
    panel.appendChild(vis);

    // 探测失败重点监控：勾上的目标在本节点失败即告警（含 ICMP 无权限这类被全局
    // 「探测连续失败」规则刻意忽略的错误——容器/LXC 节点正是因此「报错却从不告警」）。
    // 不勾则沿用全局规则。勾选项由该节点的探测目标异步载入并预勾选已保存值。
    var watchWrap = el('div', 'full');
    watchWrap.appendChild(el('h4', null, '探测失败重点监控'));
    watchWrap.appendChild(el('p', 'muted',
      '勾选的目标在本节点探测失败时立即告警，并覆盖全局规则对「ICMP 无权限 / 目标被拒」等错误的忽略；'
        + '不勾则沿用全局「探测连续失败」规则。'));
    var watchBox = el('div', 'checkbox-list');
    watchBox.id = 'n-watch-box';
    watchWrap.appendChild(watchBox);
    panel.appendChild(watchWrap);
    api('/probe-targets?node_id=' + n.id).then(function (list) {
      clear(watchBox);
      var saved = n.alert_probe_targets || [];
      var mine = (list || []).filter(function (t) { return !t.node_id || t.node_id === n.id; });
      if (!mine.length) {
        watchBox.appendChild(el('span', 'muted', '该节点暂无探测目标'));
        return;
      }
      mine.forEach(function (t) {
        var lab = el('label', 'check');
        var cb = el('input');
        cb.type = 'checkbox';
        cb.value = t.id;
        cb.checked = saved.indexOf(t.id) >= 0;
        lab.appendChild(cb);
        lab.appendChild(document.createTextNode(' ' + (t.name || ('目标 #' + t.id))
          + ' (#' + t.id + '，' + (t.method || '-') + ')'));
        watchBox.appendChild(lab);
      });
    }).catch(function () {
      clear(watchBox);
      watchBox.appendChild(el('span', 'muted', '探测目标载入失败'));
    });

    var row = el('div', 'row');
    row.appendChild(btn('保存修改', 'primary', function () {
      var body = {
        name: $('n-name').value.trim(),
        group: $('n-group').value.trim(),
        sort_order: parseInt($('n-sort').value, 10) || 0,
        ip_v4_list: splitIPInput($('n-ip4-list').value),
        ip_v6_list: splitIPInput($('n-ip6-list').value),
        alert_probe_targets: Array.prototype.slice.call(
          document.querySelectorAll('#n-watch-box input[type=checkbox]'))
          .filter(function (c) { return c.checked; })
          .map(function (c) { return parseInt(c.value, 10); }),
        provider: $('n-provider').value.trim(),
        location: $('n-location').value.trim(),
        country: $('n-country').value,
        note: $('n-note').value.trim(),
        public_visible: chk.checked
      };
      guard(api('/nodes/' + n.id, { method: 'PATCH', body: body }), '已保存')
        .then(function () { loadNodeDetail(n.id); loadNodes(); });
    }));
    row.appendChild(btn('重新签发令牌', 'ghost', function () { reenroll(n.id); }));
    row.appendChild(btn('归档节点', 'ghost', function () { archiveNode(n.id); }));
    row.appendChild(btn('删除节点', 'ghost danger', function () { deleteNode(n.id, n.name); }));
    panel.appendChild(row);
    wrap.appendChild(panel);

    // 一键安装
    var inst = el('div', 'panel');
    inst.appendChild(el('h3', null, '一键安装命令（一次性注册码，30 分钟内有效）'));
    var irow = el('div', 'row');
    var plat = el('select');
    plat.id = 'inst-platform';
    [['shell', 'Linux / macOS / FreeBSD'], ['wget', '无 curl 环境'], ['powershell', 'Windows PowerShell'], ['step_by_step', '分步命令（先审查再执行）']]
      .forEach(function (p) {
        var o = el('option', null, p[1]);
        o.value = p[0];
        plat.appendChild(o);
      });
    irow.appendChild(plat);
    irow.appendChild(btn('生成命令', 'primary', function () {
      guard(api('/nodes/' + n.id + '/install-command', { method: 'POST', body: { platform: plat.value } }))
        .then(function (r) {
          if (!r) return;
          var box = $('inst-out');
          clear(box);
          box.appendChild(el('div', 'muted', '注册码：' + r.code + '，过期时间 ' + fmtTime(r.expires_at)));
          ['shell', 'wget', 'powershell', 'step_by_step'].forEach(function (k) {
            if (!r.commands[k]) return;
            box.appendChild(el('div', 'muted', k));
            box.appendChild(el('pre', 'code', r.commands[k]));
          });
          // 生成命令即签发了一个新注册码，下方列表同步刷新，
          // 否则"刚签发的码"在面板里要等到下次打开详情才出现。
          loadEnrollCodes(n.id);
        });
    }));
    inst.appendChild(irow);
    var out = el('div');
    out.id = 'inst-out';
    inst.appendChild(out);
    wrap.appendChild(inst);

    // 令牌
    var tok = el('div', 'panel');
    tok.appendChild(el('h3', null, '令牌（仅显示前 6 位）'));
    var twrap = el('div', 'table-wrap');
    var t = el('table');
    t.id = 'tokens-table';
    var thead = el('thead');
    var htr = el('tr');
    ['ID', '提示', '创建时间', '最后使用', '最后来源 IP', '状态', '操作'].forEach(function (x) { htr.appendChild(el('th', null, x)); });
    thead.appendChild(htr);
    t.appendChild(thead);
    var tb = el('tbody');
    tb.id = 'tokens-tbody';
    t.appendChild(tb);
    twrap.appendChild(t);
    tok.appendChild(twrap);
    // 注册码：一次性、短时有效。曾经"签发即失踪"——对话框一关就无处可查，
    // 管理员手里是否还有效码、401 是因为用过还是过期，全都无从判断。
    // 现在与令牌同面板展示（前 6 位提示 + 状态由服务端按时钟算好）。
    tok.appendChild(el('h3', null, '注册码（一次性，仅显示前 6 位）'));
    var cwrap = el('div', 'table-wrap');
    var ct = el('table');
    ct.id = 'codes-table';
    var cthead = el('thead');
    var chtr = el('tr');
    ['提示', '签发时间', '有效期至', '状态'].forEach(function (x) { chtr.appendChild(el('th', null, x)); });
    cthead.appendChild(chtr);
    ct.appendChild(cthead);
    var ctb = el('tbody');
    ctb.id = 'codes-tbody';
    ct.appendChild(ctb);
    cwrap.appendChild(ct);
    tok.appendChild(cwrap);
    tok.appendChild(el('p', 'muted',
      '注册码是一次性的：被使用或过期后立即失效，需重新签发。' +
      '每节点同时存在的有效注册码数量可在系统设置「每节点注册码上限」调整。'));
    wrap.appendChild(tok);

    return wrap;
  }

  // reenroll 重新签发令牌。
  //
  // 两步走，都在站内对话框里（以前是"点了就发"再弹一个原生 alert 显示注册码）：
  //   1. 先说清楚会发生什么 —— 尤其是"旧令牌不会自动吊销"，
  //      因为用这个按钮的人往往正是怀疑令牌泄漏的那一个；
  //   2. 把注册码与要照抄的命令一起给出来，可一键复制。
  // 原来那个写法有两个实际问题：没有任何前置说明（点一下就已经签发了），
  // 而结果弹窗一旦被浏览器"阻止此页面再显示对话框"吞掉，界面上**毫无反应**，
  // 管理员只会以为按钮坏了 —— 注册码也因此拿不到。
  function reenroll(id) {
    var n = null;
    (state.nodes || []).forEach(function (x) { if (x.id === id) n = x; });
    var who = n && n.name ? ('「' + n.name + '」') : ('#' + id);

    askDialog({
      title: '重新签发令牌',
      desc: '为节点 ' + who + ' 签发一个新的注册码。节点用它到本地换一个新令牌。' +
        '旧令牌不会被自动吊销 —— 若你怀疑它已经泄漏，请在下面的令牌列表里手动吊销。',
      extra: '在你到节点上执行命令之前，节点仍用旧令牌正常上报。',
      okText: '签发注册码'
    }).then(function (ok) {
      if (!ok.ok) return;
      guard(api('/nodes/' + id + '/enroll-codes', { method: 'POST' })).then(function (r) {
        if (!r) return;
        askDialog({
          title: '注册码已签发',
          desc: '到节点上执行下面这条命令完成重新注册：',
          value: 'xb-probe-agent reenroll --code ' + r.code,
          extra: '有效期至 ' + fmtTime(r.expires_at) + '。注册码是一次性的，过期后需重新签发。',
          okText: '知道了',
          hideCancel: true
        });
        loadTokens(id);
      });
    });
  }

  function loadTokens(id) {
    api('/nodes/' + id + '/tokens').then(function (list) {
      var tb = $('tokens-tbody');
      if (!tb) return;
      clear(tb);
      (list || []).forEach(function (t) {
        var tr = el('tr');
        tr.appendChild(td(t.id));
        tr.appendChild(td(t.hint + '…'));
        tr.appendChild(td(fmtTime(t.created_at)));
        tr.appendChild(td(t.last_used_at ? ago(t.last_used_at) : '未使用'));
        tr.appendChild(td(t.last_ip || '-'));
        tr.appendChild(td(t.revoked ? '已吊销' : '有效'));
        var ops = el('td');
        if (!t.revoked) {
          ops.appendChild(btn('吊销', 'ghost mini danger', function () {
            askDialog({
              title: '吊销令牌 #' + t.id,
              desc: '吊销后该节点后续请求立即返回 403，需重新注册才能恢复上报。',
              okText: '吊销'
            }).then(function (r) {
              if (!r.ok) return;
              guard(api('/nodes/' + id + '/tokens/' + t.id, { method: 'DELETE' }), '已吊销')
                .then(function () { loadTokens(id); });
            });
          }));
        }
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
    }).catch(function () { /* 忽略 */ });
    loadEnrollCodes(id);
  }

  // loadEnrollCodes 渲染注册码列表。状态是服务端算好的（active/used/expired）：
  // 各端时钟不一致时，"还剩 3 分钟"被浏览器算成"已过期"只会制造困惑。
  function loadEnrollCodes(id) {
    api('/nodes/' + id + '/enroll-codes').then(function (list) {
      var tb = $('codes-tbody');
      if (!tb) return;
      clear(tb);
      (list || []).forEach(function (c) {
        var tr = el('tr');
        tr.appendChild(td(c.hint ? c.hint + '…' : '—'));
        tr.appendChild(td(fmtTime(c.created_at)));
        tr.appendChild(td(fmtTime(c.expires_at)));
        tr.appendChild(td(c.status === 'used' ? '已使用'
          : c.status === 'expired' ? '已过期' : '有效'));
        tb.appendChild(tr);
      });
      if (!list || !list.length) {
        var tr = el('tr');
        var cell = el('td', 'muted', '还没有签发过注册码；点上方「重新签发令牌」或「生成命令」即可。');
        cell.colSpan = 4;
        tr.appendChild(cell);
        tb.appendChild(tr);
      }
    }).catch(function () { /* 忽略 */ });
  }

  // -------------------------------------------------------------------------
  // 计费与流量（4.4）
  // -------------------------------------------------------------------------

  // 流量额度的单位换算。库里一律存字节，单位只存在于输入框上：
  // 如果连单位一起存库，日后管理员把 GB 改成 TB 就会让历史数据含义漂移。
  // QUOTA_UNITS 采用 [value, text] 格式，与 addSelect 的 [option.value, option.textContent] 一致。
  //
  // 这里**不能**写 1 << 40：JS 的位运算按 32 位有符号整数截断，1 << 40 == 256，
  // 于是"选 TB 存进去 256 字节"，节点瞬间被判定为流量超额。用乘方写才安全。
  var MB = 1024 * 1024;
  var QUOTA_UNITS = [[MB, 'MB'], [MB * 1024, 'GB'], [MB * 1024 * 1024, 'TB']];

  function quotaToBytes(value, multiplier) {
    return Math.round((value || 0) * multiplier);
  }

  // quotaToUnit 选一个"看起来最自然"的单位回填：优先能整除的最大单位
  // （1 TB 就显示 1 TB 而不是 1024 GB），除不尽再退到 GB 保留两位小数。
  function quotaToUnit(bytes) {
    var v = bytes || 0;
    for (var i = QUOTA_UNITS.length - 1; i >= 0; i--) {
      var size = QUOTA_UNITS[i][0];
      if (v >= size && v % size === 0) {
        return { value: String(v / size), multiplier: size };
      }
    }
    if (v >= QUOTA_UNITS[1][0]) {
      return { value: (v / QUOTA_UNITS[1][0]).toFixed(2), multiplier: QUOTA_UNITS[1][0] };
    }
    return { value: String(v), multiplier: QUOTA_UNITS[0][0] };
  }

  // unixToDateInput 把 Unix 秒转成 YYYY-MM-DD（日期输入框的取值格式）。
  // 用本地时间读取：与后端的"节点时区当天 23:59"最多差一天，输入框旁边
  // 会同时显示精确时间戳，管理员能立刻发现不一致。
  function unixToDateInput(sec) {
    if (!sec) return '';
    var d = new Date(sec * 1000);
    function p(n) { return (n < 10 ? '0' : '') + n; }
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate());
  }

  // attachDatePicker 自绘日历下拉，挂在 host 里、把值写进 input（YYYY-MM-DD）。
  //
  // 为什么不用 <input type="date"> 的原生弹层：弹层里的「清除 / 今天」两个
  // 按钮的颜色在 Chromium 源码里是硬编码的 #0078D4（calendar_picker.css 的
  // .clear-button/.today-button），页面上的任何 CSS 都改不到——它是全站
  // 黑白灰阶里唯一的彩色，而且无解，只能自己画。输入框本身保留 text 类型
  // （readonly），保存逻辑读 $('b-expire').value 与原生日期输入完全一致。
  function attachDatePicker(host, input) {
    var pop = el('div', 'date-pop');
    pop.hidden = true;
    var head = el('div', 'dp-head');
    var prev = el('button', 'dp-nav', '‹');
    prev.type = 'button';
    prev.title = '上个月';
    var lab = el('span', 'dp-month');
    var next = el('button', 'dp-nav', '›');
    next.type = 'button';
    next.title = '下个月';
    head.appendChild(prev); head.appendChild(lab); head.appendChild(next);
    var grid = el('div', 'dp-grid');
    var foot = el('div', 'dp-foot');
    var clearB = el('button', 'dp-act', '清除');
    var todayB = el('button', 'dp-act', '今天');
    clearB.type = 'button'; todayB.type = 'button';
    foot.appendChild(clearB); foot.appendChild(todayB);
    pop.appendChild(head); pop.appendChild(grid); pop.appendChild(foot);
    host.appendChild(pop);

    var view = null; // 当前显示的 {y, m}，m 从 0 起

    function p2(n) { return (n < 10 ? '0' : '') + n; }
    function parseValue() {
      var m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(input.value || '');
      return m ? { y: parseInt(m[1], 10), m: parseInt(m[2], 10) - 1, d: parseInt(m[3], 10) } : null;
    }
    function monthOf(d) { return { y: d.getFullYear(), m: d.getMonth() }; }

    function render() {
      lab.textContent = view.y + ' 年 ' + (view.m + 1) + ' 月';
      clear(grid);
      // 周一做起始（与中文习惯及原生弹层一致）
      ['一', '二', '三', '四', '五', '六', '日'].forEach(function (w) {
        grid.appendChild(el('span', 'dp-wd', w));
      });
      var off = (new Date(view.y, view.m, 1).getDay() + 6) % 7;
      var days = new Date(view.y, view.m + 1, 0).getDate();
      var td = new Date(), sel = parseValue();
      // 恒 6 行 42 格：弹层高度不随月份跳动（卡片位置固定的同一条纪律）
      for (var i = 0; i < 42; i++) {
        var dnum = i - off + 1;
        if (dnum < 1 || dnum > days) {
          grid.appendChild(el('span', 'dp-day pad'));
          continue;
        }
        (function (dnum) {
          var cell = el('button', 'dp-day', String(dnum));
          cell.type = 'button';
          if (td.getFullYear() === view.y && td.getMonth() === view.m && td.getDate() === dnum) {
            cell.classList.add('today');
          }
          if (sel && sel.y === view.y && sel.m === view.m && sel.d === dnum) {
            cell.classList.add('sel');
          }
          cell.addEventListener('click', function () {
            input.value = view.y + '-' + p2(view.m + 1) + '-' + p2(dnum);
            input.dispatchEvent(new Event('change', { bubbles: true }));
            close();
          });
          grid.appendChild(cell);
        })(dnum);
      }
    }
    function open() {
      var sel = parseValue();
      view = sel ? { y: sel.y, m: sel.m } : monthOf(new Date());
      render();
      pop.hidden = false;
    }
    function close() { pop.hidden = true; }

    input.addEventListener('click', function () {
      if (input.disabled) return;
      if (pop.hidden) open(); else close();
    });
    prev.addEventListener('click', function () {
      view.m--; if (view.m < 0) { view.m = 11; view.y--; } render();
    });
    next.addEventListener('click', function () {
      view.m++; if (view.m > 11) { view.m = 0; view.y++; } render();
    });
    // 「清除」清值、「今天」只跳到今天的月份（与原生弹层同语义：跳转不赋值）
    clearB.addEventListener('click', function () {
      input.value = '';
      input.dispatchEvent(new Event('change', { bubbles: true }));
      close();
    });
    todayB.addEventListener('click', function () {
      view = monthOf(new Date());
      render();
    });
    // 点外面收起：一次性弹层没有专属关闭按钮，必须给个逃生口
    document.addEventListener('click', function (ev) {
      if (!pop.hidden && !host.contains(ev.target)) close();
    });
  }

  // renderBillingCard 渲染计费表单。
  //
  // 三处刻意取舍：
  //   1. 到期时间用日期选择器，提交 expire_date（YYYY-MM-DD）而不是 Unix 秒。
  //      换算交给后端：只有后端内嵌了完整 IANA 时区库（time/tzdata），前端用
  //      new Date 换算会在"节点时区 ≠ 浏览器时区"时整体偏一天。
  //   2. 流量额度 = 数值 + 单位两个控件，存库仍是字节。
  //   3. 计费流量 = 计量口径 × 流量倍率。为了不让这个乘法只活在文档里，
  //      倍率下面实时显示换算结果（含"等于多少真实可用流量"）。
  function renderBillingCard(n) {
    var wrap = el('div');
    wrap.appendChild(el('h2', null, '计费与流量'));
    var panel = el('div', 'panel form-grid');

    // 已保存的值随节点详情一起返回（GET /nodes/{id} → billing）。
    // 这里必须回填：以前表单永远从 0 开始，管理员只改一个字段，
    // 保存后其余字段会被静默清零。
    var b = n.billing || {};
    var savedQuota = b.traffic_quota || 0;
    var savedMult = b.traffic_multiplier > 0 ? b.traffic_multiplier : 1;

    function field(text, tip) {
      var l = el('label');
      l.title = tip || text;
      l.appendChild(el('span', 'lbl', text));
      return l;
    }
    function addInput(id, type, value) {
      var i = el('input');
      i.id = id;
      i.type = type;
      i.value = value === undefined || value === null ? '' : String(value);
      return i;
    }
    function addSelect(id, opts, value) {
      var s = el('select');
      s.id = id;
      opts.forEach(function (o) {
        var op = el('option', null, o[1]);
        op.value = o[0];
        s.appendChild(op);
      });
      s.value = value;
      return s;
    }

    var lPrice = field('价格', '单位：元。公开看板的「月费合计」会按支付周期折算成月均（年付÷12、季付÷3、一次性/永久不计）。');
    var priceInput = addInput('b-price', 'number', ((b.price_minor || 0) / 100).toFixed(2));
    priceInput.step = '0.01';
    priceInput.min = '0';
    lPrice.appendChild(priceInput);
    panel.appendChild(lPrice);

    var lCur = field('币种', '三字母货币代码，如 CNY / USD / HKD；未知代码按 ¥ 显示。');
    lCur.appendChild(addInput('b-currency', 'text', b.currency || 'CNY'));
    panel.appendChild(lCur);

    var lCycle = field('支付周期', '决定「续费」按钮每次顺延多久。选「永久」后到期日会被忽略且不参与到期扫描。');
    var cySel = addSelect('b-cycle',
      [['monthly', '月付'], ['quarterly', '季付'], ['yearly', '年付'], ['once', '一次性'], ['lifetime', '永久']],
      b.cycle || 'monthly');
    lCycle.appendChild(cySel);
    panel.appendChild(lCycle);

    var savedExpire = b.expire_at || 0;
    var lExp = field('到期日', '按所选日期当天 23:59 到期（用节点的时区计算）。留空表示未设置到期时间。');
    // 自绘日历（attachDatePicker）：原生 <input type="date"> 的弹层"清除/今天"
    // 是 Chromium 硬编码的蓝色，页面上任何 CSS 都改不到，只能不用原生弹层。
    // 取值格式仍是 YYYY-MM-DD，保存逻辑（$('b-expire').value）不变。
    var expWrap = el('div', 'date-wrap');
    var expInput = addInput('b-expire', 'text', unixToDateInput(savedExpire));
    expInput.readOnly = true;
    expInput.placeholder = 'YYYY-MM-DD';
    expInput.disabled = cySel.value === 'lifetime';
    expWrap.appendChild(expInput);
    attachDatePicker(expWrap, expInput);
    lExp.appendChild(expWrap);
    // 精确时间戳一并显示：日期控件丢失了时刻与原始 Unix 值，
    // 对账时需要能看到库里到底存的是什么。
    lExp.appendChild(el('span', 'hint', savedExpire ? '当前：' + fmtTime(savedExpire) : '当前：未设置'));
    panel.appendChild(lExp);
    cySel.addEventListener('change', function () {
      expInput.disabled = cySel.value === 'lifetime';
    });

    var q0 = quotaToUnit(savedQuota);
    var lQuota = field('流量额度', '本计费周期的额度上限，填 0 表示不限量（仍统计用量，但不做阈值告警）。');
    var quotaPair = el('div', 'field-pair');
    var quotaVal = addInput('b-quota', 'number', q0.value);
    quotaVal.step = 'any';
    quotaVal.min = '0';
    var quotaUnit = addSelect('b-quota-unit', QUOTA_UNITS, String(q0.multiplier));
    quotaPair.appendChild(quotaVal);
    quotaPair.appendChild(quotaUnit);
    lQuota.appendChild(quotaPair);
    panel.appendChild(lQuota);

    var lMode = field('计量口径', '决定用哪个方向的数据去比额度：相加＝上下行都算，仅上行/仅下行＝单方向，取较大＝只算大的那一向。');
    var mdSel = addSelect('b-mode',
      [['both', '上下行相加'], ['up', '仅上行'], ['down', '仅下行'], ['max', '取较大方向']],
      b.traffic_mode || 'both');
    lMode.appendChild(mdSel);
    panel.appendChild(lMode);

    var lMult = field('流量倍率', '计费流量 = 计量口径 × 流量倍率。机房只统计单方向却按倍数扣量时填对应倍数（1:3 就填 3）。填 0 与填 1 等效。');
    var multInput = addInput('b-mult', 'number', savedMult);
    multInput.step = '0.01';
    multInput.min = '0';
    lMult.appendChild(multInput);
    panel.appendChild(lMult);

    // 倍率换算提示：把「总流量 = 计量口径 × 倍率」从一句抽象约定变成看得见的
    // 数字。否则管理员填完 3 并不知道自己还剩多少真实可用流量。
    var hint = el('p', 'muted full');
    hint.id = 'b-mult-hint';
    panel.appendChild(hint);

    function currentMult() {
      var m = parseFloat(multInput.value);
      return m > 0 ? m : 1;
    }
    function refreshHint() {
      var m = currentMult();
      var q = quotaToBytes(parseFloat(quotaVal.value) || 0, quotaUnit.value);
      if (q <= 0) {
        hint.textContent = '额度不限量；计费流量 = 计量口径 × ' + m + '。';
        return;
      }
      hint.textContent = '额度 ' + fmtBytes(q) + ' ÷ 倍率 ' + m +
        ' → 计量口径最多可用 ' + fmtBytes(Math.round(q / m)) + '（超出部分即超额）。';
    }
    quotaVal.addEventListener('input', refreshHint);
    quotaUnit.addEventListener('change', refreshHint);
    multInput.addEventListener('input', refreshHint);
    refreshHint();

    var lReset = field('每月重置日', '1-31。用于流量周期的起点；当月没有该日（如 31 号）时收敛到当月最后一天。');
    lReset.appendChild(addInput('b-reset', 'number', b.reset_day || 1));
    panel.appendChild(lReset);

    var lTZ = field('时区', 'IANA 名称（如 Asia/Shanghai），留空用全局时区。影响周期起止与「今日流量」的日界。');
    lTZ.appendChild(addInput('b-tz', 'text', b.tz || ''));
    panel.appendChild(lTZ);

    var row = el('div', 'row');
    row.appendChild(btn('保存计费', 'primary', function () {
      var body = {
        // 前端按"元"展示，后端存"最小货币单位"（分）。
        price_minor: Math.round(parseFloat($('b-price').value) * 100) || 0,
        currency: $('b-currency').value.trim(),
        cycle: $('b-cycle').value,
        // 只发 expire_date：换算放在后端，见函数头注释。
        expire_date: $('b-expire').value || '',
        traffic_quota: quotaToBytes(parseFloat($('b-quota').value) || 0, parseInt($('b-quota-unit').value, 10)),
        traffic_mode: $('b-mode').value,
        reset_day: parseInt($('b-reset').value, 10) || 1,
        tz: $('b-tz').value.trim(),
        traffic_multiplier: currentMult()
      };
      guard(api('/nodes/' + n.id + '/billing', { method: 'PUT', body: body }), '计费已保存')
        .then(function () { loadNodeDetail(n.id); });
    }));
    row.appendChild(btn('续费（按周期顺延）', 'ghost', function () {
      guard(api('/nodes/' + n.id + '/billing/renew', { method: 'POST' }))
        .then(function (r) { if (r) toast('新到期时间：' + fmtTime(r.expire_at)); loadNodeDetail(n.id); });
    }));
    panel.appendChild(row);
    wrap.appendChild(panel);

    // 流量明细
    var tr = el('div', 'panel');
    tr.appendChild(el('h3', null, '流量'));
    var box = el('div');
    box.id = 'traffic-box';
    box.appendChild(el('p', 'muted', '加载中…'));
    tr.appendChild(box);
    var trow = el('div', 'row');
    trow.appendChild(btn('看最近 30 天按日明细', 'ghost', function () {
      api('/nodes/' + n.id + '/traffic?period=daily&from=' + todayStr(-30) + '&to=' + todayStr(0))
        .then(function (days) {
          clear(box);
          if (!days || !days.length) { box.appendChild(el('p', 'muted', '暂无按日数据。')); return; }
          var wrap2 = el('div', 'table-wrap');
          var t2 = el('table');
          var thead = el('thead');
          var htr = el('tr');
          ['日期', '下行', '上行', '合计'].forEach(function (x) { htr.appendChild(el('th', null, x)); });
          thead.appendChild(htr);
          t2.appendChild(thead);
          var tb = el('tbody');
          days.forEach(function (d) {
            var r = el('tr');
            r.appendChild(td(d.day));
            r.appendChild(td(fmtBytes(d.rx)));
            r.appendChild(td(fmtBytes(d.tx)));
            r.appendChild(td(fmtBytes((d.rx || 0) + (d.tx || 0))));
            tb.appendChild(r);
          });
          t2.appendChild(tb);
          wrap2.appendChild(t2);
          // 返回入口：明细视图替换了总览内容，没有它管理员只能重进详情页。
          var back = btn('返回总览', 'ghost mini', function () { loadTraffic(n); });
          box.appendChild(back);
          box.appendChild(wrap2);
        }).catch(function (e) { toast(e.message, true); });
    }));
    tr.appendChild(trow);
    wrap.appendChild(tr);
    return wrap;
  }

  // loadTraffic 渲染「流量」面板：本周期已用量 + 手工矫正入口。
  //
  // 需要整个节点对象（不只是 id）才能拿到 n.billing：矫正框里要按当前节点
  // 的计量口径与倍率实时显示"折合计费流量"，否则管理员填完数字无从判断
  // 这个值到底对应页面上哪个百分比。
  function loadTraffic(n) {
    var id = n.id;
    api('/nodes/' + id + '/traffic?period=current').then(function (st) {
      var box = $('traffic-box');
      if (!box) return;
      clear(box);
      if (!st) {
        box.appendChild(el('p', 'muted', '尚无流量记录。'));
        renderTrafficOverride(box, n, 0, 0);
        return;
      }
      var dl = el('dl', 'kv');
      [['周期起点', fmtTime(st.period_start)], ['已用下行', fmtBytes(st.period_rx)],
        ['已用上行', fmtBytes(st.period_tx)], ['合计', fmtBytes((st.period_rx || 0) + (st.period_tx || 0))],
        ['更新时间', fmtTime(st.updated_at)]].forEach(function (kv) {
        dl.appendChild(el('dt', null, kv[0]));
        dl.appendChild(el('dd', null, kv[1]));
      });
      box.appendChild(dl);
      renderTrafficOverride(box, n, st.period_rx || 0, st.period_tx || 0);
    }).catch(function () {
      var box = $('traffic-box');
      if (box) { clear(box); box.appendChild(el('p', 'muted', '尚无流量记录。')); }
    });
  }

  // renderTrafficOverride 渲染「矫正本周期已用流量」表单。
  //
  // 为什么矫正的是**原始计量值**（period_rx / period_tx）而不是页面上那个
  // 折算后的计费流量：反推要先解一遍 ApplyMode + ApplyMultiplier，而
  // `取较大方向` 这类口径根本反推不出唯一解（把目标值给哪一向都成立）。
  // 直接编辑原始值只需要"看到什么就改什么"——输入框里的数字就是
  // 上面 dl 里显示的两个数字，改完下方立刻给出折算结果。
  function renderTrafficOverride(box, n, rx, tx) {
    var b = n.billing || {};
    var mode = b.traffic_mode || 'both';
    var mult = b.traffic_multiplier > 0 ? b.traffic_multiplier : 1;

    box.appendChild(el('h3', null, '手工矫正本周期用量'));
    box.appendChild(el('p', 'muted',
      '用途：Agent 重装后累计值从零开始、或运营商侧已计入的流量需要在面板上对齐。' +
      '两栏都填 0 即清零本周期用量；只改本周期合计，不会改写按日明细（历史事实）。'));

    var pair = el('div', 'field-pair');
    function unitInput(label, id, bytes) {
      var wrap = el('label');
      wrap.title = label + '的原始计量值（未折算、未乘倍率）';
      wrap.appendChild(el('span', 'lbl', label));
      var row = el('div', 'field-pair');
      var q = quotaToUnit(bytes || 0);
      var val = el('input');
      val.id = id;
      val.type = 'number';
      val.step = 'any';
      val.min = '0';
      val.value = q.value;
      var unit = el('select');
      unit.id = id + '-unit';
      QUOTA_UNITS.forEach(function (o) {
        var op = el('option', null, o[1]);
        op.value = String(o[0]);
        unit.appendChild(op);
      });
      unit.value = String(q.multiplier);
      row.appendChild(val);
      row.appendChild(unit);
      wrap.appendChild(row);
      return wrap;
    }
    pair.appendChild(unitInput('已用下行', 't-rx', rx));
    pair.appendChild(unitInput('已用上行', 't-tx', tx));
    box.appendChild(pair);

    var hint = el('p', 'muted');
    box.appendChild(hint);
    function current() {
      return {
        rx: quotaToBytes(parseFloat($('t-rx').value) || 0, parseInt($('t-rx-unit').value, 10)),
        tx: quotaToBytes(parseFloat($('t-tx').value) || 0, parseInt($('t-tx-unit').value, 10))
      };
    }
    // 折算提示：让"口径 + 倍率"这个乘法在矫正现场可见。否则管理员填完
    // 100 GB 完全不知道进度条会跳到哪。
    function refreshHint() {
      var v = current();
      var used = billable(v.rx, v.tx, mode, mult);
      var quota = b.traffic_quota || 0;
      var s = '折合计费流量 = ' + fmtBytes(used) + '（口径' + modeLabel(mode) + ' × 倍率 ' + mult + '）';
      if (quota > 0) {
        s += '；额度 ' + fmtBytes(quota) + '，约占 ' + (trafficPct(used, quota) / 100).toFixed(2) + '%';
      }
      hint.textContent = s;
    }
    ['t-rx', 't-tx'].forEach(function (id) {
      $(id).addEventListener('input', refreshHint);
      $(id + '-unit').addEventListener('change', refreshHint);
    });
    refreshHint();

    var row = el('div', 'row');
    row.appendChild(btn('保存矫正', 'primary', function () {
      var v = current();
      guard(api('/nodes/' + n.id + '/traffic', { method: 'PUT', body: { period_rx: v.rx, period_tx: v.tx } }),
        '已矫正本周期用量')
        .then(function () { loadTraffic(n); });
    }));
    box.appendChild(row);
  }

  function modeLabel(mode) {
    return { both: '上下行相加', up: '仅上行', down: '仅下行', max: '取较大方向' }[mode] || mode;
  }

  // trafficPct 与后端 traffic.Percent 同口径：万分比，0~10000，额度不限时 0。
  function trafficPct(used, quota) {
    if (quota <= 0 || used <= 0) return 0;
    if (used >= quota) return 10000;
    return Math.floor((used / quota) * 10000);
  }

  // billable 与后端 traffic.Billable 同口径：先按计量口径取一个数值，再乘倍率。
  function billable(rx, tx, mode, mult) {
    var used = mode === 'up' ? tx : mode === 'down' ? rx : mode === 'max' ? Math.max(rx, tx) : rx + tx;
    return Math.round(used * mult);
  }

  function renderHistoryCard(n) {
    var wrap = el('div');
    wrap.appendChild(el('h2', null, '历史曲线'));
    var bar = el('div', 'row');

    var tabs = el('div', 'tab-bar');
    var tabRes = el('button', 'tab' + (state.nodeHistoryTab === 'resources' ? ' active' : ''), '资源监控');
    tabRes.type = 'button';
    tabRes.dataset.tab = 'resources';
    var tabLat = el('button', 'tab' + (state.nodeHistoryTab === 'latency' ? ' active' : ''), '探测延迟');
    tabLat.type = 'button';
    tabLat.dataset.tab = 'latency';
    tabs.appendChild(tabRes);
    tabs.appendChild(tabLat);
    bar.appendChild(tabs);
    bar.appendChild(el('span', 'spacer'));

    var sel = el('select');
    sel.id = 'hist-range';
    [['30m', '最近 30 分钟'], ['1h', '最近 1 小时'], ['5h', '最近 5 小时'], ['24h', '最近 24 小时'], ['7d', '最近 7 天'], ['30d', '最近 30 天']]
      .forEach(function (o) {
        var op = el('option', null, o[1]);
        op.value = o[0];
        sel.appendChild(op);
      });
    sel.value = '24h';
    bar.appendChild(sel);
    bar.appendChild(btn('加载', 'ghost', function () { loadHistory(n.id, sel.value); }));
    wrap.appendChild(bar);

    tabs.addEventListener('click', function (ev) {
      if (!ev.target || !ev.target.dataset || !ev.target.dataset.tab) return;
      state.nodeHistoryTab = ev.target.dataset.tab;
      updateHistoryTabs();
      loadHistory(n.id, sel.value);
    });

    function updateHistoryTabs() {
      tabRes.classList.toggle('active', state.nodeHistoryTab === 'resources');
      tabLat.classList.toggle('active', state.nodeHistoryTab === 'latency');
      var pRes = $('panel-resources');
      var pLat = $('panel-latency');
      if (pRes) pRes.hidden = state.nodeHistoryTab !== 'resources';
      if (pLat) pLat.hidden = state.nodeHistoryTab !== 'latency';
    }

    // 资源监控面板
    var pRes = el('div', 'tab-panel');
    pRes.id = 'panel-resources';
    pRes.hidden = state.nodeHistoryTab !== 'resources';
    var grid = el('div', 'charts');
    ['cpu', 'mem', 'net_rx', 'net_tx', 'disk_used'].forEach(function (m) {
      var c = el('div', 'card');
      c.appendChild(el('div', 'k', labelOfMetric(m)));
      var cv = document.createElement('canvas');
      cv.className = 'chart';
      cv.id = 'chart-' + m;
      cv.setAttribute('role', 'img');
      cv.setAttribute('aria-label', labelOfMetric(m) + ' 历史曲线');
      c.appendChild(cv);
      grid.appendChild(c);
    });
    pRes.appendChild(grid);
    wrap.appendChild(pRes);

    // 探测延迟面板
    var pLat = el('div', 'tab-panel');
    pLat.id = 'panel-latency';
    pLat.hidden = state.nodeHistoryTab !== 'latency';
    var latCard = el('div', 'card section-gap');
    latCard.appendChild(el('div', 'k', '探测延迟（各目标）'));
    var lcv = document.createElement('canvas');
    lcv.className = 'chart';
    lcv.id = 'chart-latency';
    lcv.setAttribute('role', 'img');
    lcv.setAttribute('aria-label', '各探测目标延迟曲线');
    latCard.appendChild(lcv);
    var legend = el('div', 'legend');
    legend.id = 'admin-latency-legend';
    latCard.appendChild(legend);
    pLat.appendChild(latCard);

    pLat.appendChild(el('h3', null, '延迟明细'));
    var list = el('div', 'grid cards-3');
    list.id = 'admin-latency-list';
    pLat.appendChild(list);

    // 目标管理（全局只读展示 + 节点专属增删改）只放在延迟标签里：
    // 它曾经挂在 node-body 末尾，视觉上跟着两个标签一起出现，像重复了两份。
    pLat.appendChild(el('h3', null, '该节点的探测目标（全局 + 专属）'));
    var tgt = el('div', null);
    tgt.id = 'node-targets-slot';
    tgt.appendChild(el('p', 'muted', '加载中…'));
    pLat.appendChild(tgt);

    wrap.appendChild(pLat);

    setTimeout(function () { loadHistory(n.id, '24h'); }, 0);
    return wrap;
  }

  function labelOfMetric(m) {
    switch (m) {
      case 'cpu': return 'CPU 使用率';
      case 'mem': return '内存已用';
      case 'disk_used': return '磁盘已用';
      case 'net_rx': return '下行带宽';
      case 'net_tx': return '上行带宽';
      default: return m;
    }
  }

  // unitOfMetric 决定坐标轴怎么格式化。
  //
  // 这里曾经把所有非网速指标都当百分比（`net_* ? 'bps' : 'pct'`），
  // 而 mem / disk_used 实际是**字节**——于是内存坐标轴标成了 32000000%，
  // 曲线形状是对的、刻度完全没法读。（同理，公开看板那边的 unitOf 也修过一遍。）
  function unitOfMetric(m) {
    if (m === 'net_rx' || m === 'net_tx') return 'bps';
    if (m === 'cpu') return 'pct';
    return 'bytes';
  }

  // adminLatItems 缓存当前节点+跨度的延迟序列：图例点击开关只重画曲线，
  // 不重新发请求（数据没变，变了的是"哪些线要画"）。
  var adminLatItems = null;

  function loadHistory(id, range) {
    state.histRange = range;
    var win = axisWindow(range);
    if (state.nodeHistoryTab === 'resources') {
      api('/nodes/' + id + '/history?range=' + range + '&metrics=cpu,mem,net_rx,net_tx,disk_used')
        .then(function (series) {
          (series || []).forEach(function (s) {
            var cv = $('chart-' + s.metric);
            if (cv) drawSeries(cv, s.points, s.metric, win);
          });
        }).catch(function (e) { toast('历史数据加载失败：' + e.message, true); });
    }

    if (state.nodeHistoryTab === 'latency') {
      api('/nodes/' + id + '/latency?range=' + range).then(function (list) {
        adminLatItems = list || [];
        redrawAdminLatency();
      }).catch(function (e) { toast('延迟数据加载失败：' + e.message, true); });
    }
  }

  // redrawAdminLatency 按当前开关状态重画延迟图 + 图例 + 明细。
  // 颜色/线型按目标序号取灰阶色阶（与公开看板同一张 PALETTE 表）：
  // 序号在开关切换前后保持稳定，隐藏一条不会让其余目标窜色；
  // 颜色不再跟随窗口平均 RTT——多序列的可辨性优先于"线上有语义"，
  // 严重程度由下面的明细小卡（latencyColor 点）表达。
  function redrawAdminLatency() {
    var cv = $('chart-latency');
    if (!cv || !adminLatItems) return;
    var allSets = adminLatItems.map(function (t, i) {
      var noData = t.avg_rtt == null;
      return {
        name: t.name || ('目标 #' + t.target_id),
        points: t.points || [],
        unit: 'us',
        color: noData ? cssVar('--muted', '#868686') : paletteColor(i),
        dash: noData ? null : paletteDash(i)
      };
    });
    var sets = allSets.filter(function (s, i) {
      return state.latToggles[adminLatItems[i].target_id] !== false;
    });
    drawMulti(cv, sets, axisWindow(state.histRange || '24h'));
    renderAdminLatencyLegend(adminLatItems);
    renderAdminLatencyList(adminLatItems);
  }

  // renderAdminLatencyLegend / renderAdminLatencyList：后台详情页延迟标签的图例与明细。
  // 图例可点击：与公开看板一致，单击隐藏/显示某条目标的折线。
  // 图例小点的颜色 = 折线本身的序号色（不是严重度色），两边必须一眼对得上。
  function renderAdminLatencyLegend(items) {
    var box = $('admin-latency-legend');
    if (!box) return;
    clear(box);
    if (!items || !items.length) return;
    items.forEach(function (t, i) {
      var color = t.avg_rtt == null ? cssVar('--muted', '#868686') : paletteColor(i);
      var on = state.latToggles[t.target_id] !== false;
      var item = el('span', 'item' + (on ? '' : ' off'));
      var dot = el('span', 'dot');
      dot.style.backgroundColor = color;
      item.appendChild(dot);
      item.appendChild(document.createTextNode(
        (t.name || ('目标 #' + t.target_id)) +
        (t.avg_rtt == null ? '（无数据）' : ' ' + (t.avg_rtt / 1000).toFixed(1) + ' ms')));
      item.title = on ? '点击隐藏该目标的折线' : '点击显示该目标的折线';
      item.addEventListener('click', function () {
        state.latToggles[t.target_id] = !on;
        redrawAdminLatency();
      });
      box.appendChild(item);
    });
  }

  function renderAdminLatencyList(items) {
    var box = $('admin-latency-list');
    if (!box) return;
    clear(box);
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
      // 与公开详情页同一套文案口径：名字里已经带运营商的（"电信-上海"）不重复补后缀。
      var carrier = CARRIER[t.carrier] || '';
      var title = t.name || targetNameOf(t.target_id);
      if (carrier && title.indexOf(carrier) < 0) title += ' · ' + carrier;
      k.appendChild(document.createTextNode(title));
      c.appendChild(k);
      c.appendChild(el('div', 'v', t.avg_rtt == null ? '无数据' : (t.avg_rtt / 1000).toFixed(1) + ' ms'));
      c.appendChild(el('div', 'sub', t.avg_rtt == null
        ? '窗口内未探通 · 状态 ' + (STATUS_NAME[t.status] || t.status || '未知')
        : '最小 ' + (t.min_rtt / 1000).toFixed(1) + ' · 最大 ' + (t.max_rtt / 1000).toFixed(1) +
          ' ms · 丢包 ' + ((t.loss || 0) / 100).toFixed(1) + '% · ' +
          (STATUS_NAME[t.status] || t.status || '')));
      box.appendChild(c);
    });
  }

  // -------------------------------------------------------------------------
  // Canvas 图表（无外部依赖，CSP 只允许 'self'）
  // -------------------------------------------------------------------------

  function cssVar(name, fallback) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name);
    return (v && v.trim()) || fallback;
  }

  // 灰阶图表调色板（与公开看板 app.js 同一张表，改要两边一起改）：
  // 灰度可辨的档位只有两三档，多序列的区分度由 PALETTE_DASH 的虚线线型补足
  // （Canvas 侧 setLineDash，图例同步画虚线段）。取 cssVar 实色，
  // 因此深浅切换不需要换算，重画一遍就是新主题的颜色。
  var PALETTE = ['--c1', '--c2', '--c3', '--c4', '--c5', '--c2'];
  var PALETTE_DASH = [null, [6, 3], [2, 3], [10, 4, 2, 4], [1, 4], [8, 3]];
  function paletteColor(i) { return cssVar(PALETTE[i % PALETTE.length], '#555555'); }
  function paletteDash(i) { return PALETTE_DASH[i % PALETTE_DASH.length]; }

  function prepareCanvas(cv) {
    var dpr = window.devicePixelRatio || 1;
    var w = cv.clientWidth || 320;
    var h = cv.clientHeight || 180;
    cv.width = Math.round(w * dpr);
    cv.height = Math.round(h * dpr);
    var ctx = cv.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    return { ctx: ctx, w: w, h: h };
  }

  function drawEmpty(cv, text) {
    var p = prepareCanvas(cv);
    p.ctx.fillStyle = cssVar('--muted', '#868686');
    p.ctx.font = '12px sans-serif';
    p.ctx.fillText(text, 10, p.h / 2);
  }

  // drawSpark 画一条无坐标轴的迷你趋势线（汇总卡里的聚合下行带宽）。
  // 点数不足 2 个时不画线，避免画出一条误导性的直线。
  function drawSpark() {
    var cv = $('spark');
    if (!cv) return;
    var p = prepareCanvas(cv);
    if (spark.length < 2) return;

    var max = 0;
    for (var i = 0; i < spark.length; i++) if (spark[i] > max) max = spark[i];
    if (max <= 0) return;

    var stepX = p.w / (spark.length - 1);
    p.ctx.beginPath();
    for (var j = 0; j < spark.length; j++) {
      var x = j * stepX;
      var y = p.h - (spark[j] / max) * (p.h - 2) - 1;
      if (j === 0) p.ctx.moveTo(x, y); else p.ctx.lineTo(x, y);
    }
    p.ctx.strokeStyle = cssVar('--dir-down', '#868686');
    p.ctx.lineWidth = 1.5;
    p.ctx.stroke();
  }

  // drawSeries 画单条曲线；unit 由指标决定，不再是"非网速即百分比"。
  function drawSeries(cv, points, metric, axisWin, hoverTs) {
    drawMulti(cv, [{ name: '', points: points || [], unit: unitOfMetric(metric) }], axisWin, hoverTs);
  }

  // rangeSeconds / axisWindow：把 X 轴钉在完整请求窗口上（与公开看板
  // app.js 的同名逻辑保持一致，改要一起改）。
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
  function axisWindow(range) {
    var now = Math.floor(Date.now() / 1000);
    return { tMin: now - rangeSeconds(range), tMax: now };
  }

  // -------------------------------------------------------------------------
  // 图表刻度
  // -------------------------------------------------------------------------
  //
  // 这一段与 internal/theme/builtin/app.js 是**有意重复**的，理由和 CSS 那边一样：
  // 按 5.2 管理后台样式/脚本固定、不随可替换的公开主题变化，
  // 两个前端在物理上也是两套 embed 资源（公开 18280 / 后台 18281，不同端口）。
  // 改这里的刻度算法时，请同步改 app.js 里对应的几个函数。

  var NICE_STEPS = [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10];
  var NICE_BINARY = [1, 2, 2.5, 3, 4, 5, 6, 8, 10, 12.5, 16, 20, 25, 40, 50,
                     80, 100, 125, 200, 250, 500, 625, 800, 1000];

  // niceTop 返回"整齐"的 Y 轴上限（区间切成 4 段）。binary 为真时按 1024 进制取整。
  function niceTop(v, binary) {
    if (!(v > 0)) return binary ? 1024 : 1;
    var rough = v * 1.12 / 4;
    var base, steps, i;
    if (binary) {
      base = 1;
      while (base * 1024 <= rough && base < 1099511627776) base *= 1024;
      steps = NICE_BINARY;
      for (i = 0; i < steps.length; i++) {
        if (steps[i] * base >= rough) return steps[i] * base * 4;
      }
      return 4000 * base;
    }
    base = Math.pow(10, Math.floor(Math.log(rough) / Math.LN10));
    for (i = 0; i < NICE_STEPS.length; i++) {
      if (NICE_STEPS[i] * base >= rough) return NICE_STEPS[i] * base * 4;
    }
    return 40 * base;
  }

  function fmtVal(v, unit) {
    if (unit === 'pct') return (v / 100).toFixed(1) + '%';
    if (unit === 'us') return (v / 1000).toFixed(1) + ' ms';
    if (unit === 'bps') return fmtBps(v);
    return fmtBytes(v);
  }

  function fmtAxis(v, unit) {
    if (unit === 'pct') {
      var n = v / 100;
      return (n % 1 === 0 ? n : n.toFixed(1)) + '%';
    }
    if (unit === 'us') return (v / 1000).toFixed(0) + ' ms';
    if (unit === 'bps') return fmtBytes(v) + '/s';
    return fmtBytes(v);
  }

  // gapThreshold / segmentsOf —— 断档检测，与 app.js 里同名函数逻辑一致。
  //
  // 节点或 Master 断联期间不会落任何样本，时间序列里那一段就是空的。
  // 若不检测，前后两点会被直接连成一条直线，"断联 6 小时"在后台图上就
  // 显示成一条平滑水平线——故障被画成了健康。详细理由见 app.js 的注释。
  //
  // 阈值取该序列相邻点间隔的**中位数 ×3**：写死秒数在 3 秒（内存层）与
  // 600 秒（10 分钟层）之间必然误判一头。样本不足 4 点时返回 Infinity。
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
    var median = gaps.length % 2 ? gaps[mid] : (gaps[mid - 1] + gaps[mid]) / 2;
    if (!(median > 0)) return Infinity;
    return median * 3;
  }

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

  var TIME_STEPS = [60, 120, 300, 600, 900, 1800, 3600, 7200, 3 * 3600,
                    6 * 3600, 12 * 3600, 86400, 2 * 86400, 7 * 86400, 14 * 86400];

  // timeTicks 返回对齐到自然时间边界的刻度（整 5 分钟 / 整点 / 本地零点）。
  function timeTicks(tMin, tMax, want) {
    var span = Math.max(1, tMax - tMin);
    var target = span / Math.max(2, want);
    var step = TIME_STEPS[TIME_STEPS.length - 1];
    for (var i = 0; i < TIME_STEPS.length; i++) {
      if (TIME_STEPS[i] >= target) { step = TIME_STEPS[i]; break; }
    }
    var out = [];
    if (step >= 86400) {
      // 天级刻度对齐**本地**零点而不是 UTC 零点：UTC+8 下按 UTC 对齐会让
      // "10/6"那条日期线落在本地早上 8 点，7 天/30 天的 X 轴看起来整体错位。
      // 与公开看板 app.js 的同名函数保持一致（两份实现，改要一起改）。
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

  function clockOf(ts, span) {
    var d = new Date(ts * 1000);
    if (span > 3 * 86400) return (d.getMonth() + 1) + '/' + d.getDate();
    var hh = String(d.getHours()).padStart(2, '0');
    var mm = String(d.getMinutes()).padStart(2, '0');
    return hh + ':' + mm;
  }

  // latencyColor / lossColor：灰阶"墨量"刻度。约定只有一条——**越严重墨越重**
  // （--c1 最醒目，--c5 最接近底色）。健康的链路安静地淡着，出问题的链路
  // 自己黑出来；暗色下整体反转成"越严重越亮"，语义不变。
  // 分段（50/100/200/500ms）与公开看板 app.js 的同名函数保持一致。
  function latencyColor(rttUs) {
    var ms = (rttUs || 0) / 1000;
    if (ms <= 50)  return cssVar('--c5', '#c4c4c4');
    if (ms <= 100) return cssVar('--c4', '#9e9e9e');
    if (ms <= 200) return cssVar('--c3', '#7a7a7a');
    if (ms <= 500) return cssVar('--c2', '#555555');
    return cssVar('--c1', '#2e2e2e');
  }

  // -------------------------------------------------------------------------
  // 图表悬停（与公开看板 app.js 的同名机制保持一致，改要一起改）。
  // 悬停 = 重画一遍并叠加十字线、取样点与提示框；序列取离光标最近的
  // 那个真实测量点，不做插值。
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
        if (cv._hoverTs != null) { cv._hoverTs = null; drawMulti(cv, cv._last.sets, cv._last.axisWin); }
        return;
      }
      var ts = st.tMin + (x - st.padL) / st.plotW * (st.tMax - st.tMin);
      if (ts !== cv._hoverTs) {
        cv._hoverTs = ts;
        drawMulti(cv, cv._last.sets, cv._last.axisWin, ts);
      }
    });
    cv.addEventListener('mouseleave', function () {
      if (cv._hoverTs != null) {
        cv._hoverTs = null;
        drawMulti(cv, cv._last.sets, cv._last.axisWin);
      }
    });
  }

  // drawChartHover 画十字线、每条序列的取样圆点与提示框（与公开看板 app.js
  // 的同名机制保持一致，改要一起改）。序列取 hoverTs **附近**的点，容差
  // 1.5×平均点距、下限为窗口宽度的 0.5%——目标刚建时数据只覆盖窗口尾部，
  // 无容差地取"最近样本"会把十字线钉在几小时外，表现就是"悬停没有详情"。
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

    ctx.save();
    ctx.strokeStyle = cssVar('--line-strong', '#868686');
    ctx.lineWidth = 1;
    ctx.setLineDash([4, 3]);
    ctx.beginPath();
    ctx.moveTo(Math.round(x) + .5, ax.padT);
    ctx.lineTo(Math.round(x) + .5, ax.padT + ax.plotH);
    ctx.stroke();
    ctx.setLineDash([]);

    // 光标附近一条样本都没有：只画十字线，不出提示框（数据空档里弹一个
    // 全是"无数据"的框，比不弹更吵）。
    if (!hits.length) { ctx.restore(); return; }

    // 提示框
    var lines = [];
    var d = new Date(hits[0].pt.ts * 1000);
    var hh = String(d.getHours()).padStart(2, '0');
    var mm = String(d.getMinutes()).padStart(2, '0');
    var ss = String(d.getSeconds()).padStart(2, '0');
    lines.push({ text: (d.getMonth() + 1) + '/' + d.getDate() + ' ' + hh + ':' + mm + ':' + ss, head: true });
    hits.forEach(function (h) {
      lines.push({ text: (h.s.name ? h.s.name + '  ' : '') + fmtAxis(h.pt.value, h.s.unit), color: h.s.color });
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
    ctx.fillStyle = cssVar('--panel', '#ffffff');
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

    // 取样点最后画（压在提示框/十字线上）。横坐标用样本自己的 ts
    // （容差内允许与十字线差一点）：点代表"那一次的真实测量"。
    var st = cv._hoverState;
    hits.forEach(function (h) {
      var px = ax.padL + ax.plotW * ((h.pt.ts - ax.tMin) / (ax.tMax - ax.tMin));
      var y = ax.padT + ax.plotH - ax.plotH * Math.min(1, h.pt.value / (st ? st.top : 1));
      ctx.beginPath();
      ctx.arc(px, y, 3.2, 0, Math.PI * 2);
      ctx.fillStyle = h.s.color;
      ctx.fill();
      ctx.strokeStyle = cssVar('--panel', '#ffffff');
      ctx.stroke();
    });
    ctx.restore();
  }

  function drawMulti(cv, sets, axisWin, hoverTs) {
    sets = sets || [];
    initChartHover(cv, sets, axisWin);
    var live = sets.filter(function (s) { return s.points && s.points.length; });
    if (!live.length) {
      drawEmpty(cv, '该时间范围内没有数据');
      return;
    }
    var p = prepareCanvas(cv);
    var ctx = p.ctx;
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
    // 数据覆盖不满请求窗口时按数据跨度取刻度，会让"最近 7 天"的日期标签
    // 退化成时分（与公开看板同一缺陷，见 app.js 注释）。这里同样把轴钉到
    // 完整请求窗口上。
    if (axisWin && axisWin.tMin != null) {
      tMin = Math.min(tMin, axisWin.tMin);
      // 右边界：数据终点离"现在"够近（≤120s）就钉到数据终点，避免右侧留出
      // 一段空白（数据总是略滞后于浏览器 now，否则曲线最右端永远差一截）；
      // 数据明显落后于窗口（新节点、短保留期、刚启用的目标）才退回到窗口
      // 终点，以保证日期类刻度仍为自然边界（与下方 timeTicks 对齐）。
      var winMax = axisWin.tMax != null ? axisWin.tMax : tMax;
      tMax = (tMax >= winMax - 120) ? tMax : winMax;
    }
    if (tMax <= tMin) tMax = tMin + 1;
    var span = tMax - tMin;

    var unit = live[0].unit || 'raw';
    var top = niceTop(vMax, unit === 'bytes' || unit === 'bps');
    if (unit === 'pct' && top > 10000) top = 10000;

    // 悬停状态快照（与公开看板 app.js 同一套机制）。
    cv._hoverState = {
      tMin: tMin, tMax: tMax, padL: padL, padT: padT, padR: padR,
      plotW: plotW, plotH: plotH, top: top, unit: unit,
      live: live.map(function (s, i) {
        return { points: s.points, name: s.name, unit: s.unit || unit,
                 color: s.color || paletteColor(i) };
      })
    };

    var axis = cssVar('--line-strong', '#868686');
    var muted = cssVar('--muted', '#868686');

    // ---- 横向网格线 + Y 轴标签（4 段 5 条）----
    ctx.font = '11px sans-serif';
    ctx.textAlign = 'right';
    ctx.textBaseline = 'middle';
    for (var i = 0; i <= 4; i++) {
      var y = Math.round(padT + plotH - plotH * i / 4) + .5;
      ctx.strokeStyle = axis;
      ctx.globalAlpha = i === 0 ? 1 : .5;
      ctx.beginPath();
      ctx.moveTo(padL, y);
      ctx.lineTo(padL + plotW, y);
      ctx.stroke();
      ctx.globalAlpha = 1;
      ctx.fillStyle = muted;
      ctx.fillText(fmtAxis(top * i / 4, unit), padL - 8, y);
    }

    // ---- X 轴时间刻度（对齐到整点整分）----
    var ticks = timeTicks(tMin, tMax, Math.max(2, Math.floor(plotW / 110)));
    ctx.textBaseline = 'top';
    ticks.forEach(function (ts, idx) {
      var x = Math.round(padL + plotW * ((ts - tMin) / span)) + .5;
      if (idx > 0 && idx < ticks.length - 1) {
        ctx.strokeStyle = axis;
        ctx.globalAlpha = .28;
        ctx.beginPath();
        ctx.moveTo(x, padT);
        ctx.lineTo(x, padT + plotH);
        ctx.stroke();
        ctx.globalAlpha = 1;
      }
      // 与公开看板同样处理：居中对齐时最右端那个刻度会有一半伸出画布被裁掉。
      var align = 'center';
      if (x - padL < 26) align = 'left';
      else if (padL + plotW - x < 26) align = 'right';
      ctx.textAlign = align;
      ctx.fillStyle = muted;
      ctx.fillText(clockOf(ts, span), x, padT + plotH + 7);
    });

    // ---- 折线 + 面积 + 末点 ----
    // 断档分段绘制（与 app.js 一致）：断联期间没有样本，段与段之间不连线。
    // 灰阶多序列靠虚线线型区分（与公开看板同一张 PALETTE_DASH 表）：
    // setLineDash 只影响描边，画完要复位，否则坐标轴/悬停线也会变虚线。
    live.forEach(function (s, idx) {
      var color = s.color || paletteColor(idx);
      var dash = s.dash || null;
      var segs = segmentsOf(s.points, gapThreshold(s.points), function (pt) {
        return [padL + plotW * ((pt.ts - tMin) / span),
                padT + plotH - plotH * Math.min(1, pt.value / top)];
      });
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
      if (dash) ctx.setLineDash(dash);
      ctx.lineWidth = live.length > 1 ? 1.4 : 1.8;
      ctx.lineJoin = 'round';
      ctx.lineCap = 'round';
      segs.forEach(function (seg) {
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
      if (dash) ctx.setLineDash([]);
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
      var lx = padL + 8, ly = padT + 12;
      ctx.textAlign = 'left';
      ctx.textBaseline = 'middle';
      ctx.font = '11px sans-serif';
      ctx.globalAlpha = .82;
      ctx.fillStyle = cssVar('--panel', '#ffffff');
      ctx.fillRect(lx - 6, ly - 11, 220, sets.length * 15 + 8);
      ctx.globalAlpha = 1;
      var fg = cssVar('--fg', '#121212');
      sets.forEach(function (s, idx) {
        var color = s.color || paletteColor(idx);
        // 图例样本画成**虚线段**而不是实心方块：与公开看板一致，
        // 多序列的区分度在虚线线型上，方块表达不了"这条线是点划线"。
        ctx.strokeStyle = color;
        ctx.lineWidth = 1.6;
        if (s.dash) ctx.setLineDash(s.dash);
        ctx.beginPath();
        ctx.moveTo(lx, ly);
        ctx.lineTo(lx + 12, ly);
        ctx.stroke();
        if (s.dash) ctx.setLineDash([]);
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
  // 探测目标
  // -------------------------------------------------------------------------

  var CARRIER_OPTS = [['telecom', '电信'], ['unicom', '联通'], ['mobile', '移动'], ['custom', '自定义']];
  var METHOD_OPTS = [['tcp', 'TCP 握手'], ['icmp', 'ICMP Echo']];

  // editingTarget 是当前弹窗里正在编辑的目标（null = 弹窗未打开）。
  var editingTarget = null;
  // editingFromNode 记录这次编辑是从**节点详情页**打开的。
  //
  // 保存后要刷新的东西不一样：从节点页打开的要重画该节点的目标表与延迟曲线，
  // 从探测目标页打开的要重画目标表。两边都刷虽然也能跑，但会在无关页面上
  // 发一堆 GET，而且节点页那两个请求会因为 state.node 不匹配而白白往返。
  var editingFromNode = false;

  function fillSelect(sel, opts, value) {
    clear(sel);
    opts.forEach(function (o) {
      var op = el('option', null, o[1]);
      op.value = o[0];
      sel.appendChild(op);
    });
    sel.value = value;
  }

  // ICMP 不走端口，禁用端口输入——填了却不生效比禁用更让人困惑。
  function syncTargetMethod() {
    $('tm-port').disabled = $('tm-method').value === 'icmp';
  }

  // -------------------------------------------------------------------------
  // 节点多选（探测目标的作用域）
  //
  // 后端把作用域做成三态（见 admin.go 的 node_ids 注释）：
  //   字段缺省 → 只改参数、作用域不动；[] → 全局；[1,3,7] → 这几个节点。
  // 因此前端必须能区分"没提交范围"与"提交了空范围"——用 `hidden` 判断
  // 比用"勾了几个"判断更可靠：全局目标与锁定的编辑都不提交 node_ids。
  // -------------------------------------------------------------------------

  // state.nodes 由 loadNodes() 填入。管理员若直接用 #/targets 落进来，
  // 节点视图从未打开过、state.nodes 还是空的，多选列表会渲染成
  // "还没有节点"——看起来就像功能坏了。所以这里按需补一次加载。
  var nodesReady = false;
  function ensureNodes() {
    if (nodesReady) return Promise.resolve();
    return loadNodes().then(function () { nodesReady = true; },
      function () { nodesReady = true; });
  }

  // buildNodeChecks 渲染节点勾选列表。
  //
  // lockedID 是"这条目标自己所在的节点"。后端明确拒绝把目标移出自身节点
  // （会留下一堆孤儿延迟桶，读侧的 JOIN 守卫会把它们全过滤掉，表现为
  // "历史数据凭空消失"），所以这里直接锁住不让取消，而不是等点了保存才报错。
  function buildNodeChecks(host, checked, lockedID) {
    clear(host);
    var list = state.nodes || [];
    if (!list.length) {
      host.appendChild(el('p', 'muted', '还没有节点，无法选择专属范围。'));
      return;
    }
    list.forEach(function (n) {
      var l = el('label', 'check-row');
      var i = el('input');
      i.type = 'checkbox';
      i.value = String(n.id);
      i.checked = n.id === lockedID || checked.indexOf(n.id) >= 0;
      if (n.id === lockedID) i.disabled = true;
      l.appendChild(i);
      // 文本包一层 span：CSS 里要用 `input:disabled ~ span` 把锁定的那项压暗。
      l.appendChild(el('span', null, n.name + ' (#' + n.id + ')'));
      if (n.id === lockedID) l.title = '这条目标就在该节点上，不能取消勾选';
      host.appendChild(l);
    });
  }

  // checkedNodeIDs 读出勾选的节点 id。
  //
  // 被禁用的复选框仍然算勾选：它本来就该在范围内（后端也要求自身节点在内）。
  // 注意必须走 Array.prototype.forEach，NodeList 上没有 map。
  function checkedNodeIDs(host) {
    var out = [];
    Array.prototype.forEach.call(host.querySelectorAll('input[type=checkbox]'), function (i) {
      if (i.checked) out.push(parseInt(i.value, 10));
    });
    return out.filter(function (x) { return x > 0; });
  }

  // targetSiblings 找出"这条目标还部署在哪些节点上"，用于预勾选。
  //
  // 库里没有目标组字段，所以用「同名 + 同运营组」当身份——这不是新发明：
  // 延迟点阵本来就是按 carrier 分组的（见 store_probe.go 的 LatencyBucketsForNode）。
  // 只用于**预勾选**，绝不用于删除：没勾上的节点一点都不会被改动。
  function targetSiblings(t) {
    var out = [t.node_id];
    (lastTargets || []).forEach(function (x) {
      if (x.id === t.id || !x.node_id) return;
      if (x.name === t.name && carrierKey(x) === carrierKey(t)) out.push(x.node_id);
    });
    return out;
  }

  function carrierKey(t) { return (t && t.carrier) || 'custom'; }

  // renderTargetScope 画弹窗里的"范围"区。
  //
  // 三种情形互不相同，所以分别写清楚提示，而不是复用一个含糊的句子：
  //   - 全局目标：范围不可改（后端也拒绝），只提示"要限定节点请新建专属目标"；
  //   - 节点详情页打开的编辑（opts.lockScope）：范围锁死为该节点；
  //   - 探测目标页编辑专属目标：给勾选列表，可多选节点。
  function renderTargetScope(t, opts) {
    var list = $('tm-nodes');
    clear(list);
    if (!t.node_id) {
      list.hidden = true;
      setText($('tm-scope'), '作用域：全局，下发给所有节点。' +
        '（全局目标不能收窄成专属：需要只让部分节点探测时，请新建一条专属目标）');
      return;
    }
    if (opts.lockScope) {
      list.hidden = true;
      setText($('tm-scope'), '作用域：节点 #' + t.node_id + ' 专属。' +
        '要把这套参数一次同步到多个节点，请到「探测目标」页点这条目标的「编辑」。');
      return;
    }
    list.hidden = false;
    setText($('tm-scope'), '作用域：勾选的节点都会使用这一套参数（只增不删，' +
      '取消勾选只是跳过该节点，不会删除它已有的目标）。');
    buildNodeChecks(list, targetSiblings(t), t.node_id);
  }

  // openTargetEditor 打开站内模态框编辑目标。
  //
  // 为什么不用 window.prompt：原生弹窗一次只能问一个问题（改三个字段要弹三次），
  // 无法设置数字/下拉控件，深色主题下刺眼，且在部分浏览器里会被拦截。
  //
  // opts.lockScope=true 来自节点详情页：那里是"某个节点的视角"，
  // 把作用域搬来搬去只会让人困惑，所以只给改参数。
  function openTargetEditor(t, opts) {
    opts = opts || {};
    editingTarget = t;
    editingFromNode = !!opts.lockScope;
    setText($('target-modal-title'), '编辑探测目标 #' + t.id);
    $('tm-name').value = t.name || '';
    fillSelect($('tm-carrier'), CARRIER_OPTS, t.carrier || 'custom');
    fillSelect($('tm-method'), METHOD_OPTS, t.method || 'tcp');
    $('tm-host').value = t.host || '';
    $('tm-port').value = t.port || 443;
    $('tm-timeout').value = t.timeout_ms || 2000;
    $('tm-enabled').checked = !!t.enabled;
    $('tm-show-in-bars').checked = t.show_in_bars !== false;
    renderTargetScope(t, opts);
    if ((state.nodes || []).length === 0) {
      // 节点清单还没拉到：补一次再重画范围区。用 editingTarget === t 判断，
      // 避免管理员已经关掉弹窗/换了目标后又被晚到的响应覆盖。
      ensureNodes().then(function () {
        if (editingTarget === t) renderTargetScope(t, opts);
      });
    }
    setText($('target-modal-error'), '');
    syncTargetMethod();
    $('target-modal').hidden = false;
    $('tm-name').focus();
  }

  // targetNameOf 把运行态里的 target_id 还原成管理员看得懂的目标名。
  //
  // 拿不到映射时退回 "目标 #N" 而不是显示空白：目标被删掉的历史数据仍会出现在
  // 运行态里，此时显示编号至少还能和探测目标页的 ID 对上。
  function targetNameOf(id) {
    var n = state.targetNames[id];
    return n || ('目标 #' + id);
  }

  // loadTargetNames 拉取目标名映射。失败时静默——最坏情况只是显示编号。
  function loadTargetNames() {
    return api('/probe-targets').then(function (list) {
      var m = {};
      (list || []).forEach(function (t) { m[t.id] = t.name || ('目标 #' + t.id); });
      state.targetNames = m;
    }).catch(function () { /* 忽略 */ });
  }

  function closeTargetEditor() {
    editingTarget = null;
    editingFromNode = false;
    $('target-modal').hidden = true;
  }

  // lastTargets 缓存最近一次拉到的目标清单。
  //
  // 用途只有一个：编辑一条专属目标时，把"同一目标在别的节点上的副本"
  // 预勾选出来（见 targetSiblings）。两个入口（探测目标页 / 节点详情页）
  // 都会刷新它，所以无论从哪儿点「编辑」，预勾选的结果都一样。
  var lastTargets = [];

  function loadTargets() {
    api('/probe-targets').then(function (list) {
      lastTargets = list || [];
      var tb = $('targets-table').querySelector('tbody');
      clear(tb);
      lastTargets.forEach(function (t) {
        var tr = el('tr');
        tr.appendChild(td(t.id));
        tr.appendChild(td(t.name || '-'));
        tr.appendChild(td(t.node_id ? ('节点 #' + t.node_id) : '全局'));
        tr.appendChild(td(t.carrier));
        tr.appendChild(td(t.host + (t.method === 'tcp' ? ':' + t.port : '')));
        tr.appendChild(td(t.method));
        tr.appendChild(td(t.timeout_ms + ' ms'));

        var st = el('td');
        st.appendChild(el('span', 'chip ' + (t.enabled ? 'online' : 'offline'), t.enabled ? '启用' : '停用'));
        if (!t.show_in_bars) {
          st.appendChild(document.createTextNode(' '));
          st.appendChild(el('span', 'chip muted', '点阵隐藏'));
        }
        tr.appendChild(st);

        var ops = el('td');
        ops.appendChild(btn(t.enabled ? '停用' : '启用', 'ghost mini', function () {
          guard(api('/probe-targets/' + t.id, { method: 'PATCH', body: { enabled: !t.enabled } }), '已更新')
            .then(loadTargets);
        }));
        ops.appendChild(document.createTextNode(' '));
        ops.appendChild(btn('编辑', 'ghost mini', function () { openTargetEditor(t); }));
        ops.appendChild(document.createTextNode(' '));
        ops.appendChild(btn('删除', 'ghost mini danger', function () {
          askDialog({
            title: '删除目标 #' + t.id,
            desc: '删除后其历史延迟数据将一并清除。',
            okText: '删除'
          }).then(function (r) {
            if (!r.ok) return;
            guard(api('/probe-targets/' + t.id, { method: 'DELETE' }), '已删除').then(loadTargets);
          });
        }));
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
    }).catch(function (e) { toast(e.message, true); });
  }

  function loadNodeTargets(id) {
    // 延迟标签里附一份该节点可见的目标（全局只读 + 专属可管理）。
    // 专属目标只对该节点下发，适合"这台机器到某个内网/专线通不通"这类局部探测。
    api('/probe-targets').then(function (list) {
      lastTargets = list || [];
      var slot = $('node-targets-slot');
      if (!slot || !state.node || state.node.id !== id) return;
      clear(slot);

      var mine = lastTargets.filter(function (t) { return !t.node_id || t.node_id === id; });
      if (!mine.length) {
        slot.appendChild(el('p', 'muted', '还没有目标，可在下方新增一个只属于该节点的探测目标。'));
      } else {
        var wrap = el('div', 'table-wrap');
        var t = el('table');
        var thead = el('thead');
        var htr = el('tr');
        ['ID', '名称', '运营组', '目标', '方式', '状态', '范围', '操作'].forEach(function (x) {
          htr.appendChild(el('th', null, x));
        });
        thead.appendChild(htr);
        t.appendChild(thead);
        var tb = el('tbody');
        mine.forEach(function (x) {
          var tr = el('tr');
          tr.appendChild(td('#' + x.id));
          tr.appendChild(td(x.name || '-'));
          tr.appendChild(td(CARRIER[x.carrier] || x.carrier || '-'));
          tr.appendChild(td(x.host + (x.method === 'tcp' ? ':' + x.port : '')));
          tr.appendChild(td(x.method));
          tr.appendChild(td(x.enabled ? '启用' : '停用'));
          tr.appendChild(td(x.node_id ? '节点专属' : '全局'));
          var ops = el('td');
          if (x.node_id) {
            // 编辑入口放在这里（而不只是"探测目标页"）：管理员是在这个页面上
            // 发现"这条目标填错了"的，让他为了改个端口先跳走再找回来很别扭。
            // lockScope=true → 只给改参数，作用域锁死为本节点。
            ops.appendChild(btn('编辑', 'ghost mini', function () {
              openTargetEditor(x, { lockScope: true });
            }));
            ops.appendChild(document.createTextNode(' '));
            ops.appendChild(btn(x.enabled ? '停用' : '启用', 'ghost mini', function () {
              guard(api('/probe-targets/' + x.id, { method: 'PATCH', body: { enabled: !x.enabled } }), '已更新')
                .then(function () { reloadAfterTargetChange(id); });
            }));
            ops.appendChild(document.createTextNode(' '));
            ops.appendChild(btn('删除', 'ghost mini danger', function () {
              askDialog({
                title: '删除目标 #' + x.id,
                desc: '删除后其历史延迟数据将一并清除。',
                okText: '删除'
              }).then(function (r) {
                if (!r.ok) return;
                guard(api('/probe-targets/' + x.id, { method: 'DELETE' }), '已删除')
                  .then(function () { reloadAfterTargetChange(id); });
              });
            }));
          } else {
            // 全局目标影响所有节点，编辑入口收口在「探测目标」页，避免两处改同一份数据。
            var tip = el('span', 'muted', '在「探测目标」页管理');
            tip.title = '全局目标会下发给所有节点，请到「探测目标」页修改';
            ops.appendChild(tip);
          }
          tr.appendChild(ops);
          tb.appendChild(tr);
        });
        t.appendChild(tb);
        wrap.appendChild(t);
        slot.appendChild(wrap);
      }

      // 新增节点专属目标：与全局表单同构，但固定 node_id。
      var form = el('div', 'panel section-gap');
      form.hidden = true;
      form.id = 'node-target-form';
      function fInput(labelText, id, ph, type) {
        var l = el('label', null, labelText);
        var i = el('input');
        i.id = id;
        i.type = type || 'text';
        if (ph) i.placeholder = ph;
        l.appendChild(i);
        return l;
      }
      form.appendChild(fInput('名称', 'nt-name', '例如 专线-上海'));
      var lCarrier = el('label', null, '运营组');
      var carSel = el('select');
      carSel.id = 'nt-carrier';
      CARRIER_OPTS.forEach(function (o) {
        var op = el('option', null, o[1]);
        op.value = o[0];
        carSel.appendChild(op);
      });
      lCarrier.appendChild(carSel);
      form.appendChild(lCarrier);
      var lMethod = el('label', null, '方式');
      var mSel = el('select');
      mSel.id = 'nt-method';
      [['tcp', 'TCP'], ['icmp', 'ICMP']].forEach(function (o) {
        var op = el('option', null, o[1]);
        op.value = o[0];
        mSel.appendChild(op);
      });
      lMethod.appendChild(mSel);
      form.appendChild(lMethod);
      form.appendChild(fInput('主机 / IP', 'nt-host', '例如 10.0.0.1 或 example.com'));
      form.appendChild(fInput('端口（ICMP 留空）', 'nt-port', '443', 'number'));
      form.appendChild(fInput('超时 ms', 'nt-timeout', '2000', 'number'));
      var row = el('div', 'row');
      row.appendChild(btn('创建专属目标', 'primary', function () {
        var body = {
          node_id: id,
          name: $('nt-name').value.trim(),
          carrier: $('nt-carrier').value,
          method: $('nt-method').value,
          host: $('nt-host').value.trim(),
          port: parseInt($('nt-port').value, 10) || 0,
          timeout_ms: parseInt($('nt-timeout').value, 10) || 2000
        };
        if (!body.host) { toast('请填写主机', true); return; }
        guard(api('/probe-targets', { method: 'POST', body: body }), '专属目标已创建').then(function () {
          reloadAfterTargetChange(id);
        });
      }));
      row.appendChild(btn('取消', 'ghost', function () { form.hidden = true; }));
      form.appendChild(row);
      slot.appendChild(form);

      slot.appendChild(form);

      var addRow = el('div', 'row');
      addRow.appendChild(btn('新增节点专属目标', 'primary', function () {
        form.hidden = !form.hidden;
        if (!form.hidden) $('nt-name').focus();
      }));
      slot.appendChild(addRow);
    }).catch(function () { /* 忽略 */ });
  }

  // reloadAfterTargetChange：目标变了 → 清单重拉、延迟曲线与明细重画、运行态里的瞬时值也变了。
  function reloadAfterTargetChange(id) {
    loadTargetNames();
    loadNodeTargets(id);
    loadRuntime(id, $('runtime-cards'));
    if (state.nodeHistoryTab === 'latency' && state.node && state.node.id === id) {
      loadHistory(id, $('hist-range') ? $('hist-range').value : '24h');
    }
  }

  $('btn-refresh-targets').addEventListener('click', loadTargets);
  $('btn-new-target').addEventListener('click', function () {
    var f = $('new-target-form');
    f.hidden = !f.hidden;
    if (f.hidden) return;
    // 节点清单没拉到过就先补一次再画范围选择，否则会出现"一个节点都没有"的假故障。
    ensureNodes().then(syncCreateScope);
    $('t-name').focus();
  });
  $('btn-cancel-target').addEventListener('click', function () { $('new-target-form').hidden = true; });

  // syncCreateScope 切换"全局 / 指定节点"两种范围模式的界面。
  //
  // 新建时 `node_ids: []` 表示全局（见 admin.go）：这跟编辑时"空范围=报错"
  // 是刻意的差别——新建一条全局目标是正常需求，把已有目标改成"没有节点"不是。
  function syncCreateScope() {
    var byNodes = $('t-scope-mode').value === 'nodes';
    var list = $('t-nodes');
    list.hidden = !byNodes;
    setText($('t-scope-hint'), byNodes
      ? '勾选的每个节点各建一条同参数目标（至少勾一个）。'
      : '全局目标下发给所有节点；需要只让部分机器探测时选「指定节点」。');
    if (byNodes) {
      // 重画而不是只显示：管理员可能刚从节点页回来，node 清单已经变了。
      // 用 checkedNodeIDs 保留已勾选项，切来切去不会把选择丢掉。
      buildNodeChecks(list, checkedNodeIDs(list), 0);
    }
  }
  $('t-scope-mode').addEventListener('change', syncCreateScope);

  $('btn-create-target').addEventListener('click', function () {
    var body = {
      name: $('t-name').value.trim(),
      carrier: $('t-carrier').value,
      method: $('t-method').value,
      host: $('t-host').value.trim(),
      port: parseInt($('t-port').value, 10) || 0,
      timeout_ms: parseInt($('t-timeout').value, 10) || 2000
    };
    if (!body.host) { toast('请填写主机', true); return; }
    if ($('t-scope-mode').value === 'nodes') {
      var ids = checkedNodeIDs($('t-nodes'));
      if (!ids.length) { toast('请至少勾选一个节点', true); return; }
      body.node_ids = ids;
    } else {
      body.node_ids = [];
    }
    guard(api('/probe-targets', { method: 'POST', body: body }), '目标已创建').then(function (r) {
      if (!r) return; // 失败时保留表单，别把管理员填了一半的内容清掉
      $('new-target-form').hidden = true;
      $('t-name').value = ''; $('t-host').value = '';
      loadTargets();
    });
  });

  // --- 编辑弹窗 -----------------------------------------------------------
  $('tm-method').addEventListener('change', syncTargetMethod);
  $('btn-close-target').addEventListener('click', closeTargetEditor);
  // 点击遮罩（而不是弹窗卡片）关闭；只在 ev.target 就是遮罩本身时生效，
  // 否则点卡片里的任何空白都会把弹窗关掉。
  $('target-modal').addEventListener('click', function (ev) {
    if (ev.target === $('target-modal')) closeTargetEditor();
  });
  document.addEventListener('keydown', function (ev) {
    if (ev.key === 'Escape' && editingTarget) closeTargetEditor();
  });

  $('target-form').addEventListener('submit', function (ev) {
    ev.preventDefault();
    if (!editingTarget) return;
    var method = $('tm-method').value;
    var body = {
      name: $('tm-name').value.trim(),
      carrier: $('tm-carrier').value,
      method: method,
      host: $('tm-host').value.trim(),
      port: method === 'icmp' ? 0 : (parseInt($('tm-port').value, 10) || 0),
      timeout_ms: parseInt($('tm-timeout').value, 10) || 2000,
      enabled: $('tm-enabled').checked,
      show_in_bars: $('tm-show-in-bars').checked
    };
    // 前端先做一轮同样的校验，避免为了看一条错误信息跑一趟服务端。
    // 真正的把关仍在后端（validateTargetFields），这里只是省一次往返。
    if (!body.host) { setText($('target-modal-error'), '请填写主机'); return; }
    if (body.method === 'tcp' && (body.port < 1 || body.port > 65535)) {
      setText($('target-modal-error'), 'TCP 目标必须填 1~65535 的端口');
      return;
    }
    var t = body.timeout_ms;
    if (t < 100 || t > 30000) { setText($('target-modal-error'), '超时应在 100~30000 毫秒'); return; }
    // 范围：只在勾选列表**可见**时才提交 node_ids。
    // 列表隐藏的两种情形（全局目标、节点详情页的编辑）都必须走"不提交"这条路——
    // 一旦提交了空数组，后端会把"范围不能为空"直接拒掉，
    // 表现为"在节点详情页改个端口都存不进去"。
    if (!$('tm-nodes').hidden) {
      var ids = checkedNodeIDs($('tm-nodes'));
      if (!ids.length) {
        setText($('target-modal-error'), '请至少勾选一个节点');
        return;
      }
      body.node_ids = ids;
    }
    setText($('target-modal-error'), '');
    var fromNode = editingFromNode;
    guard(api('/probe-targets/' + editingTarget.id, { method: 'PATCH', body: body }), '目标已更新')
      .then(function (r) {
        if (!r) return; // 失败时保留弹窗，让管理员接着改
        closeTargetEditor();
        // 同步到多个节点时如实报出"新建了几条 / 更新了几条"：
        // 管理员勾了 5 个节点，需要知道那 5 个是不是都生效了。
        var extra = (r.created || 0) + (r.updated || 0);
        if (extra > 0) toast('已同步到 ' + extra + ' 个节点（新建 ' + (r.created || 0) + ' 条）');
        if (fromNode && state.node && state.node.id) reloadAfterTargetChange(state.node.id);
        else loadTargets();
      });
  });

  // -------------------------------------------------------------------------
  // 告警
  // -------------------------------------------------------------------------

  function loadChannels() {
    api('/alert-channels').then(function (list) {
      var tb = $('channels-table').querySelector('tbody');
      clear(tb);
      (list || []).forEach(function (c) {
        var tr = el('tr');
        tr.appendChild(td(c.id));
        tr.appendChild(td(c.type === 'telegram' ? 'Telegram' : 'SMTP 邮件'));
        tr.appendChild(td(c.name));
        tr.appendChild(td(c.enabled ? '启用' : '停用'));
        var ops = el('td');
        ops.appendChild(btn('测试', 'ghost mini', function () {
          guard(api('/alert-channels/' + c.id + '/test', { method: 'POST' }), '测试消息已发送');
        }));
        ops.appendChild(document.createTextNode(' '));
        ops.appendChild(btn(c.enabled ? '停用' : '启用', 'ghost mini', function () {
          guard(api('/alert-channels/' + c.id, { method: 'PATCH', body: { enabled: !c.enabled } }), '已更新')
            .then(loadChannels);
        }));
        ops.appendChild(document.createTextNode(' '));
        ops.appendChild(btn('删除', 'ghost mini danger', function () {
          askDialog({
            title: '删除渠道「' + c.name + '」',
            desc: '删除后该渠道不再接收任何告警通知。',
            okText: '删除'
          }).then(function (r) {
            if (!r.ok) return;
            guard(api('/alert-channels/' + c.id, { method: 'DELETE' }), '已删除').then(loadChannels);
          });
        }));
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
    }).catch(function (e) { toast(e.message, true); });
  }

  $('btn-refresh-alerts').addEventListener('click', function () {
    loadChannels(); loadRules(); loadEvents();
  });
  $('btn-new-channel').addEventListener('click', function () {
    var f = $('new-channel-form');
    f.hidden = !f.hidden;
  });
  $('btn-cancel-channel').addEventListener('click', function () { $('new-channel-form').hidden = true; });

  $('c-type').addEventListener('change', function () {
    var tg = $('c-type').value === 'telegram';
    $('c-tg').hidden = !tg;
    $('c-smtp').hidden = tg;
  });

  $('btn-create-channel').addEventListener('click', function () {
    var type = $('c-type').value;
    var config;
    if (type === 'telegram') {
      config = {
        bot_token: $('c-bot-token').value.trim(),
        chat_id: $('c-chat-id').value.trim(),
        parse_mode: $('c-parse-mode').value
      };
      if (!config.bot_token || !config.chat_id) { toast('Bot Token 与 Chat ID 必填', true); return; }
    } else {
      config = {
        host: $('c-host').value.trim(),
        port: parseInt($('c-port').value, 10) || 587,
        username: $('c-user').value.trim(),
        password: $('c-pass').value,
        from: $('c-from').value.trim(),
        to: $('c-to').value.split(',').map(function (s) { return s.trim(); }).filter(Boolean)
      };
      if (!config.host || !config.from || !config.to.length) { toast('SMTP 主机 / 发件人 / 收件人必填', true); return; }
    }
    guard(api('/alert-channels', { method: 'POST', body: { type: type, name: $('c-name').value.trim(), config: config, enabled: true } }), '渠道已创建')
      .then(function () {
        $('new-channel-form').hidden = true;
        loadChannels();
      });
  });

  // 规则与事件的"类型"中文名由**服务端**下发（Go 侧的 alert.KindLabel 是唯一来源），
  // 这里不再自己维护一份 kind→中文 的映射：
  //   - 规则      → /alert-rules 的每条带 label
  //   - 告警事件  → /alert-events 的每条带 kind_label
  // 曾经这里有一份 RULE_LABEL，而事件表用的是 `RULE_LABEL[kind] || kind`：
  // 事件落库的是**状态键**（expire:30、traffic:80、probe_fail:12），
  // 映射里只有不带后缀的基础名，于是永远走 fallback，表里显示的是 `expire:30`。

  function loadRules() {
    api('/alert-rules').then(function (rules) {
      state.rules = rules || {};
      var tb = $('rules-table').querySelector('tbody');
      clear(tb);
      Object.keys(state.rules).forEach(function (kind) {
        var r = state.rules[kind];
        var p = r.params || {};
        var tr = el('tr');
        // r.label 缺失时退回 kind（后端老版本/接口异常时仍然能用，不至于空一格）。
        tr.appendChild(td(r.label || kind));

        var e = el('td');
        var chk = el('input');
        chk.type = 'checkbox';
        chk.checked = !!r.enabled;
        chk.setAttribute('data-kind', kind);
        chk.setAttribute('data-field', 'enabled');
        e.appendChild(chk);
        tr.appendChild(e);

        [['threshold', p.threshold ? (p.threshold / 100) : ''],
         ['duration_s', p.duration_s || ''],
         ['confirm_n', p.confirm_n || ''],
         ['levels', (p.levels || []).join(',')],
         ['cooldown_s', r.cooldown_s || '']].forEach(function (f) {
          var cell = el('td');
          var i = el('input');
          i.type = 'text';
          i.value = f[1];
          i.setAttribute('data-kind', kind);
          i.setAttribute('data-field', f[0]);
          i.size = 6;
          cell.appendChild(i);
          tr.appendChild(cell);
        });
        tb.appendChild(tr);
      });
    }).catch(function (e) { toast(e.message, true); });
  }

  $('btn-save-rules').addEventListener('click', function () {
    var out = {};
    var nodes = $('rules-table').querySelectorAll('[data-kind]');
    Array.prototype.forEach.call(nodes, function (n) {
      var kind = n.getAttribute('data-kind');
      var field = n.getAttribute('data-field');
      if (!out[kind]) out[kind] = { kind: kind, enabled: false, params: {}, cooldown_s: 0 };
      var r = out[kind];
      if (field === 'enabled') { r.enabled = n.checked; return; }
      if (field === 'cooldown_s') { r.cooldown_s = parseInt(n.value, 10) || 0; return; }
      var v = String(n.value).trim();
      if (field === 'threshold') { if (v) r.params.threshold = Math.round(parseFloat(v) * 100); return; }
      if (field === 'duration_s') { if (v) r.params.duration_s = parseInt(v, 10) || 0; return; }
      if (field === 'confirm_n') { if (v) r.params.confirm_n = parseInt(v, 10) || 0; return; }
      if (field === 'levels') {
        var arr = v.split(',').map(function (s) { return parseInt(s.trim(), 10); }).filter(function (x) { return !isNaN(x); });
        if (arr.length) r.params.levels = arr;
        return;
      }
    });
    guard(api('/alert-rules', { method: 'PUT', body: out }), '规则已保存').then(loadRules);
  });

  // -------------------------------------------------------------------------
  // 分页（告警事件 / 审计日志共用）
  //
  // 这两张表都是"只增不减、越翻越旧"的列表，条数没有上限，
  // 所以以前那种"一次拉 200 条"的做法等于把更早的记录藏起来了。
  // -------------------------------------------------------------------------

  // pages 保存两个列表各自的分页位置。
  //
  // 用 limit/offset 而不是游标（before_id）：这两张表只有后台管理员一个人在翻，
  // 不存在"翻页期间别人插入导致漏行"的并发压力，换来的是能显示"第 3 / 12 页"
  // 和跳到指定页——游标分页做不到这两件事。
  // offset/limit 一律以服务端回传的生效值为准（见下面的 adoptPage）：
  // 保留期清理会让总数变小，前端自己记的 offset 可能已经越界。
  var pages = {
    events: { limit: 20, offset: 0, total: 0 },
    // 审计日志默认 20 条/页（与事件一致）：审计是低频回看场景，50 条一页
    // 首屏要滚很久才到底；20 条翻页更快，要批量看时下拉里还有 50/100/200。
    audit: { limit: 20, offset: 0, total: 0 }
  };
  var PAGE_SIZES = [20, 50, 100, 200];

  // adoptPage 把服务端回传的生效窗口写回本地状态。
  //
  // 不这么做的话，offset 越界时会出现"页码写着第 9 页、表里是第 3 页的内容"，
  // 而且"下一页"的禁用判断也会算错。
  function adoptPage(st, res) {
    if (!res || typeof res !== 'object') return [];
    if (typeof res.limit === 'number') st.limit = res.limit;
    if (typeof res.offset === 'number') st.offset = res.offset;
    if (typeof res.total === 'number') st.total = res.total;
    return res.items || [];
  }

  // renderPager 画一条分页条。
  //
  // 总页数用 Math.max(1, ...)：total=0 时若算成 0 页，读数会变成"第 1 / 0 页"，
  // 而"下一页"该不该禁用也会变成一道边界题。
  function renderPager(host, key, onGo) {
    if (!host) return;
    var st = pages[key];
    clear(host);
    var pageCount = Math.max(1, Math.ceil(st.total / st.limit));
    var cur = Math.floor(st.offset / st.limit) + 1;
    if (cur > pageCount) cur = pageCount;

    var sizeLab = el('label', null, '每页');
    var sizeSel = el('select');
    PAGE_SIZES.forEach(function (s) {
      var o = el('option', null, s + ' 条');
      o.value = String(s);
      sizeSel.appendChild(o);
    });
    sizeSel.value = String(st.limit);
    sizeSel.addEventListener('change', function () {
      st.limit = parseInt(sizeSel.value, 10) || st.limit;
      // 换了每页条数之后原来的 offset 已经没有意义，回到第一页。
      // 不重置的话会出现"每页 20 条时在第 5 页，切到每页 200 条后落到第 1 页
      // 之外的空窗口"。
      st.offset = 0;
      onGo();
    });
    sizeLab.appendChild(sizeSel);
    host.appendChild(sizeLab);

    host.appendChild(el('span', 'muted', '共 ' + st.total + ' 条'));
    host.appendChild(el('span', 'spacer'));

    var prev = btn('上一页', 'ghost mini', function () {
      // 用 Math.max 兜底：负 offset 不是在服务端报错，而是被静默收敛成 0，
      // 前端若不自己夹住，"上一页"在第一页点下去看起来就像没反应。
      st.offset = Math.max(0, st.offset - st.limit);
      onGo();
    });
    prev.disabled = st.offset <= 0;
    host.appendChild(prev);

    host.appendChild(el('span', 'page-pos', '第 ' + cur + ' / ' + pageCount + ' 页'));

    var next = btn('下一页', 'ghost mini', function () {
      st.offset = st.offset + st.limit;
      onGo();
    });
    next.disabled = cur >= pageCount;
    host.appendChild(next);
  }

  function loadEvents() {
    var st = pages.events;
    api('/alert-events?limit=' + st.limit + '&offset=' + st.offset).then(function (res) {
      var list = adoptPage(st, res);
      var tb = $('events-table').querySelector('tbody');
      clear(tb);
      list.forEach(function (e) {
        var tr = el('tr');
        tr.appendChild(td(fmtTime(e.created_at)));
        tr.appendChild(td(e.node_id ? ('#' + e.node_id) : '-'));
        // kind_label 由服务端出（见 alert.KindLabel）：`expire:30` → "到期提醒（剩余 ≤ 30 天）"。
        // 没拿到就退回原始键——显示一个看不懂的键，也好过显示一格空白。
        tr.appendChild(td(e.kind_label || e.kind));
        tr.appendChild(td(e.level));
        tr.appendChild(td(e.message, 'wrap'));
        tr.appendChild(td(e.delivered ? '是' : '否'));
        tb.appendChild(tr);
      });
      if (!list.length) {
        var tr0 = el('tr');
        var td0 = td(st.total ? '这一页没有记录' : '还没有告警事件', 'muted');
        td0.colSpan = 6;
        tr0.appendChild(td0);
        tb.appendChild(tr0);
      }
      renderPager($('events-pager'), 'events', loadEvents);
    }).catch(function () { /* 忽略 */ });
  }

  // -------------------------------------------------------------------------
  // 系统设置
  // -------------------------------------------------------------------------

  var SETTING_LABELS = {
    'site.title': ['站点标题', 'text'],
    'site.layout': ['站点形态', 'select:auto=跟随配置文件,single=同站（一个域名）,split=分离（两个域名）',
      '决定公开看板与管理后台的域名关系。' +
      '「跟随配置文件」= master.yaml 里配了 public_hosts / admin_hosts 就分离，没配就同站（升级前的行为）。' +
      '「同站」= 一个域名下同时提供公开看板与 /admin/，两边不再区分域名。' +
      '「分离」= 公开域名只服务公开路径、管理域名只服务管理路径，用错域名返回 421，' +
      '适合"一个域名给访客、一个域名给自己"。选「分离」时必须同时填下面两项（或写在 master.yaml 里），' +
      '否则保存会被拒绝——那会把你自己锁在后台外面。保存后立即生效，不需要重启。'],
    'site.public_hosts': ['公开域名清单（一行一个，留空则用 master.yaml 的 public_hosts）', 'lines',
      '公开看板允许使用的主机名[:端口]。填了就顶替 master.yaml 里的 public_hosts；' +
      '留空 = 没在后台配过，回落到配置文件。' +
      'host 不匹配的请求返回 421（而不是 404）——那是部署配置错误的特征，' +
      '比一个 404 好排查得多。同一台机器不同端口算不同的源，需显式写出 :端口。'],
    'site.admin_hosts': ['管理域名清单（一行一个，留空则用 master.yaml 的 admin_hosts）', 'lines',
      '管理后台允许使用的主机名[:端口]。作用同上一项，但它是**安全开关**：' +
      '配好之后，拿公开域名访问 /admin/ 或 /api/admin/v1/* 会被 421 拦掉。' +
      '选「分离」形态时该项必须非空。'],
    'site.admin_url': ['后台入口地址（公开看板的「管理后台」按钮指向这里）', 'text',
      '留空则自动推导：master.yaml 的 admin_origin → 分离且只有一个管理域名时按访问协议拼出 → ' +
      '都拿不到就保持同站相对路径 /admin/。' +
      '只有当公开看板与后台**不在同一个源**（不同端口，或不同域名）而自动推导又推不出来时才需要填，' +
      '例如 http://203.0.113.7:8081。填错的表现是"点看板上的登录按钮打开的是一个打不开的地址"。' +
      '它只是个跳转目标，不参与任何鉴权，写错不会把谁挡在外面。'],
    // 第三个元素是可选的长说明，单独用作悬浮提示；
    // 没写时沿用第二个用途——把可见文案本身当提示。
    'agent.ip_whitelist_enabled': ['Agent 接口 IP 白名单', 'select:1=启用,0=关闭',
      '启用后，只有白名单内的来源 IP 才能访问 /api/agent/v1/*（上报采集数据、拉取配置、注册与注销）。' +
      '适合在防火墙之外再收紧一层：Master 端口暴露在公网时，即使有人拿到注册码也必须从白名单 IP 发起。' +
      'Agent 经反向代理转发时，请确保 trusted_proxies 配置正确，否则 Master 看到的是代理 IP 而不是 Agent 真实 IP。'],
    'agent.ip_whitelist': ['白名单列表', 'lines',
      '每行一条，支持单 IP 与 CIDR 网段，例如 203.0.113.7 或 198.51.100.0/24。' +
      '仅在上一项「Agent 接口 IP 白名单」启用时生效；列表为空则所有来源都会被拒绝。'],
    'public.enabled': ['公开看板', 'select:1=开放,0=关闭',
      '是否对外公开节点看板。关闭后未登录访客访问公开页面与公开 API 均返回 404，' +
      '但登录管理员仍可正常打开看板自查效果；一键安装脚本与 Agent 发布文件仍然可用，不影响新节点接入。'],
    'public.cards': ['公开看板卡片（勾选 = 对公展示，逐张开关）', 'cards',
      '逐张控制公开看板上显示哪些卡片。取消勾选只是**不渲染那张卡片**，数据仍会下发；' +
      '如果连数据也不想下发，请改用公开字段白名单 public.fields。' +
      '全部取消勾选 = 一张都不展示；勾上「全部展示」= 不做限制（默认）。'],
    'public.lat_window': ['节点卡点阵时间窗', 'select:30m=近 30 分钟,1h=近 1 小时,6h=近 6 小时,24h=近 24 小时',
      '节点卡片「延迟 / 丢包」点阵统计的时间范围。30 分钟 / 1 小时读原始探测层' +
      '（10 分钟层在这么短的窗口里只有 3/6 个桶，画不出点阵），6 小时 / 24 小时读 10 分钟聚合层。' +
      '保存后调度器在 1 分钟内按新窗口重算点阵。'],
    'public.show_ip_badge': ['公开卡片显示 IP 标识（IPv4/IPv6）', 'select:1=显示,0=隐藏',
      '控制公开卡片是否渲染 IPv4/IPv6 协议栈标识。关闭后该标识不再展示，相关的协议栈信息也不再下发。'],
    'admin.session_ttl_hours': ['登录有效期小时（1-720，到期必须重新登录）', 'number',
      '从**登录那一刻**算起，超过这个时长就必须重新登录（默认 12 小时）。' +
      '它与空闲超时是两条独立的线：页面停在那里不动由空闲超时兜底（固定 2 小时），' +
      '一直在用则由本项兜底——所以这条才是"登录多久之后要重新输密码"的那个数。' +
      '判定用的是**当前设置值**，因此改小立即生效：已经超过新时长的会话在下一个请求' +
      '就会被踢回登录页（改大同样对已登录的会话生效）。'],
    'timezone': ['全局时区（IANA 名称，如 Asia/Shanghai）', 'text'],
    'retention.raw_hours': ['高精度层保留小时（1-72）', 'number'],
    'retention.mid_days': ['中精度层保留天（1-30）', 'number'],
    'retention.days': ['低精度层/总保留天（1-365）', 'number'],
    'raw_resolution_s': ['高精度层落盘粒度秒（3-60）', 'number'],
    'report_interval_s': ['Agent 上报间隔秒（2-60）', 'number'],
    'probe_interval_s': ['探测周期秒（10-600）', 'number'],
    'offline_timeout_s': ['离线超时阈值秒（10-3600）', 'number'],
    'disk_min_free_mb': ['磁盘剩余告警阈值 MB（50-100000）', 'number'],
    'install.mode': ['安装入口模式', 'select:on_demand=按需生成,always=始终可用,off=关闭安装入口',
      '控制 /install.sh 的响应策略：按需生成表示需要携带有效注册码才下发脚本。'],
    'install.master_url': ['Master 公网地址（https，拼入安装命令）', 'text'],
    'install.dist_base': ['二进制下载源（留空用 Master 的 /dist）', 'text'],
    'install.agent_version': ['安装的 Agent 版本（留空取最新）', 'text'],
    'install.code_ttl_min': ['注册码有效期分钟（5-1440）', 'number'],
    'install.max_enroll_codes': ['每节点注册码上限（1-20）', 'number',
      '同一个节点同时最多存在多少个"未使用且未过期"的注册码。' +
      '注册是一次性的，排查注册失败时常常要连签几个码；默认 3，' +
      '被"已达上限"卡住时可以调大。下方令牌面板能查看每个码的状态。'],
    'install.spki_sha256': ['Master SPKI 指纹（自签证书场景，逗号分隔）', 'text'],
    'install.pubkey': ['minisign 公钥（校验 SHA256SUMS，两行文本；留空则不校验）', 'raw'],
    'cdn.enabled': ['CDN 来源识别', 'select:0=关闭,1=开启',
      '节点经 CDN（如 Cloudflare）回源时，Master 看到的连接来源是 CDN 的边缘地址。' +
      '开启后，下方网段清单会被当作"已知的 CDN 边缘跳"：解析 X-Forwarded-For 时' +
      '把这些跳跳过去，记录到的才是节点的真实公网地址。Cloudflare 的官方网段已内置，' +
      '这里只补其它 CDN 或自建加速层。'],
    'cdn.ranges': ['CDN 边缘网段清单（每行一个 CIDR 或 IP）', 'raw',
      '从 CDN 官方页面复制即可，一行一条，支持 IPv4/IPv6；逗号分隔也可以。' +
      '例如：173.245.48.0/20。只添加你能确认属于该 CDN 的网段——' +
      '来源不落在清单里时一律按连接对端处理，伪造的代理头不会生效。'],
    'node.custom_countries': ['自定义国家/地区（每行：代码|名称）', 'raw',
      '内置地区表没有收录的地区在这里补。每行一条，格式：两位代码|名称，' +
      '例如 GB|英国 或 HK|中国香港（代码为 ISO 3166-1 alpha-2）。' +
      '只写代码不写名称也可以；保存后在节点编辑的国家/地区下拉里出现。' +
      '自定义地区没有内置国旗图形，卡片上以两位代码显示。'],
    'heartbeat_url': ['外部心跳地址（https，Master 每 5 分钟 GET 一次）', 'text'],
    'backup.enabled': ['每日自动备份', 'select:1=开启,0=关闭',
      '开启后 Master 每天在指定时刻自动生成一次数据库备份（VACUUM INTO 在线快照，不需要停服）。' +
      '关闭后不再自动备份，只能在下方「数据库备份」区手动点「立即备份」。' +
      '备份是"存在感很低但缺了就致命"的功能，除非你有自己的备份方案，否则建议保持开启。'],
    'backup.hour': ['自动备份时刻（0-23 时，Master 本地时区）', 'number',
      '一天中的哪个整点执行自动备份。按 Master 的本地时区解释——' +
      '你脑子里想的"凌晨 4 点"就是本机时钟上的 4 点（库里记录的时间戳仍是 UTC）。' +
      '建议选业务低谷时段，备份期间会有一次全库读取。'],
    'backup.keep': ['备份保留份数（1-90）', 'number',
      '最多保留多少份备份，超出的自动删除最旧的一份。' +
      '手动「立即备份」同样受这个上限约束，否则连点几次就能把备份盘塞满。' +
      '下限为 1：不允许配成"一份都不保留"。'],
    'inject.head': ['自定义 <head> 注入（支持 <meta>/<link>/<style>，不含脚本）', 'text'],
    'inject.body_end': ['页尾 </body> 前注入（0/1 开关开启后才会插入，可含 <script>）', 'text'],
    'inject.script_enabled': ['页尾脚本注入', 'select:0=关闭,1=开启',
      '开启后 /inject.body_end 里的 <script> 才会真正插入页面并执行，同时 CSP 会为该脚本下发 nonce。只在你完全信任脚本来源时开启。'],
    'theme.active': null,
    'public.fields': null
  };

  // 设置项分组：把原来一字排开的 20+ 个输入按语义拆成几组，
  // 避免一眼看去全是字段找不到重点。
  var SETTING_GROUPS = [
    ['站点与全局', ['site.title', 'timezone']],
    ['站点形态', ['site.layout', 'site.public_hosts', 'site.admin_hosts', 'site.admin_url']],
    ['登录与会话', ['admin.session_ttl_hours']],
    ['公开看板', ['public.enabled', 'public.cards', 'public.lat_window', 'public.show_ip_badge']],
    ['数据保留', ['retention.raw_hours', 'retention.mid_days', 'retention.days', 'raw_resolution_s']],
    ['Agent 与上报', ['report_interval_s', 'probe_interval_s', 'offline_timeout_s', 'disk_min_free_mb',
      'agent.ip_whitelist_enabled', 'agent.ip_whitelist']],
    ['安装与下载', ['install.mode', 'install.master_url', 'install.dist_base', 'install.agent_version', 'install.code_ttl_min', 'install.max_enroll_codes', 'install.spki_sha256', 'install.pubkey']],
    ['CDN 与来源 IP', ['cdn.enabled', 'cdn.ranges']],
    ['备份', ['backup.enabled', 'backup.hour', 'backup.keep']],
    ['脚本注入', ['inject.script_enabled', 'inject.head', 'inject.body_end']],
    ['外部与高级', ['heartbeat_url', 'node.custom_countries']]
  ];

  // buildCardPicker 渲染 public.cards 的勾选组。
  //
  // 卡片目录来自 GET /settings/meta（后端 model.PublicCardList()），前端不写死：
  // 写死的话新增一张卡片后后台勾选框里没有它，管理员就再也关不掉。
  //
  // 取值三态（与后端 model.ParsePublicCards 一致）：
  //   "all"      → 不限制，全部展示
  //   ""         → 一张都不展示
  //   "a,b,c"    → 只展示列出的这些
  // 用一个隐藏 input 承载结果，data-key 与普通设置项一致，这样上面那个
  // 通用的"收集 [data-key] 的 value 提交"逻辑一行都不用改。
  function buildCardPicker(key, title, tip) {
    var wrap = el('div', 'setting-wide');
    wrap.title = tip;
    wrap.appendChild(el('span', 'lbl', title));

    var raw = state.settings[key];
    var rawText = raw == null ? (state.cardsAll || 'all') : String(raw).trim();
    var unlimited = rawText.toLowerCase() === (state.cardsAll || 'all').toLowerCase();

    var hidden = el('input');
    hidden.type = 'hidden';
    hidden.setAttribute('data-key', key);
    hidden.value = rawText;
    wrap.appendChild(hidden);

    var picker = el('div', 'card-picker');

    var master = el('label', 'master');
    var masterBox = el('input');
    masterBox.type = 'checkbox';
    masterBox.checked = unlimited;
    master.appendChild(masterBox);
    master.appendChild(el('span', null, '全部展示（不做限制）'));
    picker.appendChild(master);

    var boxes = [];
    var lastGroup = null;
    (state.cardCatalog || []).forEach(function (c) {
      if (c.group !== lastGroup) {
        lastGroup = c.group;
        picker.appendChild(el('div', 'grp',
          c.group === 'detail' ? '节点详情信息小卡片'
            : c.group === 'node' ? '节点卡片上的区块'
            : '总览页汇总卡片'));
      }
      var lb = el('label');
      lb.title = c.name + '：' + c.desc;
      var box = el('input');
      box.type = 'checkbox';
      box.checked = unlimited || rawText.split(',').indexOf(c.key) >= 0;
      lb.appendChild(box);
      lb.appendChild(el('span', null, c.name));
      picker.appendChild(lb);
      boxes.push({ key: c.key, box: box, lb: lb });
    });
    wrap.appendChild(picker);

    // sync 把当前勾选状态写回隐藏 input，并统一刷新禁用态/选中态样式。
    function sync() {
      if (masterBox.checked) {
        hidden.value = state.cardsAll || 'all';
        boxes.forEach(function (b) {
          b.box.checked = true;
          b.box.disabled = true;
          b.lb.className = 'on';
        });
        return;
      }
      var on = [];
      boxes.forEach(function (b) {
        b.box.disabled = false;
        if (b.box.checked) { on.push(b.key); b.lb.className = 'on'; }
        else b.lb.className = 'off';
      });
      hidden.value = on.join(',');
    }

    masterBox.addEventListener('change', sync);
    boxes.forEach(function (b) { b.box.addEventListener('change', sync); });
    sync();
    return wrap;
  }

  function buildSettingInput(key) {
    var spec = SETTING_LABELS[key];
    if (spec === null) return null;
    var text = spec ? spec[0] : key;
    // 长说明优先（spec[2]），否则把可见文案当提示；其它字段的 label.title
    // 数量正好等于面板里 field 的数量，所以可以用它反过来校验"每个字段都有说明"。
    var tip = (spec && spec[2]) || text;
    var type = spec ? spec[1] : 'text';
    if (type === 'cards') {
      return buildCardPicker(key, text, tip);
    }
    if (type === 'lines') {
      // 多行文本（IP 白名单这类"一行一条"的列表）：单行 input 塞十几个
      // IP 根本没法编辑。值为逗号分隔字符串（后端契约），展示成每行一条，
      // 保存时把换行/逗号统一折叠回逗号分隔。
      var lab = el('label', 'setting-wide');
      lab.title = tip;
      lab.appendChild(el('span', 'lbl', text));
      var ta = el('textarea');
      ta.rows = 6;
      ta.setAttribute('data-key', key);
      ta.value = String(state.settings[key] == null ? '' : state.settings[key])
        .split(',').map(function (s) { return s.trim(); }).filter(Boolean).join('\n');
      ta.dataset.joinWith = ',';
      lab.appendChild(ta);
      return lab;
    }
    if (type === 'raw') {
      // 原样多行文本（minisign 公钥这种）：**绝不能**走上面的 'lines'——
      // 那条分支会按逗号切分再拼回，公钥里的 base64 恰好可能含 `+`/`/` 但不会有
      // 逗号，看似能过；可一旦某个实现细节变了（比如加了注释里的逗号），
      // 保存后公钥就被悄悄改坏了，而症状是"节点验签永远失败"。
      // 这里原样存取，不做任何切分/折叠。
      var lab2 = el('label', 'setting-wide');
      lab2.title = tip;
      lab2.appendChild(el('span', 'lbl', text));
      var ta2 = el('textarea');
      ta2.rows = 3;
      ta2.setAttribute('data-key', key);
      ta2.value = state.settings[key] == null ? '' : String(state.settings[key]);
      lab2.appendChild(ta2);
      return lab2;
    }
    var label = el('label');
    label.title = tip;
    label.appendChild(el('span', 'lbl', text));
    var input;
    if (type.indexOf('select:') === 0) {
      input = el('select');
      // 选项写法 "值=显示名"；只写值时显示名取该值本身（保持向后兼容）。
      type.slice(7).split(',').forEach(function (opt) {
        var eq = opt.indexOf('=');
        var val = eq < 0 ? opt : opt.slice(0, eq);
        var name = eq < 0 ? opt : opt.slice(eq + 1);
        var o = el('option', null, name);
        o.value = val;
        input.appendChild(o);
      });
      input.value = state.settings[key];
    } else {
      input = el('input');
      input.type = type === 'number' ? 'number' : 'text';
      input.value = state.settings[key] == null ? '' : state.settings[key];
    }
    input.setAttribute('data-key', key);
    label.appendChild(input);
    return label;
  }

  // refreshTotpStatus 拉取当前账户的 TOTP 启用状态，写入 state.totpEnabled。
  // 二次确认对话框据此决定要不要显示"动态验证码/恢复码"输入框：
  // 没启用 TOTP 的管理员强行要一个动态码，等于把这个破坏性操作彻底锁死。
  function refreshTotpStatus() {
    api('/maintenance/totp').then(function (r) {
      state.totpEnabled = !!(r && r.enabled);
    }).catch(function () { state.totpEnabled = false; });
  }

  function loadSettings() {
    // 卡片目录与当前值一起取：目录来自后端、值来自库，两者缺一都渲染不出勾选组。
    // 目录取不到时降级为空数组（勾选组只剩"全部展示"开关），不阻断整个设置页。
    Promise.all([
      api('/settings'),
      api('/settings/meta').catch(function () { return { cards: [], cards_all: 'all' }; })
    ]).then(function (rs) {
      var all = rs[0] || {};
      var meta = rs[1] || {};
      state.settings = all;
      state.cardCatalog = meta.cards || [];
      state.cardsAll = meta.cards_all || 'all';
      applySiteTitle(all);
      var grid = $('settings-form');
      clear(grid);
      var any = false;
      SETTING_GROUPS.forEach(function (g) {
        var title = g[0], keys = g[1];
        var inner = el('div', 'settings-grid');
        keys.forEach(function (key) {
          if (!state.settings.hasOwnProperty(key)) return;
          var label = buildSettingInput(key);
          if (!label) return;
          inner.appendChild(label);
          any = true;
        });
        if (!inner.childNodes.length) return;
        var sect = el('div', 'settings-section');
        sect.appendChild(el('h4', null, title));
        sect.appendChild(inner);
        grid.appendChild(sect);
      });
      // 兜底：后端返回了但分组里没有的键也显示出来，避免新增设置项漏掉。
      var known = {};
      SETTING_GROUPS.forEach(function (g) { g[1].forEach(function (k) { known[k] = true; }); });
      var extraKeys = Object.keys(state.settings).filter(function (k) {
        return !known[k] && SETTING_LABELS[k] !== null;
      });
      if (extraKeys.length) {
        var inner2 = el('div', 'settings-grid');
        extraKeys.sort().forEach(function (key) {
          var label = buildSettingInput(key);
          if (label) inner2.appendChild(label);
        });
        if (inner2.childNodes.length) {
          var sect2 = el('div', 'settings-section');
          sect2.appendChild(el('h4', null, '其他'));
          sect2.appendChild(inner2);
          grid.appendChild(sect2);
          any = true;
        }
      }
      if (!any) grid.appendChild(el('p', 'muted', '没有可编辑的设置项。'));
    }).catch(function (e) { toast(e.message, true); });
  }

  $('btn-reload-settings').addEventListener('click', function () { loadSettings(); });

  $('btn-save-settings').addEventListener('click', function () {
    var body = {};
    Array.prototype.forEach.call($('settings-form').querySelectorAll('[data-key]'), function (n) {
      var v = n.value;
      // 多行列表（每行一条）折叠回后端契约的逗号分隔形式。
      if (n.tagName === 'TEXTAREA' && n.dataset.joinWith) {
        v = v.split(/[\n,]+/).map(function (s) { return s.trim(); }).filter(Boolean).join(n.dataset.joinWith);
      }
      body[n.getAttribute('data-key')] = v;
    });
    // 保存按钮在长表单底部，保存完成后必须让操作者看见反馈。
    // 提示本身已固定跟随视口（见 toast 的注释），这里再补一步：scrollIntoView
    // 把视口拉回表单顶部，让"我改了哪些字段"和"保存成功"在同一屏里对上。
    // 但要放在 toast 之后、且 block:'start' 之外再滚一次顶栏补偿——
    // 顶栏是 sticky 的，直接把元素滚到视口顶端会被顶栏压住。
    guard(api('/settings', { method: 'PUT', body: body }), '设置已保存（受影响节点会自动重新拉取配置）')
      .then(function () {
        loadSettings();
        var bar = document.querySelector('.topbar');
        var offset = (bar ? bar.offsetHeight : 0) + 12;
        var target = $('view-settings');
        if (target && target.scrollIntoView) {
          target.scrollIntoView({ block: 'start' });
          window.scrollBy(0, -offset);
        }
      });
  });

  function loadThemes() {
    api('/themes').then(function (r) {
      state.themeList = r.themes || [];
      var sel = $('theme-select');
      clear(sel);
      state.themeList.forEach(function (t) {
        var o = el('option', null, t.name + (t.builtin ? '（内置）' : '（外部）') + ' — ' + ((t.meta && t.meta.name) || ''));
        o.value = t.name;
        sel.appendChild(o);
      });
      sel.value = r.active;
    }).catch(function () { /* 忽略 */ });
  }

  $('btn-apply-theme').addEventListener('click', function () {
    var name = $('theme-select').value;
    var t = state.themeList.filter(function (x) { return x.name === name; })[0];
    if (t && !t.builtin) {
      var goApply = function () {
        guard(api('/themes/active', { method: 'PUT', body: { name: name } }), '主题已切换，立即生效');
      };
      askDialog({
        title: '切换到外部主题「' + name + '」',
        desc: '第三方主题与页面同源运行，等同于可信代码，可能读取管理会话。请确认你信任该主题。',
        okText: '切换'
      }).then(function (r) { if (r.ok) goApply(); });
      return;
    }
    guard(api('/themes/active', { method: 'PUT', body: { name: name } }), '主题已切换，立即生效');
  });

  // 外部心跳状态卡。
  //
  // 为什么要单独做这块：心跳只写日志，管理员配完 heartbeat_url 无从判断是否生效——
  // 配错地址、外部服务返回 401、或压根还没到第一个 5 分钟，表面都没有区别。
  function loadHeartbeat() {
    api('/maintenance/heartbeat').then(function (r) {
      var box = $('heartbeat-box');
      clear(box);

      // 未配置：只说事实，不摆一堆 0 让人误读成"失败 0 次 = 正常"。
      if (!r.configured) {
        var p = el('p', 'muted note', '未配置。填写 heartbeat_url 后，这里会显示心跳的实时状态。');
        box.appendChild(p);
        return;
      }

      var state = r.healthy ? '正常' : '异常';
      var stateTip = r.healthy
        ? '最近一次心跳发送成功（或进程刚启动、尚未到第一个心跳点）'
        : '最近一次心跳失败，或已连续失败多次。外部监控可能已经开始告警。';

      var cards = el('div', 'cards');
      function card(k, v, tip) {
        var c = el('div', 'card');
        var kk = el('div', 'k', k);
        if (tip) kk.title = tip;
        c.appendChild(kk);
        c.appendChild(el('div', 'v', v));
        return c;
      }

      cards.appendChild(card('状态', state, stateTip));
      cards.appendChild(card('最后成功', r.last_ok ? ago(r.last_ok) : '从未',
        r.last_ok ? '最近一次成功发送心跳的时间：' + fmtTime(r.last_ok)
                  : '本次进程启动以来还没有成功发送过心跳'));
      cards.appendChild(card('连续失败', r.fail_count + ' 次',
        '自上次成功以来的连续失败次数；连续 2 次以上说明错过了完整周期'));
      cards.appendChild(card('发送间隔', Math.round((r.interval_sec || 300) / 60) + ' 分钟',
        'Master 向外发心跳的频率，外部监控应据此设置宽限期'));
      cards.appendChild(card('累计成功', (r.sent || 0) + ' 次', '本次进程启动以来成功发送的心跳总数'));
      box.appendChild(cards);

      // URL 单列一行（可能很长，塞进卡片会挤坏网格布局）。
      var urlRow = el('div', 'row');
      var lab = el('label', null, '心跳地址');
      lab.title = '当前生效的 heartbeat_url（在「外部与高级」分组里修改）';
      var val = el('code', 'mono-wrap', r.url || '-');
      lab.appendChild(val);
      urlRow.appendChild(lab);
      box.appendChild(urlRow);

      if (r.last_error) {
        var errRow = el('div', 'row');
        var errLab = el('label', null, '最近错误');
        errLab.title = '最后一次失败的原因';
        var code = el('code', 'mono-wrap err-text', r.last_error);
        errLab.appendChild(code);
        errRow.appendChild(errLab);
        box.appendChild(errRow);
      }
    }).catch(function (e) { toast(e.message, true); });
  }

  // 数据库备份列表与轮转状态（6.6）。
  //
  // 为什么要把 keep / hour / 目录都显示出来：备份是"出事时才看"的功能，
  // 到那时才发现"自动备份其实没开"或"只留了 1 份且已被覆盖"就太晚了。
  // 这里让配置与现状（实际有几份、最新一份是什么时候）同屏可见。
  function loadBackups() {
    api('/maintenance/backups').then(function (r) {
      var box = $('backup-box');
      clear(box);

      var cards = el('div', 'cards');
      function card(k, v, tip) {
        var c = el('div', 'card');
        var kk = el('div', 'k', k);
        if (tip) kk.title = tip;
        c.appendChild(kk);
        c.appendChild(el('div', 'v', v));
        return c;
      }

      var latest = (r.backups && r.backups.length) ? r.backups[0] : null;
      cards.appendChild(card('自动备份', r.enabled ? '已开启' : '已关闭',
        '由设置项「每日自动备份」控制；关闭后只在手动点击「立即备份」时生成'));
      cards.appendChild(card('备份份数', r.count + ' / ' + r.keep,
        '当前保存的备份数量与上限（设置项「备份保留份数」）。超出上限时自动删除最旧的一份'));
      cards.appendChild(card('最近备份', latest ? ago(latest.created_at) : '从未',
        latest ? '文件名：' + latest.name + '\n生成于 ' + fmtTime(latest.created_at) : '还没有任何备份'));
      cards.appendChild(card('占用空间', fmtBytes(r.total_size || 0),
        '全部备份文件的总大小，用于估算备份盘的容量规划'));
      cards.appendChild(card('备份时刻', (r.hour < 10 ? '0' : '') + r.hour + ':00',
        '每日自动备份的时间点（Master 本地时区），由设置项「自动备份时刻」控制'));
      box.appendChild(cards);

      var dirRow = el('div', 'row');
      var lab = el('label', null, '备份目录');
      lab.title = '备份文件所在目录；建议用独立盘或同步工具把它复制到异地';
      lab.appendChild(el('code', 'mono-wrap', r.dir || '-'));
      dirRow.appendChild(lab);
      box.appendChild(dirRow);

      if (!r.backups || !r.backups.length) {
        box.appendChild(el('p', 'muted note', '还没有备份。点击「立即备份」生成第一份。'));
        return;
      }

      var wrap = el('div', 'table-wrap');
      var t = el('table');
      var thead = el('thead');
      var htr = el('tr');
      ['备份文件', '生成时间', '大小', '操作'].forEach(function (h) {
        htr.appendChild(el('th', null, h));
      });
      thead.appendChild(htr);
      t.appendChild(thead);
      var tb = el('tbody');
      r.backups.forEach(function (b, i) {
        var tr = el('tr');
        var nameTd = el('td');
        var code = el('code', 'mono-wrap', b.name);
        nameTd.appendChild(code);
        if (i === 0) nameTd.appendChild(el('span', 'badge ok', '最新'));
        tr.appendChild(nameTd);
        var tsTd = el('td', null, fmtTime(b.created_at));
        tsTd.title = b.path;
        tr.appendChild(tsTd);
        tr.appendChild(el('td', null, fmtBytes(b.size_bytes)));
        // 操作列：恢复 + 删除，二者都需密码（TOTP 开启时再加动态码）二次确认。
        // 刻意不做"一键清空全部"：备份是最后一道退路，想整体控制占用应当
        // 调小「备份保留份数」让轮转去做（它自带 keep<=0 的保护）。
        var actTd = el('td');
        // 从备份恢复：最高危操作，用备份**覆盖当前整个库**，不可撤销。
        actTd.appendChild(btn('恢复', 'ghost mini', function () {
          askDialog({
            title: '从备份恢复「' + b.name + '」',
            desc: '将用这份备份**覆盖当前整个数据库**（节点、令牌、设置、告警、历史数据等），' +
              '操作不可撤销，且会把数据回退到备份时刻的状态。当前登录会话会被保留。\n确定要继续吗？',
            okText: '恢复备份',
            auth: true
          }).then(function (res) {
            if (!res.ok) return;
            guard(api('/maintenance/backups/' + encodeURIComponent(b.name) + '/restore', {
              method: 'POST', body: { password: res.password, code: res.code || '' }
            }), '已从备份「' + b.name + '」恢复').then(function () { loadBackups(); });
          });
        }));
        actTd.appendChild(btn('删除', 'ghost danger mini', function () {
          askDialog({
            title: '删除备份「' + b.name + '」',
            desc: fmtBytes(b.size_bytes) + ' 会被直接从磁盘删掉，不可恢复。\n' +
              '若只是想控制占用，调小设置项「备份保留份数」让轮转自动清理更稳妥。',
            okText: '删除备份',
            auth: true
          }).then(function (res) {
            if (!res.ok) return;
            guard(api('/maintenance/backups/' + encodeURIComponent(b.name), {
              method: 'DELETE', body: { password: res.password, code: res.code || '' }
            }), '已删除备份「' + b.name + '」')
              .then(function () { loadBackups(); });
          });
        }));
        tr.appendChild(actTd);
        tb.appendChild(tr);
      });
      t.appendChild(tb);
      wrap.appendChild(t);
      box.appendChild(wrap);
    }).catch(function (e) { toast(e.message, true); });
  }

  $('btn-refresh-backups').addEventListener('click', function () { loadBackups(); });

  // 账户安全：TOTP 启用流程与恢复码（6.2）。
  //
  // 交互设计上的两个关键点：
  //  1. **生成密钥与启用分两步**。先展示二维码与密钥，管理员在 App 里加好后
  //     再输入一次动态码确认。直接"生成即启用"的话，扫错码的人会在
  //     自己还没验证通过的状态下被锁在后台外面。
  //  2. **恢复码只显示一次**，用醒目的样式+明确的"请现在保存"提示。
  //     刷新页面后就再也取不回来了（库里只有哈希），不说清楚的话
  //     管理员会以为"以后还能在后台查到"。
  function loadTOTP() {
    api('/account/totp').then(function (r) {
      renderTOTP(r);
      renderRecovery(r);
    }).catch(function (e) { toast(e.message, true); });
  }

  function renderTOTP(r) {
    var box = $('totp-box');
    clear(box);

    var row = el('div', 'row');
    var status = el('label', null, '状态');
    status.title = 'TOTP 是否已启用';
    status.appendChild(el('strong', r.enabled ? 'ok-text' : 'muted-text',
      r.enabled ? '已启用' : '未启用'));
    row.appendChild(status);
    if (r.enabled) {
      var b = el('button', 'ghost', '关闭两步验证');
      b.addEventListener('click', function () { openTOTPDisable(); });
      row.appendChild(b);
    } else {
      var b2 = el('button', 'primary', '启用两步验证');
      b2.addEventListener('click', function () { startTOTPSetup(); });
      row.appendChild(b2);
    }
    box.appendChild(row);

    if (!r.enabled) {
      box.appendChild(el('p', 'muted note',
        '启用后登录需要输入认证器 App（Google Authenticator / Aegis / 1Password 等）里的 6 位动态码。' +
        '强烈建议同时保存恢复码，以免手机丢失后无法登录。'));
    }
  }

  function renderRecovery(r) {
    var box = $('recovery-box');
    clear(box);
    if (!r.enabled) return;

    var row = el('div', 'row');
    var lab = el('label', null, '恢复码');
    lab.title = '一次性登录凭证；动态验证器不可用时使用';
    var text = r.recovery_unused + ' / ' + r.recovery_total + ' 个未使用';
    lab.appendChild(el('strong', r.recovery_low ? 'warn-text' : null, text));
    row.appendChild(lab);
    var b = el('button', 'ghost', '重新生成恢复码');
    b.addEventListener('click', function () { openRecoveryRegen(); });
    row.appendChild(b);
    box.appendChild(row);

    if (r.recovery_low) {
      box.appendChild(el('p', 'muted note warn-text',
        '未使用的恢复码已不足 3 个。恢复码只有在下发那一次可见，请尽快「重新生成恢复码」并保存到安全位置。'));
    } else {
      box.appendChild(el('p', 'muted note',
        '恢复码为一次性凭证，可在手机丢失时替代动态码登录。重新生成会**立即作废**全部旧码。'));
    }
  }

  // startTOTPSetup 拉取新密钥并展示二维码/文本，等待管理员输入确认码。
  function startTOTPSetup() {
    api('/account/totp/setup', { method: 'POST', body: {} }).then(function (r) {
      showTOTPSetupDialog(r);
    }).catch(function (e) { toast(e.message, true); });
  }

  function showTOTPSetupDialog(setup) {
    var box = $('totp-box');
    var dlg = el('div', 'inline-panel');
    dlg.appendChild(el('h3', null, '配置认证器 App'));

    var steps = el('ol', 'muted note');
    steps.appendChild(el('li', null, '在手机上的认证器 App 里选择「扫描二维码」或「手动输入密钥」。'));
    steps.appendChild(el('li', null, '扫码或粘贴下方密钥后，输入 App 里显示的 6 位动态码完成绑定。'));
    dlg.appendChild(steps);

    // 二维码：SVG 由**服务端**渲染（auth.QRSVG），前端只负责插进 DOM。
    //
    // 为什么不前端手写 QR 编码：CSP 是 default-src 'self'，既不能引 CDN 的
    // qrcode 库，也不能用 data: 图片；手写编码又极容易出现"画得出来但扫不出来"
    // 的静默错误。服务端那份已用独立参考实现逐模块比对 + 真实解码器扫过。
    var qrWrap = el('div', 'qr-wrap');
    if (setup.qr_svg) {
      qrWrap.appendChild(svgNode(setup.qr_svg));
    } else {
      qrWrap.appendChild(el('p', 'muted note',
        '二维码生成失败，请使用下方密钥在认证器 App 里手动添加。'));
    }
    dlg.appendChild(qrWrap);

    var secretRow = el('div', 'row');
    var sLab = el('label', null, '密钥（手动输入用）');
    sLab.appendChild(el('code', 'mono-wrap', setup.secret));
    secretRow.appendChild(sLab);
    dlg.appendChild(secretRow);

    var codeRow = el('div', 'row');
    var cLab = el('label', null, '动态验证码');
    var input = document.createElement('input');
    input.type = 'text';
    input.inputMode = 'numeric';
    input.maxLength = 6;
    input.placeholder = '6 位数字';
    input.autocomplete = 'one-time-code';
    cLab.appendChild(input);
    codeRow.appendChild(cLab);
    dlg.appendChild(codeRow);

    var err = el('p', 'err-text');
    dlg.appendChild(err);

    var actions = el('div', 'row');
    var cancel = el('button', 'ghost', '取消');
    cancel.addEventListener('click', function () { loadTOTP(); });
    var confirm = el('button', 'primary', '确认启用');
    confirm.addEventListener('click', function () {
      var code = (input.value || '').trim();
      if (!/^[0-9]{6}$/.test(code)) {
        err.textContent = '请输入 6 位数字动态码';
        return;
      }
      confirm.disabled = true;
      api('/account/totp/enable', {
        method: 'POST', body: { secret: setup.secret, code: code }
      }).then(function (res) {
        dlg.remove();
        if (res.warning) {
          toast(res.warning, true);
        } else {
          toast('两步验证已启用');
        }
        if (res.recovery_code) showRecoveryCodes(res.recovery_code);
        loadTOTP();
      }).catch(function (e) {
        confirm.disabled = false;
        err.textContent = e.message;
      });
    });
    actions.appendChild(cancel);
    actions.appendChild(confirm);
    dlg.appendChild(actions);

    box.appendChild(dlg);
    input.focus();
  }

  // showRecoveryCodes 展示刚生成的恢复码（**唯一一次**明文可见的机会）。
  function showRecoveryCodes(codes) {
    var box = $('recovery-box');
    clear(box);
    var panel = el('div', 'inline-panel warn-panel');
    panel.appendChild(el('h3', null, '请立即保存这些恢复码'));
    panel.appendChild(el('p', 'muted note',
      '每个码只能使用一次。关闭或刷新本页面后就**无法再次查看**（服务端只保存了它们的哈希）。' +
      '请抄写到纸上或存入密码管理器，并与密码分开保管。'));
    var list = el('div', 'recovery-grid');
    codes.forEach(function (c) {
      list.appendChild(el('code', 'recovery-code', c));
    });
    panel.appendChild(list);

    var row = el('div', 'row');
    var copy = el('button', 'ghost', '复制全部');
    copy.addEventListener('click', function () {
      var text = codes.join('\n');
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function () {
          toast('已复制到剪贴板');
        }).catch(function () { toast('复制失败，请手动选中', true); });
      } else {
        toast('当前浏览器不支持自动复制，请手动选中', true);
      }
    });
    var done = el('button', 'primary', '我已保存');
    done.addEventListener('click', function () { loadTOTP(); });
    row.appendChild(copy);
    row.appendChild(done);
    panel.appendChild(row);
    box.appendChild(panel);
  }

  function openTOTPDisable() {
    var pw = window.prompt('关闭两步验证会降低账户安全性。\n请输入当前密码以确认：');
    if (pw === null) return;
    var code = window.prompt('请输入认证器 App 里的 6 位动态码，或一个未使用的恢复码：');
    if (code === null) return;
    api('/account/totp/disable', {
      method: 'POST', body: { password: pw, code: code }
    }).then(function () {
      toast('已关闭两步验证');
      loadTOTP();
    }).catch(function (e) { toast(e.message, true); });
  }

  function openRecoveryRegen() {
    var pw = window.prompt('重新生成会立即作废全部现有恢复码。\n请输入当前密码以确认：');
    if (pw === null) return;
    api('/account/recovery-codes', {
      method: 'POST', body: { password: pw }
    }).then(function (res) {
      showRecoveryCodes(res.recovery_code);
    }).catch(function (e) { toast(e.message, true); });
  }

  function loadStorage() {
    api('/storage/stats').then(function (r) {
      var box = $('storage-box');
      clear(box);
      var wr = el('div', 'cards');
      function card(k, v, tip) {
        var c = el('div', 'card');
        var kk = el('div', 'k', k);
        if (tip) kk.title = tip;
        c.appendChild(kk);
        c.appendChild(el('div', 'v', v));
        return c;
      }
      wr.appendChild(card('数据库文件', r.db_path || '-', 'SQLite 主库文件路径'));
      wr.appendChild(card('已占用空间', r.db_size_human || '-', 'SQLite 主库文件当前大小（含索引与页面碎片）'));
      wr.appendChild(card('每节点/天', r.per_node_daily_human || '-', '按当前行数估算的时序数据日均占用（节点越多越接近真实值）'));
      wr.appendChild(card('写队列积压', r.writer.queued, '等待落盘的时序行数'));
      wr.appendChild(card('已写入行数', r.writer.written, '本次启动以来写入的总行数'));
      wr.appendChild(card('丢弃行数', r.writer.dropped, '队列满时按策略丢弃的旧高精度行'));
      wr.appendChild(card('写入暂停', r.writer.paused ? '是（磁盘水位保护）' : '否', '磁盘剩余空间低于阈值时暂停写入'));
      box.appendChild(wr);

      var wrap = el('div', 'table-wrap');
      var t = el('table');
      t.className = 'storage-table';
      var thead = el('thead');
      var htr = el('tr');
      var thName = el('th', null, '表'); thName.title = '数据库表名';
      var thDesc = el('th', null, '说明'); thDesc.title = '该表存储什么数据';
      var thRows = el('th', null, '行数'); thRows.title = '当前表中的近似行数';
      var thEst = el('th', null, '估算占用'); thEst.title = '行数 × 经验行宽（含索引冗余）';
      htr.appendChild(thName); htr.appendChild(thDesc); htr.appendChild(thRows); htr.appendChild(thEst);
      thead.appendChild(htr);
      t.appendChild(thead);
      var tb = el('tbody');
      (r.tables || []).forEach(function (tinfo) {
        var tr = el('tr');
        tr.appendChild(td(tinfo.name));
        tr.appendChild(td(tinfo.description || '-', 'wrap'));
        var tdRows = td(tinfo.count.toLocaleString('zh-CN'));
        tdRows.style.textAlign = 'right';
        tr.appendChild(tdRows);
        var tdEst = td(fmtBytes(tinfo.count * tinfo.bytes_per_row));
        tdEst.style.textAlign = 'right';
        tr.appendChild(tdEst);
        tb.appendChild(tr);
      });
      t.appendChild(tb);
      wrap.appendChild(t);
      box.appendChild(wrap);

      // 清理历史时序数据（不可恢复）：删掉早于 cutoff 的全部遥测流水，缓解库膨胀。
      // 只动遥测流水，节点/令牌/设置等实体不动；需要密码（TOTP 开启时再加动态码）二次确认。
      var purge = el('div', 'panel');
      purge.appendChild(el('h3', null, '清理历史数据'));
      purge.appendChild(el('p', 'muted note',
        '删除节点更早的时序遥测（CPU/内存/流量/延迟等），不可恢复。节点、令牌、设置等实体不受影响。'));
      var pf = el('div', 'row');
      var daysInput = el('input');
      daysInput.type = 'number'; daysInput.min = '1'; daysInput.value = '90';
      daysInput.style.width = '90px';
      var daysLab = el('label', null, '保留最近 ');
      daysLab.appendChild(daysInput);
      daysLab.appendChild(document.createTextNode(' 天'));
      var dateInput = el('input');
      dateInput.type = 'date';
      var dateLab = el('label', null, '或删除 ');
      dateLab.appendChild(dateInput);
      dateLab.appendChild(document.createTextNode(' 之前的数据'));
      pf.appendChild(daysLab);
      pf.appendChild(dateLab);
      purge.appendChild(pf);
      purge.appendChild(btn('清理历史数据', 'ghost danger', function () {
        var days = parseInt(daysInput.value, 10);
        var beforeDate = dateInput.value;
        var body = {};
        if (beforeDate) body.before_date = beforeDate;
        else if (days > 0) body.days = days;
        else { toast('请填写保留天数或截止日期', true); return; }
        askDialog({
          title: '清理历史时序数据',
          desc: beforeDate
            ? ('将删除 ' + beforeDate + ' 之前（不含当天）的全部遥测数据，不可恢复。')
            : ('将删除最近 ' + days + ' 天之前（即保留最近 ' + days + ' 天）的全部遥测数据，不可恢复。'),
          okText: '清理数据',
          auth: true
        }).then(function (res) {
          if (!res.ok) return;
          body.password = res.password;
          body.code = res.code || '';
          guard(api('/maintenance/storage/purge', { method: 'POST', body: body }), null)
            .then(function (out) {
              if (out && out.deleted) {
                var parts = Object.keys(out.deleted).map(function (k) {
                  return k + ' ' + (out.deleted[k] || 0).toLocaleString('zh-CN');
                });
                toast('已清理 ' + (out.total || 0).toLocaleString('zh-CN') + ' 行（' + parts.join('、') + '）');
              }
              loadStorage();
            });
        });
      }));
      box.appendChild(purge);
    }).catch(function (e) { toast(e.message, true); });
  }

  $('btn-backup').addEventListener('click', function () {
    guard(api('/maintenance/backup', { method: 'POST' })).then(function (r) {
      if (r) toast('备份已生成：' + r.path);
    });
  });

  // -------------------------------------------------------------------------
  // 发布文件 / 审计日志
  // -------------------------------------------------------------------------

  function loadReleases() {
    api('/agent-releases').then(function (r) {
      var box = $('releases-box');
      clear(box);
      (r.missing || []).forEach(function () { /* 缺失项在下方统一展示 */ });
      var rels = r.releases || [];
      if (!rels.length) {
        box.appendChild(el('p', 'muted', 'dist 目录下还没有发布文件。把 xb-probe-agent_<os>_<arch> 与 SHA256SUMS 放到 data_dir/dist/<version>/ 即可。'));
      }
      rels.forEach(function (rel) {
        var card = el('div', 'panel');
        var head = el('div', 'panel-head');
        head.appendChild(el('h3', null, '版本 ' + rel.version));
        // 一键删除该版本的全部发布文件（不可恢复，只影响分发源）。
        head.appendChild(btn('删除整个版本', 'ghost danger mini', function () {
          askDialog({
            title: '删除整个版本「' + rel.version + '」',
            desc: '将删除该版本目录下的全部 Agent 发布文件（' + ((rel.files || []).length) + ' 个），不可恢复。\n' +
              '注意：已下载到各节点的副本不会被回收，只是收回分发源。',
            okText: '删除版本'
          }).then(function (res) {
            if (!res.ok) return;
            guard(api('/agent-releases/' + encodeURIComponent(rel.version), { method: 'DELETE' }),
              '已删除版本「' + rel.version + '」').then(function () { loadReleases(); });
          });
        }));
        card.appendChild(head);
        var wrap = el('div', 'table-wrap');
        var t = el('table');
        var thead = el('thead');
        var htr = el('tr');
        ['文件', '大小', 'SHA-256', '操作'].forEach(function (x) { htr.appendChild(el('th', null, x)); });
        thead.appendChild(htr);
        t.appendChild(thead);
        var tb = el('tbody');
        (rel.files || []).forEach(function (f) {
          var tr = el('tr');
          tr.appendChild(td(f.name));
          tr.appendChild(td(fmtBytes(f.size)));
          tr.appendChild(td(f.sha256 || '-', 'wrap'));
          var actTd = td();
          actTd.appendChild(btn('删除', 'ghost danger mini', function () {
            askDialog({
              title: '删除文件「' + f.name + '」',
              desc: '版本 ' + rel.version + ' 的 ' + f.name + ' 将被删除，不可恢复。',
              okText: '删除文件'
            }).then(function (res) {
              if (!res.ok) return;
              guard(api('/agent-releases/' + encodeURIComponent(rel.version) + '/' + encodeURIComponent(f.name), { method: 'DELETE' }),
                '已删除文件「' + f.name + '」').then(function () { loadReleases(); });
            });
          }));
          tr.appendChild(actTd);
          tb.appendChild(tr);
        });
        t.appendChild(tb);
        wrap.appendChild(t);
        card.appendChild(wrap);
        box.appendChild(card);
      });
      if (r.missing && r.missing.length) {
        var m = el('div', 'panel');
        m.appendChild(el('h3', null, '缺失的平台（' + r.missing.length + '）'));
        r.missing.forEach(function (p) { m.appendChild(el('div', 'muted', p)); });
        box.appendChild(m);
      }
    }).catch(function (e) { toast(e.message, true); });
  }

  $('btn-refresh-releases').addEventListener('click', loadReleases);

  // 审计日志保留期面板（6.5）。保留期决定"多久前的审计记录会被自动清理"，
  // 直接关系到能否追溯历史操作，因此修改/立即清理都要密码（TOTP 开启再加动态码）二次确认。
  function loadAuditRetention() {
    var box = $('audit-retention-box');
    clear(box);
    // 从审计日志分页响应里拿 retention_days 与 total（省一个往返）。
    api('/audit-log?limit=1&offset=0').then(function (res) {
      var days = res.retention_days || 30;
      clear(box);
      var head = el('div', 'panel-head');
      head.appendChild(el('h3', null, '审计日志保留'));
      head.appendChild(el('span', 'muted', '当前 ' + (res.total || 0).toLocaleString('zh-CN') + ' 条记录'));
      box.appendChild(head);
      box.appendChild(el('p', 'muted note',
        '默认保留 30 天，超期的记录会被自动清理。修改保留期会立刻按新值清理一次，' +
        '并（在告警页开启"审计保留期变更"时）发一条告警。'));
      var row = el('div', 'row');
      var inp = el('input');
      inp.type = 'number'; inp.min = '1'; inp.max = '3650'; inp.value = String(days);
      inp.style.width = '110px';
      var lab = el('label', null, '保留最近 ');
      lab.appendChild(inp);
      lab.appendChild(document.createTextNode(' 天'));
      row.appendChild(lab);
      row.appendChild(btn('保存保留期', 'ghost', function () {
        var d = parseInt(inp.value, 10);
        if (!(d >= 1 && d <= 3650)) { toast('保留天数须为 1~3650 的整数', true); return; }
        askDialog({
          title: '修改审计日志保留期',
          desc: '将保留期改为 ' + d + ' 天，并立即清理 ' + d + ' 天之前的记录（不可恢复）。' +
            (state.totpEnabled ? '请输入密码与动态验证码。' : '请输入管理员密码。'),
          okText: '保存',
          auth: true
        }).then(function (r) {
          if (!r.ok) return;
          guard(api('/maintenance/audit/retention', {
            method: 'PUT', body: { days: d, password: r.password, code: r.code || '' }
          }), '保留期已改为 ' + d + ' 天').then(function () { loadAuditRetention(); loadAudit(); });
        });
      }));
      row.appendChild(btn('立即清理', 'ghost danger', function () {
        askDialog({
          title: '立即清理审计日志',
          desc: '将按当前保留期（' + days + ' 天）清理一次超期记录，不可恢复。' +
            (state.totpEnabled ? '请输入密码与动态验证码。' : '请输入管理员密码。'),
          okText: '清理',
          auth: true
        }).then(function (r) {
          if (!r.ok) return;
          guard(api('/maintenance/audit/prune', {
            method: 'POST', body: { password: r.password, code: r.code || '' }
          }), null).then(function (out) {
            if (out) toast('已清理 ' + (out.deleted || 0).toLocaleString('zh-CN') + ' 条超期记录');
            loadAuditRetention(); loadAudit();
          });
        });
      }));
      box.appendChild(row);
    }).catch(function (e) { toast(e.message, true); });
  }

  function loadAudit() {
    var st = pages.audit;
    api('/audit-log?limit=' + st.limit + '&offset=' + st.offset).then(function (res) {
      var list = adoptPage(st, res);
      var tb = $('audit-table').querySelector('tbody');
      clear(tb);
      list.forEach(function (a) {
        var tr = el('tr');
        tr.appendChild(td(fmtTime(a.ts)));
        tr.appendChild(td(a.actor));
        var act = td(AUDIT_ACTION[a.action] || a.action);
        act.title = '原始动作：' + a.action;
        tr.appendChild(act);
        var obj = td(auditTargetText(a.action, a.target));
        obj.title = '被操作对象的类型与编号（' + (a.target || '无') + '）';
        tr.appendChild(obj);
        tr.appendChild(td(a.ip || '-'));
        tb.appendChild(tr);
      });
      if (!list.length) {
        var tr0 = el('tr');
        var td0 = td(st.total ? '这一页没有记录' : '还没有审计记录', 'muted');
        td0.colSpan = 5;
        tr0.appendChild(td0);
        tb.appendChild(tr0);
      }
      renderPager($('audit-pager'), 'audit', loadAudit);
    }).catch(function (e) { toast(e.message, true); });
  }

  $('btn-refresh-audit').addEventListener('click', loadAudit);

  // -------------------------------------------------------------------------
  // 深浅主题（与公开看板同一套逻辑，改要一起改）
  // -------------------------------------------------------------------------
  // data-theme 写在 <html> 上；只有切换动作落 localStorage（probe.theme）——
  // **启动时绝不**把系统答案写进去，否则管理员没碰过按钮也会被钉死在
  // 加载那天的系统模式。太阳/月亮图标的显隐由 CSS 完成，JS 只更新无障碍标签。
  //
  // ?theme=dark|light 只作用于本次加载、**不落盘**：与公开看板同一个入口，
  // 用来截图与排障（"把某个模式下的后台直接链接给别人"）。登录页也吃这个参数
  // ——不然想截一张暗色登录页就只能先改系统偏好。
  var ADMIN_THEME_KEY = 'probe.theme';

  function adminSystemDark() {
    return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
  }
  function adminCurrentTheme() {
    var t = document.documentElement.getAttribute('data-theme');
    return t === 'dark' || t === 'light' ? t : (adminSystemDark() ? 'dark' : 'light');
  }
  function syncThemeButtons() {
    var next = adminCurrentTheme() === 'dark' ? '浅色' : '深色';
    Array.prototype.forEach.call(document.querySelectorAll('.tgl'), function (b) {
      b.setAttribute('aria-label', '切换到' + next);
      b.setAttribute('title', '切换到' + next);
    });
  }
  function applyAdminTheme(t) {
    if (t === 'dark' || t === 'light') document.documentElement.setAttribute('data-theme', t);
    else document.documentElement.removeAttribute('data-theme');
    syncThemeButtons();
  }

  (function initAdminTheme() {
    var q = '';
    try { q = new URLSearchParams(location.search).get('theme') || ''; } catch (e) { }
    var saved = null;
    try { saved = localStorage.getItem(ADMIN_THEME_KEY); } catch (e) { }
    var t = (q === 'dark' || q === 'light') ? q
          : (saved === 'dark' || saved === 'light') ? saved
          : ''; // 跟随系统：不写 data-theme，交给 CSS 的媒体查询
    if (t) applyAdminTheme(t); else syncThemeButtons();
    // 登录卡与顶栏各有一颗 .tgl，两处共用同一段逻辑。
    Array.prototype.forEach.call(document.querySelectorAll('.tgl'), function (b) {
      b.addEventListener('click', function () {
        var next = adminCurrentTheme() === 'dark' ? 'light' : 'dark';
        applyAdminTheme(next);
        try { localStorage.setItem(ADMIN_THEME_KEY, next); } catch (e) { }
        // 图表颜色是绘制时取的 CSS 变量实色，切换后必须重跑当前视图的
        // loader 才不残留旧主题的线色（数据重取一次，切换是低频操作）。
        // 登录页上不触发：会话还没建立，loader 只会打出一屏 401 报错。
        if (!$('app').hidden && loaders[currentView]) loaders[currentView]();
      });
    });
  })();

  // -------------------------------------------------------------------------
  // 启动
  // -------------------------------------------------------------------------

  window.addEventListener('resize', function () {
    if (!$('view-node').hidden && state.node) loadHistory(state.node.id, $('hist-range').value);
  });

  boot();
})();
