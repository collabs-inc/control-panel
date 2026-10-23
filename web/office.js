'use strict';

// Office files the browser has no viewer for, loaded by files.js the first time one opens:
//  .pptx drawn from its own XML (text, pictures, shapes, tables, the theme's colours and fonts, placeholders
//  inherited from the layout and master), and the old binary .doc and .ppt read for their text (OLE containers,
//  opened with SheetJS's CFB). Everything is built with DOM calls and textContent: a document's text is never HTML.
// ponytail: no charts, SmartArt, gradients or animations in .pptx; .doc and .ppt are text only.
window.OFFICE = (() => {
  const PX = 9525; // EMU per CSS pixel (914400 per inch, 96 px per inch)
  const el = (tag, cls, css) => { const e = document.createElement(tag); if (cls) e.className = cls; if (css) Object.assign(e.style, css); return e; };
  const kids = (n, name) => (n ? [...n.children].filter((c) => c.localName === name) : []);
  const kid = (n, name) => kids(n, name)[0] || null;
  const path = (n, ...names) => names.reduce((x, k) => kid(x, k), n);
  const num = (v, d = 0) => (v == null || v === '' ? d : +v);
  const hex = (v) => (/^[0-9a-f]{6}$/i.test(v || '') ? '#' + v : null);

  // ---------- .pptx ----------
  async function pptx(buf, box) {
    const zip = await JSZip.loadAsync(buf);
    const xml = async (p) => { const f = zip.file(p); return f ? new DOMParser().parseFromString(await f.async('text'), 'application/xml') : null; };
    const rels = async (p) => { // part → { rId: absolute target }
      const d = await xml(p.replace(/([^/]+)$/, '_rels/$1.rels')), out = {};
      for (const r of d ? d.getElementsByTagName('Relationship') : []) {
        const t = r.getAttribute('Target');
        if (r.getAttribute('TargetMode') === 'External') continue; // a link out, never a part of this file
        out[r.getAttribute('Id')] = t.startsWith('/') ? t.slice(1) : decodeURIComponent(new URL(t, 'http://x/' + p).pathname.slice(1));
      }
      return out;
    };
    const pres = await xml('ppt/presentation.xml');
    const sz = pres.getElementsByTagName('p:sldSz')[0];
    const W = num(sz?.getAttribute('cx'), 12192000) / PX, H = num(sz?.getAttribute('cy'), 6858000) / PX;
    const prel = await rels('ppt/presentation.xml');
    const order = [...pres.getElementsByTagName('p:sldId')].map((s) => prel[s.getAttribute('r:id')]).filter(Boolean);
    const urls = [];
    const media = async (p) => { const f = zip.file(p); if (!f || !/\.(png|jpe?g|gif|svg|bmp|webp)$/i.test(p)) return null; const u = URL.createObjectURL(new Blob([await f.async('uint8array')], { type: /svg$/i.test(p) ? 'image/svg+xml' : '' })); urls.push(u); return u; };
    const cache = new Map();
    const part = async (p) => { if (!cache.has(p)) cache.set(p, Promise.all([xml(p), rels(p)])); return cache.get(p); };

    box.textContent = '';
    for (const [n, sp] of order.entries()) {
      const [slide, srel] = await part(sp);
      const lp = Object.values(srel).find((t) => /slideLayouts\//.test(t));
      const [layout, lrel] = lp ? await part(lp) : [null, {}];
      const mp = Object.values(lrel).find((t) => /slideMasters\//.test(t));
      const [master, mrel] = mp ? await part(mp) : [null, {}];
      const tp = Object.values(mrel).find((t) => /theme\//.test(t));
      const [theme] = tp ? await part(tp) : [null];
      const scheme = {};
      for (const c of theme?.getElementsByTagName('a:clrScheme')[0]?.children || []) {
        const v = c.firstElementChild;
        scheme[c.localName] = hex(v?.getAttribute('val') || v?.getAttribute('lastClr'));
      }
      Object.assign(scheme, { tx1: scheme.dk1, bg1: scheme.lt1, tx2: scheme.dk2, bg2: scheme.lt2 });
      const font = (k) => theme?.getElementsByTagName(`a:${k}Font`)[0]?.getElementsByTagName('a:latin')[0]?.getAttribute('typeface');
      const fonts = { major: font('major'), minor: font('minor') };
      const color = (fill) => { // a:solidFill (or anything holding a colour) → CSS colour
        if (!fill) return null;
        const c = fill.localName.endsWith('Clr') ? fill : fill.firstElementChild;
        if (!c) return null;
        let v = c.localName === 'schemeClr' ? scheme[c.getAttribute('val')] : hex(c.getAttribute('val') || c.getAttribute('lastClr'));
        const a = kid(c, 'alpha');
        if (v && a) v += Math.round((num(a.getAttribute('val'), 100000) / 100000) * 255).toString(16).padStart(2, '0');
        return v;
      };
      // placeholders: a slide's shape often has no size or style of its own, it inherits its layout's, then master's
      const phs = (doc) => [...(doc?.getElementsByTagName('p:sp') || [])].map((s) => [s.getElementsByTagName('p:ph')[0], s]).filter(([p]) => p);
      const phOf = (ph) => {
        const type = ph.getAttribute('type') || 'body', idx = ph.getAttribute('idx');
        const find = (doc) => { const all = phs(doc); return (idx && all.find(([p]) => p.getAttribute('idx') === idx)) || all.find(([p]) => (p.getAttribute('type') || 'body') === type) || null; };
        return [find(layout)?.[1], find(master)?.[1], type];
      };
      const txStyle = (type) => master?.getElementsByTagName(/title|ctrTitle/.test(type) ? 'p:titleStyle' : /body|subTitle|obj/.test(type) ? 'p:bodyStyle' : 'p:otherStyle')[0];

      const page = el('div', 'of-slide', { width: W + 'px', height: H + 'px' });
      const bg = (doc) => path(doc?.getElementsByTagName('p:cSld')[0], 'bg', 'bgPr');
      const bgPr = bg(slide) || bg(layout) || bg(master);
      const bgRef = slide.getElementsByTagName('p:bgRef')[0] || layout?.getElementsByTagName('p:bgRef')[0] || master?.getElementsByTagName('p:bgRef')[0];
      page.style.background = color(kid(bgPr, 'solidFill')) || color(bgRef) || '#fff';
      const bgBlip = kid(bgPr, 'blipFill') && kid(kid(bgPr, 'blipFill'), 'blip');
      if (bgBlip) { const u = await media((bg(slide) ? srel : bg(layout) ? lrel : mrel)[bgBlip.getAttribute('r:embed')]); if (u) Object.assign(page.style, { backgroundImage: `url("${u}")`, backgroundSize: 'cover' }); }

      const xfrm = (spPr) => { const x = kid(spPr, 'xfrm'); const o = kid(x, 'off'), e = kid(x, 'ext'); return o && e ? { x: num(o.getAttribute('x')), y: num(o.getAttribute('y')), w: num(e.getAttribute('cx')), h: num(e.getAttribute('cy')), rot: num(x.getAttribute('rot')) / 60000, flipH: x.getAttribute('flipH') === '1' } : null; };
      const place = (d, b, g) => { // b in EMU, mapped through the groups it sits in
        const x = g ? g.ox + (b.x - g.cx) * g.sx : b.x, y = g ? g.oy + (b.y - g.cy) * g.sy : b.y;
        Object.assign(d.style, { left: x / PX + 'px', top: y / PX + 'px', width: (b.w * (g ? g.sx : 1)) / PX + 'px', height: (b.h * (g ? g.sy : 1)) / PX + 'px' });
        if (b.rot || b.flipH) d.style.transform = `rotate(${b.rot}deg)${b.flipH ? ' scaleX(-1)' : ''}`;
      };
      const text = (body, inh, type) => { // a:txBody → paragraphs and runs
        const wrap = el('div', 'of-tx');
        const bp = kid(body, 'bodyPr');
        const anchor = bp?.getAttribute('anchor') || inh.map((s) => s?.getElementsByTagName('a:bodyPr')[0]?.getAttribute('anchor')).find(Boolean) || (/title/i.test(type) ? 'ctr' : 't');
        wrap.style.justifyContent = { t: 'flex-start', ctr: 'center', b: 'flex-end' }[anchor] || 'flex-start';
        const ins = (k, d) => num(bp?.getAttribute(k), d) / PX + 'px';
        wrap.style.padding = `${ins('tIns', 45720)} ${ins('rIns', 91440)} ${ins('bIns', 45720)} ${ins('lIns', 91440)}`;
        const defSz = (lvl) => { // the size this text would have if its runs didn't say: shape, layout, master, master's text styles
          const k = `a:lvl${lvl + 1}pPr`;
          for (const s of [body, ...inh, txStyle(type)]) { const v = s?.getElementsByTagName(k)[0]?.getElementsByTagName('a:defRPr')[0]?.getAttribute('sz'); if (v) return num(v) / 100; }
          return /title/i.test(type) ? 32 : 18;
        };
        const norm = bp && kid(bp, 'normAutofit'), scale = norm ? num(norm.getAttribute('fontScale'), 100000) / 100000 : 1;
        for (const p of kids(body, 'p')) {
          const pPr = kid(p, 'pPr'), lvl = num(pPr?.getAttribute('lvl'));
          const para = el('p', 'of-p', { textAlign: { ctr: 'center', r: 'right', just: 'justify' }[pPr?.getAttribute('algn')] || (type === 'ctrTitle' ? 'center' : 'left') });
          if (lvl) para.style.marginLeft = lvl * 24 + 'px';
          const bu = kid(pPr, 'buChar')?.getAttribute('char') || (!kid(pPr, 'buNone') && /^(body|obj)$/.test(type) && kids(p, 'r').length ? '•' : '');
          if (bu && !kid(pPr, 'buNone')) { const b = el('span', 'of-bu'); b.textContent = bu; para.append(b); }
          let any = false;
          for (const r of p.children) {
            if (r.localName === 'br') { para.append(el('br')); continue; }
            if (r.localName !== 'r' && r.localName !== 'fld') continue;
            const rPr = kid(r, 'rPr'), t = kid(r, 't')?.textContent ?? '';
            const s = el('span', '', { fontSize: (num(rPr?.getAttribute('sz')) / 100 || defSz(lvl)) * scale + 'pt' });
            if (rPr?.getAttribute('b') === '1') s.style.fontWeight = '700';
            if (rPr?.getAttribute('i') === '1') s.style.fontStyle = 'italic';
            if (rPr?.getAttribute('u') && rPr.getAttribute('u') !== 'none') s.style.textDecoration = 'underline';
            const c = color(kid(rPr, 'solidFill'));
            if (c) s.style.color = c;
            const tf = kid(rPr, 'latin')?.getAttribute('typeface');
            const face = tf === '+mj-lt' ? fonts.major : tf === '+mn-lt' ? fonts.minor : tf;
            if (face) s.style.fontFamily = `"${face.replace(/["\\]/g, '')}", system-ui, sans-serif`;
            s.textContent = t;
            para.append(s);
            any = true;
          }
          if (!any) { const z = el('span', '', { fontSize: defSz(lvl) * scale + 'pt' }); z.textContent = '\u200b'; para.append(z); } // an empty line keeps its height
          wrap.append(para);
        }
        const fontKind = /title/i.test(type) ? fonts.major : fonts.minor;
        if (fontKind) wrap.style.fontFamily = `"${fontKind.replace(/["\\]/g, '')}", system-ui, sans-serif`;
        return wrap;
      };
      const tree = async (node, g, relmap) => {
        for (const n of node.children) {
          const name = n.localName;
          if (name === 'grpSp') {
            const x = kid(kid(n, 'grpSpPr'), 'xfrm');
            const o = kid(x, 'off'), e = kid(x, 'ext'), co = kid(x, 'chOff'), ce = kid(x, 'chExt');
            if (!o || !e || !co || !ce) { await tree(n, g, relmap); continue; }
            const box2 = { x: num(o.getAttribute('x')), y: num(o.getAttribute('y')), w: num(e.getAttribute('cx')), h: num(e.getAttribute('cy')) };
            const ox = g ? g.ox + (box2.x - g.cx) * g.sx : box2.x, oy = g ? g.oy + (box2.y - g.cy) * g.sy : box2.y;
            const sx = (box2.w / Math.max(1, num(ce.getAttribute('cx')))) * (g ? g.sx : 1), sy = (box2.h / Math.max(1, num(ce.getAttribute('cy')))) * (g ? g.sy : 1);
            await tree(n, { ox, oy, cx: num(co.getAttribute('x')), cy: num(co.getAttribute('y')), sx, sy }, relmap);
          } else if (name === 'sp') {
            const spPr = kid(n, 'spPr'), ph = n.getElementsByTagName('p:ph')[0];
            const [lsp, msp, type] = ph ? phOf(ph) : [null, null, 'shape'];
            if (ph && /^(dt|ftr|sldNum)$/.test(type) && !n.getElementsByTagName('a:t').length) continue; // an empty date / footer / number
            const b = xfrm(spPr) || xfrm(kid(lsp, 'spPr')) || xfrm(kid(msp, 'spPr'));
            if (!b) continue;
            const d = el('div', 'of-sp');
            place(d, b, g);
            const fr = path(n, 'style', 'fillRef'); // a theme fill; idx 0 is none
            const fill = color(kid(spPr, 'solidFill')) || (!ph && fr && fr.getAttribute('idx') !== '0' && color(fr));
            if (fill && !kid(spPr, 'noFill')) d.style.background = fill;
            const ln = kid(spPr, 'ln');
            if (ln && !kid(ln, 'noFill')) { const lc = color(kid(ln, 'solidFill')); if (lc) d.style.border = `${Math.max(1, num(ln.getAttribute('w'), 12700) / PX)}px solid ${lc}`; }
            const geom = kid(spPr, 'prstGeom')?.getAttribute('prst');
            if (geom === 'ellipse') d.style.borderRadius = '50%';
            else if (geom === 'roundRect') d.style.borderRadius = '10%';
            const body = kid(n, 'txBody');
            if (body) d.append(text(body, [kid(lsp, 'txBody'), kid(msp, 'txBody')], type));
            page.append(d);
          } else if (name === 'pic') {
            const b = xfrm(kid(n, 'spPr'));
            const blip = n.getElementsByTagName('a:blip')[0];
            if (!b || !blip) continue;
            const u = await media(relmap[blip.getAttribute('r:embed')]);
            const d = el(u ? 'img' : 'div', 'of-pic');
            if (u) { d.src = u; d.alt = ''; } else d.textContent = 'picture';
            place(d, b, g);
            page.append(d);
          } else if (name === 'graphicFrame') {
            const x = kid(n, 'xfrm'), o = kid(x, 'off'), e = kid(x, 'ext');
            if (!o || !e) continue;
            const d = el('div', 'of-sp');
            place(d, { x: num(o.getAttribute('x')), y: num(o.getAttribute('y')), w: num(e.getAttribute('cx')), h: num(e.getAttribute('cy')) }, g);
            const tbl = n.getElementsByTagName('a:tbl')[0];
            if (tbl) {
              const t = el('table', 'of-tbl');
              for (const tr of kids(tbl, 'tr')) {
                const row = t.insertRow();
                row.style.height = num(tr.getAttribute('h')) / PX + 'px';
                for (const tc of kids(tr, 'tc')) {
                  if (tc.getAttribute('hMerge') === '1' || tc.getAttribute('vMerge') === '1') continue;
                  const cell = row.insertCell();
                  if (tc.getAttribute('gridSpan')) cell.colSpan = +tc.getAttribute('gridSpan');
                  if (tc.getAttribute('rowSpan')) cell.rowSpan = +tc.getAttribute('rowSpan');
                  const cf = color(kid(kid(tc, 'tcPr'), 'solidFill'));
                  if (cf) cell.style.background = cf;
                  const body = kid(tc, 'txBody');
                  if (body) cell.append(text(body, [], 'tbl'));
                }
              }
              d.append(t);
            } else { d.classList.add('of-chart'); d.textContent = 'Chart or diagram: open it in PowerPoint to see it'; }
            page.append(d);
          }
        }
      };
      // the layout's and master's own drawings (logos, bands) sit under the slide's, unless it hides them
      if (slide.getElementsByTagName('p:cSld')[0]?.parentNode.getAttribute('showMasterSp') !== '0') {
        for (const [doc, rm] of [[master, mrel], [layout, lrel]]) {
          const st = doc?.getElementsByTagName('p:spTree')[0];
          if (!st) continue;
          const shadow = st.cloneNode(true); // only the pictures and plain shapes: placeholders belong to the slide
          for (const s of [...shadow.children]) if (s.getElementsByTagName('p:ph').length) s.remove();
          await tree(shadow, null, rm);
        }
      }
      await tree(slide.getElementsByTagName('p:spTree')[0], null, srel);
      const frame = el('div', 'of-frame');
      frame.dataset.n = n + 1;
      frame.append(page);
      box.append(frame);
    }
    const fit = () => { for (const f of box.children) { const k = f.clientWidth / W; f.style.height = H * k + 'px'; f.firstChild.style.transform = `scale(${k})`; } };
    const ro = new ResizeObserver(fit);
    ro.observe(box);
    fit();
    return { slides: order.length, destroy: () => { ro.disconnect(); for (const u of urls) URL.revokeObjectURL(u); } };
  }

  // ---------- old binary Office (OLE): text only ----------
  const stream = (buf, name) => {
    const cfb = XLSX.CFB.read(new Uint8Array(buf), { type: 'array' });
    const e = XLSX.CFB.find(cfb, name);
    return e?.content ? Uint8Array.from(e.content) : null;
  };
  const CP1252 = { 0x80: '€', 0x82: '‚', 0x84: '„', 0x85: '…', 0x91: '‘', 0x92: '’', 0x93: '“', 0x94: '”', 0x95: '•', 0x96: '–', 0x97: '—', 0x99: '™' };
  const latin = (b) => { let s = ''; for (const c of b) s += CP1252[c] || String.fromCharCode(c); return s; };
  const utf16 = (b) => new TextDecoder('utf-16le').decode(b);

  // .doc: the piece table (Clx) says where each run of the main text lives and whether it's 8- or 16-bit
  function doc(buf) {
    const wd = stream(buf, 'WordDocument');
    if (!wd) throw new Error('Not a Word 97–2003 document');
    const v = new DataView(wd.buffer, wd.byteOffset, wd.byteLength);
    if (v.getUint16(0, true) !== 0xa5ec) throw new Error('Not a Word 97–2003 document');
    const tbl = stream(buf, v.getUint16(0x0a, true) & 0x0200 ? '1Table' : '0Table');
    if (!tbl) throw new Error('The document has no text table');
    const t = new DataView(tbl.buffer, tbl.byteOffset, tbl.byteLength);
    const ccpText = v.getInt32(0x4c, true), fcClx = v.getInt32(0x1a2, true);
    let p = fcClx;
    while (tbl[p] === 0x01) p += 3 + t.getUint16(p + 1, true); // property runs before the piece table
    if (tbl[p] !== 0x02) throw new Error("Can't find this document's text");
    const lcb = t.getUint32(p + 1, true);
    p += 5;
    const n = (lcb - 4) / 12, cps = [], out = [];
    for (let i = 0; i <= n; i++) cps.push(t.getUint32(p + i * 4, true));
    for (let i = 0; i < n && cps[i] < ccpText; i++) {
      const raw = t.getUint32(p + (n + 1) * 4 + i * 8 + 2, true), len = Math.min(cps[i + 1], ccpText) - cps[i];
      const packed = raw & 0x40000000, fc = raw & 0x3fffffff;
      out.push(packed ? latin(wd.subarray(fc >> 1, (fc >> 1) + len)) : utf16(wd.subarray(fc, fc + len * 2)));
    }
    // fields: keep what they show (after 0x14), drop their codes (0x13…0x14); cells (0x07) become tabs
    const s = out.join('').replace(/\x13[^\x13\x14\x15]*\x14([^\x15]*)\x15/g, '$1').replace(/\x13[^\x15]*\x15/g, '')
      .replace(/[\x13\x14\x15\x01\x08]/g, '').replace(/\x07\x07/g, '\r').replace(/\x07/g, '\t').replace(/[\x0b\x0c]/g, '\n');
    return s.split('\r');
  }

  // .ppt: the records of the PowerPoint Document stream, in order; a slide starts at each SlidePersistAtom of the
  // slide list (or, failing that, at each Slide container), and its text atoms follow
  function ppt(buf) {
    const d = stream(buf, 'PowerPoint Document');
    if (!d) throw new Error('Not a PowerPoint 97–2003 file');
    const v = new DataView(d.buffer, d.byteOffset, d.byteLength);
    const listed = [], drawn = [];
    let inList = -1, inSlide = -1; // where the slides' text list, and the current Slide container, end
    for (let p = 0; p + 8 <= d.length;) {
      const vi = v.getUint16(p, true), type = v.getUint16(p + 2, true), len = v.getUint32(p + 4, true);
      if (inList >= 0 && p >= inList) inList = -1;
      if ((vi & 0xf) === 0xf) { // a container: step into it
        if (type === 0x0ff0 && vi >> 4 === 0) inList = p + 8 + len; // the slides' text list
        if (type === 0x03ee) { drawn.push([]); inSlide = p + 8 + len; } // a Slide (not a master or a notes page)
        p += 8;
        continue;
      }
      if (type === 0x03f3 && inList >= 0) listed.push([]);
      if (type === 0x0fa0 || type === 0x0fa8) {
        const s = (type === 0x0fa0 ? utf16 : latin)(d.subarray(p + 8, p + 8 + len)).replace(/\r/g, '\n').replace(/\x0b/g, '\n');
        if (s.trim() === '*') { p += 8 + len; continue; } // a slide number's stand-in
        if (inList >= 0 && listed.length) listed[listed.length - 1].push(s);
        else if (p < inSlide) drawn[drawn.length - 1].push(s);
      }
      p += 8 + len;
    }
    const slides = listed.some((x) => x.length) ? listed : drawn;
    return slides.filter((x) => x.length);
  }

  return { pptx, doc, ppt };
})();
