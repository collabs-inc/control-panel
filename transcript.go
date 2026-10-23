package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"html"
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
	Out  string `json:"out,omitempty"`  // note of a monitor's event: what it reported
	Img  int    `json:"img,omitempty"`  // user: attached images
	Path string `json:"path,omitempty"` // file tools: the file
	Add  int    `json:"add,omitempty"`  // edit tools: lines added / removed
	Del  int    `json:"del,omitempty"`
	Diff string `json:"diff,omitempty"` // edit tools: " ", "-", "+" prefixed lines; "@@" marks skipped context
	// text: documents the message hands over (named in it, existing, changed lately), shown as file cards
	Files []string `json:"files,omitempty"`
	Mid   bool     `json:"mid,omitempty"` // user: sent while Claude was working, taken into the running turn
	// user: what started the turn when it wasn't you: "peer:<name>" (another Claude session's message), "hook"
	// (a Stop hook's feedback), "auto" (a scheduled or looping prompt)
	Via string `json:"via,omitempty"`
}

type tEntry struct {
	Type       string          `json:"type"`
	Cwd        string          `json:"cwd"`
	Subtype    string          `json:"subtype"`
	IsMeta     bool            `json:"isMeta"`
	IsAPIError bool            `json:"isApiErrorMessage"` // Claude Code's own message for a request that failed for good
	Sidechain  bool            `json:"isSidechain"`
	Timestamp  string          `json:"timestamp"`
	Content    json.RawMessage `json:"content"`
	DurationMs int64           `json:"durationMs"`
	AITitle    string          `json:"aiTitle"`
	PRNumber   int             `json:"prNumber"`
	Operation  string          `json:"operation"` // queue-operation: enqueue, dequeue, remove
	Effort     string          `json:"effort"`    // assistant: the effort level it ran at
	Compact    struct {
		PreTokens, PostTokens int64
	} `json:"compactMetadata"` // system compact_boundary: what the context held before /compact and holds after
	Attachment struct {
		Type   string          `json:"type"`
		Prompt json.RawMessage `json:"prompt"`
	} `json:"attachment"`
	Message struct {
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
		Model      string          `json:"model"`
		Usage      struct {
			Input      int64 `json:"input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Origin *struct {
		Kind, Name, Body string
	} `json:"origin"` // who sent a user entry: "human", or "peer" (another Claude session, with its name and words)
	CompactSummary bool `json:"isCompactSummary"` // the summary a compacted conversation carries on from
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
	summaryRe    = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)
	taskStatusRe = regexp.MustCompile(`<status>([a-z_]{1,20})</status>`)
	eventRe      = regexp.MustCompile(`(?s)<event>(.*?)</event>`)
	cmdNameRe    = regexp.MustCompile(`<command-name>(.*?)</command-name>`)
	effortCmdRe  = regexp.MustCompile(`<command-name>/effort</command-name>[\s\S]{0,200}?<command-args>\s*(low|medium|high|xhigh|max)\s*</command-args>`)
	stdoutRe     = regexp.MustCompile(`(?s)<local-command-stdout>(.*?)</local-command-stdout>`)
	ansiRe       = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`) // a command's terminal colours (/compact dims its line)
)

// maxUserText: what you send shows whole (the page folds a long one); past this, a paste is a document.
const maxUserText = 200 << 10

// userText turns a typed user string into a user item, or a note for harness noise.
var imgSourceRe = regexp.MustCompile(`^\[Image: source: (.+)\]$`) // Claude Code's note of where an image it attached came from

// imageSources: the paths an entry's "[Image: source: …]" blocks name (all of its text, or nothing).
func imageSources(content json.RawMessage) []string {
	_, bs := blocks(content)
	var out []string
	for _, b := range bs {
		m := imgSourceRe.FindStringSubmatch(strings.TrimSpace(b.Text))
		if b.Type != "text" || m == nil {
			return nil
		}
		out = append(out, m[1])
	}
	return out
}

func userText(s string, t int64) (ChatItem, bool) {
	ts := strings.TrimSpace(s)
	switch {
	case ts == "":
		return ChatItem{}, false
	case strings.HasPrefix(ts, "[Request interrupted by user"):
		return ChatItem{K: "note", Text: "Interrupted", T: t}, true
	case strings.HasPrefix(ts, "<task-notification>"): // Claude Code telling the agent a background task moved: a note, with how it went
		name := "task:event" // a monitor's event: no status
		if m := taskStatusRe.FindStringSubmatch(ts); m != nil {
			name = "task:" + m[1]
		}
		if m := summaryRe.FindStringSubmatch(ts); m != nil {
			it := ChatItem{K: "note", Name: name, Text: trunc(html.UnescapeString(strings.TrimSpace(m[1])), 300), T: t}
			if ev := eventRe.FindStringSubmatch(ts); ev != nil {
				it.Out = trunc(html.UnescapeString(strings.TrimSpace(ev[1])), 600)
			}
			return it, true
		}
		return ChatItem{K: "note", Name: name, Text: "Background task finished", T: t}, true
	case ts == compactCmd: // the compactor's: the "Compacted" line says it
		return ChatItem{}, false
	case strings.Contains(ts, "<command-name>"):
		if m := cmdNameRe.FindStringSubmatch(ts); m != nil && m[1] == "/compact" { // the "Compacted" line says it
			return ChatItem{}, false
		} else if m != nil {
			return ChatItem{K: "note", Text: trunc(m[1], 100), T: t}, true
		}
		return ChatItem{}, false
	case strings.HasPrefix(ts, "<local-command-stdout>"):
		if m := stdoutRe.FindStringSubmatch(ts); m != nil && strings.TrimSpace(m[1]) != "" && !strings.HasPrefix(ansiRe.ReplaceAllString(m[1], ""), "Compacted") {
			return ChatItem{K: "note", Text: trunc(strings.TrimSpace(ansiRe.ReplaceAllString(m[1], "")), 300), T: t}, true
		}
		return ChatItem{}, false
	}
	if rest, ok := strings.CutPrefix(ts, panelMark); ok { // the compactor's note or its "continue"
		return ChatItem{K: "user", Text: trunc(strings.TrimSpace(rest), maxUserText), Via: "panel", T: t}, true
	}
	return ChatItem{K: "user", Text: trunc(ts, maxUserText), T: t}, true
}

// fmtTok: 612k, 1.2M.
func fmtTok(n int64) string {
	switch {
	case n >= 1e6:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1000:
		return strconv.FormatInt((n+500)/1000, 10) + "k"
	}
	return strconv.FormatInt(n, 10)
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
func parseChat(lines [][]byte) (items []ChatItem, title string) { return parseChatOpen(lines, false) }

// turnOpen: is a turn under way after these items (else open, when they say nothing): a turn ends with its
// "Worked for" note. The live chat parses each new batch of lines on its own and carries this across.
func turnOpen(items []ChatItem, open bool) bool {
	for i := len(items) - 1; i >= 0; i-- {
		switch it := items[i]; {
		case it.K == "queue" || it.K == "model":
		case it.K == "note" && strings.HasPrefix(it.Text, "Worked for "):
			return false
		default:
			return true
		}
	}
	return open
}

// parseChatOpen parses lines that may begin inside a turn (open): a hidden entry then rides along, it doesn't
// start one.
func parseChatOpen(lines [][]byte, open bool) (items []ChatItem, title string) {
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
			items = append(items, ChatItem{K: "queue", Name: trunc(e.Operation, 20), Text: trunc(c, maxUserText), T: t})
		case "attachment": // a queued message Claude took into the turn it was working on
			var p string
			if e.Attachment.Type == "queued_command" && json.Unmarshal(e.Attachment.Prompt, &p) == nil && strings.TrimSpace(p) != "" {
				if it, ok := userText(p, t); ok { // yours, taken mid-turn; or a task's notice, which is a note
					it.Mid = it.K == "user"
					items = append(items, it)
				}
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
				open = false
			case "compact_boundary", "away_summary", "informational":
				if e.Subtype == "compact_boundary" && e.Compact.PreTokens > 0 && e.Compact.PostTokens > 0 {
					c = "Compacted " + fmtTok(e.Compact.PreTokens) + " → " + fmtTok(e.Compact.PostTokens)
				}
				if c != "" {
					items = append(items, ChatItem{K: "note", Text: trunc(c, 300), T: t})
				}
			}
		case "user":
			if e.IsMeta { // Claude Code's own; one kind rides on the message before it: where the images it attached came from
				if !open { // between turns it's what starts the next one: another session's message, a scheduled prompt, a hook
					if it, ok := triggerItem(e, t); ok {
						items = append(items, it)
						open = true
						continue
					}
				}
				if srcs := imageSources(e.Message.Content); len(srcs) > 0 {
					for i := len(items) - 1; i >= 0; i-- {
						if items[i].K == "user" {
							items[i].Text = strings.TrimSpace(items[i].Text + "\n" + strings.Join(srcs, "\n"))
							break
						}
					}
				}
				continue
			}
			open = true
			text, bs := blocks(e.Message.Content)
			if e.CompactSummary { // not something you wrote: what Claude kept, after the "Compacted" line
				items = append(items, ChatItem{K: "user", Text: trunc(strings.TrimSpace(text), maxUserText), Via: "summary", T: t})
				continue
			}
			if it, ok := userText(text, t); ok {
				items = append(items, it)
			}
			if m := effortCmdRe.FindStringSubmatch(text); m != nil && model != "" && m[1] != effort { // it applies now; the next reply only confirms it
				effort = m[1]
				items = append(items, ChatItem{K: "model", Text: trunc(model, 80), Name: effort, T: t})
			}
			imgs, firstUser := 0, -1
			var srcs []string // where the images Claude Code attached came from (a path you pasted): they ride on the message
			for _, b := range bs {
				switch b.Type {
				case "text":
					if m := imgSourceRe.FindStringSubmatch(strings.TrimSpace(b.Text)); m != nil {
						srcs = append(srcs, m[1])
						continue
					}
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
			if imgs > 0 || len(srcs) > 0 { // images ride on the message they came with (the page shows the uploads among them)
				if firstUser < 0 {
					firstUser = len(items)
					items = append(items, ChatItem{K: "user", T: t})
				}
				items[firstUser].Img = imgs
				if len(srcs) > 0 {
					items[firstUser].Text = strings.TrimSpace(items[firstUser].Text + "\n" + strings.Join(srcs, "\n"))
				}
			}
		case "assistant":
			open = true
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

// triggerItem: a hidden (isMeta) user entry that starts a turn, shown as what started it. What only rides along
// (an image's details, a command's caveat, a system reminder, a skill's text) isn't one.
func triggerItem(e tEntry, t int64) (ChatItem, bool) {
	if e.Origin != nil && e.Origin.Kind == "peer" {
		body := strings.TrimSpace(e.Origin.Body)
		if body == "" {
			body, _ = blocks(e.Message.Content)
		}
		return ChatItem{K: "user", Text: trunc(strings.TrimSpace(body), maxUserText), Via: "peer:" + trunc(e.Origin.Name, 40), T: t}, true
	}
	text, _ := blocks(e.Message.Content)
	ts := strings.TrimSpace(text)
	for _, p := range []string{"[Image", "<local-command", "<system-reminder>", "<command-", "Base directory for this skill", "Caveat:", "[Your previous response"} {
		if ts == "" || strings.HasPrefix(ts, p) {
			return ChatItem{}, false
		}
	}
	via := "auto"
	if strings.HasPrefix(ts, "Stop hook feedback") {
		via = "hook"
	}
	return ChatItem{K: "user", Text: trunc(ts, maxUserText), Via: via, T: t}, true
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
				if b.Type == "tool_result" && bytes.HasPrefix(bytes.Trim(b.Content, `"`), []byte("Structured output provided successfully")) {
					return "", true // a workflow agent hands back its result and ends there, with no closing reply
				}
			}
			if strings.HasPrefix(strings.TrimSpace(text), "[Request interrupted by user") {
				return "", true
			}
			return "Thinking", false
		}
		if e.IsAPIError { // an API error or a refusal ends the turn: nothing follows it
			return "", true
		}
		for j := len(bs) - 1; j >= 0; j-- {
			if bs[j].Type == "tool_use" {
				return trunc(bs[j].Name, 60) + ": " + toolSummary(bs[j].Name, bs[j].Input), false
			}
		}
		switch e.Message.StopReason {
		case "end_turn", "stop_sequence", "refusal":
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

// contextUse is how much of the context window the latest request filled, the way Claude Code counts it
// for its status line: input + cache-write + cache-read tokens of the last real (not synthetic, not a
// subagent's) response.
func contextUse(lines [][]byte) (used int64, model string) {
	for i := len(lines) - 1; i >= 0; i-- {
		var e tEntry
		if json.Unmarshal(lines[i], &e) != nil {
			continue
		}
		if e.Type == "system" && e.Subtype == "compact_boundary" && used == 0 && e.Compact.PostTokens > 0 {
			used = e.Compact.PostTokens // no request since /compact: the compacted size; the model is still the last one's
			continue
		}
		if e.Type != "assistant" || e.Sidechain || e.IsAPIError || e.Message.Model == "<synthetic>" {
			continue
		}
		u := e.Message.Usage
		if n := u.Input + u.CacheWrite + u.CacheRead; n > 0 {
			return cmp.Or(used, n), e.Message.Model
		}
	}
	return used, ""
}

// contextWindow: the Claude 5 models and any "[1m]" model run with a 1M window; others 200k. Claude Code
// takes the size from a model catalog it fetches; seen in these transcripts: Opus 5.5 up to 967k tokens,
// Sonnet 5.5 311k, Haiku 4.5 43k. A request that used more than 200k proves the bigger window anyway.
func contextWindow(model string, used int64) int64 {
	m := strings.ToLower(model)
	if strings.Contains(m, "[1m]") || strings.HasPrefix(m, "claude-opus-5") || strings.HasPrefix(m, "claude-sonnet-5") ||
		strings.HasPrefix(m, "claude-fable-5") || used > 200_000 {
		return 1_000_000
	}
	return 200_000
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
	return items, before, newestTitle(lines)
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
	prs   prScans // every PR each session linked
}

type tailInfo struct {
	size  int64
	mtime time.Time
	doing string
	final bool
	title string
	ctx   int64 // tokens the latest request filled
	model string
	cwd   string // where its shell is now: Claude Code writes it on every entry, and a `cd` moves it
	// Claude's last words: the Terminals page reads in them what a stopped agent needs from you
	claude   string
	claudeAt int64
	asks     bool  // and they end by asking you something
	deep     bool  // looked past the last 256 KB for them once
	compacts int64 // when it was last compacted (unix)
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
	ti := tailInfo{size: fi.Size(), mtime: fi.ModTime(), title: old.title, ctx: old.ctx, model: old.model, cwd: old.cwd,
		claude: old.claude, claudeAt: old.claudeAt, asks: old.asks, deep: old.deep, compacts: old.compacts}
	ti.doing, ti.final = lastTurn(ls)
	for i := len(ls) - 1; i >= 0; i-- {
		var e tEntry
		if json.Unmarshal(ls[i], &e) == nil && e.Cwd != "" && !e.Sidechain {
			ti.cwd = e.Cwd
			break
		}
	}
	for i := len(ls) - 1; i >= 0; i-- {
		var e tEntry
		if bytes.Contains(ls[i], []byte(`"compact_boundary"`)) && json.Unmarshal(ls[i], &e) == nil && e.Subtype == "compact_boundary" {
			ti.compacts = max(ti.compacts, unixOf(e.Timestamp))
			break
		}
	}
	if n, m := contextUse(ls); n > 0 { // not in the last 256 KB (one huge tool result): keep the last known
		ti.ctx, ti.model = n, cmp.Or(m, ti.model)
	}
	if title := newestTitle(ls); title != "" {
		ti.title = title
	}
	ti.latest(ls)
	// a long run of tool calls (a few image reads fill 256 KB) can hide the last words: look further back, once
	if !old.deep && ti.claude == "" {
		ti.deep = true
		if big, _, _ := readTail(p, 8<<20); len(big) > len(ls) {
			ti.latest(big)
		}
	}
	t.mu.Lock()
	t.tails[sid] = ti
	t.mu.Unlock()
	return ti, p
}

// latest keeps Claude's newest words in lines (the older value stays when they have none): parseChat's last text
// item, found from the end instead of parsing every line.
func (ti *tailInfo) latest(lines [][]byte) {
	for i := len(lines) - 1; i >= 0; i-- {
		var e tEntry
		if !bytes.Contains(lines[i], []byte(`"assistant"`)) || json.Unmarshal(lines[i], &e) != nil || e.Type != "assistant" {
			continue
		}
		_, bs := blocks(e.Message.Content)
		for j := len(bs) - 1; j >= 0; j-- {
			if s := strings.TrimSpace(bs[j].Text); bs[j].Type == "text" && s != "" {
				text := trunc(s, 16000) // as parseChat's item holds it
				ti.claude, ti.claudeAt, ti.asks = trunc(text, 1500), unixOf(e.Timestamp), asksYou(text)
				return
			}
		}
	}
}

// newestTitle is parseChat's title (the newest non-empty ai-title), found from the end.
func newestTitle(lines [][]byte) string {
	for i := len(lines) - 1; i >= 0; i-- {
		var e tEntry
		if bytes.Contains(lines[i], []byte(`"ai-title"`)) && json.Unmarshal(lines[i], &e) == nil && e.Type == "ai-title" && e.AITitle != "" {
			return trunc(e.AITitle, 120)
		}
	}
	return ""
}

var (
	askRe   = regexp.MustCompile(`(?i)\b(should I|want me to|shall I|let me know|your call|do you want|which one|approve|confirm)\b`)
	blockRe = regexp.MustCompile(`(?i)\b(can'?t|cannot|won'?t|unable to)\b[^.]*\buntil you\b|\bblocked\b|\bneeds? you to\b|\brequires? you\b|\bonly you can\b`)
)

// asksYou: whether a reply ends by asking you something (a question, "want me to…", or it can't go on without
// you), as the page's askOf reads it, from its last few lines: the compactor waits for your answer then.
func asksYou(text string) bool {
	r := []rune(strings.TrimSpace(text))
	t := string(r[max(0, len(r)-400):])
	return strings.Contains(t, "?") || askRe.MatchString(t) || blockRe.MatchString(t)
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
	WF      string `json:"wf,omitempty"`    // the workflow run it belongs to (wf_…), if any
	Phase   string `json:"phase,omitempty"` // its phase in that workflow
	path    string // its transcript: its own background commands are read from it
}

// Workflow is a Claude Code workflow run of a session that is still going: its agents' transcripts sit in
// <session>/subagents/workflows/<run>/, and <session>/workflows/<run>.json appears once it finishes.
type Workflow struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Done    int    `json:"done"`  // agents that finished
	Total   int    `json:"total"` // agents started so far
	Started int64  `json:"started"`
}

type subagentCache struct {
	mu   sync.Mutex
	m    map[string]subEntry
	done map[string]int64     // a workflow agent seen finished, at this size: it doesn't resume, so no re-read
	ends map[string]*taskEnds // per parent transcript
}

// taskEnds: the background tasks a parent transcript has been told ended, and the background commands
// and monitors it started, read up to off.
type taskEnds struct {
	off   int64
	ids   map[string]bool  // ended
	calls map[string]Shell // background tool calls waiting for their result, by tool_use id
	runs  map[string]Shell // started and not ended, by task id
}

// Shell is a background command (Bash with run_in_background) or a Monitor a session started: what keeps
// Claude Code's status at "shell" after a turn.
type Shell struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // command, monitor
	Desc    string `json:"desc"`
	Cmd     string `json:"cmd,omitempty"`
	Started int64  `json:"started"`
	until   int64  // a monitor's timeout (0: none)
}

// Claude Code tells a session a background task ended with a <task-notification> in its transcript;
// for a subagent the task id is the subagent's id.
var bgTextRe = regexp.MustCompile(`Monitor started \(task ([A-Za-z0-9_-]{1,64})(?:, expires in (\d+)m)?|running in background with ID: ([A-Za-z0-9_-]{1,64})`)

var taskNoteRe = regexp.MustCompile(`<task-id>([A-Za-z0-9_-]{1,64})</task-id>.{0,800}?<status>([a-z_]{1,20})</status>`)

// ended reads what was appended to the parent transcript since the last call (on the first, its last
// 8 MB) for task notifications with a final status. The caller holds c.mu.
// ponytail: one entry per parent transcript seen, never dropped; a few bytes each.
func (c *subagentCache) ended(parent string) map[string]bool {
	fi, err := os.Stat(parent)
	if err != nil {
		return nil
	}
	if c.ends == nil {
		c.ends = map[string]*taskEnds{}
	}
	te := c.ends[parent]
	if te == nil || fi.Size() < te.off { // new, or rewritten
		te = &taskEnds{off: max(0, fi.Size()-8<<20), ids: map[string]bool{}, calls: map[string]Shell{}, runs: map[string]Shell{}}
		c.ends[parent] = te
	}
	ls, next := readFrom(parent, te.off)
	for _, l := range ls {
		te.started(l)
		for _, m := range taskNoteRe.FindAllSubmatch(l, -1) {
			switch string(m[2]) {
			case "running", "pending", "queued", "started":
			default:
				te.ids[string(m[1])] = true
				delete(te.runs, string(m[1]))
			}
		}
	}
	te.off = next
	return te.ids
}

// started notes a background Bash or Monitor call, and the task id its result gives it.
func (te *taskEnds) started(l []byte) {
	call := bytes.Contains(l, []byte(`"run_in_background":true`)) || bytes.Contains(l, []byte(`"name":"Monitor"`))
	res := bytes.Contains(l, []byte(`"backgroundTaskId"`)) || bytes.Contains(l, []byte(`"taskId"`)) ||
		bytes.Contains(l, []byte("Monitor started (task ")) || bytes.Contains(l, []byte("running in background with ID: ")) // a subagent's results carry only the text
	if !call && !res {
		return
	}
	var e struct {
		Timestamp string
		Message   struct{ Content json.RawMessage }
		R         struct {
			BackgroundTaskID string `json:"backgroundTaskId"`
			TaskID           string `json:"taskId"`
			TimeoutMs        int64  `json:"timeoutMs"`
			Persistent       bool
		} `json:"toolUseResult"`
	}
	if json.Unmarshal(l, &e) != nil {
		return
	}
	var cs []struct {
		Type, ID, Name string
		ToolUseID      string          `json:"tool_use_id"`
		Content        json.RawMessage `json:"content"`
		Input          struct {
			Command, Description string
			RunInBackground      bool `json:"run_in_background"`
		}
	}
	json.Unmarshal(e.Message.Content, &cs)
	at := unixOf(e.Timestamp)
	for _, c := range cs {
		switch {
		case c.Type == "tool_use" && (c.Name == "Monitor" || c.Name == "Bash" && c.Input.RunInBackground):
			kind := map[string]string{"Bash": "command", "Monitor": "monitor"}[c.Name]
			te.calls[c.ID] = Shell{Kind: kind, Desc: trunc(c.Input.Description, 120), Cmd: trunc(c.Input.Command, 300), Started: at}
		case c.Type == "tool_result" && res:
			sh, ok := te.calls[c.ToolUseID]
			delete(te.calls, c.ToolUseID)
			id := e.R.BackgroundTaskID
			if sh.Kind == "monitor" {
				id = e.R.TaskID
			}
			if id == "" { // a subagent's transcript: the id (and a monitor's expiry) only in the result's text
				if m := bgTextRe.FindSubmatch(c.Content); m != nil {
					id = string(m[1]) + string(m[3])
					if mins, _ := strconv.Atoi(string(m[2])); mins > 0 && sh.Kind == "monitor" {
						e.R.TimeoutMs = int64(mins) * 60_000
					}
				}
			}
			if !subIDRe.MatchString(id) || te.ids[id] {
				continue
			}
			if !ok { // a command you backgrounded by hand (Ctrl+B)
				sh = Shell{Kind: "command", Desc: "Background command", Started: at}
			}
			if sh.Started == 0 {
				sh.Started = at
			}
			if sh.Kind == "monitor" && !e.R.Persistent && e.R.TimeoutMs > 0 {
				sh.until = sh.Started + e.R.TimeoutMs/1000
			}
			sh.ID = id
			te.runs[id] = sh
		}
	}
}

// shells lists the background commands and monitors still running in a session (oldest first): started
// by this Claude process (procStart: a resumed session's old tasks died with the old one), not ended,
// and a monitor not past its timeout. The caller asks only while Claude says something runs (not idle).
func (c *subagentCache) shells(parent string, procStart int64, now time.Time) []Shell {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ended(parent)
	te := c.ends[parent]
	if te == nil {
		return nil
	}
	var out []Shell
	for _, sh := range te.runs {
		if sh.Started >= procStart && (sh.until == 0 || now.Unix() < sh.until) {
			out = append(out, sh)
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

type subEntry struct {
	size  int64
	mtime time.Time
	sub   Subagent
	final bool
}

// ponytail: a subagent whose transcript hasn't moved for 60 min drops off (a tool call that long, or an abandoned one).
const subagentQuiet = 60 * time.Minute

// list returns the running subagents of a session (<its transcript, less .jsonl>/subagents), oldest first.
// Running: its last turn isn't finished, it wrote within subagentQuiet, the session wasn't told it ended
// (background ones), and the session is in a turn (foreground ones: the turn waits on them, so a
// subagent left mid-tool by an interrupt is over once the session is idle). The caller checks the
// parent is alive.
func (c *subagentCache) list(parent string, parentInTurn bool, now time.Time) []Subagent {
	sess := strings.TrimSuffix(parent, ".jsonl")
	dir := filepath.Join(sess, "subagents")
	files, _ := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	wfDirs, _ := filepath.Glob(filepath.Join(dir, "workflows", "wf_*"))
	for _, d := range wfDirs { // a finished run's agents are never listed (below): don't stat them every tick
		if !wfFinished(sess, filepath.Base(d)) {
			wf, _ := filepath.Glob(filepath.Join(d, "agent-*.jsonl"))
			files = append(files, wf...)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]subEntry{}
	}
	ended := c.ended(parent)
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
			var meta struct{ AgentType, Description, Model, RequestShape, WorkflowPhase string }
			if b, err := os.ReadFile(metaPath); err == nil {
				json.Unmarshal(b, &meta)
			}
			e.sub.Desc, e.sub.Type, e.sub.Model = trunc(meta.Description, 120), trunc(meta.AgentType, 40), trunc(meta.Model, 40)
			e.sub.BG = meta.RequestShape == "background"
			if wf := filepath.Base(filepath.Dir(p)); filepath.Base(filepath.Dir(filepath.Dir(p))) == "workflows" {
				e.sub.WF, e.sub.Phase = trunc(wf, 40), trunc(meta.WorkflowPhase, 60)
				e.sub.BG = true // a workflow runs on its own, whatever the parent's turn does
			}
			e.sub.path = p
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
		if !e.final && !ended[e.sub.ID] && (e.sub.BG || parentInTurn) && !(e.sub.WF != "" && wfFinished(sess, e.sub.WF)) {
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

func wfFinished(sess, wf string) bool {
	_, err := os.Stat(filepath.Join(sess, "workflows", wf+".json"))
	return err == nil
}

// workflows lists a session's workflow runs still going: not finished, an agent active within
// subagentQuiet, not every agent done. The name comes from its script, which Claude Code keeps under
// whichever project folder the session was in when it started it. Call after list (it reuses its reads).
func (c *subagentCache) workflows(parent string, now time.Time) []Workflow {
	sess := strings.TrimSuffix(parent, ".jsonl")
	dirs, _ := filepath.Glob(filepath.Join(sess, "subagents", "workflows", "wf_*"))
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Workflow
	for _, d := range dirs {
		id := filepath.Base(d)
		if wfFinished(sess, id) {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(d, "agent-*.jsonl"))
		w := Workflow{ID: trunc(id, 40), Name: id, Total: len(files)}
		active := false // a stat each first: an abandoned run (no summary, all quiet) costs no reads
		for _, p := range files {
			if fi, err := os.Stat(p); err == nil && now.Sub(fi.ModTime()) <= subagentQuiet {
				active = true
				break
			}
		}
		if !active {
			continue
		}
		for _, p := range files {
			fi, err := os.Stat(p)
			if err != nil {
				continue
			}
			if c.done == nil {
				c.done = map[string]int64{}
			}
			final := c.done[p] == fi.Size()
			if !final {
				e, ok := c.m[p] // list read the active ones
				if !ok || e.size != fi.Size() {
					ls, _, _ := readTail(p, 256<<10)
					_, e.final = lastTurn(ls)
				}
				if final = e.final; final {
					c.done[p] = fi.Size()
				}
			}
			if final {
				w.Done++
			}
			if mfi, err := os.Stat(strings.TrimSuffix(p, ".jsonl") + ".meta.json"); err == nil && (w.Started == 0 || mfi.ModTime().Unix() < w.Started) {
				w.Started = mfi.ModTime().Unix()
			}
		}
		if w.Done == w.Total {
			continue
		}
		if m, _ := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(sess)), "*", filepath.Base(sess), "workflows", "scripts", "*-"+id+".js")); len(m) > 0 {
			w.Name = trunc(strings.TrimSuffix(filepath.Base(m[0]), "-"+id+".js"), 80)
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started < out[j].Started })
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
	if err != nil { // a workflow's agent: one folder down (the id is a plain token, so the glob can't walk out)
		if w, _ := filepath.Glob(filepath.Join(dir, "workflows", "wf_*", "agent-"+id+".jsonl")); len(w) > 0 {
			p = w[0]
			fi, err = os.Stat(p)
		}
	}
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
	docFileRe = regexp.MustCompile(`^[\w@%+=~./-]*[\w@%+=-]\.(?i:html?|md|markdown|pdf|png|jpe?g|gif|webp|svg|csv|tsv|xlsx?|docx?)$`)
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
