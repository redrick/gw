package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// worktreeRepo makes a real git repository with a branch "foo" and a tmux stub on
// PATH, so the test never touches a running tmux server.
func worktreeRepo(t *testing.T, ignoreCase string) string {
	t.Helper()
	root := t.TempDir()
	stubs := filepath.Join(root, "bin")
	if err := os.MkdirAll(stubs, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubs, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubs+":"+os.Getenv("PATH"))

	repo := filepath.Join(root, "repo")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", repo},
		{"-C", repo, "config", "core.ignorecase", ignoreCase},
		{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "i"},
		{"-C", repo, "branch", "foo"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return repo
}

func runGit(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// linkedWorktree adds a worktree for branch "foo" outside the repository, as
// supacode does, and gives it a subfolder.
func linkedWorktree(t *testing.T, repo string) string {
	t.Helper()
	wt := filepath.Join(filepath.Dir(repo), "elsewhere", "foo")
	runGit(t, "-C", repo, "worktree", "add", "-q", wt, "foo")
	if err := os.MkdirAll(filepath.Join(wt, "deep"), 0755); err != nil {
		t.Fatal(err)
	}
	return wt
}

// realPath gives the path as git reports it. On macOS the temp folder is under a
// symlink (/var is /private/var).
func realPath(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGitRepoRootOfALinkedWorktreeIsTheMainRepository(t *testing.T) {
	repo := worktreeRepo(t, "false")
	wt := linkedWorktree(t, repo)

	for _, path := range []string{wt, filepath.Join(wt, "deep")} {
		got, err := gitRepoRoot(path)
		if err != nil {
			t.Fatalf("gitRepoRoot(%s): %v", path, err)
		}
		if got != realPath(t, repo) {
			t.Errorf("gitRepoRoot(%s) = %s, want the main repository %s", path, got, realPath(t, repo))
		}
	}
}

func TestGitRepoRootKeepsTheMainRepositoryAndSubmodules(t *testing.T) {
	repo := worktreeRepo(t, "false")
	lib := filepath.Join(filepath.Dir(repo), "lib")
	runGit(t, "init", "-q", "-b", "main", lib)
	runGit(t, "-C", lib, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "i")
	runGit(t, "-C", repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", lib, "mods/lib")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0755); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		repo:                               repo,
		filepath.Join(repo, "sub"):         repo,
		filepath.Join(repo, "mods", "lib"): filepath.Join(repo, "mods", "lib"),
	}
	for path, want := range cases {
		got, err := gitRepoRoot(path)
		if err != nil {
			t.Fatalf("gitRepoRoot(%s): %v", path, err)
		}
		if got != realPath(t, want) {
			t.Errorf("gitRepoRoot(%s) = %s, want %s", path, got, realPath(t, want))
		}
	}
}

func TestAddWorktreeRejectsBranchThatDiffersOnlyByCase(t *testing.T) {
	repo := worktreeRepo(t, "true")

	_, _, err := addWorktree(repo, "repo", "Foo", "")

	if err == nil {
		t.Fatal("expected a case collision error")
	}
	if !strings.Contains(err.Error(), `"foo"`) {
		t.Fatalf("error does not name the existing branch: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(repo), "repo-Foo")); statErr == nil {
		t.Fatal("a worktree was created despite the collision")
	}
}

func TestAddWorktreeAcceptsExactBranchName(t *testing.T) {
	repo := worktreeRepo(t, "true")

	wt, _, err := addWorktree(repo, "repo", "foo", "")

	if err != nil {
		t.Fatalf("addWorktree: %v", err)
	}
	if wt.Branch != "foo" {
		t.Fatalf("branch = %q", wt.Branch)
	}
}
