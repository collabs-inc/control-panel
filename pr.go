package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// An agent's pull requests, as GitHub sees them, for the card over its chat: every PR Claude Code linked to
// the session (its pr-link entries: feature -> stage, then stage -> main), and the open PR of the branch the
// agent works on (a PR a subagent or you opened). Looked up with gh only while a chat shows them; cached for
// 30 s while open, 10 min once merged or closed.

type PRCheck struct {
	Name  string `json:"name"`
	State string `json:"state"` // fail, run, pass, skip
	URL   string `json:"url,omitempty"`
}

type PRStatus struct {
	Number   int        `json:"number"`
	Title    string     `json:"title"`
	URL      string     `json:"url"`
	State    string     `json:"state"` // OPEN, MERGED, CLOSED
	Draft    bool       `json:"draft,omitempty"`
	Head     string     `json:"head"`
	Base     string     `json:"base"`
	Review   string     `json:"review,omitempty"` // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED
	Approved []string   `json:"approved,omitempty"`
	Changes  []string   `json:"changes,omitempty"`
	Conflict bool       `json:"conflict,omitempty"`
	Checks   []PRCheck  `json:"checks"`
	Repo     string     `json:"repo,omitempty"`
	Author   string     `json:"author,omitempty"` // a GitHub login
	MergedAt time.Time  `json:"mergedAt,omitzero"`
	ClosedAt time.Time  `json:"closedAt,omitzero"` // where a closed one sits among the rest
	merge    string     // its merge commit
	Release  int        `json:"release,omitempty"` // the stage → production PR that took (or will take) it there
	Deploys  []PRDeploy `json:"deploys,omitempty"` // each environment its change goes to, in order
	Ext      bool       `json:"ext,omitempty"`     // a release the session didn't link, carrying ones it did
}

// prScans: every PR Claude Code linked to a session (its pr-link entries), in the order they first appear:
// a feature branch's PR into stage, then stage's into main. The whole transcript is read once, then only
// what was appended since (the link is re-written after every turn, so a tail would only see the last).
type prScans struct {
	mu sync.Mutex
	m  map[string]*prScan
}

type prScan struct {
	off  int64
	refs []prRef
}

func (s *prScans) refs(path string) []prRef {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*prScan{}
	}
	sc := s.m[path]
	if sc == nil || fi.Size() < sc.off { // new, or rewritten
		sc = &prScan{}
		s.m[path] = sc
	}
	if fi.Size() > sc.off {
		if f, err := os.Open(path); err == nil {
			f.Seek(sc.off, io.SeekStart)
			rd := bufio.NewReaderSize(f, 1<<16)
			for {
				l, err := rd.ReadBytes('\n')
				if err != nil { // a line still being written: read it next time
					break
				}
				sc.off += int64(len(l))
				if bytes.Contains(l, []byte("gh pr ")) && bytes.Contains(l, []byte(`"tool_use"`)) {
					var e tEntry
					if json.Unmarshal(l, &e) == nil && e.Type == "assistant" {
						_, bs := blocks(e.Message.Content)
						for _, b := range bs {
							cmd, _ := b.Input["command"].(string)
							if b.Type != "tool_use" || b.Name != "Bash" || cmd == "" {
								continue
							}
							for _, r := range ghPRRefs(cmd) {
								if !slices.ContainsFunc(sc.refs, func(x prRef) bool { return x.pr == r.pr && (r.repo == "" || x.repo == r.repo) }) {
									sc.refs = append(sc.refs, r)
								}
							}
						}
					}
					continue
				}
				if !bytes.Contains(l, []byte(`"pr-link"`)) {
					continue
				}
				var e struct {
					Type string `json:"type"`
					N    int    `json:"prNumber"`
					Repo string `json:"prRepository"`
				}
				if json.Unmarshal(l, &e) != nil || e.Type != "pr-link" || e.N <= 0 || !repoRe.MatchString(e.Repo) || slices.Contains(sc.refs, prRef{e.Repo, e.N}) {
					continue
				}
				sc.refs = append(sc.refs, prRef{e.Repo, e.N})
			}
			f.Close()
		}
	}
	return slices.Clone(sc.refs)
}

