package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The agents' browsers, watched. An agent's cube-browser (the Cube image's Playwright tool) starts its own headless
// Chromium; the panel puts a wrapper first on PATH (~/.local/bin/cube-browser, below) that points it at a mirror of
// the image's browsers whose chrome adds --remote-debugging-port=0 and registers itself: <pid>.json in agentDir,
// naming its profile folder (where Chromium writes the port) and the zellij pane it was started from. The panel
// connects to each as a second debugging client: it streams the agent's tab and records its console, but leaves its
// viewport and input alone, unless the owner takes over (a hold file the wrapper waits on before every command).
// The wrapper also logs every command (log/<key>.jsonl): the session's timeline.

type bagent struct {
	id      string // its pid, as the registry names it
	pid     int
	key     string // its cube-browser session (the launch id): the log's, the hold's and the trail's name
	conv    string // the agent's conversation, as cube-browser knows it
	trail   *btrail
	dir     string // its Chromium's profile folder
	cwd     string
	zs      string
	zp      int
	root    string
	started time.Time
	held    bool
	heldAt  time.Time
}

type bagentLog struct {
	key   string
	steps []*bstep
	byID  map[string]*bstep
	off   int64
	ended time.Time
	seq   int
}

type bstep struct {
	N     int      `json:"n"`
	ID    string   `json:"id"`
	T     int64    `json:"t"`
	Argv  []string `json:"argv"`
	RC    *int     `json:"rc,omitempty"`
	MS    int64    `json:"ms,omitempty"`
	Out   string   `json:"out,omitempty"`
	Trail string   `json:"trail,omitempty"` // the trail its picture is in
	Shot  int      `json:"shot,omitempty"`  // the page after it
	cwd   string   // where the agent ran it: its project
	zs    string   // the pane it ran in
	zp    string
}

var brAgentsOnce sync.Once

// agentDir: where the wrapper registers browsers and logs commands (a fixed place: it doesn't know the panel's flags)
func (b *browser) agentDir() string {
	return filepath.Join(b.home, ".local/state/control-panel/agent-browsers")
}

// agentsScan runs the watcher (once): every second, from the panel's start (the trails record without a page open).
func (b *browser) agentsScan() {
	brAgentsOnce.Do(func() {
		go func() { // always: the trails record whether or not anyone watches
			for {
				b.agentsTick()
				time.Sleep(time.Second) // soon after an agent's Chromium starts: its first page loads at once
			}
		}()
	})
}

// recorder: whether this panel writes the trails. One panel on the machine does, whichever holds the lock on the
// trails folder: a test panel run beside the real one would otherwise record every session again, into the same
// trails. Only from agentsTick.
func (b *browser) recorder() bool {
	if b.recLock == nil {
		os.MkdirAll(b.trailsDir(), 0o700)
		f, err := os.OpenFile(filepath.Join(b.trailsDir(), ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return false
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			f.Close()
			return false
		}
		b.recLock = f
		raw, _ := json.Marshal(map[string]int{"pid": os.Getpid()}) // the wrapper's open waits for it while it lives
		os.WriteFile(filepath.Join(b.agentDir(), "recorder.json"), raw, 0o600)
		b.mu.Lock()
		for _, src := range b.agents { // taken over (the panel that had it ended): the sessions watched unrecorded
			if src.agent.trail == nil { // connect again, with their trails
				src.cdp.close()
			}
		}
		b.mu.Unlock()
	}
	return true
}

func (b *browser) agentsTick() {
	b.recorder()
	dir := b.agentDir()
	ents, _ := os.ReadDir(dir)
	live := map[string]bool{}
	changed := false
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		pid, err := strconv.Atoi(id)
		if err != nil {
			continue
		}
		cl, _ := os.ReadFile("/proc/" + id + "/cmdline")
		if !bytes.Contains(cl, []byte("--remote-debugging-port")) {
			os.Remove(filepath.Join(dir, name)) // that Chromium has gone
			os.Remove(filepath.Join(dir, "attached", id))
			continue
		}
		live[id] = true
		b.mu.Lock()
		have := b.agents[id] != nil
		b.mu.Unlock()
		if have {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(dir, name))
		var reg struct {
			Dir, Cwd, Key, Zs, Zp, Root, Launch, Rt string
			T                                       int64
		}
		if json.Unmarshal(raw, &reg) != nil || reg.Dir == "" {
			continue
		}
		zp, _ := strconv.Atoi(reg.Zp)
		a := &bagent{id: id, pid: pid, key: cmpOr(reg.Launch, cmpKey(reg.Key, reg.Zs, reg.Zp)), dir: reg.Dir, cwd: reg.Cwd, zs: reg.Zs, zp: zp, root: reg.Root, started: time.Unix(reg.T, 0)}
		if reg.Zp == "" {
			a.zp = -1
		}
		agentOwner(a, reg.Rt)
		if b.agentConnect(a) {
			changed = true
		}
	}
	b.mu.Lock()
	for id, src := range b.agents {
		if !live[id] {
			src.cdp.close()
		}
	}
	keys := map[string]bool{}
	for _, src := range b.agents {
		keys[src.agent.key] = true
	}
	for k, l := range b.agentLog { // finished sessions are kept a day, then forgotten
		if !keys[k] && !l.ended.IsZero() && time.Since(l.ended) > 24*time.Hour {
			delete(b.agentLog, k)
		}
	}
	b.mu.Unlock()
	if b.agentLogs(keys) || changed {
		b.push()
	}
	b.trailsWho()
	b.holdTimeouts()
	if time.Since(brLogsSwept) > time.Hour { // a week of timelines on disk, and of trails
		brLogsSwept = time.Now()
		go b.trailsSweep()
		old, _ := filepath.Glob(filepath.Join(dir, "log", "*.jsonl"))
		for _, f := range old {
			if fi, err := os.Stat(f); err == nil && time.Since(fi.ModTime()) > 7*24*time.Hour {
				os.Remove(f)
			}
		}
	}
}

var brLogsSwept time.Time

// liveAgents: how many agents' browsers are open now (the nav's badge), from the registry alone: cheap, and right
// whether or not a page has the Browser open
func (b *browser) liveAgents() int {
	if b == nil {
		return 0
	}
	ents, _ := os.ReadDir(b.agentDir())
	n := 0
	for _, e := range ents {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			if cl, _ := os.ReadFile("/proc/" + id + "/cmdline"); bytes.Contains(cl, []byte("--remote-debugging-port")) {
				n++
			}
		}
	}
	return n
}

