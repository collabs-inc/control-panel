'use strict';

// ---------- rich replies ----------
// What JARVIS's answers can carry besides markdown: styled tables (markdown ones get numbers aligned and status
// words as pills; ```table JSON adds typed columns: bars, statuses, terminal links, sorting), ```buttons (open a
// terminal chat or a panel page, send JARVIS a reply, follow a link), ```mermaid diagrams, ```chart charts
// (Chart.js: line, bar, pie, doughnut, radar, polarArea, scatter, and venn) and files on the VM it links to
// (cards that download, images inline). The chart and diagram libraries load the first time a reply needs
// them; a drawn one is kept per source, so the chat's re-renders move the same node instead of drawing again.

const RICH = { cache: new Map(), libs: {}, n: 0, mermaidReady: false };
const RICH_COLORS = ['#7aa2ff', '#2dd4bf', '#bc8cff', '#34d399', '#f472b6', '#fbbf24', '#60a5fa', '#fb7185'];

// ---- files: [label](/abs/path), ![alt](/abs/path.png) and Hermes' "MEDIA:/abs/path" lines ----
const RICH_LOCAL = /(!?)\[([^\]\n]*)\]\(((?:~|\/workspace\/home|\/tmp)\/[^)\s]+)\)|^MEDIA:\s*(\S+)\s*$/gm;
const richIsImg = (p) => /\.(png|jpe?g|gif|webp|avif)$/i.test(p);

// richMd(text, reply): md() with ```buttons and ```table drawn, and the VM's files as cards and images. reply: what
// the owner said next, if anything (buttons that sent it show as chosen; the rest of their group is spent).
function richMd(text, reply) {
  const refs = [];
  const blocks = [];
  const marked = String(text ?? '')
    .replace(/<details>\s*<summary>([\s\S]*?)<\/summary>([\s\S]*?)<\/details>/g, (_, sum, inner) => { // Telegram's collapsed blocks
      blocks.push(`<details class="ai-det"><summary>${esc(sum.trim())}</summary><div class="md">${richMd(inner.trim(), reply)}</div></details>`);
      return `\n⟦B${blocks.length - 1}⟧\n`;
    })
    .replace(/^```(buttons|table)[^\n]*\n([\s\S]*?)\n```[ \t]*$/gm, (m, kind, body) => {
      try { blocks.push(kind === 'buttons' ? richButtons(JSON.parse(body), reply) : richTable(JSON.parse(body))); } catch { return m; } // not JSON: shown as code
      return `\n⟦B${blocks.length - 1}⟧\n`;
    })
    .replace(RICH_LOCAL, (m, bang, label, path, media) => {
      const p = (path || media).replace(/^~(?=\/)/, '/workspace/home');
      refs.push({ img: !!bang || (!!media && richIsImg(p)), label, path: p });
      return `⟦F${refs.length - 1}⟧`;
    });
  return md(marked)
    .replace(/(?:<p>)?⟦B(\d+)⟧(?:<\/p>)?/g, (_, i) => blocks[+i])
    .replace(/⟦F(\d+)⟧/g, (_, i) => richFile(refs[+i]));
}

