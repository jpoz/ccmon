package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// jumpTo moves the user's tmux client to the instance's pane and surfaces
// Ghostty. Targeting by pane id is robust across sockets/sessions and across
// both the `spark` (default socket) and `work` (-L claude) layouts.
func jumpTo(i *Instance) error {
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
	// Clearing the alert state here means clicking through to a pane marks it
	// as attended, so it drops out of the "needs me" group.
	i.setState(StateWorking)
	_ = i.save()
	tagPane(i)
	// Bring the terminal forward (the click came from a background banner).
	_ = exec.Command("open", "-a", "Ghostty").Start()
	return nil
}

// runJump is the CLI entrypoint used by terminal-notifier's -execute.
func runJump(args []string) {
	if len(args) == 0 {
		return
	}
	id := args[0]
	if inst, ok := loadInstance(id); ok {
		_ = jumpTo(inst)
		return
	}
	// Fall back to treating the argument as a literal pane id on any socket.
	if strings.HasPrefix(id, "%") {
		for _, sock := range listSockets() {
			if paneExists(sock, id) {
				_ = jumpTo(&Instance{ID: id, PaneID: id, Socket: sock})
				return
			}
		}
	}
}
