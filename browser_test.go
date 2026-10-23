package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// brClient reads a viewer's websocket: JSON messages by type, binary ones by kind.
type brClient struct {
	t  *testing.T
	ws *wsConn
}

func (c *brClient) send(m map[string]any) {
	raw, _ := json.Marshal(m)
	if err := c.ws.write(wsText, raw); err != nil {
		c.t.Fatal(err)
	}
}

// until reads messages until ok says one is the one, or the time runs out.
func (c *brClient) until(what string, d time.Duration, ok func(op int, m map[string]any, bin []byte) bool) {
	c.t.Helper()
	deadline := time.Now().Add(d)
	c.ws.c.SetReadDeadline(deadline)
	defer c.ws.c.SetReadDeadline(time.Time{})
	for {
		op, p, err := c.ws.read()
		if err != nil {
			c.t.Fatalf("waiting for %s: %v", what, err)
		}
		var m map[string]any
		if op == wsText {
			json.Unmarshal(p, &m)
		} else if len(p) > 3 {
			n := int(binary.BigEndian.Uint16(p[1:3]))
			json.Unmarshal(p[3:3+n], &m)
			m["kind"] = float64(p[0])
			p = p[3+n:]
		}
		if os.Getenv("BR_DEBUG") != "" {
			c.t.Logf("got op %d %v", op, m)
		}
		done := ok(op, m, p)
		if op == wsBinary && m["kind"] == 1.0 { // drawn, as the page says it after each frame
			c.send(map[string]any{"t": "ack"})
		}
		if done {
			return
		}
	}
}

