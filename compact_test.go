package main

import (
	"errors"
	"strconv"
	"testing"
)

// run steps a record through views, a second apart from t, doing each action as if it went through;
// it returns the actions, "" for none.
func runCompact(r *compactRec, cfg CompactCfg, t int64, vs ...compactView) (acts []string) {
	for i, v := range vs {
		now := t + int64(i)
		act := r.next(v, cfg, now)
		if act != "" {
			r.did(act, nil, now)
		}
		acts = append(acts, act)
	}
	return acts
}

var cfg60 = CompactCfg{On: true, Suggest: 40, Auto: 60}

func TestCompactorCompactsAFinishedTurnOnceItSettles(t *testing.T) {
	r := &compactRec{}
	idle := compactView{state: "waiting", pct: 63, lastAt: 90}
	acts := runCompact(r, cfg60, 100, idle, idle, idle, idle)
	if want := []string{"", "", "", "compact"}; !same(acts, want) {
		t.Fatalf("acts %q, want %q: compact once the turn has been over 3 s", acts, want)
	}
	if r.Phase != "compacting" || r.Resume {
		t.Fatalf("phase %q resume %v, want compacting without a resume", r.Phase, r.Resume)
	}
	done := compactView{state: "waiting", pct: 63, lastAt: 90, compacts: 200}
	if act := r.next(done, cfg60, 200); act != "" || r.Phase != "done" {
		t.Fatalf("after the boundary: act %q phase %q, want done", act, r.Phase)
	}
	// still over (a big CLAUDE.md): not again until it has been under the limit
	for i := range 10 {
		if act := r.next(done, cfg60, 300+int64(i)); act != "" {
			t.Fatalf("compacted again while still over the limit: %q", act)
		}
	}
	under := compactView{state: "waiting", pct: 4, lastAt: 90, compacts: 200}
	r.next(under, cfg60, 400)
	if r.Phase != "" {
		t.Fatalf("under the limit: phase %q, want re-armed", r.Phase)
	}
}

func TestCompactorPausesAWorkingTurnThenResumesIt(t *testing.T) {
	r := &compactRec{}
	work := compactView{state: "working", pct: 61, lastAt: 90}
	if act := r.next(work, cfg60, 100); act != "note" {
		t.Fatalf("working over the limit: %q, want the note", act)
	}
	r.did("note", nil, 100)
	if acts := runCompact(r, cfg60, 101, work, work, work); !same(acts, []string{"", "", ""}) {
		t.Fatalf("while it finishes its step: %q, want nothing (one note only)", acts)
	}
	stopped := compactView{state: "waiting", pct: 64, lastAt: 110}
	if acts := runCompact(r, cfg60, 111, stopped, stopped, stopped, stopped); acts[3] != "compact" {
		t.Fatalf("after it stopped: %q, want a compact", acts)
	}
	if r.Phase != "compacting" || !r.Resume {
		t.Fatalf("phase %q resume %v, want compacting, then resume", r.Phase, r.Resume)
	}
	after := compactView{state: "waiting", pct: 3, lastAt: 110, compacts: 130}
	acts := runCompact(r, cfg60, 131, after, after, after, after)
	if acts[3] != "continue" || r.Phase != "done" {
		t.Fatalf("after compacting: %q phase %q, want it told to continue", acts, r.Phase)
	}
}

func TestCompactorWaitsForYourAnswerWhenAsked(t *testing.T) {
	r := &compactRec{}
	asked := compactView{state: "waiting", pct: 70, lastAt: 90, asks: true}
	if acts := runCompact(r, cfg60, 100, asked, asked, asked, asked, asked); !same(acts, []string{"", "", "", "", ""}) || r.Phase != "hold" {
		t.Fatalf("acts %q phase %q: a question waits for your answer", acts, r.Phase)
	}
	work := compactView{state: "working", pct: 71, lastAt: 90}
	if acts := runCompact(r, cfg60, 200, work, work); !same(acts, []string{"", ""}) {
		t.Fatalf("the turn after your answer: %q, want it left alone (no note)", acts)
	}
	over := compactView{state: "waiting", pct: 74, lastAt: 230, asks: true}
	if acts := runCompact(r, cfg60, 231, over, over, over, over); acts[3] != "compact" || r.Resume {
		t.Fatalf("that turn over: %q resume %v, want compacted when it ends, whatever it asks", acts, r.Resume)
	}
}

func TestCompactorLeavesAgentsAlone(t *testing.T) {
	idle := compactView{state: "waiting", pct: 80, lastAt: 90}
	for name, c := range map[string]struct {
		r   compactRec
		cfg CompactCfg
		v   compactView
	}{
		"under the limit": {cfg: cfg60, v: compactView{state: "waiting", pct: 59}},
		"switched off":    {r: compactRec{Off: true}, cfg: cfg60, v: idle},
		"auto off":        {cfg: CompactCfg{Suggest: 40, Auto: 60}, v: idle},
		"a dialog open":   {cfg: cfg60, v: compactView{state: "asking", pct: 80}},
		"a subagent runs": {cfg: cfg60, v: compactView{state: "waiting", pct: 80, kids: true}},
		"just failed":     {r: compactRec{Phase: "failed", At: 95}, cfg: cfg60, v: idle},
		"just compacted":  {cfg: cfg60, v: compactView{state: "waiting", pct: 80, compacts: 50}},
	} {
		r := c.r
		if acts := runCompact(&r, c.cfg, 100, c.v, c.v, c.v, c.v, c.v); !same(acts, []string{"", "", "", "", ""}) {
			t.Errorf("%s: %q, want nothing", name, acts)
		}
	}
}

