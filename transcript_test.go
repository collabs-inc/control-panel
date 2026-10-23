package main

import (
	"os"
	"path/filepath"
	"sort"
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
	`{"type":"user","message":{"content":"<local-command-stdout>\u001b[2mLogin successful\u001b[22m</local-command-stdout>"}}`,
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
		`note:task:completed:Background command "Run suite" completed (exit code 0)`,
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
	refused := `{"type":"assistant","isApiErrorMessage":true,"message":{"model":"<synthetic>","stop_reason":"refusal","content":[{"type":"text","text":"API Error: safeguards flagged this message"}]}}`
	if _, final = lastTurn(lines([]string{refused})); !final {
		t.Fatal("a turn ended by a refusal / API error is over")
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
	parent := filepath.Join(t.TempDir(), "sess.jsonl")
	dir := filepath.Join(strings.TrimSuffix(parent, ".jsonl"), "subagents")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(parent, nil, 0o600)
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
	got := sc.list(parent, false, time.Now())
	if len(got) != 1 {
		t.Fatalf("subagents = %+v", got)
	}
	s := got[0]
	if s.ID != "a1" || s.Desc != "Review U1" || s.Type != "general-purpose" || s.Model != "opus" || !s.BG || s.Doing != "Bash: sleep 5" {
		t.Fatalf("subagent = %+v", s)
	}
}

func TestSubagentsThatEndedDropOff(t *testing.T) {
	var sc subagentCache
	parent := filepath.Join(t.TempDir(), "sess.jsonl")
	dir := filepath.Join(strings.TrimSuffix(parent, ".jsonl"), "subagents")
	os.MkdirAll(dir, 0o755)
	running := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"x","name":"Bash","input":{"command":"sleep 5"}}]}}`
	put := func(id, meta, body string) {
		os.WriteFile(filepath.Join(dir, "agent-"+id+".meta.json"), []byte(meta), 0o644)
		os.WriteFile(filepath.Join(dir, "agent-"+id+".jsonl"), []byte(body+"\n"), 0o600)
	}
	note := func(id, status string) string { // as Claude Code writes it into the parent's transcript
		return `{"type":"queue-operation","operation":"enqueue","content":"<task-notification>\n<task-id>` + id + `</task-id>\n<tool-use-id>toolu_1</tool-use-id>\n<status>` + status + `</status>\n<summary>Agent failed</summary>\n</task-notification>"}` + "\n"
	}
	put("bgfail", `{"description":"Batch 1","requestShape":"background"}`, running) // its transcript stops mid-tool
	put("bgrun", `{"description":"Batch 2","requestShape":"background"}`, running)
	put("fg", `{"description":"Foreground"}`, running)
	put("refused", `{"description":"Refused","requestShape":"background"}`, `{"type":"assistant","isApiErrorMessage":true,"message":{"stop_reason":"refusal","content":[{"type":"text","text":"API Error"}]}}`)
	os.WriteFile(parent, []byte(note("bgfail", "failed")), 0o600)
	ids := func(busy bool) string {
		var out []string
		for _, s := range sc.list(parent, busy, time.Now()) {
			out = append(out, s.ID)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	if got := ids(true); got != "bgrun,fg" {
		t.Fatalf("parent busy: %s (failed and refused ones are over)", got)
	}
	if got := ids(false); got != "bgrun" {
		t.Fatalf("parent idle: %s (a foreground subagent can't outlive its parent's turn)", got)
	}
	f, _ := os.OpenFile(parent, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(note("bgrun", "completed"))
	f.Close()
	if got := ids(false); got != "" {
		t.Fatalf("after a later notification: %s", got)
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
	form := mk(filepath.Join(work, "out/form.docx"), time.Minute)
	mk(filepath.Join(work, "CLAUDE.md"), 72*time.Hour) // only read, long ago
	mk(filepath.Join(work, "app.go"), time.Minute)     // code, not a document
	text := "Done. The report is at `out/report.html` and the plan at [plan](~/notes/plan.md). " +
		"The form is out/form.docx. Per CLAUDE.md I left app.go alone; see https://example.com/x.html and /nope/missing.pdf."
	got := mentionedFiles(text, work, home, now.Unix())
	if strings.Join(got, ",") != report+","+notes+","+form {
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

func TestContextUseAfterACompactionIsTheCompactedSize(t *testing.T) {
	used, model := contextUse(lines([]string{
		`{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":4,"cache_read_input_tokens":811000}}}`,
		`{"type":"user","message":{"content":"/compact"}}`,
		`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted","compactMetadata":{"trigger":"manual","preTokens":815889,"postTokens":17654}}`,
		`{"type":"user","isCompactSummary":true,"message":{"content":"This session is being continued..."}}`,
	}))
	if used != 17654 || model != "claude-opus-5-5" {
		t.Fatalf("after /compact: used %d of %q, want 17654 of the model it ran on", used, model)
	}
	used, _ = contextUse(lines([]string{
		`{"type":"system","subtype":"compact_boundary","compactMetadata":{"postTokens":17654}}`,
		`{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":3,"cache_read_input_tokens":21000}}}`,
	}))
	if used != 21003 {
		t.Fatalf("a request after the compaction counts again: %d", used)
	}
}

