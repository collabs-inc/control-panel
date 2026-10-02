package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func toolLine(ts, name, input string) string {
	return `{"type":"assistant","timestamp":"` + ts + `","message":{"content":[{"type":"tool_use","id":"x","name":"` + name + `","input":` + input + `}]}}`
}

// artifactFixture: a session in a checkout that wrote a report, edited main.go, and whose subagent wrote a file outside it.
func artifactFixture(t *testing.T) (transcript, tree, outside string) {
	dir := t.TempDir()
	tree = filepath.Join(dir, "wt")
	os.MkdirAll(filepath.Join(tree, "docs"), 0o755)
	os.WriteFile(filepath.Join(tree, "docs", "report.md"), []byte("# Report\nall good\n"), 0o644)
	os.WriteFile(filepath.Join(tree, "main.go"), []byte("package main\n"), 0o644)
	outside = filepath.Join(dir, "elsewhere.txt")
	os.WriteFile(outside, []byte("hi"), 0o644)
	transcript = filepath.Join(dir, "aaaaaaaa-0001.jsonl")
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	body := strings.Join([]string{
		toolLine("2026-09-30T01:00:00Z", "Write", `{"file_path":`+q(filepath.Join(tree, "docs", "report.md"))+`,"content":"# Report\nall good\n"}`),
		toolLine("2026-09-30T01:00:05Z", "Edit", `{"file_path":`+q(filepath.Join(tree, "main.go"))+`,"old_string":"a","new_string":"b\nc"}`),
		toolLine("2026-09-30T01:00:09Z", "Edit", `{"file_path":`+q(filepath.Join(tree, "docs", "report.md"))+`,"old_string":"all good","new_string":"all very good"}`),
		toolLine("2026-09-30T01:00:10Z", "Bash", `{"command":"ls"}`),
		toolLine("2026-09-30T01:00:11Z", "Write", `{"file_path":`+q(filepath.Join(tree, "gone.txt"))+`,"content":"x"}`),
	}, "\n") + "\n"
	os.WriteFile(transcript, []byte(body), 0o600)
	sub := filepath.Join(dir, "aaaaaaaa-0001", "subagents")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "agent-a1.jsonl"), []byte(toolLine("2026-09-30T01:00:20Z", "Write", `{"file_path":`+q(outside)+`,"content":"hi"}`)+"\n"), 0o600)
	return transcript, tree, outside
}

func TestArtifactsFromTranscripts(t *testing.T) {
	transcript, tree, outside := artifactFixture(t)
	var c artifactCache
	got := c.list(transcript, tree)
	byPath := map[string]Artifact{}
	for _, a := range got {
		byPath[a.Path] = a
	}
	rep := byPath[filepath.Join(tree, "docs", "report.md")]
	if !rep.Created || rep.Edits != 2 || rep.Add != 3 || rep.Del != 1 || !rep.Exists || !rep.InTree || rep.Last != 1790730009 {
		t.Fatalf("report.md = %+v", rep)
	}
	if m := byPath[filepath.Join(tree, "main.go")]; m.Created || m.Edits != 1 || m.Add != 2 || m.Del != 1 {
		t.Fatalf("main.go = %+v", m)
	}
	if g := byPath[filepath.Join(tree, "gone.txt")]; g.Exists {
		t.Fatalf("gone.txt should not exist: %+v", g)
	}
	if o := byPath[outside]; !o.Exists || o.InTree {
		t.Fatalf("subagent file outside the tree = %+v", o)
	}
	if len(got) != 4 || got[0].Path != outside {
		t.Fatalf("want 4 files, newest first: %+v", got)
	}
	// incremental: an appended edit is picked up without rescanning
	f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(toolLine("2026-09-30T02:00:00Z", "Edit", `{"file_path":"`+filepath.Join(tree, "main.go")+`","old_string":"x","new_string":"y"}`) + "\n")
	f.Close()
	for _, a := range c.list(transcript, tree) {
		if a.Path == filepath.Join(tree, "main.go") && (a.Edits != 2 || a.Last != 1790733600) {
			t.Fatalf("after append: %+v", a)
		}
	}
}

