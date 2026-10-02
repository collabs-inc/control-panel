package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListDirsOffersReposWorktreesAndProjects(t *testing.T) {
	home := t.TempDir()
	mk := func(p string) string { p = filepath.Join(home, p); os.MkdirAll(p, 0o755); return p }
	repo := mk("repos/app")
	mk("repos/app/.git/worktrees/old")
	mk("repos/app/.git/worktrees/new")
	mk("repos/app/.git/worktrees/gone")
	oldWT, newWT := mk("repos/app/.worktrees/old"), mk("repos/app/.worktrees/new")
	os.WriteFile(filepath.Join(repo, ".git/worktrees/old/gitdir"), []byte(oldWT+"/.git\n"), 0o644)
	os.WriteFile(filepath.Join(repo, ".git/worktrees/new/gitdir"), []byte(newWT+"/.git\n"), 0o644)
	os.WriteFile(filepath.Join(repo, ".git/worktrees/gone/gitdir"), []byte(filepath.Join(home, "nowhere/.git")), 0o644)
	os.Chtimes(oldWT, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	mk("projects/panel")
	mk("projects/.hidden")
	var got []string
	for _, d := range listDirs(home) {
		got = append(got, d.Group+"|"+strings.Replace(d.Path, home, "~", 1))
	}
	want := []string{"Home|~", "Repos|~/repos/app", "app worktrees|~/repos/app/.worktrees/new", "app worktrees|~/repos/app/.worktrees/old", "Projects|~/projects/panel"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("dirs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
