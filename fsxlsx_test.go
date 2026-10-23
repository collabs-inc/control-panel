package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// a workbook as Excel leaves one: styled cells, shared strings, a row left out, an empty self-closed row, a shared
// formula, a calculation chain, and parts the panel never reads (styles, a picture)
var fsTestBook = map[string]string{
	"[Content_Types].xml": `<Types><Override PartName="/xl/workbook.xml" ContentType="wb"/><Override PartName="/xl/calcChain.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.calcChain+xml"/></Types>`,
	"xl/workbook.xml":     `<workbook xmlns:r="r"><sheets><sheet name="Rent &amp; roll" sheetId="1" r:id="rId1"/><sheet name="Other" sheetId="2" r:id="rId2"/></sheets><definedNames><definedName name="x">A1</definedName></definedNames><extLst/></workbook>`,
	"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Type="worksheet" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Type="worksheet" Target="/xl/worksheets/sheet2.xml"/>` +
		`<Relationship Id="rId9" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/calcChain" Target="calcChain.xml"/></Relationships>`,
	"xl/worksheets/sheet1.xml": `<worksheet><dimension ref="A1:C4"/><sheetData>` +
		`<row r="1" spans="1:3"><c r="A1" s="1" t="s"><v>0</v></c><c r="B1" s="2"><v>1400</v></c><c r="C1"><f t="shared" ref="C1:C2" si="0">B1*2</f><v>2800</v></c></row>` +
		`<row r="2"><c r="A2" t="s"><v>1</v></c><c r="C2"><f t="shared" si="0"/><v>0</v></c></row>` +
		`<row r="4" spans="1:1"/>` +
		`</sheetData><pageMargins left="0.7"/></worksheet>`,
	"xl/worksheets/sheet2.xml": `<worksheet><dimension ref="A1"/><sheetData/></worksheet>`,
	"xl/calcChain.xml":         `<calcChain><c r="C1" i="1"/></calcChain>`,
	"xl/styles.xml":            `<styleSheet><fills><fill><patternFill patternType="solid"/></fill></fills></styleSheet>`,
	"xl/media/image1.png":      "\x89PNG not really",
}

