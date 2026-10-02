package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ts(s string) int64 {
	t, _ := time.Parse("2006-01-02 15:04:05", s)
	return t.Unix()
}

func TestFeedParsesCompletedJob(t *testing.T) {
	var r runnerState
	log := "2026-09-29 19:08:04Z: Listening for Jobs\n" +
		"2026-09-29 19:19:15Z: Running job: Static checks (migrations, ruff, ty)\n" +
		"2026-09-29 19:19:56Z: Job Static checks (migrations, ruff, ty) completed with result: Succeeded\n"
	if n := feed(&r, "r1", []byte(log)); n != len(log) {
		t.Fatalf("consumed %d of %d", n, len(log))
	}
	if len(r.jobs) != 1 || r.cur != nil {
		t.Fatalf("jobs=%+v cur=%+v", r.jobs, r.cur)
	}
	j := r.jobs[0]
	if j.Runner != "r1" || j.Name != "Static checks (migrations, ruff, ty)" || j.Result != "Succeeded" ||
		j.Start != ts("2026-09-29 19:19:15") || j.End != ts("2026-09-29 19:19:56") {
		t.Fatalf("job = %+v", j)
	}
}

func TestFeedLeavesPartialLine(t *testing.T) {
	var r runnerState
	chunk := "2026-09-29 19:19:15Z: Running job: A\n2026-09-29 19:19:56Z: Job A compl"
	n := feed(&r, "r1", []byte(chunk))
	if n != strings.Index(chunk, "\n")+1 {
		t.Fatalf("consumed %d, want only the complete line", n)
	}
	if r.cur == nil || r.cur.Name != "A" {
		t.Fatalf("cur = %+v", r.cur)
	}
}

func TestFeedInterruptedByNextStartOrRestart(t *testing.T) {
	var r runnerState
	feed(&r, "r1", []byte("2026-09-29 10:00:00Z: Running job: A\n"+
		"2026-09-29 10:05:00Z: Running job: B\n"+
		"2026-09-29 10:06:00Z: Listening for Jobs\n"))
	if len(r.jobs) != 2 || r.cur != nil {
		t.Fatalf("jobs=%+v cur=%+v", r.jobs, r.cur)
	}
	for _, j := range r.jobs {
		if j.Result != "Interrupted" || j.End != j.Start {
			t.Fatalf("job %+v should be Interrupted with unknown (zero) duration", j)
		}
	}
}

func setupRunners(t *testing.T, logs map[string]string) string {
	dir := t.TempDir()
	for name, log := range logs {
		d := filepath.Join(dir, name)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "runner.pid"), []byte("4242\n"), 0o644)
		os.WriteFile(filepath.Join(d, "runner.log"), []byte(log), 0o644)
	}
	os.MkdirAll(filepath.Join(dir, "notarunner"), 0o755)
	return dir
}

