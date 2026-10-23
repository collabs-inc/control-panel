package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The compactor keeps the agents' context windows from filling up without cutting one off mid-thought. Past the
// auto limit (60% by default) it compacts a Claude whose turn is over; one still working gets a note to stop at
// its next natural break, is compacted then, and told to continue. A reply that asks you something waits for
// your answer: the turn after it is compacted when it ends. The page offers Compact from the suggest limit
// (40%). Claude Code's own auto-compact near the full window stays the backstop for a turn that never stops.

// panelMark starts what the compactor types into a Claude, so its chat can say who wrote it.
const panelMark = "[Control panel] "

const (
	compactCmd  = "/compact Keep the task in progress and its next step, the files being changed, and anything the user still has to answer, word for word."
	resumeMsg   = panelMark + "Compacted. Continue where you left off."
	compactTTL  = 15 * 60 // s: a /compact that hasn't finished by then failed; a failed one is tried again after it
	compactRest = 10 * 60 // s: compacted (by anyone) this recently, it isn't by itself: a limit under what a fresh start holds would loop
)

func pauseMsg(pct int) string {
	return panelMark + fmt.Sprintf("Your context is %d%% full. Finish the step you're on, then stop at your next natural break: "+
		"end your turn with a one-line status of where you are and what's next, without starting anything new. "+
		"The panel will compact the conversation and then tell you to continue.", pct)
}

// CompactCfg: the limits, in % of the context window, as the page's ring counts it.
type CompactCfg struct {
	On      bool `json:"on"`      // compact by itself at Auto
	Suggest int  `json:"suggest"` // the page offers Compact from here
	Auto    int  `json:"auto"`
}

// AgentCompact: what the page shows of one agent's record.
type AgentCompact struct {
	Phase string `json:"phase,omitempty"` // hold, pausing, compacting, done, failed
	Off   bool   `json:"off,omitempty"`
}

type compactRec struct {
	Off    bool   `json:"off,omitempty"`    // you said not to compact this one by itself
	Phase  string `json:"phase,omitempty"`  // "", hold, pausing, compacting, done, failed
	At     int64  `json:"at,omitempty"`     // when the phase began (unix s)
	Mark   int64  `json:"mark,omitempty"`   // hold: when the reply that asked was written
	Resume bool   `json:"resume,omitempty"` // compacting a turn it paused: tell it to continue after
	Manual bool   `json:"manual,omitempty"` // you pressed Compact: done even when Off or under the limit
	Seen   int64  `json:"seen,omitempty"`   // last seen live
	idle   int64  // since when its turn has been over (not kept)
}

// compactView: what the compactor reads of an agent.
type compactView struct {
	state            string // its pane's: waiting, background, working, stalled, asking…
	pct              int
	asks             bool  // its last reply asks you something
	lastAt, compacts int64 // when Claude last wrote; when it was last compacted
	kids             bool  // subagents or workflows still running: their results come back into the conversation
}

// next moves an agent's record along and says what to type into its pane: "note", "compact", "continue" or "".
// The phase an action starts is set by did, once the action went through.
func (r *compactRec) next(v compactView, cfg CompactCfg, now int64) string {
	over := v.pct >= cfg.Auto
	if (v.state == "waiting" || v.state == "background") && !v.kids {
		if r.idle == 0 {
			r.idle = now
		}
	} else {
		r.idle = 0
	}
	settled := r.idle > 0 && now-r.idle >= 3 // over for a moment: a message queued meanwhile isn't about to start a turn
	switch r.Phase {
	case "compacting":
		switch {
		case v.compacts >= r.At && r.Resume:
			if settled {
				return "continue"
			}
		case v.compacts >= r.At:
			r.set("done", now)
		case now-r.At > compactTTL:
			r.set("failed", now)
		}
		return ""
	case "pausing":
		switch {
		case v.compacts >= r.At: // Claude Code compacted it by itself, and it carried on
			r.set("done", now)
		case settled && (v.lastAt >= r.At || now-r.idle >= 30): // it stopped after the note
			return "compact"
		}
		return ""
	case "hold":
		switch {
		case !over && !r.Manual: // compacted some other way
			r.set("", now)
		case settled && v.lastAt > r.Mark: // you answered and that turn is over too
			return "compact"
		}
		return ""
	case "done":
		if over && !r.Manual {
			return "" // once, until it has been under the limit
		}
		r.set("", now)
	case "failed":
		if now-r.At < compactTTL && !r.Manual {
			return ""
		}
		r.set("", now)
	}
	if !r.Manual && (!cfg.On || r.Off || !over || v.compacts > 0 && now-v.compacts < compactRest) {
		return ""
	}
	switch {
	case settled && v.asks && !r.Manual:
		r.set("hold", now)
		r.Mark = v.lastAt
	case settled:
		return "compact"
	case v.state == "working" || v.state == "stalled":
		return "note"
	}
	return ""
}

