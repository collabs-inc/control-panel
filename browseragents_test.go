package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The wrapper end to end, in a home of its own: an agent's cube-browser command runs the Cube image's tool with a
// Chromium that registers itself, the panel attaches to it and sees its tab, the command lands in the timeline,
// and a hold makes the next command wait.
func TestAgentWatch(t *testing.T) {
	if _, err := os.Stat("/opt/cube/browser/cube-browser.mjs"); err != nil || testing.Short() {
		t.Skip("no cube-browser on this box")
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/items" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"items":[{"id":7,"name":"seven"}],"access_token":"tok-abc"}`)
			return
		}
		fmt.Fprint(w, `<!doctype html><title>Agent page</title><button id="go" onclick="document.title='clicked'; fetch('/api/items?page=2', {method: 'POST', headers: {'Content-Type': 'application/json', Authorization: 'Bearer s3cret'}, body: JSON.stringify({q: 'x', password: 'hunter2'})})">Go</button><script>console.error('agent boom'); fetch('/api/items?first=1')</script>`)
	}))
	defer page.Close()
	home := t.TempDir()
	repo, _ := os.Getwd() // cube-browser --session wants a git checkout to work in
	b := newBrowser(nil, filepath.Join(home, "state"), home)
	if err := b.installAgentWatch(); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(home, ".local/bin/cube-browser")
	runAs := func(session string, args ...string) (string, error) {
		c := exec.Command(shim, append([]string{"--session", session}, args...)...)
		c.Dir = repo
		c.Env = append(os.Environ(), "HOME="+home, "ZELLIJ_SESSION_NAME=tsess", "ZELLIJ_PANE_ID=7")
		out, err := c.CombinedOutput()
		return string(out), err
	}
	run := func(args ...string) (string, error) { return runAs("cpwatch", args...) }
	defer run("close")
	// the session's id, as cube-browser makes it for --session: the checkout and the name
	top, _ := exec.Command("git", "-C", repo, "rev-parse", "--show-toplevel").Output()
	sum := sha256.Sum256([]byte(strings.TrimSpace(string(top)) + "\x00cpwatch"))
	key := "manual-" + hex.EncodeToString(sum[:])[:32]
	if out, err := run("open", page.URL); err != nil {
		t.Fatalf("open: %v\n%s", err, out)
	}
	regs, _ := filepath.Glob(filepath.Join(home, ".local/state/control-panel/agent-browsers/[0-9]*.json"))
	if len(regs) == 0 {
		t.Fatal("the agent's Chromium didn't register")
	}
	v := &bview{wake: make(chan struct{}, 1), thumbs: map[string]bool{}, thumbAt: map[string]time.Time{}, thumbQ: map[string]*bframe{}}
	b.viewers[v] = true // as if a page had the Browser open
	var tab *btab
	for i := 0; i < 50 && tab == nil; i++ {
		b.agentsTick()
		b.mu.Lock()
		for _, src := range b.agents {
			for _, x := range src.sess {
				if strings.HasPrefix(x.URL, page.URL) {
					tab = x
				}
			}
		}
		b.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	if tab == nil {
		t.Fatal("the panel didn't attach to the agent's browser")
	}
	if tab.src.agent.key != key || tab.src.agent.zs != "tsess" || tab.src.agent.zp != 7 {
		t.Fatalf("agent %+v", tab.src.agent)
	}
	// watched: its frames stream like the owner's, at the size Playwright gave it
	b.mu.Lock()
	v.tab = tab.id
	b.mu.Unlock()
	b.recast()
	var f *bframe
	for i := 0; i < 50 && f == nil; i++ {
		time.Sleep(100 * time.Millisecond)
		b.mu.Lock()
		f = tab.frame
		b.mu.Unlock()
	}
	if f == nil || len(f.img) < 100 || !strings.Contains(string(f.meta), `"dw":1280`) {
		t.Fatalf("no frame from the agent's tab (%v)", f != nil)
	}
	if out, err := run("click", "#go"); err != nil {
		t.Fatalf("click: %v\n%s", err, out)
	}
	b.agentsTick()
	b.mu.Lock()
	l := b.agentLog[key]
	var verbs []string
	for _, st := range l.steps {
		verbs = append(verbs, st.Argv[2])
		if st.RC == nil || *st.RC != 0 {
			t.Errorf("step %v: rc %v", st.Argv, st.RC)
		}
	}
	b.mu.Unlock()
	if strings.Join(verbs, ",") != "open,click" {
		t.Fatalf("timeline %v", verbs)
	}
	for i := 0; i < 30 && tab.Title != "clicked"; i++ {
		b.history(tab)
		time.Sleep(100 * time.Millisecond)
	}
	if tab.Title != "clicked" {
		t.Fatalf("the click didn't reach the page: title %q", tab.Title)
	}
	// Take over: the next command waits until the hold is gone
	b.hold(key, true)
	done := make(chan time.Duration, 1)
	go func() {
		t0 := time.Now()
		run("snapshot")
		done <- time.Since(t0)
	}()
	time.Sleep(1500 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("a command ran while the owner held the browser")
	default:
	}
	b.hold(key, false)
	if d := <-done; d < 1500*time.Millisecond {
		t.Fatalf("waited %v", d)
	}
	// its trail: the commands, the page's load and console error, a picture after each, the motion
	tr := tab.src.agent.trail
	if tr == nil || !strings.HasPrefix(tr.id, key+"/") {
		t.Fatalf("trail %+v", tr)
	}
	ev, _ := os.ReadFile(filepath.Join(tr.dir, "events.jsonl"))
	for _, k := range []string{`"k":"step"`, `"k":"done"`, `"k":"load"`, `"k":"shot"`, `"k":"con"`, "agent boom", page.URL} {
		if !strings.Contains(string(ev), k) {
			t.Errorf("the trail has no %s:\n%s", k, ev)
		}
	}
	if fi, err := os.Stat(filepath.Join(tr.dir, "shots", "1.jpg")); err != nil || fi.Size() < 1000 {
		t.Errorf("no picture in the trail: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(tr.dir, "motion.bin")); err != nil || fi.Size() < 1000 {
		t.Errorf("no motion in the trail: %v", err)
	}
	// the page's API call: what it sent and got, credentials redacted; and the wrapper finds it
	var call []byte
	for i := 0; i < 50 && len(call) == 0; i++ {
		call, _ = os.ReadFile(filepath.Join(tr.dir, "calls", "1.json"))
		time.Sleep(100 * time.Millisecond)
	}
	for _, k := range []string{`"POST"`, `/api/items?page=2`, `\"q\":\"x\"`, `\"items\":[{\"id\":7`, `"status": 200`} {
		if !strings.Contains(string(call), k) {
			t.Errorf("the call has no %s:\n%s", k, call)
		}
	}
	for _, k := range []string{"hunter2", "s3cret", "tok-abc"} {
		if strings.Contains(string(call), k) {
			t.Errorf("the call kept a secret (%s):\n%s", k, call)
		}
	}
	if out, _ := run("api-calls", "items"); !strings.Contains(out, "POST "+page.URL+"/api/items?page=2 -> 200") || !strings.Contains(out, filepath.Join(tr.dir, "calls", "1.json")) {
		t.Errorf("api-calls:\n%s", out)
	}
	// an agent reads it: history, and a line when another session opens a page one already saw
	if out, _ := run("history", "127.0.0.1"); !strings.Contains(out, page.URL) || !strings.Contains(out, "screenshot: ") || !strings.Contains(out, "agent boom") {
		t.Errorf("history:\n%s", out)
	}
	// with a panel recording, open starts the browser blank and goes to the page once the panel is there: the
	// page's first API call is kept too; and it prints what open prints
	stop := make(chan bool)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				b.agentsTick()
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	defer runAs("cpwatch2", "close")
	out, err := runAs("cpwatch2", "open", page.URL)
	close(stop)
	if err != nil || !strings.Contains(out, "loaded this page 1 time") || !strings.Contains(out, `"session": "cube-manual-`) || !strings.Contains(out, `"result": {`) {
		t.Errorf("open in another session (%v):\n%s", err, out)
	}
	var first bool
	for i := 0; i < 30 && !first; i++ {
		raw, _ := os.ReadFile(filepath.Join(home, ".local/state/control-panel/agent-browsers/trails/calls.jsonl"))
		for _, l := range strings.Split(string(raw), "\n") {
			first = first || strings.Contains(l, "first=1") && !strings.Contains(l, `"trail":"`+key+`/`)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !first {
		t.Error("the first load's API call wasn't kept")
	}
}

// the trails keep a week, never a pinned one, and the visits of the deleted ones go with them
func TestTrailsSweep(t *testing.T) {
	b := newBrowser(nil, t.TempDir(), t.TempDir())
	root := b.trailsDir()
	mk := func(id string, age time.Duration, pinned bool) string {
		d := filepath.Join(root, id)
		os.MkdirAll(filepath.Join(d, "shots"), 0o700)
		raw, _ := json.Marshal(btrailMeta{ID: id, Pinned: pinned})
		os.WriteFile(filepath.Join(d, "meta.json"), raw, 0o600)
		os.WriteFile(filepath.Join(d, "shots", "1.jpg"), []byte("x"), 0o600)
		old := time.Now().Add(-age)
		filepath.Walk(d, func(p string, _ os.FileInfo, _ error) error { return os.Chtimes(p, old, old) })
		return d
	}
	fresh := mk("s1/20261014-100000-1", time.Hour, false)
	stale := mk("s2/20261001-100000-2", 9*24*time.Hour, false)
	pinned := mk("s3/20261001-100000-3", 9*24*time.Hour, true)
	vis := fmt.Sprintf("{\"t\":%d,\"trail\":\"s1/20261014-100000-1\",\"url\":\"a\"}\n{\"t\":%d,\"trail\":\"s2/20261001-100000-2\",\"url\":\"b\"}\n", time.Now().UnixMilli(), time.Now().UnixMilli())
	os.WriteFile(filepath.Join(root, "visits.jsonl"), []byte(vis), 0o600)
	b.trailsSweep()
	for d, want := range map[string]bool{fresh: true, stale: false, pinned: true} {
		if _, err := os.Stat(d); (err == nil) != want {
			t.Errorf("%s kept %v, want %v", d, err == nil, want)
		}
	}
	if _, err := os.Stat(filepath.Dir(stale)); err == nil {
		t.Error("the deleted trail's session folder stayed")
	}
	if raw, _ := os.ReadFile(filepath.Join(root, "visits.jsonl")); strings.Contains(string(raw), "s2/") || !strings.Contains(string(raw), "s1/") {
		t.Errorf("visits: %s", raw)
	}
}

// a trail holds its own run's commands, once: not an earlier run's from the same session's log, and not twice when
// a restarted panel reads the log again
func TestTrailStepRun(t *testing.T) {
	b := newBrowser(nil, t.TempDir(), t.TempDir())
	start := time.Now().Add(-time.Minute)
	a := &bagent{id: "42", key: "manual-x", started: start}
	rc := 0
	old := &bstep{ID: "old", T: start.Add(-18 * time.Hour).UnixMilli(), MS: 900, RC: &rc, Argv: []string{"open", "a"}}
	opener := &bstep{ID: "open", T: start.Add(-3 * time.Second).UnixMilli(), Argv: []string{"open", "b"}} // started its Chromium
	tr := b.openTrail(a)
	b.trailStep(tr, old)
	b.trailStep(tr, opener) // still running: it ended before nothing yet
	opener.RC, opener.MS = &rc, 4000
	b.trailStep(tr, opener)
	b.trailStep(tr, opener)
	tr.close()
	tr = b.openTrail(a) // the panel restarted
	b.trailStep(tr, old)
	b.trailStep(tr, opener)
	tr.close()
	raw, _ := os.ReadFile(filepath.Join(tr.dir, "events.jsonl"))
	var got []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e struct{ K, ID string }
		json.Unmarshal([]byte(l), &e)
		if e.ID != "" {
			got = append(got, e.K+":"+e.ID)
		}
	}
	if strings.Join(got, ",") != "step:open,done:open" {
		t.Fatalf("events %v", got)
	}
}

// one panel on the machine records: a second one beside it (a test panel) only watches, until the first one ends
func TestOneRecorder(t *testing.T) {
	home := t.TempDir()
	live, test := newBrowser(nil, t.TempDir(), home), newBrowser(nil, t.TempDir(), home)
	if !live.recorder() || test.recorder() {
		t.Fatal("both record")
	}
	live.recLock.Close() // the live panel ended
	if !test.recorder() {
		t.Fatal("nobody records")
	}
}

func TestBrRedactBody(t *testing.T) {
	for in, want := range map[string]string{
		`{"user":"a","userPassword":"p","nested":{"apiKey":"k","n":1}}`: `{"nested":{"apiKey":"[redacted]","n":1},"user":"a","userPassword":"[redacted]"}`,
		`{"items":[1,2]}`:            `{"items":[1,2]}`, // nothing secret: as it came
		`user=a&password=p&pin=1234`: `password=%5Bredacted%5D&pin=%5Bredacted%5D&user=a`,
		`q=shoes&page=2`:             `q=shoes&page=2`,
		`plain text with password=x`: `plain text with password=x`,
	} {
		if got := brRedactBody(in, ""); got != want {
			t.Errorf("brRedactBody(%s) = %s, want %s", in, got, want)
		}
	}
	if h := brRedactHeaders(map[string]any{"Authorization": "Bearer x", "Accept": "*/*"}); h["Authorization"] != brRedacted || h["Accept"] != "*/*" {
		t.Errorf("headers %v", h)
	}
}
