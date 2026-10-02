package main

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseStatCommWithSpacesAndParens(t *testing.T) {
	line := "4242 (a) b) S 17 4242 17 0 -1 4194304 81 0 0 0 150 50 0 0 20 0 1 0 42006683 2695168 306 18446744073709551615 0"
	st, ok := parseStat([]byte(line))
	if !ok {
		t.Fatal("parse failed")
	}
	if st.PID != 4242 || st.Comm != "a) b" || st.State != "S" || st.PPID != 17 ||
		st.UTime != 150 || st.STime != 50 || st.Start != 42006683 || st.RSS != 306 {
		t.Fatalf("got %+v", st)
	}
	if _, ok := parseStat([]byte("garbage")); ok {
		t.Fatal("garbage parsed")
	}
}

const statA = "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 50 0 50 350 50 0 0 0 0 0\ncpu1 50 0 50 350 50 0 0 0 0 0\nintr 1\n"
const statB = "cpu  200 0 200 900 100 0 0 0 0 0\ncpu0 150 0 50 400 50 0 0 0 0 0\ncpu1 50 0 150 500 50 0 0 0 0 0\nintr 1\n"

func TestCPUPercentFromTwoSamples(t *testing.T) {
	a, b := parseCPU([]byte(statA)), parseCPU([]byte(statB))
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("want total+2 cores, got %d/%d", len(a), len(b))
	}
	// total: busy +200 of +400 → 50%; cpu0: +100/+150 → 66.7%; cpu1: +100/+250 → 40%
	for i, want := range []float64{50, 66.67, 40} {
		if got := cpuPct(a[i], b[i]); got < want-0.1 || got > want+0.1 {
			t.Errorf("cpu[%d] = %.2f, want %.2f", i, got, want)
		}
	}
	if cpuPct(a[0], a[0]) != 0 {
		t.Error("zero delta should be 0%")
	}
}

func TestParseMeminfoConvertsKB(t *testing.T) {
	m := parseMeminfo([]byte("MemTotal:       32867136 kB\nMemAvailable:    5427504 kB\nHugePages_Total:       0\n"))
	if m["MemTotal"] != 32867136*1024 || m["MemAvailable"] != 5427504*1024 || m["HugePages_Total"] != 0 {
		t.Fatalf("got %v", m)
	}
}

const netDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 27128280655 27783394    0    0    0     0          0         0 27128280655 27783394    0    0    0     0       0          0
  eth0: 1000 18620126    0    0    0     0          0         0 500 22029183    0    0    0     0       0          0
  eth1: 10 1    0    0    0     0          0         0 5 1    0    0    0     0       0          0
