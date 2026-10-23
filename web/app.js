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

// iOS ignores user-scalable=no: block its pinch gestures so the home-screen app never zooms (see app.css)
for (const ev of ['gesturestart', 'gesturechange']) document.addEventListener(ev, (e) => e.preventDefault(), { passive: false });
// while a text field has focus (the phone's keyboard is up) the bottom nav steps aside (app.css: body.typing)
const typingIn = (el) => !!el?.matches?.('textarea, [contenteditable], input:not([type=checkbox], [type=radio], [type=file], [type=button], [type=submit])');
// typing in a chat (a terminal's in the drawer, or JARVIS's): <html> says so, for the aurora under the keyboard
// (app.css html.kbglow; WebKit doesn't restyle the root for a :has() deep in the page)
const kbGlow = () => document.documentElement.classList.toggle('kbglow', document.body.classList.contains('typing') &&
  (document.body.classList.contains('drawer-open') ? !!document.querySelector('#drawer.chatmode') && !!document.activeElement?.closest('#drawer') : !!document.activeElement?.closest('#ai')));
document.addEventListener('focusin', (e) => { if (typingIn(e.target)) document.body.classList.add('typing'); kbGlow(); });
document.addEventListener('focusout', () => setTimeout(() => { document.body.classList.toggle('typing', typingIn(document.activeElement)); kbGlow(); }, 0));
// the keyboard, as the visual viewport says: --vv-top (how far iOS has shifted what you see, to show the field),
// --kb (how much of the page's viewport the keyboard covers; 0 where iOS ends the viewport at the keyboard, as the
// home-screen app does) and --kb-y (the keyboard's top edge in page coordinates: what's under the translucent
// keyboard is the page itself). A chat in the drawer follows them while you type (app.css): its top stays where
// you see, its chat box sits on the keyboard, and the aurora's glow runs on under it. Laid out by the page itself,
// so a re-render that makes iOS drop its shift can't leave the chat box behind the keyboard.
const vv = globalThis.visualViewport;
function fitKeyboard() {
  const top = Math.max(0, Math.round(vv.offsetTop)), kb = Math.max(0, Math.round(document.documentElement.clientHeight - vv.height - vv.offsetTop));
  const vars = [['--vv-top', top], ['--kb', kb]];
  // --kb-y moves on every scroll and a root variable restyles the whole page: kept only while typing, its one use
  if (document.body.classList.contains('typing')) vars.push(['--kb-y', Math.round((vv.pageTop ?? scrollY + vv.offsetTop) + vv.height)]); // the keyboard's top edge
  const st = document.documentElement.style;
  for (const [k, v] of vars) if (st.getPropertyValue(k) !== v + 'px') st.setProperty(k, v + 'px');
}
if (vv) {
  vv.addEventListener('resize', fitKeyboard); vv.addEventListener('scroll', fitKeyboard); addEventListener('scroll', fitKeyboard, { passive: true });
  // iOS doesn't always say when its keyboard has settled (another field focused with it up, a chat opened under
  // it): measure again through its slide-in
  document.addEventListener('focusin', (e) => { if (typingIn(e.target)) { fitKeyboard(); for (const ms of [0, 80, 200, 400, 700, 1100]) setTimeout(fitKeyboard, ms); } });
  fitKeyboard();
}

// ---------- state ----------
const TITLES = { ai: 'J.A.R.V.I.S', res: 'Resources', ci: 'CI', term: 'Terminals', files: 'Files', browser: 'Browser', sec: 'Security' };
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
async function api(path, body, sent) { // sent: the fetch for path, already under way
  const opts = body === undefined
    ? { credentials: 'same-origin' }
    : { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) };
  const r = await (sent || fetch(path, opts));
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
  window.brLeave?.(); // the Browser's socket too: nothing of the page stays live behind the PIN screen
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
  es.onmessage = (e) => { if (!got) S.stalls = 0; got = true; onMessage(JSON.parse(e.data)); };
  setTimeout(() => { // nothing in 10 s (stuck between here and the server): reconnect; say so only when that stalls too
    if (S.es !== es || got) return;
    const again = (S.stalls = (S.stalls || 0) + 1) > 1;
    window.cpReport?.('stall', `no ${S.view} data after 10 s (stream state ${es.readyState})${again ? ', again after a reconnect' : ', reconnecting'}`, '', !again);
    if (!again) connect();
  }, 10000);
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

// ---- updates: a page on an older build says so and offers a reload; it never reloads by itself (that would
// cost you your place, a half-typed message, a scroll). The reload reopens the chat or terminal that was open.
function checkBuild(v) {
  if (!v) return;
  if (!S.build) { S.build = v; return; }
  if (v !== S.build) $('#update').hidden = false;
}
function reloadNow() {
  try { sessionStorage.setItem('cp-reopen', JSON.stringify({ key: D.key, mode: D.mode })); } catch { /* no storage: the list comes back instead */ }
  location.reload();
}

function onMessage(m) {
  checkBuild(m.v);
  S.badges = m.badges;
  renderChrome();
  if (m.view !== S.view || m.view === 'ai' || m.view === 'files' || m.view === 'browser') return; // the assistant's chat has its own stream (assistant.js); Files and the Browser ask for what they show (files.js, browser.js)
  if (m.view === 'res') {
    const h = m.data.hist || [];
    S.hist = S.fresh ? h : S.hist.concat(h).slice(-360);
    S.fresh = false;
    S.data.res = m.data.res;
  } else {
    S.data[m.view] = m.data;
    if (m.view === 'term') prefetchPRs(m.data);
  }
  // Re-rendering mid-tap swaps the element under the finger and the click is lost: wait for the release.
  if (held()) S.stale = true;
  else render();
}

