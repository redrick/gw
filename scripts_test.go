package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// scriptRun runs a generated Python script as a user's pane would: from a path
// with a space, with only the given fake tools on PATH.
type scriptRun struct {
	t       *testing.T
	dir     string
	fakeBin string
}

func newScriptRun(t *testing.T) *scriptRun {
	t.Helper()
	root := t.TempDir()
	r := &scriptRun{
		t:       t,
		dir:     filepath.Join(root, "App Support", "gw"),
		fakeBin: filepath.Join(root, "bin"),
	}
	for _, d := range []string{r.dir, r.fakeBin} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (r *scriptRun) fake(name, body string) {
	r.t.Helper()
	path := filepath.Join(r.fakeBin, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
		r.t.Fatal(err)
	}
}

func (r *scriptRun) env() []string {
	return []string{
		"HOME=" + r.dir,
		"PATH=" + r.fakeBin + ":/usr/bin:/bin",
		"GW_NOTIFY_TIMEOUT=30",
		"LANG=en_US.UTF-8",
	}
}

// run executes the script for the given time and returns what it drew.
func (r *scriptRun) run(script string, d time.Duration, args ...string) (stdout, stderr string) {
	r.t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		r.t.Skip("python3 not installed")
	}
	path := filepath.Join(r.dir, "script.py")
	if err := os.WriteFile(path, []byte(script), 0644); err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, append([]string{path}, args...)...)
	cmd.Env = r.env()
	cmd.WaitDelay = time.Second
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	cmd.Run()
	return out.String(), errOut.String()
}

func TestMonitorScriptDrawsCPUAndRAMOnThisPlatform(t *testing.T) {
	r := newScriptRun(t)

	stdout, stderr := r.run(monitorScript, 2500*time.Millisecond)

	if strings.Contains(stderr, "Traceback") {
		t.Fatalf("monitor crashed:\n%s", stderr)
	}
	for _, want := range []string{"CPU ", "RAM ", "G"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("monitor output has no %q:\n%q", want, stdout)
		}
	}
}

// windowsListFakes installs a tmux that reports one waiting background worktree
// (main) and one active worktree (feature), plus an `open` that only logs.
func windowsListFakes(r *scriptRun) {
	windowsListFakesShowing(r, "Do you want to proceed?")
}

// windowsListFakesShowing is windowsListFakes with the given text on the screen
// of the background worktree.
func windowsListFakesShowing(r *scriptRun, screen string) {
	screenFile := filepath.Join(r.dir, "screen.txt")
	if err := os.WriteFile(screenFile, []byte(screen), 0644); err != nil {
		r.t.Fatal(err)
	}
	r.fake("tmux", `case "$1" in
list-panes) printf 'wt-my-proj-main\t0\t0\tnode\nwt-my-proj-feature\t0\t0\tzsh\n' ;;
capture-pane) [ "$4" = "gw:wt-my-proj-main.0" ] && cat "`+screenFile+`" ;;
*) echo "$@" >> "`+r.dir+`/tmux.log" ;;
esac`)
	r.fake("open", `echo "$@" >> "`+r.dir+`/open.log"`)

	state := State{
		Projects:    []Project{{Path: "/repo", Name: "my-proj"}},
		ActiveTitle: "wt-my-proj-feature",
	}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(r.dir, "state.json"), data, 0644); err != nil {
		r.t.Fatal(err)
	}
}

func TestWindowsListReadsStateFromTheDirectoryItIsGiven(t *testing.T) {
	r := newScriptRun(t)
	windowsListFakes(r)

	stdout, stderr := r.run(windowsListScript, 1500*time.Millisecond, r.dir)

	if strings.Contains(stderr, "Traceback") {
		t.Fatalf("windows list crashed:\n%s", stderr)
	}
	if !strings.Contains(stdout, "my-proj") {
		t.Fatalf("project name not resolved from state:\n%q", stdout)
	}
	if !strings.Contains(stdout, "current") {
		t.Fatalf("active worktree not marked current:\n%q", stdout)
	}
	if !strings.Contains(stdout, "input") {
		t.Fatalf("waiting worktree not marked:\n%q", stdout)
	}
}

