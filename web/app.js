'use strict';

// ---------- helpers ----------
const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];
const store = {
  get(k, d) { try { return localStorage.getItem(k) ?? d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* private mode */ } },
};
const now = () => Date.now() / 1000;

function fmtB(n) {
  if (n == null || isNaN(n)) return '–';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i && n < 10 ? n.toFixed(1) : Math.round(n)) + ' ' + u[i];
}
const fmtRate = (n) => fmtB(n) + '/s';
const fmtPct = (n) => { n = n || 0; return (n >= 10 || n === 0 ? Math.round(n) : n.toFixed(1)) + '%'; };
function fmtDur(s) {
  s = Math.max(0, Math.round(s));
  if (s < 60) return s + 's';
  const m = Math.floor(s / 60);
  if (m < 60) return m + 'm' + (s % 60 ? ' ' + (s % 60) + 's' : '');
  const h = Math.floor(m / 60);
  if (h < 24) return h + 'h' + (m % 60 ? ' ' + (m % 60) + 'm' : '');
  return Math.floor(h / 24) + 'd' + (h % 24 ? ' ' + (h % 24) + 'h' : '');
}
const ago = (t) => (t ? fmtDur(now() - t) + ' ago' : '–');
const tilde = (p) => String(p ?? '').replace(/^\/workspace\/home(?=\/|$)/, '~');
const lvl = (p) => (p >= 90 ? 'bad' : p >= 75 ? 'warn' : '');

// ---------- state ----------
const TITLES = { res: 'Resources', ci: 'CI', term: 'Terminals', sec: 'Security' };
const S = {
  view: TITLES[store.get('view')] ? store.get('view') : store.get('view') === 'agents' ? 'term' : 'res',
  data: {},
  badges: null,
  hist: [],
  sort: store.get('sort', 'cpu'),
  open: new Set(),
  order: [], sortedBy: null, sortedAt: 0, procOrder: new Map(),
  heldAt: 0,
  subsOpen: new Set(),
  limit: {}, // unsaved balancer edits by group key
  openWhenSeen: null,
  es: null,
  fresh: true,
  fails: 0,
};

// ---------- API ----------
async function api(path, body) {
  const opts = body === undefined
    ? { credentials: 'same-origin' }
    : { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) };
  const r = await fetch(path, opts);
  let j = {};
  try { j = await r.json(); } catch { /* non-JSON (e.g. 403 text) */ }
  return { status: r.status, ...j };
}

// fetch() rejecting (TypeError) while online usually means Cloudflare Access redirected us to its
// login page: reload through it. HTTP errors (e.g. 502 while the panel restarts) just retry.
function networkFailed() {
  setConn('down');
  if (navigator.onLine && ++S.fails >= 3) location.reload();
  else setTimeout(recheck, 4000);
}

// ---------- login ----------
function showLogin(msg) {
  closeStream();
  closePane(false); // the drawer sits above #app: never leave pane contents over the PIN screen
  $('#app').hidden = true;
  $('#login').hidden = false;
  $('#login-err').textContent = msg || '';
  $('#pin').value = '';
  $('#pin').focus();
}

$('#login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const btn = $('#login-form button');
  btn.disabled = true;
  try {
    const r = await api('/api/login', { pin: $('#pin').value });
    if (r.status === 200) return startApp();
    $('#pin').value = '';
    $('#login-err').textContent =
      r.status === 429 ? `Too many wrong PINs. Try again in ${fmtDur(r.retryIn)}.` :
      r.status === 401 ? 'Wrong PIN.' : r.error || `Error ${r.status}`;
  } catch { $('#login-err').textContent = 'Network error.'; }
  finally { btn.disabled = false; }
});

$('#logout').addEventListener('click', async () => {
  try { await api('/api/logout', {}); } catch { /* going to the PIN screen anyway */ }
  showLogin();
});

// ---------- stream ----------
function setConn(state) {
  const c = $('#conn');
  c.className = 'conn ' + (state || '');
  c.setAttribute('aria-label', state === 'live' ? 'live' : state === 'down' ? 'reconnecting' : 'connecting');
}

function closeStream() {
  if (S.es) { S.es.close(); S.es = null; }
}

function connect() {
  closeStream();
  S.fresh = true;
  const es = new EventSource('/api/stream?view=' + S.view);
  es.onopen = () => { S.fails = 0; setConn('live'); };
  let got = false;
  es.onmessage = (e) => { got = true; onMessage(JSON.parse(e.data)); };
  setTimeout(() => { if (S.es === es && !got) window.cpReport?.('stall', `no ${S.view} data after 10 s (stream state ${es.readyState})`); }, 10000);
  es.onerror = () => {
    setConn('down');
    if (es.readyState === EventSource.CLOSED) { S.es = null; recheck(); }
  };
  S.es = es;
}

// The stream closed for good (non-200): session expired, server restarted, or Access expired.
async function recheck() {
  try {
    const r = await api('/api/me');
    if (r.status === 200) { S.fails = 0; setTimeout(connect, 1500); }
    else if (r.status === 401) showLogin('Session ended — enter your PIN.');
    else { setConn('down'); setTimeout(recheck, 4000); }
  } catch { networkFailed(); }
}

// ---- updates: a page on an older build reloads itself once it safely can, and reopens what was open ----
let lastInput = 0;
for (const ev of ['keydown', 'pointerdown']) addEventListener(ev, () => { lastInput = Date.now(); }, true);
const safeToReload = () => Date.now() - lastInput > 4000 && !$('#dr-text').value.trim() && !D.files.length && !D.sending &&
  !$('#modal').open && !$('#newdlg').open && !String(getSelection() || '');
function checkBuild(v) {
  if (!v) return;
  if (!S.build) { S.build = v; return; }
  if (v !== S.build && !S.updating) { S.updating = true; tryReload(); }
}
function reloadNow() {
  try { sessionStorage.setItem('cp-reopen', JSON.stringify({ key: D.key, mode: D.mode })); } catch { /* no storage: the list comes back instead */ }
  location.reload();
}
function tryReload() {
  if (safeToReload()) return reloadNow();
  $('#update').hidden = false; // you're busy: offer it, and try again shortly
  setTimeout(tryReload, 5000);
}

function onMessage(m) {
  checkBuild(m.v);
  S.badges = m.badges;
  renderChrome();
  if (m.view !== S.view) return;
  if (m.view === 'res') {
    const h = m.data.hist || [];
    S.hist = S.fresh ? h : S.hist.concat(h).slice(-360);
    S.fresh = false;
    S.data.res = m.data.res;
  } else {
    S.data[m.view] = m.data;
  }
  // Re-rendering mid-tap swaps the element under the finger and the click is lost: wait for the release.
  if (held()) S.stale = true;
  else render();
}

// A finger is down, or a scroll (a phone's fling too) is still moving: re-render once it's over.
const held = () => Date.now() - S.heldAt < 2000 || Date.now() - S.scrollAt < 350;
function flushStale() {
  if (held()) return;
  if (S.stale) { S.stale = false; render(); }
  if (D.stale) { D.stale = false; renderBody(); }
  if (SA.stale) renderSub();
}
S.scrollAt = 0;
addEventListener('pointerdown', () => { S.heldAt = Date.now(); }, true);
for (const ev of ['pointerup', 'pointercancel']) {
  addEventListener(ev, () => { S.heldAt = 0; setTimeout(flushStale, 60); }); // after the click has been dispatched
}
let scrollEnd = 0;
addEventListener('scroll', () => { S.scrollAt = Date.now(); clearTimeout(scrollEnd); scrollEnd = setTimeout(flushStale, 400); }, { capture: true, passive: true });

// WebKit (Safari, and every browser on an iPhone) drops a scroll position to 0 when the contents inside a
// container-query container are replaced. A re-render puts back the positions it was given, before paint.
function keepScroll(els, fn) {
  const at = els.filter(Boolean).map((e) => [e, e === window ? scrollY : e.scrollTop]);
  fn();
  for (const [e, y] of at) {
    if (e === window) { if (scrollY !== y) scrollTo(0, y); } else if (e.scrollTop !== y) e.scrollTop = y;
  }
}

document.addEventListener('visibilitychange', () => {
  if ($('#app').hidden) return;
  if (document.hidden) { // no sampling, no bandwidth while the phone is in a pocket
    closeStream();
    if (D.es) { D.es.close(); D.es = null; }
  } else {
    connect();
    if (D.key && !D.es && !D.ended) connectPane();
  }
});

// ---------- shell ----------
function startApp() {
  try { // after an update: reopen the chat or terminal that was open
    const r = JSON.parse(sessionStorage.getItem('cp-reopen') || 'null');
    sessionStorage.removeItem('cp-reopen');
    if (r?.key) { S.openWhenSeen = r.key; S.reopenMode = r.mode; }
  } catch { /* nothing to reopen */ }
  $('#login').hidden = true;
  $('#app').hidden = false;
  setView(S.view);
}

function setView(v) {
  closePane();
  S.view = v;
  store.set('view', v);
  $('#title').textContent = TITLES[v];
  document.title = TITLES[v] + ' · Control Panel';
  for (const b of $$('.nav-btn')) {
    if (b.dataset.view === v) b.setAttribute('aria-current', 'page');
    else b.removeAttribute('aria-current');
  }
  render();
  connect();
  window.scrollTo(0, 0);
}

for (const b of $$('.nav-btn')) b.addEventListener('click', () => { if (b.dataset.view !== S.view) setView(b.dataset.view); else if (D.key) closePane(); });

// desktop: the sidebar collapses to its icons (remembered)
function setNavMini(on) {
  document.body.classList.toggle('nav-mini', on);
  const t = $('#nav-mini');
  t.setAttribute('aria-label', on ? 'Expand sidebar' : 'Collapse sidebar');
  t.title = t.getAttribute('aria-label');
  t.querySelector('span').textContent = on ? 'Expand' : 'Collapse';
}
setNavMini(store.get('navMini') === '1');
$('#nav-mini').addEventListener('click', () => {
  const on = !document.body.classList.contains('nav-mini');
  setNavMini(on);
  store.set('navMini', on ? '1' : '0');
  setTimeout(() => dispatchEvent(new Event('resize')), 260); // sparklines and the terminal re-fit
});

function badge(name, text, cls) {
  const el = $(`[data-badge="${name}"]`);
  el.textContent = text || '';
  el.className = 'badge ' + (cls || '');
}

function renderChrome() {
  const b = S.badges;
  if (!b) return;
  badge('ci', b.ciFail ? b.ciFail : b.ciBusy ? b.ciBusy : '', b.ciFail ? 'bad' : 'busy');
  badge('term', b.waiting ? b.waiting : b.working ? b.working : '', b.waiting ? 'warn' : 'busy');
  badge('sec', b.secBad || '', 'bad');
  $('#top-stats').innerHTML =
    `<span>CPU <b>${fmtPct(b.cpu)}</b></span><span>RAM <b>${fmtPct(b.mem)}</b></span>` +
    (b.waiting ? `<span class="wt">waiting <b>${b.waiting}</b></span>` : '');
}

// ---------- render ----------
function render() {
  const el = $('#view');
  const d = S.view === 'res' ? S.data.res : S.data[S.view];
  const kept = $$('[data-keep-scroll]', el).map((x) => [x.dataset.keepScroll, x.scrollTop]); // inner lists: don't jump to the top every update
  const dkTop = diskEl.querySelector('.dk-list')?.scrollTop || 0; // read while the explorer is still attached
  keepScroll([window], () => { el.innerHTML = d ? VIEWS[S.view](d) : '<div class="grid">' + '<div class="skel"></div>'.repeat(4) + '</div><div class="skel"></div>'; });
  const slot = $('#disk-slot', el);
  if (slot) { // the explorer moves into the fresh view: keep its scroll where it was
    slot.replaceWith(diskEl);
    const list = diskEl.querySelector('.dk-list');
    if (list) list.scrollTop = dkTop;
    if (!DK.data && !DK.busy) loadDisk();
  }
  for (const [k, top] of kept) { const x = $(`[data-keep-scroll="${k}"]`, el); if (x) x.scrollTop = top; }
  // CSP forbids inline style attributes: sizes go through the CSSOM
  for (const i of $$('[data-w]', el)) i.style.width = Math.min(100, Math.max(0, +i.dataset.w)) + '%';
  for (const i of $$('[data-h]', el)) i.style.height = Math.min(100, Math.max(0, +i.dataset.h)) + '%';
  for (const c of $$('canvas[data-spark]', el)) drawSpark(c);
  applyDrops(el);
  if (S.view === 'term') {
    if (S.openWhenSeen && findPane(...splitKey(S.openWhenSeen))) {
      openPane(S.openWhenSeen);
      S.openWhenSeen = null;
      if (S.reopenMode && S.reopenMode !== D.mode) { D.mode = S.reopenMode; D.view = null; renderDrawer(); connectPane(); }
      S.reopenMode = null;
    }
    renderDrawer();
  }
}

// Live views re-render every second or two, and a rebuilt element restarts its animations: the shimmer,
// spinners, pulsing dots and the pixel loader would jump back to their start each time. Pinning every
// looping animation to the page clock makes a rebuilt element continue exactly where the old one was.
const LOOPS = new Set(['t-shimmer', 'pixel-on', 'spin', 'pulse', 'blink', 'curblink', 'ld-bounce', 'ld-wave', 'ld-star']);
let loopSync = 0;
new MutationObserver(() => {
  if (loopSync) return;
  loopSync = requestAnimationFrame(() => {
    loopSync = 0;
    for (const a of document.getAnimations()) if (LOOPS.has(a.animationName) && a.startTime !== 0) a.startTime = 0;
  });
}).observe(document.body, { childList: true, subtree: true });

// ---------- motion: what opens drops in; a re-render mid-animation resumes it where it was ----------
const OPENED = new Map(); // key → when it was opened
const markOpened = (k) => OPENED.set(k, Date.now());
const dropAttr = (k) => { const t = OPENED.get(k); return t && Date.now() - t < 450 ? ` data-drop="${t}"` : ''; };
function applyDrops(root) {
  const now = Date.now();
  const n = new Map();
  for (const el of root.querySelectorAll('[data-drop]')) { // rows of one opening come in one after another
    const t = +el.dataset.drop;
    const i = n.get(t) || 0;
    n.set(t, i + 1);
    el.style.setProperty('--dd', Math.min(i, 12) * 22 - (now - t) + 'ms');
  }
}

const tile = (label, value, sub, extra = '', cls = '', icon = '') =>
  `<div class="card tile ${cls}"><div class="l">${icon ? ic(icon) : ''}${esc(label)}</div><div class="v">${value}</div><div class="s">${sub}</div>${extra}</div>`;
const bar = (p, cls = '') => `<div class="bar ${cls}"><i data-w="${p}"></i></div>`;
const sec = (title, sub = '', right = '', icon = '') =>
  `<div class="sec-h"><h2>${icon ? ic(icon) : ''}${esc(title)}</h2><span class="muted">${sub}</span><span class="spacer"></span>${right}</div>`;
const caret = ic('angle-right', 'caret');

function drawSpark(c) {
  const key = c.dataset.spark;
  const pts = S.hist;
  const dpr = window.devicePixelRatio || 1;
  const w = c.clientWidth, h = c.clientHeight;
  if (!w || !h) return;
  c.width = w * dpr; c.height = h * dpr;
  const g = c.getContext('2d');
  g.scale(dpr, dpr);
  const series = key === 'net' ? [['rx', '#7aa2ff'], ['tx', '#bc8cff']] : [[key, key === 'mem' ? '#bc8cff' : '#7aa2ff']];
  if (pts.length < 2) return;
  let max = key === 'net' ? 1 : 100;
  if (key === 'net') for (const p of pts) max = Math.max(max, p.rx, p.tx);
  for (const [f, color] of series) {
    g.beginPath();
    pts.forEach((p, i) => {
      const x = (i / (pts.length - 1)) * w;
      const y = h - 2 - (Math.min(p[f], max) / max) * (h - 4);
      i ? g.lineTo(x, y) : g.moveTo(x, y);
    });
    g.strokeStyle = color; g.lineWidth = 1.5; g.lineJoin = 'round'; g.stroke();
    if (series.length === 1) {
      g.lineTo(w, h); g.lineTo(0, h); g.closePath();
      const grad = g.createLinearGradient(0, 0, 0, h);
      grad.addColorStop(0, color + '55'); grad.addColorStop(1, color + '00');
      g.fillStyle = grad; g.fill();
    }
  }
}
window.addEventListener('resize', () => { for (const c of $$('canvas[data-spark]')) drawSpark(c); });

// ---------- Resources ----------
const SORTS = { cpu: (g) => g.cpu, ram: (g) => g.rss, io: (g) => g.rd + g.wr, net: (g) => g.conns };

// ---------- balancer ----------
function limitSummary(l) {
  return [l.cores?.length ? (l.cores.length === 1 ? 'core ' : 'cores ') + l.cores.join(', ') : '',
    l.nice === 10 ? 'low priority' : l.nice === 19 ? 'lowest priority' : '', l.ioIdle ? 'disk idle' : '', l.paused ? 'paused' : ''].filter(Boolean).join(' · ');
}
const ruleOf = (key) => (S.data.res?.limits || []).find((l) => l.key === key);

function limitDraft(key, ncpu) {
  if (!S.limit[key]) {
    const r = ruleOf(key);
    S.limit[key] = { cores: new Set(r?.cores?.length ? r.cores : [...Array(ncpu).keys()]), nice: r?.nice || 0, io: !!r?.ioIdle };
  }
  return S.limit[key];
}

function limitPanel(g, ncpu) {
  const k = esc(g.key);
  const d = limitDraft(g.key, ncpu);
  const rule = ruleOf(g.key);
  const opt = (attr, v, label, on) => `<button type="button" data-${attr}="${v}" data-lkey="${k}" aria-pressed="${on}">${label}</button>`;
  return `<div class="limit">
    <div class="lrow"><span class="lk">Cores</span><div class="corepick">${[...Array(ncpu).keys()].map((i) =>
      `<button type="button" class="cpick" data-core="${i}" data-lkey="${k}" aria-pressed="${d.cores.has(i)}">${i}</button>`).join('')}</div>
      <span class="faint">${d.cores.size} of ${ncpu}</span></div>
    <div class="lrow"><span class="lk">Priority</span><div class="seg">${opt('nice', 0, 'Normal', d.nice === 0)}${opt('nice', 10, 'Low', d.nice === 10)}${opt('nice', 19, 'Lowest', d.nice === 19)}</div></div>
    <div class="lrow"><span class="lk">Disk</span><div class="seg">${opt('io', 0, 'Normal', !d.io)}${opt('io', 1, 'Idle', d.io)}</div></div>
    <div class="lrow lact"><button type="button" class="btn sm primary" data-lapply="${k}">${ic('tuning')}Apply limit</button>
      ${rule ? `<button type="button" class="btn sm" data-lremove="${k}">${ic('xmark')}Remove limit</button>` : ''}<span class="spacer"></span>
      <button type="button" class="btn sm${rule?.paused ? ' primary' : ' danger'}" data-lpause="${k}">${rule?.paused ? ic('play3') + 'Resume' : ic('pause3') + 'Pause'}</button></div>
    <div class="note">${rule ? 'Active: ' + esc(limitSummary(rule)) + '. ' : ''}Priority only goes down: raising it back takes a restart of those processes. RAM can't be capped here (no cgroups for this user).</div>
  </div>`;
}

async function sendLimit(body, done) {
  try {
    const r = await api('/api/limit', body);
    if (r.status === 200) { delete S.limit[body.key]; toast(done); }
    else toast(r.error || `Error ${r.status}`, true);
  } catch { toast('Network error.', true); }
}

