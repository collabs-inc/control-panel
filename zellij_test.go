package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeZellij answers zellij argv with canned output and records every call.
type fakeZellij struct {
	calls    [][]string
	dirs     []string
	answers  map[string]string // joined argv prefix → stdout
	attached []string
	onCall   func(args []string) // a test's hook: the pane reacting to keys (a cleared box)
}

func (f *fakeZellij) run(_ context.Context, dir string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.onCall != nil {
		f.onCall(args)
	}
	f.dirs = append(f.dirs, dir)
	j := strings.Join(args, " ")
	best := ""
	for k := range f.answers {
		if strings.HasPrefix(j, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return nil, nil
	}
	if strings.HasPrefix(f.answers[best], "ERR:") {
		return nil, errors.New(f.answers[best][4:])
	}
	return []byte(f.answers[best]), nil
}

func (f *fakeZellij) attach(session string, cols, rows int) (func(), error) {
	f.attached = append(f.attached, session+" "+itoa(cols)+"x"+itoa(rows))
	return func() { f.attached = append(f.attached, "detach") }, nil
}

func newFakeZellij(answers map[string]string) (*zellij, *fakeZellij) {
	f := &fakeZellij{answers: answers}
	return &zellij{run: f.run, attach: f.attach}, f
}

const listPanesFixture = `[
 {"id":0,"is_plugin":true,"is_suppressed":true,"title":"status","tab_id":0,"tab_position":0,"tab_name":"PMF CGP"},
 {"id":0,"is_plugin":false,"is_focused":true,"is_floating":false,"is_suppressed":false,"title":"~/repo","exited":false,"exit_status":null,"is_held":false,"pane_content_rows":57,"pane_content_columns":253,"terminal_command":null,"tab_id":0,"tab_position":0,"tab_name":"PMF CGP"},
 {"id":7,"is_plugin":false,"is_focused":false,"is_floating":true,"is_suppressed":false,"title":"htop","exited":true,"exit_status":1,"is_held":true,"pane_content_rows":20,"pane_content_columns":80,"terminal_command":"htop","tab_id":3,"tab_position":1,"tab_name":"Logs"},
 {"id":8,"is_plugin":false,"is_suppressed":true,"title":"replaced","tab_id":3,"tab_position":1,"tab_name":"Logs"}
]`

func TestZellijPanes(t *testing.T) {
	z, f := newFakeZellij(map[string]string{"--session dev-1 action list-panes": listPanesFixture})
	got, err := z.panes("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.calls[0], []string{"--session", "dev-1", "action", "list-panes", "--json", "--all"}) {
		t.Fatalf("argv = %v", f.calls[0])
	}
	if len(got) != 2 {
		t.Fatalf("want 2 terminal panes (plugins and suppressed dropped), got %+v", got)
	}
	a, b := got[0], got[1]
	if a.ID != 0 || a.TabID != 0 || a.TabName != "PMF CGP" || a.Cols != 253 || a.Rows != 57 || !a.Focused || a.Command != "" {
		t.Fatalf("a = %+v", a)
	}
	if b.ID != 7 || !b.Floating || !b.Exited || !b.Held || b.ExitStatus == nil || *b.ExitStatus != 1 || b.Command != "htop" || b.TabPos != 1 {
		t.Fatalf("b = %+v", b)
	}
}

func TestZellijCommandsArgv(t *testing.T) {
	z, f := newFakeZellij(map[string]string{
		"--session dev-1 action new-pane": "terminal_12\n",
	})
	if err := z.paste("dev-1", 3, "-rf looks like a flag"); err != nil {
		t.Fatal(err)
	}
	if err := z.keys("dev-1", 3, "Shift Tab", "Enter"); err != nil {
		t.Fatal(err)
	}
	id, err := z.newPane("dev-1", 2, "logs", "/tmp", "right")
	if err != nil || id != 12 {
		t.Fatalf("newPane = %d, %v", id, err)
	}
	if err := z.newSession("scratch", "/tmp"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"--session", "dev-1", "action", "paste", "--pane-id", "terminal_3", "--", "-rf looks like a flag"},
		{"--session", "dev-1", "action", "send-keys", "--pane-id", "terminal_3", "Shift Tab", "Enter"},
		{"--session", "dev-1", "action", "new-pane", "--tab-id", "2", "--no-focus", "--cwd", "/tmp", "--name", "logs", "--direction", "right"},
		{"attach", "--create-background", "scratch"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("argv:\n%v\nwant\n%v", f.calls, want)
	}
	if f.dirs[3] != "/tmp" {
		t.Fatalf("new session should start in the chosen dir, got %q", f.dirs[3])
	}
}

func TestZellijNewTabAttachesHeadlessClientWhenDetached(t *testing.T) {
	z, f := newFakeZellij(map[string]string{
		"--session dev-1 action list-clients": "CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n",
		"--session dev-1 action list-tabs":    `[{"tab_id":0,"position":0,"name":"a","display_area_columns":255,"display_area_rows":59}]`,
		"--session dev-1 action new-tab":      "4\n",
		"--session dev-1 action list-panes":   `[{"id":21,"is_plugin":false,"title":"zsh","tab_id":4,"tab_position":1,"tab_name":"work"}]`,
	})
	id, err := z.newTab("dev-1", "work", "/tmp")
	if err != nil || id != 21 {
		t.Fatalf("newTab = %d, %v", id, err)
	}
	if !reflect.DeepEqual(f.attached, []string{"dev-1 255x59", "detach"}) {
		t.Fatalf("headless client: %v", f.attached)
	}
}

func TestZellijNewTabUsesAttachedClient(t *testing.T) {
	z, f := newFakeZellij(map[string]string{
		"--session dev-1 action list-clients": "CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1 terminal_3 zsh\n",
		"--session dev-1 action new-tab":      "4\n",
		"--session dev-1 action list-panes":   `[{"id":21,"is_plugin":false,"title":"zsh","tab_id":4,"tab_position":1,"tab_name":"work"}]`,
	})
	if id, err := z.newTab("dev-1", "", "/tmp"); err != nil || id != 21 {
		t.Fatalf("newTab = %d, %v", id, err)
	}
	if len(f.attached) != 0 {
		t.Fatalf("attached a client although one is connected: %v", f.attached)
	}
}

const promptScreen = `  ⎿  Waiting…
╭──────────────────────────────────────────╮
 Bash command
   │ F=/tmp/x; rm -f $F/progress.txt $F/*.done; nohup $F/run_shards.sh &
   │ sleep 5; cat $F/progress.txt
   Start the serial shard runner
 │ Dangerous rm operation on possibly-empty variable path

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don't ask again for rm commands
   3. No
 Esc to cancel · Tab to amend
`

func TestParsePrompt(t *testing.T) {
	p := parsePrompt(promptScreen)
	if p == nil {
		t.Fatal("no prompt found")
	}
	if p.Q != "Do you want to proceed?" {
		t.Fatalf("q = %q", p.Q)
	}
	if len(p.Opts) != 3 || p.Opts[0] != (PromptOpt{N: "1", Label: "Yes", Sel: true}) || p.Opts[2] != (PromptOpt{N: "3", Label: "No"}) {
		t.Fatalf("opts = %+v", p.Opts)
	}
	if len(p.Ctx) == 0 || p.Ctx[len(p.Ctx)-1] != "Dangerous rm operation on possibly-empty variable path" {
		t.Fatalf("ctx = %q", p.Ctx)
	}
}

func TestParsePromptIgnoresNumberedListsInOutput(t *testing.T) {
	idle := "Plan:\n1. Read the code\n2. Fix it\n\n✻ Worked for 2m 8s\n───────\n❯ \n───────\n  ⏵⏵ bypass permissions on\n"
	if p := parsePrompt(idle); p != nil {
		t.Fatalf("false prompt: %+v", p)
	}
}

const trustScreen = `claude-multi
➜  /tmp claude-multi

────────────────────────────────────────────────
 Accessing workspace:

 /tmp

 Quick safety check: Is this a project you created or one you trust? (Like
 your own code, a well-known open source project, or work from your team). If
 not, take a moment to review what's in this folder first.

 Claude Code'll be able to read, edit, and execute files here.

 Security guide

 ❯ No, exit
   Yes, I trust this folder

 Enter to confirm · Esc to cancel



`

func TestParsePromptUnnumberedChoices(t *testing.T) {
	p := parsePrompt(trustScreen)
	if p == nil {
		t.Fatal("no prompt")
	}
	want := []PromptOpt{{Label: "No, exit", Sel: true}, {Label: "Yes, I trust this folder"}}
	if !reflect.DeepEqual(p.Opts, want) {
		t.Fatalf("opts = %+v", p.Opts)
	}
	if !strings.HasPrefix(p.Q, "Quick safety check: Is this a project") {
		t.Fatalf("q = %q", p.Q)
	}
	if len(p.Ctx) == 0 || p.Ctx[0] != "Accessing workspace:" {
		t.Fatalf("ctx = %q", p.Ctx)
	}
}

const askScreen = ` ☐ Deploy

 Which environment should I deploy to?

 ❯ 1. Staging
      Safe to break
   2. Production
      Real customers
   3. Type something.

 Enter to select · ↑/↓ to navigate · Esc to cancel
`

func TestParsePromptQuestionWithDescriptions(t *testing.T) {
	p := parsePrompt(askScreen)
	if p == nil || p.Q != "Which environment should I deploy to?" {
		t.Fatalf("prompt = %+v", p)
	}
	want := []PromptOpt{{N: "1", Label: "Staging", Sel: true, Desc: "Safe to break"}, {N: "2", Label: "Production", Desc: "Real customers"}, {N: "3", Label: "Type something.", Text: true}}
	if !reflect.DeepEqual(p.Opts, want) {
		t.Fatalf("opts = %+v", p.Opts)
	}
}

func TestChooseKeys(t *testing.T) {
	keys := func(p *Prompt, label string) string {
		st, err := p.steps(Answer{Choose: label})
		if err != nil {
			return "error: " + err.Error()
		}
		return stepsString(st)
	}
	p := parsePrompt(trustScreen)
	if k := keys(p, "Yes, I trust this folder"); k != "Down Enter" {
		t.Fatalf("keys = %q", k)
	}
	if k := keys(p, "No, exit"); k != "Enter" {
		t.Fatalf("keys = %q", k)
	}
	if k := keys(parsePrompt(promptScreen), "No"); k != "Down Down Enter" {
		t.Fatalf("numbered keys = %q (arrows, never the digit: a free-text option would take it as text)", k)
	}
	if k := keys(p, "Maybe"); !strings.HasPrefix(k, "error") {
		t.Fatal("unknown label accepted")
	}
}

// Claude Code's AskUserQuestion with several questions: their tabs on top, a description under each option,
// the free-text option (here holding "44", typed earlier) and "Chat about this" below a rule.
const askTabsScreen = `● Checking for any notes on Baker's PMS.
────────────────────────────────────────────────
←  ☐ Baker PMS  ☐ Fleet 10DLC  ☐ Test SMS  ✔ Submit  →

Which PMS is Baker on (the 'new PMS integration')?

  1. AppFolio
     Supported today; Block 2 can run right away.
  2. Buildium
     Supported; same Block 2 path.
  3. Rent Manager
     Check support level first.
❯ 4. 44
────────────────────────────────────────────────
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · ctrl+g to edit in Vim · Esc to cancel
`

// a multi-select question in a narrow pane: checkboxes, "Next" under the free-text option, the footer wrapped
const askMultiScreen = `────────────────────
←  ☒ Color  ☐ Pets  ☐ Size  ✔ Submit  →

Which pets do you have?

  1. [✔] Cat
         Cat
  2. [ ] Dog
         Dog
❯ 3. [ ] Fish
         Fish
  4. [ ] Type something
     Next
────────────────────
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate ·
Esc to cancel
`

// the last step: the answers to review, and no footer at all
const askReviewScreen = `────────────────────
←  ☒ Color  ☒ Pets  ☒ Size  ✔ Submit  →

Review your answers

 ● Which color do you prefer?
   → Purple, like a real aurora
 ● Which pets do you have?
   → Cat, Fish

Ready to submit your answers?

❯ 1. Submit answers
  2. Cancel


`

func TestParseAskWithTabsDescriptionsAndFreeText(t *testing.T) {
	p := parsePrompt(askTabsScreen)
	if p == nil || p.Q != "Which PMS is Baker on (the 'new PMS integration')?" || len(p.Ctx) != 0 || p.Multi {
		t.Fatalf("prompt = %+v", p)
	}
	if want := []PromptTab{{"Baker PMS", false}, {"Fleet 10DLC", false}, {"Test SMS", false}}; !reflect.DeepEqual(p.Tabs, want) {
		t.Fatalf("tabs = %+v", p.Tabs)
	}
	want := []PromptOpt{
		{N: "1", Label: "AppFolio", Desc: "Supported today; Block 2 can run right away."},
		{N: "2", Label: "Buildium", Desc: "Supported; same Block 2 path."},
		{N: "3", Label: "Rent Manager", Desc: "Check support level first."},
		{N: "4", Label: "44", Sel: true, Text: true, Value: "44"},
		{N: "5", Label: "Chat about this"},
	}
	if !reflect.DeepEqual(p.Opts, want) {
		t.Fatalf("opts =\n%+v\nwant\n%+v", p.Opts, want)
	}
	// the placeholder is no answer
	if q := parsePrompt(strings.Replace(askTabsScreen, "❯ 4. 44", "❯ 4. Type something.", 1)); q.Opts[3].Value != "" || !q.Opts[3].Text {
		t.Fatalf("placeholder = %+v", q.Opts[3])
	}
}

func TestParseAskMultiSelect(t *testing.T) {
	p := parsePrompt(askMultiScreen)
	if p == nil || p.Q != "Which pets do you have?" || !p.Multi {
		t.Fatalf("prompt = %+v", p)
	}
	if want := []PromptTab{{"Color", true}, {"Pets", false}, {"Size", false}}; !reflect.DeepEqual(p.Tabs, want) {
		t.Fatalf("tabs = %+v", p.Tabs)
	}
	want := []PromptOpt{
		{N: "1", Label: "Cat", Box: true, Checked: true},
		{N: "2", Label: "Dog", Box: true},
		{N: "3", Label: "Fish", Sel: true, Box: true},
		{N: "4", Label: "Type something", Box: true, Text: true},
		{Label: "Next", Next: true},
		{N: "5", Label: "Chat about this"},
	}
	if !reflect.DeepEqual(p.Opts, want) {
		t.Fatalf("opts =\n%+v\nwant\n%+v", p.Opts, want)
	}
}

func TestParseAskReview(t *testing.T) {
	p := parsePrompt(askReviewScreen)
	if p == nil || p.Q != "Ready to submit your answers?" {
		t.Fatalf("prompt = %+v", p)
	}
	if want := []PromptQA{{"Which color do you prefer?", "Purple, like a real aurora"}, {"Which pets do you have?", "Cat, Fish"}}; !reflect.DeepEqual(p.Review, want) {
		t.Fatalf("review = %+v", p.Review)
	}
	if len(p.Opts) != 2 || p.Opts[0].Label != "Submit answers" || !p.Opts[0].Sel {
		t.Fatalf("opts = %+v", p.Opts)
	}
	// without the question tabs, a numbered list at the bottom of the screen is no question
	if q := parsePrompt(strings.Replace(askReviewScreen, "←  ☒ Color  ☒ Pets  ☒ Size  ✔ Submit  →", "", 1)); q != nil {
		t.Fatalf("false prompt: %+v", q)
	}
}

func stepsString(st []keyStep) string {
	var out []string
	for _, s := range st {
		if s.Text != "" {
			out = append(out, "text:"+s.Text)
		} else {
			out = append(out, strings.Join(s.Keys, " "))
		}
	}
	return strings.Join(out, " | ")
}

func TestAnswerSteps(t *testing.T) {
	ask, multi := parsePrompt(askTabsScreen), parsePrompt(askMultiScreen)
	for _, c := range []struct {
		p    *Prompt
		a    Answer
		want string
	}{
		{ask, Answer{Choose: "AppFolio"}, "Up Up Up Enter"},
		{ask, Answer{Choose: "Chat about this"}, "Down Enter"},
		// the free text: cleared first (it held "44"), then pasted, then sent
		{ask, Answer{Choose: "44", Text: "Yardi\nBreeze"}, "Ctrl u | text:Yardi Breeze | Enter"},
		// ticks to change one by one, the free text (typing ticks it), then Next
		{multi, Answer{Checks: []string{"Dog", "Fish"}, Text: "Parrot"}, "Up Up Enter | Down Enter | Down Enter | Down Ctrl u | text:Parrot | Down Enter"},
		{multi, Answer{Checks: []string{"Cat"}}, "Down Down Enter"},
		{multi, Answer{Choose: "Chat about this"}, "Down Down Down Enter"},
	} {
		st, err := c.p.steps(c.a)
		if err != nil || stepsString(st) != c.want {
			t.Errorf("%+v: %q %v, want %q", c.a, stepsString(st), err, c.want)
		}
		for _, s := range st {
			for _, k := range s.Keys {
				if len(k) == 1 && k[0] >= '0' && k[0] <= '9' {
					t.Errorf("%+v: a digit key %q (the free-text option would take it as text)", c.a, k)
				}
			}
		}
	}
	for _, a := range []Answer{{Choose: "Maybe"}, {Choose: "44"}, {Checks: []string{"Hamster"}}} {
		p := ask
		if a.Checks != nil {
			p = multi
		}
		if _, err := p.steps(a); err == nil {
			t.Errorf("%+v accepted", a)
		}
	}
}

func TestPromptSignature(t *testing.T) {
	a, b := parsePrompt(promptScreen), parsePrompt(strings.Replace(promptScreen, "sleep 5", "sleep 9", 1))
	if a.Sig == "" || a.Sig == b.Sig || a.Sig != parsePrompt(promptScreen).Sig {
		t.Fatalf("sigs: %q %q", a.Sig, b.Sig)
	}
}

func TestZellijCloseArgv(t *testing.T) {
	z, f := newFakeZellij(nil)
	z.closePane("dev-1", 4)
	z.closeTab("dev-1", 2)
	z.deleteSession("scratch")
	want := [][]string{
		{"--session", "dev-1", "action", "close-pane", "--pane-id", "terminal_4"},
		{"--session", "dev-1", "action", "close-tab-by-id", "2"},
		{"delete-session", "--force", "scratch"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("argv = %v", f.calls)
	}
}

func TestTabNamesDropTheStatusPluginsMark(t *testing.T) {
	for in, want := range map[string]string{
		"PMF Compliance Skill UK ●": "PMF Compliance Skill UK",
		"Fix CI ⚡️":                 "Fix CI",
		"Fix CI ⏸":                  "Fix CI",
		"Fix CI ✓":                  "Fix CI",
		"New ○":                     "New",
		"PMF CGP":                   "PMF CGP",
		"✓ done list":               "✓ done list",
	} {
		if got := tabName(in); got != want {
			t.Errorf("tabName(%q) = %q, want %q", in, got, want)
		}
	}
}