// A finger is down, a scroll (a phone's fling too) is still moving, or something is being dragged: re-render once it's over.
const held = () => Date.now() - S.heldAt < 2000 || Date.now() - S.scrollAt < 350 || S.dragging; // S.dragging: a drag on the Terminals page
function flushStale() {
  if (held()) return;
  if (S.stale) { S.stale = false; render(); }
  if (D.stale) { D.stale = false; renderBody(); }
  if (SA.stale) renderSub();
  if (typeof AI !== 'undefined' && AI.stale) aiRender(); // assistant.js: a tap while the page loads can come first
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
  if (v !== 'ai') aiLeave();
  if (v !== 'browser') window.brLeave?.();
  S.view = v;
  store.set('view', v);
  document.body.classList.toggle('view-files', v === 'files');
  document.body.classList.toggle('view-browser', v === 'browser');
  // Files and the Browser fold the sidebar to its icons for the room; your own setting comes back on the other pages
  const mini = v === 'files' || v === 'browser' || store.get('navMini') === '1';
  if (document.body.classList.contains('nav-mini') !== mini) setNavMini(mini);
  $('#title').textContent = TITLES[v];
  document.title = TITLES[v] + ' · Control Panel';
  for (const b of $$('.nav-btn')) {
    const on = b.dataset.view === v, u = $('use', b), name = u.getAttribute('href').slice(3).replace(/-au$/, '');
    if (on) b.setAttribute('aria-current', 'page');
    else b.removeAttribute('aria-current');
    u.setAttribute('href', '#i-' + (on ? auIcon(name) : name)); // the current tab's icon wears the aurora
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

// a write only when it changes something: rewriting the same text still repaints (and a frosted panel re-blurs)
const setText = (el, v) => { if (el.textContent !== v) el.textContent = v; };
const setAttr = (el, k, v) => { if (el.getAttribute(k) !== v) el.setAttribute(k, v); };
const setHidden = (el, v) => { if (el.hidden !== v) el.hidden = v; };
const setDisabled = (el, v) => { if (el.disabled !== v) el.disabled = v; };
function badge(name, text, cls) { // every update: only a change touches the page (a rewrite repaints the sidebar)
  const el = $(`[data-badge="${name}"]`), t = String(text || ''), c = 'badge ' + (cls || '');
  if (el.textContent !== t) el.textContent = t;
  if (el.className !== c) el.className = c;
}

function renderChrome() {
  const b = S.badges;
  if (!b) return;
  badge('ci', b.ciFail ? b.ciFail : b.ciBusy ? b.ciBusy : '', b.ciFail ? 'bad' : 'busy');
  badge('term', b.waiting ? b.waiting : b.working ? b.working : '', b.waiting ? 'warn' : 'busy');
  badge('sec', b.secBad || '', 'bad');
  badge('browser', b.agentBrowsers || '', 'busy'); // agents' browsers open now (Browser → Agents)
  noteBadge(b);
  const stats = b.waiting ? `<span class="wt">waiting <b>${b.waiting}</b></span>` : ''; // CPU and RAM live on Resources
  if ($('#top-stats').dataset.html !== stats) { $('#top-stats').innerHTML = stats; $('#top-stats').dataset.html = stats; }
}

// ---------- morph: a live view's next second patches the page in place instead of rebuilding it ----------
// Kept elements keep hover, focus and their CSS transitions (bars glide to their new size). Children with
// a data-k (else an id) are matched by it and may move; the rest match by position and tag. An element marked
// data-own belongs to the code that put it there: once present it is left alone.
const keyOf = (n) => (n.nodeType === 1 ? n.getAttribute('data-k') || n.id : '');
const TWEENS = [];
function morph(el, html) {
  const t = document.createElement('template');
  t.innerHTML = html;
  morphKids(el, t.content);
}
function morphKids(a, b) {
  const keyed = new Map();
  for (const n of a.childNodes) { const k = keyOf(n); if (k) keyed.set(k, n); }
  const want = [...b.childNodes];
  want.forEach((nb, i) => {
    const at = a.childNodes[i] || null;
    const k = keyOf(nb);
    let old = k ? keyed.get(k) : at && !keyOf(at) ? at : null;
    if (k) keyed.delete(k);
    if (!old || old.nodeName !== nb.nodeName) { a.insertBefore(nb, at); return; }
    if (old !== at) a.insertBefore(old, at);
    patch(old, nb);
  });
  while (a.childNodes.length > want.length) a.lastChild.remove();
}
function patch(a, b) {
  if (a.nodeType !== 1) { if (a.nodeValue !== b.nodeValue) a.nodeValue = b.nodeValue; return; }
  if (b.hasAttribute('data-own')) return;
  if (a.dataset.tw && a.dataset.v !== b.dataset.v) TWEENS.push([a, +a.dataset.v, +b.dataset.v]);
  for (const { name, value } of b.attributes) if (a.getAttribute(name) !== value) a.setAttribute(name, value);
  for (const { name } of [...a.attributes]) { // style is the CSSOM's (sizes); a canvas sizes itself
    if (!b.hasAttribute(name) && name !== 'style' && !(a.nodeName === 'CANVAS' && (name === 'width' || name === 'height'))) a.removeAttribute(name);
  }
  if (a.nodeName === 'TEXTAREA' || (a.nodeName === 'INPUT' && a === document.activeElement)) return;
  morphKids(a, b);
}
// numbers that roll to their new value: <span data-tw="pct" data-v="51.2">51%</span>
const FMT = { pct: (v) => fmtPct(v), load: (v) => v.toFixed(2) };
const tw = (f, v) => `<span data-tw="${f}" data-v="${v}">${FMT[f](v)}</span>`;
const rolling = new WeakMap();
function runTweens() {
  for (const [el, from, to] of TWEENS.splice(0)) {
    const f = FMT[el.dataset.tw];
    const cur = rolling.get(el);
    const start = cur ? cur.at : from; // mid-roll: carry on from what's on screen
    const t0 = performance.now();
    const st = { at: start };
    rolling.set(el, st);
    el.textContent = f(start);
    const step = (t) => {
      if (rolling.get(el) !== st) return;
      const k = Math.min(1, (t - t0) / 500);
      st.at = start + (to - start) * (1 - (1 - k) ** 3);
      el.textContent = f(k < 1 ? st.at : to);
      if (k < 1) requestAnimationFrame(step); else rolling.delete(el);
    };
    requestAnimationFrame(step);
  }
}
const calm = matchMedia('(prefers-reduced-motion: reduce)');
// FLIP: rows of a [data-flip] list slide from where they were to where the update put them; new ones fade in
function flipFrom(root) {
  const at = new Map();
  for (const n of root.querySelectorAll('[data-flip] > [data-k]')) { const r = n.getBoundingClientRect(); at.set(n, [r.left, r.top]); }
  return () => {
    if (calm.matches) return;
    for (const n of root.querySelectorAll('[data-flip] > [data-k]')) {
      const was = at.get(n);
      if (was === undefined) { if (!n.hasAttribute('data-drop')) n.animate([{ opacity: 0 }, { opacity: 1 }], { duration: 260, easing: 'ease-out' }); continue; }
      const r = n.getBoundingClientRect(), dx = was[0] - r.left, dy = was[1] - r.top; // across too: session cards sit in two columns
      if (Math.abs(dy) > 1 || Math.abs(dx) > 1) n.animate([{ transform: `translate(${dx}px, ${dy}px)` }, { transform: 'none' }], { duration: 380, easing: 'cubic-bezier(.2, .8, .2, 1)' });
    }
  };
}

// ---------- render ----------
function render() {
  const el = $('#view');
  const d = S.view === 'res' ? S.data.res : S.data[S.view];
  const kept = $$('[data-keep-scroll]', el).map((x) => [x.dataset.keepScroll, x.scrollTop]); // inner lists: don't jump to the top every update
  const same = d && el.dataset.drawn === S.view; // the same view's next update: patch it; a new view starts clean
  const flip = same && flipFrom(el);
  keepScroll([window], () => {
    if (same) morph(el, VIEWS[S.view](d));
    else el.innerHTML = d ? VIEWS[S.view](d) : '<div class="grid">' + '<div class="skel"></div>'.repeat(4) + '</div><div class="skel"></div>';
  });
  el.dataset.drawn = d ? S.view : '';
  if (flip) flip();
  if (calm.matches) TWEENS.length = 0; else runTweens();
  const ts = $('#tok-slot', el);
  if (ts) { if (!tokEl.firstChild) renderTokens(); ts.replaceWith(tokEl); }
  if (S.view === 'term' && Date.now() - TOK.at > 60000) loadTokens();
  for (const [k, top] of kept) { const x = $(`[data-keep-scroll="${k}"]`, el); if (x) x.scrollTop = top; }
  // CSP forbids inline style attributes: sizes go through the CSSOM
  for (const i of $$('[data-w]', el)) i.style.width = Math.min(100, Math.max(0, +i.dataset.w)) + '%';
  for (const i of $$('[data-h]', el)) i.style.height = Math.min(100, Math.max(0, +i.dataset.h)) + '%';
  for (const c of $$('canvas[data-spark]', el)) drawSpark(c);
  applyDrops(el);
  if (S.view === 'term') termAfter(el);
  if (S.view === 'ai') aiMount(el);
  if (S.view === 'files') fmMount(el);
  if (S.view === 'browser') brMount(el);
  if (S.view === 'term') {
    if (S.openWhenSeen && findPane(...splitKey(S.openWhenSeen))) {
      openPane(S.openWhenSeen);
      S.openWhenSeen = null;
      if (S.reopenMode && S.reopenMode !== D.mode) { D.mode = S.reopenMode; D.view = D.raw = null; renderDrawer(); connectPane(); }
      S.reopenMode = null;
    }
    renderDrawer();
  }
}

// Live views re-render every second or two, and a rebuilt element restarts its animations: the shimmer,
// spinners, pulsing dots and the pixel loader would jump back to their start each time. Pinning every
// looping animation to the page clock makes a rebuilt element continue exactly where the old one was.
const LOOPS = new Set(['t-shimmer', 'pixel-on', 'spin', 'pulse', 'blink', 'curblink', 'ld-bounce', 'ld-wave']);
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
  `<div class="card tile ${cls}"><div class="l">${icon ? icAu(icon) : ''}${esc(label)}</div><div class="v">${value}</div><div class="s">${sub}</div>${extra}</div>`;
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
  const AU = { green: '#3fe9ae', blue: '#5c9dff', violet: '#b07bff' }; // the aurora's (app.css --au-*)
  if (key === 'memh') return drawMemHour(g, w, h, AU);
  const series = key === 'net' ? [['rx', AU.green], ['tx', AU.violet]] : [[key, ...(key === 'mem' ? [AU.blue, AU.violet] : [AU.green, AU.blue])]];
  if (pts.length < 2) return;
  let max = key === 'net' ? 1 : 100;
  if (key === 'net') for (const p of pts) max = Math.max(max, p.rx, p.tx);
  for (const [f, color, to] of series) {
    g.beginPath();
    pts.forEach((p, i) => {
      const x = (i / (pts.length - 1)) * w;
      const y = h - 2 - (Math.min(p[f], max) / max) * (h - 4);
      i ? g.lineTo(x, y) : g.moveTo(x, y);
    });
    let line = color;
    if (to) { line = g.createLinearGradient(0, 0, w, 0); line.addColorStop(0, color); line.addColorStop(1, to); }
    g.strokeStyle = line; g.lineWidth = 1.5; g.lineJoin = 'round'; g.stroke();
    if (series.length === 1) {
      g.lineTo(w, h); g.lineTo(0, h); g.closePath();
      const grad = g.createLinearGradient(0, 0, 0, h);
      grad.addColorStop(0, (to || color) + '55'); grad.addColorStop(1, (to || color) + '00');
      g.fillStyle = grad; g.fill();
    }
  }
}
window.addEventListener('resize', () => { for (const c of $$('canvas[data-spark]')) drawSpark(c); });

// Memory, last hour: apps, page cache and kernel stacked on 0..MemTotal by time, GB gridlines, a dot on the apps peak
function drawMemHour(g, w, h, AU) {
  const pts = S.hist, total = S.data.res?.memTotal, GB = 2 ** 30;
  if (!pts.length || !total) return;
  const t1 = pts[pts.length - 1].t;
  const X = (t) => w - ((t1 - t) / 3600) * w, Y = (b) => h - (b / total) * h;
  let below = pts.map(() => 0);
  for (const [f, col] of [['apps', AU.green], ['cache', AU.blue], ['kern', AU.violet]]) {
    const top = pts.map((p, i) => below[i] + (p[f] || 0));
    g.beginPath(); // a lineTo with no current point moves there
    pts.forEach((p, i) => g.lineTo(X(p.t), Y(top[i])));
    for (let i = pts.length - 1; i >= 0; i--) g.lineTo(X(pts[i].t), Y(below[i]));
    g.fillStyle = col + '38'; g.fill();
    g.beginPath();
    pts.forEach((p, i) => g.lineTo(X(p.t), Y(top[i])));
    g.strokeStyle = col; g.lineWidth = 1.5; g.lineJoin = 'round'; g.stroke();
    below = top;
  }
  const step = [1, 2, 5, 10, 20, 50, 100, 200, 500, 1000].find((s) => total / GB / s <= 5) * GB;
  g.font = '10.5px system-ui, -apple-system, sans-serif'; g.fillStyle = '#5b636f'; g.strokeStyle = 'rgba(255, 255, 255, .07)'; g.lineWidth = 1;
  for (let b = step; b < total; b += step) {
    const y = Math.round(Y(b)) + 0.5;
    g.beginPath(); g.moveTo(0, y); g.lineTo(w, y); g.stroke();
    g.fillText(b / GB + ' GB', 4, y - 4);
  }
  const pk = pts.reduce((a, p) => (p.apps > a.apps ? p : a));
  g.beginPath(); g.arc(X(pk.t), Y(pk.apps), 3.5, 0, 7); g.fillStyle = '#e7e9ec'; g.fill();
}

// ---------- Resources ----------
const SORTS = { cpu: (g) => g.cpu, ram: (g) => g.rss, io: (g) => g.rd + g.wr, net: (g) => g.conns };

// ---------- balancer ----------
function limitSummary(l) {
  return [l.cores?.length ? (l.cores.length === 1 ? 'core ' : 'cores ') + l.cores.join(', ') : '',
    l.nice === 10 ? 'low priority' : l.nice === 19 ? 'lowest priority' : '', l.ioIdle ? 'disk idle' : '', l.paused ? 'paused' : ''].filter(Boolean).join(' · ');
}
const ruleOf = (key) => (S.data.res?.limits || []).find((l) => l.key === key);
// the CI reserve (limits.go restKey): the top cores kept for CI runners while a job runs
const ciKept = (ncpu) => (ruleOf('*')?.cores?.length ? ncpu - ruleOf('*').cores.length : 0);

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
  const t = e.target.closest('[data-core], [data-nice], [data-io], [data-lapply], [data-lremove], [data-lpause], [data-cikeep]');
  if (!t) return false;
  if (t.dataset.cikeep !== undefined) {
    const n = +t.dataset.cikeep, ncpu = (S.data.res?.cores || []).length;
    if (n) sendLimit({ key: '*', label: 'Everything but CI', cores: [...Array(ncpu - n).keys()] }, `${n} cores kept for CI`);
    else if (ruleOf('*')) sendLimit({ key: '*', remove: true }, 'CI shares every core again');
    return true;
  }
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
    tile('CPU', tw('pct', r.cpu), `${cores.length} cores · load ${r.load[0].toFixed(2)}`, '<canvas class="spark" data-spark="cpu"></canvas>', lvl(r.cpu), 'cpu') +
    tile('Memory', tw('pct', memPct), `${fmtB(memUsed)} of ${fmtB(r.memTotal)}`, '<canvas class="spark" data-spark="mem"></canvas>', lvl(memPct), 'ram') +
    tile('Network', `<span class="n">↓ ${fmtRate(r.netRx)}</span>`, `↑ ${fmtRate(r.netTx)} · all interfaces`, '<canvas class="spark" data-spark="net"></canvas>', '', 'global') +
    tile('Load', tw('load', r.load[0]), `${r.load[1].toFixed(2)} · ${r.load[2].toFixed(2)} · up ${fmtDur(r.uptime)}`,
      `<div class="note">${r.nprocs} processes</div>`, r.load[0] > cores.length ? 'warn' : '', 'gauge') +
    '</div>';

  // where the RAM is (sys.go memParts), and whether anything waits for it (PSI: % of time stalled)
  const ofMem = (b) => (b / Math.max(r.memTotal, 1)) * 100;
  const psi = r.psi || {};
  const press = !(psi.some10 >= 1) ? ['ok', 'No pressure'] : psi.some10 < 10 ? ['warn', 'Some pressure'] : ['bad', 'Under pressure'];
  const parts = [['apps', 'Apps', r.anon, 'What programs hold. This is the part that fills up.'],
    ['cache', 'Page cache', r.cache, 'Files read recently. Handed back the moment apps need it.'],
    ['kern', 'Kernel', r.slab, `The kernel's own tables and caches, ${fmtB(r.sreclaim)} of it reclaimable.`],
    ['shm', 'Shared', r.shmem, 'tmpfs and shared memory.'],
    ['free', 'Free', r.memFree, 'Not used for anything yet.']];
  const memCard = `<div class="card mem"><div class="mem-h"><b class="n">${fmtB(r.memAvail)}</b><span class="muted">available of ${fmtB(r.memTotal)}</span><span class="spacer"></span><span class="pill ${press[0]}">${press[1]}</span></div>` +
    `<div class="mbar">${parts.map(([k, , b]) => `<i class="m-${k}" data-w="${ofMem(b)}"></i>`).join('')}</div>` +
    `<div class="mleg">${parts.map(([k, name, b, what]) => `<div><i class="swatch m-${k}"></i><b>${name}</b><span class="n">${fmtB(b)}</span><span class="faint">${what}</span></div>`).join('')}</div>` +
    `<div class="mfoot"><div><b>Swap</b><span class="n">${r.swapTotal ? `${fmtB(swapUsed)} of ${fmtB(r.swapTotal)}` : 'None'}</span>${r.swapTotal ? bar(swapPct, lvl(swapPct)) : ''}</div>` +
    `<div><b>Pressure</b><span class="n">${[psi.some10, psi.some60, psi.some300].map(fmtPct).join(' · ')}</span><span class="faint">of time stalled · 10 s, 1 min, 5 min</span></div>` +
    `<div><b>Dirty</b><span class="n">${fmtB(r.dirty)}</span><span class="faint">waiting to be written</span></div></div></div>`;

  // the last hour (S.hist, every 10 s): drawn by drawMemHour; the peaks named here
  const hs = S.hist, hm = (t) => new Date(t * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  const pk = hs.reduce((a, p) => (p.apps > a.apps ? p : a), hs[0]);
  const pp = hs.reduce((a, p) => (p.psi > a.psi ? p : a), hs[0]);
  const hour = `<div class="card mh"><div class="mh-cap">${pk ? `<span><i class="swatch dot"></i>Apps peak ${fmtB(pk.apps)} · ${hm(pk.t)}</span>` : 'Collecting…'}</div>` +
    '<canvas class="mh-c" data-spark="memh"></canvas><div class="mh-x"><span>60 min ago</span><span>30 min</span><span>now</span></div>' +
    `<div class="mh-leg">${parts.slice(0, 3).map(([k, name]) => `<span><i class="swatch m-${k}"></i>${name}</span>`).join('')}<span class="spacer"></span>` +
    `<span>${pp && pp.psi >= 0.05 ? `Pressure peaked at ${fmtPct(pp.psi)} at ${hm(pp.t)}` : 'Pressure stayed at 0 all hour'}</span></div></div>`;

  // who holds it: groups by RSS, and how that changed since an hour ago (sys.go rssBase; null until then)
  const MB100 = 100 * 2 ** 20;
  const byRam = [...(r.groups || [])].sort((a, b) => b.rss - a.rss), rest = byRam.slice(6);
  const grew = byRam.filter((g) => g.rss1h > MB100).sort((a, b) => b.rss1h - a.rss1h)[0];
  const urow = (label, sub, rss, d) => `<div class="urow"><div class="un"><b title="${esc(label)}">${esc(label)}</b><span class="faint">${sub}</span></div>${bar(ofMem(rss))}` +
    `<span class="n r">${fmtB(rss)}</span><span class="n r ${d > MB100 ? 'warn' : d < -MB100 ? 'ok' : 'faint'}">${d == null ? '–' : (d < 0 ? '−' : '+') + fmtB(Math.abs(d))}</span></div>`;
  const users = byRam.slice(0, 6).map((g) => urow(g.label, g.n === 1 ? '1 process' : `${g.n} processes`, g.rss, g.rss1h)).join('') +
    (rest.length ? urow('Everything else', `${rest.length} groups`, rest.reduce((n, g) => n + g.rss, 0), byRam[0].rss1h == null ? null : rest.reduce((n, g) => n + g.rss1h, 0)) : '');
  const who = `<div class="card">${grew ? `<div class="uins">${esc(grew.label)} grew ${fmtB(grew.rss1h)} this hour, the most of any group.</div>` : ''}<div class="ulist">${users}</div></div>` +
    '<div class="note">RSS, grouped like Processes. Shared pages count in every process, so groups can add up to more than Apps.</div>';

  const coreGrid = cores.map((c, i) =>
    `<div class="core" title="core ${i}: ${fmtPct(c)}"><i data-h="${c}"></i><span>${Math.round(c)}</span></div>`).join('');
  const kept = ciKept(cores.length);
  const ciSeg = `<span class="faint">Keep for CI</span><span class="seg sm" role="group" aria-label="Cores kept for CI">${[0, 2, 4, 6, 8].filter((n) => n < cores.length)
    .map((n) => `<button type="button" data-cikeep="${n}" aria-pressed="${kept === n}">${n || 'Off'}</button>`).join('')}</span>`;
  const ciNote = `<div class="note">${kept ? `While a CI job runs, cores ${cores.length - kept}–${cores.length - 1} are CI's alone and everything else runs on 0–${cores.length - kept - 1}. When CI is idle, all ${cores.length} are everyone's.`
    : 'CI shares every core with everything else.'} RAM can't be split (no cgroups for this user).</div>`;

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
    let out = `<div class="prow" role="button" tabindex="0" data-k="g:${esc(g.key)}" data-expand="${esc(g.key)}" aria-expanded="${open}">
      <div class="pname">${caret}<b title="${esc(g.label)}">${esc(g.label)}</b>${g.n > 1 ? `<span class="cnt">×${g.n}</span>` : ''}${ruleOf(g.key) ? `<span class="lim${ruleOf(g.key).paused ? ' paused' : ''}">${ruleOf(g.key).paused ? 'paused' : 'limited'}</span>` : ''}</div>
      <div class="r">${fmtPct(g.cpu)}</div><div class="r">${fmtB(g.rss)}</div>
      <div class="r hide-sm">${fmtRate(g.rd + g.wr)}</div><div class="r hide-sm">${g.conns || ''}</div>
      <i class="load" data-w="${(g.cpu / maxCPU) * 100}"></i></div>`;
    if (open) {
      const procs = keepOrder(S.procOrder.get(g.key) || [], g.procs, (p) => p.pid);
      S.procOrder.set(g.key, procs.map((p) => p.pid));
      out += procs.map((p) => `<div class="psub" data-k="p:${p.pid}"${dropAttr('p:' + g.key)}>
        <div class="cmd"><span class="pid mono">${p.pid}</span><span class="mono ellipsis" title="${esc(p.cmd)}">${esc(p.cmd)}</span></div>
        <div class="r">${fmtPct(p.cpu)}</div><div class="r">${fmtB(p.rss)}</div>
        <div class="r hide-sm">${fmtRate(p.rd + p.wr)}</div>
        <div class="r"><button class="btn sm danger" data-act="kill" data-pid="${p.pid}" data-cmd="${esc(p.cmd)}">${ic('power')}Kill</button></div></div>`).join('');
      if (g.n > g.procs.length) out += `<div class="psub" data-k="m:${esc(g.key)}"${dropAttr('p:' + g.key)}><div class="faint">+ ${g.n - g.procs.length} more (lower CPU)</div></div>`;
      out += limitPanel(g, cores.length).replace('<div class="limit">', `<div class="limit" data-k="l:${esc(g.key)}"${dropAttr('p:' + g.key)}>`);
    }
    return out;
  }).join('');

  const limits = r.limits || [];
  const limitCard = limits.length ? `<div>${sec('Limits', `${limits.length} active · re-applied every 10 s`, '', 'tuning')}<div class="card flush"><div class="list">${limits.map((l) =>
    `<div class="li"><div class="main"><div class="t">${esc(l.label)}</div><div class="d">${esc(limitSummary(l))}</div></div>
     <button type="button" class="btn sm" data-lremove="${esc(l.key)}">${ic('xmark')}Remove</button></div>`).join('')}</div></div></div>` : '';
  return tiles + limitCard +
    `<div class="cols stretch"><div>${sec('Memory', '', '', 'ram')}${memCard}</div>` +
    `<div>${sec('Cores', `${cores.length} × logical`, ciSeg, 'microchip')}<div class="card fill"><div class="cores">${coreGrid}</div></div>${ciNote}</div></div>` +
    `<div class="res-split"><div>${sec('Processes', 'grouped by app, agent pane or CI runner', chips, 'activity')}
      <div class="card flush"><div class="ptable" data-flip><div class="phead"><div>App</div><div class="r">CPU</div><div class="r">RAM</div><div class="r hide-sm">Disk I/O</div><div class="r hide-sm">Conns</div></div>${rows}</div></div>
      <div class="note">CPU is % of one core. Per-process bandwidth needs root, so network shows open connections instead.</div></div>` +
    `<div class="res-side"><div>${sec('Memory, last hour', 'every 10 s', '', 'history')}${hour}</div>` +
    `<div>${sec('Who’s using memory', 'and how it changed this hour', '', 'layers')}${who}</div></div></div>`;
}
// ---------- token use: the agents' tokens in the last 24 h, and a graph of the last 7 or 30 days ----------
// One element that survives the Terminals view's re-renders (like the disk explorer below), so the graph keeps
// its hover while the view ticks. /api/tokens is asked at most once a minute; it sends the month by the hour
// and the page cuts that into your own days.
const TOK = { data: null, at: 0, busy: false, range: +store.get('tokRange', '7') === 30 ? 30 : 7, days: [] };
const tokEl = document.createElement('div');
tokEl.className = 'card tile tok';
tokEl.dataset.k = 'tokens'; // the view's morph leaves it be (its slot is data-own)
async function loadTokens() {
  if (TOK.busy) return;
  TOK.busy = true;
  TOK.at = Date.now();
  try {
    const r = await api('/api/tokens');
    if (r.status === 200) {
      if (r.ready) TOK.data = r;
      else TOK.at -= 50000; // the first scan ever is still reading the month: look again in 10 s
      renderTokens();
    }
  } catch { /* the next look tries again */ }
  TOK.busy = false;
}
function tokDays(n) { // your last n days, oldest first: [day, tokens]
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const days = Array.from({ length: n }, (_, i) => { const d = new Date(today); d.setDate(today.getDate() - (n - 1 - i)); return [d, 0]; });
  for (const [h, a, b, c, e] of TOK.data?.hours || []) {
    const d = new Date(h * 3600000);
    d.setHours(0, 0, 0, 0);
    const i = Math.round((d - days[0][0]) / 86400000); // a DST day is 23 or 25 h
    if (i >= 0 && i < n) days[i][1] += a + b + c + e;
  }
  return days;
}
const tokDay = (d, today) => (+d === +today ? 'Today' : d.toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short' }));
function renderTokens() {
  const d = TOK.data;
  const from = Math.floor((Date.now() / 1000 - 86400) / 3600);
  const u = [0, 0, 0, 0];
  for (const [h, ...x] of d?.hours || []) if (h > from) x.forEach((v, i) => { u[i] += v; });
  const total = u[0] + u[1] + u[2] + u[3];
  const days = TOK.days = tokDays(TOK.range);
  const W = 600, H = 120, max = Math.max(1, ...days.map((x) => x[1]));
  const pts = days.map(([, v], i) => [(i / (days.length - 1)) * W, H - 4 - (v / max) * (H - 12)]);
  const line = pts.map(([x, y], i) => `${i ? 'L' : 'M'}${x.toFixed(1)} ${y.toFixed(1)}`).join(' ');
  const most = d?.agents?.[0]?.tokens || 1;
  const agents = (d?.agents || []).slice(0, 4).map((a) => `<div class="tok-ag"><span title="${esc(a.label)}">${esc(a.label)}</span><i><b data-w="${(a.tokens / most) * 100}"></b></i><em>${fmtTok(a.tokens)}</em></div>`).join('');
  const sum = days.reduce((n, x) => n + x[1], 0), today = days[days.length - 1][0];
  tokEl.innerHTML = `<div class="l">${icAu('bolt')}Tokens · last 24 h<span class="spacer"></span>` +
    `<span class="seg sm" role="group" aria-label="Graph range">${[7, 30].map((n) => `<button type="button" data-tkrange="${n}" aria-pressed="${TOK.range === n}">${n}D</button>`).join('')}</span></div>` +
    `<div class="tok-body"><div class="tok-now"><div class="v">${d ? fmtTok(total) : '–'}</div>` +
    `<div class="s">${d ? `${fmtTok(u[0])} in · ${fmtTok(u[1])} out · ${fmtTok(u[2])} cache written · ${fmtTok(u[3])} cache read` : 'Counting the transcripts…'}</div>` +
    (agents ? `<div class="tok-ags">${agents}</div>` : '') + '</div>' +
    `<div class="tok-graph"><div class="tok-cap"><span>${fmtTok(sum)} in ${TOK.range} days</span><span>peak ${fmtTok(max === 1 ? 0 : max)}</span></div>` +
    `<div class="tok-chart"><svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" aria-hidden="true"><defs><linearGradient id="tok-fill" x1="0" y1="0" x2="0" y2="1"><stop class="tok-f1" offset="0"/><stop class="tok-f2" offset="1"/></linearGradient></defs>` +
    `<path d="${line} L${W} ${H} L0 ${H} Z" fill="url(#tok-fill)"/><path d="${line}" class="tok-line" vector-effect="non-scaling-stroke"/></svg>` +
    '<i class="tok-guide" hidden></i><i class="tok-dot" hidden></i><span class="tok-tip" hidden></span></div>' +
    `<div class="tok-x"><span>${esc(tokDay(days[0][0], today))}</span><span>${esc(tokDay(days[Math.floor(days.length / 2)][0], today))}</span><span>Today</span></div></div></div>`;
  for (const b of $$('[data-w]', tokEl)) b.style.width = Math.max(1, +b.dataset.w) + '%';
  tokEl.ys = pts.map(([, y]) => y / H); // for the hover dot
}
tokEl.addEventListener('click', (e) => {
  const b = e.target.closest('[data-tkrange]');
  if (!b || +b.dataset.tkrange === TOK.range) return;
  TOK.range = +b.dataset.tkrange;
  store.set('tokRange', String(TOK.range));
  renderTokens();
});
// the day under the pointer (or finger): a guide, a dot on the line and its total
tokEl.addEventListener('pointermove', (e) => {
  const c = e.target.closest('.tok-chart');
  if (!c || !TOK.days.length) return;
  const r = c.getBoundingClientRect(), n = TOK.days.length;
  const i = Math.max(0, Math.min(n - 1, Math.round(((e.clientX - r.left) / r.width) * (n - 1))));
  const x = (i / (n - 1)) * 100, today = TOK.days[n - 1][0];
  const [guide, dot, tip] = [$('.tok-guide', c), $('.tok-dot', c), $('.tok-tip', c)];
  guide.hidden = dot.hidden = tip.hidden = false;
  guide.style.left = dot.style.left = x + '%';
  dot.style.top = tokEl.ys[i] * 100 + '%';
  tip.textContent = `${tokDay(TOK.days[i][0], today)} · ${fmtTok(TOK.days[i][1])}`;
  tip.style.left = Math.max(12, Math.min(88, x)) + '%';
});
tokEl.addEventListener('pointerleave', () => { for (const x of $$('.tok-guide, .tok-dot, .tok-tip', tokEl)) x.hidden = true; });

// ---------- disk usage: a CleanMyMac-style explorer over the server's cached du index ----------
// It lives in one element that survives the Resources view's re-renders, so hover, scroll and the
// folder you're in stay put while the numbers above it tick.
const DK = { path: '', data: null, busy: false, err: '', hot: null, timer: 0, gone: null };
const diskEl = document.createElement('div');
diskEl.className = 'disk-wrap';
diskEl.dataset.k = 'disk'; // the view's morph leaves it be (its slot is data-own)
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
  if (DK.data && (DK.data.items.some((i) => i.deleting) || DK.data.scanning)) DK.timer = setTimeout(() => { if (diskEl.isConnected && !document.hidden) loadDisk(); }, DK.data.scanning ? 4000 : 1500);
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
  const c = await api('/api/disk/check?path=' + encodeURIComponent(path)).catch(() => ({ status: 0, error: "The panel can't be reached" }));
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

const DOTS = ic('more-h');
// the Terminals page itself is terms.js (viewTerm, termAfter)

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

// The chats you looked at last, to show at once when you go back to one: the stream's first window (a moment
// later) replaces them with the latest.
const RECENT = new Map(); // pane key → { items, before, title }
function keepChat() {
  if (!D.key || D.mode !== 'chat' || !D.got || !D.items.length) return;
  RECENT.delete(D.key);
  RECENT.set(D.key, { items: D.items, before: D.before, title: D.title });
  if (RECENT.size > 8) RECENT.delete(RECENT.keys().next().value);
}

function openPane(key, push = true) {
  const [session, pane] = splitKey(key);
  const f = findPane(session, pane);
  if (!f) return;
  if (D.es) D.es.close();
  const switching = !!D.key;
  keepChat();
  stashDraft(); // the chat you're leaving keeps what you were typing
  Object.assign(D, { key, session, pane, mode: f.p.agent?.chat || f.p.agent?.pid ? 'chat' : 'screen', es: null, items: [], before: 0, title: '', view: null, raw: null, ended: false, stale: false, root: f.p.git?.root || '' });
  const kept = D.mode === 'chat' && RECENT.get(key);
  D.cached = !!kept;
  if (kept) Object.assign(D, { items: kept.items, before: kept.before, title: kept.title });
  D.openTools.clear();
  closePreview();
  closeSub();
  Object.assign(F, { list: null });
  $('#drawer').hidden = false;
  $('#dr-switch').hidden = true;
  document.body.classList.add('drawer-open');
  if (push && !switching) history.pushState({ pane: key }, '');
  else if (history.state?.pane) history.replaceState({ pane: key }, '');
  if (!kept) $('#dr-body').innerHTML = `<div class="chat"><div class="chat-empty">${shim('Loading the conversation…')}</div></div>`;
  $('#term-pre').textContent = '';
  $('#term-sb').textContent = '';
  Object.assign(D, { sb: [], sbDirty: false, termFresh: true, loadingMore: false, skills: null });
  loadSkills();
  D.seen = 0;
  restoreDraft();
  renderDrawer();
  if (kept) { D.got = false; renderBody(true); } // the chat as you left it, at once
  if (F.open) loadFiles();
  connectPane();
  if (D.mode === 'screen' && !coarse) termFocus();
}

function closePane(pop = true) {
  if (!D.key) return;
  keepChat();
  stashDraft();
  D.key = null;
  if (D.es) { D.es.close(); D.es = null; }
  D.q = '';
  for (const q of $$('#drawer .rl-q input')) q.value = '';
  toggleModels(false);
  $('#selbar').hidden = true;
  closePreview();
  closeSub();
  closeFilesPanel();
  $('#term-in').blur();
  $('#drawer').hidden = true;
  document.body.classList.remove('drawer-open');
  drAurora();
  if (pop && history.state?.pane) history.back();
}
addEventListener('popstate', () => { if (D.key) closePane(false); });

function connectPane() {
  if (D.es) D.es.close();
  const key = D.key;
  D.got = false;
  const es = new EventSource(`/api/pane/stream?session=${encodeURIComponent(D.session)}&pane=${D.pane}&mode=${D.mode}`);
  es.onmessage = (e) => {
    if (!D.got) D.stalls = 0;
    D.got = true;
    const m = JSON.parse(e.data);
    if (m.t === 'chat') onChat(m);
    else if (m.t === 'screen') onScreen(m.u);
    else if (m.t === 'end') { D.ended = true; es.close(); if (D.es === es) D.es = null; renderBody(); } // pane gone: don't reconnect
  };
  setTimeout(() => { // nothing in 10 s (stuck between here and the server): reconnect; say so only when that stalls too
    if (D.es !== es || D.got) return;
    const again = (D.stalls = (D.stalls || 0) + 1) > 1;
    window.cpReport?.('stall', `no ${D.mode} data for ${key} after 10 s (stream state ${es.readyState})${again ? ', again after a reconnect' : ', reconnecting'}`, '', !again);
    if (!again) connectPane();
  }, 10000);
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
  D.raw = u.viewport || []; // kept: when the pane's size catches up (renderDrawer), the frame is cropped again
  const next = fitScreen(D.raw, D.rows, D.cols); // what the pane shows, not what a bigger frame left behind
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

// a chat's HTML is rebuilt on every update (paintChat then keeps unchanged turns' elements): each message's
// markdown is parsed once, not every second while an agent works
const mdChat = new Map();
let mdChatChars = 0; // what it holds, text and HTML: a phone keeps a few MB at most
const mdc = (t) => {
  let h = mdChat.get(t);
  if (h === undefined) {
    if (mdChatChars > 4e6) { mdChat.clear(); mdChatChars = 0; }
    mdChat.set(t, (h = md(t)));
    mdChatChars += t.length + h.length;
  }
  return h;
};

function otherEv(it, r, running, fr = '') {
  if (PLAN.has(it.name)) return `<div class="ev plain${fr}">${ic('list-check', 'ev-ic')}<span class="ev-v">Updated the plan</span></div>`;
  const agent = it.name === 'Agent' || it.name === 'Task';
  const verb = agent ? (running ? 'Delegating' : 'Delegated') : it.name === 'Skill' ? 'Used skill' : it.name === 'Result' ? 'Result' : 'Used';
  const icon = agent ? 'people' : it.name === 'Skill' ? 'wand-sparkle' : it.name === 'Result' ? 'info-circle' : 'puzzle';
  const arg = agent || it.name === 'Skill' ? it.text : `${it.name.replace(/^mcp__[^_]+__/, '')}${it.text ? ' ' + it.text : ''}`;
  const body = (it.in && !agent ? `<pre class="ev-out">${esc(it.in)}</pre>` : '') + (r ? (agent ? `<div class="md ev-md">${mdc(r.text)}</div>` : `<pre class="ev-out${r.err ? ' err' : ''}">${esc(r.text)}</pre>`) : '');
  const see = agent && it.id ? `<button type="button" class="ev-see" data-subtool="${esc(it.id)}" title="See this subagent's chat" aria-label="See this subagent's chat">${ic('arrow-up-right')}</button>` : '';
  return `<details class="ev${r?.err ? ' err' : ''}${fr}"${opened(it.id)} data-id="${esc(it.id)}"><summary>${ic(icon, 'ev-ic')}${verbHTML(verb, running)}${arg ? `<span class="ev-a chipx">${esc(arg)}</span>` : ''}<span class="ev-m">${durOf(it, r)}</span>${failIc(r, running)}${see}${chev}</summary>${body ? `<div class="ev-body">${body}</div>` : ''}</details>`;
}

function thinkEv(it, fr) {
  const label = it.dur >= 1 ? `Thought for ${fmtDur(it.dur)}` : 'Thought';
  const id = `th-${it.t}-${(it.text || '').length}`;
  return it.text
    ? `<details class="ev think${fr}"${opened(id)} data-id="${id}"><summary>${ic('sparkle', 'ev-ic')}<span class="ev-v">${label}</span>${chev}</summary><div class="think-body md">${mdc(it.text)}</div></details>`
    : `<div class="ev think plain${fr}">${ic('sparkle', 'ev-ic')}<span class="ev-v">${label}</span></div>`;
}

// files a turn hands over (documents it wrote, or named in its messages): cards to preview or download
const PRODUCT = /\.(html?|md|markdown|pdf|png|jpe?g|gif|webp|svg|csv|tsv|xlsx?|docx?)$/i;
const KIND = [[/\.html?$/i, 'HTML page'], [/\.(md|markdown)$/i, 'Markdown document'], [/\.pdf$/i, 'PDF'], [/\.svg$/i, 'SVG image'],
  [/\.(png|jpe?g|gif|webp)$/i, 'Image'], [/\.(csv|tsv)$/i, 'Table'], [/\.xlsx?$/i, 'Spreadsheet'], [/\.docx?$/i, 'Word document']];
function fileCards(paths, qs = fileQS()) { // qs: whose files (the Terminals page's peek draws another pane's)
  return `<div class="fcards">${paths.map((p) => {
    const name = p.split('/').pop();
    const raw = `/api/pane/file?${qs}&raw=1&path=${encodeURIComponent(p)}`;
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

const LONG_MSG = 1500; // characters: a longer message you sent shows its start, and "Show all"
// a message of yours that Claude hasn't recorded yet: queued (Claude busy), sent (pasted, not taken yet), late (30 s on)
const ULBL = { queued: ['clock-circle', "Queued · goes to Claude when it's free"], sending: ['clock-circle', 'Sending…'], slow: ['clock-circle', 'Sending… (the network is slow, retrying)'],
  sent: ['check', 'Sent · waiting for Claude to take it'], late: ['alert-triangle', "Claude hasn't taken it yet: check the Terminal tab"], failed: ['alert-triangle', 'Not sent'] };
function userMsg(it, fr, queued = '', extra = '') {
  const u = splitUploads(String(it.text ?? '').replace(/<\/?pasted_content(?: id="[^"]*")?>\n?/g, '')); // Claude Code's marks around pasted text
  const imgs = u.names.filter(isImg);
  const docs = u.names.filter((n) => !isImg(n));
  const unseen = Math.max(0, (it.img || 0) - imgs.length); // images Claude got that aren't uploads here (pasted in the terminal): a count
  const lim = it.via === 'summary' ? 280 : LONG_MSG; // a compaction's summary: a glimpse, the rest on demand
  const long = u.text.length > lim;
  const fid = long ? `u:${it.t || 0}:${u.text.length}` : '';
  const folded = long && !D.openTools.has(fid);
  const shown = folded ? u.text.slice(0, lim).replace(/\s+\S*$/, '') : u.text; // cut at a word
  // a turn that something else began: another Claude session's message, a scheduled prompt, a Stop hook
  const via = !it.via ? '' : it.via.startsWith('peer:') ? [ic('people'), `From ${esc(it.via.slice(5))}, another Claude session`]
    : it.via === 'hook' ? [ic('hand'), 'A Stop hook'] : it.via === 'panel' ? [ic('layers-alt'), 'From the panel, to free up context'] : it.via === 'summary' ? [ic('layers-alt'), 'What Claude kept: the summary it carries on from'] : [ic('clock-circle'), 'A scheduled prompt'];
  return `<div class="msg-u${fr}${queued ? ' queued ' + queued : ''}${via ? ' via' : ''}">` + (via ? `<span class="q-l via-l">${via[0]}${via[1]}</span>` : '') +
    (imgs.length ? `<div class="uimgs">${imgs.map((n) => `<img src="/api/upload/${esc(n)}" alt="Sent image" loading="lazy">`).join('')}</div>` : '') +
    (docs.length ? `<div class="udocs">${docs.map((n) => `<a class="udoc" href="/api/upload/${esc(n)}" download>${fxBadge(n)}<span>${esc(uploadLabel(n))}</span>${ic('download2')}</a>`).join('')}</div>` : '') +
    (u.text || unseen ? `<div class="bubble${folded ? ' folded' : ''}">${mdInline(shown)}${unseen ? `<div class="att">${ic('image3')}${plural(unseen, 'image')}</div>` : ''}</div>` : '') +
    (long ? `<button type="button" class="u-more" data-ulong="${esc(fid)}" aria-expanded="${!folded}">${folded ? `Show all · ${u.text.length.toLocaleString()} characters` : 'Show less'}</button>` : '') +
    (it.mid ? `<span class="q-l">${ic('bolt')}Sent while Claude was working</span>` : '') +
    (queued ? `<span class="q-l">${ic(ULBL[queued][0])}${ULBL[queued][1]}${extra}</span>` : '') + '</div>';
}

const textMsg = (it, i, fr) => `<div class="msg-a${fr}"><div class="md">${mdc(it.text)}</div><div class="msg-act">` +
  `<button type="button" class="abtn" data-copyi="${i}" title="Copy" aria-label="Copy">${ic('copy')}</button>` +
  `<button type="button" class="abtn" data-quotei="${i}" title="Quote in your reply" aria-label="Quote in your reply">${ic('quote-up')}</button></div></div>`;

const NOTE_IC = [[/^Interrupted/, 'hand'], [/^PR #/, 'hierarchy'], [/compact/i, 'layers-alt'], [/^Worked for/, 'clock-circle'], [/^\//, 'slash-square'], [/^Background|completed|failed|killed/i, 'bell']];
// a background task's notice (Claude Code telling the agent a command, monitor or subagent finished):
// what it was, and how it went, at a glance — red when it failed
const TASK_ST = { completed: ['check-circle-f', 'ok', 'done'], failed: ['x-circle-f', 'bad', 'failed'], killed: ['stop-f', 'off', 'stopped'], stopped: ['stop-f', 'off', 'stopped'], event: ['bell', 'ev', ''] };
function taskNoteHTML(it, fr) {
  const st = it.name.slice(5);
  const [icon, cls, word] = TASK_ST[st] || ['bell', 'ev', st];
  const m = it.text.match(/^(Background command|Monitor|Agent)(?: event:)? "(.+?)"(?: (?:completed|failed|was stopped|stream ended|finished))?(?: with exit code (\d+)| \(exit code (\d+)\))?/);
  const what = m ? { 'Background command': 'Command', Monitor: 'Monitor', Agent: 'Subagent' }[m[1]] : '';
  const exit = m && (m[3] || m[4]);
  const tag = [what, word, exit && exit !== '0' ? 'exit ' + exit : ''].filter(Boolean).join(' · ');
  return `<div class="sysnote tnote t-${cls}${fr}" title="${esc(it.out ? it.text + '\n' + it.out : it.text)}"><span>${ic(icon)}<b>${esc(m ? m[2] : it.text)}</b>${tag ? `<em>${esc(tag)}</em>` : ''}</span></div>`;
}
// a run of one monitor's events: one row ("swarm agents finishing · 3 events · latest 2m ago"), and what
// each reported when you open it
function eventGroupHTML(g, items) {
  const evs = g.events.map((i) => items[i]);
  const name = (g.text.match(/"(.+)"/) || [0, g.text])[1];
  const ago = evs.at(-1).t ? noteAge(evs.at(-1).t) : '';
  const id = 'evg-' + (evs[0].t || g.events[0]);
  const at = (t) => (t ? new Date(t * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '');
  return `<details class="evgrp" data-id="${esc(id)}"${opened(id)}><summary><span>${ic('bell')}<b>${esc(name)}</b>` +
    `<em>${evs.length} events${ago ? ` · latest ${ago === 'now' ? 'just now' : ago + ' ago'}` : ''}</em>${ic('angle-right', 'evg-chev')}</span></summary>` +
    `<div class="evg-list">${[...evs].reverse().map((e) => `<div class="evg-i"><span class="evg-t">${esc(at(e.t))}</span><span>${esc(e.out || 'event')}</span></div>`).join('')}</div></details>`;
}
const noteHTML = (it, fr = '') => (it.name?.startsWith('task:') ? taskNoteHTML(it, fr)
  : `<div class="sysnote${fr}"><span>${ic((NOTE_IC.find(([re]) => re.test(it.text)) || [0, 'info-circle'])[1])}${esc(it.text)}</span></div>`);

// c: whose chat — the pane's (D) or a subagent's (SA); both carry items, seen, before, loadingMore, ended, key
// A chat paints in chunks, one per turn (cut at CHUNK, a comment no message can make: text is escaped). A chunk
// whose HTML is what it was last time keeps its elements untouched, so while an agent works an update redoes
// its current turn, not the whole conversation (a long chat was a full rebuild every second: 280 ms on a phone).
const CHUNK = '<!--t-->';
function paintChat(body, html) {
  const open = '<div class="chat">', close = '</div>';
  let chat = body.firstElementChild;
  if (body.childElementCount !== 1 || !chat.matches('.chat')) { body.innerHTML = open + close; chat = body.firstElementChild; }
  const parts = html.slice(open.length, -close.length).split(CHUNK);
  const pool = new Map(); // HTML → last time's chunks with it, in order
  for (const c of chat.chunks || []) { const l = pool.get(c.html); if (l) l.push(c); else pool.set(c.html, [c]); }
  const next = parts.map((h) => {
    const same = pool.get(h)?.shift();
    if (same) return same;
    const t = document.createElement('template');
    t.innerHTML = h;
    return { html: h, nodes: [...t.content.childNodes] };
  });
  const keep = new Set(next.flatMap((c) => c.nodes));
  for (const n of [...chat.childNodes]) if (!keep.has(n)) n.remove();
  let at = chat.firstChild;
  for (const c of next) for (const n of c.nodes) { if (n === at) at = at.nextSibling; else chat.insertBefore(n, at); } // only what's out of place moves
  chat.chunks = next;
}
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
  // sep: CHUNK between the rows of the turn in progress (it paints row by row: a long turn is most of a busy chat)
  const workHTML = (idx, changed, sep = '') => {
    const ev = [];
    let explore = [];
    const flush = () => { if (explore.length) ev.push(exploreEv(explore, explore.some((g) => running(g.it)), fresh(explore[explore.length - 1].i))); explore = []; };
    for (const i of idx) {
      const it = items[i];
      if (it.k === 'result') { if (!tools.has(it.id)) { flush(); ev.push(otherEv({ name: 'Result', text: '', id: it.id }, it, false, fresh(i))); } continue; }
      if (it.k === 'tool' && EXPLORE[it.name]) { explore.push({ it, r: res.get(it.id), i }); continue; }
      flush();
      if (it.k === 'think') ev.push(thinkEv(it, fresh(i)));
      else if (it.k === 'text') ev.push(textMsg(it, i, fresh(i)));
      else if (it.k === 'tool') {
        const r = res.get(it.id);
        if (RUN.has(it.name)) ev.push(runEv(it, r, running(it), fresh(i)));
        else if (EDIT.has(it.name)) {
          ev.push(editEv(it, r, running(it), fresh(i)));
          if (it.path && !r?.err) { const c = changed.get(it.path) || { add: 0, del: 0 }; c.add += it.add || 0; c.del += it.del || 0; changed.set(it.path, c); }
        } else ev.push(otherEv(it, r, running(it), fresh(i)));
      }
    }
    flush();
    return ev.join(sep);
  };
  const turns = splitTurns(items);
  // older messages load by themselves as you near the top (the sentinel below); a click works too
  let h = c.before > 0 ? `<div class="more-s" data-more aria-live="polite">${c.loadingMore ? shim('Loading earlier messages…') : ''}</div>` : '';
  turns.forEach((t, n) => {
    const last = n === turns.length - 1;
    const changed = new Map();
    h += CHUNK; // each turn paints on its own (paintChat)
    if (t.user >= 0) h += userMsg(items[t.user], fresh(t.user));
    if (t.work.length) {
      const live = last && (working || t.final < 0); // the turn in progress stays open, as a live trace
      const body = workHTML(t.work, changed, live ? CHUNK : '');
      if (live) h += CHUNK + body;
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
    if (made.size) { h += fileCards([...made], c.qs); for (const p of made) changed.delete(p); }
    if (changed.size) h += changesCard(changed);
    for (const g of groupNotes(items, t.notes)) h += g.events ? eventGroupHTML(g, items) : noteHTML(items[g.i], fresh(g.i));
    if (t.worked >= 0 && !t.work.length && t.final < 0) h += noteHTML(items[t.worked], fresh(t.worked)); // no fold line to carry it, no reply to say what it did
  });
  const subs = p?.agent?.subagents || [];
  const wfs = p?.agent?.workflows || [];
  const shells = p?.agent?.shells || []; // background commands and monitors: what "Background" means between turns
  if (subs.length || shells.length || wfs.length) {
    const subRow = (a, cls = '') => `<button type="button" class="task${cls}" data-sub="${esc(a.id)}" title="${esc(['See what it is doing', a.phase, a.type, a.model, fmtDur(now() - a.started)].filter(Boolean).join(' · '))}"><i class="spin"></i>` +
      `<span class="task-t">${esc(a.desc || a.id)}</span><span class="task-m">${esc(a.doing || 'Thinking')}</span><span class="tbadge">${a.wf ? 'Workflow' : a.bg ? 'Background' : 'Running'}</span>${ic('angle-right', 'task-go')}</button>`;
    // a workflow: one row with its progress, its running agents under it
    const wfHTML = wfs.map((w) => { const mine = subs.filter((a) => a.wf === w.id);
      return `<div class="task wf"><i class="spin"></i><span class="task-t">${esc(w.name)}</span><span class="task-m">${w.done} of ${w.total} agents done · ${fmtDur(Math.max(0, now() - w.started))}</span><span class="tbadge">Workflow</span></div>` +
        mine.slice(0, 8).map((a) => subRow(a, ' in-wf')).join('') + (mine.length > 8 ? `<div class="task in-wf more">+ ${mine.length - 8} more running</div>` : ''); }).join('');
    h += CHUNK + `<div class="tasks">${subs.filter((a) => !a.wf || !wfs.some((w) => w.id === a.wf)).map((a) => subRow(a)).join('')}${wfHTML}` +
      shells.map((x) => `<div class="task shell" title="${esc(x.cmd || x.desc)}"><i class="spin"></i><span class="task-t">${esc(x.desc || x.cmd)}</span>` +
        `${x.desc && x.cmd ? `<span class="task-m">${esc(x.cmd.split('\n')[0])}</span>` : ''}<span class="tbadge">${x.kind === 'monitor' ? 'Monitor' : 'Command'} · ${fmtDur(Math.max(0, now() - x.started))}</span></div>`).join('') + '</div>';
  }
  const seed = seedOf(`${c.key}:${p?.since || 0}`);
  if (working) h += CHUNK + `<div class="live" role="status">${ORBS[(seed >>> 8) % ORBS.length]}${shim(VERBS[seed % VERBS.length] + '…')}<span class="live-t" data-since="${p.since || 0}">${p.since ? elapsed(p.since) : ''}</span></div>`;
  const queued = working ? pendingQueue(items) : []; // typed while Claude works; an idle agent has emptied its queue
  // a cancel in progress holds the list steady: as it was (that one cancelling…) until the server answers, then
  // without it, while the transcript catches up (Claude Code notes the whole queue out, then the rest back in)
  const un = c === D && UNQ.get(c.key);
  const shown = un ? (un.done ? un.keep : un.all) : queued;
  if (c === D) D.queued = shown; // what each Cancel button stands for (data-unqueue is its place in this list)
  if (shown.length) h += CHUNK + shown.map((q, i) => q.startsWith(PANEL_MARK) ? userMsg({ text: q.slice(PANEL_MARK.length), via: 'panel' }, '', 'queued') // the compactor's note: not yours to edit
    : userMsg({ text: q }, '', 'queued', un && !un.done && i === un.at ? ' · cancelling…'
    : c === D && !un ? `<button type="button" class="q-btn" data-unqueue="${i}" title="Take it out of Claude's queue, back into your box">Edit</button>` : '')).join('');
  if (c === D && c.key) { // what you just sent shows at once, until the chat has it
    const pend = stillPending(PENDING.get(c.key) || [], items).filter((x) => x.st !== 'sent' || now() - x.ackAt < 600);
    PENDING.set(c.key, pend);
    h += CHUNK + pend.map((x) => {
      const st = x.st === 'sent' ? (now() - x.ackAt > 30 ? 'late' : 'sent') : x.st === 'sending' && x.slow ? 'slow' : x.st;
      const waiting = x.st === 'sending' && (OUTBOX.get(c.key)?.jobs || []).indexOf(x) > 0; // behind one going out now: not sent yet
      const btn = (attrs, label) => `<button type="button" class="q-btn" ${attrs}>${label}</button>`;
      const edit = btn(`data-unsend="${x.id}"`, 'Edit'); // back into the chat box, as ↑ does in the terminal
      const extra = x.st === 'failed' && x.box ? `: your earlier message is still in Claude's box<q class="q-box">${esc(x.box)}</q>` +
          `<span class="q-acts">${btn(`data-resend="${x.id}" data-clearbox`, 'Clear it & send')}${btn(`data-takebox="${x.id}"`, 'Edit')}</span>`
        : x.st === 'failed' ? `: ${esc(x.err)}${btn(`data-resend="${x.id}"`, 'Retry')}${edit}`
        : st === 'late' ? edit + btn(`data-unsend="${x.id}" data-dismiss`, 'Dismiss')
        : waiting ? edit : '';
      return userMsg({ text: x.st === 'sent' ? x.text : x.shown, img: x.st === 'sent' ? 0 : (x.files || []).length }, '', st, extra);
    }).join('');
  }
  if (c.ended) h += noteHTML({ text: 'The pane closed' });
  return `<div class="chat">${h || `<div class="chat-empty">${ic('chat-round-dots')}<span>No messages yet</span></div>`}</div>`;
}
setInterval(() => { if (D.key && D.mode === 'chat' && (PENDING.get(D.key) || []).some((x) => x.st === 'sent' && now() - x.ackAt > 30) && !$('#dr-body .msg-u.late')) renderBody(); }, 5000);
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
  keepScroll([body], () => { paintChat(body, html); });
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
  box.classList.toggle('pin', flow);
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
  fitPane();
}

