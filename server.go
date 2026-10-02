package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

type config struct {
	addr, cfTeam, cfAUD                        string
	pinFile, auditFile, ciDir, statusDir, home string
	insecure                                   bool
	embedLocal                                 bool
}

var teamRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func validateConfig(c config) error {
	if c.embedLocal && !c.insecure {
		return errors.New("-embed-local requires -insecure-no-access")
	}
	host, _, err := net.SplitHostPort(c.addr)
	if err != nil {
		return fmt.Errorf("bad -addr: %v", err)
	}
	if ip, err := netip.ParseAddr(host); err != nil || !ip.IsLoopback() {
		return fmt.Errorf("-addr %s is not loopback: the panel must only be reachable through the tunnel", c.addr)
	}
	if c.insecure {
		return nil
	}
	if !teamRe.MatchString(c.cfTeam) || c.cfAUD == "" {
		return errors.New("set -cf-team (your Zero Trust team name) and -cf-aud (the Access application AUD tag)")
	}
	return nil
}

type asset struct {
	body, gz []byte
	ctype    string
	hash     string
}

type client struct {
	view  string
	ch    chan []byte
	last  []byte
	histT int64
}

type server struct {
	cfg    config
	auth   *auth
	access *access
	audit  *auditLog
	actor  *actor
	assets map[string]*asset

	sys     *sysSampler
	hist    *history
	du      *duIndex
	version string // the build the page runs: a hash of index.html, which names every asset's hash
	skills  skillCache
	ci      *ciTracker
	term    *termTracker
	z       *zellij
	limits  *limiter
	arts    artifactCache

	uploadDir string // chat image uploads, handed to agents by path

	mu         sync.Mutex
	lastWrite  map[string]time.Time // per pane: terminal typing is audited once a minute
	clients    map[*client]bool
	wake       chan struct{}
	lastJWTLog time.Time
	errMu      sync.Mutex // client-errors.jsonl
}

func newServer(cfg config) *server {
	audit := openAudit(cfg.auditFile)
	sys := newSysSampler("/proc")
	z := newZellij()
	claude := filepath.Join(cfg.home, ".claude")
	s := &server{
		cfg:       cfg,
		audit:     audit,
		auth:      newAuth(cfg.pinFile, audit, time.Now),
		sys:       sys,
		hist:      newHistory("/proc", 360),
		du:        newDuIndex(cfg.home, filepath.Join(filepath.Dir(cfg.auditFile), "du.json")),
		ci:        &ciTracker{dir: cfg.ciDir, titles: newPRTitles(ghTitle)},
		limits:    newLimiter(filepath.Join(filepath.Dir(cfg.pinFile), "limits.json"), runtime.NumCPU(), realProcOps()),
		z:         z,
		term:      newTermTracker(z, filepath.Join(claude, "sessions"), newTranscripts(filepath.Join(claude, "projects")), &agentsTracker{dir: cfg.statusDir}),
		actor:     &actor{ciDir: cfg.ciDir, run: runCmd},
		clients:   map[*client]bool{},
		lastWrite: map[string]time.Time{},
		uploadDir: filepath.Join(filepath.Dir(cfg.auditFile), "uploads"),
		wake:      make(chan struct{}, 1),
	}
	if !cfg.insecure {
		s.access = newAccess(cfg.cfTeam, cfg.cfAUD)
	}
	s.term.paused = func(key string) bool {
		for _, l := range s.limits.list() {
			if l.Key == key && l.Paused {
				return true
			}
		}
		return false
	}
	s.loadAssets()
	return s
}

// ---- static assets: embedded, pre-gzipped, cache-busted by content hash ----

func (s *server) loadAssets() {
	s.assets = map[string]*asset{}
	fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := webFS.ReadFile(p)
		sum := sha256.Sum256(b)
		name := strings.TrimPrefix(p, "web/")
		ct := mime.TypeByExtension(path.Ext(name))
		switch path.Ext(name) {
		case ".json":
			ct = "application/manifest+json"
		case ".svg":
			ct = "image/svg+xml"
		}
		s.assets[name] = &asset{body: b, ctype: ct, hash: hex.EncodeToString(sum[:4])}
		return nil
	})
	if idx := s.assets["index.html"]; idx != nil {
		html := string(idx.body)
		for name, a := range s.assets {
			if name != "index.html" {
				html = strings.ReplaceAll(html, `"`+name+`"`, `"`+name+"?v="+a.hash+`"`)
			}
		}
		idx.body = []byte(html)
		sum := sha256.Sum256(idx.body)
		s.version = hex.EncodeToString(sum[:6])
	}
	for _, a := range s.assets {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		zw.Write(a.body)
		zw.Close()
		a.gz = buf.Bytes()
	}
}

