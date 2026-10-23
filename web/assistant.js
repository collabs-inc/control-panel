'use strict';

// ---------- assistant ----------
// The owner's Hermes agent (projects/vm-agent). One chat for every channel, in time order, like a thread of
// events: each message says who sent it, through which channel (Telegram, Panel) and when. A JARVIS turn shows
// its work as a trace (thinking, what it says while working, each tool call with its input and result) above
// the answer, which can carry tables, diagrams, charts and files (ai-rich.js). What you type here goes out
// through the Panel. Connections (the plug in the composer): the channels' state, the Google sign-in file and
// Hermes' own dashboard (/hermes/). Under the composer, the aurora (aurora.js) quickens while JARVIS works.

const AI_CH = { panel: 'Panel', telegram: 'Telegram', agent: 'Agent' }; // agent: a Claude agent's `jarvis` messages
const AI = {
  conn: false, all: false, files: [], queue: [], models: [], tasks: [], tasksOpen: false, tasksOff: store.get('aiTasksOff') === '1', items: [], more: false, live: null, state: null, es: null, google: null, loadingMore: false, stale: false,
  v: 0, drawn: '', open: new Set(), // items version, what the history shows, which trace steps are expanded
};
S.data.ai = {}; // the view draws its own content: render() only needs it present
VIEWS.ai = () => '<div id="ai" class="ai" data-own></div>';

const aiTime = (t) => new Date(t * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
function aiDay(t) { // "Today", "Yesterday", "Wed 29 Apr"
  const d = new Date(t * 1000);
  const days = Math.round((new Date().setHours(0, 0, 0, 0) - new Date(d).setHours(0, 0, 0, 0)) / 864e5);
  return days === 0 ? 'Today' : days === 1 ? 'Yesterday' : d.toLocaleDateString([], { weekday: 'short', day: 'numeric', month: 'short' });
}
// who · channel · time, the sender's side first
const aiMeta = (who, ch, t) => `<div class="ai-meta"><b>${who}</b><span class="ai-ch ch-${esc(ch)}">${esc(AI_CH[ch] || ch)}</span><span>${t}</span></div>`;

// ---- shell: history, the turn in progress, composer (built once per visit) ----
function aiMount(view) {
  const root = $('#ai', view);
  $('#title').hidden = true; // the chat speaks for itself
  if (!root || root.dataset.on) return;
  root.dataset.on = '1';
  root.innerHTML = '<aside id="ai-tasks" class="ai-tasks" aria-label="Tasks" hidden></aside><div id="ai-body" class="ai-body"><div id="ai-hist" class="ai-hist"></div><div id="ai-now" class="ai-hist"></div><div id="ai-conns" hidden></div><div id="ai-all" class="aic-body" hidden></div></div>' +
    '<div id="ai-foot" class="ai-foot"><canvas class="ai-aurora" aria-hidden="true"></canvas>' +
    `<form id="ai-form" class="composer" autocomplete="off"><button type="button" id="ai-down" class="tobottom" aria-label="Jump to the latest" title="Jump to the latest">${ic('arrow-down')}</button>` +
    `<div id="ai-files" class="files" hidden></div>` +
    `<textarea id="ai-text" rows="1" placeholder="Message JARVIS" aria-label="Message JARVIS"></textarea>` +
    `<div class="ctools"><button type="button" class="cbtn" data-aiattach aria-label="Attach files" title="Attach files or photos (or drop, or paste)">${ic('paperclip')}</button>` +
    `<button type="button" class="cbtn" data-aiconn aria-pressed="false" aria-label="Settings" title="Settings: model, channels, memory, skills, tools, jobs, logs">${ic('tuning')}</button>` +
    `<button type="button" class="cbtn ai-tbtn" data-aitasks aria-pressed="true" aria-label="Show tasks" title="Show or hide tasks" hidden>${ic('list-check')}<b class="cbadge"></b></button>` +
    `<button type="button" id="ai-clear" class="cbtn cclear" data-aiclear aria-label="Clear" title="Clear what you've written" hidden>${ic('xmark')}</button>` +
    `<button type="submit" id="ai-send" class="cbtn send" aria-label="Send" disabled>${ic('arrow-up')}</button></div></form></div>`;
  root.addEventListener('click', aiClick);
  root.addEventListener('toggle', (e) => { const k = e.target.dataset?.k; if (k) { if (e.target.open) AI.open.add(k); else AI.open.delete(k); } }, true);
  const ta = $('#ai-text', root);
  try { ta.value = sessionStorage.getItem('aiDraft') || ''; } catch { /* no storage */ }
  ta.addEventListener('input', () => { aiGrow(ta); aiSendState(); try { sessionStorage.setItem('aiDraft', ta.value); } catch { /* no storage */ } });
  ta.addEventListener('keydown', (e) => { // Enter sends on a keyboard; on a phone it's a new line (the button sends)
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && !matchMedia('(pointer: coarse)').matches) { e.preventDefault(); aiSubmit(); }
  });
  $('#ai-form', root).addEventListener('submit', (e) => { e.preventDefault(); aiSubmit(); });
  ta.addEventListener('paste', (e) => { const fs = [...(e.clipboardData?.files || [])]; if (fs.length) { e.preventDefault(); aiAddFiles(fs); } });
  root.insertAdjacentHTML('beforeend', '<input id="ai-file" type="file" multiple hidden>');
  $('#ai-file', root).addEventListener('change', (e) => { aiAddFiles([...e.target.files]); e.target.value = ''; });
  aiFilesRender();
  aiGrow(ta);
  aurora($('.ai-aurora', root));
  AI.items = [];
  AI.more = false;
  AI.drawn = '';
  AI.tasksOpen = false;
  root.classList.toggle('ai-room', AI.conn || AI.all);
  aiTasksRender();
  aiConnect();
  aiRender(true);
}

// leaving the view: no stream, no aurora, the title back
function aiLeave() {
  aiStop();
  aurora(null);
  $('#title').hidden = false;
}

function aiGrow(ta) { ta.style.height = 'auto'; ta.style.height = Math.min(200, ta.scrollHeight) + 'px'; }

// the composer's plug: Connections in place of the chat, and back
function aiConnToggle(on = !AI.conn) {
  if (on === AI.conn) return;
  AI.conn = on;
  if (on) AI.all = false;
  $('#ai')?.classList.toggle('ai-room', on);
  if (on) aiTasksToggle(false);
  if (on) { aicLoad(); if (AIC.tab === 'tools') aicLoad('mcp'); }
  aicFollow();
  aiRender(true);
}

function aiConnect() {
  aiStop();
  if (document.hidden || S.view !== 'ai') return;
  const es = new EventSource('/api/ai/stream');
  es.onmessage = (e) => aiMessage(JSON.parse(e.data));
  es.onerror = () => { if (es.readyState === EventSource.CLOSED) { AI.es = null; setTimeout(() => { if (S.view === 'ai' && !AI.es) aiConnect(); }, 3000); } };
  AI.es = es;
}
function aiStop() { if (AI.es) { AI.es.close(); AI.es = null; } }

