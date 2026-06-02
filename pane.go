package main

import (
	"regexp"
	"strings"
)

// This file infers a Claude instance's state directly from what its tmux pane is
// *showing*, independent of hooks. The pane is ground truth for whether Claude is
// blocked on you: a permission box is on screen or it isn't; the work spinner is
// running or it isn't. Hooks are great for low-latency notifications but can be
// missed, delayed, or simply absent for a transition (Claude fires no event when
// you grant permission), which is what leaves a session stuck on needs-input.
// Reconciling persisted state against the live pane self-heals all of those.

var (
	// The confirmation box always offers a numbered "Yes" choice (the first one
	// is pre-selected with ❯) and asks whether to proceed / make the edit / etc.
	// Leading run tolerates indentation, the ❯ cursor, and box borders (│).
	rePermYes = regexp.MustCompile(`(?m)^[\s│❯>]*1\.\s+Yes`)
	rePermNo  = regexp.MustCompile(`(?m)^[\s│❯>]*\d\.\s+No[,.]`)

	// The live spinner renders the verb with a trailing "…" and a running
	// "(<elapsed> · ↓ N tokens · …)" parenthetical, e.g. "Germinating… (4m 44s".
	// This single line exists only while Claude is actively working; it is
	// replaced by the completed-turn line the instant the turn ends.
	reSpinner = regexp.MustCompile(`…\s*\(\s*\d`)

	// The completed-turn status line, e.g. "✻ Cooked for 1m 39s" or
	// "✻ Worked for 59s": a short glyph-led line ending in "for <duration>".
	reCompleted = regexp.MustCompile(`(?m)^\s*\S+\s+\S+ for (?:\d+h ?)?(?:\d+m ?)?\d+s\s*$`)

	// Markers that the pane is a Claude UI at all (so we don't override hook
	// state for a pane that's actually showing a shell or an exited process).
	reClaudeChrome = regexp.MustCompile(`auto mode|\(1M context\)|esc to interrupt|Claude Code|▐▛███▜▌|⏵⏵`)
)

func hasPermissionPrompt(text string) bool {
	if !rePermYes.MatchString(text) {
		return false
	}
	return strings.Contains(text, "Do you want to") ||
		strings.Contains(text, "Would you like to") ||
		rePermNo.MatchString(text)
}

func hasActiveSpinner(text string) bool {
	return reSpinner.MatchString(text) || strings.Contains(text, "esc to interrupt")
}

func hasCompletedTurn(text string) bool {
	return reCompleted.MatchString(text) || strings.Contains(text, "recap:")
}

// liveRegionLines is how many trailing lines we treat as Claude's "live" UI.
// Its spinner, permission selector, completed-turn line and input box all render
// in the bottom few lines; everything above is transcript scrollback. Scanning
// only the tail stops conversational text that merely *quotes* a permission
// prompt ("Do you want to proceed?", "1. Yes") from being read as a real one —
// which otherwise prevents a session whose transcript discusses prompts (like
// ccmon's own) from ever settling on done.
const liveRegionLines = 12

func liveRegion(text string) string {
	lines := strings.Split(text, "\n")
	// Trim trailing blank lines tmux pads the capture with, so the window lands
	// on real content rather than empty rows below the status bar.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > liveRegionLines {
		lines = lines[len(lines)-liveRegionLines:]
	}
	return strings.Join(lines, "\n")
}

// classifyClaudePane derives a state from a Claude pane's visible content.
// ok is false when the text doesn't look like a Claude UI (or is too ambiguous),
// signalling the caller to leave the hook-reported state untouched.
//
// We only look at the live region (see liveRegionLines) and check the spinner
// first: a real permission prompt freezes the spinner, so an animating spinner
// means working even if prompt-shaped text is also on screen. Then permission
// (blocked on you), then a finished turn (done), then bare chrome (idle).
func classifyClaudePane(text string) (state string, ok bool) {
	region := liveRegion(text)
	switch {
	case hasActiveSpinner(region):
		return StateWorking, true
	case hasPermissionPrompt(region):
		return StateNeedsInput, true
	case hasCompletedTurn(region):
		return StateDone, true
	case reClaudeChrome.MatchString(region):
		// Recognisably Claude, but no prompt / spinner / completion ⇒ sitting
		// idle at an empty prompt.
		return StateIdle, true
	}
	return "", false
}

// capturePane returns the visible (no-scrollback) content of a pane.
func capturePane(socket, paneID string) (string, bool) {
	if paneID == "" {
		return "", false
	}
	out, err := tmux(socket, "capture-pane", "-p", "-t", paneID)
	if err != nil {
		return "", false
	}
	return out, true
}

// reconcileClaude corrects a Claude instance's persisted state to match what its
// pane is actually showing, and reports whether anything changed. It trusts the
// pane over the stored state but preserves the richer Msg that hooks captured.
// It returns the prior state so callers can react to a transition (e.g. notify
// when a missed Notification hook means we only now noticed it went red).
func reconcileClaude(inst *Instance) (changed bool, prev string) {
	prev = inst.State
	if inst.Source != "claude" || inst.PaneID == "" {
		return false, prev
	}
	text, ok := capturePane(inst.Socket, inst.PaneID)
	if !ok {
		return false, prev
	}
	live, ok := classifyClaudePane(text)
	if !ok {
		return false, prev
	}

	// A finished turn the user already attended (jumped to / acked) stays idle.
	// Its completed-turn line lingers on the pane until they type, so the
	// classifier keeps reading "done" — honour the demotion and keep the row
	// idle (and its message) rather than re-greening it. A spinner or permission
	// box is genuinely new activity: leave it untouched so setState clears
	// Attended and the row comes alive again.
	if inst.Attended && live == StateDone {
		live = StateIdle
	}

	dirty := false
	switch {
	case live != inst.State:
		inst.setState(live) // clears a stale permission message when leaving needs-input
		if live == StateNeedsInput && inst.Msg == "" {
			inst.Msg = "needs your input"
		}
		changed, dirty = true, true
	case live != StateNeedsInput && isStalePermissionMsg(inst.Msg):
		// State is already right, but a "needs your permission" / "waiting"
		// message outlived it (the transition that unblocked it didn't refresh
		// Msg). Scrub it so the row stops claiming it's blocked.
		inst.Msg = ""
		dirty = true
	}
	if dirty {
		_ = inst.save()
		tagPane(inst)
	}
	return changed, prev
}

// isStalePermissionMsg reports whether a message only makes sense while a
// session is blocked on you — so it has no business sitting on a working/done row.
func isStalePermissionMsg(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "permission") ||
		strings.Contains(m, "waiting for your input") ||
		strings.Contains(m, "needs your input")
}
