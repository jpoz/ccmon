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

## Install

ccmon is macOS-only and needs `tmux` and `terminal-notifier`:

    brew install tmux terminal-notifier

Install the binary, then wire it into Claude Code and Codex:

    go install github.com/jpoz/ccmon@latest   # → ~/go/bin/ccmon (keep ~/go/bin on $PATH)
    ccmon install

`ccmon install` is idempotent — safe to re-run after upgrading or moving the
binary. It:

- adds `ccmon hook` to `~/.claude/settings.json` for every relevant event
  (SessionStart, UserPromptSubmit, Notification, PreToolUse, PostToolUse, Stop,
  SubagentStop, SessionEnd), **preserving any hooks you already have**;
- sets `notify = ["…/ccmon", "codex-hook"]` in `~/.codex/config.toml` — only if
  `~/.codex` exists, and never clobbering a `notify` program you already use;
- backs up each file it touches to `<file>.bak`.

Then **restart any running Claude Code / Codex sessions** so they pick up the
hooks. To verify, undo, or check dependencies:

    ccmon doctor      # report wiring + dependency health
    ccmon uninstall   # remove ccmon's hooks / notify (leaves the rest intact)

Prefer not to use `go install`? Build from source — any dir on your `$PATH`
works, the installer wires whatever path the binary lives at:

    git clone https://github.com/jpoz/ccmon && cd ccmon
    go build -o ~/bin/ccmon .
    ~/bin/ccmon install

## Commands

| command            | who calls it                          | what it does                                  |
|--------------------|---------------------------------------|-----------------------------------------------|
| `ccmon install`    | you (once, to set up)                 | wire ccmon into Claude Code + Codex hooks     |
| `ccmon uninstall`  | you                                   | remove ccmon's hook / notify wiring           |
| `ccmon doctor`     | you / debugging                       | report install status + dependency health     |
| `ccmon hook`       | Claude Code hooks (JSON on stdin)     | update state, notify on needs-input / done    |
| `ccmon codex-hook` | codex `notify` (JSON in argv)         | update state, notify on turn-complete         |
| `ccmon jump <id>`  | notification click / TUI Return       | select the pane + `open -a Ghostty`           |
| `ccmon tui`        | you (`ccmon` with no args = tui)      | live router; arrow + Return to jump           |
| `ccmon list`       | you / debugging                       | plain-text dump of current state              |
| `ccmon test-notification` | you / debugging                | fire one notification by hand (`--osc` / `--plain` / `--done` / `--nag`) |

## State model

State is detected two ways that back each other up:

**1. Hooks (low-latency, push):**

| state         | set by                                            | notifies |
|---------------|---------------------------------------------------|----------|
| `idle`        | SessionStart                                      | no       |
| `working`     | UserPromptSubmit / Pre/PostToolUse / SubagentStop | no       |
| `needs-input` | Notification                                      | **yes**  |
| `done`        | Stop / codex turn-complete                        | **yes**  |

`PostToolUse` is the important one: Claude fires **no** event when you *grant*
permission, so a tool actually running again is the signal that you've unblocked
it. Without that hook a session stayed stuck on `needs-input` until the whole
turn ended.

**2. Pane reconciliation (self-healing, pull):** every TUI poll, each Claude
and codex pane is captured (`tmux capture-pane`) and classified directly from
what's on screen — a permission box (`Do you want to proceed?` + `❯ 1. Yes`,
or codex's `› 1. Yes, continue` approvals) ⇒ `needs-input`, a live spinner
(`Germinating… (4m 44s · ↓ 18.1k tokens)` / `• Working (14s • esc to
interrupt)`) ⇒ `working`, a completed line (`✻ Cooked for 1m 39s` / `※ recap:`
/ `─ Worked for 1m 13s ─`) ⇒ `done`. The pane is ground truth for whether the
agent is blocked on you, so this corrects any hook that was missed, delayed,
or never fired. It matters doubly for codex, which fires exactly **one** event
per turn (the completion notify): a new turn starting, an approval box
appearing, and a stale `working` are all visible only on the pane. One codex
quirk: short-turn completions are pixel-identical to an idle composer, so an
ambiguous screen never overrides hook state — except a claimed `working` on a
pane that's gone quiet (a real turn redraws its elapsed timer every second),
which demotes to `idle`. `classifyClaudePane` / `classifyCodexPane` in
`pane.go` are pure functions; `reconcileClaude` / `reconcileCodex` apply them.

State lives in `~/.ccmon/state/<id>.json`, one file per instance.
Each pane is also tagged with a `@cc_state` tmux user option for status bars.

- **Claude** instances report rich state via hooks (keyed by session id), then
  get reconciled against their live pane.
- **Codex** instances report `done` via the `notify` program, get reconciled
  against their live pane for everything in between, and are also
  auto-discovered by scanning tmux even before their first event (state read
  from the pane, falling back to output recency; shown with a `~` marker).

Instances are pruned automatically when their pane closes.

## Tests

    go test ./...            # unit + tmux integration
    go test -short ./...     # unit only (skips the live-tmux test)

- `pane_test.go` — table-driven classification of real `capture-pane` fixtures
  (working / done / idle / permission / resumed-after-grant).
- `hook_test.go` — the hook state machine, incl. `needs-input → PostToolUse → working`.
- `reconcile_integration_test.go` — paints fixtures into a throwaway tmux pane
  and asserts `reconcileClaude` / `reconcileCodex` read the live pane and
  correct stale state (including the quiet-pane stale-`working` demotion).

## Wiring (managed by `ccmon install`)

