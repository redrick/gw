package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ── State ─────────────────────────────────────────────────────────────────────

type Project struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type State struct {
	Projects         []Project         `json:"projects"`
	ActiveTitle      string            `json:"active_title,omitempty"`
	ActiveSub        map[string]string `json:"active_sub,omitempty"`
	LaunchDir        string            `json:"launch_dir,omitempty"`
	LaunchedFromShell bool             `json:"launched_from_shell,omitempty"`
}

func statePath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "gw", "state.json")
}

func loadState() State {
	data, err := os.ReadFile(statePath())
	if err != nil {
		return State{}
	}
	var st State
	json.Unmarshal(data, &st)
	return st
}

func saveState(st State) {
	p := statePath()
	os.MkdirAll(filepath.Dir(p), 0755)
	data, _ := json.MarshalIndent(st, "", "  ")
	os.WriteFile(p, data, 0644)
}

func (st *State) AddProject(path string) {
	for _, p := range st.Projects {
		if p.Path == path {
			return
		}
	}
	st.Projects = append(st.Projects, Project{
		Path: path,
		Name: filepath.Base(path),
	})
}

func (st *State) RemoveProject(path string) {
	var kept []Project
	for _, p := range st.Projects {
		if p.Path != path {
			kept = append(kept, p)
		}
	}
	st.Projects = kept
}

// ── Tmux pane management ──────────────────────────────────────────────────────
//
// Each worktree gets a hidden tmux window ("storage window") that holds its
// live pane. The visible right slot is gw:active.1. Switching is a server-side
// swap-pane — nothing is typed into the pane's process.

func shellBin() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "bash"
}

func tmuxSessionExists(name string) bool {
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

func tmuxWindowExists(name string) bool {
	out, err := exec.Command("tmux", "list-windows", "-t", "gw",
		"-F", "#{window_name}").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}

func wtWindowTitle(projectName, branch string) string {
	safe := strings.NewReplacer("/", "-", " ", "_", ".", "-", ":", "-").Replace(branch)
	return "wt-" + projectName + "-" + safe
}

// ensureStorageWindow creates a detached tmux window for the worktree if it
// does not already exist. The pane in that window preserves the worktree's
// live state (vim, servers, etc.) while another worktree is active.
func ensureStorageWindow(winName, path string) {
	if tmuxWindowExists(winName) {
		return
	}
	args := []string{"new-window", "-d", "-t", "gw", "-n", winName}
	if path != "" {
		args = append(args, "-c", path)
	}
	args = append(args, shellBin())
	exec.Command("tmux", args...).Run()
	exec.Command("tmux", "set-option", "-t", "gw:"+winName, "automatic-rename", "off").Run()
	exec.Command("tmux", "set-option", "-t", "gw:"+winName, "allow-rename", "off").Run()
}

func liveWindows() map[string]bool {
	out, err := exec.Command("tmux", "list-windows", "-t", "gw", "-F", "#{window_name}").Output()
	if err != nil {
		return nil
	}
	m := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			m[name] = true
		}
	}
	return m
}

func isPaneDead(target string) bool {
	out, _ := exec.Command("tmux", "display-message", "-t", target,
		"-p", "#{pane_dead}").Output()
	return strings.TrimSpace(string(out)) == "1"
}

// switchToWindow moves the current active pane back to its storage window and
// brings the target pane into the active right slot — all server-side.
//
// The shell item ("gw-shell") has no storage window of its own. The original
// split-shell acts as a displaced dummy that bounces through worktree storage
// slots. Switching *to* shell just returns the current worktree to its slot,
// which naturally surfaces the displaced shell in active.1. Switching *from*
// shell skips the "return to storage" step (nothing to return).
func switchToWindow(fromTitle, toTitle, toPath string, st State) {
	isShellTitle := func(t string) bool { return t == "gw-shell" }

	if isShellTitle(toTitle) {
		// Return current worktree to storage; displaced shell surfaces in active.1.
		if fromTitle != "" && !isShellTitle(fromTitle) {
			fromSub := activeSubForTitle(st, fromTitle)
			if tmuxWindowExists(fromSub) {
				exec.Command("tmux", "swap-pane",
					"-s", "gw:active.1",
					"-t", "gw:"+fromSub+".0").Run()
			}
		}
		exec.Command("tmux", "select-pane", "-t", "gw:active.1", "-T", "").Run()
		exec.Command("tmux", "select-pane", "-t", "gw:active.1").Run()
		return
	}

	ensureStorageWindow(toTitle, toPath)

	toSub := activeSubForTitle(st, toTitle)
	if !tmuxWindowExists(toSub) {
		toSub = toTitle
	}

	if isPaneDead("gw:" + toSub + ".0") {
		exec.Command("tmux", "respawn-pane", "-k",
			"-c", toPath,
			"-t", "gw:"+toSub+".0").Run()
	}

	// Return current worktree to its storage (skip when coming from shell —
	// shell has no storage window).
	if fromTitle != "" && !isShellTitle(fromTitle) {
		fromSub := activeSubForTitle(st, fromTitle)
		if tmuxWindowExists(fromSub) {
			exec.Command("tmux", "swap-pane",
				"-s", "gw:active.1",
				"-t", "gw:"+fromSub+".0").Run()
		}
	}

	// Bring target pane into active.1.
	exec.Command("tmux", "swap-pane",
		"-s", "gw:active.1",
		"-t", "gw:"+toSub+".0").Run()

	updateStatusBar(toTitle, toSub)
	exec.Command("tmux", "select-pane", "-t", "gw:active.1").Run()
}

// ── Git worktrees ─────────────────────────────────────────────────────────────

