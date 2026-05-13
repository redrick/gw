package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── Styles ────────────────────────────────────────────────────────────────────

var (
	activeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	sectionStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Bold(true)
	normalStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("86"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	helpStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

// ── List items ────────────────────────────────────────────────────────────────

type listItem struct {
	isHeader  bool
	isShell   bool   // launch dir entry — not a worktree
	project   Project
	worktree  Worktree
	title     string
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
	viewList   sidebarView = iota
	viewCreate
)

type worktreeAdded struct {
	wt    Worktree
	title string
	proj  Project
	err   error
}

type switchedMsg struct {
	title string
}

type sidebarModel struct {
	items   []listItem
	cursor  int
	view    sidebarView
	input   textinput.Model
	inErr   string
	state   State
	width   int
	windows map[string]bool
	ready   bool
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

	return sidebarModel{items: items, cursor: cursor, input: ti, state: st, windows: liveWindows()}
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

func (m sidebarModel) Init() tea.Cmd { return nil }

func (m sidebarModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
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
		item := listItem{project: msg.proj, worktree: msg.wt, title: msg.title}
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
	}

	if m.view == viewCreate {
		return m.updateCreate(msg)
	}
	return m.updateList(msg)
}

func (m sidebarModel) updateList(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "up", "k":
			m.prevItem()
		case "down", "j":
			m.nextItem()
		case "enter", " ":
			if it := m.currentItem(); it != nil {
				return m, m.doSwitch(*it)
			}
		case "n":
			if it := m.currentItem(); it != nil && !it.isShell {
				m.view = viewCreate
				m.inErr = ""
				m.input.SetValue("")
				m.input.Focus()
				return m, textinput.Blink
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
	if m.view == viewCreate {
		return m.viewCreate()
	}
	return m.viewList()
}

func (m sidebarModel) viewList() string {
	w := m.width
	if w < 10 {
		w = 30
	}

	var sb strings.Builder
	sb.WriteString(sectionStyle.Render("worktrees") + "\n")
	sb.WriteString(dimStyle.Render(strings.Repeat("─", w-1)) + "\n")

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
		branch := truncate(it.worktree.Branch, w-4-len(marker))

		var line string
		switch {
		case isCursor && isActive:
			line = activeStyle.Render("▶ " + branch + marker)
		case isCursor:
			line = activeStyle.Render("▶ " + branch + marker)
		case isActive:
			line = normalStyle.Render("  " + branch + marker)
		case hasWindow:
			line = normalStyle.Render("  " + branch + marker)
		default:
			line = dimStyle.Render("  " + branch)
		}
		sb.WriteString(line + "\n")
	}

	if len(m.items) == 0 {
		sb.WriteString(dimStyle.Render("  no projects tracked\n"))
		sb.WriteString(dimStyle.Render("  run gw from a git repo\n"))
	}

	sb.WriteString("\n" + helpStyle.Render("↑↓  move   enter  open\nn  new     r  refresh   q  quit"))
	sb.WriteString("\n\n" + dimStyle.Render(strings.Repeat("─", w-1)) + "\n")
	sb.WriteString(sectionStyle.Render("tmux") + "\n")
	sb.WriteString(helpStyle.Render("^a c  new window\n^a x  close window\n^a n  next   ^a p  prev\n^a [  scroll   q  exit\n^a d  detach"))
	return sb.String()
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

func truncate(s string, max int) string {
	if max <= 0 {
		return s
	}
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
