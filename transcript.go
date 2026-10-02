package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Claude Code transcripts (~/.claude/projects/<slug>/<sessionId>.jsonl) turned into chat items
// for the pane drawer, plus the cheap "what is it doing" and "is the turn over" questions.

// ChatItem is one rendered chat element. K: user, think, text, tool, result, note.
type ChatItem struct {
	K    string `json:"k"`
	Text string `json:"text,omitempty"`
	Name string `json:"name,omitempty"` // tool name
	ID   string `json:"id,omitempty"`   // tool_use id, pairs a result with its tool
	Err  bool   `json:"err,omitempty"`
	T    int64  `json:"t,omitempty"`
	Dur  int64  `json:"dur,omitempty"`  // think: seconds since the previous entry
	In   string `json:"in,omitempty"`   // tool: the full input, for the expanded card
	Img  int    `json:"img,omitempty"`  // user: attached images
	Path string `json:"path,omitempty"` // file tools: the file
	Add  int    `json:"add,omitempty"`  // edit tools: lines added / removed
	Del  int    `json:"del,omitempty"`
	Diff string `json:"diff,omitempty"` // edit tools: " ", "-", "+" prefixed lines; "@@" marks skipped context
	// text: documents the message hands over (named in it, existing, changed lately), shown as file cards
	Files []string `json:"files,omitempty"`
	Mid   bool     `json:"mid,omitempty"` // user: sent while Claude was working, taken into the running turn
}

type tEntry struct {
	Type       string          `json:"type"`
	Cwd        string          `json:"cwd"`
	Subtype    string          `json:"subtype"`
	IsMeta     bool            `json:"isMeta"`
	Timestamp  string          `json:"timestamp"`
	Content    json.RawMessage `json:"content"`
	DurationMs int64           `json:"durationMs"`
	AITitle    string          `json:"aiTitle"`
	PRNumber   int             `json:"prNumber"`
	Operation  string          `json:"operation"` // queue-operation: enqueue, dequeue, remove
	Effort     string          `json:"effort"`    // assistant: the effort level it ran at
	Attachment struct {
		Type   string          `json:"type"`
		Prompt json.RawMessage `json:"prompt"`
	} `json:"attachment"`
	Message struct {
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
		Model      string          `json:"model"`
	} `json:"message"`
}

type tBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Input     map[string]any  `json:"input"`
	Content   json.RawMessage `json:"content"`
}

// blocks decodes message content, which is either a plain string or a list of blocks.
func blocks(raw json.RawMessage) (text string, bs []tBlock) {
	if len(raw) == 0 {
		return "", nil
	}
	if raw[0] == '"' {
		json.Unmarshal(raw, &text)
		return text, nil
	}
	json.Unmarshal(raw, &bs)
	return "", bs
}

var homeDir, _ = os.UserHomeDir()

func tilde(p string) string {
	if homeDir != "" && (p == homeDir || strings.HasPrefix(p, homeDir+"/")) {
		return "~" + p[len(homeDir):]
	}
	return p
}

// toolSummary is the one line that says what a tool call does.
func toolSummary(name string, in map[string]any) string {
	str := func(k string) string { s, _ := in[k].(string); return s }
	var s string
	switch name {
	case "Bash":
		s = str("command")
	case "Read", "Write", "Edit", "MultiEdit", "NotebookEdit":
		s = tilde(str("file_path") + str("notebook_path"))
	case "Grep", "Glob":
		s = str("pattern")
	case "Agent", "Task":
		s = str("description")
	case "WebFetch":
		s = str("url")
	case "WebSearch":
		s = str("query")
	case "Skill":
		s = str("skill")
	default:
		keys := make([]string, 0, len(in))
		for k := range in {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if v, ok := in[k].(string); ok && v != "" {
				s = v
				break
			}
		}
	}
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return trunc(s, 200)
}