You don't edit these by hand — `ccmon install` writes them and `ccmon uninstall`
removes them — but for reference, the wiring is:

- `~/.claude/settings.json` → a `hooks` entry calling `ccmon hook` for
  SessionStart, UserPromptSubmit, Notification, **PreToolUse, PostToolUse,**
  Stop, SubagentStop, and SessionEnd.
- `~/.codex/config.toml` → `notify = ["<path-to>/ccmon", "codex-hook"]`.

## Build

    go build -o ~/bin/ccmon .   # or: go install github.com/jpoz/ccmon@latest

## TUI keys

`↑/↓` or `j/k` move · `Enter` jump to pane · `c` acknowledge (clear alert) ·
`x` forget instance · `f` toggle activity feed · `PgUp/PgDn` (or `Ctrl-U/Ctrl-D`)
scroll the feed · `m` mute/unmute sounds · `n` switch notify backend ·
`r` refresh · `q` quit

## Notification sounds & mute

Each notification kind plays a configurable macOS alert sound, overridable by env
var (set in your shell profile, then **restart Claude/Codex sessions** so the
hooks inherit it; `none`/`off` mutes a kind):

| kind          | env var             | default  |
|---------------|---------------------|----------|
| needs-input   | `CCMON_SOUND_NEEDS` | `Bottle` |
| done          | `CCMON_SOUND_DONE`  | `Blow`   |
| still-waiting | `CCMON_SOUND_NAG`   | `Submarine` |

Audition any of them without touching config:

    ccmon test-notification          # needs-input sound
    ccmon test-notification --done   # done sound
    ccmon test-notification --nag    # nag sound

`m` in the TUI mutes/unmutes all notification **sounds** (banners still show); the
header shows `⊘ MUTED` while active. It writes `~/.ccmon/muted`, which the hook
processes read too, so the mute applies everywhere — not just the TUI.

## Notification backend (terminal-notifier vs OSC-777)

`terminal-notifier` is the default and the reliable one — it delivers regardless
of tmux attach state. `n` in the TUI flips to **OSC-777** (the escape-sequence
path described in *Why*) for the rest of us who want to try it: ccmon writes the
`OSC 777 ; notify` sequence to the target pane's tty, wrapped in tmux passthrough.
The header shows an `OSC-777` tag while active, and the choice persists in
`~/.ccmon/notify-backend` so the hooks honor it live (no restart needed).

Caveats of OSC-777: it only reaches you when the target pane is in the *attached*
client, your terminal must support it (Ghostty does), and tmux needs
`set -g allow-passthrough on`. Sounds are the terminal's call, not ccmon's, so the
per-kind sounds and mute don't apply (muting just drops OSC notifications). Test it:

    ccmon test-notification --osc        # also --done / --nag

## Activity feed

`f` toggles a live feed that streams state transitions as they happen —
`working → done`, `idle → needs-input`, a green `+` when a session appears,
`✕ closed` when its pane goes away — each with the project, a relative
timestamp, and the message at the moment it changed. It turns the snapshot into
a story: see what just finished or who went red while you were looking
elsewhere.

A new row **flashes** when it lands — a frost-blue highlight that fades through
greys to nothing over about a second — so activity catches your eye and then
settles, even if you weren't looking right at it.

The panel is **responsive**: on a wide terminal it docks as a full-height column
to the right of the table; on a narrow one it drops to a strip below it. Either
way the table + feed are centered as a card so nothing smears edge-to-edge.

The **newest event sits at the top**, directly under the `── ACTIVITY` rule.
`PgUp`/`Ctrl-U` scroll up toward the live tail; `PgDn`/`Ctrl-D` scroll down into
older history. Scrolling is **sequence-anchored**, so new events streaming in
while you read don't lurch the view; a `↑N PgUp=live` / `↓N` marker on the
panel's title shows how much is off screen, and scrolling back up to the tail
re-engages live-follow.

The live view is derived by diffing successive polls (`recordEvents` in
`feed.go`), so it captures everything the TUI observes — hook-driven changes,
pane reconciliation, codex activity, and your own ack/jump. All of it is
**persisted to `~/.ccmon/feed.jsonl`**, pruned to the last 24h, so the panel
opens with a day of history instead of a blank slate. Two writers feed the log:
the **hooks** record every transition they drive (so history accrues even when
the TUI is closed), and the **TUI** records what only it can see — pane-closes
and pane-reconciled corrections, which fire no hook. A transition seen by
both lands twice and is collapsed on load (`dedupeEvents` in `feedlog.go`); the
log seeds silently so already-running sessions don't flood it. The only gap is
events that happen with the TUI closed and no hook to catch them — a killed pane
or a codex turn while you're away won't be in the history.

## Re-notifications (nagging)

While the TUI is running it acts as the watcher: any session stuck in
`needs-input` (red) is re-notified every 60s ("⏰ still waiting · Nm") until you
attend to it. Nagging stops the moment the session leaves `needs-input` —
because you jumped to it (`Enter`), acked it (`c`), or it got your input. Each
reminder removes the previous banner first so it reliably re-alerts instead of
silently updating. Only red sessions nag; `done` is informational.

If the TUI isn't running you still get the single initial banner from the hook —
the repeating reminders just need the TUI up (e.g. in a dashboard pane).

## Environment variables

| var               | default | effect                                              |
|-------------------|---------|-----------------------------------------------------|
| `CCMON_NAG_SECS`  | `60`    | seconds between re-notifications of a red session   |
| `CCMON_DEBUG`     | unset   | append every notification to `~/.ccmon/notify.log`  |
