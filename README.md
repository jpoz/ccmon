# ccmon — mission control for Claude Code & Codex in tmux

Tracks the live state of every Claude Code / Codex instance running in tmux,
raises **reliable** macOS notifications when one needs you or finishes, and gives
you a TUI to see who needs attention and jump straight to that pane.

## Why

The old setup pushed notifications with an OSC-777 escape wrapped in tmux
passthrough written to `/dev/tty`. That only reaches Ghostty when the pane is
live in the currently-attached client — so notifications from detached/background
sessions were silently dropped (the "flaky push"). ccmon instead talks to
`terminal-notifier` directly, which doesn't care about tmux forwarding, and to
tmux directly for jumping.

## Commands

| command            | who calls it                          | what it does                                  |
|--------------------|---------------------------------------|-----------------------------------------------|
| `ccmon hook`       | Claude Code hooks (JSON on stdin)     | update state, notify on needs-input / done    |
| `ccmon codex-hook` | codex `notify` (JSON in argv)         | update state, notify on turn-complete         |
| `ccmon jump <id>`  | notification click / TUI Return       | select the pane + `open -a Ghostty`           |
| `ccmon tui`        | you (`ccmon` with no args = tui)      | live router; arrow + Return to jump           |
| `ccmon list`       | you / debugging                       | plain-text dump of current state              |

## State model

| state         | set by                          | notifies |
|---------------|---------------------------------|----------|
| `idle`        | SessionStart                    | no       |
| `working`     | UserPromptSubmit / SubagentStop | no       |
| `needs-input` | Notification                    | **yes**  |
| `done`        | Stop / codex turn-complete      | **yes**  |

State lives in `~/.ccmon/state/<id>.json`, one file per instance.
Each pane is also tagged with a `@cc_state` tmux user option for status bars.

- **Claude** instances report rich state via hooks (keyed by session id).
- **Codex** instances report `done` via the `notify` program, and are also
  auto-discovered by scanning tmux even before their first event (state inferred
  from pane activity; shown with a `~` marker).

Instances are pruned automatically when their pane closes.

## Wiring (already installed)

- `~/.claude/settings.json` → `hooks` for SessionStart, UserPromptSubmit,
  Notification, Stop, SubagentStop, SessionEnd all call `ccmon hook`.
- `~/.codex/config.toml` → `notify = ["/Users/jpoz/bin/ccmon", "codex-hook"]`.
- Binary installed at `~/bin/ccmon`.

## Build

    go build -o ~/bin/ccmon .

## TUI keys

`↑/↓` or `j/k` move · `Enter` jump to pane · `c` acknowledge (clear alert) ·
`x` forget instance · `r` refresh · `q` quit