// ---- buttons: [{"label", "send" | "open" | "url", "style": "primary" | "danger", "icon"}], or {"title", "buttons": [...]} ----
// send: the text goes to JARVIS as your reply; open: "pane:<session>:<pane>" (that terminal's chat), a panel page
// (res, ci, term, sec) or "control:<tab>" (memory, skills, tools, mcp, jobs, logs); url: a link.
function richButtons(spec, reply) {
  const list = Array.isArray(spec) ? spec : spec.buttons || [];
  const said = (reply || '').trim();
  const spent = said !== '';
  const html = list.map((b) => {
    const cls = `ai-btn${b.style === 'primary' || b.style === 'danger' ? ' ' + b.style : ''}`;
    const icon = b.icon ? ic(b.icon) : b.open?.startsWith('pane:') ? ic('terminal-square') : b.open ? ic('arrow-right') : b.url ? ic('arrow-up-right') : '';
    const label = esc(b.label || b.send || b.open || b.url || '?');
    if (b.url) return `<a class="${cls}" href="${esc(/^(https?:\/\/|\/hermes\/)/.test(b.url) ? b.url : '#')}" target="_blank" rel="noopener">${label}${icon}</a>`;
    if (b.open) return `<button type="button" class="${cls}" data-aiopen="${esc(b.open)}">${icon}${label}</button>`;
    const chosen = spent && said === String(b.send ?? b.label).trim();
    // "✍️ Other" (labelled so, its send "Other", or nothing to send at all): your own answer, typed: the button opens
    // the box and sends nothing (Telegram has no box: there it sends, and JARVIS waits for the typed text)
    const otherRe = /^\W*other\W*$/i;
    if (otherRe.test(String(b.label ?? '').trim()) || otherRe.test(String(b.send ?? '').trim()) || !String(b.send ?? b.label ?? '').trim()) return `<button type="button" class="${cls}" data-aiother${spent ? ' disabled' : ''}>${icon}${label}</button>`;
    return `<button type="button" class="${cls}${chosen ? ' chosen' : ''}" data-aisend="${esc(b.send ?? b.label)}"${spent ? ' disabled' : ''}>${chosen ? ic('check') : icon}${label}</button>`;
  }).join('');
  return `<div class="ai-btns">${spec.title ? `<div class="ai-btns-t">${esc(spec.title)}</div>` : ''}<div class="ai-btns-r">${html}</div></div>`;
}

// ---- tables: {"title", "columns": [{"key", "label", "type", "align"}], "rows": [{...}] or [[...]]} ----
// types: text (markdown), number, percent, bar (0-100), status (a pill), pane ("box-dev:3" or {pane, label}: opens
// that terminal's chat), date (ISO or epoch: shown as "5m ago"), code, link ({url, label} or a URL)
const RICH_PILL = [[/^(ok|pass(ed)?|success(ful)?|succeeded|done|complete(d)?|healthy|up|online|active|yes|green|merged|✅)$/i, 'ok'],
  [/^(fail(ed|ing)?|error|errored|down|broken|critical|offline|no|red|❌|blocked)$/i, 'bad'],
  [/^(warn(ing)?|pending|waiting|stalled|slow|degraded|paused|queued|asking|review|amber|draft|⚠️)$/i, 'warn'],
  [/^(running|working|in progress|building|deploying|busy|live)$/i, 'busy']];
const richPill = (v) => { const t = String(v ?? '').trim(); const k = RICH_PILL.find(([re]) => re.test(t)); return k ? `<span class="pill ${k[1]}">${esc(t)}</span>` : null; };
const RICH_NUM = /^[-+−]?[$€£]?\s?\d[\d,.\s]*(%|ms|s|m|h|d|k|K|M|B|GB|MB|KB|TB|x|×)?$|^(\d+(\.\d+)?\s?[hms]\s?)+$/;

function richTable(spec) {
  const cols = (spec.columns || []).map((c) => (typeof c === 'string' ? { key: c, label: c } : c));
  const rows = spec.rows || [];
  const val = (r, c, i) => (Array.isArray(r) ? r[i] : r[c.key]);
  const cell = (c, v) => {
    if (v == null || v === '') return ['', '<span class="faint">–</span>', ''];
    switch (c.type) {
      case 'number': return [+v, esc(typeof v === 'number' ? v.toLocaleString() : v), 'num'];
      case 'percent': return [+v, esc(v) + '%', 'num'];
      case 'bar': { const n = Math.max(0, Math.min(100, +v)); return [n, `<div class="ai-bar ${n >= 80 ? 'ok' : n >= 50 ? 'warn' : 'bad'}"><i data-w="${n}"></i></div><span>${esc(v)}%</span>`, 'tbar']; }
      case 'status': return [String(v), richPill(v) || `<span class="pill">${esc(v)}</span>`, ''];
      case 'pane': { const p = typeof v === 'object' ? v : { pane: v }; return [p.label || p.pane, `<button type="button" class="ai-btn sm" data-aiopen="pane:${esc(p.pane)}">${ic('terminal-square')}${esc(p.label || p.pane)}</button>`, '']; }
      case 'date': { const t = typeof v === 'number' ? v : Date.parse(v) / 1000; return [t, `<span title="${esc(new Date(t * 1000).toLocaleString())}">${esc(ago(t))}</span>`, '']; }
      case 'code': return [String(v), `<code>${esc(v)}</code>`, ''];
      case 'link': { const l = typeof v === 'object' ? v : { url: v }; return [l.label || l.url, `<a href="${esc(/^https?:\/\//.test(l.url) ? l.url : '#')}" target="_blank" rel="noopener">${esc(l.label || l.url)}</a>`, '']; }
      default: return [String(v), richPill(v) || mdInline(String(v)), RICH_NUM.test(String(v).trim()) ? 'num' : ''];
    }
  };
  const head = cols.map((c, i) => `<th class="${c.align === 'right' || ['number', 'percent'].includes(c.type) ? 'num' : ''}"><button type="button" data-aisort="${i}">${esc(c.label ?? c.key)}${ic('angle-down')}</button></th>`).join('');
  const body = rows.map((r) => `<tr>${cols.map((c, i) => { const [sv, h, cls] = cell(c, val(r, c, i)); return `<td class="${cls}${c.align === 'right' ? ' num' : ''}" data-v="${esc(sv)}">${h}</td>`; }).join('')}</tr>`).join('');
  return `<figure class="ai-tblw">${spec.title ? `<figcaption>${esc(spec.title)}</figcaption>` : ''}<div class="ai-tbls"><table class="ai-tbl"><thead><tr>${head}</tr></thead><tbody>${body}</tbody></table></div></figure>`;
}