func TestContextUseIsTheLatestRealRequest(t *testing.T) {
	used, model := contextUse(lines([]string{
		`{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":5000,"output_tokens":99}}}`,
		`{"type":"user","message":{"content":"next"}}`,
		`{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":4,"cache_creation_input_tokens":1000,"cache_read_input_tokens":180000,"output_tokens":50}}}`,
		`{"type":"assistant","isSidechain":true,"message":{"model":"claude-haiku-4-5-20251001","usage":{"input_tokens":9,"cache_read_input_tokens":9}}}`,
		`{"type":"assistant","isApiErrorMessage":true,"message":{"model":"<synthetic>","usage":{"input_tokens":0}}}`,
	}))
	if used != 181004 || model != "claude-opus-5-5" {
		t.Fatalf("used %d of %q", used, model)
	}
	for _, c := range []struct {
		model string
		used  int64
		want  int64
	}{
		{"claude-opus-5-5", 181004, 1_000_000},
		{"claude-sonnet-5-5", 10, 1_000_000},
		{"claude-fable-5-1", 10, 1_000_000},
		{"claude-sonnet-4-6[1m]", 10, 1_000_000},
		{"claude-haiku-4-5-20251001", 43397, 200_000},
		{"claude-haiku-4-5-20251001", 250_000, 1_000_000}, // using more than 200k proves a bigger window
	} {
		if got := contextWindow(c.model, c.used); got != c.want {
			t.Errorf("contextWindow(%q, %d) = %d, want %d", c.model, c.used, got, c.want)
		}
	}
}

