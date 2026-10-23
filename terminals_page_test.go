package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// What the Terminals page needs beyond a pane's state: where it sits in its tab, what a server listens on and
// whether its screen shows errors, and an agent's latest exchange.

func TestScanOutputCountsErrorsAndKnowsWhenTheyAreNew(t *testing.T) {
	old := "starting\nERR stream canceled\nserving on :8080\nWARNING slow disk\nok\nok\n"
	if e, w, now := scanOutput(old); e != 1 || w != 1 || now {
		t.Fatalf("an error up the screen, then healthy lines: %d errors %d warnings now=%v", e, w, now)
	}
	if _, _, now := scanOutput("ok\nok\npanic: nil map\n\n\n"); !now {
		t.Fatal("an error among the newest lines (blank lines at the bottom don't count)")
	}
	if e, w, _ := scanOutput("ERRORS=0 is fine\nwarnings: none\n"); e != 0 || w != 0 {
		t.Fatalf("words that only contain ERR/WARN: %d %d", e, w)
	}
}

func TestTrackerGivesEachTiledPaneItsPlaceInTheTab(t *testing.T) {
	tt, _ := newTestTracker(t, nil, map[string]string{"--session s action list-panes": `[
	 {"id":1,"tab_id":0,"tab_name":"Panel","pane_x":0,"pane_y":0,"pane_columns":25,"pane_rows":49},
	 {"id":2,"tab_id":0,"tab_name":"Panel","pane_x":25,"pane_y":0,"pane_columns":25,"pane_rows":49},
	 {"id":3,"tab_id":0,"tab_name":"Panel","is_floating":true,"pane_x":5,"pane_y":5,"pane_columns":20,"pane_rows":10}]`})
	got := tt.update(time.Now(), []string{"s"}, Resources{panes: map[string]paneInfo{}}, func(int) string { return "" }, true)
	ps := got.Sessions[0].Tabs[0].Panes
	if !reflect.DeepEqual(ps[0].Geo, []int{0, 0, 25, 49}) || !reflect.DeepEqual(ps[1].Geo, []int{25, 0, 25, 49}) || ps[2].Geo != nil {
		t.Fatalf("geo: %v %v %v (a floating pane is no part of the split)", ps[0].Geo, ps[1].Geo, ps[2].Geo)
	}
}

func TestTrackerReadsAServersScreenForErrorsAndShowsItsPorts(t *testing.T) {
	tt, f := newTestTracker(t, nil, map[string]string{
		"--session s action list-panes":  `[{"id":1,"tab_id":0,"tab_name":"api"}]`,
		"--session s action dump-screen": "listening\nERR upstream refused\n",
	})
	res := Resources{panes: map[string]paneInfo{"pane:s:1": {Root: 11, Comm: "bash", FgPID: 12, FgComm: "node", FgCmd: "node server.js", FgState: "S", Ports: []int{3456}}}}
	t0 := time.Now()
	p := tt.update(t0, []string{"s"}, res, func(int) string { return "" }, true).Sessions[0].Tabs[0].Panes[0]
	if p.State != "running" || p.Errs != 1 || !p.ErrNow || !reflect.DeepEqual(p.Ports, []int{3456}) {
		t.Fatalf("server: %+v", p)
	}
	dumps := func() (n int) {
		for _, c := range f.calls {
			if strings.Contains(strings.Join(c, " "), "dump-screen") {
				n++
			}
		}
		return n
	}
	tt.update(t0.Add(5*time.Second), []string{"s"}, res, func(int) string { return "" }, true)
	if dumps() != 1 {
		t.Fatalf("the screen is read again after 15 s, not every update: %d reads", dumps())
	}
	tt.update(t0.Add(5*time.Second), []string{"s"}, res, func(int) string { return "" }, false)
	if dumps() != 1 {
		t.Fatal("without the Terminals page open, nothing reads servers' screens")
	}
}

func TestTailKeepsClaudesLastWords(t *testing.T) {
	root := t.TempDir()
	sid := "f5bafd4f-8844-4ae4-bc85-832fa542a1be"
	p := filepath.Join(root, "-r", sid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(`{"type":"user","timestamp":"2026-10-11T05:00:00Z","message":{"content":"set up the repo"}}
{"type":"assistant","timestamp":"2026-10-11T05:01:00Z","message":{"content":[{"type":"text","text":"Checking the org."}]}}
{"type":"assistant","timestamp":"2026-10-11T05:02:00Z","message":{"content":[{"type":"text","text":"Both folders are now private repos."}],"stop_reason":"end_turn"}}
`), 0o644)
	ti, _ := newTranscripts(root).tail(sid)
	if ti.claude != "Both folders are now private repos." || ti.claudeAt == 0 {
		t.Fatalf("Claude's last words: %+v", ti)
	}
}

