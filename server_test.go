package main

import (
	"bufio"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type testEnv struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	jwt   string
	clk   *fakeClock
	s     *server
	audit string
}

func newTestServer(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	pinFile := filepath.Join(dir, "pin")
	enc, _ := hashPIN("482913", 1000)
	os.WriteFile(pinFile, []byte(enc), 0o600)
	statusDir := filepath.Join(dir, "status")
	writeFile(t, filepath.Join(statusDir, "dev-1.json"), dev1Status)
	s := newServer(config{
		addr: "127.0.0.1:0", cfTeam: testTeam, cfAUD: testAUD, pinFile: pinFile,
		auditFile: filepath.Join(dir, "audit.jsonl"), ciDir: filepath.Join(dir, "ci"),
		statusDir: statusDir, home: dir,
	})
	v, key, clk := newTestAccess(t)
	s.access = v
	s.sys.sockDir = filepath.Join(dir, "zellij-sockets") // never the real sessions
	os.MkdirAll(s.sys.sockDir, 0o755)
	z, _ := newFakeZellij(nil)
	s.z, s.term.z = z, z
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return &testEnv{srv: ts, key: key, clk: clk, s: s, jwt: signJWT(t, key, goodHeader, goodClaims(clk)), audit: filepath.Join(dir, "audit.jsonl")}
}

func (e *testEnv) do(t *testing.T, method, path, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e *testEnv) login(t *testing.T) string {
	t.Helper()
	resp := e.do(t, "POST", "/api/login", `{"pin":"482913"}`, map[string]string{"Cf-Access-Jwt-Assertion": e.jwt})
	if resp.StatusCode != 200 {
		t.Fatalf("login status %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "cp_session" {
			return c.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}

func (e *testEnv) authed(tok string) map[string]string {
	return map[string]string{"Cf-Access-Jwt-Assertion": e.jwt, "Cookie": "cp_session=" + tok}
}

func TestNoAccessTokenIsForbiddenEverywhere(t *testing.T) {
	e := newTestServer(t)
	for _, p := range []string{"/", "/app.js", "/api/me", "/api/stream?view=res"} {
		if r := e.do(t, "GET", p, "", nil); r.StatusCode != 403 {
			t.Errorf("GET %s without Access token: %d", p, r.StatusCode)
		}
	}
	if r := e.do(t, "POST", "/api/login", `{"pin":"482913"}`, nil); r.StatusCode != 403 {
		t.Errorf("login without Access token: %d", r.StatusCode)
	}
	if r := e.do(t, "GET", "/", "", map[string]string{"Cf-Access-Jwt-Assertion": "forged.token.here"}); r.StatusCode != 403 {
		t.Errorf("forged token: %d", r.StatusCode)
	}
}

func TestPageServedWithSecurityHeaders(t *testing.T) {
	e := newTestServer(t)
	r := e.do(t, "GET", "/", "", map[string]string{"Cf-Access-Jwt-Assertion": e.jwt})
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(body), "app.js?v=") {
		t.Fatalf("status %d body %.200s", r.StatusCode, body)
	}
	csp := r.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("CSP = %q", csp)
	}
	if r.Header.Get("X-Content-Type-Options") != "nosniff" || r.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("headers = %v", r.Header)
	}
}

