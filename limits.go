package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unsafe"
)

// The balancer. This VM gives uid 1000 no cgroups, so hard CPU/RAM caps are impossible. What
// works without root: which cores a group may run on (affinity, reversible), lower priority (nice,
// one-way: RLIMIT_NICE is 0), idle disk priority (reversible), and pause/resume (SIGSTOP/SIGCONT).
// Rules are keyed by process-group key and re-applied every 10 s, so new processes in the group
// get them too. Affinity, nice and ioprio are per thread on Linux: every thread gets them.
//
// The CI reserve is a rule on "*": every group without cores of its own, CI runners aside, runs on its cores while
// a CI job runs, so the cores it leaves out are CI's; once CI is idle everything gets them back.
const restKey = "*"

type Limit struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Cores  []int  `json:"cores,omitempty"` // allowed CPUs; empty: all
	Nice   int    `json:"nice,omitempty"`  // 0, 10 or 19
	IOIdle bool   `json:"ioIdle,omitempty"`
	Paused bool   `json:"paused,omitempty"`
}

type procOps struct {
	tids     func(pid int) []int
	affinity func(tid int, cores []int) error
	nice     func(tid int) (int, error)
	setNice  func(tid, n int) error
	ioprio   func(tid int, idle bool) error
	signal   func(pid int, s syscall.Signal) error
	guard    func(pid int) (touch, pause bool)
	comm     func(pid int) string
}

type limiter struct {
	path  string
	ncpu  int
	ops   procOps
	mu    sync.Mutex
	rules map[string]Limit
	undo  map[string]Limit // aspects dropped by edits/removal, reverted on the next apply (only the enforcer makes syscalls)
	kick  chan struct{}
	held  bool // the CI reserve is on the processes it covers (only the enforcer touches it)
	smu   sync.Mutex
	sys   *sysSampler
}

func newLimiter(path string, ncpu int, ops procOps) *limiter {
	l := &limiter{path: path, ncpu: ncpu, ops: ops, rules: map[string]Limit{}, undo: map[string]Limit{}, kick: make(chan struct{}, 1), held: true}
	if b, err := os.ReadFile(path); err == nil {
		var rs []Limit
		if json.Unmarshal(b, &rs) == nil {
			for _, r := range rs {
				if l.valid(&r) == nil {
					l.rules[r.Key] = r
				}
			}
		}
	}
	return l
}

func joinInts(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ",")
}

func (l *limiter) valid(r *Limit) error {
	if r.Key == "" || len(r.Key) > 200 || strings.IndexFunc(r.Key, unicode.IsControl) >= 0 {
		return errors.New("bad group")
	}
	r.Label = trunc(strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return -1
		}
		return c
	}, r.Label), 120)
	seen := map[int]bool{}
	for _, c := range r.Cores {
		if c < 0 || c >= l.ncpu || seen[c] {
			return fmt.Errorf("cores must be distinct numbers from 0 to %d", l.ncpu-1)
		}
		seen[c] = true
	}
	if len(r.Cores) == l.ncpu {
		r.Cores = nil
	}
	sort.Ints(r.Cores)
	if r.Nice != 0 && r.Nice != 10 && r.Nice != 19 {
		return errors.New("priority must be normal (0), low (10) or lowest (19)")
	}
	if r.Key == restKey && (r.Nice != 0 || r.IOIdle || r.Paused) {
		return errors.New("the CI reserve only takes cores")
	}
	if len(r.Cores) == 0 && r.Nice == 0 && !r.IOIdle && !r.Paused {
		return errors.New("that limits nothing: remove the limit instead")
	}
	return nil
}

func (l *limiter) save() error {
	rs := l.listLocked()
	b, _ := json.MarshalIndent(rs, "", "  ")
	os.MkdirAll(filepath.Dir(l.path), 0o700)
	return writeFileAtomic(l.path, b, 0o600)
}

// set adds or replaces a group's rule; what the edit dropped is reverted on the next apply.
func (l *limiter) set(r Limit) error {
	if err := l.valid(&r); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if old, ok := l.rules[r.Key]; ok {
		l.addUndo(old, r)
	}
	l.rules[r.Key] = r
	l.wake()
	return l.save()
}