`

func TestParseNetDevSkipsLoopback(t *testing.T) {
	rx, tx := parseNetDev([]byte(netDev))
	if rx != 1010 || tx != 505 {
		t.Fatalf("rx=%d tx=%d", rx, tx)
	}
}

const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:5606 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 8203 1 0000000065e654d5 100 0 0 10 0
   1: 0100007F:1E61 0100007F:A2C4 01 00000000:00000000 00:00000000 00000000  1000        0 7669595 1 00000000e56e3c3f 100 0 0 10 0
`
const procNetTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: AF00AAFD7B0A5134767FEE07020006E1:0016 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 8205 1 00000000c6ec0ca8 100 0 0 10 0
`

func TestParseProcNet(t *testing.T) {
	e := parseProcNet([]byte(procNetTCP), "tcp")
	if len(e) != 2 {
		t.Fatalf("got %d entries", len(e))
	}
	if e[0].Local != netip.MustParseAddrPort("0.0.0.0:22022") || e[0].State != "0A" || e[0].Inode != "8203" || e[0].UID != 0 {
		t.Fatalf("entry 0 = %+v", e[0])
	}
	if e[1].Local != netip.MustParseAddrPort("127.0.0.1:7777") || e[1].Remote.Port() != 0xA2C4 || e[1].State != "01" || e[1].UID != 1000 {
		t.Fatalf("entry 1 = %+v", e[1])
	}
	e6 := parseProcNet([]byte(procNetTCP6), "tcp6")
	if len(e6) != 1 || e6[0].Local.Addr().String() != "fdaa:af:3451:a7b:7ee:7f76:e106:2" || e6[0].Local.Port() != 22 {
		t.Fatalf("tcp6 = %+v", e6)
	}
}

func TestGroupKey(t *testing.T) {
	pane := "ZELLIJ_SESSION_NAME=dev-1\x00ZELLIJ_PANE_ID=10\x00"
	cases := []struct {
		name                  string
		cmdline, environ      string
		comm, parentComm      string
		ppid                  int
		parentKey, key, label string
	}{
		{"runner by path wins over pane env", "/workspace/home/projects/ci-cd-jobs/fe2/bin/Runner.Listener\x00run\x00", pane, "Runner.Listener", "bash", 1, "", "runner:fe2", "CI runner fe2"},
		{"job step by RUNNER_NAME", "python\x00-m\x00pytest\x00", "HOME=/x\x00RUNNER_NAME=cube-r4\x00", "python", "bash", 99, "bash", "runner:r4", "CI runner r4"},
		{"pane root shell", "zsh\x00", pane, "zsh", "zellij", 5, "zellij", "pane:dev-1:10", "dev-1 · pane 10"},
		{"descendant inherits parent group", "claude\x00", pane, "claude", "zsh", 7, "pane:dev-1:10", "pane:dev-1:10", "dev-1 · pane 10"},
		{"daemonized from a pane groups by name", "/usr/bin/qemu-system-x86_64\x00", pane, "qemu-system-x86", "init", 1, "init", "qemu-system-x86", "qemu-system-x86"},
		{"root interpreter labelled by script", "node\x00/workspace/home/.local/bin/teamclaude\x00server\x00", "", "node", "init", 1, "init", "node ~/.local/bin/teamclaude", "node ~/.local/bin/teamclaude"},
		{"root bash -c labelled by command", "bash\x00-c\x00cd r1 && exec ./run.sh\x00", "", "bash", "init", 1, "init", "bash -c cd r1 && exec ./run.sh", "bash -c cd r1 && exec ./run.sh"},
		{"root python flags skipped", "python3\x00-u\x00-m\x00http.server\x00", "", "python3", "init", 1, "init", "python3 -m http.server", "python3 -m http.server"},
		{"kernel thread", "", "", "kworker/0:1", "kthreadd", 2, "kthreadd", "kworker/0:1", "kworker/0:1"},
	}
	for _, c := range cases {
		parentLabel := c.parentKey
		if c.parentKey == "pane:dev-1:10" {
			parentLabel = "dev-1 · pane 10"
		}
		k, l := groupKey([]byte(c.cmdline), []byte(c.environ), c.comm, c.parentComm, c.ppid, c.parentKey, parentLabel, "")
		if k != c.key || l != c.label {
			t.Errorf("%s: got %q,%q want %q,%q", c.name, k, l, c.key, c.label)
		}
	}
}

func TestSampleGroupsByAncestry(t *testing.T) {
	root := fakeProc(t)
	pane := "ZELLIJ_SESSION_NAME=dev-1\x00ZELLIJ_PANE_ID=4\x00"
	proc := func(pid, ppid int, comm, cmd, env string) {
		d := fmt.Sprintf("%s/%d/", root, pid)
		writeFile(t, d+"stat", fmt.Sprintf("%d (%s) S %d 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 10 0", pid, comm, ppid, 100+pid))
		writeFile(t, d+"cmdline", cmd)
		writeFile(t, d+"environ", env)
	}
	os.RemoveAll(root + "/123")
	proc(900, 950, "python", "python\x00x.py\x00", pane) // child listed before its parent (pid wrap)
	proc(10, 1, "zellij", "zellij\x00--server\x00", "")
	proc(11, 10, "zsh", "zsh\x00", pane)
	proc(950, 11, "claude", "claude\x00", pane)
	proc(20, 1, "qemu-system-x86", "qemu\x00", pane)
	r := newSysSampler(root).sample(time.Unix(1000, 0), sockScan{})
	got := map[int]string{}
	for _, g := range r.Groups {
		for _, p := range g.Procs {
			got[p.PID] = g.Key
		}
	}
	want := map[int]string{900: "pane:dev-1:4", 11: "pane:dev-1:4", 950: "pane:dev-1:4", 10: "zellij", 20: "qemu-system-x86"}
	for pid, k := range want {
		if got[pid] != k {
			t.Errorf("pid %d in %q, want %q", pid, got[pid], k)
		}
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fakeProc(t *testing.T) string {
	root := t.TempDir()
	writeFile(t, root+"/stat", statA)
	writeFile(t, root+"/meminfo", "MemTotal: 1000 kB\nMemAvailable: 250 kB\nSwapTotal: 100 kB\nSwapFree: 100 kB\n")
	writeFile(t, root+"/loadavg", "6.38 7.55 6.82 3/1200 999\n")
	writeFile(t, root+"/uptime", "409680.12 100.0\n")
	writeFile(t, root+"/net/dev", netDev)
	writeFile(t, root+"/mounts", "")
	writeFile(t, root+"/123/stat", "123 (claude) S 1 123 1 0 -1 0 0 0 0 0 100 0 0 0 20 0 1 0 500 0 1000 0")
	writeFile(t, root+"/123/cmdline", "claude\x00")
	writeFile(t, root+"/123/environ", "ZELLIJ_SESSION_NAME=dev-1\x00ZELLIJ_PANE_ID=4\x00")
	writeFile(t, root+"/123/io", "read_bytes: 4096\nwrite_bytes: 0\n")
	os.MkdirAll(root+"/456", 0o755) // process that vanished mid-scan: dir without stat
	return root
}

func TestSampleSurvivesVanishedProcessAndComputesRates(t *testing.T) {
	root := fakeProc(t)
	s := newSysSampler(root)
	t0 := time.Unix(1000, 0)
	s.sample(t0, sockScan{})
	writeFile(t, root+"/stat", statB)
	writeFile(t, root+"/123/stat", "123 (claude) S 1 123 1 0 -1 0 0 0 0 0 300 0 0 0 20 0 1 0 500 0 1000 0")
	writeFile(t, root+"/123/io", "read_bytes: 8192\nwrite_bytes: 0\n")
	writeFile(t, root+"/net/dev", `x
