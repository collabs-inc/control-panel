package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var chatFixture = []string{
	`{"type":"permission-mode","permissionMode":"bypassPermissions"}`,
	`{"type":"user","timestamp":"2026-09-30T01:00:00.000Z","message":{"role":"user","content":"Fix the <b>flaky</b> test"}}`,
	`{"type":"user","isMeta":true,"message":{"content":"<local-command-caveat>ignore</local-command-caveat>"}}`,
	`{"type":"assistant","timestamp":"2026-09-30T01:00:02.000Z","message":{"content":[{"type":"thinking","thinking":"secret"}]}}`,
	`{"type":"assistant","timestamp":"2026-09-30T01:00:03.000Z","message":{"content":[{"type":"text","text":"Looking at it."}]}}`,
	`{"type":"assistant","timestamp":"2026-09-30T01:00:04.000Z","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"go test ./...\nsecond line","description":"Run tests"}}]}}`,
	`{"type":"user","timestamp":"2026-09-30T01:00:09.000Z","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","is_error":true,"content":"FAIL x"}]},"toolUseResult":{}}`,
	`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu2","name":"Read","input":{"file_path":"/workspace/home/projects/a.go"}}]}}`,
	`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu2","content":[{"type":"text","text":"package main"}]}]}}`,
	`not json at all`,
	`{"type":"queue-operation","operation":"enqueue","content":"x"}`,
	`{"type":"attachment","attachment":{"type":"queued_command","prompt":"also do y"}}`,
	`{"type":"user","message":{"content":"<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n<summary>Background command \"Run suite\" completed (exit code 0)</summary>\n</task-notification>"}}`,
	`{"type":"user","message":{"content":"<command-name>/login</command-name>\n<command-message>login</command-message>\n<command-args></command-args>"}}`,
	`{"type":"user","message":{"content":"<local-command-stdout>Login successful</local-command-stdout>"}}`,
	`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`,
	`{"type":"system","subtype":"turn_duration","durationMs":128000}`,
	`{"type":"system","subtype":"stop_hook_summary","level":"suggestion"}`,
	`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted"}`,
	`{"type":"ai-title","aiTitle":"Fix flaky test"}`,
	`{"type":"pr-link","prNumber":10620,"prUrl":"https://github.com/o/r/pull/10620"}`,
	`{"type":"assistant","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"Done."}]}}`,
}

func lines(ss []string) [][]byte {
	var out [][]byte
	for _, s := range ss {
		out = append(out, []byte(s))
	}
	return out
}

func TestParseChat(t *testing.T) {
	items, title := parseChat(lines(chatFixture))
	if title != "Fix flaky test" {
		t.Fatalf("title = %q", title)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.K+":"+it.Name+":"+it.Text)
	}
	want := []string{
		"user::Fix the <b>flaky</b> test",
		"think::secret",
		"text::Looking at it.",
		"tool:Bash:go test ./...",
		"result::FAIL x",
		"tool:Read:~/projects/a.go",
		"result::package main",
		"queue:enqueue:x",
		"user::also do y",
		`note::Background command "Run suite" completed (exit code 0)`,
		"note::/login",
		"note::Login successful",
		"note::Interrupted",
		"note::Worked for 2m 8s",
		"note::Conversation compacted",
		"note::PR #10620",
		"text::Done.",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("items:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if items[0].T != 1790730000 || items[3].ID != "tu1" || items[4].ID != "tu1" || !items[4].Err || items[6].Err {
		t.Fatalf("fields: %+v %+v %+v", items[0], items[3], items[4])
	}
	if items[1].Dur != 2 {
		t.Fatalf("thinking took 2 s (01:00:00 → 01:00:02), got %+v", items[1])
	}
	if items[3].In != "go test ./...\nsecond line" {
		t.Fatalf("tool input for the expanded card: %q", items[3].In)
	}
	if items[5].Path != "/workspace/home/projects/a.go" {
		t.Fatalf("Read carries its path: %+v", items[5])
	}
	if !strings.Contains(items[5].In, `"file_path": "/workspace/home/projects/a.go"`) {
		t.Fatalf("non-Bash input is pretty JSON: %q", items[5].In)
	}
}