// agentOwner reads what cube-browser keeps about the session (its runtime's owner.json): the checkout, the
// conversation, and the agent's process, whose zellij pane is the agent's when the registry has none.
func agentOwner(a *bagent, rt string) {
	if rt == "" || !filepath.IsAbs(rt) {
		return
	}
	raw, err := os.ReadFile(filepath.Join(rt, "owner.json"))
	if err != nil {
		return
	}
	var rec struct {
		Context struct{ CheckoutRoot, ConversationID string }
		Owner   *struct{ PID int }
	}
	if json.Unmarshal(raw, &rec) != nil {
		return
	}
	a.root = cmpOr(rec.Context.CheckoutRoot, a.root)
	a.conv = rec.Context.ConversationID
	if rec.Owner != nil && rec.Owner.PID > 1 && a.zs == "" {
		if zs, zp := zellijOf(rec.Owner.PID); zs != "" {
			a.zs, a.zp = zs, zp
		}
	}
}

// paneNames: each zellij pane's tab name and pane, by "session.pane"
func (b *browser) paneNames() (map[string]TPane, map[string]string) {
	names := map[string]TPane{}
	tabNames := map[string]string{}
	if b.s != nil && b.s.term != nil {
		for _, s := range b.s.term.listing().Sessions {
			for _, tb := range s.Tabs {
				for _, p := range tb.Panes {
					k := s.Name + "." + strconv.Itoa(p.ID)
					names[k] = p
					tabNames[k] = tb.Name
				}
			}
		}
	}
	return names, tabNames
}

// trailsWho keeps each live trail's who (the agent's pane, its tab's name) and writes the metas that changed.
func (b *browser) trailsWho() {
	names, tabNames := b.paneNames()
	b.mu.Lock()
	var trs []*btrail
	for _, src := range b.agents {
		a := src.agent
		if a.trail == nil {
			continue
		}
		trs = append(trs, a.trail)
		if a.zs == "" || a.zp < 0 {
			continue
		}
		k := a.zs + "." + strconv.Itoa(a.zp)
		sub := ""
		if p, ok := names[k]; ok && p.Title != tabNames[k] {
			sub = p.Title
		}
		a.trail.setWho(tabNames[k], sub, &bpane{Session: a.zs, Pane: a.zp, Tab: tabNames[k]})
	}
	b.mu.Unlock()
	for _, tr := range trs {
		tr.flush()
	}
}

func cmpKey(key, zs, zp string) string {
	if key != "" {
		return key
	}
	if zs != "" && zp != "" {
		return zs + "." + zp
	}
	return ""
}

func (b *browser) agentConnect(a *bagent) bool {
	port, err := os.ReadFile(filepath.Join(a.dir, "DevToolsActivePort"))
	if err != nil {
		return false // not written yet: the next scan
	}
	lines := strings.SplitN(strings.TrimSpace(string(port)), "\n", 2)
	if len(lines) != 2 {
		return false
	}
	ws, err := wsDial("ws://127.0.0.1:"+strings.TrimSpace(lines[0])+strings.TrimSpace(lines[1]), 64<<20)
	if err != nil {
		return false
	}
	src := &bsrc{id: a.id, agent: a, sess: map[string]*btab{}}
	if b.recorder() {
		a.trail = b.openTrail(a)
	}
	src.cdp = cdpWS(ws, func(m cdpMsg) { b.event(src, m) })
	b.mu.Lock()
	b.agents[a.id] = src
	if a.key != "" && b.agentLog[a.key] == nil {
		b.agentLog[a.key] = &bagentLog{key: a.key, byID: map[string]*bstep{}}
	}
	var steps []*bstep
	if l := b.agentLog[a.key]; l != nil {
		l.ended = time.Time{}
		steps = slices.Clone(l.steps) // the trail starts with the command that started its Chromium (open)
	}
	if _, err := os.Stat(filepath.Join(b.agentDir(), "hold", a.key)); a.key != "" && err == nil {
		a.held, a.heldAt = true, time.Now()
	}
	b.mu.Unlock()
	for _, st := range steps {
		b.trailStep(a.trail, st)
	}
	go func() {
		<-src.cdp.done
		b.srcDown(src)
	}()
	src.cdp.call("", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil)
	src.cdp.call("", "Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": false, "flatten": true}, nil)
	return true
}

