'use strict';

// Spreadsheets parsed, edited and written off the page's thread (SheetJS): a 60k-row workbook takes a second or two
// to read, and the page would freeze for it. The workbook stays here; the page gets each sheet as the text of its
// cells and a byte per cell (1 a number, 2 a formula), which costs a fraction of copying the workbook across.
// One worker serves every open sheet tab (SheetJS loads once); each tab's workbook is kept under its id until it closes.
const books = new Map();
// a tab's read in progress: { ac (closing the tab stops its download and parse), url, edits made meanwhile, done }.
// Until the whole book is in: edits queue, another sheet's first rows come by ranges too, a save waits for it.
const reads = new Map();
let wb = null;

function pack(name) {
  const ws = wb.Sheets[name], data = ws['!data'] || [];
  const ref = ws['!ref'] ? XLSX.utils.decode_range(ws['!ref']) : { e: { r: -1, c: -1 } };
  const rows = ref.e.r + 1, cols = Math.min(ref.e.c + 1, 200), text = new Array(rows * cols).fill(''), kind = new Uint8Array(rows * cols);
  for (let r = 0; r < rows; r++) {
    const row = data[r];
    if (!row) continue;
    for (let c = 0; c < cols; c++) {
      const v = row[c];
      if (!v) continue;
      text[r * cols + c] = String(v.w ?? v.v ?? '');
      kind[r * cols + c] = v.F ? 3 : v.f ? 2 : v.t === 'n' ? 1 : 0; // 3: in an array formula (Excel changes it only whole)
    }
  }
  return { name, rows, cols, allCols: ref.e.c + 1, text, kind };
}

// what you'd type to get this cell back, and what it shows
const cell = (name, r, c) => {
  const v = wb.Sheets[name]['!data']?.[r]?.[c];
  return v ? { src: v.f ? '=' + v.f : v.v instanceof Date ? v.w ?? v.v.toISOString() : String(v.v ?? ''), shown: String(v.w ?? v.v ?? ''), f: !!v.f } : null;
};

function set(name, r, c, raw) { // a number, a formula (=…, valued when Excel recalculates), text, or nothing
  const ws = wb.Sheets[name], data = (ws['!data'] ||= []), row = (data[r] ||= []), n = raw.trim();
  if (raw === '') row[c] = undefined;
  else if (raw[0] === '=') row[c] = { t: 'n', f: raw.slice(1), w: '' };
  else if (/^[-+]?(\d+\.?\d*|\.\d+)(e[-+]?\d+)?$/i.test(n)) row[c] = { t: 'n', v: +n, w: n };
  else row[c] = { t: 's', v: raw, w: raw };
  const rg = ws['!ref'] ? XLSX.utils.decode_range(ws['!ref']) : { s: { r: 0, c: 0 }, e: { r: 0, c: 0 } };
  rg.e.r = Math.max(rg.e.r, r);
  rg.e.c = Math.max(rg.e.c, c);
  ws['!ref'] = XLSX.utils.encode_range(rg);
  return { shown: row[c] ? row[c].w : '', kind: row[c] ? (row[c].f ? 2 : row[c].t === 'n' ? 1 : 0) : 0 };
}

