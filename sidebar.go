package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── Styles ────────────────────────────────────────────────────────────────────

var (
	activeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	sectionStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Bold(true)
	normalStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("86"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	branchStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Italic(true)
	prStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	helpStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

// ── Dog animation ─────────────────────────────────────────────────────────────

type dogTickMsg struct{}

var dogIdleFrames = [2]string{
	"    / \\__\n   (    @\\___\n   /         o\n  /   (_____/\n /_____/   U",
	"    / \\__\n   (    @\\___\n   /         O\n  /   (_____/\n /_____/   U",
}

var dogActiveFrames = [4]string{
	"    / \\__\n   (    @\\___\n   /         O\n  /   (_____/\n /_____/  ~U",
	"    / \\__\n   (    @\\___\n   /        ~O\n  /   (_____/\n /_____/  ~U",
	"    / \\__\n   (    @\\___\n   /         O\n  /   (_____/\n /_____/   U",
	"    / \\__\n   (    @\\___\n   /        ~O\n  /   (_____/\n /_____/   U",
}

const dogBarkFrame = "    / \\__\n   (    @\\___\n   /         O  Woof!\n  /   (_____/\n /_____/   U"

const dogLookFrame = "    / \\__\n   (    @\\___\n   /    O\n  /   (_____/\n /_____/   U"

func dogTickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return dogTickMsg{} })
}

// ── List items ────────────────────────────────────────────────────────────────

type listItem struct {
	isHeader  bool
	isShell   bool // launch dir entry — not a worktree
	project   Project
	worktree  Worktree
	title     string // The unique identifier (e.g. wt-project-branch)
	branch    string // The human-readable branch name
	display   string // Friendly worktree display name
	shellPath string
}

func buildItems(st State) []listItem {
	var items []listItem
	for _, p := range st.Projects {
		items = append(items, listItem{isHeader: true, project: p})
		trees, err := listWorktrees(p.Path)
		if err != nil {
			continue
		}
		for _, wt := range trees {
			items = append(items, listItem{
				project:  p,
				worktree: wt,
				title:    wtWindowTitle(p.Name, wt.Branch),
				branch:   wt.Branch,
				display:  friendlyWorktreeName(p, wt),
			})
		}
	}
	if st.LaunchDir != "" {
		items = append(items, listItem{
			isShell:   true,
			title:     "gw-shell",
			shellPath: st.LaunchDir,
		})
	}
	return items
}

// ── Model ─────────────────────────────────────────────────────────────────────

type sidebarView int

const (
	viewList sidebarView = iota
	viewCreate
	viewAddProject
	viewConfirm
	viewPR
)

type pendingKind int

const (
	pendingNone pendingKind = iota
	pendingRemoveWorktree
	pendingRemoveProject
)

type worktreeAdded struct {
	wt    Worktree
	title string
	proj  Project
	err   error
}

type projectAdded struct {
	path string
	err  error
}

type worktreeRemoved struct {
	title     string
	wasActive bool
	err       error
}

type switchedMsg struct {
	title string
}

type prLoadedMsg struct {
	content string
}

type prPopupDoneMsg struct {
	err error
}

type sidebarModel struct {
	items       []listItem
	cursor      int
	view        sidebarView
	input       textinput.Model
	inErr       string
	state       State
	width       int
	height      int
	windows     map[string]bool
	ready       bool
	pending     pendingKind
	pendingItem listItem
	prContent   string
	dogFrame    int
	dogLastKey  time.Time
	dogLastBark time.Time
	dogBarking  bool
	dogBarkEnd  time.Time
	dogLooking  bool
	dogLookEnd  time.Time
	dogNextLook time.Time
}

func newSidebarModel() sidebarModel {
	ti := textinput.New()
	ti.Placeholder = "branch-name"
	ti.CharLimit = 100

	st := loadState()
	items := buildItems(st)

	cursor := 0
	if st.LaunchedFromShell {
		// Default to the shell item when launched from outside a git repo.
		for i, it := range items {
			if it.isShell {
				cursor = i
				break
			}
		}
	} else {
		// Otherwise restore last active worktree, or fall back to first item.
		for i, it := range items {
			if !it.isHeader {
				if cursor == 0 {
					cursor = i
				}
				if it.title == st.ActiveTitle {
					cursor = i
					break
				}
			}
		}
	}

	now := time.Now()
	return sidebarModel{items: items, cursor: cursor, input: ti, state: st, windows: liveWindows(), dogLastKey: now, dogLastBark: now, dogNextLook: now.Add(5 * time.Second)}
}

func runSidebar() {
	env := append(os.Environ(), "COLORFGBG=15;0")
	p := tea.NewProgram(newSidebarModel(),
		tea.WithAltScreen(),
		tea.WithEnvironment(env),
	)
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "gw sidebar:", err)
	}
	exec.Command("tmux", "kill-session", "-t", "gw").Run()
}

// ── Navigation helpers ────────────────────────────────────────────────────────

func (m *sidebarModel) prevItem() {
	for i := m.cursor - 1; i >= 0; i-- {
		if !m.items[i].isHeader {
			m.cursor = i
			return
		}
	}
}

func (m *sidebarModel) nextItem() {
	for i := m.cursor + 1; i < len(m.items); i++ {
		if !m.items[i].isHeader {
			m.cursor = i
			return
		}
	}
}

func (m *sidebarModel) currentItem() *listItem {
	if m.cursor < len(m.items) && !m.items[m.cursor].isHeader {
		return &m.items[m.cursor]
	}
	return nil
}

func (m *sidebarModel) refresh() {
	m.state = loadState()
	oldCursor := m.cursor
	m.items = buildItems(m.state)
	if oldCursor < len(m.items) {
		m.cursor = oldCursor
	}
	// Make sure cursor is on a worktree item
	if m.cursor < len(m.items) && m.items[m.cursor].isHeader {
		m.nextItem()
	}
}

// ── bubbletea ─────────────────────────────────────────────────────────────────

func (m sidebarModel) Init() tea.Cmd { return dogTickCmd(800 * time.Millisecond) }

