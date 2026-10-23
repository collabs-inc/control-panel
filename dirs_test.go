package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListDirsOffersReposAndProjectsButNoWorktrees(t *testing.T) {
	home := t.TempDir()
	mk := func(p string) string { p = filepath.Join(home, p); os.MkdirAll(p, 0o755); return p }
	repo := mk("repos/app")
	mk("repos/app/.git/worktrees/wt")
	wt := mk("repos/app/.worktrees/wt")
	os.WriteFile(filepath.Join(repo, ".git/worktrees/wt/gitdir"), []byte(wt+"/.git\n"), 0o644)
	mk("projects/panel")
	mk("projects/.hidden")
	var got []string
	for _, d := range listDirs(home) {
		got = append(got, d.Group+"|"+strings.Replace(d.Path, home, "~", 1))
	}
	want := []string{"Home|~", "Repos|~/repos/app", "Projects|~/projects/panel"} // the owner starts terminals in a repo, not one of its worktrees
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("dirs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
