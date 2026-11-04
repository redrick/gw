package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLayoutWidthsLeaveTheMainPaneUsable(t *testing.T) {
	cases := []struct {
		total, sidebar, column, main int
	}{
		{200, 40, 25, 133},
		{120, 40, 25, 53},
		{100, 30, 18, 50},
		{80, 24, 14, 40},
		{60, 20, 10, 28},
	}
	for _, c := range cases {
		sidebar, column := layoutWidths(c.total)
		main := c.total - sidebar - column - 2 // two pane borders
		if sidebar != c.sidebar || column != c.column || main != c.main {
			t.Errorf("layoutWidths(%d) = sidebar %d, column %d, main %d; want %d, %d, %d",
				c.total, sidebar, column, main, c.sidebar, c.column, c.main)
		}
	}
}

func TestStatePathUsesDotConfigOnEveryPlatform(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	want := filepath.Join(home, ".config", "gw", "state.json")
	if got := statePath(); got != want {
		t.Fatalf("statePath() = %q, want %q", got, want)
	}
}

func TestStatePathHonorsXDGConfigHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", xdg)

	want := filepath.Join(xdg, "gw", "state.json")
	if got := statePath(); got != want {
		t.Fatalf("statePath() = %q, want %q", got, want)
	}
}

func TestLoadStateCopiesLegacyMacOSState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	legacy := filepath.Join(home, "Library", "Application Support", "gw", "state.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0755); err != nil {
		t.Fatal(err)
	}
	body := `{"projects":[{"path":"/repo","name":"repo"}]}`
	if err := os.WriteFile(legacy, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	st := loadState()

	if len(st.Projects) != 1 || st.Projects[0].Name != "repo" {
		t.Fatalf("legacy projects not loaded: %+v", st.Projects)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "gw", "state.json")); err != nil {
		t.Fatalf("state not copied to the new location: %v", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy state must stay in place: %v", err)
	}
}

func TestLoadStatePrefersNewStateOverLegacy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	saveState(State{Projects: []Project{{Path: "/new", Name: "new"}}})
	legacy := filepath.Join(home, "Library", "Application Support", "gw", "state.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"projects":[{"path":"/old","name":"old"}]}`), 0644); err != nil {
		t.Fatal(err)
	}

	st := loadState()

	if len(st.Projects) != 1 || st.Projects[0].Name != "new" {
		t.Fatalf("expected the new state to win, got %+v", st.Projects)
	}
}
