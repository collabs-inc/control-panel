package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Notifications: what changed that you'd want to know (an agent finished, needs your answer or looks
// stuck; a CI job failed; a security check went bad; a disk or memory filling up), kept across restarts.
// The server keeps looking with no page open (sampleLoop's slow pace), so the list holds what happened
// while you were away. A click on one takes the page to its pane or view (Go).

type Note struct {
	ID    int64  `json:"id"`
	T     int64  `json:"t"`
	Kind  string `json:"kind"`  // done, asking, stalled, ci, sec, disk, mem
	Level string `json:"level"` // ok, warn, bad
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Go    string `json:"go"` // pane:<session>:<id>, ci, sec, res
}

const maxNotes = 200

// paneSeen: an agent pane's state at the last look, and when Claude last changed its status
type paneSeen struct {
	state string
	since int64
}

type notifier struct {
	mu    sync.Mutex
	file  string
	Notes []Note `json:"notes"` // oldest first
	Seen  int64  `json:"seen"`  // read up to this id
	Next  int64  `json:"next"`
	// what the last look saw; not saved, so a restart learns the states again instead of announcing them
	primed bool
	panes  map[string]paneSeen
	jobs   map[string]bool   // completed CI jobs seen
	checks map[string]string // security check -> level
	full   map[string]bool   // a disk or memory past 90%, until it's back under 85%
}

func newNotifier(file string) *notifier {
	n := &notifier{file: file}
	if b, err := os.ReadFile(file); err == nil {
		json.Unmarshal(b, n)
	}
	return n
}

func (n *notifier) add(now time.Time, kind, level, title, body, goTo string) {
	n.Next++
	n.Notes = append(n.Notes, Note{ID: n.Next, T: now.Unix(), Kind: kind, Level: level, Title: trunc(title, 200), Body: trunc(body, 300), Go: goTo})
	if len(n.Notes) > maxNotes {
		n.Notes = n.Notes[len(n.Notes)-maxNotes:]
	}
}

// observe compares this look with the last and notes what changed. said gives an agent's last words
// (the start of its final message), read only when it finishes.
func (n *notifier) observe(now time.Time, tm Terminals, ci CI, sec Security, res Resources, said func(TPane) string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	before := n.Next
	panes, jobs, checks := map[string]paneSeen{}, map[string]bool{}, map[string]string{}

	for _, s := range tm.Sessions {
		for _, tb := range s.Tabs {
			for _, p := range tb.Panes {
				if p.Agent == nil {
					continue
				}
				key := s.Name + ":" + strconv.Itoa(p.ID)
				panes[key] = paneSeen{p.State, p.Since}
				prev, known := n.panes[key]
				was := prev.state
				if was == "waiting" && p.State == "waiting" && p.Since > prev.since {
					was = "working" // a whole turn fit between two looks: Claude's status moved on and came back
				}
				if !n.primed || !known || was == p.State {
					continue
				}
				name := tb.Name
				if len(tb.Panes) > 1 {
					name += " · " + cmp.Or(p.Title, "pane "+strconv.Itoa(p.ID))
				}
				switch {
				case p.State == "waiting" && (was == "working" || was == "stalled" || was == "background"):
					n.add(now, "done", "ok", name+" is done", said(p), "pane:"+key)
				case p.State == "asking":
					n.add(now, "asking", "warn", name+" needs your answer", p.Detail, "pane:"+key)
				case p.State == "stalled":
					n.add(now, "stalled", "warn", name+" looks stuck", p.Detail, "pane:"+key)
				}
			}
		}
	}

	for _, j := range ci.Recent {
		id := j.Runner + "@" + strconv.FormatInt(j.Start, 10)
		jobs[id] = true
		if !n.primed || n.jobs[id] || j.Result != "Failed" {
			continue
		}
		what := ""
		if m := j.Meta; m != nil {
			what = m.Repo
			if m.PR > 0 {
				what += " PR #" + strconv.Itoa(m.PR)
			} else if m.Branch != "" {
				what += " " + m.Branch
			}
			if m.Title != "" {
				what += " · " + m.Title
			}
		}
		n.add(now, "ci", "bad", "CI failed: "+j.Meta.workflowOr(j.Name), strings.TrimSpace(what), "ci")
	}

	for _, c := range sec.Checks {
		checks[c.Title] = c.Level
		if n.primed && c.Level == "bad" && n.checks[c.Title] != "bad" {
			n.add(now, "sec", "bad", "Security: "+c.Title, c.Detail, "sec")
		}
	}

	if n.full == nil {
		n.full = map[string]bool{}
	}
	over := func(key string, used, size uint64, kind, title string) {
		if size == 0 {
			return
		}
		pct := float64(used) / float64(size) * 100
		switch {
		case pct >= 90 && !n.full[key]:
			n.full[key] = true
			if n.primed { // full at start: learned, not announced again after every restart
				n.add(now, kind, "warn", fmt.Sprintf(title, int(math.Round(pct))), fmtBytes(int64(used))+" of "+fmtBytes(int64(size)), "res")
			}
		case pct < 85:
			delete(n.full, key)
		}
	}
	for _, d := range res.Disks {
		over("disk:"+d.Mount, d.Used, d.Size, "disk", d.Mount+" is %d%% full")
	}
	over("mem", res.MemTotal-min(res.MemAvail, res.MemTotal), res.MemTotal, "mem", "Memory is %d%% used")

	n.panes, n.jobs, n.checks, n.primed = panes, jobs, checks, true
	if n.Next != before {
		n.saveLocked()
	}
}

