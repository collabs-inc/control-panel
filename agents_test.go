package main

import (
	"path/filepath"
	"testing"
)

const dev1Status = `{"sessions":[
 {"pane_id":0,"run_id":"a","tab_num":1,"tab_name":"PMF CGP","icon":"✓","detail":"28h ago","activity":"Idle","cwd":null,"agent_kind":"claude","agent_title":null,"active_subagents":0,"parent_stopped":true},
 {"pane_id":4,"run_id":"b","tab_num":5,"tab_name":"Griffin","icon":"⏸","detail":"","activity":"Waiting","cwd":"/w/griffin","agent_kind":"claude","agent_title":"fix tests","active_subagents":0,"parent_stopped":false},
 {"pane_id":2,"run_id":"c","tab_num":3,"tab_name":"Facts","icon":"⚡","detail":"Bash","activity":"Tool","cwd":"/w/facts","agent_kind":"codex","agent_title":null,"active_subagents":2,"parent_stopped":false},
 {"pane_id":1,"run_id":"d","tab_num":2,"tab_name":"U42","icon":"●","detail":"12s","activity":"Thinking","cwd":"/w/u42","agent_kind":"claude","agent_title":null,"active_subagents":0,"parent_stopped":false}
],"counts":{"active":2,"waiting":1,"done":0},"updated_at":1790709790}`

func TestAgentsPollLiveSessionsOnly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "dev-1.json"), dev1Status)
	writeFile(t, filepath.Join(dir, "didactic-piano.json"), dev1Status) // stale: session no longer live
	a := &agentsTracker{dir: dir}
	got := a.poll([]string{"box-dev", "dev-1"})
	if len(got.Sessions) != 2 || got.Sessions[0].Name != "dev-1" || got.Sessions[1].Name != "box-dev" {
		t.Fatalf("sessions = %+v (want dev-1 with agents first, then box-dev)", got.Sessions)
	}
	s := got.Sessions[0]
	order := []string{}
	for _, p := range s.Panes {
		order = append(order, p.Activity)
	}
	if len(order) != 4 || order[0] != "Waiting" || order[3] != "Idle" {
		t.Fatalf("pane order = %v, want Waiting first and Idle last", order)
	}
	if s.Panes[0].Cwd == nil || *s.Panes[0].Cwd != "/w/griffin" || s.Panes[0].AgentTitle == nil || s.Updated != 1790709790 {
		t.Fatalf("pane 0 = %+v", s.Panes[0])
	}
	if got.Waiting != 1 || got.Working != 2 {
		t.Fatalf("waiting=%d working=%d", got.Waiting, got.Working)
	}
	if len(got.Sessions[1].Panes) != 0 {
		t.Fatalf("box-dev has no status file, want no panes: %+v", got.Sessions[1])
	}
}

func TestAgentsNoZellij(t *testing.T) {
	a := &agentsTracker{dir: "/nonexistent-status-dir"}
	got := a.poll(nil)
	if len(got.Sessions) != 0 || got.Waiting != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestAgentsCorruptStatusFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "dev-1.json"), `{"sessions":[{"pane_id":`)
	got := (&agentsTracker{dir: dir}).poll([]string{"dev-1"})
	if len(got.Sessions) != 1 || got.Sessions[0].Err == "" {
		t.Fatalf("want the session listed with an error, got %+v", got)
	}
}

func TestAgentsClampsUnknownActivity(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "dev-1.json"), `{"sessions":[{"pane_id":1,"tab_num":1,"tab_name":"x","activity":"<meta http-equiv=refresh content=0;url=https://evil>"}]}`)
	got := (&agentsTracker{dir: dir}).poll([]string{"dev-1"})
	if a := got.Sessions[0].Panes[0].Activity; a != "Unknown" {
		t.Fatalf("activity = %q", a)
	}
}