function limitClick(e) {
  const t = e.target.closest('[data-core], [data-nice], [data-io], [data-lapply], [data-lremove], [data-lpause]');
  if (!t) return false;
  const key = t.dataset.lkey || t.dataset.lapply || t.dataset.lremove || t.dataset.lpause;
  const g = (S.data.res?.groups || []).find((x) => x.key === key);
  const ncpu = (S.data.res?.cores || []).length;
  const label = g?.label || ruleOf(key)?.label || key;
  const rule = ruleOf(key);
  if (t.dataset.core !== undefined) { const d = limitDraft(key, ncpu); const c = +t.dataset.core; d.cores.has(c) ? d.cores.delete(c) : d.cores.add(c); if (!d.cores.size) d.cores.add(c); }
  else if (t.dataset.nice !== undefined) limitDraft(key, ncpu).nice = +t.dataset.nice;
  else if (t.dataset.io !== undefined) limitDraft(key, ncpu).io = t.dataset.io === '1';
  else if (t.dataset.lremove !== undefined) sendLimit({ key, remove: true }, `Limit removed from ${label}`);
  else {
    const d = limitDraft(key, ncpu);
    const body = t.dataset.lpause !== undefined
      ? { key, label, cores: rule?.cores || [], nice: rule?.nice || 0, ioIdle: !!rule?.ioIdle, paused: !rule?.paused }
      : { key, label, cores: d.cores.size === ncpu ? [] : [...d.cores].sort((a, b) => a - b), nice: d.nice, ioIdle: d.io, paused: !!rule?.paused };
    const empty = !body.cores.length && !body.nice && !body.ioIdle && !body.paused;
    if (empty && rule) sendLimit({ key, remove: true }, `${label} resumed, no limits left`);
    else if (empty) toast('Pick fewer cores, a lower priority or idle disk first.', true);
    else sendLimit(body, t.dataset.lpause !== undefined ? `${label} ${body.paused ? 'paused' : 'resumed'}` : `Limit applied to ${label}`);
  }
  render();
  return true;
}

function viewRes(r) {
  const cores = r.cores || [];
  const memUsed = r.memTotal - r.memAvail;
  const memPct = r.memTotal ? (memUsed / r.memTotal) * 100 : 0;
  const swapUsed = r.swapTotal - r.swapFree;
  const swapPct = r.swapTotal ? (swapUsed / r.swapTotal) * 100 : 0;
  const tiles = '<div class="grid">' +
    tile('CPU', fmtPct(r.cpu), `${cores.length} cores · load ${r.load[0].toFixed(2)}`, '<canvas class="spark" data-spark="cpu"></canvas>', lvl(r.cpu), 'cpu') +
    tile('Memory', fmtPct(memPct), `${fmtB(memUsed)} of ${fmtB(r.memTotal)}`, '<canvas class="spark" data-spark="mem"></canvas>', lvl(memPct), 'ram') +
    tile('Network', `<span class="n">↓ ${fmtRate(r.netRx)}</span>`, `↑ ${fmtRate(r.netTx)} · all interfaces`, '<canvas class="spark" data-spark="net"></canvas>', '', 'global') +
    tile('Load', r.load[0].toFixed(2), `${r.load[1].toFixed(2)} · ${r.load[2].toFixed(2)} · up ${fmtDur(r.uptime)}`,
      `<div class="note">${r.nprocs} processes</div>`, r.load[0] > cores.length ? 'warn' : '', 'gauge') +
    '</div>';

  const disks = (r.disks || []).map((d) => {
    const p = d.size ? (d.used / d.size) * 100 : 0;
    return `<div class="meter"><div class="row"><span>${esc(d.mount)}</span><span class="muted n">${fmtB(d.used)} / ${fmtB(d.size)} · ${fmtPct(p)}</span></div>${bar(p, lvl(p))}</div>`;
  }).join('') + (r.swapTotal ? `<div class="meter"><div class="row"><span>swap</span><span class="muted n">${fmtB(swapUsed)} / ${fmtB(r.swapTotal)}</span></div>${bar(swapPct, lvl(swapPct))}</div>` : '');

  const coreGrid = cores.map((c, i) =>
    `<div class="core" title="core ${i}: ${fmtPct(c)}"><i data-h="${c}"></i><span>${Math.round(c)}</span></div>`).join('');

  // Rows only move on a sort change, or every 10 s while nothing is expanded, so they stay under your finger.
  let groups = r.groups || [];
  if (S.sortedBy !== S.sort || (!S.open.size && now() - S.sortedAt >= 10)) {
    groups = [...groups].sort((a, b) => SORTS[S.sort](b) - SORTS[S.sort](a));
    S.sortedBy = S.sort;
    S.sortedAt = now();
  } else groups = keepOrder(S.order, groups, (g) => g.key);
  S.order = groups.map((g) => g.key);
  const maxCPU = Math.max(1, cores.length) * 100;
  const chips = '<div class="chips">' + Object.keys(SORTS).map((k) =>
    `<button class="chip" data-sort="${k}" aria-pressed="${S.sort === k}">${k === 'ram' ? 'RAM' : k === 'io' ? 'Disk I/O' : k === 'net' ? 'Conns' : 'CPU'}</button>`).join('') + '</div>';
  const rows = groups.map((g) => {
    const open = S.open.has(g.key);
    let out = `<div class="prow" role="button" tabindex="0" data-expand="${esc(g.key)}" aria-expanded="${open}">
      <div class="pname">${caret}<b title="${esc(g.label)}">${esc(g.label)}</b>${g.n > 1 ? `<span class="cnt">×${g.n}</span>` : ''}${ruleOf(g.key) ? `<span class="lim${ruleOf(g.key).paused ? ' paused' : ''}">${ruleOf(g.key).paused ? 'paused' : 'limited'}</span>` : ''}</div>
      <div class="r">${fmtPct(g.cpu)}</div><div class="r">${fmtB(g.rss)}</div>
      <div class="r hide-sm">${fmtRate(g.rd + g.wr)}</div><div class="r hide-sm">${g.conns || ''}</div>
      <i class="load" data-w="${(g.cpu / maxCPU) * 100}"></i></div>`;
    if (open) {
      const procs = keepOrder(S.procOrder.get(g.key) || [], g.procs, (p) => p.pid);
      S.procOrder.set(g.key, procs.map((p) => p.pid));
      out += procs.map((p) => `<div class="psub"${dropAttr('p:' + g.key)}>
        <div class="cmd"><span class="pid mono">${p.pid}</span><span class="mono ellipsis" title="${esc(p.cmd)}">${esc(p.cmd)}</span></div>
        <div class="r">${fmtPct(p.cpu)}</div><div class="r">${fmtB(p.rss)}</div>
        <div class="r hide-sm">${fmtRate(p.rd + p.wr)}</div>
        <div class="r"><button class="btn sm danger" data-act="kill" data-pid="${p.pid}" data-cmd="${esc(p.cmd)}">${ic('power')}Kill</button></div></div>`).join('');
      if (g.n > g.procs.length) out += `<div class="psub"${dropAttr('p:' + g.key)}><div class="faint">+ ${g.n - g.procs.length} more (lower CPU)</div></div>`;
      out += limitPanel(g, cores.length).replace('<div class="limit">', `<div class="limit"${dropAttr('p:' + g.key)}>`);
    }
    return out;
  }).join('');

  const limits = r.limits || [];
  const limitCard = limits.length ? `<div>${sec('Limits', `${limits.length} active · re-applied every 10 s`, '', 'tuning')}<div class="card flush"><div class="list">${limits.map((l) =>
    `<div class="li"><div class="main"><div class="t">${esc(l.label)}</div><div class="d">${esc(limitSummary(l))}</div></div>
     <button type="button" class="btn sm" data-lremove="${esc(l.key)}">${ic('xmark')}Remove</button></div>`).join('')}</div></div></div>` : '';
  return tiles + limitCard +
    `<div class="cols stretch"><div>${sec('Storage', '', '', 'hard-drive')}<div class="card">${disks}</div></div>` +
    `<div>${sec('Cores', `${cores.length} × logical`, '', 'microchip')}<div class="card fill"><div class="cores">${coreGrid}</div></div></div></div>` +
    `<div class="res-split"><div>${sec('Processes', 'grouped by app, agent pane or CI runner', chips, 'activity')}
      <div class="card flush"><div class="ptable"><div class="phead"><div>App</div><div class="r">CPU</div><div class="r">RAM</div><div class="r hide-sm">Disk I/O</div><div class="r hide-sm">Conns</div></div>${rows}</div></div>
      <div class="note">CPU is % of one core. Per-process bandwidth needs root, so network shows open connections instead.</div></div>` +
    '<div id="disk-slot"></div></div>';
}
// ---------- disk usage: a CleanMyMac-style explorer over the server's cached du index ----------
// It lives in one element that survives the Resources view's re-renders, so hover, scroll and the
// folder you're in stay put while the numbers above it tick.
const DK = { path: '', data: null, busy: false, err: '', hot: null, timer: 0, gone: null };
const diskEl = document.createElement('div');
diskEl.className = 'disk-wrap';
const DCOL = ['#7aa2ff', '#bc8cff', '#4fd1c5', '#f6ad55', '#f472b6', '#facc15', '#68d391', '#63b3ed', '#fc8181', '#b794f4'];
const dcol = (i, it) => (it?.small ? '#2a303a' : i < DCOL.length ? DCOL[i] : '#4a5160');
const dpath = (name) => (DK.data.path === '/' ? '/' : DK.data.path + '/') + name;

async function loadDisk(path = DK.path, rescan = false) {
  clearTimeout(DK.timer);
  DK.busy = true;
  if (!DK.data) renderDisk();
  try {
    const r = await api(`/api/disk?path=${encodeURIComponent(path || '')}${rescan ? '&rescan=1' : ''}`);
    if (r.status === 200) {
      if (DK.gone && !r.items.some((i) => i.deleting && r.path + '/' + i.name === DK.gone.path)) { toast(`Deleted ${DK.gone.name}`); DK.gone = null; }
      Object.assign(DK, { data: r, path: r.path, err: '' });
    } else DK.err = r.error || `Error ${r.status}`;
  } catch { DK.err = 'Network error.'; }
  DK.busy = false;
  renderDisk();
  // follow a deletion or a scan to its end
  if (DK.data && (DK.data.items.some((i) => i.deleting) || DK.data.scanning)) DK.timer = setTimeout(() => { if (S.view === 'res' && !document.hidden) loadDisk(); }, DK.data.scanning ? 4000 : 1500);
}

function arc(r0, r1, a0, a1) {
  const C = 120;
  a1 = Math.min(a1, a0 + 2 * Math.PI - 1e-4); // a full ring still needs two distinct ends
  const pt = (r, a) => `${(C + r * Math.cos(a)).toFixed(2)} ${(C + r * Math.sin(a)).toFixed(2)}`;
  const large = a1 - a0 > Math.PI ? 1 : 0;
  return `M${pt(r1, a0)}A${r1} ${r1} 0 ${large} 1 ${pt(r1, a1)}L${pt(r0, a1)}A${r0} ${r0} 0 ${large} 0 ${pt(r0, a0)}Z`;
}

// sunburst: the folder's items on the inner ring, each big folder's own largest folders on the outer one
function diskChart(d) {
  const total = Math.max(d.bytes, 1);
  const pad = (a0, a1) => (a1 - a0 > 0.03 ? [a0 + 0.006, a1 - 0.006] : [a0, a1]);
  let a = -Math.PI / 2;
  let segs = '';
  d.items.forEach((it, i) => {
    const span = (it.bytes / total) * 2 * Math.PI;
    if (span < 0.004) { a += span; return; }
    const [s0, s1] = pad(a, a + span);
    segs += `<path d="${arc(58, 88, s0, s1)}" fill="${dcol(i, it)}" data-seg="${i}"/>`;
    let k = a;
    (it.kids || []).forEach((kid, j) => {
      const ks = (kid.bytes / total) * 2 * Math.PI;
      if (ks >= 0.01) {
        const [k0, k1] = pad(k, k + ks);
        segs += `<path d="${arc(91, 116, k0, k1)}" fill="${dcol(i, it)}" fill-opacity="${j % 2 ? 0.42 : 0.6}" data-seg="${i}" data-kid="${j}"/>`;
      }
      k += ks;
    });
    a += span;
  });
  if (d.other > 0 && (d.other / total) * 2 * Math.PI >= 0.004) segs += `<path d="${arc(58, 88, ...pad(a, a + (d.other / total) * 2 * Math.PI))}" fill="#2a303a" data-seg="other"/>`;
  const name = d.path === d.root ? '~' : d.path.split('/').pop();
  return `<svg class="dk-svg" viewBox="0 0 240 240" role="img" aria-label="Disk usage of ${esc(name)}">${segs}` +
    `<text x="120" y="114" text-anchor="middle" class="dk-c1">${esc(name.length > 18 ? name.slice(0, 17) + '…' : name)}</text>` +
    `<text x="120" y="136" text-anchor="middle" class="dk-c2">${esc(fmtB(d.bytes))}</text></svg>`;
}

function renderDisk() {
  const d = DK.data;
  const head = sec('Disk usage', d ? `${esc(tilde(d.root))} · ${d.scanning ? shim('scanning…') : d.at ? 'scanned ' + ago(d.at) : 'never scanned'}` : '',
    `<button type="button" class="btn sm" data-drescan${d?.scanning ? ' disabled' : ''}>${ic('arrow-rotate')}Rescan</button>`, 'hard-drive');
  if (!d) {
    diskEl.innerHTML = head + `<div class="card empty">${DK.err ? `<span class="err">${esc(DK.err)}</span>` : shim('Reading disk usage…')}</div>`;
    return;
  }
  const rel = d.path === d.root ? [] : d.path.slice(d.root.length + 1).split('/');
  const crumbs = `<button type="button" class="dk-cr" data-dgo="${esc(d.root)}">${ic('folder')}~</button>` +
    rel.map((seg, i) => `<span class="dk-sep">/</span><button type="button" class="dk-cr" data-dgo="${esc(d.root + '/' + rel.slice(0, i + 1).join('/'))}">${esc(seg)}</button>`).join('');
  const max = Math.max(1, ...d.items.map((i) => i.bytes), d.other);
  const row = (it, i) => `<div class="dk-row${it.dir ? ' dir' : ''}${it.deleting ? ' gone' : ''}" data-i="${i}"${it.dir && !it.deleting ? ` data-dgo="${esc(dpath(it.name))}" role="button" tabindex="0"` : ''}>` +
    `<i class="dk-dot" data-c="${dcol(i, it)}"></i>${it.dir ? ic('folder', 'dk-ic') : it.link ? ic('arrow-up-right', 'dk-ic') : fxBadge(it.name)}` +
    `<span class="dk-n" title="${esc(it.name)}">${it.deleting ? shim('Deleting ' + it.name + '…') : esc(it.name)}</span>` +
    `<span class="dk-bar"><i data-w="${(it.bytes / max) * 100}" data-c="${dcol(i, it)}"></i></span>` +
    `<span class="dk-s">${it.small ? '&lt; 1 MB' : fmtB(it.bytes)}</span><span class="dk-p">${it.small ? '' : fmtPct((it.bytes / Math.max(d.bytes, 1)) * 100)}</span>` +
    (it.deleting ? '<span class="dk-del"></span>' : `<button type="button" class="icon-btn sm dk-del" data-ddel="${i}" aria-label="Delete ${esc(it.name)}" title="Delete">${ic('trash2')}</button>`) + '</div>';
  const other = d.other > 0 || d.otherN > 0 ? `<div class="dk-row other" data-i="other"><i class="dk-dot" data-c="#2a303a"></i>${ic('layers', 'dk-ic')}<span class="dk-n">${d.otherN ? `${plural(d.otherN, 'smaller item')}` : 'Folder entries, small files'}</span>` +
    `<span class="dk-bar"><i data-w="${(d.other / max) * 100}" data-c="#2a303a"></i></span><span class="dk-s">${fmtB(d.other)}</span><span class="dk-p">${fmtPct((d.other / Math.max(d.bytes, 1)) * 100)}</span><span class="dk-del"></span></div>` : '';
  const entering = DK.shown !== d.path; // a new folder: the chart turns in, the rows follow
  DK.shown = d.path;
  diskEl.innerHTML = head + `<div class="card disk${DK.busy ? ' busy' : ''}${entering ? ' enter' : ''}"><div class="dk-chart">${diskChart(d)}</div><div class="dk-side">` +
    `<div class="dk-crumbs">${crumbs}${d.live ? '<span class="faint dk-live">measured now</span>' : ''}</div>` +
    `<div class="dk-list">${d.items.map(row).join('') + other || '<div class="empty">Empty folder</div>'}</div></div></div>`;
  for (const i of diskEl.querySelectorAll('[data-w]')) i.style.width = Math.min(100, Math.max(0.5, +i.dataset.w)) + '%';
  for (const i of diskEl.querySelectorAll('[data-c]')) i.style.background = i.dataset.c;
  if (entering) {
    diskEl.querySelectorAll('.dk-row').forEach((r, i) => r.style.setProperty('--dd', Math.min(i, 14) * 18 + 'ms'));
    // once: the explorer is re-attached on every refresh of the page, which would replay it
    const card = diskEl.querySelector('.disk');
    setTimeout(() => card?.classList.remove('enter'), 900);
  }
}

// hovering a segment or a row lights up its pair and names it in the middle of the chart
function diskHot(i, kid) {
  const d = DK.data;
  if (!d) return;
  const svg = diskEl.querySelector('.dk-svg');
  svg?.classList.toggle('hovering', i !== null);
  for (const p of diskEl.querySelectorAll('[data-seg]')) p.classList.toggle('hot', p.dataset.seg === String(i) && (kid === undefined || p.dataset.kid === String(kid)));
  for (const r of diskEl.querySelectorAll('.dk-row')) r.classList.toggle('hot', r.dataset.i === String(i));
  const it = i === 'other' ? { name: 'Smaller items', bytes: d.other } : d.items[i];
  const sub = it && kid !== undefined ? it.kids[kid] : null;
  const name = sub ? sub.name : it ? it.name : d.path === d.root ? '~' : d.path.split('/').pop();
  const bytes = sub ? sub.bytes : it ? it.bytes : d.bytes;
  const c1 = diskEl.querySelector('.dk-c1');
  const c2 = diskEl.querySelector('.dk-c2');
  if (c1) c1.textContent = name.length > 18 ? name.slice(0, 17) + '…' : name;
  if (c2) c2.textContent = it || sub ? `${fmtB(bytes)} · ${fmtPct((bytes / Math.max(d.bytes, 1)) * 100)}` : fmtB(d.bytes);
}
diskEl.addEventListener('pointerover', (e) => {
  const s = e.target.closest('[data-seg]');
  if (s) return diskHot(s.dataset.seg === 'other' ? 'other' : +s.dataset.seg, s.dataset.kid === undefined ? undefined : +s.dataset.kid);
  const r = e.target.closest('.dk-row');
  diskHot(r ? (r.dataset.i === 'other' ? 'other' : +r.dataset.i) : null);
});
diskEl.addEventListener('pointerleave', () => diskHot(null));
diskEl.addEventListener('click', (e) => {
  const d = DK.data;
  if (e.target.closest('[data-drescan]')) { toast('Rescanning your home folder (a few seconds of disk reads)'); return loadDisk(DK.path, true); }
  const del = e.target.closest('[data-ddel]');
  if (del) return askDelete(d.items[+del.dataset.ddel]);
  const seg = e.target.closest('[data-seg]');
  if (seg && seg.dataset.seg !== 'other') {
    const it = d.items[+seg.dataset.seg];
    if (it?.dir) return loadDisk(dpath(it.name) + (seg.dataset.kid !== undefined ? '/' + it.kids[+seg.dataset.kid].name : ''));
    return;
  }
  const go = e.target.closest('[data-dgo]');
  if (go) loadDisk(go.dataset.dgo);
});
diskEl.addEventListener('keydown', (e) => { if (e.key === 'Enter' && e.target.matches('[data-dgo]')) loadDisk(e.target.dataset.dgo); });

