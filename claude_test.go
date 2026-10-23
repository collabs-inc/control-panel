package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadClaudeSessions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "100.json"), `{"pid":100,"sessionId":"aaaaaaaa-0100","cwd":"/w/a","procStart":"555","name":"a-1","status":"waiting","waitingFor":"permission prompt","statusUpdatedAt":1790733474836}`)
	writeFile(t, filepath.Join(dir, "200.json"), `{"pid":200,"sessionId":"aaaaaaaa-0200","cwd":"/w/b","procStart":"777","status":"idle","statusUpdatedAt":1790732536423}`)
	writeFile(t, filepath.Join(dir, "300.json"), `{"pid":300,"sessionId":"s-300","procStart":"1","status":"busy"}`) // pid reused by another process
	writeFile(t, filepath.Join(dir, "400.json"), `{"pid":400,"sessionId":"s-400","procStart":"9","status":"busy"}`) // dead
	writeFile(t, filepath.Join(dir, "500.json"), `{"pid":501,"sessionId":"s-500","procStart":"5","status":"busy"}`) // file name and pid disagree
	writeFile(t, filepath.Join(dir, "600.json"), `not json`)
	writeFile(t, filepath.Join(dir, "100.abcdef.key"), `{"peerToken":"x"}`)
	writeFile(t, filepath.Join(dir, "700.json"), `{"pid":700,"sessionId":"aaaaaaaa-0700","procStart":"5","kind":"print","status":"busy"}`) // claude -p from a hook
	start := map[int]uint64{100: 555, 200: 777, 300: 2, 501: 5, 500: 5, 700: 5}
	got := readClaudeSessions(dir, func(pid int) (procStat, bool) {
		s, ok := start[pid]
		return procStat{Start: s, State: map[int]string{100: "T"}[pid], PGRP: pid - 1}, ok
	})
	if len(got) != 2 {
		t.Fatalf("got %d sessions: %+v", len(got), got)
	}
	a, b := got[0], got[1]
	if a.PID != 100 || a.SessionID != "aaaaaaaa-0100" || a.Cwd != "/w/a" || a.Status != "waiting" || a.WaitingFor != "permission prompt" || a.Since != 1790733474 || a.Name != "a-1" {
		t.Fatalf("a = %+v", a)
	}
	if a.State != "T" || a.PGRP != 99 {
		t.Fatalf("proc state not carried: %+v", a)
	}
	if b.PID != 200 || b.Status != "idle" || b.Since != 1790732536 {
		t.Fatalf("b = %+v", b)
	}
}

func TestReadClaudeSessionsBoundsFields(t *testing.T) {
	dir := t.TempDir()
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'x'
	}
	writeFile(t, filepath.Join(dir, "100.json"), `{"pid":100,"sessionId":"../../etc","procStart":"5","status":"`+string(long)+`","waitingFor":"`+string(long)+`"}`)
	got := readClaudeSessions(dir, func(int) (procStat, bool) { return procStat{Start: 5}, true })
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if got[0].SessionID != "" || len(got[0].Status) > 20 || len(got[0].WaitingFor) > 80 {
		t.Fatalf("unbounded fields: %+v", got[0])
	}
}

func TestStatusLineHandsOverCostAndWindow(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a3f65479-3637-4206-9a5b-220925853da4.json"), []byte(`{"session_id":"a3f65479-3637-4206-9a5b-220925853da4","cost":{"total_cost_usd":807.3249},"context_window":{"context_window_size":1000000,"used_percentage":74}}`), 0o600)
	c := ccStatus{dir: dir}
	st := c.of("a3f65479-3637-4206-9a5b-220925853da4")
	if st.Cost != 807.3249 || st.Window != 1_000_000 {
		t.Fatalf("status = %+v", st)
	}
	if st := c.of("../../etc/passwd"); st != (ccStat{}) {
		t.Fatalf("a session id that isn't one reads nothing: %+v", st)
	}
	if st := c.of("00000000-0000-0000-0000-000000000000"); st != (ccStat{}) {
		t.Fatalf("no file: nothing: %+v", st)
	}
}
