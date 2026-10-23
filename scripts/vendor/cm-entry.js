// The Files editor: CodeMirror 6 with the panel's colours, a minimap, and a language per file extension.
// Bundled once into web/cm.min.js (see build.sh); files.js loads it the first time a text file opens.
import { EditorView, ViewPlugin, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter, drawSelection, highlightSpecialChars, dropCursor, rectangularSelection, crosshairCursor } from '@codemirror/view';
import { EditorState, Compartment } from '@codemirror/state';
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands';
import { syntaxHighlighting, syntaxTree, HighlightStyle, indentOnInput, bracketMatching, foldGutter, foldKeymap, StreamLanguage } from '@codemirror/language';
import { searchKeymap, highlightSelectionMatches, search } from '@codemirror/search';
import { closeBrackets, closeBracketsKeymap } from '@codemirror/autocomplete';
import { tags as t, tagHighlighter, highlightTree } from '@lezer/highlight';
import { python } from '@codemirror/lang-python';
import { javascript } from '@codemirror/lang-javascript';
import { json } from '@codemirror/lang-json';
import { markdown } from '@codemirror/lang-markdown';
import { html } from '@codemirror/lang-html';
import { css } from '@codemirror/lang-css';
import { go } from '@codemirror/lang-go';
import { yaml } from '@codemirror/lang-yaml';
import { sql } from '@codemirror/lang-sql';
import { rust } from '@codemirror/lang-rust';
import { xml } from '@codemirror/lang-xml';
import { shell } from '@codemirror/legacy-modes/mode/shell';
import { toml } from '@codemirror/legacy-modes/mode/toml';
import { dockerFile } from '@codemirror/legacy-modes/mode/dockerfile';
import { properties } from '@codemirror/legacy-modes/mode/properties';
import { diff } from '@codemirror/legacy-modes/mode/diff';
import { lua } from '@codemirror/legacy-modes/mode/lua';
import { ruby } from '@codemirror/legacy-modes/mode/ruby';

const LANGS = {
  py: python, pyw: python, ipynb: json, js: javascript, mjs: javascript, cjs: javascript,
  jsx: () => javascript({ jsx: true }), ts: () => javascript({ typescript: true }), tsx: () => javascript({ jsx: true, typescript: true }),
  json: json, jsonl: json, md: markdown, markdown: markdown, html: html, htm: html, vue: html, svelte: html,
  css: css, scss: css, less: css, go: go, yaml: yaml, yml: yaml, sql: sql, rs: rust, xml: xml, svg: xml, plist: xml,
  sh: () => StreamLanguage.define(shell), bash: () => StreamLanguage.define(shell), zsh: () => StreamLanguage.define(shell),
  toml: () => StreamLanguage.define(toml), env: () => StreamLanguage.define(properties), ini: () => StreamLanguage.define(properties),
  cfg: () => StreamLanguage.define(properties), conf: () => StreamLanguage.define(properties), properties: () => StreamLanguage.define(properties),
  diff: () => StreamLanguage.define(diff), patch: () => StreamLanguage.define(diff), lua: () => StreamLanguage.define(lua), rb: () => StreamLanguage.define(ruby),
};
const NAMES = { dockerfile: () => StreamLanguage.define(dockerFile), makefile: () => StreamLanguage.define(shell), '.bashrc': () => StreamLanguage.define(shell), '.zshrc': () => StreamLanguage.define(shell), '.profile': () => StreamLanguage.define(shell) };
const langFor = (name) => {
  const n = name.toLowerCase(), x = n.includes('.') ? n.slice(n.lastIndexOf('.') + 1) : '';
  const f = NAMES[n] || (n.startsWith('.env') ? LANGS.env : LANGS[x]);
  return f ? f() : [];
};