async function askDelete(it) {
  if (!it) return;
  const path = dpath(it.name);
  const c = await api('/api/disk/check?path=' + encodeURIComponent(path));
  if (c.status !== 200) return toast(c.error || `Error ${c.status}`, true);
  const agents = (S.data.term?.sessions || []).flatMap((s) => (s.tabs || []).flatMap((t) => t.panes))
    .filter((p) => [p.agent?.cwd, p.git?.root].some((x) => x && (x === path || x.startsWith(path + '/'))))
    .map((p) => p.agent?.title || p.title || 'pane ' + p.id);
  let msg = `${tilde(path)}\n${fmtB(c.bytes)} · ${it.dir ? 'the folder and everything in it' : it.link ? 'the link only (not what it points to)' : 'file'}.\n\nDeleted for good: there is no trash.`;
  if (agents.length) msg += `\n\nTerminals working in it: ${agents.slice(0, 4).join(', ')}${agents.length > 4 ? '…' : ''}.`;
  if (c.nUsers) msg += `\n\nIn use by ${c.users.join(', ')}${c.nUsers > c.users.length ? ` and ${c.nUsers - c.users.length} more` : ''}: open files only free their space once those exit.`;
  if (/\/ci-cd-jobs\//.test(path)) msg += '\n\nA CI job running in it will fail.';
  modalIcon('trash2');
  $('#modal-title').textContent = `Delete ${it.name}?`;
  $('#modal-msg').textContent = msg;
  $('#modal-ok').textContent = 'Delete';
  $('#modal-ok').className = 'btn danger';
  $('#modal-err').textContent = '';
  pending = { url: '/api/disk/delete', body: { path }, done: `Deleting ${it.name}…`, after: () => { DK.gone = { path, name: it.name }; loadDisk(); } };
  $('#modal').showModal();
}


// ---------- CI ----------
const RESULT_PILL = { Succeeded: 'ok', Failed: 'bad', Canceled: 'warn', Interrupted: 'warn', Running: 'busy' };
const RESULT_IC = { Succeeded: 'check-circle-f', Failed: 'x-circle-f', Canceled: 'alert-triangle', Interrupted: 'alert-triangle' };
const gh = (m, path) => `https://github.com/${esc(m.repo)}/${path}`;
const ext = (href, text, cls = '') => `<a class="${cls}" href="${href}" target="_blank" rel="noopener noreferrer">${text}</a>`;

function prCell(m) {
  if (!m) return '<span class="faint">–</span>';
  if (m.pr) return ext(gh(m, 'pull/' + m.pr), ic('hierarchy') + '#' + m.pr, 'pr') + (m.title ? `<span class="ptitle">${esc(m.title)}</span>` : '');
  return `<span class="muted">${esc(m.event === 'workflow_dispatch' ? 'manual run' : m.event === 'schedule' ? 'scheduled' : m.event || '')}</span>`;
}
const branchCell = (m) => m?.branch ? `<span class="mono">${ic('hierarchy3')}${esc(m.branch)}</span>${m.base ? `<span class="faint"> → ${esc(m.base)}</span>` : ''}` : '<span class="faint">–</span>';

function viewCI(c) {
  const t = (x) => x || { jobs: 0, failed: 0, minutes: 0, success: 0 };
  const day = t(c.day), week = t(c.week), all = t(c.all);
  const tiles = '<div class="grid">' +
    tile('Jobs · 24 h', day.jobs, `${week.jobs} in 7 d · ${all.jobs} total`, '', '', 'layers') +
    tile('Runner time · 24 h', fmtDur(day.minutes * 60), `${fmtDur(week.minutes * 60)} in 7 d`, '', '', 'stopwatch') +
    tile('Success · 24 h', day.jobs ? fmtPct(day.success) : '–', `${week.jobs ? fmtPct(week.success) : '–'} over 7 d`, '', day.success < 90 && day.jobs ? 'warn' : 'ok', 'check-circle') +
    tile('Failed · 24 h', day.failed, `${week.failed} in 7 d`, '', day.failed ? 'bad' : '', 'x-circle') +
    '</div>';
  const runners = c.runners || [];
  const busyRunners = runners.filter((r) => r.state === 'busy');
  const busy = busyRunners.length;
  const dead = runners.filter((r) => r.state === 'dead').length;
  const cards = runners.map((r) => {
    const m = r.meta;
    const pill = r.state === 'busy' ? 'busy' : r.state === 'dead' ? 'bad' : '';
    const job = r.state === 'busy' ? shim(r.job) : r.state === 'dead' ? 'Process not running' : '<span class="faint">Waiting for jobs</span>';
    const where = r.state === 'busy' && m ? `<div class="rwhere">${m.pr ? `<span class="pr">#${m.pr}</span>` : ''}${m.branch ? `<span class="mono">${esc(m.branch)}</span>` : ''}</div>` : '';
    return `<div class="card runner ${esc(r.state)}">
      <div class="head"><b>${ic('server')}${esc(r.name)}</b><span class="pill ${pill}">${esc(r.state)}</span></div>
      <div class="job">${job}</div>${where}
      <div class="foot"><span>${r.state === 'busy' ? fmtDur(now() - r.since) : r.pid ? 'pid ' + r.pid : ''}</span>
      <button class="icon-btn sm" data-act="restart" data-runner="${esc(r.name)}" data-busy="${r.state === 'busy' ? esc(r.job) : ''}" aria-label="Restart runner ${esc(r.name)}" title="Restart runner ${esc(r.name)}">${ic('arrow-rotate')}</button></div></div>`;
  }).join('');
  const rows = busyRunners.map((r) => {
    const m = r.meta;
    const job = `${m?.workflow ? `<span class="faint">${esc(m.workflow)} · </span>` : ''}${m?.runId ? ext(gh(m, 'actions/runs/' + encodeURIComponent(m.runId)), esc(r.job)) : esc(r.job)}`;
    return `<div class="rrow busy">
      <div class="rn"><b>${esc(r.name)}</b></div>
      <div class="rpr">${prCell(m)}</div>
      <div class="rbr">${branchCell(m)}</div>
      <div class="rjob">${job}</div>
      <div class="rt num">${fmtDur(now() - r.since)}</div></div>`;
  }).join('');
  const types = (c.byWorkflow || []).map((w) => `<div class="wf-h"><b>${esc(w.name)}</b><span class="muted">${plural(w.count, 'run')}</span><span class="spacer"></span><span class="${w.success < 90 ? 'warn' : 'okc'}">${fmtPct(w.success)} ok</span></div>` +
    (w.jobs || []).map((n) => `<div class="li"><div class="main"><div class="t">${esc(n.name)}</div><div class="d">avg ${fmtDur(n.avgSec)}</div></div>
      <div class="side"><div>${plural(n.count, 'run')}</div><div class="${n.success < 90 ? 'warn' : ''}">${fmtPct(n.success)} ok</div></div></div>`).join('')).join('');
  const recent = (c.recent || []).map((j) => {
    const m = j.meta;
    const where = [m?.pr ? '#' + m.pr : m?.branch, j.runner, ago(j.end || j.start)].filter(Boolean).map(esc).join(' · ');
    const dur = j.result === 'Interrupted' ? 'interrupted' : fmtDur(j.end - j.start);
    const href = m?.repo && (m.pr ? gh(m, 'pull/' + m.pr) : m.runId ? gh(m, 'actions/runs/' + encodeURIComponent(m.runId)) : '');
    const tag = href ? `a class="li" href="${href}" target="_blank" rel="noopener noreferrer" title="${m.pr ? 'Open PR #' + m.pr : 'Open the run'} on GitHub"` : 'div class="li"';
    return `<${tag}>${RESULT_IC[j.result] ? `<span class="res-ic ${esc(j.result)}" title="${esc(j.result)}">${ic(RESULT_IC[j.result])}</span>` : `<span class="res-dot ${esc(j.result)}" title="${esc(j.result)}"></span>`}
      <div class="main"><div class="t">${esc(j.name)}</div><div class="d">${m?.workflow ? esc(m.workflow) + ' · ' : ''}${where}</div></div>
      <div class="side"><span class="pill ${RESULT_PILL[j.result] || ''}">${j.result === 'Succeeded' ? dur : esc(j.result.toLowerCase()) + (j.result === 'Interrupted' ? '' : ' · ' + dur)}</span></div></${href ? 'a' : 'div'}>`;
  }).join('');
  return (c.err ? `<div class="card err">${esc(c.err)}</div>` : '') + tiles +
    `<div>${sec('Runners', `${busy} busy · ${runners.length - busy - dead} idle${dead ? ` · <span class="err">${dead} dead</span>` : ''}`, '', 'server')}<div class="runners">${cards || '<div class="card empty">No runners found</div>'}</div></div>` +
    `<div>${sec('Active jobs', busy ? `${busy} running now` : 'nothing running', '', 'play-circle3')}
      ${busy ? `<div class="card flush"><div class="rtable"><div class="rrow rhead"><div>Runner</div><div>Pull request</div><div>Branch</div><div>Job</div><div class="rt">Running</div></div>${rows}</div></div>` : '<div class="card empty">No CI jobs are running right now.</div>'}</div>` +
    `<div class="cols"><div>${sec('Job types', 'by workflow · all time', '', 'layers')}<div class="card flush scroll" data-keep-scroll="ci-types">${types || '<div class="empty">No jobs yet</div>'}</div></div>` +
    `<div>${sec('Recent jobs', `${(c.recent || []).length} completed`, '', 'history')}<div class="card flush scroll" data-keep-scroll="ci-recent"><div class="list">${recent || '<div class="empty">No jobs yet</div>'}</div></div></div></div>`;
}

// ---------- Terminals ----------
const TSTATE = {
  asking: ['warn', 'Needs answer'], waiting: ['wait', 'Waiting for you'], working: ['busy', 'Working'], background: ['busy', 'Background'],
  stalled: ['bad', 'Stalled?'], idle: ['', 'Idle'], running: ['tool', 'Running'], stopped: ['warn', 'Stopped'],
  stuck: ['bad', 'Stuck'], exited: ['', 'Exited'], unknown: ['', 'Unknown'], starting: ['', 'Starting'],
};
const plural = (n, w) => `${n} ${w}${n === 1 ? '' : 's'}`;
const splitKey = (k) => { const i = k.lastIndexOf(':'); return [k.slice(0, i), +k.slice(i + 1)]; };

function paneDetail(p) {
  const out = p.output ? `output ${ago(p.output)}` : '';
  const q = p.agent?.prompt?.q;
  switch (p.state) {
    case 'asking': return [p.detail, q].filter(Boolean).join(': ');
    case 'waiting': return [p.since ? `Finished ${ago(p.since)}` : 'Finished', p.detail].filter(Boolean).join(' · ');
    case 'working': case 'background': return [p.detail, p.since ? fmtDur(now() - p.since) : ''].filter(Boolean).join(' · ');
    case 'idle': return ['Shell prompt', out].filter(Boolean).join(' · ');
    case 'running': return [p.detail, p.since ? 'for ' + fmtDur(now() - p.since) : '', out].filter(Boolean).join(' · ');
    default: return p.detail || '';
  }
}

const DOTS = ic('more-h');
const menuBtn = (kind, s, tb, p, label) =>
  `<button class="icon-btn sm" data-menu="${kind}" data-session="${esc(s.name)}"${tb ? ` data-tab="${tb.id}"` : ''}${p ? ` data-pane="${p.id}"` : ''} aria-label="${esc(label)}" title="${esc(label)}" aria-haspopup="menu">${DOTS}</button>`;
const plusBtn = (s, tb, label) =>
  `<button class="icon-btn sm" data-new="pane" data-session="${esc(s.name)}" data-tab="${tb.id}" data-tabname="${esc(tb.name)}" aria-label="${esc(label)}" title="${esc(label)}">${ic('plus')}</button>`;

function paneRow(s, tb, p, title, extra = '') {
  const [cls, label] = TSTATE[p.state] || ['', p.state];
  const key = s.name + ':' + p.id;
  const subs = p.agent?.subagents || [];
  const open = S.subsOpen.has(key) && subs.length > 0;
  let h = `<div class="trow st-${esc(p.state)}" role="button" tabindex="0" data-open="${esc(key)}">
    <span class="pill ${cls}">${esc(label)}</span>
    <div class="t">${title}<span class="kind">${ic(p.agent || p.kind === 'claude' || p.kind === 'codex' ? 'sparkle' : 'terminal-square')}${esc(p.kind)}</span>${p.git?.branch ? `<span class="brc" title="${esc(tilde(p.git.root))}${p.git.worktree ? ' (worktree)' : ''}">${IC.branch}${esc(p.git.branch)}</span>` : ''}</div>
    <div class="act"><span class="use">${p.n ? `${fmtPct(p.cpu)} · ${fmtB(p.rss)}` : ''}</span>${subs.length
      ? `<button class="chip subs" data-subs="${esc(key)}" aria-expanded="${open}">${ic('people')}${plural(subs.length, 'subagent')}${caret}</button>` : ''}${extra}</div>
    <div class="d">${(p.state === 'working' || p.state === 'background') && paneDetail(p) ? shim(paneDetail(p)) : esc(paneDetail(p)) || '&nbsp;'}</div></div>`;
  if (open) {
    h += '<div class="subs-list">' + subs.map((a) => `<div class="sa" role="button" tabindex="0" data-opensub="${esc(key + '|' + a.id)}" title="See what it is doing"${dropAttr('s:' + key)}><div class="t"><i class="spin"></i>${esc(a.desc || a.id)}</div>
      <div class="d">${esc([a.type, a.model, a.bg ? 'background' : '', a.doing || 'Thinking', fmtDur(now() - a.started)].filter(Boolean).join(' · '))}</div></div>`).join('') + '</div>';
  }
  return h;
}

function viewTerm(t) {
  const sessions = t.sessions || [];
  const tiles = '<div class="grid">' +
    tile('Waiting for you', t.waiting || 0, `${t.asking || 0} need an answer · ${(t.waiting || 0) - (t.asking || 0)} finished`, '', t.asking ? 'warn' : '', 'bell') +
    tile('Working', t.working || 0, plural(t.subagents || 0, 'subagent') + ' running', '', '', 'bolt') +
    tile('Terminals', t.panes || 0, `${plural(t.agents || 0, 'agent')} · ${Math.max(0, (t.panes || 0) - (t.agents || 0))} other`, '', '', 'terminal-square') +
    tile('Sessions', sessions.length, sessions.map((s) => esc(s.name)).join(' · ') || 'no zellij sessions', '', '', 'window2') +
    '</div>';
  const head = sec('Sessions', '', `<button class="btn sm" data-new="session">${ic('plus')}New session</button>`, 'window2');
  if (!sessions.length) return tiles + `<div>${head}<div class="card empty">No live zellij sessions.</div></div>`;
  return tiles + `<div>${head}<div class="sessions">` + sessions.map((s) => {
    const all = (s.tabs || []).flatMap((tb) => tb.panes);
    const n = (st) => all.filter((p) => p.state === st).length;
    const summary = [n('asking') && `${n('asking')} need answer`, n('waiting') && `${n('waiting')} waiting`,
      (n('working') + n('stalled') + n('background')) && `${n('working') + n('stalled') + n('background')} working`, plural(all.length, 'pane')].filter(Boolean).join(' · ');
    const body = (s.tabs || []).map((tb) => {
      const num = `<span class="tab">#${tb.pos + 1}</span>`;
      if (tb.panes.length === 1) return paneRow(s, tb, tb.panes[0], num + esc(tb.name), menuBtn('pane', s, tb, tb.panes[0], `Actions for ${tb.name}`));
      return `<div class="ttab">${num}<b>${esc(tb.name)}</b><span class="faint">${tb.panes.length} panes</span><span class="spacer"></span>${plusBtn(s, tb, `New pane in ${tb.name}`)}${menuBtn('tab', s, tb, null, `Actions for tab ${tb.name}`)}</div>` +
        tb.panes.map((p) => paneRow(s, tb, p, esc(p.title || `pane ${p.id}`) + (p.floating ? ' <span class="faint">floating</span>' : ''), menuBtn('pane', s, tb, p, `Actions for ${p.title || 'pane ' + p.id}`))).join('');
    }).join('');
    return `<div class="card flush session"><div class="sh">${ic('window2', 'sh-ic')}<b>${esc(s.name)}</b><span class="muted">${esc(summary)}</span><span class="spacer"></span>
      <button class="btn sm" data-new="tab" data-session="${esc(s.name)}">${ic('plus')}Tab</button>${menuBtn('session', s, null, null, `Actions for session ${s.name}`)}</div>
      ${s.err ? `<div class="empty err">${esc(s.err)}</div>` : ''}${body || '<div class="empty">No panes.</div>'}</div>`;
  }).join('') + '</div></div>';
}

function findPane(session, id) {
  for (const s of S.data.term?.sessions || []) {
    if (s.name !== session) continue;
    for (const tb of s.tabs || []) for (const p of tb.panes) if (p.id === id) return { s, tb, p };
  }
  return null;
}

// ---------- pane drawer: chat, terminal ----------
const coarse = matchMedia('(pointer: coarse)').matches;
const D = { key: null, session: '', pane: -1, mode: 'chat', es: null, got: false, items: [], before: 0, title: '', root: '', seen: 0, q: '', pick: null,
  view: null, cols: 0, rows: 0, state: '', fit: !coarse, openTools: new Set(), ended: false, stale: false, files: [], sending: false };
const F = { open: false, list: null, root: '', filter: 'all', path: '', file: null, timer: 0 };
// the Files panel lists the documents a session made (HTML, Markdown), not code or Claude's own memory files
const isDoc = (p) => /\.(md|markdown|html?)$/i.test(p) && !p.includes('/.claude/projects/');
const isHTML = (p) => /\.html?$/i.test(p);

const IC = {
  spark: ic('sparkle'), x: ic('xmark'), file: ic('file'), image: ic('image3'), branch: ic('hierarchy3'), tree: ic('folder'),
  copy: ic('copy'), up: ic('arrow-up'), stop: ic('stop-f'),
};
const EXPLORE = { Read: 'Read', NotebookRead: 'Read', Grep: 'Searched', Glob: 'Listed', LS: 'Listed', WebFetch: 'Fetched', WebSearch: 'Searched the web for' };
const RUN = new Set(['Bash', 'BashOutput', 'Monitor', 'KillShell', 'KillBash']);
const EDIT = new Set(['Edit', 'MultiEdit', 'Write', 'NotebookEdit']);
const PLAN = new Set(['TodoWrite', 'TaskCreate', 'TaskUpdate', 'TaskList', 'TaskGet', 'TaskOutput']);
const isImg = (n) => /\.(png|jpe?g|gif|webp)$/i.test(n);
const uploadLabel = (n) => n.replace(/^\d{8}-\d{6}-[0-9a-f]{16}-?/, '') || n;
const relPath = (p) => (p && D.root && p.startsWith(D.root + '/') ? p.slice(D.root.length + 1) : tilde(p));
const selIn = (el) => { const s = getSelection(); return !!(s && !s.isCollapsed && s.rangeCount && el.contains(s.getRangeAt(0).commonAncestorContainer)); };

function openPane(key, push = true) {
  const [session, pane] = splitKey(key);
  const f = findPane(session, pane);
  if (!f) return;
  if (D.es) D.es.close();
  const switching = !!D.key;
  Object.assign(D, { key, session, pane, mode: f.p.agent?.chat ? 'chat' : 'screen', es: null, items: [], before: 0, title: '', view: null, ended: false, stale: false, root: f.p.git?.root || '' });
  D.openTools.clear();
  closePreview();
  closeSub();
  Object.assign(F, { list: null });
  $('#drawer').hidden = false;
  $('#dr-switch').hidden = true;
  document.body.classList.add('drawer-open');
  if (push && !switching) history.pushState({ pane: key }, '');
  else if (history.state?.pane) history.replaceState({ pane: key }, '');
  $('#dr-body').innerHTML = `<div class="chat"><div class="chat-empty">${shim('Loading the conversation…')}</div></div>`;
  $('#term-pre').textContent = '';
  $('#term-sb').textContent = '';
  Object.assign(D, { sb: [], sbDirty: false, termFresh: true, loadingMore: false, skills: null });
  loadSkills();
  D.seen = 0;
  renderDrawer();
  if (F.open) loadFiles();
  connectPane();
  if (D.mode === 'screen' && !coarse) termFocus();
}

function closePane(pop = true) {
  if (!D.key) return;
  D.key = null;
  if (D.es) { D.es.close(); D.es = null; }
  D.q = '';
  for (const q of $$('#drawer .rl-q input')) q.value = '';
  toggleModels(false);
  $('#selbar').hidden = true;
  clearFiles();
  closePreview();
  closeSub();
  closeFilesPanel();
  $('#term-in').blur();
  $('#drawer').hidden = true;
  document.body.classList.remove('drawer-open');
  if (pop && history.state?.pane) history.back();
}
addEventListener('popstate', () => { if (D.key) closePane(false); });

function connectPane() {
  if (D.es) D.es.close();
  const key = D.key;
  D.got = false;
  const es = new EventSource(`/api/pane/stream?session=${encodeURIComponent(D.session)}&pane=${D.pane}&mode=${D.mode}`);
  es.onmessage = (e) => {
    D.got = true;
    const m = JSON.parse(e.data);
    if (m.t === 'chat') onChat(m);
    else if (m.t === 'screen') onScreen(m.u);
    else if (m.t === 'end') { D.ended = true; es.close(); if (D.es === es) D.es = null; renderBody(); } // pane gone: don't reconnect
  };
  setTimeout(() => { if (D.es === es && !D.got) window.cpReport?.('stall', `no ${D.mode} data for ${key} after 10 s (stream state ${es.readyState})`); }, 10000);
  es.onerror = () => {
    if (es.readyState !== EventSource.CLOSED || D.key !== key || D.es !== es) return;
    D.es = null;
    if (!D.got && D.mode === 'chat') { D.mode = 'screen'; renderDrawer(); } // no transcript: show the terminal
    setTimeout(() => { if (D.key === key && !D.es) connectPane(); }, 2000);
  };
  D.es = es;
}

function onChat(m) {
  if (m.reset) { D.items = m.items || []; D.before = m.before || 0; D.seen = 0; }
  else D.items = D.items.concat(m.items || []).slice(-8000);
  if (m.title) D.title = m.title;
  renderDrawer();
  renderBody();
}

// The first frame brings the pane's scrollback; after that, lines that scroll off the top of the
// screen are added to it, so the history above the live screen stays whole.
function onScreen(u) {
  const next = u.viewport || [];
  if (u.is_initial) { D.sb = (u.scrollback || []).slice(-3000); D.sbDirty = true; }
  else if (D.view) {
    const s = scrolledOff(D.view, next);
    if (s) { D.sb = D.sb.concat(D.view.slice(0, s)).slice(-3000); D.sbDirty = true; }
  }
  D.view = next;
  renderBody();
}

// ---- chat: Codex's work log drawn with beautifului.dev's components (tool chips, thinking trace, task rows) ----
const durOf = (it, r) => (r?.t && it.t && r.t >= it.t && r.t - it.t >= 1 ? fmtDur(r.t - it.t) : '');
const failIc = (r, running) => (running ? '<i class="spin"></i>' : r?.err ? `<span class="ev-bad" title="Failed">${ic('x-circle-f')}</span>` : '');
const opened = (id) => (D.openTools.has(id) ? ' open' + dropAttr('d:' + id) : '');
const chev = ic('angle-right', 'ev-chev');
const shim = (t, cls = '') => `<span class="t-shimmer${cls ? ' ' + cls : ''}" data-text="${esc(t)}">${esc(t)}</span>`;
const verbHTML = (v, running) => (running ? shim(v, 'ev-v') : `<span class="ev-v">${esc(v)}</span>`);
const EXPLORE_IC = { Read: 'eye', NotebookRead: 'eye', Grep: 'search', Glob: 'search', LS: 'folder', WebFetch: 'global', WebSearch: 'global' };
const extOf = (n) => (String(n).match(/\.([a-z0-9]{1,5})$/i)?.[1] || '').toLowerCase();
const fxBadge = (n) => { const x = extOf(n); return `<span class="fx ${esc(x)}">${esc(x || 'file')}</span>`; };
// The live line speaks like Claude Code: a whimsical verb and one of several loading orbs, picked per
// turn (seeded by when the turn began, so they hold still while it runs). The rows above say exactly what.
const VERBS = ('Accomplishing Actualizing Baking Brewing Calculating Cerebrating Channelling Churning Clauding Coalescing Cogitating ' +
  'Combobulating Computing Concocting Conjuring Considering Contemplating Cooking Crafting Crunching Deciphering Deliberating Divining ' +
  'Elucidating Enchanting Envisioning Finagling Forging Frolicking Generating Germinating Hatching Herding Hustling Ideating Imagining ' +
  'Incubating Inferring Jiving Julienning Leavening Manifesting Marinating Meandering Moseying Mulling Musing Mustering Noodling ' +
  'Percolating Perusing Philosophising Pondering Puttering Puzzling Reticulating Ruminating Scheming Schlepping Shimmying Shucking ' +
  'Simmering Smooshing Spelunking Spinning Stewing Sussing Synthesizing Tinkering Transmuting Unfurling Unravelling Vibing Whirring ' +
  'Wibbling Wizarding Wrangling').split(' ');
const ORBS = [
  `<span class="px" aria-hidden="true">${'<i></i>'.repeat(9)}</span>`,
  '<span class="ld-dots" aria-hidden="true"><i></i><i></i><i></i></span>',
  '<span class="ld-orbit" aria-hidden="true"></span>',
  '<span class="ld-wave" aria-hidden="true"><i></i><i></i><i></i><i></i><i></i></span>',
  `<span class="ld-star" aria-hidden="true">${ic('sparkle-f')}</span>`,
];
const seedOf = (v) => { let h = 2166136261; for (const c of String(v)) h = Math.imul(h ^ c.charCodeAt(0), 16777619); return h >>> 0; };
const elapsed = (since) => { const s = Math.max(0, now() - since); return s < 60 ? s.toFixed(1) + 's' : fmtDur(s); };

function diffHTML(d) {
  return (d || '').split('\n').map((l) => (l === '@@' ? '<div class="dl hunk">⋯</div>'
    : `<div class="dl ${l[0] === '+' ? 'add' : l[0] === '-' ? 'del' : ''}"><span class="dg">${l[0] === ' ' ? '' : esc(l[0])}</span>${esc(l.slice(1)) || ' '}</div>`)).join('');
}

function exploreEv(group, running, fr) {
  const n = { Read: 0, Searched: 0, Listed: 0, Fetched: 0 };
  for (const g of group) { const v = EXPLORE[g.it.name] === 'Searched the web for' ? 'Searched' : EXPLORE[g.it.name]; n[v] = (n[v] || 0) + 1; }
  const sum = [n.Read && plural(n.Read, 'file'), n.Searched && plural(n.Searched, 'search').replace('searchs', 'searches'), n.Listed && plural(n.Listed, 'listing'), n.Fetched && plural(n.Fetched, 'page')].filter(Boolean).join(', ');
  const id = 'ex-' + group[0].it.id;
  const lines = group.map(({ it, r }) => `<div class="ev-li">${ic(EXPLORE_IC[it.name] || 'search')}<span class="ev-v2">${EXPLORE[it.name]}</span><code>${esc(it.path ? relPath(it.path) : it.text)}</code>${r?.err ? `<span class="ev-bad">${ic('x-circle-f')}</span>` : ''}</div>`).join('');
  return `<details class="ev${fr}"${opened(id)} data-id="${esc(id)}"><summary>${ic('search', 'ev-ic')}${verbHTML(running ? 'Exploring' : 'Explored', running)}<span class="ev-a">${esc(sum)}</span>${running ? '<i class="spin"></i>' : ''}${chev}</summary><div class="ev-list">${lines}</div></details>`;
}

function runEv(it, r, running, fr) {
  const cmd = it.in || it.text || '';
  const first = cmd.split('\n')[0];
  return `<details class="ev run${r?.err ? ' err' : ''}${fr}"${opened(it.id)} data-id="${esc(it.id)}"><summary>${ic('terminal-square', 'ev-ic')}${verbHTML(running ? 'Running' : 'Ran', running)}<code class="ev-a chipx">${esc(first)}${cmd.includes('\n') ? ' …' : ''}</code><span class="ev-m">${durOf(it, r)}</span>${failIc(r, running)}${chev}</summary>` +
    `<div class="ev-term"><button type="button" class="abtn ev-copy" data-copyid="${esc(it.id)}" title="Copy output" aria-label="Copy output">${ic('copy')}</button><div class="ev-cmd"><span class="ps">$</span>${esc(cmd)}</div>` +
    (r ? `<pre class="ev-out${r.err ? ' err' : ''}">${r.text ? esc(r.text) : '<span class="faint">(no output)</span>'}</pre>` : '') + '</div></details>';
}

function editEv(it, r, running, fr) {
  const verb = running ? (it.name === 'Write' ? 'Writing' : 'Editing') : it.name === 'Write' ? 'Wrote' : 'Edited';
  return `<details class="ev edit${r?.err ? ' err' : ''}${fr}"${opened(it.id)} data-id="${esc(it.id)}"><summary>${ic('pen', 'ev-ic')}${verbHTML(verb, running)}<code class="ev-a chipx">${esc(relPath(it.path))}</code>` +
    `<span class="add">+${it.add || 0}</span><span class="del">−${it.del || 0}</span>${failIc(r, running)}${chev}</summary>` +
    `<div class="diff">${diffHTML(it.diff)}</div>${r?.err ? `<pre class="ev-out err">${esc(r.text)}</pre>` : ''}</details>`;
}

function otherEv(it, r, running, fr = '') {
  if (PLAN.has(it.name)) return `<div class="ev plain${fr}">${ic('list-check', 'ev-ic')}<span class="ev-v">Updated the plan</span></div>`;
  const agent = it.name === 'Agent' || it.name === 'Task';
  const verb = agent ? (running ? 'Delegating' : 'Delegated') : it.name === 'Skill' ? 'Used skill' : it.name === 'Result' ? 'Result' : 'Used';
  const icon = agent ? 'people' : it.name === 'Skill' ? 'wand-sparkle' : it.name === 'Result' ? 'info-circle' : 'puzzle';
  const arg = agent || it.name === 'Skill' ? it.text : `${it.name.replace(/^mcp__[^_]+__/, '')}${it.text ? ' ' + it.text : ''}`;
  const body = (it.in && !agent ? `<pre class="ev-out">${esc(it.in)}</pre>` : '') + (r ? (agent ? `<div class="md ev-md">${md(r.text)}</div>` : `<pre class="ev-out${r.err ? ' err' : ''}">${esc(r.text)}</pre>`) : '');
  const see = agent && it.id ? `<button type="button" class="ev-see" data-subtool="${esc(it.id)}" title="See this subagent's chat" aria-label="See this subagent's chat">${ic('arrow-up-right')}</button>` : '';
  return `<details class="ev${r?.err ? ' err' : ''}${fr}"${opened(it.id)} data-id="${esc(it.id)}"><summary>${ic(icon, 'ev-ic')}${verbHTML(verb, running)}${arg ? `<span class="ev-a chipx">${esc(arg)}</span>` : ''}<span class="ev-m">${durOf(it, r)}</span>${failIc(r, running)}${see}${chev}</summary>${body ? `<div class="ev-body">${body}</div>` : ''}</details>`;
}

function thinkEv(it, fr) {
  const label = it.dur >= 1 ? `Thought for ${fmtDur(it.dur)}` : 'Thought';
  const id = `th-${it.t}-${(it.text || '').length}`;
  return it.text
    ? `<details class="ev think${fr}"${opened(id)} data-id="${id}"><summary>${ic('sparkle', 'ev-ic')}<span class="ev-v">${label}</span>${chev}</summary><div class="think-body md">${md(it.text)}</div></details>`
    : `<div class="ev think plain${fr}">${ic('sparkle', 'ev-ic')}<span class="ev-v">${label}</span></div>`;
}

// files a turn hands over (documents it wrote, or named in its messages): cards to preview or download
const PRODUCT = /\.(html?|md|markdown|pdf|png|jpe?g|gif|webp|svg|csv|tsv|xlsx?)$/i;
const KIND = [[/\.html?$/i, 'HTML page'], [/\.(md|markdown)$/i, 'Markdown document'], [/\.pdf$/i, 'PDF'], [/\.svg$/i, 'SVG image'],
  [/\.(png|jpe?g|gif|webp)$/i, 'Image'], [/\.(csv|tsv)$/i, 'Table'], [/\.xlsx?$/i, 'Spreadsheet']];
function fileCards(paths) {
  return `<div class="fcards">${paths.map((p) => {
    const name = p.split('/').pop();
    const raw = `/api/pane/file?${fileQS()}&raw=1&path=${encodeURIComponent(p)}`;
    const thumb = /\.(png|jpe?g|gif|webp)$/i.test(p) ? `<img class="fcard-img" src="${raw}" alt="" loading="lazy">` : fxBadge(name);
    return `<div class="fcard" data-openfile="${esc(p)}" role="button" tabindex="0" title="Preview ${esc(name)}">${thumb}` +
      `<span class="fcard-b"><b>${esc(name)}</b><span>${esc((KIND.find(([re]) => re.test(p)) || [0, 'File'])[1])} · ${esc(relPath(p).replace(/[^/]*$/, '') || './')}</span></span>` +
      `<a class="icon-btn ghost sm" href="${raw}" download title="Download" aria-label="Download ${esc(name)}">${ic('download2')}</a></div>`;
  }).join('')}</div>`;
}

function changesCard(files) {
  const list = [...files];
  const add = list.reduce((a, [, c]) => a + c.add, 0);
  const del = list.reduce((a, [, c]) => a + c.del, 0);
  const shown = list.slice(0, 8);
  return `<div class="chg"><div class="chg-h">${ic('pen')}<span>${plural(list.length, 'file')} changed</span><span class="add">+${add}</span><span class="del">−${del}</span></div><div class="fchips">` +
    shown.map(([p, c]) => `<button type="button" class="fc" data-openfile="${esc(p)}" title="${esc(relPath(p))}"><span>${esc(p.split('/').pop())}</span><span class="add">+${c.add}</span>${c.del ? `<span class="del">−${c.del}</span>` : ''}</button>`).join('') +
    (list.length > shown.length ? `<button type="button" class="fc more" data-files>+${list.length - shown.length} more</button>` : '') + '</div></div>';
}

function userMsg(it, fr, queued = false) {
  const u = splitUploads(it.text);
  const imgs = u.names.filter(isImg);
  const docs = u.names.filter((n) => !isImg(n));
  return `<div class="msg-u${fr}${queued ? ' queued' : ''}">` +
    (imgs.length ? `<div class="uimgs">${imgs.map((n) => `<img src="/api/upload/${esc(n)}" alt="Sent image" loading="lazy">`).join('')}</div>` : '') +
    (docs.length ? `<div class="udocs">${docs.map((n) => `<a class="udoc" href="/api/upload/${esc(n)}" download>${fxBadge(n)}<span>${esc(uploadLabel(n))}</span>${ic('download2')}</a>`).join('')}</div>` : '') +
    (u.text || it.img ? `<div class="bubble">${mdInline(u.text)}${it.img ? `<div class="att">${ic('image3')}${plural(it.img, 'image')}</div>` : ''}</div>` : '') +
    (it.mid ? `<span class="q-l">${ic('bolt')}Sent while Claude was working</span>` : '') +
    (queued ? `<span class="q-l">${ic('clock-circle')}Queued · goes to Claude when it's free</span>` : '') + '</div>';
}

const textMsg = (it, i, fr) => `<div class="msg-a${fr}"><div class="md">${md(it.text)}</div><div class="msg-act">` +
  `<button type="button" class="abtn" data-copyi="${i}" title="Copy" aria-label="Copy">${ic('copy')}</button>` +
  `<button type="button" class="abtn" data-quotei="${i}" title="Quote in your reply" aria-label="Quote in your reply">${ic('quote-up')}</button></div></div>`;

const NOTE_IC = [[/^Interrupted/, 'hand'], [/^PR #/, 'hierarchy'], [/compact/i, 'layers-alt'], [/^Worked for/, 'clock-circle'], [/^\//, 'slash-square'], [/^Background|completed|failed|killed/i, 'bell']];
const noteHTML = (it, fr = '') => `<div class="sysnote${fr}"><span>${ic((NOTE_IC.find(([re]) => re.test(it.text)) || [0, 'info-circle'])[1])}${esc(it.text)}</span></div>`;

// c: whose chat — the pane's (D) or a subagent's (SA); both carry items, seen, before, loadingMore, ended, key
function chatHTML(p, c = D) {
  const items = c.items;
  const res = new Map();
  const tools = new Set();
  for (const it of items) { if (it.k === 'result') res.set(it.id, it); else if (it.k === 'tool') tools.add(it.id); }
  const working = p && (p.state === 'working' || p.state === 'stalled');
  let lastTool = null;
  for (let i = items.length - 1; i >= 0 && !lastTool; i--) if (items[i].k === 'tool') lastTool = items[i];
  const running = (it) => working && it === lastTool && !res.get(it.id);
  const fresh = (i) => (c.seen && i >= c.seen ? ' fresh' : '');
  // one turn's work as Codex rows: consecutive reads and searches fold into one "Explored" row
  const workHTML = (idx, changed) => {
    let h = '';
    let explore = [];
    const flush = () => { if (explore.length) h += exploreEv(explore, explore.some((g) => running(g.it)), fresh(explore[explore.length - 1].i)); explore = []; };
    for (const i of idx) {
      const it = items[i];
      if (it.k === 'result') { if (!tools.has(it.id)) { flush(); h += otherEv({ name: 'Result', text: '', id: it.id }, it, false, fresh(i)); } continue; }
      if (it.k === 'tool' && EXPLORE[it.name]) { explore.push({ it, r: res.get(it.id), i }); continue; }
      flush();
      if (it.k === 'think') h += thinkEv(it, fresh(i));
      else if (it.k === 'text') h += textMsg(it, i, fresh(i));
      else if (it.k === 'tool') {
        const r = res.get(it.id);
        if (RUN.has(it.name)) h += runEv(it, r, running(it), fresh(i));
        else if (EDIT.has(it.name)) {
          h += editEv(it, r, running(it), fresh(i));
          if (it.path && !r?.err) { const c = changed.get(it.path) || { add: 0, del: 0 }; c.add += it.add || 0; c.del += it.del || 0; changed.set(it.path, c); }
        } else h += otherEv(it, r, running(it), fresh(i));
      }
    }
    flush();
    return h;
  };
  const turns = splitTurns(items);
  // older messages load by themselves as you near the top (the sentinel below); a click works too
  let h = c.before > 0 ? `<div class="more-s" data-more aria-live="polite">${c.loadingMore ? shim('Loading earlier messages…') : ''}</div>` : '';
  turns.forEach((t, n) => {
    const last = n === turns.length - 1;
    const changed = new Map();
    if (t.user >= 0) h += userMsg(items[t.user], fresh(t.user));
    if (t.work.length) {
      const body = workHTML(t.work, changed);
      if (last && (working || t.final < 0)) h += body; // the turn in progress stays open, as a live trace
      else { // a finished turn folds its work away under one line, as in Codex
        const nt = t.work.filter((i) => items[i].k === 'tool').length;
        const nm = t.work.filter((i) => items[i].k === 'text').length;
        const id = 'turn-' + (t.work.map((i) => items[i].id).find(Boolean) || t.work[0]);
        const took = t.worked >= 0 ? items[t.worked].text : '';
        const label = [nt && plural(nt, 'tool call'), nm && plural(nm, 'message')].filter(Boolean).join(', ');
        h += `<details class="fold"${opened(id)} data-id="${esc(id)}"><summary>${ic('angle-right')}<span>${esc(label || took || 'Thought')}</span>` +
          `${label && took ? `<span class="faint">· ${esc(took)}</span>` : ''}</summary><div class="fold-b">${body}</div></details>`;
      }
    }
    if (t.final >= 0) h += textMsg(items[t.final], t.final, fresh(t.final));
    const made = new Set(); // what this turn hands over, in order
    for (const i of [...t.work, t.final]) {
      const it = items[i];
      if (!it) continue;
      if (it.k === 'tool' && it.name === 'Write' && it.path && PRODUCT.test(it.path) && !res.get(it.id)?.err) made.add(it.path);
      for (const p of it.files || []) made.add(p);
    }
    if (made.size) { h += fileCards([...made]); for (const p of made) changed.delete(p); }
    if (changed.size) h += changesCard(changed);
    for (const i of t.notes) h += noteHTML(items[i], fresh(i));
    if (t.worked >= 0 && !t.work.length) h += noteHTML(items[t.worked], fresh(t.worked)); // no fold line to carry it
  });
  const subs = p?.agent?.subagents || [];
  if (subs.length) {
    h += `<div class="tasks">${subs.map((a) => `<button type="button" class="task" data-sub="${esc(a.id)}" title="${esc(['See what it is doing', a.type, a.model, fmtDur(now() - a.started)].filter(Boolean).join(' · '))}"><i class="spin"></i>` +
      `<span class="task-t">${esc(a.desc || a.id)}</span><span class="task-m">${esc(a.doing || 'Thinking')}</span><span class="tbadge">${a.bg ? 'Background' : 'Running'}</span>${ic('angle-right', 'task-go')}</button>`).join('')}</div>`;
  }
  const seed = seedOf(`${c.key}:${p?.since || 0}`);
  if (working) h += `<div class="live" role="status">${ORBS[(seed >>> 8) % ORBS.length]}${shim(VERBS[seed % VERBS.length] + '…')}<span class="live-t" data-since="${p.since || 0}">${p.since ? elapsed(p.since) : ''}</span></div>`;
  const queued = working ? pendingQueue(items) : []; // typed while Claude works; an idle agent has emptied its queue
  if (queued.length) h += queued.map((q) => userMsg({ text: q }, '', true)).join('');
  if (c.ended) h += noteHTML({ text: 'The pane closed' });
  return `<div class="chat">${h || `<div class="chat-empty">${ic('chat-round-dots')}<span>No messages yet</span></div>`}</div>`;
}
setInterval(() => { const t = D.key && $('#dr-body .live-t'); if (t && +t.dataset.since) t.textContent = elapsed(+t.dataset.since); }, 200);

// ---- a subagent's own chat, over the main one ----
const SA = { open: false, id: '', tool: '', es: null, items: [], seen: 0, before: 0, loadingMore: false, ended: false, key: '', meta: null, got: false, err: '', stale: false };
const paneSubs = () => findPane(D.session, D.pane)?.p.agent?.subagents || [];
function openSub(id, tool = '') {
  closeSub();
  // ponytail: a subagent's chat opens on its last 5000 items with no paging back; add paging if they get that long
  Object.assign(SA, { open: true, id, tool, items: [], seen: 0, got: false, err: '', stale: false, key: `${D.key}/${id || tool}`, meta: paneSubs().find((a) => a.id === id) || null });
  $('#dr-sa').hidden = false;
  $('#sa-body').innerHTML = '';
  renderSub();
  const q = id ? 'sub=' + encodeURIComponent(id) : 'tool=' + encodeURIComponent(tool);
  const es = new EventSource(`/api/pane/stream?session=${encodeURIComponent(D.session)}&pane=${D.pane}&mode=chat&${q}`);
  es.onmessage = (e) => {
    const m = JSON.parse(e.data);
    if (m.t !== 'chat') return;
    SA.got = true;
    if (m.sub) { SA.meta = { ...SA.meta, ...m.sub }; SA.id = m.sub.id; }
    if (m.reset) { SA.items = m.items || []; SA.seen = 0; } else SA.items = SA.items.concat(m.items || []).slice(-8000);
    renderSub();
  };
  es.onerror = () => {
    if (es.readyState !== EventSource.CLOSED || SA.es !== es) return;
    if (!SA.got) { SA.err = 'This subagent has no chat to show.'; return renderSub(); }
    setTimeout(() => { if (SA.es === es) openSub(SA.id); }, 2000); // the panel restarted: pick it up again
  };
  SA.es = es;
}
function closeSub() {
  if (!SA.open) return;
  SA.open = false;
  if (SA.es) { SA.es.close(); SA.es = null; }
  $('#dr-sa').hidden = true;
  $('#sa-body').innerHTML = '';
  $('#sa-h').dataset.html = '';
}
function renderSubHead() {
  if (!SA.open) return;
  const m = SA.meta || {};
  const live = paneSubs().find((a) => a.id === SA.id);
  const state = live ? `<span class="pill busy">${live.bg ? 'Background' : 'Running'}</span>` : SA.got ? '<span class="pill">Finished</span>' : '';
  const head = `<button type="button" class="icon-btn ghost sm" data-saclose aria-label="Back to the main chat" title="Back (Esc)">${ic('angle-left')}</button>` +
    `${ic('people', 'sa-ic')}<div class="pv-t"><b>${esc(m.desc || 'Subagent')}</b><span>${esc([m.type, m.model, live ? fmtDur(now() - live.started) : ''].filter(Boolean).join(' · '))}</span></div>${state}` +
    `<button type="button" class="icon-btn ghost sm" data-saclose aria-label="Close" title="Close (Esc)">${IC.x}</button>`;
  const h = $('#sa-h');
  if (h.dataset.html !== head) { h.innerHTML = head; h.dataset.html = head; }
  return live;
}
function renderSub() {
  if (!SA.open) return;
  const live = renderSubHead();
  if (held() || selIn($('#dr-sa'))) { SA.stale = true; return; }
  SA.stale = false;
  const body = $('#sa-body');
  const atBottom = body.scrollTop + body.clientHeight >= body.scrollHeight - 80;
  const html = SA.err ? `<div class="chat"><div class="chat-empty">${ic('info-circle')}<span>${esc(SA.err)}</span></div></div>`
    : !SA.got ? `<div class="chat"><div class="chat-empty">${shim('Loading the subagent’s chat…')}</div></div>`
      : chatHTML(live ? { state: 'working', since: live.started } : { state: 'waiting' }, SA);
  keepScroll([body], () => { body.innerHTML = html; });
  applyDrops(body);
  SA.seen = SA.items.length;
  if (atBottom) body.scrollTop = body.scrollHeight;
}

// ---- terminal ----
// flow: one block per line, so text can wrap to the view while rules and tables keep their shape
function termLines(lines, cur = -1, flow = false) {
  const frag = document.createDocumentFragment();
  lines.forEach((line, i) => {
    let out = frag;
    if (flow) {
      out = document.createElement('div');
      out.className = 'tl tl-' + termLineKind(line.replace(/\x1b\[[0-9;:?]*[ -/]*[@-~]/g, ''));
      frag.append(out);
    }
    for (const g of ansiSegments(line)) {
      if (!g.fg && !g.bg && !g.b && !g.d && !g.i && !g.u && !g.inv) { out.append(g.text); continue; }
      const sp = document.createElement('span');
      sp.textContent = g.text;
      let fg = g.fg;
      let bg = g.bg;
      if (g.inv) [fg, bg] = [bg || '#07090c', fg || '#d7dae0'];
      if (fg) sp.style.color = fg;
      if (bg) sp.style.backgroundColor = bg;
      if (g.b) sp.style.fontWeight = '700';
      if (g.d) sp.style.opacity = '.6';
      if (g.i) sp.style.fontStyle = 'italic';
      if (g.u) sp.style.textDecoration = 'underline';
      out.append(sp);
    }
    if (i === cur) { const c = document.createElement('span'); c.className = 'cur'; c.textContent = ' '; out.append(c); }
    if (!flow) frag.append('\n');
  });
  return frag;
}

// The live screen sits at the bottom, like a real terminal, with its history above to scroll back through.
// Shells keep their exact grid, shrunk to fit. An agent's pane is far wider than the view (it follows your
// SSH window), so its text is sized to fill the height and wraps at the view's edge instead.
function renderTerm(p) {
  const box = $('#dr-term');
  const pre = $('#term-pre');
  const sb = $('#term-sb');
  const lines = D.view || [];
  const cols = D.cols || Math.max(40, ...lines.map((l) => l.replace(/\x1b\[[0-9;:?]*[ -/]*[@-~]/g, '').length));
  const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 40;
  const rows = D.rows || lines.length || 24;
  const flow = agentTerm();
  const byHeight = (box.clientHeight - 20) / (rows * 1.22);
  const size = (flow ? Math.max(coarse ? 9 : 10, Math.min(15, byHeight))
    : Math.max(4, Math.min(18, (box.clientWidth - 26) / (cols * 0.602), byHeight))) + 'px';
  pre.style.fontSize = size;
  sb.style.fontSize = size;
  pre.classList.toggle('flow', flow);
  // a shell at its prompt: its cursor sits after the last text on screen (subscribe doesn't report the cursor)
  let cur = -1;
  if (p?.state === 'idle') for (let i = lines.length - 1; i >= 0 && cur < 0; i--) if (lines[i].replace(/\x1b\[[0-9;:?]*[ -/]*[@-~]/g, '').trim()) cur = i;
  const frag = termLines(lines, cur, flow);
  if (D.ended) frag.append('\n[the pane closed]\n');
  keepScroll([box], () => {
    if (D.sbDirty) { D.sbDirty = false; sb.replaceChildren(termLines(D.sb || [])); }
    pre.replaceChildren(frag);
  });
  if (!flow) { // glyphs from fallback fonts run wider than the estimate: measure once and shrink to fit
    const room = box.clientWidth - 24;
    if (pre.scrollWidth > room + 1) { const fs = parseFloat(size) * room / pre.scrollWidth; pre.style.fontSize = sb.style.fontSize = Math.max(4, fs) + 'px'; }
  }
  if (atBottom || D.termFresh) { box.scrollTop = box.scrollHeight; D.termFresh = false; } // follow the live screen unless you scrolled up
}

// Re-rendering replaces the DOM: never while a finger is down or text is selected (copying must work).
function renderBody(now = false) { // now: don't wait for a scroll to settle (earlier messages going in above)
  if (!D.key) return;
  if ((!now && held()) || selIn($('#drawer'))) { D.stale = true; return; }
  D.stale = false;
  const f = findPane(D.session, D.pane);
  if (D.mode === 'screen') {
    if (D.view || D.ended) renderTerm(f?.p);
    return;
  }
  if (!D.got && !D.ended) return;
  const body = $('#dr-body');
  const atBottom = body.scrollTop + body.clientHeight >= body.scrollHeight - 80;
  keepScroll([body], () => { body.innerHTML = chatHTML(f?.p); });
  applyDrops(body);
  moreObs.disconnect();
  const more = $('[data-more]', body);
  if (more) moreObs.observe(more);
  D.seen = D.items.length;
  if (atBottom) body.scrollTop = body.scrollHeight;
  updateDown();
}
// More than a screen above the latest: the jump-down button by the composer shows.
function updateDown() {
  const b = $('#dr-body');
  $('#dr-down').classList.toggle('on', D.mode !== 'screen' && b.scrollHeight - b.scrollTop - b.clientHeight > b.clientHeight);
}
$('#dr-down').addEventListener('click', () => { const b = $('#dr-body'); b.scrollTo({ top: b.scrollHeight, behavior: 'smooth' }); });
document.addEventListener('selectionchange', () => { if (D.stale && D.key && !selIn($('#drawer'))) renderBody(); });

// ---- rail / switcher: every terminal, one tap away, searchable ----
function railHTML() {
  const q = (D.q || '').trim().toLowerCase();
  const html = (S.data.term?.sessions || []).filter((s) => s.name === D.session).map((s) => { // the session this chat was opened in
    const rows = (s.tabs || []).flatMap((tb) => tb.panes.map((p) => {
      const key = s.name + ':' + p.id;
      const title = tb.panes.length > 1 ? `${tb.name} · ${p.title || 'pane ' + p.id}` : tb.name;
      const [cls, label] = TSTATE[p.state] || ['', p.state];
      if (q && ![title, s.name, label, p.git?.branch, p.agent?.title, p.proc].some((x) => String(x || '').toLowerCase().includes(q))) return '';
      const n = p.agent?.subagents?.length || 0;
      const desc = label + (p.git?.branch ? ' · ' + p.git.branch : '');
      return `<button type="button" class="rl-i${key === D.key ? ' on' : ''}" data-go="${esc(key)}" title="${esc(title)}"><span class="dot ${cls} st-${esc(p.state)}"></span>` +
        `<span class="rl-b"><span class="rl-t">${esc(title)}</span><span class="rl-d">${p.state === 'working' || p.state === 'background' ? shim(desc) : esc(desc)}</span></span>${n ? `<span class="rl-n" title="${plural(n, 'subagent')}">${n}</span>` : ''}</button>`;
    })).join('');
    return rows ? `<div class="rl-s">${ic('window2')}${esc(s.name)}</div>` + rows : '';
  }).join('');
  return html || `<div class="rl-none">${q ? `No terminal matches “${esc(D.q.trim())}”` : 'No terminals'}</div>`;
}
function renderRail() {
  const html = railHTML();
  for (const el of $$('#drawer .rl-items')) if (el.dataset.html !== html) keepScroll([el.parentElement, el.closest('.dr-switch')], () => { el.innerHTML = html; el.dataset.html = html; });
}
for (const q of $$('#drawer .rl-q input')) {
  q.addEventListener('input', () => { D.q = q.value; for (const o of $$('#drawer .rl-q input')) if (o !== q) o.value = q.value; renderRail(); });
  q.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); q.closest('.dr-rail, .dr-switch').querySelector('.rl-i:not(.on)')?.click(); }
    else if (e.key === 'Escape' && q.value) { e.stopPropagation(); q.value = ''; q.dispatchEvent(new Event('input')); }
  });
}

// ---- model and effort: Claude Code's own /model and /effort, sent like any message ----
const MODELS = [['claude-opus-5-5', 'Opus 5.5', 'Complex work, everyday tasks'], ['claude-fable-5-1', 'Fable 5.1', 'The toughest challenges'],
  ['claude-sonnet-5-5', 'Sonnet 5.5', 'Efficient for simpler tasks'], ['claude-haiku-4-5-20251001', 'Haiku 4.5', 'Fastest, for quick answers']];
const EFFORTS = ['low', 'medium', 'high', 'xhigh', 'max'];
const modelName = (id) => (MODELS.find(([m]) => m === id) || [0, String(id || '').replace(/^claude-/, '').replace(/-(\d+)-(\d+)(-\d{8})?$/, ' $1.$2').replace(/^\w/, (c) => c.toUpperCase())])[1];
function currentModel() {
  for (let i = D.items.length - 1; i >= 0; i--) if (D.items[i].k === 'model') return { id: D.items[i].text, effort: D.items[i].name };
  return null;
}
// the pill: the model's name and five small bars for its effort
function renderModel(p) {
  const btn = $('#dr-model');
  btn.hidden = !p?.agent;
  const m = currentModel();
  const lvl = EFFORTS.indexOf(m?.effort);
  $('#dr-model-t').textContent = m ? modelName(m.id) : 'Model';
  $$('#dr-model-b i').forEach((b, i) => b.classList.toggle('on', i <= lvl));
  btn.title = m ? `${modelName(m.id)}${m.effort ? ' · ' + m.effort + ' effort' : ''}` : 'Model and effort';
  if (!$('#dr-models').hidden) renderModels();
}
// a small menu: four models, then effort as rising bars (hover previews the level, a click sets it)
function renderModels() {
  const m = currentModel();
  const lvl = EFFORTS.indexOf(m?.effort);
  $('#dr-models').innerHTML = MODELS.map(([id, name, d]) =>
    `<button type="button" class="mp-o${m?.id === id ? ' on' : ''}" data-setmodel="${id}" title="${esc(d)}"><span>${esc(name)}</span>${m?.id === id ? ic('check') : ''}</button>`).join('') +
    `<div class="mp-eff"><span class="mp-l">Effort</span><span class="mp-bars">${EFFORTS.map((e, i) => i).reverse().map((i) =>
      `<button type="button" class="mp-bar b${i}${i <= lvl ? ' on' : ''}" data-seteffort="${EFFORTS[i]}" aria-label="${EFFORTS[i]} effort" title="${EFFORTS[i]}"></button>`).join('')}</span>` +
    `<span class="mp-v">${esc(m?.effort || '')}</span></div>` +
    (agentBusy() ? '<div class="mp-n">Applies when this turn ends</div>' : '');
}
function toggleModels(force) {
  const box = $('#dr-models');
  const open = force ?? box.hidden;
  box.hidden = !open;
  $('#dr-model').setAttribute('aria-expanded', String(open));
  if (open) { renderModels(); box.style.left = $('#dr-model').offsetLeft + 'px'; }
}
$('#dr-models').addEventListener('pointerover', (e) => { // name the level under the pointer
  const b = e.target.closest('.mp-bar');
  const v = $('#dr-models .mp-v');
  if (v) v.textContent = b ? b.dataset.seteffort : currentModel()?.effort || '';
});
async function setModel(id, effort) {
  toggleModels(false);
  const text = id ? `/model ${id}` : `/effort ${effort}`;
  if (await sendInput({ text, enter: true })) toast(id ? `Asked Claude to switch to ${modelName(id)}: confirm in the chat` : `Effort set to ${effort}`);
}
addEventListener('pointerdown', (e) => { if (!$('#dr-models').hidden && !e.target.closest('#dr-models, #dr-model')) toggleModels(false); }, true);

// the rail hides (remembered) to give one chat or terminal the whole width
function setRail(off) {
  document.body.classList.toggle('rail-off', off);
  store.set('railOff', off ? '1' : '0');
  setTimeout(() => dispatchEvent(new Event('resize')), 300); // the terminal re-fits
}
document.body.classList.toggle('rail-off', store.get('railOff') === '1');

// a highlight that glides to the hovered row (beautifului's sidebar nav)
function glide(box, sel) {
  const g = $('.glide', box);
  box.addEventListener('pointerover', (e) => {
    const it = e.target.closest(sel);
    if (!it || e.pointerType === 'touch') return;
    const first = !g.classList.contains('on');
    if (first) g.style.transition = 'none';
    g.style.transform = `translate(${it.offsetLeft}px, ${it.offsetTop}px)`;
    g.style.width = it.offsetWidth + 'px';
    g.style.height = it.offsetHeight + 'px';
    if (first) { void g.offsetWidth; g.style.transition = ''; }
    g.classList.add('on');
  });
  box.addEventListener('pointerleave', () => g.classList.remove('on'));
}
glide($('.nav'), '.nav-btn, .nav-tg');
glide($('#dr-rail .rl-list'), '.rl-i');

// a long path keeps its start and its end (the worktree's own name): ~/repos/app/…/feat-x
function shortPath(p, max = 56) {
  const parts = tilde(p).split('/');
  if (parts.join('/').length <= max || parts.length <= 4) return parts.join('/');
  return [...parts.slice(0, 3), '…', parts[parts.length - 1]].join('/');
}
// the header's second line: zellij session · the folder on the VM it works in · its branch
function whereChips(s, p) {
  const g = p.git;
  const dir = g?.root || p.agent?.cwd || '';
  return `<span class="dr-where" title="zellij session">${esc(s.name)}</span>` +
    (dir ? `<span class="gchip gdir" title="${esc(tilde(dir))}${g?.worktree ? ' (a git worktree)' : ''}">${IC.tree}<span class="gpath"><span dir="ltr">${esc(shortPath(dir))}</span></span>${g?.worktree ? '<em>worktree</em>' : ''}</span>` : '') +
    (g?.branch ? `<span class="gchip gbr" title="Branch">${IC.branch}<span class="gbr-t">${esc(g.branch)}${g.detached ? ' (detached)' : ''}</span></span>` : '');
}

function renderDrawer() {
  if (!D.key) return;
  const f = findPane(D.session, D.pane);
  const chat = D.mode === 'chat';
  const seg = $$('#drawer [data-mode]');
  for (const b of seg) b.setAttribute('aria-pressed', String(b.dataset.mode === D.mode));
  const on = seg.find((b) => b.dataset.mode === D.mode);
  const pill = $('#drawer .seg-pill');
  if (on && on.offsetWidth) { pill.style.width = on.offsetWidth + 'px'; pill.style.transform = `translateX(${on.offsetLeft - 2}px)`; }
  $('#dr-body').hidden = !chat;
  $('#dr-foot').hidden = !chat;
  $('#dr-term').hidden = chat;
  $('#dr-keys').hidden = chat || !coarse;
  renderModel(f?.p);
  renderRail();
  renderSubHead();
  if (!f) { $('#dr-sub').textContent = `${D.session} · pane ${D.pane} is gone`; $('#dr-prompt').hidden = true; return; }
  const { s, tb, p } = f;
  const prev = D.state;
  Object.assign(D, { cols: p.cols, rows: p.rows, state: p.state, root: p.git?.root || D.root });
  seg[0].disabled = !p.agent?.chat;
  $('#dr-filesbtn').hidden = !p.agent?.chat;
  $('#dr-title').textContent = tb.panes.length > 1 ? (p.title || `pane ${p.id}`) : tb.name;
  const sub = whereChips(s, p);
  if ($('#dr-sub').dataset.html !== sub) { $('#dr-sub').innerHTML = sub; $('#dr-sub').dataset.html = sub; }
  const [cls, label] = TSTATE[p.state] || ['', p.state];
  $('#dr-pill').className = 'pill ' + cls;
  $('#dr-pill').textContent = label;
  const pr = $('#dr-prompt');
  const q = p.state === 'asking' ? p.agent?.prompt : null;
  pr.hidden = p.state !== 'asking';
  // beautifului's approval card: pick an option, then Continue (a stray tap on a phone answers nothing)
  const html = q
    ? `<div class="ap" data-sig="${esc(q.sig)}"><div class="ap-q">${ic('help-circle')}<span>${esc(q.q || 'Claude needs your answer')}</span></div>${(q.ctx || []).length ? `<pre class="ap-ctx">${esc(q.ctx.join('\n'))}</pre>` : ''}
       <div class="ap-opts" role="radiogroup" aria-label="Answers">${(q.opts || []).map((o) => `<button type="button" class="ap-o" role="radio" aria-checked="false" data-pick="${esc(o.label)}"><span class="ap-r"></span>${o.n ? `<span class="ap-n">${esc(o.n)}.</span>` : ''}<span>${esc(o.label)}</span></button>`).join('')}</div>
       <div class="ap-f"><span class="hint">${coarse ? '' : '↑↓ to choose'}</span><button type="button" class="btn sm" data-skip title="Sends Esc">Cancel</button><button type="button" class="btn sm primary" data-choose>Continue</button></div></div>`
    : `<div class="ap"><div class="ap-q">${ic('help-circle')}<span>${esc(p.detail || 'Claude needs your answer')}</span></div><div class="muted">Switch to Terminal to see and answer it.</div></div>`;
  if (pr.dataset.html !== html) { pr.innerHTML = html; pr.dataset.html = html; } // don't swap buttons under a finger for nothing
  if (q) {
    if (D.pick?.sig !== q.sig) D.pick = { sig: q.sig, label: ((q.opts || []).find((o) => o.sel) || (q.opts || [])[0])?.label };
    for (const b of $$('.ap-o', pr)) b.setAttribute('aria-checked', String(b.dataset.pick === D.pick.label));
  }
  updateSend();
  if (chat && prev !== p.state) renderBody(); // live indicator follows the state
}

async function sendInput(body) {
  try {
    const r = await api('/api/pane/input', { session: D.session, pane: D.pane, ...body });
    if (r.status === 200) return true;
    if (r.status === 401 && r.error === 'login required') { closePane(); showLogin('Session ended — enter your PIN.'); return false; }
    toast(r.error || `Error ${r.status}`, true);
  } catch { toast('Network error.', true); }
  return false;
}

// ---- files in: attach (chat) or drop/paste (terminal types their paths, like a real terminal) ----
async function uploadFile(file) {
  const r = await fetch(`/api/pane/upload?name=${encodeURIComponent(file.name || 'pasted')}`, { method: 'POST', credentials: 'same-origin', body: file });
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || `Upload failed (${r.status})`);
  return j.path;
}
async function pastePaths(files) {
  try {
    const paths = [];
    for (const f of files) paths.push(await uploadFile(f));
    if (paths.length) sendInput({ text: paths.join(' ') + ' ' });
  } catch (err) { toast(err.message, true); }
}

// ---- composer ----
const agentBusy = () => D.state === 'working' || D.state === 'stalled';
function updateSend() {
  const b = $('#dr-send');
  const empty = !$('#dr-text').value.trim() && !D.files.length;
  const stop = empty && agentBusy();
  b.innerHTML = stop ? IC.stop : IC.up;
  updateCmds();
  b.classList.toggle('stop', stop);
  b.setAttribute('aria-label', stop ? 'Stop (Esc)' : 'Send');
  b.title = stop ? 'Stop (Esc)' : 'Send';
  b.disabled = D.sending || (empty && !stop);
}
function renderFiles() {
  const box = $('#dr-files-chips');
  box.hidden = !D.files.length;
  box.innerHTML = D.files.map((f, i) => f.url
    ? `<div class="fchip"><img src="${esc(f.url)}" alt=""><button type="button" data-rmfile="${i}" aria-label="Remove">${ic('xmark')}</button></div>`
    : `<div class="fchip doc">${fxBadge(f.file.name)}<span>${esc(f.file.name)}</span><small>${fmtB(f.file.size)}</small><button type="button" data-rmfile="${i}" aria-label="Remove">${ic('xmark')}</button></div>`).join('');
  updateSend();
}
function addFiles(list) {
  for (const file of list) {
    if (file.size > 25 << 20) { toast(`${file.name}: files up to 25 MB.`, true); continue; }
    if (D.files.length < 10) D.files.push({ file, url: /^image\/(png|jpeg|gif|webp)$/.test(file.type) ? URL.createObjectURL(file) : '' });
  }
  renderFiles();
}
function clearFiles() { for (const f of D.files) if (f.url) URL.revokeObjectURL(f.url); D.files = []; renderFiles(); }

const autosize = () => { const t = $('#dr-text'); t.style.height = 'auto'; t.style.height = Math.min(t.scrollHeight, 200) + 'px'; updateSend(); };
$('#dr-text').addEventListener('input', autosize);
$('#dr-text').addEventListener('keydown', (e) => {
  if (C.list.length && !e.isComposing) {
    const n = C.list.length;
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { e.preventDefault(); C.sel = (C.sel + (e.key === 'ArrowDown' ? 1 : n - 1)) % n; return updateCmds(); }
    if (e.key === 'Escape') { e.preventDefault(); C.shut = $('#dr-text').value; return updateCmds(); }
    const cmd = C.list[C.sel][0];
    if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey && $('#dr-text').value.trim() !== cmd)) { e.preventDefault(); return pickCmd(cmd); }
  }
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && !coarse) { e.preventDefault(); $('#dr-form').requestSubmit(); }
});

