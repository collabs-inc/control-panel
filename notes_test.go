package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func agentPane(id int, state, detail string) TPane {
	return TPane{ID: id, Kind: "claude", State: state, Detail: detail, Agent: &TAgent{}}
}

func terms(panes ...TPane) Terminals {
	return Terminals{Sessions: []TSession{{Name: "dev-1", Tabs: []TTab{{Name: "Summit Issues", Panes: panes[:1]}, {Name: "Two panes", Panes: panes[1:]}}}}}
}

func kinds(ns []Note) string {
	var out []string
	for _, n := range ns {
		out = append(out, n.Kind+"|"+n.Title+"|"+n.Body+"|"+n.Go)
	}
	return strings.Join(out, "\n")
}

func TestNotesAnnounceAgentsThatFinishOrNeedYou(t *testing.T) {
	n := newNotifier(filepath.Join(t.TempDir(), "notes.json"))
	said := func(p TPane) string { return "Both are done. The phone list now appends." }
	now := time.Unix(1000, 0)
	look := func(tm Terminals) {
		now = now.Add(2 * time.Second)
		n.observe(now, tm, CI{}, Security{}, Resources{}, said)
	}

	look(terms(agentPane(1, "waiting", ""), agentPane(2, "working", ""), agentPane(3, "asking", "permission prompt")))
	if len(n.Notes) != 0 {
		t.Fatalf("the first look only learns the states:\n%s", kinds(n.Notes))
	}
	look(terms(agentPane(1, "working", ""), agentPane(2, "waiting", ""), agentPane(3, "asking", "permission prompt")))
	look(terms(agentPane(1, "background", ""), agentPane(2, "asking", "Which branch?"), agentPane(3, "waiting", "")))
	look(terms(agentPane(1, "waiting", ""), agentPane(2, "stalled", "No activity for 15m"), agentPane(3, "waiting", "")))
	want := strings.Join([]string{
		"done|Two panes · pane 2 is done|Both are done. The phone list now appends.|pane:dev-1:2",
		"asking|Two panes · pane 2 needs your answer|Which branch?|pane:dev-1:2",
		"done|Summit Issues is done|Both are done. The phone list now appends.|pane:dev-1:1",
		"stalled|Two panes · pane 2 looks stuck|No activity for 15m|pane:dev-1:2",
	}, "\n")
	if got := kinds(n.Notes); got != want {
		t.Fatalf("notes:\n%s\nwant:\n%s\n(an answered prompt going idle is yours, not news)", got, want)
	}
	if n.unread() != 4 {
		t.Fatalf("unread = %d", n.unread())
	}
	n.markSeen(n.Notes[1].ID)
	if n.unread() != 2 {
		t.Fatalf("unread after reading up to the second = %d", n.unread())
	}

	m := newNotifier(n.file)
	if kinds(m.Notes) != want || m.unread() != 2 {
		t.Fatalf("after a restart: unread %d\n%s", m.unread(), kinds(m.Notes))
	}
	m.observe(now, terms(agentPane(1, "working", ""), agentPane(2, "waiting", ""), agentPane(3, "waiting", "")), CI{}, Security{}, Resources{}, said)
	if len(m.Notes) != 4 {
		t.Fatalf("a restart re-learns the states, it doesn't announce them:\n%s", kinds(m.Notes))
	}
}

func TestNotesCatchATurnShorterThanTheLookInterval(t *testing.T) {
	n := newNotifier(filepath.Join(t.TempDir(), "notes.json"))
	said := func(TPane) string { return "Hello" }
	idle := func(since int64) Terminals { p := agentPane(1, "waiting", ""); p.Since = since; return terms(p) }
	now := time.Unix(1000, 0)
	n.observe(now, idle(900), CI{}, Security{}, Resources{}, said)
	n.observe(now.Add(10*time.Second), idle(900), CI{}, Security{}, Resources{}, said)
	n.observe(now.Add(20*time.Second), idle(1015), CI{}, Security{}, Resources{}, said) // busy and idle again in between
	if got := kinds(n.Notes); got != "done|Summit Issues is done|Hello|pane:dev-1:1" {
		t.Fatalf("notes:\n%s", got)
	}
}

func TestNotesAnnounceCIFailuresSecurityAndFullDisks(t *testing.T) {
	n := newNotifier(filepath.Join(t.TempDir(), "notes.json"))
	said := func(TPane) string { return "" }
	job := func(start int64, result string) Job {
		return Job{Runner: "r1", Name: "Static checks", Result: result, Start: start, Meta: &JobMeta{Repo: "o/app", PR: 7, Title: "Speed up totals"}}
	}
	res := func(diskUsed, memAvail uint64) Resources {
		return Resources{MemTotal: 100, MemAvail: memAvail, Disks: []Disk{{Mount: "/workspace", Used: diskUsed, Size: 100}}}
	}
	sec := func(level string) Security {
		return Security{Checks: []Check{{Level: level, Title: "SSH password login", Detail: "PasswordAuthentication yes"}}}
	}
	now := time.Unix(1000, 0)
	n.observe(now, Terminals{}, CI{Recent: []Job{job(1, "Failed")}}, sec("ok"), res(50, 50), said)
	n.observe(now, Terminals{}, CI{Recent: []Job{job(2, "Failed"), job(1, "Failed")}}, sec("bad"), res(93, 5), said)
	n.observe(now, Terminals{}, CI{Recent: []Job{job(3, "Succeeded"), job(2, "Failed"), job(1, "Failed")}}, sec("bad"), res(94, 5), said)
	n.observe(now, Terminals{}, CI{}, sec("ok"), res(80, 50), said)
	n.observe(now, Terminals{}, CI{}, sec("ok"), res(91, 50), said)
	want := strings.Join([]string{
		"ci|CI failed: Static checks|o/app PR #7 · Speed up totals|ci",
		"sec|Security: SSH password login|PasswordAuthentication yes|sec",
		"disk|/workspace is 93% full|93 B of 100 B|res",
		"mem|Memory is 95% used|95 B of 100 B|res",
		"disk|/workspace is 91% full|91 B of 100 B|res",
	}, "\n")
	if got := kinds(n.Notes); got != want {
		t.Fatalf("notes:\n%s\nwant:\n%s\n(a failure is told once; a full disk again only after it went below 85%%)", got, want)
	}
}
