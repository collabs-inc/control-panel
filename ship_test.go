package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// one repo as GitHub reported it: Railway's deploys of stage and production, and stage's PRs into main
func shipFixture(t0 time.Time) string {
	at := func(m int) string { return t0.Add(time.Duration(m) * time.Minute).Format(time.RFC3339) }
	dep := func(env, sha string, m int, states ...string) string { // statuses newest first, a minute apart
		var st []string
		for i, s := range states {
			st = append(st, fmt.Sprintf(`{"state":%q,"createdAt":%q,"logUrl":"https://railway.com/%s"}`, s, at(m+len(states)-i), sha))
		}
		return fmt.Sprintf(`{"environment":%q,"commitOid":%q,"createdAt":%q,"newest":{"nodes":[%s]},"oldest":{"nodes":[%s]}}`, env, sha, at(m), strings.Join(st[:min(2, len(st))], ","), strings.Join(st, ","))
	}
	return `{"data":{"repository":{"deployments":{"nodes":[` + strings.Join([]string{
		dep("Old thing / production", "zz", -40*24*60, "INACTIVE"),       // a service nobody deploys any more
		dep("Broken / production", "zy", -3*24*60, "INACTIVE"),           // one that hasn't deployed anything since
		dep("App / stage", "f1", 1, "SUCCESS", "SUCCESS", "IN_PROGRESS"), // Railway says SUCCESS again while it runs: the first is when it went live
		dep("App / production", "r1", 31, "SUCCESS", "IN_PROGRESS"),
		dep("App / production", "r0", -59, "SUCCESS"),
		dep("Worker / production", "r1", 31, "IN_PROGRESS"),
		dep("App / stage", "g1", 41, "INACTIVE", "IN_PROGRESS"), // superseded before it went live
		dep("App / stage", "h1", 43, "INACTIVE", "SUCCESS", "IN_PROGRESS"),
		dep("App / stage", "k1", 61, "FAILURE", "IN_PROGRESS"),
		dep("App / stage", "x9", 75, "SUCCESS"), // live now: carries g, h
	}, ",") + `]},"pullRequests":{"nodes":[
	 {"number":20,"title":"Release","url":"https://github.com/o/r/pull/20","state":"MERGED","baseRefName":"main","mergedAt":"` + at(30) + `","mergeCommit":{"oid":"r1"},"author":{"login":"bo"}},
	 {"number":22,"title":"Next release","url":"https://github.com/o/r/pull/22","state":"OPEN","baseRefName":"main","mergedAt":null,"mergeCommit":null},
	 {"number":19,"title":"Older","url":"https://github.com/o/r/pull/19","state":"MERGED","baseRefName":"main","mergedAt":"` + at(-60) + `","mergeCommit":{"oid":"r0"}},
	 {"number":21,"title":"Into a feature branch","url":"https://github.com/o/r/pull/21","state":"MERGED","baseRefName":"feat/x","mergedAt":"` + at(35) + `","mergeCommit":{"oid":"q1"}}]}}}}`
}

func TestShipTracksEachPRToItsReleaseAndEnvironments(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	si, err := parseShip([]byte(shipFixture(t0)))
	if err != nil {
		t.Fatal(err)
	}
	if si.releases[1].Author != "bo" {
		t.Fatalf("a release's author: %q", si.releases[1].Author)
	}
	if got := fmt.Sprint(prNums(si.releases)); got != "[19 20 22]" {
		t.Fatalf("releases: %s, want stage's PRs into main, in the order they shipped (the open one last)", got)
	}
	pr := func(n int, base, merge string, m int, state string) *PRStatus {
		return &PRStatus{Number: n, URL: fmt.Sprintf("https://github.com/o/r/pull/%d", n), Repo: "o/r", State: state, Head: "b", Base: base,
			merge: merge, MergedAt: t0.Add(time.Duration(m) * time.Minute), Checks: []PRCheck{}}
	}
	f := pr(10, "stage", "f1", 0, "MERGED")   // released in #20
	g := pr(11, "stage", "g1", 40, "MERGED")  // after it: waits for #22
	k := pr(12, "stage", "k1", 60, "MERGED")  // its deploy failed…
	k2 := pr(13, "stage", "k2", 80, "MERGED") // …and nothing deployed after this one yet
	o := pr(14, "stage", "", 0, "OPEN")
	rel := pr(20, "main", "r1", 30, "MERGED")
	extra := shipPRs([]*PRStatus{f, g, k, k2, o, rel}, map[string]*shipInfo{"o/r": si}, t0.Add(90*time.Minute))
	show := func(p *PRStatus) string {
		var out []string
		for _, d := range p.Deploys {
			s := d.Env + ":" + d.State
			if d.State == "live" {
				s += "@" + d.At.Format("15:04")
			}
			out = append(out, s)
		}
		return fmt.Sprintf("#%d %s", p.Release, strings.Join(out, " "))
	}
	for _, c := range []struct {
		p    *PRStatus
		want string
	}{
		{f, "#20 stage:live@10:03 production:run"},  // live on stage; production waits on a second service
		{g, "#22 stage:live@10:45 production:wait"}, // its own deploy was superseded: the next one carried it
		{k, "#22 stage:live@11:16 production:wait"}, // its own deploy failed; a later one went live
		{k2, "#22 stage:wait production:wait"},
		{o, "#0 "},
		{rel, "#0 production:run"},
	} {
		if got := show(c.p); got != c.want {
			t.Errorf("#%d: %s, want %s", c.p.Number, got, c.want)
		}
	}
	if !strings.Contains(f.Deploys[1].Note, "Worker: deploying") {
		t.Errorf("with several services, the note says which is behind: %q", f.Deploys[1].Note)
	}
	if len(extra) != 1 || extra[0].Number != 22 || !extra[0].Ext || len(extra[0].Deploys) != 1 || extra[0].Deploys[0].State != "wait" {
		t.Fatalf("the releases the session didn't link come along: %+v", extra)
	}
}