// slash commands: Claude Code's own, typed into the agent like any message
const CMDS = [
  ['/compact', 'Summarize the conversation to free up context'], ['/clear', 'Start a fresh conversation'],
  ['/context', 'What fills the context window'], ['/cost', 'Tokens and cost of this session'], ['/model', 'Switch model (pick it in Terminal)'],
  ['/review', 'Review a pull request'], ['/init', 'Write a CLAUDE.md for this repo'], ['/memory', 'Edit memory files'],
  ['/agents', 'Manage subagents'], ['/mcp', 'MCP servers and their tools'], ['/resume', 'Resume an earlier conversation'],
  ['/rewind', 'Rewind the code and the conversation'], ['/export', 'Export the conversation'], ['/status', 'Version, model and account'],
  ['/config', 'Settings'], ['/permissions', 'Allow and deny rules'], ['/hooks', 'Hook configuration'], ['/help', 'Every command'],
];
const C = { list: [], sel: 0, shut: null };
// the session's skills (project, yours, each plugin's, claude.ai's), fetched when a chat opens
async function loadSkills() {
  const key = D.key;
  const r = await api(`/api/pane/skills?session=${encodeURIComponent(D.session)}&pane=${D.pane}`).catch(() => ({}));
  if (key !== D.key) return;
  D.skills = r.status === 200 ? r.skills || [] : [];
  if (C.list.length) updateCmds();
}
const SRC_LABEL = { project: 'This project', yours: 'Your skills', 'claude.ai': 'From claude.ai', builtin: 'Claude Code' };
function updateCmds() {
  const v = $('#dr-text').value;
  const m = v.match(/^\/([\w:.-]*)$/);
  if (C.shut !== null && C.shut !== v) C.shut = null; // Esc closes the list until the text changes
  const q = (m?.[1] || '').toLowerCase();
  const all = (D.skills || []).map((s) => [s.cmd, s.desc, s.src]).concat(CMDS.map(([n, d]) => [n, d, 'builtin']));
  const hit = (x) => x[0].slice(1).toLowerCase().includes(q) || x[0].toLowerCase().split(':').pop().startsWith(q);
  C.list = m && C.shut === null && D.mode === 'chat'
    ? all.filter(hit).concat(q ? all.filter((x) => !hit(x) && x[1].toLowerCase().includes(q)) : []) : [];
  C.sel = Math.min(C.sel, Math.max(0, C.list.length - 1));
  const box = $('#dr-cmds');
  box.hidden = !C.list.length;
  $('#dr-text').setAttribute('aria-expanded', String(!!C.list.length));
  if (!C.list.length) { C.sel = 0; return; }
  let group = null;
  box.innerHTML = C.list.map(([n, d, src], i) => {
    const head = !q && src !== group ? `<div class="cmd-g">${esc(SRC_LABEL[src] || src.replace(/^\w/, (c) => c.toUpperCase()))}</div>` : ''; // grouped until you type
    group = src;
    return head + `<button type="button" class="cmd" role="option" tabindex="-1" aria-selected="${i === C.sel}" data-cmd="${esc(n)}" title="${esc(d)}"><b>${esc(n)}</b><span>${esc(d)}</span></button>`;
  }).join('') + `<div class="cmds-f">${coarse ? 'Tap to insert' : '↑↓ to choose · Tab to insert · Esc to close'}</div>`;
  box.querySelector('[aria-selected="true"]')?.scrollIntoView({ block: 'nearest' });
}
function pickCmd(n) {
  const t = $('#dr-text');
  t.value = n + ' ';
  t.focus();
  autosize();
}
$('#dr-cmds').addEventListener('pointerdown', (e) => e.preventDefault()); // keep the keyboard up
$('#dr-cmds').addEventListener('click', (e) => { const b = e.target.closest('[data-cmd]'); if (b) pickCmd(b.dataset.cmd); });
$('#dr-slash').addEventListener('click', () => {
  const t = $('#dr-text');
  if (!t.value.startsWith('/')) t.value = '/' + t.value.replace(/^\s+/, '');
  C.shut = null;
  t.focus();
  t.setSelectionRange(t.value.length, t.value.length);
  autosize();
});

