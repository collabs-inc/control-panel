// node --test sheet_test.js: the fast first-rows reader (web/sheet-worker.js) shows a workbook's cells exactly as
// SheetJS does once the whole book is read: shared and inline strings, formats, dates, empty rows Excel leaves out.
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const http = require('node:http');

const zlib = require('node:zlib');

// node's own DecompressionStream (an adapter over zlib) throws when output it already made arrives after a cancel,
// which the reader does once it has its rows; browsers' don't. This one drops it.
globalThis.DecompressionStream = class {
  constructor() {
    const z = zlib.createInflateRaw(), quiet = (f) => { try { f(); } catch {} };
    this.readable = new ReadableStream({
      start(c) { z.on('data', (b) => quiet(() => c.enqueue(new Uint8Array(b)))).on('end', () => quiet(() => c.close())).on('error', (e) => quiet(() => c.error(e))); },
      cancel() { z.destroy(); },
    });
    this.writable = new WritableStream({ write: (b) => new Promise((ok) => z.write(b, ok)), close: () => z.end(), abort: () => z.destroy() });
  }
};
globalThis.XLSX = require('./web/xlsx.full.min.js');
globalThis.postMessage = () => {};
globalThis.importScripts = () => {};
globalThis.onmessage = null;
const src = fs.readFileSync(__dirname + '/web/sheet-worker.js', 'utf8').replace('onmessage = async', 'globalThis.onmessage = async');
new Function(src + '; globalThis.fastFirstRows = fastFirstRows; globalThis.fetchWhole = fetchWhole; globalThis.packOf = (b) => { wb = b; return pack(b.SheetNames[0]); };')();

function book() {
  const ws = XLSX.utils.aoa_to_sheet([
    ['Property', 'Rent', 'Due', 'Share', 'Note'],
    ['Baron Court', 1400, new Date(Date.UTC(2026, 5, 10)), 0.4, 'part paid & <late>'],
    [],
    ['Midblock 12', 1550, new Date(Date.UTC(2027, 7, 13)), 1, 'ok'],
  ], { cellDates: true });
  ws.B2.z = '#,##0.00';
  ws.B4.z = '#,##0.00';
  ws.C2.z = 'd mmm yyyy';
  ws.C4.z = 'd mmm yyyy';
  ws.D2.z = '0%';
  ws.D4.z = '0%';
  ws.E6 = { t: 'n', f: 'B2+B4', v: 2950 };
  ws.F2 = { t: 'n', f: 'B2:B3*2', F: 'F2:F3', v: 2800 }; // an array formula over two cells
  ws.F3 = { t: 'n', F: 'F2:F3', v: 0 };
  ws['!ref'] = 'A1:F6';
  const wb = XLSX.utils.book_new();
  XLSX.utils.book_append_sheet(wb, ws, 'Rent roll');
  return wb;
}

for (const opts of [{ bookSST: true }, { bookSST: false }]) {
  test(`fast first rows match SheetJS (${opts.bookSST ? 'shared' : 'inline'} strings)`, async () => {
    const out = XLSX.write(book(), { type: 'buffer', bookType: 'xlsx', compression: true, cellDates: true, ...opts });
    const ab = out.buffer.slice(out.byteOffset, out.byteOffset + out.length);
    const fast = await fastFirstRows(ab, 100);
    const full = packOf(XLSX.read(out, { type: 'buffer', dense: true, cellFormula: true, cellDates: true, cellNF: true, cellStyles: true, cellHTML: false }));
    assert.equal(fast.sheet.allRows, 6);
    assert.equal(fast.sheet.rows, full.rows);
    assert.equal(full.kind[1 * full.cols + 5], 3, 'F2: in an array formula');
    assert.equal(full.kind[2 * full.cols + 5], 3, 'F3 too');
    for (let r = 0; r < full.rows; r++) {
      for (let c = 0; c < full.cols; c++) {
        assert.equal(fast.sheet.text[r * fast.sheet.cols + c] ?? '', full.text[r * full.cols + c], `row ${r + 1}, column ${c + 1}`);
        assert.equal(fast.sheet.kind[r * fast.sheet.cols + c] ?? 0, full.kind[r * full.cols + c], `kind of row ${r + 1}, column ${c + 1}`);
      }
    }
  });
}