// agentLogs reads what the wrapper appended to each session's log since the last read; a finished command gets a
// small picture of the page as it left it.
func (b *browser) agentLogs(keys map[string]bool) bool {
	changed := false
	b.mu.Lock()
	logs := []*bagentLog{}
	for k, l := range b.agentLog {
		if keys[k] || l.ended.IsZero() {
			logs = append(logs, l)
		}
	}
	b.mu.Unlock()
	for _, l := range logs {
		f, err := os.Open(filepath.Join(b.agentDir(), "log", l.key+".jsonl"))
		if err != nil {
			continue
		}
		if fi, err := f.Stat(); err == nil && fi.Size() < l.off {
			l.off = 0 // started again
		}
		f.Seek(l.off, io.SeekStart)
		br := bufio.NewReaderSize(f, 64<<10)
		var done []*bstep
		for {
			line, err := br.ReadBytes('\n')
			if err != nil {
				break // a half-written line is read again next time
			}
			l.off += int64(len(line))
			var e struct {
				ID     string
				T      int64
				Argv   []string
				Cwd    string
				Zs, Zp string
				RC     *int
				MS     int64
				Out    string
			}
			if json.Unmarshal(line, &e) != nil || e.ID == "" {
				continue
			}
			b.mu.Lock()
			st := l.byID[e.ID]
			src := b.agentOfKey(l.key)
			if st == nil && e.Argv != nil {
				l.seq++
				st = &bstep{N: l.seq, ID: e.ID, T: e.T, Argv: e.Argv, cwd: e.Cwd, zs: e.Zs, zp: e.Zp}
				l.byID[e.ID] = st
				l.steps = append(l.steps, st)
				if len(l.steps) > 300 {
					delete(l.byID, l.steps[0].ID)
					l.steps = l.steps[1:]
				}
				if zp, err := strconv.Atoi(e.Zp); src != nil && e.Zs != "" && err == nil { // the agent: whoever ran its last command
					src.agent.zs, src.agent.zp = e.Zs, zp
				}
			}
			if st != nil && e.RC != nil {
				st.RC, st.MS, st.Out = e.RC, e.MS, trunc(e.Out, 1500)
				done = append(done, st)
			}
			var tr *btrail
			if src != nil {
				tr = src.agent.trail
			}
			b.mu.Unlock()
			if st != nil {
				b.trailStep(tr, st)
			}
			changed = true
		}
		f.Close()
		for _, st := range done { // the page after a command that just ended (not one read from an old log)
			if time.Since(time.UnixMilli(st.T+st.MS)) < 10*time.Second {
				b.stepThumb(l, st)
			}
		}
	}
	if changed {
		b.agentLogKick()
	}
	return changed
}

// agentOfKey: the live Chromium of a session (the newest, if it was reopened); b.mu held
func (b *browser) agentOfKey(key string) *bsrc {
	var out *bsrc
	for _, src := range b.agents {
		if src.agent.key == key && (out == nil || src.agent.started.After(out.agent.started)) {
			out = src
		}
	}
	return out
}

// trailStep writes a command to its session's trail: its start once, its end when it has one.
// trailStep writes a command into a trail: only one of its run (a session's log keeps its earlier runs' commands,
// as its name is used again: the panel reads them all after a restart), and once. Not with b.mu held.
func (b *browser) trailStep(tr *btrail, st *bstep) {
	if tr == nil {
		return
	}
	b.mu.Lock()
	id, t, argv, cwd, rc, ms, out := st.ID, st.T, st.Argv, st.cwd, st.RC, st.MS, st.Out
	b.mu.Unlock()
	if t+ms < tr.start.UnixMilli()-2000 { // ended before this Chromium started (one still running: later, at its end)
		return
	}
	tr.mu.Lock()
	had := tr.seen[id]
	tr.seen[id] = had | 1
	if rc != nil {
		tr.seen[id] |= 2
	}
	tr.mu.Unlock()
	if had&1 == 0 {
		tr.add("step", map[string]any{"t": t, "id": id, "argv": argv, "cwd": tilde(cwd)})
	}
	if had&2 == 0 && rc != nil {
		tr.add("done", map[string]any{"t": t + ms, "id": id, "rc": *rc, "ms": ms, "out": trunc(out, 1500)})
	}
}

// stepThumb: the page the agent works in right after a command, kept in its trail.
func (b *browser) stepThumb(l *bagentLog, st *bstep) {
	b.mu.Lock()
	src := b.agentOfKey(l.key)
	var t *btab
	var tr *btrail
	if src != nil {
		t, tr = src.cur, src.agent.trail
	}
	b.mu.Unlock()
	if t == nil || tr == nil {
		return
	}
	var r struct {
		Data []byte `json:"data"`
	}
	if t.src.cdp.callT(5*time.Second, t.sess, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 60, "optimizeForSpeed": true}, &r) != nil {
		return
	}
	n := tr.shot(r.Data)
	if n == 0 {
		return
	}
	b.mu.Lock()
	st.Trail, st.Shot = tr.id, n
	url, title := t.URL, t.Title
	b.mu.Unlock()
	tr.add("shot", map[string]any{"id": st.ID, "n": n, "tab": t.id, "url": url, "title": title})
	b.agentLogKick()
}

