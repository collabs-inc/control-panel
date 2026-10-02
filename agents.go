package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// Pane mirrors one entry of /tmp/claude-tab-status/<session>.json, written by the
// claude-tab-status zellij plugin (pixel-agents-standalone), plus live resource use.
type Pane struct {
	PaneID          int     `json:"pane_id"`
	TabNum          int     `json:"tab_num"`
	TabName         string  `json:"tab_name"`
	Icon            string  `json:"icon"`
	Detail          string  `json:"detail"`
	Activity        string  `json:"activity"` // Thinking, Tool, Waiting, Done, Idle
	Cwd             *string `json:"cwd"`
	AgentKind       string  `json:"agent_kind"`
	AgentTitle      *string `json:"agent_title"`
	ActiveSubagents int     `json:"active_subagents"`
	CPU             float64 `json:"cpu"`
	RSS             uint64  `json:"rss"`
}

type ZSession struct {
	Name    string `json:"name"`
	Updated int64  `json:"updated"`
	Panes   []Pane `json:"panes"`
	Err     string `json:"err,omitempty"`
	waiting int
	working int
}

type Agents struct {
	Sessions []ZSession `json:"sessions"`
	Waiting  int        `json:"waiting"`
	Working  int        `json:"working"`
}

type agentsTracker struct {
	dir     string // claude-tab-status output
	sockDir string // zellij session sockets: a session is live iff its socket exists
}

var sessionNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func activityRank(a string) int {
	switch a {
	case "Waiting":
		return 0
	case "Thinking", "Tool":
		return 1
	case "Done":
		return 2
	}
	return 3
}

// poll reads status files for the given live sessions; files of dead sessions are ignored.
func (a *agentsTracker) poll(live []string) Agents {
	var out Agents
	for _, name := range live {
		zs := ZSession{Name: name}
		b, err := os.ReadFile(filepath.Join(a.dir, name+".json"))
		if err == nil {
			var f struct {
				Sessions  []Pane `json:"sessions"`
				UpdatedAt int64  `json:"updated_at"`
			}
			if err := json.Unmarshal(b, &f); err != nil {
				zs.Err = "unreadable status file: " + err.Error()
			}
			zs.Panes, zs.Updated = f.Sessions, f.UpdatedAt
		}
		for i := range zs.Panes { // the file is writable by any local process: only known values reach the UI
			switch zs.Panes[i].Activity {
			case "Thinking", "Tool", "Waiting", "Done", "Idle":
			default:
				zs.Panes[i].Activity = "Unknown"
			}
		}
		sort.SliceStable(zs.Panes, func(i, j int) bool {
			p, q := zs.Panes[i], zs.Panes[j]
			if rp, rq := activityRank(p.Activity), activityRank(q.Activity); rp != rq {
				return rp < rq
			}
			if p.TabNum != q.TabNum {
				return p.TabNum < q.TabNum
			}
			return p.PaneID < q.PaneID
		})
		for _, p := range zs.Panes {
			switch activityRank(p.Activity) {
			case 0:
				zs.waiting++
			case 1:
				zs.working++
			}
		}
		out.Waiting += zs.waiting
		out.Working += zs.working
		out.Sessions = append(out.Sessions, zs)
	}
	sort.SliceStable(out.Sessions, func(i, j int) bool {
		s, t := out.Sessions[i], out.Sessions[j]
		if s.waiting != t.waiting {
			return s.waiting > t.waiting
		}
		if s.working != t.working {
			return s.working > t.working
		}
		if len(s.Panes) != len(t.Panes) {
			return len(s.Panes) > len(t.Panes)
		}
		return s.Name < t.Name
	})
	return out
}