func TestBackgroundCommandsListUntilTheyEnd(t *testing.T) {
	var sc subagentCache
	parent := filepath.Join(t.TempDir(), "sess.jsonl")
	call := func(at, id, name, input string) string {
		return `{"type":"assistant","timestamp":"` + at + `","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}]}}` + "\n"
	}
	result := func(id, res string) string {
		return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"started"}]},"toolUseResult":` + res + `}` + "\n"
	}
	note := func(id, status string) string {
		return `{"type":"user","message":{"content":"<task-notification>\n<task-id>` + id + `</task-id>\n<status>` + status + `</status>\n<summary>done</summary>\n</task-notification>"}}` + "\n"
	}
	os.WriteFile(parent, []byte(
		call("2026-10-02T20:00:00Z", "t0", "Bash", `{"command":"make old","description":"Before a restart","run_in_background":true}`)+
			result("t0", `{"backgroundTaskId":"old1"}`)+
			call("2026-10-02T20:53:02Z", "t1", "Bash", `{"command":"pytest -q","description":"Run the tests","run_in_background":true}`)+
			result("t1", `{"backgroundTaskId":"bx1"}`)+
			call("2026-10-02T20:54:18Z", "t2", "Bash", `{"command":"until grep -q passed out; do sleep 5; done","description":"Wait for the tests","run_in_background":true}`)+
			result("t2", `{"backgroundTaskId":"bx2"}`)+
			call("2026-10-02T20:55:00Z", "t3", "Monitor", `{"command":"tail -f log","description":"Deploy events","timeout_ms":600000}`)+
			result("t3", `{"taskId":"mon1","timeoutMs":600000,"persistent":false}`)+
			call("2026-10-02T20:56:00Z", "t4", "Bash", `{"command":"ls"}`)+
			result("t4", `{"stdout":"a"}`)+
			note("bx2", "completed")), 0o600)
	started := time.Date(2026, 10, 2, 20, 30, 0, 0, time.UTC).Unix() // this Claude process started after "old1"
	desc := func(now time.Time) string {
		var out []string
		for _, s := range sc.shells(parent, started, now) {
			out = append(out, s.ID+":"+s.Kind+":"+s.Desc)
		}
		return strings.Join(out, ",")
	}
	at := time.Date(2026, 10, 2, 21, 0, 48, 0, time.UTC)
	if got := desc(at); got != "bx1:command:Run the tests,mon1:monitor:Deploy events" {
		t.Fatalf("running: %s", got)
	}
	if got := desc(at.Add(10 * time.Minute)); got != "bx1:command:Run the tests" {
		t.Fatalf("a monitor past its timeout is over: %s", got)
	}
	f, _ := os.OpenFile(parent, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(note("bx1", "completed"))
	f.Close()
	if got := desc(at); got != "mon1:monitor:Deploy events" {
		t.Fatalf("after the tests' notification: %s", got)
	}
}

func TestLongMessagesYouSentArriveWhole(t *testing.T) {
	long := strings.Repeat("Speaker 2: conflicting, removing, abstracting, and learning. ", 400) // ~24 KB, as pasted in a terminal
	items, _ := parseChat(lines([]string{
		`{"type":"user","message":{"content":"` + long + `"}}`,
		`{"type":"attachment","attachment":{"type":"queued_command","prompt":"` + long + `"}}`,
		`{"type":"queue-operation","operation":"enqueue","content":"` + long + `"}`,
	}))
	if len(items) != 3 {
		t.Fatalf("items = %d", len(items))
	}
	for _, it := range items {
		if strings.TrimSpace(it.Text) != strings.TrimSpace(long) {
			t.Fatalf("%s item cut to %d of %d bytes", it.K, len(it.Text), len(strings.TrimSpace(long)))
		}
	}
}

func TestEffortCommandSetsTheEffortAtOnce(t *testing.T) {
	items, _ := parseChat(lines([]string{
		`{"type":"assistant","effort":"xhigh","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"user","message":{"content":"<command-name>/effort</command-name>\n            <command-message>effort</command-message>\n            <command-args>high</command-args>"}}`,
		`{"type":"user","message":{"content":"<command-name>/effort</command-name>\n<command-args>bogus</command-args>"}}`,
	}))
	var got []string
	for _, it := range items {
		if it.K == "model" {
			got = append(got, it.Text+"/"+it.Name)
		}
	}
	if strings.Join(got, ",") != "claude-opus-5-5/xhigh,claude-opus-5-5/high" {
		t.Fatalf("model items = %v (an /effort applies before the next reply shows it; a bad one doesn't)", got)
	}
}