func (j *JobMeta) workflowOr(name string) string {
	if j == nil || j.Workflow == "" {
		return name
	}
	return j.Workflow + " / " + name
}

func (n *notifier) saveLocked() {
	b, err := json.Marshal(n)
	if err != nil || n.file == "" {
		return
	}
	writeFileAtomic(n.file, b, 0o600)
}

func (n *notifier) unread() int {
	c := 0
	for _, x := range n.Notes {
		if x.ID > n.Seen {
			c++
		}
	}
	return c
}

func (n *notifier) markSeen(id int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if id > n.Seen {
		n.Seen = min(id, n.Next)
		n.saveLocked()
	}
}

// badge: the newest id (the page fetches the list when it moves) and how many are unread.
func (n *notifier) badge() (latest int64, unread int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.Next, n.unread()
}

// lastSaid: the start of the agent's last message, for a "done" note.
func lastSaid(path string) string {
	ls, _, _ := readTail(path, 256<<10)
	for i := len(ls) - 1; i >= 0; i-- {
		var e tEntry
		if json.Unmarshal(ls[i], &e) != nil || e.Type != "assistant" || e.Sidechain || e.IsAPIError {
			continue
		}
		_, bs := blocks(e.Message.Content)
		for j := len(bs) - 1; j >= 0; j-- {
			if bs[j].Type == "text" && strings.TrimSpace(bs[j].Text) != "" {
				return trunc(strings.Join(strings.Fields(bs[j].Text), " "), 240)
			}
		}
	}
	return ""
}

func (s *server) notesList(w http.ResponseWriter, r *http.Request) {
	s.notes.mu.Lock()
	defer s.notes.mu.Unlock()
	out := make([]Note, len(s.notes.Notes))
	for i, x := range s.notes.Notes {
		out[len(out)-1-i] = x // newest first
	}
	writeJSON(w, 200, map[string]any{"notes": out, "seen": s.notes.Seen})
}

func (s *server) notesSeen(w http.ResponseWriter, r *http.Request) {
	var b struct{ ID int64 }
	if err := readJSON(r, &b); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	s.notes.markSeen(b.ID)
	writeJSON(w, 200, map[string]any{"ok": true})
}
