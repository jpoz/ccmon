package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// This file handles surfacing the *terminal* when you jump to an instance —
// distinct from the tmux navigation in jump.go. tmux's switch-client/select-pane
// moves whatever client is attached to the right pane, but it can't move your
// OS-level keyboard focus when ccmon and the target live in two different
// Ghostty splits: switching the tmux client redraws the other split, yet your
// focus stays on the ccmon split. So when we know we're running inside our own
// Ghostty split (the interactive TUI jump), we ask Ghostty (via its 1.3.0+
// AppleScript dictionary) to focus the sibling split — the one running tmux.
//
// Everything here is best-effort and Ghostty-specific: non-Ghostty users get no
// AppleScript and no spurious app launch, so their existing single-client
// switch-client workflow is untouched.

// inGhostty reports whether ccmon is running inside Ghostty. We key off
// GHOSTTY_RESOURCES_DIR / GHOSTTY_BIN_DIR rather than TERM_PROGRAM: tmux rewrites
// TERM_PROGRAM to "tmux", but Ghostty's own vars survive into a tmux session, so
// these stay reliable even when ccmon runs inside tmux.
func inGhostty() bool {
	return os.Getenv("GHOSTTY_RESOURCES_DIR") != "" || os.Getenv("GHOSTTY_BIN_DIR") != ""
}

// ghosttyFocusEnabled decides whether an interactive jump should drive Ghostty
// to focus the target split. CCMON_FOCUS overrides the autodetect:
//
//	auto (default) — focus the split only when we detect we're in Ghostty
//	ghostty        — force the Ghostty path even if detection failed
//	none/off/0     — never touch the terminal (tmux navigation only)
func ghosttyFocusEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CCMON_FOCUS"))) {
	case "none", "off", "0", "false", "no":
		return false
	case "ghostty", "on", "1", "true", "yes":
		return true
	default: // "", "auto"
		return inGhostty()
	}
}

// focusGhosttySplit asks Ghostty to bring forward the split showing this
// instance's pane. It's meant for the interactive TUI jump, where ccmon's own
// split is the focused terminal — so the target is a *sibling* split in the same
// tab. targetCwd (the instance's cwd) is used only as a tie-breaker when more
// than one sibling exists. Errors are non-fatal: the tmux navigation already
// happened, this just moves your eyes there.
func focusGhosttySplit(targetCwd string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "osascript", "-e", ghosttyFocusScript(targetCwd)).Run()
}

// ghosttyFocusScript builds the AppleScript that focuses the sibling split.
// Kept pure (no I/O) so it's unit-testable. The logic, against Ghostty's
// object model (application → windows → tabs → terminals):
//
//   - activate Ghostty, then read the focused terminal of the front window's
//     selected tab — that's ccmon's own split (self);
//   - collect the other terminals in that tab (the siblings);
//   - if one of them reports targetCwd as its working directory, focus that one
//     (handles a tab with 3+ splits); otherwise focus the first sibling — which
//     is exactly the tmux split in the common ccmon-beside-tmux layout.
//
// `working directory` can be missing/stale (tmux doesn't always forward it to
// the outer terminal), so the cwd check is only a preference and the bare
// sibling is the reliable fallback.
func ghosttyFocusScript(targetCwd string) string {
	cwd := escapeAppleScript(targetCwd)
	return `tell application "Ghostty"
	activate
	if (count of windows) is 0 then return
	set theTab to selected tab of front window
	set selfId to id of (focused terminal of theTab)
	set sibs to {}
	repeat with t in terminals of theTab
		if id of t is not selfId then set end of sibs to t
	end repeat
	if (count of sibs) is 0 then return
	repeat with t in sibs
		try
			if (working directory of t) is "` + cwd + `" then
				focus t
				return
			end if
		end try
	end repeat
	focus (item 1 of sibs)
end tell`
}

// escapeAppleScript escapes a Go string for embedding inside an AppleScript
// double-quoted literal: backslash first, then the quote.
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
