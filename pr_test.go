package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionPRsAreEveryLinkInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(path, []byte(strings.Join([]string{
		`{"type":"pr-link","prNumber":10860,"prRepository":"o/r"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"a tool result mentioning \"pr-link\""}]}}`,
		`{"type":"pr-link","prNumber":10860,"prRepository":"o/r"}`,
		`{"type":"pr-link","prNumber":13,"prRepository":"not a repo"}`,
	}, "\n")+"\n"), 0o600)
	var sc prScans
	if got := fmt.Sprint(sc.refs(path)); got != "[{o/r 10860}]" {
		t.Fatalf("first scan: %s", got)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"pr-link","prNumber":10871,"prRepository":"o/r"}` + "\n" + `{"type":"pr-link","prNumber":10860,"prRepository":"o/r"}` + "\n")
	f.Close()
	if got := fmt.Sprint(sc.refs(path)); got != "[{o/r 10860} {o/r 10871}]" {
		t.Fatalf("after the stage -> main PR: %s (in the order they were opened, once each)", got)
	}
	if sc.m[path].off != fileSize(path) {
		t.Fatal("the next look must read only what's new")
	}
	// a PR it didn't open but merged with gh, by number (in its own repo); a look at another doesn't count
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"gh pr view 5 && gh pr merge 10880 --squash"}}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"gh pr comment 10871 -b ok"}}]}}` + "\n")
	f.Close()
	if got := fmt.Sprint(sc.refs(path)); got != "[{o/r 10860} {o/r 10871} { 10880}]" {
		t.Fatalf("with gh: %s", got)
	}
}

const ghPRView = `{"number":7,"title":"Speed up the totals","url":"https://github.com/o/r/pull/7","state":"OPEN","isDraft":true,
"headRefName":"fix/totals","baseRefName":"main","reviewDecision":"CHANGES_REQUESTED","mergeable":"CONFLICTING",
"latestReviews":[{"author":{"login":"ana"},"state":"APPROVED"},{"author":{"login":"bo"},"state":"CHANGES_REQUESTED"},{"author":{"login":"cy"},"state":"COMMENTED"}],
"statusCheckRollup":[
 {"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/o/r/actions/runs/1"},
 {"__typename":"CheckRun","name":"unit","status":"IN_PROGRESS","conclusion":"","detailsUrl":"https://github.com/o/r/actions/runs/2"},
 {"__typename":"CheckRun","name":"e2e","status":"COMPLETED","conclusion":"TIMED_OUT","detailsUrl":"javascript:alert(1)"},
 {"__typename":"CheckRun","name":"docs","status":"COMPLETED","conclusion":"SKIPPED","detailsUrl":""},
 {"__typename":"StatusContext","context":"deploy - api","state":"PENDING","targetUrl":"https://railway.com/x"},
 {"__typename":"StatusContext","context":"deploy - web","state":"ERROR","targetUrl":"https://railway.com/y"}]}`

func TestParsePRSortsChecksAndNamesReviewers(t *testing.T) {
	p, err := parsePR([]byte(ghPRView))
	if err != nil {
		t.Fatal(err)
	}
	if p.Number != 7 || p.State != "OPEN" || !p.Draft || p.Head != "fix/totals" || p.Base != "main" || !p.Conflict {
		t.Fatalf("pr = %+v", p)
	}
	if p.Review != "CHANGES_REQUESTED" || strings.Join(p.Approved, ",") != "ana" || strings.Join(p.Changes, ",") != "bo" {
		t.Fatalf("reviews: %q approved %v changes %v", p.Review, p.Approved, p.Changes)
	}
	var got []string
	for _, c := range p.Checks {
		got = append(got, c.State+":"+c.Name)
	}
	if want := "fail:deploy - web,fail:e2e,run:deploy - api,run:unit,pass:lint,skip:docs"; strings.Join(got, ",") != want {
		t.Fatalf("checks = %v, want %s (failures first)", got, want)
	}
	for _, c := range p.Checks {
		if c.Name == "e2e" && c.URL != "" {
			t.Fatalf("a link that isn't https must be dropped: %q", c.URL)
		}
	}
}