type Worktree struct {
	Path   string
	Branch string
}

func listWorktrees(repoPath string) ([]Worktree, error) {
	out, err := exec.Command("git", "-C", repoPath, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil, err
	}
	var trees []Worktree
	var cur Worktree
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "worktree "):
			cur = Worktree{Path: strings.TrimPrefix(line, "worktree ")}
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "detached":
			cur.Branch = "(detached)"
		case line == "bare":
			cur.Branch = "(bare)"
		case line == "":
			if cur.Path != "" {
				if cur.Branch == "" {
					cur.Branch = "(unknown)"
				}
				trees = append(trees, cur)
				cur = Worktree{}
			}
		}
	}
	if cur.Path != "" {
		if cur.Branch == "" {
			cur.Branch = "(unknown)"
		}
		trees = append(trees, cur)
	}
	return trees, sc.Err()
}

func isGitRepo(path string) bool {
	return exec.Command("git", "-C", path, "rev-parse", "--git-dir").Run() == nil
}

func gitRepoRoot(path string) (string, error) {
	out, err := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel").Output()
	return strings.TrimSpace(string(out)), err
}

// ── Sub-window management ──────────────────────────────────────────────────────

func subWindowsForTitle(baseTitle string) []string {
	out, err := exec.Command("tmux", "list-windows", "-t", "gw", "-F", "#{window_name}").Output()
	if err != nil {
		return nil
	}
	var result []string
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == baseTitle || strings.HasPrefix(name, baseTitle+"~") {
			result = append(result, name)
		}
	}
	return result
}

func activeSubForTitle(st State, baseTitle string) string {
	if st.ActiveSub != nil {
		if name, ok := st.ActiveSub[baseTitle]; ok && tmuxWindowExists(name) {
			return name
		}
	}
	return baseTitle
}

func createSubWindow(baseTitle, path string) (string, error) {
	subs := subWindowsForTitle(baseTitle)
	n := len(subs) + 1
	newName := fmt.Sprintf("%s~%d", baseTitle, n)
	for tmuxWindowExists(newName) {
		n++
		newName = fmt.Sprintf("%s~%d", baseTitle, n)
	}
	args := []string{"new-window", "-d", "-t", "gw", "-n", newName}
	if path != "" {
		args = append(args, "-c", path)
	}
	args = append(args, shellBin())
	if err := exec.Command("tmux", args...).Run(); err != nil {
		return "", err
	}
	exec.Command("tmux", "set-option", "-t", "gw:"+newName, "automatic-rename", "off").Run()
	exec.Command("tmux", "set-option", "-t", "gw:"+newName, "allow-rename", "off").Run()
	return newName, nil
}

func updateStatusBar(baseTitle, activeSub string) {
	subs := subWindowsForTitle(baseTitle)
	if len(subs) == 0 {
		return
	}
	var parts []string
	for i, s := range subs {
		num := fmt.Sprintf("%d", i+1)
		if s == activeSub {
			parts = append(parts, "●"+num)
		} else {
			parts = append(parts, " "+num)
		}
	}
	exec.Command("tmux", "select-pane", "-t", "gw:active.1", "-T",
		strings.Join(parts, "  ")).Run()
}

func getWorktreePath(st State, baseTitle string) string {
	for _, p := range st.Projects {
		trees, err := listWorktrees(p.Path)
		if err != nil {
			continue
		}
		for _, wt := range trees {
			if wtWindowTitle(p.Name, wt.Branch) == baseTitle {
				return wt.Path
			}
		}
	}
	return ""
}

// branchExists reports whether the branch already exists as a local branch or
// as a remote-tracking ref on any configured remote.
func branchExists(projectPath, branch string) bool {
	if exec.Command("git", "-C", projectPath, "rev-parse", "--verify",
		"refs/heads/"+branch).Run() == nil {
		return true
	}
	out, err := exec.Command("git", "-C", projectPath, "branch", "-r",
		"--list", "*/"+branch).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func removeWorktree(projectPath, wtPath, title string, isActive bool, st State) error {
	if wtPath == projectPath {
		return fmt.Errorf("cannot remove main worktree")
	}

	if isActive {
		// Swap the pane back to its storage window before we kill it.
		activeSub := activeSubForTitle(st, title)
		if tmuxWindowExists(activeSub) {
			exec.Command("tmux", "swap-pane",
				"-s", "gw:active.1",
				"-t", "gw:"+activeSub+".0").Run()
		}
	}

	out, err := exec.Command("git", "-C", projectPath, "worktree", "remove", wtPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}

	for _, win := range subWindowsForTitle(title) {
		exec.Command("tmux", "kill-window", "-t", "gw:"+win).Run()
	}

	return nil
}

func addWorktree(projectPath, projectName, branch string) (Worktree, string, error) {
	safe := strings.ReplaceAll(branch, "/", "-")
	wtPath := filepath.Join(filepath.Dir(projectPath), filepath.Base(projectPath)+"-"+safe)

	var out []byte
	var err error
	if branchExists(projectPath, branch) {
		// Branch already exists locally or on a remote — check it out directly.
		// Git's DWIM will set up remote tracking automatically when applicable.
		out, err = exec.Command("git", "-C", projectPath, "worktree", "add",
			wtPath, branch).CombinedOutput()
	} else {
		// Brand-new branch — create it.
		out, err = exec.Command("git", "-C", projectPath, "worktree", "add",
			"-b", branch, wtPath).CombinedOutput()
	}
	if err != nil {
		return Worktree{}, "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}

	wt := Worktree{Path: wtPath, Branch: branch}
	title := wtWindowTitle(projectName, branch)
	ensureStorageWindow(title, wtPath)
	return wt, title, nil
}
