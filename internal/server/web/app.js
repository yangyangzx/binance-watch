(() => {
  'use strict';
  const $ = (id) => document.getElementById(id);
  const el = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined) n.textContent = text; // 一律 textContent，杜绝 XSS
    return n;
  };
  const pctText = (bp) => {
    const sign = bp > 0 ? '+' : bp < 0 ? '-' : '';
    if (bp === 0) return '0.00%';
    return sign + (Math.abs(bp) / 100).toFixed(2) + '%';
  };
  const pctCls = (bp) => (bp > 0 ? 'up' : bp < 0 ? 'down' : 'flat');
  const fmtDur = (s) => {
    if (s < 60) return s + ' 秒';
    if (s < 3600) return Math.floor(s / 60) + ' 分';
    if (s < 86400) return Math.floor(s / 3600) + ' 时 ' + Math.floor((s % 3600) / 60) + ' 分';
    return Math.floor(s / 86400) + ' 天 ' + Math.floor((s % 86400) / 3600) + ' 时';
  };
  const ago = (iso) => {
    const t = Date.parse(iso);
    if (!t || t < 1e11) return '';
    const s = Math.max(0, Math.floor((Date.now() - t) / 1000));
    return fmtDur(s) + '前';
  };

  let toastTimer;
  function toast(msg, isErr) {
    const t = $('toast');
    t.textContent = msg;
    t.className = 'toast show' + (isErr ? ' err' : '');
    clearTimeout(toastTimer);
    // 错误停留更久（且可点关闭），避免读不完长错误信息
    const life = isErr ? 6000 : 2600;
    toastTimer = setTimeout(() => (t.className = 'toast'), life);
    t.onclick = () => { t.className = 'toast'; clearTimeout(toastTimer); };
    t.tabIndex = 0;
  }

  async function api(method, path, body) {
    const opt = { method, cache: 'no-store', headers: {} };
    if (method !== 'GET') {
      opt.headers['Content-Type'] = 'application/json';
      opt.headers['X-BW'] = '1';
      opt.body = JSON.stringify(body || {});
    }
    const r = await fetch(path, opt);
    let data = null;
    try { data = await r.json(); } catch (_) { /* 非 JSON */ }
    if (!r.ok) throw new Error((data && data.error) || ('HTTP ' + r.status));
    return data;
  }

  // ------------------------------------------------------------ 渲染
  // 轮询 2s 一次；行内更新（只改文字/类名，不拆 DOM），避免打断正在输入/聚焦的表单、
  // 丢失滚动位置，以及让价格 flash 动画被反复重触发。
  const lastPx = {};
  let lastOK = Date.now();

  function pill(id, cls, text) {
    const p = $(id);
    p.className = 'pill ' + cls;
    p.lastElementChild.textContent = text;
  }

  function renderHeader(s) {
    pill('p-ws', s.ws ? 'ok' : 'bad', s.ws ? 'WS 已连接 ' + fmtDur(s.ws_since_sec) : 'WS 断开（REST 兜底）');
    const total = s.symbols.length;
    const validN = s.symbols.filter((x) => x.state !== 'invalid').length;
    const okN = s.symbols.filter((x) => x.state === 'ok').length;
    // 分母用"有效交易对数"（排除无效），避免无效交易对拉低"行情 N/M"的口径
    const feed = s.down ? 'bad' : okN === validN ? 'ok' : 'warn';
    pill('p-feed', feed, s.down ? '行情断流' : `行情 ${okN}/${validN}`);
    $('p-up').textContent = '运行 ' + fmtDur(s.uptime_sec);
    $('p-mail').textContent = '待发邮件 ' + s.pending_mail;
    const dl = $('dl-symbols');
    if (dl.childElementCount !== total) {
      dl.textContent = '';
      s.symbols.forEach((x) => { const o = el('option'); o.value = x.symbol; dl.appendChild(o); });
    }
  }

  function symSig(x) {
    return x.symbol + '|' + x.state + '|' + x.price + '|' +
      (x.changes || []).map((c) => c.label + (c.ok ? c.bp : '-')).join(',') + '|' +
      (x.atrs || []).map((a) => a.label + (a.ok ? a.price : '-')).join(',');
  }
  const symSigCache = {};
  function renderSymbols(list) {
    const box = $('symbols');
    if (!list.length) {
      if (box.firstChild && box.firstChild.className === 'empty') return; // 已是空态，不重建
      box.textContent = '';
      box.appendChild(el('div', 'empty', '还没有订阅任何交易对：在右侧新建一条规则即可自动订阅。'));
      return;
    }
    if (box.firstChild && box.firstChild.className === 'empty') box.textContent = '';
    const live = new Set(list.map((x) => x.symbol));
    for (const child of Array.from(box.children)) {
      if (child.dataset && child.dataset.sym && !live.has(child.dataset.sym)) child.remove();
    }
    list.forEach((x) => {
      let card = box.querySelector(`.sym[data-sym="${x.symbol}"]`);
      if (!card) {
        card = el('div', 'sym ' + x.state);
        card.dataset.sym = x.symbol;
        const hd = el('div', 'hd');
        const base = x.symbol.replace(/(USDT|USDC|FDUSD|BUSD|BTC|ETH|BNB)$/, '');
        const quote = x.symbol.slice(base.length);
        const name = el('span', 'name', base || x.symbol);
        if (quote && base) name.appendChild(el('small', null, '/' + quote));
        hd.appendChild(name);
        hd.appendChild(el('span', 'age'));
        card.appendChild(hd);
        card.appendChild(el('div', 'px'));
        if (x.state === 'invalid') {
          card.appendChild(el('div', 'msg', 'Binance 不存在该交易对，已停止订阅'));
        } else {
          const chg = el('div', 'chg');
          (x.changes || []).forEach((c) => {
            const d = el('div');
            d.appendChild(el('span', null, c.label));
            d.appendChild(el('b'));
            chg.appendChild(d);
          });
          card.appendChild(chg);
          const atr = el('div', 'atr');
          const atrLbl = el('span', 'lbl', 'ATR · 平均真实波幅');
          atrLbl.title = 'ATR（平均真实波幅）：单位与价格相同，表示近期波动幅度有多大，不代表涨跌方向。波动越大数值越大。';
          atr.appendChild(atrLbl);
          const cells = el('div', 'cells');
          (x.atrs || []).forEach((a) => {
            const d = el('div');
            d.title = a.label + ' 周期：取 ' + a.n + ' 根该周期 K 线的真实波幅求平均';
            d.appendChild(el('span', null, a.label));
            d.appendChild(el('b'));
            cells.appendChild(d);
          });
          atr.appendChild(cells);
          card.appendChild(atr);
        }
        box.appendChild(card);
      }
      if (!card.className.endsWith(x.state)) card.className = 'sym ' + x.state;
      const age = card.querySelector('.age');
      if (age) {
        age.className = 'age' + (x.state === 'stale' ? ' stale' : '');
        age.textContent = x.state === 'invalid' ? '无此交易对'
          : x.state === 'nodata' ? '等待数据'
          : x.state === 'stale' ? x.age_sec + 's 前（过期）'
          : x.age_sec + 's 前';
      }
      const px = card.querySelector('.px');
      if (px && px.textContent !== (x.price || '--')) {
        const prev = lastPx[x.symbol];
        if (prev !== undefined && x.price_f && prev !== x.price_f && x.state !== 'stale')
          card.classList.add(x.price_f > prev ? 'flash-up' : 'flash-down');
        px.textContent = x.price || '--';
      }
      if (x.price_f && x.state !== 'stale') lastPx[x.symbol] = x.price_f;
      // 只在涨跌幅数据真变时更新卡片，避免动画/焦点被 2s 轮询重触发
      const sig = symSig(x);
      if (symSigCache[x.symbol] !== sig) {
        if (card.querySelector('.chg')) {
          let ci = 0;
          (x.changes || []).forEach((c) => {
            const b = card.querySelectorAll('.chg b')[ci++];
            if (!b) return;
            b.className = c.ok ? pctCls(c.bp) : 'na';
            b.textContent = c.ok ? pctText(c.bp) : '--';
          });
        }
        const atrBox = card.querySelector('.atr');
        if (atrBox) {
          let ai = 0;
          (x.atrs || []).forEach((a) => {
            const b = atrBox.querySelectorAll('b')[ai++];
            if (!b) return;
            b.className = a.ok ? 'v' : 'na';
            b.textContent = a.ok ? (a.price || '--') : '--';
          });
        }
        symSigCache[x.symbol] = sig;
      }
    });
  }

  function ruleSig(r) {
    return r.active + '|' + r.armed + '|' + (r.gap || '') + '|' + r.hit_count + '|' +
      (r.last_hit_at || '') + '|' + (r.note || '');
  }
  const ruleSigCache = {};
  function renderRules(list) {
    const box = $('rules');
    $('rule-count').textContent = list.length ? `${list.filter((r) => r.active).length}/${list.length} 启用` : '';
    if (!list.length) {
      if (box.firstChild && box.firstChild.className === 'empty') return;
      box.textContent = '';
      box.appendChild(el('div', 'empty', '暂无规则。在右侧添加第一条价格提醒吧。'));
      return;
    }
    if (box.firstChild && box.firstChild.className === 'empty') box.textContent = '';
    const live = new Set(list.map((r) => r.id));
    for (const child of Array.from(box.children)) {
      if (child.dataset && child.dataset.rid && !live.has(child.dataset.rid)) child.remove();
    }
    list.forEach((r) => {
      let row = box.querySelector(`.rule[data-rid="${r.id}"]`);
      if (!row) {
        row = el('div', 'rule' + (r.active ? '' : ' off'));
        row.dataset.rid = r.id;
        row.appendChild(el('span', 'st'));
        const mid = el('div', 'mid');
        const t1 = el('div', 't1');
        t1.appendChild(el('span', 'sy', r.symbol));
        t1.appendChild(el('span', 'tag gold', r.type_label));
        t1.appendChild(el('span', 'tag'));
        mid.appendChild(t1);
        mid.appendChild(el('div', 'desc', r.desc));
        const meta = el('div', 'meta');
        mid.appendChild(meta);
        mid.appendChild(el('div', 'note'));
        row.appendChild(mid);
        const acts = el('div', 'acts');
        const sw = el('button', 'sw');
        sw.type = 'button';
        sw.setAttribute('role', 'switch');
        // 行内更新不会重建按钮，所以这里的 r.id 是稳定的；active 要读实时 DOM 状态，
        // 不能用创建时的闭包值（用户可能已经切过）。
        sw.onclick = () => {
          const nowActive = sw.classList.contains('on');
          act(() => api('POST', `/api/rules/${r.id}/active`, { active: !nowActive }), nowActive ? '已停用' : '已启用');
        };
        const del = el('button', 'icon', '✕');
        del.type = 'button';
        del.title = '删除';
        del.setAttribute('aria-label', '删除规则');
        del.onclick = () => { if (confirm(`删除 ${r.symbol} · ${r.desc} ？`)) act(() => api('DELETE', `/api/rules/${r.id}`), '已删除'); };
        acts.append(sw, del);
        row.appendChild(acts);
        box.appendChild(row);
      }
      const sig = ruleSig(r);
      if (ruleSigCache[r.id] !== sig) {
        row.className = 'rule' + (r.active ? '' : ' off');
        row.querySelector('.st').className = 'st' + (r.active ? (r.armed ? ' armed' : ' fired') : '');
        row.querySelector('.sy').textContent = r.symbol;
        row.querySelector('.tag.gold').textContent = r.type_label;
        const flagsTag = row.querySelectorAll('.tag')[1];
        flagsTag.textContent = r.flags || '';
        flagsTag.hidden = !r.flags;
        row.querySelector('.desc').textContent = r.desc;
        const meta = row.querySelector('.meta');
        meta.textContent = '';
        const status = !r.active ? (r.once && r.hit_count ? '已完成' : '已暂停') : r.armed ? '监控中' : '已触发 · 等待复位';
        meta.appendChild(el('span', null, status));
        if (r.active && r.gap) meta.appendChild(el('span', 'gap', r.gap));
        if (r.hit_count) meta.appendChild(el('span', null, `命中 ${r.hit_count} 次 · ${ago(r.last_hit_at)}`));
        row.querySelector('.note').textContent = r.note || '';
        const sw = row.querySelector('.sw');
        sw.className = 'sw' + (r.active ? ' on' : '');
        sw.setAttribute('aria-checked', String(r.active));
        sw.setAttribute('aria-label', r.active ? '停用规则' : '启用规则');
        ruleSigCache[r.id] = sig;
      }
    });
  }

  async function act(fn, okMsg) {
    try { await fn(); toast(okMsg); refresh(); } catch (e) { toast(e.message, true); }
  }

  // ------------------------------------------------------------ 轮询
  let busy = false;
  async function refresh() {
    if (busy) return;
    busy = true;
    try {
      const s = await api('GET', '/api/status');
      lastOK = Date.now();
      $('banner').hidden = true;
      renderHeader(s);
      renderSymbols(s.symbols || []);
      renderRules(s.rules || []);
    } catch (e) {
      if (Date.now() - lastOK > 6000) {
        $('banner').hidden = false;
        $('banner').textContent = '与 watchd 的连接中断：' + e.message + '（自动重试中）';
      }
    } finally { busy = false; }
  }

  // ------------------------------------------------------------ 表单
  let type = 'target', dirT = 'above', dirP = 'auto';
  // 分段控件：补全 ARIA tab 语义与键盘导航（方向键切换、Home/End），
  // 让"到价/涨跌幅/接近"和方向选择可以用键盘操作。
  function seg(id, cb) {
    const box = $(id);
    const btns = Array.from(box.querySelectorAll('button'));
    const select = (b) => {
      btns.forEach((x) => {
        x.classList.toggle('on', x === b);
        x.setAttribute('aria-selected', String(x === b));
        x.tabIndex = x === b ? 0 : -1;
      });
      b.focus();
      cb(b.dataset.v);
    };
    box.addEventListener('click', (ev) => {
      const b = ev.target.closest('button');
      if (b) select(b);
    });
    box.addEventListener('keydown', (ev) => {
      const idx = btns.findIndex((b) => b.classList.contains('on'));
      if (idx < 0) return;
      let next = idx;
      if (ev.key === 'ArrowRight' || ev.key === 'ArrowDown') next = (idx + 1) % btns.length;
      else if (ev.key === 'ArrowLeft' || ev.key === 'ArrowUp') next = (idx - 1 + btns.length) % btns.length;
      else if (ev.key === 'Home') next = 0;
      else if (ev.key === 'End') next = btns.length - 1;
      else return;
      ev.preventDefault();
      select(btns[next]);
    });
    // 初始化选中态
    const on = btns.find((b) => b.classList.contains('on')) || btns[0];
    if (on) select(on);
  }
  seg('seg-type', (v) => {
    type = v;
    $('g-target').hidden = v !== 'target';
    $('g-pct').hidden = v !== 'pct';
    $('g-near').hidden = v !== 'near';
    $('hint').textContent = '';
  });
  seg('seg-dir-t', (v) => (dirT = v));
  seg('seg-dir-p', (v) => (dirP = v));

  const num = (s) => { const n = Number(String(s).trim().replace(/,/g, '')); return Number.isFinite(n) ? n : NaN; };

  // 交易对实时校验：失焦/输入时就提示格式问题，不用等到提交
  $('f-symbol').addEventListener('blur', () => {
    const hint = $('hint');
    const v = $('f-symbol').value.trim().toUpperCase();
    if (v && !/^[A-Z0-9]{4,24}$/.test(v)) {
      hint.className = 'hint err';
      hint.textContent = '交易对应为 4–24 位大写字母/数字（如 BTCUSDT）';
    } else {
      hint.className = 'hint';
      hint.textContent = '';
    }
  });

  $('form').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const hint = $('hint');
    hint.className = 'hint';
    hint.textContent = '';
    const sym = $('f-symbol').value.trim().toUpperCase();
    // 交易对格式校验（后端 ValidSymbol: ^[A-Z0-9]{4,24}$），提前在提交前给出提示
    if (!/^[A-Z0-9]{4,24}$/.test(sym)) {
      hint.className = 'hint err';
      hint.textContent = '交易对格式不对：应为 4–24 位大写字母/数字，例如 BTCUSDT';
      return;
    }
    const needTarget = () => {
      const t = $('f-target').value.trim().replace(/,/g, '') || $('f-ntarget').value.trim().replace(/,/g, '');
      if (!t || !(Number(t) > 0)) {
        hint.className = 'hint err';
        hint.textContent = '目标价需要是大于 0 的数字';
        return false;
      }
      return true;
    };
    const body = {
      symbol: sym,
      trigger: type,
      cooldown_min: Math.round(num($('f-cd').value) || 0),
      once: $('f-once').checked,
      note: $('f-note').value.trim(),
    };
    if (type === 'target') {
      if (!needTarget()) return;
      body.dir = dirT;
      body.target = $('f-target').value.trim().replace(/,/g, '');
    } else if (type === 'pct') {
      const p = num($('f-pct').value);
      if (!(p > 0)) { hint.className = 'hint err'; hint.textContent = '幅度需要是大于 0 的百分数，例如 3'; return; }
      body.dir = dirP;
      body.pct_bp = Math.round(p * 100);
      body.window_min = Number($('f-window').value);
    } else {
      if (!needTarget()) return;
      const d = num($('f-near').value);
      if (!(d > 0)) { hint.className = 'hint err'; hint.textContent = '距离需要是大于 0 的百分数，例如 0.5'; return; }
      body.target = $('f-ntarget').value.trim().replace(/,/g, '');
      body.near_bp = Math.round(d * 100);
    }
    const btn = $('btn-add');
    btn.disabled = true;
    try {
      const r = await api('POST', '/api/rules', body);
      toast('已添加 ' + r.symbol + ' · ' + r.desc);
      $('f-target').value = $('f-ntarget').value = $('f-pct').value = $('f-near').value = $('f-note').value = '';
      refresh();
    } catch (e) {
      hint.className = 'hint err';
      hint.textContent = e.message;
    } finally { btn.disabled = false; }
  });

  // ------------------------------------------------------------ 收件人设置
  async function loadMail() {
    try {
      const r = await api('GET', '/api/mail/recipients');
      $('m-to').value = (r.to || []).join('; ');
    } catch (e) {
      $('mail-hint').className = 'hint err';
      $('mail-hint').textContent = '读取收件人失败：' + e.message;
    }
  }

  $('mail-form').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const hint = $('mail-hint');
    hint.className = 'hint';
    hint.textContent = '';
    const btn = $('btn-mail-save');
    btn.disabled = true;
    try {
      const r = await api('POST', '/api/mail/recipients', { to: $('m-to').value.trim() });
      $('m-to').value = (r.to || []).join('; ');
      toast('收件人已保存，下一封邮件即生效');
    } catch (e) {
      hint.className = 'hint err';
      hint.textContent = e.message;
    } finally { btn.disabled = false; }
  });

  $('btn-mail-test').addEventListener('click', async () => {
    const btn = $('btn-mail-test');
    btn.disabled = true;
    try {
      await api('POST', '/api/mail/test', {});
      toast('测试邮件已发送，请查收');
    } catch (e) {
      toast('测试邮件失败：' + e.message, true);
    } finally { btn.disabled = false; }
  });

  refresh();
  loadMail();
  setInterval(refresh, 2000);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(); });
})();
