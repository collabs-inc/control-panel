'use strict';

// ---------- Browser: a Chromium on the VM, in the page (browser.go) ----------
// Your windows: the panel's own Chromium (logins kept), streamed as JPEG frames over one websocket, with this page's
// mouse, wheel, touch and keys sent back. Agents: the agents' cube-browser sessions, watched live with their command
// timeline; Take over lets you drive one. Around the page: a start page with what runs on localhost and the projects
// you can run, viewports, a Problems drawer (console, exceptions, failed requests) and Annotate: draw, box and pin
// comments on a still of the page, then send it, with the elements under the pins, to an agent.
// The view is built once and owns its DOM (data-own): the 2-second stream never redraws it.

const BR_DEVICES = [
  { name: 'iPhone SE', w: 375, h: 667, dpr: 2, mobile: true, icon: 'mobile' },
  { name: 'iPhone 15', w: 393, h: 852, dpr: 3, mobile: true, icon: 'mobile' },
  { name: 'Pixel 8', w: 412, h: 915, dpr: 2.625, mobile: true, icon: 'mobile' },
  { name: 'iPad mini', w: 768, h: 1024, dpr: 2, mobile: true, icon: 'tablet' },
  { name: 'iPad Pro', w: 1024, h: 1366, dpr: 2, mobile: true, icon: 'tablet' },
  { name: 'Laptop', w: 1280, h: 800, dpr: 2, mobile: false, icon: 'monitor' },
  { name: 'Desktop', w: 1440, h: 900, dpr: 1, mobile: false, icon: 'monitor' },
  { name: '1080p', w: 1920, h: 1080, dpr: 1, mobile: false, icon: 'monitor' },
];
const BR_UA = {
  ios: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Mobile/15E148 Safari/604.1',
  ipad: 'Mozilla/5.0 (iPad; CPU OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Mobile/15E148 Safari/604.1',
  android: 'Mozilla/5.0 (Linux; Android 15; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Mobile Safari/537.36',
};
const BR_NETS = [['', 'No throttling'], ['fast4g', 'Fast 4G'], ['slow4g', 'Slow 4G'], ['3g', '3G'], ['offline', 'Offline']];
const BR_INK = ['#3fe9ae', '#f85149', '#e3b341', '#5c9dff', '#ffffff'];
const BR_VERB = { open: 'window', goto: 'globe', click: 'pointer', fill: 'text', press: 'keyboard', snapshot: 'eye', screenshot: 'camera', 'run-code': 'code',
  resize: 'monitor', console: 'terminal-square', requests: 'signal', close: 'xmark' };

const BR = {
  trailsTick: 0,
  st: (() => { try { return JSON.parse(store.get('br', '{}')) || {}; } catch { return {}; } })(),
  el: null, ws: null, open: false, retry: 0, tabs: [], agents: [], downloads: [], starting: true, error: '',
  watching: '', drawn: 0, decoding: false, next: null, meta: null, bmp: null,
  dev: { tab: '', cons: [], probs: [], nets: [], evals: [] }, steps: [], stepsFor: '',
  ports: null, portsAt: 0, projects: null, thumbs: new Map(), probes: new Map(), probeSeq: 0,
  ann: null, buttons: 0, wheel: null, move: null, lastSize: '', narrow: false, seenDl: new Set(),
};
BR.mode = BR.st.mode === 'agents' ? 'agents' : 'windows';
BR.tab = BR.st.tab || '';
BR.agent = BR.st.agent || '';
BR.devOpen = !!BR.st.devOpen;
BR.devPane = BR.st.devPane || 'probs';
BR.devH = BR.st.devH || 0;
BR.recent = Array.isArray(BR.st.recent) ? BR.st.recent : [];
BR.wall = !!BR.st.wall;
BR.dtOpen = !!BR.st.dtOpen;
BR.dtSide = BR.st.dtSide === 'bottom' ? 'bottom' : 'right';
BR.dtW = BR.st.dtW || 0;
BR.dtH = BR.st.dtH || 0;
const brSave = () => store.set('br', JSON.stringify({ mode: BR.mode, tab: BR.tab, agent: BR.agent, devOpen: BR.devOpen, devPane: BR.devPane, devH: BR.devH,
  recent: BR.recent.slice(0, 40), wall: BR.wall, dtOpen: BR.dtOpen, dtSide: BR.dtSide, dtW: BR.dtW, dtH: BR.dtH }));

VIEWS.browser = () => '<div id="browser" class="br" data-own></div>';
S.data.browser = {}; // the view draws its own content: render() only needs it present

const brRem = () => parseFloat(getComputedStyle(document.documentElement).fontSize) / 16 || 1;
const brHost = (u) => { try { const x = new URL(u); return x.host; } catch { return ''; } };
const brIsBlank = (u) => !u || u === 'about:blank' || u.startsWith('chrome://newtab');
const brMyTabs = () => BR.tabs;
const brTabOf = (id) => BR.tabs.find((t) => t.id === id) || BR.agents.flatMap((a) => a.tabs || []).find((t) => t.id === id);
const brAgentOf = (key) => BR.agents.find((a) => a.key === key);
const brShown = () => (BR.mode === 'agents' ? brAgentTab() : brTabOf(BR.tab));
const brCurTab = (a) => a?.tabs?.find((t) => t.id === a.cur) || a?.tabs?.[a.tabs.length - 1];
const brAgentTab = () => { if (BR.rp) return null; const a = brAgentOf(BR.agent); return a?.tabs?.find((t) => t.id === BR.agentTab) || brCurTab(a); };
const brCanDrive = (t) => !!t && (t.src === 'me' || !!brAgentOf(BR.agent)?.held);
const brMac = /Mac|iPhone|iPad/.test(navigator.platform);
const brCoarse = matchMedia('(pointer: coarse)');

// ---- mounting ----
function brMount(view) {
  const slot = $('#browser', view);
  if (!slot || slot === BR.el) return;
  if (BR.el) { slot.replaceWith(BR.el); brConnect(); brLayout(); return; } // back on the page: as you left it
  BR.el = slot;
  slot.innerHTML = `
  <div class="br-top">
    <div class="seg br-mode" role="tablist" aria-label="Whose browser">
      <button type="button" role="tab" data-mode="windows">${ic('window')}<span>Your windows</span></button>
      <button type="button" role="tab" data-mode="agents">${ic('sparkle')}<span>Agents</span><b class="br-live" hidden></b></button>
    </div>
    <div class="br-tabs" role="tablist" aria-label="Tabs"></div>
    <div class="br-agtabs" aria-label="The agent's tabs"></div>
    <span class="grow"></span>
    <span class="conn br-wide"></span>
    <button type="button" class="icon-btn ghost sm bell br-wide" data-bell aria-haspopup="dialog" aria-expanded="false" aria-label="Notifications" title="Notifications">${ic('bell')}<span class="bell-n" hidden></span></button>
    <button type="button" class="icon-btn ghost sm br-wide" data-act="lock" aria-label="Lock panel" title="Lock">${ic('lock-keyhole')}</button>
  </div>
  <div class="br-bar">
    <button type="button" class="icon-btn ghost sm" data-act="back" title="Back (Alt ←)" aria-label="Back">${ic('arrow-left')}</button>
    <button type="button" class="icon-btn ghost sm" data-act="fwd" title="Forward (Alt →)" aria-label="Forward">${ic('arrow-right')}</button>
    <button type="button" class="icon-btn ghost sm br-reload" data-act="reload" title="Reload (⌘R) · Shift: without the cache" aria-label="Reload">${ic('refresh')}</button>
    <form class="br-omni" autocomplete="off">
      <span class="br-site"></span>
      <input class="br-url" type="text" inputmode="search" enterkeyhint="go" spellcheck="false" autocapitalize="off" autocorrect="off" placeholder="Search, or type a URL or a port (:5173)" aria-label="Address">
      <span class="br-chip" hidden></span>
      <i class="br-prog" aria-hidden="true"></i>
    </form>
    <div class="br-agentbar" hidden></div>
    <button type="button" class="hbtn br-t" data-act="ports" title="What runs on localhost">${ic('server')}<span>Localhost</span><span class="cnt br-pn"></span></button>
    <button type="button" class="hbtn br-t" data-act="viewport" title="Viewport, colour scheme and network">${ic('monitor')}<span class="br-vpl">Fit</span></button>
    <button type="button" class="hbtn br-t" data-act="devtools" title="Chrome DevTools: Elements, Styles, Console, Network, Sources… (${brMac ? '⌥⌘I' : 'F12'}; right-click → Inspect)">${ic('code')}<span>DevTools</span></button>
    <button type="button" class="hbtn br-t br-annb" data-act="annotate" title="Draw, box and pin comments on the page, then send it to an agent (A)">${ic('pen-tool')}<span>Annotate</span></button>
    <button type="button" class="hbtn br-t br-probb" data-act="dev" title="Problems, console and network (P)">${ic('bug')}<span>Problems</span><span class="br-pc"></span></button>
    <button type="button" class="icon-btn ghost sm" data-act="more" title="More" aria-label="More">${ic('more-h')}</button>
  </div>
  <div class="br-main">
    <aside class="br-rail" aria-label="Agent sessions"></aside>
    <div class="br-work">
    <div class="br-stage">
      <div class="br-dev-frame">
        <canvas class="br-canvas" tabindex="-1" aria-label="The page"></canvas>
        <div class="br-ann" hidden></div>
        <div class="br-grip" title="Drag to resize · double-click: back to the device's width" hidden></div>
      </div>
      <div class="br-dims" hidden></div>
      <div class="br-start" hidden></div>
      <div class="br-note" hidden></div>
      <div class="br-wall" hidden></div>
      <div class="br-rp" hidden></div>
      <div class="br-drive" hidden></div>
      <div class="br-step" hidden></div>
      <div class="br-jsd" hidden></div>
      <form class="br-find" hidden><span>${ic('search')}</span><input type="text" spellcheck="false" autocomplete="off" placeholder="Find in page" aria-label="Find in page"><b class="br-fn"></b>
        <button type="button" class="icon-btn ghost sm" data-find="-1" aria-label="Previous match" title="Previous (⇧↵)">${ic('angle-up')}</button><button type="button" class="icon-btn ghost sm" data-find="1" aria-label="Next match" title="Next (↵)">${ic('angle-down')}</button><button type="button" class="icon-btn ghost sm" data-find="0" aria-label="Close" title="Close (Esc)">${IC.x}</button></form>
      <textarea class="br-kb" autocapitalize="off" autocomplete="off" autocorrect="off" spellcheck="false" aria-label="Type into the page"></textarea>
    </div>
    <div class="br-dtr" hidden title="Drag to resize"></div>
    <section class="br-dt" hidden aria-label="DevTools">
      <div class="br-dth">
        <span class="br-dtt">${ic('code')}<b>DevTools</b></span>
        <span class="br-dtsel mono"></span>
        <button type="button" class="btn sm br-dtsend" data-dt="send" disabled title="The element selected in Elements, with its HTML, CSS rules and a picture, to an agent">${ic('direct-send')}<span>Send to agent</span></button>
        <span class="grow"></span>
        <button type="button" class="icon-btn ghost sm" data-dt="pick" title="Pick an element on the page (${brMac ? '⇧⌘C' : 'Ctrl+Shift+C'})" aria-label="Pick an element">${ic('crosshairs')}</button>
        <button type="button" class="icon-btn ghost sm" data-dt="side" title="Dock at the bottom, or on the right" aria-label="Dock side">${ic('sidebar-left2')}</button>
        <button type="button" class="icon-btn ghost sm" data-dt="reload" title="Reload DevTools" aria-label="Reload DevTools">${ic('refresh')}</button>
        <button type="button" class="icon-btn ghost sm" data-dt="close" title="Close (${brMac ? '⌥⌘I' : 'F12'})" aria-label="Close DevTools">${IC.x}</button>
      </div>
      <div class="br-dtb"></div>
    </section>
    </div>
    <aside class="br-side" aria-label="Timeline"></aside>
  </div>
  <div class="br-dev" hidden>
    <div class="br-dev-rz" title="Drag to resize"></div>
    <div class="br-dev-h">
      <div class="seg sm br-devtabs" role="tablist">
        <button type="button" data-dp="probs">${ic('alert-triangle')}<span>Problems</span><b class="br-dn" data-n="probs"></b></button>
        <button type="button" data-dp="cons">${ic('terminal-square')}<span>Console</span><b class="br-dn" data-n="cons"></b></button>
        <button type="button" data-dp="nets">${ic('signal')}<span>Network</span><b class="br-dn" data-n="nets"></b></button>
      </div>
      <label class="br-filter">${ic('search')}<input type="search" placeholder="Filter" aria-label="Filter"></label>
      <span class="grow"></span>
      <button type="button" class="hbtn" data-act="devsend" title="Send what's here to an agent">${ic('direct-send')}<span>Send to agent</span></button>
      <button type="button" class="icon-btn ghost sm" data-act="devclear" title="Clear" aria-label="Clear">${ic('trash2')}</button>
      <button type="button" class="icon-btn ghost sm" data-act="dev" title="Close (P)" aria-label="Close">${IC.x}</button>
    </div>
    <div class="br-dev-b" tabindex="0"></div>
    <form class="br-eval" hidden><span>›</span><input type="text" spellcheck="false" autocomplete="off" placeholder="Run JavaScript in the page" aria-label="Console input"></form>
  </div>
  <input type="file" class="br-files" hidden>
  <dialog class="modal fm-dlg br-dlg"></dialog>`;
  const conn = $('.br-top .conn', slot);
  const sync = () => { conn.className = $('#conn').className + ' br-wide'; };
  new MutationObserver(sync).observe($('#conn'), { attributes: true });
  sync();
  BR.canvas = $('.br-canvas', slot);
  BR.ctx = BR.canvas.getContext('2d', { alpha: false, desynchronized: true });
  BR.kb = $('.br-kb', slot);
  BR.stage = $('.br-stage', slot);
  slot.addEventListener('click', brClick);
  $('.br-omni', slot).addEventListener('submit', (e) => { e.preventDefault(); brGo($('.br-url', slot).value); });
  brOmni($('.br-url', slot));
  brInput();
  brDevWire();
  brDTWire();
  new ResizeObserver(() => requestAnimationFrame(brLayout)).observe(BR.stage); // a frame later: sizing the canvas here would loop
  addEventListener('resize', () => brLayout());
  brConnect();
  brRender();
}

// leaving the page: the stream stops (the Chromium goes on: your tabs and the agents' keep running)
function brLeave() {
  if (BR.ann) brAnnClose();
  BR.open = false;
  clearTimeout(BR.retryT);
  if (BR.ws) { BR.ws.onclose = null; BR.ws.close(1000); BR.ws = null; }
}

// ---- the websocket ----
function brConnect() {
  if (BR.ws || S.view !== 'browser') return;
  BR.open = true;
  const ws = new WebSocket(`${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/api/browser/ws`);
  ws.binaryType = 'arraybuffer';
  BR.ws = ws;
  ws.onopen = () => { BR.retry = 0; BR.offline = false; brRender(); BR.watching = ''; BR.lastSize = ''; brWatch(); brThumbs(); if (BR.devOpen) brSend({ t: 'dev', tab: brShown()?.id, on: true }); if (BR.mode === 'agents') { brSend({ t: 'agent', agent: BR.agent }); brLoadTrails(); } }; // opened on Agents: its recordings at once
  ws.onmessage = (e) => (typeof e.data === 'string' ? brMsg(JSON.parse(e.data)) : brBin(new Uint8Array(e.data)));
  ws.onclose = () => {
    if (BR.ws !== ws) return;
    BR.ws = null;
    BR.q = [];
    if (S.view !== 'browser') return;
    BR.offline = true;
    brRender();
    clearTimeout(BR.retryT);
    BR.retryT = setTimeout(async () => {
      const r = await api('/api/me').catch(() => null);
      if (r?.status === 401) return showLogin('Session ended — enter your PIN.');
      brConnect();
    }, Math.min(8000, 500 * 2 ** BR.retry++));
  };
}
const brSend = (m) => { if (BR.ws?.readyState === 1) BR.ws.send(JSON.stringify(m)); };
// a phone drops the socket when it locks or switches apps, and slows the retry's timer meanwhile: back at once
const brBack = () => { if (!document.hidden && !BR.ws && BR.open && S.view === 'browser') { clearTimeout(BR.retryT); BR.retry = 0; brConnect(); } };
document.addEventListener('visibilitychange', brBack);
addEventListener('online', brBack);
addEventListener('pageshow', brBack);

function brMsg(m) {
  switch (m.t) {
    case 'state': {
      const was = BR.tabs.map((t) => t.id).join();
      BR.tabs = m.tabs || [];
      BR.agents = m.agents || [];
      BR.starting = m.starting;
      BR.error = m.error || '';
      BR.dtv = m.dtv || '';
      brDownloads(m.downloads || []);
      if (!BR.tab || !brTabOf(BR.tab)) BR.tab = BR.tabs[0]?.id || '';
      if (BR.mode === 'agents' && !brAgentOf(BR.agent)) BR.agent = BR.agents.find((a) => a.alive)?.key || BR.agents[0]?.key || '';
      if (was !== BR.tabs.map((t) => t.id).join()) brSave();
      brRender();
      brWatch();
      brThumbs();
      break;
    }
    case 'created': BR.tab = m.tab; BR.mode = 'windows'; brSave(); brRender(); brWatch(); if (BR.wantURL) { BR.wantURL = ''; } break;
    case 'opened': if (BR.mode === 'windows' && BR.tab === m.opener) { BR.tab = m.tab; brSave(); brRender(); brWatch(); } break;
    case 'dev': brDevData(m); break;
    case 'evaled': m.at = Date.now(); BR.dev.evals.push(m); if (BR.devPane === 'cons') brDevPaint(); break;
    case 'probed': BR.probes.get(m.seq)?.(m.info); BR.probes.delete(m.seq); break;
    case 'picked': brPicked(m.info); break;
    case 'clip': if (m.text) navigator.clipboard?.writeText(m.text).catch(() => {}); break;
    case 'dtnode': if (m.tab === BR.dtFor) { BR.dtSel = m.label; brDTSel(); } break;
    case 'fields': if (m.tab === BR.watching) BR.fields = { tab: m.tab, r: m.r || [] }; break;
    case 'cursor': if (m.tab === BR.watching) { BR.cursor = m.c; BR.cursorAt = Date.now(); if (!BR.ann) BR.canvas.style.cursor = /^[\w-]+$/.test(m.c) ? m.c : 'default'; } break;
    case 'steps': brSteps(m); break;
    case 'found': if (m.tab === brShown()?.id) setText($('.br-fn', BR.el), $('.br-find input', BR.el).value ? (m.n ? `${m.n} match${m.n === 1 ? '' : 'es'}` : 'No matches') : ''); break;
    case 'toast': toast(m.text, true); break;
  }
}