x
  eth0: 3000 0 0 0 0 0 0 0 1500 0 0 0 0 0 0 0
`)
	r := s.sample(t0.Add(2*time.Second), sockScan{tcpByPID: map[int]int{123: 3}})
	if r.CPU < 49 || r.CPU > 51 || len(r.Cores) != 2 {
		t.Fatalf("cpu=%v cores=%v", r.CPU, r.Cores)
	}
	if r.MemTotal != 1000*1024 || r.MemAvail != 250*1024 || r.Load[0] != 6.38 {
		t.Fatalf("mem/load = %d %d %v", r.MemTotal, r.MemAvail, r.Load)
	}
	if r.NetRx != (3000-1010)/2.0 || r.NetTx != (1500-505)/2.0 {
		t.Fatalf("net = %v/%v", r.NetRx, r.NetTx)
	}
	if len(r.Groups) != 1 {
		t.Fatalf("groups = %+v", r.Groups)
	}
	g := r.Groups[0]
	// 200 ticks over 2 s at CLK_TCK 100 → 100% of one core
	if g.Key != "claude" || g.CPU < 99 || g.CPU > 101 || g.Rd != 2048 || g.Conns != 3 || g.RSS != 1000*uint64(os.Getpagesize()) {
		t.Fatalf("group = %+v", g)
	}
	if len(g.Procs) != 1 || g.Procs[0].PID != 123 || g.Procs[0].Cmd != "claude" {
		t.Fatalf("procs = %+v", g.Procs)
	}
}

func TestHistoryRing(t *testing.T) {
	root := fakeProc(t)
	h := newHistory(root, 3)
	for i := 0; i < 5; i++ {
		h.tick(time.Unix(int64(1000+10*i), 0))
	}
	pts := h.points()
	if len(pts) != 3 || pts[0].T != 1020 || pts[2].T != 1040 {
		t.Fatalf("points = %+v", pts)
	}
}

func TestSampleRealProc(t *testing.T) {
	s := newSysSampler("/proc")
	s.sample(time.Now(), sockScan{})
	time.Sleep(300 * time.Millisecond)
	r := s.sample(time.Now(), scanSockets("/proc"))
	if r.NProcs < 10 || len(r.Groups) == 0 || r.MemTotal == 0 || len(r.Disks) == 0 || len(r.Cores) == 0 {
		t.Fatalf("implausible sample: nprocs=%d groups=%d mem=%d disks=%v cores=%d", r.NProcs, len(r.Groups), r.MemTotal, r.Disks, len(r.Cores))
	}
	for _, g := range r.Groups[:min(5, len(r.Groups))] {
		t.Logf("%-28s cpu=%6.1f%% rss=%5dMB n=%d conns=%d", g.Label, g.CPU, g.RSS>>20, g.N, g.Conns)
	}
	t.Logf("disks=%+v", r.Disks)
}

func TestZellijSessionsMatchesSocketMtimeToServerStart(t *testing.T) {
	dir := t.TempDir()
	btime := int64(1_790_000_000)
	mk := func(name string, at int64) {
		p := filepath.Join(dir, name)
		writeFile(t, p, "")
		os.Chtimes(p, time.Unix(at, 0), time.Unix(at, 0))
	}
	mk("dev-1", btime+5000)   // renamed session: server cmdline still says implacable-pheasant
	mk("box-dev", btime+9000) // started 1 s after its server
	mk("stale", btime+100)    // socket left behind by a crashed server
	stats := map[int]procStat{
		4632:  {PID: 4632, PPID: 1, Comm: "zellij", Start: 5000 * clkTck},
		11747: {PID: 11747, PPID: 1, Comm: "zellij", Start: 8999 * clkTck},
		12:    {PID: 12, PPID: 4632, Comm: "zellij", Start: 100 * clkTck}, // a client, not a server
	}
	got := zellijSessions(stats, btime, dir)
	if len(got) != 2 || got[4632] != "dev-1" || got[11747] != "box-dev" {
		t.Fatalf("got %v", got)
	}
}

func TestSamplePaneKeyUsesCurrentSessionName(t *testing.T) {
	root := fakeProc(t)
	sock := t.TempDir()
	writeFile(t, root+"/stat", statA+"btime 1790000000\n")
	os.RemoveAll(root + "/123")
	writeFile(t, root+"/10/stat", "10 (zellij) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 500000 0 10 0")
	writeFile(t, root+"/11/stat", "11 (zsh) S 10 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 500100 0 10 0")
	writeFile(t, root+"/11/environ", "ZELLIJ_SESSION_NAME=old-name\x00ZELLIJ_PANE_ID=7\x00")
	writeFile(t, sock+"/new-name", "")
	os.Chtimes(sock+"/new-name", time.Unix(1790000000+5000, 0), time.Unix(1790000000+5000, 0))
	s := newSysSampler(root)
	s.sockDir = sock
	r := s.sample(time.Unix(1000, 0), sockScan{})
	keys := map[string]bool{}
	for _, g := range r.Groups {
		keys[g.Key] = true
	}
	if !keys["pane:new-name:7"] {
		t.Fatalf("groups = %v, want pane:new-name:7", keys)
	}
	if live := s.liveSessions(); len(live) != 1 || live[0] != "new-name" {
		t.Fatalf("liveSessions = %v", live)
	}
}

func TestSampleKeepsAllGroupsForJoinButSendsTop40(t *testing.T) {
	root := fakeProc(t)
	for i := 0; i < 45; i++ {
		pid := 2000 + i
		writeFile(t, fmt.Sprintf("%s/%d/stat", root, pid), fmt.Sprintf("%d (p%d) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 10 0", pid, i, pid))
	}
	r := newSysSampler(root).sample(time.Unix(1000, 0), sockScan{})
	if len(r.Groups) != maxGroups || len(r.all) != 46 {
		t.Fatalf("Groups=%d all=%d", len(r.Groups), len(r.all))
	}
}

func TestExecRefreshesCachedCommand(t *testing.T) {
	root := fakeProc(t)
	s := newSysSampler(root)
	s.sample(time.Unix(1000, 0), sockScan{})
	// same pid and start time, new program: fork then exec
	writeFile(t, root+"/123/stat", "123 (python) S 1 123 1 0 -1 0 0 0 0 0 100 0 0 0 20 0 1 0 500 0 1000 0")
	writeFile(t, root+"/123/cmdline", "python\x00job.py\x00")
	r := s.sample(time.Unix(1002, 0), sockScan{})
	for _, g := range r.Groups {
		for _, p := range g.Procs {
			if p.PID == 123 && p.Cmd != "python job.py" {
				t.Fatalf("cmd = %q after exec", p.Cmd)
			}
		}
	}
}

func TestSamplePaneInfo(t *testing.T) {
	root := fakeProc(t)
	os.RemoveAll(root + "/123")
	pane := "ZELLIJ_SESSION_NAME=dev-1\x00ZELLIJ_PANE_ID=4\x00"
	proc := func(pid, ppid, pgrp, tpgid int, state, comm, cmd string) {
		d := fmt.Sprintf("%s/%d/", root, pid)
		writeFile(t, d+"stat", fmt.Sprintf("%d (%s) %s %d %d 1 34816 %d 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 10 0", pid, comm, state, ppid, pgrp, tpgid, 100+pid))
		writeFile(t, d+"cmdline", cmd)
		writeFile(t, d+"environ", pane)
	}
	proc(10, 1, 10, -1, "S", "zellij", "zellij\x00--server\x00")
	proc(11, 10, 11, 950, "S", "zsh", "zsh\x00-l\x00")
	proc(950, 11, 950, 950, "R", "python3", "python3\x00train.py\x00")
	proc(951, 950, 950, 950, "S", "sleep", "sleep\x001\x00")
	proc(12, 10, 12, 12, "S", "zsh", "zsh\x00")
	writeFile(t, root+"/12/environ", "ZELLIJ_SESSION_NAME=dev-1\x00ZELLIJ_PANE_ID=5\x00")
	tty := filepath.Join(t.TempDir(), "pts4")
	writeFile(t, tty, "")
	os.Chtimes(tty, time.Unix(1790000123, 0), time.Unix(1790000123, 0))
	os.MkdirAll(root+"/11/fd", 0o755)
	os.Symlink(tty, root+"/11/fd/0")

	s := newSysSampler(root)
	r := s.sample(time.Unix(1000, 0), sockScan{})
	p := r.panes["pane:dev-1:4"]
	if p.Root != 11 || p.FgPID != 950 || p.FgComm != "python3" || p.FgState != "R" || p.FgCmd != "python3 train.py" || p.N != 3 || p.Output != 1790000123 {
		t.Fatalf("pane 4 = %+v", p)
	}
	if q := r.panes["pane:dev-1:5"]; q.Root != 12 || q.FgPID != 12 || q.N != 1 {
		t.Fatalf("pane 5 (shell at its prompt) = %+v", q)
	}
	if s.keyOf(951) != "pane:dev-1:4" || s.keyOf(10) != "zellij" {
		t.Fatalf("keyOf: %q %q", s.keyOf(951), s.keyOf(10))
	}
}

func TestPidsByKeyAncestorsFirst(t *testing.T) {
	root := fakeProc(t)
	os.RemoveAll(root + "/123")
	pane := "ZELLIJ_SESSION_NAME=dev-1\x00ZELLIJ_PANE_ID=4\x00"
	proc := func(pid, ppid int, comm string) {
		d := fmt.Sprintf("%s/%d/", root, pid)
		writeFile(t, d+"stat", fmt.Sprintf("%d (%s) S %d 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 10 0", pid, comm, ppid, 100+pid))
		writeFile(t, d+"environ", pane)
	}
	proc(10, 1, "zellij")
	proc(900, 950, "sleep") // grandchild, pid below its parent's (wrap)
	proc(950, 30, "claude")
	proc(30, 10, "zsh")
	s := newSysSampler(root)
	s.sample(time.Unix(1000, 0), sockScan{})
	if got := s.pidsByKey()["pane:dev-1:4"]; fmt.Sprint(got) != "[30 950 900]" {
		t.Fatalf("pids = %v, want root shell first", got)
	}
}
