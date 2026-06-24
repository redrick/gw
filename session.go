package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ── State ─────────────────────────────────────────────────────────────────────

type Project struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type State struct {
	Projects          []Project         `json:"projects"`
	ActiveTitle       string            `json:"active_title,omitempty"`
	ActiveSub         map[string]string `json:"active_sub,omitempty"`
	LaunchDir         string            `json:"launch_dir,omitempty"`
	LaunchedFromShell bool              `json:"launched_from_shell,omitempty"`
	LaunchWorktree    string            `json:"launch_worktree,omitempty"`
	PinnedPreview     string            `json:"pinned_preview,omitempty"`
}

func statePath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "gw", "state.json")
}

// switchRequestPath holds a worktree title written by the monitor (e.g. when a
// desktop notification is clicked) requesting the sidebar switch its active
// slot to that worktree. The sidebar consumes and deletes it on its tick.
func switchRequestPath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "gw", "switch_request")
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

// previewReady is set to true once the preview column (capture pane + monitor)
// has been created. It is never torn down during a session.
var previewReady bool

// monitorScript is a self-contained Python script that renders CPU/RAM
// sparklines and all temperature sensors into the narrow (25-col) side pane.
const monitorScript = `import sys, time, json, subprocess

SPARK = " ▁▂▃▄▅▆▇█"

def spark(h, w=10):
    s = [SPARK[min(8, max(0, int(v / 100 * 8)))] for v in h[-w:]]
    return " " * (w - len(s)) + "".join(s)

def read_cpu():
    f = open("/proc/stat").readline().split()
    idle = int(f[4]) + int(f[5])
    total = sum(int(x) for x in f[1:8])
    return idle, total

def read_mem():
    d = {}
    for line in open("/proc/meminfo"):
        if ":" in line:
            k, _, v = line.partition(":")
            d[k.strip()] = int(v.split()[0])
    used = (d["MemTotal"] - d["MemAvailable"]) // 1024
    return used, d["MemTotal"] // 1024

def read_temps():
    try:
        out = subprocess.check_output(["sensors", "-j"], stderr=subprocess.DEVNULL)
        data = json.loads(out)
    except Exception:
        return []
    results = []
    for chip, sensors in data.items():
        cname = chip.rsplit("-", 2)[0]
        for sname, vals in sensors.items():
            if not isinstance(vals, dict):
                continue
            for k, v in vals.items():
                if k.endswith("_input") and k.startswith("temp") and isinstance(v, (int, float)) and v > 0:
                    results.append((cname, sname, v))
                    break
    return results

sys.stdout.write("\033[?7l")
sys.stdout.flush()

pi, pt = read_cpu()
ch, rh = [], []

while True:
    ci, ct = read_cpu()
    di, dt = ci - pi, ct - pt
    cp = 100.0 * (1 - di / dt) if dt > 0 else 0.0
    pi, pt = ci, ct
    ch.append(cp)
    ch = ch[-20:]

    um, tm = read_mem()
    rp = 100.0 * um / tm if tm else 0.0
    rh.append(rp)
    rh = rh[-20:]
    ug, tg = um / 1024, tm / 1024

    lines = [
        f"CPU {spark(ch)} {cp:4.1f}%",
        f"RAM {spark(rh)} {ug:.1f}/{tg:.0f}G",
        "─" * 20,
    ]
    for cname, sname, temp in read_temps():
        lines.append(f"{cname}/{sname}: {temp:.0f}°C")

    sys.stdout.write("\033[H")
    for line in lines:
        sys.stdout.write("\033[2K" + line[:25] + "\n")
    sys.stdout.write("\033[J")
    sys.stdout.flush()
    time.sleep(1)
`

func monitorScriptPath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "gw", "monitor.py")
}

func writeMonitorScript() string {
	p := monitorScriptPath()
	os.MkdirAll(filepath.Dir(p), 0755)
	os.WriteFile(p, []byte(monitorScript), 0644)
	return p
}