func TestWorkflowAgentsListUnderTheirWorkflow(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "-home-repos", "sess.jsonl")
	sess := strings.TrimSuffix(parent, ".jsonl")
	put := func(rel, body string) {
		p := filepath.Join(sess, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	os.WriteFile(parent, []byte(`{"type":"user","message":{"content":"go"}}`+"\n"), 0o600)
	working := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"x","name":"Bash","input":{"command":"curl portal"}}]}}` + "\n"
	done := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"s","name":"StructuredOutput","input":{}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"s","content":"Structured output provided successfully"}]}}` + "\n"
	meta := func(desc string) string {
		return `{"agentType":"workflow-subagent","description":"` + desc + `","workflowPhase":"Portal","requestShape":"foreground","model":"sonnet"}`
	}
	put("subagents/workflows/wf_run-1/agent-a1.jsonl", working)
	put("subagents/workflows/wf_run-1/agent-a1.meta.json", meta("portal:setup"))
	put("subagents/workflows/wf_run-1/agent-a2.jsonl", done)
	put("subagents/workflows/wf_run-1/agent-a2.meta.json", meta("portal:map"))
	put("subagents/workflows/wf_old-2/agent-b1.jsonl", working) // its workflow finished: not running, whatever the agent's file says
	put("subagents/workflows/wf_old-2/agent-b1.meta.json", meta("old"))
	put("workflows/wf_old-2.json", `{"runId":"wf_old-2"}`)
	other := filepath.Join(root, "-home-repos-app--worktrees-x", "sess", "workflows", "scripts") // the script can sit under another project folder
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "oracle-datapoint-endpoints-wf_run-1.js"), []byte("export const meta = {}"), 0o644)

	var sc subagentCache
	var got []string
	for _, s := range sc.list(parent, false, time.Now()) { // the parent idle: a workflow runs on without its turn
		got = append(got, s.ID+"@"+s.WF+":"+s.Desc)
	}
	if strings.Join(got, ",") != "a1@wf_run-1:portal:setup" {
		t.Fatalf("running workflow agents = %v (a structured output ends one; a finished workflow's are over)", got)
	}
	wfs := sc.workflows(parent, time.Now())
	if len(wfs) != 1 || wfs[0].ID != "wf_run-1" || wfs[0].Name != "oracle-datapoint-endpoints" || wfs[0].Done != 1 || wfs[0].Total != 2 {
		t.Fatalf("workflows = %+v", wfs)
	}
	if p, m, err := subagentPath(parent, "a1", ""); err != nil || !strings.HasSuffix(p, "wf_run-1/agent-a1.jsonl") || m.Desc != "portal:setup" {
		t.Fatalf("a workflow agent's chat opens like any subagent's: %q %+v %v", p, m, err)
	}
}

func TestTaskNotificationsAreNotesWithTheirOutcome(t *testing.T) {
	note := func(status, summary string) string {
		return `<task-notification>\n<task-id>bex85rjg6</task-id>\n<tool-use-id>toolu_1</tool-use-id>\n<output-file>/tmp/x.output</output-file>\n<status>` + status + `</status>\n<summary>` + summary + `</summary>\n</task-notification>`
	}
	items, _ := parseChat(lines([]string{
		`{"type":"attachment","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"` + note("failed", `Background command \"Check docker stack\" failed with exit code 144`) + `"}}`,
		`{"type":"user","message":{"content":"` + note("completed", `Background command \"Run tests\" completed (exit code 0)`) + `"}}`,
		`{"type":"attachment","attachment":{"type":"queued_command","prompt":"also do y"}}`,
	}))
	var got []string
	for _, it := range items {
		got = append(got, it.K+"|"+it.Name+"|"+it.Text)
	}
	want := `note|task:failed|Background command "Check docker stack" failed with exit code 144,note|task:completed|Background command "Run tests" completed (exit code 0),user||also do y`
	if strings.Join(got, ",") != want {
		t.Fatalf("items:\n%s\nwant:\n%s\n(a task's notice, mid-turn or not, is Claude Code talking to the agent: a note with its outcome, never your bubble)", strings.Join(got, "\n"), strings.ReplaceAll(want, ",", "\n"))
	}
}

func TestBackgroundTasksOfASubagentAreReadFromTheResultText(t *testing.T) {
	var sc subagentCache
	path := filepath.Join(t.TempDir(), "agent-a1.jsonl") // a workflow agent's transcript: no toolUseResult on results
	os.WriteFile(path, []byte(
		`{"type":"assistant","timestamp":"2026-10-05T21:06:00Z","message":{"content":[{"type":"tool_use","id":"m1","name":"Monitor","input":{"command":"tail -f lab.log","description":"Lighthouse runs","timeout_ms":1800000}}]}}`+"\n"+
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"m1","content":"Monitor started (task brol013ls, expires in 30m unless the source ends first; you get one notice at expiry)"}]}}`+"\n"+
			`{"type":"assistant","timestamp":"2026-10-05T21:07:00Z","message":{"content":[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"docker compose up","description":"Start the lab","run_in_background":true}}]}}`+"\n"+
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b1","content":"Command running in background with ID: bq8x2k1aa. Output is being written to: /tmp/x"}]}}`+"\n"), 0o600)
	start := time.Date(2026, 10, 5, 21, 0, 0, 0, time.UTC).Unix()
	var got []string
	for _, s := range sc.shells(path, start, time.Date(2026, 10, 5, 21, 10, 0, 0, time.UTC)) {
		got = append(got, s.ID+":"+s.Kind+":"+s.Desc)
	}
	if strings.Join(got, ",") != "brol013ls:monitor:Lighthouse runs,bq8x2k1aa:command:Start the lab" {
		t.Fatalf("shells = %v", got)
	}
	if n := len(sc.shells(path, start, time.Date(2026, 10, 5, 21, 40, 0, 0, time.UTC))); n != 1 {
		t.Fatalf("past its 30 m the monitor is over: %d left", n)
	}
}

