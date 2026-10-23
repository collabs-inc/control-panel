package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Token use: what Claude's agents spent, read from their transcripts (main and subagent ones) under
// ~/.claude/projects. Each assistant message counts once, in the hour it was written: a message written as
// several entries (one per content block) carries its usage on each, and the last one stands. The scan is
// incremental: each file is read on from where the last scan stopped, and what it learned is kept in
// tokens.json, so a restart doesn't read 2 GB of transcripts again.

const tokKeep = 31 * 24 * time.Hour

type tokUse struct {
	In  int64 `json:"i,omitempty"`
	Out int64 `json:"o,omitempty"`
	CW  int64 `json:"w,omitempty"` // written to the prompt cache
	CR  int64 `json:"r,omitempty"` // read from it
}

func (u tokUse) add(v tokUse, sign int64) tokUse {
	return tokUse{u.In + sign*v.In, u.Out + sign*v.Out, u.CW + sign*v.CW, u.CR + sign*v.CR}
}

func (u tokUse) total() int64 { return u.In + u.Out + u.CW + u.CR }

type tokFile struct {
	Off int64            `json:"off"`
	ID  string           `json:"id,omitempty"` // the last message: later entries of it replace its usage
	U   tokUse           `json:"u"`
	H   int64            `json:"h,omitempty"`
	Day map[int64]tokUse `json:"day,omitempty"` // this file's last day by the hour, for "by agent"
}

type tokScan struct {
	mu    sync.Mutex
	dir   string // ~/.claude/projects
	file  string
	Files map[string]*tokFile `json:"files"` // by path under dir
	Hours map[int64]tokUse    `json:"hours"` // by unix hour
	ready bool                // scanned once (or loaded): until then the counts are partial
}

func newTokScan(dir, file string) *tokScan {
	t := &tokScan{dir: dir, file: file}
	if b, err := os.ReadFile(file); err == nil {
		t.ready = json.Unmarshal(b, t) == nil
	}
	if t.Files == nil || t.Hours == nil {
		t.Files, t.Hours = map[string]*tokFile{}, map[int64]tokUse{}
	}
	return t
}

// scan reads what the transcripts written to in the last month gained since the last scan.
// ponytail: a transcript rewritten shorter is read from its new end on; one that's copied (a forked session)
// counts its messages again. Neither happens in normal use.
func (t *tokScan) scan(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	seen, changed := map[string]bool{}, false
	filepath.WalkDir(t.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		fi, err := d.Info()
		if err != nil || now.Sub(fi.ModTime()) > tokKeep {
			return nil
		}
		rel, _ := filepath.Rel(t.dir, p)
		seen[rel] = true
		st := t.Files[rel]
		if st == nil {
			st = &tokFile{}
			t.Files[rel] = st
		}
		if fi.Size() < st.Off {
			st.Off = fi.Size()
		}
		if fi.Size() > st.Off && t.read(p, st, now) {
			changed = true
		}
		return nil
	})
	day, old := now.Add(-25*time.Hour).Unix()/3600, now.Add(-tokKeep).Unix()/3600
	for rel, st := range t.Files {
		if !seen[rel] {
			delete(t.Files, rel)
			changed = true
		}
		for h := range st.Day {
			if h < day {
				delete(st.Day, h)
			}
		}
	}
	for h := range t.Hours {
		if h < old {
			delete(t.Hours, h)
		}
	}
	t.ready = true
	if changed {
		if b, err := json.Marshal(t); err == nil {
			writeFileAtomic(t.file, b, 0o600)
		}
	}
}

var usageKey = []byte(`"usage"`)