// The terminal gets the size the view has room for: the panel resizes the pane's tab to it (fit.go; a
// terminal you have attached elsewhere keeps deciding the size). On a phone it stays at least 80 columns wide,
// so a look from there doesn't squash every agent in the session. Asked once per size.
let fitT = 0, fitAsked = '';
function fitPane() {
  clearTimeout(fitT);
  fitT = setTimeout(() => {
    const box = $('#dr-term');
    if (!D.key || D.mode !== 'screen' || !box?.clientWidth) return;
    const fs = coarse ? 10.5 : 13;
    const cols = Math.min(400, Math.max(80, Math.floor((box.clientWidth - 26) / (fs * 0.602))));
    const rows = Math.min(150, Math.max(12, Math.floor((box.clientHeight - 20) / (fs * 1.22))));
    const ask = `${D.key}:${cols}x${rows}`; // per pane: zellij sizes each tab on its own
    if ((cols === D.cols && Math.abs(rows - D.rows) <= 2) || ask === fitAsked) return;
    fitAsked = ask;
    api('/api/pane/fit', { session: D.session, pane: D.pane, cols, rows }).catch(() => {});
  }, 300);
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
  if (!D.got && !D.ended && !D.cached) return;
  const body = $('#dr-body');
  const atBottom = body.scrollTop + body.clientHeight >= body.scrollHeight - 80;
  keepScroll([body], () => { paintChat(body, chatHTML(f?.p)); });
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
const toLatest = () => { const b = $('#dr-body'); b.scrollTo({ top: b.scrollHeight, behavior: 'smooth' }); }; // from there the chat follows
$('#dr-down').addEventListener('click', toLatest);
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
// how full the context window is, as of the agent's latest request
const fmtTok = (n) => (n >= 1e9 ? `${+(n / 1e9).toFixed(1)}B` : n >= 1e6 ? `${+(n / 1e6).toFixed(1)}M` : n >= 1000 ? `${Math.round(n / 1000)}k` : String(n));
// what the session has cost so far, as Claude Code counts it (its status line hands it over)
function renderCost(p) {
  const v = p?.agent?.cost || 0;
  const el = $('#dr-cost');
  el.hidden = !v;
  if (!v) return;
  setText(el, v >= 1000 ? '$' + Math.round(v).toLocaleString('en-US') : '$' + v.toFixed(2));
  setAttr(el, 'title', `This session has cost $${v.toFixed(2)} so far (as Claude Code counts it)`);
  setAttr(el, 'aria-label', el.title);
}
function renderCtx(p) {
  renderCost(p);
  const c = p?.agent?.ctx;
  const el = $('#dr-ctx');
  el.hidden = !c;
  if (!c) return;
  const pct = Math.min(100, Math.round(c.used / c.window * 100));
  setAttr(el, 'class', 'ctx' + (pct >= 90 ? ' bad' : pct >= 70 ? ' warn' : ''));
  const fg = el.querySelector('.ctx-fg'), dash = `${Math.max(pct, 1)} 100`;
  if (fg.style.strokeDasharray !== dash) fg.style.strokeDasharray = dash;
  setText(el.querySelector('.ctx-p'), pct + '%');
  setText(el.querySelector('.ctx-n'), `${fmtTok(c.used)} / ${fmtTok(c.window)}`);
  const t = `Context window: ${c.used.toLocaleString()} of ${c.window.toLocaleString()} tokens used (${pct}%). Compacting limits`;
  setAttr(el, 'title', t);
  setAttr(el, 'aria-label', t);
}
// ---------- the compactor (compact.go): past the suggestion a Compact button; the limits in a popover by the ring ----------
const PANEL_MARK = '[Control panel] '; // compact.go's panelMark: what the compactor types starts with it
const ctxPct = (c) => (c?.used ? Math.min(100, Math.round((c.used / c.window) * 100)) : 0);
const CPH = {
  pausing: ['Pausing to compact…', 'Claude was asked to stop at its next natural break: the panel compacts it then and tells it to continue'],
  compacting: ['Compacting…', 'Summarizing the conversation to free up its context'],
  hold: ['Compacts after your answer', 'Its last message asks you something: it is compacted when the turn after your answer ends'],
};
// compactBtn: Compact (from the suggestion on), or where the compactor is with the agent; '' otherwise
function compactBtn(a, session, pane, cls = '') {
  const cfg = S.data.term?.compact, ph = CPH[a?.compact?.phase], pct = ctxPct(a?.ctx);
  if (ph) return `<span class="cmp busy ${cls}" title="${ph[1]}">${a.compact.phase === 'hold' ? ic('clock-circle') : '<i class="spin"></i>'}<span>${ph[0]}</span></span>`;
  if (!cfg || !pct || pct < cfg.suggest) return '';
  const by = a.compact?.off ? ' (not compacted by itself)' : cfg.on ? ` (by itself at ${cfg.auto}%, at a natural break)` : '';
  return `<button type="button" class="cmp ${cls}" data-compact="${esc(session)}|${pane}" title="Context ${pct}% full: summarize the conversation to free it up${by}">${ic('layers-alt')}<span>Compact</span></button>`;
}
function renderCompact(p) {
  const el = $('#dr-compact'), h = p?.agent ? compactBtn(p.agent, D.session, D.pane) : '';
  setHidden(el, !h);
  if (el._h !== h) { el.innerHTML = h; el._h = h; }
  if (!$('#cpop').hidden) cpopHead();
}
async function compactNow(session, pane) {
  const r = await api('/api/pane/compact', { session, pane, act: 'now' }).catch(() => ({ status: 0, error: 'The panel is offline' }));
  if (r.status !== 200) return toast(r.error || 'Couldn\'t compact it', true);
  const f = findPane(session, pane), busy = f && ['working', 'stalled'].includes(f.p.state);
  toast(busy ? 'It compacts at its next natural break, then carries on' : 'Compacting');
  if (f?.p.agent) { f.p.agent.compact = r.compact; render(); }
}
document.addEventListener('click', (e) => {
  const b = e.target.closest('[data-compact], [data-cpop]');
  if (!b) return;
  e.preventDefault();
  e.stopPropagation();
  const [session, pane] = (b.dataset.compact || b.dataset.cpop).split('|');
  if (b.dataset.cpop) return $('#cpop').hidden || CP.anchor !== b ? openCpop(b, session, +pane) : closeCpop();
  compactNow(session, +pane);
}, true);
// the popover: how full, the two limits, whether agents compact by themselves, and this one
const CP = { session: '', pane: 0, anchor: null, t: 0 };
function openCpop(anchor, session, pane) {
  const f = findPane(session, pane), cfg = S.data.term?.compact;
  if (!f?.p.agent || !cfg) return;
  Object.assign(CP, { session, pane, anchor });
  const el = $('#cpop'), off = !!f.p.agent.compact?.off;
  const range = (id, label, v) => `<label class="cp-l" for="${id}"><span>${label}</span><b id="${id}-v">${v}%</b></label><input type="range" id="${id}" min="10" max="90" step="5" value="${v}">`;
  el.innerHTML = `<div class="cp-h"><b>Context</b><span id="cp-now"></span></div><div class="cp-bar" aria-hidden="true"><i class="u"></i><i class="m s"></i><i class="m a"></i></div>` +
    range('cp-suggest', 'Offer to compact at', cfg.suggest) + range('cp-auto', 'Compact by itself at', cfg.auto) +
    `<label class="cp-sw"><input type="checkbox" id="cp-on"${cfg.on ? ' checked' : ''}><span>Agents compact by themselves</span></label>` +
    `<label class="cp-sw"><input type="checkbox" id="cp-off"${off ? ' checked' : ''}><span>Not this one</span></label>` +
    '<p class="cp-note">Never mid-step: a finished turn is compacted at once; a working one is asked to stop at its next natural break, then told to carry on. When its last message asks you something, it waits for your answer.</p>' +
    `<button type="button" class="btn" data-compact="${esc(session)}|${pane}">${ic('layers-alt')}Compact now</button>`;
  el.hidden = false;
  setAttr($('#dr-ctx'), 'aria-expanded', String(anchor === $('#dr-ctx')));
  cpopHead();
  cpopPlace();
}
function cpopPlace() {
  const el = $('#cpop'), r = CP.anchor?.getBoundingClientRect();
  if (!r) return;
  const w = el.offsetWidth, h = el.offsetHeight, up = r.bottom + 6 + h > innerHeight - 8;
  el.style.left = Math.max(8, Math.min(r.left + r.width / 2 - w / 2, innerWidth - w - 8)) + 'px';
  el.style.top = (up ? Math.max(8, r.top - h - 6) : r.bottom + 6) + 'px';
  el.style.transformOrigin = up ? 'bottom center' : 'top center';
}
// the live part: how full it is now, and the bar with both limits on it
function cpopHead() {
  const f = findPane(CP.session, CP.pane), c = f?.p.agent?.ctx, cfg = S.data.term?.compact;
  if (!c || !cfg) return closeCpop();
  const pct = ctxPct(c), sv = +$('#cp-suggest').value, av = +$('#cp-auto').value;
  setText($('#cp-now'), `${fmtTok(c.used)} of ${fmtTok(c.window)} · ${pct}%`);
  const bar = $('#cpop .cp-bar');
  bar.querySelector('.u').style.width = pct + '%';
  bar.querySelector('.s').style.left = sv + '%';
  bar.querySelector('.a').style.left = av + '%';
  bar.classList.toggle('over', pct >= av);
}
function closeCpop() { $('#cpop').hidden = true; setAttr($('#dr-ctx'), 'aria-expanded', 'false'); }
$('#dr-ctx').addEventListener('click', (e) => { if (!$('#cpop').hidden) return closeCpop(); openCpop(e.currentTarget, D.session, D.pane); });
$('#cpop').addEventListener('input', (e) => {
  const s = $('#cp-suggest'), a = $('#cp-auto');
  if (e.target === s && +s.value > +a.value) a.value = s.value; // the offer comes at or before the auto-compact
  if (e.target === a && +a.value < +s.value) s.value = a.value;
  setText($('#cp-suggest-v'), s.value + '%');
  setText($('#cp-auto-v'), a.value + '%');
  cpopHead();
  if (e.target.id !== 'cp-off') { clearTimeout(CP.t); CP.t = setTimeout(saveCompact, 350); }
});
$('#cpop').addEventListener('change', async (e) => {
  if (e.target.id !== 'cp-off') return;
  const off = e.target.checked;
  const r = await api('/api/pane/compact', { session: CP.session, pane: CP.pane, act: off ? 'off' : 'on' }).catch(() => ({ status: 0 }));
  if (r.status !== 200) { e.target.checked = !off; return toast(r.error || 'Couldn\'t save that', true); }
  const f = findPane(CP.session, CP.pane);
  if (f?.p.agent) f.p.agent.compact = r.compact;
  toast(off ? 'This agent won\'t be compacted by itself' : 'This agent compacts by itself again');
  render();
});
async function saveCompact() {
  const body = { on: $('#cp-on').checked, suggest: +$('#cp-suggest').value, auto: +$('#cp-auto').value };
  const r = await api('/api/compact', body).catch(() => ({ status: 0 }));
  if (r.status !== 200) return toast(r.error || 'Couldn\'t save the limits', true);
  if (S.data.term) S.data.term.compact = r.compact;
  render();
}
addEventListener('pointerdown', (e) => { if (!$('#cpop').hidden && !e.target.closest('#cpop, #dr-ctx, [data-cpop]')) closeCpop(); }, true);
addEventListener('keydown', (e) => { if (e.key === 'Escape' && !$('#cpop').hidden) { e.stopImmediatePropagation(); closeCpop(); } }, true);
addEventListener('resize', () => { if (!$('#cpop').hidden) cpopPlace(); });
// the agent's open pull request, floating over its chat: checks and review at a glance, folded to a pill
// when you want the room (remembered per PR; folded by default on a phone). Hidden unless a PR is open.
const PRS = new Map(); // pane key -> { at, n, prs, busy }
const prFold = (() => { try { return JSON.parse(store.get('cp-pr-fold', '{}')) || {}; } catch { return {}; } })();
function syncPR(p) {
  const key = D.key;
  if (!key || !p?.agent) return renderPR([]);
  const e = PRS.get(key) || {};
  const n = (p.agent.prs || []).join(','); // a PR the session just opened shows at once
  if (!e.busy && (Date.now() - (e.at || 0) > 30000 || e.n !== n)) {
    PRS.set(key, { ...e, busy: true, n });
    api(`/api/pane/pr?session=${encodeURIComponent(D.session)}&pane=${D.pane}`)
      .then((r) => { // a stale card (the panel is asking GitHub meanwhile): shown at once, asked again shortly
        const tries = r.stale ? (e.tries || 0) + 1 : 0;
        PRS.set(key, { at: r.stale && tries < 6 ? Date.now() - 28000 : Date.now(), n, prs: r.prs || [], rels: r.releases || [], me: r.me || '', tries });
        if (r.stale && tries < 6) setTimeout(() => { if (D.key === key) syncPR(findPane(D.session, D.pane)?.p); }, 3000);
      })
      .catch(() => PRS.set(key, { ...e, at: Date.now(), n, busy: false }))
      .finally(() => { if (D.key === key) renderPR(PRS.get(key).prs || [], PRS.get(key).rels, PRS.get(key).me); });
  }
  renderPR(e.prs || [], e.rels, e.me);
}
setInterval(() => { if (D.key && !document.hidden) syncPR(findPane(D.session, D.pane)?.p); }, 30000);
// every agent's card ahead of time, once (the panel answers from its cache), so a chat shows it as it opens; a stale
// one is asked again when its chat opens
function prefetchPRs(t) {
  for (const s of t.sessions || []) for (const tb of s.tabs || []) for (const p of tb.panes) {
    const key = s.name + ':' + p.id;
    if (!p.agent?.prs?.length || PRS.has(key)) continue;
    const n = p.agent.prs.join(',');
    PRS.set(key, { busy: true, n });
    api(`/api/pane/pr?session=${encodeURIComponent(s.name)}&pane=${p.id}`)
      .then((r) => PRS.set(key, { at: r.stale ? 0 : Date.now(), n, prs: r.prs || [], rels: r.releases || [], me: r.me || '' }))
      .catch(() => PRS.set(key, { at: 0, n }))
      .finally(() => { if (D.key === key) syncPR(findPane(D.session, D.pane)?.p); });
  }
}
const CK = { fail: ['x-circle-f', 'bad'], run: ['', 'run'], pass: ['check-circle-f', 'ok'], skip: ['', 'skip'] };
const ckIcon = (st, cls = '') => (st === 'run' ? `<i class="spin ${cls}"></i>` : CK[st][0] ? ic(CK[st][0], cls) : `<i class="prc-skip ${cls}"></i>`);
function prSummary(pr) {
  const n = { fail: 0, run: 0, pass: 0, skip: 0 };
  for (const c of pr.checks) n[c.state]++;
  const ci = n.fail ? 'fail' : n.run ? 'run' : n.pass ? 'pass' : 'skip';
  const ciText = pr.checks.length ? [n.fail && `${n.fail} failed`, n.run && `${n.run} running`, n.pass && `${n.pass} passed`, !n.fail && !n.run && !n.pass && `${n.skip} skipped`].filter(Boolean).join(' · ') : 'No checks';
  const by = (l) => (l?.length ? ` by ${l.join(', ')}` : '');
  const rv = pr.review === 'CHANGES_REQUESTED' ? ['bad', 'x-circle-f', 'Changes requested' + by(pr.changes)]
    : pr.review === 'APPROVED' || (!pr.review && pr.approved?.length) ? ['ok', 'check-circle-f', 'Approved' + by(pr.approved)]
    : ['wait', 'clock-circle', pr.review === 'REVIEW_REQUIRED' ? 'Review required' : 'No review yet'];
  return { ci, ciText, rv };
}
// what the session shipped, per repo, newest first: what's still open, then what's merged into stage and not
// released yet, then a group per stage → main release. A group reads from → to: where it is (in production since…),
// this chat's PRs it carried (each its whole title, its branch → stage), "↓ released in", then the release PR. Nothing
// is cut: titles and branches wrap. Syncs (main merged back into stage), closed PRs and releases that carried
// nothing from this chat fold into one "other" line, each tagged. A PR or release someone else opened says whose;
// each line's tooltip has its journey (merged, on stage, released, in production).
const TIER = (b) => (/^(main|master|prod|production)$/.test(b) ? 'production' : /^(stage|staging)$/.test(b) ? 'stage' : '');
const isRel = (p) => TIER(p.base) === 'production' && TIER(p.head) === 'stage';
const isSync = (p) => !isRel(p) && /(^|[/_-])sync([/_-]|$)/i.test(p.head || '');
function prWhen(iso) { // 3:16 PM today, Thu 3:16 PM this week, Oct 7 before
  if (!iso) return '';
  const d = new Date(iso), n = new Date(), hm = d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  if (d.toDateString() === n.toDateString()) return hm;
  return n - d < 6 * 864e5 ? d.toLocaleDateString([], { weekday: 'short' }) + ' ' + hm : d.toLocaleDateString([], { month: 'short', day: 'numeric' });
}
const ENV = { stage: 'Stage', production: 'Prod' };
const DEP = { live: 'check-circle-f', fail: 'x-circle-f', wait: 'clock-circle' };
const depIcon = (d) => (d.state === 'run' ? '<i class="spin"></i>' : ic(DEP[d.state]));
const depOf = (p, env) => (p.deploys || []).find((d) => d.env === env);
// a line's tooltip: its journey so far
function prTrip(p, rel) {
  const st = depOf(p, 'stage'), pd = depOf(p, 'production');
  const env = (d, name) => (!d ? '' : d.state === 'live' ? `on ${name} ${prWhen(d.at)}` : d.state === 'run' ? `deploying to ${name}` : d.state === 'fail' ? `${name} deploy failed` : `not on ${name} yet`);
  return [`#${p.number} ${p.title || ''}`, `${p.head} → ${p.base}`,
    [p.mergedAt && `Merged ${prWhen(p.mergedAt)}`, env(st, 'stage'), rel && (rel.state === 'MERGED' ? `released in #${rel.number} ${prWhen(rel.mergedAt)}` : `goes out with #${rel.number} (open)`), env(pd, 'production')].filter(Boolean).join(' → ')].join('\n');
}
const by = (p, me) => (p.author && me && p.author !== me ? `<span class="prl-by">by ${esc(p.author)}</span>` : '');
const prClock = (iso) => (iso ? new Date(iso).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' }) : '');
// one PR, nothing cut: its number and whole title, then where it went from and to (its branch → the branch it
// merged into), when, and whose when it isn't yours; on the right what the group doesn't already say
const prRow = (p, me, { right = '', tip = '', when = p.mergedAt, rel = false } = {}) =>
  `<a class="prl${rel ? ' rel' : ''}" href="${esc(p.url)}" target="_blank" rel="noopener" title="${esc(tip || p.title || '')}">` +
  `${rel ? ic('layers', 'prl-ic') : ''}<b>#${p.number}</b><span class="prl-m"><span class="prl-t">${esc(p.title || p.head)}</span>` +
  `<span class="prl-s"><code>${esc(p.head)}</code> → <code>${esc(p.base)}</code>${when ? ` <span class="prl-at">· ${esc(prClock(when))}</span>` : ''}${rel ? '' : by(p, me)}</span></span>` +
  `${right ? `<span class="prl-r">${right}</span>` : ''}</a>`;
// a group's head: where its change is, in words, and since when
function prGroupHead(d, rel) {
  const st = !d ? ['mid', ic('check-circle-f'), 'Merged into main'] : d.state === 'live' ? ['live', ic('check-circle-f'), 'In production']
    : d.state === 'run' ? ['run', '<i class="spin"></i>', 'Deploying to production'] : d.state === 'fail' ? ['bad', ic('x-circle-f'), 'Production deploy failed'] : ['wait', ic('clock-circle'), 'Released · not deployed yet'];
  const when = d?.state === 'live' ? 'since ' + prWhen(d.at) : prWhen(rel.mergedAt);
  return `<div class="prg-h ${st[0]}" title="${esc(`${st[2]} ${when}${d?.note ? '\n' + d.note : ''}`)}">${st[1]}<b>${st[2]}</b><span class="prg-at">${esc(when)}</span></div>`;
}
// the arrow from this chat's PRs to the PR that took them on: "↓ released in", "↓ goes out with"
const prTo = (text) => `<div class="prl-to">${ic('arrow-down')}<span>${text}</span></div>`;
function prRepo(prs, rels, el, me) {
  const relOf = new Map([...prs, ...rels].filter(isRel).map((r) => [r.number, r]));
  const t = (p) => p.mergedAt || p.closedAt || '';
  const recent = (a, b) => (t(b) || '~').localeCompare(t(a) || '~'); // newest first; open ones (no time yet) first of all
  const shipped = new Map(), waiting = [], open = [], other = [];
  for (const p of prs) {
    const rel = relOf.get(p.release);
    if (isRel(p) || p.state === 'CLOSED' || isSync(p)) continue;
    if (p.state === 'OPEN') open.push(p);
    else if (rel?.state === 'MERGED') shipped.set(rel.number, [...(shipped.get(rel.number) || []), p]);
    else if (TIER(p.base) === 'production') shipped.set('d' + p.number, [p]); // straight into main: its own release
    else if (TIER(p.base) === 'stage') waiting.push(p);
    else other.push([p, `into ${p.base}`]);
  }
  for (const p of prs) {
    if (p.state === 'CLOSED') other.push([p, 'closed']);
    else if (isSync(p)) other.push([p, 'sync']); // main merged back into stage: no change of its own
    else if (isRel(p) && !shipped.has(p.number) && p.state === 'MERGED') other.push([p, "others' release"]); // merged here, carried none of this chat's PRs
  }
  const relRow = (r) => prRow(r, me, { rel: true, tip: prTrip(r) }) + (r.author && me && r.author !== me ? `<div class="prg-who">${ic('people')}<span>Opened by <b>${esc(r.author)}</b>: it carries their changes too</span></div>` : '');
  let h = '';
  if (open.length) h += `<div class="prg"><div class="prg-h open">${ic('hierarchy')}<b>In progress</b></div>` + open.map((p) => { const { ci, ciText, rv } = prSummary(p);
    return prRow(p, me, { when: '', right: `${p.draft ? '<span class="prl-draft">Draft</span>' : ''}${ckIcon(ci, 'prc-ci-' + ci)}${ic(rv[1], 'prc-rv-' + rv[0])}`, tip: `Checks: ${ciText} · ${rv[2]}${p.conflict ? ' · conflicts with ' + p.base : ''}` }); }).join('') + '</div>';
  if (waiting.length) {
    const next = relOf.get(waiting.find((p) => relOf.get(p.release))?.release);
    h += `<div class="prg"><div class="prg-h stage">${ic('layers')}<b>On stage · not released yet</b></div>` +
      waiting.sort(recent).map((p) => { const d = depOf(p, 'stage');
        return prRow(p, me, { right: d ? `<span class="dep ${d.state}" title="${esc(d.state === 'live' ? 'Live on stage' : d.state === 'run' ? 'Deploying to stage' : d.state === 'fail' ? 'Stage deploy failed' : 'Not on stage yet')}">${depIcon(d)}Stage</span>` : '', tip: prTrip(p, relOf.get(p.release)) }); }).join('') +
      (next ? prTo('goes out with') + relRow(next) : '') + '</div>';
  }
  const groups = [...shipped].map(([k, ps]) => { const rel = typeof k === 'number' ? relOf.get(k) : ps[0]; return [rel, ps]; }).sort((a, b) => recent(a[0], b[0]));
  for (const [rel, ps] of groups) {
    h += `<div class="prg">${prGroupHead(depOf(rel, 'production'), rel)}` + (ps[0] === rel ? prRow(rel, me, { tip: prTrip(rel) }) // straight into main
      : ps.sort(recent).map((p) => prRow(p, me, { tip: prTrip(p, rel) })).join('') + prTo(ps.length > 1 ? 'released together in' : 'released in') + relRow(rel)) + '</div>';
  }
  if (other.length) h += `<details class="prg other"${el.querySelector('details.prg.other')?.open ? ' open' : ''}><summary class="prg-h">${ic('layers')}<b>${other.length} other PR${other.length > 1 ? 's' : ''}</b><span class="prg-at">syncs, closed, others' releases</span>${ic('angle-right', 'prg-chev')}</summary>` +
    other.sort((a, b) => recent(a[0], b[0])).map(([p, why]) => prRow(p, me, { right: `<span class="prl-tag">${esc(why)}</span>`, tip: prTrip(p), when: t(p) })).join('') + '</details>';
  return h;
}
function renderPR(prs, rels = [], me = '') {
  const el = $('#dr-pr');
  if (!prs.length) { el.hidden = true; el.dataset.html = ''; return; }
  const foldKey = prs[0].url; // the session's first PR stands for the card
  const folded = prFold[foldKey] ?? matchMedia('(max-width: 599px)').matches;
  const last = (p) => p.deploys?.at(-1);
  const tip = prs.map((p) => { const s = p.state === 'OPEN' ? prSummary(p) : null, d = (p.deploys || []).map((x) => `${x.env} ${x.state}`).join(', ');
    return `#${p.number} ${p.head} → ${p.base}: ${s ? `open · checks: ${s.ciText.toLowerCase()} · ${s.rv[2].toLowerCase()}` : p.state.toLowerCase()}${d ? ' · ' + d : ''}`; }).join('\n');
  // folded: the open ones' checks and review, then the latest merged one and where it's live
  const pill = (p) => { if (p.state === 'OPEN') { const { ci, rv } = prSummary(p); return `<span class="prc-pp"><b>#${p.number}</b>${ckIcon(ci, 'prc-ci-' + ci)}${ic(rv[1], 'prc-rv-' + rv[0])}</span>`; }
    const d = last(p); return `<span class="prc-pp">#${p.number}${d ? `<span class="dep ${d.state}">${depIcon(d)}${ENV[d.env] || ''}</span>` : ic('check-circle-f', 'prc-merged')}</span>`; };
  const latest = prs.filter((p) => p.state === 'MERGED').sort((a, b) => (b.mergedAt || '').localeCompare(a.mergedAt || ''))[0];
  const pills = [...prs.filter((p) => p.state === 'OPEN').slice(matchMedia('(max-width: 599px)').matches ? -1 : -2), latest].filter(Boolean); // a phone: what fits
  const repos = [...new Set(prs.map((p) => p.repo || ''))];
  // the button opens the card above it and, open, closes it (so does a tap outside it, or Esc)
  const btn = folded
    ? `<button type="button" class="prc-pill" data-prfold="0" aria-expanded="false" title="${esc(tip)}" aria-label="${esc('Pull requests\n' + tip)}">${ic('hierarchy')}<span class="prc-cnt">${prs.length}</span>${pills.map(pill).join('<span class="prc-sep"></span>')}</button>`
    : `<button type="button" class="prc-pill on" data-prfold="1" aria-expanded="true" title="Close (Esc)" aria-label="Close the pull requests">${ic('xmark')}<span>Close</span></button>`;
  const h = (folded ? '' : `<div class="prc-card"><div class="prc-top">${ic('hierarchy')}<b>Pull requests</b><span class="prc-cnt">${prs.length}</span></div>` +
      repos.map((r) => (repos.length > 1 ? `<div class="prc-repo" title="${esc(r)}">${ic('folder')}${esc(r.split('/').pop() || 'Other')}</div>` : '') +
        prRepo(prs.filter((p) => (p.repo || '') === r), rels.filter((p) => p.repo === r), el, me)).join('') + '</div>') + btn;
  if (el.hidden) {
    const b = $('#dr-body'), end = b.scrollHeight - b.scrollTop - b.clientHeight < 80;
    el.hidden = false;
    if (end) b.scrollTop = b.scrollHeight; // the chat's bottom room just grew: its live line stays above the button
  }
  if (el.dataset.html === h) return;
  const keep = el.querySelector('.prc-card')?.scrollTop ?? 0; // a look down the list stays put across refreshes
  el.innerHTML = h; el.dataset.html = h;
  const card = el.querySelector('.prc-card');
  if (card) { fitPR(); card.scrollTop = keep; } // it opens at the newest, on top
}
// the card is as tall as the room between the chat's top and the button, whatever the header, the safe areas
// and the keyboard leave (a guess in CSS hid its top under the header on an iPhone)
function fitPR() {
  const card = $('#dr-pr .prc-card');
  if (!card) return;
  const room = Math.floor(card.getBoundingClientRect().bottom - $('#dr-body').getBoundingClientRect().top - 10);
  const mh = Math.max(120, room) + 'px';
  if (card.style.maxHeight !== mh) card.style.maxHeight = mh;
}
addEventListener('resize', fitPR);
visualViewport?.addEventListener('resize', fitPR);
// opening and closing animate (app.css .prc.opening / .closing): only then, not on the card's refreshes
function foldPR(fold) {
  const prs = PRS.get(D.key)?.prs, el = $('#dr-pr');
  if (!prs?.length) return;
  const apply = () => {
    prFold[prs[0].url] = fold;
    store.set('cp-pr-fold', JSON.stringify(prFold));
    el.dataset.html = '';
    renderPR(prs, PRS.get(D.key).rels, PRS.get(D.key).me);
  };
  clearTimeout(el._t);
  el.classList.remove('opening', 'closing');
  if (fold && el.querySelector('.prc-card') && !calm.matches) {
    el.classList.add('closing');
    el._t = setTimeout(() => { el.classList.remove('closing'); apply(); }, 170);
    return;
  }
  apply();
  if (!fold && !calm.matches) {
    void el.offsetWidth; // restart: a quick reopen plays again
    el.classList.add('opening');
    el._t = setTimeout(() => el.classList.remove('opening'), 320);
  }
}
$('#dr-pr').addEventListener('click', (e) => { const b = e.target.closest('[data-prfold]'); if (b) foldPR(b.dataset.prfold === '1'); });
document.addEventListener('click', (e) => { if ($('#dr-pr .prc-card') && !e.composedPath().includes($('#dr-pr'))) foldPR(true); }); // a tap outside closes it (the path as it was: the tap that opened it re-rendered its target away)
function renderModel(p) {
  const btn = $('#dr-model');
  btn.hidden = !p?.agent;
  const m = currentModel();
  const lvl = EFFORTS.indexOf(m?.effort);
  setText($('#dr-model-t'), m ? modelName(m.id) : 'Model');
  $$('#dr-model-b i').forEach((b, i) => b.classList.toggle('on', i <= lvl));
  setAttr(btn, 'title', m ? `${modelName(m.id)}${m.effort ? ' · ' + m.effort + ' effort' : ''}` : 'Model and effort');
  if (!$('#dr-models').hidden) renderModels();
}
// a small menu: four models, then effort as the owner's slider (drag, click or arrow keys; the level goes
// to Claude as /effort when you let go). The model list re-renders with the drawer; the slider is built once
// and only told the level, so a live update never interrupts a drag.
const EFF_NAMES = { low: 'Low', medium: 'Medium', high: 'High', xhigh: 'Extra High', max: 'Max' };
let effUI = null;
function renderModels() {
  const m = currentModel();
  const box = $('#dr-models');
  if (!effUI) {
    box.innerHTML = '<div class="mp-list"></div><div class="mp-eff"><div class="mp-eh"><span class="mp-l">Effort</span><span class="mp-v"></span></div><div class="eff"></div></div><div class="mp-n" hidden>Applies when this turn ends</div>';
    effUI = effSlider($('.eff', box), {
      show: (lv) => { $('.mp-v', box).textContent = EFF_NAMES[lv]; },
      commit: (lv) => { if (lv !== currentModel()?.effort) setModel(undefined, lv); },
    });
  }
  $('.mp-list', box).innerHTML = MODELS.map(([id, name, d]) =>
    `<button type="button" class="mp-o${m?.id === id ? ' on' : ''}" data-setmodel="${id}" title="${esc(d)}"><span>${esc(name)}</span>${m?.id === id ? ic('check') : ''}</button>`).join('');
  effUI.set(m?.effort, m ? modelName(m.id) : '');
  $('.mp-n', box).hidden = !agentBusy();
}
function effSlider(root, opt) {
  const n = EFFORTS.length;
  const st = { value: 2, display: 2, open: 0, reveal: 0, textW: 64, plateau: effPlateau(64), dragging: false, model: '', keyT: 0 };
  const tw = {};
  let raf = 0;
  const anim = (k, to, dur, delay = 0) => {
    if (calm.matches) { st[k] = to; delete tw[k]; return draw(); }
    tw[k] = { from: st[k], to, t0: performance.now() + delay, dur };
    if (!raf) raf = requestAnimationFrame(frame);
  };
  const frame = (now) => {
    raf = 0;
    for (const [k, a] of Object.entries(tw)) {
      const t = (now - a.t0) / a.dur;
      if (t < 0) continue;
      st[k] = a.from + (a.to - a.from) * easeOut3(Math.min(1, t));
      if (t >= 1) delete tw[k];
    }
    draw();
    if (Object.keys(tw).length) raf = requestAnimationFrame(frame);
  };
  const heights = [4.8, 6, 7.2, 8.4, 9.6];
  root.innerHTML = `<svg class="eff-shell" viewBox="0 0 ${EFF.TRACK} ${EFF.BUMP_H + EFF.HEIGHT}" aria-hidden="true"><defs><linearGradient id="eff-g" x1="0" y1="0" x2="0" y2="1"><stop offset="0" class="eff-s1"/><stop offset="1" class="eff-s2"/></linearGradient></defs><path class="eff-path" fill="url(#eff-g)"/></svg>` +
    '<div class="eff-label"><span class="eff-swap"><span class="eff-cur"></span><span class="eff-lvl"></span></span></div>' +
    `<div class="eff-track" role="slider" tabindex="0" aria-label="Reasoning effort" aria-valuemin="0" aria-valuemax="${n - 1}"><div class="eff-fill"></div><div class="eff-thumb"></div>${EFFORTS.map(() => '<span class="eff-tick"></span>').join('')}</div>`;
  const [path, label, swap, cur, lvl, track, fill, thumb] = ['.eff-path', '.eff-label', '.eff-swap', '.eff-cur', '.eff-lvl', '.eff-track', '.eff-fill', '.eff-thumb'].map((q) => $(q, root));
  const ticks = $$('.eff-tick', root);
  ticks.forEach((t, i) => { t.style.left = effCenter(i, n) + 'px'; t.style.height = heights[i] + 'px'; });
  thumb.style.height = EFF.REST_H + 'px';
  thumb.style.marginTop = -EFF.REST_H / 2 + 'px';
  function draw() {
    const center = effCenter(st.display, n);
    const w = EFF.REST_W + (EFF.DRAG_W - EFF.REST_W) * st.open;
    let left = clampN(center - w / 2, EFF.EDGE_PAD, EFF.TRACK - EFF.EDGE_PAD - w);
    const right = left + w;
    fill.style.width = Math.min(EFF.TRACK, right + EFF.EDGE_PAD) + 'px';
    thumb.style.left = left + 'px';
    thumb.style.width = w + 'px';
    const active = Math.round(st.display);
    ticks.forEach((t, i) => {
      const x = effCenter(i, n);
      const on = x >= left + 0.5 && x <= right - 0.5;
      t.dataset.onThumb = on;
      t.dataset.active = i === active && !on;
    });
    path.setAttribute('d', effShellPath(center, st.open, st.plateau));
    const g = effShellGeom(center, st.plateau);
    label.style.left = (g.topL + g.topR) / 2 + (g.insetL - g.insetR) / 2 + 'px';
    label.style.opacity = st.reveal;
    label.style.filter = st.reveal > 0.98 ? '' : `blur(${((1 - st.reveal) * 3).toFixed(2)}px)`;
    label.style.transform = `translate(-50%, ${3 * (1 - st.open)}px)`;
    const name = EFF_NAMES[EFFORTS[active]];
    if (lvl.textContent !== name || cur.textContent !== st.model) {
      const swapped = lvl.textContent && lvl.textContent !== name;
      cur.textContent = st.model;
      lvl.textContent = name;
      if (swapped && !calm.matches) swap.animate([{ filter: 'blur(2.5px)', opacity: 0.35 }, { filter: 'blur(0)', opacity: 1 }], { duration: 180, easing: 'ease-out' });
      const tw2 = label.getBoundingClientRect().width;
      if (tw2 > 0 && Math.abs(tw2 - st.textW) > 0.4) { st.textW = tw2; anim('plateau', effPlateau(tw2), 90); }
    }
    track.setAttribute('aria-valuenow', active);
    track.setAttribute('aria-valuetext', `${st.model} ${name}`.trim());
    opt.show(EFFORTS[active]);
  }
  const send = (ms) => { // until it's sent, the level Claude reports doesn't move the slider back
    clearTimeout(st.keyT);
    st.keyT = setTimeout(() => { st.keyT = 0; opt.commit(EFFORTS[st.value]); }, ms);
  };
  const valueFrom = (x) => { const r = track.getBoundingClientRect(); return clampN(((x - r.left - EFF.RADIUS) / (r.width - EFF.RADIUS * 2)) * (n - 1), 0, n - 1); };
  track.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    st.dragging = true;
    try { track.setPointerCapture(e.pointerId); } catch { /* an untrusted event */ }
    root.dataset.dragging = 'true';
    delete tw.display;
    st.value = st.display = valueFrom(e.clientX);
    anim('open', 1, 220);
    anim('reveal', 1, 132, 85);
    draw();
  });
  track.addEventListener('pointermove', (e) => { if (st.dragging) { st.value = st.display = valueFrom(e.clientX); draw(); } });
  const release = (e) => {
    if (!st.dragging) return;
    st.dragging = false;
    try { if (track.hasPointerCapture(e.pointerId)) track.releasePointerCapture(e.pointerId); } catch { /* an untrusted event */ }
    root.dataset.dragging = 'false';
    st.value = Math.round(st.value);
    anim('open', 0, 220);
    anim('display', st.value, 220);
    anim('reveal', 0, 90);
    send(calm.matches ? 0 : 260); // let it settle on the stop first
  };
  track.addEventListener('pointerup', release);
  track.addEventListener('pointercancel', release);
  track.addEventListener('keydown', (e) => {
    const now = Math.round(st.value);
    const next = { ArrowRight: now + 1, ArrowUp: now + 1, ArrowLeft: now - 1, ArrowDown: now - 1, Home: 0, End: n - 1, Enter: now }[e.key];
    if (next === undefined) return;
    e.preventDefault();
    st.value = st.display = clampN(next, 0, n - 1);
    draw();
    send(e.key === 'Enter' ? 0 : 700); // a run of arrow presses sends one /effort
  });
  return {
    set(effort, model) { // the level Claude reports, unless you're moving it
      st.model = model;
      const i = EFFORTS.indexOf(effort);
      if (!st.dragging && !st.keyT && i >= 0 && i !== Math.round(st.value)) { st.value = i; anim('display', i, 220); }
      draw();
    },
  };
}
function toggleModels(force) {
  const box = $('#dr-models');
  const open = force ?? box.hidden;
  box.hidden = !open;
  $('#dr-model').setAttribute('aria-expanded', String(open));
  if (open) { renderModels(); box.style.left = $('#dr-model').offsetLeft + 'px'; }
}
async function setModel(id, effort) {
  toggleModels(false);
  const text = id ? `/model ${id}` : `/effort ${effort}`;
  if (await sendInput({ text, enter: true })) toast(id ? `Asked Claude to switch to ${modelName(id)}: confirm in the chat` : `Effort set to ${EFF_NAMES[effort] || effort}, and saved as the default for new sessions`); // Claude Code's /effort saves it
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

// a chat in the drawer has the Assistant's aurora (aurora.js) under its composer, livelier while its agent works
function drAurora(p) {
  const c = $('#dr-aurora');
  if (D.key && D.mode === 'chat' && !$('#drawer').hidden) {
    aurora(c);
    auroraEnergy(['working', 'stalled', 'background'].includes(p?.state));
  } else if (AU.canvas === c) aurora(S.view === 'ai' ? $('.ai-aurora') : null);
}

function renderDrawer() {
  if (!D.key) return;
  const f = findPane(D.session, D.pane);
  const chat = D.mode === 'chat';
  const seg = $$('#drawer [data-mode]');
  // every update comes through here: each write only when it changes something (setText…), so a steady chat
  // doesn't repaint its header and chat box every second (the frosted chat box would re-blur the aurora)
  for (const b of seg) setAttr(b, 'aria-pressed', String(b.dataset.mode === D.mode));
  const on = seg.find((b) => b.dataset.mode === D.mode);
  const pill = $('#drawer .seg-pill');
  if (on && on.offsetWidth) {
    const w = on.offsetWidth + 'px', x = `translateX(${on.offsetLeft - 2}px)`;
    if (pill.style.width !== w) pill.style.width = w;
    if (pill.style.transform !== x) pill.style.transform = x;
  }
  setHidden($('#dr-body'), !chat);
  setHidden($('#dr-foot'), !chat);
  setHidden($('#dr-term'), chat);
  $('#drawer').classList.toggle('chatmode', chat);
  drAurora(f?.p);
  setHidden($('#dr-keys'), chat || !coarse);
  renderModel(f?.p);
  renderCtx(f?.p);
  renderCompact(f?.p);
  syncPR(f?.p);
  renderRail();
  renderSubHead();
  if (!f) { $('#dr-sub').textContent = `${D.session} · pane ${D.pane} is gone`; $('#dr-prompt').hidden = true; return; }
  const { s, tb, p } = f;
  const prev = D.state;
  // a resize's frame comes before its new size: cropped to the old size it lost its bottom and its right side
  const resized = D.raw && (p.cols !== D.cols || p.rows !== D.rows);
  Object.assign(D, { cols: p.cols, rows: p.rows, state: p.state, root: p.git?.root || D.root });
  if (resized) { D.view = fitScreen(D.raw, D.rows, D.cols); if (D.mode === 'screen') renderBody(); }
  setDisabled(seg[0], !p.agent?.chat && !p.agent?.pid); // a Claude with no message yet: its chat starts empty
  setHidden($('#dr-filesbtn'), !p.agent?.chat);
  setText($('#dr-title'), tb.panes.length > 1 ? (p.title || `pane ${p.id}`) : tb.name);
  const sub = whereChips(s, p);
  if ($('#dr-sub').dataset.html !== sub) { $('#dr-sub').innerHTML = sub; $('#dr-sub').dataset.html = sub; }
  const [cls, label] = TSTATE[p.state] || ['', p.state];
  setAttr($('#dr-pill'), 'class', 'pill ' + cls);
  setText($('#dr-pill'), label);
  const pr = $('#dr-prompt');
  const q = apQ(p);
  const sent = p.state === 'asking' && D.apSent?.key === D.key && Date.now() < D.apSent.until && (!q || q.sig === D.apSent.sig);
  if (p.state !== 'asking' || (q && !sent)) D.apSent = null;
  setHidden(pr, p.state !== 'asking');
  const html = sent ? `<div class="ap ap-sent" role="status"><i class="spin"></i><span>Answer sent · Claude is moving on…</span></div>`
    : q ? apHTML(q)
    : `<div class="ap"><div class="ap-q">${ic('help-circle')}<span>${esc(p.detail || 'Claude needs your answer')}</span></div><div class="muted">Switch to Terminal to see and answer it.</div></div>`;
  if (pr.dataset.html !== html) { // don't swap rows under a finger (or a box being typed in) for nothing
    const focused = pr.contains(document.activeElement);
    pr.innerHTML = html;
    pr.dataset.html = html;
    if (q && !sent) {
      if (D.ans?.sig !== q.sig) apInit(q);
      else D.ans.busy = false; // back from "Answer sent": the dialog didn't move on, it can be answered again
      apSync(true);
      if (focused) $('.ap-o[tabindex="0"]', pr)?.focus(); // answering with the keys: on to the next question
    }
  }
  D.denyAt = (!sent && q?.denyAt) || 0;
  apDeny();
  updateSend();
  if (chat && prev !== p.state) renderBody(); // live indicator follows the state
}

// ---- a question on the agent's screen: its card (beautifului's approval card) ----
// Pick, then Continue: a stray tap on a phone answers nothing. The card follows the dialog as the server reads
// it: the questions as steps, a description under each option, a box for your own answer, checkboxes when
// several may be picked, and the answers to review before they're submitted. The server answers with arrows
// and pastes the text (POST /api/pane/input, answer), then reads the screen again: the next question shows at
// once (D.apNext), not when the tracker next looks.
function apQ(p) {
  const q = p.state === 'asking' ? p.agent?.prompt || null : null;
  const nx = D.apNext;
  if (nx?.key === D.key && Date.now() < nx.until && (!q || q.sig === nx.from)) return nx.prompt; // the stream hasn't caught up
  return q;
}
// what's picked, as the dialog has it: the highlighted option, the ticks, the text in the free-text option
function apInit(q) {
  const opts = q.opts || [];
  D.ans = {
    sig: q.sig,
    pick: q.multi ? '' : (opts.find((o) => o.sel && !o.next) || opts[0])?.label,
    checks: new Set(opts.filter((o) => o.checked && !o.text).map((o) => o.label)),
    text: opts.find((o) => o.text)?.value || '',
    busy: false,
    err: '',
  };
}
function apHTML(q) {
  const cur = (q.tabs || []).findIndex((t) => !t.done);
  const step = (label, done, on) => `<span class="ap-st${done ? ' done' : ''}${on ? ' cur' : ''}">${done ? ic('check') : '<i></i>'}${esc(label)}</span>`;
  const steps = q.tabs?.length ? `<div class="ap-steps" aria-label="Questions">${q.tabs.map((t, i) => step(t.label, t.done, i === cur)).join('')}${step('Submit', false, cur < 0)}</div>` : '';
  const row = (o) => {
    const box = q.multi && o.box;
    return `<div class="ap-o${o.text ? ' ap-txt' : ''}" role="${box ? 'checkbox' : 'radio'}" tabindex="-1" aria-checked="false" data-pick="${esc(o.label)}"${box ? ' data-box' : ''}${o.text ? ' data-text' : ''}>` +
      (box ? `<span class="ap-c">${ic('check')}</span>` : '<span class="ap-r"></span>') + (o.n ? `<span class="ap-n">${esc(o.n)}.</span>` : '') +
      (o.text ? '<input class="ap-in" data-aptext type="text" placeholder="Type your own answer…" aria-label="Your own answer" autocomplete="off" spellcheck="true" enterkeyhint="send">'
        : `<span class="ap-l"><span>${esc(o.label)}</span>${o.desc ? `<small>${esc(o.desc)}</small>` : ''}</span>`) + '</div>';
  };
  const next = (q.opts || []).find((o) => o.next);
  return `<div class="ap" data-sig="${esc(q.sig)}">${steps}<div class="ap-q">${ic('help-circle')}<span>${esc(q.q || 'Claude needs your answer')}</span></div>` +
    ((q.ctx || []).length && !q.review?.length ? `<pre class="ap-ctx">${esc(q.ctx.join('\n'))}</pre>` : '') + // the review's own heading says less than the review
    (q.review?.length ? `<div class="ap-rev">${q.review.map((r) => `<div><span>${esc(r.q)}</span><b>${esc(r.a)}</b></div>`).join('')}</div>` : '') +
    (q.denyAt ? `<div class="ap-deny">${ic('clock-circle')}<span></span></div>` : '') + // apDeny counts it down
    `<div class="ap-opts" role="${q.multi ? 'group' : 'radiogroup'}" aria-label="Answers">${(q.opts || []).filter((o) => !o.next).map(row).join('')}</div>` +
    `<div class="ap-f"><span class="hint ap-hint" role="alert"></span><button type="button" class="btn sm" data-skip title="Sends Esc">Cancel</button>` +
    `<button type="button" class="btn sm primary" data-choose>${esc(q.multi ? next?.label || 'Next' : 'Continue')}</button></div></div>`;
}
// the picks onto the card; fresh: the card was just built (fill the free-text box too)
function apSync(fresh) {
  const pr = $('#dr-prompt'), a = D.ans;
  if (!a || !$('.ap[data-sig]', pr)) return;
  for (const r of $$('.ap-o', pr)) {
    const on = r.dataset.box !== undefined ? (r.dataset.text !== undefined ? !!a.text.trim() : a.checks.has(r.dataset.pick)) : a.pick === r.dataset.pick;
    r.setAttribute('aria-checked', String(on));
    r.tabIndex = (r.dataset.box !== undefined ? r === $('.ap-o', pr) : on) ? 0 : -1;
  }
  if (fresh) { const t = $('[data-aptext]', pr); if (t) t.value = a.text; }
  const multi = !!$('.ap-o[data-box]', pr);
  const hint = $('.ap-hint', pr);
  const say = a.err || (coarse ? '' : multi ? '↑↓ to move · Space ticks · Enter goes on' : '↑↓ to choose · Enter to continue');
  if (hint.textContent !== say) hint.textContent = say; // it's announced: only when it changes
  hint.classList.toggle('bad', !!a.err);
  for (const b of $$('.ap-f button', pr)) b.disabled = a.busy;
  const go = $('[data-choose]', pr);
  if (a.busy && !go.dataset.label) { go.dataset.label = go.textContent; go.innerHTML = '<i class="spin"></i>Sending'; }
  else if (!a.busy && go.dataset.label) { go.textContent = go.dataset.label; delete go.dataset.label; }
}
// Claude Code's own countdown on the dialog, ticking here and not in the card's HTML: a card rebuilt every second
// would drop the tap that lands as it's swapped
function apDeny() {
  const t = D.denyAt && $('#dr-prompt .ap-deny span');
  if (!t) return;
  const s = Math.max(0, Math.ceil((D.denyAt - Date.now()) / 1000));
  setText(t, `Claude Code will deny this by itself in ${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`);
}
setInterval(apDeny, 1000);
function apPick(row, typing) {
  const a = D.ans;
  if (!a || a.busy) return;
  a.err = '';
  if (row.dataset.box === undefined) a.pick = row.dataset.pick; // a radio, or a multi-select's "Chat about this"
  else if (row.dataset.text === undefined) { a.pick = ''; a.checks.has(row.dataset.pick) ? a.checks.delete(row.dataset.pick) : a.checks.add(row.dataset.pick); }
  else a.pick = '';
  if (row.dataset.text !== undefined && !typing) $('[data-aptext]', row).focus();
  apSync();
}
async function apSend() {
  const f = findPane(D.session, D.pane), q = f && apQ(f.p), a = D.ans;
  if (!q || !a || a.busy || a.sig !== q.sig) return;
  const textOpt = (q.opts || []).find((o) => o.text);
  const text = a.text.trim();
  let answer;
  if (q.multi && !a.pick) answer = { checks: [...a.checks], text };
  else if (a.pick === textOpt?.label) {
    if (!text) { a.err = 'Type your answer first.'; apSync(); $('#dr-prompt [data-aptext]')?.focus(); return; }
    answer = { choose: a.pick, text };
  } else answer = { choose: a.pick };
  a.busy = true;
  a.err = '';
  apSync();
  const key = D.key;
  try {
    const r = await api('/api/pane/input', { session: D.session, pane: D.pane, sig: q.sig, answer });
    if (r.status === 200) {
      D.apSent = { key, sig: q.sig, until: Date.now() + 6000 };
      if (r.moved) D.apNext = { key, from: q.sig, prompt: r.prompt || null, until: Date.now() + 5000 };
      if (D.key === key) renderDrawer();
      return;
    }
    a.err = r.status === 409 ? 'That question changed on screen: this is the one there now.' : r.error || `Not sent (error ${r.status})`;
  } catch { a.err = "Not sent: the panel can't be reached."; }
  a.busy = false;
  if (D.key === key) apSync();
}
$('#dr-prompt').addEventListener('input', (e) => {
  if (!e.target.matches('[data-aptext]') || !D.ans) return;
  D.ans.text = e.target.value;
  apPick(e.target.closest('.ap-o'), true);
});
$('#dr-prompt').addEventListener('focusin', (e) => { if (e.target.matches('[data-aptext]') && D.ans && !$('.ap-o[data-box]', $('#dr-prompt'))) apPick(e.target.closest('.ap-o'), true); });
$('#dr-prompt').addEventListener('keydown', (e) => {
  if (!D.ans || D.ans.busy || e.target.closest('button') || e.isComposing) return;
  const rows = $$('#dr-prompt .ap-o'), row = e.target.closest('.ap-o'), typing = e.target.matches('[data-aptext]');
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    e.preventDefault();
    const to = rows[Math.max(0, Math.min(rows.length - 1, rows.indexOf(row) + (e.key === 'ArrowDown' ? 1 : -1)))];
    if (!to) return;
    if (to.dataset.box === undefined) apPick(to, true); // a radio follows the focus
    ($('[data-aptext]', to) || to).focus();
  } else if (e.key === 'Enter') {
    e.preventDefault();
    apSend();
  } else if (e.key === ' ' && row && !typing) {
    e.preventDefault();
    apPick(row);
  } else if (/^[1-9]$/.test(e.key) && !typing) {
    const to = rows.find((r) => $('.ap-n', r)?.textContent === e.key + '.');
    if (to) { e.preventDefault(); apPick(to); if (to.dataset.text === undefined) to.focus(); }
  }
});

