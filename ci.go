package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Runner logs (runner.log, appended by ensure-runners.sh) carry one line per job event:
//
//	2026-09-29 19:19:15Z: Running job: <name>
//	2026-09-29 19:19:56Z: Job <name> completed with result: <Succeeded|Failed|Canceled>
var (
	jobStartRe = regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)Z: Running job: (.+)$`)
	jobDoneRe  = regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)Z: Job (.+) completed with result: (\w+)$`)
	restartRe  = regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\dZ: (Listening for Jobs|Exiting runner)`)
	runnerName = regexp.MustCompile(`^(r|fe)(\d+)$`)
)

type Job struct {
	Runner string   `json:"runner"`
	Name   string   `json:"name"`
	Result string   `json:"result"` // Succeeded, Failed, Canceled, Interrupted, Running
	Start  int64    `json:"start"`
	End    int64    `json:"end"`
	Meta   *JobMeta `json:"meta,omitempty"`
}

// JobMeta is the GitHub context of a job, read from the runner's _diag/Worker_*.log.
type JobMeta struct {
	Repo     string `json:"repo,omitempty"`
	PR       int    `json:"pr,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Base     string `json:"base,omitempty"`
	Workflow string `json:"workflow,omitempty"`
	Event    string `json:"event,omitempty"`
	RunID    string `json:"runId,omitempty"`
	Actor    string `json:"actor,omitempty"`
	Title    string `json:"title,omitempty"` // PR title (gh), when known
}

type runnerState struct {
	off     int64
	cur     *Job
	jobs    []Job
	matched int // jobs[:matched] have been matched to worker logs
	workers []*workerLog
}

type workerLog struct {
	at     int64
	path   string
	name   string // jobDisplayName
	meta   *JobMeta
	parsed bool
}

// interrupt closes an open job whose completion will never be logged; its duration is unknown (0).
func (r *runnerState) interrupt() {
	if r.cur != nil {
		j := *r.cur
		j.Result, j.End = "Interrupted", j.Start
		r.jobs = append(r.jobs, j)
		r.cur = nil
	}
}

// feed consumes complete lines from chunk and returns how many bytes it used.
func feed(r *runnerState, name string, chunk []byte) int {
	used := 0
	for {
		i := bytes.IndexByte(chunk[used:], '\n')
		if i < 0 {
			return used
		}
		line := strings.TrimRight(string(chunk[used:used+i]), "\r")
		used += i + 1
		if m := jobStartRe.FindStringSubmatch(line); m != nil {
			r.interrupt()
			r.cur = &Job{Runner: name, Name: m[2], Result: "Running", Start: parseRunnerTime(m[1])}
		} else if m := jobDoneRe.FindStringSubmatch(line); m != nil {
			j := Job{Runner: name, Name: m[2], Result: m[3], End: parseRunnerTime(m[1])}
			j.Start = j.End
			if r.cur != nil && r.cur.Name == m[2] {
				j.Start = r.cur.Start
			}
			r.cur = nil
			r.jobs = append(r.jobs, j)
		} else if restartRe.MatchString(line) {
			r.interrupt()
		}
	}
}

func parseRunnerTime(s string) int64 {
	t, _ := time.Parse("2006-01-02 15:04:05", s)
	return t.Unix()
}

type Runner struct {
	Name  string   `json:"name"`
	State string   `json:"state"` // busy, idle, dead
	Job   string   `json:"job,omitempty"`
	Since int64    `json:"since,omitempty"`
	PID   int      `json:"pid"`
	Meta  *JobMeta `json:"meta,omitempty"`
}

type Totals struct {
	Jobs    int     `json:"jobs"`
	Failed  int     `json:"failed"`
	Minutes float64 `json:"minutes"`
	Success float64 `json:"success"` // % of Succeeded among Succeeded+Failed
	other   int     // Canceled/Interrupted: counted in Jobs, excluded from Success
}