// did: the phase an action starts, once it went through (a failure waits compactTTL to try again).
func (r *compactRec) did(act string, err error, now int64) {
	r.idle = 0 // what was typed may start a turn: settle again before the next step
	switch {
	case err != nil:
		r.set("failed", now)
	case act == "note":
		r.set("pausing", now)
	case act == "compact":
		resume := r.Phase == "pausing"
		r.set("compacting", now)
		r.Resume = resume
	default:
		r.set("done", now)
	}
}

func (r *compactRec) set(phase string, now int64) {
	r.Phase, r.At = phase, now
	if phase == "" || phase == "done" || phase == "failed" {
		r.Resume, r.Manual = false, false
	}
}

type compactor struct {
	mu    sync.Mutex
	file  string
	cfg   CompactCfg
	recs  map[string]*compactRec // by Claude session id
	busy  map[string]bool        // typing into its pane right now
	scope *regexp.Regexp         // the zellij sessions it types into (nil: all); a test panel keeps to throwaway ones
}

func newCompactor(file, scope string) *compactor {
	c := &compactor{file: file, cfg: CompactCfg{On: true, Suggest: 40, Auto: 60}, recs: map[string]*compactRec{}, busy: map[string]bool{}}
	if scope != "" {
		c.scope = regexp.MustCompile(scope)
	}
	var saved struct {
		Cfg  *CompactCfg            `json:"cfg"`
		Recs map[string]*compactRec `json:"recs"`
	}
	if b, err := os.ReadFile(file); err == nil && json.Unmarshal(b, &saved) == nil {
		if saved.Cfg != nil && validCompact(*saved.Cfg) == nil {
			c.cfg = *saved.Cfg
		}
		for k, r := range saved.Recs {
			if r != nil && sessionIDRe.MatchString(k) {
				c.recs[k] = r
			}
		}
	}
	return c
}

func validCompact(c CompactCfg) error {
	if c.Suggest < 1 || c.Auto > 99 || c.Suggest > c.Auto {
		return errors.New("limits go from 1% to 99%, the suggestion at or under the auto-compact")
	}
	return nil
}

// save writes the settings and records; the caller holds mu. A session not seen for a week is dropped, unless
// you switched it off.
func (c *compactor) save(now int64) {
	for k, r := range c.recs {
		if !r.Off && now-r.Seen > 7*86400 {
			delete(c.recs, k)
		}
	}
	b, _ := json.Marshal(map[string]any{"cfg": c.cfg, "recs": c.recs})
	if os.MkdirAll(filepath.Dir(c.file), 0o700) == nil {
		writeFileAtomic(c.file, b, 0o600)
	}
}

func (c *compactor) config() CompactCfg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// of: the agent's record for the page; nil when there's nothing to say.
func (c *compactor) of(sid string) *AgentCompact {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.recs[sid]
	if r == nil || (r.Phase == "" && !r.Off) {
		return nil
	}
	return &AgentCompact{Phase: r.Phase, Off: r.Off}
}

func (c *compactor) rec(sid string) *compactRec {
	r := c.recs[sid]
	if r == nil {
		r = &compactRec{}
		c.recs[sid] = r
	}
	return r
}

// step looks at every Claude once per tick and starts what each needs, one action at a time per agent.
func (s *server) compactStep(now time.Time, tm Terminals) {
	c := s.compact
	c.mu.Lock()
	defer c.mu.Unlock()
	cfg, t, changed := c.cfg, now.Unix(), false
	for _, ss := range tm.Sessions {
		if c.scope != nil && !c.scope.MatchString(ss.Name) {
			continue
		}
		for _, tb := range ss.Tabs {
			for _, p := range tb.Panes {
				a := p.Agent
				if a == nil || a.sid == "" || a.Ctx == nil || a.Ctx.Window <= 0 || c.busy[a.sid] {
					continue
				}
				r, ok := c.recs[a.sid]
				if !ok && !cfg.On {
					continue
				}
				r = c.rec(a.sid)
				r.Seen = t
				ti, _ := s.term.tr.tail(a.sid) // cached: the tracker just read it
				v := compactView{state: p.State, pct: int((a.Ctx.Used*100 + a.Ctx.Window/2) / a.Ctx.Window), asks: ti.asks,
					lastAt: a.LastAt, compacts: ti.compacts, kids: len(a.Subagents)+len(a.Workflows) > 0}
				before := *r
				act := r.next(v, cfg, t)
				changed = changed || r.Phase != before.Phase || r.Mark != before.Mark
				if act == "" {
					continue
				}
				c.busy[a.sid] = true
				go s.compactDo(a.sid, ss.Name, p.ID, act, v.pct)
			}
		}
	}
	if changed {
		c.save(t)
	}
}