// a binary message: kind (1 a frame, 2 a still, 3 a card's frame), the metadata's length, JSON, then the image
function brBin(u8) {
  const n = (u8[1] << 8) | u8[2];
  const meta = JSON.parse(new TextDecoder().decode(u8.subarray(3, 3 + n)));
  const img = u8.subarray(3 + n);
  if (u8[0] === 1) return brFrame(meta, img);
  if (u8[0] === 2) return BR.stillWait?.(meta, img);
  if (u8[0] === 3) return brThumb(meta, img);
  if (u8[0] === 4) return brEnqueue(4, meta, img);
}

// frames and patches: decoded off the main thread (createImageBitmap) and drawn in the order they came, one at a
// time. A frame is skipped when a newer one for the tab is already queued (and a patch with it: the frame has it);
// every frame received is acknowledged, drawn or skipped: the server keeps two in flight.
function brEnqueue(kind, meta, img) {
  BR.q ||= [];
  BR.q.push({ kind, meta, img });
  if (!BR.pumping) brPump();
}
async function brPump() {
  BR.pumping = true;
  while (BR.q.length && !BR.ann) { // paused while you annotate: frames not acknowledged stop the stream
    const it = BR.q.shift();
    const newer = BR.q.some((x) => x.kind === 1 && x.meta.tab === it.meta.tab);
    if (it.meta.tab !== BR.watching || newer || (it.kind === 4 && !BR.meta)) { if (it.kind === 1) brSend({ t: 'ack' }); continue; }
    try {
      const bmp = await createImageBitmap(new Blob([it.img], { type: it.meta.fmt === 'webp' ? 'image/webp' : it.meta.fmt === 'png' ? 'image/png' : 'image/jpeg' }));
      if (it.meta.tab !== BR.watching) bmp.close();
      else if (it.kind === 1) brDraw(it.meta, bmp);
      else brPatch(it.meta, bmp);
    } catch { /* a frame that doesn't decode: the next one will */ }
    if (it.kind === 1) brSend({ t: 'ack' });
  }
  BR.pumping = false;
}
const brFrame = (meta, img) => brEnqueue(1, meta, img);
// a patch: a small sharp capture of what changed, drawn where it goes over the canvas
function brPatch(meta, bmp) {
  const c = BR.canvas, m = BR.meta;
  const kx = c.width / m.dw, ky = c.height / m.dh;
  BR.ctx.drawImage(bmp, meta.x * kx, meta.y * ky, meta.w * kx, meta.h * ky);
  bmp.close();
}

function brDraw(meta, bmp) {
  const c = BR.canvas;
  if (c.width !== bmp.width || c.height !== bmp.height) { c.width = bmp.width; c.height = bmp.height; }
  BR.ctx.drawImage(bmp, 0, 0);
  BR.bmp?.close?.();
  BR.bmp = bmp;
  const first = !BR.meta || BR.meta.dw !== meta.dw || BR.meta.dh !== meta.dh;
  BR.meta = meta;
  BR.drawn = meta.seq;
  if (first) brFit();
  BR.el.classList.add('br-has-frame');
  brFieldsAsk();
  // the card of the session you're watching: its picture from this stream (once a second), not one of its own
  if (BR.mode === 'agents' && Date.now() - (BR.cardAt || 0) > 1000 && BR.el.querySelector(`[data-thumb="${CSS.escape(meta.tab)}"]`)) {
    BR.cardAt = Date.now();
    BR.canvas.toBlob((b) => b && brThumbBlob(meta.tab, b), 'image/jpeg', 0.6);
  }
}

// the canvas's size on screen: the page's own CSS size, scaled down to fit the stage (a device bigger than it)
function brFit() {
  const m = BR.meta;
  if (!m) return;
  const fr = $('.br-dev-frame', BR.el);
  const pad = brDeviceMode() ? 2 * 24 : 0;
  const sw = BR.stage.clientWidth - pad, sh = BR.stage.clientHeight - pad - (brDeviceMode() ? 28 : 0);
  const k = Math.min(1, sw / m.dw, sh / m.dh);
  BR.scale = k;
  BR.canvas.style.width = Math.round(m.dw * k) + 'px';
  BR.canvas.style.height = Math.round(m.dh * k) + 'px';
  fr.classList.toggle('device', brDeviceMode());
  fr.classList.toggle('phone', brDeviceMode() && (brShown()?.vp?.w || 0) < 600);
  const d = $('.br-dims', BR.el);
  setHidden(d, !brDeviceMode());
  if (brDeviceMode()) {
    const vp = brShown().vp;
    d.innerHTML = `<b>${esc(vp.name || 'Custom')}</b><span>${vp.w} × ${vp.h}</span><span>${+vp.dpr.toFixed(2)}×</span>${k < 1 ? `<span class="faint">shown at ${Math.round(k * 100)}%</span>` : ''}`;
  }
  setHidden($('.br-grip', BR.el), !brDeviceMode() || !!BR.ann);
}
const brDeviceMode = () => BR.mode === 'windows' && brShown()?.vp?.mode === 'device';

// which tab this page shows, and at what size: in Fit, the page takes the stage's size at this screen's density
function brWatch() {
  const t = brShown();
  const id = t?.id || '';
  const w = Math.floor(BR.stage?.clientWidth || 0), h = Math.floor(BR.stage?.clientHeight || 0);
  const dpr = Math.min(devicePixelRatio || 1, 2);
  const mob = brCoarse.matches && innerWidth < 900;
  const key = `${id}|${w}|${h}|${dpr}|${mob}`;
  if (key === BR.lastSize) return;
  const switched = id !== BR.watching;
  BR.lastSize = key;
  BR.watching = id;
  if (switched) {
    BR.meta = null;
    BR.q = (BR.q || []).filter((x) => x.kind !== 4);
    BR.el?.classList.remove('br-has-frame');
    BR.ctx?.clearRect(0, 0, BR.canvas.width, BR.canvas.height);
    if (BR.devOpen) { BR.dev = { tab: id, cons: [], probs: [], nets: [], evals: [] }; brSend({ t: 'dev', tab: id, on: true }); brDevPaint(); }
  }
  if (w > 40 && h > 40) brSend({ t: 'watch', tab: id, w, h, dpr, mob });
}

function brLayout() {
  if (!BR.el) return;
  BR.narrow = innerWidth < 900;
  BR.el.classList.toggle('narrow', BR.narrow);
  clearTimeout(BR.layoutT);
  BR.layoutT = setTimeout(() => { brWatch(); brFit(); BR.ann && brAnnFit(); }, 120);
  brFit();
}

// ---- drawing the chrome ----
function brRender() {
  if (!BR.el) return;
  const el = BR.el, agents = BR.mode === 'agents';
  el.classList.toggle('agents', agents);
  el.classList.toggle('wall', agents && BR.wall);
  for (const b of $$('.br-mode [data-mode]', el)) b.setAttribute('aria-pressed', String(b.dataset.mode === BR.mode));
  const live = BR.agents.filter((a) => a.alive).length;
  const lv = $('.br-live', el);
  setHidden(lv, !live);
  setText(lv, String(live));
  brTabs();
  brAgents();
  const t = brShown();
  // the bar
  const blank = !t || (t.src === 'me' && brIsBlank(t.url));
  const url = $('.br-url', el);
  if (document.activeElement !== url) url.value = blank ? '' : t.url;
  url.readOnly = agents && !brCanDrive(t);
  $('.br-omni', el).classList.toggle('loading', !!t?.loading);
  $('.br-omni', el).classList.toggle('secure', !!t?.url?.startsWith('https:'));
  $('.br-site', el).innerHTML = brSiteIcon(t);
  const port = brPortOf(t?.url);
  const chip = $('.br-chip', el);
  setHidden(chip, !port);
  if (port) chip.innerHTML = `${ic('server')}<span>${esc([port.proc, port.project].filter(Boolean).join(' · '))}</span>`;
  setDisabled($('[data-act="back"]', el), !t?.back || !brCanDrive(t));
  setDisabled($('[data-act="fwd"]', el), !t?.fwd || !brCanDrive(t));
  const rl = $('.br-reload', el);
  setDisabled(rl, !t || !brCanDrive(t));
  if (rl.dataset.act !== (t?.loading ? 'stop' : 'reload')) {
    rl.innerHTML = t?.loading ? IC.x : ic('refresh');
    rl.dataset.act = t?.loading ? 'stop' : 'reload';
  }
  const n = (t?.probs || [0, 0, 0]);
  const pc = $('.br-pc', el);
  const errs = n[0] + n[2];
  pc.className = 'br-pc' + (errs ? ' bad' : n[1] ? ' warn' : '');
  setText(pc, errs || n[1] ? String(errs || n[1]) : '');
  $('[data-act="dev"]', el).setAttribute('aria-pressed', String(BR.devOpen));
  $('.br-annb', el).setAttribute('aria-pressed', String(!!BR.ann));
  setText($('.br-vpl', el), t?.vp?.mode === 'device' ? t.vp.name || 'Custom' : 'Fit');
  $('[data-act="viewport"]', el).hidden = agents;
  $('[data-act="devtools"]', el).hidden = agents && !brCanDrive(t);
  const pn = BR.ports?.length;
  setText($('.br-pn', el), pn ? String(pn) : '');
  // the stage: the page, the start page, or a note
  const start = $('.br-start', el);
  const showStart = !agents && t && t.src === 'me' && brIsBlank(t.url) && !t.loading;
  if (showStart !== !start.hidden) { setHidden(start, !showStart); if (showStart) brStart(); }
  if (showStart) brStartPorts();
  el.classList.toggle('blank', !!showStart);
  let note = '';
  if (BR.error) note = `<b>The browser didn't start</b><span>${esc(BR.error)}</span><button type="button" class="btn sm" data-act="retry">${ic('refresh')}Try again</button>`;
  else if (BR.offline) note = `<span class="br-spin"></span><span>Reconnecting to the browser…</span>`;
  else if (!agents && BR.starting && !BR.tabs.length) note = `<span class="br-spin"></span><span>Starting Chromium…</span>`;
  else if (t?.crashed) note = `<b>This tab crashed</b><span>${esc(brHost(t.url))} ran out of memory or hit a bug.</span><button type="button" class="btn sm" data-act="reload">${ic('refresh')}Reload</button>`;
  else if (agents && !BR.agents.length) note = `<span class="br-ag-empty">${icAu('sparkle')}</span><b>No agent has opened a browser yet</b><span>When an agent tests a page with <code>cube-browser</code>, its browser shows up here live, with every command it runs.</span>`;
  else if (agents && !t) note = `<b>This session has finished</b><span>Its timeline is on the right.</span>`;
  const nb = $('.br-note', el);
  setHidden(nb, !note);
  if (note && nb.dataset.html !== note) { nb.innerHTML = note; nb.dataset.html = note; }
  brDrive();
  brDTRender();
  brJSDialog(t);
  brChooser(t);
  if (BR.mode === 'agents') brTimeline();
  brFit();
}

// a site's icon: fetched through the panel (the page's CSP keeps images to it); a data: icon as it is
const brIconURL = (u) => (u.startsWith('data:image/') ? u : `/api/browser/icon?url=${encodeURIComponent(u)}`);
addEventListener('error', (e) => { // an icon that doesn't load: the globe instead
  const img = e.target;
  if (img?.tagName === 'IMG' && img.closest?.('.br-fav, .br-site')) img.replaceWith(document.createRange().createContextualFragment(ic('globe')));
}, true);
function brSiteIcon(t) {
  if (!t) return ic('globe');
  if (t.icon) return `<img src="${esc(brIconURL(t.icon))}" alt="">`;
  if (t.url?.startsWith('https:')) return ic('lock-keyhole');
  return ic(/^http:\/\/(localhost|127\.)/.test(t.url || '') ? 'server' : 'globe');
}

function brTabs() {
  const box = $('.br-tabs', BR.el);
  const html = BR.tabs.map((t) => {
    const n = t.probs || [0, 0, 0];
    const bad = n[0] + n[2];
    const title = brIsBlank(t.url) ? 'New tab' : t.title || brHost(t.url) || t.url;
    const fav = t.loading ? '<i class="br-tspin"></i>' : t.icon ? `<img src="${esc(brIconURL(t.icon))}" alt="" loading="lazy">` : ic(brIsBlank(t.url) ? 'sparkle' : /^http:\/\/(localhost|127\.)/.test(t.url) ? 'server' : 'globe');
    return `<div class="br-tab${t.id === BR.tab && BR.mode === 'windows' ? ' on' : ''}${t.crashed ? ' crashed' : ''}" role="tab" data-tab="${esc(t.id)}" title="${esc(title + (t.url && !brIsBlank(t.url) ? '\n' + t.url : ''))}" draggable="false">` +
      `<span class="br-fav">${fav}</span><span class="br-tt">${esc(title)}</span>${bad ? `<b class="br-tb bad">${bad}</b>` : n[1] ? `<b class="br-tb warn">${n[1]}</b>` : ''}` +
      `<button type="button" class="br-tx" data-close="${esc(t.id)}" aria-label="Close tab" tabindex="-1">${IC.x}</button></div>`;
  }).join('') + `<button type="button" class="br-newtab" data-act="newtab" title="New tab" aria-label="New tab">${ic('plus')}</button>`;
  if (box.dataset.html !== html) { box.innerHTML = html; box.dataset.html = html; brTabDrag(box); }
  $('.br-tab.on', box)?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
}

// tabs reorder by dragging (the pointer, not HTML drag and drop: the strip stays crisp, and it works with a finger)
function brTabDrag(box) {
  for (const el of $$('.br-tab', box)) {
    el.onpointerdown = (e) => {
      if (e.button !== 0 || e.target.closest('.br-tx')) return;
      const x0 = e.clientX;
      let moved = false;
      const tabs = $$('.br-tab', box);
      const from = tabs.indexOf(el);
      let to = from;
      const mv = (ev) => {
        const dx = ev.clientX - x0;
        if (!moved && Math.abs(dx) < 6) return;
        moved = true;
        el.classList.add('drag');
        el.style.transform = `translateX(${dx}px)`;
        const mid = el.getBoundingClientRect().left + el.offsetWidth / 2;
        to = tabs.filter((x) => x !== el).filter((x) => x.getBoundingClientRect().left + x.offsetWidth / 2 < mid).length;
      };
      const up = () => {
        removeEventListener('pointermove', mv);
        removeEventListener('pointerup', up);
        el.classList.remove('drag');
        el.style.transform = '';
        if (moved && to !== from) {
          const id = el.dataset.tab;
          BR.tabs.splice(to, 0, BR.tabs.splice(from, 1)[0]);
          box.dataset.html = '';
          brTabs();
          brSend({ t: 'move', tab: id, to });
        }
        if (moved) BR.noClick = Date.now();
      };
      addEventListener('pointermove', mv);
      addEventListener('pointerup', up);
    };
    el.onauxclick = (e) => { if (e.button === 1) brSend({ t: 'close', tab: el.dataset.tab }); };
  }
}

// ---- clicks ----
function brClick(e) {
  const el = BR.el;
  const mode = e.target.closest('[data-mode]');
  if (mode && mode.closest('.br-mode')) return brSetMode(mode.dataset.mode);
  const x = e.target.closest('[data-close]');
  if (x) { e.stopPropagation(); return brClose(x.dataset.close); }
  const tab = e.target.closest('[data-tab]');
  if (tab && !(BR.noClick > Date.now() - 300)) { BR.tab = tab.dataset.tab; BR.mode = 'windows'; brSave(); brRender(); brWatch(); return; }
  const rc = e.target.closest('[data-trail]');
  if (rc) { brReplay(rc.dataset.trail); return; }
  const card = e.target.closest('[data-agent]');
  if (card) { brRpClose(); BR.agent = card.dataset.agent; BR.agentTab = ''; if (BR.wall && card.closest('.br-wall')) BR.wall = false; brSave(); brSend({ t: 'agent', agent: BR.agent }); BR.steps = []; brRender(); brWatch(); brThumbs(); return; }
  const at = e.target.closest('[data-agtab]');
  if (at) { BR.agentTab = at.dataset.agtab; brRender(); brWatch(); return; }
  const open = e.target.closest('[data-open]');
  if (open) { e.preventDefault(); return brOpen(open.dataset.open, e.metaKey || e.ctrlKey || e.button === 1); }
  const run = e.target.closest('[data-run]');
  if (run) return brRun(BR.projects?.[+run.dataset.run]);
  const stp = e.target.closest('[data-step]');
  if (stp) { stp.classList.toggle('open'); return; }
  const dp = e.target.closest('[data-dp]');
  if (dp) { BR.devPane = dp.dataset.dp; brSave(); brDevPaint(); return; }
  const b = e.target.closest('[data-act]');
  if (!b || b.disabled) return;
  const t = brShown();
  const a = b.dataset.act;
  const r = b.getBoundingClientRect();
  switch (a) {
    case 'lock': return $('#logout').click();
    case 'back': case 'fwd': case 'stop': return brSend({ t: a, tab: t?.id });
    case 'reload': return brSend({ t: 'reload', tab: t?.id, hard: e.shiftKey });
    case 'retry': BR.error = ''; brSend({ t: 'new', url: '' }); return;
    case 'newtab': return brNewTab();
    case 'ports': return brPortsMenu(r);
    case 'viewport': return brViewportMenu(r);
    case 'annotate': return BR.ann ? brAnnClose() : brAnnotate();
    case 'devtools': return brDT(!BR.dtOpen);
    case 'dev': return brDevToggle();
    case 'devclear': brSend({ t: 'clear', tab: t?.id }); BR.dev = { tab: t?.id, cons: [], probs: [], nets: [], evals: [] }; return brDevPaint();
    case 'devsend': return brDevSend();
    case 'more': return brMore(r);
    case 'hold': return brSend({ t: 'hold', agent: BR.agent, on: !brAgentOf(BR.agent)?.held });
    case 'wall': BR.wall = !BR.wall; brSave(); brRender(); brThumbs(); return;
    case 'replay': { const ag = brAgentOf(BR.agent); if (ag?.trail) brReplay(ag.trail); return; }
    case 'chat': { const ag = brAgentOf(BR.agent); if (ag?.pane) { setView('term'); S.openWhenSeen = `${ag.pane.session}:${ag.pane.pane}`; } return; }
    case 'refresh-start': BR.portsAt = 0; BR.projects = null; return brStart();
    case 'kb': BR.kb.focus(); return;
  }
}

function brSetMode(m) {
  if (BR.mode === m) return;
  if (BR.ann) brAnnClose();
  BR.mode = m;
  if (m !== 'agents') brRpClose();
  if (m === 'agents') {
    brLoadTrails();
    if (!brAgentOf(BR.agent)) BR.agent = BR.agents.find((a) => a.alive)?.key || BR.agents[0]?.key || '';
    BR.steps = [];
    brSend({ t: 'agent', agent: BR.agent });
  } else brSend({ t: 'agent', agent: '' });
  brSave();
  brRender();
  brWatch();
  brThumbs();
}

function brClose(id) {
  const i = BR.tabs.findIndex((t) => t.id === id);
  if (id === BR.tab) BR.tab = (BR.tabs[i + 1] || BR.tabs[i - 1])?.id || '';
  if (BR.tabs.length <= 1) { brSend({ t: 'new', url: '' }); }
  brSend({ t: 'close', tab: id });
  brSave();
  brRender();
  brWatch();
}

