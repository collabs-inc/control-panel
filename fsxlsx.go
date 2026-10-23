package main

import (
	"archive/zip"
	"bytes"
	"cmp"
	"compress/flate"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"maps"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
)

// ---- a workbook's edited cells, saved into the file itself ----
//
// Saved through SheetJS, a big workbook had to be in the browser whole, was rewritten there (colours, fonts and charts
// dropped) and went back up: ~20 MB over a phone's link for one cell. This patches the edited cells' XML in place and
// copies every other part of the zip byte for byte: a few bytes go up, and nothing else in the workbook changes.

type fsCellEdit struct {
	Sheet string `json:"sheet"`
	R     int    `json:"r"`   // 0-based
	C     int    `json:"c"`   // 0-based
	Raw   string `json:"raw"` // as typed: a number, =a formula, text, or "" to clear it
}

var errFsXlsx = errors.New("can't patch this workbook in place") // the page then saves it through SheetJS

// fsSharedErr: an edit replaces a shared formula's first cell (which holds the formula the rest of its cells copy) but
// not all of them: the page writes them out one by one (Ref: where they are) and saves again.
type fsSharedErr struct {
	Sheet string `json:"sheet"`
	Ref   string `json:"ref"`
}

func (e *fsSharedErr) Error() string { return fmt.Sprintf("%s!%s is a shared formula", e.Sheet, e.Ref) }
func (e *fsSharedErr) Unwrap() error { return errFsXlsx }

func (s *server) fsXlsx(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := s.fsPath(q.Get("path"), true)
	if err != nil {
		fsFail(w, err)
		return
	}
	want, _ := strconv.ParseInt(q.Get("mtime"), 10, 64)
	var body struct {
		Edits []fsCellEdit `json:"edits"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body); err != nil || len(body.Edits) == 0 {
		writeJSON(w, 400, map[string]any{"error": "edits: [{sheet, r, c, raw}]"})
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		fsFail(w, err)
		return
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	err = fsPatchXlsx(p, tmp, body.Edits)
	if err == nil {
		err = tmp.Sync()
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	debug.FreeOSMemory() // a big sheet's XML, read and rewritten, goes back now
	if se := (*fsSharedErr)(nil); errors.As(err, &se) {
		writeJSON(w, 422, map[string]any{"error": err.Error(), "shared": se})
		return
	} else if errors.Is(err, errFsXlsx) {
		writeJSON(w, 422, map[string]any{"error": err.Error()})
		return
	} else if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	s.fsReplace(w, r, p, tmp.Name(), want, q.Get("force") == "1")
}

var (
	fsRelRe       = regexp.MustCompile(`<Relationship\b[^>]*>`)
	fsSheetRe     = regexp.MustCompile(`<sheet\b[^>]*>`)
	fsChainRelRe  = regexp.MustCompile(`<Relationship\b[^>]*calcChain[^>]*>`)
	fsChainTypeRe = regexp.MustCompile(`<Override\b[^>]*calcChain[^>]*>`)
	fsCalcPrRe    = regexp.MustCompile(`<calcPr\b[^>]*?(/?)>`)
	fsFullCalcRe  = regexp.MustCompile(`fullCalcOnLoad="[^"]*"`)
	fsDimRe       = regexp.MustCompile(`<dimension ref="([A-Z]+)(\d+)(?::([A-Z]+)(\d+))?"`)
	fsSpansRe     = regexp.MustCompile(`\sspans="[^"]*"`)
	fsNumRe       = regexp.MustCompile(`(?i)^[-+]?(\d+\.?\d*|\.\d+)(e[-+]?\d+)?$`)
)

