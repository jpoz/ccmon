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

// TestReconcileCodexAgainstRealPane covers the codex:infra stuck-working bug:
// the turn-complete notify recorded "done", a spurious activity bump promoted it
// back to "working", and nothing ever demoted it — while the pane plainly showed
// the "─ Worked for 1m 13s ─" separator. Reconcile must read the pane and fix
// the row, for the approval box too (codex raises no event for either).
func TestReconcileCodexAgainstRealPane(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	t.Setenv("HOME", t.TempDir())
	sock := filepath.Join(t.TempDir(), "ccmon-codex.sock")
	t.Cleanup(func() { _, _ = tmux(sock, "kill-server") })

	cases := []struct {
		name    string
		fixture string
		stale   string
		want    string
	}{
		{"stuck working clears to done", codexDone, StateWorking, StateDone},
		{"trust prompt sets red", codexTrustPrompt, StateWorking, StateNeedsInput},
		{"new turn revives done", codexWorking, StateDone, StateWorking},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pane := paintPane(t, sock, tc.fixture)
			inst := &Instance{
				ID: "test-codex-" + tc.name, Source: "codex",
				Socket: sock, PaneID: pane, State: tc.stale, Since: now(),
			}
			changed, prev := reconcileCodex(inst, now())
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

// TestReconcileCodexQuietDemotion covers the ambiguous short-turn screen: chrome
// with no work marker must not touch a fresh "working" (mid-turn render blink),
// but once the pane has been quiet past codexQuietSecs a claimed "working" is a
// lie — demote it to idle, keeping the last completion message on the row.
func TestReconcileCodexQuietDemotion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	t.Setenv("HOME", t.TempDir())
	sock := filepath.Join(t.TempDir(), "ccmon-codex-quiet.sock")
	t.Cleanup(func() { _, _ = tmux(sock, "kill-server") })

	pane := paintPane(t, sock, codexShortDone)

	fresh := &Instance{
		ID: "codex-fresh", Source: "codex", Socket: sock, PaneID: pane,
		State: StateWorking, Since: now(),
	}
	if changed, _ := reconcileCodex(fresh, now()); changed {
		t.Errorf("fresh activity: changed = true, want false (could be a render blink)")
	}
	if fresh.State != StateWorking {
		t.Errorf("fresh activity: state = %q, want %q", fresh.State, StateWorking)
	}

	quiet := &Instance{
		ID: "codex-quiet", Source: "codex", Socket: sock, PaneID: pane,
		State: StateWorking, Msg: "Opened PR #111:", Since: now(),
	}
	changed, _ := reconcileCodex(quiet, now()-codexQuietSecs-5)
	if !changed {
		t.Fatal("quiet pane: expected stale working to demote")
	}
	if quiet.State != StateIdle {
		t.Errorf("quiet pane: state = %q, want %q", quiet.State, StateIdle)
	}
	if quiet.Msg != "Opened PR #111:" {
		t.Errorf("quiet pane: Msg = %q, want the completion summary kept", quiet.Msg)
	}

	// A done row on the same ambiguous screen stays done — the hook said the
	// turn finished and nothing on the pane contradicts it.
	done := &Instance{
		ID: "codex-done", Source: "codex", Socket: sock, PaneID: pane,
		State: StateDone, Since: now(),
	}
	if changed, _ := reconcileCodex(done, now()-codexQuietSecs-5); changed {
		t.Errorf("done row: changed = true, want false (ambiguous screen must not clobber done)")
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