// read takes a file's whole lines from where its last read stopped (a half-written one waits for the next).
func (t *tokScan) read(path string, st *tokFile, now time.Time) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.Seek(st.Off, io.SeekStart); err != nil {
		return false
	}
	day := now.Add(-25*time.Hour).Unix() / 3600
	bump := func(h int64, u tokUse, sign int64) {
		t.Hours[h] = t.Hours[h].add(u, sign)
		if h >= day {
			if st.Day == nil {
				st.Day = map[int64]tokUse{}
			}
			st.Day[h] = st.Day[h].add(u, sign)
		}
	}
	r := bufio.NewReaderSize(f, 256<<10)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return true
		}
		st.Off += int64(len(line))
		if !bytes.Contains(line, usageKey) {
			continue
		}
		var e struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				ID    string `json:"id"`
				Usage *struct {
					In  int64 `json:"input_tokens"`
					Out int64 `json:"output_tokens"`
					CW  int64 `json:"cache_creation_input_tokens"`
					CR  int64 `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.Message.Usage == nil || e.Timestamp.IsZero() {
			continue
		}
		mu := e.Message.Usage
		u, h := tokUse{mu.In, mu.Out, mu.CW, mu.CR}, e.Timestamp.Unix()/3600
		if e.Message.ID != "" && e.Message.ID == st.ID {
			bump(st.H, st.U, -1)
		}
		bump(h, u, 1)
		st.ID, st.U, st.H = e.Message.ID, u, h
	}
}

// TokView is what /api/tokens sends: the month by the hour (the page makes local days and the last 24 h
// of it) and the last day's use by agent session, most first.
type TokView struct {
	Hours  [][5]int64 `json:"hours"` // unix hour, in, out, cache written, cache read
	Agents []TokAgent `json:"agents"`
	Ready  bool       `json:"ready"` // false while the first scan ever is still reading the month
}

type TokAgent struct {
	Label  string `json:"label"`
	Tokens int64  `json:"tokens"`
}

// projName: a transcript folder as the project it's for ("-workspace-home-repos-app--worktrees-x" → "app · x").
func projName(dir string) string {
	for _, p := range []string{"-workspace-home-repos-", "-workspace-home-projects-", "-workspace-home-"} {
		if strings.HasPrefix(dir, p) {
			dir = dir[len(p):]
			break
		}
	}
	return strings.Replace(dir, "--worktrees-", " · ", 1)
}

// view: label names a session (by its project and id) when it's an agent on screen; "" leaves the project.
func (t *tokScan) view(now time.Time, label func(proj, sid string) string) TokView {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := TokView{Hours: [][5]int64{}, Agents: []TokAgent{}, Ready: t.ready}
	for h, u := range t.Hours {
		if u.total() > 0 {
			v.Hours = append(v.Hours, [5]int64{h, u.In, u.Out, u.CW, u.CR})
		}
	}
	sort.Slice(v.Hours, func(i, j int) bool { return v.Hours[i][0] < v.Hours[j][0] })
	from := now.Add(-24*time.Hour).Unix() / 3600
	by := map[string]int64{}
	for rel, st := range t.Files {
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 2 {
			continue
		}
		proj, sid := projName(parts[0]), strings.TrimSuffix(parts[1], ".jsonl") // a subagent's file sits under its session's folder
		name := proj
		if label != nil {
			if l := label(proj, sid); l != "" {
				name = l
			}
		}
		for h, u := range st.Day {
			if h > from {
				by[name] += u.total()
			}
		}
	}
	for l, n := range by {
		if n > 0 {
			v.Agents = append(v.Agents, TokAgent{l, n})
		}
	}
	sort.Slice(v.Agents, func(i, j int) bool { return v.Agents[i].Tokens > v.Agents[j].Tokens })
	return v
}

// tokensLoop keeps the counts current: a first scan soon after start (the first ever reads the month), then
// one a minute.
func (s *server) tokensLoop() {
	time.Sleep(5 * time.Second)
	for {
		t0 := time.Now()
		s.tokens.scan(t0)
		if d := time.Since(t0); d > 5*time.Second {
			log.Printf("tokens: scanned the transcripts in %v", d.Round(time.Second))
		}
		time.Sleep(time.Minute)
	}
}

func (s *server) tokensGet(w http.ResponseWriter, r *http.Request) {
	names := s.term.agentNames()
	writeJSON(w, 200, s.tokens.view(time.Now(), func(_, sid string) string { return names[sid] }))
}
