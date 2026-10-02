// Unit tests for the pure browser helpers in web/lib.js. Run: node --test
const test = require('node:test');
const assert = require('node:assert');
const { keepOrder } = require('./web/lib.js');

test('keepOrder keeps previous positions, appends new items, drops gone ones', () => {
  const items = [{ k: 'c' }, { k: 'new1' }, { k: 'a' }, { k: 'new2' }];
  const got = keepOrder(['a', 'b', 'c'], items, (x) => x.k).map((x) => x.k);
  assert.deepStrictEqual(got, ['a', 'c', 'new1', 'new2']);
});

test('keepOrder with no previous order keeps the incoming order', () => {
  const got = keepOrder([], [{ k: 2 }, { k: 1 }], (x) => x.k).map((x) => x.k);
  assert.deepStrictEqual(got, [2, 1]);
});

const { ansiSegments, md, esc } = require('./web/lib.js');

test('ansiSegments reads zellij subscribe SGR runs and drops other escapes', () => {
  const line = '\x1b[32m\x1b[49m\x1b[29m\x1b[28m\x1b[27m\x1b[25m\x1b[1m\x1b[24m\x1b[22m\x1b[1m\x1b[23m➜  \x1b[36m/tmp\x1b[m echo \x1b]8;;http://x\x07hi\x1b]8;;\x07\x1b[2K!';
  const got = ansiSegments(line);
  assert.deepStrictEqual(got.map((s) => [s.text, s.fg || '', !!s.b]), [
    ['➜  ', PAL(2), true],
    ['/tmp', PAL(6), true],
    [' echo hi!', '', false],
  ]);
});

test('ansiSegments handles 256-colour, truecolour, inverse and bg', () => {
  const [a, b, c] = ansiSegments('\x1b[38;5;196ma\x1b[48;2;1;2;3;7mb\x1b[0;39;49mc');
  assert.strictEqual(a.fg, '#ff0000');
  assert.strictEqual(b.bg, '#010203');
  assert.strictEqual(b.inv, true);
  assert.deepStrictEqual(c, { text: 'c' });
});

test('md escapes first, then renders blocks', () => {
  assert.strictEqual(md('Use `x<y`\n```js\nif (a<b) {}\n```\n## Plan\n**bold** <img src=x onerror=alert(1)>'),
    '<p>Use <code>x&lt;y</code></p><div class="cb"><div class="cb-h"><span class="cb-l">js</span><button type="button" class="cb-copy">Copy</button></div><pre><code><span class="cl"><span class="tk-k">if</span> (a&lt;b) {}</span></code></pre></div>' +
    '<div class="mh mh2">Plan</div><p><b>bold</b> &lt;img src=x onerror=alert(1)&gt;</p>');
});

test('md lists, tables, quotes', () => {
  assert.strictEqual(md('Steps:\n- one\n- **two**\n\n1. a\n2. b'), '<p>Steps:</p><ul><li>one</li><li><b>two</b></li></ul><ol><li>a</li><li>b</li></ol>');
  assert.strictEqual(md('| A | B |\n|---|:-:|\n| 1 | `x` |'), '<table><thead><tr><th>A</th><th>B</th></tr></thead><tbody><tr><td>1</td><td><code>x</code></td></tr></tbody></table>');
  assert.strictEqual(md('> hi\n> there'), '<blockquote>hi<br>there</blockquote>');
});

test('md links: http(s) only; code spans are literal', () => {
  assert.strictEqual(md('[docs](https://x.com/a?b=1&c=2) [bad](javascript:alert(1))'),
    '<p><a href="https://x.com/a?b=1&amp;c=2" target="_blank" rel="noopener noreferrer">docs</a> [bad](javascript:alert(1))</p>');
  assert.strictEqual(md('`**not bold**` and *it*'), '<p><code>**not bold**</code> and <i>it</i></p>');
  assert.strictEqual(md('snake_case_name stays'), '<p>snake_case_name stays</p>');
});

