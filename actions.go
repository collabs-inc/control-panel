package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// actor performs the allowlisted actions. Nothing here takes a free-form command:
// every argument is validated and passed as argv, never through a shell.
type actor struct {
	ciDir string
	run   func(dir, name string, args ...string) error
}

func itoa(n int) string { return strconv.Itoa(n) }

func procAlive(pid int) bool {
	st, ok := procStartOf(pid)
	return ok && st.State != "Z" && st.State != "X"
}

func tgid(pid int) int {
	b, _ := os.ReadFile("/proc/" + itoa(pid) + "/status")
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "Tgid:"); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return -1
}

func procUID(pid int) int {
	fi, err := os.Stat("/proc/" + itoa(pid))
	if err != nil {
		return -1
	}
	return int(fi.Sys().(*syscall.Stat_t).Uid)
}

func procCmdline(pid int) string {
	b, _ := os.ReadFile("/proc/" + itoa(pid) + "/cmdline")
	return strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
}

func ppidMap() map[int]int {
	m := map[int]int{}
	dirs, _ := os.ReadDir("/proc")
	for _, d := range dirs {
		if pid, err := strconv.Atoi(d.Name()); err == nil {
			if st, ok := procStartOf(pid); ok {
				m[pid] = st.PPID
			}
		}
	}
	return m
}

func descendants(root int, ppid map[int]int) []int {
	children := map[int][]int{}
	for pid, pp := range ppid {
		children[pp] = append(children[pp], pid)
	}
	var out []int
	queue := []int{root}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out
}

// killGuard refuses processes the panel must never kill.
func killGuard(pid int, st procStat, uid int, cmdline string) error {
	switch {
	case pid <= 1:
		return errors.New("refusing to kill PID 1")
	case uid != os.Getuid():
		return errors.New("process belongs to another user")
	case pid == os.Getpid():
		return errors.New("that's the control panel itself")
	case st.Comm == "cloudflared":
		return errors.New("that's the Cloudflare tunnel — the panel would go offline")
	case st.Comm == "zellij":
		return errors.New("that's zellij — kill the agent inside the pane instead")
	case strings.Contains(cmdline, "/workspace/.cube/") || strings.Contains(cmdline, "/opt/cube/"):
		return errors.New("that's part of Cube (your terminal/daemon)")
	}
	return nil
}

// kill sends SIGTERM, then SIGKILL after 5 s if the same process (same start time) is still there.
func (a *actor) kill(pid int) error {
	st, ok := procStartOf(pid)
	if !ok || st.State == "Z" {
		return fmt.Errorf("no process %d", pid)
	}
	// /proc/<tid> resolves for threads too, and kill(2) on a TID signals the whole process
	if tgid(pid) != pid {
		return fmt.Errorf("%d is a thread, not a process", pid)
	}
	if err := killGuard(pid, st, procUID(pid), procCmdline(pid)); err != nil {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	go func() {
		time.Sleep(5 * time.Second)
		if now, ok := procStartOf(pid); ok && now.Start == st.Start && now.State != "Z" {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}()
	return nil
}

// restartRunner stops one runner's process tree and lets ensure-runners.sh start it again.
func (a *actor) restartRunner(name string) error {
	if !runnerName.MatchString(name) {
		return fmt.Errorf("bad runner name %q", name)
	}
	dir := filepath.Join(a.ciDir, name)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("no runner %s", name)
	}
	pidFile := filepath.Join(dir, "runner.pid")
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if runnerAlive(pid, dir) {
		// the tree, never the process group: runners started from a login shell share its group.
		// Signal each pid only while it still has the start time we saw — pids wrap fast on this box.
		tree := map[int]uint64{}
		for _, p := range append([]int{pid}, descendants(pid, ppidMap())...) {
			if st, ok := procStartOf(p); ok {
				tree[p] = st.Start
			}
		}
		live := func() bool {
			for p, start := range tree {
				if sameProc(p, start) {
					return true
				}
			}
			return false
		}
		for p, start := range tree {
			if sameProc(p, start) {
				syscall.Kill(p, syscall.SIGTERM)
			}
		}
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && live(); {
			time.Sleep(250 * time.Millisecond)
		}
		for p, start := range tree {
			if sameProc(p, start) {
				syscall.Kill(p, syscall.SIGKILL)
			}
		}
	}
	// ensure-runners.sh starts a runner whose pid file doesn't name a live process;
	// a stale file pointing at a reused pid would make it skip this one
	os.Remove(pidFile)
	return a.run(a.ciDir, "./ensure-runners.sh")
}

// cleanEnv drops zellij and Claude session variables. The panel may have been started from inside an
// agent's pane; a zellij server, runner or `claude` it spawns must not think it's nested in that one.
func cleanEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "ZELLIJ") || strings.HasPrefix(k, "CLAUDE") || k == "AI_AGENT" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func runCmd(dir, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = cleanEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sameProc: pid still names the process that had this start time (not a reused pid).
func sameProc(pid int, start uint64) bool {
	st, ok := procStartOf(pid)
	return ok && st.Start == start && st.State != "Z" && st.State != "X"
}

// runnerAlive: pid is alive and really is the runner in dir (runner.pid can outlive it and be reused).
func runnerAlive(pid int, dir string) bool {
	return pid > 1 && procAlive(pid) && strings.Contains(procCmdline(pid), dir+"/")
}
