// 示例外部主题：演示在当前 CSP 约束下如何只通过公开 API 取数。
// 注意：所有样式写在 style.css 里，所有脚本写在本文件里，HTML 中不得内联。
(function () {
  'use strict';

  var API = '/api/public/v1';

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  }

  function pct(v) { return v == null ? '-' : (v / 100).toFixed(1) + '%'; }

  function refresh() {
    fetch(API + '/overview', { headers: { Accept: 'application/json' } })
      .then(function (r) { return r.json(); })
      .then(function (o) {
        var s = document.getElementById('summary');
        s.textContent = '在线 ' + o.nodes_online + ' / ' + o.nodes_total
          + '　本周期流量 ' + (o.traffic_used / 1073741824).toFixed(2) + ' GB';
      })
      .catch(function () { });

    fetch(API + '/nodes', { headers: { Accept: 'application/json' } })
      .then(function (r) { return r.json(); })
      .then(function (list) {
        var box = document.getElementById('nodes');
        box.textContent = '';
        (list || []).forEach(function (n) {
          var c = el('div', 'item');
          c.appendChild(el('strong', null, n.name || n.public_id));
          c.appendChild(el('span', 'muted', ' ' + (n.status || 'offline')));
          c.appendChild(el('div', 'muted', 'CPU ' + pct(n.cpu_pct) + '　内存 ' + pct(n.mem_pct)
            + '　磁盘 ' + pct(n.disk_pct)));
          box.appendChild(c);
        });
      })
      .catch(function () { });
  }

  refresh();
  setInterval(refresh, 3000);
})();