const { keyBytes, ctrlChar, b64utf8 } = require('./web/lib.js');
test('keyBytes maps keys to what a terminal sends', () => {
  const k = (key, mods = {}) => keyBytes({ key, ctrlKey: false, altKey: false, metaKey: false, shiftKey: false, ...mods });
  assert.strictEqual(k('Enter'), '\r');
  assert.strictEqual(k('Backspace'), '\x7f');
  assert.strictEqual(k('ArrowUp'), '\x1b[A');
  assert.strictEqual(k('Tab'), '\t');
  assert.strictEqual(k('Tab', { shiftKey: true }), '\x1b[Z');
  assert.strictEqual(k('Escape'), '\x1b');
  assert.strictEqual(k('Delete'), '\x1b[3~');
  assert.strictEqual(k('F5'), '\x1b[15~');
  assert.strictEqual(k('c', { ctrlKey: true }), '\x03');
  assert.strictEqual(k('[', { ctrlKey: true }), '\x1b');
  assert.strictEqual(k(' ', { ctrlKey: true }), '\x00');
  assert.strictEqual(k('b', { altKey: true }), '\x1bb');
  assert.strictEqual(k('a'), null); // printable: comes through the input event
  assert.strictEqual(k('v', { metaKey: true }), null); // Cmd+V: the browser pastes
  assert.strictEqual(ctrlChar('d'), '\x04');
  assert.strictEqual(b64utf8('é\r'), Buffer.from('é\r').toString('base64'));
});

test('esc escapes quotes too', () => {
  assert.strictEqual(esc(`<a href="x" title='y'>&`), '&lt;a href=&quot;x&quot; title=&#39;y&#39;&gt;&amp;');
});

function PAL(i) { return require('./web/lib.js').PALETTE[i]; }

const { splitUploads } = require('./web/lib.js');
test('splitUploads turns sent image paths into thumbnails', () => {
  const t = '/home/u/.local/state/control-panel/uploads/20260930-060302-cca1e85820f38a34.png /x/uploads/20260930-060303-00000000000000ff.jpg What is this?';
  assert.deepStrictEqual(splitUploads(t), { text: 'What is this?', names: ['20260930-060302-cca1e85820f38a34.png', '20260930-060303-00000000000000ff.jpg'] });
  assert.deepStrictEqual(splitUploads('see /etc/passwd and uploads/../x.png'), { text: 'see /etc/passwd and uploads/../x.png', names: [] });
  assert.deepStrictEqual(splitUploads('/s/uploads/20260930-060303-00000000000000ff-report_v2.pdf check'), { text: 'check', names: ['20260930-060303-00000000000000ff-report_v2.pdf'] });
});

test('md: tables without edge pipes, diagrams keep their shape', () => {
  assert.strictEqual(md('Name | Runs\n--- | ---:\nCI | 564'),
    '<table><thead><tr><th>Name</th><th>Runs</th></tr></thead><tbody><tr><td>CI</td><td>564</td></tr></tbody></table>');
  assert.strictEqual(md('Flow:\nphone ─HTTPS─▶ Cloudflare\n      ─▶ tunnel <x>\nDone.'),
    '<p>Flow:</p><pre class="diagram">phone ─HTTPS─▶ Cloudflare\n      ─▶ tunnel &lt;x&gt;</pre><p>Done.</p>');
  assert.ok(md('```\n┌─┐\n└─┘\n```').includes('<span class="cb-l">diagram</span><button type="button" class="cb-copy">Copy</button></div><pre><code>┌─┐\n└─┘</code>'));
});

const { hl, splitTurns } = require('./web/lib.js');
test('hl colours tokens per line and escapes each one', () => {
  assert.deepStrictEqual(hl('const a = f("x<y", 2) // hi', 'js'),
    ['<span class="tk-k">const</span> a = <span class="tk-f">f</span>(<span class="tk-s">&quot;x&lt;y&quot;</span>, <span class="tk-n">2</span>) <span class="tk-c">// hi</span>']);
  assert.deepStrictEqual(hl('echo "$HOME" # note', 'sh'), ['<span class="tk-k">echo</span> <span class="tk-s">&quot;$HOME&quot;</span> <span class="tk-c"># note</span>']);
  assert.deepStrictEqual(hl('a { color: #fff }', 'css'), ['a { color: #fff }'], '# starts a comment only in shell-like languages');
  assert.deepStrictEqual(hl('x=a#b # c', 'sh'), ['x=a#b <span class="tk-c"># c</span>'], 'a # inside a word is not a comment');
  assert.deepStrictEqual(hl('/* a\nb */ x', 'go'), ['<span class="tk-c">/* a</span>', '<span class="tk-c">b */</span> x'], 'a token spanning lines is closed on each line');
  assert.deepStrictEqual(hl("don't stop", ''), ["don&#39;t stop"], 'an unclosed quote is plain text');
});