func (m sidebarModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tea.KeyMsg); ok {
		wasActive := time.Since(m.dogLastKey) < 3*time.Second
		m.dogLastKey = time.Now()
		if !wasActive {
			m.dogNextLook = time.Now().Add(5 * time.Second)
		}
	}

	switch msg := msg.(type) {
	case dogTickMsg:
		now := time.Now()
		if m.dogBarking {
			if now.After(m.dogBarkEnd) {
				m.dogBarking = false
				m.dogLastBark = now
			}
			return m, dogTickCmd(100 * time.Millisecond)
		}
		active := now.Sub(m.dogLastKey) < 3*time.Second
		if !active && now.Sub(m.dogLastKey) >= 30*time.Second && now.Sub(m.dogLastBark) >= 30*time.Second {
			m.dogBarking = true
			m.dogBarkEnd = now.Add(700 * time.Millisecond)
			return m, dogTickCmd(100 * time.Millisecond)
		}
		if active {
			if m.dogLooking {
				if now.After(m.dogLookEnd) {
					m.dogLooking = false
					m.dogNextLook = now.Add(time.Duration(3+int(now.UnixNano()/1e9%5)) * time.Second)
				}
			} else if now.After(m.dogNextLook) {
				m.dogLooking = true
				m.dogLookEnd = now.Add(700 * time.Millisecond)
			}
			m.dogFrame = (m.dogFrame + 1) % len(dogActiveFrames)
			return m, dogTickCmd(150 * time.Millisecond)
		}
		m.dogLooking = false
		m.dogFrame = (m.dogFrame + 1) % len(dogIdleFrames)
		return m, dogTickCmd(800 * time.Millisecond)
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if !m.ready {
			m.ready = true
			if it := m.currentItem(); it != nil {
				return m, m.doSwitch(*it)
			}
		}
	case switchedMsg:
		// Reload full state so m.state.ActiveSub stays in sync with file changes
		// made by sub-window commands between switches.
		m.state = loadState()
		m.windows = liveWindows()
		return m, nil
	case worktreeAdded:
		if msg.err != nil {
			m.inErr = msg.err.Error()
			return m, nil
		}
		// Add the new worktree to items and switch to it
		item := listItem{project: msg.proj, worktree: msg.wt, title: msg.title, branch: msg.wt.Branch, display: friendlyWorktreeName(msg.proj, msg.wt)}
		// Insert after the last item of this project
		inserted := false
		for i := len(m.items) - 1; i >= 0; i-- {
			if !m.items[i].isHeader && m.items[i].project.Path == msg.proj.Path {
				newItems := make([]listItem, 0, len(m.items)+1)
				newItems = append(newItems, m.items[:i+1]...)
				newItems = append(newItems, item)
				newItems = append(newItems, m.items[i+1:]...)
				m.items = newItems
				m.cursor = i + 1
				inserted = true
				break
			}
		}
		if !inserted {
			m.items = append(m.items, item)
			m.cursor = len(m.items) - 1
		}
		m.view = viewList
		m.input.Blur()
		m.windows = liveWindows()
		return m, m.doSwitch(item)
	case projectAdded:
		if msg.err != nil {
			m.inErr = msg.err.Error()
			return m, nil
		}
		m.state = loadState()
		m.items = buildItems(m.state)
		for i, it := range m.items {
			if !it.isHeader && it.project.Path == msg.path {
				m.cursor = i
				break
			}
		}
		m.view = viewList
		m.input.Blur()
		m.inErr = ""
		return m, nil
	case worktreeRemoved:
		if msg.err != nil {
			m.inErr = msg.err.Error()
			return m, nil
		}
		st := loadState()
		if st.ActiveSub != nil {
			delete(st.ActiveSub, msg.title)
		}
		if st.ActiveTitle == msg.title {
			st.ActiveTitle = ""
		}
		saveState(st)
		m.state = st
		// Remove the item from the list.
		for i, it := range m.items {
			if !it.isHeader && !it.isShell && it.title == msg.title {
				m.items = append(m.items[:i], m.items[i+1:]...)
				if m.cursor >= len(m.items) {
					m.cursor = len(m.items) - 1
				}
				break
			}
		}
		if m.cursor < len(m.items) && m.items[m.cursor].isHeader {
			m.nextItem()
		}
		m.windows = liveWindows()
		if msg.wasActive {
			if it := m.currentItem(); it != nil {
				return m, m.doSwitch(*it)
			}
		}
		return m, nil
	case prLoadedMsg:
		m.prContent = msg.content
		return m, nil
	case prPopupDoneMsg:
		if msg.err != nil {
			m.inErr = msg.err.Error()
		}
		return m, nil
	}

	switch m.view {
	case viewPR:
		return m.updatePR(msg)
	case viewCreate:
		return m.updateCreate(msg)
	case viewAddProject:
		return m.updateAddProject(msg)
	case viewConfirm:
		return m.updateConfirm(msg)
	default:
		return m.updateList(msg)
	}
}

func (m *sidebarModel) updatePR(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q":
			m.view = viewList
			m.prContent = ""
			return m, nil
		}
	}
	return m, nil
}

func (m sidebarModel) viewPR() string {
	var sb strings.Builder
	sb.WriteString(sectionStyle.Render("Pull Request Information") + "\n")
	sb.WriteString(dimStyle.Render(strings.Repeat("─", m.width-1)) + "\n")
	if m.prContent == "" {
		sb.WriteString(dimStyle.Render("No information available.") + "\n")
	} else {
		sb.WriteString(m.prContent + "\n")
	}
	sb.WriteString("\n" + helpStyle.Render("esc  close"))
	return sb.String()
}

func (m sidebarModel) updateList(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		m.inErr = ""
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "up", "k":
			m.prevItem()
		case "down", "j":
			m.nextItem()
		case "enter", " ", "o":
			if it := m.currentItem(); it != nil {
				return m, m.doSwitch(*it)
			}
		case "P":
			if it := m.currentItem(); it != nil && !it.isShell && it.worktree.PRInfo != "" {
				path := it.worktree.Path
				return m, openPRPopup(path)
			}
		case "C":
			if it := m.currentItem(); it != nil && !it.isShell {
				if it.worktree.PRInfo != "" {
					m.inErr = "PR already exists: " + it.worktree.PRInfo
					return m, nil
				}
				path := it.worktree.Path
				return m, openCreatePRPopup(path)
			}
		case "n":
			if it := m.currentItem(); it != nil && !it.isShell {
				m.view = viewCreate
				m.inErr = ""
				m.input.SetValue("")
				m.input.Placeholder = "branch-name"
				m.input.Focus()
				return m, textinput.Blink
			}
		case "a":
			m.view = viewAddProject
			m.inErr = ""
			m.input.SetValue("")
			m.input.Placeholder = "/path/to/repo"
			m.input.Focus()
			return m, textinput.Blink
		case "D":
			if it := m.currentItem(); it != nil && !it.isShell {
				if it.worktree.Path == it.project.Path {
					m.inErr = "cannot remove main worktree"
					return m, nil
				}
				m.pending = pendingRemoveWorktree
				m.pendingItem = *it
				m.view = viewConfirm
				return m, nil
			}
		case "d":
			if it := m.currentItem(); it != nil && !it.isShell {
				m.pending = pendingRemoveProject
				m.pendingItem = *it
				m.view = viewConfirm
				return m, nil
			}
		case "r":
			m.refresh()
			m.windows = liveWindows()
		}
	}
	return m, nil
}

