package main

import (
	"os"
	"path/filepath"
	"strings"
)

// GitInfo is the checkout a directory belongs to, read straight from .git (no git process).
type GitInfo struct {
	Branch   string `json:"branch,omitempty"`
	Root     string `json:"root,omitempty"` // the checkout's top directory (the worktree itself for worktrees)
	Repo     string `json:"repo,omitempty"` // the main repository's directory name
	Worktree bool   `json:"worktree,omitempty"`
	Detached bool   `json:"detached,omitempty"`
}

// gitInfo walks up from dir to the nearest .git: a directory for a normal checkout, or a
// "gitdir: <repo>/.git/worktrees/<name>" file for a worktree. HEAD names the branch.
func gitInfo(dir string) GitInfo {
	for d, i := filepath.Clean(dir), 0; d != "" && i < 40; d, i = filepath.Dir(d), i+1 {
		dot := filepath.Join(d, ".git")
		fi, err := os.Stat(dot)
		if err == nil {
			g := GitInfo{Root: d, Repo: filepath.Base(d)}
			gitdir := dot
			if !fi.IsDir() {
				b, _ := os.ReadFile(dot)
				p, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
				if !ok {
					return GitInfo{}
				}
				if !filepath.IsAbs(p) {
					p = filepath.Join(d, p)
				}
				gitdir, g.Worktree = p, true
				if main := filepath.Dir(filepath.Dir(p)); filepath.Base(main) == ".git" { // <repo>/.git/worktrees/<name>
					g.Repo = filepath.Base(filepath.Dir(main))
				}
			}
			head, _ := os.ReadFile(filepath.Join(gitdir, "HEAD"))
			h := strings.TrimSpace(string(head))
			if ref, ok := strings.CutPrefix(h, "ref: refs/heads/"); ok {
				g.Branch = trunc(ref, 120)
			} else if len(h) >= 7 {
				g.Branch, g.Detached = h[:7], true
			}
			return g
		}
		if d == "/" {
			break
		}
	}
	return GitInfo{}
}