func TestCompactorDoesWhatYouAsk(t *testing.T) {
	r := &compactRec{Off: true, Manual: true}
	under := compactView{state: "waiting", pct: 12, lastAt: 90, asks: true}
	if acts := runCompact(r, cfg60, 100, under, under, under, under); acts[3] != "compact" {
		t.Fatalf("you pressed Compact: %q, want it compacted (off, under the limit and asking notwithstanding)", acts)
	}
	r.next(compactView{state: "waiting", pct: 2, compacts: 110}, cfg60, 110)
	if r.Phase != "done" || r.Manual {
		t.Fatalf("phase %q manual %v: your press is used up once done", r.Phase, r.Manual)
	}
}

func TestCompactorGivesUpOnACompactThatNeverFinishes(t *testing.T) {
	r := &compactRec{}
	r.did("compact", nil, 100)
	v := compactView{state: "waiting", pct: 80}
	r.next(v, cfg60, 100+compactTTL+1)
	if r.Phase != "failed" {
		t.Fatalf("phase %q, want failed", r.Phase)
	}
	r2 := &compactRec{}
	r2.did("note", errors.New("not taken"), 100)
	if r2.Phase != "failed" {
		t.Fatalf("a note that didn't go: phase %q, want failed", r2.Phase)
	}
	if act := r.next(v, cfg60, 100+2*compactTTL+5); act != "compact" {
		t.Fatalf("after the wait: act %q phase %q, want it tried again", act, r.Phase)
	}
}

func TestCompactorStandsDownWhenClaudeCodeCompactsByItself(t *testing.T) {
	r := &compactRec{}
	r.did("note", nil, 100)
	r.next(compactView{state: "working", pct: 5, compacts: 150}, cfg60, 151)
	if r.Phase != "done" {
		t.Fatalf("phase %q, want done: it compacted mid-turn and carries on", r.Phase)
	}
}

func TestAsksYou(t *testing.T) {
	for text, want := range map[string]bool{
		"Done: the tests pass and it's deployed.":                            false,
		"Done.\n\nWant me to push it?":                                       true,
		"Pushed.\n\nShould I also update the docs":                           true,
		"I can't deploy until you add the key.":                              true,
		"Is it fast? Yes, 3x.\n\n" + string(make([]byte, 500)) + "All done.": false,
	} {
		if got := asksYou(text); got != want {
			t.Errorf("asksYou(%.40q) = %v, want %v", text, got, want)
		}
	}
}

func same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTypedBoxSkipsWhatClaudeCodeDrawsDim(t *testing.T) {
	rule := "\x1b[38;5;244m────────────────────────────────────────────────\x1b[39m" // drawn in colour, as on a real screen
	screen := func(box string) string {
		return "  hello\n" + rule + "\n" + box + "\n" + rule + "\n  [Opus 5.5] 📁 mvtest\n"
	}
	const hint = "\x1b[m❯ \x1b[39m\x1b[49m\x1b[29m\x1b[28m\x1b[27m\x1b[25m\x1b[25m\x1b[22m\x1b[24m\x1b[2m\x1b[23mTry \"fix lint errors\""
	const suggestion = "\x1b[m❯ \x1b[38;2;120;120;120m\x1b[2mrun the tests again\x1b[22m"
	const typed = "\x1b[m❯ typed by me"
	const typedTrue = "\x1b[m❯ \x1b[38;2;250;250;250mtyped by me" // a truecolor's 2 isn't dim
	for name, c := range map[string]struct{ box, want string }{
		"empty":           {"\x1b[m❯ ", ""},
		"the hint":        {hint, ""},
		"a suggestion":    {suggestion, ""},
		"typed":           {typed, "typed by me"},
		"typed in colour": {typedTrue, "typed by me"},
	} {
		if got := typedBox(screen(c.box)); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

func TestParseChatFoldsACompactionIntoOneLine(t *testing.T) {
	ts := `"timestamp":"2026-10-14T00:40:00Z"`
	lines := [][]byte{
		[]byte(`{"type":"user",` + ts + `,"message":{"role":"user","content":` + strconv.Quote(compactCmd) + `}}`),
		[]byte(`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted",` + ts + `,"compactMetadata":{"trigger":"manual","preTokens":36120,"postTokens":5657}}`),
		[]byte(`{"type":"user",` + ts + `,"isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued from a previous conversation. Summary: runs 1-2 of 8 done."}}`),
		[]byte(`{"type":"user",` + ts + `,"isMeta":true,"message":{"role":"user","content":"<local-command-caveat>Caveat: the command below</local-command-caveat>"}}`),
		[]byte(`{"type":"user",` + ts + `,"message":{"role":"user","content":"<command-name>/compact</command-name>\n<command-message>compact</command-message>\n<command-args></command-args>"}}`),
		[]byte(`{"type":"user",` + ts + `,"message":{"role":"user","content":"<local-command-stdout>\u001b[2mCompacted (ctrl+o to see full summary)\u001b[22m</local-command-stdout>"}}`),
		[]byte(`{"type":"user",` + ts + `,"message":{"role":"user","content":` + strconv.Quote(resumeMsg) + `}}`),
	}
	items, _ := parseChat(lines)
	var got []string
	for _, it := range items {
		got = append(got, it.K+"|"+it.Via+"|"+trunc(it.Text, 40))
	}
	want := []string{
		"note||Compacted 36k → 6k",
		"user|summary|This session is being continued from …",
		"user|panel|Compacted. Continue where you left off.",
	}
	if !same(got, want) {
		t.Fatalf("items:\n%q\nwant:\n%q", got, want)
	}
}
