package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"
)

func fsTestSearch(sn *fsSnap, q string, mod func(*fsQuery)) []string {
	fq := fsQuery{terms: fsTerms(q), in: -1, limit: 50, hidden: true}
	if mod != nil {
		mod(&fq)
	}
	hits, _ := sn.search(fq)
	var out []string
	for _, h := range hits {
		out = append(out, sn.path(h.id))
	}
	return out
}

func TestFsIndexRankingAndFilters(t *testing.T) {
	home, other := t.TempDir(), t.TempDir()
	for _, p := range []string{
		"docs/rent-roll.xlsx", "docs/Rent Roll 2024.pdf", "deep/a/b/c/d/rentals.txt", "proj/currentRent.go",
		"proj/node_modules/rent/index.js", ".config/rent.toml", "rent", "parent.md",
		"rentals/notes.md", "rentals/q3/summary.md",
	} {
		writeFile(t, filepath.Join(home, p), "x")
	}
	writeFile(t, filepath.Join(other, "rent.log"), "x")
	os.Symlink("/etc", filepath.Join(home, "etc-link")) // indexed as itself, never walked into
	sn := buildFsSnap(newFsIndex(home, other).roots, nil)
	h := func(p string) string { return filepath.Join(home, p) }

	got := fsTestSearch(sn, "rent", nil)
	if len(got) < 5 || got[0] != h("rent") {
		t.Fatalf("an exact name ranks first: %q", got)
	}
	rank := func(p string) int { return slices.Index(got, p) }
	if !(rank(h("docs/rent-roll.xlsx")) < rank(h("proj/currentRent.go")) && rank(h("proj/currentRent.go")) < rank(h("parent.md"))) {
		t.Fatalf("prefix > camelCase boundary > anywhere: %q", got)
	}
	if rank(h("proj/node_modules/rent")) < rank(h("docs/rent-roll.xlsx")) || rank(h(".config/rent.toml")) < rank(h("docs/rent-roll.xlsx")) {
		t.Fatalf("an exact name in junk, and a hidden file, rank below a visible prefix match: %q", got)
	}
	if fsTestSearch(sn, "etc", nil)[0] != h("etc-link") || len(fsTestSearch(sn, "passwd", nil)) != 0 {
		t.Fatal("a symlink is indexed but not followed")
	}

	if got := fsTestSearch(sn, "roll RENT", nil); len(got) != 2 || !slices.Contains(got, h("docs/rent-roll.xlsx")) || !slices.Contains(got, h("docs/Rent Roll 2024.pdf")) {
		t.Fatalf("every term, any case: %q", got)
	}
	if got := fsTestSearch(sn, "rentals summary", nil); len(got) != 1 || got[0] != h("rentals/q3/summary.md") {
		t.Fatalf("a folder term plus a file term finds the file: %q", got)
	}
	if got := fsTestSearch(sn, "rent", func(q *fsQuery) { q.hidden = false }); slices.Contains(got, h(".config/rent.toml")) {
		t.Fatalf("hidden=0 drops dotfiles: %q", got)
	}
	if got := fsTestSearch(sn, "rent", func(q *fsQuery) { q.kind = "sheets" }); len(got) != 1 || got[0] != h("docs/rent-roll.xlsx") {
		t.Fatalf("kind=sheets: %q", got)
	}
	if got := fsTestSearch(sn, "rent", func(q *fsQuery) { q.kind = "folders" }); len(got) != 2 || !slices.Contains(got, h("rentals")) {
		t.Fatalf("kind=folders: %q", got)
	}
	if got := fsTestSearch(sn, "rent", func(q *fsQuery) { q.in = sn.find(h("docs")) }); len(got) != 2 {
		t.Fatalf("in=docs: %q", got)
	}
	if got := fsTestSearch(sn, "rent", func(q *fsQuery) { q.in = sn.find(other) }); len(got) != 1 || got[0] != filepath.Join(other, "rent.log") {
		t.Fatalf("in=the other root: %q", got)
	}
	if sn.find(h("nope")) != -1 || sn.find("/elsewhere") != -1 {
		t.Fatal("find of an unindexed path")
	}
	if _, total := sn.search(fsQuery{terms: fsTerms("rent"), in: -1, limit: 1, hidden: true}); total != 10 {
		t.Fatalf("total counts every match, not just the kept ones: %d", total)
	}
}

