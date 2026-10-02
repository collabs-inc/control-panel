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
  let t = null;
  items.forEach((it, i) => {
    if (it.k === 'queue' || it.k === 'model') return; // see pendingQueue; model: what the agent runs on
    if (!t || it.k === 'user') turns.push((t = { user: it.k === 'user' ? i : -1, work: [], final: -1, notes: [], worked: -1 }));
    if (it.k === 'user') return;
    if (it.k === 'note') {
      if (/^Worked for /.test(it.text) && t.worked < 0) t.worked = i;
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

// pendingQueue replays Claude Code's message queue: the messages you typed that still wait for the agent.
// Background-task notifications queue too, but they aren't yours; and a remove can log its text a little
// differently from the enqueue, so it matches on the start of the text.
function pendingQueue(items) {
  const q = [];
  const key = (t) => String(t || '').trim().slice(0, 120);
  for (const it of items) {
    if (it.k !== 'queue') continue;
    if (it.name === 'enqueue') q.push(it.text);
    else if (it.name === 'dequeue') q.shift();
    else if (it.name === 'remove') { const i = q.findIndex((x) => key(x) === key(it.text)); if (i >= 0) q.splice(i, 1); }
  }
  return q.filter((t) => !/^\s*<[a-z-]+>/.test(t));
}

// splitUploads pulls chat image paths (/…/uploads/<stamp>-<hex>.<ext>, as the panel names them) out of a
// sent message, so the bubble shows the pictures instead of the paths.
function splitUploads(text) {
  const names = [];
  const rest = String(text ?? '').replace(/(^|\s)\/\S*\/uploads\/(\d{8}-\d{6}-[0-9a-f]{16}(?:\.(?:png|jpg|gif|webp)|-[A-Za-z0-9._-]{1,80}))(?=\s|$)/g, (_, pre, name) => { names.push(name); return pre; });
  return { text: rest.trim(), names };
}

if (typeof module === 'object') module.exports = { keepOrder, ansiSegments, md, mdInline, esc, PALETTE, keyBytes, ctrlChar, b64utf8, splitUploads, hl, splitTurns, scrolledOff, pendingQueue, termLineKind };
