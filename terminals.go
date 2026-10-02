package main

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The Terminals view: every zellij session → tab → pane in zellij's own order, each pane with
// what runs in it and a truthful state. Agent status comes from Claude's own session files.

type Terminals struct {
	Sessions  []TSession `json:"sessions"`
	Asking    int        `json:"asking"`  // agents blocked on a question
	Waiting   int        `json:"waiting"` // asking + finished agents: all waiting for the owner
	Working   int        `json:"working"`
	Panes     int        `json:"panes"`
	Agents    int        `json:"agents"`
	Subagents int        `json:"subagents"`
}

type TSession struct {
	Name string `json:"name"`
	Err  string `json:"err,omitempty"`
	Tabs []TTab `json:"tabs"`
}

type TTab struct {
	ID    int     `json:"id"`
	Pos   int     `json:"pos"`
	Name  string  `json:"name"`
	Panes []TPane `json:"panes"`
}

type TPane struct {
	ID       int      `json:"id"`
	Title    string   `json:"title"`
	Kind     string   `json:"kind"`  // claude, codex, shell, cmd
	State    string   `json:"state"` // asking, waiting, working, stalled, background, idle, running, stopped, stuck, exited, unknown
	Detail   string   `json:"detail"`
	Since    int64    `json:"since,omitempty"`
	Output   int64    `json:"output,omitempty"`
	CPU      float64  `json:"cpu"`
	RSS      uint64   `json:"rss"`
	N        int      `json:"n"`
	Floating bool     `json:"floating,omitempty"`
	Cols     int      `json:"cols"`
	Rows     int      `json:"rows"`
	Agent    *TAgent  `json:"agent,omitempty"`
	Git      *GitInfo `json:"git,omitempty"`  // branch and checkout (worktree) the pane works in
	Fg       int      `json:"fg,omitempty"`   // the foreground process, when it isn't the pane's shell
	Proc     string   `json:"proc,omitempty"` // its command line
}

type TAgent struct {
	PID        int        `json:"pid"`
	Status     string     `json:"status"`
	WaitingFor string     `json:"waitingFor,omitempty"`
	Title      string     `json:"title,omitempty"`
	Doing      string     `json:"doing,omitempty"`
	Cwd        string     `json:"cwd,omitempty"`
	Subagents  []Subagent `json:"subagents,omitempty"`
	Prompt     *Prompt    `json:"prompt,omitempty"`
	Chat       bool       `json:"chat"`
	sid, path  string
}

// agentInfo is what the tracker knows about the Claude in one pane.
type agentInfo struct {
	cp     claudeProc
	tail   tailInfo
	path   string
	subs   []Subagent
	prompt *Prompt
}

var hookState = map[string]string{"Waiting": "asking", "Thinking": "working", "Tool": "working", "Done": "waiting", "Idle": "waiting"}

func fbActivity(fb *Pane) string {
	if fb == nil {
		return ""
	}
	return fb.Activity
}

var shellComms = map[string]bool{"zsh": true, "bash": true, "sh": true, "fish": true, "dash": true}

// ponytail: 15 min without a transcript write or a redraw (Claude's spinner redraws while busy)
// and under 2% CPU reads as frozen; tune if long silent tool calls get flagged.
const stallAfter = 15 * 60

