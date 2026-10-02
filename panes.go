package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Pane drawer endpoints: live screen, chat, typing into a pane, creating sessions/tabs/panes.
// Everything is argv to zellij, never a shell; every write is audited.

// pingEvery paces SSE keep-alives and the session re-check that ends streams after logout.
var pingEvery = 15 * time.Second

var allowedKeys = map[string]bool{
	"Enter": true, "Esc": true, "Tab": true, "Shift Tab": true, "Up": true, "Down": true, "Left": true, "Right": true,
	"Backspace": true, "Ctrl c": true, "PageUp": true, "PageDown": true, "y": true, "n": true,
	"0": true, "1": true, "2": true, "3": true, "4": true, "5": true, "6": true, "7": true, "8": true, "9": true,
}

func paneParams(r *http.Request) (string, int, error) {
	session := r.URL.Query().Get("session")
	pane, err := strconv.Atoi(r.URL.Query().Get("pane"))
	if !sessionNameRe.MatchString(session) || len(session) > 64 || err != nil || pane < 0 {
		return "", 0, errors.New("bad session or pane")
	}
	return session, pane, nil
}

func (s *server) sockDir() string {
	if s.sys.sockDir != "" {
		return s.sys.sockDir
	}
	return defaultSockDir()
}

func (s *server) sessionLive(name string) bool {
	d := s.sockDir()
	if d == "" || !sessionNameRe.MatchString(name) {
		return false
	}
	_, err := os.Stat(filepath.Join(d, name))
	return err == nil
}

// ---- live screen / chat ----