func (m sidebarModel) updateCreate(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.view = viewList
			m.input.Blur()
			return m, nil
		case "enter":
			branch := strings.TrimSpace(m.input.Value())
			if branch == "" {
				return m, nil
			}
			it := m.currentItem()
			if it == nil {
				return m, nil
			}
			proj := it.project
			return m, func() tea.Msg {
				wt, title, err := addWorktree(proj.Path, proj.Name, branch)
				return worktreeAdded{wt: wt, title: title, proj: proj, err: err}
			}
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m sidebarModel) updateAddProject(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.view = viewList
			m.input.Blur()
			m.inErr = ""
			return m, nil
		case "tab":
			if completed := tabComplete(m.input.Value()); completed != m.input.Value() {
				m.input.SetValue(completed)
				m.input.CursorEnd()
			}
			return m, nil
		case "enter":
			raw := strings.TrimSpace(m.input.Value())
			if raw == "" {
				return m, nil
			}
			return m, func() tea.Msg {
				path := expandHome(raw)
				if !isGitRepo(path) {
					return projectAdded{err: fmt.Errorf("not a git repository: %s", path)}
				}
				root, err := gitRepoRoot(path)
				if err != nil {
					return projectAdded{err: err}
				}
				st := loadState()
				for _, p := range st.Projects {
					if p.Path == root {
						return projectAdded{err: fmt.Errorf("already tracked: %s", filepath.Base(root))}
					}
				}
				st.AddProject(root)
				saveState(st)
				return projectAdded{path: root}
			}
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m sidebarModel) updateConfirm(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", "n":
			m.view = viewList
			m.pending = pendingNone
			return m, nil
		case "y":
			switch m.pending {
			case pendingRemoveWorktree:
				it := m.pendingItem
				isActive := it.title == m.state.ActiveTitle
				proj := it.project
				wt := it.worktree
				title := it.title
				st := m.state
				m.view = viewList
				m.pending = pendingNone
				return m, func() tea.Msg {
					err := removeWorktree(proj.Path, wt.Path, title, isActive, st)
					return worktreeRemoved{title: title, wasActive: isActive, err: err}
				}
			case pendingRemoveProject:
				proj := m.pendingItem.project
				activeInRemoved := false
				for _, item := range m.items {
					if !item.isHeader && !item.isShell && item.title == m.state.ActiveTitle && item.project.Path == proj.Path {
						activeInRemoved = true
						break
					}
				}
				st := loadState()
				st.RemoveProject(proj.Path)
				saveState(st)
				m.state = st
				m.items = buildItems(st)
				m.windows = liveWindows()
				m.view = viewList
				m.pending = pendingNone
				if len(m.items) == 0 {
					m.cursor = 0
				} else {
					if m.cursor >= len(m.items) {
						m.cursor = len(m.items) - 1
					}
					found := false
					for i := m.cursor; i < len(m.items); i++ {
						if !m.items[i].isHeader {
							m.cursor = i
							found = true
							break
						}
					}
					if !found {
						for i := m.cursor - 1; i >= 0; i-- {
							if !m.items[i].isHeader {
								m.cursor = i
								break
							}
						}
					}
				}
				if activeInRemoved {
					if first := m.currentItem(); first != nil {
						return m, m.doSwitch(*first)
					}
				}
				return m, nil
			}
		}
	}
	return m, nil
}

func (m sidebarModel) viewConfirm() string {
	var sb strings.Builder
	switch m.pending {
	case pendingRemoveWorktree:
		sb.WriteString(errStyle.Render("remove worktree?") + "\n\n")
		sb.WriteString(dimStyle.Render("branch: ") + normalStyle.Render(m.pendingItem.worktree.Branch) + "\n")
		sb.WriteString(dimStyle.Render("path:   ") + dimStyle.Render(truncate(m.pendingItem.worktree.Path, m.width-9)) + "\n")
	case pendingRemoveProject:
		sb.WriteString(errStyle.Render("remove project?") + "\n\n")
		sb.WriteString(dimStyle.Render("project: ") + normalStyle.Render(m.pendingItem.project.Name) + "\n")
		sb.WriteString(dimStyle.Render("path:    ") + dimStyle.Render(truncate(m.pendingItem.project.Path, m.width-10)) + "\n")
	}
	sb.WriteString("\n" + helpStyle.Render("y  confirm   n / esc  cancel"))
	return sb.String()
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func tabComplete(raw string) string {
	path := expandHome(raw)

	var dir, prefix string
	switch {
	case path == "":
		dir, prefix = ".", ""
	case strings.HasSuffix(path, "/"):
		dir, prefix = path, ""
	default:
		dir, prefix = filepath.Dir(path), filepath.Base(path)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return raw
	}

	var matches []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			matches = append(matches, filepath.Join(dir, e.Name())+"/")
		}
	}

	if len(matches) == 0 {
		return raw
	}

	result := matches[0]
	for _, m := range matches[1:] {
		result = commonPrefix(result, m)
	}

	return reTilde(result)
}

func reTilde(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == home || path == home+"/" {
		return "~/"
	}
	if strings.HasPrefix(path, home+"/") {
		return "~/" + path[len(home)+1:]
	}
	return path
}

func commonPrefix(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return a[:i]
		}
	}
	return a[:n]
}

func (m sidebarModel) doSwitch(it listItem) tea.Cmd {
	from := m.state.ActiveTitle
	title := it.title
	path := it.worktree.Path
	if it.isShell {
		path = it.shellPath
	}
	return func() tea.Msg {
		// Reload from file so we have the latest ActiveSub written by sub-window
		// commands (which run as separate processes and don't update m.state).
		st := loadState()
		switchToWindow(from, title, path, st)
		st.ActiveTitle = title
		saveState(st)
		return switchedMsg{title: title}
	}
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m sidebarModel) View() string {
	switch m.view {
	case viewCreate:
		return m.viewCreate()
	case viewAddProject:
		return m.viewAddProject()
	case viewConfirm:
		return m.viewConfirm()
	case viewPR:
		return m.viewPR()
	default:
		return m.viewList()
	}
}