func (s *server) serveAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	a := s.assets[name]
	if a == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", a.ctype)
	if name != "index.html" && r.URL.Query().Get("v") == a.hash {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("Vary", "Accept-Encoding")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(a.gz)
		return
	}
	w.Write(a.body)
}

// ---- middleware ----

type ctxKey struct{}

func identity(r *http.Request) string { e, _ := r.Context().Value(ctxKey{}).(string); return e }

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("Cf-Connecting-Ip"); ip != "" {
		return ip
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.serveAsset)
	mux.HandleFunc("GET /{file}", s.serveAsset)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.withSession(s.logout))
	mux.HandleFunc("GET /api/me", s.withSession(s.me))
	mux.HandleFunc("POST /api/client-error", s.withSession(s.clientError))
	mux.HandleFunc("GET /api/stream", s.withSession(s.stream))
	mux.HandleFunc("POST /api/action/{name}", s.withSession(s.action))
	mux.HandleFunc("GET /api/pane/stream", s.withSession(s.paneStream))
	mux.HandleFunc("GET /api/pane/chat", s.withSession(s.paneChat))
	mux.HandleFunc("POST /api/pane/input", s.withSession(s.paneInput))
	mux.HandleFunc("POST /api/pane/write", s.withSession(s.paneWrite))
	mux.HandleFunc("POST /api/pane/upload", s.withSession(s.paneUpload))
	mux.HandleFunc("GET /api/upload/{name}", s.withSession(s.uploadGet))
	mux.HandleFunc("GET /api/pane/files", s.withSession(s.paneFiles))
	mux.HandleFunc("GET /api/pane/file", s.withSession(s.paneFile))
	mux.HandleFunc("POST /api/term/new", s.withSession(s.termNew))
	mux.HandleFunc("POST /api/term/close", s.withSession(s.termClose))
	mux.HandleFunc("POST /api/limit", s.withSession(s.limit))
	mux.HandleFunc("GET /api/disk", s.withSession(s.diskView))
	mux.HandleFunc("GET /api/pane/skills", s.withSession(s.paneSkills))
	mux.HandleFunc("GET /api/dirs", s.withSession(s.dirs))
	mux.HandleFunc("GET /api/disk/check", s.withSession(s.diskCheck))
	mux.HandleFunc("POST /api/disk/delete", s.withSession(s.diskDelete))
	return securityHeaders(http.NewCrossOriginProtection().Handler(s.requireAccess(mux)))
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		h.ServeHTTP(w, r)
	})
}

// requireAccess admits only requests carrying a valid Cloudflare Access token.
func (s *server) requireAccess(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email := "local"
		if s.access != nil {
			var err error
			email, err = s.access.verify(r.Header.Get("Cf-Access-Jwt-Assertion"))
			if err != nil {
				s.mu.Lock()
				logIt := time.Since(s.lastJWTLog) > time.Minute // don't let a local spammer flood the audit log
				if logIt {
					s.lastJWTLog = time.Now()
				}
				s.mu.Unlock()
				if logIt { // RemoteAddr, not Cf-Connecting-Ip: an unverified request can claim any header
					host, _, _ := net.SplitHostPort(r.RemoteAddr)
					s.audit.add("access-reject", trunc(err.Error(), 80)+" "+r.Method+" "+trunc(r.URL.Path, 100), host)
				}
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, email)))
	})
}

func (s *server) sessionCookieName() string {
	if s.cfg.embedLocal {
		return "cp_session_embed"
	}
	return "cp_session"
}

func (s *server) sessionToken(r *http.Request) string {
	c, err := r.Cookie(s.sessionCookieName())
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *server) withSession(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.validSession(s.sessionToken(r), identity(r)) {
			writeJSON(w, 401, map[string]any{"error": "login required", "retryIn": int(s.auth.lockedFor().Seconds())})
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error { return readJSONN(r, v, 4096) }

func readJSONN(r *http.Request, v any, limit int64) error {
	return json.NewDecoder(io.LimitReader(r.Body, limit)).Decode(v)
}

// pinError maps checkPIN errors to responses; returns true if the request was answered.
func (s *server) pinError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errLocked):
		writeJSON(w, 429, map[string]any{"error": err.Error(), "retryIn": int(s.auth.lockedFor().Seconds())})
	case errors.Is(err, errNoPIN):
		writeJSON(w, 503, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, 401, map[string]any{"error": err.Error()})
	}
	return true
}

// ---- auth endpoints ----

