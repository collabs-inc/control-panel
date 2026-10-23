package main

import (
	"bufio"
	"bytes"
	"cmp"
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

// A chat opens on its last chatFirst items, and scrolling up brings chatFirst more at a time: a long
// session's chat opens at once even on a phone (a 45 MB transcript used to send 4000 items, 2.5 MB, first).
const (
	chatWindow = 16 << 20 // how far back the first window may look
	chatFirst  = 300
	chatPage   = 4 << 20 // how far back each earlier page may look
	peekWindow = 2 << 20 // the peek's latest turn: a small read, it asks every few seconds while Claude works
	peekItems  = 120
)

// firstChat: a chat's last items, where "Load earlier" goes on from (before), and where its live tail starts (end).
func firstChat(path string) (items []ChatItem, before, end int64, title string) {
	ls, start, stop := readTail(path, chatWindow)
	items, before, title = tailChat(ls, start, chatFirst, 0)
	return items, before, stop, title
}

// earlierChat: the chatFirst items just before `before`, and where the next page back begins.
func earlierChat(path string, before int64) ([]ChatItem, int64) {
	ls, start := readBefore(path, before, chatPage)
	items, b, _ := tailChat(ls, start, chatFirst, 0)
	return items, b
}

func lastItems(items []ChatItem, n int) []ChatItem { return items[max(0, len(items)-n):] }

// chatStream sends the transcript's tail, then whatever gets appended (polled every second).
// A new sessionId in the same pane (/clear, /resume) resets the chat. A Claude that hasn't had a message
// yet has no transcript (Claude Code writes it with the first): the chat starts empty, so your first
// message can go from there, and switches to the transcript once it appears.
func (s *server) chatStream(w http.ResponseWriter, r *http.Request, session string, pane int) {
	p, ok := s.term.pane(session, pane)
	if !ok || p.Agent == nil {
		writeJSON(w, 404, map[string]any{"error": "no chat for this pane"})
		return
	}
	path, sid := p.Agent.path, p.Agent.sid
	q := r.URL.Query()
	if path == "" && (q.Get("sub") != "" || q.Get("tool") != "") {
		writeJSON(w, 404, map[string]any{"error": "no chat for this pane"})
		return
	}
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
	if path != "" && sub == nil { // the Files panel's list, read ahead: its first scan of a long session takes seconds
		root := ""
		if p.Git != nil {
			root = p.Git.Root
		}
		go s.arts.list(path, root)
	}
	var end int64
	var lastPR string // the PR note last sent: a later batch doesn't repeat it
	var open bool     // a turn under way at the end of what was sent: the next batch's hidden entries ride along in it
	reset := func() {
		if path == "" { // no transcript yet: an empty chat, for now
			e.send(map[string]any{"t": "chat", "reset": true, "items": []ChatItem{}, "before": 0, "title": ""})
			end = 0
			return
		}
		items, before, stop, title := firstChat(path)
		open = turnOpen(items, false)
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
			if q, ok := s.term.pane(session, pane); ok && sub == nil && q.Agent != nil && q.Agent.path != "" && (q.Agent.sid != sid || path == "") {
				path, sid = q.Agent.path, q.Agent.sid // a new session in the pane, or the first message wrote the transcript
				reset()
				continue
			}
			if path == "" {
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
			items, title := parseChatOpen(ls, open) // this batch may begin inside a turn
			open = turnOpen(items, open)
			if items = newPRs(items, &lastPR); len(items) > 0 || title != "" {
				e.send(map[string]any{"t": "chat", "items": lastItems(items, 300), "title": title})
			}
		}
	}
}

// paneChat pages back through the transcript: the chatFirst items before `before` (earlierChat).
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
	if tail := r.URL.Query().Get("tail"); before <= 0 && tail != "" { // the chat's latest items, for the Terminals page's peek:
		var items []ChatItem // "live" (asked every few seconds while Claude works) reads a little, else as the chat opens
		if tail == "live" {
			ls, start, _ := readTail(p.Agent.path, peekWindow)
			items, before, _ = tailChat(ls, start, peekItems, 0)
		} else {
			items, before, _, _ = firstChat(p.Agent.path)
		}
		writeJSON(w, 200, map[string]any{"items": items, "before": before})
		return
	}
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
	items, start := earlierChat(path, before)
	writeJSON(w, 200, map[string]any{"items": items, "before": start})
}

