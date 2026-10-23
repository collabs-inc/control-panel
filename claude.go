package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// claudeProc is one running Claude Code process as Claude itself reports it in
// ~/.claude/sessions/<pid>.json. That file is the ground truth for agent status: the
// zellij hook spool goes stale (a missed Stop hook leaves "Thinking" forever).
type claudeProc struct {
	PID        int
	SessionID  string // transcript id; empty if it doesn't look like one
	Cwd, Name  string
	Status     string // busy, idle, waiting (as Claude writes it, capped)
	WaitingFor string // e.g. "permission prompt"
	Since      int64  // unix seconds the status last changed
	Started    int64  // unix seconds the process started (its session file's startedAt)
	State      string // the process's own state: "T" means stopped (Ctrl-Z, or paused by a limit)
	PGRP       int    // process group: the one in its terminal's foreground is the one on screen
}

var (
	claudeFileRe = regexp.MustCompile(`^(\d+)\.json$`)
	sessionIDRe  = regexp.MustCompile(`^[0-9a-f-]{8,64}$`)
)

// readClaudeSessions lists live interactive Claude processes. A file counts only while its pid is
// alive and has the start time Claude recorded: pids wrap every few minutes on this box.
func readClaudeSessions(dir string, startOf func(pid int) (procStat, bool)) []claudeProc {
	ents, _ := os.ReadDir(dir)
	var out []claudeProc
	for _, e := range ents {
		m := claudeFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var f struct {
			PID             int    `json:"pid"`
			SessionID       string `json:"sessionId"`
			Cwd             string `json:"cwd"`
			Name            string `json:"name"`
			ProcStart       string `json:"procStart"`
			Status          string `json:"status"`
			Kind            string `json:"kind"`
			WaitingFor      string `json:"waitingFor"`
			StatusUpdatedAt int64  `json:"statusUpdatedAt"`
			StartedAt       int64  `json:"startedAt"`
		}
		if json.Unmarshal(b, &f) != nil || strconv.Itoa(f.PID) != m[1] || (f.Kind != "" && f.Kind != "interactive") {
			continue // `claude -p` from a hook or tool isn't the agent on screen
		}
		want, err := strconv.ParseUint(f.ProcStart, 10, 64)
		st, alive := startOf(f.PID)
		if err != nil || !alive || st.Start != want {
			continue
		}
		if !sessionIDRe.MatchString(f.SessionID) {
			f.SessionID = ""
		}
		out = append(out, claudeProc{
			PID: f.PID, SessionID: f.SessionID, Cwd: trunc(f.Cwd, 300), Name: trunc(f.Name, 80),
			Status: trunc(strings.ToLower(f.Status), 20), WaitingFor: trunc(f.WaitingFor, 80), Since: f.StatusUpdatedAt / 1000, Started: f.StartedAt / 1000,
			State: st.State, PGRP: st.PGRP,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// ccStatus reads what Claude Code hands its status line: ~/.claude/statusline.sh keeps the latest stdin per
// session in ~/.cache/claude-statusline/<session>.json. That is Claude Code's own count of the session's
// cost and the size of its context window (the panel otherwise infers it from the model). Re-read only when
// the file changed.
type ccStatus struct {
	dir string
	mu  sync.Mutex
	m   map[string]ccEntry
}

type ccStat struct {
	Cost   float64 // USD so far this session
	Window int64   // tokens
}

type ccEntry struct {
	mtime time.Time
	st    ccStat
}

func statusLineDir(home string) string {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "claude-statusline")
	}
	return filepath.Join(home, ".cache", "claude-statusline")
}

func (c *ccStatus) of(sid string) ccStat {
	if !sessionIDRe.MatchString(sid) || c.dir == "" {
		return ccStat{}
	}
	p := filepath.Join(c.dir, sid+".json")
	fi, err := os.Stat(p)
	if err != nil {
		return ccStat{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[sid]; ok && e.mtime.Equal(fi.ModTime()) {
		return e.st
	}
	var v struct {
		Cost struct {
			Total float64 `json:"total_cost_usd"`
		} `json:"cost"`
		Window struct {
			Size int64 `json:"context_window_size"`
		} `json:"context_window"`
	}
	b, err := os.ReadFile(p)
	if err != nil || json.Unmarshal(b, &v) != nil {
		return ccStat{}
	}
	if c.m == nil {
		c.m = map[string]ccEntry{}
	}
	st := ccStat{Cost: v.Cost.Total, Window: v.Window.Size}
	c.m[sid] = ccEntry{fi.ModTime(), st}
	return st
}