// markdown tables in answers get the same look: numbers aligned right, status words as pills, sortable headers
function richStyleTables(root) {
  for (const t of root.querySelectorAll('.ai-ans table:not(.ai-tbl)')) {
    t.classList.add('ai-tbl', 'md-tbl');
    const wrap = document.createElement('figure');
    wrap.className = 'ai-tblw';
    wrap.innerHTML = '<div class="ai-tbls"></div>';
    t.replaceWith(wrap);
    wrap.firstChild.append(t);
    const ncols = t.rows[0]?.cells.length || 0;
    [...(t.tHead?.rows[0]?.cells || [])].forEach((th, i) => { th.innerHTML = `<button type="button" data-aisort="${i}">${th.innerHTML}${ic('angle-down')}</button>`; }); // sortable too
    for (const td of t.querySelectorAll('tbody td')) td.dataset.v = td.textContent.trim();
    for (let c = 0; c < ncols; c++) {
      const cells = [...t.querySelectorAll(`tbody tr > :nth-child(${c + 1})`)];
      const nums = cells.filter((td) => RICH_NUM.test(td.textContent.trim())).length;
      if (cells.length && nums >= Math.ceil(cells.length * 0.6)) { // a numbers column: right-aligned, header too
        for (const td of cells) td.classList.add('num');
        t.querySelector(`thead tr > :nth-child(${c + 1})`)?.classList.add('num');
      }
      for (const td of cells) { if (!td.querySelector('*')) { const p = richPill(td.textContent); if (p) td.innerHTML = p; } }
    }
  }
  for (const i of root.querySelectorAll('.ai-bar i[data-w]')) i.style.width = i.dataset.w + '%';
}

// a ```table's header sorts its column: first by value, again to reverse
function richSort(th) {
  const t = th.closest('table');
  const i = +th.querySelector('[data-aisort]').dataset.aisort;
  const dir = th.dataset.dir === 'asc' ? 'desc' : 'asc';
  for (const h of t.tHead.rows[0].cells) delete h.dataset.dir;
  th.dataset.dir = dir;
  const rows = [...t.tBodies[0].rows];
  const key = (r) => { const v = r.cells[i]?.dataset.v ?? ''; const n = parseFloat(String(v).replace(/[^\d.\-]/g, '')); return Number.isFinite(n) && /\d/.test(v) ? n : String(v).toLowerCase(); };
  rows.sort((a, b) => { const x = key(a), y = key(b); const c = typeof x === 'number' && typeof y === 'number' ? x - y : String(x).localeCompare(String(y)); return dir === 'asc' ? c : -c; });
  t.tBodies[0].append(...rows);
}

function richFile({ img, label, path }) {
  const url = '/api/ai/file?path=' + encodeURIComponent(path);
  const name = path.split('/').pop();
  if (img) return `<a class="ai-img" href="${url}" target="_blank" rel="noopener"><img src="${url}" alt="${esc(label || name)}" loading="lazy"></a>`;
  const ext = (/\.([a-z0-9]{1,6})$/i.exec(name)?.[1] || 'file').toUpperCase();
  const open = /\.(pdf|txt)$/i.test(name) ? 'target="_blank" rel="noopener"' : `download="${esc(name)}"`;
  return `<a class="ai-file" href="${url}" ${open}><span class="ai-file-x">${esc(ext)}</span>` +
    `<span class="ai-file-n"><b>${esc(label && label !== name ? label : name)}</b><small>${esc(tilde(path))}</small></span>${ic('download2')}</a>`;
}

