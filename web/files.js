'use strict';

// ---------- Files: a file manager over ~ and /tmp (the API is fs.go) ----------
// A tree on the left (click a folder to expand it, double-click to open it as a view), tabs on the right in one
// or two groups, a viewer per type (the libraries load the first time a type needs one), an editor for text,
// and a search over every name on the box (/ or ⌘K). The view is built once and owns its DOM (data-own):
// the 2-second stream never redraws it.

const FM_PLACES = [['Home', '~'], ['Repos', '~/repos'], ['Projects', '~/projects'], ['Uploads', '~/.local/state/control-panel/uploads'], ['/tmp', '/tmp']];
// Sizes are in rem (files.css, as app.css): the root is 16px on a phone and smaller on a desktop (the panel at 85%).
// What the page computes in px (virtual-list rows, the sheet's columns) is scaled by the same factor.
let fmRemV = 0;
addEventListener('resize', () => { fmRemV = 0; });
const fmRem = () => (fmRemV ||= parseFloat(getComputedStyle(document.documentElement).fontSize) / 16 || 1);
const fmRH = () => (FM.narrow ? 44 : 30) * fmRem(); // tree and list rows (--fm-rh): the lists are virtual; a phone's rows are a thumb tall
const FM_TEXT_MAX = 5 << 20, FM_TEXT_QUICK = 256 << 10; // text opens editable up to 5 MB; past that, read-only (a log shows its last 5 MB)
const FM_EXT = {
  img: 'png jpg jpeg gif webp svg bmp ico avif heic',
  video: 'mp4 webm mov m4v ogv',
  audio: 'mp3 wav m4a ogg oga flac aac opus',
  pdf: 'pdf', sheet: 'xlsx xls xlsm ods numbers', csv: 'csv tsv', docx: 'docx', md: 'md markdown', html: 'html htm',
  office: 'pptx ppt doc',
  bin: 'zip gz tgz tar bz2 xz 7z rar key pages exe dmg iso bin so dylib o a class jar war woff woff2 ttf otf eot pyc sqlite db parquet',
};
const FM_KIND = Object.fromEntries(Object.entries(FM_EXT).flatMap(([k, v]) => v.split(' ').map((x) => [x, k])));

const FM = {
  st: (() => { try { return JSON.parse(store.get('fm', '{}')) || {}; } catch { return {}; } })(), // what's remembered
  el: null, dirs: new Map(), loading: new Map(), rows: [], sel: '', multi: new Set(),
  groups: [], g: 0, hist: [], hi: -1, home: '', recent: [], repos: null, pal: null, seq: 0,
};
FM.root = FM.st.root || '~';
FM.open = new Set(FM.st.open || []);
FM.hidden = !FM.st.nodots; // dotfiles and folders (.claude, .git…) listed unless hidden from the view menu
FM.recent = FM.st.recent || [];
const fmSave = () => store.set('fm', JSON.stringify({
  root: FM.root, open: [...FM.open].slice(-300), nodots: !FM.hidden, focus: FM.focus, w: FM.w, split: FM.split, grid: FM.grid, recent: FM.recent.slice(0, 30),
  tabs: FM.groups.map((g) => ({ on: g.on, tabs: g.tabs.map((t) => ({ p: t.path, d: t.dir, pre: t.pre })) })),
}));

// ---- paths and kinds ----
const fmBase = (p) => p.slice(p.lastIndexOf('/') + 1) || p;
const fmDir = (p) => p.slice(0, p.lastIndexOf('/')) || '/';
const fmJoin = (d, n) => (d === '/' ? '/' : d + '/') + n;
const fmTilde = (p) => (FM.home && (p === FM.home || p.startsWith(FM.home + '/')) ? '~' + p.slice(FM.home.length) : p);
const fmAbs = (p) => (p === '~' ? FM.home : p.startsWith('~/') ? FM.home + p.slice(1) : p);
const fmRaw = (p, dl) => '/api/fs/raw?path=' + encodeURIComponent(p) + (dl ? '&dl=1' : '');
// a photo scaled on the panel to a w×h box (CSS px × the screen's density), when it's big enough to be worth it, and
// always for HEIC (iPhone photos: browsers can't show the file itself); its mtime in the URL, so the browser keeps it
// until the file changes
const fmHeic = (p) => /\.hei[cf]$/i.test(p);
const fmPx = (n) => Math.round(n * devicePixelRatio);
const fmFit = () => [Math.min(innerWidth, 2560), Math.min(innerHeight, 2560)]; // the box a picture opens into (fmImage)
const fmPic = (e, w, h, cover) => (/\.(jpe?g|png|gif|webp|avif|hei[cf])$/i.test(e.path) && (e.size > 2e5 || fmHeic(e.path))
  ? `/api/fs/thumb?path=${encodeURIComponent(e.path)}&w=${fmPx(w)}&h=${fmPx(h)}&v=${e.mtime}${cover ? '&fit=cover' : ''}` +
    (cover && /\.(hei[cf]|avif)$/i.test(e.path) ? `&pre=${fmFit().map(fmPx).join('x')}` : '') // a tile: its opened size made in the same (slow) decode
  : fmRaw(e.path));
const fmKind = (name) => FM_KIND[extOf(name)] || 'text'; // text is a guess: the bytes decide (fmText)
// listed with hidden files off: all but dotfiles, and a project's .env files, there to edit (a .env folder is a virtualenv)
const fmShown = (e) => FM.hidden || e.name[0] !== '.' || (!e.dir && /^\.env(\.|$)/.test(e.name));
const fmIcon = (e) => (e.dir ? ic(FM.open.has(e.path) ? 'folder-open' : 'folder', 'fm-fold') : fxBadge(e.name));
const fmWhen = (ms) => (ms ? ago(ms / 1000) : '');
const fmLib = (n) => richLib(n); // ai-rich.js: a script from a <meta name="lib-…"> tag, loaded once
const fmMeta = (n) => new URL($(`meta[name="lib-${n}"]`).content, location.href).href;
let fmPdf = null;
let fmPdfWorker = null; // one pdf.js worker for every PDF: it starts once; a worker we hand in is never destroyed with a document
const fmPdfLib = () => (fmPdf ||= import(fmMeta('pdf')).then((m) => {
  fmPdfWorker = new m.PDFWorker({ port: new Worker(fmMeta('pdfworker'), { type: 'module' }) });
  return m;
}));