function aiMessage(m) {
  if (m.kind === 'init') { AI.items = m.items || []; AI.more = !!m.more; AI.v++; }
  else if (m.items?.length) { const have = new Set(AI.items.map((i) => i.k)); AI.items.push(...m.items.filter((i) => !have.has(i.k))); AI.v++; }
  if ('live' in m) AI.live = m.live;
  if ('state' in m) AI.state = m.state;
  if ('queue' in m) AI.queue = m.queue || [];
  if ('tasks' in m) { AI.tasks = m.tasks || []; aiTasksRender(); }
  aiRender(m.kind === 'init');
}

// ---- the task board: the work you gave JARVIS (vm-agent's Task model, kept with its task_track tool) ----
// Wide screens: a column left of the centered chat. Narrower ones: a row of chips under the top bar (status, title,
// progress), and a tap on one opens the cards over the chat. No tasks, nothing shown. The composer's list button
// shows or hides it; "All" opens every task, with its timeline, in place of the chat.
const AI_TK = { blocked: 'Blocked', in_progress: 'In progress', done: 'Done' };
const aiWhen = (t) => (aiDay(t) === 'Today' ? aiTime(t) : aiDay(t));
const aiPct = (t) => Math.max(0, Math.min(100, t.progress | 0));
// a task is a row (status dot, title, progress); a tap opens it on what done means and where it stands, and a tap
// on that opens the task in the Tasks tab, with its timeline
function aiTaskHTML(t) {
  return `<details class="tk ${esc(t.status)}" data-task="${t.id}" data-k="tk:${t.id}"${aiOpen('tk:' + t.id)}>` +
    `<summary class="tk-r"><i class="tk-dot"></i><b class="tk-t">${esc(t.title)}</b><span class="tk-pct">${aiPct(t)}%</span><span class="tk-bar"><i data-w="${aiPct(t)}"></i></span></summary>` +
    `<div class="tk-x" data-aitaskopen="${t.id}" role="button" tabindex="0" title="Details and timeline">` +
    (t.done_when ? `<div class="tk-dod"><span>Done when</span>${esc(t.done_when)}</div>` : '') +
    (t.note && t.status !== 'done' ? `<div class="tk-note">${esc(t.note)}</div>` : '') +
    `<div class="tk-when">#${t.id} · ${AI_TK[t.status] || esc(t.status)} · ${aiWhen(t.updated)}</div>` +
    `<div class="tk-acts">${aiDoneBtn(t, 'tk-act ok', ic('check'))}${aiCancelBtn(t, 'tk-act bad')}<b>Details${ic('angle-right')}</b></div></div></details>`;
}
// what the board shows: open tasks, blocked first, then the ones done in the last day
function aiTasksShown() {
  const by = (s) => AI.tasks.filter((t) => t.status === s && (s !== 'done' || now() - t.updated < 86400)).sort((a, b) => b.updated - a.updated);
  return [by('blocked'), by('in_progress'), by('done')];
}
function aiTasksHTML() {
  const [blocked, going, done] = aiTasksShown();
  const sum = [going.length && `${going.length} in progress`, blocked.length && `${blocked.length} blocked`].filter(Boolean).join(' · ');
  const chip = (t) => `<button type="button" class="tk-chip ${esc(t.status)}" data-aitask="${t.id}"><i></i><span>${esc(t.title)}</span><b>${aiPct(t)}%</b><u data-w="${aiPct(t)}"></u></button>`;
  return `<div class="tk-strip">${[...blocked, ...going, ...done].map(chip).join('')}</div>` +
    `<div class="tk-list"><div class="tk-h"><b>${ic('list-check')}Tasks</b><span>${sum}</span><button type="button" class="tk-all" data-aiall="1">All${ic('angle-right')}</button></div>` + [...blocked, ...going].map(aiTaskHTML).join('') +
    (done.length ? `<details class="tk-done" data-k="tasks:done"${aiOpen('tasks:done')}><summary>${ic('check-circle')}${done.length} done today</summary>${done.map(aiTaskHTML).join('')}</details>` : '') + '</div>';
}
// every task you gave JARVIS, in place of the chat ("All" on the board): open ones first, each opening on what done
// means, where it stands and its timeline
function aiTasksAllHTML() {
  const order = { blocked: 0, in_progress: 1, done: 2 };
  const all = [...AI.tasks].sort((a, b) => order[a.status] - order[b.status] || b.updated - a.updated);
  const open = all.filter((t) => t.status !== 'done'), done = all.filter((t) => t.status === 'done');
  const when = (t) => `${aiDay(t)} · ${aiTime(t)}`;
  const step = (st, p, t, note, label) => `<li class="aic-tl-i ${esc(st)}"><i></i><div><b>${label || AI_TK[st] || esc(st)}</b>${label ? '' : `<span class="tk-pct">${p}%</span>`}` +
    `<span class="faint">${when(t)}</span>${note ? `<p>${esc(note)}</p>` : ''}</div></li>`;
  const row = (t) => {
    const log = t.log?.length ? t.log : [{ t: t.updated, status: t.status, progress: aiPct(t), note: t.note }];
    return `<details class="card aic-task ${esc(t.status)}" data-k="task:${t.id}"${aiOpen('task:' + t.id)}><summary><i class="aic-task-dot"></i>` +
      `<div class="aic-task-m"><b>${esc(t.title)}</b><span class="faint">#${t.id} · ${AI_TK[t.status] || esc(t.status)} · ${t.status === 'done' ? 'done' : 'updated'} ${aiWhen(t.updated)}</span></div>` +
      `<div class="aic-task-p"><span class="tk-bar"><i data-w="${aiPct(t)}"></i></span><span class="tk-pct">${aiPct(t)}%</span></div>${ic('angle-down', 'aic-task-chev')}</summary>` +
      `<div class="aic-task-b"><div class="tk-dod"><span>Done when</span>${esc(t.done_when)}</div>` +
      (t.note && t.status !== 'done' ? `<div class="tk-note">${esc(t.note)}</div>` : '') +
      `<div class="tk-dod"><span>Timeline</span></div><ol class="aic-tl">${[...log].reverse().map((e) => step(e.status, e.progress, e.t, e.note)).join('')}` +
      `${step('added', 0, t.created, '', 'Added')}</ol>` +
      `<div class="aic-task-a">${aiDoneBtn(t, 'btn sm aic-task-ok', ic('check-circle'))}${aiCancelBtn(t, 'btn sm aic-task-x', ic('x-circle'))}<button type="button" class="btn sm" data-aitaskask="${t.id}">${ic('chat-round-dots')}Ask JARVIS about it</button></div></div></details>`;
  };
  return aicHead('list-check', 'Tasks', [`${open.length} open`, done.length && `${done.length} done in the last two weeks`].filter(Boolean).join(' · '), `<button type="button" class="btn sm ai-x" data-aiall="0">${ic('xmark')}Close</button>`) +
    (all.length ? (open.length ? `<div class="aic-cat">Open</div>${open.map(row).join('')}` : '') + (done.length ? `<div class="aic-cat">Done</div>${done.map(row).join('')}` : '')
      : '<div class="empty">No tasks yet. When you give JARVIS work that takes more than one reply, it adds it here with what done means, and keeps it up to date.</div>');
}