type NameStat struct {
	Name    string  `json:"name"`
	Count   int     `json:"count"`
	Success float64 `json:"success"`
	AvgSec  float64 `json:"avgSec"`
}

// WorkflowStat groups job types under their workflow (CI, Nightly, …).
type WorkflowStat struct {
	Name    string     `json:"name"`
	Count   int        `json:"count"`
	Success float64    `json:"success"`
	Jobs    []NameStat `json:"jobs"`
}

type CI struct {
	Runners    []Runner       `json:"runners"`
	Day        Totals         `json:"day"`
	Week       Totals         `json:"week"`
	All        Totals         `json:"all"`
	ByWorkflow []WorkflowStat `json:"byWorkflow"`
	Recent     []Job          `json:"recent"` // completed jobs, newest first
	FailedDay  int            `json:"failedDay"`
	Err        string         `json:"err,omitempty"`
}

type ciTracker struct {
	dir     string
	mu      sync.Mutex
	runners map[string]*runnerState
	titles  *prTitles // nil: no PR titles
}

func (c *ciTracker) poll(now time.Time, alive func(pid int, dir string) bool) CI {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runners == nil {
		c.runners = map[string]*runnerState{}
	}
	var out CI
	pids, err := filepath.Glob(filepath.Join(c.dir, "*", "runner.pid"))
	if err != nil || len(pids) == 0 {
		if _, err := os.Stat(c.dir); err != nil {
			out.Err = err.Error()
		}
	}
	var all []Job
	for _, pf := range pids {
		name := filepath.Base(filepath.Dir(pf))
		if !runnerName.MatchString(name) {
			continue
		}
		r := c.runners[name]
		if r == nil {
			r = &runnerState{}
			c.runners[name] = r
		}
		c.read(r, name)
		c.match(r, filepath.Join(c.dir, name, "_diag"))
		b, _ := os.ReadFile(pf)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		st := Runner{Name: name, PID: pid, State: "idle"}
		switch {
		case pid <= 0 || !alive(pid, filepath.Join(c.dir, name)):
			st.State = "dead"
			r.interrupt()
		case r.cur != nil:
			st.State, st.Job, st.Since, st.Meta = "busy", r.cur.Name, r.cur.Start, c.withTitle(r.cur.Meta)
		}
		out.Runners = append(out.Runners, st)
		all = append(all, r.jobs...)
	}
	sort.Slice(out.Runners, func(i, j int) bool { return runnerLess(out.Runners[i].Name, out.Runners[j].Name) })

	dayCut, weekCut := now.Add(-24*time.Hour).Unix(), now.Add(-7*24*time.Hour).Unix()
	type agg struct {
		n, ok, bad int
		sec        float64
	}
	byName := map[[2]string]*agg{} // workflow, job name
	wf := map[string]*agg{}
	for _, j := range all {
		if j.Start >= dayCut {
			addJob(&out.Day, j)
		}
		if j.Start >= weekCut {
			addJob(&out.Week, j)
		}
		addJob(&out.All, j)
		w := "Other"
		if j.Meta != nil && j.Meta.Workflow != "" {
			w = j.Meta.Workflow
		}
		for _, a := range []*agg{getAgg(byName, [2]string{w, j.Name}), getAgg(wf, w)} {
			a.n++
			a.sec += float64(j.End - j.Start)
			switch j.Result {
			case "Succeeded":
				a.ok++
			case "Failed":
				a.bad++
			}
		}
	}
	for _, t := range []*Totals{&out.Day, &out.Week, &out.All} {
		t.Success = pct(t.Jobs-t.Failed-t.other, t.Jobs-t.other)
	}
	out.FailedDay = out.Day.Failed
	for w, a := range wf {
		ws := WorkflowStat{Name: w, Count: a.n, Success: pct(a.ok, a.ok+a.bad)}
		for k, j := range byName {
			if k[0] == w {
				ws.Jobs = append(ws.Jobs, NameStat{Name: k[1], Count: j.n, Success: pct(j.ok, j.ok+j.bad), AvgSec: j.sec / float64(j.n)})
			}
		}
		sort.Slice(ws.Jobs, func(i, j int) bool {
			a, b := ws.Jobs[i], ws.Jobs[j]
			return a.Count > b.Count || (a.Count == b.Count && a.Name < b.Name)
		})
		out.ByWorkflow = append(out.ByWorkflow, ws)
	}
	sort.Slice(out.ByWorkflow, func(i, j int) bool { // named workflows first, "Other" last
		a, b := out.ByWorkflow[i], out.ByWorkflow[j]
		if (a.Name == "Other") != (b.Name == "Other") {
			return b.Name == "Other"
		}
		return a.Count > b.Count || (a.Count == b.Count && a.Name < b.Name)
	})
	sort.Slice(all, func(i, j int) bool { return all[i].Start > all[j].Start })
	out.Recent = all[:min(len(all), 200)]
	for i := range out.Recent {
		out.Recent[i].Meta = c.withTitle(out.Recent[i].Meta)
	}
	return out
}

