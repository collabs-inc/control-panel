package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Skills a Claude session can run, read from where Claude Code looks: the project's .claude, your own
// ~/.claude skills and commands, each enabled plugin's skills and commands (run as /plugin:name), and the
// skills synced from claude.ai (the newest synced set: the signed-in account).

type Skill struct {
	Cmd  string `json:"cmd"`  // what to type: /name or /plugin:name
	Desc string `json:"desc"` // the first sentences of its description
	Src  string `json:"src"`  // project, yours, a plugin's name, claude.ai
}

// frontmatter reads name and description from a SKILL.md or command file (folded > and | blocks too).
func frontmatter(path string) (name, desc string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return "", ""
	}
	var block *string
	for n := 0; sc.Scan() && n < 200; n++ {
		l := sc.Text()
		if strings.TrimSpace(l) == "---" {
			break
		}
		if block != nil && (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) {
			*block = strings.TrimSpace(*block + " " + strings.TrimSpace(l))
			continue
		}
		block = nil
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		var dst *string
		switch strings.TrimSpace(k) {
		case "name":
			dst = &name
		case "description":
			dst = &desc
		default:
			continue
		}
		if v == ">" || v == "|" || v == ">-" || v == "|-" {
			v, block = "", dst
		}
		*dst = v
	}
	return name, trunc(desc, 240)
}

func skillsIn(dir, prefix, src string, out *[]Skill) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if !e.IsDir() || e.Name() == "synced" {
			continue
		}
		name, desc := frontmatter(filepath.Join(dir, e.Name(), "SKILL.md"))
		if name == "" {
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "SKILL.md")); err != nil {
				continue
			}
			name = e.Name()
		}
		*out = append(*out, Skill{Cmd: "/" + prefix + name, Desc: desc, Src: src})
	}
}

func commandsIn(dir, prefix, src string, out *[]Skill) {
	ms, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	for _, m := range ms {
		_, desc := frontmatter(m)
		*out = append(*out, Skill{Cmd: "/" + prefix + strings.TrimSuffix(filepath.Base(m), ".md"), Desc: desc, Src: src})
	}
}

func listSkills(home, cwd string) []Skill {
	var out []Skill
	claude := filepath.Join(home, ".claude")
	if cwd != "" && cwd != home {
		skillsIn(filepath.Join(cwd, ".claude", "skills"), "", "project", &out)
		commandsIn(filepath.Join(cwd, ".claude", "commands"), "", "project", &out)
	}
	skillsIn(filepath.Join(claude, "skills"), "", "yours", &out)
	commandsIn(filepath.Join(claude, "commands"), "", "yours", &out)

	// plugins: enabled in user or project settings, found where installed_plugins.json says
	enabled := map[string]bool{}
	for _, p := range []string{filepath.Join(claude, "settings.json"), filepath.Join(cwd, ".claude", "settings.json"), filepath.Join(cwd, ".claude", "settings.local.json")} {
		var s struct {
			EnabledPlugins map[string]bool `json:"enabledPlugins"`
		}
		if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &s) == nil {
			for k, v := range s.EnabledPlugins {
				enabled[k] = v
			}
		}
	}
	var inst struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if b, err := os.ReadFile(filepath.Join(claude, "plugins", "installed_plugins.json")); err == nil {
		json.Unmarshal(b, &inst)
	}
	ids := make([]string, 0, len(enabled))
	for id, on := range enabled {
		if on && len(inst.Plugins[id]) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		name, _, _ := strings.Cut(id, "@")
		dir := inst.Plugins[id][0].InstallPath
		skillsIn(filepath.Join(dir, "skills"), name+":", name, &out)
		commandsIn(filepath.Join(dir, "commands"), name+":", name, &out)
	}

	// claude.ai skills: the most recently synced set belongs to the account signed in now
	sets, _ := os.ReadDir(filepath.Join(claude, "skills", "synced"))
	var newest string
	var at time.Time
	for _, s := range sets {
		if fi, err := s.Info(); err == nil && s.IsDir() && fi.ModTime().After(at) {
			newest, at = s.Name(), fi.ModTime()
		}
	}
	if newest != "" {
		skillsIn(filepath.Join(claude, "skills", "synced", newest), "", "claude.ai", &out)
	}

	seen := map[string]bool{}
	uniq := out[:0]
	for _, s := range out {
		if !seen[s.Cmd] {
			seen[s.Cmd] = true
			uniq = append(uniq, s)
		}
	}
	return uniq
}

// skillCache: a pane's list, reread at most every 30 s (a handful of small files)
type skillCache struct {
	mu sync.Mutex
	m  map[string]struct {
		at time.Time
		s  []Skill
	}
}

func (c *skillCache) get(home, cwd string, now time.Time) []Skill {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]struct {
			at time.Time
			s  []Skill
		}{}
	}
	if e, ok := c.m[cwd]; ok && now.Sub(e.at) < 30*time.Second {
		return e.s
	}
	s := listSkills(home, cwd)
	c.m[cwd] = struct {
		at time.Time
		s  []Skill
	}{now, s}
	return s
}

func (s *server) paneSkills(w http.ResponseWriter, r *http.Request) {
	session, pane, err := paneParams(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	p, ok := s.term.pane(session, pane)
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "no such pane"})
		return
	}
	cwd := ""
	if p.Git != nil {
		cwd = p.Git.Root
	} else if p.Agent != nil {
		cwd = p.Agent.Cwd
	}
	if strings.HasPrefix(cwd, "~") {
		cwd = s.cfg.home + cwd[1:]
	}
	writeJSON(w, 200, map[string]any{"skills": s.skills.get(s.cfg.home, cwd, time.Now())})
}