func TestLoginFlowAndCookieFlags(t *testing.T) {
	e := newTestServer(t)
	jwt := map[string]string{"Cf-Access-Jwt-Assertion": e.jwt}
	if r := e.do(t, "GET", "/api/me", "", jwt); r.StatusCode != 401 {
		t.Fatalf("/api/me before login: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/api/login", `{"pin":"000000"}`, jwt); r.StatusCode != 401 {
		t.Fatalf("wrong PIN: %d", r.StatusCode)
	}
	resp := e.do(t, "POST", "/api/login", `{"pin":"482913"}`, jwt)
	sc := resp.Header.Get("Set-Cookie")
	for _, flag := range []string{"cp_session=", "HttpOnly", "Secure", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(sc, flag) {
			t.Errorf("Set-Cookie %q missing %s", sc, flag)
		}
	}
	tok := ""
	for _, c := range resp.Cookies() {
		tok = c.Value
	}
	if r := e.do(t, "GET", "/api/me", "", e.authed(tok)); r.StatusCode != 200 {
		t.Fatalf("/api/me after login: %d", r.StatusCode)
	}
	// same cookie, different Access identity → rejected
	other := goodClaims(e.clk)
	other["email"] = "attacker@x.com"
	hdr := map[string]string{"Cf-Access-Jwt-Assertion": signJWT(t, e.key, goodHeader, other), "Cookie": "cp_session=" + tok}
	if r := e.do(t, "GET", "/api/me", "", hdr); r.StatusCode != 401 {
		t.Fatalf("stolen cookie with other identity: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/api/logout", "", e.authed(tok)); r.StatusCode != 200 {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if r := e.do(t, "GET", "/api/me", "", e.authed(tok)); r.StatusCode != 401 {
		t.Fatalf("/api/me after logout: %d", r.StatusCode)
	}
	b, _ := os.ReadFile(e.audit)
	if !strings.Contains(string(b), `"event":"login"`) || !strings.Contains(string(b), `"event":"pin-fail"`) {
		t.Fatalf("audit log = %s", b)
	}
}

func TestLockoutReportedToClient(t *testing.T) {
	e := newTestServer(t)
	jwt := map[string]string{"Cf-Access-Jwt-Assertion": e.jwt}
	for i := 0; i < 5; i++ {
		e.do(t, "POST", "/api/login", `{"pin":"000000"}`, jwt)
	}
	r := e.do(t, "POST", "/api/login", `{"pin":"482913"}`, jwt)
	var body struct {
		Error   string
		RetryIn int
	}
	json.NewDecoder(r.Body).Decode(&body)
	if r.StatusCode != 429 || body.RetryIn < 800 {
		t.Fatalf("status %d body %+v", r.StatusCode, body)
	}
}

func TestStreamSendsViewAndBadges(t *testing.T) {
	e := newTestServer(t)
	tok := e.login(t)
	go e.s.sampleLoop()
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/stream?view=term", nil)
	for k, v := range e.authed(tok) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<22)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				lines <- strings.TrimPrefix(sc.Text(), "data: ")
			}
		}
	}()
	select {
	case line := <-lines:
		var msg struct {
			View   string
			Badges map[string]float64
			Data   Terminals
			V      string // the build: open pages reload when it changes
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatal(err)
		}
		if msg.View != "term" || msg.Badges == nil || msg.V == "" || msg.V != e.s.version {
			t.Fatalf("msg = %+v", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5 s")
	}
}

func TestStreamRequiresSession(t *testing.T) {
	e := newTestServer(t)
	if r := e.do(t, "GET", "/api/stream?view=res", "", map[string]string{"Cf-Access-Jwt-Assertion": e.jwt}); r.StatusCode != 401 {
		t.Fatalf("stream without session: %d", r.StatusCode)
	}
}

// Owner decision 2026-09-30: Access + login PIN is enough; actions need a session, not a PIN.
func TestActionsNeedSessionNotPIN(t *testing.T) {
	e := newTestServer(t)
	tok := e.login(t)
	h := e.authed(tok)
	for _, name := range []string{"format-disk", "focus-pane"} {
		if r := e.do(t, "POST", "/api/action/"+name, `{}`, h); r.StatusCode != 404 {
			t.Fatalf("action %s: %d", name, r.StatusCode)
		}
	}
	r := e.do(t, "POST", "/api/action/kill", `{"pid":999999}`, h)
	var body struct{ Error string }
	json.NewDecoder(r.Body).Decode(&body)
	if r.StatusCode != 400 || !strings.Contains(body.Error, "no process") {
		t.Fatalf("kill missing pid without PIN: %d %+v", r.StatusCode, body)
	}
	cross := e.authed(tok)
	cross["Sec-Fetch-Site"] = "cross-site"
	if r := e.do(t, "POST", "/api/action/kill", `{"pid":999999}`, cross); r.StatusCode != 403 {
		t.Fatalf("cross-site action: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/api/action/kill", `{"pid":999999}`, map[string]string{"Cf-Access-Jwt-Assertion": e.jwt}); r.StatusCode != 401 {
		t.Fatalf("action without session: %d", r.StatusCode)
	}
}

func TestLoopbackOnly(t *testing.T) {
	if err := validateConfig(config{addr: "0.0.0.0:7777", cfTeam: "t", cfAUD: "a"}); err == nil {
		t.Error("0.0.0.0 accepted")
	}
	if err := validateConfig(config{addr: "127.0.0.1:7777"}); err == nil {
		t.Error("missing Access config accepted")
	}
	if err := validateConfig(config{addr: "127.0.0.1:7777", cfTeam: "evil.com/x", cfAUD: "a"}); err == nil {
		t.Error("bad team name accepted")
	}
	if err := validateConfig(config{addr: "127.0.0.1:7777", cfTeam: "my-team", cfAUD: "abc"}); err != nil {
		t.Error(err)
	}
	if err := validateConfig(config{addr: "127.0.0.1:7777", insecure: true}); err != nil {
		t.Error(err)
	}
}

func (e *testEnv) fakeZ(answers map[string]string) *fakeZellij {
	z, f := newFakeZellij(answers)
	e.s.z, e.s.term.z = z, z
	return f
}

func TestPaneInputValidatesAndAudits(t *testing.T) {
	e := newTestServer(t)
	h := e.authed(e.login(t))
	f := e.fakeZ(map[string]string{"--session dev-1 action list-panes": `[{"id":3,"title":"a","tab_id":0}]`})
	bad := []struct{ body, why string }{
		{`{"session":"../x","pane":3,"text":"hi"}`, "bad session"},
		{`{"session":"dev-1","pane":-1,"text":"hi"}`, "negative pane"},
		{`{"session":"dev-1","pane":3,"keys":["rm -rf"]}`, "key not allowed"},
		{`{"session":"dev-1","pane":3,"text":"` + strings.Repeat("x", 17000) + `"}`, "text too long"},
		{`{"session":"dev-1","pane":3}`, "nothing to send"},
	}
	for _, b := range bad {
		if r := e.do(t, "POST", "/api/pane/input", b.body, h); r.StatusCode != 400 {
			t.Errorf("%s: %d", b.why, r.StatusCode)
		}
	}
	if r := e.do(t, "POST", "/api/pane/input", `{"session":"dev-1","pane":9,"text":"hi"}`, h); r.StatusCode != 404 {
		t.Errorf("missing pane: %d", r.StatusCode)
	}
	if len(f.calls) != 1 || f.calls[0][3] != "list-panes" {
		t.Fatalf("zellij called for invalid input: %v", f.calls)
	}
	f.calls = nil
	if r := e.do(t, "POST", "/api/pane/input", `{"session":"dev-1","pane":3,"text":"run the tests","enter":true,"keys":["Esc"]}`, h); r.StatusCode != 200 {
		t.Fatalf("good input: %d", r.StatusCode)
	}
	want := [][]string{
		{"--session", "dev-1", "action", "list-panes", "--json", "--all"},
		{"--session", "dev-1", "action", "send-keys", "--pane-id", "terminal_3", "Esc"},
		{"--session", "dev-1", "action", "paste", "--pane-id", "terminal_3", "--", "run the tests"},
		{"--session", "dev-1", "action", "send-keys", "--pane-id", "terminal_3", "Enter"},
	}
	if len(f.calls) != len(want) {
		t.Fatalf("calls = %v", f.calls)
	}
	for i := range want {
		if strings.Join(f.calls[i], " ") != strings.Join(want[i], " ") {
			t.Fatalf("call %d = %v, want %v", i, f.calls[i], want[i])
		}
	}
	if b, _ := os.ReadFile(e.audit); !strings.Contains(string(b), `"event":"input"`) || !strings.Contains(string(b), "run the tests") {
		t.Fatalf("audit: %s", b)
	}
}

func TestTermNew(t *testing.T) {
	e := newTestServer(t)
	h := e.authed(e.login(t))
	os.WriteFile(filepath.Join(e.s.sys.sockDir, "dev-1"), nil, 0o600)
	cwd := t.TempDir()
	f := e.fakeZ(map[string]string{
		"--session dev-1 action new-pane":   "terminal_7\n",
		"--session dev-1 action list-panes": `[{"id":7,"title":"x","tab_id":2}]`,
	})
	bad := []struct{ body, why string }{
		{`{"what":"window"}`, "bad kind"},
		{`{"what":"session","name":"a b"}`, "bad session name"},
		{`{"what":"session","name":"dev-1"}`, "session exists"},
		{`{"what":"pane","session":"nope","tab":0}`, "session not live"},
		{`{"what":"pane","session":"dev-1","tab":0,"cwd":"/definitely/not/here"}`, "bad cwd"},
		{`{"what":"pane","session":"dev-1","tab":0,"name":"a\u0007b"}`, "control char in name"},
		{`{"what":"pane","session":"dev-1","tab":0,"dir":"up"}`, "bad direction"},
		{`{"what":"pane","session":"dev-1","tab":0,"run":"rm"}`, "bad run"},
	}
	for _, b := range bad {
		if r := e.do(t, "POST", "/api/term/new", b.body, h); r.StatusCode != 400 {
			t.Errorf("%s: %d", b.why, r.StatusCode)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("zellij called for invalid requests: %v", f.calls)
	}
	r := e.do(t, "POST", "/api/term/new", `{"what":"pane","session":"dev-1","tab":2,"name":"logs","cwd":"`+cwd+`","dir":"down","run":"claude"}`, h)
	var body struct {
		Pane int
	}
	json.NewDecoder(r.Body).Decode(&body)
	if r.StatusCode != 200 || body.Pane != 7 {
		t.Fatalf("new pane: %d %+v", r.StatusCode, body)
	}
	got := []string{}
	for _, c := range f.calls {
		got = append(got, strings.Join(c, " "))
	}
	want := []string{
		"--session dev-1 action new-pane --tab-id 2 --no-focus --cwd " + cwd + " --name logs --direction down",
		"--session dev-1 action write-chars --pane-id terminal_7 -- claude",
		"--session dev-1 action send-keys --pane-id terminal_7 Enter",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPaneChatNeedsKnownAgent(t *testing.T) {
	e := newTestServer(t)
	h := e.authed(e.login(t))
	if r := e.do(t, "GET", "/api/pane/chat?session=dev-1&pane=3&before=100", "", h); r.StatusCode != 404 {
		t.Fatalf("chat for unknown pane: %d", r.StatusCode)
	}
	if r := e.do(t, "GET", "/api/pane/stream?session=dev-1&pane=x&mode=screen", "", h); r.StatusCode != 400 {
		t.Fatalf("bad pane id: %d", r.StatusCode)
	}
}

func TestPaneInputChooseReadsTheScreenFirst(t *testing.T) {
	e := newTestServer(t)
	h := e.authed(e.login(t))
	f := e.fakeZ(map[string]string{
		"--session dev-1 action list-panes":  `[{"id":5,"title":"a","tab_id":0}]`,
		"--session dev-1 action dump-screen": trustScreen,
	})
	sig := parsePrompt(trustScreen).Sig
	if r := e.do(t, "POST", "/api/pane/input", `{"session":"dev-1","pane":5,"choose":"Yes, I trust this folder"}`, h); r.StatusCode != 400 {
		t.Fatalf("choose without the question's sig: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/api/pane/input", `{"session":"dev-1","pane":5,"choose":"Yes, I trust this folder","sig":"0000000000000000"}`, h); r.StatusCode != 409 {
		t.Fatalf("answer to a different question: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/api/pane/input", `{"session":"dev-1","pane":5,"choose":"Yes, I trust this folder","sig":"`+sig+`"}`, h); r.StatusCode != 200 {
		t.Fatalf("choose: %d", r.StatusCode)
	}
	last := f.calls[len(f.calls)-1]
	if strings.Join(last, " ") != "--session dev-1 action send-keys --pane-id terminal_5 Down Enter" {
		t.Fatalf("calls = %v", f.calls)
	}
	if r := e.do(t, "POST", "/api/pane/input", `{"session":"dev-1","pane":5,"choose":"Something else","sig":"`+sig+`"}`, h); r.StatusCode != 409 {
		t.Fatalf("stale choice: %d", r.StatusCode)
	}
}

func TestLimitEndpoint(t *testing.T) {
	e := newTestServer(t)
	h := e.authed(e.login(t))
	if r := e.do(t, "POST", "/api/limit", `{"key":"no-such-group-xyz","label":"x","nice":7}`, h); r.StatusCode != 400 {
		t.Fatalf("bad nice: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/api/limit", `{"key":"no-such-group-xyz","label":"Test group","cores":[0],"ioIdle":true}`, h); r.StatusCode != 200 {
		t.Fatalf("set: %d", r.StatusCode)
	}
	if l := e.s.limits.list(); len(l) != 1 || l[0].Label != "Test group" {
		t.Fatalf("rules = %+v", l)
	}
	if r := e.do(t, "POST", "/api/limit", `{"key":"no-such-group-xyz","remove":true}`, h); r.StatusCode != 200 {
		t.Fatalf("remove: %d", r.StatusCode)
	}
	if b, _ := os.ReadFile(e.audit); !strings.Contains(string(b), `"event":"limit"`) || !strings.Contains(string(b), "Test group: core 0, disk idle") {
		t.Fatalf("audit: %s", b)
	}
}

// A stream whose login session ends must take its zellij subscriber down with it.
func TestScreenStreamStopsSubscriberWhenSessionEnds(t *testing.T) {
	e := newTestServer(t)
	tok := e.login(t)
	dir := t.TempDir()
	fake := filepath.Join(dir, "zellij")
	os.WriteFile(fake, []byte("#!/bin/sh\necho $$ > "+dir+"/pid\nexec sleep 60\n"), 0o755)
	oldBin, oldPing := zellijBin, pingEvery
	zellijBin, pingEvery = fake, 50*time.Millisecond
	defer func() { zellijBin, pingEvery = oldBin, oldPing }()
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/pane/stream?session=dev-1&pane=1&mode=screen", nil)
	for k, v := range e.authed(tok) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var pid int
	for i := 0; i < 100 && pid == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		b, _ := os.ReadFile(dir + "/pid")
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if pid == 0 {
		t.Fatal("subscriber never started")
	}
	e.s.auth.endSession(tok)
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			io.ReadAll(resp.Body) // EOF once the handler has returned: only then restore the globals it reads
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatal("zellij subscribe still running after the session ended")
}

func TestPaneWriteSendsRawBytes(t *testing.T) {
	e := newTestServer(t)
	h := e.authed(e.login(t))
	f := e.fakeZ(map[string]string{"--session dev-1 action list-panes": `[{"id":3,"title":"a","tab_id":0}]`})
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, bad := range []string{
		`{"session":"dev-1","pane":3,"data":"%%%"}`,
		`{"session":"dev-1","pane":3,"data":""}`,
		`{"session":"dev-1","pane":3,"data":"` + b64(strings.Repeat("x", 9000)) + `"}`,
		`{"session":"../x","pane":3,"data":"` + b64("a") + `"}`,
	} {
		if r := e.do(t, "POST", "/api/pane/write", bad, h); r.StatusCode != 400 {
			t.Errorf("%.60s: %d", bad, r.StatusCode)
		}
	}
	if r := e.do(t, "POST", "/api/pane/write", `{"session":"dev-1","pane":9,"data":"`+b64("a")+`"}`, h); r.StatusCode != 404 {
		t.Fatalf("missing pane: %d", r.StatusCode)
	}
	for i := 0; i < 2; i++ {
		if r := e.do(t, "POST", "/api/pane/write", `{"session":"dev-1","pane":3,"data":"`+b64("ls\r\x1b[A")+`"}`, h); r.StatusCode != 200 {
			t.Fatalf("write: %d", r.StatusCode)
		}
	}
	if got := strings.Join(f.calls[len(f.calls)-1], " "); got != "--session dev-1 action write --pane-id terminal_3 108 115 13 27 91 65" {
		t.Fatalf("argv = %s", got)
	}
	b, _ := os.ReadFile(e.audit)
	if n := strings.Count(string(b), `"event":"terminal"`); n != 1 {
		t.Fatalf("want one audit entry per pane per minute of typing, got %d", n)
	}
}

func TestPaneUpload(t *testing.T) {
	e := newTestServer(t)
	h := e.authed(e.login(t))
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR" + strings.Repeat("\x00", 40)
	r := e.do(t, "POST", "/api/pane/upload", png, h)
	var body struct{ Path string }
	json.NewDecoder(r.Body).Decode(&body)
	if r.StatusCode != 200 || !strings.HasSuffix(body.Path, ".png") || filepath.Dir(body.Path) != e.s.uploadDir {
		t.Fatalf("upload: %d %+v", r.StatusCode, body)
	}
	fi, err := os.Stat(body.Path)
	if err != nil || fi.Mode().Perm() != 0o600 || fi.Size() != int64(len(png)) {
		t.Fatalf("saved file: %v %v", fi, err)
	}
	r = e.do(t, "POST", "/api/pane/upload?name=..%2F..%2Fmy%20notes!.txt", "hello notes\n", h)
	json.NewDecoder(r.Body).Decode(&body)
	if r.StatusCode != 200 || !strings.HasSuffix(body.Path, "-my_notes_.txt") || filepath.Dir(body.Path) != e.s.uploadDir {
		t.Fatalf("any file, sanitized name: %d %+v", r.StatusCode, body)
	}
	if r := e.do(t, "POST", "/api/pane/upload", png+strings.Repeat("x", 26<<20), h); r.StatusCode != 413 {
		t.Fatalf("too big: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/api/pane/upload", png, map[string]string{"Cf-Access-Jwt-Assertion": e.jwt}); r.StatusCode != 401 {
		t.Fatalf("no session: %d", r.StatusCode)
	}
}

func TestUploadServedBackForTheChat(t *testing.T) {
	e := newTestServer(t)
	h := e.authed(e.login(t))
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR" + strings.Repeat("\x00", 40)
	r := e.do(t, "POST", "/api/pane/upload", png, h)
	var body struct{ Path string }
	json.NewDecoder(r.Body).Decode(&body)
	name := filepath.Base(body.Path)
	g := e.do(t, "GET", "/api/upload/"+name, "", h)
	b, _ := io.ReadAll(g.Body)
	if g.StatusCode != 200 || string(b) != png || g.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("serve: %d %q %q", g.StatusCode, g.Header.Get("Content-Type"), b[:8])
	}
	r = e.do(t, "POST", "/api/pane/upload?name=page.html", "<script>alert(1)</script>", h)
	json.NewDecoder(r.Body).Decode(&body)
	g = e.do(t, "GET", "/api/upload/"+filepath.Base(body.Path), "", h)
	if g.StatusCode != 200 || !strings.HasPrefix(g.Header.Get("Content-Disposition"), "attachment") || strings.Contains(g.Header.Get("Content-Type"), "html") {
		t.Fatalf("non-images must download, never render: %d %q %q", g.StatusCode, g.Header.Get("Content-Type"), g.Header.Get("Content-Disposition"))
	}
	for _, bad := range []string{"..%2Fpin", "pin", "20260930-060302-cca1e85820f38a34.sh", strings.TrimSuffix(name, ".png") + ".gif"} {
		if r := e.do(t, "GET", "/api/upload/"+bad, "", h); r.StatusCode != 404 {
			t.Errorf("%s: %d", bad, r.StatusCode)
		}
	}
	if r := e.do(t, "GET", "/api/upload/"+name, "", map[string]string{"Cf-Access-Jwt-Assertion": e.jwt}); r.StatusCode != 401 {
		t.Fatalf("no session: %d", r.StatusCode)
	}
}

func TestTermClose(t *testing.T) {
	e := newTestServer(t)
	h := e.authed(e.login(t))
	os.WriteFile(filepath.Join(e.s.sys.sockDir, "dev-1"), nil, 0o600)
	f := e.fakeZ(nil)
	for _, bad := range []string{
		`{"what":"window","session":"dev-1"}`,
		`{"what":"pane","session":"nope","pane":1}`,
		`{"what":"tab","session":"dev-1","tab":-1}`,
		`{"what":"process","session":"dev-1","pane":4,"pid":1}`,
	} {
		if r := e.do(t, "POST", "/api/term/close", bad, h); r.StatusCode != 400 {
			t.Errorf("%s: %d", bad, r.StatusCode)
		}
	}
	for _, ok := range []string{`{"what":"pane","session":"dev-1","pane":3}`, `{"what":"tab","session":"dev-1","tab":2}`, `{"what":"session","session":"dev-1"}`} {
		if r := e.do(t, "POST", "/api/term/close", ok, h); r.StatusCode != 200 {
			t.Fatalf("%s: %d", ok, r.StatusCode)
		}
	}
	got := []string{}
	for _, c := range f.calls {
		got = append(got, strings.Join(c, " "))
	}
	if strings.Join(got, "|") != "--session dev-1 action close-pane --pane-id terminal_3|--session dev-1 action close-tab-by-id 2|delete-session --force dev-1" {
		t.Fatalf("calls = %v", got)
	}
	// a process in pane 4 of this session: killed; claimed for another pane: refused
	cmd := exec.Command("sleep", "60")
	cmd.Env = append(os.Environ(), "ZELLIJ_PANE_ID=4", "ZELLIJ_SESSION_NAME=dev-1")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	defer cmd.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	pid := strconv.Itoa(cmd.Process.Pid)
	if r := e.do(t, "POST", "/api/term/close", `{"what":"process","session":"dev-1","pane":5,"pid":`+pid+`}`, h); r.StatusCode != 400 {
		t.Fatalf("process from another pane: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/api/term/close", `{"what":"process","session":"dev-1","pane":4,"pid":`+pid+`}`, h); r.StatusCode != 200 {
		t.Fatalf("kill pane process: %d", r.StatusCode)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("process still running")
	}
	if b, _ := os.ReadFile(e.audit); strings.Count(string(b), `"event":"close"`) != 4 {
		t.Fatalf("audit: %s", b)
	}
}

func TestClientErrorsAreLoggedWithTheBrowser(t *testing.T) {
	e := newTestServer(t)
	body := `{"kind":"error","msg":"ReferenceError: D is not defined @ app.js:812","stack":"viewTerm@app.js:812","view":"term"}`
	if r := e.do(t, "POST", "/api/client-error", body, map[string]string{"Cf-Access-Jwt-Assertion": e.jwt}); r.StatusCode != 401 {
		t.Fatalf("without a session: %d", r.StatusCode)
	}
	h := e.authed(e.login(t))
	h["User-Agent"] = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X)"
	if r := e.do(t, "POST", "/api/client-error", body, h); r.StatusCode != 204 {
		t.Fatalf("status %d", r.StatusCode)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(e.audit), "client-errors.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Msg, Stack, UA, View string }
	if json.Unmarshal(b, &got) != nil || got.Msg != "ReferenceError: D is not defined @ app.js:812" || got.View != "term" || !strings.Contains(got.UA, "iPhone OS 17_5") || got.Stack == "" {
		t.Fatalf("logged %s", b)
	}
}

// Local HTTP clients must retain the session after successfully entering a PIN.
func TestLocalHTTPLoginPersistsAndLogoutClearsSession(t *testing.T) {
	e := newTestServer(t)
	e.s.cfg.insecure = true
	e.s.access = nil
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Post(e.srv.URL+"/api/login", "application/json", strings.NewReader(`{"pin":"482913"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("local HTTP cookie flags incorrect")
	}
	resp, err = client.Get(e.srv.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("session lost after login: %d", resp.StatusCode)
	}
	resp, err = client.Post(e.srv.URL+"/api/logout", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if len(jar.Cookies(resp.Request.URL)) != 0 {
		t.Fatal("logout did not clear cookie")
	}
	resp, err = client.Get(e.srv.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("session survived logout: %d", resp.StatusCode)
	}
}

func TestEmbeddedLocalCookieAndCrossSiteProtection(t *testing.T) {
	e := newTestServer(t)
	e.s.cfg.insecure = true
	e.s.cfg.embedLocal = true
	e.s.access = nil
	resp := e.do(t, "POST", "/api/login", `{"pin":"482913"}`, nil)
	cookies := resp.Cookies()
	if resp.StatusCode != 200 || len(cookies) != 1 {
		t.Fatal("embedded login failed")
	}
	c := cookies[0]
	if !c.Secure || !c.HttpOnly || !c.Partitioned || c.SameSite != http.SameSiteNoneMode {
		t.Fatal("embedded cookie must be Secure, HttpOnly, SameSite=None, Partitioned")
	}
	token := c.Value
	if r := e.do(t, "GET", "/api/me", "", map[string]string{"Cookie": "cp_session=stale-browser-cookie; cp_session_embed=" + token}); r.StatusCode != 200 {
		t.Fatal("old browser cookie shadowed embedded login")
	}
	// PIN is still required, and cross-site writes remain blocked.
	if r := e.do(t, "GET", "/api/me", "", nil); r.StatusCode != 401 {
		t.Fatal("missing session accepted")
	}
	if r := e.do(t, "POST", "/api/login", `{"pin":"482913"}`, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://untrusted.example"}); r.StatusCode != 403 {
		t.Fatalf("cross-site login: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/api/logout", `{}`, map[string]string{"Cookie": "cp_session_embed=" + token, "Sec-Fetch-Site": "cross-site"}); r.StatusCode != 403 {
		t.Fatalf("cross-site write: %d", r.StatusCode)
	}
	resp = e.do(t, "POST", "/api/logout", `{}`, map[string]string{"Cookie": "cp_session_embed=" + token})
	cookies = resp.Cookies()
	if resp.StatusCode != 200 || len(cookies) != 1 || !cookies[0].Partitioned || !cookies[0].Secure || cookies[0].MaxAge != -1 {
		t.Fatal("logout must clear the same partitioned cookie")
	}
	if r := e.do(t, "GET", "/api/me", "", map[string]string{"Cookie": "cp_session_embed=" + token}); r.StatusCode != 401 {
		t.Fatal("session survived logout")
	}
	if err := validateConfig(config{addr: "127.0.0.1:7777", cfTeam: "team", cfAUD: "aud", embedLocal: true}); err == nil {
		t.Fatal("embedded local mode must be explicitly local")
	}
}