func getAgg[K comparable, V any](m map[K]*V, k K) *V {
	if m[k] == nil {
		m[k] = new(V)
	}
	return m[k]
}

var workerNameRe = regexp.MustCompile(`^Worker_(\d{8}-\d{6})-utc\.log$`)

// match attaches GitHub context to new jobs and the running one: the worker log of the same job
// name that started closest to the job, within 2 min.
func (c *ciTracker) match(r *runnerState, diag string) {
	ents, err := os.ReadDir(diag)
	if err != nil {
		return
	}
	known := map[string]bool{}
	for _, w := range r.workers {
		known[w.path] = true
	}
	for _, e := range ents {
		m := workerNameRe.FindStringSubmatch(e.Name())
		p := filepath.Join(diag, e.Name())
		if m == nil || known[p] {
			continue
		}
		t, err := time.Parse("20060102-150405", m[1])
		if err == nil {
			r.workers = append(r.workers, &workerLog{at: t.Unix(), path: p})
		}
	}
	find := func(j *Job) *JobMeta {
		var best *workerLog
		for _, w := range r.workers {
			d := w.at - j.Start
			if d < -120 || d > 120 {
				continue
			}
			if !w.parsed {
				w.name, w.meta, w.parsed = workerMeta(w.path)
			}
			if w.parsed && w.name == j.Name && (best == nil || abs64(w.at-j.Start) < abs64(best.at-j.Start)) {
				best = w
			}
		}
		if best == nil {
			return nil
		}
		return best.meta
	}
	for ; r.matched < len(r.jobs); r.matched++ {
		r.jobs[r.matched].Meta = find(&r.jobs[r.matched])
	}
	if r.cur != nil && r.cur.Meta == nil {
		r.cur.Meta = find(r.cur)
	}
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

var (
	ghKVRe    = regexp.MustCompile(`"k":\s*"(\w+)",\s*"v":\s*"([^"]*)"`)
	jobNameRe = regexp.MustCompile(`"jobDisplayName":\s*"((?:[^"\\]|\\.)*)"`)
	prRefRe   = regexp.MustCompile(`^refs/pull/(\d+)/`)
)

// workerMeta reads a job's display name and GitHub context from the head of its worker log
// (the job message sits in the first ~25 KB). ok is false until the job message is there.
func workerMeta(path string) (name string, m *JobMeta, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, false
	}
	defer f.Close()
	b := make([]byte, 128<<10)
	n, _ := f.ReadAt(b, 0)
	b = b[:n]
	kv := map[string]string{}
	for _, x := range ghKVRe.FindAllSubmatch(b, -1) {
		if _, seen := kv[string(x[1])]; !seen {
			kv[string(x[1])] = string(x[2])
		}
	}
	jm := jobNameRe.FindSubmatch(b)
	if jm == nil || kv["event_name"] == "" {
		return "", nil, false
	}
	json.Unmarshal([]byte(`"`+string(jm[1])+`"`), &name)
	m = &JobMeta{Repo: trunc(kv["repository"], 100), Branch: trunc(kv["head_ref"], 120), Base: trunc(kv["base_ref"], 120),
		Workflow: trunc(kv["workflow"], 80), Event: trunc(kv["event_name"], 40), RunID: trunc(kv["run_id"], 20), Actor: trunc(kv["actor"], 60)}
	if pm := prRefRe.FindStringSubmatch(kv["ref"]); pm != nil {
		m.PR, _ = strconv.Atoi(pm[1])
	} else {
		m.Branch, m.Base = trunc(kv["ref_name"], 120), ""
	}
	return trunc(name, 200), m, true
}