async function fmJSON(url, body, method) {
  const r = await fetch(url, body === undefined && !method ? { credentials: 'same-origin' }
    : { method: method || 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  let j = {};
  try { j = await r.json(); } catch { /* not JSON */ }
  if (r.status === 401 && j.error === 'login required') showLogin('Session ended — enter your PIN.');
  return { status: r.status, ...j };
}
const fmErr = (r) => toast(r.error || `Error ${r.status}`, true);

// ---- folders: listed on demand, kept, refreshed while the page is visible ----
function fmLoad(path, force) {
  if (!force && FM.dirs.has(path)) return Promise.resolve(FM.dirs.get(path));
  if (FM.loading.has(path)) return FM.loading.get(path);
  const p = fetch('/api/fs/list?path=' + encodeURIComponent(path), { credentials: 'same-origin' }).then(async (res) => {
    const txt = await res.text();
    FM.loading.delete(path);
    // the entries as sent are the change key (free space moves all the time): unchanged → not even parsed again
    const key = txt.slice(0, txt.indexOf(',"free"')), old = FM.dirs.get(path);
    if (res.status === 200 && old?.key === key) { old.free = +(txt.match(/"free":(\d+)/)?.[1] || old.free); old.changed = false; return old; }
    let r = {};
    try { r = JSON.parse(txt); } catch { /* not JSON */ }
    if (res.status === 401 && r.error === 'login required') showLogin('Session ended — enter your PIN.');
    if (res.status !== 200) { FM.dirs.set(path, { err: r.error || `Error ${res.status}`, entries: [] }); return FM.dirs.get(path); }
    if (!FM.home && path === '~') FM.home = r.path;
    // the server sends them in order (folders first, natural names): nothing to sort here
    const d = { path: r.path, entries: (r.entries || []).map((e) => ({ ...e, path: fmJoin(r.path, e.name) })), more: r.more, free: r.free, key, changed: true };
    FM.dirs.delete(r.path); // re-inserted: the Map's order is then least recently listed first
    FM.dirs.set(r.path, d);
    if (path !== r.path) FM.dirs.set(path, d);
    if (FM.dirs.size > 300) {
      const keep = new Set([fmAbs(FM.root), FM.root, FM.ncwd, FM.here, ...FM.open, ...fmTabs().map((t) => (t.dir ? t.path : fmDir(t.path)))]);
      for (const k of FM.dirs.keys()) { if (FM.dirs.size <= 250) break; if (!keep.has(k)) FM.dirs.delete(k); }
    }
    return d;
  }).catch(() => { FM.loading.delete(path); return { err: "The panel can't be reached", entries: [] }; });
  FM.loading.set(path, p);
  return p;
}
async function fmRefresh(paths) { // after a change, or on the clock: reload, redraw what changed
  const shown = FM.narrow ? [FM.ncwd] : FM.rows.filter((r) => r.dir && FM.open.has(r.path)).map((r) => r.path);
  const want = new Set(paths || [fmAbs(FM.root), ...shown.slice(0, 40), ...fmTabs().filter((t) => t.dir && t.on).map((t) => t.path)]);
  const ds = await Promise.all([...want].filter(Boolean).map((p) => fmLoad(p, true)));
  if (ds.some((d) => d.changed)) { fmTree(); for (const t of fmTabs()) if (t.dir && t.on && t.view?.paint) t.view.paint(); }
}

// ---- mount: the shell, built once ----
function fmMount(view) {
  const slot = $('#files', view);
  if (!slot || slot === FM.el) return;
  if (FM.el) { slot.replaceWith(FM.el); fmTree(); return; } // back on the page: everything as you left it
  const root = slot;
  FM.el = root;
  FM.w = FM.st.w || Math.round(340 * fmRem());
  FM.focus = !!FM.st.focus;
  FM.split = FM.st.split || 50;
  FM.grid = FM.st.grid !== false;
  root.innerHTML = `<div class="fm-bar">
    <div class="fm-places">${FM_PLACES.map(([l, p]) => `<button type="button" class="chip" data-place="${p}">${l}</button>`).join('')}</div>
    <span class="vsep"></span>
    <button type="button" class="icon-btn ghost sm" data-act="back" title="Back">${ic('angle-left')}</button>
    <button type="button" class="icon-btn ghost sm" data-act="fwd" title="Forward">${ic('angle-right')}</button>
    <button type="button" class="icon-btn ghost sm" data-act="up" title="Up a folder (Backspace)">${ic('angle-up')}</button>
    <nav class="fm-crumbs" aria-label="Path"></nav>
    <button type="button" class="fm-q" data-act="search" title="Search every file (/ or ⌘K)">${ic('search')}<span>Search all files</span><kbd>/</kbd></button>
    <button type="button" class="hbtn" data-act="upload" title="Upload files here (or drop them)">${ic('upload2')}<span>Upload</span></button>
    <button type="button" class="hbtn" data-act="new">${ic('plus')}<span>New</span>${ic('angle-down', 'fm-dd')}</button>
    <button type="button" class="icon-btn ghost sm" data-act="viewmenu" title="View">${ic('more-h')}</button>
    <span class="vsep fm-wide"></span><span class="conn fm-wide"></span>
    <button type="button" class="icon-btn ghost sm bell fm-wide" data-bell aria-haspopup="dialog" aria-expanded="false" aria-label="Notifications" title="Notifications">${ic('bell')}<span class="bell-n" hidden></span></button>
    <button type="button" class="icon-btn ghost sm fm-wide" data-act="lock" aria-label="Lock panel" title="Lock">${ic('lock-keyhole')}</button>
  </div>
  <div class="fm-body">
    <aside class="fm-tree"><div class="fm-th"></div><div class="fm-list" tabindex="0" role="tree" aria-label="Folders"></div><div class="fm-tf"><span>Click to expand · double-click to open</span><span class="fm-free"></span></div></aside>
    <div class="fm-rz" title="Drag to resize · double-click resets"></div>
    <div class="fm-groups"></div>
  </div>
  <input type="file" id="fm-file" multiple hidden><input type="file" id="fm-dirup" webkitdirectory hidden>
  <dialog class="modal fm-dlg"></dialog>`;
  const conn = $('.fm-bar .conn', root); // the header's connection dot, mirrored (the header isn't shown here)
  const sync = () => { conn.className = $('#conn').className + ' fm-wide'; };
  new MutationObserver(sync).observe($('#conn'), { attributes: true });
  sync();
  root.classList.toggle('narrow', FM.narrow);
  FM.list = $('.fm-list', root);
  FM.vl = fmVlist(FM.list, fmRH, fmRowsHTML);
  root.addEventListener('click', fmClick);
  root.addEventListener('dblclick', fmDbl);
  root.addEventListener('contextmenu', fmCtx);
  root.addEventListener('keydown', fmTreeKey);
  root.addEventListener('dragstart', fmDragStart);
  root.addEventListener('dragover', fmDragOver);
  root.addEventListener('dragleave', (e) => { if (!root.contains(e.relatedTarget)) fmDropMark(null); });
  root.addEventListener('drop', fmDrop);
  $('#fm-file', root).addEventListener('change', (e) => { fmUpload([...e.target.files].map((f) => [f, f.name]), FM.upTo); e.target.value = ''; });
  $('#fm-dirup', root).addEventListener('change', (e) => { fmUpload([...e.target.files].map((f) => [f, f.webkitRelativePath || f.name]), FM.upTo); e.target.value = ''; });
  fmResizer($('.fm-rz', root), (x) => { FM.w = Math.max(200, Math.min(x - FM.list.getBoundingClientRect().left + 8, innerWidth * 0.6)); fmLayout(); }, () => { FM.w = Math.round(340 * fmRem()); fmLayout(); });
  fmLayout();
  fmLongPress(root);
  (async () => {
    await fmLoad('~');
    FM.ncwd = fmAbs(FM.root);
    await fmSetRoot(FM.root, true);
    for (const g of FM.st.tabs || []) { // reopen what was open; a tab's viewer starts when it's shown
      const grp = { tabs: [], on: 0 };
      for (const t of g.tabs || []) grp.tabs.push(fmNewTab(t.p, t.d, t.pre));
      if (grp.tabs.length) { grp.on = Math.min(g.on || 0, grp.tabs.length - 1); FM.groups.push(grp); }
    }
    fmGroups();
  })();
  setInterval(() => { if (S.view === 'files' && !document.hidden) fmRefresh(); }, 10000);
  // the editor is what most files open in: fetched while nothing else happens, so the first one opens at once
  const idle = window.requestIdleCallback || ((f) => setTimeout(f, 1500));
  idle(() => fmLib('cm').then(() => idle(() => fmPdfLib().then(() => idle(fmSheetWarm)).catch(() => {}))).catch(() => {}), { timeout: 4000 }); // then pdf.js, then the sheet worker
  addEventListener('focus', () => { if (S.view === 'files') { fmRefresh(); fmCheckDisk(fmActive()); } });
}
VIEWS.files = () => '<div id="files" class="fm" data-own></div>';
S.data.files = {}; // the view draws its own content: render() only needs it present

function fmLayout() {
  FM.el.style.setProperty('--fm-w', FM.w + 'px');
  FM.el.style.setProperty('--fm-split', FM.split + '%');
  FM.el.classList.toggle('focus', FM.focus);
  fmSave();
}
function fmResizer(h, move, reset) {
  h.addEventListener('pointerdown', (e) => {
    e.preventDefault();
    h.setPointerCapture(e.pointerId);
    document.body.classList.add('fm-resizing');
    const mv = (ev) => move(ev.clientX), up = () => { h.removeEventListener('pointermove', mv); document.body.classList.remove('fm-resizing'); fmSave(); };
    h.addEventListener('pointermove', mv);
    h.addEventListener('pointerup', up, { once: true });
  });
  h.addEventListener('dblclick', reset);
}

// a virtual list: a spacer as tall as every row, and only the rows on screen (plus a margin) in the DOM
function fmVlist(box, rh, html) {
  const sp = document.createElement('div'), win = document.createElement('div');
  sp.className = 'fm-sp';
  win.className = 'fm-win';
  box.append(sp, win);
  let items = [], raf = 0, a0 = -1, b0 = -1;
  const h = () => (typeof rh === 'function' ? rh() : rh);
  const paint = (force) => {
    raf = 0;
    const r = h(), a = Math.max(0, Math.floor(box.scrollTop / r) - 10), b = Math.min(items.length, Math.ceil((box.scrollTop + box.clientHeight) / r) + 10);
    if (!force && a === a0 && b === b0) return;
    a0 = a; b0 = b;
    win.style.transform = `translateY(${a * r}px)`;
    win.innerHTML = html(items, a, b);
  };
  box.addEventListener('scroll', () => { raf ||= requestAnimationFrame(() => paint()); }, { passive: true });
  new ResizeObserver(() => paint()).observe(box);
  return { set(xs) { items = xs; sp.style.height = xs.length * h() + 'px'; paint(true); }, paint: () => paint(true), get items() { return items; } };
}

// ---- the tree ----
async function fmSetRoot(p, keep) {
  FM.root = p;
  const d = await fmLoad(p);
  if (!keep) FM.sel = '';
  for (const b of $$('[data-place]', FM.el)) b.setAttribute('aria-pressed', String(b.dataset.place === p));
  $('.fm-free', FM.el).textContent = d.free ? fmtB(d.free) + ' free' : '';
  $('.fm-th', FM.el).innerHTML = `${ic('folder-open')}<span title="${esc(fmTilde(d.path || p))}">${esc(fmBase(d.path || p) || '/')}</span><span class="grow"></span>` +
    `<button type="button" class="icon-btn ghost sm" data-act="collapse" title="Collapse all">${ic('minimize')}</button>`;
  await Promise.all([...FM.open].filter((x) => x.startsWith((d.path || '') + '/')).map((x) => fmLoad(x)));
  fmTree();
  fmCrumbs();
  fmSave();
}
function fmFlat() {
  const d = FM.dirs.get(fmAbs(FM.root)) || FM.dirs.get(FM.root);
  const out = [];
  if (!d) return out;
  if (d.err) return [{ err: d.err, depth: 0 }];
  if (FM.narrow) { // a phone: the folder you're in, one level, drilled into by taps
    const n = FM.dirs.get(FM.ncwd) || d;
    for (const e of n.entries) if (fmShown(e)) { e.depth = 0; out.push(e); }
    return out;
  }
  const walk = (dir, depth) => {
    for (const e of dir.entries) {
      if (!fmShown(e)) continue;
      e.depth = depth; // on the cached entry itself: no copy of every row per redraw
      out.push(e);
      if (e.dir && FM.open.has(e.path)) {
        const kid = FM.dirs.get(e.path);
        if (kid) { if (kid.err) out.push({ err: kid.err, depth: depth + 1 }); else walk(kid, depth + 1); }
        else { out.push({ loading: true, depth: depth + 1 }); fmLoad(e.path).then(fmTree); }
      }
    }
    if (dir.more) out.push({ err: 'More entries than the tree lists: open the folder to see them all', depth });
  };
  walk(d, 0);
  return out;
}
function fmTree() {
  if (!FM.el) return;
  FM.rows = fmFlat();
  FM.vl.set(FM.rows);
}
function fmRowsHTML(rows, a, b) {
  let h = '';
  for (let i = a; i < b; i++) {
    const e = rows[i];
    if (e.err || e.loading) { h += `<div class="fm-row fm-note" style="--d:${e.depth}">${e.loading ? '<i class="spin"></i>' : ''}<span>${esc(e.loading ? 'Loading…' : e.err)}</span></div>`; continue; }
    const sel = e.path === FM.sel || FM.multi.has(e.path), open = e.dir && FM.open.has(e.path), inn = e.path === FM.inFolder;
    h += `<div class="fm-row${sel ? ' on' : ''}${inn ? ' in' : ''}${e.name[0] === '.' ? ' hid' : ''}" role="treeitem" draggable="true" data-p="${esc(e.path)}" data-dir="${e.dir ? 1 : ''}" style="--d:${e.depth}"${e.dir ? ` aria-expanded="${open}"` : ''} aria-selected="${sel}">` +
      `<span class="fm-chev">${e.dir && !FM.narrow ? ic('angle-right') : ''}</span>${fmIcon(e)}<span class="fm-n">${esc(e.name)}</span>` +
      `${e.br ? `<span class="fm-br" title="git branch: ${esc(e.br)}">${ic('hierarchy3')}<span>${esc(e.br)}</span></span>` : ''}${e.link ? `<span class="fm-ln" title="symlink">${ic('arrow-up-right')}</span>` : ''}` +
      `<button type="button" class="fm-act" data-menu tabindex="-1" aria-label="Actions">${ic('more-h')}</button></div>`;
  }
  return h;
}
async function fmToggle(path, on = !FM.open.has(path)) {
  if (on) { FM.open.add(path); await fmLoad(path); } else FM.open.delete(path);
  fmTree();
  fmSave();
}
async function fmReveal(path) { // open the tree down to a path and select it
  const dir = fmDir(path);
  const root = fmAbs(FM.root);
  if (!(path + '/').startsWith(root + '/')) await fmSetRoot(FM_PLACES.map(([, p]) => p).filter((p) => (path + '/').startsWith(fmAbs(p) + '/')).sort((x, y) => fmAbs(y).length - fmAbs(x).length)[0] || '~', true);
  const r2 = fmAbs(FM.root);
  for (let p = dir; p.length > r2.length && p.startsWith(r2); p = fmDir(p)) FM.open.add(p);
  await Promise.all([...FM.open].filter((p) => (dir + '/').startsWith(p + '/')).map((p) => fmLoad(p)));
  if (FM.narrow) FM.ncwd = dir;
  FM.sel = path;
  fmTree();
  const i = FM.rows.findIndex((e) => e.path === path);
  if (i >= 0) FM.list.scrollTop = Math.max(0, i * fmRH() - FM.list.clientHeight / 2);
  fmSave();
}

// ---- tabs, in one or two groups ----
const fmTabs = () => FM.groups.flatMap((g) => g.tabs.map((t, i) => Object.assign(t, { on: i === g.on })));
const fmGroup = () => FM.groups[FM.g] || FM.groups[0];
const fmActive = () => { const g = fmGroup(); return g?.tabs[g.on]; };
function fmNewTab(path, dir, pre) {
  return { id: ++FM.seq, path, name: fmBase(path), dir: !!dir, pre: !!pre, dirty: false, el: null, view: null };
}
// open a file or folder: in the group you're in (or beside it), as a preview tab unless pinned
function fmOpen(path, { dir = false, pin = false, side = false } = {}) {
  if (FM.narrow) side = false;
  if (side && FM.groups.length < 2 && FM.groups.length) FM.groups.push({ tabs: [], on: 0 });
  let gi = side ? (FM.groups.length > 1 ? (FM.g === 0 ? 1 : 0) : 0) : Math.min(FM.g, Math.max(0, FM.groups.length - 1));
  if (!FM.groups.length) { FM.groups.push({ tabs: [], on: 0 }); gi = 0; }
  const g = FM.groups[gi];
  let i = g.tabs.findIndex((t) => t.path === path);
  if (i >= 0) { if (pin) g.tabs[i].pre = false; }
  else {
    const t = fmNewTab(path, dir, !pin);
    const pi = pin ? -1 : g.tabs.findIndex((x) => x.pre && !x.dirty);
    if (pi >= 0) { fmDrop1(g.tabs[pi]); g.tabs[pi] = t; i = pi; } else { i = Math.min(g.on + 1, g.tabs.length); g.tabs.splice(i, 0, t); }
  }
  g.on = i;
  FM.g = gi;
  if (!dir) { FM.recent = [path, ...FM.recent.filter((p) => p !== path)].slice(0, 30); }
  fmGroups();
  if (FM.narrow) FM.el.classList.add('viewing');
}
function fmDrop1(t) { t.view?.destroy?.(); t.el?.remove(); t.el = t.view = null; }
async function fmClose(t, force) {
  if (!force && t.dirty && !(await fmAsk(`Close ${t.name}?`, 'Your unsaved changes will be lost.', 'Discard changes', true))) return;
  for (const g of FM.groups) {
    const i = g.tabs.indexOf(t);
    if (i < 0) continue;
    fmDrop1(t);
    g.tabs.splice(i, 1);
    if (g.on >= g.tabs.length || i < g.on) g.on = Math.max(0, g.on - 1);
  }
  FM.groups = FM.groups.filter((g) => g.tabs.length);
  FM.g = Math.min(FM.g, Math.max(0, FM.groups.length - 1));
  fmGroups();
}
function fmGroups() {
  const box = $('.fm-groups', FM.el);
  $('.fm-empty', box)?.remove();
  // a group per side, its shell kept: tabs and panes come and go inside it
  while (box.children.length > FM.groups.length) box.lastChild.remove();
  FM.groups.forEach((g, gi) => {
    let el = box.children[gi];
    if (!el) {
      el = document.createElement('section');
      el.className = 'fm-grp';
      el.innerHTML = '<div class="fm-tabs" role="tablist"></div><header class="fm-fbar"></header><div class="fm-panes"></div>';
      box.append(el);
    }
    el.dataset.g = gi;
    el.classList.toggle('idle', FM.groups.length > 1 && gi !== FM.g);
    $('.fm-tabs', el).innerHTML = g.tabs.map((t, i) => `<div class="fm-tab${i === g.on ? ' on' : ''}${t.pre ? ' pre' : ''}" role="tab" draggable="true" data-tab="${gi}:${i}" aria-selected="${i === g.on}" title="${esc(fmTilde(t.path))}">` +
      `${t.dir ? ic('folder-open', 'fm-fold') : fxBadge(t.name)}<span>${esc(t.name)}</span>${t.dirty ? '<i class="fm-dot"></i>' : ''}<button type="button" class="fm-x" data-close="${gi}:${i}" tabindex="-1" aria-label="Close">${ic('xmark')}</button></div>`).join('') +
      `<span class="grow"></span><button type="button" class="icon-btn ghost sm fm-split" data-act="split" title="${FM.groups.length > 1 ? 'Back to one side' : 'Open beside (or drag a tab to the right edge)'}">${ic('sidebar-left2')}</button>`;
    const panes = $('.fm-panes', el);
    g.tabs.forEach((t, i) => {
      if (i === g.on && !t.el) { t.el = document.createElement('div'); t.el.className = 'fm-pane'; fmView(t); }
      if (t.el && t.el.parentNode !== panes) panes.append(t.el);
      if (t.el) t.el.hidden = i !== g.on;
    });
    fmBar(g.tabs[g.on], el);
  });
  $('.fm-groups', FM.el).classList.toggle('two', FM.groups.length > 1);
  if (!FM.groups.length) box.insertAdjacentHTML('beforeend', `<div class="fm-empty">${ic('folder-open')}<b>Nothing open</b><span>Click a file in the tree to preview it, double-click a folder to open it here, or press <kbd>/</kbd> to search every file.</span></div>`);
  fmCrumbs();
  fmSave();
}
function fmBar(t, el = FM.el.querySelector(`.fm-grp[data-g="${FM.groups.findIndex((g) => g.tabs.includes(t))}"]`)) {
  if (!t || !el) return;
  const bar = $('.fm-fbar', el);
  const meta = t.meta || '';
  bar.innerHTML = `<button type="button" class="icon-btn ghost sm fm-back" data-act="unview" aria-label="Back to the list">${ic('angle-left')}</button>` +
    `<span class="fm-path" title="${esc(fmTilde(t.path))}">${esc(fmTilde(fmDir(t.path)))}/<b>${esc(t.name)}</b>${meta ? ` · ${esc(meta)}` : ''}</span>` +
    `<span class="fm-lead">${t.lead || ''}</span>` +
    (t.dirty ? `<span class="fm-dirty">Unsaved</span><button type="button" class="btn sm" data-fa="revert">Revert</button><button type="button" class="btn sm primary" data-fa="save">${ic('check')}Save<kbd>⌘S</kbd></button><span class="vsep"></span>` : '') +
    `<button type="button" class="btn sm fm-send" data-fa="send">${icAu('sparkle')}<span>Send to…</span></button>` +
    `<button type="button" class="icon-btn ghost sm" data-fa="copy" title="Copy path">${ic('copy')}</button>` +
    (t.dir ? '' : `<a class="icon-btn ghost sm" href="${fmRaw(t.path)}" target="_blank" rel="noopener" title="Open in a new browser tab">${ic('arrow-up-right')}</a>` +
      `<a class="icon-btn ghost sm" href="${fmRaw(t.path, 1)}" title="Download">${ic('download2')}</a>`) +
    `<button type="button" class="icon-btn ghost sm" data-fa="more" title="More">${ic('more-h')}</button>` +
    `<button type="button" class="icon-btn ghost sm fm-wide${FM.focus ? ' on' : ''}" data-act="focus" title="${FM.focus ? 'Show the tree' : 'Focus: hide the tree'} (F)">${ic(FM.focus ? 'minimize' : 'maximize')}</button>`;
}
function fmSetDirty(t, d) {
  if (t.dirty === d) return;
  t.dirty = d;
  if (d) t.pre = false; // an edit keeps the tab
  fmGroups();
}
function fmCrumbs() {
  const t = fmActive();
  const at = FM.narrow ? FM.ncwd || fmAbs(FM.root) : t ? (t.dir ? t.path : fmDir(t.path)) : fmAbs(FM.root);
  FM.here = at;
  FM.inFolder = t?.dir ? t.path : '';
  const nav = $('.fm-crumbs', FM.el);
  if (!nav || !at) return;
  const parts = fmTilde(at).split('/').filter(Boolean);
  let acc = at.startsWith('/') && !fmTilde(at).startsWith('~') ? '' : FM.home;
  nav.innerHTML = parts.map((p, i) => {
    acc = i === 0 && p === '~' ? FM.home : acc + '/' + p;
    return `${i ? '<i>/</i>' : ''}<button type="button" data-go="${esc(acc)}"${i === parts.length - 1 ? ' class="here"' : ''}>${esc(p)}</button>`;
  }).join('') || '<button type="button" data-go="/">/</button>';
  nav.scrollLeft = nav.scrollWidth;
}
function fmNav(dir, push = true) { // where crumbs, up, back and forward go: a phone's list, else a folder view
  if (!FM.narrow) return fmGo(dir, push);
  if (push) { FM.hist = FM.hist.slice(0, FM.hi + 1).concat(dir); FM.hi = FM.hist.length - 1; }
  FM.ncwd = dir;
  FM.el.classList.remove('viewing');
  fmLoad(dir).then(() => { fmTree(); fmCrumbs(); FM.list.scrollTop = 0; });
}
function fmGo(dir, push = true) { // a folder, opened as a view (the back / forward history is of these)
  if (push) { FM.hist = FM.hist.slice(0, FM.hi + 1).concat(dir); FM.hi = FM.hist.length - 1; }
  const t = fmActive();
  if (t?.dir && !t.dirty) { fmDrop1(t); Object.assign(t, fmNewTab(dir, true, false), { id: t.id }); fmGroups(); }
  else fmOpen(dir, { dir: true, pin: true });
  if (!FM.narrow) fmReveal(dir).then(() => { FM.sel = ''; FM.open.add(dir); fmTree(); });
  else { FM.ncwd = dir; fmTree(); }
}

// ---- viewers: one per kind, each draws into its tab's pane ----
async function fmView(t) {
  t.lead = '';
  t.t0 = performance.now();
  await fmView1(t);
  if (t.el) fmBar(t); // what the viewer learnt (size, pages, the mode switch) shows in the bar
  if (!/^(img|pdf)$/.test(fmKind(t.name)) || t.dir) fmTime(t, t.dir ? 'folder' : 'open'); // a picture, a PDF: when they show
}
// How long it took to open, sent to the panel from whatever device opened it (files-timings.jsonl on the box): what a
// real phone on its real link gets, not a simulation. Kinds and times only.
function fmTime(t, what) {
  if (!t.t0) return;
  const ms = Math.round(performance.now() - t.t0);
  t.t0 = 0;
  fetch('/api/fs/timing', { method: 'POST', credentials: 'same-origin', keepalive: true, headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ what, ext: t.dir ? '' : extOf(t.name), ms, size: fmEntry(t.path)?.size ?? -1, narrow: !!FM.narrow, net: navigator.connection?.effectiveType || '', rtt: navigator.connection?.rtt ?? -1 }) }).catch(() => {});
}
async function fmView1(t) {
  const el = t.el;
  try {
    if (t.dir) return fmFolder(t);
    const k = fmKind(t.name);
    if (k === 'img') return fmImage(t);
    if (k === 'video' || k === 'audio') {
      el.innerHTML = `<div class="fm-media"><${k} controls preload="metadata" src="${fmRaw(t.path)}"></${k}></div>`;
      return;
    }
    if (k === 'pdf') return await fmPDF(t);
    if (k === 'sheet') return await fmSheet(t);
    if (k === 'docx') return await fmDocx(t);
    if (k === 'office') return await fmOffice(t);
    if (k === 'bin') return fmCard(t);
    return await fmText(t, k);
  } catch (err) {
    el.innerHTML = `<div class="fm-card">${ic('alert-triangle')}<b>Couldn't open ${esc(t.name)}</b><span>${esc(String(err?.message || err).slice(0, 300))}</span>` +
      `<div class="fm-row2"><a class="btn sm" href="${fmRaw(t.path, 1)}">${ic('download2')}Download</a></div></div>`;
  }
}
function fmCard(t, why = 'No preview for this type') {
  const e = FM.dirs.get(fmDir(t.path))?.entries.find((x) => x.name === t.name);
  t.el.innerHTML = `<div class="fm-card">${fxBadge(t.name).replace('class="fx', 'class="fx big')}<b>${esc(t.name)}</b><span>${e ? fmtB(e.size) + ' · ' : ''}${esc(why)}</span>` +
    `<div class="fm-row2"><a class="btn sm" href="${fmRaw(t.path, 1)}">${ic('download2')}Download</a><button type="button" class="btn sm fm-send" data-fa="send">${icAu('sparkle')}Send to…</button></div></div>`;
}
function fmImage(t) { // fitted, a photo comes scaled to the screen (not GIFs: they move); 100% fetches the original
  const e = fmEntry(t.path) ?? { path: t.path, size: 1e12, mtime: '' }; // opened from search: the panel sends it as it is if it's small
  const fit = /\.gif$/i.test(t.name) ? fmRaw(t.path) : fmPic(e, ...fmFit());
  const full = fmHeic(t.path) ? fmPic(e, 4096, 4096) : fmRaw(t.path); // HEIC at 100%: all of it, made viewable
  t.el.innerHTML = `<div class="fm-img fit"><img src="${fit}" alt="${esc(t.name)}" decoding="async"${fit === full ? '' : ` data-full="${full}"`}></div>`;
  t.lead = '<div class="seg sm"><button type="button" data-fa="fit" aria-pressed="true">Fit</button><button type="button" data-fa="fit">100%</button></div>';
  const img = $('img', t.el);
  img.addEventListener('load', () => { fmTime(t, 'image'); t.meta = img.dataset.full ? (e.mtime ? fmtB(e.size) + ' · ' : '') + 'scaled to fit' : `${img.naturalWidth} × ${img.naturalHeight}`; fmBar(t); });
  img.addEventListener('error', () => fmCard(t, "This browser can't show this image"), { once: true });
}

