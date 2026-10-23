package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Claude Code's dialogs, as dump-screen showed them in a throwaway session (2.1.295, a 48-column pane):
// testdata/dialogs.
func dialogScreen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/dialogs/" + name + ".txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseFooterlessDialogs(t *testing.T) {
	// "Switch model?" (the panel's model menu sends /model): no footer; a ▔ edge where the chat box was. Mid-turn
	// too, under the spinner.
	for _, f := range []string{"model", "model-busy"} {
		p := parsePrompt(dialogScreen(t, f))
		if p == nil {
			t.Fatalf("%s: no dialog found", f)
		}
		if p.Q != "Switch model?" || len(p.Opts) != 2 || p.Opts[0].Label != "Yes, switch to Sonnet 5.5" || !p.Opts[0].Sel || p.Opts[1].Label != "No, go back" {
			t.Fatalf("%s: %+v", f, p)
		}
		if got := strings.Join(p.Ctx, " | "); got != "Your next response will be slower and use more tokens | This conversation is cached for the current model. Switching to Sonnet 5.5 means the full history gets re-read on your next message." {
			t.Fatalf("%s: ctx = %q (wrapped lines are one sentence)", f, got)
		}
	}
	// a plan to approve: no footer either; the plan itself is context, and "Tell Claude what to change" is typed into
	p := parsePrompt(dialogScreen(t, "plan"))
	if p == nil || p.Q != "Claude has written up a plan and is ready to execute. Would you like to proceed?" {
		t.Fatalf("plan: %+v", p)
	}
	if got := strings.Join(p.Ctx, " | "); got != "Ready to code? | Here is Claude's plan: | Create b.txt in the project root containing y." {
		t.Fatalf("plan ctx = %q", got)
	}
	if len(p.Opts) != 3 || p.Opts[0].Label != "Yes, and use auto mode" || p.Opts[1].Label != "Yes, manually approve edits" ||
		!p.Opts[2].Text || p.Opts[2].Value != "" || p.Opts[2].Desc != "" {
		t.Fatalf("plan opts = %+v", p.Opts)
	}
	st, err := p.steps(Answer{Choose: p.Opts[2].Label, Text: "use z instead"})
	if err != nil || stepsString(st) != "Down Down Ctrl u | text:use z instead | Enter" {
		t.Fatalf("feedback on a plan: %q %v", stepsString(st), err)
	}
	typed := parsePrompt(dialogScreen(t, "plan-typed"))
	if typed == nil || len(typed.Opts) != 3 || !typed.Opts[2].Text || !typed.Opts[2].Sel || typed.Opts[2].Value != "use z instead" {
		t.Fatalf("plan with feedback typed: %+v", typed)
	}
}

func TestParsePermissionDialogs(t *testing.T) {
	p := parsePrompt(dialogScreen(t, "bash"))
	if p == nil || p.Q != "Do you want to proceed?" || len(p.Opts) != 4 || p.Opts[3].Label != "No" {
		t.Fatalf("bash: %+v", p)
	}
	ctx := strings.Join(p.Ctx, " | ")
	if !strings.Contains(ctx, "date > when.txt") || strings.Contains(ctx, "╌") || !strings.Contains(ctx, `Tip: auto mode handles these prompts for you — choose "switch to auto mode" below`) {
		t.Fatalf("bash ctx = %q (the command, no dashed rules, wrapped lines joined)", ctx)
	}
	w := parsePrompt(dialogScreen(t, "write"))
	if w == nil || w.Q != "Do you want to create a.txt?" || len(w.Opts) != 3 || !strings.Contains(strings.Join(w.Ctx, " | "), "Create file") {
		t.Fatalf("write: %+v", w)
	}
}

