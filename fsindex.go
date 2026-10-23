package main

import (
	"bytes"
	"container/heap"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

// ---- Files search: every name under home and /tmp, in memory ----
//
// ponytail: a rewalk (every 10 min, and soon after a change made through the panel), no inotify: 2.5M files would
// blow the inotify watch limit. A rewalk lists again only the folders whose ctime moved (a stat each for the rest),
// and every sixth is a full one. Upgrade path: fanotify with FAN_REPORT_DFID_NAME (needs CAP_SYS_ADMIN).

const (
	fsDir uint8 = 1 << iota
	fsHidden
	fsJunk
	fsUpper // its name isn't all lowercase: the original is in the up arena
)

var fsJunkNames = map[string]bool{"node_modules": true, ".git": true, "__pycache__": true, ".cache": true, ".venv": true, "venv": true,
	"site-packages": true, ".npm": true, ".cargo": true, ".pixi": true, ".docker": true, "Trash": true, "dist": true, "build": true,
	"target": true, ".next": true, "_work": true} // _work: CI runners' checkouts, copies of the repos

var fsKinds = map[string]string{
	"docs":    "pdf doc docx odt rtf txt md pptx ppt key pages",
	"sheets":  "xlsx xls ods csv tsv numbers",
	"code":    "py js ts tsx jsx go rs java c h cpp sh rb php swift kt sql json yaml yml toml html css scss vue svelte lua r ipynb",
	"images":  "png jpg jpeg gif webp svg heic bmp ico tiff avif",
	"media":   "mp4 mov webm mkv mp3 wav m4a ogg flac",
	"folders": "",
}

var fsKindOf = func() map[string]string {
	m := map[string]string{}
	for k, exts := range fsKinds {
		for _, e := range strings.Fields(exts) {
			m[e] = k
		}
	}
	return m
}()

// fsSnap is one build of the index. Entries live in arenas, not structs: 2.5M names fit in ~90 MB.
type fsSnap struct {
	lower  []byte   // every name lowercased, each followed by a 0 byte so a match can't span two names
	off    []uint32 // entry i's name starts at off[i]; off[n] is the arena's end
	parent []int32  // -1 for a root, whose name is its full path
	flags  []uint8
	up     []byte   // the original of each name lowercasing changed (a fifth of them)...
	upID   []int32  // ...for these entries, ascending...
	upOff  []uint32 // ...starting here (plus an end)
	// each folder listed: its entry, where its children start and how many, and its ctime then (0: list it again
	// next time), ascending by entry; the next walk only stats a folder whose ctime hasn't moved
	dirID, dirFirst, dirN []int32
	dirCt                 []int64
	at                    int64    // built, unix ms
	arena                 *fsArena // where all of the above live
}

// fsArena is memory outside the Go heap (anonymous mmap) for a snapshot's plain bytes and numbers. On the heap,
// ~85 MB the collector never needs to scan would still let the heap grow ~2× past it between collections (GOGC):
// the panel's RSS went from 39 MB to 239 MB. Out here an index costs its size.
type fsArena struct{ maps [][]byte }

func (a *fsArena) bytes(n int) []byte {
	if n == 0 {
		return nil
	}
	b, err := syscall.Mmap(-1, 0, n, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	if err != nil {
		return make([]byte, n) // no mmap: the heap, then
	}
	a.maps = append(a.maps, b)
	return b
}

func fsU32(a *fsArena, n int) []uint32 {
	if b := a.bytes(n * 4); len(b) > 0 {
		return unsafe.Slice((*uint32)(unsafe.Pointer(&b[0])), n)
	}
	return nil
}

func fsI32(a *fsArena, n int) []int32 {
	if b := a.bytes(n * 4); len(b) > 0 {
		return unsafe.Slice((*int32)(unsafe.Pointer(&b[0])), n)
	}
	return nil
}

func fsI64(a *fsArena, n int) []int64 { // each arena slice is its own mapping: page-aligned
	if b := a.bytes(n * 8); len(b) > 0 {
		return unsafe.Slice((*int64)(unsafe.Pointer(&b[0])), n)
	}
	return nil
}

// free unmaps a replaced snapshot's memory. ponytail: after a grace period (a minute: a search takes milliseconds)
// rather than a reference count; one still running past it would fault.
func (a *fsArena) free() {
	for _, b := range a.maps {
		syscall.Munmap(b)
	}
	a.maps = nil
}

func (sn *fsSnap) lname(i int32) []byte { return sn.lower[sn.off[i] : sn.off[i+1]-1] }

func (sn *fsSnap) name(i int32) []byte {
	if sn.flags[i]&fsUpper == 0 {
		return sn.lname(i)
	}
	j, _ := slices.BinarySearch(sn.upID, i)
	return sn.up[sn.upOff[j]:sn.upOff[j+1]]
}

// listed says where folder d's children were and its ctime then
func (sn *fsSnap) listed(d int32) (first, n int32, ct int64, ok bool) {
	j, ok := slices.BinarySearch(sn.dirID, d)
	if !ok {
		return 0, 0, 0, false
	}
	return sn.dirFirst[j], sn.dirN[j], sn.dirCt[j], true
}

func (sn *fsSnap) path(i int32) string {
	var parts []string
	for ; i >= 0; i = sn.parent[i] {
		parts = append(parts, string(sn.name(i)))
	}
	slices.Reverse(parts)
	return filepath.Join(parts...)
}

type fsIndex struct {
	roots []string // real paths; roots[0] is home
	cur   atomic.Pointer[fsSnap]

	mu             sync.Mutex
	running, again bool
	pending        bool      // a rebuild is scheduled
	built          time.Time // when the current build began
	fresh          []fsFresh // made through Files since then: searchable at once, folded in by the next build
	walks          int       // every sixth is a full one, trusting no ctime
}

type fsFresh struct {
	path string
	at   time.Time
}

func newFsIndex(roots ...string) *fsIndex {
	x := &fsIndex{}
	for _, r := range roots {
		if real, err := filepath.EvalSymlinks(r); err == nil {
			r = real
		}
		x.roots = append(x.roots, filepath.Clean(r))
	}
	return x
}

// run builds the index 3 s after start, then every 10 minutes.
func (x *fsIndex) run() {
	time.Sleep(3 * time.Second)
	for t := time.NewTicker(10 * time.Minute); ; <-t.C {
		x.rebuild()
	}
}

// rebuild walks the roots and swaps the result in; a call while one runs makes that one go again.
func (x *fsIndex) rebuild() {
	x.mu.Lock()
	if x.running {
		x.again = true
		x.mu.Unlock()
		return
	}
	x.running = true
	x.mu.Unlock()
	for {
		began := time.Now()
		prev := x.cur.Load()
		if x.walks++; x.walks%6 == 0 {
			prev = nil
		}
		if old := x.cur.Swap(buildFsSnap(x.roots, prev)); old != nil {
			time.AfterFunc(time.Minute, old.arena.free)
		}
		x.mu.Lock()
		x.built = began
		x.fresh = slices.DeleteFunc(x.fresh, func(f fsFresh) bool { return f.at.Before(began) }) // the walk saw these
		x.mu.Unlock()
		debug.FreeOSMemory() // the walk's garbage and the old build go back to the OS now, not over minutes
		x.mu.Lock()
		if !x.again {
			x.running = false
			x.mu.Unlock()
			return
		}
		x.again = false
		x.mu.Unlock()
	}
}

// note records what a change through Files made (search finds it at once, from x.fresh), and schedules a rebuild
// for what it can't list (a cloned repo's files, a moved folder's insides): at most one a minute, since a full walk
// of this box costs seconds of CPU.
func (x *fsIndex) note(paths ...string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	now := time.Now()
	for _, p := range paths {
		x.fresh = append(x.fresh, fsFresh{p, now})
	}
	if over := len(x.fresh) - 5000; over > 0 { // a flood: the rebuild catches the rest
		x.fresh = x.fresh[over:]
	}
	if x.pending {
		return
	}
	x.pending = true
	time.AfterFunc(max(2*time.Second, time.Until(x.built.Add(time.Minute))), func() {
		x.mu.Lock()
		x.pending = false
		x.mu.Unlock()
		x.rebuild()
	})
}

// freshHits are the noted paths that match q, scored like the index's own (they're few: a plain scan).
func (x *fsIndex) freshHits(q fsQuery, inPath string) []fsFreshHit {
	x.mu.Lock()
	fresh := slices.Clone(x.fresh)
	x.mu.Unlock()
	var out []fsFreshHit
	seen := map[string]bool{}
	for _, f := range slices.Backward(fresh) {
		p := f.path
		if seen[p] || len(q.terms) == 0 || inPath != "" && !strings.HasPrefix(p, inPath+"/") {
			continue
		}
		seen[p] = true
		lp, name := []byte(strings.ToLower(p)), strings.ToLower(filepath.Base(p))
		if !q.hidden && strings.Contains(p, "/.") {
			continue
		}
		score, inName := 0, false
		for i, t := range q.terms {
			if !bytes.Contains(lp, t) {
				score = -1
				break
			}
			switch {
			case name == string(t):
				score += 1000
			case strings.HasPrefix(name, string(t)) && i == 0:
				score += 400
			case strings.Contains(name, string(t)):
				score += 150
			default:
				score += 30
				continue
			}
			inName = true
		}
		if score > 0 && inName {
			out = append(out, fsFreshHit{p, score - len(name)/4})
		}
	}
	return out
}

type fsFreshHit struct {
	path  string
	score int
}

func (x *fsIndex) building() bool { x.mu.Lock(); defer x.mu.Unlock(); return x.running }

var fsBufs = sync.Pool{New: func() any { b := make([]byte, 64<<10); return &b }}

// fsReadDir lists a folder with getdents64 straight into names (each followed by a 0) and dirs: no os.DirEntry or
// string per entry, and a 64 KB buffer per call. A filesystem that doesn't type its entries gets an lstat each.
func fsReadDir(fd int, dir string, buf, names []byte, dirs []bool) ([]byte, []bool) {
	for {
		n, err := syscall.Getdents(fd, buf)
		if n <= 0 || err != nil {
			return names, dirs
		}
		for p := 0; p+19 <= n; { // linux_dirent64: ino 8, off 8, reclen 2, type 1, name (0-terminated)
			rl := int(binary.NativeEndian.Uint16(buf[p+16:]))
			if rl == 0 || p+rl > n {
				break
			}
			typ, name := buf[p+18], buf[p+19:p+rl]
			name = name[:max(0, bytes.IndexByte(name, 0))]
			p += rl
			if len(name) == 0 || name[0] == '.' && (len(name) == 1 || len(name) == 2 && name[1] == '.') {
				continue
			}
			if typ == syscall.DT_UNKNOWN {
				if fi, err := os.Lstat(dir + "/" + string(name)); err == nil && fi.IsDir() {
					typ = syscall.DT_DIR
				}
			}
			names = append(append(names, name...), 0)
			dirs = append(dirs, typ == syscall.DT_DIR)
		}
	}
}

// fsMounts are the mount points inside the roots (/proc/self/mountinfo): the walk doesn't step into another
// filesystem, and needs no stat per folder to know (that was a quarter of its system calls).
func fsMounts(roots []string) map[string]bool {
	b, _ := os.ReadFile("/proc/self/mountinfo")
	unesc := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	out := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 4 {
			mp := unesc.Replace(f[4])
			for _, r := range roots {
				if strings.HasPrefix(mp, r+"/") {
					out[mp] = true
				}
			}
		}
	}
	return out
}

// fsBlock is one folder's listing; its entries' parent is entry pidx of block pseq (always an earlier block).
type fsBlock struct {
	pseq, pidx int32
	names      []byte
	dirs       []bool
	ct         int64 // the folder's ctime when listed; 0 if it had just changed (list it again next time)
}

// fsIndexMax caps a build: a flood of files (say in /tmp) stops the walk there, not the box's memory.
// ponytail: past it the rest goes unindexed; the cap is ~5× this box's 2.57M entries (~400 MB of index).
const fsIndexMax = 12_000_000

// fsSettle: a ctime this recent isn't trusted to mark a folder unchanged next time. Timestamps tick coarsely (a
// clock tick, several ms), so a second change in the same tick as the first leaves it the same.
var fsSettle = 2 * time.Second

// buildFsSnap walks the roots. With prev (the last build), a folder whose ctime hasn't moved since is only stat'd:
// its names come from prev. Adding, removing or renaming an entry moves a folder's ctime, and nothing sets it back.
func buildFsSnap(roots []string, prev *fsSnap) *fsSnap {
	var (
		mu     sync.Mutex
		blocks []*fsBlock
		wg     sync.WaitGroup
		count  atomic.Int64
	)
	add := func(b *fsBlock) int32 {
		mu.Lock()
		defer mu.Unlock()
		blocks = append(blocks, b)
		return int32(len(blocks) - 1)
	}
	top := &fsBlock{pseq: -1}
	for _, r := range roots {
		top.names, top.dirs = append(append(top.names, r...), 0), append(top.dirs, true)
	}
	add(top)
	// Each folder is listed by whichever goroutine finds it; a free slot takes a subfolder in parallel, else the
	// finder walks it itself, so the goroutines stay at NumCPU however the tree is shaped.
	sem := make(chan struct{}, runtime.NumCPU())
	mounts := fsMounts(roots) // the walk stays on each root's own filesystem
	settled := time.Now().Add(-fsSettle).UnixNano()
	var walk func(dir string, pseq, pidx, old int32)
	walk = func(dir string, pseq, pidx, old int32) { // old: this folder's entry in prev (-1: none)
		b := &fsBlock{pseq: pseq, pidx: pidx}
		var olds []int32 // each entry's own entry in prev, for its walk
		var st syscall.Stat_t
		first, n, ct, had := int32(0), int32(0), int64(0), false
		if prev != nil && old >= 0 {
			first, n, ct, had = prev.listed(old)
		}
		if had && ct != 0 && syscall.Lstat(dir, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFDIR && st.Ctim.Nano() == ct {
			b.ct = ct // unchanged: as it was listed (an original is as long as its lowercase)
			b.names, b.dirs, olds = make([]byte, 0, prev.off[first+n]-prev.off[first]), make([]bool, 0, n), make([]int32, 0, n)
			for i := first; i < first+n; i++ {
				b.names = append(append(b.names, prev.name(i)...), 0)
				b.dirs = append(b.dirs, prev.flags[i]&fsDir != 0)
				olds = append(olds, i)
			}
		} else {
			fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0) // a fifo swapped in can't block it
			if err != nil {
				return
			}
			if syscall.Fstat(fd, &st) == nil && st.Ctim.Nano() < settled { // before the listing: a change during it moves it on
				b.ct = st.Ctim.Nano()
			}
			buf := fsBufs.Get().(*[]byte)
			b.names, b.dirs = fsReadDir(fd, dir, *buf, nil, nil)
			fsBufs.Put(buf)
			syscall.Close(fd)
			if had { // its folders that were here before keep their entries in prev, for their own walks
				was := map[string]int32{}
				for i := first; i < first+n; i++ {
					if prev.flags[i]&fsDir != 0 {
						was[string(prev.name(i))] = i
					}
				}
				pos := 0
				for _, isDir := range b.dirs {
					end := pos + bytes.IndexByte(b.names[pos:], 0)
					id, ok := was[string(b.names[pos:end])]
					if !isDir || !ok {
						id = -1
					}
					olds = append(olds, id)
					pos = end + 1
				}
			}
		}
		if count.Add(int64(len(b.dirs))) > fsIndexMax {
			return
		}
		seq := add(b)
		pos := 0
		for i, isDir := range b.dirs {
			end := pos + bytes.IndexByte(b.names[pos:], 0)
			name := b.names[pos:end]
			pos = end + 1
			if !isDir {
				continue
			}
			sub := dir + "/" + string(name)
			if mounts[sub] {
				continue
			}
			o := int32(-1)
			if olds != nil {
				o = olds[i]
			}
			select {
			case sem <- struct{}{}:
				wg.Go(func() { walk(sub, seq, int32(i), o); <-sem })
			default:
				walk(sub, seq, int32(i), o)
			}
		}
	}
	for i, r := range roots {
		o := int32(-1)
		if prev != nil {
			o = int32(i) // the roots are the first entries of every build
		}
		wg.Go(func() { walk(r, 0, int32(i), o) })
	}
	wg.Wait()

	n, size := 0, 0
	for _, b := range blocks {
		n, size = n+len(b.dirs), size+len(b.names)
	}
	a := &fsArena{}
	sn := &fsSnap{lower: a.bytes(size), off: fsU32(a, n+1), parent: fsI32(a, n), flags: a.bytes(n), at: time.Now().UnixMilli(), arena: a}
	var up []byte // the originals, few: gathered on the heap, then copied out to the arena
	var upID []int32
	var upOff []uint32
	start := make([]int32, len(blocks))
	type listing struct {
		id, first, n int32
		ct           int64
	}
	lists := make([]listing, 0, len(blocks))
	base, e := 0, int32(0) // where the next block's names go; the next entry
	for s, b := range blocks {
		start[s] = e
		p, inherit := int32(-1), uint8(0)
		if b.pseq >= 0 {
			p = start[b.pseq] + b.pidx
			inherit = sn.flags[p] & (fsHidden | fsJunk)
			lists = append(lists, listing{p, e, int32(len(b.dirs)), b.ct})
		}
		lower := sn.lower[base : base+len(b.names)]
		copy(lower, b.names)
		lowerFold(lower)
		pos := 0
		for _, dir := range b.dirs {
			end := pos + bytes.IndexByte(b.names[pos:], 0)
			nm := b.names[pos:end]
			fl := inherit
			if !bytes.Equal(nm, lower[pos:end]) {
				fl |= fsUpper
				upID, upOff, up = append(upID, e), append(upOff, uint32(len(up))), append(up, nm...)
			}
			if dir {
				fl |= fsDir
			}
			if nm[0] == '.' {
				fl |= fsHidden
			}
			if fsJunkNames[string(nm)] { // "Trash": ~/.local/share/Trash
				fl |= fsJunk
			}
			sn.off[e], sn.parent[e], sn.flags[e] = uint32(base+pos), p, fl
			e++
			pos = end + 1
		}
		base += len(b.names)
		blocks[s] = nil
	}
	sn.off[e] = uint32(base)
	slices.SortFunc(lists, func(a, b listing) int { return int(a.id - b.id) })
	sn.dirID, sn.dirFirst, sn.dirN, sn.dirCt = fsI32(a, len(lists)), fsI32(a, len(lists)), fsI32(a, len(lists)), fsI64(a, len(lists))
	for j, l := range lists {
		sn.dirID[j], sn.dirFirst[j], sn.dirN[j], sn.dirCt[j] = l.id, l.first, l.n, l.ct
	}
	upOff = append(upOff, uint32(len(up)))
	sn.up, sn.upID, sn.upOff = a.bytes(len(up)), fsI32(a, len(upID)), fsU32(a, len(upOff))
	copy(sn.up, up)
	copy(sn.upID, upID)
	copy(sn.upOff, upOff)
	return sn
}

// lowerFold lowercases b in place wherever that keeps the byte length (ASCII, most accented letters), so a
// name's lowercase and original line up byte for byte.
func lowerFold(b []byte) {
	for i := 0; i < len(b); {
		if c := b[i]; c < utf8.RuneSelf {
			if 'A' <= c && c <= 'Z' {
				b[i] = c + 'a' - 'A'
			}
			i++
			continue
		}
		r, n := utf8.DecodeRune(b[i:])
		if l := unicode.ToLower(r); l != r && utf8.RuneLen(l) == n {
			utf8.EncodeRune(b[i:], l)
		}
		i += n
	}
}

// ---- queries ----

type fsQuery struct {
	terms  [][]byte // lowercased, longest first
	kind   string
	hidden bool
	in     int32 // only under this entry; -1: anywhere
	limit  int
}

type fsHit struct {
	id    int32
	score int
}

type fsHeap []fsHit // the worst kept hit on top

func (h fsHeap) Len() int           { return len(h) }
func (h fsHeap) Less(i, j int) bool { return h[i].score < h[j].score }
func (h fsHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *fsHeap) Push(x any)        { *h = append(*h, x.(fsHit)) }
func (h *fsHeap) Pop() any          { o := *h; x := o[len(o)-1]; *h = o[:len(o)-1]; return x }

func fsTerms(q string) [][]byte {
	var ts [][]byte
	for _, t := range strings.Fields(strings.ReplaceAll(q, "\x00", "")) {
		if len(ts) < 6 {
			b := []byte(t)
			lowerFold(b)
			ts = append(ts, b)
		}
	}
	slices.SortStableFunc(ts, func(a, b []byte) int { return len(b) - len(a) })
	return ts
}

// find returns the entry of a real path, or -1.
func (sn *fsSnap) find(p string) int32 {
	for r := int32(0); int(r) < len(sn.parent) && sn.parent[r] == -1; r++ {
		rest, ok := strings.CutPrefix(p, string(sn.name(r)))
		if !ok || (rest != "" && rest[0] != '/') {
			continue
		}
		id := r
		for _, c := range strings.Split(rest, "/")[1:] {
			if id = sn.child(id, c); id < 0 {
				break
			}
		}
		return id
	}
	return -1
}

// child scans for d's entry named name: a folder's entries are contiguous and come after it.
func (sn *fsSnap) child(d int32, name string) int32 {
	for i := d + 1; int(i) < len(sn.parent); i++ {
		if sn.parent[i] == d && string(sn.name(i)) == name {
			return i
		}
	}
	return -1
}

// search finds the entries whose name holds a term and whose path holds every other one, best first.
// The top ~limit are ranked by name; the caller adds what only a stat knows (gone, recent).
func (sn *fsSnap) search(q fsQuery) ([]fsHit, int) {
	if len(q.terms) == 0 {
		return nil, 0
	}
	n := int32(len(sn.parent))
	workers := int32(runtime.NumCPU())
	heaps, totals := make([]fsHeap, workers), make([]int, workers)
	var wg sync.WaitGroup
	for w := range workers {
		lo, hi := n*w/workers, n*(w+1)/workers
		wg.Go(func() { heaps[w], totals[w] = sn.scan(q, lo, hi) })
	}
	wg.Wait()
	var all []fsHit
	for w := range workers {
		all = append(all, heaps[w]...)
		totals[0] += totals[w]
	}
	slices.SortFunc(all, func(a, b fsHit) int { return b.score - a.score })
	return all[:min(len(all), q.limit)], totals[0]
}

// scan looks for each term in the names of entries [lo, hi). An entry is found by the first of the terms its own
// name holds, so it counts once.
func (sn *fsSnap) scan(q fsQuery, lo, hi int32) (fsHeap, int) {
	var h fsHeap
	total := 0
	end := sn.off[hi]
	for ti, t := range q.terms {
		id := lo
		for pos := sn.off[lo]; pos < end; pos = sn.off[id+1] {
			i := bytes.Index(sn.lower[pos:end], t)
			if i < 0 {
				break
			}
			for at := pos + uint32(i); sn.off[id+1] <= at; id++ {
			}
			nm := sn.lname(id)
			if slices.ContainsFunc(q.terms[:ti], func(e []byte) bool { return bytes.Contains(nm, e) }) {
				continue
			}
			s, ok := sn.score(q, id, ti)
			if !ok {
				continue
			}
			total++
			if len(h) < q.limit {
				heap.Push(&h, fsHit{id, s})
			} else if s > h[0].score {
				h[0] = fsHit{id, s}
				heap.Fix(&h, 0)
			}
		}
	}
	return h, total
}

// score ranks entry id, found by term ti in its name; false when a filter or another term rules it out.
func (sn *fsSnap) score(q fsQuery, id int32, ti int) (int, bool) {
	fl := sn.flags[id]
	nm, orig, t := sn.lname(id), sn.name(id), q.terms[ti]
	if fl&fsHidden != 0 && !q.hidden {
		return 0, false
	}
	switch q.kind {
	case "":
	case "folders":
		if fl&fsDir == 0 {
			return 0, false
		}
	default:
		dot := bytes.LastIndexByte(nm, '.')
		if fl&fsDir != 0 || dot < 0 || fsKindOf[string(nm[dot+1:])] != q.kind {
			return 0, false
		}
	}
	depth, under := 0, q.in < 0
	for p := sn.parent[id]; p >= 0; p = sn.parent[p] {
		depth++
		under = under || p == q.in
	}
	if !under {
		return 0, false
	}
	s := 150
	switch {
	case len(nm) == len(t):
		s = 1000
	case bytes.HasPrefix(nm, t):
		s = 400
	default:
		for i := bytes.Index(nm, t); i > 0; {
			if c := orig[i-1]; c == '-' || c == '_' || c == '.' || c == ' ' || ('a' <= c && c <= 'z' && 'A' <= orig[i] && orig[i] <= 'Z') {
				s = 250
				break
			}
			j := bytes.Index(nm[i+1:], t)
			if j < 0 {
				break
			}
			i += j + 1
		}
	}
	for j, o := range q.terms {
		if j == ti {
			continue
		}
		if bytes.Contains(nm, o) {
			s += 120
			continue
		}
		p := sn.parent[id]
		for ; p >= 0 && !bytes.Contains(sn.lname(p), o); p = sn.parent[p] {
		}
		if p < 0 {
			return 0, false
		}
		s += 30
	}
	s += -3*depth - len(orig)/4
	if fl&fsJunk != 0 {
		s -= 700 // below any real prefix match, even as an exact one: "py" isn't node_modules/…/PY
	}
	if fl&fsHidden != 0 {
		s -= 40
	}
	if fl&fsDir != 0 {
		s += 10
	}
	return s, true
}
