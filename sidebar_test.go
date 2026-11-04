package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

func sidebarWithWorktrees(n, width, height int) sidebarModel {
	p := Project{Path: "/repo", Name: "repo"}
	items := []listItem{{isHeader: true, project: p}}
	for i := 0; i < n; i++ {
		branch := fmt.Sprintf("branch-%d", i)
		items = append(items, listItem{
			project:  p,
			worktree: Worktree{Path: "/repo-" + branch, Branch: branch},
			title:    "wt-repo-" + branch,
			branch:   branch,
			display:  branch,
		})
	}
	return sidebarModel{items: items, cursor: 1, width: width, height: height, windows: map[string]bool{}, cfg: defaultConfig(),
		input: textinput.New(), searchInput: textinput.New()}
}

func TestSidebarShowsAConfigErrorInFullAtNarrowWidth(t *testing.T) {
	m := sidebarWithWorktrees(1, 24, 24)
	m.inErr = `config.json: unknown field "qiut"`

	if view := m.View(); !strings.Contains(view, "qiut") {
		t.Fatalf("the misspelled name is cut off:\n%s", view)
	}
}

func TestSidebarQuitsWithTheConfiguredKey(t *testing.T) {
	m := sidebarWithWorktrees(1, 40, 50)
	m.cfg.Keys.Sidebar.Quit = Keys{"x"}

	_, cmd := m.Update(pressKey("x"))

	if cmd == nil {
		t.Fatal("no command returned")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("configured quit key did not quit")
	}

	_, cmd = m.Update(pressKey("q"))
	if cmd != nil {
		t.Fatal("the old quit key still acts")
	}
}

func TestSidebarOpensTheCreateViewWithTheConfiguredKey(t *testing.T) {
	m := sidebarWithWorktrees(1, 40, 50)
	m.cfg.Keys.Sidebar.NewWorktree = Keys{"w"}

	updated, _ := m.Update(pressKey("w"))
	if got := updated.(sidebarModel).view; got != viewCreate {
		t.Fatalf("view = %v after the configured key", got)
	}

	updated, _ = m.Update(pressKey("n"))
	if got := updated.(sidebarModel).view; got != viewList {
		t.Fatalf("the old key still opens the view: %v", got)
	}
}

func TestSidebarMovesWithConfiguredKeys(t *testing.T) {
	m := sidebarWithWorktrees(3, 40, 50)
	m.cfg.Keys.Sidebar.MoveDown = Keys{"s"}
	m.cfg.Keys.Sidebar.MoveUp = Keys{"w"}

	updated, _ := m.Update(pressKey("s"))
	if got := updated.(sidebarModel).cursor; got != 2 {
		t.Fatalf("cursor after down = %d", got)
	}

	updated, _ = updated.(sidebarModel).Update(pressKey("w"))
	if got := updated.(sidebarModel).cursor; got != 1 {
		t.Fatalf("cursor after up = %d", got)
	}
}

func TestSidebarHelpShowsConfiguredKeys(t *testing.T) {
	m := sidebarWithWorktrees(1, 80, 50)
	m.cfg.Keys.Sidebar.NewWorktree = Keys{"w"}
	m.cfg.Tmux.Prefix = "ctrl+b"
	m.cfg.Tmux.PrefixBindings.NewTab = Keys{"t"}

	view := m.View()

	if !strings.Contains(view, "w  new worktree") {
		t.Fatalf("help does not show the sidebar key:\n%s", view)
	}
	if !strings.Contains(view, "^b t  new") {
		t.Fatalf("help does not show the prefix and tmux key:\n%s", view)
	}
}

func TestSidebarAddProjectWithALinkedWorktreeTracksTheMainRepository(t *testing.T) {
	repo := worktreeRepo(t, "false")
	wt := linkedWorktree(t, repo)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	m := sidebarWithWorktrees(1, 40, 50)
	m.view = viewAddProject
	m.input.SetValue(wt)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if cmd == nil {
		t.Fatal("enter did not add the project")
	}
	added, ok := cmd().(projectAdded)
	if !ok || added.err != nil {
		t.Fatalf("project not added: %+v", added)
	}
	if added.path != realPath(t, repo) {
		t.Fatalf("tracked %s, want the main repository %s", added.path, realPath(t, repo))
	}
	if got := loadState().Projects; len(got) != 1 || got[0].Path != realPath(t, repo) || got[0].Name != "repo" {
		t.Fatalf("state.json projects = %+v", got)
	}

	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if again := cmd().(projectAdded); again.err == nil || !strings.Contains(again.err.Error(), "already tracked: repo") {
		t.Fatalf("a worktree of a tracked repository was not reported: %+v", again)
	}
}

func TestSidebarDigitsStillOpenFrequentWorktrees(t *testing.T) {
	m := sidebarWithWorktrees(1, 40, 50)
	m.items = append(m.items, listItem{isHeader: true, frequent: true},
		listItem{frequent: true, title: "wt-repo-branch-0", branch: "branch-0"})
	freq := len(m.items) - 1

	updated, cmd := m.Update(pressKey("1"))

	if cmd == nil {
		t.Fatal("digit did not start a switch")
	}
	if got := updated.(sidebarModel).cursor; got != freq {
		t.Fatalf("cursor = %d, want %d", got, freq)
	}
}

func TestSidebarAtDefaultITermSizeShowsTheWorktreesNotTheHelp(t *testing.T) {
	m := sidebarWithWorktrees(6, 24, 24)

	view := m.View()

	for i := 0; i < 6; i++ {
		if !strings.Contains(view, fmt.Sprintf("branch-%d", i)) {
			t.Fatalf("branch-%d not visible at 24x24:\n%s", i, view)
		}
	}
	if strings.Contains(view, "(_____/") {
		t.Fatalf("dog art wastes rows at 24x24:\n%s", view)
	}
	if !strings.Contains(view, "? help") {
		t.Fatalf("no hint for the hidden help:\n%s", view)
	}
	if lines := strings.Count(view, "\n") + 1; lines > 24 {
		t.Fatalf("view has %d lines for a height of 24", lines)
	}
}

func TestSidebarQuestionMarkShowsFullHelpInASmallWindow(t *testing.T) {
	m := sidebarWithWorktrees(1, 24, 24)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})

	if view := updated.(sidebarModel).View(); !strings.Contains(view, "^a c") {
		t.Fatalf("full help not shown after ?:\n%s", view)
	}
}

func TestSidebarInATallWindowKeepsDogAndHelp(t *testing.T) {
	m := sidebarWithWorktrees(2, 40, 50)

	view := m.View()

	if !strings.Contains(view, "(_____/") {
		t.Fatalf("dog art missing at height 50:\n%s", view)
	}
	if !strings.Contains(view, "^a c") {
		t.Fatalf("full help missing at height 50:\n%s", view)
	}
}