// toolInput is the whole input of a tool call: a Bash command as-is, anything else as indented JSON.
func toolInput(in map[string]any) string {
	if c, ok := in["command"].(string); ok {
		return trunc(c, 4000)
	}
	if len(in) == 0 {
		return ""
	}
	b, _ := json.MarshalIndent(in, "", "  ")
	return trunc(string(b), 4000)
}

// editDiff gives file-writing tools (Edit, MultiEdit, Write, NotebookEdit) their diff and +/- counts.
func editDiff(name string, in map[string]any) (path, diff string, add, del int, ok bool) {
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	path = str(in, "file_path") + str(in, "notebook_path")
	var pairs [][2]string
	switch name {
	case "Edit":
		pairs = [][2]string{{str(in, "old_string"), str(in, "new_string")}}
	case "Write":
		pairs = [][2]string{{"", str(in, "content")}}
	case "NotebookEdit":
		pairs = [][2]string{{"", str(in, "new_source")}}
	case "MultiEdit":
		es, _ := in["edits"].([]any)
		for _, e := range es {
			if m, isMap := e.(map[string]any); isMap {
				pairs = append(pairs, [2]string{str(m, "old_string"), str(m, "new_string")})
			}
		}
	default:
		return "", "", 0, 0, false
	}
	var all []string
	for i, p := range pairs {
		ls, a, d := lineDiff(p[0], p[1])
		if i > 0 {
			all = append(all, "@@")
		}
		all = append(all, ls...)
		add, del = add+a, del+d
	}
	if len(all) > 300 {
		all = append(all[:300], "@@")
	}
	return trunc(path, 500), trunc(strings.Join(all, "\n"), 30000), add, del, true
}

func diffLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// lineDiff is a line-level LCS diff: " "/"-"/"+" prefixed lines, unchanged runs trimmed to 2 lines of
// context ("@@" marks a cut). ponytail: O(n·m) table, so past ~400k cells it shows old then new.
func lineDiff(a, b string) (out []string, add, del int) {
	x, y := diffLines(a), diffLines(b)
	var ops []string
	if len(x)*len(y) > 400_000 {
		for _, l := range x {
			ops = append(ops, "-"+l)
		}
		for _, l := range y {
			ops = append(ops, "+"+l)
		}
	} else {
		n, m := len(x), len(y)
		lcs := make([][]int32, n+1)
		for i := range lcs {
			lcs[i] = make([]int32, m+1)
		}
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if x[i] == y[j] {
					lcs[i][j] = lcs[i+1][j+1] + 1
				} else {
					lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && x[i] == y[j]:
				ops = append(ops, " "+x[i])
				i, j = i+1, j+1
			case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
				ops = append(ops, "-"+x[i])
				i++
			default:
				ops = append(ops, "+"+y[j])
				j++
			}
		}
	}
	keep := make([]bool, len(ops))
	for i, o := range ops {
		if o[0] != ' ' {
			if o[0] == '+' {
				add++
			} else {
				del++
			}
			for k := max(0, i-2); k <= min(len(ops)-1, i+2); k++ {
				keep[k] = true
			}
		}
	}
	for i, o := range ops {
		if keep[i] {
			out = append(out, o)
		} else if len(out) == 0 || out[len(out)-1] != "@@" {
			out = append(out, "@@")
		}
	}
	return out, add, del
}

func resultText(raw json.RawMessage) string {
	text, bs := blocks(raw)
	for _, b := range bs {
		switch b.Type {
		case "text":
			text += b.Text
		case "image":
			text += "[image]"
		}
	}
	return trunc(strings.TrimSpace(text), 1500)
}

var (
	summaryRe = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)
	cmdNameRe = regexp.MustCompile(`<command-name>(.*?)</command-name>`)
	stdoutRe  = regexp.MustCompile(`(?s)<local-command-stdout>(.*?)</local-command-stdout>`)
)