// cancelling a task: a second tap within 4 s does it. An open one leaves the board and JARVIS is told to stop the work
// (tasks.go); a done one just leaves the board
const aiCancelBtn = (t, cls, icon = '') => `<button type="button" class="${cls}" data-aitaskcancel="${t.id}" data-done="${t.status === 'done' ? 1 : ''}" title="${t.status === 'done' ? 'Take it off the board' : 'JARVIS stops working on it, and it leaves the board'}">${icon}${t.status === 'done' ? 'Remove' : 'Cancel task'}</button>`;
// a task's buttons take a second tap within 4 s: the first one says what the second will do
function aiArmed(btn, ask) {
  if (btn.classList.contains('armed')) return true;
  const label = btn.innerHTML;
  btn.classList.add('armed');
  btn.textContent = ask;
  setTimeout(() => { if (btn.isConnected && btn.classList.contains('armed')) { btn.classList.remove('armed'); btn.innerHTML = label; } }, 4000);
  return false;
}
async function aiTaskCancel(btn) {
  const id = +btn.dataset.aitaskcancel, done = !!btn.dataset.done;
  if (!aiArmed(btn, done ? 'Tap again to remove' : 'Tap again to cancel')) return;
  btn.disabled = true;
  const r = await api('/api/ai/tasks/cancel', { id }).catch(() => ({ status: 0, error: "The panel can't be reached" }));
  if (r.status !== 200 && r.status !== 404) { btn.disabled = false; return toast(r.error || `Error ${r.status}`, true); }
  AI.tasks = AI.tasks.filter((t) => t.id !== id); // the stream confirms it in a moment
  for (const el of $$(`#ai-tasks .tk[data-task="${id}"], #ai-all details[data-k="task:${id}"]`)) el.remove(); // now, not after the tap's hold
  aiTasksRender();
  if (AI.all) aiRender();
  toast(done || r.status === 404 ? `Task #${id} removed` : r.told ? `Task #${id} cancelled: JARVIS is stopping the work` : `Task #${id} cancelled. JARVIS is busy: tell it to stop the work`);
}

// Mark done: the task stays on the board, done at 100% (tasks.go); JARVIS sees it there
const aiDoneBtn = (t, cls, icon = '') => (t.status === 'done' ? '' : `<button type="button" class="${cls}" data-aitaskdone="${t.id}" title="It's finished: done at 100%">${icon}Mark done</button>`);
async function aiTaskDone(btn) {
  const id = +btn.dataset.aitaskdone;
  if (!aiArmed(btn, 'Tap again: done')) return;
  btn.disabled = true;
  const r = await api('/api/ai/tasks/done', { id }).catch(() => ({ status: 0, error: "The panel can't be reached" }));
  if (r.status !== 200) { btn.disabled = false; return toast(r.error || `Error ${r.status}`, true); }
  const t = AI.tasks.find((x) => x.id === id); // the stream confirms it in a moment
  if (t) Object.assign(t, { status: 'done', progress: 100, updated: now(), note: 'Marked done by the owner in the panel' });
  AI.tasks = [...AI.tasks];
  aiTasksRender();
  if (AI.all) { $('#ai-all').dataset.k = ''; aiRender(true); }
  toast(`Task #${id} marked done`);
}

// the task list in place of the chat, and back
function aiAllShow(on) {
  if (on === AI.all) return;
  AI.all = on;
  if (on) { AI.conn = false; aiTasksToggle(false); }
  $('#ai')?.classList.toggle('ai-room', on || AI.conn);
  aiRender(true);
}
function aiTasksRender() {
  const el = $('#ai-tasks');
  if (!el) return;
  const any = aiTasksShown().some((g) => g.length), on = any && !AI.tasksOff;
  el.hidden = !on;
  $('#ai').classList.toggle('has-tasks', on);
  if (!on && AI.tasksOpen) aiTasksToggle(false);
  const btn = $('[data-aitasks]'), open = AI.tasks.filter((t) => t.status !== 'done');
  btn.hidden = !any;
  btn.setAttribute('aria-pressed', String(on));
  btn.setAttribute('aria-label', on ? 'Hide tasks' : 'Show tasks');
  $('.cbadge', btn).textContent = open.length || '';
  $('.cbadge', btn).classList.toggle('warn', open.some((t) => t.status === 'blocked'));
  const k = JSON.stringify(AI.tasks) + aiDay(now());
  if (el.dataset.k !== k) {
    el.innerHTML = aiTasksHTML();
    for (const i of $$('[data-w]', el)) i.style.width = i.dataset.w + '%';
    el.dataset.k = k;
  }
}
// a task in the Tasks tab, open, in view
function aiTaskDetail(id) {
  AI.open.add('task:' + id);
  aiAllShow(true);
  requestAnimationFrame(() => { const d = $(`#ai-all details[data-k="task:${id}"]`); if (d) scrollTo(0, d.getBoundingClientRect().top + scrollY - 120); }); // below the tabs
}
// the composer's list button: the board on or off, remembered on this device
function aiTasksShow(on) {
  AI.tasksOff = !on;
  store.set('aiTasksOff', on ? '0' : '1');
  aiTasksRender();
}
// "Ask JARVIS about it" (the Tasks tab): back to the chat with the task named in the composer
function aiTaskAsk(id) {
  const t = AI.tasks.find((x) => String(x.id) === String(id));
  const ta = $('#ai-text');
  if (!t || !ta) return;
  aiAllShow(false);
  ta.value = `Task #${t.id} (${t.title}): `;
  aiGrow(ta);
  aiSendState();
  ta.focus();
  ta.setSelectionRange(ta.value.length, ta.value.length);
}
// narrower screens: a chip opens the cards over the chat at its task; the same chip, or a tap elsewhere, closes them
function aiTasksToggle(on = !AI.tasksOpen, id) {
  AI.tasksOpen = on;
  if (on) aiConnToggle(false);
  $('#ai')?.classList.toggle('tasks-open', on);
  for (const c of $$('[data-aitask]')) c.setAttribute('aria-pressed', String(on && c.dataset.aitask === String(id)));
  const card = on && $(`#ai-tasks .tk[data-task="${id}"]`);
  if (!card) return;
  card.open = true;
  const d = card.parentElement.closest('details');
  if (d) d.open = true;
  const list = card.closest('.tk-list');
  list.scrollTop = card.offsetTop - 8;
  card.classList.remove('tk-hl');
  void card.offsetWidth; // restart the highlight
  card.classList.add('tk-hl');
}

// ---- starting over, and the model. When a conversation keeps failing (a refusal sticks to its history), a new one
// goes to JARVIS without that history; the chat keeps it, above a divider (aisession.go). /new and /model work too ----
async function aiNewChat() {
  const r = await api('/api/ai/session', {}).catch(() => ({ status: 0 }));
  if (r.status !== 200) return toast(r.error || "Couldn't start a new conversation", true);
  aiConnToggle(false);
  aiAllShow(false);
  toast('New conversation: JARVIS starts fresh, keeping its memory and your task board');
}
async function aiSetModel(m) {
  const r = await api('/api/ai/model', { model: m }).catch(() => ({ status: 0 }));
  if (r.status !== 200) return toast(r.error || "Couldn't switch the model", true);
  toast(m ? `The panel chat now uses ${m}` : 'The panel chat uses Hermes’ default model');
}
const aiStuckHTML = () => `<div class="ai-btns"><div class="ai-btns-r"><button type="button" class="ai-btn primary" data-ainew>${ic('arrow-rotate')}New conversation</button>` +
  `<button type="button" class="ai-btn" data-aiopen="control:overview">${ic('tuning')}Change model</button></div></div>`;

