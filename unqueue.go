package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Cancelling a queued message: one you sent while Claude was working, waiting in Claude Code's queue for its
// turn to end. Claude Code has no way to drop one: ↑ in its empty input box pulls the whole queue back into
// the box (the transcript notes each message as popAll). So the panel presses ↑, reads from the transcript
// exactly what came out, clears the box (Ctrl+U a line, Backspace to join the next), and puts every other
// message back in the queue, in order. A message Claude took meanwhile isn't among them, so nothing is
// sent twice and nothing is lost.

const queueHint = "Press up to edit queued messages" // Claude Code's empty input box while something is queued

// popped: the messages ↑ took out of the queue, from the transcript lines written since.
func popped(ls [][]byte) []string {
	var out []string
	for _, l := range ls {
		if !bytes.Contains(l, []byte(`"popAll"`)) {
			continue
		}
		var e tEntry
		var c string
		if json.Unmarshal(l, &e) == nil && e.Type == "queue-operation" && e.Operation == "popAll" && json.Unmarshal(e.Content, &c) == nil {
			out = append(out, c)
		}
	}
	return out
}

// unqueuePlan: how many lines to clear from the box (one more, to be sure), what goes back in the queue, and
// whether text was there to cancel (the first message with its text goes).
func unqueuePlan(popped []string, text string) (clear int, back []string, ok bool) {
	want := strings.TrimSpace(text)
	clear = 1
	for _, t := range popped {
		clear += strings.Count(t, "\n") + 1
		if !ok && strings.TrimSpace(t) == want {
			ok = true
			continue
		}
		back = append(back, t)
	}
	return clear, back, ok
}

// inputBox: the text in Claude Code's input box, between the last two rules on the screen.
func inputBox(screen string) string {
	ls := strings.Split(screen, "\n")
	var rules []int
	for i, l := range ls {
		if strings.ContainsRune(l, '─') && borderRe.MatchString(l) {
			rules = append(rules, i)
		}
	}
	if len(rules) < 2 {
		return ""
	}
	return strings.Join(ls[rules[len(rules)-2]+1:rules[len(rules)-1]], "\n")
}

var boxHintRe = regexp.MustCompile(`^(Press up to edit queued messages|Try ".*")$`) // what an empty box shows

// claudeBox: what is typed in Claude Code's input box and not sent: a message it put back when you stopped it,
// or one typed in the terminal. "" when the box is empty (or shows a hint), or the screen isn't its prompt.
func claudeBox(screen string) string {
	ls := strings.Split(inputBox(screen), "\n")
	first := strings.TrimLeft(ls[0], " ")
	if !strings.HasPrefix(first, "❯") {
		return ""
	}
	ls[0] = strings.TrimPrefix(first, "❯")
	for i := range ls {
		ls[i] = strings.TrimSpace(ls[i])
	}
	t := strings.TrimSpace(strings.Join(ls, "\n"))
	if boxHintRe.MatchString(t) {
		return ""
	}
	return t
}

var pastedMarkRe = regexp.MustCompile(`</?pasted_content[^>]*>`)

// restored: whether the box holds a message you sent (Claude Code puts the one it was stopped on back in its
// box), not a prompt it suggests: that shows in an empty box like typed text, but nobody wrote it.
func restored(box string, sent []string) bool {
	flat := func(s string) string { return strings.Join(strings.Fields(pastedMarkRe.ReplaceAllString(s, "")), " ") }
	b := flat(box)
	if b == "" {
		return false
	}
	for _, s := range sent {
		if f := flat(s); f != "" && (strings.HasPrefix(f, b) || strings.HasPrefix(b, f)) {
			return true
		}
	}
	return false
}

// recentSent: the last messages you sent in a transcript, newest last.
func recentSent(path string) []string {
	ls, _, _ := readTail(path, 1<<20)
	items, _ := parseChat(ls)
	var out []string
	for _, it := range items {
		if it.K == "user" {
			out = append(out, it.Text)
		}
	}
	return out[max(0, len(out)-8):]
}

// clearBox empties Claude Code's input box, which holds lines lines (Ctrl+U a line, Backspace to join the
// next), and checks the screen that none of texts is left in it.
func (s *server) clearBox(session string, pane, lines int, texts []string) error {
	for round := 0; round < 3; round++ {
		keys := make([]string, 0, 2*lines+2)
		for range lines + 1 {
			keys = append(keys, "Ctrl u", "Backspace")
		}
		if err := s.z.keys(session, pane, keys...); err != nil {
			return err
		}
		time.Sleep(150 * time.Millisecond)
		screen, _ := s.z.dump(session, pane)
		box, left := inputBox(screen), false
		for _, t := range texts {
			first := strings.TrimSpace(strings.SplitN(t, "\n", 2)[0])
			left = left || (first != "" && strings.Contains(box, trunc(first, 20)))
		}
		if !left {
			return nil
		}
	}
	return errors.New("couldn't empty its input box: see the Terminal tab")
}

