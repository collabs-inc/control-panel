package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// zellij drives the zellij CLI. Every call makes the zellij server re-broadcast its state to
// all plugins (see pixel-agents-standalone 7df28cb), so callers keep them rare: list-panes at
// most every 3 s per session while someone watches, everything else only on user actions.
type zellij struct {
	run    func(ctx context.Context, dir string, args ...string) ([]byte, error)
	attach func(session string, cols, rows int) (stop func(), err error) // headless client
}

func newZellij() *zellij { return &zellij{run: zellijRun, attach: attachHeadless} }

var zellijBin = func() string {
	if p, err := exec.LookPath("zellij"); err == nil {
		return p
	}
	return homeDir + "/.local/bin/zellij"
}()

func zellijRun(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, zellijBin, args...)
	cmd.Dir = dir
	cmd.Env = cleanEnv(os.Environ())
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("zellij %s: %v: %s", args[min(len(args)-1, 3)], err, trunc(strings.TrimSpace(stderr.String()), 200))
	}
	return out, nil
}

// attachHeadless attaches a client in a pty (util-linux script) at the session's own size, so
// nothing visibly resizes. zellij 0.45 only creates tabs while some client is attached.
func attachHeadless(session string, cols, rows int) (func(), error) {
	if !sessionNameRe.MatchString(session) { // it goes into script's shell command line
		return nil, errors.New("bad session name")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, "script", "-qfc",
		fmt.Sprintf("stty cols %d rows %d; exec '%s' attach '%s'", cols, rows, zellijBin, session), "/dev/null")
	cmd.Env = append(cleanEnv(os.Environ()), "TERM=xterm-256color")
	cmd.Dir = homeDir
	stdin, _ := cmd.StdinPipe() // held open: on EOF script would end the session
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	return func() { stdin.Close(); cancel(); cmd.Wait() }, nil
}

func (z *zellij) action(session string, args ...string) ([]byte, error) {
	return z.run(context.Background(), "", append([]string{"--session", session, "action"}, args...)...)
}

// zPane is a terminal pane from `list-panes --json --all`.
type zPane struct {
	ID         int    `json:"id"`
	Plugin     bool   `json:"is_plugin"`
	Focused    bool   `json:"is_focused"`
	Floating   bool   `json:"is_floating"`
	Suppressed bool   `json:"is_suppressed"`
	Title      string `json:"title"`
	Exited     bool   `json:"exited"`
	ExitStatus *int   `json:"exit_status"`
	Held       bool   `json:"is_held"`
	Rows       int    `json:"pane_content_rows"`
	Cols       int    `json:"pane_content_columns"`
	Command    string `json:"terminal_command"`
	TabID      int    `json:"tab_id"`
	TabPos     int    `json:"tab_position"`
	TabName    string `json:"tab_name"`
	X          int    `json:"pane_x"` // where it sits in its tab, in cells
	Y          int    `json:"pane_y"`
	W          int    `json:"pane_columns"`
	H          int    `json:"pane_rows"`
}

// tabMark: the status the claude-tab-status plugin appends to a tab's name. The panel shows status itself.
var tabMark = regexp.MustCompile(`\s+[○●⚡⏸✓]\x{FE0F}?$`)

func tabName(s string) string { return tabMark.ReplaceAllString(s, "") }

// panes lists the visible terminal panes (no plugins, no panes suppressed by an in-place replace).
func (z *zellij) panes(session string) ([]zPane, error) {
	out, err := z.action(session, "list-panes", "--json", "--all")
	if err != nil {
		return nil, err
	}
	var all []zPane
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("list-panes: %v", err)
	}
	var ps []zPane
	for _, p := range all {
		if !p.Plugin && !p.Suppressed {
			p.Title, p.TabName, p.Command = trunc(p.Title, 120), trunc(tabName(p.TabName), 80), trunc(p.Command, 200)
			ps = append(ps, p)
		}
	}
	return ps, nil
}

func pid(id int) string { return "terminal_" + strconv.Itoa(id) }

func (z *zellij) paste(session string, pane int, text string) error {
	_, err := z.action(session, "paste", "--pane-id", pid(pane), "--", text)
	return err
}

func (z *zellij) write(session string, pane int, text string) error {
	_, err := z.action(session, "write-chars", "--pane-id", pid(pane), "--", text)
	return err
}