func TestBrowserLive(t *testing.T) {
	home, _ := os.UserHomeDir()
	if chromiumPath(home) == "" || testing.Short() {
		t.Skip("no Chromium")
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `<!doctype html><title>Hello</title><body style="margin:0">
<button id="b" class="save primary" style="position:fixed;left:0;top:0;width:300px;height:200px" onclick="console.error('boom', 42); fetch('/nope')">Save</button>
<input id="i" style="position:fixed;left:0;top:300px;width:300px">
<div id="card" class="funnel" style="position:fixed;left:400px;top:100px;width:200px;height:120px"><h3>Funnel</h3><p>Not connected</p></div>
<script>console.log('hi', {a: 1}); for (const k of ['mousedown', 'mouseup', 'focusin']) document.addEventListener(k, (e) => console.log('ev', k, e.clientX, e.clientY, e.target.id))</script>`)
	}))
	defer page.Close()
	dir := t.TempDir()
	if d := os.Getenv("BR_TEST_DIR"); d != "" {
		dir = d
	}
	b := newBrowser(nil, dir, home)
	defer func() {
		if t.Failed() {
			log, _ := os.ReadFile(dir + "/browser/chrome.log")
			t.Logf("start error %q; chrome.log:\n%s", b.startErr, log)
		}
	}()
	b.downDir = dir + "/dl"
	b.upDir = dir + "/up"
	defer b.close()
	srv := httptest.NewServer(http.HandlerFunc(b.serveWS))
	defer srv.Close()
	ws, err := wsDial("ws"+strings.TrimPrefix(srv.URL, "http")+"/", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := &brClient{t, ws}
	var tab string
	c.until("a tab", 30*time.Second, func(op int, m map[string]any, _ []byte) bool {
		if tabs, _ := m["tabs"].([]any); m["t"] == "state" && len(tabs) > 0 {
			tab = tabs[0].(map[string]any)["id"].(string)
			return true
		}
		return false
	})
	c.send(map[string]any{"t": "watch", "tab": tab, "w": 800, "h": 600, "dpr": 1})
	c.send(map[string]any{"t": "nav", "tab": tab, "url": page.URL})
	var titled, framed bool
	c.until("the page's title and a frame of it", 15*time.Second, func(op int, m map[string]any, img []byte) bool {
		tabs, _ := m["tabs"].([]any)
		if m["t"] == "state" && len(tabs) > 0 && tabs[0].(map[string]any)["title"] == "Hello" {
			titled = true
		}
		if op == wsBinary && m["kind"] == 1.0 && m["tab"] == tab && m["dw"] == 800.0 {
			if len(img) < 100 || img[0] != 0xFF || img[1] != 0xD8 {
				t.Fatalf("frame: %d bytes, meta %v", len(img), m)
			}
			framed = true
		}
		return titled && framed
	})
	c.send(map[string]any{"t": "dev", "tab": tab, "on": true})
	for _, ty := range []string{"move", "down", "up"} {
		c.send(map[string]any{"t": "mouse", "tab": tab, "type": ty, "x": 50, "y": 50, "btn": "left", "n": 1, "btns": map[string]int{"down": 1}[ty]})
	}
	var gotBoom, got404, gotHi bool
	c.until("the click's console error and 404", 15*time.Second, func(op int, m map[string]any, _ []byte) bool {
		if m["t"] != "dev" {
			return false
		}
		for _, x := range append(m["probs"].([]any), m["cons"].([]any)...) {
			p := x.(map[string]any)
			gotBoom = gotBoom || strings.Contains(p["text"].(string), "boom 42")
			got404 = got404 || p["kind"] == "http" && strings.HasPrefix(p["text"].(string), "404 GET")
			gotHi = gotHi || p["text"] == "hi {a: 1}"
		}
		return gotBoom && got404 && gotHi
	})
	c.send(map[string]any{"t": "probe", "tab": tab, "x": 40, "y": 40, "seq": 7})
	c.until("the probe", 10*time.Second, func(op int, m map[string]any, _ []byte) bool {
		if m["t"] != "probed" {
			return false
		}
		info := m["info"].(map[string]any)
		if info["selector"] != "#b" || info["text"] != "Save" || m["seq"] != 7.0 {
			t.Fatalf("probe: %v", m)
		}
		return true
	})
	// typing: a click in the input, then keys and inserted text
	for _, ty := range []string{"move", "down", "up"} { // as a mouse does: it moves there first
		c.send(map[string]any{"t": "mouse", "tab": tab, "type": ty, "x": 20, "y": 310, "btn": "left", "n": 1, "btns": map[string]int{"down": 1}[ty]})
	}
	c.send(map[string]any{"t": "key", "tab": tab, "type": "keyDown", "key": "a", "code": "KeyA", "kc": 65, "text": "a"})
	c.send(map[string]any{"t": "key", "tab": tab, "type": "keyUp", "key": "a", "code": "KeyA", "kc": 65})
	c.send(map[string]any{"t": "text", "tab": tab, "text": "bé"})
	time.Sleep(300 * time.Millisecond)
	c.send(map[string]any{"t": "eval", "tab": tab, "text": "document.getElementById('i').value"})
	c.until("the input's value", 10*time.Second, func(op int, m map[string]any, _ []byte) bool {
		if m["t"] != "evaled" {
			return false
		}
		if out := m["out"].(map[string]any); out["text"] != "abé" {
			c.send(map[string]any{"t": "eval", "tab": tab, "text": "document.activeElement.id + '|' + innerWidth + 'x' + innerHeight + '|' + outerWidth + 'x' + outerHeight + '|' + document.getElementById('i').getBoundingClientRect().top + '|scale ' + visualViewport.scale + '|dpr ' + devicePixelRatio"})
			c.until("why", 5*time.Second, func(op int, m map[string]any, _ []byte) bool {
				if m["t"] == "evaled" {
					t.Logf("active|title|value: %v", m["out"])
					return true
				}
				return false
			})
			c.send(map[string]any{"t": "dev", "tab": tab, "on": true})
			c.until("the page's events", 3*time.Second, func(op int, m map[string]any, _ []byte) bool {
				if m["t"] == "dev" {
					for _, x := range m["cons"].([]any) {
						if txt := x.(map[string]any)["text"].(string); strings.HasPrefix(txt, "ev") {
							t.Log(txt)
						}
					}
					return true
				}
				return false
			})
			t.Fatalf("typed %v", out)
		}
		return true
	})
	c.send(map[string]any{"t": "shot", "tab": tab})
	c.until("a still", 15*time.Second, func(op int, m map[string]any, img []byte) bool {
		return op == wsBinary && m["kind"] == 2.0 && len(img) > 8 && string(img[1:4]) == "PNG" && m["url"] == page.URL+"/"
	})
	// annotating: the clean picture is kept for an agent, not sent back (q < 0)
	c.send(map[string]any{"t": "shot", "tab": tab, "q": -1, "keep": true})
	c.until("a kept still", 15*time.Second, func(op int, m map[string]any, img []byte) bool {
		if op != wsBinary || m["kind"] != 2.0 {
			return false
		}
		p, _ := m["path"].(string)
		raw, _ := os.ReadFile(p)
		if len(img) != 0 || !strings.HasPrefix(p, dir+"/up/") || len(raw) < 100 || raw[0] != 0xff || !uploadNameRe.MatchString(filepath.Base(p)) {
			t.Fatalf("kept %q (%d bytes), sent %d", p, len(raw), len(img))
		}
		return true
	})
	// a circle drawn a little around the card: the card, and the text inside
	c.send(map[string]any{"t": "area", "tab": tab, "x": 390, "y": 90, "w": 225, "h": 140, "seq": 7})
	c.until("the area", 10*time.Second, func(op int, m map[string]any, _ []byte) bool {
		if m["t"] != "probed" || m["seq"] != 7.0 {
			return false
		}
		info := m["info"].(map[string]any)
		if info["selector"] != "div.funnel" && info["selector"] != "#card" || info["inside"] != "Funnel · Not connected" {
			t.Fatalf("area %v", info)
		}
		return true
	})
	// a second tab, then closed
	c.send(map[string]any{"t": "new", "url": ":1"}) // brURL: localhost:1, which refuses
	var second string
	c.until("the new tab", 10*time.Second, func(op int, m map[string]any, _ []byte) bool {
		if m["t"] == "created" {
			second = m["tab"].(string)
			return true
		}
		return false
	})
	c.send(map[string]any{"t": "close", "tab": second})
	c.until("one tab again", 10*time.Second, func(op int, m map[string]any, _ []byte) bool {
		tabs, _ := m["tabs"].([]any)
		return m["t"] == "state" && len(tabs) == 1
	})
	ws.close()
}

