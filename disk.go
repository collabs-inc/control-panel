package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---- disk usage: a cached du index of home, drill-down views, deletion ----
//
// du over all of home takes seconds of IO and prints ~200k folders, so it runs rarely (nice'd, idle IO) and
// only folders of duFloor and up are kept (~14k). The index is saved to disk, so a restart shows the last
// scan at once. A view adds the folder's files (and its sub-megabyte folders) live.

const duFloor = 1 << 20

type duIndex struct {
	mu        sync.Mutex
	root      string
	file      string
	size      map[string]int64    // folder → bytes
	kids      map[string][]string // folder → indexed subfolders, largest first
	at        int64
	running   bool
	forgotten []string // deleted while a scan ran: the scan may still have seen them
	deleting  map[string]bool
	scan      func(root string) (map[string]int64, error)
}

func newDuIndex(root, file string) *duIndex {
	x := &duIndex{root: root, file: file, deleting: map[string]bool{}, scan: duScan}
	x.load()
	return x
}

func duScan(root string) (map[string]int64, error) {
	cmd := exec.Command("nice", "-n19", "ionice", "-c3", "du", "-x", "-B1", root)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	m := parseDuStream(out, root)
	cmd.Wait() // du exits 1 on unreadable folders but still prints the rest
	if len(m) == 0 {
		return nil, errors.New("du printed nothing")
	}
	return m, nil
}

func parseDuStream(r io.Reader, root string) map[string]int64 {
	m := map[string]int64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		n, p, ok := strings.Cut(sc.Text(), "\t")
		v, err := strconv.ParseInt(n, 10, 64)
		if !ok || err != nil || (v < duFloor && p != root) || (p != root && !strings.HasPrefix(p, root+"/")) {
			continue
		}
		m[p] = v
	}
	return m
}

func (x *duIndex) setLocked(m map[string]int64, at int64) {
	x.size, x.at = m, at
	x.kids = map[string][]string{}
	for p := range m {
		if p != x.root {
			x.kids[filepath.Dir(p)] = append(x.kids[filepath.Dir(p)], p)
		}
	}
	for _, ks := range x.kids {
		sort.Slice(ks, func(i, j int) bool { return m[ks[i]] > m[ks[j]] })
	}
}

// refresh starts a background scan when the index is over 10 minutes old (or now, if forced).
func (x *duIndex) refresh(now time.Time, force bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.running || (!force && now.Unix()-x.at < 600) {
		return
	}
	x.running, x.forgotten = true, nil
	go func() {
		m, err := x.scan(x.root)
		x.mu.Lock()
		if err == nil {
			x.finishScan(m, time.Now().Unix())
		} else {
			x.running = false
		}
		x.mu.Unlock()
		if err == nil {
			x.save()
		}
	}()
}

// finishScan installs a scan's result, minus what was deleted while it ran (call with mu held).
func (x *duIndex) finishScan(m map[string]int64, at int64) {
	for _, p := range x.forgotten {
		if v, ok := m[p]; ok {
			dropLocked(m, x.root, p, v)
		}
	}
	x.setLocked(m, at)
	x.running, x.forgotten = false, nil
}

// dropLocked removes p and everything under it from m and takes its bytes off its ancestors.
func dropLocked(m map[string]int64, root, p string, bytes int64) {
	for q := range m {
		if q == p || strings.HasPrefix(q, p+"/") {
			delete(m, q)
		}
	}
	for a := filepath.Dir(p); ; a = filepath.Dir(a) {
		if v, ok := m[a]; ok {
			m[a] = max(0, v-bytes)
		}
		if a == root || len(a) <= len(root) {
			break
		}
	}
}

// forget takes a deleted path out of the index (and out of a scan in flight).
func (x *duIndex) forget(p string, bytes int64) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.size == nil {
		return
	}
	dropLocked(x.size, x.root, p, bytes)
	x.setLocked(x.size, x.at)
	if x.running {
		x.forgotten = append(x.forgotten, p)
	}
}

type duFile struct {
	Root string           `json:"root"`
	At   int64            `json:"at"`
	Dirs map[string]int64 `json:"dirs"` // relative to Root; "" is Root
}