func TestLastTurn(t *testing.T) {
	doing, final := lastTurn(lines(chatFixture))
	if !final || doing != "" {
		t.Fatalf("finished transcript: doing=%q final=%v", doing, final)
	}
	doing, final = lastTurn(lines(chatFixture[:6]))
	if final || doing != "Bash: go test ./..." {
		t.Fatalf("mid tool: doing=%q final=%v", doing, final)
	}
	doing, final = lastTurn(lines(chatFixture[:7])) // tool result back, model thinking
	if final || doing != "Thinking" {
		t.Fatalf("after result: doing=%q final=%v", doing, final)
	}
}

func TestReadTailAndBefore(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(p, []byte("aaaa\nbbbb\ncccc\ndd"), 0o600) // "dd" is still being written
	ls, start, end := readTail(p, 8)
	if len(ls) != 1 || string(ls[0]) != "cccc" || start != 10 || end != 15 {
		t.Fatalf("tail = %q start=%d end=%d", ls, start, end)
	}
	ls, start = readBefore(p, 10, 100)
	if len(ls) != 2 || string(ls[0]) != "aaaa" || start != 0 {
		t.Fatalf("before = %q start=%d", ls, start)
	}
	ls, next := readFrom(p, 10)
	if len(ls) != 1 || string(ls[0]) != "cccc" || next != 15 {
		t.Fatalf("from = %q next=%d", ls, next)
	}
}

func TestSubagents(t *testing.T) {
	dir := t.TempDir()
	put := func(id, meta string, body []string, age time.Duration) {
		os.WriteFile(filepath.Join(dir, "agent-"+id+".meta.json"), []byte(meta), 0o644)
		p := filepath.Join(dir, "agent-"+id+".jsonl")
		os.WriteFile(p, []byte(strings.Join(body, "\n")+"\n"), 0o600)
		mt := time.Now().Add(-age)
		os.Chtimes(p, mt, mt)
	}
	running := []string{`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"x","name":"Bash","input":{"command":"sleep 5"}}]}}`}
	done := []string{`{"type":"assistant","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}}`}
	put("a1", `{"agentType":"general-purpose","description":"Review U1","model":"opus","requestShape":"background"}`, running, time.Minute)
	put("a2", `{"agentType":"Explore","description":"Done one"}`, done, time.Minute)
	put("a3", `{"agentType":"Explore","description":"Stale"}`, running, 2*time.Hour)
	var sc subagentCache
	got := sc.list(dir, time.Now())
	if len(got) != 1 {
		t.Fatalf("subagents = %+v", got)
	}
	s := got[0]
	if s.ID != "a1" || s.Desc != "Review U1" || s.Type != "general-purpose" || s.Model != "opus" || !s.BG || s.Doing != "Bash: sleep 5" {
		t.Fatalf("subagent = %+v", s)
	}
}

func TestTailChatStartsLoadEarlierWhereThePageBegins(t *testing.T) {
	var ls [][]byte
	for i := 0; i < 5; i++ {
		ls = append(ls, []byte(`{"type":"user","message":{"content":"m`+itoa(i)+`"}}`))
	}
	items, before, _ := tailChat(ls, 1000, 2, 0)
	if len(items) != 2 || items[0].Text != "m3" || items[1].Text != "m4" {
		t.Fatalf("items = %+v", items)
	}
	if want := int64(1000 + 3*(len(ls[0])+1)); before != want {
		t.Fatalf("before = %d, want %d (offset of the line with m3)", before, want)
	}
	if _, before, _ := tailChat(ls, 1000, 10, 0); before != 1000 {
		t.Fatalf("whole window: before = %d", before)
	}
}

func TestParseChatImagesAndEmptyThinking(t *testing.T) {
	items, _ := parseChat(lines([]string{
		`{"type":"user","timestamp":"2026-09-30T01:00:00.000Z","message":{"content":[{"type":"text","text":"what is this?"},{"type":"image","source":{"type":"base64","data":"AAAA"}},{"type":"image"}]}}`,
		`{"type":"assistant","timestamp":"2026-09-30T01:00:07.000Z","message":{"content":[{"type":"thinking","thinking":"","signature":"x"}]}}`,
	}))
	if len(items) != 2 || items[0].K != "user" || items[0].Text != "what is this?" || items[0].Img != 2 {
		t.Fatalf("user with images = %+v", items)
	}
	if items[1].K != "think" || items[1].Text != "" || items[1].Dur != 7 {
		t.Fatalf("redacted thinking = %+v", items[1])
	}
}