// addUndo merges what going from old to r drops into the pending revert for the key.
func (l *limiter) addUndo(old, r Limit) {
	u := l.undo[old.Key]
	u.Key = old.Key
	if len(old.Cores) > 0 && len(r.Cores) == 0 {
		u.Cores = []int{-1} // marker: back to all cores
	}
	u.IOIdle = u.IOIdle || (old.IOIdle && !r.IOIdle)
	u.Paused = u.Paused || (old.Paused && !r.Paused)
	l.undo[old.Key] = u
}

func (l *limiter) wake() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

func (l *limiter) listLocked() []Limit {
	rs := make([]Limit, 0, len(l.rules))
	for _, r := range l.rules {
		rs = append(rs, r)
	}
	sort.Slice(rs, func(i, j int) bool {
		return rs[i].Label < rs[j].Label || (rs[i].Label == rs[j].Label && rs[i].Key < rs[j].Key)
	})
	return rs
}

func (l *limiter) list() []Limit {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.listLocked()
}

func (l *limiter) allCores() []int {
	all := make([]int, l.ncpu)
	for i := range all {
		all[i] = i
	}
	return all
}

// revert undoes the reversible parts of r (affinity, disk priority, pause) on pids (ancestors first).
// Nice stays. SIGCONT goes children first, so a pane's shell never sees its job stopped.
func (l *limiter) revert(r Limit, pids []int) {
	touched := pids[:0:0]
	for _, pid := range pids {
		if touch, _ := l.ops.guard(pid); !touch {
			continue
		}
		touched = append(touched, pid)
		for _, tid := range l.ops.tids(pid) {
			if len(r.Cores) > 0 {
				l.ops.affinity(tid, l.allCores())
			}
			if r.IOIdle {
				l.ops.ioprio(tid, false)
			}
		}
	}
	for i := len(touched) - 1; r.Paused && i >= 0; i-- {
		l.ops.signal(touched[i], syscall.SIGCONT)
	}
}

// remove drops a rule; the enforcer reverts it on its next pass (woken now).
func (l *limiter) remove(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.rules[key]
	if !ok {
		return errors.New("no limit on that group")
	}
	delete(l.rules, key)
	l.addUndo(r, Limit{Key: key})
	l.wake()
	return l.save()
}

// apply enforces every rule on the pids of its group (group key → pids, from a fresh sample).
func (l *limiter) apply(pids map[string][]int) {
	l.mu.Lock()
	rules, undo := l.listLocked(), l.undo
	l.undo = map[string]Limit{}
	l.mu.Unlock()
	busy := l.reserve(rules, undo, pids)
	for _, u := range undo {
		l.revert(u, pids[u.Key])
	}
	for _, r := range rules {
		if r.Key == restKey {
			if !busy { // CI is idle: the kept cores are everyone's again
				if l.held {
					l.revert(r, pids[restKey])
					l.held = false
				}
				continue
			}
			l.held = true
		}
		for _, pid := range pids[r.Key] {
			touch, pause := l.ops.guard(pid)
			if !touch {
				continue
			}
			for _, tid := range l.ops.tids(pid) {
				if len(r.Cores) > 0 {
					l.ops.affinity(tid, r.Cores)
				}
				if cur, err := l.ops.nice(tid); err == nil && cur < r.Nice {
					l.ops.setNice(tid, r.Nice)
				}
				if r.IOIdle {
					l.ops.ioprio(tid, true)
				}
			}
			if r.Paused && pause {
				l.ops.signal(pid, syscall.SIGSTOP)
			}
		}
	}
}