func TestShipWhenADeployFailsAndNothingFollows(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	si, _ := parseShip([]byte(shipFixture(t0)))
	k := &PRStatus{Number: 12, Repo: "o/r", State: "MERGED", Base: "stage", merge: "k1", MergedAt: t0.Add(60 * time.Minute)}
	shipPRs([]*PRStatus{k}, map[string]*shipInfo{"o/r": si}, t0.Add(70*time.Minute))
	si.deploys = si.deploys[:len(si.deploys)-1] // before x9
	shipPRs([]*PRStatus{k}, map[string]*shipInfo{"o/r": si}, t0.Add(70*time.Minute))
	if d := k.Deploys[0]; d.State != "fail" || d.URL != "https://railway.com/k1" {
		t.Fatalf("stage: %+v, want the failed deploy, linked", d)
	}
	old := &PRStatus{Number: 19, Repo: "o/r", State: "MERGED", Base: "main", merge: "r0", MergedAt: t0.Add(-60 * time.Minute)}
	two := &shipInfo{} // App, live with it, and Broken, which deployed nothing since
	for _, d := range si.deploys {
		if d.sha == "r0" || d.sha == "zy" {
			two.deploys = append(two.deploys, d)
		}
	}
	shipPRs([]*PRStatus{old}, map[string]*shipInfo{"o/r": two}, t0.Add(70*time.Minute))
	if d := old.Deploys[0]; d.State != "live" || d.Note != "" {
		t.Fatalf("production: %+v, want live: a service that deployed nothing since doesn't count", d)
	}
	none := &shipInfo{}
	p := &PRStatus{Number: 3, Repo: "o/r", State: "MERGED", Base: "stage", MergedAt: t0}
	shipPRs([]*PRStatus{p}, map[string]*shipInfo{"o/r": none}, t0)
	if p.Deploys != nil || p.Release != 0 {
		t.Fatalf("a repo nothing deploys says nothing about deploys: %+v", p)
	}
}

func prNums(ps []*PRStatus) []int {
	var out []int
	for _, p := range ps {
		out = append(out, p.Number)
	}
	return out
}

func TestShipIsAskedOftenOnlyWhileSomethingShips(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	calls, body := 0, shipFixture(t0)
	c := &prCache{fetch: func(dir string, args ...string) ([]byte, error) { calls++; return []byte(body), nil }}
	quiet := strings.ReplaceAll(strings.ReplaceAll(body, `"IN_PROGRESS"`, `"SUCCESS"`), `"Worker`, `"App`)
	now := t0.Add(2 * time.Hour)
	c.ship("o/r", "stage", t0, now)
	if c.ship("o/r", "stage", t0, now.Add(40*time.Second)); calls != 2 {
		t.Fatalf("a deploy is running: asked %d times, want again after 30 s", calls)
	}
	body = quiet
	c.ship("o/r", "stage", t0, now.Add(80*time.Second))
	if c.ship("o/r", "stage", t0, now.Add(2*time.Minute)); calls != 3 {
		t.Fatalf("nothing on its way: asked %d times, want it kept 3 min", calls)
	}
	if c.ship("o/r", "stage", now.Add(90*time.Second), now.Add(2*time.Minute)); calls != 4 {
		t.Fatalf("a PR just merged: asked %d times, want it asked again", calls)
	}
}

func TestParsePRKeepsWhereAndWhenItMerged(t *testing.T) {
	p, err := parsePR([]byte(`{"number":5,"url":"https://github.com/o/r/pull/5","state":"MERGED","headRefName":"f","baseRefName":"stage",
"mergedAt":"2026-10-09T06:14:00Z","closedAt":"2026-10-09T06:14:00Z","mergeCommit":{"oid":"abc"},"author":{"login":"ana"},"statusCheckRollup":[]}`))
	if err != nil || p.Repo != "o/r" || p.merge != "abc" || p.MergedAt.IsZero() || p.ClosedAt.IsZero() || p.Author != "ana" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestLoginIsAskedOnce(t *testing.T) {
	calls, fail := 0, true
	c := &prCache{fetch: func(dir string, args ...string) ([]byte, error) {
		calls++
		if fail {
			return nil, fmt.Errorf("gh: offline")
		}
		return []byte("owner\n"), nil
	}}
	t0 := time.Unix(1000, 0)
	if c.login(t0) != "" || c.login(t0.Add(time.Minute)) != "" || calls != 1 {
		t.Fatalf("a failure is retried after 10 min, not every request: %d calls", calls)
	}
	fail = false
	if c.login(t0.Add(11*time.Minute)) != "owner" || c.login(t0.Add(12*time.Minute)) != "owner" || calls != 2 {
		t.Fatalf("then kept: %d calls", calls)
	}
}
