package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGitInfo(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "alven")
	os.MkdirAll(filepath.Join(repo, ".git", "worktrees", "feat-x"), 0o755)
	os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	wt := filepath.Join(repo, ".worktrees", "feat-x")
	os.MkdirAll(filepath.Join(wt, "sub"), 0o755)
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(repo, ".git", "worktrees", "feat-x")+"\n"), 0o644)
	os.WriteFile(filepath.Join(repo, ".git", "worktrees", "feat-x", "HEAD"), []byte("ref: refs/heads/feat/x\n"), 0o644)

	if g := gitInfo(filepath.Join(wt, "sub")); g != (GitInfo{Branch: "feat/x", Root: wt, Repo: "alven", Worktree: true}) {
		t.Fatalf("worktree = %+v", g)
	}
	if g := gitInfo(repo); g != (GitInfo{Branch: "main", Root: repo, Repo: "alven"}) {
		t.Fatalf("main checkout = %+v", g)
	}
	os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("0123456789abcdef0123456789abcdef01234567\n"), 0o644)
	if g := gitInfo(repo); g.Branch != "0123456" || !g.Detached {
		t.Fatalf("detached = %+v", g)
	}
	if g := gitInfo(root); g != (GitInfo{}) {
		t.Fatalf("no repo = %+v", g)
	}
}
