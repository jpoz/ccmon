package main

import "testing"

func TestApplyClaudeEvent(t *testing.T) {
	cases := []struct {
		name       string
		start      string
		event      string
		wantState  string
		wantNotify string
	}{
		{"session start", "", "SessionStart", StateIdle, ""},
		{"prompt submit", StateIdle, "UserPromptSubmit", StateWorking, ""},
		{"notification needs input", StateWorking, "Notification", StateNeedsInput, StateNeedsInput},
		{"stop is done", StateWorking, "Stop", StateDone, StateDone},
		{"subagent keeps working", StateWorking, "SubagentStop", StateWorking, ""},
		{"pretooluse is working", StateIdle, "PreToolUse", StateWorking, ""},

		// The bug this fixes: granting permission produces no dedicated event,
		// but the approved tool then runs and fires PostToolUse — which must
		// clear the red state instead of leaving it stuck.
		{"posttooluse clears needs-input", StateNeedsInput, "PostToolUse", StateWorking, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := &Instance{State: tc.start}
			got := applyClaudeEvent(inst, claudePayload{HookEventName: tc.event})
			if inst.State != tc.wantState {
				t.Errorf("state = %q, want %q", inst.State, tc.wantState)
			}
			if got != tc.wantNotify {
				t.Errorf("notifyKind = %q, want %q", got, tc.wantNotify)
			}
		})
	}
}

// setState must only reset Since on a real change, so "how long has it waited"
// stays accurate when redundant events arrive (e.g. several PostToolUse in a row).
func TestSetStateStableSince(t *testing.T) {
	inst := &Instance{}
	inst.setState(StateWorking)
	first := inst.Since
	inst.setState(StateWorking) // no change
	if inst.Since != first {
		t.Fatalf("Since moved on a no-op transition: %d -> %d", first, inst.Since)
	}
}

// Leaving needs-input must drop the permission message, or a working/done row
// keeps claiming "Claude needs your permission" — the exact symptom reported.
func TestLeavingNeedsInputClearsMsg(t *testing.T) {
	inst := &Instance{State: StateNeedsInput, Msg: "Claude needs your permission"}
	applyClaudeEvent(inst, claudePayload{HookEventName: "PostToolUse"})
	if inst.State != StateWorking {
		t.Fatalf("state = %q, want working", inst.State)
	}
	if inst.Msg != "" {
		t.Fatalf("Msg = %q, want empty after unblocking", inst.Msg)
	}
}

func TestIsStalePermissionMsg(t *testing.T) {
	stale := []string{
		"Claude needs your permission",
		"Claude needs your permission to use Bash",
		"Claude is waiting for your input",
		"needs your input",
	}
	for _, m := range stale {
		if !isStalePermissionMsg(m) {
			t.Errorf("isStalePermissionMsg(%q) = false, want true", m)
		}
	}
	fresh := []string{"", "refactor the database layer", "Simmering…", "subagent finished"}
	for _, m := range fresh {
		if isStalePermissionMsg(m) {
			t.Errorf("isStalePermissionMsg(%q) = true, want false", m)
		}
	}
}
