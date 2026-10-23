package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The Assistant view: the owner's Hermes agent (projects/vm-agent) through its API server on loopback.
// Every channel's conversation (Telegram's DM sessions, this panel's "panel" session) merged into one chat
// in time order, each message labelled with its channel; what the page sends goes to the panel session. Connections (keys,
// MCP servers, skills, cron) are Hermes' own dashboard, proxied at /hermes/.

const (
	aiPanelSession = "panel"
	aiWindow       = 300 // items a chat opens on; scrolling up pages back this many at a time
)

type aiItem struct {
	K    string  `json:"k"` // session:message id (stable, the page's dedupe key)
	T    float64 `json:"t"`
	Ch   string  `json:"ch"`   // panel, telegram, agent
	Role string  `json:"role"` // user, assistant (an answer), think, note (narration between tool calls), tool, error, event
	Text string  `json:"text,omitempty"`
	Name string  `json:"name,omitempty"` // tool name (Text: a preview of its input); event: its kind
	Res  string  `json:"res,omitempty"`  // tool: the start of what it returned
	Run  bool    `json:"run,omitempty"`  // tool: no result yet
}

// aiStep: one step of the turn in progress, as the page draws its trace.
type aiStep struct {
	K     string  `json:"k"` // think, note, tool
	Text  string  `json:"text,omitempty"`
	Name  string  `json:"name,omitempty"`
	State string  `json:"state,omitempty"` // tool: run, ok, err
	T0    float64 `json:"t0,omitempty"`
	Dur   float64 `json:"dur,omitempty"`
}

// aiLive: the panel turn in progress.
type aiLive struct {
	Run   string   `json:"run,omitempty"`
	User  string   `json:"user"`
	Text  string   `json:"text"` // the answer so far
	Steps []aiStep `json:"steps,omitempty"`
	Since float64  `json:"since"`
}

type aiState struct {
	Up        bool              `json:"up"`
	Err       string            `json:"err,omitempty"`
	Platforms map[string]string `json:"platforms,omitempty"` // channel -> connected, retrying, …
	Model     string            `json:"model,omitempty"`     // as of the last panel turn
	Pick      string            `json:"pick,omitempty"`      // the model picked for the panel chat ("" = Hermes' default)
}

// aiQueued: a message the owner sent while JARVIS was answering; it goes out when that turn ends, oldest first.
type aiQueued struct {
	ID     string  `json:"id"`
	Text   string  `json:"text"`
	T      float64 `json:"t"`
	images []string
}

// aiTask: one task on the owner's board: vm-agent's Task model (models/task.schema.json), which JARVIS fills in
// and keeps up to date with its task_track tool
type aiTask struct {
	ID       int     `json:"id"`
	Title    string  `json:"title"`
	DoneWhen string  `json:"done_when"`
	Status   string  `json:"status"` // in_progress, blocked, done
	Progress int     `json:"progress"`
	Note     string  `json:"note,omitempty"`
	Created  float64 `json:"created"`
	Updated  float64 `json:"updated"`
	Log      []struct {
		T        float64 `json:"t"`
		Status   string  `json:"status"`
		Progress int     `json:"progress"`
		Note     string  `json:"note,omitempty"`
	} `json:"log,omitempty"` // how it got here, oldest first
}

type aiSess struct {
	active float64
	count  int
	items  []aiItem
}

type assistant struct {
	base, keyFile string
	hc            *http.Client
	google        string // where the Google OAuth client file goes (Hermes' google-workspace skill)
	tasksFile     string // the task board vm-agent writes
	stateFile     string // the panel chat's session and model (aisession.go)

	mu       sync.Mutex
	sess     map[string]*aiSess
	failed   []aiItem // panel turns Hermes didn't keep (provider errors): shown in the Panel chat
	items    []aiItem // every channel, oldest first
	live     *aiLive
	state    aiState
	tasks    []aiTask
	tasksMod time.Time
	queue    []aiQueued
	panel    aiPanel       // the panel chat's current Hermes session and model
	queued   int           // queue ids
	changed  chan struct{} // closed and replaced on every change
	watchers int
	cancel   context.CancelFunc // the running panel turn
}