// userText turns a typed user string into a user item, or a note for harness noise.
func userText(s string, t int64) (ChatItem, bool) {
	ts := strings.TrimSpace(s)
	switch {
	case ts == "":
		return ChatItem{}, false
	case strings.HasPrefix(ts, "[Request interrupted by user"):
		return ChatItem{K: "note", Text: "Interrupted", T: t}, true
	case strings.HasPrefix(ts, "<task-notification>"):
		if m := summaryRe.FindStringSubmatch(ts); m != nil {
			return ChatItem{K: "note", Text: trunc(strings.TrimSpace(m[1]), 300), T: t}, true
		}
		return ChatItem{K: "note", Text: "Background task finished", T: t}, true
	case strings.Contains(ts, "<command-name>"):
		if m := cmdNameRe.FindStringSubmatch(ts); m != nil {
			return ChatItem{K: "note", Text: trunc(m[1], 100), T: t}, true
		}
		return ChatItem{}, false
	case strings.HasPrefix(ts, "<local-command-stdout>"):
		if m := stdoutRe.FindStringSubmatch(ts); m != nil && strings.TrimSpace(m[1]) != "" {
			return ChatItem{K: "note", Text: trunc(strings.TrimSpace(m[1]), 300), T: t}, true
		}
		return ChatItem{}, false
	}
	return ChatItem{K: "user", Text: trunc(ts, 8000), T: t}, true
}