// text: the editor, straight away (Save shows once something changed)
async function fmText(t, kind) {
  const log = /\.(log|out|err)$/i.test(t.name), el = t.el;
  const get = async (n, priority = 'auto', signal) => { // its first (a log: last) n bytes, gzipped on the way
    const r = await fetch(fmRaw(t.path) + (log ? '&tail=' : '&head=') + n, { credentials: 'same-origin', priority, signal });
    if (r.status !== 200) throw new Error((await r.json().catch(() => ({}))).error || `Error ${r.status}`);
    const buf = new Uint8Array(await r.arrayBuffer());
    return { buf, total: +(r.headers.get('X-Total') || buf.length), mtime: +r.headers.get('X-Mtime') || 0 };
  };
  const read = ({ buf, total }, cut) => { // → its text, or null if it isn't text
    if (cut) { const i = log ? buf.indexOf(10) + 1 : buf.lastIndexOf(10) + 1; if (i > 0) buf = log ? buf.subarray(i) : buf.subarray(0, i); } // whole lines
    if (buf.subarray(0, 8000).includes(0)) return null;
    try { return new TextDecoder('utf-8', { fatal: true }).decode(buf); } catch { return total <= buf.length ? null : new TextDecoder().decode(buf); }
  };
  const said = (got, total) => fmtB(total) + (total > got ? ` · read-only, showing the ${log ? 'last' : 'first'} ${fmtB(got)}` : '');
  // A big file shows its first 256 KB at once (its last, a log), read-only, while the rest (to 5 MB) comes. Not a CSV:
  // its table would be built twice.
  const first = await get(kind === 'csv' ? FM_TEXT_MAX : FM_TEXT_QUICK);
  const more = first.total > first.buf.length && first.buf.length < FM_TEXT_MAX;
  const text = read(first, more);
  if (text === null) return fmCard(t, first.buf.subarray(0, 8000).includes(0) ? 'Binary file: no preview' : "Not UTF-8 text: no preview");
  t.mtime = first.mtime;
  t.part = first.total > first.buf.length;
  t.meta = more ? `${fmtB(first.total)} · loading the rest…` : said(first.buf.length, first.total);
  t.text = text;
  if (more) fmTextRest(t, el, get, read, said, log); // not awaited: the editor goes up now
  if (kind === 'md' || kind === 'html' || kind === 'csv') {
    t.mode = t.mode || (kind === 'csv' ? 'table' : 'view');
    t.lead = `<div class="seg sm">${(kind === 'md' ? [['view', 'Preview'], ['split', 'Split'], ['src', 'Source']] : kind === 'csv' ? [['table', 'Table'], ['src', 'Source']] : [['view', 'Page'], ['src', 'Source']])
      .map(([m, l]) => `<button type="button" data-mode="${m}" aria-pressed="${t.mode === m}">${l}</button>`).join('')}</div>`;
    t.kind = kind;
    return fmMode(t, t.mode);
  }
  await fmEditor(t, t.el);
  if (log && t.part) t.ed.view.dispatch({ selection: { anchor: t.ed.view.state.doc.length }, scrollIntoView: true });
}
async function fmTextRest(t, el, get, read, said, log) { // the rest of a big text file, into what's on screen
  const shown = async () => { // behind another tab, it waits: the link is for what's in front, and an editor that can't
    if (el.offsetWidth) return; // measure itself lays out all 5 MB (seconds, the page frozen)
    await new Promise((ok) => { const ro = new ResizeObserver(() => { if (el.offsetWidth) { ro.disconnect(); ok(); } }); ro.observe(el); });
    await new Promise((ok) => requestAnimationFrame(() => setTimeout(ok, 0))); // after it has laid itself out again
  };
  let all = null;
  while (t.el === el && !all) { // put behind another tab, the download stops; shown again, it starts over (a few hundred KB)
    await shown();
    const ac = new AbortController(), ro = new ResizeObserver(() => { if (!el.offsetWidth) ac.abort(); });
    ro.observe(el);
    all = await get(FM_TEXT_MAX, 'low', ac.signal).catch(() => null);
    ro.disconnect();
    if (!all && !ac.signal.aborted) return; // it failed: the first part stays
  }
  const text = all && read(all, false);
  if (t.el !== el || text === null || text === undefined) return; // closed or reopened meanwhile
  await shown();
  if (t.el !== el) return;
  t.text = text;
  t.part = all.total > all.buf.length;
  t.mtime = all.mtime;
  t.meta = said(all.buf.length, all.total);
  for (let i = 0; i < 100 && !t.ed && t.kind !== 'md' && t.kind !== 'html'; i++) await new Promise((r) => setTimeout(r, 30)); // the editor, being made
  if (t.ed) {
    t.ed.set(text);
    t.ed.readOnly(t.part);
    if (log) t.ed.view.dispatch({ selection: { anchor: t.ed.view.state.doc.length }, scrollIntoView: true });
  }
  if (t.kind === 'md' && t.mode !== 'src') fmMode(t, t.mode); // the preview, whole
  else fmBar(t);
}
async function fmEditor(t, box) {
  await fmLib('cm');
  box.innerHTML = '';
  box.classList.add('fm-ed');
  t.ed = CM.editor(box, { doc: t.text, name: t.name, readOnly: t.part, onChange: (d) => { fmSetDirty(t, d); if (t.mode === 'split') fmMdLive(t); }, onSave: () => fmSaveTab(t) });
  const old = t.view?.destroy;
  t.view = { destroy: () => { old?.(); t.ed?.destroy(); t.ed = null; } };
}
async function fmMode(t, m) { // md: Preview / Split / Source; csv: Table / Source; html: Page / Source
  t.mode = m;
  t.lead = t.lead.replace(/aria-pressed="true"/g, 'aria-pressed="false"').replace(`data-mode="${m}" aria-pressed="false"`, `data-mode="${m}" aria-pressed="true"`);
  if (t.sheetDirty && m === 'src') { // the table's edits, as text, into the editor (still unsaved)
    const csv = (await t.sw.ask({ op: 'write', csv: true, fs: /\.tsv$/i.test(t.name) ? '\t' : ',' })).body;
    if (t.ed) t.ed.view.dispatch({ changes: { from: 0, to: t.ed.view.state.doc.length, insert: csv } }); else t.text = csv;
    t.sheetDirty = false;
  }
  const text = t.ed ? t.ed.text() : t.text;
  if (t.ed) { t.text = text; }
  const keep = t.ed; // the editor (its undo history) survives a switch: it's moved, not rebuilt
  const edBox = keep?.view.dom.parentNode;
  t.el.innerHTML = '';
  t.el.classList.remove('fm-ed');
  const add = (cls) => { const d = document.createElement('div'); d.className = cls; t.el.append(d); return d; };
  const editorInto = async (box) => { if (keep) { box.classList.add('fm-ed'); box.append(keep.view.dom); edBox?.remove(); keep.view.requestMeasure(); } else await fmEditor(t, box); };
  if (m === 'src') await editorInto(t.el);
  else if (m === 'split') { t.el.classList.add('fm-split2'); await editorInto(add('fm-half')); add('fm-half fm-mdv md'); fmMdLive(t, true); }
  else if (t.kind === 'md') { add('fm-mdv md').innerHTML = md(text); }
  else if (t.kind === 'html') { add('fm-frame').innerHTML = `<iframe sandbox="allow-scripts allow-popups" referrerpolicy="no-referrer" src="${fmRaw(t.path)}" title="${esc(t.name)}"></iframe>`; }
  else if (t.kind === 'csv') { const box = add('fm-sheetbox'); let view = null; const book = await fmBook(t, text, (first) => { view = fmSheetView(t, { ...first, partial: true }, box); }); if (view) view.full(book); else fmSheetView(t, book, box); }
  fmBar(t);
}
function fmMdLive(t, now) {
  clearTimeout(t.mdT);
  t.mdT = setTimeout(() => { const v = $('.fm-mdv', t.el); if (v && t.ed) v.innerHTML = md(t.ed.text()); }, now ? 0 : 120);
}
async function fmSaveTab(t, force) {
  if (t?.sw && t.sheetDirty && t.mode !== 'src') return fmSaveSheet(t, force);
  if (!t?.ed || t.part) return;
  const text = t.ed.text();
  const r = await fetch(`/api/fs/file?path=${encodeURIComponent(t.path)}&mtime=${t.mtime || 0}${force ? '&force=1' : ''}`, { method: 'PUT', credentials: 'same-origin', body: text });
  const j = await r.json().catch(() => ({}));
  if (r.status === 409 && j.error === 'changed on disk') {
    if (await fmAsk(`${t.name} changed on disk`, 'Something else saved this file after you opened it. Overwrite it with your version, or cancel and use Revert to load theirs.', 'Overwrite', true)) return fmSaveTab(t, true);
    return;
  }
  if (r.status !== 200) return fmErr({ status: r.status, ...j });
  t.mtime = j.mtime;
  t.text = text;
  t.ed.clean();
  t.meta = fmtB(j.size);
  fmBar(t);
  fmRefresh([fmDir(t.path)]);
}
async function fmRevert(t) {
  if (t.dirty && !(await fmAsk(`Revert ${t.name}?`, 'Your unsaved changes will be lost, and the file reloads from disk.', 'Revert', true))) return;
  fmDrop1(t);
  t.dirty = t.sheetDirty = false;
  fmGroups();
}
async function fmCheckDisk(t) { // the file changed under an open tab: reload it if you haven't touched it
  if (!t || t.dir || !t.mtime || !t.el) return;
  const r = await fetch(fmRaw(t.path), { method: 'HEAD', credentials: 'same-origin' }).catch(() => null);
  const m = +r?.headers.get('X-Mtime');
  if (!m || m === t.mtime) return;
  if (!t.dirty) { const keepMode = t.mode; fmDrop1(t); t.mode = keepMode; fmGroups(); }
  else toast(`${t.name} changed on disk: saving asks before it overwrites`, true);
}

// pdf: pdf.js, pages drawn as they scroll into view, with a text layer (select, copy, the browser's find)
async function fmPDF(t) {
  t.el.innerHTML = '<div class="fm-pdfbar"></div><div class="fm-pdf"><div class="fm-load"><i class="spin"></i></div></div>';
  const lib = await fmPdfLib();
  // a PDF up to 8 MB comes in one request (each range is a round trip through the tunnel); a bigger one by ranges, as it scrolls
  const size = fmEntry(t.path)?.size ?? Infinity;
  const src = size < 8 << 20 ? { data: new Uint8Array(await (await fetch(fmRaw(t.path) + '&gz=1', { credentials: 'same-origin' })).arrayBuffer()) } : { url: fmRaw(t.path), disableAutoFetch: true, withCredentials: true };
  const doc = await lib.getDocument({ ...src, worker: fmPdfWorker, isEvalSupported: false }).promise;
  const box = $('.fm-pdf', t.el), bar = $('.fm-pdfbar', t.el);
  const first = await doc.getPage(1);
  const base = first.getViewport({ scale: 1 });
  let scale = 0, fitW = true, cur = 1;
  const pages = [];
  const fit = () => Math.max(0.3, Math.min(4, (box.clientWidth - 48 * fmRem()) / base.width));
  const paintBar = () => {
    bar.innerHTML = `<button type="button" class="icon-btn ghost sm" data-pdf="-1" title="Previous page">${ic('angle-left')}</button><span class="fm-pg"><input type="text" inputmode="numeric" value="${cur}" aria-label="Page"> of ${doc.numPages}</span>` +
      `<button type="button" class="icon-btn ghost sm" data-pdf="1" title="Next page">${ic('angle-right')}</button><span class="vsep"></span>` +
      `<button type="button" class="icon-btn ghost sm" data-pdf="out" title="Zoom out">−</button><span class="fm-zoom">${Math.round(scale * 100)}%</span><button type="button" class="icon-btn ghost sm" data-pdf="in" title="Zoom in">+</button>` +
      `<button type="button" class="btn sm" data-pdf="fit" aria-pressed="${fitW}">Fit width</button><span class="grow"></span><span class="faint">Text is selectable · the browser's find searches it</span>`;
  };
  let queue = Promise.resolve(); // one page drawn at a time, in the order they come near: the first one isn't kept waiting by the next three
  const io = new IntersectionObserver((xs) => { for (const x of xs) if (x.isIntersecting) { const n = +x.target.dataset.n; queue = queue.then(() => draw(n)).catch(() => {}); } }, { root: box, rootMargin: '600px 0px' });
  const far = new IntersectionObserver((xs) => { // a few screens away: its canvas goes (memory), redrawn on the way back
    for (const x of xs) if (!x.isIntersecting) { const p = pages[+x.target.dataset.n - 1]; if (p.drawn) { p.task?.cancel(); p.drawn = 0; p.el.replaceChildren(); } }
  }, { root: box, rootMargin: '3000px 0px' });
  const seen = new IntersectionObserver((xs) => {
    for (const x of xs) if (x.isIntersecting && x.intersectionRatio > 0.4) { cur = +x.target.dataset.n; const i = $('.fm-pg input', bar); if (i && document.activeElement !== i) i.value = cur; }
  }, { root: box, threshold: [0.4] });
  async function draw(n) {
    const p = pages[n - 1];
    if (p.drawn === scale) return;
    p.drawn = scale;
    const page = n === 1 ? first : await doc.getPage(n);
    const vp = page.getViewport({ scale });
    const dpr = Math.min(devicePixelRatio || 1, 2);
    const c = document.createElement('canvas');
    c.width = Math.floor(vp.width * dpr);
    c.height = Math.floor(vp.height * dpr);
    p.el.style.width = vp.width + 'px';
    p.el.style.height = vp.height + 'px';
    p.task?.cancel();
    p.task = page.render({ canvasContext: c.getContext('2d'), viewport: vp, transform: dpr !== 1 ? [dpr, 0, 0, dpr, 0, 0] : null });
    try { await p.task.promise; } catch { return; }
    if (p.drawn !== scale) return;
    if (n === 1) fmTime(t, 'page');
    const tl = document.createElement('div');
    tl.className = 'textLayer';
    p.el.replaceChildren(c, tl);
    tl.style.setProperty('--scale-factor', vp.scale);
    tl.style.setProperty('--total-scale-factor', vp.scale);
    new lib.TextLayer({ textContentSource: page.streamTextContent(), container: tl, viewport: vp }).render().catch(() => {});
  }
  const layout = () => {
    scale = fitW ? fit() : scale;
    for (const p of pages) { p.el.style.width = base.width * scale + 'px'; p.el.style.height = base.height * scale + 'px'; p.drawn = 0; }
    for (const p of pages) { io.unobserve(p.el); io.observe(p.el); }
    paintBar();
  };
  box.innerHTML = '';
  for (let n = 1; n <= doc.numPages; n++) {
    const el = document.createElement('div');
    el.className = 'fm-page';
    el.dataset.n = n;
    box.append(el);
    pages.push({ el, drawn: 0 });
    seen.observe(el);
    far.observe(el);
  }
  layout();
  t.meta = `${doc.numPages} page${doc.numPages === 1 ? '' : 's'}`;
  fmBar(t);
  const ro = new ResizeObserver(() => { if (fitW && Math.abs(fit() - scale) > 0.01) layout(); });
  ro.observe(box);
  bar.addEventListener('click', (e) => {
    const b = e.target.closest('[data-pdf]');
    if (!b) return;
    const a = b.dataset.pdf;
    if (a === '1' || a === '-1') { const n = Math.min(doc.numPages, Math.max(1, cur + +a)); pages[n - 1].el.scrollIntoView({ block: 'start' }); return; }
    fitW = a === 'fit';
    if (!fitW) scale = Math.max(0.3, Math.min(4, scale * (a === 'in' ? 1.2 : 1 / 1.2)));
    const at = cur;
    layout();
    pages[at - 1].el.scrollIntoView({ block: 'start' });
  });
  bar.addEventListener('change', (e) => { const n = Math.min(doc.numPages, Math.max(1, +e.target.value || 1)); pages[n - 1].el.scrollIntoView({ block: 'start' }); });
  t.view = { destroy: () => { io.disconnect(); seen.disconnect(); far.disconnect(); ro.disconnect(); doc.destroy(); } };
}