function brNewTab(url = '') {
  BR.mode = 'windows';
  brSend({ t: 'new', url });
  setTimeout(() => { if (!url) $('.br-start-q', BR.el)?.focus() || $('.br-url', BR.el)?.focus(); }, 150);
}

// open a URL: here (the current tab, unless it's busy being something else), or in a new tab
function brOpen(url, newTab) {
  if (!url) return;
  brRecent(url);
  const t = brTabOf(BR.tab);
  if (BR.mode === 'agents' && !brCanDrive(brShown())) newTab = true;
  if (newTab || !t || t.src !== 'me') return brNewTab(url);
  if (BR.mode !== 'windows') { BR.mode = 'windows'; brSave(); brRender(); brWatch(); }
  brSend({ t: 'nav', tab: t.id, url });
}
function brGo(q) {
  q = q.trim();
  if (!q) return;
  $('.br-url', BR.el).blur();
  $('.fm-menu.br-sug')?.remove();
  const t = brShown();
  if (BR.mode === 'agents' && brCanDrive(t)) return brSend({ t: 'nav', tab: t.id, url: q });
  brOpen(q, false);
}
function brRecent(url) {
  if (/^:?\d+$/.test(url)) url = 'http://localhost:' + url.replace(':', '');
  BR.recent = [url, ...BR.recent.filter((u) => u !== url)].slice(0, 40);
  brSave();
}