const MAX_PASTE = 100 << 10; // the server's maxPaste: one terminal paste
const PENDING = new Map(); // pane key -> [{ id, shown, text, at, st, ackAt, err }]: from the composer, not in the chat yet
// The outbox: Enter shows the message at once (sending), and it goes out behind it, in order per chat. The
// network is retried; a refusal stops the queue there, so later messages never overtake one that failed.
const OUTBOX = new Map(); // pane key -> { busy, jobs }
let pendSeq = 0;
const sleep = (ms) => new Promise((ok) => setTimeout(ok, ms));
const repaint = (key) => { if (D.key === key && D.mode === 'chat') renderBody(true); };
function queueSend(key, session, pane, shown, files) {
  const e = { id: ++pendSeq, shown, text: shown, files, session, pane, at: now(), st: 'sending' };
  PENDING.set(key, [...(PENDING.get(key) || []), e]);
  enqueue(key, e);
}
function enqueue(key, e) {
  const ob = OUTBOX.get(key) || { busy: false, jobs: [] };
  OUTBOX.set(key, ob);
  ob.jobs.push(e);
  drain(key);
}
async function drain(key) {
  const ob = OUTBOX.get(key);
  if (ob.busy) return;
  ob.busy = true;
  while (ob.jobs.length) {
    const ok = await deliver(key, ob.jobs[0]);
    ob.jobs.shift();
    if (!ok) for (const j of ob.jobs.splice(0)) { j.st = 'failed'; j.err = 'held back: the message before it was not sent'; }
  }
  ob.busy = false;
  repaint(key);
}
async function deliver(key, e) {
  e.st = 'sending';
  e.err = '';
  for (let attempt = 0; ; attempt++) {
    try {
      if (e.paths === undefined) { // uploads first: agents get files by path, as if dropped into their terminal
        const paths = [];
        for (const f of e.files || []) paths.push(await filePath(f));
        let msg = e.shown;
        if (new Blob([msg]).size > MAX_PASTE) { // too long to paste into a terminal: the agent reads it from a file
          paths.push(await uploadFile(new File([msg], 'message.txt', { type: 'text/plain' })));
          msg = 'My message is in this file (too long to paste): read it in full.';
        }
        e.paths = paths;
        e.text = [...paths, msg].filter(Boolean).join(' ');
      }
      const r = await api('/api/pane/input', { session: e.session, pane: e.pane, text: e.text, enter: true, clear: !!e.clear });
      if (r.status === 200) { e.st = 'sent'; e.ackAt = now(); for (const f of e.files || []) dropURL(f); repaint(key); return true; }
      if (r.status === 401 && r.error === 'login required') { e.st = 'failed'; e.err = 'signed out'; closePane(); showLogin('Session ended — enter your PIN.'); return false; }
      if (r.status < 500 || attempt >= 4) { // refused (bad pane, too long…): retrying won't help
        e.st = 'failed';
        e.err = r.error || 'error ' + r.status;
        e.box = r.box || ''; // Claude's input box wasn't empty: the bubble offers to clear it, or to edit both
        window.cpReport?.('send-fail', `${r.status} ${e.err} to ${key}`, '', true);
        if (e.shown.startsWith('/')) toast(`Not sent: ${e.err}`, true); // a command has no bubble to say so
        repaint(key);
        return false;
      }
    } catch (err) {
      if (attempt >= 4) {
        e.st = 'failed';
        e.err = "the panel can't be reached (network, or the sign-in expired: reload)";
        window.cpReport?.('send-fail', `network: ${err?.message || err} to ${key}`, '', true);
        if (e.shown.startsWith('/')) toast(`Not sent: ${e.err}`, true);
        repaint(key);
        return false;
      }
    }
    e.slow = true; // the network or a restarting panel: try again shortly
    repaint(key);
    await sleep(1000 * 2 ** attempt);
  }
}
async function sendInput(body) {
  try {
    const r = await api('/api/pane/input', { session: D.session, pane: D.pane, ...body });
    if (r.status === 200) {
      if (D.mode === 'chat' && (body.session ?? D.session) === D.session && (body.pane ?? D.pane) === D.pane) toLatest(); // you spoke: see the answer come in
      return true;
    }
    if (r.status === 401 && r.error === 'login required') { closePane(); showLogin('Session ended — enter your PIN.'); return false; }
    toast(`Not sent: ${r.error || 'error ' + r.status}`, true);
    window.cpReport?.('send-fail', `${r.status} ${r.error || ''} to ${body.session ?? D.session}:${body.pane ?? D.pane}`);
  } catch (err) {
    toast("Not sent: the panel can't be reached (network, or the sign-in expired: reload)", true);
    window.cpReport?.('send-fail', `network: ${err?.message || err} to ${body.session ?? D.session}:${body.pane ?? D.pane}`);
  }
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
  if (b.dataset.icon !== String(stop)) { b.innerHTML = stop ? IC.stop : IC.up; b.dataset.icon = String(stop); }
  updateCmds();
  b.classList.toggle('stop', stop);
  setHidden($('#dr-clear'), empty);
  setAttr(b, 'aria-label', stop ? 'Stop (Esc)' : 'Send');
  setAttr(b, 'title', stop ? 'Stop (Esc)' : 'Send');
  setDisabled(b, empty && !stop);
}
function renderFiles() {
  const box = $('#dr-files-chips');
  box.hidden = !D.files.length;
  box.innerHTML = D.files.map((f, i) => f.url
    ? `<div class="fchip${f.path ? '' : ' up'}"><img src="${esc(f.url)}" alt=""><button type="button" data-rmfile="${i}" aria-label="Remove">${ic('xmark')}</button></div>`
    : `<div class="fchip doc${f.path ? '' : ' up'}">${fxBadge(f.name)}<span>${esc(f.name)}</span><small>${fmtB(f.size)}</small><button type="button" data-rmfile="${i}" aria-label="Remove">${ic('xmark')}</button></div>`).join('');
  updateSend();
}
// An attachment uploads the moment it's added, so a chat you leave (or a reload, or the phone dropping the
// page) keeps it: the draft holds its path on the server (keepFiles), and sending uses that path.
function addFiles(list) {
  const key = D.key;
  for (const file of list) {
    if (file.size > 25 << 20) { toast(`${file.name}: files up to 25 MB.`, true); continue; }
    if (D.files.length >= 10) break;
    const f = { file, name: file.name || 'pasted', size: file.size, img: /^image\/(png|jpeg|gif|webp)$/.test(file.type), path: '' };
    f.url = f.img ? URL.createObjectURL(file) : '';
    f.up = uploadFile(file).then((p) => { f.path = p; keepFiles(key); if (D.key === key) renderFiles(); }, () => {}); // sending tries again
    D.files.push(f);
  }
  renderFiles();
}
// an attachment's path on the server: the composer's (uploading since it was added), or a plain File (a peek's photo)
const filePath = async (f) => { if (f instanceof Blob) return uploadFile(f); await f.up; return f.path || uploadFile(f.file); };
const dropURL = (f) => { if (f.url?.startsWith('blob:')) URL.revokeObjectURL(f.url); };
function clearFiles() { for (const f of D.files) dropURL(f); D.files = []; renderFiles(); }