func TestFileEndpointsOnlyServeSessionArtifacts(t *testing.T) {
	e := newTestServer(t)
	h := e.authed(e.login(t))
	transcript, tree, _ := artifactFixture(t)
	e.s.term.mu.Lock()
	e.s.term.last = Terminals{Sessions: []TSession{{Name: "dev-1", Tabs: []TTab{{Panes: []TPane{{ID: 3,
		Agent: &TAgent{Chat: true, sid: "aaaaaaaa-0001", path: transcript}, Git: &GitInfo{Root: tree, Branch: "feat/x"}}}}}}}}
	e.s.term.mu.Unlock()
	r := e.do(t, "GET", "/api/pane/files?session=dev-1&pane=3", "", h)
	var list struct {
		Files []Artifact
		Root  string
	}
	json.NewDecoder(r.Body).Decode(&list)
	if r.StatusCode != 200 || len(list.Files) != 4 || list.Root != tree {
		t.Fatalf("files: %d %+v", r.StatusCode, list)
	}
	md := filepath.Join(tree, "docs", "report.md")
	r = e.do(t, "GET", "/api/pane/file?session=dev-1&pane=3&path="+md, "", h)
	var f struct{ Kind, Text string }
	json.NewDecoder(r.Body).Decode(&f)
	if r.StatusCode != 200 || f.Kind != "markdown" || !strings.Contains(f.Text, "all good") {
		t.Fatalf("file: %d %+v", r.StatusCode, f)
	}
	for _, p := range []string{e.s.cfg.pinFile, "/etc/passwd", filepath.Join(tree, "docs", "..", "main.go.bak")} {
		if r := e.do(t, "GET", "/api/pane/file?session=dev-1&pane=3&path="+p, "", h); r.StatusCode != 404 {
			t.Errorf("%s: %d (only the session's own files)", p, r.StatusCode)
		}
	}
	os.WriteFile(filepath.Join(tree, "page.html"), []byte("<script>alert(1)</script>"), 0o644)
	f2, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	f2.WriteString(toolLine("2026-09-30T03:00:00Z", "Write", `{"file_path":"`+filepath.Join(tree, "page.html")+`","content":"x"}`) + "\n")
	f2.Close()
	r = e.do(t, "GET", "/api/pane/file?session=dev-1&pane=3&raw=1&path="+filepath.Join(tree, "page.html"), "", h)
	b, _ := io.ReadAll(r.Body)
	csp := r.Header.Get("Content-Security-Policy")
	// scripts run (mockups need them) but in an opaque origin: no allow-same-origin, no calls home
	if strings.Contains(csp, "allow-same-origin") || !strings.Contains(csp, "sandbox allow-scripts") || !strings.Contains(csp, "connect-src 'none'") {
		t.Fatalf("raw html policy = %q", csp)
	}
	if r.StatusCode != 200 || !strings.Contains(csp, "sandbox") || !strings.Contains(string(b), "<script>") {
		t.Fatalf("raw html must be sandboxed: %d %q", r.StatusCode, r.Header.Get("Content-Security-Policy"))
	}
}

func TestArtifactsIncludeDocumentsAMessageHandsOver(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "out", "report.html") // made by a script, so no Write tool use names it
	os.MkdirAll(filepath.Dir(report), 0o755)
	os.WriteFile(report, []byte("<h1>hi</h1>"), 0o644)
	transcript := filepath.Join(dir, "bbbbbbbb-0001.jsonl")
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	line := `{"type":"assistant","cwd":"` + dir + `","timestamp":"` + ts + `","message":{"content":[{"type":"text","text":"Report: out/report.html"}]}}`
	os.WriteFile(transcript, []byte(line+"\n"), 0o600)
	var c artifactCache
	got := c.list(transcript, "")
	if len(got) != 1 || got[0].Path != report || !got[0].Mentioned || !got[0].Exists {
		t.Fatalf("artifacts = %+v", got)
	}
}
