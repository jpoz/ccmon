package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReconcileAgainstRealPane spins up a throwaway tmux server, paints a pane
// with each state's fixture, and asserts reconcileClaude reads the live pane and
// corrects a stale stored state — exercising capturePane + classify + save for
// real, not mocked.
func TestReconcileAgainstRealPane(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// Redirect state writes (reconcile calls inst.save()) into a temp HOME.
	t.Setenv("HOME", t.TempDir())

	sock := filepath.Join(t.TempDir(), "ccmon-test.sock")
	t.Cleanup(func() { _, _ = tmux(sock, "kill-server") })

	cases := []struct {
		name    string
		fixture string
		stale   string
		want    string
	}{
		{"granted clears red", panePermissionGranted, StateNeedsInput, StateWorking},
		{"prompt sets red", panePermission, StateWorking, StateNeedsInput},
		{"completion sets done", paneDone, StateWorking, StateDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pane := paintPane(t, sock, tc.fixture)
			inst := &Instance{
				ID: "test-" + tc.name, Source: "claude",
				Socket: sock, PaneID: pane, State: tc.stale, Since: now(),
			}
			changed, prev := reconcileClaude(inst)
			if !changed {
				t.Fatalf("expected reconcile to change state from %q", tc.stale)
			}
			if prev != tc.stale {
				t.Errorf("prev = %q, want %q", prev, tc.stale)
			}
			if inst.State != tc.want {
				t.Errorf("state = %q, want %q (pane:\n%s)", inst.State, tc.want, tc.fixture)
			}
		})
	}
}

// TestReconcileScrubsStaleMsg covers arc-alt's exact situation: the state is
// already correct (working), but a leftover "needs your permission" message is
// still attached. Reconcile must scrub it without reporting a state change.
func TestReconcileScrubsStaleMsg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	t.Setenv("HOME", t.TempDir())
	sock := filepath.Join(t.TempDir(), "ccmon-scrub.sock")
	t.Cleanup(func() { _, _ = tmux(sock, "kill-server") })

	pane := paintPane(t, sock, paneWorking)
	inst := &Instance{
		ID: "scrub", Source: "claude", Socket: sock, PaneID: pane,
		State: StateWorking, Msg: "Claude needs your permission", Since: now(),
	}
	changed, _ := reconcileClaude(inst)
	if changed {
		t.Errorf("changed = true, want false (state already correct)")
	}
	if inst.Msg != "" {
		t.Errorf("Msg = %q, want empty (stale permission text scrubbed)", inst.Msg)
	}
}

// TestReconcileKeepsAttendedDoneIdle covers jumping to a finished session: the
// completed-turn line is still on the pane (classifier reads "done"), but once
// the user has attended it the row must stay idle and keep its message instead
// of bouncing back to green.
func TestReconcileKeepsAttendedDoneIdle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	t.Setenv("HOME", t.TempDir())
	sock := filepath.Join(t.TempDir(), "ccmon-attended.sock")
	t.Cleanup(func() { _, _ = tmux(sock, "kill-server") })

	pane := paintPane(t, sock, paneDone)
	inst := &Instance{
		ID: "attended", Source: "claude", Socket: sock, PaneID: pane,
		State: StateIdle, Attended: true, Msg: "shipped the refactor", Since: now(),
	}
	changed, _ := reconcileClaude(inst)
	if changed {
		t.Errorf("changed = true, want false (attended done stays idle)")
	}
	if inst.State != StateIdle {
		t.Errorf("state = %q, want %q (attended done must not re-green)", inst.State, StateIdle)
	}
	if inst.Msg != "shipped the refactor" {
		t.Errorf("Msg = %q, want it preserved", inst.Msg)
	}
}

// paintPane creates a pane that displays the given text and stays alive, then
// returns its pane id once the content has actually rendered.
func paintPane(t *testing.T, sock, text string) string {
	t.Helper()
	// `cat` of the fixture leaves it on screen; `read` keeps the pane open.
	script := "cat <<'CCMON_EOF'\n" + text + "\nCCMON_EOF\nexec sleep 60"
	if _, err := tmux(sock, "new-window", "-d", "-P", "-F", "#{pane_id}",
		"sh", "-c", script); err != nil {
		// new-window fails if no server yet; new-session bootstraps one.
		if _, err := tmux(sock, "new-session", "-d", "-x", "120", "-y", "40",
			"sh", "-c", script); err != nil {
			t.Fatalf("tmux new-session: %v", err)
		}
	}
	pane := lastPane(t, sock)
	// Wait for the shell to emit the fixture (tmux renders asynchronously).
	want := firstNonBlank(text)
	for range 50 {
		if got, ok := capturePane(sock, pane); ok && strings.Contains(got, want) {
			return pane
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pane %s never rendered fixture", pane)
	return ""
}

func lastPane(t *testing.T, sock string) string {
	t.Helper()
	out, err := tmux(sock, "list-panes", "-a", "-F", "#{pane_id}")
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		t.Fatal("no panes")
	}
	return fields[len(fields)-1]
}

func firstNonBlank(s string) string {
	for ln := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			// A short, stable substring is enough to confirm rendering.
			if len(t) > 12 {
				return t[:12]
			}
			return t
		}
	}
	return s
}
