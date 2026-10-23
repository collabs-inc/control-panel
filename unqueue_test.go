package main

import (
	"reflect"
	"testing"
)

func TestPoppedReadsWhatUpTookOutOfTheQueue(t *testing.T) {
	ls := lines([]string{
		`{"type":"queue-operation","operation":"enqueue","content":"A"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"\"popAll\""}]}}`,
		`{"type":"queue-operation","operation":"popAll","content":"A"}`,
		`{"type":"queue-operation","operation":"popAll","content":"B\nsecond line"}`,
	})
	if got := popped(ls); !reflect.DeepEqual(got, []string{"A", "B\nsecond line"}) {
		t.Fatalf("popped = %q", got)
	}
}

func TestUnqueuePlanClearsTheBoxAndPutsTheRestBack(t *testing.T) {
	for _, c := range []struct {
		popped []string
		text   string
		clear  int
		back   []string
		ok     bool
	}{
		{[]string{"A", "B", "C"}, "B", 4, []string{"A", "C"}, true},                    // three lines in the box (+1 to be sure)
		{[]string{"A", "B\nsecond line"}, "  A ", 4, []string{"B\nsecond line"}, true}, // a message of two lines is two to clear
		{[]string{"A", "A"}, "A", 3, []string{"A"}, true},                              // the same text twice: one goes
		{[]string{"A", "C"}, "B", 3, []string{"A", "C"}, false},                        // Claude took B already: all back, nothing lost
	} {
		clear, back, ok := unqueuePlan(c.popped, c.text)
		if clear != c.clear || !reflect.DeepEqual(back, c.back) || ok != c.ok {
			t.Errorf("%q - %q: clear %d back %q ok %v; want %d %q %v", c.popped, c.text, clear, back, ok, c.clear, c.back, c.ok)
		}
	}
}

func TestClaudeBoxReadsWhatIsLeftInClaudesInputBox(t *testing.T) {
	box := func(in string) string {
		return "✻ Worked for 1s\n────────────\n" + in + "\n────────────\n  [Opus 5.5] 📁 app\n  ⏵⏵ bypass permissions on\n"
	}
	for in, want := range map[string]string{
		"❯ ":                                     "",
		"❯ Press up to edit queued messages":     "", // the hint while something is queued
		`❯ Try "refactor <filepath>"`:            "", // a suggestion in an empty box
		"❯ Also, every page says Could not load": "Also, every page says Could not load",
		"❯ first line\n  second line":            "first line\nsecond line",
		"  5. Chat about this":                   "", // not Claude's box: leave it be
	} {
		if got := claudeBox(box(in)); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestRestoredIsAMessageYouSentNotASuggestion(t *testing.T) {
	sent := []string{"Also, every page says\nCould not load", "<pasted_content id=\"x\">a long paste</pasted_content>"}
	for box, want := range map[string]bool{
		"Also, every page says Could not load":    true,  // wrapped differently in the box
		"Also, every page says":                   true,  // the box shows its start
		"a long paste":                            true,  // Claude Code's marks around a paste
		"Tell me when V680 is live in production": false, // a suggestion: never sent
		"": false,
	} {
		if got := restored(box, sent); got != want {
			t.Errorf("%q: %v", box, got)
		}
	}
}
