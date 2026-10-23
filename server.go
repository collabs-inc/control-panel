package main

import (
	"bytes"
	"cmp"
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
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type config struct {
	behindCube                                 bool
	addr, cfTeam, cfAUD                        string
	pinFile, auditFile, ciDir, statusDir, home string
	agentAddr, agentTokenFile                  string
	aiURL, aiKeyFile                           string // the assistant (vm-agent): Hermes API server and its key
	hermesEnv                                  string // Hermes' .env: the Telegram settings Connections writes
	compactScope                               string // the zellij sessions the compactor types into (regexp; "": all)
	insecure                                   bool
}

var teamRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func validateConfig(c config) error {
	host, _, err := net.SplitHostPort(c.addr)
	if err != nil {
		return fmt.Errorf("bad -addr: %v", err)
	}
	if ip, err := netip.ParseAddr(host); err != nil || !ip.IsLoopback() {
		return fmt.Errorf("-addr %s is not loopback: the panel must only be reachable through the tunnel", c.addr)
	}
	if _, err := regexp.Compile(c.compactScope); err != nil {
		return fmt.Errorf("bad -compact-sessions: %v", err)
	}
	if c.agentAddr != "" {
		if err := validAgentAddr(c.agentAddr); err != nil {
			return err
		}
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

// send queues msg unless it's what the page has already. A page too slow to take it skips this tick and gets
// the state on a later one, even if nothing changes meanwhile.
func (c *client) send(msg []byte) {
	if bytes.Equal(msg, c.last) {
		return
	}
	select {
	case c.ch <- msg:
		c.last = msg
	default:
	}
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
	fsx     *fsIndex // Files: the name index, and the roots the Files API may touch
	version string   // the build the page runs: a hash of index.html, which names every asset's hash
	skills  skillCache
	ci      *ciTracker
	term    *termTracker
	z       *zellij
	limits  *limiter
	arts    artifactCache
	prs     prCache
	cards   prCards
	notes   *notifier
	tokens  *tokScan
	ai      *assistant
	compact *compactor
	br      *browser // the Browser: the owner's Chromium and the agents' (browser.go)

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
		fsx:       newFsIndex(cfg.home, "/tmp"),
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
	s.auth.gated = cfg.behindCube
	s.prs.fetch = ghIn
	s.notes = newNotifier(filepath.Join(filepath.Dir(cfg.auditFile), "notes.json"))
	s.tokens = newTokScan(filepath.Join(claude, "projects"), filepath.Join(filepath.Dir(cfg.auditFile), "tokens.json"))
	s.ai = newAssistant(cmp.Or(cfg.aiURL, "http://127.0.0.1:8642"), cmp.Or(cfg.aiKeyFile, filepath.Join(cfg.home, ".config/vm-agent/api-key")), filepath.Join(cfg.home, ".hermes/google_client_secret.json"))
	s.ai.tasksFile = filepath.Join(cfg.home, ".local/state/vm-agent/tasks.json")
	s.ai.stateFile = filepath.Join(filepath.Dir(cfg.auditFile), "assistant.json")
	s.ai.loadPanel()
	if !cfg.insecure {
		s.access = newAccess(cfg.cfTeam, cfg.cfAUD)
	}
	s.compact = newCompactor(filepath.Join(filepath.Dir(cfg.auditFile), "compact.json"), cfg.compactScope)
	s.term.compactOf = s.compact.of
	s.term.paused = func(key string) bool {
		for _, l := range s.limits.list() {
			if l.Key == key && l.Paused {
				return true
			}
		}
		return false
	}
	s.br = newBrowser(s, filepath.Dir(cfg.auditFile), cfg.home)
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
		idx.hash = s.version // its ETag changes with any asset it names
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
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	if name != "index.html" && r.URL.Query().Get("v") == a.hash {
		h.Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	h.Set("Vary", "Accept-Encoding")
	h.Set("ETag", `W/"`+a.hash+`"`) // a launch revalidates index.html (304) instead of downloading it again
	body := a.body
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
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
	mux.HandleFunc("POST /api/pane/fit", s.withSession(s.paneFit))
	mux.HandleFunc("POST /api/pane/write", s.withSession(s.paneWrite))
	mux.HandleFunc("POST /api/pane/upload", s.withSession(s.paneUpload))
	mux.HandleFunc("GET /api/upload/{name}", s.withSession(s.uploadGet))
	mux.HandleFunc("GET /api/pane/files", s.withSession(s.paneFiles))
	mux.HandleFunc("GET /api/pane/file", s.withSession(s.paneFile))
	mux.HandleFunc("GET /api/pane/file/comments", s.withSession(s.paneFileComments))
	mux.HandleFunc("POST /api/pane/file/comments", s.withSession(s.paneFileComments))
	mux.HandleFunc("POST /api/term/new", s.withSession(s.termNew))
	mux.HandleFunc("POST /api/term/close", s.withSession(s.termClose))
	mux.HandleFunc("POST /api/term/move", s.withSession(s.termMove))
	mux.HandleFunc("POST /api/term/rename", s.withSession(s.termRename))
	mux.HandleFunc("GET /api/pane/peek", s.withSession(s.panePeek))
	mux.HandleFunc("POST /api/limit", s.withSession(s.limit))
	mux.HandleFunc("GET /api/disk", s.withSession(s.diskView))
	mux.HandleFunc("GET /api/pane/skills", s.withSession(s.paneSkills))
	mux.HandleFunc("GET /api/pane/pr", s.withSession(s.panePR))
	mux.HandleFunc("POST /api/compact", s.withSession(s.compactSettings))
	mux.HandleFunc("POST /api/pane/compact", s.withSession(s.paneCompact))
	mux.HandleFunc("GET /api/notes", s.withSession(s.notesList))
	mux.HandleFunc("GET /api/tokens", s.withSession(s.tokensGet))
	mux.HandleFunc("POST /api/pane/unqueue", s.withSession(s.paneUnqueue))
	mux.HandleFunc("POST /api/pane/takebox", s.withSession(s.paneTakeBox))
	mux.HandleFunc("GET /api/term/list", s.withSession(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.term.listing()) }))
	mux.HandleFunc("POST /api/notes/seen", s.withSession(s.notesSeen))
	mux.HandleFunc("GET /api/dirs", s.withSession(s.dirs))
	mux.HandleFunc("GET /api/disk/check", s.withSession(s.diskCheck))
	mux.HandleFunc("POST /api/disk/delete", s.withSession(s.diskDelete))
	mux.HandleFunc("GET /api/fs/list", s.withSession(s.fsList))
	mux.HandleFunc("GET /api/fs/raw", s.withSession(s.fsRaw))
	mux.HandleFunc("GET /api/fs/thumb", s.withSession(s.fsThumb))
	mux.HandleFunc("PUT /api/fs/file", s.withSession(s.fsWrite))
	mux.HandleFunc("POST /api/fs/xlsx", s.withSession(s.fsXlsx))
	mux.HandleFunc("POST /api/fs/timing", s.withSession(s.fsTiming))
	mux.HandleFunc("POST /api/fs/upload", s.withSession(s.fsUpload))
	mux.HandleFunc("POST /api/fs/mkdir", s.withSession(s.fsMkdir))
	mux.HandleFunc("POST /api/fs/rename", s.withSession(s.fsRename))
	mux.HandleFunc("POST /api/fs/move", s.withSession(s.fsMoveTo))
	mux.HandleFunc("POST /api/fs/copy", s.withSession(s.fsCopy))
	mux.HandleFunc("POST /api/fs/delete", s.withSession(s.fsDelete))
	mux.HandleFunc("GET /api/fs/search", s.withSession(s.fsSearch))
	mux.HandleFunc("GET /api/fs/grep", s.withSession(s.fsGrep))
	mux.HandleFunc("GET /api/fs/repos", s.withSession(s.fsRepos))
	mux.HandleFunc("POST /api/fs/clone", s.withSession(s.fsClone))
	mux.HandleFunc("GET /api/browser/ws", s.withSession(s.br.serveWS))
	mux.HandleFunc("GET /api/browser/ports", s.withSession(s.br.ports))
	mux.HandleFunc("GET /api/browser/projects", s.withSession(s.br.projects))
	mux.HandleFunc("POST /api/browser/run", s.withSession(s.br.run))
	mux.HandleFunc("POST /api/browser/stop", s.withSession(s.br.stop))
	mux.HandleFunc("GET /api/browser/icon", s.withSession(s.br.favicon))
	mux.HandleFunc("GET /api/browser/devtools/f/{ver}/{path...}", s.withSession(s.br.devtoolsFile))
	mux.HandleFunc("GET /api/browser/devtools/ws/{tab}", s.withSession(s.br.devtoolsWS))
	mux.HandleFunc("GET /api/browser/trails", s.withSession(s.br.trails))
	mux.HandleFunc("GET /api/browser/trail", s.withSession(s.br.trail))
	mux.HandleFunc("GET /api/browser/trail/shot", s.withSession(s.br.trailShot))
	mux.HandleFunc("GET /api/browser/trail/call", s.withSession(s.br.trailCallGet))
	mux.HandleFunc("POST /api/browser/trail/edit", s.withSession(s.br.trailEdit))
	mux.HandleFunc("GET /api/ai/stream", s.withSession(s.aiStream))
	mux.HandleFunc("GET /api/ai/history", s.withSession(s.aiHistory))
	mux.HandleFunc("POST /api/ai/send", s.withSession(s.aiSend))
	mux.HandleFunc("POST /api/ai/stop", s.withSession(s.aiStop))
	mux.HandleFunc("POST /api/ai/unqueue", s.withSession(s.aiUnqueue))
	mux.HandleFunc("POST /api/ai/session", s.withSession(s.aiSession))
	mux.HandleFunc("GET /api/ai/model", s.withSession(s.aiModel))
	mux.HandleFunc("POST /api/ai/model", s.withSession(s.aiModel))
	mux.HandleFunc("GET /api/ai/google", s.withSession(s.aiGoogle))
	mux.HandleFunc("GET /api/ai/file", s.withSession(s.aiFile))
	mux.HandleFunc("GET /api/ai/telegram", s.withSession(s.aiTelegram))
	mux.HandleFunc("POST /api/ai/telegram", s.withSession(s.aiTelegram))
	mux.HandleFunc("POST /api/ai/restart", s.withSession(s.aiRestart))
	mux.HandleFunc("POST /api/ai/tasks/cancel", s.withSession(s.aiTaskCancel))
	mux.HandleFunc("POST /api/ai/tasks/done", s.withSession(s.aiTaskDone))
	mux.HandleFunc("POST /api/ai/google", s.withSession(s.aiGoogle))
	hermes := s.withSession(hermesProxy("http://127.0.0.1:9119").ServeHTTP)
	for _, p := range []string{"/hermes/", "/assets/", "/fonts/", "/fonts-terminal/", "/ds-assets/"} { // the dashboard's lazy chunks ask for its absolute asset paths, unprefixed
		mux.Handle(p, hermes)
	}
	return securityHeaders(http.NewCrossOriginProtection().Handler(s.requireAccess(mux)))
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		if strings.HasPrefix(r.URL.Path, "/hermes/") { // Hermes' dashboard (its own app): no inline-free CSP, still unframeable
			hd.Set("Content-Security-Policy", "frame-ancestors 'none'")
		} else if strings.HasPrefix(r.URL.Path, "/api/browser/devtools/f/") { // Chromium's DevTools, in a frame of the Browser
			hd.Set("Content-Security-Policy", brDTCSP)
			hd.Set("X-Content-Type-Options", "nosniff")
			hd.Set("Referrer-Policy", "no-referrer")
			hd.Set("X-Frame-Options", "SAMEORIGIN")
			h.ServeHTTP(w, r)
			return
		} else {
			// style-src allows inline styles: Mermaid's diagrams carry <style> and style attributes. Scripts stay
			// 'self' only, and with img/font/connect locked to 'self' an injected style can't send anything out.
			hd.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		}
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

func sessionToken(r *http.Request) string {
	c, err := r.Cookie("cp_session")
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *server) withSession(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.validSession(sessionToken(r), identity(r)) {
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

// umask is the process's file-creation mask, read once before anything runs: writeFileAtomic applies it to perm
// as os.WriteFile would.
var umask = func() fs.FileMode { m := syscall.Umask(0); syscall.Umask(m); return fs.FileMode(m) }()

// writeFileAtomic replaces p with b: a fresh temp file beside it (O_EXCL, so a link planted at a fixed temp name
// is never written through), then a rename, so a reader sees the old file or the new one, never half of one.
func writeFileAtomic(p string, b []byte, perm fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Chmod(f.Name(), perm&^umask)
	}
	if err == nil {
		err = os.Rename(f.Name(), p)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
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

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var body struct{ PIN string }
	readJSON(r, &body)
	if s.pinError(w, s.auth.checkPIN(body.PIN, clientIP(r))) {
		return
	}
	tok := s.auth.newSession(identity(r))
	http.SetCookie(w, &http.Cookie{Name: "cp_session", Value: tok, Path: "/", HttpOnly: true,
		Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionIdle.Seconds())})
	s.audit.add("login", identity(r), clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	s.auth.endSession(sessionToken(r))
	http.SetCookie(w, &http.Cookie{Name: "cp_session", Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
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

var views = map[string]bool{"res": true, "ci": true, "term": true, "sec": true, "ai": true, "files": true, "browser": true} // ai, files, browser: badges only, they fetch their own data

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

	c, done := s.subscribe(view)
	defer done()
	tok, email := sessionToken(r), identity(r)
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

// subscribe adds a client of view and wakes the sampler, so its first frame comes at once; done removes it.
func (s *server) subscribe(view string) (c *client, done func()) {
	c = &client{view: view, ch: make(chan []byte, 2)}
	s.mu.Lock()
	s.clients[c] = true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return c, func() {
		s.mu.Lock()
		delete(s.clients, c)
		s.mu.Unlock()
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

// sampleLoop looks every 2 s while a page is open, and every 10 s with none: enough for the
// notifications to catch what happens while you're away (zellij is asked at most every 30 s then).
func (s *server) sampleLoop() {
	go s.fsx.run() // here, not in newServer: test servers don't walk the disk
	var st sampleState
	for {
		s.tick(time.Now(), &st)
		every := 2 * time.Second
		if s.clientCount() == 0 {
			every = 10 * time.Second
		}
		select {
		case <-time.After(every):
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

	// ports and connections are shown only on a page; with none open, only the security scan needs the sockets
	if now.Sub(st.sockAt) >= 10*time.Second && (len(clients) > 0 || now.Sub(st.secAt) >= 60*time.Second) {
		st.sock, st.sockAt = scanSockets("/proc"), now
	}
	res := s.sys.sample(now, st.sock)
	if wantView["files"] { // its disk-usage chart
		s.du.refresh(now, false)
	}
	if wantView["res"] {
		res.Limits = s.limits.list()
	}
	tm := s.term.update(now, s.sys.liveSessions(), res, s.sys.keyOf, wantView["term"])
	s.compactStep(now, tm)
	s.cards.warmAll(tm, s.prCard)
	cc := s.compact.config()
	tm.Compact = &cc
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
	s.notes.observe(now, tm, st.ci, st.sec, res, func(p TPane) string {
		return lastSaid(cmp.Or(p.Agent.path, s.term.tr.path(p.Agent.sid)))
	})
	note, unread := s.notes.badge()
	badges := map[string]any{
		"cpu": res.CPU, "mem": memPct, "waiting": tm.Waiting, "asking": tm.Asking, "working": tm.Working,
		"ciBusy": busy, "ciFail": st.ci.FailedDay, "secBad": st.sec.Bad, "note": note, "unread": unread, "agentBrowsers": s.br.liveAgents(),
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
		c.send(msg)
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