// windowsListScript renders all open tmux worktree windows grouped by project.
// Each sub-window shows a colour-coded status with no program names:
//
//	blue   current   — sub-window is visible in the main pane
//	green  ● idle    — shell is in the foreground
//	yellow ⠋ running — a program is running (animated braille spinner)
//	orange ⚠ input   — an agent (claude/codex/…) is waiting for permission
//	red    ✕ error   — pane is dead / unusable
//
// "input" detection works by scraping each running agent pane with capture-pane
// and matching known permission-prompt patterns. On the rising edge (a pane that
// was not waiting starts waiting) a desktop notification is fired via notify-send
// so the user can find which background worktree needs attention without hunting
// through tabs. Patterns can be overridden with the GW_ATTN_PATTERNS env var
// (a '|'-separated, case-insensitive substring list).
const windowsListScript = `import subprocess, sys, time, json, os, shutil, threading

SHELLS = {'bash', 'zsh', 'fish', 'sh', 'dash', 'tcsh'}
W = 24
RST = '\033[0m'
SPIN = '⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏'

# Permission-prompt markers for claude / codex and similar agents. Matched as
# case-insensitive substrings against the visible pane text.
DEFAULT_PATTERNS = [
    'do you want to proceed',
    'do you want to make this edit',
    'do you want to create',
    'do you want to run',
    'allow command',
    'allow this command',
    'approve this',
    'waiting for your approval',
    '❯ 1. yes',
]
_env = os.environ.get('GW_ATTN_PATTERNS', '')
PATTERNS = [p.strip().lower() for p in _env.split('|') if p.strip()] if _env else DEFAULT_PATTERNS

NOTIFY = shutil.which('notify-send')
# gdbus lets us actively close a still-open notification by id. Critical-urgency
# notifications never auto-expire, so we use this to dismiss them after a timeout.
CLOSER = shutil.which('gdbus')
NOTIFY_TIMEOUT = float(os.environ.get('GW_NOTIFY_TIMEOUT', '35'))
# WM activators for raising the terminal when a notification is clicked. wmctrl
# is preferred; xdotool is the fallback. Both no-op gracefully when absent.
WMCTRL = shutil.which('wmctrl') if os.environ.get('DISPLAY') else None
XDOTOOL = shutil.which('xdotool') if os.environ.get('DISPLAY') else None

def c(code, text):
    return f'\033[{code}m{text}{RST}'

def run(*args):
    try:
        return subprocess.check_output(list(args), stderr=subprocess.DEVNULL).decode().strip()
    except Exception:
        return ''

def capture(target):
    try:
        return subprocess.check_output(
            ['tmux', 'capture-pane', '-p', '-t', target],
            stderr=subprocess.DEVNULL).decode(errors='replace').lower()
    except Exception:
        return ''

def waiting_for_input(sub, active=False):
    # The active worktree's live pane is swapped into gw:active.1; its storage
    # window gw:<sub>.0 holds the displaced idle shell. Capture the right pane.
    target = 'gw:active.1' if active else 'gw:' + sub + '.0'
    txt = capture(target)
    return any(p in txt for p in PATTERNS)

def _ppid(pid):
    # Parent pid from /proc/<pid>/stat. comm field is parenthesized and may itself
    # contain spaces/parens, so parse after the final ')': fields are state, ppid...
    try:
        with open('/proc/%d/stat' % pid) as f:
            data = f.read()
        return int(data[data.rfind(')') + 1:].split()[1])
    except Exception:
        return 0

def _win_for_pid(pid):
    # Window id of the top-level window owned by pid, or None. wmctrl -lp gives a
    # pid->winid table; xdotool can search by pid directly. Returns first match.
    if WMCTRL:
        for line in run(WMCTRL, '-lp').splitlines():
            parts = line.split(None, 4)
            if len(parts) >= 3 and parts[2] == str(pid):
                return parts[0]
        return None
    if XDOTOOL:
        ids = run(XDOTOOL, 'search', '--all', '--pid', str(pid)).split()
        return ids[0] if ids else None
    return None

def _activate_win(winid):
    if WMCTRL:
        subprocess.run([WMCTRL, '-i', '-a', winid],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    elif XDOTOOL:
        subprocess.run([XDOTOOL, 'windowactivate', winid],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

def raise_terminal():
    # tmux select-window only changes the active window *inside* the session; if
    # the terminal is alt-tabbed away the user never sees the switch. Walk each
    # attached gw client's pid up its ppid chain until one maps to a top-level
    # window, then activate it so the terminal comes to the foreground.
    if not (WMCTRL or XDOTOOL):
        return
    for cp in run('tmux', 'list-clients', '-t', 'gw', '-F', '#{client_pid}').split():
        try:
            p = int(cp)
        except ValueError:
            continue
        depth = 0
        while p > 1 and depth < 40:
            wid = _win_for_pid(p)
            if wid:
                _activate_win(wid)
                return
            p = _ppid(p)
            depth += 1

def focus(sub):
    # Bring the gw session to the worktree that needs input, then raise the
    # hosting terminal in the WM. We must NOT select-window onto gw:<sub>: that
    # storage window holds the worktree's bare pane and showing it directly
    # destroys the composed gw layout (sidebar + active + preview live in
    # gw:active). Instead drop a switch request for the sidebar to consume — it
    # performs the proper server-side swap-pane — and surface gw:active.
    base = sub.split('~')[0]
    try:
        p = os.path.expanduser('~/.config/gw/switch_request')
        with open(p, 'w') as f:
            f.write(base)
    except Exception:
        pass
    try:
        subprocess.run(['tmux', 'select-window', '-t', 'gw:active'],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    except Exception:
        pass
    raise_terminal()

def notify(proj, branch, idx, sub):
    if not NOTIFY:
        return
    where = proj + '/' + branch + (' #' + idx if idx != '1' else '')
    # Run notify-send with --wait so it blocks until the notification is acted on
    # or dismissed; --action makes clicking the body emit 'default' on stdout, and
    # -p prints the notification id on the first line. Critical urgency means the
    # daemon never auto-expires it, so a timer thread closes it by id via gdbus
    # after NOTIFY_TIMEOUT seconds. We use Popen (not run) so the id is readable
    # while --wait still blocks. Runs in a daemon thread so the loop keeps drawing.
    def worker():
        try:
            p = subprocess.Popen(
                [NOTIFY, '-u', 'critical', '-a', 'gw', '--wait', '-p',
                 '--action=default=Open', '⚠ worktree needs input', where],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        except Exception:
            return
        nid = (p.stdout.readline().decode(errors='replace').strip()
               if p.stdout else '')
        timer = None
        if nid and CLOSER:
            def close():
                try:
                    subprocess.run(
                        [CLOSER, 'call', '--session',
                         '--dest', 'org.freedesktop.Notifications',
                         '--object-path', '/org/freedesktop/Notifications',
                         '--method',
                         'org.freedesktop.Notifications.CloseNotification', nid],
                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                except Exception:
                    pass
            timer = threading.Timer(NOTIFY_TIMEOUT, close)
            timer.daemon = True
            timer.start()
        # Any remaining stdout is the invoked action key; 'default' = body click.
        rest = p.stdout.read().decode(errors='replace') if p.stdout else ''
        p.wait()
        if timer:
            timer.cancel()
        if 'default' in rest.split():
            focus(sub)
    threading.Thread(target=worker, daemon=True).start()

def load_state():
    try:
        p = os.path.expanduser('~/.config/gw/state.json')
        with open(p) as f:
            return json.load(f)
    except Exception:
        return {}

def load_projects(state):
    try:
        return [pr['name'] for pr in state.get('projects', [])]
    except Exception:
        return []

sys.stdout.write('\033[?7l')
sys.stdout.flush()

tick = 0
# Per-sub attention state: {'notified': bool, 'clear': int}. We fire notify-send
# once per rising edge and must NOT re-fire while the prompt is still on screen.
# The pattern match flaps (the agent pane redraws/scrolls, so a matched line can
# briefly leave the captured viewport), so we don't re-arm the moment a match
# disappears — we require ATTN_CLEAR_FRAMES consecutive non-waiting polls first.
# Without this debounce an idle window flips waiting->clear->waiting every poll
# and notifies forever.
attn_state = {}
ATTN_CLEAR_FRAMES = 3

def attn_mark(sub):
    # Pane is waiting. Returns True only on a fresh rising edge (caller notifies).
    s = attn_state.get(sub)
    if s is None:
        s = {'notified': False, 'clear': 0}
        attn_state[sub] = s
    s['clear'] = 0
    if not s['notified']:
        s['notified'] = True
        return True
    return False

def attn_clear(sub):
    # Pane is not waiting. Re-arm only after enough consecutive clear polls so a
    # transient match dropout doesn't reset the latch and re-notify.
    s = attn_state.get(sub)
    if s is None:
        return
    s['clear'] += 1
    if s['clear'] >= ATTN_CLEAR_FRAMES:
        attn_state.pop(sub, None)

while True:
    state = load_state()
    projects = load_projects(state)
    active_title = state.get('active_title', '')
    active_subs = state.get('active_sub') or {}
    active_sub = active_subs.get(active_title, active_title)
    raw = run('tmux', 'list-windows', '-t', 'gw', '-F', '#{window_name}').splitlines()

    buckets = {}
    bucket_order = []
    for w in raw:
        w = w.strip()
        if not w.startswith('wt-'):
            continue
        base = w.split('~')[0]
        rest = base[3:]
        proj = next((p for p in projects if rest == p or rest.startswith(p + '-')), rest.split('-')[0])
        if proj not in buckets:
            buckets[proj] = {}
            bucket_order.append(proj)
        if base not in buckets[proj]:
            buckets[proj][base] = []
        buckets[proj][base].append(w)

    out = []
    any_running = False
    any_attn = False
    seen_subs = set()

    for proj in bucket_order:
        out.append(c('38;5;99;1', proj[:W]))
        for base, subs in buckets[proj].items():
            rest = base[3:]
            branch = rest[len(proj)+1:] if rest.startswith(proj + '-') else rest
            out.append(c('38;5;243', (' ' + branch)[:W]))
            for sub in subs:
                seen_subs.add(sub)
                idx = sub.split('~')[1] if '~' in sub else '1'
                if sub == active_sub:
                    indicator = c('38;5;39', 'current')
                    # Notify on every rising edge, even for the current window — you
                    # may have alt-tabbed away, and a missed prompt is worse than a
                    # redundant ping. The latch keeps it to one notify per prompt.
                    if waiting_for_input(sub, active=True):
                        any_attn = True
                        if attn_mark(sub):
                            notify(proj, branch, idx, sub)
                    else:
                        attn_clear(sub)
                else:
                    dead = run('tmux', 'display-message', '-t', 'gw:' + sub + '.0', '-p', '#{pane_dead}')
                    if dead == '1':
                        indicator = c('38;5;196', '✕ error')
                        attn_clear(sub)
                    else:
                        cmd = run('tmux', 'display-message', '-t', 'gw:' + sub + '.0', '-p', '#{pane_current_command}')
                        if cmd in SHELLS or not cmd:
                            indicator = c('38;5;82', '● idle')
                            attn_clear(sub)
                        elif waiting_for_input(sub):
                            any_attn = True
                            indicator = c('1;38;5;208', '⚠ input')
                            if attn_mark(sub):
                                notify(proj, branch, idx, sub)
                        else:
                            any_running = True
                            indicator = c('38;5;226', SPIN[tick % len(SPIN)] + ' running')
                            attn_clear(sub)
                out.append('  ' + idx + ' ' + indicator)

    # Drop state for windows that no longer exist.
    for k in list(attn_state):
        if k not in seen_subs:
            attn_state.pop(k, None)

    sys.stdout.write('\033[H')
    for l in out:
        sys.stdout.write('\033[2K' + l + '\n')
    sys.stdout.write('\033[J')
    sys.stdout.flush()

    tick += 1
    # Fast poll only to animate the running spinner. The input indicator is
    # static, so a waiting pane doesn't need sub-second capture-pane churn.
    time.sleep(0.15 if any_running else 1.0)
`