// The PRs a session worked on with gh besides the ones it opened: merged, reviewed, commented on, edited,
// readied, closed or reopened (looking at one, view, checks or diff, isn't working on it). A number is in the
// repo the command runs in; a URL or --repo names its own.
var (
	ghPRVerbs    = map[string]bool{"merge": true, "review": true, "comment": true, "edit": true, "ready": true, "close": true, "reopen": true}
	ghValueFlags = map[string]bool{"-b": true, "--body": true, "-F": true, "--body-file": true, "-t": true, "--title": true, "--subject": true, "-c": true, "--comment": true, "-B": true, "--base": true, "--match-head-commit": true, "-A": true, "--author-email": true, "--add-label": true, "--remove-label": true, "--add-reviewer": true, "--remove-reviewer": true, "--add-assignee": true, "--remove-assignee": true, "--milestone": true}
	shellSplitRe = regexp.MustCompile(`&&|\|\||[;|\n]`)
	pullURLRe    = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/pull/(\d+)`)
	digitsRe     = regexp.MustCompile(`^\d{1,7}$`)
)

func ghPRRefs(cmd string) []prRef {
	var out []prRef
	for _, part := range shellSplitRe.Split(cmd, -1) {
		f := strings.Fields(part)
		for i := 0; i+2 < len(f); i++ {
			if f[i] != "gh" || f[i+1] != "pr" || !ghPRVerbs[f[i+2]] {
				continue
			}
			ref := prRef{}
			for j := i + 3; j < len(f); j++ {
				t := strings.Trim(f[j], `"'`)
				switch {
				case (t == "--repo" || t == "-R") && j+1 < len(f):
					ref.repo = strings.Trim(f[j+1], `"'`)
					j++
				case strings.HasPrefix(t, "--repo="):
					ref.repo = t[len("--repo="):]
				case ghValueFlags[t]:
					j++ // its value, not the PR
				case ref.pr == 0 && digitsRe.MatchString(t):
					ref.pr, _ = strconv.Atoi(t)
				case ref.pr == 0 && pullURLRe.MatchString(t):
					m := pullURLRe.FindStringSubmatch(t)
					ref.repo = m[1]
					ref.pr, _ = strconv.Atoi(m[2])
				}
			}
			if ref.pr > 0 && (ref.repo == "" || repoRe.MatchString(ref.repo)) {
				out = append(out, ref)
			}
			break
		}
	}
	return out
}

const prFields = "number,title,url,state,isDraft,headRefName,baseRefName,reviewDecision,mergeable,statusCheckRollup,latestReviews,mergedAt,mergeCommit,closedAt,author"

var checkRank = map[string]int{"fail": 0, "run": 1, "pass": 2, "skip": 3}

func parsePR(b []byte) (*PRStatus, error) {
	var v struct {
		Number         int
		Title, URL     string
		State          string
		IsDraft        bool
		HeadRefName    string
		BaseRefName    string
		ReviewDecision string
		Mergeable      string
		MergedAt       time.Time
		ClosedAt       time.Time
		Author         struct{ Login string }
		MergeCommit    *struct{ Oid string }
		LatestReviews  []struct {
			Author struct{ Login string }
			State  string
		}
		StatusCheckRollup []struct {
			Typename   string `json:"__typename"`
			Name       string // a check run…
			Status     string
			Conclusion string
			DetailsURL string `json:"detailsUrl"`
			Context    string // …or a commit status
			State      string
			TargetURL  string `json:"targetUrl"`
		}
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	p := &PRStatus{Number: v.Number, Title: trunc(v.Title, 300), URL: link(v.URL), State: v.State, Draft: v.IsDraft,
		Head: trunc(v.HeadRefName, 200), Base: trunc(v.BaseRefName, 200), Review: v.ReviewDecision, Conflict: v.Mergeable == "CONFLICTING", Checks: []PRCheck{}, MergedAt: v.MergedAt, ClosedAt: v.ClosedAt, Author: trunc(v.Author.Login, 100)}
	if m := pullURLRe.FindStringSubmatch(p.URL); m != nil {
		p.Repo = m[1]
	}
	if v.MergeCommit != nil {
		p.merge = v.MergeCommit.Oid
	}
	for _, r := range v.LatestReviews {
		switch r.State {
		case "APPROVED":
			p.Approved = append(p.Approved, r.Author.Login)
		case "CHANGES_REQUESTED":
			p.Changes = append(p.Changes, r.Author.Login)
		}
	}
	for _, c := range v.StatusCheckRollup {
		k := PRCheck{Name: trunc(c.Name, 120), URL: link(c.DetailsURL)}
		if c.Typename == "StatusContext" {
			k.Name, k.URL = trunc(c.Context, 120), link(c.TargetURL)
			switch c.State {
			case "SUCCESS":
				k.State = "pass"
			case "PENDING", "EXPECTED":
				k.State = "run"
			default:
				k.State = "fail"
			}
		} else {
			switch {
			case c.Status != "COMPLETED":
				k.State = "run"
			case c.Conclusion == "SUCCESS":
				k.State = "pass"
			case c.Conclusion == "SKIPPED" || c.Conclusion == "NEUTRAL":
				k.State = "skip"
			default:
				k.State = "fail"
			}
		}
		p.Checks = append(p.Checks, k)
	}
	sort.SliceStable(p.Checks, func(i, j int) bool {
		a, b := p.Checks[i], p.Checks[j]
		if checkRank[a.State] != checkRank[b.State] {
			return checkRank[a.State] < checkRank[b.State]
		}
		return a.Name < b.Name
	})
	return p, nil
}

// link keeps only https links: the page puts them in hrefs.
func link(u string) string {
	if strings.HasPrefix(u, "https://") && len(u) < 2000 {
		return u
	}
	return ""
}

var branchRe = regexp.MustCompile(`^[A-Za-z0-9._/][A-Za-z0-9._/-]{0,199}$`) // never a flag

// ponytail: names, not the repo's real default branch (another gh call); an agent on one of these is
// rarely the author of its open PR (a stage → main promotion, say).
var longLived = map[string]bool{"main": true, "master": true, "stage": true, "staging": true, "develop": true, "dev": true}

type prCache struct {
	mu    sync.Mutex
	m     map[string]prEntry
	ships map[string]shipEntry // by repo@stage branch
	me    string               // gh's login: the page names the author of a PR that isn't yours
	meAt  time.Time
	fetch func(dir string, args ...string) ([]byte, error) // gh, in dir
}

type prEntry struct {
	at time.Time
	p  *PRStatus // nil: none (kept 30 s); when gh fails the last answer stays, retried after the ttl
}

// view asks gh for one PR (by number in repo, or by branch in the checkout at dir), cached for 30 s.
func (c *prCache) view(dir, sel, repo string, now time.Time) *PRStatus {
	k := repo + "#" + sel + "@" + dir
	c.mu.Lock()
	e, ok := c.m[k]
	c.mu.Unlock()
	ttl := 30 * time.Second
	if e.p != nil && e.p.State != "OPEN" { // merged or closed: it won't change
		ttl = 10 * time.Minute
	}
	if ok && now.Sub(e.at) < ttl {
		return e.p
	}
	args := []string{"pr", "view", sel}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	var p *PRStatus
	if b, err := c.fetch(dir, append(args, "--json", prFields)...); err == nil {
		p, _ = parsePR(b)
	} else {
		p = e.p // gh failed (an overloaded machine, a blip): keep what was known, so an open PR doesn't flicker away; retried after the ttl
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]prEntry{}
	}
	for x, old := range c.m { // ponytail: a sweep per miss; a few panes' worth of entries
		if now.Sub(old.at) > 10*time.Minute {
			delete(c.m, x)
		}
	}
	c.m[k] = prEntry{now, p}
	c.mu.Unlock()
	return p
}