test('splitTurns folds the work before each final answer; notes stay visible', () => {
  const it = (k, text = '') => ({ k, text });
  const items = [it('text', 'older'), it('user', 'go'), it('think'), it('tool'), it('result'), it('text', 'mid'), it('tool'), it('result'),
    it('note', 'Interrupted'), it('text', 'final'), it('note', 'Worked for 2m 8s'), it('user', 'next'), it('tool')];
  assert.deepStrictEqual(splitTurns(items), [
    { user: -1, work: [], final: 0, notes: [], worked: -1 },
    { user: 1, work: [2, 3, 4, 5, 6, 7], final: 9, notes: [8], worked: 10 },
    { user: 11, work: [12], final: -1, notes: [], worked: -1 },
  ]);
});

test('md: a numbered list split by bullets keeps counting', () => {
  assert.strictEqual(md('1. Hygiene\n- a\n2. Claude\n- b'), '<ol><li>Hygiene</li></ol><ul><li>a</li></ul><ol start="2"><li>Claude</li></ol><ul><li>b</li></ul>');
});

const { scrolledOff } = require('./web/lib.js');
test('scrolledOff finds how many lines left the top of the screen', () => {
  const prev = ['a1', 'a2', 'out 1', 'out 2', 'out 3', 'out 4', '', '> prompt'];
  const next = ['out 1', 'out 2', 'out 3', 'out 4', 'out 5', 'out 6', '', '> prompt '];
  assert.strictEqual(scrolledOff(prev, next), 2);
  assert.strictEqual(scrolledOff(prev, prev.map((l, i) => (i === 7 ? '> typing' : l))), 0, 'only the bottom changed');
  assert.strictEqual(scrolledOff(['', '', '', 'x'], ['', '', 'x', 'y']), 0, 'blank lines alone prove nothing');
  assert.strictEqual(scrolledOff(prev, ['totally', 'different', 'screen', '', '', '', '', '']), 0);
});

const { pendingQueue } = require('./web/lib.js');
test('pendingQueue replays the queue: dequeue takes the oldest, remove takes its text', () => {
  const q = (name, text = '') => ({ k: 'queue', name, text });
  assert.deepStrictEqual(pendingQueue([q('enqueue', 'a'), q('enqueue', 'b'), q('dequeue'), q('enqueue', 'c'), q('remove', 'c')]), ['b']);
  assert.deepStrictEqual(pendingQueue([q('enqueue', '<task-notification>\n<task-id>b1</task-id>'), q('enqueue', 'mine')]), ['mine'], 'background-task notifications are not your messages');
  assert.deepStrictEqual(pendingQueue([q('enqueue', 'fix the bug please, and then run the tests again'), q('remove', 'fix the bug please, and then run the tests again ')]), [], 'a remove whose text drifted still takes its message');
  assert.deepStrictEqual(splitTurns([{ k: 'user', text: 'go' }, q('enqueue', 'later'), { k: 'text', text: 'ok' }]),
    [{ user: 0, work: [], final: 2, notes: [], worked: -1 }], 'queue operations are not part of a turn');
});

const { termLineKind } = require('./web/lib.js');
test('termLineKind tells rules and tables (keep their shape) from text (may wrap)', () => {
  assert.strictEqual(termLineKind('────────────────────────────────'), 'rule');
  assert.strictEqual(termLineKind('▔▔▔▔▔▔▔▔▔▔▔▔ Ctrl+Y to paste deleted text ▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔'), 'rule');
  assert.strictEqual(termLineKind('│ Check            │ Result                 │'), 'grid');
  assert.strictEqual(termLineKind('┌──────┬──────┐'), 'grid');
  assert.strictEqual(termLineKind('● The chat header subtitle currently duplicates the tab name'), 'text');
  assert.strictEqual(termLineKind(''), 'text');
});

const { mdInline } = require('./web/lib.js');

test('mdInline styles a message from you but keeps its lines and escapes it', () => {
  assert.strictEqual(mdInline('- It is **not a git repo: never `git init`**\n<b>x</b> *ok*'),
    '- It is <b>not a git repo: never <code>git init</code></b>\n&lt;b&gt;x&lt;/b&gt; <i>ok</i>');
});
