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
  // Claude Code 2.1.29x: queued messages arrive merged (a path glued to the text before it), images it attached
  // marked [Image #N], and where they came from appended (transcript.go folds "[Image: source: …]" in)
  const u = '/workspace/home/.local/state/control-panel/uploads/';
  const merged = `[Image #4] [Image #5]${u}20261010-003611-11eeabb54f51c242-Screenshot_2026-10-09_at_5.36.09_pm.png can you also ship it. Ship to production${u}20261010-003749-8d9b6c542a9275eb-remote-landlord-logo-uncropped.png use this logo\n${u}20261010-003505-d90e813611fb2709-remote-landlord-logo-uncropped.png\n${u}20261010-003505-d90e813611fb2709-remote-landlord-logo-uncropped.png`;
  assert.deepStrictEqual(splitUploads(merged), { text: 'can you also ship it. Ship to production use this logo', names: [
    '20261010-003611-11eeabb54f51c242-Screenshot_2026-10-09_at_5.36.09_pm.png', '20261010-003749-8d9b6c542a9275eb-remote-landlord-logo-uncropped.png', '20261010-003505-d90e813611fb2709-remote-landlord-logo-uncropped.png'] });
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

test('splitTurns: a turn ends at its "Worked for", so a turn no message of yours began keeps the last answer an answer', () => {
  const it = (k, text = '') => ({ k, text });
  // what the chat got when another session's messages (hidden entries) each started a little turn
  const items = [it('user', 'go'), it('tool'), it('result'), it('text', 'The answer.'), it('note', 'Worked for 12s'),
    it('text', 'Noted.'), it('note', 'Worked for 1s'), it('text', 'Nothing to do.'), it('note', 'Worked for 2s')];
  assert.deepStrictEqual(splitTurns(items), [
    { user: 0, work: [1, 2], final: 3, notes: [], worked: 4 },
    { user: -1, work: [], final: 5, notes: [], worked: 6 },
    { user: -1, work: [], final: 7, notes: [], worked: 8 },
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
test('pendingQueue replays the queue: dequeue takes your oldest message first, remove takes its text', () => {
  const q = (name, text = '') => ({ k: 'queue', name, text });
  assert.deepStrictEqual(pendingQueue([q('enqueue', 'a'), q('enqueue', 'b'), q('dequeue'), q('enqueue', 'c'), q('remove', 'c')]), ['b']);
  assert.deepStrictEqual(pendingQueue([q('enqueue', '<task-notification>\n<task-id>b1</task-id>'), q('enqueue', 'mine')]), ['mine'], 'background-task notifications are not your messages');
  assert.deepStrictEqual(pendingQueue([q('enqueue', 'fix the bug please, and then run the tests again'), q('remove', 'fix the bug please, and then run the tests again ')]), [], 'a remove whose text drifted still takes its message');
  const note = '<task-notification>\n<task-id>b1</task-id>';
  assert.deepStrictEqual(pendingQueue([q('enqueue', note), q('enqueue', 'mine'), q('dequeue'), q('enqueue', note), q('remove', note)]), [],
    'Claude Code takes your messages before background-task notifications, whatever their order');
  assert.deepStrictEqual(pendingQueue([q('enqueue', '<agent-message from="a1">\nDone.'), q('enqueue', '<pasted_content id="07e5">\nhi')]), ['<pasted_content id="07e5">\nhi'], 'a subagent\'s message is not yours; a paste is');
  assert.deepStrictEqual(pendingQueue([q('enqueue', 'mine'), { k: 'user', text: 'mine' }]), [], 'a message that reached Claude is not waiting any more');
  assert.deepStrictEqual(pendingQueue([{ k: 'user', text: 'yes' }, q('enqueue', 'yes')]), ['yes'], 'an earlier message with the same words does not count');
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

const { mainDir } = require('./web/lib.js');
test('mainDir shows a worktree as its repo', () => {
  assert.strictEqual(mainDir('~/repos/app/.worktrees/feat-x'), '~/repos/app');
  assert.strictEqual(mainDir('~/repos/app/.worktrees/scratch/h2'), '~/repos/app');
  assert.strictEqual(mainDir('/home/me/repos/app/.claude/worktrees/fix-y'), '/home/me/repos/app');
  assert.strictEqual(mainDir('~/repos/app'), '~/repos/app');
  assert.strictEqual(mainDir('~/notes/.worktrees-old'), '~/notes/.worktrees-old');
});

const { effShellPath, effShellGeom, effCenter, effPlateau, EFF } = require('./web/lib.js');
test('the effort slider shell: a pill at rest, a shoulder that follows the thumb and tilts onto the caps', () => {
  assert.strictEqual(effShellPath(effCenter(2, 5), 0, effPlateau(64)), 'M16 20H208A16 16 0 0 1 208 52H16A16 16 0 0 1 16 20Z', 'at rest it is the plain pill');
  assert.strictEqual(effCenter(0, 5), EFF.RADIUS);
  assert.strictEqual(effCenter(4, 5), EFF.TRACK - EFF.RADIUS);
  for (let v = 0; v <= 4; v += 0.25) {
    for (const open of [0.3, 1]) {
      const d = effShellPath(effCenter(v, 5), open, effPlateau(80));
      assert.ok(!/NaN|Infinity/.test(d) && d.endsWith('Z'), `value ${v}, open ${open}: ${d}`);
    }
  }
  const mid = effShellGeom(effCenter(2, 5), effPlateau(80));
  assert.ok(Math.abs(mid.topR - mid.topL - effPlateau(80)) < 0.01 && mid.phiL === 0 && mid.phiR === 0, 'in the middle the plateau keeps its width, flat');
  const left = effShellGeom(effCenter(0, 5), effPlateau(80));
  assert.ok(left.phiL > 0 && left.topL >= EFF.RADIUS, 'at the left end the shoulder walks down the cap and the plateau stays on the track');
});

const { stillPending } = require('./web/lib.js');
test('stillPending: a sent message waits until the chat shows Claude took it (or queued it)', () => {
  const long = 'Yes, the way it should work is that if there are rules that should only be valid until a date, the job removes them.\nSecond line.';
  const sent = [{ text: long, at: 1000 }, { text: 'short one', at: 1000 }, { text: 'old one', at: 1000 }];
  const items = [
    { k: 'user', t: 990, text: 'old one' }, // the same words, sent before: not this message
    { k: 'user', t: 1025, text: `<pasted_content id="dec7">\n${long}\n</pasted_content id="dec7">` },
    { k: 'queue', name: 'enqueue', t: 1003, text: 'short one' },
  ];
  assert.deepStrictEqual(stillPending(sent, items).map((s) => s.text), ['old one']);
  assert.deepStrictEqual(stillPending([{ text: '/effort high', at: 1000 }], []), [], 'a slash command shows as a note, not a bubble: nothing to wait for');
});

const { fitScreen } = require('./web/lib.js');
test('fitScreen crops a frame to the pane: a full-screen app leaves stale rows and columns past it', () => {
  const frame = ['\x1b[1m TeamClaude\x1b[m' + ' '.repeat(10) + '\x1b[32m4 sess\x1b[m', 'row 2', 'row 3', 'stale footer from a taller frame'];
  const out = fitScreen(frame, 3, 14);
  assert.strictEqual(out.length, 3, 'rows past the pane are gone');
  assert.strictEqual(out[0], '\x1b[1m TeamClaude\x1b[m   \x1b[m', 'cut at 14 columns, its colours closed');
  assert.strictEqual(out[1], 'row 2');
  assert.deepStrictEqual(fitScreen(frame, 0, 0), frame, 'size unknown: as it came');
  assert.strictEqual(fitScreen(['─'.repeat(20)], 1, 5)[0], '─────\x1b[m', 'box drawing counts one column each');
});

const { groupNotes } = require('./web/lib.js');
test('groupNotes folds a run of one monitor\'s events, and the little turns they woke, into one entry', () => {
  const ev = (name, out) => ({ k: 'note', name: 'task:event', text: `Monitor event: "${name}"`, out, t: 100 });
  const worked = (s) => ({ k: 'note', text: `Worked for ${s}s` });
  const items = [ev('swarm', 'a'), worked(3), ev('swarm', 'b'), worked(1), ev('swarm', 'c'), worked(7), { k: 'note', text: '/compact' }, ev('other', 'x')];
  const g = groupNotes(items, items.map((_, i) => i));
  assert.deepStrictEqual(g.map((x) => (x.events ? `group:${x.events.join(',')}+worked:${x.worked.join(',')}` : `note:${x.i}`)), ['group:0,2,4+worked:1,3,5', 'note:6', 'note:7']);
  assert.deepStrictEqual(groupNotes(items, [0, 1, 6]).map((x) => (x.events ? 'group' : x.i)), [0, 1, 6], 'a single event stays a line');
});

const { ddMatch } = require('./web/lib.js');
test('ddMatch: typing in an open dropdown jumps to the option that starts with it', () => {
  const ls = ['~/repos/alpha', 'Shell', '~/projects/control-panel', 'Claude (claude-multi)', 'shared'];
  assert.strictEqual(ddMatch(ls, 's', 0), 1);
  assert.strictEqual(ddMatch(ls, 's', 1), 4); // the same letter again: the next one
  assert.strictEqual(ddMatch(ls, 's', 4), 1); // and round again
  assert.strictEqual(ddMatch(ls, 'sh', 1), 1); // a longer prefix stays while it still fits
  assert.strictEqual(ddMatch(ls, 'sha', 1), 4);
  assert.strictEqual(ddMatch(ls, '~/p', 0), 2);
  assert.strictEqual(ddMatch(ls, 'z', 0), -1);
});

const { sessionSlug } = require('./web/lib.js');
test('sessionSlug turns what was typed into a name zellij takes (letters, digits, . _ -, starting with one of the first two)', () => {
  assert.strictEqual(sessionSlug('Baker Capital Integration'), 'Baker-Capital-Integration');
  assert.strictEqual(sessionSlug('  my tab! '), 'my-tab');
  assert.strictEqual(sessionSlug('Café / Crème'), 'Cafe-Creme');
  assert.strictEqual(sessionSlug('-.hidden_name.v2'), 'hidden_name.v2');
  assert.strictEqual(sessionSlug('scratch'), 'scratch');
  assert.strictEqual(sessionSlug('@@@'), '');
  assert.strictEqual(sessionSlug('x'.repeat(80)).length, 64);
});

test('pendingQueue: messages pulled back out of the queue (↑ in Claude Code: popAll) are no longer queued', () => {
  const q = (name, text) => ({ k: 'queue', name, text });
  const items = [q('enqueue', 'A'), q('enqueue', 'B'), q('popAll', 'A'), q('popAll', 'B'), q('enqueue', 'B')];
  assert.deepStrictEqual(pendingQueue(items), ['B']); // A cancelled, B put back
});

test('stillPending: a message Claude took merged into another (stopped, then the box sent with it) is delivered', () => {
  const at = 1000;
  const sent = [{ text: 'Continue. Disregard the last message.', at }];
  const merged = [{ k: 'user', t: at + 20, text: 'Also, every page says Could not load. This is unacceptable.Continue. Disregard the last message.' }];
  assert.deepStrictEqual(stillPending(sent, merged), []);
  const earlier = [{ k: 'user', t: at - 60, text: 'Continue. Disregard the last message.' }]; // the same words before it was sent: not it
  assert.strictEqual(stillPending(sent, earlier).length, 1);
});

test('stillPending: a message with photos waits like any other, and is delivered as Claude Code records it', () => {
  const up = (n) => `/workspace/home/.local/state/control-panel/uploads/20261011-00063${n}-4f9b7d19d66e8f2f-shot${n}.png`;
  const sent = [{ text: `${up(1)} ${up(2)} Can you check the KPIs?`, at: 1000 }, { text: `${up(3)}`, at: 1000 }];
  assert.strictEqual(stillPending(sent, []).length, 2, "a photo's path starts with / but it's no slash command: it waits for Claude");
  const items = [ // its photos are [Image #N] in the text, and their paths come after it (transcript.go)
    { k: 'user', t: 1002, text: `[Image #1] [Image #2] Can you check the KPIs?\n${up(1)}\n${up(2)}` },
    { k: 'user', t: 1003, text: `[Image #3]\n${up(3)}` },
  ];
  assert.deepStrictEqual(stillPending(sent, items), []);
  assert.strictEqual(stillPending(sent.slice(1), items.slice(0, 1)).length, 1, 'a photo alone is matched by its photo');
});

const { termKind, askOf, termState, termPurpose } = require('./web/lib.js');
test('termState: one word per pane, from what runs in it', () => {
  const agent = (state, last = '') => ({ kind: 'claude', state, agent: { last } });
  assert.strictEqual(termState(agent('working')), 'working');
  assert.strictEqual(termState(agent('stalled')), 'working', 'quiet for a while is still a turn going');
  assert.strictEqual(termState(agent('asking')), 'input', 'a dialog on screen: a quick answer');
  assert.strictEqual(termState(agent('waiting', "It's built and tested end to end, except for the Cloudflare calls: new-box can't run until you create a Cloudflare API token for Access (step 1 below).")), 'blocked');
  assert.strictEqual(termState(agent('waiting', 'I renamed the files. Should I also update the imports?')), 'input');
  assert.strictEqual(termState(agent('waiting', 'Both folders are now private repos in the roomss-ltd GitHub org.')), 'done');
  const svc = (x) => ({ kind: 'shell', state: 'running', fg: 12, cpu: 1, ...x });
  assert.strictEqual(termState(svc()), 'running');
  assert.strictEqual(termState(svc({ errNow: true })), 'error', 'errors in its newest output');
  assert.strictEqual(termState(svc({ cpu: 140 })), 'slow');
  assert.strictEqual(termState(svc({ state: 'stopped' })), 'stopped');
  assert.strictEqual(termState({ kind: 'shell', state: 'idle' }), 'idle');
  assert.strictEqual(termKind({ kind: 'cmd', state: 'running' }), 'service');
});
test('askOf: the clause that needs you, not the whole reply', () => {
  assert.deepStrictEqual(askOf("It's built and tested end to end, except for the Cloudflare calls: new-box can't run until you create a Cloudflare API token for Access (step 1 below)."),
    { k: 'blocked', text: "new-box can't run until you create a Cloudflare API token for Access (step 1 below)." });
  assert.deepStrictEqual(askOf('**Done.** The `api` tests pass.'), { k: 'done', text: 'Done.' });
  assert.deepStrictEqual(askOf('anything', 'Switch model?'), { k: 'input', text: 'Switch model?' });
  assert.strictEqual(askOf('').k, 'done');
});
test('termPurpose: a session is typed by what runs in it', () => {
  const ag = (root) => ({ kind: 'claude', agent: {}, git: { root } });
  assert.strictEqual(termPurpose([{ kind: 'shell', state: 'running', fg: 3 }, { kind: 'cmd', state: 'running' }])[0], 'Infra');
  assert.strictEqual(termPurpose([ag('/workspace/home/repos/alven-estate-agent')])[0], 'Product');
  assert.strictEqual(termPurpose([ag('/workspace/home/projects/control-panel')])[0], 'Tooling');
  assert.strictEqual(termPurpose([])[0], '');
});
