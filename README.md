# gw

A terminal-based git worktree manager that runs inside tmux. Navigate between worktrees from a persistent sidebar — each worktree keeps its own live shell, editor session, and running processes while it's in the background.

## Requirements

- Go 1.24+
- tmux
- git

## Installation

```
go install github.com/redrick/gw@latest
```

Or clone and build:

```
git clone https://github.com/redrick/gw
cd gw
make install
```

## Usage

Run `gw` from any directory. If you're inside a git repo it's automatically tracked.

```
gw
```

gw opens a tmux session with a 40-column sidebar on the left and the active worktree's shell on the right. Switching worktrees is server-side (tmux `swap-pane`) — no keystroke injection, so running processes are never interrupted.

If you re-run `gw` while a session is already open, it re-attaches to it.

## Keys

### Sidebar

| Key | Action |
|-----|--------|
| `↑↓` / `k j` | move cursor |
| `enter` / `o` | open worktree |
| `n` | create new worktree |
| `a` | add existing repo to tracking |
| `D` | remove worktree (with confirmation) |
| `d` | remove project from tracking |
| `r` | refresh worktree list |
| `q` | quit and kill session |

### tmux (prefix `^a`)

| Key | Action |
|-----|--------|
| `^a c` | new shell tab |
| `^a n` | next tab |
| `^a p` | previous tab |
| `^a s` | focus sidebar |
| `^a [` | scroll mode |

Each worktree supports multiple shell tabs (`^a c`). The active tab is shown in the status bar at the bottom of the right pane.

## State

Tracked projects and session state are persisted in `~/.config/gw/state.json`.

## License

MIT