var errBoxTaken = errors.New("its input box has text in it")

// compactDo types one step into the agent's pane: never over what's in its input box.
func (s *server) compactDo(sid, session string, pane int, act string, pct int) {
	text := map[string]string{"note": pauseMsg(pct), "compact": compactCmd, "continue": resumeMsg}[act]
	var err error
	if screen, e := s.z.dumpANSI(session, pane); e != nil {
		err = e
	} else if typedBox(screen) != "" {
		err = errBoxTaken
	} else {
		_, err = s.sendToClaude(session, pane, text)
	}
	c := s.compact
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.busy, sid)
	r, now := c.rec(sid), time.Now().Unix()
	what := fmt.Sprintf("%s:%d %s at %d%%", session, pane, map[string]string{"note": "asked to pause", "compact": "compacting", "continue": "told to continue"}[act], pct)
	if errors.Is(err, errBoxTaken) {
		return // tried again next tick: you may be writing to it
	}
	if r.Manual && act != "continue" {
		what += " (you asked)"
	}
	r.did(act, err, now)
	if err != nil {
		s.audit.add("compact-fail", what+": "+err.Error(), "panel")
	} else {
		s.audit.add("compact", what, "panel")
	}
	c.save(now)
	select {
	case s.wake <- struct{}{}: // the page sees the new phase at once
	default:
	}
}

// compactSettings: POST {on, suggest, auto}.
func (s *server) compactSettings(w http.ResponseWriter, r *http.Request) {
	var b CompactCfg
	if err := readJSON(r, &b); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	if err := validCompact(b); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	c := s.compact
	c.mu.Lock()
	old := c.cfg
	c.cfg = b
	c.save(time.Now().Unix())
	c.mu.Unlock()
	if old != b {
		s.audit.add("compact-settings", fmt.Sprintf("suggest %d%%, auto %d%%, auto-compact %s", b.Suggest, b.Auto, map[bool]string{true: "on", false: "off"}[b.On]), clientIP(r))
	}
	s.wakeUp()
	writeJSON(w, 200, map[string]any{"ok": true, "compact": b})
}

// paneCompact: POST {session, pane, act}: "now" (compact it: at once if its turn is over, else at its next
// break), "off" or "on" (whether it's compacted by itself).
func (s *server) paneCompact(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Session, Act string
		Pane         int
	}
	if err := readJSON(r, &b); err != nil || !strings.Contains("now off on", b.Act) || b.Act == "" {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	tp, ok := s.term.pane(b.Session, b.Pane)
	if !ok || tp.Agent == nil || tp.Agent.sid == "" {
		writeJSON(w, 404, map[string]any{"error": "no Claude in that pane"})
		return
	}
	c := s.compact
	if c.scope != nil && !c.scope.MatchString(b.Session) {
		writeJSON(w, 403, map[string]any{"error": "this panel doesn't compact agents in " + b.Session})
		return
	}
	c.mu.Lock()
	rec := c.rec(tp.Agent.sid)
	switch b.Act {
	case "now":
		if rec.Phase == "pausing" || rec.Phase == "compacting" {
			break
		}
		rec.set("", time.Now().Unix())
		rec.Manual = true
	default:
		rec.Off = b.Act == "off"
		if rec.Off && rec.Phase == "hold" {
			rec.set("", time.Now().Unix())
		}
	}
	c.save(time.Now().Unix())
	out := &AgentCompact{Phase: rec.Phase, Off: rec.Off}
	c.mu.Unlock()
	what := fmt.Sprintf("%s:%d", b.Session, b.Pane)
	s.audit.add("compact-"+b.Act, what, clientIP(r))
	s.wakeUp()
	writeJSON(w, 200, map[string]any{"ok": true, "compact": out})
}

func (s *server) wakeUp() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
