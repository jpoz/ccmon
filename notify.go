package main

import (
	"os"
	"os/exec"
	"strings"
)

const ghosttyBundle = "com.mitchellh.ghostty"

// notify raises a macOS notification via terminal-notifier. Unlike OSC-777
// escapes, this does not depend on tmux passthrough or which pane is attached,
// so it fires reliably from detached/background sessions. Clicking it runs
// `ccmon jump <id>`, which selects the exact pane and surfaces Ghostty.
func notify(i *Instance, kind string) {
	bin, err := exec.LookPath("terminal-notifier")
	if err != nil {
		return // not installed; silently skip
	}
	self, _ := os.Executable()

	var subtitle, sound string
	switch kind {
	case StateNeedsInput:
		subtitle = "⚠ needs your input"
		sound = "Ping"
	case StateDone:
		subtitle = "✓ done"
		sound = "Glass"
	default:
		subtitle = kind
		sound = "Ping"
	}

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
		"-group", "ccmon-" + i.ID, // replaces prior notification for this instance
		"-sender", ghosttyBundle,  // show Ghostty's icon
		"-activate", ghosttyBundle,
	}
	if self != "" {
		// -execute runs via `sh -c`; quote the id defensively.
		args = append(args, "-execute", shellQuote(self)+" jump "+shellQuote(i.ID))
	}
	if sound != "" {
		args = append(args, "-sound", sound)
	}

	// Detach so the hook returns immediately.
	cmd := exec.Command(bin, args...)
	_ = cmd.Start()
	go func() { _ = cmd.Wait() }()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