// fsPatchXlsx writes src, its edits applied, to out as a zip.
func fsPatchXlsx(src string, out io.Writer, edits []fsCellEdit) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("%w: %v", errFsXlsx, err)
	}
	defer zr.Close()
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	read := func(name string) ([]byte, error) {
		f := files[name]
		if f == nil {
			return nil, fmt.Errorf("%w: no %s", errFsXlsx, name)
		}
		if f.UncompressedSize64 > 512<<20 {
			return nil, fmt.Errorf("%w: %s is over 512 MB", errFsXlsx, name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		b := bytes.NewBuffer(make([]byte, 0, f.UncompressedSize64))
		_, err = b.ReadFrom(io.LimitReader(rc, 512<<20))
		return b.Bytes(), err
	}
	wb, err := read("xl/workbook.xml")
	if err != nil {
		return err
	}
	rels, err := read("xl/_rels/workbook.xml.rels")
	if err != nil {
		return err
	}
	targets := map[string]string{} // relationship id → part
	for _, m := range fsRelRe.FindAll(rels, -1) {
		id, _ := fsAttr(m, "Id")
		t, _ := fsAttr(m, "Target")
		if strings.HasPrefix(t, "/") {
			t = t[1:]
		} else {
			t = path.Join("xl", t)
		}
		targets[id] = t
	}
	parts, names := map[string]string{}, map[string]string{} // sheet name → part, and back
	for _, m := range fsSheetRe.FindAll(wb, -1) {
		name, _ := fsAttr(m, "name")
		id, _ := fsAttr(m, "r:id")
		parts[html.UnescapeString(name)] = targets[id]
		names[targets[id]] = html.UnescapeString(name)
	}
	bySheet := map[string][]fsCellEdit{}
	for _, e := range edits {
		part := parts[e.Sheet]
		if files[part] == nil || e.R < 0 || e.C < 0 || e.R >= 1<<20 || e.C >= 1<<14 {
			return fmt.Errorf("%w: no cell %d,%d in sheet %q", errFsXlsx, e.R, e.C, e.Sheet)
		}
		bySheet[part] = append(bySheet[part], e)
	}

	zw := zip.NewWriter(out)
	zw.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) { return flate.NewWriter(w, flate.BestSpeed) })
	for _, f := range zr.File {
		var b []byte
		switch {
		case bySheet[f.Name] != nil:
			if b, err = read(f.Name); err == nil {
				b, err = fsPatchSheet(b, bySheet[f.Name])
			}
			if se := (*fsSharedErr)(nil); errors.As(err, &se) {
				se.Sheet = names[f.Name]
			}
		case f.Name == "xl/workbook.xml":
			b, err = fsCalcOnLoad(wb)
		case f.Name == "xl/calcChain.xml":
			continue // the order Excel last calculated formulas in: it rebuilds it
		case f.Name == "xl/_rels/workbook.xml.rels":
			b = fsChainRelRe.ReplaceAll(rels, nil)
		case f.Name == "[Content_Types].xml":
			if b, err = read(f.Name); err == nil {
				b = fsChainTypeRe.ReplaceAll(b, nil)
			}
		default:
			if err := zw.Copy(f); err != nil { // as it was, still compressed
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		h := f.FileHeader
		h.Method, h.Extra, h.CRC32, h.CompressedSize64, h.UncompressedSize64 = zip.Deflate, nil, 0, 0, 0
		fw, err := zw.CreateHeader(&h)
		if err == nil {
			_, err = fw.Write(b)
		}
		if err != nil {
			return err
		}
	}
	return zw.Close()
}

// fsCalcOnLoad has Excel recalculate every formula when it opens the workbook: an edited value's dependents still
// hold what they were (cached values), and an edited formula has none yet.
func fsCalcOnLoad(wb []byte) ([]byte, error) {
	if m := fsCalcPrRe.FindSubmatchIndex(wb); m != nil {
		tag := wb[m[0]:m[1]]
		if bytes.Contains(tag, []byte("fullCalcOnLoad=")) {
			tag = fsFullCalcRe.ReplaceAll(tag, []byte(`fullCalcOnLoad="1"`))
		} else {
			tag = append([]byte(`<calcPr fullCalcOnLoad="1"`), tag[len("<calcPr"):]...)
		}
		return slices.Concat(wb[:m[0]], tag, wb[m[1]:]), nil
	}
	// none: it goes after the last of these, as the schema orders a workbook's children
	at := -1
	for _, t := range []string{"</sheets>", "</functionGroups>", "</externalReferences>", "</definedNames>"} {
		if i := bytes.LastIndex(wb, []byte(t)); i >= 0 {
			at = max(at, i+len(t))
		}
	}
	if at < 0 {
		return nil, fmt.Errorf("%w: no sheets in workbook.xml", errFsXlsx)
	}
	return slices.Concat(wb[:at], []byte(`<calcPr fullCalcOnLoad="1"/>`), wb[at:]), nil
}

// fsPatchSheet applies edits to a worksheet's XML; every byte not in an edited cell (or a row made for one) stays.
func fsPatchSheet(x []byte, edits []fsCellEdit) ([]byte, error) {
	last := map[[2]int]string{} // the last edit of each cell
	for _, e := range edits {
		last[[2]int{e.R, e.C}] = e.Raw
	}
	keys := slices.SortedFunc(maps.Keys(last), func(a, b [2]int) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1])) })
	free, err := fsSharedFree(x, last)
	if err != nil {
		return nil, err
	}
	open := fsNextTag(x, 0, len(x), "sheetData")
	if open < 0 {
		return nil, fmt.Errorf("%w: no sheetData (or a prefixed one)", errFsXlsx)
	}
	te := open + bytes.IndexByte(x[open:], '>')
	body, end := te+1, 0
	if x[te-1] == '/' { // <sheetData/>: an empty sheet
		x = slices.Concat(x[:open], []byte("<sheetData></sheetData>"), x[te+1:])
		body, end = open+len("<sheetData>"), open+len("<sheetData>")
	} else if end = bytes.Index(x[body:], []byte("</sheetData>")); end < 0 {
		return nil, fmt.Errorf("%w: sheetData isn't closed", errFsXlsx)
	} else {
		end += body
	}

	var out bytes.Buffer
	out.Grow(len(x) + 64*len(keys))
	out.Write(fsGrowDim(x[:body], keys, last))
	pos, scan, n, k := body, body, 0, 0 // x[:pos] is out; the next row is looked for from scan; n: the last row's number
	for k < len(keys) {
		i := fsNextTag(x, scan, end, "row")
		if i < 0 {
			break
		}
		tag := x[i : i+bytes.IndexByte(x[i:], '>')+1]
		self, rowEnd := tag[len(tag)-2] == '/', i+len(tag)
		if !self {
			e := bytes.Index(x[rowEnd:end], []byte("</row>"))
			if e < 0 {
				return nil, fmt.Errorf("%w: a row isn't closed", errFsXlsx)
			}
			rowEnd += e + len("</row>")
		}
		n++
		if v, ok := fsAttr(tag, "r"); ok {
			n, _ = strconv.Atoi(v)
		}
		if keys[k][0]+1 <= n {
			out.Write(x[pos:i])
			pos = i
			for k < len(keys) && keys[k][0]+1 < n { // rows that weren't there, before this one
				k = fsNewRow(&out, keys, k, last)
			}
			if k < len(keys) && keys[k][0]+1 == n {
				k2 := k
				for k2 < len(keys) && keys[k2][0] == keys[k][0] {
					k2++
				}
				if err := fsPatchRow(&out, x[i:rowEnd], tag, self, n, keys[k:k2], last, free); err != nil {
					return nil, err
				}
				pos, k = rowEnd, k2
			}
		}
		scan = rowEnd
	}
	out.Write(x[pos:end])
	for k < len(keys) {
		k = fsNewRow(&out, keys, k, last)
	}
	out.Write(x[end:])
	return out.Bytes(), nil
}