func (s *server) paneStream(w http.ResponseWriter, r *http.Request) {
	session, pane, err := paneParams(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	switch r.URL.Query().Get("mode") {
	case "screen":
		s.screenStream(w, r, session, pane)
	case "chat":
		s.chatStream(w, r, session, pane)
	default:
		writeJSON(w, 400, map[string]any{"error": "mode must be screen or chat"})
	}
}

// screenStream relays `zellij subscribe` (full viewport per render, ANSI colours only) at ≤ 5 fps,
// latest frame wins. The first frame also carries the pane's scrollback and is never dropped.
// The subscriber lives exactly as long as the request.
func (s *server) screenStream(w http.ResponseWriter, r *http.Request, session string, pane int) {
	ctx, cancel := context.WithCancel(r.Context()) // r.Context() only ends after we return: cancel first
	cmd := exec.CommandContext(ctx, zellijBin, "--session", session, "subscribe", "--pane-id", pid(pane),
		"--format", "json", "--ansi", "--scrollback", "1000")
	cmd.Env = cleanEnv(os.Environ())
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		cancel()
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	defer cmd.Wait()
	defer cancel() // runs before Wait: kills the subscriber
	e, ok := startSSE(w, r)
	if !ok {
		return
	}
	defer e.close()

	var mu sync.Mutex
	var first, latest []byte
	notify, done := make(chan struct{}, 1), make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReaderSize(out, 1<<20)
		for {
			line, err := br.ReadBytes('\n')
			if line = bytes.TrimSpace(line); len(line) > 0 && json.Valid(line) {
				mu.Lock()
				if bytes.Contains(line, []byte(`"is_initial":true`)) {
					first = line
				} else {
					latest = line
				}
				mu.Unlock()
				select {
				case notify <- struct{}{}:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()
	tok, email := sessionToken(r), identity(r)
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	var last time.Time
	for {
		select {
		case <-r.Context().Done():
			return
		case <-done:
			e.send(map[string]any{"t": "end"})
			return
		case <-ping.C:
			if !s.auth.peekSession(tok, email) {
				return
			}
			e.ping()
		case <-notify:
			if wait := 200*time.Millisecond - time.Since(last); wait > 0 {
				time.Sleep(wait)
			}
			mu.Lock()
			initial, f := first, latest
			first, latest = nil, nil
			mu.Unlock()
			if initial != nil {
				e.data([]byte(`{"t":"screen","u":` + string(initial) + `}`))
			}
			if f != nil {
				e.data([]byte(`{"t":"screen","u":` + string(f) + `}`))
			}
			last = time.Now()
		}
	}
}

// A chat opens on its last 5000 items (up to 16 MB of transcript); scrolling up pages back 4 MB at a time.
const (
	chatWindow = 16 << 20
	chatItems  = 5000
	chatPage   = 4 << 20
)

func lastItems(items []ChatItem, n int) []ChatItem { return items[max(0, len(items)-n):] }

// chatStream sends the transcript's tail, then whatever gets appended (polled every second).
// A new sessionId in the same pane (/clear, /resume) resets the chat.
func (s *server) chatStream(w http.ResponseWriter, r *http.Request, session string, pane int) {
	p, ok := s.term.pane(session, pane)
	if !ok || p.Agent == nil || p.Agent.path == "" {
		writeJSON(w, 404, map[string]any{"error": "no chat for this pane"})
		return
	}
	path, sid := p.Agent.path, p.Agent.sid
	q := r.URL.Query()
	var sub *Subagent // a subagent's own chat: its transcript instead of the pane's
	if q.Get("sub") != "" || q.Get("tool") != "" {
		sp, m, err := subagentPath(path, q.Get("sub"), q.Get("tool"))
		if err != nil {
			writeJSON(w, 404, map[string]any{"error": err.Error()})
			return
		}
		path, sub = sp, &m
	}
	e, ok := startSSE(w, r)
	if !ok {
		return
	}
	defer e.close()
	var end int64
	var lastPR string // the PR note last sent: a later batch doesn't repeat it
	reset := func() {
		ls, start, stop := readTail(path, chatWindow)
		items, before, title := tailChat(ls, start, chatItems, 0)
		lastPR = ""
		items = newPRs(items, &lastPR)
		m := map[string]any{"t": "chat", "reset": true, "items": items, "before": before, "title": title}
		if sub != nil {
			m["sub"] = sub
		}
		e.send(m)
		end = stop
	}
	reset()
	tok, email := sessionToken(r), identity(r)
	tick, ping := time.NewTicker(time.Second), time.NewTicker(pingEvery)
	defer tick.Stop()
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if !s.auth.peekSession(tok, email) {
				return
			}
			e.ping()
		case <-tick.C:
			if q, ok := s.term.pane(session, pane); ok && sub == nil && q.Agent != nil && q.Agent.path != "" && q.Agent.sid != sid {
				path, sid = q.Agent.path, q.Agent.sid
				reset()
				continue
			}
			fi, err := os.Stat(path)
			if err != nil {
				continue
			}
			if fi.Size() < end {
				reset()
				continue
			}
			ls, next := readFrom(path, end)
			if len(ls) == 0 {
				continue
			}
			end = next
			items, title := parseChat(ls)
			if items = newPRs(items, &lastPR); len(items) > 0 || title != "" {
				e.send(map[string]any{"t": "chat", "items": lastItems(items, 300), "title": title})
			}
		}
	}
}

// paneChat pages back through the transcript: items in the chatPage bytes before `before`.
func (s *server) paneChat(w http.ResponseWriter, r *http.Request) {
	session, pane, err := paneParams(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	p, ok := s.term.pane(session, pane)
	if !ok || p.Agent == nil || p.Agent.path == "" {
		writeJSON(w, 404, map[string]any{"error": "no chat for this pane"})
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if before <= 0 {
		writeJSON(w, 200, map[string]any{"items": []ChatItem{}, "before": 0})
		return
	}
	path := p.Agent.path
	if q := r.URL.Query(); q.Get("sub") != "" || q.Get("tool") != "" {
		if path, _, err = subagentPath(path, q.Get("sub"), q.Get("tool")); err != nil {
			writeJSON(w, 404, map[string]any{"error": err.Error()})
			return
		}
	}
	ls, start := readBefore(path, before, chatPage)
	items, _ := parseChat(ls)
	writeJSON(w, 200, map[string]any{"items": items, "before": start})
}

// ---- typing into a pane ----

func (s *server) paneInput(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Session string
		Pane    int
		Text    string
		Enter   bool
		Keys    []string
		Choose  string // answer the prompt on screen by option label…
		Sig     string // …of exactly this question (Prompt.Sig as the page saw it)
	}
	if err := readJSONN(r, &b, 96<<10); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	bad := func(msg string) { writeJSON(w, 400, map[string]any{"error": msg}) }
	switch {
	case !sessionNameRe.MatchString(b.Session) || len(b.Session) > 64 || b.Pane < 0:
		bad("bad session or pane")
		return
	case len(b.Text) > 16<<10 || !utf8.ValidString(b.Text):
		bad("text too long or not UTF-8")
		return
	case len(b.Keys) > 10:
		bad("too many keys")
		return
	case len(b.Choose) > 200:
		bad("choice too long")
		return
	case b.Choose != "" && b.Sig == "":
		bad("say which question you're answering")
		return
	case b.Text == "" && !b.Enter && len(b.Keys) == 0 && b.Choose == "":
		bad("nothing to send")
		return
	}
	for _, k := range b.Keys {
		if !allowedKeys[k] {
			bad("key not allowed: " + trunc(k, 20))
			return
		}
	}
	ps, err := s.z.panes(b.Session)
	found := false
	for _, p := range ps {
		found = found || p.ID == b.Pane
	}
	if err != nil || !found {
		writeJSON(w, 404, map[string]any{"error": "no such pane"})
		return
	}
	what := fmt.Sprintf("%s:%d", b.Session, b.Pane)
	if b.Choose != "" { // read the screen now: the highlight may have moved since the page last looked
		screen, err := s.z.dump(b.Session, b.Pane)
		var p *Prompt
		if err == nil {
			p = parsePrompt(screen)
		}
		keys, ok := []string(nil), false
		if p != nil && p.Sig == b.Sig {
			keys, ok = p.keysFor(b.Choose)
		}
		if !ok {
			writeJSON(w, 409, map[string]any{"error": "that question is no longer on screen"})
			return
		}
		what += " choose " + strconv.Quote(trunc(b.Choose, 80))
		b.Keys = append(keys, b.Keys...)
	}
	if len(b.Keys) > 0 {
		what += " keys " + strings.Join(b.Keys, ",")
		err = s.z.keys(b.Session, b.Pane, b.Keys...)
	}
	if err == nil && b.Text != "" {
		what += " " + strconv.Quote(trunc(b.Text, 80))
		err = s.z.paste(b.Session, b.Pane, b.Text)
	}
	if err == nil && b.Enter {
		what += " ⏎"
		err = s.z.keys(b.Session, b.Pane, "Enter")
	}
	if err != nil {
		s.audit.add("input-fail", what+": "+err.Error(), clientIP(r))
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	s.audit.add("input", what, clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- creating sessions, tabs, panes ----

func cleanName(n string) (string, bool) {
	n = strings.TrimSpace(n)
	if len(n) > 64 || !utf8.ValidString(n) {
		return "", false
	}
	for _, c := range n {
		if unicode.IsControl(c) {
			return "", false
		}
	}
	return n, true
}

func resolveDir(d string) (string, bool) {
	switch {
	case d == "" || d == "~":
		d = homeDir
	case strings.HasPrefix(d, "~/"):
		d = homeDir + d[1:]
	}
	if !filepath.IsAbs(d) {
		return "", false
	}
	d = filepath.Clean(d)
	fi, err := os.Stat(d)
	return d, err == nil && fi.IsDir()
}

func (s *server) termNew(w http.ResponseWriter, r *http.Request) {
	var b struct {
		What, Session, Name, Cwd, Run, Dir string
		Tab                                int
	}
	if err := readJSON(r, &b); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	bad := func(msg string) { writeJSON(w, 400, map[string]any{"error": msg}) }
	name, ok := cleanName(b.Name)
	if !ok {
		bad("names are up to 64 printable characters")
		return
	}
	cwd, ok := resolveDir(b.Cwd)
	if !ok {
		bad("directory not found")
		return
	}
	if b.Run != "" && b.Run != "shell" && b.Run != "claude" {
		bad("run must be shell or claude")
		return
	}
	if b.Dir != "" && b.Dir != "right" && b.Dir != "down" {
		bad("direction must be right or down")
		return
	}
	var pane int
	var err error
	session := b.Session
	switch b.What {
	case "session":
		if !sessionNameRe.MatchString(name) {
			bad("session names use letters, digits, . _ -")
			return
		}
		if s.sessionLive(name) {
			bad("session " + name + " already exists")
			return
		}
		session = name
		if err = s.z.newSession(name, cwd); err == nil {
			err = errors.New("new session did not start")
			for i := 0; i < 50; i++ {
				if ps, e := s.z.panes(name); e == nil && len(ps) > 0 {
					pane, err = ps[0].ID, nil
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	case "tab", "pane":
		if !s.sessionLive(session) {
			bad("no live session " + trunc(session, 64))
			return
		}
		if b.What == "tab" {
			pane, err = s.z.newTab(session, name, cwd)
		} else if b.Tab < 0 {
			bad("bad tab")
			return
		} else {
			pane, err = s.z.newPane(session, b.Tab, name, cwd, b.Dir)
		}
	default:
		bad("what must be session, tab or pane")
		return
	}
	what := fmt.Sprintf("new %s %s %q in %s", b.What, session, name, tilde(cwd))
	if err == nil && b.Run == "claude" {
		what += " running claude-multi" // the owner's launcher (teamclaude); typed like over SSH so aliases work
		if err = s.z.write(session, pane, "claude-multi"); err == nil {
			err = s.z.keys(session, pane, "Enter")
		}
	}
	if err != nil {
		s.audit.add("create-fail", what+": "+err.Error(), clientIP(r))
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	s.term.invalidate(session)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.audit.add("create", what, clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true, "session": session, "pane": pane})
}

// ---- closing sessions, tabs, panes, processes ----

// paneOfProc is the zellij pane a process was started in (its ZELLIJ_PANE_ID), -1 if none.
func paneOfProc(pid int) int {
	b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	for _, kv := range bytes.Split(b, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte("ZELLIJ_PANE_ID=")); ok {
			if n, err := strconv.Atoi(string(v)); err == nil {
				return n
			}
		}
	}
	return -1
}

func (s *server) termClose(w http.ResponseWriter, r *http.Request) {
	var b struct {
		What, Session  string
		Tab, Pane, PID int
	}
	if err := readJSON(r, &b); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	bad := func(msg string) { writeJSON(w, 400, map[string]any{"error": msg}) }
	if !s.sessionLive(b.Session) {
		bad("no live session " + trunc(b.Session, 64))
		return
	}
	var err error
	var what string
	switch b.What {
	case "session":
		what = "close session " + b.Session
		err = s.z.deleteSession(b.Session)
	case "tab":
		if b.Tab < 0 {
			bad("bad tab")
			return
		}
		what = fmt.Sprintf("close tab %s:%d", b.Session, b.Tab)
		err = s.z.closeTab(b.Session, b.Tab)
	case "pane":
		if b.Pane < 0 {
			bad("bad pane")
			return
		}
		what = fmt.Sprintf("close pane %s:%d", b.Session, b.Pane)
		err = s.z.closePane(b.Session, b.Pane)
	case "process":
		if b.PID <= 1 || paneOfProc(b.PID) != b.Pane {
			bad("that process isn't running in this pane")
			return
		}
		what = fmt.Sprintf("kill %d (%s) in %s:%d", b.PID, trunc(procCmdline(b.PID), 80), b.Session, b.Pane)
		if err = s.actor.kill(b.PID); err != nil {
			bad(err.Error())
			return
		}
	default:
		bad("what must be session, tab, pane or process")
		return
	}
	if err != nil {
		s.audit.add("close-fail", what+": "+err.Error(), clientIP(r))
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	s.term.invalidate(b.Session)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.audit.add("close", what, clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- balancer ----

func (s *server) limit(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Limit
		Remove bool
	}
	if err := readJSON(r, &b); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	var err error
	what := trunc(b.Label, 120)
	if b.Remove {
		for _, l := range s.limits.list() {
			if l.Key == b.Key {
				what = l.Label
			}
		}
		what = "remove limit on " + what
		err = s.limits.remove(b.Key)
	} else {
		err = s.limits.set(b.Limit)
		what += ": " + b.Limit.summary()
	}
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	s.audit.add("limit", what, clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- terminal view: raw keystrokes ----

func (s *server) paneWrite(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Session string
		Pane    int
		Data    string // base64 bytes, as a terminal would send them
	}
	if err := readJSONN(r, &b, 16<<10); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	data, err := base64.StdEncoding.DecodeString(b.Data)
	if err != nil || len(data) == 0 || len(data) > 8<<10 || !sessionNameRe.MatchString(b.Session) || len(b.Session) > 64 || b.Pane < 0 {
		writeJSON(w, 400, map[string]any{"error": "bad session, pane or data"})
		return
	}
	if _, ok := s.term.pane(b.Session, b.Pane); !ok { // not in the last snapshot (just created?): ask zellij
		ps, _ := s.z.panes(b.Session)
		found := false
		for _, p := range ps {
			found = found || p.ID == b.Pane
		}
		if !found {
			writeJSON(w, 404, map[string]any{"error": "no such pane"})
			return
		}
	}
	key := fmt.Sprintf("%s:%d", b.Session, b.Pane)
	if err := s.z.writeBytes(b.Session, b.Pane, data); err != nil {
		s.audit.add("terminal-fail", key+": "+err.Error(), clientIP(r))
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	s.mu.Lock()
	audit := time.Since(s.lastWrite[key]) > time.Minute
	if audit {
		s.lastWrite[key] = time.Now()
	}
	s.mu.Unlock()
	if audit {
		s.audit.add("terminal", "typing into "+key, clientIP(r))
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- chat attachments ----

const maxUpload = 25 << 20

var (
	imageExt   = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}
	unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// uploadName keeps a readable, safe version of the original name: base name only, [A-Za-z0-9._-].
func uploadName(orig, sniffed string) string {
	n := strings.TrimLeft(unsafeName.ReplaceAllString(filepath.Base(orig), "_"), "._")
	if n == "" || n == "_" {
		n = "file" + imageExt[sniffed]
	}
	return trunc(n, 80)
}

// paneUpload stores a file (any type, up to 25 MB) where agents can read it; the chat and the
// terminal hand agents its path. Files older than a week are pruned.
func (s *server) paneUpload(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUpload))
	if err != nil {
		writeJSON(w, 413, map[string]any{"error": "files up to 25 MB"})
		return
	}
	if err := os.MkdirAll(s.uploadDir, 0o700); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	if ents, err := os.ReadDir(s.uploadDir); err == nil {
		for _, e := range ents {
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 7*24*time.Hour {
				os.Remove(filepath.Join(s.uploadDir, e.Name()))
			}
		}
	}
	rnd := make([]byte, 8)
	rand.Read(rnd)
	name := uploadName(r.URL.Query().Get("name"), http.DetectContentType(body))
	path := filepath.Join(s.uploadDir, time.Now().Format("20060102-150405")+"-"+hex.EncodeToString(rnd)+"-"+name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	s.audit.add("upload", fmt.Sprintf("%s (%d KB)", filepath.Base(path), len(body)>>10), clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true, "path": path})
}

var uploadNameRe = regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{16}(\.(png|jpg|gif|webp)|-[A-Za-z0-9._-]{1,80})$`)

// uploadGet serves a sent file back to the page: images inline (the chat shows them), anything else
// only as a download. Strict names only.
func (s *server) uploadGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var b []byte
	err := os.ErrNotExist
	if uploadNameRe.MatchString(name) {
		b, err = os.ReadFile(filepath.Join(s.uploadDir, name))
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "private, max-age=86400")
	if ct := http.DetectContentType(b); imageExt[ct] != "" {
		h.Set("Content-Type", ct)
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", "attachment")
	}
	w.Write(b)
}