// ---- drafts: what you were typing (and attaching) stays with its chat until you send it ----
// Kept in this browser, so they survive a reload: the text, and the attachments as their paths on the server.
const DRAFTS = (() => { try { return JSON.parse(store.get('cp-drafts', '{}')) || {}; } catch { return {}; } })();
const DFILES = (() => { try { return JSON.parse(store.get('cp-draft-files', '{}')) || {}; } catch { return {}; } })();
const FILES = new Map(); // pane key → attachments waiting in a chat you left (an upload may still be going)
function keepFiles(key) {
  if (!key) return;
  const fs = (key === D.key ? D.files : FILES.get(key)) || [];
  const up = fs.filter((f) => f.path).map((f) => ({ path: f.path, name: f.name, size: f.size, img: f.img }));
  if (up.length) DFILES[key] = up; else delete DFILES[key];
  store.set('cp-draft-files', JSON.stringify(DFILES));
}
let draftT = 0;
const flushDrafts = () => { clearTimeout(draftT); store.set('cp-drafts', JSON.stringify(DRAFTS)); };
function saveDraft(key = D.key, later = true) {
  if (!key) return;
  const v = $('#dr-text').value;
  if (v.trim()) DRAFTS[key] = v; else delete DRAFTS[key];
  clearTimeout(draftT);
  if (later) draftT = setTimeout(flushDrafts, 400); else flushDrafts();
}
function stashDraft() { // leaving a chat
  if (!D.key) return;
  saveDraft(D.key, false);
  if (D.files.length) FILES.set(D.key, D.files); else FILES.delete(D.key);
  D.files = [];
  $('#dr-text').value = '';
}
function restoreDraft() { // entering one
  $('#dr-text').value = DRAFTS[D.key] || '';
  D.files = FILES.get(D.key) || (DFILES[D.key] || []).map((x) => ({ ...x, url: x.img ? '/api/upload/' + encodeURIComponent(x.path.split('/').pop()) : '' }));
  FILES.delete(D.key);
  renderFiles();
  autosize();
}
addEventListener('pagehide', () => saveDraft(D.key, false));