func unixOf(ts string) int64 {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// fmtDur matches app.js fmtDur: 45s, 2m 8s, 3h 5m, 2d 4h.
func fmtDur(s int64) string {
	if s < 60 {
		return strconv.FormatInt(s, 10) + "s"
	}
	part := func(a int64, ua string, b int64, ub string) string {
		out := strconv.FormatInt(a, 10) + ua
		if b != 0 {
			out += " " + strconv.FormatInt(b, 10) + ub
		}
		return out
	}
	m := s / 60
	if m < 60 {
		return part(m, "m", s%60, "s")
	}
	h := m / 60
	if h < 24 {
		return part(h, "h", m%60, "m")
	}
	return part(h/24, "d", h%24, "h")
}

// parseChat converts transcript lines to chat items; title is the newest ai-title seen.
func parseChat(lines [][]byte) (items []ChatItem, title string) {
	var lastT int64 // the previous timestamped entry: thinking ran from there to its own timestamp
	var model, effort string
	for _, l := range lines {
		var e tEntry
		if json.Unmarshal(l, &e) != nil {
			continue
		}
		t := unixOf(e.Timestamp)
		prevT := lastT
		if t > 0 {
			lastT = t
		}
		switch e.Type {
		case "queue-operation": // messages typed while Claude works wait in a queue until it takes them
			var c string
			json.Unmarshal(e.Content, &c)
			items = append(items, ChatItem{K: "queue", Name: trunc(e.Operation, 20), Text: trunc(c, 4000), T: t})
		case "attachment": // a queued message Claude took into the turn it was working on
			var p string
			if e.Attachment.Type == "queued_command" && json.Unmarshal(e.Attachment.Prompt, &p) == nil && strings.TrimSpace(p) != "" {
				items = append(items, ChatItem{K: "user", Text: trunc(strings.TrimSpace(p), 8000), T: t, Mid: true})
			}
		case "ai-title":
			if e.AITitle != "" {
				title = trunc(e.AITitle, 120)
			}
		case "pr-link":
			if e.PRNumber > 0 {
				items = append(items, ChatItem{K: "note", Text: "PR #" + strconv.Itoa(e.PRNumber), T: t})
			}
		case "system":
			var c string
			json.Unmarshal(e.Content, &c)
			switch e.Subtype {
			case "turn_duration":
				items = append(items, ChatItem{K: "note", Text: "Worked for " + fmtDur(e.DurationMs/1000), T: t})
			case "compact_boundary", "away_summary", "informational":
				if c != "" {
					items = append(items, ChatItem{K: "note", Text: trunc(c, 300), T: t})
				}
			}
		case "user":
			if e.IsMeta {
				continue
			}
			text, bs := blocks(e.Message.Content)
			if it, ok := userText(text, t); ok {
				items = append(items, it)
			}
			imgs, firstUser := 0, -1
			for _, b := range bs {
				switch b.Type {
				case "text":
					if it, ok := userText(b.Text, t); ok {
						if it.K == "user" && firstUser < 0 {
							firstUser = len(items)
						}
						items = append(items, it)
					}
				case "image":
					imgs++
				case "tool_result":
					items = append(items, ChatItem{K: "result", ID: b.ToolUseID, Text: resultText(b.Content), Err: b.IsError, T: t})
				}
			}
			if imgs > 0 { // images ride on the message they came with
				if firstUser < 0 {
					firstUser = len(items)
					items = append(items, ChatItem{K: "user", T: t})
				}
				items[firstUser].Img = imgs
			}
		case "assistant":
			if m := e.Message.Model; m != "" && m != "<synthetic>" && (m != model || e.Effort != effort) { // what it runs on, when that changes
				model, effort = m, e.Effort
				items = append(items, ChatItem{K: "model", Text: trunc(m, 80), Name: trunc(e.Effort, 20), T: t})
			}
			_, bs := blocks(e.Message.Content)
			for _, b := range bs {
				switch b.Type {
				case "thinking": // Claude Code usually saves only a signature: then it's just "Thought for Ns"
					it := ChatItem{K: "think", Text: trunc(strings.TrimSpace(b.Thinking), 8000), T: t}
					if prevT > 0 && t >= prevT {
						it.Dur = t - prevT
					}
					items = append(items, it)
				case "text":
					if s := strings.TrimSpace(b.Text); s != "" {
						items = append(items, ChatItem{K: "text", Text: trunc(s, 16000), T: t, Files: mentionedFiles(s, e.Cwd, homeDir, t)})
					}
				case "tool_use":
					it := ChatItem{K: "tool", Name: trunc(b.Name, 60), ID: b.ID, Text: toolSummary(b.Name, b.Input), T: t}
					if path, dl, add, del, ok := editDiff(b.Name, b.Input); ok {
						it.Path, it.Diff, it.Add, it.Del = path, dl, add, del
					} else {
						it.In = toolInput(b.Input)
						it.Path, _ = b.Input["file_path"].(string)
					}
					items = append(items, it)
				}
			}
		}
	}
	return newPRs(items, new(string)), title
}

// newPRs drops PR notes that repeat the PR already shown (Claude re-records the session's linked PR after
// many of its commands; one transcript had 882 of them) and remembers the last one shown in *last.
func newPRs(items []ChatItem, last *string) []ChatItem {
	out := items[:0]
	for _, it := range items {
		if it.K == "note" && strings.HasPrefix(it.Text, "PR #") {
			if it.Text == *last {
				continue
			}
			*last = it.Text
		}
		out = append(out, it)
	}
	return out
}

// lastTurn reads where the conversation stands: the current tool ("Bash: go test"), "Thinking",
// "Writing", or final=true when the last turn ended (end_turn or an interrupt).
func lastTurn(lines [][]byte) (doing string, final bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		var e tEntry
		if json.Unmarshal(lines[i], &e) != nil || e.IsMeta || (e.Type != "user" && e.Type != "assistant") {
			continue
		}
		text, bs := blocks(e.Message.Content)
		if e.Type == "user" {
			for _, b := range bs {
				text += b.Text
			}
			if strings.HasPrefix(strings.TrimSpace(text), "[Request interrupted by user") {
				return "", true
			}
			return "Thinking", false
		}
		for j := len(bs) - 1; j >= 0; j-- {
			if bs[j].Type == "tool_use" {
				return trunc(bs[j].Name, 60) + ": " + toolSummary(bs[j].Name, bs[j].Input), false
			}
		}
		if e.Message.StopReason == "end_turn" || e.Message.StopReason == "stop_sequence" {
			return "", true
		}
		for _, b := range bs {
			if b.Type == "text" {
				return "Writing", false
			}
		}
		return "Thinking", false
	}
	return "", false
}