// the session viewer's hl() colours
const style = HighlightStyle.define([
  { tag: [t.keyword, t.operatorKeyword, t.modifier, t.controlKeyword, t.definitionKeyword, t.moduleKeyword], color: '#c792ea' },
  { tag: [t.string, t.special(t.string), t.regexp, t.inserted], color: '#a5d6a7' },
  { tag: [t.comment, t.lineComment, t.blockComment, t.meta], color: '#6b7380', fontStyle: 'italic' },
  { tag: [t.number, t.bool, t.null, t.atom, t.unit], color: '#f5a97f' },
  { tag: [t.function(t.variableName), t.function(t.propertyName), t.definition(t.function(t.variableName)), t.className, t.typeName, t.tagName], color: '#82b1ff' },
  { tag: [t.propertyName, t.attributeName], color: '#9ecbff' },
  { tag: [t.heading], color: '#eef0f3', fontWeight: '700' },
  { tag: [t.strong], fontWeight: '700' }, { tag: [t.emphasis], fontStyle: 'italic' },
  { tag: [t.link, t.url], color: '#82b1ff', textDecoration: 'underline' },
  { tag: [t.deleted, t.invalid], color: '#ff9b94' },
]);
const theme = EditorView.theme({
  '&': { height: '100%', color: '#d7dae0', backgroundColor: '#0c0f13', fontSize: '13px' },
  '.cm-content': { caretColor: 'var(--au-green)', padding: '12px 0' },
  '.cm-cursor': { borderLeftColor: 'var(--au-green)', borderLeftWidth: '2px' },
  '.cm-gutters': { backgroundColor: '#0a0d10', color: '#4b525d', border: 'none', borderRight: '1px solid #161a20' },
  '.cm-activeLineGutter': { backgroundColor: 'transparent', color: 'var(--muted)' },
  '.cm-activeLine': { backgroundColor: 'rgba(255,255,255,.04)' },
  '&.cm-focused .cm-selectionBackground, .cm-selectionBackground, ::selection': { backgroundColor: 'rgba(92,157,255,.28) !important' },
  '.cm-selectionMatch': { backgroundColor: 'rgba(92,157,255,.14)' },
  '.cm-searchMatch': { backgroundColor: 'rgba(227,179,65,.22)' },
  '.cm-searchMatch.cm-searchMatch-selected': { backgroundColor: 'rgba(227,179,65,.5)' },
  '.cm-matchingBracket': { backgroundColor: 'rgba(63,233,174,.18)', outline: 'none' },
  '.cm-panels': { backgroundColor: 'var(--surface-2)', color: 'var(--text)', border: 'none' },
  '.cm-panels-top': { borderBottom: '1px solid var(--border)' },
  '.cm-panel.cm-search': { padding: '8px 10px', fontFamily: 'inherit' },
  '.cm-panel.cm-search input, .cm-panel.cm-search button': { font: 'inherit', fontSize: '12.5px' },
  '.cm-textfield': { backgroundColor: 'var(--bg)', border: '1px solid var(--border)', borderRadius: '7px', color: 'var(--text)', padding: '3px 8px' },
  '.cm-button': { backgroundImage: 'none', backgroundColor: 'var(--surface-3)', border: '1px solid var(--border)', borderRadius: '7px', color: 'var(--text)' },
  '.cm-foldGutter span': { color: '#4b525d' },
  '.cm-scroller': { fontFamily: 'var(--mono)', lineHeight: '1.65', marginRight: MINI_W + 'px' },
  '.cm-mini': { position: 'absolute', top: 0, right: 0, width: MINI_W + 'px', backgroundColor: '#0b0e12', borderLeft: '1px solid #161a20', cursor: 'pointer' },
}, { dark: true });