// fsPatchRow writes a row with its edited cells replaced (keeping each one's style) or put in, in column order.
func fsPatchRow(out *bytes.Buffer, row, tag []byte, self bool, n int, keys [][2]int, last map[[2]int]string, free map[string]bool) error {
	open := fsSpansRe.ReplaceAll(tag, nil) // which columns it spans: a hint, and it may span more now
	if self {
		open = append(bytes.TrimRight(bytes.TrimSuffix(open, []byte("/>")), " \t\r\n"), '>')
	}
	out.Write(open)
	var content []byte
	if !self {
		content = row[len(tag) : len(row)-len("</row>")]
	}
	tail := len(content) // new cells after the last go here: before an extLst, if it has one
	if i := bytes.Index(content, []byte("<extLst")); i >= 0 {
		tail = i
	}
	pos, scan, col, j := 0, 0, -1, 0
	for j < len(keys) {
		i := fsNextTag(content, scan, tail, "c")
		if i < 0 {
			break
		}
		ctag := content[i : i+bytes.IndexByte(content[i:], '>')+1]
		cend := i + len(ctag)
		if ctag[len(ctag)-2] != '/' {
			e := bytes.Index(content[cend:], []byte("</c>"))
			if e < 0 {
				return fmt.Errorf("%w: a cell isn't closed", errFsXlsx)
			}
			cend += e + len("</c>")
		}
		col++
		if ref, ok := fsAttr(ctag, "r"); ok {
			col = fsColOf(ref)
		}
		if keys[j][1] <= col {
			out.Write(content[pos:i])
			pos = i
			for j < len(keys) && keys[j][1] < col {
				fsWriteCell(out, n, keys[j][1], last[keys[j]], "")
				j++
			}
			if j < len(keys) && keys[j][1] == col {
				if f := bytes.Index(content[i:cend], []byte("<f")); f >= 0 {
					ftag := content[i+f : i+f+bytes.IndexByte(content[i+f:], '>')+1]
					if si, _ := fsAttr(ftag, "si"); bytes.Contains(ftag, []byte("ref=")) && !free[si] {
						return fmt.Errorf("%w: %s%d starts an array formula", errFsXlsx, fsColName(col), n)
					}
				}
				style, _ := fsAttr(ctag, "s")
				fsWriteCell(out, n, col, last[keys[j]], style)
				pos = cend
				j++
			}
		}
		scan = cend
	}
	out.Write(content[pos:tail])
	for ; j < len(keys); j++ {
		fsWriteCell(out, n, keys[j][1], last[keys[j]], "")
	}
	out.Write(content[tail:])
	out.WriteString("</row>")
	return nil
}