func (x *duIndex) save() {
	x.mu.Lock()
	f := duFile{Root: x.root, At: x.at, Dirs: make(map[string]int64, len(x.size))}
	for p, v := range x.size {
		f.Dirs[strings.TrimPrefix(strings.TrimPrefix(p, x.root), "/")] = v
	}
	x.mu.Unlock()
	b, err := json.Marshal(f)
	if err != nil || os.MkdirAll(filepath.Dir(x.file), 0o700) != nil {
		return
	}
	writeFileAtomic(x.file, b, 0o600)
}

func (x *duIndex) load() {
	b, err := os.ReadFile(x.file)
	var f duFile
	if err != nil || json.Unmarshal(b, &f) != nil || f.Root != x.root || len(f.Dirs) == 0 {
		return
	}
	m := make(map[string]int64, len(f.Dirs))
	for rel, v := range f.Dirs {
		m[filepath.Join(x.root, rel)] = v
	}
	x.mu.Lock()
	x.setLocked(m, f.At)
	x.mu.Unlock()
}

// ---- views ----

type DiskKid struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

type DiskItem struct {
	Name     string    `json:"name"`
	Dir      bool      `json:"dir,omitempty"`
	Link     bool      `json:"link,omitempty"`
	Bytes    int64     `json:"bytes"`
	Small    bool      `json:"small,omitempty"` // a folder under 1 MB: not indexed, so no size
	Kids     []DiskKid `json:"kids,omitempty"`  // its largest subfolders, for the chart's outer ring
	Deleting bool      `json:"deleting,omitempty"`
}

type DiskView struct {
	Root     string     `json:"root"`
	Path     string     `json:"path"`
	Bytes    int64      `json:"bytes"`
	Items    []DiskItem `json:"items"`
	Other    int64      `json:"other"` // what the listed items don't account for (smaller files and folders)
	OtherN   int        `json:"otherN"`
	At       int64      `json:"at"`
	Scanning bool       `json:"scanning"`
	Live     bool       `json:"live,omitempty"` // measured just now: the index doesn't hold this folder
}

func diskBytes(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}

// duLive measures a folder the index doesn't hold (new, or under 1 MB): itself and its subfolders.
func duLive(p string) map[string]int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "nice", "-n19", "du", "-x", "-B1", "-d1", p).Output()
	m := map[string]int64{}
	for _, l := range strings.Split(string(out), "\n") {
		n, q, ok := strings.Cut(l, "\t")
		if v, err := strconv.ParseInt(n, 10, 64); ok && err == nil {
			m[q] = v
		}
	}
	return m
}

// measureSmall sizes the subfolders the index doesn't hold (under 1 MB at the last scan, or made since)
// in one du call, so a folder created a minute ago shows its real size. Too many, or too slow: they stay "< 1 MB".
func measureSmall(p string, items []DiskItem) {
	var args []string
	for _, it := range items {
		if it.Small {
			args = append(args, filepath.Join(p, it.Name))
		}
	}
	if len(args) == 0 || len(args) > 300 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "nice", append([]string{"-n19", "du", "-x", "-B1", "-s", "--"}, args...)...).Output()
	got := map[string]int64{}
	for _, l := range strings.Split(string(out), "\n") {
		if n, q, ok := strings.Cut(l, "\t"); ok {
			if v, err := strconv.ParseInt(n, 10, 64); err == nil {
				got[filepath.Base(q)] = v
			}
		}
	}
	for i := range items {
		if v, ok := got[items[i].Name]; ok && items[i].Small {
			items[i].Bytes, items[i].Small = v, false
		}
	}
}

func (x *duIndex) within(p string) bool { return p == x.root || strings.HasPrefix(p, x.root+"/") }

