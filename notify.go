package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const ghosttyBundle = "com.mitchellh.ghostty"

// notify raises the initial macOS notification for a state change. Unlike
// OSC-777 escapes, terminal-notifier does not depend on tmux passthrough or
// which pane is attached, so it fires reliably from background sessions.
// Clicking runs `ccmon jump <id>`, which selects the pane and surfaces Ghostty.
func notify(i *Instance, kind string) {
	switch kind {
	case StateNeedsInput:
		sendNotify(i, "⚠ needs your input", "Ping", false)
	case StateDone:
		sendNotify(i, "✓ done", "Glass", false)
	default:
		sendNotify(i, kind, "Ping", false)
	}
}

// renotify re-alerts a session still stuck in needs-input. It removes the prior
// banner first so the new one reliably re-shows and re-plays its sound — posting
// to an existing group can otherwise update silently.
func renotify(i *Instance) {
	sendNotify(i, "⏰ still waiting · "+dur(i.Since), "Ping", true)
}

// clearNotification dismisses any banner for an instance (used on ack / jump).
func clearNotification(id string) {
	if bin, err := exec.LookPath("terminal-notifier"); err == nil {
		_ = exec.Command(bin, "-remove", "ccmon-"+id).Run()
	}
}

func sendNotify(i *Instance, subtitle, sound string, replace bool) {
	debugLog(i, subtitle)
	bin, err := exec.LookPath("terminal-notifier")
	if err != nil {
		return // not installed; silently skip
	}
	group := "ccmon-" + i.ID
	if replace {
		_ = exec.Command(bin, "-remove", group).Run()
	}
	self, _ := os.Executable()

	tool := "Claude"
	if i.Source == "codex" {
		tool = "Codex"
	}
	title := tool
	if i.Project != "" {
		title = tool + " · " + i.Project
	}
	msg := i.Msg
	if msg == "" {
		msg = subtitle
	}

	args := []string{
		"-title", title,
		"-subtitle", subtitle,
		"-message", msg,
		"-group", group, // one entry per instance; reminders replace it
		"-sender", ghosttyBundle,
		"-activate", ghosttyBundle,
	}
	if self != "" {
		// -execute runs via `sh -c`; quote defensively.
		args = append(args, "-execute", shellQuote(self)+" jump "+shellQuote(i.ID))
	}
	if sound != "" {
		args = append(args, "-sound", sound)
	}

	// Detach so the caller returns immediately.
	cmd := exec.Command(bin, args...)
	_ = cmd.Start()
	go func() { _ = cmd.Wait() }()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// debugLog appends a line per notification when CCMON_DEBUG is set — handy for
// confirming the nag loop fires.
func debugLog(i *Instance, subtitle string) {
	if os.Getenv("CCMON_DEBUG") == "" {
		return
	}
	p := filepath.Join(filepath.Dir(stateDir()), "notify.log")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%d %-6s %-10s %s | %s\n", now(), i.Source, i.Project, subtitle, i.Msg)
}