func TestLineDiff(t *testing.T) {
	lines, add, del := lineDiff("a\nb\nc", "a\nB\nc")
	if strings.Join(lines, "|") != " a|-b|+B| c" || add != 1 || del != 1 {
		t.Fatalf("diff = %q +%d -%d", lines, add, del)
	}
	old := "1\n2\n3\n4\n5\n6\n7\n8\n9\nold\n10"
	lines, add, del = lineDiff(old, strings.Replace(old, "old", "new", 1))
	if strings.Join(lines, "|") != "@@| 8| 9|-old|+new| 10" || add != 1 || del != 1 {
		t.Fatalf("context not trimmed: %q", lines)
	}
	lines, add, _ = lineDiff("", "x\ny")
	if strings.Join(lines, "|") != "+x|+y" || add != 2 {
		t.Fatalf("new file = %q", lines)
	}
}

func TestParseChatEditsCarryDiffs(t *testing.T) {
	items, _ := parseChat(lines([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"e1","name":"Edit","input":{"file_path":"/w/x.go","old_string":"a\nb","new_string":"a\nc\nd"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"w1","name":"Write","input":{"file_path":"/w/new.md","content":"# T\nbody\n"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"m1","name":"MultiEdit","input":{"file_path":"/w/y.go","edits":[{"old_string":"p","new_string":"q"},{"old_string":"r","new_string":"s"}]}}]}}`,
	}))
	e, w, m := items[0], items[1], items[2]
	if e.Path != "/w/x.go" || e.Add != 2 || e.Del != 1 || e.Diff != " a\n-b\n+c\n+d" {
		t.Fatalf("edit = %+v", e)
	}
	if w.Path != "/w/new.md" || w.Add != 2 || w.Del != 0 || w.Diff != "+# T\n+body" {
		t.Fatalf("write = %+v", w)
	}
	if m.Add != 2 || m.Del != 2 {
		t.Fatalf("multiedit = %+v", m)
	}
}

func TestTailChatKeepsTheLastHourFromTheStartOfItsTurn(t *testing.T) {
	at := func(min int) string {
		return time.Unix(1790730000+int64(min)*60, 0).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	user := func(min int, s string) []byte {
		return []byte(`{"type":"user","timestamp":"` + at(min) + `","message":{"content":"` + s + `"}}`)
	}
	text := func(min int, s string) []byte {
		return []byte(`{"type":"assistant","timestamp":"` + at(min) + `","message":{"content":[{"type":"text","text":"` + s + `"}]}}`)
	}
	now := int64(1790730000 + 180*60)
	ls := [][]byte{user(0, "old"), text(1, "old answer"), user(90, "turn start"), text(150, "inside the hour"), user(170, "latest")}
	items, before, _ := tailChat(ls, 0, 300, now-3600)
	var got []string
	for _, it := range items {
		got = append(got, it.Text)
	}
	if strings.Join(got, "|") != "turn start|inside the hour|latest" {
		t.Fatalf("items = %q", got)
	}
	if want := int64(len(ls[0]) + 1 + len(ls[1]) + 1); before != want {
		t.Fatalf("before = %d, want %d (where the kept turn starts)", before, want)
	}
	items, before, _ = tailChat(ls[:2], 0, 300, now-3600)
	if len(items) != 0 || before != int64(len(ls[0])+1+len(ls[1])+1) {
		t.Fatalf("an idle chat keeps nothing and pages back from its end: %d items, before=%d", len(items), before)
	}
}

func TestMentionedFilesAreRecentDocumentsThatExist(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	now := time.Now()
	mk := func(p string, age time.Duration) string {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
		os.Chtimes(p, now.Add(-age), now.Add(-age))
		return p
	}
	report := mk(filepath.Join(work, "out/report.html"), time.Minute)
	notes := mk(filepath.Join(home, "notes/plan.md"), time.Hour)
	mk(filepath.Join(work, "CLAUDE.md"), 72*time.Hour) // only read, long ago
	mk(filepath.Join(work, "app.go"), time.Minute)     // code, not a document
	text := "Done. The report is at `out/report.html` and the plan at [plan](~/notes/plan.md). " +
		"Per CLAUDE.md I left app.go alone; see https://example.com/x.html and /nope/missing.pdf."
	got := mentionedFiles(text, work, home, now.Unix())
	if strings.Join(got, ",") != report+","+notes {
		t.Fatalf("got %q", got)
	}
}

func TestParseChatReportsModelAndEffortWhenTheyChange(t *testing.T) {
	a := func(model, effort, text string) string {
		return `{"type":"assistant","effort":"` + effort + `","message":{"model":"` + model + `","content":[{"type":"text","text":"` + text + `"}]}}`
	}
	items, _ := parseChat(lines([]string{a("claude-opus-5-5", "xhigh", "one"), a("claude-opus-5-5", "xhigh", "two"), a("claude-sonnet-5-5", "high", "three")}))
	var got []string
	for _, it := range items {
		got = append(got, it.K+":"+it.Name+":"+it.Text)
	}
	if strings.Join(got, "|") != "model:xhigh:claude-opus-5-5|text::one|text::two|model:high:claude-sonnet-5-5|text::three" {
		t.Fatalf("items = %q", got)
	}
}

func TestPRNotesOnlyWhenTheLinkedPRChanges(t *testing.T) {
	pr := func(n, ts string) []byte {
		return []byte(`{"type":"pr-link","prNumber":` + n + `,"timestamp":"2026-10-01T10:00:0` + ts + `Z"}`)
	}
	items, _ := parseChat([][]byte{pr("10759", "1"), pr("10759", "2"), pr("10759", "3"), pr("10760", "4"), pr("10760", "5")})
	var notes []string
	for _, it := range items {
		notes = append(notes, it.Text)
	}
	if strings.Join(notes, ",") != "PR #10759,PR #10760" {
		t.Fatalf("notes = %v", notes)
	}
	last := "PR #10760" // the live stream: a later batch repeats the PR already shown
	more, _ := parseChat([][]byte{pr("10760", "6")})
	if got := newPRs(more, &last); len(got) != 0 {
		t.Fatalf("repeat in a later batch kept: %+v", got)
	}
	more, _ = parseChat([][]byte{pr("10761", "7")})
	if got := newPRs(more, &last); len(got) != 1 || last != "PR #10761" {
		t.Fatalf("new PR in a later batch: %+v, last %q", got, last)
	}
}

func TestSubagentTranscriptIsFoundByIDOrToolCall(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "sess.jsonl")
	subs := filepath.Join(dir, "sess", "subagents")
	os.MkdirAll(subs, 0o755)
	os.WriteFile(filepath.Join(subs, "agent-a1b2.jsonl"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(subs, "agent-a1b2.meta.json"), []byte(`{"agentType":"general-purpose","description":"Judge batch 0","toolUseId":"toolu_01X","model":"opus","requestShape":"background"}`), 0o644)
	want := filepath.Join(subs, "agent-a1b2.jsonl")
	if p, m, err := subagentPath(parent, "a1b2", ""); err != nil || p != want || m.ID != "a1b2" || m.Desc != "Judge batch 0" || m.Type != "general-purpose" || !m.BG {
		t.Fatalf("by id: %q %+v %v", p, m, err)
	}
	if p, m, err := subagentPath(parent, "", "toolu_01X"); err != nil || p != want || m.ID != "a1b2" {
		t.Fatalf("by tool call: %q %+v %v", p, m, err)
	}
	for _, c := range [][2]string{{"../../etc/passwd", ""}, {"a1b2/../x", ""}, {"nope", ""}, {"", "toolu_missing"}, {"", "../x"}, {"", ""}} {
		if _, _, err := subagentPath(parent, c[0], c[1]); err == nil {
			t.Errorf("subagentPath(%q, %q) should fail", c[0], c[1])
		}
	}
}