// trailLoad: an agent's page finished loading: its picture, once painted, and the visit (the wrapper's history).
func (b *browser) trailLoad(t *btab) {
	tr := t.src.agent.trail
	if tr == nil {
		return
	}
	time.Sleep(400 * time.Millisecond)
	b.history(t) // its title
	b.mu.Lock()
	url, title, cur := t.URL, t.Title, t.src.cur == t
	b.mu.Unlock()
	if url == "" || strings.HasPrefix(url, "about:") || strings.HasPrefix(url, "chrome") || strings.HasPrefix(url, "data:") {
		return
	}
	n := 0
	if cur {
		var r struct {
			Data []byte `json:"data"`
		}
		if t.src.cdp.callT(5*time.Second, t.sess, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 60, "optimizeForSpeed": true}, &r) == nil {
			n = tr.shot(r.Data)
		}
	}
	tr.add("load", map[string]any{"tab": t.id, "url": url, "title": title, "shot": n})
	tr.visit(url, title, n)
}

// agentLogKick sends each viewer watching a session the steps it doesn't have yet.
func (b *browser) agentLogKick() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for v := range b.viewers {
		if v.agent == "" {
			continue
		}
		l := b.agentLog[v.agent]
		if l == nil {
			continue
		}
		reset := v.logMark == 0
		var out []bstep
		for _, st := range l.steps {
			if st.N > v.logMark || (st.N > v.logMark-5 && st.RC != nil) { // the last few again: a finish can land after a start was sent
				out = append(out, *st)
			}
		}
		if len(out) == 0 && !reset {
			continue
		}
		if len(l.steps) > 0 {
			v.logMark = l.steps[len(l.steps)-1].N
		}
		raw, _ := json.Marshal(map[string]any{"t": "steps", "agent": v.agent, "reset": reset, "steps": out})
		v.queue(raw)
		v.kick()
	}
}

// hold: Take over (the wrapper waits before the agent's next command) or Hand back.
func (b *browser) hold(key string, on bool) {
	if key == "" || strings.ContainsAny(key, "/\\") || key[0] == '.' {
		return
	}
	dir := filepath.Join(b.agentDir(), "hold")
	if on {
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, key), []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o600)
	} else {
		os.Remove(filepath.Join(dir, key))
	}
	b.mu.Lock()
	for _, src := range b.agents {
		if src.agent.key == key {
			src.agent.held, src.agent.heldAt = on, time.Now()
		}
	}
	b.mu.Unlock()
	if !on {
		b.closeBridges(key) // DevTools on its tabs was the owner's, for while they held it
	}
	b.push()
}

// holdTimeouts: a hold nobody uses goes back after 15 minutes, or at once when no page has the Browser open: an
// agent shouldn't wait on an owner who left.
func (b *browser) holdTimeouts() {
	b.mu.Lock()
	var free []string
	for _, src := range b.agents {
		if a := src.agent; a.held && (len(b.viewers) == 0 || time.Since(a.heldAt) > 15*time.Minute) {
			free = append(free, a.key)
		}
	}
	b.mu.Unlock()
	for _, k := range free {
		b.hold(k, false)
	}
}

// touchHold: input on a held tab keeps the hold fresh.
func (b *browser) touchHold(t *btab) {
	b.mu.Lock()
	if t.src.agent != nil && t.src.agent.held {
		t.src.agent.heldAt = time.Now()
	}
	b.mu.Unlock()
}

// agentsJSON: the sessions for the page (b.mu held): live ones with their tabs, then finished ones a while.
func (b *browser) agentsJSON(tj func(*btab) btabJSON) []map[string]any {
	names, tabNames := b.paneNames()
	out := []map[string]any{}
	seen := map[string]bool{}
	ids := make([]string, 0, len(b.agents))
	for id := range b.agents {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return b.agents[ids[i]].agent.started.After(b.agents[ids[j]].agent.started) })
	for _, id := range ids { // a session reopened has one card: its newest Chromium
		src := b.agents[id]
		a := src.agent
		key := cmpOr(a.key, "pid-"+id)
		if seen[key] {
			continue
		}
		seen[key] = true
		m := map[string]any{"key": key, "alive": true, "id": a.id, "started": a.started.Unix(), "held": a.held, "cwd": tilde(a.cwd), "root": tilde(a.root)}
		if a.trail != nil {
			m["trail"] = a.trail.id
		}
		if a.zs != "" && a.zp >= 0 {
			pk := a.zs + "." + strconv.Itoa(a.zp)
			m["pane"] = map[string]any{"session": a.zs, "pane": a.zp}
			if p, ok := names[pk]; ok {
				m["label"] = tabNames[pk]
				if p.Title != "" && p.Title != tabNames[pk] {
					m["sub"] = p.Title
				}
				if p.Agent != nil {
					m["doing"], m["state"], m["title"] = p.Agent.Doing, p.State, p.Agent.Title
				}
			}
		}
		ts := make([]*btab, 0, len(src.sess))
		for _, t := range src.sess {
			ts = append(ts, t)
		}
		sort.Slice(ts, func(i, j int) bool { return ts[i].born.Before(ts[j].born) })
		tabs := []btabJSON{}
		for _, t := range ts {
			tabs = append(tabs, tj(t))
		}
		m["tabs"] = tabs
		if src.cur != nil {
			m["cur"] = src.cur.id
		}
		if l := b.agentLog[key]; l != nil {
			m["steps"] = len(l.steps)
			if n := len(l.steps); n > 0 {
				m["last"] = l.steps[n-1]
				if c := l.steps[n-1].cwd; c != "" { // the agent's own folder (its Chromium runs in Playwright's)
					m["project"] = projectOf(c)
					m["cwd"] = tilde(c)
				}
			}
		}
		if m["project"] == nil && a.root != "" {
			m["project"] = projectOf(a.root)
		}
		out = append(out, m)
	}
	return out
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---- the wrapper and the mirror ----

