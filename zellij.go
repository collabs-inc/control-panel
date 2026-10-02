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

func (z *zellij) closePane(session string, pane int) error {
	_, err := z.action(session, "close-pane", "--pane-id", pid(pane))
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
	N     string `json:"n,omitempty"` // digit shortcut, if the options are numbered
	Label string `json:"label"`
	Sel   bool   `json:"sel,omitempty"` // currently highlighted (❯)
}

// Prompt is a question a TUI is blocked on, read off the pane's screen.
type Prompt struct {
	Q    string      `json:"q"`
	Ctx  []string    `json:"ctx,omitempty"`
	Opts []PromptOpt `json:"opts"`
	Sig  string      `json:"sig"` // identifies this exact question: an answer must name it
}

var (
	optRe    = regexp.MustCompile(`^\s*(?:([❯›>])\s*)?(\d)\.\s+(\S.*?)\s*$`)
	hintRe   = regexp.MustCompile(`(?i)enter to (confirm|select)|esc to cancel`)
	borderRe = regexp.MustCompile(`^[\s─━═╭╮╰╯┌┐└┘]*$`)
)

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

// parsePrompt finds the choice list just above an "Enter to confirm / Esc to cancel" hint: numbered
// options ("❯ 1. Yes"), or plain ones where ❯ marks the highlighted line. Above the list sit the
// question (the lowest line with a "?") and its context, up to the dialog's border.
func parsePrompt(screen string) *Prompt {
	lines := strings.Split(screen, "\n")
	h := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if hintRe.MatchString(lines[i]) {
			h = i
			break
		}
	}
	e := h - 1
	for e >= 0 && strings.TrimSpace(lines[e]) == "" {
		e--
	}
	if h < 0 || e < 0 {
		return nil
	}
	s := e
	for s > 0 && strings.TrimSpace(lines[s-1]) != "" {
		s--
	}
	p := &Prompt{}
	first := -1
	for i := s; i <= e; i++ { // numbered: option lines, skipping description lines under them
		if m := optRe.FindStringSubmatch(lines[i]); m != nil {
			if first < 0 {
				first = i
			}
			p.Opts = append(p.Opts, PromptOpt{N: m[2], Label: trunc(m[3], 120), Sel: m[1] != ""})
		}
	}
	if first < 0 { // plain: the lines aligned with the highlighted one
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
	var ctx []string // the dialog above the options, top to bottom
	for i := first - 1; i >= 0 && len(ctx) < 8; i-- {
		t := cleanLine(lines[i])
		if t == "" {
			continue
		}
		if borderRe.MatchString(lines[i]) {
			break
		}
		ctx = append([]string{trunc(t, 300)}, ctx...)
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
	p.Ctx = ctx
	sig := sha256.New()
	fmt.Fprintf(sig, "%q %q", p.Q, p.Ctx)
	for _, o := range p.Opts {
		fmt.Fprintf(sig, " %q", o.Label)
	}
	p.Sig = hex.EncodeToString(sig.Sum(nil)[:8])
	return p
}

// keysFor answers with the option labelled label: its digit, or arrows from the highlighted line then Enter.
func (p *Prompt) keysFor(label string) ([]string, bool) {
	sel, idx := 0, -1
	for i, o := range p.Opts {
		if o.Sel {
			sel = i
		}
		if o.Label == label {
			idx = i
		}
	}
	if idx < 0 {
		return nil, false
	}
	if n := p.Opts[idx].N; n != "" {
		return []string{n}, true
	}
	var keys []string
	for i := sel; i < idx; i++ {
		keys = append(keys, "Down")
	}
	for i := sel; i > idx; i-- {
		keys = append(keys, "Up")
	}
	return append(keys, "Enter"), true
}
