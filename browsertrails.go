package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Trails: what each agent's browser did, kept a week (1 GB in all at most, the oldest first; a pinned one stays), so
// the owner can replay a session or compare two, and an agent can look up what the others already saw (cube-browser
// history, in the wrapper) instead of opening the same pages again. One folder per Chromium run of a session,
// trails/<launch>/<run>/:
//   - meta.json: who (the agent's pane, its project), when, how much;
//   - events.jsonl: its commands, navigations, page loads, console errors and failed requests, in order;
//   - shots/<n>.jpg: the page after each command and each load;
//   - motion.bin: the page as it moved, at most 2 frames a second (records: 8-byte ms time, 4-byte length, JPEG);
//   - calls/<n>.json: its pages' API calls, sent and received (browsercalls.go).
// trails/visits.jsonl lists every page load of every trail: the wrapper's history reads only that, and the trail's
// events for the visits it shows.

const (
	brTrailKeep      = 7 * 24 * time.Hour
	brTrailsMax      = 1 << 30
	brTrailMotionMax = 200 << 20 // one run's motion
	brTrailFrameGap  = 500 * time.Millisecond
	brTrailErrsMax   = 300 // console and network events one run keeps
)

var brTrailIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}/\d{8}-\d{6}-\d{1,10}$`)

type btrail struct {
	id        string // "<launch>/<run>"
	dir       string
	vis       string // visits.jsonl
	mu        sync.Mutex
	ev        *os.File
	mo        *os.File
	moN       int64
	moAt      time.Time
	errs      int
	meta      btrailMeta
	dirty     bool
	done      bool
	callBytes int64            // its API calls' files (browsercalls.go)
	start     time.Time        // its Chromium's start: commands that ended before it are an earlier run's
	seen      map[string]uint8 // the commands written: 1 its start, 2 its end (read back when the trail is reopened)
}

type btrailMeta struct {
	ID      string `json:"id"`
	Launch  string `json:"launch"`
	Label   string `json:"label,omitempty"`
	Sub     string `json:"sub,omitempty"`
	Root    string `json:"root,omitempty"`
	Project string `json:"project,omitempty"`
	Conv    string `json:"conv,omitempty"`
	Pane    *bpane `json:"pane,omitempty"`
	Started int64  `json:"started"`
	Ended   int64  `json:"ended,omitempty"`
	Pinned  bool   `json:"pinned,omitempty"`
	Steps   int    `json:"steps"`
	Pages   int    `json:"pages"`
	Errors  int    `json:"errors"`
	Shots   int    `json:"shots"`
	Calls   int    `json:"calls,omitempty"` // API calls kept (browsercalls.go)
	Last    string `json:"last,omitempty"`  // the last page loaded
	Title   string `json:"title,omitempty"` // and its title
	Live    bool   `json:"live,omitempty"`  // (in listings: still recording)
	Bytes   int64  `json:"bytes,omitempty"` // (in listings)
}

func (b *browser) trailsDir() string { return filepath.Join(b.agentDir(), "trails") }

// openTrail starts the trail of an agent's Chromium that just connected.
func (b *browser) openTrail(a *bagent) *btrail {
	launch := a.key
	if launch == "" || strings.ContainsAny(launch, "/\\.") {
		launch = "pid-" + a.id
	}
	// named by its Chromium's start: a panel restarted while it runs carries on with the same trail
	start := a.started
	if start.IsZero() {
		start = time.Now()
	}
	id := launch + "/" + start.Format("20060102-150405") + "-" + a.id
	tr := &btrail{id: id, dir: filepath.Join(b.trailsDir(), id), vis: filepath.Join(b.trailsDir(), "visits.jsonl"), start: start, seen: map[string]uint8{}}
	if os.MkdirAll(filepath.Join(tr.dir, "shots"), 0o700) != nil {
		return nil
	}
	var was btrailMeta
	if raw, err := os.ReadFile(filepath.Join(tr.dir, "meta.json")); err == nil && json.Unmarshal(raw, &was) == nil && was.ID == id {
		tr.meta, tr.meta.Ended = was, 0
		if f, err := os.Open(filepath.Join(tr.dir, "events.jsonl")); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for sc.Scan() {
				var e struct{ K, ID string }
				if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID != "" {
					tr.seen[e.ID] |= map[string]uint8{"step": 1, "done": 2}[e.K]
				}
			}
			f.Close()
		}
	}
	var err error
	if tr.ev, err = os.OpenFile(filepath.Join(tr.dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
		return nil
	}
	tr.mo, _ = os.OpenFile(filepath.Join(tr.dir, "motion.bin"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if fi, err := os.Stat(filepath.Join(tr.dir, "motion.bin")); err == nil {
		tr.moN = fi.Size()
	}
	if tr.meta.ID == "" {
		tr.meta = btrailMeta{ID: id, Launch: launch, Root: tilde(a.root), Project: projectOf(cmpOr(a.root, a.cwd)), Conv: a.conv, Started: start.Unix()}
	}
	if a.zs != "" && a.zp >= 0 {
		tr.meta.Pane = &bpane{Session: a.zs, Pane: a.zp}
	}
	tr.writeMeta()
	return tr
}

// add appends an event: k is its kind (step, done, tab, nav, load, shot, con, net).
func (tr *btrail) add(k string, f map[string]any) {
	if tr == nil {
		return
	}
	f["k"] = k
	if f["t"] == nil {
		f["t"] = time.Now().UnixMilli()
	}
	raw, _ := json.Marshal(f)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.done {
		return
	}
	switch k {
	case "step":
		tr.meta.Steps++
	case "load":
		tr.meta.Pages++
		tr.meta.Last, _ = f["url"].(string)
		tr.meta.Title, _ = f["title"].(string)
	case "con", "net":
		if tr.errs >= brTrailErrsMax {
			return
		}
		tr.errs++
		tr.meta.Errors++
	}
	tr.dirty = true
	tr.ev.Write(append(raw, '\n'))
}

// shot keeps a picture of the page; its number, 0 when it wasn't kept.
func (tr *btrail) shot(img []byte) int {
	if tr == nil || len(img) == 0 {
		return 0
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.done {
		return 0
	}
	n := tr.meta.Shots + 1
	if os.WriteFile(filepath.Join(tr.dir, "shots", strconv.Itoa(n)+".jpg"), img, 0o600) != nil {
		return 0
	}
	tr.meta.Shots, tr.dirty = n, true
	return n
}

// frame keeps a frame of the moving page, at most two a second.
func (tr *btrail) frame(img []byte) {
	if tr == nil || tr.mo == nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	now := time.Now()
	if tr.done || now.Sub(tr.moAt) < brTrailFrameGap || tr.moN+int64(len(img)) > brTrailMotionMax {
		return
	}
	tr.moAt = now
	var h [12]byte
	binary.BigEndian.PutUint64(h[:8], uint64(now.UnixMilli()))
	binary.BigEndian.PutUint32(h[8:], uint32(len(img)))
	if n, err := tr.mo.Write(append(h[:], img...)); err == nil {
		tr.moN += int64(n)
	}
}

// visit records a page load for the wrapper's history.
func (tr *btrail) visit(url, title string, shot int) {
	raw, _ := json.Marshal(map[string]any{"t": time.Now().UnixMilli(), "trail": tr.id, "url": url, "title": title, "shot": shot})
	if f, err := os.OpenFile(tr.vis, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		f.Write(append(raw, '\n'))
		f.Close()
	}
}

func (tr *btrail) setWho(label, sub string, pane *bpane) {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.meta.Label != label || tr.meta.Sub != sub || (pane != nil && (tr.meta.Pane == nil || *tr.meta.Pane != *pane)) {
		tr.meta.Label, tr.meta.Sub, tr.dirty = label, sub, true
		if pane != nil {
			tr.meta.Pane = pane
		}
	}
}

func (tr *btrail) writeMeta() {
	raw, _ := json.Marshal(tr.meta)
	tmp := filepath.Join(tr.dir, "meta.json.tmp")
	if os.WriteFile(tmp, raw, 0o600) == nil {
		os.Rename(tmp, filepath.Join(tr.dir, "meta.json"))
	}
	tr.dirty = false
}

// flush writes the meta when it changed (the scan, every second).
func (tr *btrail) flush() {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.dirty && !tr.done {
		tr.writeMeta()
	}
}

func (tr *btrail) close() {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.done {
		return
	}
	tr.meta.Ended = time.Now().Unix()
	tr.writeMeta()
	tr.done = true
	tr.ev.Close()
	if tr.mo != nil {
		tr.mo.Close()
	}
}

// ---- keeping: a week, 1 GB ----

type btrailDisk struct {
	dir   string
	meta  btrailMeta
	bytes int64
	last  time.Time
}

func (b *browser) trailsOnDisk() []btrailDisk {
	var out []btrailDisk
	root := b.trailsDir()
	metas, _ := filepath.Glob(filepath.Join(root, "*", "*", "meta.json"))
	for _, m := range metas {
		d := filepath.Dir(m)
		td := btrailDisk{dir: d}
		raw, err := os.ReadFile(m)
		if err != nil || json.Unmarshal(raw, &td.meta) != nil {
			continue
		}
		filepath.WalkDir(d, func(_ string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				if fi, err := e.Info(); err == nil {
					td.bytes += fi.Size()
					if fi.ModTime().After(td.last) {
						td.last = fi.ModTime()
					}
				}
			}
			return nil
		})
		out = append(out, td)
	}
	return out
}

// trailsSweep deletes trails older than a week (by their last write), then the oldest while all of them pass 1 GB;
// never a pinned one, never one still recording. The visits and API calls of the deleted ones go too.
func (b *browser) trailsSweep() {
	b.mu.Lock()
	live := map[string]bool{}
	for _, src := range b.agents {
		if tr := src.agent.trail; tr != nil {
			live[tr.id] = true
		}
	}
	b.mu.Unlock()
	all := b.trailsOnDisk()
	slices.SortFunc(all, func(x, y btrailDisk) int { return x.last.Compare(y.last) })
	var total int64
	for _, t := range all {
		total += t.bytes
	}
	gone := map[string]bool{}
	for _, t := range all {
		if t.meta.Pinned || live[t.meta.ID] {
			continue
		}
		if time.Since(t.last) > brTrailKeep || total > brTrailsMax {
			if os.RemoveAll(t.dir) == nil {
				total -= t.bytes
				gone[t.meta.ID] = true
				os.Remove(filepath.Dir(t.dir)) // the session's folder, when that was its last run
			}
		}
	}
	for _, name := range []string{"visits.jsonl", "calls.jsonl"} {
		idx := filepath.Join(b.trailsDir(), name)
		raw, err := os.ReadFile(idx)
		if err != nil {
			continue
		}
		var keep bytes.Buffer
		cut := time.Now().Add(-brTrailKeep).UnixMilli()
		for _, line := range bytes.Split(raw, []byte("\n")) {
			var v struct {
				T     int64
				Trail string
			}
			if json.Unmarshal(line, &v) == nil && !gone[v.Trail] && v.T >= cut {
				keep.Write(line)
				keep.WriteByte('\n')
			}
		}
		if keep.Len() != len(raw) {
			os.WriteFile(idx+".tmp", keep.Bytes(), 0o600)
			os.Rename(idx+".tmp", idx)
		}
	}
}

// ---- the page's API ----

func (b *browser) trailDir(id string) (string, bool) {
	if !brTrailIDRe.MatchString(id) {
		return "", false
	}
	return filepath.Join(b.trailsDir(), id), true
}

// trails lists them, newest first: the live ones' meta as it is now.
func (b *browser) trails(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	live := map[string]btrailMeta{}
	for _, src := range b.agents {
		if tr := src.agent.trail; tr != nil {
			tr.mu.Lock()
			live[tr.id] = tr.meta
			tr.mu.Unlock()
		}
	}
	b.mu.Unlock()
	out := []btrailMeta{}
	var total int64
	for _, t := range b.trailsOnDisk() {
		m := t.meta
		if lm, ok := live[m.ID]; ok {
			m, m.Live = lm, true
		}
		m.Bytes = t.bytes
		total += t.bytes
		out = append(out, m)
	}
	slices.SortFunc(out, func(x, y btrailMeta) int { return int(y.Started - x.Started) })
	writeJSON(w, 200, map[string]any{"trails": out, "bytes": total, "max": brTrailsMax, "keepDays": int(brTrailKeep / (24 * time.Hour))})
}

type btrailFrame struct {
	T   int64 `json:"t"`
	Off int64 `json:"off"`
	Len int   `json:"len"`
}

// trail: one trail's meta, events and the index of its motion frames.
func (b *browser) trail(w http.ResponseWriter, r *http.Request) {
	dir, ok := b.trailDir(r.URL.Query().Get("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var meta btrailMeta
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil || json.Unmarshal(raw, &meta) != nil {
		http.NotFound(w, r)
		return
	}
	events := []json.RawMessage{}
	if f, err := os.Open(filepath.Join(dir, "events.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() && len(events) < 20000 {
			if line := sc.Bytes(); json.Valid(line) {
				events = append(events, json.RawMessage(slices.Clone(line)))
			}
		}
		f.Close()
	}
	frames := []btrailFrame{}
	if f, err := os.Open(filepath.Join(dir, "motion.bin")); err == nil {
		var h [12]byte
		var off int64
		for {
			if _, err := io.ReadFull(f, h[:]); err != nil {
				break
			}
			n := int(binary.BigEndian.Uint32(h[8:]))
			frames = append(frames, btrailFrame{T: int64(binary.BigEndian.Uint64(h[:8])), Off: off + 12, Len: n})
			off += 12 + int64(n)
			if _, err := f.Seek(off, io.SeekStart); err != nil {
				break
			}
		}
		f.Close()
	}
	writeJSON(w, 200, map[string]any{"meta": meta, "events": events, "frames": frames})
}

// trailShot serves a picture of a trail: a shot (n) or a motion frame (off, len).
func (b *browser) trailShot(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dir, ok := b.trailDir(q.Get("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var img []byte
	if n, err := strconv.Atoi(q.Get("n")); err == nil && n > 0 {
		img, _ = os.ReadFile(filepath.Join(dir, "shots", strconv.Itoa(n)+".jpg"))
	} else if off, err := strconv.ParseInt(q.Get("off"), 10, 64); err == nil && off >= 12 {
		ln, _ := strconv.Atoi(q.Get("len"))
		if f, err := os.Open(filepath.Join(dir, "motion.bin")); err == nil && ln > 0 && ln <= 8<<20 {
			buf := make([]byte, ln)
			if _, err := f.ReadAt(buf, off); err == nil && len(buf) > 2 && buf[0] == 0xFF && buf[1] == 0xD8 {
				img = buf
			}
			f.Close()
		}
	}
	if img == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=604800, immutable")
	w.Write(img)
}

// trailEdit pins (keeps past the week and the 1 GB), unpins or deletes a trail.
func (b *browser) trailEdit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID, Do string
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	dir, ok := b.trailDir(body.ID)
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "no such trail"})
		return
	}
	b.mu.Lock()
	var live *btrail
	for _, src := range b.agents {
		if tr := src.agent.trail; tr != nil && tr.id == body.ID {
			live = tr
		}
	}
	b.mu.Unlock()
	switch body.Do {
	case "pin", "unpin":
		if live != nil {
			live.mu.Lock()
			live.meta.Pinned, live.dirty = body.Do == "pin", true
			live.mu.Unlock()
			live.flush()
			break
		}
		var meta btrailMeta
		raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil || json.Unmarshal(raw, &meta) != nil {
			writeJSON(w, 404, map[string]any{"error": "no such trail"})
			return
		}
		meta.Pinned = body.Do == "pin"
		raw, _ = json.Marshal(meta)
		os.WriteFile(filepath.Join(dir, "meta.json"), raw, 0o600)
	case "delete":
		if live != nil {
			writeJSON(w, 409, map[string]any{"error": "it's still recording: delete it when the session ends"})
			return
		}
		os.RemoveAll(dir)
		os.Remove(filepath.Dir(dir))
	default:
		writeJSON(w, 400, map[string]any{"error": "pin, unpin or delete"})
		return
	}
	if b.s != nil {
		b.s.audit.add("browser-trail", body.Do+" "+body.ID, clientIP(r))
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