// quote a passage (a selection, or a whole reply) into the composer
function quoteIntoComposer(text) {
  text = String(text || '').trim();
  if (!text) return;
  const t = $('#dr-text');
  const q = text.split('\n').map((l) => '> ' + l).join('\n') + '\n\n';
  t.value = (t.value.trim() ? t.value.replace(/\s*$/, '\n\n') : '') + q;
  getSelection()?.removeAllRanges();
  $('#selbar').hidden = true;
  t.focus();
  t.setSelectionRange(t.value.length, t.value.length);
  autosize();
}

// selection actions: highlight a passage of the chat to quote or copy it
function updateSelbar() {
  const bar = $('#selbar');
  const sel = getSelection();
  const body = $('#dr-body');
  if (!D.key || D.mode !== 'chat' || !sel || sel.isCollapsed || !sel.rangeCount || !body.contains(sel.getRangeAt(0).commonAncestorContainer) || !sel.toString().trim()) {
    bar.hidden = true;
    return;
  }
  const r = sel.getRangeAt(0).getBoundingClientRect();
  const box = body.getBoundingClientRect();
  if (r.bottom < box.top || r.top > box.bottom) { bar.hidden = true; return; }
  bar.hidden = false;
  const w = bar.offsetWidth;
  const h = bar.offsetHeight;
  bar.style.left = Math.max(8, Math.min(innerWidth - w - 8, r.left + r.width / 2 - w / 2)) + 'px';
  bar.style.top = (coarse || r.top - h - 8 < box.top ? Math.min(r.bottom + 8, innerHeight - h - 8) : r.top - h - 8) + 'px';
}
document.addEventListener('selectionchange', updateSelbar);
$('#dr-body').addEventListener('scroll', () => { if (!$('#selbar').hidden) updateSelbar(); updateDown(); }, { passive: true });
$('#selbar').addEventListener('pointerdown', (e) => e.preventDefault()); // keep the selection
$('#selbar').addEventListener('click', async (e) => {
  const b = e.target.closest('[data-sel]');
  if (!b) return;
  const text = getSelection().toString();
  if (b.dataset.sel === 'quote') return quoteIntoComposer(text);
  try { await navigator.clipboard.writeText(text); toast('Copied'); } catch { toast('Copy failed', true); }
  getSelection().removeAllRanges();
});
$('#dr-text').addEventListener('paste', (e) => {
  const files = [...(e.clipboardData?.files || [])];
  if (files.length) { e.preventDefault(); addFiles(files); }
});
$('#dr-attach').addEventListener('click', () => $('#dr-file').click());
$('#dr-file').addEventListener('change', (e) => { addFiles(e.target.files); e.target.value = ''; });
$('#dr-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const text = $('#dr-text').value.trim();
  if (!text && !D.files.length) { if (agentBusy()) sendInput({ keys: ['Esc'] }); return; }
  D.sending = true;
  updateSend();
  try {
    const paths = [];
    for (const f of D.files) paths.push(await uploadFile(f.file)); // agents get files by path, as if dropped into their terminal
    if (await sendInput({ text: [...paths, text].filter(Boolean).join(' '), enter: true })) {
      $('#dr-text').value = '';
      clearFiles();
      autosize();
    }
  } catch (err) { toast(err.message || 'Network error.', true); }
  finally { D.sending = false; updateSend(); }
});