// ---- typing into a pane ----

// maxPaste: a message goes to zellij as one argument, and Linux caps one argument at 128 KiB. The page
// sends anything longer as an attached file instead.
const maxPaste = 100 << 10

func (s *server) paneInput(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Session string
		Pane    int
		Text    string
		Enter   bool
		Keys    []string
		Choose  string  // answer the prompt on screen by option label (or with Answer)…
		Answer  *Answer // …a free-text answer, or a multi-select's ticks…
		Sig     string  // …to exactly this question (Prompt.Sig as the page saw it)
		Clear   bool    // a message: empty Claude Code's input box first (what it held goes)
	}
	if err := readJSONN(r, &b, 1<<20); err != nil { // JSON escapes can grow the text several times over
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	bad := func(msg string) { writeJSON(w, 400, map[string]any{"error": msg}) }
	switch {
	case !sessionNameRe.MatchString(b.Session) || len(b.Session) > 64 || b.Pane < 0:
		bad("bad session or pane")
		return
	case len(b.Text) > maxPaste:
		bad("messages paste up to 100 KB: attach a longer text as a file")
		return
	case !utf8.ValidString(b.Text):
		bad("text isn't UTF-8")
		return
	case len(b.Keys) > 10:
		bad("too many keys")
		return
	case b.Choose != "" && b.Answer == nil:
		b.Answer = &Answer{Choose: b.Choose}
	}
	if a := b.Answer; a != nil {
		long := len(a.Choose) > 200 || len(a.Text) > 2000 || !utf8.ValidString(a.Text) || len(a.Checks) > 20
		for _, c := range a.Checks {
			long = long || len(c) > 200
		}
		switch {
		case long:
			bad("answer too long")
			return
		case b.Sig == "":
			bad("say which question you're answering")
			return
		}
	}
	switch {
	case b.Text == "" && !b.Enter && len(b.Keys) == 0 && b.Answer == nil:
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
	if b.Answer != nil {
		s.answer(w, r, b.Session, b.Pane, b.Sig, *b.Answer, what)
		return
	}
	tp, _ := s.term.pane(b.Session, b.Pane)
	claude := tp.Agent != nil || tp.Kind == "claude"
	if b.Text != "" && b.Enter { // a message is never pasted on top of what Claude Code's box still holds (sent as one)
		if tp.Agent != nil && tp.Agent.path != "" { // a shell has no box to check
			screen, _ := s.z.dump(b.Session, b.Pane)
			if box := claudeBox(screen); box != "" && restored(box, recentSent(tp.Agent.path)) { // a suggestion isn't held against you
				if !b.Clear {
					writeJSON(w, 409, map[string]any{"error": "Claude's input box already has text in it", "box": box})
					return
				}
				if err := s.clearBox(b.Session, b.Pane, strings.Count(box, "\n")+1, []string{box}); err != nil {
					s.audit.add("input-fail", what+": "+err.Error(), clientIP(r))
					writeJSON(w, 502, map[string]any{"error": err.Error()})
					return
				}
				what += " (cleared " + strconv.Quote(trunc(box, 40)) + " from its box first)"
			}
		}
	}
	if len(b.Keys) > 0 {
		what += " keys " + strings.Join(b.Keys, ",")
		err = s.z.keys(b.Session, b.Pane, b.Keys...)
	}
	if err == nil && b.Text != "" {
		what += " " + strconv.Quote(trunc(b.Text, 80))
		if b.Enter && claude {
			var note string
			note, err = s.sendToClaude(b.Session, b.Pane, b.Text)
			what += " ⏎" + note
			b.Enter = false
		} else {
			err = s.z.paste(b.Session, b.Pane, b.Text)
		}
	}
	if err == nil && b.Enter {
		what += " ⏎"
		err = s.z.keys(b.Session, b.Pane, "Enter")
	}
	if err != nil {
		s.audit.add("input-fail", what+": "+err.Error(), clientIP(r))
		code := 502
		if errors.Is(err, errNotTaken) {
			code = 409 // not a retry: it's out of the box again, and the page says it wasn't sent
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	s.audit.add("input", what, clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

var errNotTaken = errors.New("Claude Code didn't take it: it stayed in its input box")

// sendToClaude types a message into Claude Code's input box and checks it went. Claude Code (2.1.296) reads a
// pasted image path in the background and drops an Enter that comes meanwhile (its paste handler holds the
// key, then clears it once the images land), and it attaches only a path pasted as a piece of its own (in
// "a.png b.png text", b.png stays text). So the uploads go in first, alone, and the text once they show in the
// box; then Enter, and a look at the box: still holding the message, Enter again; still there, it comes out.
func (s *server) sendToClaude(session string, pane int, text string) (note string, err error) {
	box := func() string { sc, _ := s.z.dump(session, pane); return claudeBox(sc) }
	files, msg := leadUploads(text, s.uploadDir)
	if len(files) > 0 {
		n := strings.Count(nows(box()), "[Image#")
		if err := s.z.paste(session, pane, strings.Join(files, " ")); err != nil {
			return "", err
		}
		if !waitUntil(8*time.Second, func() bool {
			b := nows(box())
			return strings.Count(b, "[Image#") > n || strings.Contains(b, "/uploads/")
		}) {
			note = " (its photos didn't show in 8 s)" // a read that failed puts the path in as text
		}
		if msg != "" {
			msg = " " + msg
		}
	}
	if msg != "" {
		if err := s.z.paste(session, pane, msg); err != nil {
			return note, err
		}
	}
	for try := range 2 {
		if try > 0 {
			note += " (Enter again: the first didn't take)"
		}
		if err := s.z.keys(session, pane, "Enter"); err != nil {
			return note, err
		}
		if waitUntil(1500*time.Millisecond, func() bool { return !holds(box(), msg, len(files) > 0) }) {
			return note, nil
		}
	}
	b := box()
	if err := s.clearBox(session, pane, strings.Count(b, "\n")+1, []string{b}); err != nil {
		return note, err
	}
	return note, errNotTaken
}

// leadUploads: the uploads the page puts before a message ("<uploads>/x.png <uploads>/y.png text"), and the text.
func leadUploads(text, dir string) (files []string, msg string) {
	for {
		f, rest, _ := strings.Cut(text, " ")
		if filepath.Dir(f) != dir || !uploadNameRe.MatchString(filepath.Base(f)) {
			return files, text
		}
		files, text = append(files, f), rest
	}
}

// holds: whether Claude Code's box still has the message in it. The screen wraps it anywhere (a path mid-word),
// so without spaces; a long one by its start or end (the box may show either), or Claude Code's mark for a paste.
func holds(box, msg string, photos bool) bool {
	b, m := nows(box), []rune(nows(msg))
	switch {
	case b == "":
		return false
	case strings.Contains(b, "[Pastedtext#"):
		return true
	case len(m) == 0:
		return photos && strings.Contains(b, "[Image#")
	case len(m) <= 48:
		return strings.Contains(b, string(m))
	}
	return strings.Contains(b, string(m[:24])) || strings.Contains(b, string(m[len(m)-24:]))
}

func nows(s string) string { return strings.Join(strings.Fields(s), "") }

// waitUntil checks ok every 100 ms for up to d.
func waitUntil(d time.Duration, ok func() bool) bool {
	for end := time.Now().Add(d); ; time.Sleep(100 * time.Millisecond) {
		if ok() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

// answer replies to the question on a pane's screen. It reads the screen first (the highlight may have moved
// since the page last looked), sends the steps with a moment between them (the dialog takes each before the
// next: an arrow changes what typing goes into), then reads it again, so the page can show the next question
// at once instead of after the tracker's next look.
func (s *server) answer(w http.ResponseWriter, r *http.Request, session string, pane int, sig string, a Answer, what string) {
	screen, err := s.z.dump(session, pane)
	var p *Prompt
	if err == nil {
		p = parsePrompt(screen)
	}
	if p == nil || p.Sig != sig {
		writeJSON(w, 409, map[string]any{"error": "that question is no longer on screen"})
		return
	}
	st, err := p.steps(a)
	if err != nil {
		code := 409 // an option that isn't there: the question changed under the page
		if err == errNoText {
			code = 400
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	what += " answer " + strconv.Quote(trunc(cmp.Or(a.Choose, strings.Join(a.Checks, ", ")), 80))
	if a.Text != "" {
		what += " " + strconv.Quote(trunc(a.Text, 80))
	}
	for i, x := range st {
		if i > 0 {
			time.Sleep(90 * time.Millisecond)
		}
		if x.Text != "" {
			err = s.z.paste(session, pane, x.Text)
		} else {
			err = s.z.keys(session, pane, x.Keys...)
		}
		if err != nil {
			s.audit.add("input-fail", what+": "+err.Error(), clientIP(r))
			writeJSON(w, 502, map[string]any{"error": err.Error()})
			return
		}
	}
	s.audit.add("input", what, clientIP(r))
	s.term.reread(session, pane)
	out := map[string]any{"ok": true}
	for range 5 { // the dialog's next state: the next question, the review, or gone
		time.Sleep(120 * time.Millisecond)
		if screen, err := s.z.dump(session, pane); err == nil {
			if next := parsePrompt(screen); next == nil || next.Sig != sig {
				out["moved"], out["prompt"] = true, next
				break
			}
		}
	}
	writeJSON(w, 200, out)
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

// termRename: POST {session, tab?, name}. A tab takes the name (a tab-status mark comes back with that plugin's
// next update); a session takes a free name.
func (s *server) termRename(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Session, Name string
		Tab           *int
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
	name, ok := cleanName(b.Name)
	if !ok || name == "" {
		bad("names are 1 to 64 printable characters")
		return
	}
	var what string
	var err error
	if b.Tab != nil {
		if *b.Tab < 0 {
			bad("bad tab")
			return
		}
		what = fmt.Sprintf("rename tab %s:%d to %q", b.Session, *b.Tab, name)
		err = s.z.renameTab(b.Session, *b.Tab, name)
	} else {
		switch {
		case !sessionNameRe.MatchString(name):
			bad("session names use letters, digits, . _ -")
			return
		case name == b.Session:
			writeJSON(w, 200, map[string]any{"ok": true, "name": name})
			return
		case s.sessionLive(name):
			bad("session " + name + " already exists")
			return
		}
		what = fmt.Sprintf("rename session %s to %s", b.Session, name)
		err = s.z.renameSession(b.Session, name)
	}
	if err != nil {
		s.audit.add("rename-fail", what+": "+err.Error(), clientIP(r))
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	s.term.invalidate(b.Session)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.audit.add("rename", what, clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true, "name": name})
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
	if len(n) > 80 { // shorter, but no "…" (not an upload name) and the extension kept: readers go by it
		ext := filepath.Ext(n)
		if len(ext) > 12 {
			ext = ""
		}
		n = n[:80-len(ext)] + ext
	}
	return n
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

// ---- the Terminals page: a peek at a pane's screen, and moving a tab to another session ----

// panePeek: the last lines on a pane's screen (a server's log, a shell's prompt). An agent's peek reads its chat
// instead (GET /api/pane/chat?tail=live or tail=turn).
func (s *server) panePeek(w http.ResponseWriter, r *http.Request) {
	session, pane, err := paneParams(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	if _, ok := s.term.pane(session, pane); !ok {
		writeJSON(w, 404, map[string]any{"error": "no such pane"})
		return
	}
	screen, err := s.z.dump(session, pane)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	lines := strings.Split(strings.TrimRight(screen, " \n"), "\n")
	for i, l := range lines {
		lines[i] = trunc(strings.TrimRight(l, " "), 400)
	}
	writeJSON(w, 200, map[string]any{"screen": lines[max(0, len(lines)-18):]})
}

// movable says why a tab can't move to another session, or "" when it can. Only what moves without losing
// anything: a one-pane tab whose Claude isn't working or asking, or a shell at its prompt. A running server
// would have to restart, which is the owner's call, not a move's.
func movable(tb TTab) string {
	if len(tb.Panes) != 1 {
		return "Only a tab with one pane can move for now."
	}
	p := tb.Panes[0]
	switch {
	case p.Kind == "codex":
		return "Only Claude agents can move: Codex can't resume here."
	case p.Agent != nil:
		switch p.State {
		case "working", "background", "stalled", "starting":
			return "It's working: move it once it stops."
		case "asking":
			return "It's asking you something: answer it first."
		}
		if p.Agent.sid == "" || p.Agent.startCwd == "" {
			return "Its conversation can't be found to resume it."
		}
	case p.Kind == "claude":
		return "Its agent isn't tracked yet: try again in a moment."
	case p.Fg > 0 || p.State == "running":
		return "What runs in it would restart: stop it, then start it in the other session."
	}
	return ""
}

// termMove moves a tab to another session. Zellij can't carry a pane across sessions, so the panel opens the tab
// again there, then closes this one: a shell opens in the same folder; Claude resumes the same conversation
// (claude-multi --resume, in the folder it started in, where its transcript lives), typed only after the old
// pane has closed, so two Claudes never write one conversation.
func (s *server) termMove(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Session, To string
		Tab         int
	}
	if err := readJSON(r, &b); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	switch {
	case !s.sessionLive(b.Session) || !s.sessionLive(b.To):
		writeJSON(w, 400, map[string]any{"error": "both sessions must be live"})
		return
	case b.Session == b.To:
		writeJSON(w, 400, map[string]any{"error": "it is already in " + b.To})
		return
	}
	tb, ok := s.term.tab(b.Session, b.Tab)
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "no such tab"})
		return
	}
	if why := movable(tb); why != "" {
		writeJSON(w, 409, map[string]any{"error": why})
		return
	}
	p := tb.Panes[0]
	cwd, cmd := p.cwd, ""
	if p.Agent != nil {
		cwd, cmd = p.Agent.startCwd, "claude-multi --resume "+p.Agent.sid
	}
	if fi, err := os.Stat(cwd); cwd == "" || err != nil || !fi.IsDir() {
		writeJSON(w, 409, map[string]any{"error": "its folder is gone, so it can't reopen there"})
		return
	}
	what := fmt.Sprintf("move tab %s:%d %q to %s (%s)", b.Session, b.Tab, tb.Name, b.To, tilde(cwd))
	fail := func(err error) {
		s.audit.add("move-fail", what+": "+err.Error(), clientIP(r))
		writeJSON(w, 502, map[string]any{"error": err.Error()})
	}
	pane, err := s.z.newTab(b.To, tb.Name, cwd)
	if err != nil {
		fail(err)
		return
	}
	if err := s.z.closePane(b.Session, p.ID); err != nil {
		fail(fmt.Errorf("opened it in %s, but the old pane did not close: %v", b.To, err))
		return
	}
	if cmd != "" {
		time.Sleep(moveSettle) // the old Claude takes its hangup and lets go of the conversation
		err = s.z.write(b.To, pane, cmd)
		if err == nil {
			err = s.z.keys(b.To, pane, "Enter")
		}
		if err != nil {
			fail(fmt.Errorf("the tab is in %s, but Claude did not resume: %v", b.To, err))
			return
		}
		what += " resuming " + p.Agent.sid
	}
	s.term.invalidate(b.Session)
	s.term.invalidate(b.To)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.audit.add("move", what, clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true, "session": b.To, "pane": pane})
}

var moveSettle = 800 * time.Millisecond
