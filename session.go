package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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
	Visits            map[string]Visit  `json:"visits,omitempty"`
}

type Visit struct {
	Count int   `json:"count"`
	Last  int64 `json:"last"`
}

// score is frecency: visit count weighted by how recently the worktree was last opened.
func (v Visit) score(now time.Time) float64 {
	age := now.Sub(time.Unix(v.Last, 0))
	switch {
	case age < 24*time.Hour:
		return float64(v.Count) * 4
	case age < 7*24*time.Hour:
		return float64(v.Count) * 2
	case age < 30*24*time.Hour:
		return float64(v.Count)
	}
	return float64(v.Count) / 4
}

func (st *State) recordVisit(title string) {
	if st.Visits == nil {
		st.Visits = make(map[string]Visit)
	}
	v := st.Visits[title]
	v.Count++
	v.Last = time.Now().Unix()
	st.Visits[title] = v
}

// gwDir is the one location for gw's files on every platform. os.UserConfigDir is
// not used: on macOS it returns a path with a space, which the tmux commands and
// the Python scripts must then agree on and quote.
func gwDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "gw")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gw")
}

func statePath() string {
	return filepath.Join(gwDir(), "state.json")
}

// legacyStatePath is where earlier builds kept state on macOS.
func legacyStatePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "gw", "state.json")
}

// switchRequestPath holds a worktree title written by the monitor (e.g. when a
// desktop notification is clicked) requesting the sidebar switch its active
// slot to that worktree. The sidebar consumes and deletes it on its tick.
func switchRequestPath() string {
	return filepath.Join(gwDir(), "switch_request")
}