// ---- files and photos for JARVIS: attach, drop anywhere on the page, or paste. A photo is scaled down here (the model reads
// up to ~1568 px; Hermes takes 10 MB a request) and uploaded at once; sending hands the panel their paths, and
// it gives them to JARVIS as images (assistant.go aiAttach) ----
const AI_IMG_PX = 1568;
async function aiPrepImage(file) {
  const plain = /^image\/(png|jpeg|gif|webp)$/.test(file.type) && file.size <= 1.5e6;
  let bmp;
  try { bmp = await createImageBitmap(file); } catch { if (plain) return file; throw new Error(`${file.name || 'That image'}: can't read it`); }
  const k = Math.min(1, AI_IMG_PX / Math.max(bmp.width, bmp.height));
  if (plain && k === 1) { bmp.close(); return file; }
  const c = document.createElement('canvas');
  c.width = Math.round(bmp.width * k);
  c.height = Math.round(bmp.height * k);
  c.getContext('2d').drawImage(bmp, 0, 0, c.width, c.height);
  bmp.close();
  const blob = await new Promise((ok) => c.toBlob(ok, 'image/jpeg', 0.86));
  if (!blob) throw new Error("Couldn't prepare that image");
  return new File([blob], (file.name || 'photo').replace(/\.\w+$/, '') + '.jpg', { type: 'image/jpeg' });
}
// Any other file (a spreadsheet, a PDF, a zip…) goes as it is, and JARVIS opens it from its path; so does a photo
// this browser can't read (it can't be scaled down for the model to see)
function aiAddFiles(list) {
  for (const file of list) {
    const img = /^image\//.test(file.type) && file.type !== 'image/svg+xml';
    if (AI.files.length >= 10) { toast('Up to 10 files a message', true); break; }
    if (img && AI.files.filter((f) => !f.doc).length >= 6) { toast('Up to 6 photos a message', true); continue; }
    if (file.size > 25 << 20) { toast(`${file.name}: files up to 25 MB`, true); continue; }
    const f = img ? { url: URL.createObjectURL(file), path: '' } : { doc: true, name: file.name || 'file', size: file.size, url: '', path: '' };
    const asFile = () => { aiRmURL(f); Object.assign(f, { doc: true, name: file.name || 'image', size: file.size }); return file; };
    f.up = (img ? aiPrepImage(file).catch(asFile) : Promise.resolve(file)).then(uploadFile)
      .then((p) => { f.path = p; aiFilesRender(); }, (e) => { toast(e.message, true); aiRmFile(f); });
    AI.files.push(f);
  }
  aiFilesRender();
}
const aiRmURL = (f) => { if (f.url) URL.revokeObjectURL(f.url); f.url = ''; };
function aiRmFile(f) { aiRmURL(f); AI.files = AI.files.filter((x) => x !== f); aiFilesRender(); }
function aiFilesRender() {
  const box = $('#ai-files');
  if (!box) return;
  box.hidden = !AI.files.length;
  const rm = (i) => `<button type="button" data-airmfile="${i}" aria-label="Remove">${ic('xmark')}</button>`;
  box.innerHTML = AI.files.map((f, i) => (f.doc
    ? `<div class="fchip doc${f.path ? '' : ' up'}">${fxBadge(f.name)}<span>${esc(f.name)}</span><small>${fmtB(f.size)}</small>${rm(i)}</div>`
    : `<div class="fchip${f.path ? '' : ' up'}"><img src="${esc(f.url)}" alt="">${rm(i)}</div>`)).join('');
  aiSendState();
}
let aiDragN = 0;
const aiDragOK = (e) => S.view === 'ai' && !!$('#ai-foot') && !e.target.closest?.('#drawer') && [...(e.dataTransfer?.types || [])].includes('Files');
const aiDropHint = (on) => $('#ai-foot')?.classList.toggle('dropping', on);
document.addEventListener('dragenter', (e) => { if (aiDragOK(e) && aiDragN++ === 0) aiDropHint(true); });
document.addEventListener('dragleave', (e) => { if (aiDragOK(e) && --aiDragN <= 0) { aiDragN = 0; aiDropHint(false); } });
document.addEventListener('dragover', (e) => { if (aiDragOK(e)) e.preventDefault(); });
document.addEventListener('drop', (e) => { if (!aiDragOK(e)) return; aiDragN = 0; aiDropHint(false); e.preventDefault(); aiAddFiles([...e.dataTransfer.files]); });
// "[Attached image: <path>]" and "[Attached file: <path>]" lines (assistant.go aiAttach) show as the photos and
// the files above your message
function aiAttached(text) {
  const imgs = [], docs = [];
  const rest = text.replace(/\n*\[Attached (image|file): (\/[^\]\n]+)\]/g, (_, k, p) => { (k === 'image' ? imgs : docs).push(p); return ''; });
  return [rest.trim(), imgs, docs];
}

// ---- drawing ----
function aiRender(jump) {
  const body = $('#ai-body');
  if (!body) return;
  auroraEnergy(!!AI.live); // outside the scrolling chat: safe to update any time
  $('[data-aiconn]')?.setAttribute('aria-pressed', String(AI.conn));
  if (held() && !jump) { AI.stale = true; return; } // flushStale (app.js) draws it when the finger or scroll stops
  AI.stale = false;
  const nearEnd = innerHeight + scrollY >= document.documentElement.scrollHeight - 160;
  const hist = $('#ai-hist'), now = $('#ai-now'), conns = $('#ai-conns'), all = $('#ai-all');
  const away = AI.conn || AI.all; // settings, or the task list, in place of the chat
  hist.hidden = now.hidden = away;
  conns.hidden = !AI.conn;
  all.hidden = !AI.all;
  keepScroll([window], () => {
    if (AI.conn) { // the control room (ai-control.js) redraws when its data changes, so its fields keep focus
      const ck = AIC.v + ':' + AIC.tab + ':' + (AIC.tab === 'overview' ? JSON.stringify([AI.state, AI.google, AI.tg, AI.models]) : '');
      const typing = conns.contains(document.activeElement) && document.activeElement.matches('input, textarea, select');
      if (conns.dataset.k !== ck && !typing) { conns.innerHTML = aicHTML(); aicAfter(conns); conns.dataset.k = ck; }
      return;
    }
    if (AI.all) {
      const k = JSON.stringify(AI.tasks) + aiDay(Date.now() / 1000); // "Today" moves on
      if (all.dataset.k !== k) { all.innerHTML = aiTasksAllHTML(); for (const i of $$('[data-w]', all)) i.style.width = i.dataset.w + '%'; all.dataset.k = k; }
      return;
    }
    const key = `${AI.v}:${AI.more}:${AI.loadingMore}:${!!AI.state}:${AI.state?.err || ''}:${!!AI.live}`;
    if (key !== AI.drawn) { // the history only redraws when it changed: its diagrams and charts stay put
      hist.innerHTML = aiHistHTML();
      richEnhance(hist);
      AI.drawn = key;
    }
    now.innerHTML = aiNowHTML();
  });
  if (away) { if (jump) scrollTo(0, 0); } else if (jump || nearEnd) scrollTo(0, document.documentElement.scrollHeight);
  aiSendState();
  aiDown();
}