func (m sidebarModel) dogView() string {
	var frame string
	if m.dogBarking {
		frame = dogBarkFrame
	} else if m.dogLooking {
		frame = dogLookFrame
	} else if time.Since(m.dogLastKey) < 3*time.Second {
		frame = dogActiveFrames[m.dogFrame%len(dogActiveFrames)]
	} else {
		frame = dogIdleFrames[m.dogFrame%len(dogIdleFrames)]
	}
	return dimStyle.Render(centerDogArt(frame, m.width))
}

func centerDogArt(art string, width int) string {
	if width <= 0 {
		return art
	}
	lines := strings.Split(art, "\n")
	maxW := 0
	for _, l := range lines {
		if lw := runeLen(l); lw > maxW {
			maxW = lw
		}
	}
	pad := (width - maxW) / 2
	if pad <= 0 {
		return art
	}
	prefix := strings.Repeat(" ", pad)
	var sb strings.Builder
	for i, l := range lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(prefix + l)
	}
	return sb.String()
}

func (m sidebarModel) viewList() string {
	w := m.width
	if w < 10 {
		w = 30
	}

	var sb strings.Builder
	sb.WriteString(m.dogView() + "\n")
	sb.WriteString(dimStyle.Render(strings.Repeat("─", w-1)) + "\n")
	sb.WriteString(sectionStyle.Render("worktrees") + "\n")
	sb.WriteString(dimStyle.Render(strings.Repeat("─", w-1)) + "\n")
	if m.inErr != "" {
		sb.WriteString(errStyle.Render("! "+truncate(m.inErr, w-3)) + "\n")
	}

	for i, it := range m.items {
		if it.isHeader {
			name := truncate(it.project.Name, w-2)
			sb.WriteString("\n" + sectionStyle.Render(name) + "\n")
			continue
		}

		isActive := it.title == m.state.ActiveTitle
		isCursor := i == m.cursor
		hasWindow := m.windows[it.title]

		if it.isShell {
			sb.WriteString("\n" + dimStyle.Render(strings.Repeat("─", w-1)) + "\n")
			sb.WriteString(dimStyle.Render("shell") + "\n")
			display := truncate(it.shellPath, w-4)
			var line string
			switch {
			case isCursor && isActive:
				line = activeStyle.Render("▶ " + display + " ●")
			case isCursor:
				line = activeStyle.Render("▶ " + display)
			case isActive:
				line = normalStyle.Render("  " + display + " ●")
			default:
				line = dimStyle.Render("  " + display)
			}
			sb.WriteString(line + "\n")
			continue
		}

		var marker string
		switch {
		case isActive:
			marker = " ●"
		case hasWindow:
			marker = " ◦"
		}

		display := it.display
		if display == "" {
			display = friendlyWorktreeName(it.project, it.worktree)
		}
		title := truncate(display, w-4-len(marker))
		titleLine := ""
		switch {
		case isCursor && isActive:
			titleLine = activeStyle.Render("▶ " + title + marker)
		case isCursor:
			titleLine = activeStyle.Render("▶ " + title + marker)
		case isActive:
			titleLine = normalStyle.Render("  " + title + marker)
		case hasWindow:
			titleLine = normalStyle.Render("  " + title + marker)
		default:
			titleLine = dimStyle.Render("  " + title)
		}

		sb.WriteString(titleLine + "\n")
		if it.branch != "" {
			sb.WriteString(formatBranchLine(it.branch, it.worktree.PRInfo, w) + "\n")
		}
		continue
	}

	if len(m.items) == 0 {
		sb.WriteString(dimStyle.Render("  no projects tracked\n"))
		sb.WriteString(dimStyle.Render("  run gw from a git repo\n"))
	}

	content := sb.String()

	div := dimStyle.Render(strings.Repeat("─", w-1))
	footer := div + "\n" +
		sectionStyle.Render("gw") + "\n" +
		helpStyle.Render("↑↓ / k j  move") + "\n" +
		helpStyle.Render("enter / o  open") + "\n" +
		helpStyle.Render("n  new worktree") + "\n" +
		helpStyle.Render("a  add project") + "\n" +
		helpStyle.Render("D  rm worktree") + "\n" +
		helpStyle.Render("d  rm project") + "\n" +
		helpStyle.Render("P  PR details") + "\n" +
		helpStyle.Render("C  create PR") + "\n" +
		helpStyle.Render("r  refresh") + "\n" +
		helpStyle.Render("q  quit") + "\n" +
		div + "\n" +
		sectionStyle.Render("tmux") + "\n" +
		helpStyle.Render("^a c  new") + "\n" +
		helpStyle.Render("^a n  next") + "\n" +
		helpStyle.Render("^a p  prev") + "\n" +
		helpStyle.Render("^a s  sidebar") + "\n" +
		helpStyle.Render("^a [  scroll mode")

	contentLines := strings.Count(content, "\n")
	footerLines := strings.Count(footer, "\n") + 1
	h := m.height
	if h < 10 {
		h = 40
	}
	gap := h - contentLines - footerLines
	if gap < 1 {
		gap = 1
	}
	return content + strings.Repeat("\n", gap) + footer
}

func (m sidebarModel) viewCreate() string {
	var sb strings.Builder
	sb.WriteString(sectionStyle.Render("new worktree") + "\n\n")
	if it := m.currentItem(); it != nil {
		sb.WriteString(dimStyle.Render("repo:   ") + normalStyle.Render(truncate(it.project.Name, m.width-9)) + "\n")
		sb.WriteString(dimStyle.Render("path:   ") + dimStyle.Render(truncate(it.project.Path, m.width-9)) + "\n\n")
	}
	sb.WriteString("branch: " + m.input.View() + "\n")
	if m.inErr != "" {
		sb.WriteString("\n" + errStyle.Render(truncate(m.inErr, m.width-2)) + "\n")
	}
	sb.WriteString("\n" + helpStyle.Render("enter  create   esc  cancel"))
	return sb.String()
}

func (m sidebarModel) viewAddProject() string {
	var sb strings.Builder
	sb.WriteString(sectionStyle.Render("add project") + "\n\n")
	sb.WriteString("path: " + m.input.View() + "\n")
	if m.inErr != "" {
		sb.WriteString("\n" + errStyle.Render(truncate(m.inErr, m.width-2)) + "\n")
	}
	sb.WriteString("\n" + helpStyle.Render("enter  add   esc  cancel"))
	return sb.String()
}

func friendlyWorktreeName(p Project, wt Worktree) string {
	if wt.Path == p.Path {
		return p.Name
	}
	name := filepath.Base(wt.Path)
	prefix := p.Name + "-"
	if strings.HasPrefix(name, prefix) {
		return strings.TrimPrefix(name, prefix)
	}
	return name
}