func TestAgentPRsAreTheSessionsAndTheBranchs(t *testing.T) {
	pr := func(n, state string) string {
		return `{"number":` + n + `,"state":"` + state + `","headRefName":"b","baseRefName":"main","statusCheckRollup":[]}`
	}
	answer := map[string]string{}
	var calls []string
	var mu sync.Mutex // gh runs for several PRs at once
	c := &prCache{fetch: func(dir string, args ...string) ([]byte, error) {
		k := dir + " " + strings.Join(args[:min(len(args), 5)], " ")
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, k)
		for prefix, a := range answer {
			if strings.HasPrefix(k, prefix) {
				return []byte(a), nil
			}
		}
		return nil, errors.New("no pull requests found")
	}}
	now := time.Unix(1000, 0)
	nums := func(ps []*PRStatus) string {
		var out []string
		for _, p := range ps {
			out = append(out, strconv.Itoa(p.Number)+":"+p.State)
		}
		return strings.Join(out, ",")
	}
	refs := []prRef{{"o/r", 5}, {"o/r", 7}}
	answer = map[string]string{" pr view 5 --repo o/r": pr("5", "MERGED"), " pr view 7 --repo o/r": pr("7", "OPEN"), "/w pr view feat": pr("9", "OPEN")}
	if got := nums(c.agentPRs(refs, "/w", "feat", now)); got != "5:MERGED,7:OPEN,9:OPEN" {
		t.Fatalf("the session's PRs in order, then its branch's open one: %s", got)
	}
	calls = nil
	c.agentPRs(refs, "/w", "feat", now.Add(5*time.Minute))
	if strings.Contains(strings.Join(calls, "|"), "view 5") || !strings.Contains(strings.Join(calls, "|"), "view 7") {
		t.Fatalf("a merged PR is kept 10 min, an open one 30 s: %v", calls)
	}
	c.m = nil
	answer[" pr view 7 --repo o/r"] = pr("7", "OPEN")
	answer["/w pr view feat"] = pr("7", "OPEN")
	if got := nums(c.agentPRs(refs, "/w", "feat", now)); got != "5:MERGED,7:OPEN" {
		t.Fatalf("the branch's PR, already linked, isn't listed twice: %s", got)
	}
	calls = nil
	if c.agentPRs(nil, "/w", "-x", now); strings.Contains(strings.Join(calls, "|"), "-x") {
		t.Fatalf("a branch that looks like a flag is never passed to gh: %v", calls)
	}
}

func fileSize(path string) int64 { fi, _ := os.Stat(path); return fi.Size() }

func TestAFailedLookupKeepsTheLastAnswer(t *testing.T) {
	fail := false
	c := &prCache{fetch: func(dir string, args ...string) ([]byte, error) {
		if fail {
			return nil, errors.New("gh: timeout") // an overloaded machine, a network blip
		}
		return []byte(`{"number":7,"state":"OPEN","headRefName":"stage","baseRefName":"main","statusCheckRollup":[]}`), nil
	}}
	now := time.Unix(1000, 0)
	if p := c.view("", "7", "o/r", now); p == nil || p.State != "OPEN" {
		t.Fatalf("first look: %+v", p)
	}
	fail = true
	if p := c.view("", "7", "o/r", now.Add(40*time.Second)); p == nil || p.Number != 7 {
		t.Fatal("a lookup that failed must keep showing what was known, not make the PR vanish")
	}
	if p := c.view("", "8", "o/r", now); p != nil {
		t.Fatal("nothing known and the lookup failed: nothing")
	}
}

func TestPRsTheSessionWorkedOnWithGh(t *testing.T) {
	for cmd, want := range map[string][]prRef{
		"gh pr merge 10925 --squash --delete-branch":               {{"", 10925}},
		"cd ~/repos/app && gh pr merge --squash 10926":             {{"", 10926}},
		`gh pr review 77 --approve -b "ship 12"`:                   {{"", 77}},
		"gh pr comment https://github.com/alven/app/pull/12 -b hi": {{"alven/app", 12}},
		"gh pr edit 5 --repo alven/app --add-label 9":              {{"alven/app", 5}},
		"gh pr close --comment 3 4":                                {{"", 4}}, // a flag's value isn't the PR
		"gh pr ready 8; gh pr merge 9 && echo done | tee log":      {{"", 8}, {"", 9}},
		"gh pr view 99 --json state":                               nil, // looking isn't working on it
		"gh pr checks 99 && gh pr diff 99":                         nil,
		"gh pr list --state open":                                  nil,
		"gh pr merge --auto --squash":                              nil, // the branch's own PR: no number to go by
		"gh pr edit 5 --repo 'not a repo; rm -rf /' --title x":     nil,
	} {
		if got := ghPRRefs(cmd); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v, want %v", cmd, got, want)
		}
	}
}

func TestPRCardsAnswerAtOnceAndRefreshBehind(t *testing.T) {
	var c prCards
	builds := make(chan int, 4)
	n := 0
	build := func(p TPane) map[string]any { n++; builds <- n; return map[string]any{"n": n} }
	p := TPane{Agent: &TAgent{sid: "s1", PRs: []int{7}}}
	if b := c.get(p, build); b["n"] != 1 || b["stale"] != nil {
		t.Fatalf("first: %v, want made now", b)
	}
	<-builds
	if b := c.get(p, build); b["n"] != 1 || len(builds) != 0 {
		t.Fatalf("again: %v, want the same card, not made again", b)
	}
	p.Agent.PRs = []int{7, 9} // the session linked another PR
	if b := c.get(p, build); b["n"] != 1 || b["stale"] != true {
		t.Fatalf("after a new PR: %v, want the last card at once, marked stale", b)
	}
	<-builds // made in the background
	for i := 0; i < 100; i++ {
		if b := c.get(p, build); b["n"] == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the fresh card never replaced the stale one")
}
