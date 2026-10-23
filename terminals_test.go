package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPaneState(t *testing.T) {
	now := time.Unix(1790000000, 0)
	shell := paneInfo{Root: 11, Comm: "zsh", FgPID: 11, FgComm: "zsh", FgState: "S"}
	busy := &agentInfo{cp: claudeProc{Status: "busy", Since: now.Unix() - 60}, tail: tailInfo{doing: "Bash: go test", mtime: now.Add(-10 * time.Second)}}
	quiet := &agentInfo{cp: claudeProc{Status: "busy", Since: now.Unix() - 3600}, tail: tailInfo{doing: "Bash: sleep", mtime: now.Add(-20 * time.Minute)}}
	cases := []struct {
		name  string
		p     zPane
		in    paneInfo
		ag    *agentInfo
		fb    *Pane
		d     int64
		kind  string
		state string
		det   string
	}{
		{"exited", zPane{Exited: true, ExitStatus: ptr(2)}, shell, nil, nil, 0, "shell", "exited", "exit 2"},
		{"claude busy", zPane{}, paneInfo{Root: 11, Comm: "zsh", Agent: "claude", Output: now.Unix()}, busy, nil, 0, "claude", "working", "Bash: go test"},
		{"claude quiet 20m, idle cpu", zPane{}, paneInfo{Root: 11, Comm: "zsh", Agent: "claude", Output: now.Unix() - 1200, CPU: 0.5}, quiet, nil, 0, "claude", "stalled", "No activity for 20m"},
		{"claude quiet but busy cpu", zPane{}, paneInfo{Root: 11, Comm: "zsh", Agent: "claude", Output: now.Unix() - 1200, CPU: 90}, quiet, nil, 0, "claude", "working", "Bash: sleep"},
		{"claude permission", zPane{}, paneInfo{Root: 11, Agent: "claude"}, &agentInfo{cp: claudeProc{Status: "waiting", WaitingFor: "permission prompt"}}, nil, 0, "claude", "asking", "permission prompt"},
		{"claude between turns, its background command running", zPane{}, paneInfo{Root: 11, Agent: "claude", Output: now.Unix() - 1200}, &agentInfo{cp: claudeProc{Status: "shell", Since: now.Unix() - 1200}, tail: tailInfo{mtime: now.Add(-20 * time.Minute)}}, nil, 0, "claude", "background", "Background command running"},
		{"claude between turns, naming what runs", zPane{}, paneInfo{Root: 11, Agent: "claude"}, &agentInfo{cp: claudeProc{Status: "shell"}, shells: []Shell{{Desc: "Run the tests"}, {Cmd: "tail -f log"}}}, nil, 0, "claude", "background", "In the background: Run the tests and 1 more"},
		{"claude idle", zPane{}, paneInfo{Root: 11, Agent: "claude"}, &agentInfo{cp: claudeProc{Status: "idle"}, tail: tailInfo{title: "Fix CI"}}, nil, 0, "claude", "waiting", "Fix CI"},
		{"hook fallback for codex", zPane{}, paneInfo{Root: 11, Agent: "codex"}, nil, &Pane{Activity: "Waiting", AgentKind: "codex", Detail: "approve?"}, 0, "codex", "asking", "approve?"},
		{"stale hook entry, claude gone", zPane{}, shell, nil, &Pane{Activity: "Thinking", AgentKind: "claude"}, 0, "shell", "idle", ""},
		{"shell prompt", zPane{}, shell, nil, nil, 0, "shell", "idle", ""},
		{"running cmd", zPane{}, paneInfo{Root: 11, Comm: "zsh", FgPID: 950, FgState: "R", FgCmd: "python3 train.py"}, nil, nil, 0, "shell", "running", "python3 train.py"},
		{"stopped", zPane{}, paneInfo{Root: 11, Comm: "zsh", FgPID: 950, FgState: "T", FgCmd: "vim x"}, nil, nil, 0, "shell", "stopped", "vim x"},
		{"D state 5s", zPane{}, paneInfo{Root: 11, Comm: "zsh", FgPID: 950, FgState: "D", FgCmd: "cp big"}, nil, nil, now.Unix() - 5, "shell", "running", "cp big"},
		{"D state 40s", zPane{}, paneInfo{Root: 11, Comm: "zsh", FgPID: 950, FgState: "D", FgCmd: "cp big"}, nil, nil, now.Unix() - 40, "shell", "stuck", "cp big"},
		{"command pane", zPane{Command: "htop"}, paneInfo{Root: 30, Comm: "htop", FgPID: 30, FgState: "S", FgCmd: "htop"}, nil, nil, 0, "cmd", "running", "htop"},
		{"no process seen yet", zPane{}, paneInfo{}, nil, nil, 0, "shell", "unknown", ""},
		{"claude before its session file", zPane{}, paneInfo{Root: 11, Comm: "zsh", Agent: "claude", FgPID: 90}, nil, nil, 0, "claude", "starting", "Starting"},
		{"hook entry with an unknown activity", zPane{}, paneInfo{Root: 11, Comm: "zsh", Agent: "claude", FgPID: 90}, nil, &Pane{Activity: "Unknown", AgentKind: "claude"}, 0, "claude", "starting", "Starting"},
	}
	for _, c := range cases {
		kind, state, det, _ := paneState(c.p, c.in, c.ag, c.fb, nil, now, c.d)
		if kind != c.kind || state != c.state || det != c.det {
			t.Errorf("%s: got %s/%s/%q, want %s/%s/%q", c.name, kind, state, det, c.kind, c.state, c.det)
		}
	}
}