// ---- reading jsonl files by byte range; only complete lines are returned ----

// splitLines keeps every line (empty ones too, so byte offsets can be recomputed from lengths).
func splitLines(b []byte) [][]byte {
	if len(b) == 0 {
		return nil
	}
	return bytes.Split(b, []byte{'\n'})
}

// tailChat keeps the chat items written since `since` (unix seconds; 0 keeps all), at most n, from lines
// (which start at byte offset start). A turn that began before `since` is kept from its user message, so
// the chat never opens mid-turn. It returns the offset of the first kept line, so "Load earlier" continues
// exactly where the page begins.
func tailChat(lines [][]byte, start int64, n int, since int64) (items []ChatItem, before int64, title string) {
	offs := make([]int64, len(lines)+1)
	o := start
	for i, l := range lines {
		offs[i] = o
		o += int64(len(l)) + 1
	}
	offs[len(lines)] = o
	i, count, inTurn := len(lines), 0, false
	for i > 0 && count < n {
		it, _ := parseChat(lines[i-1 : i])
		old := false
		for _, x := range it {
			old = old || (x.T > 0 && x.T < since)
		}
		if old && !(inTurn && count > 0) {
			break
		}
		i--
		count += len(it)
		for _, x := range it {
			if x.K == "user" {
				inTurn = false // reached the message that started the oldest kept turn
			} else if x.T > 0 && x.T >= since {
				inTurn = true
			}
		}
	}
	before = offs[i]
	if i == 0 {
		before = start
	}
	items, _ = parseChat(lines[i:]) // in one pass, so thinking gets its duration
	_, title = parseChat(lines)
	return items, before, title
}

func readRange(path string, from, to int64) []byte {
	f, err := os.Open(path)
	if err != nil || to <= from {
		return nil
	}
	defer f.Close()
	b := make([]byte, to-from)
	n, _ := f.ReadAt(b, from)
	return b[:n]
}

// readTail returns the complete lines in the last max bytes; start/end are the byte offsets they span.
func readTail(path string, max int64) (lines [][]byte, start, end int64) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, 0, 0
	}
	from := max64(0, fi.Size()-max)
	b := readRange(path, from, fi.Size())
	start = from
	if from > 0 { // first line is probably cut
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			return nil, fi.Size(), fi.Size()
		}
		b, start = b[i+1:], from+int64(i)+1
	}
	j := bytes.LastIndexByte(b, '\n')
	if j < 0 {
		return nil, start, start
	}
	return splitLines(b[:j]), start, start + int64(j) + 1
}

// readBefore returns the complete lines in the max bytes before off (a line start), and where they begin.
func readBefore(path string, off, max int64) ([][]byte, int64) {
	from := max64(0, off-max)
	b := readRange(path, from, off)
	start := from
	if from > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			return nil, off
		}
		b, start = b[i+1:], from+int64(i)+1
	}
	return splitLines(bytes.TrimSuffix(b, []byte{'\n'})), start // the window ends at a line start
}

// readFrom returns the complete lines after off and the offset just past them (at most 8 MB).
func readFrom(path string, off int64) ([][]byte, int64) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() <= off {
		return nil, off
	}
	b := readRange(path, off, min(fi.Size(), off+8<<20))
	j := bytes.LastIndexByte(b, '\n')
	if j < 0 {
		return nil, off
	}
	return splitLines(b[:j]), off + int64(j) + 1
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ---- finding transcripts ----

type transcripts struct {
	root  string // ~/.claude/projects
	mu    sync.Mutex
	paths map[string]string
	tails map[string]tailInfo
}

type tailInfo struct {
	size  int64
	mtime time.Time
	doing string
	final bool
	title string
}