// ---- diagrams and charts: ```mermaid and ```chart code blocks become figures ----
function richEnhance(root) {
  richStyleTables(root);
  const seen = new Map();
  for (const cb of root.querySelectorAll('.cb')) {
    const kind = cb.querySelector('.cb-l')?.textContent;
    if (kind !== 'mermaid' && kind !== 'chart') continue;
    const src = cb.querySelector('code').textContent;
    const n = (seen.get(src) || 0) + 1; // the same source twice: two figures
    seen.set(src, n);
    const key = `${kind}:${n}:${src}`;
    let fig = RICH.cache.get(key);
    if (!fig) {
      fig = document.createElement('figure');
      fig.className = 'rich rich-' + kind;
      fig.innerHTML = `<div class="rich-body">${shim(kind === 'chart' ? 'Drawing the chart…' : 'Drawing the diagram…')}</div>`;
      RICH.cache.set(key, fig);
      if (RICH.cache.size > 300) RICH.cache.delete(RICH.cache.keys().next().value);
      (kind === 'chart' ? richChart : richMermaid)(fig, src).catch((err) => richFail(fig, kind, src, err));
    }
    cb.replaceWith(fig);
  }
}

function richFail(fig, kind, src, err) {
  fig.className = 'rich rich-fail';
  fig.innerHTML = `<div class="rich-err">${ic('alert-triangle')}<span>Couldn't draw this ${kind === 'chart' ? 'chart' : 'diagram'}: ${esc(String(err?.message || err).split('\n')[0].slice(0, 200))}</span></div>` +
    `<pre class="rich-src"><code>${esc(src)}</code></pre>`;
}

function richLib(name) {
  return (RICH.libs[name] ||= new Promise((ok, fail) => {
    const s = document.createElement('script');
    s.src = document.querySelector(`meta[name="lib-${name}"]`).content; // versioned by the server: cached for good
    s.onload = ok;
    s.onerror = () => { delete RICH.libs[name]; fail(new Error(name + " didn't load")); };
    document.head.append(s);
  }));
}

async function richMermaid(fig, src) {
  await richLib('mermaid');
  if (!RICH.mermaidReady) {
    mermaid.initialize({
      startOnLoad: false, securityLevel: 'strict', theme: 'base', fontFamily: 'system-ui, -apple-system, "Segoe UI", sans-serif',
      themeVariables: {
        darkMode: true, background: 'transparent', fontSize: '14px',
        primaryColor: '#18202c', primaryTextColor: '#e7e9ec', primaryBorderColor: '#4d6fb8',
        secondaryColor: '#1d1830', secondaryTextColor: '#e7e9ec', secondaryBorderColor: '#7a5cc0',
        tertiaryColor: '#12241f', tertiaryTextColor: '#e7e9ec', tertiaryBorderColor: '#2f8f74',
        lineColor: '#6b7585', textColor: '#c9ced6', mainBkg: '#18202c', nodeBorder: '#4d6fb8',
        clusterBkg: 'rgba(122,162,255,.05)', clusterBorder: '#2a3240', edgeLabelBackground: '#121519',
        noteBkgColor: '#1f1a12', noteTextColor: '#e7e9ec', noteBorderColor: '#8a6d2c',
        actorBkg: '#18202c', actorBorder: '#4d6fb8', actorTextColor: '#e7e9ec', signalColor: '#9aa3b0', signalTextColor: '#c9ced6',
        pie1: RICH_COLORS[0], pie2: RICH_COLORS[1], pie3: RICH_COLORS[2], pie4: RICH_COLORS[3], pie5: RICH_COLORS[4], pie6: RICH_COLORS[5],
        pieStrokeColor: '#0a0c0f', pieOuterStrokeColor: '#2a3240', pieTitleTextColor: '#e7e9ec', pieSectionTextColor: '#0a0c0f', pieLegendTextColor: '#c9ced6',
        git0: RICH_COLORS[0], git1: RICH_COLORS[1], git2: RICH_COLORS[2], git3: RICH_COLORS[3],
      },
    });
    RICH.mermaidReady = true;
  }
  const { svg } = await mermaid.render('mmd' + ++RICH.n, src);
  fig.innerHTML = `<div class="rich-body">${svg}</div>`;
}

