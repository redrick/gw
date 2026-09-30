package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--sidebar":
			runSidebar()
			return
		case "--new-subwindow":
			runNewSubwindow()
			return
		case "--close-subwindow":
			runCloseSubwindow()
			return
		case "--next-subwindow":
			runNextSubwindow()
			return
		case "--prev-subwindow":
			runPrevSubwindow()
			return
		case "--handle-pane-dead":
			runHandlePaneDead()
			return
		case "--pin-preview":
			runPinPreview()
			return
		case "--pr-details":
			if len(os.Args) > 2 {
				runPRDetails(os.Args[2])
			}
			return
		case "--create-pr":
			if len(os.Args) > 2 {
				runCreatePR(os.Args[2])
			}
			return
		}
	}
	launch()
}

func launch() {
	wd, _ := os.Getwd()
	st := loadState()

	if wd != "" && isGitRepo(wd) {
		if root, err := gitRepoRoot(wd); err == nil {
			st.AddProject(root)
			// Remember the worktree we launched from so the sidebar defaults
			// its cursor to it instead of the first item in the list.
			st.LaunchWorktree = filepath.Clean(root)
		}
	}

	if tmuxSessionExists("gw") {
		saveState(st)
		// Redeploy the monitor script so a rebuilt binary's notification logic
		// takes effect on reattach without killing the session.
		refreshMonitorPane()
		cmd := exec.Command("tmux", "attach-session", "-t", "gw")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Run()
		return
	}

	if wd != "" && !isGitRepo(wd) {
		st.LaunchDir = wd
		st.LaunchedFromShell = true
	} else {
		st.LaunchedFromShell = false
	}
	// A fresh session starts with the bootstrap shell in active.1 — no worktree
	// is swapped in yet. Reset the stale ActiveTitle carried over from a previous
	// session so the sidebar doesn't falsely mark (and highlight) a worktree as
	// active. Leaving it stale desyncs switchToWindow: selecting that worktree
	// runs with from==to and its two swap-panes cancel out, so it never enters
	// active.1 and becomes unreachable.
	if st.LaunchedFromShell {
		st.ActiveTitle = "gw-shell"
	} else {
		st.ActiveTitle = ""
	}
	saveState(st)
	setupTmuxSession()
}

func setupTmuxSession() {
	bin, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		os.Exit(1)
	}

	// Window "active": pane 0 = sidebar TUI, pane 1 = current worktree.
	if err := exec.Command("tmux", "new-session", "-d", "-s", "gw", "-n", "active",
		bin, "--sidebar").Run(); err != nil {
		fmt.Fprintln(os.Stderr, "gw: tmux:", err)
		os.Exit(1)
	}

	sh := shellBin()
	exec.Command("tmux", "split-window", "-t", "gw:active.0", "-h", sh, "-l").Run()

	// Keep pane 1 alive even after shell exits (e.g., ^D).
	exec.Command("tmux", "set-option", "-t", "gw:active.1", "remain-on-exit", "on").Run()
	// Hook to respawn pane 1 if it dies (fallback if remain-on-exit isn't enough).
	exec.Command("tmux", "set-hook", "-t", "gw", "pane-died",
		"run-shell '"+bin+" --handle-pane-dead'").Run()
	exec.Command("tmux", "select-pane", "-t", "gw:active.0").Run()

	// Session-scoped options.
	for _, opt := range [][]string{
		{"prefix", "C-a"},
		{"mouse", "on"},
		{"status", "off"},
		{"pane-border-style", "fg=colour238"},
		{"pane-active-border-style", "fg=colour99"},
		// Prevent tmux from renaming windows — we rely on exact names for lookup.
		{"automatic-rename", "off"},
		{"allow-rename", "off"},
	} {
		exec.Command("tmux", "set-option", "-t", "gw", opt[0], opt[1]).Run()
	}
	// Allow sending a literal C-a to the running program via ^w ^w.
	exec.Command("tmux", "bind-key", "-T", "prefix", "C-a", "send-prefix").Run()

	// Pin sidebar to 30 cols now and on every future re-attach
	// (without the hook, re-attaching from a wider terminal rescales the layout).
	exec.Command("tmux", "resize-pane", "-t", "gw:active.0", "-x", "40").Run()
	exec.Command("tmux", "set-hook", "-t", "gw", "client-resized",
		"resize-pane -t gw:active.0 -x 40").Run()

	// Bottom status bar on the right pane showing sub-window tabs.
	exec.Command("tmux", "set-window-option", "-t", "gw:active", "pane-border-status", "bottom").Run()
	exec.Command("tmux", "set-window-option", "-t", "gw:active", "pane-border-format",
		"#[align=centre]#{pane_title}").Run()
	exec.Command("tmux", "select-pane", "-t", "gw:active.0", "-T", "").Run()

	// Sub-window keybindings (^a c/x/n/p) and sidebar focus (^a s).
	exec.Command("tmux", "bind-key", "c", "run-shell", bin+" --new-subwindow").Run()
	exec.Command("tmux", "bind-key", "x", "run-shell", bin+" --close-subwindow").Run()
	exec.Command("tmux", "bind-key", "n", "run-shell", bin+" --next-subwindow").Run()
	exec.Command("tmux", "bind-key", "p", "run-shell", bin+" --prev-subwindow").Run()
	exec.Command("tmux", "bind-key", "s", "select-pane", "-t", "gw:active.0").Run()
	exec.Command("tmux", "bind-key", "b", "run-shell", bin+" --pin-preview").Run()

	cmd := exec.Command("tmux", "attach-session", "-t", "gw")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Run()
}