// drag and drop anywhere in the drawer: chat attaches, terminal types the paths
let dragDepth = 0;
$('#drawer').addEventListener('dragenter', (e) => {
  if (![...(e.dataTransfer?.types || [])].includes('Files')) return;
  dragDepth++;
  $('#dr-drop-t').textContent = D.mode === 'chat' ? 'Drop files to attach' : 'Drop files to paste their paths';
  $('#dr-drop').hidden = false;
});
$('#drawer').addEventListener('dragleave', () => { if (--dragDepth <= 0) { dragDepth = 0; $('#dr-drop').hidden = true; } });
$('#drawer').addEventListener('dragover', (e) => { if ([...(e.dataTransfer?.types || [])].includes('Files')) e.preventDefault(); });
$('#drawer').addEventListener('drop', (e) => {
  dragDepth = 0;
  $('#dr-drop').hidden = true;
  const files = [...(e.dataTransfer?.files || [])];
  if (!files.length) return;
  e.preventDefault();
  if (D.mode === 'chat') addFiles(files); else pastePaths(files);
});

// ---- terminal keyboard: keystrokes go straight to the pane, batched every 25 ms ----
const T = { buf: '', busy: false, timer: 0, ctrl: false, alt: false, composing: false };
const SENT = '  '; // keeps backspace detectable on phone keyboards
const ti = $('#term-in');
function termSend(s) {
  if (!s || !D.key) return;
  T.buf += s;
  if (!T.timer && !T.busy) T.timer = setTimeout(termFlush, 25);
}
async function termFlush() {
  T.timer = 0;
  if (T.busy || !T.buf || !D.key) return;
  const data = T.buf.slice(0, 2000);
  T.buf = T.buf.slice(2000);
  T.busy = true;
  try {
    const r = await api('/api/pane/write', { session: D.session, pane: D.pane, data: b64utf8(data) });
    if (r.status === 401 && r.error === 'login required') { T.buf = ''; closePane(); showLogin('Session ended — enter your PIN.'); }
    else if (r.status !== 200) { T.buf = ''; toast(r.error || `Error ${r.status}`, true); }
  } catch { T.buf = ''; toast('Network error.', true); }
  finally { T.busy = false; if (T.buf) termFlush(); }
}
function setMod(m, on) { T[m] = on; $(`#dr-keys [data-tk="${m}"]`).setAttribute('aria-pressed', String(on)); }
function termType(s) {
  if (!s) return;
  if (T.ctrl || T.alt) {
    const c = s[0];
    s = (T.alt ? '\x1b' : '') + (T.ctrl ? (ctrlChar(c) ?? c) : c) + s.slice(1);
    setMod('ctrl', false);
    setMod('alt', false);
  }
  termSend(s.replace(/\n/g, '\r'));
}
function resetTi() { ti.value = SENT; ti.setSelectionRange(SENT.length, SENT.length); }
function termFocus() { resetTi(); ti.focus({ preventScroll: true }); }
ti.addEventListener('focus', () => $('#dr-term').classList.add('focused'));
ti.addEventListener('blur', () => $('#dr-term').classList.remove('focused'));
ti.addEventListener('keydown', (e) => {
  if (e.isComposing || e.keyCode === 229) return;
  const s = keyBytes({ key: e.key, ctrlKey: e.ctrlKey || T.ctrl, altKey: e.altKey || T.alt, metaKey: e.metaKey, shiftKey: e.shiftKey });
  if (s == null) return; // printable: arrives as beforeinput
  e.preventDefault();
  setMod('ctrl', false);
  setMod('alt', false);
  termSend(s);
});
ti.addEventListener('beforeinput', (e) => {
  const t = e.inputType;
  if (t === 'insertText' || t === 'insertReplacementText') { e.preventDefault(); termType(e.data || ''); }
  else if (t === 'insertLineBreak' || t === 'insertParagraph') { e.preventDefault(); termSend('\r'); }
  else if (t === 'deleteContentBackward') { e.preventDefault(); termSend('\x7f'); }
  else if (t === 'deleteContentForward') { e.preventDefault(); termSend('\x1b[3~'); }
});
ti.addEventListener('input', () => { // browsers that don't let beforeinput cancel: diff against the sentinel
  if (T.composing || ti.value === SENT) return;
  if (ti.value.length < SENT.length) termSend('\x7f');
  else termType(ti.value.startsWith(SENT) ? ti.value.slice(SENT.length) : ti.value);
  resetTi();
});
ti.addEventListener('compositionstart', () => { T.composing = true; });
ti.addEventListener('compositionend', (e) => { T.composing = false; termType(e.data || ''); resetTi(); });
ti.addEventListener('paste', (e) => {
  e.preventDefault();
  const files = [...(e.clipboardData?.files || [])];
  if (files.length) return pastePaths(files); // a pasted file becomes its path, as in a desktop terminal
  const text = e.clipboardData?.getData('text');
  if (text) sendInput({ text }); // bracketed paste, like a real terminal
});
$('#dr-term').addEventListener('click', () => { if (!getSelection().toString()) termFocus(); });