// withTitle returns meta with its PR title filled in (a copy: cached metas stay title-free).
func (c *ciTracker) withTitle(m *JobMeta) *JobMeta {
	if m == nil || m.PR == 0 || c.titles == nil {
		return m
	}
	cp := *m
	cp.Title = c.titles.get(m.Repo, m.PR)
	return &cp
}

// read appends whatever the log gained since the last poll; a shrunk file was rotated, so start over.
func (c *ciTracker) read(r *runnerState, name string) {
	f, err := os.Open(filepath.Join(c.dir, name, "runner.log"))
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	if fi.Size() < r.off {
		r.off = 0
	}
	if fi.Size() == r.off {
		return
	}
	buf := make([]byte, min(fi.Size()-r.off, 8<<20))
	n, _ := f.ReadAt(buf, r.off)
	r.off += int64(feed(r, name, buf[:n]))
}

func addJob(t *Totals, j Job) {
	t.Jobs++
	t.Minutes += float64(j.End-j.Start) / 60
	switch j.Result {
	case "Failed":
		t.Failed++
	case "Succeeded":
	default:
		t.other++
	}
}

func pct(a, b int) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b) * 100
}

func runnerLess(a, b string) bool {
	ma, mb := runnerName.FindStringSubmatch(a), runnerName.FindStringSubmatch(b)
	if ma[1] != mb[1] {
		return ma[1] == "r" // backend runners first
	}
	na, _ := strconv.Atoi(ma[2])
	nb, _ := strconv.Atoi(mb[2])
	return na < nb
}

// prTitles looks PR titles up with gh in the background, one at a time, cached for 30 min
// (failures too, so a broken gh isn't hammered).
type prTitles struct {
	mu    sync.Mutex
	m     map[string]titleEntry
	queue chan prRef
	fetch func(repo string, pr int) (string, error)
}

type titleEntry struct {
	title   string
	at      time.Time
	pending bool
}

type prRef struct {
	repo string
	pr   int
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func newPRTitles(fetch func(string, int) (string, error)) *prTitles {
	p := &prTitles{m: map[string]titleEntry{}, queue: make(chan prRef, 256), fetch: fetch}
	go func() {
		for r := range p.queue {
			title, err := p.fetch(r.repo, r.pr)
			p.mu.Lock()
			k := r.repo + "#" + strconv.Itoa(r.pr)
			e := p.m[k]
			e.pending, e.at = false, time.Now()
			if err == nil {
				e.title = trunc(title, 200)
			}
			p.m[k] = e
			p.mu.Unlock()
		}
	}()
	return p
}

// get returns the cached title ("" until fetched) and queues a fetch when missing or stale.
func (p *prTitles) get(repo string, pr int) string {
	if !repoRe.MatchString(repo) || pr <= 0 {
		return ""
	}
	k := repo + "#" + strconv.Itoa(pr)
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[k]
	if !e.pending && (!ok || time.Since(e.at) > 30*time.Minute) {
		select {
		case p.queue <- prRef{repo, pr}:
			e.pending = true
			p.m[k] = e
		default:
		}
	}
	return e.title
}

func ghTitle(repo string, pr int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "api", "repos/"+repo+"/pulls/"+strconv.Itoa(pr), "--jq", ".title")
	cmd.Env = cleanEnv(os.Environ())
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