func TestMonitorEventsKeepWhatHappened(t *testing.T) {
	items, _ := parseChat(lines([]string{
		`{"type":"user","timestamp":"2026-10-05T21:10:00Z","message":{"content":"<task-notification>\n<task-id>brol013ls</task-id>\n<summary>Monitor event: \"swarm agents finishing\"</summary>\n<event>DONE portal:map-records (14 endpoints) &amp; 2 left</event>\n</task-notification>"}}`,
	}))
	if len(items) != 1 || items[0].Name != "task:event" || items[0].Out != "DONE portal:map-records (14 endpoints) & 2 left" {
		t.Fatalf("items = %+v", items)
	}
}

func TestAChatOpensOnItsLastItemsAndPagesBackInSmallSteps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	var b strings.Builder
	for i := 0; i < 750; i++ {
		b.WriteString(`{"type":"user","message":{"content":"m` + itoa(i) + `"}}` + "\n")
	}
	os.WriteFile(path, []byte(b.String()), 0o600)
	items, before, end, _ := firstChat(path)
	if len(items) != chatFirst || items[len(items)-1].Text != "m749" || end != int64(b.Len()) {
		t.Fatalf("first window: %d items, last %q, end %d", len(items), items[len(items)-1].Text, end)
	}
	all := items
	for pages := 0; before > 0; pages++ {
		if pages > 5 {
			t.Fatal("paging doesn't end")
		}
		var older []ChatItem
		older, before = earlierChat(path, before)
		if len(older) > chatFirst {
			t.Fatalf("a page of %d items", len(older))
		}
		all = append(older, all...)
	}
	if len(all) != 750 || all[0].Text != "m0" || all[749].Text != "m749" {
		t.Fatalf("paged back to %d items, %q … %q", len(all), all[0].Text, all[len(all)-1].Text)
	}
	for i, it := range all {
		if it.Text != "m"+itoa(i) {
			t.Fatalf("item %d is %q: a gap or a repeat", i, it.Text)
		}
	}
}

// Claude Code 2.1.29x attaches the images a pasted path names: [Image #N] in the text, the picture, and right after
// it an entry of its own (isMeta) saying where each came from — or, in the same message, a text block. Either rides
// on the message (its path: the page shows the picture); neither is a message of its own.
func TestParseChatImageSourcesRideOnTheMessage(t *testing.T) {
	items, _ := parseChat(lines([]string{
		`{"type":"user","timestamp":"2026-10-10T00:38:00.000Z","message":{"content":[{"type":"text","text":"[Image #4] use this logo"},{"type":"image","source":{"type":"base64","data":"AAAA"}}]}}`,
		`{"type":"attachment","attachment":{"type":"something"}}`,
		`{"type":"user","isMeta":true,"timestamp":"2026-10-10T00:38:00.000Z","message":{"content":[{"type":"text","text":"[Image: source: /workspace/home/.local/state/control-panel/uploads/20261010-003505-d90e813611fb2709-logo.png]"}]}}`,
		`{"type":"user","timestamp":"2026-10-10T00:39:00.000Z","message":{"content":[{"type":"image","source":{"type":"base64","data":"AAAA"}},{"type":"text","text":"[Image: source: /tmp/elsewhere/shot.png]"}]}}`,
	}))
	if len(items) != 2 {
		t.Fatalf("items = %+v (the sources are no messages of their own)", items)
	}
	if it := items[0]; it.K != "user" || it.Img != 1 || it.Text != "[Image #4] use this logo\n/workspace/home/.local/state/control-panel/uploads/20261010-003505-d90e813611fb2709-logo.png" {
		t.Fatalf("message with its image = %+v", it)
	}
	if it := items[1]; it.K != "user" || it.Img != 1 || it.Text != "/tmp/elsewhere/shot.png" {
		t.Fatalf("an image alone, from outside the uploads = %+v", it)
	}
}