func TestMovableSaysWhyATabStays(t *testing.T) {
	agent := func(state string) TTab {
		return TTab{Panes: []TPane{{Kind: "claude", State: state, Agent: &TAgent{sid: "s1", startCwd: "/r"}}}}
	}
	for _, c := range []struct {
		tb   TTab
		want string
	}{
		{agent("waiting"), ""},
		{agent("working"), "working"},
		{agent("asking"), "asking"},
		{TTab{Panes: []TPane{{Kind: "shell", State: "idle"}}}, ""},
		{TTab{Panes: []TPane{{Kind: "shell", State: "running", Fg: 12}}}, "restart"},
		{TTab{Panes: []TPane{{Kind: "shell"}, {Kind: "shell"}}}, "one pane"},
		{TTab{Panes: []TPane{{Kind: "codex", State: "waiting"}}}, "Codex"},
	} {
		if got := movable(c.tb); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%+v: %q, want %q", c.tb.Panes[0], got, c.want)
		}
	}
}

func TestTermMoveReopensTheTabThenResumesClaude(t *testing.T) {
	e := newTestServer(t)
	h := e.authed(e.login(t))
	for _, n := range []string{"dev-1", "box-dev"} {
		os.WriteFile(filepath.Join(e.s.sys.sockDir, n), nil, 0o600)
	}
	cwd := t.TempDir()
	e.s.term.mu.Lock()
	e.s.term.last = Terminals{Sessions: []TSession{
		{Name: "dev-1", Tabs: []TTab{
			{ID: 4, Name: "KPI", Panes: []TPane{{ID: 7, Kind: "claude", State: "waiting", Agent: &TAgent{sid: "aaaa-1", startCwd: cwd}}}},
			{ID: 5, Name: "api", Panes: []TPane{{ID: 8, Kind: "shell", State: "running", Fg: 99}}},
		}},
		{Name: "box-dev"},
	}}
	e.s.term.mu.Unlock()
	moveSettle = 0
	f := e.fakeZ(map[string]string{
		"--session box-dev action list-clients": "CLIENT_ID\n1 term",
		"--session box-dev action new-tab":      "3\n",
		"--session box-dev action list-panes":   `[{"id":11,"tab_id":3,"tab_name":"KPI"}]`,
	})
	if r := e.do(t, "POST", "/api/term/move", `{"session":"dev-1","tab":5,"to":"box-dev"}`, h); r.StatusCode != 409 {
		t.Fatalf("a running server: %d, want refused", r.StatusCode)
	}
	if len(f.calls) != 0 {
		t.Fatalf("a refused move touches nothing: %v", f.calls)
	}
	r := e.do(t, "POST", "/api/term/move", `{"session":"dev-1","tab":4,"to":"box-dev"}`, h)
	var got struct{ Session string }
	json.NewDecoder(r.Body).Decode(&got)
	if r.StatusCode != 200 || got.Session != "box-dev" {
		t.Fatalf("move: %d %+v", r.StatusCode, got)
	}
	var order []string
	for _, c := range f.calls {
		j := strings.Join(c, " ")
		switch {
		case strings.Contains(j, "new-tab"):
			order = append(order, "new-tab in "+c[1]+" at "+c[len(c)-3])
		case strings.Contains(j, "close-pane"):
			order = append(order, "close "+c[1]+" "+c[len(c)-1])
		case strings.Contains(j, "write-chars"):
			order = append(order, "type "+c[len(c)-1])
		case strings.Contains(j, "send-keys"):
			order = append(order, "Enter")
		}
	}
	want := []string{"new-tab in box-dev at " + cwd, "close dev-1 terminal_7", "type claude-multi --resume aaaa-1", "Enter"}
	if strings.Join(order, " | ") != strings.Join(want, " | ") {
		t.Fatalf("steps:\n%s\nwant:\n%s", strings.Join(order, "\n"), strings.Join(want, "\n"))
	}
	if b, _ := os.ReadFile(e.audit); !strings.Contains(string(b), `"event":"move"`) {
		t.Fatalf("audit: %s", b)
	}
}

func TestTailFindsTheLastWordsPastABurstOfToolCalls(t *testing.T) {
	root := t.TempDir()
	sid := "a5bafd4f-8844-4ae4-bc85-832fa542a1be"
	p := filepath.Join(root, "-r", sid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0o755)
	var b strings.Builder
	b.WriteString(`{"type":"user","timestamp":"2026-10-11T05:00:00Z","message":{"content":"check the screenshots"}}` + "\n")
	b.WriteString(`{"type":"assistant","timestamp":"2026-10-11T05:01:00Z","message":{"content":[{"type":"text","text":"Looking at them now."}]}}` + "\n")
	big := strings.Repeat("A", 20<<10) // a screenshot read: base64 in a tool result
	for i := range 16 {
		fmt.Fprintf(&b, `{"type":"assistant","timestamp":"2026-10-11T05:02:%02dZ","message":{"content":[{"type":"tool_use","id":"t%d","name":"Read","input":{}}]}}`+"\n", i, i)
		fmt.Fprintf(&b, `{"type":"user","timestamp":"2026-10-11T05:02:%02dZ","message":{"content":[{"type":"tool_result","tool_use_id":"t%d","content":"%s"}]}}`+"\n", i, i, big)
	}
	os.WriteFile(p, []byte(b.String()), 0o644)
	ti, _ := newTranscripts(root).tail(sid)
	if ti.claude != "Looking at them now." {
		t.Fatalf("the last words before 300 KB of tool results: %q", ti.claude)
	}
}