// writeBytes sends raw bytes to the pane's pty (the terminal view's keystrokes, escape sequences included).
func (z *zellij) writeBytes(session string, pane int, data []byte) error {
	args := make([]string, 0, len(data)+3)
	args = append(args, "write", "--pane-id", pid(pane))
	for _, b := range data {
		args = append(args, strconv.Itoa(int(b)))
	}
	_, err := z.action(session, args...)
	return err
}

func (z *zellij) keys(session string, pane int, keys ...string) error {
	_, err := z.action(session, append([]string{"send-keys", "--pane-id", pid(pane)}, keys...)...)
	return err
}

func (z *zellij) dump(session string, pane int) (string, error) {
	out, err := z.action(session, "dump-screen", "--pane-id", pid(pane))
	return string(out), err
}

// dumpANSI: the screen with its colours, which tell what's typed in Claude Code's box from what it suggests.
func (z *zellij) dumpANSI(session string, pane int) (string, error) {
	out, err := z.action(session, "dump-screen", "--ansi", "--pane-id", pid(pane))
	return string(out), err
}

func (z *zellij) closePane(session string, pane int) error {
	_, err := z.action(session, "close-pane", "--pane-id", pid(pane))
	return err
}

// renameTab names one tab by its id (no focus moves); "--": a name may start with "-".
func (z *zellij) renameTab(session string, tab int, name string) error {
	_, err := z.action(session, "rename-tab", "--tab-id", strconv.Itoa(tab), "--", name)
	return err
}

// renameSession gives a session a new name (its panes and what runs in them carry on).
func (z *zellij) renameSession(session, name string) error {
	_, err := z.action(session, "rename-session", name)
	return err
}

func (z *zellij) closeTab(session string, tab int) error {
	_, err := z.action(session, "close-tab-by-id", strconv.Itoa(tab))
	return err
}

// deleteSession kills a running session and removes it (so it isn't offered for resurrection).
func (z *zellij) deleteSession(name string) error {
	_, err := z.run(context.Background(), "", "delete-session", "--force", name)
	return err
}

func (z *zellij) newSession(name, cwd string) error {
	_, err := z.run(context.Background(), cwd, "attach", "--create-background", name)
	return err
}

var termIDRe = regexp.MustCompile(`terminal_(\d+)`)

func (z *zellij) newPane(session string, tab int, name, cwd, dir string) (int, error) {
	args := []string{"new-pane", "--tab-id", strconv.Itoa(tab), "--no-focus", "--cwd", cwd}
	if name != "" {
		args = append(args, "--name", name)
	}
	if dir != "" {
		args = append(args, "--direction", dir)
	}
	out, err := z.action(session, args...)
	if err != nil {
		return 0, err
	}
	m := termIDRe.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("new-pane: no pane id in %q", trunc(string(out), 80))
	}
	return strconv.Atoi(string(m[1]))
}