func formatBranchLine(branch, prInfo string, width int) string {
	leftWidth := width - 1
	if leftWidth < 1 {
		leftWidth = 1
	}
	prefix := "    "
	if prInfo == "" {
		return branchStyle.Render(truncate(prefix+branch, leftWidth))
	}

	right := prStyle.Render(prInfo)
	rightWidth := lipgloss.Width(prInfo)
	gapWidth := leftWidth - len(prefix) - rightWidth
	if gapWidth < 1 {
		gapWidth = 1
	}
	branchText := truncate(branch, gapWidth-1)
	left := branchStyle.Render(prefix + branchText)
	spaces := leftWidth - lipgloss.Width(prefix+branchText) - rightWidth
	if spaces < 1 {
		spaces = 1
	}
	return left + strings.Repeat(" ", spaces) + right
}

func openPRPopup(path string) tea.Cmd {
	return func() tea.Msg {
		bin, err := os.Executable()
		if err != nil {
			return prPopupDoneMsg{err: err}
		}
		cmd := exec.Command("tmux", "display-popup", "-t", "gw:active.1", "-w", "90%", "-h", "90%", "-E", bin, "--pr-details", path)
		if err := cmd.Run(); err != nil {
			return prPopupDoneMsg{err: err}
		}
		return prPopupDoneMsg{}
	}
}

type prEditMode int

const (
	prEditNone prEditMode = iota
	prEditNewComment
	prEditDescription
	prEditComment
)