func windowsListScriptPath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "gw", "windows-list.py")
}

func writeWindowsListScript() string {
	p := windowsListScriptPath()
	os.MkdirAll(filepath.Dir(p), 0755)
	os.WriteFile(p, []byte(windowsListScript), 0644)
	return p
}

// previewColumnExists checks whether the preview column is already present in
// gw:active by counting panes. Sidebar + main + at least one preview pane = 3.
func previewColumnExists() bool {
	out, err := exec.Command("tmux", "list-panes", "-t", "gw:active", "-F", "x").Output()
	if err != nil {
		return false
	}
	return len(strings.Fields(string(out))) >= 3
}

// ensurePreviewColumn creates the right-side column exactly once: a windows
// activity list at the top (2/3) and a system monitor at the bottom (1/3).
func ensurePreviewColumn() {
	if previewReady {
		return
	}
	if previewColumnExists() {
		previewReady = true
		return
	}

	out, err := exec.Command("tmux", "split-window",
		"-h", "-t", "gw:active.1", "-l", "25", "-d",
		"-P", "-F", "#{pane_id}").Output()
	if err != nil {
		return
	}
	monitorID := strings.TrimSpace(string(out))

	hOut, _ := exec.Command("tmux", "display-message", "-t", monitorID, "-p", "#{pane_height}").Output()
	totalH, _ := strconv.Atoi(strings.TrimSpace(string(hOut)))
	listH := totalH * 2 / 3
	if listH < 6 {
		listH = 6
	}

	out, err = exec.Command("tmux", "split-window",
		"-v", "-t", monitorID, "-l", strconv.Itoa(listH), "-b", "-d",
		"-P", "-F", "#{pane_id}").Output()
	if err != nil {
		exec.Command("tmux", "kill-pane", "-t", monitorID).Run()
		return
	}
	listID := strings.TrimSpace(string(out))

	listScript := writeWindowsListScript()
	// Tag the pane so refreshMonitorPane can find and respawn it on relaunch,
	// picking up an updated script without killing the session.
	exec.Command("tmux", "set-option", "-p", "-t", listID, "@gw_pane", "list").Run()
	exec.Command("tmux", "send-keys", "-t", listID, "python3 "+listScript, "Enter").Run()
	monitorScript := writeMonitorScript()
	exec.Command("tmux", "send-keys", "-t", monitorID, "python3 "+monitorScript, "Enter").Run()

	previewReady = true
}