func TestTailFollowsTheFolderClaudeMovedTo(t *testing.T) {
	root := t.TempDir()
	sid := "f5bafd4f-8844-4ae4-bc85-832fa542a1be"
	p := filepath.Join(root, "-r", sid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(`{"type":"user","cwd":"/r","message":{"content":"cd into a"}}
{"type":"assistant","cwd":"/r","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"cd /r/a"}}]}}
{"type":"user","cwd":"/r/a","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
{"type":"assistant","isSidechain":true,"cwd":"/elsewhere","message":{"content":"a subagent's"}}
{"type":"last-prompt","lastPrompt":"cd into a"}
`), 0o644)
	tr := newTranscripts(root)
	if ti, _ := tr.tail(sid); ti.cwd != "/r/a" {
		t.Fatalf("cwd %q, want where its shell is now", ti.cwd)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(strings.Repeat(`{"type":"progress","data":"`+strings.Repeat("x", 1000)+`"}`+"\n", 300)) // the cwd scrolls out of the last 256 KB
	f.Close()
	if ti, _ := tr.tail(sid); ti.cwd != "/r/a" {
		t.Fatalf("cwd %q, want the last known kept", ti.cwd)
	}
}

// A turn can start without you: another Claude session's message, a scheduled prompt. Claude Code keeps those as
// hidden (isMeta) user entries; dropping them merged their turn into the one before, and that one's answer ended
// up folded away as narration. Between turns they show as what started the turn; inside one, hidden ones stay out.
func TestParseChatShowsWhatStartedATurn(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"user","timestamp":"2026-10-13T01:00:00Z","message":{"role":"user","content":"ship it"}}`),
		[]byte(`{"type":"user","isMeta":true,"timestamp":"2026-10-13T01:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /x"}]}}`),
		[]byte(`{"type":"assistant","timestamp":"2026-10-13T01:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"Shipped."}]}}`),
		[]byte(`{"type":"system","subtype":"turn_duration","durationMs":5000,"timestamp":"2026-10-13T01:00:06Z"}`),
		[]byte(`{"type":"user","isMeta":true,"origin":{"kind":"peer","name":"home-04","body":"Restarting the panel now."},"timestamp":"2026-10-13T01:01:00Z","message":{"role":"user","content":"Another Claude session sent a message: <cross-session-message>Restarting the panel now.</cross-session-message>"}}`),
		[]byte(`{"type":"assistant","timestamp":"2026-10-13T01:01:02Z","message":{"role":"assistant","content":[{"type":"text","text":"Noted: nothing to do."}]}}`),
		[]byte(`{"type":"system","subtype":"turn_duration","durationMs":2000,"timestamp":"2026-10-13T01:01:03Z"}`),
		[]byte(`{"type":"user","isMeta":true,"timestamp":"2026-10-13T01:02:00Z","message":{"role":"user","content":"KPI recon 30-min tick: check the run"}}`),
		[]byte(`{"type":"assistant","timestamp":"2026-10-13T01:02:09Z","message":{"role":"assistant","content":[{"type":"text","text":"The run is green."}]}}`),
		[]byte(`{"type":"system","subtype":"turn_duration","durationMs":9000,"timestamp":"2026-10-13T01:02:10Z"}`),
	}
	items, _ := parseChat(lines)
	var got []string
	for _, it := range items {
		got = append(got, it.K+"|"+it.Via+"|"+it.Text)
	}
	want := []string{
		"user||ship it", "text||Shipped.", "note||Worked for 5s",
		"user|peer:home-04|Restarting the panel now.", "text||Noted: nothing to do.", "note||Worked for 2s",
		"user|auto|KPI recon 30-min tick: check the run", "text||The run is green.", "note||Worked for 9s",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("items:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The live chat parses each batch of new lines on its own: whatever the batches, the items are the same as one
// parse of the whole transcript (a hidden entry inside a turn never shows as a turn's start).
func TestParseChatInBatchesMatchesOneParse(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"user","timestamp":"2026-10-13T01:00:00Z","message":{"role":"user","content":"ship it"}}`),
		[]byte(`{"type":"assistant","timestamp":"2026-10-13T01:00:02Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Skill","input":{}}]}}`),
		[]byte(`{"type":"user","isMeta":true,"timestamp":"2026-10-13T01:00:03Z","message":{"role":"user","content":[{"type":"text","text":"Goal check-in: a hidden prompt mid-turn"}]}}`),
		[]byte(`{"type":"assistant","timestamp":"2026-10-13T01:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"Shipped."}]}}`),
		[]byte(`{"type":"system","subtype":"turn_duration","durationMs":5000,"timestamp":"2026-10-13T01:00:06Z"}`),
		[]byte(`{"type":"user","isMeta":true,"origin":{"kind":"peer","name":"home-04","body":"Restarting."},"timestamp":"2026-10-13T01:01:00Z","message":{"role":"user","content":"x"}}`),
		[]byte(`{"type":"assistant","timestamp":"2026-10-13T01:01:02Z","message":{"role":"assistant","content":[{"type":"text","text":"Noted."}]}}`),
		[]byte(`{"type":"system","subtype":"turn_duration","durationMs":2000,"timestamp":"2026-10-13T01:01:03Z"}`),
	}
	key := func(items []ChatItem) string {
		var s []string
		for _, it := range items {
			if it.K != "model" {
				s = append(s, it.K+"|"+it.Via+"|"+it.Text)
			}
		}
		return strings.Join(s, "\n")
	}
	whole, _ := parseChat(lines)
	for cut := 1; cut < len(lines); cut++ {
		a, _ := parseChat(lines[:cut])
		b, _ := parseChatOpen(lines[cut:], turnOpen(a, false))
		if got := key(append(a, b...)); got != key(whole) {
			t.Fatalf("cut at %d:\n%s\nwant:\n%s", cut, got, key(whole))
		}
	}
}