func TestPollStatesTotalsAndIncrementalRead(t *testing.T) {
	now := time.Unix(ts("2026-09-29 20:00:00"), 0)
	dir := setupRunners(t, map[string]string{
		"r1": "2026-09-20 10:00:00Z: Running job: Old\n2026-09-20 10:10:00Z: Job Old completed with result: Succeeded\n" +
			"2026-09-29 12:00:00Z: Running job: Test\n2026-09-29 12:02:00Z: Job Test completed with result: Failed\n" +
			"2026-09-29 19:59:00Z: Running job: Build\n",
		"fe1": "2026-09-27 12:00:00Z: Running job: Test\n2026-09-27 12:04:00Z: Job Test completed with result: Succeeded\n",
		"r2":  "2026-09-29 19:00:00Z: Running job: Lint\n",
	})
	c := &ciTracker{dir: dir}
	alive := func(pid int, dir string) bool { return true }
	got := c.poll(now, alive)

	names := []string{}
	for _, r := range got.Runners {
		names = append(names, r.Name+"="+r.State)
	}
	if strings.Join(names, " ") != "r1=busy r2=busy fe1=idle" {
		t.Fatalf("runners = %v", names)
	}
	if got.Runners[0].Job != "Build" || got.Runners[0].Since != ts("2026-09-29 19:59:00") || got.Runners[0].PID != 4242 {
		t.Fatalf("r1 = %+v", got.Runners[0])
	}
	if got.Day.Jobs != 1 || got.Day.Failed != 1 || got.Day.Minutes != 2 || got.FailedDay != 1 {
		t.Fatalf("day = %+v failedDay=%d", got.Day, got.FailedDay)
	}
	if got.Week.Jobs != 2 || got.Week.Minutes != 6 || got.Week.Success != 50 {
		t.Fatalf("week = %+v", got.Week)
	}
	if got.All.Jobs != 3 || got.All.Minutes != 16 {
		t.Fatalf("all = %+v", got.All)
	}
	if len(got.ByWorkflow) != 1 || got.ByWorkflow[0].Name != "Other" || got.ByWorkflow[0].Jobs[0].Name != "Test" ||
		got.ByWorkflow[0].Jobs[0].Count != 2 || got.ByWorkflow[0].Jobs[0].Success != 50 || got.ByWorkflow[0].Jobs[0].AvgSec != 180 {
		t.Fatalf("byWorkflow = %+v", got.ByWorkflow)
	}
	if len(got.Recent) != 3 || got.Recent[0].Name != "Test" || got.Recent[0].Result != "Failed" {
		t.Fatalf("recent (completed only) = %+v", got.Recent)
	}

	// incremental: append a completion; poll picks it up without double-counting
	f, _ := os.OpenFile(filepath.Join(dir, "r1", "runner.log"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("2026-09-29 19:59:30Z: Job Build completed with result: Succeeded\n")
	f.Close()
	got = c.poll(now, alive)
	if got.Runners[0].State != "idle" || got.All.Jobs != 4 {
		t.Fatalf("after append: r1=%+v all=%+v", got.Runners[0], got.All)
	}

	// shrunk (rotated) file: re-read from the start, keep history
	os.WriteFile(filepath.Join(dir, "r1", "runner.log"), []byte("2026-09-29 19:59:50Z: Running job: New\n"), 0o644)
	got = c.poll(now, alive)
	if got.Runners[0].Job != "New" || got.All.Jobs != 4 {
		t.Fatalf("after rotate: r1=%+v all=%+v", got.Runners[0], got.All)
	}
}

func TestPollDeadRunnerClosesOpenJob(t *testing.T) {
	now := time.Unix(ts("2026-09-29 20:00:00"), 0)
	dir := setupRunners(t, map[string]string{"r3": "2026-09-29 19:00:00Z: Running job: Lint\n"})
	c := &ciTracker{dir: dir}
	got := c.poll(now, func(int, string) bool { return false })
	if got.Runners[0].State != "dead" || got.Runners[0].Job != "" {
		t.Fatalf("r3 = %+v", got.Runners[0])
	}
	if len(got.Recent) != 1 || got.Recent[0].Result != "Interrupted" {
		t.Fatalf("recent = %+v", got.Recent)
	}
}

func TestPollMissingDir(t *testing.T) {
	c := &ciTracker{dir: "/nonexistent-ci-dir"}
	got := c.poll(time.Now(), func(int, string) bool { return true })
	if len(got.Runners) != 0 {
		t.Fatalf("runners = %+v", got.Runners)
	}
}

func workerLogText(name, ref, headRef, event, workflow string) string {
	return `[2026-09-30 01:52:25Z INFO HostContext] No proxy settings
[2026-09-30 01:52:26Z INFO Worker] Job message:
{
  "jobId": "85ef1227",
  "jobDisplayName": "` + name + `",
  "contextData": {
    "github": {
      "t": 2,
      "d": [
        {
          "k": "ref",
          "v": "` + ref + `"
        },
        {
          "k": "repository",
          "v": "roomss-ltd/alven-estate-agent"
        },
        {
          "k": "run_id",
          "v": "36657166725"
        },
        {
          "k": "actor",
          "v": "serezha13"
        },
        {
          "k": "workflow",
          "v": "` + workflow + `"
        },
        {
          "k": "head_ref",
          "v": "` + headRef + `"
        },
        {
          "k": "base_ref",
          "v": "main"
        },
        {
          "k": "event_name",
          "v": "` + event + `"
        },
        {
          "k": "ref_name",
          "v": "main"
        }
      ]
    }
  }
}
`
}

func TestWorkerMeta(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "Worker_20260930-015225-utc.log")
	os.WriteFile(p, []byte(workerLogText("Static checks", "refs/pull/10684/merge", "stage", "pull_request", "CI")), 0o644)
	name, m, ok := workerMeta(p)
	if !ok || name != "Static checks" {
		t.Fatalf("name=%q ok=%v", name, ok)
	}
	want := JobMeta{Repo: "roomss-ltd/alven-estate-agent", PR: 10684, Branch: "stage", Base: "main", Workflow: "CI", Event: "pull_request", RunID: "36657166725", Actor: "serezha13"}
	if *m != want {
		t.Fatalf("meta = %+v", *m)
	}
	os.WriteFile(p, []byte(workerLogText("Full suite (main)", "refs/heads/main", "", "workflow_dispatch", "Nightly")), 0o644)
	_, m, _ = workerMeta(p)
	if m.PR != 0 || m.Branch != "main" || m.Base != "" || m.Workflow != "Nightly" {
		t.Fatalf("dispatch meta = %+v", *m)
	}
	os.WriteFile(p, []byte("[2026-09-30 01:52:25Z INFO HostContext] starting\n"), 0o644)
	if _, _, ok := workerMeta(p); ok {
		t.Fatal("a log without the job message yet must not count as parsed")
	}
}

func TestPollAttachesWorkerMetaAndGroupsByWorkflow(t *testing.T) {
	now := time.Unix(ts("2026-09-30 03:00:00"), 0)
	dir := setupRunners(t, map[string]string{
		"r1": "2026-09-30 01:00:05Z: Running job: Static checks\n2026-09-30 01:01:05Z: Job Static checks completed with result: Succeeded\n" +
			"2026-09-30 02:00:05Z: Running job: Full suite (main)\n2026-09-30 02:30:05Z: Job Full suite (main) completed with result: Failed\n" +
			"2026-09-30 02:59:00Z: Running job: Build\n",
	})
	diag := filepath.Join(dir, "r1", "_diag")
	os.MkdirAll(diag, 0o755)
	os.WriteFile(filepath.Join(diag, "Worker_20260930-010003-utc.log"), []byte(workerLogText("Static checks", "refs/pull/10684/merge", "stage", "pull_request", "CI")), 0o644)
	os.WriteFile(filepath.Join(diag, "Worker_20260930-020004-utc.log"), []byte(workerLogText("Full suite (main)", "refs/heads/main", "", "workflow_dispatch", "Nightly")), 0o644)
	os.WriteFile(filepath.Join(diag, "Worker_20260930-025858-utc.log"), []byte(workerLogText("Build", "refs/pull/10700/merge", "feat/x", "pull_request", "CI")), 0o644)
	c := &ciTracker{dir: dir}
	got := c.poll(now, func(int, string) bool { return true })
	if r := got.Runners[0]; r.State != "busy" || r.Meta == nil || r.Meta.PR != 10700 || r.Meta.Branch != "feat/x" {
		t.Fatalf("runner = %+v meta=%+v", r, r.Meta)
	}
	if len(got.Recent) != 2 || got.Recent[0].Meta == nil || got.Recent[0].Meta.Workflow != "Nightly" || got.Recent[1].Meta.PR != 10684 {
		t.Fatalf("recent = %+v", got.Recent)
	}
	var names []string
	for _, w := range got.ByWorkflow {
		names = append(names, w.Name+":"+itoa(w.Count)+":"+itoa(int(w.Success)))
	}
	if strings.Join(names, " ") != "CI:1:100 Nightly:1:0" {
		t.Fatalf("byWorkflow = %v", names)
	}
}

func TestPRTitlesFetchInBackgroundOnce(t *testing.T) {
	calls := 0
	p := newPRTitles(func(repo string, pr int) (string, error) {
		calls++
		return "Fix the " + repo + " flake", nil
	})
	if got := p.get("o/r", 7); got != "" {
		t.Fatalf("first get should not block on gh, got %q", got)
	}
	var got string
	for i := 0; i < 100 && got == ""; i++ {
		time.Sleep(10 * time.Millisecond)
		got = p.get("o/r", 7)
	}
	if got != "Fix the o/r flake" || calls != 1 {
		t.Fatalf("title=%q calls=%d", got, calls)
	}
	if p.get("o/r; rm -rf /", 7) != "" || p.get("o/r", 0) != "" {
		t.Fatal("bad repo or PR number accepted")
	}
}