func writeTestBook(t *testing.T, p string, parts map[string]string) {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, name := range []string{"[Content_Types].xml", "xl/workbook.xml", "xl/_rels/workbook.xml.rels", "xl/worksheets/sheet1.xml", "xl/worksheets/sheet2.xml", "xl/calcChain.xml", "xl/styles.xml", "xl/media/image1.png"} {
		w, _ := zw.Create(name)
		io.WriteString(w, parts[name])
	}
	zw.Close()
	if err := os.WriteFile(p, b.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
}

func readTestBook(t *testing.T, p string) map[string]string {
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

func TestFsXlsxPatchesCellsAndLeavesTheRest(t *testing.T) {
	f := newFsEnv(t)
	p := filepath.Join(f.home, "rent.xlsx")
	writeTestBook(t, p, fsTestBook)
	fi, _ := os.Stat(p)
	edits := []map[string]any{
		{"sheet": "Rent & roll", "r": 0, "c": 1, "raw": "1"},                    // replaced later: the last edit wins
		{"sheet": "Rent & roll", "r": 0, "c": 1, "raw": "1550.50"},              // B1: a number, its style kept
		{"sheet": "Rent & roll", "r": 1, "c": 1, "raw": " part paid & <late> "}, // B2: new, between A2 and C2
		{"sheet": "Rent & roll", "r": 1, "c": 0, "raw": ""},                     // A2: cleared (no style: gone)
		{"sheet": "Rent & roll", "r": 2, "c": 3, "raw": "=SUM(B1:B2)"},          // D3: a row that wasn't there
		{"sheet": "Rent & roll", "r": 3, "c": 0, "raw": "x"},                    // A4: into a self-closed row
		{"sheet": "Rent & roll", "r": 9, "c": 4, "raw": "-2e3"},                 // E10: after every row
		{"sheet": "Other", "r": 0, "c": 0, "raw": "hello"},                      // an empty sheet
	}
	var res map[string]any
	if code := f.call(t, "POST", "/api/fs/xlsx?path="+q(p)+"&mtime="+strconv.FormatInt(fi.ModTime().UnixMilli(), 10), map[string]any{"edits": edits}, &res); code != 200 {
		t.Fatalf("%d %v", code, res)
	}
	got := readTestBook(t, p)
	want := `<worksheet><dimension ref="A1:E10"/><sheetData>` +
		`<row r="1"><c r="A1" s="1" t="s"><v>0</v></c><c r="B1" s="2"><v>1550.5</v></c><c r="C1"><f t="shared" ref="C1:C2" si="0">B1*2</f><v>2800</v></c></row>` +
		`<row r="2"><c r="B2" t="inlineStr"><is><t xml:space="preserve"> part paid &amp; &lt;late&gt; </t></is></c><c r="C2"><f t="shared" si="0"/><v>0</v></c></row>` +
		`<row r="3"><c r="D3"><f>SUM(B1:B2)</f></c></row>` +
		`<row r="4"><c r="A4" t="inlineStr"><is><t xml:space="preserve">x</t></is></c></row>` +
		`<row r="10"><c r="E10"><v>-2000</v></c></row>` +
		`</sheetData><pageMargins left="0.7"/></worksheet>`
	if got["xl/worksheets/sheet1.xml"] != want {
		t.Fatalf("sheet1:\n%s\nwant\n%s", got["xl/worksheets/sheet1.xml"], want)
	}
	if s := got["xl/worksheets/sheet2.xml"]; s != `<worksheet><dimension ref="A1"/><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t xml:space="preserve">hello</t></is></c></row></sheetData></worksheet>` {
		t.Fatalf("sheet2: %s", s)
	}
	if _, ok := got["xl/calcChain.xml"]; ok || strings.Contains(got["xl/_rels/workbook.xml.rels"], "calcChain") || strings.Contains(got["[Content_Types].xml"], "calcChain") {
		t.Fatal("the calculation chain should go, with its relationship and content type")
	}
	if !strings.Contains(got["xl/workbook.xml"], `</definedNames><calcPr fullCalcOnLoad="1"/><extLst/>`) {
		t.Fatalf("workbook: %s", got["xl/workbook.xml"])
	}
	if got["xl/styles.xml"] != fsTestBook["xl/styles.xml"] || got["xl/media/image1.png"] != fsTestBook["xl/media/image1.png"] {
		t.Fatal("parts not edited should stay as they were")
	}
	if fi2, _ := os.Stat(p); fi2.Mode().Perm() != 0o640 || res["mtime"] == nil {
		t.Fatalf("mode %v, answer %v", fi2.Mode(), res)
	}

	// a shared formula's first cell goes only with every cell that copies it: alone, the answer says where they are
	// (the page writes their formulas out and saves again)
	fi, _ = os.Stat(p)
	ver := "&mtime=" + strconv.FormatInt(fi.ModTime().UnixMilli(), 10)
	if code := f.call(t, "POST", "/api/fs/xlsx?path="+q(p)+ver, map[string]any{"edits": []map[string]any{{"sheet": "Rent & roll", "r": 0, "c": 2, "raw": "5"}}}, &res); code != 422 || fmt.Sprint(res["shared"]) != "map[ref:C1:C2 sheet:Rent & roll]" {
		t.Fatalf("shared formula alone: %d %v", code, res)
	}
	both := []map[string]any{{"sheet": "Rent & roll", "r": 1, "c": 2, "raw": "=B2*2"}, {"sheet": "Rent & roll", "r": 0, "c": 2, "raw": "5"}}
	if code := f.call(t, "POST", "/api/fs/xlsx?path="+q(p)+ver, map[string]any{"edits": both}, &res); code != 200 {
		t.Fatalf("shared formula with its cells: %d %v", code, res)
	}
	if s := readTestBook(t, p)["xl/worksheets/sheet1.xml"]; !strings.Contains(s, `<c r="C1"><v>5</v></c>`) || !strings.Contains(s, `<c r="C2"><f>B2*2</f></c>`) || strings.Contains(s, "shared") {
		t.Fatalf("the shared formula should be gone, written out: %s", s)
	}
	// and a workbook changed since it was opened isn't overwritten
	if code := f.call(t, "POST", "/api/fs/xlsx?path="+q(p)+"&mtime=1", map[string]any{"edits": edits[:1]}, &res); code != 409 {
		t.Fatalf("stale version: %d %v", code, res)
	}
}