// refreshMonitorPane rewrites the windows-list script to disk and respawns the
// live list pane so script changes take effect on relaunch — without requiring
// the user to kill the whole gw session. No-op when the session or tagged pane
// is absent (e.g. a session created by an older binary that never tagged it).
func refreshMonitorPane() {
	if !tmuxSessionExists("gw") {
		return
	}
	script := writeWindowsListScript()
	out, err := exec.Command("tmux", "list-panes", "-t", "gw:active",
		"-F", "#{pane_id} #{@gw_pane}").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "list" {
			exec.Command("tmux", "respawn-pane", "-k", "-t", fields[0],
				"python3 "+script).Run()
			return
		}
	}
}

// updatePreviewTarget ensures the preview column is created. Called on every
// worktree switch; the windows list script refreshes itself automatically.
func updatePreviewTarget(prevTitle string, st State) {
	ensurePreviewColumn()
}

func runPinPreview() {}

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
	args = append(args, shellBin(), "-l")
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

// resizeWindowToActive pre-resizes a storage window to match gw:active.1's
// current dimensions so that swapping the pane in causes no terminal resize
// and therefore no SIGWINCH to the running process.
func resizeWindowToActive(winName string) {
	w, err := exec.Command("tmux", "display-message", "-t", "gw:active.1", "-p", "#{pane_width}").Output()
	if err != nil {
		return
	}
	h, err := exec.Command("tmux", "display-message", "-t", "gw:active.1", "-p", "#{pane_height}").Output()
	if err != nil {
		return
	}
	width := strings.TrimSpace(string(w))
	height := strings.TrimSpace(string(h))
	if width == "" || height == "" {
		return
	}
	exec.Command("tmux", "resize-window", "-t", "gw:"+winName, "-x", width, "-y", height).Run()
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

	// Match storage window size to active.1 before swapping in — prevents SIGWINCH.
	resizeWindowToActive(toSub)

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
	PRInfo string
}

func getPRInfo(path string) string {
	// Use gh to find if there's an open PR for the current branch.
	cmd := exec.Command("gh", "pr", "view", "--json", "number", "--template", `#{{.number}}`)
	cmd.Dir = path
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
				cur.PRInfo = getPRInfo(cur.Path)
				trees = append(trees, cur)
				cur = Worktree{}
			}
		}
	}
	if cur.Path != "" {
		if cur.Branch == "" {
			cur.Branch = "(unknown)"
		}
		cur.PRInfo = getPRInfo(cur.Path)
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
	args = append(args, shellBin(), "-l")
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

func addWorktree(projectPath, projectName, branch, startPoint string) (Worktree, string, error) {
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
		// Brand-new branch — create it from startPoint (or HEAD if empty).
		args := []string{"-C", projectPath, "worktree", "add", "-b", branch, wtPath}
		if startPoint != "" {
			args = append(args, startPoint)
		}
		out, err = exec.Command("git", args...).CombinedOutput()
	}
	if err != nil {
		return Worktree{}, "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}

	wt := Worktree{Path: wtPath, Branch: branch}
	title := wtWindowTitle(projectName, branch)
	ensureStorageWindow(title, wtPath)
	return wt, title, nil
}
