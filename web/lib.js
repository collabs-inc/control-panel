'use strict';
// Pure helpers shared by app.js (browser globals) and lib_test.js (node --test).

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);

// Items in the order of `prev` (a list of keys); items not in `prev` go last, in their incoming order.
// Keeps rows under the user's finger while live data changes.
function keepOrder(prev, items, key) {
  const pos = new Map(prev.map((k, i) => [k, i]));
  const at = (x) => pos.get(key(x)) ?? Number.MAX_SAFE_INTEGER;
  return [...items].sort((a, b) => at(a) - at(b));
}

// ---- terminal colours ----

const PALETTE = ['#20242b', '#f0524f', '#5cc26c', '#e5c07b', '#61afef', '#c678dd', '#56b6c2', '#c8ccd4',
  '#5c6370', '#ff6b68', '#7fd88f', '#f5d78e', '#7ab7ff', '#dd9ef5', '#6fd3de', '#ffffff'];
const hex2 = (x) => Math.max(0, Math.min(255, x | 0)).toString(16).padStart(2, '0');
const rgb = (r, g, b) => '#' + hex2(r) + hex2(g) + hex2(b);

function color256(n) {
  if (n < 16) return PALETTE[n];
  if (n < 232) {
    const v = [0, 95, 135, 175, 215, 255];
    n -= 16;
    return rgb(v[Math.floor(n / 36)], v[Math.floor(n / 6) % 6], v[n % 6]);
  }
  const g = 8 + (n - 232) * 10;
  return rgb(g, g, g);
}

