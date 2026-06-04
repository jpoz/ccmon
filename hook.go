package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// claudePayload is the JSON Claude Code passes to hooks on stdin.
type claudePayload struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd"`
	Message       string `json:"message"` // Notification
	Prompt        string `json:"prompt"`  // UserPromptSubmit
	Source        string `json:"source"`  // SessionStart
	Reason        string `json:"reason"`  // SessionEnd
}

// runHook handles a Claude Code hook invocation (payload on stdin).
func runHook() {
	raw, _ := io.ReadAll(os.Stdin)
	var p claudePayload
	_ = json.Unmarshal(raw, &p)
	if p.SessionID == "" {
		// Nothing actionable without an identity; exit quietly.
		return
	}

	if p.HookEventName == "SessionEnd" {
		if inst, ok := loadInstance(p.SessionID); ok {
			appendFeedLog(Event{Label: label(inst), From: inst.State, To: "", At: now()})
		}
		removeInstance(p.SessionID)
		return
	}

	inst, ok := loadInstance(p.SessionID)
	if !ok {
		inst = &Instance{ID: p.SessionID, Source: "claude", Since: now()}
	}
	inst.Source = "claude"
	if p.Cwd != "" {
		inst.Cwd = p.Cwd
		inst.Project = filepath.Base(p.Cwd)
	}
	// Always refresh tmux coordinates — the pane can change between events.
	if sock, pane, sess, win, winName, okc := currentCoords(); okc {
		inst.Socket, inst.PaneID = sock, pane
		inst.Session, inst.Window, inst.WinName = sess, win, winName
	}

	prev := inst.State
	notifyKind := applyClaudeEvent(inst, p)

	_ = inst.save()
	tagPane(inst) // best-effort tmux marker for status bars
	// Log the transition durably (Msg is final by now — applyClaudeEvent sets it
	// after the state change). From "" reads as an appearance in the feed.
	if inst.State != prev {
		appendFeedLog(Event{Label: label(inst), From: prev, To: inst.State, Msg: inst.Msg, At: inst.Since})
	}
	if notifyKind != "" {
		notify(inst, notifyKind)
	}
}

// applyClaudeEvent transitions inst according to a single hook event and reports
// which kind of notification (if any) the transition should raise. It is pure
// (no I/O) so the state machine can be unit-tested directly.
func applyClaudeEvent(inst *Instance, p claudePayload) (notifyKind string) {
	switch p.HookEventName {
	case "SessionStart":
		inst.setState(StateIdle)
	case "UserPromptSubmit":
		inst.setState(StateWorking)
		if s := firstLine(p.Prompt); s != "" {
			inst.Msg = s
		}
	case "Notification":
		inst.setState(StateNeedsInput)
		if p.Message != "" {
			inst.Msg = p.Message
		}
		notifyKind = StateNeedsInput
	case "PreToolUse", "PostToolUse":
		// A tool is starting or finishing ⇒ Claude is not blocked on you. This
		// is the key signal that clears a stale needs-input the moment a tool
		// runs after you grant permission — Claude fires no dedicated
		// "permission granted" event, so without this the session would stay
		// red until the whole turn ended.
		inst.setState(StateWorking)
	case "Stop":
		inst.setState(StateDone)
		notifyKind = StateDone
	case "SubagentStop":
		// A subagent finished but the main agent keeps going.
		inst.setState(StateWorking)
		inst.Msg = "subagent finished"
	default:
		inst.setState(StateWorking)
	}
	return notifyKind
}

// codexPayload is the JSON the codex CLI passes to its `notify` program.
type codexPayload struct {
	Type                 string `json:"type"`
	LastAssistantMessage string `json:"last-assistant-message"`
}

// runCodexHook handles a codex notify invocation (payload in argv).
func runCodexHook(args []string) {
	if len(args) == 0 {
		return
	}
	var p codexPayload
	_ = json.Unmarshal([]byte(args[len(args)-1]), &p)

	sock, pane, sess, win, winName, ok := currentCoords()
	if !ok {
		return // not in tmux; nothing to route to
	}
	id := "cdx-" + filepath.Base(sock) + "-" + strings.TrimPrefix(pane, "%")

	inst, found := loadInstance(id)
	if !found {
		inst = &Instance{ID: id, Source: "codex", Since: now()}
	}
	inst.Source = "codex"
	inst.Socket, inst.PaneID = sock, pane
	inst.Session, inst.Window, inst.WinName = sess, win, winName
	if cwd := paneCwd(sock, pane); cwd != "" {
		inst.Cwd = cwd
		inst.Project = filepath.Base(cwd)
	}
	if p.LastAssistantMessage != "" {
		inst.Msg = firstLine(p.LastAssistantMessage)
	}
	// codex only notifies on turn completion → it's now waiting on you.
	prev := inst.State
	inst.setState(StateDone)
	_ = inst.save()
	tagPane(inst)
	if inst.State != prev {
		appendFeedLog(Event{Label: label(inst), From: prev, To: inst.State, Msg: inst.Msg, At: inst.Since})
	}
	notify(inst, StateDone)
}

func paneCwd(socket, pane string) string {
	out, err := tmux(socket, "display-message", "-p", "-t", pane, "#{pane_current_path}")
	if err != nil {
		return ""
	}
	return out
}

// tagPane stamps a @cc_state user option on the pane so tmux status bars /
// formats can surface it. Harmless if unused.
func tagPane(i *Instance) {
	if i.PaneID == "" {
		return
	}
	_, _ = tmux(i.Socket, "set-option", "-p", "-t", i.PaneID, "@cc_state", i.State)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 120)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