func ptr(n int) *int { return &n }

func TestPaneStateTruthOverStaleSignals(t *testing.T) {
	now := time.Now()
	in := paneInfo{Root: 11, Comm: "zsh", Agent: "claude", FgPID: 90}
	cases := []struct {
		name       string
		in         paneInfo
		ag         *agentInfo
		fb         *Pane
		probe      *Prompt
		state, det string
	}{
		{"claude stopped (Ctrl-Z or paused)", in, &agentInfo{cp: claudeProc{Status: "busy", State: "T"}}, nil, nil, "stopped", "Stopped"},
		{"claude probe beats a stale hook", in, nil, &Pane{Activity: "Thinking"}, &Prompt{Q: "Trust?"}, "asking", "Needs your answer"},
		{"claude, no prompt on screen: hook fallback", in, nil, &Pane{Activity: "Thinking", Detail: "Bash"}, nil, "working", "Bash"},
		{"codex: hook first", paneInfo{Root: 11, Agent: "codex"}, nil, &Pane{Activity: "Thinking", Detail: "x"}, &Prompt{Q: "?"}, "working", "x"},
	}
	for _, c := range cases {
		if _, state, det, _ := paneState(zPane{}, c.in, c.ag, c.fb, c.probe, now, 0); state != c.state || det != c.det {
			t.Errorf("%s: got %s/%q, want %s/%q", c.name, state, det, c.state, c.det)
		}
	}
}

func TestPaneStateAgentWithoutSessionFileAsking(t *testing.T) {
	in := paneInfo{Root: 11, Comm: "zsh", Agent: "claude", FgPID: 90}
	kind, state, det, _ := paneState(zPane{}, in, nil, nil, &Prompt{Q: "Trust?"}, time.Now(), 0)
	if kind != "claude" || state != "asking" || det != "Needs your answer" {
		t.Fatalf("got %s/%s/%q", kind, state, det)
	}
}