const brShimMark = "control-panel: cube-browser, watched"

// installAgentWatch puts the wrapper first on PATH and builds the browsers' mirror, when the box has the Cube
// image's cube-browser. It never replaces a cube-browser in ~/.local/bin that isn't its own.
func (b *browser) installAgentWatch() error {
	const real = "/opt/cube/browser/cube-browser.mjs"
	const bins = "/opt/cube/browser-binaries"
	if _, err := os.Stat(real); err != nil || os.Getenv("CP_NO_AGENT_WATCH") != "" {
		return nil
	}
	mirror := filepath.Join(b.home, ".local/share/control-panel/browser-bin")
	ents, err := os.ReadDir(bins)
	if err != nil {
		return err
	}
	os.MkdirAll(mirror, 0o755)
	for _, e := range ents {
		name := e.Name()
		dst := filepath.Join(mirror, name)
		if !strings.HasPrefix(name, "chromium-") {
			if l, _ := os.Readlink(dst); l != filepath.Join(bins, name) {
				os.RemoveAll(dst)
				os.Symlink(filepath.Join(bins, name), dst)
			}
			continue
		}
		// chromium-<rev>: every file linked, except chrome-linux64/chrome, which is the wrapper
		os.MkdirAll(filepath.Join(dst, "chrome-linux64"), 0o755)
		top, _ := os.ReadDir(filepath.Join(bins, name))
		for _, t := range top {
			if t.Name() == "chrome-linux64" {
				continue
			}
			lk := filepath.Join(dst, t.Name())
			if l, _ := os.Readlink(lk); l != filepath.Join(bins, name, t.Name()) {
				os.RemoveAll(lk)
				os.Symlink(filepath.Join(bins, name, t.Name()), lk)
			}
		}
		inner, _ := os.ReadDir(filepath.Join(bins, name, "chrome-linux64"))
		for _, t := range inner {
			lk := filepath.Join(dst, "chrome-linux64", t.Name())
			src := filepath.Join(bins, name, "chrome-linux64", t.Name())
			if t.Name() == "chrome" {
				body := strings.ReplaceAll(brChromeWrapper, "__REAL__", src)
				if old, _ := os.ReadFile(lk); string(old) != body {
					os.Remove(lk)
					if err := os.WriteFile(lk, []byte(body), 0o755); err != nil {
						return err
					}
				}
				continue
			}
			if l, _ := os.Readlink(lk); l != src {
				os.RemoveAll(lk)
				os.Symlink(src, lk)
			}
		}
	}
	os.WriteFile(filepath.Join(mirror, "ok"), nil, 0o644)
	bin := filepath.Join(b.home, ".local/bin")
	os.MkdirAll(bin, 0o755)
	shim := filepath.Join(bin, "cube-browser")
	if old, err := os.ReadFile(shim); err == nil && !bytes.Contains(old, []byte(brShimMark)) {
		return nil // someone else's
	}
	if old, _ := os.ReadFile(shim); string(old) != brShim {
		tmp := shim + ".tmp"
		if err := os.WriteFile(tmp, []byte(brShim), 0o755); err != nil {
			return err
		}
		return os.Rename(tmp, shim)
	}
	return nil
}

const brChromeWrapper = `#!/bin/bash
# control-panel: an agent's Chromium (cube-browser), with a debugging port on localhost so the Control Panel's
# Browser can show it live. It registers itself in ~/.local/state/control-panel/agent-browsers/<pid>.json, with its
# cube-browser session: the runtime folder Playwright runs it in (XDG_CACHE_HOME is its cache) names it.
st="$HOME/.local/state/control-panel/agent-browsers"
dd=
for a in "$@"; do case "$a" in --user-data-dir=*) dd="${a#--user-data-dir=}";; esac; done
rt= launch=
case "${XDG_CACHE_HOME:-}" in */cache) p=${XDG_CACHE_HOME%/cache}; if [ -f "$p/owner.json" ]; then rt=$p; launch=${p##*/}; fi;; esac
[ -z "$launch" ] && launch=${CP_BROWSER_LAUNCH:-}
j() { local v=${1//[$'\t\n\r']/ }; v=${v//\\/\\\\}; v=${v//\"/\\\"}; printf '"%s"' "$v"; }
if mkdir -p "$st" 2>/dev/null; then
  printf '{"pid":%d,"dir":%s,"cwd":%s,"key":%s,"launch":%s,"rt":%s,"zs":%s,"zp":%s,"root":%s,"t":%d}\n' $$ "$(j "$dd")" "$(j "$PWD")" \
    "$(j "${CP_BROWSER_KEY:-}")" "$(j "$launch")" "$(j "$rt")" "$(j "${ZELLIJ_SESSION_NAME:-}")" "$(j "${ZELLIJ_PANE_ID:-}")" "$(j "${CUBE_BROWSER_ROOT:-}")" "$(date +%s)" \
    > "$st/$$.json.tmp" && mv "$st/$$.json.tmp" "$st/$$.json"
fi
exec "__REAL__" "$@" --remote-debugging-port=0
`