func newAssistant(base, keyFile, google string) *assistant {
	return &assistant{base: base, keyFile: keyFile, google: google, sess: map[string]*aiSess{}, changed: make(chan struct{}), panel: aiPanel{Session: aiPanelSession},
		hc: &http.Client{Transport: &http.Transport{Proxy: nil}}} // loopback: never the agent sessions' proxy
}

func (a *assistant) bump() { // a.mu held
	close(a.changed)
	a.changed = make(chan struct{})
}

func (a *assistant) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	key, err := os.ReadFile(a.keyFile)
	if err != nil {
		return nil, errors.New("the assistant isn't set up: no API key (run projects/vm-agent/setup.sh)")
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(key)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, errors.New("the assistant isn't running (Hermes gateway down)")
	}
	return resp, nil
}

func (a *assistant) getJSON(ctx context.Context, path string, v any) error {
	resp, err := a.do(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("hermes %s: %s", path, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(v)
}

// ---- reading Hermes' sessions ----

type hSession struct {
	ID         string  `json:"id"`
	Source     string  `json:"source"`
	Count      int     `json:"message_count"`
	LastActive float64 `json:"last_active"`
	Started    float64 `json:"started_at"`
}

type hMessage struct {
	ID         json.RawMessage `json:"id"`
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Reasoning  string          `json:"reasoning"`
	Timestamp  float64         `json:"timestamp"`
	Kind       string          `json:"display_kind"` // a user row Hermes wrote itself: async_delegation_complete, process_complete, …
}

// text of a message's content: a string, or a list of parts with text.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct{ Type, Text string }
	json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

type hToolCall struct {
	ID       string                           `json:"id"`
	Function struct{ Name, Arguments string } `json:"function"`
}

// calls: what a tool call ran. Hermes wraps deferred tools (MCP ones among them) as tool_call{calls: [...]}.
func (c hToolCall) calls() (names, args []string) {
	if c.Function.Name == "tool_call" {
		var w struct {
			Calls []struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"calls"`
		}
		if json.Unmarshal([]byte(c.Function.Arguments), &w) == nil && len(w.Calls) > 0 {
			for _, x := range w.Calls {
				names, args = append(names, x.Name), append(args, string(x.Arguments))
			}
			return
		}
	}
	return []string{c.Function.Name}, []string{c.Function.Arguments}
}

// toolResult: a tool message's text without Hermes' untrusted-content wrapper or the MCP {"result": …} envelope.
func toolResult(s string) string {
	if strings.HasPrefix(s, "<untrusted_tool_result") {
		if i := strings.Index(s, "\n\n"); i >= 0 {
			s = s[i+2:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "</untrusted_tool_result>")
	}
	var env struct {
		Result *string `json:"result"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(s)), &env) == nil && env.Result != nil {
		s = *env.Result
	}
	return trunc(strings.TrimSpace(s), 1500)
}

// toolCalls: a list, or that list JSON-encoded in a string.
func toolCalls(raw json.RawMessage) []hToolCall {
	var calls []hToolCall
	if json.Unmarshal(raw, &calls) == nil {
		return calls
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		json.Unmarshal([]byte(s), &calls)
	}
	return calls
}

// toolPreview: the gist of a tool call's input, one line.
func toolPreview(args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return trunc(args, 160)
	}
	if len(m) == 0 {
		return ""
	}
	for _, k := range []string{"command", "query", "url", "path", "text", "prompt", "goal", "name"} {
		if v, ok := m[k].(string); ok && v != "" {
			return trunc(strings.Join(strings.Fields(v), " "), 160)
		}
	}
	keys := slices.Sorted(maps.Keys(m)) // no main argument: its scalars, "session=dev-1 · pane=3"
	var parts []string
	for _, k := range keys {
		switch v := m[k].(type) {
		case string, float64, bool:
			parts = append(parts, k+"="+strings.Join(strings.Fields(fmt.Sprint(v)), " "))
		}
	}
	return trunc(strings.Join(parts, " · "), 160)
}

// toItems: a session's messages as chat items. An assistant message that calls tools is the turn's trace:
// its reasoning (think), what it says while working (note) and the calls (tool, with their results once
// the tool messages arrive); one that doesn't is an answer.
func toItems(sessID, ch string, msgs []hMessage) []aiItem {
	var out []aiItem
	byCall := map[string]int{} // tool call id -> its first item
	for _, m := range msgs {
		id := sessID + ":" + strings.Trim(string(m.ID), `"`)
		switch m.Role {
		case "user":
			switch m.Kind {
			case "", "steer": // the owner (steer: said while a turn ran)
				out = append(out, aiItem{K: id, T: m.Timestamp, Ch: ch, Role: "user", Text: contentText(m.Content)})
			case "hidden":
			default: // Hermes waking itself: a subagent or background process finished, a model switch, …
				out = append(out, aiItem{K: id, T: m.Timestamp, Ch: ch, Role: "event", Name: m.Kind, Text: contentText(m.Content)})
			}
		case "assistant":
			if m.Kind == "failed_turn" { // Hermes' stand-in for a turn the model refused or that failed
				out = append(out, aiItem{K: id, T: m.Timestamp, Ch: ch, Role: "error", Name: "failed_turn", Text: contentText(m.Content)})
				break
			}
			calls := toolCalls(m.ToolCalls)
			if r := strings.TrimSpace(m.Reasoning); r != "" {
				out = append(out, aiItem{K: id + ":think", T: m.Timestamp, Ch: ch, Role: "think", Text: trunc(r, 4000)})
			}
			if t := strings.TrimSpace(contentText(m.Content)); t != "" {
				role := "assistant"
				if len(calls) > 0 {
					role = "note"
				}
				out = append(out, aiItem{K: id, T: m.Timestamp, Ch: ch, Role: role, Text: t})
			}
			n := 0
			for _, c := range calls {
				names, args := c.calls()
				for i := range names {
					if i == 0 && c.ID != "" {
						byCall[c.ID] = len(out)
					}
					out = append(out, aiItem{K: id + ":" + strconv.Itoa(n), T: m.Timestamp, Ch: ch, Role: "tool", Name: names[i], Text: toolPreview(args[i]), Run: true})
					n++
				}
			}
		case "tool":
			if i, ok := byCall[m.ToolCallID]; ok {
				out[i].Res, out[i].Run = toolResult(contentText(m.Content)), false
				for j := i + 1; j < len(out) && out[j].Role == "tool" && out[j].Run && out[j].T == out[i].T; j++ {
					out[j].Run = false // the rest of one wrapped batch: answered by the same message
				}
			}
		}
	}
	return out
}

// channelOf: which chat a session belongs to ("" = not shown: cron runs, other API clients, …).
func channelOf(s hSession) string {
	switch {
	case s.Source == "telegram":
		return "telegram"
	case s.ID == aiPanelSession || strings.HasPrefix(s.ID, aiPanelSession+"-"): // every conversation the panel started
		return "panel"
	case strings.HasPrefix(s.ID, "agent-"): // a Claude agent's, through vm-agent's `jarvis` command
		return "agent"
	}
	return ""
}

// refresh re-reads what changed in Hermes: its health, and the messages of sessions that moved.
func (a *assistant) refresh(ctx context.Context) {
	var st aiState
	var health struct {
		Platforms map[string]struct {
			State string `json:"state"`
		} `json:"platforms"`
	}
	var sessions []hSession
	if err := a.getJSON(ctx, "/health/detailed", &health); err != nil {
		st.Err = err.Error()
	} else {
		st.Up = true
		st.Platforms = map[string]string{}
		for k, p := range health.Platforms {
			st.Platforms[k] = p.State
		}
		for _, src := range []string{"telegram", "api_server"} {
			var l struct{ Data []hSession }
			if err := a.getJSON(ctx, "/api/sessions?limit=200&source="+src, &l); err == nil {
				sessions = append(sessions, l.Data...)
			}
		}
	}
	var tasks []aiTask
	var mod time.Time
	if fi, err := os.Stat(a.tasksFile); err == nil {
		mod = fi.ModTime()
	}
	a.mu.Lock()
	newTasks := !mod.Equal(a.tasksMod)
	a.mu.Unlock()
	if newTasks {
		b, _ := os.ReadFile(a.tasksFile)
		json.Unmarshal(b, &tasks)
	}
	type fetched struct {
		id    string
		s     hSession
		items []aiItem
	}
	var got []fetched
	a.mu.Lock()
	known := map[string]aiSess{}
	for id, s := range a.sess {
		known[id] = *s
	}
	a.mu.Unlock()
	for _, s := range sessions {
		ch := channelOf(s)
		if ch == "" {
			continue
		}
		if k, ok := known[s.ID]; ok && k.active == s.LastActive && k.count == s.Count {
			continue
		}
		var msgs []hMessage
		for off := 0; ; off += 500 { // oldest first, a page at a time
			var page struct{ Data []hMessage }
			if err := a.getJSON(ctx, fmt.Sprintf("/api/sessions/%s/messages?order=oldest&limit=500&offset=%d", url.PathEscape(s.ID), off), &page); err != nil {
				break
			}
			msgs = append(msgs, page.Data...)
			if len(page.Data) < 500 || off > 20000 {
				break
			}
		}
		got = append(got, fetched{s.ID, s, toItems(s.ID, ch, msgs)})
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if st.Up {
		st.Model = a.state.Model
	}
	st.Pick = a.state.Pick
	if newTasks {
		a.tasks, a.tasksMod = tasks, mod
	}
	changed := newTasks || len(got) > 0 || st.Up != a.state.Up || st.Err != a.state.Err || fmt.Sprint(st.Platforms) != fmt.Sprint(a.state.Platforms)
	a.state = st
	for _, f := range got {
		a.sess[f.id] = &aiSess{active: f.s.LastActive, count: f.s.Count, items: f.items}
	}
	if len(got) > 0 {
		a.rebuild()
	}
	if changed {
		a.bump()
	}
}

func (a *assistant) rebuild() { // a.mu held
	var all []aiItem
	for _, s := range a.sess {
		all = append(all, s.items...)
	}
	all = append(all, a.failed...)
	for _, r := range a.panel.Resets { // where the panel chat started over
		all = append(all, aiItem{K: "new:" + r.Session, T: r.T, Ch: "panel", Role: "event", Name: "new_chat"})
	}
	slices.SortStableFunc(all, func(x, y aiItem) int { return cmp.Compare(x.T, y.T) })
	a.items = all
}

// watch keeps the chats fresh while at least one page shows them (Hermes is asked every 2 s).
func (a *assistant) watch() func() {
	a.mu.Lock()
	a.watchers++
	first := a.watchers == 1
	a.mu.Unlock()
	if first {
		go func() {
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				a.refresh(ctx)
				cancel()
				time.Sleep(2 * time.Second)
				a.mu.Lock()
				done := a.watchers == 0
				a.mu.Unlock()
				if done {
					return
				}
			}
		}()
	}
	return func() {
		a.mu.Lock()
		a.watchers--
		a.mu.Unlock()
	}
}

// ---- sending: one panel turn at a time, streamed into a.live ----

// send starts a panel turn, or queues the message behind the one running (queued). images: data URLs, sent
// to the model with the text (Hermes' image_url parts).
func (a *assistant) send(text string, images ...string) (queued bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.live == nil {
		a.start(text, images)
		return false, nil
	}
	if len(a.queue) >= 10 {
		return false, errors.New("10 messages are already waiting: let JARVIS catch up")
	}
	a.queued++
	a.queue = append(a.queue, aiQueued{ID: strconv.Itoa(a.queued), Text: text, T: float64(time.Now().UnixMilli()) / 1000, images: images})
	a.bump()
	return true, nil
}

func (a *assistant) start(text string, images []string) { // a.mu held
	ctx, cancel := context.WithCancel(context.Background())
	a.live, a.cancel = &aiLive{User: text, Since: float64(time.Now().UnixMicro()) / 1e6}, cancel
	a.bump()
	go a.run(ctx, text, images)
}

// unqueue takes back a queued message that hasn't gone out yet.
func (a *assistant) unqueue(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := slices.IndexFunc(a.queue, func(q aiQueued) bool { return q.ID == id })
	if i < 0 {
		return false
	}
	a.queue = slices.Delete(a.queue, i, i+1)
	a.bump()
	return true
}

func (a *assistant) run(ctx context.Context, text string, images []string) {
	reply, model, kept, err := a.stream(ctx, text, images)
	if ctx.Err() != nil && err == nil {
		err = errors.New("stopped")
	}
	rctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	a.refresh(rctx) // the turn's messages, now in Hermes' history
	cancel()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !kept { // Hermes didn't keep this turn (provider error, stop before any reply): keep it here
		since := a.live.Since
		msg := cmp.Or(reply, "")
		if err != nil {
			msg = strings.TrimSpace(msg + "\n\n" + err.Error())
		}
		a.failed = append(a.failed, aiItem{K: fmt.Sprintf("failed:%f:u", since), T: since, Ch: "panel", Role: "user", Text: text})
		if msg != "" {
			end := max(float64(time.Now().UnixMicro())/1e6, since+1e-6) // when it failed: before the next turn starts
			a.failed = append(a.failed, aiItem{K: fmt.Sprintf("failed:%f:e", since), T: end, Ch: "panel", Role: "error", Text: msg})
		}
		a.failed = a.failed[max(0, len(a.failed)-40):]
		a.rebuild()
	}
	if model != "" {
		a.state.Model = model
	}
	a.live, a.cancel = nil, nil
	if len(a.queue) > 0 { // what the owner sent meanwhile
		next := a.queue[0]
		a.queue = a.queue[1:]
		a.start(next.Text, next.images)
	}
	a.bump()
}

// stream runs one turn on the panel session and mirrors its events into a.live. kept: Hermes stored it.
func (a *assistant) stream(ctx context.Context, text string, images []string) (reply, model string, kept bool, err error) {
	a.mu.Lock()
	sess, model := a.panel.Session, a.panel.Model
	a.mu.Unlock()
	title := "Control panel" // Hermes wants every session's title to be its own
	if sess != aiPanelSession {
		title += " · " + strings.TrimPrefix(sess, aiPanelSession+"-")
	}
	r, err := a.do(ctx, "POST", "/api/sessions", map[string]any{"id": sess, "title": title})
	if err != nil {
		return "", "", false, err
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	r.Body.Close()
	if r.StatusCode != 201 && r.StatusCode != 409 { // 409: it exists already
		return "", "", false, fmt.Errorf("hermes couldn't open the conversation: %s %s", r.Status, trunc(string(b), 300))
	}
	var msg any = text
	if len(images) > 0 {
		parts := []map[string]any{{"type": "text", "text": text}}
		for _, u := range images {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
		}
		msg = parts
	}
	body := map[string]any{"message": msg}
	if model != "" {
		body["model"] = model
	}
	resp, err := a.do(ctx, "POST", "/api/sessions/"+url.PathEscape(sess)+"/chat/stream", body)
	if err != nil {
		return "", "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", "", false, fmt.Errorf("hermes: %s %s", resp.Status, trunc(string(b), 300))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var event string
	last := time.Time{}
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			event = v
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var e struct {
			RunID    string `json:"run_id"`
			Delta    string `json:"delta"`
			ToolName string `json:"tool_name"`
			Text     string `json:"text"`
			Args     any    `json:"args"`
			Already  bool   `json:"already_streamed"`
			Content  string `json:"content"`
			Error    any    `json:"error"`
			Messages []any  `json:"messages"`
			Runtime  struct {
				Provider, Model string
			} `json:"runtime"`
		}
		json.Unmarshal([]byte(data), &e)
		a.mu.Lock()
		l := a.live
		switch event {
		case "run.started":
			l.Run = e.RunID
			if e.Runtime.Model != "" {
				model = strings.Trim(e.Runtime.Provider+"/"+e.Runtime.Model, "/")
			}
		case "assistant.delta":
			l.Text += e.Delta
		case "assistant.commentary": // what it says while it works: the text streamed so far was narration
			if e.Already {
				l.Text = ""
			}
			l.Steps = append(l.Steps, aiStep{K: "note", Text: e.Text})
		case "tool.started":
			args, _ := json.Marshal(e.Args)
			l.Steps = append(l.Steps, aiStep{K: "tool", Name: e.ToolName, Text: toolPreview(string(args)), State: "run", T0: float64(time.Now().UnixMilli()) / 1000})
		case "tool.completed", "tool.failed":
			for i := len(l.Steps) - 1; i >= 0; i-- {
				if st := &l.Steps[i]; st.K == "tool" && st.State == "run" && st.Name == e.ToolName {
					st.State, st.Dur = "ok", float64(time.Now().UnixMilli())/1000-st.T0
					if event == "tool.failed" {
						st.State = "err"
					}
					break
				}
			}
		case "assistant.completed":
			reply = e.Content
			l.Text = e.Content
		case "run.completed":
			kept = len(e.Messages) > 0
		case "run.failed":
			err = fmt.Errorf("the turn failed: %v", e.Error)
		}
		// deltas come many a second: the page hears about them at most 5 times a second
		if event != "assistant.delta" || time.Since(last) > 200*time.Millisecond {
			last = time.Now()
			a.bump()
		}
		a.mu.Unlock()
	}
	return reply, model, kept, err
}

func (a *assistant) stop() {
	a.mu.Lock()
	run, cancel := "", a.cancel
	if a.live != nil {
		run = a.live.Run
	}
	a.mu.Unlock()
	if run != "" {
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		if r, err := a.do(ctx, "POST", "/v1/runs/"+url.PathEscape(run)+"/stop", map[string]any{}); err == nil {
			r.Body.Close()
		}
		c()
	}
	if cancel != nil {
		cancel()
	}
}

// ---- HTTP ----

// aiStream: the chat's last aiWindow items (every channel, each labelled), then what's appended, the turn
// in progress and the channels' state, as they change.
func (s *server) aiStream(w http.ResponseWriter, r *http.Request) {
	e, ok := startSSE(w, r)
	if !ok {
		return
	}
	defer e.close()
	a := s.ai
	defer a.watch()()
	tok, email := sessionToken(r), identity(r)
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	lastKey, lastLive, lastState, lastTasks, lastQueue, lastSig, sent := "", "", "", "", "", "", false
	for {
		a.mu.Lock()
		items := a.items
		live, _ := json.Marshal(a.live)
		state, _ := json.Marshal(a.state)
		tasks, _ := json.Marshal(a.tasks)
		queue, _ := json.Marshal(a.queue)
		wait := a.changed
		a.mu.Unlock()
		msg := map[string]any{}
		at := slices.IndexFunc(items, func(it aiItem) bool { return it.K == lastKey })
		changed := at >= 0 && aiSig(items[max(0, at+1-aiWindow):at+1]) != lastSig // a tool's result landed on a sent step
		switch {
		case !sent || (lastKey != "" && at < 0) || changed: // first, the window moved under us, or sent items changed: start over
			msg["kind"] = "init"
			msg["items"] = items[max(0, len(items)-aiWindow):]
			msg["more"] = len(items) > aiWindow
		case at < len(items)-1 || (lastKey == "" && len(items) > 0):
			msg["kind"] = "add"
			msg["items"] = items[at+1:]
		}
		if len(items) > 0 {
			lastKey, lastSig = items[len(items)-1].K, aiSig(items[max(0, len(items)-aiWindow):])
		}
		if string(live) != lastLive || !sent {
			msg["live"], lastLive = json.RawMessage(live), string(live)
		}
		if string(state) != lastState || !sent {
			msg["state"], lastState = json.RawMessage(state), string(state)
		}
		if string(queue) != lastQueue || !sent {
			msg["queue"], lastQueue = json.RawMessage(queue), string(queue)
		}
		if string(tasks) != lastTasks || !sent {
			msg["tasks"], lastTasks = json.RawMessage(tasks), string(tasks)
		}
		sent = true
		if len(msg) > 0 {
			if msg["kind"] == nil {
				msg["kind"] = "add"
			}
			b, _ := json.Marshal(msg)
			e.data(b)
		}
		select {
		case <-r.Context().Done():
			return
		case <-wait:
		case <-ping.C:
			if !s.auth.peekSession(tok, email) {
				return
			}
			e.ping()
		}
	}
}

// aiSig: what the page has of these items, to notice when a sent one changes (a tool's result arriving).
func aiSig(its []aiItem) string {
	var b strings.Builder
	for _, it := range its {
		fmt.Fprintf(&b, "%s|%t|%d|%d;", it.K, it.Run, len(it.Res), len(it.Text))
	}
	return b.String()
}

// aiHistory pages back: the aiWindow items before `before` (an item key).
func (s *server) aiHistory(w http.ResponseWriter, r *http.Request) {
	a := s.ai
	a.mu.Lock()
	items := a.items
	a.mu.Unlock()
	at := slices.IndexFunc(items, func(it aiItem) bool { return it.K == r.URL.Query().Get("before") })
	if at < 0 {
		writeJSON(w, 404, map[string]any{"error": "unknown message"})
		return
	}
	from := max(0, at-aiWindow)
	writeJSON(w, 200, map[string]any{"items": items[from:at], "more": from > 0})
}

func (s *server) aiSend(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Text   string
		Images []string // paths the page uploaded (/api/pane/upload): photos, which the model sees
		Files  []string // and any other file, which JARVIS opens itself
	}
	if err := readJSONN(r, &b, 1<<20); err != nil || (strings.TrimSpace(b.Text) == "" && len(b.Images)+len(b.Files) == 0) || !utf8.ValidString(b.Text) || len(b.Text) > maxPaste {
		writeJSON(w, 400, map[string]any{"error": "type a message (up to 100 KB)"})
		return
	}
	text, images, err := s.aiAttach(strings.TrimSpace(b.Text), b.Images, b.Files)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	queued, err := s.ai.send(text, images...)
	if err != nil {
		writeJSON(w, 409, map[string]any{"error": err.Error()})
		return
	}
	s.audit.add("assistant", strconv.Quote(trunc(b.Text, 80)), clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true, "queued": queued})
}