// ansiSegments splits one line of terminal output into styled runs {text, fg, bg, b, d, i, u, inv}.
// Only SGR is interpreted; every other escape sequence and control character is dropped.
function ansiSegments(line) {
  const out = [];
  let st = {};
  let text = '';
  const push = () => {
    text = text.replace(/[\x00-\x08\x0b-\x1f\x7f]/g, '');
    if (text) out.push({ text, ...st });
    text = '';
  };
  const re = /\x1b\[([0-9;:?]*)([ -/]*[@-~])|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b[@-Z\\-_]/g;
  let last = 0;
  let m;
  while ((m = re.exec(line))) {
    text += line.slice(last, m.index);
    last = re.lastIndex;
    if (m[2] !== 'm') continue;
    push();
    const ps = m[1] === '' ? [0] : m[1].split(/[;:]/).map(Number);
    for (let i = 0; i < ps.length; i++) {
      const p = ps[i];
      if (p === 0) st = {};
      else if (p === 1) st.b = true;
      else if (p === 2) st.d = true;
      else if (p === 3) st.i = true;
      else if (p === 4) st.u = true;
      else if (p === 7) st.inv = true;
      else if (p === 22) { delete st.b; delete st.d; }
      else if (p === 23) delete st.i;
      else if (p === 24) delete st.u;
      else if (p === 27) delete st.inv;
      else if (p >= 30 && p <= 37) st.fg = PALETTE[p - 30];
      else if (p >= 90 && p <= 97) st.fg = PALETTE[p - 82];
      else if (p === 39) delete st.fg;
      else if (p >= 40 && p <= 47) st.bg = PALETTE[p - 40];
      else if (p >= 100 && p <= 107) st.bg = PALETTE[p - 92];
      else if (p === 49) delete st.bg;
      else if (p === 38 || p === 48) {
        const k = p === 38 ? 'fg' : 'bg';
        if (ps[i + 1] === 5) { st[k] = color256(ps[i + 2] | 0); i += 2; }
        else if (ps[i + 1] === 2) { st[k] = rgb(ps[i + 2], ps[i + 3], ps[i + 4]); i += 4; }
      }
    }
  }
  text += line.slice(last);
  push();
  return out;
}

// ---- chat markdown: escape first, then add structure; links only to http(s) ----

function mdInline(src) {
  const codes = [];
  const s = esc(src).replace(/`([^`\n]+)`/g, (_, c) => { codes.push(c); return `\u0000${codes.length - 1}\u0000`; })
    .replace(/\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>')
    .replace(/\*\*([^*\n]+)\*\*/g, '<b>$1</b>')
    .replace(/(^|[^\w*])\*([^*\n]+)\*(?![\w*])/g, '$1<i>$2</i>');
  return s.replace(/\u0000(\d+)\u0000/g, (_, i) => `<code>${codes[+i]}</code>`);
}

// ---- code: a small one-pass highlighter, good enough for chat snippets in any C-like or shell language ----
const KW = new Set(('async await break case catch class const continue def default defer del do done elif else enum esac except export ' +
  'extends false fi finally fn for from func function go if impl import in interface lambda let local match mut new nil None not null ' +
  'or package pass pub raise range readonly return select self struct switch then this throw true True False try type typeof use var ' +
  'while with yield echo cd exit sudo').split(' '));
const HASH_LANGS = /^(sh|bash|zsh|shell|console|py|python|rb|ruby|ya?ml|toml|make|makefile|dockerfile|conf|ini|r|perl|nix)$/i;

// hl returns one HTML string per line; a token that spans lines (a block comment) is closed and reopened on each.
function hl(code, lang = '') {
  const hash = HASH_LANGS.test(lang);
  const re = new RegExp(String.raw`(\/\/[^\n]*|\/\*[\s\S]*?\*\/${hash ? String.raw`|#[^\n]*` : ''})` +
    String.raw`|("(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'|` + '`(?:[^`\\\\]|\\\\.)*`)' +
    String.raw`|\b(\d[\w.]*)|([A-Za-z_$][\w$]*)(?=\s*\()|([A-Za-z_][\w-]*)`, 'g');
  const toks = [];
  let last = 0;
  let m;
  while ((m = re.exec(code))) {
    if (m[0][0] === '#' && m.index > 0 && !/\s/.test(code[m.index - 1])) { re.lastIndex = m.index + 1; continue; } // a#b is not a comment
    if (m.index > last) toks.push(['', code.slice(last, m.index)]);
    const cls = m[1] ? 'c' : m[2] ? 's' : m[3] ? 'n' : KW.has(m[4] || m[5]) ? 'k' : m[4] ? 'f' : '';
    toks.push([cls, m[0]]);
    last = re.lastIndex;
  }
  if (last < code.length) toks.push(['', code.slice(last)]);
  const lines = [''];
  for (const [cls, text] of toks) {
    text.split('\n').forEach((part, i) => {
      if (i) lines.push('');
      if (part) lines[lines.length - 1] += cls ? `<span class="tk-${cls}">${esc(part)}</span>` : esc(part);
    });
  }
  return lines;
}

// An icon from icons.js when it's loaded (the browser); nothing under node --test.
const icon = (n) => (typeof ic === 'function' ? ic(n) : '');

const LIST_RE = /^\s*(?:[-*•]|(\d+)[.)])\s+(.*)$/;
const TABLE_SEP = /^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)+\|?\s*$/;
const BOX = /[─━│┃┌┐└┘├┤┬┴┼═║╔╗╚╝╠╣╦╩╬]/; // box drawing: ASCII diagrams must keep their columns
const cells = (row) => row.trim().replace(/^\|/, '').replace(/\|$/, '').split('|').map((c) => c.trim());

