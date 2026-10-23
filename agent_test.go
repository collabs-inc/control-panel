package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentAPI(t *testing.T) {
	e := newTestServer(t)
	go e.s.sampleLoop()
	f := e.fakeZ(map[string]string{"--session dev-1 action list-panes": `[{"id":3,"title":"a","tab_id":0}]`})
	ts := httptest.NewServer(e.s.agentHandler("secret-token"))
	t.Cleanup(ts.Close)
	call := func(method, path, body, tok string) (*http.Response, string) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return resp, b.String()
	}

	for _, tok := range []string{"", "wrong"} {
		if r, _ := call("GET", "/agent/snapshot?view=term", "", tok); r.StatusCode != 401 {
			t.Fatalf("token %q: %d", tok, r.StatusCode)
		}
	}
	r, body := call("GET", "/agent/snapshot?view=term", "", "secret-token")
	var tm Terminals
	if r.StatusCode != 200 || json.Unmarshal([]byte(body), &tm) != nil {
		t.Fatalf("snapshot: %d %s", r.StatusCode, body)
	}
	if r, _ := call("GET", "/agent/snapshot?view=nope", "", "secret-token"); r.StatusCode != 400 {
		t.Fatalf("bad view: %d", r.StatusCode)
	}
	f.calls = nil
	if r, body := call("POST", "/agent/pane/input", `{"session":"dev-1","pane":3,"text":"hi","enter":true}`, "secret-token"); r.StatusCode != 200 {
		t.Fatalf("input: %d %s", r.StatusCode, body)
	}
	if b, _ := os.ReadFile(e.audit); !strings.Contains(string(b), `"ip":"agent"`) {
		t.Fatalf("input not audited as agent: %s", b)
	}
}

func TestAgentAddrAndToken(t *testing.T) {
	if validAgentAddr("0.0.0.0:7778") == nil || validAgentAddr("127.0.0.1:7778") != nil {
		t.Fatal("agent address must be loopback")
	}
	p := filepath.Join(t.TempDir(), "sub", "agent-token")
	a, err := agentToken(p)
	if err != nil || len(a) != 64 {
		t.Fatalf("token %q %v", a, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if b, _ := agentToken(p); b != a {
		t.Fatal("token not reused")
	}
}