// newTab opens a tab and returns its first terminal pane.
func (z *zellij) newTab(session, name, cwd string) (int, error) {
	clients, err := z.action(session, "list-clients")
	if err != nil {
		return 0, err
	}
	if strings.Count(strings.TrimSpace(string(clients)), "\n") == 0 { // header only: detached
		cols, rows := 200, 50
		if out, err := z.action(session, "list-tabs", "--json"); err == nil {
			var tabs []struct {
				Cols int `json:"display_area_columns"`
				Rows int `json:"display_area_rows"`
			}
			json.Unmarshal(out, &tabs)
			for _, t := range tabs {
				if t.Cols >= 20 && t.Rows >= 5 {
					cols, rows = min(t.Cols, 500), min(t.Rows, 200)
					break
				}
			}
		}
		stop, err := z.attach(session, cols, rows)
		if err != nil {
			return 0, fmt.Errorf("attach helper client: %v", err)
		}
		defer stop()
		for i := 0; i < 30; i++ { // wait for the client to register
			if out, _ := z.action(session, "list-clients"); strings.Count(strings.TrimSpace(string(out)), "\n") > 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	args := []string{"new-tab", "--cwd", cwd}
	if name != "" {
		args = append(args, "--name", name)
	}
	out, err := z.action(session, args...)
	if err != nil {
		return 0, err
	}
	tab, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("new-tab: no tab id in %q", trunc(string(out), 80))
	}
	for i := 0; i < 30; i++ {
		ps, _ := z.panes(session)
		for _, p := range ps {
			if p.TabID == tab {
				return p.ID, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, errors.New("new tab did not appear")
}

// ---- prompts: permission requests, questions, the startup trust dialog ----

type PromptOpt struct {
	N       string `json:"n,omitempty"` // its number, if the options are numbered
	Label   string `json:"label"`
	Sel     bool   `json:"sel,omitempty"`     // currently highlighted (❯)
	Desc    string `json:"desc,omitempty"`    // the line(s) under it
	Box     bool   `json:"box,omitempty"`     // a multi-select's checkbox…
	Checked bool   `json:"checked,omitempty"` // …ticked
	Text    bool   `json:"text,omitempty"`    // the free-text answer ("Type something."): typing goes into it…
	Value   string `json:"value,omitempty"`   // …and what it holds now
	Next    bool   `json:"next,omitempty"`    // a multi-select's "Next": on to the next question
}

// PromptTab is one question of a dialog that asks several, as its row of tabs shows it.
type PromptTab struct {
	Label string `json:"label"`
	Done  bool   `json:"done,omitempty"`
}

// PromptQA is one answer a dialog asks you to review before it submits them.
type PromptQA struct {
	Q string `json:"q"`
	A string `json:"a"`
}

// Prompt is a question a TUI is blocked on, read off the pane's screen.
type Prompt struct {
	Q      string      `json:"q"`
	Ctx    []string    `json:"ctx,omitempty"`
	Opts   []PromptOpt `json:"opts"`
	Multi  bool        `json:"multi,omitempty"` // checkboxes: Enter ticks one, Next moves on
	Tabs   []PromptTab `json:"tabs,omitempty"`
	Review []PromptQA  `json:"review,omitempty"`
	Sig    string      `json:"sig"`              // identifies this exact question: an answer must name it
	DenyAt int64       `json:"denyAt,omitempty"` // when Claude Code denies it by itself (unix ms): its countdown, no part of the question
}

var (
	optRe    = regexp.MustCompile(`^\s*(?:([❯›>])\s*)?(\d{1,2})\.\s+(\S.*?)\s*$`)
	hintRe   = regexp.MustCompile(`(?i)enter to (confirm|select)|esc to cancel`)
	borderRe = regexp.MustCompile(`^[\s─━═╭╮╰╯┌┐└┘▔]*$`)
	dashRe   = regexp.MustCompile(`^\s*[╌┄┈]+\s*$`)        // a dashed rule inside a dialog: around a command, a file, a plan
	tabsRe   = regexp.MustCompile(`^\s*←\s.*[☐☒✔].*→\s*$`) // Claude Code's questions: "←  ☒ Color  ☐ Pets  ✔ Submit  →"
	boxRe    = regexp.MustCompile(`^\[([ ✔✓xX])\]\s*(.*)$`)
	typeRe   = regexp.MustCompile(`^(Type something\.?|Tell Claude what to change)$`) // a free-text option, empty
	denyRe   = regexp.MustCompile(`Claude Code will automatically deny this request in (?:(\d+):(\d\d)|about (\d+) (second|minute))`)
)

// denyIn reads Claude Code's countdown on a dialog (2.1.300): "… deny this request in 1:38, …", or in
// screen-reader mode "in about 2 minutes".
func denyIn(line string) (time.Duration, bool) {
	m := denyRe.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	if m[1] != "" {
		mins, _ := strconv.Atoi(m[1])
		secs, _ := strconv.Atoi(m[2])
		return time.Duration(mins*60+secs) * time.Second, true
	}
	n, _ := strconv.Atoi(m[3])
	if m[4] == "minute" {
		return time.Duration(n) * time.Minute, true
	}
	return time.Duration(n) * time.Second, true
}

func cleanLine(s string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "│┃"))
}

// labelCol is the rune column where a line's text starts, after an optional ❯ marker.
func labelCol(line string) (col int, marked bool) {
	rs := []rune(line)
	i := 0
	for i < len(rs) && rs[i] == ' ' {
		i++
	}
	if i < len(rs) && (rs[i] == '❯' || rs[i] == '›' || rs[i] == '>') {
		marked = true
		for i++; i < len(rs) && rs[i] == ' '; i++ {
		}
	}
	return i, marked
}

func labelOf(line string) (string, bool) {
	c, marked := labelCol(line)
	return strings.TrimSpace(string([]rune(line)[c:])), marked
}

// parseTabs reads a dialog's questions off its row of tabs: ☐ unanswered, ☒ answered (✔ Submit is the last step).
func parseTabs(line string) []PromptTab {
	var tabs []PromptTab
	var mark rune
	var b strings.Builder
	flush := func() {
		if mark == '☐' || mark == '☒' {
			tabs = append(tabs, PromptTab{Label: trunc(strings.TrimSpace(b.String()), 40), Done: mark == '☒'})
		}
		b.Reset()
	}
	for _, r := range strings.Trim(strings.TrimSpace(line), "←→") {
		if r == '☐' || r == '☒' || r == '✔' {
			flush()
			mark = r
			continue
		}
		b.WriteRune(r)
	}
	flush()
	return tabs
}

// chatBox: the line of Claude Code's chat box ("❯ " and what's typed, flush left). Its dialogs take the box's
// place, so a dialog's words with the box under them are in the conversation (quoted, or a list in a reply).
func chatBox(l string) bool { return strings.HasPrefix(l, "❯") && !optRe.MatchString(l) }

func indentOf(l string) int {
	return utf8.RuneCountInString(l) - utf8.RuneCountInString(strings.TrimLeft(l, " "))
}

// footless finds a dialog Claude Code draws with no footer ("Switch model?" after /model, a plan to approve):
// the lowest highlighted numbered option, under the ▔ edge Claude Code draws in its chat box's place, with no
// chat box under it. It returns the line after its options (its own hints may follow), or -1.
func footless(lines []string) int {
	sel := -1
	for i := len(lines) - 1; i >= 0 && sel < 0; i-- {
		if chatBox(lines[i]) {
			return -1
		}
		if m := optRe.FindStringSubmatch(lines[i]); m != nil && m[1] != "" {
			sel = i
		}
	}
	edge := false
	for i := sel - 1; i >= 0 && !edge && !chatBox(lines[i]); i-- {
		edge = strings.HasPrefix(strings.TrimSpace(lines[i]), "▔")
	}
	if sel < 0 || !edge {
		return -1
	}
	h, ind := sel+1, indentOf(lines[sel])
	for ; h < len(lines); h++ { // the options go on while the lines are options, under one (its description) or blank
		if l := lines[h]; strings.TrimSpace(l) != "" && !optRe.MatchString(l) && indentOf(l) <= ind {
			break
		}
	}
	return h
}

// unwrap joins the lines a dialog's text was wrapped into at the pane's width: a line continues on the next when
// the next one's first word wouldn't have fit after it (it reaches within a word of the dialog's widest line) and
// it doesn't end a sentence. "" separates paragraphs and goes.
func unwrap(ls []string) []string {
	w := 0
	for _, l := range ls {
		w = max(w, utf8.RuneCountInString(l))
	}
	var out []string
	prev := ""
	for _, l := range ls {
		word, _, _ := strings.Cut(l, " ")
		switch {
		case l == "":
		case prev != "" && len(out) > 0 && !strings.ContainsAny(prev[len(prev)-1:], ".?!:") && !strings.HasPrefix(l, "● ") && !strings.HasPrefix(l, "→ ") &&
			utf8.RuneCountInString(prev)+1+utf8.RuneCountInString(word) > w:
			out[len(out)-1] += " " + l
		default:
			out = append(out, l)
		}
		prev = l
	}
	return out
}

// parsePrompt finds the choice list just above an "Enter to select / Esc to cancel" footer (or, with none: on a
// question dialog's review step, at the bottom of the screen; on Claude Code's other footless dialogs, under the
// ▔ edge it draws in its chat box's place): numbered options ("❯ 1. Yes"), or plain ones where ❯ marks the
// highlighted line. Under a numbered option sits its description; Claude Code's questions add a free-text option
// (the last above a rule, "Chat about this" below it; on a plan, "Tell Claude what to change"), checkboxes and a
// Next line when several answers may be picked, and a row of tabs when they ask several questions. Above the list
// sit the question (the lowest sentence with a "?") and its context — a command, a file, a plan — up to the
// dialog's top edge. Nothing with the chat box under it is a dialog: the dialog would be in the box's place.
func parsePrompt(screen string) *Prompt {
	lines := strings.Split(screen, "\n")
	h := -1
	for i := len(lines) - 1; i >= 0 && !chatBox(lines[i]); i-- {
		if hintRe.MatchString(lines[i]) {
			h = i
			break
		}
	}
	for h > 0 && hintRe.MatchString(lines[h-1]) { // a footer wrapped in a narrow pane
		h--
	}
	if h < 0 {
		tabs := false
		for _, l := range lines {
			tabs = tabs || tabsRe.MatchString(l)
		}
		if h = footless(lines); h < 0 && !tabs {
			return nil
		} else if h < 0 {
			h = len(lines)
		}
	}
	e := h - 1
	for e >= 0 && strings.TrimSpace(lines[e]) == "" {
		e--
	}
	if e < 0 {
		return nil
	}
	s := e
	for s > 0 && strings.TrimSpace(lines[s-1]) != "" {
		s--
	}
	p := &Prompt{}
	first := -1
	var opts []PromptOpt
	var under [][]string // the lines under each numbered option, up to the next one
	rule := -1           // the option a rule follows
	for i := s; i <= e; i++ {
		if m := optRe.FindStringSubmatch(lines[i]); m != nil {
			if first < 0 {
				first = i
			}
			o := PromptOpt{N: m[2], Label: m[3], Sel: m[1] != ""}
			if b := boxRe.FindStringSubmatch(o.Label); b != nil {
				p.Multi, o.Box, o.Checked, o.Label = true, true, b[1] != " ", b[2]
			}
			o.Label = trunc(o.Label, 120)
			opts, under = append(opts, o), append(under, nil)
		} else if len(opts) > 0 && strings.ContainsRune(lines[i], '─') && borderRe.MatchString(lines[i]) {
			if rule < 0 {
				rule = len(opts) - 1
			}
		} else if len(opts) > 0 {
			under[len(opts)-1] = append(under[len(opts)-1], lines[i])
		}
	}
	if first >= 0 {
		txt := -1
		if rule >= 0 && rule < len(opts)-1 { // the free-text option is the last above the rule
			txt = rule
		}
		for i, o := range opts {
			if typeRe.MatchString(o.Label) || strings.Contains(strings.Join(strings.Fields(strings.Join(under[i], " ")), " "), "approve with this feedback") {
				txt = i // a plan's feedback option keeps its hint under it once typed into
			}
		}
		for i, o := range opts {
			var ls []string
			for _, l := range under[i] {
				ls = append(ls, strings.TrimSpace(l))
			}
			if i == txt {
				for j, l := range ls {
					if strings.HasPrefix(l, "shift+tab to approve") { // its hint, not what's typed
						ls = ls[:j]
						break
					}
				}
				var next *PromptOpt
				if p.Multi && len(ls) > 0 { // the last line under it is Next
					label, marked := labelOf(under[i][len(ls)-1])
					next, ls = &PromptOpt{Label: trunc(label, 40), Sel: marked, Next: true}, ls[:len(ls)-1]
				}
				o.Text = true
				if !typeRe.MatchString(o.Label) {
					o.Value = trunc(strings.Join(append([]string{o.Label}, ls...), " "), 500)
				}
				p.Opts = append(p.Opts, o)
				if next != nil {
					p.Opts = append(p.Opts, *next)
				}
				continue
			}
			if d := strings.Join(ls, " "); d != o.Label {
				o.Desc = trunc(d, 300)
			}
			p.Opts = append(p.Opts, o)
		}
	} else { // plain: the lines aligned with the highlighted one
		sel := -1
		for i := s; i <= e; i++ {
			if _, marked := labelCol(lines[i]); marked {
				sel = i
				break
			}
		}
		if sel < 0 {
			return nil
		}
		col, _ := labelCol(lines[sel])
		first = sel
		for first > s {
			if c, _ := labelCol(lines[first-1]); c != col {
				break
			}
			first--
		}
		for i := first; i <= e; i++ {
			c, marked := labelCol(lines[i])
			if c != col {
				break
			}
			p.Opts = append(p.Opts, PromptOpt{Label: trunc(strings.TrimSpace(string([]rune(lines[i])[c:])), 120), Sel: marked})
		}
	}
	var above []string // the dialog above the options, top to bottom; "" between its paragraphs
	for i := first - 1; i >= 0 && len(above) < 60; i-- {
		l := lines[i]
		t := cleanLine(l)
		if strings.HasPrefix(t, "▔") || (t != "" && borderRe.MatchString(l) && !strings.HasPrefix(l, " ")) {
			break // its top edge: a rule across the pane, or Claude Code's ▔ (its rules inside are indented)
		}
		if t == "" || borderRe.MatchString(l) || dashRe.MatchString(l) {
			above = append([]string{""}, above...)
			continue
		}
		above = append([]string{t}, above...)
	}
	above = unwrap(above)
	var ctx []string
	for i := 0; i < len(above); i++ {
		switch a := above[i]; {
		case tabsRe.MatchString(a):
			p.Tabs = parseTabs(a)
		case strings.HasPrefix(a, "● ") && i+1 < len(above) && strings.HasPrefix(above[i+1], "→ "):
			p.Review = append(p.Review, PromptQA{Q: trunc(strings.TrimPrefix(a, "● "), 200), A: trunc(strings.TrimPrefix(above[i+1], "→ "), 300)})
			i++
		case denyRe.MatchString(a): // it ticks: in the question it would make every answer stale
			d, _ := denyIn(a)
			p.DenyAt = time.Now().Add(d).UnixMilli()
		default:
			ctx = append(ctx, trunc(a, 300))
		}
	}
	if len(ctx) > 8 {
		ctx = ctx[len(ctx)-8:]
	}
	qi := -1
	for i := len(ctx) - 1; i >= 0; i-- {
		if strings.Contains(ctx[i], "?") {
			qi = i
			break
		}
	}
	if qi >= 0 {
		p.Q = trunc(ctx[qi], 200)
		ctx = append(ctx[:qi], ctx[qi+1:]...)
	}
	if len(ctx) > 6 {
		ctx = ctx[len(ctx)-6:]
	}
	if len(ctx) > 0 {
		p.Ctx = ctx
	}
	sig := sha256.New()
	fmt.Fprintf(sig, "%q %q", p.Q, p.Ctx)
	for _, o := range p.Opts {
		fmt.Fprintf(sig, " %q", o.Label)
	}
	p.Sig = hex.EncodeToString(sig.Sum(nil)[:8])
	return p
}

// Answer is what the page chose for the prompt on screen.
type Answer struct {
	Choose string   // one option, by label
	Text   string   // what the free-text option should say: the answer when it's the one chosen; with a multi-select, an extra one
	Checks []string // a multi-select: the options to leave ticked, by label
}

var errNoText = errors.New("type an answer first")

// keyStep is one write to the pane: keys, or text pasted into the free-text option.
type keyStep struct {
	Keys []string
	Text string
}

// steps turns an answer into what to send. Arrows, never an option's digit: on the free-text option a digit is
// text. The free text is cleared (Ctrl u) before it's pasted. A multi-select ticks with Enter, one option at a
// time, then takes the free text (typing ticks it) and moves on with Next.
func (p *Prompt) steps(a Answer) ([]keyStep, error) {
	pos := 0
	for i, o := range p.Opts {
		if o.Sel {
			pos = i
		}
	}
	moveTo := func(i int, then ...string) []string {
		var k []string
		for ; pos < i; pos++ {
			k = append(k, "Down")
		}
		for ; pos > i; pos-- {
			k = append(k, "Up")
		}
		return append(k, then...)
	}
	text := strings.Join(strings.Fields(a.Text), " ") // one line: a newline would send it
	var st []keyStep
	typeIn := func(i int) {
		st = append(st, keyStep{Keys: moveTo(i, "Ctrl u")})
		if text != "" {
			st = append(st, keyStep{Text: text})
		}
	}
	if !p.Multi || a.Choose != "" { // one option (a multi-select's too: "Chat about this")
		for i, o := range p.Opts {
			switch {
			case o.Label != a.Choose || o.Next || o.Box: // a checkbox is ticked, not chosen
			case !o.Text:
				return []keyStep{{Keys: moveTo(i, "Enter")}}, nil
			case text == "":
				return nil, errNoText
			default:
				typeIn(i)
				return append(st, keyStep{Keys: []string{"Enter"}}), nil
			}
		}
		return nil, errors.New("no such option on screen")
	}
	want := map[string]bool{}
	for _, c := range a.Checks {
		ok := false
		for _, o := range p.Opts {
			ok = ok || (o.Box && !o.Text && o.Label == c)
		}
		if !ok {
			return nil, errors.New("no such option on screen: " + trunc(c, 40))
		}
		want[c] = true
	}
	txt, next := -1, -1
	for i, o := range p.Opts {
		switch {
		case o.Text:
			txt = i
		case o.Next:
			next = i
		case o.Box && o.Checked != want[o.Label]:
			st = append(st, keyStep{Keys: moveTo(i, "Enter")})
		}
	}
	if txt >= 0 && text != p.Opts[txt].Value {
		typeIn(txt)
	}
	if next < 0 {
		return nil, errors.New("no Next on screen")
	}
	return append(st, keyStep{Keys: moveTo(next, "Enter")}), nil
}