// Claude Code draws on the alternate screen and keeps its own history (zellij holds none for it), so the
// wheel or a finger over an agent's terminal scrolls Claude's view, as the mouse wheel does over SSH.
// Shell panes scroll the browser through zellij's history instead.
const agentTerm = () => { const f = D.key && findPane(D.session, D.pane); return !!f && (!!f.p.agent || /claude|codex/.test(f.p.kind || '')); };
let wheelAcc = 0;
function sendWheel(clientX, clientY) {
  const pre = $('#term-pre');
  const r = pre.getBoundingClientRect();
  const fs = parseFloat(pre.style.fontSize) || 13;
  const x = Math.max(1, Math.min(D.cols || 80, Math.floor((clientX - r.left) / (fs * 0.602)) + 1));
  const y = Math.max(1, Math.min(D.rows || 24, Math.floor((clientY - r.top) / (fs * 1.22)) + 1));
  for (let n = 0; Math.abs(wheelAcc) >= 36 && n < 12; n++) {
    termSend(`\x1b[<${wheelAcc < 0 ? 64 : 65};${x};${y}M`); // SGR mouse wheel up / down
    wheelAcc -= Math.sign(wheelAcc) * 36;
  }
}
$('#dr-term').addEventListener('wheel', (e) => {
  if (!agentTerm() || Math.abs(e.deltaY) < Math.abs(e.deltaX)) return;
  e.preventDefault();
  wheelAcc += e.deltaMode === 1 ? e.deltaY * 18 : e.deltaY;
  sendWheel(e.clientX, e.clientY);
}, { passive: false });
let touch = null;
$('#dr-term').addEventListener('touchstart', (e) => { touch = { x: e.touches[0].clientX, y: e.touches[0].clientY }; }, { passive: true });
$('#dr-term').addEventListener('touchmove', (e) => {
  if (!touch || !agentTerm()) return;
  const t = e.touches[0];
  const dy = touch.y - t.clientY;
  if (Math.abs(dy) < Math.abs(touch.x - t.clientX)) return; // sideways: let the page pan a wide pane
  e.preventDefault();
  wheelAcc += dy * 2;
  touch = { x: t.clientX, y: t.clientY };
  sendWheel(t.clientX, t.clientY);
}, { passive: false });
$('#dr-term').addEventListener('touchend', () => { touch = null; });
const TK = { esc: '\x1b', tab: '\t', up: '\x1b[A', down: '\x1b[B', left: '\x1b[D', right: '\x1b[C', cc: '\x03' };
function onTermKey(e) {
  const b = e.target.closest('[data-tk]');
  if (!b) return;
  e.preventDefault(); // keep the focus (and the phone keyboard) on the terminal
  const k = b.dataset.tk;
  if (k === 'ctrl' || k === 'alt') setMod(k, !T[k]);
  else if (k === 'kbd') document.activeElement === ti ? ti.blur() : termFocus();
  else termSend(TK[k]);
}
$('#dr-keys').addEventListener('touchstart', onTermKey, { passive: false });
$('#dr-keys').addEventListener('mousedown', onTermKey);
addEventListener('resize', () => { if (D.key && D.mode === 'screen') renderBody(); });

// ---- files the session created or edited ----
const fileQS = () => `session=${encodeURIComponent(D.session)}&pane=${D.pane}`;
async function loadFiles() {
  const key = D.key;
  const r = await api(`/api/pane/files?${fileQS()}`);
  if (key !== D.key) return;
  if (r.status === 200) { F.list = r.files || []; F.root = r.root || ''; }
  else F.list = [];
  $('#dr-filescnt').textContent = F.list.filter((a) => isDoc(a.path)).length || '';
  renderFilesPanel();
}
function openFilesPanel() {
  F.open = true;
  $('#dr-files').hidden = false;
  $('#dr-filesbtn').setAttribute('aria-pressed', 'true');
  clearInterval(F.timer);
  F.timer = setInterval(loadFiles, 10000);
  renderFilesPanel();
  loadFiles();
}
function closeFilesPanel() {
  F.open = false;
  clearInterval(F.timer);
  $('#dr-files').hidden = true;
  $('#dr-filesbtn').setAttribute('aria-pressed', 'false');
}

// a file opens as a full overlay over the chat or terminal (the Documents list stays where it was)
async function openFile(path) {
  F.path = path;
  F.file = null;
  $('#dr-preview').hidden = false;
  renderPreview();
  renderFilesPanel();
  const r = await api(`/api/pane/file?${fileQS()}&path=${encodeURIComponent(path)}`);
  if (F.path !== path) return;
  F.file = r.status === 200 ? r : { kind: 'error', error: r.error || (r.status === 404 ? 'Not one of this session’s files (or it was deleted).' : `Error ${r.status}`) };
  renderPreview();
}
function closePreview() {
  if (!F.path) return;
  Object.assign(F, { path: '', file: null });
  $('#dr-preview').hidden = true;
  $('#dr-preview').innerHTML = '';
  renderFilesPanel();
}
function fileBody(f, raw) {
  switch (f.kind) {
    case 'markdown': return `<div class="md pv-md">${md(f.text)}</div>`;
    case 'html': return `<iframe class="pv-frame" sandbox="allow-scripts allow-popups allow-popups-to-escape-sandbox" src="${raw}" title="HTML preview"></iframe>`;
    case 'image': return `<div class="pv-img"><img src="${raw}" alt=""></div>`;
    case 'text': return `<pre class="fp-code">${hl(f.text, extOf(F.path)).map((l) => `<span class="cl">${l}</span>`).join('\n')}</pre>`;
    case 'binary': return `<div class="empty">Binary file (${fmtB(f.size)}). <a href="${raw}" download>Download</a></div>`;
    default: return `<div class="empty err">${esc(f.error || 'Could not open the file.')}</div>`;
  }
}
function renderPreview() {
  if (!F.path) return;
  const raw = `/api/pane/file?${fileQS()}&raw=1&path=${encodeURIComponent(F.path)}`;
  const name = F.path.split('/').pop();
  const dir = relPath(F.path).replace(/[^/]*$/, '');
  const f = F.file;
  $('#dr-preview').innerHTML = `<header class="pv-h"><button type="button" class="icon-btn ghost sm" data-pclose aria-label="Back to the chat" title="Back (Esc)">${ic('angle-left')}</button>` +
    `${fxBadge(name)}<div class="pv-t"><b title="${esc(F.path)}">${esc(name)}</b><span>${esc(dir)}</span></div>` +
    `<button type="button" class="btn sm" data-fcopy>${ic('copy')}<span>Copy path</span></button>` +
    `<a class="btn sm" href="${raw}" target="_blank" rel="noopener">${ic('arrow-up-right')}<span>Open</span></a>` +
    `<a class="btn sm" href="${raw}" download>${ic('download2')}<span>Download</span></a>` +
    `<button type="button" class="icon-btn ghost sm" data-pclose aria-label="Close preview" title="Close (Esc)">${IC.x}</button></header>` +
    `<div class="pv-view${f?.kind === 'html' ? ' frame' : ''}">${f ? fileBody(f, raw) + (f.truncated ? '<div class="note pv-note">Showing the first 1 MB.</div>' : '') : `<div class="empty">${shim('Loading…')}</div>`}</div>`;
}
function renderFilesPanel() {
  if (!F.open) return;
  const box = $('#dr-files');
  const docs = (F.list || []).filter((a) => isDoc(a.path));
  const html = docs.filter((a) => isHTML(a.path));
  const mds = docs.filter((a) => !isHTML(a.path));
  const list = F.filter === 'html' ? html : F.filter === 'md' ? mds : docs;
  const item = (a) => `<button type="button" class="fp-i${a.exists ? '' : ' gone'}${a.path === F.path ? ' on' : ''}" data-openfile="${esc(a.path)}"><span class="fp-ic">${fxBadge(a.path)}</span>` +
    `<span class="fp-b"><span class="fp-t">${esc(a.path.split('/').pop())}</span><span class="fp-d fp-dir">${esc((a.inTree ? relPath(a.path) : tilde(a.path)).replace(/[^/]*$/, '') || './')}</span><span class="fp-d">${a.mentioned ? 'Shared in chat' : (a.created ? 'Created' : 'Edited') + (a.edits > 1 ? ` · ${a.edits}×` : '') + ` · <span class="add">+${a.add}</span> <span class="del">−${a.del}</span>`} · ${ago(a.last)}${a.exists ? '' : ' · deleted'}</span></span></button>`;
  const scroll = $('.fp-list', box)?.scrollTop || 0;
  box.innerHTML = `<div class="fp-h">${ic('folder-open', 'fp-hi')}<b>Documents</b><span class="muted">${F.root ? esc(F.root.split('/').pop()) : ''}</span><span class="spacer"></span>` +
    `<button type="button" class="icon-btn ghost sm" data-fclose aria-label="Close files">${IC.x}</button></div>` +
    `<div class="fp-tabs">${[['all', 'All', docs], ['html', 'HTML', html], ['md', 'Markdown', mds]].map(([k, l, xs]) =>
      `<button type="button" data-ffilter="${k}" aria-pressed="${F.filter === k}">${l} <span>${xs.length}</span></button>`).join('')}</div>` +
    `<div class="fp-list">${F.list === null ? `<div class="empty">${shim('Reading the session…')}</div>` : list.map(item).join('') || '<div class="empty">No HTML or Markdown files from this session yet.</div>'}</div>`;
  $('.fp-list', box).scrollTop = scroll; // the list refreshes every 10 s: stay where you were
}

$('#drawer').addEventListener('click', async (e) => {
  if (e.target.closest('a[download], a[target]')) return; // a link (download, open) does its own thing
  const pk = e.target.closest('[data-pick]');
  if (pk) { D.pick = { sig: pk.closest('[data-sig]')?.dataset.sig, label: pk.dataset.pick }; for (const b of $$('#dr-prompt .ap-o')) b.setAttribute('aria-checked', String(b === pk)); return; }
  const c = e.target.closest('[data-choose], [data-skip]');
  if (c) { // one answer per question: the buttons come back when the card changes (or after 6 s)
    const btns = $$('#dr-prompt .ap-f button');
    for (const b of btns) b.disabled = true;
    const ok = c.dataset.skip !== undefined ? await sendInput({ keys: ['Esc'] })
      : await sendInput({ choose: D.pick?.label || '', sig: c.closest('[data-sig]')?.dataset.sig || '' });
    setTimeout(() => { for (const b of btns) b.disabled = false; }, ok ? 6000 : 0);
    return;
  }
  const go = e.target.closest('[data-go]');
  if (go) { $('#dr-switch').hidden = true; $('#dr-switch-btn').setAttribute('aria-expanded', 'false'); if (go.dataset.go !== D.key) openPane(go.dataset.go); return; }
  if (e.target.closest('#dr-switch-btn')) {
    if ($('#dr-rail').offsetWidth > 0) return; // the rail already lists everything
    const sw = $('#dr-switch');
    sw.hidden = !sw.hidden;
    $('#dr-switch-btn').setAttribute('aria-expanded', String(!sw.hidden));
    return;
  }
  const m = e.target.closest('[data-mode]');
  if (m && !m.disabled && m.dataset.mode !== D.mode) {
    D.mode = m.dataset.mode;
    D.view = null;
    renderDrawer();
    if (D.mode === 'screen') { $('#term-pre').textContent = ''; $('#term-sb').textContent = ''; Object.assign(D, { sb: [], sbDirty: false, termFresh: true }); if (!coarse) termFocus(); }
    else { $('#dr-body').innerHTML = `<div class="chat"><div class="chat-empty">${shim('Loading the conversation…')}</div></div>`; D.seen = 0; }
    return connectPane();
  }
  if (e.target.closest('#dr-close')) return closePane();
  if (e.target.closest('[data-saclose]')) return closeSub();
  const so = e.target.closest('[data-sub]');
  if (so) return openSub(so.dataset.sub);
  const st = e.target.closest('[data-subtool]');
  if (st) { e.preventDefault(); return openSub('', st.dataset.subtool); } // not the row's open/close
  const rl = e.target.closest('.rl-hide, #dr-railbtn');
  if (rl) return setRail(rl.id !== 'dr-railbtn');
  if (e.target.closest('#dr-model')) return toggleModels();
  const mo = e.target.closest('[data-setmodel], [data-seteffort]');
  if (mo) return setModel(mo.dataset.setmodel, mo.dataset.seteffort);
  if (e.target.closest('#dr-filesbtn')) return F.open ? closeFilesPanel() : openFilesPanel();
  if (e.target.closest('[data-files]')) return openFilesPanel();
  if (e.target.closest('[data-pclose]')) return closePreview();
  const of = e.target.closest('[data-openfile]');
  if (of) return openFile(of.dataset.openfile);
  if (e.target.closest('[data-fclose]')) return closeFilesPanel();
  const ff = e.target.closest('[data-ffilter]');
  if (ff) { F.filter = ff.dataset.ffilter; return renderFilesPanel(); }
  const copy = async (text, btn) => {
    try {
      await navigator.clipboard.writeText(text);
      const was = btn.innerHTML;
      btn.classList.add('done');
      if (btn.querySelector('use')) btn.querySelector('use').setAttribute('href', '#i-copy-success');
      setTimeout(() => { btn.classList.remove('done'); btn.innerHTML = was; }, 1200);
    } catch { toast('Copy failed', true); }
  };
  if (e.target.closest('[data-fcopy]')) return copy(F.path, e.target.closest('[data-fcopy]'));
  const its = e.target.closest('#dr-sa') ? SA.items : D.items; // the main chat's, or the subagent's over it
  const ci = e.target.closest('[data-copyi]');
  if (ci) return copy(its[+ci.dataset.copyi]?.text || '', ci);
  const qi = e.target.closest('[data-quotei]');
  if (qi) { closeSub(); return quoteIntoComposer(its[+qi.dataset.quotei]?.text || ''); }
  const cid = e.target.closest('[data-copyid]');
  if (cid) { const r = its.find((i) => i.k === 'result' && i.id === cid.dataset.copyid); return copy(r?.text || '', cid); }
  const rm = e.target.closest('[data-rmfile]');
  if (rm) { const [f] = D.files.splice(+rm.dataset.rmfile, 1); if (f?.url) URL.revokeObjectURL(f.url); return renderFiles(); }
  const cp = e.target.closest('.cb-copy');
  if (cp) return copy(cp.closest('.cb').querySelector('code').textContent, cp);
  if (e.target.closest('[data-more]')) return loadEarlier();
});
// earlier messages: fetched when the top of the chat comes near, kept in place while they land
async function loadEarlier() {
  if (D.loadingMore || D.before <= 0 || !D.key) return;
  const key = D.key;
  D.loadingMore = true;
  renderBody();
  try {
    const r = await api(`/api/pane/chat?session=${encodeURIComponent(D.session)}&pane=${D.pane}&before=${D.before}`);
    if (key !== D.key) return;
    if (r.status !== 200) { toast(r.error || `Error ${r.status}`, true); return; }
    const body = $('#dr-body');
    const fromBottom = body.scrollHeight - body.scrollTop;
    D.items = (r.items || []).concat(D.items);
    D.seen += (r.items || []).length;
    D.before = r.before || 0;
    D.loadingMore = false;
    renderBody(true);
    body.scrollTop = body.scrollHeight - fromBottom;
  } finally { if (key === D.key) { D.loadingMore = false; } }
}
const moreObs = new IntersectionObserver((es) => { if (es.some((e) => e.isIntersecting)) loadEarlier(); }, { root: $('#dr-body'), rootMargin: '600px 0px 0px 0px' });

$('#dr-body').addEventListener('keydown', (e) => { if (e.key === 'Enter' && e.target.matches('.fcard')) openFile(e.target.dataset.openfile); });
for (const el of [$('#dr-body'), $('#sa-body')]) el.addEventListener('toggle', (e) => {
  const id = e.target.dataset?.id;
  if (!id) return;
  if (!e.target.open) { D.openTools.delete(id); delete e.target.dataset.drop; return; }
  D.openTools.add(id);
  markOpened('d:' + id);
  e.target.dataset.drop = OPENED.get('d:' + id); // its contents drop in
  e.target.style.setProperty('--dd', '0ms');
}, true);
addEventListener('keydown', (e) => {
  if (e.key !== 'Escape' || !D.key || e.target.closest('textarea, dialog')) return;
  if (!$('#dr-switch').hidden) $('#dr-switch').hidden = true;
  else if (F.path) closePreview();
  else if (SA.open) closeSub();
  else if (F.open) closeFilesPanel();
  else closePane();
});

// ---------- menus: close a session, tab, pane or the process running in it ----------
function findTab(session, tab) {
  const s = (S.data.term?.sessions || []).find((x) => x.name === session);
  return { s, tb: s?.tabs.find((t) => t.id === tab) };
}
const agentsIn = (panes) => panes.filter((p) => p.agent || p.kind === 'claude' || p.kind === 'codex');
const shortCmd = (c) => { const t = String(c || '').split(' ')[0].split('/').pop(); return t.length > 24 ? t.slice(0, 24) + '…' : t; };