// spreadsheets: SheetJS in a worker (sheet-worker.js), every sheet, a virtual grid, the formula bar as the cell editor
let fmSW = null; // one worker for every sheet tab (SheetJS loads in it once); each tab's workbook kept under its id
function fmSheetWorker(book) { // → { ask(message): its answer, end(): the tab's workbook goes }
  if (!fmSW) {
    const w = new Worker(fmMeta('sheetworker')), wait = new Map();
    w.onmessage = (e) => {
      const p = wait.get(e.data.id);
      if (e.data.partial) return p?.[2]?.(e.data); // a big file's first rows, before the answer
      wait.delete(e.data.id);
      if (p) e.data.error ? p[1](new Error(e.data.error)) : p[0](e.data);
    };
    w.onerror = (e) => { e.preventDefault(); for (const [, p] of wait) p[1](new Error(e.message || "The sheet couldn't be read")); wait.clear(); };
    fmSW = { w, wait, seq: 0, open: 0 };
  }
  const sw = fmSW;
  sw.open++;
  clearTimeout(sw.idle);
  return {
    ask: (m, move, partial) => new Promise((ok, no) => { const id = ++sw.seq; sw.wait.set(id, [ok, no, partial]); sw.w.postMessage({ ...m, id, book }, move || []); }),
    tell: (m) => sw.w.postMessage({ ...m, book }), // no answer
    end: () => { // the tab's workbook goes now; the worker itself after two idle minutes (a next sheet skips its start-up)
      sw.w.postMessage({ op: 'close', book });
      if (--sw.open === 0) sw.idle = setTimeout(() => { if (fmSW === sw && !sw.open) { sw.w.terminate(); fmSW = null; } }, 120000);
    },
  };
}
function fmSheetWarm() { // the sheet worker started with SheetJS loaded, ahead of the first sheet (it ends after two idle minutes)
  if (fmSW) return;
  const sw = fmSheetWorker(0);
  fmSW.w.postMessage({ op: 'warm', lib: fmMeta('xlsx') });
  sw.end();
}
async function fmBook(t, text, partial) { // → { names, sheet } (the first sheet, packed); partial(first rows) for a big file
  if (!t.sw) {
    t.sw = fmSheetWorker(t.id);
    const old = t.view?.destroy;
    t.view = { destroy: () => { old?.(); t.sw?.end(); t.sw = null; } };
  }
  const lib = fmMeta('xlsx');
  const preview = (text?.length ?? 0) > 2e6 ? 3000 : 0;
  if (text !== undefined) return t.sw.ask({ op: 'read', lib, src: text, preview, opts: { type: 'string', raw: true, dense: true, FS: /\.tsv$/i.test(t.name) ? '\t' : undefined } }, [], partial);
  const opts = { type: 'array', dense: true, cellFormula: true, cellDates: true, cellNF: true, cellStyles: true, cellHTML: false };
  if ((fmEntry(t.path)?.size ?? Infinity) > 1.5e6 && /\.xls[xm]$/i.test(t.name)) { // big (or not listed yet): the worker fetches it itself, first rows by ranges
    const ro = new ResizeObserver(() => { const on = !!t.el?.offsetWidth; if (on !== ro.on) t.sw.tell({ op: 'shown', on: (ro.on = on) }); }); // behind another tab, its download waits
    ro.on = true;
    ro.observe(t.el);
    try {
      const book = await t.sw.ask({ op: 'read', lib, url: new URL(fmRaw(t.path), location.href).href, preview: 3000, opts }, [], (f) => { t.mtime ||= f.mtime; partial(f); });
      t.mtime = Math.max(t.mtime || 0, book.mtime || 0); // saved meanwhile (a patch): that newer version stands
      return book;
    } finally { ro.disconnect(); }
  }
  const r = await fetch(fmRaw(t.path), { credentials: 'same-origin' });
  if (!r.ok) throw new Error(`Error ${r.status}`);
  t.mtime = +r.headers.get('X-Mtime') || 0;
  const buf = await r.arrayBuffer();
  return t.sw.ask({ op: 'read', lib, src: buf, preview: buf.byteLength > 1.5e6 ? 3000 : 0, opts }, [buf], partial);
}
async function fmSheet(t) {
  t.el.innerHTML = '<div class="fm-load"><i class="spin"></i></div>';
  let view = null;
  const into = (book) => {
    if (view) return view.full(book);
    t.el.innerHTML = '';
    const box = document.createElement('div');
    box.className = 'fm-sheetbox';
    t.el.append(box);
    view = fmSheetView(t, book, box);
    fmTime(t, 'rows'); // its first rows on screen (the rest may still be coming)
  };
  into(await fmBook(t, undefined, (first) => into({ ...first, partial: true })));
}
function fmSheetView(t, book, box) { // the formula bar is the cell editor: type over a cell, Enter / Tab commits
  box.innerHTML = '<div class="fm-cellbar"><span class="fm-cell">A1</span><i>fx</i><input class="fm-fx" type="text" spellcheck="false" autocomplete="off" aria-label="Cell content (type, then Enter)"><span class="fm-val"></span></div>' +
    '<div class="fm-grid" tabindex="0" aria-label="Sheet: arrows move, typing edits, Enter edits, Delete clears"></div><div class="fm-sheets"></div>';
  const grid = $('.fm-grid', box), sheets = $('.fm-sheets', box), fx = $('.fm-fx', box);
  let sh, rows, cols, sel = [0, 0], orig = '', part = !!book.partial; // part: the first rows, the rest still loading
  let pend = []; // edits made meanwhile: the worker applies them to the whole book; until it's in, onto each part shown
  const mark = (p, r, c, raw) => { // a typed value into a packed sheet, as it'll show
    if (r >= p.rows || c >= p.cols) return;
    p.text[r * p.cols + c] = raw[0] === '=' ? '' : raw;
    p.kind[r * p.cols + c] = raw[0] === '=' ? 2 : /^[-+]?(\d+\.?\d*|\.\d+)(e[-+]?\d+)?$/i.test(raw.trim()) ? 1 : 0;
  };
  const colName = (c) => { let s = ''; for (c++; c; c = Math.floor((c - 1) / 26)) s = String.fromCharCode(65 + ((c - 1) % 26)) + s; return s; };
  const at = (r, c) => (r < sh.rows && c < sh.cols ? r * sh.cols + c : -1);
  const show = (pack, keep) => { // a packed sheet into the grid (one empty row and column past the data, to type into)
    sh = pack;
    if (part) for (const [n, r, c, raw] of pend) if (n === sh.name) mark(sh, r, c, raw);
    rows = Math.min(sh.rows + !part, 1e6);
    cols = Math.min(sh.cols + !part, 200);
    const widths = Array.from({ length: cols }, (_, c) => { // a column's width from what its first rows hold
      let w = 3;
      for (let r = 0; r < Math.min(sh.rows, 60); r++) w = Math.max(w, (sh.text[at(r, c)] || '').length);
      return Math.max(64, Math.min(320, 22 + w * 7.4)) / 16; // rem: as wide as its first rows' text, at the sheet's font
    });
    const top = keep ? grid.scrollTop : 0, left = keep ? grid.scrollLeft : 0;
    grid.style.setProperty('--cols', `3rem ${widths.map((w) => w + 'rem').join(' ')}`);
    grid.innerHTML = `<div class="fm-gh">${['', ...widths.map((_, c) => colName(c))].map((h, i) => `<span${i ? ` data-c="${i - 1}"` : ''}>${h}</span>`).join('')}</div>`;
    grid.vl = fmVlist(grid, () => 28 * fmRem(), (xs, a, b) => { // 28: .fm-gr's 1.75rem
      let h = '';
      for (let r = a; r < b; r++) {
        h += `<div class="fm-gr" data-r="${r}"><span>${r + 1}</span>`;
        for (let c = 0; c < cols; c++) {
          const i = at(r, c), k = i < 0 ? 0 : sh.kind[i];
          h += `<span class="${k ? 'n' : ''}${sel[0] === r && sel[1] === c ? ' sel' : ''}" data-c="${c}">${i < 0 ? '' : esc(sh.text[i].slice(0, 400))}</span>`;
        }
        h += '</div>';
      }
      return h;
    });
    grid.vl.set(Array.from({ length: rows }, (_, r) => r));
    grid.scrollTop = top;
    grid.scrollLeft = left;
    t.meta = `${(sh.allRows || sh.rows).toLocaleString()} rows · ${sh.allCols} columns${sh.allCols > 200 ? ' (first 200 shown)' : ''}`;
    fmBar(t);
    sheets.innerHTML = book.names.map((x) => `<button type="button" data-sheet="${esc(x)}" aria-pressed="${x === sh.name}">${esc(x)}</button>`).join('');
  };
  const pick = (r, c) => {
    sel = [Math.max(0, Math.min(rows - 1, r)), Math.max(0, Math.min(cols - 1, c))];
    const i = at(...sel), shown = i < 0 ? '' : sh.text[i];
    $('.fm-cell', box).textContent = colName(sel[1]) + (sel[0] + 1);
    fx.value = orig = shown; // what it shows now; its formula follows from the worker (a millisecond)
    $('.fm-val', box).textContent = '';
    const rh = 28 * fmRem(), y = sel[0] * rh, room = grid.clientHeight - ($('.fm-gh', grid)?.offsetHeight || 0); // on screen, below the column letters
    if (y < grid.scrollTop) grid.scrollTop = y; else if (y + rh > grid.scrollTop + room) grid.scrollTop = y + rh - room;
    grid.vl.paint();
    if (i >= 0 && sh.kind[i] === 2 || i >= 0 && shown) {
      const want = sel.join(':');
      t.sw.ask({ op: 'cell', name: sh.name, r: sel[0], c: sel[1] }).then(({ cell }) => {
        if (want !== sel.join(':') || !cell) return;
        if (document.activeElement !== fx) fx.value = orig = cell.src;
        if (cell.f) $('.fm-val', box).innerHTML = `value <b>${esc(cell.shown || '(recalculates in Excel)')}</b>`;
      }).catch(() => {});
    }
  };
  const commit = async () => { // the bar's text into the selected cell, here (at once) and in the worker's workbook
    const raw = fx.value, [r, c] = sel;
    if (raw === orig) return false;
    if (sh.kind[at(r, c)] === 3) { // as in Excel: an array formula changes only as a whole
      fx.value = orig;
      toast(`${colName(c)}${r + 1} is part of an array formula: Excel changes those only as a whole`, true);
      return false;
    }
    orig = raw;
    const grew = r >= sh.rows || c >= sh.cols;
    mark(sh, r, c, raw);
    if (part) pend.push([sh.name, r, c, raw]);
    (t.edits ||= []).push({ sheet: sh.name, r, c, raw }); // what an Excel save sends: the panel patches just these cells
    t.sheetDirty = true;
    fmSetDirty(t, true);
    grid.vl.paint();
    await t.sw.ask({ op: 'set', name: sh.name, r, c, raw });
    if (grew) show((await t.sw.ask({ op: 'sheet', name: sh.name })).sheet, true); // a new empty row / column to type into
    return true;
  };
  grid.addEventListener('click', (e) => {
    const c = e.target.closest('[data-c]'), r = e.target.closest('[data-r]');
    if (c && r) pick(+r.dataset.r, +c.dataset.c);
  });
  grid.addEventListener('dblclick', (e) => { if (e.target.closest('[data-r] [data-c]')) { fx.focus(); fx.setSelectionRange(fx.value.length, fx.value.length); } });
  grid.addEventListener('keydown', (e) => {
    if (e.metaKey || e.ctrlKey || e.altKey || e.isComposing) return;
    const k = e.key, [r, c] = sel;
    const mv = { ArrowDown: [1, 0], ArrowUp: [-1, 0], ArrowRight: [0, 1], ArrowLeft: [0, -1], Tab: [0, e.shiftKey ? -1 : 1] }[k];
    if (mv) pick(r + mv[0], c + mv[1]);
    else if (k === 'Enter' || k === 'F2') { fx.focus(); fx.setSelectionRange(fx.value.length, fx.value.length); }
    else if (k === 'Delete' || k === 'Backspace') { fx.value = ''; commit(); }
    else if (k.length === 1) { fx.value = k; fx.focus(); } // typing replaces the cell, as in a spreadsheet
    else return;
    e.preventDefault();
    e.stopPropagation();
  });
  fx.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === 'Tab') {
      e.preventDefault();
      commit();
      const [r, c] = sel;
      pick(e.key === 'Enter' ? r + (e.shiftKey ? -1 : 1) : r, e.key === 'Tab' ? c + (e.shiftKey ? -1 : 1) : c);
      grid.focus();
    } else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); pick(...sel); grid.focus(); }
  });
  fx.addEventListener('change', () => commit()); // clicked away with an edit in the bar
  sheets.addEventListener('click', async (e) => {
    const b = e.target.closest('[data-sheet]');
    if (!b) return; // still loading: its first rows, by ranges
    sel = [0, 0];
    show((await t.sw.ask({ op: 'sheet', name: b.dataset.sheet })).sheet);
    pick(0, 0);
  });
  box.classList.toggle('fm-partial', part); // edits meanwhile wait in the worker for the whole book; a save waits for it
  show(book.sheet);
  pick(0, 0);
  return {
    async full(b) { // more of the book: the first sheet's next rows, or the whole of it
      if (b.partial && sh.name !== b.sheet.name) return; // looking at another sheet meanwhile
      book = b;
      part = !!b.partial;
      if (!part) pend = [];
      box.classList.toggle('fm-partial', part);
      show(sh.name === b.sheet.name ? b.sheet : (await t.sw.ask({ op: 'sheet', name: sh.name })).sheet, true); // the sheet on screen, whole
      pick(...sel);
    },
  };
}
// A sheet's edits saved: CSV / TSV as text (nothing lost); a workbook rewritten by SheetJS, which keeps values,
// formulas, number formats, merges and column widths but not colours, fonts or charts: it asks once, and offers a copy.
async function fmSaveSheet(t, force) {
  const x = extOf(t.name), flat = x === 'csv' || x === 'tsv';
  let path = t.path;
  if ((x === 'xlsx' || x === 'xlsm') && t.edits?.length) { // patched in the file: nothing else in it changes, and nothing waits for the whole book
    const post = (edits) => fetch(`/api/fs/xlsx?path=${encodeURIComponent(path)}&mtime=${t.mtime || 0}${force ? '&force=1' : ''}`,
      { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ edits }) });
    let r = await post(t.edits), j = await r.json().catch(() => ({})), more = [];
    for (let i = 0; i < 5 && r.status === 422 && j.shared; i++) { // the first cell of a shared formula: its other cells written out (each its own formula), then again
      more = [...more, ...(await t.sw.ask({ op: 'formulas', name: j.shared.sheet, ref: j.shared.ref })).cells]; // (needs the whole book)
      r = await post([...more, ...t.edits]); // the edits after: they win
      j = await r.json().catch(() => ({}));
    }
    if (r.status === 409 && j.error === 'changed on disk') {
      if (await fmAsk(`${t.name} changed on disk`, 'Something else saved this file after you opened it. Overwrite it with your version?', 'Overwrite', true)) return fmSaveSheet(t, true);
      return;
    }
    if (r.status === 200) {
      t.mtime = j.mtime;
      t.edits = [];
      t.sheetDirty = false;
      fmSetDirty(t, false);
      fmRefresh([fmDir(path)]);
      return;
    }
    if (r.status !== 422) return fmErr({ status: r.status, ...j }); // 422: a workbook it can't patch (a shared formula's first cell, say): SheetJS writes it
  }
  if (!flat && !t.lossyOK) {
    const v = await fmDialog(`<div class="modal-h"><span class="modal-ic">${ic('alert-triangle')}</span><h2>Save ${esc(t.name)}?</h2></div>` +
      `<p class="muted fm-pre">The panel saves values, formulas, number formats, merged cells and column widths. Colours, fonts, borders, pictures and charts are dropped. A copy keeps the original as it is.</p>` +
      '<div class="modal-actions"><button type="button" class="btn" data-cancel>Cancel</button><button type="submit" class="btn" value="copy">Save a copy</button><button type="submit" class="btn danger" value="over">Overwrite</button></div>',
    { submit: (e) => e.submitter?.value || false });
    if (v !== 'copy' && v !== 'over') return;
    if (v === 'copy') {
      const base = t.name.replace(/\.[^.]+$/, '');
      for (let i = 1; i < 100; i++) { const c = fmJoin(fmDir(t.path), `${base} (edited${i > 1 ? ' ' + i : ''}).${x}`); if (!FM.dirs.get(fmDir(t.path))?.entries.some((e) => e.path === c)) { path = c; break; } }
    }
    t.lossyOK = true;
  }
  const { body } = await t.sw.ask({ op: 'write', csv: flat, fs: x === 'tsv' ? '\t' : ',', bookType: { xls: 'biff8', ods: 'ods', xlsm: 'xlsm' }[x] || 'xlsx' }); // written off the page too
  const copy = path !== t.path;
  const r = await fetch(`/api/fs/file?path=${encodeURIComponent(path)}&mtime=${copy ? 0 : t.mtime || 0}${force ? '&force=1' : ''}`, { method: 'PUT', credentials: 'same-origin', body });
  const j = await r.json().catch(() => ({}));
  if (r.status === 409 && j.error === 'changed on disk') {
    if (await fmAsk(`${t.name} changed on disk`, 'Something else saved this file after you opened it. Overwrite it with your version?', 'Overwrite', true)) return fmSaveSheet(t, true);
    return;
  }
  if (r.status !== 200) return fmErr({ status: r.status, ...j });
  if (copy) { t.path = path; t.name = fmBase(path); } // the tab goes on with the copy; the original is untouched
  t.mtime = j.mtime;
  t.edits = [];
  t.sheetDirty = false;
  if (flat) { t.text = body; if (t.ed) t.ed.set(body); }
  fmSetDirty(t, false);
  fmRefresh([fmDir(path)]);
  if (copy) toast(`Saved a copy: ${t.name}`);
}