func newTranscripts(root string) *transcripts {
	return &transcripts{root: root, paths: map[string]string{}, tails: map[string]tailInfo{}}
}

// path finds <root>/*/<sid>.jsonl; the project dir is derived from the cwd Claude started in.
func (t *transcripts) path(sid string) string {
	if !sessionIDRe.MatchString(sid) {
		return ""
	}
	t.mu.Lock()
	p := t.paths[sid]
	t.mu.Unlock()
	if p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	m, _ := filepath.Glob(filepath.Join(t.root, "*", sid+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	t.mu.Lock()
	t.paths[sid] = m[0]
	t.mu.Unlock()
	return m[0]
}

// tail says what the session is doing and its title, re-reading only when the file changed.
func (t *transcripts) tail(sid string) (tailInfo, string) {
	p := t.path(sid)
	fi, err := os.Stat(p)
	if p == "" || err != nil {
		return tailInfo{}, ""
	}
	t.mu.Lock()
	old, ok := t.tails[sid]
	t.mu.Unlock()
	if ok && old.size == fi.Size() && old.mtime.Equal(fi.ModTime()) {
		return old, p
	}
	ls, _, _ := readTail(p, 256<<10)
	if len(ls) == 0 { // one huge line at the end (a big tool result)
		ls, _, _ = readTail(p, 8<<20)
	}
	ti := tailInfo{size: fi.Size(), mtime: fi.ModTime(), title: old.title}
	ti.doing, ti.final = lastTurn(ls)
	if _, title := parseChat(ls); title != "" {
		ti.title = title
	}
	t.mu.Lock()
	t.tails[sid] = ti
	t.mu.Unlock()
	return ti, p
}

// ---- subagents ----

// Subagent is a Claude subagent (Agent/Task tool) of a live session.
type Subagent struct {
	ID      string `json:"id"`
	Desc    string `json:"desc"`
	Type    string `json:"type"`
	Model   string `json:"model,omitempty"`
	BG      bool   `json:"bg"`
	Doing   string `json:"doing"`
	Started int64  `json:"started"`
	Active  int64  `json:"active"`
}

type subagentCache struct {
	mu sync.Mutex
	m  map[string]subEntry
}

type subEntry struct {
	size  int64
	mtime time.Time
	sub   Subagent
	final bool
}

// ponytail: a subagent whose transcript hasn't moved for 60 min drops off (a tool call that long, or an abandoned one).
const subagentQuiet = 60 * time.Minute

// list returns the running subagents in dir (<transcript dir>/<sessionId>/subagents), oldest first.
// Running: its last turn isn't finished and it wrote within subagentQuiet. The caller checks the parent is alive.
func (c *subagentCache) list(dir string, now time.Time) []Subagent {
	files, _ := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]subEntry{}
	}
	var out []Subagent
	for _, p := range files {
		fi, err := os.Stat(p)
		if err != nil || now.Sub(fi.ModTime()) > subagentQuiet {
			continue
		}
		e, ok := c.m[p]
		if !ok || e.size != fi.Size() || !e.mtime.Equal(fi.ModTime()) {
			id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "agent-"), ".jsonl")
			e = subEntry{size: fi.Size(), mtime: fi.ModTime(), sub: Subagent{ID: trunc(id, 40), Active: fi.ModTime().Unix()}}
			metaPath := strings.TrimSuffix(p, ".jsonl") + ".meta.json"
			var meta struct{ AgentType, Description, Model, RequestShape string }
			if b, err := os.ReadFile(metaPath); err == nil {
				json.Unmarshal(b, &meta)
			}
			e.sub.Desc, e.sub.Type, e.sub.Model = trunc(meta.Description, 120), trunc(meta.AgentType, 40), trunc(meta.Model, 40)
			e.sub.BG = meta.RequestShape == "background"
			e.sub.Started = e.sub.Active
			if mfi, err := os.Stat(metaPath); err == nil {
				e.sub.Started = mfi.ModTime().Unix()
			}
			ls, _, _ := readTail(p, 256<<10)
			if len(ls) == 0 {
				ls, _, _ = readTail(p, 8<<20)
			}
			e.sub.Doing, e.final = lastTurn(ls)
			c.m[p] = e
		}
		if !e.final {
			out = append(out, e.sub)
		}
	}
	for p := range c.m { // forget files that went quiet or vanished
		if fi, err := os.Stat(p); err != nil || now.Sub(fi.ModTime()) > subagentQuiet {
			delete(c.m, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Started != out[j].Started {
			return out[i].Started < out[j].Started
		}
		return out[i].ID < out[j].ID
	})
	return out
}