func TestBrMobileUA(t *testing.T) {
	got := brMobileUA("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36")
	if want := "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Mobile Safari/537.36"; got != want {
		t.Fatalf("got %q", got)
	}
}

func TestBrURL(t *testing.T) {
	for in, want := range map[string]string{
		"localhost:3000":                 "http://localhost:3000",
		":5173":                          "http://localhost:5173",
		"8080/api":                       "http://localhost:8080/api",
		"example.com":                    "https://example.com",
		"news.ycombinator.com/item?id=1": "https://news.ycombinator.com/item?id=1",
		"127.0.0.1:9000":                 "http://127.0.0.1:9000",
		"https://x.dev/a":                "https://x.dev/a",
		"how do flexbox gaps work":       "https://duckduckgo.com/?q=how+do+flexbox+gaps+work",
		"about:blank":                    "about:blank",
		"javascript:alert(1)":            "https://duckduckgo.com/?q=javascript%3Aalert%281%29",
	} {
		if got := brURL(in); got != want {
			t.Errorf("brURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// A page that changes a little now and then (here a caret blinking in a focused field) costs patches, not frames:
// once it has settled, a few KB a second instead of a whole frame (and a sharp one) every blink.
func TestBrowserPatches(t *testing.T) {
	home, _ := os.UserHomeDir()
	if chromiumPath(home) == "" || testing.Short() {
		t.Skip("no Chromium")
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Caret</title><body style="font:16px system-ui;padding:40px"><h1>Form</h1><p>`+strings.Repeat("Some text to make the page busy. ", 80)+
			`</p><input id="i" autofocus style="font-size:20px;width:400px"><script>setTimeout(() => document.getElementById('i').focus(), 300)</script>`)
	}))
	defer page.Close()
	b := newBrowser(nil, t.TempDir(), home)
	defer b.close()
	srv := httptest.NewServer(http.HandlerFunc(b.serveWS))
	defer srv.Close()
	ws, err := wsDial("ws"+strings.TrimPrefix(srv.URL, "http")+"/", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := &brClient{t, ws}
	var tab string
	c.until("a tab", 30*time.Second, func(op int, m map[string]any, _ []byte) bool {
		if tabs, _ := m["tabs"].([]any); m["t"] == "state" && len(tabs) > 0 {
			tab = tabs[0].(map[string]any)["id"].(string)
			return true
		}
		return false
	})
	c.send(map[string]any{"t": "watch", "tab": tab, "w": 1400, "h": 860, "dpr": 2})
	c.send(map[string]any{"t": "nav", "tab": tab, "url": page.URL})
	c.until("the page settled", 15*time.Second, func(op int, m map[string]any, _ []byte) bool { return op == wsBinary && m["sharp"] == true })
	time.Sleep(time.Second)
	var whole, patches, bytes int
	start := time.Now()
	c.until("3 s of a blinking caret", 20*time.Second, func(op int, m map[string]any, img []byte) bool {
		if op == wsBinary {
			bytes += len(img)
			switch m["kind"] {
			case 1.0:
				whole++
			case 4.0:
				patches++
				if m["w"].(float64) > 100 || m["h"].(float64) > 100 {
					t.Errorf("a caret's patch is %vx%v", m["w"], m["h"])
				}
			}
		}
		return time.Since(start) > 3*time.Second
	})
	t.Logf("%d patches, %d whole frames, %d KB in 3 s", patches, whole, bytes/1024)
	if patches < 3 || whole > 2 || bytes > 300<<10 {
		t.Fatalf("a blinking caret cost %d patches and %d whole frames (%d KB in 3 s)", patches, whole, bytes/1024)
	}
}

// only servers whose front page is a web page count: an API's JSON, a bare string or a 404 doesn't
func TestProbeHTTPPage(t *testing.T) {
	cases := map[string]struct {
		h    http.HandlerFunc
		page bool
	}{
		"vite": {func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `<!doctype html><html><head><title>App</title></head><body><div id="root"></div></body></html>`)
		}, true},
		"login redirect": {func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			fmt.Fprint(w, `<html><body><form>…</form></body></html>`)
		}, true},
		"json api": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"name":"Alven Estate Agent","status":"running"}`)
		}, false},
		"flask string": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `Hello, World!`)
		}, false},
		"express 404": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(404)
			fmt.Fprint(w, `<!DOCTYPE html><html><body><pre>Cannot GET /</pre></body></html>`)
		}, false},
	}
	for name, c := range cases {
		srv := httptest.NewServer(c.h)
		port, _ := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
		if _, _, page := probeHTTP(port, -port); page != c.page {
			t.Errorf("%s: page %v, want %v", name, page, c.page)
		}
		srv.Close()
	}
}