// FSINDEX_REAL=1 go test -run TestFsIndexReal -v: build over the real home and /tmp, and time queries.
func TestFsIndexReal(t *testing.T) {
	if os.Getenv("FSINDEX_REAL") == "" || testing.Short() {
		t.Skip("set FSINDEX_REAL=1 to measure on the real home and /tmp")
	}
	home, _ := os.UserHomeDir()
	x := newFsIndex(home, "/tmp")
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	for range 2 { // the first walk may come from a cold disk cache
		t0 := time.Now()
		x.rebuild()
		t.Logf("build: %v", time.Since(t0))
	}
	runtime.GC()
	runtime.ReadMemStats(&m1)
	sn := x.cur.Load()
	debug.FreeOSMemory()
	status, _ := os.ReadFile("/proc/self/status")
	rss, _, _ := strings.Cut(string(status)[strings.Index(string(status), "VmRSS:")+6:], "\n")
	t.Logf("entries: %d, names: %d MB (+%d MB originals), heap held: %d MB, process RSS: %s", len(sn.parent), len(sn.lower)>>20, len(sn.up)>>20,
		(m1.HeapAlloc-m0.HeapAlloc)>>20, strings.TrimSpace(rss))
	t0 := time.Now()
	id := sn.find(filepath.Join(home, "projects/control-panel-files"))
	t.Logf("find ~/projects/control-panel-files: %v (id %d)", time.Since(t0), id)
	for _, q := range []string{"rent roll", "docker-compose.yml", ".env", "sync_appfolio", "server.go", "control-panel fs.go", "zzqx", "e", "go", "py"} {
		var ds []time.Duration
		var total int
		for range 15 {
			t0 := time.Now()
			_, total = sn.search(fsQuery{terms: fsTerms(q), in: -1, limit: 100, hidden: true})
			ds = append(ds, time.Since(t0))
		}
		slices.Sort(ds)
		hidden, _ := sn.search(fsQuery{terms: fsTerms(q), in: -1, limit: 100})
		top := ""
		if len(hidden) > 0 {
			top = sn.path(hidden[0].id)
		}
		t.Logf("%-22q p50 %6.2f ms  max %6.2f ms  matches %7d  top(hidden=0): %s", q, ms(ds[len(ds)/2]), ms(ds[len(ds)-1]), total, top)
	}
	s := &server{fsx: x} // the whole request: + paths, lstat, recency
	for _, q := range []string{"rent roll", "e", "server.go&in=~/projects"} {
		rec := httptest.NewRecorder()
		s.fsSearch(rec, httptest.NewRequest("GET", "/api/fs/search?q="+strings.ReplaceAll(q, " ", "+"), nil))
		var v struct{ Ms float64 }
		json.Unmarshal(rec.Body.Bytes(), &v)
		t.Logf("GET /api/fs/search?q=%s: %.2f ms", q, v.Ms)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func TestFsIndexFindsWhatFilesMadeBeforeTheNextWalk(t *testing.T) {
	dir := t.TempDir()
	x := newFsIndex(dir)
	x.rebuild()
	p := filepath.Join(x.roots[0], "quarterly-report.pdf")
	writeFile(t, p, "x")
	x.note(p)
	hits := x.freshHits(fsQuery{terms: fsTerms("quarterly report"), hidden: true, in: -1}, "")
	if len(hits) != 1 || hits[0].path != p {
		t.Fatalf("fresh hits = %v", hits)
	}
	if h := x.freshHits(fsQuery{terms: fsTerms("quarterly"), in: -1}, filepath.Join(x.roots[0], "elsewhere")); len(h) != 0 {
		t.Fatalf("a search in another folder found it: %v", h)
	}
	x.rebuild() // the walk folds it in: it's no longer fresh
	if h := x.freshHits(fsQuery{terms: fsTerms("quarterly"), in: -1}, ""); len(h) != 0 {
		t.Fatalf("still fresh after a walk: %v", h)
	}
}

// A rewalk lists again only the folders whose ctime moved and takes the rest from the last build: what it finds must
// be exactly what a full walk finds, through a file added deep down, one removed, a folder renamed and one made.
func TestFsIndexRewalkMatchesAFullWalk(t *testing.T) {
	defer func(s time.Duration) { fsSettle = s }(fsSettle)
	fsSettle = 30 * time.Millisecond // timestamps tick every few ms: the changes below come well after
	root := t.TempDir()
	for _, p := range []string{"a/b/c/deep.txt", "a/old.txt", "x/inner/keep.md", "quiet/q1/q2/still.go", "Top.TXT"} {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(root, p), nil, 0o644)
	}
	time.Sleep(60 * time.Millisecond)
	paths := func(sn *fsSnap) []string {
		var out []string
		for i := range sn.parent {
			out = append(out, sn.path(int32(i)))
		}
		slices.Sort(out)
		return out
	}
	prev := buildFsSnap([]string{root}, nil)
	os.WriteFile(filepath.Join(root, "a/b/c/new.txt"), nil, 0o644)
	os.Remove(filepath.Join(root, "a/old.txt"))
	os.Rename(filepath.Join(root, "x"), filepath.Join(root, "y"))
	os.MkdirAll(filepath.Join(root, "a/b/c/made/later"), 0o755)
	next, full := buildFsSnap([]string{root}, prev), buildFsSnap([]string{root}, nil)
	got, want := paths(next), paths(full)
	if !slices.Equal(got, want) {
		t.Fatalf("rewalk found\n%v\nfull walk\n%v", got, want)
	}
	for _, p := range []string{"a/b/c/new.txt", "y/inner/keep.md", "a/b/c/made/later", "quiet/q1/q2/still.go", "Top.TXT"} {
		if !slices.Contains(got, filepath.Join(root, p)) {
			t.Errorf("missing %s", p)
		}
	}
	if slices.Contains(got, filepath.Join(root, "a/old.txt")) || slices.Contains(got, filepath.Join(root, "x")) {
		t.Error("kept what's gone")
	}
	if _, _, ct, ok := next.listed(next.find(filepath.Join(root, "quiet/q1"))); !ok || ct == 0 {
		t.Error("an untouched folder should be trusted next time")
	}
	for _, sn := range []*fsSnap{prev, next, full} {
		sn.arena.free()
	}
}