// Claude Code's countdown on a dialog ("will automatically deny this request in 1:38", 2.1.300) ticks every
// second: it's no part of the question, or an answer the page sends a second later names a question that's gone
// (409) and the card is rebuilt under the tap. It's when the dialog goes, instead.
func TestDenyCountdownIsNoPartOfTheQuestion(t *testing.T) {
	plain := parsePrompt(dialogScreen(t, "bash"))
	at := time.Now()
	a := parsePrompt(dialogScreen(t, "bash-deny"))
	b := parsePrompt(strings.Replace(dialogScreen(t, "bash-deny"), "1:38", "1:33", 1))
	if a == nil || b == nil || a.Sig != plain.Sig || b.Sig != plain.Sig || strings.Join(a.Ctx, " | ") != strings.Join(plain.Ctx, " | ") || len(a.Opts) != 4 {
		t.Fatalf("the countdown changed the question:\n%+v\n%+v", a, b)
	}
	if d := a.DenyAt - at.Add(98*time.Second).UnixMilli(); d < 0 || d > 1000 {
		t.Fatalf("denyAt is %d ms off from 1:38 from now", d)
	}
	if d := a.DenyAt - b.DenyAt; d < 4900 || d > 5100 {
		t.Fatalf("1:38 and 1:33 are %d ms apart", d)
	}
	if plain.DenyAt != 0 {
		t.Fatalf("no countdown, yet denyAt %d", plain.DenyAt)
	}
	// screen-reader mode words it roughly
	for s, want := range map[string]time.Duration{"about 2 minutes": 2 * time.Minute, "about 1 second": time.Second, "0:07": 7 * time.Second} {
		if d, ok := denyIn("⚠ Claude Code will automatically deny this request in " + s + ", to avoid blocking progress on an unattended session"); !ok || d != want {
			t.Errorf("%q: %v %v", s, d, ok)
		}
	}
}

// the panel reads a working agent's screen for dialogs now: a dialog's words in its conversation (a quoted
// footer, a numbered list) with the chat box under them are no dialog
func TestDialogTextInTheConversationIsNoDialog(t *testing.T) {
	box := "\n────────────────────────────────────────────────\n❯ \n────────────────────────────────────────────────\n  ⏵⏵ bypass permissions on\n"
	for _, f := range []string{"bash", "model", "plan"} {
		if p := parsePrompt(strings.TrimRight(dialogScreen(t, f), "\n ") + "\n" + box); p != nil {
			t.Errorf("%s quoted above the chat box: %+v", f, p)
		}
	}
}

// a dialog over a turn still going ("Switch model?" asked mid-turn): Claude Code's session says busy, yet the pane
// waits for an answer; once it's answered the turn reads as working again
func TestTermTrackerSeesADialogOverAWorkingTurn(t *testing.T) {
	tt, f := newTestTracker(t, map[string]string{
		"950.json": `{"pid":950,"sessionId":"aaaaaaaa-0950","procStart":"7","status":"busy"}`,
	}, map[string]string{
		"--session s action list-panes":  `[{"id":1,"tab_id":0}]`,
		"--session s action dump-screen": dialogScreen(t, "model-busy"),
	})
	tt.startOf = func(pid int) (procStat, bool) { return procStat{Start: 7}, true }
	res := Resources{panes: map[string]paneInfo{"pane:s:1": {Root: 11, Comm: "zsh", Agent: "claude"}}}
	keyOf := func(pid int) string { return map[int]string{950: "pane:s:1"}[pid] }
	now := time.Now()
	got := tt.update(now, []string{"s"}, res, keyOf, true)
	p := got.Sessions[0].Tabs[0].Panes[0]
	if p.State != "asking" || p.Agent == nil || p.Agent.Prompt == nil || p.Agent.Prompt.Q != "Switch model?" || got.Asking != 1 || got.Working != 0 {
		t.Fatalf("pane = %+v (counts %d asking, %d working)", p, got.Asking, got.Working)
	}
	f.answers["--session s action dump-screen"] = "· Bootstrapping…\n────────\n❯ \n────────\n" // answered
	got = tt.update(now.Add(6*time.Second), []string{"s"}, res, keyOf, true)
	if p := got.Sessions[0].Tabs[0].Panes[0]; p.State == "asking" || p.Agent.Prompt != nil || got.Asking != 0 || got.Working != 1 { // working (stalled here: the test pane has no output)
		t.Fatalf("after the answer: %+v", p)
	}
}