// More than a screen above the latest: the jump-down button by the composer shows (as in the terminal chats)
function aiDown() {
  $('#ai-down')?.classList.toggle('on', !AI.conn && !AI.all && document.documentElement.scrollHeight - scrollY - innerHeight > innerHeight);
}
addEventListener('scroll', () => { if (S.view === 'ai') aiDown(); }, { passive: true });

// "[Replying to: "…"] text" (a Telegram reply) shows the quote above the message
function aiQuote(text) {
  const m = /^\[Replying to: "([\s\S]*?)"\]\s*/.exec(text) || /^\[Replying to: (your previous message)\]\s*/.exec(text);
  return m ? [m[1], text.slice(m[0].length)] : ['', text];
}

function aiUserHTML(it, sending, extra = '') {
  const [q, rest] = aiQuote(it.text || '');
  const [text, imgs, docs] = aiAttached(rest);
  return `<div class="msg-u ai-u${sending ? ' sending' : ''}">${aiMeta(it.ch === 'agent' ? 'Claude' : 'You', it.ch, aiTime(it.t))}` +
    `${q ? `<div class="ai-q">${esc(q.length > 280 ? q.slice(0, 280) + '…' : q)}</div>` : ''}` +
    (imgs.length ? `<div class="uimgs">${imgs.map((p) => { const u = '/api/ai/file?path=' + encodeURIComponent(p); return `<a href="${esc(u)}" target="_blank" rel="noopener"><img src="${esc(u)}" alt="Sent photo" loading="lazy"></a>`; }).join('')}</div>` : '') +
    (docs.length ? `<div class="udocs">${docs.map((p) => `<a class="udoc" href="${esc('/api/ai/file?path=' + encodeURIComponent(p))}" download>` +
      `${fxBadge(p)}<span>${esc(uploadLabel(p.split('/').pop()))}</span>${ic('download2')}</a>`).join('')}</div>` : '') +
    (text ? `<div class="bubble">${mdInline(text)}</div>` : '') + extra + '</div>';
}

// Hermes waking itself (a subagent or background process finished, …): a folded card, not your message
const AI_EV = { async_delegation_complete: ['Subagent results', 'people'], process_complete: ['Background process finished', 'terminal-square'],
  model_switch: ['Model switched', 'cpu'], skill_invocation: ['Skill', 'wand-sparkle'], auto_continue: ['Continuing', 'bolt'] };