// ```chart JSON: {"type", "title", "labels", "datasets": [{"label", "data"}]}; venn: "sets" with members
// ([{"label":"A","values":[…]}]) or "areas" with counts ([{"sets":["A","B"],"value":3}]).
async function richChart(fig, src) {
  const spec = JSON.parse(src);
  const type = String(spec.type || 'line').toLowerCase();
  await richLib('chart');
  if (type === 'venn') await richLib('venn');
  const Chart = window.Chart;
  Chart.defaults.font.family = 'system-ui, -apple-system, "Segoe UI", sans-serif';
  Chart.defaults.color = '#9aa3b0';
  fig.innerHTML = (spec.title ? `<figcaption>${esc(spec.title)}</figcaption>` : '') + '<div class="rich-body rich-canvas"><canvas></canvas></div>';
  const canvas = fig.querySelector('canvas');
  const round = ['pie', 'doughnut', 'polarArea'].includes(type);
  if (round || type === 'venn') fig.classList.add('rich-round');
  const tooltip = { backgroundColor: 'rgba(18,21,25,.94)', borderColor: 'rgba(255,255,255,.1)', borderWidth: 1, padding: 10, cornerRadius: 10, titleColor: '#e7e9ec', bodyColor: '#c9ced6', boxPadding: 4 };
  const legend = { position: round ? 'right' : 'bottom', labels: { color: '#c9ced6', usePointStyle: true, pointStyle: 'circle', boxWidth: 8, padding: 14 } };
  if (type === 'venn') {
    const data = spec.sets ? ChartVenn.extractSets(spec.sets, { label: spec.title || '' })
      : { labels: spec.areas.map((a) => a.sets.join(' ∩ ')), datasets: [{ label: spec.title || '', data: spec.areas.map((a) => ({ sets: a.sets, value: a.value })) }] };
    Object.assign(data.datasets[0], { backgroundColor: RICH_COLORS.map((c) => c + '40'), borderColor: RICH_COLORS, borderWidth: 2 });
    new ChartVenn.VennDiagramChart(canvas, { data, options: { maintainAspectRatio: false, animation: { duration: 900 }, plugins: { legend: { display: false }, tooltip } } });
    return;
  }
  const datasets = (spec.datasets || []).map((d, i) => {
    const c = RICH_COLORS[i % RICH_COLORS.length];
    if (round) return { borderColor: '#0a0c0f', borderWidth: 2, hoverOffset: 8, backgroundColor: (d.data || []).map((_, j) => RICH_COLORS[j % RICH_COLORS.length] + 'dd'), ...d };
    if (type === 'line' || type === 'radar' || type === 'scatter') {
      return { borderColor: c, backgroundColor: c + '26', pointBackgroundColor: c, pointRadius: type === 'scatter' ? 4 : 2.5, pointHoverRadius: 5, borderWidth: 2, tension: 0.35, fill: type === 'radar', ...d };
    }
    return { backgroundColor: c + 'cc', hoverBackgroundColor: c, borderRadius: 6, borderSkipped: false, maxBarThickness: 46, ...d };
  });
  const axis = { grid: { color: 'rgba(255,255,255,.05)', drawTicks: false }, border: { display: false }, ticks: { color: '#8b94a1', padding: 8 } };
  new Chart(canvas, {
    type,
    data: { labels: spec.labels, datasets },
    options: {
      maintainAspectRatio: false, responsive: true, animation: { duration: 900, easing: 'easeOutQuart' },
      interaction: { mode: round ? 'nearest' : 'index', intersect: round },
      plugins: { legend: { ...legend, display: datasets.length > 1 || round }, tooltip },
      scales: round ? {} : type === 'radar' ? { r: { grid: { color: 'rgba(255,255,255,.07)' }, angleLines: { color: 'rgba(255,255,255,.07)' }, pointLabels: { color: '#c9ced6' }, ticks: { display: false } } } : { x: axis, y: { ...axis, beginAtZero: type === 'bar' } },
      ...spec.options,
    },
  });
}