func TestPanePeekShowsTheScreen(t *testing.T) {
	e := newTestServer(t)
	h := e.authed(e.login(t))
	e.s.term.mu.Lock()
	e.s.term.last = Terminals{Sessions: []TSession{{Name: "dev-1", Tabs: []TTab{{ID: 1, Panes: []TPane{{ID: 2, Kind: "shell", State: "running"}}}}}}}
	e.s.term.mu.Unlock()
	e.fakeZ(map[string]string{"--session dev-1 action dump-screen": "one\ntwo\n\n\n"})
	r := e.do(t, "GET", "/api/pane/peek?session=dev-1&pane=2", "", h)
	var got struct{ Screen []string }
	json.NewDecoder(r.Body).Decode(&got)
	if r.StatusCode != 200 || strings.Join(got.Screen, "|") != "one|two" {
		t.Fatalf("peek: %d %q", r.StatusCode, got.Screen)
	}
	if r := e.do(t, "GET", "/api/pane/peek?session=dev-1&pane=9", "", h); r.StatusCode != 404 {
		t.Fatalf("no such pane: %d", r.StatusCode)
	}
}

// The peek shows an agent's latest turn as the chat does: it asks for the chat's last items at once.
func TestPaneChatTailGivesTheLatestItems(t *testing.T) {
	e := newTestServer(t)
	h := e.authed(e.login(t))
	p := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(p, []byte(`{"type":"user","timestamp":"2026-10-12T05:00:00Z","message":{"content":"ship it"}}
{"type":"assistant","timestamp":"2026-10-12T05:01:00Z","message":{"content":[{"type":"text","text":"Shipped."}],"stop_reason":"end_turn"}}
`), 0o644)
	e.s.term.mu.Lock()
	e.s.term.last = Terminals{Sessions: []TSession{{Name: "dev-1", Tabs: []TTab{{ID: 1, Panes: []TPane{{ID: 2, Kind: "claude", Agent: &TAgent{path: p}}}}}}}}
	e.s.term.mu.Unlock()
	r := e.do(t, "GET", "/api/pane/chat?session=dev-1&pane=2&tail=1", "", h)
	var got struct{ Items []ChatItem }
	json.NewDecoder(r.Body).Decode(&got)
	if r.StatusCode != 200 || len(got.Items) != 2 || got.Items[0].Text != "ship it" || got.Items[1].Text != "Shipped." {
		t.Fatalf("tail: %d %+v", r.StatusCode, got.Items)
	}
	r = e.do(t, "GET", "/api/pane/chat?session=dev-1&pane=2&tail=live", "", h)
	json.NewDecoder(r.Body).Decode(&got)
	if len(got.Items) != 2 {
		t.Fatalf("tail=live: the same, from a smaller read: %+v", got.Items)
	}
	r = e.do(t, "GET", "/api/pane/chat?session=dev-1&pane=2", "", h)
	json.NewDecoder(r.Body).Decode(&got)
	if len(got.Items) != 0 {
		t.Fatalf("no before, no tail: nothing, as before: %+v", got.Items)
	}
}

func TestTermRenameNamesATabOrASession(t *testing.T) {
	e := newTestServer(t)
	h := e.authed(e.login(t))
	for _, n := range []string{"dev-1", "box-dev"} {
		os.WriteFile(filepath.Join(e.s.sys.sockDir, n), nil, 0o600)
	}
	f := e.fakeZ(map[string]string{})
	for _, c := range []struct{ body, want string }{
		{`{"session":"dev-1","tab":3,"name":"-rf  KPI map "}`, "--session dev-1 action rename-tab --tab-id 3 -- -rf  KPI map"},
		{`{"session":"dev-1","name":"product-1"}`, "--session dev-1 action rename-session product-1"},
	} {
		f.calls = nil
		if r := e.do(t, "POST", "/api/term/rename", c.body, h); r.StatusCode != 200 || len(f.calls) != 1 || strings.Join(f.calls[0], " ") != c.want {
			t.Fatalf("%s: %d %v", c.body, r.StatusCode, f.calls)
		}
	}
	f.calls = nil
	for _, body := range []string{
		`{"session":"dev-1","name":"box-dev"}`,   // taken
		`{"session":"dev-1","name":"two words"}`, // not a session name
		`{"session":"dev-1","tab":3,"name":"  "}`,
		`{"session":"gone","tab":3,"name":"x"}`,
	} {
		if r := e.do(t, "POST", "/api/term/rename", body, h); r.StatusCode != 400 {
			t.Fatalf("%s: %d, want refused", body, r.StatusCode)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("a refused rename touches nothing: %v", f.calls)
	}
	if b, _ := os.ReadFile(e.audit); !strings.Contains(string(b), `"event":"rename"`) {
		t.Fatalf("audit: %s", b)
	}
}