// Embedded local panels have a cross-site top-level ancestor. Partition the
// HttpOnly cookie by that ancestor instead of relying on third-party cookies.
// Chromium accepts Secure cookies on localhost/loopback HTTP URLs.
func (s *server) sessionCookie(r *http.Request, token string, maxAge int) *http.Cookie {
	c := &http.Cookie{Name: s.sessionCookieName(), Value: token, Path: "/", HttpOnly: true,
		Secure: !s.cfg.insecure || r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: maxAge}
	if s.cfg.embedLocal {
		c.Secure = true
		c.SameSite = http.SameSiteNoneMode
		c.Partitioned = true
	}
	return c
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var body struct{ PIN string }
	readJSON(r, &body)
	if s.pinError(w, s.auth.checkPIN(body.PIN, clientIP(r))) {
		return
	}
	tok := s.auth.newSession(identity(r))
	http.SetCookie(w, s.sessionCookie(r, tok, int(sessionIdle.Seconds())))
	s.audit.add("login", identity(r), clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	s.auth.endSession(s.sessionToken(r))
	http.SetCookie(w, s.sessionCookie(r, "", -1))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// clientError logs an uncaught error from a browser, with its user agent, to client-errors.jsonl beside the
// audit log: a failure only one device shows (Safari on a phone) can then be read on the VM. Past 256 KB
// the log rolls over to .1, so a page stuck in an error loop can't fill the disk.
func (s *server) clientError(w http.ResponseWriter, r *http.Request) {
	var e struct{ Kind, Msg, Stack, View string }
	if readJSONN(r, &e, 8192) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad report"})
		return
	}
	line, _ := json.Marshal(map[string]any{"t": time.Now().Unix(), "kind": trunc(e.Kind, 20), "msg": trunc(e.Msg, 500),
		"stack": trunc(e.Stack, 3000), "view": trunc(e.View, 20), "ua": trunc(r.UserAgent(), 300), "ip": clientIP(r)})
	path := filepath.Join(filepath.Dir(s.cfg.auditFile), "client-errors.jsonl")
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if fi, err := os.Stat(path); err == nil && fi.Size() > 256<<10 {
		os.Rename(path, path+".1")
	}
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Write(append(line, '\n'))
		f.Close()
	}
	w.WriteHeader(204)
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"email": identity(r), "insecure": s.cfg.insecure})
}

// ---- actions ----

func (s *server) action(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PID    int
		Runner string
	}
	readJSON(r, &body)
	name := r.PathValue("name")
	var do func() error
	var what string
	switch name {
	case "kill":
		what = fmt.Sprintf("kill %d (%s)", body.PID, trunc(procCmdline(body.PID), 80))
		do = func() error { return s.actor.kill(body.PID) }
	case "restart-runner":
		what = "restart runner " + body.Runner
		do = func() error { return s.actor.restartRunner(body.Runner) }
	default:
		writeJSON(w, 404, map[string]any{"error": "no such action"})
		return
	}
	if err := do(); err != nil {
		s.audit.add("action-fail", what+": "+err.Error(), clientIP(r))
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	s.audit.add("action", what, clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true, "msg": "done: " + what})
}

// ---- live stream ----

var views = map[string]bool{"res": true, "ci": true, "term": true, "sec": true}

// sse is an open event stream (gzipped when the client accepts it).
type sse struct {
	out   io.Writer
	flush func()
	close func()
}

func startSSE(w http.ResponseWriter, r *http.Request) (*sse, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	e := &sse{out: w, flush: flusher.Flush, close: func() {}}
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		zw := gzip.NewWriter(w)
		h.Set("Content-Encoding", "gzip")
		e.out, e.close = zw, func() { zw.Close() }
		e.flush = func() { zw.Flush(); flusher.Flush() }
	}
	w.WriteHeader(200)
	fmt.Fprint(e.out, "retry: 3000\n\n")
	e.flush()
	return e, true
}

func (e *sse) data(b []byte) {
	fmt.Fprintf(e.out, "data: %s\n\n", b)
	e.flush()
}

func (e *sse) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Println("marshal:", err)
		return
	}
	e.data(b)
}

func (e *sse) ping() {
	fmt.Fprint(e.out, ": ping\n\n")
	e.flush()
}

func (s *server) stream(w http.ResponseWriter, r *http.Request) {
	view := r.URL.Query().Get("view")
	if !views[view] {
		view = "res"
	}
	e, ok := startSSE(w, r)
	if !ok {
		return
	}
	defer e.close()

	c := &client{view: view, ch: make(chan []byte, 2)}
	s.mu.Lock()
	s.clients[c] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, c)
		s.mu.Unlock()
	}()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	tok, email := s.sessionToken(r), identity(r)
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-c.ch:
			e.data(msg)
		case <-ping.C:
			if !s.auth.peekSession(tok, email) {
				return // session gone: the page's reconnect gets a 401 and shows the PIN screen
			}
			e.ping()
		}
	}
}