// fsSharedFree checks the shared formulas whose first cell is edited: each is free to go (by its si) when every cell
// that copies it is edited too; else an fsSharedErr asks for them.
func fsSharedFree(x []byte, last map[[2]int]string) (map[string]bool, error) {
	free := map[string]bool{}
	cellAt := func(f int) ([2]int, bool) { // the cell an <f> at f is in
		c := bytes.LastIndex(x[:f], []byte("<c "))
		if c < 0 {
			return [2]int{}, false
		}
		ref, ok := fsAttr(x[c:c+bytes.IndexByte(x[c:], '>')+1], "r")
		if !ok {
			return [2]int{}, false
		}
		i := strings.IndexFunc(ref, func(r rune) bool { return r >= '0' && r <= '9' })
		row, _ := strconv.Atoi(ref[max(i, 0):])
		return [2]int{row - 1, fsColOf(ref)}, i > 0
	}
	for at := 0; ; {
		i := fsNextTag(x, at, len(x), "f")
		if i < 0 {
			return free, nil
		}
		ftag := x[i : i+bytes.IndexByte(x[i:], '>')+1]
		at = i + len(ftag)
		if t, _ := fsAttr(ftag, "t"); t != "shared" {
			continue
		}
		ref, isFirst := fsAttr(ftag, "ref")
		si, _ := fsAttr(ftag, "si")
		cell, ok := cellAt(i)
		if !isFirst || !ok {
			continue
		}
		if _, edited := last[cell]; !edited {
			continue
		}
		// every cell copying it (si="…" without a ref), anywhere in the sheet, must be edited too
		mark := []byte(`si="` + si + `"`)
		for j := 0; ; {
			k := bytes.Index(x[j:], mark)
			if k < 0 {
				break
			}
			k += j
			j = k + len(mark)
			f := bytes.LastIndex(x[:k], []byte("<f "))
			if c, ok := cellAt(f); f < 0 || !ok || c != cell && !fsHas(last, c) {
				return nil, &fsSharedErr{Ref: ref}
			}
		}
		free[si] = true
	}
}

func fsHas(last map[[2]int]string, c [2]int) bool { _, ok := last[c]; return ok }

// fsNewRow writes a row for the edits in keys from k that share its number; it returns the index after them.
func fsNewRow(out *bytes.Buffer, keys [][2]int, k int, last map[[2]int]string) int {
	k2 := k
	for k2 < len(keys) && keys[k2][0] == keys[k][0] {
		k2++
	}
	var cells bytes.Buffer
	for _, key := range keys[k:k2] {
		fsWriteCell(&cells, key[0]+1, key[1], last[key], "")
	}
	if cells.Len() > 0 {
		fmt.Fprintf(out, `<row r="%d">`, keys[k][0]+1)
		out.Write(cells.Bytes())
		out.WriteString("</row>")
	}
	return k2
}