// aiUnqueue takes back a message still waiting for JARVIS to finish.
func (s *server) aiUnqueue(w http.ResponseWriter, r *http.Request) {
	var b struct{ ID string }
	if err := readJSONN(r, &b, 1<<10); err != nil || !s.ai.unqueue(b.ID) {
		writeJSON(w, 404, map[string]any{"error": "already sent"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// aiAttach takes what a message carries out of the uploads (they're cleared after a week) into
// ~/jarvis-files/uploads and names each file in the text, so JARVIS can open it and the page can show it.
// Photos: "[Attached image: <path>]", and data URLs for the model to see (within Hermes' 10 MB request limit:
// the page scales them down first). Any other file: "[Attached file: <path>]", opened with JARVIS's own tools.
// Only the page's own uploads; all of them checked before any is moved.
func (s *server) aiAttach(text string, images, files []string) (string, []string, error) {
	if len(images) > 6 || len(images)+len(files) > 10 {
		return "", nil, errors.New("up to 10 files a message, 6 of them photos")
	}
	upload := func(p string) error {
		if name := filepath.Base(p); filepath.Dir(p) != s.uploadDir || !uploadNameRe.MatchString(name) {
			return errors.New("not an upload: " + name)
		}
		if _, err := os.Stat(p); err != nil {
			return errors.New("upload gone: " + filepath.Base(p))
		}
		return nil
	}
	var urls, lines []string
	total := 0
	for _, p := range images {
		if err := upload(p); err != nil {
			return "", nil, err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", nil, err
		}
		ct := http.DetectContentType(b)
		if imageExt[ct] == "" {
			return "", nil, errors.New("not an image: " + filepath.Base(p))
		}
		if total += len(b); total > 6<<20 {
			return "", nil, errors.New("photos too large together (6 MB a message)")
		}
		urls = append(urls, "data:"+ct+";base64,"+base64.StdEncoding.EncodeToString(b))
	}
	for _, p := range files {
		if err := upload(p); err != nil {
			return "", nil, err
		}
	}
	dir := filepath.Join(s.cfg.home, "jarvis-files/uploads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	keep := func(p, kind string) error {
		dst := filepath.Join(dir, filepath.Base(p))
		if os.Rename(p, dst) != nil { // on another filesystem: copy it over
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := os.WriteFile(dst, b, 0o600); err != nil {
				return err
			}
			os.Remove(p)
		}
		lines = append(lines, "[Attached "+kind+": "+dst+"]")
		return nil
	}
	for _, p := range images {
		if err := keep(p, "image"); err != nil {
			return "", nil, err
		}
	}
	for _, p := range files {
		if err := keep(p, "file"); err != nil {
			return "", nil, err
		}
	}
	if len(lines) > 0 {
		text = strings.TrimSpace(text + "\n\n" + strings.Join(lines, "\n"))
	}
	return text, urls, nil
}

func (s *server) aiStop(w http.ResponseWriter, r *http.Request) {
	s.ai.stop()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// aiGoogle stores the Google OAuth client (Desktop app JSON from Google Cloud) where Hermes'
// google-workspace skill looks for it; the agent then runs the sign-in in chat.
func (s *server) aiGoogle(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		_, errC := os.Stat(s.ai.google)
		_, errT := os.Stat(filepath.Join(filepath.Dir(s.ai.google), "google_token.json"))
		writeJSON(w, 200, map[string]any{"client": errC == nil, "token": errT == nil})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	var c map[string]struct {
		ClientID string `json:"client_id"`
	}
	if json.Unmarshal(body, &c) != nil || (c["installed"].ClientID == "" && c["web"].ClientID == "") {
		writeJSON(w, 400, map[string]any{"error": "that isn't a Google OAuth client file (client_secret_….json, Desktop app)"})
		return
	}
	tmp := s.ai.google + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil || os.Rename(tmp, s.ai.google) != nil {
		writeJSON(w, 500, map[string]any{"error": "couldn't save it"})
		return
	}
	s.audit.add("assistant-google-client", "saved", clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// aiFile serves a file the assistant made or points to (an absolute path under the home directory or /tmp)
// for the chat's file cards and inline images. Images, PDFs and plain text open in the browser; anything else
// (HTML and SVG included: they could run in the panel's origin) downloads.
func (s *server) aiFile(w http.ResponseWriter, r *http.Request) {
	p := filepath.Clean(r.URL.Query().Get("path"))
	under := func(p string) bool { return strings.HasPrefix(p, s.cfg.home+"/") || strings.HasPrefix(p, "/tmp/") }
	real, err := filepath.EvalSymlinks(p)
	if !filepath.IsAbs(p) || !under(p) || err != nil || !under(real) {
		writeJSON(w, 404, map[string]any{"error": "no such file"})
		return
	}
	f, err := os.Open(real)
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": "no such file"})
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		writeJSON(w, 404, map[string]any{"error": "not a file"})
		return
	}
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(real)))
	inline := (strings.HasPrefix(ct, "image/") && !strings.Contains(ct, "svg")) || ct == "application/pdf" || strings.HasPrefix(ct, "text/plain")
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	if ct == "" || !inline {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": filepath.Base(real)}))
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// dashToken: the dashboard's session token, which its API wants on every call. A loopback dashboard hands it to
// whoever loads its page; the proxy reads it from there (again after a restart mints a new one).
type dashToken struct {
	base string
	mu   sync.Mutex
	tok  string
}

var dashTokenRe = regexp.MustCompile(`__HERMES_SESSION_TOKEN__="([A-Za-z0-9_\-.]+)"`)

func (d *dashToken) get() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tok != "" {
		return d.tok
	}
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}).Get(d.base + "/")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if m := dashTokenRe.FindSubmatch(b); m != nil {
		d.tok = string(m[1])
	}
	return d.tok
}

func (d *dashToken) reset() { d.mu.Lock(); d.tok = ""; d.mu.Unlock() }

// hermesProxy: Hermes' dashboard (keys, MCP servers, skills, cron, channels) under /hermes/, behind the
// panel's Access + PIN; its API also serves the Assistant's control room. Hermes sees a loopback client (its
// Host and Origin guards pass) with the prefix, and the session token its API asks for.
func hermesProxy(target string) http.Handler {
	u, _ := url.Parse(target)
	tok := &dashToken{base: target}
	return &httputil.ReverseProxy{
		ModifyResponse: func(r *http.Response) error {
			if r.StatusCode == 401 { // a restarted dashboard minted a new token
				tok.reset()
			}
			return nil
		},
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(u)
			if strings.Contains(r.In.URL.Path, "/api/") {
				r.Out.Header.Set("X-Hermes-Session-Token", tok.get())
			}
			r.Out.URL.Path = strings.TrimPrefix(r.In.URL.Path, "/hermes")
			r.Out.URL.RawPath = ""
			r.Out.Header.Set("X-Forwarded-Prefix", "/hermes")
			r.Out.Header.Del("Origin") // the panel already refused cross-origin requests
			r.Out.Header.Del("Referer")
			r.Out.Header.Del("Cf-Access-Jwt-Assertion") // the panel's credentials stay with the panel
			r.Out.Header.Del("Cookie")
			for _, c := range r.In.Cookies() {
				if c.Name != "cp_session" && c.Name != "CF_Authorization" {
					r.Out.AddCookie(c)
				}
			}
		},
		Transport: &http.Transport{Proxy: nil},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "The Hermes dashboard isn't running (it starts with projects/vm-agent/run-agent.sh).", 502)
		},
	}
}