// DevTools through the panel: its files, its session on the tab (commands and events relayed), the element it
// selects reaching the page, sent to an agent with its CSS, and right-click → Inspect revealing an element in it.
func TestBrowserDevTools(t *testing.T) {
	home, _ := os.UserHomeDir()
	if chromiumPath(home) == "" || testing.Short() {
		t.Skip("no Chromium")
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>DT</title><style>.hero { padding: 4px }</style><body style="margin:0"><h1 id="t" class="hero big" style="color: red; margin: 0; height: 100px">Hi</h1>`)
	}))
	defer page.Close()
	dir := t.TempDir()
	b := newBrowser(nil, dir, home)
	defer b.close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", b.serveWS)
	mux.HandleFunc("GET /api/browser/devtools/f/{ver}/{path...}", b.devtoolsFile)
	mux.HandleFunc("GET /api/browser/devtools/ws/{tab}", b.devtoolsWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, err := wsDial(wsURL+"/ws", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := &brClient{t, ws}
	var tab string
	c.until("a tab", 30*time.Second, func(op int, m map[string]any, _ []byte) bool {
		if tabs, _ := m["tabs"].([]any); m["t"] == "state" && len(tabs) > 0 {
			tab = tabs[0].(map[string]any)["id"].(string)
			return true
		}
		return false
	})
	c.send(map[string]any{"t": "watch", "tab": tab, "w": 800, "h": 600, "dpr": 1})
	c.send(map[string]any{"t": "nav", "tab": tab, "url": page.URL})
	c.until("the page", 15*time.Second, func(op int, m map[string]any, _ []byte) bool {
		tabs, _ := m["tabs"].([]any)
		return m["t"] == "state" && len(tabs) > 0 && tabs[0].(map[string]any)["title"] == "DT"
	})
	// its files: this Chromium's version, the page's own CSP gone (the panel's header says what goes)
	b.mu.Lock()
	ver := b.version
	b.mu.Unlock()
	for _, f := range []string{"inspector.html", "entrypoints/inspector/inspector.js"} {
		resp, err := http.Get(srv.URL + "/api/browser/devtools/f/" + ver + "/" + f)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", f, err, resp)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if f == "inspector.html" && (!strings.Contains(string(body), "DevTools") || strings.Contains(string(body), "Content-Security-Policy")) {
			t.Errorf("inspector.html:\n%s", body)
		}
		if f != "inspector.html" && !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") {
			t.Errorf("%s: %s", f, resp.Header.Get("Content-Type"))
		}
	}
	if resp, _ := http.Get(srv.URL + "/api/browser/devtools/f/" + ver + "/../../tabs.json"); resp != nil && resp.StatusCode == 200 {
		t.Error("a path out of DevTools' folder was served")
	}
	// DevTools' connection: commands answered under its own ids, events on
	dt, err := wsDial(wsURL+"/api/browser/devtools/ws/"+tab, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer dt.close()
	call := func(id int, method string, params any) map[string]any {
		raw, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
		dt.write(wsText, raw)
		dt.c.SetReadDeadline(time.Now().Add(10 * time.Second))
		for {
			_, p, err := dt.read()
			if err != nil {
				t.Fatalf("%s: %v", method, err)
			}
			var m map[string]any
			json.Unmarshal(p, &m)
			if m["id"] == float64(id) {
				if m["error"] != nil {
					t.Fatalf("%s: %v", method, m["error"])
				}
				r, _ := m["result"].(map[string]any)
				return r
			}
		}
	}
	if r := call(1, "Runtime.evaluate", map[string]any{"expression": "1+1", "returnByValue": true}); r["result"].(map[string]any)["value"] != 2.0 {
		t.Fatalf("evaluate: %v", r)
	}
	doc := call(2, "DOM.getDocument", map[string]any{"depth": -1})
	root := doc["root"].(map[string]any)["nodeId"]
	h1 := call(3, "DOM.querySelector", map[string]any{"nodeId": root, "selector": "#t"})["nodeId"]
	call(4, "DOM.setInspectedNode", map[string]any{"nodeId": h1})
	c.until("the element DevTools selected", 10*time.Second, func(op int, m map[string]any, _ []byte) bool {
		if m["t"] == "dtnode" {
			if m["label"] != "h1#t.hero.big" {
				t.Fatalf("label %v", m)
			}
			return true
		}
		return false
	})
	c.send(map[string]any{"t": "dtsel", "tab": tab})
	c.until("it, for an agent", 10*time.Second, func(op int, m map[string]any, _ []byte) bool {
		if m["t"] != "picked" {
			return false
		}
		info, _ := m["info"].(map[string]any)
		rules := fmt.Sprint(info["rules"])
		if info["selector"] != "#t" || !strings.Contains(rules, ".hero { padding: 4px }") || !strings.Contains(rules, `style="color: red;`) {
			t.Fatalf("picked %v", m)
		}
		return true
	})
	// DevTools' session is its own: the tab stays one tab, and stays when DevTools goes
	tabsOf := func() int {
		b.mu.Lock()
		defer b.mu.Unlock()
		n := 0
		for _, id := range b.order {
			if id == tab {
				n++
			}
		}
		if b.tabs[tab] == nil {
			return -1
		}
		return n
	}
	if n := tabsOf(); n != 1 {
		t.Fatalf("with DevTools open the tab is there %d times", n)
	}
	// right-click → Inspect: DevTools hears it as Chromium's picker picking that element
	c.send(map[string]any{"t": "dtreveal", "tab": tab, "x": 20, "y": 20})
	dt.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, p, err := dt.read()
		if err != nil {
			t.Fatalf("no reveal: %v", err)
		}
		var m map[string]any
		json.Unmarshal(p, &m)
		if m["method"] == "Overlay.inspectNodeRequested" {
			if n, _ := m["params"].(map[string]any)["backendNodeId"].(float64); n == 0 {
				t.Fatalf("reveal %v", m)
			}
			break
		}
	}
	dt.close()
	time.Sleep(700 * time.Millisecond)
	if n := tabsOf(); n != 1 {
		t.Fatalf("after DevTools closed the tab is there %d times", n)
	}
}