// latest and newestTitle read from the end what parseChat would make of every line: its last text item and its
// title. Every window of the fixture must give the same answer both ways.
func TestLatestAndTitleFromTheEndMatchParseChat(t *testing.T) {
	long := strings.Repeat("word ", 4000) + "Should I go on?"
	lines := []string{
		`{"type":"ai-title","aiTitle":"First title"}`,
		`{"type":"assistant","timestamp":"2026-09-30T01:00:01Z","message":{"content":[{"type":"text","text":"  first answer  "}]}}`,
		`{"type":"user","timestamp":"2026-09-30T01:00:02Z","message":{"content":"say \"assistant\" and \"ai-title\" back"}}`,
		`not json, "assistant"`,
		`{"type":"assistant","timestamp":"2026-09-30T01:00:03Z","message":{"content":[{"type":"text","text":"before the tool"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"ai-title","aiTitle":"Second title"}`,
		`{"type":"assistant","timestamp":"2026-09-30T01:00:04Z","message":{"content":[{"type":"text","text":"   "},{"type":"thinking","thinking":"hmm"}]}}`,
		`{"type":"assistant","timestamp":"2026-09-30T01:00:05Z","message":{"content":"` + long + `"}}`,
		`{"type":"ai-title","aiTitle":""}`,
		`{"type":"assistant","timestamp":"2026-09-30T01:00:06Z","message":{"content":[{"type":"text","text":"one"},{"type":"text","text":"two: want me to merge it?"},{"type":"text","text":""}]}}`,
		`{"type":"assistant","timestamp":"2026-09-30T01:00:07Z","message":{"content":[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/x"}}]}}`,
	}
	for from := range lines {
		for to := from; to <= len(lines); to++ {
			var ls [][]byte
			for _, l := range lines[from:to] {
				ls = append(ls, []byte(l))
			}
			items, title := parseChat(ls)
			var want, got tailInfo
			for i := len(items) - 1; i >= 0; i-- { // what latest kept before: the last text item
				if it := items[i]; it.K == "text" && strings.TrimSpace(it.Text) != "" {
					want.claude, want.claudeAt, want.asks = trunc(it.Text, 1500), it.T, asksYou(it.Text)
					break
				}
			}
			got.latest(ls)
			if got != want || newestTitle(ls) != title {
				t.Fatalf("lines %d..%d: %+v, %q; parseChat: %+v, %q", from, to, got, newestTitle(ls), want, title)
			}
		}
	}
}
