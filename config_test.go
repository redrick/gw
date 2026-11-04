package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

func writeConfig(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	dir := filepath.Join(home, ".config", "gw")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func keysEqual(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00")
}

func TestLoadConfigAcceptsTheExistingFileFormat(t *testing.T) {
	writeConfig(t, `{
	  "tmux": {"global_bindings": {"focus_sidebar": ["ctrl+e"], "new_tab": ["ctrl+t"],
	    "next_tab": ["ctrl+f"], "previous_tab": ["ctrl+d"]}},
	  "keys": {"pr_details": {"save": ["ctrl+g"]}, "create_pr": {"create": ["ctrl+y"]}}
	}`)

	cfg, err := loadConfig()

	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !keysEqual(cfg.Tmux.GlobalBindings.FocusSidebar, []string{"ctrl+e"}) {
		t.Fatalf("focus_sidebar = %v", cfg.Tmux.GlobalBindings.FocusSidebar)
	}
	if !keysEqual(cfg.Keys.PRDetails.Save, []string{"ctrl+g"}) {
		t.Fatalf("pr_details.save = %v", cfg.Keys.PRDetails.Save)
	}
	if !keysEqual(cfg.Keys.CreatePR.Create, []string{"ctrl+y"}) {
		t.Fatalf("create_pr.create = %v", cfg.Keys.CreatePR.Create)
	}
}

func TestLoadConfigOverridesOnlyTheActionsInTheFile(t *testing.T) {
	writeConfig(t, `{"keys": {"sidebar": {"quit": ["x"]}}, "tmux": {"prefix_bindings": {"new_tab": ["t"]}}}`)

	cfg, err := loadConfig()

	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !keysEqual(cfg.Keys.Sidebar.Quit, []string{"x"}) {
		t.Fatalf("quit = %v", cfg.Keys.Sidebar.Quit)
	}
	if !keysEqual(cfg.Keys.Sidebar.NewWorktree, []string{"n"}) {
		t.Fatalf("new_worktree lost its default: %v", cfg.Keys.Sidebar.NewWorktree)
	}
	if !keysEqual(cfg.Tmux.PrefixBindings.NewTab, []string{"t"}) {
		t.Fatalf("new_tab = %v", cfg.Tmux.PrefixBindings.NewTab)
	}
	if !keysEqual(cfg.Tmux.PrefixBindings.NextTab, []string{"n"}) {
		t.Fatalf("next_tab lost its default: %v", cfg.Tmux.PrefixBindings.NextTab)
	}
	if cfg.Tmux.Prefix != "ctrl+a" {
		t.Fatalf("prefix lost its default: %q", cfg.Tmux.Prefix)
	}
}

func TestLoadConfigWithoutAFileGivesDefaultsAndNoError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	cfg, err := loadConfig()

	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !keysEqual(cfg.Keys.PRDetails.Save, []string{"ctrl+s"}) {
		t.Fatalf("default save = %v", cfg.Keys.PRDetails.Save)
	}
	if len(cfg.Tmux.GlobalBindings.NewTab) != 0 {
		t.Fatalf("global bindings must be off by default: %v", cfg.Tmux.GlobalBindings.NewTab)
	}
}

func TestLoadConfigReportsATypoAndFallsBackToDefaults(t *testing.T) {
	writeConfig(t, `{"keys": {"sidebar": {"quit": ["x"], "qiut": ["y"]}}}`)

	cfg, err := loadConfig()

	if err == nil || err.Error() != `config.json: unknown field "qiut"` {
		t.Fatalf("expected a short error that names the typo, got %v", err)
	}
	if !keysEqual(cfg.Keys.Sidebar.Quit, []string{"q", "ctrl+c"}) {
		t.Fatalf("a rejected file must not apply partly: %v", cfg.Keys.Sidebar.Quit)
	}
}

func TestLoadConfigReportsInvalidJSON(t *testing.T) {
	writeConfig(t, `not json`)

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected an error")
	}
}