func (x *duIndex) view(p string) (DiskView, error) {
	if filepath.Clean(p) != p || !x.within(p) {
		return DiskView{}, fmt.Errorf("only folders inside %s", tilde(x.root))
	}
	if fi, err := os.Lstat(p); err != nil || !fi.IsDir() {
		return DiskView{}, errors.New("no such folder")
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return DiskView{}, err
	}
	x.mu.Lock()
	total, indexed := x.size[p]
	v := DiskView{Root: x.root, Path: p, At: x.at, Scanning: x.running} // read together: a live view mid-scan keeps the client polling
	x.mu.Unlock()
	var live map[string]int64
	if !indexed {
		live, v.Live = duLive(p), true
		total = live[p]
	}
	items := make([]DiskItem, 0, len(ents))
	x.mu.Lock()
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(p, e.Name())
		it := DiskItem{Name: e.Name(), Deleting: x.deleting[full]}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			it.Link, it.Bytes = true, diskBytes(fi)
		case fi.IsDir():
			it.Dir = true
			if b, ok := x.size[full]; ok {
				it.Bytes = b
				for _, k := range x.kids[full][:min(len(x.kids[full]), 12)] {
					it.Kids = append(it.Kids, DiskKid{filepath.Base(k), x.size[k]})
				}
			} else if b, ok := live[full]; ok {
				it.Bytes = b
			} else {
				it.Small = true
			}
		default:
			it.Bytes = diskBytes(fi)
		}
		items = append(items, it)
	}
	x.mu.Unlock()
	measureSmall(p, items)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Bytes != items[j].Bytes {
			return items[i].Bytes > items[j].Bytes
		}
		return items[i].Name < items[j].Name
	})
	v.Items = items[:min(len(items), 60)]
	var listed int64
	for _, it := range v.Items {
		listed += it.Bytes
	}
	v.Bytes = max(total, listed) // files written since the scan
	v.Other, v.OtherN = v.Bytes-listed, len(items)-len(v.Items)
	return v, nil
}

// ---- deletion ----

// deleteGuard: protect holds paths that keep the panel, its tunnel, SSH and Claude working (they, what's
// in them and what holds them can't be deleted); in ciDir only a runner's _work and _diag can go.
type deleteGuard struct {
	protect []string
	ciDir   string
}

// protected refuses a path that is, holds or sits in something the panel, its tunnel, SSH, Claude or a CI runner needs.
func (g deleteGuard) protected(p string) error {
	for _, q := range g.protect {
		if p == q || strings.HasPrefix(q, p+"/") || strings.HasPrefix(p, q+"/") {
			return fmt.Errorf("%s is protected: the panel, its tunnel, SSH or Claude need it", tilde(q))
		}
	}
	if g.ciDir != "" && (p == g.ciDir || strings.HasPrefix(g.ciDir, p+"/") || strings.HasPrefix(p, g.ciDir+"/")) {
		parts := strings.Split(strings.TrimPrefix(p, g.ciDir+"/"), "/")
		if p == g.ciDir || strings.HasPrefix(g.ciDir, p+"/") || len(parts) < 2 || (parts[1] != "_work" && parts[1] != "_diag") {
			return errors.New("CI runners need their own files: only a runner's _work and _diag can be cleaned")
		}
	}
	return nil
}

func (x *duIndex) checkDelete(p string, g deleteGuard) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return errors.New("not a clean absolute path")
	}
	if !strings.HasPrefix(p, x.root+"/") {
		return fmt.Errorf("only paths inside %s", tilde(x.root))
	}
	if real, err := filepath.EvalSymlinks(filepath.Dir(p)); err != nil || real != filepath.Dir(p) {
		return errors.New("the path goes through a symlinked folder")
	}
	if err := g.protected(p); err != nil {
		return err
	}
	if _, err := os.Lstat(p); err != nil {
		return errors.New("nothing at that path")
	}
	return nil
}

// sizeOf is what deleting p frees: the index for big folders, du for small ones, blocks for the rest.
func (x *duIndex) sizeOf(p string) int64 {
	x.mu.Lock()
	b, ok := x.size[p]
	x.mu.Unlock()
	if ok {
		return b
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return 0
	}
	if fi.IsDir() {
		return duLive(p)[p]
	}
	return diskBytes(fi)
}

// remove deletes p in the background (big trees take a while) and reports when it's done.
func (x *duIndex) remove(p string, done func(freed int64, err error)) error {
	x.mu.Lock()
	if x.deleting[p] {
		x.mu.Unlock()
		return errors.New("already being deleted")
	}
	x.deleting[p] = true
	x.mu.Unlock()
	go func() {
		freed := x.sizeOf(p)
		err := os.RemoveAll(p)
		x.mu.Lock()
		delete(x.deleting, p)
		x.mu.Unlock()
		if err != nil {
			x.refresh(time.Now(), true) // partly gone: measure again
		} else {
			x.forget(p, freed)
			x.save()
		}
		done(freed, err)
	}()
	return nil
}