type prComment struct {
	ID     string `json:"id"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

type prData struct {
	ID                                       string `json:"id"`
	Number                                   int    `json:"number"`
	Title, State, URL, Body, CreatedAt       string
	BaseRefName, HeadRefName, ReviewDecision string
	Author                                   struct {
		Login string `json:"login"`
	} `json:"author"`
	Comments []prComment `json:"comments"`
}

type prDetailsModel struct {
	path     string
	loading  bool
	tab      int
	width    int
	height   int
	pr       prData
	diff     string
	diffTool string
	err      string
	status   string
	selected int // 0 = description, 1..n = comments
	editing  prEditMode
	editIdx  int
	editor   textarea.Model
	viewport viewport.Model
}

type prDetailsLoadedMsg struct {
	pr       prData
	diff     string
	diffTool string
	err      error
}

type prSavedMsg struct{ err error }

func runPRDetails(path string) {
	ta := textarea.New()
	ta.Placeholder = "Write a PR comment…"
	ta.ShowLineNumbers = false
	ta.Focus()
	p := tea.NewProgram(prDetailsModel{path: path, loading: true, editor: ta}, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "gw pr:", err)
	}
}

func (m prDetailsModel) Init() tea.Cmd { return loadPRDetails(m.path) }

func (m prDetailsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewport.Width = max(20, msg.Width-4)
		m.viewport.Height = max(3, msg.Height-7)
		m.editor.SetWidth(max(20, msg.Width-4))
		m.editor.SetHeight(max(5, msg.Height-9))
		m.viewport.SetContent(m.currentPRBody())
	case tea.KeyMsg:
		if m.editing != prEditNone {
			switch msg.String() {
			case "ctrl+c", "esc":
				m.editing = prEditNone
				m.status = "edit cancelled"
				m.viewport.SetContent(m.currentPRBody())
				return m, nil
			case "ctrl+s":
				body := strings.TrimSpace(m.editor.Value())
				if body == "" {
					m.status = "body cannot be empty"
					return m, nil
				}
				m.loading = true
				return m, savePRText(m.path, m.pr.ID, m.editing, m.editIdx, m.commentID(m.editIdx), body)
			}
			var cmd tea.Cmd
			m.editor, cmd = m.editor.Update(msg)
			return m, cmd
		}
		switch msg.String() {
		case "ctrl+c", "esc", "q":
			return m, tea.Quit
		case "tab", "right", "l", "shift+tab", "left", "h":
			m.tab = (m.tab + 1) % 2
			m.viewport.GotoTop()
			m.viewport.SetContent(m.currentPRBody())
			return m, nil
		case "c":
			if m.tab == 0 && !m.loading && m.err == "" {
				m.startEdit(prEditNewComment, -1, "")
				return m, nil
			}
		case "e":
			if m.tab == 0 && !m.loading && m.err == "" {
				if m.selected == 0 {
					m.startEdit(prEditDescription, 0, m.pr.Body)
				} else if m.selected-1 < len(m.pr.Comments) {
					m.startEdit(prEditComment, m.selected-1, m.pr.Comments[m.selected-1].Body)
				}
				return m, nil
			}
		case "n":
			if m.tab == 0 && m.selected < len(m.pr.Comments) {
				m.selected++
				m.viewport.SetContent(m.currentPRBody())
				return m, nil
			}
		case "p":
			if m.tab == 0 && m.selected > 0 {
				m.selected--
				m.viewport.SetContent(m.currentPRBody())
				return m, nil
			}
		}
	case prDetailsLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err.Error()
		} else {
			m.err = ""
			m.pr = msg.pr
			m.diff = msg.diff
			m.diffTool = msg.diffTool
			if m.selected > len(m.pr.Comments) {
				m.selected = len(m.pr.Comments)
			}
		}
		m.viewport.SetContent(m.currentPRBody())
	case prSavedMsg:
		m.loading = false
		if msg.err != nil {
			m.status = msg.err.Error()
			return m, nil
		}
		m.editing = prEditNone
		m.status = "saved"
		return m, loadPRDetails(m.path)
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m *prDetailsModel) startEdit(mode prEditMode, idx int, body string) {
	m.editing = mode
	m.editIdx = idx
	m.editor.SetValue(body)
	m.editor.CursorEnd()
	m.editor.Focus()
	m.status = ""
}

func (m prDetailsModel) commentID(idx int) string {
	if idx >= 0 && idx < len(m.pr.Comments) {
		return m.pr.Comments[idx].ID
	}
	return ""
}

func (m prDetailsModel) View() string {
	w := m.width
	if w < 20 {
		w = 100
	}
	if m.editing != prEditNone {
		return m.prHeader(w) + "\n" + m.editTitle() + "\n" + m.editor.View() + "\n" + helpStyle.Render("ctrl+s save   esc cancel")
	}
	return m.prHeader(w) + "\n" + m.viewport.View() + "\n" + m.prFooter(w)
}

func (m prDetailsModel) editTitle() string {
	switch m.editing {
	case prEditNewComment:
		return sectionStyle.Render("New comment")
	case prEditDescription:
		return sectionStyle.Render("Edit description")
	case prEditComment:
		return sectionStyle.Render("Edit comment")
	}
	return ""
}

func (m prDetailsModel) prHeader(w int) string {
	tool := ""
	if m.tab == 1 && m.diffTool != "" {
		tool = dimStyle.Render("  diff: " + m.diffTool)
	}
	status := ""
	if m.status != "" {
		status = dimStyle.Render("  " + m.status)
	}
	return lipgloss.NewStyle().Padding(0, 1).Render(sectionStyle.Render("Pull request")+tool+status) + "\n" +
		lipgloss.NewStyle().Padding(0, 1).Render(m.tabLabel(0, "Conversation")+"  "+m.tabLabel(1, "Files changed")+dimStyle.Render("   tab/←/→ switch"))
}

func (m prDetailsModel) tabLabel(tab int, label string) string {
	if m.tab == tab {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("99")).Bold(true).Padding(0, 1).Render("● " + label)
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Padding(0, 1).Render(label)
}

func (m prDetailsModel) prFooter(w int) string {
	pos := fmt.Sprintf("%d%%", int(m.viewport.ScrollPercent()*100))
	return helpStyle.Render("↑/↓ j/k pgup/pgdn scroll   n/p select   c comment   e edit selected   q/esc close   " + pos)
}

func (m prDetailsModel) currentPRBody() string {
	if m.err != "" {
		return errStyle.Render(m.err)
	}
	if m.loading {
		return "\n" + dimStyle.Render("Loading PR details…")
	}
	if m.tab == 0 {
		return renderPRConversation(m.pr, m.selected)
	}
	return sideBySideDiff(m.diff, m.viewport.Width)
}

func loadPRDetails(path string) tea.Cmd {
	return func() tea.Msg {
		pr, err := prLoadData(path)
		if err != nil {
			return prDetailsLoadedMsg{err: err}
		}
		diff, tool, err := prDiffText(path)
		if err != nil {
			return prDetailsLoadedMsg{pr: pr, err: err}
		}
		return prDetailsLoadedMsg{pr: pr, diff: diff, diffTool: tool}
	}
}

func prLoadData(path string) (prData, error) {
	cmd := exec.Command("gh", "pr", "view", "--json", "id,number,title,state,url,author,body,comments,baseRefName,headRefName,createdAt,reviewDecision")
	cmd.Dir = path
	out, err := cmd.Output()
	if err != nil {
		return prData{}, err
	}
	var pr prData
	if err := json.Unmarshal(out, &pr); err != nil {
		return prData{}, err
	}
	return pr, nil
}

func renderPRConversation(pr prData, selected int) string {
	var sb strings.Builder
	sb.WriteString(activeStyle.Render(fmt.Sprintf("#%d %s", pr.Number, pr.Title)) + "\n")
	sb.WriteString(dimStyle.Render(fmt.Sprintf("%s opened by @%s · %s → %s", pr.State, pr.Author.Login, pr.HeadRefName, pr.BaseRefName)) + "\n")
	if pr.ReviewDecision != "" {
		sb.WriteString(dimStyle.Render("review: ") + sectionStyle.Render(pr.ReviewDecision) + "\n")
	}
	sb.WriteString(dimStyle.Render(pr.URL) + "\n\n")

	sb.WriteString(prSelectableTitle(selected == 0, "Description") + "\n")
	sb.WriteString(githubBox("", pr.Body))
	if len(pr.Comments) > 0 {
		sb.WriteString("\n" + prDivider("Conversation") + "\n")
	}
	for i, c := range pr.Comments {
		commentTitle := fmt.Sprintf("@%s commented %s", c.Author.Login, c.CreatedAt)
		sb.WriteString("\n" + prSelectableTitle(selected == i+1, commentTitle) + "\n")
		sb.WriteString(githubBox("", c.Body))
	}
	return sb.String()
}

func prSelectableTitle(selected bool, label string) string {
	if selected {
		return activeStyle.Render("▶ " + label)
	}
	return prDivider(label)
}

func savePRText(path, prID string, mode prEditMode, idx int, commentID, body string) tea.Cmd {
	return func() tea.Msg {
		var err error
		switch mode {
		case prEditNewComment:
			err = ghGraphQL(path, `mutation($subject:ID!,$body:String!){addComment(input:{subjectId:$subject,body:$body}){commentEdge{node{id}}}}`, map[string]string{"subject": prID, "body": body})
		case prEditDescription:
			err = ghGraphQL(path, `mutation($id:ID!,$body:String!){updatePullRequest(input:{pullRequestId:$id,body:$body}){pullRequest{id}}}`, map[string]string{"id": prID, "body": body})
		case prEditComment:
			if commentID == "" {
				err = fmt.Errorf("selected comment has no editable id")
			} else {
				err = ghGraphQL(path, `mutation($id:ID!,$body:String!){updateIssueComment(input:{id:$id,body:$body}){issueComment{id}}}`, map[string]string{"id": commentID, "body": body})
			}
		}
		return prSavedMsg{err: err}
	}
}

func ghGraphQL(path, query string, fields map[string]string) error {
	args := []string{"api", "graphql", "-f", "query=" + query}
	for k, v := range fields {
		args = append(args, "-f", k+"="+v)
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = path
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("gh: %s", msg)
	}
	return nil
}

func prDiffText(path string) (string, string, error) {
	gh := exec.Command("gh", "pr", "diff", "--color", "never")
	gh.Dir = path
	var ghErr bytes.Buffer
	gh.Stderr = &ghErr
	out, err := gh.Output()
	if err != nil {
		msg := strings.TrimSpace(ghErr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", "", fmt.Errorf("gh pr diff: %s", msg)
	}
	return string(out), "side-by-side", nil
}

func prDivider(label string) string {
	const width = 96
	label = strings.TrimSpace(label)
	if label == "" {
		return dimStyle.Render(strings.Repeat("─", width))
	}
	prefix := "─ " + label + " "
	remain := width - lipgloss.Width(prefix)
	if remain < 1 {
		remain = 1
	}
	return dimStyle.Render(prefix + strings.Repeat("─", remain))
}

func githubBox(title, body string) string {
	const contentWidth = 96
	body = strings.TrimSpace(body)
	if body == "" {
		body = "No description provided."
	}
	var sb strings.Builder
	if title != "" {
		sb.WriteString(sectionStyle.Render(title) + "\n")
	}
	for _, line := range wrapLines(body, contentWidth) {
		sb.WriteString(line + "\n")
	}
	return sb.String()
}

func sideBySideDiff(diff string, width int) string {
	if width < 40 {
		width = 40
	}
	sepWidth := 3
	leftWidth := (width - sepWidth) / 2
	rightWidth := width - sepWidth - leftWidth

	var sb strings.Builder
	var pendingDeletes []string

	flushDeletes := func() {
		for _, d := range pendingDeletes {
			writeDiffRow(&sb, "− "+d, "", leftWidth, rightWidth, errStyle, normalStyle)
		}
		pendingDeletes = nil
	}

	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git"):
			flushDeletes()
			sb.WriteString("\n" + sectionStyle.Render(truncate(l, width)) + "\n")
		case strings.HasPrefix(l, "@@"):
			flushDeletes()
			sb.WriteString(activeStyle.Render(truncate(l, width)) + "\n")
		case strings.HasPrefix(l, "index ") || strings.HasPrefix(l, "---") || strings.HasPrefix(l, "+++"):
			flushDeletes()
			sb.WriteString(dimStyle.Render(truncate(l, width)) + "\n")
		case strings.HasPrefix(l, "-"):
			pendingDeletes = append(pendingDeletes, l[1:])
		case strings.HasPrefix(l, "+"):
			right := l[1:]
			if len(pendingDeletes) > 0 {
				left := pendingDeletes[0]
				pendingDeletes = pendingDeletes[1:]
				writeDiffRow(&sb, "− "+left, "+ "+right, leftWidth, rightWidth, errStyle, normalStyle)
			} else {
				writeDiffRow(&sb, "", "+ "+right, leftWidth, rightWidth, errStyle, normalStyle)
			}
		case strings.HasPrefix(l, " "):
			flushDeletes()
			text := "  " + l[1:]
			writeDiffRow(&sb, text, text, leftWidth, rightWidth, lipgloss.NewStyle(), lipgloss.NewStyle())
		case l == "":
			flushDeletes()
		default:
			flushDeletes()
			sb.WriteString(truncate(l, width) + "\n")
		}
	}
	flushDeletes()
	return sb.String()
}

func writeDiffRow(sb *strings.Builder, left, right string, leftWidth, rightWidth int, leftStyle, rightStyle lipgloss.Style) {
	leftLines := wrapLines(left, leftWidth)
	rightLines := wrapLines(right, rightWidth)
	rows := len(leftLines)
	if len(rightLines) > rows {
		rows = len(rightLines)
	}
	if rows == 0 {
		rows = 1
	}
	for i := 0; i < rows; i++ {
		var l, r string
		if i < len(leftLines) {
			l = leftLines[i]
		}
		if i < len(rightLines) {
			r = rightLines[i]
		}
		sb.WriteString(leftStyle.Render(padVisual(l, leftWidth)))
		sb.WriteString(dimStyle.Render(" │ "))
		sb.WriteString(rightStyle.Render(padVisual(r, rightWidth)))
		sb.WriteString("\n")
	}
}

func padVisual(s string, n int) string {
	w := lipgloss.Width(s)
	if w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-w)
}

func wrapLines(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		for runeLen(para) > width {
			cut := wrapCut(para, width)
			out = append(out, strings.TrimRight(para[:cut], " "))
			para = strings.TrimLeft(para[cut:], " ")
		}
		out = append(out, para)
	}
	return out
}

func wrapCut(s string, width int) int {
	lastSpace, count := -1, 0
	for i, r := range s {
		if r == ' ' || r == '\t' {
			lastSpace = i
		}
		if count == width {
			if lastSpace > 0 {
				return lastSpace
			}
			return i
		}
		count++
	}
	return len(s)
}

func pad(s string, n int) string {
	l := runeLen(s)
	if l >= n {
		return s
	}
	return s + strings.Repeat(" ", n-l)
}

func truncate(s string, max int) string {
	if max <= 0 || runeLen(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

func runeLen(s string) int { return len([]rune(s)) }

func openCreatePRPopup(path string) tea.Cmd {
	return func() tea.Msg {
		bin, err := os.Executable()
		if err != nil {
			return prPopupDoneMsg{err: err}
		}
		cmd := exec.Command("tmux", "display-popup", "-t", "gw:active.1", "-w", "90%", "-h", "90%", "-E", bin, "--create-pr", path)
		if err := cmd.Run(); err != nil {
			return prPopupDoneMsg{err: err}
		}
		return prPopupDoneMsg{}
	}
}

type createPRDraft struct {
	Base  string
	Head  string
	Title string
	Body  string
	Diff  string
}

type createPRModel struct {
	path     string
	loading  bool
	creating bool
	created  string
	err      string
	width    int
	height   int
	focus    int
	draft    createPRDraft
	title    textinput.Model
	body     textarea.Model
	preview  viewport.Model
}

type createPRLoadedMsg struct {
	draft createPRDraft
	err   error
}

type createPRCreatedMsg struct {
	url string
	err error
}

func runCreatePR(path string) {
	ti := textinput.New()
	ti.Placeholder = "PR title"
	ta := textarea.New()
	ta.Placeholder = "PR description"
	ta.ShowLineNumbers = false
	ta.CharLimit = 20000
	m := createPRModel{path: path, loading: true, title: ti, body: ta}
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "gw create-pr:", err)
	}
}

func (m createPRModel) Init() tea.Cmd { return loadCreatePRDraftCmd(m.path) }

func (m createPRModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.preview.Width = max(20, msg.Width-4)
		m.preview.Height = max(3, msg.Height-10)
		m.body.SetWidth(max(20, msg.Width-4))
		m.body.SetHeight(max(4, msg.Height-12))
		m.preview.SetContent(m.previewText())
	case createPRLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.draft = msg.draft
		m.title.SetValue(msg.draft.Title)
		m.body.SetValue(msg.draft.Body)
		m.title.Focus()
		m.preview.SetContent(m.previewText())
		return m, textinput.Blink
	case createPRCreatedMsg:
		m.creating = false
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.created = msg.url
		return m, nil
	case tea.KeyMsg:
		if m.created != "" {
			switch msg.String() {
			case "ctrl+c", "esc", "q", "enter":
				return m, tea.Quit
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "tab", "shift+tab":
			if msg.String() == "shift+tab" {
				m.focus = (m.focus + 2) % 3
			} else {
				m.focus = (m.focus + 1) % 3
			}
			m.title.Blur()
			m.body.Blur()
			if m.focus == 0 {
				m.title.Focus()
			} else if m.focus == 1 {
				m.body.Focus()
			}
			m.preview.SetContent(m.previewText())
			return m, nil
		case "ctrl+s":
			if m.loading || m.creating {
				return m, nil
			}
			title := strings.TrimSpace(m.title.Value())
			body := strings.TrimSpace(m.body.Value())
			if title == "" {
				m.err = "title cannot be empty"
				return m, nil
			}
			m.creating = true
			return m, createPRCmd(m.path, m.draft.Base, m.draft.Head, title, body)
		}
	}

	var cmd tea.Cmd
	switch m.focus {
	case 0:
		m.title, cmd = m.title.Update(msg)
	case 1:
		m.body, cmd = m.body.Update(msg)
	case 2:
		m.preview, cmd = m.preview.Update(msg)
	}
	m.preview.SetContent(m.previewText())
	return m, cmd
}

func (m createPRModel) View() string {
	if m.created != "" {
		return "\n" + sectionStyle.Render("Pull request created") + "\n\n" + normalStyle.Render(m.created) + "\n\n" + helpStyle.Render("enter / q / esc  close")
	}
	if m.loading {
		return "\n" + dimStyle.Render("Preparing pull request draft…")
	}
	if m.err != "" && m.draft.Title == "" {
		return "\n" + errStyle.Render(m.err) + "\n\n" + helpStyle.Render("esc  close")
	}

	var sb strings.Builder
	sb.WriteString(sectionStyle.Render("Create pull request") + "\n")
	sb.WriteString(dimStyle.Render(fmt.Sprintf("%s → %s", m.draft.Head, m.draft.Base)) + "\n\n")
	sb.WriteString(focusLabel(m.focus == 0, "Title") + "\n")
	sb.WriteString(m.title.View() + "\n\n")
	sb.WriteString(focusLabel(m.focus == 1, "Body") + "\n")
	if m.focus == 2 {
		sb.WriteString(dimStyle.Render("(tab back to edit body)") + "\n")
	} else {
		sb.WriteString(m.body.View() + "\n")
	}
	if m.focus == 2 {
		sb.WriteString("\n" + focusLabel(true, "Diff vs base") + "\n" + m.preview.View() + "\n")
	}
	if m.err != "" {
		sb.WriteString("\n" + errStyle.Render(m.err) + "\n")
	}
	if m.creating {
		sb.WriteString("\n" + dimStyle.Render("Creating PR…") + "\n")
	}
	sb.WriteString("\n" + helpStyle.Render("tab  switch fields   ctrl+s  create PR   esc  cancel"))
	return sb.String()
}

func focusLabel(active bool, label string) string {
	if active {
		return activeStyle.Render("● " + label)
	}
	return dimStyle.Render("  " + label)
}

func (m createPRModel) previewText() string {
	return sectionStyle.Render("Diff stat") + "\n" + m.draft.Diff
}

func loadCreatePRDraftCmd(path string) tea.Cmd {
	return func() tea.Msg {
		draft, err := createPRDraftForPath(path)
		return createPRLoadedMsg{draft: draft, err: err}
	}
}

func createPRDraftForPath(path string) (createPRDraft, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return createPRDraft{}, fmt.Errorf("gh CLI is required to create pull requests")
	}
	branch, err := gitOutput(path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch == "HEAD" {
		return createPRDraft{}, fmt.Errorf("current worktree is not on a branch")
	}
	upstream, err := gitOutput(path, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil || upstream == "" {
		return createPRDraft{}, fmt.Errorf("branch is not pushed yet (set upstream first)")
	}
	parts := strings.SplitN(upstream, "/", 2)
	if len(parts) != 2 {
		return createPRDraft{}, fmt.Errorf("cannot determine upstream remote for %s", upstream)
	}
	remoteURL, _ := gitOutput(path, "config", "--get", "remote."+parts[0]+".url")
	if !strings.Contains(remoteURL, "github.com") {
		return createPRDraft{}, fmt.Errorf("upstream remote is not GitHub: %s", parts[0])
	}
	counts, err := gitOutput(path, "rev-list", "--left-right", "--count", "@{u}...HEAD")
	if err != nil {
		return createPRDraft{}, err
	}
	fields := strings.Fields(counts)
	if len(fields) == 2 && fields[1] != "0" {
		return createPRDraft{}, fmt.Errorf("branch has unpushed commits; push first")
	}
	viewCmd := exec.Command("gh", "pr", "view", "--json", "number")
	viewCmd.Dir = path
	if viewCmd.Run() == nil {
		return createPRDraft{}, fmt.Errorf("this branch already has a pull request")
	}
	base := configuredBase(path, branch)
	baseRef := base
	if gitOutputOK(path, "rev-parse", "--verify", "refs/remotes/"+parts[0]+"/"+base) {
		baseRef = parts[0] + "/" + base
	}
	log, err := gitOutput(path, "log", "--reverse", "--pretty=format:- %s", baseRef+"...HEAD")
	if err != nil || strings.TrimSpace(log) == "" {
		return createPRDraft{}, fmt.Errorf("no commits found versus %s", base)
	}
	title, _ := gitOutput(path, "log", "-1", "--pretty=%s")
	diff, _ := gitOutput(path, "diff", "--stat", baseRef+"...HEAD")
	body := "## Summary\n" + log + "\n\n## Diff\n```\n" + strings.TrimSpace(diff) + "\n```\n"
	return createPRDraft{Base: base, Head: branch, Title: title, Body: body, Diff: diff}, nil
}

func configuredBase(path, branch string) string {
	if base, err := gitOutput(path, "config", "--get", "branch."+branch+".gh-merge-base"); err == nil && base != "" {
		return base
	}
	cmd := exec.Command("gh", "repo", "view", "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name")
	cmd.Dir = path
	if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return strings.TrimSpace(string(out))
	}
	if base, err := gitOutput(path, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && strings.Contains(base, "/") {
		return strings.TrimPrefix(base, "origin/")
	}
	return "main"
}

func createPRCmd(path, base, head, title, body string) tea.Cmd {
	return func() tea.Msg {
		f, err := os.CreateTemp("", "gw-pr-body-*.md")
		if err != nil {
			return createPRCreatedMsg{err: err}
		}
		name := f.Name()
		defer os.Remove(name)
		if _, err := f.WriteString(body); err != nil {
			f.Close()
			return createPRCreatedMsg{err: err}
		}
		f.Close()
		cmd := exec.Command("gh", "pr", "create", "--base", base, "--head", head, "--title", title, "--body-file", name)
		cmd.Dir = path
		out, err := cmd.CombinedOutput()
		if err != nil {
			return createPRCreatedMsg{err: fmt.Errorf("%s", strings.TrimSpace(string(out)))}
		}
		return createPRCreatedMsg{url: strings.TrimSpace(string(out))}
	}
}

func gitOutput(path string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func gitOutputOK(path string, args ...string) bool {
	cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
	return cmd.Run() == nil
}