// paneState decides one pane's kind, state and detail line. probe: a prompt read off the screen of an
// agent that has no session file yet (Claude writes it after the startup trust dialog). dSince: when the
// foreground process entered uninterruptible sleep (0 if it isn't in D).
func paneState(p zPane, in paneInfo, ag *agentInfo, fb *Pane, probe *Prompt, now time.Time, dSince int64) (kind, state, detail string, since int64) {
	kind = "cmd"
	if shellComms[in.Comm] || (in.Comm == "" && p.Command == "") {
		kind = "shell"
	}
	switch {
	case p.Exited || p.Held:
		code := "?"
		if p.ExitStatus != nil {
			code = strconv.Itoa(*p.ExitStatus)
		}
		return kind, "exited", "exit " + code, 0
	case ag != nil && ag.cp.State == "T":
		return "claude", "stopped", "Stopped", ag.cp.Since
	case ag != nil:
		since = ag.cp.Since
		switch ag.cp.Status {
		case "busy":
			state, detail = "working", ag.tail.doing
			if detail == "" {
				detail = "Working"
			}
			last := max(ag.tail.mtime.Unix(), in.Output, ag.cp.Since)
			if quiet := now.Unix() - last; quiet >= stallAfter && in.CPU < 2 {
				state, detail = "stalled", "No activity for "+fmtDur(quiet)
			}
		case "waiting":
			state, detail = "asking", ag.cp.WaitingFor
			if detail == "" {
				detail = "Needs your answer"
			}
		case "idle":
			state, detail = "waiting", ag.tail.title
		case "shell": // the turn is over but a background command it started still runs; Claude resumes when it ends
			state, detail = "background", "Background command running"
		default:
			state, detail = "unknown", ag.cp.Status
		}
		return "claude", state, detail, since
	case in.Agent == "claude" && probe != nil: // no session file yet: the screen beats the (often stale) hook
		return in.Agent, "asking", "Needs your answer", 0
	case in.Agent != "" && hookState[fbActivity(fb)] != "": // codex, or Claude without a file: the hook plugin's word
		return in.Agent, hookState[fb.Activity], trunc(fb.Detail, 120), 0
	case in.Agent != "" && probe != nil:
		return in.Agent, "asking", "Needs your answer", 0
	case in.Agent != "":
		return in.Agent, "starting", "Starting", 0
	case in.Root == 0:
		return kind, "unknown", "", 0
	case in.FgPID == 0 || (in.FgPID == in.Root && kind == "shell"):
		return kind, "idle", "", 0
	case in.FgState == "T":
		return kind, "stopped", in.FgCmd, in.FgSince
	case in.FgState == "D" && dSince > 0 && now.Unix()-dSince >= 30:
		return kind, "stuck", in.FgCmd, in.FgSince
	}
	return kind, "running", in.FgCmd, in.FgSince
}

type termTracker struct {
	z         *zellij
	claudeDir string
	tr        *transcripts
	subs      subagentCache
	fb        *agentsTracker
	startOf   func(pid int) (procStat, bool)
	paused    func(key string) bool // a balancer rule pauses this group

	layouts   map[string][]zPane
	layoutAt  map[string]time.Time
	layoutErr map[string]string
	dSince    map[string]int64
	prompts   map[string]probe // asking agents: their prompt, re-read at most every 5 s
	probes    map[string]probe // agents without a session file: screen read at most every 5 s
	gits      map[string]gitCached

	mu    sync.Mutex // guards last, lbl and stale (shared with HTTP handlers)
	last  Terminals
	lbl   map[string]string
	stale map[string]bool
}

// invalidate makes the next update re-read a session's layout (after creating a tab or pane).
func (t *termTracker) invalidate(session string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stale == nil {
		t.stale = map[string]bool{}
	}
	t.stale[session] = true
}

func newTermTracker(z *zellij, claudeDir string, tr *transcripts, fb *agentsTracker) *termTracker {
	return &termTracker{
		z: z, claudeDir: claudeDir, tr: tr, fb: fb,
		startOf: func(pid int) (procStat, bool) {
			st, ok := procStartOf(pid)
			return st, ok && st.State != "Z" && st.State != "X"
		},
		layouts: map[string][]zPane{}, layoutAt: map[string]time.Time{}, layoutErr: map[string]string{},
		dSince: map[string]int64{}, prompts: map[string]probe{}, probes: map[string]probe{}, gits: map[string]gitCached{},
	}
}

type gitCached struct {
	at time.Time
	g  GitInfo
}

// git reads a directory's checkout, re-reading at most every 10 s.
func (t *termTracker) git(dir string, now time.Time) *GitInfo {
	if dir == "" {
		return nil
	}
	c, ok := t.gits[dir]
	if !ok || now.Sub(c.at) >= 10*time.Second {
		c = gitCached{at: now, g: gitInfo(dir)}
		t.gits[dir] = c
	}
	if c.g.Root == "" {
		return nil
	}
	g := c.g
	return &g
}

type probe struct {
	at time.Time
	p  *Prompt
}

func paneKey(session string, id int) string { return "pane:" + session + ":" + strconv.Itoa(id) }