func runNewSubwindow() {
	st := loadState()
	baseTitle := st.ActiveTitle
	if baseTitle == "" {
		return
	}
	path := getWorktreePath(st, baseTitle)
	if baseTitle == "gw-shell" {
		path = st.LaunchDir
	}

	newSub, err := createSubWindow(baseTitle, path, os.Args[2:])
	if err != nil {
		return
	}

	deadSub := ""
	if isPaneDead("gw:active.1") {
		deadSub = paneTag("gw:active.1")
	}
	showSub(newSub, path)
	if deadSub != "" {
		exec.Command("tmux", "kill-window", "-t", "gw:"+deadSub).Run()
	}

	setActiveSub(&st, baseTitle, newSub)
}

func setActiveSub(st *State, baseTitle, sub string) {
	if st.ActiveSub == nil {
		st.ActiveSub = make(map[string]string)
	}
	if sub == "" {
		delete(st.ActiveSub, baseTitle)
	} else {
		st.ActiveSub[baseTitle] = sub
	}
	saveState(*st)
	updateStatusBar(baseTitle, sub)
	exec.Command("tmux", "select-pane", "-t", "gw:active.1").Run()
}

func neighbourSub(subs []string, current string) string {
	for i, s := range subs {
		if s == current {
			if i+1 < len(subs) {
				return subs[i+1]
			}
			if i > 0 {
				return subs[i-1]
			}
			return ""
		}
	}
	for _, s := range subs {
		if s != current {
			return s
		}
	}
	return ""
}

func runCloseSubwindow() {
	st := loadState()
	baseTitle := st.ActiveTitle
	currentSub := paneTag("gw:active.1")
	if baseTitle == "" || currentSub == "" {
		return
	}
	subs := subWindowsForTitle(baseTitle)
	if len(subs) == 1 && currentSub == baseTitle {
		return
	}
	nextSub := neighbourSub(subs, currentSub)

	parkActive()
	exec.Command("tmux", "kill-window", "-t", "gw:"+currentSub).Run()
	if nextSub != "" {
		showSub(nextSub, getWorktreePath(st, baseTitle))
	}
	setActiveSub(&st, baseTitle, nextSub)
}

func runNavigateSubwindow(dir int) {
	st := loadState()
	baseTitle := st.ActiveTitle
	if baseTitle == "" {
		return
	}
	currentSub := paneTag("gw:active.1")
	subs := subWindowsForTitle(baseTitle)
	if len(subs) == 0 || (len(subs) == 1 && subs[0] == currentSub) {
		return
	}

	currentIdx := -1
	for i, s := range subs {
		if s == currentSub {
			currentIdx = i
			break
		}
	}
	nextSub := subs[(currentIdx+dir+len(subs))%len(subs)]
	if currentIdx < 0 {
		nextSub = subs[0]
	}

	showSub(nextSub, "")
	setActiveSub(&st, baseTitle, nextSub)
}

func runNextSubwindow() { runNavigateSubwindow(1) }
func runPrevSubwindow() { runNavigateSubwindow(-1) }

func runHandlePaneDead() {
	if !isPaneDead("gw:active.1") {
		return
	}
	st := loadState()
	baseTitle := st.ActiveTitle
	currentSub := paneTag("gw:active.1")
	if baseTitle == "" || currentSub == "" {
		return
	}
	subs := subWindowsForTitle(baseTitle)
	nextSub := neighbourSub(subs, currentSub)
	if nextSub == "" {
		// Last sub — leave it hanging so the pane stays visible.
		return
	}

	// Respawn as a shell before parking: a dead pane in a storage window (no
	// remain-on-exit) takes its window with it. A shell, so a sub-window started
	// with a command does not rerun it just to be closed.
	path := getWorktreePath(st, baseTitle)
	respawnArgs := []string{"respawn-pane", "-k", "-t", "gw:active.1"}
	if path != "" {
		respawnArgs = append(respawnArgs, "-c", path)
	}
	respawnArgs = append(respawnArgs, shellBin(), "-l")
	exec.Command("tmux", respawnArgs...).Run()

	parkActive()
	exec.Command("tmux", "kill-window", "-t", "gw:"+currentSub).Run()
	showSub(nextSub, path)
	setActiveSub(&st, baseTitle, nextSub)
}