// Word: docx-preview (headings, tables, images, page breaks)
async function fmDocx(t) {
  t.el.innerHTML = '<div class="fm-docx"><div class="fm-load"><i class="spin"></i></div></div>';
  await fmLib('jszip');
  await fmLib('docx');
  const r = await fetch(fmRaw(t.path), { credentials: 'same-origin' });
  if (!r.ok) throw new Error(`Error ${r.status}`);
  const box = $('.fm-docx', t.el);
  box.innerHTML = '';
  const root = box.attachShadow({ mode: 'open' }); // the document's own styles stay in it, never the panel's
  root.innerHTML = '<style>.docx-wrapper{background:transparent!important;padding:24px 0!important}.docx-wrapper>section.docx{box-shadow:0 8px 30px rgba(0,0,0,.6)!important;margin-bottom:18px!important}</style><div></div>';
  const body = root.lastChild;
  await docx.renderAsync(await r.blob(), body, body, { className: 'docx', inWrapper: true, breakPages: true, ignoreLastRenderedPageBreak: true, useBase64URL: true });
  for (const a of root.querySelectorAll('a[href]')) { a.target = '_blank'; a.rel = 'noopener noreferrer'; } // a link never takes the panel's tab
}

// PowerPoint (.pptx drawn from its XML) and the old binary .doc / .ppt (their text): office.js, on demand
async function fmOffice(t) {
  t.el.innerHTML = '<div class="fm-office"><div class="fm-load"><i class="spin"></i></div></div>';
  const x = extOf(t.name);
  await fmLib(x === 'pptx' ? 'jszip' : 'xlsx'); // JSZip opens a .pptx; SheetJS's CFB opens the old OLE files
  await fmLib('office');
  const r = await fetch(fmRaw(t.path), { credentials: 'same-origin' });
  if (!r.ok) throw new Error(`Error ${r.status}`);
  const buf = await r.arrayBuffer(), box = $('.fm-office', t.el);
  box.textContent = '';
  if (x === 'pptx') {
    const v = await OFFICE.pptx(buf, box);
    t.meta = `${v.slides} slide${v.slides === 1 ? '' : 's'}`;
    t.view = { destroy: v.destroy };
    return;
  }
  const add = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; };
  box.append(add('div', 'fm-onote', `Text only: the layout needs ${x === 'doc' ? 'Word' : 'PowerPoint'}. Download opens it there.`));
  if (x === 'doc') {
    const page = add('div', 'fm-docpage');
    const paras = OFFICE.doc(buf);
    for (const p of paras) page.append(add('p', '', p || '\u200b'));
    box.append(page);
    t.meta = `${paras.filter((p) => p.trim()).length} paragraphs`;
    return;
  }
  const slides = OFFICE.ppt(buf);
  slides.forEach((lines, i) => {
    const card = add('div', 'fm-pslide');
    card.append(add('span', 'fm-pn', String(i + 1)));
    const [first, ...rest] = lines.join('\n').split('\n').filter((l) => l.trim());
    card.append(add('h4', '', first || ''));
    for (const l of rest) card.append(add('p', '', l));
    box.append(card);
  });
  t.meta = `${slides.length} slide${slides.length === 1 ? '' : 's'}`;
}

// folders: tiles (with thumbnails), a list with size and date, or sizes (the disk-usage chart, moved here from Resources)
function fmFolder(t) {
  t.mode ||= FM.grid ? 'grid' : 'list';
  t.lead = `<div class="seg sm">${[['grid', 'Grid'], ['list', 'List'], ['sizes', 'Sizes']].map(([m, l]) => `<button type="button" data-fmode="${m}" aria-pressed="${t.mode === m}">${l}</button>`).join('')}</div>`;
  t.el.innerHTML = '<div class="fm-fv"></div><div class="fm-fvf"></div>';
  const box = $('.fm-fv', t.el);
  t.sort ||= ['name', 1];
  const paint = async () => {
    const d = await fmLoad(t.path);
    if (!t.el) return;
    const xs = d.entries.filter(fmShown);
    const nd = xs.filter((e) => e.dir).length;
    t.meta = d.err || `${nd} folder${nd === 1 ? '' : 's'} · ${xs.length - nd} file${xs.length - nd === 1 ? '' : 's'}`;
    fmBar(t);
    if (d.err) { box.innerHTML = `<div class="fm-card">${ic('alert-triangle')}<b>${esc(d.err)}</b></div>`; return; }
    if (t.mode === 'sizes') {
      box.className = 'fm-fv fm-m-sizes';
      box.replaceChildren(diskEl); // app.js: one explorer, moved to wherever it's shown
      if (DK.path !== t.path || !DK.data) loadDisk(t.path);
      return;
    }
    if (box.contains(diskEl)) diskEl.remove();
    box.className = 'fm-fv fm-m-' + t.mode;
    const [k, dir] = t.sort;
    const sorted = k === 'name' ? (dir > 0 ? xs : [...xs].reverse()) : [...xs].sort((a, b) => (b.dir - a.dir) || dir * ((a[k] || 0) - (b[k] || 0)));
    if (t.mode === 'grid') {
      const tile = (e) => `<div class="fm-tile${e.path === t.pick ? ' on' : ''}" data-p="${esc(e.path)}" data-dir="${e.dir ? 1 : ''}" draggable="true" title="${esc(e.name)}">` +
        `<div class="fm-thumb">${e.dir ? ic('folder', 'fm-fold') : FM_KIND[extOf(e.name)] === 'img' && e.size < 25e6 ? `<img loading="lazy" decoding="async" src="${fmPic(e, 180, 108, true)}" alt="">${fxBadge(e.name)}` : fxBadge(e.name).replace('class="fx', 'class="fx big')}</div>` +
        `<b>${esc(e.name)}${e.dir ? '/' : ''}</b><span>${e.dir ? (e.br ? esc(e.br) : 'folder') : fmtB(e.size)} · ${fmWhen(e.mtime)}</span></div>`;
      const fd = sorted.filter((e) => e.dir), fs = sorted.filter((e) => !e.dir);
      const cap = 600; // ponytail: tiles aren't virtual; past 600 the list (which is) shows the rest
      box.innerHTML = (fd.length ? `<h5>Folders · ${fd.length}</h5><div class="fm-tiles">${fd.slice(0, cap).map(tile).join('')}</div>` : '') +
        (fs.length ? `<h5>Files · ${fs.length}</h5><div class="fm-tiles">${fs.slice(0, cap).map(tile).join('')}</div>` : '') +
        (fd.length > cap || fs.length > cap ? '<p class="faint">Showing the first 600 of each: List shows them all.</p>' : '') +
        (xs.length ? '' : '<div class="fm-card"><b>Empty folder</b><span>Drop files here to upload them.</span></div>');
    } else {
      box.innerHTML = `<div class="fm-lh">${[['name', 'Name'], ['size', 'Size'], ['mtime', 'Modified']].map(([c, l]) => `<button type="button" data-sort="${c}" class="${c !== 'name' ? 'r' : ''}">${l}${k === c ? ic(dir > 0 ? 'angle-down' : 'angle-up') : ''}</button>`).join('')}</div><div class="fm-ll"></div>`;
      const ll = $('.fm-ll', box);
      fmVlist(ll, () => 34 * fmRem(), (rows, a, b) => rows.slice(a, b).map((e) => `<div class="fm-lr${e.path === t.pick ? ' on' : ''}" data-p="${esc(e.path)}" data-dir="${e.dir ? 1 : ''}" draggable="true">` +
        `<span class="fm-ln1">${fmIcon(e)}<span>${esc(e.name)}${e.dir ? '/' : ''}</span></span><span class="r">${e.dir ? '—' : fmtB(e.size)}</span><span class="r">${fmWhen(e.mtime)}</span></div>`).join('')).set(sorted);
    }
    $('.fm-fvf', t.el).innerHTML = `<b>${esc(t.pick ? fmBase(t.pick) : fmTilde(t.path))}</b><span class="grow"></span><span>Double-click opens · ⌘-click opens beside · Backspace goes up</span>`;
  };
  t.view = { paint, destroy: () => { if (t.el?.contains(diskEl)) diskEl.remove(); } };
  paint();
}

