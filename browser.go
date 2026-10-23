package main

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The Browser: a Chromium on the VM, shown in the page. The panel runs the owner's own (headless, a persistent
// profile so logins stay, driven over a pipe no other process can reach) and watches the agents' (their cube-browser
// sessions, each its own Chromium with a localhost debugging port: browseragents.go). A tab is streamed as JPEG
// frames (Page.startScreencast) over a websocket, and the page's mouse, wheel, touch and keys come back the same way.
// Console messages, exceptions and failed requests are kept per tab: the Problems drawer, and what an annotation
// sends an agent.
//
// Flow control, end to end: a viewer has at most two frames in flight (sent, not yet drawn); Chromium is told a frame
// was taken (screencastFrameAck) only when some viewer has room, so a slow phone gets fewer frames, never a backlog,
// and Chromium doesn't encode frames nobody sees. Only tabs someone is looking at stream.

// The stream moves at 1x and sharpens at rest: while a page changes (scrolling, an animation) its frames go at CSS
// size and JPEG quality brQuality (~120 KB, ~30 fps for a text-heavy page); brSharpAfter after the last one, one
// sharp frame follows at the viewer's density (WebP at 2x, ~280 KB). A 2x stream would be ~500 KB a frame at half
// the frame rate.
var brQuality = 65

const brSharpAfter = 180 * time.Millisecond

const (
	brFramesInFlight = 2
	brProblemsKept   = 300
	brConsoleKept    = 500
	brNetKept        = 400
)

type browser struct {
	dir     string // ~/.local/state/control-panel/browser
	home    string
	downDir string
	upDir   string // the chat's uploads: files for agents, by path
	s       *server
	recLock *os.File // held: this panel records the agents' trails (browseragents.go recorder)

	mu        sync.Mutex
	me        *bsrc            // the owner's Chromium (nil until a viewer needs it)
	agents    map[string]*bsrc // agents' Chromiums, by their registry id
	viewers   map[*bview]bool
	tabs      map[string]*btab // every tab of every Chromium, by id
	order     []string         // the owner's tabs, in their strip order
	starting  bool
	startErr  string
	ua        string // the owner's user agent: Chromium's, without "Headless"
	saveTimer *time.Timer
	pushTimer *time.Timer
	downloads []*bdownload
	agentLog  map[string]*bagentLog // the agents' command timelines (browseragents.go)
	static    map[string]*bstatic   // folders the panel serves itself (Run on a plain-HTML folder)
	pendingVP map[string]bvp        // restored tabs' viewports, until their attach arrives
	version   string                // the owner's Chromium's (DevTools' files are this version's)
	bridges   map[string]*bbridge   // DevTools' sessions (and its frames' and workers'), by session (browserdevtools.go)
	dtTab     map[string]*bbridge   // and the newest DevTools of each tab
	dt        bdthelper
}

// bsrc is one Chromium.
// mine: the owner's Chromium (an agent's has its agent).
func (s *bsrc) mine() bool { return s.agent == nil }

type bsrc struct {
	id    string // "me", or the agent's registry id
	cdp   *cdpConn
	cmd   *exec.Cmd
	agent *bagent // nil for the owner's
	sess  map[string]*btab
	cur   *btab // an agent's: the tab it works in (the one it opened or navigated last)
	down  bool
}

type btab struct {
	src                         *bsrc
	id                          string // the target id (also its main frame's id)
	sess                        string // our CDP session on it
	URL                         string
	Title                       string
	Icon                        string // its favicon's URL (fetched through the panel: /api/browser/icon)
	Loading, Back, Fwd, Crashed bool
	opener                      string
	born                        time.Time
	ready                       chan struct{} // closed once its domains are on: a restored tab navigates after (its first console lines keep their previews)

	vp         bvp
	fitW       int
	fitH       int
	fitDPR     float64
	fitMob     bool
	cssW       float64        // the page's viewport in CSS px: what input coordinates are in (the screencast's own
	cssH       float64        // metadata gives the headless window's size, not the emulated one)
	win        int            // its window (each of the owner's tabs has its own) and what the window has around
	uiW, uiH   int            // the page: the screencast captures the window's view, so the window is sized to the viewport
	applied    string         // the emulation last sent (bvp + fit, as JSON): a resize that changes nothing sends nothing
	casting    string         // "", "full" or "thumb": the screencast running
	castKey    string         // its parameters: a change restarts it
	castWant   map[string]any // the screencast wanted (nil: none)
	castDone   string         // the one Chromium has: applied under castMu, so a stale stop can't land last
	castMu     sync.Mutex
	emuMu      sync.Mutex // one emulate at a time per tab (attach, watch, viewport)
	frame      *bframe
	sharpT     *time.Timer // the sharp frame after the page settles
	wholeSeq   int64       // the seq of the last whole frame: a sharp capture taken after it still stands, patches or not
	stale      bool        // patches went after t.frame: a viewer that starts now needs a fresh capture
	lastMotion time.Time   // when the last screencast frame came (browserpatch.go)
	prevJPEG   []byte      // and the last whole frame, to compare the next one with
	prevDec    *image.YCbCr
	patching   bool
	rapid      int // screencast frames in a row that came quickly
	pend       *bpend
	ackHeld    []int // frame numbers Chromium waits on: acknowledged when a viewer has room
	seq        int64
	dialog     *bdialog
	chooser    *bchooser

	probs      []bprob
	cons       []bprob
	nets       []*bnet
	netByID    map[string]*bnet
	nE, nW, nN int   // errors, warnings, failed requests since the last navigation
	devSeq     int64 // bumped on every new entry: viewers with the drawer open get what's past their mark
	devT       *time.Timer
}

type bvp struct {
	Mode   string  `json:"mode"` // "fit" (the viewer's own size), "device"
	Name   string  `json:"name"` // the preset's name ("iPhone 15")
	W      int     `json:"w"`
	H      int     `json:"h"`
	DPR    float64 `json:"dpr"`
	Mobile bool    `json:"mobile"`
	Scheme string  `json:"scheme"` // "", "dark", "light"
	Net    string  `json:"net"`    // "", "fast4g", "slow4g", "3g", "offline"
	UA     string  `json:"ua,omitempty"`
}

type bframe struct {
	seq  int64
	img  []byte
	meta []byte // JSON: tab, seq, device size
}

type bdialog struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Default string `json:"default"`
	URL     string `json:"url"`
}

type bchooser struct {
	Mode    string `json:"mode"`
	Backend int    `json:"-"`
}

type bprob struct {
	Seq   int64  `json:"seq"`
	T     int64  `json:"t"`     // ms
	Kind  string `json:"kind"`  // console, exception, log, http, net
	Level string `json:"level"` // error, warning, info, log, debug
	Text  string `json:"text"`
	Src   string `json:"src,omitempty"` // url:line
	Stack string `json:"stack,omitempty"`
}

type bnet struct {
	Seq    int64   `json:"seq"`
	ID     string  `json:"-"`
	T      int64   `json:"t"`
	Method string  `json:"method"`
	URL    string  `json:"url"`
	Type   string  `json:"type"`
	Status int     `json:"status"`
	Mime   string  `json:"mime,omitempty"`
	Size   int64   `json:"size"`
	MS     float64 `json:"ms"`
	Err    string  `json:"err,omitempty"`
	Done   bool    `json:"done"`
	ts     float64
	// an agent's page's API call (XHR, fetch), kept in its trail when it ends (browsercalls.go)
	api        bool
	reqH, resH map[string]string
	post       string
	hasPost    bool
}