// paneTakeBox: Edit on what Claude Code holds in its input box: it comes out (to the page's chat box) and the
// box is emptied, so the next message isn't pasted on top of it and sent as one.
func (s *server) paneTakeBox(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Session string
		Pane    int
	}
	if err := readJSON(r, &b); err != nil || !sessionNameRe.MatchString(b.Session) || b.Pane < 0 {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	screen, err := s.z.dump(b.Session, b.Pane)
	box := ""
	if err == nil {
		box = claudeBox(screen)
	}
	if box == "" {
		writeJSON(w, 200, map[string]any{"text": ""})
		return
	}
	if err := s.clearBox(b.Session, b.Pane, strings.Count(box, "\n")+1, []string{box}); err != nil {
		s.audit.add("input-fail", fmt.Sprintf("%s:%d take box: %v", b.Session, b.Pane, err), clientIP(r))
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	s.audit.add("input", fmt.Sprintf("%s:%d took %s out of its box", b.Session, b.Pane, strconv.Quote(trunc(box, 80))), clientIP(r))
	writeJSON(w, 200, map[string]any{"text": box})
}

func (s *server) paneUnqueue(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Session string
		Pane    int
		Text    string
	}
	if err := readJSONN(r, &b, 1<<20); err != nil || !sessionNameRe.MatchString(b.Session) || b.Pane < 0 || strings.TrimSpace(b.Text) == "" || len(b.Text) > maxUserText {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	p, ok := s.term.pane(b.Session, b.Pane)
	if !ok || p.Agent == nil || p.Agent.path == "" {
		writeJSON(w, 404, map[string]any{"error": "no chat for this pane"})
		return
	}
	fail := func(code int, msg string) {
		s.audit.add("input-fail", fmt.Sprintf("%s:%d unqueue %s: %s", b.Session, b.Pane, strconv.Quote(trunc(b.Text, 80)), msg), clientIP(r))
		writeJSON(w, code, map[string]any{"error": msg})
	}
	if screen, err := s.z.dump(b.Session, b.Pane); err != nil || !strings.Contains(screen, queueHint) {
		fail(409, "Nothing is waiting in its queue now (or its input box has text in it): see the Terminal tab")
		return
	}
	fi, err := os.Stat(p.Agent.path)
	if err != nil {
		fail(502, err.Error())
		return
	}
	if err := s.z.keys(b.Session, b.Pane, "Up"); err != nil {
		fail(502, err.Error())
		return
	}
	var out []string
	for i := 0; i < 25 && len(out) == 0; i++ { // Claude Code notes them all at once, within a moment
		time.Sleep(100 * time.Millisecond)
		ls, _ := readFrom(p.Agent.path, fi.Size())
		out = popped(ls)
	}
	if len(out) == 0 {
		fail(502, "Claude Code didn't hand the queue back: see the Terminal tab")
		return
	}
	clear, back, found := unqueuePlan(out, b.Text)
	if err := s.clearBox(b.Session, b.Pane, clear-1, out); err != nil {
		fail(502, err.Error())
		return
	}
	for _, t := range back {
		time.Sleep(90 * time.Millisecond)
		err := s.z.paste(b.Session, b.Pane, t)
		if err == nil {
			time.Sleep(90 * time.Millisecond)
			err = s.z.keys(b.Session, b.Pane, "Enter")
		}
		if err != nil {
			fail(502, err.Error())
			return
		}
	}
	s.audit.add("input", fmt.Sprintf("%s:%d unqueue %s (%d put back)", b.Session, b.Pane, strconv.Quote(trunc(b.Text, 80)), len(back)), clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true, "cancelled": found})
}

// typedBox: what's typed in Claude Code's box, read from a screen with its colours (dumpANSI). Its hint and the
// prompts it suggests are drawn dim, which the plain screen can't tell from text someone typed: dim isn't typed.
func typedBox(ansi string) string {
	box := claudeBox(ansiRe.ReplaceAllString(ansi, ""))
	if box == "" {
		return ""
	}
	ls := strings.Split(ansi, "\n")
	var rules []int // the box's rules, found on the plain lines (the colours split them up)
	for i, l := range ls {
		if p := ansiRe.ReplaceAllString(l, ""); strings.ContainsRune(p, '─') && borderRe.MatchString(p) {
			rules = append(rules, i)
		}
	}
	if len(rules) < 2 {
		return box
	}
	dim := false
	for _, l := range ls[rules[len(rules)-2]+1 : rules[len(rules)-1]] {
		for len(l) > 0 {
			if m := sgrRe.FindStringSubmatch(l); m != nil {
				ps := strings.Split(m[1], ";")
				for i := 0; i < len(ps); i++ {
					switch ps[i] {
					case "", "0", "22":
						dim = false
					case "2":
						dim = true
					case "38", "48", "58": // a colour: 5;n or 2;r;g;b follow, not attributes
						if i+1 < len(ps) && ps[i+1] == "5" {
							i += 2
						} else if i+1 < len(ps) && ps[i+1] == "2" {
							i += 4
						}
					}
				}
				l = l[len(m[0]):]
				continue
			}
			if loc := ansiRe.FindStringIndex(l); loc != nil && loc[0] == 0 {
				l = l[loc[1]:]
				continue
			}
			r, n := utf8.DecodeRuneInString(l)
			l = l[n:]
			if !dim && !unicode.IsSpace(r) && r != '❯' && r != '\u00a0' {
				return box
			}
		}
	}
	return ""
}

var sgrRe = regexp.MustCompile(`^\x1b\[([0-9;]*)m`)