// reserve puts the pids the CI reserve covers under "*" and says whether a CI job is running. While one is, the
// runners get every core, in case one inherited fewer from the shell that started it.
func (l *limiter) reserve(rules []Limit, undo map[string]Limit, pids map[string][]int) (busy bool) {
	own, on := map[string]bool{}, false
	for _, r := range rules {
		own[r.Key] = len(r.Cores) > 0
		on = on || r.Key == restKey
	}
	if _, off := undo[restKey]; !on && !off {
		return false
	}
	var rest, runners []int
	for k, ps := range pids {
		switch {
		case strings.HasPrefix(k, "runner:"):
			for _, pid := range ps {
				busy = busy || l.ops.comm(pid) == "Runner.Worker"
			}
			if !own[k] {
				runners = append(runners, ps...)
			}
		case !own[k] && k != restKey:
			rest = append(rest, ps...)
		}
	}
	pids[restKey] = rest
	for _, pid := range runners {
		if touch, _ := l.ops.guard(pid); !on || !busy || !touch {
			continue
		}
		for _, tid := range l.ops.tids(pid) {
			l.ops.affinity(tid, l.allCores())
		}
	}
	return busy
}

// pidsNow samples /proc with the limiter's own sampler (the page sampler only runs while pages are open).
func (l *limiter) pidsNow() map[string][]int {
	l.smu.Lock()
	defer l.smu.Unlock()
	if l.sys == nil {
		l.sys = newSysSampler("/proc")
	}
	l.sys.sample(time.Now(), sockScan{})
	return l.sys.pidsByKey()
}

// run re-applies the rules every 10 s (and right after an edit); idle while there are none.
func (l *limiter) run() {
	t := time.NewTicker(10 * time.Second)
	for {
		select {
		case <-t.C:
		case <-l.kick:
		}
		l.mu.Lock()
		idle := len(l.rules) == 0 && len(l.undo) == 0
		l.mu.Unlock()
		if !idle {
			l.apply(l.pidsNow())
		}
	}
}

// ---- the real syscalls ----

func realProcOps() procOps {
	return procOps{
		tids: func(pid int) []int {
			ents, _ := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/task")
			var out []int
			for _, e := range ents {
				if n, err := strconv.Atoi(e.Name()); err == nil {
					out = append(out, n)
				}
			}
			return out
		},
		affinity: func(tid int, cores []int) error {
			var mask [16]uint64 // 1024 CPUs
			for _, c := range cores {
				mask[c/64] |= 1 << (c % 64)
			}
			_, _, e := syscall.RawSyscall(syscall.SYS_SCHED_SETAFFINITY, uintptr(tid), unsafe.Sizeof(mask), uintptr(unsafe.Pointer(&mask[0])))
			if e != 0 {
				return e
			}
			return nil
		},
		nice: func(tid int) (int, error) {
			p, err := syscall.Getpriority(syscall.PRIO_PROCESS, tid) // raw kernel value: 20 - nice
			return 20 - p, err
		},
		setNice: func(tid, n int) error { return syscall.Setpriority(syscall.PRIO_PROCESS, tid, n) },
		ioprio: func(tid int, idle bool) error {
			class, level := 2, 4 // best-effort, default level
			if idle {
				class, level = 3, 0
			}
			_, _, e := syscall.Syscall(syscall.SYS_IOPRIO_SET, 1 /* IOPRIO_WHO_PROCESS */, uintptr(tid), uintptr(class<<13|level))
			if e != 0 {
				return e
			}
			return nil
		},
		signal: func(pid int, s syscall.Signal) error { return syscall.Kill(pid, s) },
		guard: func(pid int) (touch, pause bool) {
			st, ok := procStartOf(pid)
			uid := procUID(pid)
			if !ok || pid <= 1 || uid != os.Getuid() || pid == os.Getpid() || st.Comm == "cloudflared" {
				return false, false
			}
			return true, killGuard(pid, st, uid, procCmdline(pid)) == nil
		},
		comm: func(pid int) string { st, _ := procStartOf(pid); return st.Comm },
	}
}

// summary says what a rule does, for the audit log ("cores 0,1,2, low priority, paused").
func (r Limit) summary() string {
	var parts []string
	switch len(r.Cores) {
	case 0:
	case 1:
		parts = append(parts, "core "+strconv.Itoa(r.Cores[0]))
	default:
		parts = append(parts, "cores "+joinInts(r.Cores))
	}
	switch r.Nice {
	case 10:
		parts = append(parts, "low priority")
	case 19:
		parts = append(parts, "lowest priority")
	}
	if r.IOIdle {
		parts = append(parts, "disk idle")
	}
	if r.Paused {
		parts = append(parts, "paused")
	}
	return strings.Join(parts, ", ")
}