func loadState() State {
	data, err := os.ReadFile(statePath())
	if err != nil {
		data, err = os.ReadFile(legacyStatePath())
		if err != nil {
			return State{}
		}
		os.MkdirAll(gwDir(), 0755)
		os.WriteFile(statePath(), data, 0644)
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

// ── Config ────────────────────────────────────────────────────────────────────

// Keys are the key names for one action, in the bubbletea format ("ctrl+s"). The
// help text shows the first key. An empty list turns the action off.
type Keys []string

func (k Keys) Has(key string) bool {
	for _, name := range k {
		if name == key {
			return true
		}
	}
	return false
}

// Label is the short form of the first key for help text.
func (k Keys) Label() string {
	if len(k) == 0 {
		return ""
	}
	switch k[0] {
	case "up":
		return "↑"
	case "down":
		return "↓"
	case "left":
		return "←"
	case "right":
		return "→"
	case " ":
		return "space"
	}
	if rest, ok := strings.CutPrefix(k[0], "ctrl+"); ok && len(rest) == 1 {
		return "^" + rest
	}
	return k[0]
}

type TmuxKeys struct {
	FocusSidebar Keys `json:"focus_sidebar"`
	NewTab       Keys `json:"new_tab"`
	CloseTab     Keys `json:"close_tab"`
	NextTab      Keys `json:"next_tab"`
	PreviousTab  Keys `json:"previous_tab"`
}

type SidebarKeys struct {
	MoveUp         Keys `json:"move_up"`
	MoveDown       Keys `json:"move_down"`
	Open           Keys `json:"open"`
	NewWorktree    Keys `json:"new_worktree"`
	AddProject     Keys `json:"add_project"`
	RemoveWorktree Keys `json:"remove_worktree"`
	RemoveProject  Keys `json:"remove_project"`
	PRDetails      Keys `json:"pr_details"`
	CreatePR       Keys `json:"create_pr"`
	Refresh        Keys `json:"refresh"`
	Search         Keys `json:"search"`
	Help           Keys `json:"help"`
	Quit           Keys `json:"quit"`
}

type PRDetailsKeys struct {
	SwitchTab   Keys `json:"switch_tab"`
	NextComment Keys `json:"next_comment"`
	PrevComment Keys `json:"prev_comment"`
	NewComment  Keys `json:"new_comment"`
	Edit        Keys `json:"edit"`
	Save        Keys `json:"save"`
	Close       Keys `json:"close"`
}

type CreatePRKeys struct {
	NextField Keys `json:"next_field"`
	PrevField Keys `json:"prev_field"`
	Create    Keys `json:"create"`
}

// Config is the optional ~/.config/gw/config.json. Only actions that have a
// binding here can change. Enter, esc and arrows in text inputs and y/n dialogs
// stay fixed, so that a config cannot make a dialog impossible to leave.
type Config struct {
	Tmux struct {
		Prefix         string   `json:"prefix"`
		PrefixBindings TmuxKeys `json:"prefix_bindings"`
		// GlobalBindings work without the prefix.
		GlobalBindings TmuxKeys `json:"global_bindings"`
	} `json:"tmux"`
	Keys struct {
		Sidebar   SidebarKeys   `json:"sidebar"`
		PRDetails PRDetailsKeys `json:"pr_details"`
		CreatePR  CreatePRKeys  `json:"create_pr"`
	} `json:"keys"`
}

func defaultConfig() Config {
	var cfg Config
	cfg.Tmux.Prefix = "ctrl+a"
	cfg.Tmux.PrefixBindings = TmuxKeys{
		FocusSidebar: Keys{"s"},
		NewTab:       Keys{"c"},
		CloseTab:     Keys{"x"},
		NextTab:      Keys{"n"},
		PreviousTab:  Keys{"p"},
	}
	cfg.Keys.Sidebar = SidebarKeys{
		MoveUp:         Keys{"up", "k"},
		MoveDown:       Keys{"down", "j"},
		Open:           Keys{"enter", " ", "o"},
		NewWorktree:    Keys{"n"},
		AddProject:     Keys{"a"},
		RemoveWorktree: Keys{"D"},
		RemoveProject:  Keys{"d"},
		PRDetails:      Keys{"P"},
		CreatePR:       Keys{"C"},
		Refresh:        Keys{"r"},
		Search:         Keys{"/"},
		Help:           Keys{"?"},
		Quit:           Keys{"q", "ctrl+c"},
	}
	cfg.Keys.PRDetails = PRDetailsKeys{
		SwitchTab:   Keys{"tab", "right", "l", "shift+tab", "left", "h"},
		NextComment: Keys{"n"},
		PrevComment: Keys{"p"},
		NewComment:  Keys{"c"},
		Edit:        Keys{"e"},
		Save:        Keys{"ctrl+s"},
		Close:       Keys{"q", "esc", "ctrl+c"},
	}
	cfg.Keys.CreatePR = CreatePRKeys{
		NextField: Keys{"tab"},
		PrevField: Keys{"shift+tab"},
		Create:    Keys{"ctrl+s"},
	}
	return cfg
}

// loadConfig decodes config.json over the defaults, so the file holds only what
// it changes. A file with an error applies not at all: the caller gets the
// defaults and the error, which names a misspelled action.
func loadConfig() (Config, error) {
	data, err := os.ReadFile(filepath.Join(gwDir(), "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return defaultConfig(), nil
	}
	if err != nil {
		return defaultConfig(), err
	}
	cfg := defaultConfig()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return defaultConfig(), fmt.Errorf("config.json: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return cfg, nil
}

var tmuxKeyNames = map[string]string{
	"up": "Up", "down": "Down", "left": "Left", "right": "Right",
	"enter": "Enter", "tab": "Tab", "esc": "Escape", "space": "Space",
	"pgup": "PPage", "pgdown": "NPage", "home": "Home", "end": "End",
}

// tmuxKey translates a config key name to tmux's notation: "ctrl+e" to "C-e".
func tmuxKey(key string) string {
	if key == "shift+tab" {
		return "BTab"
	}
	parts := strings.Split(key, "+")
	var sb strings.Builder
	for _, mod := range parts[:len(parts)-1] {
		switch mod {
		case "ctrl":
			sb.WriteString("C-")
		case "alt":
			sb.WriteString("M-")
		case "shift":
			sb.WriteString("S-")
		}
	}
	base := parts[len(parts)-1]
	if name, ok := tmuxKeyNames[base]; ok {
		base = name
	} else if len(base) > 1 && base[0] == 'f' {
		base = "F" + base[1:]
	}
	sb.WriteString(base)
	return sb.String()
}

type tmuxAction struct {
	keys    Keys
	command string
}

// actions pairs each key list with its tmux command. self is the shell-quoted gw
// binary.
func (t TmuxKeys) actions(self string) []tmuxAction {
	shell := func(flag string) string {
		return "run-shell " + tmuxQuote(self+" "+flag)
	}
	return []tmuxAction{
		{t.FocusSidebar, "select-pane -t gw:active.0"},
		{t.NewTab, shell("--new-subwindow")},
		{t.CloseTab, shell("--close-subwindow")},
		{t.NextTab, shell("--next-subwindow")},
		{t.PreviousTab, shell("--prev-subwindow")},
	}
}

// bindTmuxKeys applies the prefix and global bindings. The default prefix keys are
// unbound first: a moved action must not leave tmux's own meaning behind, because
// next-window, for example, would show a storage window. tmux bindings apply to
// the whole server, so outside the gw session a global key goes to the program as
// usual.
func bindTmuxKeys(self string, cfg Config) {
	for _, a := range defaultConfig().Tmux.PrefixBindings.actions(self) {
		for _, key := range a.keys {
			exec.Command("tmux", "unbind-key", "-T", "prefix", tmuxKey(key)).Run()
		}
	}
	for _, a := range cfg.Tmux.PrefixBindings.actions(self) {
		for _, key := range a.keys {
			exec.Command("tmux", "bind-key", "-T", "prefix", tmuxKey(key), a.command).Run()
		}
	}
	for _, a := range cfg.Tmux.GlobalBindings.actions(self) {
		for _, key := range a.keys {
			k := tmuxKey(key)
			exec.Command("tmux", "bind-key", "-n", k, "if-shell", "-F",
				"#{==:#{session_name},gw}", a.command, "send-keys "+k).Run()
		}
	}
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
const monitorScript = `import ctypes, re, shutil, sys, time, json, subprocess

SPARK = " ▁▂▃▄▅▆▇█"

def spark(h, w=10):
    s = [SPARK[min(8, max(0, int(v / 100 * 8)))] for v in h[-w:]]
    return " " * (w - len(s)) + "".join(s)

if sys.platform == "darwin":
    LIBC = ctypes.CDLL("/usr/lib/libSystem.B.dylib")
    LIBC.mach_host_self.restype = ctypes.c_uint
    HOST = LIBC.mach_host_self()
    HOST_CPU_LOAD_INFO = 3

    def read_cpu():
        # Ticks since boot, in the order user, system, idle, nice.
        ticks = (ctypes.c_uint * 4)()
        count = ctypes.c_uint(4)
        LIBC.host_statistics(HOST, HOST_CPU_LOAD_INFO, ticks, ctypes.byref(count))
        return ticks[2], sum(ticks)

    def read_mem():
        total = ctypes.c_uint64()
        size = ctypes.c_size_t(ctypes.sizeof(total))
        LIBC.sysctlbyname(b"hw.memsize", ctypes.byref(total), ctypes.byref(size), None, 0)
        out = subprocess.check_output(["vm_stat"]).decode()
        page = int(re.search(r"page size of (\d+)", out).group(1))
        pages = {}
        for line in out.splitlines()[1:]:
            k, _, v = line.partition(":")
            pages[k.strip()] = int(v.strip().rstrip("."))
        free = pages["Pages free"] + pages["Pages speculative"] + pages["Pages inactive"]
        tm = total.value // (1024 * 1024)
        return tm - free * page // (1024 * 1024), tm
else:
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

    cols = shutil.get_terminal_size((25, 16)).columns
    sw = max(1, min(10, cols - 15))
    lines = [
        f"CPU {spark(ch, sw)} {cp:4.1f}%",
        f"RAM {spark(rh, sw)} {ug:.1f}/{tg:.0f}G",
        "─" * min(20, cols),
    ]
    for cname, sname, temp in read_temps():
        lines.append(f"{cname}/{sname}: {temp:.0f}°C")

    sys.stdout.write("\033[H")
    for line in lines:
        sys.stdout.write("\033[2K" + line[:cols] + "\n")
    sys.stdout.write("\033[J")
    sys.stdout.flush()
    time.sleep(1)
`

func monitorScriptPath() string {
	return filepath.Join(gwDir(), "monitor.py")
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
//	orange ⚠ input   — an agent (claude/codex/cursor/…) is waiting for permission
//	red    ✕ error   — pane is dead / unusable
//
// "input" detection works by scraping each running agent pane with capture-pane
// and matching known permission-prompt patterns. On the rising edge (a pane that
// was not waiting starts waiting) a desktop notification is fired so the user can
// find which background worktree needs attention without hunting through tabs. It
// uses notify-send on Linux, and terminal-notifier or iTerm2's OSC 9 escape code on
// macOS. Patterns can be overridden with the GW_ATTN_PATTERNS env var (a
// '|'-separated, case-insensitive substring list). The first argument is gw's
// directory, so that the script and the Go code read the same files.
const windowsListScript = `import subprocess, sys, time, json, os, shlex, shutil, threading

GW_DIR = sys.argv[1] if len(sys.argv) > 1 else os.path.expanduser('~/.config/gw')
SWITCH_REQUEST = os.path.join(GW_DIR, 'switch_request')

SHELLS = {'bash', 'zsh', 'fish', 'sh', 'dash', 'tcsh'}
W = 24
RST = '\033[0m'
SPIN = '⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏'

# Permission-prompt markers for claude / codex / cursor and similar agents. Matched
# as case-insensitive substrings against the visible pane text.
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
    # Cursor's "agent" shows these in its input box while it waits for you.
    'waiting for decision (y/n/p)',
    'approve mode switch (y/n)',
    'answer questions (enter to select/next, esc to skip)',
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
# macOS: terminal-notifier can run a command on click, so a click can switch the
# worktree. Without it, gw asks iTerm2 to post the notification (OSC 9), and a
# click only raises iTerm2.
MAC_NOTIFIER = shutil.which('terminal-notifier')
TERMINAL_BUNDLE = os.environ.get('__CFBundleIdentifier') or 'com.googlecode.iterm2'

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

# capture-pane is the costly call; re-scrape each pane at most once per ATTN_POLL
# seconds even while the spinner redraws faster.
ATTN_POLL = 1.0
attn_cache = {}

def waiting_for_input(sub, active=False):
    # The active worktree's live pane is swapped into gw:active.1; its storage
    # window gw:<sub>.0 holds the displaced idle shell. Capture the right pane.
    target = 'gw:active.1' if active else 'gw:' + sub + '.0'
    now = time.monotonic()
    key = (target, sub)
    hit = attn_cache.get(key)
    if hit and now - hit[0] < ATTN_POLL:
        return hit[1]
    txt = capture(target)
    waiting = any(p in txt for p in PATTERNS)
    attn_cache[key] = (now, waiting)
    return waiting

PANE_FMT = '#{window_name}\t#{pane_index}\t#{pane_dead}\t#{pane_current_command}'

def list_panes():
    # One tmux call per tick: window order plus dead/command for each storage pane.
    names, panes = [], {}
    for line in run('tmux', 'list-panes', '-s', '-t', 'gw', '-F', PANE_FMT).splitlines():
        parts = line.split('\t')
        if len(parts) != 4 or parts[1] != '0':
            continue
        names.append(parts[0])
        panes[parts[0]] = (parts[2] == '1', parts[3])
    return names, panes

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
        with open(SWITCH_REQUEST, 'w') as f:
            f.write(base)
    except Exception:
        pass
    try:
        subprocess.run(['tmux', 'select-window', '-t', 'gw:active'],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    except Exception:
        pass
    raise_terminal()

def notify_terminal_notifier(where, sub):
    # The click command runs later, outside tmux, with a minimal environment. It
    # repeats focus() with absolute paths and an explicit tmux socket.
    tmux = [shutil.which('tmux') or 'tmux']
    socket = os.environ.get('TMUX', '').split(',')[0]
    if socket:
        tmux += ['-S', socket]
    tmux += ['select-window', '-t', 'gw:active']
    click = '; '.join([
        'printf %s ' + shlex.quote(sub.split('~')[0]) + ' > ' + shlex.quote(SWITCH_REQUEST),
        ' '.join(shlex.quote(a) for a in tmux),
        'open -b ' + shlex.quote(TERMINAL_BUNDLE),
    ])
    group = 'gw-' + sub
    subprocess.Popen(
        [MAC_NOTIFIER, '-title', '⚠ worktree needs input', '-message', where,
         '-group', group, '-execute', click],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    timer = threading.Timer(NOTIFY_TIMEOUT, lambda: subprocess.run(
        [MAC_NOTIFIER, '-remove', group],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
    timer.daemon = True
    timer.start()

def notify_osc9(where):
    # tmux forwards this to the terminal only with allow-passthrough set on the
    # pane. Control characters in a branch name must not end the sequence early.
    text = ''.join(ch for ch in '⚠ ' + where + ' needs input' if ch.isprintable())
    sys.stdout.write('\033Ptmux;\033\033]9;' + text + '\007\033\\')
    sys.stdout.flush()

def notify(proj, branch, idx, sub):
    where = proj + '/' + branch + (' #' + idx if idx != '1' else '')
    if not NOTIFY:
        if MAC_NOTIFIER:
            notify_terminal_notifier(where, sub)
        elif sys.platform == 'darwin':
            notify_osc9(where)
        return
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

STATE_PATH = os.path.join(GW_DIR, 'state.json')
_state = (None, {})

def load_state():
    global _state
    try:
        mtime = os.stat(STATE_PATH).st_mtime_ns
        if mtime != _state[0]:
            with open(STATE_PATH) as f:
                _state = (mtime, json.load(f))
    except Exception:
        pass
    return _state[1]

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
    raw, panes = list_panes()

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
                    dead, cmd = panes.get(sub, (False, ''))
                    if dead:
                        indicator = c('38;5;196', '✕ error')
                        attn_clear(sub)
                    else:
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
    for k in list(attn_cache):
        if k[1] not in seen_subs:
            attn_cache.pop(k, None)

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
	return filepath.Join(gwDir(), "windows-list.py")
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

	total, _ := windowSize()
	_, column := layoutWidths(total)
	out, err := exec.Command("tmux", "split-window",
		"-h", "-t", "gw:active.1", "-l", strconv.Itoa(column), "-d",
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

	// Tag the panes so refreshMonitorPane can find and respawn them on relaunch,
	// picking up updated scripts without killing the session.
	exec.Command("tmux", "set-option", "-p", "-t", listID, "@gw_pane", "list").Run()
	startListPane(listID)
	exec.Command("tmux", "set-option", "-p", "-t", monitorID, "@gw_pane", "monitor").Run()
	startMonitorPane(monitorID)

	applyLayout()
	previewReady = true
}

// startScriptPane replaces the pane's process with a Python script. tmux runs
// separate arguments without a shell, so a path needs no quoting.
func startScriptPane(id string, args ...string) {
	exec.Command("tmux", append([]string{"respawn-pane", "-k", "-t", id, "python3"}, args...)...).Run()
}

func startListPane(id string) {
	// The list posts iTerm2 notifications with an escape code that tmux blocks
	// unless the pane allows it.
	exec.Command("tmux", "set-option", "-p", "-t", id, "allow-passthrough", "on").Run()
	startScriptPane(id, writeWindowsListScript(), gwDir())
}

func startMonitorPane(id string) {
	startScriptPane(id, writeMonitorScript())
}

// refreshMonitorPane rewrites the scripts to disk and respawns the tagged panes so
// script changes take effect on relaunch — without requiring the user to kill the
// whole gw session. No-op when the session or tagged panes are absent (e.g. a
// session created by an older binary that never tagged them).
func refreshMonitorPane() {
	if !tmuxSessionExists("gw") {
		return
	}
	out, err := exec.Command("tmux", "list-panes", "-t", "gw:active",
		"-F", "#{pane_id} #{@gw_pane}").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[1] {
		case "list":
			startListPane(fields[0])
		case "monitor":
			startMonitorPane(fields[0])
		}
	}
}

// layoutWidths gives the sidebar and right column widths for a window. Below 120
// columns both shrink, so that the main pane stays usable in a default 80-column
// terminal.
func layoutWidths(total int) (sidebar, column int) {
	if total >= 120 {
		return 40, 25
	}
	return max(20, total*3/10), max(10, total*18/100)
}

func windowSize() (width, height int) {
	out, err := exec.Command("tmux", "display-message", "-p", "-t", "gw:active",
		"#{window_width} #{window_height}").Output()
	if err != nil {
		return 0, 0
	}
	fmt.Sscan(string(out), &width, &height)
	return width, height
}

// applyLayout sets the pane sizes from the current window size. tmux scales panes
// in proportion when a client attaches or resizes, which alone would leave the
// fixed-width columns at a size that depends on when the column was created.
func applyLayout() {
	total, height := windowSize()
	if total == 0 {
		return
	}
	sidebar, column := layoutWidths(total)
	exec.Command("tmux", "resize-pane", "-t", "gw:active.0", "-x", strconv.Itoa(sidebar)).Run()

	out, err := exec.Command("tmux", "list-panes", "-t", "gw:active",
		"-F", "#{pane_id} #{@gw_pane}").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "list" {
			exec.Command("tmux", "resize-pane", "-t", fields[0],
				"-x", strconv.Itoa(column), "-y", strconv.Itoa(max(6, height*2/3))).Run()
			return
		}
	}
}

// shellQuote makes s one word for /bin/sh, which runs tmux's run-shell commands.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tmuxQuote makes s one argument in a command string that tmux parses, as in
// set-hook.
func tmuxQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`).Replace(s) + `"`
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

// tmux reads "." and ":" in a target as pane and window separators, so they
// must not appear in window names.
var titleSafe = strings.NewReplacer("/", "-", " ", "_", ".", "-", ":", "-")

func wtWindowTitle(projectName, branch string) string {
	return "wt-" + titleSafe.Replace(projectName) + "-" + titleSafe.Replace(branch)
}

// ensureStorageWindow creates a detached tmux window for the worktree if it
// does not already exist. The pane in that window preserves the worktree's
// live state (vim, servers, etc.) while another worktree is active.
func ensureStorageWindow(winName, path string) {
	if tmuxWindowExists(winName) {
		return
	}
	if newStorageWindow(winName, path, nil) == nil {
		tagPane("gw:"+winName+".0", winName)
	}
}

func newStorageWindow(winName, path string, command []string) error {
	args := []string{"new-window", "-d", "-t", "gw:", "-n", winName}
	if path != "" {
		args = append(args, "-c", path)
	}
	if len(command) > 0 {
		args = append(args, command...)
	} else {
		args = append(args, shellBin(), "-l")
	}
	if err := exec.Command("tmux", args...).Run(); err != nil {
		return err
	}
	exec.Command("tmux", "set-option", "-t", "gw:"+winName, "automatic-rename", "off").Run()
	exec.Command("tmux", "set-option", "-t", "gw:"+winName, "allow-rename", "off").Run()
	return nil
}

// Every worktree pane carries @gw_sub, the storage window it belongs to. The spare
// shell that fills the storage window of the pane on show has no tag. Panes are
// parked and fetched by tag, never by ActiveSub or window position: a storage
// window vanishing (its spare shell exited) or stale state used to swap panes into
// the wrong worktree's window.

func paneTag(target string) string {
	out, _ := exec.Command("tmux", "display-message", "-p", "-t", target, "#{@gw_sub}").Output()
	return strings.TrimSpace(string(out))
}

func tagPane(target, sub string) {
	exec.Command("tmux", "set-option", "-p", "-t", target, "@gw_sub", sub).Run()
}

func findPane(sub string) string {
	out, err := exec.Command("tmux", "list-panes", "-s", "-t", "gw",
		"-F", "#{pane_id} #{@gw_sub}").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if id, tag, ok := strings.Cut(line, " "); ok && tag == sub {
			return id
		}
	}
	return ""
}

// parkActive returns the shown pane to its storage window; the spare shell held
// there moves into the slot.
func parkActive() {
	sub := paneTag("gw:active.1")
	if sub == "" {
		return
	}
	if !tmuxWindowExists(sub) {
		newStorageWindow(sub, "", nil)
	}
	exec.Command("tmux", "swap-pane", "-s", "gw:active.1", "-t", "gw:"+sub+".0").Run()
}

// showSub parks the shown pane and brings sub's pane into the slot.
func showSub(sub, path string) {
	if paneTag("gw:active.1") == sub {
		return
	}
	parkActive()
	id := findPane(sub)
	if id == "" {
		return
	}
	if isPaneDead(id) {
		args := []string{"respawn-pane", "-k", "-t", id}
		if path != "" {
			args = append(args, "-c", path)
		}
		exec.Command("tmux", args...).Run()
	}
	resizeWindowToActive(id)
	exec.Command("tmux", "swap-pane", "-s", "gw:active.1", "-t", id).Run()
}

// adoptPanes tags panes of a session started by a gw without tagging: a storage
// window's pane is its own, except in the shown sub's window, which holds the
// spare shell while the sub's pane is in the slot.
func adoptPanes(st State) {
	shown := ""
	if st.ActiveTitle != "" {
		shown = activeSubForTitle(st, st.ActiveTitle)
	}
	out, err := exec.Command("tmux", "list-panes", "-s", "-t", "gw",
		"-F", "#{window_name}\t#{pane_index}\t#{pane_id}\t#{@gw_sub}").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || f[3] != "" {
			continue
		}
		win, idx, id := f[0], f[1], f[2]
		switch {
		case win == "active" && idx == "1":
			if shown != "" && shown != "gw-shell" {
				tagPane(id, shown)
			}
		case win == shown:
		case idx == "0" && (strings.HasPrefix(win, "wt-") || strings.HasPrefix(win, "gw-shell~")):
			tagPane(id, win)
		}
	}
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
func resizeWindowToActive(target string) {
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
	exec.Command("tmux", "resize-window", "-t", target, "-x", width, "-y", height).Run()
}

func isPaneDead(target string) bool {
	out, _ := exec.Command("tmux", "display-message", "-t", target,
		"-p", "#{pane_dead}").Output()
	return strings.TrimSpace(string(out)) == "1"
}

// switchToWindow parks the shown pane in its storage window and brings the
// target's active sub-window into the slot — all server-side. The shell item has
// no storage window of its own: showing it leaves the spare shell in the slot,
// unless a sub-window was opened on it.
func switchToWindow(toTitle, toPath string, st State) {
	toSub := activeSubForTitle(st, toTitle)
	if toTitle == "gw-shell" && toSub == toTitle {
		parkActive()
		exec.Command("tmux", "select-pane", "-t", "gw:active.1", "-T", "").Run()
		exec.Command("tmux", "select-pane", "-t", "gw:active.1").Run()
		return
	}
	if toTitle != "gw-shell" {
		ensureStorageWindow(toTitle, toPath)
	}
	showSub(toSub, toPath)
	updateStatusBar(toTitle, toSub)
	exec.Command("tmux", "select-pane", "-t", "gw:active.1").Run()
}

// ── Git worktrees ─────────────────────────────────────────────────────────────

type Worktree struct {
	Path   string
	Branch string
	PRInfo string
}

type prRef struct {
	Number      int    `json:"number"`
	HeadRefName string `json:"headRefName"`
	State       string `json:"state"`
}

func ghPRList(repoPath string, args ...string) []prRef {
	cmd := exec.Command("gh", append([]string{"pr", "list", "--state", "all",
		"--json", "number,headRefName,state"}, args...)...)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var prs []prRef
	json.Unmarshal(out, &prs)
	return prs
}

// fetchPRs maps each of branches to "#N", preferring an open PR over closed/merged
// ones. One batched gh call per repo (a per-worktree `gh pr view` costs ~0.6s each),
// then a parallel per-branch lookup only for branches older than the batch reaches.
func fetchPRs(repoPath string, branches []string) map[string]string {
	result := make(map[string]string)
	open := make(map[string]bool)
	add := func(prs []prRef) {
		for _, pr := range prs {
			isOpen := pr.State == "OPEN"
			if _, seen := result[pr.HeadRefName]; seen && (open[pr.HeadRefName] || !isOpen) {
				continue
			}
			result[pr.HeadRefName] = fmt.Sprintf("#%d", pr.Number)
			open[pr.HeadRefName] = isOpen
		}
	}
	batch := ghPRList(repoPath, "--limit", "300")
	add(batch)
	if len(batch) < 300 {
		return result
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, b := range branches {
		if _, ok := result[b]; ok {
			continue
		}
		wg.Add(1)
		go func(b string) {
			defer wg.Done()
			prs := ghPRList(repoPath, "--head", b, "--limit", "20")
			mu.Lock()
			add(prs)
			mu.Unlock()
		}(b)
	}
	wg.Wait()
	return result
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

// gitRepoRoot returns the main worktree of the repository at path, so that a
// linked worktree resolves to the repository that it belongs to. A submodule or a
// bare repository has no ".git" folder in a main worktree, so it keeps the top
// level of path.
func gitRepoRoot(path string) (string, error) {
	common, err := gitOutput(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err == nil && filepath.Base(common) == ".git" {
		return filepath.Dir(common), nil
	}
	return gitWorktreeRoot(path)
}

// gitWorktreeRoot returns the top level of the worktree that contains path.
func gitWorktreeRoot(path string) (string, error) {
	return gitOutput(path, "rev-parse", "--show-toplevel")
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

// createSubWindow opens a new sub-window running command, or a login shell when command is empty
// (`gw --new-subwindow [command...]`, used by tools such as margin).
func createSubWindow(baseTitle, path string, command []string) (string, error) {
	subs := subWindowsForTitle(baseTitle)
	n := len(subs) + 1
	newName := fmt.Sprintf("%s~%d", baseTitle, n)
	for tmuxWindowExists(newName) {
		n++
		newName = fmt.Sprintf("%s~%d", baseTitle, n)
	}
	if err := newStorageWindow(newName, path, command); err != nil {
		return "", err
	}
	tagPane("gw:"+newName+".0", newName)
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
		if !strings.HasPrefix(baseTitle, "wt-"+titleSafe.Replace(p.Name)+"-") {
			continue
		}
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
		parkActive()
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

// caseCollision returns the existing branch that differs from branch only by case,
// when the repository is on a case-insensitive filesystem (APFS by default). Git
// keeps loose refs as files there, so both names would share one ref.
func caseCollision(projectPath, branch string) string {
	if v, _ := gitOutput(projectPath, "config", "--bool", "core.ignorecase"); v != "true" {
		return ""
	}
	out, err := gitOutput(projectPath, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return ""
	}
	for _, existing := range strings.Split(out, "\n") {
		if existing != branch && strings.EqualFold(existing, branch) {
			return existing
		}
	}
	return ""
}

func addWorktree(projectPath, projectName, branch, startPoint string) (Worktree, string, error) {
	if existing := caseCollision(projectPath, branch); existing != "" {
		return Worktree{}, "", fmt.Errorf("branch %q differs only by case from %q", branch, existing)
	}
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