type bdownload struct {
	GUID  string `json:"guid"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	State string `json:"state"`
	Got   int64  `json:"got"`
	Total int64  `json:"total"`
	Path  string `json:"path"`
	T     int64  `json:"t"`
}

func newBrowser(s *server, stateDir, home string) *browser {
	b := &browser{dir: filepath.Join(stateDir, "browser"), home: home, downDir: filepath.Join(home, "Downloads"), s: s,
		agents: map[string]*bsrc{}, viewers: map[*bview]bool{}, tabs: map[string]*btab{}, agentLog: map[string]*bagentLog{}, static: map[string]*bstatic{}, pendingVP: map[string]bvp{},
		bridges: map[string]*bbridge{}, dtTab: map[string]*bbridge{}}
	if s != nil {
		b.upDir = s.uploadDir
	}
	os.MkdirAll(b.dir, 0o700)
	go b.titles()
	return b
}

// ---- the owner's Chromium ----

// chromiumPath: the newest Chromium Playwright installed (the Cube image's, then this user's cache), else one on PATH.
func chromiumPath(home string) string {
	var found []string
	for _, root := range []string{"/opt/cube/browser-binaries", filepath.Join(home, ".cache/ms-playwright")} {
		m, _ := filepath.Glob(filepath.Join(root, "chromium-*", "chrome-linux64", "chrome"))
		found = append(found, m...)
	}
	rev := func(p string) int {
		n, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(filepath.Dir(filepath.Dir(p))), "chromium-"))
		return n
	}
	sort.Slice(found, func(i, j int) bool { return rev(found[i]) > rev(found[j]) })
	if len(found) > 0 {
		return found[0]
	}
	for _, n := range []string{"chromium", "chromium-browser", "google-chrome"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// ensureMe starts the owner's Chromium if it isn't running, then restores the tabs it had.
func (b *browser) ensureMe() {
	b.mu.Lock()
	if (b.me != nil && !b.me.down) || b.starting {
		b.mu.Unlock()
		return
	}
	b.starting = true
	b.mu.Unlock()
	src, err := b.launch()
	b.mu.Lock()
	b.starting = false
	if err != nil {
		b.startErr = err.Error()
		log.Println("browser:", err)
		b.mu.Unlock()
		b.push()
		return
	}
	b.startErr = ""
	b.me = src
	b.mu.Unlock()
	b.restore(src)
	b.push()
}

func (b *browser) launch() (*bsrc, error) {
	bin := chromiumPath(b.home)
	if bin == "" {
		return nil, errors.New("no Chromium on this box (Playwright's isn't installed)")
	}
	profile := filepath.Join(b.dir, "profile")
	os.MkdirAll(profile, 0o700)
	freeProfile(profile)
	// the panel reopens the tabs itself (tabs.json): Chromium's own restore, after what looks to it like a crash (the
	// panel restarting closes its pipe), would open them a second time
	os.RemoveAll(filepath.Join(profile, "Default", "Sessions"))
	if old, _ := filepath.Glob(filepath.Join(b.dir, "upload-*")); len(old) > 0 { // files handed to pages: a day
		for _, d := range old {
			if fi, err := os.Stat(d); err == nil && time.Since(fi.ModTime()) > 24*time.Hour {
				os.RemoveAll(d)
			}
		}
	}
	os.MkdirAll(b.downDir, 0o755)
	inR, inW, err := os.Pipe() // Chromium reads its commands from inR (its fd 3)
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe() // and answers on outW (its fd 4)
	if err != nil {
		return nil, err
	}
	args := []string{"--headless", "--remote-debugging-pipe", "--user-data-dir=" + profile,
		"--no-first-run", "--no-default-browser-check", "--no-proxy-server", "--mute-audio", "--password-store=basic",
		"--disable-features=Translate,MediaRouter,OptimizationHints,AutofillServerCommunication", "--disable-component-update",
		"--disable-background-timer-throttling", "--disable-renderer-backgrounding", "--disable-backgrounding-occluded-windows",
		"--hide-scrollbars=false", "--window-size=1280,800", "--force-device-scale-factor=2", "about:blank"}
	cmd := exec.Command(bin, args...)
	cmd.ExtraFiles = []*os.File{inR, outW}
	cmd.Dir = b.home
	for _, kv := range os.Environ() { // straight to the internet: the box's proxy is for the agents' API calls
		if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, "http_proxy") && !strings.EqualFold(k, "https_proxy") && !strings.EqualFold(k, "all_proxy") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if f, err := os.OpenFile(filepath.Join(b.dir, "chrome.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
		cmd.Stderr = f
		cmd.Stdout = f
		defer f.Close()
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	inR.Close()
	outW.Close()
	src := &bsrc{id: "me", cmd: cmd, sess: map[string]*btab{}}
	src.cdp = cdpPipe(inW, outR, func(m cdpMsg) { b.event(src, m) })
	go func() {
		cmd.Wait()
		src.cdp.close()
		b.srcDown(src)
	}()
	var ver struct{ UserAgent, Product string }
	if err := src.cdp.callT(20*time.Second, "", "Browser.getVersion", nil, &ver); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("Chromium didn't answer: %v (see %s)", err, filepath.Join(b.dir, "chrome.log"))
	}
	b.mu.Lock()
	b.ua = strings.Replace(ver.UserAgent, "HeadlessChrome", "Chrome", 1)
	b.version = ver.Product[strings.LastIndex(ver.Product, "/")+1:]
	b.mu.Unlock()
	src.cdp.call("", "Browser.setDownloadBehavior", map[string]any{"behavior": "allow", "downloadPath": b.downDir, "eventsEnabled": true}, nil)
	src.cdp.call("", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil)
	if err := src.cdp.call("", "Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true}, nil); err != nil {
		cmd.Process.Kill()
		return nil, err
	}
	return src, nil
}

// freeProfile: a Chromium left from an earlier panel still holds the profile (its SingletonLock names it). It's
// ours, and nothing can reach it any more: stop it so this one can start.
func freeProfile(profile string) {
	l, err := os.Readlink(filepath.Join(profile, "SingletonLock"))
	if err != nil {
		return
	}
	i := strings.LastIndexByte(l, '-')
	pid, _ := strconv.Atoi(l[i+1:])
	if pid <= 1 {
		return
	}
	if cl, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); bytes.Contains(cl, []byte(profile)) {
		syscall.Kill(pid, syscall.SIGKILL)
		time.Sleep(300 * time.Millisecond)
	}
}

func (b *browser) srcDown(src *bsrc) {
	b.mu.Lock()
	src.down = true
	for _, t := range src.sess {
		delete(b.tabs, t.id)
	}
	if src.mine() {
		if b.me == src {
			b.me, b.order = nil, nil // tabs.json keeps them: the next start reopens them
			if len(b.viewers) > 0 {  // it crashed (or was killed) while you watch: back in a second
				time.AfterFunc(time.Second, b.ensureMe)
			}
		}
	} else if b.agents[src.id] == src {
		delete(b.agents, src.id)
		go src.agent.trail.close()
		if l := b.agentLog[src.agent.key]; l != nil {
			l.ended = time.Now()
		}
	}
	b.mu.Unlock()
	b.push()
}

// ---- tabs saved across restarts ----

type bsaved struct {
	Tabs []struct {
		URL string `json:"url"`
		VP  bvp    `json:"vp"`
	} `json:"tabs"`
}

func (b *browser) restore(src *bsrc) {
	var sv bsaved
	if raw, err := os.ReadFile(filepath.Join(b.dir, "tabs.json")); err == nil {
		json.Unmarshal(raw, &sv)
	}
	// the about:blank Chromium opens with becomes the first tab (its attach is on its way)
	var first *btab
	for i := 0; i < 40 && first == nil; i++ {
		b.mu.Lock()
		for _, t := range src.sess {
			first = t
		}
		b.mu.Unlock()
		if first == nil {
			time.Sleep(50 * time.Millisecond)
		}
	}
	for i, st := range sv.Tabs {
		if i == 0 && first != nil {
			b.mu.Lock()
			first.vp = st.VP
			b.mu.Unlock()
			if st.URL != "" && st.URL != "about:blank" {
				select {
				case <-first.ready:
				case <-time.After(5 * time.Second):
				}
				src.cdp.call(first.sess, "Page.navigate", map[string]any{"url": st.URL}, nil)
			}
			continue
		}
		var r struct{ TargetID string }
		if src.cdp.call("", "Target.createTarget", map[string]any{"url": cmp.Or(st.URL, "about:blank"), "newWindow": true, "background": true}, &r) == nil {
			b.mu.Lock()
			if t := b.tabs[r.TargetID]; t != nil {
				t.vp = st.VP
			} else {
				b.pendingVP[r.TargetID] = st.VP
			}
			b.mu.Unlock()
		}
	}
}

func (b *browser) save() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.saveTimer != nil {
		b.saveTimer.Stop()
	}
	b.saveTimer = time.AfterFunc(time.Second, func() {
		b.mu.Lock()
		var sv bsaved
		for _, id := range b.order {
			if t := b.tabs[id]; t != nil {
				sv.Tabs = append(sv.Tabs, struct {
					URL string `json:"url"`
					VP  bvp    `json:"vp"`
				}{t.URL, t.vp})
			}
		}
		b.mu.Unlock()
		if raw, err := json.Marshal(sv); err == nil {
			os.WriteFile(filepath.Join(b.dir, "tabs.json"), raw, 0o600)
		}
	})
}

// ---- CDP events ----

type cdpTargetInfo struct {
	TargetID         string `json:"targetId"`
	Type             string `json:"type"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	OpenerID         string `json:"openerId"`
	BrowserContextID string `json:"browserContextId"`
}

func (b *browser) event(src *bsrc, m cdpMsg) {
	if os.Getenv("BR_DEBUG") != "" {
		log.Printf("cdp %s %s %s", src.id, m.Method, trunc(string(m.Params), 300))
	}
	if m.SessionID != "" { // DevTools' sessions: its, not the panel's
		b.mu.Lock()
		br := b.bridges[m.SessionID]
		b.mu.Unlock()
		if br != nil {
			br.event(m)
			return
		}
	}
	switch m.Method {
	case "Target.attachedToTarget":
		var p struct {
			SessionID          string        `json:"sessionId"`
			TargetInfo         cdpTargetInfo `json:"targetInfo"`
			WaitingForDebugger bool          `json:"waitingForDebugger"`
		}
		json.Unmarshal(m.Params, &p)
		if p.TargetInfo.Type != "page" {
			if p.WaitingForDebugger {
				src.cdp.fire(p.SessionID, "Runtime.runIfWaitingForDebugger", nil)
			}
			return
		}
		b.mu.Lock()
		have := b.tabs[p.TargetInfo.TargetID]
		b.mu.Unlock()
		if have != nil && have.src == src { // a second session on a tab the panel has: DevTools' (browserdevtools.go), not a tab
			return
		}
		b.attach(src, p.SessionID, p.TargetInfo, p.WaitingForDebugger)
		return
	case "Target.targetInfoChanged":
		var p struct{ TargetInfo cdpTargetInfo }
		json.Unmarshal(m.Params, &p)
		b.mu.Lock()
		t := b.tabs[p.TargetInfo.TargetID]
		p.TargetInfo.URL, p.TargetInfo.Title = trunc(p.TargetInfo.URL, 2000), trunc(p.TargetInfo.Title, 300)
		if t != nil && t.src == src && (t.URL != p.TargetInfo.URL || t.Title != p.TargetInfo.Title) {
			t.URL, t.Title = p.TargetInfo.URL, p.TargetInfo.Title
			b.mu.Unlock()
			b.push()
			if src.mine() {
				b.save()
			}
			return
		}
		b.mu.Unlock()
		return
	case "Target.detachedFromTarget", "Target.targetDestroyed":
		var p struct{ SessionID, TargetID string }
		json.Unmarshal(m.Params, &p)
		b.mu.Lock()
		if br := b.bridges[p.SessionID]; br != nil && br.sess == p.SessionID { // DevTools' tab went: so does DevTools
			b.mu.Unlock()
			br.close()
			return
		}
		var t *btab
		if p.SessionID != "" {
			t = src.sess[p.SessionID]
		} else if x := b.tabs[p.TargetID]; x != nil && x.src == src {
			t = x
		}
		if t != nil {
			delete(src.sess, t.sess)
			delete(b.tabs, t.id)
			b.order = slices.DeleteFunc(b.order, func(id string) bool { return id == t.id })
			if src.cur == t {
				src.cur = nil
				for _, x := range src.sess {
					if src.cur == nil || x.born.After(src.cur.born) {
						src.cur = x
					}
				}
			}
		}
		b.mu.Unlock()
		if t != nil {
			b.push()
			if src.mine() {
				b.save()
			}
		}
		return
	case "Target.targetCrashed":
		var p struct{ TargetID string }
		json.Unmarshal(m.Params, &p)
		b.mu.Lock()
		if t := b.tabs[p.TargetID]; t != nil {
			t.Crashed, t.casting = true, ""
		}
		b.mu.Unlock()
		b.push()
		return
	case "Browser.downloadWillBegin":
		var p struct{ GUID, URL, SuggestedFilename string }
		json.Unmarshal(m.Params, &p)
		b.mu.Lock()
		b.downloads = append(b.downloads, &bdownload{GUID: p.GUID, Name: p.SuggestedFilename, URL: p.URL, State: "inProgress", T: time.Now().UnixMilli(),
			Path: filepath.Join(b.downDir, p.SuggestedFilename)})
		if len(b.downloads) > 20 {
			b.downloads = b.downloads[len(b.downloads)-20:]
		}
		b.mu.Unlock()
		b.push()
		return
	case "Browser.downloadProgress":
		var p struct {
			GUID          string
			State         string
			ReceivedBytes float64
			TotalBytes    float64
		}
		json.Unmarshal(m.Params, &p)
		b.mu.Lock()
		changed := false
		for _, d := range b.downloads {
			if d.GUID == p.GUID {
				changed = d.State != p.State || time.Now().UnixMilli()-d.T > 500
				d.State, d.Got, d.Total = p.State, int64(p.ReceivedBytes), int64(p.TotalBytes)
				if changed {
					d.T = time.Now().UnixMilli()
				}
			}
		}
		b.mu.Unlock()
		if changed {
			b.push()
		}
		return
	}
	if m.SessionID == "" {
		return
	}
	b.mu.Lock()
	t := src.sess[m.SessionID]
	b.mu.Unlock()
	if t != nil {
		b.tabEvent(t, m)
	}
}