func TestTermTrackerUpdate(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, "sessions")
	writeFile(t, filepath.Join(claudeDir, "950.json"), `{"pid":950,"sessionId":"aaaaaaaa-0950","procStart":"7","status":"waiting","waitingFor":"permission prompt"}`)
	writeFile(t, filepath.Join(claudeDir, "960.json"), `{"pid":960,"sessionId":"aaaaaaaa-0960","procStart":"7","status":"idle"}`)
	writeFile(t, filepath.Join(claudeDir, "970.json"), `{"pid":970,"sessionId":"aaaaaaaa-0970","procStart":"7","status":"busy"}`) // not in zellij
	z, f := newFakeZellij(map[string]string{
		"--session dev-1 action list-panes": `[
		 {"id":3,"title":"b","tab_id":5,"tab_position":1,"tab_name":"Second"},
		 {"id":1,"title":"a","tab_id":2,"tab_position":0,"tab_name":"First"},
		 {"id":9,"title":"float","is_floating":true,"tab_id":2,"tab_position":0,"tab_name":"First"},
		 {"id":4,"title":"c","tab_id":2,"tab_position":0,"tab_name":"First"}]`,
		"--session dev-1 action dump-screen": promptScreen,
	})
	tt := newTermTracker(z, claudeDir, newTranscripts(filepath.Join(dir, "projects")), &agentsTracker{dir: filepath.Join(dir, "status")})
	tt.startOf = func(pid int) (procStat, bool) { return procStat{Start: 7, PGRP: pid}, true }
	res := Resources{panes: map[string]paneInfo{
		"pane:dev-1:1": {Root: 11, Comm: "zsh", Agent: "claude"},
		"pane:dev-1:4": {Root: 12, Comm: "zsh", FgPID: 12},
		"pane:dev-1:3": {Root: 13, Comm: "zsh", Agent: "claude"},
		"pane:dev-1:9": {Root: 14, Comm: "zsh", FgPID: 14},
	}}
	keys := map[int]string{950: "pane:dev-1:1", 960: "pane:dev-1:3", 970: "zellij"}
	got := tt.update(now, []string{"dev-1"}, res, func(pid int) string { return keys[pid] }, true)

	if len(got.Sessions) != 1 || len(got.Sessions[0].Tabs) != 2 {
		t.Fatalf("structure: %+v", got)
	}
	var order []string
	for _, tb := range got.Sessions[0].Tabs {
		for _, p := range tb.Panes {
			order = append(order, tb.Name+"/"+itoa(p.ID)+"="+p.State)
		}
	}
	if strings.Join(order, " ") != "First/1=asking First/4=idle First/9=idle Second/3=waiting" {
		t.Fatalf("order/state = %v", order)
	}
	if got.Asking != 1 || got.Waiting != 2 || got.Working != 0 || got.Panes != 4 || got.Agents != 2 {
		t.Fatalf("counts = %+v", got)
	}
	a := got.Sessions[0].Tabs[0].Panes[0].Agent
	if a == nil || a.Prompt == nil || a.Prompt.Q != "Do you want to proceed?" || a.sid != "aaaaaaaa-0950" {
		t.Fatalf("asking agent = %+v", a)
	}
	// the prompt is read once per waiting spell, not every tick
	n := len(f.calls)
	tt.update(now.Add(3*time.Second), []string{"dev-1"}, res, func(pid int) string { return keys[pid] }, true)
	for _, c := range f.calls[n:] {
		if c[3] == "dump-screen" {
			t.Fatal("dump-screen repeated for the same waiting spell")
		}
	}
	// without a Terminals page open: counts only, no zellij calls within 30 s
	n = len(f.calls)
	light := tt.update(now.Add(4*time.Second), []string{"dev-1"}, res, func(pid int) string { return keys[pid] }, false)
	if len(f.calls) != n || light.Waiting != 2 || light.Asking != 1 {
		t.Fatalf("light update: calls=%d counts=%+v", len(f.calls)-n, light)
	}
	agents := 0 // the notifications read the agents' states from this update too
	for _, s := range light.Sessions {
		for _, tb := range s.Tabs {
			for _, p := range tb.Panes {
				if p.Agent != nil && p.Agent.sid != "" && p.Agent.Prompt == nil {
					agents++
				}
			}
		}
	}
	if agents == 0 {
		t.Fatalf("light update lists no agent panes: %+v", light.Sessions)
	}
	if l := tt.labels(); l["pane:dev-1:3"] != "Second · dev-1" {
		t.Fatalf("labels = %v", l)
	}
}