// The minimap: the file in miniature on a canvas, 2 px a line, with a box over what's on screen. Only the lines it
// shows are drawn (a window that slides with the scroll when the file is taller than the map, as in VS Code), so a
// keystroke costs the same in a 100-line file and a 120,000-line log. Click or drag it to scroll.
const MINI_W = 96, MINI_LH = 2, MINI_CW = 1.1;
const miniColor = tagHighlighter([ // the same colours as the text, as "classes"
  { tag: [t.keyword, t.operatorKeyword, t.modifier, t.controlKeyword, t.definitionKeyword, t.moduleKeyword], class: '#c792ea' },
  { tag: [t.string, t.special(t.string), t.regexp], class: '#a5d6a7' },
  { tag: [t.comment, t.lineComment, t.blockComment], class: '#4f5864' },
  { tag: [t.number, t.bool, t.null, t.atom], class: '#f5a97f' },
  { tag: [t.function(t.variableName), t.function(t.propertyName), t.className, t.typeName, t.tagName, t.heading], class: '#82b1ff' },
]);
const minimap = ViewPlugin.fromClass(class {
  constructor(view) {
    this.view = view;
    this.c = document.createElement('canvas');
    this.c.className = 'cm-mini';
    this.c.title = 'Click or drag to scroll';
    view.dom.append(this.c);
    this.queue = () => { this.raf ||= requestAnimationFrame(() => { this.raf = 0; this.draw(); }); };
    view.scrollDOM.addEventListener('scroll', this.queue, { passive: true });
    const go = (e) => { // the line under the pointer, to the middle of the screen
      const r = this.c.getBoundingClientRect(), n = Math.max(1, Math.min(view.state.doc.lines, Math.round(this.first + (e.clientY - r.top) / MINI_LH)));
      const b = view.lineBlockAt(view.state.doc.line(n).from);
      view.scrollDOM.scrollTop = b.top - view.scrollDOM.clientHeight / 2;
    };
    this.c.addEventListener('pointerdown', (e) => {
      e.preventDefault();
      this.c.setPointerCapture(e.pointerId);
      go(e);
      const mv = (ev) => go(ev);
      this.c.addEventListener('pointermove', mv);
      this.c.addEventListener('pointerup', () => this.c.removeEventListener('pointermove', mv), { once: true });
    });
    this.queue();
  }
  update(u) { if (u.docChanged || u.viewportChanged || u.geometryChanged || u.selectionSet) this.queue(); }
  draw() {
    const v = this.view, doc = v.state.doc, sd = v.scrollDOM, c = this.c, dpr = devicePixelRatio || 1;
    const H = sd.clientHeight, fit = Math.floor(H / MINI_LH), lines = doc.lines;
    if (c.width !== MINI_W * dpr || c.height !== H * dpr) { c.width = MINI_W * dpr; c.height = H * dpr; c.style.height = H + 'px'; }
    const g = c.getContext('2d');
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    g.clearRect(0, 0, MINI_W, H);
    const room = Math.max(1, sd.scrollHeight - H), f = Math.min(1, sd.scrollTop / room);
    this.first = lines <= fit ? 1 : Math.round(1 + f * (lines - fit)); // the slice of the file the map shows
    const last = Math.min(lines, this.first + fit), from = doc.line(this.first).from, to = doc.line(last).to;
    g.globalAlpha = 0.55;
    g.fillStyle = '#8a919c';
    const pad = 6;
    for (let n = this.first; n <= last; n++) { // every word a little block
      const text = doc.line(n).text, y = (n - this.first) * MINI_LH;
      for (let i = 0, end = Math.min(text.length, 80); i < end; i++) {
        if (text.charCodeAt(i) <= 32) continue;
        let j = i;
        while (j < end && text.charCodeAt(j) > 32) j++;
        g.fillRect(pad + i * MINI_CW, y, (j - i) * MINI_CW, MINI_LH - 0.5);
        i = j;
      }
    }
    g.globalAlpha = 0.9; // the syntax colours over them, for what's parsed (the parser runs ahead of the viewport)
    highlightTree(syntaxTree(v.state), miniColor, (a, b, cls) => {
      const la = doc.lineAt(a);
      if (la.number < this.first || la.number > last) return;
      const col = a - la.from;
      if (col >= 80) return;
      g.fillStyle = cls.split(' ')[0];
      g.fillRect(pad + col * MINI_CW, (la.number - this.first) * MINI_LH, Math.min(b, la.to, la.from + 80) - a > 0 ? (Math.min(b, la.to, la.from + 80) - a) * MINI_CW : 0, MINI_LH - 0.5);
    }, from, to);
    g.globalAlpha = 1;
    const top = v.lineBlockAtHeight(sd.scrollTop), bot = v.lineBlockAtHeight(sd.scrollTop + H - 1);
    const a = doc.lineAt(top.from).number, b = doc.lineAt(bot.from).number;
    g.fillStyle = 'rgba(255,255,255,.08)'; // what's on screen
    g.fillRect(0, (a - this.first) * MINI_LH, MINI_W, Math.max(6, (b - a + 1) * MINI_LH));
    const cur = doc.lineAt(v.state.selection.main.head).number; // where you are
    g.fillStyle = 'rgba(63,233,174,.6)';
    g.fillRect(0, (cur - this.first) * MINI_LH, MINI_W, MINI_LH);
  }
  destroy() { cancelAnimationFrame(this.raf); this.view.scrollDOM.removeEventListener('scroll', this.queue); this.c.remove(); }
});

// editor(parent, { doc, name, readOnly, onChange(dirty), onSave() }) → { view, text(), set(doc), clean(), dirty(), destroy() }
function editor(parent, o) {
  const ro = new Compartment(), wrap = new Compartment();
  const view = new EditorView({
    parent,
    state: EditorState.create({
      doc: o.doc,
      extensions: [
        lineNumbers(), foldGutter(), highlightActiveLineGutter(), highlightSpecialChars(), history(), drawSelection(), dropCursor(),
        EditorState.allowMultipleSelections.of(true), indentOnInput(), bracketMatching(), closeBrackets(), rectangularSelection(), crosshairCursor(),
        highlightActiveLine(), highlightSelectionMatches(), search({ top: true }),
        syntaxHighlighting(style), theme, langFor(o.name || ''),
        ro.of(EditorState.readOnly.of(!!o.readOnly)), wrap.of(o.wrap ? EditorView.lineWrapping : []),
        keymap.of([
          { key: 'Mod-s', preventDefault: true, run: () => { o.onSave?.(); return true; } },
          ...closeBracketsKeymap, ...defaultKeymap, ...searchKeymap, ...historyKeymap, ...foldKeymap, indentWithTab,
        ]),
        o.minimap === false ? [] : minimap,
        EditorView.updateListener.of((u) => { if (u.docChanged) o.onChange?.(!u.state.doc.eq(saved)); }),
      ],
    }),
  });
  let saved = view.state.doc; // a Text: eq() compares chunk by chunk, no string copies per keystroke
  return {
    view,
    text: () => view.state.doc.toString(),
    set(doc) { view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: doc } }); saved = view.state.doc; o.onChange?.(false); },
    clean() { saved = view.state.doc; o.onChange?.(false); },
    dirty: () => !view.state.doc.eq(saved),
    readOnly: (on) => view.dispatch({ effects: ro.reconfigure(EditorState.readOnly.of(on)) }),
    wrap: (on) => view.dispatch({ effects: wrap.reconfigure(on ? EditorView.lineWrapping : []) }),
    destroy: () => view.destroy(),
  };
}

window.CM = { editor };