// ---- actions ----
function fmClick(e) {
  const tgt = e.target;
  const place = tgt.closest('[data-place]');
  if (place) { FM.ncwd = fmAbs(place.dataset.place); return fmSetRoot(place.dataset.place); }
  const go = tgt.closest('[data-go]');
  if (go) return fmNav(go.dataset.go);
  const act = tgt.closest('[data-act]')?.dataset.act;
  if (act) return fmAct(act, tgt.closest('[data-act]'));
  const tab = tgt.closest('[data-tab]'), cl = tgt.closest('[data-close]');
  if (cl) { const [gi, i] = cl.dataset.close.split(':').map(Number); return fmClose(FM.groups[gi].tabs[i]); }
  if (tab) { const [gi, i] = tab.dataset.tab.split(':').map(Number); FM.g = gi; FM.groups[gi].on = i; if (FM.narrow) FM.el.classList.add('viewing'); return fmGroups(); }
  const grp = tgt.closest('.fm-grp');
  if (grp && +grp.dataset.g !== FM.g) { FM.g = +grp.dataset.g; fmGroups(); }
  const t = fmActive();
  const fa = tgt.closest('[data-fa]')?.dataset.fa;
  if (fa && t) return fmFileAct(fa, t, tgt.closest('[data-fa]'));
  const mode = tgt.closest('[data-mode]');
  if (mode && t) return fmMode(t, mode.dataset.mode);
  const fmode = tgt.closest('[data-fmode]');
  if (fmode && t) { t.mode = fmode.dataset.fmode; FM.grid = t.mode === 'grid' ? true : t.mode === 'list' ? false : FM.grid; return fmFolder(t); }
  const sortB = tgt.closest('[data-sort]');
  if (sortB && t?.dir) { const k = sortB.dataset.sort; t.sort = [k, t.sort[0] === k ? -t.sort[1] : k === 'name' ? 1 : -1]; return t.view.paint(); }
  const menu = tgt.closest('[data-menu]');
  const row = tgt.closest('.fm-row[data-p]');
  if (row) {
    const p = row.dataset.p, dir = !!row.dataset.dir;
    if (menu) { FM.sel = p; fmSelPaint(); const r = menu.getBoundingClientRect(); return fmRowMenu([p], r.right, r.bottom); }
    if (e.shiftKey && FM.sel) { // a range, for delete / move / send
      const a = FM.rows.findIndex((x) => x.path === FM.sel), b = FM.rows.findIndex((x) => x.path === p);
      FM.multi = new Set(FM.rows.slice(Math.min(a, b), Math.max(a, b) + 1).filter((x) => x.path).map((x) => x.path));
      return fmSelPaint();
    }
    FM.multi.clear();
    FM.sel = p;
    // a double click, counted here: the first click redraws the row, so the browser's dblclick never comes
    const dbl = FM.click?.p === p && e.timeStamp - FM.click.t < 450;
    FM.click = dbl ? null : { p, t: e.timeStamp };
    if (dbl && !FM.narrow) return dir ? fmGo(p) : fmOpen(p, { pin: true });
    if (dir) { if (FM.narrow) return fmNav(p); return fmToggle(p); }
    fmSelPaint();
    return fmOpen(p, { side: e.metaKey || e.ctrlKey });
  }
  const tile = tgt.closest('.fm-tile[data-p], .fm-lr[data-p]');
  if (tile && t?.dir) {
    if (e.metaKey || e.ctrlKey) return fmOpen(tile.dataset.p, { dir: !!tile.dataset.dir, pin: true, side: true });
    if (FM.narrow || matchMedia('(pointer: coarse)').matches) return tile.dataset.dir ? fmNav(tile.dataset.p) : fmOpen(tile.dataset.p, { pin: true });
    t.pick = tile.dataset.p;
    for (const x of $$('.fm-tile.on, .fm-lr.on', t.el)) x.classList.remove('on');
    tile.classList.add('on');
    $('.fm-fvf b', t.el).textContent = fmBase(t.pick);
  }
}
function fmDbl(e) {
  if (e.target.closest('[data-menu], .fm-x, .fm-fbar, .fm-bar')) return;
  const tab = e.target.closest('[data-tab]');
  if (tab) { const [gi, i] = tab.dataset.tab.split(':').map(Number); FM.groups[gi].tabs[i].pre = false; return fmGroups(); }
  const it = e.target.closest('.fm-tile[data-p], .fm-lr[data-p]'); // tree rows count their own double clicks (fmClick)
  if (!it) return;
  getSelection()?.removeAllRanges();
  if (it.dataset.dir) return fmGo(it.dataset.p);
  fmOpen(it.dataset.p, { pin: true });
}
function fmCtx(e) {
  const it = e.target.closest('.fm-row[data-p], .fm-tile[data-p], .fm-lr[data-p]');
  if (!it) return;
  e.preventDefault();
  const p = it.dataset.p;
  const paths = FM.multi.has(p) ? [...FM.multi] : [p];
  if (it.classList.contains('fm-row')) { FM.sel = p; fmSelPaint(); }
  fmRowMenu(paths, e.clientX, e.clientY);
}
function fmLongPress(root) { // a phone: touch and hold a row for its actions
  let tm = 0;
  root.addEventListener('touchstart', (e) => {
    const it = e.target.closest('.fm-row[data-p], .fm-tile[data-p], .fm-lr[data-p]');
    if (!it) return;
    const t0 = e.touches[0];
    tm = setTimeout(() => { tm = 0; navigator.vibrate?.(10); fmRowMenu([it.dataset.p], t0.clientX, t0.clientY); }, 520);
  }, { passive: true });
  for (const ev of ['touchend', 'touchmove', 'touchcancel']) root.addEventListener(ev, () => { clearTimeout(tm); }, { passive: true });
}
// a selection change: the rows on screen swap a class (a repaint rebuilds them; this is 40× cheaper)
function fmSelPaint() {
  for (const r of FM.list.querySelectorAll('.fm-row[data-p]')) {
    const on = r.dataset.p === FM.sel || FM.multi.has(r.dataset.p);
    if (r.classList.contains('on') !== on) { r.classList.toggle('on', on); r.setAttribute('aria-selected', on); }
  }
}
const fmEntry = (p) => FM.dirs.get(fmDir(p))?.entries.find((x) => x.path === p);
function fmRowMenu(paths, x, y) {
  const one = paths.length === 1, p = paths[0], dir = one && !!fmEntry(p)?.dir;
  fmMenu(x, y, [
    one && { k: 'open', l: dir ? 'Open as a view' : 'Open', i: 'eye' },
    one && { k: 'side', l: 'Open to the side', i: 'sidebar-left2', kbd: '⌘ click' },
    one && dir && { k: 'newfile', l: 'New file here', i: 'file-text' },
    one && dir && { k: 'newdir', l: 'New folder here', i: 'folder' },
    one && dir && { k: 'uphere', l: 'Upload here…', i: 'upload2' },
    { hr: 1 },
    one && { k: 'rename', l: 'Rename…', i: 'pen', kbd: 'F2' },
    { k: 'move', l: 'Move to…', i: 'folder-open' },
    one && { k: 'dup', l: 'Duplicate', i: 'copy' },
    { k: 'copy', l: one ? 'Copy path' : 'Copy paths', i: 'copy' },
    one && !dir && { k: 'dl', l: 'Download', i: 'download2' },
    { k: 'send', l: 'Send to agent…', i: 'sparkle' },
    { hr: 1 },
    { k: 'del', l: one ? 'Delete' : `Delete ${paths.length} items`, i: 'trash2', danger: 1, kbd: 'Del' },
  ], (k) => fmDo(k, paths));
}
async function fmDo(k, paths) {
  const p = paths[0], e = fmEntry(p), dir = !!e?.dir;
  if (k === 'open') return dir ? (FM.narrow ? fmNav(p) : fmGo(p)) : fmOpen(p, { pin: true });
  if (k === 'side') return fmOpen(p, { dir, pin: true, side: true });
  if (k === 'newfile') return fmNewFile(p);
  if (k === 'newdir') return fmNewDir(p);
  if (k === 'uphere') { FM.upTo = p; return $('#fm-file', FM.el).click(); }
  if (k === 'copy') return fmCopy(paths.join('\n'));
  if (k === 'dl') return void (location.href = fmRaw(p, 1));
  if (k === 'send') return fmSend(paths);
  if (k === 'del') return fmDelete(paths);
  if (k === 'dup') { const r = await fmJSON('/api/fs/copy', { path: p }); if (r.status !== 200) return fmErr(r); await fmRefresh([fmDir(p)]); return fmReveal(r.path); }
  if (k === 'rename') {
    const name = await fmPrompt(`Rename ${fmBase(p)}`, fmBase(p), 'Rename');
    if (!name || name === fmBase(p)) return;
    const r = await fmJSON('/api/fs/rename', { path: p, name });
    if (r.status !== 200) return fmErr(r);
    const np = r.path || fmJoin(fmDir(p), name);
    for (const t of fmTabs()) if (t.path === p || t.path.startsWith(p + '/')) { t.path = np + t.path.slice(p.length); t.name = fmBase(t.path); }
    if (FM.open.delete(p)) FM.open.add(np);
    await fmRefresh([fmDir(p)]);
    fmGroups();
    return fmReveal(np);
  }
  if (k === 'move') {
    const to = await fmPrompt(paths.length === 1 ? `Move ${fmBase(p)} to` : `Move ${paths.length} items to`, fmTilde(fmDir(p)), 'Move', 'A folder path. ~ is your home.');
    if (to) return fmMove(paths, fmAbs(to.replace(/\/+$/, '')));
  }
}
async function fmMove(paths, to) {
  const r = await fmJSON('/api/fs/move', { paths, to });
  if (r.status !== 200) return fmErr(r);
  for (const s of r.skipped || []) toast(`${fmBase(s.path)}: ${s.error}`, true);
  const moved = r.moved || [];
  paths.forEach((p, i) => { const np = moved[i]; if (!np) return; for (const t of fmTabs()) if (t.path === p || t.path.startsWith(p + '/')) { t.path = np + t.path.slice(p.length); t.name = fmBase(t.path); } });
  await fmRefresh([...new Set(paths.map(fmDir)), to]);
  fmGroups();
  if (moved.length) toast(`Moved ${moved.length === 1 ? fmBase(moved[0]) : moved.length + ' items'} to ${fmTilde(to)}`);
}
async function fmDelete(paths) {
  const sizes = await Promise.all(paths.filter((p) => p.startsWith(FM.home + '/')).slice(0, 8).map((p) => fmJSON('/api/disk/check?path=' + encodeURIComponent(p)).catch(() => ({})))); // the size index covers ~
  const bytes = sizes.reduce((a, s) => a + (s.bytes || 0), 0);
  const users = sizes.flatMap((s) => s.users || []);
  const what = paths.length === 1 ? fmTilde(paths[0]) : `${paths.length} items: ${paths.slice(0, 4).map(fmBase).join(', ')}${paths.length > 4 ? '…' : ''}`;
  if (!(await fmAsk(`Delete ${paths.length === 1 ? fmBase(paths[0]) : paths.length + ' items'}?`,
    `${what}${bytes ? ` (${fmtB(bytes)})` : ''}\n\nThere is no bin, so this can't be undone.${users.length ? `\n\nIn use by ${[...new Set(users)].slice(0, 4).join(', ')}.` : ''}`, 'Delete', true, 'trash2'))) return;
  const r = await fmJSON('/api/fs/delete', { paths });
  if (r.status !== 200) return fmErr(r);
  for (const s of r.skipped || []) toast(`${fmBase(s.path)}: ${s.error}`, true);
  for (const t of fmTabs()) if (paths.some((p) => t.path === p || t.path.startsWith(p + '/'))) fmClose(t, true);
  FM.multi.clear();
  FM.sel = '';
  await fmRefresh([...new Set(paths.map(fmDir))]);
  toast(`Deleted ${(r.deleted || []).length === 1 ? fmBase(r.deleted[0]) : (r.deleted || []).length + ' items'}`);
}
async function fmNewFile(dir = FM.here) {
  const name = await fmPrompt('New file', 'untitled.txt', 'Create', `In ${fmTilde(dir)}`);
  if (!name) return;
  const p = fmJoin(dir, name);
  const r = await fetch(`/api/fs/file?path=${encodeURIComponent(p)}&mtime=0`, { method: 'PUT', credentials: 'same-origin', body: '' });
  if (r.status !== 200) return fmErr({ status: r.status, ...(await r.json().catch(() => ({}))) });
  await fmRefresh([dir]);
  fmReveal(p);
  fmOpen(p, { pin: true });
}
async function fmNewDir(dir = FM.here) {
  const name = await fmPrompt('New folder', 'untitled folder', 'Create', `In ${fmTilde(dir)}`);
  if (!name) return;
  const r = await fmJSON('/api/fs/mkdir', { path: fmJoin(dir, name) });
  if (r.status !== 200) return fmErr(r);
  await fmRefresh([dir]);
  fmReveal(fmJoin(dir, name));
}
function fmCopy(text) {
  navigator.clipboard?.writeText(text).then(() => toast('Copied ' + (text.includes('\n') ? 'paths' : fmTilde(text))), () => toast("Couldn't copy", true));
}
function fmFileAct(a, t, b) {
  if (a === 'save') return fmSaveTab(t);
  if (a === 'revert') return fmRevert(t);
  if (a === 'copy') return fmCopy(t.path);
  if (a === 'send') return fmSend([t.path]);
  if (a === 'fit') { for (const x of $$('[data-fa="fit"]', b.parentNode)) x.setAttribute('aria-pressed', String(x === b)); $('.fm-img', t.el)?.classList.toggle('fit', b.textContent === 'Fit'); const img = $('.fm-img img[data-full]', t.el); if (img && b.textContent !== 'Fit') { img.src = img.dataset.full; delete img.dataset.full; } t.lead = b.parentNode.outerHTML; return; }
  if (a === 'more') { const r = b.getBoundingClientRect(); return fmMenu(r.right, r.bottom, [{ k: 'reveal', l: 'Show in tree', i: 'folder-open' }, { k: 'rename', l: 'Rename…', i: 'pen' }, { k: 'move', l: 'Move to…', i: 'folder-open' }, { k: 'dup', l: 'Duplicate', i: 'copy' }, { k: 'side', l: 'Open to the side', i: 'sidebar-left2' }, { hr: 1 }, { k: 'del', l: 'Delete', i: 'trash2', danger: 1 }], (k) => (k === 'reveal' ? fmReveal(t.path) : fmDo(k, [t.path]))); }
}
async function fmAct(a, b) {
  if (a === 'back' || a === 'fwd') { const i = FM.hi + (a === 'back' ? -1 : 1); if (i < 0 || i >= FM.hist.length) return; FM.hi = i; return fmNav(FM.hist[i], false); }
  if (a === 'up') { const at = FM.here; if (at && at !== '/') fmNav(fmDir(at)); return; }
  if (a === 'search') return fmPalette();
  if (a === 'lock') return $('#logout').click();
  if (a === 'focus') { FM.focus = !FM.focus; fmLayout(); return fmGroups(); }
  if (a === 'collapse') { FM.open.clear(); fmTree(); return fmSave(); }
  if (a === 'unview') { FM.el.classList.remove('viewing'); return; }
  if (a === 'split') {
    if (FM.groups.length > 1) { FM.groups[0].tabs.push(...FM.groups[1].tabs.filter((t) => !FM.groups[0].tabs.some((x) => x.path === t.path))); FM.groups.length = 1; FM.g = 0; return fmGroups(); }
    const t = fmActive();
    if (!t) return;
    const g = fmGroup();
    if (g.tabs.length > 1) { g.tabs.splice(g.on, 1); g.on = Math.max(0, g.on - 1); FM.groups.push({ tabs: [t], on: 0 }); }
    else FM.groups.push({ tabs: [], on: 0 });
    FM.g = 1;
    return fmGroups();
  }
  const r = b.getBoundingClientRect();
  if (a === 'upload') { FM.upTo = FM.here; return $('#fm-file', FM.el).click(); }
  if (a === 'new') return fmMenu(r.left, r.bottom, [{ k: 'f', l: 'New file', i: 'file-text' }, { k: 'd', l: 'New folder', i: 'folder' }, { k: 'u', l: 'Upload a folder…', i: 'upload2' }, { hr: 1 }, { k: 'c', l: 'Clone a git repository…', i: 'hierarchy3' }],
    (k) => (k === 'f' ? fmNewFile() : k === 'd' ? fmNewDir() : k === 'c' ? fmClone() : (FM.upTo = FM.here, $('#fm-dirup', FM.el).click())), true);
  if (a === 'viewmenu') return fmMenu(r.right, r.bottom, [{ k: 'h', l: 'Show hidden files', i: FM.hidden ? 'check' : 'eye' }, { k: 'c', l: 'Collapse all folders', i: 'minimize' }, { k: 'r', l: 'Refresh', i: 'arrow-rotate' }],
    (k) => { if (k === 'h') { FM.hidden = !FM.hidden; fmTree(); for (const t of fmTabs()) if (t.dir && t.view) t.view.paint(); fmSave(); } else if (k === 'c') fmAct('collapse'); else { FM.dirs.clear(); fmRefresh(); } });
}

// ---- a menu at a point ----
function fmMenu(x, y, items, pick, left) {
  $('.fm-menu')?.remove();
  const m = document.createElement('div');
  m.className = 'fm-menu';
  m.setAttribute('role', 'menu');
  m.innerHTML = items.filter(Boolean).map((it) => (it.hr ? '<hr>' : `<button type="button" role="menuitem" data-mi="${it.k}"${it.danger ? ' class="danger"' : ''}>${ic(it.i)}${esc(it.l)}${it.kbd ? `<kbd>${esc(it.kbd)}</kbd>` : ''}</button>`)).join('').replace(/^<hr>|<hr>$/g, '').replace(/(<hr>)+/g, '<hr>');
  document.body.append(m);
  const w = m.offsetWidth, h = m.offsetHeight;
  m.style.left = Math.max(8, Math.min(left ? x : x - w, innerWidth - w - 8)) + 'px';
  m.style.top = (y + h + 8 > innerHeight ? Math.max(8, y - h - 4) : y + 4) + 'px';
  const close = () => { m.remove(); removeEventListener('pointerdown', out, true); removeEventListener('keydown', key, true); };
  const out = (e) => { if (!m.contains(e.target)) close(); };
  const key = (e) => { if (e.key === 'Escape') { e.stopPropagation(); close(); } };
  addEventListener('pointerdown', out, true);
  addEventListener('keydown', key, true);
  m.addEventListener('click', (e) => { const b = e.target.closest('[data-mi]'); if (b) { close(); pick(b.dataset.mi); } });
  m.querySelector('button')?.focus({ preventScroll: true });
}

// ---- dialogs: the panel's own modal look, one <dialog> reused ----
function fmDialog(html, setup) {
  const d = $('.fm-dlg', FM.el);
  d.innerHTML = `<form method="dialog" class="fm-df">${html}</form>`;
  return new Promise((done) => {
    let val = null;
    const form = $('form', d);
    form.addEventListener('submit', (e) => { e.preventDefault(); const v = setup?.submit?.(e); if (v === false) return; val = v ?? true; d.close(); });
    d.addEventListener('close', () => done(val), { once: true });
    $('[data-cancel]', d)?.addEventListener('click', () => d.close());
    setup?.init?.(d);
    d.showModal();
  });
}
function fmAsk(title, msg, ok, danger, icon = 'alert-triangle') {
  return fmDialog(`<div class="modal-h"><span class="modal-ic">${ic(icon)}</span><h2>${esc(title)}</h2></div><p class="muted fm-pre">${esc(msg)}</p>` +
    `<div class="modal-actions"><button type="button" class="btn" data-cancel>Cancel</button><button type="submit" class="btn ${danger ? 'danger' : 'primary'}" autofocus>${esc(ok)}</button></div>`).then((v) => !!v);
}
function fmPrompt(title, value, ok, hint = '') {
  return fmDialog(`<div class="modal-h"><h2>${esc(title)}</h2></div><input class="fm-in" type="text" autocomplete="off" spellcheck="false" value="${esc(value)}">${hint ? `<p class="faint fm-hint">${esc(hint)}</p>` : ''}` +
    `<div class="modal-actions"><button type="button" class="btn" data-cancel>Cancel</button><button type="submit" class="btn primary">${esc(ok)}</button></div>`, {
    init: (d) => { const i = $('.fm-in', d); setTimeout(() => { i.focus(); const dot = value.lastIndexOf('.'); i.setSelectionRange(0, dot > 0 ? dot : value.length); }, 0); },
    submit: () => { const v = $('.fm-dlg .fm-in', FM.el).value.trim(); return v || false; },
  });
}