// ---- the address bar: suggestions as you type (localhost servers, open tabs, where you've been, a search) ----
function brOmni(input) {
  let sel = -1, items = [];
  const close = () => { $('.fm-menu.br-sug')?.remove(); sel = -1; };
  const paint = () => {
    close();
    const q = input.value.trim().toLowerCase();
    if (document.activeElement !== input) return;
    const ports = (BR.ports || []).map((p) => ({ url: `http://localhost:${p.port}`, l: `localhost:${p.port}`, s: [p.title, p.proc, p.project].filter(Boolean).join(' · '), i: 'server' }));
    const tabs = BR.tabs.filter((t) => !brIsBlank(t.url) && t.id !== BR.tab).map((t) => ({ tab: t.id, l: t.title || t.url, s: 'Switch to tab · ' + brHost(t.url), i: 'window' }));
    const rec = BR.recent.map((u) => ({ url: u, l: u.replace(/^https?:\/\//, '').replace(/\/$/, ''), s: '', i: 'history' }));
    const match = (x) => !q || (x.l + ' ' + x.s).toLowerCase().includes(q.replace(/^:/, ''));
    items = [...ports.filter(match), ...tabs.filter(match).slice(0, 4), ...rec.filter(match).slice(0, q ? 6 : 5)];
    const seen = new Set();
    items = items.filter((x) => { const k = x.url || x.tab; if (seen.has(k)) return false; seen.add(k); return true; }).slice(0, 9);
    if (q && !/^:?\d+$/.test(q)) items.push({ go: input.value.trim(), l: input.value.trim(), s: /\s/.test(q) || !/\./.test(q) ? 'Search DuckDuckGo' : 'Go to this address', i: 'search' });
    if (!items.length) return;
    const r = input.getBoundingClientRect();
    const m = document.createElement('div');
    m.className = 'fm-menu br-sug';
    m.innerHTML = items.map((x, i) => `<button type="button" data-si="${i}">${ic(x.i)}<span class="br-sl">${esc(x.l)}</span>${x.s ? `<span class="br-ss">${esc(x.s)}</span>` : ''}</button>`).join('');
    m.style.left = r.left - 6 + 'px';
    m.style.top = r.bottom + 6 + 'px';
    m.style.width = Math.max(r.width + 12, 320) + 'px';
    document.body.append(m);
    m.addEventListener('pointerdown', (e) => {
      const b = e.target.closest('[data-si]');
      if (!b) return;
      e.preventDefault();
      pick(items[+b.dataset.si], e.metaKey || e.ctrlKey);
    });
  };
  const pick = (x, newTab) => {
    close();
    input.blur();
    if (x.tab) { BR.tab = x.tab; BR.mode = 'windows'; brSave(); brRender(); brWatch(); return; }
    if (x.go) return brGo(x.go);
    brOpen(x.url, newTab);
  };
  // all of it selected, to type over (as a browser's bar does): at once on focus (not on a timer: a busy page runs
  // keystrokes first, and a late select-all would take what was typed), and again on the click that focused it (its
  // caret lands after focus); never once you've typed
  let fresh = false;
  const all = () => { if (fresh) input.setSelectionRange(0, input.value.length); };
  input.addEventListener('focus', () => { fresh = true; all(); brLoadPorts(); paint(); });
  input.addEventListener('click', () => { all(); fresh = false; });
  input.addEventListener('blur', () => setTimeout(() => { close(); brRender(); }, 120));
  input.addEventListener('input', () => { fresh = false; sel = -1; paint(); });
  input.addEventListener('keydown', (e) => {
    const m = $('.fm-menu.br-sug');
    if (e.key === 'Escape') { close(); input.blur(); BR.kb.focus(); return; }
    if (!m || !items.length) return;
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      sel = (sel + (e.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length;
      $$('[data-si]', m).forEach((b, i) => b.classList.toggle('on', i === sel));
    } else if (e.key === 'Enter' && sel >= 0) { e.preventDefault(); pick(items[sel], e.metaKey || e.ctrlKey); }
  });
}

// ---- input: the page gets this page's mouse, wheel, touch and keys ----
const brMods = (e) => (e.altKey ? 1 : 0) | (e.ctrlKey || (brMac && e.metaKey) ? 2 : 0) | (e.shiftKey ? 8 : 0);
const brBtn = (b) => ['left', 'middle', 'right', 'back', 'forward'][b] || 'left';
function brXY(e) {
  const r = BR.canvas.getBoundingClientRect(), m = BR.meta;
  if (!m || !r.width) return null;
  return { x: (e.clientX - r.left) * (m.dw / r.width), y: (e.clientY - r.top) * (m.dh / r.height) };
}
function brInput() {
  const c = BR.canvas, kb = BR.kb;
  const tab = () => brShown();
  const drivable = () => brCanDrive(tab()) && !BR.ann;
  c.addEventListener('pointerdown', (e) => {
    if (!drivable()) { if (BR.mode === 'agents' && tab()) brHint(); return; }
    if (e.pointerType === 'touch') return; // touch: below
    if (e.button === 2) return; // the right button: the menu (contextmenu)
    const p = brXY(e);
    if (!p) return;
    e.preventDefault();
    kb.focus({ preventScroll: true });
    c.setPointerCapture(e.pointerId);
    BR.buttons |= 1 << e.button;
    brSend({ t: 'mouse', tab: tab().id, type: 'down', x: p.x, y: p.y, btn: brBtn(e.button), btns: brButtons(), n: e.detail || 1, mods: brMods(e) });
  });
  c.addEventListener('pointerup', (e) => {
    if (e.pointerType === 'touch' || e.button === 2 || !drivable()) return;
    const p = brXY(e);
    if (!p) return;
    BR.buttons &= ~(1 << e.button);
    brFlushMove();
    brSend({ t: 'mouse', tab: tab().id, type: 'up', x: p.x, y: p.y, btn: brBtn(e.button), btns: brButtons(), n: e.detail || 1, mods: brMods(e) });
  });
  c.addEventListener('pointermove', (e) => {
    if (e.pointerType === 'touch' || !drivable()) return;
    const p = brXY(e);
    if (!p) return;
    const first = !BR.move;
    BR.move = { t: 'mouse', tab: tab().id, type: 'move', x: p.x, y: p.y, btns: brButtons(), mods: brMods(e) };
    if (first) requestAnimationFrame(brFlushMove);
    brCursorAsk(p);
  });
  c.addEventListener('contextmenu', brCtxMenu);
  c.addEventListener('wheel', (e) => {
    if (!drivable()) return;
    e.preventDefault();
    const p = brXY(e);
    if (!p) return;
    const k = e.deltaMode === 1 ? 40 : e.deltaMode === 2 ? 800 : 1;
    const first = !BR.wheel;
    BR.wheel = { t: 'wheel', tab: tab().id, x: p.x, y: p.y, dx: (BR.wheel?.dx || 0) + e.deltaX * k, dy: (BR.wheel?.dy || 0) + e.deltaY * k, mods: brMods(e) };
    if (first) requestAnimationFrame(() => { const w = BR.wheel; BR.wheel = null; if (w) brSend(w); });
  }, { passive: false });
  // touch: a mobile viewport (touch emulated) gets real touches; a desktop-sized one gets a tap as a click and a
  // drag as the wheel, so a phone can use either
  const touches = (e) => [...e.touches].map((t) => ({ ...brXY(t), id: t.identifier })).filter((p) => p.x !== undefined);
  let tap = null, tap0 = 0, tapP = null;
  const keyboard = (t, p) => { if (brOnField(t, p) || BR.cursor === 'text') kb.focus({ preventScroll: true }); else kb.blur(); setTimeout(brFieldsAsk, 400); };
  c.addEventListener('touchstart', (e) => {
    if (!drivable()) return;
    e.preventDefault();
    const t = tab();
    if (brTouchMode()) { tap0 = Date.now(); tapP = brXY(e.touches[0]); BR.cursor = ''; brCursorAsk(tapP, true); return brSend({ t: 'touch', tab: t.id, type: 'start', pts: touches(e) }); }
    const p = brXY(e.touches[0]);
    tap = { x: e.touches[0].clientX, y: e.touches[0].clientY, p, at: Date.now(), moved: false };
    BR.cursor = '';
    brCursorAsk(p, true); // a field under the finger: the keyboard opens on the tap (iOS: only inside the gesture)
  }, { passive: false });
  c.addEventListener('touchmove', (e) => {
    if (!drivable()) return;
    e.preventDefault();
    const t = tab();
    if (brTouchMode()) return brSend({ t: 'touch', tab: t.id, type: 'move', pts: touches(e) });
    if (!tap) return;
    const x = e.touches[0].clientX, y = e.touches[0].clientY;
    if (!tap.moved && Math.hypot(x - tap.x, y - tap.y) < 8) return;
    tap.moved = true;
    const k = (BR.meta?.dw || 1) / (BR.canvas.getBoundingClientRect().width || 1);
    brSend({ t: 'wheel', tab: t.id, x: tap.p.x, y: tap.p.y, dx: (tap.x - x) * k, dy: (tap.y - y) * k, mods: 0 });
    tap.x = x; tap.y = y;
  }, { passive: false });
  const tend = (e) => {
    if (!drivable()) return;
    e.preventDefault();
    const t = tab();
    if (brTouchMode()) {
      if (e.type === 'touchend' && tap0 && Date.now() - tap0 < 600) keyboard(t, tapP);
      return brSend({ t: 'touch', tab: t.id, type: e.type === 'touchcancel' ? 'cancel' : 'end', pts: touches(e) });
    }
    if (tap && !tap.moved && Date.now() - tap.at < 600) {
      for (const ty of ['move', 'down', 'up']) brSend({ t: 'mouse', tab: t.id, type: ty, x: tap.p.x, y: tap.p.y, btn: 'left', btns: ty === 'down' ? 1 : 0, n: 1, mods: 0 });
      keyboard(t, tap.p);
    }
    tap = null;
  };
  c.addEventListener('touchend', tend, { passive: false });
  c.addEventListener('touchcancel', tend, { passive: false });
  // keys: a hidden field has the focus while you use the page. Printable keys go with their text (the page's key
  // handlers see them); the rest as raw keys; an IME's or a phone keyboard's text as inserted text.
  kb.addEventListener('keydown', (e) => {
    if (!drivable()) return;
    if (brShortcut(e)) return;
    if (e.isComposing || e.key === 'Process' || e.key === 'Unidentified' || e.keyCode === 229) return; // the input event has it
    const t = tab(), mods = brMods(e);
    const meta = brMac && e.metaKey;
    if (meta && /^[cx]$/i.test(e.key)) brSend({ t: 'copy', tab: t.id }); // the page's selection to your clipboard
    if (meta && e.key.toLowerCase() === 'v') return; // the paste event brings the text
    if (!(e.ctrlKey && !brMac && e.key.toLowerCase() === 'v')) e.preventDefault();
    else return; // Ctrl+V on Linux/Windows: the paste event too
    if (!brMac && e.ctrlKey && /^[cx]$/i.test(e.key)) brSend({ t: 'copy', tab: t.id });
    const printable = e.key.length === 1 && !(mods & 3);
    const text = e.key === 'Enter' ? '\r' : printable ? e.key : '';
    brSend({ t: 'key', tab: t.id, type: text ? 'keyDown' : 'rawKeyDown', key: e.key, code: e.code, kc: e.keyCode, mods, text });
  });
  kb.addEventListener('keyup', (e) => {
    if (!drivable() || e.key === 'Unidentified' || e.keyCode === 229) return;
    brSend({ t: 'key', tab: tab().id, type: 'keyUp', key: e.key, code: e.code, kc: e.keyCode, mods: brMods(e) });
  });
  kb.addEventListener('input', (e) => {
    if (!drivable()) { kb.value = ''; return; }
    if (e.isComposing) return;
    const t = tab();
    if (e.inputType === 'deleteContentBackward') {
      for (const ty of ['rawKeyDown', 'keyUp']) brSend({ t: 'key', tab: t.id, type: ty, key: 'Backspace', code: 'Backspace', kc: 8, mods: 0 });
    } else if (e.inputType === 'insertLineBreak') {
      brSend({ t: 'key', tab: t.id, type: 'keyDown', key: 'Enter', code: 'Enter', kc: 13, mods: 0, text: '\r' });
      brSend({ t: 'key', tab: t.id, type: 'keyUp', key: 'Enter', code: 'Enter', kc: 13, mods: 0 });
    } else if (kb.value) brSend({ t: 'text', tab: t.id, text: kb.value });
    kb.value = '';
  });
  kb.addEventListener('compositionend', (e) => { if (drivable() && e.data) brSend({ t: 'text', tab: tab().id, text: e.data }); kb.value = ''; });
  kb.addEventListener('paste', (e) => {
    if (!drivable()) return;
    const text = e.clipboardData?.getData('text/plain');
    e.preventDefault();
    if (text) brSend({ t: 'text', tab: tab().id, text });
  });
  c.addEventListener('dblclick', (e) => e.preventDefault());
}
// a phone's map of the page's text fields (see "fields" in browser.go): asked after the page changes, at most every
// 300 ms, and once more when it stops
function brFieldsAsk() {
  if (!brCoarse.matches) return;
  clearTimeout(BR.fieldsT);
  const go = () => { BR.fieldsAt = Date.now(); const t = brShown(); if (t) brSend({ t: 'fields', tab: t.id }); };
  if (Date.now() - (BR.fieldsAt || 0) > 300) go();
  BR.fieldsT = setTimeout(go, 350);
}
const brOnField = (t, p) => !!p && BR.fields?.tab === t.id && BR.fields.r.some(([x, y, w, h]) => p.x >= x - 6 && p.x <= x + w + 6 && p.y >= y - 6 && p.y <= y + h + 6);
// ask the page what's under the pointer (at most every 120 ms; a touch at once)
function brCursorAsk(p, now) {
  if (!p) return;
  BR.cursorP = p;
  const go = () => { BR.cursorT = 0; const t = brShown(); if (t && BR.cursorP) brSend({ t: 'cursor', tab: t.id, x: BR.cursorP.x, y: BR.cursorP.y }); };
  if (now) { clearTimeout(BR.cursorT); return go(); }
  if (!BR.cursorT) BR.cursorT = setTimeout(go, 120);
}
const brButtons = () => (BR.buttons & 1 ? 1 : 0) | (BR.buttons & 4 ? 2 : 0) | (BR.buttons & 2 ? 4 : 0);
function brFlushMove() { const m = BR.move; BR.move = null; if (m) brSend(m); }
const brTouchMode = () => { const t = brShown(); return t?.src === 'me' && (t.vp?.mode === 'device' ? t.vp.mobile : brCoarse.matches && innerWidth < 900); };

// the Browser's own keys while the page has the focus: they never reach the page
function brShortcut(e) {
  const k = e.key.toLowerCase(), cmd = brMac ? e.metaKey : e.ctrlKey;
  const t = brShown();
  let act = null;
  if (cmd && k === 'l') act = () => { const u = $('.br-url', BR.el); u.focus(); };
  else if ((cmd && k === 'r') || e.key === 'F5') act = () => brSend({ t: 'reload', tab: t?.id, hard: e.shiftKey });
  else if (e.altKey && e.key === 'ArrowLeft') act = () => brSend({ t: 'back', tab: t?.id });
  else if (e.altKey && e.key === 'ArrowRight') act = () => brSend({ t: 'fwd', tab: t?.id });
  else if (e.altKey && k === 't') act = () => brNewTab();
  else if (e.altKey && k === 'w') act = () => t && brClose(t.id);
  else if (cmd && e.shiftKey && k === 'a') act = () => brAnnotate();
  else if (e.key === 'F12' || (brMac ? e.metaKey && e.altKey && e.code === 'KeyI' : e.ctrlKey && e.shiftKey && e.code === 'KeyI')) act = () => brDT(!BR.dtOpen);
  else if (cmd && e.shiftKey && e.code === 'KeyC' && brCanDrive(t)) act = () => { if (!BR.dtOpen) brDT(true); brSend({ t: 'dtpick', tab: t.id }); };
  else if (cmd && k === 'f' && brCanDrive(t)) act = () => brFind(true);
  if (!act) return false;
  e.preventDefault();
  act();
  return true;
}

// ---- find in the page ----
function brFind(open) {
  const f = $('.br-find', BR.el), i = $('input', f);
  if (!open) { f.hidden = true; brSend({ t: 'find', tab: brShown()?.id, text: '', n: 0 }); BR.kb.focus({ preventScroll: true }); return; }
  f.hidden = false;
  i.focus();
  i.select();
  if (!f.dataset.wired) {
    f.dataset.wired = '1';
    let fresh = true;
    const go = (dir) => { const t = brShown(); if (t) brSend({ t: 'find', tab: t.id, text: i.value, dx: dir, n: fresh ? 0 : 1 }); fresh = false; };
    i.addEventListener('input', () => { fresh = true; clearTimeout(f.t); f.t = setTimeout(() => go(1), 150); });
    i.addEventListener('keydown', (e) => { if (e.key === 'Escape') { e.preventDefault(); brFind(false); } if ((e.metaKey || e.ctrlKey) && e.key === 'f') { e.preventDefault(); i.select(); } });
    f.addEventListener('submit', (e) => { e.preventDefault(); go(BR.findBack ? -1 : 1); });
    i.addEventListener('keydown', (e) => { BR.findBack = e.key === 'Enter' && e.shiftKey; });
    f.addEventListener('click', (e) => { const b = e.target.closest('[data-find]'); if (!b) return; if (b.dataset.find === '0') brFind(false); else go(+b.dataset.find); });
  }
}

// ---- the start page (a new tab): what runs here, the projects you can run, where you've been ----
async function brStart() {
  const box = $('.br-start', BR.el);
  if (!box.firstChild) {
    box.innerHTML = `<div class="br-sw">
      <div class="br-hero"><span class="br-orb">${icAu('globe')}</span><h2>Where to?</h2><p class="muted">A port, a URL, or a search. The browser runs on the VM: <b>localhost</b> is the VM's.</p></div>
      <form class="br-start-f"><span>${ic('search')}</span><input class="br-start-q" type="text" inputmode="search" enterkeyhint="go" spellcheck="false" autocapitalize="off" autocorrect="off" autocomplete="off" placeholder=":5173, localhost:3000, github.com, or anything"><kbd>↵</kbd></form>
      <section><h3>${ic('server')}Running on localhost<button type="button" class="icon-btn ghost sm" data-act="refresh-start" title="Look again" aria-label="Look again">${ic('refresh')}</button></h3><div class="br-ports"><div class="br-skel"></div><div class="br-skel"></div></div></section>
      <section><h3>${ic('play3')}Run a project</h3><div class="br-projs"><div class="br-skel"></div></div></section>
      <section class="br-recent-s" hidden><h3>${ic('history')}Recently</h3><div class="br-recent"></div></section>
    </div>`;
    $('.br-start-f', box).addEventListener('submit', (e) => { e.preventDefault(); const v = $('.br-start-q', box).value; $('.br-start-q', box).value = ''; brGo(v); });
  }
  const rec = BR.recent.slice(0, 10);
  setHidden($('.br-recent-s', box), !rec.length);
  $('.br-recent', box).innerHTML = rec.map((u) => `<a class="chip br-rc" href="#" data-open="${esc(u)}">${esc(u.replace(/^https?:\/\//, '').replace(/\/$/, ''))}</a>`).join('');
  brLoadPorts(true).then(brStartPorts);
  if (!BR.projects) {
    const r = await api('/api/browser/projects').catch(() => null);
    BR.projects = r?.projects || [];
  }
  $('.br-projs', box).innerHTML = BR.projects.length ? BR.projects.slice(0, 12).map((p, i) => `<div class="br-proj">
      <span class="br-pk ${p.kind}">${ic(p.kind === 'static' ? 'document-code' : p.kind === 'python' ? 'code' : 'layers')}</span>
      <span class="br-pb"><b>${esc(p.name)}</b><span class="mono">${esc(p.cmd || 'served by the panel (static files)')}${p.sub ? ` <span class="faint">in ${esc(p.sub)}/</span>` : ''}</span></span>
      ${p.ports?.length ? `<a class="chip on" href="#" data-open="http://localhost:${p.ports[0]}">${ic('arrow-up-right')}:${p.ports[0]}</a>` : `<button type="button" class="btn sm" data-run="${i}">${ic('play3')}Run</button>`}
    </div>`).join('') : '<p class="faint br-none">No projects found in ~/projects, ~/repos or ~/side-quests with a dev script or an index.html.</p>';
}
function brStartPorts() {
  const box = $('.br-start .br-ports', BR.el);
  if (!box || !BR.ports) return;
  // the server lists only what serves a web page: a backend (an API, a database) isn't something to browse
  const card = (p) => `<a class="br-port" href="#" data-open="http://localhost:${p.port}">
      <span class="br-pp">:${p.port}</span><span class="br-pb"><b>${esc(p.title || p.project || p.proc || 'localhost')}</b><span>${esc([p.proc, p.project, p.pane ? `${p.pane.session} · ${p.pane.tab || 'pane ' + p.pane.pane}` : ''].filter(Boolean).join(' · '))}</span></span></a>`;
  const html = BR.ports.length ? BR.ports.map(card).join('') : '<p class="faint br-none">No web page is being served on localhost right now. Run a project below, or start a dev server in a terminal: it shows up here.</p>';
  if (box.dataset.html !== html) { box.innerHTML = html; box.dataset.html = html; }
}
async function brLoadPorts(force) {
  if (!force && Date.now() - BR.portsAt < 4000) return BR.ports;
  BR.portsAt = Date.now();
  const r = await api('/api/browser/ports').catch(() => null);
  if (r?.ports) { BR.ports = r.ports; brRender(); }
  return BR.ports;
}
const brPortOf = (url) => { const m = /^http:\/\/(?:localhost|127\.0\.0\.1|\[::1\]):(\d+)/.exec(url || ''); return m && BR.ports?.find((p) => p.port === +m[1]); };
setInterval(() => { if (S.view === 'browser' && BR.open && !document.hidden) brLoadPorts(); }, 10000);

// the Localhost menu: open a server, stop it, run a project
async function brPortsMenu(r) {
  await brLoadPorts(true);
  const ps = BR.ports || [];
  const items = [];
  for (const p of ps) items.push({ k: 'o' + p.port, l: `:${p.port}  ${p.title || p.project || p.proc || ''}`.trim(), i: 'server' });
  if (!items.length) items.push({ k: 'none', l: 'Nothing listens on localhost', i: 'info-circle' });
  items.push({ hr: 1 }, { k: 'run', l: 'Run a project…', i: 'play3' });
  if (ps.some((p) => p.static || p.pane)) items.push({ k: 'stop', l: 'Stop a server…', i: 'stop-f' });
  fmMenu(r.left, r.bottom, items, async (k) => {
    if (k.startsWith('o')) return brOpen(`http://localhost:${k.slice(1)}`, false);
    if (k === 'run') return brRunPicker();
    if (k === 'stop') return fmMenu(r.left, r.bottom, ps.filter((p) => p.static || p.pane).map((p) => ({ k: String(p.port), l: `Stop :${p.port} ${p.project || p.proc}`, i: 'stop-f', danger: 1 })), async (port) => {
      const x = await api('/api/browser/stop', { port: +port });
      if (x.status !== 200) toast(x.error || 'Could not stop it', true); else { toast(`Stopped :${port}`); setTimeout(() => brLoadPorts(true), 800); }
    }, true);
  }, true);
}
async function brRunPicker() {
  if (!BR.projects) { const r = await api('/api/browser/projects').catch(() => null); BR.projects = r?.projects || []; }
  const r = $('[data-act="ports"]', BR.el).getBoundingClientRect();
  fmMenu(r.left, r.bottom, BR.projects.slice(0, 16).map((p, i) => ({ k: String(i), l: `${p.name} · ${p.cmd || 'static'}`, i: p.ports?.length ? 'arrow-up-right' : 'play3' })), (k) => brRun(BR.projects[+k]), true);
}

// run: confirm (or edit) the command, start it, then open its port once it listens
async function brRun(p) {
  if (!p) return;
  if (p.ports?.length) return brOpen(`http://localhost:${p.ports[0]}`, false);
  let cmd = p.cmd;
  if (p.kind !== 'static') {
    cmd = await brDialog(`<div class="modal-h"><span class="modal-ic fm-ic-au">${icAu('play3')}</span><h2>Run ${esc(p.name)}</h2></div>
      <p class="muted">In a new tab of the <b>servers</b> terminal session, in <span class="mono">${esc(p.dir + (p.sub ? '/' + p.sub : ''))}</span>. Its output stays in Terminals; the page opens here when it listens.</p>
      <input class="fm-in br-runcmd mono" type="text" spellcheck="false" autocomplete="off" value="${esc(p.cmd)}">
      <div class="modal-actions"><button type="button" class="btn" data-cancel>Cancel</button><button type="submit" class="btn primary">${ic('play3')}Run</button></div>`,
    { init: (d) => setTimeout(() => $('.br-runcmd', d).focus(), 0), submit: () => $('.br-dlg .br-runcmd', BR.el).value.trim() || false });
    if (!cmd) return;
  }
  const before = new Set((await brLoadPorts(true) || []).map((x) => x.port));
  const r = await api('/api/browser/run', { dir: p.dir, sub: p.sub || '', cmd: p.kind === 'static' ? '' : cmd });
  if (r.status !== 200) return toast(r.error || 'It did not start', true);
  BR.projects = null;
  const fresh = !brIsBlank(brTabOf(BR.tab)?.url); // a project opens in a new tab, unless this one is empty
  if (r.port) { toast(`Serving ${p.name} on :${r.port}`); return brOpen(`http://localhost:${r.port}`, fresh); }
  toast(`Starting ${p.name}… the page opens when it listens`);
  // its port: the first new one listening from its pane (or its folder), checked for a minute
  for (let i = 0; i < 60; i++) {
    await new Promise((ok) => setTimeout(ok, 1000));
    const ps = await brLoadPorts(true) || [];
    const hit = ps.find((x) => !before.has(x.port) && ((x.pane && x.pane.session === r.session && x.pane.pane === r.pane) || (x.dir && (x.dir === r.dir || x.dir.startsWith(r.dir)))));
    if (hit) { brOpen(`http://localhost:${hit.port}`, fresh); return; }
  }
  toast(`${p.name} hasn't opened a port in a minute: look at its terminal (servers session)`, true);
}

// ---- viewports ----
function brViewportMenu(r) {
  const t = brShown();
  if (!t || t.src !== 'me') return;
  $('.br-vpm')?.remove();
  const vp = t.vp?.mode ? t.vp : { mode: 'fit', scheme: '', net: '' };
  const m = document.createElement('div');
  m.className = 'fm-menu br-vpm';
  m.innerHTML = `<button type="button" data-vp="fit" class="${vp.mode !== 'device' ? 'on' : ''}">${ic('fullscreen')}<span>Fit the window</span><kbd>${BR.stage.clientWidth}×${BR.stage.clientHeight}</kbd></button><hr>
    ${BR_DEVICES.map((d, i) => `<button type="button" data-vp="${i}" class="${vp.mode === 'device' && vp.name === d.name ? 'on' : ''}">${ic(d.icon)}<span>${esc(d.name)}</span><kbd>${d.w}×${d.h}</kbd></button>`).join('')}
    ${vp.mode === 'device' ? `<button type="button" data-vp="rotate">${ic('arrow-rotate')}<span>Rotate</span></button>` : ''}
    <hr><div class="br-vrow"><span>Colours</span><div class="seg sm">${[['', 'Auto'], ['light', 'Light'], ['dark', 'Dark']].map(([k, l]) => `<button type="button" data-scheme="${k}" aria-pressed="${(vp.scheme || '') === k}">${l}</button>`).join('')}</div></div>
    <div class="br-vrow"><span>Network</span><select class="br-net">${BR_NETS.map(([k, l]) => `<option value="${k}"${(vp.net || '') === k ? ' selected' : ''}>${l}</option>`).join('')}</select></div>`;
  document.body.append(m);
  const w = m.offsetWidth;
  m.style.left = Math.max(8, Math.min(r.left, innerWidth - w - 8)) + 'px';
  m.style.top = r.bottom + 6 + 'px';
  const set = (patch) => { const nv = { ...vp, ...patch }; brSend({ t: 'viewport', tab: t.id, vp: nv }); t.vp = nv; BR.lastSize = ''; brRender(); setTimeout(brWatch, 50); };
  m.addEventListener('click', (e) => {
    const b = e.target.closest('[data-vp],[data-scheme]');
    if (!b) return;
    if (b.dataset.scheme !== undefined) { set({ scheme: b.dataset.scheme }); for (const x of $$('[data-scheme]', m)) x.setAttribute('aria-pressed', String(x === b)); return; }
    const v = b.dataset.vp;
    close();
    if (v === 'fit') return set({ mode: 'fit', name: '', w: 0, h: 0, dpr: 0, mobile: false, ua: '' });
    if (v === 'rotate') return set({ w: vp.h, h: vp.w });
    const d = BR_DEVICES[+v];
    set({ mode: 'device', name: d.name, w: d.w, h: d.h, dpr: d.dpr, mobile: d.mobile, ua: d.mobile ? (d.name.startsWith('iPad') ? BR_UA.ipad : d.name.startsWith('iPhone') ? BR_UA.ios : BR_UA.android) : '' });
  });
  $('.br-net', m).addEventListener('change', (e) => set({ net: e.target.value }));
  const close = () => { m.remove(); removeEventListener('pointerdown', out, true); };
  const out = (e) => { if (!m.contains(e.target) && !e.target.closest('[data-act="viewport"]')) close(); };
  addEventListener('pointerdown', out, true);
}
// a device's width, by dragging its right edge (Custom)
(() => {
  addEventListener('pointerdown', (e) => {
    const g = e.target.closest?.('.br-grip');
    if (!g) return;
    const t = brShown();
    if (!t?.vp || t.vp.mode !== 'device') return;
    e.preventDefault();
    const x0 = e.clientX, w0 = t.vp.w, k = BR.scale || 1;
    document.body.classList.add('br-resizing');
    let last = 0;
    const mv = (ev) => {
      const w = Math.round(Math.max(240, Math.min(2560, w0 + ((ev.clientX - x0) * 2) / k)));
      if (w === t.vp.w || Date.now() - last < 60) return;
      last = Date.now();
      t.vp = { ...t.vp, w, name: 'Custom' };
      brSend({ t: 'viewport', tab: t.id, vp: t.vp });
      brFit();
    };
    const up = () => { removeEventListener('pointermove', mv); removeEventListener('pointerup', up); document.body.classList.remove('br-resizing'); brRender(); };
    addEventListener('pointermove', mv);
    addEventListener('pointerup', up);
  });
  addEventListener('dblclick', (e) => {
    if (!e.target.closest?.('.br-grip')) return;
    const t = brShown(), d = BR_DEVICES.find((x) => x.h === t?.vp?.h);
    if (d) { t.vp = { ...t.vp, w: d.w, name: d.name }; brSend({ t: 'viewport', tab: t.id, vp: t.vp }); brRender(); }
  });
})();

// ---- More ----
function brMore(r) {
  const t = brShown();
  const items = [
    { k: 'copy', l: 'Copy the address', i: 'copy' },
    t?.src === 'me' && t?.url && !brIsBlank(t.url) ? { k: 'newtab', l: 'Duplicate this tab', i: 'window' } : null,
    { k: 'shot', l: 'Save a screenshot', i: 'camera' },
    { k: 'hard', l: 'Reload without the cache', i: 'refresh' },
    { hr: 1 },
    { k: 'downloads', l: 'Downloads (in ~/Downloads)', i: 'download2' },
    brCoarse.matches ? { k: 'kb', l: 'Show the keyboard', i: 'keyboard' } : null,
  ];
  fmMenu(r.right, r.bottom, items, async (k) => {
    if (k === 'copy') { await navigator.clipboard?.writeText(t?.url || ''); toast('Copied the address'); }
    if (k === 'newtab') brNewTab(t.url);
    if (k === 'hard') brSend({ t: 'reload', tab: t?.id, hard: true });
    if (k === 'kb') BR.kb.focus();
    if (k === 'downloads') { setView('files'); setTimeout(() => window.fmGo?.('~/Downloads'), 300); }
    if (k === 'shot') {
      const s = await brStill(t);
      if (!s) return;
      const a = document.createElement('a');
      a.href = URL.createObjectURL(s.blob);
      a.download = `${(brHost(t.url) || 'page').replace(/[^\w.-]+/g, '-')}-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-')}.png`;
      a.click();
      setTimeout(() => URL.revokeObjectURL(a.href), 4000);
    }
  });
}

// a still of a tab, sharp (a fresh PNG capture, not the stream's JPEG); keep: the panel also saves it for an
// agent (meta.path), so it's never uploaded from here
function brStill(t, q, keep) {
  return new Promise((done) => {
    if (!t) return done(null);
    const timer = setTimeout(() => { BR.stillWait = null; toast("The page didn't answer", true); done(null); }, 15000);
    BR.stillWait = (meta, img) => { clearTimeout(timer); BR.stillWait = null; done({ meta, blob: img.length ? new Blob([img], { type: q > 0 ? 'image/jpeg' : 'image/png' }) : null }); };
    brSend({ t: 'shot', tab: t.id, q, keep });
  });
}

// ---- the page's own dialogs, file choosers, downloads ----
function brJSDialog(t) {
  const box = $('.br-jsd', BR.el);
  const d = t?.dialog;
  setHidden(box, !d);
  if (!d) { box.dataset.k = ''; return; }
  const k = t.id + d.type + d.message;
  if (box.dataset.k === k) return;
  box.dataset.k = k;
  const canDrive = brCanDrive(t);
  box.innerHTML = `<form class="br-jsf"><div class="br-jsh">${ic(d.type === 'alert' ? 'info-circle' : d.type === 'beforeunload' ? 'alert-triangle' : 'help-circle')}<b>${esc(brHost(d.url) || 'This page')} says</b></div>
    <p>${esc(d.message || (d.type === 'beforeunload' ? 'Leave this page? Changes you made may not be saved.' : ''))}</p>
    ${d.type === 'prompt' ? `<input class="fm-in" type="text" value="${esc(d.default)}">` : ''}
    <div class="modal-actions">${d.type !== 'alert' ? `<button type="button" class="btn" data-jsd="0">Cancel</button>` : ''}<button type="submit" class="btn primary"${canDrive ? '' : ' disabled'}>${d.type === 'beforeunload' ? 'Leave' : 'OK'}</button></div></form>`;
  const f = $('form', box);
  const answer = (ok) => brSend({ t: 'dialog', tab: t.id, ok, text: $('input', box)?.value || '' });
  f.addEventListener('submit', (e) => { e.preventDefault(); answer(true); });
  $('[data-jsd]', box)?.addEventListener('click', () => answer(false));
  setTimeout(() => ($('input', box) || $('[type=submit]', box)).focus(), 0);
}
function brChooser(t) {
  const ch = t?.chooser;
  if (!ch || BR.chooserFor === t.id || !brCanDrive(t)) { if (!ch) BR.chooserFor = ''; return; }
  BR.chooserFor = t.id;
  // a picker needs a click of yours (browsers only open one from a gesture): a bar over the page asks for it
  const box = $('.br-jsd', BR.el);
  box.hidden = false;
  box.dataset.k = 'chooser';
  box.innerHTML = `<div class="br-jsf"><div class="br-jsh">${ic('upload2')}<b>The page wants ${ch.mode === 'selectMultiple' ? 'files' : 'a file'}</b></div>
    <p class="muted">Pick from this device: it's uploaded to the VM and handed to the page.</p>
    <div class="modal-actions"><button type="button" class="btn" data-ch="0">Cancel</button><button type="button" class="btn primary" data-ch="1">${ic('upload2')}Choose…</button></div></div>`;
  const input = $('.br-files', BR.el);
  input.multiple = ch.mode === 'selectMultiple';
  let names = [];
  const done = (paths) => { brSend({ t: 'files', tab: t.id, ok: !!paths, paths: paths || [], names }); box.hidden = true; box.dataset.k = ''; };
  $('[data-ch="0"]', box).onclick = () => done(null);
  $('[data-ch="1"]', box).onclick = () => input.click();
  input.onchange = async () => {
    const files = [...input.files];
    input.value = '';
    if (!files.length) return done(null);
    names = files.map((f) => f.name);
    try {
      box.querySelector('p').textContent = `Uploading ${files.length > 1 ? files.length + ' files' : files[0].name}…`;
      const paths = [];
      for (const f of files) paths.push(await uploadFile(f));
      done(paths);
    } catch (err) { toast(err.message, true); done(null); }
  };
}
function brDownloads(ds) {
  BR.downloads = ds;
  for (const d of ds) {
    if (d.state === 'completed' && !BR.seenDl.has(d.guid)) {
      BR.seenDl.add(d.guid);
      if (Date.now() - d.t < 15000) toast(`Downloaded ${d.name} to ~/Downloads`);
    }
    if (d.state === 'canceled' && !BR.seenDl.has(d.guid)) { BR.seenDl.add(d.guid); }
  }
}

// ---- Problems: the drawer (problems, console with a prompt, network) ----
function brDevWire() {
  const el = BR.el, dev = $('.br-dev', el);
  if (BR.devH) dev.style.height = BR.devH + 'px';
  setHidden(dev, !BR.devOpen);
  $('.br-dev-rz', el).addEventListener('pointerdown', (e) => {
    e.preventDefault();
    const y0 = e.clientY, h0 = dev.offsetHeight;
    document.body.classList.add('br-resizing-y');
    const mv = (ev) => { dev.style.height = Math.max(120, Math.min(innerHeight * 0.75, h0 - (ev.clientY - y0))) + 'px'; };
    const up = () => { removeEventListener('pointermove', mv); removeEventListener('pointerup', up); document.body.classList.remove('br-resizing-y'); BR.devH = dev.offsetHeight; brSave(); brLayout(); };
    addEventListener('pointermove', mv);
    addEventListener('pointerup', up);
  });
  $('.br-filter input', el).addEventListener('input', () => brDevPaint());
  const ev = $('.br-eval', el), evi = $('input', ev);
  const hist = [];
  let hi = 0;
  ev.addEventListener('submit', (e) => {
    e.preventDefault();
    const x = evi.value.trim();
    if (!x || !brShown()) return;
    hist.push(x);
    hi = hist.length;
    evi.value = '';
    brSend({ t: 'eval', tab: brShown().id, text: x });
  });
  evi.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowUp' && hi > 0) { e.preventDefault(); evi.value = hist[--hi]; }
    if (e.key === 'ArrowDown') { e.preventDefault(); hi = Math.min(hist.length, hi + 1); evi.value = hist[hi] || ''; }
  });
}
function brDevToggle(on = !BR.devOpen) {
  BR.devOpen = on;
  brSave();
  setHidden($('.br-dev', BR.el), !on);
  const t = brShown();
  BR.dev = { tab: t?.id, cons: [], probs: [], nets: [], evals: [] };
  brSend({ t: 'dev', tab: t?.id, on });
  brRender();
  brDevPaint();
  brLayout();
}
function brDevData(m) {
  if (m.tab !== brShown()?.id) return;
  if (m.reset || BR.dev.tab !== m.tab) BR.dev = { tab: m.tab, cons: [], probs: [], nets: [], evals: BR.dev.tab === m.tab ? BR.dev.evals : [] };
  const d = BR.dev;
  const merge = (arr, xs, cap) => {
    for (const x of xs || []) { const i = arr.findIndex((y) => y.seq === x.seq); if (i >= 0) arr[i] = x; else arr.push(x); }
    if (arr.length > cap) arr.splice(0, arr.length - cap);
  };
  merge(d.cons, m.cons, 500);
  merge(d.probs, m.probs, 300);
  // a request's entry is replaced as it goes (sent, answered, finished): keyed by URL+time
  for (const n of m.nets || []) { const i = d.nets.findIndex((y) => y.t === n.t && y.url === n.url && y.method === n.method); if (i >= 0) d.nets[i] = n; else d.nets.push(n); }
  if (d.nets.length > 400) d.nets.splice(0, d.nets.length - 400);
  clearTimeout(BR.devT);
  BR.devT = setTimeout(brDevPaint, 60);
}
// where a message came from, as DevTools says it: the file's name (the page itself is "(index)") and the line
const brSrc = (src) => { const m = /^(.*?):(\d+)(?::\d+)?$/.exec(src || ''); if (!m) return src || ''; let f = m[1]; try { const u = new URL(f); f = u.pathname.split('/').pop() || '(index)'; } catch { f = f.split('/').pop(); } return `${f}:${m[2]}`; };
const brTime = (ms) => new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
const brSize = (n) => (n < 1024 ? n + ' B' : n < 1 << 20 ? (n / 1024).toFixed(1) + ' kB' : (n / 1048576).toFixed(1) + ' MB');
function brDevPaint() {
  if (!BR.el || !BR.devOpen) return;
  const d = BR.dev, el = BR.el;
  for (const b of $$('[data-dp]', el)) b.setAttribute('aria-pressed', String(b.dataset.dp === BR.devPane));
  const counts = { probs: d.probs.length, cons: d.cons.length, nets: d.nets.filter((n) => n.err || n.status >= 400).length };
  for (const b of $$('.br-dn', el)) { const n = counts[b.dataset.n]; setText(b, n ? String(n) : ''); b.classList.toggle('bad', b.dataset.n !== 'cons' && n > 0); }
  setHidden($('.br-eval', el), BR.devPane !== 'cons' || !brShown());
  const q = $('.br-filter input', el).value.trim().toLowerCase();
  const f = (x) => !q || JSON.stringify(x).toLowerCase().includes(q);
  const body = $('.br-dev-b', el);
  const atEnd = body.scrollTop + body.clientHeight >= body.scrollHeight - 30;
  let html = '';
  if (BR.devPane === 'nets') {
    const ns = d.nets.filter(f);
    html = ns.length ? `<div class="br-net-h"><span>Status</span><span>Method</span><span>Name</span><span>Type</span><span>Size</span><span>Time</span></div>` +
      ns.map((n) => { const u = (() => { try { const x = new URL(n.url); return x.pathname + x.search; } catch { return n.url; } })();
        return `<div class="br-netr${n.err || n.status >= 400 ? ' bad' : ''}${n.done ? '' : ' wait'}" title="${esc(n.url)}${n.err ? '\n' + esc(n.err) : ''}"><span class="br-ns">${n.err ? '✕' : n.status || '…'}</span><span>${esc(n.method)}</span><span class="br-nu">${esc(u || n.url)}<i>${esc(brHost(n.url))}</i></span><span>${esc(n.type || '')}</span><span>${n.done ? brSize(n.size) : ''}</span><span>${n.ms ? Math.round(n.ms) + ' ms' : ''}</span></div>`; }).join('')
      : '<p class="faint br-none">No requests yet. They show as the page loads.</p>';
  } else {
    const xs = (BR.devPane === 'probs' ? d.probs : [...d.cons, ...d.evals.map((e) => ({ ...e.out, kind: 'eval', expr: e.expr, t: e.at, seq: 1e12 + d.evals.indexOf(e) }))]).filter(f);
    if (BR.devPane === 'cons') xs.sort((a, b) => (a.kind === 'eval') - (b.kind === 'eval') || a.seq - b.seq);
    html = xs.length ? xs.map((p) => `<div class="br-log ${p.level}${p.kind === 'eval' ? ' eval' : ''}">` +
      (p.kind === 'eval' ? `<span class="br-ex">› ${esc(p.expr)}</span>` : '') +
      `<span class="br-li">${ic(p.level === 'error' ? 'x-circle-f' : p.level === 'warning' ? 'alert-triangle' : p.kind === 'eval' ? 'arrow-left' : 'info-circle')}</span>` +
      `<span class="br-lt">${p.kind === 'http' || p.kind === 'net' ? `<b class="br-lk">${p.kind === 'http' ? 'HTTP' : 'Network'}</b> ` : p.kind === 'exception' ? '<b class="br-lk">Uncaught</b> ' : ''}${esc(p.text)}${p.stack ? `<pre>${esc(p.stack)}</pre>` : ''}</span>` +
      `<span class="br-ls" title="${esc(p.src || '')}">${esc(p.kind === 'http' || p.kind === 'net' ? p.src || '' : brSrc(p.src))}</span>${p.t ? `<span class="br-ltm">${brTime(p.t)}</span>` : ''}</div>`).join('')
      : `<p class="faint br-none">${BR.devPane === 'probs' ? 'No problems: no errors or warnings in the console, no exceptions, no failed requests.' : 'The console is empty.'}</p>`;
  }
  if (body.dataset.html !== html) { body.innerHTML = html; body.dataset.html = html; if (atEnd) body.scrollTop = body.scrollHeight; }
}
function brDevSend() {
  const t = brShown(), d = BR.dev;
  const probs = d.probs.slice(-40).map((p) => `- [${p.kind}${p.level === 'warning' ? ' warning' : ''}] ${p.text}${p.src ? ` (${p.src})` : ''}${p.stack ? '\n' + p.stack.split('\n').slice(0, 4).join('\n') : ''}`);
  const failed = d.nets.filter((n) => n.err || n.status >= 400).slice(-20).map((n) => `- ${n.method} ${n.url} → ${n.err || n.status}`);
  const text = `Problems on ${t?.title || ''} (${t?.url || ''}), from the panel's browser:\n\n${probs.join('\n') || '(none in the console)'}${failed.length ? '\n\nFailed requests:\n' + failed.join('\n') : ''}`;
  brSendSheet({ text, title: 'Send the problems to', files: [] });
}

// ---- DevTools: Chromium's own, docked beside the page (browserdevtools.go); its selected element can go to an agent ----
function brDT(on) {
  BR.dtOpen = on;
  brSave();
  brDTRender();
  brLayout();
}
function brDTRender() {
  const dt = $('.br-dt', BR.el), rz = $('.br-dtr', BR.el), work = $('.br-work', BR.el);
  if (!dt) return;
  const show = BR.dtOpen && !BR.rp && !(BR.mode === 'agents' && BR.wall);
  setHidden(dt, !show);
  setHidden(rz, !show);
  const bottom = BR.dtSide === 'bottom' || BR.narrow;
  work.classList.toggle('bottom', bottom);
  $('[data-act="devtools"]', BR.el)?.setAttribute('aria-pressed', String(!!show));
  if (!show) { if (BR.dtFor !== undefined) { $('.br-dtb', dt).innerHTML = ''; BR.dtFor = undefined; } return; }
  const size = bottom ? BR.dtH : BR.dtW;
  dt.style.flexBasis = size ? size + 'px' : bottom ? '45%' : '42%';
  const t = brShown();
  const ok = !!t && brCanDrive(t) && !!BR.dtv;
  const want = ok ? t.id : '';
  if (want === BR.dtFor) return;
  BR.dtFor = want;
  BR.dtSel = '';
  brDTSel();
  const body = $('.br-dtb', dt);
  if (!ok) {
    body.innerHTML = `<p class="br-dtn faint">${!t ? 'Open a page to inspect it.' : t.src !== 'me' && !brCanDrive(t) ? "Take over this agent's browser to inspect it: DevTools can change the page." : 'Starting…'}</p>`;
    return;
  }
  try { localStorage.setItem('screencast-enabled', 'false'); } catch {} // its own picture of the page (the panel shows it): off (its setting, in a hosted DevTools' storage)
  const ws = `${location.protocol === 'https:' ? 'wss' : 'ws'}=${location.host}/api/browser/devtools/ws/${encodeURIComponent(t.id)}`;
  body.innerHTML = `<iframe class="br-dtf" title="DevTools" src="/api/browser/devtools/f/${encodeURIComponent(BR.dtv)}/inspector.html?${ws}"></iframe>`;
}
function brDTSel() {
  const el = $('.br-dtsel', BR.el);
  if (!el) return;
  setText(el, BR.dtSel || 'Select an element in Elements');
  el.classList.toggle('faint', !BR.dtSel);
  $('.br-dtsend', BR.el).disabled = !BR.dtSel;
}
function brDTWire() {
  const dt = $('.br-dt', BR.el), rz = $('.br-dtr', BR.el), work = $('.br-work', BR.el);
  rz.addEventListener('pointerdown', (e) => {
    e.preventDefault();
    const bottom = work.classList.contains('bottom'), r = work.getBoundingClientRect();
    // the frame would take the pointer's moves: a cover over everything while it drags
    document.body.classList.add(bottom ? 'br-resizing-y' : 'br-resizing');
    const mv = (ev) => {
      const v = bottom ? Math.min(r.height - 120, Math.max(160, r.bottom - ev.clientY)) : Math.min(r.width - 240, Math.max(300, r.right - ev.clientX));
      dt.style.flexBasis = v + 'px';
      if (bottom) BR.dtH = v; else BR.dtW = v;
    };
    const up = () => { removeEventListener('pointermove', mv); removeEventListener('pointerup', up); document.body.classList.remove('br-resizing', 'br-resizing-y'); brSave(); brLayout(); };
    addEventListener('pointermove', mv);
    addEventListener('pointerup', up);
  });
  rz.addEventListener('dblclick', () => { BR.dtW = BR.dtH = 0; brSave(); brDTRender(); brLayout(); });
  dt.addEventListener('click', (e) => {
    const k = e.target.closest('[data-dt]')?.dataset.dt;
    const t = brShown();
    if (k === 'close') brDT(false);
    if (k === 'side') { BR.dtSide = BR.dtSide === 'bottom' ? 'right' : 'bottom'; brSave(); brDTRender(); brLayout(); }
    if (k === 'reload') { BR.dtFor = undefined; brDTRender(); }
    if (k === 'pick' && t) brSend({ t: 'dtpick', tab: t.id });
    if (k === 'send' && BR.dtFor) brSend({ t: 'dtsel', tab: BR.dtFor });
  });
}
// the page's right-click: the browser's menu (the page's own is one of its items)
function brCtxMenu(e) {
  e.preventDefault();
  const t = brShown(), p = brXY(e);
  if (!t || !p || BR.ann) return;
  const drive = brCanDrive(t);
  fmMenu(e.clientX, e.clientY, [
    drive && { k: 'back', l: 'Back', i: 'arrow-left' }, drive && { k: 'fwd', l: 'Forward', i: 'arrow-right' }, drive && { k: 'reload', l: 'Reload', i: 'refresh' }, { hr: 1 },
    drive && { k: 'inspect', l: 'Inspect', i: 'code', kbd: brMac ? '⌥⌘I' : 'F12' },
    drive && { k: 'send', l: 'Send this element to an agent', i: 'direct-send' },
    { k: 'annotate', l: 'Annotate', i: 'pen-tool', kbd: 'A' }, { hr: 1 },
    drive && { k: 'page', l: "The page's own right-click", i: 'pointer' },
  ], (k) => {
    if (k === 'back' || k === 'fwd' || k === 'reload') return brSend({ t: k, tab: t.id });
    if (k === 'inspect') { if (!BR.dtOpen) brDT(true); return brSend({ t: 'dtreveal', tab: t.id, x: p.x, y: p.y }); }
    if (k === 'send') { allPanes(); const seq = ++BR.probeSeq; BR.probes.set(seq, (info) => brPicked(info)); return brSend({ t: 'probe', tab: t.id, x: p.x, y: p.y, seq }); }
    if (k === 'annotate') return brAnnotate();
    if (k === 'page') for (const ty of ['move', 'down', 'up']) brSend({ t: 'mouse', tab: t.id, type: ty, x: p.x, y: p.y, btn: 'right', btns: ty === 'down' ? 2 : 0, n: 1, mods: 0 });
  }, true);
}
// a picture of an element, from what's on screen (its box, in the page's CSS px, and a little around it)
function brCrop(rect) {
  const m = BR.meta, c = BR.canvas;
  if (!m || !rect || !c.width) return Promise.resolve(null);
  const k = c.width / m.dw, pad = 8;
  const x = Math.max(0, (rect.x - pad) * k), y = Math.max(0, (rect.y - pad) * k);
  const w = Math.min(c.width - x, (rect.w + 2 * pad) * k), h = Math.min(c.height - y, (rect.h + 2 * pad) * k);
  if (w < 4 || h < 4) return Promise.resolve(null);
  const o = document.createElement('canvas');
  o.width = Math.round(w);
  o.height = Math.round(h);
  o.getContext('2d').drawImage(c, x, y, w, h, 0, 0, o.width, o.height);
  return new Promise((ok) => o.toBlob(ok, 'image/png'));
}
async function brPicked(info) {
  if (!info || info.error) return toast(info?.error || 'Nothing there', true);
  const t = brShown();
  let text = `An element on ${t?.title || ''} (${t?.url || ''}):\n\n${brElementText(info)}`;
  if (info.rules?.length) text += `\n   CSS rules on it (most specific first):\n${info.rules.map((r) => '     ' + r).join('\n')}`;
  const shot = await brCrop(info.rect);
  const files = shot ? [brUp(new File([shot], `element-${info.tag || 'node'}.png`, { type: 'image/png' }))] : [];
  brSendSheet({ text, title: `Send <${info.tag}> to`, files, preview: shot ? URL.createObjectURL(shot) : undefined, note: true });
}
function brElementText(info, n) {
  if (!info || info.error) return '(no element there)';
  const st = Object.entries(info.styles || {}).map(([k, v]) => `${k}: ${v}`).join('; ');
  return `${n ? n + '. ' : ''}\`${info.selector}\`${info.text ? ` "${info.text}"` : ''}${info.component ? ` (component ${info.component})` : ''} at ${info.rect.x},${info.rect.y} ${info.rect.w}×${info.rect.h}\n   ${info.html.replace(/\n\s*/g, ' ')}\n   styles: ${st}`;
}

// ---- Annotate: a still of the page, drawn on, sent with the elements under the pins ----
async function brAnnotate() {
  const t = brShown();
  if (!t || BR.ann || !BR.meta) return;
  // what's on screen, at once: a capture from the VM costs a second or more (a 2x PNG, then the tunnel)
  const still = document.createElement('canvas');
  still.className = 'br-still';
  still.width = BR.canvas.width;
  still.height = BR.canvas.height;
  still.getContext('2d').drawImage(BR.canvas, 0, 0);
  const a = BR.ann = { t, meta: { ...BR.meta, url: t.url, title: t.title, vp: t.vp, probs: t.probs }, still, tool: 'pin', color: BR_INK[1], marks: [], undo: [] };
  allPanes(); // who you can send it to, ready when you are
  // where it's scrolled to, and the clean picture an agent gets, kept by the panel; a page that was moving (so
  // 1x on screen) also gets that sharp capture under your marks
  const soft = still.width < BR.meta.dw * Math.min(devicePixelRatio || 1, 2) - 1;
  a.shot = brStill(t, soft ? 85 : -1, true).then(async (s) => {
    if (!s || BR.ann !== a) return;
    Object.assign(a.meta, { sx: s.meta.sx, sy: s.meta.sy });
    a.path = s.meta.path;
    const bmp = s.blob && (await createImageBitmap(s.blob).catch(() => null));
    if (!bmp || BR.ann !== a) return;
    still.width = bmp.width;
    still.height = bmp.height;
    still.getContext('2d').drawImage(bmp, 0, 0);
    bmp.close();
  });
  const box = $('.br-ann', BR.el);
  box.hidden = false;
  box.innerHTML = `<canvas class="br-ink"></canvas>
    <div class="br-pins"></div>
    <div class="br-tools" role="toolbar" aria-label="Annotate">
      <span class="br-tgrip" title="Drag to move (double-click: back to the middle)"></span>
      ${[['pin', 'comment', 'Pin a comment to an element'], ['pen', 'brush', 'Draw'], ['arrow', 'arrow-up-right', 'Arrow'], ['box', 'br-rect', 'Box'], ['text', 'text', 'Text']]
        .map(([k, i, l]) => `<button type="button" data-tool="${k}" title="${l}" aria-label="${l}"${k === 'pin' ? ' aria-pressed="true"' : ''}>${ic(i)}</button>`).join('')}
      <span class="br-tsep"></span>
      ${BR_INK.map((c, i) => `<button type="button" class="br-swatch" data-ink="${c}" aria-label="Colour ${i + 1}"${i === 1 ? ' aria-pressed="true"' : ''}><i></i></button>`).join('')}
      <span class="br-tsep"></span>
      <button type="button" data-ann="undo" title="Undo (⌘Z)" aria-label="Undo">${ic('undo')}</button>
      <button type="button" data-ann="clear" title="Clear" aria-label="Clear">${ic('eraser')}</button>
      <span class="br-tsep"></span>
      <button type="button" data-ann="copy" title="Copy the image" aria-label="Copy the image">${ic('copy')}</button>
      <button type="button" class="br-send" data-ann="send">${ic('direct-send')}<span>Send to agent</span></button>
      <button type="button" data-ann="close" title="Done (Esc)" aria-label="Close">${IC.x}</button>
    </div>
    <div class="br-annhint">${ic('comment')}<span>Click an element to pin a comment, or draw. Then <b>Send to agent</b>.</span></div>`;
  box.prepend(still);
  BR.el.classList.add('annotating');
  brAnnFit();
  brAnnWire(box);
  brRender();
}
function brAnnFit() {
  const a = BR.ann;
  if (!a) return;
  const box = $('.br-ann', BR.el);
  const r = BR.canvas.getBoundingClientRect(), fr = $('.br-dev-frame', BR.el).getBoundingClientRect();
  const ink = $('.br-ink', box);
  a.w = r.width; a.h = r.height;
  box.style.left = r.left - fr.left + 'px';
  box.style.top = r.top - fr.top + 'px';
  box.style.width = r.width + 'px';
  box.style.height = r.height + 'px';
  const k = devicePixelRatio || 1;
  ink.width = Math.round(r.width * k);
  ink.height = Math.round(r.height * k);
  brAnnTools();
  brAnnPaint();
  brAnnPins();
}
// the toolbar: over the stage's top, centred, or where you dragged it (kept on screen)
function brAnnTools() {
  const tools = $('.br-tools', BR.el), at = BR.toolsAt;
  if (!tools) return;
  if (!at) {
    const sr = BR.stage.getBoundingClientRect();
    tools.style.transform = '';
    tools.style.left = sr.left + sr.width / 2 + 'px';
    tools.style.top = sr.top + 12 + 'px';
    return;
  }
  tools.style.transform = 'none';
  tools.style.left = Math.max(4, Math.min(innerWidth - tools.offsetWidth - 4, at.x)) + 'px';
  tools.style.top = Math.max(4, Math.min(innerHeight - tools.offsetHeight - 4, at.y)) + 'px';
}
// marks are kept in the page's CSS px (meta.dw × meta.dh): they survive a resize and map onto the still's pixels
function brAnnWire(box) {
  const a = BR.ann, ink = $('.br-ink', box);
  let r = null, cur = null, raf = 0;
  const pt = (e) => ({ x: (e.clientX - r.left) * (a.meta.dw / r.width), y: (e.clientY - r.top) * (a.meta.dh / r.height) });
  const paint = () => { raf ||= requestAnimationFrame(() => { raf = 0; brAnnPaint(); }); };
  ink.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    r = ink.getBoundingClientRect();
    const p = pt(e);
    if (a.tool === 'pin') return brAnnPin(p);
    if (a.tool === 'text') return brAnnText(p);
    ink.setPointerCapture(e.pointerId);
    cur = { kind: a.tool, color: a.color, pts: [p] };
    a.marks.push(cur);
  });
  ink.addEventListener('pointermove', (e) => {
    if (!cur) return;
    if (cur.kind === 'pen') for (const x of e.getCoalescedEvents?.() || [e]) cur.pts.push(pt(x)); // every point the pointer passed, not one a frame
    else cur.pts[1] = pt(e);
    paint();
  });
  ink.addEventListener('pointerup', () => {
    if (!cur) return;
    if (cur.pts.length < 2 || (cur.kind !== 'pen' && Math.hypot(cur.pts[1].x - cur.pts[0].x, cur.pts[1].y - cur.pts[0].y) < 4)) a.marks.pop();
    else {
      brAnnProbe(cur);
      if (cur.kind === 'box') brAnnNote(cur); // a box can carry a comment too
    }
    cur = null;
    brAnnPaint();
  });
  const tools = $('.br-tools', box);
  tools.addEventListener('pointerdown', (e) => { // dragged by its grip or anywhere between its buttons
    if (e.button !== 0 || e.target.closest('button')) return;
    e.preventDefault();
    const tr = tools.getBoundingClientRect(), dx = e.clientX - tr.left, dy = e.clientY - tr.top;
    tools.setPointerCapture(e.pointerId);
    tools.classList.add('dragging');
    const move = (ev) => { BR.toolsAt = { x: ev.clientX - dx, y: ev.clientY - dy }; brAnnTools(); };
    tools.addEventListener('pointermove', move);
    tools.addEventListener('lostpointercapture', () => { tools.removeEventListener('pointermove', move); tools.classList.remove('dragging'); }, { once: true });
  });
  tools.addEventListener('dblclick', (e) => { if (!e.target.closest('button')) { BR.toolsAt = null; brAnnTools(); } }); // the capture makes it the toolbar's
  box.addEventListener('click', async (e) => {
    const tool = e.target.closest('[data-tool]');
    if (tool) { a.tool = tool.dataset.tool; for (const b of $$('[data-tool]', box)) b.setAttribute('aria-pressed', String(b === tool)); ink.dataset.tool = a.tool; return; }
    const sw = e.target.closest('[data-ink]');
    if (sw) { a.color = sw.dataset.ink; for (const b of $$('[data-ink]', box)) b.setAttribute('aria-pressed', String(b === sw)); return; }
    const act = e.target.closest('[data-ann]')?.dataset.ann;
    if (act === 'undo') { a.marks.pop(); brAnnPaint(); brAnnPins(); }
    if (act === 'clear') { a.marks = []; brAnnPaint(); brAnnPins(); }
    if (act === 'close') brAnnClose();
    if (act === 'copy') {
      const blob = await brAnnCompose();
      try { await navigator.clipboard.write([new ClipboardItem({ 'image/png': blob })]); toast('Copied the annotated screenshot'); } catch { toast("This browser can't copy images: use Send or Save", true); }
    }
    if (act === 'send') brAnnSend();
  });
  ink.dataset.tool = a.tool;
  for (const b of $$('.br-swatch', box)) b.style.setProperty('--c', b.dataset.ink);
}
function brAnnPaint() {
  const a = BR.ann;
  if (!a) return;
  const ink = $('.br-ink', BR.el), ctx = ink.getContext('2d');
  ctx.setTransform(1, 0, 0, 1, 0, 0);
  ctx.clearRect(0, 0, ink.width, ink.height);
  const k = ink.width / a.meta.dw;
  brInk(ctx, a.marks, k);
}
// strokes, arrows, boxes and text (pins are DOM while you edit, and drawn into the image when it's made)
function brInk(ctx, marks, k, withPins) {
  ctx.lineCap = 'round';
  ctx.lineJoin = 'round';
  for (const [i, m] of marks.entries()) {
    ctx.strokeStyle = ctx.fillStyle = m.color;
    ctx.lineWidth = 3.5 * k;
    const p = m.pts.map((q) => ({ x: q.x * k, y: q.y * k }));
    if (m.kind === 'pen') { ctx.beginPath(); p.forEach((q, i) => (i ? ctx.lineTo(q.x, q.y) : ctx.moveTo(q.x, q.y))); ctx.stroke(); }
    if (m.kind === 'box') { ctx.strokeRect(Math.min(p[0].x, p[1].x), Math.min(p[0].y, p[1].y), Math.abs(p[1].x - p[0].x), Math.abs(p[1].y - p[0].y)); }
    if (m.kind === 'arrow') {
      const [s, e] = p, ang = Math.atan2(e.y - s.y, e.x - s.x), h = 16 * k;
      ctx.beginPath(); ctx.moveTo(s.x, s.y); ctx.lineTo(e.x, e.y); ctx.stroke();
      ctx.beginPath(); ctx.moveTo(e.x, e.y); ctx.lineTo(e.x - h * Math.cos(ang - 0.45), e.y - h * Math.sin(ang - 0.45)); ctx.lineTo(e.x - h * Math.cos(ang + 0.45), e.y - h * Math.sin(ang + 0.45)); ctx.closePath(); ctx.fill();
    }
    if (m.kind === 'text' && m.text) {
      ctx.font = `600 ${16 * k}px system-ui, sans-serif`;
      const w = ctx.measureText(m.text).width;
      ctx.fillStyle = 'rgba(10,12,15,.78)';
      ctx.fillRect(p[0].x - 6 * k, p[0].y - 18 * k, w + 12 * k, 26 * k);
      ctx.fillStyle = m.color;
      ctx.fillText(m.text, p[0].x, p[0].y);
    }
    // its number, which the text an agent gets refers to (pins and boxes are DOM while you edit)
    if (withPins || (m.kind !== 'pin' && m.kind !== 'box')) {
      const q = brAnnAt(m), r = 13 * k, W = ctx.canvas.width, H = ctx.canvas.height;
      const at = { x: Math.max(r, Math.min(W - r, q.x * k)), y: Math.max(r, Math.min(H - r, q.y * k)) }; // all of it in the picture
      ctx.beginPath(); ctx.arc(at.x, at.y, r, 0, Math.PI * 2); ctx.fillStyle = m.color; ctx.fill();
      ctx.lineWidth = 2.5 * k; ctx.strokeStyle = '#0a0c0f'; ctx.stroke();
      ctx.fillStyle = '#0a0c0f'; ctx.font = `700 ${14 * k}px system-ui, sans-serif`; ctx.textAlign = 'center'; ctx.textBaseline = 'middle';
      ctx.fillText(String(i + 1), at.x, at.y + 0.5 * k);
      ctx.textAlign = 'start'; ctx.textBaseline = 'alphabetic';
    }
    if (withPins && m.note) { // its comment, beside the number
      const q = brAnnAt(m), at = { x: q.x * k, y: q.y * k }, r = 13 * k;
      ctx.font = `600 ${13 * k}px system-ui, sans-serif`;
      const lines = m.note.split('\n').slice(0, 4).map((l) => (l.length > 60 ? l.slice(0, 59) + '…' : l));
      const w = Math.max(...lines.map((l) => ctx.measureText(l).width)) + 16 * k;
      let bx = at.x + r + 6 * k, by = at.y - 12 * k;
      if (bx + w > ctx.canvas.width) bx = at.x - r - 6 * k - w;
      ctx.fillStyle = 'rgba(10,12,15,.88)';
      ctx.beginPath(); ctx.roundRect(bx, by, w, (lines.length * 17 + 9) * k, 6 * k); ctx.fill();
      ctx.fillStyle = '#fff';
      lines.forEach((l, j) => ctx.fillText(l, bx + 8 * k, by + (17 + j * 17) * k));
    }
  }
}
// a pin: a number on the element under it, with a comment; the element is described by the page (probe)
function brAnnPin(p) {
  const a = BR.ann;
  const m = { kind: 'pin', color: a.color, pts: [p], note: '', info: null };
  a.marks.push(m);
  brAnnProbe(m);
  brAnnPins(m);
}
// a mark's corners, in page CSS px
const brBox = (pts) => {
  const xs = pts.map((q) => q.x), ys = pts.map((q) => q.y), x = Math.min(...xs), y = Math.min(...ys);
  return { x, y, w: Math.max(...xs) - x, h: Math.max(...ys) - y };
};
// where a mark's number goes: on a pin, an arrow's tail, before a text, a box's top-left corner, a freehand
// mark's top (on its line: a circle's corner is off it)
const brAnnAt = (m) => (m.kind === 'pin' || m.kind === 'arrow' ? m.pts[0] : m.kind === 'text' ? { x: m.pts[0].x - 16, y: m.pts[0].y - 6 }
  : m.kind === 'pen' ? m.pts.reduce((a, q) => (q.y < a.y ? q : a)) : brBox(m.pts));
// what's under a mark, asked of the page: the element at a pin, an arrow's head or a text; the element a box or a
// freehand mark frames, and the text inside it
function brAnnProbe(m) {
  const a = BR.ann, seq = ++BR.probeSeq;
  m.wait = new Promise((ok) => BR.probes.set(seq, (info) => { m.info = info; ok(); if (m.kind === 'pin') brAnnLabel(m); })); // in place: the comment you're typing keeps its focus
  if (m.kind === 'pin' || m.kind === 'arrow' || m.kind === 'text') {
    const p = m.pts[m.kind === 'arrow' ? 1 : 0];
    return brSend({ t: 'probe', tab: a.t.id, x: p.x, y: m.kind === 'text' ? p.y - 6 : p.y, seq });
  }
  const b = brBox(m.pts);
  brSend({ t: 'area', tab: a.t.id, x: b.x, y: b.y, w: Math.max(1, Math.round(b.w)), h: Math.max(1, Math.round(b.h)), seq });
}
function brAnnNote(m) { m.note = ''; brAnnPins(m); }
function brAnnText(p) {
  const a = BR.ann;
  const m = { kind: 'text', color: a.color, pts: [p], text: '' };
  const box = $('.br-pins', BR.el);
  const i = document.createElement('input');
  i.className = 'br-tin';
  i.placeholder = 'Text';
  const k = a.w / a.meta.dw;
  i.style.left = p.x * k + 'px';
  i.style.top = p.y * k - 18 + 'px';
  i.style.color = a.color;
  box.append(i);
  setTimeout(() => i.focus(), 0);
  let over = false; // Enter, then the blur its removal causes: once
  const done = () => { if (over) return; over = true; if (i.value.trim()) { m.text = i.value.trim(); a.marks.push(m); brAnnProbe(m); brAnnPaint(); } i.remove(); };
  i.addEventListener('keydown', (e) => { if (e.key === 'Enter') done(); if (e.key === 'Escape') { e.stopPropagation(); over = true; i.remove(); } });
  i.addEventListener('blur', done);
}
const brAnnInfo = (m) => (m.info && !m.info.error ? `${esc(m.info.selector.split(' > ').slice(-2).join(' > '))}${m.info.text ? ` · “${esc(m.info.text.slice(0, 40))}”` : ''}` : m.info?.error ? esc(m.info.error) : 'finding the element…');
function brAnnLabel(m) {
  const el = m.el?.querySelector('.br-pel');
  if (!el) return;
  el.innerHTML = brAnnInfo(m);
  el.classList.toggle('faint', !m.info || !!m.info.error);
}
function brAnnPins(edit) {
  const a = BR.ann;
  if (!a) return;
  const box = $('.br-pins', BR.el);
  for (const x of $$('.br-pin', box)) x.remove();
  const k = a.w / a.meta.dw;
  for (const [i, m] of a.marks.entries()) {
    if (m.kind !== 'pin' && !(m.kind === 'box' && m.note !== undefined)) continue;
    const n = i + 1, at = brAnnAt(m);
    const el = document.createElement('div');
    el.className = 'br-pin' + (m === edit ? ' edit' : '');
    el.style.left = at.x * k + 'px';
    el.style.top = at.y * k + 'px';
    el.style.setProperty('--c', m.color);
    const info = m.kind === 'pin' ? `<span class="br-pel mono${m.info && !m.info.error ? '' : ' faint'}">${brAnnInfo(m)}</span>` : '';
    el.innerHTML = `<b>${n}</b><div class="br-pc-b">${info}<textarea rows="2" placeholder="What should change here?">${esc(m.note || '')}</textarea><div class="br-pc-a"><button type="button" class="icon-btn ghost sm" data-del aria-label="Remove">${ic('trash2')}</button><button type="button" class="btn sm" data-ok>Done</button></div></div>`;
    box.append(el);
    m.el = el;
    const ta = $('textarea', el);
    ta.addEventListener('input', () => { m.note = ta.value; });
    ta.addEventListener('keydown', (e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); el.classList.remove('edit'); } if (e.key === 'Escape') { e.stopPropagation(); el.classList.remove('edit'); } });
    $('b', el).addEventListener('click', () => { for (const x of $$('.br-pin', box)) x.classList.toggle('edit', x === el && !x.classList.contains('edit')); if (el.classList.contains('edit')) ta.focus(); });
    $('[data-ok]', el).addEventListener('click', () => el.classList.remove('edit'));
    $('[data-del]', el).addEventListener('click', () => { a.marks.splice(a.marks.indexOf(m), 1); brAnnPaint(); brAnnPins(); });
    if (m === edit) setTimeout(() => ta.focus(), 0);
  }
  setHidden($('.br-annhint', BR.el), a.marks.length > 0);
}
// a clipboard takes only PNG; an agent gets a JPEG no wider than it reads (a 2x PNG of a photo is MBs to upload)
async function brAnnCompose(type = 'image/png', maxW = Infinity) {
  const a = BR.ann;
  const c = document.createElement('canvas'), z = Math.min(1, maxW / a.still.width);
  c.width = Math.round(a.still.width * z);
  c.height = Math.round(a.still.height * z);
  const ctx = c.getContext('2d');
  ctx.drawImage(a.still, 0, 0, c.width, c.height);
  brInk(ctx, a.marks, c.width / a.meta.dw, true);
  return new Promise((ok) => c.toBlob(ok, type, 0.9));
}
function brAnnClose() {
  const a = BR.ann;
  if (!a) return;
  BR.ann = null;
  const box = $('.br-ann', BR.el);
  box.hidden = true;
  box.innerHTML = '';
  BR.el.classList.remove('annotating');
  brRender();
  if (BR.q?.length && !BR.pumping) brPump(); // the stream, paused while you drew
}
async function brAnnSend() {
  const a = BR.ann;
  if (a.sending) return;
  a.sending = true;
  const t = a.t, vp = a.meta.vp || {};
  // what's under the marks, and the panel's clean picture: here by now, almost always
  await Promise.race([Promise.all([a.shot, ...a.marks.map((m) => m.wait)]), sleep(2500)]);
  const lines = [];
  lines.push(`Look at this page in the panel's browser: ${a.meta.title || ''} — ${a.meta.url}`);
  lines.push(`Viewport ${Math.round(a.meta.dw)}×${Math.round(a.meta.dh)}${vp.mode === 'device' ? ` (${vp.name}, ${vp.dpr}×${vp.mobile ? ', mobile' : ''})` : ''}${vp.scheme ? `, ${vp.scheme} scheme` : ''}${vp.net ? `, ${vp.net} network` : ''}, scrolled to ${Math.round(a.meta.sx || 0)},${Math.round(a.meta.sy || 0)}.`);
  if (a.marks.length) {
    lines.push('', 'The numbered marks on the screenshot (positions in the page\'s CSS px, in the viewport):');
    a.marks.forEach((m, i) => lines.push(brAnnMarkText(m, i + 1)));
  }
  const p = a.meta.probs || [0, 0, 0];
  if (p[0] + p[1] + p[2]) {
    lines.push('', `The page's console has ${p[0]} error${p[0] === 1 ? '' : 's'}, ${p[1]} warning${p[1] === 1 ? '' : 's'} and ${p[2]} failed request${p[2] === 1 ? '' : 's'} since it loaded.`);
    const d = BR.dev.tab === t.id ? BR.dev.probs.slice(-8) : [];
    for (const x of d) lines.push(`- ${x.text.split('\n')[0].slice(0, 300)}${x.src ? ` (${x.src})` : ''}`);
  }
  lines.push('', 'The first image is the annotated screenshot, the second the page as it is.');
  const composed = await brAnnCompose('image/jpeg', 1600);
  const stamp = new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-');
  const clean = a.path ? { name: `page-${stamp}.jpg`, path: a.path, img: true }
    : brUp(new File([await new Promise((ok) => a.still.toBlob(ok, 'image/jpeg', 0.85))], `page-${stamp}.jpg`, { type: 'image/jpeg' }));
  const files = [brUp(new File([composed], `annotated-${stamp}.jpg`, { type: 'image/jpeg' })), clean];
  const sent = await brSendSheet({ text: lines.join('\n'), title: 'Send the annotated page to', files, preview: URL.createObjectURL(composed), note: true });
  a.sending = false;
  if (sent) brAnnClose();
}
// a mark, in words: what it points at or frames on the page
function brAnnMarkText(m, n) {
  const note = m.note?.trim() ? `"${m.note.trim()}" → ` : '';
  const el = brElementText(m.info);
  if (m.kind === 'pin') return `${n}. ${note || '(no comment) → '}pinned on ${el}`;
  if (m.kind === 'arrow') return `${n}. an arrow pointing at ${el}`;
  if (m.kind === 'text') return `${n}. the words "${m.text}" written on ${el}`;
  const b = brBox(m.pts), r = (v) => Math.round(v);
  return `${n}. ${note}${m.kind === 'box' ? 'a box' : 'a freehand mark (a circle, an underline…)'} over ${r(b.x)},${r(b.y)} to ${r(b.x + b.w)},${r(b.y + b.h)}; the element it best frames: ${el}` +
    (m.info?.inside ? `\n   the text inside the mark: "${m.info.inside}"` : '');
}
// a file for queueSend (app.js), uploading from now on: done by the time you've picked who gets it
function brUp(file) {
  const f = { file, name: file.name, size: file.size, img: /^image\//.test(file.type), path: '' };
  f.up = uploadFile(file).then((p) => { f.path = p; }, () => {}); // a failure: sending tries again
  return f;
}

// ---- sending to an agent: pick the pane (the session you watch comes first), add a note ----
async function brSendSheet({ text, title, files, preview, note }) {
  const panes = (await allPanes(60000)).filter((x) => x.p.agent);
  if (!panes.length) { toast('No agent is running: start one in Terminals', true); return false; }
  const ag = BR.mode === 'agents' ? brAgentOf(BR.agent) : null;
  const near = (x) => (ag?.pane && x.session === ag.pane.session && x.p.id === ag.pane.pane ? 0 : 1);
  panes.sort((x, y) => near(x) - near(y));
  let pick = 0;
  const dot = (p) => `<i class="br-dot ${p.state || ''}"></i>`;
  const ok = await brDialog(`<div class="modal-h"><span class="modal-ic fm-ic-au">${icAu('direct-send')}</span><h2>${esc(title)}</h2></div>
    ${preview ? `<img class="br-prev" src="${preview}" alt="The annotated screenshot">` : ''}
    <div class="fm-agents">${panes.map((x, i) => `<button type="button" class="fm-ag${i === 0 ? ' on' : ''}" data-ag="${i}"><span class="fm-av">${ic('terminal-square')}</span><span class="fm-agb"><b>${dot(x.p)}${esc(x.label)}</b><span>${esc([x.session, x.p.agent?.title !== x.label && x.p.agent?.title, x.p.agent?.doing].filter(Boolean).join(' · '))}</span></span></button>`).join('')}</div>
    ${note ? '<textarea class="fm-in fm-note" rows="2" placeholder="Add a message (optional)"></textarea>' : ''}
    <details class="br-what"><summary>What the agent gets</summary><pre>${esc(text)}</pre>${files.length ? `<p class="faint">+ ${files.map((f) => esc(f.name)).join(', ')}</p>` : ''}</details>
    <div class="modal-actions"><button type="button" class="btn" data-cancel>Cancel</button><button type="submit" class="btn primary">${ic('direct-send')}Send</button></div>`, {
    init: (d) => {
      $('.fm-agents', d).addEventListener('click', (e) => { const b = e.target.closest('[data-ag]'); if (!b) return; pick = +b.dataset.ag; for (const x of $$('.fm-ag', d)) x.classList.toggle('on', x === b); });
      $('.fm-note', d)?.addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) $('form', d).requestSubmit(); });
      setTimeout(() => ($('.fm-note', d) || $('[type=submit]', d)).focus(), 0);
    },
    submit: () => ($('.br-dlg .fm-note', BR.el)?.value.trim() || '') || ' ',
  });
  if (preview) URL.revokeObjectURL(preview);
  if (!ok) return false;
  const x = panes[pick];
  const msg = (ok.trim() ? ok.trim() + '\n\n' : '') + text;
  queueSend(x.key, x.session, x.p.id, msg, files);
  toast(`Sent to ${x.label}`);
  return true;
}
function brDialog(html, setup) {
  const d = $('.br-dlg', BR.el);
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

// ---- Agents: their sessions (cards with a live picture), the one you watch, its timeline, the wall ----
function brAgents() {
  const rail = $('.br-rail', BR.el), wall = $('.br-wall', BR.el);
  if (BR.mode !== 'agents') { rail.dataset.html = ''; rail.innerHTML = ''; setHidden(wall, true); $('.br-agtabs', BR.el).innerHTML = ''; $('.br-agentbar', BR.el).hidden = true; return; }
  const card = (a, big) => {
    const t = brCurTab(a);
    const last = a.last;
    const busy = a.alive && last && (!last.rc && last.rc !== 0 || Date.now() - last.t < 8000);
    const st = !a.alive ? 'done' : a.held ? 'held' : busy ? 'busy' : 'idle';
    const n = t?.probs ? t.probs[0] + t.probs[2] : 0;
    return `<button type="button" class="br-card ${st}${a.key === BR.agent && !BR.rp ? ' on' : ''}${big ? ' big' : ''}" data-agent="${esc(a.key)}">
      ${a.alive ? '<span class="br-badge live"><i></i>Live</span>' : ''}<span class="br-thumb" data-thumb="${esc(t?.id || '')}">${t ? '' : `<span class="br-thx">${ic('history')}</span>`}</span>
      <span class="br-cb"><b>${esc(a.label || a.title || a.project || a.key)}</b><span class="br-cs">${esc(a.doing || [a.project, a.sub].filter(Boolean).join(' · ') || a.cwd || '')}</span>
      <span class="br-cu">${t ? esc(brHost(t.url) + (t.url ? new URL(t.url, 'http://x').pathname : '')) : esc(a.ended ? 'finished ' + ago(a.ended) : '')}</span>
      <span class="br-cl">${last ? `${ic(BR_VERB[brVerb(last)] || 'play3')}<span>${esc(brStepText(last))}</span>` : '<span class="faint">no commands yet</span>'}</span></span>
      <span class="br-cst">${st === 'busy' ? '<i class="br-pulse"></i>' : ''}${st === 'held' ? 'You' : ''}${n ? `<b class="br-tb bad">${n}</b>` : ''}</span></button>`;
  };
  const live = BR.agents.filter((a) => a.alive);
  const liveTrails = new Set(live.map((a) => a.trail));
  const recs = (BR.trails || []).filter((r) => !liveTrails.has(r.id));
  const rec = (r) => `<button type="button" class="br-card rec${BR.rp?.id === r.id ? ' on' : ''}" data-trail="${esc(r.id)}">
      <span class="br-badge rec">${ic('play3')}Replay</span><span class="br-thumb">${r.shots ? `<img alt="" loading="lazy" src="${brShotURL(r.id, r.shots)}">` : `<span class="br-thx">${ic('history')}</span>`}</span>
      <span class="br-cb"><b>${esc(r.label || r.project || 'An agent')}</b><span class="br-cs">${esc([r.sub, r.project].filter(Boolean).join(' · '))}</span>
      <span class="br-cu">${esc(r.last ? brHost(r.last) + new URL(r.last, 'http://x').pathname : '')}</span>
      <span class="br-cl">${ic('history')}<span>${esc(ago(r.ended || r.started))} · ${r.steps} command${r.steps === 1 ? '' : 's'} · ${r.pages} page${r.pages === 1 ? '' : 's'}${r.calls ? ` · ${r.calls} API call${r.calls === 1 ? '' : 's'}` : ''}${r.errors ? ` · ${r.errors} error${r.errors === 1 ? '' : 's'}` : ''}</span></span></span>
      <span class="br-cst">${r.pinned ? ic('bookmark-check') : ''}</span></button>`;
  const html = `<div class="br-rh"><b>Live now</b><span class="faint">${live.length || 'none'}</span><span class="grow"></span><button type="button" class="icon-btn ghost sm" data-act="wall" aria-pressed="${BR.wall}" title="All of them at once" aria-label="Wall">${ic('grid4')}</button></div>` +
    (live.length ? live.map((a) => card(a)).join('') : '<p class="faint br-none">No agent browser is open.</p>') +
    `<div class="br-rh sub"><b>Recordings</b><span class="faint">${recs.length}</span><span class="grow"></span><span class="faint" title="Kept ${BR.trailsKeep || 7} days (1 GB at most, the oldest go first); a pinned one stays">${BR.trailsKeep || 7} days${BR.trailsBytes ? ' · ' + brSize(BR.trailsBytes) : ''}</span></div>` +
    (recs.length ? recs.slice(0, 40).map(rec).join('') : '<p class="faint br-none">Every agent session is recorded here, to replay and compare.</p>');
  if (rail.dataset.html !== html) { rail.innerHTML = html; rail.dataset.html = html; brThumbPaint(); }
  setHidden(wall, !BR.wall);
  if (BR.wall) {
    const wh = live.length ? live.map((a) => card(a, true)).join('') : '<p class="faint br-none">No agent browser is open right now.</p>';
    if (wall.dataset.html !== wh) { wall.innerHTML = wh; wall.dataset.html = wh; brThumbPaint(); }
  }
  // the bar: who this is, what it's doing, Take over
  const a = brAgentOf(BR.agent);
  const bar = $('.br-agentbar', BR.el);
  bar.hidden = !a || !!BR.rp;
  if (a) {
    const bh = `<span class="br-ab">${ic('sparkle')}<b>${esc(a.label || a.key)}</b>${a.sub ? `<span class="faint">${esc(a.sub)}</span>` : ''}</span>` +
      (a.pane ? `<button type="button" class="hbtn" data-act="chat">${ic('chat-round-dots')}<span>Open its chat</span></button>` : '') +
      (a.trail ? `<button type="button" class="hbtn" data-act="replay">${ic('history')}<span>Replay</span></button>` : '') +
      (a.alive && a.tabs?.length ? `<button type="button" class="hbtn br-hold${a.held ? ' on' : ''}" data-act="hold">${ic(a.held ? 'arrow-rotate' : 'hand')}<span>${a.held ? 'Hand back' : 'Take over'}</span></button>` : '');
    if (bar.dataset.html !== bh) { bar.innerHTML = bh; bar.dataset.html = bh; }
  }
  const tabs = a?.tabs || [];
  const th = tabs.length > 1 ? tabs.map((t) => `<button type="button" class="chip${t.id === brAgentTab()?.id ? ' on' : ''}" data-agtab="${esc(t.id)}">${esc(t.title || brHost(t.url) || 'tab')}</button>`).join('') : '';
  const ab = $('.br-agtabs', BR.el);
  if (ab.dataset.html !== th) { ab.innerHTML = th; ab.dataset.html = th; }
}
const brVerb = (s) => { const a = s.argv || []; let i = 0; while (i < a.length && a[i].startsWith('-')) i += a[i] === '--session' ? 2 : 1; return a[i] || ''; };
function brStepText(s) {
  const a = s.argv || [];
  let i = 0;
  while (i < a.length && a[i].startsWith('-')) i += a[i] === '--session' ? 2 : 1;
  const v = a[i] || '', rest = a.slice(i + 1).join(' ');
  return `${v} ${rest}`.trim().replace(/\s+/g, ' ').slice(0, 140);
}
// which tabs this page shows as cards (the rail, the wall): their small streams
function brThumbs() {
  const ids = BR.mode === 'agents' ? BR.agents.filter((a) => a.alive).map((a) => brCurTab(a)?.id).filter(Boolean) : [];
  const k = ids.join();
  if (k === BR.thumbKey) return;
  BR.thumbKey = k;
  brSend({ t: 'thumbs', ids });
}
function brThumb(meta, img) { brThumbBlob(meta.tab, new Blob([img], { type: 'image/jpeg' })); }
function brThumbBlob(tab, blob) {
  const old = BR.thumbs.get(tab);
  if (old) URL.revokeObjectURL(old);
  BR.thumbs.set(tab, URL.createObjectURL(blob));
  brThumbPaint(tab);
}
function brThumbPaint(only) {
  for (const el of $$(only ? `[data-thumb="${CSS.escape(only)}"]` : '[data-thumb]', BR.el)) {
    const u = BR.thumbs.get(el.dataset.thumb);
    if (!u) continue;
    let img = el.querySelector('img');
    if (!img) { img = document.createElement('img'); img.alt = ''; el.replaceChildren(img); }
    img.src = u;
  }
}

// ---- recordings: every agent session's trail (a week), replayed: its pictures over time, its commands, its pages ----
const brShotURL = (id, n) => `/api/browser/trail/shot?id=${encodeURIComponent(id)}&n=${n}`;
async function brLoadTrails() {
  const r = await api('/api/browser/trails').catch(() => null);
  if (!r?.trails) return;
  BR.trails = r.trails;
  BR.trailsBytes = r.bytes;
  BR.trailsKeep = r.keepDays;
  if (BR.el) { $('.br-rail', BR.el).dataset.html = ''; brAgents(); }
}
async function brTrail(id) {
  const r = await api('/api/browser/trail?id=' + encodeURIComponent(id)).catch(() => null);
  if (!r?.meta) return null;
  const ev = r.events.slice().sort((a, b) => a.t - b.t);
  // the pictures over time: the motion's frames, and the sharp ones after each command and load
  const pics = r.frames.map((f) => ({ t: f.t, src: `/api/browser/trail/shot?id=${encodeURIComponent(id)}&off=${f.off}&len=${f.len}` }));
  for (const e of ev) if ((e.k === 'shot' && e.n) || (e.k === 'load' && e.shot)) pics.push({ t: e.t, src: brShotURL(id, e.n || e.shot), sharp: true });
  pics.sort((a, b) => a.t - b.t);
  const t0 = Math.min(ev[0]?.t ?? Infinity, pics[0]?.t ?? Infinity, r.meta.started * 1000);
  const t1 = Math.max(ev.at(-1)?.t ?? 0, pics.at(-1)?.t ?? 0, t0 + 1000);
  return { id, meta: r.meta, ev, pics, t0, t1, t: t1, playing: false, speed: 4 };
}
async function brReplay(id) {
  const rp = await brTrail(id);
  if (!rp) return toast("That recording isn't there any more", true);
  rp.t = rp.t0;
  BR.rp = rp;
  BR.rp2 = null;
  BR.mode = 'agents';
  brRender();
  brWatch();
  brRpPaint();
}
function brRpClose() {
  if (!BR.rp) return;
  BR.rp = BR.rp2 = null;
  cancelAnimationFrame(BR.rpRAF);
  brRender();
  brWatch();
}
const brBare = (u) => String(u || '').replace(/^[a-z]+:\/\//i, '').replace(/^www\./, '').replace(/^127\.0\.0\.1(?=[:/]|$)/, 'localhost').replace(/\/$/, '');
// what the trail showed at its time t: the newest picture, the page, the command running
function brRpAt(rp, t) {
  let pic = null, url = '', title = '', step = null;
  for (const p of rp.pics) { if (p.t > t) break; pic = p; }
  pic ||= rp.pics[0] || null; // before its first picture: that one
  for (const e of rp.ev) {
    if (e.t > t) break;
    if (e.url && (e.k === 'nav' || e.k === 'load' || e.k === 'shot' || e.k === 'tab')) { url = e.url; if (e.title) title = e.title; }
    if (e.k === 'step') step = e;
  }
  return { pic, url, title, step };
}
const brRpClock = (ms) => { const s = Math.max(0, Math.round(ms / 1000)); return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`; };
function brRpView(rp, i) {
  const span = rp.t1 - rp.t0 || 1;
  const marks = rp.ev.filter((e) => e.k === 'step' || e.k === 'load' || e.k === 'con' || e.k === 'net').map((e) => `<i class="br-rpm ${e.k}" style="left:${(((e.t - rp.t0) / span) * 100).toFixed(2)}%"></i>`).join('');
  return `<div class="br-rpv" data-rpv="${i}">
    <div class="br-rpu"><span class="br-rpurl mono"></span><span class="br-rpstep"></span></div>
    <div class="br-rps"><img class="br-rpimg" alt=""></div>
    <div class="br-rpbar">
      <button type="button" class="icon-btn ghost sm" data-rp="play" aria-label="Play">${ic('play3')}</button>
      <span class="br-rpt mono"></span>
      <div class="br-rptrack"><div class="br-rpmarks">${marks}</div><input type="range" min="0" max="${span}" step="100" value="0" aria-label="Time"></div>
      <button type="button" class="chip" data-rp="speed" title="Speed (idle stretches are skipped)">${rp.speed}×</button>
    </div></div>`;
}
function brRpRender() {
  const box = $('.br-rp', BR.el);
  const rp = BR.rp;
  setHidden(box, !rp);
  BR.el.classList.toggle('replaying', !!rp);
  if (!rp) { box.innerHTML = ''; box.dataset.k = ''; return; }
  const k = rp.id + '|' + (BR.rp2?.id || '') + '|' + rp.meta.pinned;
  if (box.dataset.k === k) return;
  box.dataset.k = k;
  const m = rp.meta;
  box.innerHTML = `<div class="br-rph">
      <span class="br-rpw">${ic('history')}<b>${esc(m.label || m.project || 'An agent')}</b><span class="faint">${esc(new Date(m.started * 1000).toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' }))} · ${m.steps} command${m.steps === 1 ? '' : 's'} · ${m.pages} page${m.pages === 1 ? '' : 's'}${m.errors ? ` · ${m.errors} error${m.errors === 1 ? '' : 's'}` : ''}${m.live ? ' · <b class="br-live">recording</b>' : ''}</span></span>
      <span class="grow"></span>
      <button type="button" class="hbtn" data-rp="compare">${ic('layer')}<span>${BR.rp2 ? 'Stop comparing' : 'Compare'}</span></button>
      <button type="button" class="hbtn${m.pinned ? ' on' : ''}" data-rp="pin" title="${m.pinned ? 'Kept until you unpin it' : 'Keep it past the week'}">${ic('bookmark-check')}<span>${m.pinned ? 'Pinned' : 'Pin'}</span></button>
      ${m.live ? '' : `<button type="button" class="icon-btn ghost sm" data-rp="delete" title="Delete this recording" aria-label="Delete">${ic('trash2')}</button>`}
      <button type="button" class="icon-btn ghost sm" data-rp="close" title="Close" aria-label="Close">${IC.x}</button></div>
    <div class="br-rpvs${BR.rp2 ? ' two' : ''}">${brRpView(rp, 0)}${BR.rp2 ? brRpView(BR.rp2, 1) : ''}</div>`;
  for (const v of $$('.br-rpv', box)) {
    const x = +v.dataset.rpv ? BR.rp2 : BR.rp;
    $('input[type=range]', v).addEventListener('input', (e) => { x.t = x.t0 + +e.target.value; x.playing = false; brRpPaint(); });
  }
}
function brRpPaint() {
  brRpRender();
  for (const [i, rp] of [BR.rp, BR.rp2].entries()) {
    if (!rp) continue;
    const v = $(`[data-rpv="${i}"]`, BR.el);
    if (!v) continue;
    const at = brRpAt(rp, rp.t);
    const img = $('.br-rpimg', v);
    if (at.pic && img.dataset.src !== at.pic.src) { img.dataset.src = at.pic.src; img.src = at.pic.src; }
    setHidden(img, !at.pic);
    setText($('.br-rpurl', v), at.url ? brBare(at.url) : '');
    setText($('.br-rpstep', v), at.step ? brStepText(at.step) : '');
    setText($('.br-rpt', v), `${brRpClock(rp.t - rp.t0)} / ${brRpClock(rp.t1 - rp.t0)}`);
    const range = $('input[type=range]', v);
    if (document.activeElement !== range) range.value = String(rp.t - rp.t0);
    $('[data-rp="play"]', v).innerHTML = ic(rp.playing ? 'pause3' : 'play3');
    // the next pictures, so playing doesn't wait on them
    const n = rp.pics.findIndex((p) => p.t > rp.t);
    for (const p of rp.pics.slice(Math.max(0, n), n + 3)) if (!p.pre) { p.pre = new Image(); p.pre.src = p.src; }
  }
  const side = $('.br-side', BR.el);
  if (side) for (const x of $$('.br-rpe', side)) x.classList.toggle('on', +x.dataset.t <= BR.rp.t && +(x.nextElementSibling?.dataset.t ?? Infinity) > BR.rp.t);
}
// playing: the trail's time runs at its speed; a stretch where nothing happened for over 2 s is skipped
function brRpPlay(rp) {
  rp.playing = !rp.playing;
  if (rp.t >= rp.t1) rp.t = rp.t0;
  let last = performance.now();
  const tick = (now) => {
    const dt = (now - last) * rp.speed;
    last = now;
    if (!rp.playing || (BR.rp !== rp && BR.rp2 !== rp)) return;
    let t = rp.t + dt;
    const next = Math.min(rp.pics.find((p) => p.t > rp.t)?.t ?? Infinity, rp.ev.find((e) => e.t > rp.t)?.t ?? Infinity);
    if (next - rp.t > 2000 && next !== Infinity) t = Math.max(t, next - 300);
    rp.t = Math.min(t, rp.t1);
    if (rp.t >= rp.t1) rp.playing = false;
    brRpPaint();
    if (rp.playing) BR.rpRAF = requestAnimationFrame(tick);
  };
  brRpPaint();
  if (rp.playing) BR.rpRAF = requestAnimationFrame(tick);
}
// jump to a moment; comparing, the other trail goes to its own visit of the same page
function brRpSeek(t, url) {
  const rp = BR.rp;
  rp.t = Math.max(rp.t0, Math.min(rp.t1, t));
  rp.playing = false;
  if (BR.rp2 && url) {
    const e = BR.rp2.ev.find((x) => x.k === 'load' && brBare(x.url) === brBare(url));
    if (e) { BR.rp2.t = e.t + 50; BR.rp2.playing = false; }
  }
  brRpPaint();
}
// the side: the recording's steps (commands, page loads, errors), or the API calls its pages made, each opened in place
function brRpSide(side) {
  const rp = BR.rp;
  const verb = (e) => e.k === 'step' ? brVerb(e) : e.k === 'load' ? 'loaded' : e.k === 'nav' ? 'went to' : e.k === 'con' ? (e.level === 'error' ? 'error' : 'warning') : e.k === 'net' ? 'request failed' : '';
  const calls = rp.ev.filter((e) => e.k === 'call');
  const onCalls = BR.rpTab === 'calls' && calls.length > 0;
  const shown = rp.ev.filter((e) => ['step', 'load', 'con', 'net'].includes(e.k));
  const head = calls.length ? `<div class="br-rh"><b>Recording</b><span class="grow"></span><span class="br-rptabs" role="tablist">
      <button type="button" class="chip${onCalls ? '' : ' on'}" role="tab" aria-selected="${!onCalls}" data-rptab="steps">Steps ${shown.length}</button>
      <button type="button" class="chip${onCalls ? ' on' : ''}" role="tab" aria-selected="${onCalls}" data-rptab="calls">API calls ${calls.length}</button></span></div>`
    : `<div class="br-rh"><b>Recording</b><span class="faint">${shown.length} events</span></div>`;
  if (onCalls) {
    const html = head + `<ol class="br-tl">${calls.map((e) => `<li class="br-sx br-rpe br-call${e.err || e.status >= 400 ? ' bad' : ''}" data-t="${e.t}" data-call="${e.n}">
      <span class="br-si">${ic('code')}</span>
      <span class="br-sb"><span class="br-sv"><b>${esc(e.method)}</b><span class="mono">${esc(brBare(e.url).slice(0, 300))}</span></span>
      <span class="br-sm">${e.err ? esc(e.err) : e.status} · ${brSize(e.size || 0)}${e.ms ? ` · ${Math.round(e.ms)} ms` : ''} · ${brRpClock(e.t - rp.t0)} · ${brTime(e.t)}</span>
      <div class="br-calld" hidden></div></span></li>`).join('')}</ol>`;
    if (side.dataset.html !== html) { side.innerHTML = html; side.dataset.html = html; }
    return;
  }
  const html = head + `<ol class="br-tl">${shown.map((e) => {
    const bad = e.k === 'con' || e.k === 'net';
    const text = e.k === 'step' ? brStepText(e).replace(/^\S+\s?/, '') : e.k === 'load' ? (e.title ? e.title + ' · ' : '') + brBare(e.url) : e.text || brBare(e.url);
    const icon = e.k === 'step' ? BR_VERB[brVerb(e)] || 'play3' : e.k === 'load' ? 'globe' : 'bug';
    return `<li class="br-sx br-rpe${bad ? ' bad' : ''}" data-t="${e.t}" data-url="${esc(e.url || '')}">
      <span class="br-si">${ic(icon)}</span>
      <span class="br-sb"><span class="br-sv"><b>${esc(verb(e))}</b><span class="mono">${esc(String(text).slice(0, 200))}</span></span>
      <span class="br-sm">${brRpClock(e.t - rp.t0)} · ${brTime(e.t)}</span>
      ${e.k === 'load' && e.shot ? `<img class="br-sp" loading="lazy" src="${brShotURL(rp.id, e.shot)}" alt="">` : ''}</span></li>`; }).join('')}</ol>`;
  if (side.dataset.html !== html) { side.innerHTML = html; side.dataset.html = html; }
}
// an API call, opened: what was sent and what came back (kept in the trail: browsercalls.go)
async function brRpCall(li) {
  const d = $('.br-calld', li);
  if (!d.hidden) { d.hidden = true; return; }
  d.hidden = false;
  if (d.call) return;
  d.innerHTML = '<span class="faint">Loading…</span>';
  const r = await api(`/api/browser/trail/call?id=${encodeURIComponent(BR.rp.id)}&n=${li.dataset.call}`).catch(() => null);
  if (!r?.url) { d.innerHTML = '<span class="faint">This call wasn\'t kept.</span>'; return; }
  d.call = r;
  const pretty = (s) => { try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s || ''; } };
  const body = (x) => (x?.base64 ? '(not text: kept as base64 in the file)' : pretty(x?.body).slice(0, 200000) + (x?.truncated ? `\n… (cut: ${brSize(x.truncated)} in all)` : ''));
  const heads = (h) => Object.entries(h || {}).map(([k, v]) => `${k}: ${v}`).join('\n');
  d.innerHTML = `<div class="mono br-callu">${esc(r.method)} ${esc(r.url)}</div>
    ${r.request?.body ? `<div class="br-callh">Sent</div><pre>${esc(body(r.request))}</pre>` : ''}
    <div class="br-callh">Received <span class="faint">${r.error ? esc(r.error) : `${r.response?.status} · ${esc(r.mime || '')}`}</span></div>${r.response?.body ? `<pre>${esc(body(r.response))}</pre>` : '<p class="faint">No body.</p>'}
    <details><summary>Headers</summary><pre>${esc(`Request\n${heads(r.request?.headers)}\n\nResponse\n${heads(r.response?.headers)}`)}</pre></details>
    <div class="br-calla"><button type="button" class="btn sm" data-copycall>${ic('copy')}Copy the response</button><span class="faint">Agents: <code>cube-browser api-calls ${esc(brBare(r.page).split('/')[0])}</code></span></div>`;
}
// the player's buttons, and the side's events
document.addEventListener('click', async (e) => {
  if (!BR.el || !BR.rp || !BR.el.contains(e.target)) return;
  const tab = e.target.closest('[data-rptab]');
  if (tab) { BR.rpTab = tab.dataset.rptab; return brRpSide($('.br-side', BR.el)); }
  if (e.target.closest('[data-copycall]')) {
    const c = e.target.closest('.br-calld')?.call;
    try { await navigator.clipboard.writeText(c?.response?.body || ''); toast('Copied the response'); } catch { toast("This browser can't copy here", true); }
    return;
  }
  if (e.target.closest('.br-calld')) return; // reading it, selecting its text
  const ev = e.target.closest('.br-rpe');
  if (ev?.dataset.call) brRpCall(ev);
  if (ev) return brRpSeek(+ev.dataset.t, ev.dataset.url);
  const b = e.target.closest('[data-rp]');
  if (!b) return;
  const v = b.closest('[data-rpv]');
  const rp = v && +v.dataset.rpv ? BR.rp2 : BR.rp;
  const k = b.dataset.rp;
  if (k === 'play') return brRpPlay(rp);
  if (k === 'speed') { rp.speed = { 1: 2, 2: 4, 4: 8, 8: 16, 16: 1 }[rp.speed] || 4; b.textContent = rp.speed + '×'; return; }
  if (k === 'close') return brRpClose();
  if (k === 'pin' || k === 'delete') {
    if (k === 'delete' && !b.classList.contains('sure')) { // a second click within 3 s deletes it
      b.classList.add('sure');
      b.innerHTML = `${ic('trash2')}<span>Delete?</span>`;
      setTimeout(() => { b.classList.remove('sure'); b.innerHTML = ic('trash2'); }, 3000);
      return;
    }
    const r = await api('/api/browser/trail/edit', { method: 'POST', body: JSON.stringify({ id: BR.rp.id, do: k === 'pin' ? (BR.rp.meta.pinned ? 'unpin' : 'pin') : 'delete' }) }).catch(() => null);
    if (!r?.ok) return toast(r?.error || "That didn't work", true);
    if (k === 'delete') { brRpClose(); toast('Recording deleted'); }
    else { BR.rp.meta.pinned = !BR.rp.meta.pinned; brRpPaint(); }
    return brLoadTrails();
  }
  if (k === 'compare') {
    if (BR.rp2) { BR.rp2 = null; brRpPaint(); return; }
    const others = (BR.trails || []).filter((r) => r.id !== BR.rp.id);
    if (!others.length) return toast('No other recording to compare with yet');
    const same = (r) => (r.project && r.project === BR.rp.meta.project ? 0 : 1);
    others.sort((a, c) => same(a) - same(c) || c.started - a.started);
    const br = b.getBoundingClientRect();
    fmMenu(br.right, br.bottom, others.slice(0, 14).map((r) => ({ k: r.id, l: `${r.label || r.project || 'An agent'} · ${ago(r.ended || r.started)} · ${brBare(r.last || '')}`, i: 'history' })), async (id) => {
      const rp2 = await brTrail(id);
      if (!rp2) return;
      rp2.t = rp2.t0;
      BR.rp2 = rp2;
      const at = brRpAt(BR.rp, BR.rp.t);
      brRpSeek(BR.rp.t, at.url);
    });
  }
});

// the banner over an agent's page: it's driving (what it just did), or you are
function brDrive() {
  const box = $('.br-drive', BR.el);
  const a = BR.mode === 'agents' ? brAgentOf(BR.agent) : null;
  const t = brShown();
  setHidden(box, !a || !t || !a.alive);
  if (!a || !t) return;
  const last = a.last;
  const h = a.held
    ? `<span class="br-dv you">${ic('hand')}<b>You have control.</b><span>The agent waits until you hand it back.</span></span><button type="button" class="btn sm" data-act="hold">${ic('arrow-rotate')}Hand back</button>`
    : `<span class="br-dv">${last && !last.rc && last.rc !== 0 ? '<i class="br-pulse"></i>' : ic('eye')}<b>${esc(a.label || 'The agent')} is driving.</b><span>${last ? esc(brStepText(last)) : 'Watching'}</span></span><button type="button" class="btn sm" data-act="hold">${ic('hand')}Take over</button>`;
  if (box.dataset.html !== h) { box.innerHTML = h; box.dataset.html = h; }
}
function brHint() {
  const box = $('.br-drive', BR.el);
  box.classList.remove('nudge');
  void box.offsetWidth;
  box.classList.add('nudge');
}

// the timeline: every command the agent's cube-browser ran, with a picture of the page after it
function brSteps(m) {
  if (m.agent !== BR.agent) return;
  if (m.reset || BR.stepsFor !== m.agent) { BR.steps = []; BR.stepsFor = m.agent; }
  for (const s of m.steps || []) { const i = BR.steps.findIndex((x) => x.id === s.id); if (i >= 0) BR.steps[i] = s; else BR.steps.push(s); }
  const last = BR.steps[BR.steps.length - 1];
  if (last && Date.now() - last.t < 10000) brStepFlash(last);
  brTimeline();
}
function brStepFlash(s) {
  const box = $('.br-step', BR.el);
  box.innerHTML = `${ic(BR_VERB[brVerb(s)] || 'play3')}<span>${esc(brStepText(s))}</span>${s.rc === undefined || s.rc === null ? '<i class="br-tspin"></i>' : s.rc ? `<b class="bad">failed</b>` : ''}`;
  box.hidden = false;
  box.classList.remove('show');
  void box.offsetWidth;
  box.classList.add('show');
  clearTimeout(BR.stepT);
  BR.stepT = setTimeout(() => { box.classList.remove('show'); }, 3500);
}
function brTimeline() {
  const side = $('.br-side', BR.el);
  if (BR.mode !== 'agents') { side.innerHTML = ''; side.dataset.html = ''; return; }
  if (BR.rp) return brRpSide(side);
  const a = brAgentOf(BR.agent);
  const steps = BR.stepsFor === BR.agent ? BR.steps : [];
  const html = `<div class="br-rh"><b>Timeline</b><span class="faint">${steps.length} command${steps.length === 1 ? '' : 's'}</span></div>` + (steps.length ? `<ol class="br-tl">${steps.slice().reverse().map((s) => {
    const run = s.rc === undefined || s.rc === null;
    return `<li class="br-sx${run ? ' run' : s.rc ? ' bad' : ''}" data-step="${esc(s.id)}">
      <span class="br-si">${ic(BR_VERB[brVerb(s)] || 'play3')}</span>
      <span class="br-sb"><span class="br-sv"><b>${esc(brVerb(s))}</b><span class="mono">${esc(brStepText(s).replace(/^\S+\s?/, ''))}</span></span>
      <span class="br-sm">${run ? '<i class="br-tspin"></i>running' : s.rc ? 'failed' : 'ok'}${s.ms ? ` · ${s.ms < 1000 ? s.ms + ' ms' : (s.ms / 1000).toFixed(1) + ' s'}` : ''} · ${brTime(s.t)}</span>
      ${s.shot ? `<img class="br-sp" loading="lazy" src="${brShotURL(s.trail, s.shot)}" alt="">` : ''}
      ${s.out ? `<pre class="br-so">${esc(s.out)}</pre>` : ''}</span></li>`; }).join('')}</ol>`
    : `<p class="faint br-none">${a ? 'Its commands show here as it runs them.' : ''}</p>`);
  if (side.dataset.html !== html) {
    const open = new Set($$('.br-sx.open', side).map((x) => x.dataset.step));
    side.innerHTML = html;
    side.dataset.html = html;
    for (const x of $$('.br-sx', side)) if (open.has(x.dataset.step)) x.classList.add('open');
  }
}
setInterval(() => { if (S.view === 'browser' && BR.mode === 'agents' && BR.el) { BR.el.querySelector('.br-rail').dataset.html = ''; brAgents(); if (++BR.trailsTick % 3 === 0) brLoadTrails(); } }, 5000); // "8 s ago" turns into idle; the recordings every 15 s

// ---- keys on the Browser page (not while the page itself has them) ----
addEventListener('keydown', (e) => {
  if (S.view !== 'browser' || !BR.el || e.target === BR.kb) return;
  if (e.key === 'Escape' && BR.ann && !e.target.closest?.('.br-pin, .br-tin, dialog')) { e.preventDefault(); return brAnnClose(); }
  if (e.target.closest?.('input, textarea, select, [contenteditable], dialog')) return;
  if (brShortcut(e)) return;
  if (e.metaKey || e.ctrlKey || e.altKey) { if (BR.ann && (e.metaKey || e.ctrlKey) && e.key === 'z') { e.preventDefault(); BR.ann.marks.pop(); brAnnPaint(); brAnnPins(); } return; }
  if (e.key === 'a' && !BR.ann && brShown()) { e.preventDefault(); brAnnotate(); }
  else if (e.key === 'p') { e.preventDefault(); brDevToggle(); }
  else if (e.key === 'l') { e.preventDefault(); $('.br-url', BR.el).focus(); }
});