function aiEventHTML(it) {
  const [label, icon] = AI_EV[it.name] || ['Notification', 'bell'];
  const text = it.text || '';
  const tasks = [...text.matchAll(/^--- (\S) TASK \d+\/\d+: (.*?) {2}\(status=/gm)]; // a fan-out's tasks: "✓ goal · ✗ goal"
  const preview = tasks.length ? tasks.map((m) => m[1] + ' ' + m[2]).join(' · ') : text.split('\n')[0].replace(/^\[|\]$/g, '');
  return `<details class="ai-ev" data-k="${esc(it.k)}"${aiOpen(it.k)}><summary><span class="tr-ic">${ic(icon)}</span><b>${label}</b>` +
    `<span class="ai-ev-p">${esc(preview)}</span><span class="ai-ch ch-${esc(it.ch)}">${esc(AI_CH[it.ch] || it.ch)}</span><span class="ai-ev-t">${aiTime(it.t)}</span>` +
    `</summary><pre class="ai-ev-b">${esc(text)}</pre></details>`;
}

// ---- the trace: a turn's thinking, narration and tool calls on one rail ----
// "mcp__vm__fleet_status" → ["fleet_status", "vm"]; built-in tools keep their name
const aiTool = (name) => { const m = /^mcp__(.+?)__(.+)$/.exec(name || ''); return m ? [m[2], m[1]] : [name || 'tool', '']; };
const AI_TOOL_IC = [[/^vm$/, 'server'], [/terminal|shell|process|exec|code/, 'terminal-square'], [/file|read|write|patch|edit/, 'document-code'],
  [/web|browser|fetch|url|search/, 'global'], [/memory|session/, 'layers'], [/deleg|agent/, 'people'], [/cron|schedule/, 'clock-circle'],
  [/skill/, 'wand-sparkle'], [/image|vision/, 'image3'], [/google|gmail|calendar|mail/, 'chat-round-dots']];
const aiToolIc = (name, src) => (AI_TOOL_IC.find(([re]) => re.test(src || name)) || [0, 'bolt'])[1];
const aiSecs = (s) => (s < 1 ? Math.max(0.1, s).toFixed(1) + 's' : s < 60 ? s.toFixed(s < 10 ? 1 : 0) + 's' : fmtDur(s));
const aiOpen = (k) => (AI.open.has(k) ? ' open' : '');

function aiStepHTML(s) {
  if (s.k === 'note') return `<div class="tr-s tr-note"><i class="tr-n"></i><div class="tr-t">${mdInline(s.text || '')}</div></div>`;
  if (s.k === 'think') {
    const first = (s.text || '').trim().split('\n')[0];
    return `<details class="tr-s tr-think" data-k="${esc(s.key)}"${aiOpen(s.key)}><summary><i class="tr-n"></i><span class="tr-l">Thinking</span>` +
      `<span class="tr-p">${esc(first)}</span></summary><div class="tr-b"><div class="tr-thought">${esc(s.text || '')}</div></div></details>`;
  }
  const [name, src] = aiTool(s.name);
  const st = s.state || 'ok';
  const tail = st === 'run' ? '<span class="tr-d run">running</span>' : st === 'err' ? '<span class="tr-d err">failed</span>' : s.dur ? `<span class="tr-d">${aiSecs(s.dur)}</span>` : '';
  const io = (label, text) => `<div class="tr-io"><span>${label}</span><pre>${esc(text)}</pre></div>`;
  const body = (s.text ? io('in', s.text) : '') + (s.res ? io('out', s.res) : '');
  return `<details class="tr-s tr-tool ${st}" data-k="${esc(s.key)}"${aiOpen(s.key)}${body ? '' : ' data-empty'}><summary><i class="tr-n"></i>` +
    `<span class="tr-ic">${ic(aiToolIc(name, src))}</span><b>${esc(name)}</b>${src ? `<span class="tr-src">${esc(src)}</span>` : ''}` +
    `<code>${esc(s.text || '')}</code>${tail}</summary>${body ? `<div class="tr-b">${body}</div>` : ''}</details>`;
}

// a long trace folds its middle: the start and the latest steps stay in view
function aiTraceHTML(steps, live, key) {
  if (!steps.length) return '';
  let inner;
  if (!live && steps.length > 9) {
    const fk = key + ':fold';
    inner = steps.slice(0, 2).map(aiStepHTML).join('') +
      `<details class="tr-fold" data-k="${esc(fk)}"${aiOpen(fk)}><summary><i class="tr-n"></i><span>${plural(steps.length - 6, 'more step')}</span></summary>${steps.slice(2, -4).map(aiStepHTML).join('')}</details>` +
      steps.slice(-4).map(aiStepHTML).join('');
  } else inner = steps.map(aiStepHTML).join('');
  return `<div class="tr${live ? ' tr-live' : ''}">${inner}</div>`;
}

function aiHistHTML() {
  // Hermes saves a turn as it goes (your message, then each step): while it runs, the live view below shows it,
  // so the history leaves out the Panel's items from that turn on, and takes them back once it's done
  const cut = AI.live ? AI.live.since - 1 : Infinity;
  const its = AI.live ? AI.items.filter((it) => !(it.ch === 'panel' && it.t >= cut)) : AI.items;
  if (!AI.state) return '<div class="skel"></div><div class="skel"></div>';
  if (!its.length && !AI.live) {
    return `<div class="empty ai-empty">${ic('sparkle')}<b>Say hi to JARVIS</b>` +
      '<span>Ask for anything on this VM: start an agent, check CI, chart your data, read your mail. Your Telegram chat shows up here too.</span>' +
      (AI.state.err ? `<span class="err">${esc(AI.state.err)}</span>` : '') + '</div>'; // no error yet: still connecting
  }
  let h = AI.more ? `<div class="ai-more"><button type="button" class="btn sm" data-aimore>${AI.loadingMore ? 'Loading…' : 'Earlier messages'}</button></div>` : '';
  let day = '';
  let turn = null; // the JARVIS turn being drawn: { ch, steps, key }
  const closeTurn = () => {
    if (!turn) return;
    h += aiTraceHTML(turn.steps, false, turn.key) + '</div>';
    turn = null;
  };
  // each day is a section: its pill sticks only while the day is on screen, then the next day's takes over
  const dayPill = (t) => { const d = aiDay(t); if (d !== day) { closeTurn(); h += (day ? '</section>' : '') + `<section class="ai-dayg"><div class="ai-day"><span>${esc(d)}</span></div>`; day = d; } };
  const reply = []; // what the owner said after each item (buttons it answered show as chosen)
  for (let i = its.length - 1, next = ''; i >= 0; i--) { reply[i] = next; if (its[i].role === 'user') next = its[i].text || ''; }
  // the Panel chat's last turn failed (a refusal sticks to a conversation's history): offer the way out
  const lastErr = its.findLastIndex((x) => x.ch === 'panel' && x.role === 'error');
  const stuck = lastErr >= 0 && !its.slice(lastErr + 1).some((x) => x.ch === 'panel' && x.role === 'assistant') ? lastErr : -1;
  for (const [i, it] of its.entries()) {
    dayPill(it.t);
    if (it.role === 'user') { closeTurn(); h += aiUserHTML(it); continue; }
    if (it.role === 'event' && it.name === 'new_chat') { closeTurn(); h += '<div class="ai-new"><span>New conversation</span></div>'; continue; }
    if (it.role === 'event') { closeTurn(); h += aiEventHTML(it); continue; }
    if (turn && turn.ch !== it.ch) closeTurn(); // another channel's turn interleaved in time
    if (!turn) { turn = { ch: it.ch, steps: [], key: it.k }; h += `<div class="ai-a">${aiMeta('JARVIS', it.ch, aiTime(it.t))}`; }
    if (it.role === 'think' || it.role === 'note') { turn.steps.push({ k: it.role, text: it.text, key: it.k }); continue; }
    if (it.role === 'tool') { turn.steps.push({ k: 'tool', name: it.name, text: it.text, res: it.res, state: it.run ? 'run' : 'ok', key: it.k }); continue; }
    h += aiTraceHTML(turn.steps, false, turn.key); // the work, then what it came to
    turn.steps = [];
    h += it.role === 'error' ? `<div class="ai-err">${ic('alert-triangle')}<div class="md">${md(it.text || '')}</div></div>${i === stuck ? aiStuckHTML() : ''}` : `<div class="md ai-ans">${richMd(it.text || '', reply[i] || (AI.live && i === its.length - 1 ? AI.live.user : ''))}</div>` +
      `<div class="msg-act"><button type="button" class="abtn" data-aicopy="${esc(it.k)}" title="Copy" aria-label="Copy">${ic('copy')}</button>` +
      `<button type="button" class="abtn" data-aiquote="${esc(it.k)}" title="Quote in your reply" aria-label="Quote in your reply">${ic('quote-up')}</button></div>`; // as a terminal chat's replies
  }
  closeTurn();
  return h + (day ? '</section>' : '');
}

// the turn in progress: your message, the steps so far (the running one pulsing), the answer as it streams
// what you sent while JARVIS was answering: it goes out when the turn ends, in order (assistant.go's queue)
const aiQueueHTML = () => AI.queue.map((q) => aiUserHTML({ text: q.text, ch: 'panel', t: q.t }, false,
  `<span class="q-l">${ic('clock-circle')}Sends when JARVIS is done<button type="button" class="q-x" data-aiunq="${esc(q.id)}" title="Take it back into your box">Edit</button></span>`)
  .replace('class="msg-u ai-u', 'class="msg-u ai-u queued')).join('');

function aiNowHTML() {
  const l = AI.live;
  if (!l) return aiQueueHTML();
  const steps = (l.steps || []).map((s, i) => ({ ...s, key: 'live:' + i }));
  const run = steps.findLast((s) => s.state === 'run');
  const status = run ? `Running ${aiTool(run.name)[0]}` : l.text ? '' : steps.length ? 'Working' : 'Thinking';
  return aiUserHTML({ text: l.user, ch: 'panel', t: l.since }, true) +
    `<div class="ai-a ai-going">${aiMeta('JARVIS', 'panel', 'now')}${aiTraceHTML(steps, true, 'live')}` +
    (l.text ? `<div class="md ai-ans">${md(l.text)}</div>` : '') +
    (status ? `<div class="ai-status"><i class="ai-pulse"></i>${shim(status + '…')}</div>` : '') + '</div>' + aiQueueHTML();
}

// the control room's overview ends with the channels and connections (ai-control.js draws the rest)
function aiConnHTML() {
  const st = AI.state || {};
  const p = st.platforms || {};
  const g = AI.google;
  const row = (ok, title, detail, action = '') => `<div class="check ${ok === null ? '' : ok ? 'ok' : 'warn'}"><span class="ic">${ic(ok ? 'check-circle-f' : ok === null ? 'info-circle' : 'alert-triangle')}</span>` +
    `<div class="ai-row"><b>${title}</b><span class="muted">${detail}</span></div>${action}</div>`;
  const dash = (path, label) => `<a class="btn sm" href="/hermes${path}" target="_blank" rel="noopener">${label}${ic('arrow-up-right')}</a>`;
  return `<div class="sec-h"><h2>${ic('plug')}Channels</h2></div><div class="card flush">` +
    row(!!st.up, 'JARVIS (Hermes)', st.up ? 'Running' + (st.model ? ' · last model ' + esc(st.model) : '') : esc(st.err || 'Not running: start projects/vm-agent/run-agent.sh'), dash('/logs', 'Logs')) +
    row(p.api_server === 'connected', 'Panel chat', esc(p.api_server || 'not connected')) +
    aiTelegramHTML(p.telegram, row) +
    '</div>' +
    `<div class="sec-h"><h2>${ic('sparkle')}Models</h2><span class="muted">Pick per task with /model in any chat</span></div><div class="card flush">` +
    row(null, 'Providers and keys', 'Default: Claude Opus 5.5 (ANTHROPIC_API_KEY). Add other providers’ keys (OpenRouter, OpenAI) to switch per task with /model.', dash('/models', 'Models')) +
    '</div>' +
    `<div class="sec-h"><h2>${ic('global')}Google</h2><span class="muted">Gmail and Calendar</span></div><div class="card flush">` +
    row(g ? g.client : false, 'OAuth client', g?.client ? 'Saved' : 'Google Cloud → APIs: enable Gmail and Calendar → Credentials → OAuth client, type Desktop app → download the JSON. Publish the consent screen to production (testing sign-ins expire in 7 days).',
      `<label class="btn sm">${ic('upload2')}${g?.client ? 'Replace' : 'Upload'}<input type="file" accept="application/json,.json" data-aigoogle hidden></label>`) +
    row(g ? g.token : false, 'Signed in', g?.token ? 'JARVIS can read and send mail and manage your calendar' : 'After the upload, ask JARVIS to connect Google: it sends a sign-in link, you paste back the page it lands on.',
      g?.client && !g?.token ? `<button type="button" class="btn sm primary" data-aiask="Connect my Google account (Gmail and Calendar) with the google-workspace skill. Send me the sign-in link and tell me what to paste back.">Connect</button>` : '') +
    '</div>' +
    `<div class="sec-h"><h2>${ic('puzzle')}More connections</h2></div><div class="card flush">` +
    row(null, 'MCP servers', 'Any service with an MCP server: Notion, Linear, GitHub, Slack, databases… from the catalog or by URL/command.', dash('/mcp', 'MCP')) +
    row(null, 'Everything else', 'Hermes’ full dashboard: config, sessions, pairing, webhooks, API keys.', dash('/', 'Dashboard')) +
    '</div>';
}

// Telegram, set right here: the bot token (write-only: the panel only ever says whether one is saved) and your user
// id go to Hermes' .env; a restart of the gateway connects it. AI.tg: what /api/ai/telegram reports.
function aiTelegramHTML(state, row) {
  const tg = AI.tg || {};
  const ready = tg.token && tg.users;
  const detail = state === 'connected' ? `Connected${tg.users ? ' · only ' + esc(tg.users) + ' can talk to it' : ''}`
    : state ? esc(state) + (ready ? ' · restart if you just saved' : '')
    : ready ? 'Saved: restart JARVIS to connect' : 'Not set up yet: two things from Telegram, below';
  const restart = ready && state !== 'connected'
    ? `<button type="button" class="btn sm${AIC.arm === 'restart' ? ' danger' : ' primary'}" data-aicrestart>${ic('arrow-rotate')}${AIC.arm === 'restart' ? 'Tap again to restart' : 'Restart to connect'}</button>` : '';
  return row(state === 'connected' ? true : false, 'Telegram', detail, restart) +
    `<form class="aic-tg" data-aitgform autocomplete="off">` +
    `<label class="fld">Bot token <span class="faint">${tg.token ? '· saved' : '· from @BotFather: /newbot'}</span>` +
    `<input id="tg-token" type="password" autocomplete="off" spellcheck="false" placeholder="${tg.token ? 'Saved · type a new one to replace it' : '123456789:AA…'}"></label>` +
    `<label class="fld">Your Telegram user id <span class="faint">· from @userinfobot; only these ids can talk to JARVIS</span>` +
    `<input id="tg-users" inputmode="numeric" autocomplete="off" placeholder="e.g. 123456789" value="${esc(tg.users || '')}"></label>` +
    `<div class="aic-tg-a"><button type="submit" class="btn sm primary">${ic('check')}Save</button></div></form>`;
}

async function aiTelegramLoad() {
  const r = await api('/api/ai/telegram').catch(() => null);
  if (r?.status === 200) { AI.tg = r; if (AI.conn) aicBump(); }
}

document.addEventListener('submit', async (e) => {
  if (!e.target.matches('[data-aitgform]')) return;
  e.preventDefault();
  const token = $('#tg-token').value.trim();
  const users = $('#tg-users').value.trim();
  const body = {};
  if (token) body.token = token;
  if (users && users !== (AI.tg?.users || '')) body.users = users;
  if (!Object.keys(body).length) return toast('Nothing new to save');
  const r = await api('/api/ai/telegram', body).catch(() => ({ status: 0, error: "The panel can't be reached" }));
  if (r.status !== 200) return toast(r.error || `Error ${r.status}`, true);
  $('#tg-token').value = '';
  AI.tg = r;
  toast(r.token && r.users ? 'Saved. Restart JARVIS to connect Telegram' : 'Saved');
  aicBump();
});

// ---- actions ----
function aiSendState() {
  const b = $('#ai-send');
  if (!b) return;
  const ta = $('#ai-text'), has = !!ta.value.trim() || AI.files.length > 0;
  const stop = !!AI.live && !has; // while JARVIS answers: Stop, or, once you've typed, queue it
  b.classList.toggle('stop', stop);
  b.innerHTML = ic(stop ? 'stop-f' : 'arrow-up');
  b.setAttribute('aria-label', stop ? 'Stop' : AI.live ? 'Queue it: sends when JARVIS is done' : 'Send');
  b.title = b.getAttribute('aria-label');
  b.disabled = !stop && (!has || AI.files.some((f) => !f.path)); // photos still uploading
  const clr = $('#ai-clear');
  if (clr) clr.hidden = !has;
  ta.placeholder = AI.live ? 'Message JARVIS: sends when it’s done' : 'Message JARVIS';
}

async function aiSubmit() {
  const ta = $('#ai-text');
  const text = ta.value.trim();
  const paths = (doc) => AI.files.filter((f) => !!f.doc === doc).map((f) => f.path).filter(Boolean);
  const images = paths(false), files = paths(true);
  const slash = /^\/(new|reset|clear|model)\b\s*([\s\S]*)$/i.exec(text); // the panel's own: they never reach the model
  if (slash && !images.length && !files.length) {
    ta.value = '';
    try { sessionStorage.removeItem('aiDraft'); } catch { /* no storage */ }
    aiGrow(ta);
    aiSendState();
    const arg = slash[2].trim();
    if (slash[1].toLowerCase() !== 'model') return aiNewChat();
    return arg ? aiSetModel(arg === 'default' ? '' : arg) : aiGo('control:overview');
  }
  if (!text && !images.length && !files.length) { if (AI.live) await api('/api/ai/stop', {}).catch(() => {}); return; } // the empty box's button is Stop
  if (!(await aiSend(text, images, files))) return;
  for (const f of AI.files) aiRmURL(f);
  AI.files = [];
  aiFilesRender();
  ta.value = '';
  try { sessionStorage.removeItem('aiDraft'); } catch { /* no storage */ }
  aiGrow(ta);
  aiSendState();
}

// aiSend(text): a message to JARVIS from the Panel (the composer, a reply button, a rule taught in Memory). While
// it answers, the panel queues it and sends it when that turn ends.
async function aiSend(text, images = [], files = []) {
  if (!String(text ?? '').trim() && !images.length && !files.length) { $('#ai-text')?.focus(); return false; } // nothing to send: the box, to type it
  const r = await api('/api/ai/send', { text, images, files }).catch(() => ({ status: 0, error: "The panel can't be reached" }));
  if (r.status !== 200) { toast(r.error || `Error ${r.status}`, true); return false; }
  aiConnToggle(false); // sent from settings or the task list: show the chat it went to
  aiAllShow(false);
  return true;
}

// a reply button's "open": a terminal's chat, a panel page, or a tab of the control room
function aiGo(target) {
  const [kind, rest] = [target.split(':')[0], target.slice(target.indexOf(':') + 1)];
  if (target === 'control:tasks') return aiAllShow(true); // JARVIS's buttons still say so
  if (kind === 'control') { aiConnToggle(true); return aicTab(rest || 'overview'); }
  noteGo(target);
}

async function aiMore() {
  if (AI.loadingMore || !AI.items.length) return;
  AI.loadingMore = true;
  aiRender();
  const r = await api(`/api/ai/history?before=${encodeURIComponent(AI.items[0].k)}`).catch(() => ({ status: 0 }));
  AI.loadingMore = false;
  if (r.status === 200) {
    const h0 = document.documentElement.scrollHeight;
    AI.items = (r.items || []).concat(AI.items);
    AI.more = !!r.more;
    AI.v++;
    aiRender();
    scrollTo(0, scrollY + document.documentElement.scrollHeight - h0); // stay on the message you were reading
  } else aiRender();
}

async function aiGoogle() {
  const r = await api('/api/ai/google').catch(() => null);
  if (r?.status === 200) { AI.google = r; if (AI.conn) aiRender(); }
}

function aiClick(e) {
  const chip = e.target.closest('[data-aitask]');
  if (chip) return aiTasksToggle(!(AI.tasksOpen && chip.getAttribute('aria-pressed') === 'true'), chip.dataset.aitask);
  if (AI.tasksOpen && !e.target.closest('#ai-tasks')) aiTasksToggle(false);
  if (AI.conn && aicClick(e)) return;
  const unq = e.target.closest('[data-aiunq]');
  if (unq) { // Edit: out of the queue, back into the box (after what's there), as ↑ does in a terminal
    unq.disabled = true;
    const q = AI.queue.find((x) => x.id === unq.dataset.aiunq);
    return api('/api/ai/unqueue', { id: unq.dataset.aiunq }).then((r) => {
      if (r.status !== 200) return toast('Already sent', true);
      const ta = $('#ai-text');
      if (q && ta) { ta.value = ta.value.trim() ? ta.value.replace(/\s+$/, '') + '\n\n' + q.text : q.text; aiGrow(ta); aiSendState(); ta.focus(); }
    });
  }
  if (e.target.closest('[data-aiclear]')) { // Clear: the box, its photos, the draft
    const ta = $('#ai-text');
    for (const f of AI.files) URL.revokeObjectURL(f.url);
    AI.files = [];
    aiFilesRender();
    ta.value = '';
    try { sessionStorage.removeItem('aiDraft'); } catch { /* no storage */ }
    aiGrow(ta);
    aiSendState();
    return ta.focus();
  }
  if (e.target.closest('[data-ainew]')) return aiNewChat();
  if (e.target.closest('#ai-down')) return scrollTo({ top: document.documentElement.scrollHeight, behavior: 'smooth' });
  const att = e.target.closest('[data-aiattach],[data-airmfile]');
  if (att) { const f = AI.files[+att.dataset.airmfile]; return att.dataset.airmfile ? f && aiRmFile(f) : $('#ai-file').click(); }
  const al = e.target.closest('[data-aiall]');
  if (al) return aiAllShow(al.dataset.aiall === '1');
  const cx = e.target.closest('[data-aitaskcancel]');
  if (cx) return aiTaskCancel(cx);
  const dn = e.target.closest('[data-aitaskdone]');
  if (dn) return aiTaskDone(dn);
  const tx = e.target.closest('[data-aitaskopen]');
  if (tx) return aiTaskDetail(tx.dataset.aitaskopen);
  const tb = e.target.closest('[data-aitasks],[data-aitaskask]');
  if (tb) return tb.dataset.aitaskask ? aiTaskAsk(tb.dataset.aitaskask) : aiTasksShow(AI.tasksOff);
  const cp = e.target.closest('.cb-copy');
  if (cp) return copy(cp.closest('.cb').querySelector('code').textContent, cp);
  if (e.target.closest('[data-aiother]')) { const ta = $('#ai-text'); ta.focus(); ta.setSelectionRange(ta.value.length, ta.value.length); return; } // "Other": type your own answer
  const rq = e.target.closest('[data-aicopy],[data-aiquote]'); // a reply's Copy or Quote (app.js copy, quoteIntoComposer)
  if (rq) { const text = AI.items.find((x) => x.k === (rq.dataset.aicopy ?? rq.dataset.aiquote))?.text || ''; return rq.dataset.aicopy !== undefined ? copy(text, rq) : quoteIntoComposer(text); }
  if (e.target.closest('details[data-empty] > summary')) return e.preventDefault(); // a call with nothing to show
  const t = e.target.closest('[data-aiconn],[data-aimore],[data-aiask],[data-aisend],[data-aiopen],[data-aisort]');
  if (!t) return;
  if (t.hasAttribute('data-aiconn')) aiConnToggle();
  else if (t.hasAttribute('data-aimore')) aiMore();
  else if (t.dataset.aiopen) aiGo(t.dataset.aiopen);
  else if (t.dataset.aisort) richSort(t.closest('th'));
  else aiSend(t.dataset.aisend ?? t.dataset.aiask).then((ok) => { if (ok && t.dataset.aisend) { t.classList.add('chosen'); for (const b of t.parentElement.querySelectorAll('[data-aisend]')) b.disabled = true; } });
}

document.addEventListener('change', (e) => { if (e.target.matches('[data-aicmodel]')) aiSetModel(e.target.value); });
document.addEventListener('change', async (e) => {
  if (!e.target.matches('[data-aigoogle]')) return;
  const f = e.target.files[0];
  e.target.value = '';
  if (!f) return;
  const r = await fetch('/api/ai/google', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: await f.text() })
    .then(async (x) => ({ status: x.status, ...(await x.json().catch(() => ({}))) })).catch(() => ({ status: 0 }));
  if (r.status !== 200) return toast(r.error || `Error ${r.status}`, true);
  toast('Google client saved');
  aiGoogle();
});

document.addEventListener('focusout', (e) => { if (AI.conn && e.target.closest?.('#ai-conns')) setTimeout(() => aiRender(), 0); }); // catch up on what typing held back

document.addEventListener('visibilitychange', () => {
  if (S.view !== 'ai') return;
  if (document.hidden) aiStop(); else if (!AI.es) aiConnect();
});
