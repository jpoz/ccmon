package main

import "testing"

// These fixtures are real `tmux capture-pane -p` output from live Claude panes,
// one per visible state. If Claude's UI changes shape, update these from a fresh
// capture and the classifier follows.

const paneWorking = `⏺ Now test the nag loop end‑to‑end with a 3s interval — I'll verify the banner.
⏺ Bash(go build -o /Users/jpoz/bin/ccmon . || exit 1
      SOCK=/private/tmp/tmux-501/ccmontest…)
  ⎿  T1 (after ~4s):
  ⎿  Allowed by auto mode classifier
✻ Germinating… (4m 44s · ↓ 18.1k tokens · thinking with xhigh effort)
────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  ███░░░░░░░░░░░░░░░░░  16% (160.2k) | Opus 4.8 (1M context) in ccmon
  ⏵⏵ auto mode on (shift+tab to cycle)`

const paneDone = `  re-run make sqlc/tests), and pull main into the branch? I can do both now.
✻ Cooked for 1m 39s
※ recap: Goal: merge PR #81 (auth/multi-tenancy). CI is green; the only real
  blocker is needing an approving review. Next: get a teammate to approve.
  (disable recaps in /config)
─────────────────────────────────────────────────────────────────────────────────
❯
─────────────────────────────────────────────────────────────────────────────────
  █░░░░░░░░░░░░░░░░░░░   4% (47.3k) | Opus 4.8 (1M context) in arc
  ⏵⏵ auto mode on (shift+tab to cycle) · PR #81 · ← for agents`

// Done with the short "Worked for 59s" form and no token parenthetical.
const paneDoneShort = `  The Changes/Validation sections were left intact since they already matched.
✻ Worked for 59s
※ recap: Updated PR #61's description. Next: review and merge when ready.
───────────────────────────────────────────────────────────────────────────────
❯
───────────────────────────────────────────────────────────────────────────────
  █░░░░░░░░░░░░░░░░░░░   6% (56.9k) | Opus 4.8 (1M context) in infra-alt
  ⏵⏵ auto mode on (shift+tab to cycle) · PR #61 · ← for agents`

const paneIdle = ` ▐▛███▜▌   Claude Code v2.1.160
▝▜█████▛▘  Opus 4.8 (1M context) with xhigh effort · Claude Team
  ▘▘ ▝▝    ~/Developer/arc
 ⚠ 1 setup issue: MCP · /doctor
❯ /clear
  ⎿  (no content)
────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  ⏵⏵ auto mode on (shift+tab to cycle) · PR #88 · ← for agents`

// A reconstructed permission box (auto mode was on across all live panes, so no
// real one was on screen to capture). Markers match Claude's confirm dialog:
// a "Do you want to…" question plus the numbered, pre-selected Yes option.
const panePermission = `⏺ I'll remove the build directory.
╭──────────────────────────────────────────────────────────────╮
│ Bash command                                                   │
│                                                                │
│   rm -rf build                                                 │
│   Remove the build directory                                   │
╰──────────────────────────────────────────────────────────────╯

Do you want to proceed?
❯ 1. Yes
  2. Yes, and don't ask again for rm commands in this project
  3. No, and tell Claude what to do differently (esc)`

// A permission box that resumed: the prompt is gone and the spinner is back.
// This is the exact moment the old code stayed stuck on needs-input.
const panePermissionGranted = `⏺ Bash(rm -rf build)
  ⎿  (running)
✳ Simmering… (3s · ↑ 0.2k tokens)
────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  ███░░░░░░░░░░░░░░░░░  16% (160.2k) | Opus 4.8 (1M context) in ccmon
  ⏵⏵ auto mode on (shift+tab to cycle)`

func TestClassifyClaudePane(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
		ok   bool
	}{
		{"working spinner", paneWorking, StateWorking, true},
		{"done with recap", paneDone, StateDone, true},
		{"done short form", paneDoneShort, StateDone, true},
		{"idle prompt", paneIdle, StateIdle, true},
		{"permission box", panePermission, StateNeedsInput, true},
		{"resumed after grant", panePermissionGranted, StateWorking, true},
		{"empty pane", "", "", false},
		{"plain shell", "$ ls -la\ntotal 0\n$ ", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classifyClaudePane(tc.text)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("classifyClaudePane = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// A pane whose *transcript* quotes a permission prompt (as ccmon's own session
// does while we debug this) must not be read as blocked. While a spinner is
// live, it's working...
const paneWorkingQuotesPrompt = `⏺ I added the permission fixture, which reads:
   Do you want to proceed?
   ❯ 1. Yes
   2. Yes, and don't ask again
   3. No, and tell Claude what to do differently
⏺ Now running the tests:
⏺ Bash(go test ./...)
  ⎿  ok  ccmon
· Boondoggling… (46s · ↓ 2.6k tokens)
────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  ██░░░░░░░░░░░░░░░░░░  12% (121.4k) | Opus 4.8 in ccmon
  ⏵⏵ auto mode on (shift+tab to cycle)`

// ...and once the turn finishes, the quoted prompt is far up in the scrollback,
// so the bottom region shows only the completed turn ⇒ done, not blocked.
const paneDoneQuotesPrompt = `⏺ Earlier I explained the permission prompt:
   Do you want to proceed?
   ❯ 1. Yes
   3. No, and tell Claude what to do differently
⏺ Then I made the edits and ran everything.
⏺ Update(pane.go)
⏺ Update(state.go)
⏺ Bash(go build ./...)
  ⎿  BUILD OK
⏺ All green. Here's the summary of the fix.
✻ Cooked for 2m 58s
※ recap: hardened the pane classifier to ignore scrollback.
────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  ██░░░░░░░░░░░░░░░░░░  12% (121.4k) | Opus 4.8 in ccmon
  ⏵⏵ auto mode on (shift+tab to cycle)`

func TestScrollbackPromptDoesNotFakeNeedsInput(t *testing.T) {
	if got, _ := classifyClaudePane(paneWorkingQuotesPrompt); got != StateWorking {
		t.Errorf("working pane quoting a prompt: got %q, want working", got)
	}
	if got, _ := classifyClaudePane(paneDoneQuotesPrompt); got != StateDone {
		t.Errorf("done pane quoting a prompt: got %q, want done", got)
	}
}
