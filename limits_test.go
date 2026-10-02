package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeOps records every syscall the limiter would make.
type fakeOps struct {
	calls []string
	nice  map[int]int
}

func (f *fakeOps) ops(protected map[int]string) procOps {
	return procOps{
		tids: func(pid int) []int { return []int{pid, pid + 1} },
		affinity: func(tid int, cores []int) error {
			f.calls = append(f.calls, "aff "+itoa(tid)+" "+joinInts(cores))
			return nil
		},
		nice:    func(tid int) (int, error) { return f.nice[tid], nil },
		setNice: func(tid, n int) error { f.calls = append(f.calls, "nice "+itoa(tid)+" "+itoa(n)); return nil },
		ioprio: func(tid int, idle bool) error {
			f.calls = append(f.calls, "io "+itoa(tid)+" "+map[bool]string{true: "idle", false: "be"}[idle])
			return nil
		},
		signal: func(pid int, s syscall.Signal) error {
			f.calls = append(f.calls, "sig "+itoa(pid)+" "+map[syscall.Signal]string{syscall.SIGSTOP: "STOP", syscall.SIGCONT: "CONT"}[s])
			return nil
		},
		guard: func(pid int) (touch, pause bool) {
			switch protected[pid] {
			case "never":
				return false, false
			case "nopause":
				return true, false
			}
			return true, true
		},
	}
}

func TestLimiterApplyAndRemove(t *testing.T) {
	f := &fakeOps{nice: map[int]int{100: 0, 101: 0, 200: 12, 201: 12}}
	l := newLimiter(filepath.Join(t.TempDir(), "limits.json"), 10, f.ops(map[int]string{300: "never", 400: "nopause"}))
	rule := Limit{Key: "runner:r1", Label: "CI runner r1", Cores: []int{0, 1}, Nice: 10, IOIdle: true, Paused: true}
	if err := l.set(rule); err != nil {
		t.Fatal(err)
	}
	l.apply(map[string][]int{"runner:r1": {100, 200, 300, 400}, "other": {500}})
	got := strings.Join(f.calls, "\n")
	for _, want := range []string{"aff 100 0,1", "aff 101 0,1", "nice 100 10", "nice 101 10", "io 100 idle", "sig 100 STOP", "aff 400 0,1", "sig 200 STOP"} {
		if !strings.Contains(got, want+"\n") && !strings.HasSuffix(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	for _, bad := range []string{"nice 200", " 300 ", "sig 400", " 500 "} {
		if strings.Contains(got+"\n", bad) {
			t.Errorf("unexpected %q (nice only goes up; guarded pids untouched)", bad)
		}
	}
	f.calls = nil
	if err := l.remove("runner:r1"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("remove must leave syscalls to the enforcer, got %v", f.calls)
	}
	l.apply(map[string][]int{"runner:r1": {100}})
	if strings.Join(f.calls, "|") != "aff 100 0,1,2,3,4,5,6,7,8,9|io 100 be|aff 101 0,1,2,3,4,5,6,7,8,9|io 101 be|sig 100 CONT" {
		t.Fatalf("revert calls = %v", f.calls)
	}
	if len(l.list()) != 0 {
		t.Fatalf("rules after remove: %+v", l.list())
	}
}

func TestLimiterPersistsRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "limits.json")
	f := &fakeOps{}
	l := newLimiter(path, 10, f.ops(nil))
	if err := l.set(Limit{Key: "qemu-system-x86", Label: "qemu", Cores: []int{6, 7, 8, 9}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("limits file: %v %v", fi, err)
	}
	again := newLimiter(path, 10, f.ops(nil))
	if r := again.list(); len(r) != 1 || r[0].Key != "qemu-system-x86" || joinInts(r[0].Cores) != "6,7,8,9" {
		t.Fatalf("reloaded = %+v", r)
	}
}

func TestLimitValidation(t *testing.T) {
	l := newLimiter(filepath.Join(t.TempDir(), "l.json"), 10, (&fakeOps{}).ops(nil))
	for _, r := range []Limit{
		{Key: "", Label: "x"},
		{Key: "k", Cores: []int{10}},
		{Key: "k", Cores: []int{-1}},
		{Key: "k", Cores: []int{1, 1}},
		{Key: "k", Nice: 5},
		{Key: "k\x00", Nice: 10},
		{Key: "k"}, // no limit at all
	} {
		if err := l.set(r); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
	if err := l.set(Limit{Key: "k", Cores: []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}}); err == nil {
		t.Error("all cores is not a limit")
	}
}

// The real syscalls, on a child we own: affinity, nice, disk priority, pause and resume.
func TestRealProcOpsOnOwnChild(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep binary")
	}
	defer cmd.Process.Kill()
	pid := cmd.Process.Pid
	l := newLimiter(filepath.Join(t.TempDir(), "l.json"), runtime.NumCPU(), realProcOps())
	if err := l.set(Limit{Key: "k", Cores: []int{0}, Nice: 10, IOIdle: true, Paused: true}); err != nil {
		t.Fatal(err)
	}
	l.apply(map[string][]int{"k": {pid}})
	status := func() string { b, _ := os.ReadFile("/proc/" + itoa(pid) + "/status"); return string(b) }
	settle := func(stopped bool) { // signals are delivered asynchronously
		for i := 0; i < 100 && strings.Contains(status(), "State:\tT") != stopped; i++ {
			time.Sleep(5 * time.Millisecond)
		}
	}
	settle(true)
	if !strings.Contains(status(), "Cpus_allowed_list:\t0\n") {
		t.Fatalf("affinity not applied:\n%s", status())
	}
	if !strings.Contains(status(), "State:\tT") {
		t.Fatalf("not stopped:\n%s", status())
	}
	if n, _ := realProcOps().nice(pid); n != 10 {
		t.Fatalf("nice = %d", n)
	}
	if err := l.remove("k"); err != nil {
		t.Fatal(err)
	}
	l.apply(map[string][]int{"k": {pid}})
	settle(false)
	if s := status(); strings.Contains(s, "State:\tT") || strings.Contains(s, "Cpus_allowed_list:\t0\n") {
		t.Fatalf("not reverted:\n%s", s)
	}
}

