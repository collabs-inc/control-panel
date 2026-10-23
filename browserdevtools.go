package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DevTools: Chromium's own, docked beside the page. Its files come from the Chromium the panel runs (the same
// version): a helper Chromium with an empty profile and a debugging port serves them, the panel copies each one it's
// asked for to disk (browser/devtools/<version>/) and serves them from there after, and the helper stops after a
// minute without a fetch. It runs in the owner's browser, in a frame of the panel, and speaks to the tab through the
// panel (/api/browser/devtools/ws/<tab>): the panel gives it a session of its own on the tab (the page's stream and
// drawer keep theirs) and relays its messages both ways, ids renumbered, the frames and workers it attaches included.
// The panel reads along for one thing: the element DevTools selects (DOM.setInspectedNode), which the page can send
// to an agent. Only a tab the viewer may drive: the owner's, or an agent's while the owner has taken it over.

var brDTPathRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./-]{0,300}$`)

type bbridge struct {
	b     *browser
	t     *btab
	ws    *wsConn
	sess  string          // its session on the tab
	kids  map[string]bool // the sessions of the frames and workers it attached (b.mu)
	q     chan []byte     // what goes to DevTools: a writer of its own, so a slow page never holds up the browser
	done  chan struct{}
	once  sync.Once
	ready chan struct{} // DevTools has the document: a reveal can go to it
	rdy   sync.Once
	pick  bool // the panel turned Chromium's picker on: off again when it picks

	nodeSess string // the element DevTools selected (b.mu): the session it's in, and its id there
	nodeID   int
}

func (br *bbridge) send(m any) {
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	select {
	case br.q <- raw:
	case <-br.done:
	default: // DevTools isn't reading: it's gone, or too slow to keep
		br.close()
	}
}

func (br *bbridge) close() {
	br.once.Do(func() {
		close(br.done)
		br.ws.close()
	})
}

// event: what Chromium sent on the bridge's sessions, on to DevTools (b.event's goroutine: it mustn't wait)
func (br *bbridge) event(m cdpMsg) {
	switch m.Method {
	case "Target.attachedToTarget", "Target.detachedFromTarget":
		var p struct{ SessionID string }
		json.Unmarshal(m.Params, &p)
		b := br.b
		b.mu.Lock()
		if m.Method == "Target.attachedToTarget" {
			br.kids[p.SessionID] = true
			b.bridges[p.SessionID] = br
		} else {
			delete(br.kids, p.SessionID)
			delete(b.bridges, p.SessionID)
		}
		b.mu.Unlock()
	case "Overlay.inspectNodeRequested":
		if br.pick && m.SessionID == br.sess {
			br.pick = false
			br.t.src.cdp.fire(br.sess, "Overlay.setInspectMode", map[string]any{"mode": "none", "highlightConfig": brHighlight})
		}
	}
	out := map[string]any{"method": m.Method, "params": m.Params}
	if m.SessionID != br.sess {
		out["sessionId"] = m.SessionID
	}
	br.send(out)
}

// devtoolsWS: DevTools' connection to a tab.
func (b *browser) devtoolsWS(w http.ResponseWriter, r *http.Request) {
	t := b.mine(r.PathValue("tab"))
	if t == nil {
		http.Error(w, "no such tab, or not yours to inspect (take the agent's browser over first)", http.StatusForbidden)
		return
	}
	ws, err := wsUpgrade(w, r, 64<<20)
	if err != nil {
		return
	}
	var att struct{ SessionID string }
	if err := t.src.cdp.call("", "Target.attachToTarget", map[string]any{"targetId": t.id, "flatten": true}, &att); err != nil || att.SessionID == "" {
		ws.closeWith(1011)
		return
	}
	br := &bbridge{b: b, t: t, ws: ws, sess: att.SessionID, kids: map[string]bool{}, q: make(chan []byte, 8192), done: make(chan struct{}), ready: make(chan struct{})}
	b.mu.Lock()
	b.bridges[br.sess] = br
	b.dtTab[t.id] = br
	b.mu.Unlock()
	defer func() {
		br.close()
		b.mu.Lock()
		delete(b.bridges, br.sess)
		for k := range br.kids {
			delete(b.bridges, k)
		}
		if b.dtTab[t.id] == br {
			delete(b.dtTab, t.id)
		}
		b.mu.Unlock()
		t.src.cdp.fire("", "Target.detachFromTarget", map[string]any{"sessionId": br.sess})
	}()
	go func() {
		for {
			select {
			case raw := <-br.q:
				if ws.write(wsText, raw) != nil {
					br.close()
					return
				}
			case <-br.done:
				return
			}
		}
	}()
	for {
		op, raw, err := ws.read()
		if err != nil {
			return
		}
		if op != wsText {
			continue
		}
		var m struct {
			ID        int64           `json:"id"`
			Method    string          `json:"method"`
			Params    json.RawMessage `json:"params"`
			SessionID string          `json:"sessionId"`
		}
		if json.Unmarshal(raw, &m) != nil || m.Method == "" {
			continue
		}
		sess := m.SessionID
		b.mu.Lock()
		ok := sess == "" || br.kids[sess]
		b.mu.Unlock()
		if !ok {
			br.send(map[string]any{"id": m.ID, "sessionId": sess, "error": map[string]any{"code": -32001, "message": "no such session"}})
			continue
		}
		if sess == "" {
			sess = br.sess
		}
		if strings.HasPrefix(m.Method, "Page.") && strings.Contains(m.Method, "creencast") { // its own picture of the page:
			// the panel shows the page already, and a second screencast would take frames from the panel's stream
			out := map[string]any{"id": m.ID, "result": json.RawMessage("{}")}
			if m.SessionID != "" {
				out["sessionId"] = m.SessionID
			}
			br.send(out)
			continue
		}
		if m.Method == "DOM.setInspectedNode" { // the element DevTools selected: the page can send it to an agent
			var p struct{ NodeID int }
			json.Unmarshal(m.Params, &p)
			b.mu.Lock()
			br.nodeSess, br.nodeID = sess, p.NodeID
			b.mu.Unlock()
			go b.dtNodeLabel(br, sess, p.NodeID)
		}
		id, method, from := m.ID, m.Method, m.SessionID
		t.src.cdp.relay(sess, m.Method, m.Params, func(res cdpMsg) {
			out := map[string]any{"id": id}
			if from != "" {
				out["sessionId"] = from
			}
			if res.Error != nil {
				out["error"] = res.Error
			} else if len(res.Result) > 0 {
				out["result"] = res.Result
			} else {
				out["result"] = json.RawMessage("{}")
			}
			br.send(out)
			if method == "DOM.getDocument" && from == "" {
				br.rdy.Do(func() { close(br.ready) })
			}
		})
	}
}

// dtNodeLabel tells the pages showing the tab which element DevTools selected (tag#id.class).
func (b *browser) dtNodeLabel(br *bbridge, sess string, id int) {
	var r struct {
		Node struct {
			LocalName  string
			NodeName   string
			Attributes []string
		}
	}
	if br.t.src.cdp.callT(3*time.Second, sess, "DOM.describeNode", map[string]any{"nodeId": id}, &r) != nil {
		return
	}
	label := cmpOr(r.Node.LocalName, strings.ToLower(r.Node.NodeName))
	for i := 0; i+1 < len(r.Node.Attributes); i += 2 {
		switch k, v := r.Node.Attributes[i], r.Node.Attributes[i+1]; k {
		case "id":
			label += "#" + v
		case "class":
			for _, c := range strings.Fields(v) {
				if len(label) > 90 {
					break
				}
				label += "." + c
			}
		}
	}
	b.mu.Lock()
	cur := br.nodeSess == sess && br.nodeID == id
	b.mu.Unlock()
	if cur {
		b.toTab(br.t.id, map[string]any{"t": "dtnode", "tab": br.t.id, "label": trunc(label, 120)})
	}
}

// toTab sends a message to the pages showing a tab.
func (b *browser) toTab(tab string, m any) {
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for v := range b.viewers {
		if v.tab == tab {
			v.queue(raw)
			v.kick()
		}
	}
}

// dtBridge: the tab's DevTools once it has the document, waiting a while for it to open (the page opens it first).
func (b *browser) dtBridge(tab string, wait time.Duration) *bbridge {
	for end := time.Now().Add(wait); ; time.Sleep(100 * time.Millisecond) {
		b.mu.Lock()
		br := b.dtTab[tab]
		b.mu.Unlock()
		if br != nil {
			select {
			case <-br.ready:
				return br
			default:
			}
		}
		if time.Now().After(end) {
			return nil
		}
	}
}

// dtReveal shows the element at a point of the page in DevTools' Elements (the page's right-click → Inspect).
func (b *browser) dtReveal(v *bview, t *btab, x, y float64) {
	br := b.dtBridge(t.id, 20*time.Second)
	if br == nil {
		v.send(map[string]any{"t": "toast", "text": "DevTools didn't open"})
		return
	}
	var n struct{ BackendNodeID int }
	if br.t.src.cdp.call(br.sess, "DOM.getNodeForLocation", map[string]any{"x": int(x), "y": int(y), "includeUserAgentShadowDOM": false, "ignorePointerEventsNone": true}, &n) != nil || n.BackendNodeID == 0 {
		return
	}
	// as Chromium says it when its picker picks: DevTools selects and shows the node
	br.send(map[string]any{"method": "Overlay.inspectNodeRequested", "params": map[string]any{"backendNodeId": n.BackendNodeID}})
}

// dtPick turns Chromium's element picker on for DevTools (⌘⇧C): a click on the page selects in Elements.
func (b *browser) dtPick(t *btab) {
	br := b.dtBridge(t.id, 20*time.Second)
	if br == nil {
		return
	}
	br.pick = true
	br.t.src.cdp.call(br.sess, "Overlay.setInspectMode", map[string]any{"mode": "searchForNode", "highlightConfig": brHighlight}, nil)
}

// dtSelected: the element DevTools has selected, for an agent: what describe says, and the CSS rules on it.
func (b *browser) dtSelected(v *bview, t *btab) {
	b.mu.Lock()
	br := b.dtTab[t.id]
	var sess string
	var id int
	if br != nil {
		sess, id = br.nodeSess, br.nodeID
	}
	b.mu.Unlock()
	if br == nil || id == 0 {
		v.send(map[string]any{"t": "picked", "tab": t.id, "info": map[string]any{"error": "Select an element in DevTools' Elements first"}})
		return
	}
	c := br.t.src.cdp
	var d struct{ Node struct{ BackendNodeID int } }
	if c.call(sess, "DOM.describeNode", map[string]any{"nodeId": id}, &d) != nil {
		v.send(map[string]any{"t": "picked", "tab": t.id, "info": map[string]any{"error": "That element is gone"}})
		return
	}
	info := b.describeOn(c, sess, d.Node.BackendNodeID)
	if info != nil && info["error"] == nil {
		info["rules"] = b.dtRules(c, sess, id)
	}
	v.send(map[string]any{"t": "picked", "tab": t.id, "info": info})
}

// dtRules: the author CSS rules that match a node (not the browser's own), as text: selector { declarations }.
func (b *browser) dtRules(c *cdpConn, sess string, id int) []string {
	var r struct {
		InlineStyle     *struct{ CSSText string }
		MatchedCSSRules []struct {
			Rule struct {
				Origin       string
				SelectorList struct{ Text string }
				Style        struct{ CSSText string }
				StyleSheetID string
			}
		}
	}
	c.call(sess, "CSS.enable", nil, nil) // DevTools has it on its session already; a no-op then
	if c.callT(5*time.Second, sess, "CSS.getMatchedStylesForNode", map[string]any{"nodeId": id}, &r) != nil {
		return nil
	}
	var out []string
	if r.InlineStyle != nil && strings.TrimSpace(r.InlineStyle.CSSText) != "" {
		out = append(out, "style=\""+trunc(strings.TrimSpace(r.InlineStyle.CSSText), 400)+"\"")
	}
	for i := len(r.MatchedCSSRules) - 1; i >= 0 && len(out) < 14; i-- { // the most specific first
		x := r.MatchedCSSRules[i].Rule
		if x.Origin == "user-agent" || strings.TrimSpace(x.Style.CSSText) == "" {
			continue
		}
		out = append(out, trunc(x.SelectorList.Text, 200)+" { "+trunc(strings.Join(strings.Fields(x.Style.CSSText), " "), 500)+" }")
	}
	return out
}

// closeBridges ends DevTools on an agent's tabs when the owner hands the browser back.
func (b *browser) closeBridges(key string) {
	b.mu.Lock()
	var brs []*bbridge
	for _, br := range b.dtTab {
		if a := br.t.src.agent; a != nil && a.key == key {
			brs = append(brs, br)
		}
	}
	b.mu.Unlock()
	for _, br := range brs {
		br.close()
	}
}

// ---- its files ----

type bdthelper struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	stop func()
	port string
	last time.Time
}

// stopDT ends the helper now (the panel stopping).
func (b *browser) stopDT() {
	h := &b.dt
	h.mu.Lock()
	stop := h.stop
	h.cmd, h.stop = nil, nil
	h.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// devtoolsFile serves a file of DevTools, from disk, or fetched from the helper first.
func (b *browser) devtoolsFile(w http.ResponseWriter, r *http.Request) {
	ver, rest := r.PathValue("ver"), r.PathValue("path")
	b.mu.Lock()
	want := b.version
	b.mu.Unlock()
	if ver == "" || ver != want || !brDTPathRe.MatchString(rest) || strings.Contains(rest, "..") {
		http.NotFound(w, r)
		return
	}
	file := filepath.Join(b.dir, "devtools", ver, filepath.FromSlash(rest))
	body, err := os.ReadFile(file)
	if err != nil {
		if body, err = b.dtFetch(rest); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, r)
			} else {
				http.Error(w, err.Error(), http.StatusBadGateway)
			}
			return
		}
		os.MkdirAll(filepath.Dir(file), 0o700)
		if os.WriteFile(file+".tmp", body, 0o600) == nil {
			os.Rename(file+".tmp", file)
		}
	}
	if strings.HasSuffix(rest, ".html") { // its own CSP allows a websocket to 127.0.0.1 only: the panel's header says what goes
		body = brDTMetaCSP.ReplaceAll(body, nil)
	}
	ct := mime.TypeByExtension(path.Ext(rest))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable") // the version is in the path
	w.Write(body)
}

var brDTMetaCSP = regexp.MustCompile(`(?is)<meta\s+http-equiv="Content-Security-Policy"[^>]*>`)

// dtFetch gets a file from the helper Chromium, started when it isn't running.
func (b *browser) dtFetch(rest string) ([]byte, error) {
	h := &b.dt
	h.mu.Lock()
	if h.cmd == nil {
		if err := b.dtStart(); err != nil {
			h.mu.Unlock()
			return nil, err
		}
	}
	h.last = time.Now()
	port := h.port
	h.mu.Unlock()
	c := http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := c.Get("http://127.0.0.1:" + port + "/devtools/" + rest)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, os.ErrNotExist
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("devtools: %s: %s", rest, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// dtStart starts the helper (h.mu held): headless, an empty profile of its own, the debugging port that serves the
// files. It stops a minute after the last fetch.
func (b *browser) dtStart() error {
	h := &b.dt
	bin := chromiumPath(b.home)
	if bin == "" {
		return errors.New("no Chromium")
	}
	dir, err := os.MkdirTemp("", "cp-devtools-")
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "--headless", "--remote-debugging-port=0", "--user-data-dir="+dir, "--no-first-run", "--no-default-browser-check", "--no-proxy-server", "about:blank")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL} // it dies with the panel: it's only a file server
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return err
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			cmd.Wait()
			os.RemoveAll(dir)
		})
	}
	var port string
	for i := 0; i < 100 && port == ""; i++ {
		time.Sleep(100 * time.Millisecond)
		if raw, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort")); err == nil {
			port = strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
		}
	}
	if _, err := strconv.Atoi(port); err != nil {
		stop()
		return errors.New("the DevTools helper didn't start")
	}
	h.cmd, h.stop, h.port, h.last = cmd, stop, port, time.Now()
	go func() {
		for {
			time.Sleep(15 * time.Second)
			h.mu.Lock()
			if h.cmd != cmd { // stopped meanwhile
				h.mu.Unlock()
				return
			}
			if time.Since(h.last) > time.Minute {
				h.cmd, h.stop = nil, nil
				h.mu.Unlock()
				stop()
				return
			}
			h.mu.Unlock()
		}
	}()
	return nil
}

// brDTCSP: what DevTools' pages may do (securityHeaders): its scripts, its styles, its workers, the websocket to the
// panel; and a frame of the panel only.
const brDTCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; " +
	"connect-src 'self' ws: wss: data:; worker-src 'self' blob:; frame-src 'self' data:; base-uri 'self'; form-action 'none'; frame-ancestors 'self'"