func newTestTracker(t *testing.T, sessions map[string]string, answers map[string]string) (*termTracker, *fakeZellij) {
	dir := t.TempDir()
	for name, body := range sessions {
		writeFile(t, filepath.Join(dir, "sessions", name), body)
	}
	z, f := newFakeZellij(answers)
	tt := newTermTracker(z, filepath.Join(dir, "sessions"), newTranscripts(filepath.Join(dir, "projects")), &agentsTracker{dir: filepath.Join(dir, "status")})
	return tt, f
}

func TestTermTrackerPicksTheBlockedClaudeInAPane(t *testing.T) {
	tt, _ := newTestTracker(t, map[string]string{
		"950.json": `{"pid":950,"sessionId":"aaaaaaaa-0950","procStart":"7","status":"waiting","waitingFor":"permission prompt","statusUpdatedAt":1000000}`,
		"951.json": `{"pid":951,"sessionId":"aaaaaaaa-0951","procStart":"7","status":"busy","statusUpdatedAt":2000000}`,
		"960.json": `{"pid":960,"sessionId":"aaaaaaaa-0960","procStart":"7","status":"idle","statusUpdatedAt":1000000}`,
		"961.json": `{"pid":961,"sessionId":"aaaaaaaa-0961","procStart":"7","status":"busy","statusUpdatedAt":2000000}`,
	}, map[string]string{"--session s action list-panes": `[{"id":1,"tab_id":0},{"id":2,"tab_id":0}]`})
	pgrp := map[int]int{950: 90, 951: 90, 960: 80, 961: 81} // pane 2: only 960 is in the foreground group
	tt.startOf = func(pid int) (procStat, bool) { return procStat{Start: 7, PGRP: pgrp[pid]}, true }
	res := Resources{panes: map[string]paneInfo{
		"pane:s:1": {Root: 11, Comm: "zsh", Agent: "claude", FgPID: 90},
		"pane:s:2": {Root: 12, Comm: "zsh", Agent: "claude", FgPID: 80},
	}}
	keys := map[int]string{950: "pane:s:1", 951: "pane:s:1", 960: "pane:s:2", 961: "pane:s:2"}
	got := tt.update(time.Now(), []string{"s"}, res, func(pid int) string { return keys[pid] }, true)
	ps := got.Sessions[0].Tabs[0].Panes
	if ps[0].State != "asking" || ps[1].State != "waiting" {
		t.Fatalf("pane 1 = %s (want asking: waiting beats busy), pane 2 = %s (want waiting: the foreground claude)", ps[0].State, ps[1].State)
	}
}

func TestTermTrackerExitedPaneAndPromptRefresh(t *testing.T) {
	tt, f := newTestTracker(t, map[string]string{
		"950.json": `{"pid":950,"sessionId":"aaaaaaaa-0950","procStart":"7","status":"waiting","statusUpdatedAt":1000000}`,
	}, map[string]string{
		"--session s action list-panes":  `[{"id":1,"tab_id":0},{"id":2,"tab_id":0,"exited":true,"exit_status":3}]`,
		"--session s action dump-screen": "no prompt here yet",
	})
	tt.startOf = func(pid int) (procStat, bool) { return procStat{Start: 7}, true }
	res := Resources{panes: map[string]paneInfo{"pane:s:1": {Root: 11, Comm: "zsh", Agent: "claude"}}} // pane 2 has no process left
	keyOf := func(pid int) string { return map[int]string{950: "pane:s:1"}[pid] }
	now := time.Now()
	got := tt.update(now, []string{"s"}, res, keyOf, true)
	if p := got.Sessions[0].Tabs[0].Panes[1]; p.State != "exited" || p.Detail != "exit 3" {
		t.Fatalf("held pane = %+v", p)
	}
	if got.Sessions[0].Tabs[0].Panes[0].Agent.Prompt != nil {
		t.Fatal("prompt from a screen without one")
	}
	f.answers["--session s action dump-screen"] = promptScreen // the dialog finished rendering
	got = tt.update(now.Add(6*time.Second), []string{"s"}, res, keyOf, true)
	if p := got.Sessions[0].Tabs[0].Panes[0].Agent.Prompt; p == nil || p.Q != "Do you want to proceed?" || p.Sig == "" {
		t.Fatalf("prompt not re-read after 5 s: %+v", p)
	}
}

