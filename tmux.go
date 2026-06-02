package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// tmux runs a tmux command against a specific socket ("" = default socket).
func tmux(socket string, args ...string) (string, error) {
	full := args
	if socket != "" {
		full = append([]string{"-S", socket}, args...)
	}
	out, err := exec.Command("tmux", full...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// currentCoords reads where the calling process lives in tmux, from the
// inherited $TMUX (socket,pid,session) and $TMUX_PANE env vars. Hooks run as
// children of the claude/codex process, so they inherit these.
func currentCoords() (socket, pane, session, window, winName string, ok bool) {
	tm := os.Getenv("TMUX")
	pane = os.Getenv("TMUX_PANE")
	if tm == "" || pane == "" {
		return "", "", "", "", "", false
	}
	socket = strings.SplitN(tm, ",", 2)[0]
	out, err := tmux(socket, "display-message", "-p", "-t", pane,
		"#{session_name}\x1f#{window_index}\x1f#{window_name}")
	if err != nil {
		// Pane coords still useful even if display fails.
		return socket, pane, "", "", "", true
	}
	parts := strings.Split(out, "\x1f")
	if len(parts) == 3 {
		session, window, winName = parts[0], parts[1], parts[2]
	}
	return socket, pane, session, window, winName, true
}

// listSockets enumerates the tmux socket files for this user.
func listSockets() []string {
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = fmt.Sprintf("/private/tmp/tmux-%d", os.Getuid())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}

type paneInfo struct {
	socket   string
	paneID   string
	session  string
	window   string
	winName  string
	command  string
	path     string
	activity int64 // window_activity unix secs
}

// scanPanes lists every pane on every socket.
func scanPanes() []paneInfo {
	var out []paneInfo
	const f = "#{pane_id}\x1f#{session_name}\x1f#{window_index}\x1f#{window_name}\x1f#{pane_current_command}\x1f#{pane_current_path}\x1f#{window_activity}"
	for _, sock := range listSockets() {
		res, err := tmux(sock, "list-panes", "-a", "-F", f)
		if err != nil || res == "" {
			continue
		}
		for line := range strings.SplitSeq(res, "\n") {
			p := strings.Split(line, "\x1f")
			if len(p) < 7 {
				continue
			}
			pi := paneInfo{
				socket: sock, paneID: p[0], session: p[1], window: p[2],
				winName: p[3], command: p[4], path: p[5],
			}
			fmt.Sscanf(p[6], "%d", &pi.activity)
			out = append(out, pi)
		}
	}
	return out
}

// paneExists reports whether a pane id still exists on a socket.
func paneExists(socket, paneID string) bool {
	if paneID == "" {
		return false
	}
	_, err := tmux(socket, "display-message", "-p", "-t", paneID, "#{pane_id}")
	return err == nil
}

func isCodexCommand(cmd string) bool {
	return strings.HasPrefix(cmd, "codex")
}
