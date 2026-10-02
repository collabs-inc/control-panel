package main

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Folders to start a session, tab or pane in: home, each repo and its git worktrees (newest first), and
// each project. The page adds the folders terminals use right now.

type Dir struct {
	Path  string `json:"path"`
	Group string `json:"group"`
}

func subdirs(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// worktrees reads a repo's linked worktrees from .git/worktrees/*/gitdir, newest first; gone ones are skipped.
func worktrees(repo string) []string {
	type wt struct {
		path string
		at   int64
	}
	var out []wt
	for _, d := range subdirs(filepath.Join(repo, ".git", "worktrees")) {
		b, err := os.ReadFile(filepath.Join(d, "gitdir"))
		if err != nil {
			continue
		}
		p := strings.TrimSuffix(strings.TrimSpace(string(b)), "/.git")
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, wt{p, fi.ModTime().UnixNano()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at > out[j].at })
	paths := make([]string, len(out))
	for i, w := range out {
		paths[i] = w.path
	}
	return paths
}

func listDirs(home string) []Dir {
	out := []Dir{{home, "Home"}}
	repos := subdirs(filepath.Join(home, "repos"))
	for _, r := range repos {
		out = append(out, Dir{r, "Repos"})
	}
	for _, r := range repos {
		for _, w := range worktrees(r) {
			out = append(out, Dir{w, filepath.Base(r) + " worktrees"})
		}
	}
	for _, base := range []string{"projects", "side-quests"} {
		for _, p := range subdirs(filepath.Join(home, base)) {
			out = append(out, Dir{p, map[string]string{"projects": "Projects", "side-quests": "Side quests"}[base]})
		}
	}
	return out
}

func (s *server) dirs(w http.ResponseWriter, r *http.Request) {
	ds := listDirs(s.cfg.home)
	for i := range ds {
		ds[i].Path = tilde(ds[i].Path)
	}
	writeJSON(w, 200, map[string]any{"dirs": ds})
}