func (s *server) clientCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

// historyLoop keeps 1 h of cheap totals so charts are full the moment a page opens.
func (s *server) historyLoop() {
	for t := time.NewTicker(10 * time.Second); ; <-t.C { // first tick runs immediately
		s.hist.tick(time.Now())
	}
}

// sampleLoop runs only while at least one page is open.
func (s *server) sampleLoop() {
	tick := time.NewTicker(2 * time.Second)
	var st sampleState
	for {
		if s.clientCount() == 0 {
			<-s.wake
			// baseline, so the first frame shows current CPU rather than 0 or an average over the idle gap
			st.sock, st.sockAt = scanSockets("/proc"), time.Now()
			s.sys.sample(time.Now(), st.sock)
			time.Sleep(300 * time.Millisecond)
		}
		s.tick(time.Now(), &st)
		select {
		case <-tick.C:
		case <-s.wake:
		}
	}
}

type sampleState struct {
	sock                sockScan
	sockAt, ciAt, secAt time.Time
	ci                  CI
	sec                 Security
}

func (s *server) tick(now time.Time, st *sampleState) {
	s.mu.Lock()
	clients := make([]*client, 0, len(s.clients))
	wantView := map[string]bool{}
	for c := range s.clients {
		clients = append(clients, c)
		wantView[c.view] = true
	}
	s.mu.Unlock()

	if now.Sub(st.sockAt) >= 10*time.Second {
		st.sock, st.sockAt = scanSockets("/proc"), now
	}
	res := s.sys.sample(now, st.sock)
	if wantView["res"] {
		s.du.refresh(now, false)
		res.Limits = s.limits.list()
	}
	tm := s.term.update(now, s.sys.liveSessions(), res, s.sys.keyOf, wantView["term"])
	lbl := s.term.labels()
	for i := range res.Groups {
		if l, ok := lbl[res.Groups[i].Key]; ok {
			res.Groups[i].Label = l
		}
	}
	if now.Sub(st.ciAt) >= 5*time.Second {
		st.ci, st.ciAt = s.ci.poll(now, runnerAlive), now
	}
	if now.Sub(st.secAt) >= 60*time.Second {
		_, pinErr := os.Stat(s.cfg.pinFile)
		st.sec = secScan(st.sock, procComm, secEnv{
			home: s.cfg.home, ciDir: s.cfg.ciDir, panelAddr: s.cfg.addr,
			accessOn: s.access != nil, pinSet: pinErr == nil, tunnelUp: commRunning("cloudflared"),
			audit: s.audit, fails24h: s.auth.fails24h(), lockedFor: s.auth.lockedFor(),
		})
		st.secAt = now
	}
	busy := 0
	for _, r := range st.ci.Runners {
		if r.State == "busy" {
			busy++
		}
	}
	memPct := 0.0
	if res.MemTotal > 0 {
		memPct = float64(res.MemTotal-res.MemAvail) / float64(res.MemTotal) * 100
	}
	badges := map[string]any{
		"cpu": res.CPU, "mem": memPct, "waiting": tm.Waiting, "asking": tm.Asking, "working": tm.Working,
		"ciBusy": busy, "ciFail": st.ci.FailedDay, "secBad": st.sec.Bad,
	}
	hist := s.hist.points()
	for _, c := range clients {
		var data any
		switch c.view {
		case "res":
			var newPts []histPoint
			for _, p := range hist {
				if p.T > c.histT {
					newPts = append(newPts, p)
				}
			}
			if len(newPts) > 0 {
				c.histT = newPts[len(newPts)-1].T
			}
			data = map[string]any{"res": res, "hist": newPts}
		case "ci":
			data = st.ci
		case "term":
			data = tm
		case "sec":
			data = st.sec
		}
		msg, err := json.Marshal(map[string]any{"view": c.view, "badges": badges, "data": data, "v": s.version})
		if err != nil {
			log.Println("marshal:", err)
			continue
		}
		if bytes.Equal(msg, c.last) {
			continue
		}
		c.last = msg
		select {
		case c.ch <- msg:
		default: // slow client: drop this tick, the next one carries fresh state
		}
	}
}

func procComm(pid int) string {
	b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	return strings.TrimSpace(string(b))
}

func commRunning(name string) bool {
	dirs, _ := os.ReadDir("/proc")
	for _, d := range dirs {
		if pid, err := strconv.Atoi(d.Name()); err == nil && procComm(pid) == name {
			return true
		}
	}
	return false
}