func (b *browser) attach(src *bsrc, session string, ti cdpTargetInfo, waiting bool) {
	b.mu.Lock()
	t := &btab{src: src, id: ti.TargetID, sess: session, URL: ti.URL, Title: ti.Title, opener: ti.OpenerID, born: time.Now(), netByID: map[string]*bnet{}, ready: make(chan struct{})}
	if vp, ok := b.pendingVP[ti.TargetID]; ok {
		t.vp = vp
		delete(b.pendingVP, ti.TargetID)
	}
	src.sess[session] = t
	b.tabs[t.id] = t
	if !src.mine() {
		src.cur = t
		src.agent.trail.add("tab", map[string]any{"tab": t.id, "url": t.URL})
	}
	if src.mine() {
		at := len(b.order)
		if i := slices.Index(b.order, ti.OpenerID); i >= 0 && ti.OpenerID != "" {
			at = i + 1 // a popup opens beside the tab that opened it
			for at < len(b.order) && b.tabs[b.order[at]] != nil && b.tabs[b.order[at]].opener == ti.OpenerID {
				at++
			}
		}
		b.order = slices.Insert(b.order, at, t.id)
	}
	ua := b.ua
	b.mu.Unlock()
	c := src.cdp
	go func() {
		c.call(session, "Page.enable", nil, nil)
		c.call(session, "Runtime.enable", nil, nil)
		if src.mine() {
			c.call(session, "Network.enable", map[string]any{"maxTotalBufferSize": 4 << 20, "maxResourceBufferSize": 1 << 20}, nil)
		} else { // an agent's: its API calls' bodies are read when they end (browsercalls.go)
			c.call(session, "Network.enable", map[string]any{"maxTotalBufferSize": 64 << 20, "maxResourceBufferSize": 16 << 20}, nil)
			if src.agent.trail != nil { // recorded from here on: the wrapper's open waits for this to load its page
				os.MkdirAll(filepath.Join(b.agentDir(), "attached"), 0o700)
				os.WriteFile(filepath.Join(b.agentDir(), "attached", src.agent.id), nil, 0o600)
			}
		}
		c.call(session, "Log.enable", nil, nil)
		if src.mine() {
			b.window(t)
			c.call(session, "Page.setInterceptFileChooserDialog", map[string]any{"enabled": true}, nil)
			c.call(session, "Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true}, nil)
			if ua != "" {
				c.call(session, "Emulation.setUserAgentOverride", map[string]any{"userAgent": ua}, nil)
			}
			b.emulate(t)
		}
		if waiting {
			c.call(session, "Runtime.runIfWaitingForDebugger", nil, nil)
		}
		close(t.ready)
		if !src.mine() {
			b.measure(t)
		}
		b.history(t)
		b.recast()
		if !src.mine() {
			b.trailLoad(t) // what it already shows: an agent's open loads its page before the scan attaches
		}
	}()
	b.push()
	if src.mine() {
		b.save()
		if ti.OpenerID != "" {
			b.broadcast(map[string]any{"t": "opened", "tab": t.id, "opener": ti.OpenerID})
		}
	}
}

func (b *browser) tabEvent(t *btab, m cdpMsg) {
	switch m.Method {
	case "Page.screencastFrame":
		var p struct {
			Data      []byte `json:"data"`
			SessionID int    `json:"sessionId"`
			Metadata  struct {
				DeviceWidth, DeviceHeight, OffsetTop, PageScaleFactor float64
			} `json:"metadata"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			return
		}
		b.frame(t, p.Data, p.SessionID, p.Metadata.DeviceWidth, p.Metadata.DeviceHeight, p.Metadata.OffsetTop)
	case "Page.frameStartedLoading", "Page.frameStoppedLoading":
		var p struct{ FrameID string }
		json.Unmarshal(m.Params, &p)
		if p.FrameID != t.id {
			return
		}
		b.mu.Lock()
		t.Loading = m.Method == "Page.frameStartedLoading"
		b.mu.Unlock()
		b.push()
		if !t.Loading {
			go b.iconOf(t)
			go b.history(t)
			if !t.src.mine() {
				go b.trailLoad(t)
			}
		}
	case "Page.frameNavigated":
		var p struct {
			Frame struct{ ID, ParentID, URL string }
		}
		json.Unmarshal(m.Params, &p)
		if p.Frame.ParentID != "" {
			return
		}
		b.mu.Lock()
		t.nE, t.nW, t.nN, t.Crashed = 0, 0, 0, false
		t.devSeq++
		t.castDone = "" // a navigation to another process ends the screencast: start it again
		if !t.src.mine() {
			t.src.cur = t
		}
		b.mu.Unlock()
		if !t.src.mine() {
			t.src.agent.trail.add("nav", map[string]any{"tab": t.id, "url": trunc(p.Frame.URL, 2000)})
		}
		go b.history(t)
		go b.recast()
	case "Page.navigatedWithinDocument":
		go b.history(t)
	case "Page.frameResized":
		if !t.src.mine() {
			go b.measure(t)
		}
	case "Page.javascriptDialogOpening":
		var d bdialog
		json.Unmarshal(m.Params, &d)
		d.Message, d.Default, d.URL = trunc(d.Message, 2000), trunc(d.Default, 2000), trunc(d.URL, 500)
		b.mu.Lock()
		t.dialog = &d
		b.mu.Unlock()
		b.push()
	case "Page.javascriptDialogClosed":
		b.mu.Lock()
		t.dialog = nil
		b.mu.Unlock()
		b.push()
	case "Page.fileChooserOpened":
		var p struct {
			Mode          string
			BackendNodeID int
		}
		json.Unmarshal(m.Params, &p)
		b.mu.Lock()
		t.chooser = &bchooser{Mode: p.Mode, Backend: p.BackendNodeID}
		b.mu.Unlock()
		b.push()
	case "Runtime.consoleAPICalled":
		var p struct {
			Type       string
			Args       []cdpRemote
			StackTrace *cdpStack
		}
		json.Unmarshal(m.Params, &p)
		parts := make([]string, 0, len(p.Args))
		for _, a := range p.Args {
			parts = append(parts, a.text())
		}
		lvl := map[string]string{"error": "error", "assert": "error", "warning": "warning", "info": "info", "debug": "debug"}[p.Type]
		pr := bprob{Kind: "console", Level: cmp.Or(lvl, "log"), Text: trunc(strings.Join(parts, " "), 4000), Src: p.StackTrace.top()}
		if lvl == "error" {
			pr.Stack = p.StackTrace.String()
		}
		b.addDev(t, pr, lvl == "error" || lvl == "warning", true)
	case "Runtime.exceptionThrown":
		var p struct {
			ExceptionDetails struct {
				Text       string
				URL        string
				LineNumber int
				Exception  *cdpRemote
				StackTrace *cdpStack
			}
		}
		json.Unmarshal(m.Params, &p)
		d := p.ExceptionDetails
		text := d.Text
		if d.Exception != nil && d.Exception.Description != "" {
			text = d.Exception.Description
		}
		src := d.StackTrace.top()
		if src == "" && d.URL != "" {
			src = d.URL + ":" + strconv.Itoa(d.LineNumber+1)
		}
		b.addDev(t, bprob{Kind: "exception", Level: "error", Text: trunc(text, 4000), Src: trunc(src, 600), Stack: d.StackTrace.String()}, true, true)
	case "Log.entryAdded":
		var p struct {
			Entry struct {
				Source, Level, Text, URL string
				LineNumber               int
			}
		}
		json.Unmarshal(m.Params, &p)
		e := p.Entry
		if e.Level != "error" && e.Level != "warning" {
			return
		}
		if e.Source == "network" && strings.Contains(e.Text, "status of") { // the failed response is in the network list already
			return
		}
		src := e.URL
		if src != "" && e.LineNumber > 0 {
			src += ":" + strconv.Itoa(e.LineNumber+1)
		}
		b.addDev(t, bprob{Kind: "log", Level: e.Level, Text: trunc(e.Text, 4000), Src: trunc(src, 600)}, true, true)
	case "Network.requestWillBeSent":
		var p struct {
			RequestID string
			Request   struct {
				URL, Method string
				Headers     map[string]any
				PostData    string
				HasPostData bool
			}
			Type      string
			Timestamp float64
		}
		json.Unmarshal(m.Params, &p)
		if strings.HasPrefix(p.Request.URL, "data:") {
			return
		}
		b.mu.Lock()
		n := &bnet{ID: p.RequestID, T: time.Now().UnixMilli(), Method: p.Request.Method, URL: trunc(p.Request.URL, 2000), Type: p.Type, ts: p.Timestamp}
		if !t.src.mine() && (p.Type == "XHR" || p.Type == "Fetch") {
			n.api, n.reqH, n.post, n.hasPost = true, brRedactHeaders(p.Request.Headers), p.Request.PostData, p.Request.HasPostData
		}
		if old := t.netByID[p.RequestID]; old != nil { // a redirect: the same id goes on
			old.Done = true
		}
		t.netByID[p.RequestID] = n
		t.devSeq++
		n.Seq = t.devSeq
		t.nets = append(t.nets, n)
		if len(t.nets) > 2*brNetKept {
			for _, x := range t.nets[:len(t.nets)-brNetKept] {
				if t.netByID[x.ID] == x {
					delete(t.netByID, x.ID)
				}
			}
			t.nets = slices.Clone(t.nets[len(t.nets)-brNetKept:])
		}
		b.mu.Unlock()
		b.devKick(t)
	case "Network.responseReceived":
		var p struct {
			RequestID string
			Type      string
			Response  struct {
				Status   int
				MimeType string
				URL      string
				Headers  map[string]any
			}
		}
		json.Unmarshal(m.Params, &p)
		b.mu.Lock()
		n := t.netByID[p.RequestID]
		if n != nil {
			n.Status, n.Mime = p.Response.Status, p.Response.MimeType
			if n.api {
				n.resH = brRedactHeaders(p.Response.Headers)
			}
			t.devSeq++
			n.Seq = t.devSeq
		}
		b.mu.Unlock()
		if n != nil && p.Response.Status >= 400 {
			b.addDev(t, bprob{Kind: "http", Level: "error", Text: fmt.Sprintf("%d %s %s", p.Response.Status, n.Method, trunc(p.Response.URL, 500)), Src: p.Type}, true, false)
		} else {
			b.devKick(t)
		}
	case "Network.loadingFinished", "Network.loadingFailed":
		var p struct {
			RequestID         string
			Timestamp         float64
			EncodedDataLength float64
			ErrorText         string
			Canceled          bool
			BlockedReason     string
		}
		json.Unmarshal(m.Params, &p)
		b.mu.Lock()
		n := t.netByID[p.RequestID]
		var failed string
		var call *bnet
		if n != nil {
			n.Done = true
			n.Size = int64(p.EncodedDataLength)
			if n.ts > 0 && p.Timestamp > 0 {
				n.MS = math.Round((p.Timestamp-n.ts)*10000) / 10
			}
			if m.Method == "Network.loadingFailed" {
				n.Err = cmp.Or(p.BlockedReason, p.ErrorText)
				if !p.Canceled {
					failed = fmt.Sprintf("%s %s: %s", n.Method, n.URL, n.Err)
				}
			}
			t.devSeq++
			n.Seq = t.devSeq
			if n.api && !p.Canceled {
				c := *n
				call = &c
			}
		}
		b.mu.Unlock()
		if call != nil {
			go b.trailCall(t, *call)
		}
		if failed != "" {
			b.addDev(t, bprob{Kind: "net", Level: "error", Text: trunc(failed, 1000)}, true, false)
		} else {
			b.devKick(t)
		}
	case "Inspector.targetCrashed":
		b.mu.Lock()
		t.Crashed, t.casting = true, ""
		b.mu.Unlock()
		b.push()
	}
}

type cdpRemote struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	Value       json.RawMessage `json:"value"`
	Description string          `json:"description"`
	Preview     *struct {
		Properties []struct{ Name, Value, Type string }
		Overflow   bool
		Subtype    string
	} `json:"preview"`
}

// text: a console argument as DevTools would print it, short.
func (r cdpRemote) text() string {
	if r.Type == "string" {
		var s string
		json.Unmarshal(r.Value, &s)
		return s
	}
	if len(r.Value) > 0 && r.Type != "object" {
		return string(r.Value)
	}
	if r.Preview != nil && r.Subtype != "error" {
		var parts []string
		for _, p := range r.Preview.Properties {
			v := p.Value
			if p.Type == "string" {
				v = strconv.Quote(v)
			}
			if r.Preview.Subtype == "array" {
				parts = append(parts, v)
			} else {
				parts = append(parts, p.Name+": "+v)
			}
		}
		if r.Preview.Overflow {
			parts = append(parts, "…")
		}
		if r.Preview.Subtype == "array" {
			return "[" + strings.Join(parts, ", ") + "]"
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	if r.Description != "" {
		return r.Description
	}
	return r.Type
}

type cdpStack struct {
	CallFrames []struct {
		FunctionName string
		URL          string
		LineNumber   int
		ColumnNumber int
	}
}

func (s *cdpStack) top() string {
	if s == nil || len(s.CallFrames) == 0 {
		return ""
	}
	f := s.CallFrames[0]
	return trunc(f.URL, 500) + ":" + strconv.Itoa(f.LineNumber+1) + ":" + strconv.Itoa(f.ColumnNumber+1)
}

func (s *cdpStack) String() string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	for i, f := range s.CallFrames {
		if i == 12 {
			b.WriteString("    …\n")
			break
		}
		fmt.Fprintf(&b, "    at %s (%s:%d:%d)\n", trunc(cmp.Or(f.FunctionName, "(anonymous)"), 200), trunc(f.URL, 500), f.LineNumber+1, f.ColumnNumber+1)
	}
	return trunc(strings.TrimRight(b.String(), "\n"), 4000)
}

// addDev keeps a console line (cons), a problem (probs), or both, and counts it on the tab's badge.
func (b *browser) addDev(t *btab, p bprob, problem, console bool) {
	b.mu.Lock()
	t.devSeq++
	p.Seq, p.T = t.devSeq, time.Now().UnixMilli()
	if console { // trimmed in batches: a page logging in a loop mustn't cost a copy per line
		t.cons = append(t.cons, p)
		if len(t.cons) > 2*brConsoleKept {
			t.cons = slices.Clone(t.cons[len(t.cons)-brConsoleKept:])
		}
	}
	if problem {
		t.probs = append(t.probs, p)
		if len(t.probs) > 2*brProblemsKept {
			t.probs = slices.Clone(t.probs[len(t.probs)-brProblemsKept:])
		}
		switch {
		case p.Kind == "http" || p.Kind == "net":
			t.nN++
		case p.Level == "error":
			t.nE++
		default:
			t.nW++
		}
	}
	b.mu.Unlock()
	if problem {
		b.push()
		if !t.src.mine() {
			k := "con"
			if p.Kind == "http" || p.Kind == "net" {
				k = "net"
			}
			t.src.agent.trail.add(k, map[string]any{"tab": t.id, "level": p.Level, "text": trunc(p.Text, 600), "src": p.Src})
		}
	}
	b.devKick(t)
}

const brIconJS = `(() => { const l = [...document.querySelectorAll('link[rel~="icon"], link[rel="shortcut icon"], link[rel="apple-touch-icon"]')];
  const best = l.find((x) => /svg/.test(x.type || x.href)) || l.find((x) => /^(16|32)/.test(x.sizes?.value || '')) || l[0];
  return best ? best.href : (location.protocol.startsWith('http') ? location.origin + '/favicon.ico' : ''); })()`

func (b *browser) iconOf(t *btab) {
	var r struct{ Result cdpRemote }
	if t.src.cdp.callT(5*time.Second, t.sess, "Runtime.evaluate", map[string]any{"expression": brIconJS, "returnByValue": true}, &r) != nil {
		return
	}
	icon := r.Result.text()
	if strings.HasPrefix(icon, "data:") && len(icon) > 8000 || !strings.HasPrefix(icon, "http") && !strings.HasPrefix(icon, "data:image/") {
		icon = ""
	}
	b.mu.Lock()
	changed := icon != t.Icon
	t.Icon = icon
	b.mu.Unlock()
	if changed {
		b.push()
	}
}

func (b *browser) history(t *btab) {
	var h struct {
		CurrentIndex int
		Entries      []struct {
			ID         int
			URL, Title string
		}
	}
	if t.src.cdp.callT(5*time.Second, t.sess, "Page.getNavigationHistory", nil, &h) != nil || h.CurrentIndex >= len(h.Entries) {
		return
	}
	cur := h.Entries[h.CurrentIndex]
	cur.URL, cur.Title = trunc(cur.URL, 2000), trunc(cur.Title, 300) // a page's own strings: bounded
	b.mu.Lock()
	back, fwd := h.CurrentIndex > 0, h.CurrentIndex < len(h.Entries)-1
	changed := back != t.Back || fwd != t.Fwd || (cur.Title != "" && cur.Title != t.Title) || (cur.URL != "" && cur.URL != t.URL)
	t.Back, t.Fwd = back, fwd
	if cur.Title != "" { // headless Chromium doesn't report a title as it changes: it's read here
		t.Title = cur.Title
	}
	if cur.URL != "" {
		t.URL = cur.URL
	}
	mine := t.src.mine()
	b.mu.Unlock()
	if changed {
		b.push()
		if mine {
			b.save()
		}
	}
}

// window: the tab's window, and how much bigger it is than its page (the headless browser's own frame)
func (b *browser) window(t *btab) {
	var w struct {
		WindowID int `json:"windowId"`
	}
	if t.src.cdp.call("", "Browser.getWindowForTarget", map[string]any{"targetId": t.id}, &w) != nil {
		return
	}
	var r struct{ Result cdpRemote }
	t.src.cdp.call(t.sess, "Runtime.evaluate", map[string]any{"expression": "[outerWidth - innerWidth, outerHeight - innerHeight]", "returnByValue": true}, &r)
	var ui [2]int
	json.Unmarshal(r.Result.Value, &ui)
	b.mu.Lock()
	t.win, t.uiW, t.uiH = w.WindowID, max(ui[0], 0), max(ui[1], 0)
	b.mu.Unlock()
}

// measure: an agent's page's viewport (Playwright set it; the panel doesn't emulate those).
func (b *browser) measure(t *btab) {
	var lm struct {
		CSSLayoutViewport struct{ ClientWidth, ClientHeight float64 } `json:"cssLayoutViewport"`
	}
	if t.src.cdp.callT(5*time.Second, t.sess, "Page.getLayoutMetrics", nil, &lm) != nil || lm.CSSLayoutViewport.ClientWidth == 0 {
		return
	}
	b.mu.Lock()
	t.cssW, t.cssH = lm.CSSLayoutViewport.ClientWidth, lm.CSSLayoutViewport.ClientHeight
	b.mu.Unlock()
}

// titles: the tabs on someone's screen are asked for their title every 2 s (a single-page app changes it as you go).
func (b *browser) titles() {
	for range time.Tick(2 * time.Second) {
		b.mu.Lock()
		var ts []*btab
		for v := range b.viewers {
			if t := b.tabs[v.tab]; t != nil && !slices.Contains(ts, t) {
				ts = append(ts, t)
			}
		}
		b.mu.Unlock()
		for _, t := range ts {
			b.history(t)
		}
	}
}

// ---- emulation (the owner's tabs only: an agent's Playwright owns its viewport) ----

var brNets = map[string][3]float64{ // latency ms, down and up in kbit/s (DevTools' presets)
	"fast4g": {165, 9000, 1500},
	"slow4g": {562.5, 1600, 750},
	"3g":     {2000, 400, 400},
}

func (b *browser) emulate(t *btab) {
	t.emuMu.Lock()
	defer t.emuMu.Unlock()
	b.mu.Lock()
	vp, w, h, dpr, mob := t.vp, t.fitW, t.fitH, t.fitDPR, t.fitMob
	if vp.Mode == "device" {
		w, h, dpr, mob = vp.W, vp.H, vp.DPR, vp.Mobile
	}
	if w <= 0 || h <= 0 {
		w, h = 1280, 800
	}
	// always the 2x Chromium renders at (--force-device-scale-factor): an emulated density that differs from it
	// makes Chromium scale input by the wrong factor now and then (a click lands at twice its coordinates). A
	// device's own density (3x, 1x) is shown, not emulated; a 1x screen still gets 1x frames (the stream's maxWidth).
	dpr = 2
	ua := b.ua
	if vp.Mode == "device" && vp.UA != "" {
		ua = vp.UA
	} else if mob { // a phone: sites that choose their mobile version by the user agent (Wikipedia…) send it
		ua = brMobileUA(b.ua)
	}
	key, _ := json.Marshal([]any{w, h, dpr, mob, vp.Scheme, vp.Net, ua})
	if string(key) == t.applied || !t.src.mine() {
		b.mu.Unlock()
		return
	}
	t.applied = string(key)
	t.cssW, t.cssH = float64(w), float64(h)
	win, uiW, uiH := t.win, t.uiW, t.uiH
	b.mu.Unlock()
	c, s := t.src.cdp, t.sess
	if win != 0 { // the window first: the page's view can't be bigger than it
		c.call("", "Browser.setWindowBounds", map[string]any{"windowId": win, "bounds": map[string]any{"windowState": "normal", "width": w + uiW, "height": h + uiH}}, nil)
	}
	c.call(s, "Emulation.setDeviceMetricsOverride", map[string]any{"width": w, "height": h, "deviceScaleFactor": dpr, "mobile": mob,
		"screenWidth": w, "screenHeight": h}, nil)
	c.call(s, "Emulation.setTouchEmulationEnabled", map[string]any{"enabled": mob, "maxTouchPoints": 5}, nil)
	if ua != "" {
		c.call(s, "Emulation.setUserAgentOverride", map[string]any{"userAgent": ua}, nil)
	}
	c.call(s, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-color-scheme", "value": vp.Scheme}}}, nil)
	if n, ok := brNets[vp.Net]; ok {
		c.call(s, "Network.emulateNetworkConditions", map[string]any{"offline": false, "latency": n[0], "downloadThroughput": n[1] * 128, "uploadThroughput": n[2] * 128}, nil)
	} else {
		c.call(s, "Network.emulateNetworkConditions", map[string]any{"offline": vp.Net == "offline", "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1}, nil)
	}
}

// ---- streaming ----

// recast starts, changes or stops each tab's screencast to match who's watching it: full frames for a viewer
// showing it, small ones for the agents' cards, none when nobody looks.
func (b *browser) recast() {
	type job struct {
		t      *btab
		mode   string
		params map[string]any
	}
	var jobs []job
	b.mu.Lock()
	want := map[*btab]string{}
	for v := range b.viewers {
		if t := b.tabs[v.tab]; t != nil {
			want[t] = "full"
		}
		for id := range v.thumbs {
			if t := b.tabs[id]; t != nil && want[t] == "" {
				want[t] = "thumb"
			}
		}
	}
	for _, t := range b.tabs {
		mode := want[t]
		if mode == "" && !t.src.mine() && t.src.cur == t && t.src.agent.trail != nil {
			mode = "record" // its trail's motion
		}
		if t.Crashed {
			mode = ""
		}
		var params map[string]any
		if mode == "full" {
			w, h := t.fitW, t.fitH
			if t.vp.Mode == "device" {
				w, h = t.vp.W, t.vp.H
			}
			if !t.src.mine() { // an agent's: as Playwright set it (1280×800); room for a bigger one
				w, h = 1920, 1200
			}
			params = map[string]any{"format": "jpeg", "quality": brQuality, "maxWidth": cmp.Or(w, 1280), "maxHeight": cmp.Or(h, 800), "everyNthFrame": 1}
		} else if mode == "thumb" {
			params = map[string]any{"format": "jpeg", "quality": 55, "maxWidth": 640, "maxHeight": 400, "everyNthFrame": 2}
		} else if mode == "record" {
			params = map[string]any{"format": "jpeg", "quality": 50, "maxWidth": 1280, "maxHeight": 800, "everyNthFrame": 2}
		}
		key := ""
		if params != nil {
			k, _ := json.Marshal(params)
			key = string(k)
		}
		if key == t.castKey && t.castDone == key {
			continue
		}
		t.casting, t.castKey, t.castWant, t.ackHeld = mode, key, params, nil
		jobs = append(jobs, job{t, mode, params})
	}
	b.mu.Unlock()
	for _, j := range jobs {
		go b.applyCast(j.t)
	}
}

// applyCast makes Chromium's screencast for a tab what's wanted now (read under the tab's lock, so of several
// racing changes the last one wins, whatever order their goroutines run in)
func (b *browser) applyCast(t *btab) {
	t.castMu.Lock()
	defer t.castMu.Unlock()
	b.mu.Lock()
	key, want := t.castKey, t.castWant
	done := t.castDone == key
	b.mu.Unlock()
	if done {
		return
	}
	c, s := t.src.cdp, t.sess
	c.call(s, "Page.stopScreencast", nil, nil)
	if want != nil {
		c.call(s, "Page.startScreencast", want, nil)
	}
	b.mu.Lock()
	t.castDone = key
	b.mu.Unlock()
}

// sharpen sends the settled page at the viewer's density: WebP at 2x, else a fine JPEG at 1x. Dropped if the page
// moved meanwhile (its frames are on their way).
func (b *browser) sharpen(t *btab, whole int64, force ...bool) {
	b.mu.Lock()
	if t.wholeSeq != whole || (t.casting != "full" && len(force) == 0) || t.dialog != nil {
		b.mu.Unlock()
		return
	}
	w, h := t.cssW, t.cssH
	mine := t.src.mine()
	b.mu.Unlock()
	// a capture comes at the page's emulated density (the viewer's, set by emulate): WebP when that's 2x
	params := map[string]any{"format": "jpeg", "quality": 88, "optimizeForSpeed": true}
	format := "jpeg"
	if mine { // the owner's pages are 2x: WebP keeps that small (a 1x screen draws it scaled)
		params, format = map[string]any{"format": "webp", "quality": 82}, "webp"
	}
	var r struct {
		Data []byte `json:"data"`
	}
	if t.src.cdp.callT(4*time.Second, t.sess, "Page.captureScreenshot", params, &r) != nil || len(r.Data) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.wholeSeq != whole { // the page moved on while it was captured: its frames are coming
		return
	}
	t.seq++
	t.stale = false
	meta, _ := json.Marshal(map[string]any{"tab": t.id, "seq": t.seq, "dw": cmp.Or(w, 1), "dh": cmp.Or(h, 1), "fmt": format, "sharp": true})
	f := &bframe{seq: t.seq, img: r.Data, meta: meta}
	t.frame = f
	for v := range b.viewers {
		if v.tab == t.id {
			v.frame, v.patches = f, nil
			v.kick()
		}
	}
}

// ---- viewers: a page's websocket ----

type bview struct {
	b          *browser
	ws         *wsConn
	tab        string          // the tab it shows
	thumbs     map[string]bool // tabs it shows as small cards (the agents' rail and wall)
	thumbAt    map[string]time.Time
	thumbQ     map[string]*bframe
	devTab     string // the tab whose drawer is open
	devMark    int64  // the drawer's entries it has, up to
	frame      *bframe
	patches    []*bframe // to draw over the frame, in order, once it's sent
	sentSeq    int64
	inflight   int
	text       [][]byte
	wake       chan struct{}
	gone       bool
	agent      string // the agent session it watches, for its timeline
	cursorBusy bool   // a cursor question is out: the next waits for its answer
	fieldsBusy bool   // and a question for the page's text fields
	logMark    int
	work       chan func()
	tok, email string // its session: checked again every 25 s (a lock or an expiry ends the socket)
}

// queue adds a text message for the viewer (b.mu held). A viewer far behind (thousands queued) keeps the newest:
// the next state push replaces what was dropped, and its drawer starts again from what's kept.
func (v *bview) queue(raw []byte) {
	v.text = append(v.text, raw)
	if len(v.text) > 3000 {
		v.text = slices.Clone(v.text[len(v.text)-500:])
		v.devMark = 0
	}
}

// do runs f on the viewer's worker (in order); if its queue is full, here
func (v *bview) do(f func()) {
	select {
	case v.work <- f:
	default:
		f()
	}
}

func (v *bview) kick() {
	select {
	case v.wake <- struct{}{}:
	default:
	}
}

func (b *browser) serveWS(w http.ResponseWriter, r *http.Request) {
	ws, err := wsUpgrade(w, r, 8<<20)
	if err != nil {
		return
	}
	v := &bview{b: b, ws: ws, thumbs: map[string]bool{}, thumbAt: map[string]time.Time{}, thumbQ: map[string]*bframe{}, wake: make(chan struct{}, 1),
		work: make(chan func(), 256), tok: sessionToken(r), email: identity(r)}
	b.mu.Lock()
	b.viewers[v] = true
	b.mu.Unlock()
	go b.ensureMe() // (the agents' browsers: main.go scans for them, only in the real panel: not a test's)
	go v.writer()
	go func() { // what waits on Chromium (navigating, emulating, dialogs…), in order, off the read loop: acks keep flowing
		for f := range v.work {
			f()
		}
	}()
	defer close(v.work)
	v.send(b.state())
	defer func() {
		b.mu.Lock()
		delete(b.viewers, v)
		v.gone = true
		b.mu.Unlock()
		v.kick()
		ws.close()
		b.recast()
	}()
	for {
		op, p, err := ws.read()
		if err != nil {
			return
		}
		if op != wsText {
			continue
		}
		var m bmsg
		if json.Unmarshal(p, &m) != nil {
			continue
		}
		b.handle(v, m)
	}
}

// writer sends a viewer what's queued: text in order, then the newest frame when it has room for one.
func (v *bview) writer() {
	b := v.b
	keep := time.NewTicker(25 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-v.wake:
		case <-keep.C:
			if b.s != nil && !b.s.auth.peekSession(v.tok, v.email) { // locked, expired or revoked: this socket goes too
				v.ws.close()
				return
			}
			v.ws.write(wsPing, nil)
			continue
		}
		for {
			b.mu.Lock()
			if v.gone {
				b.mu.Unlock()
				return
			}
			text := v.text
			v.text = nil
			var f *bframe
			waiting := v.frame != nil && v.frame.seq != v.sentSeq
			if waiting && v.inflight < brFramesInFlight {
				f = v.frame
				v.sentSeq = f.seq
				v.inflight++
				waiting = false
			}
			var patches []*bframe
			if !waiting { // a patch draws over the frame before it: never ahead of it
				patches, v.patches = v.patches, nil
			}
			var thumbs []*bframe
			for id, tf := range v.thumbQ {
				thumbs = append(thumbs, tf)
				v.thumbAt[id] = time.Now()
				delete(v.thumbQ, id)
			}
			b.mu.Unlock()
			if len(text) == 0 && f == nil && len(thumbs) == 0 && len(patches) == 0 {
				break
			}
			for _, t := range text {
				if v.ws.write(wsText, t) != nil {
					return
				}
			}
			if f != nil && v.ws.write(wsBinary, frameMsg(1, f)) != nil {
				return
			}
			for _, p := range patches {
				if v.ws.write(wsBinary, frameMsg(4, p)) != nil {
					return
				}
			}
			for _, tf := range thumbs {
				if v.ws.write(wsBinary, frameMsg(3, tf)) != nil {
					return
				}
			}
		}
	}
}

// frameMsg: a binary message is a kind byte (1 a frame, 2 a still for annotating, 3 a card's frame, 4 a patch), the
// metadata's length (2 bytes) and JSON, then the image.
func frameMsg(kind byte, f *bframe) []byte {
	if len(f.meta) > 65535 { // its length is two bytes: never true with the bounds above, but never a wrapped length
		f = &bframe{seq: f.seq, img: f.img, meta: []byte(`{"bad":true}`)}
	}
	out := make([]byte, 0, 3+len(f.meta)+len(f.img))
	out = append(out, kind)
	out = binary.BigEndian.AppendUint16(out, uint16(len(f.meta)))
	out = append(out, f.meta...)
	return append(out, f.img...)
}

func (v *bview) send(x any) {
	raw, err := json.Marshal(x)
	if err != nil {
		return
	}
	v.b.mu.Lock()
	v.queue(raw)
	v.b.mu.Unlock()
	v.kick()
}

func (b *browser) broadcast(x any) {
	raw, err := json.Marshal(x)
	if err != nil {
		return
	}
	b.mu.Lock()
	for v := range b.viewers {
		v.queue(raw)
		v.kick()
	}
	b.mu.Unlock()
}

// push sends every viewer the tabs and sessions, at most ten times a second.
func (b *browser) push() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pushTimer != nil {
		return
	}
	b.pushTimer = time.AfterFunc(100*time.Millisecond, func() {
		b.mu.Lock()
		b.pushTimer = nil
		b.mu.Unlock()
		st := b.state()
		raw, _ := json.Marshal(st)
		b.mu.Lock()
		for v := range b.viewers {
			v.queue(raw)
			v.kick()
		}
		b.mu.Unlock()
	})
}

type btabJSON struct {
	ID      string    `json:"id"`
	Src     string    `json:"src"`
	URL     string    `json:"url"`
	Title   string    `json:"title"`
	Icon    string    `json:"icon,omitempty"`
	Loading bool      `json:"loading,omitempty"`
	Back    bool      `json:"back,omitempty"`
	Fwd     bool      `json:"fwd,omitempty"`
	Crashed bool      `json:"crashed,omitempty"`
	Probs   [3]int    `json:"probs"`
	VP      bvp       `json:"vp"`
	Dialog  *bdialog  `json:"dialog,omitempty"`
	Chooser *bchooser `json:"chooser,omitempty"`
}

func (b *browser) state() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	tj := func(t *btab) btabJSON {
		return btabJSON{ID: t.id, Src: t.src.id, URL: t.URL, Title: t.Title, Icon: t.Icon, Loading: t.Loading, Back: t.Back, Fwd: t.Fwd, Crashed: t.Crashed,
			Probs: [3]int{t.nE, t.nW, t.nN}, VP: t.vp, Dialog: t.dialog, Chooser: t.chooser}
	}
	tabs := []btabJSON{}
	for _, id := range b.order {
		if t := b.tabs[id]; t != nil {
			tabs = append(tabs, tj(t))
		}
	}
	return map[string]any{"t": "state", "tabs": tabs, "agents": b.agentsJSON(tj), "dtv": b.version, "starting": b.starting || (b.me == nil && b.startErr == ""),
		"error": b.startErr, "downloads": b.downloads}
}

// ---- what a viewer asks ----

type bmsg struct {
	T     string   `json:"t"`
	Tab   string   `json:"tab"`
	URL   string   `json:"url"`
	Seq   int64    `json:"seq"`
	W     int      `json:"w"`
	H     int      `json:"h"`
	DPR   float64  `json:"dpr"`
	Mob   bool     `json:"mob"`
	X     float64  `json:"x"`
	Y     float64  `json:"y"`
	DX    float64  `json:"dx"`
	DY    float64  `json:"dy"`
	Type  string   `json:"type"`
	Btn   string   `json:"btn"`
	Btns  int      `json:"btns"`
	N     int      `json:"n"`
	Mods  int      `json:"mods"`
	Key   string   `json:"key"`
	Code  string   `json:"code"`
	KC    int      `json:"kc"`
	Text  string   `json:"text"`
	On    bool     `json:"on"`
	Ok    bool     `json:"ok"`
	To    int      `json:"to"`
	IDs   []string `json:"ids"`
	Paths []string `json:"paths"`
	Names []string `json:"names"`
	VP    *bvp     `json:"vp"`
	Pts   []struct {
		X, Y float64
		ID   int
	} `json:"pts"`
	Agent string `json:"agent"`
	Hard  bool   `json:"hard"`
	Q     int    `json:"q"`
	Keep  bool   `json:"keep"`
}

func (b *browser) tab(id string) *btab {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tabs[id]
}

// mine: a tab the viewer may drive. The owner's, always; an agent's only while the owner holds it (Take over).
func (b *browser) mine(id string) *btab {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.tabs[id]
	if t == nil || (!t.src.mine() && (t.src.agent == nil || !t.src.agent.held)) {
		return nil
	}
	return t
}

var brMouse = map[string]string{"down": "mousePressed", "up": "mouseReleased", "move": "mouseMoved"}

// handle: acks, input and bookkeeping here, at once; anything that waits on Chromium on the viewer's worker
func (b *browser) handle(v *bview, m bmsg) {
	switch m.T {
	case "watch", "ack", "thumbs", "mouse", "wheel", "touch", "key", "text", "dev", "clear", "eval", "shot", "probe", "area", "cursor", "fields", "find", "agent", "hold":
		b.handle1(v, m)
	default:
		v.do(func() { b.handle1(v, m) })
	}
}

func (b *browser) handle1(v *bview, m bmsg) {
	switch m.T {
	case "watch": // which tab this viewer shows, and how big its view is
		b.mu.Lock()
		v.tab = m.Tab
		if v.frame != nil && v.frame.seq > 0 && (b.tabs[m.Tab] == nil || b.tabs[m.Tab].frame != v.frame) {
			v.frame = nil
		}
		v.inflight = 0
		t := b.tabs[m.Tab]
		v.patches = nil
		stale, seq := false, int64(0)
		if t != nil && t.frame != nil {
			v.frame, v.sentSeq = t.frame, 0 // the last frame at once: the tab shows before it next paints
			stale, seq = t.stale, t.wholeSeq
		}
		if t != nil && m.W > 0 && m.H > 0 && t.src.mine() {
			t.fitW, t.fitH, t.fitDPR, t.fitMob = min(m.W, 3840), min(m.H, 2160), m.DPR, m.Mob
		}
		b.mu.Unlock()
		v.kick()
		b.pokeAck(t)
		v.do(func() {
			if t != nil {
				b.emulate(t)
			}
			b.recast()
			if stale { // patches went on after that frame: a fresh one
				b.sharpen(t, seq, true)
			}
		})
	case "thumbs": // the agents' cards on screen
		b.mu.Lock()
		v.thumbs = map[string]bool{}
		for _, id := range m.IDs {
			v.thumbs[id] = true
		}
		b.mu.Unlock()
		v.do(b.recast)
	case "ack": // a frame was drawn
		b.mu.Lock()
		v.inflight = max(0, v.inflight-1)
		t := b.tabs[v.tab]
		b.mu.Unlock()
		v.kick()
		b.pokeAck(t)
	case "new":
		b.ensureMe()
		b.mu.Lock()
		me := b.me
		b.mu.Unlock()
		if me == nil {
			return
		}
		u := brURL(m.URL)
		var r struct{ TargetID string }
		if me.cdp.call("", "Target.createTarget", map[string]any{"url": cmp.Or(u, "about:blank"), "newWindow": true}, &r) == nil {
			v.send(map[string]any{"t": "created", "tab": r.TargetID})
		}
	case "close":
		if t := b.tab(m.Tab); t != nil && t.src.mine() {
			t.src.cdp.call("", "Target.closeTarget", map[string]any{"targetId": t.id}, nil)
		}
	case "move": // the strip, reordered by a drag
		b.mu.Lock()
		if i := slices.Index(b.order, m.Tab); i >= 0 && m.To >= 0 && m.To < len(b.order) {
			b.order = slices.Insert(slices.Delete(b.order, i, i+1), m.To, m.Tab)
		}
		b.mu.Unlock()
		b.push()
		b.save()
	case "nav":
		if t := b.mine(m.Tab); t != nil {
			if u := brURL(m.URL); u != "" {
				var r struct{ ErrorText string }
				t.src.cdp.call(t.sess, "Page.navigate", map[string]any{"url": u}, &r)
				if r.ErrorText != "" && r.ErrorText != "net::ERR_ABORTED" {
					v.send(map[string]any{"t": "toast", "text": "Couldn't open " + trunc(u, 120) + ": " + r.ErrorText})
				}
			}
		}
	case "back", "fwd":
		if t := b.mine(m.Tab); t != nil {
			var h struct {
				CurrentIndex int
				Entries      []struct{ ID int }
			}
			if t.src.cdp.call(t.sess, "Page.getNavigationHistory", nil, &h) == nil {
				i := h.CurrentIndex - 1
				if m.T == "fwd" {
					i = h.CurrentIndex + 1
				}
				if i >= 0 && i < len(h.Entries) {
					t.src.cdp.call(t.sess, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[i].ID}, nil)
				}
			}
		}
	case "reload":
		if t := b.mine(m.Tab); t != nil {
			if t.Crashed {
				b.mu.Lock()
				t.Crashed, t.castDone = false, ""
				b.mu.Unlock()
				b.recast()
			}
			t.src.cdp.call(t.sess, "Page.reload", map[string]any{"ignoreCache": m.Hard}, nil)
		}
	case "stop":
		if t := b.mine(m.Tab); t != nil {
			t.src.cdp.call(t.sess, "Page.stopLoading", nil, nil)
		}
	case "mouse":
		if t := b.mine(m.Tab); t != nil {
			typ := brMouse[m.Type]
			if typ == "" {
				return
			}
			p := map[string]any{"type": typ, "x": m.X, "y": m.Y, "modifiers": m.Mods, "buttons": m.Btns, "pointerType": "mouse"}
			if m.Type != "move" {
				p["button"] = cmp.Or(m.Btn, "left")
				p["clickCount"] = max(m.N, 1)
			} else if m.Btns&1 != 0 {
				p["button"] = "left"
			}
			t.src.cdp.fire(t.sess, "Input.dispatchMouseEvent", p)
		}
	case "wheel":
		if t := b.mine(m.Tab); t != nil {
			t.src.cdp.fire(t.sess, "Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": m.X, "y": m.Y, "deltaX": m.DX, "deltaY": m.DY, "modifiers": m.Mods, "pointerType": "mouse"})
		}
	case "touch":
		if t := b.mine(m.Tab); t != nil {
			typ := map[string]string{"start": "touchStart", "move": "touchMove", "end": "touchEnd", "cancel": "touchCancel"}[m.Type]
			if typ == "" {
				return
			}
			pts := []map[string]any{}
			for _, p := range m.Pts {
				pts = append(pts, map[string]any{"x": p.X, "y": p.Y, "id": p.ID, "radiusX": 4, "radiusY": 4, "force": 1})
			}
			t.src.cdp.fire(t.sess, "Input.dispatchTouchEvent", map[string]any{"type": typ, "touchPoints": pts, "modifiers": m.Mods})
		}
	case "key":
		if t := b.mine(m.Tab); t != nil {
			p := map[string]any{"type": m.Type, "modifiers": m.Mods, "key": m.Key, "code": m.Code, "windowsVirtualKeyCode": m.KC, "nativeVirtualKeyCode": m.KC}
			if m.Text != "" {
				p["text"], p["unmodifiedText"] = m.Text, m.Text
			}
			if cmds := brEditCommands(m); cmds != nil {
				p["commands"] = cmds
			}
			t.src.cdp.fire(t.sess, "Input.dispatchKeyEvent", p)
		}
	case "text": // typed through the phone's keyboard, an IME, or pasted
		if t := b.mine(m.Tab); t != nil && m.Text != "" {
			t.src.cdp.fire(t.sess, "Input.insertText", map[string]any{"text": m.Text})
		}
	case "copy": // what's selected in the page, for the viewer's clipboard
		if t := b.tab(m.Tab); t != nil {
			var r struct{ Result cdpRemote }
			t.src.cdp.call(t.sess, "Runtime.evaluate", map[string]any{"expression": brSelectionJS, "returnByValue": true}, &r)
			v.send(map[string]any{"t": "clip", "text": r.Result.text()})
		}
	case "viewport":
		if t := b.tab(m.Tab); t != nil && m.VP != nil && t.src.mine() {
			b.mu.Lock()
			t.vp = *m.VP
			t.applied = ""
			b.mu.Unlock()
			b.emulate(t)
			b.recast()
			b.push()
			b.save()
		}
	case "dialog":
		if t := b.mine(m.Tab); t != nil {
			t.src.cdp.call(t.sess, "Page.handleJavaScriptDialog", map[string]any{"accept": m.Ok, "promptText": m.Text}, nil)
			b.mu.Lock()
			t.dialog = nil
			b.mu.Unlock()
			b.push()
		}
	case "files": // a file chooser, answered with uploads (paths in the panel's upload folder)
		if t := b.mine(m.Tab); t != nil {
			b.mu.Lock()
			ch := t.chooser
			t.chooser = nil
			b.mu.Unlock()
			if ch != nil && m.Ok {
				var paths []string
				for i, p := range m.Paths {
					c := filepath.Clean(p)
					if b.upDir == "" || !strings.HasPrefix(c, b.upDir+"/") {
						continue
					}
					// the page sees the file's own name: a link to the upload, in a folder of its own
					if i < len(m.Names) {
						if name := filepath.Base(filepath.Clean("/" + m.Names[i])); name != "/" && name != "." {
							if d, err := os.MkdirTemp(b.dir, "upload-"); err == nil {
								if os.Link(c, filepath.Join(d, name)) == nil {
									c = filepath.Join(d, name)
								}
							}
						}
					}
					paths = append(paths, c)
				}
				t.src.cdp.call(t.sess, "DOM.setFileInputFiles", map[string]any{"files": paths, "backendNodeId": ch.Backend}, nil)
			}
			b.push()
		}
	case "dev": // the drawer opened (or closed) on a tab
		b.mu.Lock()
		v.devTab, v.devMark = "", 0
		if m.On {
			v.devTab = m.Tab
		}
		t := b.tabs[m.Tab]
		b.mu.Unlock()
		if t != nil && m.On {
			b.devKick(t)
		}
	case "clear": // the drawer's Clear: the badge starts again
		b.mu.Lock()
		if t := b.tabs[m.Tab]; t != nil {
			t.probs, t.cons, t.nE, t.nW, t.nN = nil, nil, 0, 0, 0
			t.nets, t.netByID = nil, map[string]*bnet{}
			t.devSeq++
		}
		for x := range b.viewers {
			if x.devTab == m.Tab {
				x.devMark = 0
			}
		}
		b.mu.Unlock()
		b.push()
		if t := b.tab(m.Tab); t != nil {
			b.devKick(t)
		}
	case "eval": // the console's prompt
		if t := b.tab(m.Tab); t != nil {
			go b.eval(v, t, m.Text)
		}
	case "shot": // a still, sharp: a fresh capture at the tab's full density (q < 0: only where the page is)
		if t := b.tab(m.Tab); t != nil {
			go b.still(v, t, m.Q, m.Keep)
		}
	case "probe": // what element is at a point (an annotation's pin)
		if t := b.tab(m.Tab); t != nil {
			go b.probe(v, t, m.X, m.Y, m.Seq)
		}
	case "area": // what a drawn mark encloses (a circle, a box)
		if t := b.tab(m.Tab); t != nil {
			go b.area(v, t, m.X, m.Y, m.W, m.H, m.Seq)
		}
	case "dtsel": // the element DevTools selected, to send to an agent
		if t := b.mine(m.Tab); t != nil {
			go b.dtSelected(v, t)
		}
	case "dtreveal": // right-click → Inspect: the element there, in DevTools' Elements
		if t := b.mine(m.Tab); t != nil {
			go b.dtReveal(v, t, m.X, m.Y)
		}
	case "dtpick": // ⌘⇧C: Chromium's picker, for DevTools
		if t := b.mine(m.Tab); t != nil {
			go b.dtPick(t)
		}
	case "cursor": // what's under the pointer (headless Chromium doesn't say what cursor it would show)
		if t := b.tab(m.Tab); t != nil {
			b.mu.Lock()
			busy := v.cursorBusy
			v.cursorBusy = true
			b.mu.Unlock()
			if !busy {
				go func() {
					var r struct{ Result cdpRemote }
					t.src.cdp.callT(2*time.Second, t.sess, "Runtime.evaluate", map[string]any{"expression": fmt.Sprintf(brCursorJS, m.X, m.Y), "returnByValue": true}, &r)
					b.mu.Lock()
					v.cursorBusy = false
					b.mu.Unlock()
					v.send(map[string]any{"t": "cursor", "tab": t.id, "c": r.Result.text(), "seq": m.Seq})
				}()
			}
		}
	case "fields": // where the page's text fields are: a phone opens its keyboard on a tap only inside the tap itself, too
		// soon to ask the VM then, so the page keeps this map and looks the tap up in it
		if t := b.tab(m.Tab); t != nil {
			b.mu.Lock()
			busy := v.fieldsBusy
			v.fieldsBusy = true
			b.mu.Unlock()
			if !busy {
				go func() {
					var r struct {
						Result struct{ Value json.RawMessage }
					}
					t.src.cdp.callT(2*time.Second, t.sess, "Runtime.evaluate", map[string]any{"expression": brFieldsJS, "returnByValue": true}, &r)
					b.mu.Lock()
					v.fieldsBusy = false
					b.mu.Unlock()
					if len(r.Result.Value) > 0 {
						v.send(map[string]any{"t": "fields", "tab": t.id, "r": r.Result.Value})
					}
				}()
			}
		}
	case "find": // find in the page: the page's own find selects the next match and scrolls to it
		if t := b.mine(m.Tab); t != nil {
			q, _ := json.Marshal(m.Text)
			go func() {
				var r struct{ Result cdpRemote }
				t.src.cdp.callT(5*time.Second, t.sess, "Runtime.evaluate", map[string]any{"expression": fmt.Sprintf(brFindJS, q, m.DX < 0, m.N == 0), "returnByValue": true}, &r)
				var res struct {
					Ok bool `json:"ok"`
					N  int  `json:"n"`
				}
				json.Unmarshal(r.Result.Value, &res)
				v.send(map[string]any{"t": "found", "tab": t.id, "ok": res.Ok, "n": res.N})
			}()
		}
	case "agent": // the agent session this viewer watches: its timeline comes with it
		b.mu.Lock()
		v.agent, v.logMark = m.Agent, 0
		b.mu.Unlock()
		b.agentLogKick()
	case "hold": // Take over / Hand back an agent's browser
		b.hold(m.Agent, m.On)
	}
}

// pokeAck: a viewer made room; if Chromium waits on an ack for this tab, give it.
func (b *browser) pokeAck(t *btab) {
	if t == nil {
		return
	}
	b.mu.Lock()
	ns := t.ackHeld
	t.ackHeld = nil
	b.mu.Unlock()
	for _, n := range ns {
		t.src.cdp.fire(t.sess, "Page.screencastFrameAck", map[string]any{"sessionId": n})
	}
}

// devKick sends drawer entries past their mark to the viewers with that tab's drawer open, four times a second
func (b *browser) devKick(t *btab) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.devT == nil {
		t.devT = time.AfterFunc(250*time.Millisecond, func() {
			b.mu.Lock()
			t.devT = nil
			b.mu.Unlock()
			b.devSend(t)
		})
	}
}

func (b *browser) devSend(t *btab) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for v := range b.viewers {
		if v.devTab != t.id || t.devSeq <= v.devMark {
			continue
		}
		mark := v.devMark
		v.devMark = t.devSeq
		var cons, probs []bprob
		if t.cons != nil && len(t.cons) > brConsoleKept && mark < t.cons[len(t.cons)-brConsoleKept].Seq {
			mark = 0 // more than it keeps: start it again from what's kept
		}
		for _, p := range t.cons {
			if p.Seq > mark {
				cons = append(cons, p)
			}
		}
		for _, p := range t.probs {
			if p.Seq > mark {
				probs = append(probs, p)
			}
		}
		var nets []bnet
		for _, n := range t.nets {
			if n.Seq > mark {
				nets = append(nets, *n)
			}
		}
		raw, _ := json.Marshal(map[string]any{"t": "dev", "tab": t.id, "reset": mark == 0, "cons": cons, "probs": probs, "nets": nets})
		v.queue(raw)
		v.kick()
	}
}

func (b *browser) eval(v *bview, t *btab, expr string) {
	var r struct {
		Result           cdpRemote
		ExceptionDetails *struct {
			Text      string
			Exception *cdpRemote
		}
	}
	err := t.src.cdp.callT(10*time.Second, t.sess, "Runtime.evaluate", map[string]any{"expression": expr, "replMode": true, "generatePreview": true,
		"userGesture": true, "awaitPromise": true, "allowUnsafeEvalBlockedByCSP": true}, &r)
	out := bprob{Kind: "eval", Level: "log", Text: r.Result.text()}
	if err != nil {
		out.Level, out.Text = "error", err.Error()
	} else if r.ExceptionDetails != nil {
		out.Level = "error"
		out.Text = r.ExceptionDetails.Text
		if r.ExceptionDetails.Exception != nil {
			out.Text = r.ExceptionDetails.Exception.text()
		}
	}
	if r.Result.Type == "undefined" {
		out.Text = "undefined"
	}
	v.send(map[string]any{"t": "evaled", "tab": t.id, "expr": trunc(expr, 2000), "out": out})
}

// still: a capture of the tab as it is now, for annotating (PNG at the page's density: text stays sharp).
// keep: also saved with the chat's uploads, as the clean picture an agent gets (its path in the meta), so the
// page never uploads it; q < 0 then sends only the meta.
func (b *browser) still(v *bview, t *btab, q int, keep bool) {
	var r struct {
		Data []byte `json:"data"`
	}
	cq := q
	if keep && cq < 0 {
		cq = 85
	}
	params := map[string]any{"format": "png", "fromSurface": true, "captureBeyondViewport": false}
	if cq > 0 {
		params = map[string]any{"format": "jpeg", "quality": min(cq, 95), "fromSurface": true}
	}
	if cq >= 0 {
		if err := t.src.cdp.callT(15*time.Second, t.sess, "Page.captureScreenshot", params, &r); err != nil {
			v.send(map[string]any{"t": "toast", "text": "Couldn't capture the page: " + err.Error()})
			return
		}
	}
	path := ""
	if keep && len(r.Data) > 0 && b.upDir != "" && os.MkdirAll(b.upDir, 0o700) == nil {
		rnd := make([]byte, 8)
		rand.Read(rnd)
		p := filepath.Join(b.upDir, time.Now().Format("20060102-150405")+"-"+hex.EncodeToString(rnd)+"-page.jpg")
		if os.WriteFile(p, r.Data, 0o600) == nil {
			path = p
		}
	}
	if q < 0 {
		r.Data = nil
	}
	var lm struct {
		CSSVisualViewport struct{ ClientWidth, ClientHeight, PageX, PageY float64 } `json:"cssVisualViewport"`
	}
	t.src.cdp.call(t.sess, "Page.getLayoutMetrics", nil, &lm)
	b.mu.Lock()
	meta, _ := json.Marshal(map[string]any{"tab": t.id, "url": trunc(t.URL, 2000), "title": trunc(t.Title, 300), "dw": lm.CSSVisualViewport.ClientWidth, "dh": lm.CSSVisualViewport.ClientHeight,
		"sx": lm.CSSVisualViewport.PageX, "sy": lm.CSSVisualViewport.PageY, "vp": t.vp, "probs": [3]int{t.nE, t.nW, t.nN}, "path": path})
	b.mu.Unlock()
	f := &bframe{img: r.Data, meta: meta}
	raw := frameMsg(2, f)
	b.mu.Lock()
	gone := v.gone
	b.mu.Unlock()
	if !gone {
		v.ws.write(wsBinary, raw)
	}
}

// probe: the element at a point in the page, described for an agent: a selector, its text, its HTML (short), its
// box and the styles that usually matter.
func (b *browser) probe(v *bview, t *btab, x, y float64, seq int64) {
	var n struct{ BackendNodeID int }
	err := t.src.cdp.call(t.sess, "DOM.getNodeForLocation", map[string]any{"x": int(x), "y": int(y), "includeUserAgentShadowDOM": false, "ignorePointerEventsNone": true}, &n)
	if err != nil {
		t.src.cdp.call(t.sess, "DOM.enable", nil, nil)
		t.src.cdp.call(t.sess, "DOM.getDocument", map[string]any{"depth": 0}, nil)
		err = t.src.cdp.call(t.sess, "DOM.getNodeForLocation", map[string]any{"x": int(x), "y": int(y), "includeUserAgentShadowDOM": false, "ignorePointerEventsNone": true}, &n)
	}
	info := b.describe(t, n.BackendNodeID)
	if err != nil {
		info = map[string]any{"error": err.Error()}
	}
	v.send(map[string]any{"t": "probed", "tab": t.id, "seq": seq, "info": info})
}

// area: what a drawn mark encloses, for an agent: the element whose box fits the mark's best, and the text inside.
func (b *browser) area(v *bview, t *btab, x, y float64, w, h int, seq int64) {
	var r struct {
		Result struct{ Value map[string]any }
	}
	info := map[string]any{"error": "nothing there"}
	err := t.src.cdp.call(t.sess, "Runtime.evaluate", map[string]any{"expression": fmt.Sprintf(brAreaJS, brDescribeJS, x, y, w, h), "returnByValue": true}, &r)
	if err != nil {
		info = map[string]any{"error": err.Error()}
	} else if r.Result.Value != nil {
		info = r.Result.Value
	}
	v.send(map[string]any{"t": "probed", "tab": t.id, "seq": seq, "info": info})
}

func (b *browser) describe(t *btab, backend int) map[string]any {
	return b.describeOn(t.src.cdp, t.sess, backend)
}

func (b *browser) describeOn(c *cdpConn, sess string, backend int) map[string]any {
	if backend == 0 {
		return map[string]any{"error": "nothing there"}
	}
	var o struct{ Object struct{ ObjectID string } }
	if err := c.call(sess, "DOM.resolveNode", map[string]any{"backendNodeId": backend}, &o); err != nil {
		return map[string]any{"error": err.Error()}
	}
	var r struct {
		Result struct{ Value map[string]any }
	}
	c.call(sess, "Runtime.callFunctionOn", map[string]any{"objectId": o.Object.ObjectID, "functionDeclaration": brDescribeJS, "returnByValue": true}, &r)
	c.fire(sess, "Runtime.releaseObject", map[string]any{"objectId": o.Object.ObjectID})
	return r.Result.Value
}

var brHighlight = map[string]any{"showInfo": true, "showStyles": true, "showAccessibilityInfo": true,
	"contentColor": map[string]any{"r": 111, "g": 168, "b": 220, "a": 0.66}, "paddingColor": map[string]any{"r": 147, "g": 196, "b": 125, "a": 0.55},
	"borderColor": map[string]any{"r": 255, "g": 229, "b": 153, "a": 0.66}, "marginColor": map[string]any{"r": 246, "g": 178, "b": 107, "a": 0.66}}

// brCursorJS: the cursor the page would show at a point: its CSS cursor, else what's there (a link, a field, text)
// brMobileUA: Chrome's own Android user agent for this version (its reduced form: "Android 10; K")
func brMobileUA(ua string) string {
	if i, j := strings.Index(ua, "("), strings.Index(ua, ")"); i >= 0 && j > i {
		ua = ua[:i] + "(Linux; Android 10; K)" + ua[j+1:]
	}
	if !strings.Contains(ua, "Mobile") {
		ua = strings.Replace(ua, " Safari/", " Mobile Safari/", 1)
	}
	return ua
}

// brFieldsJS: the boxes of the page's visible fields you can type in, in CSS px of the viewport
const brFieldsJS = `(() => { const out = [], vw = innerWidth, vh = innerHeight;
  for (const e of document.querySelectorAll('input, textarea, [contenteditable]:not([contenteditable="false"])')) {
    if (e.disabled || e.readOnly || (e.tagName === 'INPUT' && /^(hidden|checkbox|radio|button|submit|reset|range|color|file|image)$/i.test(e.type))) continue;
    const r = e.getBoundingClientRect();
    if (r.width < 2 || r.height < 2 || r.bottom < 0 || r.right < 0 || r.top > vh || r.left > vw) continue;
    out.push([Math.round(r.left), Math.round(r.top), Math.round(r.width), Math.round(r.height)]);
    if (out.length >= 300) break;
  }
  return out; })()`

const brCursorJS = `((x, y) => { const e = document.elementFromPoint(x, y); if (!e) return 'default';
  const c = getComputedStyle(e).cursor; if (c && c !== 'auto') return c;
  for (let n = e; n && n.nodeType === 1; n = n.parentElement) {
    if (n.isContentEditable) return 'text';
    if (n.localName === 'a' && n.href) return 'pointer';
    if (n.localName === 'textarea') return 'text';
    if (n.localName === 'input') return /^(button|submit|reset|checkbox|radio|file|image|range|color)$/.test(n.type) ? 'default' : 'text';
    if (n.localName === 'button' || n.localName === 'select' || n.localName === 'label') return 'default';
  }
  const r = document.caretRangeFromPoint && document.caretRangeFromPoint(x, y);
  return r && r.startContainer.nodeType === 3 && r.startContainer.textContent.trim() ? 'text' : 'default'; })(%f, %f)`

// brFindJS: the next (or previous) match, selected and scrolled to, and how many there are
const brFindJS = `((q, back, fresh) => { if (fresh) getSelection().removeAllRanges();
  const ok = !!q && window.find(q, false, back, true, false, true, false);
  let n = 0;
  if (q) { const lq = q.toLowerCase(), w = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    while (w.nextNode() && n < 9999) { const s = w.currentNode.data.toLowerCase(); for (let i = s.indexOf(lq); i >= 0; i = s.indexOf(lq, i + lq.length)) n++; } }
  return { ok, n }; })(%s, %t, %t)`

const brSelectionJS = `(() => { const a = document.activeElement;
  if (a && (a.tagName === 'INPUT' || a.tagName === 'TEXTAREA') && typeof a.selectionStart === 'number') return a.value.slice(a.selectionStart, a.selectionEnd);
  return String(getSelection()); })()`

// brDescribeJS runs on the element (this): what an agent needs to find it in the code.
const brDescribeJS = `function () {
  let el = this.nodeType === 1 ? this : this.parentElement;
  if (!el) return { error: 'not an element' };
  const sel = (e) => {
    const parts = [];
    for (; e && e.nodeType === 1 && parts.length < 6; e = e.parentElement) {
      if (e.id && /^[A-Za-z][\w-]*$/.test(e.id)) { parts.unshift('#' + e.id); break; }
      let s = e.localName;
      const cls = [...e.classList].filter((c) => /^[A-Za-z_][\w-]*$/.test(c) && c.length < 40).slice(0, 2);
      if (cls.length) s += '.' + cls.join('.');
      const tid = e.getAttribute('data-testid') || e.getAttribute('data-test');
      if (tid) s += '[data-testid="' + tid + '"]';
      const p = e.parentElement;
      if (p && [...p.children].filter((c) => c.localName === e.localName).length > 1 && !tid) s += ':nth-of-type(' + ([...p.children].filter((c) => c.localName === e.localName).indexOf(e) + 1) + ')';
      parts.unshift(s);
    }
    return parts.join(' > ');
  };
  const r = el.getBoundingClientRect(), cs = getComputedStyle(el);
  const keep = ['display', 'position', 'width', 'height', 'margin', 'padding', 'color', 'background-color', 'font-family', 'font-size', 'font-weight', 'line-height', 'border', 'border-radius', 'gap', 'flex-direction', 'justify-content', 'align-items', 'grid-template-columns', 'z-index', 'opacity', 'overflow'];
  const styles = {};
  for (const k of keep) { const v = cs.getPropertyValue(k); if (v && v !== 'none' && v !== 'normal' && v !== 'auto' && v !== '0px' && v !== 'rgba(0, 0, 0, 0)') styles[k] = v; }
  let html = el.outerHTML;
  if (html.length > 1500) html = html.slice(0, 1500) + '…';
  const text = (el.innerText || el.textContent || '').replace(/\s+/g, ' ').trim().slice(0, 200);
  const comp = (() => { for (let e = el; e; e = e.parentElement) { const k = Object.keys(e).find((k) => k.startsWith('__reactFiber$')); if (k) { let f = e[k]; while (f) { const t = f.type; if (t && typeof t !== 'string') return t.displayName || t.name || ''; f = f.return; } } if (e.__vueParentComponent) return e.__vueParentComponent.type?.name || e.__vueParentComponent.type?.__name || ''; } return ''; })();
  return { selector: sel(el), tag: el.localName, text, html, rect: { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) }, styles, role: el.getAttribute('role') || '', component: comp };
}`

// brAreaJS: the element a rectangle (viewport CSS px) best frames: of the elements under a grid of points in it,
// and their parents, the one whose box overlaps it most (intersection over union); described like a pin's, plus
// the text that shows inside the rectangle.
const brAreaJS = `((describe, x, y, w, h) => {
  const seen = new Set(), area = (r) => Math.max(0, r.width) * Math.max(0, r.height);
  let best = null, bs = 0;
  for (let i = 0; i < 5; i++) for (let j = 0; j < 5; j++) {
    for (let e = document.elementFromPoint(x + w * (i + 0.5) / 5, y + h * (j + 0.5) / 5); e && e !== document.documentElement; e = e.parentElement) {
      if (seen.has(e)) break;
      seen.add(e);
      const r = e.getBoundingClientRect();
      const ix = Math.max(0, Math.min(x + w, r.right) - Math.max(x, r.left)) * Math.max(0, Math.min(y + h, r.bottom) - Math.max(y, r.top));
      const s = ix / (w * h + area(r) - ix);
      if (s > bs) { bs = s; best = e; }
    }
  }
  if (!best) return { error: 'nothing there' };
  const d = describe.call(best);
  const parts = [], tw = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT), rg = document.createRange();
  let len = 0;
  for (let n = 0; tw.nextNode() && n < 20000 && len < 400; n++) {
    const s = tw.currentNode.data.replace(/\s+/g, ' ').trim();
    if (!s) continue;
    rg.selectNodeContents(tw.currentNode);
    const r = rg.getBoundingClientRect(), cx = r.left + r.width / 2, cy = r.top + r.height / 2;
    if (r.width && cx >= x && cx <= x + w && cy >= y && cy <= y + h) { parts.push(s); len += s.length + 3; }
  }
  d.inside = parts.join(' · ').slice(0, 400);
  d.fit = Math.round(bs * 100);
  return d;
})(%s, %f, %f, %d, %d)`

// brEditCommands: Chromium on Linux does its editing shortcuts itself, except the ones a Mac sends with ⌘: the
// page's keys arrive with Ctrl already (browser.js), and these make sure the edit happens.
func brEditCommands(m bmsg) []string {
	if m.Type != "rawKeyDown" && m.Type != "keyDown" || m.Mods&2 == 0 {
		return nil
	}
	switch strings.ToLower(m.Key) {
	case "a":
		return []string{"selectAll"}
	case "z":
		if m.Mods&8 != 0 {
			return []string{"redo"}
		}
		return []string{"undo"}
	case "y":
		return []string{"redo"}
	}
	return nil
}

var brHostRe = regexp.MustCompile(`^(localhost|\d{1,3}(\.\d{1,3}){3}|\[[0-9a-fA-F:]+\]|[\w-]+(\.[\w-]+)+)(:\d+)?(/.*)?$`)

// brURL turns what was typed in the address bar into a URL: a scheme is kept, ":3000" or "3000" is localhost,
// a host is https (http for localhost and IPs), anything else is a search.
func brURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if regexp.MustCompile(`^:?\d{2,5}(/.*)?$`).MatchString(s) {
		return "http://localhost:" + strings.TrimPrefix(s, ":")
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && (u.Host != "" || u.Scheme == "about" || u.Scheme == "data" || u.Scheme == "file" || u.Scheme == "chrome") {
		switch u.Scheme {
		case "http", "https", "about", "data", "file", "chrome", "view-source":
			return s
		}
	}
	if !strings.Contains(s, " ") && brHostRe.MatchString(s) {
		host := s
		if i := strings.IndexAny(host, "/"); i >= 0 {
			host = host[:i]
		}
		h := host
		if hh, _, err := netSplit(host); err == nil {
			h = hh
		}
		if h == "localhost" || strings.HasSuffix(h, ".localhost") || regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`).MatchString(h) || strings.HasPrefix(h, "[") {
			return "http://" + s
		}
		return "https://" + s
	}
	// DuckDuckGo: Google, Brave and Startpage answer the VM's datacenter address with a robot check
	return "https://duckduckgo.com/?q=" + url.QueryEscape(s)
}

func netSplit(hostport string) (string, string, error) {
	i := strings.LastIndexByte(hostport, ':')
	if i < 0 || strings.HasSuffix(hostport, "]") {
		return hostport, "", errors.New("no port")
	}
	return hostport[:i], hostport[i+1:], nil
}

// close stops the owner's Chromium (tests; the panel's own exit takes it with the pipe).
func (b *browser) close() {
	b.stopDT()
	b.mu.Lock()
	me := b.me
	b.mu.Unlock()
	if me != nil && me.cmd != nil && me.cmd.Process != nil {
		syscall.Kill(-me.cmd.Process.Pid, syscall.SIGKILL)
	}
}
