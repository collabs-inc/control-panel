package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Folders to start a session, tab or pane in: home, each repo and each project; not the repos' worktrees
// (the owner's ask). The page adds the folders terminals use right now, a worktree as its repo.

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

func listDirs(home string) []Dir {
	out := []Dir{{home, "Home"}}
	repos := subdirs(filepath.Join(home, "repos"))
	for _, r := range repos {
		out = append(out, Dir{r, "Repos"})
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