const brShim = `#!/usr/bin/env node
// control-panel: cube-browser, watched. The Control Panel installed this (it's first on PATH): it runs the Cube
// image's cube-browser exactly as before, with its Chromium from a mirror that opens a localhost debugging port, so
// the owner can watch this agent's browser live in the panel's Browser, and replay it later (its trail). It also
// logs each command (the session's timeline), waits while the owner has taken over, and adds "history": what the
// agents' browsers did here in the last 7 days. Delete this file to go back to the plain tool.
'use strict';
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const REAL = '/opt/cube/browser/cube-browser.mjs';
const HOME = process.env.HOME || '';
const ST = HOME + '/.local/state/control-panel/agent-browsers';
const TR = ST + '/trails';
const BIN = HOME + '/.local/share/control-panel/browser-bin';
const env = { ...process.env };
const zs = env.ZELLIJ_SESSION_NAME || '', zp = env.ZELLIJ_PANE_ID || '';
const args = process.argv.slice(2);
const session = args[0] === '--session' ? args[1] : undefined;
const cmd = args[session !== undefined ? 2 : 0] || '';
const out = (s) => process.stdout.write(s + '\n');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const ago = (t) => { const s = (Date.now() - t) / 1000; return s < 90 ? Math.round(s) + ' s ago' : s < 5400 ? Math.round(s / 60) + ' min ago' : s < 129600 ? Math.round(s / 3600) + ' h ago' : Math.round(s / 86400) + ' d ago'; };
const bare = (u) => String(u || '').replace(/^[a-z]+:\/\//i, '').replace(/^www\./, '').replace(/^127\.0\.0\.1(?=[:/]|$)/, 'localhost').replace(/\/$/, '');
function visits() {
  let raw = '';
  try { raw = fs.readFileSync(TR + '/visits.jsonl', 'utf8'); } catch {}
  const cut = Date.now() - 7 * 864e5;
  return raw.split('\n').map((l) => { try { return JSON.parse(l); } catch { return null; } }).filter((v) => v && v.url && v.t >= cut);
}
const meta = (id) => { try { return JSON.parse(fs.readFileSync(TR + '/' + id + '/meta.json', 'utf8')); } catch { return null; } };
const events = (id) => { try { return fs.readFileSync(TR + '/' + id + '/events.jsonl', 'utf8').split('\n').map((l) => { try { return JSON.parse(l); } catch { return null; } }).filter(Boolean).sort((a, b) => a.t - b.t); } catch { return []; } };
const who = (m) => (m ? [m.label, m.sub, m.project].filter(Boolean).join(' · ') || m.launch : 'an agent');
const shot = (id, n) => (n ? TR + '/' + id + '/shots/' + n + '.jpg' : '');
const verb = (a) => { a = (a || []).slice(); if (a[0] === '--session') a.splice(0, 2); return a.join(' ').slice(0, 160); };

function calls() {
  let raw = '';
  try { raw = fs.readFileSync(TR + '/calls.jsonl', 'utf8'); } catch {}
  const cut = Date.now() - 7 * 864e5;
  return raw.split('\n').map((l) => { try { return JSON.parse(l); } catch { return null; } }).filter((c) => c && c.url && c.t >= cut);
}
const kb = (n) => (n < 1024 ? n + ' B' : n < 1048576 ? Math.round(n / 1024) + ' KB' : (n / 1048576).toFixed(1) + ' MB');

// api-calls: the API calls agents' pages made here (to a URL, or from a page, matching the text): the newest of each,
// with the file holding what was sent and what came back
function apiCalls(q) {
  const ql = q.toLowerCase(), qb = bare(q).toLowerCase();
  const hits = calls().filter((c) => !q || (c.method + ' ' + c.url + ' ' + (c.page || '')).toLowerCase().includes(ql) || bare(c.url).toLowerCase().includes(qb) || bare(c.page).toLowerCase().includes(qb));
  if (!hits.length) return out(q ? 'No API call matching "' + q + '" was kept in the last 7 days. (cube-browser api-calls, alone, lists the latest.)' : "No API calls kept from agents' browsers here in the last 7 days.");
  const by = new Map(); // the newest of each method and URL, and how many times
  for (const c of hits) { const k = c.method + ' ' + c.url; by.set(k, { c, n: (by.get(k)?.n || 0) + 1 }); }
  const list = [...by.values()].sort((a, b) => b.c.t - a.c.t), metas = new Map();
  out((q ? 'API calls matching "' + q + '"' : "The latest API calls of agents' pages here") + ', last 7 days (newest first):');
  for (const { c, n } of list.slice(0, 20)) {
    if (!metas.has(c.trail)) metas.set(c.trail, meta(c.trail));
    out('* ' + ago(c.t) + ' - ' + c.method + ' ' + c.url + ' -> ' + (c.err ? 'failed: ' + c.err : c.status + (c.mime ? ' ' + c.mime : '') + ', ' + kb(c.size || 0)) + (n > 1 ? ' (' + n + ' times)' : '') + ' - from ' + bare(c.page) + ' by ' + who(metas.get(c.trail)));
    out('    ' + TR + '/' + c.trail + '/calls/' + c.n + '.json');
  }
  if (list.length > 20) out('(+' + (list.length - 20) + ' more: narrow the text)');
  out('\nEach file has the request (method, URL, headers, body) and the response (status, headers, body); credentials are redacted. Read it instead of calling that API again when it is recent enough for what you need.');
}

// history: the sessions, or each visit to the pages matching a query, with what it led to and looked like
function history(q) {
  const vs = visits();
  if (!vs.length) return out("No agent browser trails here in the last 7 days.");
  if (!q) {
    const by = new Map();
    for (const v of vs) { if (!by.has(v.trail)) by.set(v.trail, []); by.get(v.trail).push(v); }
    const list = [...by.entries()].sort((a, b) => b[1][b[1].length - 1].t - a[1][a[1].length - 1].t).slice(0, 8);
    out("Agents' browser sessions on this machine, last 7 days (newest first):");
    for (const [id, v] of list) {
      const m = meta(id), last = v[v.length - 1];
      const pages = [...new Set(v.map((x) => bare(x.url)))];
      out('* ' + ago(last.t) + ' - ' + who(m) + (m && m.root ? ' (' + m.root + ')' : '') + ' - ' + (m ? m.steps + ' command' + (m.steps === 1 ? '' : 's') + ', ' : '') + v.length + ' page load' + (v.length === 1 ? '' : 's') + (m && m.errors ? ', ' + m.errors + ' console/network error' + (m.errors === 1 ? '' : 's') : ''));
      out('    pages: ' + pages.slice(0, 6).join('  |  ') + (pages.length > 6 ? '  (+' + (pages.length - 6) + ')' : ''));
      const sh = [...v].reverse().find((x) => x.shot);
      if (sh) out('    last screenshot: ' + shot(id, sh.shot));
    }
    return out('\ncube-browser history <part of a URL or title> lists each visit to those pages: what it looked like (a screenshot to open), its errors, and the command that led there.');
  }
  const ql = q.toLowerCase(), qb = bare(q).toLowerCase();
  const hits = vs.filter((v) => (v.url + ' ' + (v.title || '')).toLowerCase().includes(ql) || bare(v.url).toLowerCase().includes(qb)).reverse().slice(0, 8);
  if (!hits.length) return out('No agent loaded a page matching "' + q + '" in the last 7 days. (cube-browser history, alone, lists the sessions.)');
  out('Visits to pages matching "' + q + '", last 7 days (newest first):');
  for (const v of hits) {
    const ev = events(v.trail), m = meta(v.trail);
    const i = ev.findIndex((e) => e.k === 'load' && e.t === v.t);
    let step = null;
    for (let j = (i < 0 ? ev.length : i) - 1; j >= 0 && !step; j--) if (ev[j].k === 'step') step = ev[j];
    const errs = []; // the page view's: between the tab's previous load and its next; a failed page request is the page it asked for
    if (i >= 0) {
      const tab = ev[i].tab;
      let a = i;
      while (a > 0 && !(ev[a - 1].k === 'load' && ev[a - 1].tab === tab)) a--;
      for (let j = a; j < ev.length && errs.length < 4; j++) {
        const e = ev[j];
        if (j > i && e.k === 'load' && e.tab === tab) break;
        if ((e.k !== 'con' && e.k !== 'net') || e.tab !== tab) continue;
        if (e.k === 'net' && e.src === 'Document' && !String(e.text).endsWith(' ' + v.url) && !String(e.text).endsWith(' ' + v.url.replace(/\/$/, ''))) continue;
        errs.push(e.text.split('\n')[0].slice(0, 200) + (e.src ? ' (' + e.src + ')' : ''));
      }
    }
    out('* ' + ago(v.t) + ' - ' + v.url + (v.title ? ' - "' + v.title + '"' : '') + ' - by ' + who(m));
    if (step) out('    after: cube-browser ' + verb(step.argv));
    out(errs.length ? '    errors: ' + errs.join('; ') : '    no console or network errors');
    if (v.shot) out('    screenshot: ' + shot(v.trail, v.shot));
  }
}

(async () => {
  if (cmd === 'history') return history(args.slice(session !== undefined ? 3 : 1).join(' ').trim());
  if (cmd === 'api-calls') return apiCalls(args.slice(session !== undefined ? 3 : 1).join(' ').trim());
  const help = !args.length || args.includes('--help');
  // the session's id, worked out by cube-browser's own code: the timeline's, the hold's and the trail's name
  let launch = '';
  if (!help) {
    try { const { resolveContext } = await import('/opt/cube/browser/context.mjs'); launch = (await resolveContext(env, process.cwd(), session)).launchId; } catch {}
    if (!launch && cmd === 'close' && session === undefined) launch = env.CUBE_BROWSER_LAUNCH || '';
  }
  const key = (launch || (zs && zp ? zs + '.' + zp : env.CUBE_BROWSER_CONVERSATION ? 'c-' + env.CUBE_BROWSER_CONVERSATION : 'p-' + process.ppid)).replace(/[^\w.-]/g, '_').slice(0, 128);
  if (fs.existsSync(BIN + '/ok')) env.CUBE_BROWSER_BINARIES = BIN;
  env.CP_BROWSER_KEY = key;
  env.CP_BROWSER_LAUNCH = launch;
  const log = (o) => { if (help) return; try { fs.mkdirSync(ST + '/log', { recursive: true }); fs.appendFileSync(ST + '/log/' + key + '.jsonl', JSON.stringify(o) + '\n'); } catch {} };
  const hold = ST + '/hold/' + key;
  if (!help && fs.existsSync(hold)) {
    process.stderr.write('The owner has taken over this browser in the Control Panel; waiting for them to hand it back…\n');
    for (let waited = 0; fs.existsSync(hold) && waited < 100000; waited += 500) await sleep(500);
    if (fs.existsSync(hold)) {
      process.stderr.write('The owner still has control of this browser (Control Panel → Browser). Wait a little, then retry the command.\n');
      process.exitCode = 75;
      return;
    }
  }
  const id = Date.now().toString(36) + Math.random().toString(36).slice(2, 6), t0 = Date.now();
  log({ id, t: t0, argv: args.map((a) => a.slice(0, 2000)), cwd: process.cwd(), launch, zs, zp, conv: env.CUBE_BROWSER_CONVERSATION || '' });
  let kid = null;
  for (const s of ['SIGINT', 'SIGTERM', 'SIGHUP']) process.on(s, () => kid?.kill(s));
  // the real tool; live: its output as it comes, else kept and handed back
  const real = (argv, live) => new Promise((done) => {
    const child = kid = spawn(process.execPath, [REAL, ...argv], { env, stdio: ['inherit', 'pipe', 'pipe'] });
    let o = '', e = '';
    child.stdout.on('data', (d) => { if (live) process.stdout.write(d); o += d; });
    child.stderr.on('data', (d) => { if (live) process.stderr.write(d); e += d; });
    child.on('error', (err) => done({ code: 127, o, e: e + String(err) + '\n' }));
    child.on('close', (code) => done({ code: code ?? 1, o, e }));
  });
  const url = cmd === 'open' && args[args.indexOf(cmd) + 1];
  // open <url>, while a panel records: the browser starts blank, the panel attaches to it, then it goes to the page,
  // so the panel sees all of the page's requests (its first load's API calls are kept too); printed as open prints it
  async function openWatched() {
    let rec = null;
    try { rec = JSON.parse(fs.readFileSync(ST + '/recorder.json', 'utf8')); } catch {}
    if (!launch || !rec || !fs.existsSync('/proc/' + rec.pid)) return null;
    const pre = session !== undefined ? ['--session', session] : [];
    const a = await real([...pre, 'open']);
    const head = a.code === 0 && /^(\{[\s\S]*?\n\})\n/.exec(a.o);
    let h = null;
    try { h = head && JSON.parse(head[1]); } catch {}
    if (!h) return null; // not as expected: the command as it is
    for (const t = Date.now(); Date.now() - t < 3000; await sleep(100)) {
      let pid = 0, at = 0; // its Chromium: the newest of this session's that runs
      try { for (const f of fs.readdirSync(ST)) if (/^\d+\.json$/.test(f)) { const r = JSON.parse(fs.readFileSync(ST + '/' + f, 'utf8')); if (r.launch === launch && r.t >= at && fs.existsSync('/proc/' + r.pid)) { pid = r.pid; at = r.t; } } } catch {}
      if (pid && fs.existsSync(ST + '/attached/' + pid)) break;
    }
    const g = await real([...pre, 'goto', url]);
    const body = g.code === 0 && /^(\{[\s\S]*?\n\})\n/.exec(g.o);
    let res = null;
    try { res = body && JSON.parse(body[1]); } catch {}
    if (!res) return { ...g, e: a.e + g.e };
    return { code: 0, o: JSON.stringify({ session: h.session, pid: h.pid, result: res }, null, 2) + g.o.slice(body[1].length), e: a.e + g.e };
  }
  const w = url && !help ? await openWatched() : null;
  if (w) { process.stdout.write(w.o); process.stderr.write(w.e); }
  const r = w || await real(args, true);
  {
    const code = r.code, text = (r.o + r.e).slice(0, 4000);
    log({ id, rc: code, ms: Date.now() - t0, out: text });
    process.exitCode = code;
    if (help) out('\nControl Panel adds:\n  history [text]   what the agents\' browsers here did in the last 7 days: pages, errors, screenshots.\n                   Check it before testing a page another agent already went through.\n  api-calls [text] the API calls their pages made (to a URL, or from a page, matching the text): what was\n                   sent and what came back, a file each. Read it instead of calling that API again.');
    const went = (cmd === 'open' || cmd === 'goto') && code === 0 && args[args.indexOf(cmd) + 1];
    if (went) { // seen before? one line, so the agent can reuse it
      const b = bare(went).toLowerCase(), seen = visits().filter((v) => bare(v.url).toLowerCase() === b && !v.trail.startsWith(key + '/'));
      const host = b.split('/')[0], api = seen.length ? calls().filter((c) => bare(c.page).toLowerCase().startsWith(host)).length : 0;
      if (seen.length) out('\n(Control Panel: agents\' browsers loaded this page ' + seen.length + ' time' + (seen.length === 1 ? '' : 's') + ' in the last 7 days, last ' + ago(seen[seen.length - 1].t) + '. cube-browser history ' + JSON.stringify(b) + ' shows what they saw' + (api ? '; cube-browser api-calls ' + JSON.stringify(host) + ' the ' + api + ' API call' + (api === 1 ? '' : 's') + ' its pages made, with their responses' : '') + '.)');
    }
  }
})();
`