// update builds the snapshot. full: a Terminals page is open, so refresh layouts every 3 s and read
// transcripts, subagents and prompts; otherwise only the counts for the nav badge (layouts every 30 s
// keep Resources labels fresh). Runs on the sampling goroutine.
func (t *termTracker) update(now time.Time, live []string, res Resources, keyOf func(int) string, full bool) Terminals {
	every := 30 * time.Second
	if full {
		every = 3 * time.Second
	}
	t.mu.Lock()
	for s := range t.stale {
		delete(t.layoutAt, s)
	}
	t.stale = nil
	t.mu.Unlock()
	isLive := map[string]bool{}
	for _, s := range live {
		isLive[s] = true
		if now.Sub(t.layoutAt[s]) >= every {
			t.layoutAt[s] = now
			if ps, err := t.z.panes(s); err != nil {
				t.layoutErr[s] = err.Error()
			} else {
				t.layouts[s], t.layoutErr[s] = ps, ""
			}
		}
	}
	for s := range t.layouts {
		if !isLive[s] {
			delete(t.layouts, s)
			delete(t.layoutAt, s)
			delete(t.layoutErr, s)
		}
	}

	// One Claude per pane: the one in the terminal's foreground group, then the most blocked
	// (waiting > busy or shell > idle), then the latest status change.
	rank := map[string]int{"waiting": 0, "busy": 1, "shell": 1, "idle": 2}
	better := func(a, b claudeProc, fg int) bool {
		if (a.PGRP == fg) != (b.PGRP == fg) {
			return a.PGRP == fg
		}
		ra, oka := rank[a.Status]
		rb, okb := rank[b.Status]
		if !oka {
			ra = 3
		}
		if !okb {
			rb = 3
		}
		if ra != rb {
			return ra < rb
		}
		return a.Since > b.Since
	}
	chosen := map[string]claudeProc{}
	for _, cp := range readClaudeSessions(t.claudeDir, t.startOf) {
		k := keyOf(cp.PID)
		if !strings.HasPrefix(k, "pane:") {
			continue // a Claude outside zellij (Cube's own agents)
		}
		if old, ok := chosen[k]; !ok || better(cp, old, res.panes[k].FgPID) {
			chosen[k] = cp
		}
	}
	agents := map[string]*agentInfo{}
	for k, cp := range chosen {
		ag := &agentInfo{cp: cp}
		if full && cp.SessionID != "" {
			ag.tail, ag.path = t.tr.tail(cp.SessionID)
			if ag.path != "" {
				ag.subs = t.subs.list(strings.TrimSuffix(ag.path, ".jsonl")+"/subagents", now)
			}
		}
		agents[k] = ag
	}
	fallback := map[string]*Pane{}
	for _, zs := range t.fb.poll(live).Sessions {
		for i := range zs.Panes {
			fallback[paneKey(zs.Name, zs.Panes[i].PaneID)] = &zs.Panes[i]
		}
	}

	var out Terminals
	zpane := map[string]zPane{}
	for s, ps := range t.layouts {
		for _, p := range ps {
			zpane[paneKey(s, p.ID)] = p
		}
	}
	states := map[string]TPane{}
	asking := map[string]bool{}
	for k, in := range res.panes {
		if in.FgState == "D" {
			if t.dSince[k] == 0 {
				t.dSince[k] = now.Unix()
			}
		} else {
			delete(t.dSince, k)
		}
		ag := agents[k]
		pr, probed := t.probes[k]
		if ag == nil && in.Agent != "" && (in.Agent == "claude" || hookState[fbActivity(fallback[k])] == "") {
			if full && now.Sub(pr.at) >= 5*time.Second {
				pr = probe{at: now}
				if s, id, ok := splitPaneKey(k); ok {
					if screen, err := t.z.dump(s, id); err == nil {
						pr.p = parsePrompt(screen)
					}
				}
				t.probes[k], probed = pr, true
			}
		} else if probed {
			delete(t.probes, k)
			pr = probe{}
		}
		kind, state, detail, since := paneState(zpane[k], in, ag, fallback[k], pr.p, now, t.dSince[k])
		if state == "stopped" && t.paused != nil && t.paused(k) {
			detail = "Paused by a limit (Resources)"
		}
		tp := TPane{Kind: kind, State: state, Detail: detail, Since: since, Output: in.Output, CPU: in.CPU, RSS: in.RSS, N: in.N}
		if in.FgPID > 0 && (in.FgPID != in.Root || !shellComms[in.Comm]) {
			tp.Fg, tp.Proc = in.FgPID, trunc(in.FgCmd, 120)
		}
		if ag != nil || in.Agent != "" {
			out.Agents++
		}
		switch state {
		case "asking":
			out.Asking++
			out.Waiting++
		case "waiting":
			out.Waiting++
		case "working", "stalled", "background":
			out.Working++
		}
		if ag != nil && full {
			a := &TAgent{PID: ag.cp.PID, Status: ag.cp.Status, WaitingFor: ag.cp.WaitingFor, Title: ag.tail.title,
				Doing: ag.tail.doing, Cwd: tilde(ag.cp.Cwd), Subagents: ag.subs, Chat: ag.path != "", sid: ag.cp.SessionID, path: ag.path}
			out.Subagents += len(ag.subs)
			if state == "asking" {
				asking[k] = true
				pr := t.prompts[k]
				if now.Sub(pr.at) >= 5*time.Second { // re-read: the dialog may still be drawing, or a queued one replaced it
					pr = probe{at: now}
					if s, id, ok := splitPaneKey(k); ok {
						if screen, err := t.z.dump(s, id); err == nil {
							pr.p = parsePrompt(screen)
						}
					}
					t.prompts[k] = pr
				}
				a.Prompt = pr.p
			}
			tp.Agent = a
		} else if pr.p != nil && full {
			tp.Agent = &TAgent{Status: "starting", Prompt: pr.p}
		}
		if full {
			dir := in.Cwd
			if ag != nil && ag.cp.Cwd != "" {
				dir = ag.cp.Cwd
			}
			tp.Git = t.git(dir, now)
		}
		states[k] = tp
	}
	for d, c := range t.gits {
		if now.Sub(c.at) > time.Minute {
			delete(t.gits, d)
		}
	}
	for pk := range t.prompts {
		if !asking[pk] {
			delete(t.prompts, pk)
		}
	}
	for k := range t.dSince {
		if _, ok := res.panes[k]; !ok {
			delete(t.dSince, k)
		}
	}
	for k := range t.probes {
		if _, ok := res.panes[k]; !ok {
			delete(t.probes, k)
		}
	}

	lbl := map[string]string{}
	for _, s := range live {
		ts := TSession{Name: s, Err: t.layoutErr[s]}
		byTab := map[int]*TTab{}
		var tabs []*TTab
		for _, p := range t.layouts[s] {
			k := paneKey(s, p.ID)
			lbl[k] = p.TabName + " · " + s
			tb := byTab[p.TabID]
			if tb == nil {
				tb = &TTab{ID: p.TabID, Pos: p.TabPos, Name: p.TabName}
				byTab[p.TabID] = tb
				tabs = append(tabs, tb)
			}
			tp, ok := states[k]
			if !ok { // no process left in it (a held/exited pane) or not sampled yet
				kind, state, detail, _ := paneState(p, paneInfo{}, nil, nil, nil, now, 0)
				tp = TPane{Kind: kind, State: state, Detail: detail}
			}
			tp.ID, tp.Title, tp.Floating, tp.Cols, tp.Rows = p.ID, p.Title, p.Floating, p.Cols, p.Rows
			tb.Panes = append(tb.Panes, tp)
			out.Panes++
		}
		sort.Slice(tabs, func(i, j int) bool { return tabs[i].Pos < tabs[j].Pos })
		for _, tb := range tabs {
			sort.SliceStable(tb.Panes, func(i, j int) bool {
				a, b := tb.Panes[i], tb.Panes[j]
				if a.Floating != b.Floating {
					return !a.Floating
				}
				return a.ID < b.ID
			})
			ts.Tabs = append(ts.Tabs, *tb)
		}
		if full {
			out.Sessions = append(out.Sessions, ts)
		}
	}
	if !full {
		out.Panes = len(zpane)
	}
	t.mu.Lock()
	if full {
		t.last = out
	}
	t.lbl = lbl
	t.mu.Unlock()
	return out
}

func splitPaneKey(k string) (session string, id int, ok bool) {
	rest, found := strings.CutPrefix(k, "pane:")
	i := strings.LastIndexByte(rest, ':')
	if !found || i < 0 {
		return "", 0, false
	}
	id, err := strconv.Atoi(rest[i+1:])
	return rest[:i], id, err == nil
}

// labels names pane process groups after their tab for the Resources view ("PMF U42 · dev-1").
func (t *termTracker) labels() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lbl
}

// pane finds a pane in the last full snapshot.
func (t *termTracker) pane(session string, id int) (TPane, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.last.Sessions {
		if s.Name != session {
			continue
		}
		for _, tb := range s.Tabs {
			for _, p := range tb.Panes {
				if p.ID == id {
					return p, true
				}
			}
		}
	}
	return TPane{}, false
}
