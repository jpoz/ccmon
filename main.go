// ccmon — mission control for Claude Code & Codex instances running in tmux.
//
//	ccmon hook         called by Claude Code hooks (JSON on stdin)
//	ccmon codex-hook   called by codex `notify` (JSON in argv)
//	ccmon jump <id>    focus the instance's tmux pane + Ghostty
//	ccmon tui          interactive router (default with no args)
//	ccmon list         plain-text dump of current state
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		runTUI()
		return
	}
	switch os.Args[1] {
	case "hook":
		runHook()
	case "codex-hook":
		runCodexHook(os.Args[2:])
	case "jump":
		runJump(os.Args[2:])
	case "tui":
		runTUI()
	case "list":
		runList()
	default:
		fmt.Fprintln(os.Stderr, "usage: ccmon [hook|codex-hook|jump <id>|tui|list]")
		os.Exit(2)
	}
}
