package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestKillGuard(t *testing.T) {
	me := os.Getuid()
	cases := []struct {
		name    string
		pid     int
		comm    string
		uid     int
		cmdline string
		ok      bool
	}{
		{"ordinary process", 5000, "python", me, "python x.py", true},
		{"init", 1, "init", me, "/sbin/init", false},
		{"foreign user", 5000, "sshd", 0, "sshd", false},
		{"panel itself", os.Getpid(), "control-panel", me, "control-panel", false},
		{"tunnel", 5000, "cloudflared", me, "cloudflared tunnel run", false},
		{"zellij", 5000, "zellij", me, "zellij --server x", false},
		{"cube daemon", 5000, "node", me, "/usr/local/bin/node /workspace/.cube/cubed/f318ca1fddae/index.js", false},
	}
	for _, c := range cases {
		err := killGuard(c.pid, procStat{PID: c.pid, Comm: c.comm}, c.uid, c.cmdline)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func startReaped(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait() // reap so the pid doesn't linger as a zombie
	t.Cleanup(func() { cmd.Process.Kill() })
	return cmd
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	for i := 0; i < 40; i++ {
		if !procAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive", pid)
}

func TestKillTerminatesProcess(t *testing.T) {
	cmd := startReaped(t, "sleep", "300")
	a := &actor{ciDir: t.TempDir()}
	if err := a.kill(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	waitGone(t, cmd.Process.Pid)
	if err := a.kill(cmd.Process.Pid); err == nil {
		t.Fatal("killing a gone pid should error")
	}
	if err := a.kill(os.Getpid()); err == nil {
		t.Fatal("panel killed itself")
	}
}

func TestRestartRunnerValidatesName(t *testing.T) {
	a := &actor{ciDir: t.TempDir(), run: func(string, string, ...string) error { t.Fatal("ran something"); return nil }}
	for _, n := range []string{"../r1", "r1;rm -rf /", "x1", "r1/../r2", "", "r", "fe"} {
		if err := a.restartRunner(n); err == nil {
			t.Errorf("restartRunner(%q) accepted", n)
		}
	}
	if err := a.restartRunner("r7"); err == nil {
		t.Error("restartRunner on missing dir accepted")
	}
}

func TestRestartRunnerKillsTreeAndRunsEnsure(t *testing.T) {
	ci := t.TempDir()
	os.MkdirAll(filepath.Join(ci, "r1"), 0o755)
	marker := filepath.Join(ci, "ensured")
	writeFile(t, filepath.Join(ci, "ensure-runners.sh"), "#!/bin/sh\ntouch "+marker+"\n")
	os.Chmod(filepath.Join(ci, "ensure-runners.sh"), 0o755)
	// a fake runner whose cmdline carries its dir, with a child process
	runner := startReaped(t, "bash", "-c", "sleep 300 & wait", filepath.Join(ci, "r1", "run.sh"))
	writeFile(t, filepath.Join(ci, "r1", "runner.pid"), itoa(runner.Process.Pid)+"\n")
	var child int
	for i := 0; i < 40 && child == 0; i++ {
		time.Sleep(25 * time.Millisecond)
		for _, p := range descendants(runner.Process.Pid, ppidMap()) {
			child = p
		}
	}
	if child == 0 {
		t.Fatal("fake runner child never appeared")
	}
	a := &actor{ciDir: ci, run: runCmd}
	if err := a.restartRunner("r1"); err != nil {
		t.Fatal(err)
	}
	waitGone(t, runner.Process.Pid)
	waitGone(t, child)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("ensure-runners.sh was not run")
	}
}

func TestRestartRunnerWithStalePIDRestartsWithoutKilling(t *testing.T) {
	ci := t.TempDir()
	os.MkdirAll(filepath.Join(ci, "r1"), 0o755)
	stranger := startReaped(t, "sleep", "300") // runner.pid now points at an unrelated process
	writeFile(t, filepath.Join(ci, "r1", "runner.pid"), itoa(stranger.Process.Pid))
	ran := false
	a := &actor{ciDir: ci, run: func(string, string, ...string) error {
		if _, err := os.Stat(filepath.Join(ci, "r1", "runner.pid")); err == nil {
			t.Error("stale runner.pid left in place: ensure-runners.sh would think the runner is alive")
		}
		ran = true
		return nil
	}}
	if err := a.restartRunner("r1"); err != nil || !ran {
		t.Fatalf("err=%v ran=%v", err, ran)
	}
	if !procAlive(stranger.Process.Pid) {
		t.Fatal("unrelated process was killed")
	}
}

func TestDescendants(t *testing.T) {
	ppid := map[int]int{2: 1, 3: 2, 4: 3, 5: 2, 6: 1, 7: 6}
	got := descendants(2, ppid)
	want := map[int]bool{3: true, 4: true, 5: true}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for _, p := range got {
		if !want[p] {
			t.Fatalf("got %v", got)
		}
	}
}

func TestCleanEnvStripsAgentAndZellijVars(t *testing.T) {
	got := cleanEnv([]string{"HOME=/h", "ZELLIJ=0", "ZELLIJ_SESSION_NAME=box", "CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=x", "AI_AGENT=1", "PATH=/bin", "TERM=xterm"})
	if want := []string{"HOME=/h", "PATH=/bin", "TERM=xterm"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanEnv = %v", got)
	}
}