// inUse names the processes working in p (their cwd) or holding a file open in it.
func inUse(procRoot, p string) []string {
	ents, _ := os.ReadDir(procRoot)
	var out []string
	inside := func(l string) bool { return l == p || strings.HasPrefix(l, p+"/") }
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		hit := false
		if l, err := os.Readlink(filepath.Join(dir, "cwd")); err == nil && inside(l) {
			hit = true
		}
		if !hit {
			fds, _ := os.ReadDir(filepath.Join(dir, "fd"))
			for _, fd := range fds {
				if l, err := os.Readlink(filepath.Join(dir, "fd", fd.Name())); err == nil && inside(l) {
					hit = true
					break
				}
			}
		}
		if hit {
			comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
			out = append(out, fmt.Sprintf("%s (%d)", strings.TrimSpace(string(comm)), pid))
		}
	}
	return out
}

// ---- endpoints ----

// secretPaths are never served as session files, whatever a message names.
func (s *server) secretPaths() []string {
	h := s.cfg.home
	return []string{filepath.Join(h, ".ssh"), filepath.Join(h, ".cloudflared"), filepath.Dir(s.cfg.pinFile),
		filepath.Join(h, ".claude/.credentials.json"), filepath.Join(h, ".claude.json"), filepath.Join(h, ".config/gh")}
}

func (s *server) deleteGuard() deleteGuard {
	h := s.cfg.home
	g := deleteGuard{ciDir: s.cfg.ciDir, protect: []string{
		filepath.Join(h, ".ssh"), filepath.Join(h, ".cloudflared"), filepath.Dir(s.cfg.pinFile), filepath.Dir(s.cfg.auditFile),
		filepath.Join(h, ".claude/sessions"), filepath.Join(h, ".claude/settings.json"), filepath.Join(h, ".claude/.credentials.json"),
		filepath.Join(h, ".claude.json"), filepath.Join(h, ".local/bin"), filepath.Join(h, ".zshrc"),
	}}
	if exe, err := os.Executable(); err == nil {
		g.protect = append(g.protect, filepath.Dir(exe)) // the panel's own folder: ensure-panel.sh rebuilds from it
	}
	real := func(p string) string { // Files checks resolved paths: a symlinked home must still match
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	for i, p := range g.protect {
		g.protect[i] = real(p)
	}
	if g.ciDir != "" {
		g.ciDir = real(g.ciDir)
	}
	return g
}

func (s *server) diskView(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		p = s.du.root
	}
	s.du.refresh(time.Now(), r.URL.Query().Get("rescan") == "1")
	v, err := s.du.view(p)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, v)
}

// diskCheck answers "can this go, how big is it, who is using it" before the confirmation.
func (s *server) diskCheck(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if err := s.du.checkDelete(p, s.deleteGuard()); err != nil {
		writeJSON(w, 403, map[string]any{"error": err.Error()})
		return
	}
	users := inUse("/proc", p)
	writeJSON(w, 200, map[string]any{"bytes": s.du.sizeOf(p), "users": users[:min(len(users), 8)], "nUsers": len(users)})
}

func (s *server) diskDelete(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Path string `json:"path"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&b) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	if err := s.du.checkDelete(b.Path, s.deleteGuard()); err != nil {
		writeJSON(w, 403, map[string]any{"error": err.Error()})
		return
	}
	ip := clientIP(r)
	err := s.du.remove(b.Path, func(freed int64, err error) {
		if err != nil {
			s.audit.add("delete-fail", tilde(b.Path)+": "+err.Error(), ip)
		} else {
			s.audit.add("delete", fmt.Sprintf("%s (%s)", tilde(b.Path), fmtBytes(freed)), ip)
		}
	})
	if err != nil {
		writeJSON(w, 409, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func fmtBytes(n int64) string {
	u := []string{"B", "KB", "MB", "GB", "TB"}
	f, i := float64(n), 0
	for f >= 1024 && i < len(u)-1 {
		f, i = f/1024, i+1
	}
	if i > 0 && f < 10 {
		return strconv.FormatFloat(f, 'f', 1, 64) + " " + u[i]
	}
	return strconv.FormatFloat(f, 'f', 0, 64) + " " + u[i]
}