// Job control: stop the pane's shell before its children and continue it last, or zsh reaps the
// stop, prints "suspended" and the resumed agent is left in the background (SIGTTIN).
func TestLimiterSignalsParentsFirstOnStopChildrenFirstOnContinue(t *testing.T) {
	f := &fakeOps{}
	l := newLimiter(filepath.Join(t.TempDir(), "l.json"), 10, f.ops(nil))
	l.set(Limit{Key: "pane:dev-1:3", Label: "p", Paused: true})
	group := map[string][]int{"pane:dev-1:3": {10, 20, 30}} // pidsByKey order: root first
	l.apply(group)
	var sigs []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "sig ") {
			sigs = append(sigs, c)
		}
	}
	if strings.Join(sigs, "|") != "sig 10 STOP|sig 20 STOP|sig 30 STOP" {
		t.Fatalf("stop order = %v", sigs)
	}
	f.calls = nil
	l.remove("pane:dev-1:3")
	l.apply(group)
	if strings.Join(f.calls, "|") != "sig 30 CONT|sig 20 CONT|sig 10 CONT" {
		t.Fatalf("continue order = %v", f.calls)
	}
}

// Edits inside one enforcer cycle must not lose a pending revert.
func TestLimiterUndoMerges(t *testing.T) {
	f := &fakeOps{}
	l := newLimiter(filepath.Join(t.TempDir(), "l.json"), 10, f.ops(nil))
	l.set(Limit{Key: "k", Label: "k", Cores: []int{0}, Paused: true})
	l.apply(map[string][]int{"k": {5}})
	f.calls = nil
	l.set(Limit{Key: "k", Label: "k", Cores: []int{0}})    // resume
	l.set(Limit{Key: "k", Label: "k", Cores: []int{0, 1}}) // new cores before the enforcer ran
	l.apply(map[string][]int{"k": {5}})
	if !strings.Contains(strings.Join(f.calls, "|"), "sig 5 CONT") {
		t.Fatalf("resume lost: %v", f.calls)
	}
}