// agentPRs: every PR the session linked or worked on, in order, whatever their state (one named by number
// alone is looked up in the agent's checkout), then the open PR of the agent's branch when the session didn't
// name it (one a subagent or you opened). The page shows them all, merged and closed ones too.
func (c *prCache) agentPRs(refs []prRef, dir, branch string, now time.Time) []*PRStatus {
	type job struct{ in, sel, repo string }
	var jobs []job
	for _, r := range refs {
		in := ""
		if r.repo == "" {
			if in = dir; in == "" {
				continue // no checkout to say which repo
			}
		}
		if j := (job{in, strconv.Itoa(r.pr), r.repo}); !slices.Contains(jobs, j) {
			jobs = append(jobs, j)
		}
	}
	if dir != "" && branchRe.MatchString(branch) && !longLived[branch] {
		jobs = append(jobs, job{dir, branch, ""})
	}
	got := make([]*PRStatus, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6) // gh at once: a session can name dozens
	for i, j := range jobs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			got[i] = c.view(j.in, j.sel, j.repo, now)
		})
	}
	wg.Wait()
	out := []*PRStatus{}
	seen := map[string]bool{}
	for i, p := range got {
		if p == nil || seen[p.Repo+"#"+strconv.Itoa(p.Number)] || (jobs[i].sel == branch && p.State != "OPEN") {
			continue
		}
		k := p.Repo + "#" + strconv.Itoa(p.Number)
		seen[k] = true
		out = append(out, p)
	}
	return out
}

// login: who gh is logged in as (the owner's account, which the agents use too), asked once (again after
// 10 min if gh failed).
func (c *prCache) login(now time.Time) string {
	c.mu.Lock()
	me, at := c.me, c.meAt
	c.mu.Unlock()
	if me != "" || now.Sub(at) < 10*time.Minute && !at.IsZero() {
		return me
	}
	if b, err := c.fetch("", "api", "user", "--jq", ".login"); err == nil {
		me = trunc(strings.TrimSpace(string(b)), 100)
	}
	c.mu.Lock()
	c.me, c.meAt = me, now
	c.mu.Unlock()
	return me
}

func ghIn(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	cmd.Env = cleanEnv(os.Environ())
	return cmd.Output()
}