func TestTermTrackerShowsBranchAndWorktree(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/feat/panel\n"), 0o644)
	shellDir := t.TempDir()
	os.MkdirAll(filepath.Join(shellDir, ".git"), 0o755)
	os.WriteFile(filepath.Join(shellDir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	tt, _ := newTestTracker(t, map[string]string{
		"950.json": `{"pid":950,"sessionId":"aaaaaaaa-0950","procStart":"7","cwd":"` + repo + `","status":"idle"}`,
	}, map[string]string{"--session s action list-panes": `[{"id":1,"tab_id":0},{"id":2,"tab_id":0}]`})
	tt.startOf = func(pid int) (procStat, bool) { return procStat{Start: 7}, true }
	res := Resources{panes: map[string]paneInfo{
		"pane:s:1": {Root: 11, Comm: "zsh", Agent: "claude", Cwd: "/somewhere/else"},
		"pane:s:2": {Root: 12, Comm: "zsh", FgPID: 12, Cwd: shellDir},
	}}
	got := tt.update(time.Now(), []string{"s"}, res, func(pid int) string { return map[int]string{950: "pane:s:1"}[pid] }, true)
	ps := got.Sessions[0].Tabs[0].Panes
	if ps[0].Git == nil || ps[0].Git.Branch != "feat/panel" || ps[0].Git.Root != repo {
		t.Fatalf("agent pane git (from Claude's cwd) = %+v", ps[0].Git)
	}
	if ps[1].Git == nil || ps[1].Git.Branch != "main" {
		t.Fatalf("shell pane git (from its cwd) = %+v", ps[1].Git)
	}
}

func TestTermTrackerForegroundProcess(t *testing.T) {
	tt, _ := newTestTracker(t, nil, map[string]string{"--session s action list-panes": `[{"id":1,"tab_id":0},{"id":2,"tab_id":0}]`})
	res := Resources{panes: map[string]paneInfo{
		"pane:s:1": {Root: 11, Comm: "zsh", FgPID: 950, FgState: "R", FgCmd: "python3 train.py"},
		"pane:s:2": {Root: 12, Comm: "zsh", FgPID: 12, FgCmd: "zsh"},
	}}
	ps := tt.update(time.Now(), []string{"s"}, res, func(int) string { return "" }, true).Sessions[0].Tabs[0].Panes
	if ps[0].Fg != 950 || ps[0].Proc != "python3 train.py" || ps[1].Fg != 0 {
		t.Fatalf("fg = %d %q / %d", ps[0].Fg, ps[0].Proc, ps[1].Fg)
	}
}

func TestAgentNamesNameSessionsByTheirTab(t *testing.T) {
	tm := Terminals{Sessions: []TSession{{Name: "dev-1", Tabs: []TTab{
		{Name: "Summit Issue", Panes: []TPane{{ID: 1, Agent: &TAgent{sid: "s1"}}}},
		{Name: "Two panes", Panes: []TPane{{ID: 2, Title: "left", Agent: &TAgent{sid: "s2"}}, {ID: 3, Agent: &TAgent{sid: "s3"}}, {ID: 4}}},
	}}}}
	got := namesOf(tm)
	want := map[string]string{"s1": "Summit Issue", "s2": "Two panes · left", "s3": "Two panes · pane 3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v", got)
	}
}
