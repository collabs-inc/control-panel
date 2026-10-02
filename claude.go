package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
			Status: trunc(strings.ToLower(f.Status), 20), WaitingFor: trunc(f.WaitingFor, 80), Since: f.StatusUpdatedAt / 1000,
			State: st.State, PGRP: st.PGRP,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}