func (s *server) panePR(w http.ResponseWriter, r *http.Request) {
	session, pane, err := paneParams(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	p, ok := s.term.pane(session, pane)
	if !ok || p.Agent == nil {
		writeJSON(w, 200, map[string]any{"prs": []*PRStatus{}})
		return
	}
	writeJSON(w, 200, s.cards.get(p, s.prCard))
}

// prCards: each agent's card as last made, so a chat shows its PRs the moment it opens: gh takes seconds, so a
// card older than 30 s (or one made before the session linked another PR) is served as it is, marked stale,
// while a fresh one is made in the background; the page asks again shortly. Made ahead for every agent with PRs.
type prCards struct {
	mu   sync.Mutex
	m    map[string]prCardEntry // by Claude session id
	busy map[string]bool
	sem  chan struct{} // two cards made at once: gh is the owner's, the agents use it too
}

type prCardEntry struct {
	at       time.Time
	sig, prs string
	body     map[string]any
}

func cardSig(p TPane) string {
	sig := fmt.Sprint(p.Agent.PRs)
	if p.Git != nil && !p.Git.Detached {
		sig += " " + p.Git.Root + " " + p.Git.Branch
	}
	return sig
}

// get answers from the last card (made now if there's none), refreshing it in the background when it's stale.
func (c *prCards) get(p TPane, build func(TPane) map[string]any) map[string]any {
	c.mu.Lock()
	e, ok := c.m[p.Agent.sid]
	c.mu.Unlock()
	if !ok {
		return c.put(p, build(p))
	}
	if time.Since(e.at) < 30*time.Second && e.sig == cardSig(p) {
		return e.body
	}
	c.warm(p, build)
	return mapWith(e.body, "stale", true)
}

// warm makes an agent's card in the background, one agent at a time per agent.
func (c *prCards) warm(p TPane, build func(TPane) map[string]any) {
	sid := p.Agent.sid
	c.mu.Lock()
	if c.busy == nil {
		c.busy, c.sem = map[string]bool{}, make(chan struct{}, 2)
	}
	if c.busy[sid] {
		c.mu.Unlock()
		return
	}
	c.busy[sid] = true
	c.mu.Unlock()
	go func() {
		c.sem <- struct{}{}
		c.put(p, build(p))
		<-c.sem
		c.mu.Lock()
		delete(c.busy, sid)
		c.mu.Unlock()
	}()
}

func (c *prCards) put(p TPane, body map[string]any) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]prCardEntry{}
	}
	c.m[p.Agent.sid] = prCardEntry{time.Now(), cardSig(p), fmt.Sprint(p.Agent.PRs), body}
	return body
}

// warmAll makes ahead the card of every agent that linked a PR and has none yet (or linked another since).
func (c *prCards) warmAll(tm Terminals, build func(TPane) map[string]any) {
	for _, ss := range tm.Sessions {
		for _, tb := range ss.Tabs {
			for _, p := range tb.Panes {
				if p.Agent == nil || p.Agent.sid == "" || len(p.Agent.PRs) == 0 {
					continue
				}
				c.mu.Lock()
				e, ok := c.m[p.Agent.sid]
				c.mu.Unlock()
				if !ok || e.prs != fmt.Sprint(p.Agent.PRs) {
					c.warm(p, build)
				}
			}
		}
	}
}

func mapWith(m map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for x, y := range m {
		out[x] = y
	}
	out[k] = v
	return out
}

// prCard: the agent's PRs as GitHub has them now, and where what they merged has got to.
func (s *server) prCard(p TPane) map[string]any {
	dir, branch := "", ""
	if p.Git != nil && !p.Git.Detached {
		dir, branch = p.Git.Root, p.Git.Branch
	}
	refs := s.term.tr.prs.refs(cmp.Or(p.Agent.path, s.term.tr.path(p.Agent.sid)))
	now := time.Now()
	prs := s.prs.agentPRs(refs, dir, branch, now)
	stages := map[string]string{}    // the repos with a merged PR -> their stage branch
	merged := map[string]time.Time{} // -> when the latest of them merged
	for i, p := range prs {
		q := *p // the cache's copy stays as gh said
		prs[i] = &q
		if q.State == "MERGED" && q.Repo != "" {
			stages[q.Repo] = "stage"
			if q.MergedAt.After(merged[q.Repo]) {
				merged[q.Repo] = q.MergedAt
			}
		}
	}
	for _, p := range prs {
		for _, b := range []string{p.Base, p.Head} {
			if _, ok := stages[p.Repo]; ok && branchTier(b) == "stage" {
				stages[p.Repo] = b
			}
		}
	}
	info := map[string]*shipInfo{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for repo, stage := range stages {
		wg.Go(func() {
			si := s.prs.ship(repo, stage, merged[repo], now)
			mu.Lock()
			info[repo] = si
			mu.Unlock()
		})
	}
	wg.Wait()
	releases := shipPRs(prs, info, now)
	return map[string]any{"prs": prs, "releases": releases, "me": s.prs.login(now)}
}