const autosize = () => { const t = $('#dr-text'); t.style.height = 'auto'; t.style.height = Math.min(t.scrollHeight, 200) + 'px'; updateSend(); };
$('#dr-text').addEventListener('input', () => { autosize(); saveDraft(); });
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
  setHidden(box, !C.list.length);
  setAttr($('#dr-text'), 'aria-expanded', String(!!C.list.length));
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

// copy to the clipboard from a button, which shows a tick for a moment (every chat's: the terminals', JARVIS's)
async function copy(text, btn) {
  try {
    await navigator.clipboard.writeText(text);
    const was = btn.innerHTML;
    btn.classList.add('done');
    if (btn.querySelector('use')) btn.querySelector('use').setAttribute('href', '#i-copy-success');
    setTimeout(() => { btn.classList.remove('done'); btn.innerHTML = was; }, 1200);
  } catch { toast('Copy failed', true); }
}

// the chat on screen, for quoting and the selection bar: a terminal's (in the drawer) or JARVIS's
const chatHost = () => (D.key ? (D.mode === 'chat' ? { body: $('#dr-body'), box: $('#dr-text') } : null) : S.view === 'ai' && $('#ai') ? { body: $('#ai'), box: $('#ai-text') } : null);

// On a computer the chat box on screen keeps the cursor: dictation (Wispr Flow) pastes only into a focused text
// field, and Chrome leaves focus on whatever you clicked (Send, a row, the chat itself), so every other dictation
// went nowhere. After a click that leaves no text field focused, focus goes back to the box: the drawer's, the
// Terminals peek's, JARVIS's. Not after a keyboard's click, a selection, or in a menu, dialog or question card.
document.addEventListener('click', (e) => {
  if (coarse || !e.detail) return; // a phone: no keyboard springing up
  setTimeout(() => {
    const box = D.key ? D.mode === 'chat' && $('#dr-text') : (TV.peek && $('#tp-text')) || (S.view === 'ai' && $('#ai-text'));
    if (!box?.offsetParent || box.disabled || typingIn(document.activeElement) || getSelection().toString() ||
      $('dialog[open], #menu:not([hidden]), #cpop:not([hidden])') || e.target.closest?.('#dr-prompt, [role=listbox], [role=tablist]')) return;
    box.focus({ preventScroll: true });
  });
}, true); // ahead of the views' own handlers, which stop the click (a row, Open the chat)