// ---- send a file to J.A.R.V.I.S or an agent pane ----
async function fmSend(paths, body = '') { // body: a prepared message (a board's comments); the note goes above it
  const panes = (await allPanes()).filter((x) => x.p.agent);
  const items = [{ key: 'ai', label: 'J.A.R.V.I.S', sub: 'your assistant' }, ...panes.map((x) => ({ key: x.key, label: x.p.agent?.title || x.label, sub: `${x.session} · ${x.label}`, x }))];
  const name = paths.length === 1 ? fmBase(paths[0]) : `${paths.length} items`;
  let pick = 0;
  const ok = await fmDialog(`<div class="modal-h"><span class="modal-ic fm-ic-au">${icAu('sparkle')}</span><h2>Send ${body ? `the comments on ${esc(name)}` : esc(name)} to</h2></div>` +
    `<div class="fm-agents">${items.map((it, i) => `<button type="button" class="fm-ag${i === 0 ? ' on' : ''}" data-ag="${i}"><span class="fm-av">${ic(i ? 'terminal-square' : 'sparkle')}</span><span class="fm-agb"><b>${esc(it.label)}</b><span>${esc(it.sub)}</span></span></button>`).join('')}</div>` +
    `<textarea class="fm-in fm-note" rows="3" placeholder="Add a note (optional)"></textarea><p class="faint fm-hint">The agent gets the path and your note, and reads the file from disk.</p>` +
    `<div class="modal-actions"><button type="button" class="btn" data-cancel>Cancel</button><button type="submit" class="btn primary">${ic('arrow-right')}Send</button></div>`, {
    init: (d) => {
      $('.fm-agents', d).addEventListener('click', (e) => { const b = e.target.closest('[data-ag]'); if (!b) return; pick = +b.dataset.ag; for (const x of $$('.fm-ag', d)) x.classList.toggle('on', x === b); });
      $('.fm-note', d).addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) $('form', d).requestSubmit(); });
      setTimeout(() => $('.fm-note', d).focus(), 0);
    },
    submit: () => $('.fm-dlg .fm-note', FM.el).value.trim() || '',
  });
  if (ok === null) return false;
  const note = typeof ok === 'string' ? ok : '';
  const text = body ? (note ? note + '\n\n' : '') + body : `${note || `Have a look at ${paths.length === 1 ? 'this' : 'these'}:`}\n\n${paths.join('\n')}`;
  const it = items[pick];
  if (it.key === 'ai') {
    const r = await fmJSON('/api/ai/send', { text, images: [], files: [] });
    if (r.status !== 200) { fmErr(r); return false; }
  } else queueSend(it.key, it.x.session, it.x.p.id, text, []);
  toast(`Sent ${body ? 'the comments' : name} to ${it.label}`);
  return true;
}

// A design board opened in Files keeps its comments the way the session viewer does: {type:'cp-comments',
// op:'load'|'save'} → "<page>.comments.json" beside it; op 'send' hands them to the agent you pick (you confirm).
addEventListener('message', async (e) => {
  const m = e.data;
  if (!m || m.type !== 'cp-comments' || !FM.el) return;
  const t = fmTabs().find((x) => x.el && $('iframe', x.el)?.contentWindow === e.source);
  if (!t) return;
  const reply = (o) => e.source?.postMessage({ type: 'cp-comments', ...o }, '*');
  const store = t.path + '.comments.json';
  if (m.op === 'load') {
    const r = await fetch(fmRaw(store), { credentials: 'same-origin' }).catch(() => null);
    if (r?.status === 404) return reply({ op: 'loaded', ok: true, comments: [], file: store });
    const j = r?.ok ? await r.json().catch(() => null) : null;
    return reply({ op: 'loaded', ok: !!j, comments: j?.comments || [], file: store, error: j ? '' : "Couldn't read the comments" });
  }
  if (m.op === 'save') {
    const body = JSON.stringify({ page: t.path, saved_at: new Date().toISOString(), comments: Array.isArray(m.comments) ? m.comments : [] }, null, 2);
    if (body.length > 2 << 20) return reply({ op: 'saved', ok: false, error: 'Comments go up to 2 MB' });
    // nolink: the page names this file, so a link a cloned repo put there is replaced, never written through
    const r = await fetch(`/api/fs/file?path=${encodeURIComponent(store)}&force=1&nolink=1`, { method: 'PUT', credentials: 'same-origin', body }).catch(() => null);
    return reply({ op: 'saved', ok: r?.status === 200, file: store, error: r?.status === 200 ? '' : "Couldn't save the comments" });
  }
  if (m.op === 'send') {
    const text = String(m.text || '').trim().slice(0, 20000);
    if (!text) return reply({ op: 'sent', ok: false, error: 'nothing to send' });
    reply({ op: 'sent', ok: await fmSend([t.path], `${text}\n\n(Comments file: ${store})`) });
  }
});

// ---- upload: drop files or folders anywhere on the tree or a folder view, or the Upload button ----
async function fmUpload(files, dir, replace) {
  if (!files.length) return;
  if (!dir) return toast('Pick a folder to upload into first', true);
  const box = fmUpBox();
  const rows = files.map(([f, rel]) => {
    const r = document.createElement('div');
    r.className = 'fm-upr';
    r.innerHTML = `${fxBadge(rel)}<span class="fm-upn">${esc(rel)}</span><span class="fm-ups">0%</span><i class="fm-upb"><i></i></i>`;
    $('.fm-upl', box).append(r);
    return r;
  });
  const exists = [];
  let next = 0, done = 0, failed = 0;
  const one = (i) => new Promise((ok) => {
    const [f, rel] = files[i];
    const x = new XMLHttpRequest();
    const fd = new FormData();
    fd.append('file', f, rel);
    x.open('POST', `/api/fs/upload?dir=${encodeURIComponent(dir)}${replace ? '&replace=1' : ''}`);
    x.upload.onprogress = (e) => { if (e.lengthComputable) { const p = Math.round((e.loaded / e.total) * 100); $('.fm-ups', rows[i]).textContent = p + '%'; $('.fm-upb i', rows[i]).style.width = p + '%'; } };
    x.onload = () => {
      let j = {};
      try { j = JSON.parse(x.responseText); } catch { /* not JSON */ }
      const skipped = (j.exists || []).length > 0;
      if (skipped) exists.push(files[i]);
      const good = x.status === 200 && !skipped;
      if (!good && !skipped) failed++;
      $('.fm-ups', rows[i]).innerHTML = good ? ic('check') : skipped ? 'exists' : esc(j.error || 'failed');
      rows[i].classList.add(good ? 'ok' : 'bad');
      ok();
    };
    x.onerror = () => { failed++; $('.fm-ups', rows[i]).textContent = 'failed'; rows[i].classList.add('bad'); ok(); };
    x.send(fd);
  });
  const worker = async () => { while (next < files.length) { const i = next++; await one(i); done++; $('.fm-uph span', box).textContent = `${done} of ${files.length}`; } };
  $('.fm-uph b', box).textContent = `Uploading ${files.length} file${files.length === 1 ? '' : 's'}`;
  await Promise.all([worker(), worker(), worker()]); // three at a time
  await fmRefresh([dir]);
  fmWarmPics(files.map(([, rel]) => fmJoin(dir, rel)));
  fmUpDone(box);
  if (exists.length && await fmAsk(`Replace ${exists.length} file${exists.length === 1 ? '' : 's'}?`, `${exists.slice(0, 6).map(([, r]) => r).join(', ')}${exists.length > 6 ? '…' : ''} already ${exists.length === 1 ? 'exists' : 'exist'} in ${fmTilde(dir)}.`, 'Replace', true)) fmUpload(exists, dir, true);
  else if (!failed && !exists.length) toast(`Uploaded ${files.length === 1 ? files[0][1] : files.length + ' files'} to ${fmTilde(dir)}`);
}
// Photos just uploaded that are slow to decode (HEIC, AVIF: ~0.5 s each, the panel's first time): their tiles (and with
// them the size they open at) made now, in the background, so the folder and each photo show at once when opened
function fmWarmPics(paths) {
  for (const p of paths) {
    const e = /\.(hei[cf]|avif)$/i.test(p) && fmEntry(p);
    if (e) fetch(fmPic(e, 180, 108, true), { credentials: 'same-origin', priority: 'low' }).catch(() => {});
  }
}
function fmUpBox() {
  let b = $('.fm-up');
  if (!b) {
    b = document.createElement('div');
    b.className = 'fm-up';
    b.innerHTML = `<div class="fm-uph">${ic('upload2')}<b></b><span></span><button type="button" class="icon-btn ghost sm" aria-label="Dismiss">${ic('xmark')}</button></div><div class="fm-upl"></div>`;
    $('button', b).addEventListener('click', () => b.remove());
    document.body.append(b);
  }
  clearTimeout(b.tm);
  return b;
}
function fmUpDone(b) { b.tm = setTimeout(() => b.remove(), 6000); }
function fmDropMark(el) {
  for (const x of $$('.fm-dropon', FM.el)) if (x !== el) x.classList.remove('fm-dropon');
  el?.classList.add('fm-dropon');
}
function fmDropTarget(e) { // where a drop lands: a folder row/tile, a file's folder, the tree's root, the open folder, the right edge (a tab)
  if (e.dataTransfer.types.includes('application/x-fm-tab')) return { el: e.target.closest('.fm-grp') || $('.fm-groups', FM.el), tab: true };
  const it = e.target.closest('.fm-row[data-p], .fm-tile[data-p], .fm-lr[data-p]');
  if (it) return { el: it.dataset.dir ? it : e.target.closest('.fm-tree, .fm-fv') || it, dir: it.dataset.dir ? it.dataset.p : fmDir(it.dataset.p) };
  if (e.target.closest('.fm-tree')) return { el: $('.fm-tree', FM.el), dir: fmAbs(FM.narrow ? FM.ncwd || FM.root : FM.root) };
  const fv = e.target.closest('.fm-pane')?.querySelector('.fm-fv');
  const t = fmActive();
  if (fv && t?.dir) return { el: fv, dir: t.path };
  return null;
}
function fmDragStart(e) {
  const tab = e.target.closest('[data-tab]');
  if (tab) { e.dataTransfer.setData('application/x-fm-tab', tab.dataset.tab); e.dataTransfer.effectAllowed = 'move'; return; }
  const it = e.target.closest('[data-p]');
  if (!it) return;
  const p = it.dataset.p;
  const paths = FM.multi.has(p) ? [...FM.multi] : [p];
  e.dataTransfer.setData('application/x-fm-paths', JSON.stringify(paths));
  e.dataTransfer.setData('text/plain', paths.join('\n'));
  e.dataTransfer.effectAllowed = 'copyMove';
}
function fmDragOver(e) {
  const t = fmDropTarget(e);
  if (!t) { if (e.dataTransfer.types.includes('Files')) { e.preventDefault(); e.dataTransfer.dropEffect = 'none'; } return fmDropMark(null); }
  e.preventDefault();
  e.dataTransfer.dropEffect = e.dataTransfer.types.includes('Files') ? 'copy' : 'move';
  fmDropMark(t.el);
}
async function fmDrop(e) {
  const t = fmDropTarget(e);
  fmDropMark(null);
  e.preventDefault();
  if (!t) return;
  const dt = e.dataTransfer;
  if (t.tab) { // a tab dropped on the other side (or the right edge): it moves there
    const [gi, i] = dt.getData('application/x-fm-tab').split(':').map(Number);
    const tab = FM.groups[gi]?.tabs[i];
    if (!tab) return;
    const box = $('.fm-groups', FM.el).getBoundingClientRect();
    let to = t.el.classList.contains('fm-grp') ? +t.el.dataset.g : 0;
    if (FM.groups.length === 1 && e.clientX > box.left + box.width * 0.7) { FM.groups.push({ tabs: [], on: 0 }); to = 1; }
    if (to === gi && FM.groups.length > 1) return;
    FM.groups[gi].tabs.splice(i, 1);
    FM.groups[gi].on = Math.max(0, Math.min(FM.groups[gi].on, FM.groups[gi].tabs.length - 1));
    FM.groups[to].tabs.push(tab);
    FM.groups[to].on = FM.groups[to].tabs.length - 1;
    FM.groups = FM.groups.filter((g) => g.tabs.length);
    FM.g = Math.min(to, FM.groups.length - 1);
    return fmGroups();
  }
  const moving = dt.getData('application/x-fm-paths');
  if (moving) {
    const paths = JSON.parse(moving).filter((p) => p !== t.dir && fmDir(p) !== t.dir && !t.dir.startsWith(p + '/'));
    if (paths.length) fmMove(paths, t.dir);
    return;
  }
  const files = [];
  const walk = async (entry, pre) => { // a dropped folder: every file in it, with its path inside
    if (entry.isFile) { await new Promise((ok) => entry.file((f) => { files.push([f, pre + f.name]); ok(); }, ok)); return; }
    const rd = entry.createReader();
    for (;;) {
      const batch = await new Promise((ok) => rd.readEntries(ok, () => ok([])));
      if (!batch.length) break;
      for (const x of batch) await walk(x, pre + entry.name + '/');
    }
  };
  const entries = [...dt.items].map((i) => i.webkitGetAsEntry?.()).filter(Boolean);
  if (entries.some((x) => x.isDirectory)) for (const x of entries) await walk(x, '');
  else for (const f of dt.files) files.push([f, f.name]);
  fmUpload(files, t.dir);
}

// ---- clone a git repository ----
async function fmClone() {
  const into = FM.here === FM.home || !FM.here ? FM.home + '/repos' : FM.here;
  if (!FM.repos) FM.repos = fmJSON('/api/fs/repos').catch(() => ({}));
  let depth = 0;
  const v = await fmDialog(`<div class="modal-h"><span class="modal-ic fm-ic-git">${ic('hierarchy3')}</span><h2>Clone a repository</h2></div>` +
    '<label class="fm-fld"><span>Repository</span><input class="fm-in" name="url" type="text" autocomplete="off" spellcheck="false" placeholder="owner/name, https://… or git@…" required></label>' +
    '<div class="fm-repos"><div class="fm-load"><i class="spin"></i></div></div>' +
    `<div class="fm-frow"><label class="fm-fld"><span>Into</span><input class="fm-in mono" name="into" type="text" value="${esc(fmTilde(into))}" spellcheck="false"></label>` +
    '<label class="fm-fld fm-nm"><span>Folder name</span><input class="fm-in mono" name="name" type="text" spellcheck="false" placeholder="from the repo"></label>' +
    '<label class="fm-fld fm-nm"><span>Branch</span><input class="fm-in mono" name="branch" type="text" spellcheck="false" placeholder="default"></label></div>' +
    '<div class="fm-frow2"><span>History</span><div class="seg sm"><button type="button" data-depth="0" aria-pressed="true">Full</button><button type="button" data-depth="1" aria-pressed="false">Latest commit only</button></div></div>' +
    `<p class="faint fm-hint">${ic('key')}Private repos use the box's own GitHub login (gh). SSH addresses use the keys in ~/.ssh.</p>` +
    `<div class="modal-actions"><button type="button" class="btn" data-cancel>Cancel</button><button type="submit" class="btn primary">${ic('arrow-down')}Clone</button></div>`, {
    init: async (d) => {
      const url = $('[name=url]', d), list = $('.fm-repos', d);
      setTimeout(() => url.focus(), 0);
      $('.fm-frow2', d).addEventListener('click', (e) => { const b = e.target.closest('[data-depth]'); if (!b) return; depth = +b.dataset.depth; for (const x of $$('[data-depth]', d)) x.setAttribute('aria-pressed', String(x === b)); });
      const r = await FM.repos;
      const repos = r.repos || [];
      const paint = () => {
        const q = url.value.toLowerCase().trim();
        const hits = repos.filter((x) => !q || x.name.toLowerCase().includes(q)).slice(0, 6);
        list.innerHTML = r.error && !repos.length ? `<p class="faint">${esc(r.error)}</p>` : hits.length ? `<div class="grp">Your GitHub repositories</div>${hits.map((x) => `<button type="button" class="fm-ag${x.name.toLowerCase() === q ? ' on' : ''}" data-repo="${esc(x.name)}"><span class="fm-av">${ic('hierarchy3')}</span><span class="fm-agb"><b>${esc(x.name)}</b><span>${[x.private ? 'private' : 'public', x.lang, x.updated && 'updated ' + ago(Date.parse(x.updated) / 1000)].filter(Boolean).map(esc).join(' · ')}</span></span>${x.here ? `<span class="fm-here" title="${esc(x.here)}">in ${esc(fmTilde(fmDir(x.here)))}</span>` : ''}</button>`).join('')}` : '';
      };
      paint();
      url.addEventListener('input', paint);
      list.addEventListener('click', (e) => { const b = e.target.closest('[data-repo]'); if (b) { url.value = b.dataset.repo; paint(); url.focus(); } });
    },
    submit: () => { const d = $('.fm-dlg', FM.el); const f = (n) => $(`[name=${n}]`, d).value.trim(); return { url: f('url'), dir: fmAbs(f('into') || '~/repos'), name: f('name'), branch: f('branch'), depth }; },
  });
  if (!v || v === true) return;
  const box = fmUpBox();
  const row = document.createElement('div');
  row.className = 'fm-upr';
  row.innerHTML = `${ic('hierarchy3')}<span class="fm-upn">${esc(v.url)}</span><span class="fm-ups"><i class="spin"></i></span><span class="fm-upp"></span>`;
  $('.fm-uph b', box).textContent = 'Cloning';
  $('.fm-upl', box).append(row);
  let last = {};
  try {
    const r = await fetch('/api/fs/clone', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(v) });
    if (!r.ok) { last = await r.json().catch(() => ({ error: `Error ${r.status}` })); }
    else {
      const rd = r.body.getReader(), dec = new TextDecoder();
      let buf = '';
      for (;;) {
        const { value, done } = await rd.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        const lines = buf.split('\n');
        buf = lines.pop();
        for (const l of lines) { if (!l.trim()) continue; try { last = JSON.parse(l); } catch { continue; } if (last.p) $('.fm-upp', row).textContent = last.p; }
      }
    }
  } catch (err) { last = { error: String(err.message || err) }; }
  $('.fm-ups', row).innerHTML = last.ok ? ic('check') : 'failed';
  $('.fm-upp', row).textContent = last.ok ? fmTilde(last.path) : last.error || 'failed';
  row.classList.add(last.ok ? 'ok' : 'bad');
  fmUpDone(box);
  if (!last.ok) return toast(last.error || 'Clone failed', true);
  await fmRefresh([v.dir]);
  fmGo(last.path);
}