// fsWriteCell writes one cell as typed (as the sheet worker reads what's typed): a number, a formula (no value yet:
// Excel calculates it on opening), text (inline), or, cleared, only its style if it had one.
func fsWriteCell(out *bytes.Buffer, row, col int, raw, style string) {
	ref := fsColName(col) + strconv.Itoa(row)
	if raw == "" {
		if style != "" {
			fmt.Fprintf(out, `<c r="%s" s="%s"/>`, ref, style)
		}
		return
	}
	fmt.Fprintf(out, `<c r="%s"`, ref)
	if style != "" {
		fmt.Fprintf(out, ` s="%s"`, style)
	}
	t := strings.TrimSpace(raw)
	if len(raw) > 1 && raw[0] == '=' {
		out.WriteString("><f>" + fsXMLText(raw[1:]) + "</f></c>")
	} else if v, err := strconv.ParseFloat(t, 64); err == nil && fsNumRe.MatchString(t) {
		out.WriteString("><v>" + strconv.FormatFloat(v, 'g', -1, 64) + "</v></c>")
	} else {
		out.WriteString(` t="inlineStr"><is><t xml:space="preserve">` + fsXMLText(raw) + "</t></is></c>")
	}
}

// fsXMLText escapes text for an element's content, dropping what XML 1.0 can't hold.
func fsXMLText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' || r == 0xFFFE || r == 0xFFFF {
			return -1
		}
		return r
	}, s)
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// fsGrowDim widens the sheet's <dimension> (the used range, before sheetData) to take in new cells.
func fsGrowDim(head []byte, keys [][2]int, last map[[2]int]string) []byte {
	m := fsDimRe.FindSubmatchIndex(head)
	if m == nil {
		return head
	}
	c0, r0 := fsColOf(string(head[m[2]:m[3]])), atoi(head[m[4]:m[5]])
	c1, r1 := c0, r0
	if m[6] >= 0 {
		c1, r1 = fsColOf(string(head[m[6]:m[7]])), atoi(head[m[8]:m[9]])
	}
	nc1, nr1 := c1, r1
	for _, k := range keys {
		if last[k] != "" {
			nc1, nr1 = max(nc1, k[1]), max(nr1, k[0]+1)
		}
	}
	if nc1 == c1 && nr1 == r1 {
		return head
	}
	ref := fmt.Sprintf(`<dimension ref="%s%d:%s%d"`, fsColName(c0), r0, fsColName(nc1), nr1)
	return slices.Concat(head[:m[0]], []byte(ref), head[m[1]:])
}

func atoi(b []byte) int { n, _ := strconv.Atoi(string(b)); return n }

// fsNextTag finds the next <name element (not one whose name only starts so) in x[from:to].
func fsNextTag(x []byte, from, to int, name string) int {
	for from < to {
		i := bytes.Index(x[from:to], []byte("<"+name))
		if i < 0 {
			return -1
		}
		i += from
		if e := i + 1 + len(name); e < len(x) && (x[e] == ' ' || x[e] == '>' || x[e] == '/' || x[e] == '\t' || x[e] == '\n' || x[e] == '\r') {
			return i
		}
		from = i + 1
	}
	return -1
}

// fsAttr reads an attribute of a start tag.
func fsAttr(tag []byte, name string) (string, bool) {
	for i := 0; ; {
		j := bytes.Index(tag[i:], []byte(name+"="))
		if j < 0 {
			return "", false
		}
		j += i
		if q := j + len(name) + 1; j > 0 && strings.IndexByte(" \t\r\n", tag[j-1]) >= 0 && q < len(tag) && (tag[q] == '"' || tag[q] == '\'') {
			if e := bytes.IndexByte(tag[q+1:], tag[q]); e >= 0 {
				return string(tag[q+1 : q+1+e]), true
			}
			return "", false
		}
		i = j + 1
	}
}

// fsColOf: "AB12" → 27 (0-based); fsColName: 27 → "AB".
func fsColOf(ref string) int {
	c := 0
	for _, ch := range ref {
		if ch < 'A' || ch > 'Z' {
			break
		}
		c = c*26 + int(ch-'A'+1)
	}
	return c - 1
}

func fsColName(c int) string {
	var b []byte
	for c++; c > 0; c = (c - 1) / 26 {
		b = append([]byte{byte('A' + (c-1)%26)}, b...)
	}
	return string(b)
}