// quote a passage (a selection, or a whole reply) into the chat box
function quoteIntoComposer(text) {
  text = String(text || '').trim();
  const t = chatHost()?.box;
  if (!text || !t) return;
  const q = text.split('\n').map((l) => '> ' + l).join('\n') + '\n\n';
  t.value = (t.value.trim() ? t.value.replace(/\s*$/, '\n\n') : '') + q;
  getSelection()?.removeAllRanges();
  $('#selbar').hidden = true;
  t.focus();
  t.setSelectionRange(t.value.length, t.value.length);
  if (t.id === 'dr-text') autosize();
  else t.dispatchEvent(new Event('input', { bubbles: true })); // JARVIS's box grows, saves its draft, lights Send
}

// selection actions: highlight a passage of the chat to quote or copy it
function updateSelbar() {
  const bar = $('#selbar');
  const sel = getSelection();
  const body = chatHost()?.body;
  if (!body || !sel || sel.isCollapsed || !sel.rangeCount || !body.contains(sel.getRangeAt(0).commonAncestorContainer) || !sel.toString().trim()) {
    bar.hidden = true;
    return;
  }
  const r = sel.getRangeAt(0).getBoundingClientRect();
  const box = body.id === 'ai' ? { top: 0, bottom: innerHeight } : body.getBoundingClientRect(); // JARVIS's chat is the page
  if (r.bottom < box.top || r.top > box.bottom) { bar.hidden = true; return; }
  bar.hidden = false;
  const w = bar.offsetWidth;
  const h = bar.offsetHeight;
  bar.style.left = Math.max(8, Math.min(innerWidth - w - 8, r.left + r.width / 2 - w / 2)) + 'px';
  bar.style.top = (coarse || r.top - h - 8 < box.top ? Math.min(r.bottom + 8, innerHeight - h - 8) : r.top - h - 8) + 'px';
}
document.addEventListener('selectionchange', updateSelbar);
$('#dr-body').addEventListener('scroll', () => { if (!$('#selbar').hidden) updateSelbar(); updateDown(); }, { passive: true });
addEventListener('scroll', () => { if (!$('#selbar').hidden && S.view === 'ai') updateSelbar(); }, { passive: true }); // JARVIS's chat scrolls the page
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
$('#dr-clear').addEventListener('click', () => { // empties the box: text, attachments, the draft
  $('#dr-text').value = '';
  clearFiles();
  keepFiles(D.key);
  autosize();
  saveDraft(D.key, false);
  $('#dr-text').focus();
});
$('#dr-file').addEventListener('change', (e) => { addFiles(e.target.files); e.target.value = ''; });
$('#dr-form').addEventListener('submit', (e) => {
  e.preventDefault();
  const text = $('#dr-text').value.trim();
  if (!text && !D.files.length) { if (agentBusy()) sendInput({ keys: ['Esc'] }); return; }
  queueSend(D.key, D.session, D.pane, text, D.files); // shows now; goes out behind it
  D.files = [];
  keepFiles(D.key);
  renderFiles();
  $('#dr-text').value = '';
  autosize();
  saveDraft(D.key, false);
  updateSend();
  renderBody(true);
  toLatest();
});
// A message waiting in Claude Code's queue (sent while it worked): cancelled on the server, which takes the queue
// back out of Claude's input box and puts the others back (unqueue.go). It shows "cancelling…" until the
// chat sees it gone.
const UNQ = new Map(); // pane key → { all, at, keep, done }: one cancel at a time per chat
async function unqueue(at) {
  const key = D.key, all = D.queued || [];
  if (UNQ.has(key) || all[at] == null) return;
  const un = { all, at, keep: all.filter((_, i) => i !== at), done: false };
  UNQ.set(key, un);
  repaint(key);
  let ok = false;
  try {
    const r = await api('/api/pane/unqueue', { session: D.session, pane: D.pane, text: all[at] });
    ok = r.status === 200;
    if (ok && r.cancelled) putInBox(all[at]);
    else if (ok) toast('Claude had already taken that message.', true);
    else toast(r.error || `Couldn't take it back (error ${r.status})`, true);
  } catch { toast("Couldn't take it back: the panel can't be reached.", true); }
  un.done = true;
  if (!ok) UNQ.delete(key); // nothing changed: the live list again
  repaint(key);
  setTimeout(() => { if (UNQ.get(key) === un) UNQ.delete(key); repaint(key); }, ok ? 3000 : 0); // by then the chat has caught up
}
// a message that wasn't sent: send it again, or take it back into the box to change it
$('#dr-body').addEventListener('click', (e) => {
  const u = e.target.closest('[data-unqueue]');
  if (u) return unqueue(+u.dataset.unqueue);
  const b = e.target.closest('[data-resend], [data-unsend]');
  if (!b) return;
  const list = PENDING.get(D.key) || [];
  const x = list.find((y) => String(y.id) === (b.dataset.resend || b.dataset.unsend));
  if (!x) return;
  if (b.dataset.resend) { x.at = now(); x.clear = b.dataset.clearbox !== undefined; enqueue(D.key, x); return repaint(D.key); }
  const ob = OUTBOX.get(D.key);
  if (ob) ob.jobs = ob.jobs.filter((j) => j !== x); // not sent yet: it leaves the outbox
  PENDING.set(D.key, list.filter((y) => y !== x));
  if (b.dataset.dismiss === undefined) putInBox(x.shown, x.files);
  renderBody(true);
});
// Edit on a message held by what Claude's box still holds: that comes out of its box, and both into yours
$('#dr-body').addEventListener('click', async (e) => {
  const b = e.target.closest('[data-takebox]');
  if (!b) return;
  const key = D.key, list = PENDING.get(key) || [];
  const x = list.find((y) => String(y.id) === b.dataset.takebox);
  if (!x) return;
  b.disabled = true;
  const r = await api('/api/pane/takebox', { session: D.session, pane: D.pane }).catch(() => null);
  if (!r || r.status !== 200) { b.disabled = false; return toast(r?.error || "Couldn't empty Claude's box: see the Terminal tab", true); }
  if (D.key !== key) return;
  PENDING.set(key, (PENDING.get(key) || []).filter((y) => y !== x));
  putInBox([r.text, x.shown].filter(Boolean).join('\n\n'), x.files);
  renderBody(true);
});
// putInBox: a message back in the chat box, after what's there, to change and send again
function putInBox(text, files = []) {
  const ta = $('#dr-text');
  ta.value = ta.value.trim() ? ta.value.replace(/\s+$/, '') + '\n\n' + text : text;
  if (files?.length) { D.files = D.files.concat(files); renderFiles(); keepFiles(D.key); }
  autosize();
  saveDraft();
  ta.focus();
  ta.setSelectionRange(ta.value.length, ta.value.length);
}

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
  if (document.hidden) return; // the 10 s refresh: not while away (it would fail the moment the laptop wakes)
  const key = D.key;
  const r = await api(`/api/pane/files?${fileQS()}`).catch(() => null);
  if (key !== D.key || !r) return; // no network: keep the list, the next tick tries again
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
  const r = await api(`/api/pane/file?${fileQS()}&path=${encodeURIComponent(path)}`).catch(() => ({ status: 0, error: "The panel can't be reached: try again in a moment." }));
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
// An HTML preview runs sandboxed (no storage, no network). It keeps review comments by asking us:
// {type:'cp-comments', op:'load'|'save'} → the panel stores them in "<page>.comments.json" beside the page.
// {op:'send', text} hands them to the pane's agent as a message, once you confirm in a bar over the page: a page
// can ask whenever it likes, and only you decide what reaches the chat.
addEventListener('message', async (e) => {
  const fr = $('#dr-preview .pv-frame');
  const m = e.data;
  if (!fr || e.source !== fr.contentWindow || !m || m.type !== 'cp-comments' || !F.path) return;
  if (m.op === 'send') return askSend(fr, String(m.text || '').trim().slice(0, 20000));
  const url = `/api/pane/file/comments?${fileQS()}&path=${encodeURIComponent(F.path)}`;
  const net = () => ({ status: 0, error: "The panel can't be reached" });
  const r = m.op === 'save' ? await api(url, { comments: m.comments || [] }).catch(net) : m.op === 'load' ? await api(url).catch(net) : null;
  if (!r) return;
  fr.contentWindow.postMessage({ type: 'cp-comments', op: m.op === 'save' ? 'saved' : 'loaded', ok: r.status === 200,
    comments: r.comments, file: r.file, error: r.error }, '*');
});
function askSend(fr, text) {
  const reply = (ok, error = '') => fr.contentWindow?.postMessage({ type: 'cp-comments', op: 'sent', ok, error }, '*');
  $('#dr-preview .pv-ask')?.remove();
  if (!text || !D.key) return reply(false, text ? 'no chat to send to' : 'nothing to send');
  const name = F.path.split('/').pop(), path = F.path;
  const bar = document.createElement('div');
  bar.className = 'pv-ask';
  bar.setAttribute('role', 'alertdialog');
  bar.innerHTML = `${ic('chat-round-dots')}<div class="pv-ask-t"><b>Send ${esc(name)}'s comments to the chat?</b><span>${esc(text.replace(/\s+/g, ' ').slice(0, 220))}${text.length > 220 ? '…' : ''}</span></div>` +
    '<button type="button" class="btn sm" data-ask="no">Cancel</button><button type="button" class="btn sm primary" data-ask="yes">Send</button>';
  bar.addEventListener('click', (ev) => {
    const b = ev.target.closest('[data-ask]');
    if (!b) return;
    bar.remove();
    if (b.dataset.ask !== 'yes') return reply(false);
    queueSend(D.key, D.session, D.pane, `${text}\n\n(Comments file: ${path}.comments.json)`, []);
    reply(true);
  });
  $('#dr-preview .pv-h').after(bar);
  $('[data-ask="yes"]', bar).focus();
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
  const pk = e.target.closest('#dr-prompt [data-pick]');
  if (pk) { if (!e.target.matches('[data-aptext]')) apPick(pk); return; }
  if (e.target.closest('#dr-prompt [data-choose]')) return apSend();
  if (e.target.closest('#dr-prompt [data-skip]')) {
    if (D.ans) { D.ans.busy = true; apSync(); }
    await sendInput({ keys: ['Esc'] });
    if (D.ans) { D.ans.busy = false; apSync(); }
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
    D.view = D.raw = null;
    renderDrawer();
    if (D.mode === 'screen') { $('#term-pre').textContent = ''; $('#term-sb').textContent = ''; Object.assign(D, { sb: [], sbDirty: false, termFresh: true }); if (!coarse) termFocus(); }
    else { $('#dr-body').innerHTML = `<div class="chat"><div class="chat-empty">${shim('Loading the conversation…')}</div></div>`; D.seen = 0; }
    return connectPane();
  }
  if (e.target.closest('#dr-close')) return closePane();
  if (e.target.closest('[data-saclose]')) return closeSub();
  const ul = e.target.closest('[data-ulong]'); // a long message you sent: unfold or fold it
  if (ul) { const id = ul.dataset.ulong; if (!D.openTools.delete(id)) D.openTools.add(id); return ul.closest('#dr-sa') ? renderSub() : renderBody(true); }
  const so = e.target.closest('[data-sub]');
  if (so) return openSub(so.dataset.sub);
  const st = e.target.closest('[data-subtool]');
  if (st) { e.preventDefault(); return openSub('', st.dataset.subtool); } // not the row's open/close
  const rl = e.target.closest('.rl-hide, #dr-railbtn');
  if (rl) return setRail(rl.id !== 'dr-railbtn');
  if (e.target.closest('#dr-model')) return toggleModels();
  const mo = e.target.closest('[data-setmodel]');
  if (mo) return setModel(mo.dataset.setmodel);
  if (e.target.closest('#dr-filesbtn')) return F.open ? closeFilesPanel() : openFilesPanel();
  if (e.target.closest('[data-files]')) return openFilesPanel();
  if (e.target.closest('[data-pclose]')) return closePreview();
  const of = e.target.closest('[data-openfile]');
  if (of) return openFile(of.dataset.openfile);
  if (e.target.closest('[data-fclose]')) return closeFilesPanel();
  const ff = e.target.closest('[data-ffilter]');
  if (ff) { F.filter = ff.dataset.ffilter; return renderFilesPanel(); }
  if (e.target.closest('[data-fcopy]')) return copy(F.path, e.target.closest('[data-fcopy]'));
  const its = e.target.closest('#dr-sa') ? SA.items : D.items; // the main chat's, or the subagent's over it
  const ci = e.target.closest('[data-copyi]');
  if (ci) return copy(its[+ci.dataset.copyi]?.text || '', ci);
  const qi = e.target.closest('[data-quotei]');
  if (qi) { closeSub(); return quoteIntoComposer(its[+qi.dataset.quotei]?.text || ''); }
  const cid = e.target.closest('[data-copyid]');
  if (cid) { const r = its.find((i) => i.k === 'result' && i.id === cid.dataset.copyid); return copy(r?.text || '', cid); }
  const rm = e.target.closest('[data-rmfile]');
  if (rm) { const [f] = D.files.splice(+rm.dataset.rmfile, 1); if (f) dropURL(f); keepFiles(D.key); return renderFiles(); }
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
  else if ($('#dr-pr .prc-card')) foldPR(true);
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
    item('plus', 'New tab', { mi: 'new', new: 'tab', session });
    item('row-horizontal', 'New pane in this tab', { mi: 'new', new: 'pane', session, tab, tabname: tb?.name || '' });
    item('close-square', 'Close tab…', { mi: 'close', what: 'tab', session, tab }, true);
  } else {
    const f = findPane(session, pane);
    if (!f) return;
    if (D.key !== `${session}:${pane}`) item('arrow-up-right', 'Open', { mi: 'open', key: `${session}:${pane}` });
    item('plus', 'New tab', { mi: 'new', new: 'tab', session });
    item('row-horizontal', 'New pane in this tab', { mi: 'new', new: 'pane', session, tab: f.tb.id, tabname: f.tb.name });
    if (f.p.fg) item('power', `Kill ${shortCmd(f.p.proc)}…`, { mi: 'close', what: 'process', session, pane, pid: f.p.fg }, true);
    item('xmark', 'Close pane…', { mi: 'close', what: 'pane', session, pane }, true);
    if (f.tb.panes.length > 1) item('close-square', 'Close tab…', { mi: 'close', what: 'tab', session, tab: f.tb.id }, true);
    // closing the whole session lives on the session card only, never in a pane's menu
  }
  showMenu(btn, items.join(''));
}
// #menu, right-aligned under btn (above it when there's no room below)
function showMenu(btn, html) {
  const m = $('#menu');
  m.hidden = true;
  void m.offsetWidth; // a menu opened from another one still pops in
  m.innerHTML = html;
  m.hidden = false;
  const r = btn.getBoundingClientRect(), w = m.offsetWidth, h = m.offsetHeight;
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
  if (d.mi === 'compact') return compactNow(d.session, +d.pane);
  if (d.mi === 'cpop') return openCpop(document.querySelector(`[data-menu="tpane"][data-session="${CSS.escape(d.session)}"][data-pane="${+d.pane}"]`), d.session, +d.pane);
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

// ---------- dropdowns: the panel's own list in place of the browser's ----------
// The <select> stays, hidden, as the value: code reads and sets .value, fills its options and listens for 'change'
// as before. A button shows the chosen option and opens a list (option groups as headings); arrows, Home/End,
// Enter, Esc and type-ahead (ddMatch) work as in a native one. The button comes first, so the field's label opens it.
function customSelect(sel) {
  const box = document.createElement('div');
  box.className = 'dd';
  box.innerHTML = `<button type="button" class="dd-b" aria-haspopup="listbox" aria-expanded="false"><span class="dd-v"></span>${ic('angle-down', 'dd-c')}</button><div class="dd-l" role="listbox" hidden></div>`;
  sel.before(box);
  sel.hidden = true;
  const btn = $('.dd-b', box), list = $('.dd-l', box);
  const lid = 'dd-' + (sel.id || Math.random().toString(36).slice(2));
  list.id = lid;
  btn.setAttribute('aria-controls', lid);
  const sync = () => { const o = sel.selectedOptions[0]; $('.dd-v', box).textContent = o ? o.textContent : ''; btn.title = o ? o.textContent : ''; };
  const native = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'value');
  Object.defineProperty(sel, 'value', { get() { return native.get.call(this); }, set(v) { native.set.call(this, v); sync(); } });
  new MutationObserver(sync).observe(sel, { childList: true, subtree: true });
  let act = -1, typed = '', typedT = 0;
  const optHTML = (o) => `<div class="dd-o" role="option" id="${lid}-${o.index}" data-i="${o.index}" aria-selected="${o.selected}">${ic('check', 'dd-k')}<span>${esc(o.textContent)}</span></div>`;
  const items = () => $$('.dd-o', list);
  const move = (i) => {
    const xs = items();
    if (!xs.length) return;
    act = Math.max(0, Math.min(xs.length - 1, i));
    xs.forEach((x, k) => x.classList.toggle('act', k === act));
    xs[act].scrollIntoView({ block: 'nearest' });
    btn.setAttribute('aria-activedescendant', xs[act].id);
  };
  const close = () => {
    if (list.hidden) return;
    list.hidden = true;
    btn.setAttribute('aria-expanded', 'false');
    btn.removeAttribute('aria-activedescendant');
  };
  const open = () => {
    list.innerHTML = [...sel.children].map((c) => (c.tagName === 'OPTGROUP'
      ? `<div class="dd-g" role="presentation">${esc(c.label)}</div>` + [...c.children].map(optHTML).join('')
      : optHTML(c))).join('');
    // below the button, or above it when the screen has more room there
    const r = btn.getBoundingClientRect(), below = innerHeight - r.bottom - 12, above = r.top - 12;
    const up = below < 220 && above > below;
    box.classList.toggle('up', up);
    list.style.maxHeight = Math.min(300, up ? above : below) + 'px';
    list.hidden = false;
    btn.setAttribute('aria-expanded', 'true');
    move(items().findIndex((x) => +x.dataset.i === sel.selectedIndex));
  };
  const pick = (i) => {
    if (sel.selectedIndex !== i) {
      sel.selectedIndex = i;
      sync();
      sel.dispatchEvent(new Event('change', { bubbles: true }));
    }
    close();
    btn.focus();
  };
  btn.addEventListener('click', () => (list.hidden ? open() : close()));
  list.addEventListener('mousedown', (e) => e.preventDefault()); // the button keeps the focus (and the keys)
  list.addEventListener('click', (e) => {
    e.preventDefault(); // inside a <label>: a click must not reach the button again through it
    const o = e.target.closest('.dd-o');
    if (o) pick(+o.dataset.i);
  });
  list.addEventListener('mousemove', (e) => { const o = e.target.closest('.dd-o'); if (o) move(items().indexOf(o)); });
  btn.addEventListener('keydown', (e) => {
    const shut = list.hidden, k = e.key;
    if (k === 'Escape' && !shut) { e.preventDefault(); e.stopPropagation(); close(); return; } // closes the list, not its dialog
    if (k === 'Tab') return close();
    if (['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(k)) {
      e.preventDefault();
      if (shut) return open();
      move(k === 'Home' ? 0 : k === 'End' ? items().length - 1 : act + (k === 'ArrowDown' ? 1 : -1));
    } else if ((k === 'Enter' || k === ' ') && !shut && act >= 0) {
      e.preventDefault();
      pick(+items()[act].dataset.i);
    } else if (k.length === 1 && !e.metaKey && !e.ctrlKey && !e.altKey && k !== ' ') {
      if (shut) open();
      typed = performance.now() - typedT < 700 ? typed + k : k;
      typedT = performance.now();
      const i = ddMatch(items().map((x) => x.textContent), typed, act);
      if (i >= 0) move(i);
    }
  });
  box.addEventListener('focusout', (e) => { if (!box.contains(e.relatedTarget)) close(); });
  document.addEventListener('pointerdown', (e) => { if (!box.contains(e.target)) close(); });
  sync();
}
for (const s of $$('select')) customSelect(s);

// ---------- create session / tab / pane ----------
let NEW = null;
function openNew(d) {
  NEW = { what: d.new, session: d.session || '', tab: +(d.tab || 0) };
  $('#new-title').textContent = d.new === 'session' ? 'New zellij session' : d.new === 'tab' ? `New tab in ${d.session}` : `New pane in ${d.tabname}`;
  $('#new-name').value = '';
  $('#new-name').required = d.new === 'session';
  $('#new-name').placeholder = d.new === 'session' ? 'e.g. scratch' : 'optional';
  $('#new-slug').hidden = true;
  fillDirs(d);
  $('#new-run').value = store.get('newRun', 'shell');
  $('#new-dir-f').hidden = d.new !== 'pane';
  $('#new-err').textContent = '';
  $('#newdlg').showModal();
  $('#new-name').focus();
}
// where to start it: a list, not a text box: the folders terminals use now (a worktree as its repo), repos, projects
let DIRS = null;
const paneDir = (p) => (p ? (p.git?.root ? tilde(p.git.root) : p.agent?.cwd || '') : '');
async function fillDirs(d) {
  const sel = $('#new-cwd-sel');
  const tab = d.new === 'pane' ? findTab(d.session, +d.tab).tb : null;
  const sess = (S.data.term?.sessions || []).find((s) => s.name === d.session);
  const want = mainDir(paneDir(tab?.panes[0]) || paneDir(sess?.tabs?.[0]?.panes?.[0]) || store.get('newCwd', '~'));
  const inUse = [...new Set((S.data.term?.sessions || []).flatMap((s) => (s.tabs || []).flatMap((t) => t.panes.map((p) => mainDir(paneDir(p))))).filter(Boolean))];
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

// a session's name can't hold spaces and the like: say what it will be called instead of refusing it
$('#new-name').addEventListener('input', (e) => {
  const v = e.target.value.trim(), slug = sessionSlug(v);
  const note = $('#new-slug');
  note.hidden = NEW?.what !== 'session' || !v || slug === v;
  note.textContent = slug ? `Will be named ${slug}` : 'Needs a letter or digit';
});
$('#new-cancel').addEventListener('click', () => $('#newdlg').close());
$('#update').addEventListener('click', reloadNow);
$('#new-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  if (!NEW) return;
  const ok = $('#new-ok');
  ok.disabled = true;
  const sel = $('#new-cwd-sel').value;
  const typed = $('#new-name').value.trim();
  const body = { ...NEW, name: NEW.what === 'session' ? sessionSlug(typed) : typed, cwd: sel === '__other' ? $('#new-cwd').value.trim() : sel, run: $('#new-run').value, dir: NEW.what === 'pane' ? $('#new-dir').value : '' };
  if (NEW.what === 'session' && !body.name) { $('#new-err').textContent = 'A session name needs a letter or digit.'; ok.disabled = false; return; }
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

const VIEWS = { res: viewRes, ci: viewCI, term: (d) => viewTerm(d), sec: viewSec }; // viewTerm: terms.js, loaded after this file

// ---------- interactions ----------
$('#view').addEventListener('click', (e) => {
  if (limitClick(e)) return;
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

// ---------- notifications: the bell, top right (in the page header and a chat's) ----------
// The server keeps the list (notes.go); the badge carries the newest id and the unread count. Notes that
// arrive while the page is open also pop up as a toast you can click.
const N = { latest: 0, list: [], seen: 0, shownSeen: 0, open: false };
const BELL_IC = { done: 'check-circle-f', asking: 'help-circle', stalled: 'alert-triangle', ci: 'x-circle-f', sec: 'shield-check', disk: 'hard-drive', mem: 'ram' };
async function loadNotes() {
  const r = await api('/api/notes').catch(() => ({}));
  if (r.status !== 200) return [];
  const fresh = r.notes.filter((x) => N.latest && x.id > N.latest);
  N.list = r.notes;
  N.seen = r.seen;
  N.latest = r.notes[0]?.id || 0;
  if (N.open) renderNotes();
  return fresh;
}
function noteBadge(b) {
  const n = b.unread || 0;
  for (const el of $$('[data-bell]')) {
    const c = el.querySelector('.bell-n');
    setHidden(c, !n);
    setText(c, n > 99 ? '99+' : String(n));
    setAttr(el, 'aria-label', n ? `Notifications, ${n} unread` : 'Notifications');
  }
  if ((b.note || 0) <= N.latest) return;
  const first = !N.latest;
  if (first) N.latest = b.note; // the page's first look: no toasts for what came before
  loadNotes().then((fresh) => {
    const mine = fresh.filter((x) => !(x.go === 'pane:' + D.key && !document.hidden)); // you're looking at it
    const shown = mine.length > 3 ? mine.slice(0, 2) : mine;
    if (mine.length > 3) noteToast({ title: `${mine.length - 2} more notifications`, level: 'ok', go: 'bell' });
    for (const x of [...shown].reverse()) noteToast(x); // the newest ends up on top
  });
}
// a pop-up in the top-right corner: click it to go there; it fades after 6 s (hovering holds it)
// pop-ups: a note as it arrives, and the panel's own messages (toast). Top right, newest on top, three at most.
// Each has an × and goes by itself after 5 s; the bar along its bottom shows the time left, and a pointer or the
// keyboard on it holds both.
const POPUP_MS = 5000;
function popup(body, go, bad) {
  const box = $('#ntoasts');
  const el = document.createElement('div');
  el.className = 'ntoast' + (bad ? ' bad' : '');
  el.innerHTML = (go ? `<button type="button" class="nt-go" data-ngo="${esc(go)}">${body}</button>` : `<div class="nt-go">${body}</div>`) +
    `<button type="button" class="nt-x" aria-label="Dismiss">${ic('xmark')}</button><i class="nt-timer" aria-hidden="true"></i>`;
  let left = POPUP_MS, since = Date.now(), held = false;
  const close = () => { if (!el.classList.contains('out')) { el.classList.add('out'); setTimeout(() => el.remove(), 220); } };
  let timer = setTimeout(close, left);
  const hold = () => { if (held) return; held = true; clearTimeout(timer); left -= Date.now() - since; el.classList.add('held'); };
  const run = () => { if (!held) return; held = false; since = Date.now(); timer = setTimeout(close, left); el.classList.remove('held'); };
  el.addEventListener('pointerenter', hold);
  el.addEventListener('pointerleave', run);
  el.addEventListener('focusin', hold);
  el.addEventListener('focusout', (e) => { if (!el.contains(e.relatedTarget)) run(); });
  $('.nt-x', el).addEventListener('click', close);
  box.prepend(el);
  while (box.children.length > 3) box.lastChild.remove();
}
function noteToast(x) {
  popup(ic(BELL_IC[x.kind] || 'bell', 'lv-' + x.level) + `<span class="nt-b"><span class="nt-t">${esc(x.title)}</span>${x.body ? `<span class="nt-d">${esc(x.body)}</span>` : ''}</span>`, x.go);
}
function renderNotes() {
  const box = $('#notes');
  const unread = N.list.filter((x) => x.id > N.shownSeen).length;
  box.innerHTML = `<div class="nt-h"><b>Notifications</b>${unread ? `<span class="nt-c">${unread} new</span>` : ''}<span class="spacer"></span>` +
    `<button type="button" class="icon-btn sm ghost" data-ntclose aria-label="Close">${ic('xmark')}</button></div>` +
    (N.list.length ? `<div class="nt-list">${N.list.map((x) => `<button type="button" class="nt${x.id > N.shownSeen ? ' new' : ''}" data-ngo="${esc(x.go)}" data-nid="${x.id}">` +
      `${ic(BELL_IC[x.kind] || 'bell', 'lv-' + x.level)}<span class="nt-b"><span class="nt-t">${esc(x.title)}</span>${x.body ? `<span class="nt-d">${esc(x.body)}</span>` : ''}</span>` +
      `<span class="nt-time">${esc(noteAge(x.t))}</span></button>`).join('')}</div>`
      : `<div class="nt-empty">${ic('bell')}<span>Nothing yet. Agents finishing or waiting on you, failed CI jobs and alerts land here.</span></div>`);
}
const noteAge = (t) => { const s = now() - t; return s < 60 ? 'now' : s < 3600 ? Math.floor(s / 60) + 'm' : s < 86400 ? Math.floor(s / 3600) + 'h' : Math.floor(s / 86400) + 'd'; };
function toggleNotes(force) {
  N.open = force ?? !N.open;
  $('#notes').hidden = !N.open;
  for (const el of $$('[data-bell]')) el.setAttribute('aria-expanded', String(N.open));
  if (!N.open) return;
  N.shownSeen = N.seen; // keep this opening's "new" marks; the badge clears now
  renderNotes();
  loadNotes().then(() => {
    N.shownSeen = Math.min(N.shownSeen, N.seen);
    renderNotes();
    if (N.latest > N.seen) { api('/api/notes/seen', { id: N.latest }).catch(() => {}); N.seen = N.latest; for (const c of $$('.bell-n')) c.hidden = true; }
  });
}
// where a note takes you: its agent's chat, or the view
function noteGo(g) {
  toggleNotes(false);
  if (g === 'bell') return toggleNotes(true);
  if (g.startsWith('pane:')) {
    const key = g.slice(5);
    if (S.view !== 'term') { S.openWhenSeen = key; return setView('term'); }
    if (findPane(...splitKey(key))) return openPane(key);
    return toast('That pane is gone', true);
  }
  if (TITLES[g]) { if (S.view !== g) setView(g); else closePane(); }
}
addEventListener('click', (e) => {
  if (e.target.closest('[data-bell]')) return toggleNotes();
  if (e.target.closest('[data-ntclose]')) return toggleNotes(false);
  const g = e.target.closest('[data-ngo]');
  if (g) { e.stopPropagation(); g.closest('.ntoast')?.remove(); return noteGo(g.dataset.ngo); }
  if (N.open && !e.target.closest('#notes')) toggleNotes(false);
}, true);
addEventListener('keydown', (e) => { if (e.key === 'Escape' && N.open) { e.stopImmediatePropagation(); toggleNotes(false); } }, true);

function toast(msg, bad) {
  popup(ic(bad ? 'alert-triangle' : 'check-circle-f', bad ? 'lv-bad' : 'lv-ok') + `<span class="nt-b"><span class="nt-t">${esc(msg)}</span></span>`, '', bad);
}

// ---------- keyboard shortcuts ----------
// Plain keys, never with Cmd, Ctrl or Alt (those belong to the browser, Arc's included, and to macOS and Windows),
// and never while you type (a text field, the chat box, a terminal): "g" and a letter goes to a page, as on
// GitHub or Gmail; "/" jumps anywhere by name. "?" lists them, and turns them off (screen readers have
// single-key commands of their own).
const GO_KEYS = { a: 'ai', r: 'res', c: 'ci', t: 'term', f: 'files', b: 'browser', s: 'sec' };
const KS = { on: store.get('keys', '1') !== '0', g: 0, list: null, at: 0, items: [], sel: 0 };
for (const [k, v] of Object.entries(GO_KEYS)) { const b = $(`.nav-btn[data-view="${v}"]`); if (b) b.title = `${TITLES[v]} (g then ${k})`; }
const kbd = (...ks) => ks.map((k) => `<kbd>${esc(k)}</kbd>`).join('');
// every terminal, in the Terminals page's order: the live data when it's that page, else the server's latest list
// (fetched again when older than maxAge ms)
async function allPanes(maxAge = 5000) {
  let tm = S.data.term;
  if (S.view !== 'term' && Date.now() - KS.at > maxAge) {
    const r = await api('/api/term/list').catch(() => null);
    if (r?.status === 200) { KS.list = r; KS.at = Date.now(); }
  }
  if (S.view !== 'term' && KS.list) tm = KS.list;
  const out = [];
  for (const s of tm?.sessions || []) for (const tb of s.tabs || []) for (const p of tb.panes) {
    out.push({ key: `${s.name}:${p.id}`, label: tb.panes.length > 1 ? `${tb.name} · ${p.title || 'pane ' + p.id}` : tb.name, session: s.name, p });
  }
  return out;
}
async function stepPane(d) { // j / k: the next or previous terminal
  const xs = await allPanes();
  if (!xs.length) return;
  const i = xs.findIndex((x) => x.key === D.key);
  const to = xs[i < 0 ? (d > 0 ? 0 : xs.length - 1) : (i + d + xs.length) % xs.length];
  if (findPane(...splitKey(to.key))) openPane(to.key);
  else { setView('term'); S.openWhenSeen = to.key; } // not in this page's data yet: it opens once the list has it
}
function keyHint(on) {
  let h = $('#keyhint');
  if (!h) { h = document.createElement('div'); h.id = 'keyhint'; h.className = 'keyhint'; h.setAttribute('role', 'status'); document.body.append(h); }
  h.innerHTML = on ? `Go to ${Object.entries(GO_KEYS).map(([k, v]) => `${kbd(k)} ${esc(TITLES[v])}`).join(' ')}` : '';
  h.hidden = !on;
}
function renderKeys() {
  const row = (keys, what) => `<div class="ks-r"><span>${what}</span><span class="ks-k">${keys}</span></div>`;
  const then = (a, b) => `${kbd(a)}<i>then</i>${kbd(b)}`;
  $('#keys-body').innerHTML =
    `<div class="ks-g"><h3>Go to</h3>${Object.entries(GO_KEYS).map(([k, v]) => row(then('g', k), esc(TITLES[v]))).join('')}</div>` +
    `<div class="ks-g"><h3>Anywhere</h3>${row(kbd('/'), 'Jump to a terminal or page by name')}${row(kbd('j') + kbd('k'), 'Next / previous terminal')}${row(kbd('n'), 'Notifications')}${row(kbd('?'), 'This list')}</div>` +
    `<div class="ks-g"><h3>In Files</h3>${row(kbd('/'), 'Search every file (⌘K from any page)')}${row(kbd('↑') + kbd('↓') + kbd('←') + kbd('→'), 'Move, open and close folders in the tree')}${row(kbd('↵'), 'Open (a folder as a view)')}${row(kbd('f'), 'Focus: hide the tree')}${row(kbd('⌫'), 'Up a folder')}${row(kbd('F2'), 'Rename')}${row(kbd('Del'), 'Delete')}${row(kbd('⌘') + kbd('S'), 'Save')}</div>` +
    `<div class="ks-g"><h3>In a terminal</h3>${row(kbd('c'), 'Write in the chat box')}${row(kbd('t'), 'Chat ↔ terminal')}${row(kbd('f'), 'Files')}${row(kbd('Esc'), 'Back to the list')}</div>` +
    `<div class="ks-on"><span><b>Single-key shortcuts</b><small>Plain keys only, so nothing clashes with the browser or the system. They never fire while you type.</small></span>` +
    `<button type="button" class="sw" role="switch" aria-checked="${KS.on}" aria-label="Single-key shortcuts" data-keyson></button></div>`;
}
function showKeys() { renderKeys(); $('#keys').showModal(); }
$('#keys').addEventListener('click', (e) => {
  if (e.target === $('#keys') || e.target.closest('[data-keysclose]')) return $('#keys').close(); // outside the card, or ×
  const sw = e.target.closest('[data-keyson]');
  if (sw) { KS.on = !KS.on; store.set('keys', KS.on ? '1' : '0'); sw.setAttribute('aria-checked', String(KS.on)); }
});
$('#nav-keys').addEventListener('click', showKeys);

// "/": a list of every page and terminal, narrowed as you type
async function openJump() {
  const q = $('#jump-q');
  q.value = '';
  KS.sel = 0;
  $('#jump').showModal();
  q.focus();
  const views = Object.keys(TITLES).map((v) => ({ view: v, label: TITLES[v], sub: 'Page' }));
  KS.items = views;
  renderJump();
  KS.items = views.concat((await allPanes()).map((x) => {
    const [, st] = TSTATE[x.p.state] || ['', x.p.state || ''];
    return { key: x.key, label: x.label, sub: [x.session, st, x.p.git?.branch].filter(Boolean).join(' · '), cls: (TSTATE[x.p.state] || [''])[0] };
  }));
  renderJump();
}
function jumpHits() {
  const words = $('#jump-q').value.toLowerCase().split(/\s+/).filter(Boolean);
  return KS.items.filter((x) => { const hay = (x.label + ' ' + x.sub).toLowerCase(); return words.every((w) => hay.includes(w)); });
}
function renderJump() {
  const hits = jumpHits();
  KS.sel = Math.min(KS.sel, Math.max(0, hits.length - 1));
  $('#jump-list').innerHTML = hits.map((x, i) => `<div class="jump-i${i === KS.sel ? ' on' : ''}" role="option" id="jump-${i}" aria-selected="${i === KS.sel}" data-ji="${i}">` +
    `${x.view ? ic('arrow-up-right') : `<i class="dot ${x.cls || ''}"></i>`}<span class="jump-l">${esc(x.label)}</span><span class="jump-s">${esc(x.sub)}</span></div>`).join('') ||
    '<div class="jump-none">Nothing by that name.</div>';
  $('#jump-q').setAttribute('aria-activedescendant', hits.length ? 'jump-' + KS.sel : '');
  $('#jump-list .on')?.scrollIntoView({ block: 'nearest' });
}
function jumpGo(x) {
  if (!x) return;
  $('#jump').close();
  if (x.view) return setView(x.view);
  if (findPane(...splitKey(x.key))) openPane(x.key);
  else { setView('term'); S.openWhenSeen = x.key; }
}
$('#jump-q').addEventListener('input', () => { KS.sel = 0; renderJump(); });
$('#jump-q').addEventListener('keydown', (e) => {
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { e.preventDefault(); KS.sel = Math.max(0, KS.sel + (e.key === 'ArrowDown' ? 1 : -1)); renderJump(); }
  else if (e.key === 'Enter') { e.preventDefault(); jumpGo(jumpHits()[KS.sel]); }
});
$('#jump').addEventListener('click', (e) => {
  if (e.target === $('#jump')) return $('#jump').close();
  const i = e.target.closest('[data-ji]');
  if (i) jumpGo(jumpHits()[+i.dataset.ji]);
});

addEventListener('keydown', (e) => {
  if (!KS.on || e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey || e.isComposing || typingIn(e.target) || $('dialog[open]') || !$('#app') || $('#app').hidden) return;
  const k = e.key;
  if (KS.g && Date.now() - KS.g < 1500) { // "g" waits for its letter
    KS.g = 0;
    keyHint(false);
    const v = GO_KEYS[k.toLowerCase()];
    if (v) { e.preventDefault(); setView(v); }
    return;
  }
  const chat = D.key && D.mode === 'chat';
  const act = {
    g: () => { KS.g = Date.now(); keyHint(true); setTimeout(() => { if (KS.g && Date.now() - KS.g >= 1500) { KS.g = 0; keyHint(false); } }, 1600); },
    '/': openJump,
    '?': showKeys,
    n: () => toggleNotes(),
    j: () => stepPane(1),
    k: () => stepPane(-1),
    c: chat && (() => $('#dr-text').focus()),
    t: D.key && (() => $(`#drawer [data-mode="${D.mode === 'chat' ? 'screen' : 'chat'}"]`)?.click()),
    f: chat && !$('#dr-filesbtn').hidden && (() => $('#dr-filesbtn').click()),
  }[k];
  if (!act || e.repeat) return;
  e.preventDefault(); // "/" would open Firefox's quick find; "c" would type into the chat box it just focused
  act();
});

// ---------- boot ----------
// report.js sent /api/me. The answer can land between two deferred scripts, and the app needs all of them
// (terms.js, files.js, assistant.js), so it starts once they have run: at DOMContentLoaded, which can't have
// fired yet while this deferred script runs.
(async () => {
  const scripts = new Promise((ok) => addEventListener('DOMContentLoaded', ok, { once: true }));
  try {
    const r = await api('/api/me', undefined, window.cpMe);
    await scripts;
    if (r.status === 200) startApp();
    else showLogin(r.status === 401 ? '' : r.error || '');
  } catch { showLogin('Network error — retrying…'); setTimeout(() => location.reload(), 5000); }
})();