test('fast first rows read a workbook on the panel by ranges, without the whole file', async () => {
  const wb = book(), rows = [];
  for (let r = 0; r < 60000; r++) rows.push([`Unit ${r}`, Math.random(), Math.random(), Math.random(), Math.random()]);
  XLSX.utils.sheet_add_aoa(wb.Sheets['Rent roll'], rows, { origin: 'A7' }); // big enough to read by ranges
  const out = XLSX.write(wb, { type: 'buffer', bookType: 'xlsx', compression: true, cellDates: true, bookSST: true });
  let sent = 0;
  const srv = http.createServer((req, res) => { // what ServeContent does with the ranges asked for
    if (req.url !== '/book.xlsx') return res.writeHead(404).end(); // (this sandbox probes listening ports)
    const m = /bytes=(\d*)-(\d*)/.exec(req.headers.range);
    const from = m[1] === '' ? out.length - Math.min(+m[2], out.length) : +m[1], to = m[1] === '' || m[2] === '' ? out.length : Math.min(+m[2] + 1, out.length);
    res.writeHead(206, { 'Content-Range': `bytes ${from}-${to - 1}/${out.length}`, 'Content-Length': to - from });
    (async () => { // in chunks, paced like a link and not a loopback's megabytes of buffers, as long as the client reads
      for (let at = from; at < to && !res.destroyed; at += 16384) {
        await new Promise((ok) => setTimeout(ok, 1));
        sent += Math.min(16384, to - at);
        if (!res.write(out.subarray(at, Math.min(at + 16384, to)))) await new Promise((ok) => { res.once('drain', ok); res.once('close', ok); });
      }
      res.end();
    })();
  });
  await new Promise((ok) => srv.listen(0, '127.0.0.1', ok));
  try {
    const remote = await fastFirstRows(`http://127.0.0.1:${srv.address().port}/book.xlsx`, 100);
    const local = await fastFirstRows(out.buffer.slice(out.byteOffset, out.byteOffset + out.length), 100);
    assert.deepEqual(remote.sheet.text, local.sheet.text);
    assert.equal(remote.sheet.allRows, 60006);
    assert.ok(out.length > 1.5e6 && sent < out.length / 4, `${sent} of ${out.length} bytes fetched`);
  } finally {
    srv.close();
  }
});

test('a whole-file download stops while its tab is behind another and goes on from the byte it had', async () => {
  const file = Buffer.alloc(3 << 20, 7);
  for (let i = 0; i < file.length; i += 4096) file[i] = i % 251;
  let sent = 0;
  const ranges = [];
  const srv = http.createServer((req, res) => {
    if (req.url !== '/big.xlsx') return res.writeHead(404).end(); // (this sandbox probes listening ports)
    const from = +(/bytes=(\d+)-/.exec(req.headers.range || '')?.[1] || 0);
    ranges.push(from);
    res.writeHead(from ? 206 : 200, { 'X-Mtime': '42', 'Content-Length': file.length - from });
    (async () => { // slowly, as over a phone's link
      for (let at = from; at < file.length && !res.destroyed; at += 16384) {
        await new Promise((ok) => setTimeout(ok, 2));
        sent += Math.min(16384, file.length - at);
        if (!res.write(file.subarray(at, Math.min(at + 16384, file.length)))) await new Promise((ok) => { res.once('drain', ok); res.once('close', ok); });
      }
      res.end();
    })();
  });
  await new Promise((ok) => srv.listen(0, '127.0.0.1', ok));
  try {
    const r0 = { ac: new AbortController(), gate: Promise.resolve() };
    const whole = fetchWhole(`http://127.0.0.1:${srv.address().port}/big.xlsx`, r0);
    await new Promise((ok) => setTimeout(ok, 60));
    r0.gate = new Promise((y) => (r0.open = y)); // its tab goes behind another
    r0.pause();
    await new Promise((ok) => setTimeout(ok, 200)); // the server notices the request is gone
    const paused = sent;
    await new Promise((ok) => setTimeout(ok, 200));
    assert.equal(sent, paused, 'nothing more comes while it waits');
    r0.open();
    const { buf, mtime } = await whole;
    assert.ok(Buffer.from(buf).equals(file), 'the same bytes, whole');
    assert.equal(mtime, 42);
    assert.deepEqual(ranges.length, 2);
    assert.ok(ranges[1] > 0, 'went on from where it was');
  } finally {
    srv.close();
  }
});