func TestTmuxKeyTranslatesConfigNames(t *testing.T) {
	cases := map[string]string{
		"ctrl+e":     "C-e",
		"alt+x":      "M-x",
		"ctrl+alt+x": "C-M-x",
		"shift+up":   "S-Up",
		"shift+tab":  "BTab",
		"f5":         "F5",
		"enter":      "Enter",
		"pgdown":     "NPage",
	}
	for in, want := range cases {
		if got := tmuxKey(in); got != want {
			t.Errorf("tmuxKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeysLabelShowsTheFirstKeyInShortForm(t *testing.T) {
	cases := []struct {
		keys Keys
		want string
	}{
		{Keys{"n"}, "n"},
		{Keys{"ctrl+s", "ctrl+g"}, "^s"},
		{Keys{"up", "k"}, "↑"},
		{Keys{" "}, "space"},
		{Keys{"shift+tab"}, "shift+tab"},
		{Keys{}, ""},
	}
	for _, c := range cases {
		if got := c.keys.Label(); got != c.want {
			t.Errorf("%v.Label() = %q, want %q", c.keys, got, c.want)
		}
	}
}

func pressKey(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestPRDetailsSavesWithTheConfiguredKey(t *testing.T) {
	ta := textarea.New()
	ta.SetValue("hello")
	keys := defaultConfig().Keys.PRDetails
	keys.Save = Keys{"ctrl+g"}
	m := prDetailsModel{editing: prEditNewComment, editor: ta, keys: keys}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})

	if !updated.(prDetailsModel).loading || cmd == nil {
		t.Fatal("configured key did not start a save")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if updated.(prDetailsModel).loading {
		t.Fatal("ctrl+s still saves after the key was changed")
	}
}

func TestPRDetailsSelectsCommentsWithConfiguredKeys(t *testing.T) {
	keys := defaultConfig().Keys.PRDetails
	keys.NextComment = Keys{"J"}
	keys.PrevComment = Keys{"K"}
	m := prDetailsModel{keys: keys, pr: prData{Comments: []prComment{{}, {}}}, editor: textarea.New()}

	updated, _ := m.Update(pressKey("J"))
	if got := updated.(prDetailsModel).selected; got != 1 {
		t.Fatalf("selected after next = %d", got)
	}

	updated, _ = updated.(prDetailsModel).Update(pressKey("K"))
	if got := updated.(prDetailsModel).selected; got != 0 {
		t.Fatalf("selected after prev = %d", got)
	}

	updated, _ = m.Update(pressKey("n"))
	if got := updated.(prDetailsModel).selected; got != 0 {
		t.Fatalf("the old key still selects: %d", got)
	}
}

func TestPRDetailsClosesWithTheConfiguredKey(t *testing.T) {
	keys := defaultConfig().Keys.PRDetails
	keys.Close = Keys{"x"}
	m := prDetailsModel{keys: keys, editor: textarea.New()}

	_, cmd := m.Update(pressKey("x"))

	if cmd == nil {
		t.Fatal("no command returned")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("configured close key did not quit")
	}
}

func TestPRDetailsFooterShowsConfiguredKeys(t *testing.T) {
	keys := defaultConfig().Keys.PRDetails
	keys.NewComment = Keys{"m"}
	m := prDetailsModel{keys: keys, editor: textarea.New()}

	if footer := m.prFooter(100); !strings.Contains(footer, "m comment") {
		t.Fatalf("footer does not show the configured key: %q", footer)
	}
}

func TestCreatePRSubmitsWithTheConfiguredKey(t *testing.T) {
	ti := textinput.New()
	ti.SetValue("title")
	keys := defaultConfig().Keys.CreatePR
	keys.Create = Keys{"ctrl+y"}
	m := createPRModel{keys: keys, title: ti, body: textarea.New()}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})

	if !updated.(createPRModel).creating || cmd == nil {
		t.Fatal("configured key did not start PR creation")
	}
}

func TestCreatePRMovesBetweenFieldsWithConfiguredKeys(t *testing.T) {
	keys := defaultConfig().Keys.CreatePR
	keys.NextField = Keys{"ctrl+n"}
	keys.PrevField = Keys{"ctrl+p"}
	m := createPRModel{keys: keys, title: textinput.New(), body: textarea.New()}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	if got := updated.(createPRModel).focus; got != 1 {
		t.Fatalf("focus after next = %d", got)
	}

	updated, _ = updated.(createPRModel).Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	if got := updated.(createPRModel).focus; got != 0 {
		t.Fatalf("focus after prev = %d", got)
	}
}