var (
	subIDRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	toolUseRe = regexp.MustCompile(`^toolu_[A-Za-z0-9_-]{1,80}$`)
)

// subagentPath finds a subagent's transcript beside its parent's (<parent>/subagents/agent-<id>.jsonl), by
// the subagent's id or by the parent's Agent tool call that started it (its meta file names the call).
// Neither id becomes a path unless it is a plain token, so a request can't walk out of that folder.
func subagentPath(parent, id, tool string) (string, Subagent, error) {
	dir := filepath.Join(strings.TrimSuffix(parent, ".jsonl"), "subagents")
	var m Subagent
	switch {
	case id != "" && subIDRe.MatchString(id):
	case id == "" && toolUseRe.MatchString(tool):
		metas, _ := filepath.Glob(filepath.Join(dir, "agent-*.meta.json"))
		for _, f := range metas {
			var meta struct{ ToolUseID string }
			if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &meta) == nil && meta.ToolUseID == tool {
				id = strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "agent-"), ".meta.json")
				break
			}
		}
		if id == "" {
			return "", m, errors.New("no subagent for that tool call")
		}
	default:
		return "", m, errors.New("bad subagent")
	}
	p := filepath.Join(dir, "agent-"+id+".jsonl")
	fi, err := os.Stat(p)
	if err != nil {
		return "", m, errors.New("no such subagent")
	}
	var meta struct{ AgentType, Description, Model, RequestShape string }
	if b, err := os.ReadFile(strings.TrimSuffix(p, ".jsonl") + ".meta.json"); err == nil {
		json.Unmarshal(b, &meta)
	}
	m = Subagent{ID: id, Desc: trunc(meta.Description, 120), Type: trunc(meta.AgentType, 40), Model: trunc(meta.Model, 40),
		BG: meta.RequestShape == "background", Active: fi.ModTime().Unix()}
	return p, m, nil
}

// ---- files a message hands over ----

var (
	docFileRe = regexp.MustCompile(`^[\w@%+=~./-]*[\w@%+=-]\.(?i:html?|md|markdown|pdf|png|jpe?g|gif|webp|svg|csv|tsv|xlsx?)$`)
)

// mentionedFiles finds the documents a message names (absolute, ~/ or relative to the session's cwd)
// that exist and changed in the six hours before it: what the agent made, not every file it cites.
func mentionedFiles(text, cwd, home string, at int64) []string {
	var out []string
	seen := map[string]bool{}
	for _, tok := range strings.FieldsFunc(text, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune("`'\"()[]<>*|", r) }) {
		tok = strings.TrimRight(tok, ".,:;!?")
		if !docFileRe.MatchString(tok) {
			continue
		}
		p := tok
		switch {
		case strings.HasPrefix(p, "~/"):
			p = filepath.Join(home, p[2:])
		case !filepath.IsAbs(p):
			if cwd == "" {
				continue
			}
			p = filepath.Join(cwd, p)
		}
		p = filepath.Clean(p)
		fi, err := os.Stat(p)
		if seen[p] || err != nil || !fi.Mode().IsRegular() || (at > 0 && fi.ModTime().Unix() < at-6*3600) {
			continue
		}
		seen[p] = true
		if out = append(out, p); len(out) == 8 {
			break
		}
	}
	return out
}