// ---- search every file (/ or ⌘K) ----
const FM_KINDS = [['', 'All'], ['docs', 'Documents'], ['sheets', 'Sheets'], ['code', 'Code'], ['images', 'Images'], ['media', 'Media'], ['folders', 'Folders']];
function fmPalette() {
  if (S.view !== 'files') setView('files');
  let P = FM.pal;
  if (!P) {
    const d = document.createElement('dialog');
    d.className = 'fm-pal';
    d.innerHTML = `<div class="fm-pin">${ic('search')}<input type="text" autocomplete="off" spellcheck="false" placeholder="Search every file on the box" aria-label="Search files"><span class="fm-pt"></span></div>` +
      `<div class="fm-pf"><div class="seg sm"><button type="button" data-pm="names" aria-pressed="true">Names</button><button type="button" data-pm="text" aria-pressed="false">Contents</button></div><span class="vsep"></span>` +
      `${FM_KINDS.map(([k, l]) => `<button type="button" class="chip" data-pk="${k}" aria-pressed="${!k}">${l}</button>`).join('')}<span class="grow"></span>` +
      `<button type="button" class="chip" data-ph aria-pressed="true">${ic('eye')}Hidden</button><button type="button" class="chip" data-ps aria-pressed="false" title="Only under the folder you're in">This folder</button></div>` +
      '<div class="fm-pb"><div class="fm-hits" role="listbox"></div><div class="fm-qp"></div></div>' +
      `<div class="fm-pk"><span>${kbd('↑', '↓')} move</span><span>${kbd('↵')} open</span><span>${kbd('⌘', '↵')} open beside</span><span>${kbd('⌥', '↵')} show in tree</span><span>${kbd('⌘', 'C')} copy path</span><span class="grow"></span><span class="fm-pidx"></span></div>`;
    document.body.append(d);
    P = FM.pal = { d, input: $('input', d), mode: 'names', kind: '', hidden: true, scope: false, items: [], sel: 0, ac: null, cache: new Map() };
    P.input.addEventListener('input', () => fmPalQuery());
    P.input.addEventListener('keydown', fmPalKey);
    d.addEventListener('click', (e) => {
      if (e.target === d) return d.close();
      const b = e.target.closest('[data-pm], [data-pk], [data-ph], [data-ps], [data-hit]');
      if (!b) return;
      if (b.dataset.hit !== undefined) { P.sel = +b.dataset.hit; return fmPalGo(e.metaKey || e.ctrlKey ? 'side' : e.altKey ? 'reveal' : 'open'); }
      if (b.dataset.pm) {
        P.mode = b.dataset.pm;
        for (const x of $$('[data-pm]', d)) x.setAttribute('aria-pressed', String(x === b));
        const sc = $('[data-ps]', d); // contents always search the folder you're in
        sc.disabled = P.mode === 'text';
        sc.setAttribute('aria-pressed', String(P.scope || P.mode === 'text'));
        P.items = [];
        fmPalPaint();
      }
      if (b.dataset.pk !== undefined) { P.kind = b.dataset.pk; for (const x of $$('[data-pk]', d)) x.setAttribute('aria-pressed', String(x === b)); }
      if (b.hasAttribute('data-ph')) { P.hidden = !P.hidden; b.setAttribute('aria-pressed', String(P.hidden)); }
      if (b.hasAttribute('data-ps')) { P.scope = !P.scope; b.setAttribute('aria-pressed', String(P.scope)); }
      P.input.focus();
      fmPalQuery();
    });
    d.addEventListener('mousemove', (e) => { const h = e.target.closest('[data-hit]'); if (h && +h.dataset.hit !== P.sel) { P.sel = +h.dataset.hit; fmPalPaint(); } });
  }
  P.d.showModal();
  P.input.select();
  fmPalQuery();
}
async function fmPalQuery() {
  const P = FM.pal, q = P.input.value.trim();
  P.ac?.abort();
  clearTimeout(P.tm);
  if (!q) { // nothing typed: what you opened lately
    P.items = FM.recent.map((p) => ({ path: p, name: fmBase(p) }));
    P.sel = 0;
    $('.fm-pt', P.d).textContent = P.items.length ? 'Recent' : '';
    return fmPalPaint();
  }
  const ac = (P.ac = new AbortController());
  const go = async () => {
    const t0 = performance.now();
    const qs = new URLSearchParams({ q, hidden: P.hidden ? '1' : '0' });
    if (P.kind && P.mode === 'names') qs.set('kind', P.kind);
    if ((P.scope || P.mode === 'text') && FM.here) qs.set('in', FM.here); // contents: the folder you're in (all of ~ would take seconds)
    let r;
    try { r = await (await fetch(`/api/fs/${P.mode === 'names' ? 'search' : 'grep'}?${qs}`, { credentials: 'same-origin', signal: ac.signal })).json(); } catch { return; }
    if (ac.signal.aborted) return;
    if (r.error) { P.items = []; $('.fm-pt', P.d).textContent = r.error; return fmPalPaint(); }
    P.items = r.items || [];
    P.sel = 0;
    P.q = q;
    const ms = Math.round(performance.now() - t0);
    $('.fm-pt', P.d).innerHTML = P.mode === 'names'
      ? `${(r.total ?? P.items.length).toLocaleString()} match${r.total === 1 ? '' : 'es'} · <b>${ms} ms</b>`
      : `${P.items.length}${r.truncated ? '+' : ''} line${P.items.length === 1 ? '' : 's'} · <b>${ms} ms</b>`;
    $('.fm-pidx', P.d).textContent = r.indexed ? `${r.indexed.toLocaleString()} files indexed${r.building ? ' · updating' : ''}` : '';
    fmPalPaint();
  };
  if (P.mode === 'names') go();
  else { $('.fm-pt', P.d).innerHTML = `<i class="spin"></i> in ${esc(fmTilde(FM.here || '~'))}`; P.tm = setTimeout(go, 220); } // ripgrep is heavier: wait for a pause
}
function fmPalPaint() {
  const P = FM.pal;
  const words = (P.q || '').toLowerCase().split(/\s+/).filter(Boolean).map((w) => w.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'));
  const re = words.length ? new RegExp(`(${words.join('|')})`, 'gi') : null;
  const mark = (s) => (re ? String(s).split(re).map((part, i) => (i % 2 ? `<mark>${esc(part)}</mark>` : esc(part))).join('') : esc(s)); // split on the raw text, then escape each piece
  $('.fm-hits', P.d).innerHTML = P.items.map((x, i) => {
    const hid = /(^|\/)\./.test(fmTilde(x.path).replace(/^~/, ''));
    return `<div class="fm-hit${i === P.sel ? ' on' : ''}${hid ? ' hid' : ''}" role="option" data-hit="${i}" aria-selected="${i === P.sel}">` +
      `${x.dir ? ic('folder', 'fm-fold') : fxBadge(x.name || fmBase(x.path))}<div class="fm-hb"><b>${mark(x.name || fmBase(x.path))}${x.line ? `<em>:${x.line}</em>` : ''}</b>` +
      `<span>${x.text !== undefined ? mark(x.text.trim().slice(0, 200)) : esc(fmTilde(fmDir(x.path)))}</span></div>` +
      `<span class="fm-hm">${x.text !== undefined ? esc(fmTilde(fmDir(x.path))) : x.dir ? 'folder' : [x.size !== undefined && fmtB(x.size), fmWhen(x.mtime)].filter(Boolean).join(' · ')}</span></div>`;
  }).join('') || `<div class="fm-none">${P.input.value.trim() ? 'Nothing matches.' : 'Type a name: any part of it, in any folder, hidden ones too.'}</div>`;
  $('.fm-hit.on', P.d)?.scrollIntoView({ block: 'nearest' });
  clearTimeout(P.qt);
  P.qt = setTimeout(fmPalPreview, 60);
}
async function fmPalPreview() { // a quick look at the highlighted result
  const P = FM.pal, x = P.items[P.sel], box = $('.fm-qp', P.d);
  if (!x) { box.innerHTML = ''; return; }
  const name = x.name || fmBase(x.path), k = x.dir ? 'dir' : fmKind(name);
  const head = `<div class="fm-qph">${x.dir ? ic('folder', 'fm-fold') : fxBadge(name)}<b>${esc(name)}</b></div>`;
  if (k === 'img') { box.innerHTML = head + `<div class="fm-qpb fm-qimg"><img src="${x.size === undefined ? fmRaw(x.path) : fmPic(x, 480, 480)}" alt=""></div>`; return; }
  const key = x.path;
  let body = P.cache.get(key);
  if (!body) {
    if (k === 'dir') {
      const d = await fmLoad(x.path);
      body = `<div class="fm-qpb fm-qdir">${d.entries.slice(0, 40).map((e) => `<div>${fmIcon(e)}<span>${esc(e.name)}</span></div>`).join('') || '<span class="faint">Empty folder</span>'}${d.entries.length > 40 ? `<div class="faint">+ ${d.entries.length - 40} more</div>` : ''}</div>`;
    } else if (['text', 'md', 'csv', 'html'].includes(k)) {
      const r = await fetch(fmRaw(x.path) + '&head=16384', { credentials: 'same-origin' }).catch(() => null);
      const buf = r?.status === 200 ? new Uint8Array(await r.arrayBuffer()) : null;
      body = !buf ? '<div class="fm-qpb faint">Can\'t read it</div>' : buf.includes(0) ? '<div class="fm-qpb faint">Binary file</div>'
        : `<pre class="fm-qpb fm-qcode">${hl(new TextDecoder().decode(buf).split('\n').slice(0, 60).join('\n'), extOf(name)).map((l, i) => `<span class="ln">${i + 1}</span>${l}`).join('\n')}</pre>`;
    } else body = `<div class="fm-qpb fm-qcard">${fxBadge(name).replace('class="fx', 'class="fx big')}<span>${esc(x.size !== undefined ? fmtB(x.size) : '')}</span><span class="faint">${ic('arrow-right')}Enter opens it</span></div>`;
    P.cache.set(key, body);
    if (P.cache.size > 60) P.cache.delete(P.cache.keys().next().value);
  }
  if (P.items[P.sel] === x) box.innerHTML = head + body;
}
function fmPalKey(e) {
  const P = FM.pal;
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { e.preventDefault(); P.sel = Math.max(0, Math.min(P.items.length - 1, P.sel + (e.key === 'ArrowDown' ? 1 : -1))); return fmPalPaint(); }
  if (e.key === 'Enter') { e.preventDefault(); return fmPalGo(e.metaKey || e.ctrlKey ? 'side' : e.altKey ? 'reveal' : 'open'); }
  if (e.key === 'c' && (e.metaKey || e.ctrlKey) && P.input.selectionStart === P.input.selectionEnd && P.items[P.sel]) { e.preventDefault(); fmCopy(P.items[P.sel].path); }
}
function fmPalGo(how) {
  const P = FM.pal, x = P.items[P.sel];
  if (!x) return;
  P.d.close();
  if (how === 'reveal') { if (FM.focus) fmAct('focus'); return fmReveal(x.path); }
  const dir = !!x.dir || FM.dirs.get(x.path) !== undefined;
  if (dir) return fmGo(x.path);
  fmOpen(x.path, { pin: true, side: how === 'side' });
  if (x.line) setTimeout(() => fmGoLine(x.path, x.line), 400);
}
async function fmGoLine(path, line) { // a Contents hit: the editor, at that line
  for (let i = 0; i < 20; i++) {
    const t = fmTabs().find((x) => x.path === path);
    if (t?.ed) { const l = t.ed.view.state.doc.line(Math.min(line, t.ed.view.state.doc.lines)); t.ed.view.dispatch({ selection: { anchor: l.from }, scrollIntoView: true }); t.ed.view.focus(); return; }
    if (t?.mode && t.mode !== 'src' && t.kind) { await fmMode(t, 'src'); continue; }
    await new Promise((ok) => setTimeout(ok, 150));
  }
}

// ---- keys: plain ones while you're not typing (the panel's rule), plus ⌘K / Ctrl+K and ⌘S ----
function fmTreeKey(e) {
  if (!e.target.closest('.fm-list') || typingIn(e.target)) return;
  const i = FM.rows.findIndex((x) => x.path === FM.sel), cur = FM.rows[i];
  const move = (j) => { const x = FM.rows[Math.max(0, Math.min(FM.rows.length - 1, j))]; if (x?.path) { FM.sel = x.path; FM.multi.clear(); fmSelPaint(); const rh = fmRH(), top = FM.rows.indexOf(x) * rh; if (top < FM.list.scrollTop) FM.list.scrollTop = top; else if (top + rh > FM.list.scrollTop + FM.list.clientHeight) FM.list.scrollTop = top + rh - FM.list.clientHeight; } };
  const k = e.key;
  if (k === 'ArrowDown' || k === 'ArrowUp') move(i < 0 ? 0 : i + (k === 'ArrowDown' ? 1 : -1));
  else if (k === 'ArrowRight' && cur?.dir) { if (!FM.open.has(cur.path)) fmToggle(cur.path, true); else move(i + 1); }
  else if (k === 'ArrowLeft' && cur) { if (cur.dir && FM.open.has(cur.path)) fmToggle(cur.path, false); else { const up = FM.rows.slice(0, i).reverse().find((x) => x.depth < cur.depth); if (up) move(FM.rows.indexOf(up)); } }
  else if (k === 'Enter' && cur) { if (cur.dir) fmGo(cur.path); else fmOpen(cur.path, { pin: true, side: e.metaKey || e.ctrlKey }); }
  else if (k === ' ' && cur && !cur.dir) fmOpen(cur.path);
  else if (k === 'F2' && cur) fmDo('rename', [cur.path]);
  else if ((k === 'Delete' || (k === 'Backspace' && (e.metaKey || e.ctrlKey))) && cur) fmDelete(FM.multi.size ? [...FM.multi] : [cur.path]);
  else if (k === 'c' && (e.metaKey || e.ctrlKey) && cur && !getSelection()?.toString()) fmCopy(FM.multi.size ? [...FM.multi].join('\n') : cur.path);
  else return;
  e.preventDefault();
  e.stopPropagation();
}
addEventListener('keydown', (e) => {
  if ($('#app')?.hidden) return;
  const mod = e.metaKey || e.ctrlKey;
  if (mod && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'k' && (S.view === 'files' || !typingIn(e.target))) { e.preventDefault(); e.stopPropagation(); return fmPalette(); }
  if (S.view !== 'files' || !FM.el) return;
  if (mod && e.key.toLowerCase() === 's') { e.preventDefault(); return fmSaveTab(fmActive()); } // not the browser's "save page"
  if (e.key === 'Escape' && FM.el.classList.contains('viewing') && !$('dialog[open]')) { FM.el.classList.remove('viewing'); return; }
  if (!KS.on || mod || e.altKey || e.isComposing || typingIn(e.target) || e.target.closest?.('.cm-editor, .fm-grid') || $('dialog[open]')) return; // KS.on: the single-key switch in ?
  if (e.key === '/') { e.preventDefault(); e.stopPropagation(); return fmPalette(); }
  if (e.key === 'f' || e.key === 'F') { e.preventDefault(); e.stopPropagation(); return fmAct('focus'); }
  if (e.key === 'Backspace' && !e.target.closest('.fm-list')) { e.preventDefault(); return fmAct('up'); }
}, true);
addEventListener('beforeunload', (e) => { if (fmTabs().some((t) => t.dirty)) e.preventDefault(); });

// a phone: one column, the list drills into folders and a file opens over it
const fmNarrowQ = matchMedia('(max-width: 899px)');
const fmNarrow = () => { FM.narrow = fmNarrowQ.matches; FM.ncwd ||= fmAbs(FM.root); if (FM.el) { FM.el.classList.toggle('narrow', FM.narrow); fmTree(); fmCrumbs(); } };
fmNarrowQ.addEventListener('change', fmNarrow);
FM.narrow = fmNarrowQ.matches;