function openMenu(btn, kind, session, tab, pane) {
  const items = [];
  const item = (icon, label, data, danger) => items.push(`<button type="button" role="menuitem" class="mi${danger ? ' danger' : ''}" ${Object.entries(data).map(([k, v]) => `data-${k}="${esc(v)}"`).join(' ')}>${ic(icon)}<span>${esc(label)}</span></button>`);
  if (kind === 'session') {
    item('plus', 'New tab', { mi: 'new', new: 'tab', session });
    item('trash2', 'Close session…', { mi: 'close', what: 'session', session }, true);
  } else if (kind === 'tab') {
    const { tb } = findTab(session, tab);
    item('row-horizontal', 'New pane in this tab', { mi: 'new', new: 'pane', session, tab, tabname: tb?.name || '' });
    item('close-square', 'Close tab…', { mi: 'close', what: 'tab', session, tab }, true);
  } else {
    const f = findPane(session, pane);
    if (!f) return;
    if (D.key !== `${session}:${pane}`) item('arrow-up-right', 'Open', { mi: 'open', key: `${session}:${pane}` });
    item('row-horizontal', 'New pane in this tab', { mi: 'new', new: 'pane', session, tab: f.tb.id, tabname: f.tb.name });
    if (f.p.fg) item('power', `Kill ${shortCmd(f.p.proc)}…`, { mi: 'close', what: 'process', session, pane, pid: f.p.fg }, true);
    item('xmark', 'Close pane…', { mi: 'close', what: 'pane', session, pane }, true);
    if (f.tb.panes.length > 1) item('close-square', 'Close tab…', { mi: 'close', what: 'tab', session, tab: f.tb.id }, true);
    // closing the whole session lives on the session card only, never in a pane's menu
  }
  const m = $('#menu');
  m.hidden = true;
  void m.offsetWidth; // a menu opened from another one still pops in
  m.innerHTML = items.join('');
  m.hidden = false;
  const r = btn.getBoundingClientRect();
  const w = m.offsetWidth;
  const h = m.offsetHeight;
  m.style.left = Math.max(8, Math.min(r.right - w, innerWidth - w - 8)) + 'px';
  const up = r.bottom + 4 + h > innerHeight - 8;
  m.style.top = (up ? Math.max(8, r.top - h - 4) : r.bottom + 4) + 'px';
  m.style.transformOrigin = `${up ? 'bottom' : 'top'} right`;
}
const closeMenu = () => { $('#menu').hidden = true; };
addEventListener('pointerdown', (e) => { if (!$('#menu').hidden && !e.target.closest('#menu, [data-menu], #dr-menu')) closeMenu(); }, true);
addEventListener('scroll', closeMenu, true);
addEventListener('resize', closeMenu);
addEventListener('keydown', (e) => { if (e.key === 'Escape' && !$('#menu').hidden) { e.stopImmediatePropagation(); closeMenu(); } }, true);

$('#menu').addEventListener('click', (e) => {
  const b = e.target.closest('[data-mi]');
  if (!b) return;
  closeMenu();
  const d = b.dataset;
  if (d.mi === 'new') return openNew(d);
  if (d.mi === 'open') return openPane(d.key);
  if (d.mi === 'close') openClose(d.what, d.session, d.tab === undefined ? null : +d.tab, d.pane === undefined ? null : +d.pane, d.pid === undefined ? null : +d.pid);
});
$('#dr-menu').addEventListener('click', (e) => { const f = D.key && findPane(D.session, D.pane); if (f) openMenu(e.currentTarget, 'pane', D.session, f.tb.id, D.pane); });

function openClose(what, session, tab, pane, pid) {
  const names = (ps) => ps.slice(0, 4).map((p) => p.agent?.title || p.title || 'pane ' + p.id).join(', ') + (ps.length > 4 ? '…' : '');
  let title;
  let msg;
  let body;
  let gone; // does it take the open drawer's pane with it?
  if (what === 'session') {
    const s = (S.data.term?.sessions || []).find((x) => x.name === session);
    const panes = (s?.tabs || []).flatMap((t) => t.panes);
    const ag = agentsIn(panes);
    title = `Close session ${session}?`;
    msg = `Ends all ${plural(panes.length, 'pane')} in ${plural(s?.tabs.length || 0, 'tab')} and everything running in them` +
      (ag.length ? `, including ${plural(ag.length, 'Claude agent')} (${names(ag)})` : '') + '. The session is deleted, not kept for resurrection.';
    body = { what, session };
    gone = () => D.session === session;
  } else if (what === 'tab') {
    const { tb } = findTab(session, tab);
    const panes = tb?.panes || [];
    const ag = agentsIn(panes);
    title = `Close tab ${tb?.name || tab}?`;
    msg = `Ends its ${plural(panes.length, 'pane')} and what runs in them` + (ag.length ? `, including ${plural(ag.length, 'Claude agent')} (${names(ag)})` : '') + '.';
    body = { what, session, tab };
    gone = () => D.session === session && panes.some((p) => p.id === D.pane);
  } else if (what === 'pane') {
    const f = findPane(session, pane);
    title = `Close pane ${f ? (f.tb.panes.length > 1 ? f.p.title || 'pane ' + pane : f.tb.name) : pane}?`;
    msg = `Ends ${f?.p.proc ? `“${f.p.proc}”` : 'its shell'}${f && agentsIn([f.p]).length ? ' and the Claude agent in it' : ''}.` + (f && f.tb.panes.length === 1 ? ' It is the only pane, so the tab closes too.' : '');
    body = { what, session, pane };
    gone = () => D.session === session && D.pane === pane;
  } else {
    const f = findPane(session, pane);
    title = `Kill ${shortCmd(f?.p.proc)}?`;
    msg = `${f?.p.proc || 'pid ' + pid}\n\nSIGTERM to pid ${pid} now, SIGKILL after 5 s if it's still running. The pane stays open.`;
    body = { what, session, pane, pid };
    gone = () => false;
  }
  pending = { url: '/api/term/close', body, done: title.replace(/^Close/, 'Closed').replace(/^Kill/, 'Killed').replace(/\?$/, ''),
    after: () => { if (gone()) closePane(); } };
  modalIcon(what === 'process' ? 'power' : what === 'session' ? 'trash2' : 'xmark');
  $('#modal-title').textContent = title;
  $('#modal-msg').textContent = msg;
  $('#modal-ok').textContent = what === 'process' ? 'Kill' : 'Close';
  $('#modal-ok').className = 'btn danger';
  $('#modal-err').textContent = '';
  $('#modal').showModal();
}

// ---------- create session / tab / pane ----------
let NEW = null;
function openNew(d) {
  NEW = { what: d.new, session: d.session || '', tab: +(d.tab || 0) };
  $('#new-title').textContent = d.new === 'session' ? 'New zellij session' : d.new === 'tab' ? `New tab in ${d.session}` : `New pane in ${d.tabname}`;
  $('#new-name').value = '';
  $('#new-name').required = d.new === 'session';
  $('#new-name').placeholder = d.new === 'session' ? 'e.g. scratch' : 'optional';
  fillDirs(d);
  $('#new-run').value = store.get('newRun', 'shell');
  $('#new-dir-f').hidden = d.new !== 'pane';
  $('#new-err').textContent = '';
  $('#newdlg').showModal();
  $('#new-name').focus();
}
// where to start it: a list, not a text box: the folders terminals use now, repos and their worktrees, projects
let DIRS = null;
const paneDir = (p) => (p ? (p.git?.root ? tilde(p.git.root) : p.agent?.cwd || '') : '');
async function fillDirs(d) {
  const sel = $('#new-cwd-sel');
  const tab = d.new === 'pane' ? findTab(d.session, +d.tab).tb : null;
  const sess = (S.data.term?.sessions || []).find((s) => s.name === d.session);
  const want = paneDir(tab?.panes[0]) || paneDir(sess?.tabs?.[0]?.panes?.[0]) || store.get('newCwd', '~');
  const inUse = [...new Set((S.data.term?.sessions || []).flatMap((s) => (s.tabs || []).flatMap((t) => t.panes.map(paneDir))).filter(Boolean))];
  const render = () => {
    const groups = [['In use', inUse]];
    for (const x of DIRS || [{ path: '~', group: 'Home' }]) {
      if (inUse.includes(x.path)) continue;
      const g = groups.find(([n]) => n === x.group);
      if (g) g[1].push(x.path); else groups.push([x.group, [x.path]]);
    }
    if (![...groups.flatMap(([, xs]) => xs)].includes(want)) groups[0][1].unshift(want);
    sel.innerHTML = groups.filter(([, xs]) => xs.length).map(([g, xs]) => `<optgroup label="${esc(g)}">${xs.map((p) => `<option value="${esc(p)}">${esc(p)}</option>`).join('')}</optgroup>`).join('') +
      '<option value="__other">Other…</option>';
    sel.value = want;
    $('#new-cwd-f').hidden = true;
  };
  render();
  if (!DIRS) {
    const r = await api('/api/dirs').catch(() => ({}));
    if (r.status === 200) { DIRS = r.dirs || []; if ($('#newdlg').open) render(); }
  }
}
$('#new-cwd-sel').addEventListener('change', (e) => {
  const other = e.target.value === '__other';
  $('#new-cwd-f').hidden = !other;
  if (other) { $('#new-cwd').value = ''; $('#new-cwd').focus(); }
});

$('#new-cancel').addEventListener('click', () => $('#newdlg').close());
$('#update').addEventListener('click', reloadNow);
$('#new-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  if (!NEW) return;
  const ok = $('#new-ok');
  ok.disabled = true;
  const sel = $('#new-cwd-sel').value;
  const body = { ...NEW, name: $('#new-name').value.trim(), cwd: sel === '__other' ? $('#new-cwd').value.trim() : sel, run: $('#new-run').value, dir: NEW.what === 'pane' ? $('#new-dir').value : '' };
  try {
    const r = await api('/api/term/new', body);
    if (r.status === 200) {
      store.set('newCwd', body.cwd || '~');
      store.set('newRun', body.run);
      $('#newdlg').close();
      toast(`Created ${NEW.what}${body.name ? ' ' + body.name : ''}`);
      S.openWhenSeen = `${r.session}:${r.pane}`;
    } else $('#new-err').textContent = r.error || `Error ${r.status}`;
  } catch { $('#new-err').textContent = 'Network error.'; }
  finally { ok.disabled = false; }
});

// ---------- Security ----------
const ICON = { ok: ic('check-circle-f'), warn: ic('alert-triangle'), bad: ic('x-circle-f') };

function viewSec(s) {
  const checks = s.checks || [];
  const bad = checks.filter((c) => c.level === 'bad').length;
  const warn = checks.filter((c) => c.level === 'warn').length;
  const list = checks.map((c) => `<div class="check ${esc(c.level)}"><div class="ic">${ICON[c.level] || ""}</div><div><div class="t">${esc(c.title)}</div><div class="d">${esc(c.detail)}</div></div></div>`).join('');
  const localBy = {};
  for (const l of (s.listens || []).filter((l) => !l.public)) (localBy[l.proc || '?'] ||= []).push(l.port);
  const listens = (s.listens || []).filter((l) => l.public).map((l) =>
    `<div class="li"><span class="pill warn">exposed</span>
     <div class="main"><div class="t mono">${esc(l.proto)} ${esc(l.addr.includes(':') ? `[${l.addr}]` : l.addr)}:${l.port}</div><div class="d">${esc(l.proc || '?')}${l.pid ? ' · pid ' + l.pid : ''}</div></div></div>`).join('') +
    Object.entries(localBy).sort((a, b) => b[1].length - a[1].length).map(([proc, ports]) =>
    `<div class="li"><span class="pill ok">local</span>
     <div class="main"><div class="t">${esc(proc)}</div><div class="d mono wrap">${ports.length} port${ports.length > 1 ? 's' : ''}: ${ports.sort((a, b) => a - b).join(', ')}</div></div></div>`).join('');
  const outbound = (s.outbound || []).map((o) =>
    `<div class="li"><div class="main"><div class="t mono">${esc(o.addr)}</div><div class="d">${esc(o.procs || '?')}</div></div><div class="side">${o.n} conn</div></div>`).join('');
  const inbound = (s.inbound || []).map((c) =>
    `<div class="li"><span class="pill warn">inbound</span><div class="main"><div class="t mono">${esc(c.remote)} → :${c.localPort}</div><div class="d">${esc(c.proc || '?')}</div></div></div>`).join('');
  const keys = (s.keys || []).map((k) =>
    `<div class="li"><div class="main"><div class="t">${esc(k.comment || k.type)}</div><div class="d mono">${esc(k.type)} ${esc(k.fp)}</div></div></div>`).join('');
  const loose = (s.loose || []).map((p) => `<div class="li"><div class="main"><div class="t mono">${esc(p)}</div></div></div>`).join('');
  const audit = (s.audit || []).map((e) => {
    const cls = /fail|lockout|reject/.test(e.event) ? 'bad' : e.event === 'action' ? 'busy' : 'ok';
    return `<div class="li"><span class="pill ${cls}">${esc(e.event)}</span><div class="main"><div class="t">${esc(e.detail || '')}</div><div class="d">${ago(e.t)}${e.ip ? ' · ' + esc(e.ip) : ''}</div></div></div>`;
  }).join('');
  const tiles = '<div class="grid">' +
    tile('Status', bad ? 'At risk' : warn ? 'Review' : 'Healthy', `${checks.length - bad - warn} ok · ${warn} to review · ${bad} critical`, '', bad ? 'bad' : warn ? 'warn' : 'ok', 'shield-check') +
    tile('Exposed ports', (s.listens || []).filter((l) => l.public).length, `${(s.listens || []).length} listening in total`, '', '', 'plug') +
    tile('Outbound hosts', (s.outbound || []).length, `${(s.inbound || []).length} inbound public`, '', '', 'global') +
    tile('Loose secrets', (s.loose || []).length, 'world-readable credential files', '', (s.loose || []).length ? 'warn' : 'ok', 'key') +
    '</div>';
  return tiles +
    `<div>${sec('Checks', 'refreshed every minute', '', 'shield-check')}<div class="card flush">${list}</div></div>` +
    `<div class="cols"><div>${sec('Listening ports', '', '', 'plug')}<div class="card flush"><div class="list">${listens || '<div class="empty">None</div>'}</div></div>` +
    `<div class="note">"Exposed" means bound beyond localhost. Whether the internet can reach it is decided by the Cube/Fly edge.</div></div>` +
    `<div>${sec('Connections', 'public peers', '', 'global')}<div class="card flush"><div class="list">${inbound}${outbound || (inbound ? '' : '<div class="empty">None</div>')}</div></div></div></div>` +
    `<div class="cols"><div>${sec('World-readable secrets', (s.loose || []).length ? 'fix: chmod 600 &lt;file&gt;' : '', '', 'key')}<div class="card flush"><div class="list">${loose || '<div class="empty">None — credential files are private.</div>'}</div></div></div>` +
    `<div>${sec('SSH authorized keys', '', '', 'fingerprint')}<div class="card flush"><div class="list">${keys || '<div class="empty">No authorized_keys</div>'}</div></div></div></div>` +
    `<div>${sec('Panel audit log', 'logins, wrong PINs, actions', '', 'receipt-text')}<div class="card flush"><div class="list">${audit || '<div class="empty">No events yet</div>'}</div></div></div>`;
}

const VIEWS = { res: viewRes, ci: viewCI, term: viewTerm, sec: viewSec };

// ---------- interactions ----------
$('#view').addEventListener('click', (e) => {
  if (limitClick(e)) return;
  const tog = e.target.closest('[data-toggle]');
  if (tog) { S[tog.dataset.toggle] = !S[tog.dataset.toggle]; return render(); }
  const sort = e.target.closest('[data-sort]');
  if (sort) { S.sort = sort.dataset.sort; S.sortedBy = null; store.set('sort', S.sort); return render(); }
  const act = e.target.closest('[data-act]');
  if (act) return openAction(act.dataset);
  const mb = e.target.closest('[data-menu]');
  if (mb) return openMenu(mb, mb.dataset.menu, mb.dataset.session, mb.dataset.tab === undefined ? null : +mb.dataset.tab, mb.dataset.pane === undefined ? null : +mb.dataset.pane);
  const nw = e.target.closest('[data-new]');
  if (nw) return openNew(nw.dataset);
  const subs = e.target.closest('[data-subs]');
  if (subs) {
    const k = subs.dataset.subs;
    if (S.subsOpen.has(k)) S.subsOpen.delete(k); else { S.subsOpen.add(k); markOpened('s:' + k); }
    return render();
  }
  const os = e.target.closest('[data-opensub]');
  if (os) { const [k, id] = os.dataset.opensub.split('|'); openPane(k); return D.key === k && openSub(id); }
  const pane = e.target.closest('[data-open]');
  if (pane) return openPane(pane.dataset.open);
  const row = e.target.closest('[data-expand]');
  if (row) {
    const k = row.dataset.expand;
    if (S.open.has(k)) { S.open.delete(k); S.procOrder.delete(k); } else { S.open.add(k); markOpened('p:' + k); }
    render();
  }
});
$('#view').addEventListener('keydown', (e) => {
  if ((e.key === 'Enter' || e.key === ' ') && e.target.matches('[data-expand], [data-open], [data-opensub]')) { e.preventDefault(); e.target.click(); }
});

let pending = null;
const modalIcon = (name) => { $('#modal-ic').innerHTML = ic(name); };
function openAction(d) {
  modalIcon(d.act === 'kill' ? 'power' : 'arrow-rotate');
  if (d.act === 'kill') {
    pending = { name: 'kill', body: { pid: +d.pid }, done: `Sent SIGTERM to ${d.pid}` };
    $('#modal-title').textContent = `Kill process ${d.pid}?`;
    $('#modal-msg').textContent = `${d.cmd}\n\nIt gets SIGTERM now and SIGKILL after 5 s if it's still running.`;
    $('#modal-ok').textContent = 'Kill';
    $('#modal-ok').className = 'btn danger';
  } else if (d.act === 'restart') {
    pending = { name: 'restart-runner', body: { runner: d.runner }, done: `Runner ${d.runner} restarted` };
    $('#modal-title').textContent = `Restart runner ${d.runner}?`;
    $('#modal-msg').textContent = d.busy ? `It's running "${d.busy}" — that job will be cancelled.` : 'It is idle; it will reconnect to GitHub in a few seconds.';
    $('#modal-ok').textContent = 'Restart';
    $('#modal-ok').className = 'btn danger';
  } else return;
  $('#modal-err').textContent = '';
  $('#modal').showModal();
}

$('#modal-cancel').addEventListener('click', () => $('#modal').close());
$('#modal-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  if (!pending) return;
  const after = pending.after;
  const ok = $('#modal-ok');
  ok.disabled = true;
  try {
    const r = await api(pending.url || '/api/action/' + pending.name, pending.body);
    if (r.status === 200) {
      const done = pending.done;
      $('#modal').close();
      toast(done);
      if (after) after();
    } else if (r.status === 401 && r.error === 'login required') {
      $('#modal').close();
      showLogin('Session ended — enter your PIN.');
    } else {
      $('#modal-err').textContent = r.error || `Error ${r.status}`;
    }
  } catch { $('#modal-err').textContent = 'Network error.'; }
  finally { ok.disabled = false; }
});
$('#modal').addEventListener('close', () => { pending = null; });

let toastTimer;
function toast(msg, bad) {
  const t = $('#toast');
  t.innerHTML = ic(bad ? 'alert-triangle' : 'check-circle-f') + `<span>${esc(msg)}</span>`;
  t.className = 'toast' + (bad ? ' bad' : '');
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.classList.add('out'); toastTimer = setTimeout(() => { t.hidden = true; }, 180); }, 3500);
}

// ---------- boot ----------
(async () => {
  try {
    const r = await api('/api/me');
    if (r.status === 200) startApp();
    else showLogin(r.status === 401 ? '' : r.error || '');
  } catch { showLogin('Network error — retrying…'); setTimeout(() => location.reload(), 5000); }
})();
