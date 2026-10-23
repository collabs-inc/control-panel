package main

import (
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Where a merged PR's change went: the stage → main PR that took (or will take) it to production, and, in
// each environment the repo deploys to, whether it's live there. Both come from one GitHub query per repo:
// its deployments (Railway reports each one with its statuses) and stage's PRs into main. A deploy carries a
// PR when it's of the PR's merge commit, or the first one after it: a branch only moves forward.
// ponytail: a rollback to an older commit would still read as carrying it; GitHub can't tell us otherwise
// without a compare per pair.

type PRDeploy struct {
	Env   string    `json:"env"`            // stage, production
	State string    `json:"state"`          // live, run, fail, wait
	At    time.Time `json:"at,omitzero"`    // when it went live, or failed
	URL   string    `json:"url,omitempty"`  // the deploy's log
	Note  string    `json:"note,omitempty"` // each service's state, when the environment has several
}

type ghDeploy struct {
	env, sha string
	at       time.Time // created
	state    string    // ok, run, fail, skip (superseded before it went live)
	done     time.Time
	url      string
}

type shipInfo struct {
	deploys  []ghDeploy  // oldest first
	releases []*PRStatus // stage's PRs into production, in the order they merged (an open one last)
}

const shipQuery = `query($o:String!,$n:String!,$h:String!){repository(owner:$o,name:$n){
deployments(last:100){nodes{environment commitOid createdAt newest:statuses(first:2){nodes{state createdAt logUrl}} oldest:statuses(last:20){nodes{state createdAt}}}}
pullRequests(headRefName:$h,states:[OPEN,MERGED],last:30){nodes{number title url state baseRefName mergedAt mergeCommit{oid} author{login}}}}}`

func parseShip(b []byte) (*shipInfo, error) {
	var v struct {
		Data struct {
			Repository struct {
				Deployments struct {
					Nodes []struct {
						Environment, CommitOid string
						CreatedAt              time.Time
						Newest, Oldest         struct { // statuses, newest first
							Nodes []struct {
								State     string
								CreatedAt time.Time
								LogURL    string `json:"logUrl"`
							}
						}
					}
				}
				PullRequests struct {
					Nodes []struct {
						Number                         int
						Title, URL, State, BaseRefName string
						MergedAt                       time.Time
						MergeCommit                    *struct{ Oid string }
						Author                         struct{ Login string }
					}
				}
			}
		}
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	si := &shipInfo{}
	for _, d := range v.Data.Repository.Deployments.Nodes {
		g := ghDeploy{env: d.Environment, sha: d.CommitOid, at: d.CreatedAt, state: "run"} // no status yet: queued
		if st := d.Newest.Nodes; len(st) > 0 {
			g.url = link(st[0].LogURL)
			// Railway says SUCCESS again every few minutes while a deploy runs: it went live at the first
			live := time.Time{}
			for _, s := range slices.Concat(d.Oldest.Nodes, st) {
				if s.State == "SUCCESS" && (live.IsZero() || s.CreatedAt.Before(live)) {
					live = s.CreatedAt
				}
			}
			switch st[0].State {
			case "SUCCESS", "INACTIVE": // inactive: replaced by a newer deploy; it was live if it succeeded first
				g.state, g.done = "ok", live
				if live.IsZero() {
					g.state = "skip"
				}
			case "FAILURE", "ERROR":
				g.state, g.done = "fail", st[0].CreatedAt
			case "PENDING", "QUEUED", "IN_PROGRESS", "WAITING":
			default:
				g.state = "skip"
			}
		}
		si.deploys = append(si.deploys, g)
	}
	sort.SliceStable(si.deploys, func(i, j int) bool { return si.deploys[i].at.Before(si.deploys[j].at) })
	for _, p := range v.Data.Repository.PullRequests.Nodes {
		if branchTier(p.BaseRefName) != "production" {
			continue
		}
		r := &PRStatus{Number: p.Number, Title: trunc(p.Title, 300), URL: link(p.URL), State: p.State, Base: trunc(p.BaseRefName, 200), MergedAt: p.MergedAt, Checks: []PRCheck{}, Author: trunc(p.Author.Login, 100)}
		if p.MergeCommit != nil {
			r.merge = p.MergeCommit.Oid
		}
		si.releases = append(si.releases, r)
	}
	sort.SliceStable(si.releases, func(i, j int) bool {
		a, b := si.releases[i].MergedAt, si.releases[j].MergedAt
		return !a.IsZero() && (b.IsZero() || a.Before(b))
	})
	return si, nil
}

// branchTier: the environment a branch deploys to.
func branchTier(b string) string {
	switch b {
	case "main", "master", "production", "prod":
		return "production"
	case "stage", "staging":
		return "stage"
	}
	return ""
}

// envTier: the same for a deployment environment ("Alven AI Server / production", "Production").
func envTier(env string) string {
	n := strings.ToLower(env)
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	switch n = strings.TrimSpace(n); {
	case strings.HasPrefix(n, "prod"):
		return "production"
	case strings.HasPrefix(n, "stag"):
		return "stage"
	}
	return ""
}

// envs: the repo's environments of a tier that something still deploys to (one untouched for two weeks
// is a service nobody runs any more).
func (si *shipInfo) envs(tier string, now time.Time) []string {
	var out []string
	for i := len(si.deploys) - 1; i >= 0; i-- {
		d := si.deploys[i]
		if envTier(d.env) == tier && now.Sub(d.at) < 14*24*time.Hour && !slices.Contains(out, d.env) {
			out = append(out, d.env)
		}
	}
	sort.Strings(out)
	return out
}

var deployRank = map[string]int{"fail": 0, "run": 1, "wait": 2, "live": 3}

// deployed: whether the change merged as sha at time at is live in the tier's environments (the worst of
// them, when there are several services). nil when nothing deploys that tier.
func (si *shipInfo) deployed(tier, sha string, at, now time.Time) *PRDeploy {
	envs := si.envs(tier, now)
	if len(envs) == 0 {
		return nil
	}
	out := PRDeploy{Env: tier, State: "live"}
	var notes []string
	for _, env := range envs {
		var ds []ghDeploy
		for _, d := range si.deploys {
			if d.env == env {
				ds = append(ds, d)
			}
		}
		i0 := slices.IndexFunc(ds, func(d ghDeploy) bool { return sha != "" && d.sha == sha })
		if i0 < 0 {
			i0 = slices.IndexFunc(ds, func(d ghDeploy) bool { return !d.at.Before(at.Add(-time.Minute)) }) // clocks differ a little
		}
		if i0 < 0 {
			continue // nothing deployed there since: not yet, or a service nobody deploys
		}
		e := PRDeploy{Env: tier, State: "wait"}
		{
			for _, d := range ds[i0:] {
				if d.state == "ok" {
					e = PRDeploy{Env: tier, State: "live", At: d.done, URL: d.url}
					break
				}
				if d.state != "skip" { // the newest attempt says what's happening
					e = PRDeploy{Env: tier, State: d.state, At: d.done, URL: d.url} // run or fail
				}
			}
		}
		notes = append(notes, strings.TrimSpace(strings.Split(env, " / ")[0])+": "+map[string]string{"live": "live", "run": "deploying", "fail": "failed", "wait": "not deployed yet"}[e.State])
		if deployRank[e.State] < deployRank[out.State] || (e.State == out.State && e.At.After(out.At)) {
			out = e
		}
	}
	switch {
	case len(notes) == 0:
		return &PRDeploy{Env: tier, State: "wait"}
	case len(notes) > 1:
		out.Note = strings.Join(notes, " · ")
	}
	return &out
}

// release: the stage → production PR that takes a change merged into stage at time at there: the first one
// merged after it, else the open one it will go in.
func (si *shipInfo) release(at time.Time) *PRStatus {
	for _, r := range si.releases {
		if r.State == "MERGED" && r.MergedAt.After(at) {
			return r
		}
	}
	for _, r := range si.releases {
		if r.State == "OPEN" {
			return r
		}
	}
	return nil
}

// shipPRs fills in each merged PR's release and deploys, and returns the releases that carry the session's
// PRs but that it didn't link itself (so the page can group them under it).
func shipPRs(prs []*PRStatus, info map[string]*shipInfo, now time.Time) []*PRStatus {
	have := map[string]bool{}
	for _, p := range prs {
		have[p.Repo+"#"+strconv.Itoa(p.Number)] = true
	}
	var extra []*PRStatus
	add := func(p *PRStatus, d *PRDeploy) {
		if d != nil {
			p.Deploys = append(p.Deploys, *d)
		}
	}
	for _, p := range prs {
		si := info[p.Repo]
		p.Deploys, p.Release = nil, 0
		if si == nil || p.State != "MERGED" {
			continue
		}
		tier := branchTier(p.Base)
		add(p, si.deployed(tier, p.merge, p.MergedAt, now))
		if tier != "stage" {
			continue
		}
		r := si.release(p.MergedAt)
		if r != nil {
			p.Release = r.Number
			if k := p.Repo + "#" + strconv.Itoa(r.Number); !have[k] {
				have[k] = true
				x := *r
				x.Repo, x.Head, x.Ext = p.Repo, p.Base, true
				if x.State == "MERGED" {
					add(&x, si.deployed("production", x.merge, x.MergedAt, now))
				} else if len(si.envs("production", now)) > 0 {
					x.Deploys = []PRDeploy{{Env: "production", State: "wait"}}
				}
				extra = append(extra, &x)
			}
		}
		if r != nil && r.State == "MERGED" {
			add(p, si.deployed("production", r.merge, r.MergedAt, now))
		} else if len(si.envs("production", now)) > 0 {
			add(p, &PRDeploy{Env: "production", State: "wait"})
		}
	}
	return extra
}

// ship: a repo's deploys and releases (stage's PRs into production). The query takes GitHub a second or two,
// so it's kept 30 s while something is on its way (a deploy running, a PR merged in the last 15 min; the page
// asks every 30 s while a chat is open), else 3 min. When gh fails the last answer stays.
func (c *prCache) ship(repo, stage string, merged, now time.Time) *shipInfo {
	k := repo + "@" + stage
	c.mu.Lock()
	e, ok := c.ships[k]
	c.mu.Unlock()
	ttl := 3 * time.Minute
	if e.si == nil || now.Sub(merged) < 15*time.Minute || slices.ContainsFunc(e.si.deploys, func(d ghDeploy) bool { return d.state == "run" }) {
		ttl = 30 * time.Second
	}
	if ok && now.Sub(e.at) < ttl {
		return e.si
	}
	o, n, _ := strings.Cut(repo, "/")
	if b, err := c.fetch("", "api", "graphql", "-f", "query="+shipQuery, "-f", "o="+o, "-f", "n="+n, "-f", "h="+stage); err == nil {
		if si, err := parseShip(b); err == nil {
			e.si = si
		}
	}
	c.mu.Lock()
	if c.ships == nil {
		c.ships = map[string]shipEntry{}
	}
	c.ships[k] = shipEntry{now, e.si}
	c.mu.Unlock()
	return e.si
}

type shipEntry struct {
	at time.Time
	si *shipInfo
}