function md(src) {
  const lines = String(src ?? '').replace(/\r\n?/g, '\n').split('\n');
  let html = '';
  let para = [];
  const flush = () => { if (para.length) html += `<p>${para.map(mdInline).join('<br>')}</p>`; para = []; };
  let i = 0;
  while (i < lines.length) {
    const l = lines[i];
    let m;
    if ((m = l.match(/^\s*```\s*([\w+#.-]*)/))) {
      flush();
      const code = [];
      for (i++; i < lines.length && !/^\s*```/.test(lines[i]); i++) code.push(lines[i]);
      i++;
      const diagram = !m[1] && BOX.test(code.join(''));
      const label = m[1] || (diagram ? 'diagram' : 'code');
      const body = diagram ? esc(code.join('\n')) : hl(code.join('\n'), m[1]).map((l) => `<span class="cl">${l}</span>`).join('\n');
      html += `<div class="cb${diagram ? ' plain' : ''}"><div class="cb-h">${icon(diagram ? 'hierarchy' : 'document-code')}<span class="cb-l">${esc(label)}</span>` +
        `<button type="button" class="cb-copy">${icon('copy')}Copy</button></div><pre><code>${body}</code></pre></div>`;
    } else if (!l.trim()) {
      flush();
      i++;
    } else if ((m = l.match(/^(#{1,6})\s+(.*)$/))) {
      flush();
      html += `<div class="mh mh${Math.min(m[1].length, 3)}">${mdInline(m[2])}</div>`;
      i++;
    } else if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(l)) {
      flush();
      html += '<hr>';
      i++;
    } else if (/^\s*>/.test(l)) {
      flush();
      const q = [];
      while (i < lines.length && /^\s*>/.test(lines[i])) q.push(lines[i++].replace(/^\s*>\s?/, ''));
      html += `<blockquote>${q.map(mdInline).join('<br>')}</blockquote>`;
    } else if (l.includes('|') && TABLE_SEP.test(lines[i + 1] || '')) {
      flush();
      const head = cells(l);
      const body = [];
      for (i += 2; i < lines.length && lines[i].includes('|') && lines[i].trim(); i++) body.push(cells(lines[i]));
      html += `<table><thead><tr>${head.map((c) => `<th>${mdInline(c)}</th>`).join('')}</tr></thead><tbody>` +
        body.map((r) => `<tr>${r.map((c) => `<td>${mdInline(c)}</td>`).join('')}</tr>`).join('') + '</tbody></table>';
    } else if ((m = l.match(LIST_RE))) {
      flush();
      const ordered = !!m[1];
      const first = +m[1] || 1; // "2." after a few bullets continues the numbering
      const items = [];
      let mm;
      while (i < lines.length && (mm = lines[i].match(LIST_RE)) && !!mm[1] === ordered) { items.push(mm[2]); i++; }
      const tag = ordered ? 'ol' : 'ul';
      html += `<${tag}${ordered && first > 1 ? ` start="${first}"` : ''}>${items.map((x) => `<li>${mdInline(x)}</li>`).join('')}</${tag}>`;
    } else if (BOX.test(l)) {
      flush();
      const d = [];
      while (i < lines.length && BOX.test(lines[i])) d.push(lines[i++]);
      html += `<pre class="diagram">${esc(d.join('\n'))}</pre>`;
    } else {
      para.push(l);
      i++;
    }
  }
  flush();
  return html;
}

// ---- terminal keyboard: what an xterm sends for a key (printable keys arrive via the input event) ----

const KEYS = {
  Enter: '\r', Backspace: '\x7f', Tab: '\t', Escape: '\x1b',
  ArrowUp: '\x1b[A', ArrowDown: '\x1b[B', ArrowRight: '\x1b[C', ArrowLeft: '\x1b[D',
  Home: '\x1b[H', End: '\x1b[F', PageUp: '\x1b[5~', PageDown: '\x1b[6~', Delete: '\x1b[3~', Insert: '\x1b[2~',
  F1: '\x1bOP', F2: '\x1bOQ', F3: '\x1bOR', F4: '\x1bOS', F5: '\x1b[15~', F6: '\x1b[17~',
  F7: '\x1b[18~', F8: '\x1b[19~', F9: '\x1b[20~', F10: '\x1b[21~', F11: '\x1b[23~', F12: '\x1b[24~',
};

function ctrlChar(k) {
  if (k === ' ' || k === '@' || k === '2') return '\x00';
  if (k === '/') return '\x1f';
  if (k === '?') return '\x7f';
  const c = k.toUpperCase().charCodeAt(0);
  return k.length === 1 && c >= 64 && c <= 95 ? String.fromCharCode(c - 64) : null;
}

function keyBytes(e) {
  if (e.metaKey) return null;
  if (e.key === 'Tab' && e.shiftKey) return '\x1b[Z';
  if (KEYS[e.key] !== undefined) return (e.altKey ? '\x1b' : '') + KEYS[e.key];
  if (e.key.length !== 1) return null;
  if (e.ctrlKey && !e.altKey) return ctrlChar(e.key);
  if (e.altKey && !e.ctrlKey) return '\x1b' + e.key;
  return null;
}

function b64utf8(s) {
  let bin = '';
  for (const b of new TextEncoder().encode(s)) bin += String.fromCharCode(b);
  return btoa(bin);
}

// scrolledOff says how many lines left the top of a terminal screen between two frames (0: it didn't
// scroll): the smallest shift after which the old screen reads like the start of the new one, for at
// least half of what's left and three lines with text (blank lines alone prove nothing).
function scrolledOff(prev, next) {
  const n = Math.min(prev.length, next.length);
  const plain = (l) => l.replace(/\x1b\[[0-9;:?]*[ -/]*[@-~]/g, '').trim();
  for (let s = 1; s <= n - 3; s++) {
    let k = 0;
    let text = 0;
    while (s + k < n && prev[s + k] === next[k]) { if (plain(next[k])) text++; k++; }
    if (text >= 3 && k >= (n - s) / 2) return s;
  }
  return 0;
}

// termLineKind sorts a terminal line (plain text) for a reflowed view: a rule (a line drawn across)
// and a grid row (a box-drawing table) keep their shape; text may wrap to the view's width.
function termLineKind(plain) {
  const t = String(plain).trim();
  if ((t.match(/[│┃├┤┼┬┴┌┐└┘╭╮╰╯]/g) || []).length >= 2) return 'grid';
  if (t.length >= 12 && (t.match(/[─━═▔▁_-]/g) || []).length >= t.length * 0.5) return 'rule';
  return 'text';
}

// splitTurns cuts a chat into turns at each user message (indexes into items). A turn's final answer is its
// last text with no work after it; the work before it (thinking, tools, narration) can fold away under one
// header, as in Codex. Notes stay visible; "Worked for …" becomes the header's timing.
function splitTurns(items) {
  const turns = [];
  let t = null, ended = false;
  items.forEach((it, i) => {
    if (it.k === 'queue' || it.k === 'model') return; // see pendingQueue; model: what the agent runs on
    // a turn ends with its "Worked for": what follows is the next turn, even one that no message of yours began
    // (else its reply would turn the last turn's answer into narration, folded away)
    if (!t || it.k === 'user' || ended) { turns.push((t = { user: it.k === 'user' ? i : -1, work: [], final: -1, notes: [], worked: -1 })); ended = false; }
    if (it.k === 'user') return;
    if (it.k === 'note') {
      if (/^Worked for /.test(it.text) && t.worked < 0) { t.worked = i; ended = true; }
      else t.notes.push(i);
      return;
    }
    if (t.final >= 0) t.work.push(t.final); // more work after a text: that text was narration
    t.final = -1;
    if (it.k === 'text') t.final = i;
    else t.work.push(i);
  });
  return turns;
}

// groupNotes folds a turn's notes for the chat: a run of one monitor's events, with the "Worked for" notes
// of the little turns they woke in between, becomes one entry ({ events, worked }); everything else, a
// single event included, stays a line ({ i }). A chatty monitor then takes one row, not two per event.
function groupNotes(items, idxs) {
  const out = [];
  let g = null;
  const close = () => { if (g) { if (g.events.length > 1) out.push(g); else for (const i of g.all) out.push({ i }); g = null; } };
  for (const i of idxs) {
    const it = items[i];
    if (it.name === 'task:event') {
      if (g && g.text !== it.text) close();
      if (!g) g = { text: it.text, events: [], worked: [], all: [] };
      g.events.push(i);
      g.all.push(i);
    } else if (g && /^Worked for /.test(it.text)) {
      g.worked.push(i);
      g.all.push(i);
    } else {
      close();
      out.push({ i });
    }
  }
  close();
  return out.map((x) => (x.events ? { text: x.text, events: x.events, worked: x.worked } : x));
}

// pendingQueue replays Claude Code's message queue: the messages you typed that still wait for the agent.
// Background-task notifications queue too, but they aren't yours. A dequeue doesn't log what it took, and
// Claude Code takes the best priority first (your messages before notifications), not the oldest; a message
// that then shows up in the chat has surely left the queue. A remove can log its text a little differently
// from the enqueue, so it matches on the start of the text.
function pendingQueue(items) {
  const q = [];
  const key = (t) => String(t || '').trim().slice(0, 120);
  const sys = (t) => /^\s*<[a-z-]+[\s>]/.test(t); // <task-notification>, <agent-message from=…>
  const drop = (t) => { const i = q.findIndex((x) => key(x) === key(t)); if (i >= 0) q.splice(i, 1); };
  for (const it of items) {
    if (it.k === 'user') drop(it.text);
    if (it.k !== 'queue') continue;
    if (it.name === 'enqueue') q.push(it.text);
    else if (it.name === 'dequeue') q.splice(Math.max(0, q.findIndex((t) => !sys(t))), 1);
    else if (it.name === 'remove' || it.name === 'popAll') drop(it.text); // popAll: ↑ took it back out (to edit, or to cancel it)
  }
  return q.filter((t) => !sys(t));
}

// ---- the effort slider's shape (the owner's ReasoningEffort design): a pill whose top edge grows a
// shoulder carrying the level's name while you drag; at either end the shoulder walks down the cap ----
const EFF = { TRACK: 224, HEIGHT: 32, RADIUS: 16, BUMP_H: 20, REST_W: 27.2, DRAG_W: 33.6, REST_H: 27.2, SHOULDER: 23.572, TEXT_SIDE: 20, PHI_MAX: 0.6, PHI_REACH: 36 };
EFF.EDGE_PAD = (EFF.HEIGHT - EFF.REST_H) / 2;
const clampN = (n, lo, hi) => Math.min(hi, Math.max(lo, n));
const easeOut3 = (t) => 1 - (1 - t) ** 3;
const smoothstep = (t) => { const x = clampN(t, 0, 1); return x * x * (3 - 2 * x); };
const effCenter = (value, stops) => EFF.RADIUS + (value / (stops - 1)) * (EFF.TRACK - EFF.RADIUS * 2);
const effPlateau = (textW) => Math.max(8, textW + EFF.TEXT_SIDE * 2 - EFF.SHOULDER * 2);
function effCapFoot(side, phi) {
  const crown = side === -1 ? EFF.RADIUS : EFF.TRACK - EFF.RADIUS;
  return { x: crown + side * EFF.RADIUS * Math.sin(phi), y: EFF.BUMP_H + EFF.RADIUS * (1 - Math.cos(phi)) };
}
function effShellGeom(cx, plateauW) {
  const { RADIUS, TRACK, SHOULDER, PHI_MAX, PHI_REACH } = EFF;
  let topL = cx - plateauW / 2;
  let topR = topL + plateauW;
  let phiL = 0;
  let phiR = 0;
  const overflowL = RADIUS - (topL - SHOULDER);
  if (overflowL > 0) {
    phiL = PHI_MAX * smoothstep(overflowL / PHI_REACH);
    const minTopL = effCapFoot(-1, phiL).x + SHOULDER * Math.cos(phiL);
    if (topL < minTopL) { topR += minTopL - topL; topL = minTopL; }
  }
  const overflowR = topR + SHOULDER - (TRACK - RADIUS);
  if (overflowR > 0) {
    phiR = PHI_MAX * smoothstep(overflowR / PHI_REACH);
    const maxTopR = effCapFoot(1, phiR).x - SHOULDER * Math.cos(phiR);
    if (topR > maxTopR) { topL -= topR - maxTopR; topR = maxTopR; }
  }
  // the tilted shoulder is shorter in x: grow the plateau inward by that, and the label with it
  const insetL = phiL > 0 ? SHOULDER * (1 - Math.cos(phiL)) : 0;
  const insetR = phiR > 0 ? SHOULDER * (1 - Math.cos(phiR)) : 0;
  return { topL: topL - insetR, topR: topR + insetL, phiL, phiR, insetL, insetR };
}
function effShoulder(footX, footY, platX, platY, phi, dir) {
  const xs = Math.abs(platX - footX) / EFF.SHOULDER;
  const ys = Math.max(0.001, (footY - platY) / 20);
  const map = (lx, ly) => ({ x: footX + dir * lx * xs, y: footY + (ly - 20) * ys });
  const h1 = 5.398 * xs;
  const p0 = { x: footX, y: footY };
  const c1 = { x: footX + dir * Math.cos(phi) * h1, y: footY - Math.sin(phi) * h1 };
  const c2 = map(10.012, 16.114), p1 = map(10.93, 10.794), c3 = map(11.916, 4.577), c4 = map(17.277, 0);
  const p3 = { x: platX, y: platY };
  const f = (p) => `${p.x.toFixed(2)} ${p.y.toFixed(2)}`;
  return dir === 1 ? `C${f(c1)} ${f(c2)} ${f(p1)}C${f(c3)} ${f(c4)} ${f(p3)}` : `C${f(c4)} ${f(c3)} ${f(p1)}C${f(c2)} ${f(c1)} ${f(p0)}`;
}
// one outline: the pill, with the label's shoulder grown out of its top edge (open 0..1)
function effShellPath(cx, open, plateauW) {
  const { RADIUS: R, TRACK, BUMP_H, HEIGHT, SHOULDER } = EFF;
  const top = BUMP_H, bot = BUMP_H + HEIGHT, cl = R, cr = TRACK - R;
  if (open < 0.012) return `M${cl} ${top}H${cr}A${R} ${R} 0 0 1 ${cr} ${bot}H${cl}A${R} ${R} 0 0 1 ${cl} ${top}Z`;
  const bumpTop = BUMP_H * (1 - open);
  const { topL, topR, phiL, phiR } = effShellGeom(cx, plateauW);
  const c = [`M${cl} ${bot}`];
  if (phiL > 0.001) {
    const foot = effCapFoot(-1, phiL);
    c.push(`A${R} ${R} 0 0 1 ${foot.x.toFixed(2)} ${foot.y.toFixed(2)}`, effShoulder(foot.x, foot.y, topL, bumpTop, phiL, 1));
  } else {
    c.push(`A${R} ${R} 0 0 1 ${cl} ${top}`);
    const footL = topL - SHOULDER;
    if (footL > cl + 0.4) c.push(`H${footL.toFixed(2)}`);
    c.push(effShoulder(footL, top, topL, bumpTop, 0, 1));
  }
  if (topR > topL + 0.4) c.push(`H${topR.toFixed(2)}`);
  if (phiR > 0.001) {
    const foot = effCapFoot(1, phiR);
    c.push(effShoulder(foot.x, foot.y, topR, bumpTop, phiR, -1), `A${R} ${R} 0 0 1 ${cr} ${bot}`);
  } else {
    const footR = topR + SHOULDER;
    c.push(effShoulder(footR, top, topR, bumpTop, 0, -1));
    if (footR < cr - 0.4) c.push(`H${cr}`);
    c.push(`A${R} ${R} 0 0 1 ${cr} ${bot}`);
  }
  c.push(`H${cl}Z`);
  return c.join('');
}

// stillPending: the messages you sent that the chat doesn't show yet. Claude records one when it takes it
// (or queues it while busy), which can lag the paste by seconds; until then the page shows it as sent.
// Matched on the words (Claude Code may wrap a paste in its marks), from a few seconds before the send.
const sentKey = (t) => String(t || '').replace(/<\/?pasted_content[^>]*>/g, '').replace(/\s+/g, ' ').trim().slice(0, 100);
// A message counts as taken when Claude's next message (or queue entry) is it, or ends with it: stopped mid-
// message, Claude Code puts that message back in its box, and the next one pasted there goes out with it as one.
const flat = (t) => String(t || '').replace(/<\/?pasted_content[^>]*>/g, '').replace(/\s+/g, ' ').trim();
// Photos go by path and come back as [Image #N] with their paths after the text: matched on the words without
// them, and a message of photos alone on its first photo.
function stillPending(sent, items) {
  return sent.filter((s) => {
    const a = splitUploads(s.text);
    return !a.text.startsWith('/') && !items.some((it) => {
      const b = splitUploads(it.text);
      return (it.k === 'user' || (it.k === 'queue' && it.name === 'enqueue')) && (it.t || 0) >= s.at - 5 &&
        (a.text ? sentKey(b.text) === sentKey(a.text) || (it.k === 'user' && flat(a.text).length >= 8 && flat(b.text).endsWith(flat(a.text))) : b.names.includes(a.names[0]));
    });
  });
}

// fitScreen crops a frame to the pane's size (rows x cols), as a terminal shows it: zellij keeps rows and
// columns past the pane that a full-screen app drew while the pane was bigger and never cleared.
// ponytail: one column per character, so a line of wide (CJK, emoji) characters may run a little long.
function fitScreen(lines, rows, cols) {
  if (!rows || !cols) return lines;
  const esc = /\x1b\[[0-9;:?]*[ -/]*[@-~]/y;
  return lines.slice(0, rows).map((l) => {
    let n = 0;
    for (let i = 0; i < l.length;) {
      esc.lastIndex = i;
      if (l[i] === '\x1b' && esc.test(l)) { i = esc.lastIndex; continue; }
      if (n === cols) return l.slice(0, i) + '\x1b[m';
      i += l.codePointAt(i) > 0xffff ? 2 : 1;
      n++;
    }
    return l;
  });
}

// mainDir shows a worktree (under .worktrees, or Claude Code's .claude/worktrees) as the repo it belongs to.
const mainDir = (p) => String(p || '').replace(/\/\.(?:claude\/)?worktrees\/.*$/, '');

// splitUploads pulls chat image paths (/…/uploads/<stamp>-<hex>.<ext>, as the panel names them) out of a
// sent message, so the bubble shows the pictures instead of the paths: wherever they sit (queued messages arrive
// merged, a path glued to the text before it), once each, with Claude Code's own marks for the images it attached
// ([Image #4]) gone too. Only where something went do the spaces close up (a pasted block keeps its indent).
function splitUploads(text) {
  const names = [];
  const rest = String(text ?? '')
    .replace(/\/\S*?\/uploads\/(\d{8}-\d{6}-[0-9a-f]{16}(?:\.(?:png|jpe?g|gif|webp)|-[A-Za-z0-9._-]{1,80}))(?![A-Za-z0-9._-])/g, (_, name) => { if (!names.includes(name)) names.push(name); return '\0'; })
    .replace(/\[Image #\d+\]/g, '\0')
    .replace(/[ \t]*(?:\0[ \t]*)+/g, ' ')
    .replace(/^ $/gm, '').replace(/\n{3,}/g, '\n\n');
  return { text: rest.trim(), names };
}

// sessionSlug: a zellij session name from what was typed (the server takes letters, digits, . _ - only, starting
// with a letter or digit): accents dropped, anything else a dash, so "Baker Capital" is Baker-Capital, not an error.
const sessionSlug = (n) => String(n || '').normalize('NFKD').replace(/[\u0300-\u036f]/g, '')
  .replace(/[^A-Za-z0-9._-]+/g, '-').replace(/-{2,}/g, '-').replace(/^[^A-Za-z0-9]+/, '').replace(/-+$/, '').slice(0, 64);

// ddMatch: the option a dropdown's type-ahead lands on. One letter looks past the current option (typing it again
// cycles through those that start with it); a longer run of keys stays on the current one while it still fits.
function ddMatch(labels, q, cur) {
  q = q.toLowerCase();
  const n = labels.length, from = q.length === 1 ? cur + 1 : cur;
  for (let k = 0; k < n; k++) {
    const i = (((from + k) % n) + n) % n;
    if (labels[i].toLowerCase().startsWith(q)) return i;
  }
  return -1;
}

// ---------- the Terminals page: what each pane is, its state in one word, what a stopped agent needs ----------
// termKind: an agent (Claude, Codex), a service (a command running in the pane), or a shell at its prompt.
const termKind = (p) => (p.agent || p.kind === 'claude' || p.kind === 'codex' ? 'agent' : p.kind === 'cmd' || p.fg || p.state === 'running' ? 'service' : 'shell');
const BLOCK_RE = /\b(can'?t|cannot|won'?t|unable to)\b[^.]*\buntil you\b|\bblocked\b|\bneeds? you to\b|\brequires? you\b|\bonly you can\b/i;
const ASK_RE = /\?|\b(should I|want me to|shall I|let me know|your call|do you want|which one|approve|confirm)\b/i;
// askOf: what an agent that stopped needs from you, in its own words: the clause of its last reply that blocks on
// you, else the one that asks, else its first sentence (what it finished). A dialog on screen asks by itself.
// ponytail: sentence patterns; a small model could write the line when these miss.
function askOf(last, promptQ) {
  if (promptQ) return { k: 'input', text: promptQ };
  const flat = String(last || '').replace(/<\/?pasted_content[^>]*>/g, '').replace(/\[Image #\d+\]/g, '(photo)').replace(/\*\*|`|^#+ /gm, '').replace(/\s+/g, ' ').trim();
  const ss = flat.split(/(?<=[.?!])\s+/).map((x) => x.trim()).filter(Boolean);
  const clause = (x, re) => x.split(/(?<=[:;])\s+|\s+—\s+/).find((c) => re.test(c)) || x;
  const block = ss.find((x) => BLOCK_RE.test(x));
  if (block) return { k: 'blocked', text: clause(block, BLOCK_RE) };
  const q = [...ss].reverse().find((x) => /\?$/.test(x)) || ss.find((x) => ASK_RE.test(x));
  if (q) return { k: 'input', text: clause(q, ASK_RE) };
  return { k: 'done', text: ss[0] || '' };
}
// termState: the orb a pane wears. Agents: working, input (a quick answer or a permission), blocked (can't go on
// without you), done. Services: running, slow, stopped, error (errors in its newest output). Shells: idle.
function termState(p) {
  const k = termKind(p);
  if (k === 'agent') {
    if (['working', 'background', 'starting', 'stalled', 'stuck'].includes(p.state)) return 'working';
    if (p.state === 'asking') return 'input';
    if (p.state === 'stopped' || p.state === 'exited') return 'stopped';
    return askOf(p.agent?.last).k;
  }
  if (k === 'service') {
    if (p.state === 'stopped' || p.state === 'exited') return 'stopped';
    if (p.errNow) return 'error';
    return p.state === 'stuck' || (p.cpu || 0) >= 90 ? 'slow' : 'running';
  }
  return 'idle';
}
// termPurpose: a session's type, from what runs in it: servers make it Infra, agents in ~/repos Product, agents in
// ~/projects Tooling. [label, why] — the why is the tag's tooltip.
function termPurpose(panes) {
  const ag = panes.filter((p) => termKind(p) === 'agent'), sv = panes.filter((p) => termKind(p) === 'service');
  if (sv.length && sv.length >= ag.length) return ['Infra', `Set automatically: ${sv.length} server${sv.length === 1 ? '' : 's'} run in it`];
  const roots = ag.map((p) => p.git?.root || p.agent?.cwd || '');
  if (roots.some((r) => /(^|\/)repos\//.test(r) || r.startsWith('~/repos'))) return ['Product', 'Set automatically: its agents work in product repos (~/repos)'];
  if (ag.length) return ['Tooling', 'Set automatically: its agents work on tools (~/projects)'];
  return ['', ''];
}

if (typeof module === 'object') module.exports = { keepOrder, ansiSegments, md, mdInline, esc, PALETTE, keyBytes, ctrlChar, b64utf8, splitUploads, hl, splitTurns, scrolledOff, pendingQueue, termLineKind, mainDir, EFF, effCenter, effPlateau, effShellGeom, effShellPath, clampN, easeOut3, stillPending, fitScreen, groupNotes, ddMatch, sessionSlug, termKind, askOf, termState, termPurpose };