// A big .xlsx's first rows without reading all of it: the zip's entries from its central directory, the sheet
// inflated natively (DecompressionStream) and read only until n rows are in, then the stream cancelled. Values are
// shown through SheetJS's own formatter (SSF), so dates and currencies look as they will once the whole book is in.
// Anything unusual throws, and the caller falls back to SheetJS reading the first rows itself.
const XML_ENT = { amp: '&', lt: '<', gt: '>', quot: '"', apos: "'" };
const unxml = (s) => s.replace(/&(#x[0-9a-f]+|#\d+|\w+);/gi, (m, e) => (e[0] === '#' ? String.fromCodePoint(e[1] === 'x' || e[1] === 'X' ? parseInt(e.slice(2), 16) : +e.slice(1)) : XML_ENT[e] ?? m));
const ATTR = {}; // an attribute's pattern, compiled once
const attr = (tag, k) => tag.match((ATTR[k] ||= new RegExp(`\\s${k}="([^"]*)"`)))?.[1];
const texts = (xml) => [...xml.matchAll(/<t(?:\s[^>]*)?>([^<]*)<\/t>/g)].map((t) => unxml(t[1])).join('');
// The zip's entries, from its central directory (at the end of the file): name → { method, size, at }.
function zipDir(u8, base) {
  const dv = new DataView(u8.buffer, u8.byteOffset, u8.byteLength);
  let e = u8.length - 22;
  while (e >= 0 && dv.getUint32(e, true) !== 0x06054b50) e--; // the end of the central directory
  if (e < 0) throw new Error('not a zip');
  const files = {}, dec = new TextDecoder(), cd = dv.getUint32(e + 16, true) - base;
  if (cd < 0) throw new Error('the directory is further back'); // a huge one: SheetJS reads the whole file instead
  for (let p = cd, i = dv.getUint16(e + 10, true); i > 0; i--) {
    const nl = dv.getUint16(p + 28, true);
    files[dec.decode(u8.subarray(p + 46, p + 46 + nl))] = { method: dv.getUint16(p + 10, true), size: dv.getUint32(p + 20, true), at: dv.getUint32(p + 42, true) };
    p += 46 + nl + dv.getUint16(p + 30, true) + dv.getUint16(p + 32, true);
  }
  return files;
}
const inflate = (f, s) => (f.method === 8 ? s.pipeThrough(new DecompressionStream('deflate-raw')) : s);

// a zip in memory
function zipLocal(buf) {
  const u8 = new Uint8Array(buf), dv = new DataView(buf), files = zipDir(u8, 0);
  return {
    files,
    stream(f) {
      const start = f.at + 30 + dv.getUint16(f.at + 26, true) + dv.getUint16(f.at + 28, true);
      return inflate(f, new Blob([u8.subarray(start, start + f.size)]).stream());
    },
  };
}

// a zip on the panel, read by ranges: its directory with one request from the end, then only the entries asked for,
// the sheet streamed and cancelled once the first rows are in. Over a slow link that's a few hundred KB, not the file.
async function zipRemote(url) {
  const range = (from, to) => fetch(url, { credentials: 'same-origin', headers: { Range: to === undefined ? `bytes=${from}` : `bytes=${from}-${to - 1}` } });
  const r = await range('-65600'); // the last 64 KB: the directory of a workbook's few dozen parts
  if (r.status !== 206) throw new Error('no ranges');
  const tail = new Uint8Array(await r.arrayBuffer()), total = +r.headers.get('Content-Range').split('/')[1];
  if (total < 1.5e6) throw new Error('small: quicker read whole'); // opened from search, its size unknown until now
  const files = zipDir(tail, total - tail.length);
  return {
    files,
    mtime: +r.headers.get('X-Mtime') || 0, // the version these ranges are of: a save before the whole book is in checks it
    async stream(f) { // its local header and its data in one request (a header's name and extra field: well under 1 KB);
      // cancelling the stream aborts the request
      const body = (await range(f.at, f.at + 30 + 1024 + f.size)).body;
      let head = null, skip = -1, left = f.size;
      return inflate(f, body.pipeThrough(new TransformStream({
        transform(b, c) {
          if (skip < 0) { // the header's 30 bytes say how long the rest of it is
            if (head) { const x = new Uint8Array(head.length + b.length); x.set(head); x.set(b, head.length); b = x; }
            if (b.length < 30) { head = b; return; }
            const dv = new DataView(b.buffer, b.byteOffset, b.length);
            skip = 30 + dv.getUint16(26, true) + dv.getUint16(28, true);
          }
          const k = Math.min(skip, b.length);
          skip -= k;
          b = b.subarray(k, k + left);
          left -= b.length;
          if (b.length) c.enqueue(b);
        },
      })));
    },
  };
}

// src: a workbook's bytes, or its URL on the panel; want: a sheet's name (else the first); got: what an earlier call
// for the same file fetched (its directory, its parts); early(first rows): the first 200, while the rest of n come
// The whole file, read while its tab is in front: behind another it stops (the link is for what's in front) and,
// shown again, goes on from the byte it had (from the start if the file changed meanwhile, or ranges don't work).
async function fetchWhole(url, r0) {
  const parts = [];
  let got = 0, mtime = 0;
  for (;;) {
    await r0.gate;
    r0.ac.signal.throwIfAborted(); // the tab closed while it waited
    const ac = new AbortController(), stop = () => ac.abort();
    r0.ac.signal.addEventListener('abort', stop);
    r0.pause = stop;
    try {
      const res = await fetch(url, { credentials: 'same-origin', signal: ac.signal, priority: 'low', headers: got ? { Range: `bytes=${got}-` } : {} });
      if (!res.ok) throw new Error(`Error ${res.status}`);
      const m = +res.headers.get('X-Mtime') || 0;
      if (got && (res.status !== 206 || m !== mtime)) { parts.length = 0; got = 0; }
      mtime = m;
      for (const rd = res.body.getReader(); ;) {
        const { value, done } = await rd.read();
        if (done) break;
        parts.push(value);
        got += value.length;
      }
      const buf = new Uint8Array(got);
      for (let o = 0, i = 0; i < parts.length; o += parts[i].length, i++) buf.set(parts[i], o);
      return { buf: buf.buffer, mtime };
    } catch (e) {
      if (r0.ac.signal.aborted || !ac.signal.aborted) throw e; // closed, or failed; else paused: on when it's shown
    } finally {
      r0.ac.signal.removeEventListener('abort', stop);
      r0.pause = null;
    }
  }
}

async function fastFirstRows(src, n, want, got = new Map(), early = null) {
  const zip = typeof src === 'string' ? await (got.get('') ?? got.set('', zipRemote(src)).get('')) : zipLocal(src);
  const entry = (name) => { const f = zip.files[name.replace(/^\//, '')]; return f && (f.method === 0 || f.method === 8) ? f : null; };
  const stream = async (name) => { const f = entry(name); return f ? zip.stream(f) : null; };
  const text = (name) => got.get(name) ?? got.set(name, stream(name).then((s) => (s ? new Response(s).text() : ''))).get(name);
  const styles = text('xl/styles.xml'), strings = stream('xl/sharedStrings.xml');
  styles.catch(() => {}); // a failure is thrown where it's awaited, below
  strings.catch(() => {});
  const [wbx, rels] = await Promise.all([text('xl/workbook.xml'), text('xl/_rels/workbook.xml.rels')]);
  const sheets = [...wbx.matchAll(/<sheet\b[^>]*>/g)].map((m) => ({ name: unxml(attr(m[0], 'name') || ''), rid: attr(m[0], 'r:id') }));
  const one = sheets.find((x) => x.name === want) || sheets[0];
  const rel = [...rels.matchAll(/<Relationship\b[^>]*>/g)].map((m) => m[0]).find((r) => attr(r, 'Id') === one?.rid);
  const target = rel && attr(rel, 'Target');
  if (!target) throw new Error('no first sheet');
  const sheetName = target.startsWith('/') ? target : 'xl/' + target.replace(/^\.\//, '');
  const sheet = stream(sheetName); // under way while the shared strings arrive
  sheet.catch(() => {});
  const sty = await styles;
  // The shared strings are read only as far as the rows shown need: Excel lists them in the order they're first used,
  // so 3,000 rows want the first few thousand of a book's 100k+ (and a few hundred KB of its megabytes).
  const sstIn = (await strings)?.pipeThrough(new TextDecoderStream()).getReader();
  let sst = '', sstEnd = !sstIn;
  const at = []; // where each shared string starts
  const upTo = async (k) => { // read on until string k is whole, or the strings end
    while (!sstEnd && !(at.length > k + 1 || at.length === k + 1 && sst.indexOf('</si>', at[k]) >= 0)) {
      const { value, done } = await sstIn.read();
      if (done) { sstEnd = true; break; }
      const from = Math.max(0, sst.length - 3);
      sst += value;
      for (let i = sst.indexOf('<si>', from); i >= 0; i = sst.indexOf('<si>', i + 4)) at.push(i + 4);
    }
  };
  const memo = [];
  const str = (k) => memo[k] ??= at[k] === undefined ? '' : texts(sst.slice(at[k], sst.indexOf('</si>', at[k])));
  const later = []; // [cells, column, string]: shared-string cells, filled in once their strings are in
  const fill = async () => {
    await upTo(later.reduce((m, x) => Math.max(m, x[2]), -1));
    for (const [cells, col, k] of later) cells[col][0] = str(k);
    later.length = 0;
  };
  const fmts = Object.fromEntries([...sty.matchAll(/<numFmt\b[^>]*>/g)].map((m) => [attr(m[0], 'numFmtId'), unxml(attr(m[0], 'formatCode') || 'General')]));
  const xfs = [...(sty.match(/<cellXfs\b[\s\S]*?<\/cellXfs>/)?.[0] || '').matchAll(/<xf\b[^>]*>/g)].map((m) => +(attr(m[0], 'numFmtId') || 0));
  const d1904 = /date1904="(1|true)"/.test(wbx);
  const seen = new Map(); // formatted before: a column's values repeat (rents, statuses), and SSF parses its format each call
  const shown = (v, s) => { // as SheetJS shows it: a format it can't read falls back to General, as there
    const id = xfs[+s || 0] || 0, key = id + '|' + v;
    let out = seen.get(key);
    if (out !== undefined) return out;
    const x = v instanceof Date ? v : +v;
    try { out = XLSX.SSF.format(id ? fmts[id] ?? XLSX.SSF.get_table()[id] ?? 'General' : 'General', x, { date1904: d1904 }); } catch { out = XLSX.SSF.format('General', x); }
    if (seen.size < 50000) seen.set(key, out);
    return out;
  };
  const reader = (await sheet).pipeThrough(new TextDecoderStream()).getReader();
  const rows = [], arrays = []; // arrays: where array formulas are (their every cell is part of them)
  let buf2 = '', dim = null, maxc = 0;
  for (;;) {
    const { value, done } = await reader.read();
    if (value) buf2 += value;
    if (!dim) dim = buf2.match(/<dimension ref="([^"]+)"/)?.[1];
    let end;
    while (rows.length < n && (end = buf2.indexOf('</row>')) >= 0) { // each whole row in what's arrived
      const row = buf2.slice(buf2.indexOf('<row'), end), cells = [];
      const at = +(attr(row.slice(0, row.indexOf('>')), 'r') || rows.length + 1) - 1; // Excel leaves empty rows out
      if (at >= n) { rows.length = n; break; }
      while (rows.length < at) rows.push([]);
      for (const c of row.matchAll(/<c\b([^>]*?)(?:\/>|>([\s\S]*?)<\/c>)/g)) {
        const a = c[1], body = c[2] || '', r = attr(' ' + a, 'r'), t = attr(' ' + a, 't'), v = body.match(/<v>([^<]*)<\/v>/)?.[1];
        const col = r ? XLSX.utils.decode_col(r.replace(/\d+$/, '')) : cells.length;
        if (col >= 200) continue;
        const f = body.includes('<f');
        if (f && body.includes('t="array"')) arrays.push(attr(body.slice(body.indexOf('<f'), body.indexOf('>', body.indexOf('<f')) + 1), 'ref') || r);
        if (t === 's') later.push([cells, col, +v]);
        const txt = t === 's' ? '' : t === 'inlineStr' ? texts(body) : t === 'b' ? (v === '1' ? 'TRUE' : 'FALSE') : t === 'str' || t === 'e' ? unxml(v ?? '')
          : v === undefined ? '' : shown(t === 'd' ? new Date(v) : v, attr(' ' + a, 's')); // d: an ISO date, as some writers store them
        cells[col] = [txt, f ? 2 : !t || t === 'n' ? (v === undefined ? 0 : 1) : 0];
        maxc = Math.max(maxc, col + 1);
      }
      rows[at] = cells;
      buf2 = buf2.slice(end + 6);
    }
    if (rows.length >= n || done) { reader.cancel().catch(() => {}); break; }
    if (early && rows.length >= 200) { await fill(); early(packed()); early = null; }
  }
  await fill();
  sstIn?.cancel().catch(() => {});
  return packed();
  function packed() {
    const all = dim ? XLSX.utils.decode_range(dim.includes(':') ? dim : dim + ':' + dim) : null;
    const cols = Math.min(Math.max(maxc, 1), 200), shownText = new Array(rows.length * cols).fill(''), kind = new Uint8Array(rows.length * cols);
    rows.forEach((cells, r) => cells.forEach((x, c) => { if (x) { shownText[r * cols + c] = x[0]; kind[r * cols + c] = x[1]; } }));
    for (const ref of arrays) {
      const g = XLSX.utils.decode_range(ref.includes(':') ? ref : ref + ':' + ref);
      for (let r = g.s.r; r <= Math.min(g.e.r, rows.length - 1); r++) for (let c = g.s.c; c <= Math.min(g.e.c, cols - 1); c++) kind[r * cols + c] = 3;
    }
    return { names: sheets.map((x) => x.name), mtime: zip.mtime, sheet: { name: one.name, rows: rows.length, cols, allCols: all ? all.e.c + 1 : cols, allRows: all ? all.e.r + 1 : rows.length, text: shownText, kind } };
  }
}

onmessage = async ({ data: m }) => {
  try {
    let out, move = [];
    if (m.op === 'close') { books.delete(m.book); const r = reads.get(m.book); r?.ac.abort(); r?.open?.(); return; }
    if (m.op === 'shown') { // its tab in front (on) or behind another: a whole-file download goes on or waits
      const r = reads.get(m.book);
      if (r && m.on) { r.open?.(); r.open = null; r.gate = Promise.resolve(); }
      else if (r && !r.open) { r.gate = new Promise((y) => (r.open = y)); r.pause?.(); }
      return;
    }
    if (m.op === 'warm') { if (typeof XLSX === 'undefined') importScripts(m.lib); return; } // started ahead, while idle
    const rd = m.op === 'read' ? null : reads.get(m.book);
    if (rd) { // the whole book isn't in yet
      if (m.op === 'set') { rd.edits.push(m); postMessage({ id: m.id }); return; }
      if (m.op === 'cell') { postMessage({ id: m.id, cell: null }); return; }
      const first = m.op === 'sheet' && rd.url ? await fastFirstRows(rd.url, 3000, m.name, rd.seen).catch(() => null) : null;
      if (first) { postMessage({ id: m.id, sheet: { ...first.sheet, partial: true } }, [first.sheet.kind.buffer]); return; }
      await rd.done;
    }
    wb = books.get(m.book) || null;
    if (m.op === 'read') {
      if (typeof XLSX === 'undefined') importScripts(m.lib); // SheetJS, from the panel itself (versioned like every asset)
      let first = null, mtime = 0, ok;
      const r0 = { ac: new AbortController(), url: m.url, seen: new Map(), edits: [], done: new Promise((y) => (ok = y)), gate: Promise.resolve() }, ac = r0.ac;
      reads.set(m.book, r0);
      try {
        const show = (f) => postMessage({ id: m.id, partial: true, ...f }, [f.sheet.kind.buffer]);
        if (m.url && m.preview) first = await fastFirstRows(m.url, m.preview, null, r0.seen, show).catch(() => null); // by ranges: no waiting for the whole file
        if (first) show(first);
        if (m.url) { // then the whole of it, for every row, every sheet and editing
          ({ buf: m.src, mtime } = await fetchWhole(m.url, r0)); // at low priority: behind what's opened next
        }
        const src = typeof m.src === 'string' ? m.src : new Uint8Array(m.src);
        if (m.preview && !first) { // a big file: its first rows on screen at once, the rest while you look at them
          if (typeof m.src !== 'string') first = await fastFirstRows(m.src, m.preview).catch(() => null);
          if (!first) { wb = XLSX.read(src, { ...m.opts, sheetRows: m.preview }); first = { names: wb.SheetNames, sheet: pack(wb.SheetNames[0]) }; }
          postMessage({ id: m.id, partial: true, ...first }, [first.sheet.kind.buffer]);
        }
        ac.signal.throwIfAborted();
        wb = XLSX.read(src, m.opts);
        for (const e of r0.edits) set(e.name, e.r, e.c, e.raw); // made while it loaded
        books.set(m.book, wb);
        out = { names: wb.SheetNames, sheet: pack(wb.SheetNames[0]), mtime };
        move = [out.sheet.kind.buffer];
      } finally {
        if (reads.get(m.book) === r0) reads.delete(m.book);
        ok();
      }
    } else if (m.op === 'sheet') { out = { sheet: pack(m.name) }; move = [out.sheet.kind.buffer]; }
    else if (m.op === 'cell') out = { cell: cell(m.name, m.r, m.c) };
    else if (m.op === 'formulas') { // every formula in a range, each cell's own (SheetJS writes a shared one out per cell)
      const rg = XLSX.utils.decode_range(m.ref), data = wb.Sheets[m.name]?.['!data'] || [], cells = [];
      for (let r = rg.s.r; r <= rg.e.r; r++) for (let c = rg.s.c; c <= rg.e.c; c++) if (data[r]?.[c]?.f) cells.push({ sheet: m.name, r, c, raw: '=' + data[r][c].f });
      out = { cells };
    }
    else if (m.op === 'set') out = set(m.name, m.r, m.c, m.raw);
    else if (m.op === 'write') {
      out = { body: m.csv ? XLSX.utils.sheet_to_csv(wb.Sheets[wb.SheetNames[0]], { FS: m.fs, RS: '\n', blankrows: true }).replace(/\n*$/, '\n') : XLSX.write(wb, { bookType: m.bookType, type: 'array', compression: true }) };
      if (out.body instanceof ArrayBuffer) move = [out.body];
    }
    postMessage({ id: m.id, ...out }, move);
  } catch (err) {
    postMessage({ id: m.id, error: String(err?.message || err) });
  }
};
