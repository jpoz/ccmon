package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// jumpTo moves the user's tmux client to the instance's pane and surfaces the
// terminal. Targeting by pane id is robust across sockets/sessions and across
// both the `spark` (default socket) and `work` (-L claude) layouts.
//
// interactive is true for the TUI's Enter (ccmon is the focused Ghostty split,
// so we can pivot off it to focus the sibling split running tmux) and false for
// the detached notification-click / CLI path (just bring the app forward). See
// surfaceTerminal in focus.go.
func jumpTo(i *Instance, interactive bool) error {
	if i.PaneID == "" {
		return fmt.Errorf("instance %s has no pane", i.ID)
	}
	if !paneExists(i.Socket, i.PaneID) {
		return fmt.Errorf("pane %s is gone", i.PaneID)
	}
	// Point whatever client is attached at the target session, then focus the
	// exact window+pane. switch-client is a no-op/harmless if already there.
	if i.Session != "" {
		_, _ = tmux(i.Socket, "switch-client", "-t", i.Session)
	}
	_, _ = tmux(i.Socket, "select-window", "-t", i.PaneID)
	_, _ = tmux(i.Socket, "select-pane", "-t", i.PaneID)
	// Jumping to a pane means you've seen it, so it drops out of the "needs me"
	// groups. A finished turn becomes idle but keeps its summary message — the
	// work is done and you've read it, yet what it said is still worth a glance;
	// Attended stops the reconciler re-deriving "done" from the completed-turn
	// line still on the pane. Anything else (a live or blocked session) reads as
	// working until the next event/reconcile says otherwise.
	if i.State == StateDone {
		i.setState(StateIdle)
		i.Attended = true
	} else {
		i.setState(StateWorking)
	}
	_ = i.save()
	tagPane(i)
	clearNotification(i.ID) // attended → drop any lingering banner
	surfaceTerminal(i, interactive)
	return nil
}

// surfaceTerminal brings the right terminal surface forward after the tmux
// navigation. On an interactive jump from a Ghostty split we focus the sibling
// split that's running tmux (see focusGhosttySplit); on the detached path (a
// banner click) we just front the app, since there's no ccmon split to pivot
// from. Non-Ghostty users get neither — switch-client already moved their
// attached client — so their workflow is left untouched.
func surfaceTerminal(i *Instance, interactive bool) {
	if interactive {
		if ghosttyFocusEnabled() {
			_ = focusGhosttySplit(i.Cwd)
		}
		return
	}
	// Detached (notification click / CLI): the banner came from the background,
	// so bring Ghostty forward. We can't detect the terminal here (this process
	// runs under terminal-notifier's -execute, with no Ghostty env), but the
	// notification path is already Ghostty-targeted — terminal-notifier -activates
	// the Ghostty bundle on click — so this just preserves that behavior.
	_ = exec.Command("open", "-a", "Ghostty").Start()
}

// runJump is the CLI entrypoint used by terminal-notifier's -execute.
func runJump(args []string) {
	if len(args) == 0 {
		return
	}
	id := args[0]
	if inst, ok := loadInstance(id); ok {
		_ = jumpTo(inst, false)
		return
	}
	// Fall back to treating the argument as a literal pane id on any socket.
	if strings.HasPrefix(id, "%") {
		for _, sock := range listSockets() {
			if paneExists(sock, id) {
				_ = jumpTo(&Instance{ID: id, PaneID: id, Socket: sock}, false)
				return
			}
		}
	}
}
