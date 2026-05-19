package main

import (
	"fmt"
	"os"
	"os/exec"
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
		}
	}

	if tmuxSessionExists("gw") {
		saveState(st)
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
	exec.Command("tmux", "set-hook", "-t", "gw", "pane-dead",
		"if-shell ' #{pane_index} = 1' 'respawn-pane -k -t gw:active.1'").Run()
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
	currentSub := activeSubForTitle(st, baseTitle)

	newSub, err := createSubWindow(baseTitle, path)
	if err != nil {
		return
	}

	if tmuxWindowExists(currentSub) {
		exec.Command("tmux", "swap-pane",
			"-s", "gw:active.1",
			"-t", "gw:"+currentSub+".0").Run()
	}
	exec.Command("tmux", "swap-pane",
		"-s", "gw:active.1",
		"-t", "gw:"+newSub+".0").Run()

	if st.ActiveSub == nil {
		st.ActiveSub = make(map[string]string)
	}
	st.ActiveSub[baseTitle] = newSub
	saveState(st)
	updateStatusBar(baseTitle, newSub)
	exec.Command("tmux", "select-pane", "-t", "gw:active.1").Run()
}

func runCloseSubwindow() {
	st := loadState()
	baseTitle := st.ActiveTitle
	if baseTitle == "" {
		return
	}
	currentSub := activeSubForTitle(st, baseTitle)
	subs := subWindowsForTitle(baseTitle)
	if len(subs) == 1 {
		// If the current subwindow is the only one left, closing it should return us to the shell.
		// We need to swap the shell back from the storage window to active.1 before killing the window.
		if currentSub != baseTitle {
			exec.Command("tmux", "swap-pane",
				"-s", "gw:active.1",
				"-t", "gw:"+currentSub+".0").Run()
			exec.Command("tmux", "kill-window", "-t", "gw:"+currentSub).Run()

			if st.ActiveSub != nil {
				delete(st.ActiveSub, baseTitle)
			}
			saveState(st)
			updateStatusBar(baseTitle, "")
			exec.Command("tmux", "select-pane", "-t", "gw:active.1").Run()
		}
		return
	}

	var nextSub string
	for i, s := range subs {
		if s == currentSub {
			if i+1 < len(subs) {
				nextSub = subs[i+1]
			} else {
				nextSub = subs[i-1]
			}
			break
		}
	}
	if nextSub == "" {
		nextSub = subs[0]
	}

	exec.Command("tmux", "swap-pane",
		"-s", "gw:active.1",
		"-t", "gw:"+nextSub+".0").Run()
	exec.Command("tmux", "kill-window", "-t", "gw:"+currentSub).Run()

	if st.ActiveSub == nil {
		st.ActiveSub = make(map[string]string)
	}
	st.ActiveSub[baseTitle] = nextSub
	saveState(st)
	updateStatusBar(baseTitle, nextSub)
	exec.Command("tmux", "select-pane", "-t", "gw:active.1").Run()
}

func runNavigateSubwindow(dir int) {
	st := loadState()
	baseTitle := st.ActiveTitle
	if baseTitle == "" {
		return
	}
	currentSub := activeSubForTitle(st, baseTitle)
	subs := subWindowsForTitle(baseTitle)
	if len(subs) <= 1 {
		return
	}

	currentIdx := 0
	for i, s := range subs {
		if s == currentSub {
			currentIdx = i
			break
		}
	}
	nextIdx := (currentIdx + dir + len(subs)) % len(subs)
	nextSub := subs[nextIdx]

	exec.Command("tmux", "swap-pane",
		"-s", "gw:active.1",
		"-t", "gw:"+currentSub+".0").Run()
	exec.Command("tmux", "swap-pane",
		"-s", "gw:active.1",
		"-t", "gw:"+nextSub+".0").Run()

	if st.ActiveSub == nil {
		st.ActiveSub = make(map[string]string)
	}
	st.ActiveSub[baseTitle] = nextSub
	saveState(st)
	updateStatusBar(baseTitle, nextSub)
	exec.Command("tmux", "select-pane", "-t", "gw:active.1").Run()
}

func runNextSubwindow() { runNavigateSubwindow(1) }
func runPrevSubwindow() { runNavigateSubwindow(-1) }