func TestTerminalNotifierClickSwitchesToTheWaitingWorktree(t *testing.T) {
	r := newScriptRun(t)
	windowsListFakes(r)
	argvLog := filepath.Join(r.dir, "notifier.log")
	r.fake("terminal-notifier", `for a in "$@"; do printf '%s\n' "$a" >> "`+argvLog+`"; done`)

	r.run(windowsListScript, 1500*time.Millisecond, r.dir)

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("terminal-notifier was not called: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	var execute, message string
	for i, a := range args[:len(args)-1] {
		switch a {
		case "-execute":
			execute = args[i+1]
		case "-message":
			message = args[i+1]
		}
	}
	if !strings.Contains(message, "my-proj/main") {
		t.Fatalf("message does not name the worktree: %q", message)
	}
	if execute == "" {
		t.Fatalf("no -execute command in %q", args)
	}

	click := exec.Command("/bin/sh", "-c", execute)
	click.Env = r.env()
	if out, err := click.CombinedOutput(); err != nil {
		t.Fatalf("click command failed: %v\n%s", err, out)
	}

	req, err := os.ReadFile(filepath.Join(r.dir, "switch_request"))
	if err != nil {
		t.Fatalf("click wrote no switch request: %v", err)
	}
	if string(req) != "wt-my-proj-main" {
		t.Fatalf("switch request = %q", req)
	}
	opened, _ := os.ReadFile(filepath.Join(r.dir, "open.log"))
	if !strings.Contains(string(opened), "-b com.googlecode.iterm2") {
		t.Fatalf("click did not raise iTerm2: %q", opened)
	}
}

// The Cursor agent ("agent") screens below use the texts of its 2026.10.22 bundle.
var cursorWaitScreens = map[string]string{
	"approval": ` ⬢ Shell rm -rf build
   Waiting for approval...

 Run this command?
 Not in allowlist: rm -rf build
  → Run (once) (y)
    Add Shell(rm) to allowlist (tab)
    Auto-run (shift+tab)
    Skip (esc or n)

 → Waiting for decision (y/n/p)...`,
	"question": ` Which database do you use?
  → Postgres
    SQLite

 → Answer questions (Enter to select/next, Esc to skip)`,
	"mode switch": ` Switch to Agent mode to make the changes?

 → Approve mode switch (y/n)`,
}

func TestWindowsListMarksACursorWaitAsInputAndNotifiesOnce(t *testing.T) {
	for name, screen := range cursorWaitScreens {
		t.Run(name, func(t *testing.T) {
			r := newScriptRun(t)
			windowsListFakesShowing(r, screen)
			argvLog := filepath.Join(r.dir, "notifier.log")
			r.fake("terminal-notifier", `for a in "$@"; do printf '%s\n' "$a" >> "`+argvLog+`"; done`)

			stdout, _ := r.run(windowsListScript, 1500*time.Millisecond, r.dir)

			if !strings.Contains(stdout, "input") {
				t.Fatalf("Cursor wait not marked as input:\n%q", stdout)
			}
			data, err := os.ReadFile(argvLog)
			if err != nil {
				t.Fatalf("no notification: %v", err)
			}
			if n := strings.Count(string(data), "-message\n"); n != 1 {
				t.Fatalf("%d notifications, want 1", n)
			}
		})
	}
}

func TestWindowsListKeepsAWorkingCursorAgentRunning(t *testing.T) {
	r := newScriptRun(t)
	windowsListFakesShowing(r, ` ⬢ Read README.md
 ⬢ Shell go test ./...
   … 12 output lines hidden · ctrl+o to expand

 → Add a follow-up                                  ctrl+c to stop`)
	argvLog := filepath.Join(r.dir, "notifier.log")
	r.fake("terminal-notifier", `for a in "$@"; do printf '%s\n' "$a" >> "`+argvLog+`"; done`)

	stdout, _ := r.run(windowsListScript, 1500*time.Millisecond, r.dir)

	if strings.Contains(stdout, "input") || !strings.Contains(stdout, "running") {
		t.Fatalf("a working agent is not shown as running:\n%q", stdout)
	}
	if _, err := os.Stat(argvLog); err == nil {
		t.Fatal("a working agent sent a notification")
	}
}

func TestWithoutANotifierMacOSUsesOSC9ThroughTmux(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("OSC 9 fallback is macOS only")
	}
	r := newScriptRun(t)
	windowsListFakes(r)

	stdout, _ := r.run(windowsListScript, 1500*time.Millisecond, r.dir)

	want := "\033Ptmux;\033\033]9;⚠ my-proj/main needs input\007\033\\"
	if !strings.Contains(stdout, want) {
		t.Fatalf("no OSC 9 passthrough sequence in:\n%q", stdout)
	}
	if strings.Count(stdout, "\033]9;") != 1 {
		t.Fatalf("expected exactly one notification per prompt:\n%q", stdout)
	}
}
